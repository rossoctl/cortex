package table_test

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
)

var (
	red  = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff0000")).Bold(true)
	dim  = lipgloss.NewStyle().Foreground(lipgloss.Color("#555555"))
	alt  = lipgloss.NewStyle().Background(lipgloss.Color("#222222"))
	bar  = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff8800"))
	none = lipgloss.NewStyle()
)

const (
	redSGR = "38;2;255;0;0"
	dimSGR = "38;2;85;85;85"
	altSGR = "48;2;34;34;34"
)

// col1Red styles cell 1 of every row in red bold.
func col1Red(_ int, _ table.Row, col int) lipgloss.Style {
	if col == 1 {
		return red
	}
	return none
}

func styledTable(s table.Styles, rows []table.Row, widths ...int) table.Model {
	tb := twoColTable(rows, widths...)
	tb.SetStyles(s)
	return tb
}

func TestCellStyle_NilHookIsByteIdentical(t *testing.T) {
	s := searchStyles(t)
	tb := styledTable(s, []table.Row{{"r0", "aq"}, {"r1", "bq"}, {"r2", "a long value"}}, 4, 6)
	tb.SetSearch("q", nil)
	tb.SetCursor(1)
	want := tb.View()
	tb.SetCellStyle(nil)
	if got := tb.View(); got != want {
		t.Errorf("nil hook changed the output:\n got %q\nwant %q", got, want)
	}
	opt := table.New(table.WithColumns(tb.Columns()), table.WithRows(tb.Rows()), table.WithFocused(true),
		table.WithStyles(s), table.WithCellStyle(nil))
	opt.SetHeight(4)
	opt.SetSearch("q", nil)
	opt.SetCursor(1)
	if got := opt.View(); got != want {
		t.Errorf("WithCellStyle(nil) changed the output:\n got %q\nwant %q", got, want)
	}
}

func TestCellStyle_Precedence(t *testing.T) {
	s := searchStyles(t)
	tb := styledTable(s, []table.Row{{"r0", "aq"}, {"r1", "bq"}, {"r2", "cc"}, {"r3", "dd"}}, 4, 6)
	tb.SetCellStyle(col1Red)
	tb.UpdateViewport()

	// No search: the cell ink shows on a plain row and through the cursor row's background.
	if l := lineFor(t, tb, "r3"); !strings.Contains(l, red.Render("dd")) {
		t.Errorf("plain row: no red cell in %q", l)
	}
	if l := lineFor(t, tb, "r0"); !strings.Contains(l, s.Selected.Inherit(red).Render("aq")) {
		t.Errorf("cursor row: want selected background and red ink in %q", l)
	}

	// Search "q": a match draws over the cell ink; a marked row keeps its warn ink.
	tb.SetSearch("q", []table.SearchRow{{}, {}, {Dropped: []string{"q"}}, {}})
	if l := lineFor(t, tb, "r0"); !strings.Contains(l, s.Selected.Inherit(red).Render("a")+s.CurrentMatch.Inherit(s.Selected).Render("q")) {
		t.Errorf("cursor row match: %q", l)
	}
	if l := lineFor(t, tb, "r1"); !strings.Contains(l, red.Render("b")+s.Match.Render("q")) {
		t.Errorf("row match: %q", l)
	}
	l := lineFor(t, tb, "r2")
	if !strings.Contains(l, s.Marked.Inherit(red).Render("cc")) || strings.Contains(l, redSGR) {
		t.Errorf("marked row: want the Marked ink, not red, in %q", l)
	}
}

func TestCellStyle_StyledCellPadIsRowOnly(t *testing.T) {
	s := searchStyles(t)
	s.Alt = alt
	tb := styledTable(s, []table.Row{{"r0", "aq"}, {"r1", "bq"}}, 4, 6)
	tb.SetCellStyle(col1Red)
	tb.UpdateViewport()
	if l := lineFor(t, tb, "r0"); !strings.Contains(l, s.Selected.Inherit(red).Render("aq")+s.Selected.Render("    ")) {
		t.Errorf("cursor row: pad is not a Selected-only run in %q", l)
	}
	l := lineFor(t, tb, "r1")
	if !strings.Contains(l, strings.TrimSuffix(red.Render("bq"), "\x1b[0m")+"\x1b[22;39m    ") {
		t.Errorf("zebra row: no red cell with an unstyled pad after it in %q", l)
	}
	if off := offAlt(l); off != "" {
		t.Errorf("zebra row: %q is not on the Alt background in %q", off, l)
	}
}

