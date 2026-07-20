package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/rossocortex/authbridge/authlib/bypass"
	"github.com/rossoctl/rossocortex/authbridge/authlib/plugins/jwtvalidation/validation"
	"github.com/rossoctl/rossocortex/authbridge/authlib/plugins/tokenexchange/cache"
	"github.com/rossoctl/rossocortex/authbridge/authlib/plugins/tokenexchange/exchange"
	"github.com/rossoctl/rossocortex/authbridge/authlib/routing"
)

// mockVerifier captures the audiences arg and returns configured claims/error.
type mockVerifier struct {
	claims        *validation.Claims
	err           error
	lastAudiences []string
}

func (m *mockVerifier) Verify(_ context.Context, _ string, audiences []string) (*validation.Claims, error) {
	m.lastAudiences = audiences
	return m.claims, m.err
}

func validClaims() *validation.Claims {
	return &validation.Claims{
		Subject:  "user-123",
		Issuer:   "http://keycloak/realms/test",
		Audience: []string{"my-agent"},
		ClientID: "caller-app",
		Scopes:   []string{"openid"},
	}
}

// --- Inbound Tests ---

func TestHandleInbound_BypassPath(t *testing.T) {
	m, _ := bypass.NewMatcher(bypass.DefaultPatterns)
	a := New(Config{Bypass: m, Verifier: &mockVerifier{claims: validClaims()}})
	result := a.HandleInbound(context.Background(), "", "/healthz", "")
	if result.Action != ActionAllow {
		t.Errorf("expected allow for bypass path, got %s", result.Action)
	}
}

func TestHandleInbound_MissingAuth(t *testing.T) {
	a := New(Config{Verifier: &mockVerifier{}})
	result := a.HandleInbound(context.Background(), "", "/api/test", "")
	if result.Action != ActionDeny || result.DenyStatus != http.StatusUnauthorized {
		t.Errorf("expected deny/401, got %s/%d", result.Action, result.DenyStatus)
	}
}

func TestHandleInbound_InvalidFormat(t *testing.T) {
	a := New(Config{Verifier: &mockVerifier{}})
	result := a.HandleInbound(context.Background(), "Basic abc123", "/api/test", "")
	if result.Action != ActionDeny {
		t.Errorf("expected deny for non-Bearer auth, got %s", result.Action)
	}
}

func TestHandleInbound_CaseInsensitiveBearer(t *testing.T) {
	a := New(Config{
		Verifier: &mockVerifier{claims: validClaims()},
		Identity: IdentityConfig{Audiences: []string{"my-agent"}},
	})
	// RFC 7235: auth scheme is case-insensitive
	for _, header := range []string{"Bearer token", "bearer token", "BEARER token", "beArer token"} {
		result := a.HandleInbound(context.Background(), header, "/api/test", "")
		if result.Action != ActionAllow {
			t.Errorf("expected allow for %q, got %s: %s", header, result.Action, result.DenyReason)
		}
	}
}

func TestHandleInbound_ValidJWT(t *testing.T) {
	a := New(Config{
		Verifier: &mockVerifier{claims: validClaims()},
		Identity: IdentityConfig{Audiences: []string{"my-agent"}},
	})
	result := a.HandleInbound(context.Background(), "Bearer valid-token", "/api/test", "")
	if result.Action != ActionAllow {
		t.Errorf("expected allow, got %s: %s", result.Action, result.DenyReason)
	}
	if result.Claims == nil || result.Claims.Subject != "user-123" {
		t.Error("expected claims with subject user-123")
	}
}

func TestHandleInbound_InvalidJWT(t *testing.T) {
	a := New(Config{
		Verifier: &mockVerifier{err: fmt.Errorf("token expired")},
		Identity: IdentityConfig{Audiences: []string{"my-agent"}},
	})
	result := a.HandleInbound(context.Background(), "Bearer expired-token", "/api/test", "")
	if result.Action != ActionDeny || result.DenyStatus != http.StatusUnauthorized {
		t.Errorf("expected deny/401, got %s/%d", result.Action, result.DenyStatus)
	}
}

