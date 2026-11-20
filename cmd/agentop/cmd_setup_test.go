package main

import (
	"bytes"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
)

const stopBackgroundLine = "  Cortex runs without a supervisor here; stop it with: kill $(cat ~/.cortex/proxy.pid)\n"

// A background proxy already running leaves the service step done: only a proxy
// the run started gets the stop line. The undo line follows either way, as
// uninstall undoes more than the stop.
func TestSetupEndingNamesTheStopCommandOnlyForAProxyItStarted(t *testing.T) {
	plainOutput(t)
	const undo = "  Undo any time: agentop uninstall\n"
	for _, c := range []struct{ started, claudeCode bool }{{false, false}, {true, false}, {true, true}} {
		var out bytes.Buffer
		env := &setupEnv{home: "/h", binDir: "/h/.local/bin", cortexDir: "/h/.cortex", freshInstall: true, binOnPath: true,
			unsupervised: true, startedBackground: c.started, opts: setupOptions{claudeCode: c.claudeCode}}
		printSetupEnding(env, checklist.New(&out, false), 0)
		if got := strings.Contains(out.String(), stopBackgroundLine); got != c.started {
			t.Errorf("started a background proxy=%v, stop line printed=%v:\n%s", c.started, got, out.String())
		}
		if !strings.HasSuffix(out.String(), undo) {
			t.Errorf("started=%v claude-code=%v: want the undo line %q last:\n%s", c.started, c.claudeCode, undo, out.String())
		}
	}
}

