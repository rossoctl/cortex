package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/authbridge/authlib/observe/claude"
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
	// Asserted alongside TITLE because a short row shows up here first: a missing cell earlier
	// in the row is what makes this one wrong. The marker rides in UPDATED since ACTIVE was
	// replaced by CONTEXT(1M).
	if got := strings.TrimSpace(sessionsCell(t, m, row, "UPDATED")); got != cachedMarker {
		t.Errorf("cached-only marker = %q, want %q — cells have shifted: %v", got, cachedMarker, row)
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
	// THE MODEL's width, not a SetColumns call: rebuildSessionsTable recomputes the header
	// from m.width and passes the resulting TITLE width into the cell, so a header installed
	// behind its back is ignored. Set to the declared total, where TITLE sits at its floor
	// with no slack to grow into — at the fixture's default 200 it grows past the path and
	// there is nothing to truncate.
	m.width = tableWidth(sessionsColumns())
	m.rebuildSessionsTable()

	got := sessionsCell(t, m, titleRow(t, m, "s1"), "TITLE")
	if !strings.HasPrefix(got, "…") {
		t.Errorf("TITLE = %q, want a leading ellipsis marking the cut front", got)
	}
	if !strings.HasSuffix(got, "authbridge") {
		t.Errorf("TITLE = %q, want the tail of the path — the distinguishing end", got)
	}
	if n := lipgloss.Width(got); n > sessionsTitleWidth {
		t.Errorf("TITLE is %d columns, wider than the %d-column cell: %q", n, sessionsTitleWidth, got)
	}
}

