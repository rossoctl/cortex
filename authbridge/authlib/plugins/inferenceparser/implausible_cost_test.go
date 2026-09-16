package inferenceparser

import (
	"context"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/costing"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// THE TRUST BOUNDARY, END TO END THROUGH THE PLUGIN. unparsed_cost_test.go covers the
// widening this cap bounds: cost is settled on every response, including the ones where the
// request was never parsed, so a gateway-priced /v1/embeddings call is no longer free of
// charge. The other side of that coin is that ANY path on ANY host an agent is proxied to
// can now name its own figure in a response header nobody authenticated, and inference-parser
// is in the default local pipeline.
//
// The verified blast radius of one accepted forgery: published, aggregated, written to the
// thirty-day ledger, and fed to litellm_budgettrack, which denies with HTTP 429 once the
// daily total passes MaxBudget. So one hostile site can lock an agent out of all further
// inference and poison durable cost reports.
//
// Driven through both response hooks, like the tests it sits beside, because a mutation that
// neuters one must not be covered by the other.

// TestUnparsedEndpoint_ImplausibleCostContributesNothing is the regression.
//
// Nothing about the record may read as money: not the figure, not the settled bit, not the
// micros conversion every consumer accumulates in, and not costevent.Priced, which is the
// admission guard the aggregator's Decode and the ledger writer's own gate both go through.
func TestUnparsedEndpoint_ImplausibleCostContributesNothing(t *testing.T) {
	for _, header := range []string{"1000000000", "10000.000001", "1e13"} {
		for _, site := range unparsedSites() {
			t.Run(header+"/"+site.name, func(t *testing.T) {
				p := NewInferenceParser()
				p.SetPricingResolver(bodylessRates(t))
				pctx := unparsedCtx("/v1/embeddings", header)

				site.drive(p, pctx)

				ev, ok := publishedCost(t, pctx)
				if !ok {
					t.Fatal("no record published at all; a refusal that reaches no record makes a forged $1,000,000,000 identical to a response that reported no cost, hiding both the misconfiguration and the attack")
				}
				if ev.CostUSD != 0 {
					t.Errorf("CostUSD = %v, want 0 — refused, not clamped to the cap: a clamped figure is a wrong number wearing a right label and the ledger keeps it for thirty days", ev.CostUSD)
				}
				if ev.Settled {
					t.Error("Settled = true; a refused figure is not an answer, and a settled zero would suppress every consumer's own fallback")
				}
				if ev.Priced() {
					t.Error("Priced() = true; this is the predicate the ledger writer and the aggregator both admit on, so a true here is the 429 and the poisoned ledger row")
				}
				if ev.Micros() != 0 {
					t.Errorf("Micros() = %d, want 0", ev.Micros())
				}
				// VISIBLE AS A GAP. The record exists precisely so the refusal is
				// nameable rather than silent.
				if ev.RejectedReason != costevent.RejectedImplausible {
					t.Errorf("RejectedReason = %q, want %q", ev.RejectedReason, costevent.RejectedImplausible)
				}
				// And the state a later plugin in the same request reads.
				settled, loaded := costing.Load(pctx)
				if !loaded {
					t.Fatal("costing.Load found nothing; a consumer cannot tell 'no figure' from 'this pass never ran'")
				}
				if settled.Priced || settled.CostUSD != 0 {
					t.Errorf("stashed state is priced: %+v — litellm_budgettrack bills off this, so a figure here is a ledger row and a daily total", settled)
				}
				if settled.RejectedReason != costevent.RejectedImplausible {
					t.Errorf("stashed RejectedReason = %q, want %q", settled.RejectedReason, costevent.RejectedImplausible)
				}
			})
		}
	}
}

// TestUnparsedEndpoint_PlausibleCostStillSettles is the guard against fixing the hole by
// breaking the feature that opened it.
//
// LiteLLM prices /v1/embeddings and stamps the same cost header on it as on a completion.
// That spend has no inference extension behind it, so it settles from the header alone —
// and a cap that swallowed it would trade a forged-figure bug for a missing-money bug, which
// is the defect unparsed_cost_test.go exists for.
//
// Walked to the cap's edge, not just at a typical figure: $10,000 exactly must still settle.
func TestUnparsedEndpoint_PlausibleCostStillSettles(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   float64
	}{
		{"0.0042", 0.0042},
		{"20", 20},
		{"10000", 10000},
	} {
		for _, site := range unparsedSites() {
			t.Run(tc.header+"/"+site.name, func(t *testing.T) {
				p := NewInferenceParser()
				p.SetPricingResolver(bodylessRates(t))
				pctx := unparsedCtx("/v1/embeddings", tc.header)

				site.drive(p, pctx)

				ev, ok := publishedCost(t, pctx)
				if !ok {
					t.Fatalf("no cost record for a plausible header of %s; gateway-priced embeddings spend reaches neither /v1/usage, the ledger, nor the budget", tc.header)
				}
				if ev.CostUSD != tc.want {
					t.Errorf("CostUSD = %v, want %v", ev.CostUSD, tc.want)
				}
				if !ev.Priced() {
					t.Error("Priced() = false; a consumer would re-price or ignore real spend")
				}
				if ev.RejectedReason != "" {
					t.Errorf("RejectedReason = %q on a figure that settled; the cap is refusing bills a real call could produce", ev.RejectedReason)
				}
			})
		}
	}
}

