package settle

import (
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// TestSettle_PricedAgreesWithTheRecordItProduces is the second must-fix from review round 6.
//
// THE PRODUCER AND THE CONSUMER ANSWER THE SAME QUESTION IN TWO PLACES. Settle sets
// Settled.Priced; event.Event.Priced() decides whether that record reads as spend. They
// are meant to be the same predicate — cost/event's own doctrine is that Micros and Priced must
// agree, or a consumer adds money to a total while counting the request as uncovered — but
// nothing made them agree, and they were validated against different bounds: the header arm
// accepted any finite non-negative float, while Priced() rejects anything the micros unit
// cannot hold. They disagreed over ($9.007 billion, +Inf), and on a PARSED endpoint the
// plausibility cap bails out before it could narrow that.
//
// BOTH HALVES OF THE DISAGREEMENT ARE REACHABLE, and they are reachable at once. Something
// gating on Settled.Priced charges the figure — litellm_budgettrack accumulates it into
// TotalSpend, which drives the HTTP 429 lockout — while everything reading the record through
// cost/event files the same request as UNPRICED. Two headers of ~1.8e308 sum to +Inf, and the
// ledger's json.Marshal cannot write that at all: one response poisons the file.
//
// SO THE TEST IS THE AGREEMENT ITSELF, not a state per row. Asserting "1e10 is refused" would
// pin today's answer; asserting "the two predicates agree on every row" pins the invariant, and
// stays honest if a later change moves the bound. The rows exist to make the assertion reach
// the disputed range, from a plausible figure to the largest float there is.
func TestSettle_PricedAgreesWithTheRecordItProduces(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		parsed bool
	}{
		{"an ordinary figure", "0.25", true},
		{"a declared-free zero", "0", true},
		{"a negative figure", "-1", true},
		{"an unparseable string", "not-a-number", true},
		{"past the plausibility cap, unparsed", "1e9", false},
		{"past the plausibility cap, parsed", "1e9", true},
		// THE DISPUTED RANGE. Over the micros bound and under +Inf: finite, non-negative,
		// and on a parsed path the plausibility cap returns early without looking.
		{"past the micros bound, parsed", "1e10", true},
		{"the largest float there is, parsed", "1.7976931348623157e308", true},
		{"past the micros bound, unparsed", "1e10", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pctx := unparsedCostCtx(tc.header)
			if tc.parsed {
				pctx.Extensions.Inference = &pipeline.InferenceExtension{
					Model:        "claude-opus-5",
					InputTokens:  1000,
					OutputTokens: 500,
				}
			}

			got := Settle(pctx, rates(t))
			record := NewRecord(got, nil)

			if got.Priced != record.Priced() {
				t.Errorf("Settled.Priced = %v but the record it produced reads Priced() = %v (CostUSD %v): a consumer gating on one of these charges a figure the other files as unpriced",
					got.Priced, record.Priced(), got.CostUSD)
			}
			// AND THE FIGURE ITSELF, because agreeing on the verdict is not enough: a record
			// that says priced must carry a figure the unit can hold, or Micros zeroes it and
			// the two disagree one level down instead.
			if record.Priced() {
				if _, ok := pricing.MicrosFromUSD(record.CostUSD); !ok {
					t.Errorf("the record is priced at %v, which pricing.MicrosFromUSD cannot represent: Micros() returns 0 for it, so every micros consumer reads this charge as free",
						record.CostUSD)
				}
			}
		})
	}
}

