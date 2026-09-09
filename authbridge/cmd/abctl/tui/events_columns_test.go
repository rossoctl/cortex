package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

func colNames(cols []eventColumn) []string {
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, string(c.id))
	}
	return out
}

func has(cols []eventColumn, id eventColumnID) bool {
	for _, c := range cols {
		if c.id == id {
			return true
		}
	}
	return false
}

// The scenario from issue #866: "if I suppressed the DIR, TOKEN and TIME columns
// the HOST column could appear on the screen using a reasonable font size."
//
// This is the acceptance test for the whole feature, and it failed on the first
// implementation: HOST is last in display order, so dropping from the right took
// the one column the user had turned others off to see.
func TestFitColumns_IssueScenarioRevealsHost(t *testing.T) {
	sel := defaultColumnSelection()
	sel[colDir] = false
	sel[colTokens] = false
	sel[colTime] = false
	sel[colHost] = true

	fitted, _ := fitColumns(selectedColumns(sel), 80)

	if !has(fitted, colHost) {
		t.Errorf("HOST not visible at 80 columns after suppressing DIR/TIME/TOKENS; got %v",
			colNames(fitted))
	}
	if w := columnsWidth(fitted); w > 80 {
		t.Errorf("fitted width %d exceeds the terminal", w)
	}
	for _, off := range []eventColumnID{colDir, colTokens, colTime} {
		if has(fitted, off) {
			t.Errorf("%s was suppressed but still rendered", off)
		}
	}
}

// A column the user turned on explicitly must outrank one that is merely on by
// default. Without this the feature does not work: the user's choice is the first
// thing sacrificed.
func TestFitColumns_OptedInColumnsSurviveDefaults(t *testing.T) {
	sel := defaultColumnSelection()
	sel[colHost] = true // opted in; defaultOn is false

	fitted, dropped := fitColumns(selectedColumns(sel), 90)
	if dropped == 0 {
		t.Skip("terminal wide enough that nothing was dropped")
	}
	if !has(fitted, colHost) {
		t.Errorf("the opted-in HOST column was dropped before default-on ones; got %v",
			colNames(fitted))
	}
}

// Whatever the width, the result must fit and must never be empty — a blank pane
// is unrecoverable without restarting.
func TestFitColumns_AlwaysFitsAndNeverEmpty(t *testing.T) {
	all := selectedColumns(defaultColumnSelection())
	for _, width := range []int{1, 5, 20, 40, 80, 100, 120, 140, 200, 400} {
		fitted, dropped := fitColumns(all, width)
		if len(fitted) == 0 {
			t.Fatalf("width %d: no columns at all", width)
		}
		// One column may exceed a very narrow terminal — better a clipped cell
		// than nothing — but with room to spare the fit must be honest.
		if len(fitted) > 1 && columnsWidth(fitted) > width {
			t.Errorf("width %d: fitted %d columns needing %d", width, len(fitted), columnsWidth(fitted))
		}
		if dropped != len(all)-len(fitted) {
			t.Errorf("width %d: reported %d dropped, actually dropped %d",
				width, dropped, len(all)-len(fitted))
		}
	}
}

// The first column is never sacrificed: "#" pairs a request with its response, and
// without it the timeline cannot be read at all.
func TestFitColumns_KeepsTheIndexColumn(t *testing.T) {
	all := selectedColumns(defaultColumnSelection())
	for _, width := range []int{1, 10, 30, 60} {
		fitted, _ := fitColumns(all, width)
		if len(fitted) == 0 || fitted[0].id != colIndex {
			t.Errorf("width %d: lost the # column; got %v", width, colNames(fitted))
		}
	}
}

// A wide terminal drops nothing, so the indicator stays silent when there is
// nothing to indicate.
func TestFitColumns_WideTerminalDropsNothing(t *testing.T) {
	all := selectedColumns(defaultColumnSelection())
	fitted, dropped := fitColumns(all, columnsWidth(all))
	if dropped != 0 || len(fitted) != len(all) {
		t.Errorf("exact-width terminal dropped %d of %d", dropped, len(all))
	}
}

