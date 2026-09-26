package extproc

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/rossoctl/cortex/core/auth"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/plugintesting"
)

// traceRewriterPlugin mimics a pipeline plugin's header writes — a tracestate
// stamp (wire contract v1.5), a traceparent rewrite to prove the mechanism is
// not stamp-specific, and arbitrary set/delete to prove it is not trace-specific
// either (static-inject's x-api-key is the upstream case). Used to assert the
// listener forwards plugin header writes as mutations: before withHeaderMutation
// everything but Authorization died in pctx.Headers (inert on the wire).
type traceRewriterPlugin struct {
	traceparent string
	tracestate  string
	set         map[string]string
	del         []string
	setNil      []string          // pctx.Headers[k] = nil — a delete spelled without Del
	setRaw      map[string]string // direct map assignment, key stored verbatim (no canonicalisation)
	readsBody   bool
}

func (p *traceRewriterPlugin) Name() string { return "trace-rewriter" }
func (p *traceRewriterPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{ReadsBody: p.readsBody}
}
func (p *traceRewriterPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if p.traceparent != "" {
		pctx.Headers.Set("traceparent", p.traceparent)
	}
	if p.tracestate != "" {
		pctx.Headers.Set("tracestate", p.tracestate)
	}
	for k, v := range p.set {
		pctx.Headers.Set(k, v)
	}
	for _, k := range p.del {
		pctx.Headers.Del(k)
	}
	for _, k := range p.setNil {
		pctx.Headers[http.CanonicalHeaderKey(k)] = nil
	}
	for k, v := range p.setRaw {
		pctx.Headers[k] = []string{v} // verbatim key — bypasses http.Header canonicalisation
	}
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *traceRewriterPlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func traceRewriterServer(t *testing.T, plugin pipeline.Plugin) *Server {
	t.Helper()
	outbound, err := pipeline.New([]pipeline.Plugin{plugin})
	if err != nil {
		t.Fatal(err)
	}
	inbound, err := plugintesting.BuildPipeline([]pipeline.Plugin{plugintesting.NewJWTValidation(auth.New(auth.Config{}), false)})
	if err != nil {
		t.Fatal(err)
	}
	return &Server{InboundPipeline: pipeline.NewHolder(inbound), OutboundPipeline: pipeline.NewHolder(outbound)}
}

// mutationHeaderValue returns the mutation value for key, or "" when absent.
// (setHeaderValue in placeholder_test.go unwraps a full RequestHeaders
// response; this one takes the bare mutation so body-phase responses can
// share it.)
func mutationHeaderValue(hm *extprocv3.HeaderMutation, key string) string {
	if hm == nil {
		return ""
	}
	for _, sh := range hm.SetHeaders {
		if sh.Header != nil && sh.Header.Key == key {
			return string(sh.Header.RawValue)
		}
	}
	return ""
}

// TestExtProc_Outbound_TraceRewriteReachesWire: a plugin rewrite of the outbound
// traceparent/tracestate must be emitted as SetHeaders on the headers-phase
// response — this is what puts the plugin's stamp on the wire.
func TestExtProc_Outbound_TraceRewriteReachesWire(t *testing.T) {
	const newTP = "00-4bf92f3577b34da6a3ce929d0e0e4736-aaaaaaaaaaaaaaaa-01"
	const newTS = "dg-parent=aaaaaaaaaaaaaaaa"
	srv := traceRewriterServer(t, &traceRewriterPlugin{traceparent: newTP, tracestate: newTS})

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			outboundRequest(makeHeaders(
				":authority", "fanin-echo",
				"traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
				"tracestate", "dg-parent=00f067aa0ba902b7",
			)),
		},
	}
	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	rh := stream.responses[0].GetRequestHeaders()
	if rh == nil || rh.Response == nil || rh.Response.HeaderMutation == nil {
		t.Fatalf("expected HeadersResponse with header mutation, got %+v", stream.responses[0])
	}
	if got := mutationHeaderValue(rh.Response.HeaderMutation, "traceparent"); got != newTP {
		t.Errorf("traceparent mutation = %q, want %q (trace rewrite lost on the wire)", got, newTP)
	}
	if got := mutationHeaderValue(rh.Response.HeaderMutation, "tracestate"); got != newTS {
		t.Errorf("tracestate mutation = %q, want %q", got, newTS)
	}
}

