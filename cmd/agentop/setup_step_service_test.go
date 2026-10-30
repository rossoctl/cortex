package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func ok200(w http.ResponseWriter, _ *http.Request) {}

func TestServiceStepFreshInstallAndUndo(t *testing.T) {
	loaded := fakeSupervisor(t)
	env := serviceEnv(t, ok200)
	p, prob := serviceStep{}.plan(env)
	if prob != nil || p.done || !strings.Contains(p.where, supervisorName(env.goos)) || env.priorService {
		t.Fatalf("plan = %+v %v prior=%v", p, prob, env.priorService)
	}
	detail, u, err := serviceStep{}.apply(env, nil)
	if first, _, _ := strings.Cut(detail, "\n"); err != nil || !strings.HasSuffix(first, "healthy on 127.0.0.1:47600") {
		t.Fatalf("apply = %q %v", detail, err)
	}
	if _, err := os.Stat(loaded); err != nil {
		t.Fatal("the fake job is not loaded")
	}
	// The supervisor's own commands: a fresh install's rollback removes agentop.
	want := "systemctl --user disable --now cortex.service; rm -f ~/.config/systemd/user/cortex.service; systemctl --user daemon-reload"
	if env.goos == "darwin" {
		want = "launchctl bootout gui/" + strconv.Itoa(os.Getuid()) + "/io.rossoctl.cortex; rm -f ~/Library/LaunchAgents/io.rossoctl.cortex.plist"
	}
	if u.manual != want {
		t.Errorf("fresh undo manual = %q, want %q", u.manual, want)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(loaded); err == nil {
		t.Error("undo left the job loaded")
	}
	sp, _ := resolveServicePaths(env.configPath(), "", filepath.Join(env.binDir, "cortex"))
	for _, f := range []string{sp.unitFile, sp.stampFile} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("undo left %s", f)
		}
	}
}

