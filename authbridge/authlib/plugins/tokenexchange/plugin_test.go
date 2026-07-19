package tokenexchange

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
	fwspiffe "github.com/rossoctl/rossocortex/authbridge/authlib/spiffe"
)

// fakeJWTSource is a minimal fwspiffe.JWTSource for unit-testing the
// SPIFFE identity wiring without spinning up a real SPIRE socket.
type fakeJWTSource struct {
	token string
	err   error
}

func (f *fakeJWTSource) FetchToken(_ context.Context) (string, error) {
	return f.token, f.err
}

// setJWTSourceForTest installs a fake JWTSource on the plugin, bypassing
// the SPIFFE Provider. Lives in the test file so the production API
// surface stays clean. Use only from tests in this package.
func (p *TokenExchange) setJWTSourceForTest(j fwspiffe.JWTSource) {
	p.testJWTSource = j
}

func invokeOnRequest(p pipeline.Plugin, pctx *pipeline.Context) pipeline.Action {
	pctx.SetCurrentPlugin(p.Name(), pipeline.InvocationPhaseRequest)
	defer pctx.ClearCurrentPlugin()
	return p.OnRequest(context.Background(), pctx)
}

// --- Configure ---

func TestTokenExchange_Configure_MissingTokenURL(t *testing.T) {
	p := NewTokenExchange()
	if err := p.Configure([]byte(`{"identity":{"type":"client-secret","client_id":"c","client_secret":"s"}}`)); err == nil {
		t.Fatal("expected error for missing token_url")
	}
}

