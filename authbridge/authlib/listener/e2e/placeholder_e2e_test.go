// Package e2e contains cross-listener, in-process integration tests that
// must import BOTH the reverseproxy (inbound) and forwardproxy (outbound)
// listeners. It lives in its own test-only package to avoid an import
// cycle between the two listener packages.
package e2e

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/rossoctl/rossocortex/authbridge/authlib/listener/forwardproxy"
	"github.com/rossoctl/rossocortex/authbridge/authlib/listener/reverseproxy"
	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/rossocortex/authbridge/authlib/placeholder"
	jwtvalidation "github.com/rossoctl/rossocortex/authbridge/authlib/plugins/jwtvalidation"
	tokenexchange "github.com/rossoctl/rossocortex/authbridge/authlib/plugins/tokenexchange"
	"github.com/rossoctl/rossocortex/authbridge/authlib/shared"
)

// startJWKS mints an RSA keypair, serves its public half as a JWKS, and
// returns the private key (for signing) and the JWKS server. Mirrors the
// helper in plugins/jwtvalidation/e2e_audiences_test.go.
func startJWKS(t *testing.T) (*rsa.PrivateKey, *httptest.Server) {
	t.Helper()
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pubJWK, err := jwk.FromRaw(privKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	_ = pubJWK.Set(jwk.KeyIDKey, "e2e-key-1")
	_ = pubJWK.Set(jwk.AlgorithmKey, jwa.RS256)
	keySet := jwk.NewSet()
	_ = keySet.AddKey(pubJWK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(keySet)
	}))
	t.Cleanup(srv.Close)
	return privKey, srv
}

