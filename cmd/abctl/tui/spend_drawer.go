package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/usage"
)

// The spend drawer is the strip EXPANDED IN PLACE, not a pane.
//
// WHY NOT A PANE. The breakdown behind the strip's figures — which models, which
// endpoints, which agents — was designed as a seventh pane, and a pane is the wrong
// container for it twice over. Seven panes is already a lot to hold in your head, and cost
// read IN CONTEXT beats cost read by navigating away from whatever you were looking at:
// the whole point of the strip is that spend arrives before the data rather than after a
// keypress. Expanding under the strip keeps the table on screen, which a pane cannot.
//
// Overlays and drawers are the established precedent for "more detail without leaving" —
// help_overlay.go and edit_overlay.go are both non-panes. This is smaller than either: a
// few rows, no cursor, no scroll, no focus.
//
// IT HOLDS NO STATE OF ITS OWN beyond a bool, an axis and a span. Everything it renders
// comes from the snapshot the strip already polled, so opening it costs no request and
// closing it loses nothing. Changing the axis or the span does trigger a refetch, because
// the server does the folding — a client-side regroup would be a second implementation of
// the aggregator's fold, which is the one thing this package refuses to duplicate.
const (
	// spendDrawerSeries is how many series get their own row before the rest are folded
	// into an "(other)" band.
	//
	// Three, because the drawer's whole justification is that it fits under the strip
	// without displacing the table: three rows plus the band plus the key hints is five,
	// which a 26-row terminal can spare and a 20-row one cannot. A scrolling drawer would
	// be a pane wearing a smaller name.
	spendDrawerSeries = 3

	// spendDrawerMinHeight is the terminal height at which the drawer may open: the
	// strip's own floor, the drawer's rows, and the separator between them.
	//
	// Below it the answer is "no", not "a table with two visible rows": the drawer exists
	// to be read ALONGSIDE the data, and one that squeezes the data out has defeated its
	// reason for not being a pane.
	//
	// DERIVED, for the reason spendDrawerLines states about itself: written as a literal it
	// does not follow the drawer's height, and a floor that lags costs the table a row at
	// the very size it exists to protect.
	//
	// spendDrawerLines, not spendDrawerLinesFor: a floor has to admit the TALLEST form, or
	// widening the terminal would squeeze the table.
	spendDrawerMinHeight = spendStripMinHeight + spendDrawerLines + dividerLines

	// spendDrawerLines is the drawer's MAXIMUM height — the two-column form. What layout()
	// actually holds back is spendDrawerLinesFor(width), which is this at a width that
	// affords the tier column and one row less below it.
	//
	// spendDrawerSeries named rows, plus the "(other)" band, plus the hint line. Derived rather
	// than written as 5 so the two cannot drift: renderSpendDrawer emits exactly this many at
	// full height, and layout() reserving fewer is not a cosmetic slip — the view comes out
	// taller than the terminal and the footer goes off the bottom, which is the failure
	// spendStripReservesRow's own doc describes for one row.
	//
	// A HEADER row, the taller of the two columns, and the hint line. The left column is
	// tierPanelLines and the right is spendDrawerSeries plus the "(other)" band.
	//
	// The child row is reserved UNCONDITIONALLY, even though it renders only when a
	// provider reports a split: a height that followed the data would move the footer when
	// one session reports reasoning and the next does not.
	spendDrawerLines = max(tierPanelLines, spendDrawerSeries+1) + 2
)

// drawerTwoColumn reports whether the panel has room for the tier column beside the
// series column.
//
// ONE PREDICATE, because it decided the reservation and the render independently: the
// same `width >= spendDrawerTwoColumnMin` test was written in spendDrawerLinesFor and
// again in renderSpendDrawer, and a height that disagreed with what was drawn is the
// defect this file's whole comment discipline is about.
func drawerTwoColumn(width int) bool { return width >= spendDrawerTwoColumnMin }

// spendDrawerLinesFor is the reservation at a given WIDTH.
//
// The tier column only exists in two columns, so only there does the panel need room
// for tierPanelLines; reserving that height unconditionally costs a narrow terminal a
// body row for a row it cannot draw.
//
// A function rather than a constant because WIDTH is known wherever this is called.
// The same argument does not extend to varying the height by whether a split was
// reported — that is data, and the height must not follow it: see renderTierRows.
func spendDrawerLinesFor(width int) int {
	left := spendDrawerSeries + 1 // one column: the ranked series plus "(other)"
	if drawerTwoColumn(width) {
		left = max(tierPanelLines, spendDrawerSeries+1)
	}
	return left + 2 // the header and the hint line
}

// spendDrawerAxes are the breakdown axes `g` cycles through.
//
// NO GroupNone in the cycle, unlike the Usage pane's grouping. This drawer's only content
// IS the breakdown, so "no breakdown" is an empty drawer — a state the user can already
// reach, more directly, by pressing `$` again. The cycle offers the three axes that answer
// a different question about the same spend: which MODEL cost this, which ENDPOINT it was
// billed through, and which AGENT asked for it.
//
// GroupSession is deliberately absent too: the sessions picker is the surface with a row
// per session, and it now carries COST and SAVED columns of its own, summed server-side
// over each session's whole life rather than over this drawer's rolling window. Two
// per-session answers with different scopes on two surfaces is how a reader comes to think
// one of them is wrong.
var spendDrawerAxes = []usage.Group{usage.GroupModel, usage.GroupEndpoint, usage.GroupAgent}

// spendDrawerWindows are the spans `w` cycles through: EXACTLY THE BAND'S FOUR.
//
// THE RING-ONLY RESTRICTION IS GONE, and both of the reasons for it have expired. It was
// {15m, 1h, 6h}, on the grounds that a "today" option "would render the same number twice"
// and that a ledger query on a keypress was too expensive.
//
// The first was true while the band showed a single day headline and the drawer had no span
// of its own to name. It is not true now: the band shows four TOTALS and the drawer shows a
// BREAKDOWN, so pointing it at the month answers "what is this month's spend made of", which
// no cell on the band can say at any width.
//
// The second was really a worry about the twenty-second POLL LOOP, not about user demand. The
// drawer has its own chain now, polled only while it is open and at the slow cadence, and
// `abctl cost --window 7d` has always done exactly this read on demand from a shell. A
// keypress that costs a disk walk is a keypress someone asked for.
//
// 15m and 6h are dropped. They are ring diagnostics rather than budget spans, and six entries
// to cycle through to reach four useful ones is a worse surface than four. Both remain
// reachable through `abctl cost --window` and the Usage pane.
//
// KNOWN LIMITATION, ON A DEPLOYMENT WITH NO COST LEDGER. Three of these four spans are
// ledger-backed, and without a ledger — Kubernetes by default — the server answers all three
// from the six-hour ring instead, clamped to the window asked for. So `w` has ONE span the
// store can really distinguish there (the hour), and the other three stops return the same
// clamped answer under three different captions. The band already detects and discloses that
// per cell, via servedAsRequested; THIS surface does not, so the drawer shows the clamped
// breakdown without saying it is one.
//
// Left as it is deliberately, and scoped: the target is a local install, where the ledger is on
// by default and all four spans are real. Dropping the ring spans is what a ledger-less
// deployment lost, and re-adding them for that case would put back the clutter this set exists
// to remove. The two ways out, if the Kubernetes case ever matters: disclose here the way the
// band does, or make the cycle's contents depend on whether a ledger answered.
//
// STRINGS, not durations, because three of the four are symbolic boundaries that
// time.ParseDuration cannot express — the same reason spendSpanDefs.window is a string.
var spendDrawerWindows = []string{
	spendSpanDefs[spanHour].window,
	spendSpanDefs[spanToday].window,
	spendSpanDefs[span7d].window,
	spendSpanDefs[spanMonth].window,
}

