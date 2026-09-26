package extproc

import (
	"context"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/rossoctl/cortex/core/costevent"
)

// A RESPONSE THAT ENDS AFTER ITS HEADERS AND BEFORE ANY BODY MESSAGE STILL HAS TO SETTLE.
//
// handleResponseHeaders defers to the body phase whenever the pipeline needs a body and Envoy
// has not set end_of_stream — so on this shape the response phase has not run when the stream
// ends. Gating the teardown flush on "a body message was seen" leaves nothing to finalize it:
// no terminal frame, no session event, and the gateway's own cost header — which arrived on
// those headers — reaches no aggregate, no ledger and no budget.
//
// Reachable whenever the upstream resets after headers, or Envoy tears the stream down before
// forwarding a body: a 200 with a cost header and a body that never came.
func TestExtProc_HeadersThenTeardownStillSettlesTheHeaderCost(t *testing.T) {
	srv, store := newStreamedServer(t)

	reqs := splitStreamRequests()
	// Keep everything up to and including the response headers, and drop both body
	// messages: the response never gets a body phase at all.
	kept := make([]*extprocv3.ProcessingRequest, 0, len(reqs))
	for _, r := range reqs {
		if r.GetResponseBody() != nil {
			continue
		}
		kept = append(kept, r)
	}
	// A positive cost header, so there is a figure to lose. The streamed fixture's own
	// header is litellm's placeholder 0, which settles nothing.
	for _, r := range kept {
		if rh := r.GetResponseHeaders(); rh != nil {
			rh.Headers = makeHeaders(
				":status", "200",
				"content-type", "text/event-stream",
				"x-litellm-response-cost", "0.004",
			)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &cancelOnEndStream{mockStream: &mockStream{ctx: ctx, requests: kept}, cancel: cancel}
	_ = srv.Process(stream)
	if stream.recvIdx != len(kept) {
		t.Fatalf("consumed %d of %d messages; the listener bailed mid-script", stream.recvIdx, len(kept))
	}

	ev := responseEvent(t, store)
	if ev == nil {
		t.Fatal("no outbound response row recorded for a response that ended after its headers: " +
			"the teardown flush is gated on a body message, so this shape finalizes nowhere")
	}
	rec, ok := costevent.Record(ev)
	if !ok {
		t.Fatalf("no cost record on the response row (Plugins keys = %v): the gateway reported "+
			"0.004 on the response headers and it reached no consumer", pluginKeys(ev))
	}
	if rec.CostUSD != 0.004 {
		t.Errorf("CostUSD = %v, want 0.004 from the response header", rec.CostUSD)
	}
}

// A NON-SSE BODY DELIVERED IN MORE THAN ONE MESSAGE IS ONE RESPONSE, NOT N OF THEM.
//
// pctx.ResponseBody is REPLACED per message, and the non-SSE arm of dispatchBufferedFrames
// finalizes whatever it holds — so a JSON body split across two messages parses each fragment
// as a whole response. Neither fragment is valid JSON, so the usage never lands, and the
// parser's settle latch pins the first (empty) answer where a later message cannot correct it.
// The whole body is never seen by anything.
//
// Same shape as the SSE split this PR already fixed, on the arm that was left behind. Off the
// shipped configuration — Envoy is asked for BUFFERED — and reachable exactly where that
// request is not honoured: a statically configured STREAMED response mode, or a filter with
// allow_mode_override off.
func TestExtProc_SplitJSONResponseBodyIsParsedAsOneResponse(t *testing.T) {
	srv, store := newStreamedServer(t)

	// A complete OpenAI-dialect JSON response, cut mid-object so neither half parses alone.
	whole := `{"model":"claude-opus-5","choices":[{"message":{"content":"hi"}}],` +
		`"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`
	cut := len(whole) / 2

	reqs := []*extprocv3.ProcessingRequest{
		{Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{
				// content-length is load-bearing in the fixture, not decoration:
				// requestHasBody reads it, and without it the listener takes its no-body
				// path and the request never reaches the parser at all.
				Headers: makeHeaders(
					"x-authbridge-direction", "outbound",
					":method", "POST",
					":path", "/v1/chat/completions",
					":authority", "litellm.local",
					"content-type", "application/json",
					"content-length", "69",
				),
			},
		}},
		{Request: &extprocv3.ProcessingRequest_RequestBody{
			RequestBody: &extprocv3.HttpBody{
				Body:        []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`),
				EndOfStream: true,
			},
		}},
		{Request: &extprocv3.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extprocv3.HttpHeaders{
				// No cost header at all, so the modelled figure from the token counters is
				// the only one available — which is what a lost body costs.
				Headers: makeHeaders(":status", "200", "content-type", "application/json"),
			},
		}},
		{Request: &extprocv3.ProcessingRequest_ResponseBody{
			ResponseBody: &extprocv3.HttpBody{Body: []byte(whole[:cut]), EndOfStream: false},
		}},
		{Request: &extprocv3.ProcessingRequest_ResponseBody{
			ResponseBody: &extprocv3.HttpBody{Body: []byte(whole[cut:]), EndOfStream: true},
		}},
	}

	stream := &mockStream{ctx: context.Background(), requests: reqs}
	_ = srv.Process(stream)
	if stream.recvIdx != len(reqs) {
		t.Fatalf("consumed %d of %d messages; the listener bailed mid-script", stream.recvIdx, len(reqs))
	}

	ev := responseEvent(t, store)
	if ev == nil {
		t.Fatal("no outbound response row recorded for a split JSON body")
	}
	if ev.Inference == nil {
		t.Fatal("no inference extension on the response row")
	}
	// The whole figure: 1000 prompt + 500 completion at 1 micro-dollar per token.
	if got := ev.Inference.TotalTokens; got != 1500 {
		t.Errorf("TotalTokens = %d, want 1500 — a body split across two messages was parsed as "+
			"two responses, so neither fragment was valid JSON and the usage never landed", got)
	}
	rec, ok := costevent.Record(ev)
	if !ok {
		t.Fatalf("no cost record on the response row (Plugins keys = %v)", pluginKeys(ev))
	}
	if rec.CostUSD != streamedWholeUSD {
		t.Errorf("CostUSD = %v, want %v (1000 + 500 tokens at 1e-6); a fragment-parsed body "+
			"settles whatever the first message could model and latches it", rec.CostUSD,
			streamedWholeUSD)
	}
}
