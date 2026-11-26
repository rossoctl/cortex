package accounting

import (
	"time"

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
// (pricing.ProvBundled), or when there is no rate at all (pricing.ProvNone, or the tier is unset —
// the figure is then 0). Zero tokens price at 0 and are not an estimate of anything.
//
// TierOutput is priced like any other tier; the library's TierTokens have no output, so only a
// host's own model calls reach it.
//
// Cache write 1h tokens are a SUBSET of CacheWrite and have no Tier of their own (see
// pricing.Tier's doc), so they are priced by USD1h, not this function.
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
	rate, ok := rates.For(t)
	if !ok {
		return 0, true
	}
	return float64(tokens) * rate, estimated
}

// USD1h is USD for 1-hour-TTL cache-write tokens, which price at pricing.Rates.CacheWrite1hRate
// rather than at a Tier's flat rate — see pricing.Tier's doc for why that rate has no Tier of its
// own. estimated is additionally true when CacheWrite1hRate itself falls back to the 5m rate.
func USD1h(r pricing.Resolver, endpoint, model string, promptTotal int, tokens int64) (usd float64, estimated bool) {
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
	rate, ok, est1h := rates.CacheWrite1hRate(settle.IsAnthropicModel(model))
	estimated = estimated || est1h
	if !ok {
		return 0, true
	}
	return float64(tokens) * rate, estimated
}

// TierUSD prices the library's tokens by tier with USD: Input at TierInput, CacheWrite5m at
// TierCacheWrite, CacheWrite1h via USD1h, CacheRead at TierCacheRead. It is estimated when any
// tier that holds tokens is. The tiers are summed in dollars only; tokens of different tiers are
// never added together (R52).
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
		{pricing.TierCacheRead, tt.CacheRead},
	} {
		usd, est := USD(r, endpoint, model, promptTotal, p.tier, int64(p.tokens))
		total += usd
		estimated = estimated || est
	}
	usd1h, est1h := USD1h(r, endpoint, model, promptTotal, int64(tt.CacheWrite1h))
	total += usd1h
	estimated = estimated || est1h
	return total, estimated
}

// tierRate is the rate of one token of tier t at the endpoint, the model and the prompt total:
// USD, or USD1h for TierCacheWrite1h, which has no pricing.Tier of its own. An unknown tier has
// no rate: (0, true), "no money, and estimated".
func tierRate(r pricing.Resolver, endpoint, model string, promptTotal int, t accountant.Tier) (float64, bool) {
	if t == accountant.TierCacheWrite1h {
		return USD1h(r, endpoint, model, promptTotal, 1)
	}
	tier, ok := pricingTier(t)
	if !ok {
		return 0, true
	}
	return USD(r, endpoint, model, promptTotal, tier, 1)
}

// sizedRates is the ledger's accountant.SizedPricer: tierRate at one endpoint, at the prompt size
// accountant.Figure asks for. Figure asks for two sizes on a request: the size without the
// operations for what they saved, and the billed size for what they caused (rossoctl/cortex#2135).
type sizedRates struct {
	r        pricing.Resolver
	endpoint string
}

func (s sizedRates) PriceAt(model string, _ time.Time, t accountant.Tier, promptTokens int64) (float64, bool) {
	return tierRate(s.r, s.endpoint, model, int(promptTokens), t)
}
