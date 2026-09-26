package inferenceparser

import (
	"context"
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/costevent"
	"github.com/rossoctl/cortex/core/costing"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/pricing"
)

// A body-less response is not a free response. The gateway reports what it charged
// in a RESPONSE HEADER, so every path that gives up on the body must still settle
// the cost — otherwise a LiteLLM-costed response whose body was empty or
// unrecognised is spend that reaches neither the aggregator nor the budget ledger.
//
// Each of the parser's three no_response_body paths is exercised twice: once with a
// positive header (must charge) and once with no header at all (must publish
// nothing, rather than a settled zero for every body-less response). Both halves
// also assert the Skip row survives — the fix must not trade the diagnostic for the
// charge.

// bodylessRates prices every tier at 1 micro-dollar per token.
//
// A REAL table is injected deliberately, not a nil resolver. With rates in hand the
// "no header" rows prove that publication is suppressed because pricing.Cost refuses
// an all-zero Usage as unpriced, not merely because nothing could price anything.
func bodylessRates(t *testing.T) pricing.Resolver {
	t.Helper()
	var r pricing.Rates
	for _, tier := range []pricing.Tier{
		pricing.TierInput, pricing.TierCacheWrite, pricing.TierCacheRead, pricing.TierOutput,
	} {
		r.Base[tier], r.Set[tier] = 1e-6, true
	}
	tab, err := pricing.NewTable([]pricing.Entry{
		{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured},
	})
	if err != nil {
		t.Fatal(err)
	}
	return pricing.NewRegistry(tab)
}

// publishedCost reads the cost record off pctx, if one was published.
//
// Reads the canonical key rather than the legacy plugin-name alias: costing.Publish
// writes both, and asserting on the concern-named one is what keeps this test honest
// about which key a consumer is meant to read.
func publishedCost(t *testing.T, pctx *pipeline.Context) (costevent.Event, bool) {
	t.Helper()
	raw, ok := pctx.Extensions.Custom[costevent.Key+pipeline.PluginEventSuffix]
	if !ok {
		return costevent.Event{}, false
	}
	ev, ok := raw.(costevent.Event)
	if !ok {
		t.Fatalf("cost record has type %T, want costevent.Event", raw)
	}
	return ev, true
}

// skipRows counts the no_response_body Skip invocations recorded on pctx.
func skipRows(pctx *pipeline.Context) int {
	if pctx.Extensions.Invocations == nil {
		return 0
	}
	var n int
	for _, list := range [][]pipeline.Invocation{
		pctx.Extensions.Invocations.Outbound, pctx.Extensions.Invocations.Inbound,
	} {
		for _, iv := range list {
			if iv.Action == pipeline.ActionSkip && iv.Reason == "no_response_body" {
				n++
			}
		}
	}
	return n
}

// bodylessSite is one of the parser's three no_response_body paths, named and
// driven. Named so a failure — including a mutation that neuters exactly one
// settleCost call — says which site is uncovered rather than only that something is.
type bodylessSite struct {
	name string
	// stream is what the REQUEST asked for, which is what selects the arm inside
	// OnResponseFrame. It is not a property of the response.
	stream bool
	// drive delivers a body-less response to the site under test.
	drive func(p *InferenceParser, pctx *pipeline.Context)
}

