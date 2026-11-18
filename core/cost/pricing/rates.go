// Package pricing owns model rates and the arithmetic that turns tokens into
// dollars. Before it, rates lived in two plugins' configs — 16 fields between them
// — and dollars were computed in four places, which is how a stale "understates by
// 4x" comment survived a gateway repricing.
//
// Cost is the only implementation of that arithmetic now. One multiplication
// survives outside it, in tool-prune's metrics (tokens x rate for its own $ saved
// counter), which therefore skips this package's micro quantization and can
// disagree with the rendered figure in the last digit.
//
// The package holds no I/O and no provider knowledge. A rate table is built once
// at startup and swapped atomically; resolution is a pure function of
// (endpoint, model, prompt size).
package pricing

// Tier names which kind of token is being priced.
//
// Four tiers, because that is what every provider on a Claude path bills
// separately and prompt-cache tiers differ by more than 12x: a cache write is
// 1.25x input and a cache read 0.1x, so one flat rate misprices cache-heavy
// traffic (Claude Code) by up to ~10x.
//
// Deliberately absent: service-tier variants (flex / priority / ultrafast). A real
// field in LiteLLM's ModelInfoBase, not on a path we run, and an unpopulated tier is
// an untested tier.
//
// The 1-hour-TTL cache-write premium is NOT absent (2026-11-17
// accountant-integration task 7 added TierCacheWrite1h) — this paragraph used to
// call it out alongside service tiers, and that line is now wrong rather than
// merely stale.
type Tier int

const (
	TierInput Tier = iota
	TierCacheWrite
	TierCacheRead
	TierOutput

	// TierCacheWrite1h addresses the 1-hour cache-write premium through Rates.For
	// and Rates.CacheWrite1hRate. Deliberately NOT one of the numTiers slots Cost
	// sums over: CacheWrite1h tokens are already counted once inside Usage.CacheWrite
	// (see Usage.CacheWrite1h), so folding this into the generic per-tier loop would
	// double-bill them. settle.modelledCost re-prices just the 1h subset at the 1h
	// rate and adds the DIFFERENCE from the 5m rate CostByTier already applied.
	// Added for the 1h cache-write tier (2026-11-17 accountant-integration task 7) —
	// the doc above calling it "deliberately absent" predates this.
	TierCacheWrite1h
)

// numTiers sizes the per-tier arrays. Arrays rather than named fields so the
// arithmetic is a loop over every tier: an enumerated form is where a forgotten
// tier hides, and a forgotten tier silently prices at zero.
const numTiers = 4

// NumTiers is numTiers for callers outside this package, so a consumer sizing a
// per-tier array names the constant instead of writing 4 — a literal that would go
// stale silently if a tier were ever added, which is the failure numTiers' own doc
// says arrays exist to prevent.
const NumTiers = numTiers

// Usage is one request's token count, split by tier.
//
// This mirrors parsercommon.TokenUsage, which is what the inference parsers
// already publish. This package cannot import parsercommon: parsercommon's
// tests import cost/usage, which imports this package, so the import makes a
// test import cycle.
// Reasoning is omitted (a subset of Output, already counted) and so is
// ReportedTotal (an aggregate no tier prices).
type Usage struct {
	Input      int // uncached prompt tokens
	CacheWrite int // prompt tokens written to cache
	CacheRead  int // prompt tokens served from cache
	Output     int // generated completion tokens
	// CacheWrite1h is the part of CacheWrite written with a 1-hour TTL, never an
	// addition to it — mirrors model.InferenceExtension.CacheWrite1hTokens.
	// Deliberately NOT read by tokens()/CostByTier's generic per-tier loop: those
	// tokens are already counted once inside CacheWrite, and settle.modelledCost
	// re-prices just this subset at the 1h rate on top (Rates.CacheWrite1hRate).
	// Added for the 1h cache-write tier (2026-11-17 accountant-integration task 7).
	CacheWrite1h int
}

// PromptTotal is the prompt-side total, which is what a context threshold is
// measured against. Output is excluded: a long-context premium is priced on how
// much prompt was sent, not on what came back.
func (u Usage) PromptTotal() int { return u.Input + u.CacheWrite + u.CacheRead }

// tokens indexes the counts by Tier so Cost can loop over every tier.
func (u Usage) tokens() [numTiers]int {
	return [numTiers]int{
		TierInput:      u.Input,
		TierCacheWrite: u.CacheWrite,
		TierCacheRead:  u.CacheRead,
		TierOutput:     u.Output,
	}
}

