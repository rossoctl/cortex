package tui

import (
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

func TestOpenAtRow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rows   int
		oldest bool
		want   int
	}{
		{"newest of many", 40, false, 39},
		{"oldest of many", 40, true, 0},
		{"single row", 1, false, 0},
		{"single row, oldest", 1, true, 0},
		// An empty table has no end to land on; setCursorVisible treats 0 as a no-op.
		{"empty", 0, false, 0},
		{"empty, oldest", 0, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := openAtRow(tc.rows, tc.oldest); got != tc.want {
				t.Errorf("openAtRow(%d, %v) = %d, want %d", tc.rows, tc.oldest, got, tc.want)
			}
		})
	}
}

// The headline behaviour: a session opens at the end the operator last jumped to.
func TestEventsTable_OpensAtThePreferredEnd(t *testing.T) {
	for _, tc := range []struct {
		name   string
		oldest bool
		want   int
	}{
		{"default is the newest, as it always was", false, 39},
		{"after g, the oldest", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := cursorModel(t, 40) // resets Settings
			Settings.Events.OpenAtOldest = tc.oldest
			// Force the next rebuild to count as an opening, which is what entering a
			// session from the picker does.
			m.eventsBuiltFor = ""
			m.rebuildEventsTable()
			if got := m.eventsTbl.Cursor(); got != tc.want {
				t.Errorf("cursor = %d, want %d", got, tc.want)
			}
		})
	}
}

// `g` and `G` are the two gestures that already mean "go to an end", so they are
// what sets the preference — and an arrow key must NOT, or scrolling up to read
// would silently change where every future session opens.
func TestGoTopGoBottom_SetThePreference(t *testing.T) {
	m := cursorModel(t, 40)

	m.goTop()
	if !Settings.Events.OpenAtOldest {
		t.Error("g did not record the oldest end")
	}
	m.goBottom()
	if Settings.Events.OpenAtOldest {
		t.Error("G did not record the newest end")
	}

	// Arrowing to row 0 is navigation, not a preference.
	m.goTop()
	Settings.Events.OpenAtOldest = false
	for i := 0; i < 45; i++ {
		m.eventsTbl.MoveUp(1)
	}
	if Settings.Events.OpenAtOldest {
		t.Error("arrowing to the top changed the preference; only g should")
	}
}

// Only the OPENING gets to choose an end. A refresh — the two-second poll, a
// filter, a column toggle — must leave the operator where they are, or reading a
// session would be impossible while events arrive.
func TestEventsTable_RefreshDoesNotReAnchor(t *testing.T) {
	m := cursorModel(t, 40)
	Settings.Events.OpenAtOldest = true

	// Open, then move somewhere deliberate. No pin is set on purpose: this fixture's
	// events are identical under keyOf (same At/direction/phase/requestID), so a pin
	// resolves to row 0 and would hide what is being tested. The unpinned
	// chronological path preserves prevRow, which is the invariant that matters.
	m.eventsBuiltFor = ""
	m.rebuildEventsTable()
	setCursorVisible(&m.eventsTbl, 17)

	// A rebuild for the SAME session is a refresh, not an opening.
	m.rebuildEventsTable()
	if got := m.eventsTbl.Cursor(); got != 17 {
		t.Errorf("cursor = %d, want 17 preserved: a refresh must not re-anchor to an end", got)
	}
}

// Under a sort the row order is a ranking, so "an end" is the largest or smallest
// value rather than the oldest or newest event — the preference must not apply, and
// the existing selectedEventKey pin carries the cursor instead.
func TestEventsTable_SortIgnoresTheOpenPreference(t *testing.T) {
	// Asserted as an INDEPENDENCE rather than a specific row: under a sort the
	// cursor is placed by the selectedEventKey pin, whose landing row is a property
	// of the fixture's values. What must hold is that flipping the preference
	// changes nothing — which is falsifiable, whereas "not row 0" would pass
	// vacuously whenever the pin happened to resolve elsewhere.
	openWith := func(oldest bool) int {
		m := sortCursorModel(t, 40)
		Settings.Events.OpenAtOldest = oldest
		m.sortCol = colDuration
		m.sortDesc = true
		m.eventsBuiltFor = ""
		m.rebuildEventsTable()
		if m.sortCol == "" {
			t.Fatal("fixture lost its sort")
		}
		return m.eventsTbl.Cursor()
	}

	atOldest, atNewest := openWith(true), openWith(false)
	if atOldest != atNewest {
		t.Errorf("the open-at preference moved the cursor under a sort: %d with oldest, %d with newest — "+
			"under a ranking an 'end' is a value, not an event, and the pin should decide",
			atOldest, atNewest)
	}
}

// Setting a preference has to survive, or the whole point is lost. Asserted
// through the persistence callback rather than the file, which is what the rest of
// the settings tests do.
func TestSetOpenAtOldest_Persists(t *testing.T) {
	resetSettingsForTest(t)
	saved := 0
	m := &model{save: func(UserSettings) error { saved++; return nil }}

	m.setOpenAtOldest(true)
	if saved != 1 {
		t.Errorf("save called %d times, want 1", saved)
	}
	// No write for a keypress that changed nothing: `g` twice is one preference.
	m.setOpenAtOldest(true)
	if saved != 1 {
		t.Errorf("save called %d times for an unchanged value, want 1", saved)
	}
	m.setOpenAtOldest(false)
	if saved != 2 {
		t.Errorf("save called %d times after a change, want 2", saved)
	}
}

// THE PATH THAT ACTUALLY HAPPENS, and the one the first version of this feature
// silently failed: entering a session rebuilds the table BEFORE the snapshot
// returns, so for any session whose history is not cached the first rebuild has
// zero rows. Latching "opening" on that empty rebuild made the real one a refresh,
// and the cursor went to the newest row with the preference ignored.
//
// The earlier test set m.eventsBuiltFor = "" by hand and so never exercised this —
// it tested the implementation rather than the sequence.
func TestEventsTable_OpeningSurvivesAnEmptyFirstRebuild(t *testing.T) {
	for _, tc := range []struct {
		name   string
		oldest bool
		want   int
	}{
		{"oldest", true, 0},
		{"newest", false, 39},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetSettingsForTest(t)
			Settings.Events.OpenAtOldest = tc.oldest

			// Entering a session: selectedSess is set and the table is rebuilt with
			// nothing in it yet, exactly as keys.go does before snapshotCmd.
			m := &model{
				pane: paneEvents, selectedSess: "s", bodyHeight: 12, width: 200,
				events: map[string][]pipeline.SessionEvent{},
			}
			m.eventsTbl = newEventsTable()
			m.rebuildEventsTable()
			if got := len(m.eventsTbl.Rows()); got != 0 {
				t.Fatalf("setup: expected an empty first rebuild, got %d rows", got)
			}

			// The snapshot lands.
			m.events["s"] = cursorRowsFixture(40)
			m.rebuildEventsTable()

			if got := m.eventsTbl.Cursor(); got != tc.want {
				t.Errorf("cursor = %d, want %d — the empty rebuild consumed the opening", got, tc.want)
			}
		})
	}
}
