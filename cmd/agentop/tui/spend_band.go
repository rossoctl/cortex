package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/cmd/agentop/money"
)

// spendBandLines is the band's height: ONE row, labels inline with their figures.
//
// Constant in every state, for the same reason numTierRows is — layout() reserves it from
// the terminal height and paneView fills it, and a renderer whose height follows its data
// floats the footer or overflows the terminal.
//
// IT WAS TWO, stacking labels over values so their figures could share a column stride. The
// row is worth more to the sessions table than the stride was to a reader: keys.go does
// `bodyH -= spendBandLines`, so this hands a row back to the body of every pane the band draws
// on, drawer open or closed. What is given up and why it is affordable is recorded on
// TestRenderSpendBand_EveryLabelSitsBesideItsOwnFigure, which replaced the stride assertion.
const spendBandLines = 1

// bandSeparator divides cells. A middle dot with a space either side, which is how this package
// already separates peer readings on one line — the title bar's "agentop · <url> · [Sessions]" and
// the drawer's "model · endpoint · agent" hint. Two spaces was enough when the cells were
// columns; on one line a label follows a figure directly, and "…$18.80  THIS MONTH…" runs the two
// readings together where "…$18.80 · THIS MONTH…" does not.
const bandSeparator = " · "

// bandSeparatorWidth is bandSeparator in display columns, named so the width arithmetic below
// cannot drift from the string above it.
const bandSeparatorWidth = 3

// bandCell is one labelled figure. Label and value travel together because a reader pairs them
// by adjacency now that there is no column to pair them by position.
type bandCell struct{ label, value string }

// render is the cell as it appears: label, one space, figure.
//
// PLAIN, ALWAYS. Width arithmetic runs over this, and an escape sequence is not a display column —
// the rule app.go's composition site states for the band and footer.go records the cost of
// breaking. renderMuting below is the styled form, and it is width-identical by construction.
func (c bandCell) render() string { return c.label + " " + c.value }

// renderMuting is render with the LABEL dimmed and the figure left alone.
//
// THE HIERARCHY THE TWO-LINE BAND GOT FOR FREE. app.go used to mute the whole label ROW and leave
// the whole value row bright — `styleMuted.Render(band[0]), band[1]` — which folding to one line
// would have destroyed silently, muting labels and figures together into one uniform grey. The
// contrast is the thing that makes four readings scannable, so it moves down here where a cell
// knows which half is which.
//
// mute is passed in rather than reaching for styleMuted directly so this file stays free of the
// package's styles and a test can assert the geometry with an identity function.
func (c bandCell) renderMuting(mute func(string) string) string {
	return mute(c.label) + " " + c.value
}

// width is what that pair occupies.
//
// lipgloss.Width, never len() and never a rune count — the rule fitStripFigures states for
// this package and the one footer.go records the cost of breaking. It is not merely style
// here: the values carry markers and an em dash, and bandWidth's arithmetic feeds the drop
// enumeration, so a cell measured in runes would drop cells at the wrong widths.
//
// MEASURED OFF render() rather than summed from the halves, so the separating space is counted
// once and in one place. Summing is how a one-column drift gets into a budget.
//
// It does NOT fix an East-Asian ambiguous width, and it is worth saying so rather than leaving
// the next reader to assume it did. U+2014 is ambiguous-width, and a terminal under an EA locale
// gives it two columns while lipgloss v1.1.0 reports 1 either way — measured, including with
// go-runewidth's EastAsianWidth forced on, where runewidth says 2 and lipgloss still says 1. So
// an em-dash band under that locale under-counts no matter which of these two functions is used;
// what changes is that the band now under-counts the same way the table, the strip and the footer
// do, instead of in its own private way.
func (c bandCell) width() int { return lipgloss.Width(c.render()) }

// bandDropOrder is the order cells are given up in as the terminal narrows, FIRST DROPPED
// FIRST. It is deliberately NOT the visual order.
//
// The band reads left to right in ascending span — the hour, the day, the week, the month —
// because that is how a reader zooms out from "right now". Dropping in that direction would
// give up the month first, which is the budget figure and the reason three of these spans
// exist at all. Dropping in reverse would give up the hour, which is the live one.
//
// So the two ends survive and the middle yields: the week goes first, then the hour, leaving
// TODAY and MONTH — "what have I spent today" and "how much of the budget is gone" — as the
// last two readings on a narrow terminal. Every cell still drops WHOLE and nothing is ever
// clipped, which is the rule a truncated money figure breaks.
var bandDropOrder = [numSpendSpans]spendSpan{span7d, spanHour, spanToday, spanMonth}

