package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
		Timeout: 5 * time.Second, // Custom timeout
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