func TestHandleInbound_NoVerifier_Denies(t *testing.T) {
	a := New(Config{}) // no verifier = fail-closed
	result := a.HandleInbound(context.Background(), "Bearer some-token", "/api/test", "")
	if result.Action != ActionDeny {
		t.Errorf("expected deny when verifier not configured, got %s", result.Action)
	}
}

// DenyReasonCode must be populated on every ActionDeny InboundResult. The
// listener/plugin layer uses the code (not the free-form DenyReason string)
// to populate SessionEvent.Auth.Inbound[].Reason for machine-stable
// filtering. A drift here would silently leave the event field empty on
// denied requests.
func TestHandleInbound_PopulatesDenyReasonCode(t *testing.T) {
	cases := []struct {
		name       string
		cfg        Config
		authHeader string
		audience   string
		want       InboundDenialReason
	}{
		{
			name:       "no header",
			cfg:        Config{Verifier: &mockVerifier{}},
			authHeader: "",
			want:       DENY_NO_HEADER,
		},
		{
			name:       "malformed header",
			cfg:        Config{Verifier: &mockVerifier{}},
			authHeader: "Basic abc",
			want:       DENY_MALFORMED_HEADER,
		},
		{
			name:       "validator missing",
			cfg:        Config{}, // nil Verifier
			authHeader: "Bearer tok",
			want:       DENY_VALIDATOR_MISSING,
		},
		{
			name: "jwt failed",
			cfg: Config{
				Verifier: &mockVerifier{err: fmt.Errorf("bad signature")},
				Identity: IdentityConfig{Audiences: []string{"aud"}},
			},
			authHeader: "Bearer tok",
			want:       DENY_JWT_FAILED,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := New(tc.cfg)
			res := a.HandleInbound(context.Background(), tc.authHeader, "/api", tc.audience)
			if res.Action != ActionDeny {
				t.Fatalf("expected deny, got %q", res.Action)
			}
			if res.DenyReasonCode != tc.want {
				t.Errorf("DenyReasonCode = %v, want %v", res.DenyReasonCode, tc.want)
			}
			if res.DenyReason == "" {
				t.Error("DenyReason (human string) should still be populated alongside code")
			}
		})
	}
}

func TestHandleInbound_AudienceOverride(t *testing.T) {
	mv := &mockVerifier{claims: validClaims()}
	a := New(Config{
		Verifier: mv,
		Identity: IdentityConfig{Audiences: []string{"default-aud"}},
	})

	// Empty audience uses configured defaults
	a.HandleInbound(context.Background(), "Bearer t", "/api", "")
	if len(mv.lastAudiences) != 1 || mv.lastAudiences[0] != "default-aud" {
		t.Errorf("expected [default-aud], got %v", mv.lastAudiences)
	}

	// Explicit audience overrides default (waypoint mode)
	a.HandleInbound(context.Background(), "Bearer t", "/api", "derived-from-host")
	if len(mv.lastAudiences) != 1 || mv.lastAudiences[0] != "derived-from-host" {
		t.Errorf("expected [derived-from-host], got %v", mv.lastAudiences)
	}
}

// --- Outbound Tests ---

func newTestExchangeServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "exchanged-" + r.FormValue("audience"),
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	}))
}

func TestHandleOutbound_NoRouter(t *testing.T) {
	a := New(Config{})
	result := a.HandleOutbound(context.Background(), "Bearer token", "some-host")
	if result.Action != ActionAllow {
		t.Errorf("expected allow with no router, got %s", result.Action)
	}
}

func TestHandleOutbound_PassthroughRoute(t *testing.T) {
	router, _ := routing.NewRouter("passthrough", []routing.Route{
		{Host: "internal-svc", Action: "passthrough"},
	})
	a := New(Config{Router: router})
	result := a.HandleOutbound(context.Background(), "Bearer token", "internal-svc")
	if result.Action != ActionAllow {
		t.Errorf("expected allow for passthrough route, got %s", result.Action)
	}
}

