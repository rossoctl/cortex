package tui

import (
	"fmt"
	"net"
	"slices"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/cmd/agentop/money"
	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session/timeline"
)

// eventKey pins a row to a specific event across rebuilds, so the
// cursor follows the same event when the underlying slice shifts:
// FIFO eviction at session.max_events, a re-sort, hideInactive
// toggled. Zero value = unpinned. `at` is UnixNano so struct == is
// safe against time.Time's monotonic clock and Location pointer.
type eventKey struct {
	at        int64
	direction pipeline.Direction
	phase     pipeline.SessionPhase
	requestID string
	// isTool, tool, toolAgent and toolSeq pin a tool row: its tool ID, agent and response seq.
	// The seq tells apart two calls with one ID: some local models send call_0 on every turn,
	// and some send no ID. toolPos tells apart two calls with no ID in one response.
	isTool          bool
	tool, toolAgent string
	toolSeq         uint64
	toolPos         int
}

func keyOf(e *pipeline.SessionEvent) eventKey {
	if e == nil {
		return eventKey{}
	}
	return eventKey{at: e.At.UnixNano(), direction: e.Direction, phase: e.Phase, requestID: e.RequestID}
}

// pinKeyOf is the pin key of row er: its tool for a tool row, else its event.
func pinKeyOf(er eventRow) eventKey {
	if er.tool != nil {
		return eventKey{isTool: true, tool: er.tool.ID, toolAgent: er.tool.Agent, toolSeq: er.tool.Seq, toolPos: er.tool.Pos}
	}
	return keyOf(er.event)
}

// selectedPinKey is the pin key of the row under the cursor.
func (m *model) selectedPinKey() eventKey {
	return pinKeyOf(m.selectedRowOn(m.eventsTbl.Cursor()))
}

// findByKey is the row that holds the event with key k, by either half (#1681). A pin can
// name a response: a response-only row is keyed by it, and the row merges with its request
// when the older page with the request loads.
func findByKey(rows []eventRow, k eventKey) int {
	if k.isTool {
		return slices.IndexFunc(rows, func(r eventRow) bool {
			return r.tool != nil && r.tool.ID == k.tool && r.tool.Agent == k.toolAgent && r.tool.Seq == k.toolSeq &&
				r.tool.Pos == k.toolPos
		})
	}
	return slices.IndexFunc(rows, func(r eventRow) bool {
		return rowHolds(r, func(e *pipeline.SessionEvent) bool { return keyOf(e) == k })
	})
}

// rowHolds reports whether either half of row r satisfies match.
func rowHolds(r eventRow, match func(*pipeline.SessionEvent) bool) bool {
	return (r.event != nil && match(r.event)) || (r.resp != nil && match(r.resp))
}

