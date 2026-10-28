package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This file's two tests deliberately don't mirror TestWaitBootedOut_RealLaunchd's
// shape, because the two platforms don't need the same thing proved.
//
// macOS: launchd does not reliably restart an agent added mid-session (measured
// and documented on renderUnitFor's darwin branch), so this codebase runs its own
// supervisor process (supervise.go) and has launchd supervise THAT instead.
// TestWaitBootedOut_RealLaunchd proves our own supervisor's bootout/restart
// handling — a mechanism this repo had to build because launchd would not do it.
//
// Linux: systemd's Restart=on-failure is trusted to work natively, so
// renderUnitFor's linux branch runs the proxy directly — one process, no
// supervisor. The two tests below instead prove systemd's OWN restart mechanism
// actually behaves as documented: TestSupervisorRestartsAfterCrash_RealSystemd is
// a claim about systemd, not about code this repo wrote — which is also why it's
// simpler than the darwin test: there's no supervisor layer or bootout race to
// reproduce, just the bare Restart=on-failure claim itself.

// isExitCode reports whether err is a process exit with exactly this code.
func isExitCode(err error, code int) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == code
}

// requireRealSystemd skips (or, with AGENTOP_SYSTEMD_TESTS=required, fails) unless
// this process can actually drive a live systemd --user session — covering the same
// ground as TestWaitBootedOut_RealLaunchd's skip guards (cmd_service_bootout_test.go),
// deliberately in a different shape: three categories here (wrong GOOS, a binary
// missing from PATH, no reachable systemctl --user session) versus that test's four,
// since its fourth — launchd refusing to start the test agent in this domain at all —
// has no systemd analog. See this file's header comment for why the two platforms
// don't need the same thing proved in the first place.
//
// That macOS test's env-var escape hatch exists because silent skipping is exactly
// how the bootout-race bug (#880) shipped unexercised. Its own workflow never sets
// the var, though, so the test has skipped in every CI run since it was written.
// The one CI job that runs THIS test should set AGENTOP_SYSTEMD_TESTS=required after
// setting up a real systemd --user session, so it can't fall into the same trap.
func requireRealSystemd(t *testing.T) {
	t.Helper()
	skip := t.Skipf
	if os.Getenv("AGENTOP_SYSTEMD_TESTS") == "required" {
		skip = t.Fatalf
	}
	if runtime.GOOS != "linux" {
		skip("systemd only (GOOS=%s)", runtime.GOOS)
		return
	}
	for _, bin := range []string{"systemctl", "systemd-run"} {
		if _, err := exec.LookPath(bin); err != nil {
			skip("%s not on PATH: %v", bin, err)
			return
		}
	}
	if out, err := exec.Command("systemctl", "--user", "show-environment").CombinedOutput(); err != nil {
		skip("no reachable systemd --user session: %v: %s", err, strings.TrimSpace(string(out)))
		return
	}
}

// slowScript writes a throwaway script that mimics a proxy that takes a moment to
// stop: it ignores nothing, but takes a couple of seconds to actually exit once
// asked to, and otherwise just idles. A trivial script that died instantly
// would hide a slow-teardown bug the same way it did for the darwin bootout race
// (see the comment on TestWaitBootedOut_RealLaunchd).
func slowScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "slow.sh")
	body := "#!/bin/sh\ntrap 'sleep 2; exit 0' TERM\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	return path
}

const restartOnFailureProp = "Restart=on-failure"

