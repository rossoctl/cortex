package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/core/usage"
)

// stripGap separates whole figures. Three spaces rather than a glyph separator so
// the figures read as independent readings rather than as one expression — the
// same spacing the footer's status line uses between its own readings.
const stripGap = "   "

// inexactMarker precedes a dollar figure that is NOT EXACT: at least one of the priced
// requests behind it carries a figure the aggregator could not settle exactly
// (usage.Counts.IncompleteRequests) — a stream that died before its output count, so the
// amount is a LOWER BOUND, or a gateway that reported only a total. It reads
// "approximately", and the real number is at least this much.
//
// ONE SPELLING, EVERYWHERE. This is the marker the band puts on its TODAY and LAST figures, the
// drawer puts on a per-model COST, and the strip puts on its own two — all four through
// markMoney, all four when incomplete > 0. A branch that carries a commit titled "Stop
// publishing a truncated stream's floor as an exact total" had three money surfaces republishing
// that floor with no annotation at all, and three different annotations would have been barely
// better: a marker a reader has to learn twice is a marker they learn once and misread
// thereafter.
//
// ON A VALUE ONLY WHEN IT IS CONDITIONAL, which is the rule that decides where it goes. Every
// case above is earned per reading — incomplete > 0 — so it rides the figure. An UNCONDITIONAL
// caveat is a property of the whole column and goes on the heading instead: the sessions table's
// SAVED heading reads "SAVED~" and the band's label reads "SAVED~", because a saving is always an
// estimate and a glyph on every row states one fact once per session.
//
// WHERE IT IS GONE, precisely, because the difference is easy to overstate:
//
//   - The drawer's TIER rows, which format through formatUSDTotalMicros directly. Their caveat was
//     unconditional and the panel has no money heading to move it onto, so it was a glyph on every
//     row or nothing — renderTierRows records the choice.
//   - The drawer's SAVED figure, for the same reason.
//   - The sessions table's COST and SAVED VALUES. Not because the caveat moved in both cases —
//     SAVED's did, to the heading — but because sessionMoneyCell takes no incomplete count at all.
//     Its only value marker is partialMarker for a clamped total.
//
// The drawer's per-model COST is NOT in that list and keeps its conditional marker, which is what
// the rule says should happen. Said explicitly because an earlier version of this comment claimed
// the drawer's "tier and model rows" both dropped it, and the model half was wrong.
//
// The Usage pane never carried it — a claim that sat here stale for some time; renderCostSummary
// emits no marker. The events table's "~" is a different marker with a different meaning
// (savingSign, "projected"), deliberately not this one.
//
// One display column, so it survives every width the strip's fitter can produce and
// every width fitTableColumns can leave the COST column at. The burn rate has always
// worn the same "~" for the same reason — a derived rate is not an exact figure either.
//
// headerTitle strips it, so a heading that carries it is still addressable by its bare name.
const inexactMarker = "~"

// partialMarker follows a dollar figure that covers only PART of the traffic it
// appears to be about: something in the window was priceable and carries no figure, so
// the real total is LARGER than the number shown.
//
// One display column, and it rides on the figure itself rather than in the words beside
// it. That is the whole point: fitStripFigures may shorten a caveat to nothing and may
// drop a whole figure, but while a figure is on screen its marker is on screen with it,
// so a partial total can never be published as a complete one. It reads as "and more",
// which is what a coverage gap means for a total.
const partialMarker = "+"

