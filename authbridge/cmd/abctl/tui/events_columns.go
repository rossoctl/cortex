package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/table"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// eventColumnID names a column stably, so a saved selection survives reordering
// and a future column being inserted in the middle.
type eventColumnID string

const (
	colIndex    eventColumnID = "#"
	colTime     eventColumnID = "TIME"
	colDir      eventColumnID = "DIR"
	colPhase    eventColumnID = "PHASE"
	colAction   eventColumnID = "ACTION"
	colPlugin   eventColumnID = "PLUGIN"
	colMethod   eventColumnID = "METHOD"
	colStatus   eventColumnID = "STATUS"
	colDuration eventColumnID = "DURATION"
	colTokens   eventColumnID = "TOKENS"
	colCost     eventColumnID = "COST"
	colHost     eventColumnID = "HOST"
)

// cellContext is what a cell function needs beyond the event itself.
//
// Two cells (TOKENS, COST) pair a request with its response to show a saving, so
// they need the row slice and the partner index as well as the model. Passing one
// struct keeps every cell function the same shape, rather than some taking an
// event and others taking five arguments.
type cellContext struct {
	m       *model
	rows    []eventRow
	partner map[int]int
	i       int
	row     eventRow
	// ids pairs a request event with its response, computed once per rebuild by
	// computeEventPairs. Carried here rather than recomputed per cell.
	ids map[*pipeline.SessionEvent]int
}

// eventColumn is one column: how to head it, how wide, and how to fill it.
//
// Header and cell live together on purpose. They were two parallel slices —
// []table.Column and a positional table.Row literal — maintained by hand, so
// inserting a column meant editing both in step and a mismatch silently shifted
// every cell right of it into the wrong heading.
type eventColumn struct {
	id    eventColumnID
	width int
	// cell renders this column for one row.
	cell func(cellContext) string
	// defaultOn is whether the column shows without the user asking.
	defaultOn bool
	// desc is the one-line explanation shown beside the name in the picker. Twelve
	// abbreviated headers are not self-describing — DIR, PHASE and METHOD least of
	// all — and a picker that only lists names asks the user to guess.
	desc string
	// keep ranks a column against being dropped when the terminal is too narrow.
	// Higher survives longer.
	//
	// Separate from defaultOn because the two are different questions: HOST is now
	// shown by default AND must outlive the columns it competes with, since it is
	// the one the user came for (#866). Ranking by defaultOn alone made every
	// default equally expendable and HOST — last in display order — the first to go.
	keep int
}

// Keep ranks. Only the ordering matters, not the values.
const (
	keepLow    = 0 // give these up first
	keepNormal = 1
	keepHigh   = 2 // give these up last
)

