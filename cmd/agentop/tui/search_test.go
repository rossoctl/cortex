package tui

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// forceColour makes lipgloss emit escapes without a terminal, so a test can see how a row is
// drawn — see table_ansi_test.go for why the suite is otherwise blind to styling. Called before
// the model is built: the table renders its rows when they are set, not when View is called.
func forceColour(t *testing.T) {
	t.Helper()
	restore := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })
}

// rowLook is how tbl draws the screen line that shows tok: "selected" (the cursor), "marked"
// (a search match — its characters highlighted, or the whole row marked when nothing on
// screen shows the match), "plain", or "absent" when no line on screen shows it.
func rowLook(tbl table.Model, tok string) string {
	esc := func(s lipgloss.Style) string { return strings.SplitN(s.Render("x"), "x", 2)[0] }
	st := tableStyles()
	sel := esc(st.Selected)
	for _, l := range strings.Split(tbl.View(), "\n") {
		if !strings.Contains(stripANSI(l), tok) {
			continue
		}
		switch {
		case strings.Contains(l, sel):
			return "selected"
		case strings.Contains(l, esc(st.Match)), strings.Contains(l, esc(st.Marked)):
			return "marked"
		}
		return "plain"
	}
	return "absent"
}

func checkLooks(t *testing.T, what string, tbl table.Model, want map[string]string) {
	t.Helper()
	for tok, w := range want {
		if got := rowLook(tbl, tok); got != w {
			t.Errorf("%s: row %s is %s, want %s", what, tok, got, w)
		}
	}
}

func typeKeys(m *model, s string) {
	for _, r := range s {
		m.Update(keyRune(r))
	}
}

func press(m *model, k tea.KeyType) { m.Update(tea.KeyMsg{Type: k}) }

// searchIDs are four sessions whose ids are their own screen tokens: "x" matches rows 0 and 3,
// "y" rows 1 and 2, "x-a" row 0 only.
var searchIDs = []string{"x-alpha", "y-beta", "y-gamma", "x-delta"}

// newSearchSessionsModel is a connected model on the sessions pane, built by New so the search
// input is the one production builds.
func newSearchSessionsModel(t *testing.T, ids ...string) *model {
	t.Helper()
	resetSettingsForTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := New(ctx, apiclient.New("http://127.0.0.1:1")).(*model)
	m.width, m.height = 200, 40
	m.layout()
	now := time.Now()
	for _, id := range ids {
		m.sessions = append(m.sessions, session.SessionSummary{ID: id, UpdatedAt: now})
	}
	m.rebuildSessionsTable()
	return m
}

// hostEvents is one outbound request per host, a second apart, oldest first. The host is the
// row's screen token.
func hostEvents(hosts ...string) []pipeline.SessionEvent {
	base := time.Now().Add(-time.Hour)
	out := make([]pipeline.SessionEvent, len(hosts))
	for i, h := range hosts {
		out[i] = pipeline.SessionEvent{
			At: base.Add(time.Duration(i) * time.Second), Direction: pipeline.Outbound,
			Phase: pipeline.SessionRequest, Host: h,
		}
	}
	return out
}

// newSearchEventsModel is the events pane on session "s", one row per host. It opens on the
// newest row, which is where a session opens by default.
func newSearchEventsModel(t *testing.T, hosts ...string) *model {
	t.Helper()
	resetSettingsForTest(t)
	m := &model{
		pane: paneEvents, selectedSess: "s", width: 200, height: 40,
		events:       map[string][]pipeline.SessionEvent{"s": hostEvents(hosts...)},
		eventColumns: defaultColumnSelection(),
		searchInput:  newSearchInput(),
		sessionsTbl:  newSessionsTable(),
		eventsTbl:    newEventsTable(),
	}
	m.layout()
	m.rebuildEventsTable()
	return m
}

