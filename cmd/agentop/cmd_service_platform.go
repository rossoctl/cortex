package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func supervisorName(goos string) string {
	if goos == "darwin" {
		return "launchd user agent"
	}
	return "systemd user unit"
}

// renderUnit produces the plist or unit file.
//
// Two choices are load-bearing on both platforms:
//
//   - launchd gets unconditional KeepAlive; systemd gets Restart=on-failure.
//     KeepAlive=true is the simpler guarantee for the requirement that matters —
//     come back after a crash — and it costs nothing, because the way to stop this
//     deliberately is `service uninstall`, which boots the job out rather than
//     leaning on a KeepAlive condition. Apple's {SuccessfulExit:false} form is
//     documented in terms of EXIT STATUS, which makes its behaviour on death by
//     signal ambiguous; there is no reason to depend on resolving that.
//
//     systemd needs no such trade: on-failure covers signal death, and a
//     `systemctl stop` is distinguishable from a crash, so a stop stays stopped.
//
//     NOT verified end to end: restart-after-crash could not be observed from the
//     session this was developed in. launchd logged "pending spawn, domain in
//     on-demand-only mode", i.e. it queued the respawn rather than running it —
//     a property of a headless context, not of this plist. Worth one manual
//     `kill -9` from a real GUI login before trusting it.
//
//   - A throttle. A config error is fatal at startup, so without one a bad edit
//     becomes a tight respawn loop.
func renderUnit(p servicePaths) string { return renderUnitFor(runtime.GOOS, p) }

// renderUnitFor takes the OS explicitly so both renderings are testable from
// either platform. Without it the systemd unit would be written on a Mac and
// never exercised until a Linux user hit it.
// xmlStr renders a plist <string> with its content XML-escaped. $HOME and a
// --config path are user-supplied: an "&" or "<" in either produces a plist that
// launchctl bootstrap rejects as malformed.
func xmlStr(v string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(v))
	return b.String()
}

// shQuote single-quotes a systemd ExecStart argument. systemd splits ExecStart on
// whitespace, so an unquoted path containing a space becomes two wrong arguments.
func shQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// renderUnitFor builds the unit for goos.
//
// The macOS plist runs the proxy under --supervise. launchd will not restart these
// agents itself: in a gui/<uid> domain an agent bootstrapped mid-session gets
// "pending spawn, domain in on-demand-only mode", and neither KeepAlive nor
// StartInterval nor RunAtLoad fires (measured on macOS 26.5.2, active Aqua session,
// approved in Login Items, via both bootstrap and legacy load -w). Only kickstart
// starts it. So launchd starts a supervisor and the supervisor restarts the proxy.
// KeepAlive stays in the plist for the cases launchd does honour, such as a
// login-time load; it is no longer what crash recovery depends on.
//
// The systemd unit deliberately does NOT use --supervise: Restart=on-failure works
// there and counts restarts against StartLimit, which a nested supervisor would hide.
func renderUnitFor(goos string, p servicePaths) string {
	if goos == "darwin" {
		// HOME is set explicitly: the config interpolates ${HOME} at load, and an
		// agent's environment is minimal enough not to rely on inheritance.
		//
		// ExitTimeOut=5 is launchd's own default for this job, measured, and pinned
		// so the order of the stop does not rest on a value Apple documents only as
		// "system-defined". When it runs out launchd SIGKILLs the supervisor and only
		// SIGTERMs the rest of the process group — which a proxy that has already
		// taken its one SIGTERM ignores, so it lives on, orphaned, holding its ports.
		// The supervisor's own kill (superviseStopGrace, 3s, cmd/cortex/supervise.go)
		// therefore has to land first, and the proxy closes rather than drains so it
		// is gone long before either. A supervisor that waited 20s made every restart
		// with a request in flight crash-loop its replacement on the bind.
		//
		// No ProcessType: Background confines the job to the efficiency cores, and every
		// request an agent sends waits on this process. Decoding one 16MB archive segment
		// took 1.0s at normal priority and 3.9-4.1s under `taskpolicy -b`.
		return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + launchdLabel + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + xmlStr(p.binary) + `</string>
    <string>--supervise</string>
    <string>--config</string>
    <string>` + xmlStr(p.configFile) + `</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict><key>HOME</key><string>` + xmlStr(p.home) + `</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>AgentopVersion</key><string>` + xmlStr(version) + `</string>
  <key>StandardOutPath</key><string>` + xmlStr(p.logFile) + `</string>
  <key>StandardErrorPath</key><string>` + xmlStr(p.logFile) + `</string>
  <key>ExitTimeOut</key><integer>5</integer>
</dict>
</plist>
`
	}
	// StartLimit* live in [Unit], not [Service]: systemd moved them in v229 and
	// deprecates them in [Service], where they can be ignored outright — silently
	// voiding the crash-loop throttle they exist to provide. It gives up rather than
	// loop forever, leaving a failed unit that `systemctl --user status` reports.
	//
	// No After=network-online.target: that target is not part of a user manager, and
	// every listener here is loopback, so ordering against the network would be a
	// dependency that never arrives.
	//
	// TimeoutStopSec=5: the plist's ExitTimeOut, for the same stop. A local install's
	// proxy closes its listeners on SIGTERM rather than draining them (cmd/cortex/main.go)
	// and is gone in well under a second, so this only ever ends one that hangs — which
	// systemd's own default would let hold a restart for 90s. There is no supervisor in
	// between here: KillMode=control-group signals the proxy directly.
	return `[Unit]
Description=Cortex local proxy (cortex)
Documentation=https://github.com/rossoctl/cortex
X-AgentopVersion=` + version + `
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
ExecStart=` + shQuote(p.binary) + ` --config ` + shQuote(p.configFile) + `
Restart=on-failure
RestartSec=10
TimeoutStopSec=5
StandardOutput=append:` + p.logFile + `
StandardError=append:` + p.logFile + `

[Install]
WantedBy=default.target
`
}