func TestHandleOutbound_NoMatch_Passthrough(t *testing.T) {
	router, _ := routing.NewRouter("passthrough", []routing.Route{
		{Host: "known-svc", Audience: "known"},
	})
	a := New(Config{Router: router})
	result := a.HandleOutbound(context.Background(), "Bearer token", "unknown-svc")
	if result.Action != ActionAllow {
		t.Errorf("expected allow for unmatched host with passthrough default, got %s", result.Action)
	}
}

func TestHandleOutbound_Exchange(t *testing.T) {
	srv := newTestExchangeServer(t)
	defer srv.Close()

	router, _ := routing.NewRouter("passthrough", []routing.Route{
		{Host: "target-svc", Audience: "target-aud", Scopes: "openid"},
	})
	exchanger := exchange.NewClient(srv.URL, &exchange.ClientSecretAuth{
		ClientID: "agent", ClientSecret: "secret",
	})
	a := New(Config{
		Router:    router,
		Exchanger: exchanger,
		Cache:     cache.New(),
	})

	result := a.HandleOutbound(context.Background(), "Bearer user-token", "target-svc")
	if result.Action != ActionReplaceToken {
		t.Fatalf("expected replace_token, got %s: %s", result.Action, result.DenyReason)
	}
	if result.Token != "exchanged-target-aud" {
		t.Errorf("token = %q, want %q", result.Token, "exchanged-target-aud")
	}
}

func TestHandleOutbound_CacheHit(t *testing.T) {
	router, _ := routing.NewRouter("passthrough", []routing.Route{
		{Host: "target-svc", Audience: "target-aud"},
	})
	c := cache.New()
	c.Set("user-token", "target-aud", "cached-token", 5*time.Minute)

	a := New(Config{Router: router, Cache: c})

	result := a.HandleOutbound(context.Background(), "Bearer user-token", "target-svc")
	if result.Action != ActionReplaceToken || result.Token != "cached-token" {
		t.Errorf("expected cached token, got action=%s token=%q", result.Action, result.Token)
	}
}

func TestHandleOutbound_NoToken_ClientCredentials(t *testing.T) {
	srv := newTestExchangeServer(t)
	defer srv.Close()

	router, _ := routing.NewRouter("exchange", []routing.Route{})
	exchanger := exchange.NewClient(srv.URL, &exchange.ClientSecretAuth{
		ClientID: "agent", ClientSecret: "secret",
	})
	a := New(Config{
		Router:        router,
		Exchanger:     exchanger,
		NoTokenPolicy: NoTokenPolicyClientCredentials,
	})

	result := a.HandleOutbound(context.Background(), "", "any-svc")
	if result.Action != ActionReplaceToken {
		t.Fatalf("expected replace_token from client_credentials, got %s: %s", result.Action, result.DenyReason)
	}
}

func TestHandleOutbound_NoToken_Allow(t *testing.T) {
	router, _ := routing.NewRouter("exchange", []routing.Route{})
	a := New(Config{
		Router:        router,
		NoTokenPolicy: NoTokenPolicyAllow,
	})

	result := a.HandleOutbound(context.Background(), "", "any-svc")
	if result.Action != ActionAllow {
		t.Errorf("expected allow for no-token allow policy, got %s", result.Action)
	}
}

func TestHandleOutbound_NoToken_Deny(t *testing.T) {
	router, _ := routing.NewRouter("exchange", []routing.Route{})
	a := New(Config{
		Router:        router,
		NoTokenPolicy: NoTokenPolicyDeny,
	})

	result := a.HandleOutbound(context.Background(), "", "any-svc")
	if result.Action != ActionDeny {
		t.Errorf("expected deny for no-token deny policy, got %s", result.Action)
	}
}

