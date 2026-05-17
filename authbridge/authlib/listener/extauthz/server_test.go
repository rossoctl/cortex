package extauthz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc/codes"

	authpkg "github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/jwtvalidation/validation"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/plugintesting"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/tokenexchange/cache"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/tokenexchange/exchange"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/routing"
)

type mockVerifier struct {
	claims       *validation.Claims
	err          error
	lastAudience string
}

func (m *mockVerifier) Verify(_ context.Context, _ string, audience string) (*validation.Claims, error) {
	m.lastAudience = audience
	return m.claims, m.err
}

func serverFromAuth(t *testing.T, a *authpkg.Auth) *Server {
	t.Helper()
	// ext_authz is waypoint mode — audience derived from host
	inbound, err := plugintesting.BuildPipeline([]pipeline.Plugin{plugintesting.NewJWTValidation(a, true)})
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

func checkRequest(host, path, authHeader string) *authv3.CheckRequest {
	headers := map[string]string{
		":authority": host,
		":path":      path,
	}
	if authHeader != "" {
		headers["authorization"] = authHeader
	}
	return &authv3.CheckRequest{
		Attributes: &authv3.AttributeContext{
			Request: &authv3.AttributeContext_Request{
				Http: &authv3.AttributeContext_HttpRequest{
					Headers: headers,
					Path:    path,
				},
			},
		},
	}
}

// ServiceNameFromHost is tested in routing/hostutil_test.go (shared implementation)

func TestCheck_ValidToken_Exchange(t *testing.T) {
	exchangeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "exchanged",
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	}))
	defer exchangeSrv.Close()

	mv := &mockVerifier{claims: &validation.Claims{
		Subject: "user", Audience: []string{"caller-agent"},
	}}
	router, _ := routing.NewRouter("exchange", []routing.Route{})
	exchanger := exchange.NewClient(exchangeSrv.URL, &exchange.ClientSecretAuth{
		ClientID: "svc", ClientSecret: "secret",
	})
	a := authpkg.New(authpkg.Config{
		Verifier:  mv,
		Router:    router,
		Exchanger: exchanger,
		Cache:     cache.New(),
		Identity:  authpkg.IdentityConfig{Audience: "default-aud"},
	})
	srv := serverFromAuth(t, a)

	resp, err := srv.Check(context.Background(),
		checkRequest("auth-target-service.authbridge.svc:8081", "/api/test", "Bearer user-token"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Audience should be derived from host
	if mv.lastAudience != "auth-target-service" {
		t.Errorf("audience = %q, want auth-target-service", mv.lastAudience)
	}

	// Should be OK with token replacement
	ok := resp.GetOkResponse()
	if ok == nil {
		t.Fatal("expected OkResponse")
	}
	if len(ok.Headers) == 0 {
		t.Fatal("expected Authorization header override")
	}
	if ok.Headers[0].Header.Value != "Bearer exchanged" {
		t.Errorf("token = %q, want Bearer exchanged", ok.Headers[0].Header.Value)
	}
}

func TestCheck_InvalidToken(t *testing.T) {
	a := authpkg.New(authpkg.Config{
		Verifier: &mockVerifier{err: fmt.Errorf("bad token")},
		Identity: authpkg.IdentityConfig{Audience: "aud"},
	})
	srv := serverFromAuth(t, a)

	resp, _ := srv.Check(context.Background(),
		checkRequest("svc", "/api", "Bearer bad"))

	denied := resp.GetDeniedResponse()
	if denied == nil {
		t.Fatal("expected DeniedResponse")
	}
	if resp.Status.Code != int32(codes.Unauthenticated) {
		t.Errorf("code = %d, want %d", resp.Status.Code, codes.Unauthenticated)
	}
}

func TestCheck_MissingHTTPAttributes(t *testing.T) {
	a := authpkg.New(authpkg.Config{})
	srv := serverFromAuth(t, a)

	resp, _ := srv.Check(context.Background(), &authv3.CheckRequest{})

	denied := resp.GetDeniedResponse()
	if denied == nil {
		t.Fatal("expected DeniedResponse for missing attributes")
	}
}

// schemeCapturePlugin records pctx.Scheme. Duplicated in each listener
// test package because these are the smallest-unit Plugin types that
// don't belong in authlib.
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

// TestCheck_PopulatesSchemeFromAttribute verifies that both the
// inbound and outbound pctx constructed by Check() read Scheme from
// the CheckRequest's HTTP attribute, so plugins composing target
// URLs or branching on transport see the right value in waypoint
// mode.
func TestCheck_PopulatesSchemeFromAttribute(t *testing.T) {
	tests := []struct {
		name   string
		scheme string
		want   string
	}{
		{name: "http", scheme: "http", want: "http"},
		{name: "https", scheme: "https", want: "https"},
		{name: "empty_passes_through", scheme: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inCap := &schemeCapturePlugin{}
			outCap := &schemeCapturePlugin{}
			in, err := plugintesting.BuildPipeline([]pipeline.Plugin{inCap})
			if err != nil {
				t.Fatalf("BuildPipeline inbound: %v", err)
			}
			out, err := plugintesting.BuildPipeline([]pipeline.Plugin{outCap})
			if err != nil {
				t.Fatalf("BuildPipeline outbound: %v", err)
			}
			srv := &Server{
				InboundPipeline:  pipeline.NewHolder(in),
				OutboundPipeline: pipeline.NewHolder(out),
			}

			req := &authv3.CheckRequest{
				Attributes: &authv3.AttributeContext{
					Request: &authv3.AttributeContext_Request{
						Http: &authv3.AttributeContext_HttpRequest{
							Scheme:  tc.scheme,
							Headers: map[string]string{":authority": "agent.example"},
							Path:    "/x",
						},
					},
				},
			}
			_, _ = srv.Check(context.Background(), req)
			if inCap.got != tc.want {
				t.Errorf("inbound pctx.Scheme = %q, want %q", inCap.got, tc.want)
			}
			if outCap.got != tc.want {
				t.Errorf("outbound pctx.Scheme = %q, want %q", outCap.got, tc.want)
			}
		})
	}
}