// TestSearch_MarksTheMatchesAndHidesNothing is the issue (#1339): `/` used to drop every row
// that did not match. Now every row stays, and the matches are drawn as matches.
func TestSearch_MarksTheMatchesAndHidesNothing(t *testing.T) {
	forceColour(t)
	m := newSearchSessionsModel(t, searchIDs...)
	typeKeys(m, "/x")
	press(m, tea.KeyEnter)

	if n := len(m.sessionsTbl.Rows()); n != len(searchIDs) {
		t.Fatalf("search left %d rows, want all %d", n, len(searchIDs))
	}
	checkLooks(t, "after /x", m.sessionsTbl, map[string]string{
		"x-alpha": "selected", "y-beta": "plain", "y-gamma": "plain", "x-delta": "marked",
	})
}

// TestSearch_TypingJumpsFromWhereSlashWasPressed: each keystroke searches again from the row
// the cursor was on at `/`, not from wherever the last keystroke left it, and wraps past the
// last row. An empty box puts the cursor back where it started.
func TestSearch_TypingJumpsFromWhereSlashWasPressed(t *testing.T) {
	forceColour(t)
	m := newSearchSessionsModel(t, searchIDs...)
	setCursorVisible(&m.sessionsTbl, 1) // y-beta

	steps := []struct {
		keys   func()
		cursor string
	}{
		// matches 0 and 3; the first at or after row 1 is 3.
		{func() { typeKeys(m, "/x") }, "x-delta"},
		// only row 0 matches, so the search wraps to it.
		{func() { typeKeys(m, "-a") }, "x-alpha"},
		// back to "x", counted from row 1 again — from the cursor's row 0 it would stay put.
		{func() { press(m, tea.KeyBackspace); press(m, tea.KeyBackspace) }, "x-delta"},
		// an empty box returns to where `/` was pressed.
		{func() { press(m, tea.KeyBackspace) }, "y-beta"},
	}
	for i, s := range steps {
		s.keys()
		if got := rowLook(m.sessionsTbl, s.cursor); got != "selected" {
			t.Fatalf("step %d: %s is %s, want the cursor on it; screen:\n%s",
				i, s.cursor, got, stripANSI(m.sessionsTbl.View()))
		}
	}
	if !m.searching {
		t.Error("typing closed the prompt")
	}
}

// TestSearch_EscPutsBackTheCursorAndTheSearch: Esc cancels — the cursor returns to the row
// `/` was pressed on, and the marks are the committed search's again, not the typed one's.
func TestSearch_EscPutsBackTheCursorAndTheSearch(t *testing.T) {
	forceColour(t)
	m := newSearchSessionsModel(t, searchIDs...)
	typeKeys(m, "/x")
	press(m, tea.KeyEnter)
	m.Update(keyRune('n')) // x-delta

	typeKeys(m, "/y")
	checkLooks(t, "typing /y", m.sessionsTbl, map[string]string{
		"y-beta": "selected", "y-gamma": "marked", "x-alpha": "plain",
	})
	press(m, tea.KeyEsc)
	if m.searching {
		t.Fatal("Esc left the prompt open")
	}
	checkLooks(t, "after Esc", m.sessionsTbl, map[string]string{
		"x-delta": "selected", "x-alpha": "marked", "y-beta": "plain", "y-gamma": "plain",
	})
}

// TestSearch_EnterKeepsTheCursorOnTheMatch: Enter closes the prompt and leaves the cursor on
// the match it reached, which is the point of searching.
func TestSearch_EnterKeepsTheCursorOnTheMatch(t *testing.T) {
	forceColour(t)
	m := newSearchSessionsModel(t, searchIDs...)
	setCursorVisible(&m.sessionsTbl, 1)
	typeKeys(m, "/x")
	press(m, tea.KeyEnter)
	if m.searching {
		t.Fatal("Enter left the prompt open")
	}
	checkLooks(t, "after Enter", m.sessionsTbl, map[string]string{
		"x-delta": "selected", "x-alpha": "marked", "y-beta": "plain",
	})
}

