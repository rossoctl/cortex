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
// an untested tier. Adding one later is a field plus a threshold row, not a reshape.
//
// The 1-hour-TTL cache-write premium is not a tier either, for a different reason:
// those tokens are a SUBSET of CacheWrite, so a fifth slot in the per-tier loop would
// bill them twice. Usage.CacheWrite1h carries the count and CostWith1h adds the
// premium on top of CostByTier.
type Tier int

const (
	TierInput Tier = iota
	TierCacheWrite
	TierCacheRead
	TierOutput
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
	// addition to it. It mirrors model.InferenceExtension.CacheWrite1hTokens.
	// tokens() does not return it, so CostByTier prices these tokens once, inside
	// CacheWrite, at the 5m rate. CostWith1h adds the 1h premium on top.
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
	out := Rates{Base: r.Base, Set: r.Set}
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
	if int(t) < 0 || int(t) >= numTiers {
		return 0, false
	}
	return r.Base[t], r.Set[t]
}

// CacheWrite1hRate derives the per-token rate for a cache write made with a 1-hour
// TTL. No table carries an explicit 1h rate, so the rate is derived:
//
//  1. anthropicShaped AND the table's own 5m write rate is Anthropic's documented
//     1.25x-of-input shape (isAnthropicCacheWriteShape): Anthropic's 1-hour price is
//     exactly 2x the input rate (platform.claude.com, prompt-caching § cache
//     duration), so estimated is false. anthropicShaped alone is not enough — it
//     names the MODEL (see AnthropicShaped), and a configured or remote rate
//     attached to that model name is not guaranteed to bill at Anthropic's real
//     ratio. A flat-rate gateway that happens to serve a Claude model would
//     otherwise have its cache writes doubled with no estimated caveat, for a
//     figure the table itself gives no reason to believe.
//  2. Otherwise: the 5-minute write rate, and estimated is true, because nothing
//     here says what another provider's 1h premium is. That figure is low, never
//     high, for any provider whose 1h writes cost more than its 5m writes.
//
// The rates are read from r as given, so a caller that wants the long-context rate
// passes rates already flattened by At (Resolve returns them flattened). ok is false
// only when the rate the derivation needs is unset.
func (r Rates) CacheWrite1hRate(anthropicShaped bool) (rate float64, ok, estimated bool) {
	cw, cwSet := r.For(TierCacheWrite)
	if anthropicShaped && cwSet {
		if in, inSet := r.For(TierInput); inSet && isAnthropicCacheWriteShape(in, cw) {
			return in * 2.0, true, false
		}
	}
	return cw, cwSet, cwSet
}

// isAnthropicCacheWriteShape reports whether cacheWrite5m is Anthropic's documented
// 1.25x-of-input cache-write premium, within a tolerance wide enough to absorb
// rounding in a bundled or hand-typed table but narrow enough to exclude a rate
// priced some other way — a flat per-token gateway, for instance, or one with its
// own, different cache premium. This is the check that makes anthropicShaped a
// statement about the TABLE as well as the model name: see CacheWrite1hRate.
func isAnthropicCacheWriteShape(input, cacheWrite5m float64) bool {
	if input <= 0 {
		return false
	}
	const (
		documentedRatio = 1.25
		tolerance       = 0.05
		// epsilon absorbs the float64 division itself, not the table's own rounding:
		// the tolerance window's lower edge (1.20) is exactly Claude 3 Haiku's ratio,
		// but cacheWrite5m/input for equivalent, equally real-looking rates can land
		// one ULP on either side of 1.20 depending on which decimal values divide to
		// it — a per-million config value of 0.10/0.12 lands one ULP BELOW 1.20,
		// where 0.25/0.30 (Haiku 3's actual bundled rate) lands AT it. Without this, a
		// row with Haiku 3's real ratio but typed from different decimal inputs could
		// fail a window that Haiku 3 itself passes.
		epsilon = 1e-9
	)
	// CLOSED interval, not open: Claude 3 Haiku's bundled row (core/cost/pricing/bundled.go)
	// is priced at exactly 1.20, the lower edge, and an open interval excluded it — the
	// bundled table's own first-party row losing the 2x-input derivation it documents.
	// TestCacheWrite1hRate_EveryBundledClaudeRowGetsTheDerivation pins every bundled Claude
	// row against this window so a future vendor row outside it fails a test, not silently.
	ratio := cacheWrite5m / input
	return ratio >= documentedRatio-tolerance-epsilon && ratio <= documentedRatio+tolerance+epsilon
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
	}
	return TierInput, false
}
