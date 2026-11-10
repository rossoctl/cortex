package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestBinariesFreshInstallAndUndo(t *testing.T) {
	env := newTestSetupEnv(t)
	env.fromDir = stageDir(t, "#!/bin/sh\necho cortex v9.9.9\n")
	p, prob := binariesStep{}.plan(env)
	if prob != nil || p.done || p.verb != "install" || !env.freshInstall {
		t.Fatalf("plan = %+v %v fresh=%v", p, prob, env.freshInstall)
	}
	_, u, err := binariesStep{}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"agentop", "cortex"} {
		got, _ := os.ReadFile(filepath.Join(env.binDir, n))
		want, _ := os.ReadFile(filepath.Join(env.fromDir, n))
		if !bytes.Equal(got, want) || !isExecutable(filepath.Join(env.binDir, n)) {
			t.Errorf("%s not installed as staged", n)
		}
	}
	if !strings.Contains(u.manual, "~/.cortex/previous/ back into ~/.local/bin") {
		t.Errorf("undo manual = %q, want the previous/ copy-back", u.manual)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"agentop", "cortex"} {
		if _, err := os.Stat(filepath.Join(env.binDir, n)); err == nil {
			t.Errorf("undo left %s", n)
		}
	}
}

func TestBinariesUpgradeKeepsThePreviousUntilUndoneOrDone(t *testing.T) {
	env := newTestSetupEnv(t)
	writeExe(t, filepath.Join(env.binDir, "agentop"), "#!/bin/sh\necho agentop v1.0.0\n")
	writeExe(t, filepath.Join(env.binDir, "cortex"), "#!/bin/sh\necho cortex v1.0.0\n")
	env.fromDir = stageDir(t, "#!/bin/sh\necho cortex v9.9.9\n")
	p, prob := binariesStep{}.plan(env)
	if prob != nil || p.verb != "replace" || env.freshInstall || env.installedVersion != "v1.0.0" {
		t.Fatalf("plan = %+v %v installed=%q", p, prob, env.installedVersion)
	}
	_, u, err := binariesStep{}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(env.previousDir(), "cortex")); !bytes.Contains(b, []byte("v1.0.0")) {
		t.Error("the previous cortex was not kept")
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(env.binDir, "cortex")); !bytes.Contains(b, []byte("v1.0.0")) {
		t.Error("undo did not bring the previous cortex back")
	}
	if _, err := os.Stat(filepath.Join(env.previousDir(), "cortex")); err == nil {
		t.Error("undo left its copy in previous/")
	}
	if _, err := os.Stat(env.previousDir()); err == nil {
		t.Error("undo left the previous/ it made")
	}

	// Done: once every step has applied, onSuccess drops previous/.
	env.onSuccess = nil
	if _, _, err := (binariesStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	for _, f := range env.onSuccess {
		f()
	}
	if _, err := os.Stat(env.previousDir()); err == nil {
		t.Error("onSuccess left previous/")
	}
}

// A failure part way through puts back each binary the step had replaced, the one
// it failed on included, and leaves no previous/ behind.
func TestBinariesUndoAfterAPartialFailureRestoresEach(t *testing.T) {
	env := newTestSetupEnv(t)
	writeExe(t, filepath.Join(env.binDir, "agentop"), "#!/bin/sh\necho agentop v1.0.0\n")
	writeExe(t, filepath.Join(env.binDir, "cortex"), "#!/bin/sh\necho cortex v1.0.0\n")
	env.fromDir = stageDir(t, "#!/bin/sh\necho cortex v9.9.9\n")
	if _, prob := (binariesStep{}).plan(env); prob != nil {
		t.Fatal(prob)
	}
	// agentop goes in; cortex, gone from the stage since the plan, fails.
	if err := os.Remove(filepath.Join(env.fromDir, "cortex")); err != nil {
		t.Fatal(err)
	}
	_, u, err := binariesStep{}.apply(env, nil)
	if err == nil {
		t.Fatal("apply with a staged binary gone succeeded")
	}
	if b, _ := os.ReadFile(filepath.Join(env.binDir, "agentop")); !bytes.Contains(b, []byte("v9.9.9")) {
		t.Fatal("fixture: the step failed before agentop went in")
	}
	if u.fn == nil {
		t.Fatal("a partly applied step returned no undo")
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"agentop", "cortex"} {
		if b, _ := os.ReadFile(filepath.Join(env.binDir, n)); !bytes.Contains(b, []byte(n+" v1.0.0")) {
			t.Errorf("after a partial failure, undo left %s = %q, want the previous one", n, b)
		}
	}
	if entries, err := os.ReadDir(env.previousDir()); err == nil {
		t.Errorf("after a partial failure, undo left previous/ holding %d entries", len(entries))
	}
}

