package extproc

import (
	"context"
	"net/http"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferenceparser"
	"github.com/rossoctl/cortex/core/session"
)

// A RESPONSE BODY DELIVERED IN MORE THAN ONE MESSAGE WAS BOTH UNDER-CHARGED AND
// DOUBLE-RECORDED, and the two faults hid each other.
//
// This listener runs the whole buffered dispatch — terminal frame included — once per
// ResponseBody message Envoy sends (handleResponseBody -> dispatchBufferedFrames), which
// cost.go's settleCost comment already states. Two consequences nothing was watching:
//
//	UNDER-CHARGE   Every message finalized the parsers. On the Anthropic dialect the first
//	               message carries message_start alone — prompt counted, output not, no stop
//	               reason — so it finalizes into a FLOOR, and the parser's settle latch pins
//	               it. The pass that finally carries message_delta is then short-circuited by
//	               the very guard added to prevent a double charge, and the output tokens are
//	               never billed. On Bedrock output burns quota at 10x, so this is the
//	               expensive half of the bill.
//	DOUBLE-RECORD  Each dispatch also appended a response session event, and the cost record
//	               sits in pctx.Extensions.Custom where nothing clears it — so the SAME figure
//	               was serialised into every event, and the aggregator and the ledger summed
//	               them. One request, N charges.
//
// So the old behaviour on this fixture was 2 x the floor: neither the right figure nor the
// right number of them.
//
// OFF THE SHIPPED CONFIGURATION, and pinned anyway. Buffered mode sends one body message
// with end_of_stream set, which is what handleResponseHeaders asks for via ModeOverride —
// but that is a request to Envoy, not a contract: a statically configured STREAMED response
// body mode, or any filter with allow_mode_override off, produces this. "Correct only while
// Envoy is configured the way we asked" is not a property to leave untested when the failure
// is money.
//
// The test drives Server.Process, so the message loop under test is the real one. Two
// ResponseBody messages is the whole fixture; nothing else about the request is unusual.

// streamedRates prices every tier at 1 micro-dollar per token, so the floor and the whole
// figure are different numbers a test can tell apart.
//
// A REAL table is required here, unlike the header-only file next to this one: litellm
// stamps 0 in the cost header on a streamed response, so the modelled figure from the token
// counters is the only figure available — which is exactly why losing message_delta loses
// money rather than merely losing a caveat.
func streamedRates(t *testing.T) pricing.Resolver {
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

const (
	// 1000 prompt tokens at 1e-6.
	streamedFloorUSD = 0.001
	// Plus 500 output tokens at 1e-6. The figure the request really cost.
	streamedWholeUSD = 0.0015
)

// newStreamedServer builds a Server holding the real inference-parser with rates wired.
func newStreamedServer(t *testing.T) (*Server, *session.Store) {
	t.Helper()
	parser := inferenceparser.NewInferenceParser()
	parser.SetPricingResolver(streamedRates(t))
	p, err := pipeline.New([]pipeline.Plugin{parser})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	empty, err := pipeline.New(nil)
	if err != nil {
		t.Fatalf("pipeline.New(nil): %v", err)
	}
	store := session.New(0, 100, 100)
	t.Cleanup(func() { store.Close() })
	return &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		InboundPipeline:  pipeline.NewHolder(empty),
		Sessions:         store,
	}, store
}

// splitStreamRequests scripts one streamed Anthropic turn whose body arrives in two
// ResponseBody messages, which is what a STREAMED response body mode produces.
//
// The split is at the point that matters and not an arbitrary byte offset: message_start in
// the first message, message_delta — the event carrying both the output tally and the stop
// reason — in the second. That is the shape a mid-stream finalization gets wrong.
func splitStreamRequests() []*extprocv3.ProcessingRequest {
	return []*extprocv3.ProcessingRequest{
		{Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{
				Headers: makeHeaders(
					"x-authbridge-direction", "outbound",
					":method", "POST",
					":path", "/v1/messages",
					":authority", "litellm.local",
					"content-type", "application/json",
					"content-length", "64",
				),
			},
		}},
		{Request: &extprocv3.ProcessingRequest_RequestBody{
			RequestBody: &extprocv3.HttpBody{
				Body: []byte(`{"model":"claude-opus-5","stream":true,` +
					`"messages":[{"role":"user","content":"hi"}]}`),
				EndOfStream: true,
			},
		}},
		{Request: &extprocv3.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extprocv3.HttpHeaders{
				Headers: makeHeaders(
					":status", "200",
					"content-type", "text/event-stream",
					// litellm's placeholder on a stream. A zero header is not a
					// declared-free call here, which is why the modelled figure is
					// the one under test.
					"x-litellm-response-cost", "0",
				),
			},
		}},
		{Request: &extprocv3.ProcessingRequest_ResponseBody{
			ResponseBody: &extprocv3.HttpBody{
				Body: []byte("data: {\"type\":\"message_start\",\"message\":" +
					"{\"usage\":{\"input_tokens\":1000}}}\n\n"),
				// The middle of the body. Everything that must happen once per
				// response hangs off this being false.
				EndOfStream: false,
			},
		}},
		{Request: &extprocv3.ProcessingRequest_ResponseBody{
			ResponseBody: &extprocv3.HttpBody{
				Body: []byte("data: {\"type\":\"message_delta\",\"delta\":" +
					"{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":500}}\n\n" +
					"data: {\"type\":\"message_stop\"}\n\n"),
				EndOfStream: true,
			},
		}},
	}
}

