package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
