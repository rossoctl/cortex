package tokenbroker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// =============================================================================
// Test Helpers
// =============================================================================

// writeTempRoutesFile creates a temporary routes file for testing
func writeTempRoutesFile(t *testing.T, content string) (string, error) {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), "routes-*.yaml")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(strings.TrimSpace(content)); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// createSuccessBroker creates a mock broker that returns a successful token response
func createSuccessBroker(t *testing.T, token string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"token": token})
	}))
}

// createErrorBroker creates a mock broker that returns an error response
func createErrorBroker(t *testing.T, statusCode int, oauthError, message string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   oauthError,
			"message": message,
		})
	}))
}

// createCapturingBroker creates a mock broker that captures request details
func createCapturingBroker(t *testing.T, token string, captureFunc func(*http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if captureFunc != nil {
			captureFunc(r)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"token": token})
	}))
}