// Prose keeps its HEAD: it is cut from the right, the opposite side from a path.
func TestSessionsPane_ProseTitleTruncatesFromTheRight(t *testing.T) {
	const prose = "Investigate the flaky reloader debounce test"
	m := newTitleModel(t, map[string]SessionMetadata{"s1": {Title: prose}}, "s1")
	m.sessionsTbl.SetColumns(sessionsColumnsFor(116))
	m.rebuildSessionsTable()

	// Arrives whole HERE because it fits here, not because prose is exempt. The comment this
	// replaced claimed the cell was "handed to bubbles whole" and "under no obligation to
	// arrive pre-truncated", which stopped being true when every cell became bounded — it
	// passed only because a 116-column fixture leaves this 43-character title room to spare.
	// Asserting equality is still the right check at this width; what was wrong was the reason
	// given for it, which invited someone to widen the title and conclude the code had broken.
	got := sessionsCell(t, m, titleRow(t, m, "s1"), "TITLE")
	if got != prose {
		t.Errorf("TITLE = %q, want the untouched title %q — it fits this width", got, prose)
	}

	// And at a width where it does NOT fit, the cut takes the tail and keeps the opening
	// words, which is the side this test is named for.
	titleW := sessionsColumnWidth(sessionsColumnsFor(90), "TITLE")
	cut := m.sessionTitleCell("s1", titleW)
	if lipgloss.Width(cut) > titleW {
		t.Errorf("TITLE is %d columns against a %d-column cell: %q", lipgloss.Width(cut), titleW, cut)
	}
	// "Investi…" at the narrow end — the opening words, however few fit. Checked as a prefix of
	// the original rather than against a fixed string, so the assertion says "the head
	// survived" without hard-coding what this width happens to allow.
	head := strings.TrimSuffix(cut, "…")
	if head == "" || !strings.HasPrefix(prose, head) {
		t.Errorf("prose lost its head: %q is not the start of %q", cut, prose)
	}
	if !strings.HasSuffix(cut, "…") {
		t.Errorf("the cut is not marked: %q", cut)
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
	// SESSION, not ID, and 14 rather than 40: main narrowed the id column to seat COST and
	// SAVED. The property is unchanged — growth comes from slack, never from a neighbour.
	if got := width(at200, "SESSION"); got != 14 {
		t.Errorf("SESSION at 200 = %d, want its declared 14 — growth must come from slack, not a neighbour", got)
	}
	if got := tableWidth(at200); got != 200 {
		t.Errorf("rendered width at 200 = %d, want exactly 200", got)
	}

	// The narrow path applies no growth, and the shrink is the CALLER's: sessionsColumnsFor
	// returns the unfitted set and rebuildSessionsTable wraps it in fitTableColumns, which needs
	// the fitted widths for padLeft anyway.
	//
	// NOT EQUAL TO THE TERMINAL, and that is the point: fitTableColumns only ever shrinks, so a
	// set that already fits renders narrower than the screen rather than being stretched. What
	// must hold is that it never renders WIDER.
	if got := tableWidth(fitTableColumns(sessionsColumnsFor(80), 80)); got > 80 {
		t.Errorf("rendered width at 80 = %d, past the terminal", got)
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

// TestTruncLeft_BudgetsInDisplayColumns is the must-fix: truncLeft measured in RUNES while
// every caller budgets in display columns.
//
// The two failures differ and both are bad. In a header the over-wide string wraps the
// terminal, which costs a body row. In the table the library re-truncates from the RIGHT,
// destroying the tail that left-truncation exists to preserve — so the feature inverts for
// exactly the titles that need it most. Titles are model-generated text or filesystem paths,
// which this package documents as content nobody here controls, so CJK and emoji are expected
// by design rather than exotic: a 14-column budget returned 27 columns before the fix.
func TestTruncLeft_BudgetsInDisplayColumns(t *testing.T) {
	for _, s := range []string{
		"/Users/snible/src/cortex/.worktrees/claudesessions",
		"日本語のセッションタイトルです日本語のセッション",
		"🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉",
		"mixed 日本語 and ascii together",
		// A COMBINING MARK and a VARIATION SELECTOR, which is what the ellipsis measurement
		// exists for: both are zero-width and fuse onto whatever precedes them, so a tail
		// beginning with one is narrower beside the ellipsis than the sum of its parts. Without
		// these the corpus is all ASCII, CJK and plain emoji, and reverting that fix still
		// passed — the test was blind to the case it was written for.
		"/a/b/c/e\u0301\u0301\u0301fghijklmnop",
		"/a/b/c/\u2708\ufe0f\u2708\ufe0fdefghijklmnop",
	} {
		for _, n := range []int{1, 2, 5, 14, 24, 40} {
			got := truncLeft(s, n)
			if w := lipgloss.Width(got); w > n {
				t.Errorf("truncLeft(%q, %d) is %d display columns — over budget: %q", s, n, w, got)
			}
		}
	}
	// And the tail is what survives, which is the whole point of truncating from the left.
	if got := truncLeft("/a/b/c/distinguishing-end", 14); !strings.HasSuffix(got, "end") {
		t.Errorf("truncLeft dropped the tail: %q", got)
	}
}

// TestSessionsShowTitle_RendersAtTheCommonWidth pins the width at which TITLE first appears.
//
// NOTHING PINNED IT BEFORE, which is why a 20-column error survived review: the code comment,
// a test comment and the commit message all said 78 while the real threshold was 98, and the
// column this change exists for was invisible on an 80-column terminal — the common default,
// and the width the PR description used as its worked example.
//
// 80 is asserted directly rather than derived from the gate's own arithmetic, because deriving
// it would restate the implementation and agree with it however wrong it became.
func TestSessionsShowTitle_RendersAtTheCommonWidth(t *testing.T) {
	if !hasColumn(sessionsColumnsFor(80), "TITLE") {
		t.Errorf("no TITLE column at 80 columns: %v", titles(sessionsColumnsFor(80)))
	}
	// And the money columns are what yield for it there — the deliberate trade, so a change
	// that silently reversed it fails here rather than in someone's terminal.
	if hasColumn(sessionsColumnsFor(80), "COST") {
		t.Errorf("COST survives beside TITLE at 80 columns; the budget cannot hold both: %v",
			titles(sessionsColumnsFor(80)))
	}
	// And both are present at SOME width — found by sweeping rather than by naming one. The
	// literal was 82 and went stale the moment main added CONTEXT(1M), which is the third time
	// a hardcoded width in this file has had to be re-derived; the property worth asserting is
	// that such a width exists and is close to 80, not what it happens to be today.
	both := -1
	for w := 80; w <= 120; w++ {
		c := sessionsColumnsFor(w)
		if hasColumn(c, "TITLE") && hasColumn(c, "COST") && hasColumn(c, "SAVED") {
			both = w
			break
		}
	}
	if both < 0 {
		t.Fatalf("no width up to 120 carries TITLE and the money columns together")
	}
	// 97, which is the answer rather than a bound with slack in it: the money columns wait until
	// the whole set holds every minimum at once, TITLE's floor included, rather than rendering
	// beside a title squeezed to eight columns. An earlier revision allowed up to 100 and so had
	// three columns of drift to spare, which defeats the point of the assertion.
	if both > 97 {
		t.Errorf("TITLE and the money columns first coexist at %d columns, further out than "+
			"this table should push a reader", both)
	}
}

// TestSessionsPane_NarrowingResizeDoesNotLeaveAnOverWideTitle reproduces the stale-width bug
// directly, which the truncation test above cannot: it rebuilds from a settled state, where the
// live header already matches.
//
// The rows are built BEFORE SetColumns installs the new header, so a cell truncated against the
// table's current width is truncated against the PREVIOUS one. On a narrowing resize that cell
// is too wide, reaches the table's fixed-width box, and is re-truncated from the RIGHT —
// destroying the tail left-truncation exists to keep, which is the inversion this whole feature
// is about. One rebuild, from wide to narrow, is what exposes it.
func TestSessionsPane_NarrowingResizeDoesNotLeaveAnOverWideTitle(t *testing.T) {
	const long = "/Users/someone/src/cortex/.worktrees/claudesessions/authbridge"
	m := newTitleModel(t, map[string]SessionMetadata{"s1": {Title: long}}, "s1")

	// Settle wide, where TITLE grows into the slack and the cell is barely truncated.
	m.width = 200
	m.rebuildSessionsTable()

	// Then narrow, in ONE rebuild — the resize path.
	m.width = tableWidth(sessionsColumns())
	m.rebuildSessionsTable()

	got := sessionsCell(t, m, titleRow(t, m, "s1"), "TITLE")
	titleW := sessionsColumnWidth(m.sessionsTbl.Columns(), "TITLE")
	if w := lipgloss.Width(got); w > titleW {
		t.Errorf("after narrowing, TITLE is %d columns in a %d-column cell: %q — bubbles will "+
			"re-truncate it from the right and the tail is lost", w, titleW, got)
	}
	if !strings.HasSuffix(got, "authbridge") {
		t.Errorf("TITLE = %q, want the tail of the path", got)
	}
}

// TestSessionsShowTitle_FloorHoldsInTheDecisiveBand is the tripwire for the floor enforcement
// itself, in the band where enforcing it changes the answer.
//
// WIDTHS 73 TO 79, which no other test reaches: the threshold test starts at 80. Between 73 and
// 79 the two forms of this gate disagree completely — measuring the fitted set grants TITLE and
// drops the money columns, while the admission arithmetic it replaced does the opposite. So a
// revert of the fix flips the layout here and nowhere else, and without a case in this band the
// whole suite stayed green through it.
//
// That matters more than usual: this revision exists because an unenforced floor went unnoticed,
// so shipping the enforcement with no tripwire would repeat the failure it was written to
// correct.
func TestSessionsShowTitle_FloorHoldsInTheDecisiveBand(t *testing.T) {
	for w := 73; w <= 79; w++ {
		cols := sessionsColumnsFor(w)
		if !hasColumn(cols, "TITLE") {
			t.Errorf("width %d: no TITLE column (%v) — the gate is admitting on declared widths "+
				"again rather than on what the fitter leaves", w, titles(cols))
			continue
		}
		// And it holds its floor, which is the property the gate now checks rather than assumes.
		if got := sessionsColumnWidth(fitTableColumns(cols, w), "TITLE"); got < sessionsTitleWidth {
			t.Errorf("width %d: TITLE fitted to %d, under its %d floor: %v",
				w, got, sessionsTitleWidth, titles(cols))
		}
	}
}

// TestSessionsTitle_WidthNeverShrinksAsTheTerminalGrows pins the monotonicity of TITLE's WIDTH,
// which is separate from the monotonicity of column presence and was not held.
//
// TITLE absorbed every spare column, so at 96 it was 27 wide with the money columns absent, and
// at 97 they returned and the fitter clawed the shortfall back to its 11 floor. A legible path
// became a leaf fragment as the terminal got WIDER — the harm this column's floor exists to
// prevent, arriving by the other door. The width band rendered during development was
// 200/120/100/80, which is why it survived.
//
// TITLE only. SESSION and UPDATED still narrow at the two widths where a column group arrives
// (73 and 97), which is inherent to seating a new column in a fixed budget and is not what this
// asserts.
func TestSessionsTitle_WidthNeverShrinksAsTheTerminalGrows(t *testing.T) {
	prev := 0
	for w := 60; w < 250; w++ {
		got := sessionsColumnWidth(fitTableColumns(sessionsColumnsFor(w), w), "TITLE")
		if got == 0 { // not rendered at this width
			continue
		}
		if prev > 0 && got < prev {
			t.Errorf("widening %d to %d shrank TITLE from %d to %d columns", w-1, w, prev, got)
		}
		prev = got
	}
}

// forceColor makes styling real for the duration of a test.
//
// CI has no TTY, so lipgloss defaults to Ascii and every Render is a no-op there — which means a
// width assertion measures plain text locally AND in CI, and never sees the escape sequences a real
// terminal gets. That is the gap this closes: a style that emitted an unterminated sequence, or
// padding computed from a styled string's byte length, would be invisible to every one of these
// tests. Same idiom as event_retention_test.go and footer_test.go, which force it for the same
// reason.
func forceColor(t *testing.T) {
	t.Helper()
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(orig) })
}

// truncRight budgets in display columns, the same rule TestTruncLeft_BudgetsInDisplayColumns
// pins for its sibling.
//
// The prose branch of sessionTitleCell used trunc, which counts RUNES, so a CJK or emoji title
// measured roughly twice its budget: 11 columns returned 21 of CJK and 14 of emoji. The frame
// stayed intact — the table's fixed-width box re-cuts an over-wide cell — so the symptom was
// cosmetic over-truncation at a point the renderer picked rather than a broken line. Worth
// fixing anyway, and worth a test: the sibling had one and this path had none, and making the
// harvest default-on newly exposes it to every user.
func TestTruncRight_BudgetsInDisplayColumns(t *testing.T) {
	forceColor(t)
	for _, s := range []string{
		"fix the parser bug and add a regression test",
		"日本語のセッションタイトルです日本語のセッション",
		"🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉",
		"mixed 日本語 and ascii together",
		// Combining marks and variation selectors, for the reason the sibling lists them: both
		// are zero-width and fuse onto what precedes them, so measuring the head PLUS the
		// ellipsis is the only way to be sure of the result.
		"é́́fghijklmnop prose",
		"✈️✈️defghijklmnop prose",
	} {
		for _, n := range []int{1, 2, 5, 11, 14, 24, 40} {
			got := truncRight(s, n)
			if w := lipgloss.Width(got); w > n {
				t.Errorf("truncRight(%q, %d) is %d display columns — over budget: %q", s, n, w, got)
			}
		}
	}
	// And the HEAD is what survives, which is why prose truncates from the right. 13 columns
	// of it, not 14: the ellipsis marking the cut occupies one of the budgeted columns.
	if got := truncRight("distinguishing-start of some prose", 14); !strings.HasPrefix(got, "distinguishin") {
		t.Errorf("truncRight dropped the head: %q", got)
	}
	if got := truncRight("distinguishing-start of some prose", 14); !strings.HasSuffix(got, "…") {
		t.Errorf("truncRight did not mark the cut: %q", got)
	}
}

// A CJK prose title reaches the table cell already inside its budget.
//
// The unit test above pins truncRight; this pins that sessionTitleCell actually ROUTES prose
// through it. The two used to disagree: the cell called the rune-counting helper, so the
// function was right and the caller was not.
func TestSessionTitleCell_BoundsCJKProse(t *testing.T) {
	forceColor(t)
	const id = "cjk-prose"
	m := newTitleModel(t, map[string]SessionMetadata{
		id: {Title: "日本語のセッションタイトルです日本語のセッション"},
	}, id)
	for _, w := range []int{11, 14, 20} {
		got := m.sessionTitleCell(id, w)
		if cw := lipgloss.Width(got); cw > w {
			t.Errorf("sessionTitleCell(%d) is %d display columns: %q", w, cw, got)
		}
	}
}

// A background harvest that lands after the UI is up names the sessions already on screen.
//
// This is the whole point of running the scan async: the viewer opens immediately with whatever
// the metadata file held, and a session the harvest newly names gets its title when the scan
// finishes rather than at the next launch. Without the rebuild in the harvestedMsg handler the
// map would update and the table would keep showing the old cells until the next poll.
func TestHarvestedMsg_NamesSessionsAlreadyOnScreen(t *testing.T) {
	const id = "late-named"
	m := newTitleModel(t, map[string]SessionMetadata{}, id)
	if got := m.sessionTitle(id); got != "" {
		t.Fatalf("title = %q before the harvest, want empty", got)
	}

	m.Update(harvestedMsg{meta: map[string]SessionMetadata{
		id: {Title: "arrived late"},
	}})

	if got := m.sessionTitle(id); got != "arrived late" {
		t.Errorf("title = %q after the harvest, want %q", got, "arrived late")
	}
	// And it is in the rendered cell, not merely in the map.
	if got := sessionsCell(t, m, titleRow(t, m, id), "TITLE"); !strings.Contains(got, "arrived late") {
		t.Errorf("TITLE cell = %q, want it to carry the harvested title", got)
	}
}

// A harvest MERGES rather than replaces, so it cannot blank a title the viewer already shows.
//
// The map it merges into was loaded from a file that may hold entries from another config dir or
// from a transcript since pruned — the same reason the harvester itself upserts. An incremental
// harvest also returns the merged file rather than only what it re-read, but this handler must
// not depend on that.
func TestHarvestedMsg_DoesNotBlankExistingTitles(t *testing.T) {
	const kept, renamed = "keep-me", "rename-me"
	m := newTitleModel(t, map[string]SessionMetadata{
		kept:    {Title: "from another config dir"},
		renamed: {Title: "old name"},
	}, kept, renamed)

	m.Update(harvestedMsg{meta: map[string]SessionMetadata{
		renamed: {Title: "new name"},
	}})

	if got := m.sessionTitle(kept); got != "from another config dir" {
		t.Errorf("an entry the harvest did not see was lost: %q", got)
	}
	if got := m.sessionTitle(renamed); got != "new name" {
		t.Errorf("the harvest did not win for a session it re-read: %q", got)
	}
}

// A harvest that brought nothing back changes nothing.
//
// One case, not two: a FAILED harvest and an EMPTY one are the same message here, because
// harvestedMsg carries no error — there is nowhere to report one by the time this arrives, so
// the handler has nothing to distinguish. A test named for failure alone would have promised
// more than it checked, which is what the review pointed out.
func TestHarvestedMsg_EmptyResultLeavesTitlesAlone(t *testing.T) {
	const id = "s1"
	m := newTitleModel(t, map[string]SessionMetadata{id: {Title: "existing"}}, id)

	m.Update(harvestedMsg{})

	if got := m.sessionTitle(id); got != "existing" {
		t.Errorf("an empty harvest disturbed the title: %q", got)
	}
}

// sanitizeLabel neutralizes every control class that can disturb a rendered label.
//
// Titles are LLM-generated transcript text read from a file nothing authenticates, so the
// question is not whether a hostile title is likely but what one can do. C0 and DEL were already
// handled; C1 controls and the bidi overrides were not, and the bidi ones are the ones that
// actually render — each is zero-width, so the width math stays self-consistent and the frame
// holds, but the terminal REORDERS the surrounding text and the title displays in an order that
// is not the order of its bytes.
func TestSanitizeLabel_NeutralizesControlClasses(t *testing.T) {
	forceColor(t)
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"C0 newline", "two\nlines"},
		{"C0 escape", "colour\x1b[31mshift"},
		{"DEL", "del\x7fete"},
		{"C1 NEL", "next\u0085line"},
		{"C1 CSI", "csi\u009bm"},
		{"bidi RLO", "report‮gnp.exe"},
		{"bidi LRO", "a‭b"},
		{"bidi isolate", "a⁦b⁩c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeLabel(tc.in)
			if got == tc.in {
				t.Errorf("sanitizeLabel passed it through unchanged: %q", got)
			}
			if !strings.ContainsRune(got, '�') {
				t.Errorf("no replacement character in %q", got)
			}
		})
	}

	// Legitimate content is untouched — including the CJK and emoji that real titles carry, which
	// must not be swept up by a rule aimed at controls.
	for _, ok := range []string{
		"fix the parser bug",
		"日本語のセッションタイトル",
		"🎉 ship it",
		"/Users/somebody/src/cortex",
		"café naïve",
	} {
		if got := sanitizeLabel(ok); got != ok {
			t.Errorf("sanitizeLabel altered legitimate text %q -> %q", ok, got)
		}
	}
}

