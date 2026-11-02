package tui

import (
	"fmt"
)

// subagentBlock is one subagent's share of the events pane (#1047): the rows its
// requests produced, grouped under a collapsible header anchored at the first of
// them, so an agent's fan-out can be read as one line — or opened, when the
// question is which of two subagents is the slow or costly one.
//
// Membership is STRICT: a row belongs to the block its `subagent` field names, and
// a row naming no agent is the main conversation's and stays top-level. Rows the
// header can't attribute — an opaque CONNECT, whose CONNECT carries no agent-id
// header — stay top-level too, because folding unattributed rows in by
// time-contiguity would misfile the main conversation's rows that interleave with
// a subagent's.
type subagentBlock struct {
	id string
	// rows indexes cellContexts, in arrival order, among the rows that passed the
	// inactive filter — the same population the row loop renders, so the header's
	// rollup counts what expanding would show.
	rows []int
	// tokens and cost are the block's rollup, from the same readings the TOKENS
	// and COST cells sort by (rowTokens / rowCostUSD). A request whose response
	// has not landed contributes nothing yet, like its cells.
	tokens int
	cost   float64
	// msgs is the block's exchange count — the unit the # column numbers member
	// rows in, so the header's rollup and the last row's number agree.
	msgs int
	// anchored is set when the header has been emitted for this rebuild, so the
	// header lands at the block's FIRST surviving row and never before it.
	anchored bool
}

// headerText is the block's collapsible line: the rollup the issue asked for —
// how many messages, how many tokens, what cost — behind the label. The caller
// wraps it in a one-cell table.Row (rendered as a section row spanning every
// column) and hands the SAME string to the search, so what the header displays
// is what the search matches — the label alone would hide the rollup figures
// from it. The count is in the # column's unit (exchanges, numbered from 1
// inside the block), so the rollup and the rows it summarizes cannot disagree.
func (b *subagentBlock) headerText(label string, open bool) string {
	glyph := "▸"
	if open {
		glyph = "▾"
	}
	unit := "msgs"
	if b.msgs == 1 {
		unit = "msg"
	}
	return fmt.Sprintf("%s─ %s · %d %s · %s tok · %s",
		glyph, label, b.msgs, unit, humanizeCount(int64(b.tokens)), formatUSDTotal(b.cost))
}

// subagentLabel names a block. The wire carries only the 16-hex id, so a label a
// human can act on comes from the harvested meta.json for this session
// (SessionMetadata.Subagents), falling back to the raw id for everything the
// harvest cannot name — a teammate, a non-Claude-Code agent, a stale harvest.
// Description first: it is the one Claude Code shows in its own tree, and the
// type alone ("general-purpose") answers less than the id does about WHICH
// general-purpose agent this was.
func (m *model) subagentLabel(id string) string {
	if info, ok := m.sessionsData[m.selectedSess].Subagents[id]; ok {
		if info.Description != "" {
			return sanitizeLabel(info.Description)
		}
		if info.AgentType != "" {
			return sanitizeLabel(info.AgentType)
		}
	}
	return id
}

// blockOpen reports whether a block's rows are shown. Collapsed is the default —
// the issue's motivating case is an agent whose subagents drown the conversation,
// so the pane opens quiet and Enter buys the detail. A live search opens every
// block: a match inside a collapsed block must be findable, and hidden rows are
// unreachable from a search that only sees rendered rows.
func (m *model) blockOpen(id string, searching bool) bool {
	return searching || m.subagentOpen[id]
}

// rowInactiveHidden is the hideInactive (`s` toggle) predicate, stated once so the
// row loop and collectSubagentBlocks filter the same population: a block's rollup
// must count the rows expanding it would show, and a second spelling of the
// predicate here would drift with the first edit to either.
func (m *model) rowInactiveHidden(ctxs []cellContext, partner map[int]int, j int) bool {
	return m.hideInactive && eventInactive(ctxs[j].invs) && !partnerActive(ctxs, partner, j)
}

// collectSubagentBlocks groups the rows that survive the inactive filter by their
// `subagent` field. Nil when no row names one, which is every session without
// Claude Code subagents — those rebuild exactly as before.
func (m *model) collectSubagentBlocks(ctxs []cellContext, partner map[int]int) map[string]*subagentBlock {
	var blocks map[string]*subagentBlock
	for j, cc := range ctxs {
		if m.rowInactiveHidden(ctxs, partner, j) {
			continue
		}
		id := cc.row.event.Subagent
		if id == "" {
			continue
		}
		if blocks == nil {
			blocks = make(map[string]*subagentBlock)
		}
		b := blocks[id]
		if b == nil {
			b = &subagentBlock{id: id}
			blocks[id] = b
		}
		b.rows = append(b.rows, j)
		b.tokens += rowTokens(cc)
		b.cost += rowCostUSD(cc)
	}
	return blocks
}

// toggleSubagentBlock flips the block under the events cursor, if the cursor is
// on a header row. Reports whether it acted, so the caller can skip the
// detail-pane open that Enter otherwise means on this pane.
func (m *model) toggleSubagentBlock() bool {
	b, ok := m.headerRows[m.eventsTbl.Cursor()]
	if !ok {
		return false
	}
	if m.subagentOpen == nil {
		m.subagentOpen = make(map[string]bool)
	}
	if m.subagentOpen[b.id] {
		delete(m.subagentOpen, b.id)
	} else {
		m.subagentOpen[b.id] = true
	}
	m.rebuildEventsTable()
	return true
}
