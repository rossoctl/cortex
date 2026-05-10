package plugins

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/bypass"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/validation"
)

// invokeOnRequest mirrors what Pipeline.Run does around each plugin
// dispatch: set the current-plugin / current-phase attribution fields
// on pctx so pctx.Record / Allow / Skip / Observe / Modify fill in
// Plugin and Phase correctly. Tests that call plugin.OnRequest directly
// (bypassing Pipeline.Run) need this wrapper to exercise the same code
// path as production. Without it, Invocations would land with empty
// Plugin and Phase fields.
func invokeOnRequest(p pipeline.Plugin, pctx *pipeline.Context) pipeline.Action {
	pctx.SetCurrentPlugin(p.Name(), pipeline.InvocationPhaseRequest)
	defer pctx.ClearCurrentPlugin()
	return p.OnRequest(context.Background(), pctx)
}

// invokeOnResponse is the response-phase twin of invokeOnRequest.
func invokeOnResponse(p pipeline.Plugin, pctx *pipeline.Context) pipeline.Action {
	pctx.SetCurrentPlugin(p.Name(), pipeline.InvocationPhaseResponse)
	defer pctx.ClearCurrentPlugin()
	return p.OnResponse(context.Background(), pctx)
}

// TestAuthbridgeCombinedYAML_Loads asserts that the in-repo default
// config consumed by the combined sidecar image
// (authbridge/authproxy/authbridge-combined.yaml) parses, env-expands,
// and produces working pipelines. Since that YAML leans on per-plugin
// defaults for file paths and bypass patterns, a future rename of any
// default constant would silently break the shipped image unless this
// test fails. It's cheaper to fail in CI than in a running pod.
func TestAuthbridgeCombinedYAML_Loads(t *testing.T) {
	// The canonical file path is relative to this test file —
	// plugins_test.go lives in authlib/plugins/, the YAML in
	// authproxy/. Go up two directories, across into authproxy/.
	yamlPath := filepath.Join("..", "..", "authproxy", "authbridge-combined.yaml")
	if _, err := os.Stat(yamlPath); err != nil {
		t.Skipf("authbridge-combined.yaml not found (repo layout changed?): %v", err)
	}

	envs := map[string]string{
		"ISSUER":                  "http://keycloak.localtest.me:8080/realms/kagenti",
		"KEYCLOAK_URL":            "http://keycloak-service.keycloak.svc:8080",
		"KEYCLOAK_REALM":          "kagenti",
		"DEFAULT_OUTBOUND_POLICY": "passthrough",
		"TOKEN_URL":               "", // intentionally empty: the plugin should derive from keycloak_url + realm
	}
	for k, v := range envs {
		t.Setenv(k, v)
	}

	cfg, err := config.Load(yamlPath)
	if err != nil {
		t.Fatalf("Load(%s): %v", yamlPath, err)
	}
	if cfg.Mode != config.ModeEnvoySidecar {
		t.Errorf("mode = %q, want %q", cfg.Mode, config.ModeEnvoySidecar)
	}
	if err := config.Validate(cfg); err != nil {
		t.Errorf("Validate: %v", err)
	}

	// Build both pipelines. Any plugin whose Configure rejects the
	// env-expanded config subtree (e.g. because a default path moved
	// but the YAML still relies on it) fails the build here.
	if _, err := Build(cfg.Pipeline.Inbound.Plugins); err != nil {
		t.Errorf("Build inbound: %v", err)
	}
	if _, err := Build(cfg.Pipeline.Outbound.Plugins); err != nil {
		t.Errorf("Build outbound: %v", err)
	}
}

// --- JWTValidation: Configure ---

func TestJWTValidation_Configure_MissingIssuer(t *testing.T) {
	p := NewJWTValidation()
	err := p.Configure([]byte(`{}`))
	if err == nil {
		t.Fatal("expected error for missing issuer")
	}
}

func TestJWTValidation_Configure_UnknownField(t *testing.T) {
	p := NewJWTValidation()
	err := p.Configure([]byte(`{"issuer":"http://ex","audience":"a","not_a_field":"x"}`))
	if err == nil {
		t.Fatal("expected error for unknown field; DisallowUnknownFields should reject")
	}
}

