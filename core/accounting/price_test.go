package accounting

import (
	"math"
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/cost/pricing"
)

// fixedRates is a Resolver that answers every lookup with one rate set, and records the last
// lookup so a test can see what was asked.
type fixedRates struct {
	rates pricing.Rates
	prov  pricing.Provenance

	endpoint, model string
	promptTotal     int
}

func (f *fixedRates) Resolve(endpoint, model string, promptTotal int) (pricing.Rates, pricing.Provenance) {
	f.endpoint, f.model, f.promptTotal = endpoint, model, promptTotal
	return f.rates, f.prov
}

func (f *fixedRates) CurrencyFor(string, string) string { return "USD" }

// testRates are per-token rates with every tier set: input 3e-6, cache write 3.75e-6, cache read
// 3e-7, output 1.5e-5 (Sonnet's list rates).
func testRates(prov pricing.Provenance) *fixedRates {
	r := pricing.Rates{}
	r.Base[pricing.TierInput], r.Set[pricing.TierInput] = 3e-6, true
	r.Base[pricing.TierCacheWrite], r.Set[pricing.TierCacheWrite] = 3.75e-6, true
	r.Base[pricing.TierCacheRead], r.Set[pricing.TierCacheRead] = 3e-7, true
	r.Base[pricing.TierOutput], r.Set[pricing.TierOutput] = 1.5e-5, true
	return &fixedRates{rates: r, prov: prov}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// One token in each tier at a configured table prices at exactly that tier's rate, and is not
// estimated.
func TestUSD_OneTokenPerTier(t *testing.T) {
	r := testRates(pricing.ProvConfigured)
	for _, c := range []struct {
		tier pricing.Tier
		want float64
	}{
		{pricing.TierInput, 3e-6},
		{pricing.TierCacheWrite, 3.75e-6},
		{pricing.TierCacheRead, 3e-7},
		{pricing.TierOutput, 1.5e-5},
	} {
		usd, est := USD(r, "api.anthropic.com", "claude-sonnet-4-5", 1000, c.tier, 1)
		if !near(usd, c.want) || est {
			t.Errorf("%v: USD = %v, %v; want %v, false", c.tier, usd, est, c.want)
		}
	}
	// Anthropic documents the 1h write as 2x input: not an estimate.
	if usd, est := USD1h(r, "api.anthropic.com", "claude-sonnet-4-5", 1000, 1); !near(usd, 6e-6) || est {
		t.Errorf("USD1h = %v, %v; want 6e-6, false", usd, est)
	}
	if r.endpoint != "api.anthropic.com" || r.model != "claude-sonnet-4-5" || r.promptTotal != 1000 {
		t.Errorf("Resolve asked for %q %q %d, want the request's endpoint, model and prompt total",
			r.endpoint, r.model, r.promptTotal)
	}
}

// A bundled table is a starting point, not a fact about the account: every figure from it is
// estimated. So is a figure with no rate at all, and a 1h write priced at the 5m fallback.
func TestUSD_Estimated(t *testing.T) {
	if usd, est := USD(testRates(pricing.ProvBundled), "h", "claude-sonnet-4-5", 10, pricing.TierInput, 1000); !near(usd, 3e-3) || !est {
		t.Errorf("bundled: USD = %v, %v; want 0.003, true", usd, est)
	}
	if usd, est := USD(testRates(pricing.ProvNone), "h", "m", 10, pricing.TierInput, 1000); usd != 0 || !est {
		t.Errorf("no rate: USD = %v, %v; want 0, true", usd, est)
	}
	// A non-Anthropic model with no explicit 1h rate: the 5m rate, estimated.
	if usd, est := USD1h(testRates(pricing.ProvConfigured), "h", "gpt-5", 10, 1); !near(usd, 3.75e-6) || !est {
		t.Errorf("1h fallback: USD1h = %v, %v; want 3.75e-6, true", usd, est)
	}
	if usd, est := USD(nil, "h", "m", 10, pricing.TierInput, 1000); usd != 0 || !est {
		t.Errorf("nil resolver: USD = %v, %v; want 0, true", usd, est)
	}
	// No tokens, nothing priced: not an estimate of anything.
	if usd, est := USD(testRates(pricing.ProvBundled), "h", "m", 10, pricing.TierInput, 0); usd != 0 || est {
		t.Errorf("zero tokens: USD = %v, %v; want 0, false", usd, est)
	}
}

func TestTierUSD_MapsEveryTier(t *testing.T) {
	r := testRates(pricing.ProvConfigured)
	tt := accountant.TierTokens{Input: 1, CacheWrite5m: 1, CacheWrite1h: 1, CacheRead: 1}
	usd, est := TierUSD(r, "h", "claude-sonnet-4-5", 10, tt)
	if want := 3e-6 + 3.75e-6 + 6e-6 + 3e-7; !near(usd, want) || est {
		t.Errorf("TierUSD = %v, %v; want %v, false", usd, est, want)
	}
	// The 1h tier on a non-Anthropic model falls back to the 5m rate, so the sum is estimated.
	if _, est := TierUSD(r, "h", "gpt-5", 10, accountant.TierTokens{CacheWrite1h: 5}); !est {
		t.Error("a 1h write priced at the 5m fallback is not marked estimated")
	}
	if _, est := TierUSD(r, "h", "gpt-5", 10, accountant.TierTokens{Input: 5}); est {
		t.Error("no 1h tokens, yet the figure is estimated")
	}
}
