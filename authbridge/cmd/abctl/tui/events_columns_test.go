package tui

import (
	tea "github.com/charmbracelet/bubbletea"

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

// The popup must name every column, describe it, show a checkbox, and mark the
// cursor — a one-line list of twelve abbreviated headers asked the user to guess
// what DIR or METHOD meant.
func TestRenderColumnPicker(t *testing.T) {
	sel := defaultColumnSelection()
	out := stripANSI(renderColumnPicker(sel, 0, 160, 40))

	for _, c := range eventColumns {
		if !strings.Contains(out, string(c.id)) {
			t.Errorf("popup omits the %s column: %q", c.id, out)
		}
		if c.desc == "" {
			t.Errorf("column %s has no description", c.id)
		} else if !strings.Contains(out, c.desc) {
			t.Errorf("popup omits %s's description %q", c.id, c.desc)
		}
	}
	if !strings.Contains(out, "[x]") {
		t.Errorf("no checked box for a default-on column: %q", out)
	}
	// Cursor marked with a glyph, not only a colour, so it reads without colour.
	if !strings.Contains(out, "\u25b8") {
		t.Errorf("popup does not mark the cursor row: %q", out)
	}
	// Quit is advertised: the popup is modal, so the key a user reaches for has to
	// be listed.
	if !strings.Contains(out, "[q] quit") {
		t.Errorf("popup does not advertise quit: %q", out)
	}
}

// An unchecked column shows an empty box, so on and off are distinguishable.
func TestRenderColumnPicker_ShowsUncheckedBoxes(t *testing.T) {
	sel := defaultColumnSelection()
	sel[colHost] = false
	out := stripANSI(renderColumnPicker(sel, 0, 160, 40))
	if !strings.Contains(out, "[ ]") {
		t.Errorf("no empty box for a disabled column: %q", out)
	}
}

// A selected column that cannot fit is marked in place. Without it, ticking HOST in
// an 80-column window looks like the checkbox did nothing.
func TestRenderColumnPicker_MarksColumnsThatDoNotFit(t *testing.T) {
	narrow := stripANSI(renderColumnPicker(defaultColumnSelection(), 0, 80, 40))
	if !strings.Contains(narrow, "no room") {
		t.Errorf("narrow terminal does not flag unfittable columns: %q", narrow)
	}
	wide := stripANSI(renderColumnPicker(defaultColumnSelection(), 0, 200, 40))
	if strings.Contains(wide, "no room") {
		t.Errorf("wide terminal wrongly flags a column as unfittable: %q", wide)
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

// `q` must quit while the picker is open. A modal that traps the user until they
// find its exit is worse than one that honours the key they already reach for, and
// `q` means quit everywhere else in abctl.
func TestColumnPicker_QuitStaysLive(t *testing.T) {
	m := newTestEventsModel(t)
	m.handleKey(keyRune('c'))
	if !m.colPicker {
		t.Fatal("picker did not open")
	}
	// The general handler calls m.cancel, which a bare test model does not have.
	// Supplying it also proves the key reached that handler rather than being
	// swallowed by the picker's switch.
	cancelled := false
	m.cancel = func() { cancelled = true }

	cmd := m.handleKey(keyRune('q'))
	if !cancelled {
		t.Error("`q` was swallowed by the picker; it must reach the quit handler")
	}
	if cmd == nil {
		t.Error("`q` returned no command; expected tea.Quit")
	}
}

// esc closes the picker without quitting, so the two exits are distinguishable.
func TestColumnPicker_EscClosesWithoutQuitting(t *testing.T) {
	m := newTestEventsModel(t)
	m.handleKey(keyRune('c'))
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.colPicker {
		t.Error("esc did not close the picker")
	}
}

// Up and down move the cursor and stop at the ends rather than wrapping or
// indexing out of range.
func TestColumnPicker_CursorStaysInRange(t *testing.T) {
	m := newTestEventsModel(t)
	m.handleKey(keyRune('c'))

	for i := 0; i < len(eventColumns)*2; i++ {
		m.handleKey(keyRune('j'))
	}
	if m.colCursor != len(eventColumns)-1 {
		t.Errorf("cursor = %d after over-scrolling down, want %d", m.colCursor, len(eventColumns)-1)
	}
	for i := 0; i < len(eventColumns)*2; i++ {
		m.handleKey(keyRune('k'))
	}
	if m.colCursor != 0 {
		t.Errorf("cursor = %d after over-scrolling up, want 0", m.colCursor)
	}
}

// HOST is on by default and survives a narrow terminal. It is the column #866 was
// filed about: declared but never visible, because it is last in display order and
// nothing ranked it above the columns it competed with.
func TestDefaultColumns_HostIsOnAndSurvivesNarrowTerminals(t *testing.T) {
	sel := defaultColumnSelection()
	if !sel[colHost] {
		t.Error("HOST is not a default column")
	}
	for _, width := range []int{80, 100, 120, 140, 160} {
		fitted, _ := fitColumns(selectedColumns(sel), width)
		if !has(fitted, colHost) {
			t.Errorf("width %d: HOST dropped; got %v", width, colNames(fitted))
		}
	}
}
