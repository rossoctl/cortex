package table

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// A section row is as wide as a data row, so its highlight under the cursor ends at the
// same right edge as a data row's. The viewport pads every line to its width in plain
// spaces, so only the row's own render shows the difference.
func TestSectionRowIsAsWideAsADataRow(t *testing.T) {
	tb := New(
		WithColumns([]Column{{Title: "a", Width: 4}, {Title: "b", Width: 6}, {Title: "c", Width: 8}}),
		WithRows([]Row{{"1", "x", "y"}, {"▸─ subagent · 2 msgs"}, {"2", "tool row"}}),
		WithFocused(true),
	)
	data := lipgloss.Width(tb.renderRow(0))
	for _, r := range []int{1, 2} {
		if got := lipgloss.Width(tb.renderRow(r)); got != data {
			t.Errorf("row %d is %d columns wide, a data row %d", r, got, data)
		}
	}
}
