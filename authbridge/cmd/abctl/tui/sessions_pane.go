package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// sessionsColumns is the table's full-width column set, before any terminal-fitting.
//
// A function rather than a package var so layout() can re-fit from the originals on every
// resize: fitting the LIVE columns would be cumulative, and a terminal that got narrower once
// would keep its narrowed columns after being widened again.
//
// COST is INSERTED before ACTIVE rather than appended after it, because ACTIVE is a
// status marker and belongs at the end of a row where the eye stops. table.Row is a
// []string addressed positionally, so that insert shifted every reader of ACTIVE by
// one — selectedSessionID takes [0] and is unaffected, but a retention test pinned
// "cached" at [4] and now reads [5]. That shift COMPILES and keeps asserting; it just
// asserts about COST instead. Both were moved deliberately; if a third positional
// reader appears, move it in the same commit as any reordering.
//
// The declared widths sum to 90, so the table renders 102 display columns: six columns,
// each charged cellPadding for the space bubbles puts either side of a cell. That is past
// an 80-column terminal, which is why layout() runs this set through fitTableColumns.
//
// What fitTableColumns does is SHRINK the widest column one cell at a time until the total
// fits; it never DROPS a column. So the fitted set always has six entries in this order,
// and the positional trap above stays closed at every terminal width — but the burden of
// not clipping moves onto the CELL, because a column can arrive narrower than its content.
// Measured, fitTableColumns leaves COST 5 columns wide on a 40-column terminal, 7 at 50, 8
// at 60, and only the declared 10 from 70 up. bubbles renders each cell as
// runewidth.Truncate(value, col.Width, "…"), so a 5-wide COST column turns $1234.5678 into
// "$123…" — a figure short by an order of magnitude wearing an ellipsis narrow enough to
// miss. fitCostCell is what stops that; see its comment for what a too-narrow cell says
// instead. TestSessionsTable_CostCellIsNeverATruncatedNumber exercises the fitter's OUTPUT
// at those widths, which is the check that was missing while only the DECLARED widths were
// pinned.
//
// SPAN (UpdatedAt-CreatedAt) was added and then removed WITHIN this branch. It never
// existed on main, so a diff against upstream will not show it going, and nothing outside
// this branch ever depended on it. Two reasons it went, and neither of them was the fitter
// — the fitter drops nothing. It measured ELAPSED time, so a session idle for an hour
// reported an hour, which made it nearly redundant with UPDATED two columns to its left;
// and it was always blank on cached-only rows, which have no CreatedAt to subtract from. It
// cost width on every terminal and said something on few.
func sessionsColumns() []table.Column {
	return []table.Column{
		{Title: "ID", Width: 40},
		{Title: "UPDATED", Width: 14},
		{Title: "EVENTS", Width: 8},
		{Title: "TOKENS", Width: 10},
		// The title names the span, and it is the only thing on the row that can. See
		// costColumnTitle: TOKENS beside it is the session's LIFETIME count and this is a
		// ROLLING-window figure, and a bare "COST" promised nothing about either.
		// 10 columns is exactly "$9999.9999", the widest figure formatUSDCell's fixed 4dp
		// produces below five figures of dollars. Above that bubbles truncates with
		// runewidth.Truncate, putting an ellipsis inside a dollar amount — the one thing
		// #953 rules out. The ceiling means one session spending $10,000 inside the
		// strip's window.
		//
		// This is the DECLARED width, which the table only gets from a 70-column terminal
		// up; below that fitTableColumns shrinks it and fitCostCell elides rather than let
		// bubbles clip. Pinned at both levels — TestSessionsTable_CostFitsItsColumn on the
		// declaration, TestSessionsTable_CostCellIsNeverATruncatedNumber on the fitted
		// widths — so the trade is visible if the formatter's precision changes.
		{Title: costColumnTitle(""), Width: 10},
		{Title: "ACTIVE", Width: 8},
	}
}

// costColumnPrefix is what every COST title starts with, and it is how the column is
// FOUND rather than how it is spelled.
//
// The title carries a span now, so an exact match on "COST" stops finding the column the
// moment the span changes — and the two readers that matter, fittedSessionsColumnWidth
// and the row builder, would silently fall back to "no such column" and render every
// figure against a width of zero. Matched by prefix so the span can move without either
// of them noticing.
const costColumnPrefix = "COST"