// damagedMarker precedes a dollar figure that is SHORT of real spend by an amount nothing in
// the response can state. It has TWO CAUSES and one meaning:
//
//   - THE LEDGER READ WAS INCOMPLETE: rows the answer needed could not be read at all — a day
//     file that lost lines, or one whose scan was abandoned part-way. See
//     usage.Snapshot.Degraded and snapshotDamaged.
//   - THE AGGREGATE ARITHMETIC WAS CLAMPED: an addition into the totals hit the int64 ceiling
//     and was capped rather than allowed to wrap, so every figure in that window is a floor.
//     See usage.Counts.Saturated, whose own doc says the disclosure "travels on the same
//     response as the number it qualifies, which is where an operator reading that number will
//     see it" — this is that place.
//
// ONE GLYPH FOR BOTH, because the claim a reader acts on is identical: the number on screen is
// less than the money that was spent, and by how much is unstatable. The words beside it name
// which cause, exactly as inexactMarker carries one glyph over the two directions
// usage.Snapshot.IncompleteBy distinguishes. A second glyph would make the strip's vocabulary
// four marks deep to draw a distinction that changes nothing about how the figure must be
// read. figureIsShort is the one spelling of the test.
//
// A THIRD GLYPH, and deliberately not one of the other two. Degraded's own doc is explicit
// that this is a different claim from usage.Counts.IncompleteRequests and that the two must
// not be merged or shown under one marker: inexactMarker says a figure the total CARRIES is
// a floor, this says spend is MISSING FROM THE SUM. partialMarker is closer — the real
// total is larger either way — but a coverage gap is COUNTED and nameable ("412 requests on
// api.openai.com gpt-5"), while this shortfall is unstatable, and collapsing them would
// tell an operator to add a pricing entry for a corrupt file.
//
// Prefixed OUTSIDE inexactMarker, so the leftmost cell carries the most serious claim and
// a figure that is both reads "!~$4.1700+" — three one-column claims, each with its own
// meaning, rather than one marker doing two jobs badly.
//
// One display column, so it survives every width fitStripFigures can produce. It rides on
// the FIGURE rather than in the words beside it, for the reason partialMarker records: the
// fitter may shorten a caveat to nothing, but while a figure is on screen its markers are
// too, so a total that is short can never be published as a complete one. The cost is that
// each marker widens the figure by a column, which moves the width at which a LATER reading
// is dropped — accepted, and the same trade the other two already make, because the marker
// is the fact and the words are the explanation.
const damagedMarker = "!"

// figureIsShort reports that a figure wears damagedMarker: it is short of real spend by an
// amount nothing in the response can state, from either of the two causes that produce that
// claim. See damagedMarker.
//
// A function rather than the disjunction written at each site, for the reason negativeCost is
// one: the same test is made by the strip's today figure, the Cost pane's TOTAL, the pane's
// caveat block and `abctl cost`, and a predicate written four times is a predicate that drifts.
// usage.Counts.Saturated arrived after usage.Snapshot.Degraded and only one surface picked it
// up; the next disclosure of this class should have one place to be added.
func figureIsShort(degraded *usage.Degraded, saturated bool) bool {
	return snapshotDamaged(degraded) || saturated
}

// stripFigure is one reading in the strip, in the two forms it can take.
//
// full spells the caveat out; compact keeps the figure and its marker and drops only
// the words. Both forms of a qualified figure carry the marker — see partialMarker —
// so degradation costs an explanation and never the fact.
//
// Two forms rather than a general shortener because the strip has exactly one thing to
// give up under width pressure, and the fitter's job is to choose between whole
// readings, not to edit them.
type stripFigure struct {
	full    string
	compact string
	// col is the display width this figure occupies in a COLUMN, so figures stacked on successive
	// rows line up. Zero means "as wide as the text", which is every caller that renders a single
	// row on its own — the drawer's hint line, the fixed readings.
	//
	// A MINIMUM, NOT A LIMIT: a figure wider than its column overflows rather than being cut,
	// because the alternative is a truncated money figure and every money surface here refuses
	// that. A shifted row is visible and recoverable; a wrong number is not. The one figure that
	// exceeds its column in practice is the prose fallback "cost unavailable", on a row whose
	// money slot has no figure to align against anyway.
	col int
	// leftAlign pads on the RIGHT, which is what a label wants. Figures pad on the left, because
	// a column of numbers is read down its last digit — the rule the tables and renderTierRows
	// already follow.
	leftAlign bool
}

// plainFigure is a reading with nothing to qualify: both forms are the same string, and it takes
// no column — see stripFigure.col.
func plainFigure(s string) stripFigure { return stripFigure{full: s, compact: s} }