// TestExtProc_SplitResponseBody_ChargesTheWholeFigureExactlyOnce is the regression.
//
// responseEvent fails on more than one match, with the reason spelled out there, so the
// double-record shows up as a failure in this test rather than as a doubled total in a
// ledger nobody is auditing.
func TestExtProc_SplitResponseBody_ChargesTheWholeFigureExactlyOnce(t *testing.T) {
	srv, store := newStreamedServer(t)
	reqs := splitStreamRequests()

	stream := &mockStream{ctx: context.Background(), requests: reqs}
	_ = srv.Process(stream)
	if stream.recvIdx != len(reqs) {
		t.Fatalf("consumed %d of %d messages; the listener bailed", stream.recvIdx, len(reqs))
	}

	ev := responseEvent(t, store)
	if ev == nil {
		t.Fatal("no outbound response row recorded for a split streamed body")
	}
	rec, ok := event.Record(ev)
	if !ok {
		t.Fatalf("no cost record on the response row (Plugins keys = %v)", pluginKeys(ev))
	}

	// THE FIGURE. The floor is the specific wrong answer, so it is named: a bare
	// "want 0.0015" would not say that getting 0.001 means the second message was
	// discarded rather than that the rates moved.
	switch rec.CostUSD {
	case streamedWholeUSD:
	case streamedFloorUSD:
		t.Errorf("CostUSD = %v — the message_start-only FLOOR. The pass carrying message_delta "+
			"was short-circuited by the settle latch, so the output tokens went unbilled",
			rec.CostUSD)
	default:
		t.Errorf("CostUSD = %v, want %v (1000 input + 500 output at 1e-6)", rec.CostUSD, streamedWholeUSD)
	}
	// And it is a whole figure, not a bound. A correct total published as incomplete would
	// still print the "+ partial" marker and the "the real total is higher" language over
	// money that is exact.
	if rec.Incomplete {
		t.Errorf("Incomplete = true with reason %q, over a stream that delivered its stop "+
			"reason and its output tally", rec.IncompleteReason)
	}
	if rec.Source != event.SourceUsageFallback {
		t.Errorf("Source = %q, want %q — a stream's zero cost header is a placeholder, so the "+
			"modelled figure is the authority here", rec.Source, event.SourceUsageFallback)
	}
}

// The frames still reach the parsers on a non-final message — the fix gates the TERMINAL
// frame, not the dispatch.
//
// Without this, "record once" could be satisfied by dropping the earlier messages entirely,
// which would lose the prompt counters that only ever arrive on message_start and turn an
// under-charge into a bigger one. The assertion is the prompt half of the figure above:
// reaching streamedWholeUSD at all requires message_start to have been folded from a message
// that was not the last.
func TestExtProc_SplitResponseBody_NonFinalMessagesStillReachTheParsers(t *testing.T) {
	probe := &streamingRecorderPlugin{}
	p, err := pipeline.New([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{InboundPipeline: pipeline.NewHolder(p), OutboundPipeline: pipeline.NewHolder(p)}

	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		ResponseHeaders: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
	}

	// Two messages, two events each, and only the second is the end of the body.
	srv.handleResponseBody(context.Background(), []byte("data: {\"id\":1}\n\ndata: {\"id\":2}\n\n"),
		pctx, "inbound", false)
	if len(probe.frames) != 2 {
		t.Fatalf("got %d frames from the first message, want 2 — the events must still be "+
			"delivered, or the counters they carry are lost", len(probe.frames))
	}
	for i, last := range probe.lasts {
		if last {
			t.Errorf("frame[%d] last=true on a non-final body message: that is the premature "+
				"finalization the fix removes", i)
		}
	}

	srv.handleResponseBody(context.Background(), []byte("data: {\"id\":3}\n\n"), pctx, "inbound", true)
	if len(probe.frames) != 4 {
		t.Fatalf("got %d frames in total, want 4 (3 events + 1 terminal)", len(probe.frames))
	}
	if !probe.lasts[3] {
		t.Error("the final message did not deliver a terminal frame, so nothing ever finalizes")
	}
	// Exactly one terminal frame across both messages. Two would restore the double
	// finalization even with the recording gated.
	var terminals int
	for _, last := range probe.lasts {
		if last {
			terminals++
		}
	}
	if terminals != 1 {
		t.Errorf("%d terminal frames across a two-message body, want 1", terminals)
	}
}
