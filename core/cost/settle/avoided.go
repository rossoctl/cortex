package settle

import (
	"math"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// pruneFacts is the shape this package reads out of tool-prune's event.
//
// Read structurally rather than by importing the plugin, because the dependency must not
// run that way: a plugin that shrinks a body should know nothing about pricing, and this
// package should not link a plugin. Only the fields needed to price a saving are declared,
// so tool-prune can change everything else without touching settle.
type pruneFacts struct {
	BytesRemoved int
	// BodyBytesAfter is the body as the component left it. Avoided calibrates on it only
	// when no write took effect; otherwise on the size the framework recorded as sent.
	BodyBytesAfter int
	Projected      bool
}

// factsFrom pulls those fields off whatever a plugin published, via its JSON shape.
//
// The published value is a plugin-defined struct this package cannot name, so it is
// re-decoded through its own wire tags. That indirection is the price of not linking the
// plugin, and it is checked by a test that fails if tool-prune renames a field.
func factsFrom(v any) (pruneFacts, bool) {
	m, ok := asMap(v)
	if !ok {
		return pruneFacts{}, false
	}
	f := pruneFacts{
		BytesRemoved:   intField(m, "bytesRemoved"),
		BodyBytesAfter: intField(m, "bodyBytesAfter"),
		Projected:      boolField(m, "projected"),
	}
	if f.BytesRemoved <= 0 || f.BodyBytesAfter <= 0 {
		return pruneFacts{}, false
	}
	return f, true
}

// Avoided prices what other components kept off the wire, for this request.
//
// Runs HERE rather than in the component that did the pruning, because the dollar amount
// depends on which prompt-cache tier the saving came out of — 1x, 1.25x or 0.1x of the same
// rate — and that is only knowable from the response. The saving is inherently a
// request-fact times a response-fact, so it can only be priced where both are in hand.
//
// Every figure is an ESTIMATE derived from a byte ratio, and every entry says so. Nothing
// returned here is spend: see event.Saving.
func Avoided(pctx *pipeline.Context, rates pricing.Resolver) []event.Saving {
	if pctx == nil || len(pctx.Extensions.Custom) == 0 {
		return nil
	}
	usage := pricing.UsageFromInference(pctx.Extensions.Inference)
	prompt := usage.PromptTotal()
	if prompt <= 0 {
		return nil // no response usage yet: nothing to calibrate against
	}
	model := ""
	if pctx.Extensions.Inference != nil {
		model = pctx.Extensions.Inference.Model
	}
	// CALIBRATED ON THE BODY THE REQUEST SENT, the one the prompt tokens were counted on.
	// Request writers chain, so a component's own figure is the body as IT left it, and a
	// later writer may have shrunk or grown it: dividing by that size mis-states
	// tokens-per-byte, and every saving priced from it, by the ratio of the two. Once any
	// write took effect, pctx.Body is the body the listener sent: each applied SetBody
	// replaces it, a shadow write under on_error: observe leaves it alone, and no listener
	// changes it after the request pass. With no applied write the body went as the
	// client sent it, and the component's figure is that size.
	cal := calibration{usage: usage, prompt: prompt, rewritten: pctx.BodyMutated(), sentBytes: len(pctx.Body)}

	var out []event.Saving
	for _, sc := range savingComponents {
		v, ok := pctx.Extensions.Custom[sc.name+pipeline.PluginEventSuffix]
		if !ok {
			continue
		}
		d, ok := sc.draft(v, cal)
		if !ok || d.tokens <= 0 {
			continue
		}
		// NOT CALIBRATED ON A REPORT THIS PACKAGE REFUSED. AvoidedUsage slices the response's own
		// counters, so an impossible report produced an impossible saving — measured: 2,000,000
		// tokens avoided at $20, on a record marked refused. The saving comes from the same numbers
		// Settle declined, so it goes with them.
		if !pricing.PlausibleUsage(usage) {
			continue
		}
		s := event.Saving{
			Component:     sc.name,
			TokensAvoided: d.tokens,
			Tier:          d.tier.String(),
			Projected:     d.projected,
			Estimated:     true,
		}
		// Priced at the REQUEST's prompt size (`prompt`), not the saving's own token
		// count. A saving is a slice of this request's prompt, so it bills at the rates
		// this request landed on — including a long-context premium, which keying the
		// lookup on the slice would have dropped: 9.7k avoided tokens resolve at At(9700)
		// and miss a 200k threshold the 641k request they came out of is well past.
		// The tier split is discarded here, deliberately. A saving is not spend — nothing may
		// add it to a cost total in either direction — so splitting it by tier would invite
		// exactly the addition the invariant forbids, and compound a bytes-to-tokens estimate
		// with a modelled ratio into a figure nobody could defend.
		if micros, _, prov, ok, _ := modelledCost(rates, pctx.Host, model, d.saved, prompt); ok {
			s.USD, s.Provenance = float64(micros)/1e6, prov.String()
		}
		if d.prefixRewritten {
			s.PenaltyUSD = rewritePenalty(rates, pctx.Host, model, usage.CacheWrite, prompt)
		}
		// Published even with no dollar figure: the token saving is still known, and
		// reporting 0 dollars where a rate is missing would read as "this saved
		// nothing" rather than "we cannot price it".
		out = append(out, s)
	}
	return out
}

// savingComponents names the plugins whose events describe a body reduction worth pricing.
//
// An explicit list, not a scan for anything with a bytesRemoved field: a saving attributed
// to the wrong component is worse than one not attributed at all, and a plugin should not be
// able to opt itself into the money record by naming a field conveniently. Adding a
// component here is a deliberate act with a test.
var savingComponents = []savingComponent{
	{name: "tool-prune", draft: pruneDraft},
	{name: compactionComponent, draft: compactionDraft},
}

// savingComponent is one component on the list and the rule that reads its event. Each event has
// its own facts type, so a field one component publishes can never be read off another's.
type savingComponent struct {
	name  string
	draft func(v any, c calibration) (draft, bool)
}

// calibration is what every saving is measured against: this response's usage, and the body the
// prompt tokens were counted on.
type calibration struct {
	usage     pricing.Usage
	prompt    int
	rewritten bool // a request write took effect, so sentBytes is the body sent
	sentBytes int
}

// bodyBytes is the size to calibrate tokens-per-byte on: the body sent once any write took
// effect, else the component's own figure (see Avoided).
func (c calibration) bodyBytes(own int) int {
	if c.rewritten {
		return c.sentBytes
	}
	return own
}

// draft is a saving before it is priced: how many tokens, at which tier, and whether the
// component also caused a cache-rewrite penalty on this request.
type draft struct {
	tokens          int
	saved           pricing.Usage
	tier            pricing.Tier
	projected       bool
	prefixRewritten bool
}

// pruneDraft is tool-prune's rule: bytes to tokens on the response's own ratio, priced at the
// tier the prompt landed in.
func pruneDraft(v any, c calibration) (draft, bool) {
	f, ok := factsFrom(v)
	if !ok {
		return draft{}, false
	}
	tokens := pricing.EstimateTokensFromBytes(f.BytesRemoved, c.prompt, c.bodyBytes(f.BodyBytesAfter))
	saved, tier := pricing.AvoidedUsage(c.usage, tokens)
	return draft{tokens: tokens, saved: saved, tier: tier, projected: f.Projected}, true
}

// compactionComponent is context-guru, priced by its own rules: see compactionDraft and
// docs/superpowers/specs/2026-10-14-contextguru-savings-and-spend-design.md §1.4.
const compactionComponent = "context-guru"

// compactionFacts is the shape this package reads out of context-guru's event
// (plugins/contextguru savingEvent). Read structurally, for the reason pruneFacts gives; the
// plugin's TestPublishedSavingIsPricedBySettle fails if either side renames a field.
type compactionFacts struct {
	BytesRemoved   int
	BodyBytesAfter int
	Projected      bool
	// TokensBefore and TokensAfter are the engine's own counts for the whole request.
	TokensBefore int
	TokensAfter  int
	// PrefixRewritten says a change landed inside the prefix the provider had cached.
	PrefixRewritten bool
	// UniqueFrac is the part of the saving removed for the first time in this session
	// (SavedUnique/SavedGross), in [0, 1]. The rest is content removed again on a later turn.
	// 1 when the event does not split its saving.
	UniqueFrac float64
}

// compactionFactsFrom decodes context-guru's event. Refused, like factsFrom, without a positive
// byte figure: the byte ratio is the fallback when the engine counts are absent.
func compactionFactsFrom(v any) (compactionFacts, bool) {
	m, ok := asMap(v)
	if !ok {
		return compactionFacts{}, false
	}
	f := compactionFacts{
		BytesRemoved:    intField(m, "bytesRemoved"),
		BodyBytesAfter:  intField(m, "bodyBytesAfter"),
		Projected:       boolField(m, "projected"),
		TokensBefore:    intField(m, "tokensBefore"),
		TokensAfter:     intField(m, "tokensAfter"),
		PrefixRewritten: boolField(m, "prefixRewritten"),
		UniqueFrac:      1,
	}
	if f.BytesRemoved <= 0 || f.BodyBytesAfter <= 0 {
		return compactionFacts{}, false
	}
	if gross := intField(m, "savedGross"); gross > 0 {
		f.UniqueFrac = min(1, max(0, float64(intField(m, "savedUnique"))/float64(gross)))
	}
	return f, true
}

// compactionDraft is context-guru's rule. Tokens are the engine's own count of what it removed,
// preferred to the byte ratio because compaction rewrites message text, whose token density need
// not match the rest of the body — still an estimate, since the engine's tokenizer is not the
// provider's. The tokens split by UniqueFrac: the unique part is priced at compactionUsage's tier,
// the repeated part at repeatUsage's. A prefix rewrite carries a penalty.
func compactionDraft(v any, c calibration) (draft, bool) {
	f, ok := compactionFactsFrom(v)
	if !ok {
		return draft{}, false
	}
	tokens := pricing.EstimateTokensFromBytes(f.BytesRemoved, c.prompt, c.bodyBytes(f.BodyBytesAfter))
	if f.TokensBefore > f.TokensAfter && f.TokensAfter > 0 {
		tokens = f.TokensBefore - f.TokensAfter
	}
	unique := int(math.Round(float64(tokens) * f.UniqueFrac))
	saved, tier := compactionUsage(c.usage, unique)
	if repeat := tokens - unique; repeat > 0 {
		r, rTier := repeatUsage(c.usage, repeat)
		saved = addUsage(saved, r)
		// One tier per saving on the wire: name the larger part. USD prices both.
		if repeat > unique {
			tier = rTier
		}
	}
	return draft{tokens: tokens, saved: saved, tier: tier, projected: f.Projected,
		prefixRewritten: f.PrefixRewritten}, true
}

// compactionUsage is the tier the UNIQUE part of a compaction would have been billed at: content
// removed for the first time. The engine compacts the uncached tail, so without compaction those
// tokens were fresh input — or a cache write, on a turn whose response shows the tail being
// written. Never cache-read: the tier the compacted prompt landed in (pricing.AvoidedUsage's rule)
// is not the counterfactual here.
func compactionUsage(u pricing.Usage, tokens int) (pricing.Usage, pricing.Tier) {
	if u.CacheWrite > 0 {
		return pricing.Usage{CacheWrite: tokens}, pricing.TierCacheWrite
	}
	return pricing.Usage{Input: tokens}, pricing.TierInput
}

// repeatUsage is the tier the REPEATED part would have been billed at: content removed on an
// earlier turn and removed again now. Without compaction it would sit in the cached prefix by now,
// so it is a cache read — when this request's cache hit. On a miss the provider re-bills the whole
// prompt: at the cache-write rate when the write covers the prompt (writes at least the fresh
// input), else as fresh input. This is the context-guru proxy's dash.Event.repeatRate, so the two
// report the same saving for the same request.
//
// Pricing repeats as unique was the over-report this replaces: on a long session nearly all of a
// turn's saving is repeated content, and pricing it at 1x or 1.25x instead of 0.1x reported about
// five times the measured saving.
func repeatUsage(u pricing.Usage, tokens int) (pricing.Usage, pricing.Tier) {
	switch {
	case u.CacheRead > 0:
		return pricing.Usage{CacheRead: tokens}, pricing.TierCacheRead
	case u.CacheWrite > 0 && u.CacheWrite >= u.Input:
		return pricing.Usage{CacheWrite: tokens}, pricing.TierCacheWrite
	default:
		return pricing.Usage{Input: tokens}, pricing.TierInput
	}
}

// addUsage sums two prompt-tier usages. Only the prompt tiers: a saving has no output.
func addUsage(a, b pricing.Usage) pricing.Usage {
	return pricing.Usage{Input: a.Input + b.Input, CacheWrite: a.CacheWrite + b.CacheWrite,
		CacheRead: a.CacheRead + b.CacheRead}
}

// rewritePenalty prices a rewrite inside the cached prefix: the response's cache-write tokens at
// the cache-write rate, less what the same tokens would have cost read from cache. An UPPER
// bound, because part of a turn's cache write is its new tail, which is written anyway — so it
// leans toward "compaction did not pay", the safe direction for a savings figure. Zero when
// either side cannot be priced.
func rewritePenalty(rates pricing.Resolver, host, model string, cacheWrite, prompt int) float64 {
	if cacheWrite <= 0 {
		return 0
	}
	written, _, _, okW, _ := modelledCost(rates, host, model, pricing.Usage{CacheWrite: cacheWrite}, prompt)
	read, _, _, okR, _ := modelledCost(rates, host, model, pricing.Usage{CacheRead: cacheWrite}, prompt)
	if !okW || !okR || written <= read {
		return 0
	}
	return float64(written-read) / 1e6
}
