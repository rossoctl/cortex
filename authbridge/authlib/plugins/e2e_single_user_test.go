package plugins_test

// End-to-end test for single-player mode (inbound→outbound token
// bridging). Exercises the realistic plugin stack:
//
//   1. Real RSA keypair + JWKS server (mock IdP)
//   2. Real signed JWT presented on inbound
//   3. Real plugins built via plugins.Build (same path as production)
//   4. Real pipeline.Run dispatch with full Context wiring
//   5. Mock token-exchange endpoint that echoes the subject_token so the
//      assertion can prove the cached inbound token actually reached the
//      IdP as subject_token (closing the loop end-to-end).
//
// Two pipelines are built — inbound and outbound — and a request is
// driven through each. The package-level cache in authlib/auth is the
// only shared state between them, mirroring how the binary uses them
// (jwt-validation in the inbound pipeline, token-exchange in the
// outbound pipeline, cache state spans the request boundary).

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
	_ "github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/jwtvalidation"
	_ "github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/tokenexchange"
)

// --- Test harness ---

type e2eHarness struct {
	jwksSrv     *httptest.Server
	exchangeSrv *httptest.Server
	privKey     *rsa.PrivateKey
	issuer      string

	// receivedSubjectTokens records every subject_token the mock token
	// exchange endpoint observed, in order. Used to prove the cached
	// inbound token reached the IdP.
	receivedSubjectTokens []string
}

// newE2EHarness boots a JWKS server and a token-exchange server. Both
// are torn down via t.Cleanup so the test owns no resources after
// returning.
func newE2EHarness(t *testing.T) *e2eHarness {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pubJWK, err := jwk.FromRaw(priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	_ = pubJWK.Set(jwk.KeyIDKey, "e2e-key-1")
	_ = pubJWK.Set(jwk.AlgorithmKey, jwa.RS256)
	keySet := jwk.NewSet()
	_ = keySet.AddKey(pubJWK)

	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(keySet)
	}))
	t.Cleanup(jwksSrv.Close)

	h := &e2eHarness{
		privKey: priv,
		jwksSrv: jwksSrv,
		issuer:  "http://e2e-issuer.example",
	}

	// Token-exchange endpoint: returns "exchanged-{subject_token}" so
	// the test can verify the cached inbound token actually reached the
	// IdP as subject_token. Records each call for later assertions.
	h.exchangeSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		subj := r.PostFormValue("subject_token")
		h.receivedSubjectTokens = append(h.receivedSubjectTokens, subj)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "exchanged-" + subj,
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	}))
	t.Cleanup(h.exchangeSrv.Close)

	return h
}

// signJWT creates a signed RS256 JWT with reasonable defaults plus any
// caller-supplied claims (e.g., subject, exp).
func (h *e2eHarness) signJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	defaults := map[string]any{
		"iss": h.issuer,
		"aud": []string{"agent-aud"},
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(2 * time.Minute).Unix(),
	}
	for k, v := range claims {
		defaults[k] = v
	}
	builder := jwt.New()
	for k, v := range defaults {
		_ = builder.Set(k, v)
	}
	privJWK, err := jwk.FromRaw(h.privKey)
	if err != nil {
		t.Fatal(err)
	}
	_ = privJWK.Set(jwk.KeyIDKey, "e2e-key-1")
	_ = privJWK.Set(jwk.AlgorithmKey, jwa.RS256)
	signed, err := jwt.Sign(builder, jwt.WithKey(jwa.RS256, privJWK))
	if err != nil {
		t.Fatal(err)
	}
	return string(signed)
}

// buildInboundPipeline returns a Pipeline configured with jwt-validation
// against the harness's JWKS server.
func (h *e2eHarness) buildInboundPipeline(t *testing.T) *pipeline.Pipeline {
	t.Helper()
	jwtCfg := json.RawMessage(fmt.Sprintf(`{
		"issuer": %q,
		"jwks_url": %q,
		"audience": "agent-aud"
	}`, h.issuer, h.jwksSrv.URL))
	p, err := plugins.Build([]config.PluginEntry{
		{Name: "jwt-validation", Config: jwtCfg},
	})
	if err != nil {
		t.Fatalf("inbound Build: %v", err)
	}
	return p
}

