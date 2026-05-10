package tokenbroker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =============================================================================
// Test Helpers
// =============================================================================

// TestHelper provides utilities for tokenbroker client tests
type TestHelper struct {
	t testing.TB
}

// NewTestHelper creates a new test helper
func NewTestHelper(t testing.TB) *TestHelper {
	return &TestHelper{t: t}
}

// NewSuccessBroker creates a mock broker that returns a token
func (h *TestHelper) NewSuccessBroker(token string) *httptest.Server {
	h.t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"token": token})
	}))
}

// NewErrorBroker creates a mock broker that returns an error
func (h *TestHelper) NewErrorBroker(statusCode int, oauthError, message string) *httptest.Server {
	h.t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   oauthError,
			"message": message,
		})
	}))
}

// NewCapturingBroker creates a mock broker that captures request details
func (h *TestHelper) NewCapturingBroker(token string, captureFunc func(*http.Request)) *httptest.Server {
	h.t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if captureFunc != nil {
			captureFunc(r)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"token": token})
	}))
}

// NewDelayedBroker creates a mock broker that delays before responding
func (h *TestHelper) NewDelayedBroker(token string, delay func()) *httptest.Server {
	h.t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay != nil {
			delay()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"token": token})
	}))
}

// AssertBrokerError verifies a BrokerError has expected values
func (h *TestHelper) AssertBrokerError(err error, expectedStatus int, expectedError, expectedDesc string) {
	h.t.Helper()

	if err == nil {
		h.t.Fatal("expected BrokerError, got nil")
	}

	brokerErr, ok := err.(*BrokerError)
	if !ok {
		h.t.Fatalf("expected *BrokerError, got %T", err)
	}

	if brokerErr.StatusCode != expectedStatus {
		h.t.Errorf("StatusCode = %d, want %d", brokerErr.StatusCode, expectedStatus)
	}

	if brokerErr.OAuthError != expectedError {
		h.t.Errorf("OAuthError = %q, want %q", brokerErr.OAuthError, expectedError)
	}

	if brokerErr.OAuthDescription != expectedDesc {
		h.t.Errorf("OAuthDescription = %q, want %q", brokerErr.OAuthDescription, expectedDesc)
	}
}

// =============================================================================
// Basic Functionality Tests
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

// =============================================================================
// Context and Timeout Tests
// =============================================================================

func TestClient_AcquireToken_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	client := NewClient()
	_, err := client.AcquireToken(ctx, srv.URL, "user-token", "https://api.github.com", "", "")

	if err == nil {
		t.Fatal("expected context cancellation error, got nil")
	}
	if !strings.Contains(err.Error(), "context") && !strings.Contains(err.Error(), "deadline") {
		t.Errorf("error should mention context cancellation, got: %v", err)
	}
}

func TestClient_AcquireToken_ClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1 * time.Second)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"token": "late-token"})
	}))
	defer srv.Close()

	client := &Client{
		httpClient: &http.Client{Timeout: 100 * time.Millisecond},
	}

	_, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.github.com", "", "")

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timeout") && !strings.Contains(err.Error(), "deadline") {
		t.Errorf("error should mention timeout, got: %v", err)
	}
}

func TestClient_AcquireToken_ContextCancellationMidRequest(t *testing.T) {
	requestStarted := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient()

	errCh := make(chan error, 1)
	go func() {
		_, err := client.AcquireToken(ctx, srv.URL, "user-token", "https://api.example.com", "", "")
		errCh <- err
	}()

	<-requestStarted
	cancel()

	err := <-errCh
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context") {
		t.Errorf("expected context cancellation error, got: %v", err)
	}
}

func TestClient_AcquireToken_LongPolling(t *testing.T) {
	const brokerDelay = 200 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(brokerDelay)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"token": "delayed-token"})
	}))
	defer srv.Close()

	client := NewClient()
	start := time.Now()
	token, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.github.com", "", "")
	duration := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "delayed-token" {
		t.Errorf("token = %q, want delayed-token", token)
	}
	if duration < brokerDelay {
		t.Errorf("request completed too quickly: %v (expected >= %v)", duration, brokerDelay)
	}
}

// =============================================================================
// HTTP Features Tests
// =============================================================================