// newEventsTable builds an empty events table. Uses the shared tableStyles like every other pane.
//
// The selection highlight is a background tint now, not a reverse — see tableStyles. What stood
// here said Reverse was "safe because per-cell ANSI coloring was removed from this table", and
// that same removal is why styles.go was left claiming Reverse protected colouring that no
// longer existed anywhere.
func newEventsTable() table.Model {
	t := table.New(
		// One definition, not two. This used to hold a hand-written twelve-entry
		// copy of eventColumns' widths — harmless, because the first
		// rebuildEventsTable overwrites it via SetColumns, which is exactly what
		// made it a hazard: a width changed in one place and not the other produced
		// no symptom at all. The rationale for the non-obvious widths now lives
		// beside the widths that actually decide, in eventColumns.
		// No sort column: the table is chronological until a model says otherwise.
		table.WithColumns(tableColumns(selectedColumns(defaultColumnSelection()), "", false)),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// eventRow is one display row: one exchange (#1681). event is the row's first event — the
// request, or the only event of a row with one half: a denial, a request no answer reached,
// a response whose request is not held. resp is the response or denial that mergeExchanges
// folded in. tunnel, when non-nil, is a CONNECT tunnel-open event folded into a TLS-bridged
// request (see buildEventRows). It contributes a summary line to the detail pane but no
// separate row.
type eventRow struct {
	event  *pipeline.SessionEvent
	tunnel *pipeline.SessionEvent
	resp   *pipeline.SessionEvent
	// raw and respRaw are the exact captures of event and resp, set on the row the detail
	// pane fetched them for.
	raw     *pipeline.RawCapture
	respRaw *pipeline.RawCapture
	// tool is set on a tool row (#1776), which has no event. last is set on the last tool
	// of its exchange.
	tool *toolRow
	last bool
}

// invocations returns every plugin invocation the row's ACTION/PLUGIN cell and the inactive
// filter should consider: both halves of the exchange, plus any folded CONNECT tunnel's gate
// invocations — so a bridged row reflects activity on the tunnel-open (e.g. an egress gate
// that allowed the CONNECT) and isn't wrongly hidden or under-reported.
func (er eventRow) invocations() []pipeline.Invocation {
	invs := allInvocations(er.event)
	if er.tunnel != nil {
		invs = append(invs, allInvocations(er.tunnel)...)
	}
	if er.resp != nil {
		invs = append(invs, allInvocations(er.resp)...)
	}
	return invs
}

// partnerActive reports whether row i's paired request or response is one a plugin
// acted on, so hideInactive keeps row i with it.
func partnerActive(ctxs []cellContext, partner map[int]int, i int) bool {
	p, ok := partner[i]
	return ok && !eventInactive(ctxs[p].invs)
}

// rebuildEventsTable populates the events table from the cache for the
// currently selected session, applying hideInactive, marking the search's
// matches and preserving the cursor. Also
// resizes the table height to account for the IDENTITY banner — when
// the session has inbound identity, subtract the banner's rendered
// height so it doesn't push rows off-screen; otherwise claim the full
// body height.
func (m *model) rebuildEventsTable() {
	events := m.events[m.selectedSess]

	if m.bodyHeight > 0 {
		// Measured, not declared: identityBannerHeightFor renders the banner at this
		// terminal's width and reports what it costs. The old form subtracted a constant
		// and an over-wide banner then wrapped in the terminal, taking rows the table had
		// already claimed.
		h := m.bodyHeight - identityBannerHeightFor(events, m.width)
		if h < 3 {
			h = 3
		}
		m.eventsTbl.SetHeight(h)
	}

	prevRow := m.eventsTbl.Cursor()
	// A tool pin that waits for its timeline left the cursor on a stand-in row, not at the end.
	pinWaits := m.selectedEventKey.isTool && findByKey(m.visibleRows, m.selectedEventKey) < 0
	wasAtEnd := prevRow >= len(m.eventsTbl.Rows())-1 && !pinWaits

	// One display row per network message. CONNECT tunnel-opens are folded
	// into the decrypted inner request that immediately follows them (TLS
	// bridge), so a bridged call reads as a single request row — the same
	// shape as a plaintext call.
	eventRows := buildEventRows(events)

	// Pair request rows with their response rows. ids drives the # column: one
	// integer repeated across a request/response exchange, which is how an
	// exchange is read off the timeline.
	ids, partner := computeEventPairs(eventRows)
	// One row per exchange (#1681). ids stays the pass above's: it is keyed by event pointer,
	// so a merged row keeps the number its request had, and # reads as it always did.
	eventRows, partner = mergeExchanges(eventRows, partner)

	// One cellContext per row of the SESSION, before hideInactive drops any. The
	// fitsContent columns are measured over all of them (see measureColumns), and the row
	// loop below shows the cells measured there, so a column cannot be sized from one
	// reading of a row and filled from another.
	//
	// That computes invocations, rowAction and the measured cells for rows hideInactive
	// then hides, which the loop used to skip. Each is evaluated once per row, which is
	// what showing every row pays on every rebuild anyway, so hiding rows never makes a
	// rebuild dearer than showing them.
	ref := m.lostRef(events)
	ctxs := make([]cellContext, len(eventRows))
	for i, er := range eventRows {
		// One invocations and one rowAction per row: hideInactive below reads invs,
		// and the ACTION and PLUGIN cells read this one rowAction result rather than
		// each recomputing the pair and discarding half of it. Both consider the
		// folded tunnel's invocations too.
		invs := er.invocations()
		action, plugin := rowAction(er, invs)
		ctxs[i] = cellContext{
			m: m, row: er, ids: ids,
			invs: invs, action: action, plugin: plugin, lost: rowLost(er, partner, i, ref),
		}
	}

	// One selection per rebuild, sized to the session and fitted to the terminal.
	// Both the header and every row cell come from `cols`, so they cannot disagree.
	var measured map[eventColumnID][]string
	m.eventColWidths, measured = measureColumns(selectedColumns(m.eventColumns), ctxs)
	cols, dropped := layoutColumns(m.eventColumns, m.eventColWidths, m.width)
	m.eventColsDropped = dropped

	// Rows are built first and handed to the table together with their columns at
	// the end of this function — see the note there on why the order matters.
	rows := make([]table.Row, 0, len(eventRows))
	m.visibleRows = m.visibleRows[:0]
	m.hiddenInactive = 0

	// The older-events row goes first, with no event under it, like a subagent header.
	// lead is how many rows precede the events: the sort below must not move this one.
	lead := 0
	older := m.olderNotice()
	if older != "" {
		rows = append(rows, table.Row{older})
		m.visibleRows = append(m.visibleRows, eventRow{})
		lead = 1
	}

	// Subagent blocks (#1047), only while CHRONOLOGICAL: under a sort the row
	// order is a ranking, and pulling a subagent's rows under a header would
	// contradict the very thing the operator sorted to see. Collected over the
	// same population the row loop renders — the inactive filter is applied
	// identically, via the same predicate — so a header's rollup counts the rows
	// expanding it would show.
	//
	// headerRows is rebuilt with them; subagentOpen (the operator's collapses)
	// is cleared when the session under the table changed, since a subagent id
	// belongs to the session that spawned it.
	m.headerRows = nil
	if m.eventsBuiltFor != m.selectedSess {
		m.subagentOpen = nil
	}
	if m.sortCol == "" {
		m.subagentBlocks = m.collectSubagentBlocks(ctxs, partner)
	} else {
		m.subagentBlocks = nil
	}
	// Member rows number their exchanges WITHIN their block, from 1 (#1047): the
	// block is read as the subagent's own transcript, so its first exchange is 1
	// — the session-wide number the main rows keep would start a block at 94 and
	// say nothing about how much the subagent itself did. The session-wide ids
	// still decide what an exchange IS (a request and its response share one
	// number there, and therefore here); only the counting restarts per block.
	// Assigned into ctxs here, after the blocks exist and before any cell
	// renders — the # column is not a measured one, so measureColumns above never
	// read it.
	var blockIDs map[*pipeline.SessionEvent]int
	for _, b := range m.subagentBlocks {
		if blockIDs == nil {
			blockIDs = make(map[*pipeline.SessionEvent]int)
		}
		local := make(map[int]int, len(b.rows))
		for _, j := range b.rows {
			e := ctxs[j].row.event
			n, ok := local[ids[e]]
			if !ok {
				n = len(local) + 1
				local[ids[e]] = n
			}
			blockIDs[e] = n
		}
		b.msgs = len(local)
	}
	for i := range ctxs {
		ctxs[i].blockIDs = blockIDs
	}
	// keys is the sort value for each row that survived the filters, parallel to
	// rows and m.visibleRows. Collected inside the loop because the sortKey funcs
	// take the same cellContext the cells do, and that context only exists here.
	// nil when no sort is active, so the default path allocates nothing.
	var keys []rowKey
	var sortCol *eventColumn
	if m.sortCol != "" {
		for i := range eventColumns {
			if eventColumns[i].id == m.sortCol && eventColumns[i].sortKey != nil {
				sortCol = &eventColumns[i]
				keys = make([]rowKey, 0, len(eventRows))
				break
			}
		}
	}
	// Tool rows (#1776) join an exchange by its response's seq, only for the session the
	// timeline is for: a linked bucket's events carry seqs of another session.
	var tools map[uint64][]toolRow
	if m.timeline.session == m.selectedSess {
		tools = m.timeline.tools
	}
	toolsOf := func(er eventRow) []toolRow {
		resp := exchangeResp(er)
		if len(tools) == 0 || resp == nil || resp.Seq == 0 || eventSession(m.selectedSess, resp) != m.selectedSess {
			return nil
		}
		return tools[resp.Seq]
	}
	now := m.clock()
	q := m.searchQuery(paneEvents)
	var full []table.SearchRow
	var droppedCols []eventColumn
	if q != "" {
		droppedCols = droppedColumns(selectedColumns(m.eventColumns), cols)
		for range lead {
			full = append(full, table.SearchRow{NoMatch: true})
		}
	}
	// renderEventRow appends one event's cells to every parallel structure the
	// pane keeps — the table's rows, visibleRows, and the search rows — so the
	// two places a row can render from (its own loop turn, or its block's
	// expansion below) cannot drift apart. The exchange's tool rows follow it, and it
	// returns them.
	renderEventRow := func(j int) []toolRow {
		cc := ctxs[j]
		row := make(table.Row, 0, len(cols))
		for _, c := range cols {
			// A measured column's cell was evaluated for this row by measureColumns,
			// so it is aligned and shown rather than evaluated a second time.
			if vs, ok := measured[c.id]; ok {
				row = append(row, c.align(vs[j]))
				continue
			}
			// render, not cell: it sets cc.width from the column AND applies the
			// column's declared alignment, which is the same field tableColumns pads
			// the heading with. A cell that aligned itself could not tell the header,
			// and for the numeric columns it never did.
			row = append(row, c.render(cc))
		}
		rows = append(rows, row)
		m.visibleRows = append(m.visibleRows, cc.row)
		if q != "" {
			full = append(full, eventSearchRow(cols, droppedCols, cc))
		}
		ts := toolsOf(cc.row)
		for i := range ts {
			tr := eventRow{tool: &ts[i], last: i == len(ts)-1}
			cells, sr := toolCells(cols, cc, tr, sortCol != nil, now)
			rows = append(rows, cells)
			m.visibleRows = append(m.visibleRows, tr)
			if q != "" {
				full = append(full, sr)
			}
		}
		return ts
	}
	for j, cc := range ctxs {
		// hideInactive (the `s` toggle) is off by default — every message is
		// shown, including passthrough/skip-only ones, per "I should see all
		// network messages". Turning it on focuses the timeline on plugin
		// activity (deny/modify/observe/allow). It hides whole exchanges: a row whose
		// partner is shown is shown too, or a routed request refused for its model —
		// no plugin acts on the 403 — would look as if it never got an answer.
		// The predicate is rowInactiveHidden, shared with collectSubagentBlocks
		// so the two filter the same rows.
		if m.rowInactiveHidden(ctxs, partner, j) {
			m.hiddenInactive++
			continue
		}

		// Subagent blocks: a row naming an agent joins that agent's block, whose
		// header sits at the block's first surviving row. Collapsed (the default,
		// and always while no search is live) the row is not rendered at all — the
		// header's rollup is what a collapsed block says. Expanded, the block's
		// rows render TOGETHER, directly under the header, pulled out of the
		// chronological interleaving with the main conversation — at their own
		// timestamps they scatter across the pane (a subagent's exchange spans
		// several main-conversation turns), and the rows under the header are the
		// main conversation's, which reads as the expansion doing nothing. The
		// timestamp and # columns still carry where each row sat. m.visibleRows
		// keeps an empty eventRow for the header so it stays parallel to the
		// rendered rows; headerRows is what says a rendered row is a header.
		if b := m.subagentBlocks[cc.row.event.Subagent]; b != nil {
			if !b.anchored {
				b.anchored = true
				text := b.headerText(m.subagentLabel(b.id), m.blockOpen(b.id, q != ""))
				rows = append(rows, table.Row{text})
				m.visibleRows = append(m.visibleRows, eventRow{})
				if m.headerRows == nil {
					m.headerRows = make(map[int]*subagentBlock)
				}
				m.headerRows[len(rows)-1] = b
				if q != "" {
					full = append(full, table.SearchRow{Cells: []string{text}})
				}
				if m.blockOpen(b.id, q != "") {
					// b.rows holds exactly the member rows that survived the
					// inactive filter, in arrival order — the same population the
					// loop below renders — so nothing is double-counted or skipped.
					for _, mj := range b.rows {
						renderEventRow(mj)
					}
				}
			}
			// The member's cells went out with its block's header — or nowhere,
			// while the block is collapsed. Either way not here.
			continue
		}

		ts := renderEventRow(j)
		// Keyed from the SAME cellContext that just rendered the row, so the value
		// sorted on and the value displayed cannot come apart. cc.width stays zero
		// here — render takes its own copy — and no sortKey reads it.
		//
		// Each row is one exchange (#1681), so exchange is the row's own arrival
		// index. j indexes ctxs, which is every row before hideInactive drops any.
		// The sort never reads it as a position in `keys` — only compares two of
		// them — so the filter's gaps are harmless.
		//
		// Member rows take the block branch above and never reach this — safe
		// because blocks exist only while no sort is active (see the collection
		// above), so a row the sort needs cannot be skipped by a block.
		if sortCol != nil {
			k := sortCol.sortKey(cc)
			keys = append(keys, rowKey{val: k, exchange: j})
			for i := range ts {
				keys = append(keys, rowKey{val: toolSortKey(sortCol.id, ts[i], k.numeric, now), exchange: j})
			}
		}
	}
	// Sort the OUTPUT, never the input. buildEventRows' tunnel fold, the backward
	// walk in computeEventPairs and mergeExchanges all require arrival order, and
	// every one of them has already run by the time we get here — so reordering the
	// finished rows leaves the exchange numbering and the merged figures exactly as
	// they were.
	if sortCol != nil {
		sortEventRows(rows[lead:], m.visibleRows[lead:], full[min(lead, len(full)):], keys, m.sortDesc)
	}
	// Columns are re-set only when they actually differ, and in an order that keeps
	// every intermediate state renderable.
	//
	// SetColumns and SetRows each call UpdateViewport, which re-renders whatever rows
	// are loaded against whatever columns are set, and bubbles' renderRow walks the
	// ROW's cells while indexing m.cols[i] — so a row with more cells than there are
	// columns reads past the end and panics ("index out of range [10] with length
	// 10"). Fewer cells than columns is harmless. So when the new set has fewer
	// columns the new rows go in first, and otherwise the new columns do: at no step
	// is a loaded row longer than the columns it is rendered against.
	//
	// The order replaced a SetRows(nil) before SetColumns, which avoided the panic
	// but also emptied the viewport's content, resetting its offset to 0 and parking
	// the cursor at −1: the scroll position was GONE before the restore below could
	// preserve it. Columns change on three triggers — a column toggled, the terminal
	// resized past a fit boundary, and a fitsContent column (TOKENS, COST) widening
	// or narrowing as figures arrive, which can push a column off the terminal or
	// let one back on. The third happens mid-stream with no keystroke behind it, so
	// none of them may re-anchor the pane under an operator who is reading it.
	newCols := tableColumns(cols, m.sortCol, m.sortDesc)
	switch oldCols := m.eventsTbl.Columns(); {
	case slices.Equal(oldCols, newCols):
		m.eventsTbl.SetRows(rows)
	case len(newCols) < len(oldCols):
		m.eventsTbl.SetRows(rows)
		m.eventsTbl.SetColumns(newCols)
	default:
		m.eventsTbl.SetColumns(newCols)
		m.eventsTbl.SetRows(rows)
	}
	m.eventsTbl.SetSearch(q, full)

	// Auto-follow: if user was at the bottom, stay at the bottom. Otherwise
	// preserve position so reading isn't disturbed by new events.
	//
	// Unconditional now, and through setCursorVisible rather than SetCursor. Two
	// reasons, both about the highlight rather than the index:
	//
	//   - SetCursor does not reconcile the viewport's offset, so restoring any row
	//     at or past one screenful left the cursor one line below the rendered
	//     window. On a live session that meant the highlight disappeared on the very
	//     next event, for every session long enough to scroll. setCursorVisible
	//     carries the mechanics.
	//   - the old `else if prevRow < len(rows)` skipped the restore entirely when
	//     the rows SHRANK under the cursor — a filter typed, hideInactive toggled.
	//     SetRows had clamped the index by then, so the cursor was left wherever
	//     that landed, with an offset nobody reconciled.
	//
	// setCursorVisible leaves the scroll offset alone when the cursor is already on
	// the target row — which on a pane rebuilt by a two-second poll is the common
	// case, and is what keeps the poll from scrolling the rows out from under a
	// reader who had arrowed back up from the tail.
	//
	// Restore precedence: tail-follow, then identity pin, then prevRow. The pin
	// resolves to whatever row currently holds the selected event, so the cursor
	// tracks the event through FIFO eviction at session.max_events.
	// Seed the pin when a sort is active and nothing has pinned yet.
	//
	// Suppressing tail-follow below (see the next comment) removes the only arm that
	// fires when no event is pinned — and nothing in production sets selectedEventKey
	// until the operator moves the cursor, so a sort restored from the config file is
	// live on the very first rebuild with no keypress behind it. Without this, restore
	// falls through to a positional prevRow while every arriving event reshuffles the
	// rows underneath it, and the cursor lands on a different event each time with
	// nothing on screen to explain it.
	//
	// Seeded from the row the cursor is on right now, so the operator keeps whatever
	// the previous rebuild left them looking at. Only when the key is still
	// zero-valued: a real pin, including one the eviction branch below deliberately
	// cleared, must not be overwritten.
	if m.sortCol != "" && m.selectedEventKey == (eventKey{}) {
		m.selectedEventKey = pinKeyOf(m.selectedRowOn(prevRow))
	}

	// An OPENING, not a refresh: the session under the table changed. This is the
	// one rebuild that gets to choose an end, because there is no position to
	// preserve yet — every later one (the poll, a search, a column toggle) must
	// leave the operator where they are.
	//
	// LATCHED ONLY ONCE THERE ARE ROWS, which is the whole subtlety. Entering a
	// session calls rebuildEventsTable BEFORE snapshotCmd returns (keys.go), so for
	// any session whose history is not already cached the first rebuild has zero
	// rows. Latching on that one made the post-snapshot rebuild a "refresh", the
	// preference was skipped, and tail-follow placed the cursor on the newest row —
	// silently ignoring the setting in exactly the case it exists for, a
	// pre-existing session opened to read from the oldest end.
	//
	// A table with no rows has no end to land on anyway, so deferring costs nothing.
	// A session that stays empty simply never latches, and every rebuild of it is a
	// harmless no-op opening.
	opening := m.eventsBuiltFor != m.selectedSess
	if len(rows) > 0 {
		m.eventsBuiltFor = m.selectedSess
	}

	target := prevRow
	switch {
	// Where a session opens: the end the operator last jumped to with g/G. Only
	// while chronological — under a sort the row order is a ranking, so "an end"
	// means the largest or smallest value rather than the oldest or newest event,
	// and the selectedEventKey pin below is what carries the cursor there.
	case opening && m.sortCol == "":
		target = openAtRow(len(rows), Settings.Events.OpenAtOldest)
	// Tail-follow only while the table is chronological. "Stay at the bottom" means
	// "follow the newest event" ONLY because the last row is the newest one; under a
	// DURATION or COST sort the last row is the smallest value, so following it would
	// drag the cursor to a different event on every streamed message — and away from
	// the large-value end the operator sorted to look at. With a sort active the
	// selectedEventKey pin above carries the cursor instead, which is what the
	// operator actually wants followed.
	case wasAtEnd && m.sortCol == "":
		target = len(rows) - 1
		// Move a tool pin to the new last row: a pin left on an older tool row pulls the cursor
		// back to it when a later timeline lands, and ends tail-follow.
		if m.selectedEventKey.isTool {
			m.selectedEventKey = pinKeyOf(m.selectedRowOn(target))
		}
	case m.selectedEventKey != (eventKey{}):
		if idx := findByKey(m.visibleRows, m.selectedEventKey); idx >= 0 {
			target = idx
		} else if !m.selectedEventKey.isTool || m.toolsLoaded() {
			// Event evicted — clamp to the oldest surviving row,
			// the nearest edge to where the pin was, and drop the stale
			// key so internal state matches what the operator sees.
			target = lead
			m.selectedEventKey = eventKey{}
		}
	}
	setCursorVisible(&m.eventsTbl, target)
}

// toolsLoaded reports that the events pane's session has its timeline, or that its fetch
// failed. Until then a tool pin that matches no row is kept.
func (m *model) toolsLoaded() bool {
	return m.timeline.session == m.selectedSess && (m.timeline.tl != nil || m.timeline.err != nil)
}

// sortEventRows reorders the rendered rows by their sort keys (#865).
//
// Permutes rows and visible AS A UNIT, because the table cursor indexes both:
// selectedEventRow reads m.visibleRows[cursor] to answer what the detail pane and
// yank operate on, so letting the two drift would show the operator one row and
// act on another.
//
// SliceStable, not Slice: ties keep arrival order. That matters most on the
// columns worth sorting — a session's DURATION column is mostly rows with no
// duration at all, and an unstable sort would shuffle them on every rebuild, i.e.
// on every streamed event.
//
// Sorts an index permutation rather than swapping three slices in the comparator,
// so each row's key travels with it without the comparator having to reorder keys
// as it reads them.
func sortEventRows(rows []table.Row, visible []eventRow, full []table.SearchRow, keys []rowKey, desc bool) {
	if len(rows) != len(keys) || len(rows) != len(visible) {
		// Defensive: a length mismatch means the loop above stopped appending to one
		// of the three in step. Sorting on a short key slice would panic; leaving the
		// order alone degrades to chronological, which is always a valid view.
		return
	}
	idx := make([]int, len(rows))
	for i := range idx {
		idx[i] = i
	}
	// THREE TIERS, and only the first one reverses.
	//
	//  1. the column's key, flipped for descending — what the operator asked for.
	//  2. the exchange, ascending always. Each row is one exchange (#1681), so
	//     exchange is the row's own arrival index. Ties keep arrival order.
	//  3. the row's position, ascending always.
	//
	// Tiers 2 and 3 do NOT flip. Descending flips which exchange comes first, never
	// the order of rows that tie.
	//
	// Both tiers compare arrival indexes without indexing anything by them, so the
	// gaps hideInactive leaves in that numbering cost nothing.
	sort.SliceStable(idx, func(a, b int) bool {
		ia, ib := idx[a], idx[b]
		ka, kb := keys[ia], keys[ib]
		switch {
		case ka.val.less(kb.val):
			return !desc
		case kb.val.less(ka.val):
			return desc
		case ka.exchange != kb.exchange:
			return ka.exchange < kb.exchange
		default:
			return ia < ib
		}
	})

	outRows := make([]table.Row, len(rows))
	outVis := make([]eventRow, len(visible))
	for newPos, oldPos := range idx {
		outRows[newPos] = rows[oldPos]
		outVis[newPos] = visible[oldPos]
	}
	copy(rows, outRows)
	copy(visible, outVis)
	// The search's full values move with their rows, or they would describe the rows that
	// now sit where theirs were.
	if len(full) == len(rows) {
		outFull := make([]table.SearchRow, len(full))
		for newPos, oldPos := range idx {
			outFull[newPos] = full[oldPos]
		}
		copy(full, outFull)
	}
}

// selectedRowOn returns the row at an arbitrary row of the rows just built, or the zero
// eventRow when the row is out of range.
//
// Distinct from selectedEventRow, which reads the TABLE's cursor: during a rebuild the
// table has already been handed the new rows but the cursor has not been restored
// yet, so the only meaningful row index is the one carried over from before the
// rebuild. Used to seed the sort pin from that row.
func (m *model) selectedRowOn(row int) eventRow {
	if row < 0 || row >= len(m.visibleRows) {
		return eventRow{}
	}
	return m.visibleRows[row]
}

// selectedEvent returns the event at the cursor row, or nil. The cursor
// points into m.visibleRows (one entry per rendered message), and each row
// carries a reference to its source event.
func (m *model) selectedEvent() *pipeline.SessionEvent {
	er, ok := m.selectedEventRow()
	if !ok {
		return nil
	}
	return er.event
}

// selectedEventRow returns the full row (event + any folded tunnel) under the
// cursor. ok is false when there are no rendered rows or the cursor is out of
// range.
func (m *model) selectedEventRow() (eventRow, bool) {
	if len(m.visibleRows) == 0 {
		return eventRow{}, false
	}
	cur := m.eventsTbl.Cursor()
	if cur < 0 || cur >= len(m.visibleRows) {
		return eventRow{}, false
	}
	return m.visibleRows[cur], true
}

// buildEventRows turns the chronological event slice into display rows, one
// per network message. The only folding is the TLS-bridge CONNECT pair: a
// tunnel-open event (opaque, host:port) immediately followed by the decrypted
// inner row for the same host — a request, or a denial when a plugin rejected
// it before any request row — is rendered as a single row keyed on the
// inner row, with the tunnel attached for the detail pane. A non-bridged
// (passthrough) tunnel has no inner row following it, so it stands as its
// own row — it IS the whole message.
func buildEventRows(events []pipeline.SessionEvent) []eventRow {
	closed := closedTunnels(events)
	rows := make([]eventRow, 0, len(events))
	for i := 0; i < len(events); i++ {
		e := &events[i]
		if i+1 < len(events) && isTunnelOpen(e) && !stayedOpaque(e, closed) {
			inner := &events[i+1]
			if isBridgedInner(e, inner) {
				rows = append(rows, eventRow{event: inner, tunnel: e})
				i++ // consume the inner request too
				continue
			}
		}
		rows = append(rows, eventRow{event: e})
	}
	return rows
}

// mergeExchanges folds each response or denial into the row of the request it answers
// (#1681): one row per exchange, at the request's position, so a response changes its row
// instead of adding one.
//
// ONLY AN EXACT PAIR MERGES — one partner joined by RequestID. The heuristic pass in
// computeEventPairs can pair a response with another call's request, and a merged row would
// then show that call's status and duration as this one's. A guessed pair stays two rows.
//
// Returns the merged rows and the partner map re-indexed onto them. Only guessed pairs are
// left in it, which is what rowInactiveHidden still needs to keep such a pair together.
func mergeExchanges(rows []eventRow, partner map[int]int) ([]eventRow, map[int]int) {
	out := make([]eventRow, 0, len(rows))
	at := make(map[int]int, len(rows)) // index in rows → index in out
	for i, r := range rows {
		if j, ok := partner[i]; ok && exactPair(rows, i, j) {
			if r.event.Phase != pipeline.SessionRequest {
				continue // it went out with its request
			}
			r.resp = rows[j].event
		}
		at[i] = len(out)
		out = append(out, r)
	}
	left := make(map[int]int)
	for i, j := range partner {
		if !exactPair(rows, i, j) {
			left[at[i]] = at[j]
		}
	}
	return out, left
}

// exactPair reports that rows i and j share a RequestID: the pair computeEventPairs made from
// the proxy's stamp, not from its guess.
func exactPair(rows []eventRow, i, j int) bool {
	a, b := rows[i].event, rows[j].event
	return a.RequestID != "" && a.RequestID == b.RequestID
}

// answer is the event that holds row r's outcome — STATUS, DURATION, the response's bytes:
// the merged response when there is one, else the row's own event (a denial, a response
// whose request is not held, or a request no answer reached yet).
func answer(r eventRow) *pipeline.SessionEvent {
	if r.resp != nil {
		return r.resp
	}
	return r.event
}

// exchangeResp is the response that speaks for row r: the merged one, or the row's own event
// when the row is a response. Nil while no answer arrived. A guessed pair's request row gets
// nil too, because its response stays on a row of its own.
func exchangeResp(r eventRow) *pipeline.SessionEvent {
	if r.resp != nil {
		return r.resp
	}
	if r.event != nil && r.event.Phase == pipeline.SessionResponse {
		return r.event
	}
	return nil
}

// exchangeBytes is what row r carried: the request's body up and the response's down. An
// opaque tunnel's close row counts both directions itself, so it answers alone.
func exchangeBytes(r eventRow) (up, down int64) {
	if r.resp == nil || r.resp.Tunnel {
		a := answer(r)
		return a.BytesUp, a.BytesDown
	}
	return r.event.BytesUp, r.resp.BytesDown
}

// lostRef is the instant the lost rule measures against: the later of now and the newest
// event held. It is the ref Timeline.LoseUnanswered uses, so the two panes call the same
// request lost. The newest event matters when the clock is behind the session's clock. The
// README demo runs a shifted clock.
func (m *model) lostRef(events []pipeline.SessionEvent) time.Time {
	ref := m.clock()
	for i := range events {
		if events[i].At.After(ref) {
			ref = events[i].At
		}
	}
	return ref
}

// rowLost reports that row i is a request no answer will reach (#1681): no response, and
// older than timeline.HostLostAfter at ref. On real traffic 52 such requests sat among
// answered ones, and a blank STATUS would have said "on its way" forever.
//
// A tunnel open is never lost. It carries no status, and a passthrough tunnel stays open for
// hours (6.4 hours measured), so age says nothing about it. A guessed pair's request is not
// lost either: its response is on a row of its own. A guessed pair can only form around a
// request with no RequestID, so such a request is never lost here. The Timeline judges
// outbound requests with a host only, so an inbound or hostless one is never lost here either.
func rowLost(r eventRow, partner map[int]int, i int, ref time.Time) bool {
	e := r.event
	if r.resp != nil || e == nil || e.Direction != pipeline.Outbound || e.Host == "" ||
		e.Phase != pipeline.SessionRequest || e.Tunnel || e.RequestID == "" {
		return false
	}
	if _, ok := partner[i]; ok {
		return false
	}
	return ref.Sub(e.At) > timeline.HostLostAfter
}

// isTunnelOpen reports whether e is a CONNECT / transparent-redirect
// tunnel-open. It keys on the explicit Tunnel marker the producer
// (recordTunnelOpened) sets — NOT on host/extension shape, which an ordinary
// unparsed outbound request could mimic and get wrongly folded.
//
// The REQUEST phase as well as the marker: a tunnel's close row carries Tunnel too,
// and it is an exchange's response, not a CONNECT waiting for its decrypted request.
// Keyed on the marker alone, a close followed by a request to the same host folded
// that request into itself and the close vanished from the timeline.
func isTunnelOpen(e *pipeline.SessionEvent) bool {
	return e.Tunnel && e.Phase == pipeline.SessionRequest
}

// isTunnelClose reports whether e is the row the proxy records when a tunnel ends.
func isTunnelClose(e *pipeline.SessionEvent) bool {
	return e.Tunnel && e.Phase == pipeline.SessionResponse
}

// stayedOpaque reports whether open's tunnel is known to have carried no decrypted
// request: its reason says why the bytes stayed opaque, or events hold a close for it,
// which the proxy records only for a tunnel no decrypted request answered. The reason
// alone is not enough: the transparent listener records a reachable destination's open
// with no reason, whether or not the bytes then stay opaque.
func stayedOpaque(open *pipeline.SessionEvent, closed map[string]bool) bool {
	return open.TunnelReason != "" || (open.RequestID != "" && closed[open.RequestID])
}

// closedTunnels is the RequestIDs events holds a tunnel close for.
func closedTunnels(events []pipeline.SessionEvent) map[string]bool {
	var out map[string]bool
	for i := range events {
		if e := &events[i]; isTunnelClose(e) && e.RequestID != "" {
			if out == nil {
				out = make(map[string]bool)
			}
			out[e.RequestID] = true
		}
	}
	return out
}

// isBridgedInner reports whether inner is the decrypted request the TLS bridge
// produced right after opening tunnel. The two fold into one row: tunnel
// carries "host:port" + opaque bytes, inner carries the same host (the Host
// header, usually port-stripped) plus the real method/path. Matching on the
// host-part (port-insensitive) handles both the standard-443 case (inner host
// = "example.com") and the non-standard case (inner host = "example.com:8443").
//
// A redirected inner request also folds: a plugin (inference-router) may have
// moved it to another host after decryption, so inner.Host is the dial target
// and the host the client named — the CONNECT's target — is RequestedHost.
//
// Two guards keep unrelated events from folding:
//   - inner must be another outbound REQUEST or DENIAL, never a response, so a
//     plain request→response exchange isn't mistaken for a bridged pair. A
//     denial folds too: the proxy defers a bridged open and records it with
//     whichever row comes first, and a pipeline plugin's Reject can land
//     first.
//   - inner must NOT itself be a tunnel-open (Tunnel marker). Two back-to-back
//     passthrough CONNECTs to the same host (common with connection pooling)
//     would otherwise fold — hiding one real message and mislabeling the other.
func isBridgedInner(tunnel, inner *pipeline.SessionEvent) bool {
	if inner.Direction != pipeline.Outbound ||
		(inner.Phase != pipeline.SessionRequest && inner.Phase != pipeline.SessionDenied) ||
		isTunnelOpen(inner) {
		return false
	}
	host := hostOnly(inner.Host)
	if host != "" && host == hostOnly(tunnel.Host) {
		return true
	}
	// RequestedHost is present only when a redirect moved the request, so this
	// arm cannot fold an unredirected request onto someone else's tunnel.
	return inner.RequestedHost != "" &&
		hostOnly(inner.RequestedHost) == hostOnly(tunnel.Host)
}

// hostOnly returns the host portion of a "host:port" string, or the input
// unchanged when it carries no port. Handles bracketed IPv6 literals.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// allInvocations returns every plugin invocation on an event, both
// directions concatenated (inbound first). Returns nil when the event
// carries no Invocations.
func allInvocations(e *pipeline.SessionEvent) []pipeline.Invocation {
	if e == nil || e.Invocations == nil {
		return nil
	}
	out := make([]pipeline.Invocation, 0, len(e.Invocations.Inbound)+len(e.Invocations.Outbound))
	out = append(out, e.Invocations.Inbound...)
	out = append(out, e.Invocations.Outbound...)
	return out
}

// actionRank orders the five invocation verbs for at-a-glance aggregation,
// highest wins: deny > modify > observe > allow > skip. The pipeline's own
// outcome only distinguishes deny vs allow (pipeline/outcome.go); the rest of
// this ordering is an agentop display choice. deny/modify rank top because they
// changed the message's fate. observe ranks ABOVE allow on purpose: a parser
// that understood the message (and supplied the METHOD shown on the row) tells
// the operator more than a gate that merely permitted it — and surfacing the
// gate while METHOD came from the parser reads as inconsistent. skip is the
// floor (a plugin ran but didn't apply); 0 is the sentinel for "no plugin
// acted".
func actionRank(a pipeline.InvocationAction) int {
	switch a {
	case pipeline.ActionDeny:
		return 5
	case pipeline.ActionModify:
		return 4
	case pipeline.ActionObserve:
		return 3
	case pipeline.ActionAllow:
		return 2
	case pipeline.ActionSkip:
		return 1
	}
	return 0
}

// topInvocation returns the highest-ranked invocation satisfying keep and how many distinct
// plugins tied at that top rank. Distinct, because a row is an exchange (#1681): the same
// two plugins observe both halves of most inference calls, and counting invocations read
// "4 plugins". winner is the zero Invocation (Action "", rank 0) when none satisfy keep.
func topInvocation(invs []pipeline.Invocation, keep func(pipeline.Invocation) bool) (pipeline.Invocation, int) {
	best := -1
	var winner pipeline.Invocation
	var names []string
	for _, iv := range invs {
		if !keep(iv) {
			continue
		}
		switch r := actionRank(iv.Action); {
		case r > best:
			best, winner, names = r, iv, []string{iv.Plugin}
		case r == best && !slices.Contains(names, iv.Plugin):
			names = append(names, iv.Plugin)
		}
	}
	return winner, len(names)
}

// shadowFlagged reports whether any invocation is a shadow deny/modify — a
// plugin that ran under on_error: observe and WOULD have blocked or rewritten
// the message, but didn't (the framework converted its Reject to a pass and
// set Shadow). These are the signal a shadow-policy rollout is watching for.
func shadowFlagged(invs []pipeline.Invocation) bool {
	for _, iv := range invs {
		if iv.Shadow && (iv.Action == pipeline.ActionDeny || iv.Action == pipeline.ActionModify) {
			return true
		}
	}
	return false
}

// actionColWidth is the ACTION column's width. Named rather than inlined in the
// column literal so a test can assert against the real value: transcribing it
// into the test gave two independent 8s, and narrowing the column left the test
// passing while the label overflowed.
const actionColWidth = 8

// methodColWidth is the METHOD column's width, shared with eventMethod for the
// same reason actionColWidth is named: two independent numbers drifted apart the
// moment the column was narrowed to pay for the TOKENS/COST split.
//
// 18 rather than 14. The values reaching it are wider than a short model alias:
// eventMethodValue also returns A2A and MCP method names
// ("notifications/initialized", 25) and dated provider model IDs
// ("claude-sonnet-4-5-20250929", 26). At 14 both "claude-sonnet-4-5-20250929"
// and "claude-sonnet-4-20250514" render as "claude-sonnet…", which makes the
// column unable to say which model the COST cell beside it is reporting — the
// one thing it most needs to disambiguate now that costs are per-row.
const methodColWidth = 18

// tunnelAction is the ACTION cell for an opaque CONNECT that no plugin acted on.
//
// It must fit actionColWidth. Truncation matters more here than on other rows:
// METHOD and STATUS are both empty for an unbridged CONNECT, so a clipped
// "tunne" would take the row back to unreadable — the state this label exists to
// fix.
const tunnelAction = "tunnel"

// rowAction is the ACTION + PLUGIN pair for one display row.
//
// It wraps eventAction to name an unbridged CONNECT. Such a row carries TLS bytes,
// so no plugin ran, no protocol was parsed and there is no status — left as "— —"
// it reads as a request that failed or that the pipeline ignored, which is how a
// routine egress tunnel came to look like a bug.
//
// The label applies only when nothing acted: a gate CAN deny a CONNECT on the
// tunnel-open itself, and that deny must keep the headline. A BRIDGED tunnel never
// reaches this branch — buildEventRows folds it into the decrypted inner request,
// whose own action is the interesting one.
//
// invs is passed in rather than derived from er, so the headline is computed from
// the same set the caller's visibility decision used. Deriving it again would make
// that relationship implicit and silently divergent: were the call site ever to
// filter invs — to honour a plugin-name filter in the hide logic, say — the ACTION
// cell would keep using the unfiltered set and disagree with the row's own reason
// for being visible.
func rowAction(er eventRow, invs []pipeline.Invocation) (action, plugin string) {
	action, plugin = eventAction(invs)
	if er.event != nil && er.event.Tunnel && action == "—" {
		// The PLUGIN cell carries the REASON on these rows. No plugin ran — that
		// is what makes the row a tunnel — so the cell would otherwise be a
		// second em dash beside the first, on the one row type that needs an
		// explanation most. "tunnel" alone cannot distinguish a passthrough
		// working as designed from a client that rejected the bridge CA, and
		// those demand opposite responses.
		return tunnelAction, tunnelReasonCell(er.event.TunnelReason)
	}
	return action, plugin
}

// tunnelReasonCell renders SessionEvent.TunnelReason for the PLUGIN column.
//
// Reasons are already short, kebab-case and stable, so they are shown verbatim
// rather than prettified: an operator grepping the proxy log for the same string
// should find the same token. An unknown reason is passed through untouched — a
// newer proxy paired with an older agentop should show the new reason, not hide it.
func tunnelReasonCell(reason pipeline.TunnelReason) string {
	if reason == "" {
		return "—"
	}
	return string(reason)
}

// eventAction folds a message's per-plugin invocations into the single ACTION +
// PLUGIN cell pair shown in the timeline. The headline reflects what actually
// took effect:
//
//   - The winner is the highest-ranked ENFORCED (non-shadow) invocation —
//     deny > modify > observe > allow > skip (see actionRank). A shadow
//     deny/modify did NOT take effect (outcome.go enforces deny only when
//     !Shadow), so it must not headline over the action that really applied.
//     PLUGIN names that plugin, or "N plugins" when several tie at the top.
//   - A trailing "*" flags that a shadow policy would have blocked/changed the
//     message (e.g. "allow*"). Lets a rollout stay scannable without claiming
//     a block that never happened.
//   - When nothing enforced acted above a skip, a shadow deny/modify — if
//     present — becomes the headline ("deny*"), since it's the only signal
//     worth surfacing (a lone shadow deny on an otherwise-passthrough request).
//   - Otherwise "—  —": nothing meaningful happened. A skip never credits a
//     plugin (naming a skipper like "token-exchange" on an unrelated host
//     reads as if it processed the message). Per-plugin detail is on drill-in.
func eventAction(invs []pipeline.Invocation) (action, plugin string) {
	winner, count := topInvocation(invs, func(iv pipeline.Invocation) bool { return !iv.Shadow })
	shadow := shadowFlagged(invs)

	if actionRank(winner.Action) > actionRank(pipeline.ActionSkip) {
		action = string(winner.Action)
		if shadow {
			action += "*"
		}
		if count == 1 {
			plugin = winner.Plugin
		} else {
			plugin = fmt.Sprintf("%d plugins", count)
		}
		return action, plugin
	}

	if shadow {
		sh, _ := topInvocation(invs, func(iv pipeline.Invocation) bool { return iv.Shadow })
		return string(sh.Action) + "*", sh.Plugin
	}

	return "—", "—"
}

// eventInactive reports whether no plugin took a meaningful action — either no
// invocations at all (a pure passthrough / unprocessed message) or every
// invocation was a skip (plugins ran but none matched). A shadow deny/modify
// counts as activity (it flags a would-have-blocked message worth keeping
// visible during a rollout). The `s` key hides inactive messages so an
// operator can focus on plugin activity.
func eventInactive(invs []pipeline.Invocation) bool {
	if len(invs) == 0 {
		return true
	}
	for _, iv := range invs {
		if iv.Action != pipeline.ActionSkip {
			return false
		}
	}
	return true
}

func shortDirection(d pipeline.Direction) string {
	if d == pipeline.Inbound {
		return "in"
	}
	return "out"
}

// eventMethodValue is the raw, untruncated method/model for an event — the A2A
// method, inference model, or MCP method. Used for logic (pairing, filtering)
// where truncation would conflate distinct names sharing a 22-char prefix or
// hide searchable suffixes.
func eventMethodValue(e pipeline.SessionEvent) string {
	switch {
	case e.A2A != nil:
		return e.A2A.Method
	case e.Inference != nil:
		return e.Inference.Model
	case e.MCP != nil:
		return e.MCP.Method
	}
	return ""
}

// eventMethod is the display form of the method/model — truncated to the
// METHOD column width. Render-only; never compare or search on it.
//
// Shares methodColWidth with the column definition rather than repeating the
// number, the same drift guard actionColWidth provides for ACTION: the two were
// already 22 and 14 after the column shrank, which bubbles hid by re-truncating
// each cell at the column width anyway.
func eventMethod(e pipeline.SessionEvent) string {
	return trunc(eventMethodValue(e), methodColWidth)
}

func statusCell(e pipeline.SessionEvent) string {
	if e.StatusCode == 0 {
		return ""
	}
	return fmt.Sprintf("%d", e.StatusCode)
}

// bytesCell renders a row's byte counts, up (client to destination) then down.
//
// A tunnel's close row is the one that reports BOTH, and it keeps the pair rendering
// even where one side is zero: the counts are absent on the wire when zero, so a
// genuine zero one way is shown only once the other side proves the row carries
// counts at all. A merged row reports both too (exchangeBytes): the request's ↑ and
// the response's ↓. Each of its sides shows only when it is not zero. A row with one
// half reports a single side — a request knows what it forwarded, a response what
// came back — so printing the pair there would put a fabricated "↓0B" next to every
// request size.
//
// Blank when nothing was counted, which is NOT the same as a body of zero bytes:
// an unbuffered body is relayed unmeasured and reads here exactly as a body-less GET
// does. See pipeline.SessionEvent.BytesUp — formatBytes renders 0 as "0B", never
// blank, so the decision is this gate's alone.
func bytesCell(e pipeline.SessionEvent) string {
	if e.BytesUp == 0 && e.BytesDown == 0 {
		return ""
	}
	if e.Tunnel {
		return "↑" + formatBytes(e.BytesUp) + " ↓" + formatBytes(e.BytesDown)
	}
	var parts []string
	if e.BytesUp > 0 {
		parts = append(parts, "↑"+formatBytes(e.BytesUp))
	}
	if e.BytesDown > 0 {
		parts = append(parts, "↓"+formatBytes(e.BytesDown))
	}
	return strings.Join(parts, " ")
}

// formatBytes renders a byte count in decimal units, 1kB being 1000 bytes. Like
// formatCompact it promotes where the rounding carries, so no tier prints "1000.0".
func formatBytes(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%dB", n)
	}
	units := [...]string{"kB", "MB", "GB", "TB", "PB", "EB"}
	v, i := float64(n)/1000, 0
	for v >= 999.95 && i < len(units)-1 {
		v, i = v/1000, i+1
	}
	return fmt.Sprintf("%.1f%s", v, units[i])
}

// durationCell renders a duration at the precision a reader wants for its size:
// milliseconds, then seconds to the hundredth, then minutes and seconds, then hours
// and minutes. The last two are for tunnels, whose DURATION is how long they stayed
// open — "252.00s" for a `kubectl logs -f` makes the reader do the division.
//
// Seconds hand over to minutes at 59.995s rather than 60s, because %.2f rounds
// 59.999s up to "60.00s", a seconds figure that is already a minute.
func durationCell(e pipeline.SessionEvent) string {
	return formatDuration(e.Duration)
}

// formatDuration is durationCell's body, split out so the detail pane's timing
// header can format ttfb and the body's share of a response at the same
// precision the DURATION column uses — one rule for the same unit, everywhere
// a reader sees it.
func formatDuration(d time.Duration) string {
	if d == 0 {
		return ""
	}
	ms := d.Milliseconds()
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 59_995:
		return fmt.Sprintf("%.2fs", float64(ms)/1000)
	}
	secs := (ms + 500) / 1000
	if secs < 3600 {
		return fmt.Sprintf("%dm%02ds", secs/60, secs%60)
	}
	mins := (ms + 30_000) / 60_000
	return fmt.Sprintf("%dh%02dm", mins/60, mins%60)
}

