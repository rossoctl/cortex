package extproc

import (
	"context"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// ttfbProbePlugin publishes one plugin-public Custom entry and asks for no
// body in either direction, so a pipeline holding it has NeedsBody() false —
// the arm where handleResponseHeaders records the response row itself because
// the body phase is never requested. The entry is what makes the row exist at
// all: recordOutboundResponseSession gates on a protocol payload, an
// Invocation or a Custom entry, and this test needs the row without any of
// the body-reading plugins that would move the recording to the body phase.
type ttfbProbePlugin struct{}

func (ttfbProbePlugin) Name() string { return "ttfb-probe" }
func (ttfbProbePlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (ttfbProbePlugin) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (ttfbProbePlugin) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if pctx.Extensions.Custom == nil {
		pctx.Extensions.Custom = map[string]any{}
	}
	pctx.Extensions.Custom["ttfb-probe/event"] = map[string]any{"seen": true}
	return pipeline.Action{Type: pipeline.Continue}
}

// newTtfbProbeServer builds a Server whose outbound pipeline holds only the
// probe above, so a response is recorded on the headers path rather than the
// body phase.
func newTtfbProbeServer(t *testing.T) (*Server, *session.Store) {
	t.Helper()
	p, err := pipeline.New([]pipeline.Plugin{ttfbProbePlugin{}})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	if p.NeedsBody() {
		t.Fatal("NeedsBody() = true for a pipeline holding only the probe; this file's premise is the body-free arm")
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

// ttfbProbeStream drives one outbound exchange: a bodyless request, then
// response headers with the given end_of_stream. Driving Server.Process is
// the listener's real message loop, same choice as server_headeronly_test.go.
func ttfbProbeStream(srv *Server, endOfStream bool) *mockStream {
	reqs := []*extprocv3.ProcessingRequest{
		{Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{
				Headers: makeHeaders(
					"x-authbridge-direction", "outbound",
					":method", "GET",
					":path", "/health",
					":authority", "internal.local",
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
				),
				EndOfStream: endOfStream,
			},
		}},
	}
	return &mockStream{ctx: context.Background(), requests: reqs}
}

// TestExtProc_HeadersRecordedRowWithBodyComingOmitsTtfb pins the arm where a
// body-free pipeline makes handleResponseHeaders record the row itself while
// the body is still coming: Envoy relays it without this listener ever seeing
// the bytes, so Duration stops at the headers and a ttfb measured there would
// equal Duration — agentop would render "body 0ms" for a body that crossed
// Envoy, the exact misreading #1251's split exists to prevent. The row
// reports NO ttfb instead: absent is the contract's "not measured".
func TestExtProc_HeadersRecordedRowWithBodyComingOmitsTtfb(t *testing.T) {
	srv, store := newTtfbProbeServer(t)

	stream := ttfbProbeStream(srv, false) // headers only; the body is still coming
	_ = srv.Process(stream)
	if stream.recvIdx != 2 {
		t.Fatalf("consumed %d of 2 messages; the listener bailed", stream.recvIdx)
	}
	// No ModeOverride on the response-headers reply is what puts this exchange
	// on the arm under test: the body phase was never requested, so the row was
	// recorded at the headers.
	if mo := stream.responses[1].GetModeOverride(); mo != nil {
		t.Fatalf("the response-header phase asked Envoy for a body (%v); this test's premise is the body-free arm", mo.GetResponseBodyMode())
	}

	ev := responseEvent(t, store)
	if ev == nil {
		t.Fatal("no outbound response row recorded; the probe's Custom entry should have qualified it")
	}
	if ev.Ttfb != 0 {
		t.Errorf("response row ttfb = %v, want absent (0) — the body is relayed by Envoy and never seen here, so duration−ttfb would read as a zero body it did not measure", ev.Ttfb)
	}
}

// TestExtProc_HeadersRecordedRowOnEndOfStreamCarriesTtfb is the positive
// control: when the headers END the stream there is no body at all, so
// headers arrival is the whole answer and the row carries a real ttfb —
// distinguishing "not measured" (above) from "measured, and it is
// everything".
func TestExtProc_HeadersRecordedRowOnEndOfStreamCarriesTtfb(t *testing.T) {
	srv, store := newTtfbProbeServer(t)

	stream := ttfbProbeStream(srv, true) // headers end the stream: a bodyless response
	_ = srv.Process(stream)
	if stream.recvIdx != 2 {
		t.Fatalf("consumed %d of 2 messages; the listener bailed", stream.recvIdx)
	}

	ev := responseEvent(t, store)
	if ev == nil {
		t.Fatal("no outbound response row recorded; the probe's Custom entry should have qualified it")
	}
	if ev.Ttfb <= 0 {
		t.Errorf("response row ttfb = %v, want > 0 — the headers ended the stream, so their arrival is a real, reportable figure", ev.Ttfb)
	}
}
