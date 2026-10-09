package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `agentop claude-code --help` printed `unknown claude-code action "--help"` and
// exited 2, sending someone looking for the command list to the one place that
// refused to print it. All three spellings are pinned, not just the one in the
// report: -h is what a habit produces and `help` is what someone copies from
// another tool.
func TestClaudeCodeHelp_PrintsUsageOnStdout(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		t.Run(arg, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runClaudeCode([]string{arg}, &out, &errb); code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			if !strings.Contains(out.String(), "agentop configure claude-code —") {
				t.Errorf("usage not on stdout:\n%s", out.String())
			}
			// The bug's signature. Asserted directly so a regression names itself.
			if strings.Contains(out.String()+errb.String(), "unknown claude-code action") {
				t.Errorf("help was read as an action name:\n%s\n%s", out.String(), errb.String())
			}
			// Explicit help is a successful answer, so it must be pipeable.
			if errb.Len() != 0 {
				t.Errorf("stderr not empty: %q", errb.String())
			}
		})
	}
}

// A genuine typo must still be an error: the help case above must not have widened
// into swallowing every unrecognised action.
func TestClaudeCodeUnknownAction_StillErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runClaudeCode([]string{"enabel"}, &out, &errb); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "unknown claude-code action") {
		t.Errorf("stderr does not report the unknown action: %q", errb.String())
	}
}

// `configure claude-code` must be an alternate spelling, not a reimplementation.
//
// Asserted as equality against the old spelling rather than against a hardcoded
// string: this keeps passing when claudeCodeStatus's wording changes, and fails only
// if the two spellings actually diverge — which is the property being claimed.
//
// `status` is the action to test because it is the only one of the three that neither
// prompts on a tty nor writes to a file.
func TestConfigure_ClaudeCodeReachesTheSameLogic(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(settings, []byte(settingsWithSecret), 0o600); err != nil {
		t.Fatal(err)
	}

	var newOut, newErr bytes.Buffer
	newCode := runConfigure([]string{"claude-code", "status", "--settings", settings}, &newOut, &newErr)

	var oldOut, oldErr bytes.Buffer
	oldCode := runClaudeCode([]string{"status", "--settings", settings}, &oldOut, &oldErr)

	if newCode != oldCode {
		t.Errorf("exit codes differ: configure = %d, claude-code = %d", newCode, oldCode)
	}
	if newOut.String() != oldOut.String() {
		t.Errorf("stdout differs:\nconfigure:\n%s\nclaude-code:\n%s", newOut.String(), oldOut.String())
	}
	// The notice belongs to the old spelling's dispatch arm in main, so neither of
	// these — both of which bypass main — may carry it.
	if strings.Contains(newErr.String(), "configure") {
		t.Errorf("the current spelling was told to use a different one: %q", newErr.String())
	}
}

// Usage errors, and the stdout/stderr split they turn on: an explicit --help is a
// successful answer (stdout, 0); an incomplete or wrong invocation is an error
// (stderr, 2).
func TestConfigure_UsageErrors(t *testing.T) {
	t.Run("no args", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runConfigure(nil, &out, &errb); code != 2 {
			t.Errorf("exit = %d, want 2", code)
		}
		if !strings.Contains(errb.String(), "agentop configure —") {
			t.Errorf("usage not on stderr:\n%s", errb.String())
		}
		if out.Len() != 0 {
			t.Errorf("stdout not empty: %q", out.String())
		}
	})

	t.Run("help", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runConfigure([]string{"--help"}, &out, &errb); code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
		if !strings.Contains(out.String(), "agentop configure —") {
			t.Errorf("usage not on stdout:\n%s", out.String())
		}
		if errb.Len() != 0 {
			t.Errorf("stderr not empty: %q", errb.String())
		}
	})

	t.Run("unknown agent", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runConfigure([]string{"frobnicate"}, &out, &errb); code != 2 {
			t.Errorf("exit = %d, want 2", code)
		}
		got := errb.String()
		// Quoted, so an agent name carrying a stray shell character is legible.
		if !strings.Contains(got, `"frobnicate"`) {
			t.Errorf("stderr does not quote the input: %q", got)
		}
		// Naming the valid set is the difference between a refusal and a dead end.
		//
		// "bobshell" is spelled in full because the shorter name is a substring of
		// it, so an assertion on "bob" alone cannot tell the two apart.
		//
		// Which makes the "bob" entry below VACUOUS, and it is listed anyway only so
		// the set matches the message: drop "bob" from the error text and this loop
		// stays green, because "bobshell" still contains it. Do not read a passing
		// run here as evidence that the message names the editor agent. What pins
		// that arm is TestConfigure_BobReachesTheSameLogic and
		// TestConfigure_BobAndBobShellAreDifferentAgents.
		for _, agent := range []string{"claude-code", "bob", "bobshell", "codex", "opencode"} {
			if !strings.Contains(got, agent) {
				t.Errorf("stderr omits %q: %q", agent, got)
			}
		}
	})
}

