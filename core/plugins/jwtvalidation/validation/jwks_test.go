package validation

import (
	"context"
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
)

func TestClaims_HasAudience(t *testing.T) {
	c := &Claims{Audience: []string{"aud-1", "aud-2"}}
	if !c.HasAudience("aud-1") {
		t.Error("expected HasAudience(aud-1) = true")
	}
	if c.HasAudience("aud-3") {
		t.Error("expected HasAudience(aud-3) = false")
	}
}

func TestClaims_HasScope(t *testing.T) {
	c := &Claims{Scopes: []string{"openid", "profile"}}
	if !c.HasScope("openid") {
		t.Error("expected HasScope(openid) = true")
	}
	if c.HasScope("email") {
		t.Error("expected HasScope(email) = false")
	}
}

func TestClaims_EmptyAudience(t *testing.T) {
	c := &Claims{}
	if c.HasAudience("anything") {
		t.Error("expected HasAudience = false on nil audience")
	}
}

// --- JWKSVerifier integration tests with in-memory JWKS ---

func setupTestJWKS(t *testing.T) (*rsa.PrivateKey, *httptest.Server) {
	t.Helper()
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	pubJWK, err := jwk.FromRaw(privKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubJWK.Set(jwk.KeyIDKey, "test-key-1")
	pubJWK.Set(jwk.AlgorithmKey, jwa.RS256)

	keySet := jwk.NewSet()
	keySet.AddKey(pubJWK)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(keySet)
	}))

	return privKey, srv
}

func signToken(t *testing.T, privKey *rsa.PrivateKey, claims map[string]interface{}) string {
	t.Helper()
	builder := jwt.New()
	for k, v := range claims {
		builder.Set(k, v)
	}

	privJWK, err := jwk.FromRaw(privKey)
	if err != nil {
		t.Fatal(err)
	}
	privJWK.Set(jwk.KeyIDKey, "test-key-1")
	privJWK.Set(jwk.AlgorithmKey, jwa.RS256)

	signed, err := jwt.Sign(builder, jwt.WithKey(jwa.RS256, privJWK))
	if err != nil {
		t.Fatal(err)
	}
	return string(signed)
}