// spendDrawerSpans is the same list as spendDrawerWindows, as SPANS, and it is the list
// windowSpan resolves against.
//
// Two slices rather than one derivation because the strings above are what the request carries
// and the spans here are what the cadence and the label come from; they are checked against
// each other by TestSpendDrawerWindows_AreTheBandsSpans rather than trusted to stay in step.
var spendDrawerSpans = []spendSpan{spanHour, spanToday, span7d, spanMonth}

// window is the span the drawer currently requests.
//
// A PLAIN INDEX NOW, and that is a footgun retired rather than a simplification. windowStep
// used to be an OFFSET from spendDrawerWindowDefault, purely because the old slice had 1h in
// the MIDDLE: zero is the natural zero value of a counter, an index of zero pointed at the
// first entry, and the first entry was 15m — so a freshly constructed model would silently
// have polled a span nobody asked for. The offset existed to make zero mean "the middle".
//
// With the band's four spans in ascending order the hour IS first, so the zero value is
// already the right answer and the offset has nothing left to correct. The wrap survives as
// defence in depth; see wrapIndex.
func (s *spendState) window() string {
	return spendSpanDefs[s.windowSpan()].window
}

// windowSpan is which of the band's four spans the drawer is currently pointed at.
//
// THE DRAWER'S WINDOWS ARE THE BAND'S SPANS, one for one — spendDrawerWindows is built from
// spendSpanDefs — so the span is the honest identity of the current selection and the window
// string is one of its fields. Named because a second reader needs it: the drawer's poll
// cadence comes from the span it is showing, not from one constant for all four.
func (s *spendState) windowSpan() spendSpan {
	return spendDrawerSpans[s.windowStepIndex()]
}

// pollInterval is how often THIS drawer selection refreshes: the cadence of the span it is
// showing.
//
// ONE CONSTANT FOR ALL FOUR WAS WRONG IN BOTH DIRECTIONS, and the hour is the case that shows
// it: the band's hour cell refreshes every twenty seconds and the breakdown underneath it every
// five minutes, so the two disagreed for up to five minutes about the same span — a breakdown
// that does not add up to the figure above it is the failure bandSpanCell's own doc argues
// against. The month keeps its slow cadence, which is what the single constant was chosen for:
// re-folding thirty-one day files every twenty seconds to redraw four rows.
func (s *spendState) pollInterval() time.Duration {
	return spendSpanDefs[s.windowSpan()].pollInterval()
}

// windowResolution asks the ring for a single bucket, and omits the parameter for a symbolic
// window — the ledger serves those as one bucket and ignores the resolution entirely.
func (s *spendState) windowResolution() time.Duration {
	if _, ok := parseWindowSpan(s.window()); ok {
		return spendResolution
	}
	return 0
}

// windowStepIndex resolves windowStep to a slice index, through the same wrap axis() uses.
func (s *spendState) windowStepIndex() int {
	return wrapIndex(s.windowStep, len(spendDrawerWindows))
}

// axis is the breakdown the strip asks the server to fold for.
//
// Requested unconditionally, whether or not the drawer is open, so opening it renders
// immediately from the snapshot already in hand instead of showing an empty frame for a
// poll interval. It costs nothing to carry: usage.Snapshot sums Totals from the raw buckets
// BEFORE folding, so every figure on the strip is byte-identical under any axis.
func (s *spendState) axis() usage.Group {
	return spendDrawerAxes[wrapIndex(s.groupIdx, len(spendDrawerAxes))]
}

// wrapIndex reduces a monotonically incremented counter to a slice index.
//
// WRAPS RATHER THAN CLAMPS, which is the difference that matters. axis() used to return
// GroupModel for anything out of range, so an index that ran past the end collapsed to the FIRST
// axis and stayed there — indistinguishable from a correct wrap on the step right after the end,
// and wrong on every step after that. Both cyclers keep the counter in range anyway, so this is
// defence in depth: it means a future change to either one cannot produce a resolution that
// silently answers "model" forever.
//
// Negative is floored into range for the same reason: a resolution that cannot be out of range
// has no silent failure to hide.
func wrapIndex(i, n int) int {
	i %= n
	if i < 0 {
		i += n
	}
	return i
}

// spendDrawerHost reports whether the current pane can host the drawer, and says why not when
// it cannot.
//
// THE REASON IS RETURNED, not inferred by the caller, because the refusal is shown to the user
// and a wrong reason is worse than none: `$` on a 60-row pod picker used to flash "no room for
// the strip on a terminal this short", which is a height complaint about a pane condition. A
// reader resizes, nothing changes, and the key looks broken.
func (m *model) spendDrawerHost() (bool, string) {
	return spendDrawerHostPane(m.pane)
}

// spendDrawerHostPane is the pane rule on its own, so the [?] overlay can ask it
// without a model. The overlay only lists `$` where `$` works, and it used to
// hardcode "(not on usage)" beside the key instead — a second copy of this switch,
// in prose, on a line that rendered on every pane including the two where the
// drawer is refused for an entirely different reason.
func spendDrawerHostPane(pane paneID) (bool, string) {
	switch pane {
	case paneNamespaces, panePods:
		// The strip itself does not draw here: these run before a connection exists, so there
		// is no spend to summarise, let alone break down.
		return false, "spend: nothing to break down until a pod is connected"
	case paneUsage:
		// THAT PANE IS ALREADY THE BREAKDOWN, with its own axis, window and metric cycles — and
		// its key handler runs before this one and returns, so the drawer's own keys could never
		// reach it. A drawer whose hint line advertises [w] on the one pane where `w` belongs to
		// something else is a lie printed on screen.
		return false, "spend: the usage pane is the breakdown — use its own [b] and [w]"
	}
	return true, ""
}