func bodylessSites() []bodylessSite {
	return []bodylessSite{{
		// plugin.go OnResponse: the legacy buffered hook, reachable from a pipeline
		// that calls OnResponse directly rather than through RunResponse.
		name: "OnResponse/empty-ResponseBody",
		drive: func(p *InferenceParser, pctx *pipeline.Context) {
			p.OnResponse(context.Background(), pctx)
		},
	}, {
		// plugin.go OnResponseFrame, one-shot arm: the listener buffered the response and
		// there was nothing in it. This is the arm a 204/304 takes on every listener. On
		// extproc it is reached from the response-HEADERS phase, which dispatches the terminal
		// frame when Envoy sets end_of_stream there — a header-only response never produces a
		// body message to hang it off.
		name: "OnResponseFrame/json-one-shot-empty-frame",
		drive: func(p *InferenceParser, pctx *pipeline.Context) {
			p.OnResponseFrame(context.Background(), pctx, nil, true)
		},
	}, {
		// plugin.go OnResponseFrame, streaming arm: a stream that finalized with no
		// completion, no finish reason, no usage and no tool calls.
		//
		// THE STREAMING SITE HAS TO BE DRIVEN AS A STREAM. `OnResponseFrame(nil, true)`
		// alone is byte-identical to the one-shot site above it: with no earlier frame there
		// is no stream state, so the terminal call takes the `state == nil` branch and this
		// table exercises two of the three sites it claims. Mutation-verified in both directions: deleting the settle in the
		// streaming arm passed all 59 packages of core, while deleting the one in the
		// arm below it failed two tests, so the method was sound and this row was the hole.
		//
		// A NON-TERMINAL FRAME FIRST is what creates the state (see the `!last` branch:
		// existence of the scratch is what tells the terminal frame a stream ran). The frame
		// is a real Anthropic `ping` event, which the folder ignores by design — so the
		// stream demonstrably RAN and demonstrably carried nothing, which is the production
		// shape this arm exists for: an SSE keepalive followed by a turn that was cancelled
		// or died before message_start. An empty non-terminal frame would create the state
		// too, but a zero-length SSE event is not something a listener dispatches.
		name:   "OnResponseFrame/empty-stream",
		stream: true,
		drive: func(p *InferenceParser, pctx *pipeline.Context) {
			p.OnResponseFrame(context.Background(), pctx, []byte(`{"type":"ping"}`), false)
			p.OnResponseFrame(context.Background(), pctx, nil, true)
		},
	}}
}

// bodylessCtx builds a response context with no body and the given cost header.
func bodylessCtx(costHeader string, stream bool) *pipeline.Context {
	h := http.Header{}
	// A header-only reply to a streaming request is not itself an event stream, so
	// application/json is the honest content type on all three sites. It also keeps
	// a zero header meaningful, which text/event-stream would not — see
	// costing.IsEventStream.
	h.Set("Content-Type", "application/json")
	if costHeader != "" {
		h.Set(costing.ResponseCostHeader, costHeader)
	}
	pctx := &pipeline.Context{
		Direction:       pipeline.Outbound,
		Host:            "gw.internal",
		Path:            "/v1/messages",
		ResponseHeaders: h,
		Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{
			Model:  "claude-opus-5",
			Stream: stream,
		}},
	}
	pctx.SetCurrentPlugin("inference-parser", pipeline.InvocationPhaseResponse)
	return pctx
}

// TestBodylessResponse_PositiveCostHeaderIsCharged is the regression proper: every
// no_response_body path must publish the gateway's figure.
//
// One subtest per site, so neutering one settleCost call fails exactly one row and
// the row's name says which site lost its charge.
func TestBodylessResponse_PositiveCostHeaderIsCharged(t *testing.T) {
	for _, site := range bodylessSites() {
		t.Run(site.name, func(t *testing.T) {
			p := NewInferenceParser()
			p.SetPricingResolver(bodylessRates(t))
			pctx := bodylessCtx("0.25", site.stream)

			site.drive(p, pctx)

			ev, ok := publishedCost(t, pctx)
			if !ok {
				t.Fatal("no cost record published: a positive gateway cost header on a body-less response is real spend and went unrecorded")
			}
			if ev.CostUSD != 0.25 {
				t.Errorf("CostUSD = %v, want the header's 0.25", ev.CostUSD)
			}
			if ev.Source != costevent.SourceGatewayHeader {
				t.Errorf("Source = %q, want %q", ev.Source, costevent.SourceGatewayHeader)
			}
			if !ev.Settled {
				t.Error("Settled = false; an authoritative gateway figure is settled")
			}
			if !ev.Priced() {
				t.Error("Priced() = false; the aggregator would fall through to its own rate table")
			}
			// The charge must not cost us the diagnostic. Exactly one Skip row: the
			// response row abctl pairs with the request row.
			if n := skipRows(pctx); n != 1 {
				t.Errorf("no_response_body Skip rows = %d, want 1", n)
			}
		})
	}
}