// TestSearch_NextAndPreviousWrap: n and N step through the matches from the cursor, wrapping
// at both ends.
func TestSearch_NextAndPreviousWrap(t *testing.T) {
	forceColour(t)
	m := newSearchSessionsModel(t, searchIDs...)
	typeKeys(m, "/x")
	press(m, tea.KeyEnter) // x-alpha

	for i, s := range []struct {
		key    rune
		cursor string
	}{
		{'n', "x-delta"}, {'n', "x-alpha"}, {'N', "x-delta"}, {'N', "x-alpha"},
	} {
		m.Update(keyRune(s.key))
		if got := rowLook(m.sessionsTbl, s.cursor); got != "selected" {
			t.Fatalf("step %d (%c): %s is %s, want the cursor on it", i, s.key, s.cursor, got)
		}
	}
}

// TestSearch_NextWithNoSearchDoesNothing: without a committed search, n and N leave the cursor
// alone.
func TestSearch_NextWithNoSearchDoesNothing(t *testing.T) {
	m := newSearchSessionsModel(t, searchIDs...)
	setCursorVisible(&m.sessionsTbl, 2)
	m.Update(keyRune('n'))
	m.Update(keyRune('N'))
	if c := m.sessionsTbl.Cursor(); c != 2 {
		t.Errorf("n/N with no search moved the cursor to row %d, want 2", c)
	}
}

// TestSearch_SurvivesAPollThatReorders: the sessions list re-sorts under the cursor. The cursor
// follows the session it was on, and the marks follow the matching sessions to their new rows
// rather than staying on the old row numbers.
func TestSearch_SurvivesAPollThatReorders(t *testing.T) {
	forceColour(t)
	m := newSearchSessionsModel(t, searchIDs...)
	typeKeys(m, "/x")
	press(m, tea.KeyEnter)
	m.Update(keyRune('n')) // x-delta, row 3

	// x-delta moves to the top: rows are now x-delta, y-gamma, x-alpha, y-beta.
	s := m.sessions
	m.sessions = []session.SessionSummary{s[3], s[2], s[0], s[1]}
	m.rebuildSessionsTable()
	checkLooks(t, "after the reorder", m.sessionsTbl, map[string]string{
		"x-delta": "selected", "x-alpha": "marked", "y-gamma": "plain", "y-beta": "plain",
	})
}

// TestSearch_IsPerPane: the sessions search does not follow the operator into a session's
// events, and the events search does not come back out with them.
func TestSearch_IsPerPane(t *testing.T) {
	m := newSearchSessionsModel(t, searchIDs...)
	m.events["x-alpha"] = hostEvents("r0.test", "r1-zeta.test")
	typeKeys(m, "/x")
	press(m, tea.KeyEnter)
	press(m, tea.KeyEnter) // drill into x-alpha
	if m.pane != paneEvents {
		t.Fatalf("Enter on a session left pane %v, want events", m.pane)
	}
	if st := stripANSI(statusRow(m.footerView())); strings.Contains(st, "[/x") {
		t.Errorf("the sessions search followed into the events pane: %q", st)
	}
	typeKeys(m, "/zeta")
	press(m, tea.KeyEnter)
	if st := stripANSI(statusRow(m.footerView())); !strings.Contains(st, "[/zeta 1/1]") {
		t.Errorf("events status row = %q, want [/zeta 1/1]", st)
	}

	press(m, tea.KeyEsc) // back to sessions
	if m.pane != paneSessions {
		t.Fatalf("Esc on events left pane %v, want sessions", m.pane)
	}
	if st := stripANSI(statusRow(m.footerView())); !strings.Contains(st, "[/x 1/2]") || strings.Contains(st, "zeta") {
		t.Errorf("sessions status row = %q, want its own [/x 1/2] and nothing of the events search", st)
	}
}

// streamHost appends one event for host to session "s", newer than every event already there,
// and rebuilds — what a streamed event does to the events pane.
func streamHost(m *model, host string) {
	evs := m.events["s"]
	e := hostEvents(host)[0]
	e.At = evs[len(evs)-1].At.Add(time.Second)
	m.events["s"] = append(evs, e)
	m.rebuildEventsTable()
}

