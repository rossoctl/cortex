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

