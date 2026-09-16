package inferenceparser

import (
	"context"
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/costing"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// THE FOURTH PATH. bodyless_cost_test.go covers the three finalization paths where the
// parser understood the REQUEST and could not read the RESPONSE BODY. This file covers
// the one where the request itself was never parsed, so Extensions.Inference is nil:
// /v1/embeddings, /v1/rerank, /v1/moderations, any other endpoint the gateway mounts,
// and any request whose body was empty or not JSON.
//
// Same money bug, same shape, one guard further out. Both response hooks returned on
// that nil before they reached settleCost, so a gateway-reported cost on any of those
// calls was recorded NOWHERE — absent from /v1/usage, absent from the durable ledger,
// and absent from litellm-budget-track, which amends a settled record rather than
// computing its own and therefore enforced no budget over spend that produced no record.
//
// The negative row matters as much as the positive ones: settling on every path must not
// manufacture a record for the MCP, health-check and tunnel traffic the same proxy
// handles, or the coverage denominator inflates and a correct deployment reads as
// partially priced.

// unparsedCtx builds a response context for a request the parser never parsed:
// Extensions.Inference is nil, exactly as OnRequest leaves it.
//
// path is carried so a failure names the endpoint, and because it is the field the old
// request-side allowlist keyed on — the whole point being that the outcome no longer
// depends on it.
func unparsedCtx(path, costHeader string) *pipeline.Context {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if costHeader != "" {
		h.Set(costing.ResponseCostHeader, costHeader)
	}
	pctx := &pipeline.Context{
		Direction:       pipeline.Outbound,
		Host:            "gw.internal",
		Path:            path,
		ResponseHeaders: h,
		// NO Inference extension. This is the state under test.
	}
	pctx.SetCurrentPlugin("inference-parser", pipeline.InvocationPhaseResponse)
	return pctx
}

// invocationRows counts every invocation recorded on pctx, of any action.
//
// Used for the assertion that the nil-extension path records NONE. skipRows would not
// catch a spurious Observe row, and an Observe naming a model that does not exist is the
// specific mistake available here.
func invocationRows(pctx *pipeline.Context) int {
	if pctx.Extensions.Invocations == nil {
		return 0
	}
	return len(pctx.Extensions.Invocations.Outbound) + len(pctx.Extensions.Invocations.Inbound)
}

// unparsedSite is one of the two response hooks that guarded on a nil extension, named
// and driven separately so a mutation neutering one is not covered by the other.
type unparsedSite struct {
	name  string
	drive func(p *InferenceParser, pctx *pipeline.Context)
}

func unparsedSites() []unparsedSite {
	return []unparsedSite{{
		// The production path under every listener: this plugin is a StreamingResponder,
		// so RunResponse skips it and the terminal frame is the only dispatch it sees.
		name: "OnResponseFrame/terminal-frame",
		drive: func(p *InferenceParser, pctx *pipeline.Context) {
			p.OnResponseFrame(context.Background(), pctx, []byte(`{"data":[]}`), true)
		},
	}, {
		// The buffered hook, reachable from a pipeline that calls OnResponse directly.
		name: "OnResponse/buffered",
		drive: func(p *InferenceParser, pctx *pipeline.Context) {
			pctx.ResponseBody = []byte(`{"data":[]}`)
			p.OnResponse(context.Background(), pctx)
		},
	}}
}

// TestUnparsedEndpoint_GatewayCostIsCharged is the regression proper: an endpoint this
// parser has no dialect for still spends money, and the gateway says how much in a
// response header that needs neither a model nor a body to read.
//
// /v1/embeddings is the concrete instance, and it is not hypothetical: LiteLLM prices
// embeddings and stamps the same cost header on them as on a completion.
func TestUnparsedEndpoint_GatewayCostIsCharged(t *testing.T) {
	for _, path := range []string{"/v1/embeddings", "/v1/rerank", "/v1/moderations"} {
		for _, site := range unparsedSites() {
			t.Run(path+"/"+site.name, func(t *testing.T) {
				p := NewInferenceParser()
				p.SetPricingResolver(bodylessRates(t))
				pctx := unparsedCtx(path, "0.0042")

				site.drive(p, pctx)

				ev, ok := publishedCost(t, pctx)
				if !ok {
					t.Fatalf("no cost record published for %s: the gateway reported a cost and it is now recorded nowhere — not in /v1/usage, not in the ledger, and not in litellm-budget-track, which amends a settled record rather than settling its own, so this spend escapes budget enforcement entirely", path)
				}
				if ev.CostUSD != 0.0042 {
					t.Errorf("CostUSD = %v, want the header's 0.0042", ev.CostUSD)
				}
				if ev.Source != costevent.SourceGatewayHeader {
					t.Errorf("Source = %q, want %q", ev.Source, costevent.SourceGatewayHeader)
				}
				if !ev.Priced() {
					t.Error("Priced() = false; a consumer would treat this as unpriced traffic and re-price or ignore it")
				}
				if ev.Incomplete {
					t.Errorf("Incomplete = true (%q); a gateway-reported figure is what the call charged, whatever counters we did or did not parse", ev.IncompleteReason)
				}
			})
		}
	}
}

// TestUnparsedEndpoint_RecordsNoInvocationRow pins the diagnostic decision that goes
// with the charge, which is the opposite of the one the body-less paths make.
//
// Those paths record Skip("no_response_body") because the body really was missing. Here
// the body may be perfectly well-formed and simply not a dialect this parser reads, so
// that reason would be a false diagnostic — and there is no model to name in a matched_
// Observe row. OnRequest records nothing for this traffic either, so charging it must not
// be what starts putting every MCP call and health check in the invocation timeline.
func TestUnparsedEndpoint_RecordsNoInvocationRow(t *testing.T) {
	for _, site := range unparsedSites() {
		t.Run(site.name, func(t *testing.T) {
			p := NewInferenceParser()
			p.SetPricingResolver(bodylessRates(t))
			pctx := unparsedCtx("/v1/embeddings", "0.0042")

			site.drive(p, pctx)

			if n := invocationRows(pctx); n != 0 {
				t.Errorf("invocation rows = %d, want 0: %+v", n, pctx.Extensions.Invocations)
			}
		})
	}
}

// TestUnparseableBody_GatewayCostIsCharged is the same bug reached the other way: a
// RECOGNISED path whose body the parser could not decode.
//
// OnRequest is driven for real rather than hand-waved, so the test pins the actual
// consequence of the ext == nil arm instead of a state a test author asserted was
// equivalent to it. An empty body and a non-JSON body are both covered, because they are
// two different arms of parseOpenAIRequest and the Anthropic parser has the pair as well.
func TestUnparseableBody_GatewayCostIsCharged(t *testing.T) {
	bodies := map[string][]byte{
		"empty":    nil,
		"not-json": []byte("<html>502 Bad Gateway</html>"),
	}
	for _, path := range []string{"/v1/chat/completions", anthropicMessagesPath} {
		for name, body := range bodies {
			for _, site := range unparsedSites() {
				t.Run(path+"/"+name+"/"+site.name, func(t *testing.T) {
					p := NewInferenceParser()
					p.SetPricingResolver(bodylessRates(t))
					pctx := unparsedCtx(path, "0.0042")
					pctx.Body = body

					// The request pass is what leaves the extension nil. Driven, not assumed.
					pctx.SetCurrentPlugin("inference-parser", pipeline.InvocationPhaseRequest)
					p.OnRequest(context.Background(), pctx)
					if pctx.Extensions.Inference != nil {
						t.Fatalf("OnRequest populated Extensions.Inference from a %s body; this test no longer exercises the nil-extension path", name)
					}
					pctx.SetCurrentPlugin("inference-parser", pipeline.InvocationPhaseResponse)

					site.drive(p, pctx)

					ev, ok := publishedCost(t, pctx)
					if !ok {
						t.Fatalf("no cost record published: a %s request body on %s does not make the gateway's charge disappear, and this spend reaches neither /v1/usage, the ledger, nor the budget", name, path)
					}
					if ev.CostUSD != 0.0042 {
						t.Errorf("CostUSD = %v, want the header's 0.0042", ev.CostUSD)
					}
					if ev.Source != costevent.SourceGatewayHeader {
						t.Errorf("Source = %q, want %q", ev.Source, costevent.SourceGatewayHeader)
					}
				})
			}
		}
	}
}

// TestUnparsedEndpoint_NoCostHeaderPublishesNothing is the negative case, and it is what
// stops this fix inflating the denominator.
//
// The proxy handles MCP calls, health checks and CONNECT tunnels through the same
// pipeline, and every one of them now reaches settleCost. None may produce a record: with
// no extension there is no usage to model, so pricing has nothing to price and
// settleCost's own gate finds neither a figure nor a saving to publish. A record here
// would count non-inference traffic as priceable and make a correct deployment read as
// partially priced forever.
//
// Rates ARE injected, so nothing could price this except a rule that declines to.
func TestUnparsedEndpoint_NoCostHeaderPublishesNothing(t *testing.T) {
	for _, path := range []string{"/v1/embeddings", "/healthz", "/mcp", "/some/tunnel"} {
		for _, site := range unparsedSites() {
			t.Run(path+"/"+site.name, func(t *testing.T) {
				p := NewInferenceParser()
				p.SetPricingResolver(bodylessRates(t))
				pctx := unparsedCtx(path, "")

				site.drive(p, pctx)

				if ev, ok := publishedCost(t, pctx); ok {
					t.Errorf("published %+v for %s; a response with no cost header has nothing to say, and a record here would put non-inference traffic in the coverage denominator", ev, path)
				}
				if n := invocationRows(pctx); n != 0 {
					t.Errorf("invocation rows = %d, want 0", n)
				}
			})
		}
	}
}

// TestUnparsedEndpoint_NonTerminalFrameSettlesNothing pins the `last` gate.
//
// An unparsed endpoint settles where every other path does — at end of stream — rather
// than on whichever frame arrived first. Without the gate the first frame of a streamed
// response would publish, and while the idempotence guard means it would publish only
// once, it would publish at a different point in the request lifecycle from every other
// path and before a listener has necessarily finished with the response.
//
// The terminal frame in the same context is then asserted to settle, so this cannot pass
// by the settling being broken outright.
func TestUnparsedEndpoint_NonTerminalFrameSettlesNothing(t *testing.T) {
	p := NewInferenceParser()
	p.SetPricingResolver(bodylessRates(t))
	pctx := unparsedCtx("/v1/embeddings", "0.0042")

	p.OnResponseFrame(context.Background(), pctx, []byte(`{"partial":true}`), false)

	if ev, ok := publishedCost(t, pctx); ok {
		t.Fatalf("published %+v on a non-terminal frame; an unparsed endpoint must settle at end of stream, like every other path", ev)
	}

	p.OnResponseFrame(context.Background(), pctx, nil, true)

	if _, ok := publishedCost(t, pctx); !ok {
		t.Fatal("no cost record after the terminal frame: the non-terminal assertion above passes vacuously if nothing ever settles")
	}
}

// TestUnparsedEndpoint_SettlesExactlyOnce guards the money against the listener behaviour
// settleCost's idempotence key exists for: a repeated terminal dispatch, which extproc
// produces once per ResponseBody message when Envoy delivers a body in more than one (see
// settleCost for why the old "once for headers, once for the body" reading was wrong).
//
// Asserted on this path specifically because it is the path with no extension to inspect —
// the other paths' finalize functions are assignments and are self-idempotent, while here
// the ONLY thing standing between two dispatches and two charges is that key.
func TestUnparsedEndpoint_SettlesExactlyOnce(t *testing.T) {
	p := NewInferenceParser()
	p.SetPricingResolver(bodylessRates(t))
	pctx := unparsedCtx("/v1/embeddings", "0.0042")

	p.OnResponseFrame(context.Background(), pctx, nil, true)
	first, ok := publishedCost(t, pctx)
	if !ok {
		t.Fatal("no cost record published on the first terminal frame")
	}

	// The second terminal dispatch. Also the buffered hook, since a
	// pipeline that ran both must not charge twice either.
	p.OnResponseFrame(context.Background(), pctx, nil, true)
	p.OnResponse(context.Background(), pctx)

	second, _ := publishedCost(t, pctx)
	if second.CostUSD != first.CostUSD {
		t.Errorf("CostUSD moved from %v to %v across repeated terminal dispatches; a double charge is not recoverable from a later correction", first.CostUSD, second.CostUSD)
	}
}

// TestUnparsedEndpoint_DeclaredFreeZeroIsSettled keeps this path's rule identical to the
// one the body-less paths already follow.
//
// A non-streamed response carrying an exactly-zero cost header is the gateway SAYING the
// call was free — an embeddings cache hit, or an error it declined to charge for. Publishing
// nothing would let a consumer fall through to its own rate table and fabricate a cost for
// it. There is no usage to fabricate one FROM on this path today, which is precisely why
// the rule has to be pinned here rather than left to follow from that: the protection is
// the settled zero, not the absence of counters.
func TestUnparsedEndpoint_DeclaredFreeZeroIsSettled(t *testing.T) {
	p := NewInferenceParser()
	p.SetPricingResolver(bodylessRates(t))
	pctx := unparsedCtx("/v1/embeddings", "0")

	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ev, ok := publishedCost(t, pctx)
	if !ok {
		t.Fatal("no cost record published; a gateway-declared free call is an ANSWER, and publishing nothing invites a consumer to re-price it")
	}
	if ev.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want 0", ev.CostUSD)
	}
	if !ev.Settled {
		t.Error("Settled = false; an unsettled zero reads as unpriced, which is the state that let a cache hit be billed")
	}
	if !ev.Priced() {
		t.Error("Priced() = false; a settled zero must count as priced so no consumer re-prices it")
	}
}

