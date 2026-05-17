package reverseproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/bypass"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/jwtvalidation/validation"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/plugintesting"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
)

type mockVerifier struct {
	claims *validation.Claims
	err    error
}

func (m *mockVerifier) Verify(_ context.Context, _ string, _ string) (*validation.Claims, error) {
	return m.claims, m.err
}

func inboundPipelineFromAuth(t *testing.T, a *auth.Auth) *pipeline.Holder {
	t.Helper()
	p, err := plugintesting.BuildPipeline([]pipeline.Plugin{plugintesting.NewJWTValidation(a, false)})
	if err != nil {
		t.Fatalf("building inbound pipeline: %v", err)
	}
	return pipeline.NewHolder(p)
}

func TestReverseProxy_AllowedRequest(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("backend-ok"))
	}))
	defer backend.Close()

	a := auth.New(auth.Config{
		Verifier: &mockVerifier{claims: &validation.Claims{Subject: "user"}},
		Identity: auth.IdentityConfig{Audience: "my-app"},
	})
	srv, err := NewServer(inboundPipelineFromAuth(t, a), nil, backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	req, _ := http.NewRequest("GET", proxy.URL+"/api/data", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestReverseProxy_DeniedRequest(t *testing.T) {
	a := auth.New(auth.Config{
		Verifier: &mockVerifier{err: fmt.Errorf("invalid token")},
		Identity: auth.IdentityConfig{Audience: "my-app"},
	})
	srv, _ := NewServer(inboundPipelineFromAuth(t, a), nil, "http://localhost:9999")

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	req, _ := http.NewRequest("GET", proxy.URL+"/api/data", nil)
	req.Header.Set("Authorization", "Bearer bad-token")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestReverseProxy_BypassPath(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("agent-card"))
	}))
	defer backend.Close()

	matcher, _ := bypass.NewMatcher(bypass.DefaultPatterns)
	a := auth.New(auth.Config{
		Verifier: &mockVerifier{err: fmt.Errorf("should not be called")},
		Bypass:   matcher,
	})
	srv, _ := NewServer(inboundPipelineFromAuth(t, a), nil, backend.URL)

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// No auth header, but bypass path should be allowed
	req, _ := http.NewRequest("GET", proxy.URL+"/.well-known/agent.json", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 for bypass path", resp.StatusCode)
	}
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

// bodyMutatorPlugin declares WritesBody and rewrites the request body
// to a fixed payload. The pipeline validator requires WritesBody run
// after any ReadsBody plugin, which this satisfies by itself (no reader
// present when used alone).
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