// A non-positive budget yields no cell, not an unbounded one.
//
// Unreachable today — the column is admitted with a floor and layout only shrinks to it — but it
// used to return the FULL title, which is an unbudgeted cell handed to a table that then has to
// cut it somewhere. Every other branch of sessionTitleCell exists to stop exactly that, so if
// this one ever becomes reachable it should fail in the safe direction.
func TestSessionTitleCell_NonPositiveWidthYieldsNothing(t *testing.T) {
	forceColor(t)
	const prose, path = "prose-id", "path-id"
	m := newTitleModel(t, map[string]SessionMetadata{
		prose: {Title: "Investigate the flaky reloader debounce test"},
		path:  {Title: "/Users/somebody/src/cortex/.worktrees/long-name"},
	}, prose, path)

	for _, id := range []string{prose, path} {
		for _, w := range []int{0, -1} {
			if got := m.sessionTitleCell(id, w); got != "" {
				t.Errorf("sessionTitleCell(%q, %d) = %q, want \"\"", id, w, got)
			}
		}
	}
}

// trunc budgets in display columns, and is unchanged on the ASCII its callers pass today.
//
// It counted RUNES, with four live callers — session ids, and the identity block's JWT subject,
// client and scope claims, which are remote-controlled rather than ASCII-guaranteed. Measured
// before the fix: an 11-column budget returned 21 columns of CJK.
//
// The ASCII half of this test is what makes the fix safe to make as a redirect rather than a
// rewrite: if the two ever diverge on the input today's callers actually pass, this fails and the
// redirect is not behaviour-preserving after all.
func TestTrunc_BudgetsInDisplayColumnsAndKeepsASCIIIdentical(t *testing.T) {
	forceColor(t)
	for _, s := range []string{
		"日本語のセッションタイトルです",
		"🎉🎉🎉🎉🎉🎉🎉🎉",
		"mixed 日本語 and ascii",
	} {
		for _, n := range []int{1, 2, 8, 11, 14, 40} {
			if w := lipgloss.Width(trunc(s, n)); w > n {
				t.Errorf("trunc(%q, %d) is %d display columns: %q", s, n, w, trunc(s, n))
			}
		}
	}

	// Unchanged for the shapes the live callers pass: session ids, a UUID, a JWT-ish claim line.
	for _, s := range []string{
		"agent-07.team1.svc.cluster.local:8080",
		"3eb6d5ce-0000-0000-0000-000000000001",
		"subject  alice@example.com",
	} {
		for _, n := range []int{8, 14, 20, 40} {
			if got, want := trunc(s, n), truncRight(s, n); got != want {
				t.Errorf("trunc(%q, %d) = %q, want %q", s, n, got, want)
			}
			if w := lipgloss.Width(trunc(s, n)); w > n {
				t.Errorf("trunc(%q, %d) is %d columns, over budget", s, n, w)
			}
		}
	}
}