// The undo hint is agentop uninstall however Cortex runs, as uninstall stops a
// background proxy and unroutes Claude Code too. An install-only run has none.
func TestUndoHintIsUninstallHoweverCortexRuns(t *testing.T) {
	for _, c := range []struct {
		name string
		env  setupEnv
		want string
	}{
		{"supervised", setupEnv{}, "Undo any time: agentop uninstall"},
		{"background", setupEnv{unsupervised: true}, "Undo any time: agentop uninstall"},
		{"background, Claude Code routed", setupEnv{unsupervised: true, opts: setupOptions{claudeCode: true}},
			"Undo any time: agentop uninstall"},
		{"install only", setupEnv{unsupervised: true, opts: setupOptions{installOnly: true}}, ""},
	} {
		env := c.env
		env.home, env.cortexDir = "/h", "/h/.cortex"
		if got := undoHint(&env); got != c.want {
			t.Errorf("%s: undoHint = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestParseSetupFlagsTakesTheFrozenInterface(t *testing.T) {
	o, err := parseSetupFlags([]string{"--from", "/s", "--claude-code", "-y", "--no-service", "--install-only",
		"--no-modify-path", "--restart", "--handoff-bytes=2048", "--handoff-seconds=3", "--local"}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	want := setupOptions{from: "/s", claudeCode: true, yes: true, noService: true, installOnly: true,
		noModifyPath: true, restart: true, handoffBytes: 2048, handoffSeconds: 3}
	if o != want {
		t.Errorf("parsed %+v\n  want %+v", o, want)
	}
	if !o.fromInstaller() {
		t.Error("handoff flags did not mark installer mode")
	}
}

func TestSetupConfirmFromDefaultsToYes(t *testing.T) {
	for in, want := range map[string]bool{"\n": true, "y\n": true, "YES\n": true, "n\n": false, "no\n": false, "": false, "later\n": false} {
		var w strings.Builder
		if got := setupConfirmFrom(strings.NewReader(in), &w); got != want {
			t.Errorf("answer %q → %v, want %v", in, got, want)
		}
		if w.String() != "  Continue? [Y/n] " {
			t.Errorf("prompt = %q", w.String())
		}
	}
}

func TestIsStagingDir(t *testing.T) {
	home := t.TempDir()
	staged := t.TempDir() // under os.TempDir()
	if !isStagingDir(staged, home) {
		t.Error("a dir under TMPDIR was not a staging dir")
	}
	if isStagingDir(os.TempDir(), home) {
		t.Error("TMPDIR itself read as a staging dir")
	}

	// A test's HOME is itself under TMPDIR, so everything in it reads as staged.
	// The rest move the system roots to a dir of their own.
	base := t.TempDir()
	sys, home := filepath.Join(base, "tmp"), filepath.Join(base, "home")
	saved := systemTempRoots
	systemTempRoots = func() []string { return []string{sys} }
	t.Cleanup(func() { systemTempRoots = saved })
	local := filepath.Join(home, ".cortex", "tmp", "stage")
	repo := filepath.Join(home, "src", "cortex", "bin")
	sibling := filepath.Join(base, "tmp-other") // shares the root's prefix, not its tree
	for _, d := range []string{filepath.Join(sys, "stage"), local, repo, sibling} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if !isStagingDir(filepath.Join(sys, "stage"), home) {
		t.Error("a dir under the system temp root was not a staging dir")
	}
	if !isStagingDir(local, home) {
		t.Error("~/.cortex/tmp/stage was not a staging dir")
	}
	if isStagingDir(repo, home) {
		t.Error("a source tree's bin dir read as a staging dir — setup would delete it")
	}
	if isStagingDir(filepath.Dir(local), home) {
		t.Error("~/.cortex/tmp itself read as a staging dir")
	}
	if isStagingDir(sibling, home) {
		t.Error("a sibling sharing the temp root's prefix read as a staging dir")
	}

	// A TMPDIR of / or of HOME would hold every source tree.
	for _, root := range []string{"/", home} {
		systemTempRoots = func() []string { return []string{root} }
		if isStagingDir(repo, home) {
			t.Errorf("with %s as a temp root, a source tree's bin dir read as a staging dir", root)
		}
	}

	// With HOME unset there is no ~/.cortex/tmp, not one under the working directory.
	// installerStage passes an absolute dir, which filepath.Rel already will not put
	// under a relative root; a relative one shows the root itself is gone.
	systemTempRoots = func() []string { return []string{sys} }
	t.Chdir(home)
	for _, d := range []string{local, filepath.Join(".cortex", "tmp", "stage")} {
		if isStagingDir(d, "") {
			t.Errorf("with no HOME, %s read as a staging dir", d)
		}
	}
}

// setupSignals' own body, which every other test replaces: SIGINT and SIGTERM
// arrive on its channel until its stop func runs.
func TestSetupSignalsCatchesInterruptAndTerminate(t *testing.T) {
	// The test's own catch, so a signal setupSignals misses is reported here rather
	// than killing the test binary.
	guard := make(chan os.Signal, 4)
	signal.Notify(guard, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(guard)
	sigs, stop := setupSignals()
	for _, s := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		if err := killPID(os.Getpid(), s); err != nil {
			t.Fatal(err)
		}
		<-guard
		if got := <-sigs; got != s {
			t.Errorf("sent %v, setupSignals' channel got %v", s, got)
		}
	}
	stop()
	// os/signal hands each signal to every subscriber before it takes the next, so
	// once guard has the second, any send of the first to sigs has happened.
	for _, s := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		if err := killPID(os.Getpid(), s); err != nil {
			t.Fatal(err)
		}
		<-guard
	}
	select {
	case got := <-sigs:
		t.Errorf("a %v arrived after the stop func ran", got)
	default:
	}
}

// Help is -h, --help or -help as a flag, or help as an argument: stdout, exit 0,
// before anything is looked at. A flag's value is never one.
func TestSetupHelpIsAFlagOrAnArgumentNotAValue(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"-help"}, {"help"}, {"--yes", "help"}, {"--yes", "--help"}} {
		var out, errb bytes.Buffer
		if code := runSetup(args, &out, &errb); code != 0 || out.String() != setupUsage || errb.Len() != 0 {
			t.Errorf("setup %q: exit %d, stdout %q, stderr %q", args, code, out.String(), errb.String())
		}
	}
	sc := newSetupScene(t, ok200)
	code, out := sc.run(t, "--from", "help", "--yes")
	if code == 0 || strings.Contains(out, "Usage:") {
		t.Errorf("--from help printed the usage, exit %d:\n%s", code, out)
	}
}

// A stray argument is a usage error that says what it was.
func TestSetupNamesAStrayArgument(t *testing.T) {
	var out, errb bytes.Buffer
	code := runSetup([]string{"--yes", "stray"}, &out, &errb)
	if code != 2 || !strings.HasPrefix(errb.String(), "agentop: unexpected argument \"stray\"\n"+setupUsage[:20]) || out.Len() != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
}

func TestDownloadSizeNeverReadsZeroMB(t *testing.T) {
	for n, want := range map[int64]string{
		512:        "512 B",
		999:        "999 B",
		1000:       "1.0 kB",
		2048:       "2.0 kB",
		999_949:    "999.9 kB",
		999_950:    "1.0 MB",
		24_000_000: "24.0 MB",
	} {
		if got := downloadSize(n); got != want {
			t.Errorf("downloadSize(%d) = %q, want %q", n, got, want)
		}
	}
}
