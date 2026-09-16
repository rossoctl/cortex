package extproc

import (
	"context"
	"testing"

	extprocfilterv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins/inferenceparser"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
)

// A HEADER-ONLY RESPONSE IS NOT A FREE RESPONSE, and until this file existed
// extproc dropped every one of them.
//
// handleResponseHeaders returned as soon as Pipeline.NeedsBody() was true, and
// inference-parser's undirected ReadsBody makes that true for every shipped
// pipeline (pipeline.NeedsRequestBody explains why ReadsBody counts toward both
// directions). So the RunResponse and the terminal RunResponseFrame(nil, true)
// below that return were unreachable — and for a response with no body at all
// Envoy sends no ResponseBody message either, whatever ModeOverride was asked
// for. Nothing settled the cost and NO RESPONSE ROW WAS RECORDED, on a shape
// that happens in production: a 204, a 304, or an error status ended on headers
// while the gateway still reported what it charged in a response header.
//
// The gate is now the response headers' end_of_stream, mirroring the request
// side's requestHasBody guard at server.go:113 — the symmetry the response path
// was simply missing.
//
// These tests drive Server.Process, so the assertion is about the listener's
// real message loop rather than a handler called by hand. The parser is the real
// one: the premise under test is a property of its capabilities, and a stand-in
// declaring ReadsBody by hand would let the premise drift away from the plugin
// that actually ships.

// headerOnlyCostUSD is what the gateway says it charged. Well under
// pricing.MaxPlausibleRequestCostMicros ($10,000), which costing refuses on an
// unparsed endpoint — a figure over the cap would be rejected and the test would
// then be asserting the refusal path instead of the charge.
const headerOnlyCostUSD = 0.002

// newHeaderOnlyServer builds a Server whose outbound pipeline holds the real
// inference-parser and whose session store is fresh.
//
// rates are deliberately NOT wired: a positive gateway header is authoritative
// and costing.Settle prices it with a nil resolver, so nothing here depends on a
// rate table and the test cannot pass for the wrong reason (a modelled figure
// invented from token counters that a body-less response does not have).
func newHeaderOnlyServer(t *testing.T) (*Server, *session.Store) {
	t.Helper()
	parser := inferenceparser.NewInferenceParser()
	if !parser.Capabilities().ReadsBody {
		t.Fatal("inference-parser ReadsBody = false; the premise of this file no longer holds")
	}
	p, err := pipeline.New([]pipeline.Plugin{parser})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	if !p.NeedsBody() {
		t.Fatal("NeedsBody() = false for a pipeline holding only inference-parser; this file's premise is that it is unconditionally true")
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

// responseEvent returns the sole outbound response-phase event in the default
// bucket. Fails on more than one so a double-record regression surfaces here
// rather than as a doubled charge nobody notices.
func responseEvent(t *testing.T, store *session.Store) *pipeline.SessionEvent {
	t.Helper()
	v := store.View(session.DefaultSessionID)
	if v == nil {
		return nil
	}
	var (
		found   *pipeline.SessionEvent
		matches int
	)
	for i := range v.Events {
		if v.Events[i].Phase == pipeline.SessionResponse && v.Events[i].Direction == pipeline.Outbound {
			found = &v.Events[i]
			matches++
		}
	}
	if matches > 1 {
		t.Fatalf("%d outbound response events recorded; the listener must record exactly once or the request is charged twice", matches)
	}
	return found
}

// TestExtProc_HeaderOnlyResponse_SettlesCostAndRecordsResponseRow is the
// headline case: a response that ends on its headers must still reach the
// pipeline's terminal dispatch, so the cost settles and a response row exists to
// pair with the request row.
//
// No ResponseBody message is sent, because Envoy sends none for a response with
// no body — that omission is the whole point, and a test that synthesized one
// would be testing a message shape that never arrives.
func TestExtProc_HeaderOnlyResponse_SettlesCostAndRecordsResponseRow(t *testing.T) {
	srv, store := newHeaderOnlyServer(t)

	reqs := []*extprocv3.ProcessingRequest{
		{Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{
				Headers: makeHeaders(
					"x-authbridge-direction", "outbound",
					":method", "GET",
					":path", "/v1/embeddings",
					":authority", "litellm.local",
				),
				EndOfStream: true,
			},
		}},
		{Request: &extprocv3.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extprocv3.HttpHeaders{
				Headers: makeHeaders(
					":status", "429",
					"x-litellm-response-cost", "0.002",
				),
				// The one fact that makes this a header-only response.
				EndOfStream: true,
			},
		}},
	}

	stream := &mockStream{ctx: context.Background(), requests: reqs}
	// The error is always the io.EOF the mock returns once its script is
	// exhausted; recvIdx is the real "did the listener stay on the stream"
	// check, same as the parity harness makes.
	_ = srv.Process(stream)
	if stream.recvIdx != len(reqs) {
		t.Fatalf("consumed %d of %d messages; the listener bailed", stream.recvIdx, len(reqs))
	}

	ev := responseEvent(t, store)
	if ev == nil {
		t.Fatal("no outbound response row recorded for a header-only response; the response phase reached neither the pipeline nor the recorder")
	}
	if ev.StatusCode != 429 {
		t.Errorf("row StatusCode = %d, want 429", ev.StatusCode)
	}
	rec, ok := costevent.Record(ev)
	if !ok {
		t.Fatalf("no cost record on the response row (Plugins keys = %v); the gateway reported a charge and nothing settled it", pluginKeys(ev))
	}
	if !rec.Settled {
		t.Errorf("cost record Settled = false; want a settled figure from the gateway header")
	}
	if rec.CostUSD != headerOnlyCostUSD {
		t.Errorf("cost record CostUSD = %v, want %v (the gateway's own header)", rec.CostUSD, headerOnlyCostUSD)
	}
	if rec.Source != costevent.SourceGatewayHeader {
		t.Errorf("cost record Source = %q, want %q", rec.Source, costevent.SourceGatewayHeader)
	}
}

