package table

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// A section row is as wide as a data row, so its highlight under the cursor ends at the
// same right edge as a data row's. The viewport pads every line to its width in plain
// spaces, so only the row's own render shows the difference. The widths are lipgloss's, as
// the terminal counts them: go-runewidth counts a VS16 emoji as 1 column, and, in a CJK
// locale, counts the header's ▸ ─ · as 2 columns each.
func TestSectionRowIsAsWideAsADataRow(t *testing.T) {
	prev := runewidth.DefaultCondition.EastAsianWidth
	t.Cleanup(func() { runewidth.DefaultCondition.EastAsianWidth = prev })
	for _, eastAsian := range []bool{false, true} {
		runewidth.DefaultCondition.EastAsianWidth = eastAsian
		tb := New(
			WithColumns([]Column{{Title: "a", Width: 4}, {Title: "b", Width: 6}, {Title: "c", Width: 8}}),
			WithRows([]Row{{"1", "x", "y"}, {"▸─ subagent · 2 msgs"}, {"2", "tool row"}, {"▸─ check ✔️ done ❤️ x"}}),
			WithFocused(true),
		)
		data := lipgloss.Width(tb.renderRow(0))
		for _, r := range []int{1, 2, 3} {
			if got := lipgloss.Width(tb.renderRow(r)); got != data {
				t.Errorf("EastAsianWidth=%v: row %d is %d columns wide, a data row %d", eastAsian, r, got, data)
			}
		}
	}
}

// A section row in a table with no columns renders without a panic: it does not index a
// column that is not there.
func TestSectionRowWithNoColumns(t *testing.T) {
	tb := New(WithRows([]Row{{"▸─ subagent"}}))
	_ = tb.renderRow(0)
}
