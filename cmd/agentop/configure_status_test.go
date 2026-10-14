package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tri-state's words are what the table prints, so they are pinned here rather
// than only where the table is rendered: the issue (#1164) asked for "enabled" and
// "uncertain" by name, and configureUnknown being the ZERO VALUE is the part a future
// agent's state function depends on without knowing it — a row whose state was never
// set must read as "agentop could not tell", never as a confident "not enabled".
func TestConfigureState_Words(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state configureState
		want  string
	}{
		{"on", configureOn, "enabled"},
		{"off", configureOff, "not enabled"},
		{"partial", configurePartial, "partly enabled"},
		{"unknown", configureUnknown, "uncertain"},
		// Not a state any code produces — the point is what happens if one day it
		// does. A new constant added without a String arm must land on "uncertain",
		// which is the only answer that is never a false claim about a harness.
		{"out of range", configureState(99), "uncertain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}

	// The zero value, asserted separately from the table above so the claim is legible
	// as its own property rather than as one row among five.
	var unset configureState
	if unset != configureUnknown {
		t.Errorf("the zero configureState is %v, want configureUnknown — an unset state must not read as a verdict", unset)
	}
}

// claude-code's three states, which are the reason the shared type is not a bool.
//
// The PARTIAL case is the one worth the fixture work: claudeCodeStatus has always
// distinguished "N of M set" from both "all set" and "none set", and a table that
// flattened it would tell a user with a half-written settings.json that Claude Code is
// not routed — when in fact enable has something left to finish, and a session would
// reach the proxy and fail to verify its certificate. The count itself is asserted in
// the detail string, because "partly" without a figure is not actionable.
func TestClaudeCodeState_ThreeStates(t *testing.T) {
	// One managed key, so the fixture is unambiguously partial whatever managedKeys
	// grows to; the full case is built by setting every key the list names.
	all := func() string {
		var b strings.Builder
		b.WriteString(`{"env": {`)
		for i, k := range managedKeys {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`"` + k + `": "x"`)
		}
		b.WriteString(`}}`)
		return b.String()
	}

	for _, tc := range []struct {
		name, settings string
		want           configureState
		detail         string
	}{
		{"every key set", all(), configureOn, ""},
		{"no env block at all", `{"model": "opus"}`, configureOff, "no Cortex keys"},
		// A settings file holding unrelated env keys is still "not enabled": the
		// presence of an env block is not the presence of Cortex's keys.
		{"env block without our keys", `{"env": {"ANTHROPIC_BASE_URL": "https://x"}}`, configureOff, "no Cortex keys"},
		{"one key of seven", `{"env": {"` + envProxy + `": "http://127.0.0.1:47600"}}`, configurePartial, "1 of 7 keys set"},
		// Not readable JSON is NOT "not enabled": the file exists and says something
		// agentop cannot parse, so there is no finding to report about its contents.
		{"not JSON", `{broken`, configureUnknown, "not valid JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, _ := fixture(t, tc.settings)
			got := claudeCodeState(settings)
			if got.Agent != "claude-code" {
				t.Errorf("Agent = %q, want claude-code", got.Agent)
			}
			if got.State != tc.want {
				t.Errorf("State = %v (%s), want %v (%s)\ndetail: %s",
					got.State, got.State, tc.want, tc.want, got.Detail)
			}
			if tc.detail != "" && !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("Detail = %q, want it to mention %q", got.Detail, tc.detail)
			}
		})
	}

	// An absent file is "not enabled" and not "uncertain", unlike bob and opencode —
	// the two agents whose absent case IS an uncertainty, because for them there is no
	// configuration to read at all (no settings document, no binary) rather than a
	// place to look that is empty. readSettings has always treated a
	// missing file as the empty document — that is a definite answer about Claude
	// Code's configuration, because Claude Code reads the same absence the same way.
	t.Run("absent file", func(t *testing.T) {
		got := claudeCodeState(filepath.Join(t.TempDir(), "nope.json"))
		if got.State != configureOff {
			t.Errorf("State = %s, want %s", got.State, configureOff)
		}
	})
}