// TestBodylessResponse_NoCostHeaderPublishesNothing is the other half of the fix:
// settling on these paths must not manufacture a settled zero for every body-less
// response, which would count unpriced traffic as priced.
//
// The suppression is load-bearing on pricing.Cost refusing an all-zero Usage — rates
// ARE injected here, so nothing could price this except a rule that declines to.
func TestBodylessResponse_NoCostHeaderPublishesNothing(t *testing.T) {
	for _, site := range bodylessSites() {
		t.Run(site.name, func(t *testing.T) {
			p := NewInferenceParser()
			p.SetPricingResolver(bodylessRates(t))
			pctx := bodylessCtx("", site.stream)

			site.drive(p, pctx)

			if ev, ok := publishedCost(t, pctx); ok {
				t.Errorf("published %+v; a body-less response with no cost header has nothing to say, and a settled zero would report unpriced traffic as free", ev)
			}
			// Whatever settling decided, the pairing row is still owed.
			if n := skipRows(pctx); n != 1 {
				t.Errorf("no_response_body Skip rows = %d, want 1", n)
			}
		})
	}
}

// TestBodylessResponse_ZeroUsageIsUnpriced pins the property the fix above relies on,
// at the level it actually holds: pricing.Cost treats an all-zero Usage as UNPRICED,
// not as a cost of zero.
//
// Asserted directly rather than inferred from the parser's behaviour, because it is
// the reason settling on a body-less path is safe at all. If this ever became
// (0, true), every body-less response would publish a settled zero and the aggregate
// would count unpriced traffic as priced — so this test is the tripwire for that
// change, wherever it is made.
func TestBodylessResponse_ZeroUsageIsUnpriced(t *testing.T) {
	var r pricing.Rates
	for _, tier := range []pricing.Tier{
		pricing.TierInput, pricing.TierCacheWrite, pricing.TierCacheRead, pricing.TierOutput,
	} {
		r.Base[tier], r.Set[tier] = 1e-6, true
	}
	if micros, ok := pricing.Cost(r, pricing.Usage{}); ok {
		t.Errorf("pricing.Cost(rates, zero usage) = (%d, true), want ok=false: unknown usage is not a free request", micros)
	}
}

// TestBodylessResponse_DeclaredFreeZeroIsPublishedAsSettled pins the third header state,
// which is a real behaviour change of this fix and not merely a side effect of it.
//
// A non-streamed response carrying an exactly-zero cost header is the gateway SAYING the
// call was free — a cache hit, or an error it declined to charge for. Before the fix these
// paths published nothing at all, so a downstream consumer saw no record, fell through to
// its own rate table, and could fabricate a cost for a call the gateway had explicitly
// declared free. A settled zero suppresses that fallback, which is the whole reason
// costevent.Event.Settled exists.
//
// So the assertion is specifically that Settled is TRUE while the cost is zero. A record
// with cost 0 and Settled false would be worse than no record: costevent.Priced() reads it
// as unpriced, which is the state that invited the fabrication.
func TestBodylessResponse_DeclaredFreeZeroIsPublishedAsSettled(t *testing.T) {
	for _, site := range bodylessSites() {
		t.Run(site.name, func(t *testing.T) {
			p := NewInferenceParser()
			p.SetPricingResolver(bodylessRates(t))
			pctx := bodylessCtx("0", site.stream)

			site.drive(p, pctx)

			ev, ok := publishedCost(t, pctx)
			if !ok {
				t.Fatal("no cost record published; a gateway-declared free call is an ANSWER, and publishing nothing lets a consumer re-price it from its own table")
			}
			if ev.CostUSD != 0 {
				t.Errorf("CostUSD = %v, want 0", ev.CostUSD)
			}
			if !ev.Settled {
				t.Error("Settled = false; an unsettled zero reads as unpriced, which is exactly the state that let a cache hit be billed")
			}
			if !ev.Priced() {
				t.Error("Priced() = false; a settled zero must count as priced so no consumer re-prices it")
			}
			if ev.Source != costevent.SourceGatewayHeader {
				t.Errorf("Source = %q, want %q", ev.Source, costevent.SourceGatewayHeader)
			}
			// A declared-free call is an EXACT total, so it must carry no inexactness
			// caveat — this is the gate in costing.Settle that keys on the source rather
			// than on "the header was not positive".
			if ev.Incomplete {
				t.Errorf("Incomplete = true (%q); the gateway stating it charged nothing is an exact total, not a lower bound on nothing", ev.IncompleteReason)
			}
			if n := skipRows(pctx); n != 1 {
				t.Errorf("no_response_body Skip rows = %d, want 1", n)
			}
		})
	}
}