// spendDrawerReservesRows reports whether layout() must hold spendDrawerLinesFor(width)
// back.
//
// THE FLAG AND THE HEIGHT, deliberately blind to the pane — the same asymmetry
// spendStripReservesRow has, for the same reason: layout() is called from the WindowSizeMsg
// handler, and no pane transition re-runs it. There is no setPane choke point to hook (the pane
// is assigned in twenty-odd places), so a pane-aware reservation would go stale on the next
// transition — and stale in the dangerous direction, because a drawer that DRAWS against an
// unreserved body overflows the terminal.
//
// The cost is larger than the strip's: while the drawer is open, a pane that cannot host it
// renders its reservation shorter than it could. That is a visible loss where the strip's was
// one invisible row, and it is still the right trade — the alternative is a footer pushed off
// the bottom, and the state is transient and user-initiated.
func (m *model) spendDrawerReservesRows() bool {
	return m.spend.expanded && m.height >= spendDrawerMinHeight
}

// spendDrawerVisible reports whether the drawer draws its rows.
//
// Requires the strip: the drawer is an expansion OF it, and a breakdown floating under a
// title with no summary above it would be a pane after all. So a terminal too short for the
// strip is too short for this, and `$` on a 19-row terminal does nothing rather than
// producing a headless breakdown.
func (m *model) spendDrawerVisible() bool {
	if ok, _ := m.spendDrawerHost(); !ok {
		return false
	}
	return m.spend.expanded && m.spendStripVisible() && m.height >= spendDrawerMinHeight
}

// toggleSpendDrawer handles `$`.
//
// REFUSES ON A SHORT TERMINAL, and says so, rather than setting a flag whose effect the
// user cannot see. A key that silently does nothing reads as a broken key; a key that
// explains the height requirement is a key the user stops pressing for a reason.
//
// The refusal does not set expanded, so growing the terminal later does not surprise the
// user with a drawer they asked for minutes ago and were told they could not have.
// IT RETURNS A COMMAND NOW, because the drawer owns a poll chain. Opening starts it and
// closing stops it: the breakdown is the only thing that reads it, and its span can be a
// ledger window, so a chain left running behind a closed drawer would walk day files for
// nobody. Closing invalidates rather than merely ceasing to reschedule — a reply already in
// the air outlives the keypress by up to spendFetchTimeout, and storing it would leave a
// snapshot the next open would render before its own first poll lands.
// THE CLOSE BRANCH IS GATED ON WHAT IS ON SCREEN, not on the flag, for the reason esc is: the
// flag survives a move to a pane that cannot host the drawer and a resize below the height floor,
// so `$` on the Usage pane closed a drawer the user could not see and gave no sign it had. Open it
// on Sessions, press `u`, press `$` — and the breakdown was gone on the way back, with no flash to
// say why, which is precisely the "a key that silently does nothing reads as a broken key" failure
// this function's own doc argues against, arriving through the other door. Off screen, `$` now
// falls through to the refusals below and says which one applies.
func (m *model) toggleSpendDrawer() tea.Cmd {
	if m.spendDrawerVisible() {
		m.spend.expanded = false
		m.spend.drawer.invalidate()
		// Give the rows back, for the reason the open path takes them.
		m.layout()
		return nil
	}
	// The PANE first, because its refusal has nothing to do with height and a height message
	// there sends the reader to resize a terminal that was never the problem.
	if ok, why := m.spendDrawerHost(); !ok {
		m.setFlash(why)
		return nil
	}
	if !m.spendStripVisible() {
		m.setFlash("spend: no room for the band on a terminal this short")
		return nil
	}
	if m.height < spendDrawerMinHeight {
		m.setFlash(fmt.Sprintf("spend: the breakdown needs %d rows, this terminal has %d",
			spendDrawerMinHeight, m.height))
		return nil
	}
	m.spend.expanded = true
	// The reservation is made by layout(), which otherwise only runs on a resize — so without
	// this the drawer draws into a body sized for a closed one and the footer goes off the
	// bottom until the terminal happens to change size.
	m.layout()
	// Fetch immediately AND schedule: the drawer is opened to be read now, so waiting out its
	// span's cadence would show an empty breakdown for twenty seconds on the hour and five
	// minutes on the month.
	m.spend.drawer.invalidate()
	return tea.Batch(m.fetchSpendDrawer(), spendDrawerTick(m.spend.drawer.tickGen, m.spend.pollInterval()))
}

// cycleSpendAxis handles `a` while the drawer is open, and refetches.
//
// `a` FOR AXIS, and emphatically not `g` for group. `g` is globally "go to top" (see goTop), and
// the Usage pane already rejected `g` for its own breakdown on exactly that reasoning —
// "shadowing a vim-style motion inside one pane is worse than picking a second-choice mnemonic".
// This drawer is designed to stay OPEN alongside the table, so it would shadow the motion for
// most of a session rather than inside one pane.
//
// Nor `b`, which that pane chose: `b` is global page-up here (see the pgup case), so it would
// shadow a worse motion than `g` did. `a` is unbound.
//
// A refetch rather than a client-side regroup: the server folds, and doing it here would be
// a second implementation of the aggregator's arithmetic that could disagree with the
// figures above it. The old snapshot stays on screen until the new one lands, so the drawer
// does not blink through an empty frame — it is a breakdown of the same traffic either way.
func (m *model) cycleSpendAxis() tea.Cmd {
	m.spend.groupIdx = (m.spend.groupIdx + 1) % len(spendDrawerAxes)
	// THE PREVIOUS SPAN'S ERROR IS DROPPED HERE, and it has to be dropped rather than left to
	// the reply that will overwrite it. applySpendLoaded stores both fields together, so a
	// failed poll leaves err set and snap nil — and the diagnostic is captioned with the
	// label, which now names the span just asked for. Measured: a failed month poll then `w`
	// printed "breakdown unavailable for LAST 1H: <the month's error>", attributing a failure
	// to a request nobody has made yet, for the whole round trip.
	//
	// The SNAPSHOT deliberately survives (see above) because it is a breakdown of the same
	// traffic either way. An error is not: it describes one request.
	m.spend.drawer.err = nil
	return m.fetchSpendDrawer()
}

