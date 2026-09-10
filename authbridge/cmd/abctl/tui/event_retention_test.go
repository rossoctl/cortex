package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
)

// #870: users reported the events they were investigating vanishing after a
// proxy restart or a communication blip.
//
// The store is in-memory and per-pod, so abctl's cache is the only copy. An
// empty /v1/sessions arrives as an ordinary message, not an error, so the old
// reconcile read "the server does not list this" as "delete it" and wiped the
// events about two seconds after the user looked away.
func TestSessionsRefresh_KeepsEventsAndPane(t *testing.T) {
	m := newRetentionModel(t, "default", 3)

	m.Update(sessionsLoadedMsg{}) // proxy restarted: list is empty

	if got := len(m.events["default"]); got != 3 {
		t.Errorf("cached events dropped on an empty server list: got %d, want 3", got)
	}
	if m.pane != paneEvents {
		t.Errorf("pane changed under the user: got %v, want paneEvents", m.pane)
	}
	if m.selectedSess != "default" {
		t.Errorf("selection cleared: got %q, want %q", m.selectedSess, "default")
	}
}

// The same protection while the user is on the usage charts, which are computed
// from the same cache.
func TestSessionsRefresh_KeepsUsageInvestigation(t *testing.T) {
	m := newRetentionModel(t, "default", 3)
	m.pane = paneUsage

	m.Update(sessionsLoadedMsg{})

	if got := len(m.events["default"]); got != 3 {
		t.Errorf("cached events dropped while on the usage pane: got %d, want 3", got)
	}
	if m.pane != paneUsage {
		t.Errorf("pane changed under the user: got %v, want paneUsage", m.pane)
	}
}

// A session the server drops individually (evicted under max_sessions) is
// retained too — same reasoning, and the user may still be reading it.
func TestSessionsRefresh_KeepsEvictedSession(t *testing.T) {
	m := newRetentionModel(t, "old", 3)

	m.Update(sessionsLoadedMsg{{ID: "fresh", UpdatedAt: time.Now()}})

	if got := len(m.events["old"]); got != 3 {
		t.Errorf("evicted session's events dropped: got %d, want 3", got)
	}
}

// Retention is only half a fix if the events cannot be reached. After a restart
// the server lists nothing, so the picker must still offer a row for whatever
// the cache holds.
func TestSessionsPicker_ListsCachedOnlySessions(t *testing.T) {
	m := newRetentionModel(t, "default", 3)

	m.Update(sessionsLoadedMsg{})
	m.rebuildSessionsTable()

	var row []string
	for _, r := range m.sessionsTbl.Rows() {
		if r[0] == "default" {
			row = r
		}
	}
	if row == nil {
		t.Fatal("no picker row for a session whose events are still cached — " +
			"the retained history is unreachable")
	}
	if row[2] != "3" {
		t.Errorf("row event count = %q, want %q", row[2], "3")
	}
	if row[4] != "cached" {
		t.Errorf("row not marked as cached-only: %v", row)
	}
}

// The cached-only marker must survive rendering under a colour profile. bubbles
// truncates each cell with runewidth.Truncate BEFORE styling, and runewidth is
// not ANSI-aware, so a styled cell measures its escape bytes against the column
// width and comes out mangled with the reset stripped. The marker is therefore
// plain text; CI has no TTY and cannot catch a regression here, so force one.
func TestSessionsPicker_CachedMarkerRendersIntact(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(orig) })

	m := newRetentionModel(t, "vanished", 3)
	m.pane = paneSessions
	m.Update(sessionsLoadedMsg{})
	m.rebuildSessionsTable()

	view := m.sessionsTbl.View()
	if !strings.Contains(view, "cached") {
		t.Errorf("marker did not survive rendering:\n%s", view)
	}
	if strings.Contains(view, "cache…") || strings.Contains(view, "cach…") {
		t.Error("marker was truncated mid-word — a styled cell is being measured " +
			"with its escape bytes counted against the column width")
	}
}

// A cache key with no events must not produce a row: snapshotLoadedMsg assigns
// m.events[id] unconditionally, so drilling into an empty session creates the
// key, and a row advertising zero events helps nobody.
func TestSessionsPicker_SkipsEmptyCacheKeys(t *testing.T) {
	m := newRetentionModel(t, "real", 3)
	m.events["empty"] = nil

	m.Update(sessionsLoadedMsg{})
	m.rebuildSessionsTable()

	for _, r := range m.sessionsTbl.Rows() {
		if r[0] == "empty" {
			t.Errorf("empty cache key produced a picker row: %v", r)
		}
	}
}

// The single release point: picking a DIFFERENT session in the picker. That is
// the only reliable signal the previous events stopped mattering, and it is
// what bounds the cache now that the refresh never deletes.
func TestPickingAnotherSession_ReleasesThePrevious(t *testing.T) {
	m := newRetentionModel(t, "default", 3)
	m.events["other"] = make([]pipeline.SessionEvent, 2)
	m.sessions = []session.SessionSummary{{ID: "default"}, {ID: "other"}}
	m.rebuildSessionsTable()

	// Back to the picker, cursor on "other", press enter.
	m.pane = paneSessions
	for i, r := range m.sessionsTbl.Rows() {
		if r[0] == "other" {
			m.sessionsTbl.SetCursor(i)
		}
	}
	m.handleKey(keyRune('l'))

	if m.selectedSess != "other" {
		t.Fatalf("precondition: handler did not open \"other\" (got %q)", m.selectedSess)
	}
	if _, still := m.events["default"]; still {
		t.Error("the previous session's events were not released")
	}
	if got := len(m.events["other"]); got != 2 {
		t.Errorf("the newly-opened session's events were dropped: got %d, want 2", got)
	}
}

// Re-opening the SAME session must not release its own events.
func TestReopeningSameSession_KeepsItsEvents(t *testing.T) {
	m := newRetentionModel(t, "default", 3)
	m.rebuildSessionsTable()
	m.pane = paneSessions

	m.handleKey(keyRune('l'))

	if got := len(m.events["default"]); got != 3 {
		t.Errorf("re-opening the same session dropped its events: got %d, want 3", got)
	}
}

func newRetentionModel(t *testing.T, id string, n int) *model {
	t.Helper()
	evs := make([]pipeline.SessionEvent, n)
	for i := range evs {
		evs[i] = pipeline.SessionEvent{
			At:        time.Now(),
			Direction: pipeline.Outbound,
			Phase:     pipeline.SessionRequest,
			Host:      "api.example.com",
		}
	}
	m := &model{
		pane:         paneEvents,
		selectedSess: id,
		width:        200,
		height:       40,
		bodyHeight:   12,
		events:       map[string][]pipeline.SessionEvent{id: evs},
		eventColumns: defaultColumnSelection(),
		sessions:     []session.SessionSummary{{ID: id}},
	}
	m.eventsTbl = newEventsTable()
	m.sessionsTbl = newSessionsTable()
	m.rebuildEventsTable()
	return m
}
