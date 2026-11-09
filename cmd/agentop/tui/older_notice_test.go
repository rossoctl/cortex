package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// A truncated timeline has to say so. Without this the events pane looks identical
// whether a session has 3 events or 5000 of which 3 arrived — which is exactly how the
// unbounded-snapshot bug presented: a full session rendering as three rows, with the
// failure only visible as a transport error in the footer.
func TestTimeline_NamesTheEventsTheSnapshotDidNotFetch(t *testing.T) {
	m := fitModel(t, paneEvents, 200, 40, cursorRowsFixture(3))
	m.rebuildEventsTable()

	if got := m.eventsTbl.Rows()[0][0]; strings.Contains(got, "older") {
		t.Fatalf("an untruncated snapshot must say nothing about older events: %q", got)
	}

	m.Update(snapshotLoadedMsg{id: m.selectedSess, events: cursorRowsFixture(3), olderNotFetched: 4571})
	if got := m.eventsTbl.Rows()[0][0]; got != "▲ press Enter to load 4571 older events" {
		t.Errorf("first row does not name the omitted events: %q", got)
	}
	if n := len(m.eventsTbl.Rows()); n != 4 {
		t.Errorf("%d rows, want the older row and 3 events", n)
	}
	// The row is off the screen at the tail, so the footer points at it.
	if got := m.helpView(); !strings.Contains(got, "4571 older (▲ at top)") {
		t.Errorf("footer does not point at the older row: %q", got)
	}
}

// The count comes from the server's own total, and this drives the real snapshotCmd
// against a real HTTP server rather than re-implementing its arithmetic in the test —
// a test that recomputes the thing it is checking passes whatever the code does.
func TestSnapshotCmd_DerivesTheOlderCountFromTotalEvents(t *testing.T) {
	for _, tc := range []struct {
		name        string
		total, sent int
		want        int
	}{
		{"truncated", 5000, 4, 4996},
		{"whole session reports nothing", 0, 4, 0},
		{"total equal to sent reports nothing", 4, 4, 0},
		{"a total below sent cannot underflow", 2, 4, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(pipeline.SessionView{
					ID:          "s1",
					Events:      cursorRowsFixture(tc.sent),
					TotalEvents: tc.total,
				})
			}))
			defer ts.Close()

			m := &model{ctx: context.Background(), client: apiclient.New(ts.URL)}
			msg, ok := m.snapshotCmd("s1")().(snapshotLoadedMsg)
			if !ok {
				t.Fatalf("snapshotCmd returned %T, want snapshotLoadedMsg", m.snapshotCmd("s1")())
			}
			if msg.olderNotFetched != tc.want {
				t.Errorf("olderNotFetched = %d, want %d", msg.olderNotFetched, tc.want)
			}
			if len(msg.events) != tc.sent {
				t.Errorf("events = %d, want %d", len(msg.events), tc.sent)
			}
		})
	}
}

// Snapshots land asynchronously, for whichever session was selected when the fetch
// started. A late one for a session the operator has left must not describe the session
// they are looking at — which is what a single shared counter did.
func TestTimeline_OlderCountIsPerSession(t *testing.T) {
	m := fitModel(t, paneEvents, 200, 40, cursorRowsFixture(3))
	m.selectedSess = "current"
	m.events["current"] = cursorRowsFixture(3)
	m.rebuildEventsTable()

	// The snapshot for the session being viewed: nothing omitted.
	m.Update(snapshotLoadedMsg{id: "current", events: cursorRowsFixture(3)})
	// Then a straggler for a session that was abandoned, reporting thousands omitted.
	m.Update(snapshotLoadedMsg{id: "abandoned", events: cursorRowsFixture(1), olderNotFetched: 9999})

	if got := m.olderNotice(); got != "" {
		t.Errorf("a late snapshot for another session leaked into the timeline: %q", got)
	}

	// And selecting that session shows its own count, not the current one's.
	m.selectedSess = "abandoned"
	if got := m.olderNotice(); !strings.Contains(got, "9999 older events") {
		t.Errorf("the session's own count is not shown after selecting it: %q", got)
	}
}