// costColumnTitle names the span the COST column's figures actually cover.
//
// A bare "COST" promised nothing, and the cell two columns to its left promised something
// else: TOKENS is the session's LIFETIME total, straight from the session summary, while
// COST is summed out of the strip's ROLLING-window snapshot. So a six-hour live session
// showed six hours of tokens against one hour of dollars — understated roughly 6x — and
// dividing the two cells fabricated a rate that was never a rate. events_pane.go states
// the rule this breaks in capitals: EVERY CELL IS ROW-LOCAL. This file already refuses a
// cost lookup for cached-only rows on the argument that "any figure found would cover
// whatever part of the last hour happens to overlap, not the session the row is about",
// which applies verbatim to any live session older than the window.
//
// windowLabel comes from the SNAPSHOT, via spendSummary, not from the spendWindow
// constant: the server answers with the span it actually served and nothing may assume
// the two agree, so a title driven off the request could name an hour over six hours of
// data. Empty — no poll has answered yet — falls back to naming the span the strip ASKS
// for, because a bare "COST" is the thing being fixed and a header must say something.
//
// "COST/1h" is 7 display columns against a declared width of 10, so the declared widths
// and the 102-column arithmetic in sessionsColumns are untouched. Below 7 fitted columns
// bubbles ellipsises the header ("COST…"), which is a terminal at 40 columns, where
// fitCostCell is already eliding most figures: the span is lost exactly where the numbers
// are too.
func costColumnTitle(windowLabel string) string {
	if windowLabel == "" {
		windowLabel = formatWindowLabel(spendWindow)
	}
	return costColumnPrefix + "/" + windowLabel
}

// costElision is what a COST cell says when the column it landed in is too narrow to hold
// the whole figure.
//
// A lone ellipsis, and deliberately NOT blank. Blank already means "unpriced / unknown"
// everywhere in this table — sessionCost's contract, the cached-only rows, and three tests
// that pin it — so reusing it here would collapse "nobody knows what this cost" into "the
// terminal is too narrow to say it". They are different truths and a cell that conflates
// them is the same class of lie as "$0.00" for an unknown cost. The ellipsis asserts
// neither: it says a figure exists and did not fit, which no reader can mistake for an
// amount.
//
// One display column wide, so it fits any column fitTableColumns can produce
// (minColumnWidth is 4).
const costElision = "…"

// fitCostCell renders a priced session's cost for a column exactly width cells wide,
// eliding rather than letting bubbles clip it.
//
// inexact prefixes the figure with inexactMarker: at least one of this session's priced
// requests carries a figure the aggregator could not settle exactly, so the amount is a
// LOWER BOUND. Same marker as the strip and the Usage pane, for the reason
// inexactMarker's own comment gives — one spelling across every money surface. The
// alternative was what this cell did before: republish a truncated stream's floor as
// "$0.2546", an exact-looking figure to four decimal places.
//
// The marker costs one column, which lowers what this cell can state in full: the
// declared width is 10, "$9999.9999" is exactly 10, so "~$1234.5678" is 11 and elides
// even on a wide terminal. Accepted rather than widening the column, because the widths
// are pinned arithmetic (see sessionsColumns' 102-column note) and an elision is honest
// where a clipped figure is not — the in-cell ceiling for an INEXACT figure is
// $999.9999, and above it the cell says "a figure exists and did not fit".
//
// bubbles' renderRow is runewidth.Truncate(value, col.Width, "…"), which cuts a money
// figure from the RIGHT and keeps its LEADING digits: $1234.5678 in the 5-wide column a
// 40-column terminal leaves comes out "$123…", an amount ten times smaller that still reads
// as an amount. #953's rule is that no number is ever half-rendered, and a figure wrong by
// an order of magnitude is the worst case of it.
//
// Value-aware rather than a width threshold, because formatUSDCell is variable-width once
// the "$" and the "<" floor are counted: $0.1200 is 7 cells and states itself in full in
// the 7-wide column a 50-column terminal leaves, while $1234.5678 needs 10 and cannot. A
// blanket "elide below 10" would hide the common small figure on every narrow terminal for
// nothing.
//
// lipgloss.Width, never len() and never a rune count: it measures DISPLAY COLUMNS, which is
// what runewidth.Truncate compares against. footer.go:88-92 records what the other two cost.
//
// width <= 0 means the caller found no such column — a table whose columns have not been
// fitted yet. Render the figure: the declared width is 10 and holds everything below five
// figures of dollars.
//
// A NEGATIVE amount renders blank, which in this table means "unpriced / unknown" — the
// same refusal costTotalSection and sessionCost make, restated at the last step before a
// figure reaches the screen. formatUSDCell is faithful about the sign, so this cell
// printed "$-5.0000" and a minus sign in a column of costs reads as a refund nobody
// issued. sessionCost already declines to return one, which makes this the SECOND layer
// on purpose: the amount arrives here as a bare float64 with nothing on it to say where
// it came from, and every other money formatter in the package is reachable from a caller
// this one cannot see. See negativeCost — the guarantee is upstream and this is defence
// in depth.
func fitCostCell(usd float64, inexact bool, width int) string {
	if usd < 0 {
		return ""
	}
	cell := formatUSDCell(usd)
	if inexact {
		cell = inexactMarker + cell
	}
	if width > 0 && lipgloss.Width(cell) > width {
		return costElision
	}
	return cell
}

