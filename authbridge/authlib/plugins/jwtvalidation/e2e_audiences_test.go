package jwtvalidation

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// E2E-style test: full jwt-validation Configure + OnRequest against a
// real in-memory JWKS and a signed JWT whose aud claim is an array.
// Complements validation/jwks_test.go (unit JWKSVerifier) by exercising
// the plugin stack end-to-end.

func e2eJWKS(t *testing.T) (*rsa.PrivateKey, *httptest.Server) {
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

func e2eSignToken(t *testing.T, privKey *rsa.PrivateKey, claims map[string]interface{}) string {
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

func TestJWTValidation_E2E_acceptsJWTWithAudArray_viaAllowedAudiences(t *testing.T) {
	priv, jwksSrv := e2eJWKS(t)
	iss := "http://test-issuer"
	token := e2eSignToken(t, priv, map[string]interface{}{
		"iss": iss,
		"aud": []string{"other-service", "accepted-aud"},
		"sub": "user-123",
		"exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Unix(),
	})

	raw := []byte(`{
		"issuer": "` + iss + `",
		"jwks_url": "` + jwksSrv.URL + `",
		"audience": "",
		"audience_file": "",
		"allowed_audiences": ["accepted-aud"]
	}`)

	p := NewJWTValidation()
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if !p.Ready() {
		t.Fatal("expected plugin Ready() after Configure")
	}

	pctx := &pipeline.Context{Headers: http.Header{}, Path: "/api/call"}
	pctx.Headers.Set("Authorization", "Bearer "+token)
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v violation=%+v", action.Type, action.Violation)
	}
}
