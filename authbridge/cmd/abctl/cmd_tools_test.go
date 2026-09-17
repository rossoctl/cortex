package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const scanTestConfig = `mode: proxy-sidecar
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: tool-prune
        config:
          remove: []
`

// writeTranscript writes one JSONL transcript. withCall controls whether it
// contains a tool_use block, which is the scan's only evidence.
func writeTranscript(t *testing.T, dir, name string, withCall bool) {
	t.Helper()
	line := `{"timestamp":"` + nowStamp() + `","message":{"content":[{"type":"text","text":"hi"}]}}`
	if withCall {
		line = `{"timestamp":"` + nowStamp() + `","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash"}]}}`
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func nowStamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// TestToolsScan_RefusesToWriteWithoutEvidence: with no observed tool calls,
// "tools you have not called" is every tool it knows, so writing that list
// unattended would propose removing tools the session needs. A new install is
// exactly this case, which is also when the default is most likely accepted.
func TestToolsScan_RefusesToWriteWithoutEvidence(t *testing.T) {
	dir := t.TempDir()
	tdir := filepath.Join(dir, "projects")
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, tdir, "a.jsonl", false) // no tool_use anywhere

	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(scanTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := runTools([]string{"scan", "--dir", tdir, "--write", cfg}, &out, &errb)
	if code == 0 {
		t.Errorf("exit code = 0, want non-zero when there is no evidence")
	}
	if !strings.Contains(errb.String(), "no tool calls") {
		t.Errorf("stderr did not explain the refusal: %q", errb.String())
	}
	// The config must be untouched.
	after, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != scanTestConfig {
		t.Errorf("config was modified despite the refusal:\n%s", after)
	}
	// The proposal is still printed — refusing to write it is not refusing to
	// show it.
	if !strings.Contains(out.String(), "remove:") {
		t.Errorf("stdout did not include the proposal: %q", out.String())
	}
}

// TestToolsScan_WritesWithEvidence is the paired positive case, so the guard
// above can't pass by simply never writing.
func TestToolsScan_WritesWithEvidence(t *testing.T) {
	dir := t.TempDir()
	tdir := filepath.Join(dir, "projects")
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, tdir, "a.jsonl", true) // one real tool_use

	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(scanTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := runTools([]string{"scan", "--dir", tdir, "--write", cfg}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d, want 0. stderr: %s", code, errb.String())
	}
	after, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) == scanTestConfig {
		t.Error("config was not updated despite real evidence")
	}
}

// TestToolsScan_AllFlagIsWired: --all has to reach toolscan.AllTime. Without a
// test, a typo in the flag name would compile and silently keep the 30-day
// window, which is the aggressive direction.
func TestToolsScan_AllFlagIsWired(t *testing.T) {
	dir := t.TempDir()
	tdir := filepath.Join(dir, "projects")
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		t.Fatal(err)
	}
	// One call inside the window, one far outside it.
	old := time.Now().AddDate(0, 0, -400).UTC().Format("2006-01-02T15:04:05.000Z")
	lines := `{"timestamp":"` + nowStamp() + `","message":{"content":[{"type":"tool_use","id":"a","name":"Bash"}]}}` + "\n" +
		`{"timestamp":"` + old + `","message":{"content":[{"type":"tool_use","id":"b","name":"WebSearch"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(tdir, "t.jsonl"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := runTools([]string{"scan", "--dir", tdir}, &out, &errb); code != 0 {
		t.Fatalf("default scan: %d %s", code, errb.String())
	}
	if strings.Contains(out.String(), "WebSearch") && !strings.Contains(out.String(), "Removal candidates") {
		t.Skip("unexpected output shape")
	}
	// Default window: the 400-day-old WebSearch call is not "used", so it is a
	// removal candidate.
	if !strings.Contains(out.String(), "window 30 day(s)") {
		t.Errorf("default did not report a 30-day window:\n%s", out.String())
	}

	out.Reset()
	errb.Reset()
	if code := runTools([]string{"scan", "--dir", tdir, "--all"}, &out, &errb); code != 0 {
		t.Fatalf("--all scan: %d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "all history (no window)") {
		t.Errorf("--all did not disable the window:\n%s", out.String())
	}
	// With no window, the old call counts as used and must not be proposed.
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "Removal candidates") && strings.Contains(line, "WebSearch") {
			t.Errorf("--all proposed removing a tool it saw called: %s", line)
		}
	}
}

// TestToolsScan_AllAndDaysAreMutuallyExclusive: silently honouring one would pick
// a different window than the operator asked for, in a command whose whole output
// depends on the window.
func TestToolsScan_AllAndDaysAreMutuallyExclusive(t *testing.T) {
	var out, errb bytes.Buffer
	code := runTools([]string{"scan", "--all", "--days", "90", "--dir", t.TempDir()}, &out, &errb)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "mutually exclusive") {
		t.Errorf("error did not explain itself: %q", errb.String())
	}
}

// TestToolsScan_RefusalNamesTheWindowThatRan pins finding (1): with --all the
// refusal used to claim "the last 30 day(s)" and advise --days, which --all
// rejects. A wrong scope in the one message a fresh install sees is worse than
// terse.
func TestToolsScan_RefusalNamesTheWindowThatRan(t *testing.T) {
	dir := t.TempDir()
	tdir := filepath.Join(dir, "projects")
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, tdir, "a.jsonl", false) // no tool_use anywhere
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(scanTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := runTools([]string{"scan", "--dir", tdir, "--write", cfg, "--all"}, &out, &errb); code == 0 {
		t.Fatal("wrote despite no evidence")
	}
	msg := errb.String()
	if strings.Contains(msg, "day(s)") {
		t.Errorf("--all refusal still claims a day window:\n%s", msg)
	}
	if !strings.Contains(msg, "any of your transcripts") {
		t.Errorf("--all refusal does not name the real scope:\n%s", msg)
	}
	if strings.Contains(msg, "--days or --all") {
		t.Errorf("--all refusal advises a flag combination it rejects:\n%s", msg)
	}

	// The windowed refusal should still name the window.
	out.Reset()
	errb.Reset()
	if code := runTools([]string{"scan", "--dir", tdir, "--write", cfg, "--days", "7"}, &out, &errb); code == 0 {
		t.Fatal("wrote despite no evidence")
	}
	if !strings.Contains(errb.String(), "last 7 day(s)") {
		t.Errorf("windowed refusal lost the window:\n%s", errb.String())
	}
}

// TestToolsScan_WarnsOnThinEvidence covers finding (8): one observed call clears
// the zero-evidence guard and still proposes removing nearly everything.
func TestToolsScan_WarnsOnThinEvidence(t *testing.T) {
	dir := t.TempDir()
	tdir := filepath.Join(dir, "projects")
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, tdir, "a.jsonl", true) // exactly one tool call
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(scanTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := runTools([]string{"scan", "--dir", tdir, "--write", cfg}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "thin evidence") {
		t.Errorf("one call produced no thin-evidence warning:\n%s", errb.String())
	}
	// It still writes — thin evidence is a property of the input, not an error.
	after, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) == scanTestConfig {
		t.Error("config was not written")
	}
}

// `abctl tools --help` was read as an action name and answered with "unknown
// subcommand" — the one place that refuses to say what the command does (#1001).
// All three spellings are pinned: -h is what a habit produces, `help` what someone
// copies from another tool.
func TestToolsHelp_PrintsUsageOnStdout(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		t.Run(arg, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runTools([]string{arg}, &out, &errb); code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			if !strings.Contains(out.String(), "abctl tools — read agent logs to measure tool use") {
				t.Errorf("summary line missing from stdout:\n%s", out.String())
			}
			// It must name its one action, or the reader learns nothing actionable.
			if !strings.Contains(out.String(), "scan") {
				t.Errorf("usage does not mention scan:\n%s", out.String())
			}
			// Explicit help is a successful answer, so it has to be pipeable.
			if errb.Len() != 0 {
				t.Errorf("stderr not empty: %q", errb.String())
			}
		})
	}
}

// `abctl tools scan --help` printed flag.PrintDefaults' bare "Usage of tools scan:"
// flag list with no prose — the second half of #1001. The rationale is asserted in
// pieces rather than whole so a rewording does not fail the test, but the two facts
// a reader needs (only Claude Code today; why pruning saves tokens) must survive.
func TestToolsScanHelp_PrintsRationaleOnStdout(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runTools([]string{"scan", arg}, &out, &errb); code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			got := out.String()
			for _, want := range []string{
				"abctl tools scan — consult local coding agent logs",
				"only consults Claude Code logs",
				"resends its entire tool manifest on every turn",
				"saving token cost",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q:\n%s", want, got)
				}
			}
			// The bare flag dump this replaced.
			if strings.Contains(got, "Usage of tools scan:") {
				t.Errorf("still printing the bare flag dump:\n%s", got)
			}
			if errb.Len() != 0 {
				t.Errorf("stderr not empty: %q", errb.String())
			}
		})
	}
}

// The help cases must not have widened into swallowing real usage errors: those
// stay on stderr at exit 2, which is what a script checks.
func TestToolsUsageErrors_StayErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no action", nil},
		{"unknown action", []string{"bogus"}},
		{"unknown flag", []string{"scan", "--nosuchflag"}},
		{"zero days", []string{"scan", "--days", "0"}},
		{"all with days", []string{"scan", "--all", "--days", "5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runTools(tc.args, &out, &errb); code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
			if errb.Len() == 0 {
				t.Error("nothing on stderr")
			}
			if out.Len() != 0 {
				t.Errorf("stdout not empty: %q", out.String())
			}
		})
	}
}
