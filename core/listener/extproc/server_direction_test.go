package extproc

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
)

// directionCountPlugin counts OnRequest dispatches, so a test can tell
// which chain (inbound or outbound) a request was routed to.
type directionCountPlugin struct {
	calls atomic.Int64
}

func (d *directionCountPlugin) Name() string { return "direction-count" }
func (d *directionCountPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (d *directionCountPlugin) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
	d.calls.Add(1)
	return pipeline.Action{Type: pipeline.Continue}
}
func (d *directionCountPlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// TestExtProc_DuplicateDirectionHeaderRejected: the Envoy inbound listener
// APPENDS x-authbridge-direction (Lua headers():add), so a client that
// pre-set the header leaves two entries — theirs first, and getHeader
// returns the first, routing a forged "outbound" around the inbound chain
// (jwt-validation) entirely. A request carrying more than one entry must be
// rejected before either pipeline runs. The single-entry shapes (one Envoy
// "inbound", none for plain outbound) must keep routing as before.
func TestExtProc_DuplicateDirectionHeaderRejected(t *testing.T) {
	inbound := &directionCountPlugin{}
	outbound := &directionCountPlugin{}
	inP, err := pipeline.New([]pipeline.Plugin{inbound})
	if err != nil {
		t.Fatalf("pipeline.New inbound: %v", err)
	}
	outP, err := pipeline.New([]pipeline.Plugin{outbound})
	if err != nil {
		t.Fatalf("pipeline.New outbound: %v", err)
	}
	srv := &Server{InboundPipeline: pipeline.NewHolder(inP), OutboundPipeline: pipeline.NewHolder(outP)}

	// Forged: the client's "outbound" lands before Envoy's appended
	// "inbound" — the wire shape the Lua headers():add produces.
	stream := &mockStream{ctx: context.Background(), requests: []*extprocv3.ProcessingRequest{
		inboundRequest(makeHeaders(
			"x-authbridge-direction", "outbound",
			"x-authbridge-direction", "inbound",
			":method", "GET",
			":path", "/x",
		)),
	}}
	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	ir := stream.responses[0].GetImmediateResponse()
	if ir == nil {
		t.Fatalf("expected ImmediateResponse, got %+v", stream.responses[0])
	}
	if ir.Status.Code != 400 {
		t.Errorf("status = %d, want 400", ir.Status.Code)
	}
	if inbound.calls.Load() != 0 || outbound.calls.Load() != 0 {
		t.Errorf("pipelines ran (inbound=%d outbound=%d); a forged direction must reach neither",
			inbound.calls.Load(), outbound.calls.Load())
	}

	// Unforged single entry still routes inbound.
	stream = &mockStream{ctx: context.Background(), requests: []*extprocv3.ProcessingRequest{
		inboundRequest(makeHeaders(
			"x-authbridge-direction", "inbound",
			":method", "GET",
			":path", "/x",
		)),
	}}
	_ = srv.Process(stream)
	if inbound.calls.Load() != 1 {
		t.Errorf("single inbound entry: inbound dispatches = %d, want 1", inbound.calls.Load())
	}

	// No entry still routes outbound.
	stream = &mockStream{ctx: context.Background(), requests: []*extprocv3.ProcessingRequest{
		inboundRequest(makeHeaders(
			":method", "GET",
			":path", "/x",
		)),
	}}
	_ = srv.Process(stream)
	if outbound.calls.Load() != 1 {
		t.Errorf("no direction entry: outbound dispatches = %d, want 1", outbound.calls.Load())
	}
}
