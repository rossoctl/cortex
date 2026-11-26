package accounting

import (
	"fmt"
	"math"
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/accountant/test/cases"
	"github.com/rossoctl/cortex/core/config"
)

// This file plays every github.com/rossoctl/accountant/test/cases case through Cortex's OWN
// host-side dollar conversion (SavingsBilled, savings.go) and checks the net it produces against
// the case's expected net — the same agreement check context-guru's
// dash/accountant_agree_test.go (TestProxyPathMatchesTheAccountantCases) runs for the proxy's own
// conversion path. core/accounting/cases_test.go already proves Cortex's call into the library
// (BeginRequest/Add/EndRequest/Compute) agrees with the case's Expected accountant.Result; this
// file proves the conversion ON TOP of that Result — SavingsBilled's split into one event.Saving
// per operator plus Extras.Spends — nets to the same dollar figure the case expects, so a bug in
// that conversion layer (not in the library itself) has a test that would catch it.

// rateOf returns a price function for SavingsBilled that reproduces one case's rate table exactly
// (Rate.Picos, converted from pico-dollars to dollars): every op and cache-effect Saved/Caused is
// priced by the request's own model, which is what the case's StepPicos assumes too (see
// cases.Case.StepRate), so the model and host arguments are ignored here on purpose.
func rateOf(r cases.Rate) func(model, host string, tt accountant.TierTokens) (float64, bool) {
	return func(model, host string, tt accountant.TierTokens) (float64, bool) {
		return float64(r.Picos(tt)) / 1e12, false
	}
}

// spendOf prices a spend op's prompt tiers at the call's own model rate (cases.Case.ModelRate),
// the way cases.Case.StepPicos does (it does not price the call's output there; SavingsBilled's
// spend path does, via spendRecord reading Call.Usage.CostUSD first — the case fixtures always
// set CostUSD on a spend's usage, so spendOf is never asked to price one).
func spendOf(c cases.Case) SpendPrice {
	return func(call *accountant.ModelCall, host string) (float64, bool, string) {
		rate := c.ModelRate(call.Model)
		return float64(rate.CallPicos(call.Usage)) / 1e12, false, ""
	}
}

// microsAgree rounds a dollar figure to micro-dollars, half away from zero, matching
// cases.RoundMicros's own rounding of pico-dollars (half up) closely enough for the tolerance
// agreeDiffs checks with (1 micro-dollar).
func microsAgree(usd float64) int64 { return int64(math.Round(usd * 1e6)) }