// buildOutboundPipeline returns a Pipeline configured with token-exchange
// pointed at the harness's exchange endpoint, with one route mapping the
// host "target-svc" to a fixed audience.
func (h *e2eHarness) buildOutboundPipeline(t *testing.T) *pipeline.Pipeline {
	t.Helper()
	tokCfg := json.RawMessage(fmt.Sprintf(`{
		"token_url": %q,
		"default_policy": "exchange",
		"identity": {"type":"client-secret","client_id":"agent","client_secret":"secret"},
		"routes": {"rules": [{"host":"target-svc","target_audience":"downstream-aud"}]}
	}`, h.exchangeSrv.URL))
	p, err := plugins.Build([]config.PluginEntry{
		{Name: "token-exchange", Config: tokCfg},
	})
	if err != nil {
		t.Fatalf("outbound Build: %v", err)
	}
	return p
}

// runInbound dispatches a Context through the inbound pipeline with the
// given Bearer token. Sets framework attribution like the real listener.
func runInbound(t *testing.T, p *pipeline.Pipeline, bearer string) (*pipeline.Context, pipeline.Action) {
	t.Helper()
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Headers:   http.Header{},
		Path:      "/api/call",
	}
	if bearer != "" {
		pctx.Headers.Set("Authorization", "Bearer "+bearer)
	}
	action := p.Run(context.Background(), pctx)
	return pctx, action
}

// runOutbound dispatches a Context through the outbound pipeline with
// the given (optional) Bearer header.
func runOutbound(t *testing.T, p *pipeline.Pipeline, bearer string) (*pipeline.Context, pipeline.Action) {
	t.Helper()
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{},
	}
	if bearer != "" {
		pctx.Headers.Set("Authorization", "Bearer "+bearer)
	}
	action := p.Run(context.Background(), pctx)
	return pctx, action
}

// withSingleUserMode flips the package-level enabled bit and restores
// the prior state on cleanup.
func withSingleUserMode(t *testing.T, enabled bool) {
	t.Helper()
	prev := auth.IsSingleUserModeEnabled()
	auth.SetSingleUserModeEnabled(enabled)
	t.Cleanup(func() { auth.SetSingleUserModeEnabled(prev) })
}

// --- The actual scenarios ---

// TestE2E_SinglePlayerMode_HappyPath is the headline test: an agent
// receives a user JWT inbound, then makes an outbound call WITHOUT
// propagating the Authorization header (mimicking LangChain's tool
// execution model). With single-player mode on, the cached inbound
// token reaches the IdP as subject_token and the exchanged result
// lands on the outbound request.
func TestE2E_SinglePlayerMode_HappyPath(t *testing.T) {
	withSingleUserMode(t, true)
	t.Cleanup(auth.ResetSingleUserToken)

	h := newE2EHarness(t)
	inbound := h.buildInboundPipeline(t)
	outbound := h.buildOutboundPipeline(t)

	// Sign a fresh JWT for "alice" and present it on inbound.
	userJWT := h.signJWT(t, map[string]any{"sub": "alice"})
	pctxIn, actionIn := runInbound(t, inbound, userJWT)

	if actionIn.Type != pipeline.Continue {
		t.Fatalf("inbound: expected Continue, got %v (violation=%+v)", actionIn.Type, actionIn.Violation)
	}
	if pctxIn.Identity == nil || pctxIn.Identity.Subject() != "alice" {
		t.Fatalf("inbound: Identity.Subject() = %v, want alice", pctxIn.Identity)
	}

	// The cache should now hold alice's token.
	cached, sub, ok := auth.GetSingleUserToken(time.Now())
	if !ok {
		t.Fatal("inbound did not capture token into single-user store")
	}
	if cached != userJWT {
		t.Errorf("cached token mismatch: got len=%d, want len=%d", len(cached), len(userJWT))
	}
	if sub != "alice" {
		t.Errorf("cached subject = %q, want alice", sub)
	}

	// Now simulate the agent making an outbound call WITHOUT propagating
	// the Authorization header (the LangChain/CrewAI/Claude Code shape).
	pctxOut, actionOut := runOutbound(t, outbound, "")
	if actionOut.Type != pipeline.Continue {
		t.Fatalf("outbound: expected Continue, got %v (violation=%+v)", actionOut.Type, actionOut.Violation)
	}

	// The downstream-bound Authorization header should be the EXCHANGED
	// version of alice's cached token.
	gotAuth := pctxOut.Headers.Get("Authorization")
	wantAuth := "Bearer exchanged-" + userJWT
	if gotAuth != wantAuth {
		t.Errorf("outbound Authorization mismatch:\n  got:  %q\n  want: %q", gotAuth, wantAuth)
	}

	// And the exchange endpoint should have seen alice's token as
	// subject_token (closing the loop end-to-end).
	if len(h.receivedSubjectTokens) != 1 {
		t.Fatalf("token exchange: got %d calls, want 1", len(h.receivedSubjectTokens))
	}
	if h.receivedSubjectTokens[0] != userJWT {
		t.Errorf("subject_token sent to IdP did not match captured inbound token")
	}

	// Telemetry: one cache_hit invocation from the cache lookup, one
	// token_replaced invocation from the exchange.
	if pctxOut.Extensions.Invocations == nil {
		t.Fatal("outbound invocations not recorded")
	}
	var cacheHit, replaced bool
	for _, inv := range pctxOut.Extensions.Invocations.Outbound {
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
		t.Error("expected token_replaced invocation")
	}
}