// renderSpendBand is the always-on spend band: ONE LINE, each label inline with its own figure,
// one cell per budget span, separated by bandSeparator.
//
// FOUR COST CELLS AND NOTHING ELSE, and the exclusion is the design rather than an omission.
// The band used to carry TODAY, LAST 1H, SAVED, CACHE HIT and TOKENS, and three of those five
// had no span on them at all — CACHE HIT and TOKENS were read off the rolling-hour snapshot
// while sitting in a row that opened with TODAY, so an hour's token count read as a day's, with
// nothing on screen to tell a reader otherwise.
//
// IT SUPERSEDES #1074, which fixed the same defect a different way and landed while this was in
// review. That change kept all five cells and suffixed each label with its span — "TOKENS 1H",
// "CACHE HIT 1H" — grouping the row into a day run and a window run. It is a smaller change and
// it does make every cell name its period.
//
// This goes further because the cells were not only mislabelled, they were the wrong cells: an
// operator reads spend against the hour, the day, the week and the month, and the old five could
// reach two of those. Suffixing labels makes a heterogeneous row honest; replacing it with four
// readings of ONE quantity over four periods makes the row comparable, which is what the uniform
// width and right alignment below are for. #1074's divider survives untouched — it is orthogonal
// and closes the top block whatever the band holds.
//
// THE INVARIANT THAT REPLACES THEM: every cell's label names the period its figure covers.
// Cost-only satisfies it trivially, because spendSpanDefs gives each span its label; any cell
// added later has to satisfy it too. The volume readings are not lost — the drawer's tier
// column says the cache story with more detail, the sessions table carries per-session tokens
// and savings, and the Usage pane keeps the full metric set.
//
// MARKERS, NOT PROSE. Each money value comes from moneyAmount, so the three disclosure glyphs
// ride on the figures; moneyFigure's parenthesised caveats do not fit one line beside four
// readings. The marker is the fact and the words are the explanation, and `agentop cost` is the
// surface with room for both.
//
// PAIRED BY ADJACENCY, which is what replaced the uniform-width stride when the band folded to one
// line. The two-line form stacked labels over values and right-aligned both in cells of ONE shared
// width, because four readings of the SAME quantity left-flushed in cells of their own widths put
// "$4.04" and "$703.18" four columns apart and a reader scanning down could not compare them.
//
// A single line has no column to scan, so that property is not weakened but inapplicable — and
// what takes its place is stronger: "TODAY $18.80" is one unit, and no arrangement of neighbours
// can pair a label with another span's money. What is genuinely given up is that two cells of
// unequal width no longer put their decimal points near each other. The row buys that back in the
// sessions table; see spendBandLines and
// TestRenderSpendBand_EveryLabelSitsBesideItsOwnFigure.
//
// THE SELECTION IS SPLIT OUT FROM THE JOIN so the band can be drawn twice from one decision: plain
// for the width arithmetic and the assertions, muted-label for the screen. See renderSpendBand and
// renderSpendBandStyled, which differ in nothing else.
func spendBandCells(s spendSummary, width int) []bandCell {
	// Cells in VISUAL order, every span present. A span with nothing to say still gets a cell:
	// "THIS MONTH —" says "not known here", where a missing cell says nothing at all.
	var cells [numSpendSpans]bandCell
	for span := spendSpan(0); span < numSpendSpans; span++ {
		cells[span] = bandSpanCell(spendSpanDefs[span].label, s.Spans[span])
	}

	// THE BEST FITTING SET, chosen by enumeration rather than by dropping until it fits.
	//
	// Four spans is sixteen subsets, so the whole space is cheap to search — and searching it is
	// what makes the answer both MAXIMAL and MONOTONE IN WIDTH. Dropping greedily is neither.
	// Every cell shares one width, so a single wide cell inflates the budget for all of them, and
	// a loop that stops the moment the set fits keeps whichever wide cell it has not reached yet:
	//
	//	measured, with a five-figure TODAY carrying all three markers ("!~$17265.97+", 12 columns)
	//	  width 25 -> LAST 1H  7 DAYS  MONTH      three cells, 3x7 + 2x2 = 25
	//	  width 26 -> TODAY  MONTH                two, because 2x12 + 2 = 26 also fits
	//
	// Widening the terminal by one column LOST a reading, and it stayed lost through width 39. A
	// put-back pass cannot repair that: it can only un-drop cells, never surrender the wide one
	// that is inflating the shared width, so it never reaches the three-cell set.
	//
	// CHOSEN BY VALUE, NOT BY COUNT, which is the whole point of having a drop order. The weight
	// below gives each surviving cell a bit, most valued highest, so maximising it prefers the set
	// that keeps MONTH over any set that does not — and only then more cells. A weight determines
	// its set uniquely, so nothing else is needed to break a tie.
	//
	// RANKING BY COUNT FIRST INVERTED THE ORDER, and every cell sharing one width is why: three
	// narrow cells are cheaper than two that include a wide one. Measured, with a month just over
	// $999.99 ($4.04 / $18.80 / $216.44 / $1234.56):
	//
	//	width 24 -> TODAY  MONTH
	//	width 25 -> LAST 1H  TODAY  7 DAYS      the month gone, the week back
	//	width 28 -> LAST 1H  TODAY  MONTH       the month returns
	//
	// So at 25 to 27 the band hid the budget figure and showed the week instead — the exact
	// inversion bandDropOrder exists to prevent, and month-to-date is structurally the largest
	// figure here, so it is the likely case rather than the exotic one. Enumeration had made the
	// COUNT monotone in width and left the SET free to change shape.
	var dropped [numSpendSpans]bool
	bestWeight := -1
	for mask := 0; mask < 1<<int(numSpendSpans); mask++ {
		var try [numSpendSpans]bool
		weight := 0
		for i := 0; i < int(numSpendSpans); i++ {
			if mask&(1<<i) != 0 {
				try[bandDropOrder[i]] = true
				continue
			}
			weight |= 1 << i
		}
		if bandWidth(cells, try) > width {
			continue
		}
		if weight > bestWeight {
			bestWeight, dropped = weight, try
		}
	}

	// NO PADDING PASS. The two-line band padded every cell to one shared width so the figures
	// shared a stride; each cell is now exactly as wide as its own contents, which is what
	// bandWidth above charges for and what keeps one wide span off the other three.
	out := make([]bandCell, 0, numSpendSpans)
	for span := spendSpan(0); span < numSpendSpans; span++ {
		if !dropped[span] {
			out = append(out, cells[span])
		}
	}
	return out
}

