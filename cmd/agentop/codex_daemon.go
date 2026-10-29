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
	PID     int
	Started string // the pid file's processStartTime, as Codex wrote it; may be empty
	Proxy   string // the HTTPS proxy in its environment, as Codex reads it (codexEnvironProxy); "" when none
	EnvErr  error  // why its environment could not be read; nil when it was
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
var codexStopUpdater = func(codexHome string) error {
	u, ok := runningCodexProc(codexHome, codexUpdaterPIDRel)
	if !ok {
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
		PID              int    `json:"pid"`
		ProcessStartTime string `json:"processStartTime"`
	}
	if json.Unmarshal(b, &rec) != nil || rec.PID > math.MaxInt32 || !codexDaemonAlive(rec.PID) {
		return codexDaemon{}, false
	}
	d := codexDaemon{PID: rec.PID, Started: rec.ProcessStartTime}
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
// (codexKeys) whose value is Cortex's, judged as disable judges them (codexIsOurs). Every
// other entry is kept.
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
// applying the change restarts it. It returns that daemon, nil when none runs — the next
// codex starts one with the new file, so there is nothing to restart.
func noteCodexDaemonRestart(codexHome string, stdout io.Writer) *codexDaemon {
	d, ok := runningCodexDaemon(codexHome)
	if !ok {
		return nil
	}
	fmt.Fprintf(stdout, "Codex's background app-server is running (pid %d%s).\n"+
		"  Applying this restarts it: Codex turns already running get up to its shutdown grace\n"+
		"  (a minute by default) to finish, and open Codex windows reconnect.\n",
		d.PID, startedSuffix(d))
	return &d
}

func startedSuffix(d codexDaemon) string {
	if d.Started == "" {
		return ""
	}
	return ", started " + d.Started
}

// finishCodexChange restarts the daemon that was running before enable (configured) or
// disable changed the file, then checks it again and says whether its proxy now matches
// the change: cortexProxy after enable, anything but cortexProxy after disable. was is
// what noteCodexDaemonRestart returned; nil leaves everything alone. The auto-updater is
// stopped first, so the restart starts a fresh one (see codexUpdaterPIDRel), and checked
// after. A restart that fails is reported on stderr; the change itself succeeded, so the
// caller still exits 0.
func finishCodexChange(codexHome, cortexProxy string, want map[string]string, configured bool, was *codexDaemon, stdout, stderr io.Writer) {
	if was == nil {
		return
	}
	if err := codexStopUpdater(codexHome); err != nil {
		// Not fatal: the restart still applies the change to the daemon, and the check
		// below names an updater still on the old settings.
		fmt.Fprintf(stderr, "agentop: %v\n", err)
	}
	fmt.Fprintf(stdout, "Restarting Codex's background app-server (pid %d)...\n", was.PID)
	if err := codexRestartDaemon(codexHome, want); err != nil {
		fmt.Fprintf(stderr, "agentop: could not restart Codex's background app-server (%v).\n"+
			"  It may still be running with its old settings. To apply the change now:\n"+
			"    %s\n", err, codexDaemonRestartCmd)
		return
	}
	reportRestartedDaemon(codexHome, cortexProxy, configured, stdout)
	reportRestartedUpdater(codexHome, cortexProxy, configured, stdout)
}

// reportRestartedUpdater names an updater whose proxy disagrees with the change, since
// the daemon it starts after its next update inherits that proxy. No updater, one whose
// environment cannot be read, or one that agrees, needs no line.
func reportRestartedUpdater(codexHome, cortexProxy string, configured bool, stdout io.Writer) {
	u, ok := runningCodexUpdater(codexHome)
	if !ok || u.EnvErr != nil || cortexProxy == "" || sameProxy(u.Proxy, cortexProxy) == configured {
		return
	}
	stale := "still uses Cortex's proxy"
	if configured {
		stale = "does not use Cortex's proxy"
	}
	fmt.Fprintf(stdout, "Codex's auto-updater (pid %d) %s, and the app-server it starts after an update\n"+
		"  inherits that. Stop it, then restart the app-server so a new updater starts:\n"+
		"    kill %d && %s\n", u.PID, stale, u.PID, codexDaemonRestartCmd)
}

// reportRestartedDaemon says whether the restarted daemon's proxy matches the change.
func reportRestartedDaemon(codexHome, cortexProxy string, configured bool, stdout io.Writer) {
	d, ok := runningCodexDaemon(codexHome)
	switch {
	case !ok:
		fmt.Fprintln(stdout, "Codex's background app-server was restarted, but is not running now. "+
			"It starts with the new settings the next time you run codex.")
	case d.EnvErr != nil:
		fmt.Fprintf(stdout, "Codex's background app-server was restarted (pid %d); its environment could not be checked.\n", d.PID)
	case cortexProxy == "":
		// disable with no proxy of Cortex's taken out: nothing to compare with.
		fmt.Fprintf(stdout, "Codex's background app-server was restarted (pid %d).\n", d.PID)
	case sameProxy(d.Proxy, cortexProxy) == configured:
		if configured {
			fmt.Fprintf(stdout, "Codex's background app-server was restarted (pid %d) and is using Cortex.\n", d.PID)
		} else {
			fmt.Fprintf(stdout, "Codex's background app-server was restarted (pid %d) and no longer uses Cortex.\n", d.PID)
		}
	case configured:
		fmt.Fprintf(stdout, "Codex's background app-server was restarted (pid %d), but its proxy is %s, not Cortex's.\n",
			d.PID, proxyOrNone(d.Proxy))
	default:
		fmt.Fprintf(stdout, "Codex's background app-server was restarted (pid %d), but it still uses Cortex's proxy,\n"+
			"  set somewhere other than %s: in the environment you ran this from, or Codex was started from.\n",
			d.PID, filepath.Join(codexHome, ".env"))
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
// says only what needs doing or what it confirmed.
func reportCodexDaemon(codexHome, cortexProxy string, configured, status bool, stdout io.Writer) {
	d, ok := runningCodexDaemon(codexHome)
	switch {
	case !ok:
		if status {
			fmt.Fprintln(stdout, "Codex's background app-server is not running.")
		}
		return
	case d.EnvErr != nil && status:
		fmt.Fprintf(stdout, "Could not check Codex's background app-server (pid %d): %v.\n", d.PID, d.EnvErr)
		return
	case d.EnvErr != nil:
		fmt.Fprintf(stdout, "Could not check Codex's background app-server (pid %d): %v.\n"+
			"  Restart it to be sure: %s\n", d.PID, d.EnvErr, codexDaemonRestartCmd)
		return
	}
	using := sameProxy(d.Proxy, cortexProxy)
	switch {
	case using && configured:
		fmt.Fprintf(stdout, "Codex's background app-server (pid %d) is using Cortex.\n", d.PID)
	case configured:
		fmt.Fprintf(stdout, "Codex's background app-server (pid %d%s) is running with its old settings,\n"+
			"  so Codex's prompts bypass Cortex. It picks up the change when it restarts:\n"+
			"    %s   # open Codex windows reconnect\n", d.PID, startedSuffix(d), codexDaemonRestartCmd)
	case using && status:
		fmt.Fprintf(stdout, "Codex's background app-server (pid %d) is using Cortex, but %s does not route it there,\n"+
			"  so its next start will not use Cortex.\n", d.PID, filepath.Join(codexHome, ".env"))
	case status:
		fmt.Fprintf(stdout, "Codex's background app-server (pid %d) is not using Cortex.\n", d.PID)
	}
}