// codex's states, and the one case where a count is not a verdict.
//
// A dotenv file that defines a managed key twice is configureUnknown even when every
// key matches: Codex's own loader decides which line wins, so the value agentop read
// may not be the one Codex uses. codexStatus has always warned about this; what the
// table cannot do is print both the warning and the count, so the state function keeps
// the honest half. Pinned here because a later simplification that folded the
// duplicate check into the count would be invisible in the per-agent output.
func TestCodexState_ThreeStatesAndDuplicates(t *testing.T) {
	// The full case is built by running enable, so the fixture holds exactly the
	// values codexWanted derives rather than a copy of them that can drift.
	enabled := func(t *testing.T) (envPath, cfgPath string) {
		t.Helper()
		envPath, cfgPath = codexFixture(t, "")
		var out, errb bytes.Buffer
		if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
			t.Fatalf("enable: exit %d: %s", code, errb.String())
		}
		return envPath, cfgPath
	}

	t.Run("every key set", func(t *testing.T) {
		envPath, cfgPath := enabled(t)
		if got := codexState(envPath, cfgPath); got.State != configureOn {
			t.Errorf("State = %s, want %s (detail: %s)", got.State, configureOn, got.Detail)
		}
	})

	t.Run("no keys set", func(t *testing.T) {
		envPath, cfgPath := codexFixture(t, "UNRELATED=1\n")
		got := codexState(envPath, cfgPath)
		if got.State != configureOff {
			t.Errorf("State = %s, want %s (detail: %s)", got.State, configureOff, got.Detail)
		}
	})

	t.Run("one key of five", func(t *testing.T) {
		envPath, cfgPath := enabled(t)
		// Break exactly one value, so what is left is genuinely partial rather than
		// a file with a key missing — a stale proxy address is the realistic shape:
		// it is what an enable from before the port moved leaves behind.
		body, err := os.ReadFile(envPath)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(body), "\n")
		kept := lines[:0]
		for _, l := range lines {
			if strings.HasPrefix(l, "HTTP_PROXY=") {
				l = "HTTP_PROXY=http://127.0.0.1:1"
			}
			kept = append(kept, l)
		}
		if err := os.WriteFile(envPath, []byte(strings.Join(kept, "\n")), 0o600); err != nil {
			t.Fatal(err)
		}
		got := codexState(envPath, cfgPath)
		if got.State != configurePartial {
			t.Fatalf("State = %s, want %s (detail: %s)", got.State, configurePartial, got.Detail)
		}
		if !strings.Contains(got.Detail, "4 of 5 keys set") {
			t.Errorf("Detail = %q, want the count in it", got.Detail)
		}
	})

	t.Run("duplicate definition is uncertain, not a count", func(t *testing.T) {
		envPath, cfgPath := enabled(t)
		f, err := os.OpenFile(envPath, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		// A second definition of a key whose first definition is already correct, so
		// the count is still 5 of 5 and only the ambiguity makes this uncertain.
		if _, err := f.WriteString("HTTP_PROXY=http://127.0.0.1:1\n"); err != nil {
			t.Fatal(err)
		}
		f.Close()
		got := codexState(envPath, cfgPath)
		if got.State != configureUnknown {
			t.Errorf("State = %s, want %s — a duplicate makes the count unusable (detail: %s)",
				got.State, configureUnknown, got.Detail)
		}
		if !strings.Contains(got.Detail, "more than once") {
			t.Errorf("Detail = %q, want it to name the duplication", got.Detail)
		}
	})

	// Without Cortex's own config there is nothing to compare the file's values
	// against, which is the same refusal codexStatus makes on its own last line.
	t.Run("no Cortex config", func(t *testing.T) {
		envPath, _ := enabled(t)
		got := codexState(envPath, filepath.Join(t.TempDir(), "absent.yaml"))
		if got.State != configureUnknown {
			t.Errorf("State = %s, want %s (detail: %s)", got.State, configureUnknown, got.Detail)
		}
	})
}