// inColumn fixes f to a display width: left-aligned for a label, right-aligned for a figure.
//
// IT MAKES THE COMPACT FORM INERT FOR THIS FIGURE, and that is intended rather than overlooked.
// Both forms pad to the same column, so whenever the full form already fits its column the compact
// one saves nothing and fitStripFigures' compact pass cannot help: degradation for a columnar
// figure is DROP-ONLY. Measured on a drawer row — six distinct renderings across widths 1 to 200,
// none of them containing "568r" or a bare "137M".
//
// PADDING THE COMPACT FORM IS LOAD-BEARING, which is why the answer is not "skip the pad in compact
// mode". The fitter runs per ROW against one shared width, and rows do not all reach the same
// decision: a money slot holding "cost unavailable" is 16 columns in a 9-column column while "$9.43"
// fits, so at some widths one row degrades and its neighbour does not. If compact forms rendered
// unpadded, those two rows would then disagree about their column stride — silently misaligning the
// table, which is the defect the columns exist to fix. A narrower compact column per figure would
// preserve both, at the cost of a second width on every slot; it is not worth it for the band of
// terminal widths it would serve, and the trade is recorded here rather than left to be rediscovered.
func (f stripFigure) inColumn(width int, leftAlign bool) stripFigure {
	f.col, f.leftAlign = width, leftAlign
	return f
}

// pad renders one form of f at its column width.
//
// padLeft and padRight measure in DISPLAY COLUMNS, never Fprintf's rune count: these figures carry
// an em dash and marker glyphs today and a styled string the moment anyone adds one, and a column
// measured in one vocabulary and filled in another is wrong by the difference. footer.go records
// what that cost the last time.
func (f stripFigure) pad(s string) string {
	if f.col <= 0 {
		return s
	}
	// AN EMPTY FIGURE STILL OCCUPIES ITS COLUMN, and this case has to be spelled out because
	// padLeft and padRight both return "" unchanged — `if s == "" || w >= width` — so neither will
	// hold a column open. That guard suits the table cells they were written for and is exactly
	// wrong here: an empty MIDDLE slot is the whole reason drawerFigures is positional, and
	// collapsing it moves every column to its right.
	//
	// Measured before this: a row with a saving and no tokens rendered
	// "model-b  $1.00  100 req      saved $0.50" against its neighbour's
	// "model-a  $1.00  100 req  900k tokens  saved $0.50" — the saving eleven columns out of line,
	// which is the defect the columns exist to prevent, surviving inside the fix for it.
	if s == "" {
		return strings.Repeat(" ", f.col)
	}
	if f.leftAlign {
		return padRight(s, f.col)
	}
	return padLeft(s, f.col)
}

// coverageNote is the one spelling of a coverage gap, shared by every branch so the
// wording cannot drift between them.
func coverageNote(unpriced, priceable int64) string {
	return fmt.Sprintf("%d of %d unpriced", unpriced, priceable)
}

// damagedNote is the strip's spelling of a damaged ledger read: the shortest form that
// still says WHAT was lost, because "incomplete" on its own gives a reader nothing to act
// on where "1 day file lost" names something to go and look at.
//
// One fact at three verbosities, the same relationship coverageNote has with the Cost
// pane's "covers N of M priceable requests" and cmd_cost.go's line: costDamagedNote spells
// it out where there is room for a sentence, this is what a strip can afford.
//
// It rides in the figure's FULL form only. damagedMarker is what survives into the compact
// form, so width pressure costs the explanation and never the fact.
//
// "lost", not "skipped": the ledger's own verbs describe what IT did, and the reader of a
// spend line cares what the number is missing.
func damagedNote(d *usage.Degraded) string {
	switch {
	case d.SkippedLines > 0 && d.TruncatedDays > 0:
		return fmt.Sprintf("%d lines, %d day file%s lost",
			d.SkippedLines, d.TruncatedDays, plural(int(d.TruncatedDays)))
	case d.SkippedLines > 0:
		return fmt.Sprintf("%d line%s lost", d.SkippedLines, plural(int(d.SkippedLines)))
	case d.TruncatedDays > 0:
		return fmt.Sprintf("%d day file%s lost", d.TruncatedDays, plural(int(d.TruncatedDays)))
	default:
		// A disclosure carrying no counters. Presence is still the claim — see
		// snapshotDamaged — and this is the least it can say without inventing a number.
		return "rows lost"
	}
}

