package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `abctl claude-code --help` printed `unknown claude-code action "--help"` and
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
			if !strings.Contains(out.String(), "abctl configure claude-code —") {
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

// The three agents Cortex can run but cannot yet configure persistently.
//
// Each case asserts the message names ITS OWN agent, in both the opening clause and
// the exec command. That is the point of the table: the change request this
// implements carried a copy-paste slip in two of its three messages ("Persistent Bob
// configuration" under codex, "run Codex" under opencode), and a per-agent assertion
// is what catches that class of error.
func TestConfigure_ComingSoonAgents(t *testing.T) {
	for _, tc := range []struct{ agent, display, product string }{
		{"bob", "Bob", "IBM Bob"},
		{"codex", "Codex", "Codex"},
		{"opencode", "OpenCode", "OpenCode"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			var out, errb bytes.Buffer
			// Exit 0: printing the guidance is the whole job, and it succeeded.
			if code := runConfigure([]string{tc.agent}, &out, &errb); code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			got := out.String()
			if want := "Persistent " + tc.display + " configuration coming soon."; !strings.Contains(got, want) {
				t.Errorf("missing %q:\n%s", want, got)
			}
			// Backticks included: they are part of the message, and losing them is the
			// silent half of a rewrite.
			if want := "`abctl exec -- " + tc.agent + "`"; !strings.Contains(got, want) {
				t.Errorf("missing %q:\n%s", want, got)
			}
			if want := "to run " + tc.product + " under Cortex."; !strings.Contains(got, want) {
				t.Errorf("missing %q:\n%s", want, got)
			}
			// An answer on stderr cannot be piped.
			if errb.Len() != 0 {
				t.Errorf("stderr not empty: %q", errb.String())
			}
		})
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
		if !strings.Contains(errb.String(), "abctl configure —") {
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
		if !strings.Contains(out.String(), "abctl configure —") {
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
		for _, agent := range []string{"claude-code", "bob", "codex", "opencode"} {
			if !strings.Contains(got, agent) {
				t.Errorf("stderr omits %q: %q", agent, got)
			}
		}
	})
}
