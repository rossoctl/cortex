package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
)

// serviceStep starts the proxy: under launchd or systemd through `agentop service
// install`'s runServiceInstall, or, where no supervisor can be used, as a
// background process in its own session.
type serviceStep struct{}

func (serviceStep) name() string { return "started" }

func (s serviceStep) plan(env *setupEnv) (stepPlan, *problem) {
	p := stepPlan{label: "started"}
	pidFile := filepath.Join(env.cortexDir, "proxy.pid")
	h, prob := s.preflightPorts(env)
	if prob != nil {
		return p, prob
	}
	env.unsupervised = env.opts.noService
	note := ""
	sup := strings.Fields(supervisorName(env.goos))[0]
	if !env.unsupervised && !setupSupervisorUsable(env.goos) {
		env.unsupervised = true
		note = " — " + sup + " can't be used here"
	}
	// Pins, not Bob's rate: a running proxy reloads pricing from the file itself.
	changing := len(env.binaryChanges) > 0 || env.configPinsPending || env.configFresh || env.opts.restart
	if env.unsupervised {
		// Ours, but not the background proxy proxy.pid records: a supervisor runs
		// this Cortex, and a second copy would only crash-loop on its ports.
		if h.known && supervisedHolder(pidFile, h.pid) {
			if !changing {
				p.done = true
				p.advice = &problem{reason: "Cortex is already running under " + sup + "; not starting a second copy"}
				env.unsupervised = false // the Cortex that serves is supervised
				return p, nil
			}
			if env.opts.noService {
				return p, &problem{reason: "a supervised Cortex is running here, and --no-service would start a second copy",
					fix: []string{"remove it first: agentop service uninstall", "or run agentop setup without --no-service"}}
			}
			// service stop asks the supervisor too, so it is run from there as well.
			return p, &problem{reason: "a supervised Cortex is running here, and this environment cannot manage " + sup,
				fix: []string{"run agentop setup from a normal terminal", "or stop it first, from a normal terminal: agentop service stop"}}
		}
		if _, running := proxyRunning(pidFile); running && !changing {
			p.done, p.doneMsg = true, "in the background (no supervisor)"
			return p, nil
		}
		p.verb, p.what = "run", "the cortex proxy"
		// cortex --supervise restarts its proxy after a crash; nothing starts it at login.
		p.where = "unsupervised · won't restart after a reboot" + note
		return p, nil
	}
	if !env.configFresh {
		if sp, err := resolveServicePaths(env.configPath(), "", filepath.Join(env.binDir, "cortex")); err == nil {
			// Accepted: an endpoint that hangs costs ~2.5s here (waitHealthy's 2s client
			// timeout, then its pause), and one slower than 2s reads as not serving.
			env.priorService = serviceInstalled(sp) && sp.healthURL != "" && serviceLoaded(env.goos) && waitHealthy(sp.healthURL, time.Second)
			// serviceIsCurrent only behind priorService: on a job that is not running it
			// polls the supervisor for its full readiness timeout.
			if env.priorService && !changing && serviceIsCurrent(sp) {
				p.done, p.doneMsg = true, supervisorName(env.goos)+" · healthy"
				return p, nil
			}
		}
	}
	addr, _ := forwardListener(env) // a fresh install's built-in config gives the default
	p.verb, p.what, p.where = "run", "the cortex proxy", supervisorName(env.goos)+" · "+addr
	return p, nil
}

// forwardHold is the process holding the config's forward port, when lsof or ss
// can name it. Past preflightPorts, a known holder is ours.
type forwardHold struct {
	pid   int
	known bool
}

