package pricing

import "testing"

// perMillion is the production constant under a shorter name, so tests read in the
// unit providers publish. Divided by a CONSTANT so the compiler folds it exactly —
// a runtime division lands a ulp low and would make expected values disagree in
// the last digit for no reason (the rule toolprune/pricing.go:45-47 states).
const perMillion = tokensPerMillion

func TestUsagePromptTotal(t *testing.T) {
	u := Usage{Input: 10, CacheWrite: 20, CacheRead: 30, Output: 40}
	// Output is NOT prompt-side: a context threshold is measured against the
	// prompt, so folding output in would trip the premium early.
	if got, want := u.PromptTotal(), 60; got != want {
		t.Fatalf("PromptTotal() = %d, want %d", got, want)
	}
}

func TestUsageTokensIndexedByTier(t *testing.T) {
	u := Usage{Input: 1, CacheWrite: 2, CacheRead: 3, Output: 4}
	got := u.tokens()
	for tier, want := range map[Tier]int{
		TierInput: 1, TierCacheWrite: 2, TierCacheRead: 3, TierOutput: 4,
	} {
		if got[tier] != want {
			t.Errorf("tokens()[%d] = %d, want %d", tier, got[tier], want)
		}
	}
}

// base is a fully-priced Rates in the shape a real Claude entry has.
func base() Rates {
	return Rates{
		Base: [numTiers]float64{
			TierInput:      3.80 / perMillion,
			TierCacheWrite: 4.75 / perMillion,
			TierCacheRead:  0.38 / perMillion,
			TierOutput:     19.00 / perMillion,
		},
		Set: [numTiers]bool{TierInput: true, TierCacheWrite: true, TierCacheRead: true, TierOutput: true},
	}
}

func TestRatesAt_ThresholdIsStrictlyExceeded(t *testing.T) {
	r := base()
	r.Thresholds = []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 7.60 / perMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}

	// Exactly at the boundary pays the base rate: "$X above 200k tokens" means
	// the premium starts at token 200,001. The spec calls this boundary out for
	// direct cover because off-by-one here misprices every long session.
	if got, want := r.At(200_000).Base[TierInput], 3.80/perMillion; got != want {
		t.Errorf("At(200000) input = %v, want %v", got, want)
	}
	if got, want := r.At(200_001).Base[TierInput], 7.60/perMillion; got != want {
		t.Errorf("At(200001) input = %v, want %v", got, want)
	}
}

func TestRatesAt_UnsetThresholdTierKeepsBase(t *testing.T) {
	r := base()
	r.Thresholds = []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 7.60 / perMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}
	eff := r.At(300_000)
	// A table that prices only long-context input must not silently unprice
	// output — that would flip a whole request to unpriced via Cost's invariant.
	if !eff.Set[TierOutput] {
		t.Fatal("output tier lost its rate when a threshold overrode only input")
	}
	if got, want := eff.Base[TierOutput], 19.00/perMillion; got != want {
		t.Errorf("At(300000) output = %v, want %v", got, want)
	}
}

func TestRatesAt_HighestExceededThresholdWins_AnyOrder(t *testing.T) {
	r := base()
	// Deliberately ascending: At must not depend on slice order, so a hand-built
	// Rates in a test or a hand-written config cannot pick the wrong tier.
	r.Thresholds = []ContextThreshold{
		{AbovePromptTokens: 200_000, Rate: [numTiers]float64{TierInput: 7.60 / perMillion}, Set: [numTiers]bool{TierInput: true}},
		{AbovePromptTokens: 500_000, Rate: [numTiers]float64{TierInput: 11.40 / perMillion}, Set: [numTiers]bool{TierInput: true}},
	}
	if got, want := r.At(600_000).Base[TierInput], 11.40/perMillion; got != want {
		t.Errorf("At(600000) input = %v, want %v", got, want)
	}
	if got, want := r.At(300_000).Base[TierInput], 7.60/perMillion; got != want {
		t.Errorf("At(300000) input = %v, want %v", got, want)
	}
}

func TestRatesAt_IsIdempotent(t *testing.T) {
	r := base()
	r.Thresholds = []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 7.60 / perMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}
	// Resolve returns already-flattened rates (Task 1.3), and Cost calls At again.
	// Flattening twice must be a no-op or the premium would be applied to a Rates
	// that no longer carries its thresholds and quietly fall back to base.
	once := r.At(300_000)
	twice := once.At(300_000)
	// Rates holds a slice, so it is not comparable with ==; compare the two
	// comparable halves and assert the slice is empty separately.
	if twice.Base != once.Base || twice.Set != once.Set {
		t.Errorf("At not idempotent: %+v then %+v", once, twice)
	}
	if len(once.Thresholds) != 0 {
		t.Error("At returned rates that still carry thresholds")
	}
}

func TestRatesAny(t *testing.T) {
	if (Rates{}).any() {
		t.Error("empty Rates reported a rate")
	}
	if !base().any() {
		t.Error("fully-priced Rates reported no rate")
	}
	// Threshold-only is still a rate: NewTable (Task 1.3) uses any() to reject
	// rows that price nothing, and a long-context-only row prices something.
	thresholdOnly := Rates{Thresholds: []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 7.60 / perMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}}
	if !thresholdOnly.any() {
		t.Error("threshold-only Rates reported no rate")
	}
}

func TestRatesFor_PerTierAvailability(t *testing.T) {
	// The accessor form of the invariant: a model priced only for cache reads must
	// report "no rate" for a cache write, not the zero value.
	cacheReadOnly := Rates{
		Base: [numTiers]float64{TierCacheRead: 0.38 / perMillion},
		Set:  [numTiers]bool{TierCacheRead: true},
	}
	if v, ok := cacheReadOnly.For(TierCacheRead); !ok || v != 0.38/perMillion {
		t.Errorf("For(TierCacheRead) = %v, %v; want %v, true", v, ok, 0.38/perMillion)
	}
	if v, ok := cacheReadOnly.For(TierCacheWrite); ok || v != 0 {
		t.Errorf("For(TierCacheWrite) = %v, %v; want 0, false", v, ok)
	}
}

func TestRatesFor_OutOfRangeTier(t *testing.T) {
	if v, ok := base().For(Tier(99)); ok || v != 0 {
		t.Errorf("For(99) = %v, %v; want 0, false", v, ok)
	}
	if v, ok := base().For(Tier(-1)); ok || v != 0 {
		t.Errorf("For(-1) = %v, %v; want 0, false", v, ok)
	}
}

func TestRatesFor_ReadsFlattenedTierAfterAt(t *testing.T) {
	r := base()
	r.Thresholds = []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 7.60 / perMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}
	if v, _ := r.At(300_000).For(TierInput); v != 7.60/perMillion {
		t.Errorf("For after At(300000) = %v, want %v", v, 7.60/perMillion)
	}
}
