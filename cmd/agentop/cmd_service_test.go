package main

import (
	"bytes"
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func servicePathsFixture(t *testing.T) servicePaths {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "cortex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return servicePaths{
		unitFile:   filepath.Join(dir, "unit"),
		binary:     bin,
		configFile: filepath.Join(dir, "config.yaml"),
		logFile:    filepath.Join(dir, "proxy.log"),
		pidFile:    filepath.Join(dir, "proxy.pid"),
		stampFile:  filepath.Join(dir, "proxy.sha256"),
		healthURL:  "http://127.0.0.1:1/healthz",
		home:       dir,
	}
}

// TestUnloadService_Darwin: a label that is already gone is the state an unload wants,
// so it is not an error. `agentop service stop` boots the label out, and the uninstall
// that followed printed launchctl's "No such process" over a removal that had worked
// (#1255). Passing "darwin" makes this run on any host: only launchctl is stubbed.
func TestUnloadService_Darwin(t *testing.T) {
	t.Run("already booted out: nil", func(t *testing.T) {
		p := servicePathsFixture(t)
		fakeLaunchctl(t, "#!/bin/sh\necho 'Boot-out failed: 3: No such process' >&2\nexit 3\n")
		if err := unloadService("darwin", p); err != nil {
			t.Errorf("err = %v, want nil — nothing loaded is what an unload is for", err)
		}
	})

	t.Run("any other bootout failure is reported", func(t *testing.T) {
		p := servicePathsFixture(t)
		fakeLaunchctl(t, "#!/bin/sh\necho 'Boot-out failed: 1: Operation not permitted' >&2\nexit 1\n")
		err := unloadService("darwin", p)
		if err == nil || !strings.Contains(err.Error(), "launchctl bootout") ||
			!strings.Contains(err.Error(), "Operation not permitted") {
			t.Errorf("err = %v, want it to name launchctl bootout and the underlying reason", err)
		}
	})
}

