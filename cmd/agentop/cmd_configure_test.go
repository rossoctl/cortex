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
		// "bobshell" is asserted to be ABSENT below, so a passing loop here is
		// evidence the message names the editor agent and not merely a substring
		// of the retired spelling.
		for _, agent := range []string{"claude-code", "bob", "codex", "opencode"} {
			if !strings.Contains(got, agent) {
				t.Errorf("stderr omits %q: %q", agent, got)
			}
		}
		if strings.Contains(got, "bobshell") {
			t.Errorf("stderr still offers the retired \"bobshell\" spelling: %q", got)
		}
	})
}

// `configure bob` must be a dispatch arm, not a reimplementation.
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

// `configure bobshell` is a RETIRED spelling, and the break is clean: the name is
// not an alias, not a hidden arm, and not an error that hands back the block to paste
// by hand. It answers as any unknown agent does, so a user following an old write-up
// learns the current namespace in one step. This is the same convention the
// abctl → agentop rename used.
func TestConfigure_BobShellIsRetiredNotAnAlias(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runConfigure([]string{"bobshell", "status"}, &out, &errb); code != 2 {
		t.Errorf("exit = %d, want 2 — the retired spelling is an unknown agent", code)
	}
	if !strings.Contains(errb.String(), "unknown agent \"bobshell\"") {
		t.Errorf("stderr does not name the spelling as unknown: %q", errb.String())
	}
	if !strings.Contains(errb.String(), "(claude-code, bob, codex, opencode, pi)") {
		t.Errorf("stderr does not list the current agents: %q", errb.String())
	}
}
