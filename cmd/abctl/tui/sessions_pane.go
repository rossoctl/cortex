package tui

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/core/observe/claude"
	"github.com/rossoctl/cortex/core/pipeline"
)

// sessionsColumns is the table's full-width column set, before any terminal-fitting.
//
// A function rather than a package var so layout() can re-fit from the originals on every
// resize: fitting the LIVE columns would be cumulative, and a terminal that got narrower once
// would keep its narrowed columns after being widened again.
//
// These widths sum to 88 declared columns, or 104 rendered once bubbles adds its two of padding
// per cell, which is why they are fitted rather than used as-is — see fitTableColumns.
//
// THIS PARAGRAPH HAS CARRIED WRONG NUMBERS TWICE: 104/116 before a rebase, then 98/114 after one
// replaced ACTIVE with CONTEXT(1M). Both were stale rather than mistaken, which is the failure
// mode to expect here — any column added or resized moves them, and nothing recomputes them.
//
// TITLE is the widest optional column, and it is dropped rather than shrunk when the terminal
// cannot seat it — see sessionsShowTitle, which decides after the money columns so that
// widening the window never takes a column away. Where TITLE does render, fitTableColumns
// shrinks the widest column rather than dropping any, so it and SESSION pay for each other:
// measured, SESSION is 12 at 100 columns and 14 from 116 up. A 12-character id prefix still
// distinguishes sessions in practice, and an unnamed session is what this column exists to fix.
//
// A terminal WIDER than 104 rendered is handled by sessionsColumnsFor rather than here:
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
		// After SESSION, never before it: rebuildSessionsTable and selectedSessionID both
		// read row[0] as the session id, so a column at index 0 would silently make the
		// cursor restore and every Enter act on a title instead.
		{Title: "TITLE", Width: sessionsTitleWidth},
		{Title: "MODEL", Width: modelColWidth},
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
		// the ring covers only its rolling span. So these RESET when the proxy restarts
		// while the strip's "today" figure does not — both are correct, and neither is a
		// check on the other.
		{Title: "COST", Width: 10},
		{Title: "SAVED", Width: 10},
		// CONTEXT(1M) replaces an ACTIVE column that carried a ● for a flag nobody acted on.
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
// leaves, and a column narrower than its own heading would have bubbles truncate the heading to
// "CONTEXT(1…", which states no scale at all.
const contextColumnTitle = "CONTEXT(1M)"

// modelColWidth is the MODEL column's width — the first model this session was seen using, folded
// server-side and published on SessionSummary.
//
// Named rather than inlined in the column literal, on the pattern of methodColWidth in
// events_pane.go: two independent numbers drift apart the moment one of them is narrowed. THE
// SAME MEASUREMENT that set methodColWidth to 18 applies here — the values reaching this column
// are the same model ids the METHOD column truncates, and dated provider ids share long prefixes:
// "claude-sonnet-4-5-20250929" (26) and "claude-sonnet-4-20250514" (25) both render as
// "claude-sonnet…" at 14, which makes the column unable to say which model the row beside it is
// reporting. DELIBERATELY ITS OWN CONSTANT rather than shared with the events table, so a future
// change to one pane's budget cannot silently move the other's.
//
// THE FLOOR THE GATE ENFORCES. 12 rather than the fitter's global floor of four: a truncated
// model id is not merely clipped, it is a WRONG one, because ids share long prefixes
// ("claude-sonnet-4-5-…" and "claude-sonnet-4-…" are different models), and this column exists
// to say WHICH model. Measured on those two real prefixes: at 12 both render
// "claude-sonne…" — already ambiguous; below 12 everything is. sessionsShowModel is what keeps
// the column from being seated at that width at all, on the pattern of sessionsShowTitle's
// floor, which exists for the same reason on a path-shaped value.
const modelColWidth = 18

// sessionsShowModel reports whether this terminal is wide enough to afford MODEL.
//
// MODEL IS THE FIRST CASUALTY AS THE TERMINAL NARROWS — the most expendable column this table
// has. Two reasons it earns that spot: it is the widest optional column (18 against TITLE's 11),
// and its absence costs the least — the per-event model is on every row of the events pane's
// METHOD column, so a session's model is recoverable one drill-in away, where a session's TITLE
// is not recoverable anywhere else on this screen.
//
// THE POINT OF YIELDING FIRST IS THAT THE NARROW TABLE DOES NOT MOVE. Below MODEL's threshold
// the pane's column set is byte-identical to the one every existing width test was written
// against: TITLE still renders from 73, the money columns still return at 97, and TOKENS still
// holds "999.9M" down to 40 columns. Seating MODEL unconditionally moved all of those — measured,
// TOKENS truncated at 42 — which is the failure sessionsShowMoney's own doc refuses for COST:
// a column that breaks another column's floor is dropped, not seated at its expense.
//
// MEASURED AGAINST THE FULL DECLARED SET, not against whatever the other gates admit at this
// width. MODEL yields to every other column, so "the set that outranks it" is simply all of
// sessionsColumns — and measuring a smaller admitted set bought MODEL a room the money columns
// would later claim back: measured, MODEL rendered from 89, vanished at 97 when COST and SAVED
// returned, and came back at 113. A column that leaves as the terminal WIDENS breaks the
// presence-monotonicity this table otherwise holds, and a reader who had found the model would
// reasonably read its disappearance as a fault. Against the full set, MODEL is granted only
// where it holds its floor beside everything: it arrives at 113, last, and never leaves.
//
// VALIDATED AGAINST THE FITTED SET, exactly as sessionsShowTitle validates TITLE's: admission
// arithmetic alone is not a floor, because the fitter then shrinks MODEL freely and a gate that
// admits a column the fitter narrows past its floor has granted nothing.
func sessionsShowModel(termWidth int) bool {
	if termWidth <= 0 {
		return true
	}
	return sessionsColumnWidth(fitTableColumns(sessionsColumns(), termWidth), "MODEL") >= sessionsModelCellMin
}

// sessionsModelCellMin is MODEL's floor: the narrowest cell worth granting the column. MODEL is
// dropped rather than narrowed below it, exactly as TITLE is dropped below sessionsTitleWidth.
// 12 for the prefix-ambiguity reason modelColWidth's doc gives.
const sessionsModelCellMin = 12

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
// CONTEXT(1M) is here for its EMPTY cell rather than its full one. Every gauge is exactly the
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
// does not displace render 67 wide, and 67 + 11 + 2 is 80" — 67 is right, but the gate does not
// add up declared widths any more: it asks the fitter what TITLE is left with, and the fitter
// narrows the two wider columns before it touches TITLE. So the real threshold is 73, not 80.
//
// 11 is the narrowest cell where a left-truncated path still says which session it is:
// "…claudesessions" fits, where 8 columns give a leaf fragment identifying nothing. Picked for
// that reason and then checked against the layout, rather than derived from a budget — which is
// what the wrong derivation above invited the next reader to redo.
//
// Re-derived once already. It was 14 until main replaced ACTIVE with CONTEXT(1M), which widened
// that base by three and pushed TITLE's threshold to 83 — caught by
// TestSessionsShowTitle_RendersAtTheCommonWidth, which exists because an earlier revision let a
// 20-column error through with nothing asserting the threshold. Any future column added to this
// table moves this number again, and that test is what says so.
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