// signToken signs a JWT with the given claims using privKey.
func signToken(t *testing.T, privKey *rsa.PrivateKey, claims map[string]interface{}) string {
	t.Helper()
	builder := jwt.New()
	for k, v := range claims {
		_ = builder.Set(k, v)
	}
	privJWK, err := jwk.FromRaw(privKey)
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

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	return u
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TestPlaceholderSwap_EndToEnd proves, through real in-process HTTP
// round-trips, that:
//
//   - The agent backend (what reverseproxy forwards to) only ever sees an
//     opaque "abph_" placeholder, never the real user JWT.
//   - The real JWT is stored server-side in a shared store, recoverable
//     only inside the proxy via the handle.
//   - When the agent makes an outbound call through the forwardproxy, the
//     placeholder is resolved and exchanged (RFC 8693), so the external
//     upstream receives the EXCHANGED token — neither the real JWT nor the
//     raw placeholder.
//
// The whole point is a single process with a single shared store wired
// into BOTH listeners.
func TestPlaceholderSwap_EndToEnd(t *testing.T) {
	// 1. JWKS + signed JWT --------------------------------------------------
	priv, jwksSrv := startJWKS(t)
	issuer := "http://test-issuer"
	realJWT := signToken(t, priv, map[string]interface{}{
		"iss": issuer,
		"aud": "agent",
		"sub": "user@example.com",
		"exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Unix(),
	})

	// 2. Token-exchange stub ------------------------------------------------
	exchangeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "EXCHANGED-TOKEN",
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	}))
	defer exchangeSrv.Close()

	// 3. Upstream server (external API the agent calls) ---------------------
	var upstreamReceivedAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamReceivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream-ok"))
	}))
	defer upstream.Close()

	// 5. Shared store — ONE store, shared by both listeners. -----------------
	store := shared.New()

	// 7. Outbound stack (built first so the agent backend can route
	//    through it). token-exchange resolves the placeholder and exchanges.
	txPlugin := tokenexchange.NewTokenExchange()
	txCfg := []byte(`{
	  "token_url":"` + exchangeSrv.URL + `",
	  "default_policy":"exchange",
	  "resolve_placeholders":true,
	  "identity":{"type":"client-secret","client_id":"agent","client_secret":"secret"}
	}`)
	if err := txPlugin.Configure(txCfg); err != nil {
		t.Fatalf("token-exchange Configure: %v", err)
	}
	txPipe, err := pipeline.New([]pipeline.Plugin{txPlugin})
	if err != nil {
		t.Fatalf("outbound pipeline.New: %v", err)
	}
	fpSrv, err := forwardproxy.NewServer(pipeline.NewHolder(txPipe), nil, nil)
	if err != nil {
		t.Fatalf("forwardproxy.NewServer: %v", err)
	}
	fpSrv.Shared = store
	forwardProxy := httptest.NewServer(fpSrv.Handler())
	defer forwardProxy.Close()

	// 4. Agent backend (what reverseproxy forwards to). Records the
	//    Authorization header IT sees, then simulates the agent making an
	//    outbound call THROUGH the forwardproxy to the upstream, carrying
	//    exactly the Authorization header the agent received.
	var agentReceivedAuth string
	agentBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentReceivedAuth = r.Header.Get("Authorization")

		// Agent's outbound client routes through the forward proxy.
		egressClient := &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyURL(mustParseURL(t, forwardProxy.URL)),
			},
		}
		egressReq, _ := http.NewRequest(http.MethodGet, upstream.URL+"/external", nil)
		// The agent forwards whatever Authorization it was handed — the
		// opaque placeholder, never the real token (it never had it).
		egressReq.Header.Set("Authorization", agentReceivedAuth)
		egressResp, err := egressClient.Do(egressReq)
		if err != nil {
			http.Error(w, "agent egress failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		_, _ = io.Copy(io.Discard, egressResp.Body)
		egressResp.Body.Close()

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("agent-ok"))
	}))
	defer agentBackend.Close()

	// 6. Inbound stack: jwt-validation in placeholder mode, reverseproxy
	//    forwarding to the agent backend, sharing the SAME store.
	jwtPlugin := jwtvalidation.NewJWTValidation()
	jwtCfg := []byte(`{
	  "issuer":"` + issuer + `",
	  "jwks_url":"` + jwksSrv.URL + `",
	  "audience":"agent",
	  "placeholder_mode":true
	}`)
	if err := jwtPlugin.Configure(jwtCfg); err != nil {
		t.Fatalf("jwt-validation Configure: %v", err)
	}
	if !jwtPlugin.Ready() {
		t.Fatal("jwt-validation not Ready after Configure")
	}
	inboundPipe, err := pipeline.New([]pipeline.Plugin{jwtPlugin})
	if err != nil {
		t.Fatalf("inbound pipeline.New: %v", err)
	}
	rpSrv, err := reverseproxy.NewServer(pipeline.NewHolder(inboundPipe), nil, agentBackend.URL, nil)
	if err != nil {
		t.Fatalf("reverseproxy.NewServer: %v", err)
	}
	rpSrv.Shared = store
	reverseProxy := httptest.NewServer(rpSrv.Handler())
	defer reverseProxy.Close()

	// 8. Drive it: client → reverseproxy with the REAL JWT. ------------------
	req, _ := http.NewRequest(http.MethodGet, reverseProxy.URL+"/work", nil)
	req.Header.Set("Authorization", "Bearer "+realJWT)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("reverseproxy status = %d, want 200 (body: %s)", resp.StatusCode, body)
	}

	// --- Observability for the human ---------------------------------------
	agentBearer := strings.TrimPrefix(agentReceivedAuth, "Bearer ")
	t.Logf("real JWT (caller sent):      Bearer %s", trunc(realJWT, 25))
	t.Logf("agent received:              %s  <-- opaque handle, NOT the real token", agentReceivedAuth)
	if storedRaw, ok := store.Get(placeholder.Key(agentBearer)); ok {
		t.Logf("store[%s] holds: Bearer %s  <-- real token kept server-side", trunc(agentBearer, 12), trunc(storedRaw.(string), 25))
	}
	t.Logf("upstream received:           %s  <-- exchanged token, not the real JWT and not the handle", upstreamReceivedAuth)

	// --- Assertions --------------------------------------------------------

	// Agent saw an opaque placeholder.
	if !strings.HasPrefix(agentReceivedAuth, "Bearer "+placeholder.Prefix) {
		t.Fatalf("agent Authorization = %q, want prefix %q", agentReceivedAuth, "Bearer "+placeholder.Prefix)
	}
	if !placeholder.IsPlaceholder(agentBearer) {
		t.Fatalf("agent bearer %q is not a placeholder", agentBearer)
	}

	// The real token NEVER reached the agent.
	if strings.Contains(agentReceivedAuth, realJWT) {
		t.Fatalf("LEAK: agent Authorization %q contains the real JWT", agentReceivedAuth)
	}

	// The shared store, keyed by the handle the agent got, returns the real JWT.
	stored, ok := store.Get(placeholder.Key(agentBearer))
	if !ok {
		t.Fatalf("shared store has no entry for handle %q", agentBearer)
	}
	if stored.(string) != realJWT {
		t.Fatalf("store holds %q, want the real JWT", trunc(stored.(string), 25))
	}

	// The external upstream saw the EXCHANGED token — not the real JWT,
	// not the raw placeholder handle.
	if upstreamReceivedAuth != "Bearer EXCHANGED-TOKEN" {
		t.Fatalf("upstream Authorization = %q, want Bearer EXCHANGED-TOKEN", upstreamReceivedAuth)
	}
	if strings.Contains(upstreamReceivedAuth, realJWT) {
		t.Fatalf("LEAK: upstream Authorization %q contains the real JWT", upstreamReceivedAuth)
	}
	if strings.Contains(upstreamReceivedAuth, placeholder.Prefix) {
		t.Fatalf("LEAK: upstream Authorization %q still contains the placeholder handle", upstreamReceivedAuth)
	}
}
