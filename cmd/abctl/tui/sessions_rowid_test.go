package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// Realistic ids, which is the whole point of this file.
//
// Every pre-existing drill-down test uses a short id like "abc" or "s1", so the SESSION cell
// never truncated and the picker's use of the rendered cell as a data channel was invisible.
// A 36-character UUID is what a real session id is.
const (
	uuidOpened = "ecb7387f-bffd-4172-adc4-8da23e992e9a"
	uuidOther  = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	// Shares its first 13 characters with uuidOpened, which is more than the SESSION cell
	// shows at any width. Two rows that RENDER identically must still be told apart.
	uuidSibling = "ecb7387f-bffd-4172-adc4-999999999999"
)

func uuidPicker(t *testing.T, width int, ids ...string) *model {
	t.Helper()
	m := &model{width: width, height: 40, endpoint: "http://x"}
	m.pane = paneSessions
	m.sessionsTbl = newSessionsTable()
	m.events = map[string][]pipeline.SessionEvent{}
	for _, id := range ids {
		m.sessions = append(m.sessions, session.SessionSummary{
			ID: id, UpdatedAt: time.Now(), EventCount: 3, TotalTokens: 1000,
		})
		m.events[id] = []pipeline.SessionEvent{
			{Phase: pipeline.SessionRequest}, {Phase: pipeline.SessionResponse},
		}
	}
	m.rebuildSessionsTable()
	return m
}

// The selected id is the FULL id, whatever the cell shows.
//
// THE CELL IS NOT A DATA CHANNEL. selectedSessionID read rows[cursor][0], so once SESSION
// began truncating, the ellipsised string became m.selectedSess — and everything keyed on a
// session id then missed.
func TestSelectedSessionID_IsTheFullIDNotTheRenderedCell(t *testing.T) {
	for _, width := range []int{72, 100, 160, 200} {
		m := uuidPicker(t, width, uuidOpened, uuidOther)
		cell := m.sessionsTbl.Rows()[0][0]
		got := m.selectedSessionID()

		if got != uuidOpened {
			t.Errorf("width %d: selectedSessionID = %q, want the full %q", width, got, uuidOpened)
		}
		// And the premise holds: the cell really is shorter, or this proves nothing.
		if cell == uuidOpened {
			t.Errorf("width %d: the cell is untruncated (%q), so this test asserted nothing",
				width, cell)
		}
		if !strings.HasPrefix(uuidOpened, strings.TrimSuffix(cell, "…")) {
			t.Errorf("width %d: cell %q is not a prefix of %q", width, cell, uuidOpened)
		}
	}
}

// Drilling into a session with a realistic id fetches it and KEEPS its events.
//
// Four things broke together when the truncated id escaped: no snapshot was requested (the id
// missed the live map and took the cached-only branch), the events pane rendered empty, the id
// sent to the server was truncated, and the release loop deleted the cache for every session
// including the one being opened — the unrecoverable loss #870 exists to prevent.
func TestDrillIntoSession_WithARealisticIDKeepsItsEventsAndFetchesIt(t *testing.T) {
	m := uuidPicker(t, 100, uuidOpened, uuidOther)

	cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})

	if m.selectedSess != uuidOpened {
		t.Errorf("selectedSess = %q, want %q", m.selectedSess, uuidOpened)
	}
	if cmd == nil {
		t.Error("no command issued: a LIVE session took the cached-only branch, so its " +
			"events are never fetched")
	}
	if n := len(m.events[uuidOpened]); n != 2 {
		t.Errorf("the opened session has %d cached events, want 2 — opening a session "+
			"deleted its own cache", n)
	}
}

// Two ids that render identically are still different sessions.
func TestSelectedSessionID_DistinguishesIDsThatRenderTheSame(t *testing.T) {
	m := uuidPicker(t, 100, uuidOpened, uuidSibling)
	rows := m.sessionsTbl.Rows()
	// THE PREMISE, asserted rather than assumed: the two ids must render to the same cell, or
	// telling them apart proves nothing about reading the id out of band.
	if rows[0][0] != rows[1][0] {
		t.Fatalf("the two ids render differently (%q vs %q), so this asserted nothing",
			rows[0][0], rows[1][0])
	}
	m.sessionsTbl.SetCursor(1)
	if got := m.selectedSessionID(); got != uuidSibling {
		t.Errorf("selectedSessionID on row 1 = %q, want %q", got, uuidSibling)
	}
}

// One id per row, always, including with a filter and cached-only rows in play.
//
// The two are built in the same loop precisely so they cannot drift, and a drift is not a
// cosmetic bug: it silently shifts every id by one, so the picker acts on a session the
// operator did not select.
func TestSessionRowIDs_MatchTheRowsOneForOne(t *testing.T) {
	cases := 0
	for _, width := range []int{60, 72, 100, 200} {
		for _, filter := range []string{"", "ecb", "zzz"} {
			m := uuidPicker(t, width, uuidOpened, uuidOther)
			// A cached-only session: held by abctl, absent from the server's list.
			m.events["cached-only-session-1234"] = []pipeline.SessionEvent{
				{Phase: pipeline.SessionRequest},
			}
			m.filter = filter
			m.rebuildSessionsTable()

			rows := m.sessionsTbl.Rows()
			if len(m.sessionRowIDs) != len(rows) {
				t.Errorf("width %d filter %q: %d ids for %d rows",
					width, filter, len(m.sessionRowIDs), len(rows))
			}
			for i, id := range m.sessionRowIDs {
				if id == "" {
					t.Errorf("width %d filter %q: row %d has an empty id", width, filter, i)
				}
				// The id and its row agree: the rendered cell is a prefix of the id it stands for.
				cell := strings.TrimSuffix(rows[i][0], "…")
				if !strings.HasPrefix(id, cell) {
					t.Errorf("width %d filter %q: row %d renders %q for id %q — the slices drifted",
						width, filter, i, rows[i][0], id)
				}
			}
			cases++
		}
	}
	if cases == 0 {
		t.Fatal("no case ran")
	}
}

// An empty picker answers "" rather than panicking on a cursor with no row.
func TestSelectedSessionID_EmptyPickerIsEmptyNotAPanic(t *testing.T) {
	m := &model{width: 100, height: 40}
	m.pane = paneSessions
	m.sessionsTbl = newSessionsTable()
	m.rebuildSessionsTable()
	if got := m.selectedSessionID(); got != "" {
		t.Errorf("selectedSessionID = %q on an empty picker, want \"\"", got)
	}
}