// bob's four states. The ownership test is bobOwns's, already exercised in
// cmd_bob_test.go; what is pinned here is the MAPPING from it onto the shared
// tri-state, and specifically that bob's two distinct "no" cases do not collapse:
//
//   - a foreign proxy is configureOff — a positive finding that Bob routes somewhere
//     that is not Cortex, and agentop deliberately leaves it alone.
//   - a loopback proxy that is not the address the config now names is
//     configurePartial — almost always Cortex from before the port moved, which is a
//     different problem with a different fix.
//
// Collapsing the second into the first is what the table would do with a bool, and it
// is the case bobStatus spends two detail lines on.
func TestBobState_FourStates(t *testing.T) {
	for _, tc := range []struct {
		name, settings string
		want           configureState
	}{
		{"ours", `{"http.proxy": "http://127.0.0.1:47600"}`, configureOn},
		{"unset", `{"editor.fontSize": 13}`, configureOff},
		{"foreign", `{"http.proxy": "http://proxy.corp.example.com:3128"}`, configureOff},
		{"drifted loopback", `{"http.proxy": "http://127.0.0.1:47599"}`, configurePartial},
		// Not a string. configureOff rather than uncertain: the key is there and
		// holds something Bob cannot read as a proxy URL either.
		{"wrong type", `{"http.proxy": 47600}`, configureOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, cfg := fixture(t, tc.settings)
			want, _ := bobWanted(cfg, t.TempDir())
			got := bobState(settings, cfg, want)
			if got.Agent != "bob" {
				t.Errorf("Agent = %q, want bob", got.Agent)
			}
			if got.State != tc.want {
				t.Errorf("State = %s, want %s (detail: %s)", got.State, tc.want, got.Detail)
			}
		})
	}

	// The two uncertainties, which are bob's own and the reason bobStatusUnknown
	// exists: with no settings document there is nothing to make a claim about, and
	// with no Cortex config a loopback value cannot be judged either way.
	t.Run("no settings document", func(t *testing.T) {
		dir := t.TempDir()
		_, cfg := fixture(t, "")
		want, _ := bobWanted(cfg, dir)
		got := bobState(filepath.Join(dir, "absent.json"), cfg, want)
		if got.State != configureUnknown {
			t.Errorf("State = %s, want %s (detail: %s)", got.State, configureUnknown, got.Detail)
		}
	})

	// TWO MORE UNCERTAINTIES, both aligned with codex and claude-code in #1164's
	// review. Each used to answer configureOff — a positive finding that Bob is not
	// routed — for a file agentop could not actually read a verdict out of.
	t.Run("duplicate key is uncertain, not a finding", func(t *testing.T) {
		// Two values for one key: Bob's own precedence decides which wins, so the
		// value agentop read may not be the one in effect. codexState answers the same
		// situation the same way, in its own words, and the two disagreed until now.
		settings, cfg := fixture(t, `{"http.proxy": "http://127.0.0.1:47600", "http.proxy": "http://127.0.0.1:47600"}`)
		want, _ := bobWanted(cfg, t.TempDir())
		got := bobState(settings, cfg, want)
		if got.State != configureUnknown {
			t.Errorf("State = %s, want %s (detail: %s)", got.State, configureUnknown, got.Detail)
		}
	})

	t.Run("unparseable settings file is uncertain", func(t *testing.T) {
		// The file exists and agentop cannot read what it says, which is not the same
		// as reading it and finding nothing — claudeCodeStateFrom's rule for the
		// identical case.
		settings, cfg := fixture(t, `{not json`)
		want, _ := bobWanted(cfg, t.TempDir())
		got := bobState(settings, cfg, want)
		if got.State != configureUnknown {
			t.Errorf("State = %s, want %s (detail: %s)", got.State, configureUnknown, got.Detail)
		}
	})

	t.Run("loopback proxy with no Cortex config", func(t *testing.T) {
		settings, _ := fixture(t, `{"http.proxy": "http://127.0.0.1:47600"}`)
		absent := filepath.Join(t.TempDir(), "absent.yaml")
		// bobWanted over a config that is not there returns an empty proxy, which is
		// what makes bobOwns answer bobUnknown.
		got := bobState(settings, absent, "")
		if got.State != configureUnknown {
			t.Errorf("State = %s, want %s (detail: %s)", got.State, configureUnknown, got.Detail)
		}
	})
}

