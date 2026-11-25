package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
	"github.com/rossoctl/cortex/cmd/agentop/internal/testhome"
)

// failingStep plans like the step it wraps and fails instead of applying.
type failingStep struct{ step }

func (f failingStep) apply(*setupEnv, *checklist.Running) (string, undo, error) {
	return "", undo{}, errors.New("injected failure")
}

func failAt(t *testing.T, label string) {
	t.Helper()
	saved := setupStepsHook
	setupStepsHook = func(s []step) []step {
		out := append([]step(nil), s...)
		for i, st := range out {
			if st.name() == label {
				out[i] = failingStep{st}
			}
		}
		return out
	}
	t.Cleanup(func() { setupStepsHook = saved })
}

// A failure at each step leaves HOME's files exactly as they were. The first
// step's failure has nothing before it to undo, so the rollback says so.
func TestSetupRollsBackAFailureAtEachStep(t *testing.T) {
	for _, tc := range []struct{ label, rolledBack string }{
		{"installed", "rolled back  nothing had been changed"},
		{"PATH", "rolled back  the changes above are undone"},
		{"config", "rolled back  the changes above are undone"},
		{"started", "rolled back  the changes above are undone"},
		{"routed", "rolled back  the changes above are undone"},
		{"late step", "rolled back  the changes above are undone"}, // after routed: its undo runs too
	} {
		t.Run(tc.label, func(t *testing.T) {
			sc := newSetupScene(t, ok200)
			writeExe(t, filepath.Join(sc.home, ".zshrc"), "alias ll='ls -l'\n")
			writeExe(t, filepath.Join(sc.home, settingsRel), `{"model":"opus"}`)
			before := homeFiles(t, sc.home)
			if tc.label == "late step" {
				failLast(t)
			} else {
				failAt(t, tc.label)
			}
			code, out := sc.run(t, "--from", sc.stage, "--yes", "--claude-code")
			if code != 1 || !strings.Contains(out, "  ✗ "+tc.label) || !strings.Contains(out, tc.rolledBack) {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			sameFiles(t, before, homeFiles(t, sc.home))
			if _, err := os.Stat(sc.loaded); err == nil {
				t.Error("rollback left the service job loaded")
			}
		})
	}
}

// runningCortex makes the fake supervisor's job run the binary its unit named
// when it was loaded, as a real job does: each load copies the ~/.local/bin file
// the unit names (cortex, or v0.7.0's authbridge-proxy) to running and starts the
// copy once with --fake-start, its output appended to ~/.cortex/proxy.log as the
// unit's is, and each unload removes the copy. A health check that read
// ~/.local/bin/cortex itself could not tell the previous Cortex reloaded from the
// broken one left loaded, since the binaries undo puts the old bytes back on disk
// either way. It wraps only fakeSupervisor's stubs, which name loaded: the real
// launchctl acts on the real io.rossoctl.cortex.
func runningCortex(t *testing.T, loaded, home, running string) {
	t.Helper()
	binDir := filepath.Join(home, ".local", "bin")
	log := filepath.Join(home, ".cortex", "proxy.log")
	for _, w := range []struct{ tool, load, unload, unit string }{
		{"launchctl", "bootstrap*", "bootout*", filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")},
		{"systemctl", "*restart*", "*disable*", filepath.Join(home, ".config", "systemd", "user", systemdUnit)},
	} {
		fake, err := exec.LookPath(w.tool)
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(fake); !isFakeSupervisorStub(b, loaded) { //nolint:gosec
			t.Fatalf("%s on PATH is not fakeSupervisor's", fake)
		}
		installStub(t, w.tool, `#!/bin/sh
case "$*" in
  `+w.load+`) bin=$(grep -o '`+binDir+`/[a-z-]*' '`+w.unit+`' | head -n 1)
    cp "$bin" '`+running+`'; '`+running+`' --fake-start >> '`+log+`' 2>&1 ;;
  `+w.unload+`) rm -f '`+running+`' ;;
esac
exec '`+fake+`' "$@"
`)
	}
}

// newUpgradeScene is a scene after a first install, whose health endpoint answers
// only while the loaded job runs a cortex without the BROKEN marker. It returns the
// copy of the cortex that job runs.
func newUpgradeScene(t *testing.T) (setupScene, string) {
	t.Helper()
	running := filepath.Join(t.TempDir(), "running")
	sc := newSetupScene(t, func(w http.ResponseWriter, _ *http.Request) {
		b, err := os.ReadFile(running)
		if err != nil || bytes.Contains(b, []byte("BROKEN")) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	runningCortex(t, sc.loaded, sc.home, running)
	if code, out := sc.run(t, "--from", sc.stage, "--yes"); code != 0 {
		t.Fatalf("first install: %d\n%s", code, out)
	}
	return sc, running
}

// brokenLogLine is what the broken cortex logs when its job starts, with HOME in
// it, as a real startup error names a file.
const brokenLogLine = "cortex: open $HOME/.cortex/ca/ca.pem: permission denied"

// brokenRelease stages an upgrade whose cortex never answers and logs
// brokenLogLine as its job starts, and returns it with the installed cortex it
// replaces.
func brokenRelease(t *testing.T, sc setupScene) (string, []byte) {
	t.Helper()
	next := t.TempDir()
	writeExe(t, filepath.Join(next, "agentop"), "#!/bin/sh\necho agentop v10.0.0\n")
	old, err := os.ReadFile(filepath.Join(sc.stage, "cortex"))
	if err != nil {
		t.Fatal(err)
	}
	writeExe(t, filepath.Join(next, "cortex"), "#!/bin/sh\n# BROKEN\n"+
		`if [ "$1" = --fake-start ]; then echo "`+brokenLogLine+`" >&2; exit 1; fi`+"\n"+
		strings.TrimPrefix(string(old), "#!/bin/sh\n"))
	return next, old
}

// readyOnFirstAnswer is for a test whose stubs answer at once: each health probe and
// supervisor ask takes its first answer, so a stub that never answers is not waited
// out.
func readyOnFirstAnswer(t *testing.T) {
	t.Helper()
	saved, first := serviceReadyTimeout, healthFirstAnswer
	serviceReadyTimeout, healthFirstAnswer = 0, true
	t.Cleanup(func() { serviceReadyTimeout, healthFirstAnswer = saved, first })
}

// An upgrade whose new cortex never answers puts the old one back, serving.
func TestSetupFailedUpgradeRevertsToThePreviousVersion(t *testing.T) {
	readyOnFirstAnswer(t)
	sc, running := newUpgradeScene(t)
	before := homeFiles(t, sc.home)
	next, old := brokenRelease(t, sc)

	code, out := sc.run(t, "--from", next, "--yes")
	if code != 1 {
		t.Fatalf("a broken upgrade exited %d:\n%s", code, out)
	}
	for _, want := range []string{"  ✗ started", "reverted     the previous Cortex is running again · healthy"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// The new version's log, read before the revert, under its failure.
	failed := strings.Index(out, "  ✗ started")
	tail := strings.Index(out, "\n      proxy.log    "+strings.ReplaceAll(brokenLogLine, "$HOME", "~")+"\n")
	reverted := strings.Index(out, "reverted     ")
	if failed < 0 || tail < failed || reverted < tail {
		t.Errorf("the broken cortex's proxy.log line is not under ✗ started:\n%s", out)
	}
	if strings.Contains(out, "cortex stub: listening") {
		t.Errorf("the tail shows the previous version's lines, not just the new one's:\n%s", out)
	}
	if strings.Contains(out, sc.home) {
		t.Errorf("the failure shows HOME in full, not as ~:\n%s", out)
	}
	sameFiles(t, before, homeFiles(t, sc.home))
	if _, err := os.Stat(sc.loaded); err != nil {
		t.Error("the previous Cortex is not loaded after the revert")
	}
	if b, _ := os.ReadFile(running); !bytes.Equal(b, old) {
		t.Error("the job left loaded after the revert does not run the previous cortex")
	}
}

// An upgrade over a Cortex stopped with `agentop service stop` rolls back to it
// stopped. The failed install loaded the new job; left loaded, it would undo the
// user's stop and run again at the next login.
func TestSetupFailedUpgradeLeavesAStoppedServiceStopped(t *testing.T) {
	readyOnFirstAnswer(t)
	sc, _ := newUpgradeScene(t)
	sp, err := resolveServicePaths(filepath.Join(sc.home, ".cortex", "config.yaml"), "",
		filepath.Join(sc.home, ".local", "bin", "cortex"))
	if err != nil {
		t.Fatal(err)
	}
	if err := controlService(runtimeGOOS(), "stop", sp, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sc.loaded); err == nil {
		t.Fatal("agentop service stop left the fake job loaded")
	}
	before := homeFiles(t, sc.home)
	next, _ := brokenRelease(t, sc)

	code, out := sc.run(t, "--from", next, "--yes")
	if code != 1 || !strings.Contains(out, "  ✗ started") || strings.Contains(out, "could not roll back") {
		t.Fatalf("a broken upgrade over a stopped service exited %d:\n%s", code, out)
	}
	sameFiles(t, before, homeFiles(t, sc.home))
	if _, err := os.Stat(sc.loaded); err == nil {
		t.Error("the rollback left a stopped Cortex's job loaded")
	}
	if runtimeGOOS() == "darwin" {
		if _, err := os.Stat(sc.loaded + ".disabled"); err != nil {
			t.Error("the rollback did not leave the job disabled, as agentop service stop does")
		}
	}
}

// make dev-install's shape: a source tree's ./bin, no dotfile edits, always restart.
func TestSetupDevInstallKeepsItsBinDirAndRestarts(t *testing.T) {
	sc := newSetupScene(t, ok200)
	repoBin := filepath.Join(sc.home, "src", "cortex", "bin")
	for _, n := range []string{"agentop", "cortex"} {
		if err := copyFile(filepath.Join(sc.stage, n), filepath.Join(repoBin, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"--from", repoBin, "--yes", "--no-modify-path", "--restart"}
	if code, out := sc.run(t, args...); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	code, out := sc.run(t, args...)
	// "healthy on" and not "already running": only a load reaches it, so this is
	// the restart, not service install finding nothing to change.
	started := "  ✓ started      " + supervisorName(runtimeGOOS()) + " · healthy on 127.0.0.1:47600\n"
	if code != 0 || !strings.Contains(out, started) || !strings.Contains(out, "  ! PATH") {
		t.Errorf("second dev-install exit %d — --restart must restart and PATH must advise:\n%s", code, out)
	}
	if !strings.Contains(out, "from ~/src/cortex/bin") {
		t.Errorf("the header does not say where the binaries came from:\n%s", out)
	}
	if _, err := os.Stat(repoBin); err != nil {
		t.Error("setup deleted a source tree's bin dir")
	}
	if b, _ := os.ReadFile(filepath.Join(sc.home, ".zshrc")); len(b) > 0 {
		t.Error("--no-modify-path edited the profile")
	}
}

// In installer mode the staging dir goes, on success and on failure alike.
func TestSetupRemovesTheInstallersStagingDir(t *testing.T) {
	t.Run("on success", func(t *testing.T) {
		sc := newSetupScene(t, ok200)
		if code, out := sc.run(t, "--from", sc.stage, "--yes", "--handoff-bytes=2048", "--handoff-seconds=1"); code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if _, err := os.Stat(sc.stage); err == nil {
			t.Error("the staging dir survived a successful run")
		}
	})
	t.Run("on failure", func(t *testing.T) {
		sc := newSetupScene(t, ok200)
		failAt(t, "config")
		code, out := sc.run(t, "--from", sc.stage, "--yes", "--handoff-bytes=2048")
		if code != 1 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if _, err := os.Stat(sc.stage); err == nil {
			t.Error("the staging dir survived a failed run")
		}
		if !strings.Contains(out, "  ✓ downloaded   2.0 kB · sha256 verified") {
			t.Errorf("no downloaded line:\n%s", out)
		}
	})
	// Unlike a usage error: those flags parsed, so the stage is known.
	t.Run("when HOME cannot be resolved", func(t *testing.T) {
		sc := newSetupScene(t, ok200)
		testhome.Set(t, "")
		code, out := sc.run(t, "--from", sc.stage, "--yes", "--handoff-bytes=2048")
		if code != 1 || !strings.Contains(out, "cannot determine your home directory") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if _, err := os.Stat(sc.stage); err == nil {
			t.Error("the staging dir survived a run that could not resolve HOME")
		}
	})
}