// offAlt is the visible text of line that is drawn with no Alt background in effect.
func offAlt(line string) string {
	var state, off strings.Builder
	for len(line) > 0 {
		if strings.HasPrefix(line, "\x1b[") {
			q := strings.IndexByte(line, 'm')
			if p := line[2:q]; p == "0" {
				state.Reset()
			} else {
				state.WriteString(";" + p)
			}
			line = line[q+1:]
			continue
		}
		if !strings.Contains(state.String(), altSGR) {
			off.WriteByte(line[0])
		}
		line = line[1:]
	}
	return off.String()
}

// TestZebra_RowCostsOneBackground pins the zebra row's size: the plain row's bytes plus
// the Alt background after each reset, not a background on every nested cell run.
func TestZebra_RowCostsOneBackground(t *testing.T) {
	s := searchStyles(t)
	s.Alt = alt
	s.Glyphs = map[rune]lipgloss.Style{'↑': dim}
	rows := []table.Row{{"r0", "x"}, {"r1", "↑1kB"}, {"r1", "↑1kB"}}
	tb := styledTable(s, rows, 4, 6)
	tb.SetCellStyle(col1Red)
	tb.UpdateViewport()
	tb.SetCursor(0)
	lines := strings.Split(tb.View(), "\n")
	zebra, plain := lines[2], lines[3]
	if ansi.Strip(zebra) != ansi.Strip(plain) {
		t.Fatalf("rows differ: %q vs %q", zebra, plain)
	}
	if off := offAlt(zebra); off != "" {
		t.Errorf("zebra row: %q is not on the Alt background in %q", off, zebra)
	}
	bg := len(alt.Render("x")) - len("x") - len("\x1b[0m")
	if limit := len(plain) + (strings.Count(plain, "\x1b[0m")+1)*bg + len("\x1b[0m"); len(zebra) > limit {
		t.Errorf("zebra row is %d bytes, want at most %d (plain row %d):\n%q", len(zebra), limit, len(plain), zebra)
	}
}

func TestZebra_OddRowsOnly(t *testing.T) {
	s := searchStyles(t)
	s.Alt = alt
	tb := styledTable(s, []table.Row{{"r0", "x"}, {"r1", "x"}, {"r2", "x"}, {"r3", "x"}}, 4, 4)
	for tok, want := range map[string]bool{"r0": false, "r1": true, "r2": false, "r3": true} {
		if got := strings.Contains(lineFor(t, tb, tok), altSGR); got != want {
			t.Errorf("%s: Alt background = %v, want %v", tok, got, want)
		}
	}
	tb.SetCursor(1)
	tb.SetSearch("q", []table.SearchRow{{}, {}, {}, {Dropped: []string{"q"}}})
	for _, tok := range []string{"r1", "r3"} {
		if strings.Contains(lineFor(t, tb, tok), altSGR) {
			t.Errorf("%s (cursor or marked): has the Alt background", tok)
		}
	}

	sec := styledTable(s, []table.Row{{"r0", "x"}, {"section"}}, 4, 4)
	if !strings.Contains(lineFor(t, sec, "section"), altSGR) {
		t.Errorf("section row on index 1: no Alt background")
	}
}