// `configure bobshell` must be a dispatch arm, not a reimplementation.
//
// Asserted as equality against runBobShell rather than against a hardcoded string, the
// same shape as TestConfigure_ClaudeCodeReachesTheSameLogic: this keeps passing when
// the wording changes and fails only if the two paths actually diverge, which is the
// property being claimed.
//
// `status` is the action to test because it is the only one of the three that touches
// no file.
func TestConfigure_BobShellReachesTheSameLogic(t *testing.T) {
	t.Setenv(bobShellEnvVar, "1")

	var viaConfigure, configureErr bytes.Buffer
	configureCode := runConfigure([]string{"bobshell", "status"}, &viaConfigure, &configureErr)

	var direct, directErr bytes.Buffer
	directCode := runBobShell([]string{"status"}, &direct, &directErr)

	if configureCode != directCode {
		t.Errorf("exit codes differ: configure = %d, bobshell = %d", configureCode, directCode)
	}
	if viaConfigure.String() != direct.String() {
		t.Errorf("stdout differs:\nconfigure:\n%s\nbobshell:\n%s", viaConfigure.String(), direct.String())
	}
	if configureErr.String() != directErr.String() {
		t.Errorf("stderr differs: %q vs %q", configureErr.String(), directErr.String())
	}
}

// `configure bob` must be a dispatch arm too, on the same terms as bobshell above.
//
// This replaces TestConfigure_BobIsNoLongerAnAgent, whose premise — that `bob` is not
// an agent — this change reverses. #1133 removed the name reasoning that IBM Bob needs
// no configuring; that was right about the binary and wrong about the editor, which is
// a VS Code fork with a settings.json. The old test is deleted rather than adapted
// because there is nothing left of what it claimed.
//
// `--settings` is not optional here: without it the default path is the real IBM Bob
// settings file on whatever machine runs the test.
func TestConfigure_BobReachesTheSameLogic(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "settings.json")

	var viaConfigure, configureErr bytes.Buffer
	configureCode := runConfigure([]string{"bob", "status", "--settings", settings}, &viaConfigure, &configureErr)

	var direct, directErr bytes.Buffer
	directCode := runBob([]string{"status", "--settings", settings}, &direct, &directErr)

	if configureCode != directCode {
		t.Errorf("exit codes differ: configure = %d, bob = %d", configureCode, directCode)
	}
	if viaConfigure.String() != direct.String() {
		t.Errorf("stdout differs:\nconfigure:\n%s\nbob:\n%s", viaConfigure.String(), direct.String())
	}
	if configureErr.String() != directErr.String() {
		t.Errorf("stderr differs: %q vs %q", configureErr.String(), directErr.String())
	}
}

// Two agents, both spelled with "bob", and the shorter name is a prefix of the longer.
// So the thing worth pinning is that they are NOT aliases of each other: `bob`
// configures the editor's settings.json, `bobshell` writes a shell function, and a
// dispatch that prefix-matched would silently collapse them into one.
func TestConfigure_BobAndBobShellAreDifferentAgents(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "settings.json")

	var bobOut, bobErr bytes.Buffer
	runConfigure([]string{"bob", "status", "--settings", settings}, &bobOut, &bobErr)

	t.Setenv(bobShellEnvVar, "1")
	var shellOut, shellErr bytes.Buffer
	runConfigure([]string{"bobshell", "status"}, &shellOut, &shellErr)

	if bobOut.String() == shellOut.String() {
		t.Errorf("the two agents report identically, so one is aliasing the other:\n%s", bobOut.String())
	}

	// The other direction of the same claim: bobshell has no --settings, so an
	// aliasing dispatch would make this succeed instead of failing as a usage error.
	var aliasOut, aliasErr bytes.Buffer
	if code := runConfigure([]string{"bobshell", "status", "--settings", settings}, &aliasOut, &aliasErr); code != 2 {
		t.Errorf("bobshell accepted bob's --settings: exit = %d, want 2", code)
	}
}