// TestHeaderCost_PastTheMicrosBoundIsUnusable pins the state the fix chose, and why that one.
//
// UNUSABLE, NOT IMPLAUSIBLE. The two refusals are different claims. headerImplausible means a
// figure was on the wire, this process understood it, and declined to believe it — a fact an
// operator needs, so it is DISCLOSED on the record as event.RejectedImplausible. A figure
// past the micros bound is a different animal: it is not a price this system can represent at
// all, which puts it in the same class as a NaN or a garbage string, and those are noise. The
// header is unauthenticated, so treating unrepresentable input as noise rather than as a
// reportable event also keeps a hostile host from writing rows into an operator's disclosure
// stream at will.
//
// AND THE MODELLED FIGURE THEN WINS, which is the outcome that matters: on a parsed endpoint
// there is usage, so refusing the header does not lose the charge — it replaces a number
// nothing can hold with one the rate table produced. HasReported stays false deliberately, so
// the drift check cannot measure the table against a figure this package already refused.
func TestHeaderCost_PastTheMicrosBoundIsUnusable(t *testing.T) {
	const forged = "1e10"
	pctx := unparsedCostCtx(forged)
	pctx.Extensions.Inference = &pipeline.InferenceExtension{
		Model:        "claude-opus-5",
		InputTokens:  1000,
		OutputTokens: 500,
	}

	cost, state := headerCost(pctx)
	if state != headerUnusable {
		t.Errorf("state = %d for %s, want headerUnusable (%d)", state, forged, headerUnusable)
	}
	if cost != 0 {
		t.Errorf("cost = %v, want 0: a refused figure must not travel", cost)
	}

	got := Settle(pctx, rates(t))
	if !got.Priced || got.Source != event.SourceUsageFallback {
		t.Errorf("Priced = %v / Source = %q, want true / %q: the usage fallback is what makes refusing the header safe on a parsed path",
			got.Priced, got.Source, event.SourceUsageFallback)
	}
	if got.HasReported {
		t.Error("HasReported = true: the drift check would then compare the rate table against a figure this package refused, and report the table as stale")
	}
	if got.RejectedReason != "" {
		t.Errorf("RejectedReason = %q: unrepresentable input is noise, not a disclosure — see TestHeaderCost_ImplausibleIsItsOwnState for the refusal that IS disclosed", got.RejectedReason)
	}
}

// implausibleHalvesRates prices input and output at the highest per-token rate the plausibility
// derivation admits ($0.001), so that maxPlausibleTokens in each of the two tiers makes each
// HALF exactly the $10,000 ceiling and the whole request twice it.
func implausibleHalvesRates(t *testing.T) pricing.Resolver {
	t.Helper()
	var r pricing.Rates
	r.Base[pricing.TierInput], r.Set[pricing.TierInput] = 1e-3, true
	r.Base[pricing.TierOutput], r.Set[pricing.TierOutput] = 1e-3, true
	tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
	if err != nil {
		t.Fatal(err)
	}
	return pricing.NewRegistry(tab)
}

// TestSettle_HalvesAreRefusedWhenTheirSumIs is suggestion 8 of review round 6.
//
// THE PLAUSIBILITY CAP IS PER Cost CALL, AND Settle MAKES THREE OF THEM — the whole request,
// the prompt half, the output half. So a request whose total is refused as impossible can have
// both of its halves come back priced, each one just under the ceiling, and the record then
// says two things that cannot both be true: unpriced, cost $0, prompt $10,000, output $10,000.
// Anything summing the halves — a request row plus a response row, which is exactly what those
// two fields exist for — charges the $20,000 the total refused.
//
// REFUSED TOGETHER, NOT DISCLOSED. Halving a forged figure does not make it two real ones, and
// there is no per-half reason field to carry a disclosure in, so the honest record is the one
// with no half figures on it at all.
//
// AND ONLY WHEN THEIR SUM IS THE PROBLEM, which is why the guard reads the sum rather than
// "was the whole priced". A single half surviving alone is deliberate and load-bearing: where
// the output tier has tokens and no rate, Cost refuses the whole request, and the prompt half is
// then the only figure anyone can attribute — see the comment above promptOnly.
func TestSettle_HalvesAreRefusedWhenTheirSumIs(t *testing.T) {
	const half = 1e7 * 1e-3 // maxPlausibleTokens at the ceiling rate: $10,000, the cap itself
	if !pricing.PlausibleRequestCostUSD(half) {
		t.Fatal("a half is already implausible on its own; this fixture would then prove nothing about the pair")
	}
	if pricing.PlausibleRequestCostUSD(2 * half) {
		t.Fatal("the pair is plausible; this fixture no longer builds the divergence it is about")
	}
	pctx := ctx(map[string]string{"Content-Type": "application/json"}, 1e7, 1e7)

	got := Settle(pctx, implausibleHalvesRates(t))

	if got.Priced || got.CostUSD != 0 {
		t.Errorf("Priced = %v / CostUSD = %v, want false / 0: the whole request is past the plausibility ceiling",
			got.Priced, got.CostUSD)
	}
	if got.HasPrompt || got.PromptUSD != 0 {
		t.Errorf("PromptUSD = %v (HasPrompt %v), want 0 / false: a request row carrying half of a figure this package refused is that figure, charged", got.PromptUSD, got.HasPrompt)
	}
	if got.HasOutput || got.OutputUSD != 0 {
		t.Errorf("OutputUSD = %v (HasOutput %v), want 0 / false", got.OutputUSD, got.HasOutput)
	}
	record := NewRecord(got, nil)
	if sum := record.PromptUSD + record.OutputUSD; sum != 0 {
		t.Errorf("the record's halves sum to %v while it reads Priced() = %v: a consumer adding the two rows charges what the total refused",
			sum, record.Priced())
	}
}