func TestClient_AcquireToken_HTTPRedirects(t *testing.T) {
	finalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": "redirected-token"})
	}))
	defer finalSrv.Close()

	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, finalSrv.URL+"/sessions/token", http.StatusFound)
	}))
	defer redirectSrv.Close()

	client := NewClient()
	token, err := client.AcquireToken(context.Background(), redirectSrv.URL, "user-token", "https://api.github.com", "", "")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "redirected-token" {
		t.Errorf("token = %q, want redirected-token", token)
	}
}

func TestClient_AcquireToken_ResponseHeaders(t *testing.T) {
	tests := []struct {
		name        string
		headers     map[string]string
		expectError bool
	}{
		{
			name:        "standard headers",
			headers:     map[string]string{"Content-Type": "application/json"},
			expectError: false,
		},
		{
			name:        "missing content-type",
			headers:     map[string]string{},
			expectError: false,
		},
		{
			name:        "extra headers",
			headers:     map[string]string{"Content-Type": "application/json", "X-Custom-Header": "value", "X-Request-ID": "12345"},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tt.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"token":"header-test-token"}`))
			}))
			defer srv.Close()

			client := NewClient()
			token, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.example.com", "", "")

			if tt.expectError && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.expectError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !tt.expectError && token != "header-test-token" {
				t.Errorf("token = %q, want header-test-token", token)
			}
		})
	}
}

func TestClient_AcquireToken_LargeResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		largeData := strings.Repeat("x", 2*1024*1024) // 2MB
		json.NewEncoder(w).Encode(map[string]string{
			"token": "small-token",
			"data":  largeData,
		})
	}))
	defer srv.Close()

	client := NewClient()
	_, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.github.com", "", "")

	if err == nil {
		t.Error("expected error for response larger than 1MB, got nil")
	}
}

// =============================================================================
// URL and Parameter Tests
// =============================================================================

func TestClient_AcquireToken_TrailingSlashBrokerURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": "slash-token"})
	}))
	defer srv.Close()

	client := NewClient()
	token, err := client.AcquireToken(context.Background(), srv.URL+"/", "user-token", "https://api.github.com", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "slash-token" {
		t.Errorf("token = %q, want slash-token", token)
	}
	if gotPath != "//sessions/token" {
		t.Errorf("path = %q, want %q", gotPath, "//sessions/token")
	}
}

func TestClient_AcquireToken_ServerURLEncoding(t *testing.T) {
	tests := []struct {
		name      string
		serverURL string
	}{
		{"URL with query parameters", "https://api.example.com/path?key=value&foo=bar"},
		{"URL with fragment", "https://api.example.com/path#section"},
		{"URL with encoded characters", "https://api.example.com/path%20with%20spaces"},
		{"URL with unicode", "https://api.example.com/path/日本語"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedServerURL string
			helper := NewTestHelper(t)
			srv := helper.NewCapturingBroker("test-token", func(r *http.Request) {
				capturedServerURL = r.Header.Get("X-Server-Url")
			})
			defer srv.Close()

			client := NewClient()
			_, err := client.AcquireToken(context.Background(), srv.URL, "user-token", tt.serverURL, "", "")

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if capturedServerURL != tt.serverURL {
				t.Errorf("X-Server-Url = %q, want %q", capturedServerURL, tt.serverURL)
			}
		})
	}
}

func TestClient_AcquireToken_EmptyParameters(t *testing.T) {
	helper := NewTestHelper(t)

	t.Run("empty broker URL", func(t *testing.T) {
		client := NewClient()
		_, err := client.AcquireToken(context.Background(), "", "token", "https://api.example.com", "", "")
		if err == nil {
			t.Fatal("expected error for empty broker URL")
		}
	})

	t.Run("empty token parameter", func(t *testing.T) {
		srv := helper.NewSuccessBroker("test-token")
		defer srv.Close()

		client := NewClient()
		token, err := client.AcquireToken(context.Background(), srv.URL, "", "https://api.example.com", "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if token != "test-token" {
			t.Errorf("token = %q, want test-token", token)
		}
	})

	t.Run("empty server URL", func(t *testing.T) {
		var capturedServerURL string
		srv := helper.NewCapturingBroker("test-token", func(r *http.Request) {
			capturedServerURL = r.Header.Get("X-Server-Url")
		})
		defer srv.Close()

		client := NewClient()
		_, err := client.AcquireToken(context.Background(), srv.URL, "token", "", "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if capturedServerURL != "" {
			t.Errorf("X-Server-Url = %q, want empty", capturedServerURL)
		}
	})
}

// =============================================================================
// Concurrency Tests
// =============================================================================

func TestClient_AcquireToken_ConcurrentRequests(t *testing.T) {
	var requestCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"token": "concurrent-token"})
	}))
	defer srv.Close()

	client := NewClient()

	const numRequests = 10
	results := make(chan error, numRequests)

	for i := 0; i < numRequests; i++ {
		go func() {
			_, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.github.com", "", "")
			results <- err
		}()
	}

	for i := 0; i < numRequests; i++ {
		if err := <-results; err != nil {
			t.Errorf("concurrent request %d failed: %v", i, err)
		}
	}

	finalCount := atomic.LoadInt32(&requestCount)
	if finalCount != numRequests {
		t.Errorf("expected %d requests, got %d", numRequests, finalCount)
	}
}

func TestClient_AcquireToken_ConcurrentDifferentServers(t *testing.T) {
	servers := make([]*httptest.Server, 5)
	helper := NewTestHelper(t)
	for i := range servers {
		token := string(rune('A' + i))
		servers[i] = helper.NewSuccessBroker("token-" + token)
		defer servers[i].Close()
	}

	client := NewClient()
	results := make(chan error, len(servers))

	for i, srv := range servers {
		go func(serverURL string, index int) {
			_, err := client.AcquireToken(context.Background(), serverURL, "user-token", "https://api.github.com", "", "")
			results <- err
		}(srv.URL, i)
	}

	for i := 0; i < len(servers); i++ {
		if err := <-results; err != nil {
			t.Errorf("concurrent request %d failed: %v", i, err)
		}
	}
}

func TestClient_AcquireToken_RaceConditions(t *testing.T) {
	helper := NewTestHelper(t)
	srv := helper.NewSuccessBroker("race-token")
	defer srv.Close()

	client := NewClient()

	const numGoroutines = 10
	results := make(chan error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			_, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.github.com", "", "")
			results <- err
		}()
	}

	for i := 0; i < numGoroutines; i++ {
		if err := <-results; err != nil {
			t.Errorf("goroutine %d failed: %v", i, err)
		}
	}
}

func TestClient_AcquireToken_ConnectionReuse(t *testing.T) {
	connectionCount := 0
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connectionCount++
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"token":"reuse-token"}`))
	}))
	defer srv.Close()

	client := NewClient()

	for i := 0; i < 5; i++ {
		_, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.example.com", "", "")
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
	}

	mu.Lock()
	count := connectionCount
	mu.Unlock()

	if count != 5 {
		t.Errorf("expected 5 requests, got %d", count)
	}
}

