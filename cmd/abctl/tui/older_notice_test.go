package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/abctl/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// A truncated timeline has to say so. Without this the events pane looks identical
// whether a session has 3 events or 5000 of which 3 arrived — which is exactly how the
// unbounded-snapshot bug presented: a full session rendering as three rows, with the
// failure only visible as a transport error in the footer.
func TestFooter_NamesTheEventsTheSnapshotDidNotFetch(t *testing.T) {
	m := fitModel(t, paneEvents, 200, 40, cursorRowsFixture(3))
	m.rebuildEventsTable()

	if got := m.helpView(); strings.Contains(got, "older") {
		t.Fatalf("an untruncated snapshot must say nothing about older events: %q", got)
	}

	m.Update(snapshotLoadedMsg{id: m.selectedSess, events: cursorRowsFixture(3), olderNotFetched: 4571})
	got := m.helpView()
	if !strings.Contains(got, "4571 older ([o] to load)") {
		t.Errorf("footer does not name the omitted events: %q", got)
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
func TestFooter_OlderCountIsPerSession(t *testing.T) {
	m := fitModel(t, paneEvents, 200, 40, cursorRowsFixture(3))
	m.selectedSess = "current"
	m.events["current"] = cursorRowsFixture(3)
	m.rebuildEventsTable()

	// The snapshot for the session being viewed: nothing omitted.
	m.Update(snapshotLoadedMsg{id: "current", events: cursorRowsFixture(3)})
	// Then a straggler for a session that was abandoned, reporting thousands omitted.
	m.Update(snapshotLoadedMsg{id: "abandoned", events: cursorRowsFixture(1), olderNotFetched: 9999})

	if got := m.helpView(); strings.Contains(got, "9999") {
		t.Errorf("a late snapshot for another session leaked into the footer: %q", got)
	}

	// And selecting that session shows its own count, not the current one's.
	m.selectedSess = "abandoned"
	if got := m.helpView(); !strings.Contains(got, "9999 older ([o] to load)") {
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