// TestSettle_OnePricedHalfSurvivesAlone is the control for the guard above, and the reason it
// reads the sum instead of the whole.
//
// A table with no OUTPUT rate is not a hypothetical: tool-prune ships one. Cost refuses the
// whole request there — a tier carried tokens and had no rate, so any total would be quietly
// low — and the prompt half is then the only figure that can be attributed to anything. A
// guard keyed on "the whole was refused" would delete it and take the one honest number in the
// record with it.
func TestSettle_OnePricedHalfSurvivesAlone(t *testing.T) {
	var r pricing.Rates
	r.Base[pricing.TierInput], r.Set[pricing.TierInput] = 1e-6, true
	tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
	if err != nil {
		t.Fatal(err)
	}
	pctx := ctx(map[string]string{"Content-Type": "application/json"}, 1000, 500)

	got := Settle(pctx, pricing.NewRegistry(tab))

	if got.HasModelled {
		t.Fatal("HasModelled = true with no output rate over 500 output tokens; the premise of this control is that Cost refuses that whole")
	}
	if !got.HasPrompt || got.PromptUSD != 1000*1e-6 {
		t.Errorf("PromptUSD = %v (HasPrompt %v), want %v / true: the prompt half is the only attributable figure here and must survive",
			got.PromptUSD, got.HasPrompt, 1000*1e-6)
	}
	if got.HasOutput {
		t.Errorf("HasOutput = true at %v with no output rate", got.OutputUSD)
	}
}

// TestSettle_SplitUnreportedReachesTheRecord closes the gap review round 6 named in item 11:
// this reason was pinned only where pricing RETURNS it, never once at the level that publishes
// it.
//
// The two levels are different claims, and the string is load-bearing at both. Settle decides
// whether to attach a reason at all — it gates the call on the usage-fallback arm, so a gateway
// header would suppress it — and NewRecord decides whether the reason survives onto the wire,
// which is where a consumer reads it. Dropping it at either point leaves a figure that is
// approximate in no known direction being rendered as exact: a total-only gateway's number is
// attributed wholly to uncached input, so it over-prices a cache-heavy turn and under-prices a
// generation-heavy one, and nothing about it looks unusual.
//
// TotalTokens alone with no per-kind split is the shape that produces it, which is a standing
// property of a gateway rather than an incident — see pricing.ReasonSplitUnreported.
func TestSettle_SplitUnreportedReachesTheRecord(t *testing.T) {
	pctx := ctx(map[string]string{"Content-Type": "application/json"}, 0, 0)
	pctx.Extensions.Inference.TotalTokens = 1700

	got := Settle(pctx, rates(t))

	if !got.Priced {
		t.Fatalf("Priced = false for a total-only gateway (%+v); the approximation is a caveat on a figure, so there has to be a figure", got)
	}
	if !got.Incomplete || got.IncompleteReason != pricing.ReasonSplitUnreported {
		t.Errorf("Incomplete = %v / reason = %q, want true / %q: without it an approximate total is published as exact",
			got.Incomplete, got.IncompleteReason, pricing.ReasonSplitUnreported)
	}
	record := NewRecord(got, nil)
	if !record.Incomplete || record.IncompleteReason != pricing.ReasonSplitUnreported {
		t.Errorf("record Incomplete = %v / reason = %q, want true / %q: a caveat that reaches no record reaches no consumer",
			record.Incomplete, record.IncompleteReason, pricing.ReasonSplitUnreported)
	}
}