// renderSpendBand is the band as PLAIN TEXT: one line, no escape sequences.
//
// This is what the width arithmetic and every assertion run over, which is why it is the form the
// renderer publishes. renderSpendBandStyled is the same line dressed for the screen.
//
// Returns a slice rather than a string so it keeps paneView's shape — spendBandLines rows in, the
// same number out — and so growing the band back to two rows would not change the signature again.
// scoped says a single session is on screen; see allSessionsMarker. Both forms take it
// so the plain line stays the exact width of the styled one, which is the property
// TestRenderSpendBand_StylingCostsNoColumns holds and app.go's reservation relies on.
func renderSpendBand(s spendSummary, width int, scoped bool) []string {
	line := joinBandCells(spendBandCells(s, width), nil)
	if scoped {
		line = prefixAllSessions(line, width, nil)
	}
	return []string{line}
}

// allSessionsMarker says the band's figures are the whole proxy's, not the session on
// screen.
//
// WHY A LABEL AND NOT A PER-SESSION FIGURE (#1116). A reader who drills into one
// session and sees "LAST 1H $7.53 · TODAY $31.25" above twelve rows that cost nothing
// reasonably reads those as this session's — the reporter did, on a Bob session whose
// own events carried no cost at all. The honest fix is NOT to scope the band: the cost
// ledger's row key carries no session dimension by design (see fetchSpendSpan), so
// there is no per-session total to show, and the band is deliberately all-sessions
// anyway (see spendState) so it keeps reading while the Usage pane is scoped. Showing
// $0 for the drilled session, which the issue asked for, would be a figure the ledger
// cannot support — and a wrong number wearing a right label is the one outcome
// fetchSpendSpan's comment rules out. So the number stays and the ambiguity goes.
//
// SHOWN ONLY WHEN DRILLED IN, because on the sessions list the band has nothing to be
// confused with: there is no single session on screen for a reader to attribute it to.
const allSessionsMarker = "ALL SESSIONS"