// TestRenderUnit_BothPlatforms exercises the launchd and systemd renderings from
// either host. Keying off runtime.GOOS meant the systemd unit was written on a Mac
// and never checked until a Linux user hit it.
func TestRenderUnit_BothPlatforms(t *testing.T) {
	p := servicePathsFixture(t)

	t.Run("darwin restarts after a crash", func(t *testing.T) {
		// Unconditional KeepAlive on purpose: it is the simpler guarantee for the
		// requirement that matters — come back after a crash — and costs nothing,
		// because stopping deliberately is what `service uninstall` is for.
		// {SuccessfulExit:false} is documented in terms of exit STATUS, which leaves
		// its behaviour on death by signal ambiguous.
		u := renderUnitFor("darwin", p)
		if !strings.Contains(u, "<key>KeepAlive</key><true/>") {
			t.Error("KeepAlive is not unconditional; a kill -9 would not be restarted")
		}
		if strings.Contains(u, "SuccessfulExit") {
			t.Error("KeepAlive is conditioned on exit status, which signal death does not satisfy")
		}
		if !strings.Contains(u, "ThrottleInterval") {
			t.Error("no ThrottleInterval; a bad config would respawn tightly")
		}
		if !strings.Contains(u, "<key>HOME</key>") || !strings.Contains(u, p.home) {
			t.Error("HOME not set; the config's ${HOME} would not expand")
		}
		if !strings.Contains(u, "<key>RunAtLoad</key><true/>") {
			t.Error("does not start at login")
		}
		// ExitTimeOut pinned, not left to launchd's system-defined default (5s measured):
		// the supervisor's own kill (superviseStopGrace, 3s, cmd/cortex/supervise.go) has to
		// land first, or launchd SIGKILLs the supervisor mid-wait and orphans the proxy on
		// its ports. See the rationale comment in renderUnitFor's darwin branch.
		if !strings.Contains(u, "<key>ExitTimeOut</key><integer>5</integer>") {
			t.Error("ExitTimeOut is missing or not 5; a stop relies on launchd's unstated default")
		}
		// Background would throttle every request an agent sends through the proxy.
		if strings.Contains(u, "ProcessType") {
			t.Error("ProcessType is set; Background confines the proxy to the efficiency cores")
		}
	})

	t.Run("linux restarts on failure only", func(t *testing.T) {
		u := renderUnitFor("linux", p)
		if !strings.Contains(u, "Restart=on-failure") {
			t.Errorf("not on-failure:\n%s", u)
		}
		if strings.Contains(u, "Restart=always") {
			t.Error("Restart=always; a stop could never stick")
		}
		for _, want := range []string{
			"[Unit]", "[Service]", "[Install]",
			"ExecStart=", "RestartSec=", "StartLimitBurst=",
			"WantedBy=default.target",
			"append:" + p.logFile, // restarts must not truncate the evidence
		} {
			if !strings.Contains(u, want) {
				t.Errorf("unit missing %q:\n%s", want, u)
			}
		}
		// TimeoutStopSec is the macOS ExitTimeOut's counterpart, explicitly — not
		// whatever systemd's own default (90s) happens to be. A local install's proxy
		// closes rather than drains, so it stops in well under a second; this bounds the
		// one that hangs. See the rationale comment above renderUnitFor's linux branch.
		if !strings.Contains(u, "TimeoutStopSec=5\n") {
			t.Error("TimeoutStopSec is missing or not 5; a stuck proxy would hold a " +
				"restart for systemd's 90s default")
		}
		// StartLimit* must sit in [Unit]. systemd moved them there in v229 and
		// deprecated them in [Service], where they can be ignored outright —
		// silently voiding the crash-loop throttle.
		unitSec := u[strings.Index(u, "[Unit]"):strings.Index(u, "[Service]")]
		for _, want := range []string{"StartLimitIntervalSec=", "StartLimitBurst="} {
			if !strings.Contains(unitSec, want) {
				t.Errorf("%s is not in the [Unit] section:\n%s", want, u)
			}
		}
		// A user manager has no network-online.target; ordering against it would be
		// a dependency that never resolves, and every listener here is loopback.
		if strings.Contains(u, "network-online.target") {
			t.Error("user unit orders against network-online.target")
		}
		// One ExecStart, and it names absolute paths — a unit has no shell PATH.
		if n := strings.Count(u, "ExecStart="); n != 1 {
			t.Errorf("ExecStart appears %d times, want 1", n)
		}
		// Quoted: systemd splits ExecStart on whitespace, so a $HOME with a space in
		// it would otherwise become two wrong arguments.
		if !strings.Contains(u, "'"+p.binary+"' --config '"+p.configFile+"'") {
			t.Errorf("ExecStart does not invoke quoted absolute paths:\n%s", u)
		}
	})

	t.Run("both name the same log file", func(t *testing.T) {
		for _, goos := range []string{"darwin", "linux"} {
			if !strings.Contains(renderUnitFor(goos, p), p.logFile) {
				t.Errorf("%s unit does not log to %s", goos, p.logFile)
			}
		}
	})

	t.Run("systemd unit has no unescaped newline hazards", func(t *testing.T) {
		// A stray blank key or a value spanning lines makes systemd reject the unit
		// at daemon-reload, which surfaces only as a non-zero exit.
		for _, line := range strings.Split(renderUnitFor("linux", p), "\n") {
			l := strings.TrimSpace(line)
			if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "[") {
				continue
			}
			if !strings.Contains(l, "=") {
				t.Errorf("unit line is neither section, comment nor key=value: %q", l)
			}
		}
	})
}

// TestRenderedPlistIsValid runs plutil, so a malformed plist cannot ship: launchd
// would reject it at bootstrap and the user would see a bare exit status.
func TestRenderedPlistIsValid(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("plutil is macOS-only")
	}
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("plutil not available")
	}
	p := servicePathsFixture(t)
	path := filepath.Join(t.TempDir(), "t.plist")
	if err := os.WriteFile(path, []byte(renderUnit(p)), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Errorf("plutil rejected the plist: %v\n%s\n%s", err, out, renderUnit(p))
	}
}

