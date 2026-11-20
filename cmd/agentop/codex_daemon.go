package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rossoctl/cortex/core/peerproc"
)

// codexDaemonPIDRel is where Codex records its app-server daemon, under CODEX_HOME.
//
// THE DAEMON IS WHY ENABLE CAN LOOK DONE AND NOT BE. Codex runs a long-lived background
// server (`codex app-server --managed-daemon`) that makes the model calls for every
// codex window, and a window is only a screen that hands it each prompt. The daemon
// reads $CODEX_HOME/.env when it starts and never again, so a daemon already running
// when enable writes that file keeps calling OpenAI directly: the window's own startup
// checks and telemetry reach Cortex, its prompts do not, and nothing says so. Disable has
// the mirror problem — the daemon keeps the proxy it started with, so it breaks if
// Cortex then stops. Measured on Codex 0.163.0: a daemon started on Oct 23 sent every
// prompt straight to chatgpt.com after an enable on Oct 27.
//
// So enable and disable restart a daemon they find running, as they do OpenCode's
// service. A restart interrupts Codex sessions rather than ending them (read from Codex's
// source, 0.163.0 and 0.164.0): the old daemon refuses new turns and gives running ones its shutdown
// grace to finish (60s by default), saves its threads, and the new one restores them and
// continues a local turn the grace cut off; an open codex window reconnects on its own.
const codexDaemonPIDRel = "app-server-daemon/daemon.pid"

// codexUpdaterPIDRel is where Codex records its auto-updater, the second long-lived
// process an auto-updatable install runs (`codex app-server daemon pid-update-loop`).
//
// IT IS WHY RESTARTING THE DAEMON ALONE IS NOT ENOUGH. The updater also reads the dotenv
// file only when it starts, and the daemon it starts after installing an update inherits
// the updater's environment. `daemon restart` replaces the daemon but keeps a running
// updater: Codex starts one only when its pid file names no live process. So after
// disable the daemon was clean and the updater still had Cortex's proxy, and the next
// update put the daemon back on Cortex (reproduced on 0.164.0 updating to 0.165.0).
// Enable does not need it, since a daemon's own dotenv load adds a proxy it did not
// inherit; it is stopped on both so the two processes never disagree. Stopping it first
// lets the restart start a fresh one, which reads the new file.
const codexUpdaterPIDRel = "app-server-daemon/daemon-updater.pid"

// codexDaemonRestartCmd is Codex's own command for applying a changed environment.
const codexDaemonRestartCmd = "codex app-server daemon restart"

// codexRestartTimeout bounds that command. It waits for the old daemon to drain, for up to
// its shutdown grace — at most 5 minutes — and Codex's own lock allows that plus 75s for
// the forced stop and the new start. This allows the same, plus a margin.
const codexRestartTimeout = 5*time.Minute + 90*time.Second

// codexDaemon is a running app-server daemon, as its pid file and the process table
// describe it.
type codexDaemon struct {
	PID      int
	Started  string             // the pid file's processStartTime, as Codex wrote it; may be empty
	Identity *codexProcIdentity // the pid file's processIdentity; nil when it has none
	Proxy    string             // the HTTPS proxy in its environment, as Codex reads it (codexEnvironProxy); "" when none
	EnvErr   error              // why its environment could not be read; nil when it was
}

// codexProcIdentity is a pid file's processIdentity, as Codex writes it
// (codex-rs/app-server-daemon/src/backend/pid_identity.rs). Codex's three shapes are one
// JSON object with different fields present: Linux {bootId, startTicks}, macOS
// {bootId, uniqueId, startSeconds, startMicroseconds}, and an older macOS
// {startSeconds, startMicroseconds}. Pointers, so a field that is absent is told from one
// that is zero.
type codexProcIdentity struct {
	BootID            *string `json:"bootId"`
	StartTicks        *uint64 `json:"startTicks"`
	UniqueID          *uint64 `json:"uniqueId"`
	StartSeconds      *uint64 `json:"startSeconds"`
	StartMicroseconds *uint64 `json:"startMicroseconds"`
}

