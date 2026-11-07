package tui

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/cmd/agentop/money"
	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// sessionsColumns is the table's full-width column set, before any terminal-fitting.
//
// A function rather than a package var so layout() can re-fit from the originals on every
// resize: fitting the LIVE columns would be cumulative, and a terminal that got narrower once
// would keep its narrowed columns after being widened again.
//
// These widths sum to 84 declared columns, or 100 rendered once bubbles adds its two of padding
// per cell, which is why they are fitted rather than used as-is — see fitTableColumns.
//
// THIS PARAGRAPH HAS CARRIED WRONG NUMBERS THREE TIMES: 104/116 before a rebase, 98/114 after one
// replaced ACTIVE with the context column, then 88/104 until that column's heading went from
// CONTEXT(1M) to CTX(1M). Each was stale rather than mistaken, which is the failure mode to
// expect here — any column added or resized moves them, and nothing recomputes them.
//
// TITLE is the widest optional column, and it is dropped rather than shrunk when the terminal
// cannot seat it — see sessionsShowTitle, which decides after the money columns so that
// widening the window never takes a column away. Where TITLE does render, fitTableColumns
// shrinks the widest column rather than dropping any, so it and SESSION pay for each other:
// measured, SESSION is 12 at 97 columns and 14 from 100 up. A 12-character id prefix still
// distinguishes sessions in practice, and an unnamed session is what this column exists to fix.
//
// A terminal WIDER than 100 rendered is handled by sessionsColumnsFor rather than here:
// fitTableColumns only ever shrinks, so these declared widths are simultaneously the
// narrow-terminal budget and the wide-terminal ceiling, and titles are mostly directory paths
// with nothing to gain from a ceiling of 24 when there are spare columns on the screen.
func sessionsColumns() []table.Column {
	return []table.Column{
		// SESSION, and 14 wide rather than 40. A session id is a 36-character UUID whose
		// first characters identify it to a human as well as all of them do, and the 26
		// columns it was spending are the ones the money columns need. `/` filters on the
		// FULL id, so nothing is lost for finding a session — only for reading one.
		{Title: "SESSION", Width: 14},
		// After SESSION, never before it. Nothing reads the id off a row any more —
		// selectedSessionID and the cursor restore read sessionRowIDs, kept beside the rows —
		// but SESSION first is where a reader looks for it, and withColumnIfItFits seats every
		// optional column after it for that reason.
		{Title: "TITLE", Width: sessionsTitleWidth},
		{Title: "UPDATED", Width: 14},
		{Title: "EVENTS", Width: 8},
		{Title: "TOKENS", Width: 10},
		// COST and SAVED are LIFETIME figures, scoped exactly like TOKENS beside them, so
		// the row is internally consistent. A per-hour cost next to a lifetime token count
		// understated the row by roughly 6x on a long session and invited a reader to
		// derive a rate from two figures measured over different spans.
		//
		// Both come from the server's own sum over the session's events
		// (session.SessionSummary.CostMicros / .AvoidedMicros), not from the strip's ring
		// window: the durable ledger's row key carries no session dimension by design, and
		// the ring covers only its rolling span. So these continue across a proxy restart
		// where it keeps a session archive and reset where it does not, while the strip's
		// "today" figure always continues — both are correct, and neither is a check on the
		// other.
		{Title: "COST", Width: 10},
		{Title: "SAVED", Width: 10},
		// CTX(1M) replaces an ACTIVE column that carried a ● for a flag nobody acted on.
		// UPDATED already says whether a session is live, in seconds rather than as a dot.
		//
		// The denominator is IN THE TITLE because it is fixed at one million and a gauge with
		// no stated scale is a decoration. See contextWindowTokens for why it is fixed and what
		// that costs.
		{Title: contextColumnTitle, Width: len(contextColumnTitle)},
	}
}

// contextColumnTitle names the column and its denominator together.
//
// The width follows the title rather than the gauge: the gauge scales to whatever the fitter
// leaves, and a column narrower than its own heading would have bubbles truncate the heading,
// losing the "(1M)" first — the one part that states a scale.
//
// SEVEN COLUMNS, BECAUSE A DECLARED WIDTH IS NOT A FITTED ONE. The heading was CONTEXT(1M), declared
// at exactly its eleven columns so it "could not" be clipped — but fitTableColumns shrinks the
// widest column against one global floor of four, and at eleven this was often the widest. It was
// clipped at widths 45-58, which no declared width can prevent; seven clears every width from 45 to
// 200, and TestSessionsHeaders_CarryNoANSIUnderAForcedColourProfile holds it to that with no
// exemption. Any column added to this table moves the fit, so that sweep is what keeps it true.
const contextColumnTitle = "CTX(1M)"

// contextWindowTokens is the denominator every gauge is drawn against.
//
// FIXED AT ONE MILLION, which is the largest window on any path this proxy sees — the Claude
// [1m] beta — and the same figure core/cost/usage and core/cost/pricing already reason against.
//
// WHAT THAT COSTS, stated because the gauge is only as honest as its scale: a model with a
// 200k window at 180k tokens is 90% full and draws here as 18%, near-empty, at exactly the
// moment a reader most needs to see otherwise. The fix is a per-model window table, and it is
// deliberately not in this change — it needs the model on the session summary, which the server
// does not publish today. Until then the gauge reads as "how much of a 1M window", which the
// title says, and not as "how close to this model's limit".
const contextWindowTokens = 1_000_000

// sessionsRightAligned names the columns whose cells rebuildSessionsTable right-aligns with
// padLeft, and whose HEADINGS therefore have to be right-aligned too.
//
// One list, read by alignSessionsHeaders below and by the header/cell agreement test, so a
// column cannot be padded in its cells while its heading stays on the far side of the column —
// which is what all four of these did until now. There is no eventColumn-style struct to hang
// the flag on here: the sessions table is a []table.Column whose cells are built inline against
// each column's fitted width, so the set is named instead.
//
// CTX(1M) is here for its EMPTY cell rather than its full one. Every gauge is exactly the
// column's width, so where one sits is moot — but an unknown context renders as the same em dash
// COST and SAVED use, and a reader scanning for "nothing known here" should find all three in
// one vertical line.
var sessionsRightAligned = map[string]bool{
	"EVENTS":           true,
	"TOKENS":           true,
	"COST":             true,
	"SAVED":            true,
	contextColumnTitle: true,
}