// cycleSpendWindow handles `w` while the drawer is open, and refetches.
//
// IT NO LONGER MOVES THE BAND, and that separation is the point of the drawer having its own
// chain. The two used to share one poll, so `w` silently changed what the band's window cell
// reported — press it once and "LAST 1H" became "LAST 15M" with nobody having asked for a
// different band. The band now holds four fixed spans and this changes exactly the breakdown
// it is pointed at.
//
// Refetches rather than regrouping client-side: the server folds, and doing it here would be
// a second implementation of the aggregator's arithmetic that could disagree with the figures
// above it. The old snapshot stays on screen until the new one lands, so the drawer does not
// blink through an empty frame.
func (m *model) cycleSpendWindow() tea.Cmd {
	m.spend.windowStep = (m.spend.windowStep + 1) % len(spendDrawerWindows)
	// Same reason as cycleSpendAxis: see there.
	m.spend.drawer.err = nil
	return m.fetchSpendDrawer()
}

// plainFigures lifts a run of fixed strings into the fitter's type. None of them can be partial,
// so none of them degrades.
//
// HERE RATHER THAN IN spend_strip.go, where it used to live: the strip stopped using it when its
// fixed readings became individual figures in one list, and a helper with no caller is dead code
// a linter is right to flag. The drawer's hint line is its only consumer now, so it lives with it.
func plainFigures(ss ...string) []stripFigure {
	out := make([]stripFigure, 0, len(ss))
	for _, s := range ss {
		out = append(out, plainFigure(s))
	}
	return out
}

// drawerLabels are the axis and span the drawer's hint line reports.
//
// FROM THE SNAPSHOT, not from what was last requested. m.spend.axis() and .window() are what the
// NEXT poll will ask for, so reading them at render time repainted the label the instant `a` or
// `w` was pressed — one poll ahead of the rows underneath it, which were still grouped and spanned
// the old way. A label that describes something other than the data beside it is the mislabel this
// whole surface is written against, and the strip already follows the rule: print the window the
// SERVER reported, never the one requested.
//
// The exposure was one poll, not indefinite — applySpendLoaded stores msg.snap on success and nil
// on error, so a failed fetch clears the snapshot rather than leaving a stale one — and one frame
// of a wrong label is still a wrong label.
//
// FALLING BACK to the requested values when the snapshot cannot say: no snapshot yet, or a server
// that echoed no group. The alternative is a hint line with a blank axis, which reads as a
// rendering fault rather than as an answer in flight.
// spanLabelFor renders a REQUESTED window the way the hint line wants it.
//
// A span the band already names gets the band's own label, so "month" reads as MONTH in both
// places rather than as two spellings of the same period. A duration the band does NOT name is
// compacted through formatWindowLabel ("6h0m0s" -> "6h"), and anything else is carried
// through as given.
//
// THE MATCH IS servedAsRequested's, NOT ==, and that distinction is the whole invariant. The
// hour's window is the one span written as a DURATION, and a server answers a duration window by
// stringifying it: "1h0m0s" against a requested "1h". An exact comparison missed that, fell
// through to formatWindowLabel, and returned "1h" — so the caption flipped from LAST 1H to 1h
// the moment the first poll landed, while the band cell directly above it still said LAST 1H.
// Two spellings of one period in one region, which is exactly what this function is for.
//
// Used for the fallback AND for the served label — drawerLabels prefers what the server
// actually served, for the reason its own doc gives, and routes it through here.
func spanLabelFor(window string) string {
	for span := spendSpan(0); span < numSpendSpans; span++ {
		if servedAsRequested(spendSpanDefs[span].window, window) {
			return spendSpanDefs[span].label
		}
	}
	if d, ok := parseWindowSpan(window); ok {
		return formatWindowLabel(d)
	}
	return window
}

// drawerAge is how long ago the breakdown on screen answered, and whether that is longer than
// its own span's cadence allows.
//
// THE SAME RULE THE BAND USES — twice the interval; see spanReadings — so one wedged chain
// reads the same way wherever it is. Without this, drawer.lastFetch was recorded and never
// rendered: a drawer whose poll had been failing for an hour showed an hour-old breakdown
// under a caption that named the span and nothing about when it was true. The band spent the
// whole of its own review round on exactly that, one row up.
//
// A ZERO lastFetch IS NOT STALE. Nothing has answered yet, which is a different state from a
// wedged chain and is already visible: the drawer draws no rows.
func (s *spendState) drawerAge(now time.Time) (time.Duration, bool) {
	if s.drawer.lastFetch.IsZero() {
		return 0, false
	}
	age := now.Sub(s.drawer.lastFetch)
	return age, age > 2*s.pollInterval()
}

func (m *model) drawerLabels() (usage.Group, string) {
	axis, window := m.spend.axis(), spanLabelFor(m.spend.window())
	snap := m.spend.drawer.snap
	if snap == nil {
		return axis, window
	}
	// SANITISED FOR THE SAME REASON snap.Window IS, twelve lines down, and it was not: Group is
	// server-supplied JSON that reaches the terminal verbatim through drawerHeaders, which renders
	// "BY " + ToUpper(axis) and clips it to width — and clipRow shortens a row without neutralising
	// anything in it. Measured: a Group of "model\x1b[2J\x1b[H" produced "   BY MODEL\x1b[2J\x1b[H"
	// with the clear-screen and cursor-home intact, and a newline in it returned two rows where the
	// drawer's reservation allows one.
	//
	// Pre-existing rather than introduced here, but this function is rewritten on this branch and
	// the sanitize suite beside it made the field look covered, which is worse than an obvious gap.
	if l := sanitizeLabel(string(snap.Group)); l != "" {
		axis = usage.Group(l)
	}
	// FROM THE DRAWER'S OWN SNAPSHOT, not from spendSummary. It used to read
	// spendSummary().WindowLabel, which was right while the drawer and the band shared one
	// poll and is wrong now that they do not: the summary's label describes the BAND's hour,
	// so a drawer showing a month would have been captioned "1h" — the mislabel this
	// function exists to prevent, arriving through the function meant to prevent it.
	//
	// Sanitised because snap.Window is server-supplied and reaches the terminal verbatim, and
	// compacted through the same parse-then-format the summary used: the server answers a
	// duration window as "1h0m0s", which is literally true and three columns wider than the
	// "1h" the caller asked for. A SYMBOLIC window ("today", "month") does not parse as a
	// duration and is carried through as the server spelled it.
	if l := sanitizeLabel(snap.Window); l != "" {
		// Through spanLabelFor, so a span the band names reads the SAME here: a drawer showing
		// the month says MONTH, not "month", and the reader is not left matching two spellings
		// of one period across two rows of the same region.
		window = spanLabelFor(l)
	}
	// THE AGE RIDES ON THE LABEL, and only when the chain is late — the same place and the same
	// condition as the band's, so "LAST 1H 6m" means one thing on both rows.
	if age, stale := m.spend.drawerAge(time.Now()); stale {
		window += " " + formatSpendAge(age)
	}
	return axis, window
}

