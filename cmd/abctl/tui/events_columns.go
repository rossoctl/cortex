package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"

	"github.com/rossoctl/cortex/core/pipeline"
)

// eventColumnID names a column stably, so a selection is keyed by identity rather
// than position: a future column inserted in the middle cannot silently change what
// an existing selection means. (Nothing persists a selection today — it lives in
// m.eventColumns for the process lifetime — so this is the property the naming
// buys, not behaviour already in place.)
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
	// invs is this row's invocations, computed once in the row loop.
	//
	// rebuildEventsTable already needs them for the hideInactive check, and
	// rowAction returns ACTION and PLUGIN together — so having each of those two
	// cells call row.invocations() ran it three times per row and discarded half of
	// rowAction's result twice. invocations() allocates (allInvocations, plus an
	// append when a tunnel row is folded in) and eventAction walks the slice twice,
	// on up to 500 rows for every SSE event and every resize.
	//
	// Carrying it also makes it structural rather than incidental that the ACTION
	// and PLUGIN cells agree: they now read one rowAction result instead of two.
	invs []pipeline.Invocation
	// action and plugin are that one rowAction result.
	action string
	plugin string
	// width is the declared width of the column being filled, set per column in
	// the row loop.
	//
	// PLUGIN and HOST previously hardcoded 18 and 20 in their truncStr calls,
	// duplicating the width two lines above them — the same drift actionColWidth and
	// methodColWidth were named to prevent, in the one file whose premise is that a
	// column is a single definition. Reading it from here means changing a width
	// cannot leave a truncation behind.
	width int
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
	//
	// Returns the VALUE, unpadded: alignment is rightAlign's business, applied by
	// render below, so a cell function cannot align itself in a way its header does
	// not know about. That is what went wrong — see rightAlign.
	cell func(cellContext) string
	// rightAlign presses this column's values, and its heading, against the column's
	// right edge. For the numeric columns: figures are compared down a column, which
	// needs their last digits in one place.
	//
	// ONE FLAG FOR BOTH, which is the point. The three numeric cells each called
	// padLeft themselves and nothing did the same for the header, so bubbles rendered
	// every heading left-flush and TOKENS sat eleven columns from the figure it
	// named. Declaring the alignment on the column means the header cannot be left
	// out of it: render and tableColumns read this same field.
	rightAlign bool
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
	// sortKey extracts the value this column orders by (#865). nil means the column
	// cannot be sorted on — only "#", whose order IS the chronological order the
	// exchange numbers are assigned in, so sorting by it is what sorting by nothing
	// already does.
	//
	// Deliberately NOT the rendered cell string. durationCell emits "340ms" and
	// "1.20s", which compare lexically as 340ms > 1.20s — backwards, and on exactly
	// the column issue #865 names. TOKENS ("1,048,576(−12.3k)") and COST
	// ("$0.2546(−$0.0037)") carry thousands separators, a currency sigil and a
	// parenthesised second figure, none of which sort either.
	//
	// Takes a cellContext rather than an event because TOKENS and COST are
	// exchange-level: a request row's figures live on its paired RESPONSE, reached
	// through the same pairedResponse(rows, partner, i, ev) call their cells make.
	// Sharing the input means the key and the cell cannot disagree about which
	// response a row belongs to.
	sortKey func(cellContext) sortValue
}

// sortValue is one row's position along one column.
//
// Two fields rather than a single float64: HOST, PLUGIN and METHOD have no numeric
// reading, and mapping strings onto floats to force one would either collide or
// need a full collation table. Two rather than an interface{}, because every
// comparison is between two cells of the SAME column, so exactly one of the fields
// is ever live and a type switch per comparison buys nothing.
//
// The zero value sorts first ascending / last descending, which is where a blank
// cell belongs: "no figure published" is not "zero dollars", and a descending sort
// by COST should open with the most expensive call rather than with every row the
// proxy could not price.
type sortValue struct {
	// numeric selects which field decides. Set per column, never per row, so a
	// column cannot compare a number against a string.
	numeric bool
	num     float64
	str     string
}