// fittedSessionsColumnWidth reports the width the sessions column whose title starts with
// prefix is CURRENTLY set to, which after layout() is fitTableColumns' output rather than
// sessionsColumns' declaration. 0 when there is no such column.
//
// A PREFIX, not the whole title, because the COST title carries its window label and that
// label follows the server's answer — see costColumnTitle. An exact match would stop
// resolving the column the first time the server served a span other than the one the
// strip asked for, and the row builder would then measure every figure against 0.
//
// Read from the live table at row-build time rather than computed here, so there is exactly
// one fitter and the cell measures itself against what the renderer will actually use.
//
// The two run on different messages, and that leaves a known seam: layout() re-fits the
// columns on WindowSizeMsg and does NOT rebuild the rows, so a resize leaves the existing
// rows formatted for the previous width until the next sessionsLoadedMsg or the next
// streamed event repaints them. For that interval a terminal just narrowed can show one
// clipped figure and one just widened can show one needless ellipsis. Closing it means
// rebuilding the rows from layout(), which is keys.go's call to make, not this file's.
func (m *model) fittedSessionsColumnWidth(prefix string) int {
	for _, c := range m.sessionsTbl.Columns() {
		if strings.HasPrefix(c.Title, prefix) {
			return c.Width
		}
	}
	return 0
}

// syncCostColumnTitle re-labels the COST column with the span its cells currently cover.
//
// Driven from the snapshot rather than declared once, so the header cannot drift from the
// data: the strip REQUESTS an hour and the server answers with whatever span it actually
// served, and the figures in these cells are summed out of that answer.
//
// Called from the row builder, which is the only place that knows the cells are about to
// be rebuilt from a particular snapshot. That leaves the same seam fittedSessionsColumnWidth
// documents in the other direction: layout() re-fits the columns on WindowSizeMsg and does
// not rebuild the rows, so a resize can leave the previous title in place until the next
// sessionsLoadedMsg or streamed event. The title is never WRONG in that interval — it is
// the span the current figures were summed over — merely older than the width beside it.
func (m *model) syncCostColumnTitle() {
	want := costColumnTitle(m.spendSummary().WindowLabel)
	// A copy: bubbles' Columns() hands back its own slice, so mutating an element in place
	// would change the live table without the UpdateViewport that SetColumns performs.
	cols := append([]table.Column(nil), m.sessionsTbl.Columns()...)
	for i := range cols {
		if !strings.HasPrefix(cols[i].Title, costColumnPrefix) {
			continue
		}
		if cols[i].Title == want {
			return
		}
		cols[i].Title = want
		m.sessionsTbl.SetColumns(cols)
		return
	}
}