// pluginKeys lists a row's plugin-event keys, so the failure above names what
// WAS published instead of only what was missing.
func pluginKeys(ev *pipeline.SessionEvent) []string {
	keys := make([]string, 0, len(ev.Plugins))
	for k := range ev.Plugins {
		keys = append(keys, k)
	}
	return keys
}

// TestExtProc_ResponseWithBody_StillAsksEnvoyToBuffer guards the OTHER
// direction, and is the reason the fix adds a condition rather than deleting the
// early return.
//
// When the response headers do NOT end the stream there is a body coming, and
// this listener must still return the BUFFERED ModeOverride and dispatch
// nothing: running the pipeline here would settle a cost off headers alone and
// then settle it again off the body, and the terminal frame is what turns a
// stream's folded usage into a figure. Deleting the early return outright passes
// the header-only test above and breaks every response that has a body.
func TestExtProc_ResponseWithBody_StillAsksEnvoyToBuffer(t *testing.T) {
	srv, store := newHeaderOnlyServer(t)

	reqs := []*extprocv3.ProcessingRequest{
		{Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{
				Headers: makeHeaders(
					"x-authbridge-direction", "outbound",
					":method", "GET",
					":path", "/v1/embeddings",
					":authority", "litellm.local",
				),
				EndOfStream: true,
			},
		}},
		{Request: &extprocv3.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extprocv3.HttpHeaders{
				Headers: makeHeaders(
					":status", "200",
					"content-type", "application/json",
					"content-length", "2",
					"x-litellm-response-cost", "0.002",
				),
				EndOfStream: false,
			},
		}},
	}

	stream := &mockStream{ctx: context.Background(), requests: reqs}
	_ = srv.Process(stream) // see the note in the header-only test above
	if stream.recvIdx != len(reqs) {
		t.Fatalf("consumed %d of %d messages; the listener bailed", stream.recvIdx, len(reqs))
	}

	if n := len(stream.responses); n != 2 {
		t.Fatalf("sent %d responses, want 2 (request headers + response headers)", n)
	}
	last := stream.responses[1]
	mode := last.GetModeOverride()
	if mode == nil {
		t.Fatal("no ModeOverride on the response-headers reply; Envoy was never asked to buffer the body")
	}
	if mode.GetResponseBodyMode() != extprocfilterv3.ProcessingMode_BUFFERED {
		t.Errorf("ResponseBodyMode = %v, want BUFFERED", mode.GetResponseBodyMode())
	}
	if ev := responseEvent(t, store); ev != nil {
		t.Errorf("a response row was recorded on the headers phase of a response that has a body still to come; the body phase records it, and both would charge twice (row = %+v)", ev)
	}
}