// less orders two cells of one column. Ascending; the caller flips for descending.
func (a sortValue) less(b sortValue) bool {
	if a.numeric {
		return a.num < b.num
	}
	return a.str < b.str
}

// numKey and strKey keep the eventColumns table readable — a column declares what
// it sorts on, not how sortValue is shaped.
func numKey[T int | int64 | float64 | time.Duration](n T) sortValue {
	return sortValue{numeric: true, num: float64(n)}
}

func strKey(s string) sortValue { return sortValue{str: s} }

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
	// UnixNano, not the rendered "15:04:05.00": that string drops the date and
	// truncates to hundredths, so it collates two events a day apart as equal and
	// sorts 23:59 above 00:01 from the following morning.
	{id: colTime, width: 12, defaultOn: true, keep: keepNormal,
		desc:    "wall-clock time the message was recorded",
		cell:    func(c cellContext) string { return c.row.event.At.Format("15:04:05.00") },
		sortKey: func(c cellContext) sortValue { return numKey(c.row.event.At.UnixNano()) }},
	{id: colDir, width: 4, defaultOn: true, keep: keepLow,
		desc:    "in = toward your agent, out = toward an upstream",
		cell:    func(c cellContext) string { return shortDirection(c.row.event.Direction) },
		sortKey: func(c cellContext) sortValue { return strKey(shortDirection(c.row.event.Direction)) }},
	{id: colPhase, width: 7, defaultOn: true, keep: keepNormal,
		desc:    "req, resp, or denied",
		cell:    func(c cellContext) string { return shortPhase(c.row.event.Phase) },
		sortKey: func(c cellContext) sortValue { return strKey(shortPhase(c.row.event.Phase)) }},
	// c.action, the value the cell shows — not actionRank. The rank orders by
	// severity for topInvocation's "which invocation headlines this row" question;
	// a user sorting the ACTION column is grouping like with like, and a severity
	// order would put rows under a heading whose alphabet they do not follow.
	{id: colAction, width: actionColWidth, defaultOn: true, keep: keepNormal,
		desc:    "what took effect: deny, modify, observe, allow, or tunnel",
		cell:    func(c cellContext) string { return c.action },
		sortKey: func(c cellContext) sortValue { return strKey(c.action) }},
	// Sorts on the FULL plugin name, not the truncated cell: two plugins sharing an
	// 18-character prefix render identically and would otherwise tie arbitrarily.
	{id: colPlugin, width: 18, defaultOn: true, keep: keepNormal,
		desc: "which plugin acted; blank when none did",
		cell: func(c cellContext) string {
			p := c.plugin
			return truncStr(p, c.width)
		},
		sortKey: func(c cellContext) sortValue { return strKey(c.plugin) }},
	// methodColWidth rather than 22: the widest realistic value is a model name
	// ("claude-opus-5"), and the columns freed pay for splitting TOKENS and COST
	// apart below.
	{id: colMethod, width: methodColWidth, defaultOn: true, keep: keepNormal,
		desc: "protocol operation: model name, MCP or A2A method",
		cell: func(c cellContext) string { return eventMethod(*c.row.event) },
		// eventMethodValue, not eventMethod: the latter is the former truncated to
		// methodColWidth, and two long model names sharing a prefix must not tie.
		sortKey: func(c cellContext) sortValue { return strKey(eventMethodValue(*c.row.event)) }},
	// The integer, not statusCell's string: the cell can carry an error marker, and
	// a numeric sort is what puts the 5xx rows together at one end.
	{id: colStatus, width: 7, defaultOn: true, keep: keepNormal,
		desc:    "HTTP status of the response",
		cell:    func(c cellContext) string { return statusCell(*c.row.event) },
		sortKey: func(c cellContext) sortValue { return numKey(c.row.event.StatusCode) }},
	// The Duration itself. This is the column #865 is about ("the events with the
	// longest duration"), and the one where sorting the rendered string is most
	// obviously wrong: "340ms" > "1.20s" lexically.
	{id: colDuration, width: 10, defaultOn: true, keep: keepLow, rightAlign: true,
		desc:    "how long the exchange took",
		cell:    func(c cellContext) string { return durationCell(*c.row.event) },
		sortKey: func(c cellContext) sortValue { return numKey(c.row.event.Duration) }},
	// 17, not 15: sized for a SEVEN-digit prompt, "1,048,576(−12.3k)". Million-token
	// contexts are in service, and bubbles truncates a cell at the column width, so
	// 15 rendered "1,048,576(−1…" — dropping the saving, which is the half of this
	// cell that appears nowhere else.
	{id: colTokens, width: 17, defaultOn: true, keep: keepLow, rightAlign: true,
		desc: "tokens used, and what tool-prune saved",
		cell: func(c cellContext) string {
			return c.m.tokensCell(c.rows, c.partner, c.i, c.row.event)
		},
		sortKey: func(c cellContext) sortValue { return numKey(rowTokens(c)) }},
	// 19 fits the widest cell the formatter can produce: "<$0.0001(−<$0.0001)",
	// where both halves fell under the four-decimal floor. The ordinary shape is
	// "$0.2546(−$0.0037)" at 17.
	{id: colCost, width: 19, defaultOn: true, keep: keepLow, rightAlign: true,
		desc: "estimated cost, and what tool-prune saved",
		cell: func(c cellContext) string {
			return c.m.costCell(c.rows, c.partner, c.i, c.row.event)
		},
		sortKey: func(c cellContext) sortValue { return numKey(rowCostUSD(c)) }},
	// keepHigh: the column #866 was filed about. Last in display order, so without
	// a rank it is the first thing a narrow terminal drops — which is how it came
	// to be declared but never visible.
	{id: colHost, width: 20, defaultOn: true, keep: keepHigh,
		desc: "host the message was sent to",
		cell: func(c cellContext) string { return truncStr(c.row.event.Host, c.width) },
		// hostOnly, so "api.example.com:443" and "api.example.com" sort together
		// rather than the port deciding. Full host, not the truncated cell, for the
		// same reason PLUGIN uses the full name.
		sortKey: func(c cellContext) sortValue { return strKey(hostOnly(c.row.event.Host)) }},
}