// ContextThreshold is a long-context rate override: above AbovePromptTokens the
// tiers it Sets replace the base ones.
//
// Tiers it does not set keep their base rate rather than becoming unset. A table
// that prices only long-context input would otherwise unset output above the
// threshold, and Cost's per-tier invariant would flip the whole request to
// unpriced — turning a partial price list into a coverage hole.
type ContextThreshold struct {
	AbovePromptTokens int
	Rate              [numTiers]float64
	Set               [numTiers]bool
}

// Rates is one (endpoint, model) pair's rates, in USD per token.
//
// Set exists because a zero rate and an absent rate are different answers. Zero
// means free; absent means unknown, and Cost refuses to price a request whose
// tokens land on an absent tier (see Cost).
type Rates struct {
	Base       [numTiers]float64
	Set        [numTiers]bool
	Thresholds []ContextThreshold
	// CacheWrite1h and CacheWrite1hSet are an EXPLICIT 1-hour cache-write rate, kept
	// outside the Base/Set arrays because TierCacheWrite1h is not one of the numTiers
	// slots Cost sums over (see TierCacheWrite1h). Unset on every table this package
	// ships today — no bundled or configured entry names one yet — which is exactly
	// the case Rates.For and Rates.CacheWrite1hRate fall back for. Added for the 1h
	// cache-write tier (2026-11-17 accountant-integration task 7).
	CacheWrite1h    float64
	CacheWrite1hSet bool
}

// At flattens Rates for a prompt of promptTotal tokens: the highest threshold
// strictly exceeded, overlaid on Base, with Thresholds cleared.
//
// Strictly exceeded, not met — "$X above 200k tokens" means the premium starts at
// token 200,001, so a request of exactly 200k pays base.
//
// The scan takes the maximum exceeded threshold rather than the first, so At does
// not depend on slice order. Order-dependence here would be a footgun with no
// visible symptom: a config or test that happened to list thresholds ascending
// would price long context at the lower premium and look plausible.
//
// Idempotent: the result carries no thresholds, so flattening it again returns it
// unchanged. Resolve hands out flattened rates and Cost flattens again.
func (r Rates) At(promptTotal int) Rates {
	// CacheWrite1h/CacheWrite1hSet carried forward: a threshold row never sets them
	// (ContextThreshold has no slot for TierCacheWrite1h — it is not in Base/Set),
	// so there is nothing for the loop below to overlay, but the flattened result
	// must still answer the explicit rate a caller configured.
	out := Rates{Base: r.Base, Set: r.Set, CacheWrite1h: r.CacheWrite1h, CacheWrite1hSet: r.CacheWrite1hSet}
	best := -1
	var pick *ContextThreshold
	for i := range r.Thresholds {
		t := &r.Thresholds[i]
		if promptTotal > t.AbovePromptTokens && t.AbovePromptTokens > best {
			best, pick = t.AbovePromptTokens, t
		}
	}
	if pick == nil {
		return out
	}
	for i := range out.Base {
		if pick.Set[i] {
			out.Base[i], out.Set[i] = pick.Rate[i], true
		}
	}
	return out
}

// For returns the rate for one tier and whether a rate is actually available.
//
// The bool is the per-tier invariant in accessor form, and it is the whole reason
// this is not a plain field read: Set is per tier, so a model priced only for cache
// reads must report "no rate" for a cache write rather than the zero value. Cost
// enforces the same rule across a whole request; consumers that publish individual
// rates (tool-prune's per-request event) use this.
//
// Call it on rates from Resolve or At, which are already flattened for the
// request's prompt size. On unflattened rates it reads the BASE tier and ignores
// thresholds — correct for "what is this model's list rate", wrong for pricing a
// specific request.
func (r Rates) For(t Tier) (float64, bool) {
	if t == TierCacheWrite1h {
		// FALLS BACK, as the type's own doc promises: with no explicit 1h rate this
		// has no provider knowledge to reach for (CacheWrite1hRate is where that
		// lives, for a caller that has a model name to test), so the only answer
		// For can give is the 5-minute write rate — the SAME number
		// CostByTier's generic loop already prices these tokens at, since they are
		// counted once inside CacheWrite. A caller pricing TierCacheWrite1h through
		// this method alone therefore sees no 1h premium at all; settle.modelledCost
		// is what applies one, via CacheWrite1hRate.
		rate, ok := r.For(TierCacheWrite)
		if r.CacheWrite1hSet {
			return r.CacheWrite1h, true
		}
		return rate, ok
	}
	if int(t) < 0 || int(t) >= numTiers {
		return 0, false
	}
	return r.Base[t], r.Set[t]
}