// Legacy test obsolete: applyDefaults now sets audience_file to
// /shared/client-id.txt when neither audience nor audience_file is
// supplied, so this scenario no longer reaches validate(). The
// replacement test is TestJWTValidation_Configure_DefaultAudienceFile.

func TestJWTValidation_Configure_PerHost(t *testing.T) {
	p := NewJWTValidation()
	// per-host mode does not require an audience field.
	err := p.Configure([]byte(`{"issuer":"http://ex","audience_mode":"per-host"}`))
	if err != nil {
		t.Fatalf("per-host mode should not require audience: %v", err)
	}
	if p.audienceDeriver == nil {
		t.Error("per-host mode should set audienceDeriver")
	}
}

// When neither audience nor audience_file is supplied, the plugin
// defaults audience_file to /shared/client-id.txt (the Kagenti
// client-registration convention). Omitting both in the YAML must not
// fail validation — the file read is best-effort with a background
// fallback poll.
func TestJWTValidation_Configure_DefaultAudienceFile(t *testing.T) {
	p := NewJWTValidation()
	if err := p.Configure([]byte(`{"issuer":"http://ex"}`)); err != nil {
		t.Fatalf("Configure with defaults: %v", err)
	}
	if p.cfg.AudienceFile != "/shared/client-id.txt" {
		t.Errorf("AudienceFile = %q, want /shared/client-id.txt", p.cfg.AudienceFile)
	}
}