// TestSearch_CancellingKeepsFollowingTheTail: a live events pane opened on the newest row
// follows new events. Opening the prompt, events streaming in while it is open, and closing it
// again without a match must leave it following — the cursor on the newest row, not on the
// row that was newest when `/` was pressed.
func TestSearch_CancellingKeepsFollowingTheTail(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		close tea.KeyType
	}{
		{"slash then Esc", "", tea.KeyEsc},
		{"no match then Esc", "nomatch", tea.KeyEsc},
		{"no match then Enter", "nomatch", tea.KeyEnter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forceColour(t)
			m := newSearchEventsModel(t, "r0.test", "r1.test", "r2.test")
			typeKeys(m, "/")
			streamHost(m, "r3.test")
			streamHost(m, "r4.test")
			typeKeys(m, tc.query)
			press(m, tc.close)
			if got := rowLook(m.eventsTbl, "r4.test"); got != "selected" {
				t.Fatalf("after closing the prompt the newest row is %s, want the cursor on it; screen:\n%s",
					got, stripANSI(m.eventsTbl.View()))
			}
			streamHost(m, "r5.test")
			if got := rowLook(m.eventsTbl, "r5.test"); got != "selected" {
				t.Errorf("the next event is %s, want the cursor to follow it; screen:\n%s",
					got, stripANSI(m.eventsTbl.View()))
			}
		})
	}
}

// TestSearch_EventsSearchStaysWithItsSession: the events search is for the session it was typed
// in. Backing out and re-entering that session keeps it; opening a different session's events
// starts with none.
func TestSearch_EventsSearchStaysWithItsSession(t *testing.T) {
	m := newSearchSessionsModel(t, searchIDs...)
	m.events["x-alpha"] = hostEvents("r0.test", "r1-zeta.test")
	m.events["y-beta"] = hostEvents("q0-zeta.test")
	status := func() string { return stripANSI(statusRow(m.footerView())) }

	press(m, tea.KeyEnter) // x-alpha
	typeKeys(m, "/zeta")
	press(m, tea.KeyEnter)
	if st := status(); !strings.Contains(st, "[/zeta 1/1]") {
		t.Fatalf("setup: status row = %q, want [/zeta 1/1]", st)
	}

	press(m, tea.KeyEsc)
	press(m, tea.KeyEnter) // x-alpha again
	if m.selectedSess != "x-alpha" {
		t.Fatalf("setup: re-entered %q, want x-alpha", m.selectedSess)
	}
	if st := status(); !strings.Contains(st, "[/zeta") {
		t.Errorf("re-entering the same session dropped its search: status row = %q", st)
	}

	press(m, tea.KeyEsc)
	m.Update(keyRune('j'))
	press(m, tea.KeyEnter) // y-beta
	if m.selectedSess != "y-beta" {
		t.Fatalf("setup: entered %q, want y-beta", m.selectedSess)
	}
	if st := status(); strings.Contains(st, "[/zeta") {
		t.Errorf("another session's events opened with the last session's search: status row = %q", st)
	}
}

// TestSearch_EventsCursorSurvivesEviction: a search's cursor is pinned to the event, not to the
// row number, so it stays on its match when the oldest events are evicted from under it.
func TestSearch_EventsCursorSurvivesEviction(t *testing.T) {
	forceColour(t)
	m := newSearchEventsModel(t, "r0.test", "r1-zeta.test", "r2.test", "r3.test", "r4-zeta.test", "r5.test")
	typeKeys(m, "/zeta") // opens on r5, the newest; wraps to r1
	press(m, tea.KeyEnter)
	checkLooks(t, "after /zeta", m.eventsTbl, map[string]string{
		"r1-zeta": "selected", "r4-zeta": "marked", "r2.test": "plain",
	})

	m.events["s"] = m.events["s"][1:] // r0 evicted: r1-zeta is row 0 now
	m.rebuildEventsTable()
	checkLooks(t, "after the eviction", m.eventsTbl, map[string]string{
		"r1-zeta": "selected", "r4-zeta": "marked", "r2.test": "plain",
	})

	m.Update(keyRune('n'))
	m.events["s"] = append(m.events["s"], hostEvents("r6-zeta.test")[0])
	m.events["s"][len(m.events["s"])-1].At = time.Now()
	m.rebuildEventsTable()
	checkLooks(t, "after n and a new event", m.eventsTbl, map[string]string{
		"r4-zeta": "selected", "r1-zeta": "marked", "r6-zeta": "marked", "r5.test": "plain",
	})
}