func loadService(goos string, p servicePaths, progress io.Writer) error {
	if goos == "darwin" {
		uid := strconv.Itoa(os.Getuid())
		target := "gui/" + uid + "/" + launchdLabel
		// Clear any disable left by `service stop`: a disabled label cannot be
		// bootstrapped, so without this a stop would make every later start and
		// install fail for a reason nothing on screen explains.
		_ = exec.Command("launchctl", "enable", target).Run() //nolint:errcheck

		// bootout, keeping its output: a REFUSED bootout and a SLOW one both leave the
		// label in the domain, and telling someone to "try again" is only right for the
		// slow one. Its error alone is not enough to distinguish them — it also fails
		// when nothing was loaded, which is the common case — so the output is kept and
		// only consulted if the label is still there afterwards.
		bootoutOut, bootoutErr := exec.Command("launchctl", "bootout", target).CombinedOutput()

		// WAIT for it to actually leave the domain. `launchctl bootout` returns before
		// teardown finishes, and bootstrapping into a domain that still holds the label
		// fails with "Bootstrap failed: 5: Input/output error" — which is what a real
		// upgrade produced: the running service could not be replaced, the install
		// rolled back, and Cortex was left stopped.
		//
		// Our teardown is not instant: bootout SIGTERMs the supervisor, which forwards
		// to the proxy and waits up to superviseStopGrace (3s) before insisting — and a
		// previous release's supervisor waited out a 15s drain until launchd's exit
		// timeout cut it off. A trivial job dies fast enough to hide this, which is why
		// every test that started from nothing or ran uninstall first passed.
		if !waitBootedOutf(target, serviceBootoutTimeout, progress) {
			if bootoutErr != nil && !strings.Contains(string(bootoutOut), "No such process") {
				return fmt.Errorf("could not remove the previous %s: %v: %s",
					supervisorName(goos), bootoutErr, strings.TrimSpace(string(bootoutOut)))
			}
			return fmt.Errorf("the previous %s is still shutting down after %s; "+
				"run `agentop service status`, then try again", supervisorName(goos), serviceBootoutTimeout)
		}

		// Retried on EIO, re-checking the domain each time. Without the re-check the
		// claim that bootstrap is idempotent here would hold for the first attempt
		// only: an attempt that registers the label and THEN fails leaves the next one
		// returning "File exists" rather than EIO.
		for attempt := 1; ; attempt++ {
			out, err := exec.Command("launchctl", "bootstrap", "gui/"+uid, p.unitFile).CombinedOutput()
			if err == nil {
				break
			}
			if attempt >= 3 || !strings.Contains(string(out), "Input/output error") {
				return fmt.Errorf("launchctl bootstrap failed: %v: %s", err, strings.TrimSpace(string(out)))
			}
			time.Sleep(time.Duration(attempt) * time.Second)
			if !waitBootedOutf(target, serviceBootoutTimeout, progress) {
				return fmt.Errorf("launchctl bootstrap failed and the label is still "+
					"registered: %v: %s", err, strings.TrimSpace(string(out)))
			}
		}
		// bootstrap REGISTERS the job; it does not reliably start it. Observed on a
		// real install: the agent loaded, `state = not running`, nothing served, and
		// Claude Code was broken until an explicit kickstart — RunAtLoad
		// notwithstanding. launchd's log said "pending spawn, domain in
		// on-demand-only mode", so whether a given session spawns at bootstrap
		// depends on the domain's state. Start it deliberately instead of depending
		// on that.
		if out, err := exec.Command("launchctl", "kickstart", "-p", "gui/"+uid+"/"+launchdLabel).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl kickstart failed: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl not found — this system has no systemd (WSL1 or a container?).\n" +
			"  Remove the unit file and start the proxy yourself instead")
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("systemctl", "--user", "enable", systemdUnit).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user enable %s: %v: %s", systemdUnit, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("systemctl", "--user", "restart", systemdUnit).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user restart %s: %v: %s", systemdUnit, err, strings.TrimSpace(string(out)))
	}
	// Without lingering, a user unit stops at logout — which defeats the point on a
	// headless or SSH-only box. Best-effort: an unprivileged user needs polkit for it.
	//
	// uid, not $USER: a supervisor environment may not set USER at all, and the uid is
	// already what every other call here uses. loginctl accepts either.
	//
	// Record it only when WE turn it on, so uninstall can undo exactly that. Linger is
	// a per-user setting shared with every other user unit, so disabling it
	// unconditionally on uninstall could stop services that have nothing to do with
	// Cortex.
	uid := strconv.Itoa(os.Getuid())
	if !lingerEnabled(uid) {
		if out, err := exec.Command("loginctl", "enable-linger", uid).CombinedOutput(); err != nil {
			// Not fatal — the unit is loaded and serving — but it does mean the service
			// stops at logout, so it must not be reported as surviving one.
			return lingerFailure(strings.TrimSpace(string(out)), polkitPresent())
		}
		_ = os.WriteFile(lingerMarker(p), []byte("enabled by agentop\n"), 0o600) //nolint:errcheck
	}
	return nil
}