func TestServiceStepIsDoneWhenCurrent(t *testing.T) {
	fakeSupervisor(t)
	env := serviceEnv(t, ok200)
	if _, prob := (serviceStep{}).plan(env); prob != nil {
		t.Fatal(prob)
	}
	if _, _, err := (serviceStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	again := *env
	again.configFresh, again.binaryChanges = false, nil
	if p, prob := (serviceStep{}).plan(&again); prob != nil || !p.done || !again.priorService {
		t.Errorf("a current service planned a change: %+v %v", p, prob)
	}
}

func TestServiceStepUpgradeUndoRestoresTheUnitAndThePrior(t *testing.T) {
	loaded := fakeSupervisor(t)
	env := serviceEnv(t, ok200)
	if _, prob := (serviceStep{}).plan(env); prob != nil {
		t.Fatal(prob)
	}
	if _, _, err := (serviceStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	sp, _ := resolveServicePaths(env.configPath(), "", filepath.Join(env.binDir, "cortex"))
	// An older agentop's unit, so the upgrade's rewrite changes its bytes: without
	// this the rewrite is byte-identical and an undo that skipped the unit would pass.
	unitBefore, _ := os.ReadFile(sp.unitFile)
	unitBefore = append(unitBefore, "\n# written by an older agentop\n"...)
	if err := os.WriteFile(sp.unitFile, unitBefore, 0o644); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	stampBefore, _ := os.ReadFile(sp.stampFile)

	up := *env
	up.configFresh, up.binaryChanges = false, []string{"cortex"}
	writeExe(t, filepath.Join(env.binDir, "cortex"), cortexStub("x")+"# v2\n")
	if p, prob := (serviceStep{}).plan(&up); prob != nil || p.done || !up.priorService {
		t.Fatalf("upgrade plan = %+v %v prior=%v", p, prob, up.priorService)
	}
	_, u, err := serviceStep{}.apply(&up, nil)
	if err != nil {
		t.Fatal(err)
	}
	if u.manual != "agentop service install --restart" {
		t.Errorf("upgrade undo manual = %q", u.manual)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(sp.unitFile); !bytes.Equal(b, unitBefore) {
		t.Error("undo did not restore the unit's bytes")
	}
	if b, _ := os.ReadFile(sp.stampFile); !bytes.Equal(b, stampBefore) {
		t.Error("undo did not restore the stamp")
	}
	if up.restorePrior == nil {
		t.Fatal("an upgrade over a serving Cortex set no restorePrior")
	}
	// The job is still loaded from the upgrade: unload it, as a failed later step's
	// rollback would find it, so only restorePrior can bring it back.
	if err := os.Remove(loaded); err != nil {
		t.Fatal(err)
	}
	if err := up.restorePrior(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(loaded); err != nil {
		t.Error("restorePrior did not load the job")
	}
}

// A restore of the previous service that fails names the manual fix for what is
// left: service restart while the unit is there, setup itself once the service
// undo could not put it back.
func TestServiceStepFailedRestoreNamesWhatIsLeft(t *testing.T) {
	shortReadyTimeout(t)
	for _, unitGone := range []bool{false, true} {
		t.Run(fmt.Sprintf("unit gone %v", unitGone), func(t *testing.T) {
			fakeSupervisor(t)
			var serving atomic.Bool
			serving.Store(true)
			env := serviceEnv(t, func(w http.ResponseWriter, _ *http.Request) {
				if !serving.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			})
			if _, prob := (serviceStep{}).plan(env); prob != nil {
				t.Fatal(prob)
			}
			if _, _, err := (serviceStep{}).apply(env, nil); err != nil {
				t.Fatal(err)
			}
			up := *env
			up.configFresh, up.binaryChanges = false, []string{"cortex"}
			// New bytes, so the install replaces the job rather than finding it current.
			writeExe(t, filepath.Join(env.binDir, "cortex"), cortexStub("x")+"# v2\n")
			if p, prob := (serviceStep{}).plan(&up); prob != nil || p.done || !up.priorService {
				t.Fatalf("upgrade plan = %+v %v prior=%v", p, prob, up.priorService)
			}
			if _, _, err := (serviceStep{}).apply(&up, nil); err != nil || up.restorePrior == nil {
				t.Fatalf("upgrade apply = %v, restorePrior set %v", err, up.restorePrior != nil)
			}
			sp, _ := resolveServicePaths(env.configPath(), "", filepath.Join(env.binDir, "cortex"))
			if unitGone {
				if err := os.Remove(sp.unitFile); err != nil {
					t.Fatal(err)
				}
			}
			serving.Store(false)
			want := "" // rollback's default, agentop service restart
			if unitGone {
				want = "re-run agentop setup"
			}
			if err := up.restorePrior(); err == nil || up.priorManual != want {
				t.Errorf("restore error %v, manual %q, want %q", err, up.priorManual, want)
			}
		})
	}
}

// A background proxy the unsupervised start stops is started again by a
// rollback. If that fails, the manual fix is the same start by hand: there is no
// service to restart.
func TestServiceStepUnsupervisedRestoreNamesItsStartByHand(t *testing.T) {
	fakeNoSupervisor(t)
	env := serviceEnv(t, ok200)
	t.Setenv(sleeperEnv, "1") // the copy of the test binary the step starts sleeps too
	pidFile := filepath.Join(env.cortexDir, "proxy.pid")
	old := detachedSleeperAt(t, filepath.Join(env.binDir, "cortex"))
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(old)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.binaryChanges = []string{"cortex"}
	if p, prob := (serviceStep{}).plan(env); prob != nil || p.done {
		t.Fatalf("plan = %+v %v", p, prob)
	}
	var pid int
	stopProcessOnCleanup(t, &pid)
	_, u, err := serviceStep{}.apply(env, nil)
	pid = readPIDFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	if alive(old) {
		t.Fatal("fixture: the step did not stop the background proxy")
	}
	want := "sh -c 'nohup ~/.local/bin/cortex --local --supervise >> ~/.cortex/proxy.log 2>&1 & echo $! > ~/.cortex/proxy.pid'"
	if env.restorePrior == nil || env.priorManual != want {
		t.Errorf("restorePrior set %v, manual %q, want %q", env.restorePrior != nil, env.priorManual, want)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
}

// stopProcessOnCleanup kills *pid, if it is still alive when the test ends:
// startUnsupervised's own goroutine reaps it, so this waits for it to go.
func stopProcessOnCleanup(t *testing.T, pid *int) {
	t.Helper()
	t.Cleanup(func() {
		if !alive(*pid) {
			return
		}
		_ = syscall.Kill(*pid, syscall.SIGKILL)
		for i := 0; i < 50 && alive(*pid); i++ {
			time.Sleep(100 * time.Millisecond)
		}
	})
}

func TestServiceStepRunsUnsupervisedInASandbox(t *testing.T) {
	fakeNoSupervisor(t)
	env := serviceEnv(t, ok200)
	p, prob := serviceStep{}.plan(env)
	if prob != nil || !env.unsupervised || !strings.HasPrefix(p.where, "unsupervised") {
		t.Fatalf("plan = %+v %v", p, prob)
	}
	if strings.Contains(p.where, "crash") { // cortex --supervise restarts its proxy
		t.Errorf("consent row %q says a crash is not recovered", p.where)
	}
	var pid int
	stopProcessOnCleanup(t, &pid)
	_, u, err := serviceStep{}.apply(env, nil)
	pid = readPIDFile(filepath.Join(env.cortexDir, "proxy.pid"))
	if err != nil {
		t.Fatal(err)
	}
	if !alive(pid) {
		t.Fatal("no background proxy")
	}
	if u.manual != "kill $(cat ~/.cortex/proxy.pid)" {
		t.Errorf("unsupervised undo manual = %q", u.manual)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if alive(pid) {
		t.Error("undo left the background proxy running")
		return
	}
	pid = 0 // gone: the cleanup must not signal whatever reuses the number
}

func TestServiceStepPreflight(t *testing.T) {
	t.Run("a busy port on a fresh install", func(t *testing.T) {
		fakeSupervisor(t)
		env := newTestSetupEnv(t)
		env.configFresh = true
		saved := portInUse
		portInUse = func(p string) bool { return p == "47601" }
		t.Cleanup(func() { portInUse = saved })
		_, prob := serviceStep{}.plan(env)
		if prob == nil || prob.reason != "port 47601 is already in use by something else" {
			t.Errorf("problem = %+v", prob)
		}
	})
	t.Run("a foreign holder of 47600", func(t *testing.T) {
		fakeSupervisor(t)
		env := serviceEnv(t, ok200)
		strangerOn(t, "47600")
		_, prob := serviceStep{}.plan(env)
		if prob == nil || !strings.Contains(prob.reason, "/usr/bin/python3 (pid 31337), not this Cortex") {
			t.Errorf("problem = %+v", prob)
		}
	})
	// The port checked is the one the config names, so moving it, as the fresh
	// install's busy-port advice says to, is not refused on the old one.
	t.Run("a stranger on 47600 after the config moved to 47700", func(t *testing.T) {
		fakeSupervisor(t)
		env := serviceEnv(t, ok200)
		moveForwardPort(t, env, "127.0.0.1:47700")
		strangerOn(t, "47600")
		if _, prob := (serviceStep{}).plan(env); prob != nil {
			t.Errorf("a stranger on the old port refused the moved one: %+v", prob)
		}
	})
	t.Run("a stranger on the moved port", func(t *testing.T) {
		fakeSupervisor(t)
		env := serviceEnv(t, ok200)
		moveForwardPort(t, env, "127.0.0.1:47700")
		strangerOn(t, "47700")
		_, prob := serviceStep{}.plan(env)
		// The message names the listener since #1545 — the preflight checks
		// every port the config binds, so which one matters.
		if prob == nil || !strings.HasPrefix(prob.reason, "the forward-proxy port 127.0.0.1:47700 is held by /usr/bin/python3 (pid 31337)") {
			t.Errorf("moved-port problem = %+v", prob)
		}
	})
	t.Run("a stranger run from under HOME", func(t *testing.T) {
		fakeSupervisor(t)
		env := serviceEnv(t, ok200)
		installStub(t, "lsof", `#!/bin/sh
case "$1" in
  -nP) case "$2" in -iTCP@127.0.0.1:47600) printf 'p31337\n' ;; esac ;;
  -p) printf 'p31337\nftxt\nn`+env.home+`/tools/squid\n' ;;
esac
exit 0
`)
		fakeProc(t, 31337, "")
		_, prob := serviceStep{}.plan(env)
		if prob == nil || prob.reason != "the forward-proxy port 127.0.0.1:47600 is held by ~/tools/squid (pid 31337), not this Cortex" {
			t.Errorf("problem = %+v", prob)
		}
	})
	// A stranger its supervisor would start again after a kill: the fix stops the
	// job instead. Each OS's lookup is stubbed, so both run anywhere.
	for _, tc := range []struct {
		goos, want string
		job        func(t *testing.T)
	}{
		{"darwin", "stop the job that runs it: launchctl bootout gui/" + strconv.Itoa(os.Getuid()) + "/org.squid-cache.squid", func(t *testing.T) {
			installStub(t, "launchctl", "#!/bin/sh\n[ \"$1\" = list ] && printf 'PID\\tStatus\\tLabel\\n-\\t0\\tcom.apple.x\\n31337\\t0\\torg.squid-cache.squid\\n'\nexit 0\n")
		}},
		{"linux", "stop the job that runs it: systemctl --user disable --now squid.service", func(t *testing.T) {
			cg := "0::/user.slice/user-501.slice/user@501.service/app.slice/squid.service\n"
			if err := os.WriteFile(filepath.Join(procRoot, "31337", "cgroup"), []byte(cg), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run("a stranger a "+tc.goos+" job runs", func(t *testing.T) {
			fakeSupervisor(t)
			env := serviceEnv(t, ok200)
			env.goos = tc.goos
			strangerOn(t, "47600")
			_, prob := serviceStep{}.preflightPorts(env)
			if prob == nil || len(prob.fix) != 2 || prob.fix[0] != "kill 31337" {
				t.Fatalf("with no job found, the fix is not the kill: %+v", prob)
			}
			tc.job(t)
			_, prob = serviceStep{}.preflightPorts(env)
			if prob == nil || !slices.Equal(prob.fix, []string{tc.want}) {
				t.Errorf("fix = %+v, want [%q]", prob, tc.want)
			}
		})
	}
	// The holder is ours when proxy.pid names it, or when it runs the installed
	// cortex: each case pins one of the two paths the step hands ourProxy.
	for _, tc := range []struct {
		name    string
		pidFile bool
	}{{"our proxy by its pidfile", true}, {"our proxy by its executable", false}} {
		t.Run(tc.name, func(t *testing.T) {
			fakeSupervisor(t)
			env := serviceEnv(t, ok200)
			installStub(t, "lsof", `#!/bin/sh
case "$1" in
  -nP) case "$2" in -iTCP@127.0.0.1:47600) printf 'p31337\n' ;; esac ;;
esac
exit 0
`)
			exe := filepath.Join(env.binDir, "cortex")
			if tc.pidFile {
				exe = "/usr/bin/python3"
				if err := os.WriteFile(filepath.Join(env.cortexDir, "proxy.pid"), []byte("31337\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			fakeProc(t, 31337, exe)
			if _, prob := (serviceStep{}).plan(env); prob != nil {
				t.Errorf("our own proxy read as foreign: %+v", prob)
			}
		})
	}
}

// strangerOn makes lsof name pid 31337, /usr/bin/python3, as the listener on
// 127.0.0.1:port.
func strangerOn(t *testing.T, port string) {
	t.Helper()
	installStub(t, "lsof", `#!/bin/sh
case "$1" in
  -nP) case "$2" in -iTCP@127.0.0.1:`+port+`) printf 'p31337\n' ;; esac ;;
  -p) printf 'p31337\nftxt\nn/usr/bin/python3\n' ;;
esac
exit 0
`)
	fakeProc(t, 31337, "")
}

// moveForwardPort rewrites the config's forward_proxy_addr to addr.
func moveForwardPort(t *testing.T, env *setupEnv, addr string) {
	t.Helper()
	b, err := os.ReadFile(env.configPath())
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(string(b), "forward_proxy_addr: 127.0.0.1:47600", "forward_proxy_addr: "+addr, 1)
	if moved == string(b) {
		t.Fatal("the config names no forward_proxy_addr to move")
	}
	if err := os.WriteFile(env.configPath(), []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}
}

// logSupervisorCalls wraps fakeSupervisor's launchctl and systemctl, so each call
// is appended to the returned log before the fake answers it. It refuses to wrap
// anything but the fakes, which name loaded: the real launchctl acts on the real
// io.rossoctl.cortex.
func logSupervisorCalls(t *testing.T, loaded string) string {
	t.Helper()
	calls := filepath.Join(t.TempDir(), "calls.log")
	for _, tool := range []string{"launchctl", "systemctl"} {
		fake, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(fake); !strings.Contains(string(b), loaded) { //nolint:gosec
			t.Fatalf("%s on PATH is not fakeSupervisor's", fake)
		}
		installStub(t, tool, "#!/bin/sh\necho '"+tool+" '\"$*\" >> '"+calls+"'\nexec '"+fake+"' \"$@\"\n")
	}
	return calls
}

// With the service current, a config step that adds listener pins makes the
// service step restart it: the config step's migration leaves runServiceInstall's
// own with nothing to do. Bob's rate alone does not, as the proxy reloads pricing.
func TestServiceStepRestartsForNewPinsButNotForBobsRate(t *testing.T) {
	for _, tc := range []struct {
		name string
		pins bool
	}{{"new listener pins", true}, {"Bob's rate only", false}} {
		t.Run(tc.name, func(t *testing.T) {
			loaded := fakeSupervisor(t)
			env := serviceEnv(t, ok200)
			unpinned, err := os.ReadFile(env.configPath())
			if err != nil {
				t.Fatal(err)
			}
			if _, prob := (serviceStep{}).plan(env); prob != nil {
				t.Fatal(prob)
			}
			if _, _, err := (serviceStep{}).apply(env, nil); err != nil {
				t.Fatal(err)
			}
			// The service is current; its config goes back to needing the migrations.
			if err := os.WriteFile(env.configPath(), unpinned, 0o600); err != nil {
				t.Fatal(err)
			}
			if !tc.pins {
				if _, err := migrateConfig(env.configPath(), io.Discard); err != nil {
					t.Fatal(err)
				}
			}
			next := *env
			if cp, prob := (configStep{}).plan(&next); prob != nil || cp.verb != "update" || next.configPinsPending != tc.pins {
				t.Fatalf("config plan: %+v %v pinsPending=%v", cp, prob, next.configPinsPending)
			}
			if _, _, err := (configStep{}).apply(&next, nil); err != nil || next.configPinsChanged != tc.pins {
				t.Fatalf("config apply: %v pinsChanged=%v", err, next.configPinsChanged)
			}
			calls := logSupervisorCalls(t, loaded)
			p, prob := serviceStep{}.plan(&next)
			if prob != nil || p.done != !tc.pins {
				t.Fatalf("service plan done=%v, want %v (%v)", p.done, !tc.pins, prob)
			}
			if !tc.pins {
				return
			}
			detail, _, err := serviceStep{}.apply(&next, nil)
			if first, _, _ := strings.Cut(detail, "\n"); err != nil || strings.HasSuffix(first, "already running") {
				t.Errorf("new pins did not restart the service: %q %v", detail, err)
			}
			log, _ := os.ReadFile(calls)
			if !strings.Contains(string(log), "launchctl bootstrap") && !strings.Contains(string(log), "systemctl --user restart") {
				t.Errorf("the supervisor saw no start: %q", log)
			}
		})
	}
}

// Unsupervised, beside a Cortex of ours that proxy.pid does not record: a
// supervisor runs it, so setup must not start a second copy on its ports.
func TestServiceStepWillNotStartASecondCortexBesideASupervisedOne(t *testing.T) {
	ourHolder := func(t *testing.T, env *setupEnv) {
		t.Helper()
		installStub(t, "lsof", `#!/bin/sh
case "$1" in
  -nP) case "$2" in -iTCP@127.0.0.1:47600) printf 'p31337\n' ;; esac ;;
esac
exit 0
`)
		fakeProc(t, 31337, filepath.Join(env.binDir, "cortex"))
	}
	sup := func(env *setupEnv) string { return strings.Fields(supervisorName(env.goos))[0] }

	t.Run("nothing changes", func(t *testing.T) {
		fakeNoSupervisor(t)
		env := serviceEnv(t, ok200)
		ourHolder(t, env)
		p, prob := serviceStep{}.plan(env)
		want := "Cortex is already running under " + sup(env) + "; not starting a second copy"
		if prob != nil || !p.done || p.advice == nil || p.advice.reason != want {
			t.Errorf("plan = %+v advice=%+v %v", p, p.advice, prob)
		}
	})
	t.Run("an upgrade in a sandbox", func(t *testing.T) {
		fakeNoSupervisor(t)
		env := serviceEnv(t, ok200)
		ourHolder(t, env)
		env.binaryChanges = []string{"cortex"}
		_, prob := serviceStep{}.plan(env)
		want := "a supervised Cortex is running here, and this environment cannot manage " + sup(env)
		// service stop cannot reach the supervisor from here either.
		fix := []string{"run agentop setup from a normal terminal", "or stop it first, from a normal terminal: agentop service stop"}
		if prob == nil || prob.reason != want || !slices.Equal(prob.fix, fix) {
			t.Errorf("sandbox problem = %+v", prob)
		}
	})
	t.Run("an upgrade with --no-service", func(t *testing.T) {
		fakeSupervisor(t)
		env := serviceEnv(t, ok200)
		ourHolder(t, env)
		env.opts.noService, env.binaryChanges = true, []string{"cortex"}
		_, prob := serviceStep{}.plan(env)
		if prob == nil || prob.reason != "a supervised Cortex is running here, and --no-service would start a second copy" {
			t.Errorf("--no-service problem = %+v", prob)
		}
	})
	// The background proxy is cortex --supervise, and its child holds the port: the
	// pidfile names the holder's parent, which is still ours, unsupervised.
	t.Run("our background proxy's child", func(t *testing.T) {
		fakeNoSupervisor(t)
		env := serviceEnv(t, ok200)
		child := exec.Command("/bin/sleep", "30")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
		installStub(t, "lsof", `#!/bin/sh
case "$1" in
  -nP) case "$2" in -iTCP@127.0.0.1:47600) printf 'p`+strconv.Itoa(child.Process.Pid)+`\n' ;; esac ;;
esac
exit 0
`)
		fakeProc(t, child.Process.Pid, filepath.Join(env.binDir, "cortex"))
		if err := os.WriteFile(filepath.Join(env.cortexDir, "proxy.pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		env.binaryChanges = []string{"cortex"}
		p, prob := serviceStep{}.plan(env)
		if prob != nil || p.done || p.advice != nil {
			t.Errorf("our own background proxy read as supervised: %+v advice=%+v %v", p, p.advice, prob)
		}
	})
	// The same for v0.7.0's background proxy: its child runs authbridge-proxy.
	t.Run("a pre-rename background proxy's child", func(t *testing.T) {
		fakeNoSupervisor(t)
		env := serviceEnv(t, ok200)
		child := exec.Command("/bin/sleep", "30")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
		installStub(t, "lsof", `#!/bin/sh
case "$1" in
  -nP) case "$2" in -iTCP@127.0.0.1:47600) printf 'p`+strconv.Itoa(child.Process.Pid)+`\n' ;; esac ;;
esac
exit 0
`)
		fakeProc(t, child.Process.Pid, filepath.Join(env.binDir, "authbridge-proxy"))
		if err := os.WriteFile(filepath.Join(env.cortexDir, "proxy.pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		env.binaryChanges = []string{"cortex"}
		p, prob := serviceStep{}.plan(env)
		if prob != nil || p.done || p.advice != nil {
			t.Errorf("the pre-rename background proxy read as foreign or supervised: %+v advice=%+v %v", p, p.advice, prob)
		}
	})
}

// A sandbox's ps may not name the holder's parent. A background proxy alive in
// proxy.pid is then taken to be that parent; with none alive, the holder runs
// under a supervisor.
func TestSupervisedHolderWhenPsCannotNameTheParent(t *testing.T) {
	installStub(t, "ps", "#!/bin/sh\nexit 1\n")
	pidFile := filepath.Join(t.TempDir(), "proxy.pid")
	for _, tc := range []struct {
		pf   int
		want bool
	}{{os.Getpid(), false}, {999999999, true}} {
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(tc.pf)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := supervisedHolder(pidFile, 31337); got != tc.want {
			t.Errorf("proxy.pid %d alive=%v: supervisedHolder = %v, want %v", tc.pf, alive(tc.pf), got, tc.want)
		}
	}
}

// The consent row names the forward address the config gives, not the default.
func TestServiceStepConsentRowNamesTheConfiguredAddress(t *testing.T) {
	fakeSupervisor(t)
	env := serviceEnv(t, ok200)
	moveForwardPort(t, env, "127.0.0.1:47700")
	p, prob := serviceStep{}.plan(env)
	if want := supervisorName(env.goos) + " · 127.0.0.1:47700"; prob != nil || p.where != want {
		t.Errorf("consent row = %q (%v), want %q", p.where, prob, want)
	}
}

// serviceLoaded asks the supervisor, not the unit file. The fake's is-active and
// is-enabled follow one file, so on linux this loop never reaches is-enabled
// alone; the stubs after it do, for both platforms.
func TestServiceLoadedFollowsTheSupervisor(t *testing.T) {
	loaded := fakeSupervisor(t)
	if serviceLoaded(runtimeGOOS()) {
		t.Error("a job the supervisor does not have read as loaded")
	}
	if err := os.WriteFile(loaded, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !serviceLoaded(runtimeGOOS()) {
		t.Error("a loaded job read as not loaded")
	}
	// Enabled but not active: a unit stopped by hand, or one between restarts.
	installStub(t, "systemctl", "#!/bin/sh\ncase \"$*\" in *is-active*) exit 3 ;; *is-enabled*) exit 0 ;; esac\nexit 1\n")
	if !serviceLoaded("linux") {
		t.Error("an enabled systemd unit read as not loaded")
	}
	// A launchctl that cannot see the domain does not know, so the job may be there.
	installStub(t, "launchctl", "#!/bin/sh\necho 'Domain does not support specified action' >&2\nexit 125\n")
	if !serviceLoaded("darwin") {
		t.Error("a launchctl that could not tell read as the job gone")
	}
}

// A failed start shows what the log gained during it, not an older run's lines:
// from where the log ended before it, the last logTailLines, the first labelled
// proxy.log in the checklist's label column. Nothing gained is a row saying so.
func TestLogTailIsWhatTheStartWrote(t *testing.T) {
	env := newTestSetupEnv(t)
	log := filepath.Join(env.home, "proxy.log")
	nothing := []string{"proxy.log    (it wrote nothing)"}
	if got := logTail(env, log, markLog(log)); !slices.Equal(got, nothing) {
		t.Errorf("no log: %q", got)
	}
	if err := os.WriteFile(log, []byte("old run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := markLog(log)
	if got := logTail(env, log, m); !slices.Equal(got, nothing) {
		t.Errorf("nothing written since the mark: %q", got)
	}
	f, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("one\n\ntwo " + env.home + "/x\nthree\nfour\nfive\nsix\n")
	_ = f.Close()
	want := []string{"proxy.log    two ~/x", "             three", "             four", "             five", "             six"}
	if got := logTail(env, log, m); !slices.Equal(got, want) {
		t.Errorf("tail = %q, want %q", got, want)
	}
	// Rotated: runServiceInstall renames a long log away, so the start writes a new
	// one. Longer than the mark, so only the file's identity says to read it whole.
	if err := os.Rename(log, log+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, []byte("fresh after the rotation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := logTail(env, log, m); !slices.Equal(got, []string{"proxy.log    fresh after the rotation"}) {
		t.Errorf("after a rotation, tail = %q", got)
	}
	// Truncated in place: the same file, so only its size says to read it whole.
	m = markLog(log)
	if err := os.WriteFile(log, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := logTail(env, log, m); !slices.Equal(got, []string{"proxy.log    short"}) {
		t.Errorf("after a truncation, tail = %q", got)
	}
	// A line longer than the scanner's 1 MiB ends the read; the tail says so.
	m = markLog(log)
	if err := os.WriteFile(log, []byte("short\nbefore\n"+strings.Repeat("x", 1<<20+1)+"\nafter\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want = []string{"proxy.log    before", "             (stopped reading: bufio.Scanner: token too long)"}
	if got := logTail(env, log, m); !slices.Equal(got, want) {
		t.Errorf("past a line over 1 MiB, tail = %.300q, want %q", got, want)
	}
}

// An unsupervised proxy that exits at once shows its own log lines too.
func TestServiceStepUnsupervisedFailureShowsTheLog(t *testing.T) {
	fakeNoSupervisor(t)
	env := serviceEnv(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	if _, prob := (serviceStep{}).plan(env); prob != nil || !env.unsupervised {
		t.Fatalf("plan: %v unsupervised=%v", prob, env.unsupervised)
	}
	log := filepath.Join(env.cortexDir, "proxy.log")
	if err := os.WriteFile(log, []byte("an older run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeExe(t, filepath.Join(env.binDir, "cortex"), "#!/bin/sh\necho \"cortex: open $HOME/.cortex/ca/ca.pem: permission denied\" >&2\nexit 1\n")
	_, _, err := serviceStep{}.apply(env, nil)
	var se stepError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v", err)
	}
	want := []string{"see ~/.cortex/proxy.log", "proxy.log    cortex: open ~/.cortex/ca/ca.pem: permission denied"}
	if se.reason != "the proxy exited immediately" || !slices.Equal(se.detail, want) {
		t.Errorf("failure = %q %q, want detail %q", se.reason, se.detail, want)
	}
}

// What service install warns about on a start that works stays under the
// started line: its $HOME warning on macOS, where a test's HOME is not the login
// home, and on Linux its linger caveat.
func TestServiceStepKeepsServiceInstallsWarnings(t *testing.T) {
	fakeSupervisor(t)
	want := "loaded, but lingering could not be turned on: Cortex will stop when you log out."
	if runtimeGOOS() == "darwin" {
		lh := loginHome()
		if lh == "" {
			t.Skip("no login home to tell from HOME")
		}
		want = "$HOME is ~ but your login home is " + lh + ".\nThe unit goes to $HOME/Library/LaunchAgents"
	} else {
		installStub(t, "loginctl", "#!/bin/sh\ncase \"$1\" in show-user) echo Linger=no ;; enable-linger) exit 1 ;; esac\nexit 0\n")
	}
	env := serviceEnv(t, ok200)
	if _, prob := (serviceStep{}).plan(env); prob != nil {
		t.Fatal(prob)
	}
	detail, _, err := serviceStep{}.apply(env, nil)
	first, notes, _ := strings.Cut(detail, "\n")
	if err != nil || !strings.HasSuffix(first, "healthy on 127.0.0.1:47600") || !strings.HasPrefix(notes, want) {
		t.Errorf("apply = %q %v, want the warning %q under the first line", detail, err, want)
	}
}

// notRunning makes fakeSupervisor's job, while loaded, read as not running to
// supervisorRunning: launchctl print of it says "state = not running", and
// systemctl is-active says failed. serviceLoaded's own asks still see it loaded.
// It wraps only the fakes, which name loaded.
func notRunning(t *testing.T, loaded string) {
	t.Helper()
	for _, w := range []struct{ tool, ask, answer string }{
		{"launchctl", "print gui/*/*", "echo 'state = not running'; exit 0"},
		{"systemctl", "--user is-active cortex.service", "echo failed; exit 3"},
	} {
		fake, err := exec.LookPath(w.tool)
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(fake); !strings.Contains(string(b), loaded) { //nolint:gosec
			t.Fatalf("%s on PATH is not fakeSupervisor's", fake)
		}
		installStub(t, w.tool, "#!/bin/sh\ncase \"$*\" in\n  \""+strings.ReplaceAll(w.ask, "*", "\"*\"")+
			"\") if [ -f '"+loaded+"' ]; then "+w.answer+"; fi ;;\nesac\nexec '"+fake+"' \"$@\"\n")
	}
}

// A failed start's detail is service install's own words, less what the rollback
// makes untrue, then the log since the start: not install's tail of the whole log.
func TestServiceStepFailureDetailIsWhatThisStartLeft(t *testing.T) {
	shortReadyTimeout(t)
	t.Run("nothing answers", func(t *testing.T) {
		fakeSupervisor(t)
		env := serviceEnv(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
		if _, prob := (serviceStep{}).plan(env); prob != nil {
			t.Fatal(prob)
		}
		_, _, err := serviceStep{}.apply(env, nil)
		var se stepError
		if !errors.As(err, &se) || !strings.HasPrefix(se.reason, "installed, but nothing answered ") ||
			len(se.detail) == 0 || !strings.HasPrefix(se.detail[0], "Check ~/.cortex/proxy.log — ") {
			t.Fatalf("failure = %+v (%v)", se, err)
		}
		for _, d := range se.detail {
			if strings.Contains(d, "agentop service status") {
				t.Errorf("the detail points at service status, which shows the rolled-back state: %q", se.detail)
			}
		}
	})
	t.Run("the supervisor does not run the job", func(t *testing.T) {
		loaded := fakeSupervisor(t)
		env := serviceEnv(t, ok200)
		if err := os.WriteFile(filepath.Join(env.cortexDir, "proxy.log"), []byte("an older run's line\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		notRunning(t, loaded)
		if _, prob := (serviceStep{}).plan(env); prob != nil {
			t.Fatal(prob)
		}
		_, _, err := serviceStep{}.apply(env, nil)
		var se stepError
		if !errors.As(err, &se) || !strings.HasPrefix(se.reason, "the unit loaded but the supervisor does not report it running") {
			t.Fatalf("failure = %+v (%v)", se, err)
		}
		if got := strings.Join(se.detail, "\n"); strings.Contains(got, "Last log lines:") || strings.Contains(got, "an older run's line") {
			t.Errorf("the detail carries service install's tail of the whole log: %q", se.detail)
		}
		// The fake job writes nothing, so the log rows say just that.
		if n := len(se.detail); n == 0 || se.detail[n-1] != "proxy.log    (it wrote nothing)" {
			t.Errorf("the detail does not end on this start's log rows: %q", se.detail)
		}
	})
}

// A failure before runServiceInstall replaced the job leaves the previous Cortex
// serving, so the rollback has nothing to bring back: a reload would only bounce it.
func TestServiceStepRestoresThePriorOnlyOnceTheJobWasReplaced(t *testing.T) {
	fakeSupervisor(t)
	env := serviceEnv(t, ok200)
	if _, prob := (serviceStep{}).plan(env); prob != nil {
		t.Fatal(prob)
	}
	if _, _, err := (serviceStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	up := *env
	up.configFresh, up.binaryChanges = false, []string{"cortex"}
	if p, prob := (serviceStep{}).plan(&up); prob != nil || p.done || !up.priorService {
		t.Fatalf("upgrade plan = %+v %v prior=%v", p, prob, up.priorService)
	}
	if err := os.Remove(filepath.Join(env.binDir, "cortex")); err != nil { // install refuses before it touches the job
		t.Fatal(err)
	}
	if _, _, err := (serviceStep{}).apply(&up, nil); err == nil || up.restorePrior != nil {
		t.Errorf("apply = %v; restorePrior set %v, want none for a job never replaced", err, up.restorePrior != nil)
	}
}

// The undo after a failed or rolled-back upgrade leaves the job as the upgrade
// found it, and its manual says how: a stopped job is stopped again, and a loaded
// one that was not answering stays loaded.
func TestServiceStepUndoLeavesTheJobAsItFoundIt(t *testing.T) {
	shortReadyTimeout(t)
	for _, tc := range []struct {
		name                   string
		loaded, serving, prior bool
		manual                 string
		loadedAfter            bool
	}{
		{"stopped", false, false, false, "agentop service stop", false},
		{"loaded but not answering", true, false, false, "agentop service install --restart", true},
		{"answering but not loaded", false, true, false, "agentop service stop", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loaded := fakeSupervisor(t)
			var serving atomic.Bool
			serving.Store(true)
			env := serviceEnv(t, func(w http.ResponseWriter, _ *http.Request) {
				if !serving.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			})
			if _, prob := (serviceStep{}).plan(env); prob != nil {
				t.Fatal(prob)
			}
			if _, _, err := (serviceStep{}).apply(env, nil); err != nil {
				t.Fatal(err)
			}
			if !tc.loaded {
				if err := os.Remove(loaded); err != nil {
					t.Fatal(err)
				}
			}
			serving.Store(tc.serving)
			up := *env
			up.configFresh, up.binaryChanges = false, []string{"cortex"}
			if p, prob := (serviceStep{}).plan(&up); prob != nil || p.done || up.priorService != tc.prior {
				t.Fatalf("upgrade plan = %+v %v prior=%v", p, prob, up.priorService)
			}
			_, u, _ := serviceStep{}.apply(&up, nil)
			if u.manual != tc.manual {
				t.Errorf("undo manual = %q, want %q", u.manual, tc.manual)
			}
			if err := u.fn(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(loaded); (err == nil) != tc.loadedAfter {
				t.Errorf("after the undo the job is loaded=%v, want %v", err == nil, tc.loadedAfter)
			}
		})
	}
}

// The background restore refuses while the new job is loaded, whether or not it
// answers: one crash-looping still fights a background copy for the ports.
func TestServiceStepBackgroundRestoreRefusesBesideALoadedUnhealthyJob(t *testing.T) {
	shortReadyTimeout(t)
	loaded := fakeSupervisor(t)
	env := serviceEnv(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	t.Setenv(sleeperEnv, "1") // a restarted copy of the test binary sleeps too
	pidFile := filepath.Join(env.cortexDir, "proxy.pid")
	old := detachedSleeperAt(t, filepath.Join(env.binDir, "cortex"))
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(old)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.binaryChanges = []string{"cortex"}
	if p, prob := (serviceStep{}).plan(env); prob != nil || p.done || env.unsupervised || env.priorService {
		t.Fatalf("plan = %+v %v", p, prob)
	}
	_, u, err := serviceStep{}.apply(env, nil)
	if err == nil || alive(old) || env.restorePrior == nil {
		t.Fatalf("fixture: apply = %v, background proxy alive %v, restorePrior set %v", err, alive(old), env.restorePrior != nil)
	}
	failingUnload(t, loaded)
	if err := u.fn(); err == nil {
		t.Fatal("fixture: the service undo's unload did not fail")
	}
	var second int
	stopProcessOnCleanup(t, &second)
	err = env.restorePrior()
	second = readPIDFile(pidFile)
	if err == nil || !strings.HasPrefix(err.Error(), "the new service is still loaded") || second != 0 {
		t.Errorf("restore = %v, background pid %d: want a refusal and no background copy", err, second)
	}
}

// startByHand's line runs as written: from sh here, and from fish, which has no
// $!, where installed. sh -c expands the ~ in each path, the redirections' too.
func TestStartByHandRunsFromAShell(t *testing.T) {
	env := newTestSetupEnv(t)
	writeExe(t, filepath.Join(env.binDir, "cortex"), "#!/bin/sh\necho \"started $*\"\nexec sleep 30\n")
	if err := os.MkdirAll(env.cortexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	shells := []string{"/bin/sh"}
	if fish, err := exec.LookPath("fish"); err == nil {
		shells = append(shells, fish)
	}
	for _, sh := range shells {
		pidFile := filepath.Join(env.cortexDir, "proxy.pid")
		_ = os.Remove(pidFile)
		cmd := exec.Command(sh, "-c", startByHand(env, filepath.Join(env.binDir, "cortex"))) //nolint:gosec // the line under test
		cmd.Env = append(os.Environ(), "HOME="+env.home)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", sh, err, out)
		}
		var pid int
		for deadline := time.Now().Add(5 * time.Second); pid == 0 && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			pid = readPIDFile(pidFile)
		}
		stopProcessOnCleanup(t, &pid)
		log, _ := os.ReadFile(filepath.Join(env.cortexDir, "proxy.log"))
		for deadline := time.Now().Add(5 * time.Second); !strings.Contains(string(log), "started --local --supervise") && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			log, _ = os.ReadFile(filepath.Join(env.cortexDir, "proxy.log"))
		}
		if !alive(pid) || !strings.Contains(string(log), "started --local --supervise") {
			t.Errorf("%s: proxy.pid %d alive=%v, proxy.log %q", sh, pid, alive(pid), log)
		}
	}
}

// A job that never leaves launchd's domain is the service undo's error once
// serviceBootoutTimeout runs out, naming that bound, so "undone" means gone.
// systemd's disable --now has returned by then, so linux waits for nothing.
func TestWaitUnloadedNamesItsTimeout(t *testing.T) {
	saved := serviceBootoutTimeout
	serviceBootoutTimeout = 500 * time.Millisecond
	t.Cleanup(func() { serviceBootoutTimeout = saved })
	installStub(t, "launchctl", "#!/bin/sh\necho 'state = running'\n") // the job is still there
	began := time.Now()
	err := waitUnloaded("darwin")
	if want := "the launchd user agent is still shutting down after 500ms"; err == nil || err.Error() != want {
		t.Errorf("waitUnloaded = %v, want %q", err, want)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("waited %s, not serviceBootoutTimeout", took)
	}
	if err := waitUnloaded("linux"); err != nil {
		t.Errorf("linux waited: %v", err)
	}
}