// TestUnparsedEndpoint_StreamPlaceholderZeroPublishesNothing is the sibling that makes the
// test above mean something.
//
// On a text/event-stream response the same "0" header is LiteLLM's placeholder — the total
// is unknown when headers are sent — so treating it as an answer would publish a settled
// zero for every streamed response on an unparsed endpoint and count unpriced traffic as
// free. Same header value, same path, opposite outcome, separated only by Content-Type.
func TestUnparsedEndpoint_StreamPlaceholderZeroPublishesNothing(t *testing.T) {
	p := NewInferenceParser()
	p.SetPricingResolver(bodylessRates(t))
	pctx := unparsedCtx("/v1/embeddings", "0")
	pctx.ResponseHeaders.Set("Content-Type", "text/event-stream")

	p.OnResponseFrame(context.Background(), pctx, nil, true)

	if ev, ok := publishedCost(t, pctx); ok {
		t.Errorf("published %+v; a stream's zero cost header is a placeholder, not a declaration of free", ev)
	}
}

// TestUnparsedEndpoint_OriginalCostHeaderIsRead covers the header LiteLLM actually sends on
// the Anthropic-shaped path, where the bare one is absent.
//
// It is the fallback in costing.headerCost, and without it budget tracking silently records
// $0 for every request of that shape. Pinned on the unparsed path too, so a change that
// narrowed the fallback could not leave this path reading only the bare header.
func TestUnparsedEndpoint_OriginalCostHeaderIsRead(t *testing.T) {
	p := NewInferenceParser()
	p.SetPricingResolver(bodylessRates(t))
	pctx := unparsedCtx("/v1/embeddings", "")
	pctx.ResponseHeaders.Set(costing.ResponseCostOriginalHeader, "0.0042")

	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ev, ok := publishedCost(t, pctx)
	if !ok {
		t.Fatal("no cost record published from the -Original header; that is the only cost header LiteLLM sends on some paths, so ignoring it records $0 for real spend")
	}
	if ev.CostUSD != 0.0042 {
		t.Errorf("CostUSD = %v, want 0.0042", ev.CostUSD)
	}
}
