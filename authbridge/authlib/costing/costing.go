// Package costing turns one response's facts into one settled cost.
//
// It exists because that decision was made in two places with two shapes — inside
// litellm-budget-track and again inside the usage aggregator — and the two could disagree
// about the same request. Whichever component owns costing now calls Settle, and there is
// one precedence rule: an authoritative figure the gateway reported beats a figure modelled
// from token counts and a rate table.
//
// Separate from the parser that produces the token counts, and separate from the plugins
// that act on the money, because it is neither: a gateway's cost header is
// vendor-specific knowledge that has no place in a provider-shaped body parser, and a
// ledger has no business deciding what a request cost.
package costing

import (
	"math"
	"strconv"
	"strings"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// Response cost headers emitted by LiteLLM.
//
// Exported because they are wire names, not internals: a test needs to synthesize them, and
// a future adapter for a gateway that reports cost differently needs to say which of these
// it is standing in for.
//
// MEASURED 2026-09-11 against ete-litellm (LiteLLM 1.85.5), because an earlier version of
// this comment had the semantics backwards and a reviewer reasonably concluded from it
// that drift detection would false-positive on every Anthropic-format request:
//
//	/v1/chat/completions  both headers present, IDENTICAL values
//	/v1/messages          only "-original", same value the other path reports
//
// For 16 input + 4 output tokens of claude-opus-5 both paths reported
// 0.00013680000000000002, which is 0.76 x vendor list (16x$5 + 4x$25 per Mtok = 0.00018).
// So "-original" is the cost the gateway ACTUALLY CHARGED, not a pre-discount list price,
// and falling back to it compares like with like.
//
// "Original" refers to LiteLLM's OWN discount/margin layer, reported alongside in
// X-Litellm-Response-Cost-{Discount,Margin}-Amount: original is the figure before that
// layer is applied. This gateway runs neither (both report 0.0), so the two agree. A
// gateway that DOES configure them would see them diverge, which is why checkDrift
// checks those headers before comparing — see driftComparable.
//
// The fallback itself is load-bearing: without it, budget tracking silently records $0
// for every Anthropic-format request, which is the shape Claude Code sends.
const (
	ResponseCostHeader         = "X-Litellm-Response-Cost"
	ResponseCostOriginalHeader = "X-Litellm-Response-Cost-Original"

	// LiteLLM's own adjustment layer, non-zero only where an operator configured it.
	DiscountAmountHeader = "X-Litellm-Response-Cost-Discount-Amount"
	MarginAmountHeader   = "X-Litellm-Response-Cost-Margin-Amount"
)

// headerCostState says what the gateway's cost header actually told us. A bool
// could not carry this, and collapsing these cases caused a real defect: every
// state below except headerPositive returned (0, true), so "the gateway declared
// this call free" was indistinguishable from "the header was garbage" and from
// "this is a stream, where LiteLLM always stamps 0 as a placeholder". Publishing a
// settled zero for all of them counted unpriced traffic as priced.
type headerCostState int

const (
	// headerAbsent: no cost header at all. Price from token usage.
	headerAbsent headerCostState = iota
	// headerUnusable: a header was present but could not be believed — unparseable,
	// negative, NaN or Inf. NOT a declaration of anything, so it must not suppress
	// the usage fallback and must never publish a settled zero.
	headerUnusable
	// headerZero: present, parsed, finite and exactly zero. On a NON-streamed
	// response this is the gateway saying the call was free — a cache hit, or an
	// error it declined to charge for. On a streamed response it means nothing:
	// LiteLLM stamps 0 there by design because the total is unknown when headers
	// are sent.
	headerZero
	// headerPositive: a usable figure.
	headerPositive
)

// headerCost returns the cost the gateway reported and what kind of answer it was.
func headerCost(pctx *pipeline.Context) (cost float64, state headerCostState) {
	costStr := pctx.ResponseHeaders.Get(ResponseCostHeader)
	if costStr == "" {
		// Anthropic /v1/messages (and newer LiteLLM) omit the bare header.
		costStr = pctx.ResponseHeaders.Get(ResponseCostOriginalHeader)
	}
	if costStr == "" {
		return 0, headerAbsent
	}
	c, err := strconv.ParseFloat(costStr, 64)
	if err != nil || math.IsNaN(c) || math.IsInf(c, 0) || c < 0 {
		return 0, headerUnusable
	}
	if c == 0 {
		return 0, headerZero
	}
	return c, headerPositive
}

// IsEventStream reports whether the response is a text/event-stream (SSE) — the
// streamed shape where LiteLLM reports cost 0 in the header, so usage-based
// pricing is the intended fallback.
func IsEventStream(pctx *pipeline.Context) bool {
	ct := pctx.ResponseHeaders.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "text/event-stream")
}