// TestReverseProxy_RequestBodyMutation: a WritesBody plugin that
// rewrites pctx.Body via SetBody must cause the upstream backend to
// receive the new bytes with a correct Content-Length header. Confirms
// that the reverseproxy request-path propagation is wired to the
// BodyMutated flag.
func TestReverseProxy_RequestBodyMutation(t *testing.T) {
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
	srv, err := NewServer(pipeline.NewHolder(p), nil, backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	orig := `{"original":"prompt"}`
	req, _ := http.NewRequest("POST", proxy.URL+"/agent", strings.NewReader(orig))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip") // plugin may have decompressed; listener clears
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if string(gotBody) != newBody {
		t.Errorf("backend got body = %q, want %q (listener did not propagate mutation)", gotBody, newBody)
	}
	if gotLength != "23" { // len(`{"sanitized":"payload"}`)
		t.Errorf("Content-Length = %q, want 23 (mutation rewrite)", gotLength)
	}
	if gotEnc != "" {
		t.Errorf("Content-Encoding = %q, want empty (listener should clear on mutation)", gotEnc)
	}
}

func TestReverseProxy_BodyBuffering(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	recorder := &bodyRecorderPlugin{}
	p, err := pipeline.New([]pipeline.Plugin{recorder})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(pipeline.NewHolder(p), nil, backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	body := `{"method":"tools/call","id":1,"params":{"name":"get_weather"}}`
	req, _ := http.NewRequest("POST", proxy.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	if string(recorder.receivedBody) != body {
		t.Errorf("plugin got body = %q, want %q", recorder.receivedBody, body)
	}
}

func TestReverseProxy_BodyTooLarge(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("backend should not be reached for oversized body")
	}))
	defer backend.Close()

	recorder := &bodyRecorderPlugin{}
	p, err := pipeline.New([]pipeline.Plugin{recorder})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(pipeline.NewHolder(p), nil, backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	bigBody := strings.Repeat("x", maxBodySize+1)
	req, _ := http.NewRequest("POST", proxy.URL+"/mcp", strings.NewReader(bigBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

func TestReverseProxy_BodyNotBuffered_WhenNotNeeded(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	a := auth.New(auth.Config{
		Verifier: &mockVerifier{claims: &validation.Claims{Subject: "user"}},
		Identity: auth.IdentityConfig{Audience: "test"},
	})
	srv, err := NewServer(inboundPipelineFromAuth(t, a), nil, backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	body := `{"data":"should not be buffered"}`
	req, _ := http.NewRequest("POST", proxy.URL+"/api", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer valid-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestRecordInboundReject_EmitsDeniedPhase verifies the reverse-proxy
// listener's inbound reject-event recording. Gap: before this, an
// inbound request denied by jwt-validation or any other gate plugin
// produced a 401/403 on the wire but no SessionDenied event — abctl
// and /v1/sessions showed nothing, making misconfigurations invisible.
func TestRecordInboundReject_EmitsDeniedPhase(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Host:      "agent.example",
		Extensions: pipeline.Extensions{
			A2A: &pipeline.A2AExtension{SessionID: "sess-abc"},
			Invocations: &pipeline.Invocations{
				Inbound: []pipeline.Invocation{{
					Plugin: "jwt-validation", Action: pipeline.ActionDeny,
					Phase: pipeline.InvocationPhaseRequest, Reason: "jwt_failed",
					Details: map[string]string{
						"expected_issuer": "http://issuer.example",
					},
				}},
			},
		},
	}
	action := pipeline.DenyStatus(401, "auth.unauthorized", "token validation failed")
	s.recordInboundReject(pctx, action)

	v := store.View("sess-abc")
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 event under sess-abc, got %+v", v)
	}
	ev := v.Events[0]
	if ev.Direction != pipeline.Inbound || ev.Phase != pipeline.SessionDenied {
		t.Errorf("Direction/Phase = %v/%v, want Inbound/SessionDenied", ev.Direction, ev.Phase)
	}
	if ev.StatusCode != 401 || ev.Error == nil || ev.Error.Code != "auth.unauthorized" {
		t.Errorf("Status/Error = %d/%+v, want 401/auth.unauthorized", ev.StatusCode, ev.Error)
	}
	if ev.Invocations == nil || len(ev.Invocations.Inbound) != 1 {
		t.Errorf("Invocations lost on denied event: %+v", ev.Invocations)
	}
}

// TestRecordInboundReject_FallbackBucketing confirms the bucket
// selection falls through A2A.SessionID → ActiveSession → default.
// A request with no A2A context (bypass path that denied in a later
// gate) lands in the default bucket so operators still see it.
func TestRecordInboundReject_FallbackBucketing(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Extensions: pipeline.Extensions{
			Invocations: &pipeline.Invocations{
				Inbound: []pipeline.Invocation{{
					Plugin: "jwt-validation", Action: pipeline.ActionDeny,
					Phase: pipeline.InvocationPhaseRequest, Reason: "no_header",
				}},
			},
		},
	}
	action := pipeline.DenyStatus(401, "auth.unauthorized", "no bearer token")
	s.recordInboundReject(pctx, action)

	if v := store.View(session.DefaultSessionID); v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 event in default bucket, got %+v", v)
	}
}

// TestRecordInboundReject_SkipsWithoutInvocations confirms the skip
// rule: denials with no diagnostic context are not recorded.
func TestRecordInboundReject_SkipsWithoutInvocations(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	action := pipeline.DenyStatus(401, "auth.unauthorized", "denied")
	s.recordInboundReject(&pipeline.Context{Direction: pipeline.Inbound}, action)

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

// TestReverseProxy_SchemeFromTLS verifies that pctx.Scheme is derived
// from r.TLS (presence = "https", absence = "http") rather than
// r.URL.Scheme, which Go leaves empty for server-side requests.
// httptest.NewServer is plaintext (TLS nil); NewTLSServer sets TLS so
// "https" falls out.
func TestReverseProxy_SchemeFromTLS(t *testing.T) {
	// Backend that the reverse proxy forwards to. Scheme on backend is
	// irrelevant here — we're asserting on pctx.Scheme at the listener
	// entry, which reflects the caller→proxy leg, not the proxy→backend
	// leg.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	tests := []struct {
		name   string
		useTLS bool
		want   string
	}{
		{name: "plaintext_inbound", useTLS: false, want: "http"},
		{name: "tls_inbound", useTLS: true, want: "https"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			capturer := &schemeCapturePlugin{}
			// BuildPipeline matches the construction used in
			// extproc/extauthz scheme tests — keeps the four
			// listener tests grep-parallel.
			p, err := plugintesting.BuildPipeline([]pipeline.Plugin{capturer})
			if err != nil {
				t.Fatalf("BuildPipeline: %v", err)
			}
			srv, err := NewServer(pipeline.NewHolder(p), nil, backend.URL)
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}

			var proxy *httptest.Server
			var client *http.Client
			if tc.useTLS {
				proxy = httptest.NewTLSServer(srv.Handler())
				client = proxy.Client() // trusts the test cert
			} else {
				proxy = httptest.NewServer(srv.Handler())
				client = http.DefaultClient
			}
			defer proxy.Close()

			resp, err := client.Get(proxy.URL + "/api")
			if err != nil {
				t.Fatalf("client.Get: %v", err)
			}
			resp.Body.Close()

			if capturer.got != tc.want {
				t.Errorf("pctx.Scheme = %q, want %q", capturer.got, tc.want)
			}
		})
	}
}

// a2aStampPlugin is a tiny inline plugin that simulates a2a-parser's
// two-phase behavior:
//   - OnRequest populates pctx.Extensions.A2A so the request-side
//     session-event gate fires (no SessionID yet — first turn).
//   - OnResponse stamps the server-assigned SessionID, mimicking
//     extractSessionID(pctx.ResponseBody) in the real parser. This is
//     what triggers the rekey path in modifyResponse.
type a2aStampPlugin struct {
	method      string
	responseSID string // SessionID to stamp during OnResponse
}

func (p *a2aStampPlugin) Name() string { return "a2a-stamp" }
func (p *a2aStampPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *a2aStampPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.Extensions.A2A = &pipeline.A2AExtension{Method: p.method}
	pctx.Observe("matched_request")
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *a2aStampPlugin) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if pctx.Extensions.A2A != nil {
		pctx.Extensions.A2A.SessionID = p.responseSID
	}
	pctx.Observe("matched_response")
	return pipeline.Action{Type: pipeline.Continue}
}