// runTransientUnit starts script under a throwaway, uniquely-named unit with the
// same Restart=on-failure our real renderUnitFor writes (RestartSec=1 here, not the
// production 10, purely so the test doesn't wait 10s per restart it triggers), and
// registers its own teardown — stop and reset-failed, so a failed assertion never
// leaves a unit respawning after the test process exits.
//
// Restart=on-failure is hand-copied into the systemd-run args below rather than
// read from renderUnitFor, so nothing would otherwise tie this test to the unit it
// claims to vouch for: delete the property from renderUnitFor's linux branch and
// this test would go on passing, proving a fact about systemd that the shipped
// unit no longer requests. This assertion is the missing link — it fails if the
// real renderer and this test's hand-copied property ever diverge.
func runTransientUnit(t *testing.T, name, script string) {
	t.Helper()
	if u := renderUnitFor("linux", servicePaths{}); !strings.Contains(u, restartOnFailureProp) {
		t.Fatalf("the linux unit no longer sets %s; this test would be vouching for a "+
			"property the real unit doesn't request:\n%s", restartOnFailureProp, u)
	}
	stop := func() {
		// systemd-run transient units are garbage-collected once inactive, so by the
		// time this runs the unit is typically already gone: `stop` on a unit that
		// isn't loaded exits 5, and `reset-failed` on one that's neither failed nor
		// loaded exits 1. Both are the ordinary end of a transient unit's life, not
		// evidence of a leak — confirmed from this suite's own CI output, where both
		// print on every passing run. Logging them unconditionally defeated the point
		// of logging at all: a real leak would read identically to normal. Only
		// anything else is worth surfacing.
		//
		// stop's exit 5 is a narrow, confirmed-benign case, checked by code. reset-failed's
		// exit 1 is systemd's generic failure code, not a specific one — checking it by
		// code would suppress nearly everything this call can produce, including a user
		// bus that goes away mid-run. Matched by message instead, so only the confirmed
		// "unit doesn't exist" case is swallowed.
		if err := exec.Command("systemctl", "--user", "stop", name).Run(); err != nil && !isExitCode(err, 5) {
			t.Logf("cleanup: systemctl --user stop %s: %v", name, err)
		}
		if out, err := exec.Command("systemctl", "--user", "reset-failed", name).CombinedOutput(); err != nil &&
			!strings.Contains(string(out), "not loaded") && !strings.Contains(string(out), "not found") {
			t.Logf("cleanup: systemctl --user reset-failed %s: %v: %s", name, err, strings.TrimSpace(string(out)))
		}
	}
	t.Cleanup(stop)
	// No pre-emptive stop() here: the unit name embeds this process's own pid, unique
	// to this run, so there is no plausible same-named leftover to clear first (unlike
	// the darwin test this mirrors, which uses one fixed label — that's precisely why
	// its pre-emptive bootout is meaningful and this one would not be). Calling it
	// anyway only logs a spurious "cleanup:" failure on every normal passing run,
	// since stopping/reset-failing a unit that was never registered is itself an error.

	args := []string{
		"--user", "--unit=" + name,
		"-p", restartOnFailureProp,
		"-p", "RestartSec=1",
		script,
	}
	if out, err := exec.Command("systemd-run", args...).CombinedOutput(); err != nil {
		t.Fatalf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
	}
}

// currentMainPID reads MainPID once, or 0 if unset/unparseable.
func currentMainPID(name string) int {
	out, err := exec.Command("systemctl", "--user", "show", name, "--property=MainPID", "--value").Output()
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return pid
}

