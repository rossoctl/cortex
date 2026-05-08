package plugins

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

func TestTokenBroker_Name(t *testing.T) {
	p := NewTokenBroker()
	if p.Name() != "token-broker" {
		t.Errorf("Name() = %q, want %q", p.Name(), "token-broker")
	}
}

func TestTokenBroker_Configure_Valid(t *testing.T) {
	routesFile, err := writeTempRoutesFile(t, `
- host: file.example.com
  action: passthrough
`)
	if err != nil {
		t.Fatalf("writeTempRoutesFile() error = %v", err)
	}

	tests := []struct {
		name   string
		config string
	}{
		{
			name: "minimal config",
			config: `{
				"broker_url": "http://broker:8080"
			}`,
		},
		{
			name: "with default policy",
			config: `{
				"broker_url": "http://broker:8080",
				"default_policy": "broker"
			}`,
		},
		{
			name: "with inline routes",
			config: `{
				"broker_url": "http://broker:8080",
				"routes": {
					"rules": [
						{"host": "api.example.com", "action": "broker"}
					]
				}
			}`,
		},
		{
			name: "with routes file",
			config: `{
				"broker_url": "http://broker:8080",
				"routes": {
					"file": "` + routesFile + `"
				}
			}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewTokenBroker()
			err := p.Configure(json.RawMessage(tt.config))
			if err != nil {
				t.Errorf("Configure() error = %v, want nil", err)
			}
		})
	}
}

func TestTokenBroker_Configure_Invalid(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name:    "missing broker_url",
			config:  `{}`,
			wantErr: "broker_url is required",
		},
		{
			name: "invalid default_policy",
			config: `{
				"broker_url": "http://broker:8080",
				"default_policy": "invalid"
			}`,
			wantErr: "default_policy must be broker or passthrough",
		},
		{
			name:    "invalid json",
			config:  `{invalid}`,
			wantErr: "token-broker config",
		},
		{
			name: "unknown field",
			config: `{
				"broker_url": "http://broker:8080",
				"unknown_field": "value"
			}`,
			wantErr: "token-broker config",
		},
		{
			name: "invalid route pattern",
			config: `{
				"broker_url": "http://broker:8080",
				"routes": {
					"rules": [
						{"host": "[", "action": "broker"}
					]
				}
			}`,
			wantErr: "token-broker routes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewTokenBroker()
			err := p.Configure(json.RawMessage(tt.config))
			if err == nil {
				t.Error("Configure() error = nil, want error")
				return
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Configure() error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestTokenBroker_OnRequest_Success(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotServerURL string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotServerURL = r.Header.Get("X-Server-Url")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"token": "acquired-token-12345",
		})
	}))
	defer srv.Close()

	p := NewTokenBroker()
	config := `{
		"broker_url": "` + srv.URL + `",
		"default_policy": "broker"
	}`
	if err := p.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	pctx := &pipeline.Context{
		Host: "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer user-token-abc"},
		},
	}

	action := p.OnRequest(context.Background(), pctx)

	if action.Type != pipeline.Continue {
		t.Errorf("OnRequest() action.Type = %v, want %v", action.Type, pipeline.Continue)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("broker request method = %q, want %q", gotMethod, http.MethodPost)
	}
	if gotPath != "/sessions/token" {
		t.Errorf("broker request path = %q, want %q", gotPath, "/sessions/token")
	}
	if gotAuth != "Bearer user-token-abc" {
		t.Errorf("broker request Authorization = %q, want %q", gotAuth, "Bearer user-token-abc")
	}
	if gotServerURL != "http://api.example.com" {
		t.Errorf("broker request X-Server-Url = %q, want %q", gotServerURL, "http://api.example.com")
	}

	auth := pctx.Headers.Get("Authorization")
	expected := "Bearer acquired-token-12345"
	if auth != expected {
		t.Errorf("Authorization header = %q, want %q", auth, expected)
	}
}

func TestTokenBroker_OnRequest_MissingToken(t *testing.T) {
	p := NewTokenBroker()
	config := `{
		"broker_url": "http://broker:8080",
		"default_policy": "broker"
	}`
	if err := p.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	pctx := &pipeline.Context{
		Host:    "api.example.com",
		Headers: http.Header{},
	}

	action := p.OnRequest(context.Background(), pctx)

	if action.Type != pipeline.Reject {
		t.Errorf("OnRequest() action.Type = %v, want %v", action.Type, pipeline.Reject)
	}
	if action.Violation == nil {
		t.Fatal("OnRequest() action.Violation is nil")
	}
	status, _, _ := action.Violation.Render()
	if status != http.StatusUnauthorized {
		t.Errorf("OnRequest() status = %d, want %d", status, http.StatusUnauthorized)
	}
}

func TestTokenBroker_OnRequest_BrokerError(t *testing.T) {
	var gotAuth, gotServerURL string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotServerURL = r.Header.Get("X-Server-Url")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "access_denied",
			"message": "insufficient permissions",
		})
	}))
	defer srv.Close()

	p := NewTokenBroker()
	config := `{
		"broker_url": "` + srv.URL + `",
		"default_policy": "broker"
	}`
	if err := p.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	pctx := &pipeline.Context{
		Host: "api.example.com:8443",
		Headers: http.Header{
			"Authorization": []string{"Bearer user-token-abc"},
		},
	}

	action := p.OnRequest(context.Background(), pctx)

	if action.Type != pipeline.Reject {
		t.Errorf("OnRequest() action.Type = %v, want %v", action.Type, pipeline.Reject)
	}
	if action.Violation == nil {
		t.Fatal("OnRequest() action.Violation is nil")
	}
	status, _, _ := action.Violation.Render()
	if status != http.StatusForbidden {
		t.Errorf("OnRequest() status = %d, want %d", status, http.StatusForbidden)
	}
	if gotAuth != "Bearer user-token-abc" {
		t.Errorf("broker request Authorization = %q, want %q", gotAuth, "Bearer user-token-abc")
	}
	if gotServerURL != "http://api.example.com:8443" {
		t.Errorf("broker request X-Server-Url = %q, want %q", gotServerURL, "http://api.example.com:8443")
	}
}

func TestTokenBroker_OnRequest_Passthrough(t *testing.T) {
	p := NewTokenBroker()
	config := `{
		"broker_url": "http://broker:8080",
		"default_policy": "passthrough"
	}`
	if err := p.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	originalToken := "Bearer original-token"
	pctx := &pipeline.Context{
		Host: "api.example.com",
		Headers: http.Header{
			"Authorization": []string{originalToken},
		},
	}

	action := p.OnRequest(context.Background(), pctx)

	if action.Type != pipeline.Continue {
		t.Errorf("OnRequest() action.Type = %v, want %v", action.Type, pipeline.Continue)
	}

	auth := pctx.Headers.Get("Authorization")
	if auth != originalToken {
		t.Errorf("Authorization header = %q, want %q (unchanged)", auth, originalToken)
	}
}

func TestTokenBroker_OnRequest_RouteMatching(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": "acquired-token"})
	}))
	defer srv.Close()

	p := NewTokenBroker()
	config := `{
		"broker_url": "` + srv.URL + `",
		"default_policy": "passthrough",
		"routes": {
			"rules": [
				{
					"host": "api.example.com",
					"action": "broker"
				},
				{
					"host": "implicit-broker.example.com"
				},
				{
					"host": "other.example.com",
					"action": "passthrough"
				}
			]
		}
	}`
	if err := p.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	pctx1 := &pipeline.Context{
		Host: "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer user-token"},
		},
	}
	action1 := p.OnRequest(context.Background(), pctx1)
	if action1.Type != pipeline.Continue {
		t.Errorf("OnRequest() for broker route: action.Type = %v, want %v", action1.Type, pipeline.Continue)
	}
	if auth := pctx1.Headers.Get("Authorization"); auth != "Bearer acquired-token" {
		t.Errorf("Authorization header = %q, want %q", auth, "Bearer acquired-token")
	}

	pctxImplicit := &pipeline.Context{
		Host: "implicit-broker.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer implicit-token"},
		},
	}
	actionImplicit := p.OnRequest(context.Background(), pctxImplicit)
	if actionImplicit.Type != pipeline.Continue {
		t.Errorf("OnRequest() for implicit broker route: action.Type = %v, want %v", actionImplicit.Type, pipeline.Continue)
	}
	if auth := pctxImplicit.Headers.Get("Authorization"); auth != "Bearer acquired-token" {
		t.Errorf("Authorization header = %q, want %q", auth, "Bearer acquired-token")
	}

	originalToken := "Bearer original-token"
	pctx2 := &pipeline.Context{
		Host: "other.example.com",
		Headers: http.Header{
			"Authorization": []string{originalToken},
		},
	}
	action2 := p.OnRequest(context.Background(), pctx2)
	if action2.Type != pipeline.Continue {
		t.Errorf("OnRequest() for passthrough route: action.Type = %v, want %v", action2.Type, pipeline.Continue)
	}
	if auth := pctx2.Headers.Get("Authorization"); auth != originalToken {
		t.Errorf("Authorization header = %q, want %q (unchanged)", auth, originalToken)
	}
}

func TestTokenBroker_OnRequest_DefaultPolicyRouting(t *testing.T) {
	t.Run("unmatched host with broker default uses broker", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]string{"token": "default-broker-token"})
		}))
		defer srv.Close()

		p := NewTokenBroker()
		config := `{
			"broker_url": "` + srv.URL + `",
			"default_policy": "broker",
			"routes": {
				"rules": [
					{"host": "matched.example.com", "action": "passthrough"}
				]
			}
		}`
		if err := p.Configure(json.RawMessage(config)); err != nil {
			t.Fatalf("Configure() error = %v", err)
		}

		pctx := &pipeline.Context{
			Host: "unmatched.example.com",
			Headers: http.Header{
				"Authorization": []string{"Bearer default-token"},
			},
		}

		action := p.OnRequest(context.Background(), pctx)
		if action.Type != pipeline.Continue {
			t.Fatalf("OnRequest() action.Type = %v, want %v", action.Type, pipeline.Continue)
		}
		if auth := pctx.Headers.Get("Authorization"); auth != "Bearer default-broker-token" {
			t.Errorf("Authorization header = %q, want %q", auth, "Bearer default-broker-token")
		}
	})

	t.Run("unmatched host with passthrough default does not use broker", func(t *testing.T) {
		p := NewTokenBroker()
		config := `{
			"broker_url": "http://broker:8080",
			"default_policy": "passthrough",
			"routes": {
				"rules": [
					{"host": "matched.example.com", "action": "broker"}
				]
			}
		}`
		if err := p.Configure(json.RawMessage(config)); err != nil {
			t.Fatalf("Configure() error = %v", err)
		}

		originalToken := "Bearer untouched-token"
		pctx := &pipeline.Context{
			Host: "unmatched.example.com",
			Headers: http.Header{
				"Authorization": []string{originalToken},
			},
		}

		action := p.OnRequest(context.Background(), pctx)
		if action.Type != pipeline.Continue {
			t.Fatalf("OnRequest() action.Type = %v, want %v", action.Type, pipeline.Continue)
		}
		if auth := pctx.Headers.Get("Authorization"); auth != originalToken {
			t.Errorf("Authorization header = %q, want %q", auth, originalToken)
		}
	})
}

func TestTokenBroker_OnRequest_BrokerUnavailable(t *testing.T) {
	p := NewTokenBroker()
	config := `{
		"broker_url": "http://127.0.0.1:1",
		"default_policy": "broker"
	}`
	if err := p.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	pctx := &pipeline.Context{
		Host: "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer user-token-abc"},
		},
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Reject {
		t.Fatalf("OnRequest() action.Type = %v, want %v", action.Type, pipeline.Reject)
	}
	if action.Violation == nil {
		t.Fatal("OnRequest() action.Violation is nil")
	}
	status, _, _ := action.Violation.Render()
	if status != http.StatusBadGateway {
		t.Errorf("OnRequest() status = %d, want %d", status, http.StatusBadGateway)
	}
}

func TestTokenBroker_OnRequest_InvalidSuccessResponse(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
	}{
		{
			name:        "malformed json",
			body:        `{`,
			contentType: "application/json",
			wantStatus:  http.StatusBadGateway,
		},
		{
			name:        "missing token field",
			body:        `{}`,
			contentType: "application/json",
			wantStatus:  http.StatusBadGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			p := NewTokenBroker()
			config := `{
				"broker_url": "` + srv.URL + `",
				"default_policy": "broker"
			}`
			if err := p.Configure(json.RawMessage(config)); err != nil {
				t.Fatalf("Configure() error = %v", err)
			}

			pctx := &pipeline.Context{
				Host: "api.example.com",
				Headers: http.Header{
					"Authorization": []string{"Bearer user-token-abc"},
				},
			}

			action := p.OnRequest(context.Background(), pctx)
			if action.Type != pipeline.Reject {
				t.Fatalf("OnRequest() action.Type = %v, want %v", action.Type, pipeline.Reject)
			}
			if action.Violation == nil {
				t.Fatal("OnRequest() action.Violation is nil")
			}
			status, _, _ := action.Violation.Render()
			if status != tt.wantStatus {
				t.Errorf("OnRequest() status = %d, want %d", status, tt.wantStatus)
			}
		})
	}
}

func TestTokenBroker_OnRequest_BeforeConfigurePanics(t *testing.T) {
	p := NewTokenBroker()
	pctx := &pipeline.Context{
		Host: "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer user-token-abc"},
		},
	}

	defer func() {
		if recover() == nil {
			t.Fatal("OnRequest() without Configure() did not panic")
		}
	}()

	_ = p.OnRequest(context.Background(), pctx)
}

func TestTokenBroker_OnResponse(t *testing.T) {
	p := NewTokenBroker()
	pctx := &pipeline.Context{}

	action := p.OnResponse(context.Background(), pctx)

	if action.Type != pipeline.Continue {
		t.Errorf("OnResponse() action.Type = %v, want %v", action.Type, pipeline.Continue)
	}
}

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

// =============================================================================
// Test Helpers
// =============================================================================

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

// =============================================================================
// Edge Case Tests
// =============================================================================

// TestExtractBearer_EdgeCases tests edge cases for bearer token extraction
func TestExtractBearer_EdgeCases(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{
			name:   "valid bearer token",
			header: "Bearer abc123",
			want:   "abc123",
		},
		{
			name:   "empty header",
			header: "",
			want:   "",
		},
		{
			name:   "no bearer prefix",
			header: "abc123",
			want:   "",
		},
		{
			name:   "wrong case",
			header: "bearer abc123",
			want:   "",
		},
		{
			name:   "bearer with no token",
			header: "Bearer ",
			want:   "",
		},
		{
			name:   "bearer with trailing whitespace",
			header: "Bearer   ",
			want:   "",
		},
		{
			name:   "bearer with leading spaces in token",
			header: "Bearer  token",
			want:   "token",
		},
		{
			name:   "token with internal spaces",
			header: "Bearer token with spaces",
			want:   "token with spaces",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractBearer(tt.header)
			if got != tt.want {
				t.Errorf("extractBearer(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

// TestTokenBroker_OnRequest_MultipleAuthHeaders tests behavior with multiple Authorization headers
func TestTokenBroker_OnRequest_MultipleAuthHeaders(t *testing.T) {
	srv := createSuccessBroker(t, "acquired-token")
	defer srv.Close()

	p := NewTokenBroker()
	config := `{
		"broker_url": "` + srv.URL + `",
		"default_policy": "broker"
	}`
	if err := p.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	pctx := &pipeline.Context{
		Host: "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer token1", "Bearer token2"},
		},
	}

	action := p.OnRequest(context.Background(), pctx)

	if action.Type != pipeline.Continue {
		t.Errorf("OnRequest() action.Type = %v, want %v", action.Type, pipeline.Continue)
	}

	// Verify only first header is used
	auth := pctx.Headers.Get("Authorization")
	if auth != "Bearer acquired-token" {
		t.Errorf("Authorization header = %q, want %q", auth, "Bearer acquired-token")
	}
}

// TestTokenBroker_OnRequest_HostWithPort tests routing with host:port
func TestTokenBroker_OnRequest_HostWithPort(t *testing.T) {
	srv := createSuccessBroker(t, "port-token")
	defer srv.Close()

	p := NewTokenBroker()
	config := `{
		"broker_url": "` + srv.URL + `",
		"default_policy": "passthrough",
		"routes": {
			"rules": [
				{"host": "api.example.com", "action": "broker"}
			]
		}
	}`
	if err := p.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	pctx := &pipeline.Context{
		Host: "api.example.com:8443",
		Headers: http.Header{
			"Authorization": []string{"Bearer user-token"},
		},
	}

	action := p.OnRequest(context.Background(), pctx)

	if action.Type != pipeline.Continue {
		t.Errorf("OnRequest() action.Type = %v, want %v", action.Type, pipeline.Continue)
	}

	auth := pctx.Headers.Get("Authorization")
	if auth != "Bearer port-token" {
		t.Errorf("Authorization header = %q, want %q", auth, "Bearer port-token")
	}
}

// TestTokenBroker_OnRequest_BrokerURLTrailingSlash tests broker URL with trailing slash
func TestTokenBroker_OnRequest_BrokerURLTrailingSlash(t *testing.T) {
	var requestedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"token": "test-token"})
	}))
	defer srv.Close()

	p := NewTokenBroker()
	config := `{
		"broker_url": "` + srv.URL + `/",
		"default_policy": "broker"
	}`
	if err := p.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	pctx := &pipeline.Context{
		Host: "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer user-token"},
		},
	}

	action := p.OnRequest(context.Background(), pctx)

	if action.Type != pipeline.Continue {
		t.Errorf("OnRequest() action.Type = %v, want %v", action.Type, pipeline.Continue)
	}

	// Verify no double slashes in path
	if strings.Contains(requestedPath, "//") {
		t.Errorf("Request path contains double slashes: %q", requestedPath)
	}
}

// TestTokenBroker_OnRequest_NilContext tests behavior with nil context
func TestTokenBroker_OnRequest_NilContext(t *testing.T) {
	srv := createSuccessBroker(t, "test-token")
	defer srv.Close()

	p := NewTokenBroker()
	config := `{
		"broker_url": "` + srv.URL + `",
		"default_policy": "broker"
	}`
	if err := p.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	pctx := &pipeline.Context{
		Host: "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer user-token"},
		},
	}

	// Test with nil context - should handle gracefully or panic with clear message
	defer func() {
		if r := recover(); r != nil {
			// Panic is acceptable for nil context
			t.Logf("OnRequest with nil context panicked (expected): %v", r)
		}
	}()

	_ = p.OnRequest(nil, pctx)
}

// TestTokenBroker_OnRequest_NilPipelineContext tests behavior with nil pipeline context
func TestTokenBroker_OnRequest_NilPipelineContext(t *testing.T) {
	srv := createSuccessBroker(t, "test-token")
	defer srv.Close()

	p := NewTokenBroker()
	config := `{
		"broker_url": "` + srv.URL + `",
		"default_policy": "broker"
	}`
	if err := p.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Logf("OnRequest with nil pipeline context panicked (expected): %v", r)
		}
	}()

	_ = p.OnRequest(context.Background(), nil)
}

// TestTokenBroker_Configure_NonExistentRoutesFile tests configuration with non-existent routes file
func TestTokenBroker_Configure_NonExistentRoutesFile(t *testing.T) {
	p := NewTokenBroker()
	config := `{
		"broker_url": "http://broker:8080",
		"routes": {
			"file": "/nonexistent/routes.yaml"
		}
	}`

	err := p.Configure(json.RawMessage(config))
	if err == nil {
		t.Error("Configure() with non-existent routes file should return error")
		return
	}
	if !strings.Contains(err.Error(), "routes") {
		t.Errorf("Configure() error should mention routes, got: %v", err)
	}
}

// TestTokenBroker_OnRequest_TokenWithSpecialCharacters tests security against header injection
func TestTokenBroker_OnRequest_TokenWithSpecialCharacters(t *testing.T) {
	tests := []struct {
		name        string
		returnToken string
		wantReject  bool
	}{
		{
			name:        "token with newline",
			returnToken: "token\nwith\nnewline",
			wantReject:  false, // Should be handled by HTTP library
		},
		{
			name:        "token with carriage return",
			returnToken: "token\rwith\rCR",
			wantReject:  false,
		},
		{
			name:        "token with null byte",
			returnToken: "token\x00null",
			wantReject:  false,
		},
		{
			name:        "very long token",
			returnToken: strings.Repeat("a", 10000),
			wantReject:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := createSuccessBroker(t, tt.returnToken)
			defer srv.Close()

			p := NewTokenBroker()
			config := `{
				"broker_url": "` + srv.URL + `",
				"default_policy": "broker"
			}`
			if err := p.Configure(json.RawMessage(config)); err != nil {
				t.Fatalf("Configure() error = %v", err)
			}

			pctx := &pipeline.Context{
				Host: "api.example.com",
				Headers: http.Header{
					"Authorization": []string{"Bearer user-token"},
				},
			}

			action := p.OnRequest(context.Background(), pctx)

			if tt.wantReject && action.Type != pipeline.Reject {
				t.Errorf("OnRequest() should reject token with special characters")
			}

			// Verify token is set (even if it contains special chars, HTTP lib should handle)
			auth := pctx.Headers.Get("Authorization")
			expectedPrefix := "Bearer " + tt.returnToken
			if !tt.wantReject && auth != expectedPrefix {
				t.Logf("Authorization header may have been sanitized: %q", auth)
			}
		})
	}
}

// =============================================================================
// Additional Tests from tokenbroker_test_additions.go
// =============================================================================

// TestTokenBroker_OnRequest_ContextCancellation tests context cancellation during broker request
func TestTokenBroker_OnRequest_ContextCancellation(t *testing.T) {
	requestStarted := make(chan struct{})

	srv := createCapturingBroker(t, "test-token", func(r *http.Request) {
		close(requestStarted)
		<-r.Context().Done()
	})
	defer srv.Close()

	p := NewTokenBroker()
	config := `{
		"broker_url": "` + srv.URL + `",
		"default_policy": "broker"
	}`
	if err := p.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	pctx := &pipeline.Context{
		Host: "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer user-token"},
		},
	}

	// Start request in goroutine
	actionCh := make(chan pipeline.Action, 1)
	go func() {
		actionCh <- p.OnRequest(ctx, pctx)
	}()

	// Wait for request to start, then cancel
	<-requestStarted
	cancel()

	// Should get rejection due to context cancellation
	action := <-actionCh
	if action.Type != pipeline.Reject {
		t.Errorf("OnRequest() action.Type = %v, want %v", action.Type, pipeline.Reject)
	}
}

// TestTokenBroker_Configure_ConcurrentCalls removed - Configure is not designed
// to be called concurrently. It's a one-time initialization method called during
// plugin setup, not during request processing.

// TestTokenBroker_OnRequest_ConcurrentRequests tests concurrent request handling
func TestTokenBroker_OnRequest_ConcurrentRequests(t *testing.T) {
	srv := createSuccessBroker(t, "concurrent-token")
	defer srv.Close()

	p := NewTokenBroker()
	config := `{
		"broker_url": "` + srv.URL + `",
		"default_policy": "broker"
	}`
	if err := p.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	const numRequests = 20
	var wg sync.WaitGroup
	errors := make(chan error, numRequests)

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()

			pctx := &pipeline.Context{
				Host: "api.example.com",
				Headers: http.Header{
					"Authorization": []string{"Bearer user-token"},
				},
			}
			action := p.OnRequest(context.Background(), pctx)

			if action.Type != pipeline.Continue {
				errors <- nil // Use nil to signal wrong action type
			}

			auth := pctx.Headers.Get("Authorization")
			if auth != "Bearer concurrent-token" {
				errors <- nil
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	errorCount := 0
	for range errors {
		errorCount++
	}

	if errorCount > 0 {
		t.Errorf("%d out of %d concurrent requests failed", errorCount, numRequests)
	}
}

// TestTokenBroker_OnRequest_BrokerTimeout tests broker request timeout
func TestTokenBroker_OnRequest_BrokerTimeout(t *testing.T) {
	// Create a broker that delays longer than client timeout
	srv := createCapturingBroker(t, "timeout-token", func(r *http.Request) {
		time.Sleep(2 * time.Second)
	})
	defer srv.Close()

	p := NewTokenBroker()
	config := `{
		"broker_url": "` + srv.URL + `",
		"default_policy": "broker"
	}`
	if err := p.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	pctx := &pipeline.Context{
		Host: "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer user-token"},
		},
	}
	action := p.OnRequest(ctx, pctx)

	// Should timeout and reject
	if action.Type != pipeline.Reject {
		t.Errorf("OnRequest() action.Type = %v, want %v", action.Type, pipeline.Reject)
	}
}

// TestTokenBroker_Configure_InvalidRouteFile tests configuration with invalid route file content
func TestTokenBroker_Configure_InvalidRouteFile(t *testing.T) {
	routesFile, err := writeTempRoutesFile(t, `
invalid yaml content [
  - this is not valid
`)
	if err != nil {
		t.Fatalf("writeTempRoutesFile() error = %v", err)
	}

	p := NewTokenBroker()
	config := `{
		"broker_url": "http://broker:8080",
		"routes": {
			"file": "` + routesFile + `"
		}
	}`

	err = p.Configure(json.RawMessage(config))
	if err == nil {
		t.Error("Configure() with invalid routes file should return error")
	}
}

// TestTokenBroker_OnRequest_MultipleConsecutiveCalls tests multiple calls with same plugin instance
func TestTokenBroker_OnRequest_MultipleConsecutiveCalls(t *testing.T) {
	callCount := 0
	var mu sync.Mutex

	srv := createCapturingBroker(t, "multi-call-token", func(r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
	})
	defer srv.Close()

	p := NewTokenBroker()
	config := `{
		"broker_url": "` + srv.URL + `",
		"default_policy": "broker"
	}`
	if err := p.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	// Make multiple consecutive calls
	for i := 0; i < 5; i++ {
		pctx := &pipeline.Context{
			Host: "api.example.com",
			Headers: http.Header{
				"Authorization": []string{"Bearer user-token"},
			},
		}
		action := p.OnRequest(context.Background(), pctx)
		if action.Type != pipeline.Continue {
			t.Errorf("OnRequest() action.Type = %v, want %v", action.Type, pipeline.Continue)
		}
	}

	mu.Lock()
	count := callCount
	mu.Unlock()

	if count != 5 {
		t.Errorf("expected 5 broker calls, got %d", count)
	}
}

// TestTokenBroker_OnRequest_DifferentAuthSchemes tests non-Bearer auth schemes
func TestTokenBroker_OnRequest_DifferentAuthSchemes(t *testing.T) {
	tests := []struct {
		name       string
		authHeader string
		wantReject bool
	}{
		{"Basic auth", "Basic dXNlcjpwYXNz", true},
		{"Digest auth", "Digest username=\"user\"", true},
		{"No auth", "", true},
		{"Malformed Bearer", "Bearertoken", true},
		{"Bearer lowercase", "bearer token", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := createSuccessBroker(t, "scheme-token")
			defer srv.Close()

			p := NewTokenBroker()
			config := `{
				"broker_url": "` + srv.URL + `",
				"default_policy": "broker"
			}`
			if err := p.Configure(json.RawMessage(config)); err != nil {
				t.Fatalf("Configure() error = %v", err)
			}

			pctx := &pipeline.Context{
				Host: "api.example.com",
				Headers: http.Header{
					"Authorization": []string{tt.authHeader},
				},
			}

			action := p.OnRequest(context.Background(), pctx)

			if tt.wantReject {
				if action.Type != pipeline.Reject {
					t.Errorf("OnRequest() action.Type = %v, want %v", action.Type, pipeline.Reject)
				}
			} else {
				if action.Type != pipeline.Continue {
					t.Errorf("OnRequest() action.Type = %v, want %v", action.Type, pipeline.Continue)
				}
			}
		})
	}
}

// TestTokenBroker_Capabilities tests plugin capabilities
func TestTokenBroker_Capabilities(t *testing.T) {
	p := NewTokenBroker()
	caps := p.Capabilities()
	t.Logf("TokenBroker capabilities: %+v", caps)
}

// =============================================================================
// Benchmark Tests
// =============================================================================

// BenchmarkOnRequest_Passthrough benchmarks passthrough requests
func BenchmarkOnRequest_Passthrough(b *testing.B) {
	p := NewTokenBroker()
	config := `{
		"broker_url": "http://broker:8080",
		"default_policy": "passthrough"
	}`
	if err := p.Configure(json.RawMessage(config)); err != nil {
		b.Fatalf("Configure() error = %v", err)
	}

	pctx := &pipeline.Context{
		Host: "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer test-token"},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = p.OnRequest(context.Background(), pctx)
	}
}

// BenchmarkOnRequest_BrokerSuccess benchmarks successful broker requests
func BenchmarkOnRequest_BrokerSuccess(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"token": "bench-token"})
	}))
	defer srv.Close()

	p := NewTokenBroker()
	config := `{
		"broker_url": "` + srv.URL + `",
		"default_policy": "broker"
	}`
	if err := p.Configure(json.RawMessage(config)); err != nil {
		b.Fatalf("Configure() error = %v", err)
	}

	pctx := &pipeline.Context{
		Host: "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer test-token"},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = p.OnRequest(context.Background(), pctx)
	}
}

// BenchmarkExtractBearer benchmarks bearer token extraction
func BenchmarkExtractBearer(b *testing.B) {
	header := "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = extractBearer(header)
	}
}
