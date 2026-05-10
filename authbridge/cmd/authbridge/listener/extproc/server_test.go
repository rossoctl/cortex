package extproc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocfilterv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc/metadata"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/bypass"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/cache"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/exchange"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/plugintesting"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/routing"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/validation"
)

// mockStream implements ExternalProcessor_ProcessServer for testing.
type mockStream struct {
	extprocv3.ExternalProcessor_ProcessServer
	ctx       context.Context
	requests  []*extprocv3.ProcessingRequest
	responses []*extprocv3.ProcessingResponse
	recvIdx   int
}

func (m *mockStream) Context() context.Context { return m.ctx }
func (m *mockStream) Send(resp *extprocv3.ProcessingResponse) error {
	m.responses = append(m.responses, resp)
	return nil
}
func (m *mockStream) Recv() (*extprocv3.ProcessingRequest, error) {
	if m.recvIdx >= len(m.requests) {
		return nil, fmt.Errorf("EOF")
	}
	req := m.requests[m.recvIdx]
	m.recvIdx++
	return req, nil
}
func (m *mockStream) SetHeader(metadata.MD) error  { return nil }
func (m *mockStream) SendHeader(metadata.MD) error { return nil }
func (m *mockStream) SetTrailer(metadata.MD)       {}
func (m *mockStream) SendMsg(any) error            { return nil }
func (m *mockStream) RecvMsg(any) error            { return nil }

type mockVerifier struct {
	claims *validation.Claims
	err    error
}

func (v *mockVerifier) Verify(_ context.Context, _ string, _ string) (*validation.Claims, error) {
	return v.claims, v.err
}

// stubIdentity is a minimal pipeline.Identity for tests that construct
// pctx directly (without running jwt-validation). A handful of session-
// recording tests rely on "pctx has a known identity" to verify the
// snapshot path.
type stubIdentity struct {
	subject, clientID string
	scopes            []string
}

func (s stubIdentity) Subject() string  { return s.subject }
func (s stubIdentity) ClientID() string { return s.clientID }
func (s stubIdentity) Scopes() []string { return s.scopes }

func serverFromAuth(t *testing.T, a *auth.Auth) *Server {
	t.Helper()
	// Plugins build their own auth.Auth from local config in
	// production. Tests inject a pre-built *auth.Auth via
	// NewJWTValidationForTest / NewTokenExchangeForTest so the
	// listener-level assertions don't have to care about the plugin's
	// internal construction path.
	inbound, err := plugintesting.BuildPipeline([]pipeline.Plugin{plugintesting.NewJWTValidation(a, false)})
	if err != nil {
		t.Fatalf("building inbound pipeline: %v", err)
	}
	outbound, err := plugintesting.BuildPipeline([]pipeline.Plugin{plugintesting.NewTokenExchange(a)})
	if err != nil {
		t.Fatalf("building outbound pipeline: %v", err)
	}
	return &Server{
		InboundPipeline:  pipeline.NewHolder(inbound),
		OutboundPipeline: pipeline.NewHolder(outbound),
	}
}

func makeHeaders(kvs ...string) *corev3.HeaderMap {
	hm := &corev3.HeaderMap{}
	for i := 0; i < len(kvs); i += 2 {
		hm.Headers = append(hm.Headers, &corev3.HeaderValue{
			Key:      kvs[i],
			RawValue: []byte(kvs[i+1]),
		})
	}
	return hm
}

func inboundRequest(headers *corev3.HeaderMap) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{
		Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{Headers: headers},
		},
	}
}

func outboundRequest(headers *corev3.HeaderMap) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{
		Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{Headers: headers},
		},
	}
}

// --- Inbound Tests ---

func TestExtProc_Inbound_ValidJWT(t *testing.T) {
	a := auth.New(auth.Config{
		Verifier: &mockVerifier{claims: &validation.Claims{Subject: "user-1"}},
		Identity: auth.IdentityConfig{Audience: "my-agent"},
	})
	srv := serverFromAuth(t, a)

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			inboundRequest(makeHeaders(
				"x-authbridge-direction", "inbound",
				"authorization", "Bearer valid-token",
				":path", "/api/test",
			)),
		},
	}

	_ = srv.Process(stream) // returns error on EOF from Recv, expected

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	resp := stream.responses[0]
	// Should be allow (HeadersResponse, not ImmediateResponse)
	rh := resp.GetRequestHeaders()
	if rh == nil {
		t.Fatal("expected RequestHeaders response (allow), got ImmediateResponse")
	}
	// Should remove x-authbridge-direction header
	if rh.Response == nil || rh.Response.HeaderMutation == nil {
		t.Fatal("expected header mutation to remove direction header")
	}
	found := false
	for _, h := range rh.Response.HeaderMutation.RemoveHeaders {
		if h == "x-authbridge-direction" {
			found = true
		}
	}
	if !found {
		t.Error("expected x-authbridge-direction in RemoveHeaders")
	}
}