// padLeft right-aligns s in a field of the given display width. Uses
// lipgloss.Width (matching padRight in help_overlay.go) so the saving sign
// (U+2212, 3 bytes / 1 column) in TOKENS/COST cells doesn't cause padding to
// mis-count. An empty cell stays blank.
func padLeft(s string, width int) string {
	w := lipgloss.Width(s)
	if s == "" || w >= width {
		return s
	}
	return strings.Repeat(" ", width-w) + s
}

// computeEventPairs matches each response row to its request row and returns
// two views of the result:
//
//   - ids: a small integer per event, shared across a (request, response)
//     exchange and freshly minted for unpaired rows. Drives the # column.
//   - partner: a bidirectional row-index map (partner[i]=j and partner[j]=i)
//     for matched pairs. mergeExchanges folds the exact ones into one row.
//
// Each response row is matched to the closest preceding unpaired request row
// sharing direction + host (port-normalized) + method. The method component
// keeps a fire-and-forget request (e.g. MCP notifications/initialized, which
// never gets a response) from stealing a later response that belongs to a
// different method.
//
// Pairing prefers SessionEvent.RequestID, which the proxy stamps on both the
// request and response event of the same exchange. That is exact, including
// under concurrency. A denial with the request's RequestID answers it too, as
// in the Timeline: the egress allow list refuses a redirect target after the
// request row landed.
//
// The closest-preceding heuristic below remains for events with no RequestID —
// an older proxy, or a listener that has not been taught to stamp it. It matches
// on direction + host (port-normalized) + method, and it cross-pairs when a
// client has concurrent same-host+method calls in flight. That is not
// hypothetical: Claude Code fires its session-title request alongside the main
// one, and the heuristic drew a 400 from the title request under the main
// request's row, which read as the pipeline plugin on that row having caused it.
//
// IDs are keyed by event pointer so the render loop can look one up without
// knowing the row index. They start at 1 and increment in first-seen row order
// so adjacent exchanges get adjacent integers.
func computeEventPairs(rows []eventRow) (map[*pipeline.SessionEvent]int, map[int]int) {
	partner := make(map[int]int) // row index → matched row index

	// Exact pass: pair by the proxy-stamped RequestID. Indexed by id so a
	// response finds its request regardless of how much traffic interleaves
	// between them.
	reqByID := make(map[string]int)
	for i := range rows {
		e := rows[i].event
		if e.RequestID == "" || e.Phase != pipeline.SessionRequest {
			continue
		}
		if _, dup := reqByID[e.RequestID]; !dup {
			reqByID[e.RequestID] = i
		}
	}
	for j := range rows {
		e := rows[j].event
		if e.RequestID == "" || (e.Phase != pipeline.SessionResponse && e.Phase != pipeline.SessionDenied) {
			continue
		}
		i, ok := reqByID[e.RequestID]
		if !ok {
			continue
		}
		if _, taken := partner[i]; taken {
			continue
		}
		partner[i] = j
		partner[j] = i
	}

	// Heuristic pass: only for rows the exact pass could not place.
	for j := range rows {
		rj := rows[j].event
		if rj.Phase != pipeline.SessionResponse {
			continue
		}
		if _, done := partner[j]; done {
			continue // already paired exactly by RequestID
		}
		if rj.RequestID != "" {
			// It carried an id and still did not pair — a second response for
			// the same request (a retry, or a streamed reply recorded twice).
			// Letting it fall through would have the heuristic walk back and
			// claim an unrelated earlier request, which is exactly the
			// mis-attribution the id was added to end. Leave it unpaired.
			continue
		}
		for i := j - 1; i >= 0; i-- {
			if _, taken := partner[i]; taken {
				continue
			}
			ri := rows[i].event
			if ri.Phase != pipeline.SessionRequest {
				continue
			}
			if ri.Direction != rj.Direction ||
				hostOnly(ri.Host) != hostOnly(rj.Host) ||
				eventMethodValue(*ri) != eventMethodValue(*rj) {
				continue
			}
			partner[i] = j
			partner[j] = i
			break
		}
	}

	ids := make(map[*pipeline.SessionEvent]int, len(rows))
	next := 0
	for i := range rows {
		e := rows[i].event
		if _, done := ids[e]; done {
			continue
		}
		if p, ok := partner[i]; ok {
			if pid, ok := ids[rows[p].event]; ok {
				ids[e] = pid
				continue
			}
		}
		next++
		ids[e] = next
	}
	return ids, partner
}

