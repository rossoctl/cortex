package tui

import (
	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// costEvent is the litellm-budget-track per-response event.
//
// Unlike tool-prune's event, this one carries a finished dollar figure rather
// than rates, so a consumer has no arithmetic left to do. It is NOT always an
// authoritative figure though: the plugin prefers the cost LiteLLM stamps on the
// response, but a streamed response always reports 0 there, so those are priced
// from the plugin's own per-token rates instead. Source says which happened.
//
// An ALIAS, not a copy: this is authlib's costevent.Event, so the compiler — not
// a decode test — is what keeps abctl and the producer agreeing on the wire. This
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
type costEvent = costevent.Event

// decodeCostEvent pulls the litellm-budget-track event off a response event, if
// present. Absent whenever the plugin is not in the pipeline, or the response
// was not priced (a cache hit / error charges nothing and emits nothing) — so a
// false return is the normal case, not an error.
//
// Kept as a local name because the TUI reads better for it; the logic, the
// lookup key and the non-positive-cost rejection all live in authlib/costevent.
func decodeCostEvent(e *pipeline.SessionEvent) (costEvent, bool) {
	return costevent.Decode(e)
}

// promptCost is what the PROMPT of one request cost, read off the cost record.
//
// Not computed here any more. The record carries a prompt-only figure precisely so a
// request row can show one: the gateway reports a single total for the call and cannot
// answer "what did the prompt cost", while a flat prompt-times-one-rate figure overstates
// a cache-heavy turn — the common case for a long-running agent — by close to an order of
// magnitude, because a cache read bills at ~0.1x input and a write at ~1.25x.
//
// False when the proxy could not model it, which is an older proxy or a model with no rate,
// rather than a $0.00 that would read as a free prompt.
func promptCost(resp *pipeline.SessionEvent) (usd float64, ok bool) {
	ev, ok := costevent.Record(resp)
	if !ok || ev.PromptUSD <= 0 {
		return 0, false
	}
	return ev.PromptUSD, true
}

// promptTokens is the request's own billed token count: what the provider
// counted for everything we sent. It lives on the response because the provider
// is the only party that tokenizes, but it is a request-side quantity — which is
// what lets a request row show a total at all.
//
// The PromptTokens fallback cannot currently fire, and is kept only to mirror
// savedTokensAndCost: parsercommon.TokenUsage.Fill is the sole production writer
// of these fields and sets PromptTokens to Input+CacheRead+CacheWrite — the same
// sum computed here — so when the split is zero the aggregate is zero too. It
// costs nothing and would start earning its keep if a parser ever published the
// aggregate directly.
func promptTokens(resp *pipeline.InferenceExtension) int {
	if resp == nil {
		return 0
	}
	if n := resp.InputTokens + resp.CacheReadTokens + resp.CacheWriteTokens; n > 0 {
		return n
	}
	return resp.PromptTokens
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

// formatTokensWithSaving renders "681,300(−9.9k)" — the total this row is
// responsible for, with what was kept off it in parentheses. A bare total when
// there was no saving.
//
// The total is exact-with-commas and an ESTIMATED saving compact: the total is a measured
// count worth reading precisely, an estimate a figure where trailing digits would be false
// precision. That is the estimate marker — precision itself — and it is why a counted saving
// renders exact instead.
//
// No third glyph for estimated. "~" already means projected, and every saving published today
// is estimated (costing derives them from a byte ratio), so a marker on 100% of rows would
// distinguish nothing while adding noise to every one. The moment a component reports a
// saving counted by a tokenizer, it renders differently here without a legend to learn.
//
// The record itself carries the flag verbatim, so an operator pressing enter sees
// "estimated": true regardless of how the cell reads.
func formatTokensWithSaving(total int, saved float64, projected, estimated bool) string {
	if total <= 0 {
		return ""
	}
	cell := formatCount(total)
	if saved <= 0 {
		return cell
	}
	figure := formatCompact(saved)
	if !estimated {
		figure = formatCount(int(saved))
	}
	return cell + "(" + savingSign(projected) + figure + ")"
}

// formatUSDWithSaving is formatTokensWithSaving for money: "$0.2546(−$0.0037)".
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
