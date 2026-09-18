package tui

import (
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/session"
)

// sessionTotals are the values this column has to render, including the two from the
// report and the boundaries where formatCompact changes tier.
var sessionTotals = []int{
	0, 1, 999, 1000, 9_999, 100_000, 999_949, 999_950,
	1_000_000, 9_999_999, 30_661_090,
	137_156_234, // the one that truncated
	547_853_512, // the largest real one seen
	999_949_999, 999_950_000, 1_000_000_000, 9_999_999_999,
}

// Every plausible session total has to fit the cell it is rendered into, at every width
// the pane is actually laid out at — not only the declared one.
//
// The declared width is 10, but fitTableColumns squeezes columns toward minColumnWidth
// on a narrow terminal, and TOKENS is one it squeezes: 10 at 60 columns, 8 at 50, 7 at
// 46, 6 at 40, 5 at 36. So building the table from newSessionsTable() and asserting
// against 10 — which the first version of this test did — pins the reported case and
// nothing narrower, while the argument for compacting rather than widening the column is
// precisely that the fit budget matters.
//
// 44 columns is the floor this can promise: formatCompact's widest output is six runes
// ("999.9M"), so a cell of six or more always holds it. Below that the cell is five and
// the value cannot fit however it is formatted — recorded by the last case rather than
// left as a surprise.
//
// That floor was 40 until the TITLE column was added. It is not the formatter that moved:
// one more column costs its own width plus bubbles' two of padding out of the same budget,
// so TOKENS reaches six four columns later than it used to. Between 40 and 43 a total now
// truncates where it once fitted — the accepted price of naming the rows, at widths where
// the id itself is already down to four or five characters.
func TestSessionTokens_FitsEveryFittedWidth(t *testing.T) {
	for _, term := range []int{200, 90, 60, 50, 46, 44} {
		width := 0
		for _, c := range fitTableColumns(sessionsColumns(), term) {
			if c.Title == "TOKENS" {
				width = c.Width
			}
		}
		if width == 0 {
			t.Fatalf("term %d: no TOKENS column after fitting", term)
		}
		for _, total := range sessionTotals {
			got := sessionTokens(total, nil)
			if n := len([]rune(got)); n > width {
				t.Errorf("term %d (TOKENS=%d): total %d renders as %q (%d runes) — truncates",
					term, width, total, got, n)
			}
		}
	}
}

// The floor, stated rather than discovered: at 43 columns the cell is five wide and
// formatCompact's widest output is six, so it truncates. That is the fit budget running
// out, not the formatter — worth a test so a future change to either is measured against
// it instead of assumed.
//
// 43 rather than 36 because adding a column moved the floor; see the note on
// TestSessionTokens_FitsEveryFittedWidth. Asserting at the width just below the floor,
// rather than at a fixed narrow number, is what makes this test track the boundary instead
// of a point far below it.
func TestSessionTokens_BelowTheFloorTheCellIsTooNarrow(t *testing.T) {
	width := 0
	for _, c := range fitTableColumns(sessionsColumns(), 43) {
		if c.Title == "TOKENS" {
			width = c.Width
		}
	}
	if width != 5 {
		t.Fatalf("TOKENS fitted to %d at 36 columns, want 5 — the floor moved", width)
	}
	if n := len([]rune(sessionTokens(547_853_512, nil))); n <= width {
		t.Errorf("547.9M now fits a %d-wide cell in %d runes; the comment above is stale", width, n)
	}
}

// The compact form still distinguishes the magnitudes an operator is reading the column
// for. A formatter that rendered everything as "1.0M" would fit and say nothing.
func TestSessionTokens_DistinguishesMagnitudes(t *testing.T) {
	for _, tc := range []struct {
		total int
		want  string
	}{
		{0, "—"},
		{842, "842"},
		{4_385_706, "4.4M"},
		{30_661_090, "30.7M"},
		{137_156_234, "137.2M"},
		{547_853_512, "547.9M"},
	} {
		if got := sessionTokens(tc.total, nil); got != tc.want {
			t.Errorf("sessionTokens(%d) = %q, want %q", tc.total, got, tc.want)
		}
	}
}

// And the rendered row agrees with the cell, so a future width change to the column or a
// change in how rows are built cannot reintroduce the truncation.
func TestSessionsPane_TokensCellIsNotTruncatedInTheRow(t *testing.T) {
	m := &model{pane: paneSessions, width: 200}
	m.sessionsTbl = newSessionsTable()
	m.sessionsTbl.SetHeight(6)
	m.sessions = []session.SessionSummary{{ID: "s1", EventCount: 5289, TotalTokens: 547_853_512}}
	m.rebuildSessionsTable()

	rows := m.sessionsTbl.Rows()
	if len(rows) != 1 {
		t.Fatalf("built %d rows, want 1", len(rows))
	}
	// By title, not by index: rows are built positionally, so a hardcoded 3 keeps
	// asserting after a column is inserted ahead of TOKENS — on the wrong cell, quietly.
	col := -1
	for i, c := range m.sessionsTbl.Columns() {
		if c.Title == "TOKENS" {
			col = i
		}
	}
	if col < 0 {
		t.Fatalf("no TOKENS column in %v", m.sessionsTbl.Columns())
	}
	cell := strings.TrimSpace(rows[0][col])
	if cell != "547.9M" {
		t.Errorf("TOKENS cell = %q, want %q", cell, "547.9M")
	}
	if strings.Contains(m.sessionsTbl.View(), "…") {
		t.Errorf("the rendered pane truncates a cell:\n%s", m.sessionsTbl.View())
	}
}