// drawerRow is one rendered series line.
type drawerRow struct {
	label  string
	counts usage.Counts
}

// spendDrawerRows folds a snapshot's series into the rows the drawer draws.
//
// RANKED ON COST, not on requests or tokens. This is the spend drawer: the row worth
// reading first is the one that cost the most, which for mixed traffic is emphatically not
// the one with the most requests — a handful of opus calls outranks hundreds of haiku ones.
//
// Which is why this does NOT rank through collectSeries, the Usage pane's ranking. That
// function ranks by a usageMetric over the pane's own visible buckets — a window and
// resolution the operator chose for a chart, not the fixed-hour ring this drawer reads.
// The pane does now carry a cost metric (#1060), so the shared enum is no longer the
// obstacle; the window is. Ranking here is four lines.
//
// The FOLD is still foldTailSeries, which is the part worth sharing: it merges the tail into
// "(other)" AND merges any "(other)" the aggregator already produced by its own label
// capping, so there is never one band meaning "the rest" beside another meaning "the rest".
//
// Summed through usage.Counts.Add, never field by field: Add is the single summation point
// for this struct and its own doc records what happened to the out-of-module copy that
// enumerated the fields by hand — it silently dropped PricedRequests under a comment saying
// every field had to be carried.
func spendDrawerRows(snap *usage.Snapshot, keep int) []drawerRow {
	if snap == nil || len(snap.Buckets) == 0 {
		return nil
	}
	// BOTH RETURNS, and the first one is the whole reason to call this rather than truncate the
	// slice here. foldTailSeries merges an "(other)" the AGGREGATOR already produced — from its
	// own label capping — into the tail total instead of appending a second band. Hand-rolling
	// the truncation appended unconditionally, so a snapshot whose Series already held an
	// "(other)" ranking inside the top three rendered that band TWICE, each carrying the same
	// merged total: the rows then summed past the strip's headline above them by the whole tail.
	ranked, folded := foldTailSeries(snap.Buckets, rankSeriesByCost(snap.Buckets), keep)

	totals := make(map[string]usage.Counts, len(ranked))
	for _, b := range folded {
		for label, c := range b.Series {
			t := totals[label]
			t.Add(c)
			totals[label] = t
		}
	}
	// THE BAND THE FOLD MADE BUT DID NOT NAME. foldTailSeries appends "(other)" to its kept list
	// only when the tail's METRIC TOTAL is positive, while it rewrites the buckets whenever the
	// tail carried requests — and this drawer's metric is COST. So a tail of UNPRICED models (the
	// normal case for a gateway without a full rate card) produced folded buckets holding the band
	// and a ranked list that never mentioned it, and the lookup below dropped the row: 100
	// requests and 2.7M tokens invisible, with the figures above them unchanged.
	//
	// That is the invariant this function's own doc protects, inverted — instead of summing PAST
	// the headline the rows summed under it, which is the quieter of the two failures and the one
	// nothing would have noticed.
	//
	// FIXED HERE RATHER THAN IN foldTailSeries, which is shared with the Usage pane's stacked
	// chart. That pane's metric is tokens or requests, so its tail total is positive whenever the
	// tail exists and it never meets this case; changing when the shared helper emits a band would
	// alter a surface this fix is not about.
	if _, folded := totals[tailLabel]; folded && !hasSeries(ranked, tailLabel) {
		ranked = append(ranked, seriesKey{label: tailLabel})
	}
	out := make([]drawerRow, 0, len(ranked))
	for _, s := range ranked {
		c, ok := totals[s.label]
		if !ok {
			// Defensive, and it has been wrong once: this comment used to say it could not fire,
			// while it was silently dropping the "(other)" band for an unpriced tail — the case
			// the append above now covers. A skipped row here is data vanishing off a money
			// surface, so if this ever fires again it is a defect and not a tidy fallback.
			continue
		}
		out = append(out, drawerRow{label: s.label, counts: c})
	}
	// Stable under equal cost so the rows do not reshuffle under the reader between polls.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].label == tailLabel {
			return false
		}
		if out[j].label == tailLabel {
			return true
		}
		return out[i].counts.CostMicros > out[j].counts.CostMicros
	})
	return out
}

// hasSeries reports whether a ranked list already names a label.
func hasSeries(ranked []seriesKey, label string) bool {
	for _, s := range ranked {
		if s.label == label {
			return true
		}
	}
	return false
}

// spendDrawerTwoColumnMin is the width below which the panel shows one column.
//
// THE TIER COLUMN IS THE ONE THAT YIELDS, so the panel degrades to exactly the per-model
// drawer that shipped before this feature: an addition gives way to the existing contract,
// never the reverse. Measured from the parts rather than chosen, so it moves with them.
//
// EVERY PART NAMED, which it was not: this read `tierColumnWidth + 2 + 36` while its own doc
// claimed "plus its gutter", and drawerColumnGutter was not in the sum — so at the threshold the
// series column got 33 columns rather than the 36 intended. The literal 36 also predates the series
// table: "enough for a model label and two figures" was a soft estimate when the figures had no
// fixed widths, and now that they do the arithmetic can simply be stated. A label, the money column
// and the request column, with their gutters, is 40.
//
// The leading 2 is paneView's indent, not a gutter; both are charged because the row carries both.
const spendDrawerTwoColumnMin = tierColumnWidth + drawerColumnGutter + 2 +
	drawerLabelWidth + len(stripGap) + drawerMoneyWidth + len(stripGap) + drawerReqWidth

// tierColumnWidth is the left column's share. Fixed rather than proportional so the model
// labels to its right do not reflow every time a tier figure changes width.
//
// DERIVED FROM THE ROW IT HOLDS, not a magic number. It was 34, chosen when a row was a label, a
// bar and a left-flushed figure. Adding the share column left that one column short of a full bar,
// so tierBarBudget would have quietly degraded to the HALF bar at every terminal size — and a
// six-column bar on a 140-column terminal is the state a live panel was measured in even before
// the share existed, because the old 34 was already below the old budget's own threshold of 35.
//
// Summing the parts means the column and the budget cannot disagree, and that adding a field to
// the row is a compile-time change to both rather than a silent degradation in one.
const tierColumnWidth = tierLabelWidth + 1 + tierPctWidth + 1 + tierBarWidth + 1 + tierMoneyWidth

// drawerColumnGutter separates the tier column from the series column.
//
// DECLARED, because it used to be an accident. The layout is `"  " + %-*s(tierColumnWidth) + series`
// with no gap in it at all, and it looked like there was one only because tierColumnWidth was 34
// against rows that rendered 25 — nine columns of padding nobody had asked for. Deriving the width
// from the row's own parts removed that slack and the two columns went flush: measured,
// "$56.51claude-opus-5". A gutter that exists only while a constant is too big is not a gutter.
//
// Three columns, matching stripGap, so the space BETWEEN the panels is the same as the space
// between figures inside one.
const drawerColumnGutter = 3