// bypass_paths defaults to bypass.DefaultPatterns so health / .well-known
// endpoints don't reject every JWT-less probe from kubelet.
func TestJWTValidation_Configure_DefaultBypassPaths(t *testing.T) {
	p := NewJWTValidation()
	if err := p.Configure([]byte(`{"issuer":"http://ex","audience":"a"}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if len(p.cfg.BypassPaths) == 0 {
		t.Fatal("expected default bypass paths")
	}
}

// Inline audience suppresses the audience_file default: operators who
// supply a literal audience must not also get a surprise file read.
func TestJWTValidation_Configure_InlineAudienceSuppressesFileDefault(t *testing.T) {
	p := NewJWTValidation()
	if err := p.Configure([]byte(`{"issuer":"http://ex","audience":"literal"}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.cfg.AudienceFile != "" {
		t.Errorf("AudienceFile = %q, want empty (inline audience should suppress default)", p.cfg.AudienceFile)
	}
}

func TestJWTValidation_Configure_DefaultsJWKSFromIssuer(t *testing.T) {
	p := NewJWTValidation()
	err := p.Configure([]byte(`{"issuer":"http://keycloak/realms/kagenti","audience":"a"}`))
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	// Issuer-derived fallback; verify the exact URL so a future refactor
	// of the priority chain can't silently fall through to a 404 path.
	if got, want := p.cfg.JWKSURL, "http://keycloak/realms/kagenti/protocol/openid-connect/certs"; got != want {
		t.Errorf("JWKSURL = %q, want %q", got, want)
	}
	if p.inner == nil {
		t.Fatal("Configure produced no inner auth handler")
	}
}

// Split-horizon case: operator supplies a public `issuer` (for iss-claim
// matching) and internal `keycloak_url` + `keycloak_realm` (for reachable
// JWKS fetching). The derivation prefers the internal URL — deriving from
// issuer here would send the request into the public hostname which, in
// Kagenti's Kind/OpenShift setups, resolves to 127.0.0.1 and fails
// "connection refused" (see authbridge CLAUDE.md gotcha #2).
func TestJWTValidation_Configure_DerivesJWKSFromInternalKeycloakURL(t *testing.T) {
	p := NewJWTValidation()
	raw := []byte(`{
		"issuer": "http://keycloak.localtest.me:8080/realms/kagenti",
		"keycloak_url": "http://keycloak-service.keycloak.svc:8080",
		"keycloak_realm": "kagenti",
		"audience": "a"
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	want := "http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/certs"
	if got := p.cfg.JWKSURL; got != want {
		t.Errorf("JWKSURL = %q, want %q (internal URL from keycloak_url+realm, not issuer)", got, want)
	}
}

// Explicit jwks_url beats every derivation source. Operators who know
// exactly which endpoint to hit (e.g. a custom JWKS proxy) must not have
// it silently overwritten by the keycloak_url/realm derivation.
func TestJWTValidation_Configure_ExplicitJWKSURLWins(t *testing.T) {
	p := NewJWTValidation()
	raw := []byte(`{
		"issuer": "http://keycloak.public:8080/realms/kagenti",
		"jwks_url": "http://custom-jwks-proxy.example/keys",
		"keycloak_url": "http://keycloak-internal:8080",
		"keycloak_realm": "kagenti",
		"audience": "a"
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if got, want := p.cfg.JWKSURL, "http://custom-jwks-proxy.example/keys"; got != want {
		t.Errorf("JWKSURL = %q, want %q (explicit jwks_url must win over derivations)", got, want)
	}
}

// Only one of keycloak_url/keycloak_realm is supplied — derivation can't
// complete (Keycloak path needs both). Fall through to issuer-derivation
// rather than crashing or leaving JWKSURL empty.
//
// Covered both directions to catch a future refactor that ANDs the check
// differently (e.g. a short-circuit that treats empty realm as default
// would silently build a bogus URL).
func TestJWTValidation_Configure_PartialKeycloakConfigFallsThroughToIssuer(t *testing.T) {
	cases := []struct {
		name, raw string
	}{
		{
			name: "keycloak_url without realm",
			raw: `{
				"issuer": "http://keycloak/realms/kagenti",
				"keycloak_url": "http://internal:8080",
				"audience": "a"
			}`,
		},
		{
			name: "keycloak_realm without url",
			raw: `{
				"issuer": "http://keycloak/realms/kagenti",
				"keycloak_realm": "kagenti",
				"audience": "a"
			}`,
		},
	}
	want := "http://keycloak/realms/kagenti/protocol/openid-connect/certs"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewJWTValidation()
			if err := p.Configure([]byte(tc.raw)); err != nil {
				t.Fatalf("Configure: %v", err)
			}
			if got := p.cfg.JWKSURL; got != want {
				t.Errorf("JWKSURL = %q, want %q (partial keycloak_* must not short-circuit issuer-derivation)", got, want)
			}
		})
	}
}

func TestJWTValidation_Configure_AudienceFromFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "aud")
	if err := os.WriteFile(f, []byte("my-agent"), 0600); err != nil {
		t.Fatal(err)
	}
	p := NewJWTValidation()
	raw := []byte(`{"issuer":"http://ex","audience_file":"` + f + `"}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if !p.inner.Ready() {
		t.Error("expected inner.Ready() == true after synchronous audience load")
	}
}

// --- JWTValidation: Ready ---

// Synchronous audience load → plugin reports ready immediately after
// Configure. The /readyz probe sees this on the kubelet's first check.
func TestJWTValidation_Ready_AfterSyncLoad(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "aud")
	if err := os.WriteFile(f, []byte("my-agent"), 0600); err != nil {
		t.Fatal(err)
	}
	p := NewJWTValidation()
	if err := p.Configure([]byte(`{"issuer":"http://ex","audience_file":"` + f + `"}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if !p.Ready() {
		t.Error("expected Ready() == true after synchronous audience_file load")
	}
}

