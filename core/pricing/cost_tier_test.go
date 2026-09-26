package pricing

import "testing"

// The per-tier split is the arithmetic Cost already does, kept instead of summed away.
//
// The fixture is deliberately cache-heavy so TOKENS and MONEY rank differently:
// cache-read is 94% of the tokens and 35% of the cost, output is 3% of the tokens and
// 52% of the cost. A test whose fixture ranked both the same way would pass against an
// implementation that split by tokens, which is the mistake this feature exists to fix.
func TestCostByTier_SplitsTheTotalItAlreadyComputes(t *testing.T) {
	r := Rates{
		Base: [numTiers]float64{TierInput: 3e-06, TierCacheWrite: 3.75e-06, TierCacheRead: 3e-07, TierOutput: 1.5e-05},
		Set:  [numTiers]bool{TierInput: true, TierCacheWrite: true, TierCacheRead: true, TierOutput: true},
	}
	u := Usage{Input: 1000, CacheWrite: 2000, CacheRead: 100000, Output: 3000}

	tiers, total, ok, refusal := CostByTier(r, u)
	if !ok || refusal != RefusalNone {
		t.Fatalf("refused a priceable request: ok=%v refusal=%v", ok, refusal)
	}
	for tier, want := range map[Tier]int64{
		TierInput:      3000,  // 1000 x 3e-06
		TierCacheWrite: 7500,  // 2000 x 3.75e-06
		TierCacheRead:  30000, // 100000 x 3e-07
		TierOutput:     45000, // 3000 x 1.5e-05
	} {
		if tiers[tier] != want {
			t.Errorf("tier %d = %d micros, want %d", tier, tiers[tier], want)
		}
	}
	// The total is the one CostWithReason reports, because it is the same sum.
	wantTotal, wantOK, _ := CostWithReason(r, u)
	if !wantOK || total != wantTotal {
		t.Errorf("total = %d, want %d — CostByTier and CostWithReason disagree", total, wantTotal)
	}
	// A tier that carried no tokens is zero, and zero here means "no tokens", which is
	// why the caller needs a separate "is there a split at all" signal.
	none, _, _, _ := CostByTier(r, Usage{Output: 3000})
	if none[TierInput] != 0 {
		t.Errorf("input tier = %d for a request with no input tokens, want 0", none[TierInput])
	}
}

// Delegation changed no refusal. CostWithReason keeps its signature and its behaviour;
// this walks every way it can decline so the refactor cannot quietly reorder them.
func TestCostWithReason_RefusalsSurviveTheDelegation(t *testing.T) {
	full := Rates{
		Base: [numTiers]float64{TierInput: 3e-06, TierOutput: 1.5e-05},
		Set:  [numTiers]bool{TierInput: true, TierOutput: true},
	}
	for _, tc := range []struct {
		name string
		r    Rates
		u    Usage
		want Refusal
	}{
		{"no tokens at all", full, Usage{}, RefusalNoTokens},
		{"impossible count", full, Usage{Input: -1}, RefusalImpossibleCount},
		{"a tier with tokens and no rate", full, Usage{Input: 10, CacheRead: 10}, RefusalNoRate},
		{"a negative rate", Rates{
			Base: [numTiers]float64{TierInput: -1},
			Set:  [numTiers]bool{TierInput: true},
		}, Usage{Input: 10}, RefusalNoRate},
		{"priceable", full, Usage{Input: 10}, RefusalNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, got := CostWithReason(tc.r, tc.u); got != tc.want {
				t.Errorf("refusal = %q, want %q", got, tc.want)
			}
		})
	}
}
