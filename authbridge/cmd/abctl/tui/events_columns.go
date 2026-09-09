package tui

import (
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
	//
	// HOST is off by default despite being the column issue #866 was filed about:
	// with everything on, the table needs ~151 columns, so HOST was never visible
	// anyway. Off-by-default plus a picker means it is reachable, rather than
	// present-but-clipped.
	defaultOn bool
}

// eventColumns is the single definition every events-table column comes from, in
// display order.
var eventColumns = []eventColumn{
	{id: colIndex, width: 4, defaultOn: true,
		cell: func(c cellContext) string {
			if id, ok := c.ids[c.row.event]; ok {
				return strconv.Itoa(id)
			}
			return ""
		}},
	{id: colTime, width: 12, defaultOn: true,
		cell: func(c cellContext) string { return c.row.event.At.Format("15:04:05.00") }},
	{id: colDir, width: 4, defaultOn: true,
		cell: func(c cellContext) string { return shortDirection(c.row.event.Direction) }},
	{id: colPhase, width: 7, defaultOn: true,
		cell: func(c cellContext) string { return shortPhase(c.row.event.Phase) }},
	{id: colAction, width: actionColWidth, defaultOn: true,
		cell: func(c cellContext) string { a, _ := rowAction(c.row, c.row.invocations()); return a }},
	{id: colPlugin, width: 18, defaultOn: true,
		cell: func(c cellContext) string {
			_, p := rowAction(c.row, c.row.invocations())
			return truncStr(p, 18)
		}},
	{id: colMethod, width: methodColWidth, defaultOn: true,
		cell: func(c cellContext) string { return eventMethod(*c.row.event) }},
	{id: colStatus, width: 7, defaultOn: true,
		cell: func(c cellContext) string { return statusCell(*c.row.event) }},
	{id: colDuration, width: 10, defaultOn: true,
		cell: func(c cellContext) string { return durationCell(*c.row.event) }},
	{id: colTokens, width: 17, defaultOn: true,
		cell: func(c cellContext) string { return c.m.tokensCell(c.rows, c.partner, c.i, c.row.event) }},
	{id: colCost, width: 19, defaultOn: true,
		cell: func(c cellContext) string { return c.m.costCell(c.rows, c.partner, c.i, c.row.event) }},
	// The column the issue is about. Off by default — see eventColumn.defaultOn.
	{id: colHost, width: 20, defaultOn: false,
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
func columnsWidth(cols []eventColumn) int {
	w := 0
	for _, c := range cols {
		w += c.width + 1
	}
	return w
}

// fitColumns keeps as many selected columns as the terminal holds, and reports how
// many it had to drop.
//
// Explicit rather than letting bubbles clip: with every column on the table needs
// ~135 terminal columns, so HOST simply was not there and nothing said so. The
// count feeds the footer, which is what issue #866 asks for.
//
// Drops by declaration order from the right, EXCEPT that a column the user turned
// on explicitly outranks one that is merely on by default. Without that, the
// issue's own scenario fails: turn off DIR/TIME/TOKENS to make room for HOST, and
// HOST — last in display order — is still the first thing dropped, so the user
// gets none of what they asked for. An explicit choice has to survive the columns
// nobody chose.
//
// Never returns an empty slice: one clipped column beats a blank pane.
func fitColumns(cols []eventColumn, width int) (fitted []eventColumn, dropped int) {
	if width <= 0 || columnsWidth(cols) <= width {
		return cols, 0
	}

	// Rank sacrifices: default-on columns first, from the right, then opted-in
	// ones. Index 0 is never sacrificed — "#" pairs the exchange and is what makes
	// the timeline readable at all.
	// Two passes: default-on columns are given up first, then the opted-in ones. A
	// column with defaultOn=false is only present because the user asked for it, so
	// it is sacrificed LAST — the reverse of that is what made HOST vanish again in
	// the issue's own scenario.
	sacrificeOrder := make([]int, 0, len(cols))
	for _, giveUpDefaults := range []bool{true, false} {
		for i := len(cols) - 1; i > 0; i-- {
			if cols[i].defaultOn != giveUpDefaults {
				continue
			}
			sacrificeOrder = append(sacrificeOrder, i)
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

// columnPickerLine is the one-line column selector, rendered in the footer while
// the picker is open.
//
// A line rather than a modal overlay: the choice is a set of toggles over twelve
// short names, and seeing the table change underneath as you toggle is the whole
// point — an overlay would cover the thing being configured.
func columnPickerLine(sel map[eventColumnID]bool, cursor int) string {
	var b strings.Builder
	b.WriteString("columns: ")
	for i, c := range eventColumns {
		name := string(c.id)
		switch {
		case i == cursor && sel[c.id]:
			b.WriteString(styleOK.Render("[" + name + "]"))
		case i == cursor:
			b.WriteString(styleWarn.Render("[" + name + "]"))
		case sel[c.id]:
			b.WriteString(styleOK.Render(name))
		default:
			b.WriteString(styleMuted.Render(name))
		}
		if i < len(eventColumns)-1 {
			b.WriteString(" ")
		}
	}
	return b.String()
}