// TestE2E_SinglePlayerMode_DisabledFallsThroughToNoTokenPolicy: the same
// scenario as the happy path but with single_user_mode=false. The cache
// is never consulted, so the outbound request is denied by the default
// no_token_policy.
func TestE2E_SinglePlayerMode_DisabledFallsThroughToNoTokenPolicy(t *testing.T) {
	withSingleUserMode(t, false)
	t.Cleanup(auth.ResetSingleUserToken)

	h := newE2EHarness(t)
	inbound := h.buildInboundPipeline(t)
	outbound := h.buildOutboundPipeline(t)

	userJWT := h.signJWT(t, map[string]any{"sub": "alice"})
	if _, action := runInbound(t, inbound, userJWT); action.Type != pipeline.Continue {
		t.Fatalf("inbound: expected Continue, got %v", action.Type)
	}

	// Capture should have been a no-op (Store gated).
	if _, _, ok := auth.GetSingleUserToken(time.Now()); ok {
		// We can't directly probe the gate-disabled state without
		// re-enabling, but Get also short-circuits when disabled, so
		// this branch can't fire under normal conditions. Still
		// asserts the right post-condition.
		t.Error("expected cache miss while single-user mode is disabled")
	}

	// Outbound with no header should fail the no-token-policy check.
	_, action := runOutbound(t, outbound, "")
	if action.Type != pipeline.Reject {
		t.Errorf("outbound: expected Reject (no_token_policy=deny), got %v", action.Type)
	}

	// IdP must NOT have been called.
	if len(h.receivedSubjectTokens) != 0 {
		t.Errorf("token exchange: got %d calls, want 0 (cache never consulted when disabled)", len(h.receivedSubjectTokens))
	}
}

// TestE2E_SinglePlayerMode_ExplicitHeaderWins: when the agent DOES
// propagate the inbound Authorization header (the well-behaved case),
// the cache must not override it.
func TestE2E_SinglePlayerMode_ExplicitHeaderWins(t *testing.T) {
	withSingleUserMode(t, true)
	t.Cleanup(auth.ResetSingleUserToken)

	h := newE2EHarness(t)
	inbound := h.buildInboundPipeline(t)
	outbound := h.buildOutboundPipeline(t)

	// Inbound: alice's JWT — captured into the cache.
	aliceJWT := h.signJWT(t, map[string]any{"sub": "alice"})
	if _, action := runInbound(t, inbound, aliceJWT); action.Type != pipeline.Continue {
		t.Fatalf("inbound: expected Continue, got %v", action.Type)
	}

	// Outbound WITH explicit (different) header — this is the
	// well-behaved agent that propagated something itself.
	explicitToken := "explicit-tok"
	pctxOut, actionOut := runOutbound(t, outbound, explicitToken)
	if actionOut.Type != pipeline.Continue {
		t.Fatalf("outbound: expected Continue, got %v", actionOut.Type)
	}

	// The exchange should have used the EXPLICIT token, not the cached one.
	gotAuth := pctxOut.Headers.Get("Authorization")
	wantAuth := "Bearer exchanged-" + explicitToken
	if gotAuth != wantAuth {
		t.Errorf("explicit header should win:\n  got:  %q\n  want: %q", gotAuth, wantAuth)
	}

	if len(h.receivedSubjectTokens) != 1 || h.receivedSubjectTokens[0] != explicitToken {
		t.Errorf("subject_token sent to IdP = %v, want [%s] (explicit)", h.receivedSubjectTokens, explicitToken)
	}

	// No cache_hit invocation (the lookup branch is gated on missing header).
	if pctxOut.Extensions.Invocations != nil {
		for _, inv := range pctxOut.Extensions.Invocations.Outbound {
			if inv.Reason == "single_user_cache_hit" {
				t.Error("cache_hit invocation should not fire when explicit header is present")
			}
		}
	}
}