// previous/ that was there before the run is not the undo's to remove, even
// when it is empty again afterwards.
func TestBinariesUndoKeepsAPreviousDirThatExisted(t *testing.T) {
	env := newTestSetupEnv(t)
	writeExe(t, filepath.Join(env.binDir, "agentop"), "#!/bin/sh\necho agentop v1.0.0\n")
	writeExe(t, filepath.Join(env.binDir, "cortex"), "#!/bin/sh\necho cortex v1.0.0\n")
	mustMkdir(t, env.previousDir())
	env.fromDir = stageDir(t, "#!/bin/sh\necho cortex v9.9.9\n")
	if _, prob := (binariesStep{}).plan(env); prob != nil {
		t.Fatal(prob)
	}
	_, u, err := binariesStep{}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(env.previousDir()); err != nil {
		t.Error("undo removed a previous/ that existed before the step")
	}
}

// The consent row and the done line name what the stage holds, so a staged
// cortex-session-dump is named too.
func TestBinariesNameWhatIsStaged(t *testing.T) {
	env := newTestSetupEnv(t)
	env.fromDir = stageDir(t, "#!/bin/sh\necho cortex v9.9.9\n")
	writeExe(t, filepath.Join(env.fromDir, "cortex-session-dump"), "#!/bin/sh\n")
	p, prob := binariesStep{}.plan(env)
	if want := "agentop, cortex, cortex-session-dump"; prob != nil || p.what != want {
		t.Fatalf("plan = %+v %v, want what = %q", p, prob, want)
	}
	detail, _, err := binariesStep{}.apply(env, nil)
	if want := "agentop, cortex, cortex-session-dump → ~/.local/bin"; err != nil || detail != want {
		t.Errorf("apply = %q %v, want %q", detail, err, want)
	}
}

// Setup installs cortex-gateway only when the installer staged it.
func TestStagedBinariesNeedNoGateway(t *testing.T) {
	env := newTestSetupEnv(t)
	env.fromDir = stageDir(t, "#!/bin/sh\necho cortex v9.9.9\n")
	if got := stagedBinaries(env); !slices.Equal(got, []string{"agentop", "cortex"}) {
		t.Errorf("stagedBinaries = %q, want agentop and cortex only", got)
	}
	writeExe(t, filepath.Join(env.fromDir, "cortex-gateway"), "#!/bin/sh\n")
	if got := stagedBinaries(env); !slices.Contains(got, "cortex-gateway") {
		t.Errorf("stagedBinaries = %q, want cortex-gateway in it", got)
	}
}