// TestSearch_EventsMarksFollowTheSort: a sort permutes the events rows after they are built, so
// the marks have to be read off the rows in their sorted order. Read off arrival order, they land
// on whichever rows now sit where the matches arrived.
func TestSearch_EventsMarksFollowTheSort(t *testing.T) {
	forceColour(t)
	// Arrival order c, alpha, b, delta; HOST descending puts them delta, c, b, alpha — so the
	// matches, which arrived first and third, sit second and third. No host is a substring of
	// another, or rowLook would read one row for another.
	m := newSearchEventsModel(t, "c-zeta.test", "alpha.test", "b-zeta.test", "delta.test")
	m.sortCol, m.sortDesc = colHost, true
	m.rebuildEventsTable()
	typeKeys(m, "/zeta")
	press(m, tea.KeyEnter)
	for _, tok := range []string{"delta.test", "alpha.test"} {
		if got := rowLook(m.eventsTbl, tok); got != "plain" {
			t.Errorf("%s does not match but is %s", tok, got)
		}
	}
	for _, tok := range []string{"c-zeta", "b-zeta"} {
		if got := rowLook(m.eventsTbl, tok); got == "plain" || got == "absent" {
			t.Errorf("%s matches but is %s", tok, got)
		}
	}
}

// TestSearch_DenyStillFindsDeniedEvents: the events pane's `deny` form was a filter; it is a
// search now, and finds the denied events the same way.
func TestSearch_DenyStillFindsDeniedEvents(t *testing.T) {
	forceColour(t)
	m := newSearchEventsModel(t, "r0.test", "r1.test", "r2.test")
	m.events["s"][0].Phase = pipeline.SessionDenied
	m.events["s"][0].Invocations = &pipeline.Invocations{
		Inbound: []pipeline.Invocation{{Plugin: "jwt-validation", Action: pipeline.ActionDeny}},
	}
	m.rebuildEventsTable()
	typeKeys(m, "/deny")
	press(m, tea.KeyEnter)
	checkLooks(t, "after /deny", m.eventsTbl, map[string]string{
		"r0.test": "selected", "r1.test": "plain", "r2.test": "plain",
	})
}

// TestSearch_IsNeverSaved: the filter was written to the config file on Enter and came back on
// the next start. A search is not.
func TestSearch_IsNeverSaved(t *testing.T) {
	m := newSearchSessionsModel(t, searchIDs...)
	saves := recordSaves(t, m)
	typeKeys(m, "/x")
	press(m, tea.KeyEnter)
	m.Update(keyRune('n'))
	if len(*saves) != 0 {
		t.Errorf("searching saved settings %d times, want 0", len(*saves))
	}
}

// TestFooter_SearchStatus: where the cursor is among the matches, how many there are when it is
// on none, or that there are none — while typing as well as after Enter.
func TestFooter_SearchStatus(t *testing.T) {
	m := newSearchSessionsModel(t, searchIDs...)
	status := func() string { return stripANSI(statusRow(m.footerView())) }
	hints := func() string { return stripANSI(m.helpView()) }

	if strings.Contains(hints(), "[n/N]") {
		t.Errorf("hint line offers n/N with no search: %q", hints())
	}
	typeKeys(m, "/x")
	press(m, tea.KeyEnter)
	if st := status(); !strings.Contains(st, "[/x 1/2]") {
		t.Errorf("on the first match: status row = %q, want [/x 1/2]", st)
	}
	if !strings.Contains(hints(), "[n/N] next/prev") {
		t.Errorf("hint line does not offer n/N with a search: %q", hints())
	}
	m.Update(keyRune('j')) // y-beta, not a match
	if st := status(); !strings.Contains(st, "[/x 2 matches]") {
		t.Errorf("off the matches: status row = %q, want [/x 2 matches]", st)
	}
	typeKeys(m, "/nothing")
	if st := status(); !strings.Contains(st, "[/nothing: no match]") {
		t.Errorf("typing a query that matches nothing: status row = %q, want [/nothing: no match]", st)
	}
	press(m, tea.KeyEnter)
	if st := status(); !strings.Contains(st, "[/nothing: no match]") {
		t.Errorf("no match: status row = %q, want [/nothing: no match]", st)
	}
	// An empty search, committed, is how a search is cleared.
	m.Update(keyRune('/'))
	press(m, tea.KeyEnter)
	if st := status(); strings.Contains(st, "[/") {
		t.Errorf("a cleared search still shows: %q", st)
	}
}