// alignSessionsHeaders right-aligns the headings of the numeric columns, against the widths
// they were FITTED to rather than the widths they declare — see rightAlignHeader.
//
// A copy, never the caller's slice, for the same reason fitTableColumns takes one: the input
// comes from a package-level constructor, and writing through it would make the result
// permanent for every later caller instead of per-rebuild.
//
// Idempotent, because each heading is re-derived from headerTitle rather than padded again:
// re-aligning an already-aligned set is a no-op rather than a heading pushed off its column.
// headerTitle strips the estimate marker as well as the padding, which is what keeps that true
// for SAVED — otherwise each pass would add another "~".
//
// THE ESTIMATE MARKER IS ADDED HERE, at render time, and the declared Title stays the bare name.
// A saving is always an estimate, so the caveat is the column's rather than any row's and is
// stated once above them instead of on every cell — see sessionMoneyCell. Doing it here rather
// than in sessionsColumns is what keeps "SAVED" the lookup key: four callers address this column
// by that literal string, and a declared Title of "SAVED~" would make all four read it as absent
// and render every saving as "—".
func alignSessionsHeaders(cols []table.Column) []table.Column {
	out := make([]table.Column, len(cols))
	copy(out, cols)
	for i, c := range out {
		title := headerTitle(c)
		if title == "SAVED" {
			title += inexactMarker
		}
		if sessionsRightAligned[headerTitle(c)] {
			out[i].Title = rightAlignHeader(title, c.Width)
		} else {
			out[i].Title = title
		}
	}
	return out
}

// sessionsTitleWidth is TITLE's floor: the narrowest cell worth granting the column, and its
// width at the declared layout.
//
// 11, and the arithmetic an earlier revision gave for it was wrong. It said "the columns TITLE
// does not displace render 67 wide, and 67 + 11 + 2 is 80" — 67 was right then (63 since the
// context heading became CTX(1M)), but the gate does not add up declared widths any more: it asks
// the fitter what TITLE is left with, and the fitter narrows the two wider columns before it
// touches TITLE. So the real threshold is lower than that sum — 69, measured.
//
// 11 is the narrowest cell where a left-truncated path still says which session it is:
// "…claudesessions" fits, where 8 columns give a leaf fragment identifying nothing. Picked for
// that reason and then checked against the layout, rather than derived from a budget — which is
// what the wrong derivation above invited the next reader to redo.
//
// Re-derived once already. It was 14 until main replaced ACTIVE with CONTEXT(1M), which widened
// that base by three and pushed TITLE's threshold to 83 — caught by
// TestSessionsShowTitle_RendersAtTheCommonWidth, which exists because an earlier revision let a
// 20-column error through with nothing asserting the threshold. Shortening that heading to
// CTX(1M) took four columns back out, moving the threshold from 73 to 69. Any future column
// added to this table moves this number again, and that test is what says so.
//
// Narrow for a path, deliberately: truncLeft keeps the TAIL, so 14 columns of
// "…s/claudesessions" still says which session this is, where the same 14 from the left would
// say "/Users/person/" and distinguish nothing. Wider terminals grow it from slack — see
// growSessionsTitle — so this is a floor rather than the usual case.
const sessionsTitleWidth = 11