// TestSettle_AnImpossibleTokenReportIsRefusedWhole is must-fix 1 of review round 7.
//
// THE HALVES ARE NOT A PARTITION OF THE COUNTS, they are two passes over the same usage with
// different tiers zeroed — and zeroing is exactly what erases the offending counter. A report
// whose Output is negative, or larger than any request could produce, makes pricing.Cost refuse
// the whole request and the output half; promptOnly zeroes Output, so the count that caused the
// refusal is gone and that call SUCCEEDS. Measured, at 3.8 micros/token for both -5 and
// MaxPlausibleTokens+1: whole ok=false, promptOnly ok=true micros=3800, outputOnly ok=false.
//
// AND A LONE PROMPT HALF IS ENOUGH TO PUBLISH. settleCost's skip gate is
// `!Priced && !HasPrompt && RejectedReason == ""`, so HasPrompt alone carries a record onto the
// wire; abctl's renderer then tests PromptUSD > 0 rather than Priced(). So a report this package
// declared impossible shipped as Priced=false, RejectedReason="", PromptUSD=$0.0038 — and
// rendered. Reachable from the wire: parsercommon assigns these provider ints with no floor.
//
// The sum guard added last round cannot catch it. That one keys on HasPrompt && HasOutput,
// because a lone surviving half is legitimate when a tier has no RATE — a different claim, and
// TestSettle_OnePricedHalfSurvivesAlone is its control. An impossible COUNT is not that case: the
// contract says such a report is refused whole, because mixing a believed figure with a refused
// one in the same row produces a breakdown nobody can reconcile.
func TestSettle_AnImpossibleTokenReportIsRefusedWhole(t *testing.T) {
	for _, tc := range []struct {
		name   string
		input  int
		output int
	}{
		{"a negative output count", 1000, -5},
		{"an output count past what a request could report", 1000, pricing.MaxPlausibleTokens + 1},
		{"a negative input count", -1, 500},
		{"an input count past what a request could report", pricing.MaxPlausibleTokens + 1, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pctx := ctx(map[string]string{"Content-Type": "application/json"}, tc.input, tc.output)

			got := Settle(pctx, rates(t))

			if got.HasModelled || got.Priced {
				t.Errorf("HasModelled = %v / Priced = %v at %v, want false: no figure derived from an impossible count is a price",
					got.HasModelled, got.Priced, got.CostUSD)
			}
			if got.HasPrompt || got.PromptUSD != 0 {
				t.Errorf("PromptUSD = %v (HasPrompt %v), want 0 / false: zeroing the offending tier is what let this through, and a lone prompt half is enough to publish a record",
					got.PromptUSD, got.HasPrompt)
			}
			if got.HasOutput || got.OutputUSD != 0 {
				t.Errorf("OutputUSD = %v (HasOutput %v), want 0 / false", got.OutputUSD, got.HasOutput)
			}
			// AND SAID OUT LOUD. A refusal that reaches no record is indistinguishable from
			// traffic nobody could price — the same argument that put RejectedImplausible on a
			// refused header. Without it, an impossible token report is silent.
			if got.RejectedReason != event.RejectedImplausibleUsage {
				t.Errorf("RejectedReason = %q, want %q: a refused report that discloses nothing looks exactly like unpriced traffic",
					got.RejectedReason, event.RejectedImplausibleUsage)
			}
			record := NewRecord(got, nil)
			if record.Priced() {
				t.Error("the record reads Priced(): a refused report must not read as spend anywhere")
			}
			if sum := record.PromptUSD + record.OutputUSD; sum != 0 {
				t.Errorf("the record's halves sum to %v: that is the figure a request row renders", sum)
			}
		})
	}
}