// TestE2E_SinglePlayerMode_BypassPathDoesNotPoison: a /healthz request
// that bypasses jwt-validation should not populate the cache, even when
// it carries an Authorization header. Otherwise an unauthenticated
// liveness probe could poison the bridge.
func TestE2E_SinglePlayerMode_BypassPathDoesNotPoison(t *testing.T) {
	withSingleUserMode(t, true)
	t.Cleanup(auth.ResetSingleUserToken)

	h := newE2EHarness(t)
	inbound := h.buildInboundPipeline(t)

	// Send a /healthz request with a "Bearer suspicious-token" — it
	// should be bypassed before validation, and the cache stays empty.
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Headers:   http.Header{},
		Path:      "/healthz",
	}
	pctx.Headers.Set("Authorization", "Bearer suspicious-token")
	if action := inbound.Run(context.Background(), pctx); action.Type != pipeline.Continue {
		t.Fatalf("bypass: expected Continue, got %v", action.Type)
	}

	if _, _, ok := auth.GetSingleUserToken(time.Now()); ok {
		t.Error("bypass path must not populate single-user cache")
	}
}

// TestE2E_SinglePlayerMode_SecondInboundOverwrites simulates the
// single-player assumption being violated mid-session: alice's token is
// captured, then bob's token arrives. Last-write-wins (documented).
// Subsequent outbound exchanges use bob's token.
func TestE2E_SinglePlayerMode_SecondInboundOverwrites(t *testing.T) {
	withSingleUserMode(t, true)
	t.Cleanup(auth.ResetSingleUserToken)

	h := newE2EHarness(t)
	inbound := h.buildInboundPipeline(t)
	outbound := h.buildOutboundPipeline(t)

	aliceJWT := h.signJWT(t, map[string]any{"sub": "alice"})
	if _, action := runInbound(t, inbound, aliceJWT); action.Type != pipeline.Continue {
		t.Fatalf("alice inbound: %v", action.Type)
	}

	bobJWT := h.signJWT(t, map[string]any{"sub": "bob"})
	if _, action := runInbound(t, inbound, bobJWT); action.Type != pipeline.Continue {
		t.Fatalf("bob inbound: %v", action.Type)
	}

	// Outbound now should pick up BOB's token (last write).
	pctxOut, actionOut := runOutbound(t, outbound, "")
	if actionOut.Type != pipeline.Continue {
		t.Fatalf("outbound: %v", actionOut.Type)
	}
	wantAuth := "Bearer exchanged-" + bobJWT
	if got := pctxOut.Headers.Get("Authorization"); got != wantAuth {
		t.Errorf("expected last-write-wins (bob), got Authorization=%q", got)
	}

	// IdP saw bob's token.
	if len(h.receivedSubjectTokens) != 1 || h.receivedSubjectTokens[0] != bobJWT {
		t.Errorf("subject_token sent to IdP did not match bob's JWT")
	}
}

// TestE2E_SinglePlayerMode_NearExpiryTokenSkipped: a JWT that's about to
// expire (below the MinTTL floor) should NOT be cached — otherwise the
// next outbound exchange races the token's lifetime and may receive a
// fresh-but-already-expired access token.
func TestE2E_SinglePlayerMode_NearExpiryTokenSkipped(t *testing.T) {
	withSingleUserMode(t, true)
	t.Cleanup(auth.ResetSingleUserToken)

	h := newE2EHarness(t)
	inbound := h.buildInboundPipeline(t)

	// 10-second exp is below the 30-second floor.
	nearExpiryJWT := h.signJWT(t, map[string]any{
		"sub": "alice",
		"exp": time.Now().Add(10 * time.Second).Unix(),
	})
	if _, action := runInbound(t, inbound, nearExpiryJWT); action.Type != pipeline.Continue {
		t.Fatalf("inbound: %v", action.Type)
	}

	if _, _, ok := auth.GetSingleUserToken(time.Now()); ok {
		t.Error("near-expiry token should not be cached (below MinTTL floor)")
	}
}