func absAgree(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// playedNet is SavingsBilled's net for one step, in micro-dollars: every APPLIED (non-Projected)
// saving's USD minus PenaltyUSD, minus every COUNTED (neither Projected nor Refused) spend's USD,
// minus Extras.UnattributedBreakUSD (a mark or break no applied op owns) — the same exclusions and
// the same extra term event.Event.Figures applies when it nets a request's cost (W7.1), and the
// reason a case's dry-run or refused figures must not silently inflate this comparison.
func playedNet(c cases.Case, s cases.Step, res accountant.Result) int64 {
	savings, extras := SavingsBilled(res, rateOf(c.StepRate(s)), spendOf(c), "", "", accountant.BillingAPI, config.DefaultSubscriptionWeights())
	var net float64
	for _, sv := range savings {
		if sv.Projected {
			continue
		}
		net += sv.USD - sv.PenaltyUSD
	}
	for _, sp := range extras.Spends {
		if sp.Counted() {
			net -= sp.USD
		}
	}
	// A mark premium/payback or break with no applied op owning its ChangeID (e.g. a mark on a
	// session's first request, before any operator has reported a ChangeID at all) is not in any
	// Saving: SavingsBilled puts it in Extras.UnattributedBreakUSD instead (W7.1). It is a net
	// COST (positive = cost, same sign convention as event.Event.UnattributedBreakUSD), so it
	// subtracts from net the same way a Saving's PenaltyUSD does.
	net -= extras.UnattributedBreakUSD
	return microsAgree(net)
}

// expectedNet is the case's own expected net for one step, in micro-dollars: StepPicos rounded
// the way the library's own rate tables are (RoundMicros, half up from pico-dollars).
func expectedNet(c cases.Case, s cases.Step) int64 {
	return cases.RoundMicros(c.StepPicos(s))
}

// TestSavingsAgreesWithTheAccountantCases plays every case through SavingsBilled, none skipped
// (same as TestAccountantCases), and wants its net to match the case's expected net on every
// step, and over the whole case.
func TestSavingsAgreesWithTheAccountantCases(t *testing.T) {
	all := cases.All()
	if len(all) == 0 {
		t.Fatal("no cases: github.com/rossoctl/accountant/test/cases returned none")
	}
	for _, c := range all {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			ts := accountant.NewTrackers()
			var wantTotal, gotTotal int64
			for i, s := range c.Steps {
				if s.IsStandalone() {
					// A keep-alive ping: SavingsBilled takes a request-shaped Result, so a
					// standalone step's net is priced the same way cases.Case.StepPicos prices one
					// (the call's own spend, nothing else) rather than through SavingsBilled.
					var spend int64
					for _, op := range s.Standalone.Ops {
						if op.Kind == accountant.KindSpend && op.Call != nil {
							spend += c.ModelRate(op.Call.Model).CallPicos(op.Call.Usage)
						}
					}
					got := -cases.RoundMicros(spend)
					want := expectedNet(c, s)
					if absAgree(got-want) > 1 {
						t.Errorf("step %d (standalone): net %d µ$, want %d µ$", i, got, want)
					}
					wantTotal += want
					gotTotal += got
					continue
				}
				r := ts.BeginRequest(s.Begin.Meta, s.Begin.Client, s.Begin.ClientBreakpoints)
				for _, op := range s.Add {
					r.Add(op)
				}
				r.EndRequest(s.End.Thread, s.End.Forwarded, s.End.Breakpoints, s.End.Facts.EntryFacts())
				res := r.Compute(s.Usage)
				got := playedNet(c, s, res)
				want := expectedNet(c, s)
				if absAgree(got-want) > 1 {
					t.Errorf("step %d: net %d µ$, want %d µ$", i, got, want)
				}
				wantTotal += want
				gotTotal += got
			}
			// The total sums the per-step nets, each rounded to µ$. playedNet rounds a float
			// dollar figure (math.Round, half away from zero); expectedNet rounds exact
			// pico-dollars (cases.RoundMicros, half up). At a step whose net is an exact half
			// µ$, float error puts the value just below the half, so the two paths can disagree
			// by 1 µ$. Each step is held to 1 µ$ above, so the total can drift by up to one µ$
			// per step. Bound the total by the step count, not by 1.
			if absAgree(gotTotal-wantTotal) > int64(len(c.Steps)) {
				t.Errorf("thread total: net %d µ$, want %d µ$", gotTotal, wantTotal)
			}
		})
	}
}

// TestRateOfMatchesCasesPicos pins rateOf's own conversion against cases.Rate.Picos directly, so
// a future change to either rounding cannot silently drift the comparison above out of agreement
// with itself.
func TestRateOfMatchesCasesPicos(t *testing.T) {
	rate := cases.Rate{Input: 3_000_000, CacheWrite5m: 3_750_000, CacheWrite1h: 6_000_000, CacheRead: 300_000, Output: 15_000_000}
	tt := accountant.TierTokens{Input: 1000, CacheWrite5m: 500, CacheWrite1h: 200, CacheRead: 100}
	price := rateOf(rate)
	usd, estimated := price("any-model", "any-host", tt)
	if estimated {
		t.Error("rateOf reported estimated, want false")
	}
	wantMicros := cases.RoundMicros(rate.Picos(tt))
	gotMicros := microsAgree(usd)
	if gotMicros != wantMicros {
		t.Errorf("rateOf: %d µ$, want %d µ$ (%s)", gotMicros, wantMicros, fmt.Sprintf("picos=%d", rate.Picos(tt)))
	}
}