func TestExtProc_Inbound_InvalidJWT(t *testing.T) {
	a := auth.New(auth.Config{
		Verifier: &mockVerifier{err: fmt.Errorf("token expired")},
		Identity: auth.IdentityConfig{Audience: "my-agent"},
	})
	srv := serverFromAuth(t, a)

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			inboundRequest(makeHeaders(
				"x-authbridge-direction", "inbound",
				"authorization", "Bearer bad-token",
				":path", "/api/test",
			)),
		},
	}

	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	ir := stream.responses[0].GetImmediateResponse()
	if ir == nil {
		t.Fatal("expected ImmediateResponse (deny)")
	}
	if ir.Status.Code != 401 {
		t.Errorf("status = %d, want 401", ir.Status.Code)
	}
}

func TestExtProc_Inbound_BypassPath(t *testing.T) {
	matcher, _ := bypass.NewMatcher(bypass.DefaultPatterns)
	a := auth.New(auth.Config{
		Verifier: &mockVerifier{err: fmt.Errorf("should not be called")},
		Bypass:   matcher,
	})
	srv := serverFromAuth(t, a)

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			inboundRequest(makeHeaders(
				"x-authbridge-direction", "inbound",
				":path", "/healthz",
			)),
		},
	}

	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	rh := stream.responses[0].GetRequestHeaders()
	if rh == nil {
		t.Fatal("expected allow for bypass path")
	}
}

// --- Outbound Tests ---

func TestExtProc_Outbound_Exchange(t *testing.T) {
	exchangeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "exchanged-token",
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	}))
	defer exchangeSrv.Close()

	router, _ := routing.NewRouter("exchange", []routing.Route{})
	exchanger := exchange.NewClient(exchangeSrv.URL, &exchange.ClientSecretAuth{
		ClientID: "agent", ClientSecret: "secret",
	})
	a := auth.New(auth.Config{
		Router:    router,
		Exchanger: exchanger,
		Cache:     cache.New(),
	})
	srv := serverFromAuth(t, a)

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			outboundRequest(makeHeaders(
				":authority", "target-svc",
				"authorization", "Bearer user-token",
			)),
		},
	}

	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	rh := stream.responses[0].GetRequestHeaders()
	if rh == nil || rh.Response == nil || rh.Response.HeaderMutation == nil {
		t.Fatal("expected HeadersResponse with token replacement")
	}
	if len(rh.Response.HeaderMutation.SetHeaders) == 0 {
		t.Fatal("expected SetHeaders with new token")
	}
	got := string(rh.Response.HeaderMutation.SetHeaders[0].Header.RawValue)
	if got != "Bearer exchanged-token" {
		t.Errorf("token = %q, want Bearer exchanged-token", got)
	}
}

func TestExtProc_Outbound_Passthrough(t *testing.T) {
	router, _ := routing.NewRouter("passthrough", []routing.Route{})
	a := auth.New(auth.Config{Router: router})
	srv := serverFromAuth(t, a)

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			outboundRequest(makeHeaders(
				":authority", "unknown-svc",
				"authorization", "Bearer token",
			)),
		},
	}

	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	rh := stream.responses[0].GetRequestHeaders()
	if rh == nil {
		t.Fatal("expected passthrough (HeadersResponse)")
	}
	// Passthrough should have no header mutations
	if rh.Response != nil && rh.Response.HeaderMutation != nil && len(rh.Response.HeaderMutation.SetHeaders) > 0 {
		t.Error("passthrough should not set headers")
	}
}

func TestExtProc_Outbound_Deny(t *testing.T) {
	router, _ := routing.NewRouter("exchange", []routing.Route{})
	a := auth.New(auth.Config{
		Router:        router,
		NoTokenPolicy: auth.NoTokenPolicyDeny,
	})
	srv := serverFromAuth(t, a)

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			outboundRequest(makeHeaders(
				":authority", "target-svc",
				// No authorization header
			)),
		},
	}

	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	ir := stream.responses[0].GetImmediateResponse()
	if ir == nil {
		t.Fatal("expected ImmediateResponse (deny)")
	}
	// Pre-Violation, the listener used to hardcode 503 here regardless of
	// what the plugin said. That's now fixed — we pass through whatever
	// status the plugin (via auth.HandleOutbound) chose. NoTokenPolicyDeny
	// returns 401 because "no auth credential present" is a caller problem,
	// not an upstream-unavailable one.
	if ir.Status.Code != 401 {
		t.Errorf("status = %d, want 401", ir.Status.Code)
	}
}

// --- Response Headers ---