func TestHandleOutbound_PerRouteTokenEndpoint(t *testing.T) {
	// Main server should NOT be called
	mainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("main token URL should not be called when route overrides it")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mainSrv.Close()

	// Per-route server SHOULD be called
	routeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "from-route-endpoint",
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	}))
	defer routeSrv.Close()

	router, _ := routing.NewRouter("passthrough", []routing.Route{
		{Host: "custom-svc", Audience: "custom-aud", TokenEndpoint: routeSrv.URL},
	})
	exchanger := exchange.NewClient(mainSrv.URL, &exchange.ClientSecretAuth{
		ClientID: "agent", ClientSecret: "secret",
	})
	a := New(Config{Router: router, Exchanger: exchanger})

	result := a.HandleOutbound(context.Background(), "Bearer token", "custom-svc")
	if result.Action != ActionReplaceToken || result.Token != "from-route-endpoint" {
		t.Errorf("expected token from route endpoint, got action=%s token=%q", result.Action, result.Token)
	}
}

// --- Stats Tests ---

func TestNewStats(t *testing.T) {
	s := NewStats()
	if s == nil {
		t.Fatal("NewStats() returned nil")
	}
	if len(s.inboundApprovals) != 0 {
		t.Errorf("inboundApprovals = %d entries, want 0", len(s.inboundApprovals))
	}
	if len(s.inboundDenials) != 0 {
		t.Errorf("inboundDenials = %d entries, want 0", len(s.inboundDenials))
	}
	if len(s.outboundApprovals) != 0 {
		t.Errorf("outboundApprovals = %d entries, want 0", len(s.outboundApprovals))
	}
	if len(s.outboundDenials) != 0 {
		t.Errorf("outboundDenials = %d entries, want 0", len(s.outboundDenials))
	}
	if len(s.outboundReplaceTokens) != 0 {
		t.Errorf("outboundReplaceTokens = %d entries, want 0", len(s.outboundReplaceTokens))
	}
}

func TestIncInboundApprove(t *testing.T) {
	a := New(Config{})

	a.IncInboundApprove(APPROVE_PASSTHROUGH)
	a.IncInboundApprove(APPROVE_PASSTHROUGH)
	a.IncInboundApprove(APPROVE_AUTHORIZED)

	if got := a.Stats.inboundApprovals[APPROVE_PASSTHROUGH]; got != 2 {
		t.Errorf("APPROVE_PASSTHROUGH = %d, want 2", got)
	}
	if got := a.Stats.inboundApprovals[APPROVE_AUTHORIZED]; got != 1 {
		t.Errorf("APPROVE_AUTHORIZED = %d, want 1", got)
	}
}

func TestIncInboundDeny(t *testing.T) {
	a := New(Config{})

	a.IncInboundDeny(DENY_NO_HEADER)
	a.IncInboundDeny(DENY_MALFORMED_HEADER)
	a.IncInboundDeny(DENY_VALIDATOR_MISSING)
	a.IncInboundDeny(DENY_JWT_FAILED)
	a.IncInboundDeny(DENY_JWT_FAILED)

	expected := map[InboundDenialReason]int{
		DENY_NO_HEADER:         1,
		DENY_MALFORMED_HEADER:  1,
		DENY_VALIDATOR_MISSING: 1,
		DENY_JWT_FAILED:        2,
	}
	for reason, want := range expected {
		if got := a.Stats.inboundDenials[reason]; got != want {
			t.Errorf("%s = %d, want %d", reason, got, want)
		}
	}
}