// Missing audience_file → plugin not ready until Init's poller flips
// it. Without per-plugin Ready(), /readyz would return 200 and the
// pod would get traffic that immediately 503s.
func TestJWTValidation_Ready_PendingWithoutFile(t *testing.T) {
	p := NewJWTValidation()
	if err := p.Configure([]byte(`{"issuer":"http://ex","audience_file":"/does/not/exist"}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.Ready() {
		t.Error("expected Ready() == false when audience_file is missing")
	}
}

// Per-host mode derives audience per request; no deferred state.
// Must be always-ready so waypoint deployments don't stay unready.
func TestJWTValidation_Ready_PerHostAlwaysReady(t *testing.T) {
	p := NewJWTValidation()
	if err := p.Configure([]byte(`{"issuer":"http://ex","audience_mode":"per-host"}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if !p.Ready() {
		t.Error("expected Ready() == true in per-host mode")
	}
}

// --- JWTValidation: OnRequest ---

func TestJWTValidation_OnRequest_NotConfigured(t *testing.T) {
	p := NewJWTValidation()
	action := invokeOnRequest(p, &pipeline.Context{Headers: http.Header{}})
	if action.Type != pipeline.Reject {
		t.Errorf("got %v, want Reject for unconfigured plugin", action.Type)
	}
}

// --- JWTValidation: Auth extension population ---
//
// These tests verify jwt-validation surfaces its decision on
// pctx.Extensions.Invocations.Inbound so the listener can record a
// SessionEvent reflecting allow/deny/bypass. Plumbed through a
// mockVerifier injected into p.inner (instead of spinning up a real
// JWKS server) — keeps the test focused on plugin behavior, not crypto.

// mockJWTVerifier lets the tests below dictate what the inner validator
// returns without standing up an httptest JWKS server. It implements the
// validation.Verifier interface.
type mockJWTVerifier struct {
	claims *validation.Claims
	err    error
}

func (m *mockJWTVerifier) Verify(_ context.Context, _, _ string) (*validation.Claims, error) {
	return m.claims, m.err
}

// newTestJWTValidation constructs a JWTValidation plugin without calling
// Configure — skips file I/O (audience_file polling, bypass pattern
// compile via config) and lets each test wire a tailored inner auth.Auth.
func newTestJWTValidation(t *testing.T, issuer string, inner *auth.Auth) *JWTValidation {
	t.Helper()
	p := NewJWTValidation()
	p.cfg.Issuer = issuer
	p.inner = inner
	return p
}

func TestJWTValidation_OnRequest_PopulatesAuth_Bypass(t *testing.T) {
	matcher, _ := bypass.NewMatcher(bypass.DefaultPatterns)
	inner := auth.New(auth.Config{
		Bypass:   matcher,
		Verifier: &mockJWTVerifier{claims: &validation.Claims{Subject: "s"}},
		Identity: auth.IdentityConfig{Audience: "agent-aud"},
	})
	p := newTestJWTValidation(t, "http://issuer", inner)

	pctx := &pipeline.Context{Headers: http.Header{}, Path: "/healthz"}
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("bypass should Continue, got %v", action.Type)
	}
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Inbound) != 1 {
		t.Fatalf("expected one Auth.Inbound entry, got %+v", pctx.Extensions.Invocations)
	}
	got := pctx.Extensions.Invocations.Inbound[0]
	if got.Plugin != "jwt-validation" {
		t.Errorf("Plugin = %q, want jwt-validation", got.Plugin)
	}
	if got.Action != pipeline.ActionSkip || got.Reason != "path_bypass" {
		t.Errorf("got Action=%q Reason=%q, want skip/path_bypass", got.Action, got.Reason)
	}
}

func TestJWTValidation_OnRequest_PopulatesAuth_Deny_NoHeader(t *testing.T) {
	inner := auth.New(auth.Config{
		Verifier: &mockJWTVerifier{},
		Identity: auth.IdentityConfig{Audience: "agent-aud"},
	})
	p := newTestJWTValidation(t, "http://issuer.example", inner)

	pctx := &pipeline.Context{Headers: http.Header{}, Path: "/api/call"}
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Reject {
		t.Fatalf("expected Reject on missing auth header, got %v", action.Type)
	}
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Inbound) != 1 {
		t.Fatalf("expected one Auth.Inbound entry, got %+v", pctx.Extensions.Invocations)
	}
	got := pctx.Extensions.Invocations.Inbound[0]
	if got.Action != pipeline.ActionDeny {
		t.Errorf("Action = %q, want deny", got.Action)
	}
	// Reason comes from InboundDenialReason.String() so consumers can
	// filter on a machine-stable code without parsing English.
	if got.Reason != "no_header" {
		t.Errorf("Reason = %q, want no_header", got.Reason)
	}
	if got.Details["expected_issuer"] != "http://issuer.example" {
		t.Errorf("expected_issuer = %q, want http://issuer.example", got.Details["expected_issuer"])
	}
}