func TestBinariesUnchangedIsDone(t *testing.T) {
	env := newTestSetupEnv(t)
	env.fromDir = stageDir(t, "#!/bin/sh\necho cortex v9.9.9\n")
	for _, n := range []string{"agentop", "cortex"} {
		if err := copyFile(filepath.Join(env.fromDir, n), filepath.Join(env.binDir, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if p, prob := (binariesStep{}).plan(env); prob != nil || !p.done {
		t.Errorf("identical binaries planned a change: %+v %v", p, prob)
	}
}

func TestBinariesRepairModeNeedsAnInstall(t *testing.T) {
	env := newTestSetupEnv(t)
	if _, prob := (binariesStep{}).plan(env); prob == nil {
		t.Error("repair mode with nothing installed raised no problem")
	}
	writeExe(t, filepath.Join(env.binDir, "agentop"), "#!/bin/sh\necho agentop v1\n")
	writeExe(t, filepath.Join(env.binDir, "cortex"), "#!/bin/sh\n")
	if p, prob := (binariesStep{}).plan(env); prob != nil || !p.done {
		t.Errorf("repair mode with an install = %+v %v, want done", p, prob)
	}
}

func TestCleanupRemovesOnlyOurStaleBinariesAndCanUndo(t *testing.T) {
	env := newTestSetupEnv(t)
	writeExe(t, filepath.Join(env.binDir, "abctl"), "#!/bin/sh\n# github.com/rossoctl/cortex/cmd/abctl\n")
	writeExe(t, filepath.Join(env.binDir, "authbridge-proxy"), "#!/bin/sh\n# github.com/rossoctl/cortex/authbridge/cmd/authbridge-proxy\n")
	if p, _ := (cleanupStep{afterService: false}).plan(env); p.what != "pre-rename abctl" {
		t.Errorf("before the service step, plan = %q, want abctl only", p.what)
	}
	p, _ := cleanupStep{afterService: true}.plan(env)
	if p.hidden || p.what != "pre-rename abctl, authbridge-proxy" {
		t.Fatalf("plan = %+v", p)
	}
	_, u, err := cleanupStep{afterService: true}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(env.binDir, "abctl")); err == nil {
		t.Error("our abctl was not removed")
	}
	if _, err := os.Stat(filepath.Join(env.previousDir(), "abctl")); err != nil {
		t.Error("abctl was not moved into previous/")
	}
	if !strings.Contains(u.manual, "~/.cortex/previous/ back into ~/.local/bin") {
		t.Errorf("undo manual = %q, want the previous/ copy-back", u.manual)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(env.binDir, "abctl")); err != nil {
		t.Error("undo did not restore abctl")
	}
	if _, err := os.Stat(filepath.Join(env.previousDir(), "abctl")); err == nil {
		t.Error("undo left its copy of abctl in previous/")
	}
	if _, err := os.Stat(env.previousDir()); err == nil {
		t.Error("undo left the previous/ it made")
	}
}

// A failure part way through puts back what the step had already moved.
func TestCleanupUndoAfterAPartialFailureRestoresWhatItMoved(t *testing.T) {
	env := newTestSetupEnv(t)
	abctl := "#!/bin/sh\n# github.com/rossoctl/cortex/cmd/abctl\n"
	writeExe(t, filepath.Join(env.binDir, "abctl"), abctl)
	writeExe(t, filepath.Join(env.binDir, "authbridge-proxy"), "#!/bin/sh\n# github.com/rossoctl/cortex/cmd/authbridge-proxy\n")
	// A directory where authbridge-proxy's copy would go fails its move, after
	// abctl's.
	mustMkdir(t, filepath.Join(env.previousDir(), "authbridge-proxy", "x"))
	_, u, err := cleanupStep{afterService: true}.apply(env, nil)
	if err == nil {
		t.Fatal("moving authbridge-proxy onto a directory succeeded")
	}
	if _, err := os.Stat(filepath.Join(env.binDir, "abctl")); err == nil {
		t.Fatal("fixture: the step failed before abctl was moved")
	}
	if u.fn == nil {
		t.Fatal("a partly applied step returned no undo")
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(env.binDir, "abctl")); string(b) != abctl {
		t.Errorf("after a partial failure, undo left abctl = %q", b)
	}
	if _, err := os.Stat(filepath.Join(env.previousDir(), "abctl")); err == nil {
		t.Error("after a partial failure, undo left its copy of abctl in previous/")
	}
}

func TestCleanupKeepsABinaryAUnitStillRuns(t *testing.T) {
	for _, unit := range []string{
		filepath.Join("Library", "LaunchAgents", launchdLabel+".plist"),
		filepath.Join(".config", "systemd", "user", systemdUnit),
	} {
		env := newTestSetupEnv(t)
		writeExe(t, filepath.Join(env.binDir, "authbridge-proxy"), "#!/bin/sh\n# github.com/rossoctl/cortex/cmd/authbridge-proxy\n")
		writeExe(t, filepath.Join(env.home, unit), "<string>"+filepath.Join(env.binDir, "authbridge-proxy")+"</string>")
		detail, u, err := cleanupStep{afterService: true}.apply(env, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(env.binDir, "authbridge-proxy")); err != nil {
			t.Errorf("%s: a binary the unit still runs was removed", unit)
		}
		if want := "kept authbridge-proxy: the service still runs it;" +
			" `agentop service install` moves the service over, then delete the old file"; detail != want {
			t.Errorf("%s: detail = %q, want %q", unit, detail, want)
		}
		// Keeping everything changed nothing: no "undone" line, no success hook.
		if u.fn != nil || len(env.onSuccess) != 0 {
			t.Errorf("%s: a run that kept everything returned undo=%v, %d success hooks", unit, u.fn != nil, len(env.onSuccess))
		}
	}
}

// A live pidfile process that ps cannot name may still be a pre-rename proxy, so
// authbridge-proxy stays; abctl is not subject to that guard.
func TestCleanupKeepsAuthbridgeProxyWhileAnUnnamedProcessHoldsThePidfile(t *testing.T) {
	env := newTestSetupEnv(t)
	writeExe(t, filepath.Join(env.binDir, "abctl"), "#!/bin/sh\n# github.com/rossoctl/cortex/cmd/abctl\n")
	writeExe(t, filepath.Join(env.binDir, "authbridge-proxy"), "#!/bin/sh\n# github.com/rossoctl/cortex/cmd/authbridge-proxy\n")
	if err := os.MkdirAll(env.cortexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pid := []byte(strconv.Itoa(os.Getpid()) + "\n")
	if err := os.WriteFile(filepath.Join(env.cortexDir, "proxy.pid"), pid, 0o600); err != nil {
		t.Fatal(err)
	}
	installStub(t, "ps", "#!/bin/sh\nexit 1\n")
	detail, _, err := cleanupStep{afterService: true}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(env.binDir, "authbridge-proxy")); err != nil {
		t.Error("authbridge-proxy was removed while an unnamed process held the pidfile")
	}
	if want := "removed abctl; kept authbridge-proxy: the process in ~/.cortex/proxy.pid may still run it (ps cannot name it)"; detail != want {
		t.Errorf("detail = %q, want %q", detail, want)
	}
}

func TestCleanupIgnoresSomeoneElsesAbctl(t *testing.T) {
	env := newTestSetupEnv(t)
	writeExe(t, filepath.Join(env.binDir, "abctl"), "#!/bin/sh\necho abctl v1.0.0\n")
	if p, _ := (cleanupStep{afterService: true}).plan(env); !p.hidden {
		t.Errorf("a foreign abctl was planned for removal: %+v", p)
	}
}

// remove_stale matches cmd/<old>, not just our module: a file named abctl that
// carries another of our commands' paths is not a pre-rename abctl.
func TestCleanupWantsTheOldCommandsOwnPath(t *testing.T) {
	env := newTestSetupEnv(t)
	writeExe(t, filepath.Join(env.binDir, "abctl"), "#!/bin/sh\n# github.com/rossoctl/cortex/cmd/agentop\n")
	if p, _ := (cleanupStep{afterService: true}).plan(env); !p.hidden {
		t.Errorf("an abctl without cmd/abctl was planned for removal: %+v", p)
	}
}