func TestIncOutboundApprove(t *testing.T) {
	a := New(Config{})

	a.IncOutboundApprove(OUTBOUND_NO_MATCHING_ROUTE)
	a.IncOutboundApprove(OUTBOUND_PASSTHROUGH)
	a.IncOutboundApprove(OUTBOUND_PASSTHROUGH)
	a.IncOutboundApprove(OUTBOUND_NO_TOKEN_POLICY)

	if got := a.Stats.outboundApprovals[OUTBOUND_NO_MATCHING_ROUTE]; got != 1 {
		t.Errorf("OUTBOUND_NO_MATCHING_ROUTE = %d, want 1", got)
	}
	if got := a.Stats.outboundApprovals[OUTBOUND_PASSTHROUGH]; got != 2 {
		t.Errorf("OUTBOUND_PASSTHROUGH = %d, want 2", got)
	}
	if got := a.Stats.outboundApprovals[OUTBOUND_NO_TOKEN_POLICY]; got != 1 {
		t.Errorf("OUTBOUND_NO_TOKEN_POLICY = %d, want 1", got)
	}
}

func TestIncOutboundDeny(t *testing.T) {
	a := New(Config{})

	a.IncOutboundDeny(OUTBOUND_CREDS_REQUESTED_NO_EXCHANGER)
	a.IncOutboundDeny(OUTBOUND_CREDENTIALS_GRANT_FAILURE)
	a.IncOutboundDeny(OUTBOUND_CREDENTIALS_GRANT_FAILURE)
	a.IncOutboundDeny(OUTBOUND_NO_TOKEN)
	a.IncOutboundDeny(OUTBOUND_TOKEN_EXCHANGE_FAILED)

	expected := map[OutboundDenialReason]int{
		OUTBOUND_CREDS_REQUESTED_NO_EXCHANGER: 1,
		OUTBOUND_CREDENTIALS_GRANT_FAILURE:    2,
		OUTBOUND_NO_TOKEN:                     1,
		OUTBOUND_TOKEN_EXCHANGE_FAILED:        1,
	}
	for reason, want := range expected {
		if got := a.Stats.outboundDenials[reason]; got != want {
			t.Errorf("%s = %d, want %d", reason, got, want)
		}
	}
}

func TestIncOutboundReplaceToken(t *testing.T) {
	a := New(Config{})

	a.IncOutboundReplaceToken(OUTBOUND_ACTION_REPLACE_TOKEN)
	a.IncOutboundReplaceToken(OUTBOUND_ACTION_REPLACE_TOKEN)
	a.IncOutboundReplaceToken(OUTBOUND_ACTION_CACHE_HIT)

	if got := a.Stats.outboundReplaceTokens[OUTBOUND_ACTION_REPLACE_TOKEN]; got != 2 {
		t.Errorf("OUTBOUND_ACTION_REPLACE_TOKEN = %d, want 2", got)
	}
	if got := a.Stats.outboundReplaceTokens[OUTBOUND_ACTION_CACHE_HIT]; got != 1 {
		t.Errorf("OUTBOUND_ACTION_CACHE_HIT = %d, want 1", got)
	}
}

// MergeStats sums counters from multiple Stats objects into a fresh
// one. Each source's mutex is taken independently — no deadlock even
// if sources are being mutated concurrently.
func TestMergeStats_SumsCounters(t *testing.T) {
	a := New(Config{})
	a.IncInboundApprove(APPROVE_AUTHORIZED)
	a.IncInboundApprove(APPROVE_AUTHORIZED)
	a.IncInboundDeny(DENY_NO_HEADER)

	b := New(Config{})
	b.IncInboundApprove(APPROVE_AUTHORIZED) // +1, total 3
	b.IncInboundApprove(APPROVE_PASSTHROUGH)
	b.IncOutboundApprove(OUTBOUND_PASSTHROUGH)

	merged := MergeStats(a.Stats, b.Stats)
	if got, want := merged.inboundApprovals[APPROVE_AUTHORIZED], 3; got != want {
		t.Errorf("APPROVE_AUTHORIZED = %d, want %d", got, want)
	}
	if got, want := merged.inboundApprovals[APPROVE_PASSTHROUGH], 1; got != want {
		t.Errorf("APPROVE_PASSTHROUGH = %d, want %d", got, want)
	}
	if got, want := merged.inboundDenials[DENY_NO_HEADER], 1; got != want {
		t.Errorf("DENY_NO_HEADER = %d, want %d", got, want)
	}
	if got, want := merged.outboundApprovals[OUTBOUND_PASSTHROUGH], 1; got != want {
		t.Errorf("OUTBOUND_PASSTHROUGH = %d, want %d", got, want)
	}

	// Merging must not mutate source counters — the aggregator runs
	// on every /stats probe, and we can't have each probe double-
	// counting.
	if got, want := a.Stats.inboundApprovals[APPROVE_AUTHORIZED], 2; got != want {
		t.Errorf("source a mutated: APPROVE_AUTHORIZED = %d, want %d", got, want)
	}
}

