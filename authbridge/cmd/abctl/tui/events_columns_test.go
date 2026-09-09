package tui

import (
	"strings"
	"testing"
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
