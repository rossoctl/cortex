package costevent

import (
	"fmt"
	"math"
	"testing"

	"github.com/rossoctl/cortex/core/pricing"
)

// THE CROSS-PRODUCT, ENUMERATED, which is what rossoctl/cortex#1030 was filed about.
//
// Seven rounds of review each added a field or a string for a case where the existing labels lied,
// and every one was tested on its own. What nothing tested was the COMBINATIONS: a refusal beside a
// figure, an Incomplete with no reason, a settled zero that is also unrepresentable. Those are
// reachable — a producer sets two fields and forgets a third — and each one has exactly one honest
// answer, which nothing wrote down.
//
// So this walks the product of the fields that decide trust, rather than a list of cases someone
// thought of: settled × figure × refusal × incompleteness × reason. 2 × 6 × 3 × 2 × 4 = 288
// records, each checked against the rule below and against the invariants that hold for all of
// them.
//
// THE RULE IS RESTATED HERE, NOT IMPORTED, which is the one duplication this file wants: a test
// that calls Trust to decide what Trust should return asserts nothing. The table's version is
// written from the DOCUMENTED semantics — most-damning first — so a change to either has to be
// made deliberately in both.

// trustFields are the axes. Named values rather than raw literals so a failure says which case
// broke, and so adding an axis value is a one-line change that expands the product.
var (
	settledValues = []struct {
		name    string
		settled bool
	}{{"settled", true}, {"unsettled", false}}

	figureValues = []struct {
		name string
		usd  float64
	}{
		{"zero", 0},
		{"an ordinary figure", 0.25},
		{"a negative figure", -0.25},
		{"past the micros unit", 1e10},
		{"not a number", math.NaN()},
		{"an infinity", math.Inf(1)},
	}

	refusalValues = []struct {
		name   string
		reason string
	}{
		{"not refused", ""},
		{"refused as implausible", RejectedImplausible},
		{"refused as an impossible report", RejectedImplausibleUsage},
	}

	incompleteValues = []struct {
		name       string
		incomplete bool
	}{{"complete", false}, {"incomplete", true}}

	reasonValues = []struct {
		name   string
		reason string
	}{
		{"no reason", ""},
		{"output uncounted", pricing.ReasonOutputUncounted},
		{"split unreported", pricing.ReasonSplitUnreported},
		{"counters below total", pricing.ReasonCountersBelowTotal},
	}
)

// wantTrust is the documented rule, written out independently of the implementation.
func wantTrust(e Event) Trust {
	switch {
	case e.RejectedReason != "":
		return TrustRefused
	case e.CostUSD < 0 || math.IsNaN(e.CostUSD) || math.IsInf(e.CostUSD, 0):
		return TrustUnpriced
	case e.CostUSD*1e6 >= float64(pricing.MaxCostMicros):
		return TrustUnpriced
	case !e.Settled && e.CostUSD == 0:
		return TrustUnpriced
	case !e.Incomplete:
		return TrustExact
	case e.IncompleteReason == pricing.ReasonSplitUnreported:
		return TrustApproximate
	default:
		return TrustFloor
	}
}