func TestMergeStats_TolerantOfNilSources(t *testing.T) {
	merged := MergeStats(nil, nil)
	if merged == nil {
		t.Fatal("MergeStats(nil, nil) returned nil")
	}
}

func TestMergeStats_Empty(t *testing.T) {
	merged := MergeStats()
	if merged == nil {
		t.Fatal("MergeStats() returned nil")
	}
}

func TestStatsMarshalJSON_Empty(t *testing.T) {
	s := NewStats()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}

	var got map[string]map[string]int
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	for _, key := range []string{"inbound_approvals", "inbound_denials", "outbound_approvals", "outbound_denials"} {
		m, ok := got[key]
		if !ok {
			t.Errorf("missing key %q", key)
			continue
		}
		if len(m) != 0 {
			t.Errorf("%s = %v, want empty map", key, m)
		}
	}
}

func TestStatsMarshalJSON_WithCounts(t *testing.T) {
	a := New(Config{})

	a.IncInboundApprove(APPROVE_AUTHORIZED)
	a.IncInboundApprove(APPROVE_AUTHORIZED)
	a.IncInboundApprove(APPROVE_PASSTHROUGH)
	a.IncInboundDeny(DENY_NO_HEADER)
	a.IncInboundDeny(DENY_JWT_FAILED)
	a.IncInboundDeny(DENY_JWT_FAILED)
	a.IncOutboundApprove(OUTBOUND_PASSTHROUGH)
	a.IncOutboundDeny(OUTBOUND_TOKEN_EXCHANGE_FAILED)

	data, err := json.Marshal(a.Stats)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}

	var got map[string]map[string]int
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	checks := []struct {
		section string
		key     string
		want    int
	}{
		{"inbound_approvals", "authorized", 2},
		{"inbound_approvals", "passthrough", 1},
		{"inbound_denials", "no_header", 1},
		{"inbound_denials", "jwt_failed", 2},
		{"outbound_approvals", "passthrough", 1},
		{"outbound_denials", "token_exchange_failed", 1},
	}
	for _, tc := range checks {
		if got[tc.section][tc.key] != tc.want {
			t.Errorf("%s.%s = %d, want %d", tc.section, tc.key, got[tc.section][tc.key], tc.want)
		}
	}
}

func TestStatsMarshalJSON_UsesStringKeys(t *testing.T) {
	a := New(Config{})
	a.IncInboundApprove(APPROVE_PASSTHROUGH)
	a.IncInboundDeny(DENY_MALFORMED_HEADER)
	a.IncOutboundApprove(OUTBOUND_NO_MATCHING_ROUTE)
	a.IncOutboundDeny(OUTBOUND_CREDS_REQUESTED_NO_EXCHANGER)

	data, err := json.Marshal(a.Stats)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}

	raw := string(data)
	expectedKeys := []string{
		`"passthrough"`, `"malformed_header"`, `"no_matching_route"`, `"creds_requested_no_exchanger"`,
	}
	for _, key := range expectedKeys {
		if !strings.Contains(raw, key) {
			t.Errorf("JSON missing key %s: %s", key, raw)
		}
	}
}