// bobshell NEVER reports enabled, and that is the whole reason it gets a state
// function rather than a bool read off the environment variable.
//
// bobShellStatus already spends a paragraph on why: the variable is exported, so every
// child process inherits it — including a non-interactive subshell that never reads
// the rc file and therefore has no bob function at all. agentop is such a child, and a
// shell's function table is never exported, so agentop cannot see the thing that
// actually routes the command. "The variable is set" is evidence; configurePartial is
// what evidence without confirmation is. A future change that "fixes" this to
// configureOn would be printing a claim agentop cannot check.
func TestBobShellState_NeverFullyEnabled(t *testing.T) {
	t.Run("variable set", func(t *testing.T) {
		t.Setenv(bobShellEnvVar, "1")
		got := bobShellState()
		if got.Agent != "bobshell" {
			t.Errorf("Agent = %q, want bobshell", got.Agent)
		}
		if got.State != configurePartial {
			t.Errorf("State = %s, want %s — agentop cannot see a shell's function table",
				got.State, configurePartial)
		}
		if got.State == configureOn {
			t.Error("reported fully enabled from an inherited variable")
		}
		// The detail has to carry the one thing the user can do about the gap, which
		// is the check bobShellStatus names.
		if !strings.Contains(got.Detail, "type bob") {
			t.Errorf("Detail = %q, want it to name the \"type bob\" check", got.Detail)
		}
	})

	t.Run("variable unset", func(t *testing.T) {
		// Unset rather than empty: an empty-but-present variable is a different case
		// from an absent one, and LookupEnv is what distinguishes them.
		// t.Setenv first so the testing package records the original value and
		// restores it on cleanup; Unsetenv alone would leave a developer who exports
		// CORTEX_BOBSHELL without it for the rest of the run. Same pairing as
		// cmd_bobshell_test.go's.
		t.Setenv(bobShellEnvVar, "")
		if err := os.Unsetenv(bobShellEnvVar); err != nil {
			t.Fatal(err)
		}
		got := bobShellState()
		if got.State != configureOff {
			t.Errorf("State = %s, want %s", got.State, configureOff)
		}
	})
}

// opencode's states, including both of its uncertainties.
//
// Both have to stay uncertain. A CLI that cannot be run means the service environment
// cannot be read at all, and a Cortex config that cannot be read means the nine values
// that WERE read have nothing to be judged against — openCodeStatus prints the keys in
// both cases and declines to close with a verdict, and the table says the same thing
// in one word. "not enabled" in either case would be a finding agentop did not make.
func TestOpenCodeState_StatesAndUncertainties(t *testing.T) {
	t.Run("every key set", func(t *testing.T) {
		f := &openCodeCLI{}
		home := stubOpenCodeCLI(t, f)
		cfg := filepath.Join(home, cortexCfgRel)
		want, _, err := wantedFromConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		f.env = openCodeValues(want)
		got := openCodeState(fakeOpenCodePath, cfg)
		if got.Agent != "opencode" {
			t.Errorf("Agent = %q, want opencode", got.Agent)
		}
		if got.State != configureOn {
			t.Errorf("State = %s, want %s (detail: %s)", got.State, configureOn, got.Detail)
		}
	})

	t.Run("no keys set", func(t *testing.T) {
		f := &openCodeCLI{env: map[string]string{"UNRELATED": "1"}}
		home := stubOpenCodeCLI(t, f)
		got := openCodeState(fakeOpenCodePath, filepath.Join(home, cortexCfgRel))
		if got.State != configureOff {
			t.Errorf("State = %s, want %s (detail: %s)", got.State, configureOff, got.Detail)
		}
	})

	// The partial case, and the realistic shape of it: a service environment whose
	// proxy variables were set by an enable from before the port moved. Those values
	// are not counted — a service started with them would not reach this Cortex — so
	// the verdict is partial and the count says how much is left.
	t.Run("some keys set", func(t *testing.T) {
		f := &openCodeCLI{}
		home := stubOpenCodeCLI(t, f)
		cfg := filepath.Join(home, cortexCfgRel)
		want, _, err := wantedFromConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		vals := openCodeValues(want)
		partial := map[string]string{}
		for k, v := range vals {
			partial[k] = v
		}
		delete(partial, openCodeKeys[0])
		f.env = partial
		got := openCodeState(fakeOpenCodePath, cfg)
		if got.State != configurePartial {
			t.Fatalf("State = %s, want %s (detail: %s)", got.State, configurePartial, got.Detail)
		}
		if !strings.Contains(got.Detail, "of 9 keys set") {
			t.Errorf("Detail = %q, want the count in it", got.Detail)
		}
	})

	// The service environment cannot be read. Modelled as the CLI printing something
	// that is not the JSON object openCodeServiceEnv expects, which is what an
	// opencode of the wrong version does — the same failure class as the binary being
	// missing, and the one the fake can express.
	t.Run("service environment unreadable", func(t *testing.T) {
		notJSON := "Usage: opencode [options]"
		f := &openCodeCLI{getEnv: &notJSON}
		home := stubOpenCodeCLI(t, f)
		got := openCodeState(fakeOpenCodePath, filepath.Join(home, cortexCfgRel))
		if got.State != configureUnknown {
			t.Errorf("State = %s, want %s (detail: %s)", got.State, configureUnknown, got.Detail)
		}
	})

	t.Run("no Cortex config", func(t *testing.T) {
		f := &openCodeCLI{}
		stubOpenCodeCLI(t, f)
		got := openCodeState(fakeOpenCodePath, filepath.Join(t.TempDir(), "absent.yaml"))
		if got.State != configureUnknown {
			t.Errorf("State = %s, want %s (detail: %s)", got.State, configureUnknown, got.Detail)
		}
		if !strings.Contains(got.Detail, "cannot compare") {
			t.Errorf("Detail = %q, want it to say the comparison could not be made", got.Detail)
		}
	})
}