// drawerTotals is snap.Totals with a nil snapshot answered rather than dereferenced. The
// caller may hold nil before the first poll answers, exactly as spendDrawerRows may.
func drawerTotals(snap *usage.Snapshot) usage.Counts {
	if snap == nil {
		return usage.Counts{}
	}
	return snap.Totals
}

// drawerHeaders names the two columns.
//
// The right one names the CURRENT AXIS — "BY MODEL", "BY ENDPOINT", "BY AGENT" — which is
// what the tree glyphs were gesturing at before they were dropped, and which the hint line
// otherwise says only in brackets. The left one is constant because tiers are always tiers:
// that asymmetry is the point, since one column answers "on what" and the other "by whom".
func drawerHeaders(axis usage.Group, twoCol bool, width int) string {
	right := "BY " + strings.ToUpper(string(axis))
	if !twoCol {
		// THREE SPACES, matching what fitStripFigures(" ", ...) puts in front of the rows
		// below — `label + "  "`. Two would leave the header one column left of its column.
		return clipRow("   "+right, width)
	}
	return clipRow(fmt.Sprintf("  %-*s%s", tierColumnWidth+drawerColumnGutter,
		"WHERE IT WENT", right), width)
}

// renderSpendDrawer returns the drawer's rows, ready to join under the strip.
//
// Rows rather than one string, so the caller composes them with the strip and the body in
// one JoinVertical and nothing here needs to know the terminal's height budget.
//
// Empty when there is nothing to break down. An open drawer over a snapshot with no series
// draws its hint line and nothing else — enough to say "this is open and there is nothing
// here", which is a different message from the drawer having failed to open.
// windowLabel is the span in the strip's own vocabulary — "1h", not time.Duration's
// "1h0m0s". Passed in already formatted rather than formatted here, so the drawer's hint and
// the strip's own figure label are produced by one function and cannot drift apart.
// err is the drawer chain's own failure, and it is a PARAMETER rather than something this
// function infers from a nil snapshot, because the two are different states that must not read
// the same. A nil snapshot before the first poll is "no answer yet"; a nil snapshot after a
// failed one is "we asked and could not find out", and drawing the second as the first is
// exactly the silence applySpendLoaded's doc forbids for the band — the chain clears snap on
// failure, so without this a broken endpoint rendered as headers over blank rows forever.
func renderSpendDrawer(snap *usage.Snapshot, err error, axis usage.Group, windowLabel string, width int) []string {
	if err != nil {
		// The reservation still has to be filled, so this is spendDrawerLinesFor(width) rows with the
		// diagnostic on the first and the hint line last — the hints stay because `w` and `esc`
		// still work, and a failed span is the moment an operator most wants to try another.
		out := make([]string, 0, spendDrawerLines)
		out = append(out, clipRow("  breakdown unavailable for "+windowLabel+": "+
			sanitizeLabel(err.Error()), width))
		for len(out) < spendDrawerLinesFor(width)-1 {
			out = append(out, "")
		}
		return append(out, fitStripFigures(" ", plainFigures(
			"[a] "+axisHint(axis),
			"[w] "+windowLabel,
			"esc closes",
		), width))
	}
	rows := spendDrawerRows(snap, spendDrawerSeries)
	// TWO COLUMNS: what the money was spent ON, and who spent it. They answer different
	// questions, and with a single model in the window the series column alone restated the
	// band's own window total and saving verbatim — a breakdown of one thing is not a
	// breakdown. The tier column says something in that case, which is the common one.
	twoCol := drawerTwoColumn(width)
	seriesWidth := width
	var tiers []string
	if twoCol {
		tiers = renderTierRows(drawerTotals(snap), tierColumnWidth)
		seriesWidth = width - tierColumnWidth - drawerColumnGutter - 2
	}

	out := make([]string, 0, spendDrawerLines)
	out = append(out, drawerHeaders(axis, twoCol, width))
	// THE LEFT COLUMN IS tierPanelLines, NOT numTierRows: the four rate tiers PLUS the
	// reasoning row that hangs under output. The right column's slots are filled by the
	// `i < len(rows)` guard below, so whichever column is shorter pads itself rather than
	// ending the loop early.
	//
	// DERIVED FROM THE RESERVATION, minus the header and the hint line, so the loop and
	// the reservation cannot disagree about the panel's height.
	//
	// The height is a layout fact and the index is a slice fact: restating the height
	// here as a bound over len(tiers) conflates them, and tiers[i] is guarded where it
	// is read instead.
	bound := spendDrawerLinesFor(width) - 2
	for i := 0; i < bound; i++ {
		// NO BRANCH GLYPHS BETWEEN THE COLUMNS' OWN ROWS. "├" and "└" once prefixed every
		// row here and implied a parent none of them had; the column headers name the
		// grouping instead. The one "└" now in the panel is the reasoning row's, which does
		// have a parent directly above it — that is the distinction, not the glyph.
		series := ""
		if i < len(rows) {
			series = fitStripFigures(" ", drawerFigures(rows[i]), seriesWidth)
		}
		if !twoCol {
			out = append(out, series)
			continue
		}
		// TRIMMED, because fitStripFigures emits its own `label + "  "` indent and the
		// outer column owns the placement here. Left in, the series text sat three columns
		// right of the header naming it — measured, not guessed: "BY MODEL" at column 36
		// against "claude-opus-5" at 39.
		// THE SAME TWO-COLUMN INDENT THE HEADER USES. The rows started at column 0 while
		// "WHERE IT WENT" started at 2, so the left column's header sat two columns right of
		// its own values — the same off-by-two measured and fixed for the right column above,
		// surviving on the other side of the panel because only the right one had a test.
		// Costs no width: the two columns are 2 + tierColumnWidth + seriesWidth either way.
		// TRIMMED ON THE RIGHT, because the gutter pads the tier column whether or not a series
		// row follows it: the fourth tier row is drawn beside an empty series slot on any window
		// with fewer than four series, and paneView passes these straight to styleMuted.Render,
		// so the padding becomes styled trailing whitespace on a line nobody can see the end of.
		// GUARDED, not assumed. UNREACHABLE TODAY — bound is len(tiers) in two columns and
		// the one-column path continues above — so no test covers it, and saying so is the
		// point: renderTierRows' row count is a contract held in another package, and this
		// is the index that would crash the render if it slipped. A short tier column pads
		// with blanks; a missing row is cosmetic where an out-of-range read is a dead TUI.
		// Same standing as addSat's overflow guard in core/usage.
		tier := ""
		if i < len(tiers) {
			tier = tiers[i]
		}
		out = append(out, strings.TrimRight(fmt.Sprintf("  %-*s%s",
			tierColumnWidth+drawerColumnGutter, tier, strings.TrimLeft(series, " ")), " "))
	}
	// The hint line is LAST and always present: it is the only place the two keys and the
	// current axis are written down, and a drawer whose controls are undiscoverable is a
	// drawer nobody changes the axis of.
	out = append(out, fitStripFigures(" ", plainFigures(
		"[a] "+axisHint(axis),
		"[w] "+windowLabel,
		"esc closes",
	), width))

	// PADDED OUT TO THE RESERVATION. layout() holds back spendDrawerLinesFor(width), and it
	// has to: a poll can land between the layout and the render, so sizing the body to the rows
	// that happen to exist right now is a race against the next snapshot. Emitting fewer lines
	// than were reserved leaves the footer floating above the bottom of the terminal — three rows
	// up for a deployment using one model, which is the common case, and six before the first poll
	// answers.
	//
	// Blank lines rather than a taller body, because the body is already sized: the drawer occupies
	// the space that was set aside for it, so the table's position does not jump when a second
	// model appears.
	for len(out) < spendDrawerLinesFor(width) {
		out = append(out, "")
	}
	return out
}