func TestExtProc_ResponseHeaders(t *testing.T) {
	a := auth.New(auth.Config{})
	srv := serverFromAuth(t, a)

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			{
				Request: &extprocv3.ProcessingRequest_ResponseHeaders{
					ResponseHeaders: &extprocv3.HttpHeaders{
						Headers: makeHeaders("content-type", "application/json"),
					},
				},
			},
		},
	}

	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	rh := stream.responses[0].GetResponseHeaders()
	if rh == nil {
		t.Fatal("expected ResponseHeaders passthrough")
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

// bodyMutatorPlugin declares WritesBody and rewrites pctx.Body via
// SetBody. Used to assert extproc emits a BodyMutation on the wire
// when a plugin rewrites the request body.
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

// TestExtProc_RequestBodyMutation_Inbound: a WritesBody plugin must
// produce a RequestBody ProcessingResponse carrying BodyMutation with
// the new bytes, and the header mutation must request content-encoding
// be removed.
func TestExtProc_RequestBodyMutation_Inbound(t *testing.T) {
	mutator := &bodyMutatorPlugin{newBody: []byte(`{"sanitized":"v"}`)}
	inbound, err := pipeline.New([]pipeline.Plugin{mutator})
	if err != nil {
		t.Fatal(err)
	}
	outbound, err := plugintesting.BuildPipeline([]pipeline.Plugin{plugintesting.NewTokenExchange(auth.New(auth.Config{}))})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{InboundPipeline: pipeline.NewHolder(inbound), OutboundPipeline: pipeline.NewHolder(outbound)}

	body := []byte(`{"original":"payload"}`)
	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			inboundRequest(makeHeaders(
				"x-authbridge-direction", "inbound",
				":method", "POST",
				":path", "/mcp",
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

	// Second response is the body-phase result. Unwrap it and assert
	// BodyMutation carries the mutator's new bytes.
	rb := stream.responses[1].GetRequestBody()
	if rb == nil || rb.Response == nil || rb.Response.BodyMutation == nil {
		t.Fatalf("expected RequestBody.Response.BodyMutation, got %+v", stream.responses[1])
	}
	gotBody := rb.Response.BodyMutation.GetBody()
	if string(gotBody) != string(mutator.newBody) {
		t.Errorf("BodyMutation.Body = %q, want %q", gotBody, mutator.newBody)
	}

	// Header mutation should include content-encoding in RemoveHeaders.
	hm := rb.Response.HeaderMutation
	if hm == nil {
		t.Fatal("expected HeaderMutation to clear content-encoding")
	}
	found := false
	for _, h := range hm.RemoveHeaders {
		if h == "content-encoding" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("RemoveHeaders = %v, want to include content-encoding", hm.RemoveHeaders)
	}
}

func TestExtProc_BodyBuffering_Inbound(t *testing.T) {
	recorder := &bodyRecorderPlugin{}
	p, err := pipeline.New([]pipeline.Plugin{recorder})
	if err != nil {
		t.Fatal(err)
	}

	outbound, err := plugintesting.BuildPipeline([]pipeline.Plugin{plugintesting.NewTokenExchange(auth.New(auth.Config{}))})
	if err != nil {
		t.Fatal(err)
	}

	srv := &Server{InboundPipeline: pipeline.NewHolder(p), OutboundPipeline: pipeline.NewHolder(outbound)}

	body := []byte(`{"method":"tools/call","id":1,"params":{"name":"get_weather"}}`)
	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			inboundRequest(makeHeaders(
				"x-authbridge-direction", "inbound",
				":method", "POST",
				":path", "/mcp",
				"content-length", fmt.Sprintf("%d", len(body)),
			)),
			{
				Request: &extprocv3.ProcessingRequest_RequestBody{
					RequestBody: &extprocv3.HttpBody{Body: body},
				},
			},
		},
	}

	_ = srv.Process(stream)

	if len(stream.responses) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(stream.responses))
	}

	// First response should request body (mode override with BUFFERED)
	first := stream.responses[0]
	rh := first.GetRequestHeaders()
	if rh == nil {
		t.Fatal("first response should be HeadersResponse (requesting body)")
	}
	if first.ModeOverride == nil {
		t.Fatal("expected ModeOverride requesting body buffering")
	}
	if first.ModeOverride.RequestBodyMode != extprocfilterv3.ProcessingMode_BUFFERED {
		t.Errorf("RequestBodyMode = %v, want BUFFERED", first.ModeOverride.RequestBodyMode)
	}

	// Second response should be the body-phase pipeline result (RequestBody response)
	second := stream.responses[1]
	if second.GetRequestBody() == nil && second.GetImmediateResponse() == nil {
		t.Fatal("second response should be a body-phase pipeline result")
	}

	// Plugin should have received the body
	if string(recorder.receivedBody) != string(body) {
		t.Errorf("plugin got body = %q, want %q", recorder.receivedBody, body)
	}
}

func TestExtProc_BodyBuffering_Outbound(t *testing.T) {
	recorder := &bodyRecorderPlugin{}
	p, err := pipeline.New([]pipeline.Plugin{recorder})
	if err != nil {
		t.Fatal(err)
	}

	inbound, err := plugintesting.BuildPipeline([]pipeline.Plugin{
		plugintesting.NewJWTValidation(auth.New(auth.Config{
			Verifier: &mockVerifier{claims: &validation.Claims{Subject: "user"}},
			Identity: auth.IdentityConfig{Audience: "test"},
		}), false),
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := &Server{InboundPipeline: pipeline.NewHolder(inbound), OutboundPipeline: pipeline.NewHolder(p)}

	body := []byte(`{"key":"value"}`)
	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			outboundRequest(makeHeaders(
				":method", "POST",
				":authority", "target-svc",
				"authorization", "Bearer token",
				"content-length", fmt.Sprintf("%d", len(body)),
			)),
			{
				Request: &extprocv3.ProcessingRequest_RequestBody{
					RequestBody: &extprocv3.HttpBody{Body: body},
				},
			},
		},
	}

	_ = srv.Process(stream)

	if len(stream.responses) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(stream.responses))
	}

	// First response requests body
	if stream.responses[0].ModeOverride == nil {
		t.Fatal("expected ModeOverride on first response")
	}

	// Plugin should have received the body
	if string(recorder.receivedBody) != string(body) {
		t.Errorf("plugin got body = %q, want %q", recorder.receivedBody, body)
	}
}