// rebuildSessionsTable updates the rows from m.sessions, applies the current
// filter, and keeps the cursor on the previously-selected session if still
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
	now := time.Now()
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
	// MODEL is decided by the same chain and yields before either of them — see
	// sessionsShowModel, which reads both gates above.
	showModel := sessionsShowModel(m.width)
	// The header this rebuild will install, computed first because the money cells are rendered
	// against their column's FITTED width — the fitter shrinks columns on a narrow terminal, so
	// the declared 10 is a ceiling rather than the budget.
	//
	// Fitted, THEN aligned, and in that order: the alignment pads each heading to the width the
	// fitter settled on, so padding first would pad to a width the column no longer has. Every
	// lookup below still finds its column, because they go through headerTitle.
	want := alignSessionsHeaders(fitTableColumns(sessionsColumnsFor(m.width), m.width))
	costW := sessionsColumnWidth(want, "COST")
	savedW := sessionsColumnWidth(want, "SAVED")
	// The other cells are fitted too: padLeft right-aligns into the FITTED width, so digits
	// line up at whatever width the fitter settled on. Left-aligned numbers were the main
	// reason this table read as ragged — "5" and "105" began at the same column and ended
	// two apart, so no two rows could be compared by eye.
	idW := sessionsColumnWidth(want, "SESSION")
	// From `want`, the header about to be installed, not from the table's current one.
	titleW := sessionsColumnWidth(want, "TITLE")
	modelW := sessionsColumnWidth(want, "MODEL")
	eventsW := sessionsColumnWidth(want, "EVENTS")
	tokensW := sessionsColumnWidth(want, "TOKENS")
	// The gauge is drawn to the FITTED width like every other cell, so a narrow terminal gets a
	// shorter track rather than a wrapped one. Every row shares it, so the fills stay comparable.
	contextW := sessionsColumnWidth(want, contextColumnTitle)
	rows := make([]table.Row, 0, len(m.sessions))
	ids := make([]string, 0, len(m.sessions))
	for _, s := range m.sessions {
		if m.filter != "" && !strings.Contains(s.ID, m.filter) {
			continue
		}
		row := table.Row{
			trunc(s.ID, idW),
		}
		if showTitle {
			// s.Title straight off the summary, where sessionLabel reaches the same value by
			// id through m.servedTitle. Two routes, equal only because they read the same
			// slice — probed across many inputs without finding a divergence. Do not "unify"
			// one into the other: this loop has the summary in hand and should not pay a
			// lookup, and the header path has only an id and cannot avoid one.
			row = append(row, m.sessionTitleCell(s.ID, s.Title, titleW))
		}
		// Straight off the summary, the first model the server folded for this session — see
		// sessionModelCell for the em dash's meaning and for what a cached-only row does instead.
		if showModel {
			row = append(row, trunc(sessionModelCell(s.Model), modelW))
		}
		row = append(row,
			relTime(now, s.UpdatedAt),
			// The server's count, and only ever the server's: it is the complete one.
			// abctl's own cache holds what it snapshotted plus what it has streamed
			// since attaching, which for a session older than the connection is a
			// smaller number — and when handleStreamEvent also wrote this field, the
			// cell flipped between the two on live traffic. The cached-only rows below
			// use len(cached) because the server does not list those at all.
			padLeft(fmt.Sprintf("%d", s.EventCount), eventsW),
			padLeft(sessionTokens(s.TotalTokens, m.events[s.ID]), tokensW),
		)
		if showMoney {
			row = append(row,
				padLeft(sessionMoneyCell(s.CostMicros, s.Saturated, costW), costW),
				padLeft(sessionMoneyCell(s.AvoidedMicros, s.Saturated, savedW), savedW))
		}
		// The server's published figure is merged with abctl's own — see sessionContextFor for
		// why neither source dominates. It is what lets a row idle since before abctl attached
		// draw a gauge on the first poll, with nothing opened.
		row = append(row, padLeft(contextGauge(m.sessionContextFor(s.ID, s.PromptContext), contextW), contextW))
		rows = append(rows, row)
		// APPENDED IN LOCKSTEP, one line apart, so the two cannot drift: the row carries what
		// a reader sees and this carries what the code acts on.
		ids = append(ids, s.ID)
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
		row := table.Row{
			trunc(id, idW),
		}
		if showTitle {
			// These rows exist precisely because the server no longer lists the session, so
			// there is no summary to carry a served title. Harvested metadata outlives the
			// listing, so the cell can still fill from that. Named constant rather than a bare
			// "" so the absence reads as a fact about this row, not a forgotten argument.
			row = append(row, m.sessionTitleCell(id, noServedTitle, titleW))
		}
		// No model: the summary is what carries it, and these rows exist precisely because the
		// server no longer lists the session — see sessionModelCell for why the cached events
		// are not scanned for a stand-in.
		if showModel {
			row = append(row, emptyCell)
		}
		row = append(row,
			// "cached" sits in UPDATED now, where an em dash used to, because ACTIVE is gone
			// and that marker is the only thing on the row saying why it has no server
			// figures. It answers this column's question as well as anything can: the server
			// has forgotten the session, so there is no update time to report, and what a
			// reader needs to know is that these events are local.
			cachedMarker,
			padLeft(fmt.Sprintf("%d", len(cached)), eventsW),
			padLeft(sessionTokens(0, cached), tokensW),
		)
		if showMoney {
			// No figures for a session the server no longer lists. abctl holds these
			// events and never held their costs: the money is summed server-side from the
			// session store, and this row exists precisely because that store has
			// forgotten the session. An em dash says "not known here", where $0.00 would
			// say the session was free.
			row = append(row, emptyCell, emptyCell)
		}
		// These rows DO have a context, and it is the one case where abctl's cache is the only
		// possible source: the server has forgotten the session, so nothing else could answer.
		// It does not list these rows at all, so there is no summary and no published figure —
		// hence the nil, which is the merge's identity.
		row = append(row, padLeft(contextGauge(m.sessionContextFor(id, nil), contextW), contextW))
		rows = append(rows, row)
		ids = append(ids, id)
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
	// Published with the rows it describes, never separately.
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

// sessionTitle names a session from the harvested metadata, or "" when nothing names it.
//
// Nil-safe by construction rather than by a guard: a read on a nil map yields the zero
// SessionMetadata, so a session nobody harvested, an id not in the file, and the file being
// absent altogether all render the same empty cell. That is the right answer for all three —
// none of them is a fact about the session, only about whether anyone has run the harvester.
//
// HARVEST-ONLY, AND THAT IS LOAD-BEARING — do not fold the server's /v1/sessions title in here.
// sessionHasTitle reads this, and through it every backoff predicate in session_metadata.go, so a
// server title arriving would make the row read as named and STOP the re-harvest for good. The
// fallback lives in sessionTitleFor instead, which only display paths call. See its doc.
func (m *model) sessionTitle(id string) string {
	// SANITISED AT THE ACCESSOR, so every consumer is covered by one line. The title is
	// LLM-generated transcript text — sessions_metadata.go says so — and it reaches a table
	// cell, the title bar, and two headers. The cell path stripped newlines but not escape
	// sequences or carriage returns, and the title-bar path constrained neither: a newline
	// splits the frame and an ESC recolours the pane from a string nobody here wrote.
	//
	// sanitizeLabel is the package's existing answer for exactly this, introduced with a
	// CWE-150 citation. Severity is bounded — the file lives under the operator's own home
	// directory — which is why this is a one-line routing rather than a redesign.
	//
	// AND CAPPED HERE TOO, for the reason sessionTitleFor caps the served title: the harvester
	// emits at most claude.MaxTitleLen runes, but NOTHING RE-CHECKS THAT ON LOAD.
	// LoadSessionMetadata parses the JSON and applies no cap, so a hand-edited or rewritten
	// ~/.cortex/session-metadata.json reaches the quadratic path in truncLeft/truncRight exactly
	// as an uncapped served title would. Measured before this line existed: a 10003-rune
	// path-shaped title with combining marks made ONE rebuildSessionsTable take 1.11s.
	//
	// Pre-existing rather than introduced by the fallback — but capping only the served side left
	// the two sources asymmetric for no reason, and the fix is the same constant. Re-capping an
	// already-capped harvested title costs a length check.
	//
	// TRIMMED BEFORE THE CUT, and that is a CORRECTNESS requirement rather than tidiness — but note
	// the history, because this comment asserted the OPPOSITE order for the same reason and was
	// wrong twice over.
	//
	// The cap was first written claiming "truncation cannot turn a non-blank title blank". False. It
	// was then rewritten to trim AFTER the cut, with a comment claiming that trimming cannot
	// introduce the failure it prevents "because it only ever removes whitespace". Also false, and
	// the measurements are the refutation: a title whose first MaxTitleLen runes are whitespace with
	// real text after them loses the text to the CUT, and is then emptied by the TRIM. Measured at
	// MaxTitleLen = 80: 40 leading spaces keeps 50 runes, 79 keeps just "m", and 80 or more returns
	// "" — for U+0020, U+00A0 and U+3000 alike. sessionHasTitle reads sessionTitle, so that flips a
	// named row to unnamed and re-harvests ~/.claude every ~3 minutes for the life of the process:
	// the permanent-rescan cost this file documents as the price of an UNNAMABLE session, charged
	// instead to a session with a perfectly good name.
	//
	// Trimming first dissolves the problem rather than guarding against it. Leading whitespace is not
	// part of the name, so it should never have consumed the rune budget; once it does not, no amount
	// of it can push real text past the cut. capTitleRunes trims both ends of its input before
	// measuring, and trims again after cutting to drop whitespace the cut newly exposed.
	//
	// clipTitle (core/observe/claude/harvest.go) trims after its own cut and is safe doing so for a
	// reason that does not transfer: normalizeTitle has already collapsed every whitespace run ahead
	// of it, so it never sees a leading run long enough to matter. Neither string here has been
	// through that — which is the same precondition the cluster walk below exists because this side
	// lacks.
	return capTitleRunes(sanitizeLabel(m.sessionsData[id].Title))
}

// capTitleRunes cuts s to at most claude.MaxTitleLen runes, on a grapheme-cluster boundary, and
// trims the result.
//
// ONE HELPER BECAUSE THERE ARE TWO SOURCES. sessionTitle caps the harvested title and
// sessionTitleFor caps the served one; both need the same bound, and the two open-coded copies that
// preceded this differed only in which string they read. The cost of the cap is documented at both
// call sites — briefly, truncLeft/truncRight bound their OUTPUT but search quadratically over their
// INPUT whenever a zero-width rune disables the fast path, so an uncapped title is seconds per row
// per rebuild on the UI goroutine.
//
// utf8.RuneCountInString RATHER THAN len([]rune(s)), because the common case is a title already
// under the cap and that case must allocate nothing. The rune slice was 160 B/op and 232ns for a
// 40-rune title against 0 B/op and 155ns here, on a path reached ~3-4x per row per 2s tick. The
// slice is still built when a cut is actually needed, where its cost is the point rather than
// overhead. Note the trade: counting first walks the string twice, so on the over-long path this is
// ~45% slower than slicing immediately (780µs vs 1.13ms at 200k runes). That is the right way round
// — the short path is the one that runs constantly, and the long path is a cut this is preventing
// the expensive consequences of, not an operation to optimise.
//
// THE CUT LANDS ON A CLUSTER BOUNDARY, and that is not a nicety. clipTitle upstream says a plain
// rune cut is safe for it ONLY BECAUSE normalizeTitle has already removed every character that
// binds to its neighbour — combining marks, modifiers, joiners, regional indicators. NEITHER string
// here has been through that: the harvested title is re-read from a file that may have been
// rewritten, and the served title comes from core/session's sanitizeTitle, whose own doc says it
// KEEPS combining marks "so café survives". So a blind cut at MaxTitleLen can sever a cluster,
// leaving a dangling accent bound to whatever precedes it, half an emoji ZWJ sequence, or one
// regional indicator of a flag — a title that renders as something nobody wrote. Walking back off
// the binders costs a few rune tests on the only path that ever cuts.
//
// normalizeTitle is unexported and staying that way, so this is a boundary walk rather than a reuse.
// It is deliberately narrower than normalising: the goal is only that the cut not land mid-cluster,
// not that clusters be removed.
func capTitleRunes(s string) string {
	// TRIMMED FIRST, so surrounding whitespace never consumes the rune budget. See sessionTitle's
	// note: trimming after the cut let 80 leading spaces empty a genuinely-named title, because the
	// cut kept only the spaces and the trim then removed them. Whitespace is not part of a name, so
	// the fix is for it not to count rather than to detect the damage afterwards.
	//
	// This also makes the early return below exact. Trimming after it would mean a string of 80
	// real runes plus one trailing space took the cut path to produce a result the early return
	// could have returned untouched.
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= claude.MaxTitleLen {
		return s
	}
	r := []rune(s)
	cut := claude.MaxTitleLen
	// Walk back off a cut that would orphan r[cut] from what precedes it.
	//
	// ONE CLUSTER IS THE WHOLE BUDGET, and saying so is the entire reason this is a loop with a
	// floor rather than a while-it-binds walk. Two earlier versions each walked until the rune at
	// the cut stopped binding, and each emptied a non-blank title on a long enough run:
	//
	//   - Regional indicators pair into flags, so in a run only every second one binds. Treating
	//     all of them as binders walked to index 0: 41 consecutive flags capped to "".
	//   - Combining marks STACK — "a mark cannot follow a mark" is false, and this comment used to
	//     assert it as the loop's bound. "a" + 100 U+0301 capped to "", and "a"*60 + 40 marks lost
	//     21 runes against a documented bound of one step.
	//
	// Both are the same bug reached by different routes, and the shared root cause is that the
	// binding predicate answers "is this rune part of a cluster?" while the loop needed "where does
	// this cluster START?". An unbounded walk answers the first question repeatedly and can consume
	// the whole title; a degenerate cluster is not a reason to return nothing.
	//
	// Emptying the title is the failure that matters, not the shortening: sessionHasTitle reads
	// sessionTitle, so a named row that caps to "" reads as unnamed and restarts the permanent
	// ~3-minute re-harvest. That is the same failure f5a0a615 fixed for whitespace, which is why
	// this walk now has a floor that cannot reach 0 while any non-binder precedes the cut.
	//
	// So: find the start of the cluster the cut lands inside, then take all of it or none of it.
	// clusterStart stops at the first non-binder, so it reads at most the ONE cluster straddling the
	// cut — and the ONLY way to lose the whole title is a string that is a single degenerate cluster
	// from index 0, a title with no base character at all, which no longer costs anything because
	// the fallback below keeps MaxTitleLen runes of it rather than returning "".
	//
	// "One cluster" is the right bound but NOT a small number, and it is worth being exact because a
	// reviewer read the earlier wording as promising one step. A degenerate cluster can be as long as
	// the cut, so the scan is min(cluster length, MaxTitleLen) — 80 steps for "a" + 100 marks, and
	// still 80 for "a" + 2000, which is the part that matters: it does not grow with the title. On
	// every shape where ordinary text precedes the cut it is 1 step, because r[cut] is a base
	// character and the loop returns immediately.
	// A LEADING DEGENERATE CLUSTER IS THE ONE SHAPE WITH NO BOUNDARY TO CUT ON, and it is decided
	// STRUCTURALLY — by clusterStart reporting 0 — rather than by noticing afterwards that the
	// result came out blank. That distinction is the whole reason this reads the way it does. A
	// blanket "if the cap emptied a non-blank title, cut bluntly instead" guard was written here
	// first and rejected: it rescues the output of ANY walk, including the unbounded one this
	// replaces, so every mutation test of the walk passed under it. A fallback that makes the
	// broken and the fixed implementation indistinguishable is not a safety net, it is a mask.
	//
	// clusterStart == 0 means r[0] itself binds to what precedes it, i.e. the title opens with
	// marks or an odd flag half and there is no earlier boundary in the string. Cutting bluntly at
	// MaxTitleLen splits that cluster, which is the lesser evil by a wide margin: a dangling accent
	// renders as one odd glyph, whereas "" flips sessionHasTitle to unnamed and restarts the
	// permanent ~3-minute re-harvest.
	if start := clusterStart(r, cut); start > 0 {
		cut = start
	}
	return strings.TrimSpace(string(r[:cut]))
}

// clusterStart returns the index of the first rune of the grapheme cluster that r[cut] belongs to,
// or cut itself when r[cut] starts its own cluster and the cut is already on a boundary.
//
// BOUNDED BY THE ONE CLUSTER STRADDLING THE CUT, not by the title. The scan stops at the first rune
// that does not bind to what precedes it. That bound is the fix for two defects that shipped in this
// file: capTitleRunes' doc above has the measurements.
//
// NOT bounded by a constant, and the difference has been misread: a degenerate cluster can run the
// whole way back, so the worst case is min(cluster length, cut) steps — 80 for "a" + 100 combining
// marks, and still 80 for "a" + 2000, which is the property that matters. Where ordinary text
// precedes the cut it returns on the first iteration.
//
// cut MAY EQUAL len(r), meaning "the cut is past the last rune". r[cut] does not exist there, so
// there is nothing to orphan and the answer is cut itself. Handled explicitly rather than left to
// the caller: capTitleRunes only ever passes a cut strictly inside r (its early return guarantees
// len(r) > MaxTitleLen), so this arm is unreachable from the one live caller today — but the
// alternative was an index-out-of-range panic on a plausible direct call, load-bearing on a coupling
// two functions apart that nothing stated and no test pinned. bindsToPrevious' doc invites other
// callers in this package; this makes the invitation safe. A cut ABOVE len(r) is a caller bug and
// still panics, deliberately: clamping it would invent an answer for a question the caller got wrong.
//
// Regional indicators are resolved by parity rather than by the per-rune predicate, because their
// binding depends on POSITION — only the second of a pair binds. riBindsAtCut counts the preceding
// run, so an even run means r[i] opens a fresh pair and i is already a boundary.
func clusterStart(r []rune, cut int) int {
	if cut >= len(r) {
		return cut
	}
	for i := cut; i > 0; i-- {
		if r[i] >= 0x1F1E6 && r[i] <= 0x1F1FF {
			if !riBindsAtCut(r, i) {
				return i
			}
		} else if !bindsToPrevious(r[i]) {
			return i
		}
	}
	return 0
}

// riBindsAtCut reports whether the regional indicator at r[cut] is the SECOND half of a flag, so
// cutting before it would leave a bare letter where a flag was.
//
// Regional indicators are the one binder whose binding depends on POSITION rather than on the rune:
// they pair left to right, so in "🇺🇸🇬🇧" the first and third bind to nothing while the second and
// fourth complete a flag. Counting the unbroken run of them that precedes the cut gives the parity —
// an even-length run means r[cut] starts a fresh pair and the cut is already on a boundary.
//
// The scan is bounded by the run, not by the title: it stops at the first non-RI rune. A title that
// is nothing but flags is the worst case, and it is exactly the case a global walk got wrong.
func riBindsAtCut(r []rune, cut int) bool {
	run := 0
	for i := cut - 1; i >= 0 && r[i] >= 0x1F1E6 && r[i] <= 0x1F1FF; i-- {
		run++
	}
	return run%2 == 1
}

// bindsToPrevious reports that r renders as part of the cluster started by the rune before it, so a
// cut immediately before r would split that cluster.
//
// Mn/Me/Mc ARE THE MARK CATEGORIES, and together they approximate Unicode's Grapheme_Extend: Mn
// non-spacing (a combining accent, a variation selector), Me enclosing, Mc spacing-combining (a
// Devanagari vowel sign, which occupies a column but still belongs to the letter before it). Two
// more bind without any category saying so: U+200D ZWJ is what joins the codepoints of a
// multi-part emoji, and Regional_Indicator runes pair up into flags, so a cut between two leaves a
// bare letter where a flag was.
//
// REGIONAL INDICATORS ANSWER TRUE HERE BUT ARE NOT DECIDED HERE. Their binding depends on position,
// not on the rune — only the second of a pair binds — and this function sees one rune with no
// context. capTitleRunes routes them through riBindsAtCut instead; this arm remains so the
// predicate's answer to "can this rune ever bind?" stays honest for any other caller.
//
// THE ZWJ ARM IS UNREACHABLE FROM BOTH PRODUCTION CALLERS TODAY, and deliberately kept. Since
// sanitizeLabel began delegating to pipeline.IsControlRune, a ZWJ is replaced by U+FFFD before either
// cap site sees it, and U+FFFD binds to nothing. So the hazard the paragraph above describes cannot
// currently occur on either path. It is kept because the unreachability is incidental — it depends on
// a sanitiser in another file continuing to treat ZWJ as a control rune, which is a rule about
// terminal safety and not about grapheme clusters. A future caller that caps an unsanitised string,
// or a narrowing of IsControlRune, restores the hazard silently. A dead rune test is cheaper than
// that coupling.
//
// DELIBERATELY NOT Lm. Modifier LETTERS (U+02B0 ʰ and the like) read as though they belong here and
// Grapheme_Extend excludes them — and the category also holds runes that legitimately START a
// cluster, U+02BB ʻokina being a letter in Hawaiian orthography. Including Lm would walk the cut
// back off an ordinary word character. Sk (modifier SYMBOLS, U+02C7 ˇ) is out for the same reason.
// A test fixture built on U+02B0 is what surfaced this; it was the fixture that was wrong.
//
// A SMALL EXPLICIT SET rather than a grapheme-segmentation library: abctl has no such dependency,
// this is one cut on one display path, and being slightly conservative only moves the cut earlier by
// a rune or two. The failure it prevents is a severed cluster; the cost of over-walking is a shorter
// title.
func bindsToPrevious(r rune) bool {
	if unicode.In(r, unicode.Mn, unicode.Me, unicode.Mc) {
		return true
	}
	return r == '‍' || (r >= 0x1F1E6 && r <= 0x1F1FF)
}

// noServedTitle is the served-title argument for a row that cannot have one. Only the cached-only
// rows qualify, and only because the server has stopped listing those sessions entirely.
//
// It exists because sessionTitleFor takes the served title as a parameter, so passing "" silently
// disables the fallback and compiles. The parameter stays — the live row loop already holds the
// summary, and resolving it inside would put a scan of m.sessions in the per-row render path — so
// the one legitimate empty argument says so by name instead.
//
// "CANNOT HAVE ONE" IS ABOUT TODAY'S STRUCTURE, not a claim that nothing better is possible. A
// session named ONLY by the proxy keeps its name while listed and loses it the moment it becomes
// cached-only, so an operator watches a title vanish from a row whose events are deliberately
// preserved (the #870 scenario). Remembering the last-seen served title would fix that, and nothing
// here does: neither this constant nor cachedOnlySessionIDs retains anything from the summary that
// went away. Deferred rather than overlooked — it means holding title state across list refreshes,
// which is a store question and not a rendering one.
const noServedTitle = ""

// sessionTitleFor names a session for DISPLAY, falling back to the title the proxy served.
//
// Two independent sources, and each covers what the other cannot. The harvest reads Claude Code's
// transcripts on this machine; /v1/sessions carries a title the proxy derived from the session's
// own events. So a session the harvester has no transcript for can still be named, and that is not
// a rare shape: on a laptop where every blank harvested entry was checked, all of them belonged to
// an agent with no Claude Code transcript tree — one that does route through the proxy, so a served
// title existed for exactly those. A blank cell was never "this session has no name", only "no name
// where abctl was looking".
//
// HARVEST WINS when both exist. Deliberately a fixed precedence and not a judgement about which
// string is better: both sides pick a title through their own ranking, both may change, and this
// says as little as possible about either mechanism so that it does not go stale when they do.
// What it costs is worth knowing — the two rankings do not agree on every session, so a row can
// show a harvested title while the proxy held one an operator would have preferred (an explicit
// /rename is the clearest case). That is accepted for now, pending what operators report; the
// precedence is one line to invert if it turns out wrong.
//
// THE FALLBACK IS ONLY HERE, not in sessionTitle. Every backoff predicate in session_metadata.go
// judges "unnamed" through sessionTitle, so this deliberately leaves a server-titled row reading as
// unnamed to them: the harvest keeps looking for the harvested title, at the cost of a periodic
// transcript scan. That cost is the trade, not a leak — but it is a PERMANENT steady state on
// exactly the rows this fallback serves, not a transient one. An agent with no Claude Code
// transcript tree is the case that motivated the feature, and for it the harvest can never
// succeed, so the backoff pins at untitledBackoffCap and re-walks ~/.claude every 3 minutes for
// the life of the process (~0.7s for a first full scan, per the README). Accepted because the
// alternative is the re-harvest stopping on a served title and never picking up a transcript that
// appears later; worth revisiting if that walk ever becomes expensive enough to matter.
//
// THROUGH titleIsBlank rather than == "", because a harvested " " renders as nothing and filling
// nothing is the whole point. titleIsBlank's own doc anticipates this seam.
//
// served IS SANITISED TOO, and not on the assumption that it arrives clean. The proxy does trim and
// cap it, but /v1/sessions is unauthenticated and the title is folded from caller-supplied event
// content — sessionTitle's CWE-150 reasoning applies to this string at least as much as to the
// harvested one.
//
// THAT INDEPENDENCE IS NOW ACTUALLY TRUE, and it was not when the fallback first landed.
// sanitizeLabel then replaced the BIDI overrides and isolates but not the BIDI MARKS (U+200E/200F,
// U+061C) or the zero-widths (U+200B/200C/200D/2060/FEFF), all of which pipeline.IsControlRune names
// and core/session's sanitizeTitle does strip. So the only thing keeping a mark out of the cell was
// the producer this comment claimed not to rely on — a claim the code contradicted, for exactly the
// class of rune whose whole purpose is to make the rendered order differ from the byte order.
// sanitizeLabel delegates to IsControlRune now; see its doc.
//
// AN EMPTY served DOES NOT ALWAYS MEAN "the proxy derived none". A session that arrives on the
// event stream before it appears in a list refresh gets a stub SessionSummary with a zero Title
// (app.go's streamed-event path), so its row shows no served title until the next poll fills the
// summary in — under two seconds, and it self-corrects with no help from here. Worth knowing only
// because it makes a blank cell briefly ambiguous; nothing downstream needs to tell the two apart.
// served IS CAPPED HERE, and this is the only thing that bounds it. The harvested title arrives
// already capped at claude.MaxTitleLen runes, and the existing cross-module cap test asserts that
// against a harvested fixture — a path the served title never takes. The proxy does cap at its own
// maxTitleLen, but that is 80 in ANOTHER MODULE, unexported on purpose (its doc: "deliberately NOT
// that constant"), so nothing here can assert it and no client should assume it. /v1/sessions is
// unauthenticated and operator-pointed, so a title of any length is a thing abctl can be handed.
//
// What that costs without a cap is not a wide cell — truncLeft/truncRight bound the OUTPUT — it is
// the SEARCH inside them. Their fast path is disabled by any zero-width rune, and a served title
// keeps its combining marks, so a long one runs a quadratic scan: measured, 20003 runes takes 4.50s
// on ONE call, and 200003 did not finish in two minutes. That is the UI goroutine, once per row per
// rebuild. Capping the input is what REMOVES THE QUADRATIC TERM — not what makes the whole path
// flat, which an earlier version of this comment claimed and the code does not do. Two passes still
// run over the UNTRUNCATED string before the cap can apply: sanitizeLabel builds a new string with
// b.Grow(len(s)), and the cap's own rune count walks it. Both are linear, so a 200k-rune title still
// costs ~1.6MB of transient allocation and a couple of walks per row per rebuild.
//
// TWO IS THE FLOOR, AND IT WAS THREE. Review caught sessionTitleFor calling titleIsBlank(served) and
// then sanitizeLabel(served), which sanitised the untruncated string TWICE — ~3.2MB, not ~1.6MB, for
// the same answer, because sanitizeLabel is idempotent and the second copy was pure waste. It now
// sanitises into a local and blank-checks that (see blankSanitized). Anything that reintroduces a
// second sanitise, or a second []rune conversion, doubles the number in this paragraph; there is no
// benchmark that would notice, so read the call sites. Linear is the
// difference between a laggy column and an unusable one — the measured 4.50s at 20003 runes was the
// quadratic search, not these — but "flat" was wrong, and the honest bound is what a future reader
// needs when deciding whether to cap EARLIER, at the decode in apiclient, where it would be.
//
// AT THE HARVESTER'S CAP, reusing claude.MaxTitleLen rather than a new number: it is what the other
// source is already capped to, so the two titles get the same budget and the column keeps one rule.
// Runes, not columns, matching what that constant counts — the width re-measure downstream is what
// turns either into a fitted cell.
//
// THE TWO ARGUMENTS ARE UNCHECKED AGAINST EACH OTHER, and nothing here can detect a mismatch. served
// is meant to be THIS id's Title, but it is passed in rather than looked up, so pairing one session's
// id with another's title compiles and renders a confident wrong name — the worst failure shape this
// column has, because a title is what an operator uses to pick a row before acting on it. The two
// live callers are safe by construction (the row loop reads both from one SessionSummary; sessionLabel
// resolves served from the same id it passes), and that is the invariant a third caller must preserve:
// RESOLVE served FROM id, never from an index, a neighbouring row, or a previous frame's summary.
// servedTitle(id) exists for exactly that and is the right thing to reach for. The parameter stays
// because resolving inside would put a scan of m.sessions in the per-row render path — see
// noServedTitle — so this is a documented contract rather than an enforced one.
func (m *model) sessionTitleFor(id, served string) string {
	// PRECEDENCE IS DECIDED ON THE RAW HARVESTED TITLE, not on what the cap left of it, and that
	// separation is the fix for a real inversion. This read m.sessionTitle(id) — sanitised AND capped
	// — and asked whether THAT was blank. So any route by which the cap could blank a non-blank
	// harvested title also silently handed the row to the served one: sessionTitleFor(85 spaces +
	// "real", "served-name") returned "served-name", showing the proxy's name to an operator who has
	// been told in two docs and a core/session comment that HARVEST WINS. Worse than the missing
	// title it replaced, because a wrong name is acted on and a blank one is not.
	//
	// The whitespace defect behind that specific case is fixed in capTitleRunes, so the two
	// formulations now agree on every input known to differ. This one is still the right question to
	// ask: "did the harvest name this session?" is about the harvest, and routing it through a
	// length cap makes a display bound into a precedence rule. Whatever the cap does to a long
	// title, it cannot move the row to the other source.
	//
	// sanitizeLabel is still applied — blankness must be judged on what RENDERS, which is
	// titleIsBlank's whole reason for sanitising before trimming ("\t" paints a visible glyph, so it
	// is not blank). Only the cap is out of the decision.
	if raw := m.sessionsData[id].Title; !titleIsBlank(sanitizeLabel(raw)) {
		return m.sessionTitle(id)
	}
	// SANITISE ONCE, INTO A LOCAL, then use it for both the blank check and the cap.
	//
	// SANITISE BEFORE CAPPING, matching sessionTitle, so the cluster walk inside the cap sees the
	// string that will actually render. sanitizeLabel is rune-for-rune, so the two orders agree on
	// WHERE the cut lands — but only one of them agrees on WHAT is at the cut: sanitising afterwards
	// would walk back off a combining mark that sanitizeLabel then replaces with U+FFFD, a standalone
	// glyph that never needed the walk. Ordering it this way keeps one rule for both sources.
	//
	// ONE PASS, NOT TWO: this used to call titleIsBlank(served) and then sanitizeLabel(served) on the
	// next line. Both sanitise, and sanitizeLabel is O(len) with a b.Grow(len(s)) on the UNTRUNCATED
	// input, so the pair built two full copies of a served title to reach one answer — doubling the
	// transient allocation this function's own cost note bounds, per row per rebuild. Idempotent made
	// it harmless but not free.
	clean := sanitizeLabel(served)
	// BLANK-CHECKED ON THIS SIDE TOO, because titleIsBlank was applied to the harvested title and
	// not to this one — so a whitespace-only served title painted spaces into the cell while
	// sessionLabel, which blank-checks what this returns, showed the bare id. The same session named
	// two different ways by two callers of one accessor. Returning "" makes the cell agree with the
	// header, and "" is what both already do when nothing names a session.
	//
	// blankSanitized rather than titleIsBlank because clean has already been through sanitizeLabel —
	// and the ORDER that check documents is preserved, not dropped: sanitising happened above, and the
	// trim happens here, which is the same sanitise-then-trim titleIsBlank performs internally.
	if blankSanitized(clean) {
		return ""
	}
	return capTitleRunes(clean)
}

// sessionTitleCell is sessionTitleFor fitted to the TITLE column, truncated from the LEFT.
//
// Truncating from the left because the titles are mostly paths. bubbles truncates every cell
// from the right, which on "/Users/person/src/cortex/.worktrees/claudesessions" keeps
// "/Users/person/src/cor…" — the half every session on the machine shares, and none of the
// half that says which one this is. Keeping the tail instead gives "…es/claudesessions".
//
// Pre-truncated here rather than left to bubbles because bubbles offers no choice of side, so
// the cell has to arrive already short enough. That means reading the LIVE column width, not
// the declared one: layout() fits the columns and this reads back what it decided, which is
// why layout() must also rebuild these rows — see its call to rebuildSessionsTable.
//
// Left alone when it is not a path: a title is prose, and prose reads from the left.
func (m *model) sessionTitleCell(id, served string, titleW int) string {
	title := m.sessionTitleFor(id, served)
	if title == "" {
		return title
	}
	// EVERY CELL IS BOUNDED, whichever branch it takes. Prose was returned untouched on the
	// reasoning that it reads from the left and so loses nothing to a right-side cut — true, but
	// it left the cell UNBUDGETED, and bubbles then truncated it in the fixed-width box anyway.
	// A Windows-style path took this branch too and came out 63 columns wide against a budget of
	// 11: that was previously dismissed as unreachable on this platform, which confused where the
	// string comes FROM with where it is rendered. Titles are harvested text; the renderer does
	// not get to assume their shape.
	if !looksLikePath(title) {
		if titleW <= 0 {
			// FAIL SAFE, not wide. A non-positive budget means the caller could not tell us how
			// much room there is, and returning the full title then hands an unbounded cell to a
			// table that has to cut it somewhere — the one thing every other branch here exists
			// to prevent. Traces as unreachable today (the column is admitted with a floor, and
			// layout only shrinks to it), which is precisely why it should not be the branch that
			// quietly reintroduces an unbudgeted cell if that ever changes.
			return ""
		}
		// From the RIGHT for prose, which reads left-to-right — the opposite of a path, whose
		// tail is the distinguishing end.
		return truncRight(title, titleW)
	}
	// THE WIDTH IS PASSED IN, not read back off the live table. The rows are built before
	// SetColumns installs the new header, so reading the table gave the PREVIOUS width: on a
	// narrowing resize the cell came out too wide, reached the table's fixed-width box, and was
	// re-truncated from the RIGHT — destroying the tail this function exists to keep, which is
	// the inversion it was written to prevent. Bounded in practice, since the next poll rebuilt
	// it correctly, so it showed only while polling was stalled.
	//
	// Passing it in also removes the header lookup, which had to go through headerTitle and
	// silently returned the untruncated title if it ever found nothing.
	if titleW <= 0 {
		// Same as the prose branch above: no budget means no cell, not an unbounded one.
		return ""
	}
	return truncLeft(title, titleW)
}

// looksLikePath reports whether a title should be truncated from the LEFT, keeping its tail.
//
// A LEADING SLASH IS NOT ENOUGH. It was the whole test until session titles started coming from the
// user's own prompts, and a typed slash command begins with one too: "/review <url> carefully" was
// left-truncated to "…pull/1101 carefully", discarding the command name — the one part of it a reader
// needs. That inverts this file's own rule that prose reads left-to-right.
//
// The distinction that matters is whether the string's LEAF is the identifying part. A filesystem path
// has more than one segment and no spaces in its first one; a slash command is one word followed by
// arguments. So a title qualifies only if it has a second "/" before any space — which "/review x"
// does not, and "/Users/somebody/src" does.
//
// Deliberately simple, and wrong in one direction on purpose: "/tmp foo" reads as prose and would be
// right-truncated. A single-segment path is not something Claude Code records as a cwd, and the cost
// is a cell cut at the other end rather than anything unbounded.
//
// THAT MISS IS PINNED, by TestLooksLikePath_SingleSegmentPathWithASpaceReadsAsProse, as
// characterization rather than endorsement — it also measures the cost, so widening this rule is a
// visible diff. The same test covers the opposite direction, which is NOT a miss: "/review a/b" is a
// slash command whose argument holds a slash, and prose is the right answer. One clause produces both,
// so neither behaviour can change alone.
//
// ANY UNICODE SPACE SEPARATES, not just " " and "\t". An earlier IndexAny(rest, " \t") saw no
// separator in "/review\u00a0docs/plan.md", found the second "/", and left-truncated the slash
// command — the exact regression above, reachable through a non-breaking space, an ideographic space,
// or any of the U+2000 block. Only from a stale or hand-edited metadata file today, since the
// harvester now folds every unicode space to U+0020 before writing; this does not rely on that,
// because a renderer should not assume its input came from the current harvester.
func looksLikePath(title string) bool {
	if !strings.HasPrefix(title, "/") {
		return false
	}
	rest := title[1:]
	if i := strings.IndexFunc(rest, unicode.IsSpace); i >= 0 {
		rest = rest[:i]
	}
	return strings.Contains(rest, "/")
}

// zeroWidthFree reports whether every rune in s occupies at least one display column.
//
// What licenses the prefix skip in truncLeft and truncRight: with no zero-width rune, a run of more
// than n runes cannot fit n columns, so n runes from the relevant end is an exact lower bound and
// every index beyond it is provably too wide to measure. One zero-width rune breaks that, and a
// differential test against the pre-skip implementation caught exactly that case.
//
// Checked structurally rather than by measuring: the classes runewidth gives zero columns are
// non-spacing and enclosing marks, format characters and controls, plus the modifier symbols this
// project already drops upstream. Cheap — one pass, no allocation — and the answer is yes for every
// title, so the skip is not hypothetical.
func zeroWidthFree(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) ||
			unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Sk, r) {
			return false
		}
	}
	return true
}