// TestSettle_AModelledFigurePastTheCeilingIsDisclosed is item 9 of review round 7.
//
// A gateway's refused header is disclosed; a refused MODELLED figure was silent, and the two are
// the same claim about a number — that no one inference call could have cost it. Silence there is
// worse than for the header case, because the causes are ours: an operator's typo in a rate, or a
// figure discovered from a gateway's /model/info, either of which unprices every request it
// touches while looking exactly like traffic nobody had rates for.
//
// The counts here are entirely plausible — this is the RATE that is wrong, which is why the
// impossible-count refusal above cannot cover it.
func TestSettle_AModelledFigurePastTheCeilingIsDisclosed(t *testing.T) {
	var r pricing.Rates
	// $1 per token: a typo of six orders of magnitude, and 20,000 tokens then model $20,000.
	r.Base[pricing.TierInput], r.Set[pricing.TierInput] = 1.0, true
	r.Base[pricing.TierOutput], r.Set[pricing.TierOutput] = 1.0, true
	tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
	if err != nil {
		t.Fatal(err)
	}
	pctx := ctx(map[string]string{"Content-Type": "application/json"}, 15000, 5000)

	got := Settle(pctx, pricing.NewRegistry(tab))

	if got.Priced || got.HasModelled {
		t.Errorf("Priced = %v / HasModelled = %v at %v: a figure past the per-request ceiling is refused, not charged",
			got.Priced, got.HasModelled, got.CostUSD)
	}
	if got.RejectedReason != event.RejectedImplausible {
		t.Errorf("RejectedReason = %q, want %q: the refusal is the whole signal that a rate table has gone wrong",
			got.RejectedReason, event.RejectedImplausible)
	}
	if record := NewRecord(got, nil); record.RejectedReason != event.RejectedImplausible {
		t.Errorf("record RejectedReason = %q: a disclosure that reaches no record reaches no operator", record.RejectedReason)
	}
}

// TestSettle_AHeaderFigureSurvivesAnImpossibleTokenReport is the guard on both disclosures above.
//
// RejectedReason means UNPRICED to every consumer — event.Priced() returns false on any
// non-empty reason. So disclosing a modelled refusal on a response the GATEWAY priced would
// throw away an authoritative figure to complain about a derived one, which is a bigger error
// than the silence it fixes. The disclosure is therefore gated on nothing else having settled.
func TestSettle_AHeaderFigureSurvivesAnImpossibleTokenReport(t *testing.T) {
	pctx := ctx(map[string]string{
		ResponseCostHeader: "0.25",
		"Content-Type":     "application/json",
	}, 1000, -5)

	got := Settle(pctx, rates(t))

	if !got.Priced || got.CostUSD != 0.25 {
		t.Errorf("Priced = %v / CostUSD = %v, want true / 0.25: the gateway's own figure is what the call charged, whatever the token counters claimed",
			got.Priced, got.CostUSD)
	}
	if got.RejectedReason != "" {
		t.Errorf("RejectedReason = %q on a header-priced response: Event.Priced() reads that as unpriced, so this would discard a real charge", got.RejectedReason)
	}
	if record := NewRecord(got, nil); !record.Priced() {
		t.Error("the record reads unpriced: the gateway's figure has to survive a bad token report")
	}
}

// outputOnlyRates is a table with an OUTPUT rate and no input rate — the shape tool-prune ships.
func outputOnlyRates(t *testing.T) pricing.Resolver {
	t.Helper()
	var r pricing.Rates
	r.Base[pricing.TierOutput], r.Set[pricing.TierOutput] = 1e-5, true
	tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
	if err != nil {
		t.Fatal(err)
	}
	return pricing.NewRegistry(tab)
}