// errLingerUnavailable means the unit loaded but will not outlive a logout. It is
// what every linger failure wraps, so callers test for it with errors.Is; the
// wrapping message is what names the cause.
var errLingerUnavailable = errors.New(
	"loaded, but lingering could not be turned on: Cortex will stop when you log out")

// lingerFailure explains why `loginctl enable-linger` failed and what fixes it.
//
// An unprivileged user cannot turn lingering on alone. logind asks polkit whether
// org.freedesktop.login1.set-self-linger is allowed, and systemd's policy for it
// allows anyone, without a password. That policy is read only by polkitd, though,
// which ships in a package of its own that systemd merely suggests. Without it
// there is no one to ask, and logind's fallback for this action is to deny, so
// loginctl prints "Access denied". A minimal Debian install with only the SSH
// server is that case, so the headless box lingering exists for is where it fails.
//
// The two causes need different fixes, so it matters which one this is. With
// polkitd absent, installing it lets this user turn lingering on by themselves.
// With it present, the refusal is a local policy decision, and only an
// administrator can override it. Root never asks polkit, so `sudo loginctl
// enable-linger` works in either case.
//
// The advice runs loginctl directly rather than re-running setup. Lingering is
// only attempted while loading the unit, and a re-run that finds Cortex installed
// and healthy loads nothing, so after the polkit install it would change nothing
// and say all is well.
//
// Polkit is blamed only when loginctl says authorization was refused. logind can
// also fail after polkit has said yes, writing /var/lib/systemd/linger, and then
// neither polkit fix would help. Any other failure gets loginctl's own words and
// the root fix alone.
//
// detail is loginctl's own output; present is polkitPresent's answer.
func lingerFailure(detail string, present bool) error {
	if detail == "" {
		detail = "no reason given"
	}
	admin := "    sudo loginctl enable-linger " + lingerTarget()
	switch {
	case !authRefused(detail):
		return fmt.Errorf("%w.\n"+
			"  loginctl enable-linger: %s\n"+
			"  An administrator can turn it on for you:\n"+
			"%s",
			errLingerUnavailable, detail, admin)
	case present:
		return fmt.Errorf("%w.\n"+
			"  loginctl enable-linger: %s\n"+
			"  polkit refused it, so this system's policy does not let users keep their own\n"+
			"  services running. An administrator can turn it on for you:\n"+
			"%s",
			errLingerUnavailable, detail, admin)
	}
	return fmt.Errorf("%w.\n"+
		"  loginctl enable-linger: %s\n"+
		"  polkit is not installed, and logind needs it to let a user turn lingering on.\n"+
		"  Install it, then turn lingering on:\n"+
		"    sudo apt install polkitd    # Debian and Ubuntu; elsewhere the package is polkit\n"+
		"    loginctl enable-linger\n"+
		"  or have an administrator turn it on for you:\n"+
		"%s",
		errLingerUnavailable, detail, admin)
}

