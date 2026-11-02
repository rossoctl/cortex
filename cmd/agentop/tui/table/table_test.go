package table_test

import (
	"fmt"
	"math/rand"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
)

// Public API only, and the screen read off View(): the scroll position is what an
// operator sees, so that is what these assert. It also means the same tests run unchanged
// against upstream bubbles, which is how they were checked to catch the bug they guard.

var label = regexp.MustCompile(`r(\d{3})`)

func rows(n int) []table.Row {
	out := make([]table.Row, n)
	for i := range out {
		out[i] = table.Row{fmt.Sprintf("r%03d", i)}
	}
	return out
}

func newTable(n, height int) table.Model {
	t := table.New(
		table.WithColumns([]table.Column{{Title: "row", Width: 6}}),
		table.WithRows(rows(n)),
		table.WithFocused(true),
	)
	t.SetHeight(height + 1) // + the header line
	return t
}

// screen is the rows View() shows, in order.
func screen(t *testing.T, tb table.Model) []int {
	t.Helper()
	var out []int
	for _, l := range strings.Split(tb.View(), "\n")[1:] {
		if m := label.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[1])
			out = append(out, n)
		}
	}
	return out
}

// checkScreen asserts what must hold after any operation: the screen is full — a run of
// consecutive rows as long as the screen, or every row when there are fewer — and the
// cursor is on it.
func checkScreen(t *testing.T, tb table.Model, what string) (top int) {
	t.Helper()
	got := screen(t, tb)
	n, h := len(tb.Rows()), tb.Height()
	if want := min(n, h); len(got) != want {
		t.Fatalf("%s: screen shows %d rows %v, want %d", what, len(got), got, want)
	}
	if n == 0 {
		return 0
	}
	for i := 1; i < len(got); i++ {
		if got[i] != got[i-1]+1 {
			t.Fatalf("%s: screen rows are not consecutive: %v", what, got)
		}
	}
	if c := tb.Cursor(); c < got[0] || c > got[len(got)-1] {
		t.Fatalf("%s: cursor %d is off screen (rows %d..%d)", what, c, got[0], got[len(got)-1])
	}
	return got[0]
}

// TestMoveDown_ShortTableKeepsTheScreenAfterScrollingBack is the reported bug, at the
// table: 25 rows on a 20-line screen, the last row, eleven rows up, then down. Upstream
// scrolled the rows down a line and moved the highlight two on every press.
func TestMoveDown_ShortTableKeepsTheScreenAfterScrollingBack(t *testing.T) {
	tb := newTable(25, 20)
	tb.GotoBottom()
	for i := 0; i < 11; i++ {
		tb.MoveUp(1)
	}
	want := checkScreen(t, tb, "after scrolling back")
	for i := 0; i < 11; i++ {
		tb.MoveDown(1)
		if got := checkScreen(t, tb, fmt.Sprintf("down %d", i+1)); got != want {
			t.Fatalf("down %d scrolled the screen: top row %d, want %d", i+1, got, want)
		}
	}
}

// TestCursorMoves_ScrollOnlyAtTheEdges walks every key the table handles across tables
// shorter than one screen, between one and two, and longer, and pins the rule: the
// screen stays put while the cursor lands on it, and otherwise scrolls just far enough to
// put the cursor on the edge it crossed.
func TestCursorMoves_ScrollOnlyAtTheEdges(t *testing.T) {
	keys := []tea.KeyMsg{
		{Type: tea.KeyUp}, {Type: tea.KeyDown},
		{Type: tea.KeyPgUp}, {Type: tea.KeyPgDown},
		{Type: tea.KeyRunes, Runes: []rune("u")}, {Type: tea.KeyRunes, Runes: []rune("d")},
		{Type: tea.KeyHome}, {Type: tea.KeyEnd},
	}
	// Mostly single steps, which is what arrowing through a table is.
	weights := []int{6, 6, 1, 1, 1, 1, 1, 1}
	var pick []tea.KeyMsg
	for i, w := range weights {
		for j := 0; j < w; j++ {
			pick = append(pick, keys[i])
		}
	}
	rng := rand.New(rand.NewSource(1))
	const h = 10
	for _, n := range []int{0, 1, 5, 10, 11, 15, 19, 20, 21, 35, 100} {
		tb := newTable(n, h)
		top := checkScreen(t, tb, fmt.Sprintf("n=%d new", n))
		for step := 0; step < 400; step++ {
			k := pick[rng.Intn(len(pick))]
			tb, _ = tb.Update(k)
			what := fmt.Sprintf("n=%d step %d %q", n, step, k.String())
			got := checkScreen(t, tb, what)
			if n == 0 {
				continue
			}
			c := tb.Cursor()
			want := top
			switch {
			case c < top:
				want = c
			case c >= top+h:
				want = c - h + 1
			}
			if got != want {
				t.Fatalf("%s: cursor %d, screen top %d → %d, want %d", what, c, top, got, want)
			}
			top = got
		}
	}
}

