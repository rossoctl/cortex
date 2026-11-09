package main

import (
	"bytes"
	"errors"
	"fmt"
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

	"github.com/rossoctl/cortex/core/peerproc"
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

	updaterPID     int
	updaterEnv     []string
	newUpdaterPID  int   // the updater a restart starts when none is running
	stopErr        error // stopping the updater fails, and it keeps running
	updaterReused  bool  // the updater's pid now names another process, so its record is stale
	updaterUnknown bool  // agentop cannot tell whether the updater's pid is the recorded process

	restarts []map[string]string // the want each restart was given
	steps    []string            // "stop-updater" and "restart", in the order they ran
}

// stubCodexDaemon points every daemon seam at f for one test, so nothing a test does can
// restart a real daemon, stop a real updater or read a real process: the pids are fake,
// and a real process with one of them would otherwise be the one inspected.
func stubCodexDaemon(t *testing.T, codexHome string, f *fakeCodexDaemon) {
	t.Helper()
	alive, environ, restart, stop, started := codexDaemonAlive, codexEnviron, codexRestartDaemon, codexStopUpdater, codexProcStarted
	t.Cleanup(func() {
		codexDaemonAlive, codexEnviron, codexRestartDaemon, codexStopUpdater, codexProcStarted = alive, environ, restart, stop, started
	})
	// The fake pids name no real process, so nothing could vouch for them: the pid files
	// these tests write carry no processIdentity, so codexIsRecorded asks
	// codexProcStarted, and each fake process is the one its file recorded unless the
	// test says otherwise. A reused pid and an unknown one both fail that legacy check, so
	// a reused one is told apart by its identity in the tests that need it.
	codexProcStarted = func(pid int, _ string) bool {
		if pid == f.updaterPID {
			return !f.updaterReused && !f.updaterUnknown
		}
		return pid != 0 && pid == f.pid
	}
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
	writeCodexPIDStarted(t, codexHome, rel, pid, "Fri Oct 23 09:03:25 2026")
}

// writeCodexPIDStarted is writeCodexPID with the processStartTime given; "" leaves the
// field out, as a record written by a version that did not keep it.
func writeCodexPIDStarted(t *testing.T, codexHome, rel string, pid int, started string) {
	t.Helper()
	dir := filepath.Join(codexHome, filepath.Dir(rel))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"pid":` + strconv.Itoa(pid) + `}`
	if started != "" {
		body = `{"pid":` + strconv.Itoa(pid) + `,"processStartTime":"` + started + `"}`
	}
	if err := os.WriteFile(filepath.Join(codexHome, rel), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeCodexPIDIdentity writes one of Codex's pid files with a processIdentity, the JSON
// object given, and no processStartTime.
func writeCodexPIDIdentity(t *testing.T, codexHome, rel string, pid int, identity string) {
	t.Helper()
	dir := filepath.Join(codexHome, filepath.Dir(rel))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"pid":` + strconv.Itoa(pid) + `,"processIdentity":` + identity + `}`
	if err := os.WriteFile(filepath.Join(codexHome, rel), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// stubProcessIdentity answers codexProcessIdentity from ids; a pid not in it is not found.
func stubProcessIdentity(t *testing.T, ids map[int32]peerproc.Identity) {
	t.Helper()
	prev := codexProcessIdentity
	t.Cleanup(func() { codexProcessIdentity = prev })
	codexProcessIdentity = func(pid int32) (peerproc.Identity, error) {
		if id, ok := ids[pid]; ok {
			return id, nil
		}
		return peerproc.Identity{}, peerproc.ErrNotFound
	}
}

// identityJSON is id as Codex writes it in a pid file's processIdentity, in this
// platform's shape.
func identityJSON(t *testing.T, id peerproc.Identity) string {
	t.Helper()
	switch runtime.GOOS {
	case "darwin":
		return fmt.Sprintf(`{"bootId":%q,"uniqueId":%d,"startSeconds":%d,"startMicroseconds":%d}`,
			id.BootID, id.UniqueID, id.StartSeconds, id.StartMicroseconds)
	case "linux":
		return fmt.Sprintf(`{"bootId":%q,"startTicks":%d}`, id.BootID, id.StartTicks)
	}
	t.Skip("no processIdentity on " + runtime.GOOS)
	return ""
}

// psStarted is what Codex records as a process's processStartTime: ps's lstart, trimmed.
func psStarted(t *testing.T, pid int) string {
	t.Helper()
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Skipf("ps cannot read pid %d's start time here: %v", pid, err)
	}
	return strings.TrimSpace(string(out))
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

// An updater pid that agentop cannot confirm is never named after the restart (#1736
// review): naming it printed a kill command for what may be an unrelated process. With a
// record of another process the restart has replaced the updater, so nothing is said;
// when agentop cannot tell, it says only that, and what to do.
func TestCodexEnable_ReportsOnlyAConfirmedUpdater(t *testing.T) {
	for _, tc := range []struct {
		name            string
		reused, unknown bool
		want            string // "" for no line about the updater at all
	}{
		{"another process", true, false, ""},
		{"cannot tell", false, true, "Could not confirm that Codex's auto-updater was restarted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envPath, cfgPath := codexFixture(t, "")
			f := &fakeCodexDaemon{
				pid: 5613, env: offCortex, newPID: 7001, newEnv: onCortex,
				updaterPID: 5600, updaterEnv: offCortex, updaterReused: tc.reused, updaterUnknown: tc.unknown,
			}
			stubCodexDaemon(t, filepath.Dir(envPath), f)
			// Stands in for the real codexStopUpdater, which leaves an unconfirmed pid alone.
			codexStopUpdater = func(string) error {
				f.steps = append(f.steps, "stop-updater")
				return nil
			}
			if tc.reused {
				// The record's identity says another process: Codex's native check.
				writeCodexPIDIdentity(t, filepath.Dir(envPath), codexUpdaterPIDRel, 5600, `{"bootId":"old-boot","startTicks":1}`)
				stubProcessIdentity(t, map[int32]peerproc.Identity{5600: {BootID: "this-boot", StartTicks: 2}})
			}
			var out, errb bytes.Buffer
			if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
				t.Fatalf("enable exit %d: %s", code, errb.String())
			}
			got := out.String()
			if strings.Contains(got, "kill 5600") || strings.Contains(got, "auto-updater (pid 5600)") {
				t.Errorf("named an unconfirmed pid as Codex's updater:\n%s", got)
			}
			if tc.want == "" {
				if strings.Contains(got, "auto-updater") {
					t.Errorf("spoke of an updater the restart has replaced:\n%s", got)
				}
				return
			}
			wantAll(t, "enable output", got, tc.want, "run this again")
		})
	}
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

