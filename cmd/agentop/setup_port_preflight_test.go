package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The #1545 acceptance cases: a foreign holder on ANY listener the existing
// config names fails preflight naming the port and its holder; a proxy that
// logs "cannot start" during the health wait is a failure, not "ready"; and a
// fresh-install collision names a command that produces the file it says to
// edit.

// TestPreflightPorts_ForeignSessionAPIPortIsAProblem: the preflight used to
// check only the forward port, so a session API held by another process
// sailed through and the proxy failed at boot with setup calling the install
// ready. Now every listener the config names is checked.
func TestPreflightPorts_ForeignSessionAPIPortIsAProblem(t *testing.T) {
	fakeSupervisor(t)
	env := serviceEnv(t, ok200)
	freePorts(t) // lsof/ss stubs from freePorts are replaced below for our port

	// Pin the session API to a port nothing in this test binds — portHolder
	// is what says who holds it, and it is stubbed to name the stranger.
	cfgPath := env.configPath()
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(string(b), "roles: [forward]",
		"roles: [forward]\n  session_api_addr: 127.0.0.1:47890", 1)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// A stranger: a live process whose executable is not ours.
	stranger := startSleeperAt(t, filepath.Join(t.TempDir(), "stranger-proxy"))
	// lsof names the stranger on 47890 (portHolder) and names its exe
	// (setupPIDExePath); ss never runs.
	installStub(t, "lsof", "#!/bin/sh\ncase \"$*\" in *47890*) echo p"+strconv.Itoa(stranger)+" ;; -p*) echo n"+filepath.Join(t.TempDir(), "stranger-proxy")+" ;; esac\n")
	installStub(t, "ss", "#!/bin/sh\nexit 1\n")

	_, prob := serviceStep{}.preflightPorts(env)
	if prob == nil {
		t.Fatal("a foreign holder on the session API port passed preflight")
	}
	if !strings.Contains(prob.reason, "session-api") || !strings.Contains(prob.reason, "47890") {
		t.Errorf("problem does not name the listener and port: %q", prob.reason)
	}
	if !strings.Contains(prob.reason, strconv.Itoa(stranger)) {
		t.Errorf("problem does not name the holder: %q", prob.reason)
	}
}