// newSessionsTable builds an empty sessions table. Columns are fitted to the terminal by
// layout(), which is called on every WindowSizeMsg.
//
// The headings are aligned here too, even though these widths are the unfitted ones: an
// unaligned header would otherwise be what a pane that has not seen a WindowSizeMsg yet
// renders.
func newSessionsTable() table.Model {
	t := table.New(
		table.WithColumns(alignSessionsHeaders(sessionsColumns())),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// rebuildSessionsTable updates the rows from m.sessions, marks the rows the pane's
// search matches, and keeps the cursor on the previously-selected session if still
// present.
func (m *model) rebuildSessionsTable() {
	// The FULL id of the previously-selected row, read out of band. Taken from the rendered
	// cell it was a truncated string compared against other truncated strings, which happened
	// to work and would have stopped working the moment two ids shared a prefix — at 72
	// columns the SESSION cell fits eight runes.
	prev := ""
	if c := m.sessionsTbl.Cursor(); c >= 0 && c < len(m.sessionRowIDs) {
		prev = m.sessionRowIDs[c]
	}
	now := m.clock()
	// ONE FUNCTION SETS THE HEADER AND THE ROWS, and it is this one. They have to change
	// together — the money columns come and go with the terminal width — and anything that
	// moves one without the other is a crash rather than a cosmetic bug:
	//
	//   - bubbles' SetColumns calls UpdateViewport SYNCHRONOUSLY, and renderRow indexes
	//     cols[i] once per CELL. So installing a 5-column header while 7-cell rows are still
	//     loaded panics inside SetColumns itself — "index out of range [5] with length 5" —
	//     and layout() did exactly that on any resize across 53 columns. A tmux split was
	//     enough. Appending a rebuild after SetColumns cannot help; the panic is inside it.
	//   - The other direction does not crash, it MISALIGNS: a 5-cell row under a 7-column
	//     header puts ACTIVE's dot under COST.
	//
	// Reading the flag off the table's own columns fixed the disagreement WITHIN a rebuild and
	// did nothing for this, because the columns were still set somewhere else. Derived once
	// here now, and used for both, so the two cannot be produced by separate decisions at all.
	showMoney := sessionsShowMoney(m.width)
	// The SAME predicates the header used, for the reason its own comment gives: the columns
	// and the rows are decided by two separate calls, and one consulted here and not there
	// makes every cell after it render under the wrong heading — or, when the arity differs,
	// panics inside bubbles' SetColumns.
	showTitle := sessionsShowTitle(m.width)
	// The header this rebuild will install, computed first because the money cells are rendered
	// against their column's FITTED width — the fitter shrinks columns on a narrow terminal, so
	// the declared 10 is a ceiling rather than the budget.
	//
	// Fitted, THEN aligned, and in that order: the alignment pads each heading to the width the
	// fitter settled on, so padding first would pad to a width the column no longer has. Every
	// lookup below still finds its column, because they go through headerTitle.
	scope := m.sessionsScope()
	router, showServer := m.sessionsShowServer()
	// AGENT and SERVER are asked for by what is listed, then dropped whole when the terminal has
	// no room. Kept, so the search can still read a dropped one.
	twoAgents := m.sessionsListTwoAgents(scope)
	want := alignSessionsHeaders(fitTableColumns(
		sessionsColumnsWith(m.width, twoAgents, showServer), m.width))
	agentW := sessionsColumnWidth(want, "AGENT")
	serverW := sessionsColumnWidth(want, "SERVER")
	costW := sessionsColumnWidth(want, "COST")
	savedW := sessionsColumnWidth(want, "SAVED")
	// The other cells are fitted too: padLeft right-aligns into the FITTED width, so digits
	// line up at whatever width the fitter settled on. Left-aligned numbers were the main
	// reason this table read as ragged — "5" and "105" began at the same column and ended
	// two apart, so no two rows could be compared by eye.
	idW := sessionsColumnWidth(want, "SESSION")
	// From `want`, the header about to be installed, not from the table's current one.
	titleW := sessionsColumnWidth(want, "TITLE")
	eventsW := sessionsColumnWidth(want, "EVENTS")
	tokensW := sessionsColumnWidth(want, "TOKENS")
	// The gauge is drawn to the FITTED width like every other cell, so a narrow terminal gets a
	// shorter track rather than a wrapped one. Every row shares it, so the fills stay comparable.
	contextW := sessionsColumnWidth(want, contextColumnTitle)
	rows := make([]table.Row, 0, len(m.sessions))
	ids := make([]string, 0, len(m.sessions))
	var full, bucketFull []table.SearchRow
	var bucketRows []table.Row
	var bucketIDs []string
	emit := func(id string, b rowBuilder) {
		if _, _, ok := pendingBucket(id); ok {
			bucketRows = append(bucketRows, b.row)
			bucketIDs = append(bucketIDs, id)
			bucketFull = append(bucketFull, b.searchRow())
			return
		}
		rows = append(rows, b.row)
		// APPENDED IN LOCKSTEP, one line apart, so the three cannot drift: the row carries
		// what a reader sees, this what the code acts on, and the search row what the row
		// says in full.
		ids = append(ids, id)
		full = append(full, b.searchRow())
	}
	q := m.searchQuery(paneSessions)
	folded := m.foldedFigures()
	for _, s := range m.sessions {
		if !m.sessionListed(s, scope) || foldedInto(s) != "" {
			continue
		}
		b := newRowBuilder(q != "")
		b.cut(sessionIDCell(s.ID, idW), func() string { return sessionIDCell(s.ID, noLimit) })
		// s.Title straight off the summary, where sessionLabel reaches the same value by id
		// through m.servedTitle. Two routes, equal only because they read the same slice —
		// probed across many inputs without finding a divergence. Do not "unify" one into the
		// other: this loop has the summary in hand and should not pay a lookup, and the header
		// path has only an id and cannot avoid one.
		title := func() string { return m.sessionTitleCell(s.ID, s.Title, noLimit) }
		if showTitle {
			b.cut(m.sessionTitleCell(s.ID, s.Title, titleW), title)
		} else {
			// TITLE is the column a narrow terminal gives up; it is still searched.
			b.dropped(title)
		}
		agent := func() string { return sessionAgentCell(s, noLimit) }
		if agentW > 0 {
			b.cut(sessionAgentCell(s, agentW), agent)
		} else if twoAgents {
			b.dropped(agent)
		}
		// Read-only: where the session's inference went. No key here changes it — that would
		// move a running conversation, which the router exists not to do.
		server := func() string { return sessionServerCell(router, s.InferenceHost, noLimit) }
		if serverW > 0 {
			b.cut(sessionServerCell(router, s.InferenceHost, serverW), server)
		} else if showServer {
			b.dropped(server)
		}
		b.cell(relTime(now, s.UpdatedAt))
		// The server's count, and only ever the server's: it is the complete one. agentop's
		// own cache holds what it snapshotted plus what it has streamed since attaching, which
		// for a session older than the connection is a smaller number — and when
		// handleStreamEvent also wrote this field, the cell flipped between the two on live
		// traffic. The cached-only rows below use len(cached) because the server does not list
		// those at all.
		b.cell(padLeft(fmt.Sprintf("%d", s.EventCount), eventsW))
		// The buckets folded into this session are its calls; their figures are its too.
		if f, ok := folded[s.ID]; ok {
			s.CostMicros, s.AvoidedMicros, s.TotalTokens = s.CostMicros+f.cost, s.AvoidedMicros+f.avoided, s.TotalTokens+f.tokens
		}
		b.cell(padLeft(sessionTokens(s.TotalTokens, m.events[s.ID]), tokensW))
		cost := func() string { return sessionMoneyCellIn(s.CostMicros, s.Saturated, noLimit, s.Currencies) }
		saved := func() string { return sessionMoneyCellIn(s.AvoidedMicros, s.Saturated, noLimit, s.Currencies) }
		if showMoney {
			b.cut(padLeft(sessionMoneyCellIn(s.CostMicros, s.Saturated, costW, s.Currencies), costW), cost)
			b.cut(padLeft(sessionMoneyCellIn(s.AvoidedMicros, s.Saturated, savedW, s.Currencies), savedW), saved)
		} else {
			b.dropped(cost)
			b.dropped(saved)
		}
		// The server's published figure is merged with agentop's own — see sessionContextFor for
		// why neither source dominates. It is what lets a row idle since before agentop attached
		// draw a gauge on the first poll, with nothing opened.
		b.cell(padLeft(contextGauge(m.sessionContextFor(s.ID, s.PromptContext), contextW), contextW))
		emit(s.ID, b)
	}
	// Sessions whose events agentop still holds but the server no longer lists.
	// Retaining the events (#870) is only half a fix if there is no row to
	// select them from: a proxy without a session archive lists nothing after a
	// restart, so without this the picker is empty and the retained history is
	// unreachable. One with an archive lists everything it holds, and these rows
	// are then mostly absent.
	for _, id := range m.cachedOnlySessionIDs() {
		// A scope lists only sessions the server names an agent for. Other is the exception: a
		// session the server no longer lists names no agent, which is that row's definition.
		if scope != "" && scope != otherAgents {
			continue
		}
		cached := m.events[id]
		b := newRowBuilder(q != "")
		b.cut(sessionIDCell(id, idW), func() string { return sessionIDCell(id, noLimit) })
		// These rows exist precisely because the server no longer lists the session, so there
		// is no summary to carry a served title. Harvested metadata outlives the listing, so
		// the cell can still fill from that. Named constant rather than a bare "" so the
		// absence reads as a fact about this row, not a forgotten argument.
		title := func() string { return m.sessionTitleCell(id, noServedTitle, noLimit) }
		if showTitle {
			b.cut(m.sessionTitleCell(id, noServedTitle, titleW), title)
		} else {
			b.dropped(title)
		}
		summary := session.SessionSummary{ID: id}
		agent := func() string { return sessionAgentCell(summary, noLimit) }
		if agentW > 0 {
			b.cut(sessionAgentCell(summary, agentW), agent)
		} else if twoAgents {
			b.dropped(agent)
		}
		// No summary, so no inference host to name a server from.
		if serverW > 0 {
			b.cell(emptyCell)
		}
		// "cached" sits in UPDATED now, where an em dash used to, because ACTIVE is gone and
		// that marker is the only thing on the row saying why it has no server figures. It
		// answers this column's question as well as anything can: the server has forgotten the
		// session, so there is no update time to report, and what a reader needs to know is
		// that these events are local.
		b.cell(cachedMarker)
		b.cell(padLeft(fmt.Sprintf("%d", len(cached)), eventsW))
		b.cell(padLeft(sessionTokens(0, cached), tokensW))
		if showMoney {
			// No figures for a session the server no longer lists. agentop holds these
			// events and never held their costs: the money is summed server-side from the
			// session store, and this row exists precisely because that store has
			// forgotten the session. An em dash says "not known here", where $0.00 would
			// say the session was free.
			b.cell(emptyCell)
			b.cell(emptyCell)
		}
		// These rows DO have a context, and it is the one case where agentop's cache is the only
		// possible source: the server has forgotten the session, so nothing else could answer.
		// It does not list these rows at all, so there is no summary and no published figure —
		// hence the nil, which is the merge's identity.
		b.cell(padLeft(contextGauge(m.sessionContextFor(id, nil), contextW), contextW))
		emit(id, b)
	}
	rows, ids = append(rows, bucketRows...), append(ids, bucketIDs...)
	if q == "" {
		full = nil
	} else {
		full = append(full, bucketFull...)
	}
	// ONLY WHEN THE HEADER ACTUALLY CHANGES, which is a resize and nothing else. SetRows(nil)
	// resets the viewport's offset, and this function runs on every poll — clearing
	// unconditionally scrolled the picker back under the operator twice a second, which
	// TestSessionsTable_PollRebuildKeepsScrollPosition exists to catch.
	//
	// When it does change, the rows go first: SetColumns renders whatever rows are loaded, so
	// there must be none it could misread. A scroll reset is unavoidable there — the rows are
	// being rebuilt against a different header — and a resize is already a re-layout.
	if !sameColumns(m.sessionsTbl.Columns(), want) {
		m.sessionsTbl.SetRows(nil)
		m.sessionsTbl.SetColumns(want)
	}
	m.sessionsTbl.SetRows(rows)
	m.sessionsTbl.SetSearch(q, full)
	// Published with the rows they describe, never separately.
	m.sessionRowIDs = ids

	// Restore cursor position if possible. Through setCursorVisible: a restored row
	// past the first screenful would otherwise land one line below the rendered
	// window, leaving the pane with no highlight — see setCursorVisible.
	if prev != "" {
		for i, id := range ids {
			if id == prev {
				setCursorVisible(&m.sessionsTbl, i)
				return
			}
		}
	}
	setCursorVisible(&m.sessionsTbl, 0)
}

// cachedOnlySessionIDs lists sessions agentop has events for that the server's
// current list omits, sorted so the picker does not reshuffle under the cursor
// on each refresh. Empty caches are skipped: a row advertising zero events
// helps nobody, and snapshotLoadedMsg can create the key with an empty slice.
// Since #1542 the only way events are held is opening (or watching) a session,
// so a session the user never opened no longer appears here once the server
// drops it — the archive's ?archived=true list is where such sessions surface
// now, which is why that is a fine trade.
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

// sessionListed reports whether the sessions table shows s: every session, and under an agent
// scope only the sessions of that agent — under Other, every session that names no recognised
// agent. A search hides nothing; it marks.
func (m *model) sessionListed(s session.SessionSummary, scope string) bool {
	switch scope {
	case "":
		return true
	case otherAgents:
		return inOtherAgents(s)
	}
	return sessionAgent(s) == scope
}

// sessionsListTwoAgents reports whether the listed sessions name two or more agents, which is when
// the AGENT column says something.
func (m *model) sessionsListTwoAgents(scope string) bool {
	var first string
	for _, s := range m.sessions {
		agent := sessionAgent(s)
		if agent == "" || !m.sessionListed(s, scope) {
			continue
		}
		if first == "" {
			first = agent
		} else if agent != first {
			return true
		}
	}
	return false
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

// sessionTokens reports the total tokens for a session, compactly: 137,156,234 renders
// as "137.2M".
//
// Grouped digits overflowed the 10-column cell at 100M and were truncated to
// "137,156,2…", which is worse than a rounded figure in every way — it is longer, less
// readable, and its last digits are the ones that got cut. A session total is a
// magnitude, not something anyone reconciles: the exact per-event counts are in the
// events pane, and the exact total is one curl away on /v1/sessions.
//
// Prefers the server-computed count from SessionSummary (authoritative, covers the
// full event backlog even before we've streamed anything for this
// session). Falls back to a client-side sum over the cached events when
// the server returned zero (older authbridge server without token
// aggregation). Returns "—" when neither source has data.
func sessionTokens(serverTotal int, cached []pipeline.SessionEvent) string {
	if serverTotal > 0 {
		return formatCompact(float64(serverTotal))
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
	return formatCompact(float64(total))
}

// selectedSessionID returns the cursor row's session ID, or "".
func (m *model) selectedSessionID() string {
	// OUT OF BAND, never the rendered cell: see model.sessionRowIDs for what reading the cell
	// cost when the SESSION column began truncating.
	c := m.sessionsTbl.Cursor()
	if c < 0 || c >= len(m.sessionRowIDs) {
		return ""
	}
	return m.sessionRowIDs[c]
}

// emptyCell is what a table cell shows for a figure that is NOT KNOWN, as opposed to one
// that is zero. One spelling, because the difference between the two is the whole point and
// a surface that used "0" in one column and "—" in another would erase it.
const emptyCell = "—"

// sessionMoneyCell renders one session's lifetime cost or its lifetime saving. The two are the
// same cell: they differ only in which micros the caller passes and in the column heading above
// them, not in how a figure is formatted or marked.
//
// UNKNOWN AND ZERO ARE THE SAME CELL HERE, and deliberately: micros == 0 means either the
// session's traffic could not be priced or it genuinely charged nothing, and this function
// cannot tell those apart — the server omits the field for both (omitempty on a zero). So it
// prints the em dash for both rather than "$0.00", which would assert the stronger of the
// two readings. That is the standing rule on every money surface in this package: never
// $0.00 for a figure that might be unknown.
//
// A NEGATIVE figure is refused through the shared negativeCost, not clamped: the session API
// sums non-negative per-request figures, so a negative can only come from a broken producer,
// and "-$5.00" in a column of costs reads as a refund nobody issued.
//
// A saving is estimated — from a bytes-to-tokens ratio, gross of the prompt-cache re-warm, see
// usage.Counts.AvoidedMicros — and that caveat is NOT on this cell. It is on the column HEADER,
// which reads "SAVED~"; see alignSessionsHeaders.
//
// THE MARKER MOVED BECAUSE IT IS A PROPERTY OF THE COLUMN, not of the row. It applied to every
// saving this cell has ever rendered — the per-request flags recording which caveats applied do
// not survive summation, so it could never be conditional — and a glyph repeated down every row
// of a column states one fact as many times as there are sessions. The general rule this package
// now follows: an UNCONDITIONAL caveat belongs on the header, a CONDITIONAL one rides the figure.
// partialMarker below is conditional and stays here.
//
// This is a deliberate exception to "a marker rides ON the figure" (see partialMarker's own doc),
// and it is narrow: a bubbles table always renders its header, so unlike the spend strip's
// fitter — which can drop the words beside a figure while the figure stays — there is no width at
// which these cells are visible and their header is not. sessionsShowMoney drops the columns
// whole rather than letting them narrow far enough to clip the heading.
//
// saturated marks the figure as a FLOOR: the session's total reached the int64 ceiling and
// was clamped, so the real number is larger by an amount nothing can state. It arrives from
// session.SessionSummary.Saturated, which exists because MaxInt64 micros is about
// $9.2 trillion — a well-formed dollar amount indistinguishable from a measured one.
//
// partialMarker, the glyph that already means "and more" on the spend strip. Two causes share
// it in this cell — a clamped total and, if this column ever renders one, a partially covered
// one — because both mean exactly "the real figure is larger than the number shown" and a
// ten-column cell has no room to say which. The strip has room for a note and distinguishes
// them there. A marker that rides ON the figure is the point: a cell can be truncated to
// nothing but while the number is on screen its caveat is too.
// budget is the column's FITTED width, and the figure is rendered less precisely rather than
// wider when cents will not fit.
//
// A bubbles table does not re-flow an overflowing cell, it truncates — and truncating a money
// figure produces a smaller figure that reads as real, which is the one thing every surface here
// refuses. The cell used to be unbounded: "~$936.5777+" is eleven columns against a ten-column
// header, so a session past about $937 overflowed, and the saturated case was the worst of all
// ("~$9223372036854.7754+", twenty-one) because the marker that says "this is a floor" is
// appended to the longest value there is.
//
// PRECISION IS WHAT YIELDS, in order: cents, whole dollars, then humanizeCount's compact form,
// which is itself width-bounded and clamps at ">999T". The last candidate always fits a sane
// column, so this cannot fall through to an unbounded string.
//
// CENTS AT THE TOP, not four decimals, and that is a reversal. This column read "$1.8140" — four
// decimals — on the reasoning that a per-session figure is attributable to one thing and is
// routinely sub-cent. It is the wrong trade for a column a reader SCANS: four decimals are two
// digits of noise on every row of a table whose purpose is comparing sessions to each other, and
// the sub-cent case is served by the floor below rather than by widening every other row. The
// cost is real and accepted: two sessions that differ below a cent now read alike. See the
// precision rule beside formatUSDTotal, which this change rewrote.
func sessionMoneyCell(micros int64, saturated bool, budget int) string {
	return sessionMoneyCellIn(micros, saturated, budget, nil)
}

// sessionMoneyCellIn is sessionMoneyCell in the units SessionSummary.Currencies names: nil is
// dollars, rendered exactly as sessionMoneyCell always has; one foreign unit relabels each rung of
// the same ladder (money.Relabel), so credits follow the dollar precision rule; two or more are a
// cross-unit sum with no amount to show, so the cell says money.Mixed.
func sessionMoneyCellIn(micros int64, saturated bool, budget int, currencies []string) string {
	if micros == 0 || negativeCost(micros) {
		return emptyCell
	}
	unit, ok := money.WindowUnit(currencies)
	if !ok {
		if lipgloss.Width(money.Mixed) <= budget {
			return money.Mixed
		}
		return emptyCell
	}
	decorate := func(amount string) string {
		if saturated {
			amount += partialMarker
		}
		return amount
	}
	usd := float64(micros) / 1e6
	// A RUNG THAT ROUNDS THE FIGURE TO ZERO IS SKIPPED, not rendered. %.0f turns anything under
	// fifty cents into "$0" — and a known non-zero cost displayed as free is worse than the
	// unknown one emptyCell stands for. This cell's own rule, fifty lines up, is never $0.00 for a
	// figure that might be unknown; a figure that is KNOWN and shown as nothing breaks it harder.
	//
	// THE GUARD NOW PROTECTS ONLY THE LOWER RUNGS. It used to be what kept a sub-cent charge off
	// the cents rung, because that rung was a bare fmt.Sprintf("%.2f") with no floor of its own and
	// printed "$0.00". formatUSDTotalMicros has the floor built in, so the top rung is honest for
	// every positive figure and the guard never fires on it.
	//
	// Each rung carries the rounded value it would print, so "is this rung honest" is one
	// comparison rather than a guess about the format string.
	type rung struct {
		text    string
		rounded float64
	}
	compact := rung{"$" + humanizeCount(int64(usd)), math.Trunc(usd)}
	for _, r := range []rung{
		// THROUGH MICROS, not usd: formatUSDTotalMicros rounds half-up on the integer, which is
		// the whole reason it exists — 1_005_000 micros is exactly $1.005, the nearest float64 is
		// 1.00499999…, and %.2f prints "$1.00". The integer is already in hand here, so there is
		// no reason to launder it through a float and back.
		//
		// rounded is usd, NOT the rounded cents value, for the same reason the four-decimal rung
		// carried it: this formatter has its own floor — anything positive under half a cent
		// renders "<$0.01" rather than "$0.00" — so it never claims zero for a real charge and
		// must not be skipped by the guard below.
		{formatUSDTotalMicros(micros), usd},
		{"$" + fmt.Sprintf("%.0f", usd), math.Round(usd)},
		compact,
	} {
		if r.rounded == 0 {
			continue
		}
		// lipgloss.Width, like everything else that budgets a cell here. Safe as a rune count
		// today — the output is ASCII digits with width-one markers — but it is the same class
		// of bug truncLeft had, and the cost of being right is one call.
		text := r.text
		if !money.IsDefault(unit) {
			room := budget
			if saturated {
				room -= lipgloss.Width(partialMarker)
			}
			if text = money.Relabel(text, unit, room); text == "" {
				continue
			}
		}
		if cell := decorate(text); lipgloss.Width(cell) <= budget {
			return cell
		}
	}
	// NOTHING FITS — or nothing that can be shown WITHOUT rounding a real charge to zero — so
	// nothing is shown. The floor is "<$0.01" at six columns, seven with the clamp marker, which
	// sessionMoneyCellMin is derived from: the columns are dropped whole rather than narrowed past
	// it, so this is unreachable through sessionsShowMoney and is kept for a caller that passes a
	// budget of its own.
	//
	// The em dash rather than a truncation, and rather than dropping the marker to buy a column:
	// a clipped figure is a smaller figure that reads as real, and "+" says the figure is a floor,
	// so a bare number in its place is a different claim. If it cannot be said correctly it is not
	// said, which is the rule the spend strip's ladder follows for the same reason.
	return emptyCell
}

// sessionsColumnWidth is the fitted width of one named column, or 0 when it is not present.
//
// By headerTitle rather than by the raw Title, so it finds a column whose heading carries
// alignment padding. Against the raw string, a right-aligned COST reads as absent and its cells
// get rendered against a zero budget — which sessionMoneyCell renders as "not known here" for
// every charge.
func sessionsColumnWidth(cols []table.Column, title string) int {
	for _, c := range cols {
		if headerTitle(c) == title {
			return c.Width
		}
	}
	return 0
}

// sessionTokensCellMin is the narrowest TOKENS cell that can hold every value it renders:
// sessionTokens' widest output is six runes ("999.9M"), so six always fits and five never
// does. Named rather than inlined because two things depend on it — the fit decision below
// and TestSessionTokens_FitsEveryFittedWidth, which walks the same boundary.
const sessionTokensCellMin = 6

// sessionMoneyCellMin is the width below which sessionsShowMoney drops the COST and SAVED
// columns rather than render them dishonestly.
//
// NINE, WHICH IS NO LONGER THE MONEY CELL'S OWN FLOOR, and the gap is deliberate. The cell's
// floor came down to seven when this column moved to cents: sessionMoneyCell's top rung is
// formatUSDTotalMicros, whose own floor is "<$0.01" at six runes, plus partialMarker for a
// clamped total — "<$0.01+", seven. The estimate marker used to make it nine ("~<$0.0001") and
// it no longer rides the figure at all.
//
// SO WHY NOT SEVEN. Because this constant does not only gate the money cell — it is what keeps
// the fitter away from the rest of the table. fitTableColumns shrinks the widest column first,
// so SESSION and UPDATED are what pay for two extra money columns, and MEASURED at seven the
// money columns survive down to terminal width 59, where the fitter has taken UPDATED to six or
// seven runes and relTime's "just now" needs eight — a clipped cell, which is the one thing this
// file refuses. Guarding UPDATED instead is worse in the other direction: its widest output is
// the date form past a day ("Jan 12 15:04", twelve), and holding twelve drops the money columns
// below width 86, thirteen columns worse than today.
//
// Nine is therefore the empirical width at which this WHOLE table still renders honestly, and it
// is the number the drop is keyed on until UPDATED can degrade its own cell the way this one
// does. Both figures are here because a later reader will otherwise re-derive seven from the
// cell and wonder why the constant disagrees.
//
// Deliberately NOT ten, the declared width, even though the fitter's shrink order means the
// columns are at their declared width whenever they survive today. Ten is what the widest
// value happens to need; nine is what the table needs, and only the second one stays true if a
// column's declared width changes.
const sessionMoneyCellMin = 9

// sessionsShowMoney reports whether this terminal can afford the COST and SAVED columns.
//
// MEASURED, not thresholded. fitTableColumns squeezes the widest column repeatedly until the
// table fits, so two more columns cost every other column width — at 50 columns they took
// TOKENS from 8 runes to 5 and "100.0k" began truncating, which is the exact failure
// TestSessionTokens_FitsEveryFittedWidth exists to catch. A hardcoded "60 columns" would be
// the same fact written as a number that goes stale the moment any column's declared width
// changes; asking the fitter keeps it true by construction.
//
// DROP WHOLE COLUMNS, NEVER CLIP A CELL — the rule the spend strip's ladder follows for the
// same reason. A truncated figure is a wrong figure, and two money columns are not worth
// making the token count unreadable on a narrow terminal.
//
// True before the first layout, when the width is still zero: newSessionsTable is built from
// the declared columns, so the rows must match them, and the first WindowSizeMsg refits both.
// AND ON THE MONEY COLUMNS' OWN FLOOR, which measuring TOKENS alone did not cover. The two
// tests are independent: a budget can be generous enough to leave TOKENS readable and still be
// too small for any honest money figure. Measured before this half existed, with a known
// $0.0012 charge: COST printed the em-dash at widths 53-58 and SAVED at 53-64, because the
// narrowest form that does not round a real charge to zero is "~<$0.0001" at nine runes.
//
// That em-dash is the collision worth refusing. emptyCell means "not known here", and
// sessionMoneyCell falls back to it when nothing fits — so a column kept at a width where the
// fallback is the only outcome renders a KNOWN charge as unknown. That is the same lie as
// "$0.00" for a sub-cent figure, read from the other end, and the rule this file states fifty
// lines above sessionMoneyCell forbids it in both directions.
//
// The cost is real and measured: on this floor alone the columns disappear below 71 columns
// (TITLE, which they yield to, keeps them out until 93 — see sessionsShowTitle), where an ordinary
// "$36.58" would still have fitted. Dropping a whole column is this file's stated answer to
// not being able to render a cell honestly, and a reader who cannot see COST at all goes
// looking for the width; one who sees "—" against a session that definitely spent money
// concludes the figure is missing from the server.
func sessionsShowMoney(termWidth int) bool {
	if termWidth <= 0 {
		return true
	}
	// WITH TITLE when TITLE renders, so the set measured here is the set the header will carry.
	// TITLE is decided first (see sessionsShowTitle) and these yield to it, which is why its
	// floor is one of the minimums below rather than a check somewhere else: each column
	// individually "fitting" while the row as a whole was unreadable is the failure that reached
	// review, with TITLE squeezed to eight columns beside money cells that all passed.
	cols := make([]table.Column, 0, len(sessionsColumns()))
	title := sessionsShowTitle(termWidth)
	for _, c := range sessionsColumns() {
		if headerTitle(c) == "TITLE" && !title {
			continue
		}
		cols = append(cols, c)
	}
	fitted := fitTableColumns(cols, termWidth)
	mins := []struct {
		title string
		min   int
	}{
		{"TOKENS", sessionTokensCellMin},
		{"COST", sessionMoneyCellMin},
		{"SAVED", sessionMoneyCellMin},
	}
	if title {
		mins = append(mins, struct {
			title string
			min   int
		}{"TITLE", sessionsTitleWidth})
	}
	for _, c := range mins {
		if sessionsColumnWidth(fitted, c.title) < c.min {
			return false
		}
	}
	return true
}

// sessionsColumnsFor is the column set this terminal actually gets. Paired with
// sessionsShowMoney in rebuildSessionsTable so the row arity always matches the header.
func sessionsColumnsFor(termWidth int) []table.Column {
	return sessionsColumnsWith(termWidth, false, false)
}

// sessionsAgentWidth fits "claude-code", an agent label (versionless; see pipeline.AgentName).
const sessionsAgentWidth = 11

// sessionsColumnsWith is sessionsColumnsFor plus an AGENT column after TITLE when agent is set,
// and a SERVER column after that when server is set, each only when it fits without narrowing any
// other; see rebuildSessionsTable for when each is asked.
//
// DROPPED WHOLE ON A NARROW TERMINAL, never squeezed, the rule the money columns follow: AGENT
// first, since it is offered first, then SERVER against what is left. Squeezing either would cut
// it to a fragment and narrow the columns every width test here pins.
//
// AND NEVER TAKEN AWAY BY WIDENING (TestSessionsTable_WideningNeverDropsAColumnOrNarrowsTitle):
// see withColumnIfItFits for presence, and growSessionsTitle's pending for TITLE's width, which
// does not grow into the room an asked-for column has yet to claim.
func sessionsColumnsWith(termWidth int, agent, server bool) []table.Column {
	cols := sessionsBaseColumns(termWidth)
	pending := 0
	for _, opt := range []struct {
		asked bool
		col   table.Column
	}{
		{agent, table.Column{Title: "AGENT", Width: sessionsAgentWidth}},
		{server, table.Column{Title: "SERVER", Width: sessionsServerWidth}},
	} {
		if !opt.asked {
			continue
		}
		if with, ok := withColumnIfItFits(cols, opt.col, termWidth); ok {
			cols = with
		} else {
			pending += opt.col.Width + cellPadding
		}
	}
	return growSessionsTitle(cols, termWidth, pending)
}

// withColumnIfItFits is cols with col inserted after the identifying columns — SESSION, then
// TITLE and AGENT where present — and true, or cols unchanged and false when the fitter would
// have to narrow any column to make room.
//
// After them, in the order they are offered, so a row reads which session, then whose, then
// where — and SESSION, always present, stays first, where a reader looks for the id.
//
// ONLY WHERE COST AND SAVED RENDER. Below that width TITLE holds them out, and the room they
// leave is room they take back at the width they return: a column seated in it showed at 90,
// vanished at 93 and came back near 113, so widening the window took a column away. They rank
// above AGENT and SERVER — a session's spend is what this table is read for — so those two wait.
func withColumnIfItFits(cols []table.Column, col table.Column, termWidth int) ([]table.Column, bool) {
	if termWidth > 0 && !sessionsShowMoney(termWidth) {
		return cols, false
	}
	at := 0
	for i, c := range cols {
		switch headerTitle(c) {
		case "SESSION", "TITLE", "AGENT":
			at = i + 1
		}
	}
	with := make([]table.Column, 0, len(cols)+1)
	with = append(with, cols[:at]...)
	with = append(with, col)
	with = append(with, cols[at:]...)
	fitted := fitTableColumns(with, termWidth)
	for i := range with {
		if fitted[i].Width < with[i].Width {
			return cols, false
		}
	}
	return with, true
}

func sessionsBaseColumns(termWidth int) []table.Column {
	cols := sessionsColumns()
	// TITLE YIELDS FIRST, before the money columns do. It is the widest optional column and
	// the only one whose absence costs nothing a `/` filter cannot recover — the id is still
	// there, and the title is a convenience for reading a row rather than for finding one.
	// Keeping it at a width where TOKENS truncates would trade a legible figure for a
	// truncated phrase, which is the trade sessionsShowMoney already refuses for COST.
	if !sessionsShowTitle(termWidth) {
		out := make([]table.Column, 0, len(cols))
		for _, c := range cols {
			if headerTitle(c) == "TITLE" {
				continue
			}
			out = append(out, c)
		}
		cols = out
	}
	if !sessionsShowMoney(termWidth) {
		out := make([]table.Column, 0, len(cols))
		for _, c := range cols {
			if t := headerTitle(c); t == "COST" || t == "SAVED" {
				continue
			}
			out = append(out, c)
		}
		cols = out
	}
	return cols
}

// sessionsShowTitle reports whether this terminal is wide enough to afford TITLE.
//
// Decided FIRST, and the money columns read this rather than the reverse: TITLE is the column
// this pane gained and the only one whose absence nothing else recovers, where a session's cost
// is also in the Usage pane, `agentop cost` and the spend strip. So COST and SAVED yield to it and
// return at the width where the whole set holds every minimum at once.
func sessionsShowTitle(termWidth int) bool {
	if termWidth <= 0 {
		return true
	}
	// ONE STATEMENT, REPLACING SIX. Earlier revisions stacked a paragraph per attempt without
	// retiring the last, so this block named thresholds of 78, 80, 82, 98 and 104 — none of them
	// the answer — and two of its paragraphs were near-verbatim duplicates contradicting each
	// other about whether a change raised or lowered the threshold. In a file where the comments
	// are the only statement of design intent, that leaves the next reader unable to tell which
	// paragraph is live.
	//
	// What the function does: TITLE is granted when the FITTED set leaves it its floor, measured
	// without the money columns because those are what yield to it. TITLE renders from 69; COST
	// and SAVED are absent from 69 to 92 and return at 93, where the whole set holds every
	// minimum at once.
	//
	// Why in that order: TITLE is the column this pane gained and the only one whose absence
	// nothing else recovers — a session's cost is also in the Usage pane, `agentop cost` and the
	// spend strip, while a nameless session is nameless everywhere.
	//
	// VALIDATED AGAINST THE FITTED SET, the way sessionsMoneyFits validates the money cells.
	// Admission arithmetic alone was not a floor: it only asked whether the declared widths
	// summed under the terminal, and the fitter then shrank TITLE freely — measured at EIGHT
	// columns from width 83, not recovering 11 until 100. Eight columns of a path is a leaf
	// fragment that identifies nothing, which is the failure left-truncation exists to prevent,
	// so a column that cannot hold its floor is not worth granting at all.
	//
	// Measured WITHOUT the money columns, because they are what yields: this gate is decided
	// first and sessionsShowMoney reads it, so COST and SAVED take only what is left once a
	// legible title has its room. They are absent from 69 to 92 for that reason and return at
	// 93, where the full set holds every minimum at once.
	keep := make([]table.Column, 0, len(sessionsColumns()))
	for _, c := range sessionsColumns() {
		switch headerTitle(c) {
		case "COST", "SAVED":
			continue
		}
		keep = append(keep, c)
	}
	return sessionsColumnWidth(fitTableColumns(keep, termWidth), "TITLE") >= sessionsTitleWidth
}

// growSessionsTitle widens TITLE into whatever slack the terminal leaves, and only TITLE.
//
// fitTableColumns, which every other table here relies on, only SHRINKS — the right rule for
// columns holding bounded things (a count, a status, a plugin name) that gain nothing from
// extra room. TITLE is the exception: it holds a session title that is usually a directory
// path, and measured on one machine 107 of 109 were longer than the declared 24. So that 24
// was acting as a ceiling on a 200-column terminal with columns going spare.
//
// AFTER the money columns are decided, not before: dropping COST and SAVED frees 20 columns,
// and computing the slack first would hand TITLE a width that the narrower set then leaves
// stale. Growing only into slack that already exists means widening never costs another
// column anything, and a terminal with none hands the set back untouched.
//
// pending is the room of the optional columns asked for but not yet seated (AGENT, SERVER; see
// sessionsColumnsWith), and is not slack for the money columns' reason below: TITLE grown into it
// collapsed to its floor the moment the column arrived — 23 to 11 at 113 — as the window widened.
func growSessionsTitle(cols []table.Column, termWidth, pending int) []table.Column {
	slack := termWidth - tableWidth(cols) - pending
	if slack <= 0 || termWidth <= 0 {
		return cols
	}
	// SLACK THE MONEY COLUMNS WILL CLAIM IS NOT SLACK. Taking every spare column made TITLE'S
	// WIDTH non-monotonic: at 96 the money columns are absent and TITLE absorbed all 27 columns
	// of room, then at 97 they returned and the fitter clawed the shortfall back until TITLE
	// landed on its floor — 27 to 11 as the terminal got WIDER, a legible path becoming a leaf
	// fragment, which is the harm this column's floor exists to prevent.
	//
	// So growth stops at what survives their return. Below the width where they fit, the room
	// they will take is reserved rather than lent: TITLE grows steadily instead of ballooning
	// and collapsing. Column PRESENCE is monotonic — TITLE from 69, the money columns from 93,
	// AGENT and SERVER only where those render (withColumnIfItFits) — and this, with pending, is
	// what makes TITLE's width so.
	if !sessionsShowMoney(termWidth) {
		reserved := 0
		for _, c := range sessionsColumns() {
			if t := headerTitle(c); t == "COST" || t == "SAVED" {
				reserved += c.Width + 2
			}
		}
		if slack -= reserved; slack <= 0 {
			return cols
		}
	}
	out := make([]table.Column, len(cols))
	copy(out, cols)
	for i := range out {
		if headerTitle(out[i]) == "TITLE" {
			out[i].Width += slack
			break
		}
	}
	return out
}

// sameColumns reports whether two header sets are identical in titles and widths.
//
// Both matter. A title change is the money columns coming or going, and a WIDTH change is the
// fitter squeezing the same columns for a narrower terminal — either one means the loaded rows
// were measured against a different header, and only a change justifies the scroll reset that
// reinstalling them costs.
//
// The RAW titles, alignment padding included, because both sides are post-alignment header sets
// and that padding is derived from the width this compares anyway — so it can neither miss a
// change nor invent one.
func sameColumns(a, b []table.Column) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Title != b[i].Title || a[i].Width != b[i].Width {
			return false
		}
	}
	return true
}