func TestStatsConcurrentAccess(t *testing.T) {
	a := New(Config{})
	done := make(chan struct{})

	for i := range 125 {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			switch n % 5 {
			case 0:
				a.IncInboundApprove(APPROVE_AUTHORIZED)
			case 1:
				a.IncInboundDeny(DENY_JWT_FAILED)
			case 2:
				a.IncOutboundApprove(OUTBOUND_PASSTHROUGH)
			case 3:
				a.IncOutboundDeny(OUTBOUND_TOKEN_EXCHANGE_FAILED)
			case 4:
				a.IncOutboundReplaceToken(OUTBOUND_ACTION_REPLACE_TOKEN)
			}
		}(i)
	}
	for range 125 {
		<-done
	}

	// (This function has the only copy of the stats so we don't need to hold
	// the mutex to read these counters.)
	if got := a.Stats.inboundApprovals[APPROVE_AUTHORIZED]; got != 25 {
		t.Errorf("concurrent inbound approvals = %d, want 25", got)
	}
	if got := a.Stats.inboundDenials[DENY_JWT_FAILED]; got != 25 {
		t.Errorf("concurrent inbound denials = %d, want 25", got)
	}
	if got := a.Stats.outboundApprovals[OUTBOUND_PASSTHROUGH]; got != 25 {
		t.Errorf("concurrent outbound approvals = %d, want 25", got)
	}
	if got := a.Stats.outboundDenials[OUTBOUND_TOKEN_EXCHANGE_FAILED]; got != 25 {
		t.Errorf("concurrent outbound denials = %d, want 25", got)
	}
	if got := a.Stats.outboundReplaceTokens[OUTBOUND_ACTION_REPLACE_TOKEN]; got != 25 {
		t.Errorf("concurrent outbound replace tokens = %d, want 25", got)
	}
}

func TestStatsConcurrentMarshal(t *testing.T) {
	a := New(Config{})
	errs := make(chan error, 50)
	done := make(chan struct{})

	for range 50 {
		go func() {
			defer func() { done <- struct{}{} }()
			a.IncInboundApprove(APPROVE_AUTHORIZED)
		}()
		go func() {
			defer func() { done <- struct{}{} }()
			if _, err := json.Marshal(a.Stats); err != nil {
				errs <- err
			}
		}()
	}
	for range 100 {
		<-done
	}
	close(errs)
	for err := range errs {
		t.Fatal("concurrent MarshalJSON:", err)
	}
}

// --- Reason Stringer Tests ---

func TestInboundDenialReasonString(t *testing.T) {
	tests := []struct {
		reason InboundDenialReason
		want   string
	}{
		{DENY_NO_HEADER, "no_header"},
		{DENY_MALFORMED_HEADER, "malformed_header"},
		{DENY_VALIDATOR_MISSING, "validator_missing"},
		{DENY_JWT_FAILED, "jwt_failed"},
		{InboundDenialReason(99), "unknown"},
	}
	for _, tc := range tests {
		if got := tc.reason.String(); got != tc.want {
			t.Errorf("InboundDenialReason(%d).String() = %q, want %q", tc.reason, got, tc.want)
		}
	}
}

func TestInboundApprovalReasonString(t *testing.T) {
	tests := []struct {
		reason InboundApprovalReason
		want   string
	}{
		{APPROVE_PASSTHROUGH, "passthrough"},
		{APPROVE_AUTHORIZED, "authorized"},
		{InboundApprovalReason(99), "unknown"},
	}
	for _, tc := range tests {
		if got := tc.reason.String(); got != tc.want {
			t.Errorf("InboundApprovalReason(%d).String() = %q, want %q", tc.reason, got, tc.want)
		}
	}
}

func TestOutboundApprovalReasonString(t *testing.T) {
	tests := []struct {
		reason OutboundApprovalReason
		want   string
	}{
		{OUTBOUND_NO_MATCHING_ROUTE, "no_matching_route"},
		{OUTBOUND_PASSTHROUGH, "passthrough"},
		{OUTBOUND_NO_TOKEN_POLICY, "no_token_policy"},
		{OUTBOUND_NO_EXCHANGER, "no_exchanger"},
		{OutboundApprovalReason(99), "unknown"},
	}
	for _, tc := range tests {
		if got := tc.reason.String(); got != tc.want {
			t.Errorf("OutboundApprovalReason(%d).String() = %q, want %q", tc.reason, got, tc.want)
		}
	}
}