// saturatedNote is the strip's spelling of a clamped aggregate: the shortest form that still
// says which way the figure is wrong.
//
// One fact at three verbosities, the same relationship damagedNote has with
// costSaturatedNote's sentence and cmd_cost.go's line. "clamped" alone would leave a reader
// guessing whether the number shown is too big or too small, and "floors" is the half they
// act on — the real spend is LARGER than the figure beside it.
//
// A constant, because usage.Counts.Saturated is a bool: there is no count of clamped
// additions to interpolate, and its doc explains why there deliberately is not one.
//
// It rides in the figure's FULL form only. damagedMarker is what survives into the compact
// form, exactly as with damagedNote, so width pressure costs the explanation and never the fact.
const saturatedNote = "clamped, figures are floors"

// moneyAmount is the marked figure alone, without a label or a caveat clause.
//
// Extracted from moneyFigure so the KPI band and the strip cannot disagree about which
// markers a reading earns. THE MARKERS ARE THE COMPACT DISCLOSURE: the band has no room for
// the parenthesised prose moneyFigure adds, and neither does the strip at a narrow width —
// fitStripFigures already falls back to this same marked form there. So a surface showing
// markers only is the established degradation rather than a new loss.
//
// Marker order is load-bearing and unchanged: damaged outermost so the leftmost glyph is the
// most serious claim, inexact next to the amount, partial trailing.
//
// THE PRECISION IS THE CALLER'S, and splitting it out is what keeps the rule beside
// formatUSDTotal true. This function used to format as well as mark, so every surface reaching
// it got one precision whether or not the rule said it should.
//
// NOTHING ON THIS FOUR-DECIMAL PATH IS REACHABLE FROM A SCREEN. moneyAmount's single production
// caller was moneyFigure, and the drawer — moneyFigure's only live caller — now goes through
// moneyFigureTotal instead, because its per-model column is scanned and compared. The strip that
// held the other two callers is gone, replaced by renderSpendBand, so moneyAmount and moneyFigure
// are both reachable only from tests — and damagedNote with them, because moneyFigureTotal's one
// live call site (renderTierRows) passes nil for degraded, so the branch that spells out a damaged
// read cannot be reached from production at all.
//
// TESTS COUNT AS USES, which is why `golangci-lint -E unused` says nothing and the suite reports
// coverage over code no screen can render. That is the write-only-surface defect this branch exists
// to remove, so naming it here rather than leaving it to be rediscovered.
//
// NOT DELETED HERE, deliberately: this is the per-item half of main's precision rule — the side that
// keeps four decimals so one cache-read request at $0.000038 says something — and main's rule
// comment and its money_precision_test address it by name. Removing a documented API of main's
// inside a branch about spend spans is the wrong place for that call. What has to move with it:
// moneyFigure, damagedNote, and the tests that reach all three.
func moneyAmount(usd float64, unpriced, priceable, incomplete int64,
	degraded *usage.Degraded, saturated bool) string {
	return markMoney(formatUSDCell(usd), unpriced, priceable, incomplete, degraded, saturated, false)
}

// moneyTotal is moneyAmount for a SPAN TOTAL — the day's spend, the window's — so it reads in
// cents. See the precision rule beside formatUSDTotal.
func moneyTotal(usd float64, unpriced, priceable, incomplete int64,
	degraded *usage.Degraded, saturated bool) string {
	return markMoney(formatUSDTotal(usd), unpriced, priceable, incomplete, degraded, saturated, false)
}