// TestSettle_AnImpossibleCountIsRefusedWhateverElseIsWrong is round 8's must-fix, and it is a
// lesson about the instrument rather than about the bound.
//
// Round 7 refused an impossible token report by reading WHY pricing.Cost declined the whole
// request. Cost returns the FIRST reason it meets, so any earlier problem masks the count: with a
// table that has no INPUT rate, {Input: 1000, CacheRead: -1, Output: 50} refuses as "no-rate"
// before the loop ever reaches the -1 — the refusal-reason gate never fires, and the output half,
// which zeroes the cache tiers along with the offending counter, comes back PRICED. Measured:
// OutputUSD 0.0005 on a record with RejectedReason "".
//
// That is precisely the "believed figure beside a refused one in the same row" the gate exists to
// prevent, reached by a route the gate could not see. The counters are provider-controlled ints, so
// the check has to be a question asked of THEM — pricing.PlausibleUsage, once, before any figure is
// derived — and not an inference from an enum that answers a different question.
func TestSettle_AnImpossibleCountIsRefusedWhateverElseIsWrong(t *testing.T) {
	for _, tc := range []struct {
		name  string
		inf   pipeline.InferenceExtension
		rates func(*testing.T) pricing.Resolver
	}{{
		// The reviewer's fixture: the count that is impossible is in a tier the surviving half
		// zeroes, and an earlier tier has no rate.
		name:  "a negative cache count, masked by a missing input rate",
		inf:   pipeline.InferenceExtension{Model: "claude-opus-5", InputTokens: 1000, CacheReadTokens: -1, OutputTokens: 50},
		rates: outputOnlyRates,
	}, {
		name:  "a negative output count, masked by a missing input rate",
		inf:   pipeline.InferenceExtension{Model: "claude-opus-5", InputTokens: 1000, OutputTokens: -5},
		rates: outputOnlyRates,
	}, {
		// And still refused when nothing else is wrong, which round 7's gate did catch: the same
		// claim must not depend on the table.
		name:  "a negative cache count with a complete table",
		inf:   pipeline.InferenceExtension{Model: "claude-opus-5", InputTokens: 1000, CacheReadTokens: -1, OutputTokens: 50},
		rates: rates,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			inf := tc.inf
			pctx := &pipeline.Context{Host: "gw.internal", ResponseHeaders: http.Header{},
				Extensions: pipeline.Extensions{Inference: &inf}}

			got := Settle(pctx, tc.rates(t))

			if got.HasPrompt || got.HasOutput || got.HasModelled || got.Priced {
				t.Errorf("figures survived an impossible count: Priced=%v Cost=%v Prompt=%v(%v) Output=%v(%v)",
					got.Priced, got.CostUSD, got.PromptUSD, got.HasPrompt, got.OutputUSD, got.HasOutput)
			}
			if got.RejectedReason != event.RejectedImplausibleUsage {
				t.Errorf("RejectedReason = %q, want %q: the report was impossible whatever else the table could not price",
					got.RejectedReason, event.RejectedImplausibleUsage)
			}
			if rec := NewRecord(got, nil); rec.PromptUSD+rec.OutputUSD != 0 {
				t.Errorf("the record carries %v across its halves, which is what a request row renders", rec.PromptUSD+rec.OutputUSD)
			}
		})
	}
}

// TestSettle_AHeaderKeepsItsFigureAndTheCountsAreStillRefused is round 8's disclosure gap.
//
// When a gateway header prices the response, an impossible token report produced NO disclosure at
// all: RejectedReason is deliberately withheld there, because it reads as unpriced everywhere and
// would discard a real charge — so Trust() came back "exact" and a client rendered exact money
// beside counts nobody could have counted.
//
// RejectedReason was the wrong carrier for it. UsageRefused says the thing that is true — the
// dollars stand, the token totals do not — without touching what may be spent.
func TestSettle_AHeaderKeepsItsFigureAndTheCountsAreStillRefused(t *testing.T) {
	pctx := ctx(map[string]string{
		ResponseCostHeader: "0.25",
		"Content-Type":     "application/json",
	}, 1000, -5)

	got := Settle(pctx, rates(t))

	if !got.Priced || got.CostUSD != 0.25 {
		t.Errorf("Priced = %v / CostUSD = %v, want true / 0.25: the header is what the call charged, whatever the counters claimed",
			got.Priced, got.CostUSD)
	}
	if !got.UsageRefused {
		t.Error("UsageRefused = false: the counts were impossible and nothing on the record said so")
	}
	if got.RejectedReason != "" {
		t.Errorf("RejectedReason = %q: that reads as unpriced to every consumer and would discard a real charge", got.RejectedReason)
	}
	rec := NewRecord(got, nil)
	if !rec.UsageRefused {
		t.Error("the record does not carry UsageRefused: a consumer rendering token totals cannot see it")
	}
	// AND THE MONEY IS UNTOUCHED, which is the whole point of a separate carrier.
	if !rec.Priced() || rec.Trust() != event.TrustExact {
		t.Errorf("record Priced = %v / Trust = %q, want true / %q: an impossible token report must not unprice a gateway's own figure",
			rec.Priced(), rec.Trust(), event.TrustExact)
	}
	// AND NO SAVING IS CALIBRATED ON THE REFUSED COUNTS.
	if avoided := Avoided(pctx, rates(t)); len(avoided) != 0 {
		t.Errorf("avoided = %+v on a response whose counters were refused: the saving is a slice of those same counters", avoided)
	}
}

