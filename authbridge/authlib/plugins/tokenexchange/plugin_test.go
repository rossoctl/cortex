package tokenexchange

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

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

// --- Single-player mode (cache-fallback for missing inbound header) ---

// newSPExchangeServer returns an httptest token endpoint that responds
// with "exchanged-{subject_token}" as the access token, so the test can
// assert that the right subject_token reached the IdP.
func newSPExchangeServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		subj := r.PostFormValue("subject_token")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "exchanged-" + subj,
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	}))
}

// configureSPPlugin configures a plugin against a stand-up exchange
// server with a single route mapping any host to a target audience.
// The single-player feature is now controlled top-level via
// auth.SetSingleUserModeEnabled, not via plugin config.
func configureSPPlugin(t *testing.T, exchangeURL string) *TokenExchange {
	t.Helper()
	p := NewTokenExchange()
	cfg := map[string]any{
		"token_url":      exchangeURL,
		"default_policy": "exchange",
		"identity": map[string]any{
			"type":          "client-secret",
			"client_id":     "agent",
			"client_secret": "secret",
		},
		"routes": map[string]any{
			"rules": []map[string]any{
				{"host": "target-svc", "target_audience": "downstream-aud"},
			},
		},
	}
	raw, _ := json.Marshal(cfg)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return p
}

// withSingleUserMode flips the package-level enabled bit for the test
// and restores it on cleanup.
func withSingleUserMode(t *testing.T, enabled bool) {
	t.Helper()
	prev := auth.IsSingleUserModeEnabled()
	auth.SetSingleUserModeEnabled(enabled)
	t.Cleanup(func() { auth.SetSingleUserModeEnabled(prev) })
}

func TestTokenExchange_SingleUserMode_CacheHit_InjectsToken(t *testing.T) {
	withSingleUserMode(t, true)
	t.Cleanup(auth.ResetSingleUserToken)
	srv := newSPExchangeServer(t)
	defer srv.Close()

	auth.StoreSingleUserToken("inbound-tok", "alice", time.Now().Add(2*time.Minute))

	p := configureSPPlugin(t, srv.URL)

	// Outbound request with NO Authorization header.
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{},
	}
	if got := invokeOnRequest(p, pctx).Type; got != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", got)
	}

	// Authorization should be the exchanged token derived from the
	// cached subject_token.
	got := pctx.Headers.Get("Authorization")
	want := "Bearer exchanged-inbound-tok"
	if got != want {
		t.Errorf("Authorization = %q, want %q (proves cached token used as subject_token)", got, want)
	}

	// Two outbound entries: cache_hit modify, plus the actual exchange's
	// token_replaced modify.
	if pctx.Extensions.Invocations == nil {
		t.Fatal("expected invocations recorded")
	}
	var cacheHit, replaced bool
	for _, inv := range pctx.Extensions.Invocations.Outbound {
		if inv.Reason == "single_user_cache_hit" {
			cacheHit = true
		}
		if inv.Reason == "token_replaced" {
			replaced = true
		}
	}
	if !cacheHit {
		t.Error("expected single_user_cache_hit invocation")
	}
	if !replaced {
		t.Error("expected token_replaced invocation (exchange should have run on the cached token)")
	}
}

func TestTokenExchange_SingleUserMode_CacheMiss_NoTokenPolicy(t *testing.T) {
	withSingleUserMode(t, true)
	t.Cleanup(auth.ResetSingleUserToken)
	srv := newSPExchangeServer(t)
	defer srv.Close()

	// Cache empty — verify reset is in effect.
	if _, _, ok := auth.GetSingleUserToken(time.Now()); ok {
		t.Fatal("test prelude: cache should be empty after reset")
	}

	p := configureSPPlugin(t, srv.URL)

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{},
	}
	action := invokeOnRequest(p, pctx)

	// Default no_token_policy is "deny".
	if action.Type != pipeline.Reject {
		t.Errorf("got %v, want Reject (cache miss should fall through to no_token_policy=deny)", action.Type)
	}
	if pctx.Headers.Get("Authorization") != "" {
		t.Error("Authorization should remain empty on cache miss")
	}
}

func TestTokenExchange_SingleUserMode_ExplicitHeaderWins(t *testing.T) {
	withSingleUserMode(t, true)
	t.Cleanup(auth.ResetSingleUserToken)
	srv := newSPExchangeServer(t)
	defer srv.Close()

	auth.StoreSingleUserToken("inbound-tok", "alice", time.Now().Add(2*time.Minute))

	p := configureSPPlugin(t, srv.URL)

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{"Authorization": []string{"Bearer explicit-tok"}},
	}
	if got := invokeOnRequest(p, pctx).Type; got != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", got)
	}

	// Explicit header should be used as subject_token, not the cached one.
	got := pctx.Headers.Get("Authorization")
	want := "Bearer exchanged-explicit-tok"
	if got != want {
		t.Errorf("Authorization = %q, want %q (explicit header must win over cache)", got, want)
	}

	// No cache_hit invocation should be recorded — the cache lookup
	// branch is gated on authHeader == "".
	if pctx.Extensions.Invocations != nil {
		for _, inv := range pctx.Extensions.Invocations.Outbound {
			if inv.Reason == "single_user_cache_hit" {
				t.Error("cache_hit invocation should not fire when explicit header is present")
			}
		}
	}
}

func TestTokenExchange_SingleUserMode_DisabledIgnoresCache(t *testing.T) {
	withSingleUserMode(t, false)
	t.Cleanup(auth.ResetSingleUserToken)
	srv := newSPExchangeServer(t)
	defer srv.Close()

	// Populate the cache while disabled — Store will no-op. Re-enable
	// briefly to populate, then disable for the actual test.
	auth.SetSingleUserModeEnabled(true)
	auth.StoreSingleUserToken("inbound-tok", "alice", time.Now().Add(2*time.Minute))
	auth.SetSingleUserModeEnabled(false)

	p := configureSPPlugin(t, srv.URL)

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{},
	}
	action := invokeOnRequest(p, pctx)

	// With the top-level flag disabled, GetSingleUserToken short-circuits
	// to ok=false, so the cache lookup misses and we fall through to
	// no_token_policy=deny.
	if action.Type != pipeline.Reject {
		t.Errorf("got %v, want Reject (cache should be ignored when single_user_mode is disabled at the top level)", action.Type)
	}
}