// TestRunningPID_OnlyClaimsOurOwnProcess: the pidfile can name a recycled pid, and
// install stops whatever it reports. Stopping a stranger's process would be the
// worst possible bug in this command.
func TestRunningPID_OnlyClaimsOurOwnProcess(t *testing.T) {
	dir := t.TempDir()
	pf := filepath.Join(dir, "proxy.pid")

	// No file.
	if got := runningPID(pf); got != 0 {
		t.Errorf("absent pidfile -> %d, want 0", got)
	}
	// Garbage.
	_ = os.WriteFile(pf, []byte("not-a-pid\n"), 0o600)
	if got := runningPID(pf); got != 0 {
		t.Errorf("garbage pidfile -> %d, want 0", got)
	}
	// A live pid that is NOT cortex: this process.
	_ = os.WriteFile(pf, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
	if got := runningPID(pf); got != 0 {
		t.Errorf("pid of a foreign process -> %d, want 0 (it is not ours)", got)
	}
	// A pid that is not running at all.
	_ = os.WriteFile(pf, []byte("999999\n"), 0o600)
	if got := runningPID(pf); got != 0 {
		t.Errorf("dead pid -> %d, want 0", got)
	}
}

const (
	sleeperEnv      = "AGENTOP_TEST_SLEEPER"
	sleeperReadyEnv = "AGENTOP_TEST_SLEEPER_READY"
)

func init() {
	if os.Getenv(sleeperEnv) == "1" {
		if ready := os.Getenv(sleeperReadyEnv); ready != "" {
			_ = os.WriteFile(ready, nil, 0o600)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

// startSleeperAt runs a copy of this test binary from path, so the process's
// executable is path itself — a shell script's would be its interpreter. It
// returns once the copy is running Go code: on macOS, deleting the binary any
// sooner (while dyld still reads it) kills the process, and a test that deletes it
// would then be asserting about a zombie.
func startSleeperAt(t *testing.T, path string) int {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Skip("cannot locate the test binary")
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o755); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(path)
	cmd.Env = append(os.Environ(), sleeperEnv+"=1", sleeperReadyEnv+"="+ready)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start the fixture process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fixture process at %s never started", path)
		}
	}
	return cmd.Process.Pid
}

// TestServiceStatus_ReportsTheV070ProxyInThePidfile: v0.7.0's installer ran
// <bin dir>/authbridge-proxy without a supervisor and recorded it in this pidfile.
func TestServiceStatus_ReportsTheV070ProxyInThePidfile(t *testing.T) {
	p := servicePathsFixture(t)
	pid := startSleeperAt(t, filepath.Join(filepath.Dir(p.binary), "authbridge-proxy"))
	_ = os.WriteFile(p.pidFile, []byte(strconv.Itoa(pid)), 0o600)

	var out bytes.Buffer
	_ = serviceStatus(p, &out)
	if want := "is running (pid " + strconv.Itoa(pid) + ")"; !strings.Contains(out.String(), want) {
		t.Errorf("status did not report the v0.7.0 proxy %q:\n%s", want, out.String())
	}
}

func TestAdoptablePID_PreRenameProxyOnlyBesideOurs(t *testing.T) {
	p := servicePathsFixture(t)
	write := func(pid int) { _ = os.WriteFile(p.pidFile, []byte(strconv.Itoa(pid)), 0o600) }

	ours := startSleeperAt(t, filepath.Join(filepath.Dir(p.binary), "authbridge-proxy"))
	write(ours)
	if got := adoptablePID(p); got != ours {
		t.Errorf("authbridge-proxy beside cortex -> %d, want %d", got, ours)
	}

	elsewhere := t.TempDir()
	stranger := startSleeperAt(t, filepath.Join(elsewhere, "authbridge-proxy"))
	write(stranger)
	if got := adoptablePID(p); got != 0 {
		t.Errorf("authbridge-proxy from another directory -> %d, want 0", got)
	}

	link := filepath.Join(t.TempDir(), "bin")
	if err := os.Symlink(filepath.Dir(p.binary), link); err != nil {
		t.Fatal(err)
	}
	viaLink := p
	viaLink.binary = filepath.Join(link, "cortex")
	write(ours)
	if got := adoptablePID(viaLink); got != ours {
		t.Errorf("bin dir reached through a symlink -> %d, want %d", got, ours)
	}

	write(os.Getpid())
	if got := adoptablePID(p); got != 0 {
		t.Errorf("a live process that is neither -> %d, want 0", got)
	}
	write(999999)
	if got := adoptablePID(p); got != 0 {
		t.Errorf("dead pid -> %d, want 0", got)
	}
}

// TestAdoptablePID_MissingBinariesFromAnotherDir covers samePath's fallback for a
// missing file in the direction that matters most: adopting means stopping, so a
// stranger must never read as ours just because both binaries are gone.
func TestAdoptablePID_MissingBinariesFromAnotherDir(t *testing.T) {
	// A stranger whose executable cannot be named is refused for that reason alone,
	// which would pass these tests without reaching samePath at all.
	named := func(t *testing.T, pid int) string {
		t.Helper()
		exe := setupPIDExePath(pid)
		if exe == "" {
			t.Fatalf("fixture: pid %d's executable cannot be named after its binary was removed", pid)
		}
		return exe
	}

	t.Run("a stranger from another directory", func(t *testing.T) {
		p := servicePathsFixture(t) // holds no authbridge-proxy: ours is missing too
		other := t.TempDir()
		stranger := startSleeperAt(t, filepath.Join(other, "authbridge-proxy"))
		if err := os.Remove(filepath.Join(other, "authbridge-proxy")); err != nil {
			t.Fatal(err)
		}
		_ = named(t, stranger)
		if err := os.WriteFile(p.pidFile, []byte(strconv.Itoa(stranger)), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := adoptablePID(p); got != 0 {
			t.Errorf("a stranger from %s, both binaries missing -> %d, want 0", other, got)
		}
	})

	// macOS ps reports the exec path as typed. <ours>/link/../authbridge-proxy, with
	// link -> other/sub, really runs other/authbridge-proxy: the link is followed
	// before the "..", so cleaning the ".." away first would land in our directory.
	t.Run("a stranger spelled through a link and ..", func(t *testing.T) {
		p := servicePathsFixture(t)
		ours := filepath.Dir(p.binary)
		other := t.TempDir()
		if err := os.Mkdir(filepath.Join(other, "sub"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(other, "sub"), filepath.Join(ours, "link")); err != nil {
			t.Fatal(err)
		}
		stranger := startSleeperAt(t, filepath.Join(ours, "link")+"/../authbridge-proxy")
		if err := os.Remove(filepath.Join(other, "authbridge-proxy")); err != nil {
			t.Fatalf("fixture: the binary is not at the link's parent: %v", err)
		}
		// The path shape this subtest was named for — spelled through a link
		// and ".." — reached adoptablePID as ps comm reported it, which kept
		// the "../" on macOS. setupPIDExePath (the unified probe, #1565)
		// resolves through lsof first, so the exe arrives resolved and the
		// dotdot rule itself is pinned by TestSamePathFollowsLinksBeforeDotDot.
		// What this subtest still proves: a stranger from another directory is
		// refused however its executable is spelled.
		_ = named(t, stranger)
		if err := os.WriteFile(p.pidFile, []byte(strconv.Itoa(stranger)), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := adoptablePID(p); got != 0 {
			t.Errorf("a stranger from %s, spelled through %s -> %d, want 0", other, ours, got)
		}
	})
}

// TestSamePathFollowsLinksBeforeDotDot pins the same rule without a process, so it
// holds where /proc already reports a resolved path (Linux).
func TestSamePathFollowsLinksBeforeDotDot(t *testing.T) {
	ours := t.TempDir()
	other := t.TempDir()
	if err := os.Mkdir(filepath.Join(other, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "sub"), filepath.Join(ours, "link")); err != nil {
		t.Fatal(err)
	}
	spelled := filepath.Join(ours, "link") + "/../authbridge-proxy"
	if samePath(spelled, filepath.Join(ours, "authbridge-proxy")) {
		t.Errorf("%s matched our missing binary; it names %s", spelled, filepath.Join(other, "authbridge-proxy"))
	}
	if !samePath(spelled, filepath.Join(other, "authbridge-proxy")) {
		t.Errorf("%s did not match %s, the file it names", spelled, filepath.Join(other, "authbridge-proxy"))
	}
}

// TestDialableAddr covers the bind-vs-dial distinction the health probe depends
// on: ":9091" is not something a client can connect to.
func TestDialableAddr(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"127.0.0.1:47604", "127.0.0.1:47604"},
		{":47604", "localhost:47604"},
		{"0.0.0.0:47604", "localhost:47604"},
		{"[::]:47604", "localhost:47604"},
		{"[::1]:47604", "[::1]:47604"},
	} {
		if got := dialableAddr(tc.in); got != tc.want {
			t.Errorf("dialableAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestServiceStatus_NamesTheUnsupervisedCase is the diagnostic that matters most:
// a hand-started proxy works until the next reboot, and nothing says so.
func TestServiceStatus_NamesTheUnsupervisedCase(t *testing.T) {
	p := servicePathsFixture(t)
	var out bytes.Buffer
	if code := serviceStatus(p, &out); code != 0 {
		t.Errorf("exit = %d, want 0 when not installed", code)
	}
	if !strings.Contains(out.String(), "not installed") {
		t.Errorf("status did not say it is uninstalled: %q", out.String())
	}

	// With a live proxy of ours in the pidfile it should say nothing restarts it.
	cmd := exec.Command(p.binary) // the fixture's sleep script
	if err := cmd.Start(); err != nil {
		t.Skip("cannot start the fixture process")
	}
	defer func() { _ = cmd.Process.Kill() }()
	_ = os.WriteFile(p.pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	// The fixture is not named cortex, so runningPID rejects it — which is
	// itself the property TestRunningPID covers. Assert the uninstalled branch only.
	out.Reset()
	_ = serviceStatus(p, &out)
	if !strings.Contains(out.String(), "not installed") {
		t.Errorf("unexpected status output: %q", out.String())
	}
}

// TestServiceUsage_NamesTheAgentsThatDependOnTheProxy: once configured, OpenCode depends on
// the proxy being up as Claude Code does, and the usage says so beside it.
func TestServiceUsage_NamesTheAgentsThatDependOnTheProxy(t *testing.T) {
	prose := strings.Join(strings.Fields(serviceUsage), " ")
	want := `Claude Code and OpenCode depend on the proxy being up once "agentop configure claude-code enable" ` +
		`or "agentop configure opencode enable" has run, and nothing else keeps it up.`
	if !strings.Contains(prose, want) {
		t.Errorf("serviceUsage lacks %q", want)
	}
}

// TestServiceStatus_NotAnsweringNamesTheAgentsThatFail: an installed proxy that does not
// answer its health check fails every agent configured to use it, OpenCode as well as
// Claude Code.
func TestServiceStatus_NotAnsweringNamesTheAgentsThatFail(t *testing.T) {
	p := servicePathsFixture(t)
	if err := os.WriteFile(p.unitFile, []byte("unit"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := serviceStatus(p, &out); code != 1 {
		t.Errorf("exit = %d, want 1 for a proxy that is not answering", code)
	}
	want := "Cortex is NOT answering " + p.healthURL + "\n" +
		"  Claude Code and OpenCode will fail while this is true. Last log lines:\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("status =\n%s\nwant it to contain\n%s", out.String(), want)
	}
}

// TestWaitHealthy_TimesOut: install claims success only after the health endpoint
// answers, because a supervisor reports "loaded" for a proxy that is crash-looping.
func TestWaitHealthy_TimesOut(t *testing.T) {
	start := time.Now()
	if waitHealthy("http://127.0.0.1:1/healthz", 1200*time.Millisecond) {
		t.Error("reported healthy against a closed port")
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("returned after %s; it should have used the timeout", elapsed)
	}
}

// TestLastLines_SurfacesTheReason: "installed but not answering" is useless
// without the log tail that says why.
func TestLastLines_SurfacesTheReason(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "proxy.log")
	_ = os.WriteFile(p, []byte("one\ntwo\nthree\nfour\n"), 0o600)
	got := lastLines(p, 2)
	if len(got) != 2 || got[0] != "three" || got[1] != "four" {
		t.Errorf("lastLines = %v, want [three four]", got)
	}
	if msg := lastLines(filepath.Join(dir, "nope.log"), 3); len(msg) != 1 || !strings.Contains(msg[0], "no log") {
		t.Errorf("missing log should say so, got %v", msg)
	}
}

// TestRenderUnit_HostilePaths covers what a real $HOME can contain. Both renderings
// interpolate user-supplied paths: an unescaped "&" makes the plist malformed XML
// that launchctl bootstrap rejects, and an unquoted space splits systemd's ExecStart
// into the wrong arguments.
func TestRenderUnit_HostilePaths(t *testing.T) {
	p := servicePaths{
		binary:     "/Users/a & b/.local/bin/cortex",
		configFile: "/Users/a & b/.cortex/my config.yaml",
		logFile:    "/Users/a & b/.cortex/proxy.log",
		home:       "/Users/a & b",
	}

	t.Run("plist stays well-formed XML", func(t *testing.T) {
		u := renderUnitFor("darwin", p)
		if strings.Contains(u, "a & b") {
			t.Error("raw & left in the plist; launchctl bootstrap would reject it")
		}
		if !strings.Contains(u, "a &amp; b") {
			t.Errorf("& not escaped:\n%s", u)
		}
		var v any
		if err := xml.Unmarshal([]byte(u), &v); err != nil {
			t.Errorf("rendered plist is not well-formed XML: %v", err)
		}
	})

	t.Run("systemd ExecStart keeps the path as one argument", func(t *testing.T) {
		u := renderUnitFor("linux", p)
		var execLine string
		for _, l := range strings.Split(u, "\n") {
			if strings.HasPrefix(l, "ExecStart=") {
				execLine = l
			}
		}
		if execLine == "" {
			t.Fatal("no ExecStart line")
		}
		// Both paths must be quoted, or the spaces split them.
		if !strings.Contains(execLine, "'"+p.binary+"'") {
			t.Errorf("binary not quoted: %s", execLine)
		}
		if !strings.Contains(execLine, "'"+p.configFile+"'") {
			t.Errorf("config path not quoted: %s", execLine)
		}
	})

	t.Run("a single quote in the path cannot break out", func(t *testing.T) {
		q := servicePaths{
			binary:     "/home/o'brien/bin/cortex",
			configFile: "/home/o'brien/.cortex/config.yaml",
			logFile:    "/home/o'brien/.cortex/proxy.log",
			home:       "/home/o'brien",
		}
		u := renderUnitFor("linux", q)
		if strings.Contains(u, "ExecStart='/home/o'brien") {
			t.Errorf("quote not neutralised, ExecStart is breakable:\n%s", u)
		}
	})
}

// TestSupervisionIsPlatformCorrect: launchd does not restart these agents, so the
// plist must run the proxy under --supervise; systemd does, and nesting a supervisor
// there would hide crashes from its StartLimit accounting.
func TestSupervisionIsPlatformCorrect(t *testing.T) {
	p := servicePaths{
		binary: "/u/bin/cortex", configFile: "/u/.cortex/config.yaml",
		logFile: "/u/.cortex/proxy.log", home: "/u",
	}
	darwin := renderUnitFor("darwin", p)
	if !strings.Contains(darwin, "<string>--supervise</string>") {
		t.Error("the plist does not supervise; a crash would go unrecovered on macOS")
	}
	// The supervise flag must come before --config, as a flag not a config value.
	if i, j := strings.Index(darwin, "--supervise"), strings.Index(darwin, "--config"); i < 0 || j < 0 || i > j {
		t.Errorf("--supervise is not ordered before --config (%d vs %d)", i, j)
	}
	linux := renderUnitFor("linux", p)
	if strings.Contains(linux, "--supervise") {
		t.Error("systemd unit nests a supervisor; Restart=on-failure already does this")
	}
	if !strings.Contains(linux, "Restart=on-failure") {
		t.Error("systemd unit lost its own restart policy")
	}
}

// TestBridgeCANotBefore_SilentWithoutACA: this is one advisory line in `status`, so
// every failure to determine it must be silent. A broken or bridgeless config must
// still let status report everything else.
func TestBridgeCANotBefore_SilentWithoutACA(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "nope.yaml")
	if nb, f := bridgeCANotBefore(missing); nb != "" || f != "" {
		t.Errorf("missing config: got (%q,%q), want empty", nb, f)
	}

	noBridge := filepath.Join(dir, "nobridge.yaml")
	if err := os.WriteFile(noBridge, []byte("mode: proxy-sidecar\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if nb, f := bridgeCANotBefore(noBridge); nb != "" || f != "" {
		t.Errorf("config without a bridge: got (%q,%q), want empty", nb, f)
	}

	// A configured bridge whose ca.crt is not there yet (first boot) is also silent
	// rather than reporting a cutoff of the zero time, which would read as 1 Jan
	// year 1 and make every client look stale.
	caDir := filepath.Join(dir, "ca")
	withBridge := filepath.Join(dir, "bridge.yaml")
	body := "mode: proxy-sidecar\ntls_bridge:\n  mode: enabled\n  ca_dir: " + caDir + "\n"
	if err := os.WriteFile(withBridge, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if nb, _ := bridgeCANotBefore(withBridge); nb != "" {
		t.Errorf("bridge configured but no ca.crt yet: got %q, want empty", nb)
	}
}

// TestPortOfAddr: the value is pasted into an lsof command, so a bare port or an
// unexpected shape must still produce something recognisable rather than "".
func TestPortOfAddr(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"127.0.0.1:47600", "47600"},
		{":47600", "47600"},
		{"47600", "47600"},
	} {
		if got := portOfAddr(tc.in); got != tc.want {
			t.Errorf("portOfAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// holderOnPort makes lsof name pid as the listener on 127.0.0.1:port, and exe as that
// pid's executable — strangerOn's stub, parameterised, because which executable holds
// the port is the whole of what foreignHolderLines decides on. An empty exe is a free
// port: lsof names no pid for any of the four addresses portHolder asks about.
func holderOnPort(t *testing.T, port string, pid int, exe string) {
	t.Helper()
	p := strconv.Itoa(pid)
	body := "#!/bin/sh\nexit 0\n"
	if exe != "" {
		body = `#!/bin/sh
case "$1" in
  -nP) case "$2" in -iTCP@127.0.0.1:` + port + `) printf 'p` + p + `\n' ;; esac ;;
  -p) printf 'p` + p + `\nftxt\nn` + exe + `\n' ;;
esac
exit 0
`
	}
	installStub(t, "lsof", body)
	// No /proc on a Mac and none faked here, so setupPIDExePath falls through to the
	// lsof txt entry above on both platforms rather than to the host's real process.
	fakeProc(t, pid, "")
	// And no supervisor job owns the holder, on either platform: launchctl names no
	// pid, and the faked /proc has no cgroup file. Without this the remedy line would
	// depend on what is loaded on the machine running the test.
	installStub(t, "launchctl", "#!/bin/sh\nexit 0\n")
}

// A pinned forward port is the one cause of "installed but not answering" that no
// restart fixes, and #1122's reporter had nothing in `service status` saying so — the
// proxy's own log said it, buried in a five-line tail of restart lines.
func TestForeignHolderLines(t *testing.T) {
	const port = "47600"
	const addr = "127.0.0.1:" + port

	t.Run("a stranger holding the port is named", func(t *testing.T) {
		p := servicePathsFixture(t)
		p.forwardAddr = addr
		holderOnPort(t, port, 31337, "/usr/bin/python3")

		lines := foreignHolderLines(p)
		if len(lines) == 0 {
			t.Fatal("nothing said about a port held by another process")
		}
		want := addr + " is held by /usr/bin/python3 (pid 31337), not this Cortex"
		if lines[0] != want {
			t.Errorf("lines[0] = %q, want %q", lines[0], want)
		}
		// The certificate sentence is for a Cortex holder only: a stranger on the port
		// is refusing connections, not bridging them with a CA of its own.
		for _, l := range lines {
			if strings.Contains(l, "ITS CA") {
				t.Errorf("a non-Cortex holder is blamed for the certificate error: %q", l)
			}
		}
		if last := lines[len(lines)-1]; !strings.Contains(last, "kill 31337") {
			t.Errorf("remedy = %q, want it to name the pid to stop", last)
		}
	})

	t.Run("another Cortex explains the certificate error too", func(t *testing.T) {
		p := servicePathsFixture(t)
		p.forwardAddr = addr
		// Under HOME, as a second install is: both to exercise the ~/… rendering every
		// other line of setup uses, and because whose install it is is the next question.
		other := filepath.Join(p.home, "other", "bin", "cortex")
		holderOnPort(t, port, 31337, other)

		lines := foreignHolderLines(p)
		if len(lines) < 2 {
			t.Fatalf("lines = %q, want the holder and the certificate sentence", lines)
		}
		if !strings.Contains(lines[0], "~/other/bin/cortex") {
			t.Errorf("lines[0] = %q, want the holder under HOME shown as ~/…", lines[0])
		}
		if !strings.Contains(lines[1], "ITS CA") || !strings.Contains(lines[1], "self-signed-certificate") {
			t.Errorf("lines[1] = %q; the companion symptom goes unexplained", lines[1])
		}
	})

	// Our own proxy on the port is the ordinary case — it is starting, or the pidfile
	// is stale — and calling it a conflict would send the user to stop the thing they
	// are trying to run.
	t.Run("our own proxy is not a conflict", func(t *testing.T) {
		t.Run("by executable", func(t *testing.T) {
			p := servicePathsFixture(t)
			p.forwardAddr = addr
			holderOnPort(t, port, 31337, p.binary)
			if lines := foreignHolderLines(p); lines != nil {
				t.Errorf("lines = %q for this install's own binary", lines)
			}
		})
		t.Run("by pidfile", func(t *testing.T) {
			p := servicePathsFixture(t)
			p.forwardAddr = addr
			if err := os.WriteFile(p.pidFile, []byte("31337\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			holderOnPort(t, port, 31337, "/usr/bin/python3")
			if lines := foreignHolderLines(p); lines != nil {
				t.Errorf("lines = %q for the pid in our own pidfile", lines)
			}
		})
	})

	// Silence rather than "something holds it": a line that cannot name the holder adds
	// nothing to the status, and the log tail below it is what is left to read.
	t.Run("says nothing when there is nothing to say", func(t *testing.T) {
		t.Run("the port is free", func(t *testing.T) {
			p := servicePathsFixture(t)
			p.forwardAddr = addr
			holderOnPort(t, port, 31337, "")
			if lines := foreignHolderLines(p); lines != nil {
				t.Errorf("lines = %q with nothing on the port", lines)
			}
		})
		// No forward address is the broken-config path, where the file that names the
		// port could not be read — so there is no port to ask about.
		t.Run("no forward address is known", func(t *testing.T) {
			p := servicePathsFixture(t)
			holderOnPort(t, port, 31337, "/usr/bin/python3")
			if lines := foreignHolderLines(p); lines != nil {
				t.Errorf("lines = %q with no configured forward address", lines)
			}
		})
	})
}

// Where the holder line lands matters: it is the cause, so it goes above the log tail
// and above the consequence, not after five lines of supervisor output.
func TestReportNotAnswering_LeadsWithTheHolder(t *testing.T) {
	p := servicePathsFixture(t)
	p.forwardAddr = "127.0.0.1:47600"
	holderOnPort(t, "47600", 31337, "/usr/bin/python3")

	var out bytes.Buffer
	reportNotAnswering(p, &out)

	s := out.String()
	notAnswering := strings.Index(s, "NOT answering")
	holder := strings.Index(s, "is held by /usr/bin/python3 (pid 31337)")
	willFail := strings.Index(s, "will fail while this is true")
	switch {
	case notAnswering < 0 || willFail < 0:
		t.Fatalf("the existing report is gone:\n%s", s)
	case holder < 0:
		t.Fatalf("the holder is not named at all:\n%s", s)
	case holder < notAnswering || holder > willFail:
		t.Errorf("the holder line is not between those two:\n%s", s)
	}
}