// A rendered TITLE cell carries no ANSI, which is what makes the column measurement sufficient.
//
// bubbles v1.0.0 runs runewidth.Truncate over every cell before styling, and runewidth does not skip
// escape sequences. Measuring in display columns here is therefore only safe while the cell is PLAIN:
// escape bytes would be charged against the budget and a narrow cell would collapse to a lone
// ellipsis. The harvester guarantees plain titles; this asserts the renderer does not reintroduce
// styling, so the pair of facts the comment on truncLeft depends on is actually held by a test.
func TestSessionTitleCell_CarriesNoANSI(t *testing.T) {
	forceColor(t) // styling real, so a Render that added escapes would show up here

	const id = "s1"
	for _, title := range []string{
		"a plain prose title",
		"/Users/somebody/src/cortex/.worktrees/a-long-name/authbridge",
		"日本語のセッションタイトルです",
		"ship it 🎉",
	} {
		m := newTitleModel(t, map[string]SessionMetadata{id: {Title: title}}, id)

		// The WIDTH half, on the helper. This one is real here: sessionTitleCell does the truncation,
		// so a budget it fails to honour is its own bug.
		for _, w := range []int{11, 20, 40} {
			got := m.sessionTitleCell(id, w)
			if lipgloss.Width(got) > w {
				t.Errorf("title cell is %d columns against a %d-column budget: %q",
					lipgloss.Width(got), w, got)
			}
		}

		// The ESCAPE half, on the STORED CELL — the string this package hands to bubbles.
		//
		// It used to assert on sessionTitleCell's return, which makes no Render call, so it held by
		// construction. The stored row cell is one step further along and is the value that actually
		// matters for the hazard MaxTitleLen's doc comment describes: bubbles renders every cell as
		// styles.Cell.Render(style.Render(runewidth.Truncate(value, width, "…"))) — table.go:435 in
		// v1.0.0 — and runewidth is NOT ANSI-aware. So escape bytes in `value` are charged against
		// the column budget and a narrow cell collapses to a lone ellipsis. Asserting on the stored
		// value is asserting on runewidth's input, which is where the contract lives.
		//
		// NOT the rendered View(): that string legitimately contains escapes — tableStyles sets
		// Selected to bold-on-background and DefaultStyles pads every cell — so a scan for 0x1b
		// there would fail on correct output and says nothing about the title.
		//
		// forceColor is above, so lipgloss emits real escapes and a styled title is caught.
		for _, termW := range []int{80, 100, 200} {
			m.width = termW
			m.sessionsTbl.SetColumns(sessionsColumnsFor(termW))
			m.rebuildSessionsTable()
			cell := sessionsCell(t, m, titleRow(t, m, id), "TITLE")
			if strings.ContainsRune(cell, 0x1b) {
				t.Errorf("stored TITLE cell carries an escape byte at terminal width %d: %q — "+
					"bubbles measures this value with runewidth, which counts escape bytes against "+
					"the column budget and would collapse a narrow cell to an ellipsis", termW, cell)
			}
			titleW := sessionsColumnWidth(sessionsColumnsFor(termW), "TITLE")
			if lipgloss.Width(cell) > titleW {
				t.Errorf("at terminal width %d: stored TITLE cell is %d columns against a "+
					"%d-column column: %q", termW, lipgloss.Width(cell), titleW, cell)
			}
		}
	}
}