// markMoney puts the disclosure markers on an already-formatted amount.
// alsoPartial is a SECOND reason the figure covers less than the question implied, independent of
// the unpriced gap — and a parameter rather than another counter, because the caller's reasons are
// not this function's business: it composes markers.
//
// Today the only extra reason is a window reaching past the ledger's retention: the total cannot
// include days the configuration does not keep, which is exactly partialMarker's claim ("the real
// total is LARGER than the number shown") arrived at by a different route. Smuggling it in through
// `unpriced` would have said requests went unpriced, which is a different fact.
func markMoney(amount string, unpriced, priceable, incomplete int64,
	degraded *usage.Degraded, saturated, alsoPartial bool) string {
	if incomplete > 0 {
		amount = inexactMarker + amount
	}
	if figureIsShort(degraded, saturated) {
		amount = damagedMarker + amount
	}
	if (unpriced > 0 && priceable > 0) || alsoPartial {
		amount += partialMarker
	}
	return amount
}

// markMoneyTotal is markMoney over a figure that reads in cents — a span total, which is what the
// band's four cells are. See the precision rule beside formatUSDTotal.
//
// It exists so the band can pass alsoPartial without every other caller of moneyTotal growing a
// parameter it has no reason for: the retention shortfall is a fact about a ledger-backed SPAN, so
// only a span total can carry it.
func markMoneyTotal(usd float64, unpriced, priceable, incomplete int64,
	degraded *usage.Degraded, saturated, alsoPartial bool) string {
	return markMoney(formatUSDTotal(usd), unpriced, priceable, incomplete, degraded, saturated,
		alsoPartial)
}

// moneyFigure builds one dollar reading together with the caveats that belong to IT.
//
// label is the figure's own suffix — "today", "/1h" — and it is why this takes one at
// all: the coverage note used to ride at the END of the line, describing the rolling
// window, while the today figure sat at the front as the headline. "SPEND $4.1700
// today  $1.1200 /1h  40 of 40 unpriced" reads as qualifying the day and describes the
// hour. A caveat attached to its own figure cannot be misread that way, and a figure
// with no caveat of its own now says so by carrying no marker.
//
// Exactness and coverage are separate claims about the same number — whether the figure
// is the real one, and how much of the traffic it covers — and both can be true at once.
// They are stated in that order, matching cmd_cost.go, whose comment records why: the
// exactness caveat qualifies the dollar figure itself, where coverage qualifies how much
// of the traffic the figure is about.
//
// degraded is a THIRD claim and it goes first, because it is the only one of the three that
// says the SUM is incomplete rather than qualifying a figure the sum contains. nil means
// the read was clean and nothing is rendered for it — see snapshotDamaged, which is where
// the pointer semantics are argued. Only a ledger-backed figure can carry one, so the
// window reading passes nil.
//
// saturated is a FOURTH claim that SHARES the third one's glyph, because it makes the same
// demand of a reader: this figure is short of real spend by an amount nothing can state. It is
// also the only claim on this line that BOTH readings can carry — usage.Counts.Saturated lives
// on Counts, so the ring's window totals and the ledger's day totals can each clamp, where a
// damaged read is ledger-only. See figureIsShort and damagedMarker.
// THE PRECISION IS THE CALLER'S here too, split the same way moneyAmount and moneyTotal are:
// moneyFigure formats four decimals and moneyFigureTotal formats cents, over one shared body.
// The two live side by side because they sit on opposite sides of the precision rule — the
// drawer's per-model rows read in cents through moneyFigureTotal, and the four-decimal half is
// kept for the per-request side of main's rule rather than for a live caller of its own (see
// moneyAmount's doc).
func moneyFigure(usd float64, label string, unpriced, priceable, incomplete int64,
	degraded *usage.Degraded, saturated bool) stripFigure {
	return moneyFigureFrom(moneyAmount(usd, unpriced, priceable, incomplete, degraded, saturated),
		label, unpriced, priceable, incomplete, degraded, saturated)
}

// moneyFigureTotal is moneyFigure for a figure that reads in CENTS — see the precision rule
// beside formatUSDTotal.
func moneyFigureTotal(usd float64, label string, unpriced, priceable, incomplete int64,
	degraded *usage.Degraded, saturated bool) stripFigure {
	return moneyFigureFrom(moneyTotal(usd, unpriced, priceable, incomplete, degraded, saturated),
		label, unpriced, priceable, incomplete, degraded, saturated)
}

