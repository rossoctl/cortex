package extproc

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/rossoctl/cortex/authbridge/authlib/listener/skiphost"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins/plugintesting"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
)

// markerPlugin records one Invocation per OnRequest call so tests can
// assert whether the outbound pipeline ran. Mirrors the helper in the
// forwardproxy skiphost tests; kept local to avoid a public test-only
// type in plugintesting.
type markerPlugin struct {
	calls atomic.Int32
}

func (p *markerPlugin) Name() string                              { return "marker" }
func (p *markerPlugin) Capabilities() pipeline.PluginCapabilities { return pipeline.PluginCapabilities{} }
func (p *markerPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *markerPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.calls.Add(1)
	pctx.Record(pipeline.Invocation{
		Plugin: "marker",
		Action: pipeline.ActionObserve,
		Phase:  pipeline.InvocationPhaseRequest,
		Reason: "ran",
	})
	return pipeline.Action{Type: pipeline.Continue}
}

func newSkipServer(t *testing.T, store *session.Store, skip *skiphost.Matcher) (*Server, *markerPlugin) {
	t.Helper()
	inbound, err := plugintesting.BuildPipeline([]pipeline.Plugin{})
	if err != nil {
		t.Fatalf("building inbound pipeline: %v", err)
	}
	mp := &markerPlugin{}
	outbound, err := plugintesting.BuildPipeline([]pipeline.Plugin{mp})
	if err != nil {
		t.Fatalf("building outbound pipeline: %v", err)
	}
	return &Server{
		InboundPipeline:  pipeline.NewHolder(inbound),
		OutboundPipeline: pipeline.NewHolder(outbound),
		Sessions:         store,
		SkipHosts:        skip,
	}, mp
}

// TestExtProc_SkipHosts_OutboundBypass asserts the headers-only outbound
// path: a SkipHosts-matched destination produces zero plugin invocations
// and zero session events, and the response is a plain pass-through.
// The motivating case is OTel-collector traffic in envoy-sidecar
// deployments — without this gate, every export from the agent would
// run the pipeline and append a session event, evicting the inbound
// A2A user intent from the FIFO buffer.
func TestExtProc_SkipHosts_OutboundBypass(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	// Match the agent's exgentic-style FQDN pattern: a leading-* glob
	// against the fixed suffix is the operator-friendly way to write
	// this and matches the hostname after net.SplitHostPort strips the
	// :8335 from pctx.Host.
	skip, err := skiphost.New([]string{"otel-collector.*.svc.cluster.local"})
	if err != nil {
		t.Fatalf("skiphost.New: %v", err)
	}

	srv, mp := newSkipServer(t, store, skip)

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			outboundRequest(makeHeaders(
				":authority", "otel-collector.rossoctl-system.svc.cluster.local:8335",
				":path", "/v1/traces",
			)),
		},
	}

	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	rh := stream.responses[0].GetRequestHeaders()
	if rh == nil {
		t.Fatal("expected RequestHeaders pass-through response")
	}
	if rh.Response != nil && rh.Response.HeaderMutation != nil &&
		len(rh.Response.HeaderMutation.SetHeaders) > 0 {
		t.Error("skipped host must not have header mutations (pipeline did not run)")
	}
	if mp.calls.Load() != 0 {
		t.Errorf("pipeline ran %d times; want 0 — SkipHosts must short-circuit before pipeline.Run", mp.calls.Load())
	}
	if sessions := store.ListSessions(); len(sessions) != 0 {
		t.Errorf("%d session(s) recorded; want 0 — SkipHosts must skip recording entirely", len(sessions))
	}
}

// TestExtProc_SkipHosts_NonMatchingRunsPipeline is the regression guard:
// with a SkipHosts list set, hosts that don't match must still run the
// pipeline and have their Invocation recorded. Without this pairing,
// the bypass test above could pass trivially with a globally disabled
// pipeline.
func TestExtProc_SkipHosts_NonMatchingRunsPipeline(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	skip, err := skiphost.New([]string{"otel-collector*"})
	if err != nil {
		t.Fatalf("skiphost.New: %v", err)
	}

	srv, mp := newSkipServer(t, store, skip)

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			outboundRequest(makeHeaders(
				":authority", "github-tool-mcp:8000",
				":path", "/mcp",
			)),
		},
	}

	_ = srv.Process(stream)

	if mp.calls.Load() != 1 {
		t.Errorf("pipeline ran %d times; want 1 — host did not match skip list", mp.calls.Load())
	}
	if sessions := store.ListSessions(); len(sessions) != 1 {
		t.Errorf("session count = %d; want 1 — Invocation should drive recording for non-skipped hosts", len(sessions))
	}
}

// TestExtProc_SkipHosts_NilMatcherPreservesBehavior asserts the
// upgrade-safety contract: a Server without SkipHosts (nil Matcher)
// behaves identically to today's code. Pipeline runs, sessions record.
func TestExtProc_SkipHosts_NilMatcherPreservesBehavior(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	srv, mp := newSkipServer(t, store, nil)

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			outboundRequest(makeHeaders(
				":authority", "any-service",
				":path", "/",
			)),
		},
	}

	_ = srv.Process(stream)

	if mp.calls.Load() != 1 {
		t.Errorf("nil SkipHosts: pipeline ran %d times, want 1", mp.calls.Load())
	}
}
