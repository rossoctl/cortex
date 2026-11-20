package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
)

// These tests cover the Cortex that was running before setup: v0.7.0's
// authbridge-proxy, supervised or in the background.

// lateFailure is a step after every real one that fails, so the rollback undoes
// all of them, the cleanup step's included.
type lateFailure struct{}

func (lateFailure) name() string { return "late step" }

func (lateFailure) plan(*setupEnv) (stepPlan, *problem) {
	return stepPlan{verb: "fail", what: "on purpose"}, nil
}

func (lateFailure) apply(*setupEnv, *checklist.Running) (string, undo, error) {
	return "", undo{}, errors.New("injected failure")
}

func failLast(t *testing.T) {
	t.Helper()
	saved := setupStepsHook
	setupStepsHook = func(s []step) []step { return append(append([]step(nil), s...), lateFailure{}) }
	t.Cleanup(func() { setupStepsHook = saved })
}

// detachedSleeperAt runs a copy of the test binary from path, as startSleeperAt
// does, but reparented to init: init reaps it once it is stopped, so kill -0 and
// stopPID see it go rather than a zombie of this process. It returns once the
// copy runs Go code.
func detachedSleeperAt(t *testing.T, path string) int {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self) //nolint:gosec // the test binary
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// Whatever stub is at path goes: a new file, not that inode rewritten.
	_ = os.Remove(path)
	if err := os.WriteFile(path, b, 0o755); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	ready, waitReady := readyPipe(t)
	cmd := exec.Command("/bin/sh", "-c", `"$0" >/dev/null 2>&1 </dev/null & echo $!`, path)
	cmd.Env = append(os.Environ(), sleeperEnv+"=1", sleeperReadyEnv+"=1")
	cmd.ExtraFiles = []*os.File{ready}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	// Only while the pid is still this copy: setup may stop it, and the number may
	// be reused by then.
	t.Cleanup(func() {
		if ranFrom(pid, path) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	waitReady()
	return pid
}

// ranFrom reports whether pid is alive and was started as path, by ps's argv[0].
func ranFrom(pid int, path string) bool {
	if !alive(pid) {
		return false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	f := strings.Fields(string(out))
	return err == nil && len(f) > 0 && f[0] == path
}

// preRenameProxyStub is v0.7.0's authbridge-proxy: our module path, which
// remove_stale's rule looks for, over the stub cortex's commands.
func preRenameProxyStub(cortexScript string) string {
	return "#!/bin/sh\n# github.com/rossoctl/cortex/authbridge/cmd/authbridge-proxy\n" +
		strings.TrimPrefix(cortexScript, "#!/bin/sh\n")
}

// v0.7.0, supervised: the job under setup's own label runs ~/.local/bin/
// authbridge-proxy, and its --supervise child holds the port. That is this Cortex
// before the rename, so setup upgrades it rather than refusing it as a stranger's.
// A failure after the cleanup step puts it back: the cleanup undo restores the
// binary, then restorePrior reloads the job, which runs it again.
func TestSetupUpgradesAPreRenameSupervisedCortex(t *testing.T) {
	running := filepath.Join(t.TempDir(), "running")
	sc := newSetupScene(t, func(w http.ResponseWriter, _ *http.Request) {
		if _, err := os.Stat(running); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	runningCortex(t, sc.loaded, sc.home, running)
	bin := filepath.Join(sc.home, ".local", "bin")
	stub, err := os.ReadFile(filepath.Join(sc.stage, "cortex"))
	if err != nil {
		t.Fatal(err)
	}
	proxy := preRenameProxyStub(string(stub))
	writeExe(t, filepath.Join(bin, "authbridge-proxy"), proxy)
	writeExe(t, filepath.Join(bin, "abctl"), "#!/bin/sh\n# github.com/rossoctl/cortex/authbridge/cmd/abctl\necho abctl v0.7.0\n")
	if out, err := exec.Command(filepath.Join(bin, "authbridge-proxy"), "--local", "--write-config").CombinedOutput(); err != nil {
		t.Fatalf("write-config: %v %s", err, out)
	}
	old, err := resolveServicePaths(filepath.Join(sc.home, ".cortex", "config.yaml"), "", filepath.Join(bin, "authbridge-proxy"))
	if err != nil {
		t.Fatal(err)
	}
	writeExe(t, old.unitFile, renderUnit(old))
	if err := loadService(runtimeGOOS(), old, io.Discard); err != nil {
		t.Fatal(err)
	}
	installStub(t, "lsof", `#!/bin/sh
case "$1" in
  -nP) case "$2" in -iTCP@127.0.0.1:47600) printf 'p31337\n' ;; esac ;;
esac
exit 0
`)
	fakeProc(t, 31337, filepath.Join(bin, "authbridge-proxy"))
	before := homeFiles(t, sc.home)
	failLast(t)

	code, out := sc.run(t, "--from", sc.stage, "--yes")
	if strings.Contains(out, "not this Cortex") || strings.Contains(out, "Nothing was changed.") {
		t.Fatalf("setup refused the pre-rename Cortex before changing anything:\n%s", out)
	}
	for _, want := range []string{
		"  ✓ started      " + supervisorName(runtimeGOOS()) + " · healthy on 127.0.0.1:47600\n",
		"  ✓ cleaned up   removed abctl, authbridge-proxy\n",
		"reverted     the previous Cortex is running again · healthy",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if code != 1 {
		t.Errorf("a failed upgrade exited %d", code)
	}
	sameFiles(t, before, homeFiles(t, sc.home))
	if b, _ := os.ReadFile(running); !bytes.Equal(b, []byte(proxy)) {
		t.Error("the job reloaded after the rollback does not run the pre-rename authbridge-proxy")
	}
}

// v0.7.0, in the background: proxy.pid names ~/.local/bin/authbridge-proxy. As
// start_unsupervised does, setup stops it before starting cortex — left running,
// it would keep the ports, orphaned once proxy.pid names the new proxy — and a
// failure after the cleanup step starts it again from the binary that undo
// restores.
func TestSetupStopsAPreRenameBackgroundProxyAndRestartsItOnRollback(t *testing.T) {
	// The new cortex notes whether the old proxy was still alive as it started, and
	// its own pid; it answers only once it has, so the rollback cannot stop it first.
	seen := filepath.Join(t.TempDir(), "seen")
	sc := newSetupScene(t, func(w http.ResponseWriter, _ *http.Request) {
		if _, err := os.Stat(seen); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	fakeNoSupervisor(t)
	t.Setenv(sleeperEnv, "1") // the restarted copy of the test binary sleeps too
	bin := filepath.Join(sc.home, ".local", "bin")
	pidFile := filepath.Join(sc.home, ".cortex", "proxy.pid")
	if out, err := exec.Command(filepath.Join(sc.stage, "cortex"), "--local", "--write-config").CombinedOutput(); err != nil {
		t.Fatalf("write-config: %v %s", err, out)
	}
	old := detachedSleeperAt(t, filepath.Join(bin, "authbridge-proxy"))
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(old)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stub, err := os.ReadFile(filepath.Join(sc.stage, "cortex"))
	if err != nil {
		t.Fatal(err)
	}
	const start = `"--local --supervise") exec sleep 300 ;;`
	recording := strings.Replace(string(stub), start, `"--local --supervise")
    { if kill -0 `+strconv.Itoa(old)+` 2>/dev/null; then echo alive; else echo gone; fi; echo $$; } > '`+seen+`.tmp'
    mv '`+seen+`.tmp' '`+seen+`'; exec sleep 300 ;;`, 1)
	if recording == string(stub) {
		t.Fatal("the stub cortex has no --supervise branch to record in")
	}
	writeExe(t, filepath.Join(sc.stage, "cortex"), recording)
	before := homeFiles(t, sc.home)
	failLast(t)
	var newPID, restarted int
	stopProcessOnCleanup(t, &newPID)
	stopProcessOnCleanup(t, &restarted)

	code, out := sc.run(t, "--from", sc.stage, "--yes")
	restarted = readPIDFile(pidFile)
	b, _ := os.ReadFile(seen)
	lines := strings.Fields(string(b))
	if len(lines) != 2 {
		t.Fatalf("the new cortex never started (%q):\n%s", b, out)
	}
	newPID, _ = strconv.Atoi(lines[1])
	if lines[0] != "gone" {
		t.Errorf("the new cortex started while the pre-rename proxy (pid %d) still ran:\n%s", old, out)
	}
	for _, want := range []string{
		"  ✓ cleaned up   removed authbridge-proxy\n",
		"reverted     the previous background Cortex is running again",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if code != 1 {
		t.Errorf("a failed run exited %d", code)
	}
	if alive(newPID) {
		t.Error("the rollback left the new cortex running")
	}
	if !ranFrom(restarted, filepath.Join(bin, "authbridge-proxy")) || restarted == old {
		t.Errorf("proxy.pid names %d after the rollback, not a restarted %s", restarted, filepath.Join(bin, "authbridge-proxy"))
	}
	// proxy.pid names the restarted proxy now, so it is compared by that, above.
	after := homeFiles(t, sc.home)
	delete(before, filepath.Join(".cortex", "proxy.pid"))
	delete(after, filepath.Join(".cortex", "proxy.pid"))
	sameFiles(t, before, after)
}

// backgroundCortexScene is a machine with a Cortex in the background, recorded in
// proxy.pid, and no service; setup now runs from a normal terminal. name is the
// binary it runs from ~/.local/bin: cortex, after an earlier --no-service or
// sandboxed install, or authbridge-proxy, v0.7.0's. It returns the scene and that
// proxy's pid.
func backgroundCortexScene(t *testing.T, name string) (setupScene, int) {
	t.Helper()
	sc := newSetupScene(t, ok200)
	t.Setenv(sleeperEnv, "1") // a restarted copy of the test binary sleeps too
	if name == "cortex" {
		if code, out := sc.run(t, "--from", sc.stage, "--yes", "--install-only"); code != 0 {
			t.Fatalf("first run %d\n%s", code, out)
		}
	}
	if out, err := exec.Command(filepath.Join(sc.stage, "cortex"), "--local", "--write-config").CombinedOutput(); err != nil {
		t.Fatalf("write-config: %v %s", err, out)
	}
	bin := filepath.Join(sc.home, ".local", "bin")
	old := detachedSleeperAt(t, filepath.Join(bin, name))
	if err := os.WriteFile(filepath.Join(sc.home, ".cortex", "proxy.pid"), []byte(strconv.Itoa(old)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sp, err := resolveServicePaths(filepath.Join(sc.home, ".cortex", "config.yaml"), "", filepath.Join(bin, "cortex"))
	if err != nil {
		t.Fatal(err)
	}
	if got := adoptablePID(sp); got != old {
		t.Fatalf("fixture: adoptablePID = %d, want the background %s %d", got, name, old)
	}
	return sc, old
}

// runServiceInstall stops the background Cortex to put the service in its place.
// A later failure undoes the service, so the rollback starts that Cortex again,
// from the binary it ran: put back by the binaries undo, for cortex.
func TestSetupRollbackRestartsTheBackgroundCortexTheServiceReplaced(t *testing.T) {
	for _, name := range []string{"cortex", "authbridge-proxy"} {
		t.Run(name, func(t *testing.T) {
			sc, old := backgroundCortexScene(t, name)
			bin := filepath.Join(sc.home, ".local", "bin", name)
			pidFile := filepath.Join(sc.home, ".cortex", "proxy.pid")
			before := homeFiles(t, sc.home)
			failAt(t, "routed")
			var restarted int
			stopProcessOnCleanup(t, &restarted)

			code, out := sc.run(t, "--from", sc.stage, "--yes", "--claude-code")
			restarted = readPIDFile(pidFile)
			for _, want := range []string{
				"  ✓ started      " + supervisorName(runtimeGOOS()) + " · healthy on 127.0.0.1:47600\n",
				"reverted     the previous background Cortex is running again",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			if code != 1 {
				t.Errorf("a failed run exited %d", code)
			}
			if restarted == old || !ranFrom(restarted, bin) {
				t.Errorf("proxy.pid names %d after the rollback, not a restarted %s (the original was %d)", restarted, bin, old)
			}
			if _, err := os.Stat(sc.loaded); err == nil {
				t.Error("the rollback left the service job loaded")
			}
			// proxy.pid names the restarted proxy now, so it is compared by that, above.
			after := homeFiles(t, sc.home)
			delete(before, filepath.Join(".cortex", "proxy.pid"))
			delete(after, filepath.Join(".cortex", "proxy.pid"))
			sameFiles(t, before, after)
		})
	}
}

// beforeApply runs fn just before the step it wraps applies.
type beforeApply struct {
	step
	fn func()
}

func (b beforeApply) apply(env *setupEnv, act *checklist.Running) (string, undo, error) {
	b.fn()
	return b.step.apply(env, act)
}

// A service install that fails before it stops the background Cortex (here, its
// binary gone) leaves that Cortex running, so the rollback starts no second copy:
// one would crash-loop on the ports and take proxy.pid from the first.
func TestSetupRollbackStartsNoSecondBackgroundCortex(t *testing.T) {
	sc, old := backgroundCortexScene(t, "cortex")
	bin := filepath.Join(sc.home, ".local", "bin", "cortex")
	before := homeFiles(t, sc.home)
	saved := setupStepsHook
	setupStepsHook = func(s []step) []step {
		out := append([]step(nil), s...)
		for i, st := range out {
			if st.name() == "started" {
				out[i] = beforeApply{st, func() { _ = os.Remove(bin) }}
			}
		}
		return out
	}
	t.Cleanup(func() { setupStepsHook = saved })
	var second int
	stopProcessOnCleanup(t, &second)

	code, out := sc.run(t, "--from", sc.stage, "--yes")
	if pf := readPIDFile(filepath.Join(sc.home, ".cortex", "proxy.pid")); pf != old {
		second = pf
		t.Errorf("proxy.pid names %d after the rollback, not the background cortex %d, still running", pf, old)
	}
	if code != 1 || !strings.Contains(out, "  ✗ started      cortex not found") || strings.Contains(out, "reverted") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if !ranFrom(old, bin) {
		t.Error("the background cortex is not running after the rollback")
	}
	// The install loaded nothing, so the undo's unload, which then fails, is best effort.
	if strings.Contains(out, "could not roll back") {
		t.Errorf("a service that was never loaded reads as one the rollback could not unload:\n%s", out)
	}
	sameFiles(t, before, homeFiles(t, sc.home))
}

// failingUnload makes fakeSupervisor's unload fail while its job is loaded, which
// then stays loaded, as a bootout launchd refuses does. It wraps only the fakes,
// which name loaded: the real launchctl acts on the real io.rossoctl.cortex.
func failingUnload(t *testing.T, loaded string) {
	t.Helper()
	for _, w := range []struct{ tool, unload string }{{"launchctl", "bootout*"}, {"systemctl", "*disable*"}} {
		fake, err := exec.LookPath(w.tool)
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(fake); !strings.Contains(string(b), loaded) { //nolint:gosec
			t.Fatalf("%s on PATH is not fakeSupervisor's", fake)
		}
		installStub(t, w.tool, `#!/bin/sh
case "$*" in
  `+w.unload+`) if [ -f '`+loaded+`' ]; then echo 'unload refused' >&2; exit 5; fi ;;
esac
exec '`+fake+`' "$@"
`)
	}
}

// An unload the supervisor refuses leaves the new job loaded. The rollback says
// so, with the supervisor's own commands to finish it and the unit left for them,
// rather than that the changes are undone. And it starts no background copy
// beside that job, which answers the health check the copy would be judged by.
func TestSetupRollbackReportsAServiceItCouldNotUnload(t *testing.T) {
	sc, _ := backgroundCortexScene(t, "cortex")
	pidFile := filepath.Join(sc.home, ".cortex", "proxy.pid")
	failingUnload(t, sc.loaded)
	failAt(t, "routed")
	var second int
	stopProcessOnCleanup(t, &second)

	code, out := sc.run(t, "--from", sc.stage, "--yes", "--claude-code")
	if second = readPIDFile(pidFile); second != 0 {
		t.Errorf("the rollback started a background cortex (pid %d) beside the service it could not unload", second)
	}
	if _, err := os.Stat(sc.loaded); err != nil {
		t.Fatal("fixture: the fake job is not loaded, so the unload did not fail")
	}
	sp, err := resolveServicePaths(filepath.Join(sc.home, ".cortex", "config.yaml"), "", filepath.Join(sc.home, ".local", "bin", "cortex"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sp.unitFile); err != nil {
		t.Error("the undo removed the unit the manual fix unloads by")
	}
	unload := "systemctl --user disable --now cortex.service; rm -f ~/.config/systemd/user/cortex.service; systemctl --user daemon-reload"
	if runtimeGOOS() == "darwin" {
		unload = "launchctl bootout gui/" + strconv.Itoa(os.Getuid()) + "/io.rossoctl.cortex; rm -f ~/Library/LaunchAgents/io.rossoctl.cortex.plist"
	}
	for _, want := range []string{
		"    could not roll back the service: ",
		"unload refused\n      do it yourself: " + unload + "\n",
		"    could not roll back the previous Cortex: the new service is still loaded, so a background copy would only fight it for the ports\n" +
			"      do it yourself: sh -c 'nohup ~/.local/bin/cortex --local --supervise >> ~/.cortex/proxy.log 2>&1 & echo $! > ~/.cortex/proxy.pid'\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the rollback does not report %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "the changes above are undone") || code != 1 {
		t.Errorf("exit %d, and a rollback that left the service loaded calls itself complete:\n%s", code, out)
	}
}

// Beside a healthy supervised Cortex, a pidfile proxy runServiceInstall also
// adopts and stops. The supervised one is what was serving, so the rollback
// reloads its unit and starts no background copy.
func TestSetupRollbackReloadsTheSupervisedCortexOverABackgroundOne(t *testing.T) {
	readyOnFirstAnswer(t)
	sc, running := newUpgradeScene(t)
	bin := filepath.Join(sc.home, ".local", "bin", "cortex")
	pidFile := filepath.Join(sc.home, ".cortex", "proxy.pid")
	beside := filepath.Join(t.TempDir(), "cortex")
	old := detachedSleeperAt(t, beside)
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(old)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sp, err := resolveServicePaths(filepath.Join(sc.home, ".cortex", "config.yaml"), "", bin)
	if err != nil {
		t.Fatal(err)
	}
	if got := adoptablePID(sp); got != old {
		t.Fatalf("fixture: adoptablePID = %d, want the pidfile proxy %d", got, old)
	}
	prev, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	next := t.TempDir()
	writeExe(t, filepath.Join(next, "agentop"), "#!/bin/sh\necho agentop v10.0.0\n")
	writeExe(t, filepath.Join(next, "cortex"), string(prev)+"# v10.0.0\n")
	before := homeFiles(t, sc.home)
	failLast(t)
	var second int
	stopProcessOnCleanup(t, &second)

	code, out := sc.run(t, "--from", next, "--yes")
	if ranFrom(old, beside) {
		t.Fatal("fixture: runServiceInstall did not stop the pidfile proxy")
	}
	if second = readPIDFile(pidFile); second != 0 {
		t.Errorf("the rollback started a background cortex (pid %d) beside the supervised one", second)
	}
	if code != 1 || !strings.Contains(out, "reverted     the previous Cortex is running again · healthy") ||
		strings.Contains(out, "background") {
		t.Errorf("exit %d, and the rollback did not reload the supervised Cortex alone:\n%s", code, out)
	}
	if b, _ := os.ReadFile(running); !bytes.Equal(b, prev) {
		t.Error("the job loaded after the rollback does not run the previous cortex")
	}
	// runServiceInstall stopped the pidfile proxy and removed its pidfile; nothing
	// brings that back, which second == 0 above checks.
	after := homeFiles(t, sc.home)
	delete(before, filepath.Join(".cortex", "proxy.pid"))
	sameFiles(t, before, after)
}

// Beside a service stopped with agentop service stop, a pidfile proxy answers the
// health check. That is the background Cortex, which runServiceInstall adopts and
// stops, so the rollback starts it again and leaves the job stopped.
func TestSetupRollbackRestartsTheBackgroundCortexBesideAStoppedService(t *testing.T) {
	readyOnFirstAnswer(t)
	running := filepath.Join(t.TempDir(), "running")
	sc := newSetupScene(t, func(w http.ResponseWriter, _ *http.Request) {
		_, err := os.Stat(running)
		if err != nil && !alive(readPIDFile(filepath.Join(os.Getenv("HOME"), ".cortex", "proxy.pid"))) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	t.Setenv(sleeperEnv, "1") // the restarted copy of the test binary sleeps too
	runningCortex(t, sc.loaded, sc.home, running)
	if code, out := sc.run(t, "--from", sc.stage, "--yes"); code != 0 {
		t.Fatalf("first install: %d\n%s", code, out)
	}
	bin := filepath.Join(sc.home, ".local", "bin", "cortex")
	pidFile := filepath.Join(sc.home, ".cortex", "proxy.pid")
	sp, err := resolveServicePaths(filepath.Join(sc.home, ".cortex", "config.yaml"), "", bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := controlService(runtimeGOOS(), "stop", sp, io.Discard); err != nil {
		t.Fatal(err)
	}
	beside := filepath.Join(t.TempDir(), "cortex")
	old := detachedSleeperAt(t, beside)
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(old)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sc.loaded); err == nil || adoptablePID(sp) != old {
		t.Fatalf("fixture: job loaded %v, adoptablePID = %d, want the pidfile proxy %d", err == nil, adoptablePID(sp), old)
	}
	prev, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	next := t.TempDir()
	writeExe(t, filepath.Join(next, "agentop"), "#!/bin/sh\necho agentop v10.0.0\n")
	writeExe(t, filepath.Join(next, "cortex"), string(prev)+"# v10.0.0\n")
	before := homeFiles(t, sc.home)
	failLast(t)
	var restarted int
	stopProcessOnCleanup(t, &restarted)

	code, out := sc.run(t, "--from", next, "--yes")
	restarted = readPIDFile(pidFile)
	if ranFrom(old, beside) {
		t.Fatal("fixture: runServiceInstall did not stop the pidfile proxy")
	}
	if _, err := os.Stat(sc.loaded); err == nil {
		t.Error("the rollback loaded the job agentop service stop had stopped")
	}
	if restarted == old || !ranFrom(restarted, beside) {
		t.Errorf("proxy.pid names %d after the rollback, not a restarted %s", restarted, beside)
	}
	if code != 1 || !strings.Contains(out, "reverted     the previous background Cortex is running again\n") ||
		strings.Contains(out, "the previous Cortex is running again") {
		t.Errorf("exit %d, and the rollback did not restart the background Cortex alone:\n%s", code, out)
	}
	// proxy.pid names the restarted proxy now, so it is compared by that, above.
	after := homeFiles(t, sc.home)
	delete(before, filepath.Join(".cortex", "proxy.pid"))
	delete(after, filepath.Join(".cortex", "proxy.pid"))
	sameFiles(t, before, after)
}

// asyncTeardown makes fakeSupervisor's bootout of a loaded job return at once and
// the job leave the domain at the second look at it after that, as launchd's does
// while the job's supervisor drains the proxy: the first look still finds it
// running. Everything else goes to the fake, which it alone may wrap: the real
// launchctl acts on the real io.rossoctl.cortex.
func asyncTeardown(t *testing.T, loaded string) {
	t.Helper()
	fake, err := exec.LookPath("launchctl")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(fake); !strings.Contains(string(b), loaded) { //nolint:gosec
		t.Fatalf("%s on PATH is not fakeSupervisor's", fake)
	}
	leaving, looked := loaded+".leaving", loaded+".looked"
	installStub(t, "launchctl", `#!/bin/sh
case "$1" in
  bootout) if [ -f '`+loaded+`' ]; then : > '`+leaving+`'; exit 0; fi ;;
  bootstrap) rm -f '`+leaving+`' '`+looked+`' ;;
  print)
    case "$2" in
      */*/*) if [ -f '`+leaving+`' ]; then
               if [ -f '`+looked+`' ]; then rm -f '`+loaded+`' '`+leaving+`' '`+looked+`'; else : > '`+looked+`'; fi
             fi ;;
    esac ;;
esac
exec '`+fake+`' "$@"
`)
}

// launchd's bootout returns before the job has gone, so the service undo waits
// for it: an undo that says "undone" means the job has left. Without the wait, the
// background restore after it finds the job still loaded and refuses.
func TestSetupRollbackWaitsOutLaunchdTeardown(t *testing.T) {
	if runtimeGOOS() != "darwin" {
		t.Skip("launchd's bootout only: systemd's disable --now returns once the unit has stopped")
	}
	readyOnFirstAnswer(t)
	t.Run("a fresh service, then the background Cortex it replaced", func(t *testing.T) {
		sc, old := backgroundCortexScene(t, "cortex")
		bin := filepath.Join(sc.home, ".local", "bin", "cortex")
		pidFile := filepath.Join(sc.home, ".cortex", "proxy.pid")
		asyncTeardown(t, sc.loaded)
		failAt(t, "routed")
		var restarted int
		stopProcessOnCleanup(t, &restarted)

		code, out := sc.run(t, "--from", sc.stage, "--yes", "--claude-code")
		_, stillLoaded := os.Stat(sc.loaded)
		restarted = readPIDFile(pidFile)
		if stillLoaded == nil {
			t.Error("setup returned with the service undone and its job still tearing down")
		}
		if code != 1 || strings.Contains(out, "could not roll back") ||
			!strings.Contains(out, "undone       the service\n") ||
			!strings.Contains(out, "reverted     the previous background Cortex is running again\n") {
			t.Errorf("exit %d, and the rollback did not wait for the job to go before the restore:\n%s", code, out)
		}
		if restarted == old || !ranFrom(restarted, bin) {
			t.Errorf("proxy.pid names %d after the rollback, not a restarted %s", restarted, bin)
		}
	})
	t.Run("a stopped service", func(t *testing.T) {
		running := filepath.Join(t.TempDir(), "running")
		sc := newSetupScene(t, func(w http.ResponseWriter, _ *http.Request) {
			if _, err := os.Stat(running); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		})
		asyncTeardown(t, sc.loaded)
		runningCortex(t, sc.loaded, sc.home, running)
		if code, out := sc.run(t, "--from", sc.stage, "--yes"); code != 0 {
			t.Fatalf("first install: %d\n%s", code, out)
		}
		bin := filepath.Join(sc.home, ".local", "bin", "cortex")
		sp, err := resolveServicePaths(filepath.Join(sc.home, ".cortex", "config.yaml"), "", bin)
		if err != nil {
			t.Fatal(err)
		}
		if err := controlService(runtimeGOOS(), "stop", sp, io.Discard); err != nil {
			t.Fatal(err)
		}
		// The stopped job leaves at the second look.
		target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + launchdLabel
		for range 2 {
			_, _ = labelGone(target)
		}
		if _, err := os.Stat(sc.loaded); err == nil {
			t.Fatal("fixture: the stopped job never left")
		}
		prev, err := os.ReadFile(bin)
		if err != nil {
			t.Fatal(err)
		}
		next := t.TempDir()
		writeExe(t, filepath.Join(next, "agentop"), "#!/bin/sh\necho agentop v10.0.0\n")
		writeExe(t, filepath.Join(next, "cortex"), string(prev)+"# v10.0.0\n")
		before := homeFiles(t, sc.home)
		failLast(t)

		code, out := sc.run(t, "--from", next, "--yes")
		if _, err := os.Stat(sc.loaded); err == nil {
			t.Error("setup returned with the stopped service's job still tearing down")
		}
		if code != 1 || strings.Contains(out, "could not roll back") || !strings.Contains(out, "undone       the service\n") {
			t.Errorf("exit %d:\n%s", code, out)
		}
		sameFiles(t, before, homeFiles(t, sc.home))
	})
}