// preflightPorts is install.sh's port preflight for a fresh install, and its
// foreign-proxy check for an existing one — asked before consent here, rather than
// after a failed service install. The existing config's forward port is the one
// it names, so a user who moved the ports is checked on the new ones.
func (serviceStep) preflightPorts(env *setupEnv) (forwardHold, *problem) {
	if env.configFresh {
		var busy []string
		for _, port := range cortexPorts {
			if portInUse(port) {
				busy = append(busy, port)
			}
		}
		if len(busy) == 0 {
			return forwardHold{}, nil
		}
		reason := "port " + busy[0] + " is already in use by something else"
		if len(busy) > 1 {
			reason = "ports " + portList(busy) + " are already in use by something else"
		}
		return forwardHold{}, &problem{reason: reason,
			fix: []string{"free it, or change the ports in " + env.tilde(env.configPath()) + ", then " + env.rerun()}}
	}
	addr, port := forwardListener(env)
	pid, exe, ok := portHolder(port)
	if !ok {
		return forwardHold{}, nil
	}
	if ourProxy(env.binDir, filepath.Join(env.cortexDir, "proxy.pid"), pid, exe) {
		return forwardHold{pid: pid, known: true}, nil
	}
	fix := []string{fmt.Sprintf("kill %d", pid)}
	switch job := holderJob(env.goos, pid); {
	case job != "": // a kill would only have its supervisor start it again
		fix = []string{"stop the job that runs it: " + job}
	case env.goos == "darwin":
		fix = append(fix, "or, if it is an older Cortex service: "+launchdBootout(launchdLabel))
	default:
		fix = append(fix, "or, if it is an older Cortex service: systemctl --user disable --now "+systemdUnit)
	}
	return forwardHold{}, &problem{reason: fmt.Sprintf("%s is held by %s (pid %d), not this Cortex", addr, env.tilde(exe), pid), fix: fix}
}

// forwardListener is the existing config's forward proxy address and its port,
// or 127.0.0.1:47600 when the config does not give one that parses.
func forwardListener(env *setupEnv) (addr, port string) {
	sp, err := resolveServicePaths(env.configPath(), "", filepath.Join(env.binDir, "cortex"))
	if err == nil {
		if _, p, serr := net.SplitHostPort(sp.forwardAddr); serr == nil && p != "" {
			return sp.forwardAddr, p
		}
	}
	return "127.0.0.1:47600", "47600"
}

// supervisedHolder reports whether pid, our proxy on the forward port, runs under
// a supervisor rather than as the background proxy proxy.pid records. That proxy
// is cortex --supervise, whose child holds the port, so the pidfile names the
// holder or its parent. When ps cannot name the parent, a live pidfile proxy is
// taken to be it.
//
// Accepted: anything of ours that proxy.pid does not name reads as supervised —
// a cortex run by hand, or a --supervise child its parent left behind — so its
// advice names the supervisor; and a stale proxy.pid whose number now names the
// holder's parent reads as the background proxy.
func supervisedHolder(pidFile string, pid int) bool {
	pf := readPIDFile(pidFile)
	if pf == pid {
		return false
	}
	if ppid, ok := parentPID(pid); ok {
		return ppid != pf
	}
	_, running := proxyRunning(pidFile)
	return !running
}

// serviceLoaded reports whether the supervisor has our job: in launchd's domain,
// running or not, or active or enabled under systemd. `agentop service stop`
// leaves neither; a crash-looping job is either. Unlike supervisorRunning it asks
// once, rather than waiting for the job to come up. A launchctl that cannot tell
// (labelGone's not known) counts as loaded: each caller then claims less — no
// stop to undo, an unload failure reported, no background copy beside it.
//
// Accepted: on Linux a unit stopped by hand with systemctl --user stop is still
// enabled, so it reads as loaded and a failed upgrade's undo leaves the job it
// started running; on macOS a unit on disk never bootstrapped reads as stopped,
// so the undo's stop also disables it. The fake launchd does not model refusing a
// disabled label.
func serviceLoaded(goos string) bool {
	if goos == "darwin" {
		gone, _ := labelGone("gui/" + strconv.Itoa(os.Getuid()) + "/" + launchdLabel) // not known: not gone
		return !gone
	}
	if exec.Command("systemctl", "--user", "is-active", "--quiet", systemdUnit).Run() == nil {
		return true
	}
	return exec.Command("systemctl", "--user", "is-enabled", "--quiet", systemdUnit).Run() == nil
}

// parentPID is ps's ppid for pid, and whether ps answered.
func parentPID(pid int) (int, bool) {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	return n, err == nil && n > 0
}