// The table itself: shape, header, and that every agent appears exactly once.
//
// The rows are built by hand rather than from the real machine, because the table's
// rendering is the thing under test and a run against a live home directory reports
// whatever that machine happens to have configured.
func TestWriteConfigureStatusTable_Shape(t *testing.T) {
	var out bytes.Buffer
	writeConfigureStatusTable([]configureStatus{
		{Agent: "claude-code", State: configureOn, Detail: "/home/u/.claude/settings.json"},
		{Agent: "bob", State: configureOff, Detail: `"http.proxy" is unset`},
		{Agent: "bobshell", State: configurePartial, Detail: "CORTEX_BOB=1"},
		{Agent: "opencode", State: configureUnknown, Detail: "opencode not found"},
		{Agent: "codex", State: configurePartial, Detail: "3 of 5 keys set"},
	}, &out)
	got := out.String()

	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	// The header names the issue's two columns plus the detail the one-word verdict
	// rests on. Asserted as the first line so a row prepended above it fails here.
	if !strings.HasPrefix(lines[0], "HARNESS") || !strings.Contains(lines[0], "STATUS") || !strings.Contains(lines[0], "DETAIL") {
		t.Errorf("first line is not the header: %q", lines[0])
	}
	for i, agent := range []string{"claude-code", "bob", "bobshell", "opencode", "codex"} {
		if !strings.HasPrefix(lines[i+1], agent) {
			t.Errorf("row %d = %q, want it to start with %q", i+1, lines[i+1], agent)
		}
	}
	// EACH ROW'S OWN VERDICT, read out of that row rather than searched for in the
	// whole output. strings.Contains(got, "enabled") is satisfied by "not enabled" and
	// by "partly enabled", so a renderer that dropped the claude-code row's verdict
	// entirely still passed a Contains check — it is only ever false if no row has any
	// verdict at all. Fields()[1] is the STATUS cell on a row whose agent name and
	// verdict are both single words; the two-word verdicts are compared as a prefix of
	// the rest of the line for the same reason.
	wantVerdict := map[string]string{
		"claude-code": "enabled",
		"bob":         "not enabled",
		"bobshell":    "partly enabled",
		"opencode":    "uncertain",
		"codex":       "partly enabled",
	}
	for i, agent := range []string{"claude-code", "bob", "bobshell", "opencode", "codex"} {
		rest := strings.TrimSpace(strings.TrimPrefix(lines[i+1], agent))
		if want := wantVerdict[agent]; !strings.HasPrefix(rest, want) {
			t.Errorf("row %q verdict = %q, want it to start with %q\n%s", agent, rest, want, got)
		}
	}
	// No trailing padding on any line. tabwriter pads every cell including the last,
	// which leaves invisible whitespace a diff or a `git add` hook will complain
	// about; runServerList trims for the same reason.
	for _, l := range lines {
		if l != strings.TrimRight(l, " ") {
			t.Errorf("line has trailing whitespace: %q", l)
		}
	}
	// The footer is where the paragraph went. Without it the one-line detail reads as
	// everything agentop has to say, and bob alone has four things to report.
	if !strings.Contains(got, "agentop configure <harness> status") {
		t.Errorf("output does not point at the per-agent status:\n%s", got)
	}
}

// Control characters in a detail string must not reach the terminal.
//
// The detail column carries file paths and error strings, and an error wrapping a
// path a user chose can carry anything — including an escape sequence that would
// repaint or clear the terminal this table is being read in. sanitizeCell is the
// repo's existing answer (cmd_pipeline.go), reused rather than re-derived.
func TestWriteConfigureStatusTable_SanitizesDetail(t *testing.T) {
	var out bytes.Buffer
	writeConfigureStatusTable([]configureStatus{
		{Agent: "codex", State: configureUnknown, Detail: "/tmp/\x1b[2Jevil/.env"},
	}, &out)
	if strings.Contains(out.String(), "\x1b") {
		t.Errorf("an escape sequence reached the output: %q", out.String())
	}
}