// Turning every column off must not leave a blank pane: the selection falls back
// to the defaults, since a user cannot recover from an empty table in-place.
func TestSelectedColumns_EmptySelectionFallsBack(t *testing.T) {
	sel := map[eventColumnID]bool{}
	got := selectedColumns(sel)
	if len(got) == 0 {
		t.Fatal("empty selection produced no columns")
	}
	if !has(got, colIndex) {
		t.Errorf("fallback does not include #; got %v", colNames(got))
	}
}

// Header and cells come from one definition, so they cannot disagree. They were
// two hand-maintained parallel lists, where inserting a column shifted every later
// cell under the wrong heading.
func TestTableColumns_MatchesTheDefinition(t *testing.T) {
	cols := selectedColumns(defaultColumnSelection())
	tc := tableColumns(cols)
	if len(tc) != len(cols) {
		t.Fatalf("tableColumns produced %d for %d columns", len(tc), len(cols))
	}
	for i := range cols {
		if tc[i].Title != string(cols[i].id) {
			t.Errorf("column %d: header %q, definition %q", i, tc[i].Title, cols[i].id)
		}
		if tc[i].Width != cols[i].width {
			t.Errorf("column %d (%s): header width %d, definition %d",
				i, cols[i].id, tc[i].Width, cols[i].width)
		}
	}
}

// Every column must render a cell, or a selection would panic on a nil func.
func TestEventColumns_AllHaveCellFuncs(t *testing.T) {
	seen := map[eventColumnID]bool{}
	for _, c := range eventColumns {
		if c.cell == nil {
			t.Errorf("column %s has no cell function", c.id)
		}
		if c.width <= 0 {
			t.Errorf("column %s has width %d", c.id, c.width)
		}
		if seen[c.id] {
			t.Errorf("duplicate column id %s", c.id)
		}
		seen[c.id] = true
	}
}

// The picker names every column, marks which are on, and shows the cursor —
// otherwise a user cannot tell what pressing space would do.
func TestColumnPickerLine(t *testing.T) {
	sel := defaultColumnSelection()
	line := stripANSI(columnPickerLine(sel, 0))

	for _, c := range eventColumns {
		if !strings.Contains(line, string(c.id)) {
			t.Errorf("picker omits %s: %q", c.id, line)
		}
	}
	// The cursor is bracketed, so the highlighted column is identifiable without
	// colour — a terminal without it, or a screenshot, still reads.
	if !strings.Contains(line, "["+string(eventColumns[0].id)+"]") {
		t.Errorf("picker does not bracket the cursor column: %q", line)
	}
	if strings.Contains(stripANSI(columnPickerLine(sel, 3)), "["+string(eventColumns[0].id)+"]") {
		t.Error("bracket did not move with the cursor")
	}
}

// newTestEventsModel builds a model with a populated events table, at a width
// wide enough for every default column.
func newTestEventsModel(t *testing.T) *model {
	t.Helper()
	events := make([]pipeline.SessionEvent, 6)
	for i := range events {
		events[i] = pipeline.SessionEvent{
			At:        time.Now(),
			Direction: pipeline.Outbound,
			Phase:     pipeline.SessionRequest,
			Host:      "api.example.com",
			Inference: &pipeline.InferenceExtension{Model: "claude-sonnet-5", TotalTokens: 100},
		}
	}
	m := &model{
		pane: paneEvents, selectedSess: "s", bodyHeight: 12, width: 200,
		events:       map[string][]pipeline.SessionEvent{"s": events},
		eventColumns: defaultColumnSelection(),
	}
	m.eventsTbl = newEventsTable()
	m.rebuildEventsTable()
	return m
}

