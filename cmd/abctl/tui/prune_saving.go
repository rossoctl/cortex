package tui

import (
	"fmt"
	"math"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// This file used to turn tool-prune's byte saving into tokens and dollars.
//
// It no longer does any of that arithmetic. The proxy prices the saving where both halves
// are in hand — the byte delta from the request, the tier and the token ratio from the
// response — and publishes the result in the cost record. Doing it here meant abctl held a
// rate table's worth of assumptions, applied them to base-tier rates that were wrong past a
// long-context threshold, and could disagree with the server after a hot reload swapped the
// table between the event and the render.
//
// What is left is reading a figure and formatting it.

// savingFor returns the saving the proxy attributed to a component, off the response event
// that carries the cost record.
//
// The response event, not the request one: the saving is only priceable once the response
// reveals which prompt tier it came out of, so that is where the priced figure lands. A
// request row reaches it through the same pairing it already does to show token totals.
func savingFor(resp *pipeline.SessionEvent, component string) (event.Saving, bool) {
	ev, ok := event.Record(resp)
	if !ok {
		return event.Saving{}, false
	}
	for _, s := range ev.Avoided {
		if s.Component == component {
			return s, true
		}
	}
	return event.Saving{}, false
}

// pruneSavingFor is the tool-prune saving specifically, which is the only one rendered today.
func pruneSavingFor(resp *pipeline.SessionEvent) (event.Saving, bool) {
	return savingFor(resp, "tool-prune")
}

// formatCompact renders a token count tersely enough for a table cell: 10577
// becomes "10.6k". Exact below 1000, where the extra digits still fit.
func formatCompact(v float64) string {
	// Thresholds sit where the ROUNDING carries, not at the round number. %.1f turns
	// 999,999,999 into "1000.0M" — seven runes of four-digit millions, one wider than
	// any value on either side of it, and the same carry happens at every boundary
	// ("1000.0k"). Promoting at 999.95 of a tier keeps the widest output six runes
	// ("999.9M"), which is what lets the column's fit guarantee be stated at all.
	switch {
	case v >= 999_950_000:
		// A billion-token session is not hypothetical: the picker showed 547.9M on a
		// day-old one, and the figure is a sum over turns that keeps climbing. Without
		// this tier it reads "10000.0M" — eight runes of five-digit millions, which is
		// both the shape this function exists to avoid and the one that overflows the
		// TOKENS column once fitTableColumns squeezes it on a narrow terminal.
		return fmt.Sprintf("%.1fB", v/1_000_000_000)
	case v >= 999_950:
		return fmt.Sprintf("%.1fM", v/1_000_000)
	case v >= 999.95:
		return fmt.Sprintf("%.1fk", v/1_000)
	default:
		return fmt.Sprintf("%.0f", math.Round(v))
	}
}

// formatUSD4 renders a USD amount at fixed precision, for the case where two
// amounts of different magnitude share one column and their decimal points must
// line up. It returns no "$" — the caller places it, since a saving needs it
// inside the parentheses.
func formatUSD4(v float64) string { return fmt.Sprintf("%.4f", v) }

// usdFloor is the smallest amount four decimal places can state. Anything
// positive below half of it rounds to "0.0000".
const usdFloor = 0.0001

// formatUSDCell renders a dollar amount for a table cell, with the "$" attached
// and a floor below which it says so rather than rounding to zero.
//
// The floor exists because %.4f renders anything under $0.00005 as "$0.0000",
// which reads as "this was free" — the exact reading decodeCostEvent (declining a
// cost of 0) and promptCost (declining an unpriced model rather than showing
// $0.00) both go out of their way to avoid. Reintroducing it at the formatting
// layer would undo both. Reachable on a small cache-read-only request: 100
// cache-read tokens at a typical rate is $0.000038.
func formatUSDCell(v float64) string {
	if v > 0 && v < usdFloor/2 {
		return "<$" + formatUSD4(usdFloor)
	}
	return "$" + formatUSD4(v)
}

// WHICH PRECISION A MONEY FIGURE GETS, stated here once because it was decided twice.
//
// Two decimals — formatUSDTotal — WHEREVER A READER SCANS AND COMPARES: the day, the rolling
// window, the endpoint, the sessions table's COST and SAVED, the Usage pane's COST, the drawer's
// tiers and models. Four — formatUSDCell — for ONE REQUEST, and only there: the events table's
// COST and the cost-event cells behind it.
//
// THE CARVE-OUT IS MAGNITUDE, NOT AGGREGATION, and it is narrow on purpose. A single cache-read
// request is $0.000038; cents renders it "<$0.01" and renders the whole column identically, so
// four decimals is the only precision at which one request says anything at all. Everywhere else
// the figures are cent-scale or larger, and there the extra two digits are noise on every row of
// a surface whose job is comparing rows to each other.
//
// AN EARLIER VERSION OF THIS RULE DREW THE LINE AT "span versus one thing", which put the sessions
// table and the drawer on the four-decimal side. That was wrong about what those surfaces are for:
// a per-session figure is attributable to one session, but the COLUMN exists to be read down, and
// "$1.8140" against "$2.8984" is two digits of precision nobody is comparing. The cost is real and
// was accepted knowingly — two sessions differing below a cent now read alike.
//
// It was decided twice because #1042 rounded the Usage pane to cents with its own inline
// arithmetic while every other surface kept four decimals, so the same money read "$1.01" in one
// panel and "$1.0060" in the panel above it, and a reader comparing them could not tell rounding
// from disagreement. The arithmetic now lives in one place and the boundary is a sentence rather
// than a per-surface habit.
//
// NEITHER FORM EVER RENDERS A POSITIVE FIGURE AS ZERO. Cents falls back to "<$0.01" and four
// decimals to "<$0.0001", which is the rule decodeCostEvent and promptCost already go out of
// their way to keep: "free" is a claim about the traffic and must not be a rounding artefact.
//
// A caller with micros in hand should use formatUSDTotalMicros directly. Going through float64
// is safe — see MicrosFromUSD — but pointless when the integer is already there.
func formatUSDTotal(usd float64) string {
	micros, ok := pricing.MicrosFromUSD(usd)
	if !ok {
		// Negative, NaN, or past MaxCostMicros. Not this function's call to make: every
		// surface that shows a total already has its own word for an impossible figure
		// ("unavailable", a clamp marker), and inventing a third here would hide theirs.
		// Four decimals is the honest fallback — it shows whatever the figure actually is.
		return formatUSDCell(usd)
	}
	return formatUSDTotalMicros(micros)
}

// formatUSDTotalMicros is formatUSDTotal for a caller that already has integer micros.
//
// INTEGER ARITHMETIC, not %.2f on micros/1e6, and the reason is #1042's: 1_005_000 micros is
// exactly $1.005, the float64 nearest it is 1.00499999…, and %.2f prints $1.00. Rounding
// half-up on the integer gets $1.01. The conversion in formatUSDTotal is safe for the same
// reason in reverse — MicrosFromUSD rounds, so it recovers 1_005_000 from that float.
func formatUSDTotalMicros(micros int64) string {
	// A NEGATIVE FIGURE MUST NOT REACH THE ARITHMETIC BELOW. Go's / and % truncate toward zero,
	// so -150_000 renders "$0.-15" and -12_345_678 renders "$-12.-34"; worse, anything above
	// -5_000 rounds to "$0.00", which claims the traffic was free — the reading the floor below
	// exists to prevent.
	//
	// This guard used to be inseparable from the arithmetic: in renderCostSummary the
	// negativeCost case and the cent branches were arms of one switch, and extracting the
	// arithmetic left the guard behind at the call site. The float64 entry point is safe by
	// accident of MicrosFromUSD rejecting a negative, so the two entry points disagreed.
	//
	// Four decimals, which is what formatUSDTotal answers for the same input, so they agree.
	// Naming it — "unavailable" — stays the caller's job: renderCostSummary has its own word
	// for an impossible figure and a second spelling here would hide it.
	if micros < 0 {
		return formatUSDCell(float64(micros) / 1e6)
	}
	if micros > 0 && micros < 5_000 {
		// Positive but under half a cent. The same floor rule formatUSDCell applies at
		// $0.0001, two decimal places up.
		return "<$0.01"
	}
	cents := micros / 10_000
	if micros%10_000 >= 5_000 {
		cents++
	}
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}