// A MULTI-LINE DETAIL COLLAPSES TO ONE LINE, rather than carrying a U+FFFD per newline.
//
// Several details that reach this column are wrapped paragraphs written for a
// paragraph-shaped surface — errBridgeDisabled and codexWanted's missing-ca_dir error.
// sanitizeCell maps every rune under 0x20 to U+FFFD, so before the collapse a newline
// rendered as a literal "�" mid-sentence, twice in the codex row under
// `tls_bridge.mode: disabled` (caught in #1164's review).
//
// Both halves are asserted, because collapsing with a plain ReplaceAll would satisfy
// the first and not the second: strings.Fields drops the two-space continuation
// indents that follow those newlines too, so the sentence reads with single spaces
// rather than with three.
func TestWriteConfigureStatusTable_CollapsesAMultiLineDetail(t *testing.T) {
	var out bytes.Buffer
	writeConfigureStatusTable([]configureStatus{
		{Agent: "codex", State: configureUnknown, Detail: "cannot compare with Cortex's config:\n  no enabled TLS bridge;\n  enable it first"},
	}, &out)
	got := out.String()
	if strings.ContainsRune(got, '\uFFFD') {
		t.Errorf("a newline became U+FFFD in the detail column:\n%s", got)
	}
	if want := "cannot compare with Cortex's config: no enabled TLS bridge; enable it first"; !strings.Contains(got, want) {
		t.Errorf("the detail did not collapse to one line:\n got %q\nwant it to contain %q", got, want)
	}
	// Still one row and one header: the collapse must not introduce a line break.
	if n := len(strings.Split(strings.TrimRight(got, "\n"), "\n")); n != 4 {
		t.Errorf("output has %d lines, want 4 (header, row, blank, footer):\n%s", n, got)
	}
}

// ONE AGENT'S MISSING CONFIG MUST NOT BREAK THE TABLE.
//
// This is the property the whole command rests on, and the common case rather than an
// edge one: nobody has all five harnesses installed, so most runs have several agents
// with no settings file, no dotenv and no opencode binary at all. Each of those is a
// row saying "uncertain" with the reason, never an early return, a panic, or a missing
// row.
//
// The fixture is a home directory with NOTHING in it, which is the strongest version
// of the claim: every one of the five state functions is reached with every file it
// reads absent. The assertion is on the count of rows, not on the words, so it fails
// for a dropped row rather than only for a changed verdict.
func TestConfigureStatusTable_OneAgentsMissingConfigDoesNotBreakTheTable(t *testing.T) {
	home := t.TempDir()
	// findOpenCode consults PATH before ~/.opencode/bin, so an empty PATH with an
	// empty HOME is what makes the opencode row's "not installed" arm reachable here.
	t.Setenv("PATH", "")
	t.Setenv("HOME", home)
	// CODEX_HOME would otherwise point codex at a real file on the machine running
	// the test, which is the one path defaultCodexEnvPath honours over HOME.
	t.Setenv(codexHomeEnv, filepath.Join(home, ".codex"))
	// Paired with t.Setenv for the reason above: the cleanup restores whatever the
	// developer running the test had exported.
	t.Setenv(bobShellEnvVar, "")
	if err := os.Unsetenv(bobShellEnvVar); err != nil {
		t.Fatal(err)
	}

	rows := make([]configureStatus, 0, 5)
	for _, fn := range configureStatusFuncs() {
		rows = append(rows, fn(home))
	}
	if len(rows) != 5 {
		t.Fatalf("got %d rows, want 5 — a failing agent dropped its row", len(rows))
	}
	for _, r := range rows {
		if r.Agent == "" {
			t.Errorf("a row has no agent name: %+v", r)
		}
		// Every row must say something. A blank detail on an agent that could not be
		// read is the output someone cannot act on, which is the whole complaint
		// #1164 opens with.
		if r.Detail == "" {
			t.Errorf("%s has an empty detail; nothing says why", r.Agent)
		}
		// Nothing on an empty machine may claim to be routed through Cortex.
		if r.State == configureOn {
			t.Errorf("%s reports enabled against an empty home: %s", r.Agent, r.Detail)
		}
	}

	// And it renders, rather than only building.
	var out bytes.Buffer
	writeConfigureStatusTable(rows, &out)
	for _, agent := range []string{"claude-code", "bob", "bobshell", "opencode", "codex"} {
		if !strings.Contains(out.String(), agent) {
			t.Errorf("table omits %q:\n%s", agent, out.String())
		}
	}
}