// Toggling a column OFF must not panic.
//
// bubbles' SetColumns calls UpdateViewport, which re-renders the rows already
// loaded; its renderRow iterates the ROW's cells while indexing m.cols[i], so a
// row wider than the new column set reads past the end of m.cols. Setting the
// columns before the rows was exactly that — 11 stale cells against 10 new
// columns — and it panicked inside the bubbletea render loop the first time a
// user pressed space.
func TestRebuildEventsTable_ToggleOffDoesNotPanic(t *testing.T) {
	m := newTestEventsModel(t)

	// Start wide so every default column is present, then narrow the selection one
	// column at a time. Each rebuild renders the previous, wider rows.
	for _, id := range []eventColumnID{colCost, colTokens, colDuration, colStatus, colMethod} {
		m.eventColumns[id] = false
		m.rebuildEventsTable() // must not panic
	}
	// And back on again, which widens rows against a narrower column set.
	for _, id := range []eventColumnID{colMethod, colStatus, colDuration, colTokens, colCost} {
		m.eventColumns[id] = true
		m.rebuildEventsTable()
	}
}

// Every row must carry exactly one cell per rendered column. A mismatch is what
// panics inside bubbles, so assert the invariant directly rather than only that we
// survived.
func TestRebuildEventsTable_RowWidthMatchesColumns(t *testing.T) {
	m := newTestEventsModel(t)

	for _, sel := range []map[eventColumnID]bool{
		defaultColumnSelection(),
		{colIndex: true, colHost: true},
		{colIndex: true},
		{}, // falls back to the defaults
	} {
		m.eventColumns = sel
		m.rebuildEventsTable()

		want := len(m.eventsTbl.Columns())
		for i, row := range m.eventsTbl.Rows() {
			if len(row) != want {
				t.Errorf("row %d has %d cells for %d columns", i, len(row), want)
			}
		}
	}
}

// A narrow terminal re-fits on every rebuild, so the same invariant has to hold
// as the width changes underneath.
func TestRebuildEventsTable_WidthChangesKeepRowsAligned(t *testing.T) {
	m := newTestEventsModel(t)
	for _, w := range []int{200, 120, 80, 40, 200} {
		m.width = w
		m.rebuildEventsTable()
		want := len(m.eventsTbl.Columns())
		for i, row := range m.eventsTbl.Rows() {
			if len(row) != want {
				t.Errorf("width %d: row %d has %d cells for %d columns", w, i, len(row), want)
			}
		}
	}
}

// Drive the picker the way a user does — `c`, then space and arrow keys. The crash
// this guards against fired through the key path, not through a direct
// rebuildEventsTable call, so exercise that path end to end.
func TestColumnPicker_KeyPathSurvivesEveryToggle(t *testing.T) {
	m := newTestEventsModel(t)

	m.handleKey(keyRune('c'))
	if !m.colPicker {
		t.Fatal("`c` did not open the picker")
	}
	// Toggle every column off, moving right each time.
	for i := 0; i < len(eventColumns); i++ {
		m.handleKey(keyRune(' '))
		m.handleKey(keyRune('l'))
	}
	// Never a blank table, however much was turned off.
	if len(m.eventsTbl.Columns()) == 0 {
		t.Error("every column off left a blank table")
	}
	// Reset restores the defaults.
	m.handleKey(keyRune('r'))
	if got, want := len(m.eventsTbl.Columns()), len(selectedColumns(defaultColumnSelection())); got != want {
		t.Errorf("after reset: %d columns, want %d", got, want)
	}
	m.handleKey(keyRune('c'))
	if m.colPicker {
		t.Error("`c` did not close the picker")
	}
}

// The picker owns the keyboard while open: space and the arrows must not also move
// the table cursor underneath it.
func TestColumnPicker_OwnsTheKeyboard(t *testing.T) {
	m := newTestEventsModel(t)
	m.eventsTbl.SetCursor(2)

	m.handleKey(keyRune('c'))
	m.handleKey(keyRune('l'))
	m.handleKey(keyRune(' '))

	if got := m.eventsTbl.Cursor(); got != 2 {
		t.Errorf("table cursor moved to %d while the picker was open", got)
	}
}