// The count and the events it describes are cleared together.
//
// Two sites drop cached events — backToPodsPane resets the map wholesale on an endpoint
// switch, and the picker prunes single entries the server still lists. A count left behind
// by either describes events that no longer exist, which is the bug that made this map
// per-session, only narrower: it needs the operator back in the events pane on a matching
// session id before that session's snapshot lands.
func TestOlderCount_IsClearedWithTheEventsItDescribes(t *testing.T) {
	t.Run("wholesale reset", func(t *testing.T) {
		m := fitModel(t, paneEvents, 200, 40, cursorRowsFixture(3))
		// backToPodsPane derives a fresh context from parentCtx, which fitModel leaves nil.
		m.parentCtx, m.ctx = context.Background(), context.Background()
		m.cancel = func() {}
		m.Update(snapshotLoadedMsg{id: "s1", events: cursorRowsFixture(1), olderNotFetched: 700})
		if m.olderNotFetched["s1"] == 0 {
			t.Fatal("fixture did not record a count")
		}

		m.backToPodsPane()

		if len(m.events) != 0 {
			t.Fatalf("events survived the reset: %d", len(m.events))
		}
		if n := m.olderNotFetched["s1"]; n != 0 {
			t.Errorf("count %d survived a reset that dropped its events", n)
		}
	})

	t.Run("single-entry prune", func(t *testing.T) {
		m := fitModel(t, paneEvents, 200, 40, cursorRowsFixture(3))
		m.selectedSess = "keep"
		m.events["keep"] = cursorRowsFixture(2)
		m.Update(snapshotLoadedMsg{id: "prune-me", events: cursorRowsFixture(1), olderNotFetched: 900})
		m.Update(snapshotLoadedMsg{id: "keep", events: cursorRowsFixture(2), olderNotFetched: 5})

		// The picker prunes cached entries the server still lists, on selecting another
		// session. Drive it the way handleKey does.
		m.sessions = []session.SessionSummary{{ID: "prune-me"}, {ID: "keep"}}
		m.rebuildSessionsTable()
		setCursorVisible(&m.sessionsTbl, 0)
		m.pane = paneSessions
		m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})

		// The invariant, rather than which id the picker happens to prune: a count may
		// not outlive the events it describes. Selecting a session prunes the OTHER
		// cached-but-live entries, so hardcoding a survivor here would pin the picker's
		// policy instead of the lockstep — the first version of this did, and failed for
		// naming the wrong side.
		if len(m.olderNotFetched) == 0 {
			t.Fatal("fixture recorded no counts, so nothing is under test")
		}
		for id, n := range m.olderNotFetched {
			if _, haveEvents := m.events[id]; !haveEvents && n > 0 {
				t.Errorf("count %d survives for %q, whose events were pruned", n, id)
			}
		}
	})
}

// The older-events row is the timeline's first row at every terminal size, and the view
// still fits the terminal with it.
func TestTimeline_OlderRowIsTheFirstRow(t *testing.T) {
	forceColor(t)
	for _, dim := range fitSizes {
		m := fitModel(t, paneEvents, dim[0], dim[1], cursorRowsFixture(60))
		label := fmt.Sprintf("%dx%d", dim[0], dim[1])
		m.Update(snapshotLoadedMsg{id: m.selectedSess, events: cursorRowsFixture(60), olderNotFetched: 4571})
		m.goTop() // the session opens at the newest event, below the screen's first rows
		lines := strings.Split(m.View(), "\n")
		for i, line := range lines {
			if strings.Contains(line, "TIME") && (i+1 == len(lines) || !strings.Contains(lines[i+1], "press Enter")) {
				t.Errorf("%s: the line under the headers is not the older row", label)
			}
		}
		assertFits(t, m, label)
	}
}

// olderRowModel is a session of ten events, Seq 501-510, with 500 older ones not loaded.
func olderRowModel(t *testing.T) *model {
	t.Helper()
	m := fitModel(t, paneEvents, 200, 40, pagedEvents(501, 10))
	m.client = apiclient.New("http://127.0.0.1:1")
	m.ctx = context.Background()
	m.olderNotFetched = map[string]int{"sess-1": 500}
	m.rebuildEventsTable()
	return m
}

// ↑ from the first event selects the older row, and Enter on it loads the page before. When
// the page lands, the cursor is on the event that was first, with the loaded page above it.
func TestOlderRow_UpSelectsItAndEnterLoadsAbove(t *testing.T) {
	m := olderRowModel(t)
	setCursorVisible(&m.eventsTbl, 1)
	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if !m.onOlderRow() {
		t.Fatalf("↑ from the first event left the cursor on row %d", m.eventsTbl.Cursor())
	}

	if cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil {
		t.Fatalf("Enter on the older row asked for nothing (flash %q)", m.flash)
	}
	st := m.paging["sess-1"]
	if st == nil || !st.loading {
		t.Fatal("Enter on the older row did not start a page load")
	}
	if m.pane != paneEvents {
		t.Errorf("Enter on the older row opened pane %v", m.pane)
	}

	m.Update(olderPageLoadedMsg{id: "sess-1", gen: st.wantGen, events: pagedEvents(491, 10), serverOldest: 1})
	if e := m.selectedEvent(); e == nil || e.Seq != 501 {
		t.Errorf("after the load the cursor is on %+v, want the event that was first (Seq 501)", e)
	}
	if got := m.eventsTbl.Rows()[0][0]; !strings.Contains(got, "490 older events") {
		t.Errorf("first row after the load: %q", got)
	}
}