func TestOutboundDenialReasonString(t *testing.T) {
	tests := []struct {
		reason OutboundDenialReason
		want   string
	}{
		{OUTBOUND_CREDS_REQUESTED_NO_EXCHANGER, "creds_requested_no_exchanger"},
		{OUTBOUND_CREDENTIALS_GRANT_FAILURE, "credentials_grant_failure"},
		{OUTBOUND_NO_TOKEN, "no_token"},
		{OUTBOUND_TOKEN_EXCHANGE_FAILED, "token_exchange_failed"},
		{OutboundDenialReason(99), "unknown"},
	}
	for _, tc := range tests {
		if got := tc.reason.String(); got != tc.want {
			t.Errorf("OutboundDenialReason(%d).String() = %q, want %q", tc.reason, got, tc.want)
		}
	}
}

func TestOutboundReplaceTokenReasonString(t *testing.T) {
	tests := []struct {
		reason OutboundReplaceTokenReason
		want   string
	}{
		{OUTBOUND_ACTION_REPLACE_TOKEN, "replace_token"},
		{OUTBOUND_ACTION_CACHE_HIT, "cache_hit"},
		{OutboundReplaceTokenReason(99), "unknown"},
	}
	for _, tc := range tests {
		if got := tc.reason.String(); got != tc.want {
			t.Errorf("OutboundReplaceTokenReason(%d).String() = %q, want %q", tc.reason, got, tc.want)
		}
	}
}

// --- Integration: HandleInbound/HandleOutbound increment stats ---

func TestHandleInbound_IncrementsStats(t *testing.T) {
	m, _ := bypass.NewMatcher(bypass.DefaultPatterns)
	a := New(Config{
		Bypass:   m,
		Verifier: &mockVerifier{claims: validClaims()},
		Identity: IdentityConfig{Audiences: []string{"my-agent"}},
	})

	a.HandleInbound(context.Background(), "", "/healthz", "")
	a.HandleInbound(context.Background(), "", "/api/test", "")
	a.HandleInbound(context.Background(), "Basic bad", "/api/test", "")
	a.HandleInbound(context.Background(), "Bearer valid", "/api/test", "")

	if got := a.Stats.inboundApprovals[APPROVE_PASSTHROUGH]; got != 1 {
		t.Errorf("bypass approval = %d, want 1", got)
	}
	if got := a.Stats.inboundDenials[DENY_NO_HEADER]; got != 1 {
		t.Errorf("no_header denial = %d, want 1", got)
	}
	if got := a.Stats.inboundDenials[DENY_MALFORMED_HEADER]; got != 1 {
		t.Errorf("malformed_header denial = %d, want 1", got)
	}
	if got := a.Stats.inboundApprovals[APPROVE_AUTHORIZED]; got != 1 {
		t.Errorf("authorized approval = %d, want 1", got)
	}
}

func TestHandleInbound_NoVerifier_IncrementsStats(t *testing.T) {
	a := New(Config{})
	a.HandleInbound(context.Background(), "Bearer token", "/api/test", "")

	if got := a.Stats.inboundDenials[DENY_VALIDATOR_MISSING]; got != 1 {
		t.Errorf("validator_missing denial = %d, want 1", got)
	}
}

func TestHandleInbound_JWTFailed_IncrementsStats(t *testing.T) {
	a := New(Config{
		Verifier: &mockVerifier{err: fmt.Errorf("bad token")},
		Identity: IdentityConfig{Audiences: []string{"aud"}},
	})
	a.HandleInbound(context.Background(), "Bearer bad", "/api", "")

	if got := a.Stats.inboundDenials[DENY_JWT_FAILED]; got != 1 {
		t.Errorf("jwt_failed denial = %d, want 1", got)
	}
}