// truncRight clips s to n DISPLAY COLUMNS keeping the LEFT end, marking the cut with a
// trailing ellipsis.
//
// The mirror of truncLeft, for prose: a title reads left-to-right, so the opening words are
// the distinguishing end and the tail is what goes.
//
// Its own function rather than a fix to trunc, which this replaced at one call site: trunc
// counts RUNES and has four other callers (session ids, an events line, a test helper) that
// budget in characters and would change behaviour under a column-aware rewrite. Measured, the
// rune count is wrong by about 2x for the input this path actually gets: an 11-column budget
// returned 21 columns of CJK and 14 of emoji. Narrowing trunc's own gap is a separate change
// to those callers; this one bounds the cell that harvested titles flow into.
//
// The symptom it fixes is over-truncation, not a broken frame: the table's fixed-width box
// re-cuts an over-wide cell, so the line stayed at terminal width — a CJK title just lost
// characters it had budget for, and lost them at a point the renderer chose. Titles are
// model-generated text, so CJK and emoji are expected rather than exotic.
func truncRight(s string, n int) string {
	if lipgloss.Width(s) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	// Runes are dropped from the end until the remainder fits the budget. One at a time
	// rather than by arithmetic, for truncLeft's reason: a rune is 1 or 2 columns wide, so
	// there is no index that can be computed from the total.
	//
	// The ELLIPSIS PLUS THE HEAD is measured, not the head plus one, so a trailing combining
	// mark that fuses onto the ellipsis cannot push the result over budget.
	//
	// AND IT SKIPS AHEAD, for truncLeft's reason, under the same guard: a head longer than n runes
	// cannot fit n columns PROVIDED no rune is zero-width, so the first n runes are then an exact
	// lower bound. This had the identical quadratic shape — 2500 runes 39ms, 20000 2.30s on one call
	// — and is reachable the same way, since looksLikePath needs a leading "/" so a RELATIVE cwd
	// routes down this branch while just as uncapped.
	//
	// AND THE GUARD'S SLOW PATH RUNS HERE TOO, for the reason truncLeft now spells out: a title
	// served by /v1/sessions keeps its combining marks, so an accent or an emoji turns the skip off.
	// Prose is the COMMON shape for a served title — it is folded from a user's own message — so this
	// branch is the likelier of the two to meet one. Correctness is unaffected; the cost is bounded
	// by sessionTitleFor's cap, not by anything here.
	r := []rune(s)
	if len(r) > n && zeroWidthFree(s) {
		r = r[:n]
	}
	for i := len(r); i > 0; i-- {
		if out := string(r[:i]) + "…"; lipgloss.Width(out) <= n {
			return out
		}
	}
	return "…"
}