// THE CROSS-MODULE CONTRACT: the harvester's rune cap is safe only because this package
// re-truncates by display width.
//
// Each side was tested independently and neither held the relationship. authlib/observe/claude can
// assert only that MaxTitleLen counts runes — it has no width library — and this package asserts only
// that cells fit their column. So deleting the renderer's truncation broke no test, while the
// harvester's own comment warned that 80 runes of CJK occupy 160 columns.
//
// This closes it from the side that can see both: it takes a title at exactly the harvester's cap,
// in the worst case for the mismatch, and requires the rendered cell to fit a narrow column anyway.
// It fails if either the cap stops being a rune count or the renderer stops measuring in columns.
func TestTitleCap_IsSafeOnlyBecauseTheRendererRemeasures(t *testing.T) {
	forceColor(t)

	// A title the harvester would emit at its limit: MaxTitleLen runes of CJK, which is twice that
	// in display columns.
	title := strings.Repeat("日", claude.MaxTitleLen)
	if n := len([]rune(title)); n != claude.MaxTitleLen {
		t.Fatalf("fixture is %d runes, want %d", n, claude.MaxTitleLen)
	}
	if w := lipgloss.Width(title); w <= claude.MaxTitleLen {
		t.Fatalf("fixture is %d columns for %d runes — it no longer exercises the mismatch, so "+
			"either MaxTitleLen has become a width budget or this fixture needs wider characters",
			w, claude.MaxTitleLen)
	}

	// BOTH BRANCHES, because the cap meets a different truncator depending on the title's shape and
	// this test named only one of them. The prose fixture above has no leading "/", so looksLikePath
	// is false and it exercises truncRight alone — mutating truncLeft to a passthrough left this test
	// green while ten others in the package failed. A path-shaped fixture at the same cap routes down
	// the other branch, so the constant's doc comment can claim the relationship is guarded here.
	pathTitle := "/" + strings.Repeat("日", claude.MaxTitleLen-5) + "/日日日"
	if n := len([]rune(pathTitle)); n != claude.MaxTitleLen {
		t.Fatalf("path fixture is %d runes, want %d", n, claude.MaxTitleLen)
	}
	if !looksLikePath(pathTitle) {
		t.Fatalf("path fixture %q does not route down the left-truncating branch", pathTitle)
	}

	for _, tc := range []struct{ name, title string }{
		{"prose", title},
		{"path", pathTitle},
	} {
		const id = "s1"
		m := newTitleModel(t, map[string]SessionMetadata{id: {Title: tc.title}}, id)
		for _, w := range []int{11, 14, 20, 40} {
			got := m.sessionTitleCell(id, w)
			if cw := lipgloss.Width(got); cw > w {
				t.Errorf("%s: a %d-rune title rendered %d columns into a %d-column cell: %q — the "+
					"harvester's cap is a RUNE count, so this package must re-truncate by width",
					tc.name, claude.MaxTitleLen, cw, w, got)
			}
		}
	}

	const id = "s1"
	m := newTitleModel(t, map[string]SessionMetadata{id: {Title: title}}, id)

	// And the same through the rendered row, so the guard covers what a reader actually sees rather
	// than only the cell helper.
	//
	// The budget has to come from the model's OWN width. An earlier version of this test installed a
	// 100-column header while the fixture model was 200 wide, then asserted against the 100-column
	// budget — rebuildSessionsTable reads the width it is about to install, so it correctly produced
	// a 107-column cell and the test called that a bug. The failure was in the fixture.
	for _, termW := range []int{80, 100, 200} {
		m.width = termW
		m.sessionsTbl.SetColumns(sessionsColumnsFor(termW))
		m.rebuildSessionsTable()
		titleW := sessionsColumnWidth(sessionsColumnsFor(termW), "TITLE")
		cell := sessionsCell(t, m, titleRow(t, m, id), "TITLE")
		if lipgloss.Width(cell) > titleW {
			t.Errorf("at terminal width %d: rendered TITLE cell is %d columns against a %d-column "+
				"column: %q", termW, lipgloss.Width(cell), titleW, cell)
		}
	}
}