// eventColumns is the single definition every events-table column comes from, in
// display order.
var eventColumns = []eventColumn{
	{id: colIndex, width: 4, defaultOn: true, keep: keepHigh,
		desc: "exchange number; a request and its response share one",
		cell: func(c cellContext) string {
			if id, ok := c.ids[c.row.event]; ok {
				return strconv.Itoa(id)
			}
			return ""
		}},
	{id: colTime, width: 12, defaultOn: true, keep: keepNormal,
		desc: "wall-clock time the message was recorded",
		cell: func(c cellContext) string { return c.row.event.At.Format("15:04:05.00") }},
	{id: colDir, width: 4, defaultOn: true, keep: keepLow,
		desc: "in = toward your agent, out = toward an upstream",
		cell: func(c cellContext) string { return shortDirection(c.row.event.Direction) }},
	{id: colPhase, width: 7, defaultOn: true, keep: keepNormal,
		desc: "req, resp, or denied",
		cell: func(c cellContext) string { return shortPhase(c.row.event.Phase) }},
	{id: colAction, width: actionColWidth, defaultOn: true, keep: keepNormal,
		desc: "what took effect: deny, modify, observe, allow, or tunnel",
		cell: func(c cellContext) string { a, _ := rowAction(c.row, c.row.invocations()); return a }},
	{id: colPlugin, width: 18, defaultOn: true, keep: keepNormal,
		desc: "which plugin acted; blank when none did",
		cell: func(c cellContext) string {
			_, p := rowAction(c.row, c.row.invocations())
			return truncStr(p, 18)
		}},
	// methodColWidth rather than 22: the widest realistic value is a model name
	// ("claude-opus-5"), and the columns freed pay for splitting TOKENS and COST
	// apart below.
	{id: colMethod, width: methodColWidth, defaultOn: true, keep: keepNormal,
		desc: "protocol operation: model name, MCP or A2A method",
		cell: func(c cellContext) string { return eventMethod(*c.row.event) }},
	{id: colStatus, width: 7, defaultOn: true, keep: keepNormal,
		desc: "HTTP status of the response",
		cell: func(c cellContext) string { return statusCell(*c.row.event) }},
	{id: colDuration, width: 10, defaultOn: true, keep: keepLow,
		desc: "how long the exchange took",
		cell: func(c cellContext) string { return durationCell(*c.row.event) }},
	// 17, not 15: sized for a SEVEN-digit prompt, "1,048,576(−12.3k)". Million-token
	// contexts are in service, and bubbles truncates a cell at the column width, so
	// 15 rendered "1,048,576(−1…" — dropping the saving, which is the half of this
	// cell that appears nowhere else.
	{id: colTokens, width: 17, defaultOn: true, keep: keepLow,
		desc: "tokens used, and what tool-prune saved",
		cell: func(c cellContext) string { return c.m.tokensCell(c.rows, c.partner, c.i, c.row.event) }},
	// 19 fits the widest cell the formatter can produce: "<$0.0001(−<$0.0001)",
	// where both halves fell under the four-decimal floor. The ordinary shape is
	// "$0.2546(−$0.0037)" at 17.
	{id: colCost, width: 19, defaultOn: true, keep: keepLow,
		desc: "estimated cost, and what tool-prune saved",
		cell: func(c cellContext) string { return c.m.costCell(c.rows, c.partner, c.i, c.row.event) }},
	// keepHigh: the column #866 was filed about. Last in display order, so without
	// a rank it is the first thing a narrow terminal drops — which is how it came
	// to be declared but never visible.
	{id: colHost, width: 20, defaultOn: true, keep: keepHigh,
		desc: "host the message was sent to",
		cell: func(c cellContext) string { return truncStr(c.row.event.Host, 20) }},
}

// defaultColumnSelection is the set shown before the user chooses.
func defaultColumnSelection() map[eventColumnID]bool {
	out := make(map[eventColumnID]bool, len(eventColumns))
	for _, c := range eventColumns {
		out[c.id] = c.defaultOn
	}
	return out
}