// =============================================================================
// Security Tests
// =============================================================================

func TestClient_Security_NoTokenLeakageInErrors(t *testing.T) {
	sensitiveToken := "secret-token-12345"

	client := NewClient()
	_, err := client.AcquireToken(context.Background(),
		"http://invalid-host-12345.invalid",
		sensitiveToken,
		"https://api.example.com",
		"",
		"")

	if err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, sensitiveToken) {
			t.Errorf("Token leaked in error message: %s", errMsg)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	_, err = client.AcquireToken(context.Background(), srv.URL, sensitiveToken, "https://api.example.com", "", "")
	if err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, sensitiveToken) {
			t.Errorf("Token leaked in broker error: %s", errMsg)
		}
	}
}

func TestClient_Security_HeaderInjection(t *testing.T) {
	tests := []struct {
		name      string
		token     string
		serverURL string
	}{
		{"newline in token", "token\nX-Injected: malicious", "https://api.example.com"},
		{"carriage return in token", "token\rX-Injected: malicious", "https://api.example.com"},
		{"newline in serverURL", "valid-token", "https://api.example.com\nX-Injected: malicious"},
		{"CRLF in serverURL", "valid-token", "https://api.example.com\r\nX-Injected: malicious"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var injectedHeader string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				injectedHeader = r.Header.Get("X-Injected")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"token":"test-token"}`))
			}))
			defer srv.Close()

			client := NewClient()
			_, _ = client.AcquireToken(context.Background(), srv.URL, tt.token, tt.serverURL, "", "")

			if injectedHeader != "" {
				t.Errorf("Header injection succeeded: X-Injected = %q", injectedHeader)
			}
		})
	}
}

func TestClient_Security_LargeTokenHandling(t *testing.T) {
	tests := []struct {
		name      string
		tokenSize int
	}{
		{"1KB token", 1024},
		{"10KB token", 10 * 1024},
		{"100KB token", 100 * 1024},
		{"1MB token", 1024 * 1024},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			largeToken := strings.Repeat("a", tt.tokenSize)
			helper := NewTestHelper(t)
			srv := helper.NewSuccessBroker("response-token")
			defer srv.Close()

			client := NewClient()
			_, err := client.AcquireToken(context.Background(), srv.URL, largeToken, "https://api.example.com", "", "")

			if err != nil {
				t.Logf("Large token (%d bytes) failed: %v", tt.tokenSize, err)
			} else {
				t.Logf("Large token (%d bytes) handled successfully", tt.tokenSize)
			}
		})
	}
}

func TestClient_Security_MaliciousResponseHandling(t *testing.T) {
	tests := []struct {
		name     string
		response string
		wantErr  bool
	}{
		{
			name:     "extremely nested JSON",
			response: strings.Repeat(`{"a":`, 1000) + `"value"` + strings.Repeat(`}`, 1000),
			wantErr:  true,
		},
		{
			name:     "JSON with null bytes",
			response: `{"token":"test\x00token"}`,
			wantErr:  false,
		},
		{
			name:     "JSON bomb (repeated keys)",
			response: `{"token":"a","token":"b","token":"c"}`,
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(tt.response))
			}))
			defer srv.Close()

			client := NewClient()
			_, err := client.AcquireToken(context.Background(), srv.URL, "token", "https://api.example.com", "", "")

			if tt.wantErr && err == nil {
				t.Error("expected error for malicious response")
			}
		})
	}
}

// =============================================================================
// Client Configuration Tests
// =============================================================================

func TestClient_AcquireToken_CustomHTTPClient(t *testing.T) {
	helper := NewTestHelper(t)
	srv := helper.NewSuccessBroker("custom-client-token")
	defer srv.Close()

	customClient := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{MaxIdleConns: 10},
	}

	client := &Client{httpClient: customClient}
	token, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.example.com", "", "")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "custom-client-token" {
		t.Errorf("token = %q, want custom-client-token", token)
	}
}

// =============================================================================
// Benchmark Tests
// =============================================================================

func BenchmarkAcquireToken_Success(b *testing.B) {
	helper := NewTestHelper(b)
	srv := helper.NewSuccessBroker("bench-token")
	defer srv.Close()

	client := NewClient()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := client.AcquireToken(ctx, srv.URL, "user-token", "https://api.github.com", "", "")
		if err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
	}
}

func BenchmarkAcquireToken_Error(b *testing.B) {
	helper := NewTestHelper(b)
	srv := helper.NewErrorBroker(http.StatusUnauthorized, "unauthorized", "test error")
	defer srv.Close()

	client := NewClient()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = client.AcquireToken(ctx, srv.URL, "user-token", "https://api.github.com", "", "")
	}
}

func BenchmarkAcquireToken_LargeToken(b *testing.B) {
	largeToken := strings.Repeat("x", 8192) // 8KB token
	helper := NewTestHelper(b)
	srv := helper.NewSuccessBroker(largeToken)
	defer srv.Close()

	client := NewClient()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := client.AcquireToken(ctx, srv.URL, "user-token", "https://api.github.com", "", "")
		if err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
	}
}

func BenchmarkAcquireToken_Parallel(b *testing.B) {
	helper := NewTestHelper(b)
	srv := helper.NewSuccessBroker("bench-token")
	defer srv.Close()

	client := NewClient()
	ctx := context.Background()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = client.AcquireToken(ctx, srv.URL, "user-token", "https://api.example.com", "", "")
		}
	})
}

func BenchmarkAcquireToken_Allocations(b *testing.B) {
	helper := NewTestHelper(b)
	srv := helper.NewSuccessBroker("alloc-token")
	defer srv.Close()

	client := NewClient()
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = client.AcquireToken(ctx, srv.URL, "user-token", "https://api.example.com", "", "")
	}
}

func BenchmarkNewClient(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = NewClient()
	}
}

func BenchmarkBrokerError_Error(b *testing.B) {
	err := &BrokerError{
		StatusCode:       401,
		OAuthError:       "unauthorized",
		OAuthDescription: "test error message",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = err.Error()
	}
}
