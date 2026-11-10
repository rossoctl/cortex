package tui

import (
	"context"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
	"github.com/rossoctl/cortex/core/observe/claude"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
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
	// replaced by CTX(1M).
	if got := strings.TrimSpace(sessionsCell(t, m, row, "UPDATED")); got != cachedMarker {
		t.Errorf("cached-only marker = %q, want %q — cells have shifted: %v", got, cachedMarker, row)
	}
}

// LoadSessionMetadata reads what the harvester writes, and answers empty for every failure.
//
// The absent case is the contract that matters: it is the state of every machine until
// someone runs `agentop experimental read-claude-sessions`, and it must be silent rather than
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

// newServedTitleModel is newTitleModel with titles on the SERVER summaries too.
//
// A sibling rather than a wider newTitleModel: the 50-odd existing callers are all about harvested
// titles, and threading an empty map through every one of them would say nothing. served is keyed
// by session id; an id absent from it lists with no title, which is what a proxy that derived none
// sends.
func newServedTitleModel(t *testing.T, meta map[string]SessionMetadata, served map[string]string, ids ...string) *model {
	t.Helper()
	m := newTitleModel(t, meta, ids...)
	for i := range m.sessions {
		m.sessions[i].Title = served[m.sessions[i].ID]
	}
	return m
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
	cut := m.sessionTitleCell("s1", noServedTitle, titleW)
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
	// A WHITESPACE-ONLY TITLE IS UNNAMED HERE TOO, on the same terms as the TITLE cell and the
	// harvest gate. A raw != "" accepted it and produced "    (id-3)": a header indented by a
	// title that displays nothing, which reads as a rendering fault rather than as a session
	// nobody has named.
	m.sessionsData["id-3"] = SessionMetadata{Title: "   "}
	if got, want := m.sessionLabel("id-3"), "id-3"; got != want {
		t.Errorf("sessionLabel for a blank title = %q, want the bare id %q", got, want)
	}
	if got, want := m.sessionHeader("id-1", ""), "agentop · fix the parser (id-1)"; got != want {
		t.Errorf("events header = %q, want %q", got, want)
	}
	if got, want := m.sessionHeader("id-1", "event"), "agentop · fix the parser (id-1) · event"; got != want {
		t.Errorf("detail header = %q, want %q", got, want)
	}
	// The build, when one was given, follows the name on this header as on every other.
	m.version = "v0.8.0"
	if got, want := m.sessionHeader("id-1", ""), "agentop v0.8.0 · fix the parser (id-1)"; got != want {
		t.Errorf("events header with a version = %q, want %q", got, want)
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
// the leaf of the path rather than the "agentop · " that is on every screen anyway.
func TestSessionHeader_ClipsToTerminalWidthFromTheLeft(t *testing.T) {
	const id = "0e61b82d-8578-4d16-a18e-1d085ca678fc"
	const title = "/Users/someone/src/cortex/.worktrees/claudesessions/authbridge/cmd/agentop"
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
		if !strings.HasPrefix(got, "agentop · …") {
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
	const title = "/Users/someone/src/cortex/.worktrees/claudesessions/authbridge/cmd/agentop"
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
		"/Users/person/src/cortex/.worktrees/claudesessions",
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
	// literal was 82 and went stale the moment main added CTX(1M), which is the third time
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
		got := m.sessionTitleCell(id, noServedTitle, w)
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
			if got := m.sessionTitleCell(id, noServedTitle, w); got != "" {
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
			got := m.sessionTitleCell(id, noServedTitle, w)
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
// Each side was tested independently and neither held the relationship. core/observe/claude can
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
			got := m.sessionTitleCell(id, noServedTitle, w)
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
			got := m.sessionTitleCell(id, noServedTitle, w)
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
		{"how do I build agentop?", false},
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

// The session picker harvests on arrival, so a session started elsewhere gets named.
//
// The picker is the one pane where someone may sit with nothing refreshing the titles: the 2s tick
// skips the session fetch there (m.client may be nil), and the harvest once ran only at Init, so a
// session started in another terminal stayed nameless until the viewer was restarted.
//
// ON ARRIVAL, NOT ON AN INTERVAL, which is what this test was renamed from. The interval it used to
// assert has been removed: once pickerHarvested capped the pane at one scan per visit, the clock
// could only SUPPRESS that scan — entering the picker soon after any other harvest skipped the
// visit's only walk. What remains worth pinning here is the in-flight stacking guard, which is why
// that part of the test is unchanged.
func TestPicker_HarvestsOnArrival(t *testing.T) {
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

	// A FRESH STAMP MUST NOT SUPPRESS THE SCAN. This is the removed interval, stated as the
	// assertion it now fails: lastHarvest is seconds old — as it is on arriving in the picker just
	// after startup — and the visit's harvest must still run.
	m := newPicker(harvest)
	m.lastHarvest = time.Now()
	_, cmd := m.Update(refreshTickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("the refresh ticker was not re-armed")
	}
	runBatch(t, cmd)
	if called != 1 {
		t.Fatalf("harvested %d times on arrival with a fresh stamp, want 1", called)
	}
	if !m.harvesting {
		t.Error("the in-flight guard was not set, so a second tick could stack a harvest")
	}

	// While one is in flight, a further tick must not start another. Clearing pickerHarvested is
	// what makes this test the GUARD's: leaving it set would stop the second tick by itself, so the
	// assertion would pass with the in-flight check removed.
	before := called
	m.pickerHarvested = false
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

// TestTrunc_ScalesLinearly pins the cost of both truncators against the INPUT length.
//
// Both measured the whole remaining string with lipgloss.Width once per dropped rune, so the cost grew
// with the square of the input: on one call, 2500 runes took 36ms, 5000 142ms, 10000 572ms and 20000
// 2.33s — four times the input for sixteen times the work. Reachable because the cwd tier was
// uncapped, and the renderer redraws on every poll.
//
// The cost is counted in allocations, so the assertion is about the work done and not about the
// machine: the skip-ahead tries at most budget candidates whatever the input length, and the
// one-at-a-time search allocates a candidate per dropped rune.
func TestTrunc_ScalesLinearly(t *testing.T) {
	const budget = 40
	measure := func(f func(string, int) string, runes int) float64 {
		s := "/" + strings.Repeat("a", runes-1)
		return testing.AllocsPerRun(3, func() { f(s, budget) })
	}
	for _, tc := range []struct {
		name string
		f    func(string, int) string
	}{
		{"truncLeft", truncLeft},
		{"truncRight", truncRight},
	} {
		t.Run(tc.name, func(t *testing.T) {
			small := measure(tc.f, 4000)
			large := measure(tc.f, 16000)
			// The allocation count is process-wide, so the ceiling leaves room for a stray
			// allocation; the quadratic search makes thousands.
			if large > 2*small {
				t.Errorf("4x the input made %.0f allocations, up from %.0f — that is the quadratic shape "+
					"back: the per-rune search must skip to the last/first n runes before measuring",
					large, small)
			}
		})
	}
}

// TestTrunc_SkipAheadMatchesTheOneAtATimeSearch is the differential test that caught the first version
// of the skip being wrong, kept so it cannot regress.
//
// The skip rests on "every rune is at least one column, so n runes from the end is an exact lower
// bound". That premise is FALSE for zero-width runes — combining marks, joiners, variation selectors —
// and an unguarded skip returned different bytes than the one-at-a-time search at n == 1 on a string of
// combining marks. The guard is zeroWidthFree; this asserts the equivalence it is supposed to buy,
// across alphabets chosen so both sides of the guard are exercised.
func TestTrunc_SkipAheadMatchesTheOneAtATimeSearch(t *testing.T) {
	// The pre-skip implementations, as an oracle.
	oldLeft := func(s string, n int) string {
		if lipgloss.Width(s) <= n {
			return s
		}
		if n < 1 {
			return ""
		}
		r := []rune(s)
		for i := range r {
			if out := "…" + string(r[i:]); lipgloss.Width(out) <= n {
				return out
			}
		}
		return "…"
	}
	oldRight := func(s string, n int) string {
		if lipgloss.Width(s) <= n {
			return s
		}
		if n < 1 {
			return ""
		}
		r := []rune(s)
		for i := len(r); i > 0; i-- {
			if out := string(r[:i]) + "…"; lipgloss.Width(out) <= n {
				return out
			}
		}
		return "…"
	}

	alphabets := []string{
		"abcdefghijklmnopqrstuvwxyz /._-", // the ordinary case, and the one that must be fast
		"日本語のセッションタイトル漢字",                 // two columns per rune
		"🎉🚀✨🔥",                            // wide emoji
		"aあ🎉/b日x",                         // mixed widths
		"éà",                            // COMBINING MARKS: zero width, the case that broke it
		"️‍",                              // variation selector, ZWJ: also zero width
	}
	rng := rand.New(rand.NewSource(20260923))
	checked := 0
	for _, alpha := range alphabets {
		ar := []rune(alpha)
		for trial := 0; trial < 120; trial++ {
			var sb strings.Builder
			for i := 0; i < rng.Intn(60); i++ {
				sb.WriteRune(ar[rng.Intn(len(ar))])
			}
			s := sb.String()
			for n := -2; n <= 45; n++ {
				if want, got := oldLeft(s, n), truncLeft(s, n); want != got {
					t.Fatalf("truncLeft(%q, %d) = %q, one-at-a-time search gives %q", s, n, got, want)
				}
				if want, got := oldRight(s, n), truncRight(s, n); want != got {
					t.Fatalf("truncRight(%q, %d) = %q, one-at-a-time search gives %q", s, n, got, want)
				}
				checked += 2
			}
		}
	}
	t.Logf("%d comparisons against the one-at-a-time search, all byte-identical", checked)
}

// TestZeroWidthFree pins the guard's own answer, including that an ordinary title takes the fast path.
func TestZeroWidthFree(t *testing.T) {
	for _, s := range []string{"", "plain prose", "/Users/x/src", "日本語", "🎉", "a b-c_d.e"} {
		if !zeroWidthFree(s) {
			t.Errorf("zeroWidthFree(%q) = false, want true — an ordinary title must take the fast path", s)
		}
	}
	for _, s := range []string{"é", "a‍", "x️", "a\u0000b", "́"} {
		if zeroWidthFree(s) {
			t.Errorf("zeroWidthFree(%q) = true, want false — %U occupies no column, so the prefix "+
				"bound does not hold", s, []rune(s)[len([]rune(s))-1])
		}
	}
}

// TestSessionsPane_ReHarvestsSettledUntitledSessions pins the sessions LIST as a pane that
// re-harvests, which it was not.
//
// The re-harvest was gated on paneNamespaces/panePods, so an operator sitting on the sessions
// list — the pane they pick a session FROM — watched a new session stay a bare UUID
// indefinitely: every other cell in the row refreshes on the 2s poll, so the row looked live
// while sessionsData was frozen at whatever startup loaded. Reported from exactly that.
//
// Keyed off UpdatedAt rather than a wall clock, since traffic on a session is what says its
// transcript is being appended to.
//
// ASSERTS ON m.harvesting, NOT by running the returned batch. The sessions pane's tick also
// batches loadSessionsCmd, which dereferences a nil apiclient in a unit model — so running the
// batch panics on the fetch rather than testing the harvest. The in-flight guard is set in the
// same branch that creates the harvest command and is what the next tick reads, so it is the
// honest observable here; TestPicker_ReHarvestsOnAnInterval covers the closure actually running.
func TestSessionsPane_ReHarvestsSettledUntitledSessions(t *testing.T) {
	newSessions := func(updatedAt time.Time, meta map[string]SessionMetadata) *model {
		m := newTitleModel(t, meta, "s1")
		m.sessions = []session.SessionSummary{{ID: "s1", UpdatedAt: updatedAt}}
		m.harvest = func() (map[string]SessionMetadata, error) {
			return map[string]SessionMetadata{"s1": {Title: "named at last"}}, nil
		}
		// Backdated so the settle delay is the only thing under test; the tick's own floor
		// reuses lastHarvest and would otherwise mask it.
		m.lastHarvest = time.Now().Add(-time.Hour)
		return m
	}

	// Settled and unnamed: the harvest starts.
	m := newSessions(time.Now().Add(-2*untitledSettleDelay), map[string]SessionMetadata{})
	if _, cmd := m.Update(refreshTickMsg(time.Now())); cmd == nil {
		t.Fatal("no command returned; the refresh ticker must stay armed")
	}
	if !m.harvesting {
		t.Error("a settled untitled session did not trigger a re-harvest")
	}
	// And an arriving result clears the guard and reaches the table, which is the point.
	m.Update(harvestedMsg{meta: map[string]SessionMetadata{"s1": {Title: "named at last"}}})
	if m.harvesting {
		t.Error("harvestedMsg did not clear the in-flight guard")
	}
	if got := m.sessionTitle("s1"); got != "named at last" {
		t.Errorf("harvested title did not reach the model: %q", got)
	}

	// STILL BEING WRITTEN: an event landed just now, so the transcript's last turn may be
	// mid-write and the tiers read the LAST prompt. No harvest.
	m = newSessions(time.Now(), map[string]SessionMetadata{})
	m.Update(refreshTickMsg(time.Now()))
	if m.harvesting {
		t.Error("harvested an unsettled session, whose transcript may still be mid-write")
	}

	// ALREADY NAMED: the steady state must cost nothing, or this poll would scan the
	// transcript tree every two seconds forever.
	m = newSessions(time.Now().Add(-2*untitledSettleDelay),
		map[string]SessionMetadata{"s1": {Title: "known"}})
	m.Update(refreshTickMsg(time.Now()))
	if m.harvesting {
		t.Error("harvested with every row already named")
	}

	// A nil harvester (--skip-claude-metadata) is never called, and the ticker stays armed.
	m = newSessions(time.Now().Add(-2*untitledSettleDelay), map[string]SessionMetadata{})
	m.harvest = nil
	if _, c := m.Update(refreshTickMsg(time.Now())); c == nil {
		t.Error("the ticker must stay armed with no harvester")
	}
	if m.harvesting {
		t.Error("claimed a harvest was in flight with no harvester")
	}
}

// TestSessionsPane_BacksOffFruitlessHarvests pins the exponential backoff.
//
// A session with no transcript under the agent's config dir can never be named — a different
// agent wrote it, the tree was pruned, CLAUDE_CONFIG_DIR moved. Its row keeps the settle gate
// satisfied forever, so without a backoff the pane re-walks the whole transcript tree every
// untitledSettleDelay for a title that is not coming.
func TestSessionsPane_BacksOffFruitlessHarvests(t *testing.T) {
	m := newTitleModel(t, map[string]SessionMetadata{}, "s1")
	m.sessions = []session.SessionSummary{{ID: "s1", UpdatedAt: time.Now().Add(-time.Hour)}}
	m.harvest = func() (map[string]SessionMetadata, error) { return nil, nil }

	// SPEND THE ARRIVAL RESET FIRST. s1 is unnamed and this model has never scored it, so the
	// first harvest resets rather than counting — see untitledCounted. That is deliberate (a row
	// nobody has asked about yet is not evidence that asking is fruitless) and it is not what
	// this test is about, so get past it before measuring the widening.
	m.Update(harvestedMsg{})
	if m.untitledMisses != 0 {
		t.Fatalf("first harvest on a never-scored row should reset, got %d", m.untitledMisses)
	}

	// A harvest that names nothing widens the wait.
	for want := 1; want <= 3; want++ {
		m.lastHarvest = time.Now().Add(-untitledBackoffCap)
		m.Update(refreshTickMsg(time.Now()))
		if !m.harvesting {
			t.Fatalf("miss %d: no harvest started with the backoff elapsed", want)
		}
		m.Update(harvestedMsg{})
		if m.untitledMisses != want {
			t.Fatalf("after %d fruitless harvests untitledMisses = %d", want, m.untitledMisses)
		}
	}

	// THE WIDENED WAIT IS ACTUALLY ENFORCED. Backdated by the PREVIOUS step's delay, which the
	// flat settle delay would have accepted; the current backoff must not.
	m.lastHarvest = time.Now().Add(-untitledBackoff(m.untitledMisses - 1))
	m.Update(refreshTickMsg(time.Now()))
	if m.harvesting {
		t.Error("harvested before the backed-off interval had elapsed")
	}

	// A harvest that names something resets to the fast cadence.
	m.lastHarvest = time.Now().Add(-untitledBackoffCap)
	m.Update(refreshTickMsg(time.Now()))
	if !m.harvesting {
		t.Fatal("no harvest started with the backoff fully elapsed")
	}
	m.Update(harvestedMsg{meta: map[string]SessionMetadata{"s1": {Title: "named at last"}}})
	if m.untitledMisses != 0 {
		t.Errorf("a harvest that named a session left untitledMisses = %d", m.untitledMisses)
	}
}

// TestUntitledBackoff_DoublesAndIsBounded pins the schedule, including the overflow guard.
//
// misses is unbounded — a viewer left open overnight keeps counting — and an unguarded
// `untitledSettleDelay << misses` goes NEGATIVE past 62, which would make the gate fire on every
// tick: the exact failure the backoff exists to prevent, reached by way of its own fix.
func TestUntitledBackoff_DoublesAndIsBounded(t *testing.T) {
	if got := untitledBackoff(0); got != untitledSettleDelay {
		t.Errorf("untitledBackoff(0) = %v, want the flat settle delay %v", got, untitledSettleDelay)
	}
	if got := untitledBackoff(1); got != 2*untitledSettleDelay {
		t.Errorf("untitledBackoff(1) = %v, want %v", got, 2*untitledSettleDelay)
	}
	// Monotonic, never negative, never past the cap — including the shift-overflow range.
	prev := time.Duration(0)
	for _, misses := range []int{0, 1, 2, 3, 8, 24, 25, 62, 63, 64, 1 << 20} {
		got := untitledBackoff(misses)
		if got <= 0 {
			t.Fatalf("untitledBackoff(%d) = %v, must be positive", misses, got)
		}
		if got > untitledBackoffCap {
			t.Errorf("untitledBackoff(%d) = %v, past the cap %v", misses, got, untitledBackoffCap)
		}
		if got < prev {
			t.Errorf("untitledBackoff(%d) = %v went backwards from %v", misses, got, prev)
		}
		prev = got
	}
}

// TestPicker_HarvestsAtMostOncePerVisit pins the picker to one tree walk per visit.
//
// The namespaces/pods panes hold no session rows, so nothing there can tell a fruitless walk from
// a useful one and an interval alone would re-walk the tree for as long as the operator sits
// there. One scan is what idle time is worth spending: a session started elsewhere is named by
// the time they scroll to it.
func TestPicker_HarvestsAtMostOncePerVisit(t *testing.T) {
	m := newTitleModel(t, map[string]SessionMetadata{}, "s1")
	m.pane = paneNamespaces
	// pickerShowing = true, so the arrival edge has already been spent and what the ticks below
	// exercise is the budget rather than the edge. TestPicker_HarvestsOnFirstTickFromConstructor
	// covers the arrival itself, from an unseeded model.
	m.pickerShowing = true
	called := 0
	m.harvest = func() (map[string]SessionMetadata, error) {
		called++
		return nil, nil
	}

	_, cmd := m.Update(refreshTickMsg(time.Now()))
	runBatch(t, cmd)
	if called != 1 {
		t.Fatalf("first tick harvested %d times, want 1", called)
	}
	m.Update(harvestedMsg{})

	// A second tick in the same visit must not walk the tree again.
	_, again := m.Update(refreshTickMsg(time.Now()))
	runBatch(t, again)
	if called != 1 {
		t.Errorf("a second tick in the same visit harvested again (%d calls)", called)
	}

	// DRILLING IN IS THE SAME VISIT. enter on a namespace moves to panePods and esc comes back;
	// keying the budget on the exact pane made each hop a new visit and re-walked the whole
	// transcript tree per keystroke. The operator never left the picker, so nothing should rescan.
	m.pane = panePods
	_, hop := m.Update(refreshTickMsg(time.Now()))
	runBatch(t, hop)
	if called != 1 {
		t.Errorf("the namespaces->pods hop re-harvested (%d calls, want 1)", called)
	}
	m.pane = paneNamespaces
	_, back := m.Update(refreshTickMsg(time.Now()))
	runBatch(t, back)
	if called != 1 {
		t.Errorf("the pods->namespaces hop re-harvested (%d calls, want 1)", called)
	}

	// Leaving the picker ALTOGETHER and returning is a new visit, detected on the arrival edge
	// rather than by every assignment to m.pane announcing itself.
	m.pane = paneSessions
	m.Update(refreshTickMsg(time.Now()))
	m.pane = paneNamespaces
	_, revisit := m.Update(refreshTickMsg(time.Now()))
	runBatch(t, revisit)
	if called != 2 {
		t.Errorf("a return to the picker did not harvest again (%d calls, want 2)", called)
	}
}

// TestPicker_HarvestsOnFirstTickFromConstructor pins the arrival harvest for the visit that
// matters most: the first one, on a model straight from newPickerModel.
//
// THE ZERO VALUE HAS TO BE RIGHT HERE, and it was not when this state was a paneID. The picker
// model STARTS on paneNamespaces, so a previous-pane field zero-valuing to paneNamespaces (it is
// iota 0) recorded "already here" before any tick ran — the edge never fired, and the first visit
// to the picker harvested only because Init happens to scan separately. Every test that set the
// field by hand to match m.pane masked it. This one constructs the state the way the constructor
// leaves it and asserts the scan happens anyway.
func TestPicker_HarvestsOnFirstTickFromConstructor(t *testing.T) {
	m := newTitleModel(t, map[string]SessionMetadata{}, "s1")
	m.pane = paneNamespaces
	// Deliberately NOT seeding pickerShowing: the whole point is that the constructor does not
	// either, and the first tick must still see an arrival.
	called := 0
	m.harvest = func() (map[string]SessionMetadata, error) {
		called++
		return nil, nil
	}

	_, cmd := m.Update(refreshTickMsg(time.Now()))
	runBatch(t, cmd)
	if called != 1 {
		t.Fatalf("the first tick on a constructor-shaped picker model harvested %d times, want 1", called)
	}
	if !m.pickerShowing {
		t.Error("pickerShowing was not recorded, so the next tick will re-harvest")
	}
}

// TestBackToPodsPane_ResetsTheBackoff pins the widened backoff to the pod it was measured on.
//
// untitledMisses prices the NEXT harvest, and those misses were recorded against a session list
// that back-out throws away (m.sessions is cleared in the same block). A different pod is a
// different set of sessions with a different chance of being nameable, so carrying the counter
// across means the new pod's list waits out the old pod's penalty — at the cap, 3 minutes before
// its first scan instead of the 5s settle delay. Every other field describing the old connection
// is cleared there; this one was missed.
func TestBackToPodsPane_ResetsTheBackoff(t *testing.T) {
	m := newTitleModel(t, map[string]SessionMetadata{}, "s1")
	// backToPodsPane derives a fresh context from parentCtx.
	m.parentCtx, m.ctx = context.Background(), context.Background()
	m.cancel = func() {}
	// Six fruitless harvests is past the cap, so the failure is a 3m wait rather than a small one.
	m.untitledMisses = 6
	if untitledBackoff(m.untitledMisses) != untitledBackoffCap {
		t.Fatalf("fixture did not reach the cap: %v", untitledBackoff(m.untitledMisses))
	}

	// The set that prices the counter is part of the same state. Asserting only the counter passed
	// while the set still carried this pod's unnamed ids into the next connection.
	m.untitledCounted = map[string]bool{"shared": true}

	m.backToPodsPane()

	if m.untitledMisses != 0 {
		t.Errorf("untitledMisses = %d after backing out; the next pod inherits a %v delay",
			m.untitledMisses, untitledBackoff(m.untitledMisses))
	}
	if len(m.untitledCounted) != 0 {
		t.Errorf("untitledCounted = %v after backing out; a session id shared with the next pod "+
			"(the `default` bucket, or a redeployed agent) reads as already counted and loses its "+
			"fresh-row reset", m.untitledCounted)
	}
}

// TestUntitledSettled_BlankAndUnknownRows pins what counts as unnamed and as settled.
//
// A title of " " is non-empty to Go and blank in the column, so a raw `!= ""` suppressed the
// harvest for a row displaying nothing. A zero UpdatedAt is unknown, not quiet since the epoch.
func TestUntitledSettled_BlankAndUnknownRows(t *testing.T) {
	settled := time.Now().Add(-2 * untitledSettleDelay)

	cases := []struct {
		name string
		meta map[string]SessionMetadata
		upd  time.Time
		want bool
	}{
		{"unnamed and settled", map[string]SessionMetadata{}, settled, true},
		{"named", map[string]SessionMetadata{"s1": {Title: "known"}}, settled, false},
		{"whitespace title is unnamed", map[string]SessionMetadata{"s1": {Title: "   "}}, settled, true},
		// A CONTROL CHARACTER IS NAMED, not blank. sanitizeLabel REPLACES it with U+FFFD rather
		// than stripping it, so the cell shows a visible glyph and TrimSpace does not remove it.
		// Pinned to record which side of the line this falls on: the predicate asks what the cell
		// renders, and the cell renders something here.
		{"control-only title renders a glyph", map[string]SessionMetadata{"s1": {Title: "\t"}}, settled, false},
		{"unsettled", map[string]SessionMetadata{}, time.Now(), false},
		{"unknown UpdatedAt is not settled", map[string]SessionMetadata{}, time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTitleModel(t, tc.meta, "s1")
			m.sessions = []session.SessionSummary{{ID: "s1", UpdatedAt: tc.upd}}
			if got := m.untitledSettled(time.Now()); got != tc.want {
				t.Errorf("untitledSettled = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUntitledSettled_NamedRowWhoseTranscriptMoved pins the re-read of a NAMED row.
//
// A session started by /clear and left idle is named by its cwd. Before this, a named row was never
// read again while agentop ran, so the prompt and ai-title that came later never reached the column.
func TestUntitledSettled_NamedRowWhoseTranscriptMoved(t *testing.T) {
	settled := time.Now().Add(-2 * untitledSettleDelay)
	log := filepath.Join(t.TempDir(), "s1.jsonl")
	if err := os.WriteFile(log, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	harvested := time.Now().Add(-time.Hour)
	if err := os.Chtimes(log, harvested, harvested); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		meta SessionMetadata
		want bool
	}{
		{"unchanged since the harvest", SessionMetadata{Title: "/w/repo", LogFile: log, LogModTime: harvested}, false},
		{"changed since the harvest", SessionMetadata{Title: "/w/repo", LogFile: log, LogModTime: harvested.Add(-time.Minute)}, true},
		{"no transcript recorded", SessionMetadata{Title: "/w/repo"}, false},
		{"transcript gone", SessionMetadata{Title: "/w/repo", LogFile: log + ".gone"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTitleModel(t, map[string]SessionMetadata{"s1": tc.meta}, "s1")
			m.sessions = []session.SessionSummary{{ID: "s1", UpdatedAt: settled}}
			if got := m.untitledSettled(time.Now()); got != tc.want {
				t.Errorf("untitledSettled = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSessionsPane_RefreshOfNamedRowCountsAsMiss pins the limit on re-reads of a NAMED row.
//
// An active session's transcript moves on every turn. If a re-read that names nothing new did not
// count as a miss, every pause in that session would start a harvest at the 5s floor.
func TestSessionsPane_RefreshOfNamedRowCountsAsMiss(t *testing.T) {
	log := filepath.Join(t.TempDir(), "s1.jsonl")
	if err := os.WriteFile(log, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(log)
	if err != nil {
		t.Fatal(err)
	}
	moved := SessionMetadata{Title: "/w/repo", LogFile: log, LogModTime: st.ModTime().Add(-time.Minute)}
	m := newTitleModel(t, map[string]SessionMetadata{"s1": moved}, "s1")
	m.sessions = []session.SessionSummary{{ID: "s1", UpdatedAt: time.Now().Add(-time.Hour)}}
	m.harvest = func() (map[string]SessionMetadata, error) { return nil, nil }

	m.lastHarvest = time.Now().Add(-untitledBackoffCap)
	m.Update(refreshTickMsg(time.Now()))
	if !m.harvesting {
		t.Fatal("no harvest started for a named row whose transcript moved")
	}
	// The re-read gives the same title and the transcript's current mtime.
	m.Update(harvestedMsg{meta: map[string]SessionMetadata{
		"s1": {Title: "/w/repo", LogFile: log, LogModTime: st.ModTime()},
	}})
	if m.untitledMisses != 1 {
		t.Errorf("a re-read that named nothing new left untitledMisses = %d, want 1", m.untitledMisses)
	}

	// The transcript has not moved since that re-read, so no harvest starts.
	m.lastHarvest = time.Now().Add(-untitledBackoffCap)
	m.Update(refreshTickMsg(time.Now()))
	if m.harvesting {
		t.Error("harvested again although the transcript did not move")
	}
}

// TestHarvestNamedSomething_JudgesOnlyRowsOnScreen pins the backoff's notion of progress.
//
// Two ways to get this wrong, and the function had the second one. len(meta) > 0 is true on every
// call once the file exists, because an incremental harvest returns the whole MERGED map. Walking
// that map and asking "is this id unnamed here?" fails the same way for a subtler reason: the map
// carries every session the harvester has ever seen — ~180 on a laptop against the few a pod serves
// — so the first historical id the model has no metadata for answers yes, every time, pinning the
// backoff at zero.
func TestHarvestNamedSomething_JudgesOnlyRowsOnScreen(t *testing.T) {
	m := newTitleModel(t, map[string]SessionMetadata{"s1": {Title: "known"}}, "s1")

	if m.harvestNamedSomething(map[string]SessionMetadata{"s1": {Title: "known"}}) {
		t.Error("a map repeating a title this model already had counted as progress")
	}
	// THE HISTORICAL-SESSIONS CASE. "old" is not a row on screen, so naming it is not progress
	// toward naming what the viewer is showing — this is the assertion that fails if the function
	// goes back to iterating the result map.
	if m.harvestNamedSomething(map[string]SessionMetadata{"old": {Title: "some session from last week"}}) {
		t.Error("a title for a session that is not on screen counted as progress")
	}

	// An unnamed row on screen, which the harvest can make progress on.
	m = newTitleModel(t, map[string]SessionMetadata{}, "s1")
	if m.harvestNamedSomething(map[string]SessionMetadata{"s1": {Title: "  "}}) {
		t.Error("a blank title counted as naming a session")
	}
	// SANITISED BEFORE JUDGING, which matters HERE and not in sessionHasTitle's cases: this is the
	// one caller handing titleIsBlank a RAW harvest result, where sessionTitle has not already
	// sanitised on the way in. A control character becomes U+FFFD and renders a visible glyph, so
	// the row IS named — dropping the sanitize would call it blank and re-harvest forever for a
	// row that is already showing something.
	if !m.harvestNamedSomething(map[string]SessionMetadata{"s1": {Title: "\t"}}) {
		t.Error("a control-only title is a visible glyph in the cell, so it names the row")
	}
	if m.harvestNamedSomething(map[string]SessionMetadata{"old": {Title: "elsewhere"}}) {
		t.Error("a title for the wrong session counted as naming the row on screen")
	}
	if !m.harvestNamedSomething(map[string]SessionMetadata{"s1": {Title: "new name"}}) {
		t.Error("a title for the unnamed row on screen was not counted")
	}
}

// TestHarvestedMsg_UnscoreableHarvestsDoNotMoveTheBackoff pins fix A.
//
// harvestNamedSomething asks whether a result names a session m.sessions holds and could not name,
// so with that list EMPTY the answer is false however much the harvest learned. Counting it moved
// the backoff on no evidence — and it is the ordinary path, not a corner: Init harvests before the
// session fetch batched alongside it returns, backing out to the picker sets m.sessions to nil, and
// the picker harvests on arrival. So the normal route into a session list used to inflate
// untitledMisses several steps before the first row was drawn, starting the backoff already widened.
func TestHarvestedMsg_UnscoreableHarvestsDoNotMoveTheBackoff(t *testing.T) {
	// A harvest arriving with no session rows: neither progress nor a miss.
	m := newTitleModel(t, map[string]SessionMetadata{}, "s1")
	m.sessions = nil
	for i := 0; i < 3; i++ {
		m.Update(harvestedMsg{meta: map[string]SessionMetadata{"s1": {Title: "learned plenty"}}})
	}
	if m.untitledMisses != 0 {
		t.Errorf("harvests with no rows to judge moved the counter to %d, want 0", m.untitledMisses)
	}

	// HOLDS, rather than resetting. A harvest nobody could judge is no evidence the tree started
	// producing titles either, so an already-widened backoff must not be cleared by one.
	m.untitledMisses = 4
	m.sessions = nil
	m.Update(harvestedMsg{meta: map[string]SessionMetadata{"s1": {Title: "learned plenty"}}})
	if m.untitledMisses != 4 {
		t.Errorf("an unscoreable harvest reset the counter to %d, want it held at 4", m.untitledMisses)
	}

	// With rows present the scoring is unchanged — the guard narrows when counting happens, not
	// what counting means.
	m = newTitleModel(t, map[string]SessionMetadata{}, "s1")
	m.sessions = []session.SessionSummary{{ID: "s1", UpdatedAt: time.Now()}}
	// Twice: the first harvest meets s1 and resets on arrival, the second is the miss being
	// measured. Scoring a row the model has never seen unnamed is a reset by design.
	m.Update(harvestedMsg{meta: map[string]SessionMetadata{"other": {Title: "not on screen"}}})
	m.Update(harvestedMsg{meta: map[string]SessionMetadata{"other": {Title: "not on screen"}}})
	if m.untitledMisses != 1 {
		t.Errorf("a fruitless harvest with rows present left untitledMisses = %d, want 1", m.untitledMisses)
	}
	m.Update(harvestedMsg{meta: map[string]SessionMetadata{"s1": {Title: "named"}}})
	if m.untitledMisses != 0 {
		t.Errorf("a harvest that named a visible row left untitledMisses = %d, want 0", m.untitledMisses)
	}
}

// TestPicker_InitHarvestCountsAgainstTheVisit pins Init's own scan as the visit's one scan.
//
// Init harvests unconditionally, and in picker mode that IS the arrival harvest for the first
// visit — the operator is already on paneNamespaces when it runs. It stamped lastHarvest and set
// harvesting but not pickerHarvested, so once harvestedMsg cleared harvesting the very next tick
// found an unspent budget and walked the whole transcript tree a second time, about two seconds
// into startup. The deleted interval was the only thing suppressing that.
//
// Calls Init, which is what TestPicker_HarvestsOnFirstTickFromConstructor cannot: that test
// starts from a constructor-shaped model and never runs startup, so it sees one scan either way.
func TestPicker_InitHarvestCountsAgainstTheVisit(t *testing.T) {
	m := newTitleModel(t, map[string]SessionMetadata{}, "s1")
	m.pane = paneNamespaces
	m.ctx = context.Background()
	// Init batches loadAgentsCmd alongside the harvest, and runBatch dispatches the whole batch.
	m.lister = &fakeLister{namespaces: fixtureNamespaces}
	called := 0
	m.harvest = func() (map[string]SessionMetadata, error) {
		called++
		return nil, nil
	}

	runBatch(t, m.Init())
	if called != 1 {
		t.Fatalf("Init harvested %d times, want 1", called)
	}
	if !m.pickerHarvested {
		t.Error("Init did not spend the visit's budget, so the next tick will rescan")
	}
	m.Update(harvestedMsg{})

	// The first tick after startup must not walk the tree again: the operator has not left the
	// picker, and Init already scanned on their behalf.
	_, cmd := m.Update(refreshTickMsg(time.Now()))
	runBatch(t, cmd)
	if called != 1 {
		t.Errorf("the first tick after Init re-harvested (%d calls, want 1)", called)
	}

	// And not on the tick after that either.
	_, again := m.Update(refreshTickMsg(time.Now()))
	runBatch(t, again)
	if called != 1 {
		t.Errorf("a later tick in the same visit re-harvested (%d calls, want 1)", called)
	}
}

// TestUntitledMisses_NewSessionClearsAnotherRowsPenalty is the #1109 symptom with a longer fuse.
//
// untitledMisses is one model-wide counter, but the thing it describes — "asking about this row is
// fruitless" — is per-row. A session with no transcript under the agent's config dir can never be
// named, so it drives the counter to untitledBackoffCap. Before the arrival reset, a genuinely new
// unnamed session appearing afterwards inherited that 3m wait for its FIRST title: the backoff gate
// refused it, though nothing had ever been asked about it.
//
// DRIVES THE REAL HANDLER, not the fields. The reset has to happen where the misses are counted, so
// a test that set untitledMisses by hand would pass against a fix placed anywhere at all.
//
// ASSERTS THE COUNTER, WHICH IS NOT THE USER-VISIBLE BEHAVIOUR — and reading it as though it were is
// how a live defect sat behind a passing test. The scoring reset this checks runs after a harvest
// FINISHES, so it cannot help the row that triggered it; whether that row's harvest ever STARTS is
// decided by the backoff gate on the refreshTickMsg path. See
// TestRefreshTick_FreshRowHarvestsDespiteAnotherRowsBackoff, which measures that, and keep the two
// together: this one pins the bookkeeping, that one pins the timing.
func TestUntitledMisses_NewSessionClearsAnotherRowsPenalty(t *testing.T) {
	// One row that no harvest will ever name.
	m := newTitleModel(t, map[string]SessionMetadata{}, "unnameable")
	empty := map[string]SessionMetadata{}

	// Eight fruitless harvests on that row. Past the cap, so the failure is a 3m wait.
	for i := 0; i < 8; i++ {
		m.Update(harvestedMsg{meta: empty})
	}
	if got := untitledBackoff(m.untitledMisses); got != untitledBackoffCap {
		t.Fatalf("fixture did not reach the cap: misses=%d backoff=%v", m.untitledMisses, got)
	}

	// A new session appears — the ordinary case, an operator watching a pod while an unnameable
	// row sits on screen. The poll path replaces the slice wholesale; both rows are unnamed.
	m.sessions = append(m.sessions, session.SessionSummary{ID: "brandnew", UpdatedAt: time.Now()})

	// The next harvest still names nothing: the new session's transcript is not readable yet,
	// which is exactly when its first retry matters most.
	m.Update(harvestedMsg{meta: empty})

	if m.untitledMisses != 0 {
		t.Errorf("untitledMisses = %d after a previously-unseen unnamed session arrived; "+
			"its first title waits %v, earned by a different row",
			m.untitledMisses, untitledBackoff(m.untitledMisses))
	}
}

// TestUntitledMisses_SameUnnameableRowKeepsBackingOff is the other half of the arrival reset.
//
// The reset keys on a row this scoring has not seen unnamed before. If it instead fired whenever
// any unnamed row was present, the backoff would be permanently reset and the loop it exists to
// break — a full transcript-tree walk every untitledSettleDelay for a title that is never coming —
// would be back with extra code. So: the same row asked again must still widen the wait.
func TestUntitledMisses_SameUnnameableRowKeepsBackingOff(t *testing.T) {
	m := newTitleModel(t, map[string]SessionMetadata{}, "unnameable")
	empty := map[string]SessionMetadata{}

	// FOUR HARVESTS, THREE MISSES. The first one meets this row for the first time and resets,
	// which is the arrival reset working as intended rather than an off-by-one: a row that has
	// never been asked about is not evidence that asking is fruitless. Only the repeats count.
	for i := 0; i < 4; i++ {
		m.Update(harvestedMsg{meta: empty})
	}

	if m.untitledMisses != 3 {
		t.Errorf("untitledMisses = %d after 4 fruitless harvests on one unchanged row, want 3 "+
			"(backoff %v); the arrival reset is firing on a row it already counted",
			m.untitledMisses, untitledBackoff(m.untitledMisses))
	}
}

// TestUntitledMisses_ReturningSessionCountsAsNew pins that the set is rebuilt, not appended to.
//
// A row that leaves the list and comes back is a new row to an operator watching the pane, and
// agentop's own docs note a session id can be re-created after eviction. An append-only set would
// remember it as "already counted" and make its first title wait out a backoff earned before it
// went away.
func TestUntitledMisses_ReturningSessionCountsAsNew(t *testing.T) {
	m := newTitleModel(t, map[string]SessionMetadata{}, "comesback")
	empty := map[string]SessionMetadata{}

	for i := 0; i < 8; i++ {
		m.Update(harvestedMsg{meta: empty})
	}
	if got := untitledBackoff(m.untitledMisses); got != untitledBackoffCap {
		t.Fatalf("fixture did not reach the cap: misses=%d backoff=%v", m.untitledMisses, got)
	}

	// Gone: the poll returned a list without it. Scored while absent, so the set forgets it.
	m.sessions = []session.SessionSummary{{ID: "other", UpdatedAt: time.Now()}}
	m.sessionsData = map[string]SessionMetadata{"other": {Title: "named"}}
	m.Update(harvestedMsg{meta: empty})

	// Back again, still unnamed.
	m.sessions = append(m.sessions, session.SessionSummary{ID: "comesback", UpdatedAt: time.Now()})
	m.Update(harvestedMsg{meta: empty})

	if m.untitledMisses != 0 {
		t.Errorf("untitledMisses = %d after a session returned to the list; "+
			"the set is remembering ids whose rows are gone", m.untitledMisses)
	}
}

// TestUntitledMisses_TitledRowsStayOutOfTheSet pins that only rows counted against the backoff are
// remembered.
//
// A row that already has a title is not what the backoff is about. Admitting it to the set would
// mean a row LOSING its title later — a hand-edited metadata file, a merge that blanks one — read
// as "not new" and skipped the reset, leaving it to wait out a penalty it never earned.
func TestUntitledMisses_TitledRowsStayOutOfTheSet(t *testing.T) {
	m := newTitleModel(t, map[string]SessionMetadata{"s1": {Title: "known"}}, "s1", "unnameable")
	empty := map[string]SessionMetadata{}

	for i := 0; i < 8; i++ {
		m.Update(harvestedMsg{meta: empty})
	}
	if got := untitledBackoff(m.untitledMisses); got != untitledBackoffCap {
		t.Fatalf("fixture did not reach the cap: misses=%d backoff=%v", m.untitledMisses, got)
	}

	// s1 loses its title. It is now an unnamed row that was never counted as one.
	m.sessionsData = map[string]SessionMetadata{}
	m.Update(harvestedMsg{meta: empty})

	if m.untitledMisses != 0 {
		t.Errorf("untitledMisses = %d after a titled row went blank; it is being treated as "+
			"already counted", m.untitledMisses)
	}
}

// TestUntitledSettled_FutureUpdatedAtIsNotQuietForever covers client/server clock skew.
//
// now is the laptop's clock; UpdatedAt was stamped in the pod. Nothing keeps them in step — the
// pane reaches the store through a port-forward, Kubernetes does not synchronise node clocks, and
// a laptop that slept is the ordinary way the gap gets large. With a plain now.Sub, a pod clock
// even 5s ahead makes the delta negative, which can never reach untitledSettleDelay: that row's
// title never arrives, with no error and no log. Treating a future stamp as settled costs one
// wasted tree walk that the backoff then widens.
func TestUntitledSettled_FutureUpdatedAtIsNotQuietForever(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name string
		upd  time.Time
		want bool
	}{
		// The skew only has to exceed untitledSettleDelay to strand a row forever.
		{"pod clock slightly ahead", now.Add(2 * untitledSettleDelay), true},
		{"pod clock badly ahead", now.Add(36 * time.Hour), true},
		// Unchanged behaviour on the in-step cases, so the clamp cannot be mistaken for
		// "always settled".
		{"quiet long enough", now.Add(-2 * untitledSettleDelay), true},
		{"still busy", now, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTitleModel(t, map[string]SessionMetadata{}, "s1")
			m.sessions[0].UpdatedAt = tc.upd
			if got := m.untitledSettled(now); got != tc.want {
				t.Errorf("untitledSettled = %v, want %v (UpdatedAt %v from now)",
					got, tc.want, tc.upd.Sub(now))
			}
		})
	}
}

// TestUntitledMisses_ReturningSessionWithNoInterveningScoring is the path
// TestUntitledMisses_ReturningSessionCountsAsNew does not reach.
//
// That test scores a non-empty list while the session is away, which rebuilds the set and drops the
// absent id. This one never does: the list goes empty and the harvests that land while it is empty
// are unscoreable. Those are the ordinary ones — Init harvests before its session fetch returns,
// backing out to the picker sets m.sessions = nil, and the picker harvests on arrival.
//
// With the set maintained only inside the len(m.sessions) > 0 guard, the empty scoring left the
// previous list's ids in place, so the returning row read as "already counted" and inherited the
// full 3m cap for its first title — measured at misses=9. The set now rebuilds on every harvest:
// an empty list has no unnamed rows, so the set correctly empties and the row is new again.
func TestUntitledMisses_ReturningSessionWithNoInterveningScoring(t *testing.T) {
	m := newTitleModel(t, map[string]SessionMetadata{}, "comesback")
	m.sessions = []session.SessionSummary{{ID: "comesback", UpdatedAt: time.Now().Add(-time.Hour)}}
	empty := map[string]SessionMetadata{}

	for i := 0; i < 9; i++ {
		m.Update(harvestedMsg{meta: empty})
	}
	if got := untitledBackoff(m.untitledMisses); got != untitledBackoffCap {
		t.Fatalf("fixture did not reach the cap: misses=%d backoff=%v", m.untitledMisses, got)
	}

	// Gone, and the only harvest landing while it is away has no rows to score.
	m.sessions = nil
	m.Update(harvestedMsg{meta: empty})

	// THE COUNTER MUST NOT HAVE MOVED. An unscoreable harvest is neither progress nor a miss —
	// pinned here too, because rebuilding the set outside the scoring guard must not be mistaken
	// for scoring outside it.
	if m.untitledMisses == 0 {
		t.Errorf("an unscoreable harvest reset untitledMisses; the set rebuild leaked into scoring")
	}

	// Back again, still unnamed and settled.
	m.sessions = []session.SessionSummary{{ID: "comesback", UpdatedAt: time.Now().Add(-6 * time.Second)}}
	m.Update(harvestedMsg{meta: empty})

	if m.untitledMisses != 0 {
		t.Errorf("untitledMisses = %d after the session returned with no non-empty scoring in "+
			"between; its first title waits %v", m.untitledMisses, untitledBackoff(m.untitledMisses))
	}
}

// TestBackToPodsPane_ClearsTheCountedSet covers the pod switch end to end, through the real
// harvestedMsg handler rather than by assigning the set.
//
// Two pods can hold a session with the same id: `default` is the bucket every pod's denial events
// aggregate into, and a redeployed agent can reuse one. Carrying the previous pod's set across the
// switch made that id read as already counted, so the new pod's first harvest counted a miss the
// row had not earned instead of resetting on arrival.
func TestBackToPodsPane_ClearsTheCountedSet(t *testing.T) {
	m := newTitleModel(t, map[string]SessionMetadata{}, "default")
	m.parentCtx, m.ctx = context.Background(), context.Background()
	m.cancel = func() {}
	m.sessions = []session.SessionSummary{{ID: "default", UpdatedAt: time.Now().Add(-time.Hour)}}
	empty := map[string]SessionMetadata{}

	for i := 0; i < 9; i++ {
		m.Update(harvestedMsg{meta: empty})
	}
	if got := untitledBackoff(m.untitledMisses); got != untitledBackoffCap {
		t.Fatalf("fixture did not reach the cap: misses=%d backoff=%v", m.untitledMisses, got)
	}

	m.backToPodsPane()

	// A different pod, whose list also holds a `default` session — unnamed, settled.
	m.sessions = []session.SessionSummary{{ID: "default", UpdatedAt: time.Now().Add(-6 * time.Second)}}
	m.Update(harvestedMsg{meta: empty})

	if m.untitledMisses != 0 {
		t.Errorf("untitledMisses = %d on the new pod's first scoring of a shared session id; "+
			"the previous pod's set suppressed the fresh-row reset", m.untitledMisses)
	}
}

// TestRefreshTick_FreshRowHarvestsDespiteAnotherRowsBackoff is the arrival reset measured where an
// operator feels it: whether a harvest actually STARTS.
//
// THE COUNTER TESTS ABOVE CANNOT CATCH THIS, which is why this one exists. They hand the handler a
// harvestedMsg and assert untitledMisses == 0 — true, and irrelevant to the row that needed it. The
// scoring reset runs when a harvest FINISHES; the backoff gate on the refreshTickMsg path decides
// whether one BEGINS. So with the reset in place and the gate reading the raw counter, an unnameable
// row at untitledBackoffCap still made a brand-new settled session wait 3m for its first title, and
// every arrival-reset test passed the whole time. The gate is the only place this is observable.
//
// DRIVES refreshTickMsg THROUGH Update, not the gate expression, so it fails if the freshness check
// is placed anywhere that is not the decision itself.
func TestRefreshTick_FreshRowHarvestsDespiteAnotherRowsBackoff(t *testing.T) {
	m := newTitleModel(t, map[string]SessionMetadata{}, "unnameable")
	empty := map[string]SessionMetadata{}
	m.harvest = func() (map[string]SessionMetadata, error) { return empty, nil }

	// Drive the unnameable row past the cap through the real handler.
	m.sessions = []session.SessionSummary{{ID: "unnameable", UpdatedAt: time.Now().Add(-time.Minute)}}
	for i := 0; i < 10; i++ {
		m.Update(harvestedMsg{meta: empty})
	}
	if got := untitledBackoff(m.untitledMisses); got != untitledBackoffCap {
		t.Fatalf("fixture did not reach the cap: misses=%d backoff=%v", m.untitledMisses, got)
	}

	// A new session appears and settles. Ten seconds since the last harvest: past
	// untitledSettleDelay, nowhere near the 3m the other row earned.
	m.harvesting = false
	m.lastHarvest = time.Now().Add(-10 * time.Second)
	m.sessions = append(m.sessions,
		session.SessionSummary{ID: "brandnew", UpdatedAt: time.Now().Add(-untitledSettleDelay * 2)})

	m.Update(refreshTickMsg{})

	// m.harvesting is the gate's own record that it fired, and the one the batched command is
	// guarded by — asserting the returned tea.Cmd is non-nil would pass on the refresh tick alone.
	if !m.harvesting {
		t.Errorf("no harvest started for a brand-new settled session while another row held the "+
			"backoff at %v; its first title waits for a penalty it did not earn",
			untitledBackoff(m.untitledMisses))
	}
}

// TestRefreshTick_UnnameableRowStillBacksOffAtTheGate is the other side of the gate check.
//
// Forgiving the backoff for a row the set has not seen must not forgive it for the row that earned
// it. If untitledFresh answered yes whenever any unnamed row was on screen — or if the gate used the
// floor unconditionally — the transcript-tree walk every untitledSettleDelay would be back, which is
// the loop untitledMisses exists to break.
func TestRefreshTick_UnnameableRowStillBacksOffAtTheGate(t *testing.T) {
	m := newTitleModel(t, map[string]SessionMetadata{}, "unnameable")
	empty := map[string]SessionMetadata{}
	m.harvest = func() (map[string]SessionMetadata, error) { return empty, nil }
	m.sessions = []session.SessionSummary{{ID: "unnameable", UpdatedAt: time.Now().Add(-time.Minute)}}

	// Six harvests: five misses, so the earned wait is well past untitledSettleDelay.
	for i := 0; i < 6; i++ {
		m.Update(harvestedMsg{meta: empty})
	}
	earned := untitledBackoff(m.untitledMisses)
	if earned <= untitledSettleDelay {
		t.Fatalf("fixture did not back off: misses=%d backoff=%v", m.untitledMisses, earned)
	}

	// Past the settle floor, inside the earned backoff. Nothing new on screen.
	m.harvesting = false
	m.lastHarvest = time.Now().Add(-untitledSettleDelay * 2)

	m.Update(refreshTickMsg{})

	if m.harvesting {
		t.Errorf("harvested %v after the last one despite an earned backoff of %v; the same "+
			"unnameable row re-walks the transcript tree", untitledSettleDelay*2, earned)
	}
}

// TestRefreshTick_FreshRowStillWaitsToSettle keeps the arrival reset from eating the settle delay.
//
// A new row forgives the accumulated PENALTY, not the wait that makes this poll affordable: its
// transcript is being appended to, and the title tiers read the last message, so harvesting the
// instant a session appears reads a file the agent is still writing. The gate prices a fresh row at
// untitledBackoff(0), which IS untitledSettleDelay — not at zero.
func TestRefreshTick_FreshRowStillWaitsToSettle(t *testing.T) {
	m := newTitleModel(t, map[string]SessionMetadata{}, "unnameable")
	empty := map[string]SessionMetadata{}
	m.harvest = func() (map[string]SessionMetadata, error) { return empty, nil }
	m.sessions = []session.SessionSummary{{ID: "unnameable", UpdatedAt: time.Now().Add(-time.Minute)}}
	for i := 0; i < 6; i++ {
		m.Update(harvestedMsg{meta: empty})
	}

	// Long past any backoff, so the settle test is the only thing that can refuse. The new row is
	// BUSY — updated a moment ago, transcript still being written.
	m.harvesting = false
	m.lastHarvest = time.Now().Add(-time.Hour)
	m.sessions = []session.SessionSummary{{ID: "brandnew", UpdatedAt: time.Now()}}

	m.Update(refreshTickMsg{})

	if m.harvesting {
		t.Errorf("harvested a brand-new session that is still receiving events; the settle delay " +
			"exists because the title tiers read a transcript the agent has not finished writing")
	}
}

// An oversized file must be REFUSED WHOLE, not read as a valid prefix of itself.
//
// What the cap does and does not protect against, measured rather than assumed. A file over the
// limit was read to exactly maxMetadataBytes with no +1, and what happened next depended entirely
// on where the cut landed:
//
//   - Cut mid-token — the overwhelmingly common case — Unmarshal fails with "unexpected end of
//     JSON input" and the result is the empty map. Identical before and after this fix, and not
//     data loss beyond the TITLE column this function is contracted to lose on any failure.
//   - Cut where the prefix is INDEPENDENTLY VALID JSON, which trailing whitespace past the cap
//     produces: the prefix decoded and the viewer loaded a partial map, believing it complete,
//     while claude.ReadMetadata refused the very same file with ErrMetadataTooLarge. Measured: a
//     16,777,238-byte file loaded 1 entry before the fix and 0 after.
//
// The second is the one worth a test, because it is the only shape where the two readers of this
// file disagreed about its CONTENTS rather than merely about how loudly to fail.
func TestLoadSessionMetadata_AtTheReadCap(t *testing.T) {
	const cap = 16 << 20
	path := filepath.Join(t.TempDir(), "session-metadata.json")

	// A valid object followed by enough whitespace to push the file past the cap. The first
	// cap bytes are therefore valid JSON on their own — which is exactly what makes a truncated
	// read decode successfully and silently drop whatever followed.
	body := `{"a":{"title":"real"}}`
	over := []byte(body + strings.Repeat(" ", cap))
	if len(over) <= cap {
		t.Fatalf("fixture is %d bytes, within the %d cap: it cannot test the over case", len(over), cap)
	}
	if err := os.WriteFile(path, over, 0o600); err != nil {
		t.Fatal(err)
	}
	// Zero, not one. One means a prefix decoded and the viewer is now showing a file it only
	// partly read, with no error anywhere and claude.ReadMetadata refusing the same bytes.
	if got := LoadSessionMetadata(path); len(got) != 0 {
		t.Errorf("loaded %d entries from a %d byte file: a valid prefix of an oversize file decoded",
			len(got), len(over))
	}

	// And the cap still admits everything under it, so the guard is a limit and not a wall.
	under := []byte(body + strings.Repeat(" ", 1024))
	if err := os.WriteFile(path, under, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LoadSessionMetadata(path); len(got) != 1 || got["a"].Title != "real" {
		t.Errorf("loaded %d entries from a %d byte file, want the 1 real entry", len(got), len(under))
	}
}

// A served title fills a cell the harvest could not name.
//
// The motivating case, and the reason the fallback exists at all: on the laptop this was found on,
// every blank harvested entry belonged to an agent with no Claude Code transcript tree to read — but
// one that routes through the proxy, so /v1/sessions had derived a title for exactly those sessions.
// A blank TITLE was never "this session has no name", only "no name where agentop was looking".
func TestSessionsPane_ServedTitleFillsAnUnharvestedCell(t *testing.T) {
	m := newServedTitleModel(t,
		map[string]SessionMetadata{},
		map[string]string{"s1": "Investigate the flaky reloader test"},
		"s1")
	m.rebuildSessionsTable()

	if got := sessionsCell(t, m, titleRow(t, m, "s1"), "TITLE"); got != "Investigate the flaky reloader test" {
		t.Errorf("TITLE = %q, want the served title", got)
	}
	// And through the header, which an operator reaches by pressing Enter on that same row. A row
	// selected BY its served title must not lose it one keystroke later.
	if got := m.sessionLabel("s1"); got != "Investigate the flaky reloader test (s1)" {
		t.Errorf("sessionLabel = %q, want the served title and the id", got)
	}
}

// Each session gets ITS OWN served title, which needs two of them to say anything at all.
//
// servedTitle walks m.sessions comparing ids, and against a one-session fixture that walk is
// indistinguishable from returning the first entry unconditionally — `if s.ID == id` mutated to
// `if s.ID != ""` survived every other test in this package. The bug that hides there is not
// exotic: on any pod listing two sessions, session B's header would show A's title. So the
// fixture lists two, and the unlisted id pins the miss path that returns "".
func TestSessionMetadata_ServedTitleIsPerSession(t *testing.T) {
	m := newServedTitleModel(t, map[string]SessionMetadata{},
		map[string]string{"s1": "first", "s2": "second"}, "s1", "s2")

	for id, want := range map[string]string{
		"s1": "first (s1)",
		"s2": "second (s2)",
		// Listed by nobody: servedTitle finds no match and the bare id is all a header can say.
		"gone": "gone",
	} {
		if got := m.sessionLabel(id); got != want {
			t.Errorf("sessionLabel(%q) = %q, want %q", id, got, want)
		}
	}
}

// The ROW LOOP pairs each session with ITS OWN served title, which only two rows can show.
//
// The sibling above pins the same property for the HEADER, and that is why this one is separate
// rather than an extra assertion there: the two paths reach the served title differently. The header
// calls servedTitle(id), a lookup by id. The row loop never looks anything up — it renders from the
// summary it is already iterating and passes s.Title straight into sessionTitleCell. A lookup can be
// wrong about WHICH id it matched; a loop can be wrong about WHICH ITERATION it read from, and no
// test of the lookup can see that.
//
// Review found the gap by mutating the loop to pass m.sessions[0].Title instead of s.Title — every
// row rendering the FIRST session's title — and the whole package stayed green, because every
// fixture that put a served title in the table had exactly one row. On a pod listing two sessions
// that mutant labels B's row with A's name, which is indistinguishable from the bug this feature
// exists to fix except that it is confidently wrong rather than blank.
//
// Distinct titles in BOTH directions, so neither "always the first" nor "always the last" passes.
func TestSessionsPane_ServedTitleCellIsPerRow(t *testing.T) {
	m := newServedTitleModel(t, map[string]SessionMetadata{},
		map[string]string{"s1": "first session", "s2": "second session"}, "s1", "s2")
	m.rebuildSessionsTable()

	for id, want := range map[string]string{
		"s1": "first session",
		"s2": "second session",
	} {
		if got := sessionsCell(t, m, titleRow(t, m, id), "TITLE"); got != want {
			t.Errorf("TITLE for %s = %q, want %q", id, got, want)
		}
	}
}

// capTitleRunes MUST NOT EMPTY A TITLE THAT RENDERS SOMETHING, whatever the cluster structure.
//
// This is the property, stated once, over every degenerate shape found so far. It matters because
// sessionHasTitle reads sessionTitle: a named row whose title caps to "" reads as UNNAMED, which
// zeroes untitledMisses and restarts the permanent ~3-minute re-harvest. f5a0a615 fixed that for
// whitespace; two more routes to the same failure have shipped since, both in the cap's walk:
//
//   - 41 consecutive flags capped to "", because regional indicators pair and a walk that treated
//     every one as a binder ran to index 0.
//   - "a" + 100 combining marks capped to "", because the walk's documented bound — "a mark cannot
//     follow a mark" — is false. Marks stack, so the run walked to the base and past it.
//
// Both were found by review rather than by a test, which is the argument for asserting the INVARIANT
// over a table of shapes instead of the arithmetic of any one of them. A new binder class added to
// bindsToPrevious gets this check for free; an expected-length assertion would not.
//
// A THIRD ROUTE WAS FOUND WITH THIS TEST ALREADY GREEN, and it is why the table now carries
// whitespace shapes. Every fixture here was binder-class, so none of them exercised the cap's TRIM \u2014
// and the trim ran AFTER the cut, so a title whose first MaxTitleLen runes were whitespace lost its
// real text to the cut and was then emptied by the trim. 80 leading spaces plus "my-project"
// returned "". The property was right and the table could not reach the defect: the lesson is that
// an invariant test is only as broad as its shapes, so a fixture belongs here for every code path
// inside the function, not just for every bug that has been found in one of them.
func TestSessionsPane_CapNeverEmptiesANonBlankTitle(t *testing.T) {
	const acute = "\u0301" // Mn, stacks without limit
	flag := "\U0001F1FA\U0001F1F8"
	// Over the cap on their own, so the cut fires before any trim can.
	const pad = claude.MaxTitleLen + 5

	for _, tc := range []struct{ name, in string }{
		// THE TRIM ROUTE. Whitespace is not binder-class, so none of the cluster fixtures below
		// reach it: these cover the cut-then-trim interaction instead. Three whitespace classes,
		// because TrimSpace is Unicode-aware and a fix that only counted U+0020 would pass on one.
		{"long space prefix then text", strings.Repeat(" ", pad) + "my-project"},
		{"long NBSP prefix then text", strings.Repeat("\u00a0", pad) + "my-project"},
		{"long ideographic-space prefix then text", strings.Repeat("\u3000", pad) + "my-project"},
		// Exactly at the boundary, where the old code first returned "" rather than one rune.
		{"space prefix exactly at the cap", strings.Repeat(" ", claude.MaxTitleLen) + "my-project"},
		// Interior whitespace must not be mistaken for the same thing: this one has real text
		// inside the first MaxTitleLen runes and must keep some of it either way.
		{"text then long space run then text", "a" + strings.Repeat(" ", pad) + "b"},
		// The two shipped defects, at the sizes they were measured at.
		{"base plus a long mark run", "a" + strings.Repeat(acute, 100)},
		{"consecutive flags", strings.Repeat(flag, 41)},
		{"text then flags", strings.Repeat("a", 70) + strings.Repeat(flag, 10)},
		// No base character at all: every rune binds, so there is no boundary to cut on and the
		// cluster-start walk correctly reports 0. The blunt cut is the deliberate fallback — a
		// dangling mark renders oddly, "" restarts the re-harvest.
		{"nothing but marks", strings.Repeat(acute, 100)},
		{"nothing but one flag half", strings.Repeat("\U0001F1FA", 100)},
		// Mixed, because a run of one binder class is not the only degenerate shape.
		{"marks and flags interleaved", strings.Repeat(acute+flag, 40)},
	} {
		got := capTitleRunes(tc.in)
		if titleIsBlank(tc.in) {
			t.Fatalf("%s: fixture is blank before capping, so it proves nothing", tc.name)
		}
		if titleIsBlank(got) {
			t.Errorf("%s: capTitleRunes(%d runes) = %q, which is blank — this flips sessionHasTitle and restarts the re-harvest",
				tc.name, utf8.RuneCountInString(tc.in), got)
		}
		if n := utf8.RuneCountInString(got); n > claude.MaxTitleLen {
			t.Errorf("%s: capTitleRunes returned %d runes, over the %d cap", tc.name, n, claude.MaxTitleLen)
		}
	}
}

// A WHITESPACE-PREFIXED HARVESTED TITLE MUST STILL READ AS NAMED, end to end.
//
// This is the reported symptom rather than the mechanism, and it is asserted through
// sessionHasTitle on purpose: the cap tests above check the string, and this one checks the
// CONSEQUENCE. sessionHasTitle gates all four backoff predicates (untitledSettled, untitledFresh,
// countUntitled, harvestNamedSomething), so a title emptied by the cap does not merely render blank
// — it zeroes untitledMisses and re-harvests ~/.claude every ~3 minutes for the life of the process,
// on a session the harvest has ALREADY successfully named. That is the cost this file documents as
// the price of an unnamable session, billed to a named one.
//
// The lengths bracket the old failure curve: at 40 leading spaces the old code kept 50 runes, at 79
// it kept exactly "m", and at 80 and beyond it returned "". All of them must now read as named.
func TestSessionsPane_WhitespacePrefixedHarvestStillReadsAsNamed(t *testing.T) {
	for _, ws := range []struct{ name, r string }{
		{"space", " "},
		{"no-break space", " "},
		{"ideographic space", "　"},
	} {
		for _, n := range []int{40, claude.MaxTitleLen - 1, claude.MaxTitleLen, claude.MaxTitleLen + 5, 500} {
			m := newTitleModel(t, map[string]SessionMetadata{
				"s1": {Title: strings.Repeat(ws.r, n) + "my-project"},
			}, "s1")

			if got := m.sessionTitle("s1"); titleIsBlank(got) {
				t.Errorf("%s x%d: sessionTitle = %q, blank — the harvest named this session", ws.name, n, got)
			}
			if !m.sessionHasTitle("s1") {
				t.Errorf("%s x%d: sessionHasTitle = false for a named session; this restarts the permanent re-harvest", ws.name, n)
			}
			// The name itself must survive, not just some non-blank remnant. The old code's
			// 79-space case kept "m", which is non-blank and useless.
			if got := m.sessionTitle("s1"); got != "my-project" {
				t.Errorf("%s x%d: sessionTitle = %q, want the name with its padding trimmed", ws.name, n, got)
			}
		}
	}
}

// THE WALK IS BOUNDED BY ONE CLUSTER, not by the title — the claim the old comment got wrong.
//
// The superseded bound was "at most one step for marks and ZWJ, because a mark cannot follow a
// mark". Marks do follow marks, so the real bound has to come from somewhere else: clusterStart
// stops at the first rune that does not bind, so the work and the loss are both proportional to the
// ONE cluster straddling the cut.
//
// Growing that cluster from 5 marks to 2000 must not move the cut, and marks parked far past the cut
// must not move it either. An expected-length assertion is the point here rather than an invariant:
// the defect this replaces was measurable only as "how much did it lose", and 2000 marks losing the
// same 1 rune as 5 is what says the walk terminates at the base instead of running through it.
func TestSessionsPane_CapWalkIsBoundedByOneCluster(t *testing.T) {
	const acute = "\u0301"

	// A cluster straddling the cut: 79 plain runes, then a base at 79 carrying every mark. The only
	// boundary at or before the cut is 79, so that is where it must land, for any run length.
	for _, marks := range []int{5, 40, 200, 2000} {
		in := strings.Repeat("a", 79) + "e" + strings.Repeat(acute, marks) + "tail"
		got := capTitleRunes(in)
		if want := strings.Repeat("a", 79); got != want {
			t.Errorf("straddling cluster with %d marks: got %d runes, want the 79 before the base",
				marks, utf8.RuneCountInString(got))
		}
	}
	// Marks far PAST the cut are not the cut's business at all: index 80 sits inside a run of plain
	// letters, which is already a boundary, so nothing should be walked back.
	for _, marks := range []int{2, 200, 2000} {
		in := strings.Repeat("a", 100) + strings.Repeat(acute, marks)
		if got := utf8.RuneCountInString(capTitleRunes(in)); got != claude.MaxTitleLen {
			t.Errorf("trailing %d marks: got %d runes, want the full %d — the cut is on a boundary already",
				marks, got, claude.MaxTitleLen)
		}
	}
	// THE WORST CASE IS THE CUT, NOT THE TITLE, and this is the case a reviewer read the doc as
	// denying. A cluster degenerate from index 0 has no boundary anywhere, so the scan runs the whole
	// way back — but that is MaxTitleLen steps and not len(title) steps, so growing the title 20x
	// past the cut must not cost anything. Asserted through clusterStart directly, because
	// capTitleRunes' blunt-cut fallback returns the same answer either way and would hide it.
	for _, marks := range []int{100, 2000} {
		r := []rune("a" + strings.Repeat(acute, marks))
		if got := clusterStart(r, claude.MaxTitleLen); got != 0 {
			t.Errorf("degenerate cluster with %d marks: clusterStart = %d, want 0 — every rune binds", marks, got)
		}
	}
}

// clusterStart MUST NOT PANIC WHEN THE CUT IS PAST THE LAST RUNE.
//
// clusterStart([]rune("abc"), 3) indexed r[3] and panicked with "index out of range [3] with length
// 3". It was safe in production only because capTitleRunes' early return guarantees len(r) >
// MaxTitleLen before it ever calls — a coupling two functions apart that no comment stated and no
// test pinned, while bindsToPrevious' own doc invites other callers in this package. The smallest
// fixture anywhere in this file is 89 runes, so nothing came close to it.
//
// cut == len(r) means "the cut is past the last rune": r[cut] does not exist, nothing can be
// orphaned, and the answer is cut itself. Short strings are checked at their own length rather than
// at MaxTitleLen so the boundary is the SUBJECT of the test and not incidental to it.
func TestSessionsPane_ClusterStartHandlesACutAtTheEnd(t *testing.T) {
	const acute = "́"
	for _, in := range []string{"abc", "", "a", strings.Repeat("a", claude.MaxTitleLen), "e" + acute} {
		r := []rune(in)
		got := clusterStart(r, len(r))
		if got != len(r) {
			t.Errorf("clusterStart(%q, %d) = %d, want %d — a cut past the last rune orphans nothing",
				in, len(r), got, len(r))
		}
	}
	// The one length that used to be load-bearing: exactly MaxTitleLen + 1 runes is the shortest
	// string capTitleRunes will cut, so it is the shortest slice clusterStart has ever been handed.
	// A test at 89 runes cannot tell whether the bound is len(r) or something larger.
	r := []rune(strings.Repeat("a", claude.MaxTitleLen+1))
	if got := clusterStart(r, claude.MaxTitleLen); got != claude.MaxTitleLen {
		t.Errorf("clusterStart at the shortest cuttable length = %d, want %d", got, claude.MaxTitleLen)
	}
}

// Harvest wins when both sources name the session.
//
// A fixed precedence, not a judgement about which string is better — see sessionTitleFor. Neither
// side is the "stable" one: the harvest is LAST-wins (core/observe/claude, every tier) and the
// served title is FIRST-wins, so they disagree about which turn should name a session rather than
// one of them being steadier. Asserted at the cell AND the header, because they reach the fallback
// by different routes (the row loop carries the summary; the header looks it up).
func TestSessionsPane_HarvestBeatsServedTitle(t *testing.T) {
	m := newServedTitleModel(t,
		map[string]SessionMetadata{"s1": {Title: "harvested name"}},
		map[string]string{"s1": "served name"},
		"s1")
	m.rebuildSessionsTable()

	if got := sessionsCell(t, m, titleRow(t, m, "s1"), "TITLE"); got != "harvested name" {
		t.Errorf("TITLE = %q, want the harvested title to win", got)
	}
	if got := m.sessionLabel("s1"); got != "harvested name (s1)" {
		t.Errorf("sessionLabel = %q, want the harvested title to win", got)
	}
}

// HARVEST WINS EVEN WHEN THE CAP MANGLES THE HARVESTED TITLE, because precedence is a question about
// the harvest and not about the display bound.
//
// sessionTitleFor used to ask whether m.sessionTitle(id) — sanitised AND CAPPED — was blank. So every
// route by which the cap could blank a non-blank harvested title ALSO silently inverted the
// documented precedence: with the cap trimming after its cut, sessionTitleFor(85 spaces + "real",
// "served-name") returned "served-name". That is worse than the blank cell it replaced. An operator
// picks a row by its name before acting on it, and cmd/agentop/README.md and
// core/session/summary.go both tell them the harvest is what they are looking at.
//
// NOT A MUTATION GATE, and saying so is the honest version of this test. Reverting the guard to
// !titleIsBlank(m.sessionTitle(id)) leaves the whole suite green — verified by running it. It has to:
// the guard only behaves differently when the cap can blank a non-blank title, and that is exactly
// what the whitespace fix and the blunt-cut fallback between them removed. A search over the
// degenerate shapes this file knows — combining marks, ZWJ, BOM, word joiner, NBSP, ideographic
// space, lone regional indicators, Thai vowel signs, at five lengths around the cap, with and
// without a real suffix — found ZERO inputs that sanitise non-blank and cap to blank. With no such
// input there is no observable difference, so no black-box test can pin the structure.
//
// It is kept anyway, for the two things it does do: it pins the OUTCOME (the measured inversion
// string now resolves to the harvested title, at the cell and the header), and it is where the next
// person who reintroduces a cap-blanking route will find out what else breaks. The structural form
// of the guard is defence for when that search stops being exhaustive — a display bound should not
// be able to reach into a precedence decision at all — and its justification is the argument in
// sessionTitleFor's doc, not a failing assertion here.
func TestSessionsPane_HarvestWinsEvenWhenTheCapShortensIt(t *testing.T) {
	const acute = "́"
	for _, tc := range []struct {
		name      string
		harvested string
	}{
		// The measured inversion: leading whitespace long enough to consume the whole budget.
		{"long space prefix", strings.Repeat(" ", claude.MaxTitleLen+5) + "real"},
		// A degenerate cluster from index 0, where the cap has no boundary to cut on and falls
		// back to a blunt cut. If that fallback is ever removed, the served title takes over a
		// named row rather than the cell merely going blank — this pins both consequences at once.
		{"nothing but combining marks", strings.Repeat(acute, claude.MaxTitleLen+20)},
	} {
		m := newServedTitleModel(t,
			map[string]SessionMetadata{"s1": {Title: tc.harvested}},
			map[string]string{"s1": "served-name"},
			"s1")
		m.rebuildSessionsTable()

		// Asserted at the accessor AND both display paths, because the served title reaches them by
		// different routes — the row loop carries the summary, sessionLabel looks it up — and the
		// inversion showed the proxy's name in all three.
		if got := m.sessionTitleFor("s1", "served-name"); got == "served-name" {
			t.Errorf("%s: sessionTitleFor returned the SERVED title, inverting the documented harvest-wins precedence", tc.name)
		} else if titleIsBlank(got) {
			t.Errorf("%s: sessionTitleFor = %q, blank — the harvested title named this session", tc.name, got)
		}
		if got := sessionsCell(t, m, titleRow(t, m, "s1"), "TITLE"); strings.Contains(got, "served-name") {
			t.Errorf("%s: TITLE cell = %q, want the harvested name", tc.name, got)
		}
		if got := m.sessionLabel("s1"); strings.Contains(got, "served-name") {
			t.Errorf("%s: sessionLabel = %q, want the harvested name", tc.name, got)
		}
	}
}

// A BLANK harvested title falls back; a title that RENDERS SOMETHING does not.
//
// The distinction is titleIsBlank's, not == "" — a harvested " " paints nothing, so falling back
// fills nothing rather than overriding something. "\t" is the opposite case and the one a
// simplification gets wrong: sanitizeLabel REPLACES it with U+FFFD rather than stripping it, so the
// cell shows a visible glyph and the row is named. Falling back there would override a title that
// is on screen.
func TestSessionsPane_ServedTitleOnlyFillsWhatRendersBlank(t *testing.T) {
	for _, tc := range []struct {
		name      string
		harvested string
		want      string
	}{
		{"empty", "", "served name"},
		{"one space", " ", "served name"},
		{"several spaces", "   ", "served name"},
		{"tab renders a glyph", "\t", "�"},
		{"newline renders a glyph", "\n", "�"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newServedTitleModel(t,
				map[string]SessionMetadata{"s1": {Title: tc.harvested}},
				map[string]string{"s1": "served name"},
				"s1")
			// THROUGH THE FIXTURE, not a literal. Passing "served name" here instead read the
			// same string the served map holds, so the assertion passed whether or not
			// newServedTitleModel had wired Title onto the summary at all — leaving one test as
			// the only gate on that wiring. sessionLabel resolves the served title by id, so
			// going through it asserts the fixture and the accessor together.
			if got := m.sessionTitleFor("s1", m.servedTitle("s1")); got != tc.want {
				t.Errorf("sessionTitleFor(%q harvested) = %q, want %q", tc.harvested, got, tc.want)
			}
		})
	}
}

// Neither source names it: the cell stays empty and the header stays a bare id.
//
// The pre-existing behaviour, pinned against the fallback having quietly introduced a placeholder.
// "" is still the honest answer for a session nothing has named.
func TestSessionsPane_NoTitleAnywhereRendersAsBefore(t *testing.T) {
	m := newServedTitleModel(t,
		map[string]SessionMetadata{"s1": {Title: " "}},
		map[string]string{"s1": "  "},
		"s1")
	m.rebuildSessionsTable()

	// ASSERTED EXACTLY, not through TrimSpace: trimming here would accept a cell of spaces, which is
	// the precise defect titleIsBlank exists to prevent — a whitespace-only title reaching the screen
	// while sessionLabel calls the same row unnamed.
	if got := sessionsCell(t, m, titleRow(t, m, "s1"), "TITLE"); got != "" {
		t.Errorf("TITLE = %q, want an empty cell when neither source names the session", got)
	}
	if got := m.sessionLabel("s1"); got != "s1" {
		t.Errorf("sessionLabel = %q, want the bare id", got)
	}
}

// A served title is SANITISED, exactly as a harvested one is.
//
// /v1/sessions is unauthenticated and the title is folded from caller-supplied event content, so the
// CWE-150 reasoning behind sanitizeLabel applies to this string at least as much as to the file the
// harvester wrote. An ESC recolours the pane; a newline splits the frame.
func TestSessionsPane_ServedTitleIsSanitized(t *testing.T) {
	// THROUGH servedTitle, not with the string handed in directly. Passing the hostile title as a
	// literal tests sanitizeLabel and nothing else; resolving it from the summary the way the header
	// does means this also fails if the lookup stops finding it.
	const hostile = "before\x1b[31mafter\nnext"
	m := newServedTitleModel(t, map[string]SessionMetadata{}, map[string]string{"s1": hostile}, "s1")

	if served := m.servedTitle("s1"); served != hostile {
		t.Fatalf("servedTitle = %q, want the fixture's title — the rest of this test is vacuous "+
			"without it", served)
	}
	got := m.sessionTitleFor("s1", m.servedTitle("s1"))
	for _, bad := range []string{"\x1b", "\n"} {
		if strings.Contains(got, bad) {
			t.Errorf("sessionTitleFor = %q, still carries %q", got, bad)
		}
	}
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Errorf("sessionTitleFor = %q, want the text kept and only the control runes replaced", got)
	}
}

// EVERY CONTROL CLASS IS REPLACED, including the ones agentop used to leave to the producer.
//
// THIS TEST REPLACES A CHARACTERIZATION. sanitizeLabel covered the BIDI overrides and isolates
// (U+202A-202E, U+2066-2069) and stopped there, so the plain MARKS — U+200E LRM, U+200F RLM,
// U+061C ALM — and the zero-widths passed through untouched. The test that used to sit here recorded
// that as a deliberate reliance on core/session.sanitizeTitle stripping them upstream, and said in
// its own doc that closing the gap should DELETE it rather than invert it. This is that deletion.
//
// WHY THE RELIANCE WAS WRONG. The comment it justified claimed the served path "does not rely on the
// producer" — while the only thing keeping a mark out of the cell WAS the producer. /v1/sessions is
// unauthenticated and operator-pointed, the proxy's normaliser is unexported in another module, and
// this is the rune class whose entire function is to make the rendered order differ from the byte
// order: "report\u202Egnp.exe" reads as something else on screen. A cross-module invariant is a poor
// place to keep that, and pipeline.IsControlRune already named the full set.
//
// THE MARKS AND THE ZERO-WIDTHS ARE DIFFERENT ATTACKS, asserted together because one predicate now
// covers both. A mark REORDERS what follows it and needs no matching pop, so one is enough. A
// zero-width makes two distinct titles render identically, so a row can wear another session's name
// while nothing addresses it by that name.
//
// U+FFFD RATHER THAN DROPPED, per sanitizeLabel's standing rule: tampering must be visible instead of
// silently producing a plausible label.
func TestSessionsPane_ServedTitleStripsEveryControlClass(t *testing.T) {
	for _, tc := range []struct{ name, bad string }{
		{"LRM U+200E", "\u200e"},
		{"RLM U+200F", "\u200f"},
		{"ALM U+061C", "\u061c"},
		{"ZWSP U+200B", "\u200b"},
		{"ZWNJ U+200C", "\u200c"},
		{"ZWJ U+200D", "\u200d"},
		{"WJ U+2060", "\u2060"},
		{"BOM U+FEFF", "\ufeff"},
		{"RLO U+202E", "\u202e"},
		{"LRI U+2066", "\u2066"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// BUILT PER CASE AND RESOLVED THROUGH servedTitle, so this gates the lookup as well as
			// the sanitiser. Handing sessionTitleFor a literal leaves it passing with servedTitle
			// stubbed to "" — the header path would then be silently untested here.
			title := "report" + tc.bad + "exe"
			m := newServedTitleModel(t, map[string]SessionMetadata{}, map[string]string{"s1": title}, "s1")
			if served := m.servedTitle("s1"); served != title {
				t.Fatalf("servedTitle = %q, want the fixture's title", served)
			}

			got := m.sessionTitleFor("s1", m.servedTitle("s1"))
			if strings.Contains(got, tc.bad) {
				t.Errorf("sessionTitleFor = %q still carries %s — an unauthenticated API is not a "+
					"place to rely on another module having stripped it", got, tc.name)
			}
			if !strings.Contains(got, "\ufffd") {
				t.Errorf("sessionTitleFor = %q, want %s replaced by U+FFFD so tampering is visible, "+
					"not dropped into a plausible-looking label", got, tc.name)
			}
			if !strings.Contains(got, "report") || !strings.Contains(got, "exe") {
				t.Errorf("sessionTitleFor = %q, want the surrounding text kept", got)
			}
		})
	}
}

// AND THE HARVESTED TITLE TOO, since both sources share one sanitiser.
//
// The widening was motivated by the served path, but sessionTitle reads a file on disk that anything
// may have rewritten — LoadSessionMetadata applies no sanitisation of its own — so the same runes
// arrive by that route. Asserting both is what keeps a future narrowing of sanitizeLabel from being
// justified as "only the served path needed it".
func TestSessionsPane_HarvestedTitleStripsBidiMarks(t *testing.T) {
	m := newTitleModel(t, map[string]SessionMetadata{
		"s1": {Title: "report\u200eexe"},
	}, "s1")

	if got := m.sessionTitle("s1"); strings.Contains(got, "\u200e") {
		t.Errorf("sessionTitle = %q still carries U+200E LRM — the metadata file is not a trusted "+
			"input either", got)
	}
}

// A served title that looks like a path is truncated from the LEFT, like any other.
//
// The fallback feeds the existing cell, so it inherits looksLikePath and both truncation sides
// rather than bypassing them. Cheap to assert and it pins that the fallback did not become a
// second, unbounded rendering path.
func TestSessionsPane_ServedPathTitleTruncatesFromTheLeft(t *testing.T) {
	const long = "/Users/someone/src/cortex/.worktrees/servedtitle/authbridge"
	m := newServedTitleModel(t, map[string]SessionMetadata{}, map[string]string{"s1": long}, "s1")
	m.width = tableWidth(sessionsColumns())
	m.rebuildSessionsTable()

	got := sessionsCell(t, m, titleRow(t, m, "s1"), "TITLE")
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "authbridge") {
		t.Errorf("TITLE = %q, want the cut front and the kept tail", got)
	}
	if n := lipgloss.Width(got); n > sessionsTitleWidth {
		t.Errorf("TITLE is %d columns against a %d-column cell: %q", n, sessionsTitleWidth, got)
	}
}

// A cached-only row shows no served title, and a live row beside it still shows its own.
//
// HONEST ABOUT ITS OWN REACH: this cannot fail on the "simplification" of having the cached-only
// loop call m.servedTitle(id) itself — checked, the whole suite stays green. cachedOnlySessionIDs
// skips every id in m.sessions, so servedTitle can only ever return "" there; the behavior is
// guaranteed by that exclusion, not by this assertion. What the test does earn is the second
// check — that a cached-only row in the table does not disturb the live row's title — plus a
// worked example of the two row kinds side by side. Kept for that, not as a mutation gate.
func TestSessionsPane_CachedOnlyRowTakesNoServedTitle(t *testing.T) {
	m := newServedTitleModel(t,
		map[string]SessionMetadata{},
		map[string]string{"live": "a live session's name"},
		"live")
	m.events["gone"] = []pipeline.SessionEvent{{Host: "example.test"}}
	m.rebuildSessionsTable()

	if got := sessionsCell(t, m, titleRow(t, m, "gone"), "TITLE"); got != "" {
		t.Errorf("cached-only TITLE = %q, want \"\" — the server lists no summary for it", got)
	}
	if got := sessionsCell(t, m, titleRow(t, m, "live"), "TITLE"); got != "a live session's name" {
		t.Errorf("live TITLE = %q, want its own served title", got)
	}
}

// THE REGRESSION GUARD: a served title must NOT satisfy the harvest-backoff predicates.
//
// Every predicate in session_metadata.go judges "unnamed" through sessionTitle, and that is
// deliberate — a server-titled row has to keep reading as unnamed so the harvest keeps looking for
// the harvested title. Fold the fallback into sessionTitle and the row reads as named,
// untitledMisses zeroes, and the re-harvest stops for good.
//
// BOTH SIDES OF THE BACKOFF, because they fail differently and only one used to be asserted here.
// The SCORING side (countUntitled, harvestNamedSomething) records what a harvest achieved; the GATE
// side (untitledSettled, untitledFresh) decides whether one starts at all. Repointing either gate
// predicate at sessionTitleFor survives every other test in this package — and untitledSettled is
// the exact regression this test is named for, since a served-only row that never opens the gate
// never gets re-harvested no matter what the scoring would have said.
//
// This is the only test that fails on any of those mutations: every display test above still
// passes, because the cell is correct either way. That is exactly why it is here.
func TestSessionMetadata_ServedTitleDoesNotSatisfyTheHarvestBackoff(t *testing.T) {
	m := newServedTitleModel(t, map[string]SessionMetadata{}, map[string]string{"s1": "served name"}, "s1")

	if m.sessionHasTitle("s1") {
		t.Error("sessionHasTitle is true for a row only the SERVER named: the harvest will stop")
	}
	if n, _ := m.countUntitled(); len(n) != 1 {
		t.Errorf("countUntitled counted %d untitled rows, want 1 — a served title is not a harvested one", len(n))
	}
	// And the harvest can still report progress on it, which is what keeps the backoff from
	// capping out on a row it could in fact name.
	if !m.harvestNamedSomething(map[string]SessionMetadata{"s1": {Title: "harvested at last"}}) {
		t.Error("harvestNamedSomething is false for a served-only row the harvest just named")
	}
	// The gate, which is the half that decides whether a harvest runs. Aged past the settle delay
	// so the only thing left that can hold the gate shut is the title question.
	now := time.Now()
	m.sessions[0].UpdatedAt = now.Add(-untitledSettleDelay)
	if !m.untitledSettled(now) {
		t.Error("untitledSettled is false for a settled served-only row: no harvest will ever start")
	}
	if !m.untitledFresh() {
		t.Error("untitledFresh is false for an uncounted served-only row")
	}
	// The display, meanwhile, was never blank — which is the point of the whole change.
	if got := m.sessionTitleFor("s1", "served name"); got != "served name" {
		t.Errorf("sessionTitleFor = %q, want the served title on screen throughout", got)
	}
}

// combiningMarkRune is "e" followed by U+0301 COMBINING ACUTE ACCENT — a DECOMPOSED "e-acute",
// two runes that render as one glyph.
//
// A NAMED CONSTANT BECAUSE THE BYTES ARE THE POINT, and a literal in the test body does not survive
// its own file being edited. Both cap tests need a rune that makes zeroWidthFree false, which is
// what selects the quadratic truncation path an uncapped title would take; a precomposed U+00E9
// "é" is a single Mn-free rune and takes the FAST path, so a fixture that gets normalised — by an
// editor, a formatter, a copy through a tool that applies NFC — silently stops testing anything
// while still passing. That is the failure mode assertFixtureIsSlowPath exists to catch.
const combiningMarkRune = "e\u0301"

// assertFixtureIsSlowPath fails when a fixture no longer exercises the cost a cap prevents.
//
// SHARED BY BOTH CAP TESTS, because the hazard is identical on both sides and only one of them used
// to check: the served test guarded zeroWidthFree inline and the harvested one guarded nothing, so
// an NFC normalisation of the harvested fixture would have gone unnoticed. Asserting the rune count
// too catches the other half — a "é" that normalised to one rune also halves the fixture's length,
// which could bring it under the cap and make the test vacuous rather than merely fast.
func assertFixtureIsSlowPath(t *testing.T, s string) {
	t.Helper()
	if zeroWidthFree(s) {
		t.Fatalf("fixture takes the truncation FAST path — it no longer exercises the cost a cap "+
			"prevents. Most likely %q was normalised to a precomposed form; it must stay decomposed "+
			"(a base letter plus a combining mark)", combiningMarkRune)
	}
	if n := len([]rune(s)); n <= claude.MaxTitleLen {
		t.Fatalf("fixture is %d runes, at or under the %d-rune cap — it cannot show that a cap "+
			"applies", n, claude.MaxTitleLen)
	}
}

// THE SERVED TITLE IS CAPPED CLIENT-SIDE, and nothing upstream of agentop is what guarantees it.
//
// The pairing this closes: TestTitleCap_IsSafeOnlyBecauseTheRendererRemeasures covers the HARVESTED
// title's cap against claude.MaxTitleLen, and the served title never touches that path. The proxy has
// its own cap, but it is unexported in another module on purpose, /v1/sessions is unauthenticated, and
// agentop is pointed at whatever host an operator names — so "the producer caps it" is not an assertion
// this side can make.
//
// WHY A LENGTH AND NOT A DEADLINE. What an uncapped title costs is not a malformed cell — truncLeft
// and truncRight bound their output regardless — but the quadratic search inside them, whose fast path
// any combining mark disables and which a served title keeps its marks through. Measured on one call,
// 20003 runes took 4.50s and 200003 did not finish in two minutes, on the UI goroutine, per row per
// rebuild. A timing assertion would encode a machine's speed and flake in CI, so this pins the INPUT
// bound that makes the cost flat instead.
//
// THE FIXTURE CARRIES COMBINING MARKS, which is what makes it the real shape rather than a long
// string: an ASCII title of the same length takes the fast path and would pass a weaker cap.
func TestSessionsPane_ServedTitleIsCappedBeforeTruncation(t *testing.T) {
	// Every other rune is U+0301, so zeroWidthFree is false and the slow path is what a missing cap
	// would hand this to.
	served := strings.Repeat(combiningMarkRune, 5000)
	assertFixtureIsSlowPath(t, served)

	m := newServedTitleModel(t, map[string]SessionMetadata{}, map[string]string{"s1": served}, "s1")

	got := m.sessionTitleFor("s1", served)
	// EXACTLY THE CAP, NOT "AT MOST" IT. A `>` assertion passed for a BYTE-based cap too: this
	// fixture is two bytes per rune, so cutting at 80 BYTES leaves 40 runes, which satisfies any
	// at-most bound while silently halving the budget a multi-byte title gets. The cap is specified
	// in runes and this is what pins that. combiningMarkRune cuts on a clean boundary at index 80 —
	// every even index is the base letter — so the cluster walk does not move the cut here; the
	// dedicated test below covers the case where it does.
	if n := len([]rune(got)); n != claude.MaxTitleLen {
		t.Errorf("sessionTitleFor returned %d runes, want exactly %d — a byte-based cap satisfies "+
			"an at-most bound while halving a multi-byte title", n, claude.MaxTitleLen)
	}

	// AND THROUGH THE HEADER TOO, which reaches the same accessor by id alone. Without this, a cap
	// applied only in the cell's own call path would pass.
	// EXACTLY, not at-most, for the reason argued five lines above: an at-most bound is satisfied by a
	// byte cap that halves a multi-byte title's rune budget.
	if n, want := len([]rune(m.sessionLabel("s1"))), claude.MaxTitleLen+len(" (s1)"); n != want {
		t.Errorf("sessionLabel is %d runes, want exactly %d — the served title capped before it is "+
			"labelled", n, want)
	}
}

// THE HARVESTED TITLE IS CAPPED AT LOAD TOO, not only by the harvester that wrote it.
//
// The asymmetry this closes: the served title is capped in sessionTitleFor, and the harvested one
// was capped only upstream in core/observe/claude. LoadSessionMetadata re-reads that file and
// applies no cap, so a rewritten or hand-edited ~/.cortex/session-metadata.json bypassed the
// guarantee entirely and reached the same quadratic truncation the served cap exists to prevent.
// Measured before the cap: one rebuildSessionsTable took 1.11s on a 10003-rune title.
//
// THROUGH sessionTitle, which is where every consumer reads it — the cell, the headers, and
// sessionHasTitle. Capping at the accessor rather than at load is what makes that one line cover
// all of them.
//
// AND THE BACKOFF VERDICT MUST NOT MOVE, which is where the first version of this cap went wrong.
// sessionHasTitle reads the same accessor, so a clip that blanks a title flips a row from named to
// unnamed and restarts the ~3-minute ~/.claude re-harvest permanently — the cost this package
// documents as the price of an UNNAMABLE session, charged to one that has a name.
//
// THE WHITESPACE-PREFIX CASE IS THE WHOLE POINT, AND THIS TEST GOT IT BACKWARDS TWICE.
//
// It first asserted "truncation cannot turn a non-blank title blank" using a leading-"/" path
// fixture, where no prefix is whitespace — so it could not exercise the claim it made and passed for
// the wrong reason. The whitespace fixture was then added, and the expectation written down as
// wantNamed: FALSE for a title reading 80 spaces + "real name" — labelled "the correctness case"
// while the assertion five lines below it called an unnamed verdict the thing that "restarts the
// ~3-minute re-harvest forever". The test encoded the defect and then explained why the defect was
// bad.
//
// It is wantNamed: true now, because "real name" is a name and nothing about padding changes that.
// capTitleRunes trims BEFORE measuring, so leading whitespace no longer spends the rune budget and
// there is no window for it to fill. See sessionTitle's doc for the measured curve this replaces.
func TestSessionsPane_HarvestedTitleIsCappedAtLoad(t *testing.T) {
	for _, tc := range []struct {
		name      string
		title     string
		wantNamed bool
		// slowPath marks the over-long fixture whose combining marks are what select the quadratic
		// truncation path. Only that case can degrade silently under NFC normalisation, so only it
		// is guarded — the whitespace cases are short by design and would fail the guard's own
		// length check.
		slowPath bool
	}{
		{
			// The performance case: over-long, with a combining mark so the truncation fast path
			// is off.
			name:      "over-long path with combining marks",
			title:     "/a/" + strings.Repeat(combiningMarkRune, 5000),
			wantNamed: true,
			slowPath:  true,
		},
		{
			// The correctness case: real text begins after where a naive cut would land, so a cap
			// that measured before trimming kept only spaces and then trimmed them to "".
			name:      "whitespace fills the whole clip window",
			title:     strings.Repeat(" ", claude.MaxTitleLen) + "real name",
			wantNamed: true,
		},
		{
			// Far past it, so no "off by a few runes" fix can pass this by accident.
			name:      "whitespace far past the clip window",
			title:     strings.Repeat(" ", 500) + "real name",
			wantNamed: true,
		},
		{
			// And the one that must STILL read as named: whitespace prefix, text inside the window.
			name:      "whitespace prefix but text within the window",
			title:     strings.Repeat(" ", claude.MaxTitleLen-4) + "real name",
			wantNamed: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.slowPath {
				assertFixtureIsSlowPath(t, tc.title)
			}
			m := newTitleModel(t, map[string]SessionMetadata{"s1": {Title: tc.title}}, "s1")

			got := m.sessionTitle("s1")
			if n := len([]rune(got)); n > claude.MaxTitleLen {
				t.Errorf("sessionTitle returned %d runes, want at most %d — an uncapped harvested "+
					"title reaches the quadratic truncation path on the UI goroutine",
					n, claude.MaxTitleLen)
			}
			// NO LEADING OR TRAILING WHITESPACE SURVIVES THE CAP, which is what makes the verdict
			// below deliberate rather than incidental. The cap trims on both sides of the cut —
			// before, so padding does not spend the budget, and after, to drop whitespace the cut
			// newly exposed.
			if got != strings.TrimSpace(got) {
				t.Errorf("sessionTitle = %q, want it trimmed — untrimmed whitespace "+
					"is what flips a named row to unnamed", got)
			}
			if named := m.sessionHasTitle("s1"); named != tc.wantNamed {
				t.Errorf("sessionHasTitle = %v, want %v — a wrong verdict here either restarts "+
					"the ~3-minute re-harvest forever or stops it on a session with no name",
					named, tc.wantNamed)
			}
		})
	}
}

// THE CAP CUTS ON A GRAPHEME BOUNDARY, never inside a cluster.
//
// WHY THIS IS NOT PEDANTRY. clipTitle upstream (core/observe/claude) does a plain rune cut and says
// in its own doc that this is safe ONLY BECAUSE normalizeTitle ran first and removed every character
// that binds to its neighbour. Neither string capTitleRunes sees has been through that: the harvested
// title is re-read from a file that may have been rewritten, and the served title comes from
// core/session's sanitizeTitle, whose doc says it KEEPS combining marks "so café survives". The first
// version of this cap mirrored clipTitle's TrimSpace while silently dropping its precondition.
//
// WHAT A BLIND CUT PRODUCES is a title nobody wrote: an accent rebound to whatever letter happens to
// land last, half of a ZWJ emoji sequence, or one regional indicator of a two-letter flag — which
// renders as a bare letter. On a column an operator reads to pick a row before acting on it.
//
// EACH CASE PUTS THE BINDER EXACTLY AT THE CUT, which is the only index where the walk-back matters;
// a fixture with clusters merely present would pass with no walk at all.
func TestSessionsPane_CapCutsOnAClusterBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		// binder is placed AT index claude.MaxTitleLen, so a cut that does not walk back would
		// orphan it from the base rune at MaxTitleLen-1.
		binder string
	}{
		{"combining acute U+0301 (Mn)", "\u0301"},
		{"variation selector U+FE0F (Mn)", "\ufe0f"},
		{"enclosing circle U+20DD (Me)", "\u20dd"},
		{"Devanagari vowel sign U+093E (Mc)", "\u093e"},
		{"zero-width joiner U+200D", "\u200d"},
		// NOT A LONE REGIONAL INDICATOR. One RI preceded by ordinary text STARTS a flag rather than
		// completing one, so the cut before it is already on a boundary and must NOT walk back —
		// asserting otherwise is what pinned the walk-to-zero defect in place. RI pairing gets its
		// own test below, where the run length is what decides.
		//
		// NOT U+02B0 AND FRIENDS. Lm modifier LETTERS look like they belong in this list and do
		// not: Unicode's Grapheme_Extend property excludes them, and Lm also contains runes that
		// legitimately START a cluster (U+02BB ʻokina is a letter in Hawaiian orthography). Treating
		// Lm as a binder would walk the cut back off an ordinary word character. The categories that
		// extend a cluster are Mn/Me/Mc, and the two sequence-builders below are the special cases
		// that carry no category marking them.
	} {
		t.Run(tc.name, func(t *testing.T) {
			// "a" * (MaxTitleLen-1) + a base rune + the binder + filler past the cap.
			title := strings.Repeat("a", claude.MaxTitleLen-1) + "e" + tc.binder +
				strings.Repeat("z", 50)
			r := []rune(title)
			if len(r) <= claude.MaxTitleLen {
				t.Fatalf("fixture is %d runes, must exceed the %d-rune cap", len(r), claude.MaxTitleLen)
			}
			if r[claude.MaxTitleLen] != []rune(tc.binder)[0] {
				t.Fatalf("fixture misaligned: rune at the cut is %U, want the binder %U",
					r[claude.MaxTitleLen], []rune(tc.binder)[0])
			}

			got := capTitleRunes(title)

			// THE BINDER MUST NOT LEAD THE RESULT'S TAIL. Concretely: the cut must not have landed
			// between the base rune and its binder, which is what leaves the binder as the first
			// rune of nothing.
			gr := []rune(got)
			if len(gr) == 0 {
				t.Fatalf("capTitleRunes returned empty for %q", title)
			}
			if last := gr[len(gr)-1]; last == []rune(tc.binder)[0] {
				t.Errorf("capTitleRunes = ...%U, want the cut walked BACK off the binder so the "+
					"cluster is dropped whole rather than severed", last)
			}
			// AND THE BASE RUNE GOES WITH IT. Keeping "e" while dropping its accent is the same
			// defect from the other side: the glyph changes into one the title never contained.
			if strings.HasSuffix(got, "e") {
				t.Errorf("capTitleRunes = %q, want the base rune dropped alongside its binder — "+
					"keeping it silently changes the final glyph", got)
			}
			if n := len(gr); n > claude.MaxTitleLen {
				t.Errorf("capTitleRunes returned %d runes, want at most %d", n, claude.MaxTitleLen)
			}
		})
	}
}

// REGIONAL INDICATORS PAIR, so the walk must count the run rather than treat each one as a binder.
//
// THE DEFECT THIS PINS: bindsToPrevious answers true for every RI, and the walk used to act on that
// answer directly, so a run of them had no even-parity rune to stop at and the cut slid to index 0.
// 41 consecutive flags capped to "" and "a"*70 + 10 flags lost 11 runes, against a doc promising the
// over-walk costs "a rune or two".
//
// WHY AN EMPTIED TITLE IS NOT COSMETIC, and why this test sits with the backoff tests in spirit:
// sessionHasTitle reads sessionTitle, which caps. A harvested flag title that caps to "" reads as
// UNNAMED, zeroing untitledMisses and restarting the permanent ~3-minute re-harvest for a row the
// harvest had already named. f5a0a615 fixed that exact failure for whitespace-only titles; this is
// the same failure reached through the cap instead of through the trim.
//
// THE PARITY IS WHAT IS ASSERTED, not a single index: an even-length run before the cut means the
// rune at the cut starts a fresh flag and the cut is already on a boundary, while an odd-length run
// means it completes one and the cut must step back exactly once.
func TestSessionsPane_CapWalksBackOverAtMostOneRegionalIndicator(t *testing.T) {
	const flag = "\U0001F1FA\U0001F1F8" // two RIs

	t.Run("a title of nothing but flags keeps its cap", func(t *testing.T) {
		// 41 flags = 82 RIs, every one of them a binder by category. A walk that does not pair
		// returns "" here.
		title := strings.Repeat(flag, 41)
		got := capTitleRunes(title)
		n := utf8.RuneCountInString(got)
		if n == 0 {
			t.Fatalf("capTitleRunes emptied a %d-rune flag title — the walk ran to index 0 instead "+
				"of stopping at a pair boundary, which flips the row to unnamed and restarts the "+
				"re-harvest", utf8.RuneCountInString(title))
		}
		// The cap is even and flags are two runes wide, so the whole budget is usable here.
		if n != claude.MaxTitleLen {
			t.Errorf("capTitleRunes returned %d runes, want exactly %d", n, claude.MaxTitleLen)
		}
		// AND NO HALF FLAG AT THE TAIL: an odd count would mean a severed pair.
		if n%2 != 0 {
			t.Errorf("capTitleRunes returned an odd %d runes, so the tail is half a flag", n)
		}
	})

	t.Run("text then flags loses at most one rune to the walk", func(t *testing.T) {
		// The cut at index 80 lands on the FIRST RI of the sixth flag: the run before it is 10 runes
		// (five whole flags), which is even, so it starts a pair and the cut needs no walk at all.
		title := strings.Repeat("a", 70) + strings.Repeat(flag, 10)
		got := capTitleRunes(title)
		if n := utf8.RuneCountInString(got); n != claude.MaxTitleLen {
			t.Errorf("capTitleRunes returned %d runes, want %d — the walk crossed a pair boundary "+
				"it had no reason to cross", n, claude.MaxTitleLen)
		}
	})

	t.Run("an odd run steps back exactly once", func(t *testing.T) {
		// 71 letters shifts the parity: the cut now lands on the SECOND RI of a flag, so the walk
		// must step back one rune and no further.
		title := strings.Repeat("a", 71) + strings.Repeat(flag, 10)
		got := capTitleRunes(title)
		n := utf8.RuneCountInString(got)
		if n != claude.MaxTitleLen-1 {
			t.Errorf("capTitleRunes returned %d runes, want %d — exactly one step back off the "+
				"second half of a flag", n, claude.MaxTitleLen-1)
		}
		// DELIBERATELY NO "the tail is not an RI" ASSERTION. It is the tempting one and it is wrong:
		// after stepping back, the tail here IS a regional indicator — the FIRST of a pair, which is
		// a legal boundary. A title may end where a flag was about to begin. Only an odd-parity RI
		// at the cut is a severed pair, and the length assertion above is what pins that.
		if n%2 == 0 {
			t.Errorf("capTitleRunes returned %d runes; this fixture's cut has odd parity, so an even "+
				"result means the walk moved further than the one step the pairing calls for", n)
		}
	})
}

// A HARVESTED FLAG TITLE STILL READS AS NAMED, which is the consequence the cap must not break.
//
// The display tests above assert what the cell shows; this asserts what the BACKOFF concludes, and
// they are different questions with different failure modes. sessionHasTitle goes through
// sessionTitle, which caps — so any cap bug that empties a title silently converts "named" into
// "unnamed", zeroes untitledMisses and restarts a ~3-minute re-harvest that can never succeed. The
// row keeps rendering whatever the fallback finds, so nothing on screen says anything is wrong.
//
// THIS IS THE SECOND ROUTE TO ONE FAILURE. f5a0a615 closed the first: a whitespace-prefixed title
// that TrimSpace emptied. The cap is the other, and a test that only checks the rendered cell cannot
// tell them apart.
func TestSessionsPane_FlagOnlyHarvestedTitleStaysNamed(t *testing.T) {
	flags := strings.Repeat("\U0001F1FA\U0001F1F8", 41)
	m := newTitleModel(t, map[string]SessionMetadata{"s1": {Title: flags}}, "s1")

	if got := m.sessionTitle("s1"); got == "" {
		t.Fatalf("sessionTitle emptied a flag-only harvested title")
	}
	if !m.sessionHasTitle("s1") {
		t.Errorf("sessionHasTitle = false for a session the harvest DID name, so the row will "+
			"re-harvest forever; sessionTitle = %q", m.sessionTitle("s1"))
	}
	if counted, _ := m.countUntitled(); len(counted) != 0 {
		t.Errorf("countUntitled counted %d rows, want 0 — a named row must not be a miss", len(counted))
	}
}

// THE Lm/Sk EXCLUSION IS A GATE, not just a paragraph in bindsToPrevious's doc.
//
// Adding unicode.Lm and unicode.Sk to that predicate is the single most plausible "improvement" a
// future reader can make to it — they are modifier categories, they look like the mark categories,
// and until this test existed the whole suite stayed green while every title ending in one of them
// silently lost a character. U+02BB ʻokina is a LETTER in Hawaiian orthography, so walking back off
// it truncates an ordinary word.
//
// ASSERTED AT THE PREDICATE, deliberately, rather than only through capTitleRunes: a cap-level test
// would need a fixture placing the rune exactly at index MaxTitleLen, and the point is the category
// judgement itself.
func TestSessionsPane_ModifierLettersDoNotBind(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    rune
	}{
		{"U+02BB okina (Lm) — starts a cluster in Hawaiian", '\u02bb'},
		{"U+02B0 modifier small h (Lm)", '\u02b0'},
		{"U+02C7 caron (Sk) — a standalone symbol", '\u02c7'},
		{"U+02D0 triangular colon (Lm)", '\u02d0'},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if bindsToPrevious(tc.r) {
				t.Errorf("bindsToPrevious(%U) = true, want false: Grapheme_Extend excludes modifier "+
					"letters and symbols, and Lm holds runes that legitimately START a cluster",
					tc.r)
			}
		})
	}

	// AND AT THE CAP, for the one that is a real word character: a title ending in ʻokina keeps it.
	title := strings.Repeat("a", claude.MaxTitleLen-1) + "\u02bb" + strings.Repeat("z", 20)
	got := capTitleRunes(title)
	if n := utf8.RuneCountInString(got); n != claude.MaxTitleLen {
		t.Errorf("capTitleRunes returned %d runes, want %d — the walk crossed a modifier letter",
			n, claude.MaxTitleLen)
	}
	if !strings.HasSuffix(got, "\u02bb") {
		t.Errorf("capTitleRunes = %q, want the trailing okina kept", got)
	}
}

// SANITISING HAPPENS BEFORE CAPPING, and the order is observable rather than a matter of taste.
//
// sessionTitleFor's doc argues the ordering; this makes it fail if reversed. The two orders agree on
// WHERE the cut falls — sanitizeLabel is rune-for-rune — but not on WHAT sits at it. A ZWJ exactly at
// the cut is the discriminating case: sanitising first replaces it with U+FFFD, a standalone glyph
// that binds to nothing, so the cut stands and 80 runes survive. Capping first sees the live ZWJ,
// walks back off it, and returns 79 — one rune shorter for a title whose rendered form contains no
// joiner at all, because the joiner was going to be replaced regardless.
//
// ASSERTED THROUGH sessionTitleFor, the real caller, so this pins the composition and not just two
// helpers in isolation.
func TestSessionsPane_ServedTitleIsSanitizedBeforeCapping(t *testing.T) {
	// A base rune then a ZWJ exactly at index MaxTitleLen, then filler past the cap.
	title := strings.Repeat("a", claude.MaxTitleLen-1) + "e\u200d" + strings.Repeat("z", 20)
	if r := []rune(title); r[claude.MaxTitleLen] != '\u200d' {
		t.Fatalf("fixture misaligned: rune at the cut is %U, want U+200D", r[claude.MaxTitleLen])
	}

	m := newServedTitleModel(t, map[string]SessionMetadata{}, map[string]string{"s1": title}, "s1")
	got := m.sessionTitleFor("s1", m.servedTitle("s1"))

	if n := utf8.RuneCountInString(got); n != claude.MaxTitleLen {
		t.Errorf("sessionTitleFor returned %d runes, want %d — capping before sanitising walks back "+
			"off a joiner that sanitizeLabel was about to replace with a standalone glyph", n,
			claude.MaxTitleLen)
	}
	// THE TAIL IS THE BASE RUNE, NOT THE JOINER, and that is the point rather than an oversight: the
	// cut is exclusive, so r[:MaxTitleLen] never contains the rune AT the cut. What the ordering
	// decides is whether the walk moves that boundary, and the rune count above is the only thing
	// that can see it. Asserting a U+FFFD tail here would be asserting the fixture, not the order.
	if !strings.HasSuffix(got, "e") {
		t.Errorf("sessionTitleFor = %q, want the base rune at the boundary kept", got)
	}
}

// A title of nothing but combining marks KEEPS ITS CAP-LENGTH PREFIX rather than capping to "".
//
// THIS TEST ASSERTED THE OPPOSITE AND WAS WRONG, in the same way the lone-regional-indicator test
// was wrong an earlier round: it characterised what the walk did instead of what the cap owes its
// callers. Its old reasoning was that every rune binds, so the walk runs to index 0 and "there is no
// cluster to keep" — and that "\"\" is what sessionHasTitle already handles". That last clause is
// the defect, stated as the justification. sessionHasTitle does not "handle" "": it reads it as
// UNNAMED, which zeroes untitledMisses and restarts the permanent ~3-minute re-harvest for a session
// that had a perfectly good name. Returning "" is the expensive answer, not the honest one.
//
// The degenerate end of the walk is still worth pinning, so this keeps the fixture and inverts the
// expectation. With no base character anywhere there IS no boundary to cut on, so the cap takes its
// blunt prefix and accepts a split cluster. That trade is deliberate and one-directional: a dangling
// mark renders as an odd glyph on one row, while "" silently costs a transcript scan every three
// minutes for as long as the pane is open.
func TestSessionsPane_CapOfOnlyCombiningMarksKeepsAPrefix(t *testing.T) {
	title := strings.Repeat("\u0301", claude.MaxTitleLen+20)

	got := capTitleRunes(title)
	if titleIsBlank(got) {
		t.Errorf("capTitleRunes = %q, which is blank — a named session would read as unnamed and re-harvest forever", got)
	}
	if n := utf8.RuneCountInString(got); n != claude.MaxTitleLen {
		t.Errorf("capTitleRunes returned %d runes, want the blunt %d-rune prefix", n, claude.MaxTitleLen)
	}
}

// A WHITESPACE-ONLY SERVED TITLE MUST NOT PAINT SPACES INTO THE CELL.
//
// titleIsBlank guarded the harvested title and not the served one, so sessionTitleFor returned
// "   " verbatim: the cell rendered blanks while sessionLabel — which blank-checks what
// sessionTitleFor returns — rendered the bare id. One accessor, two callers, two different names
// for the same session. Asserted on both ends so they cannot drift apart again.
//
// SPACES ONLY, deliberately. A tab or a newline is NOT blank by this package's definition:
// titleIsBlank sanitises before it trims, so "\t" becomes a visible U+FFFD glyph and is a real —
// if ugly — title. Writing this test with "\t" in the fixture failed, and the test was wrong
// rather than the code; TestSessionsPane_ServedTitleOnlyFillsWhatRendersBlank already pins that
// sanitise-before-trim order from the harvested side, and the two must not contradict each other.
func TestSessionsPane_BlankServedTitleRendersAsUnnamed(t *testing.T) {
	for _, served := range []string{" ", "   ", " ", " 　 "} {
		t.Run(strconv.Quote(served), func(t *testing.T) {
			m := newServedTitleModel(t, map[string]SessionMetadata{},
				map[string]string{"s1": served}, "s1")

			if got := m.sessionTitleFor("s1", m.servedTitle("s1")); got != "" {
				t.Errorf("sessionTitleFor = %q, want an empty string — a blank served title must "+
					"not paint whitespace into the cell", got)
			}
			if got := m.sessionLabel("s1"); got != "s1" {
				t.Errorf("sessionLabel = %q, want the bare id", got)
			}
		})
	}
}

// TestSessionsPane_ZeroWidthFreeAndBindsToPreviousDisagreeOnPurpose pins the rune classes on which
// the file's two Unicode-category predicates deliberately differ.
//
// They look like near-duplicates and are not. zeroWidthFree asks "could a rune count of this string
// be wrong about its DISPLAY WIDTH?" and so covers Cf and Cc (invisible) and Sk (modifier symbols,
// which combine in emoji sequences) alongside Mn/Me. bindsToPrevious asks "would cutting BEFORE this
// rune orphan it from its cluster?" and so covers Mc — a spacing combining mark, which has width and
// therefore does not concern zeroWidthFree at all — while excluding Cf and Cc because sanitizeLabel
// has already replaced those with U+FFFD before either predicate sees a served title.
//
// WITHOUT THIS TEST the divergence is unpinned, and the tempting refactor — one shared category set,
// or one predicate calling the other — passes the rest of the suite while being wrong in both
// directions: it would make a Mc-terminated title take zeroWidthFree's slow path for no reason, and
// make bindsToPrevious walk back off an Sk that starts nothing.
func TestSessionsPane_ZeroWidthFreeAndBindsToPreviousDisagreeOnPurpose(t *testing.T) {
	cases := []struct {
		name          string
		r             rune
		zeroWidthFree bool // false == "this rune defeats the fast path"
		binds         bool
	}{
		// Mc: spacing combining mark. Binds (cutting before it orphans it), but it HAS a column, so
		// a rune count is not wrong about it and the width fast path may keep running.
		{"Mc DEVANAGARI SIGN VISARGA U+0903", 'ः', true, true},
		// Sk: modifier symbol. The emoji skin-tone modifiers are here, and they DO combine — so a
		// rune count misjudges the width and zeroWidthFree must claim them. bindsToPrevious does
		// not, because Grapheme_Extend excludes Sk: U+1F3FB is Emoji_Modifier, which the grapheme
		// rules handle as part of an emoji sequence rather than as an extender. This is the
		// sharpest of the four rows — the only one where a reader might think bindsToPrevious is
		// the one with the bug. It is not: over-claiming here costs a rune off a title for
		// nothing, and the cut is exclusive, so a cut BEFORE U+1F3FB leaves the base emoji whole.
		{"Sk EMOJI MODIFIER FITZPATRICK U+1F3FB", 0x1F3FB, false, false},
		// Cf: format. Invisible, so the width count is wrong — and sanitizeLabel has already turned
		// it into U+FFFD by the time the cap runs, which is why bindsToPrevious need not claim it.
		{"Cf ZWSP-adjacent U+2060 WORD JOINER", '⁠', false, false},
		// Mn: the one class both claim, for their two different reasons.
		{"Mn COMBINING ACUTE U+0301", '́', false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := zeroWidthFree(string(tc.r)); got != tc.zeroWidthFree {
				t.Errorf("zeroWidthFree(%U) = %v, want %v", tc.r, got, tc.zeroWidthFree)
			}
			if got := bindsToPrevious(tc.r); got != tc.binds {
				t.Errorf("bindsToPrevious(%U) = %v, want %v", tc.r, got, tc.binds)
			}
		})
	}
}