// render is one cell as the table receives it: the column's value, aligned the way the column
// declares.
//
// Sets cc.width itself rather than trusting the caller to, so the width a cell truncates
// against and the width it is aligned into are read from the same place — the column. The copy
// is local (cellContext is passed by value), so nothing leaks into the next column's turn.
//
// The header counterpart is tableColumns, which reads the same rightAlign field.
func (c eventColumn) render(cc cellContext) string {
	cc.width = c.width
	v := c.cell(cc)
	if c.rightAlign {
		return padLeft(v, c.width)
	}
	return v
}

// rowTokens and rowCostUSD are the numeric readings behind the TOKENS and COST
// cells, for sorting (#865).
//
// They mirror tokensCell / costCell arm for arm, including the pairedResponse
// guard, because a sort that disagreed with the number on screen would be worse
// than no sort: the user sorts to find the biggest figure and then reads the cell
// to see what it was. Kept beside the column definitions rather than folded into
// the cells, because the cells return preformatted strings — the formatting is the
// part that cannot be sorted.
//
// Zero means "nothing to order by" — no figure published, or a request whose
// response has not landed — and lands at the bottom of a descending sort.
func rowTokens(c cellContext) int {
	ev := c.row.event
	switch ev.Phase {
	case pipeline.SessionResponse:
		if ev.Inference == nil {
			return 0
		}
		if n := ev.Inference.OutputTokens; n > 0 {
			return n
		}
		return ev.Inference.CompletionTokens
	case pipeline.SessionRequest:
		resp := pairedResponse(c.rows, c.partner, c.i, ev)
		if resp == nil {
			return 0
		}
		return pipeline.PromptTokensOf(resp.Inference)
	default:
		return 0
	}
}