// authRefused reports whether loginctl's error is logind refusing authorization,
// rather than failing after it. These are systemd's own fixed strings:
// bus_error_message prints "Access denied" for org.freedesktop.DBus.Error.AccessDenied,
// and bus-polkit.c's "Interactive authentication required." is what a non-interactive
// caller gets when polkit would have prompted. Ubuntu words the latter as "Access
// denied as the requested operation requires interactive authentication", which the
// first match covers. A rewording in some later systemd lands in the other branch,
// whose root fix works whatever the cause.
func authRefused(detail string) bool {
	d := strings.ToLower(detail)
	return strings.Contains(d, "access denied") || strings.Contains(d, "interactive authentication required")
}

// lingerTarget names the account the administrator fix must enable: the one this
// process runs as, which loadService passed to loginctl as a uid. A name reads
// better than a uid to whoever runs the command, but it is looked up from the uid,
// not taken from $USER, which can name someone else (su without -l keeps it). The
// uid itself, which loginctl accepts too, when the lookup fails.
func lingerTarget() string {
	uid := strconv.Itoa(os.Getuid())
	if u, err := user.LookupId(uid); err == nil && u.Username != "" {
		return u.Username
	}
	return uid
}

// polkitPresent reports whether polkitd can answer logind on the system bus:
// running, or installed so D-Bus starts it on first use. `busctl list` shows both
// kinds of name, so one call covers them. busctl ships with systemd itself, so it
// is there wherever loginctl is. An answer that cannot be read counts as present,
// which leaves the advice pointing at an administrator rather than claiming
// polkit is missing.
func polkitPresent() bool {
	out, err := exec.Command("busctl", "--system", "--no-legend", "--no-pager", "list").Output()
	if err != nil {
		return true
	}
	for _, l := range strings.Split(string(out), "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] == "org.freedesktop.PolicyKit1" {
			return true
		}
	}
	return false
}

// lingerMarker records that we enabled lingering, so uninstall undoes only that.
func lingerMarker(p servicePaths) string {
	return filepath.Join(filepath.Dir(p.configFile), "linger-enabled-by-agentop")
}

// lingerEnabled reports whether lingering is already on. A parse failure reads as
// "already on", which errs toward leaving the user's setting alone.
func lingerEnabled(uid string) bool {
	out, err := exec.Command("loginctl", "show-user", uid, "--property=Linger").Output()
	if err != nil {
		return true
	}
	return !strings.Contains(strings.ToLower(string(out)), "linger=no")
}