// The table must cover every agent `agentop configure <x> status` covers, which is the
// literal ask of #1164 — and the way that stops being true is someone adding a sixth
// agent to runConfigure and configureUsage without adding it here.
//
// Asserted against configureUsage rather than a second hardcoded list, because a
// hardcoded list is the thing that goes stale: this fails when the usage text and the
// table disagree, which is exactly when a user would be misled.
func TestConfigureStatusTable_CoversEveryAgentInUsage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PATH", "")
	t.Setenv("HOME", home)
	t.Setenv(codexHomeEnv, filepath.Join(home, ".codex"))

	covered := map[string]bool{}
	for _, fn := range configureStatusFuncs() {
		covered[fn(home).Agent] = true
	}

	// The "Agents:" block of configureUsage lists one agent per line, name first.
	_, agents, ok := strings.Cut(configureUsage, "Agents:\n")
	if !ok {
		t.Fatal("configureUsage has no \"Agents:\" block; this test reads the list from there")
	}
	listed := 0
	for _, line := range strings.Split(agents, "\n") {
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
			// Continuation lines are indented further; a blank line ends the block.
			if strings.TrimSpace(line) == "" && listed > 0 {
				break
			}
			continue
		}
		name := strings.Fields(line)[0]
		listed++
		if !covered[name] {
			t.Errorf("configureUsage lists %q but the status table has no row for it", name)
		}
	}
	if listed == 0 {
		t.Fatal("parsed no agent names out of configureUsage")
	}
	if listed != len(covered) {
		t.Errorf("usage lists %d agents, the table covers %d: %v", listed, len(covered), covered)
	}
}

// The new command's dispatch: `configure status` reaches the table, `--help` is a
// successful answer, and a stray argument is a usage error rather than being read as
// an agent name.
//
// "status" occupying a slot in the agent namespace is the design decision worth
// pinning: the five agents all take a `status` ACTION, so `configure status` with the
// agent left out is the natural spelling of "all of them", and it is safe because
// nothing is configured by being called "status".
func TestRunConfigure_StatusDispatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PATH", "")
	t.Setenv("HOME", home)
	t.Setenv(codexHomeEnv, filepath.Join(home, ".codex"))

	t.Run("prints the table", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runConfigure([]string{"status"}, &out, &errb); code != 0 {
			t.Fatalf("exit = %d, want 0: %s", code, errb.String())
		}
		if !strings.Contains(out.String(), "HARNESS") {
			t.Errorf("stdout is not the table:\n%s", out.String())
		}
		// A report goes to stdout so it is pipeable, the same split every per-agent
		// status uses.
		if errb.Len() != 0 {
			t.Errorf("stderr not empty: %q", errb.String())
		}
	})

	// Exit 0 whatever the rows say. A non-zero status for "nothing is configured"
	// would make the command unusable in a pipeline, and would be a strange thing to
	// call a failure: an empty machine is a correct answer.
	t.Run("exit 0 with nothing configured", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runConfigure([]string{"status"}, &out, &errb); code != 0 {
			t.Errorf("exit = %d, want 0 — a report is not a verdict", code)
		}
	})

	t.Run("help", func(t *testing.T) {
		for _, arg := range []string{"-h", "--help", "help"} {
			var out, errb bytes.Buffer
			if code := runConfigure([]string{"status", arg}, &out, &errb); code != 0 {
				t.Errorf("%s: exit = %d, want 0", arg, code)
			}
			if !strings.Contains(out.String(), "agentop configure —") {
				t.Errorf("%s: usage not on stdout:\n%s", arg, out.String())
			}
		}
	})

	// It takes no flags of its own, deliberately: five agents read five different
	// files and their per-agent overrides do not compose into one flag set — `--config`
	// alone means Cortex's config for three of them and the agent's own for a fourth.
	// So a flag must be a usage error that names itself, not a silently ignored one.
	t.Run("unexpected argument", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runConfigure([]string{"status", "--settings", "/tmp/x"}, &out, &errb); code != 2 {
			t.Errorf("exit = %d, want 2", code)
		}
		if !strings.Contains(errb.String(), "--settings") {
			t.Errorf("stderr does not quote the argument: %q", errb.String())
		}
	})

	// `status` must not have become an agent name in the error path: a typo still has
	// to be refused with the list of real agents.
	t.Run("a typo is still an error", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runConfigure([]string{"statuss"}, &out, &errb); code != 2 {
			t.Errorf("exit = %d, want 2", code)
		}
		if !strings.Contains(errb.String(), "unknown agent") {
			t.Errorf("stderr = %q", errb.String())
		}
	})
}

