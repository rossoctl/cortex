package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests pin the output of each `agentop service install|uninstall` path below
// as it was before install was split into a CLI half and runServiceInstall. The
// comparison is exact after norm; TestCharacterize_ServiceInstall_LoginHomeWarning pins
// the one block norm strips from stderr. They run the same scenario on darwin and
// linux: fakeSupervisor stubs both platforms' tools, and the code under test picks
// whichever runtime.GOOS calls for.

// fakeSupervisor stubs launchctl, systemctl, loginctl and lsof with one shared bit of
// state: bootstrap/restart loads the job, bootout/disable unloads it, and print /
// is-active / is-enabled report it. It returns the path of that state file, which
// exists exactly while the fake job is loaded, so newServiceSceneServing can follow
// it. launchctl disable and enable add and remove <that path>.disabled, as launchd's
// per-user disabled database records them.
//
// The four stubs go through installStub, whose reachability check is load-bearing here:
// the label is the REAL io.rossoctl.cortex, so a test that fell through to the real
// launchctl would boot out the Cortex this machine is running.
func fakeSupervisor(t *testing.T) string {
	t.Helper()
	skipSceneInShort(t)
	loaded := filepath.Join(t.TempDir(), "loaded")
	installStub(t, "launchctl", `#!/bin/sh
case "$1" in
  print)
    case "$2" in
      */*/*) if [ -f '`+loaded+`' ]; then echo 'state = running'; exit 0; fi
             echo 'Could not find service' >&2; exit 113 ;;
    esac
    exit 0 ;;
  bootstrap) : > '`+loaded+`' ;;
  bootout) if [ -f '`+loaded+`' ]; then rm -f '`+loaded+`'; exit 0; fi
           echo 'Boot-out failed: 3: No such process' >&2; exit 3 ;;
  enable) rm -f '`+loaded+`.disabled' ;;
  disable) : > '`+loaded+`.disabled' ;;
esac
exit 0
`)
	installStub(t, "systemctl", `#!/bin/sh
case "$*" in
  *is-active*) if [ -f '`+loaded+`' ]; then echo active; exit 0; fi; echo inactive; exit 3 ;;
  *is-enabled*) if [ -f '`+loaded+`' ]; then echo enabled; exit 0; fi; echo disabled; exit 1 ;;
  *restart*) : > '`+loaded+`' ;;
  *disable*) rm -f '`+loaded+`' ;;
esac
exit 0
`)
	installStub(t, "loginctl", "#!/bin/sh\necho Linger=yes\n")
	installFalse(t, "lsof")
	return loaded
}

// fakeNoSupervisor makes both platforms' supervisors unreachable, the way a
// restricted sandbox does. Both stubs print the same first line, because that line is
// the reason launchdUsable and systemdUsable report, so the refusal reads the same on
// either platform and can be pinned exactly.
func fakeNoSupervisor(t *testing.T) {
	t.Helper()
	skipSceneInShort(t)
	installStub(t, "launchctl", "#!/bin/sh\necho 'supervisor unreachable' >&2\nexit 125\n")
	installStub(t, "systemctl", "#!/bin/sh\necho 'supervisor unreachable' >&2\nexit 1\n")
}

// serviceScene is one scenario's machine: a temp HOME, a config whose health endpoint
// is a live test server, a stand-in cortex binary, and the resolved paths.
type serviceScene struct {
	home string
	p    servicePaths
}