func TestTrust_CoversEveryCombination(t *testing.T) {
	var checked int
	for _, s := range settledValues {
		for _, f := range figureValues {
			for _, rj := range refusalValues {
				for _, ic := range incompleteValues {
					for _, rs := range reasonValues {
						ev := Event{
							CostUSD:          f.usd,
							Settled:          s.settled,
							RejectedReason:   rj.reason,
							Incomplete:       ic.incomplete,
							IncompleteReason: rs.reason,
						}
						name := fmt.Sprintf("%s/%s/%s/%s/%s", s.name, f.name, rj.name, ic.name, rs.name)
						checked++
						if got, want := ev.Trust(), wantTrust(ev); got != want {
							t.Errorf("%s: Trust() = %q, want %q", name, got, want)
						}
						// THE INVARIANT EVERY CONSUMER DEPENDS ON. Priced is now defined as
						// Trust().Spendable(), so this asserts they cannot drift — and it is the
						// assertion that would have caught the producer/consumer disagreement
						// that reached the budget in an earlier round.
						if ev.Priced() != ev.Trust().Spendable() {
							t.Errorf("%s: Priced() = %v but Trust() = %q (spendable %v)",
								name, ev.Priced(), ev.Trust(), ev.Trust().Spendable())
						}
						// MONEY AND VERDICT MUST AGREE. A record nothing may spend must convert
						// to zero micros, or a consumer that adds Micros() while counting the
						// request as uncovered puts money in a total it also calls empty.
						if !ev.Trust().Spendable() && ev.Micros() != 0 {
							t.Errorf("%s: Trust() = %q but Micros() = %d, want 0",
								name, ev.Trust(), ev.Micros())
						}
						// AND THE REASON MUST MATCH THE VERDICT, so no consumer renders an
						// incompleteness on a refused record or the reverse.
						switch ev.Trust() {
						case TrustRefused:
							if ev.TrustReason() != ev.RejectedReason {
								t.Errorf("%s: TrustReason() = %q, want the refusal %q", name, ev.TrustReason(), ev.RejectedReason)
							}
						case TrustFloor, TrustApproximate:
							if ev.TrustReason() != ev.IncompleteReason {
								t.Errorf("%s: TrustReason() = %q, want the incompleteness %q", name, ev.TrustReason(), ev.IncompleteReason)
							}
						default:
							if ev.TrustReason() != "" {
								t.Errorf("%s: TrustReason() = %q on a %q record, want empty", name, ev.TrustReason(), ev.Trust())
							}
						}
					}
				}
			}
		}
	}
	// A PRODUCT THAT SHRANK IS A COVERAGE LOSS NOBODY WOULD SEE, since a table test over zero
	// combinations passes. 2 × 6 × 3 × 2 × 4.
	if want := 2 * 6 * 3 * 2 * 4; checked != want {
		t.Errorf("checked %d combinations, want %d: an axis lost its values", checked, want)
	}
}

// TestTrust_TheDamningOrderIsDeliberate pins the four precedence decisions in the rule, because
// each one is a case where two labels are set at once and only one answer is honest. A change here
// is a change to what a consumer is told about money, not a refactor.
func TestTrust_TheDamningOrderIsDeliberate(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   Event
		want Trust
		why  string
	}{{
		name: "a refusal beside a figure",
		ev:   Event{CostUSD: 0.25, Settled: true, RejectedReason: RejectedImplausible},
		want: TrustRefused,
		why:  "the figure was declined; believing it because a number survived is how a forged charge gets spent",
	}, {
		name: "a refusal beside an incompleteness",
		ev:   Event{Settled: true, RejectedReason: RejectedImplausibleUsage, Incomplete: true, IncompleteReason: pricing.ReasonOutputUncounted},
		want: TrustRefused,
		why:  "there is nothing to qualify — a floor implies a figure to stand on",
	}, {
		name: "an unrepresentable figure the producer settled",
		ev:   Event{CostUSD: 1e10, Settled: true},
		want: TrustUnpriced,
		why:  "the micros unit cannot hold it, so every consumer's total would read zero for it anyway",
	}, {
		name: "incomplete with no reason given",
		ev:   Event{CostUSD: 0.25, Settled: true, Incomplete: true},
		want: TrustFloor,
		why:  "a missing reason must not upgrade the claim to merely-approximate",
	}, {
		name: "a settled zero is a real answer",
		ev:   Event{CostUSD: 0, Settled: true},
		want: TrustExact,
		why:  "the gateway said this call was free; unpriced would hide a fact it stated",
	}, {
		name: "an unsettled zero is not",
		ev:   Event{CostUSD: 0},
		want: TrustUnpriced,
		why:  "nobody priced it, and $0.00 would report unpriced traffic as free",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ev.Trust(); got != tc.want {
				t.Errorf("Trust() = %q, want %q — %s", got, tc.want, tc.why)
			}
		})
	}
}