// CacheWrite1hRate derives the per-token rate for a cache write made with a 1-hour
// TTL, three ways in order:
//
//  1. An EXPLICIT 1h rate (CacheWrite1hSet) always wins and is never estimated —
//     an operator or a future table row that names one has told us the real figure.
//  2. anthropicShaped, for a table with no explicit rate: Anthropic documents its
//     1-hour cache-write premium as exactly 2x the input rate (platform.claude.com,
//     prompt-caching § cache duration), so this derivation is not a guess either —
//     estimated is false. The caller passes this rather than Rates detecting it
//     itself: Rates carries no provider or model name, and settle.modelledCost
//     already has the model string Cost does not.
//  3. Otherwise: the 5-minute write rate, the same fallback Rates.For(TierCacheWrite1h)
//     uses — and estimated is true, because nothing here says what a non-Anthropic
//     provider's 1h premium actually is.
//
// ok is false only when even the 5m write rate is unset, which is Cost's existing
// RefusalNoRate case: a table that cannot price ordinary cache writes cannot price
// 1-hour ones either.
func (r Rates) CacheWrite1hRate(anthropicShaped bool) (rate float64, ok, estimated bool) {
	if r.CacheWrite1hSet {
		return r.CacheWrite1h, true, false
	}
	if anthropicShaped {
		if in, inSet := r.For(TierInput); inSet {
			return in * 2.0, true, false
		}
	}
	cw, cwSet := r.For(TierCacheWrite)
	return cw, cwSet, cwSet
}

// free reports whether these rates price every tier at zero, with no long-context
// override. A view renders it as free rather than as unset, which is what a zero rate
// field otherwise means. EVERY tier, not every set one: a view prints "free" across all
// four columns, so a row with an unset tier would show a coverage gap as free.
func (r Rates) free() bool {
	if len(r.Thresholds) > 0 {
		return false
	}
	for t := range r.Set {
		if !r.Set[t] || r.Base[t] != 0 {
			return false
		}
	}
	return true
}

// any reports whether these rates price anything at all, at any prompt size.
// NewTable uses it to reject a row that prices nothing: such a row would match
// traffic and then resolve it as unpriced, which is indistinguishable from having
// no row and hides the typo that produced it.
func (r Rates) any() bool {
	if r.CacheWrite1hSet {
		return true
	}
	for _, s := range r.Set {
		if s {
			return true
		}
	}
	for _, t := range r.Thresholds {
		for _, s := range t.Set {
			if s {
				return true
			}
		}
	}
	return false
}

// PromptTier picks which prompt tier a saving on the cached prefix came out of.
//
// A write-dominant request wrote the prefix, a read-dominant one read it, and one
// reporting no cache tokens at all sent it as plain input. That distinction is
// worth more than it looks: a cache write bills at 1.25x the input rate and a read
// at 0.1x, so identical saved tokens are worth over 12x more on a miss than a hit,
// and reporting one blended figure would hide a factor of twelve.
//
// Lives here rather than in a consumer because two of them need it — the plugin
// that measures the saving and the UI that renders it — and a second copy of this
// rule would be a second place for it to drift.
//
// This assumes the saving sits inside the cached prefix, which holds for a client
// that puts cache_control on its tool block (Claude Code does). It is the one
// assumption to revisit for a client that lays its prompt out differently.
func PromptTier(u Usage) Tier {
	switch {
	case u.CacheWrite > u.CacheRead && u.CacheWrite > 0:
		return TierCacheWrite
	case u.CacheRead > 0:
		return TierCacheRead
	default:
		return TierInput
	}
}

// String names a tier using the spelling operators write in `pricing:` config, so a tier
// reported in a cost record or an error can be pasted straight into a YAML key without
// translation.
func (t Tier) String() string {
	switch t {
	case TierInput:
		return "input"
	case TierCacheWrite:
		return "cache_write"
	case TierCacheRead:
		return "cache_read"
	case TierOutput:
		return "output"
	case TierCacheWrite1h:
		return "cache_write_1h"
	}
	return "unknown"
}

// TierFromString is the inverse of Tier.String, for a tier that has been through JSON.
//
// Reports false for anything unrecognized rather than defaulting to input: a saving
// attributed to the wrong tier is off by up to 12.5x, and silently picking the cheapest
// interpretation would understate it.
func TierFromString(s string) (Tier, bool) {
	switch s {
	case "input":
		return TierInput, true
	case "cache_write":
		return TierCacheWrite, true
	case "cache_read":
		return TierCacheRead, true
	case "output":
		return TierOutput, true
	case "cache_write_1h":
		return TierCacheWrite1h, true
	}
	return TierInput, false
}
