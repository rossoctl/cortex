package pricing

import "math"

// EstimateTokensFromBytes converts a byte-level body reduction into an estimated prompt
// token reduction, calibrated on the request it came from.
//
// ESTIMATE, and named so. The calibration is this request's own measured ratio —
// promptTokens over the body size actually sent — which is sound when the removed span
// had the same token density as what remained, and wrong when it did not. A tool manifest
// is denser in punctuation than prose, so a prune measured against a prompt of mostly
// prose overstates its own saving. The output gets quoted in dollars, so that has to be
// said out loud rather than left for a reader to infer.
//
// bodyBytes must be the size of the body ACTUALLY SENT upstream, which is not the pruned
// size when a plugin measured a saving without applying it: dividing by the smaller
// counterfactual size would inflate tokens-per-byte and overstate the saving.
//
// Returns 0 when any input is non-positive, which covers "nothing was removed", "no
// response yet" and "no body" without the caller distinguishing them — there is no
// estimate to make in any of those cases.
func EstimateTokensFromBytes(bytesRemoved, promptTokens, bodyBytes int) int {
	if bytesRemoved <= 0 || promptTokens <= 0 || bodyBytes <= 0 {
		return 0
	}
	tokens := float64(bytesRemoved) * float64(promptTokens) / float64(bodyBytes)
	if tokens < 0.5 {
		// Rounds to zero. Report it as zero rather than as a token, so a saving too
		// small to price does not read as one that was priced at nothing.
		return 0
	}
	return int(math.Round(tokens))
}

// AvoidedUsage places an estimated prompt-token saving into the single tier the request's
// prompt actually landed in.
//
// One tier, not spread across them: the removed bytes were part of one prompt, and that
// prompt was billed as cache-write, cache-read or fresh input. Which one changes the
// answer by up to 12.5x, so the choice is the whole substance of the figure and belongs
// beside PromptTier rather than in a caller.
//
// A deliberate simplification: a prompt can straddle tiers — some cached, some fresh —
// and PromptTier picks the dominant one. Attributing the saving to the dominant tier is
// the best available answer without knowing WHERE in the prompt the removal happened.
//
// A cache-write saving keeps the request's 1h share: out.CacheWrite1h is tokens times
// u.CacheWrite1h / u.CacheWrite, rounded to the nearest token. So a saving from all-1h
// writes is all 1h and prices at the 1h rate, as settlement prices the request's own
// writes; a saving from all-5m writes has no 1h part and keeps its 5m price; and a saving
// from mixed writes is split in the same ratio as the writes, because nothing says which
// TTL the removed bytes were written under.
func AvoidedUsage(u Usage, tokens int) (Usage, Tier) {
	tier := PromptTier(u)
	out := Usage{}
	switch tier {
	case TierCacheWrite:
		out.CacheWrite = tokens
		out.CacheWrite1h = share1h(u, tokens)
	case TierCacheRead:
		out.CacheRead = tokens
	default:
		tier = TierInput
		out.Input = tokens
	}
	return out, tier
}

// share1h is the part of a tokens-sized cache-write saving that carries u's 1h share:
// tokens * u.CacheWrite1h / u.CacheWrite, rounded and held within [0, tokens]. 0 when u
// reports no 1h writes, or a 1h count outside [0, CacheWrite] that PlausibleUsage would
// refuse anyway.
func share1h(u Usage, tokens int) int {
	if u.CacheWrite <= 0 || u.CacheWrite1h <= 0 || u.CacheWrite1h > u.CacheWrite || tokens <= 0 {
		return 0
	}
	if u.CacheWrite1h == u.CacheWrite {
		return tokens
	}
	return int(math.Round(float64(tokens) * float64(u.CacheWrite1h) / float64(u.CacheWrite)))
}
