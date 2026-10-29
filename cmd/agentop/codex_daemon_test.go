package main

import (
	"bytes"
	"errors"
	"io"
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

const cortexProxyURL = "http://127.0.0.1:47600" // the forward proxy in cortexCfg

// fakeCodexDaemon is the Codex a test sees: a daemon and, when updaterPID is set, an
// auto-updater, each with a pid file under its CODEX_HOME, a process table that says the
// pid is alive, and an environment. restart, when not failing, replaces the daemon with
// newPID running with newEnv and, as Codex's restart does, starts an updater only when
// none is running — a running one keeps its pid and its old environment. stopUpdater ends
// the updater unless stopErr says it refused.
type fakeCodexDaemon struct {
	pid    int
	env    []string
	envErr error

	newPID     int
	newEnv     []string
	newGone    bool  // the restarted daemon exits straight away
	restartErr error // the restart command fails

	updaterPID    int
	updaterEnv    []string
	newUpdaterPID int   // the updater a restart starts when none is running
	stopErr       error // stopping the updater fails, and it keeps running

	restarts []map[string]string // the want each restart was given
	steps    []string            // "stop-updater" and "restart", in the order they ran
}

// stubCodexDaemon points every daemon seam at f for one test, so nothing a test does can
// restart a real daemon, stop a real updater or read a real process: the pids are fake,
// and a real process with one of them would otherwise be the one inspected.
func stubCodexDaemon(t *testing.T, codexHome string, f *fakeCodexDaemon) {
	t.Helper()
	alive, environ, restart, stop := codexDaemonAlive, codexEnviron, codexRestartDaemon, codexStopUpdater
	t.Cleanup(func() {
		codexDaemonAlive, codexEnviron, codexRestartDaemon, codexStopUpdater = alive, environ, restart, stop
	})
	if f.pid != 0 {
		writeCodexDaemonPID(t, codexHome, f.pid)
	}
	if f.updaterPID != 0 {
		writeCodexPID(t, codexHome, codexUpdaterPIDRel, f.updaterPID)
	}
	codexDaemonAlive = func(pid int) bool { return pid != 0 && (pid == f.pid || pid == f.updaterPID) }
	codexEnviron = func(pid int32) ([]string, error) {
		switch int(pid) {
		case f.pid:
			return f.env, f.envErr
		case f.updaterPID:
			return f.updaterEnv, nil
		}
		t.Errorf("read the environment of pid %d, which is neither fake process", pid)
		return nil, errors.New("not a fake process")
	}
	codexStopUpdater = func(home string) error {
		if home != codexHome {
			t.Errorf("stopped the updater of %s, want %s", home, codexHome)
		}
		f.steps = append(f.steps, "stop-updater")
		if f.stopErr != nil {
			return f.stopErr
		}
		f.updaterPID = 0
		return nil
	}
	codexRestartDaemon = func(home string, want map[string]string) error {
		if home != codexHome {
			t.Errorf("restarted the daemon of %s, want %s", home, codexHome)
		}
		f.steps = append(f.steps, "restart")
		f.restarts = append(f.restarts, want)
		if f.restartErr != nil {
			return f.restartErr
		}
		f.pid, f.env, f.envErr = f.newPID, f.newEnv, nil
		if f.newGone {
			f.pid = 0
		} else {
			writeCodexDaemonPID(t, codexHome, f.newPID)
		}
		if f.updaterPID == 0 && f.newUpdaterPID != 0 {
			// A fresh updater reads the file the change just wrote, as the daemon does.
			f.updaterPID, f.updaterEnv = f.newUpdaterPID, f.newEnv
			writeCodexPID(t, codexHome, codexUpdaterPIDRel, f.newUpdaterPID)
		}
		return nil
	}
}

// writeCodexDaemonPID writes a daemon.pid under codexHome as Codex does.
func writeCodexDaemonPID(t *testing.T, codexHome string, pid int) {
	t.Helper()
	writeCodexPID(t, codexHome, codexDaemonPIDRel, pid)
}

// writeCodexPID writes one of Codex's pid files, rel under codexHome.
func writeCodexPID(t *testing.T, codexHome, rel string, pid int) {
	t.Helper()
	dir := filepath.Join(codexHome, filepath.Dir(rel))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"pid":` + strconv.Itoa(pid) + `,"processStartTime":"Fri Oct 23 09:03:25 2026"}`
	if err := os.WriteFile(filepath.Join(codexHome, rel), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// onCortex is a daemon environment routed through Cortex, as enable's file makes it.
var onCortex = []string{"HOME=/home/someone", "HTTPS_PROXY=" + cortexProxyURL, "https_proxy=" + cortexProxyURL}

// offCortex is one with no proxy at all.
var offCortex = []string{"HOME=/home/someone"}

func enableCodex(t *testing.T, envPath, cfgPath string) {
	t.Helper()
	var out, errb bytes.Buffer
	if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
		t.Fatalf("enable exit %d: %s", code, errb.String())
	}
}

func wantAll(t *testing.T, what, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("%s lacks %q:\n%s", what, w, got)
		}
	}
}

// Enable with a daemon running restarts it, once and after the file is written, then
// confirms the new daemon uses Cortex. This is the case that used to look enabled and was
// not: the old daemon kept sending every prompt straight to OpenAI.
func TestCodexEnable_RestartsARunningDaemon(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	f := &fakeCodexDaemon{pid: 5613, env: offCortex, newPID: 7001, newEnv: onCortex}
	stubCodexDaemon(t, filepath.Dir(envPath), f)
	var wroteFirst bool
	prev := codexRestartDaemon
	codexRestartDaemon = func(home string, want map[string]string) error {
		_, err := os.Stat(envPath)
		wroteFirst = err == nil
		return prev(home, want)
	}

	var out, errb bytes.Buffer
	if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
		t.Fatalf("enable exit %d: %s", code, errb.String())
	}
	got := out.String()
	wantAll(t, "enable output", got, "pid 5613", "Fri Oct 23 09:03:25 2026", "Applying this restarts it",
		"up to its shutdown grace", "a minute by default", "open Codex windows reconnect",
		"restarted (pid 7001) and is using Cortex")
	if len(f.restarts) != 1 {
		t.Fatalf("restarted %d times, want once", len(f.restarts))
	}
	if !wroteFirst {
		t.Error("restarted before the file was written, so the new daemon read the old one")
	}
	if f.restarts[0]["HTTPS_PROXY"] != cortexProxyURL {
		t.Errorf("restart was given want %v, which does not name Cortex's proxy", f.restarts[0])
	}
	// The warning is said before the question, so it can be declined.
	if strings.Index(got, "Applying this restarts it") > strings.Index(got, "Enabled —") {
		t.Errorf("the restart is announced after the change:\n%s", got)
	}
}

// Disable stops a running auto-updater before restarting, so the restart starts a fresh
// one that read the new file. Without that the old updater kept Cortex's proxy, and the
// daemon it started after the next update was back on Cortex (#1526 review, reproduced
// on Codex 0.164.0 → 0.165.0).
func TestCodexDisable_StopsTheUpdaterBeforeRestarting(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	enableCodex(t, envPath, cfgPath)
	f := &fakeCodexDaemon{
		pid: 5613, env: onCortex, newPID: 7001, newEnv: offCortex,
		updaterPID: 5600, updaterEnv: onCortex, newUpdaterPID: 7002,
	}
	stubCodexDaemon(t, filepath.Dir(envPath), f)

	var out, errb bytes.Buffer
	if code := codexDisable(envPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
		t.Fatalf("disable exit %d: %s", code, errb.String())
	}
	if got := strings.Join(f.steps, ","); got != "stop-updater,restart" {
		t.Errorf("steps = %s, want the updater stopped before the restart", got)
	}
	if f.updaterPID != 7002 {
		t.Errorf("updater pid = %d, want the fresh one the restart started", f.updaterPID)
	}
	if strings.Contains(out.String(), "auto-updater") {
		t.Errorf("named the fresh updater, which agrees with the change:\n%s", out.String())
	}
}

// An updater that refuses to stop keeps its old environment through the restart, and is
// named with the command that fixes it rather than reported as fixed.
func TestCodexDisable_NamesAnUpdaterThatKeptCortex(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	enableCodex(t, envPath, cfgPath)
	f := &fakeCodexDaemon{
		pid: 5613, env: onCortex, newPID: 7001, newEnv: offCortex,
		updaterPID: 5600, updaterEnv: onCortex, stopErr: errors.New("Codex's updater (pid 5600) did not exit within 10s"),
	}
	stubCodexDaemon(t, filepath.Dir(envPath), f)

	var out, errb bytes.Buffer
	if code := codexDisable(envPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
		t.Fatalf("disable exit %d: %s", code, errb.String())
	}
	wantAll(t, "stderr", errb.String(), "did not exit")
	wantAll(t, "disable output", out.String(), "no longer uses Cortex",
		"auto-updater (pid 5600) still uses Cortex's proxy", "kill 5600 && "+codexDaemonRestartCmd)
}

// Enable stops the updater too, so the two processes never disagree, and an install with
// no updater (auto-update off) stops nothing and names nothing.
func TestCodexEnable_UpdaterHandling(t *testing.T) {
	t.Run("running updater is replaced", func(t *testing.T) {
		envPath, cfgPath := codexFixture(t, "")
		f := &fakeCodexDaemon{
			pid: 5613, env: offCortex, newPID: 7001, newEnv: onCortex,
			updaterPID: 5600, updaterEnv: offCortex, newUpdaterPID: 7002,
		}
		stubCodexDaemon(t, filepath.Dir(envPath), f)
		var out, errb bytes.Buffer
		if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
			t.Fatalf("enable exit %d: %s", code, errb.String())
		}
		if got := strings.Join(f.steps, ","); got != "stop-updater,restart" || f.updaterPID != 7002 {
			t.Errorf("steps = %s, updater = %d; want stop-updater,restart and the fresh updater", got, f.updaterPID)
		}
	})
	t.Run("no updater", func(t *testing.T) {
		envPath, cfgPath := codexFixture(t, "")
		f := &fakeCodexDaemon{pid: 5613, env: offCortex, newPID: 7001, newEnv: onCortex}
		stubCodexDaemon(t, filepath.Dir(envPath), f)
		var out, errb bytes.Buffer
		if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
			t.Fatalf("enable exit %d: %s", code, errb.String())
		}
		if strings.Contains(out.String(), "auto-updater") || errb.Len() != 0 {
			t.Errorf("spoke of an updater that is not running:\nstdout %s\nstderr %s", out.String(), errb.String())
		}
	})
}

// Declining changes nothing, so nothing is restarted, though the warning was shown.
func TestCodexEnable_DeclinedRestartsNothing(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	f := &fakeCodexDaemon{pid: 5613, env: offCortex, newPID: 7001, newEnv: onCortex}
	stubCodexDaemon(t, filepath.Dir(envPath), f)
	prev := codexConfirm
	t.Cleanup(func() { codexConfirm = prev })
	codexConfirm = func(io.Writer) bool { return false }

	var out, errb bytes.Buffer
	if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), false, &out, &errb); code != exitDeclined {
		t.Fatalf("exit %d, want %d", code, exitDeclined)
	}
	wantAll(t, "declined enable output", out.String(), "Applying this restarts it", "Not changed.")
	if len(f.restarts) != 0 {
		t.Errorf("a declined enable restarted the daemon %d time(s)", len(f.restarts))
	}
}

// A restarted daemon that still has no Cortex proxy is named, not reported as fixed:
// something other than the file, such as the environment it was started from, wins.
func TestCodexEnable_SaysWhenTheRestartedDaemonStillBypassesCortex(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	f := &fakeCodexDaemon{pid: 5613, env: offCortex, newPID: 7001, newEnv: offCortex}
	stubCodexDaemon(t, filepath.Dir(envPath), f)

	var out, errb bytes.Buffer
	if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
		t.Fatalf("enable exit %d: %s", code, errb.String())
	}
	wantAll(t, "enable output", out.String(), "restarted (pid 7001), but its proxy is none, not Cortex's")
}

// A restart that fails leaves the change made, so enable still exits 0, and stderr names
// the command that finishes the job.
func TestCodexEnable_RestartFailureIsReported(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	f := &fakeCodexDaemon{pid: 5613, env: offCortex, restartErr: errors.New("codex app-server daemon restart: exit status 1: busy")}
	stubCodexDaemon(t, filepath.Dir(envPath), f)

	var out, errb bytes.Buffer
	if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
		t.Fatalf("enable exit %d, want 0: the file was changed", code)
	}
	wantAll(t, "stderr", errb.String(), "could not restart", "busy", codexDaemonRestartCmd)
	if readCodexEnv(t, envPath)["HTTPS_PROXY"] != cortexProxyURL {
		t.Error("the file was not enabled")
	}
}

// A restarted daemon that exits, or whose environment cannot be read, is said as such
// rather than claimed to be on Cortex.
func TestCodexEnable_RestartedDaemonUnchecked(t *testing.T) {
	for name, tc := range map[string]struct {
		f    fakeCodexDaemon
		want string
	}{
		"gone":       {fakeCodexDaemon{pid: 5613, env: offCortex, newGone: true}, "restarted, but is not running now"},
		"unreadable": {fakeCodexDaemon{pid: 5613, env: offCortex, newPID: 7001}, "restarted (pid 7001); its environment could not be checked"},
	} {
		t.Run(name, func(t *testing.T) {
			envPath, cfgPath := codexFixture(t, "")
			f := tc.f
			stubCodexDaemon(t, filepath.Dir(envPath), &f)
			var out, errb bytes.Buffer
			if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
				t.Fatalf("enable exit %d: %s", code, errb.String())
			}
			wantAll(t, "enable output", out.String(), tc.want)
		})
	}
}

// Disable restarts the daemon too, the other way round: a daemon left on Cortex would
// stop working once Cortex stops. Cortex's config is not read, so the restart is given no
// want, and the proxy it is compared with is the one the file held.
func TestCodexDisable_RestartsARunningDaemon(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	enableCodex(t, envPath, cfgPath)
	f := &fakeCodexDaemon{pid: 5613, env: onCortex, newPID: 7001, newEnv: offCortex}
	stubCodexDaemon(t, filepath.Dir(envPath), f)

	var out, errb bytes.Buffer
	if code := codexDisable(envPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
		t.Fatalf("disable exit %d: %s", code, errb.String())
	}
	wantAll(t, "disable output", out.String(), "pid 5613", "Applying this restarts it", "restarted (pid 7001) and no longer uses Cortex")
	if len(f.restarts) != 1 || f.restarts[0] != nil {
		t.Errorf("restarts = %v, want one, given no want", f.restarts)
	}
}

// A daemon still on Cortex's proxy after disable got it from somewhere other than the
// file, and is told so.
func TestCodexDisable_SaysWhenTheRestartedDaemonStillUsesCortex(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	enableCodex(t, envPath, cfgPath)
	f := &fakeCodexDaemon{pid: 5613, env: onCortex, newPID: 7001, newEnv: onCortex}
	stubCodexDaemon(t, filepath.Dir(envPath), f)

	var out, errb bytes.Buffer
	if code := codexDisable(envPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
		t.Fatalf("disable exit %d: %s", code, errb.String())
	}
	wantAll(t, "disable output", out.String(), "still uses Cortex's proxy")
}

// Running enable again is what someone does when Codex does not seem to go through
// Cortex, and a daemon older than the file is the usual reason. With nothing to change it
// reports rather than restarts, and names the command.
func TestCodexEnable_AlreadyEnabledReportsTheDaemon(t *testing.T) {
	for name, tc := range map[string]struct {
		env  []string
		want string
	}{
		"old settings": {offCortex, "running with its old settings"},
		"on Cortex":    {onCortex, "(pid 5613) is using Cortex"},
	} {
		t.Run(name, func(t *testing.T) {
			envPath, cfgPath := codexFixture(t, "")
			enableCodex(t, envPath, cfgPath)
			f := &fakeCodexDaemon{pid: 5613, env: tc.env}
			stubCodexDaemon(t, filepath.Dir(envPath), f)

			var out, errb bytes.Buffer
			if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
				t.Fatalf("second enable exit %d: %s", code, errb.String())
			}
			wantAll(t, "second enable output", out.String(), "Already enabled", tc.want)
			if len(f.restarts) != 0 {
				t.Errorf("an enable with nothing to change restarted the daemon")
			}
		})
	}
}

// status says what the running daemon uses, since that, not the file, is what sends
// Codex's prompts.
func TestCodexStatus_ReportsTheDaemon(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	enableCodex(t, envPath, cfgPath)
	stubCodexDaemon(t, filepath.Dir(envPath), &fakeCodexDaemon{pid: 5613, env: offCortex})

	var out bytes.Buffer
	if code := codexStatus(envPath, cfgPath, &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	wantAll(t, "status output", out.String(), "enabled in "+envPath, "running with its old settings", codexDaemonRestartCmd)
}

// No daemon, or one whose pid file outlived it, needs nothing: the next codex starts a
// fresh daemon that reads the file enable just wrote.
func TestCodexEnable_NothingToRestartWithoutALiveDaemon(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, codexHome string){
		"no pid file": func(*testing.T, string) {},
		"stale pid file": func(t *testing.T, codexHome string) {
			writeCodexDaemonPID(t, codexHome, 5613)
		},
		"unreadable pid file": func(t *testing.T, codexHome string) {
			dir := filepath.Join(codexHome, filepath.Dir(codexDaemonPIDRel))
			_ = os.MkdirAll(dir, 0o700)
			_ = os.WriteFile(filepath.Join(codexHome, codexDaemonPIDRel), []byte("not json"), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			envPath, cfgPath := codexFixture(t, "")
			f := &fakeCodexDaemon{} // pid 0: nothing is alive
			stubCodexDaemon(t, filepath.Dir(envPath), f)
			setup(t, filepath.Dir(envPath))

			var out, errb bytes.Buffer
			if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
				t.Fatalf("enable exit %d: %s", code, errb.String())
			}
			if strings.Contains(out.String(), "app-server") {
				t.Errorf("spoke of a daemon that is not running:\n%s", out.String())
			}
			if len(f.restarts) != 0 {
				t.Error("restarted a daemon that is not running")
			}
		})
	}
}

// Codex's own reading of the proxy variables, which is not Bun's: uppercase first, an
// empty value is unset, and ALL_PROXY is the fallback.
func TestCodexEnvironProxy(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"none", []string{"HOME=/h"}, ""},
		{"uppercase wins", []string{"https_proxy=http://lower:1", "HTTPS_PROXY=http://upper:2"}, "http://upper:2"},
		{"lowercase alone", []string{"https_proxy=http://lower:1"}, "http://lower:1"},
		{"empty uppercase hides lowercase", []string{"HTTPS_PROXY=", "https_proxy=http://lower:1"}, ""},
		{"ALL_PROXY fallback", []string{"ALL_PROXY=http://all:3"}, "http://all:3"},
		{"HTTP_PROXY is not for HTTPS", []string{"HTTP_PROXY=http://plain:4"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexEnvironProxy(tc.env); got != tc.want {
				t.Errorf("codexEnvironProxy(%q) = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}

// The restart's environment loses Cortex's values and keeps everything else, a proxy of
// the user's own included, so a restart after disable cannot put the daemon back on
// Cortex from the shell it ran in.
func TestCodexCLIEnv(t *testing.T) {
	env := []string{"HOME=/h", "HTTPS_PROXY=" + cortexProxyURL, "http_proxy=http://corp.example.com:3128", "SSL_CERT_FILE=/h/.cortex/ca/bundle.crt"}
	got := strings.Join(codexCLIEnv(env, nil), " ")
	if want := "HOME=/h http_proxy=http://corp.example.com:3128"; got != want {
		t.Errorf("codexCLIEnv = %q, want %q", got, want)
	}
}

// The real runner, given /bin/sh rather than codex: a failure carries stderr's first
// line, the child gets no Cortex proxy, and CODEX_HOME names a home Codex would not find
// by itself.
func TestCodexRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv(codexHomeEnv, "")
	t.Setenv("HTTPS_PROXY", cortexProxyURL)
	home := t.TempDir()
	err := codexRun("/bin/sh", home, nil, "-c", `echo "home=$CODEX_HOME proxy=$HTTPS_PROXY" >&2; echo second >&2; exit 3`)
	if err == nil {
		t.Fatal("a non-zero exit returned no error")
	}
	if msg := err.Error(); !strings.HasSuffix(msg, ": exit status 3: home="+home+" proxy=") {
		t.Errorf("err = %q, want it to end with the exit status, CODEX_HOME and no proxy", msg)
	}
	if err := codexRun("/bin/sh", home, nil, "-c", "exit 0"); err != nil {
		t.Errorf("a zero exit returned %v", err)
	}
	// A restart leaves a daemon running, and a daemon that kept the command's stdout or
	// stderr open must not turn a successful restart into a failure (#1526 review). The
	// background sleep holds both for longer than WaitDelay.
	if err := codexRun("/bin/sh", home, nil, "-c", "sleep 5 & exit 0"); err != nil {
		t.Errorf("a successful command whose child kept its output open returned %v", err)
	}
	// The same child behind a failed command is still a failure.
	if err := codexRun("/bin/sh", home, nil, "-c", "sleep 5 & exit 4"); err == nil || !strings.Contains(err.Error(), "exit status 4") {
		t.Errorf("a failed command whose child kept its output open returned %v, want exit status 4", err)
	}
}

// The real updater stop, against a process this test starts as its own process-group
// leader, as Codex starts its updater: SIGTERM to the group, then a wait for it to exit.
// An updater that is not running, or a stale pid file, needs nothing.
func TestCodexStopUpdater_RealProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups")
	}
	home := t.TempDir()
	if err := codexStopUpdater(home); err != nil {
		t.Errorf("no pid file: %v", err)
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-exited })
	writeCodexPID(t, home, codexUpdaterPIDRel, cmd.Process.Pid)

	// codexDaemonAlive counts a zombie as alive, and only Wait reaps this child, so
	// liveness here is whether Wait has returned.
	prev := codexDaemonAlive
	t.Cleanup(func() { codexDaemonAlive = prev })
	codexDaemonAlive = func(pid int) bool {
		if pid != cmd.Process.Pid {
			return false
		}
		select {
		case <-exited:
			return false
		default:
			return true
		}
	}
	start := time.Now()
	if err := codexStopUpdater(home); err != nil {
		t.Fatalf("stopping a live updater: %v", err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("codexStopUpdater returned with the updater still running")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("stop took %s; SIGTERM should end sleep at once", d)
	}
	if err := codexStopUpdater(home); err != nil {
		t.Errorf("stale pid file: %v", err)
	}
}

// codex is found on PATH, else where its standalone installer puts it.
func TestFindCodex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	t.Setenv("PATH", t.TempDir())
	home := t.TempDir()
	if _, err := findCodex(home); err == nil {
		t.Error("found a codex that is not there")
	}
	bin := filepath.Join(home, "packages", "standalone", "current", "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := findCodex(home); err != nil || got != bin {
		t.Errorf("findCodex = %q, %v; want %q", got, err, bin)
	}
}

// The liveness check against the real process table: this process is alive, a pid no
// process can have is not.
func TestCodexDaemonAlive_RealProcesses(t *testing.T) {
	if !codexDaemonAlive(os.Getpid()) {
		t.Error("this test's own process reads as not running")
	}
	if codexDaemonAlive(0) || codexDaemonAlive(-1) {
		t.Error("a non-positive pid reads as running")
	}
}