func TestExtProc_BodyTooLarge(t *testing.T) {
	recorder := &bodyRecorderPlugin{}
	p, err := pipeline.New([]pipeline.Plugin{recorder})
	if err != nil {
		t.Fatal(err)
	}

	outbound, err := plugintesting.BuildPipeline([]pipeline.Plugin{plugintesting.NewTokenExchange(auth.New(auth.Config{}))})
	if err != nil {
		t.Fatal(err)
	}

	srv := &Server{InboundPipeline: pipeline.NewHolder(p), OutboundPipeline: pipeline.NewHolder(outbound)}

	bigBody := make([]byte, maxBodySize+1)
	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			inboundRequest(makeHeaders(
				"x-authbridge-direction", "inbound",
				":method", "POST",
				":path", "/mcp",
				"content-length", fmt.Sprintf("%d", len(bigBody)),
			)),
			{
				Request: &extprocv3.ProcessingRequest_RequestBody{
					RequestBody: &extprocv3.HttpBody{Body: bigBody},
				},
			},
		},
	}

	_ = srv.Process(stream)

	if len(stream.responses) < 2 {
		t.Fatalf("expected at least 2 responses, got %d", len(stream.responses))
	}

	// Second response should be an immediate 413 rejection
	second := stream.responses[1]
	ir := second.GetImmediateResponse()
	if ir == nil {
		t.Fatal("expected ImmediateResponse for oversized body")
	}
	if ir.Status.Code != 413 {
		t.Errorf("status = %d, want 413", ir.Status.Code)
	}
}

func TestExtProc_NoBodyBuffering_WhenNotNeeded(t *testing.T) {
	a := auth.New(auth.Config{
		Verifier: &mockVerifier{claims: &validation.Claims{Subject: "user"}},
		Identity: auth.IdentityConfig{Audience: "my-agent"},
	})
	srv := serverFromAuth(t, a)

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			inboundRequest(makeHeaders(
				"x-authbridge-direction", "inbound",
				"authorization", "Bearer valid-token",
				":path", "/api/test",
			)),
		},
	}

	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response (no body phase), got %d", len(stream.responses))
	}
	// Should NOT have ModeOverride
	if stream.responses[0].ModeOverride != nil {
		t.Error("should not request body when pipeline doesn't need it")
	}
}

func TestRekeyInboundSession(t *testing.T) {
	cases := []struct {
		name         string
		direction    string
		a2a          *pipeline.A2AExtension
		seedDefault  bool
		wantDefault  bool
		wantNewID    string
		wantActiveID string
	}{
		{
			name:         "inbound rekey migrates default to contextId",
			direction:    "inbound",
			a2a:          &pipeline.A2AExtension{SessionID: "ctx-abc"},
			seedDefault:  true,
			wantDefault:  false,
			wantNewID:    "ctx-abc",
			wantActiveID: "ctx-abc",
		},
		{
			name:        "outbound direction is a no-op",
			direction:   "outbound",
			a2a:         &pipeline.A2AExtension{SessionID: "ctx-abc"},
			seedDefault: true,
			wantDefault: true,
		},
		{
			name:        "nil A2A extension is a no-op",
			direction:   "inbound",
			a2a:         nil,
			seedDefault: true,
			wantDefault: true,
		},
		{
			name:        "empty SessionID is a no-op",
			direction:   "inbound",
			a2a:         &pipeline.A2AExtension{SessionID: ""},
			seedDefault: true,
			wantDefault: true,
		},
		{
			name:        "SessionID equal to DefaultSessionID is a no-op",
			direction:   "inbound",
			a2a:         &pipeline.A2AExtension{SessionID: session.DefaultSessionID},
			seedDefault: true,
			wantDefault: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := session.New(5*time.Minute, 100, 0)
			defer store.Close()
			if tc.seedDefault {
				store.Append(session.DefaultSessionID, pipeline.SessionEvent{Direction: pipeline.Inbound})
			}
			s := &Server{Sessions: store}
			pctx := &pipeline.Context{Extensions: pipeline.Extensions{A2A: tc.a2a}}

			s.rekeyInboundSession(pctx, tc.direction)

			hasDefault := store.View(session.DefaultSessionID) != nil
			if hasDefault != tc.wantDefault {
				t.Errorf("default session present = %v, want %v", hasDefault, tc.wantDefault)
			}
			if tc.wantNewID != "" && store.View(tc.wantNewID) == nil {
				t.Errorf("expected session under %q after rekey", tc.wantNewID)
			}
			if tc.wantActiveID != "" && store.ActiveSession() != tc.wantActiveID {
				t.Errorf("ActiveSession = %q, want %q", store.ActiveSession(), tc.wantActiveID)
			}
		})
	}
}