// assertEveryCellSearchable is the check that would have caught PHASE: for every row and
// every column on screen, the cell's drawn text, searched for, matches that row.
func assertEveryCellSearchable(t *testing.T, tbl *table.Model, search func(q string)) {
	t.Helper()
	rows, cols := tbl.Rows(), tbl.Columns()
	if len(rows) == 0 {
		t.Fatal("setup: the table has no rows")
	}
	for i, r := range rows {
		for j, v := range r {
			if j >= len(cols) || cols[j].Width <= 0 {
				continue
			}
			q := strings.TrimSpace(strings.ReplaceAll(stripANSI(v), "…", ""))
			if q == "" {
				continue
			}
			search(q)
			if !slices.Contains(tbl.Matches(), i) {
				t.Errorf("row %d, column %s: %q does not match its own row", i, cols[j].Title, q)
			}
		}
	}
	search("")
}

// richEvents is one of each row shape the events table draws differently: a plain request,
// its response, an inference call, a denial, and an opaque tunnel.
func richEvents() []pipeline.SessionEvent {
	evs := hostEvents("r0.test", "r0.test", "llm.test", "auth.test", "tun.test")
	evs[1].Phase, evs[1].StatusCode, evs[1].Duration = pipeline.SessionResponse, 200, 42*time.Millisecond
	evs[2].Inference = &pipeline.InferenceExtension{Model: "claude-opus-5-5"}
	evs[3].Phase = pipeline.SessionDenied
	evs[3].Direction = pipeline.Inbound
	evs[3].Invocations = &pipeline.Invocations{
		Inbound: []pipeline.Invocation{{Plugin: "jwt-validation", Action: pipeline.ActionDeny, Reason: "expired"}},
	}
	evs[4].Tunnel, evs[4].TunnelReason = true, pipeline.TunnelReason("no-bridge")
	return evs
}

func TestSearch_EveryEventsCellIsSearchable(t *testing.T) {
	m := newSearchEventsModel(t)
	m.events["s"] = richEvents()
	m.rebuildEventsTable()
	assertEveryCellSearchable(t, &m.eventsTbl, func(q string) {
		m.search = map[paneID]string{paneEvents: q}
		m.rebuildEventsTable()
	})
}

func TestSearch_EverySessionsCellIsSearchable(t *testing.T) {
	m := newSearchSessionsModel(t, searchIDs...)
	m.sessions[0].Title = "fix the flaky archive test"
	m.sessions[1].EventCount, m.sessions[1].TotalTokens = 12, 3400
	m.rebuildSessionsTable()
	assertEveryCellSearchable(t, &m.sessionsTbl, func(q string) {
		m.search = map[paneID]string{paneSessions: q}
		m.rebuildSessionsTable()
	})
}

// TestSearch_EventsMatchAHostPastItsCut: HOST is cut to its column; the whole host matches.
func TestSearch_EventsMatchAHostPastItsCut(t *testing.T) {
	forceColour(t)
	long := "a-very-long-hostname-that-the-column-cuts.example.com"
	m := newSearchEventsModel(t, long, "r1.test")
	typeKeys(m, "/cuts.example")
	press(m, tea.KeyEnter)
	if got := m.eventsTbl.Matches(); !slices.Equal(got, []int{0}) {
		t.Fatalf("Matches = %v, want [0]", got)
	}
}