// The usage text has to mention the new command, or it is undiscoverable: this
// command exists because the per-agent verbs were hard to remember, and a verb absent
// from the help has the same problem one step further out.
func TestConfigureUsage_MentionsTheStatusTable(t *testing.T) {
	if !strings.Contains(configureUsage, "agentop configure status") {
		t.Error("configureUsage does not show `agentop configure status` in its Usage block")
	}
	// The three-state vocabulary is explained there too: "uncertain" is the word that
	// needs explaining, since a reader's first guess is that it means "broken".
	if !strings.Contains(configureUsage, "uncertain") {
		t.Error("configureUsage does not explain the \"uncertain\" verdict")
	}
}

// The per-agent status commands must print the verdict the table prints.
//
// This is the point of the refactor: each agent's `status` paragraph and its table row
// now come from one value, so they cannot drift. Asserted by driving both and checking
// they agree, rather than by comparing strings — the two surfaces word things
// differently on purpose, and what must match is the verdict underneath.
func TestStatusAndTableAgree(t *testing.T) {
	t.Run("claude-code enabled", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(`{"env": {`)
		for i, k := range managedKeys {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`"` + k + `": "x"`)
		}
		b.WriteString(`}}`)
		settings, _ := fixture(t, b.String())

		var out bytes.Buffer
		if code := claudeCodeStatus(settings, &out); code != 0 {
			t.Fatalf("exit = %d", code)
		}
		said := strings.Contains(out.String(), "enabled in ") && !strings.Contains(out.String(), "not fully enabled")
		if got := claudeCodeState(settings).State; (got == configureOn) != said {
			t.Errorf("state says %s but the paragraph says otherwise:\n%s", got, out.String())
		}
	})

	t.Run("claude-code partial", func(t *testing.T) {
		settings, _ := fixture(t, `{"env": {"`+envProxy+`": "http://127.0.0.1:47600"}}`)
		var out bytes.Buffer
		if code := claudeCodeStatus(settings, &out); code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if !strings.Contains(out.String(), "not fully enabled") {
			t.Errorf("paragraph does not report a partial state:\n%s", out.String())
		}
		if got := claudeCodeState(settings).State; got != configurePartial {
			t.Errorf("state = %s, want %s", got, configurePartial)
		}
	})

	// bob's verdict line is a fixed string, so this one can assert the exact pairing:
	// only configureOn may produce bobStatusYes. The drifted-loopback case is the
	// trap — it is configurePartial in the table and has always been bobStatusNo in
	// the paragraph, and both of those are correct.
	t.Run("bob", func(t *testing.T) {
		for _, tc := range []struct {
			name, settings string
			wantYes        bool
		}{
			{"ours", `{"http.proxy": "http://127.0.0.1:47600"}`, true},
			{"foreign", `{"http.proxy": "http://proxy.corp.example.com:3128"}`, false},
			{"drifted", `{"http.proxy": "http://127.0.0.1:47599"}`, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				settings, cfg := fixture(t, tc.settings)
				want, ca := bobWanted(cfg, t.TempDir())
				var out bytes.Buffer
				if code := bobStatus(settings, cfg, want, ca, &out); code != 0 {
					t.Fatalf("exit = %d", code)
				}
				first := strings.SplitN(out.String(), "\n", 2)[0]
				if (first == bobStatusYes) != tc.wantYes {
					t.Errorf("first line = %q, wantYes = %v", first, tc.wantYes)
				}
				if (bobState(settings, cfg, want).State == configureOn) != tc.wantYes {
					t.Errorf("state and paragraph disagree for %s", tc.name)
				}
			})
		}
	})

	// bobshell's paragraph branch is now taken on the state, so the variable being
	// visible and the paragraph admitting it must stay in step.
	t.Run("bobshell", func(t *testing.T) {
		t.Setenv(bobShellEnvVar, "1")
		var out bytes.Buffer
		if code := bobShellStatus(&out); code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if !strings.Contains(out.String(), bobShellEnvVar+"=1") {
			t.Errorf("paragraph does not show the variable:\n%s", out.String())
		}
		if got := bobShellState().State; got != configurePartial {
			t.Errorf("state = %s, want %s", got, configurePartial)
		}
	})
}
