package extproc

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/rossoctl/cortex/core/listener/skiphost"
	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// removed returns the RemoveHeaders of a request-headers or request-body response.
func removed(t *testing.T, resp *extprocv3.ProcessingResponse) []string {
	t.Helper()
	var cr *extprocv3.CommonResponse
	switch r := resp.Response.(type) {
	case *extprocv3.ProcessingResponse_RequestHeaders:
		cr = r.RequestHeaders.Response
	case *extprocv3.ProcessingResponse_RequestBody:
		cr = r.RequestBody.Response
	}
	if cr == nil || cr.HeaderMutation == nil {
		return nil
	}
	return cr.HeaderMutation.RemoveHeaders
}

// A skip_hosts destination runs no pipeline, but the header is still for Cortex only: both the
// headers phase and the body phase remove it.
func TestNoRecord_ExtProcSkipHostRemovesTheHeader(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	skip, err := skiphost.New([]string{"otel-collector.*.svc.cluster.local"})
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := newSkipServer(t, store, skip)
	hdrs := func() *corev3.HeaderMap {
		return makeHeaders(":authority", "otel-collector.ns.svc.cluster.local:8335", ":path", "/v1/traces",
			"x-cortex-record", "off")
	}

	stream := &mockStream{ctx: context.Background(), requests: []*extprocv3.ProcessingRequest{outboundRequest(hdrs())}}
	_ = srv.Process(stream)
	if len(stream.responses) != 1 || !slices.Contains(removed(t, stream.responses[0]), "x-cortex-record") {
		t.Fatalf("headers phase: responses %d, RemoveHeaders %v, want x-cortex-record removed",
			len(stream.responses), removed(t, stream.responses[0]))
	}

	resp, pctx := srv.handleOutboundBody(&mockStream{ctx: context.Background()}, hdrs(), []byte(`{}`))
	if pctx != nil {
		t.Fatal("body phase ran the pipeline for a skipped host")
	}
	if got := removed(t, resp); !slices.Contains(got, "x-cortex-record") {
		t.Fatalf("body phase: RemoveHeaders %v, want x-cortex-record removed", got)
	}
}

// ext_proc forwards the wire headers and only the changes it emits. ResolveClient removed
// X-Cortex-Record from pctx.Headers before the listener cloned them, so the diff alone cannot
// see it: withHeaderMutation must remove it explicitly, whatever its value.
func TestNoRecord_ExtProcRemovesTheHeader(t *testing.T) {
	for _, v := range []string{"off", "on"} {
		h := http.Header{}
		h.Set(model.RecordHeader, v)
		pctx := &pipeline.Context{Headers: h}
		pctx.ResolveClient()
		orig := pctx.Headers.Clone() // as handleInbound and handleOutbound do
		got := removed(t, withHeaderMutation(allowResponse(), pctx, orig))
		if !slices.Contains(got, "x-cortex-record") {
			t.Fatalf("value %q: RemoveHeaders = %v, want x-cortex-record", v, got)
		}
	}
	// No header: nothing extra is removed.
	pctx := &pipeline.Context{Headers: http.Header{"User-Agent": {"curl/8"}}}
	pctx.ResolveClient()
	if got := removed(t, withHeaderMutation(allowResponse(), pctx, pctx.Headers.Clone())); slices.Contains(got, "x-cortex-record") {
		t.Fatalf("RemoveHeaders = %v with no header sent", got)
	}
}
