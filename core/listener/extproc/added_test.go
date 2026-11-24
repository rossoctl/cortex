package extproc

import (
	"context"
	"testing"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// slowProbePlugin is ttfbProbePlugin with request-side work: it takes d on every request,
// standing in for the proxy's own time before Envoy forwards.
type slowProbePlugin struct {
	ttfbProbePlugin
	d time.Duration
}

func (p slowProbePlugin) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	// timing: this stands in for real request-side work; the test measures the resulting
	// elapsed Added duration, so the wait must be real.
	time.Sleep(p.d)
	return pipeline.Action{Type: pipeline.Continue}
}

// slowOriginStream delays the response-headers message by origin: in ext_proc the origin's
// time is the gap between Cortex answering the request phase and Envoy delivering the
// response headers.
type slowOriginStream struct {
	*mockStream
	origin time.Duration
}

func (s *slowOriginStream) Recv() (*extprocv3.ProcessingRequest, error) {
	if s.recvIdx < len(s.requests) && s.requests[s.recvIdx].GetResponseHeaders() != nil {
		// timing: stands in for the origin's real wait before its headers; the test measures
		// the resulting elapsed Added/Ttfb split, so the wait must be real.
		time.Sleep(s.origin)
	}
	return s.mockStream.Recv()
}

// TestExtProc_ResponseRowCarriesAdded is the proxy listeners' CarriesAdded test on ext_proc:
// Added covers the pipeline's request-side work and stops when the request phase is answered
// (Envoy forwards then), so the origin's wait before its headers is in Ttfb only.
func TestExtProc_ResponseRowCarriesAdded(t *testing.T) {
	const work, origin = 30 * time.Millisecond, 120 * time.Millisecond
	p, err := pipeline.New([]pipeline.Plugin{slowProbePlugin{d: work}})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	empty, err := pipeline.New(nil)
	if err != nil {
		t.Fatalf("pipeline.New(nil): %v", err)
	}
	store := session.New(0, 100, 100)
	t.Cleanup(func() { store.Close() })
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), InboundPipeline: pipeline.NewHolder(empty), Sessions: store}

	stream := &slowOriginStream{mockStream: ttfbProbeStream(srv, true), origin: origin}
	_ = srv.Process(stream)
	if stream.recvIdx != 2 {
		t.Fatalf("consumed %d of 2 messages; the listener bailed", stream.recvIdx)
	}

	ev := responseEvent(t, store)
	if ev == nil {
		t.Fatal("no outbound response row recorded")
	}
	if ev.Added < work {
		t.Errorf("added = %v, want ≥ %v — the plugin's work ran before Envoy forwarded", ev.Added, work)
	}
	if ev.Added >= origin {
		t.Errorf("added = %v, want < %v — the origin's wait is not the proxy's", ev.Added, origin)
	}
	if ev.Ttfb-ev.Added < origin/2 {
		t.Errorf("ttfb−added = %v, want ≥ %v — the origin waited %v after the request phase", ev.Ttfb-ev.Added, origin/2, origin)
	}
}
