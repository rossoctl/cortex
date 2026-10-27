package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeCodexDaemonPID writes a daemon.pid under codexHome as Codex does.
func writeCodexDaemonPID(t *testing.T, codexHome string, pid int) {
	t.Helper()
	dir := filepath.Join(codexHome, filepath.Dir(codexDaemonPIDRel))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"pid":` + strconv.Itoa(pid) + `,"processStartTime":"Fri Oct 23 09:03:25 2026"}`
	if err := os.WriteFile(filepath.Join(codexHome, codexDaemonPIDRel), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// withCodexDaemonAlive makes codexDaemonAlive answer for the test, so no real process
// is needed or touched.
func withCodexDaemonAlive(t *testing.T, alive func(int) bool) {
	t.Helper()
	prev := codexDaemonAlive
	codexDaemonAlive = alive
	t.Cleanup(func() { codexDaemonAlive = prev })
}

// Enable with Codex's daemon running says the daemon still calls OpenAI directly, and
// names the command that fixes it. This is the case that looked enabled and was not:
// every prompt bypassed Cortex.
func TestCodexEnable_WarnsAboutARunningDaemon(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	writeCodexDaemonPID(t, filepath.Dir(envPath), 5613)
	withCodexDaemonAlive(t, func(pid int) bool { return pid == 5613 })

	var out, errb bytes.Buffer
	if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
		t.Fatalf("enable exit %d: %s", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{"pid 5613", "Fri Oct 23 09:03:25 2026", "go straight to OpenAI", codexDaemonRestartCmd} {
		if !strings.Contains(got, want) {
			t.Errorf("enable output lacks %q:\n%s", want, got)
		}
	}
}

// Disable with the daemon running says the opposite: it still sends through Cortex,
// and breaks if Cortex stops.
func TestCodexDisable_WarnsAboutARunningDaemon(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	statePath := filepath.Join(filepath.Dir(envPath), "state.json")
	var out, errb bytes.Buffer
	if code := codexEnable(envPath, cfgPath, statePath, true, &out, &errb); code != 0 {
		t.Fatalf("enable exit %d: %s", code, errb.String())
	}
	writeCodexDaemonPID(t, filepath.Dir(envPath), 5613)
	withCodexDaemonAlive(t, func(pid int) bool { return pid == 5613 })

	out.Reset()
	if code := codexDisable(envPath, statePath, true, &out, &errb); code != 0 {
		t.Fatalf("disable exit %d: %s", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{"pid 5613", "still sends through Cortex", codexDaemonRestartCmd} {
		if !strings.Contains(got, want) {
			t.Errorf("disable output lacks %q:\n%s", want, got)
		}
	}
}

// Running enable again is what someone does when Codex does not seem to go through
// Cortex, and a daemon older than the file is the usual reason, so "already enabled"
// warns too.
func TestCodexEnable_AlreadyEnabledStillWarnsAboutADaemon(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	statePath := filepath.Join(filepath.Dir(envPath), "state.json")
	var out, errb bytes.Buffer
	if code := codexEnable(envPath, cfgPath, statePath, true, &out, &errb); code != 0 {
		t.Fatalf("enable exit %d: %s", code, errb.String())
	}
	writeCodexDaemonPID(t, filepath.Dir(envPath), 5613)
	withCodexDaemonAlive(t, func(pid int) bool { return pid == 5613 })
	out.Reset()
	if code := codexEnable(envPath, cfgPath, statePath, true, &out, &errb); code != 0 {
		t.Fatalf("second enable exit %d: %s", code, errb.String())
	}
	if got := out.String(); !strings.Contains(got, "Already enabled") || !strings.Contains(got, codexDaemonRestartCmd) {
		t.Errorf("second enable output = %q, want the already-enabled line and the daemon warning", got)
	}
}

// No daemon, or one whose pid file outlived it, needs no warning: the next codex starts
// a fresh daemon that reads the file enable just wrote.
func TestCodexEnable_NoWarningWithoutALiveDaemon(t *testing.T) {
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
			setup(t, filepath.Dir(envPath))
			withCodexDaemonAlive(t, func(int) bool { return false })

			var out, errb bytes.Buffer
			if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
				t.Fatalf("enable exit %d: %s", code, errb.String())
			}
			if strings.Contains(out.String(), "app-server") {
				t.Errorf("warned about a daemon that is not running:\n%s", out.String())
			}
		})
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