// TestExtProc_OutboundBody_TraceRewriteReachesWire: same guarantee on the
// body-phase path (a ReadsBody plugin defers the pipeline to the body
// message; the trace-header diff must ride that response instead).
func TestExtProc_OutboundBody_TraceRewriteReachesWire(t *testing.T) {
	const newTP = "00-4bf92f3577b34da6a3ce929d0e0e4736-bbbbbbbbbbbbbbbb-01"
	srv := traceRewriterServer(t, &traceRewriterPlugin{traceparent: newTP, readsBody: true})

	body := []byte(`{"jsonrpc":"2.0"}`)
	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			outboundRequest(makeHeaders(
				":authority", "fanin-echo",
				"traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
				"content-length", fmt.Sprintf("%d", len(body)),
			)),
			{Request: &extprocv3.ProcessingRequest_RequestBody{
				RequestBody: &extprocv3.HttpBody{Body: body},
			}},
		},
	}
	_ = srv.Process(stream)

	if len(stream.responses) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(stream.responses))
	}
	rb := stream.responses[1].GetRequestBody()
	if rb == nil || rb.Response == nil || rb.Response.HeaderMutation == nil {
		t.Fatalf("expected RequestBody response with header mutation, got %+v", stream.responses[1])
	}
	if got := mutationHeaderValue(rb.Response.HeaderMutation, "traceparent"); got != newTP {
		t.Errorf("traceparent mutation = %q, want %q (trace rewrite lost on the body path)", got, newTP)
	}
}

// TestExtProc_Outbound_UnchangedTraceHeadersEmitNothing: when no plugin
// touches the trace headers, the listener must not emit mutations for them
// (echoing an unchanged header back would be a silent no-op today but
// masks diff regressions).
func TestExtProc_Outbound_UnchangedTraceHeadersEmitNothing(t *testing.T) {
	srv := traceRewriterServer(t, &traceRewriterPlugin{})

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			outboundRequest(makeHeaders(
				":authority", "fanin-echo",
				"traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
				"tracestate", "dg-parent=00f067aa0ba902b7",
			)),
		},
	}
	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	rh := stream.responses[0].GetRequestHeaders()
	if rh == nil {
		t.Fatal("expected HeadersResponse")
	}
	if rh.Response != nil && rh.Response.HeaderMutation != nil {
		hm := rh.Response.HeaderMutation
		if v := mutationHeaderValue(hm, "traceparent"); v != "" {
			t.Errorf("unexpected traceparent mutation %q on unchanged header", v)
		}
		if v := mutationHeaderValue(hm, "tracestate"); v != "" {
			t.Errorf("unexpected tracestate mutation %q on unchanged header", v)
		}
	}
}

// mutationRemovesHeader reports whether hm removes key.
func mutationRemovesHeader(hm *extprocv3.HeaderMutation, key string) bool {
	if hm == nil {
		return false
	}
	for _, rh := range hm.RemoveHeaders {
		if rh == key {
			return true
		}
	}
	return false
}

// TestExtProc_Outbound_ArbitraryHeaderReachesWire: the sync is generic — a
// plugin injecting any header (static-inject's x-api-key is the upstream
// case) must reach the wire, not just the trace headers.
func TestExtProc_Outbound_ArbitraryHeaderReachesWire(t *testing.T) {
	srv := traceRewriterServer(t, &traceRewriterPlugin{
		set: map[string]string{"x-api-key": "secret-value"},
	})

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			outboundRequest(makeHeaders(":authority", "fanin-echo")),
		},
	}
	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	rh := stream.responses[0].GetRequestHeaders()
	if rh == nil || rh.Response == nil || rh.Response.HeaderMutation == nil {
		t.Fatalf("expected HeadersResponse with header mutation, got %+v", stream.responses[0])
	}
	if got := mutationHeaderValue(rh.Response.HeaderMutation, "x-api-key"); got != "secret-value" {
		t.Errorf("x-api-key mutation = %q, want %q (injected header dropped)", got, "secret-value")
	}
}

// TestExtProc_Outbound_DeletedHeaderIsRemoved: a plugin deleting a header
// must emit RemoveHeaders — the narrow two-name diff could not express this.
func TestExtProc_Outbound_DeletedHeaderIsRemoved(t *testing.T) {
	srv := traceRewriterServer(t, &traceRewriterPlugin{del: []string{"x-drop-me"}})

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			outboundRequest(makeHeaders(":authority", "fanin-echo", "x-drop-me", "present")),
		},
	}
	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	rh := stream.responses[0].GetRequestHeaders()
	if rh == nil || rh.Response == nil || rh.Response.HeaderMutation == nil {
		t.Fatalf("expected HeadersResponse with header mutation, got %+v", stream.responses[0])
	}
	if !mutationRemovesHeader(rh.Response.HeaderMutation, "x-drop-me") {
		t.Errorf("expected x-drop-me in RemoveHeaders, got %+v", rh.Response.HeaderMutation)
	}
}