// TestPreflightPorts_OurOwnSessionAPIPortStillPasses: the same check must not
// fire on our own running proxy, which holds every listener legitimately.
func TestPreflightPorts_OurOwnSessionAPIPortStillPasses(t *testing.T) {
	fakeSupervisor(t)
	env := serviceEnv(t, ok200)
	freePorts(t)

	cfgPath := env.configPath()
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(string(b), "roles: [forward]",
		"roles: [forward]\n  session_api_addr: 127.0.0.1:47890", 1)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// Ours: the pidfile names the holder, so ourProxy accepts it without
	// even asking the exe.
	ours := startSleeperAt(t, filepath.Join(env.binDir, "cortex"))
	if err := os.WriteFile(filepath.Join(env.cortexDir, "proxy.pid"), []byte(strconv.Itoa(ours)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	installStub(t, "lsof", "#!/bin/sh\ncase \"$*\" in *47890*) echo p"+strconv.Itoa(ours)+" ;; esac\n")
	installStub(t, "ss", "#!/bin/sh\nexit 1\n")

	hold, prob := serviceStep{}.preflightPorts(env)
	if prob != nil {
		t.Fatalf("our own proxy on the session API port is a problem: %v", prob)
	}
	if hold.known && hold.pid != ours {
		t.Errorf("hold = %+v, want the pidfile's %d", hold, ours)
	}
}

// TestApplyUnsupervised_CannotStartIsAFailure: a proxy that logs
// "cannot start" during the health wait will never answer, and the step used
// to return success — "not answering yet" — with setup printing ready and
// exiting 0 while the supervisor crash-looped the proxy against the port
// (#1545).
func TestApplyUnsupervised_CannotStartIsAFailure(t *testing.T) {
	fakeNoSupervisor(t)
	// The health endpoint answers 500 forever: the proxy is "alive but never
	// healthy", which is the state the false-ready shipped through.
	env := serviceEnv(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	freePorts(t)

	// A cortex whose supervise arm writes the cannot-start line to the log
	// (startUnsupervised redirects both streams there) and never serves
	// health — the shape portconflict.go produces every few seconds.
	writeExe(t, filepath.Join(env.binDir, "cortex"), `#!/bin/sh
case "$*" in
  --version) echo "cortex v9.9.9" ;;
  "--local --write-config") exit 0 ;;
  "--local --supervise")
    echo 'level=ERROR msg="cannot start: another process already holds this address" listener=session-api address=127.0.0.1:47601 holder_pid=65100' >&2
    exec sleep 300 ;;
esac
`)

	_, _, err := serviceStep{}.applyUnsupervised(env)
	if err == nil {
		t.Fatal("a proxy that logged cannot start was reported as started")
	}
	if !strings.Contains(err.Error(), "cannot bind") {
		t.Errorf("error does not say the proxy cannot bind: %v", err)
	}
}

// TestPreflightPorts_FreshInstallFixNamesWriteConfig: on a fresh install the
// collision fix pointed at config.yaml — a file setup refused to write, so
// the user had nothing to edit. Where a cortex is already on disk (an
// upgrade whose config was deleted), the fix must name the command that
// produces the file first.
func TestPreflightPorts_FreshInstallFixNamesWriteConfig(t *testing.T) {
	fakeSupervisor(t)
	env := newTestSetupEnv(t)
	env.configFresh = true
	freePorts(t)
	writeExe(t, filepath.Join(env.binDir, "cortex"), "#!/bin/sh\nexit 0\n")
	saved := portInUse
	portInUse = func(string) bool { return true }
	t.Cleanup(func() { portInUse = saved })

	_, prob := serviceStep{}.preflightPorts(env)
	if prob == nil {
		t.Fatal("taken default ports passed a fresh install's preflight")
	}
	joined := strings.Join(prob.fix, " ")
	if !strings.Contains(joined, "--write-config") {
		t.Errorf("fix does not name the command that writes the config: %v", prob.fix)
	}
	if !strings.Contains(joined, "change the ports") {
		t.Errorf("fix does not say to change the ports: %v", prob.fix)
	}
}

// TestPreflightPorts_FreshInstallWithoutCortexNamesNoCommand: this refusal
// happens in the plan phase, before the binaries step installs anything, so
// on a machine with no Cortex the write-config remedy names a command that
// cannot run — the same broken advice (edit a file that does not exist) the
// fix replaced. The remedy there is freeing the port and re-running.
func TestPreflightPorts_FreshInstallWithoutCortexNamesNoCommand(t *testing.T) {
	fakeSupervisor(t)
	env := newTestSetupEnv(t)
	env.configFresh = true
	freePorts(t)
	saved := portInUse
	portInUse = func(string) bool { return true }
	t.Cleanup(func() { portInUse = saved })

	_, prob := serviceStep{}.preflightPorts(env)
	if prob == nil {
		t.Fatal("taken default ports passed a fresh install's preflight")
	}
	joined := strings.Join(prob.fix, " ")
	if strings.Contains(joined, "--write-config") {
		t.Errorf("fix names a command whose binary this machine does not have: %v", prob.fix)
	}
	if !strings.Contains(joined, "re-run") {
		t.Errorf("fix does not say to re-run: %v", prob.fix)
	}
}

// TestPreflightPorts_TransparentAddrIsNotChecked: a hand-edited laptop config
// may carry a transparent_proxy_addr, but the proxy setup starts never binds
// it — both starts are local installs (the config lives under ~/.cortex),
// which skip the transparent listeners — so a foreign holder on that port is
// not a preflight problem.
func TestPreflightPorts_TransparentAddrIsNotChecked(t *testing.T) {
	fakeSupervisor(t)
	env := serviceEnv(t, ok200)
	freePorts(t)

	cfgPath := env.configPath()
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(string(b), "roles: [forward]",
		"roles: [forward]\n  transparent_proxy_addr: 127.0.0.1:47891", 1)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	stranger := startSleeperAt(t, filepath.Join(t.TempDir(), "stranger-proxy"))
	installStub(t, "lsof", "#!/bin/sh\ncase \"$*\" in *47891*) echo p"+strconv.Itoa(stranger)+" ;; -p*) echo n"+filepath.Join(t.TempDir(), "stranger-proxy")+" ;; esac\n")
	installStub(t, "ss", "#!/bin/sh\nexit 1\n")

	_, prob := serviceStep{}.preflightPorts(env)
	if prob != nil {
		t.Fatalf("a transparent port the local proxy never binds is a problem: %v", prob)
	}
}