// identityBannerStyle renders the small bordered box above the events
// table. Rounded border matches the outer frame; muted color keeps the
// banner as context rather than competing with the event rows.
var identityBannerStyle = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(lipgloss.AdaptiveColor{Light: "#94A3B8", Dark: "#475569"}).
	Padding(0, 1)

// identityBannerHeight is the banner's height when nothing wraps: four lines of content plus
// two border lines.
//
// Not what layout reserves — identityBannerHeightFor measures the rendered banner instead.
// A constant was the whole defect: it stayed 6 no matter what the banner did, and the banner
// had no Width, so a long subject or several distinct callers produced a line 109 to 624
// columns long. lipgloss.Height still reported 6 (it counts newlines, and without a Width
// nothing wrapped), the accounting looked correct, and the TERMINAL wrapped the line into 1-7
// extra screen rows that the events table had already claimed. Rows fell off the bottom, which
// is indistinguishable from the cursor bug this pane also had.
const identityBannerHeight = 6

// olderNotice is the text of the timeline's first row when the session holds events the
// snapshot did not load. Enter on that row loads the page before them. Empty when nothing
// is missing. Plain text: the table truncates a row by its width in runes.
func (m *model) olderNotice() string {
	n := m.olderNotFetched[m.selectedSess]
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("▲ press Enter to load %d older event%s", n, plural(n))
}