func (s serviceStep) apply(env *setupEnv, act *checklist.Running) (string, undo, error) {
	if env.unsupervised {
		return s.applyUnsupervised(env)
	}
	sp, err := resolveServicePaths(env.configPath(), "", filepath.Join(env.binDir, "cortex"))
	if err != nil {
		return "", undo{}, err
	}
	unitSnap, err := snapshotFile(sp.unitFile)
	if err != nil {
		return "", undo{}, err
	}
	stampSnap, err := snapshotFile(sp.stampFile)
	if err != nil {
		return "", undo{}, err
	}
	// A unit on disk whose job the supervisor does not have is what `agentop service
	// stop` leaves. The install below loads the job, so the undo stops it again; the
	// stop needs no binary, as the binaries undo runs after this one. Accepted: a job
	// loaded but not serving stays loaded as the install reloaded it; the undo puts
	// back the unit file, not the definition the supervisor read from the new one.
	stopped := unitSnap.existed && !env.priorService && !serviceLoaded(env.goos)
	u := undo{label: "the service", fn: func() error {
		if !unitSnap.existed {
			// A failed install may have loaded nothing, so a failed unload counts only
			// while the job is still loaded. Either way the unit then stays, for the
			// manual fix.
			if err := unloadService(env.goos, sp); err != nil {
				if serviceLoaded(env.goos) {
					return err
				}
			} else if err := waitUnloaded(env.goos); err != nil {
				return err
			}
		}
		err := errors.Join(unitSnap.restore(), stampSnap.restore())
		if stopped {
			if serr := controlService(env.goos, "stop", sp, io.Discard); serr != nil {
				err = errors.Join(err, serr)
			} else {
				err = errors.Join(err, waitUnloaded(env.goos))
			}
		}
		return err
	}, manual: "agentop service uninstall"}
	switch {
	case stopped:
		u.manual = "agentop service stop"
	case unitSnap.existed:
		u.manual = "agentop service install --restart"
	default:
		u.manual = unloadByHand(env, sp.unitFile)
	}
	// Set below, once runServiceInstall has replaced the job: a failure before
	// that leaves the previous Cortex serving, and a reload would only bounce it.
	reloadPrior := func() error {
		err := loadService(env.goos, sp, io.Discard)
		if errors.Is(err, errLingerUnavailable) {
			err = nil
		}
		if err == nil && !waitHealthy(sp.healthURL, serviceReadyTimeout) {
			err = fmt.Errorf("it did not answer %s", sp.healthURL)
		}
		if err != nil && !fileExists(sp.unitFile) {
			// The service undo could not put the unit back, so service restart has
			// nothing to start; setup writes it again.
			env.priorManual = "re-run agentop setup"
		}
		return err
	}
	if act != nil {
		act.Wait("the proxy to answer /healthz", serviceReadyTimeout)
	}
	var out, errb bytes.Buffer
	// The config step ran the pins migration, so runServiceInstall's own finds
	// nothing to do and would skip the restart that puts the new pins live.
	restart := env.opts.restart || env.configPinsChanged
	mark := markLog(sp.logFile)
	adopt := adoptablePID(sp)
	adoptExe, adoptSeen := sp.binary, "" // or what it ran, where that can be told: v0.7.0's authbridge-proxy, say
	if adopt > 0 {
		if adoptSeen = setupPIDExePath(adopt); filepath.IsAbs(adoptSeen) {
			adoptExe = adoptSeen
		}
	}
	res := runServiceInstall(sp, adopt, restart, false, &out, &errb)
	switch {
	case env.priorService:
		if res.replaced {
			env.priorDesc, env.restorePrior = "the previous Cortex is running again · healthy", reloadPrior
		}
	// The background proxy runServiceInstall stopped to put the service in its place.
	// With no supervised Cortex to reload instead, a rollback starts it again. Only
	// once it is gone: a failure before the stop leaves it running, and a second copy
	// would take proxy.pid from it. Asked up to a minute after the stop, so a live pid
	// that now runs something else is gone too.
	case adopt > 0 && (!alive(adopt) || setupPIDExePath(adopt) != adoptSeen):
		env.priorDesc = "the previous background Cortex is running again"
		env.priorManual = startByHand(env, adoptExe)
		env.restorePrior = func() error {
			// A new job the service undo could not remove (its unload refused, or its
			// teardown not done in time) holds the ports a background copy needs, or
			// keeps retrying for them; healthy, it also answers the check that copy
			// would be judged by.
			if serviceLoaded(env.goos) {
				return errors.New("the new service is still loaded, so a background copy would only fight it for the ports")
			}
			_, _, err := startUnsupervised(adoptExe, env.cortexDir, resolveHealthURL(env.configPath()))
			return err
		}
	}
	if res.exit != 0 {
		// Accepted: a supervisor that refuses here (exitNoSupervisor, a bootstrap
		// EIO) fails the step rather than falling back to a background proxy, and
		// runServiceInstall's "start it yourself" stands above the rollback.
		//
		// Read now, before the rollback brings the previous Cortex back to write
		// after them. They are what the log gained during the install: the new
		// version's lines, after, on an upgrade over a serving Cortex, the old one's
		// shutdown lines (accepted: they are not trimmed off).
		reason, detail := lastAgentopError(errb.String())
		kept := detail[:0]
		for _, d := range detail {
			if d == "Last log lines:" { // runServiceInstall's own tail, of the whole log
				break
			}
			// The state it would show is the one the rollback leaves, not this failure.
			if d != "agentop service status shows the current state." {
				kept = append(kept, env.tildeText(d))
			}
		}
		detail = append(kept, logTail(env, sp.logFile, mark)...)
		return "", u, stepError{reason: env.tildeText(reason), detail: detail}
	}
	detail := supervisorName(env.goos)
	switch {
	case res.healthy:
		detail += " · healthy on " + sp.forwardAddr
	case res.alreadyCurrent:
		detail += " · already running"
	default:
		detail += " · started"
	}
	return detail + stderrNotes(env, errb.String()), u, nil
}