func unloadService(goos string, p servicePaths) error {
	if goos == "darwin" {
		uid := strconv.Itoa(os.Getuid())
		// "No such process" is a label already out of the domain — what `service stop`
		// leaves, and the state this wants — so, as in loadService and controlService,
		// it is not an error. Reported, it read as a failed uninstall (#1255).
		out, err := exec.Command("launchctl", "bootout", "gui/"+uid+"/"+launchdLabel).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "No such process") {
			return fmt.Errorf("launchctl bootout: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil // nothing could have been loaded
	}
	// Undo lingering only if we are the ones who enabled it.
	if marker := lingerMarker(p); marker != "" {
		if _, serr := os.Stat(marker); serr == nil {
			_ = exec.Command("loginctl", "disable-linger", strconv.Itoa(os.Getuid())).Run() //nolint:errcheck
			_ = os.Remove(marker)                                                           //nolint:errcheck
		}
	}
	if out, err := exec.Command("systemctl", "--user", "disable", "--now", systemdUnit).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user disable --now %s: %v: %s", systemdUnit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runningPID returns the pid from a pidfile only when the probe says the proxy
// is running. Built on setup_probe's proxyRunning — the set install.sh uses —
// so the service path and the setup path can no longer disagree about whether
// the proxy is running (#1565): the private copy here returned 0 whenever ps
// failed, while setup's deliberately sandbox-blind check reported the proxy
// running, and a hand-started proxy the install path then failed to see and
// stop crash-looped the supervised copy against the port.
//
// The trade-off that blindness buys, stated so no one "fixes" it back: when ps
// cannot see the process (seatbelt sandboxes, hidepid), the pidfile is trusted
// as-is, exactly as install.sh trusts it. That means a stale pidfile naming a
// recycled pid can be signalled here — the price of never failing to stop a
// proxy ps is merely blind to. Tighten both call sites together or neither.
func runningPID(pidFile string) int {
	if pid, ok := proxyRunning(pidFile); ok {
		return pid
	}
	return 0
}

// adoptablePID is runningPID plus the pre-rename proxy: a live pidfile process
// whose executable is v0.7.0's authbridge-proxy, which an upgrade adopts
// rather than fight for the port. preRenameProxyRunning is setup_probe's
// version of the same check.
func adoptablePID(p servicePaths) int {
	if pid := runningPID(p.pidFile); pid > 0 {
		return pid
	}
	if pid, ok := preRenameProxyRunning(filepath.Dir(p.binary), p.pidFile); ok {
		return pid
	}
	return 0
}

// samePath reports whether a and b name one file: equal as text, or equal once
// both are resolved the way install.sh's GNU `readlink -f` resolves them.
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := resolvePath(a)
	rb, errB := resolvePath(b)
	return errA == nil && errB == nil && ra == rb
}

// resolvePath is GNU `readlink -f`: p with every link resolved, where the last
// component alone may be missing, as a binary removed or not yet installed is.
// The parent comes from filepath.Split, not filepath.Dir: Dir cleans the path
// first, dropping "link/.." before link is followed, so <ours>/link/../x would
// resolve into <ours> instead of the link target's parent.
func resolvePath(p string) (string, error) {
	r, err := filepath.EvalSymlinks(p)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return r, err
	}
	parent, base := filepath.Split(p)
	dir, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, base), nil
}

// stopPID asks politely, then waits. A proxy from before restarts stopped draining
// allows itself 15s to, and this may be stopping one, so it waits longer than that
// before giving up rather than reporting success on a process that still holds the
// ports. A current one is gone in well under a second.
func stopPID(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	for i := 0; i < 90; i++ {
		if err := syscall.Kill(pid, 0); err != nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("still alive after 18s")
}

func waitHealthy(url string, within time.Duration) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		resp, err := c.Get(url) //nolint:noctx // bounded by Timeout
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func lastLines(path string, n int) []string {
	f, err := os.Open(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return []string{"(no log at " + path + ")"}
	}
	defer f.Close()
	var ring []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		ring = append(ring, sc.Text())
		if len(ring) > n {
			ring = ring[1:]
		}
	}
	if len(ring) == 0 {
		return []string{"(log is empty)"}
	}
	return ring
}

// dialableAddr turns a bind address into one a client can connect to. Named apart
// from the existing dialable() predicate in local_endpoint.go, which answers a
// different question.
func dialableAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}

