package tui

import (
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/rossoctl/cortex/cmd/agentop/tui/find"
)

// searchDoc is a text view's searchable content: the lines the pane drew, colored and not yet
// wrapped, and the drawn rows a search's matches start on.
//
// MATCHED BEFORE THE WRAP, so a match the wrap splits is still found and the terminal's width
// never changes what matches. But n and N move by DRAWN ROW, not by line: one JSON string
// value is one line, and a prompt or a completion wraps to dozens of rows, so moving to where
// the line starts could leave the match off screen. Each match is therefore placed on the row
// it starts on, which takes finding where each wrapped row begins (rowStarts).
//
// The highlight goes on before the wrap as well. Escape sequences are zero-width, so the
// colored, highlighted line wraps at the same places as its plain text, and the rows counted
// from the plain text are the rows drawn.
type searchDoc struct {
	lines []string
	wrap  int
	// matches is the drawn rows a match starts on, ascending, from the last render.
	matches []int
	// cur is the row n or N last moved to; hasCur says there is one. A bool rather than -1,
	// so the zero value — which test models and the second constructor both start from — is
	// a document with no current row.
	cur    int
	hasCur bool
	// gutter draws a two-column cursor gutter left of every row, with line cursor's rows
	// marked. The event detail pane sets it; the plugin detail and help panes do not.
	gutter bool
	cursor int
	// firsts is the drawn row each line starts on, from the last render.
	firsts   []int
	lastRows int
}

// gutterWidth is the columns the cursor gutter takes from the wrap width.
const gutterWidth = 2

// setLines replaces the text. The current row is kept only when the text and the wrap width
// are what they were: layout() redraws the detail on every prompt open and close, and that is
// the same document, while a new message, a body that just arrived, or a resize is not.
func (d *searchDoc) setLines(lines []string, wrap int) {
	if wrap != d.wrap || !slices.Equal(lines, d.lines) {
		d.hasCur = false
	}
	d.lines, d.wrap = lines, wrap
}

func (d *searchDoc) setCurrent(row int)      { d.cur, d.hasCur = row, true }
func (d *searchDoc) clearCurrent()           { d.hasCur = false }
func (d *searchDoc) currentRow() (int, bool) { return d.cur, d.hasCur }

// render draws the document with q's matches highlighted — the current row's in
// styleCurrentMatch — wrapped to d.wrap (0: as drawn), and records the rows they start on.
func (d *searchDoc) render(q string) string {
	d.matches, d.firsts = nil, d.firsts[:0]
	wrap := d.wrap
	if d.gutter && wrap > 0 {
		wrap = max(wrap-gutterWidth, 1)
	}
	var b strings.Builder
	row := 0
	for i, line := range d.lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		d.firsts = append(d.firsts, row)
		plain := ansi.Strip(line)
		ranges := find.Ranges(plain, q)
		starts := []int{0}
		if wrap > 0 && len(ranges) > 0 {
			starts = rowStarts(plain, strings.Split(ansi.Wrap(plain, wrap, " -"), "\n"))
		}
		spans := make([]find.Styled, len(ranges))
		for k, r := range ranges {
			at := row + startRowOf(starts, r.Start)
			if n := len(d.matches); n == 0 || d.matches[n-1] != at {
				d.matches = append(d.matches, at)
			}
			style := styleMatch
			if d.hasCur && at == d.cur {
				style = styleCurrentMatch
			}
			spans[k] = find.Styled{Range: r, Style: style}
		}
		out := find.Highlight(line, spans)
		if wrap > 0 {
			out = closeRows(ansi.Wrap(out, wrap, " -"))
		}
		if d.gutter {
			mark := "  "
			if i == d.cursor {
				mark = styleDetailCursor.Render("▌") + " "
			}
			out = mark + strings.ReplaceAll(out, "\n", "\n"+mark)
		}
		b.WriteString(out)
		row += strings.Count(out, "\n") + 1
	}
	d.lastRows = row
	return b.String()
}

// closeRows makes each row of wrapped draw in its own colors. ansi.Wrap breaks a colored line
// but does not close its color at the break, so the next row drew in whatever color the
// terminal had: none after the cursor gutter's reset, and the previous pane line's at the top
// of a scrolled viewport. Each row now ends with a reset and starts with the SGR sequences
// that were in force where the row before it broke.
func closeRows(wrapped string) string {
	rows := strings.Split(wrapped, "\n")
	if len(rows) == 1 {
		return wrapped
	}
	active := ""
	for i, r := range rows {
		open := active
		active = activeSGR(active, r)
		if i < len(rows)-1 && active != "" {
			r += ansi.ResetStyle
		}
		rows[i] = open + r
	}
	return strings.Join(rows, "\n")
}

// activeSGR is the SGR sequences in force after row, given those in force before it: each
// reset clears them, and every other SGR sequence adds to them.
func activeSGR(active, row string) string {
	for {
		i := strings.Index(row, "\x1b[")
		if i < 0 {
			return active
		}
		row = row[i+2:]
		j := strings.IndexFunc(row, func(r rune) bool { return r >= 0x40 && r <= 0x7e })
		if j < 0 {
			return active
		}
		if row[j] == 'm' {
			if params := row[:j]; params == "" || params == "0" {
				active = ""
			} else {
				active += "\x1b[" + params + "m"
			}
		}
		row = row[j+1:]
	}
}

// rowsOf is the first and last drawn row of line i, from the last render.
func (d *searchDoc) rowsOf(i int) (first, last int) {
	if i < 0 || i >= len(d.firsts) {
		return 0, 0
	}
	last = d.rows() - 1
	if i+1 < len(d.firsts) {
		last = d.firsts[i+1] - 1
	}
	return d.firsts[i], last
}

// lineAtRow is the line drawn on row, from the last render.
func (d *searchDoc) lineAtRow(row int) int {
	i, _ := slices.BinarySearch(d.firsts, row+1)
	return max(i-1, 0)
}

// rows is how many rows the last render drew.
func (d *searchDoc) rows() int { return d.lastRows }

// rowStarts is the column of plain at which each of its wrapped rows begins. ansi.Wrap drops
// a space it breaks at and keeps one it does not, so each row is looked for where the last one
// ended, past any spaces the wrap dropped.
//
// The column is carried forward, measuring only what was passed since the last row. Measuring
// each row's whole prefix again made one long string quadratic: 2.2s per keystroke on a 225KB
// completion.
func rowStarts(plain string, rows []string) []int {
	starts := make([]int, len(rows))
	p, measured, col := 0, 0, 0
	for k, r := range rows {
		for p < len(plain) && plain[p] == ' ' && !strings.HasPrefix(plain[p:], r) {
			p++
		}
		col += ansi.StringWidth(plain[measured:p])
		measured = p
		starts[k] = col
		if strings.HasPrefix(plain[p:], r) {
			p += len(r)
		}
	}
	return starts
}

// startRowOf is the row column col is drawn on: the last row that starts at or before it.
func startRowOf(starts []int, col int) int {
	k := 0
	for k+1 < len(starts) && starts[k+1] <= col {
		k++
	}
	return k
}
