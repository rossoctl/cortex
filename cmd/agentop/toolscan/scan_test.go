package toolscan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// entry renders one transcript line containing a tool_use block.
func entry(ts time.Time, id, name string) string {
	return fmt.Sprintf(
		`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":%q,"input":{}}]}}`,
		ts.Format(time.RFC3339), id, name)
}

func writeTranscript(t *testing.T, dir, name string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func contains(v []string, s string) bool {
	for _, x := range v {
		if x == s {
			return true
		}
	}
	return false
}

// TestScan_DeduplicatesByToolUseID: a transcript is rewritten on every resume,
// so the same tool_use block appears many times. Counting raw occurrences would
// make a heavily-resumed session look busier than it was — and, worse, could
// make a tool look "used" on the strength of one ancient call replayed often.
func TestScan_DeduplicatesByToolUseID(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTranscript(t, dir, "a.jsonl",
		entry(now, "toolu_1", "WebFetch"),
		entry(now, "toolu_1", "WebFetch"), // same id, replayed
		entry(now, "toolu_2", "WebFetch"),
	)
	res, err := Scan(dir, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.CallCounts["WebFetch"]; got != 2 {
		t.Errorf("WebFetch counted %d times, want 2 (ids deduplicated)", got)
	}
}

// TestScan_WindowsByTimestamp: a tool called only outside the window must show
// up as a candidate, which is the entire point of --days.
func TestScan_WindowsByTimestamp(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "a.jsonl",
		entry(time.Now().AddDate(0, 0, -90), "toolu_old", "NotebookEdit"),
		entry(time.Now(), "toolu_new", "WebFetch"),
	)
	res, err := Scan(dir, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	if contains(res.Called, "NotebookEdit") {
		t.Error("NotebookEdit was called 90 days ago; outside a 30-day window it is not 'called'")
	}
	if !contains(res.Candidates, "NotebookEdit") {
		t.Errorf("NotebookEdit should be a candidate: %v", res.Candidates)
	}
	if !contains(res.Called, "WebFetch") {
		t.Errorf("WebFetch is inside the window: %v", res.Called)
	}
	if contains(res.Candidates, "WebFetch") {
		t.Error("a tool called inside the window must never be a candidate")
	}
}

// TestScan_UnknownNamesAreNeverProposed is the safety property. An MCP tool or
// a built-in from a newer Claude Code release is not in the table, so it can
// never be proposed for removal however long it goes unused.
func TestScan_UnknownNamesAreNeverProposed(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "a.jsonl", entry(time.Now(), "toolu_1", "Bash"))
	res, err := Scan(dir, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Candidates {
		if !contains(KnownTools(), c) {
			t.Errorf("candidate %q is not in the known-tool table", c)
		}
	}
	// A tool that does the primary work is not even in the table, so an idle
	// window can't propose it.
	for _, never := range []string{"Read", "Write", "Edit", "Bash", "Grep", "Glob", "TodoWrite", "ExitPlanMode"} {
		if contains(res.Candidates, never) {
			t.Errorf("%q must never be a removal candidate", never)
		}
	}
}

// TestScan_ImpliesWithholdsIndirectlyUsedTools: Agent drives SendMessage, so a
// transcript showing Agent must not propose removing SendMessage even though
// SendMessage never appears by name.
func TestScan_ImpliesWithholdsIndirectlyUsedTools(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "a.jsonl", entry(time.Now(), "toolu_1", "Agent"))
	res, err := Scan(dir, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	if contains(res.Candidates, "SendMessage") {
		t.Error("Agent implies SendMessage; it must be withheld, not proposed")
	}
	if !contains(res.Kept, "SendMessage") {
		t.Errorf("SendMessage should be reported as withheld: %v", res.Kept)
	}
}

func TestScan_KeepFlagWithholds(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "a.jsonl", entry(time.Now(), "toolu_1", "Bash"))
	res, err := Scan(dir, 30, []string{"NotebookEdit", " WebSearch "})
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{"NotebookEdit", "WebSearch"} {
		if contains(res.Candidates, kept) {
			t.Errorf("%q was passed to --keep; must not be proposed", kept)
		}
	}
}

// TestScan_SkipsLinesWithoutToolUse verifies the prefilter does not change
// results — only cost. A transcript of pure text must yield no calls.
func TestScan_SkipsLinesWithoutToolUse(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "a.jsonl",
		`{"type":"user","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`,
	)
	res, err := Scan(dir, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Called) != 0 {
		t.Errorf("Called = %v, want none", res.Called)
	}
	if res.Lines != 0 {
		t.Errorf("Lines = %d, want 0 (prefilter should reject all)", res.Lines)
	}
}