// Comparable reports whether the gateway's figure can be compared against a modelled
// one at all.
//
// False when the bare header is absent AND LiteLLM's own discount/margin layer is active:
// the fallback figure is then pre-adjustment while the modelled figure is what the caller
// pays, so any comparison measures the gateway's adjustment rather than the rate table.
func Comparable(pctx *pipeline.Context) bool {
	if pctx.ResponseHeaders.Get(ResponseCostHeader) != "" {
		return true // the effective figure itself; nothing to reconcile
	}
	for _, h := range []string{DiscountAmountHeader, MarginAmountHeader} {
		v := pctx.ResponseHeaders.Get(h)
		if v == "" {
			continue
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil && f != 0 {
			return false
		}
	}
	return true
}

// Settled is what one response cost, and how that was arrived at.
//
// Carries BOTH figures, not just the winner. A consumer that only needs the number reads
// CostUSD; the drift check needs the pair, and keeping them here means it compares what
// was actually decided rather than recomputing either half and hoping the two agree.
type Settled struct {
	// CostUSD is the figure to charge. Meaningful only when Priced.
	CostUSD float64
	// Source is costevent.SourceGatewayHeader or costevent.SourceUsageFallback.
	Source string
	// Provenance is ProvAuthoritative for a gateway figure, else the rate table's level.
	Provenance pricing.Provenance
	// Priced reports that a figure exists — INCLUDING a settled zero, which is the
	// gateway saying the call was free. Not the same as CostUSD > 0.
	Priced bool
	// DeclaredFree marks a gateway-reported, non-streamed, exactly-zero cost: a genuine
	// free call. Distinguished from an unusable header and from a stream's placeholder
	// zero, because publishing a settled zero for those counted unpriced traffic as
	// priced.
	DeclaredFree bool

	// ReportedUSD is the gateway's own figure, when it gave one.
	ReportedUSD float64
	HasReported bool
	// ModelledUSD is what the rate table says the same usage costs, when it can say.
	// Computed even when a reported figure won, because the comparison is the only
	// signal that a rate table has gone stale.
	ModelledUSD float64
	HasModelled bool
	// ModelledProv is the provenance behind ModelledUSD.
	ModelledProv pricing.Provenance

	// PromptUSD is the PROMPT half of the modelled figure — output excluded, tiers
	// weighted. It exists because a request row can show what it cost while the
	// response row shows the total, and the two must not be a sum: a cache read bills
	// at ~0.1x the uncached input rate and a write at ~1.25x, so a flat prompt figure
	// overstates a cache-heavy turn by close to an order of magnitude.
	//
	// Modelled, never authoritative: a gateway reports one total for the call and does
	// not break it down, so this is the table's answer even when the total is the
	// gateway's.
	PromptUSD float64
	HasPrompt bool
}

// Settle prices one response.
//
// The precedence, in one place: a positive header figure wins outright. Otherwise the
// token counts are priced through the rate table — but NOT when the gateway declared the
// call free, because inventing a cost for a call it explicitly charged nothing for is how
// a cache hit came to be billed. A stream's zero is a placeholder, not an answer, so it
// falls back like an absent header.
//
// rates may be nil: a binary with no pricing wired reports unpriced rather than crashing.
func Settle(pctx *pipeline.Context, rates pricing.Resolver) Settled {
	var out Settled
	if pctx == nil {
		return out
	}

	cost, state := headerCost(pctx)
	if state == headerPositive {
		out.ReportedUSD, out.HasReported = cost, true
	} else if state == headerZero && !IsEventStream(pctx) {
		out.ReportedUSD, out.HasReported = 0, true
	}

	// Modelled alongside, always, so the pair is available for drift even when the
	// gateway's figure wins.
	usage := pricing.UsageFromInference(pctx.Extensions.Inference)
	model := ""
	if pctx.Extensions.Inference != nil {
		model = pctx.Extensions.Inference.Model
	}
	if micros, prov, ok := modelledCost(rates, pctx.Host, model, usage); ok {
		out.ModelledUSD, out.HasModelled, out.ModelledProv = float64(micros)/1e6, true, prov
	}

	// The prompt half, for a request row. Output zeroed rather than subtracted, because
	// Cost refuses to price a tier that carried tokens with no rate — and a partial total
	// presented as a whole one is worse than none.
	promptOnly := usage
	promptOnly.Output = 0
	if micros, _, ok := modelledCost(rates, pctx.Host, model, promptOnly); ok {
		out.PromptUSD, out.HasPrompt = float64(micros)/1e6, true
	}

	streamedPlaceholder := state == headerZero && IsEventStream(pctx)
	out.DeclaredFree = state == headerZero && !streamedPlaceholder

	switch {
	case state == headerPositive:
		out.CostUSD, out.Source, out.Provenance, out.Priced =
			cost, costevent.SourceGatewayHeader, pricing.ProvAuthoritative, true
	case out.DeclaredFree:
		// A real answer of zero. Nothing is charged, but it must be published as
		// PRICED so no consumer re-prices it from the usage block.
		out.CostUSD, out.Source, out.Provenance, out.Priced =
			0, costevent.SourceGatewayHeader, pricing.ProvAuthoritative, true
	case out.HasModelled:
		out.CostUSD, out.Source, out.Provenance, out.Priced =
			out.ModelledUSD, costevent.SourceUsageFallback, out.ModelledProv, true
	}
	return out
}

// modelledCost prices usage through the rate table.
//
// The nil guard is on the INTERFACE, which is the trap: an un-injected consumer holds a
// nil interface and calling a method on it panics, where a nil *pricing.Registry would
// have been safe. tool-prune hit exactly this and its fail-open masked the panic, so
// pruning silently stopped.
func modelledCost(rates pricing.Resolver, host, model string, u pricing.Usage) (int64, pricing.Provenance, bool) {
	if rates == nil || u == (pricing.Usage{}) {
		return 0, pricing.ProvNone, false
	}
	r, prov := rates.Resolve(host, model, u.PromptTotal())
	if prov == pricing.ProvNone {
		return 0, pricing.ProvNone, false
	}
	micros, ok := pricing.Cost(r, u)
	if !ok {
		return 0, pricing.ProvNone, false
	}
	return micros, prov, true
}

// StateKey is where the full Settled outcome is stashed for the rest of the request.
//
// In-process only — deliberately not on the wire. A consumer that needs the number reads
// the published costevent record; the drift check needs BOTH figures, and putting them in
// the session event would widen a public shape for one diagnostic's benefit. Keeping them
// here also means drift compares what was actually decided instead of recomputing a half
// and hoping the two agree.
const StateKey = "costing.settled"

// Store stashes the outcome for later plugins in the same request.
func Store(pctx *pipeline.Context, s Settled) {
	pipeline.SetState(pctx, StateKey, &s)
}

// Load retrieves the outcome. False means no cost owner ran — which, given costing runs in
// the parser and every consumer declares a hard dependency on it, means this request had no
// inference in it at all.
func Load(pctx *pipeline.Context) (Settled, bool) {
	if s := pipeline.GetState[Settled](pctx, StateKey); s != nil {
		return *s, true
	}
	return Settled{}, false
}

// Publish writes the wire record onto the session event.
//
// Under costevent.Key, and ALSO under the legacy plugin-name key: abctl is a separate
// binary that can lag the proxy, and an older one reads only the legacy key, so writing
// just the new one would blank the cost column for anyone who has not upgraded both. The
// legacy write comes out a release later.
//
// Ledger fields are left zero. They belong to whoever enforces a budget, which is not this
// package's business; litellm-budget-track amends them in place when it is present. That
// amendment is the one sanctioned second write to this key.
func Publish(pctx *pipeline.Context, ev costevent.Event) {
	if pctx.Extensions.Custom == nil {
		pctx.Extensions.Custom = map[string]any{}
	}
	pctx.Extensions.Custom[costevent.Key+pipeline.PluginEventSuffix] = ev
	pctx.Extensions.Custom[costevent.PluginName+pipeline.PluginEventSuffix] = ev
}

// Amend rewrites the published record, for a consumer adding fields it owns.
//
// Returns false when nothing has been published, so a caller cannot resurrect a record for
// a request that was never priced.
func Amend(pctx *pipeline.Context, f func(*costevent.Event)) bool {
	if pctx.Extensions.Custom == nil {
		return false
	}
	ev, ok := pctx.Extensions.Custom[costevent.Key+pipeline.PluginEventSuffix].(costevent.Event)
	if !ok {
		return false
	}
	f(&ev)
	Publish(pctx, ev)
	return true
}

// Record builds the wire record from a settled outcome.
func Record(s Settled, avoided []costevent.Saving) costevent.Event {
	return costevent.Event{
		CostUSD:    s.CostUSD,
		Source:     s.Source,
		Provenance: s.Provenance.String(),
		Settled:    s.Priced,
		PromptUSD:  s.PromptUSD,
		Avoided:    avoided,
	}
}