// moneyFigureFrom builds the reading and its caveat list around an already-formatted, already-
// marked amount. The counters are passed again because the CAVEAT LIST needs them in prose even
// though markMoney has already spent them on glyphs.
func moneyFigureFrom(amount, label string, unpriced, priceable, incomplete int64,
	degraded *usage.Degraded, saturated bool) stripFigure {
	// AN EMPTY LABEL ADDS NO SEPARATOR. Unconditional concatenation left a trailing space on
	// every unlabelled figure, which the joiner then compounded into a four-space gap — visible
	// in the drawer, whose rows have always passed "" here ("claude-opus-5   $35.5797    234
	// req"), and one column of a width budget this file measures to the cell. Every remaining
	// caller passes "" — the drawer's rows carry their span on the group heading, and the band
	// labels its cells itself — so the guard is the only thing standing between those callers and
	// a stray column.
	//
	// THE PARTIAL MARKER IS NOT APPLIED HERE. It was, on this change's own base; moneyAmount
	// owns all three markers now, so re-applying it rendered the figure twice-marked ("$1.12++").
	// `partial` above survives because the CAVEAT LIST below still needs it.
	reading := amount
	if label != "" {
		reading += " " + label
	}
	fig := plainFigure(reading)
	if note := moneyCaveatNote(unpriced, priceable, incomplete, degraded, saturated); note != "" {
		fig.full = fig.compact + " " + note
	}
	return fig
}

// moneyCaveatNote is the parenthesised caveat list for a figure, or "" when there is nothing to
// qualify.
//
// EXTRACTED so the drawer can put the words somewhere other than inside the figure. Its series
// rows are a TABLE, and a parenthesised sentence of variable length sitting in the money column
// knocked every column to its right out of line on whichever row happened to be inexact; the
// drawer appends this as the row's LAST figure instead, where width pressure drops it first. One
// spelling either way, which is the point of extracting it rather than composing it twice.
//
// The marker is NOT here and must not be: moneyAmount owns all three glyphs, so a caller that
// wants the fact without the explanation already has it in stripFigure.compact.
func moneyCaveatNote(unpriced, priceable, incomplete int64,
	degraded *usage.Degraded, saturated bool) string {
	var caveats []string
	// The clamp leads even the damaged read: it is short in every column of the aggregate, not
	// only in the dollars, and it is the reason a figure this line renders can be absurd rather
	// than merely low. Both can be true of one reading, and each keeps its own words — one sends
	// an operator to a day file, the other to whatever produced 9.2e18 micros of traffic.
	if saturated {
		caveats = append(caveats, saturatedNote)
	}
	if snapshotDamaged(degraded) {
		caveats = append(caveats, damagedNote(degraded))
	}
	if incomplete > 0 {
		// No denominator: spendSummary carries the day's and the window's priced counts
		// nowhere, and the count alone is what a strip has room for. The Cost pane states
		// the full "N of M priced figures are lower bounds" for a reader who wants it.
		caveats = append(caveats, fmt.Sprintf("%d inexact", incomplete))
	}
	// A gap is only readable with a denominator, and a denominator of zero is not a gap at all —
	// it is a window with nothing to price, which the caller handles. moneyAmount owns the MARKER;
	// this is the prose.
	if unpriced > 0 && priceable > 0 {
		caveats = append(caveats, coverageNote(unpriced, priceable))
	}
	if len(caveats) == 0 {
		return ""
	}
	return "(" + strings.Join(caveats, ", ") + ")"
}

// formatSpendAge renders an age the way a strip has room for: "3m", not "3m12.4s".
//
// Rounded DOWN to the coarsest unit that still says something, because the number's job
// is to distinguish "a poll or two behind" from "this chain stopped answering", and no
// reader needs the seconds of a five-minute-old figure to tell those apart.
func formatSpendAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