func TestTokenExchange_Configure_DerivesTokenURL(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "keycloak_url":"http://keycloak:8080",
	  "keycloak_realm":"rossoctl",
	  "identity":{"type":"client-secret","client_id":"c","client_secret":"s"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	want := "http://keycloak:8080/realms/rossoctl/protocol/openid-connect/token"
	if p.cfg.TokenURL != want {
		t.Errorf("token_url = %q, want %q", p.cfg.TokenURL, want)
	}
}

func TestTokenExchange_Configure_DefaultIdentityPaths_SPIFFE(t *testing.T) {
	// T8 dropped the per-plugin jwt_svid_path field; the JWTSource is
	// supplied by the framework SPIFFE provider (T11). With T11's wiring
	// in place, Configure on a spiffe-typed plugin succeeds when a
	// JWTSource is injected, and the client_id_file default is applied.
	p := NewTokenExchange()
	p.setJWTSourceForTest(&fakeJWTSource{token: "test-jwt"})
	raw := []byte(`{
	  "token_url":"http://t",
	  "identity":{"type":"spiffe","jwt_audience":"http://kc/realms/test"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.cfg.Identity.ClientIDFile != "/shared/client-id.txt" {
		t.Errorf("ClientIDFile = %q, want /shared/client-id.txt", p.cfg.Identity.ClientIDFile)
	}
}

func TestTokenExchange_Configure_DefaultIdentityPaths_ClientSecret(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"http://t",
	  "identity":{"type":"client-secret"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.cfg.Identity.ClientIDFile != "/shared/client-id.txt" {
		t.Errorf("ClientIDFile = %q, want /shared/client-id.txt", p.cfg.Identity.ClientIDFile)
	}
	if p.cfg.Identity.ClientSecretFile != "/shared/client-secret.txt" {
		t.Errorf("ClientSecretFile = %q, want /shared/client-secret.txt", p.cfg.Identity.ClientSecretFile)
	}
}

func TestTokenExchange_Configure_InlineIdentitySuppressesFileDefaults(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"http://t",
	  "identity":{"type":"client-secret","client_id":"c","client_secret":"s"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.cfg.Identity.ClientIDFile != "" {
		t.Errorf("ClientIDFile = %q, want empty", p.cfg.Identity.ClientIDFile)
	}
	if p.cfg.Identity.ClientSecretFile != "" {
		t.Errorf("ClientSecretFile = %q, want empty", p.cfg.Identity.ClientSecretFile)
	}
}

func TestTokenExchange_Configure_DefaultRoutesFile(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"http://t",
	  "identity":{"type":"client-secret","client_id":"c","client_secret":"s"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.cfg.Routes.File != "/etc/authproxy/routes.yaml" {
		t.Errorf("Routes.File = %q, want /etc/authproxy/routes.yaml", p.cfg.Routes.File)
	}
}

func TestTokenExchange_Configure_DefaultsPassthrough(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"http://token",
	  "identity":{"type":"client-secret","client_id":"c","client_secret":"s"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.cfg.DefaultPolicy != "passthrough" {
		t.Errorf("default_policy = %q, want passthrough", p.cfg.DefaultPolicy)
	}
}

func TestTokenExchange_Configure_InvalidDefaultPolicy(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"http://token",
	  "default_policy":"nope",
	  "identity":{"type":"client-secret","client_id":"c","client_secret":"s"}
	}`)
	if err := p.Configure(raw); err == nil {
		t.Fatal("expected error for invalid default_policy")
	}
}

func TestTokenExchange_Configure_IdentityValidation(t *testing.T) {
	cases := []struct{ name, raw string }{
		{"type missing", `{"token_url":"http://t"}`},
		{"type unknown", `{"token_url":"http://t","identity":{"type":"whatever"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := NewTokenExchange()
			if err := p.Configure([]byte(c.raw)); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

// --- Ready ---

func TestTokenExchange_Ready_InlineCredentials(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"http://t",
	  "identity":{"type":"client-secret","client_id":"c","client_secret":"s"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if !p.Ready() {
		t.Error("expected Ready() == true with inline credentials")
	}
}

func TestTokenExchange_Ready_PendingWithoutCredentials(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"http://t",
	  "identity":{"type":"client-secret"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.Ready() {
		t.Error("expected Ready() == false when defaulted credential files don't exist")
	}
}

// --- OnRequest ---

func TestTokenExchange_Passthrough(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"http://unused",
	  "default_policy":"passthrough",
	  "identity":{"type":"client-secret","client_id":"c","client_secret":"s"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "some-host",
		Headers:   http.Header{"Authorization": []string{"Bearer user-token"}},
	}
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("got %v, want Continue", action.Type)
	}
	if pctx.Headers.Get("Authorization") != "Bearer user-token" {
		t.Error("headers should not be modified for passthrough")
	}
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Outbound) != 1 {
		t.Fatalf("expected one Outbound entry, got %+v", pctx.Extensions.Invocations)
	}
	ob := pctx.Extensions.Invocations.Outbound[0]
	if ob.Plugin != "token-exchange" || ob.Action != pipeline.ActionSkip {
		t.Errorf("entry = (%q, %q), want (token-exchange, skip)", ob.Plugin, ob.Action)
	}
	if ob.Details["route_host"] != "some-host" {
		t.Errorf("route_host = %q, want some-host", ob.Details["route_host"])
	}
	if ob.Details["route_matched"] != "false" {
		t.Errorf("route_matched = %q, want false on default-policy passthrough", ob.Details["route_matched"])
	}
}

func TestTokenExchange_ExchangeSuccess(t *testing.T) {
	exchangeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-token", "token_type": "Bearer", "expires_in": 300,
		})
	}))
	defer exchangeSrv.Close()

	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"` + exchangeSrv.URL + `",
	  "default_policy":"exchange",
	  "identity":{"type":"client-secret","client_id":"agent","client_secret":"secret"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{"Authorization": []string{"Bearer user-token"}},
	}
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("got %v, want Continue", action.Type)
	}
	if pctx.Headers.Get("Authorization") != "Bearer new-token" {
		t.Errorf("token = %q, want Bearer new-token", pctx.Headers.Get("Authorization"))
	}
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Outbound) != 1 {
		t.Fatalf("expected one entry, got %+v", pctx.Extensions.Invocations)
	}
	got := pctx.Extensions.Invocations.Outbound[0]
	if got.Plugin != "token-exchange" || got.Action != pipeline.ActionModify {
		t.Errorf("got Plugin=%q Action=%q, want token-exchange/modify", got.Plugin, got.Action)
	}
	if got.Details["route_host"] != "target-svc" {
		t.Errorf("route_host = %q, want target-svc", got.Details["route_host"])
	}
	if got.Details["cache_hit"] != "false" {
		t.Errorf("cache_hit = %q, want false on first exchange", got.Details["cache_hit"])
	}
}

func TestTokenExchange_ExchangeFailure(t *testing.T) {
	exchangeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer exchangeSrv.Close()

	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"` + exchangeSrv.URL + `",
	  "default_policy":"exchange",
	  "identity":{"type":"client-secret","client_id":"agent","client_secret":"secret"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{"Authorization": []string{"Bearer user-token"}},
	}
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Reject {
		t.Fatalf("got %v, want Reject", action.Type)
	}
	status, _, _ := action.Violation.Render()
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", status)
	}
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Outbound) != 1 {
		t.Fatalf("expected one entry, got %+v", pctx.Extensions.Invocations)
	}
	got := pctx.Extensions.Invocations.Outbound[0]
	if got.Action != pipeline.ActionDeny {
		t.Errorf("Action = %q, want deny", got.Action)
	}
	if got.Reason != "token_exchange_failed" {
		t.Errorf("Reason = %q, want token_exchange_failed", got.Reason)
	}
}

func TestTokenExchange_NoToken_Deny(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"http://unused",
	  "default_policy":"exchange",
	  "no_token_policy":"deny",
	  "identity":{"type":"client-secret","client_id":"c","client_secret":"s"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{},
	}
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Reject {
		t.Fatalf("got %v, want Reject", action.Type)
	}
	status, _, _ := action.Violation.Render()
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
}

// --- SPIFFE provider injection (T11) ---

func TestTokenExchange_SPIFFE_Identity_UsesInjectedJWTSource(t *testing.T) {
	p := NewTokenExchange()
	p.setJWTSourceForTest(&fakeJWTSource{token: "test-jwt"})

	raw := []byte(`{
	  "token_url":"http://example/token",
	  "identity":{"type":"spiffe","client_id":"agent-1","jwt_audience":"http://kc/realms/test"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if !p.Ready() {
		t.Error("expected Ready=true after spiffe Configure with injected JWTSource")
	}
}

func TestTokenExchange_SPIFFE_Identity_ErrorsWhenNoJWTSource(t *testing.T) {
	p := NewTokenExchange()
	// No SetSPIFFEProvider, no setJWTSourceForTest — Configure must
	// fail rather than panic on the spiffe identity path.
	raw := []byte(`{
	  "token_url":"http://example/token",
	  "identity":{"type":"spiffe","client_id":"agent-1","jwt_audience":"http://kc/realms/test"}
	}`)
	if err := p.Configure(raw); err == nil {
		t.Fatal("expected error when spiffe identity has no JWTSource, got nil")
	}
}

// ========================================
// IdP provider registry and endpoint resolution tests
// ========================================

func mustKeycloakProvider(t *testing.T) IdPProvider {
	t.Helper()
	p := LookupProvider("keycloak")
	if p == nil {
		t.Fatal("keycloak provider not registered")
	}
	return p
}

func TestKeycloakProvider_TokenEndpoint(t *testing.T) {
	p := mustKeycloakProvider(t)
	got := p.TokenEndpoint("https://keycloak.example.com", "my-realm")
	want := "https://keycloak.example.com/realms/my-realm/protocol/openid-connect/token"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestKeycloakProvider_TokenEndpoint_TrailingSlash(t *testing.T) {
	p := mustKeycloakProvider(t)
	got := p.TokenEndpoint("https://keycloak.example.com/", "my-realm")
	want := "https://keycloak.example.com/realms/my-realm/protocol/openid-connect/token"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestKeycloakProvider_DefaultAssertionType(t *testing.T) {
	p := mustKeycloakProvider(t)
	if got := p.DefaultAssertionType(); got != "jwt-spiffe" {
		t.Errorf("keycloak default assertion type: got %q, want jwt-spiffe", got)
	}
}

func TestKeycloakProvider_SupportedIdentityTypes(t *testing.T) {
	p := mustKeycloakProvider(t)
	supported := p.SupportedIdentityTypes()
	want := map[string]bool{"client-secret": true, "spiffe": true}
	if len(supported) != len(want) {
		t.Fatalf("keycloak supported identity types: got %v, want %v", supported, want)
	}
	for _, s := range supported {
		if !want[s] {
			t.Errorf("unexpected identity type %q in supported list", s)
		}
	}
}

func TestKeycloakProvider_BuildClientAuth_ClientSecret(t *testing.T) {
	p := mustKeycloakProvider(t)
	id := IdentityConfig{Type: "client-secret", ClientID: "agent-1", ClientSecret: "secret"}
	auth, err := p.BuildClientAuth(id, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth == nil {
		t.Fatal("expected non-nil ClientAuth")
	}
}

func TestKeycloakProvider_BuildClientAuth_Spiffe(t *testing.T) {
	p := mustKeycloakProvider(t)
	jwt := &fakeJWTSource{token: "test-jwt"}
	id := IdentityConfig{Type: "spiffe", ClientID: "agent-1", JWTAudience: "http://kc/realms/test"}
	auth, err := p.BuildClientAuth(id, jwt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth == nil {
		t.Fatal("expected non-nil ClientAuth")
	}
}

func TestKeycloakProvider_BuildClientAuth_UnsupportedType(t *testing.T) {
	p := mustKeycloakProvider(t)
	id := IdentityConfig{Type: "certificate", ClientID: "agent-1"}
	_, err := p.BuildClientAuth(id, nil)
	if err == nil {
		t.Fatal("expected error for unsupported identity type")
	}
}

func TestKeycloakProvider_MissingFields(t *testing.T) {
	p := LookupProvider("keycloak")
	if got := p.TokenEndpoint("", "realm"); got != "" {
		t.Errorf("missing URL should return empty, got %q", got)
	}
	if got := p.TokenEndpoint("https://kc.example.com", ""); got != "" {
		t.Errorf("missing realm should return empty, got %q", got)
	}
}

func TestLookupProvider_UnknownReturnsNil(t *testing.T) {
	if p := LookupProvider("unknown-idp"); p != nil {
		t.Errorf("unknown provider should return nil, got %v", p)
	}
}

func TestLookupProvider_EmptyReturnsNil(t *testing.T) {
	if p := LookupProvider(""); p != nil {
		t.Errorf("empty provider should return nil, got %v", p)
	}
}

// ========================================
// Backward compatibility: keycloak_url/keycloak_realm
// ========================================

func TestConfigure_BackwardCompat_KeycloakURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// token endpoint
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok", "token_type": "Bearer", "expires_in": 3600,
		})
	}))
	defer srv.Close()

	// Use keycloak_url + keycloak_realm (deprecated) — should still work
	raw, _ := json.Marshal(map[string]any{
		"keycloak_url":   srv.URL,
		"keycloak_realm": "test-realm",
		"identity":       map[string]any{"type": "client-secret", "client_id": "agent-1", "client_secret": "secret"},
	})
	p := NewTokenExchange()
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure with keycloak_url/keycloak_realm should succeed: %v", err)
	}
	// Verify provider was set to keycloak
	if p.cfg.Provider != "keycloak" {
		t.Errorf("provider should be auto-set to keycloak, got %q", p.cfg.Provider)
	}
}

func TestConfigure_ProviderURL_Preferred(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok", "token_type": "Bearer", "expires_in": 3600,
		})
	}))
	defer srv.Close()

	// Use provider + provider_url + provider_realm (new way)
	raw, _ := json.Marshal(map[string]any{
		"provider":       "keycloak",
		"provider_url":   srv.URL,
		"provider_realm": "test-realm",
		"identity":       map[string]any{"type": "client-secret", "client_id": "agent-1", "client_secret": "secret"},
	})
	p := NewTokenExchange()
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure with provider fields should succeed: %v", err)
	}
}