func TestJWTValidation_OnRequest_PopulatesAuth_Allow(t *testing.T) {
	claims := &validation.Claims{
		Subject:  "alice",
		Issuer:   "http://issuer.example",
		Audience: []string{"agent-aud"},
		ClientID: "caller",
		Scopes:   []string{"openid", "write"},
	}
	inner := auth.New(auth.Config{
		Verifier: &mockJWTVerifier{claims: claims},
		Identity: auth.IdentityConfig{Audience: "agent-aud"},
	})
	p := newTestJWTValidation(t, "http://issuer.example", inner)

	pctx := &pipeline.Context{Headers: http.Header{}, Path: "/api/call"}
	pctx.Headers.Set("Authorization", "Bearer tok")
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v (violation=%+v)", action.Type, action.Violation)
	}
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Inbound) != 1 {
		t.Fatalf("expected one Auth.Inbound entry, got %+v", pctx.Extensions.Invocations)
	}
	got := pctx.Extensions.Invocations.Inbound[0]
	if got.Action != pipeline.ActionAllow || got.Reason != "authorized" {
		t.Errorf("got Action=%q Reason=%q, want allow/authorized", got.Action, got.Reason)
	}
	if got.Details["token_subject"] != "alice" {
		t.Errorf("token_subject = %q, want alice", got.Details["token_subject"])
	}
	if got.Details["token_scopes"] != "openid write" {
		t.Errorf("token_scopes = %q, want \"openid write\"", got.Details["token_scopes"])
	}
	if got.Details["token_audience"] != "agent-aud" {
		t.Errorf("token_audience = %q, want agent-aud", got.Details["token_audience"])
	}
}

// --- TokenExchange: Configure ---

func TestTokenExchange_Configure_MissingTokenURL(t *testing.T) {
	p := NewTokenExchange()
	err := p.Configure([]byte(`{"identity":{"type":"client-secret","client_id":"c","client_secret":"s"}}`))
	if err == nil {
		t.Fatal("expected error for missing token_url")
	}
}

