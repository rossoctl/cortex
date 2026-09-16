package tui

import (
	"testing"

	"github.com/charmbracelet/bubbles/table"
)

// What fitTableColumns actually does, pinned.
//
// The sessions pane's godoc claimed for two commits that this function "drops trailing
// columns rather than letting the renderer clip a figure mid-digits". It does the opposite:
// it shrinks the widest column one cell at a time and never drops one, so a narrow terminal
// gets six narrow columns rather than four wide ones. That inversion is not cosmetic —
// downstream code chose blank cells over elided ones on the strength of it, and a reader
// reasoning about a positional table.Row needs to know the column COUNT is invariant.
//
// Nothing tested either half. Both are asserted here so the correction cannot rot.
func TestFitTableColumns_ShrinksTheWidestAndNeverDropsAColumn(t *testing.T) {
	cols := []table.Column{
		{Title: "WIDE", Width: 40},
		{Title: "MID", Width: 14},
		{Title: "NARROW", Width: 8},
	}
	// tableWidth(cols) is 40+14+8 + 3×cellPadding = 68. Ask for 60 and 8 columns have to go.
	fitted := fitTableColumns(cols, 60)

	if len(fitted) != len(cols) {
		t.Fatalf("fitTableColumns returned %d of %d columns; it must SHRINK, never drop — a dropped column shifts every positional table.Row reader", len(fitted), len(cols))
	}
	for i, c := range fitted {
		if c.Title != cols[i].Title {
			t.Fatalf("column %d is %q, want %q; the order is part of the contract", i, c.Title, cols[i].Title)
		}
	}
	if got := tableWidth(fitted); got > 60 {
		t.Errorf("fitted table is %d columns wide, want at most 60", got)
	}
	// The space came out of WIDE, which had the most to give. Spreading it evenly would
	// take cells from NARROW, which is already tight.
	if fitted[2].Width != cols[2].Width {
		t.Errorf("NARROW shrank from %d to %d; the widest column pays first", cols[2].Width, fitted[2].Width)
	}
	if fitted[0].Width >= cols[0].Width {
		t.Errorf("WIDE is still %d wide; it is the column that should have paid", fitted[0].Width)
	}

	// A copy, never the caller's slice: these come from package-level constructors, so
	// mutating in place would make the fit permanent and cumulative across resizes.
	if cols[0].Width != 40 {
		t.Errorf("fitTableColumns mutated the caller's slice (WIDE is now %d); a resize would be cumulative", cols[0].Width)
	}
}

// Below minColumnWidth the fitter stops rather than lie about its content. A table that
// overflows visibly beats one whose every cell is an ellipsis, and stopping is what bounds
// the loop.
func TestFitTableColumns_StopsAtTheFloorRatherThanEmptyingColumns(t *testing.T) {
	fitted := fitTableColumns(sessionsColumns(), 1)
	if len(fitted) != len(sessionsColumns()) {
		t.Fatalf("fitTableColumns dropped columns at width 1: %v", fitted)
	}
	for _, c := range fitted {
		if c.Width < minColumnWidth {
			t.Errorf("column %q shrank to %d, below the %d floor", c.Title, c.Width, minColumnWidth)
		}
	}
}