func TestBar_ColorAndCursorOnly(t *testing.T) {
	s := searchStyles(t)
	s.Bar = bar
	rows := []table.Row{{"r0", "x"}, {"r1", "x"}, {"sec"}}
	tb := styledTable(s, rows, 4, 4)
	tb.SetSearch("r", nil) // the bar is never a match
	tb.SetCursor(1)
	plain := styledTable(searchStyles(t), rows, 4, 4)
	plain.SetSearch("r", nil)
	plain.SetCursor(1)
	lines, plainLines := strings.Split(tb.View(), "\n")[1:], strings.Split(plain.View(), "\n")[1:]
	for i, l := range lines {
		stripped := ansi.Strip(l)
		if lipgloss.Width(l) != lipgloss.Width(plainLines[i]) {
			t.Errorf("line %d: width %d, want %d", i, lipgloss.Width(l), lipgloss.Width(plainLines[i]))
		}
		if wantBar := i == 1; strings.HasPrefix(stripped, "▌") != wantBar {
			t.Errorf("line %d: bar = %v, want %v: %q", i, !wantBar, wantBar, stripped)
		}
	}
	if !strings.HasPrefix(lines[1], bar.Inherit(s.Selected).Render("▌")) {
		t.Errorf("cursor line: bar not drawn in Bar over Selected: %q", lines[1])
	}
	tb.SetCursor(2) // a section row gets the bar too
	if l := strings.Split(tb.View(), "\n")[3]; !strings.HasPrefix(ansi.Strip(l), "▌") {
		t.Errorf("section cursor row: no bar in %q", l)
	}

	lipgloss.SetColorProfile(termenv.Ascii)
	tb.SetCursor(1)
	if strings.Contains(tb.View(), "▌") {
		t.Errorf("Ascii: bar drawn:\n%s", tb.View())
	}
	lipgloss.SetColorProfile(termenv.TrueColor)
	s.Bar = lipgloss.Style{}
	tb.SetStyles(s)
	if strings.Contains(tb.View(), "▌") {
		t.Errorf("zero Bar: bar drawn:\n%s", tb.View())
	}
}

func TestGlyphs_OnlyInStyledCells(t *testing.T) {
	s := searchStyles(t)
	s.Glyphs = map[rune]lipgloss.Style{'↑': dim}
	tb := styledTable(s, []table.Row{{"s", "↑1kB"}, {"u", "↑1kB"}, {"c", "x"}}, 4, 6)
	tb.SetCursor(2)
	tb.SetCellStyle(func(r int, row table.Row, col int) lipgloss.Style {
		if col == 1 && row[0] == "s" {
			return red
		}
		return none
	})
	tb.UpdateViewport()
	if l := lineFor(t, tb, "s"); !strings.Contains(l, dim.Render("↑")+red.Render("1kB")) {
		t.Errorf("styled cell: want a dim ↑ and red text in %q", l)
	}
	if l := lineFor(t, tb, "u"); strings.Contains(l, dimSGR) {
		t.Errorf("unstyled cell: glyph drawn dim in %q", l)
	}
	// A match on a glyph rune wins over the glyph style.
	tb.SetSearch("↑", nil)
	l := lineFor(t, tb, "s")
	if !strings.Contains(l, s.Match.Render("↑")+red.Render("1kB")) || strings.Contains(l, dimSGR) {
		t.Errorf("match on a glyph: %q", l)
	}
}

func TestStyles_WidthUnchanged(t *testing.T) {
	for _, w := range []int{80, 100, 140} {
		cols := []table.Column{{Title: "A", Width: 10}, {Title: "B", Width: w - 30}, {Title: "C", Width: 12}}
		rows := []table.Row{
			{"r0", "↑1kB ↓2kB", "▕██▏"},
			{"r1", strings.Repeat("long ", w), "x"},
			{"r2", "↑3kB", "y"},
			{"section row"},
		}
		build := func(s table.Styles, hook table.CellStyleFunc) table.Model {
			tb := table.New(table.WithColumns(cols), table.WithRows(rows), table.WithFocused(true),
				table.WithStyles(s), table.WithCellStyle(hook))
			tb.SetHeight(len(rows) + 1)
			tb.SetSearch("↑", nil)
			return tb
		}
		base := strings.Split(build(searchStyles(t), nil).View(), "\n")
		s := searchStyles(t)
		s.Alt, s.Bar = alt, bar
		s.Glyphs = map[rune]lipgloss.Style{'↑': dim, '↓': dim, '▕': dim, '▏': dim}
		all := func(int, table.Row, int) lipgloss.Style { return red }
		got := strings.Split(build(s, all).View(), "\n")
		if len(got) != len(base) {
			t.Fatalf("w=%d: %d lines, want %d", w, len(got), len(base))
		}
		for i := range got {
			if lipgloss.Width(got[i]) != lipgloss.Width(base[i]) {
				t.Errorf("w=%d line %d: width %d, want %d", w, i, lipgloss.Width(got[i]), lipgloss.Width(base[i]))
			}
		}
		// The "…" of a cut cell takes the cell style.
		cut := ansi.Truncate(rows[1][1], w-30, "…")
		if l := lineFor(t, build(s, all), "r1"); !strings.Contains(l, cut) || !strings.Contains(l, redSGR) || offAlt(l) != "" {
			t.Errorf("w=%d: the cut cell is not drawn in the cell style: %q", w, l)
		}
	}
}
