package forwardproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/plugintesting"
)

// headerMutatorPlugin performs an arbitrary set / overwrite / delete on
// pctx.Headers during OnRequest. It is the forwardproxy analog of the
// ext_proc traceRewriterPlugin: the header names are deliberately
// ordinary (x-api-key, x-drop-me) rather than Authorization, because the
// point of PR #760 is that EVERY plugin header mutation — not just the
// old Authorization special case — must reach the upstream request. The
// plugin declares no capabilities: a header write does not need
// ReadsBody/WritesRequestBody, mirroring how staticinject/cpex mutate headers.
type headerMutatorPlugin struct {
	set map[string]string // header -> value to Set (set or overwrite)
	del []string          // headers to Del
}

func (p *headerMutatorPlugin) Name() string { return "header-mutator" }
func (p *headerMutatorPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *headerMutatorPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	for k, v := range p.set {
		pctx.Headers.Set(k, v)
	}
	for _, k := range p.del {
		pctx.Headers.Del(k)
	}
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *headerMutatorPlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// headerMutatorFixture wires a forward proxy whose outbound pipeline runs
// the given mutator, plus a backend that captures the headers it actually
// received on the wire.
type headerMutatorFixture struct {
	client     *http.Client
	backendURL string
	headers    func() http.Header
}

// newHeaderMutatorFixture returns a fixture and a cleanup func. The proxy
// dials the httptest backend because the request URL (backendURL) is the
// backend's own URL — the forward-proxy contract. The captured headers
// are what reached the backend AFTER the outbound pipeline + the sync
// block PR #760 added, so asserting on them proves the sync fired.
func newHeaderMutatorFixture(t *testing.T, mut *headerMutatorPlugin) (*headerMutatorFixture, func()) {
	t.Helper()

	gotHeaders := make(chan http.Header, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))

	p, err := plugintesting.BuildPipeline([]pipeline.Plugin{mut})
	if err != nil {
		backend.Close()
		t.Fatalf("BuildPipeline: %v", err)
	}
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())

	fx := &headerMutatorFixture{
		client:     &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}},
		backendURL: backend.URL,
		headers: func() http.Header {
			select {
			case h := <-gotHeaders:
				return h
			default:
				t.Fatal("backend was never reached")
				return nil
			}
		},
	}
	cleanup := func() {
		proxy.Close()
		backend.Close()
	}
	return fx, cleanup
}

// do sends a GET through the proxy to the backend, applying setup to the
// outgoing request (e.g. seeding client headers), and returns the headers
// the backend saw. It fails the test on transport error or non-200.
func (fx *headerMutatorFixture) do(t *testing.T, setup func(*http.Request)) http.Header {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fx.backendURL+"/x", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if setup != nil {
		setup(req)
	}
	resp, err := fx.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	return fx.headers()
}

// TestForwardProxy_ArbitraryHeaderReachesWire is the forwardproxy analog
// of the ext_proc TestExtProc_Outbound_ArbitraryHeaderReachesWire: a
// plugin that Sets a non-Authorization header must have that header reach
// the upstream. Before PR #760, forwardproxy forwarded only Authorization,
// silently dropping this injected header (e.g. static-inject's x-api-key).
func TestForwardProxy_ArbitraryHeaderReachesWire(t *testing.T) {
	fx, cleanup := newHeaderMutatorFixture(t, &headerMutatorPlugin{
		set: map[string]string{"X-Api-Key": "secret-123"},
	})
	defer cleanup()

	// The request carries no X-Api-Key; only the plugin injects it.
	h := fx.do(t, nil)
	if got := h.Get("X-Api-Key"); got != "secret-123" {
		t.Errorf("backend X-Api-Key = %q, want secret-123 (plugin-injected header did not reach the wire)", got)
	}
}

// TestForwardProxy_OverwrittenHeaderReachesWire asserts a plugin that
// Sets an already-present header overwrites the client's value on the
// forwarded request (set/replace, not append).
func TestForwardProxy_OverwrittenHeaderReachesWire(t *testing.T) {
	fx, cleanup := newHeaderMutatorFixture(t, &headerMutatorPlugin{
		set: map[string]string{"X-Tenant": "server-chosen"},
	})
	defer cleanup()

	h := fx.do(t, func(r *http.Request) { r.Header.Set("X-Tenant", "client-supplied") })
	if got := h.Values("X-Tenant"); len(got) != 1 || got[0] != "server-chosen" {
		t.Errorf("backend X-Tenant = %v, want [server-chosen] (plugin overwrite did not replace client value)", got)
	}
}

// TestForwardProxy_DeletedHeaderIsRemoved is the forwardproxy analog of
// the ext_proc TestExtProc_Outbound_DeletedHeaderIsRemoved: a plugin that
// Dels a header the client sent must strip it from the forwarded request.
// Before PR #760 the Authorization-only path had no way to express a
// deletion, so a plugin asking to remove a header was ignored.
func TestForwardProxy_DeletedHeaderIsRemoved(t *testing.T) {
	fx, cleanup := newHeaderMutatorFixture(t, &headerMutatorPlugin{
		del: []string{"X-Drop-Me"},
	})
	defer cleanup()

	h := fx.do(t, func(r *http.Request) { r.Header.Set("X-Drop-Me", "please-remove") })
	if got := h.Get("X-Drop-Me"); got != "" {
		t.Errorf("backend X-Drop-Me = %q, want empty (plugin deletion did not reach the wire)", got)
	}
}

// TestForwardProxy_UnchangedHeaderPreserved guards the other direction:
// a header the client sent that NO plugin touches must still reach the
// upstream unchanged. This pins that the delete loop only strips headers
// the plugin actually removed, never a spurious drop of untouched ones.
func TestForwardProxy_UnchangedHeaderPreserved(t *testing.T) {
	// Plugin mutates an unrelated header so the pipeline runs, but leaves
	// X-Keep alone.
	fx, cleanup := newHeaderMutatorFixture(t, &headerMutatorPlugin{
		set: map[string]string{"X-Added": "1"},
	})
	defer cleanup()

	h := fx.do(t, func(r *http.Request) { r.Header.Set("X-Keep", "keep-me") })
	if got := h.Get("X-Keep"); got != "keep-me" {
		t.Errorf("backend X-Keep = %q, want keep-me (untouched client header was dropped)", got)
	}
	if got := h.Get("X-Added"); got != "1" {
		t.Errorf("backend X-Added = %q, want 1", got)
	}
}