func TestRekeyInboundSession_NilStore(t *testing.T) {
	// Session tracking disabled — must not panic.
	s := &Server{Sessions: nil}
	s.rekeyInboundSession(&pipeline.Context{
		Extensions: pipeline.Extensions{A2A: &pipeline.A2AExtension{SessionID: "ctx-abc"}},
	}, "inbound")
}

func TestRecordInboundResponseSession(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			A2A: &pipeline.A2AExtension{
				Method:    "message/stream",
				SessionID: "ctx-abc",
			},
		},
	}
	s.recordInboundResponseSession(pctx)

	v := store.View("ctx-abc")
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 event under ctx-abc, got %v", v)
	}
	e := v.Events[0]
	if e.Direction != pipeline.Inbound || e.Phase != pipeline.SessionResponse {
		t.Errorf("event fields = (%v, %v), want (Inbound, SessionResponse)", e.Direction, e.Phase)
	}
	if e.A2A == nil || e.A2A.SessionID != "ctx-abc" {
		t.Errorf("A2A extension not attached: %+v", e.A2A)
	}
}

func TestRecordInboundResponseSession_EmptyPctx(t *testing.T) {
	// No A2A, no Auth, no plugin-public Custom entries — nothing to
	// record (parallel to the empty-request gate). Auth-only and plugin-
	// only cases are covered separately below.
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	s.recordInboundResponseSession(&pipeline.Context{})
	if store.View(session.DefaultSessionID) != nil {
		t.Error("no session should have been created with empty pctx")
	}
}

// TestRecordInboundResponseSession_AuthOnly covers the exact scenario
// Option 2 activates for the chart default pipeline (jwt-validation only,
// no A2A parser). The request-phase gate was widened in d55524b but the
// response-phase gate kept the old A2A-only check, so auth-only response
// events were silently dropped — operators saw request rows without their
// paired response rows. Locks the fix.
func TestRecordInboundResponseSession_AuthOnly(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	// Record side filters by phase, so this test passes a response-phase
	// invocation. In production jwt-validation's OnResponse is a no-op,
	// but the test exercises the gate: any response-phase entry is
	// sufficient to record a SessionResponse event.
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			Invocations: &pipeline.Invocations{
				Inbound: []pipeline.Invocation{{
					Plugin: "jwt-validation",
					Phase:  pipeline.InvocationPhaseResponse,
					Action: pipeline.ActionAllow,
					Reason: "authorized",
				}},
			},
		},
		StatusCode: 200,
	}
	s.recordInboundResponseSession(pctx)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 event under default, got %v", v)
	}
	e := v.Events[0]
	if e.Direction != pipeline.Inbound || e.Phase != pipeline.SessionResponse {
		t.Errorf("event fields = (%v, %v), want (Inbound, SessionResponse)", e.Direction, e.Phase)
	}
	if e.Invocations == nil || len(e.Invocations.Inbound) != 1 || e.Invocations.Inbound[0].Action != pipeline.ActionAllow {
		t.Errorf("Invocations not attached correctly: %+v", e.Invocations)
	}
	if e.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", e.StatusCode)
	}
}

// Regression: a fresh inbound request with empty A2A.SessionID must bootstrap
// to DefaultSessionID, NOT inherit ActiveSession() from a prior conversation.
// Before the fix this produced cross-conversation contamination — the new
// conversation's request events landed in the previous conversation's
// rekeyed bucket, and the response-phase event (which used the new
// contextId) ended up orphaned in its own 1-event bucket.
func TestInboundSessionID_DoesNotLeakAcrossConversations(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	// Simulate a prior conversation that finished: rekey "default" to
	// "prev-ctx". ActiveSession now points at "prev-ctx".
	store.Append(session.DefaultSessionID, pipeline.SessionEvent{A2A: &pipeline.A2AExtension{}})
	store.Rekey(session.DefaultSessionID, "prev-ctx")
	if id := store.ActiveSession(); id != "prev-ctx" {
		t.Fatalf("precondition: ActiveSession = %q, want prev-ctx", id)
	}

	s := &Server{Sessions: store}

	// New conversation's first inbound request — empty A2A.SessionID.
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{A2A: &pipeline.A2AExtension{Method: "message/send"}},
	}
	s.recordInboundSession(pctx)

	// Must land in DefaultSessionID, NOT prev-ctx.
	if v := store.View(session.DefaultSessionID); v == nil || len(v.Events) != 1 {
		t.Errorf("expected new request under DefaultSessionID, got %v", v)
	}
	if v := store.View("prev-ctx"); v == nil || len(v.Events) != 1 {
		t.Errorf("prev-ctx should be unchanged (1 seed event), got %v", v)
	}
}