// onOlderRow reports whether the events cursor is on the older-events row.
func (m *model) onOlderRow() bool {
	return m.eventsTbl.Cursor() == 0 && m.olderNotice() != ""
}

// loadOlderFromRow is Enter on the older-events row. It pins the cursor to the first row
// that has an event, so the page lands above it and the operator keeps their place. A
// subagent header has no event, so it cannot be the pin. It pins only when a load starts:
// a pin from a refused load moves the cursor on the next rebuild.
func (m *model) loadOlderFromRow() tea.Cmd {
	cmd := m.loadOlderPage()
	if cmd == nil {
		return nil
	}
	if i := slices.IndexFunc(m.visibleRows, func(r eventRow) bool { return r.event != nil }); i >= 0 {
		m.selectedEventKey = keyOf(m.visibleRows[i].event)
	}
	return cmd
}

// identityBannerHeightFor is how many rows layout must give up for the banner: the height of
// the thing that will actually be rendered, at the width it will be rendered at. Zero when
// there is no banner.
//
// Measured rather than declared, so the two cannot drift again. It costs one banner render per
// rebuild, against a session's worth of identities that are already deduped.
func identityBannerHeightFor(events []pipeline.SessionEvent, width int) int {
	banner := identityBanner(events, width)
	if banner == "" {
		return 0
	}
	return lipgloss.Height(banner)
}