// truncLeft clips s to n DISPLAY COLUMNS keeping the RIGHT end, marking the cut with a leading
// ellipsis.
//
// The mirror of trunc, for values whose distinguishing end is the last one: a path, where every
// sibling shares the prefix. n < 1 yields "" and n == 1 yields just the ellipsis, so the result
// never exceeds the cell it was measured for.
func truncLeft(s string, n int) string {
	// DISPLAY COLUMNS, not runes. Every caller budgets in columns — a table cell's fitted
	// width, a header's remaining space — and a rune count is a different number the moment
	// the title is not ASCII. Titles are model-generated text or filesystem paths, which this
	// package documents as content nobody here controls, so CJK and emoji are expected rather
	// than exotic: measured, a 14-column budget returned 27 columns of CJK.
	//
	// The two failures differ and both are bad. In a header the over-wide string wraps the terminal,
	// costing a body row.
	//
	// In the table the cell is cut a SECOND time, and the result is worse than either end alone.
	// bubbles renders every cell through runewidth.Truncate(value, width, "…"), which keeps the HEAD
	// and appends its own ellipsis — so an over-wide left-truncation is cut again from the right,
	// with this function's leading ellipsis already in place. Measured on a 14-column budget:
	//
	//	truncLeft, correct     "…日日日日日日"      13 columns, tail kept, one ellipsis
	//	measured in runes      "…日日日日日日日日日日日日日"  27 columns
	//	  ...after the table   "…日日日日日日…"     14 columns, TWO ellipses, tail gone
	//
	// So the failure is not that left-truncation inverts into right-truncation — it is that the cell
	// ends up cut at BOTH ends and keeps the middle, which is the one part of a path that identifies
	// nothing. At a narrow budget it degenerates completely: an 11-column cell renders "…日日日日…" and
	// a 2-column one renders "……".
	//
	// lipgloss.Width, mirroring padLeft, which measures this way for the same reason.
	//
	// AND IT IS NOT THE LAST MEASUREMENT THE CELL MEETS. bubbles v1.0.0 runs runewidth.Truncate over
	// every cell before styling, and runewidth does not skip ANSI. Measuring here in display columns
	// is therefore necessary but not sufficient: it holds only while the cell is PLAIN, which for a
	// title is guaranteed upstream (core/observe/claude normalises a harvested one; sessionTitleFor
	// runs sanitizeLabel over a served one) and asserted below.
	// Styling a title would put escape bytes inside that second budget and collapse a narrow cell to
	// a lone ellipsis.
	if lipgloss.Width(s) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	// Runes are dropped from the front until the remainder fits the budget less the ellipsis.
	// One at a time rather than by arithmetic: a rune's width is 1 or 2, so there is no index
	// that can be computed from the total.
	//
	// BUT THE SEARCH SKIPS AHEAD FIRST, because measuring from i == 0 made this quadratic: each
	// iteration rebuilt the whole remaining tail and handed it to lipgloss.Width, so the cost grew
	// with the square of the input. Measured on one call, dropping runes one at a time from the
	// front: 2500 runes 36ms, 5000 142ms, 10000 572ms, 20000 2.33s — four times the input for
	// sixteen times the work. The same shape stripHarnessSpans was flagged for; these two were left.
	//
	// THE SKIP IS GUARDED, because the obvious bound is not universally true. "Every rune is at
	// least one column, so the last n runes are an exact lower bound" holds only while no rune is
	// ZERO columns — and combining marks, joiners and variation selectors all are. A differential
	// test against the old implementation caught it at n == 1: on a string of combining marks the
	// bound skipped past marks the one-at-a-time search would have kept, and the two returned
	// different bytes.
	//
	// So skipping is conditional on the premise: zeroWidthFree reports whether s contains any
	// zero-width rune, and only then is the prefix provably untestable.
	//
	// THE SLOW PATH IS REACHABLE, and the comment here used to deny it. It read that a title never
	// contains a zero-width rune because core/observe/claude drops every Mn/Me/Cf/Cc/Sk — true of a
	// HARVESTED title, and no longer the only kind. A title served by /v1/sessions goes through
	// core/session.sanitizeTitle instead, which deliberately KEEPS combining marks (its own doc: "so
	// café survives"), and pipeline.IsControlRune covers C0/C1/DEL/BIDI/Cf but not Mn/Me/Sk. So an
	// accented word or any ordinary emoji — U+FE0F VARIATION SELECTOR-16 is Mn — makes zeroWidthFree
	// false and runs the quadratic search this comment claimed never executes. Measured on one call
	// with a combining mark every other rune: 2503 runes 72ms, 5003 287ms, 10003 1.12s, 20003 4.50s,
	// which is worse than the ASCII figures above because each measurement now folds a mark too.
	//
	// The guard is STRUCTURAL, so the answer stays correct either way — this is about the premise,
	// not a bug. It is called out because the premise is what someone would delete the guard on, and
	// because the cost is only bounded by sessionTitleFor capping its input; see the cap there.
	r := []rune(s)
	start := 0
	if len(r) > n && zeroWidthFree(s) {
		start = len(r) - n
	}
	for i := start; i < len(r); i++ {
		// The ELLIPSIS PLUS THE TAIL is measured, not the tail plus one. A tail that begins
		// with a combining mark or a variation selector fuses onto the ellipsis, so the pair
		// is narrower than the sum of its parts — and assuming the ellipsis always adds
		// exactly one column let the result exceed the budget. Measuring what is actually
		// returned cannot be wrong about it.
		if out := "…" + string(r[i:]); lipgloss.Width(out) <= n {
			return out
		}
	}
	return "…"
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

// sessionModelCell is the MODEL column's cell: the first model this session was seen using,
// straight off the server's SessionSummary.
//
// "—" MEANS NOT KNOWN, per the standing rule emptyCell states, and two very different rows land
// on it: a session with no inference events (MCP-only traffic — there is no model to show), and
// a proxy older than the field. A blank string would read as the column failing rather than as
// an answer, which is the difference this package keeps apart everywhere a figure can be absent.
//
// NO CLIENT-SIDE FALLBACK, unlike sessionTokens beside it, and the asymmetry is deliberate.
// Tokens can be recomputed from cached events because a SUM over a suffix still approximates the
// whole; the model is FIRST-WINS server-side, so the first model in abctl's cache is not the
// first in the session — abctl attached partway through — and scanning the cache would render a
// stand-in that is neither the server's answer nor honestly labeled. The cached-only rows
// (server no longer lists the session) take the em dash for the same reason: the events are
// there, but the fact they could support is not derivable from them.
func sessionModelCell(model string) string {
	if model == "" {
		return emptyCell
	}
	return model
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
	if micros == 0 || negativeCost(micros) {
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
		if cell := decorate(r.text); lipgloss.Width(cell) <= budget {
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
// The cost is real and measured: the columns now disappear below 73 columns, where an ordinary
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
	//
	// MODEL IS EXCLUDED TOO, because it yields to these columns rather than competing with them
	// (see sessionsShowModel, which reads this gate): admitting it here would let an
	// unseatable MODEL shrink the very cells this function is protecting.
	cols := make([]table.Column, 0, len(sessionsColumns()))
	title := sessionsShowTitle(termWidth)
	for _, c := range sessionsColumns() {
		t := headerTitle(c)
		if t == "TITLE" && !title || t == "MODEL" {
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
	// AND MODEL LAST, after everything else has claimed its room: it yields to TITLE and the
	// money columns alike (see sessionsShowModel, which reads both gates), so dropping it after
	// them keeps the narrow table byte-identical to the pre-MODEL layout rather than shifting
	// every floor by MODEL's width. The per-event model remains available in the events pane.
	if !sessionsShowModel(termWidth) {
		out := make([]table.Column, 0, len(cols))
		for _, c := range cols {
			if headerTitle(c) == "MODEL" {
				continue
			}
			out = append(out, c)
		}
		cols = out
	}
	return growSessionsTitle(cols, termWidth)
}

// sessionsShowTitle reports whether this terminal is wide enough to afford TITLE.
//
// Decided FIRST, and the money columns read this rather than the reverse: TITLE is the column
// this pane gained and the only one whose absence nothing else recovers, where a session's cost
// is also in the Usage pane, `abctl cost` and the spend strip. So COST and SAVED yield to it and
// return at the width where the whole set holds every minimum at once.
func sessionsShowTitle(termWidth int) bool {
	if termWidth <= 0 {
		return true
	}
	//
	// What the function does: TITLE is granted when the FITTED set leaves it its floor, measured
	// without the money columns because those are what yield to it. TITLE renders from 73; COST
	// and SAVED are absent from 73 to 96 and return at 97, where the whole set holds every
	// minimum at once.
	//
	// Why in that order: TITLE is the column this pane gained and the only one whose absence
	// nothing else recovers — a session's cost is also in the Usage pane, `abctl cost` and the
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
	// legible title has its room. They are absent from 73 to 96 for that reason and return at
	// 97, where the full set holds every minimum at once. MODEL IS EXCLUDED FOR THE SAME
	// REASON IN REVERSE: it yields to TITLE (see sessionsShowModel), so seating it here would
	// let the column this gate protects be narrowed by one that gives way to it.
	keep := make([]table.Column, 0, len(sessionsColumns()))
	for _, c := range sessionsColumns() {
		switch headerTitle(c) {
		case "COST", "SAVED", "MODEL":
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
func growSessionsTitle(cols []table.Column, termWidth int) []table.Column {
	slack := termWidth - tableWidth(cols)
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
	// and collapsing. Column PRESENCE was already monotonic; this is what makes width so.
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
	// AND MODEL'S ROOM IS NOT SLACK EITHER, for the same reason at a wider band: MODEL is
	// absent below its own threshold and TITLE absorbs the room, then returns (at 113 as
	// measured) and the fitter claws TITLE back to its floor — the same non-monotonicity,
	// arriving by the same door. Reserved with cellPadding spelled out as 2 to match the
	// money branch's idiom rather than reaching for the constant across the file boundary.
	if !sessionsShowModel(termWidth) {
		reserved := 0
		for _, c := range sessionsColumns() {
			if headerTitle(c) == "MODEL" {
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