// ========================================
// Assertion type configurability
// ========================================

func TestBuildClientAuth_DefaultAssertionType(t *testing.T) {
	jwt := &fakeJWTSource{token: "test-jwt"}
	id := tokenExchangeIdentity{Type: "spiffe", ClientID: "client-1"}
	auth, err := buildClientAuth("keycloak", id, jwt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	form := make(map[string][]string)
	if err := auth.Apply(context.Background(), form); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	got := ""
	if vals, ok := form["client_assertion_type"]; ok && len(vals) > 0 {
		got = vals[0]
	}
	want := "urn:ietf:params:oauth:client-assertion-type:jwt-spiffe"
	if got != want {
		t.Errorf("default assertion type: got %q, want %q", got, want)
	}
}

func TestBuildClientAuth_JWTBearerAssertionType(t *testing.T) {
	jwt := &fakeJWTSource{token: "test-jwt"}
	id := tokenExchangeIdentity{Type: "spiffe", ClientID: "client-1", AssertionType: "jwt-bearer"}
	// Use generic provider (no registered provider) to test fallback
	auth, err := buildClientAuth("", id, jwt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	form := make(map[string][]string)
	if err := auth.Apply(context.Background(), form); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	got := ""
	if vals, ok := form["client_assertion_type"]; ok && len(vals) > 0 {
		got = vals[0]
	}
	want := "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	if got != want {
		t.Errorf("jwt-bearer assertion type: got %q, want %q", got, want)
	}
}

func TestValidate_InvalidAssertionType(t *testing.T) {
	c := tokenExchangeConfig{
		TokenURL:      "http://example.com/token",
		DefaultPolicy: "passthrough",
		NoTokenPolicy: "deny",
		Identity: tokenExchangeIdentity{
			Type:          "spiffe",
			JWTAudience:   "http://kc/realms/test",
			AssertionType: "invalid-type",
		},
	}
	if err := c.validate(); err == nil {
		t.Fatal("expected error for invalid assertion_type, got nil")
	}
}

func TestValidate_ProviderIdentityIncompatibility(t *testing.T) {
	c := tokenExchangeConfig{
		Provider:      "keycloak",
		TokenURL:      "http://example.com/token",
		DefaultPolicy: "passthrough",
		NoTokenPolicy: "deny",
		Identity: tokenExchangeIdentity{
			Type:     "certificate", // keycloak doesn't support certificate
			ClientID: "agent-1",
		},
	}
	if err := c.validate(); err == nil {
		t.Fatal("expected error for unsupported identity type on keycloak, got nil")
	}
}

func TestValidate_UnknownProviderRejected(t *testing.T) {
	c := tokenExchangeConfig{
		Provider:      "nonexistent-idp",
		TokenURL:      "http://example.com/token",
		DefaultPolicy: "passthrough",
		NoTokenPolicy: "deny",
		Identity:      tokenExchangeIdentity{Type: "client-secret", ClientID: "agent-1", ClientSecret: "s"},
	}
	if err := c.validate(); err == nil {
		t.Fatal("expected error for unknown provider, got nil")
	}
}