// identityBanner renders a compact "IDENTITY" box summarizing the caller
// of this session's inbound events. If callers diverge across the
// session, it reports the count so the operator knows to check detail
// rows. Returns an empty string when no inbound identity is present
// (e.g. outbound-only buckets).
// width is the terminal width; every content line is truncated to what fits inside the border
// and padding. Without it the banner was unbounded — the multi-caller branch joins EVERY
// distinct subject into one line — and nothing downstream clips, so an over-wide line reached
// the terminal and wrapped. Truncating here keeps the banner exactly as tall as it claims.
func identityBanner(events []pipeline.SessionEvent, width int) string {
	idents := distinctInboundIdentities(events)
	if len(idents) == 0 {
		return ""
	}

	// The border costs one column each side and the style pads one more: four in total.
	inner := width - borderWidth - 2
	if inner < 8 {
		inner = 8
	}
	line := func(s string) string { return trunc(s, inner) }

	var b strings.Builder
	b.WriteString(styleTitle.Render("IDENTITY"))
	b.WriteByte('\n')

	if len(idents) == 1 {
		id := idents[0]
		b.WriteString(line(fmt.Sprintf("subject  %s", nonEmpty(id.Subject, "—"))) + "\n")
		b.WriteString(line(fmt.Sprintf("client   %s", nonEmpty(id.ClientID, "—"))) + "\n")
		b.WriteString(line(fmt.Sprintf("scopes   %s", nonEmpty(truncateScopes(id.Scopes, 3), "—"))))
	} else {
		// Multiple distinct callers — surface the count; detail rows
		// carry the full identity for drill-down.
		subjects := make([]string, 0, len(idents))
		for _, id := range idents {
			subjects = append(subjects, nonEmpty(id.Subject, "—"))
		}
		b.WriteString(line(fmt.Sprintf("subjects  %d distinct: %s",
			len(idents), strings.Join(subjects, ", "))) + "\n")
		b.WriteString("client    (see individual events)\n")
		b.WriteString("scopes    (see individual events)")
	}
	return identityBannerStyle.Render(b.String())
}

