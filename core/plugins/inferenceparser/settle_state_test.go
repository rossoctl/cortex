package inferenceparser

import (
	"context"
	"testing"

	"github.com/rossoctl/cortex/core/costing"
)

// THE TWO CLAIMS SETTLECOST'S STATE MAKES, neither of which was pinned anywhere.
//
// The published record has a whole file of coverage (unparsed_cost_test.go,
// bodyless_cost_test.go). These two are about what settleCost leaves BEHIND on pctx: the
// idempotence latch, and the Settled outcome that litellm-budget-track reads back through
// costing.Load. Both are read by other components and neither is visible in a published
// event, so a regression in either is silent.

// TestSettleCost_NothingToSayDoesNotLatch is the placement of the idempotence latch.
//
// The latch protects a PUBLISHED figure against a double charge. It must therefore be set
// when a figure is published and not when a pass finds nothing to say, or a pass that
// published nothing locks the request out of ever being settled by a later pass that has a
// figure — the latch turning into the thing that loses the money it exists to protect.
//
// litellm-budget-track's bill() already takes exactly this position, and says so:
// "NOT marked settled: a listener that calls OnResponse before the parser has finalized
// must not lock this request out of being billed by the terminal frame that follows."
// Two guards over one request's money must not disagree about their own rule, whichever
// of the two is reachable first under whichever listener.
//
// Not reachable in production TODAY: every current dispatch reaches settleCost with the
// response headers already populated, so a pass that finds nothing is followed only by
// passes that find nothing. This pins the RULE rather than a live defect, which is the
// same standing the sibling guard has.
func TestSettleCost_NothingToSayDoesNotLatch(t *testing.T) {
	p := NewInferenceParser()
	p.SetPricingResolver(bodylessRates(t))
	// No cost header and no extension: nothing to price, nothing to publish.
	pctx := unparsedCtx("/v1/embeddings", "")

	p.OnResponseFrame(context.Background(), pctx, nil, true)

	if ev, ok := publishedCost(t, pctx); ok {
		t.Fatalf("published %+v with no cost header and no usage; this fixture must settle NOTHING or the second half of the test proves nothing", ev)
	}

	// The figure arrives on a later pass. Contrived — see the doc comment — and the point
	// is that the guard's answer must not depend on that.
	pctx.ResponseHeaders.Set(costing.ResponseCostHeader, "0.0042")
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ev, ok := publishedCost(t, pctx)
	if !ok {
		t.Fatal("no cost record after a second pass carrying a gateway figure: the latch was set by a pass that published nothing, so this request can never be settled and the spend reaches no ledger, no /v1/usage total and no budget")
	}
	if ev.CostUSD != 0.0042 {
		t.Errorf("CostUSD = %v, want 0.0042", ev.CostUSD)
	}
}

// TestSettleCost_LatchesOncePublished is the mirror, and it is what stops the test above
// being satisfied by deleting the latch.
//
// A terminal frame can arrive twice, and a plugin cannot verify from the inside that it will
// not — see settleCost. Once a figure is out, a repeat dispatch must not produce a second one:
// double-counting money is not recoverable from a later correction, because the ledger file
// has already been written.
func TestSettleCost_LatchesOncePublished(t *testing.T) {
	p := NewInferenceParser()
	p.SetPricingResolver(bodylessRates(t))
	pctx := unparsedCtx("/v1/embeddings", "0.0042")

	p.OnResponseFrame(context.Background(), pctx, nil, true)
	if _, ok := publishedCost(t, pctx); !ok {
		t.Fatal("nothing published on the first terminal frame")
	}

	// A second gateway figure the latch must ignore. Rewriting the header proves the latch
	// short-circuits before Settle reads it, rather than the two passes happening to agree.
	pctx.ResponseHeaders.Set(costing.ResponseCostHeader, "99.0")
	p.OnResponseFrame(context.Background(), pctx, nil, true)
	// The buffered hook as well, since a pipeline that reached both must not charge twice
	// either. It cannot fire under a real listener (RunResponse skips a StreamingResponder),
	// so this is the direct-caller half of the same latch.
	p.OnResponse(context.Background(), pctx)

	ev, _ := publishedCost(t, pctx)
	if ev.CostUSD != 0.0042 {
		t.Errorf("CostUSD = %v after a repeat dispatch, want the first figure 0.0042 — a second settle is a double charge", ev.CostUSD)
	}
}

// TestSettleCost_StoresOnEveryProxiedResponse is the claim costing.Load's contract now
// rests on.
//
// Load's false does NOT mean "no inference at all" — a reading that holds only while Store is
// reached from the parsed paths alone. It is reached on every proxied response, so false means
// the cost owner's response pass did not run, which says nothing about the traffic. A consumer that keeps the old reading would treat a health check as an
// inference request that failed to price, instead of as a decision that correctly found
// nothing.
//
// The pairing is the whole assertion: Load TRUE (a decision was reached) with Priced FALSE
// (the decision was not a figure). Asserting either alone would pass under the behaviour
// this replaces.
func TestSettleCost_StoresOnEveryProxiedResponse(t *testing.T) {
	for _, path := range []string{"/v1/embeddings", "/healthz", "/mcp", "/some/tunnel"} {
		t.Run(path, func(t *testing.T) {
			p := NewInferenceParser()
			p.SetPricingResolver(bodylessRates(t))
			pctx := unparsedCtx(path, "")

			p.OnResponseFrame(context.Background(), pctx, nil, true)

			settled, ok := costing.Load(pctx)
			if !ok {
				t.Fatalf("costing.Load reported no outcome for %s; Store runs on every proxied response, and a consumer reading false as \"no inference\" is reading a claim this no longer makes", path)
			}
			if settled.Priced {
				t.Errorf("Settled.Priced = true for %s with no cost header and no usage; a stored outcome is a decision, not a figure, and pricing this would put non-inference traffic in the coverage denominator", path)
			}
		})
	}
}