// stderrNotes is what a step that applied wrote to stderr, such as service
// install's linger caveat or its $HOME warning, as detail rows: each non-blank
// line after a newline, without the "agentop: " prefix and with HOME as ~.
func stderrNotes(env *setupEnv, stderr string) string {
	var b strings.Builder
	for _, l := range trimAll(strings.Split(stderr, "\n")) {
		b.WriteString("\n" + env.tildeText(strings.TrimPrefix(l, "agentop: ")))
	}
	return b.String()
}

func (serviceStep) applyUnsupervised(env *setupEnv) (string, undo, error) {
	bin := filepath.Join(env.binDir, "cortex")
	pidFile := filepath.Join(env.cortexDir, "proxy.pid")
	health := resolveHealthURL(env.configPath())
	// start_unsupervised's order: v0.7.0's authbridge-proxy first, then ours. Either
	// is stopped, and a rollback starts the binary it ran again; for authbridge-proxy
	// that is after the cleanup step's undo has put it back.
	prior := filepath.Join(env.binDir, "authbridge-proxy")
	old, running := preRenameProxyRunning(env.binDir, pidFile)
	if !running {
		prior = bin
		old, running = proxyRunning(pidFile)
	}
	if running {
		if err := stopPID(old); err != nil {
			return "", undo{}, fmt.Errorf("could not stop the background proxy (pid %d): %w", old, err)
		}
		env.priorDesc = "the previous background Cortex is running again"
		env.priorManual = startByHand(env, prior)
		env.restorePrior = func() error {
			_, _, err := startUnsupervised(prior, env.cortexDir, health)
			return err
		}
	}
	logPath := filepath.Join(env.cortexDir, "proxy.log")
	mark := markLog(logPath)
	pid, healthy, err := startUnsupervised(bin, env.cortexDir, health)
	var se stepError
	if errors.As(err, &se) {
		// The proxy exited: show what it wrote, as a supervised start's failure does.
		se.detail = append([]string{"see " + env.tilde(logPath)}, logTail(env, logPath, mark)...)
		err = se
	}
	u := undo{label: "the background proxy", fn: func() error {
		if alive(pid) {
			if err := stopPID(pid); err != nil {
				return err
			}
		}
		return removeIfExists(pidFile)
	}, manual: "kill $(cat " + env.tilde(pidFile) + ")"}
	if err != nil {
		return "", u, err
	}
	env.startedBackground = true
	if healthy {
		return "in the background · healthy", u, nil
	}
	return "in the background · not answering yet; see " + env.tilde(filepath.Join(env.cortexDir, "proxy.log")), u, nil
}

// waitUnloaded waits for a job an unload succeeded on to leave launchd: bootout
// returns while teardown is still going, and the job's supervisor allows the
// proxy up to 20s to drain. It is an error if the job is still there after
// serviceBootoutTimeout, so a service undo that returns nil means the job is gone.
// systemd's disable --now returns once the unit has stopped.
//
// Accepted: the rollback shows no progress line while this waits, as an undo has
// no line of its own to write one on; and a launchctl that cannot tell whether
// the job is there (labelGone's not known) is waited out in full, then reported.
func waitUnloaded(goos string) error {
	if goos != "darwin" {
		return nil
	}
	if !waitBootedOut("gui/"+strconv.Itoa(os.Getuid())+"/"+launchdLabel, serviceBootoutTimeout) {
		return fmt.Errorf("the %s is still shutting down after %s", supervisorName(goos), serviceBootoutTimeout)
	}
	return nil
}