// TestReverseProxy_ModifyResponse_RekeyAndResponseEvent locks in the
// behavior that an A2A first-turn (request without contextId, agent
// assigns contextId in response) ends up with all the turn's events
// merged into the contextId bucket, plus a SessionResponse event
// recorded. Without rekey + the response-side append, abctl users see
// orphan request rows in `default` and no inbound response row at all.
func TestReverseProxy_ModifyResponse_RekeyAndResponseEvent(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	const ctxID = "ctx-1234"
	stamp := &a2aStampPlugin{method: "message/stream", responseSID: ctxID}
	p, err := plugintesting.BuildPipeline([]pipeline.Plugin{stamp})
	if err != nil {
		t.Fatalf("BuildPipeline: %v", err)
	}

	store := session.New(30*time.Minute, 100, 100)
	srv, err := NewServer(pipeline.NewHolder(p), store, backend.URL)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	resp, err := http.DefaultClient.Get(proxy.URL + "/api")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()

	// After the response: the `default` bucket should be gone (rekeyed
	// to ctxID) and the ctxID bucket should hold both events from this
	// turn (request + response).
	if v := store.View(session.DefaultSessionID); v != nil {
		t.Errorf("default bucket still exists with %d events; expected rekey to %q", len(v.Events), ctxID)
	}
	view := store.View(ctxID)
	if view == nil {
		t.Fatalf("session %q not found; rekey did not run", ctxID)
	}
	if len(view.Events) != 2 {
		t.Fatalf("session %q has %d events, want 2 (request + response)", ctxID, len(view.Events))
	}

	gotPhases := []pipeline.SessionPhase{view.Events[0].Phase, view.Events[1].Phase}
	wantPhases := []pipeline.SessionPhase{pipeline.SessionRequest, pipeline.SessionResponse}
	for i := range wantPhases {
		if gotPhases[i] != wantPhases[i] {
			t.Errorf("event %d phase = %v, want %v", i, gotPhases[i], wantPhases[i])
		}
	}

	// Response event must carry the StatusCode + populated Invocations
	// (the response-phase "observe" we appended in OnResponse) — those
	// are the bits that previously dropped out.
	respEvent := view.Events[1]
	if respEvent.StatusCode != http.StatusOK {
		t.Errorf("response StatusCode = %d, want 200", respEvent.StatusCode)
	}
	if respEvent.Invocations == nil || len(respEvent.Invocations.Inbound) == 0 {
		t.Errorf("response event has no Invocations; expected one a2a-stamp observe entry")
	}
}
