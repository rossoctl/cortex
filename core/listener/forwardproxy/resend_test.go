package forwardproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/tidwall/gjson"
)

// substituter redirects every request to target, at path when path is set, and
// answers a Resend by swapping the body's model for sub. resend says whether it asks.
type substituter struct {
	target, path, sub string
	resend            bool

	mu    sync.Mutex
	asked []int
}

func (p *substituter) Name() string { return "substituter" }
func (p *substituter) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{WritesDestination: true, WritesRequestBody: true, Description: "test"}
}
func (p *substituter) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	// The parse inference-parser would have made, so the rows carry a model.
	if m, ok := pctx.RequestModel(); ok {
		pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: m}
	}
	u, _ := url.Parse(p.target)
	if err := pctx.Redirect(u); err != nil {
		panic(err)
	}
	if p.path != "" {
		if err := pctx.SetRedirectPath(p.path); err != nil {
			panic(err)
		}
	}
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *substituter) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *substituter) Resend(_ context.Context, pctx *pipeline.Context, status int, _ []byte) bool {
	p.mu.Lock()
	p.asked = append(p.asked, status)
	p.mu.Unlock()
	if !p.resend {
		return false
	}
	return pctx.SetRequestModel(p.sub) == nil
}

// modelServer refuses every model but served, as a LiteLLM team does, and records
// each request's path and model. stream answers the served model as SSE.
type modelServer struct {
	served string
	stream bool
	big    int // when > 0, the refusal body is padded to this many bytes

	mu      sync.Mutex
	paths   []string
	model   []string
	headers []http.Header
}

func (m *modelServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	model := gjson.GetBytes(body, "model").String()
	m.mu.Lock()
	m.paths = append(m.paths, r.URL.RequestURI())
	m.model = append(m.model, model)
	m.headers = append(m.headers, r.Header.Clone())
	m.mu.Unlock()
	if model != m.served {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		refusal := `{"error":{"type":"team_model_access_denied","param":"model","code":"403","message":"team not allowed"}}`
		if m.big > 0 {
			refusal += strings.Repeat(" ", m.big-len(refusal))
		}
		_, _ = io.WriteString(w, refusal)
		return
	}
	if m.stream {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"PONG\"}}]}\n\ndata: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"model":"`+model+`","choices":[{"message":{"content":"PONG"}}]}`)
}

func (m *modelServer) seen() (paths, models []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.paths...), append([]string(nil), m.model...)
}

// postVia sends a chat request for model through a forward proxy whose outbound
// pipeline is plugin, and returns the answer and the session store.
func postVia(t *testing.T, plugin pipeline.Plugin, model string) (*http.Response, string, *session.Store) {
	t.Helper()
	p, err := pipeline.New([]pipeline.Plugin{plugin})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Sessions: store, Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)

	req, _ := http.NewRequest(http.MethodPost, "http://agent-provider.example/zen/v1/chat/completions?beta=true",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body), store
}

func TestResend_TheClientSeesOnlyTheSecondAnswer(t *testing.T) {
	upstream := &modelServer{served: "glm-5-3"}
	backend := httptest.NewServer(upstream)
	defer backend.Close()

	plugin := &substituter{target: backend.URL, path: "/v1/chat/completions", sub: "glm-5-3", resend: true}
	resp, body, _ := postVia(t, plugin, "exo-free")

	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "PONG") {
		t.Fatalf("client got %d %q, want the substitute's 200", resp.StatusCode, body)
	}
	paths, models := upstream.seen()
	if len(models) != 2 || models[0] != "exo-free" || models[1] != "glm-5-3" {
		t.Errorf("upstream saw models %v, want the client's then the substitute", models)
	}
	for _, p := range paths {
		if p != "/v1/chat/completions?beta=true" {
			t.Errorf("upstream saw path %q, want the redirect path with the client's query", p)
		}
	}
	if len(plugin.asked) != 1 || plugin.asked[0] != http.StatusForbidden {
		t.Errorf("Resend asked with %v, want exactly once, with the refusal's 403", plugin.asked)
	}
}

func TestResend_AStreamedSecondAnswerReachesTheClient(t *testing.T) {
	upstream := &modelServer{served: "glm-5-3", stream: true}
	backend := httptest.NewServer(upstream)
	defer backend.Close()

	resp, body, _ := postVia(t, &substituter{target: backend.URL, sub: "glm-5-3", resend: true}, "exo-free")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "PONG") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("client got %d %q, want the substitute's stream", resp.StatusCode, body)
	}
}

func TestResend_ASecondRefusalReachesTheClient(t *testing.T) {
	upstream := &modelServer{served: "nothing-at-all"}
	backend := httptest.NewServer(upstream)
	defer backend.Close()

	plugin := &substituter{target: backend.URL, sub: "glm-5-3", resend: true}
	resp, body, _ := postVia(t, plugin, "exo-free")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "team_model_access_denied") {
		t.Fatalf("client got %d %q, want the second refusal", resp.StatusCode, body)
	}
	if _, models := upstream.seen(); len(models) != 2 {
		t.Errorf("upstream saw %d sends, want exactly two: nothing is asked twice", len(models))
	}
	if len(plugin.asked) != 1 {
		t.Errorf("Resend asked %d times, want once", len(plugin.asked))
	}
}