// drawerFigures is one series row's cells, in the drop order fitStripFigures applies.
//
// The LABEL is first, so it is the last thing a narrow terminal gives up — a row whose model
// name has been dropped is unreadable whatever else survives. Cost second, because this is
// the spend drawer and cost is the reason the row is ranked where it is. Requests and tokens
// are context and go last.
//
// Cost through moneyFigure, so a series row carries the same coverage and exactness caveats
// the strip's own figures do, built from THIS row's counters rather than the window's. A
// per-model total that is 3-of-40 priced must say so on its own line; borrowing the window's
// coverage would attach a caveat measured over other traffic.
// drawerColWidths are the series table's columns, in display columns.
//
// A TABLE, NOT A LIST OF FIGURES. These rows are stacked, so a reader reads DOWN them — and joined
// by a fixed gap with no columns, each row's figures began wherever the previous row's ended. The
// model name is the widest variable on the row, so "claude-opus-5" against "claude-sonnet-5" put
// two rows' money, request counts and token counts in six different places.
//
// Sized to the widest real content plus nothing: "claude-sonnet-5" is 15, "$16740.85" is 9 and is
// the widest figure a month of traffic reaches, "1187 req" is 8, "137M tokens" is 11, and
// "saved $4.40" is 11. A figure wider than its column overflows rather than truncating — see
// stripFigure.col — so these are chosen to make that rare, not to make it impossible.
const (
	drawerLabelWidth  = 16
	drawerMoneyWidth  = 9
	drawerReqWidth    = 9
	drawerTokensWidth = 11
	drawerSavedWidth  = 11
)