// TestExtProc_Outbound_PseudoHeadersNeverEmitted: headerMapToHTTP copies the
// HTTP/2 pseudo-headers into pctx.Headers (Go's canonicaliser leaves
// ":"-prefixed keys alone). Emitting a mutation for :authority would rewrite
// routing, so the sync must skip them even while emitting a real change.
func TestExtProc_Outbound_PseudoHeadersNeverEmitted(t *testing.T) {
	srv := traceRewriterServer(t, &traceRewriterPlugin{
		set: map[string]string{"x-api-key": "secret-value"},
	})

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			outboundRequest(makeHeaders(
				":authority", "fanin-echo",
				":method", "POST",
				":path", "/rpc",
			)),
		},
	}
	_ = srv.Process(stream)

	rh := stream.responses[0].GetRequestHeaders()
	if rh == nil || rh.Response == nil || rh.Response.HeaderMutation == nil {
		t.Fatalf("expected HeadersResponse with header mutation, got %+v", stream.responses[0])
	}
	for _, pseudo := range []string{":authority", ":method", ":path", ":scheme"} {
		if got := mutationHeaderValue(rh.Response.HeaderMutation, pseudo); got != "" {
			t.Errorf("pseudo-header %s emitted as %q — would rewrite routing", pseudo, got)
		}
		if mutationRemovesHeader(rh.Response.HeaderMutation, pseudo) {
			t.Errorf("pseudo-header %s emitted in RemoveHeaders", pseudo)
		}
	}
}

// TestExtProc_Outbound_LowercaseContentHeaderStillSkipped: the transport-header
// exclusion must be case-insensitive. Content-Length / Content-Encoding are
// owned by the body-rewrite block and the transport, so the sync must never
// forward them regardless of key casing. Every production caller reaches
// pctx.Headers through http.Header, which canonicalises to "Content-Encoding",
// so a plugin writing the key verbatim by raw map assignment is the only way a
// non-canonical spelling arrives — exactly the case an exact-string compare
// would miss and strings.EqualFold catches.
func TestExtProc_Outbound_LowercaseContentHeaderStillSkipped(t *testing.T) {
	srv := traceRewriterServer(t, &traceRewriterPlugin{
		setRaw: map[string]string{"content-encoding": "gzip"},
	})

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			outboundRequest(makeHeaders(
				":authority", "fanin-echo",
				":path", "/rpc",
			)),
		},
	}
	_ = srv.Process(stream)

	rh := stream.responses[0].GetRequestHeaders()
	if rh == nil {
		t.Fatal("expected HeadersResponse")
	}
	if rh.Response != nil && rh.Response.HeaderMutation != nil {
		hm := rh.Response.HeaderMutation
		if v := mutationHeaderValue(hm, "content-encoding"); v != "" {
			t.Errorf("lowercase content-encoding emitted as %q — transport header leaked past the skip filter", v)
		}
		if mutationRemovesHeader(hm, "content-encoding") {
			t.Error("lowercase content-encoding emitted in RemoveHeaders — transport header should be left untouched")
		}
	}
}

// TestExtProc_Outbound_NilValueHeaderIsRemoved: pctx.Headers[k] = nil is a
// delete spelled without Del(k). It must land in RemoveHeaders, not as an
// empty SetHeaders value — Envoy drops empty values only when
// keep_empty_value is unset, which would make the outcome config-dependent.
func TestExtProc_Outbound_NilValueHeaderIsRemoved(t *testing.T) {
	srv := traceRewriterServer(t, &traceRewriterPlugin{setNil: []string{"x-drop-me"}})

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			outboundRequest(makeHeaders(":authority", "fanin-echo", "x-drop-me", "present")),
		},
	}
	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	rh := stream.responses[0].GetRequestHeaders()
	if rh == nil || rh.Response == nil || rh.Response.HeaderMutation == nil {
		t.Fatalf("expected HeadersResponse with header mutation, got %+v", stream.responses[0])
	}
	hm := rh.Response.HeaderMutation
	if !mutationRemovesHeader(hm, "x-drop-me") {
		t.Errorf("expected x-drop-me in RemoveHeaders, got %+v", hm)
	}
	for _, sh := range hm.SetHeaders {
		if sh.Header != nil && sh.Header.Key == "x-drop-me" {
			t.Errorf("x-drop-me emitted as SetHeaders %q — must be a removal", string(sh.Header.RawValue))
		}
	}
}