// TestSearch_EventsMatchAColumnTheTerminalDropped: DIR gives way first on a narrow terminal.
// It is still a column the operator turned on, so it still matches, and the row is marked
// because nothing on screen shows the match.
func TestSearch_EventsMatchAColumnTheTerminalDropped(t *testing.T) {
	forceColour(t)
	m := newSearchEventsModel(t, "r0.test", "r1.test")
	hasDir := func() bool {
		return slices.ContainsFunc(m.eventsTbl.Columns(), func(c table.Column) bool {
			return strings.HasPrefix(c.Title, string(colDir))
		})
	}
	for m.width = 120; hasDir() && m.width > 30; m.width-- {
		m.layout()
	}
	if hasDir() {
		t.Skip("DIR is never dropped at any width this test tries")
	}
	m.search = map[paneID]string{paneEvents: "out"}
	m.rebuildEventsTable()
	if got := m.eventsTbl.Matches(); !slices.Equal(got, []int{0, 1}) {
		t.Fatalf("Matches = %v, want both outbound rows", got)
	}
	if got := rowLook(m.eventsTbl, "r0.test"); got != "marked" {
		t.Errorf("r0.test is %s, want marked", got)
	}
}

// TestSearch_SessionsMatchTheWholeTitle: TITLE is cut to its column; the whole title matches.
func TestSearch_SessionsMatchTheWholeTitle(t *testing.T) {
	m := newSearchSessionsModel(t, searchIDs...)
	m.width = 100
	m.sessions[2].Title = "a title long enough that the TITLE column has to cut it short, tailword"
	m.layout()
	typeKeys(m, "/tailword")
	press(m, tea.KeyEnter)
	if got := m.sessionsTbl.Matches(); len(got) != 1 || m.sessionRowIDs[got[0]] != "y-gamma" {
		t.Errorf("Matches = %v, want y-gamma's row only", got)
	}
}

// TestSearch_StreamedEventsUnderAnOpenPrompt: events keep arriving while the operator types.
// Each rebuild matches the new rows too, and the count says so.
func TestSearch_StreamedEventsUnderAnOpenPrompt(t *testing.T) {
	m := newSearchEventsModel(t, "r0-zeta.test", "r1.test")
	typeKeys(m, "/zeta")
	streamHost(m, "r2-zeta.test")
	if got := m.eventsTbl.Matches(); !slices.Equal(got, []int{0, 2}) {
		t.Fatalf("Matches = %v, want [0 2]", got)
	}
	if st := stripANSI(statusRow(m.footerView())); !strings.Contains(st, "[/zeta") {
		t.Errorf("status row = %q, want the live search", st)
	}
}

// TestSearch_SessionsMatchColumnsTheTerminalDropped: AGENT and SERVER are asked for here — two
// agents, a router with two servers — and at 80 columns the terminal has room for neither. Both
// are still searched, as a dropped column is everywhere, and as the old matcher always searched
// the agent.
func TestSearch_SessionsMatchColumnsTheTerminalDropped(t *testing.T) {
	m := sessionsServerModel(routerRaw)
	m.width = 80
	m.rebuildSessionsTable()
	for _, title := range []string{"AGENT", "SERVER"} {
		if _, shown := serverCellOf(t, m, "on-glm", title); shown {
			t.Fatalf("setup: %s is shown at 80 columns", title)
		}
	}
	// Neither query is in any session id, so only the dropped column can match it.
	for q, want := range map[string][]string{
		"opencode":  {"elsewhere", "quiet"},
		"anthropic": {"elsewhere"},
	} {
		m.search = map[paneID]string{paneSessions: q}
		m.rebuildSessionsTable()
		got := []string{}
		for _, i := range m.sessionsTbl.Matches() {
			got = append(got, m.sessionRowIDs[i])
		}
		if !slices.Equal(got, want) {
			t.Errorf("/%s matched %v, want %v", q, got, want)
		}
	}
}