func TestJWKSVerifier_ValidToken(t *testing.T) {
	privKey, jwksSrv := setupTestJWKS(t)
	defer jwksSrv.Close()

	ctx := context.Background()
	v, err := NewJWKSVerifier(ctx, jwksSrv.URL, "http://test-issuer")
	if err != nil {
		t.Fatal(err)
	}

	token := signToken(t, privKey, map[string]interface{}{
		"iss": "http://test-issuer",
		"aud": []string{"my-agent"},
		"sub": "user-123",
		"azp": "caller-app",
		"exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Unix(),
	})

	claims, err := v.Verify(ctx, token, []string{"my-agent"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claims.Subject != "user-123" {
		t.Errorf("subject = %q, want user-123", claims.Subject)
	}
	if claims.ClientID != "caller-app" {
		t.Errorf("client_id = %q, want caller-app", claims.ClientID)
	}
	if !claims.HasAudience("my-agent") {
		t.Error("expected audience my-agent")
	}
}

func TestJWKSVerifier_ExpiredToken(t *testing.T) {
	privKey, jwksSrv := setupTestJWKS(t)
	defer jwksSrv.Close()

	ctx := context.Background()
	v, _ := NewJWKSVerifier(ctx, jwksSrv.URL, "http://test-issuer")

	token := signToken(t, privKey, map[string]interface{}{
		"iss": "http://test-issuer",
		"aud": []string{"my-agent"},
		"sub": "user",
		"exp": time.Now().Add(-1 * time.Minute).Unix(),
		"iat": time.Now().Add(-5 * time.Minute).Unix(),
	})

	_, err := v.Verify(ctx, token, []string{"my-agent"})
	if err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestJWKSVerifier_WrongIssuer(t *testing.T) {
	privKey, jwksSrv := setupTestJWKS(t)
	defer jwksSrv.Close()

	ctx := context.Background()
	v, _ := NewJWKSVerifier(ctx, jwksSrv.URL, "http://expected-issuer")

	token := signToken(t, privKey, map[string]interface{}{
		"iss": "http://wrong-issuer",
		"aud": []string{"my-agent"},
		"sub": "user",
		"exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Unix(),
	})

	_, err := v.Verify(ctx, token, []string{"my-agent"})
	if err == nil {
		t.Fatal("expected error for wrong issuer")
	}
}

func TestJWKSVerifier_WrongAudience(t *testing.T) {
	privKey, jwksSrv := setupTestJWKS(t)
	defer jwksSrv.Close()

	ctx := context.Background()
	v, _ := NewJWKSVerifier(ctx, jwksSrv.URL, "http://test-issuer")

	token := signToken(t, privKey, map[string]interface{}{
		"iss": "http://test-issuer",
		"aud": []string{"other-agent"},
		"sub": "user",
		"exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Unix(),
	})

	_, err := v.Verify(ctx, token, []string{"my-agent"})
	if err == nil {
		t.Fatal("expected error for wrong audience")
	}
}

func TestJWKSVerifier_EmptyAudience_Rejected(t *testing.T) {
	privKey, jwksSrv := setupTestJWKS(t)
	defer jwksSrv.Close()

	ctx := context.Background()
	v, _ := NewJWKSVerifier(ctx, jwksSrv.URL, "http://test-issuer")

	token := signToken(t, privKey, map[string]interface{}{
		"iss": "http://test-issuer",
		"aud": []string{"my-agent"},
		"sub": "user",
		"exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Unix(),
	})

	_, err := v.Verify(ctx, token, nil)
	if err == nil {
		t.Fatal("expected error for empty audiences (confused deputy protection)")
	}
}

func TestJWKSVerifier_InvalidSignature(t *testing.T) {
	_, jwksSrv := setupTestJWKS(t)
	defer jwksSrv.Close()

	// Sign with a DIFFERENT key than the one served by JWKS
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)

	ctx := context.Background()
	v, _ := NewJWKSVerifier(ctx, jwksSrv.URL, "http://test-issuer")

	token := signToken(t, otherKey, map[string]interface{}{
		"iss": "http://test-issuer",
		"aud": []string{"my-agent"},
		"sub": "user",
		"exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Unix(),
	})

	_, err := v.Verify(ctx, token, []string{"my-agent"})
	if err == nil {
		t.Fatal("expected error for invalid signature")
	}
}

func TestJWKSVerifier_MultiAudience_MatchSecond(t *testing.T) {
	privKey, jwksSrv := setupTestJWKS(t)
	defer jwksSrv.Close()

	ctx := context.Background()
	v, err := NewJWKSVerifier(ctx, jwksSrv.URL, "http://test-issuer")
	if err != nil {
		t.Fatal(err)
	}

	// Token has multiple audiences; expected audience matches the second one
	token := signToken(t, privKey, map[string]interface{}{
		"iss": "http://test-issuer",
		"aud": []string{"other-service", "my-agent"},
		"sub": "user-123",
		"exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Unix(),
	})

	claims, err := v.Verify(ctx, token, []string{"my-agent"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !claims.HasAudience("my-agent") {
		t.Error("expected audience my-agent")
	}
}

func TestJWKSVerifier_MultiExpectedAudiences_OneMatches(t *testing.T) {
	privKey, jwksSrv := setupTestJWKS(t)
	defer jwksSrv.Close()

	ctx := context.Background()
	v, err := NewJWKSVerifier(ctx, jwksSrv.URL, "http://test-issuer")
	if err != nil {
		t.Fatal(err)
	}

	// Token has audiences that overlap with the second expected audience
	token := signToken(t, privKey, map[string]interface{}{
		"iss": "http://test-issuer",
		"aud": []string{"other", "my-agent"},
		"sub": "user-123",
		"exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Unix(),
	})

	claims, err := v.Verify(ctx, token, []string{"no-match", "my-agent"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !claims.HasAudience("my-agent") {
		t.Error("expected audience my-agent")
	}
}

func TestJWKSVerifier_MultiExpectedAudiences_NoneMatch(t *testing.T) {
	privKey, jwksSrv := setupTestJWKS(t)
	defer jwksSrv.Close()

	ctx := context.Background()
	v, _ := NewJWKSVerifier(ctx, jwksSrv.URL, "http://test-issuer")

	// Token audience doesn't match any of the expected audiences
	token := signToken(t, privKey, map[string]interface{}{
		"iss": "http://test-issuer",
		"aud": []string{"unrelated-service", "another-service"},
		"sub": "user",
		"exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Unix(),
	})

	_, err := v.Verify(ctx, token, []string{"aud1", "aud2"})
	if err == nil {
		t.Fatal("expected error when no expected audience matches token audiences")
	}
}