// inboundRewriterServer wires the rewriter plugin into the INBOUND pipeline.
// handleInbound/handleInboundBody took the same withHeaderMutation change as
// the outbound handlers; before these tests their only mutation coverage was
// placeholder_test.go's Authorization case — the one header that already
// worked.
func inboundRewriterServer(t *testing.T, plugin pipeline.Plugin) *Server {
	t.Helper()
	inbound, err := pipeline.New([]pipeline.Plugin{plugin})
	if err != nil {
		t.Fatal(err)
	}
	return &Server{InboundPipeline: pipeline.NewHolder(inbound)}
}

// TestExtProc_Inbound_ArbitraryHeaderReachesWire: the inbound twin of
// TestExtProc_Outbound_ArbitraryHeaderReachesWire.
func TestExtProc_Inbound_ArbitraryHeaderReachesWire(t *testing.T) {
	srv := inboundRewriterServer(t, &traceRewriterPlugin{
		set: map[string]string{"x-api-key": "secret-value"},
	})

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			inboundRequest(makeHeaders("x-authbridge-direction", "inbound", ":path", "/")),
		},
	}
	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	rh := stream.responses[0].GetRequestHeaders()
	if rh == nil || rh.Response == nil || rh.Response.HeaderMutation == nil {
		t.Fatalf("expected HeadersResponse with header mutation, got %+v", stream.responses[0])
	}
	if got := mutationHeaderValue(rh.Response.HeaderMutation, "x-api-key"); got != "secret-value" {
		t.Errorf("x-api-key mutation = %q, want %q (injected header dropped inbound)", got, "secret-value")
	}
}

// TestExtProc_Inbound_DeletedHeaderIsRemoved: the inbound twin of
// TestExtProc_Outbound_DeletedHeaderIsRemoved. The removal must compose with
// allowResponse's own x-authbridge-direction removal rather than replace it.
func TestExtProc_Inbound_DeletedHeaderIsRemoved(t *testing.T) {
	srv := inboundRewriterServer(t, &traceRewriterPlugin{del: []string{"x-drop-me"}})

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			inboundRequest(makeHeaders("x-authbridge-direction", "inbound", ":path", "/", "x-drop-me", "present")),
		},
	}
	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	rh := stream.responses[0].GetRequestHeaders()
	if rh == nil || rh.Response == nil || rh.Response.HeaderMutation == nil {
		t.Fatalf("expected HeadersResponse with header mutation, got %+v", stream.responses[0])
	}
	hm := rh.Response.HeaderMutation
	if !mutationRemovesHeader(hm, "x-drop-me") {
		t.Errorf("expected x-drop-me in RemoveHeaders, got %+v", hm)
	}
	if !mutationRemovesHeader(hm, "x-authbridge-direction") {
		t.Errorf("plugin removal clobbered allowResponse's x-authbridge-direction removal: %+v", hm)
	}
}

// TestExtProc_InboundBody_ArbitraryHeaderReachesWire: the inbound twin of
// TestExtProc_OutboundBody_TraceRewriteReachesWire. A ReadsBody plugin
// defers the inbound pipeline to the body message, so the header diff has
// to ride the RequestBody response instead of the headers one. Before this,
// the only inbound-body coverage was placeholder_test.go's Authorization
// case — the one header the old special case already forwarded.
func TestExtProc_InboundBody_ArbitraryHeaderReachesWire(t *testing.T) {
	srv := inboundRewriterServer(t, &traceRewriterPlugin{
		set:       map[string]string{"x-api-key": "secret-value"},
		readsBody: true,
	})

	body := []byte(`{"jsonrpc":"2.0"}`)
	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			inboundRequest(makeHeaders(
				"x-authbridge-direction", "inbound",
				":path", "/",
				"content-length", fmt.Sprintf("%d", len(body)),
			)),
			{Request: &extprocv3.ProcessingRequest_RequestBody{
				RequestBody: &extprocv3.HttpBody{Body: body},
			}},
		},
	}
	_ = srv.Process(stream)

	if len(stream.responses) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(stream.responses))
	}
	rb := stream.responses[1].GetRequestBody()
	if rb == nil || rb.Response == nil || rb.Response.HeaderMutation == nil {
		t.Fatalf("expected RequestBody response with header mutation, got %+v", stream.responses[1])
	}
	if got := mutationHeaderValue(rb.Response.HeaderMutation, "x-api-key"); got != "secret-value" {
		t.Errorf("x-api-key mutation = %q, want %q (injected header lost on the inbound body path)", got, "secret-value")
	}
}
