package pricing

import (
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// Presence bits, mirrored from parsercommon.Kind so a fixture can say which kinds it means
// rather than carrying bare shifts.
//
// Mirrored rather than imported because parsercommon lives under plugins/internal and is
// unreachable from here — the same constraint that made pipeline.InferenceExtension.PresentKinds
// a plain uint8 with its layout written into the doc comment. They live in this file because the
// only readers are the assertions below, which pin what the mask CANNOT discriminate: nothing in
// the package reads it.
const (
	presentInput  uint8 = 1 << 0 // parsercommon.KindInput
	presentOutput uint8 = 1 << 3 // parsercommon.KindOutput
)

// IncompleteReason decides whether a modelled figure may be presented as an exact
// total, so the cases below are the boundary of what the system claims to know about
// its own spend. Both directions matter: a missed floor understates the bill silently,
// and a false one attaches a permanent caveat to figures that are exact.
func TestIncompleteReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		inf  *pipeline.InferenceExtension
		want string
	}{{
		// THE BUG. message_start landed, message_delta never did: prompt counted,
		// output zero, no stop reason.
		//
		// PresentKinds here is what a real truncated Anthropic stream carries —
		// Input|CacheRead|Output, with the Output BIT set and its TALLY zero, because
		// toNeutral asserts Input|Output unconditionally and
		// mergeAnthropicUsageMaxSeen ORs the mask without ever assigning Output. The
		// fixture keeps that bit set on purpose: it is what makes this case
		// indistinguishable from a reported zero by mask alone, and any future
		// "simplification" to the bitmask has to fail here.
		name: "truncated Anthropic stream: prompt counted, output bit set but never tallied",
		inf: &pipeline.InferenceExtension{
			InputTokens: 1000, CacheReadTokens: 500,
			PromptTokens: 1500,
			PresentKinds: presentInput | 1<<1 | presentOutput,
		},
		want: ReasonOutputUncounted,
	}, {
		name: "complete stream: output tallied and a stop reason",
		inf: &pipeline.InferenceExtension{
			InputTokens: 1000, OutputTokens: 240,
			PromptTokens: 1000, CompletionTokens: 240,
			PresentKinds: presentInput | presentOutput,
			FinishReason: "end_turn",
		},
		want: "",
	}, {
		// A stop reason is the provider stating the tally finished, so a genuine
		// zero-output turn is exact rather than a floor.
		name: "stop reason with zero output is exact",
		inf: &pipeline.InferenceExtension{
			InputTokens: 1000, PromptTokens: 1000,
			PresentKinds: presentInput | presentOutput,
			FinishReason: "max_tokens",
		},
		want: "",
	}, {
		// The tally is what the figure needs; the reason is only the signal that the
		// tally is final. A response that arrived WHOLE — Stream unset — has a final
		// tally whether or not the dialect names a stop reason, so demanding one here
		// would attach a permanent caveat to exact money.
		name: "output tallied without a stop reason, not a stream, is exact",
		inf: &pipeline.InferenceExtension{
			InputTokens: 1000, OutputTokens: 12,
			PromptTokens: 1000, CompletionTokens: 12,
			PresentKinds: presentInput | presentOutput,
		},
		want: "",
	}, {
		// THE SAME EXTENSION, PLUS Stream, AND THE OPPOSITE ANSWER — which is why the row
		// above cannot be read as a general rule.
		//
		// foldOpenAIFrame REPLACES the accumulated usage with each usage-bearing chunk's
		// totals ("OpenAI streams cumulative usage: each usage-bearing chunk restates the
		// full totals"), so on that dialect a mid-generation tally is a RUNNING total. A
		// stream that dies there has output > 0 and no stop reason, and the old
		// `OutputTokens > 0 -> exact` early return — which sat ABOVE the stop-reason check
		// — published that floor as a whole figure. That is the same failure as the
		// truncated-Anthropic case at the top of this table, reached from the other side.
		//
		// StreamedResponse, not Stream: the shape that decides whether a tally is final is the
		// RESPONSE's, and the request's flag is a different fact. See the two rows below.
		name: "truncated OpenAI-shaped stream: running tally, no stop reason",
		inf: &pipeline.InferenceExtension{
			StreamedResponse: true,
			InputTokens:      1000, OutputTokens: 12,
			PromptTokens: 1000, CompletionTokens: 12,
			PresentKinds: presentInput | presentOutput,
		},
		want: ReasonOutputUncounted,
	}, {
		// A STREAMED RESPONSE TO A NON-STREAMING REQUEST, truncated. Reachable — a gateway
		// answers with SSE whatever the request asked — and keyed on the request flag this was
		// classified EXACT: a running tally, no stop reason, Stream false. A floor labelled whole,
		// which is the failure this table exists for.
		name: "truncated stream that the request never asked for",
		inf: &pipeline.InferenceExtension{
			Stream: false, StreamedResponse: true,
			InputTokens: 1000, OutputTokens: 12,
			PromptTokens: 1000, CompletionTokens: 12,
			PresentKinds: presentInput | presentOutput,
		},
		want: ReasonOutputUncounted,
	}, {
		// AND THE OTHER DIRECTION, which is why this is the response's shape rather than either
		// flag: a client asked for a stream and got a buffered body. The tally arrived whole, so
		// caveating it would print "the real total is higher" over money that is exact — the
		// false-positive direction that teaches a reader to ignore the marker.
		name: "a buffered response to a streaming request is exact",
		inf: &pipeline.InferenceExtension{
			Stream: true, StreamedResponse: false,
			InputTokens: 1000, OutputTokens: 240,
			PromptTokens: 1000, CompletionTokens: 240,
			PresentKinds: presentInput | presentOutput,
		},
		want: "",
	}, {
		// And the stream that FINISHED stays exact, so the row above is a statement about
		// truncation and not about streaming. Without this, gating on Stream would be
		// indistinguishable from caveating every streamed response — which is most of the
		// traffic, and would make the marker meaningless.
		name: "completed stream with a tally and a stop reason is exact",
		inf: &pipeline.InferenceExtension{
			StreamedResponse: true,
			InputTokens:      1000, OutputTokens: 240,
			PromptTokens: 1000, CompletionTokens: 240,
			PresentKinds: presentInput | presentOutput,
			FinishReason: "stop",
		},
		want: "",
	}, {
		// Legacy aggregates from a provider that reports no split. Output is there.
		name: "legacy completion aggregate alone counts as output",
		inf: &pipeline.InferenceExtension{
			PromptTokens: 900, CompletionTokens: 40,
		},
		want: "",
	}, {
		// A gateway reporting only total_tokens. UsageFromInference attributes that
		// total wholly to uncached input, so the figure is approximate in no known
		// direction — NOT a floor, and a different conversation from a truncated
		// stream. This is the branch where the presence mask IS the right instrument.
		name: "total-only gateway is approximate, not partial",
		inf: &pipeline.InferenceExtension{
			TotalTokens: 1700,
		},
		want: ReasonSplitUnreported,
	}, {
		// Distinguishing the two above is the whole reason for a reason string: this
		// one is a standing property of the gateway, the truncated stream is an
		// incident. One boolean could not tell them apart.
		name: "total-only with a stop reason is still approximate",
		inf: &pipeline.InferenceExtension{
			TotalTokens: 1700, FinishReason: "stop",
		},
		want: ReasonSplitUnreported,
	}, {
		// No counters at all: no figure exists, so there is nothing to qualify.
		// costing publishes nothing for this request. See the settle-time test in
		// core/cost/settle for the path that proves it never reaches the flag.
		name: "no counters at all is unpriced, not incomplete",
		inf:  &pipeline.InferenceExtension{Model: "claude-opus-5"},
		want: "",
	}, {
		// A cancelled turn mid-tool-arguments: the model generated tokens the wire
		// never tallied. Genuinely a floor.
		name: "tool call captured but no output tally",
		inf: &pipeline.InferenceExtension{
			InputTokens: 800, PromptTokens: 800,
			ToolCalls: []pipeline.InferenceToolCall{{ID: "t1", Name: "Read"}},
		},
		want: ReasonOutputUncounted,
	}, {
		// A producer that filled the split without the derived aggregate. Read the
		// split rather than trusting Fill to have run.
		name: "cache-write-only prompt, split without aggregate",
		inf: &pipeline.InferenceExtension{
			CacheWriteTokens: 4096,
		},
		want: ReasonOutputUncounted,
	}, {
		// THE FALSE POSITIVE A PRESENCE-MASK TEST PRODUCES, and the reason this predicate
		// reads counters instead. Cache counts are a per-kind split: they were reported,
		// UsageFromInference prices them, and no total was attributed to input — so there is
		// nothing approximate to disclose. The Input and Output BITS are clear here, which is
		// all a mask test would look at. The stop reason keeps this off the
		// floor branch, so the split branch is the one under test.
		name: "cache-only split with a total is not a totals-only gateway",
		inf: &pipeline.InferenceExtension{
			CacheReadTokens: 30000, TotalTokens: 30000,
			PresentKinds: 1<<1 | 1<<2, // KindCacheRead | KindCacheWrite
			FinishReason: "end_turn",
		},
		want: "",
	}, {
		// THE FALSE NEGATIVE, which the mask could never see. Anthropic asserts
		// Input|Output unconditionally, so both bits are set with both tallies zero; a
		// total arriving alongside them IS attributed wholly to uncached input by
		// UsageFromInference, and the mask would have published that as an exact figure.
		name: "totals-only behind bits a dialect sets unconditionally is still approximate",
		inf: &pipeline.InferenceExtension{
			TotalTokens:  1700,
			PresentKinds: presentInput | presentOutput,
			FinishReason: "stop",
		},
		want: ReasonSplitUnreported,
	}, {
		name: "nil extension",
		inf:  nil,
		want: "",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IncompleteReason(tc.inf); got != tc.want {
				t.Errorf("IncompleteReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIncompleteReason_MaskAloneCannotDiscriminate is the tripwire for the
// "simplification" this predicate exists to resist.
//
// It asserts the property directly: on a truncated Anthropic stream the Output presence
// bit is SET while the tally is zero, so `PresentKinds & KindOutput == 0` is FALSE for
// the exact case the bug lives on. Anyone who replaces the stop-reason discriminator
// with the bitmask fails here with a message saying why, rather than shipping a check
// that cannot fire for its own defect.
//
// The asymmetry is dialect-specific and that is the trap: for OpenAI the mask WOULD
// work, because inferenceUsage.toNeutral gates each bit on a pointer. Checking the mask
// against the OpenAI path finds it correct and invites the conclusion that it
// generalizes — and Anthropic, which is what Claude Code speaks, is where it does not.
func TestIncompleteReason_MaskAloneCannotDiscriminate(t *testing.T) {
	// Anthropic, truncated: mask says output was reported, the tally says zero.
	truncated := &pipeline.InferenceExtension{
		InputTokens: 1000, PromptTokens: 1000,
		PresentKinds: presentInput | presentOutput,
	}
	if truncated.PresentKinds&presentOutput == 0 {
		t.Fatal("fixture does not carry the Output bit; it no longer represents a truncated Anthropic stream and the rest of this test proves nothing")
	}
	if got := IncompleteReason(truncated); got != ReasonOutputUncounted {
		t.Errorf("IncompleteReason = %q, want %q: the Output BIT is set here while the TALLY is zero, so a bitmask check cannot see this case — the stop reason is what discriminates", got, ReasonOutputUncounted)
	}

	// A genuine reported zero on the same dialect: bit-identical to the above, and the
	// stop reason is the ONLY thing separating them.
	reportedZero := &pipeline.InferenceExtension{
		InputTokens: 1000, PromptTokens: 1000,
		PresentKinds: presentInput | presentOutput,
		FinishReason: "end_turn",
	}
	if reportedZero.PresentKinds != truncated.PresentKinds {
		t.Fatal("the two fixtures must be bit-identical for this test to mean anything")
	}
	if got := IncompleteReason(reportedZero); got != "" {
		t.Errorf("IncompleteReason = %q, want \"\": a stop reason means the tally finished, so this total is exact", got)
	}
}

// tierMicros is what distinctTierRates charges per token in each tier, in micro-dollars.
//
// DISTINCT PER TIER, and that is the entire point of the numbers. Charging 1e-6 in all four
// tiers makes the floor test's expectation a function of the token TOTAL and of nothing else:
// 1,000 uncached-input and 500 cache-read tokens price identically to 500 input and 1,000
// cache reads — a 10x error on the real card, and the exact mistake the four-way split exists
// to prevent — so the assertion stays green with TierInput and TierCacheRead transposed in
// Usage.tokens.
//
// FOUR PRIMES, no two of which sum to or divide a third, so no transposition can
// coincide. 1/2/4/8 would not do: a fixture carrying twice as many of the 4-tier's tokens
// as the 8-tier's prices the same either way round, and a swap of those two would pass.
// Ordered cache_read < input < cache_write < output so the SHAPE matches a real rate card
// and nothing here reads as an inverted one; the values themselves are synthetic and are
// nobody's price list.
//
// The SAME four values as core/cost/settle's tierMicros, deliberately: 853fedc3 fixed this
// blindness there first, and one convention across the two suites means a figure copied
// between them still means the same thing. Not shared through a helper package because a
// test fixture that two packages import is a third thing to keep honest.
var tierMicros = map[Tier]float64{
	TierCacheRead:  3,
	TierInput:      7,
	TierCacheWrite: 11,
	TierOutput:     23,
}

// distinctTierRates builds a fully-priced table charging tierMicros per token, so a total
// says WHICH tiers the tokens were priced in and not merely how many there were.
//
// The pairwise-distinctness check is a guard on the FIXTURE, not on Cost: distinctness is
// the property that makes an expectation written against this table tier-sensitive at all,
// so a future edit that quietly gives two tiers one rate fails here rather than silently
// restoring the blind spot.
func distinctTierRates(t *testing.T) Rates {
	t.Helper()
	var r Rates
	seen := make(map[float64]Tier, len(tierMicros))
	for tier, perToken := range tierMicros {
		if other, dup := seen[perToken]; dup {
			t.Fatalf("%v and %v both charge %v micro-dollars per token; two tiers on one rate makes every expectation against this table blind to a swap between them",
				tier, other, perToken)
		}
		seen[perToken] = tier
		r.Base[tier], r.Set[tier] = perToken*1e-6, true
	}
	return r
}

// A floor must still PRICE. Refusing to price it would be the wrong fix: the prompt cost
// is real, and dropping it understates spend by more than presenting it as exact ever
// did. This is the property settle.Settle relies on to publish a figure at all.
//
// Priced at each tier's OWN rate, not at a flat per-token one. A floor charged off the
// wrong tier would be a second error hiding inside a disclosed one, and it is the error
// that costs real money silently: a long-running agent's traffic is overwhelmingly cache
// reads, billed as uncached input at 10x on the published card.
func TestIncompleteReason_FloorStillPrices(t *testing.T) {
	inf := &pipeline.InferenceExtension{
		InputTokens: 1000, CacheReadTokens: 500, PromptTokens: 1500,
	}
	if IncompleteReason(inf) != ReasonOutputUncounted {
		t.Fatal("fixture is not a floor; the rest of this test proves nothing")
	}
	micros, ok := Cost(distinctTierRates(t), UsageFromInference(inf))
	if !ok {
		t.Fatal("Cost refused a floor; a lower bound is still a figure, and dropping it loses real spend")
	}
	// Per-tier arithmetic rather than a bare total, so the assertion says which rate each
	// count is charged at: 1,000 uncached input tokens at the input rate plus 500 cache
	// reads at the cache-read rate. Transposed, the same two counts come to 6,500.
	want := int64(1000*tierMicros[TierInput] + 500*tierMicros[TierCacheRead])
	if micros != want {
		t.Errorf("micros = %d, want %d — 1000 uncached input tokens at %v/token and 500 cache reads at %v/token, each in its OWN tier",
			micros, want, tierMicros[TierInput]*1e-6, tierMicros[TierCacheRead]*1e-6)
	}
}

// TestIncompleteReason_FloorChargesEachTierAtItsOwnRate pins the transposition directly
// rather than relying on the floor fixture to catch it incidentally.
//
// One fixture per tier, the same 1,000 tokens in each, each expected at its own rate: a
// swap anywhere between InferenceExtension's field names, UsageFromInference's mapping and
// Cost's rate lookup moves two of the four figures. This is the pricing-side mirror of
// core/cost/settle's TestSettle_PricesEachTierAtItsOwnRate, one layer down — that one goes
// through Settle and a Resolver, this one straight through the two functions the mapping
// actually lives in, so a failure names which of the two layers moved.
func TestIncompleteReason_FloorChargesEachTierAtItsOwnRate(t *testing.T) {
	const n = 1000
	r := distinctTierRates(t)
	settled := make(map[Tier]int64, len(tierMicros))
	for _, tc := range []struct {
		tier Tier
		inf  pipeline.InferenceExtension
	}{
		{TierInput, pipeline.InferenceExtension{InputTokens: n}},
		{TierCacheWrite, pipeline.InferenceExtension{CacheWriteTokens: n}},
		{TierCacheRead, pipeline.InferenceExtension{CacheReadTokens: n}},
		{TierOutput, pipeline.InferenceExtension{OutputTokens: n}},
	} {
		t.Run(tc.tier.String(), func(t *testing.T) {
			inf := tc.inf
			micros, ok := Cost(r, UsageFromInference(&inf))
			if !ok {
				t.Fatalf("Cost reported unpriced for %d %s tokens against a fully-priced table", n, tc.tier)
			}
			if want := int64(n * tierMicros[tc.tier]); micros != want {
				t.Errorf("micros = %d, want %d — %d %s tokens must charge the %s rate (%v/token), and no other tier's",
					micros, want, n, tc.tier, tc.tier, tierMicros[tc.tier]*1e-6)
			}
			settled[tc.tier] = micros
		})
	}

	byCost := make(map[int64]Tier, len(settled))
	for tier, micros := range settled {
		if other, dup := byCost[micros]; dup {
			t.Errorf("%v and %v both price %d tokens at %d micros; two tiers charging one rate makes every expectation in this file blind to a swap between them",
				tier, other, n, micros)
		}
		byCost[micros] = tier
	}
}

// TOKENS REFUSED, MONEY BELIEVED — the divergence this bound closes, measured on both sides.
//
// headerCost refuses a gateway's own figure past MaxPlausibleRequestCostMicros ($10,000), and
// usage.plausibleTokenReport refuses an implausible token REPORT, counting it in
// Counts.RefusedTokenRequests. The modelled path had neither: pricing.Cost bounded the counts
// only below zero, so the dollars derived from a report the aggregator had just called
// impossible were priced and kept, bounded only by MaxCostMicros — $9 billion, five orders of
// magnitude past the cap the header path enforces on the same request.
//
// The figures below are what makes it worth a test rather than a comment: at the dearest rate
// that ships, one forged field prices at tens of millions of dollars, and every one of those
// dollars reaches the budget ledger, the 30-day file and the daily total.
func TestCost_RefusesAnImplausibleTokenCountOnTheModelledPath(t *testing.T) {
	// The dearest tier in the bundled table: $75/Mtok.
	var r Rates
	r.Base[TierOutput], r.Set[TierOutput] = 7.5e-5, true
	r.Base[TierInput], r.Set[TierInput] = 7.5e-5, true

	for _, tc := range []struct {
		name string
		u    Usage
		want bool // priced?
	}{
		{"a real large call is still priced", Usage{Input: 1_000_000, Output: 64_000}, true},
		{"the plausible ceiling itself is priced", Usage{Output: maxPlausibleTokens}, true},
		{"one token past the ceiling is unpriced", Usage{Output: maxPlausibleTokens + 1}, false},
		// What the old code did with this: 1e12 tokens x 7.5e-5 = $75,000,000, comfortably
		// under MaxCostMicros, so it was priced and settled.
		{"a forged trillion-token report", Usage{Output: 1_000_000_000_000}, false},
		// And the mixed case, because the loop must not stop at the first plausible field.
		{"one bad field among good ones", Usage{Input: 1000, Output: maxPlausibleTokens + 1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			micros, ok := Cost(r, tc.u)
			if ok != tc.want {
				t.Errorf("Cost() priced = %v (%d micros), want priced = %v", ok, micros, tc.want)
			}
			if ok && micros > MaxPlausibleRequestCostMicros {
				t.Errorf("Cost() = %d micros, past MaxPlausibleRequestCostMicros (%d): bounding the "+
					"COUNTS is supposed to make the figure bound follow arithmetically",
					micros, MaxPlausibleRequestCostMicros)
			}
		})
	}
}

// TestIncompleteReason_CountersBelowTotal is item 4 of review round 7.
//
// A response reporting input 600, output 0, total 1000 with a stop reason of "stop" prices 600
// tokens and called that figure EXACT. It fell between the two predicates: outputUncounted
// returns false on any stop reason, and totalsOnly requires every counter to be empty. So 400
// reported tokens went unpriced and unqualified — a floor published as a total, which is the one
// outcome this file exists to prevent.
//
// The rows below are the discrimination, not decoration. The cross-check compares figures the
// RESPONSE supplied, so a genuinely zero-output call agrees with itself and stays exact; that is
// the false positive outputUncounted's comment warns about, and the reason this is a separate
// branch rather than a loosening of that predicate.
func TestIncompleteReason_CountersBelowTotal(t *testing.T) {
	for _, tc := range []struct {
		name string
		inf  *pipeline.InferenceExtension
		want string
	}{{
		name: "a stated total larger than the counters",
		inf:  &pipeline.InferenceExtension{InputTokens: 600, OutputTokens: 0, TotalTokens: 1000, FinishReason: "stop"},
		want: ReasonCountersBelowTotal,
	}, {
		// THE CONTROL THAT MATTERS. Genuinely zero output — a refusal, or max_tokens of zero —
		// reports a total equal to the prompt. Flagging this would put a permanent caveat on
		// ordinary traffic, which is how an operator learns to ignore the real one.
		name: "a genuine zero-output call is exact",
		inf:  &pipeline.InferenceExtension{InputTokens: 600, OutputTokens: 0, TotalTokens: 600, FinishReason: "stop"},
		want: "",
	}, {
		name: "counters that add up to the total are exact",
		inf:  &pipeline.InferenceExtension{InputTokens: 600, OutputTokens: 400, TotalTokens: 1000, FinishReason: "stop"},
		want: "",
	}, {
		// The legacy aggregates land in the same two tiers, so the cross-check covers them
		// without knowing which field carried the count.
		name: "the legacy aggregates are cross-checked too",
		inf:  &pipeline.InferenceExtension{PromptTokens: 600, CompletionTokens: 0, TotalTokens: 1000, FinishReason: "stop"},
		want: ReasonCountersBelowTotal,
	}, {
		// A total SMALLER than the parts is a gateway contradicting itself, and nothing here
		// knows which side to believe — so it is not a claim that the figure is low.
		name: "a total smaller than the counters is not a floor",
		inf:  &pipeline.InferenceExtension{InputTokens: 600, OutputTokens: 400, TotalTokens: 700, FinishReason: "stop"},
		want: "",
	}, {
		// Still the stronger claim when both could apply: no stop reason at all.
		name: "no stop reason still reports the floor it already did",
		inf:  &pipeline.InferenceExtension{InputTokens: 600, OutputTokens: 0, TotalTokens: 1000},
		want: ReasonOutputUncounted,
	}, {
		name: "a totals-only gateway keeps its own reason",
		inf:  &pipeline.InferenceExtension{TotalTokens: 1700},
		want: ReasonSplitUnreported,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IncompleteReason(tc.inf); got != tc.want {
				t.Errorf("IncompleteReason = %q, want %q", got, tc.want)
			}
		})
	}
}
