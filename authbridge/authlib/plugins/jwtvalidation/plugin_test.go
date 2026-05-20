package jwtvalidation

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/bypass"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/jwtvalidation/validation"
)

// invokeOnRequest mirrors what Pipeline.Run does around each plugin
// dispatch: set the current-plugin / current-phase attribution fields
// on pctx so pctx.Record / Allow / Skip / Observe / Modify fill in
// Plugin and Phase correctly.
func invokeOnRequest(p pipeline.Plugin, pctx *pipeline.Context) pipeline.Action {
	pctx.SetCurrentPlugin(p.Name(), pipeline.InvocationPhaseRequest)
	defer pctx.ClearCurrentPlugin()
	return p.OnRequest(context.Background(), pctx)
}

// --- Configure ---

func TestJWTValidation_Configure_MissingIssuer(t *testing.T) {
	p := NewJWTValidation()
	if err := p.Configure([]byte(`{}`)); err == nil {
		t.Fatal("expected error for missing issuer")
	}
}

func TestJWTValidation_Configure_UnknownField(t *testing.T) {
	p := NewJWTValidation()
	if err := p.Configure([]byte(`{"issuer":"http://ex","audience":"a","not_a_field":"x"}`)); err == nil {
		t.Fatal("expected error for unknown field; DisallowUnknownFields should reject")
	}
}

func TestJWTValidation_Configure_PerHost(t *testing.T) {
	p := NewJWTValidation()
	if err := p.Configure([]byte(`{"issuer":"http://ex","audience_mode":"per-host"}`)); err != nil {
		t.Fatalf("per-host mode should not require audience: %v", err)
	}
	if p.audienceDeriver == nil {
		t.Error("per-host mode should set audienceDeriver")
	}
}

func TestJWTValidation_Configure_DefaultAudienceFile(t *testing.T) {
	p := NewJWTValidation()
	if err := p.Configure([]byte(`{"issuer":"http://ex"}`)); err != nil {
		t.Fatalf("Configure with defaults: %v", err)
	}
	if p.cfg.AudienceFile != "/shared/client-id.txt" {
		t.Errorf("AudienceFile = %q, want /shared/client-id.txt", p.cfg.AudienceFile)
	}
}

func TestJWTValidation_Configure_DefaultBypassPaths(t *testing.T) {
	p := NewJWTValidation()
	if err := p.Configure([]byte(`{"issuer":"http://ex","audience":"a"}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if len(p.cfg.BypassPaths) == 0 {
		t.Fatal("expected default bypass paths")
	}
}

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
	if err := p.Configure([]byte(`{"issuer":"http://keycloak/realms/kagenti","audience":"a"}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if got, want := p.cfg.JWKSURL, "http://keycloak/realms/kagenti/protocol/openid-connect/certs"; got != want {
		t.Errorf("JWKSURL = %q, want %q", got, want)
	}
	if p.inner == nil {
		t.Fatal("Configure produced no inner auth handler")
	}
}

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
		t.Errorf("JWKSURL = %q, want %q", got, want)
	}
}