// codexDaemonAlive reports whether pid names a running process. Signal 0 delivers
// nothing; it only asks whether the process exists and can be signalled. A process of
// another user answers EPERM, which still means it is alive.
var codexDaemonAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}

// codexEnviron reads a process's environment; a package variable so tests replace it.
var codexEnviron = peerproc.Environ

// codexRestartDaemon restarts the daemon recorded under codexHome with Codex's own command.
// want is Cortex's values for the managed keys, nil when they are not known. A package
// variable so tests replace it: nothing in a test may restart a real daemon.
var codexRestartDaemon = func(codexHome string, want map[string]string) error {
	bin, err := findCodex(codexHome)
	if err != nil {
		return err
	}
	return codexRun(bin, codexHome, want, "app-server", "daemon", "restart")
}

// codexStopUpdater ends the updater recorded under codexHome, if it runs, and waits for it
// to exit. Codex stops its updater the same way: SIGTERM to its process group, which the
// updater handles by shutting down. A package variable so tests replace it.
//
// THE PID IS CHECKED BEFORE ITS GROUP IS SIGNALLED. A pid file outlives the updater that
// wrote it, and its pid can be reused by an unrelated process that leads its own group —
// a shell, a terminal session — and kill(-pid) would end all of it (#1526 review). So it
// is signalled only when codexIsRecorded says the process holding the pid is the one the
// file records. Anything else skips the stop:
//
//   - another process: Codex's restart, which runs next, makes the same check, finds the
//     record stale, and starts a fresh updater on the new settings;
//   - cannot tell: reportRestartedUpdater says so after the restart.
//
// Codex has no command that restarts only its updater, which is why agentop does this at
// all: `codex app-server daemon restart` keeps an updater it finds running.
var codexStopUpdater = func(codexHome string) error {
	u, ok := runningCodexProc(codexHome, codexUpdaterPIDRel)
	if !ok {
		return nil
	}
	if codexIsRecorded(u) != codexRecordMatches {
		return nil
	}
	if err := syscall.Kill(-u.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stopping Codex's updater (pid %d): %w", u.PID, err)
	}
	deadline := time.Now().Add(codexUpdaterStopTimeout)
	for codexDaemonAlive(u.PID) {
		if time.Now().After(deadline) {
			return fmt.Errorf("Codex's updater (pid %d) did not exit within %s", u.PID, codexUpdaterStopTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

// codexRecordMatch is whether the process holding a pid file's pid is the one the file
// records.
type codexRecordMatch int

const (
	codexRecordUnknown codexRecordMatch = iota // could not tell
	codexRecordMatches                         // the recorded process
	codexRecordStale                           // another process: the pid was reused
)

// codexIsRecorded reports whether the process holding d's pid is the one its pid file
// recorded, the way Codex decides it (process_matches_record, app-server-daemon's
// backend/pid.rs):
//
//   - with a processIdentity, by that alone (codexIdentityMatches). It is the kernel's own
//     record of the process, so a time zone or locale change does not move it, and its
//     boot id means a record from before a reboot never matches.
//   - without one, as Codex's legacy check does, by the start time ps prints
//     (codexProcStarted), which a time zone or locale change does move. So there only a
//     match is an answer; a mismatch is codexRecordUnknown, as Codex treats it ("legacy
//     start time changed; PID record retained").
func codexIsRecorded(d codexDaemon) codexRecordMatch {
	if d.Identity != nil {
		return codexIdentityMatches(d.PID, *d.Identity)
	}
	if codexProcStarted(d.PID, d.Started) {
		return codexRecordMatches
	}
	return codexRecordUnknown
}

// codexProcessIdentity reads a process's identity; a package variable so tests replace it.
var codexProcessIdentity = peerproc.ProcessIdentity

// codexIdentityMatches compares a recorded processIdentity with the live process holding
// pid, field for field as Codex's matches_process does. A different boot id or unique id
// is stale even when the rest cannot be read, as for another user's process on macOS. A
// zombie has exited, so it is stale too. A shape this agentop does not know, or an
// identity it cannot read, is codexRecordUnknown.
func codexIdentityMatches(pid int, rec codexProcIdentity) codexRecordMatch {
	live, err := codexProcessIdentity(int32(pid))
	if rec.BootID != nil && live.BootID != "" && *rec.BootID != live.BootID {
		return codexRecordStale
	}
	if rec.UniqueID != nil && live.UniqueID != 0 && *rec.UniqueID != live.UniqueID {
		return codexRecordStale
	}
	if err != nil {
		return codexRecordUnknown
	}
	if live.Zombie {
		return codexRecordStale
	}
	same := func(r *uint64, v uint64) bool { return r != nil && *r == v }
	switch {
	case rec.StartTicks != nil:
		// Linux. Its shape always carries a boot id.
		if rec.BootID == nil || live.StartTicks == 0 {
			return codexRecordUnknown
		}
		if same(rec.StartTicks, live.StartTicks) {
			return codexRecordMatches
		}
	case rec.StartSeconds != nil && rec.StartMicroseconds != nil:
		// macOS, with or without the newer shape's boot id and unique id.
		if live.StartSeconds == 0 {
			return codexRecordUnknown
		}
		if same(rec.StartSeconds, live.StartSeconds) && same(rec.StartMicroseconds, live.StartMicroseconds) {
			return codexRecordMatches
		}
	default:
		return codexRecordUnknown
	}
	return codexRecordStale
}

// codexProcStarted reports whether pid is the process a Codex pid file recorded, by the
// record's processStartTime: Codex's legacy check, for a record with no processIdentity.
// Codex writes it from `ps -o lstart=`, so the same command reads it back, compared after
// trimming. An empty record, a ps that fails or prints nothing, or a different time is
// false. A package variable so tests replace it.
//
// lstart is printed in the local time zone and, on macOS, locale (openai/codex#43608), so
// a change to either since the process started reads as a mismatch. That is why a
// mismatch here means "cannot tell" (codexIsRecorded), not "another process".
var codexProcStarted = func(pid int, recorded string) bool {
	recorded = strings.TrimSpace(recorded)
	if pid <= 0 || recorded == "" {
		return false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	return err == nil && strings.TrimSpace(string(out)) == recorded
}

// codexUpdaterStopTimeout bounds the wait for the updater to exit. It has no turns to
// drain, so it exits at once unless it is mid-install; a timeout leaves the restart to go
// ahead and the check after it to name the updater if it kept its old settings.
const codexUpdaterStopTimeout = 10 * time.Second

// runningCodexDaemon returns the daemon recorded under codexHome when its process is
// still alive. ok is false when there is no pid file, it cannot be read, or the process
// it names has exited — a stale file Codex leaves behind after a crash or a reboot.
func runningCodexDaemon(codexHome string) (codexDaemon, bool) {
	return runningCodexProc(codexHome, codexDaemonPIDRel)
}

// runningCodexUpdater is runningCodexDaemon for the auto-updater.
func runningCodexUpdater(codexHome string) (codexDaemon, bool) {
	return runningCodexProc(codexHome, codexUpdaterPIDRel)
}

// runningCodexProc reads one of Codex's pid files under codexHome, and the environment of
// the process it names when that process is alive.
func runningCodexProc(codexHome, pidRel string) (codexDaemon, bool) {
	b, err := os.ReadFile(filepath.Join(codexHome, pidRel))
	if err != nil {
		return codexDaemon{}, false
	}
	var rec struct {
		PID              int             `json:"pid"`
		ProcessStartTime string          `json:"processStartTime"`
		ProcessIdentity  json.RawMessage `json:"processIdentity"`
	}
	if json.Unmarshal(b, &rec) != nil || rec.PID > math.MaxInt32 || !codexDaemonAlive(rec.PID) {
		return codexDaemon{}, false
	}
	d := codexDaemon{PID: rec.PID, Started: rec.ProcessStartTime}
	// Decoded apart from the rest, so an identity of a type agentop does not expect makes
	// only the identity unknown (#1736 review). Failing the whole record would hide a
	// running daemon, and enable and disable would skip its restart. An identity that
	// fails to decode is used empty rather than half-filled: codexIdentityMatches knows no
	// shape in it, so the answer is codexRecordUnknown.
	if len(rec.ProcessIdentity) > 0 && string(rec.ProcessIdentity) != "null" {
		var id codexProcIdentity
		if json.Unmarshal(rec.ProcessIdentity, &id) != nil {
			id = codexProcIdentity{}
		}
		d.Identity = &id
	}
	env, err := codexEnviron(int32(rec.PID))
	switch {
	case err != nil:
		d.EnvErr = fmt.Errorf("reading the environment of pid %d: %w", rec.PID, err)
	case len(env) == 0:
		// Unknown, not absent, for the reason probeOpenCodeService gives.
		d.EnvErr = errors.New("its environment could not be read")
	default:
		d.Proxy = codexEnvironProxy(env)
	}
	return d, true
}

// codexEnvironProxy is the proxy Codex sends HTTPS requests through, from env, a list of
// "NAME=value" strings: HTTPS_PROXY, else ALL_PROXY, each read uppercase first and then
// lowercase, and taken only when non-empty. That is Codex's own reading
// (codex-rs/http-client's proxy_env_value) and not Bun's, which is why environProxy is
// not used: Codex prefers the uppercase name, and an uppercase name set to "" hides the
// lowercase one rather than falling through to it.
func codexEnvironProxy(env []string) string {
	lookup := func(name string) (string, bool) {
		for _, kv := range env {
			if k, v, ok := strings.Cut(kv, "="); ok && k == name {
				return v, true
			}
		}
		return "", false
	}
	for _, upper := range []string{"HTTPS_PROXY", "ALL_PROXY"} {
		v, ok := lookup(upper)
		if !ok {
			v, _ = lookup(strings.ToLower(upper))
		}
		if v != "" {
			return v
		}
	}
	return ""
}

// findCodex is the codex binary: on PATH, else where Codex's standalone installer puts it
// under codexHome.
func findCodex(codexHome string) (string, error) {
	if p, err := exec.LookPath("codex"); err == nil {
		return p, nil
	}
	p := filepath.Join(codexHome, "packages", "standalone", "current", "bin", "codex")
	if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
		return p, nil
	}
	return "", fmt.Errorf("codex not found on PATH or in %s", filepath.Dir(p))
}

// codexRun runs the codex CLI against codexHome and returns an error carrying the first
// line of its stderr when it fails.
//
// The restarted daemon inherits this command's environment, overlaid with the dotenv file,
// so agentop's own variables reach it only when they are not Cortex's: after disable, a
// restart from a shell with Cortex's HTTPS_PROXY set would otherwise leave the daemon on
// Cortex. CODEX_HOME is set only when Codex would not find codexHome without it, so a
// daemon gains no variable it did not have before.
func codexRun(bin, codexHome string, want map[string]string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), codexRestartTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin is the codex CLI, found by findCodex
	env := codexCLIEnv(os.Environ(), want)
	if home, err := os.UserHomeDir(); err != nil || defaultCodexEnvPath(home) != filepath.Join(codexHome, ".env") {
		env = append(env, codexHomeEnv+"="+codexHome)
	}
	cmd.Env = env
	// Only stderr is read. stdout goes nowhere rather than to a pipe, so a daemon that kept
	// an inherited stdout open could not hold Wait. stderr is a pipe and might be held the
	// same way: WaitDelay bounds that, and ErrWaitDelay, which Go returns only after a
	// successful exit, is not a failed restart.
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		what := filepath.Base(bin) + " " + strings.Join(args, " ")
		if ctx.Err() != nil {
			return fmt.Errorf("%s: no answer in %s", what, codexRestartTimeout)
		}
		if line, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n"); line != "" {
			return fmt.Errorf("%s: %w: %s", what, err, strings.TrimSpace(line))
		}
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// codexCLIEnv is env, a list of "NAME=value" strings, without those of the managed keys
// (codexKeys) whose value is Cortex's, by codexIsOurs: Cortex's value or its shape, since
// a process environment has no record to judge by. Every other entry is kept.
func codexCLIEnv(env []string, want map[string]string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); isCodexKey(k) && codexIsOurs(k, v, want) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func isCodexKey(k string) bool {
	for _, c := range codexKeys {
		if k == c {
			return true
		}
	}
	return false
}

// noteCodexDaemonRestart is said before enable or disable asks, when a daemon is running:
// applying the change restarts it. With none running, the next codex starts one with the
// new file, so there is nothing to say.
func noteCodexDaemonRestart(codexHome string, stdout io.Writer) {
	d, ok := runningCodexDaemon(codexHome)
	if !ok {
		return
	}
	fmt.Fprintf(stdout, "Codex's background app-server is running (pid %d%s).\n"+
		"  Applying this restarts it: Codex turns already running get up to its shutdown grace\n"+
		"  (a minute by default) to finish, and open Codex windows reconnect.\n",
		d.PID, startedSuffix(d))
}

func startedSuffix(d codexDaemon) string {
	if d.Started == "" {
		return ""
	}
	return ", started " + d.Started
}

// codexProcState is how one of Codex's two background processes stands against the routing
// its dotenv file now has: through Cortex's proxy after enable (configured), not through
// it after disable or uninstall. Both read the file only when they start.
type codexProcState int

const (
	codexNotRunning   codexProcState = iota // none runs, or its record names another process, which a restart replaces
	codexAgrees                             // it has the routing the file now gives
	codexDisagrees                          // it has the other one
	codexUnjudged                           // no proxy of Cortex's is known to compare it with
	codexUnreadable                         // its environment could not be read
	codexUnconfirmed                        // the updater: agentop cannot tell it is the process its record names
	codexNotStopped                         // the updater: its stop failed, so it keeps the environment it started with
	codexNotRestarted                       // the app-server: its restart failed
	codexGone                               // the app-server: restarted, and not running now
)

// codexProcs is how codexHome's app-server and auto-updater stand, from checkCodexProcs, or
// from settleCodexProcs once it has acted. Every Codex command judges them here, so the
// commands cannot disagree about what to stop, what to restart, or what is left. Each says
// it in its own words: configure's commands with printCodexProcs and reportCodexDaemon,
// uninstall with codexProcsLeft.
type codexProcs struct {
	daemon, updater           codexDaemon // the process each state is about, when one runs
	daemonState, updaterState codexProcState
	restarted                 bool  // settleCodexProcs restarted the app-server
	restartErr, stopErr       error // why that restart, or the updater's stop, failed
}

// checkCodexProcs is how codexHome's processes stand against the routing configured names,
// through cortexProxy or not, read now and acting on nothing.
//
// The updater is judged only once codexIsRecorded confirms the process holding its pid is
// the one its record names, so a reused pid never gets a kill command (#1736 review). A
// record of another process counts as none: Codex's restart finds it stale and starts a
// fresh updater. When agentop cannot tell, the updater is codexUnconfirmed, and the
// callers say what could not be confirmed rather than name a process they may be wrong
// about.
func checkCodexProcs(codexHome, cortexProxy string, configured bool) codexProcs {
	var p codexProcs
	var ok bool
	p.daemon, ok = runningCodexDaemon(codexHome)
	p.daemonState = codexStateOf(p.daemon, ok, cortexProxy, configured)
	p.updater, ok = runningCodexUpdater(codexHome)
	if ok {
		switch codexIsRecorded(p.updater) {
		case codexRecordStale:
			ok = false
		case codexRecordUnknown:
			p.updaterState = codexUnconfirmed
			return p
		}
	}
	p.updaterState = codexStateOf(p.updater, ok, cortexProxy, configured)
	return p
}

func codexStateOf(d codexDaemon, running bool, cortexProxy string, configured bool) codexProcState {
	switch {
	case !running:
		return codexNotRunning
	case d.EnvErr != nil:
		return codexUnreadable
	case cortexProxy == "":
		return codexUnjudged
	case sameProxy(d.Proxy, cortexProxy) == configured:
		return codexAgrees
	}
	return codexDisagrees
}

// codexSettles is whether settleCodexProcs acts on p, as checked before it acts: on the
// updater, by stopping it, and on a running app-server, by restarting it. changed is
// whether the dotenv file changed. When it did, both are settled, since both read the
// file only when they start. When it did not, only one that disagrees is: restarting an
// app-server that already agrees would interrupt its turns for nothing.
func codexSettles(p codexProcs, changed bool) (stop, restart bool) {
	stop = changed || p.updaterState == codexDisagrees
	restart = p.daemonState != codexNotRunning && (changed || p.daemonState == codexDisagrees)
	return stop, restart
}

// settleCodexProcs stops codexHome's auto-updater and restarts its app-server, as
// codexSettles says, then checks both again. The updater goes first, so the restart starts
// a fresh one that reads the new file (codexUpdaterPIDRel), and it goes even with no
// app-server running: Codex's daemon stop leaves it running, and the app-server it starts
// after its next update inherits its environment. want is Cortex's values for the managed
// keys, nil when they are not known. progress, when not nil, is told before a restart,
// which can take a minute.
//
// A failed stop or restart is not the caller's failure: the file holds the change, and the
// states say what still has the old routing.
func settleCodexProcs(codexHome, cortexProxy string, want map[string]string, configured, changed bool, progress io.Writer) codexProcs {
	was := checkCodexProcs(codexHome, cortexProxy, configured)
	stop, restart := codexSettles(was, changed)
	var stopErr, restartErr error
	if stop {
		stopErr = codexStopUpdater(codexHome)
	}
	if restart {
		if progress != nil {
			fmt.Fprintf(progress, "Restarting Codex's background app-server (pid %d)...\n", was.daemon.PID)
		}
		restartErr = codexRestartDaemon(codexHome, want)
	}
	p := checkCodexProcs(codexHome, cortexProxy, configured)
	p.restarted, p.restartErr, p.stopErr = restart && restartErr == nil, restartErr, stopErr
	switch {
	case restartErr != nil:
		p.daemon, p.daemonState = was.daemon, codexNotRestarted
	case p.restarted && p.daemonState == codexNotRunning:
		p.daemonState = codexGone
	}
	// An updater the stop could not end still has the environment it started with, so it
	// is named unless that environment shows the routing the file now gives: saying agentop
	// could not stop it says why it is left to the user.
	if stopErr != nil && p.updaterState != codexNotRunning && p.updaterState != codexUnconfirmed && p.updaterState != codexAgrees {
		p.updaterState = codexNotStopped
	}
	return p
}

// codexRestartCmdHere is codexRestartCmdFor for configure's commands, which have no setup
// run to take HOME from: HOME as codexRun reads it, so the command printed restarts the
// app-server codexRun would.
func codexRestartCmdHere(codexHome string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return codexHomeEnv + "=" + shellQuote(codexHome) + " " + codexDaemonRestartCmd
	}
	return codexRestartCmdFor(home, codexHome)
}

// codexRestartCmdFor is codexDaemonRestartCmd as the user should run it for the app-server
// of codexHome: with CODEX_HOME set when codexHome is not the home their shell's codex
// uses, under the condition codexRun sets it on. Bare, it would restart another Codex's
// app-server and leave this one on its old settings.
func codexRestartCmdFor(home, codexHome string) string {
	if defaultCodexEnvPath(home) == filepath.Join(codexHome, ".env") {
		return codexDaemonRestartCmd
	}
	return codexHomeEnv + "=" + shellPathIn(home, codexHome) + " " + codexDaemonRestartCmd
}

// codexKillThenRestart is the command that ends a confirmed updater and restarts the
// app-server once it has gone. kill only signals it, and Codex's restart starts a fresh
// updater only when the record names no live process, so a restart run straight after
// the kill could keep the old one, proxy and all. A kill that fails stops the line there.
func codexKillThenRestart(pid int, restartCmd string) string {
	return fmt.Sprintf("kill %d && while kill -0 %d 2>/dev/null; do sleep 1; done && %s", pid, pid, restartCmd)
}

// printCodexProcs says what settleCodexProcs did after enable (configured) or disable
// changed the file, and names what still disagrees with the change, each with the command
// that fixes it. A restart that fails is reported on stderr; the change itself succeeded,
// so the caller still exits 0.
func printCodexProcs(p codexProcs, configured bool, codexHome string, stdout, stderr io.Writer) {
	restartCmd := codexRestartCmdHere(codexHome)
	if p.stopErr != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", p.stopErr)
	}
	d := p.daemon
	switch {
	case p.daemonState == codexNotRestarted:
		fmt.Fprintf(stderr, "agentop: could not restart Codex's background app-server (%v).\n"+
			"  It may still be running with its old settings. To apply the change now:\n"+
			"    %s\n", p.restartErr, restartCmd)
	case !p.restarted:
	case p.daemonState == codexGone:
		fmt.Fprintln(stdout, "Codex's background app-server was restarted, but is not running now. "+
			"It starts with the new settings the next time you run codex.")
	case p.daemonState == codexUnreadable:
		fmt.Fprintf(stdout, "Codex's background app-server was restarted (pid %d); its environment could not be checked.\n", d.PID)
	case p.daemonState == codexUnjudged:
		// disable with no proxy of Cortex's taken out: nothing to compare with.
		fmt.Fprintf(stdout, "Codex's background app-server was restarted (pid %d).\n", d.PID)
	case p.daemonState == codexAgrees && configured:
		fmt.Fprintf(stdout, "Codex's background app-server was restarted (pid %d) and is using Cortex.\n", d.PID)
	case p.daemonState == codexAgrees:
		fmt.Fprintf(stdout, "Codex's background app-server was restarted (pid %d) and no longer uses Cortex.\n", d.PID)
	case configured:
		fmt.Fprintf(stdout, "Codex's background app-server was restarted (pid %d), but its proxy is %s, not Cortex's.\n",
			d.PID, proxyOrNone(d.Proxy))
	default:
		fmt.Fprintf(stdout, "Codex's background app-server was restarted (pid %d), but it still uses Cortex's proxy,\n"+
			"  set somewhere other than %s: in the environment you ran this from, or Codex was started from.\n",
			d.PID, filepath.Join(codexHome, ".env"))
	}
	printCodexUpdater(p, configured, restartCmd, stdout)
}

// printCodexUpdater names an updater that may still have the old routing, since the
// app-server it starts after its next update inherits it.
func printCodexUpdater(p codexProcs, configured bool, restartCmd string, stdout io.Writer) {
	u := p.updater
	switch p.updaterState {
	case codexUnconfirmed:
		// No pid and no kill: agentop could not tell what the process is, so the user is
		// not asked to stop it (#1736 review). Running agentop again would make the same
		// check, and Codex's own restart keeps such a record too, so the fix offered is the
		// one that cannot touch another process: a computer restart ends the old updater,
		// and the fresh one Codex starts reads the new settings.
		fmt.Fprint(stdout, "Could not confirm that Codex's auto-updater picked up this change. If\n"+
			"  `agentop configure codex status` later shows the app-server on its old settings,\n"+
			"  restart your computer.\n")
	case codexNotStopped:
		fmt.Fprintf(stdout, "Codex's auto-updater (pid %d) could not be stopped, so it may still have its old settings,\n"+
			"  and the app-server it starts after an update inherits them. Stop it, then restart the\n"+
			"  app-server so a new updater starts:\n"+
			"    %s\n", u.PID, codexKillThenRestart(u.PID, restartCmd))
	case codexDisagrees:
		stale := "still uses Cortex's proxy"
		if configured {
			stale = "does not use Cortex's proxy"
		}
		fmt.Fprintf(stdout, "Codex's auto-updater (pid %d) %s, and the app-server it starts after an update\n"+
			"  inherits that. Stop it, then restart the app-server so a new updater starts:\n"+
			"    %s\n", u.PID, stale, codexKillThenRestart(u.PID, restartCmd))
	}
}

func proxyOrNone(p string) string {
	if p == "" {
		return "none"
	}
	return p
}

// reportCodexDaemon says how the running daemon compares with the file's routing, without
// restarting it. configured is whether the file routes Codex through Cortex, whose proxy is
// cortexProxy. status asks for a line in every case; enable, finding nothing to change,
// and disable, finding nothing to remove, say only what needs doing or what they confirmed.
//
// It names the updater too when it still has Cortex's proxy and the file does not route
// Codex there: the app-server it starts after its next update would go back to Cortex. One
// without Cortex's proxy after enable needs no line, since an app-server's own dotenv load
// adds the proxy it did not inherit (codexUpdaterPIDRel).
func reportCodexDaemon(codexHome, cortexProxy string, configured, status bool, stdout io.Writer) {
	p := checkCodexProcs(codexHome, cortexProxy, configured)
	restartCmd := codexRestartCmdHere(codexHome)
	d := p.daemon
	switch p.daemonState {
	case codexNotRunning:
		if status {
			fmt.Fprintln(stdout, "Codex's background app-server is not running.")
		}
	case codexUnreadable:
		if status {
			fmt.Fprintf(stdout, "Could not check Codex's background app-server (pid %d): %v.\n", d.PID, d.EnvErr)
		} else {
			fmt.Fprintf(stdout, "Could not check Codex's background app-server (pid %d): %v.\n"+
				"  Restart it to be sure: %s\n", d.PID, d.EnvErr, restartCmd)
		}
	case codexAgrees:
		if configured {
			fmt.Fprintf(stdout, "Codex's background app-server (pid %d) is using Cortex.\n", d.PID)
		} else if status {
			fmt.Fprintf(stdout, "Codex's background app-server (pid %d) is not using Cortex.\n", d.PID)
		}
	case codexDisagrees:
		switch {
		case configured:
			fmt.Fprintf(stdout, "Codex's background app-server (pid %d%s) is running with its old settings,\n"+
				"  so Codex's prompts bypass Cortex. It picks up the change when it restarts:\n"+
				"    %s   # open Codex windows reconnect\n", d.PID, startedSuffix(d), restartCmd)
		case status:
			fmt.Fprintf(stdout, "Codex's background app-server (pid %d) is using Cortex, but %s does not route it there,\n"+
				"  so its next start will not use Cortex.\n", d.PID, filepath.Join(codexHome, ".env"))
		default:
			// disable with nothing of Cortex's to take out: one started before the user
			// changed the file keeps the proxy it started with. Nothing is restarted, since
			// nothing was asked (#1783 review).
			fmt.Fprintf(stdout, "\nCodex's background app-server (pid %d%s) still uses Cortex's proxy, from before\n"+
				"  these changes. It picks up the file when it restarts:\n"+
				"    %s   # open Codex windows reconnect\n", d.PID, startedSuffix(d), restartCmd)
		}
	}
	if !configured && p.updaterState == codexDisagrees {
		printCodexUpdater(p, configured, restartCmd, stdout)
	}
}