// controlService maps stop/start/restart onto the platform's supervisor.
func controlService(goos, action string, p servicePaths, progress io.Writer) error {
	if goos == "darwin" {
		target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + launchdLabel
		switch action {
		case "stop":
			// Two steps, because either alone is not a stop.
			//
			// `launchctl stop` is undone by KeepAlive within seconds. bootout removes
			// the job from the running domain — but the plist stays in
			// ~/Library/LaunchAgents, which launchd bootstraps again at next login, and
			// RunAtLoad then starts Cortex right back up. A "stop" that quietly undoes
			// itself when you log in is worse than no stop command, so pair it with
			// launchctl disable, which persists in the per-user disabled database.
			out, err := exec.Command("launchctl", "bootout", target).CombinedOutput()
			if err != nil && !strings.Contains(string(out), "No such process") {
				return fmt.Errorf("launchctl bootout: %v: %s", err, strings.TrimSpace(string(out)))
			}
			if out, derr := exec.Command("launchctl", "disable", target).CombinedOutput(); derr != nil {
				return fmt.Errorf("launchctl disable (stop would not survive a login): %v: %s",
					derr, strings.TrimSpace(string(out)))
			}
			return nil
		case "start":
			return loadService(goos, p, progress) // loadService clears the disable
		default: // restart
			_ = exec.Command("launchctl", "bootout", target).Run() //nolint:errcheck
			return loadService(goos, p, progress)
		}
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl not found; this system has no systemd")
	}
	// `systemctl --user stop` leaves the unit ENABLED, so it comes back at the next
	// boot exactly as launchd's plist does. Use the persistent forms for stop/start so
	// both platforms mean the same thing by those words; restart stays transient.
	args := []string{"--user", action, systemdUnit}
	switch action {
	case "stop":
		args = []string{"--user", "disable", "--now", systemdUnit}
	case "start":
		args = []string{"--user", "enable", "--now", systemdUnit}
	}
	if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// supervisorRunning asks the supervisor whether OUR job is up, which health alone
// cannot establish: an unadopted proxy keeps the ports, the supervised copy
// crash-loops on the bind, and the probe succeeds against the survivor.
func supervisorRunning(goos string, p servicePaths) (bool, string) {
	if goos == "darwin" {
		target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + launchdLabel
		// Poll rather than sample once. Immediately after a kickstart the job passes
		// through transient states — "xpcproxy" while launchd's exec helper is still
		// in the middle of the exec — and reading one of those as a verdict failed an
		// install whose service was merely still starting.
		deadline := time.Now().Add(serviceReadyTimeout)
		last := "no state reported"
		for {
			out, err := exec.Command("launchctl", "print", target).CombinedOutput()
			if err != nil {
				last = "launchctl print: job not found"
			} else {
				for _, line := range strings.Split(string(out), "\n") {
					line = strings.TrimSpace(line)
					if strings.HasPrefix(line, "state = ") {
						st := strings.TrimPrefix(line, "state = ")
						if st == "running" {
							return true, "state = running"
						}
						last = "state = " + st
						break
					}
				}
			}
			if time.Now().After(deadline) {
				return false, last
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return true, "" // no systemd to ask; do not block on a check we cannot make
	}
	out, err := exec.Command("systemctl", "--user", "is-active", systemdUnit).Output()
	st := strings.TrimSpace(string(out))
	if err != nil && st == "" {
		return false, "systemctl is-active gave no answer"
	}
	return st == "active", "is-active = " + st
}

// establishedConns counts live TCP connections to addr's port, best-effort.
//
// Stopping the proxy cannot be made graceful from this side: HTTPS_PROXY is baked
// into each Claude Code process's environment at startup, so a running session has
// no way to fall back to a direct connection and `claude-code disable` cannot reach
// it. The stop therefore breaks every in-flight session, and the failure surfaces as
// a bare CURLE_COULDNT_CONNECT that looks like a Cortex bug. Naming the number of
// attached clients first turns that into a five-second diagnosis.
//
// Returns -1 when it cannot tell (no lsof), so the caller can stay silent rather
// than claim zero.
func establishedConns(addr string) int {
	if addr == "" {
		return -1
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return -1
	}
	if _, err := exec.LookPath("lsof"); err != nil {
		return -1
	}
	cmd := exec.Command("lsof", "-nP", "-iTCP:"+port, "-sTCP:ESTABLISHED")
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err != nil {
		// lsof exits 1 both when nothing matches and on real failures (permission,
		// for one). Only the silent case is a true zero; anything that complained on
		// stderr is unknown, because reporting "0 attached" when we could not look
		// would be a confident wrong answer.
		if len(out) == 0 && errBuf.Len() == 0 {
			return 0
		}
		return -1
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" || strings.HasPrefix(line, "COMMAND") {
			continue
		}
		n++
	}
	return n
}

// rotateLog keeps one previous generation once the log passes maxBytes.
//
// One unrotated file on a permanently-on service is a slow leak on its own — the
// proxy logs a line per inference request and response — and a fatal config error
// turns it into a fast one: unconditional KeepAlive with ThrottleInterval 10 means
// roughly six restarts a minute, each writing its startup banner. Rotating at
// start-of-run bounds exactly that case, because every one of those restarts passes
// through here.
func rotateLog(path string, maxBytes int64) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() <= maxBytes {
		return
	}
	// Rename, not truncate: the supervisor opens the path per spawn, so the next
	// start gets a fresh file while anything still holding the old inode keeps
	// writing there harmlessly.
	_ = os.Rename(path, path+".1") //nolint:errcheck
}

// unitWriterVersion reports which agentop wrote the installed unit, or "" if it
// carries no stamp (written before stamping existed).
//
// The unit outlives the tool that manages it. A newer build can write a plist while
// an older agentop stays on PATH, and that older one answers `service status` with
// "unknown subcommand" — so the launchd artifact is live and unmanageable, with no
// hint anywhere that the two disagree. Observed on a real machine.
func unitWriterVersion(unitFile string) string {
	b, err := os.ReadFile(unitFile) //nolint:gosec // path we wrote
	if err != nil {
		return ""
	}
	body := string(b)
	for _, marker := range []string{"<key>AgentopVersion</key><string>", "X-AgentopVersion="} {
		i := strings.Index(body, marker)
		if i < 0 {
			continue
		}
		rest := body[i+len(marker):]
		end := strings.IndexAny(rest, "<\n")
		if end < 0 {
			continue
		}
		return strings.TrimSpace(rest[:end])
	}
	return ""
}

// writeProxyStamp records the hash of the binary about to be, or just, launched.
//
// Called after the unit is written and after every start or restart, which is what
// makes the stamp mean "the bytes the supervisor last launched" rather than "the bytes
// present at the last install". Those are the same on the install path and diverge on
// every other one — replace the binary, restart, and only this file moves.
//
// Silent on failure, and deliberately so: a missing or stale stamp costs one
// unnecessary restart on the next install, which is the safe direction. Nothing here
// may be the reason a service fails to come up.
func writeProxyStamp(p servicePaths) {
	h := binarySHA256(p.binary)
	if h == "" || p.stampFile == "" {
		return
	}
	// Best effort by design — see the doc comment above.
	_ = writeFileAtomic(p.stampFile, []byte(h+"\n"), 0o600)
}

// readProxyStamp returns the recorded hash, or "" when there is none to read.
func readProxyStamp(p servicePaths) string {
	if p.stampFile == "" {
		return ""
	}
	b, err := os.ReadFile(p.stampFile) //nolint:gosec // path we wrote
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// binarySHA256 hashes the file at path, or returns "" when it cannot be read.
//
// "" is a legitimate answer rather than an error: renderUnit calls this while
// building a unit for a binary that install has already validated, and every
// comparison against the result treats "" as "does not match", which is the safe
// direction for a binary that has gone missing.
func binarySHA256(path string) string {
	f, err := os.Open(path) //nolint:gosec // operator-supplied binary path, validated by install
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck // read-only
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// waitBootedOut polls until the label is gone from its domain.
//
// `launchctl bootout` is asynchronous: it returns while teardown is still in progress,
// and a bootstrap issued in that window fails with EIO. Polling `launchctl print` is
// the only signal available — a non-zero exit means the label is no longer there.
func waitBootedOut(target string, d time.Duration) bool {
	return waitBootedOutf(target, d, nil)
}

// waitBootedOutf is waitBootedOut with a progress line.
//
// Up to 30s of silence right after "Setting up the launchd user agent..." is
// indistinguishable from a hang, and it lands on exactly the people who just hit the
// EIO failure this wait exists to prevent. One line after the first second, so a fast
// teardown — the common case — stays silent.
func waitBootedOutf(target string, d time.Duration, progress io.Writer) bool {
	return waitGone(d, progress, func() bool {
		gone, _ := labelGone(target)
		return gone
	})
}

// labelGone reports whether target is absent from its domain, and whether we could
// tell at all.
//
// "Any non-zero exit means gone" was wrong, and wrong in the direction that hurts:
// launchctl print exits 113 with "Could not find service" when the label really is
// absent, but 125 ("Domain does not support specified action"), 64, or a permission
// denial when it cannot see the domain — which happens in restricted sandboxes. Reading
// those as "gone" made the code march into a bootstrap that then failed with a bare
// EIO, and report the one state we had actually ruled out.
func labelGone(target string) (gone, known bool) {
	out, err := exec.Command("launchctl", "print", target).CombinedOutput()
	if err == nil {
		return false, true // present
	}
	if strings.Contains(string(out), "Could not find service") {
		return true, true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 113 {
		return true, true
	}
	// Anything else: launchctl could not answer. Not gone, and not known.
	return false, false
}

// launchdUsable reports whether this process can manage launchd jobs at all.
//
// Restricted environments — sandboxes, some CI, a shell without a full user session —
// deny the launchd IPC that bootstrap needs. Probing the DOMAIN rather than our label
// separates "cannot talk to launchd" from "our job is not loaded", so the failure can be
// explained instead of surfacing as "Bootstrap failed: 5: Input/output error", which
// names neither cause nor remedy.
func launchdUsable() (bool, string) {
	if runtime.GOOS != "darwin" {
		return true, ""
	}
	if _, err := exec.LookPath("launchctl"); err != nil {
		return false, "launchctl is not on PATH"
	}
	out, err := exec.Command("launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())).CombinedOutput()
	if err == nil {
		return true, ""
	}
	first := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if first == "" {
		first = err.Error()
	}
	return false, first
}

// systemdUsable reports whether this process can drive a systemd --user session.
// launchdUsable's Linux equivalent: `systemctl --user show-environment` is the same
// live probe requireRealSystemd (cmd_service_systemd_integration_test.go) and
// install.sh's shell-level supervisor_usable() already use to distinguish "systemctl
// present but the user manager/D-Bus is unreachable" from a genuinely usable session.
// Unlike launchdUsable, this has no internal GOOS guard: the caller picks which of the
// two to run via serviceManagerUsable, so this stays callable from either host for
// tests, the same reason loadService/controlService/supervisorRunning/unloadService
// take goos explicitly rather than branching on runtime.GOOS themselves.
func systemdUsable() (bool, string) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false, "systemctl is not on PATH"
	}
	out, err := exec.Command("systemctl", "--user", "show-environment").CombinedOutput()
	if err == nil {
		return true, ""
	}
	first := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if first == "" {
		first = err.Error()
	}
	return false, first
}

// serviceManagerUsable is the goos-dispatched preflight serviceInstall calls before
// writing anything to disk. Without it on Linux, the first sign of an unusable
// systemd --user session was systemctl's own failure after the unit was already
// written — the exact bug launchdUsable was already guarding against on macOS.
func serviceManagerUsable(goos string) (bool, string) {
	if goos == "darwin" {
		return launchdUsable()
	}
	return systemdUsable()
}

// waitGone polls gone() until it reports true, or d elapses.
//
// Split from waitBootedOutf so the progress behaviour is testable without launchd. The
// first attempt at testing it could only skip — there is no way to hold a real launchd
// label in a half-torn-down state on demand — and a test that skips is how the bug this
// wait exists to prevent got shipped in the first place.
func waitGone(d time.Duration, progress io.Writer, gone func() bool) bool {
	announced := false
	start := time.Now()
	for {
		if gone() {
			if announced && progress != nil {
				fmt.Fprintln(progress, "  ...stopped.")
			}
			return true
		}
		if !announced && progress != nil && time.Since(start) > time.Second {
			fmt.Fprintf(progress, "  Waiting for the previous Cortex to stop (up to %s)...\n", d)
			announced = true
		}
		if time.Since(start) > d {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}
