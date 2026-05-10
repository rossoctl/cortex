package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// =============================================================================
// Basic Token Acquisition Tests
// =============================================================================

func TestClient_AcquireToken_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/sessions/token" {
			t.Errorf("expected /sessions/token, got %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("expected Bearer token in Authorization header, got %q", auth)
		}
		if serverURL := r.Header.Get("X-Server-Url"); serverURL == "" {
			t.Error("expected X-Server-Url header")
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"token": "gho_test_token_12345"})
	}))
	defer srv.Close()

	client := NewClient()
	token, err := client.AcquireToken(context.Background(), srv.URL, "user-jwt-token", "https://api.github.com", "", "")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "gho_test_token_12345" {
		t.Errorf("token = %q, want gho_test_token_12345", token)
	}
}

func TestClient_AcquireToken_RequestFormat(t *testing.T) {
	var capturedMethod, capturedPath, capturedAuth, capturedServerURL string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		capturedAuth = r.Header.Get("Authorization")
		capturedServerURL = r.Header.Get("X-Server-Url")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": "test-token"})
	}))
	defer srv.Close()

	client := NewClient()
	_, err := client.AcquireToken(context.Background(), srv.URL, "my-jwt-token", "https://target.example.com", "", "")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedMethod != "POST" {
		t.Errorf("method = %q, want POST", capturedMethod)
	}
	if capturedPath != "/sessions/token" {
		t.Errorf("path = %q, want /sessions/token", capturedPath)
	}
	if capturedAuth != "Bearer my-jwt-token" {
		t.Errorf("Authorization = %q, want Bearer my-jwt-token", capturedAuth)
	}
	if capturedServerURL != "https://target.example.com" {
		t.Errorf("X-Server-Url = %q, want https://target.example.com", capturedServerURL)
	}
}

func TestClient_AcquireToken_WithAuthorizationEndpoint(t *testing.T) {
	var capturedAuthEndpoint string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuthEndpoint = r.Header.Get("X-Authorization-Endpoint")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": "test-token"})
	}))
	defer srv.Close()

	client := NewClient()

	// Test with authorization endpoint
	_, err := client.AcquireToken(context.Background(), srv.URL, "my-jwt-token", "https://target.example.com", "https://auth.example.com/oauth/authorize", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedAuthEndpoint != "https://auth.example.com/oauth/authorize" {
		t.Errorf("X-Authorization-Endpoint = %q, want %q", capturedAuthEndpoint, "https://auth.example.com/oauth/authorize")
	}

	// Test without authorization endpoint (empty string)
	capturedAuthEndpoint = "should-be-cleared"
	_, err = client.AcquireToken(context.Background(), srv.URL, "my-jwt-token", "https://target.example.com", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedAuthEndpoint != "" {
		t.Errorf("X-Authorization-Endpoint = %q, want empty string", capturedAuthEndpoint)
	}
}

func TestClient_AcquireToken_WithTokenEndpoint(t *testing.T) {
	var capturedTokenEndpoint string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedTokenEndpoint = r.Header.Get("X-Token-Endpoint")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": "test-token"})
	}))
	defer srv.Close()

	client := NewClient()

	// Test with token endpoint
	_, err := client.AcquireToken(context.Background(), srv.URL, "my-jwt-token", "https://target.example.com", "", "https://auth.example.com/oauth/token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedTokenEndpoint != "https://auth.example.com/oauth/token" {
		t.Errorf("X-Token-Endpoint = %q, want %q", capturedTokenEndpoint, "https://auth.example.com/oauth/token")
	}

	// Test without token endpoint (empty string)
	capturedTokenEndpoint = "should-be-cleared"
	_, err = client.AcquireToken(context.Background(), srv.URL, "my-jwt-token", "https://target.example.com", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedTokenEndpoint != "" {
		t.Errorf("X-Token-Endpoint = %q, want empty string", capturedTokenEndpoint)
	}
}

func TestClient_AcquireToken_WithBothEndpoints(t *testing.T) {
	var capturedAuthEndpoint, capturedTokenEndpoint string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuthEndpoint = r.Header.Get("X-Authorization-Endpoint")
		capturedTokenEndpoint = r.Header.Get("X-Token-Endpoint")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": "test-token"})
	}))
	defer srv.Close()

	client := NewClient()

	// Test with both endpoints
	_, err := client.AcquireToken(context.Background(), srv.URL, "my-jwt-token", "https://target.example.com", "https://auth.example.com/oauth/authorize", "https://auth.example.com/oauth/token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedAuthEndpoint != "https://auth.example.com/oauth/authorize" {
		t.Errorf("X-Authorization-Endpoint = %q, want %q", capturedAuthEndpoint, "https://auth.example.com/oauth/authorize")
	}

	if capturedTokenEndpoint != "https://auth.example.com/oauth/token" {
		t.Errorf("X-Token-Endpoint = %q, want %q", capturedTokenEndpoint, "https://auth.example.com/oauth/token")
	}
}