// A sort ranks the events, not the older row: it stays first, in both directions.
func TestOlderRow_StaysFirstUnderASort(t *testing.T) {
	for _, desc := range []bool{false, true} {
		m := olderRowModel(t)
		m.sortCol, m.sortDesc = colTime, desc
		m.rebuildEventsTable()
		if got := m.eventsTbl.Rows()[0][0]; !strings.Contains(got, "older events") {
			t.Errorf("desc=%v: first row is %q", desc, got)
		}
		want := uint64(501)
		if desc {
			want = 510
		}
		if e := m.selectedEventOn(1); e == nil || e.Seq != want {
			t.Errorf("desc=%v: row 1 is %+v, want Seq %d", desc, e, want)
		}
	}
}

// When the first row under the older row is a subagent header, which has no event, the load
// keeps the cursor on the first row that has one, not on the older row above the new page.
func TestOlderRow_LoadKeepsPlaceUnderASubagentHeader(t *testing.T) {
	m := olderRowModel(t)
	for i := range 3 {
		m.events["sess-1"][i].Subagent = "a1"
	}
	m.rebuildEventsTable()
	if _, ok := m.headerRows[1]; !ok {
		t.Fatal("row 1 is not the subagent header")
	}
	setCursorVisible(&m.eventsTbl, 0)
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	st := m.paging["sess-1"]
	if st == nil {
		t.Fatal("Enter on the older row did not start a page load")
	}
	m.Update(olderPageLoadedMsg{id: "sess-1", gen: st.wantGen, events: pagedEvents(491, 10), serverOldest: 1})
	if e := m.selectedEvent(); e == nil || e.Seq != 504 {
		t.Errorf("after the load the cursor is on %+v (row %d), want the first main event (Seq 504)", e, m.eventsTbl.Cursor())
	}
}

// Enter on the older row that starts no load leaves the cursor there, through later rebuilds.
func TestOlderRow_RefusedLoadKeepsTheCursor(t *testing.T) {
	m := olderRowModel(t)
	for i := range m.events["sess-1"] {
		m.events["sess-1"][i].Seq = 0 // a proxy that predates Seq: loadOlderPage refuses
	}
	m.rebuildEventsTable()
	setCursorVisible(&m.eventsTbl, 0)
	if cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("a proxy with no Seq started a page load")
	}
	m.rebuildEventsTable() // the next poll or streamed event
	if !m.onOlderRow() {
		t.Errorf("the cursor moved to row %d with no key press", m.eventsTbl.Cursor())
	}
}

// The older row replaced the o key.
func TestOKey_NoLongerLoadsOlder(t *testing.T) {
	m := olderRowModel(t)
	if cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")}); cmd != nil || m.paging["sess-1"] != nil {
		t.Error("o still starts a page load")
	}
}

// A search never matches the older row: not its words, and not its count, which can be a
// status code the operator searches for.
func TestOlderRow_SearchDoesNotMatchIt(t *testing.T) {
	for _, q := range []string{"older", "press enter", "500"} {
		m := olderRowModel(t)
		if m.search == nil {
			m.search = map[paneID]string{}
		}
		m.search[paneEvents] = q
		m.rebuildEventsTable()
		if m.olderNotice() == "" {
			t.Fatal("the older row is not shown")
		}
		if got := m.eventsTbl.Matches(); len(got) > 0 && got[0] == 0 {
			t.Errorf("/%s matches the older row: matches %v", q, got)
		}
	}
}

// The older row goes away when a page reaches the session's start, whatever the count says:
// the count drifts when the store drops events after the snapshot.
func TestOlderRow_GoesAwayAtTheSessionStart(t *testing.T) {
	for name, page := range map[string][]pipeline.SessionEvent{
		"page reaches the start": pagedEvents(491, 10),
		"empty page":             nil,
	} {
		m := olderRowModel(t)
		setCursorVisible(&m.eventsTbl, 0)
		m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
		st := m.paging["sess-1"]
		if st == nil {
			t.Fatalf("%s: Enter on the older row did not start a page load", name)
		}
		m.Update(olderPageLoadedMsg{id: "sess-1", gen: st.wantGen, events: page, serverOldest: 491})
		if got := m.olderNotice(); got != "" {
			t.Errorf("%s: the older row is still shown at the session start: %q", name, got)
		}
	}
}

// When the pinned event is gone, the cursor goes to the first row under the older row, not to it.
func TestOlderRow_LostPinSkipsTheOlderRow(t *testing.T) {
	m := olderRowModel(t)
	setCursorVisible(&m.eventsTbl, 5)
	m.selectedEventKey = eventKey{at: 1} // an event the rows no longer hold
	m.rebuildEventsTable()
	if e := m.selectedEvent(); e == nil || e.Seq != 501 {
		t.Errorf("cursor on row %d (%+v), want the oldest event (Seq 501)", m.eventsTbl.Cursor(), e)
	}
}