// spendBandNeedsScopeMarker reports whether ONE session is on screen, so the band has
// something a reader could mistake its figures for. See allSessionsMarker for why the
// answer is a label rather than a per-session figure (#1116).
//
// AN ALLOWLIST, NOT AN EXCLUSION LIST, and the difference matters because selectedSess
// outlives the pane that set it: it is cleared only by a connection switch or a clear,
// so "a session is selected" is true on Pipeline, Catalog and Agents too, none of which
// shows that session. Excluding the list panes instead would mark the band on all of
// them. The Usage pane would need excluding either way, for its own reason: it states
// its scope in its own header, and a user who scoped it to one session chose that scope
// rather than arriving at it.
//
// TestSpendBandNeedsScopeMarker_EveryPaneIsClassified enumerates the real pane range, so
// a new pane has to be classified rather than inheriting `false` in silence.
func (m *model) spendBandNeedsScopeMarker() bool {
	switch m.pane {
	case paneEvents, paneDetail:
		return m.selectedSess != ""
	}
	return false
}

// prefixAllSessions puts allSessionsMarker ahead of a rendered band when it fits.
//
// CHARGED AGAINST THE WIDTH BUDGET LAST, AND DROPPED FIRST. spendBandCells chooses the
// best-fitting set of spend cells by enumerating all sixteen subsets over a fixed-size
// array, and that arithmetic is both measured and monotone in width — so the marker is
// added here, after the fit, rather than as a fifth cell inside it. A figure is worth
// more than the label explaining its scope: when the terminal cannot hold both, the
// reading survives and the marker is what goes.
//
// Width-identical in both forms for the reason renderSpendBandStyled documents: the
// mute wraps text without changing its columns.
func prefixAllSessions(line string, width int, mute func(string) string) string {
	if line == "" {
		return line
	}
	marker := allSessionsMarker
	if lipgloss.Width(line)+lipgloss.Width(marker)+bandSeparatorWidth > width {
		return line
	}
	if mute != nil {
		marker = mute(marker)
	}
	return marker + bandSeparator + line
}

// renderSpendBandStyled is renderSpendBand with each label muted and each figure left bright, plus
// whether anything survived the fit.
//
// WIDTH-IDENTICAL TO THE PLAIN FORM, which is the property that lets the fit be computed on one and
// drawn from the other: lipgloss.Width ignores escape sequences, and the mute wraps the label
// without changing its text. TestRenderSpendBand_StylingCostsNoColumns holds the line.
//
// IT RETURNS `drew` RATHER THAN LEAVING THE CALLER TO TEST THE STRING, and that is the whole reason
// for the second return value. paneView needs to know whether a figure reached the screen — the
// drawer must not open under an empty band — and it cannot ask the styled line, because an escape
// sequence is not whitespace and a band with nothing in it is still a non-empty string. Reading it
// off the cell list is exact: bandSpanCell always sets a label and a value, so a surviving cell is a
// visible reading. The alternative, which this replaced, was rendering the band a SECOND time in
// plain form and calling spendSummary() again for it — twice per frame on bubbletea's per-event
// render path, with spendSummary walking all four poll chains each time.
// scoped says a single session is on screen, so the band names its own scope — see
// allSessionsMarker.
func renderSpendBandStyled(s spendSummary, width int, scoped bool) (lines []string, drew bool) {
	cells := spendBandCells(s, width)
	// Wrapped rather than passed directly: lipgloss's Render is variadic, and a variadic mute would
	// let a caller pass several strings and get them joined by a rule this file does not own.
	mute := func(s string) string { return styleMuted.Render(s) }
	line := joinBandCells(cells, mute)
	if scoped {
		line = prefixAllSessions(line, width, mute)
	}
	// `drew` still reports whether a FIGURE reached the screen, which the marker is not:
	// the drawer must not open under a band that is only a label.
	return []string{line}, len(cells) > 0
}

// joinBandCells lays surviving cells out on one line, separated by bandSeparator.
//
// A nil mute means plain. One walk for both forms, so the styled band cannot come out in a different
// order or with a different separator from the one that was measured.
func joinBandCells(cells []bandCell, mute func(string) string) string {
	var line strings.Builder
	for i, c := range cells {
		if i > 0 {
			line.WriteString(bandSeparator)
		}
		if mute == nil {
			line.WriteString(c.render())
			continue
		}
		line.WriteString(c.renderMuting(mute))
	}
	// One line whatever happened, including when nothing survived: an empty band is one blank
	// line, never zero. See spendBandLines.
	return line.String()
}