// identityKey is the comparable shape used to dedupe identities in the
// banner. Using a struct avoids string concatenation (and the theoretical
// "|" collision) — subject+clientID are the two fields that define a
// unique caller; scopes can legitimately vary turn-to-turn.
type identityKey struct {
	subject  string
	clientID string
}

// distinctInboundIdentities returns the unique EventIdentity values seen on
// inbound events, in first-seen order.
func distinctInboundIdentities(events []pipeline.SessionEvent) []*pipeline.EventIdentity {
	var out []*pipeline.EventIdentity
	seen := map[identityKey]bool{}
	for i := range events {
		e := &events[i]
		if e.Direction != pipeline.Inbound || e.Identity == nil {
			continue
		}
		k := identityKey{subject: e.Identity.Subject, clientID: e.Identity.ClientID}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, e.Identity)
	}
	return out
}

// truncateScopes joins the first n scopes with commas and appends a
// "+N more" suffix if the list was longer. Keeps the identity banner
// from overflowing the terminal when a caller has many scopes.
func truncateScopes(scopes []string, n int) string {
	if len(scopes) == 0 {
		return ""
	}
	if len(scopes) <= n {
		return strings.Join(scopes, ", ")
	}
	return strings.Join(scopes[:n], ", ") + fmt.Sprintf(" +%d more", len(scopes)-n)
}