// newServiceScene's health endpoint always answers, so something is already serving
// before the install runs.
func newServiceScene(t *testing.T) serviceScene {
	t.Helper()
	return newServiceSceneWith(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// newServiceSceneServing's health endpoint follows the fake supervisor instead: 503
// until loaded (fakeSupervisor's state file) exists, 200 after. So nothing is serving
// before a first install, and the job that install loads is what answers.
func newServiceSceneServing(t *testing.T, loaded string) serviceScene {
	t.Helper()
	return newServiceSceneWith(t, func(w http.ResponseWriter, _ *http.Request) {
		if _, err := os.Stat(loaded); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func newServiceSceneWith(t *testing.T, healthz http.HandlerFunc) serviceScene {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	health := httptest.NewServer(healthz)
	t.Cleanup(health.Close)
	cfgDir := filepath.Join(home, ".cortex")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(cfgDir, "config.yaml")
	body := "mode: proxy-sidecar\nlistener:\n  roles: [forward]\n" +
		"  forward_proxy_addr: 127.0.0.1:47600\n" +
		"  health_addr: " + strings.TrimPrefix(health.URL, "http://") + "\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "bin", "cortex")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	p, err := resolveServicePaths(cfg, filepath.Join(home, "unit"), bin)
	if err != nil {
		t.Fatal(err)
	}
	return serviceScene{home: home, p: p}
}

type svcRun struct {
	code        int
	out, errOut string
}

func (sc serviceScene) install(t *testing.T, yes, restart bool) svcRun {
	t.Helper()
	var out, errb bytes.Buffer
	code := serviceInstall(sc.p, yes, restart, &out, &errb)
	return svcRun{code, sc.norm(out.String()), sc.norm(errb.String())}
}

func (sc serviceScene) uninstall(t *testing.T, yes bool) svcRun {
	t.Helper()
	var out, errb bytes.Buffer
	code := serviceUninstall(sc.p, yes, &out, &errb)
	return svcRun{code, sc.norm(out.String()), sc.norm(errb.String())}
}

// norm makes output comparable across machines: the temp HOME becomes $HOME, the
// platform's supervisor becomes <supervisor>, and the two platform-only blocks (the
// macOS crash-recovery note and the login-home warning) are removed.
func (sc serviceScene) norm(s string) string {
	s = strings.ReplaceAll(s, loginHomeWarning(sc.home), "")
	s = strings.ReplaceAll(s, crashRecoveryNote+"\n", "")
	s = strings.ReplaceAll(s, sc.home, "$HOME")
	return strings.ReplaceAll(s, supervisorName(runtime.GOOS), "<supervisor>")
}

// loginHomeWarning is the block serviceInstall prints on macOS when $HOME is not the
// login home, which is always true under t.TempDir.
func loginHomeWarning(home string) string {
	lh := loginHome()
	if lh == "" || lh == home {
		return "\x00never-matches\x00"
	}
	return "agentop: $HOME is " + home + " but your login home is " + lh + ".\n" +
		"  The unit goes to $HOME/Library/LaunchAgents, which launchd does not scan at\n" +
		"  login, so Cortex will NOT come back after a logout. Crash recovery still\n" +
		"  works while you are logged in.\n\n"
}

func (r svcRun) check(t *testing.T, code int, out, errOut string) {
	t.Helper()
	if r.code != code {
		t.Errorf("exit = %d, want %d", r.code, code)
	}
	if r.out != out {
		t.Errorf("stdout differs\n got: %q\nwant: %q", r.out, out)
	}
	if r.errOut != errOut {
		t.Errorf("stderr differs\n got: %q\nwant: %q", r.errOut, errOut)
	}
}

func TestCharacterize_ServiceInstall_Refusals(t *testing.T) {
	t.Run("no config", func(t *testing.T) {
		fakeSupervisor(t)
		sc := newServiceScene(t)
		if err := os.Remove(sc.p.configFile); err != nil {
			t.Fatal(err)
		}
		sc.install(t, true, false).check(t, 1, "",
			"agentop: no config at $HOME/.cortex/config.yaml. Create it with:\n"+
				"  cortex --local --write-config\n")
	})
	t.Run("no supervisor", func(t *testing.T) {
		fakeNoSupervisor(t)
		sc := newServiceScene(t)
		sc.install(t, true, false).check(t, exitNoSupervisor, "",
			"agentop: this environment cannot manage <supervisor>s (supervisor unreachable).\n\n"+
				"  Cortex still runs, just not supervised — start it yourself:\n"+
				"    $HOME/bin/cortex --local\n\n"+
				"  It will not restart after a crash or come back at login while running that\n"+
				"  way. To stop it: kill that process.\n")
	})
	t.Run("no binary", func(t *testing.T) {
		fakeSupervisor(t)
		sc := newServiceScene(t)
		sc.p.binary = filepath.Join(sc.home, "missing", "cortex")
		sc.install(t, true, false).check(t, 1, "",
			"agentop: cortex not found at $HOME/missing/cortex; install it first\n")
	})
	t.Run("config will not load", func(t *testing.T) {
		fakeSupervisor(t)
		sc := newServiceScene(t)
		if err := os.WriteFile(sc.p.configFile, []byte("listener:\n  roles: [forward\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		p, err := resolveServicePaths(sc.p.configFile, sc.p.unitFile, sc.p.binary)
		if err != nil || p.configErr == nil {
			t.Fatalf("broken config not detected: %v", err)
		}
		sc.p = p
		sc.install(t, true, false).check(t, 1, "",
			"agentop: $HOME/.cortex/config.yaml will not load, so a supervised proxy could not start:\n"+
				"  "+sc.norm(p.configErr.Error())+"\n"+
				"  Fix it (or delete it and run: cortex --local --write-config), then re-run.\n")
	})
}

func TestCharacterize_ServiceInstall_Declined(t *testing.T) {
	answerPrompt(t, &serviceConfirm, "n\n")
	fakeSupervisor(t)
	sc := newServiceScene(t)
	sc.install(t, false, false).check(t, exitDeclined,
		"This will install a <supervisor> that runs:\n"+
			"  $HOME/bin/cortex --config $HOME/.cortex/config.yaml\n\n"+
			"It restarts on failure and starts at login, so Claude Code and OpenCode keep\n"+
			"working after a crash or a reboot. Unit file: $HOME/unit\n\n"+
			"Undo with: agentop service uninstall\n\n"+
			"Apply? [y/N] Not changed.\n", "")
	if _, err := os.Stat(sc.p.unitFile); err == nil {
		t.Error("a declined install wrote the unit")
	}
}

// wantFreshStdout and wantFreshStderr are captured from serviceInstall as it was before
// the split. To recapture, set both to "" and copy the values the test then prints.
const (
	wantFreshStdout = "Updated $HOME/.cortex/config.yaml (previous kept as $HOME/.cortex/config.yaml.before-agentop-migrate):\n" +
		"  + bind_loopback_only: true   (was: wildcard binds for anything unpinned)\n" +
		"  + transparent_proxy_addr: 127.0.0.1:47603   (was defaulting to :8082 — every interface)\n" +
		"Updated $HOME/.cortex/config.yaml (previous kept as $HOME/.cortex/config.yaml.before-agentop-pricing):\n" +
		"  + pricing.endpoints: api.us-east.bob.ibm.com at 2 Bobcoins per million tokens   (was: unpriced)\n" +
		"  A running proxy reloads pricing from the file; this needs no restart.\n" +
		"Running as a <supervisor>, healthy.\n" +
		"  Captured session history is cleared: the store is in memory, so any\n" +
		"  timeline you were reading in agentop starts over.\n"
	wantFreshStderr = ""
)

// wantAlreadyCurrentStdout is a re-run's output when nothing needs to change.
const wantAlreadyCurrentStdout = "Already current: cortex is running under <supervisor> and healthy.\n" +
	"  Nothing to change. Use `agentop service restart` to restart it anyway.\n"

// wantFile asserts that path exists (present) or does not (!present). what names the
// moment being checked, so a failure says which step left the wrong state.
func wantFile(t *testing.T, path string, present bool, what string) {
	t.Helper()
	_, err := os.Stat(path)
	switch {
	case present && err != nil:
		t.Errorf("%s: %s is missing: %v", what, path, err)
	case !present && err == nil:
		t.Errorf("%s: %s is still there", what, path)
	}
}

func TestCharacterize_ServiceLifecycle(t *testing.T) {
	loaded := fakeSupervisor(t)
	sc := newServiceScene(t)

	fresh := sc.install(t, true, false)
	if wantFreshStdout == "" && wantFreshStderr == "" {
		t.Fatalf("capture these into wantFreshStdout / wantFreshStderr:\nstdout: %q\nstderr: %q\nexit: %d",
			fresh.out, fresh.errOut, fresh.code)
	}
	fresh.check(t, 0, wantFreshStdout, wantFreshStderr)
	// Present now, so the "gone" checks after uninstall cannot pass vacuously.
	wantFile(t, sc.p.unitFile, true, "after install")
	wantFile(t, sc.p.stampFile, true, "after install")
	wantFile(t, loaded, true, "after install (the job is loaded)")

	sc.install(t, true, false).check(t, 0, wantAlreadyCurrentStdout, "")

	sc.uninstall(t, true).check(t, 0,
		"This will stop and remove the <supervisor> at:\n  $HOME/unit\n\n"+
			"Cortex will no longer start at login. Claude Code and OpenCode stop working\n"+
			"whenever the proxy is not running — `agentop configure claude-code disable` and\n"+
			"`agentop configure opencode disable` remove that dependency.\n\n"+
			"\nRemoved. Cortex is stopped; Claude Code and OpenCode will fail until it runs again.\n"+
			"  Set it up again with:  agentop service install\n"+
			"  Or unwire Claude Code: agentop configure claude-code disable\n"+
			"  Or unwire OpenCode:    agentop configure opencode disable\n"+
			"  The config and CA are untouched in $HOME/.cortex\n", "")
	wantFile(t, sc.p.unitFile, false, "after uninstall")
	wantFile(t, sc.p.stampFile, false, "after uninstall")
	wantFile(t, loaded, false, "after uninstall (the job must be unloaded)")

	sc.uninstall(t, true).check(t, 0, "Nothing to do: no unit at $HOME/unit\n", "")
}

func TestCharacterize_ServiceUninstall_Declined(t *testing.T) {
	loaded := fakeSupervisor(t)
	sc := newServiceScene(t)
	if r := sc.install(t, true, false); r.code != 0 {
		t.Fatalf("setup install failed: %+v", r)
	}
	answerPrompt(t, &serviceConfirm, "n\n")
	sc.uninstall(t, false).check(t, exitDeclined,
		"This will stop and remove the <supervisor> at:\n  $HOME/unit\n\n"+
			"Cortex will no longer start at login. Claude Code and OpenCode stop working\n"+
			"whenever the proxy is not running — `agentop configure claude-code disable` and\n"+
			"`agentop configure opencode disable` remove that dependency.\n\n"+
			"Apply? [y/N] Not changed.\n", "")
	wantFile(t, sc.p.unitFile, true, "after a declined uninstall")
	wantFile(t, loaded, true, "after a declined uninstall (the job must stay loaded)")
}

// Stop, then uninstall: the order someone removing Cortex reaches for. stop has already
// booted the job out, so the uninstall's own bootout finds nothing — the state it wants,
// so nothing to report. It used to print launchctl's "No such process" over a removal
// that had worked (#1255). Only darwin can show it: systemctl's disable --now succeeds
// on a unit that is already disabled, so linux passed this before the fix too.
func TestCharacterize_ServiceUninstall_AfterStop(t *testing.T) {
	loaded := fakeSupervisor(t)
	sc := newServiceScene(t)
	if r := sc.install(t, true, false); r.code != 0 {
		t.Fatalf("setup install failed: %+v", r)
	}
	if err := controlService(runtime.GOOS, "stop", sc.p, io.Discard); err != nil {
		t.Fatalf("stop: %v", err)
	}
	wantFile(t, loaded, false, "after stop (the job is booted out)")
	if r := sc.uninstall(t, true); r.code != 0 || r.errOut != "" {
		t.Errorf("uninstall after stop: exit = %d, stderr = %q; want 0 and nothing on stderr", r.code, r.errOut)
	}
	wantFile(t, sc.p.unitFile, false, "after uninstall")
}

// wantAcceptStdout and wantAcceptStderr are captured the same way as wantFreshStdout
// and wantFreshStderr above.
const (
	wantAcceptStdout = "This will install a <supervisor> that runs:\n" +
		"  $HOME/bin/cortex --config $HOME/.cortex/config.yaml\n\n" +
		"It restarts on failure and starts at login, so Claude Code and OpenCode keep\n" +
		"working after a crash or a reboot. Unit file: $HOME/unit\n\n" +
		"Undo with: agentop service uninstall\n\n" +
		"Apply? [y/N] " +
		"Updated $HOME/.cortex/config.yaml (previous kept as $HOME/.cortex/config.yaml.before-agentop-migrate):\n" +
		"  + bind_loopback_only: true   (was: wildcard binds for anything unpinned)\n" +
		"  + transparent_proxy_addr: 127.0.0.1:47603   (was defaulting to :8082 — every interface)\n" +
		"Updated $HOME/.cortex/config.yaml (previous kept as $HOME/.cortex/config.yaml.before-agentop-pricing):\n" +
		"  + pricing.endpoints: api.us-east.bob.ibm.com at 2 Bobcoins per million tokens   (was: unpriced)\n" +
		"  A running proxy reloads pricing from the file; this needs no restart.\n" +
		"Wrote $HOME/unit\n" +
		"Running as a <supervisor>, healthy.\n" +
		"  Captured session history is cleared: the store is in memory, so any\n" +
		"  timeline you were reading in agentop starts over.\n"
	wantAcceptStderr = ""
)

// Answering yes at the prompt, rather than passing --yes. For install this is the only
// path that prints "Wrote <unit>".
func TestCharacterize_ServiceInteractiveAccept(t *testing.T) {
	t.Run("install", func(t *testing.T) {
		answerPrompt(t, &serviceConfirm, "y\n")
		fakeSupervisor(t)
		sc := newServiceScene(t)
		r := sc.install(t, false, false)
		if wantAcceptStdout == "" && wantAcceptStderr == "" {
			t.Fatalf("capture these into wantAcceptStdout / wantAcceptStderr:\nstdout: %q\nstderr: %q\nexit: %d",
				r.out, r.errOut, r.code)
		}
		r.check(t, 0, wantAcceptStdout, wantAcceptStderr)
		if _, err := os.Stat(sc.p.unitFile); err != nil {
			t.Errorf("no unit after an accepted install: %v", err)
		}
	})
	t.Run("uninstall", func(t *testing.T) {
		loaded := fakeSupervisor(t)
		sc := newServiceScene(t)
		if r := sc.install(t, true, false); r.code != 0 {
			t.Fatalf("setup install failed: %+v", r)
		}
		// Present now, so the "gone" checks below cannot pass vacuously.
		wantFile(t, sc.p.stampFile, true, "after install")
		wantFile(t, loaded, true, "after install (the job is loaded)")
		answerPrompt(t, &serviceConfirm, "y\n")
		sc.uninstall(t, false).check(t, 0,
			"This will stop and remove the <supervisor> at:\n  $HOME/unit\n\n"+
				"Cortex will no longer start at login. Claude Code and OpenCode stop working\n"+
				"whenever the proxy is not running — `agentop configure claude-code disable` and\n"+
				"`agentop configure opencode disable` remove that dependency.\n\n"+
				"Apply? [y/N] "+
				"\nRemoved. Cortex is stopped; Claude Code and OpenCode will fail until it runs again.\n"+
				"  Set it up again with:  agentop service install\n"+
				"  Or unwire Claude Code: agentop configure claude-code disable\n"+
				"  Or unwire OpenCode:    agentop configure opencode disable\n"+
				"  The config and CA are untouched in $HOME/.cortex\n", "")
		wantFile(t, sc.p.unitFile, false, "after an accepted uninstall")
		wantFile(t, sc.p.stampFile, false, "after an accepted uninstall")
		wantFile(t, loaded, false, "after an accepted uninstall (the job must be unloaded)")
	})
}

// wantNothingServingStdout and wantNothingServingStderr are captured the same way as
// wantFreshStdout and wantFreshStderr above.
const (
	wantNothingServingStdout = "Updated $HOME/.cortex/config.yaml (previous kept as $HOME/.cortex/config.yaml.before-agentop-migrate):\n" +
		"  + bind_loopback_only: true   (was: wildcard binds for anything unpinned)\n" +
		"  + transparent_proxy_addr: 127.0.0.1:47603   (was defaulting to :8082 — every interface)\n" +
		"Updated $HOME/.cortex/config.yaml (previous kept as $HOME/.cortex/config.yaml.before-agentop-pricing):\n" +
		"  + pricing.endpoints: api.us-east.bob.ibm.com at 2 Bobcoins per million tokens   (was: unpriced)\n" +
		"  A running proxy reloads pricing from the file; this needs no restart.\n" +
		"Running as a <supervisor>, healthy.\n"
	wantNothingServingStderr = ""
)

// A first install with nothing answering beforehand, which is the realistic one. It
// pins the branch where nothing was serving before the install, so the
// session-history lines must not print. TestCharacterize_ServiceLifecycle's health
// endpoint answers from the start, so it pins the other branch.
func TestCharacterize_ServiceInstall_NothingServingYet(t *testing.T) {
	loaded := fakeSupervisor(t)
	sc := newServiceSceneServing(t, loaded)
	r := sc.install(t, true, false)
	if wantNothingServingStdout == "" && wantNothingServingStderr == "" {
		t.Fatalf("capture these into wantNothingServingStdout / wantNothingServingStderr:\nstdout: %q\nstderr: %q\nexit: %d",
			r.out, r.errOut, r.code)
	}
	r.check(t, 0, wantNothingServingStdout, wantNothingServingStderr)
	if _, err := os.Stat(sc.p.unitFile); err != nil {
		t.Errorf("no unit after install: %v", err)
	}
}

// wantRestartStdout and wantRestartStderr are captured the same way as wantFreshStdout
// and wantFreshStderr above.
const (
	wantRestartStdout = "Running as a <supervisor>, healthy.\n" +
		"  Captured session history is cleared: the store is in memory, so any\n" +
		"  timeline you were reading in agentop starts over.\n"
	wantRestartStderr = ""
)

// --restart on an install that is already current must restart rather than report
// "Already current".
func TestCharacterize_ServiceInstall_RestartWhenCurrent(t *testing.T) {
	loaded := fakeSupervisor(t)
	sc := newServiceSceneServing(t, loaded)
	if r := sc.install(t, true, false); r.code != 0 {
		t.Fatalf("setup install failed: %+v", r)
	}
	// The precondition: without --restart this install is a no-op, so the run below
	// shows --restart overriding it rather than restarting something stale.
	sc.install(t, true, false).check(t, 0, wantAlreadyCurrentStdout, "")
	r := sc.install(t, true, true)
	if wantRestartStdout == "" && wantRestartStderr == "" {
		t.Fatalf("capture these into wantRestartStdout / wantRestartStderr:\nstdout: %q\nstderr: %q\nexit: %d",
			r.out, r.errOut, r.code)
	}
	r.check(t, 0, wantRestartStdout, wantRestartStderr)
	if strings.Contains(r.out, "Already current") {
		t.Errorf("--restart skipped the restart: %q", r.out)
	}
}

// norm strips the login-home warning so one golden serves both platforms; this pins
// the warning itself, on the raw output. On darwin a fresh --yes install under a
// t.TempDir HOME prints it once. On linux loginHome is "", so the branch is a no-op.
func TestCharacterize_ServiceInstall_LoginHomeWarning(t *testing.T) {
	fakeSupervisor(t)
	sc := newServiceScene(t)
	var out, errb bytes.Buffer
	if code := serviceInstall(sc.p, true, false, &out, &errb); code != 0 {
		t.Fatalf("install failed (exit %d): %q", code, errb.String())
	}
	if runtime.GOOS != "darwin" {
		if lh := loginHome(); lh != "" {
			t.Errorf("loginHome() = %q on %s, want \"\": the warning is darwin-only", lh, runtime.GOOS)
		}
		if strings.Contains(errb.String(), "but your login home is") {
			t.Errorf("a login-home warning printed on %s: %q", runtime.GOOS, errb.String())
		}
		return
	}
	if lh := loginHome(); lh == "" || lh == sc.home {
		t.Fatalf("loginHome() = %q with HOME = %q: this check needs a login home that differs from HOME", lh, sc.home)
	}
	if n := strings.Count(errb.String(), loginHomeWarning(sc.home)); n != 1 {
		t.Errorf("raw stderr holds the login-home warning %d times, want 1:\n%q", n, errb.String())
	}
}

// A proxy that ran the session archive kept its history through the restart, so the install
// says the archive keeps it rather than that it was cleared.
func TestCharacterize_ServiceInstall_OverAnArchivingProxy(t *testing.T) {
	fakeSupervisor(t)
	sc := newServiceScene(t)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"sessions":[],"archive":{"bytes":1,"maxBytes":2,"retentionDays":30}}`))
	}))
	t.Cleanup(api.Close)
	f, err := os.OpenFile(sc.p.configFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("  session_api_addr: " + strings.TrimPrefix(api.URL, "http://") + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	run := sc.install(t, true, false)
	const want = "  The proxy's memory is cleared, but its session archive is not: agentop\n" +
		"  still lists every session it holds.\n"
	if run.code != 0 || !strings.HasSuffix(run.out, want) || strings.Contains(run.out, "history is cleared") {
		t.Fatalf("exit %d, stdout:\n%s\nwant it to end with:\n%s", run.code, run.out, want)
	}
}
