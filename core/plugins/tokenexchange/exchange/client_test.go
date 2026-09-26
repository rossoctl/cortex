package exchange

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExchange_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if got := r.FormValue("grant_type"); got != "urn:ietf:params:oauth:grant-type:token-exchange" {
			t.Errorf("grant_type = %q", got)
		}
		if got := r.FormValue("subject_token"); got != "original-token" {
			t.Errorf("subject_token = %q", got)
		}
		if got := r.FormValue("audience"); got != "target-aud" {
			t.Errorf("audience = %q", got)
		}
		if got := r.FormValue("client_id"); got != "my-client" {
			t.Errorf("client_id = %q", got)
		}
		if got := r.FormValue("client_secret"); got != "my-secret" {
			t.Errorf("client_secret = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "exchanged-token",
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, &ClientSecretAuth{
		ClientID:     "my-client",
		ClientSecret: "my-secret",
	})

	resp, err := client.Exchange(context.Background(), &ExchangeRequest{
		SubjectToken: "original-token",
		Audience:     "target-aud",
		Scopes:       "openid",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.AccessToken != "exchanged-token" {
		t.Errorf("access_token = %q, want %q", resp.AccessToken, "exchanged-token")
	}
	if resp.ExpiresIn != 300 {
		t.Errorf("expires_in = %d, want 300", resp.ExpiresIn)
	}
}

func TestExchange_OAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_grant",
			"error_description": "subject token expired",
		})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, &ClientSecretAuth{ClientID: "c", ClientSecret: "s"})
	_, err := client.Exchange(context.Background(), &ExchangeRequest{
		SubjectToken: "expired-token",
		Audience:     "aud",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	exchErr, ok := err.(*ExchangeError)
	if !ok {
		t.Fatalf("expected *ExchangeError, got %T", err)
	}
	if exchErr.StatusCode != 400 {
		t.Errorf("status = %d, want 400", exchErr.StatusCode)
	}
	if exchErr.OAuthError != "invalid_grant" {
		t.Errorf("oauth_error = %q, want %q", exchErr.OAuthError, "invalid_grant")
	}
}

func TestClientCredentials_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if got := r.FormValue("grant_type"); got != "client_credentials" {
			t.Errorf("grant_type = %q", got)
		}
		if got := r.FormValue("audience"); got != "target-aud" {
			t.Errorf("audience = %q, want target-aud", got)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "cc-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, &ClientSecretAuth{ClientID: "c", ClientSecret: "s"})
	resp, err := client.ClientCredentials(context.Background(), "target-aud", "openid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.AccessToken != "cc-token" {
		t.Errorf("access_token = %q, want %q", resp.AccessToken, "cc-token")
	}
}

func TestExchange_WithActorToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if got := r.FormValue("actor_token"); got != "actor-jwt" {
			t.Errorf("actor_token = %q, want %q", got, "actor-jwt")
		}
		if got := r.FormValue("actor_token_type"); got != "urn:ietf:params:oauth:token-type:access_token" {
			t.Errorf("actor_token_type = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "delegated-token",
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, &ClientSecretAuth{ClientID: "c", ClientSecret: "s"})
	resp, err := client.Exchange(context.Background(), &ExchangeRequest{
		SubjectToken: "user-token",
		Audience:     "aud",
		ActorToken:   "actor-jwt",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.AccessToken != "delegated-token" {
		t.Errorf("access_token = %q", resp.AccessToken)
	}
}

func TestExchange_ConnectionFailure(t *testing.T) {
	client := NewClient("http://127.0.0.1:1", &ClientSecretAuth{ClientID: "c", ClientSecret: "s"})
	_, err := client.Exchange(context.Background(), &ExchangeRequest{
		SubjectToken: "token",
		Audience:     "aud",
	})
	if err == nil {
		t.Fatal("expected error for connection failure")
	}
}

func TestExchange_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not-json"))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, &ClientSecretAuth{ClientID: "c", ClientSecret: "s"})
	_, err := client.Exchange(context.Background(), &ExchangeRequest{
		SubjectToken: "token",
		Audience:     "aud",
	})
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestExchange_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Simulate slow server — but context should cancel before we respond
		select {}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	client := NewClient(srv.URL, &ClientSecretAuth{ClientID: "c", ClientSecret: "s"})
	_, err := client.Exchange(ctx, &ExchangeRequest{
		SubjectToken: "token",
		Audience:     "aud",
	})
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestJWTAssertionAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if got := r.FormValue("client_assertion"); got != "jwt-svid-token" {
			t.Errorf("client_assertion = %q", got)
		}
		if got := r.FormValue("client_assertion_type"); got != "urn:ietf:params:oauth:client-assertion-type:jwt-spiffe" {
			t.Errorf("client_assertion_type = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "spiffe-exchanged",
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, &JWTAssertionAuth{
		ClientID:      "spiffe-client",
		AssertionType: "urn:ietf:params:oauth:client-assertion-type:jwt-spiffe",
		TokenSource: func(_ context.Context) (string, error) {
			return "jwt-svid-token", nil
		},
	})
	resp, err := client.Exchange(context.Background(), &ExchangeRequest{
		SubjectToken: "user-token",
		Audience:     "target",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.AccessToken != "spiffe-exchanged" {
		t.Errorf("access_token = %q", resp.AccessToken)
	}
}