// outputTokens is what a response generated. CompletionTokens is the older field for the same
// count.
func outputTokens(inf *pipeline.InferenceExtension) int {
	if inf.OutputTokens > 0 {
		return inf.OutputTokens
	}
	return inf.CompletionTokens
}

// exchangeTokensCell renders an exchange's TOKENS as prompt → output, each compact, with what
// tool-prune kept off the prompt in parentheses: "70.6k(−3.1k)→9" (#1681). The provider is
// the only party that tokenizes, so both figures are on the response, and the cell is blank
// until it arrives.
func exchangeTokensCell(r eventRow) string {
	resp := exchangeResp(r)
	if resp == nil || resp.Inference == nil {
		return ""
	}
	prompt, out := pipeline.PromptTokensOf(resp.Inference), outputTokens(resp.Inference)
	if prompt <= 0 && out <= 0 {
		return ""
	}
	cell := formatCompact(float64(prompt))
	if s, ok := pruneSavingFor(resp); ok && s.TokensAvoided > 0 {
		cell += "(" + savingSign(s.Projected) + formatCompact(float64(s.TokensAvoided)) + ")"
	}
	return cell + "→" + formatCompact(float64(out))
}

// exchangeCostCell renders an exchange's COST: its total, with what tool-prune saved in
// parentheses. Blank when no figure was published, which reads as "not reported".
func exchangeCostCell(r eventRow) string {
	resp := exchangeResp(r)
	if resp == nil {
		return ""
	}
	usd, ok := exchangeCost(resp)
	if !ok {
		return ""
	}
	var saved float64
	var projected bool
	if s, ok := pruneSavingFor(resp); ok {
		saved, projected = s.USD, s.Projected
	}
	return money.Relabel(formatUSDWithSaving(usd, saved, projected), recordUnit(resp), 0)
}