// fitStripFigures joins as many LEADING figures as fit, dropping whole ones from
// the right. Returns "" when even the first figure cannot fit without clipping,
// because a clipped figure is a wrong figure.
//
// Two degradation steps per prefix length, in this order: every figure spelled out,
// then every figure compact. So the line gives up its EXPLANATIONS before it gives up a
// READING, and it gives up a reading before it clips anything. The compact form still
// carries each figure's marker, so no step of this can turn a partial total into one
// that looks complete — the worst outcome available on this line, and the one the old
// tail-mounted coverage note produced whenever it was the thing that got dropped.
//
// Width arithmetic is lipgloss.Width throughout, never len() and never a rune
// count. footer.go:88-92 records the bug that costs: a budget computed in display
// columns and then sliced by rune index overflowed on any wide character,
// rendering 55 columns for a 40-column budget. The package's trunc and truncStr
// helpers are both display-cell-unaware for the same reason, so neither is usable
// here.
//
// Linear rather than a binary search over n: the list is at most five figures
// long, and the loop is the specification — "the widest prefix that fits".
func fitStripFigures(label string, figures []stripFigure, width int) string {
	if len(figures) == 0 {
		return ""
	}
	join := func(n int, compact bool) string {
		parts := make([]string, 0, n)
		for _, f := range figures[:n] {
			s := f.full
			if compact {
				s = f.compact
			}
			parts = append(parts, f.pad(s))
		}
		// EVERY FIGURE PADDED, THEN THE LINE RIGHT-TRIMMED, which is one rule where skipping the
		// last figure was two and got one of them wrong. A right-aligned figure pads on the LEFT,
		// so its padding is what puts it in its column and dropping it on the last figure
		// misaligns the column it was meant to join — measured: "137M tokens" and "45M tokens" in
		// an 11-column slot ended one column apart. Only a LEFT-aligned last figure has trailing
		// padding, and that is invisible, so trimming it here is free and keeps it off the width
		// budget rather than costing a figure that would have fitted.
		return strings.TrimRight(label+"  "+strings.Join(parts, stripGap), " ")
	}
	for n := len(figures); n >= 1; n-- {
		for _, compact := range []bool{false, true} {
			if candidate := join(n, compact); lipgloss.Width(candidate) <= width {
				return candidate
			}
		}
	}
	// The label does not fit alongside even one figure. Drop the LABEL before
	// dropping the number: the figure is the information, the label is decoration,
	// and a dollar amount on its own is still unambiguous in the chrome.
	for _, only := range []string{figures[0].full, figures[0].compact} {
		if lipgloss.Width(only) <= width {
			return only
		}
	}
	return ""
}

// spendStripMinHeight is the terminal height at which the strip earns its row.
//
// Below it the row is worth more to the table than to the chrome, so the strip
// yields it entirely rather than shrink the body further. 20 rows is roughly
// where an events table stops being able to show a turn's request and its
// response together, which is the smallest useful unit of that pane.
const spendStripMinHeight = 20

// spendStripVisible reports whether the strip DRAWS a row on the current pane.
//
// False on paneNamespaces and panePods: those run before a connection exists, so
// there is no spend to report, and they return early from paneView with their own
// JoinVertical anyway.
//
// Note the asymmetry with layout(), which reserves the row on HEIGHT alone and
// ignores the pane. That is deliberate and is explained where the reservation is
// made: layout() runs on WindowSizeMsg, so a pane-aware reservation would have to
// be re-run on every pane transition. The consequence is that the two picker
// panes render one row shorter than they strictly need — invisible, next to an
// events table that is wrong by a row and pushes the footer off-screen.
func (m *model) spendStripVisible() bool {
	if !m.spendStripReservesRow() {
		return false
	}
	switch m.pane {
	case paneNamespaces, panePods:
		return false
	}
	return true
}

// spendStripReservesRow reports whether layout() must hold a row back for the
// strip. Height ONLY — deliberately blind to the pane.
//
// layout() is called from exactly one place, the WindowSizeMsg handler; no pane
// transition re-runs it. So a reservation that read m.pane would go stale the
// moment the user moved between a picker and a data pane, and bodyHeight would
// stay wrong until the next terminal resize — which for a pane the strip DOES
// draw on means a body one row too tall and a footer pushed off the bottom.
//
// Being blind to the pane costs the two picker panes one row they could have
// used. That is the accepted trade: a picker one row short is invisible, an
// events table one row long is not.
func (m *model) spendStripReservesRow() bool { return m.height >= spendStripMinHeight }