func TestRecordOutboundResponseSession_MCP(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	store.Append(session.DefaultSessionID, pipeline.SessionEvent{Direction: pipeline.Inbound}) // seed active
	s := &Server{Sessions: store}

	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			MCP: &pipeline.MCPExtension{
				Method: "tools/call",
				Result: map[string]any{"content": []any{map[string]any{"text": "result"}}},
			},
		},
	}
	s.recordOutboundResponseSession(pctx)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 2 { // 1 seed + 1 response
		t.Fatalf("expected 2 events, got %v", v)
	}
	resp := v.Events[1]
	if resp.Direction != pipeline.Outbound || resp.Phase != pipeline.SessionResponse {
		t.Errorf("event fields = (%v, %v), want (Outbound, SessionResponse)", resp.Direction, resp.Phase)
	}
	if resp.MCP == nil || resp.MCP.Result["content"] == nil {
		t.Errorf("MCP Result not attached: %+v", resp.MCP)
	}
}

// Regression: record helpers used to store the live pctx.Extensions.*
// pointers on SessionEvent. When OnResponse later mutated those same
// structs (setting Completion/FinishReason/Tokens on Inference, or
// Result/Err on MCP), the already-appended request event's view was
// retroactively rewritten to include the eventual response's fields.
// Snapshot-at-record-time prevents that cross-phase bleed.
func TestRecordOutboundSession_SnapshotsInferenceAgainstLaterMutation(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	inf := &pipeline.InferenceExtension{
		Model: "llama3", Messages: []pipeline.InferenceMessage{{Role: "user"}},
	}
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{Inference: inf},
	}
	s.recordOutboundSession(pctx)

	// Simulate OnResponse mutating the live extension after the request
	// event was recorded.
	inf.Completion = "answer"
	inf.FinishReason = "stop"
	inf.PromptTokens = 10
	inf.CompletionTokens = 5
	inf.TotalTokens = 15

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 event, got %+v", v)
	}
	got := v.Events[0].Inference
	if got == nil {
		t.Fatal("Inference is nil on request event")
	}
	if got.Completion != "" || got.FinishReason != "" || got.TotalTokens != 0 {
		t.Errorf("request event's Inference was mutated by later response-side updates: %+v", got)
	}
}

func TestRecordOutboundSession_SnapshotsMCPAgainstLaterMutation(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	mcp := &pipeline.MCPExtension{Method: "tools/call"}
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{MCP: mcp},
	}
	s.recordOutboundSession(pctx)

	// Simulate OnResponse attaching a Result.
	mcp.Result = map[string]any{"content": "ok"}

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 event, got %+v", v)
	}
	if v.Events[0].MCP.Result != nil {
		t.Errorf("request event's MCP.Result was mutated by later response: %+v", v.Events[0].MCP.Result)
	}
}

func TestRecordOutboundResponseSession_Inference(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	store.Append(session.DefaultSessionID, pipeline.SessionEvent{})
	s := &Server{Sessions: store}

	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			Inference: &pipeline.InferenceExtension{
				Model:            "llama3",
				Completion:       "Hello",
				FinishReason:     "stop",
				PromptTokens:     10,
				CompletionTokens: 5,
				TotalTokens:      15,
			},
		},
	}
	s.recordOutboundResponseSession(pctx)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 2 {
		t.Fatalf("expected 2 events, got %v", v)
	}
	resp := v.Events[1]
	if resp.Inference == nil || resp.Inference.Completion != "Hello" {
		t.Errorf("Inference not attached with response data: %+v", resp.Inference)
	}
	if resp.Inference.TotalTokens != 15 {
		t.Errorf("TotalTokens = %d, want 15", resp.Inference.TotalTokens)
	}
}

func TestRecordOutboundResponseSession_NothingToRecord(t *testing.T) {
	// Neither MCP nor Inference populated — nothing to write.
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	s.recordOutboundResponseSession(&pipeline.Context{})
	if store.View(session.DefaultSessionID) != nil {
		t.Error("no session should have been created without MCP/Inference")
	}
}

func TestRecordResponseSessions_NilStore(t *testing.T) {
	// Session tracking disabled — both helpers must be safe to call.
	s := &Server{Sessions: nil}
	s.recordInboundResponseSession(&pipeline.Context{
		Extensions: pipeline.Extensions{A2A: &pipeline.A2AExtension{}},
	})
	s.recordOutboundResponseSession(&pipeline.Context{
		Extensions: pipeline.Extensions{MCP: &pipeline.MCPExtension{}},
	})
}

func TestSnapshotIdentity(t *testing.T) {
	cases := []struct {
		name     string
		identity pipeline.Identity
		agent    *pipeline.AgentIdentity
		want     *pipeline.EventIdentity
	}{
		{
			name: "identity and agent both set",
			identity: stubIdentity{
				subject:  "alice",
				clientID: "kagenti-ui",
				scopes:   []string{"openid", "weather-read"},
			},
			agent: &pipeline.AgentIdentity{WorkloadID: "spiffe://localtest.me/ns/team1/sa/weather-agent"},
			want: &pipeline.EventIdentity{
				Subject:  "alice",
				ClientID: "kagenti-ui",
				Scopes:   []string{"openid", "weather-read"},
				AgentID:  "spiffe://localtest.me/ns/team1/sa/weather-agent",
			},
		},
		{
			name:     "only agent (outbound, no JWT validation)",
			identity: nil,
			agent:    &pipeline.AgentIdentity{WorkloadID: "spiffe://localtest.me/ns/team1/sa/weather-agent"},
			want:     &pipeline.EventIdentity{AgentID: "spiffe://localtest.me/ns/team1/sa/weather-agent"},
		},
		{
			name:     "neither set",
			identity: nil,
			agent:    nil,
			want:     nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := snapshotIdentity(&pipeline.Context{Identity: tc.identity, Agent: tc.agent})
			if tc.want == nil {
				if got != nil {
					t.Errorf("got %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil, want populated")
			}
			if got.Subject != tc.want.Subject || got.ClientID != tc.want.ClientID || got.AgentID != tc.want.AgentID {
				t.Errorf("identity mismatch: got %+v, want %+v", got, tc.want)
			}
			if len(got.Scopes) != len(tc.want.Scopes) {
				t.Errorf("scopes len: got %d, want %d", len(got.Scopes), len(tc.want.Scopes))
			}
		})
	}
}