func TestResend_ARefusalNotResentGoesThroughByteForByte(t *testing.T) {
	// Larger than the bytes a Resender is shown, so the relay must stitch them back.
	upstream := &modelServer{served: "glm-5-3", big: pipeline.ResendPeekLimit + 4096}
	backend := httptest.NewServer(upstream)
	defer backend.Close()

	resp, body, _ := postVia(t, &substituter{target: backend.URL, sub: "glm-5-3"}, "exo-free")
	if resp.StatusCode != http.StatusForbidden || len(body) != upstream.big ||
		!strings.HasPrefix(body, `{"error":{"type":"team_model_access_denied"`) {
		t.Fatalf("client got %d and %d bytes, want the refusal's %d bytes intact", resp.StatusCode, len(body), upstream.big)
	}
	if _, models := upstream.seen(); len(models) != 1 {
		t.Errorf("upstream saw %d sends, want one", len(models))
	}
}

func TestResend_BothAttemptsAreRecordedAndPairByRequestID(t *testing.T) {
	upstream := &modelServer{served: "glm-5-3"}
	backend := httptest.NewServer(upstream)
	defer backend.Close()

	_, _, store := postVia(t, &substituter{target: backend.URL, sub: "glm-5-3", resend: true}, "exo-free")
	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 4 {
		t.Fatalf("rows = %+v, want request, refusal, re-sent request, answer", v)
	}
	ev := v.Events
	if ev[0].Phase != pipeline.SessionRequest || ev[1].Phase != pipeline.SessionResponse || ev[1].StatusCode != http.StatusForbidden ||
		ev[2].Phase != pipeline.SessionRequest || ev[3].Phase != pipeline.SessionResponse || ev[3].StatusCode != http.StatusOK {
		t.Fatalf("rows are %s/%d %s/%d %s/%d %s/%d, want request, 403, request, 200",
			ev[0].Phase, ev[0].StatusCode, ev[1].Phase, ev[1].StatusCode, ev[2].Phase, ev[2].StatusCode, ev[3].Phase, ev[3].StatusCode)
	}
	if ev[0].RequestID != ev[1].RequestID || ev[2].RequestID != ev[3].RequestID || ev[0].RequestID == ev[2].RequestID {
		t.Errorf("request ids %s %s %s %s, want each attempt paired with its own answer",
			ev[0].RequestID, ev[1].RequestID, ev[2].RequestID, ev[3].RequestID)
	}
	if ev[1].Error == nil || ev[1].Error.Message != "team_model_access_denied" {
		t.Errorf("refusal row error = %+v, want the upstream's error type", ev[1].Error)
	}
	// Each attempt's rows name the model it was sent with: the refused one the
	// client's, the resent one the substitute, with the client's beside it.
	for i, want := range []struct{ model, requested string }{{"exo-free", ""}, {"exo-free", ""}, {"glm-5-3", "exo-free"}, {"glm-5-3", "exo-free"}} {
		if inf := ev[i].Inference; inf == nil || inf.Model != want.model || inf.RequestedModel != want.requested {
			t.Errorf("row %d inference = %+v, want model %q requested %q", i, inf, want.model, want.requested)
		}
	}
}

// bodyReader reads response bodies, as inference-parser does, so the listener strips
// the client's Accept-Encoding and lets its transport negotiate and decode.
type bodyReader struct{}

func (bodyReader) Name() string { return "body-reader" }
func (bodyReader) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{ReadsBody: true, Description: "test"}
}
func (bodyReader) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (bodyReader) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// The resent request is prepared for the upstream as the first send is: the client's
// proxy credential and hop-by-hop headers stay with the proxy, and the client's
// Accept-Encoding is left to the transport when a plugin reads response bodies.
func TestResend_TheResentRequestIsPreparedAsTheFirstSendIs(t *testing.T) {
	upstream := &modelServer{served: "glm-5-3"}
	backend := httptest.NewServer(upstream)
	defer backend.Close()

	p, err := pipeline.New([]pipeline.Plugin{bodyReader{}, &substituter{target: backend.URL, sub: "glm-5-3", resend: true}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	proxyURL := mustParseURL(proxy.URL)
	proxyURL.User = url.UserPassword("user", "proxy-secret")
	req, _ := http.NewRequest(http.MethodPost, "http://agent-provider.example/v1/chat/completions",
		strings.NewReader(`{"model":"exo-free","messages":[]}`))
	req.Header.Set("Accept-Encoding", "gzip, br")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("Connection", "keep-alive")
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if len(upstream.headers) != 2 {
		t.Fatalf("upstream saw %d sends, want the refused one and the resend", len(upstream.headers))
	}
	for i, h := range upstream.headers {
		for _, name := range []string{"Proxy-Authorization", "Keep-Alive", "Connection"} {
			if v := h.Get(name); v != "" {
				t.Errorf("send %d carried %s: %q", i+1, name, v)
			}
		}
		if got := h.Get("Accept-Encoding"); got == "gzip, br" {
			t.Errorf("send %d carried the client's Accept-Encoding %q", i+1, got)
		}
	}
	if a, b := upstream.headers[0].Get("Accept-Encoding"), upstream.headers[1].Get("Accept-Encoding"); a != b {
		t.Errorf("Accept-Encoding %q on the first send but %q on the resend", a, b)
	}
}
