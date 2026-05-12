package forwardproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/jwtvalidation/validation"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/plugintesting"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/tokenexchange/cache"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/tokenexchange/exchange"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/routing"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
)

type mockVerifier struct {
	claims *validation.Claims
	err    error
}

func (m *mockVerifier) Verify(_ context.Context, _ string, _ string) (*validation.Claims, error) {
	return m.claims, m.err
}

func outboundPipelineFromAuth(t *testing.T, a *auth.Auth) *pipeline.Holder {
	t.Helper()
	p, err := plugintesting.BuildPipeline([]pipeline.Plugin{plugintesting.NewTokenExchange(a)})
	if err != nil {
		t.Fatalf("building outbound pipeline: %v", err)
	}
	return pipeline.NewHolder(p)
}

func TestForwardProxy_Exchange(t *testing.T) {
	// Token exchange server
	exchangeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "exchanged-token",
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	}))
	defer exchangeSrv.Close()

	// Backend server that the proxy forwards to
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if got != "Bearer exchanged-token" {
			t.Errorf("backend got Authorization = %q, want Bearer exchanged-token", got)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	router, _ := routing.NewRouter("exchange", []routing.Route{})
	exchanger := exchange.NewClient(exchangeSrv.URL, &exchange.ClientSecretAuth{
		ClientID: "agent", ClientSecret: "secret",
	})
	a := auth.New(auth.Config{
		Router:    router,
		Exchanger: exchanger,
		Cache:     cache.New(),
	})

	srv := &Server{OutboundPipeline: outboundPipelineFromAuth(t, a), Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// Forward proxy: request URL is the full backend URL (as a proxy would receive)
	req, _ := http.NewRequest("GET", backend.URL+"/test", nil)
	req.Header.Set("Authorization", "Bearer user-token")

	// Route through the proxy by sending the request to proxy address
	// but with the backend URL as the target (simulates HTTP_PROXY behavior)
	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestForwardProxy_CONNECT_Rejected(t *testing.T) {
	a := auth.New(auth.Config{})
	srv := NewServer(outboundPipelineFromAuth(t, a), nil)
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	req, _ := http.NewRequest("CONNECT", proxy.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestForwardProxy_Deny(t *testing.T) {
	router, _ := routing.NewRouter("exchange", []routing.Route{})
	a := auth.New(auth.Config{
		Router:        router,
		NoTokenPolicy: auth.NoTokenPolicyDeny,
	})

	srv := NewServer(outboundPipelineFromAuth(t, a), nil)
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	req, _ := http.NewRequest("GET", proxy.URL+"/test", nil)
	// No Authorization header — should be denied
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func mustParseURL(rawURL string) *url.URL {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	return u
}

// --- Body Buffering Tests ---

// bodyRecorderPlugin records whether it received a body during OnRequest.
type bodyRecorderPlugin struct {
	receivedBody []byte
}

func (p *bodyRecorderPlugin) Name() string { return "body-recorder" }
func (p *bodyRecorderPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{BodyAccess: true}
}
func (p *bodyRecorderPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.receivedBody = pctx.Body
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *bodyRecorderPlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// bodyMutatorPlugin declares WritesBody and rewrites pctx.Body via
// SetBody. Used below to confirm the forwardproxy propagates the
// mutation to the upstream request.
type bodyMutatorPlugin struct {
	newBody []byte
}

func (p *bodyMutatorPlugin) Name() string { return "body-mutator" }
func (p *bodyMutatorPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{WritesBody: true}
}
func (p *bodyMutatorPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.SetBody(p.newBody)
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *bodyMutatorPlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// TestForwardProxy_RequestBodyMutation: a WritesBody plugin rewriting
// pctx.Body must cause the upstream backend to receive the new bytes
// with a correct Content-Length and no Content-Encoding.
func TestForwardProxy_RequestBodyMutation(t *testing.T) {
	newBody := `{"sanitized":"payload"}`
	var (
		gotBody   []byte
		gotLength string
		gotEnc    string
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotLength = r.Header.Get("Content-Length")
		gotEnc = r.Header.Get("Content-Encoding")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	mutator := &bodyMutatorPlugin{newBody: []byte(newBody)}
	p, err := pipeline.New([]pipeline.Plugin{mutator})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	orig := `{"original":"prompt"}`
	req, _ := http.NewRequest("POST", backend.URL+"/agent", strings.NewReader(orig))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")

	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if string(gotBody) != newBody {
		t.Errorf("backend got body = %q, want %q", gotBody, newBody)
	}
	if gotLength != "23" {
		t.Errorf("Content-Length = %q, want 23", gotLength)
	}
	if gotEnc != "" {
		t.Errorf("Content-Encoding = %q, want empty", gotEnc)
	}
}

func TestForwardProxy_BodyBuffering(t *testing.T) {
	recorder := &bodyRecorderPlugin{}
	p, err := pipeline.New([]pipeline.Plugin{recorder})
	if err != nil {
		t.Fatal(err)
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	body := `{"method":"tools/call","id":1,"params":{"name":"get_weather"}}`
	req, _ := http.NewRequest("POST", backend.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	if string(recorder.receivedBody) != body {
		t.Errorf("plugin got body = %q, want %q", recorder.receivedBody, body)
	}
}

func TestForwardProxy_BodyTooLarge(t *testing.T) {
	recorder := &bodyRecorderPlugin{}
	p, err := pipeline.New([]pipeline.Plugin{recorder})
	if err != nil {
		t.Fatal(err)
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("backend should not be reached for oversized body")
	}))
	defer backend.Close()

	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// Send body larger than maxBodySize (1MB)
	bigBody := strings.Repeat("x", maxBodySize+1)
	req, _ := http.NewRequest("POST", backend.URL+"/mcp", strings.NewReader(bigBody))
	req.Header.Set("Content-Type", "application/json")

	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

func TestForwardProxy_NoBodyBuffering_WhenNotNeeded(t *testing.T) {
	a := auth.New(auth.Config{})
	p := outboundPipelineFromAuth(t, a) // default pipeline has no body-access plugins; already a Holder

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	srv := &Server{OutboundPipeline: p, Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	body := `{"data":"should not be buffered"}`
	req, _ := http.NewRequest("POST", backend.URL+"/api", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestRecordOutboundReject_EmitsDeniedPhase verifies the forward-proxy
// listener's reject-event recording: a rejected outbound request
// produces a SessionDenied event with the plugin Invocation context
// and the Violation mapped to StatusCode + EventError.
func TestRecordOutboundReject_EmitsDeniedPhase(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	store.Append("sess-active", pipeline.SessionEvent{
		At:        time.Now().Add(-50 * time.Millisecond),
		Direction: pipeline.Inbound,
		Phase:     pipeline.SessionRequest,
		A2A:       &pipeline.A2AExtension{Method: "message/send", SessionID: "sess-active"},
	})
	s := &Server{Sessions: store}

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "external.example",
		Extensions: pipeline.Extensions{
			Invocations: &pipeline.Invocations{
				Outbound: []pipeline.Invocation{{
					Plugin: "ibac", Action: pipeline.ActionDeny,
					Phase: pipeline.InvocationPhaseRequest, Reason: "blocked",
					Details: map[string]string{"llm_reason": "unrelated to user intent"},
				}},
			},
		},
	}
	action := pipeline.DenyStatus(403, "ibac.blocked", "unrelated to user intent")
	s.recordOutboundReject(pctx, action)

	v := store.View("sess-active")
	if v == nil || len(v.Events) != 2 {
		t.Fatalf("expected 2 events under sess-active, got %+v", v)
	}
	ev := v.Events[1]
	if ev.Direction != pipeline.Outbound || ev.Phase != pipeline.SessionDenied {
		t.Errorf("Direction/Phase = %v/%v, want Outbound/SessionDenied", ev.Direction, ev.Phase)
	}
	if ev.StatusCode != 403 || ev.Error == nil || ev.Error.Code != "ibac.blocked" {
		t.Errorf("Status/Error = %d/%+v, want 403/ibac.blocked", ev.StatusCode, ev.Error)
	}
	if ev.Host != "external.example" {
		t.Errorf("Host = %q, want external.example", ev.Host)
	}
	if ev.Invocations == nil || len(ev.Invocations.Outbound) != 1 {
		t.Errorf("Invocations lost on denied event: %+v", ev.Invocations)
	}
}

// TestRecordOutboundReject_SkipsWithoutInvocations confirms the skip
// rule matches extproc's equivalent: denials with no diagnostic
// context are not recorded, so session stream attribution stays
// meaningful.
func TestRecordOutboundReject_SkipsWithoutInvocations(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	action := pipeline.DenyStatus(403, "policy.forbidden", "forbidden")
	s.recordOutboundReject(&pipeline.Context{Direction: pipeline.Outbound}, action)

	if v := store.View(session.DefaultSessionID); v != nil {
		t.Errorf("expected no event, got %+v", v)
	}
}

// schemeCapturePlugin captures pctx.Scheme for the scheme-wiring
// test below.
type schemeCapturePlugin struct {
	got string
}

func (p *schemeCapturePlugin) Name() string { return "scheme-capture" }
func (p *schemeCapturePlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *schemeCapturePlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.got = pctx.Scheme
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *schemeCapturePlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// TestForwardProxy_PopulatesSchemeFromRequestURL verifies the
// forward-proxy listener surfaces r.URL.Scheme on pctx. For HTTP
// forward proxies the agent's request line carries the full URL
// including scheme, so Go's net/http populates r.URL.Scheme
// reliably.
func TestForwardProxy_PopulatesSchemeFromRequestURL(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	capturer := &schemeCapturePlugin{}
	// BuildPipeline is a thin wrapper over pipeline.New; using it
	// across all four listener scheme tests keeps the construction
	// one-liner identical and the tests grep-parallel.
	p, err := plugintesting.BuildPipeline([]pipeline.Plugin{capturer})
	if err != nil {
		t.Fatalf("BuildPipeline: %v", err)
	}
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// Use the backend's http:// URL so the proxy actually dials it.
	// pctx.Scheme is observed BEFORE the outbound call, so the value
	// we assert on is whatever r.URL.Scheme was when the pipeline
	// ran, independent of whether the backend responds OK.
	req, _ := http.NewRequest("GET", backend.URL+"/x", nil)
	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	resp.Body.Close()

	if capturer.got != "http" {
		t.Errorf("pctx.Scheme = %q, want http", capturer.got)
	}
}
