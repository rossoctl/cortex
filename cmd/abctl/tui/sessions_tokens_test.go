package tui

import (
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/session"
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
// on a narrow terminal, and TOKENS is one it squeezes. So building the table from
// newSessionsTable() and asserting against 10 — which the first version of this test did —
// pins the reported case and nothing narrower, while the argument for compacting rather
// than widening the column is precisely that the fit budget matters.
//
// sessionsColumnsFor, not sessionsColumns: the set actually laid out is width-dependent
// since COST and SAVED arrived. Two more columns cost every other column width, and at 50
// they took this one to 5 runes and truncated "100.0k" — so they are dropped outright below
// the width that can hold them, and this test walks the set the pane really gets.
//
// 40 columns is the floor this can promise: formatCompact's widest output is six runes
// ("999.9M"), so a cell of six or more always holds it. Below that the cell is five and the
// value cannot fit however it is formatted — recorded by the companion test below rather than
// left as a surprise.
//
// THE FLOOR DID NOT MOVE WHEN TITLE ARRIVED, and an earlier revision of this branch claimed
// it had — raising the narrowest case to 44 and deleting 40 and 42 on that reasoning. TITLE
// is dropped entirely below terminal width 73 (see sessionsShowTitle), so at 40 and 42 the
// column set is byte-identical to the one this test was written against. Measured: TOKENS is
// 6 at 40 and 7 at 42, unchanged. Those two were also the ONLY cases exercising a six-column
// cell, which is the boundary the paragraph above says this test exists to pin, so removing
// them dropped the coverage while appearing to update it.
func TestSessionTokens_FitsEveryFittedWidth(t *testing.T) {
	// 80, 81 and 82 are in the sweep because they are the widths this layout changes — where
	// TITLE arrives and the money columns yield — and skipping them is what let an unenforced
	// TITLE floor go unnoticed there.
	for _, term := range []int{200, 90, 82, 81, 80, 60, 50, 46, 42, 40} {
		width := 0
		for _, c := range fitTableColumns(sessionsColumnsFor(term), term) {
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

// The floor, stated rather than discovered: below 37 columns the cell is five wide and
// formatCompact's widest output is six, so it truncates. That is the fit budget running out,
// not the formatter — worth a test so a future change to either is measured against it instead
// of assumed.
//
// Measured: five columns between 32 and 36, six at 37, seven by 43. An earlier revision of this
// comment named 43 as the five-column width while the body below kept using the right one, so
// the prose and the assertion disagreed.
//
// Through sessionsColumnsFor, so the floor is the one the PANE has. 36 columns is far below
// the width that affords COST and SAVED, so they are dropped — and TITLE does not grow, since
// there is no slack at 36 — leaving the same budget this floor was measured against.
func TestSessionTokens_BelowFortyColumnsTheCellIsTooNarrow(t *testing.T) {
	width := 0
	for _, c := range fitTableColumns(sessionsColumnsFor(36), 36) {
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
	// Through headerTitle, since TOKENS right-aligns its heading and so arrives padded.
	col := -1
	for i, c := range m.sessionsTbl.Columns() {
		if headerTitle(c) == "TOKENS" {
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
