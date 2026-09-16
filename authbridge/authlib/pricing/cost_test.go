package pricing

import (
	"math"
	"testing"
)

// TestMicrosFromUSD_RejectsANegativeFigureHoweverSmall is the boundary the sign check
// exists to hold, walked from both sides of zero.
//
// The conversion used to check the sign of the ROUNDED figure, and the sign does not
// survive the rounding: math.Round(-1e-07 * 1e6) is math.Round(-0.1), which is negative
// zero, and `-0.0 < 0` is false in Go. Every figure in (-5e-07, 0) therefore came back
// (0, true) — a priced zero standing in for a wrong-signed figure, which is the one class
// of arithmetic error this package cannot detect after the fact: a total that says "free"
// is indistinguishable from a call that was free.
//
// -5e-07 is the boundary and it was never the bug: it rounds to -1, so the old check
// caught it. Both sides are asserted because a fix that rejected the whole neighbourhood
// of zero would take a genuine free call with it.
func TestMicrosFromUSD_RejectsANegativeFigureHoweverSmall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		usd     float64
		wantOK  bool
		wantMic int64
	}{
		// The hole. Under the old rounded-sign check every one of these was (0, true).
		{"one tenth of a micro negative", -1e-07, false, 0},
		{"four tenths of a micro negative", -4e-07, false, 0},
		{"the largest negative that still rounds to zero", -4.999e-07, false, 0},
		// The boundary, from the side the old check already caught: math.Round rounds
		// half AWAY from zero, so -5e-07 becomes -1 rather than -0.
		{"exactly half a micro negative", -5e-07, false, 0},
		{"a milli-dollar negative", -0.001, false, 0},
		// Zero and above must stay priced. -0.0 is a value of ZERO, not a negative
		// figure: a settled zero is a producer saying the call was free, and refusing it
		// would move a genuine free call into the coverage gap.
		{"negative zero is zero", math.Copysign(0, -1), true, 0},
		{"positive zero", 0, true, 0},
		{"a tiny positive rounds to a priced zero", 1e-07, true, 0},
		{"exactly half a micro positive rounds up", 5e-07, true, 1},
		{"a cent", 0.01, true, 10_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			micros, ok := MicrosFromUSD(tc.usd)
			if ok != tc.wantOK {
				t.Errorf("MicrosFromUSD(%v) ok = %v, want %v — the doc promises false for a negative figure, and a tiny one is still negative",
					tc.usd, ok, tc.wantOK)
			}
			if micros != tc.wantMic {
				t.Errorf("MicrosFromUSD(%v) = %d micros, want %d", tc.usd, micros, tc.wantMic)
			}
		})
	}
}

// TestMicrosFromUSD_RoundedSignIsNotLoadBearing states the reason the check moved, so a
// future edit that folds it back onto the rounded value fails with the explanation.
//
// Asserts the language fact directly: rounding a tiny negative yields a value whose SIGN
// BIT is set but which compares equal to zero. Any guard reading that value's sign with
// `< 0` cannot see the input's sign.
func TestMicrosFromUSD_RoundedSignIsNotLoadBearing(t *testing.T) {
	rounded := math.Round(-1e-07 * 1e6)
	if !math.Signbit(rounded) {
		t.Fatal("fixture no longer produces negative zero; the rest of this test proves nothing")
	}
	if rounded < 0 {
		t.Fatal("negative zero compared less than zero; the premise of this test is gone")
	}
	if _, ok := MicrosFromUSD(-1e-07); ok {
		t.Error("MicrosFromUSD(-1e-07) reported a usable figure: the sign must be checked on the INPUT, because math.Round(-0.1) is negative zero and `-0.0 < 0` is false")
	}
}