// unitMainPID polls for a MainPID, since it is briefly 0 right after start.
func unitMainPID(t *testing.T, name string, within time.Duration) (int, bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if pid := currentMainPID(name); pid > 0 {
			return pid, true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return 0, false
}

// waitForNewMainPID polls until MainPID is both nonzero and different from oldPID.
// Checking is-active alone is not enough: systemd can still report a unit "active"
// in the brief window right after a kill, before it has noticed the death and
// respawned — is-active going true first, then a same-old-PID read right behind
// it, would misreport a real restart as a failure to restart. Requiring a genuinely
// new PID is the direct claim ("something new is running"), not the reachable proxy
// for it.
func waitForNewMainPID(t *testing.T, name string, oldPID int, within time.Duration) (int, bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if pid := currentMainPID(name); pid > 0 && pid != oldPID {
			return pid, true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return 0, false
}

func unitIsActive(name string) bool {
	out, err := exec.Command("systemctl", "--user", "is-active", name).Output()
	return err == nil && strings.TrimSpace(string(out)) == "active"
}

func waitUntil(within time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if ok() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// TestSupervisorRestartsAfterCrash_RealSystemd drives a real systemd --user session
// to prove Restart=on-failure actually restarts a crashed unit — the assumption
// renderUnitFor's comment states ("systemd needs no such trade: on-failure covers
// signal death") but this repo had never verified against a real systemd, unlike the
// darwin KeepAlive claim, which WAS tested and found false (see the comment on
// renderUnitFor's darwin branch, and TestWaitBootedOut_RealLaunchd).
func TestSupervisorRestartsAfterCrash_RealSystemd(t *testing.T) {
	requireRealSystemd(t)

	unit := "cortex-test-crash-" + strconv.Itoa(os.Getpid()) + ".service"
	runTransientUnit(t, unit, slowScript(t))

	pid, ok := unitMainPID(t, unit, 5*time.Second)
	if !ok {
		t.Fatal("unit never reported a main PID")
	}

	// SIGKILL, not a plain stop: this must bypass the script's own TERM trap
	// entirely, so it looks like a real crash (a segfault, an OOM kill) rather
	// than a deliberate, distinguishable stop — which the next test proves does
	// NOT restart. syscall.Kill instead of exec.Command("kill", ...): requireRealSystemd
	// already gated this on GOOS=linux, so the syscall package is always usable here,
	// and it removes an external-binary dependency requireRealSystemd doesn't guard —
	// on a slim image missing /bin/kill, that would surface as a failed test with
	// AGENTOP_SYSTEMD_TESTS=required set, rather than the environment-problem skip it
	// actually is.
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill -9 %d: %v", pid, err)
	}

	// The authoritative signal is a genuinely new PID, not is-active: is-active can
	// still read "active" in the brief window right after the kill, before systemd
	// has noticed the death and respawned, which would let a same-old-PID read slip
	// through as a false "it restarted."
	if _, ok := waitForNewMainPID(t, unit, pid, 10*time.Second); !ok {
		t.Fatal("no new main PID within 10s — Restart=on-failure did not fire")
	}
	if !unitIsActive(unit) {
		t.Error("got a new PID but the unit does not report active")
	}
}

// TestSupervisorStaysStoppedAfterDeliberateStop_RealSystemd proves the other half
// of the same comment: "a `systemctl stop` is distinguishable from a crash, so a
// stop stays stopped." Restart=on-failure must NOT fire for a deliberate stop, or
// `agentop service stop` would look exactly like the "stop that does not stop" bug
// this whole feature exists to avoid on the launchd side.
func TestSupervisorStaysStoppedAfterDeliberateStop_RealSystemd(t *testing.T) {
	requireRealSystemd(t)

	unit := "cortex-test-stop-" + strconv.Itoa(os.Getpid()) + ".service"
	runTransientUnit(t, unit, slowScript(t))

	if _, ok := unitMainPID(t, unit, 5*time.Second); !ok {
		t.Fatal("unit never reported a main PID")
	}

	if out, err := exec.Command("systemctl", "--user", "stop", unit).CombinedOutput(); err != nil {
		t.Fatalf("systemctl --user stop: %v: %s", err, strings.TrimSpace(string(out)))
	}

	// `systemctl --user stop` already blocked until the stop job completed, so the
	// script's ~2s TERM-trap drain is behind us by the time we get here — this is
	// the first second of the negative assertion, not a grace period. It's tightened
	// to 1s (not the full 4s below) because RestartSec=1 means a wrongly-firing
	// restart would already be visible this early: is-active reads "deactivating"
	// mid-drain and "activating"/"active" only once a new process exists, so this
	// can't mistake the trap's own tail for a restart.
	if waitUntil(1*time.Second, func() bool { return unitIsActive(unit) }) {
		t.Fatal("unit is active within 1s of a deliberate stop — looks like a wrongly-firing restart, not the stop itself")
	}
	// A flat sleep, not a poll, because this asserts a negative: there is no "it
	// happened" event to wait for. The risk this accepts is one-directional — a
	// heavily loaded runner could make this pass when it shouldn't (a slow wrong
	// restart lands after the check), never fail when it shouldn't (nothing here
	// depends on speed for a legitimate pass).
	time.Sleep(4 * time.Second) // past RestartSec=1; a wrongly-firing restart would show by now
	if unitIsActive(unit) {
		t.Error("unit restarted after a deliberate `systemctl stop` — Restart=on-failure should not cover this")
	}
}

// healthStub writes a stand-in for the cortex binary: a script that serves
// /healthz on addr, as the proxy's health listener does, until it is told to stop.
// What `agentop service` decides is all from the unit's state and that one
// endpoint, so this exercises every decision without building the proxy. python3,
// because it is on every CI runner and on the hosts this runs on by hand; the test
// skips without it rather than fail for an environment reason.
func healthStub(t *testing.T, dir, addr string) string {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 not on PATH: %v", err)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cortex")
	body := "#!" + py + " -I\n" +
		"import http.server, signal, sys\n" +
		"signal.signal(signal.SIGTERM, lambda *a: sys.exit(0))\n" +
		"class H(http.server.BaseHTTPRequestHandler):\n" +
		"    def do_GET(self):\n" +
		"        self.send_response(200 if self.path == '/healthz' else 404)\n" +
		"        self.end_headers()\n" +
		"    def log_message(self, *a):\n" +
		"        pass\n" +
		"http.server.HTTPServer(('" + host + "', " + port + "), H).serve_forever()\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	return path
}

// freeLoopbackAddr is a 127.0.0.1 port nothing holds right now.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// TestServiceLifecycle_RealSystemd drives `agentop service` itself — install,
// status, stop, start, restart, a crash, a crash loop and uninstall — against a
// live systemd --user session, and checks what each command reports against what
// systemd says the unit is doing. The two tests above prove systemd's own restart
// behaviour on a throwaway unit; this proves agentop's reading of it, which is
// what #945's "status | start | stop | restart accurate in every state" asks for.
//
// It installs the real cortex.service, the name every agentop command addresses,
// so it refuses to run where one is already installed: that would be someone's
// Cortex, and this test stops and uninstalls what it finds.
func TestServiceLifecycle_RealSystemd(t *testing.T) {
	requireRealSystemd(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	realUnit := filepath.Join(home, ".config", "systemd", "user", systemdUnit)
	if _, err := os.Stat(realUnit); err == nil {
		t.Skipf("%s exists: this account has a Cortex installed, which this test would replace", realUnit)
	}
	if exec.Command("systemctl", "--user", "cat", systemdUnit).Run() == nil {
		t.Skipf("systemd already knows %s; not replacing it", systemdUnit)
	}

	dir := t.TempDir()
	addr := freeLoopbackAddr(t)
	bin := healthStub(t, dir, addr)
	cfg := filepath.Join(dir, "config.yaml")
	body := "mode: proxy-sidecar\nlistener:\n  roles: [forward]\n" +
		"  forward_proxy_addr: " + freeLoopbackAddr(t) + "\n" +
		"  health_addr: " + addr + "\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := resolveServicePaths(cfg, "", bin)
	if err != nil {
		t.Fatal(err)
	}
	if p.unitFile != realUnit {
		t.Fatalf("unit file = %s, want %s", p.unitFile, realUnit)
	}
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "--user", "disable", "--now", systemdUnit).Run()
		_ = exec.Command("systemctl", "--user", "reset-failed", systemdUnit).Run()
		_ = os.Remove(realUnit)
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	})

	run := func(name string, f func(stdout, stderr io.Writer) int) (int, string) {
		t.Helper()
		var out, errb bytes.Buffer
		code := f(&out, &errb)
		t.Logf("%s -> exit %d\n%s%s", name, code, out.String(), errb.String())
		return code, out.String() + errb.String()
	}
	status := func() (int, string) {
		t.Helper()
		return run("status", func(o, _ io.Writer) int { return serviceStatus(p, o) })
	}
	control := func(action string) (int, string) {
		t.Helper()
		return run(action, func(o, e io.Writer) int { return serviceControl(action, p, o, e) })
	}
	expect := func(step string, code int, out string, wantCode int, wants ...string) {
		t.Helper()
		if code != wantCode {
			t.Errorf("%s: exit %d, want %d:\n%s", step, code, wantCode, out)
		}
		for _, w := range wants {
			if !strings.Contains(out, w) {
				t.Errorf("%s: output lacks %q:\n%s", step, w, out)
			}
		}
	}
	unit := func() unitState { return systemdUnitState() }

	code, out := status()
	expect("status before install", code, out, 0, "not installed")

	code, out = run("install", func(o, e io.Writer) int { return serviceInstall(p, true, false, o, e) })
	expect("install", code, out, 0, "healthy")
	if s := unit(); s.active != "active" || s.enabled != "enabled" {
		t.Fatalf("after install systemd says %+v, want active and enabled", s)
	}
	code, out = status()
	expect("status running", code, out, 0, "Cortex is healthy according to")

	code, out = control("stop")
	expect("stop", code, out, 0, "stopped")
	if s := unit(); s.active != "inactive" || s.enabled != "disabled" {
		t.Errorf("after stop systemd says %+v, want inactive and disabled", s)
	}
	code, out = status()
	expect("status after stop", code, out, 0, "Cortex is stopped (agentop service stop)")

	code, out = control("start")
	expect("start", code, out, 0)
	code, out = status()
	expect("status after start", code, out, 0, "Cortex is healthy according to")

	before := currentMainPID(systemdUnit)
	code, out = control("restart")
	expect("restart", code, out, 0, "restarted")
	if after := currentMainPID(systemdUnit); after == 0 || after == before {
		t.Errorf("restart left MainPID %d (was %d), want a new process", after, before)
	}

	// A crash: the unit's own RestartSec=10 brings it back.
	pid := currentMainPID(systemdUnit)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill -9 %d: %v", pid, err)
	}
	if !waitUntil(3*time.Second, func() bool { return unit().sub == "auto-restart" }) {
		t.Fatalf("after kill -9 systemd says %+v, want auto-restart", unit())
	}
	code, out = status()
	expect("status while a crash restarts", code, out, 1, "systemd is restarting it")
	if _, ok := waitForNewMainPID(t, systemdUnit, pid, 20*time.Second); !ok {
		t.Fatalf("no restart within 20s of kill -9; systemd says %+v", unit())
	}
	if !waitHealthy(p.healthURL, 10*time.Second) {
		t.Fatal("restarted unit never answered /healthz")
	}
	code, out = status()
	expect("status after crash recovery", code, out, 0, "Cortex is healthy according to")

	// A crash loop until systemd gives up: a binary that exits at once. Stopped
	// first, so the running stand-in is not the file being replaced.
	if code, out := control("stop"); code != 0 {
		t.Fatalf("stop before crash loop: exit %d:\n%s", code, out)
	}
	good, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	_, _ = control("start") // fails: nothing answers
	if !waitUntil(90*time.Second, func() bool { return unit().active == "failed" }) {
		t.Fatalf("the crash loop never hit the start limit; systemd says %+v", unit())
	}
	// Not "(start-limit-hit)": which reason systemd records here varies by version.
	// 261 records the limit; 257 keeps the last exit, "exit-code". What status must
	// say either way is that systemd gave up, and what to run once it is fixed.
	code, out = status()
	expect("status after the start limit", code, out, 1, "systemd stopped restarting it (", "agentop service restart")

	// Fixed, then the restart status just told the user to run. Before
	// clearStartLimit this failed with "start of the service was attempted too
	// often" for the rest of systemd's five-minute window.
	if err := os.WriteFile(bin, good, 0o700); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	code, out = control("restart")
	expect("restart after the start limit", code, out, 0)
	code, out = status()
	expect("status after recovering", code, out, 0, "Cortex is healthy according to")

	code, out = run("uninstall", func(o, e io.Writer) int { return serviceUninstall(p, true, o, e) })
	expect("uninstall", code, out, 0, "Removed.")
	code, out = status()
	expect("status after uninstall", code, out, 0, "not installed")
	if unitIsActive(systemdUnit) {
		t.Error("unit still active after uninstall")
	}
}