// newSessionsTable builds an empty sessions table. Columns are fitted to the terminal by
// layout(), which is called on every WindowSizeMsg.
func newSessionsTable() table.Model {
	t := table.New(
		table.WithColumns(sessionsColumns()),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// rebuildSessionsTable updates the rows from m.sessions, applies the current
// filter, and keeps the cursor on the previously-selected session if still
// present.
func (m *model) rebuildSessionsTable() {
	prev := ""
	if rows := m.sessionsTbl.Rows(); len(rows) > 0 {
		prev = rows[m.sessionsTbl.Cursor()][0]
	}
	now := time.Now()
	// The header first: these cells are about to be summed out of a particular snapshot,
	// and the column has to name the span that snapshot covers.
	m.syncCostColumnTitle()
	// The width COST was actually FITTED to, not the 10 sessionsColumns declares:
	// fitTableColumns shrinks it to 5 on a 40-column terminal, and fitCostCell needs the
	// real number to decide whether a figure fits. Hoisted out of the loop because it is
	// the same for every row.
	costWidth := m.fittedSessionsColumnWidth(costColumnPrefix)
	rows := make([]table.Row, 0, len(m.sessions))
	for _, s := range m.sessions {
		if m.filter != "" && !strings.Contains(s.ID, m.filter) {
			continue
		}
		active := ""
		if s.Active {
			active = "●"
		}
		// Blank, never "$0.00", when the cost is unknown. usage_render.go establishes
		// the rule and the strip repeats it: a zero cost and an unknown cost are
		// different answers, and only one of them means the traffic was free. A cell
		// has no room to say "unavailable", so it says nothing — see sessionCost for
		// the three states that collapse into priced=false.
		//
		// A KNOWN cost goes through fitCostCell, which renders costElision rather than a
		// figure the fitted column would clip. That is a third state and it is why the
		// elision is not blank: blank here would say the cost is unknown when it is known
		// and merely too wide for this terminal.
		cost := ""
		if usd, priced, inexact := m.sessionCost(s.ID); priced {
			cost = fitCostCell(usd, inexact, costWidth)
		}
		rows = append(rows, table.Row{
			s.ID,
			relTime(now, s.UpdatedAt),
			// The server's count, and only ever the server's: it is the complete one.
			// abctl's own cache holds what it snapshotted plus what it has streamed
			// since attaching, which for a session older than the connection is a
			// smaller number — and when handleStreamEvent also wrote this field, the
			// cell flipped between the two on live traffic. The cached-only rows below
			// use len(cached) because the server does not list those at all.
			fmt.Sprintf("%d", s.EventCount),
			sessionTokens(s.TotalTokens, m.events[s.ID]),
			cost,
			active,
		})
	}
	// Sessions whose events abctl still holds but the server no longer lists.
	// Retaining the events (#870) is only half a fix if there is no row to
	// select them from: after a proxy restart the server lists nothing, so
	// without this the picker is empty and the retained history is unreachable.
	for _, id := range m.cachedOnlySessionIDs() {
		if m.filter != "" && !strings.Contains(id, m.filter) {
			continue
		}
		cached := m.events[id]
		// COST is blank for these rows on purpose, not because the lookup would be
		// awkward. It COULD be looked up — the id is all sessionCost needs — but these
		// are sessions the server has expired out of its store, while the strip's window
		// is a rolling hour: any figure found would cover whatever part of the last hour
		// happens to overlap, not the session the row is about. Blank says "unknown",
		// which is what it is.
		rows = append(rows, table.Row{
			id,
			"—",
			fmt.Sprintf("%d", len(cached)),
			sessionTokens(0, cached),
			"",
			"cached",
		})
	}
	m.sessionsTbl.SetRows(rows)

	// Restore cursor position if possible. Through setCursorVisible: a restored row
	// past the first screenful would otherwise land one line below the rendered
	// window, leaving the pane with no highlight — see setCursorVisible.
	if prev != "" {
		for i, r := range rows {
			if r[0] == prev {
				setCursorVisible(&m.sessionsTbl, i)
				return
			}
		}
	}
	setCursorVisible(&m.sessionsTbl, 0)
}

// cachedOnlySessionIDs lists sessions abctl has events for that the server's
// current list omits, sorted so the picker does not reshuffle under the cursor
// on each refresh. Empty caches are skipped: a row advertising zero events
// helps nobody, and snapshotLoadedMsg can create the key with an empty slice.
func (m *model) cachedOnlySessionIDs() []string {
	live := make(map[string]bool, len(m.sessions))
	for _, s := range m.sessions {
		live[s.ID] = true
	}
	out := make([]string, 0, len(m.events))
	for id, evs := range m.events {
		if live[id] || len(evs) == 0 {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// relTime renders "Ns", "Nm", "Nh" for small deltas; absolute time otherwise.
func relTime(now, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	switch {
	case d < time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("Jan 2 15:04")
	}
}

// sessionTokens reports the total tokens for a session. Prefers the
// server-computed count from SessionSummary (authoritative, covers the
// full event backlog even before we've streamed anything for this
// session). Falls back to a client-side sum over the cached events when
// the server returned zero (older authbridge server without token
// aggregation). Returns "—" when neither source has data.
func sessionTokens(serverTotal int, cached []pipeline.SessionEvent) string {
	if serverTotal > 0 {
		return formatCount(serverTotal)
	}
	var total int
	for i := range cached {
		if cached[i].Phase != pipeline.SessionResponse {
			continue
		}
		if cached[i].Inference == nil {
			continue
		}
		total += cached[i].Inference.TotalTokens
	}
	if total == 0 {
		return "—"
	}
	return formatCount(total)
}

// selectedSessionID returns the cursor row's session ID, or "".
func (m *model) selectedSessionID() string {
	rows := m.sessionsTbl.Rows()
	if len(rows) == 0 {
		return ""
	}
	return rows[m.sessionsTbl.Cursor()][0]
}