// A slash command keeps its COMMAND NAME; a filesystem path keeps its leaf.
//
// The discriminator was a bare leading "/", which was the whole story until session titles started
// coming from the user's own prompts. A typed slash command begins with one too, so
// "/review <url> carefully" was left-truncated to "…pull/1101 carefully" — discarding the command
// name, the one part a reader needs, and inverting this file's own rule that prose reads
// left-to-right.
func TestSessionTitleCell_SlashCommandIsNotAPath(t *testing.T) {
	forceColor(t)
	const id = "s1"
	for _, tc := range []struct {
		name, title string
		keepHead    bool
	}{
		{"slash command with args", "/review https://github.com/rossoctl/cortex/pull/1101 carefully", true},
		{"slash command with a path arg", "/fix-ocr some/path.md and then report", true},
		{"slash command alone", "/clear", true},
		// A real cwd: more than one segment, no space before the second "/", so the leaf is what
		// identifies it and left-truncation is right.
		{"absolute path", "/Users/somebody/src/cortex/.worktrees/alpha/authbridge", false},
		{"short absolute path", "/tmp/build/output/artifacts/final", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTitleModel(t, map[string]SessionMetadata{id: {Title: tc.title}}, id)
			const w = 20
			got := m.sessionTitleCell(id, w)
			if lipgloss.Width(got) > w {
				t.Fatalf("cell is %d columns against a %d-column budget: %q", lipgloss.Width(got), w, got)
			}
			// RUNE slices, not byte slices. Every fixture here is ASCII today, but this suite
			// deliberately exercises CJK elsewhere, and a byte slice would split a multi-byte
			// character the moment someone adds such a case — producing an invalid-UTF-8 needle and
			// a failure that looks like the code's fault.
			head := string([]rune(tc.title)[:5])
			tail := func(s string) string { r := []rune(s); return string(r[len(r)-5:]) }
			if tc.keepHead {
				if !strings.HasPrefix(got, head) {
					t.Errorf("command name lost: %q from %q", got, tc.title)
				}
				if strings.HasPrefix(got, "…") {
					t.Errorf("a slash command was truncated from the LEFT: %q", got)
				}
				return
			}
			if !strings.HasSuffix(got, tail(tc.title)) {
				t.Errorf("path leaf lost: %q from %q", got, tc.title)
			}
		})
	}
}