// TestParsedEndpoint_ImplausibleCostIsUnchanged states the scope in an executable form.
//
// A hostile host serving an inference-shaped path could always forge a figure, and the cap
// deliberately does not touch that: it is a blast-radius bound on traffic nothing
// corroborated, not authentication, and the fix for the narrower hole is an allowlist of
// hosts whose cost headers are believed at all.
//
// OnRequest is driven for real so the extension is populated the way production populates it,
// rather than by a test author asserting an equivalent state. If a later change caps this
// path too, this test fails and the scope decision gets made deliberately instead of by
// accident.
func TestParsedEndpoint_ImplausibleCostIsUnchanged(t *testing.T) {
	const forged = 1e9
	if pricing.PlausibleRequestCostUSD(forged) {
		t.Fatal("the fixture is under the cap; this test would prove nothing about the parsed path")
	}
	p := NewInferenceParser()
	p.SetPricingResolver(bodylessRates(t))
	pctx := unparsedCtx("/v1/chat/completions", "1000000000")
	pctx.Body = []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`)

	pctx.SetCurrentPlugin("inference-parser", pipeline.InvocationPhaseRequest)
	p.OnRequest(context.Background(), pctx)
	if pctx.Extensions.Inference == nil {
		t.Fatal("OnRequest left the extension nil; this test no longer exercises the PARSED path")
	}
	pctx.SetCurrentPlugin("inference-parser", pipeline.InvocationPhaseResponse)

	p.OnResponseFrame(context.Background(), pctx, []byte(`{"id":"x"}`), true)

	ev, ok := publishedCost(t, pctx)
	if !ok {
		t.Fatal("no cost record on the parsed path")
	}
	if ev.CostUSD != forged {
		t.Errorf("CostUSD = %v, want %v — a parsed endpoint keeps today's behaviour, unchanged and out of this fix's scope", ev.CostUSD, forged)
	}
	if ev.RejectedReason != "" {
		t.Errorf("RejectedReason = %q; the cap must key on the nil extension, which is exactly the set of responses whose spend this PR newly admitted", ev.RejectedReason)
	}
}

// TestUnparsedEndpoint_RefusalIsPublishedExactlyOnce holds the latch on the new publish
// path.
//
// A listener can dispatch the terminal frame more than once — extproc settles once per
// ResponseBody message Envoy sends, see settleCost. The refusal record is the first thing
// this plugin publishes with no figure in it, so
// the latch is exercised on a shape it has never carried before — and a second publish would
// double a disclosure, which is how a coverage report grows requests that never happened.
func TestUnparsedEndpoint_RefusalIsPublishedExactlyOnce(t *testing.T) {
	p := NewInferenceParser()
	p.SetPricingResolver(bodylessRates(t))
	pctx := unparsedCtx("/v1/embeddings", "1000000000")

	p.OnResponseFrame(context.Background(), pctx, nil, true)
	first, ok := publishedCost(t, pctx)
	if !ok {
		t.Fatal("no record on the first terminal frame")
	}

	p.OnResponseFrame(context.Background(), pctx, nil, true)
	p.OnResponse(context.Background(), pctx)

	second, _ := publishedCost(t, pctx)
	if second.CostUSD != first.CostUSD || second.RejectedReason != first.RejectedReason || second.Settled != first.Settled {
		t.Errorf("the record changed across repeated terminal dispatches:\nfirst  %+v\nsecond %+v", first, second)
	}
	if second.RejectedReason != costevent.RejectedImplausible {
		t.Errorf("RejectedReason = %q after the repeat dispatches, want %q — a latch that dropped the disclosure would be as wrong as one that doubled it", second.RejectedReason, costevent.RejectedImplausible)
	}
}

// TestUnparsedEndpoint_NoHeaderStillPublishesNothing is the negative control for the new
// publish arm, and it is what stops the fix inflating the coverage denominator.
//
// The refusal is now a reason to publish a record where nothing else is. That must remain
// scoped to a refusal: the same pipeline handles MCP calls, health checks and CONNECT
// tunnels, and a record for each of those would put non-inference traffic in the cost
// denominator — the mistake that made a correct deployment read "1/10 priced" forever.
func TestUnparsedEndpoint_NoHeaderStillPublishesNothing(t *testing.T) {
	for _, path := range []string{"/healthz", "/mcp", "/some/tunnel"} {
		for _, site := range unparsedSites() {
			t.Run(path+"/"+site.name, func(t *testing.T) {
				p := NewInferenceParser()
				p.SetPricingResolver(bodylessRates(t))
				pctx := unparsedCtx(path, "")

				site.drive(p, pctx)

				if ev, ok := publishedCost(t, pctx); ok {
					t.Errorf("published %+v for %s; only a REFUSED figure justifies a record with no money in it", ev, path)
				}
			})
		}
	}
}
