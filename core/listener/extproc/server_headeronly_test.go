package extproc

import (
	"context"
	"testing"

	extprocfilterv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferenceparser"
	"github.com/rossoctl/cortex/core/session"
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
// pricing.MaxPlausibleRequestCostMicros ($10,000), which cost/settle refuses on an
// unparsed endpoint — a figure over the cap would be rejected and the test would
// then be asserting the refusal path instead of the charge.
const headerOnlyCostUSD = 0.002

// newHeaderOnlyServer builds a Server whose outbound pipeline holds the real
// inference-parser and whose session store is fresh.
//
// rates are deliberately NOT wired: a positive gateway header is authoritative
// and settle.Settle prices it with a nil resolver, so nothing here depends on a
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

	// THE ROW ALONE DOES NOT SAY WHICH PATH MADE IT, which is how this test stopped discriminating:
	// reverting the !endOfStream guard leaves it green, because the teardown flush added by this PR
	// produces a row too. What distinguishes them is the RESPONSE this listener sent Envoy — asking
	// for a body it will never get, which is what the reverted guard does.
	//
	// responses[1] is the answer to the response-header phase (responses[0] answered the request
	// headers): no ModeOverride means "this phase finished the response", a ModeOverride means "send
	// me the body", and Envoy sends no body for a response that has none.
	if len(stream.responses) < 2 {
		t.Fatalf("listener sent %d responses, want at least 2 (request headers, response headers)", len(stream.responses))
	}
	if mo := stream.responses[1].GetModeOverride(); mo != nil {
		t.Errorf("the response-header phase asked Envoy for a body (%v) on a response that has none: Envoy sends none, so nothing would settle the cost — the row this test then finds comes from the teardown flush instead, which is not the path under test",
			mo.GetResponseBodyMode())
	}

	ev := responseEvent(t, store)
	if ev == nil {
		t.Fatal("no outbound response row recorded for a header-only response; the response phase reached neither the pipeline nor the recorder")
	}
	if ev.StatusCode != 429 {
		t.Errorf("row StatusCode = %d, want 429", ev.StatusCode)
	}
	rec, ok := event.Record(ev)
	if !ok {
		t.Fatalf("no cost record on the response row (Plugins keys = %v); the gateway reported a charge and nothing settled it", pluginKeys(ev))
	}
	if !rec.Settled {
		t.Errorf("cost record Settled = false; want a settled figure from the gateway header")
	}
	if rec.CostUSD != headerOnlyCostUSD {
		t.Errorf("cost record CostUSD = %v, want %v (the gateway's own header)", rec.CostUSD, headerOnlyCostUSD)
	}
	if rec.Source != event.SourceGatewayHeader {
		t.Errorf("cost record Source = %q, want %q", rec.Source, event.SourceGatewayHeader)
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
	// The store is unused now that this test asserts only the ModeOverride; the teardown
	// claim it used to duplicate lives in server_finalize_test.go.
	srv, _ := newHeaderOnlyServer(t)

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
	// NO ROW ASSERTION HERE, deliberately. This test's subject is the ModeOverride: that a
	// response with a body still to come asks Envoy to buffer. What happens when such a stream
	// then ENDS without that body is the teardown flush's claim, and
	// TestExtProc_HeadersThenTeardownStillSettlesTheHeaderCost owns it — asserting it here as
	// well made this test carry two subjects and duplicated that one.
	//
	// The no-double-charge half is not lost: responseEvent fatals on more than one match, and
	// TestExtProc_SplitResponseBody_ChargesTheWholeFigureExactlyOnce drives a body that
	// actually arrives.
}