// startGroupLeader starts a process as its own process-group leader, as Codex starts its
// updater, and points codexDaemonAlive at it: codexDaemonAlive counts a zombie as alive,
// and only Wait reaps this child, so liveness here is whether Wait has returned.
func startGroupLeader(t *testing.T) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-exited })
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
	return cmd, exited
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
	for _, record := range []string{"processIdentity", "processStartTime"} {
		t.Run(record, func(t *testing.T) {
			cmd, exited := startGroupLeader(t)
			pid := cmd.Process.Pid
			if record == "processIdentity" {
				id, err := peerproc.ProcessIdentity(int32(pid))
				if err != nil {
					t.Skipf("cannot read pid %d's identity here: %v", pid, err)
				}
				writeCodexPIDIdentity(t, home, codexUpdaterPIDRel, pid, identityJSON(t, id))
			} else {
				writeCodexPIDStarted(t, home, codexUpdaterPIDRel, pid, psStarted(t, pid))
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
		})
	}
}

// A pid file whose pid now belongs to another process is not that process's to stop
// (#1526 review). The process here leads its own group, as a shell does, which is what
// makes signalling it costly: kill(-pid) would end the whole group. It must be left
// running whichever way the record says so — another boot or start in its
// processIdentity, another start time in a legacy record — and when the record carries
// nothing to check.
func TestCodexStopUpdater_LeavesAReusedPIDAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups")
	}
	for _, tc := range []struct {
		name, started string
		identity      func(peerproc.Identity) peerproc.Identity // nil for a legacy record
	}{
		{"the identity names another boot", "", func(id peerproc.Identity) peerproc.Identity { id.BootID = "another-boot"; return id }},
		{"the identity names another start", "", func(id peerproc.Identity) peerproc.Identity {
			id.StartTicks++
			id.StartMicroseconds++
			return id
		}},
		{"the record names another start time", "Fri Oct 23 09:03:25 2026", nil},
		{"the record has no start time", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			cmd, exited := startGroupLeader(t)
			pid := cmd.Process.Pid
			switch {
			case tc.identity != nil:
				id, err := peerproc.ProcessIdentity(int32(pid))
				if err != nil {
					t.Skipf("cannot read pid %d's identity here: %v", pid, err)
				}
				writeCodexPIDIdentity(t, home, codexUpdaterPIDRel, pid, identityJSON(t, tc.identity(id)))
			case tc.started != "" && tc.started == psStarted(t, pid):
				t.Skip("the fixed start time happens to be this process's")
			default:
				writeCodexPIDStarted(t, home, codexUpdaterPIDRel, pid, tc.started)
			}
			if err := codexStopUpdater(home); err != nil {
				t.Errorf("a pid that is not the updater: %v", err)
			}
			select {
			case <-exited:
				t.Fatal("signalled a process the pid file does not describe")
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
}

// codexIdentityMatches compares a recorded processIdentity field for field, in each of
// Codex's three shapes, and is unknown — never a match — when it cannot read the process
// or does not know the shape.
func TestCodexIdentityMatches(t *testing.T) {
	u := func(v uint64) *uint64 { return &v }
	str := func(v string) *string { return &v }
	mac := peerproc.Identity{BootID: "B", UniqueID: 7, StartSeconds: 100, StartMicroseconds: 5}
	linux := peerproc.Identity{BootID: "B", StartTicks: 900}
	macRec := codexProcIdentity{BootID: str("B"), UniqueID: u(7), StartSeconds: u(100), StartMicroseconds: u(5)}
	linuxRec := codexProcIdentity{BootID: str("B"), StartTicks: u(900)}
	with := func(r codexProcIdentity, f func(*codexProcIdentity)) codexProcIdentity { f(&r); return r }
	for _, tc := range []struct {
		name string
		live peerproc.Identity
		err  error
		rec  codexProcIdentity
		want codexRecordMatch
	}{
		{"macOS, the same process", mac, nil, macRec, codexRecordMatches},
		{"macOS, another boot", mac, nil, with(macRec, func(r *codexProcIdentity) { r.BootID = str("A") }), codexRecordStale},
		{"macOS, another unique id", mac, nil, with(macRec, func(r *codexProcIdentity) { r.UniqueID = u(8) }), codexRecordStale},
		{"macOS, another start", mac, nil, with(macRec, func(r *codexProcIdentity) { r.StartMicroseconds = u(6) }), codexRecordStale},
		{"macOS, the older shape", mac, nil, codexProcIdentity{StartSeconds: u(100), StartMicroseconds: u(5)}, codexRecordMatches},
		{"Linux, the same process", linux, nil, linuxRec, codexRecordMatches},
		{"Linux, another boot", linux, nil, with(linuxRec, func(r *codexProcIdentity) { r.BootID = str("A") }), codexRecordStale},
		{"Linux, another start", linux, nil, with(linuxRec, func(r *codexProcIdentity) { r.StartTicks = u(901) }), codexRecordStale},
		{"a zombie", peerproc.Identity{BootID: "B", StartTicks: 900, Zombie: true}, nil, linuxRec, codexRecordStale},
		{"another user's process, another unique id", peerproc.Identity{BootID: "B", UniqueID: 8}, peerproc.ErrNotFound, macRec, codexRecordStale},
		{"another user's process, the same unique id", peerproc.Identity{BootID: "B", UniqueID: 7}, peerproc.ErrNotFound, macRec, codexRecordUnknown},
		{"unreadable", peerproc.Identity{}, peerproc.ErrNotFound, macRec, codexRecordUnknown},
		{"a shape this agentop does not know", mac, nil, codexProcIdentity{BootID: str("B")}, codexRecordUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := codexProcessIdentity
			t.Cleanup(func() { codexProcessIdentity = prev })
			codexProcessIdentity = func(int32) (peerproc.Identity, error) { return tc.live, tc.err }
			if got := codexIdentityMatches(42, tc.rec); got != tc.want {
				t.Errorf("= %d, want %d", got, tc.want)
			}
		})
	}
}

// The start-time check against the real process table: this process matches the start
// time ps reports for it, and nothing else does.
func TestCodexProcStarted_RealProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ps")
	}
	pid := os.Getpid()
	started := psStarted(t, pid)
	if !codexProcStarted(pid, started) {
		t.Errorf("this process's own start time %q reads as a mismatch", started)
	}
	if !codexProcStarted(pid, "  "+started+"\n") {
		t.Error("surrounding whitespace in the record reads as a mismatch")
	}
	if codexProcStarted(pid, "Fri Oct 23 09:03:25 2026") && started != "Fri Oct 23 09:03:25 2026" {
		t.Error("another start time reads as a match")
	}
	if codexProcStarted(pid, "") || codexProcStarted(0, started) || codexProcStarted(-1, started) {
		t.Error("an empty record or a non-positive pid reads as a match")
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