func TestSnapshotIdentity_ScopesDeepCopy(t *testing.T) {
	// Mutating the source slice after snapshot must not mutate the event.
	scopes := []string{"a", "b"}
	id := snapshotIdentity(&pipeline.Context{Identity: stubIdentity{subject: "alice", scopes: scopes}})
	scopes[0] = "x"
	if id.Scopes[0] != "a" {
		t.Errorf("snapshot scopes aliased original: got %v", id.Scopes)
	}
}

func TestDeriveError(t *testing.T) {
	cases := []struct {
		name     string
		pctx     *pipeline.Context
		wantNil  bool
		wantKind string
		wantCode string
	}{
		{name: "2xx no block", pctx: &pipeline.Context{StatusCode: 200}, wantNil: true},
		{name: "no status (request phase)", pctx: &pipeline.Context{}, wantNil: true},
		{name: "404 backend_error", pctx: &pipeline.Context{StatusCode: 404}, wantKind: "backend_error", wantCode: "404"},
		{name: "500 backend_error", pctx: &pipeline.Context{StatusCode: 500}, wantKind: "backend_error", wantCode: "500"},
		{
			name: "guardrail block wins over status",
			pctx: &pipeline.Context{
				StatusCode: 200,
				Extensions: pipeline.Extensions{
					Security: &pipeline.SecurityExtension{Blocked: true, BlockReason: "pii_detected"},
				},
			},
			wantKind: "blocked",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveError(tc.pctx)
			if tc.wantNil {
				if got != nil {
					t.Errorf("got %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil, want error")
			}
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if tc.wantCode != "" && got.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", got.Code, tc.wantCode)
			}
		})
	}
}

func TestRecordOutboundResponseSession_CapturesStatusAndError(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	store.Append(session.DefaultSessionID, pipeline.SessionEvent{})
	s := &Server{Sessions: store}

	pctx := &pipeline.Context{
		StatusCode: 503,
		Identity:   stubIdentity{subject: "alice"},
		Extensions: pipeline.Extensions{
			MCP: &pipeline.MCPExtension{Method: "tools/call"},
		},
	}
	s.recordOutboundResponseSession(pctx)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) < 2 {
		t.Fatalf("expected event appended, got %v", v)
	}
	resp := v.Events[len(v.Events)-1]
	if resp.StatusCode != 503 {
		t.Errorf("StatusCode = %d, want 503", resp.StatusCode)
	}
	if resp.Error == nil || resp.Error.Kind != "backend_error" || resp.Error.Code != "503" {
		t.Errorf("Error = %+v, want backend_error/503", resp.Error)
	}
	if resp.Identity == nil || resp.Identity.Subject != "alice" {
		t.Errorf("Identity not snapshotted: %+v", resp.Identity)
	}
}

func TestDurationSince(t *testing.T) {
	if d := durationSince(time.Time{}); d != 0 {
		t.Errorf("zero StartedAt should yield 0, got %v", d)
	}
	start := time.Now().Add(-50 * time.Millisecond)
	if d := durationSince(start); d < 50*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 50ms", d)
	}
}

func TestRecordOutboundResponseSession_CapturesHostAndDuration(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	store.Append(session.DefaultSessionID, pipeline.SessionEvent{})
	s := &Server{Sessions: store}

	start := time.Now().Add(-25 * time.Millisecond)
	pctx := &pipeline.Context{
		StartedAt:  start,
		Host:       "github-tool-mcp",
		StatusCode: 200,
		Extensions: pipeline.Extensions{
			MCP: &pipeline.MCPExtension{Method: "tools/call"},
		},
	}
	s.recordOutboundResponseSession(pctx)

	v := store.View(session.DefaultSessionID)
	resp := v.Events[len(v.Events)-1]
	if resp.Host != "github-tool-mcp" {
		t.Errorf("Host = %q, want github-tool-mcp", resp.Host)
	}
	if resp.Duration < 25*time.Millisecond {
		t.Errorf("Duration = %v, want >= 25ms", resp.Duration)
	}
}