// The sibling of the case above, and the reason it has to be tested as a PAIR: on a
// streamed response the very same "0" header means nothing at all. LiteLLM stamps it by
// design because the total is unknown when headers are sent, so treating it as an answer
// would publish a settled zero for every streamed response and count unpriced traffic as
// free.
//
// Same header value, same body-less path, opposite outcome — separated only by
// Content-Type. Without this row, a change that dropped costing's IsEventStream check
// would still pass the declared-free test above.
//
// It also covers "a stream that died before ANY usage arrived", which incomplete_cost_test.go
// asserted separately with an identical context: no counters and a placeholder zero publish
// nothing, and a caveat on nothing would count unpriced traffic as partially priced.
func TestBodylessResponse_StreamPlaceholderZeroPublishesNothing(t *testing.T) {
	p := NewInferenceParser()
	p.SetPricingResolver(bodylessRates(t))
	pctx := bodylessCtx("0", true)
	// The one thing that changes the meaning of the header.
	pctx.ResponseHeaders.Set("Content-Type", "text/event-stream")

	p.OnResponseFrame(context.Background(), pctx, nil, true)

	if ev, ok := publishedCost(t, pctx); ok {
		t.Errorf("published %+v; a stream's zero cost header is a placeholder, not a declaration of free, and publishing it as settled would count unpriced traffic as free", ev)
	}
	if n := skipRows(pctx); n != 1 {
		t.Errorf("no_response_body Skip rows = %d, want 1", n)
	}
}

// TestCapabilities_ReadsBodyDecidesTheExtprocBranch pins the premise the comments above and
// in settleCost now rest on, and the reason the body-less fix does not reach extproc.
//
// ReadsBody is undirected, so it counts toward NeedsRequestBody and NeedsResponseBody alike
// (pipeline.NeedsRequestBody says why). Any pipeline containing this parser therefore reports
// NeedsBody() == true unconditionally — which is why extproc's response-header phase cannot
// gate its early return on that condition alone. It gates on `NeedsBody() && !endOfStream`, so
// a response with no body at all — 204, 304, an error status ended on headers — takes the
// terminal dispatch from the HEADERS phase, and a stream that ends before any end_of_stream
// arrives is finalized by the flush in Process. What this test pins is the capability shape
// those two paths rest on.
//
// This test fails if ReadsBody is dropped, which would silently move which branch of that
// listener runs.
func TestCapabilities_ReadsBodyDecidesTheExtprocBranch(t *testing.T) {
	p := NewInferenceParser()
	if !p.Capabilities().ReadsBody {
		t.Fatal("ReadsBody = false; the listeners would stop buffering response bodies and every token count would vanish")
	}
	pipe, err := pipeline.New([]pipeline.Plugin{p})
	if err != nil {
		t.Fatalf("New pipeline: %v", err)
	}
	if !pipe.NeedsBody() {
		t.Error("NeedsBody() = false for a pipeline holding only this parser; extproc would take its header-only response branch instead of asking Envoy to buffer")
	}
	if !pipe.NeedsResponseBody() {
		t.Error("NeedsResponseBody() = false; the response body would never be buffered")
	}
	if !pipe.HasStreamingResponders() {
		t.Error("HasStreamingResponders() = false; the listeners would call OnResponse instead of OnResponseFrame and no terminal frame would ever arrive")
	}
}