// TestScan_ToleratesMalformedLines: a truncated final line (a crashed session)
// must not abort the scan.
func TestScan_ToleratesMalformedLines(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "a.jsonl",
		entry(time.Now(), "toolu_1", "WebFetch"),
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_2"`,
	)
	res, err := Scan(dir, 30, nil)
	if err != nil {
		t.Fatalf("a malformed line must not fail the scan: %v", err)
	}
	if res.CallCounts["WebFetch"] != 1 {
		t.Errorf("valid line should still be counted: %+v", res.CallCounts)
	}
}

func TestScan_WalksNestedProjectDirs(t *testing.T) {
	root := t.TempDir()
	writeTranscript(t, filepath.Join(root, "proj-a"), "s1.jsonl", entry(time.Now(), "t1", "WebFetch"))
	writeTranscript(t, filepath.Join(root, "proj-b"), "s2.jsonl", entry(time.Now(), "t2", "Monitor"))
	res, err := Scan(root, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 2 {
		t.Errorf("Files = %d, want 2", res.Files)
	}
	if !contains(res.Called, "WebFetch") || !contains(res.Called, "Monitor") {
		t.Errorf("Called = %v, want both", res.Called)
	}
}

func TestYAMLBlock(t *testing.T) {
	r := &Result{Candidates: []string{"NotebookEdit", "WebSearch"}}
	got := r.YAMLBlock()
	want := "      - name: context-guru\n" +
		"        config:\n" +
		"          engine:\n" +
		"            preset: xhigh\n" +
		"            components:\n" +
		"              toolfilter:\n" +
		"                remove: [NotebookEdit, WebSearch]\n"
	if got != want {
		t.Errorf("block is not the context-guru path:\ngot:\n%s\nwant:\n%s", got, want)
	}
	// on_error is intentionally absent: it defaults to enforce, and the remove
	// list is the gate. Emitting a policy line would imply it is the switch.
	if strings.Contains(got, "on_error") {
		t.Errorf("block should not emit an on_error line:\n%s", got)
	}
	empty := (&Result{}).YAMLBlock()
	if !strings.HasSuffix(empty, "              toolfilter:\n                remove: []\n") {
		t.Errorf("no candidates should render an empty list:\n%s", empty)
	}
}

// TestScan_AllTimeIgnoresTheWindow: --all exists because reaching for it via a
// huge --days is obscure, and because the honest answer to "scan everything" must
// not be a magic number. A wider window can only find MORE tools in use, so it
// proposes fewer for removal — the safe direction.
func TestScan_AllTimeIgnoresTheWindow(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTranscript(t, dir, "a.jsonl",
		entry(now.AddDate(0, 0, -400), "toolu_old", "WebSearch"),
		entry(now, "toolu_new", "WebFetch"),
	)

	windowed, err := Scan(dir, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	if contains(windowed.Called, "WebSearch") {
		t.Error("30-day window counted a 400-day-old call as used")
	}
	if !contains(windowed.Candidates, "WebSearch") {
		t.Error("WebSearch should be a removal candidate inside a 30-day window")
	}

	everything, err := Scan(dir, AllTime, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(everything.Called, "WebSearch") {
		t.Errorf("AllTime missed the 400-day-old call; Called = %v", everything.Called)
	}
	if contains(everything.Candidates, "WebSearch") {
		t.Error("AllTime must not propose removing a tool it saw called")
	}
}

// TestScan_LinesCountsOnlyInWindow guards a fix for a genuinely misleading
// readout: Lines used to be incremented before the timestamp filter, so the
// summary printed the same tool-call count for --days 1 and --days 36500 while
// displaying "window N day(s)" beside it. That reads as a broken window.
func TestScan_LinesCountsOnlyInWindow(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTranscript(t, dir, "a.jsonl",
		entry(now.AddDate(0, 0, -400), "toolu_old", "WebSearch"),
		entry(now.AddDate(0, 0, -200), "toolu_mid", "WebFetch"),
		entry(now, "toolu_new", "Bash"),
	)

	for _, tc := range []struct {
		name string
		days int
		want int
	}{
		{"30 days: only the recent call", 30, 1},
		{"300 days: two of three", 300, 2},
		{"all history: every call", AllTime, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Scan(dir, tc.days, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.Lines != tc.want {
				t.Errorf("Lines = %d, want %d", res.Lines, tc.want)
			}
		})
	}
}

// TestSummary_AllTimeSaysSoInsteadOfPrintingAZeroWindow: with no window there is
// no meaningful "since" date, and printing "window 0 day(s) since 0001-01-01"
// would look like a bug.
func TestSummary_AllTimeSaysSoInsteadOfPrintingAZeroWindow(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "a.jsonl", entry(time.Now(), "toolu_1", "Bash"))
	res, err := Scan(dir, AllTime, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := res.Summary(AllTime)
	if !strings.Contains(got, "all history") {
		t.Errorf("summary does not say the scan was unbounded: %q", got)
	}
	if strings.Contains(got, "0001-01-01") || strings.Contains(got, "window 0 day") {
		t.Errorf("summary leaked the zero window: %q", got)
	}
}
