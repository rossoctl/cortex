package main

import (
	"bytes"
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

// launchdSkip is t.Skipf, or t.Fatalf under AGENTOP_LAUNCHD_TESTS=required: the
// macOS CI job sets it, so a test that cannot reach launchd fails there instead of
// passing having asserted nothing. Silent skipping is how the bootout race (#880)
// shipped unexercised.
func launchdSkip(t *testing.T) func(format string, args ...any) {
	t.Helper()
	if os.Getenv("AGENTOP_LAUNCHD_TESTS") == "required" {
		return t.Fatalf
	}
	return t.Skipf
}

// requireRealLaunchd skips (or, with AGENTOP_LAUNCHD_TESTS=required, fails) unless
// this process can load a per-user launch agent: macOS, launchctl on PATH, and a
// desktop session (gui/<uid>) to load it into. Only a login at the screen or
// automatic login creates that session. An SSH session has none, which is why these
// tests skip over SSH to a Mac nobody is logged in to. GitHub's macOS runners log
// their user in automatically, so the session is there in CI.
func requireRealLaunchd(t *testing.T) {
	t.Helper()
	skip := launchdSkip(t)
	if runtime.GOOS != "darwin" {
		skip("launchd only (GOOS=%s)", runtime.GOOS)
		return
	}
	if ok, why := launchdUsable(); !ok {
		skip("cannot load a launch agent here (no launchctl, or no desktop session gui/%d): %s", os.Getuid(), why)
	}
}

// realCortexBinary is the cortex the launchd lifecycle test installs. Unlike the
// systemd test's python stand-in it has to be the real binary: on macOS the job
// launchd runs is cortex's own supervisor (`cortex --supervise`), and the
// supervisor is what the test checks.
//
// AGENTOP_TEST_CORTEX names a prebuilt binary, for a machine with no Go toolchain,
// such as a VM this test binary was copied to. Otherwise it is built from ../cortex
// with the laptop's `local` profile, as `make cortex PROFILE=local` does.
func realCortexBinary(t *testing.T, dir string) string {
	t.Helper()
	if bin := os.Getenv("AGENTOP_TEST_CORTEX"); bin != "" {
		if !isExecutable(bin) {
			t.Fatalf("AGENTOP_TEST_CORTEX=%s is not an executable file", bin)
		}
		return bin
	}
	if _, err := exec.LookPath("go"); err != nil {
		launchdSkip(t)("no go toolchain to build cortex with, and AGENTOP_TEST_CORTEX is unset: %v", err)
	}
	env := append(os.Environ(), "GOWORK=off")
	tagsCmd := exec.Command("go", "-C", filepath.Join("..", "..", "scripts", "profile-tags"), "run", ".", "local")
	tagsCmd.Env = env
	tags, err := tagsCmd.Output()
	if err != nil {
		t.Fatalf("resolving the local profile's build tags: %v", err)
	}
	bin := filepath.Join(dir, proxyBinary)
	build := exec.Command("go", "build", "-tags", strings.TrimSpace(string(tags)), "-o", bin, ".")
	build.Dir = filepath.Join("..", "cortex")
	build.Env = env
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ../cortex: %v\n%s", err, out)
	}
	return bin
}

// launchdJobPID is the pid launchd reports for target, or 0 while it has none.
func launchdJobPID(target string) int {
	out, err := exec.Command("launchctl", "print", target).Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "pid = "); ok {
			if pid, err := strconv.Atoi(v); err == nil && pid > 1 {
				return pid
			}
		}
	}
	return 0
}

// childPID is parent's child process, or 0 while it has none. The supervisor
// runs exactly one, the proxy.
func childPID(parent int) int {
	out, _ := exec.Command("pgrep", "-P", strconv.Itoa(parent)).Output()
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil && pid > 1 {
			return pid
		}
	}
	return 0
}

// launchdDisabled reports whether target's label is in the per-user disabled
// database, which is what makes `service stop` survive the next login.
func launchdDisabled(target string) bool {
	domain, label, _ := strings.Cut(strings.TrimPrefix(target, "gui/"), "/")
	out, _ := exec.Command("launchctl", "print-disabled", "gui/"+domain).Output()
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, `"`+label+`" =>`) {
			return strings.HasSuffix(line, "disabled") || strings.HasSuffix(line, "true")
		}
	}
	return false
}

