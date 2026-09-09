package tui

import (
	"github.com/charmbracelet/bubbles/table"

	"github.com/charmbracelet/lipgloss"

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

// A high-ranked column must outlive a low-ranked one. Ranking by "is it a default"
// instead made every default equally expendable, so HOST — last in display order —
// was the first thing dropped, which is the failure #866 describes.
func TestFitColumns_KeepRankDecidesWhatSurvives(t *testing.T) {
	fitted, dropped := fitColumns(selectedColumns(defaultColumnSelection()), 90)
	if dropped == 0 {
		t.Fatal("nothing dropped at 90 columns; the ranking is untested")
	}

	for _, c := range fitted {
		if c.keep != keepLow {
			continue
		}
		// A keepLow column survived, so no keepHigh one may have been dropped in
		// its place.
		for _, want := range eventColumns {
			if want.keep == keepHigh && !has(fitted, want.id) {
				t.Errorf("dropped %s (keepHigh) while keeping %s (keepLow); got %v",
					want.id, c.id, colNames(fitted))
			}
		}
	}
	// Concretely: the columns the ranks exist to protect.
	for _, id := range []eventColumnID{colIndex, colHost} {
		if !has(fitted, id) {
			t.Errorf("%s is keepHigh but was dropped; got %v", id, colNames(fitted))
		}
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
	// Toggle every column off, moving DOWN each time.
	//
	// `j`, not `l`: the picker binds up/k and down/j, and `l` falls to the switch's
	// default. With `l` the cursor never moved, so all twelve space presses toggled
	// the same column — an even count that ended back at all-on, and both assertions
	// below passed without the picker ever having emptied the selection.
	for i := 0; i < len(eventColumns); i++ {
		m.handleKey(keyRune(' '))
		m.handleKey(keyRune('j'))
	}
	// The final toggle empties the selection, and the handler snaps it back to the
	// defaults so the checkboxes keep matching the table. Before that snap-back the
	// popup drew twelve empty boxes over a table showing twelve default columns.
	if !anyColumnSelected(m.eventColumns) {
		t.Fatal("emptying the selection left every checkbox off; the table shows defaults, so the picker misreports it")
	}
	if got, want := len(selectedColumns(m.eventColumns)), len(selectedColumns(defaultColumnSelection())); got != want {
		t.Errorf("after emptying: %d columns selected, want the %d defaults", got, want)
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

// TestColumnPicker_UDoesNotEscapeModality is the first blocker from the #915
// review. The `u`-opens-Usage handler sits ABOVE the picker block, so `u` reached
// openUsage while m.colPicker stayed true: View() then drew the picker popup over
// the Usage pane, paneUsage's own m/w/b/s bindings went live underneath it, and
// `esc` closed the picker onto Usage rather than the events timeline the user
// opened it from.
func TestColumnPicker_UDoesNotEscapeModality(t *testing.T) {
	m := newTestEventsModel(t)
	m.handleKey(keyRune('c'))
	if !m.colPicker {
		t.Fatal("picker did not open")
	}

	m.handleKey(keyRune('u'))

	if m.pane != paneEvents {
		t.Errorf("pane switched to %v underneath the open picker; the picker owns the keyboard", m.pane)
	}
	if !m.colPicker {
		t.Error("picker closed on `u`; it should have been ignored")
	}
}

// The picker popup must bound itself to the terminal. overlayCenter is explicit
// that this is the caller's job: it renders an over-wide panel flush-left and
// drops rows past the bottom edge, so an unbounded panel loses content silently.
func TestColumnPicker_FitsTheTerminal(t *testing.T) {
	// Every column on, so the "(no room)" markers are live too — the widest state.
	sel := map[eventColumnID]bool{}
	for _, c := range eventColumns {
		sel[c.id] = true
	}
	for _, w := range []int{60, 80, 100, 200} {
		for _, c := range []int{0, len(eventColumns) - 1} {
			out := renderColumnPicker(sel, c, w, 40)
			for _, ln := range strings.Split(out, "\n") {
				if n := lipgloss.Width(ln); n > w {
					t.Errorf("width %d, cursor %d: a row is %d columns wide:\n%s", w, c, n, ln)
					break
				}
			}
		}
	}
}

// The bottom row is the key hints, and overlayCenter drops rows past the bottom
// edge. Before the height guard, a 12-line terminal lost "[esc] close" entirely —
// the keys a stuck user reaches for, which is the same thing fitHintLine exists to
// protect in the footer.
func TestColumnPicker_HintsSurviveAShortTerminal(t *testing.T) {
	sel := defaultColumnSelection()
	base := strings.Repeat("row\n", 11)
	for _, h := range []int{40, 24, 18, 14, 12, 10, 8} {
		panel := renderColumnPicker(sel, 0, 100, h)
		if got := len(strings.Split(panel, "\n")); got > h {
			t.Errorf("height %d: panel is %d lines, so overlayCenter will clip it", h, got)
		}
		out := overlayCenter(base, panel, 100, h)
		if !strings.Contains(out, "close") {
			t.Errorf("height %d: the close hint was dropped:\n%s", h, out)
		}
	}
}

// Clipping the list must not hide the cursor: space would then toggle a column the
// user cannot see.
func TestColumnPicker_CursorStaysVisibleWhenClipped(t *testing.T) {
	sel := defaultColumnSelection()
	// 12 lines leaves room for only a few column rows.
	last := len(eventColumns) - 1
	out := renderColumnPicker(sel, last, 100, 12)
	if !strings.Contains(out, string(eventColumns[last].id)) {
		t.Errorf("cursor row %q is not rendered in a clipped popup:\n%s", eventColumns[last].id, out)
	}
	if !strings.Contains(out, "terminal too short") {
		t.Errorf("a clipped list should say so:\n%s", out)
	}
}

// The invariant the whole feature rests on: what fitColumns returns must actually
// render inside the terminal width. Asserted against a real bubbles table, because
// the bug this replaces was a mismodelled padding constant — every unit test that
// checked only fitColumns' return value passed while the rendered row overflowed.
//
// bubbles pads each cell on both sides (Padding(0, 1) on Cell and Header), so a
// column occupies width+2. Modelling it as +1 under-counted by one per column: a
// row of all twelve rendered at 168 against a computed 156, so the table wrapped
// at 80 and — worse, between 156 and 167 — reported dropped==0 while up to twelve
// columns sat off the edge, with no footer count and no "(no room)" marker.
func TestFitColumns_RenderedWidthNeverExceedsTerminal(t *testing.T) {
	// Several selections, not just all-on. The all-on case alone let a real bug
	// through: fitColumns credited back width+1 while columnsWidth charged
	// width+cellPadding, and the greedy loop never reconsidered an overshoot — at 80
	// it dropped seven columns and left 19 unused. Neither shows up if the only
	// input is "everything on", because the invariant checked (rendered <= width)
	// stays true when too MUCH is dropped.
	for _, tc := range columnSelectionCases() {
		t.Run(tc.name, func(t *testing.T) {
			cols := selectedColumns(tc.sel)
			for w := 20; w <= 180; w++ {
				fitted, dropped := fitColumns(cols, w)
				if len(fitted) == 0 {
					t.Fatalf("width %d: no columns returned", w)
				}

				tbl := newEventsTable()
				tbl.SetColumns(tableColumns(fitted))
				row := make([]string, len(fitted))
				for i := range row {
					row[i] = "x"
				}
				tbl.SetRows([]table.Row{row})

				rendered := 0
				for _, ln := range strings.Split(tbl.View(), "\n") {
					if n := lipgloss.Width(ln); n > rendered {
						rendered = n
					}
				}

				// The one legitimate exception: a terminal too narrow for even one
				// column. fitColumns never returns empty, so a single column may exceed.
				if len(fitted) == 1 && rendered > w {
					continue
				}
				if rendered > w {
					t.Errorf("width %d: %d columns render at %d (%d dropped) — overflows by %d",
						w, len(fitted), rendered, dropped, rendered-w)
				}
				if got := columnsWidth(fitted); got != rendered {
					t.Errorf("width %d: columnsWidth says %d, bubbles renders %d", w, got, rendered)
				}
			}
		})
	}
}

// The other half of "fits": it must not drop MORE than it has to. The
// rendered-width check above is satisfied by dropping everything, so without this
// an over-eager fitColumns looks correct.
func TestFitColumns_DropsNoMoreThanNecessary(t *testing.T) {
	for _, tc := range columnSelectionCases() {
		t.Run(tc.name, func(t *testing.T) {
			cols := selectedColumns(tc.sel)
			for w := 20; w <= 180; w++ {
				fitted, _ := fitColumns(cols, w)
				used := columnsWidth(fitted)
				if used > w {
					continue // covered by the invariant test above
				}
				in := make(map[eventColumnID]bool, len(fitted))
				for _, c := range fitted {
					in[c.id] = true
				}
				// Any dropped column that would still fit in the leftover room means the
				// drop was unnecessary — the user lost a column for nothing.
				for _, c := range cols {
					if in[c.id] {
						continue
					}
					if used+c.width+cellPadding <= w {
						t.Errorf("width %d: dropped %s (needs %d) with %d columns unused",
							w, c.id, c.width+cellPadding, w-used)
					}
				}
			}
		})
	}
}

// columnSelectionCases spans the selection shapes the picker can produce: all on,
// the defaults, and several partial sets chosen to vary which keep-ranks and which
// widths are present.
func columnSelectionCases() []struct {
	name string
	sel  map[eventColumnID]bool
} {
	all := map[eventColumnID]bool{}
	for _, c := range eventColumns {
		all[c.id] = true
	}
	// Only the widest columns, so a single drop swings the total a long way — the
	// shape that made the greedy loop overshoot.
	wide := map[eventColumnID]bool{}
	for _, c := range eventColumns {
		if c.width >= 17 {
			wide[c.id] = true
		}
	}
	// Only narrow ones, where many drops are needed to move the total at all.
	narrow := map[eventColumnID]bool{}
	for _, c := range eventColumns {
		if c.width <= 10 {
			narrow[c.id] = true
		}
	}
	// Alternating, to mix ranks and widths.
	alt := map[eventColumnID]bool{}
	for i, c := range eventColumns {
		if i%2 == 0 {
			alt[c.id] = true
		}
	}
	// A single column: the degenerate case fitColumns must not return empty for.
	one := map[eventColumnID]bool{eventColumns[len(eventColumns)-1].id: true}

	return []struct {
		name string
		sel  map[eventColumnID]bool
	}{
		{"all-on", all},
		{"defaults", defaultColumnSelection()},
		{"wide-only", wide},
		{"narrow-only", narrow},
		{"alternating", alt},
		{"single", one},
	}
}

// Nothing bounds the table downstream — SetWidth is never called and paneView
// applies no MaxWidth — so dropped==0 has to mean the whole selection really fits.
func TestFitColumns_ZeroDroppedMeansItFits(t *testing.T) {
	sel := map[eventColumnID]bool{}
	for _, c := range eventColumns {
		sel[c.id] = true
	}
	all := selectedColumns(sel)
	full := columnsWidth(all)

	for w := full - 20; w <= full+4; w++ {
		fitted, dropped := fitColumns(all, w)
		if dropped != 0 {
			continue
		}
		if len(fitted) != len(all) {
			t.Errorf("width %d: dropped==0 but only %d of %d columns returned",
				w, len(fitted), len(all))
		}
		if columnsWidth(fitted) > w {
			t.Errorf("width %d: dropped==0 while the table needs %d — silently off the edge",
				w, columnsWidth(fitted))
		}
	}
}

// The picker must not own the keyboard, or draw itself, over a pane it does not
// belong to. No KEY can switch panes underneath it, but a MESSAGE can: the
// sessionsMsg handler drops to paneSessions when the selected session disappears
// server-side, which left the popup drawn over the sessions table with its key
// block swallowing enter/j/k — an inert pane until the user guessed esc.
func TestColumnPicker_DoesNotStrandOnAnAsyncPaneChange(t *testing.T) {
	m := newTestEventsModel(t)
	m.handleKey(keyRune('c'))
	if !m.colPicker {
		t.Fatal("picker did not open")
	}

	// The server no longer reports the session being read: same shape as
	// app.go's sessionsMsg handler.
	m.selectedSess = ""
	m.pane = paneSessions

	// The picker's block must not claim these any more.
	if cmd := m.handleKey(keyRune('j')); cmd != nil {
		t.Log("j returned a command, which is fine — what matters is the pane below")
	}
	if m.colCursor != 0 {
		t.Errorf("picker moved its cursor while the sessions pane was active (colCursor=%d)", m.colCursor)
	}

	// And it must not paint over the sessions pane.
	if strings.Contains(m.paneView(), "COLUMNS") {
		t.Error("the column picker popup was drawn over the sessions pane")
	}
}
