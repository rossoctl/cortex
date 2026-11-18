package accounting

import (
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/settle"
)

// USD prices tokens of one tier with the process rate table, at the rates the request landed on:
// its endpoint, its model and its prompt total, so a long-context premium applies (design §5.6).
// It is the one pricing function of the accountant's figures: the ledger's savings and the
// metrics pricer both call it, so the two cannot price one token differently.
//
// estimated is true when the rate is a starting point rather than a fact about the account
// (pricing.ProvBundled), when there is no rate at all (pricing.ProvNone, or the tier is unset —
// the figure is then 0), and for a 1h write priced at the 5m fallback rate
// (pricing.Rates.CacheWrite1hRate). Zero tokens price at 0 and are not an estimate of anything.
//
// TierOutput is priced like any other tier; the library's TierTokens have no output, so only a
// host's own model calls reach it.
func USD(r pricing.Resolver, endpoint, model string, promptTotal int, t pricing.Tier, tokens int64) (usd float64, estimated bool) {
	if tokens == 0 {
		return 0, false
	}
	if r == nil {
		return 0, true
	}
	rates, prov := r.Resolve(endpoint, model, promptTotal)
	if prov == pricing.ProvNone {
		return 0, true
	}
	estimated = prov == pricing.ProvBundled
	var rate float64
	var ok bool
	if t == pricing.TierCacheWrite1h {
		var est1h bool
		rate, ok, est1h = rates.CacheWrite1hRate(settle.IsAnthropicModel(model))
		estimated = estimated || est1h
	} else {
		rate, ok = rates.For(t)
	}
	if !ok {
		return 0, true
	}
	return float64(tokens) * rate, estimated
}

// TierUSD prices the library's tokens by tier with USD: Input at TierInput, CacheWrite5m at
// TierCacheWrite, CacheWrite1h at TierCacheWrite1h, CacheRead at TierCacheRead. It is estimated
// when any tier that holds tokens is. The tiers are summed in dollars only; tokens of different
// tiers are never added together (R52).
//
// A negative count (a CacheEffect's Net can have one) prices negative.
func TierUSD(r pricing.Resolver, endpoint, model string, promptTotal int, tt accountant.TierTokens) (float64, bool) {
	var total float64
	var estimated bool
	for _, p := range []struct {
		tier   pricing.Tier
		tokens int
	}{
		{pricing.TierInput, tt.Input},
		{pricing.TierCacheWrite, tt.CacheWrite5m},
		{pricing.TierCacheWrite1h, tt.CacheWrite1h},
		{pricing.TierCacheRead, tt.CacheRead},
	} {
		usd, est := USD(r, endpoint, model, promptTotal, p.tier, int64(p.tokens))
		total += usd
		estimated = estimated || est
	}
	return total, estimated
}