func drawerFigures(r drawerRow) []stripFigure {
	// POSITIONAL, so column N means the same thing on every row. Built as a fixed set of slots and
	// trimmed at the end rather than appended to conditionally: appending shifted a row's tokens
	// into the request column whenever Requests was absent, which is the whole failure this table
	// is for.
	//
	// WHICH ROWS ACTUALLY EXERCISE IT, because this doc used to say "an MCP-only row" and that is
	// the wrong example. MCP traffic has requests and no tokens and nothing priceable, so its empty
	// slots are all TRAILING — they are trimmed, and no column is held open at all. The cases that
	// need a placeholder are the ones with a filled column AFTER an empty one:
	//
	//	a saving with no token count — tool-prune removes prompt tokens from a request whose
	//	response could not be parsed, so AvoidedMicros is set while Tokens is zero
	//	a priced row reporting no request count, which leaves the request column empty under a
	//	token figure
	//
	// Both are covered; see TestDrawerFigures_AnEmptyMiddleColumnHoldsItsPlace. The placeholder
	// itself lives in stripFigure.pad, which has to spell the empty case out because padLeft and
	// padRight deliberately leave "" alone.
	var money, note, req, tokens, saved stripFigure
	figs := []stripFigure{plainFigure(r.label).inColumn(drawerLabelWidth, true)}
	switch {
	case negativeCost(r.counts.CostMicros):
		// AN IMPOSSIBLE FIGURE IS NOT A FIGURE, and this row is where that guard was missing.
		// The gate below admits anything with PricedRequests > 0, so a series summing negative
		// reached formatUSDCell and printed "$-5.0000" — a credit nobody issued, in a column of
		// costs. Every other money surface in this package refuses it through this same
		// predicate; the drawer is the newest one and the only one that did not inherit it.
		//
		// NO COVERAGE NOTE beside it, unlike the unpriced branch below: the gap may well be zero
		// here — every request priced, and the sum still impossible — so coverage is not what is
		// wrong and naming it would point a reader at the rate table.
		money = plainFigure("cost unavailable")
	case r.counts.PricedRequests > 0 || r.counts.CostMicros > 0:
		// Cents, through moneyFigureTotal: this column is scanned and compared down its length,
		// which is the side of the precision rule cents is for. It read four decimals until the
		// sessions table moved, and a drawer at "$1.0601" under a band at "$1.06" was the same
		// two-precisions-for-one-figure the rule exists to prevent, one panel lower.
		//
		// THE COMPACT FORM IN THE COLUMN AND THE CAVEATS AT THE END OF THE ROW. moneyFigureTotal's
		// full form is the marked amount followed by "(3 inexact, 40 of 100 unpriced)", and a
		// parenthesised sentence of variable length inside a column every other row aligns against
		// is what put this table out of line one row at a time. The words move to the last slot,
		// where width pressure drops them first — which is where they were always going to go.
		// The MARKER stays on the figure, so the fact never leaves the column.
		full := moneyFigureTotal(float64(r.counts.CostMicros)/1e6, "",
			gapOf(r.counts), r.counts.PriceableRequests, r.counts.IncompleteRequests, nil,
			r.counts.Saturated)
		money = plainFigure(full.compact)
		if n := moneyCaveatNote(gapOf(r.counts), r.counts.PriceableRequests,
			r.counts.IncompleteRequests, nil, r.counts.Saturated); n != "" {
			note = plainFigure(n)
		}
	case r.counts.PriceableRequests > 0:
		// A SERIES THAT PRICED NOTHING still has to say so, and this branch is the row most in
		// need of a caveat: the gate above skipped moneyFigure entirely, so a 0-of-40-priced
		// model rendered "mystery-model  40 req  900k tokens" with nothing anywhere on the line
		// saying its cost is unknown. "Each row's caveats come from its own counters" held only
		// for rows that managed to price something.
		//
		// The strip's own spelling for a figure nobody can produce, plus the gap, in the slot the
		// money figure would have taken — so the row reads left to right the same way a priced
		// one does.
		//
		// THE GAP IS THE ROW'S NOTE, not a second figure in the money slot. As two figures it
		// occupied the request column on this row and the money column on every other, which is
		// the misalignment the table exists to end; as the trailing note it sits where every
		// other row's caveats sit and yields to width pressure the same way.
		money = plainFigure("cost unavailable")
		note = plainFigure("(" + coverageNote(gapOf(r.counts), r.counts.PriceableRequests) + ")")
	case r.counts.Requests > 0:
		// NOTHING HERE COULD EVER CARRY A PRICE, which is a different answer from the branch
		// above and needs a different word. Requests without a single priceable one is the
		// shape PriceableRequests' own doc describes: Model Context Protocol (MCP) tool calls,
		// health checks, any proxied traffic that is not inference. Under the endpoint and agent
		// axes that is not a corner case — it is what an MCP-only row looks like every poll.
		//
		// "cost unavailable" WOULD BE A LIE HERE. That phrase means the figure exists and this
		// surface cannot produce it; here the figure is nothing, and the row's own counters say
		// so definitively. Sending a reader to check the rate table for traffic that has no rate
		// is the same class of false lead as an empty gap list beside a "1/10 priced" warning.
		//
		// And a blank was the third wrong answer: before this branch the row rendered
		// "some-endpoint  9 req  8.0k tokens" with the cost slot simply missing, which reads as
		// a drawer that failed to fill a cell rather than as an answer.
		money = plainFigure("not priceable")
	}
	if r.counts.Requests > 0 {
		req = stripFigure{
			full:    fmt.Sprintf("%d req", r.counts.Requests),
			compact: fmt.Sprintf("%dr", r.counts.Requests),
		}
	}
	if r.counts.Tokens > 0 {
		tokens = stripFigure{
			full:    humanizeCount(r.counts.Tokens) + " tokens",
			compact: humanizeCount(r.counts.Tokens),
		}
	}
	// THE COMPACT FORM KEEPS THE WORD "saved", which the other figures here drop. It used to be a
	// bare "~$0.1804", distinguishable from this row's cost only by the marker — and this panel no
	// longer marks its money (see renderTierRows for why), so a bare "$0.18" beside a cost of
	// "$1.06" would read as a second spend figure with no way to tell which is which. The word is
	// this figure's identity rather than an explanation of it, so it is not the part that yields;
	// the strip's own saved figure made the same argument before it was replaced.
	if r.counts.AvoidedMicros > 0 {
		saved = plainFigure("saved " + formatUSDTotalMicros(r.counts.AvoidedMicros))
	}

	// THE COLUMNAR SLOTS, IN ORDER, each at its own width. A slot with nothing in it still occupies
	// its column when a later COLUMN is filled — that is what keeps a tokens figure out of the
	// request column on an MCP-only row.
	cols := []stripFigure{
		money.inColumn(drawerMoneyWidth, false),
		req.inColumn(drawerReqWidth, false),
		tokens.inColumn(drawerTokensWidth, false),
		saved.inColumn(drawerSavedWidth, false),
	}
	// TRIMMED AGAINST THE LAST FILLED COLUMN, NOT THE LAST FILLED SLOT, and the note is the reason
	// the distinction matters. Nothing is aligned against the note — it is prose of whatever length
	// the caveats come to — so an empty column held open in FRONT of it buys no alignment and costs
	// its width plus a gap. Measured with the note counted as a slot: a row with caveats and no
	// saving held drawerSavedWidth open for nothing, so the note first fitted at 91 columns where 77
	// would do, and was dropped on every terminal between.
	last := -1
	for i, f := range cols {
		if f.full != "" {
			last = i
		}
	}
	figs = append(figs, cols[:last+1]...)
	// The note goes on the end and takes no column, so it is the first thing fitStripFigures gives
	// up — which is right, since it is the explanation and every figure before it is the fact.
	if note.full != "" {
		figs = append(figs, note)
	}
	return figs
}

// gapOf is a row's unpriced count: priceable minus priced, floored at zero.
//
// Floored because the two counters can invert on a row — a response carrying a gateway's
// own cost header but no parsed token counts is priced without being priceable by the
// model-and-tokens test — and a NEGATIVE gap fails every consumer's `> 0` check, so the
// caveat disappears exactly where there is something to caveat. The ledger writer records
// the same inversion and closes it the same way.
func gapOf(c usage.Counts) int64 {
	if gap := c.PriceableRequests - c.PricedRequests; gap > 0 {
		return gap
	}
	return 0
}

// axisHint spells the axis cycle with the current one bracketed, so the line says both what
// `a` will do and where it currently is.
func axisHint(axis usage.Group) string {
	out := ""
	for i, a := range spendDrawerAxes {
		if i > 0 {
			out += " · "
		}
		if a == axis {
			out += "[" + string(a) + "]"
			continue
		}
		out += string(a)
	}
	return out
}

// rankSeriesByCost orders a snapshot's series labels by what they cost, descending.
//
// Mirrors collectSeries' shape — a map of totals, then a sorted slice — and differs only in
// the quantity summed. Ties break on the label so the order is deterministic: two series
// that cost the same must not swap places between polls, which would make the drawer flicker
// under a reader who is trying to compare rows.
//
// MONEY THROUGH usage.CostSum, not a raw `+=`. The figures these rows display come from
// Counts.Add, which saturates; ranking them with a raw accumulator let the two disagree, and
// disagree in the one direction that loses a row. A series summing past MaxInt64 across
// buckets wraps NEGATIVE, ranks below a ten-micro series, and foldTailSeries then folds the
// window's single most expensive model into (other) — its figure still correct wherever it
// lands, its row simply gone. That needs $9.2T in one label to reach, so this is not a bug
// anybody will hit; it is the drawer being the one money surface in the package that added
// its own way, and the fix is the idiom already here.
func rankSeriesByCost(buckets []usage.Bucket) []seriesKey {
	totals := map[string]*usage.CostSum{}
	for _, b := range buckets {
		for label, c := range b.Series {
			if totals[label] == nil {
				totals[label] = &usage.CostSum{}
			}
			totals[label].Add(c.CostMicros)
		}
	}
	out := make([]seriesKey, 0, len(totals))
	for label, sum := range totals {
		out = append(out, seriesKey{label: label, total: sum.Micros})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].total != out[j].total {
			return out[i].total > out[j].total
		}
		return out[i].label < out[j].label
	})
	return out
}