func TestJWTValidation_Configure_PartialKeycloakConfigFallsThroughToIssuer(t *testing.T) {
	cases := []struct{ name, raw string }{
		{"keycloak_url without realm", `{"issuer":"http://keycloak/realms/kagenti","keycloak_url":"http://internal:8080","audience":"a"}`},
		{"keycloak_realm without url", `{"issuer":"http://keycloak/realms/kagenti","keycloak_realm":"kagenti","audience":"a"}`},
	}
	want := "http://keycloak/realms/kagenti/protocol/openid-connect/certs"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewJWTValidation()
			if err := p.Configure([]byte(tc.raw)); err != nil {
				t.Fatalf("Configure: %v", err)
			}
			if got := p.cfg.JWKSURL; got != want {
				t.Errorf("JWKSURL = %q, want %q", got, want)
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
	if err := p.Configure([]byte(`{"issuer":"http://ex","audience_file":"` + f + `"}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if !p.inner.Ready() {
		t.Error("expected inner.Ready() == true after synchronous audience load")
	}
}

// --- Ready ---

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

func TestJWTValidation_Ready_PendingWithoutFile(t *testing.T) {
	p := NewJWTValidation()
	if err := p.Configure([]byte(`{"issuer":"http://ex","audience_file":"/does/not/exist"}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.Ready() {
		t.Error("expected Ready() == false when audience_file is missing")
	}
}

func TestJWTValidation_Ready_PerHostAlwaysReady(t *testing.T) {
	p := NewJWTValidation()
	if err := p.Configure([]byte(`{"issuer":"http://ex","audience_mode":"per-host"}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if !p.Ready() {
		t.Error("expected Ready() == true in per-host mode")
	}
}

// --- OnRequest ---

func TestJWTValidation_OnRequest_NotConfigured(t *testing.T) {
	p := NewJWTValidation()
	action := invokeOnRequest(p, &pipeline.Context{Headers: http.Header{}})
	if action.Type != pipeline.Reject {
		t.Errorf("got %v, want Reject for unconfigured plugin", action.Type)
	}
}

// mockJWTVerifier lets the tests below dictate what the inner validator
// returns without standing up an httptest JWKS server.
type mockJWTVerifier struct {
	claims *validation.Claims
	err    error
}

func (m *mockJWTVerifier) Verify(_ context.Context, _ string, audiences []string) (*validation.Claims, error) {
	return m.claims, m.err
}

// newTestJWTValidation constructs a JWTValidation plugin without calling
// Configure — skips file I/O and lets each test wire a tailored inner.
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
		Identity: auth.IdentityConfig{Audiences: []string{"agent-aud"}},
	})
	p := newTestJWTValidation(t, "http://issuer", inner)

	pctx := &pipeline.Context{Headers: http.Header{}, Path: "/healthz"}
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("bypass should Continue, got %v", action.Type)
	}
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Inbound) != 1 {
		t.Fatalf("expected one Invocations.Inbound entry, got %+v", pctx.Extensions.Invocations)
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
		Identity: auth.IdentityConfig{Audiences: []string{"agent-aud"}},
	})
	p := newTestJWTValidation(t, "http://issuer.example", inner)

	pctx := &pipeline.Context{Headers: http.Header{}, Path: "/api/call"}
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Reject {
		t.Fatalf("expected Reject on missing auth header, got %v", action.Type)
	}
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Inbound) != 1 {
		t.Fatalf("expected one entry, got %+v", pctx.Extensions.Invocations)
	}
	got := pctx.Extensions.Invocations.Inbound[0]
	if got.Action != pipeline.ActionDeny {
		t.Errorf("Action = %q, want deny", got.Action)
	}
	if got.Reason != "no_header" {
		t.Errorf("Reason = %q, want no_header", got.Reason)
	}
	if got.Details["expected_issuer"] != "http://issuer.example" {
		t.Errorf("expected_issuer = %q, want http://issuer.example", got.Details["expected_issuer"])
	}
	if g, want := got.Details["expected_audiences"], "agent-aud"; g != want {
		t.Errorf("expected_audiences = %q, want %q", g, want)
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
		Identity: auth.IdentityConfig{Audiences: []string{"agent-aud"}},
	})
	p := newTestJWTValidation(t, "http://issuer.example", inner)

	pctx := &pipeline.Context{Headers: http.Header{}, Path: "/api/call"}
	pctx.Headers.Set("Authorization", "Bearer tok")
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v (violation=%+v)", action.Type, action.Violation)
	}
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Inbound) != 1 {
		t.Fatalf("expected one entry, got %+v", pctx.Extensions.Invocations)
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

// TestJWTValidation_OnRequest_MultiAudience_CommaJoined pins the encoding
// of multi-value audiences to comma-join. JWT RFC 7519 allows spaces in
// aud values, so a space-joined form would be ambiguous for consumers
// splitting on the delimiter. Deliberately locked down here so a future
// change to strings.Join(..., " ") is caught before shipping.
func TestJWTValidation_OnRequest_MultiAudience_CommaJoined(t *testing.T) {
	claims := &validation.Claims{
		Subject:  "alice",
		Issuer:   "http://issuer.example",
		Audience: []string{"aud-a", "aud-b"},
		ClientID: "caller",
		Scopes:   []string{"openid", "write"},
	}
	inner := auth.New(auth.Config{
		Verifier: &mockJWTVerifier{claims: claims},
		Identity: auth.IdentityConfig{Audiences: []string{"aud-a"}},
	})
	p := newTestJWTValidation(t, "http://issuer.example", inner)

	pctx := &pipeline.Context{Headers: http.Header{}, Path: "/api/call"}
	pctx.Headers.Set("Authorization", "Bearer tok")
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v (violation=%+v)", action.Type, action.Violation)
	}
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Inbound) != 1 {
		t.Fatalf("expected one entry, got %+v", pctx.Extensions.Invocations)
	}
	got := pctx.Extensions.Invocations.Inbound[0]
	if got.Details["token_audience"] != "aud-a,aud-b" {
		t.Errorf("token_audience = %q, want %q (comma-joined per RFC 7519 spaces-allowed-in-aud)",
			got.Details["token_audience"], "aud-a,aud-b")
	}
	// Also sanity-check scopes still use space — the two conventions
	// must coexist in the same record.
	if got.Details["token_scopes"] != "openid write" {
		t.Errorf("token_scopes = %q, want %q", got.Details["token_scopes"], "openid write")
	}
}

func TestJWTValidation_Configure_AllowedAudiencesOnly(t *testing.T) {
	p := NewJWTValidation()
	raw := []byte(`{"issuer":"http://kc/realms/x","allowed_audiences":["playground","account"]}`)
	if err := p.Configure(raw); err != nil {
		t.Fatal(err)
	}
	if p.cfg.AudienceFile != "" {
		t.Errorf("AudienceFile = %q, want empty when allowed_audiences supplies static audiences", p.cfg.AudienceFile)
	}
	got := p.inner.InboundAudiences()
	want := []string{"playground", "account"}
	if !slices.Equal(got, want) {
		t.Errorf("InboundAudiences() = %#v, want %#v", got, want)
	}
}

func TestJWTValidation_Configure_AllowedAudiencesUnionsLiteral(t *testing.T) {
	p := NewJWTValidation()
	raw := []byte(`{"issuer":"http://kc/realms/x","audience":"spi-like-id","allowed_audiences":["public-ui"]}`)
	if err := p.Configure(raw); err != nil {
		t.Fatal(err)
	}
	got := p.inner.InboundAudiences()
	want := []string{"public-ui", "spi-like-id"}
	if !slices.Equal(got, want) {
		t.Errorf("InboundAudiences() = %#v, want %#v", got, want)
	}
}

// --- Single-user capture (single-player mode) ---

// allowClaimsForCapture builds a Claims that satisfies the inner Auth's
// allow path with a future-dated ExpiresAt comfortably above MinTTL and
// below MaxTTL — so capture happens via the happy-path code branch
// rather than getting elided by the cap/floor policy.
func allowClaimsForCapture(t *testing.T, subject string) *validation.Claims {
	t.Helper()
	return &validation.Claims{
		Subject:   subject,
		Issuer:    "http://issuer.example",
		Audience:  []string{"agent-aud"},
		ClientID:  "caller",
		Scopes:    []string{"openid"},
		ExpiresAt: time.Now().Add(2 * time.Minute),
	}
}

func newCapturePlugin(t *testing.T, claims *validation.Claims) *JWTValidation {
	t.Helper()
	inner := auth.New(auth.Config{
		Verifier: &mockJWTVerifier{claims: claims},
		Identity: auth.IdentityConfig{Audiences: []string{"agent-aud"}},
	})
	return newTestJWTValidation(t, "http://issuer.example", inner)
}

// withSingleUserMode flips the package-level enabled bit for the test
// and restores it on cleanup.
func withSingleUserMode(t *testing.T, enabled bool) {
	t.Helper()
	prev := auth.IsSingleUserModeEnabled()
	auth.SetSingleUserModeEnabled(enabled)
	t.Cleanup(func() { auth.SetSingleUserModeEnabled(prev) })
}

func TestJWTValidation_Capture_Enabled_ValidToken(t *testing.T) {
	withSingleUserMode(t, true)
	t.Cleanup(auth.ResetSingleUserToken)

	p := newCapturePlugin(t, allowClaimsForCapture(t, "alice"))

	pctx := &pipeline.Context{Headers: http.Header{}, Path: "/api/call"}
	pctx.Headers.Set("Authorization", "Bearer tok-1")

	if got := invokeOnRequest(p, pctx).Type; got != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", got)
	}

	tok, sub, ok := auth.GetSingleUserToken(time.Now())
	if !ok {
		t.Fatal("expected cache hit after capture")
	}
	if tok != "tok-1" || sub != "alice" {
		t.Errorf("cache = (%q, %q), want (tok-1, alice)", tok, sub)
	}
}

func TestJWTValidation_Capture_Disabled_NoCacheWrite(t *testing.T) {
	withSingleUserMode(t, false)
	t.Cleanup(auth.ResetSingleUserToken)

	p := newCapturePlugin(t, allowClaimsForCapture(t, "alice"))

	pctx := &pipeline.Context{Headers: http.Header{}, Path: "/api/call"}
	pctx.Headers.Set("Authorization", "Bearer tok-1")

	invokeOnRequest(p, pctx)

	// Re-enable for the read so we can confirm the cache really is
	// empty (rather than just gated on Get).
	auth.SetSingleUserModeEnabled(true)
	if _, _, ok := auth.GetSingleUserToken(time.Now()); ok {
		t.Error("cache should not be populated when single_user_mode is disabled at the top level")
	}
}

func TestJWTValidation_Capture_NearExpiry_Skipped(t *testing.T) {
	withSingleUserMode(t, true)
	t.Cleanup(auth.ResetSingleUserToken)

	// 10s from now < MinTTL (30s); CapSingleUserExpiry returns false
	// and capture is skipped.
	claims := allowClaimsForCapture(t, "alice")
	claims.ExpiresAt = time.Now().Add(10 * time.Second)

	p := newCapturePlugin(t, claims)

	pctx := &pipeline.Context{Headers: http.Header{}, Path: "/api/call"}
	pctx.Headers.Set("Authorization", "Bearer tok-1")

	invokeOnRequest(p, pctx)

	if _, _, ok := auth.GetSingleUserToken(time.Now()); ok {
		t.Error("cache should not be populated when token is near expiry (below MinTTL floor)")
	}
}

func TestJWTValidation_Capture_LongLived_Capped(t *testing.T) {
	withSingleUserMode(t, true)
	t.Cleanup(auth.ResetSingleUserToken)

	// 1h from now > MaxTTL (5m); cached exp should be capped at
	// approximately now+MaxTTL — verify by probing Get with a future "now".
	claims := allowClaimsForCapture(t, "alice")
	claims.ExpiresAt = time.Now().Add(1 * time.Hour)

	p := newCapturePlugin(t, claims)

	pctx := &pipeline.Context{Headers: http.Header{}, Path: "/api/call"}
	pctx.Headers.Set("Authorization", "Bearer tok-1")

	invokeOnRequest(p, pctx)

	// Just past MaxTTL: cache should report miss, proving the exp was
	// capped (otherwise the original 1h exp would still be valid).
	pastMaxTTL := time.Now().Add(6 * time.Minute)
	if _, _, ok := auth.GetSingleUserToken(pastMaxTTL); ok {
		t.Error("cache should expire at MaxTTL even when token's natural exp is later")
	}

	// Sanity: still valid right now.
	if _, _, ok := auth.GetSingleUserToken(time.Now()); !ok {
		t.Error("cache should be populated immediately after capture")
	}
}

func TestJWTValidation_Capture_SubjectChange_EmitsWarn(t *testing.T) {
	withSingleUserMode(t, true)
	t.Cleanup(auth.ResetSingleUserToken)

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	// First capture: alice. No warn.
	p := newCapturePlugin(t, allowClaimsForCapture(t, "alice"))
	pctx := &pipeline.Context{Headers: http.Header{}, Path: "/api/call"}
	pctx.Headers.Set("Authorization", "Bearer tok-alice")
	invokeOnRequest(p, pctx)

	if strings.Contains(logBuf.String(), "subject changed") {
		t.Fatalf("first capture should not warn, got: %s", logBuf.String())
	}

	// Second capture: bob — single-player assumption violated.
	p2 := newCapturePlugin(t, allowClaimsForCapture(t, "bob"))
	pctx2 := &pipeline.Context{Headers: http.Header{}, Path: "/api/call"}
	pctx2.Headers.Set("Authorization", "Bearer tok-bob")
	invokeOnRequest(p2, pctx2)

	out := logBuf.String()
	if !strings.Contains(out, "single-user cached subject changed") {
		t.Errorf("expected subject-change WARN, log was: %s", out)
	}
	if !strings.Contains(out, "previous=alice") || !strings.Contains(out, "current=bob") {
		t.Errorf("expected previous=alice and current=bob in log, got: %s", out)
	}

	// Same subject re-captured: no warn.
	logBuf.Reset()
	p3 := newCapturePlugin(t, allowClaimsForCapture(t, "bob"))
	pctx3 := &pipeline.Context{Headers: http.Header{}, Path: "/api/call"}
	pctx3.Headers.Set("Authorization", "Bearer tok-bob-2")
	invokeOnRequest(p3, pctx3)
	if strings.Contains(logBuf.String(), "subject changed") {
		t.Errorf("same-subject re-capture should not warn, got: %s", logBuf.String())
	}
}

func TestJWTValidation_Capture_BypassPath_NoCacheWrite(t *testing.T) {
	withSingleUserMode(t, true)
	t.Cleanup(auth.ResetSingleUserToken)

	matcher, _ := bypass.NewMatcher(bypass.DefaultPatterns)
	inner := auth.New(auth.Config{
		Bypass:   matcher,
		Verifier: &mockJWTVerifier{claims: allowClaimsForCapture(t, "alice")},
		Identity: auth.IdentityConfig{Audiences: []string{"agent-aud"}},
	})
	p := newTestJWTValidation(t, "http://issuer.example", inner)

	// /healthz is in DefaultPatterns; bypass returns Continue with nil
	// Claims, the capture branch is gated on the success path which
	// runs only after pctx.Identity is set.
	pctx := &pipeline.Context{Headers: http.Header{}, Path: "/healthz"}
	pctx.Headers.Set("Authorization", "Bearer tok-1")
	invokeOnRequest(p, pctx)

	if _, _, ok := auth.GetSingleUserToken(time.Now()); ok {
		t.Error("bypass path should not populate single-user cache")
	}
}