// unloadByHand finishes removing a job setup loaded, from a shell: the
// supervisor's own commands, as agentop itself may be gone by then (a fresh
// install's rollback removes it). The unit goes after the unload, as systemd
// unloads by it, and systemd then rereads its unit files.
func unloadByHand(env *setupEnv, unit string) string {
	if env.goos == "darwin" {
		return launchdBootout(launchdLabel) + "; rm -f " + env.tilde(unit)
	}
	return "systemctl --user disable --now " + systemdUnit + "; rm -f " + env.tilde(unit) + "; systemctl --user daemon-reload"
}

// launchdBootout is the command that boots label out of this user's launchd
// domain. The uid is spelled out: fish before 3.4 cannot read $(id -u).
func launchdBootout(label string) string {
	return "launchctl bootout gui/" + strconv.Itoa(os.Getuid()) + "/" + label
}

// startByHand is the nearest a shell comes to startUnsupervised: bin in the
// background, under nohup where startUnsupervised gives it a session of its own
// (macOS has no setsid command), its output appended to proxy.log and its pid in
// proxy.pid. It goes through sh -c, so it reads the same from any shell: fish has
// no $!, and sh still expands the ~ in each path.
//
// The binary that ran, rather than `agentop setup --no-service`: the rollback has
// just put back what was there, which for v0.7.0 is authbridge-proxy and no
// agentop at all; and setup runs every step again, then starts ~/.local/bin/cortex
// rather than the binary that ran.
func startByHand(env *setupEnv, bin string) string {
	return "sh -c 'nohup " + env.tilde(bin) + " --local --supervise >> " + env.tilde(filepath.Join(env.cortexDir, "proxy.log")) +
		" 2>&1 & echo $! > " + env.tilde(filepath.Join(env.cortexDir, "proxy.pid")) + "'"
}

// logTailLines caps the proxy.log rows under a failed start.
const logTailLines = 5

// logMark is where the proxy log ended before a start, so that a failure shows
// what the log gained since rather than an older run's lines.
type logMark struct {
	fi   os.FileInfo // nil when there was no log
	size int64
}

func markLog(path string) logMark {
	fi, err := os.Stat(path)
	if err != nil {
		return logMark{}
	}
	return logMark{fi: fi, size: fi.Size()}
}

// logTail is the last logTailLines non-blank lines the log at path gained since
// m, as checklist detail rows: the first labelled proxy.log in the label column,
// HOME shown as ~. A log rotated or truncated since m is read whole. A log that
// gained nothing, or is not there, is one row saying so; one it could not read to
// the end ends on a row saying why.
func logTail(env *setupEnv, path string, m logMark) []string {
	var ring []string
	f, err := os.Open(path) //nolint:gosec // the service's own log
	switch {
	case os.IsNotExist(err):
	case err != nil:
		ring = append(ring, "("+err.Error()+")")
	default:
		defer func() { _ = f.Close() }()
		if fi, err := f.Stat(); err == nil && m.fi != nil && os.SameFile(fi, m.fi) && fi.Size() >= m.size {
			if _, err := f.Seek(m.size, io.SeekStart); err != nil {
				return nil
			}
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			if l := strings.TrimRight(sc.Text(), " \t\r"); strings.TrimSpace(l) != "" {
				ring = append(ring, l)
				if len(ring) > logTailLines {
					ring = ring[1:]
				}
			}
		}
		if err := sc.Err(); err != nil { // a line over 1 MiB, say: the rest goes unread
			ring = append(ring, "(stopped reading: "+err.Error()+")")
		}
	}
	if len(ring) == 0 {
		ring = []string{"(it wrote nothing)"}
	}
	ring = ring[max(0, len(ring)-logTailLines):]
	var rows []string
	for i, l := range ring {
		label := "" // the checklist's label column is 12 wide
		if i == 0 {
			label = "proxy.log"
		}
		rows = append(rows, fmt.Sprintf("%-12s %s", label, env.tildeText(l)))
	}
	return rows
}

// lastAgentopError picks runServiceInstall's failure out of what it wrote to
// stderr: the last "agentop: " line is the reason, the lines after it the detail.
// Accepted: the detail is capped at six lines; the log rows under it are
// logTail's, so the cap cuts only runServiceInstall's own words.
func lastAgentopError(s string) (string, []string) {
	lines := tailLines(s, 1<<10)
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "agentop: ") {
			detail := lines[i+1:]
			if len(detail) > 6 {
				detail = detail[:6]
			}
			return strings.TrimPrefix(lines[i], "agentop: "), detail
		}
	}
	if len(lines) > 0 {
		return lines[len(lines)-1], nil
	}
	return "the service did not start", nil
}