// claudeOpus is a fully-priced entry in the published unit.
func claudeOpus() Rates {
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

func TestCost_AllFourTiers(t *testing.T) {
	// 1000*3.80 + 2000*4.75 + 10000*0.38 + 500*19.00, per million
	//   = 3800 + 9500 + 3800 + 9500 micros
	u := Usage{Input: 1000, CacheWrite: 2000, CacheRead: 10_000, Output: 500}
	micros, ok := Cost(claudeOpus(), u)
	if !ok {
		t.Fatal("Cost reported unpriced for a fully-priced model")
	}
	if want := int64(26_600); micros != want {
		t.Errorf("Cost = %d micros, want %d", micros, want)
	}
}

// TestCost_PerTierInvariant is the regression this function exists to hold. A
// model priced only for cache reads must not price a cache-write request at zero.
func TestCost_PerTierInvariant(t *testing.T) {
	cacheReadOnly := Rates{
		Base: [numTiers]float64{TierCacheRead: 0.38 / perMillion},
		Set:  [numTiers]bool{TierCacheRead: true},
	}

	if _, ok := Cost(cacheReadOnly, Usage{CacheWrite: 5000}); ok {
		t.Error("a cache-write request priced against a cache-read-only model reported priced")
	}
	// The mirror: the tier it does price still prices.
	micros, ok := Cost(cacheReadOnly, Usage{CacheRead: 10_000})
	if !ok {
		t.Fatal("a cache-read request against a cache-read rate reported unpriced")
	}
	if want := int64(3_800); micros != want {
		t.Errorf("Cost = %d micros, want %d", micros, want)
	}
}

func TestCost_UnsetTierWithNoTokensStillPrices(t *testing.T) {
	// Absent output rate, zero output tokens. The invariant is about tiers that
	// CARRIED tokens; refusing here would unprice every request on a prompt-only
	// rate table (which is exactly what toolprune ships, having no output rate).
	noOutput := Rates{
		Base: [numTiers]float64{TierInput: 3.80 / perMillion},
		Set:  [numTiers]bool{TierInput: true},
	}
	micros, ok := Cost(noOutput, Usage{Input: 1000})
	if !ok {
		t.Fatal("reported unpriced when the only unset tier carried no tokens")
	}
	if want := int64(3_800); micros != want {
		t.Errorf("Cost = %d micros, want %d", micros, want)
	}
}

func TestCost_ZeroUsageIsUnpriced(t *testing.T) {
	// No tokens reported at all is unknown usage, not a free request. Calling it
	// "priced $0" would put it in the priced denominator and dilute coverage.
	if _, ok := Cost(claudeOpus(), Usage{}); ok {
		t.Error("empty usage reported priced")
	}
}

func TestCost_NegativeTokensRefused(t *testing.T) {
	// A negative count is a parser bug or a hostile body. Refuse rather than emit
	// a negative cost, which would corrode a running total that nothing re-derives.
	if _, ok := Cost(claudeOpus(), Usage{Input: -1, Output: 100}); ok {
		t.Error("negative token count reported priced")
	}
}

func TestCost_UnpricedModelIsUnpriced(t *testing.T) {
	if _, ok := Cost(Rates{}, Usage{Input: 1000}); ok {
		t.Error("empty Rates reported priced")
	}
}

func TestCost_AppliesContextThreshold(t *testing.T) {
	r := claudeOpus()
	r.Thresholds = []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 7.60 / perMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}

	// 300k prompt tokens, all uncached: 300000 * 7.60/1e6 = 2.28 USD.
	micros, ok := Cost(r, Usage{Input: 300_000})
	if !ok {
		t.Fatal("long-context request reported unpriced")
	}
	if want := int64(2_280_000); micros != want {
		t.Errorf("Cost above threshold = %d micros, want %d", micros, want)
	}

	// And below it, the base rate: 100000 * 3.80/1e6 = 0.38 USD.
	micros, ok = Cost(r, Usage{Input: 100_000})
	if !ok {
		t.Fatal("short request reported unpriced")
	}
	if want := int64(380_000); micros != want {
		t.Errorf("Cost below threshold = %d micros, want %d", micros, want)
	}
}

func TestCost_ThresholdMeasuredOnPromptNotOutput(t *testing.T) {
	r := claudeOpus()
	r.Thresholds = []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 7.60 / perMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}
	// The output count must exceed the threshold ON ITS OWN while the prompt stays
	// under it, or the assertion holds for either basis and proves nothing. The
	// earlier version used Output: 1, summing to 100,001 — under 200,000 whichever
	// way it was measured, so mutating the basis to PromptTotal()+Output left the
	// whole module green.
	u := Usage{Input: 100_000, Output: 500_000}
	if u.PromptTotal() >= 200_000 {
		t.Fatalf("fixture broken: prompt %d already exceeds the threshold", u.PromptTotal())
	}
	if u.PromptTotal()+u.Output <= 200_000 {
		t.Fatalf("fixture broken: prompt+output %d does not exceed the threshold, so this cannot detect the wrong basis",
			u.PromptTotal()+u.Output)
	}

	micros, ok := Cost(r, u)
	if !ok {
		t.Fatal("reported unpriced")
	}
	// Base input rate, not the premium: 100000*3.80 + 500000*19.00, per million.
	if want := int64(380_000 + 9_500_000); micros != want {
		t.Errorf("Cost = %d micros, want %d — a threshold measured on prompt+output would price input at 7.60/Mtok",
			micros, want)
	}
}

// TestCost_PerTierInvariantCoversEveryTier extends the invariant beyond the one
// tier it was originally tested on.
//
// Output matters most: tool-prune ships no output rate at all, and abctl's
// promptCost zeroes u.Output specifically to avoid tripping this rule — so the rule
// firing correctly for output is what makes that workaround necessary and correct.
func TestCost_PerTierInvariantCoversEveryTier(t *testing.T) {
	full := claudeOpus()
	for _, tc := range []struct {
		name    string
		missing Tier
		usage   Usage
	}{
		{"input", TierInput, Usage{Input: 1000}},
		{"cache write", TierCacheWrite, Usage{CacheWrite: 1000}},
		{"cache read", TierCacheRead, Usage{CacheRead: 1000}},
		{"output", TierOutput, Usage{Output: 1000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := full
			r.Base[tc.missing], r.Set[tc.missing] = 0, false

			if _, ok := Cost(r, tc.usage); ok {
				t.Errorf("a request carrying %s tokens priced against a table with no %s rate reported PRICED",
					tc.name, tc.name)
			}
			// And the same table prices a request that avoids that tier.
			other := Usage{Input: 1000}
			if tc.missing == TierInput {
				other = Usage{Output: 1000}
			}
			if _, ok := Cost(r, other); !ok {
				t.Errorf("removing the %s rate unpriced a request that does not use it", tc.name)
			}
		})
	}
}
