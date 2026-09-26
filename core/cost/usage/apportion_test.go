package usage

import (
	"testing"

	"github.com/rossoctl/cortex/core/cost/pricing"
)

// mixed is a Counts whose modelled split is 3000/7500/30000/45000 micros — the shape of a
// cache-heavy agent turn — against whatever authoritative total the caller sets.
func mixed(total int64) Counts {
	return Counts{
		CostMicros:           total,
		InputCostMicros:      3000,
		CacheWriteCostMicros: 7500,
		CacheReadCostMicros:  30000,
		OutputCostMicros:     45000,
	}
}

// The four tiers sum EXACTLY to CostMicros, which is the property the whole display rests
// on: a reader checks the column against the headline above it first.
//
// The totals are chosen so integer division leaves a remainder. Round numbers that happen
// to divide evenly would pass against an implementation with no remainder rule at all,
// which is the one thing this test exists to catch.
func TestApportionTiers_SumsToTheAuthoritativeTotalExactly(t *testing.T) {
	for _, total := range []int64{1, 7, 999, 100003, 1234567, 9999999999} {
		tiers, ok := mixed(total).ApportionTiers()
		if !ok {
			t.Fatalf("total %d: refused a Counts with a mix", total)
		}
		var sum int64
		for i, v := range tiers {
			sum += v
			if v < 0 {
				t.Errorf("total %d: tier %d is negative (%d)", total, i, v)
			}
		}
		if sum != total {
			t.Errorf("total %d: tiers sum to %d, off by %d", total, sum, sum-total)
		}
	}
}

// The mix is the RATIO, so the biggest modelled tier stays the biggest apportioned one.
func TestApportionTiers_PreservesTheRanking(t *testing.T) {
	tiers, ok := mixed(1_000_000).ApportionTiers()
	if !ok {
		t.Fatal("refused a Counts with a mix")
	}
	if tiers[pricing.TierOutput] <= tiers[pricing.TierCacheRead] {
		t.Errorf("output %d did not stay above cache-read %d",
			tiers[pricing.TierOutput], tiers[pricing.TierCacheRead])
	}
	// Output is 45000 of an 85500 mix, so ~52.6% of the total.
	if got := tiers[pricing.TierOutput]; got < 520_000 || got > 530_000 {
		t.Errorf("output apportioned to %d, want ~526000 (45000/85500 of 1000000)", got)
	}
}

// No mix means no answer: not four zeros, which a caller renders as four free tiers, and
// not a divide by zero.
func TestApportionTiers_RefusesWithNoMix(t *testing.T) {
	if _, ok := (Counts{CostMicros: 500_000}).ApportionTiers(); ok {
		t.Error("apportioned a total with no modelled mix at all")
	}
	// A mix with no total is nothing to apportion either.
	if _, ok := (Counts{InputCostMicros: 3000}).ApportionTiers(); ok {
		t.Error("apportioned a zero total")
	}
	// And a negative total is not a total — the same refusal every money surface makes.
	if _, ok := (Counts{CostMicros: -5, InputCostMicros: 3000}).ApportionTiers(); ok {
		t.Error("apportioned a negative total")
	}
}

// A mix covering a tiny fraction of the spend still apportions.
//
// THE POSITIVE CONTROL FOR HAVING NO COVERAGE THRESHOLD. An earlier draft refused below a
// floor; any reintroduced floor blanks this case. The reasoning is that no particular floor
// is defensible — 50% and 10% are equally arbitrary — and the `~` marker already says the
// figure is inexact.
func TestApportionTiers_ASmallMixStillApportions(t *testing.T) {
	c := Counts{CostMicros: 30_935_000, InputCostMicros: 12} // 12 micros of mix, $30.93 of spend
	tiers, ok := c.ApportionTiers()
	if !ok {
		t.Fatal("a small mix was refused — has a coverage floor come back?")
	}
	if tiers[pricing.TierInput] != 30_935_000 {
		t.Errorf("input = %d, want the whole total: it is the only tier in the mix",
			tiers[pricing.TierInput])
	}
}

