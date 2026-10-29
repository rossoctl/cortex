package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// piHome is a scratch HOME with zsh as the shell and none of the machine's own Pi
// settings: PI_CODING_AGENT_DIR and CORTEX_PI would otherwise point at, or report on,
// the developer's real Pi.
func piHome(t *testing.T) string {
	t.Helper()
	home := fakeHome(t)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv(piAgentDirEnv, "")
	t.Setenv(piShellEnvVar, "")
	if err := os.Unsetenv(piShellEnvVar); err != nil {
		t.Fatal(err)
	}
	return home
}

func runPiCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runPi(args, &out, &errb)
	return code, out.String(), errb.String()
}

func readOrEmpty(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// enable writes both pieces, disable removes both, and the rc file comes back
// byte-identical: the user's own lines are untouched both ways.
func TestPiEnableDisableRoundTrip(t *testing.T) {
	home := piHome(t)
	rc := filepath.Join(home, ".zshrc")
	const mine = "export EDITOR=vim\nalias ll='ls -l'"
	if err := os.WriteFile(rc, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	ext := filepath.Join(home, ".pi", "agent", "extensions", piExtName)

	code, out, errb := runPiCmd(t, "enable", "--yes")
	if code != 0 {
		t.Fatalf("enable = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if got := readOrEmpty(t, ext); got != piExtension {
		t.Errorf("extension = %q, want piExtension", got)
	}
	if got := readOrEmpty(t, rc); got != mine+piShellBlock {
		t.Errorf("rc after enable = %q", got)
	}
	if st, err := os.Stat(rc); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("rc mode changed: %v %v", st.Mode(), err)
	}
	if s := piState(home, "/bin/zsh"); s.State != configureOn {
		t.Errorf("status after enable = %s (%s), want enabled", s.State, s.Detail)
	}

	code, out, _ = runPiCmd(t, "enable", "--yes")
	if code != 0 || !strings.Contains(out, "Already enabled") {
		t.Errorf("second enable = %d, %q; want 0 and Already enabled", code, out)
	}
	if n := strings.Count(readOrEmpty(t, rc), piShellMarkerStart); n != 1 {
		t.Errorf("second enable left %d blocks, want 1", n)
	}

	code, out, errb = runPiCmd(t, "disable", "--yes")
	if code != 0 {
		t.Fatalf("disable = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if got := readOrEmpty(t, rc); got != mine {
		t.Errorf("rc after disable = %q, want the original bytes", got)
	}
	if _, err := os.Stat(ext); !os.IsNotExist(err) {
		t.Errorf("extension still there after disable: %v", err)
	}
	if s := piState(home, "/bin/zsh"); s.State != configureOff {
		t.Errorf("status after disable = %s (%s), want not enabled", s.State, s.Detail)
	}
	code, out, _ = runPiCmd(t, "disable", "--yes")
	if code != 0 || !strings.Contains(out, "Nothing to do") {
		t.Errorf("second disable = %d, %q", code, out)
	}
}

// A file at the extension's path that agentop did not write is never overwritten by
// enable or deleted by disable. The rc half still applies, and the exit is 1
// because the configuration is not what was asked for.
func TestPiLeavesAForeignExtensionAlone(t *testing.T) {
	home := piHome(t)
	ext := piExtensionPath(home)
	if err := os.MkdirAll(filepath.Dir(ext), 0o755); err != nil {
		t.Fatal(err)
	}
	const theirs = "export default function () {}\n"
	if err := os.WriteFile(ext, []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, _ := runPiCmd(t, "enable", "--yes")
	if code != 1 || !strings.Contains(out, "is not the file agentop writes") {
		t.Errorf("enable = %d, %q; want 1 and the finding", code, out)
	}
	if got := readOrEmpty(t, ext); got != theirs {
		t.Errorf("enable overwrote the extension: %q", got)
	}
	if !strings.Contains(readOrEmpty(t, filepath.Join(home, ".zshrc")), piShellBlock) {
		t.Error("enable did not write the rc block beside the finding")
	}
	if s := piState(home, "/bin/zsh"); s.State != configurePartial || !strings.Contains(s.Detail, "is not agentop's") {
		t.Errorf("status = %s (%s), want partly enabled naming the foreign file", s.State, s.Detail)
	}

	code, _, _ = runPiCmd(t, "disable", "--yes")
	if code != 1 {
		t.Errorf("disable = %d, want 1", code)
	}
	if got := readOrEmpty(t, ext); got != theirs {
		t.Errorf("disable deleted or changed the extension: %q", got)
	}
}

// An edited block is left alone by both verbs, through the shared rc rules.
func TestPiLeavesAnEditedBlockAlone(t *testing.T) {
	home := piHome(t)
	rc := filepath.Join(home, ".zshrc")
	edited := strings.Replace(piShellBlock, "agentop exec -- pi", "agentop exec --print; pi", 1)
	if err := os.WriteFile(rc, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"enable", "disable"} {
		code, out, _ := runPiCmd(t, verb, "--yes")
		if code != 1 || !strings.Contains(out, "cortex pi block") {
			t.Errorf("%s = %d, %q; want 1 and the finding", verb, code, out)
		}
		if got := readOrEmpty(t, rc); got != edited {
			t.Errorf("%s changed the edited block: %q", verb, got)
		}
	}
}

// PI_CODING_AGENT_DIR moves the extension, because Pi loads from there instead.
func TestPiExtensionFollowsAgentDirOverride(t *testing.T) {
	home := piHome(t)
	dir := filepath.Join(home, "elsewhere")
	t.Setenv(piAgentDirEnv, dir)
	if code, out, errb := runPiCmd(t, "enable", "--yes"); code != 0 {
		t.Fatalf("enable = %d\n%s%s", code, out, errb)
	}
	if got := readOrEmpty(t, filepath.Join(dir, "extensions", piExtName)); got != piExtension {
		t.Errorf("extension not under PI_CODING_AGENT_DIR: %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".pi")); !os.IsNotExist(err) {
		t.Errorf("enable also wrote ~/.pi: %v", err)
	}
}

// Pi expands a leading ~ in PI_CODING_AGENT_DIR itself, so a quoted "~/pi" means
// $HOME/pi — not a directory named ~ under wherever agentop ran.
func TestPiExtensionExpandsTildeInAgentDir(t *testing.T) {
	home := piHome(t)
	for env, want := range map[string]string{
		"~/elsewhere": filepath.Join(home, "elsewhere", "extensions", piExtName),
		"~":           filepath.Join(home, "extensions", piExtName),
	} {
		t.Setenv(piAgentDirEnv, env)
		if got := piExtensionPath(home); got != want {
			t.Errorf("PI_CODING_AGENT_DIR=%q: path = %q, want %q", env, got, want)
		}
	}
}

// Without a terminal and without --yes, nothing is written and the exit says declined.
func TestPiDeclineWritesNothing(t *testing.T) {
	home := piHome(t)
	old := piConfirm
	piConfirm = func(io.Writer) bool { return false }
	t.Cleanup(func() { piConfirm = old })
	code, out, _ := runPiCmd(t, "enable")
	if code != exitDeclined || !strings.Contains(out, "Not changed") {
		t.Errorf("enable = %d, %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".zshrc")); !os.IsNotExist(err) {
		t.Errorf("rc file written after decline: %v", err)
	}
	if _, err := os.Stat(piExtensionPath(home)); !os.IsNotExist(err) {
		t.Errorf("extension written after decline: %v", err)
	}
}

// An unrecognised shell gets the block printed for enable and the markers for
// disable, while the extension half still runs.
func TestPiUnknownShellAdvises(t *testing.T) {
	home := piHome(t)
	t.Setenv("SHELL", "/usr/bin/fish")
	code, out, _ := runPiCmd(t, "enable", "--yes")
	if code != 0 || !strings.Contains(out, piShellBlock) {
		t.Errorf("enable = %d; want 0 and the block printed:\n%s", code, out)
	}
	if got := readOrEmpty(t, piExtensionPath(home)); got != piExtension {
		t.Error("the extension half did not run under an unknown shell")
	}
	code, out, _ = runPiCmd(t, "disable", "--yes")
	if code != 0 || strings.Contains(out, piShellBlock) || !strings.Contains(out, piShellMarkerStart) {
		t.Errorf("disable = %d; want the markers, not the block:\n%s", code, out)
	}
	if s := piState(home, "/usr/bin/fish"); s.State != configureUnknown {
		t.Errorf("status = %s (%s), want uncertain", s.State, s.Detail)
	}
}

func TestPiStatusStates(t *testing.T) {
	home := piHome(t)
	if s := piState(home, "/bin/zsh"); s.State != configureOff {
		t.Errorf("empty home: %s (%s)", s.State, s.Detail)
	}
	rc := filepath.Join(home, ".zshrc")
	if err := os.WriteFile(rc, []byte(piShellBlock), 0o644); err != nil {
		t.Fatal(err)
	}
	s := piState(home, "/bin/zsh")
	if s.State != configurePartial || !strings.Contains(s.Detail, "extension: missing") ||
		!strings.Contains(s.Detail, piShellEnvVar+" is unset") {
		t.Errorf("rc only: %s (%s)", s.State, s.Detail)
	}
	t.Setenv(piShellEnvVar, "1")
	if s := piState(home, "/bin/zsh"); !strings.Contains(s.Detail, piShellEnvVar+" is set") {
		t.Errorf("CORTEX_PI set, detail does not say so: %s", s.Detail)
	}
}

func TestPiUsageErrors(t *testing.T) {
	piHome(t)
	for _, args := range [][]string{nil, {"bogus"}, {"enable", "extra"}, {"status", "--yes"}} {
		if code, _, _ := runPiCmd(t, args...); code != 2 {
			t.Errorf("runPi(%q) = %d, want 2", args, code)
		}
	}
	if code, out, _ := runPiCmd(t, "--help"); code != 0 || !strings.Contains(out, "agentop configure pi enable") {
		t.Errorf("--help = %d, %q", code, out)
	}
}
