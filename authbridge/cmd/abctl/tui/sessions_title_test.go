package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
)

// A harvested title reaches the row; an id nobody harvested renders empty.
//
// The empty case is the one worth pinning. "" is the deliberate answer for a session the
// harvester has not seen, and the tempting alternatives — the em dash UPDATED uses for
// "unknown", or echoing the id — would both read as a fact about the session when they are
// only a fact about whether anyone has run the harvester.
func TestSessionsPane_TitleFromSessionsData(t *testing.T) {
	m := newTitleModel(t, map[string]SessionMetadata{
		"known": {Title: "refactor the parser"},
	}, "known", "unharvested")
	m.rebuildSessionsTable()

	if got := sessionsCell(t, m, titleRow(t, m, "known"), "TITLE"); got != "refactor the parser" {
		t.Errorf("TITLE for a harvested session = %q, want %q", got, "refactor the parser")
	}
	if got := sessionsCell(t, m, titleRow(t, m, "unharvested"), "TITLE"); got != "" {
		t.Errorf("TITLE for an unharvested session = %q, want empty", got)
	}
}

// A nil sessionsData must render, not panic — the absent-file case, which is every user who
// has never run the harvester, i.e. the default state of the feature. A guard-free
// m.sessionsData[id] is what makes this work, so a future author adding a len() check has a
// test saying it was never needed.
func TestSessionsPane_NilSessionsDataRendersEmptyTitles(t *testing.T) {
	m := newTitleModel(t, nil, "s1")
	m.rebuildSessionsTable()

	if got := sessionsCell(t, m, titleRow(t, m, "s1"), "TITLE"); got != "" {
		t.Errorf("TITLE with no metadata loaded = %q, want empty", got)
	}
}

// The cached-only loop carries the cell too.
//
// Two loops build these rows and it is the second that gets forgotten: table.Row is a
// []string, so a row one cell short is not a compile error and not a panic — the cells after
// the gap simply shift left and every later column shows its neighbour's value. That is the
// post-restart path (#870), where the server lists nothing and these are the only rows there
// are.
func TestSessionsPane_CachedOnlyRowCarriesTheTitle(t *testing.T) {
	m := newTitleModel(t, map[string]SessionMetadata{"cachedsess": {Title: "an old session"}})
	m.events["cachedsess"] = []pipeline.SessionEvent{{Host: "api.example.com"}}
	m.rebuildSessionsTable()

	row := titleRow(t, m, "cachedsess")
	if got := len(row); got != len(m.sessionsTbl.Columns()) {
		t.Fatalf("cached-only row has %d cells for %d columns: %v", got, len(m.sessionsTbl.Columns()), row)
	}
	if got := sessionsCell(t, m, row, "TITLE"); got != "an old session" {
		t.Errorf("TITLE on a cached-only row = %q, want %q", got, "an old session")
	}
	// Asserted alongside TITLE because a short row shows up here first: ACTIVE is the last
	// column, so a missing cell earlier in the row is what makes this one wrong.
	if got := sessionsCell(t, m, row, "ACTIVE"); got != "cached" {
		t.Errorf("cached-only marker = %q, want %q — cells have shifted: %v", got, "cached", row)
	}
}