// selectedColumns returns the visible columns in display order.
//
// Falls back to the defaults on an empty selection rather than rendering a table
// with no columns: a user who turns everything off should get a chart back, not a
// blank pane they cannot recover from without restarting.
func selectedColumns(sel map[eventColumnID]bool) []eventColumn {
	var out []eventColumn
	for _, c := range eventColumns {
		if sel[c.id] {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		for _, c := range eventColumns {
			if c.defaultOn {
				out = append(out, c)
			}
		}
	}
	return out
}

// tableColumns converts the selection into the bubbles column slice.
func tableColumns(cols []eventColumn) []table.Column {
	out := make([]table.Column, 0, len(cols))
	for _, c := range cols {
		out = append(out, table.Column{Title: string(c.id), Width: c.width})
	}
	return out
}

// columnsWidth is how many terminal columns a selection needs, including the
// one-space gutter bubbles renders between cells.
// cellPadding is what bubbles adds to every cell's declared width.
//
// TWO, not one: bubbles pads each cell on BOTH sides rather than putting a single
// space between them. table.DefaultStyles sets Cell and Header to
// `Padding(0, 1)`, renderRow runs each value through styles.Cell.Render and
// headersView through styles.Header.Render, and this repo's tableStyles() keeps
// both paddings (it only adds Foreground/Bold). So a column occupies width+2 and
// a row is sum(width) + 2n.
//
// Modelling it as +1 made fitColumns systematically under-count, so the fitting
// the whole feature rests on never actually fit — measured against bubbles v1.0.0,
// a row of all twelve columns renders at 168 while columnsWidth reported 156. At
// 80 the table overflowed by one column and wrapped, costing two terminal rows per
// event and throwing off the SetHeight accounting; between 156 and 167 it was
// worse, because dropped==0 meant the footer said nothing and the picker showed no
// "(no room)" while 8-12 columns sat off the edge — issue #866's exact failure
// mode at a different width.
const cellPadding = 2

// borderWidth is what styleBorder costs: one column each side. It has no padding —
// Border(RoundedBorder()).BorderForeground(colorMuted) — so the panel is its
// content plus two, and reserving four trimmed the hint line two columns earlier
// than necessary.
const borderWidth = 2

func columnsWidth(cols []eventColumn) int {
	w := 0
	for _, c := range cols {
		w += c.width + cellPadding
	}
	return w
}

// fitColumns keeps as many selected columns as the terminal holds, and reports how
// many it had to drop.
//
// Explicit rather than letting bubbles clip: with every column on the table needs
// ~168 terminal columns (144 of declared width plus two of bubbles padding per
// column, see columnsWidth), so HOST simply was not there and nothing said so. The
// count feeds the footer, which is what issue #866 asks for.
//
// Drops from the right by the static keep rank, so the columns that identify a row
// outlive the ones that merely annotate it.
//
// Never returns an empty slice: one clipped column beats a blank pane.
func fitColumns(cols []eventColumn, width int) (fitted []eventColumn, dropped int) {
	if width <= 0 || columnsWidth(cols) <= width {
		return cols, 0
	}

	// Give up low-ranked columns first, then normal, then high — and within a rank,
	// from the right. Ranking by "is it a default" instead made every default
	// equally expendable, so HOST (last in display order) went first, which is the
	// failure #866 describes.
	//
	// Index 0 is never sacrificed: "#" pairs a request with its response, and
	// without it the timeline cannot be read at all.
	sacrificeOrder := make([]int, 0, len(cols))
	for _, rank := range []int{keepLow, keepNormal, keepHigh} {
		for i := len(cols) - 1; i > 0; i-- {
			if cols[i].keep == rank {
				sacrificeOrder = append(sacrificeOrder, i)
			}
		}
	}

	drop := make(map[int]bool, len(cols))
	used := columnsWidth(cols)
	for _, i := range sacrificeOrder {
		if used <= width {
			break
		}
		drop[i] = true
		used -= cols[i].width + 1
	}

	for i, c := range cols {
		if !drop[i] {
			fitted = append(fitted, c)
		}
	}
	return fitted, len(drop)
}

// renderColumnPicker draws the column selector as a centred popup.
//
// A popup rather than the footer line this replaced: twelve abbreviated headers
// are not self-describing, and a one-line list had no room to say what DIR or
// METHOD mean. A box gives every column its own row, a checkbox, and a
// description — which is the difference between choosing and guessing.
//
// A column that is selected but will not fit the current terminal is marked, so
// enabling something and seeing no change is explained in place rather than only
// by the footer's count.
func renderColumnPicker(sel map[eventColumnID]bool, cursor, width, height int) string {
	fitted, _ := fitColumns(selectedColumns(sel), width)
	visible := make(map[eventColumnID]bool, len(fitted))
	for _, c := range fitted {
		visible[c.id] = true
	}

	var b strings.Builder
	b.WriteString(styleTitle.Render("COLUMNS"))
	b.WriteString("\n\n")

	// Bound the list to what the terminal can actually show.
	//
	// overlayCenter breaks out at `row >= len(out)`, so rows past the bottom edge
	// are dropped silently — and the LAST row is the key hints, the one thing a
	// stuck user needs. Verified: at 12 lines the "[esc] close" hint disappeared
	// entirely. The `height` parameter was accepted and ignored; this is the guard
	// it exists for.
	//
	// Reserve, counted against the rendered panel rather than guessed: 2 border
	// rows + title + blank after it + blank before the hint + hint = 6, plus 1 for
	// the "N of M shown" note that appears exactly when the list is clipped.
	rows := len(eventColumns)
	if height > 0 {
		if avail := height - 7; avail < rows {
			rows = avail
		}
		// Always show at least one column row, however short the terminal: a picker
		// with a title and no columns is worse than one that is obviously clipped.
		// Below ~8 lines the panel cannot fit its own border, title and hint, so it
		// is clipped by overlayCenter — a physical limit of a bordered modal, not
		// something this guard can recover. At 8 it degrades correctly: fitHintLine
		// drops "[↑↓] move" and keeps close/quit.
		if rows < 1 {
			rows = 1
		}
	}
	// Scroll the window so the cursor stays visible, rather than clipping the tail
	// and leaving the selection off-screen where space toggles something unseen.
	start := 0
	if cursor >= rows {
		start = cursor - rows + 1
	}
	end := start + rows
	if end > len(eventColumns) {
		end = len(eventColumns)
		if start = end - rows; start < 0 {
			start = 0
		}
	}

	for i, c := range eventColumns {
		if i < start || i >= end {
			continue
		}
		box := "[ ]"
		if sel[c.id] {
			box = "[x]"
		}
		name := fmt.Sprintf("%-9s", string(c.id))

		// Mark a selection the terminal cannot honour. Without this, turning HOST
		// on in an 80-column window looks like the checkbox did nothing.
		//
		// BEFORE the description, not after: the MaxWidth backstop truncates from the
		// right, so a marker appended to a long row was the first thing cut — ACTION's
		// row at 60 columns rendered as "… observe, allow, or" with the marker gone
		// entirely, losing the signal and keeping the prose that merely qualifies it.
		// This way a narrow terminal loses the explanation instead.
		marker := ""
		if sel[c.id] && !visible[c.id] {
			marker = styleMuted.Render("(no room) ")
		}
		line := fmt.Sprintf("%s %s %s%s", box, name, marker, c.desc)

		if i == cursor {
			b.WriteString(styleTitle.Render("▸ " + line))
		} else {
			b.WriteString("  " + line)
		}
		b.WriteString("\n")
	}

	if end-start < len(eventColumns) {
		b.WriteString(styleMuted.Render(fmt.Sprintf("  … %d of %d shown (terminal too short)",
			end-start, len(eventColumns))))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	// fitHintLine, so a narrow terminal drops hints from the front and keeps
	// [q] quit rather than wrapping the line. Same treatment the footer gets.
	b.WriteString(styleHint.Render(fitHintLine(
		"[↑↓] move  [space]/[x] toggle  [r] reset  [esc]/[enter] close  [q] quit",
		width-borderWidth)))

	// MaxWidth as a backstop. overlayCenter is explicit that bounding the panel is
	// the caller's job — it renders an over-wide panel flush-left and lets the
	// content spill — and renderHelpOverlay holds up its end the same way.
	box := styleBorder
	if width > 0 {
		box = box.MaxWidth(width)
	}
	return box.Render(b.String())
}

// anyColumnSelected reports whether the user has at least one column on.
//
// Distinct from len(selectedColumns(sel)) > 0, which is never zero: that function
// substitutes the defaults for an empty selection, so it cannot answer "did the
// user turn everything off". The toggle handler needs exactly that question.
func anyColumnSelected(sel map[eventColumnID]bool) bool {
	for _, c := range eventColumns {
		if sel[c.id] {
			return true
		}
	}
	return false
}