// A tier absent from the mix stays absent from the answer. Spreading the remainder across
// every tier would invent a cache-write charge for traffic that wrote no cache.
func TestApportionTiers_ATierWithNoMixGetsNothing(t *testing.T) {
	c := Counts{CostMicros: 999_999, InputCostMicros: 7, OutputCostMicros: 23}
	tiers, ok := c.ApportionTiers()
	if !ok {
		t.Fatal("refused a two-tier mix")
	}
	if tiers[pricing.TierCacheRead] != 0 || tiers[pricing.TierCacheWrite] != 0 {
		t.Errorf("a tier absent from the mix was given money: %v", tiers)
	}
}

// ApportionReasoning's four not-known paths, each of which must refuse rather than
// return a zero a caller would render as "$0.00" — the claim that the reasoning was
// free.
func TestApportionReasoning_RefusesRatherThanReturningZero(t *testing.T) {
	base := Counts{
		OutputTokens: 1593, ReasoningTokens: 948,
		PresentKinds: uint8(KindOutput | KindReasoning),
	}
	for _, tc := range []struct {
		name         string
		mutate       func(*Counts)
		outputMicros int64
	}{
		{"nothing reported a split", func(c *Counts) {
			c.ReasoningTokens, c.PresentKinds = 0, uint8(KindOutput)
		}, 1_000_000},
		{"no output tokens, so no denominator", func(c *Counts) { c.OutputTokens = 0 }, 1_000_000},
		{"no output money to take a share of", func(c *Counts) {}, 0},
		// 100 * 1/1000 = 0.1, which truncates away.
		{"share truncates below one micro", func(c *Counts) {
			c.ReasoningTokens, c.OutputTokens = 1, 1000
		}, 100},
		// NEGATIVE, which returned (-595103, true) before the guard: the ratio goes
		// negative, the upper clamp does not fire, and the truncation escape does not
		// either. plausibleTokenReport screens negatives at ingest, but this is exported
		// and addSat's argument in this package applies — "unreachable today" is how the
		// wrap arrived.
		{"negative reasoning count", func(c *Counts) { c.ReasoningTokens = -948 }, 1_000_000},
		{"negative output count", func(c *Counts) { c.OutputTokens = -1593 }, 1_000_000},
		{"negative output money", func(c *Counts) {}, -1_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			tc.mutate(&c)
			got, ok := c.ApportionReasoning(tc.outputMicros)
			if ok {
				t.Errorf("ok = true with %d micros; the caller would render a figure it cannot defend", got)
			}
			if got != 0 {
				t.Errorf("micros = %d, want 0 alongside ok=false", got)
			}
			if got < 0 {
				t.Errorf("micros = %d is NEGATIVE; a caller would publish negative money "+
					"or hand it to tierBar", got)
			}
		})
	}
}

// A REPORTED ZERO is still a measurement, but it apportions to nothing, so it refuses
// too — the figure is what cannot be stated, not the observation.
func TestApportionReasoning_ReportedZeroApportionsToNothing(t *testing.T) {
	c := Counts{
		OutputTokens: 1593, ReasoningTokens: 0,
		PresentKinds: uint8(KindOutput | KindReasoning),
	}
	if got, ok := c.ApportionReasoning(1_000_000); ok {
		t.Errorf("ok = true with %d micros for a reported zero", got)
	}
}

// A non-zero value with the bit CLEAR still apportions: that is an event from a
// producer predating PresentKinds, where the value is the only evidence there is.
func TestApportionReasoning_LegacyProducerStillApportions(t *testing.T) {
	c := Counts{OutputTokens: 1593, ReasoningTokens: 948} // no PresentKinds
	got, ok := c.ApportionReasoning(1_000_000)
	if !ok {
		t.Fatal("refused a producer predating PresentKinds, dropping the only evidence available")
	}
	if want := int64(594_000); got < want-2_000 || got > want+2_000 {
		t.Errorf("micros = %d, want about %d (948/1593 of the parent)", got, want)
	}
}

// CLAMPED TO THE PARENT. A provider reporting reasoning above output must not yield a
// figure above the one it is a share of; the counts themselves are left as reported.
func TestApportionReasoning_ClampsToTheParent(t *testing.T) {
	c := Counts{
		OutputTokens: 1593, ReasoningTokens: 4_000, // impossible on the wire
		PresentKinds: uint8(KindOutput | KindReasoning),
	}
	got, ok := c.ApportionReasoning(1_000_000)
	if !ok {
		t.Fatal("refused a clampable figure")
	}
	if got != 1_000_000 {
		t.Errorf("micros = %d, want it clamped to the parent's 1000000", got)
	}
}
