package tui

import (
	"fmt"
	"math"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
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
func savingFor(resp *pipeline.SessionEvent, component string) (costevent.Saving, bool) {
	ev, ok := costevent.Record(resp)
	if !ok {
		return costevent.Saving{}, false
	}
	for _, s := range ev.Avoided {
		if s.Component == component {
			return s, true
		}
	}
	return costevent.Saving{}, false
}

// pruneSavingFor is the tool-prune saving specifically, which is the only one rendered today.
func pruneSavingFor(resp *pipeline.SessionEvent) (costevent.Saving, bool) {
	return savingFor(resp, "tool-prune")
}

// formatCompact renders a token count tersely enough for a table cell: 10577
// becomes "10.6k". Exact below 1000, where the extra digits still fit.
func formatCompact(v float64) string {
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%.1fM", v/1_000_000)
	case v >= 1_000:
		return fmt.Sprintf("%.1fk", v/1_000)
	default:
		return fmt.Sprintf("%.0f", math.Round(v))
	}
}

func formatUSD(v float64) string {
	switch {
	case v >= 1:
		return fmt.Sprintf("%.2f", v)
	case v >= 0.01:
		return fmt.Sprintf("%.3f", v)
	default:
		return fmt.Sprintf("%.4f", v)
	}
}

// formatUSD4 is formatUSD at fixed precision, for the case where two amounts of
// different magnitude share one column and their decimal points must line up.
// Neither returns a "$" — the caller places it, since a saving needs it inside
// the parentheses.
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