// looksLikePath itself, so the rule is pinned independently of how a cell renders.
func TestLooksLikePath(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"/Users/somebody/src/cortex", true},
		{"/tmp/build/out", true},
		{"/a/b", true},
		{"/review some/path.md", false}, // a space before the second slash
		{"/clear", false},               // one segment
		{"/fix-ocr", false},
		{"how do I build abctl?", false},
		{"src/cortex/authbridge", false}, // no leading slash
		{"", false},
	} {
		if got := looksLikePath(tc.in); got != tc.want {
			t.Errorf("looksLikePath(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestLooksLikePath_SingleSegmentPathWithASpaceReadsAsProse pins the rule's ONE deliberate miss.
//
// The doc comment calls this out as wrong on purpose, but nothing held it, so the trade was a claim
// rather than a decision anyone could see change. Characterization, not an endorsement: it asserts
// what the current rule does so that widening it is a visible diff and not a silent one.
//
// "/tmp foo" is a real single-segment directory with a space in it. The rule wants a second "/" before
// any space, so it reads as prose and is truncated from the RIGHT — the opposite end from the one a
// path wants kept. The cost is bounded and small, which is why the rule stays simple: the cell is cut
// at the wrong end, not unbounded, and Claude Code records a cwd with at least two segments, so this
// shape does not arise from harvesting. It can only arrive from a hand-edited metadata file or a
// prompt that happens to look like one.
//
// The OPPOSITE direction is not a miss and belongs here so the two are not confused: "/review a/b" and
// "/read docs/x.md" are slash commands whose argument contains a slash, and prose is the right answer
// for them. The rule gets those right for the same reason it gets "/tmp foo" wrong — it looks for the
// second "/" before any space — so one behaviour cannot be changed without the other.
func TestLooksLikePath_SingleSegmentPathWithASpaceReadsAsProse(t *testing.T) {
	// The deliberate miss: a genuine path, classified as prose, right-truncated.
	for _, in := range []string{"/tmp foo", "/opt my notes", "/srv a"} {
		if looksLikePath(in) {
			t.Errorf("looksLikePath(%q) = true, want false — the rule requires a second %q before "+
				"any space, so a single-segment path with a space reads as prose. If this now "+
				"returns true the trade-off changed; update the doc comment with it", in, "/")
		}
	}

	// And what it costs, measured rather than described: the leaf goes, the head is kept.
	const budget = 6
	if got := truncRight("/tmp foo", budget); got != "/tmp …" {
		t.Errorf("truncRight(%q, %d) = %q, want %q — this is the cost of the miss above, pinned so "+
			"it is a bounded wrong-end cut and not something worse", "/tmp foo", budget, got, "/tmp …")
	}

	// NOT a miss: a slash command with a slash in its argument. Prose is correct, and the same clause
	// produces both answers.
	for _, in := range []string{"/review a/b", "/read docs/x.md", "/cd /usr/local"} {
		if looksLikePath(in) {
			t.Errorf("looksLikePath(%q) = true, want false: a slash command reads left-to-right, so "+
				"left-truncating it would discard the command name", in)
		}
	}
}

// runBatch invokes a command and, if it is a tea.Batch, every member it carries.
//
// tea.Batch does not run its members: it returns a tea.BatchMsg, which the runtime then dispatches.
// A test asserting that a batched command reached a closure has to do that dispatch itself.
func runBatch(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			runBatch(t, c)
		}
	}
}

// The session picker re-harvests periodically, so a session started elsewhere gets named.
//
// The picker is the one pane where someone may sit for minutes with nothing refreshing the titles:
// the 2s tick skips the session fetch there (m.client may be nil), and the harvest previously ran
// once at Init. A session started in another terminal meanwhile stayed nameless until the viewer was
// restarted.
func TestPicker_ReHarvestsOnAnInterval(t *testing.T) {
	newPicker := func(harvest HarvestFunc) *model {
		m := newTitleModel(t, map[string]SessionMetadata{})
		m.pane = paneNamespaces
		m.harvest = harvest
		return m
	}
	called := 0
	harvest := func() (map[string]SessionMetadata, error) {
		called++
		return map[string]SessionMetadata{"s1": {Title: "found later"}}, nil
	}

	// Not yet due: the stamp is fresh, so the tick only re-arms the timer.
	m := newPicker(harvest)
	m.lastHarvest = time.Now()
	if _, cmd := m.Update(refreshTickMsg(time.Now())); cmd == nil {
		t.Fatal("the refresh ticker was not re-armed")
	}
	if called != 0 {
		t.Errorf("harvested %d times while not due, want 0", called)
	}

	// Due: the returned command must include the harvest, which we run to observe it.
	m = newPicker(harvest)
	m.lastHarvest = time.Now().Add(-2 * reharvestInterval)
	_, cmd := m.Update(refreshTickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("no command returned when a re-harvest was due")
	}
	// tea.Batch returns a BatchMsg holding the member commands rather than running them, so the
	// members have to be invoked to reach the harvest closure.
	runBatch(t, cmd)
	if called == 0 {
		t.Error("a due re-harvest did not run the harvester")
	}
	if !m.harvesting {
		t.Error("the in-flight guard was not set, so a second tick could stack a harvest")
	}

	// While one is in flight, a further tick must not start another — even once the interval has
	// elapsed again. Backdating the stamp is what makes this test the guard's: without it the tick is
	// simply not due, so the assertion passed with the guard removed (confirmed by mutation).
	before := called
	m.lastHarvest = time.Now().Add(-2 * reharvestInterval)
	_, stacked := m.Update(refreshTickMsg(time.Now()))
	runBatch(t, stacked)
	if called != before {
		t.Errorf("a harvest was stacked while one was in flight (%d -> %d)", before, called)
	}

	// The arriving result clears the guard.
	m.Update(harvestedMsg{meta: map[string]SessionMetadata{"s1": {Title: "found later"}}})
	if m.harvesting {
		t.Error("harvestedMsg did not clear the in-flight guard")
	}
	if got := m.sessionTitle("s1"); got != "found later" {
		t.Errorf("the re-harvested title did not reach the model: %q", got)
	}

	// A nil harvester (--skip-claude-metadata) must never be called.
	m = newPicker(nil)
	m.lastHarvest = time.Now().Add(-2 * reharvestInterval)
	if _, c := m.Update(refreshTickMsg(time.Now())); c == nil {
		t.Error("the ticker must stay armed even with no harvester")
	}
}

// TestLooksLikePath_AnyUnicodeSpaceSeparates pins the separator class.
//
// The function's job is to keep a slash command from being left-truncated, and it decided that on an
// ASCII-only IndexAny(rest, " \t"). A command separated by a non-breaking or ideographic space had no
// separator by that test, so the second "/" in its argument made it a path and the command name — the
// part a reader needs — was the part discarded.
//
// Reachable only from a stale or hand-edited metadata file, since the harvester folds every unicode
// space to U+0020 before writing. Asserted here anyway: this package should not depend on its input
// having come from the current harvester.
func TestLooksLikePath_AnyUnicodeSpaceSeparates(t *testing.T) {
	for _, sep := range []string{" ", "\t", " ", "　", " ", " ", " "} {
		title := "/review" + sep + "docs/plan.md"
		if looksLikePath(title) {
			t.Errorf("looksLikePath(%q) = true, want false: the %U separator makes this a slash "+
				"command with an argument, not a path — left-truncating it discards the command name",
				title, []rune(sep)[0])
		}
	}

	// The other direction still holds: a real path has no space at all before its second segment.
	for _, title := range []string{"/Users/somebody/src", "/w/x/y", "/a/b"} {
		if !looksLikePath(title) {
			t.Errorf("looksLikePath(%q) = false, want true", title)
		}
	}
}