// portFree reports whether nothing is listening on addr.
func portFree(addr string) bool {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// TestServiceLifecycle_RealLaunchd is TestServiceLifecycle_RealSystemd's macOS
// counterpart: it drives `agentop service` install, status, stop, start, restart and
// uninstall against a live launchd, with the real cortex binary, and checks what
// each command reports against what launchd says the job is doing.
//
// Its central check is that a killed proxy comes back under the SAME supervisor.
// launchd's KeepAlive cannot be relied on for that: on some Macs in daily use launchd
// logs "pending spawn, domain in on-demand-only mode" and restarts nothing for hours
// (cmd/cortex/supervise.go). On a fresh machine or a CI runner it does restart jobs,
// though, so "Cortex came back" alone would pass with the supervisor broken.
//
// Like the systemd test, it installs the real label every agentop command addresses,
// so it refuses to run where one is already installed: that would be someone's
// Cortex, and this test stops and uninstalls what it finds.
func TestServiceLifecycle_RealLaunchd(t *testing.T) {
	requireRealLaunchd(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	realPlist := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
	if _, err := os.Stat(realPlist); err == nil {
		t.Skipf("%s exists: this account has a Cortex installed, which this test would replace", realPlist)
	}
	target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + launchdLabel
	if exec.Command("launchctl", "print", target).Run() == nil {
		t.Skipf("launchd already has %s loaded; not replacing it", target)
	}

	dir := t.TempDir()
	bin := realCortexBinary(t, dir)
	health, forward := freeLoopbackAddr(t), freeLoopbackAddr(t)
	// Every listener on a spare loopback port, since the defaults are fixed ports
	// another Cortex may hold, and no archive. With the job's HOME a temp dir below,
	// the run writes nothing to the real one.
	cfg := filepath.Join(dir, "config.yaml")
	body := "mode: proxy-sidecar\nlistener:\n  roles: [forward]\n  bind_loopback_only: true\n" +
		"  forward_proxy_addr: " + forward + "\n" +
		"  health_addr: " + health + "\n" +
		"  session_api_addr: " + freeLoopbackAddr(t) + "\n" +
		"  transparent_proxy_addr: " + freeLoopbackAddr(t) + "\n" +
		"stats:\n  address: " + freeLoopbackAddr(t) + "\n" +
		"session:\n  archive:\n    enabled: false\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := resolveServicePaths(cfg, "", bin)
	if err != nil {
		t.Fatal(err)
	}
	if p.unitFile != realPlist {
		t.Fatalf("unit file = %s, want %s", p.unitFile, realPlist)
	}
	p.home = filepath.Join(dir, "home")
	if err := os.Mkdir(p.home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("launchctl", "bootout", target).Run()
		waitBootedOut(target, 20*time.Second)
		_ = exec.Command("launchctl", "enable", target).Run()
		_ = os.Remove(realPlist)
		_ = exec.Command("pkill", "-f", bin).Run()
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

	code, out := status()
	expect("status before install", code, out, 0, "not installed")

	code, out = run("install", func(o, e io.Writer) int { return serviceInstall(p, true, false, o, e) })
	expect("install", code, out, 0, "healthy")
	var sup, proxy int
	if !waitUntil(10*time.Second, func() bool {
		sup = launchdJobPID(target)
		proxy = childPID(sup)
		return sup > 0 && proxy > 0
	}) {
		t.Fatalf("after install launchd reports supervisor pid %d with child %d, want both running", sup, proxy)
	}
	code, out = status()
	expect("status running", code, out, 0, "Cortex is healthy according to")

	// The supervisor's restart. A proxy crash must not reach launchd at all: the job's
	// pid is the supervisor's, and it stays the same while a new proxy takes over.
	// syscall.Kill gets a pid from childPID, which never returns one below 2: kill(2)
	// reads 0 as this process's own group, which holds `go test` too.
	if err := syscall.Kill(proxy, syscall.SIGKILL); err != nil {
		t.Fatalf("kill -9 proxy %d: %v", proxy, err)
	}
	// The wait follows whatever supervisor launchd reports now, so that a broken
	// supervisor which launchd replaced is reported as that, not as a timeout.
	var nowSup, newProxy int
	if !waitUntil(20*time.Second, func() bool {
		nowSup = launchdJobPID(target)
		newProxy = childPID(nowSup)
		return newProxy > 0 && newProxy != proxy && waitHealthy(p.healthURL, 200*time.Millisecond)
	}) {
		t.Fatalf("Cortex was not serving again within 20s of kill -9 on proxy %d (supervisor now %d, its child %d)", proxy, nowSup, newProxy)
	}
	if nowSup != sup {
		t.Errorf("after the proxy died launchd reports supervisor %d, was %d: the restart was launchd's, not the supervisor's", nowSup, sup)
	}
	code, out = status()
	expect("status after the supervisor restarted the proxy", code, out, 0, "Cortex is healthy according to")

	// stop: bootout, plus disable so the next login does not start it again, and no
	// process left holding the ports for the next start to lose to.
	code, out = control("stop")
	expect("stop", code, out, 0, "stopped, and it will stay stopped across logins")
	if exec.Command("launchctl", "print", target).Run() == nil {
		t.Error("after stop launchd still has the job loaded")
	}
	if !launchdDisabled(target) {
		t.Error("after stop the label is not disabled, so the next login would start Cortex again")
	}
	for _, pid := range []int{sup, newProxy} {
		if syscall.Kill(pid, 0) == nil {
			t.Errorf("after stop process %d is still running", pid)
		}
	}
	for _, addr := range []string{forward, health} {
		if !portFree(addr) {
			t.Errorf("after stop something still holds %s", addr)
		}
	}
	// Not the wording: on macOS a deliberate stop and a crash still read alike
	// ("NOT answering"), where Linux tells them apart (#1529). Only that status does
	// not call a stopped Cortex healthy, and does not exit 0.
	code, out = status()
	if code == 0 || strings.Contains(out, "is healthy") {
		t.Errorf("status after stop: exit %d, want nonzero and no claim of health:\n%s", code, out)
	}

	code, out = control("start")
	expect("start", code, out, 0)
	if launchdDisabled(target) {
		t.Error("after start the label is still disabled, so the next login would not start Cortex")
	}
	code, out = status()
	expect("status after start", code, out, 0, "Cortex is healthy according to")

	before := launchdJobPID(target)
	code, out = control("restart")
	expect("restart", code, out, 0, "restarted")
	if after := launchdJobPID(target); after == 0 || after == before {
		t.Errorf("restart left supervisor pid %d (was %d), want a new process", after, before)
	}
	code, out = status()
	expect("status after restart", code, out, 0, "Cortex is healthy according to")

	code, out = run("uninstall", func(o, e io.Writer) int { return serviceUninstall(p, true, o, e) })
	expect("uninstall", code, out, 0, "Removed.")
	if _, err := os.Stat(realPlist); err == nil {
		t.Errorf("uninstall left %s", realPlist)
	}
	if exec.Command("launchctl", "print", target).Run() == nil {
		t.Error("after uninstall launchd still has the job loaded")
	}
	code, out = status()
	expect("status after uninstall", code, out, 0, "not installed")
}
