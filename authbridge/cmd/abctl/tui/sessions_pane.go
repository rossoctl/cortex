package tui

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

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

// contextWindowTokens is the denominator every gauge is drawn against.
//
// FIXED AT ONE MILLION, which is the largest window on any path this proxy sees — the Claude
// [1m] beta — and the same figure authlib/usage and authlib/pricing already reason against.
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
// say "/Users/snible/" and distinguish nothing. Wider terminals grow it from slack — see
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
			row = append(row, m.sessionTitleCell(s.ID, titleW))
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
		row = append(row, padLeft(contextGauge(m.sessionContextFor(s.ID), contextW), contextW))
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
			row = append(row, m.sessionTitleCell(id, titleW))
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
		row = append(row, padLeft(contextGauge(m.sessionContextFor(id), contextW), contextW))
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
	return sanitizeLabel(m.sessionsData[id].Title)
}

// sessionTitleCell is sessionTitle fitted to the TITLE column, truncated from the LEFT.
//
// Truncating from the left because the titles are mostly paths. bubbles truncates every cell
// from the right, which on "/Users/snible/src/cortex/.worktrees/claudesessions" keeps
// "/Users/snible/src/cor…" — the half every session on the machine shares, and none of the
// half that says which one this is. Keeping the tail instead gives "…es/claudesessions".
//
// Pre-truncated here rather than left to bubbles because bubbles offers no choice of side, so
// the cell has to arrive already short enough. That means reading the LIVE column width, not
// the declared one: layout() fits the columns and this reads back what it decided, which is
// why layout() must also rebuild these rows — see its call to rebuildSessionsTable.
//
// Left alone when it is not a path: a title is prose, and prose reads from the left.
func (m *model) sessionTitleCell(id string, titleW int) string {
	title := m.sessionTitle(id)
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
	// title is guaranteed upstream (authlib/observe/claude normalises every one) and asserted below.
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
	// zero-width rune, and only then is the prefix provably untestable. A title reaching this file
	// never contains one — authlib/observe/claude drops every Mn/Me/Cf/Cc/Sk, asserted there across
	// the whole Unicode range — so the fast path is what actually runs. The fallback exists because
	// these are general helpers with callers that make no such promise, and a wrong answer is worse
	// than a slow one.
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
	// ONE STATEMENT, REPLACING SIX. Earlier revisions stacked a paragraph per attempt without
	// retiring the last, so this block named thresholds of 78, 80, 82, 98 and 104 — none of them
	// the answer — and two of its paragraphs were near-verbatim duplicates contradicting each
	// other about whether a change raised or lowered the threshold. In a file where the comments
	// are the only statement of design intent, that leaves the next reader unable to tell which
	// paragraph is live.
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
	// 97, where the full set holds every minimum at once.
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
