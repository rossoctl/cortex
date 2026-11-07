package tui

import (
	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

// costEvent is the litellm-budget-track per-response event.
//
// Unlike tool-prune's event, this one carries a finished dollar figure rather
// than rates, so a consumer has no arithmetic left to do. It is NOT always an
// authoritative figure though: the plugin prefers the cost LiteLLM stamps on the
// response, but a streamed response always reports 0 there, so those are priced
// from the plugin's own per-token rates instead. Source says which happened.
//
// An ALIAS, not a copy: this is core's event.Event, so the compiler — not
// a decode test — is what keeps agentop and the producer agreeing on the wire. This
// file used to redeclare the struct and its decoder, which meant a field rename
// in the plugin silently blanked a column here until a test happened to catch it.
//
// Source is "gateway-header" (the gateway's own post-discount figure) or
// "usage-fallback" (priced from token counters by the plugin, used for streamed
// responses whose header reports 0 — for a streaming agent this is the common
// case, not the exception). It is decoded but deliberately not rendered: the COST
// column shows modelled and gateway-stamped figures identically, matching the
// request-side cost, which is modelled too. Marking one and not the other would be
// the inconsistency. DailyTotalUSD / DailyMaxUSD are likewise carried but not yet
// rendered.
type costEvent = event.Event

// decodeCostEvent pulls the litellm-budget-track event off a response event, if
// present. Absent whenever the plugin is not in the pipeline, or the response
// was not priced (a cache hit / error charges nothing and emits nothing) — so a
// false return is the normal case, not an error.
//
// Kept as a local name because the TUI reads better for it; the logic, the
// lookup key and the non-positive-cost rejection all live in core/cost/event.
func decodeCostEvent(e *pipeline.SessionEvent) (costEvent, bool) {
	return event.Decode(e)
}

// recordUnit is the unit resp's cost record is in, "" for dollars or no record. See
// event.Event.Currency: the record's figures are named in USD for history's sake only.
func recordUnit(resp *pipeline.SessionEvent) string {
	ev, ok := event.Record(resp)
	if !ok {
		return ""
	}
	return ev.Currency
}

// exchangeCost is what one exchange cost in total: the record's cost_usd (#1681). It is the
// figure an authoritative gateway sets, and on real traffic prompt_usd + output_usd equal it
// to within 1% in 5,704 of 5,707 records.
func exchangeCost(resp *pipeline.SessionEvent) (usd float64, ok bool) {
	ev, ok := event.Record(resp)
	if !ok || ev.CostUSD <= 0 {
		return 0, false
	}
	return ev.CostUSD, true
}

// savingSign distinguishes a realized saving from a projected one. A projected
// saving (on_error: observe, where bytes were measured but not removed) uses "~"
// instead of "−": the money was still spent, and rendering it identically would
// invite an operator to subtract it twice.
func savingSign(projected bool) string {
	if projected {
		return "~"
	}
	return "−"
}

// formatUSDWithSaving renders a cost with what was saved in parentheses: "$0.2546(−$0.0037)".
// A bare total when there was no saving.
//
// Both halves are rendered at a fixed 4 decimal places rather than through
// formatUSD's variable precision. Stacked in one column, "$0.255" above
// "−$0.0037" misaligns the decimal point and reads as though the two figures
// were measured to different accuracy.
func formatUSDWithSaving(total float64, saved float64, projected bool) string {
	if total <= 0 {
		return ""
	}
	cell := formatUSDCell(total)
	if saved <= 0 {
		return cell
	}
	return cell + "(" + savingSign(projected) + formatUSDCell(saved) + ")"
}