// TestSettle_ARefusedWholeLeavesNoHalfStanding is the hole the pair guard left.
//
// That guard requires BOTH halves, so it closed only the case where each is individually under the
// ceiling and their sum is over. When one half ITSELF breaks the ceiling, pricing.Cost refuses that
// half, the pair test cannot fire, and the sibling is published on a record that reads unpriced —
// where abctl's renderer, which tests OutputUSD > 0 rather than Priced(), displays it as money.
//
// Driven at $1 per token, the same six-order-of-magnitude rate typo as the fixture above, in both
// directions: whichever half is the larger one is the one Cost refuses, and the survivor is drawn
// from the same condemned table at the same prompt total.
//
// THE LAST TWO ROWS ARE A SECOND, WORSE LEAK, and the reason the guard asks ImpossibleFigure rather
// than naming one refusal. Cost tests representability BEFORE the plausibility ceiling, so a figure
// past ~$9.007e9 is labelled RefusalUnrepresentable and carried no disclosure at all: refused,
// unpriced, both halves' sibling still standing, and RejectedReason EMPTY. Keying on the smaller
// bound covered $10,000 to $9 billion and let everything above it through.
func TestSettle_ARefusedWholeLeavesNoHalfStanding(t *testing.T) {
	rates := func(in, out float64) pricing.Resolver {
		var r pricing.Rates
		r.Base[pricing.TierInput], r.Set[pricing.TierInput] = in, true
		r.Base[pricing.TierOutput], r.Set[pricing.TierOutput] = out, true
		tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
		if err != nil {
			t.Fatal(err)
		}
		return pricing.NewRegistry(tab)
	}

	for _, tc := range []struct {
		name            string
		inRate, outRate float64
		input, output   int
	}{
		// $15,000 prompt half refused, $5,000 output half was published.
		{"the prompt half is the one over the ceiling", 1.0, 1.0, 15000, 5000},
		// The mirror image, which the same guard has to cover.
		{"the output half is the one over the ceiling", 1.0, 1.0, 5000, 15000},
		// Neither half over, sum over: the case the original pair test was written for, kept
		// here so one test states the whole rule.
		{"neither half over, the pair is", 1.0, 1.0, 8000, 8000},
		// Past representability, where the refusal carries the OTHER label: $1e6 per input token
		// makes the whole and the prompt half unrepresentable while the output half stays ordinary.
		{"the prompt half is past representability", 1e6, 1.0, 15000, 5000},
		{"the output half is past representability", 1.0, 1e6, 5000, 15000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := rates(tc.inRate, tc.outRate)
			pctx := ctx(map[string]string{"Content-Type": "application/json"}, tc.input, tc.output)

			got := Settle(pctx, reg)

			// TWO THINGS AT ONCE, deliberately. It is the precondition — a whole refused for
			// magnitude is what makes the halves suspect, and without it a row could pass on a
			// table that priced nothing — AND it is the disclosure, which is the half of this
			// that went missing: keyed on one label, a figure past representability came back
			// refused, unpriced and silent, so an operator saw no reason at all.
			if got.RejectedReason != event.RejectedImplausible {
				t.Fatalf("RejectedReason = %q, want %q: a magnitude refusal must be disclosed, and an undisclosed one is also the case this row cannot otherwise reach",
					got.RejectedReason, event.RejectedImplausible)
			}
			if got.HasPrompt || got.PromptUSD != 0 {
				t.Errorf("HasPrompt = %v PromptUSD = %v, want false/0: a half drawn from the rate table that produced an impossible whole is not attributable",
					got.HasPrompt, got.PromptUSD)
			}
			if got.HasOutput || got.OutputUSD != 0 {
				t.Errorf("HasOutput = %v OutputUSD = %v, want false/0", got.HasOutput, got.OutputUSD)
			}
			// The record is what a renderer reads, and it does no Priced() gating of the halves.
			if rec := NewRecord(got, nil); rec.PromptUSD != 0 || rec.OutputUSD != 0 {
				t.Errorf("record PromptUSD = %v OutputUSD = %v, want 0/0: this is the figure abctl would display for a refused request",
					rec.PromptUSD, rec.OutputUSD)
			}
		})
	}
}