// bandWidth is what these cells render at on one line, skipping the dropped ones.
//
// THE SUM OF THEIR OWN WIDTHS, not a count times a shared one. That is the arithmetic change the
// fold makes, and it is why a wide cell now costs only itself — see
// TestRenderSpendBand_AWideCellDoesNotWidenTheOthers. It also makes the drop enumeration cheaper
// to reason about: a set's width no longer depends on which member happens to be widest.
//
// Only the separators BETWEEN cells are charged, for the reason the gutter was not: the rendered
// line has none trailing, so charging one would lose a cell to space nobody sees.
func bandWidth(cells [numSpendSpans]bandCell, dropped [numSpendSpans]bool) int {
	total, n := 0, 0
	for span := spendSpan(0); span < numSpendSpans; span++ {
		if dropped[span] {
			continue
		}
		n++
		total += cells[span].width()
	}
	if n == 0 {
		return 0
	}
	return total + (n-1)*bandSeparatorWidth
}

// bandSpanCell is one span's cell: its label, and its figure or an em dash.
//
// THE LABEL CARRIES THE STALENESS, when there is any. A wedged chain holding a good old figure
// is otherwise indistinguishable from a current reading — the failure spendStaleAfter exists to
// name, and one this band could not report at all between the strip's deletion and this change:
// spendSummary computed Age and Stale and no renderer read either.
//
// ON THE LABEL RATHER THAN THE VALUE, and not as a fourth marker glyph. The value's markers all
// qualify the FIGURE — it is a floor, it is inexact, it is short — while an age qualifies the
// ANSWER, and it is a duration rather than a claim. A word beside the period it belongs to says
// that better than a symbol: "TODAY 7m" reads as a day figure polled seven minutes ago, which is
// exactly what it is.
//
// PER SPAN, because the four poll fifteen times apart. One age for the whole band would either
// alarm on a healthy month chain between its own five-minute polls, or stay silent while the
// hour chain wedged.
// The label comes from spendSpanDefs via the caller rather than from the reading, because it
// belongs to the span and not to one poll's answer — a reading built anywhere else would
// otherwise render a nameless column.
func bandSpanCell(label string, r spanReading) bandCell {
	if r.Stale {
		label += " " + formatSpendAge(r.Age)
	}
	if r.Unanswerable || r.Failed || !r.Priced {
		// ONE RENDERING FOR THREE CAUSES, deliberately. "this deployment cannot answer this
		// span", "the poll failed" and "nothing here was priced" differ in WHY and not at all
		// in what a reader may conclude: the figure is not known. A cell seven columns wide has
		// no room to distinguish them, and the only alternative to an em dash is a number that
		// is not one. The drawer and `agentop cost` are the surfaces with room to say which.
		return bandCell{label: label, value: emptyCell}
	}
	return bandCell{
		label: label,
		// A SPAN TOTAL, so it reads in cents — every cell here answers "how much has this span
		// cost", which is the side of main's precision rule that compares rows to each other.
		value: bandValue(r),
	}
}

// bandValue is a span's figure with its caveat markers, in the units the span holds.
//
// DOLLARS ARE markMoneyTotal EXACTLY, which is what the band printed before units existed. One
// foreign unit relabels that same figure; several are printed side by side, dollars first, and
// never added — "$6.20 + 0.03 Bobcoins". A mixed span with no per-unit split to show (an older
// server, or a split that did not add up) says money.Mixed, with no markers: there is no amount
// for them to qualify.
func bandValue(r spanReading) string {
	amount, ok := bandAmount(r)
	if !ok {
		return money.Mixed
	}
	return markMoney(amount, r.Unpriced, r.Priceable, r.Incomplete, r.Degraded, r.Clamped,
		r.DaysOutsideRetention > 0)
}

func bandAmount(r spanReading) (string, bool) {
	if unit, ok := money.WindowUnit(r.Units); ok {
		return money.Relabel(formatUSDTotal(r.USD), unit, 0), true
	}
	if r.ByUnit == nil {
		return "", false
	}
	order := make([]string, 0, len(r.Units))
	for _, u := range r.Units {
		if _, ok := r.ByUnit[u]; !ok {
			return "", false
		}
		if money.IsDefault(u) {
			order = append([]string{u}, order...)
		} else {
			order = append(order, u)
		}
	}
	parts := make([]string, 0, len(order))
	for _, u := range order {
		parts = append(parts, money.Relabel(formatUSDTotalMicros(r.ByUnit[u]), u, 0))
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, " + "), true
}