// TestSetCursor_KeepsTheCursorOnScreen covers the call upstream got wrong for any row at
// or past one screenful: SetCursor re-windowed the rows without reconciling the offset,
// leaving the highlight one line below the last visible row. And a target already on
// screen must not scroll at all — the panes restore their cursor on every poll.
func TestSetCursor_KeepsTheCursorOnScreen(t *testing.T) {
	for _, target := range []int{0, 5, 9, 10, 19, 20, 33, 39, -3, 99} {
		tb := newTable(40, 10)
		tb.SetCursor(target)
		checkScreen(t, tb, fmt.Sprintf("SetCursor(%d)", target))
	}

	tb := newTable(40, 10)
	tb.GotoBottom()
	tb.MoveUp(4)
	before := checkScreen(t, tb, "scrolled back")
	tb.SetCursor(tb.Cursor())
	if got := checkScreen(t, tb, "restore in place"); got != before {
		t.Errorf("restoring the cursor in place scrolled: top %d → %d", before, got)
	}
}

// TestResizeAndRows_KeepTheCursorOnScreen covers the two changes that arrive without a
// keystroke: a resize, and the rows changing under the cursor (a filter, an arriving
// event). Each keeps the screen where it was when the cursor is still on it, and pulls
// the rows back down to fill it when the table shrank.
func TestResizeAndRows_KeepTheCursorOnScreen(t *testing.T) {
	for _, h := range []int{3, 7, 10, 25, 60} {
		tb := newTable(40, 10)
		tb.GotoBottom()
		tb.MoveUp(6)
		tb.SetHeight(h + 1)
		checkScreen(t, tb, fmt.Sprintf("height 10 → %d", h))
	}

	tb := newTable(40, 10)
	tb.GotoBottom()
	tb.MoveUp(3)
	before := checkScreen(t, tb, "scrolled back")
	tb.SetRows(rows(41))
	if got := checkScreen(t, tb, "a row appended"); got != before {
		t.Errorf("appending a row scrolled the screen: top %d → %d", before, got)
	}
	for _, n := range []int{30, 12, 5, 0} {
		tb.SetRows(rows(n))
		checkScreen(t, tb, fmt.Sprintf("rows shrank to %d", n))
	}
}

// A one-cell row renders as one line spanning every column — the section-row shape the
// events pane's subagent block headers use. A label longer than the first column's width
// must survive whole, which is the whole point of the convention: squeezed into column
// one it would read "▸ PR…".
func TestSectionRowSpansTheColumns(t *testing.T) {
	tb := table.New(
		table.WithColumns([]table.Column{
			{Title: "row", Width: 6},
			{Title: "col", Width: 40},
		}),
		table.WithRows([]table.Row{
			{"1", "a"},
			{"▸ PR review (execution-first) · 14 rows"},
			{"2", "b"},
		}),
		table.WithFocused(true),
	)
	tb.SetHeight(10)
	v := tb.View()
	if !strings.Contains(v, "▸ PR review (execution-first) · 14 rows") {
		t.Errorf("section row truncated or missing:\n%s", v)
	}
	// The cursor can land on it, and it renders selected rather than vanishing.
	tb.SetCursor(1)
	if v := tb.View(); !strings.Contains(v, "▸ PR review (execution-first) · 14 rows") {
		t.Errorf("section row missing under the cursor:\n%s", v)
	}
}

// A section row is searchable like any other row: a hit marks it, and one that does not
// match still renders while the search is live.
func TestSectionRowSearches(t *testing.T) {
	tb := table.New(
		table.WithColumns([]table.Column{{Title: "row", Width: 6}, {Title: "col", Width: 10}}),
		table.WithRows([]table.Row{
			{"1", "a"},
			{"▸ PR review (execution-first)"},
		}),
		table.WithFocused(true),
	)
	tb.SetHeight(10)
	tb.SetSearch("execution", []table.SearchRow{
		{Cells: []string{"", ""}},
		{Cells: []string{"▸ PR review (execution-first)"}},
	})
	if got := tb.Matches(); !slices.Equal(got, []int{1}) {
		t.Errorf("matches = %v, want [1]", got)
	}
}