// TestRecordInboundSession_AuthOnly verifies the widened gate: a request
// that never reached a protocol parser (A2A is nil) but populated the
// Auth extension still gets recorded. This is the session-stream
// visibility path for auth decisions that don't carry conversation
// payload — e.g., an authorized inbound ping to a non-A2A endpoint.
func TestRecordInboundSession_AuthOnly(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			Invocations: &pipeline.Invocations{
				Inbound: []pipeline.Invocation{{
					Plugin: "jwt-validation",
					Phase:  pipeline.InvocationPhaseRequest,
					Action: pipeline.ActionAllow,
					Reason: "authorized",
				}},
			},
		},
	}
	s.recordInboundSession(pctx)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 event under default session, got %v", v)
	}
	ev := v.Events[0]
	if ev.Invocations == nil || len(ev.Invocations.Inbound) != 1 {
		t.Fatalf("Invocations.Inbound not snapshotted: %+v", ev.Invocations)
	}
	if ev.Invocations.Inbound[0].Action != pipeline.ActionAllow {
		t.Errorf("Action lost in snapshot: %+v", ev.Invocations.Inbound[0])
	}
}

// TestRecordInboundReject_EmitsDeniedPhase verifies the new denial
// recording path: when the pipeline rejects an inbound request, a
// SessionDenied event appears with the Auth diagnostic context and the
// Violation mapped onto StatusCode + EventError. Before this, denied
// requests were invisible in /v1/sessions.
func TestRecordInboundReject_EmitsDeniedPhase(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	pctx := &pipeline.Context{
		StartedAt: time.Now().Add(-10 * time.Millisecond),
		Extensions: pipeline.Extensions{
			Invocations: &pipeline.Invocations{
				Inbound: []pipeline.Invocation{{
					Plugin:           "jwt-validation",
					Phase:            pipeline.InvocationPhaseRequest,
					Action:           pipeline.ActionDeny,
					Reason:           "jwt_failed",
					ExpectedIssuer:   "http://issuer.example",
					ExpectedAudience: "agent-aud",
				}},
			},
		},
	}
	action := pipeline.DenyStatus(401, "auth.unauthorized", "token validation failed")
	s.recordInboundReject(pctx, action)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 event under default session, got %v", v)
	}
	ev := v.Events[0]
	if ev.Phase != pipeline.SessionDenied {
		t.Errorf("Phase = %v, want SessionDenied", ev.Phase)
	}
	if ev.StatusCode != 401 {
		t.Errorf("StatusCode = %d, want 401", ev.StatusCode)
	}
	if ev.Error == nil || ev.Error.Code != "auth.unauthorized" {
		t.Errorf("Error = %+v, want code=auth.unauthorized", ev.Error)
	}
	if ev.Invocations == nil || len(ev.Invocations.Inbound) != 1 || ev.Invocations.Inbound[0].Action != pipeline.ActionDeny {
		t.Errorf("Invocations context lost on denied event: %+v", ev.Invocations)
	}
	if ev.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0", ev.Duration)
	}
}

// TestRecordInboundReject_SkipsWithoutAuth ensures the denial recording
// path is gated on Auth being populated — otherwise every plugin reject
// (including those unrelated to auth, e.g. body-size-exceeded) would
// land in the session stream with no useful context. Stats counters are
// the right place for those; session denials are for auth-class events.
func TestRecordInboundReject_SkipsWithoutAuth(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	action := pipeline.DenyStatus(413, "request.too-large", "body too large")
	s.recordInboundReject(&pipeline.Context{}, action)

	if v := store.View(session.DefaultSessionID); v != nil {
		t.Errorf("expected no event recorded, got %+v", v)
	}
}

// TestSnapshotPlugins_FiltersByEventSuffix verifies the plugin-public
// observability convention: only Custom entries with keys ending in
// pipeline.PluginEventSuffix are promoted to SessionEvent.Plugins.
// Plugin-private state (Custom entries without the suffix, used by
// SetState / GetState) stays out of the session stream.
func TestSnapshotPlugins_FiltersByEventSuffix(t *testing.T) {
	type rateLimiterPrivate struct {
		TokenBucket int
	}
	type rateLimiterEvent struct {
		Allowed    bool `json:"allowed"`
		TokensLeft int  `json:"tokensLeft"`
	}
	custom := map[string]any{
		// Private state — stored by SetState for cross-phase continuity.
		// Must NOT appear in SessionEvent.Plugins.
		"rate-limiter": &rateLimiterPrivate{TokenBucket: 17},
		// Public event — stored with the "/event" suffix. Must appear,
		// keyed by "rate-limiter" (suffix stripped) in the output map.
		"rate-limiter" + pipeline.PluginEventSuffix: rateLimiterEvent{
			Allowed: true, TokensLeft: 42,
		},
	}
	out := snapshotPlugins(custom)
	if _, private := out["rate-limiter"]; !private {
		// Key exists in out because the /event entry WAS promoted.
		// Clarifying: we want exactly one entry, keyed "rate-limiter".
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 promoted plugin event, got %d: %+v", len(out), out)
	}
	raw, ok := out["rate-limiter"]
	if !ok {
		t.Fatalf("expected key 'rate-limiter' (suffix stripped), got keys %v",
			keysOf(out))
	}
	// Round-trip JSON to verify the payload is intact.
	var got rateLimiterEvent
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Allowed || got.TokensLeft != 42 {
		t.Errorf("payload drifted: %+v", got)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