func rowCostUSD(c cellContext) float64 {
	ev := c.row.event
	switch ev.Phase {
	case pipeline.SessionResponse:
		usd, ok := outputCost(ev)
		if !ok {
			return 0
		}
		return usd
	case pipeline.SessionRequest:
		resp := pairedResponse(c.rows, c.partner, c.i, ev)
		if resp == nil {
			return 0
		}
		usd, ok := promptCost(resp)
		if !ok {
			return 0
		}
		return usd
	default:
		return 0
	}
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

// sortGlyphAsc and sortGlyphDesc mark the sorted column in its own header.
//
// One column wide each, which is what makes the marker free: measured against
// bubbles v1.0.0, headersView renders
// runewidth.Truncate(col.Title, col.Width, "…"), and Truncate leaves a string
// whose width EQUALS the limit alone. Every header plus one glyph fits its
// declared width, with STATUS (7 of 7) and DIR (4 of 4) exactly on the boundary —
// so no column had to be widened and fitColumns' arithmetic is untouched.
// TestTableColumns_SortGlyphFitsEveryWidth pins that, since a longer header or a
// tighter width would silently start clipping the column's NAME.
const (
	sortGlyphAsc  = "▲"
	sortGlyphDesc = "▼"
)

// tableColumns converts the selection into the bubbles column slice, marking the
// sorted column (#865) and right-aligning the heading of every column that
// right-aligns its cells — the header half of eventColumn.rightAlign.
//
// The glyph goes on BEFORE the padding, so the marker stays against the name's right edge
// where a reader expects it rather than drifting to the column's far side. It costs one of the
// column's own columns either way, which is what TestTableColumns_SortGlyphFitsEveryWidth
// measures.
//
// sortCol is "" for chronological order, in which case no header carries a glyph —
// which is what newEventsTable passes, having no sort state to consult.
func tableColumns(cols []eventColumn, sortCol eventColumnID, desc bool) []table.Column {
	out := make([]table.Column, 0, len(cols))
	for _, c := range cols {
		title := string(c.id)
		if sortCol != "" && c.id == sortCol {
			if desc {
				title += sortGlyphDesc
			} else {
				title += sortGlyphAsc
			}
		}
		if c.rightAlign {
			title = rightAlignHeader(title, c.width)
		}
		out = append(out, table.Column{Title: title, Width: c.width})
	}
	return out
}

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

// columnsWidth is how many terminal columns a selection needs: each column's
// declared width plus the cellPadding bubbles adds around it.
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
		// cellPadding, matching what columnsWidth charged. Crediting back +1 against
		// a +2 charge refunded one column too little per drop.
		used -= cols[i].width + cellPadding
	}

	// Second pass: take back what still fits.
	//
	// The loop above is greedy and never reconsiders, so it overshoots whenever the
	// column that finally gets it under the limit is a wide one. At 80 it stood at 81
	// — one column over — and had to give up an 18-wide PLUGIN to comply, landing at
	// 61 and leaving 19 columns unused while DIR (6), STATUS (9), DURATION (12) and
	// TOKENS (19) each would have fit in that slack.
	//
	// Restoring in REVERSE sacrifice order returns the most valuable candidates
	// first: the last thing given up was the least expendable, so it has the best
	// claim on the room that turned out to be there. A column that does not fit is
	// skipped rather than ending the pass, since a narrower one further back may
	// still fit — which is exactly the COST-then-TOKENS case at 80.
	for i := len(sacrificeOrder) - 1; i >= 0; i-- {
		idx := sacrificeOrder[i]
		if !drop[idx] {
			continue
		}
		cost := cols[idx].width + cellPadding
		if used+cost <= width {
			delete(drop, idx)
			used += cost
		}
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
func renderColumnPicker(sel map[eventColumnID]bool, cursor, width, height int,
	sortCol eventColumnID, sortDesc bool) string {
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
		// The sorted column is marked here as well as in the table header, because
		// this popup is where the sort is CHOSEN: `s` cycles three states and an
		// operator pressing it needs to see which one they landed in without closing
		// the modal to look at the table behind it.
		name := string(c.id)
		if sortCol != "" && c.id == sortCol {
			if sortDesc {
				name += sortGlyphDesc
			} else {
				name += sortGlyphAsc
			}
		}
		name = fmt.Sprintf("%-10s", name)

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
		"[↑↓] move  [space]/[x] toggle  [s] sort  [r] reset  [esc]/[enter] close  [q] quit",
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
