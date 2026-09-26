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
// Error Handling Tests
// =============================================================================

func TestClient_AcquireToken_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized","message":"session expired"}`))
	}))
	defer srv.Close()

	client := NewClient()
	_, err := client.AcquireToken(context.Background(), srv.URL, "expired-token", "https://api.github.com", "", "")

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	brokerErr, ok := err.(*BrokerError)
	if !ok {
		t.Fatalf("expected *BrokerError, got %T", err)
	}
	if brokerErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", brokerErr.StatusCode)
	}
	if brokerErr.OAuthError != "unauthorized" {
		t.Errorf("oauth_error = %q, want unauthorized", brokerErr.OAuthError)
	}
}

func TestClient_AcquireToken_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestTimeout)
		w.Write([]byte(`{"error":"timeout","message":"user authorization timed out"}`))
	}))
	defer srv.Close()

	client := NewClient()
	_, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.github.com", "", "")

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	brokerErr, ok := err.(*BrokerError)
	if !ok {
		t.Fatalf("expected *BrokerError, got %T", err)
	}
	if brokerErr.StatusCode != http.StatusRequestTimeout {
		t.Errorf("status = %d, want 408", brokerErr.StatusCode)
	}
}

func TestClient_AcquireToken_AdditionalStatusCodes(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantErr    bool
	}{
		{"400 Bad Request", http.StatusBadRequest, true},
		{"403 Forbidden", http.StatusForbidden, true},
		{"404 Not Found", http.StatusNotFound, true},
		{"429 Too Many Requests", http.StatusTooManyRequests, true},
		{"500 Internal Server Error", http.StatusInternalServerError, true},
		{"502 Bad Gateway", http.StatusBadGateway, true},
		{"503 Service Unavailable", http.StatusServiceUnavailable, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			helper := NewTestHelper(t)
			srv := helper.NewErrorBroker(tt.statusCode, "error", "test error")
			defer srv.Close()

			client := NewClient()
			_, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.github.com", "", "")

			if (err != nil) != tt.wantErr {
				t.Errorf("AcquireToken() error = %v, wantErr %v", err, tt.wantErr)
			}

			if err != nil {
				brokerErr, ok := err.(*BrokerError)
				if !ok {
					t.Errorf("expected *BrokerError, got %T", err)
				} else if brokerErr.StatusCode != tt.statusCode {
					t.Errorf("status = %d, want %d", brokerErr.StatusCode, tt.statusCode)
				}
			}
		})
	}
}

func TestClient_AcquireToken_NetworkError(t *testing.T) {
	client := NewClient()
	_, err := client.AcquireToken(context.Background(), "http://invalid-host-that-does-not-exist:9999", "token", "https://api.github.com", "", "")

	if err == nil {
		t.Fatal("expected network error, got nil")
	}
	if !strings.Contains(err.Error(), "token broker request failed") {
		t.Fatalf("error = %v, want wrapped broker request failure", err)
	}
}

func TestClient_AcquireToken_DNSFailure(t *testing.T) {
	client := NewClient()
	_, err := client.AcquireToken(context.Background(),
		"http://this-domain-definitely-does-not-exist-12345.invalid",
		"token",
		"https://api.example.com",
		"",
		"")

	if err == nil {
		t.Fatal("expected DNS error")
	}
	if !strings.Contains(err.Error(), "token broker request failed") {
		t.Errorf("error should mention broker request failure, got: %v", err)
	}
}

// =============================================================================
// JSON Response Tests
// =============================================================================

func TestClient_AcquireToken_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{invalid json`))
	}))
	defer srv.Close()

	client := NewClient()
	_, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.github.com", "", "")

	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
	if !strings.Contains(err.Error(), "parsing") {
		t.Errorf("error should mention parsing failure, got: %v", err)
	}
}

func TestClient_AcquireToken_MissingTokenField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	client := NewClient()
	_, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.github.com", "", "")

	if err == nil {
		t.Fatal("expected error for missing token field, got nil")
	}
	if !strings.Contains(err.Error(), "missing token") {
		t.Errorf("error should mention missing token, got: %v", err)
	}
}

func TestClient_AcquireToken_EmptyToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"token": ""})
	}))
	defer srv.Close()

	client := NewClient()
	_, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.github.com", "", "")

	if err == nil {
		t.Fatal("expected error for empty token, got nil")
	}
	if !strings.Contains(err.Error(), "missing token") {
		t.Errorf("error should mention missing token, got: %v", err)
	}
}

func TestClient_AcquireToken_ExtraJSONFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"token":       "extra-fields-token",
			"extra_field": "should be ignored",
			"version":     "2.0",
			"metadata":    map[string]string{"key": "value"},
		})
	}))
	defer srv.Close()

	client := NewClient()
	token, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.github.com", "", "")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "extra-fields-token" {
		t.Errorf("token = %q, want extra-fields-token", token)
	}
}

func TestClient_AcquireToken_NonJSONErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("bad gateway"))
	}))
	defer srv.Close()

	client := NewClient()
	_, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.github.com", "", "")

	if err == nil {
		t.Fatal("expected broker error, got nil")
	}
	brokerErr, ok := err.(*BrokerError)
	if !ok {
		t.Fatalf("expected *BrokerError, got %T", err)
	}
	if brokerErr.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", brokerErr.StatusCode, http.StatusBadGateway)
	}
}

func TestClient_AcquireToken_PartialJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"token":"partial`))

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			conn.Close()
		}
	}))
	defer srv.Close()

	client := NewClient()
	_, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.github.com", "", "")

	if err == nil {
		t.Error("expected error for partial JSON, got nil")
	}
	if !strings.Contains(err.Error(), "parsing") && !strings.Contains(err.Error(), "EOF") {
		t.Errorf("error should mention parsing or EOF, got: %v", err)
	}
}