func TestTokenExchange_Configure_DerivesTokenURL(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "keycloak_url":"http://keycloak:8080",
	  "keycloak_realm":"kagenti",
	  "identity":{"type":"client-secret","client_id":"c","client_secret":"s"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	want := "http://keycloak:8080/realms/kagenti/protocol/openid-connect/token"
	if p.cfg.TokenURL != want {
		t.Errorf("token_url = %q, want %q", p.cfg.TokenURL, want)
	}
}

// Identity file paths default to Kagenti conventions when the operator
// doesn't supply them. Inline values suppress the default.
func TestTokenExchange_Configure_DefaultIdentityPaths_SPIFFE(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"http://t",
	  "identity":{"type":"spiffe"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.cfg.Identity.ClientIDFile != "/shared/client-id.txt" {
		t.Errorf("ClientIDFile = %q, want /shared/client-id.txt", p.cfg.Identity.ClientIDFile)
	}
	if p.cfg.Identity.JWTSVIDPath != "/opt/jwt_svid.token" {
		t.Errorf("JWTSVIDPath = %q, want /opt/jwt_svid.token", p.cfg.Identity.JWTSVIDPath)
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

// Inline identity values must suppress the file defaults, otherwise an
// operator who writes inline credentials could be silently overridden
// by a pre-existing file on the mount point.
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

// Identity type is still required — defaulting covers the *paths* to
// credential files, not the choice between SPIFFE and client-secret.
// Unknown types fall through to the default error branch.
func TestTokenExchange_Configure_IdentityValidation(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
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

// --- TokenExchange: Ready ---

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

// Default /shared/* paths don't exist in the test environment, so
// Configure's sync load fails and Ready stays false. pollCredentials
// would flip it later; this test doesn't call Init.
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

// --- TokenExchange: OnRequest (end-to-end through Configure) ---

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
	// Passthrough populates Auth.Outbound with Action="passthrough" —
	// symmetric with jwt-validation's bypass recording so operators can
	// see every outbound host the pod talks to in the session stream.
	// RouteHost carries the target so they can spot unexpected egress
	// without hunting through slog lines.
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Outbound) != 1 {
		t.Fatalf("expected one Auth.Outbound entry, got %+v", pctx.Extensions.Invocations)
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
			"access_token": "new-token",
			"token_type":   "Bearer",
			"expires_in":   300,
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
	// Auth extension must surface the exchange action so it flows into
	// SessionEvent.Auth.Outbound once the listener records. Empty route
	// (TargetAudience, RequestedScopes) is OK here — this test doesn't
	// configure routes, it uses default_policy=exchange.
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Outbound) != 1 {
		t.Fatalf("expected one Auth.Outbound entry, got %+v", pctx.Extensions.Invocations)
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
	// Deny branch must surface a "denied" Auth.Outbound entry with the
	// machine-stable reason — matches what the listener needs to emit a
	// SessionDenied event on outbound exchange failure.
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Outbound) != 1 {
		t.Fatalf("expected one Auth.Outbound entry, got %+v", pctx.Extensions.Invocations)
	}
	got := pctx.Extensions.Invocations.Outbound[0]
	if got.Action != pipeline.ActionDeny {
		t.Errorf("Action = %q, want deny", got.Action)
	}
	if got.Reason != "token_exchange_failed" {
		t.Errorf("Reason = %q, want token_exchange_failed (from OutboundDenialReason.String)", got.Reason)
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

// --- Stats aggregation ---

// CollectStats walks a pipeline and returns *auth.Stats from each
// plugin implementing StatsSource. Non-Configurable plugins (parsers)
// don't implement StatsSource, so they're skipped; the slice length
// reflects only plugins with observable counters.
func TestCollectStats_CollectsOnlyStatsSources(t *testing.T) {
	jwt := NewJWTValidation()
	if err := jwt.Configure([]byte(`{"issuer":"http://ex","audience":"a"}`)); err != nil {
		t.Fatalf("jwt Configure: %v", err)
	}
	tok := NewTokenExchange()
	if err := tok.Configure([]byte(`{"token_url":"http://t","identity":{"type":"client-secret","client_id":"c","client_secret":"s"}}`)); err != nil {
		t.Fatalf("tok Configure: %v", err)
	}
	// a2a-parser does not implement StatsSource.
	p, err := pipeline.New([]pipeline.Plugin{jwt, NewA2AParser(), tok})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := CollectStats(p)
	if len(got) != 2 {
		t.Errorf("len(CollectStats) = %d, want 2 (jwt + tok, parser skipped)", len(got))
	}
}

// Nil pipeline must not panic — callers often cons up a statsProvider
// closure that references both inbound and outbound pipelines, and
// one could legitimately be nil in a degenerate config.
func TestCollectStats_NilPipeline(t *testing.T) {
	if got := CollectStats(nil); got != nil {
		t.Errorf("CollectStats(nil) = %v, want nil", got)
	}
}

// --- Registry / Build ---

func TestBuild_ValidNames(t *testing.T) {
	p, err := Build([]config.PluginEntry{
		{Name: "a2a-parser"},
		{Name: "mcp-parser"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil pipeline")
	}
}

func TestBuild_UnknownName(t *testing.T) {
	_, err := Build([]config.PluginEntry{{Name: "nonexistent-plugin"}})
	if err == nil {
		t.Fatal("expected error for unknown plugin name")
	}
}

func TestBuild_EmptyList(t *testing.T) {
	p, err := Build([]config.PluginEntry{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	action := p.Run(context.Background(), &pipeline.Context{Headers: http.Header{}})
	if action.Type != pipeline.Continue {
		t.Errorf("empty pipeline got %v, want Continue", action.Type)
	}
}

// A config: block on a plugin that doesn't implement Configurable is a
// startup error. Silent acceptance would hide typos (wrong plugin name)
// and stale config across refactors.
func TestBuild_ConfigForNonConfigurablePlugin(t *testing.T) {
	_, err := Build([]config.PluginEntry{
		{Name: "a2a-parser", Config: []byte(`{"unused":true}`)},
	})
	if err == nil {
		t.Fatal("expected error for config on non-Configurable plugin")
	}
	// Error text is operator-facing contract — a future refactor that
	// changes it must update this assertion intentionally.
	if !strings.Contains(err.Error(), "does not accept configuration") {
		t.Errorf("error %q does not match the operator-facing contract "+
			`"%q does not accept configuration"`, err, "a2a-parser")
	}
}

// Configure errors surface through Build with the offending plugin's
// name so startup logs identify the broken entry without the operator
// having to read every plugin's error wording.
func TestBuild_ConfigureError(t *testing.T) {
	_, err := Build([]config.PluginEntry{
		{Name: "jwt-validation", Config: []byte(`{}`)}, // missing issuer
	})
	if err == nil {
		t.Fatal("expected error for invalid jwt-validation config")
	}
	if !strings.Contains(err.Error(), "jwt-validation") {
		t.Errorf("error %q does not name the offending plugin", err)
	}
}
