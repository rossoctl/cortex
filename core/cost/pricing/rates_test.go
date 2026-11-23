package pricing

import (
	"fmt"
	"testing"
)

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

// TestRates_CacheWrite1hFallback: an Anthropic-shaped table derives the 1h rate as
// 2x input, Anthropic's documented price, and does not mark it estimated. Any other
// table falls back to the 5m write rate and marks it estimated.
func TestRates_CacheWrite1hFallback(t *testing.T) {
	r := base() // Sonnet-shaped: input 3.80/Mtok, cache write (5m) 4.75/Mtok

	rate, ok, estimated := r.CacheWrite1hRate(true)
	if !ok || estimated {
		t.Fatalf("anthropicShaped: ok=%v estimated=%v, want ok=true estimated=false", ok, estimated)
	}
	if want := 3.80 / perMillion * 2.0; rate != want {
		t.Errorf("anthropicShaped rate = %v, want input x 2.0 = %v", rate, want)
	}

	rate, ok, estimated = r.CacheWrite1hRate(false)
	if !ok || !estimated {
		t.Fatalf("non-Anthropic: ok=%v estimated=%v, want ok=true estimated=true", ok, estimated)
	}
	if want := 4.75 / perMillion; rate != want {
		t.Errorf("non-Anthropic rate = %v, want the 5m write rate %v", rate, want)
	}
}

// TestRates_CacheWrite1hFallback_RejectsAShapeMismatch: anthropicShaped (the model
// name) is not enough on its own — a table whose 5m write rate is not Anthropic's
// 1.25x-of-input ratio falls back to the 5m rate and is marked estimated, the same
// as a non-Anthropic model.
func TestRates_CacheWrite1hFallback_RejectsAShapeMismatch(t *testing.T) {
	flat := Rates{
		Base: [numTiers]float64{TierInput: 2.0 / perMillion, TierCacheWrite: 2.0 / perMillion},
		Set:  [numTiers]bool{TierInput: true, TierCacheWrite: true},
	}
	rate, ok, estimated := flat.CacheWrite1hRate(true)
	if !ok || !estimated {
		t.Fatalf("flat table, anthropicShaped=true: ok=%v estimated=%v, want ok=true estimated=true", ok, estimated)
	}
	if want := 2.0 / perMillion; rate != want {
		t.Errorf("rate = %v, want the 5m write rate %v, not 2x input", rate, want)
	}
}

// TestIsAnthropicCacheWriteShape_SurvivesTheFloatDivision: a per-million config
// value divides through tokensPerMillion at runtime (config.go's ratesOf, the
// f.million/tokensPerMillion line), and the review that found this fixture found
// that division can land one ULP below the window's 1.20 lower edge for some
// decimal inputs, even though the resulting rate is numerically Haiku 3's own
// 1.2x-of-input ratio. 0.10/0.12 per million is the review's own failing case
// before the epsilon; 0.25/0.30 (Haiku 3's actual bundled rate) already landed on
// the edge, which is why TestCacheWrite1hRate_EveryBundledClaudeRowGetsTheDerivation
// alone did not catch this.
func TestIsAnthropicCacheWriteShape_SurvivesTheFloatDivision(t *testing.T) {
	for _, permillion := range [][2]float64{
		{0.10, 0.12}, {0.40, 0.48}, {0.80, 0.96}, // one ULP low before the epsilon
		{0.25, 0.30}, {0.30, 0.36}, {0.70, 0.84}, {2.5, 3.0}, // exactly on the edge
	} {
		in := permillion[0] / tokensPerMillion
		cw := permillion[1] / tokensPerMillion
		if !isAnthropicCacheWriteShape(in, cw) {
			t.Errorf("isAnthropicCacheWriteShape(%v/Mtok, %v/Mtok) = false, want true — "+
				"this is Haiku 3's 1.2x-of-input ratio, typed as a per-million config value",
				permillion[0], permillion[1])
		}
	}
}

// TestCacheWrite1hRate_EveryBundledClaudeRowGetsTheDerivation guards the shape
// window itself: every bundled Claude row must keep Anthropic's documented
// 2x-input derivation, at its base rate and at every threshold's "above" rate.
// Without this, a vendor row the window excludes (Claude 3 Haiku's cache-write
// ratio is exactly 1.2, the window's lower edge) silently loses the derivation
// the next time either the table or the window changes, rather than failing here.
func TestCacheWrite1hRate_EveryBundledClaudeRowGetsTheDerivation(t *testing.T) {
	for _, e := range Bundled() {
		if !AnthropicShaped(e.Model) {
			continue
		}
		check := func(name string, flat Rates) {
			in, inSet := flat.For(TierInput)
			cw, cwSet := flat.For(TierCacheWrite)
			if !inSet || !cwSet || in <= 0 {
				return // nothing to derive from; CacheWrite1hRate's own ok=false covers it
			}
			rate, ok, estimated := flat.CacheWrite1hRate(true)
			if !ok || estimated || rate != in*2.0 {
				t.Errorf("%s %s: rate=%v ok=%v estimated=%v (input=%v cache-write=%v, ratio=%v), "+
					"want 2x input/true/false",
					e.Model, name, rate, ok, estimated, in, cw, cw/in)
			}
		}
		check("base", e.Rates.At(0))
		for _, th := range e.Rates.Thresholds {
			check(fmt.Sprintf("above %d", th.AbovePromptTokens), e.Rates.At(th.AbovePromptTokens+1))
		}
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