// LoadSessionMetadata reads what the harvester writes, and answers empty for every failure.
//
// The absent case is the contract that matters: it is the state of every machine until
// someone runs `abctl experimental read-claude-sessions`, and it must be silent rather than
// an error the viewer has to render.
func TestLoadSessionMetadata(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "good.json")
	body, err := json.Marshal(map[string]SessionMetadata{"abc": {Title: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(good, body, 0o600); err != nil {
		t.Fatal(err)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	null := filepath.Join(dir, "null.json")
	if err := os.WriteFile(null, []byte("null\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		path  string
		title string // "" means "expect no entry for abc"
	}{
		{"harvested", good, "hello"},
		{"absent", filepath.Join(dir, "nope.json"), ""},
		{"corrupt", bad, ""},
		{"json null", null, ""},
		{"empty path", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := LoadSessionMetadata(tc.path)
			if got == nil {
				t.Fatal("returned a nil map; callers index it without a guard")
			}
			if got["abc"].Title != tc.title {
				t.Errorf("title = %q, want %q", got["abc"].Title, tc.title)
			}
		})
	}
}

// newTitleModel builds a sessions-pane model with the given metadata and server-listed ids.
func newTitleModel(t *testing.T, meta map[string]SessionMetadata, ids ...string) *model {
	t.Helper()
	sessions := make([]session.SessionSummary, 0, len(ids))
	for _, id := range ids {
		sessions = append(sessions, session.SessionSummary{ID: id})
	}
	return &model{
		pane:         paneSessions,
		width:        200,
		height:       40,
		sessions:     sessions,
		events:       map[string][]pipeline.SessionEvent{},
		sessionsData: meta,
		sessionsTbl:  newSessionsTable(),
	}
}

// titleRow finds the row for one session id. Keyed on cell 0, which is the id by contract —
// sessionsColumns' comment and selectedSessionID both depend on that.
func titleRow(t *testing.T, m *model, id string) table.Row {
	t.Helper()
	for _, r := range m.sessionsTbl.Rows() {
		if r[0] == id {
			return r
		}
	}
	t.Fatalf("no row for session %q", id)
	return nil
}

// A path title keeps its END, which is the half that says which session it is.
//
// Every session on a machine shares the leading directories, so right-truncation — what
// bubbles does to any cell it is handed whole — spends the entire column on the shared prefix.
// This is the change's whole point, so it is asserted on both the value and the marker: the
// leading "…" is what tells a reader the front was cut rather than that the path starts there.
func TestSessionsPane_PathTitleTruncatesFromTheLeft(t *testing.T) {
	const long = "/Users/someone/src/cortex/.worktrees/claudesessions/authbridge"
	m := newTitleModel(t, map[string]SessionMetadata{"s1": {Title: long}}, "s1")
	m.sessionsTbl.SetColumns(sessionsColumnsFor(116)) // TITLE at its declared 24
	m.rebuildSessionsTable()

	got := sessionsCell(t, m, titleRow(t, m, "s1"), "TITLE")
	if !strings.HasPrefix(got, "…") {
		t.Errorf("TITLE = %q, want a leading ellipsis marking the cut front", got)
	}
	if !strings.HasSuffix(got, "authbridge") {
		t.Errorf("TITLE = %q, want the tail of the path — the distinguishing end", got)
	}
	if n := len([]rune(got)); n > 24 {
		t.Errorf("TITLE is %d runes, wider than the 24-column cell: %q", n, got)
	}
}

// Prose is not truncated from the left: it reads from the beginning.
func TestSessionsPane_ProseTitleTruncatesFromTheRight(t *testing.T) {
	const prose = "Investigate the flaky reloader debounce test"
	m := newTitleModel(t, map[string]SessionMetadata{"s1": {Title: prose}}, "s1")
	m.sessionsTbl.SetColumns(sessionsColumnsFor(116))
	m.rebuildSessionsTable()

	// Handed to bubbles whole: it is under no obligation to arrive pre-truncated, because
	// bubbles' own right-truncation is already the correct side for prose.
	if got := sessionsCell(t, m, titleRow(t, m, "s1"), "TITLE"); got != prose {
		t.Errorf("TITLE = %q, want the untouched title %q", got, prose)
	}
}

// TITLE takes the slack on a wide terminal, and nothing else changes.
//
// The declared widths are simultaneously the narrow-terminal budget and, because
// fitTableColumns only shrinks, the wide-terminal ceiling. Growing TITLE is what stops a
// 200-column terminal from showing a 24-character path with 84 columns unused — but it must
// come only from slack, so the id keeps its full 40 either way.
func TestSessionsColumnsFor_TitleGrowsIntoSlackOnly(t *testing.T) {
	width := func(cols []table.Column, title string) int {
		t.Helper()
		for _, c := range cols {
			if c.Title == title {
				return c.Width
			}
		}
		t.Fatalf("no %q column", title)
		return 0
	}
	declared := tableWidth(sessionsColumns())

	at116 := sessionsColumnsFor(declared)
	if got := width(at116, "TITLE"); got != sessionsTitleWidth {
		t.Errorf("TITLE at the declared width = %d, want %d — no slack to take", got, sessionsTitleWidth)
	}

	at200 := sessionsColumnsFor(200)
	if got, want := width(at200, "TITLE"), sessionsTitleWidth+(200-declared); got != want {
		t.Errorf("TITLE at 200 = %d, want %d (declared %d + all the slack)", got, want, declared)
	}
	if got := width(at200, "ID"); got != 40 {
		t.Errorf("ID at 200 = %d, want its declared 40 — growth must come from slack, not a neighbour", got)
	}
	if got := tableWidth(at200); got != 200 {
		t.Errorf("rendered width at 200 = %d, want exactly 200", got)
	}

	// The narrow path is unchanged: still fitTableColumns' shrink, with no growth applied.
	if got := tableWidth(sessionsColumnsFor(80)); got != 80 {
		t.Errorf("rendered width at 80 = %d, want 80", got)
	}
}

// The three session-scoped headers name the session, keeping the id in every case.
//
// The id stays because it is what /v1/sessions is keyed by and what a bug report has to
// quote — and because a harvested title is a directory, so two sessions in one checkout
// share it and a title alone would not say which is on screen.
func TestSessionLabelAndHeader(t *testing.T) {
	m := newTitleModel(t, map[string]SessionMetadata{"id-1": {Title: "fix the parser"}})
	m.width = 200

	if got, want := m.sessionLabel("id-1"), "fix the parser (id-1)"; got != want {
		t.Errorf("sessionLabel = %q, want %q", got, want)
	}
	if got, want := m.sessionLabel("id-2"), "id-2"; got != want {
		t.Errorf("sessionLabel for an unharvested session = %q, want the bare id %q", got, want)
	}
	if got, want := m.sessionHeader("id-1", ""), "abctl · fix the parser (id-1)"; got != want {
		t.Errorf("events header = %q, want %q", got, want)
	}
	if got, want := m.sessionHeader("id-1", "event"), "abctl · fix the parser (id-1) · event"; got != want {
		t.Errorf("detail header = %q, want %q", got, want)
	}
}

// A full UUID is not clipped on a terminal with room for it.
//
// The regression this pins: the events header truncated at a fixed 36 and the detail header at
// 24, so "0e61b82d-8578-4d16-a18e…" appeared on a 200-column screen. 36 is exactly a UUID's
// length, which is why the old constant looked right until a title was added in front of it.
func TestSessionHeader_DoesNotClipWhenThereIsRoom(t *testing.T) {
	const id = "0e61b82d-8578-4d16-a18e-1d085ca678fc"
	m := newTitleModel(t, nil)
	m.width = 200

	for _, suffix := range []string{"", "event"} {
		got := m.sessionHeader(id, suffix)
		if !strings.Contains(got, id) {
			t.Errorf("header %q dropped part of the id %q", got, id)
		}
		if strings.Contains(got, "…") {
			t.Errorf("header %q clipped on a 200-column terminal", got)
		}
	}
}

// A header too long for the terminal is clipped to the terminal, from the left.
//
// Two things this pins that the short-title cases above cannot. First, the clip tracks
// m.width: the constants it replaced were 36 and 24, and a fixed 36 leaves a titled session
// showing only its title with the id — the part a bug report has to quote — cut off entirely.
// Second, the surviving end is the RIGHT one, so what a narrow terminal keeps is the id and
// the leaf of the path rather than the "abctl · " that is on every screen anyway.
func TestSessionHeader_ClipsToTerminalWidthFromTheLeft(t *testing.T) {
	const id = "0e61b82d-8578-4d16-a18e-1d085ca678fc"
	const title = "/Users/someone/src/cortex/.worktrees/claudesessions/authbridge/cmd/abctl"
	m := newTitleModel(t, map[string]SessionMetadata{id: {Title: title}})

	// 100 and below: the full header is 119 columns wide, so 120 is genuinely NOT clipped and
	// asserting an ellipsis there would be asserting a bug.
	for _, w := range []int{100, 80, 60} {
		m.width = w
		got := m.sessionHeader(id, "")
		if n := lipgloss.Width(got); n > w {
			t.Errorf("width %d: header is %d columns wide: %q", w, n, got)
		}
		// The id survives at every width a real terminal has. It is the reason the label
		// keeps its right end rather than its left.
		if !strings.HasSuffix(got, "("+id+")") {
			t.Errorf("width %d: header lost the id: %q", w, got)
		}
		if !strings.HasPrefix(got, "abctl · …") {
			t.Errorf("width %d: want a left-clipped label after the prefix, got %q", w, got)
		}
	}
}

// The usage header names the session without the "session: " prefix, and fits the terminal.
//
// The prefix went because the pane is only ever reached by pressing u on a session, so it
// restated what the operator had just selected — and it cost nine columns on a line that
// already overflowed an 80-column terminal by 13 with a bare id. Clipping is what keeps
// adding a title from making that overflow worse rather than better.
func TestRenderUsage_ScopeIsTheSessionLabel(t *testing.T) {
	const id = "4d0d159b-3c04-4646-8ed3-46bdb5a7a9ae"
	const title = "/Users/someone/src/cortex/.worktrees/claudesessions/authbridge/cmd/abctl"
	m := newTitleModel(t, map[string]SessionMetadata{id: {Title: title}})
	m.usage = usageState{session: id}

	m.width = 200
	first := strings.Split(m.renderUsage(m.width, 20), "\n")[0]
	if strings.Contains(first, "session: ") {
		t.Errorf("usage header still carries the redundant prefix: %q", first)
	}
	if !strings.Contains(first, title) || !strings.Contains(first, id) {
		t.Errorf("usage header should name title and id at 200 columns: %q", first)
	}

	// At 80 the label has to give way, and the id is what must survive it.
	m.width = 80
	narrow := strings.Split(m.renderUsage(m.width, 20), "\n")[0]
	if n := lipgloss.Width(narrow); n > 80 {
		t.Errorf("usage header is %d columns at width 80: %q", n, narrow)
	}
	if !strings.Contains(narrow, "46bdb5a7a9ae)") {
		t.Errorf("usage header dropped the end of the id at width 80: %q", narrow)
	}

	// No session selected keeps its own wording, which is not a label at all.
	m.usage = usageState{}
	if all := strings.Split(m.renderUsage(m.width, 20), "\n")[0]; !strings.Contains(all, "all sessions") {
		t.Errorf("unscoped usage header = %q, want it to say all sessions", all)
	}
}
