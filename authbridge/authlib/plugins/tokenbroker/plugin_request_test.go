package tokenbroker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// =============================================================================
// OnRequest Handler Tests
// =============================================================================

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

func TestTokenBroker_OnRequest_WithAuthorizationEndpoint(t *testing.T) {
	var capturedAuthEndpoint string

	srv := createCapturingBroker(t, "test-token", func(r *http.Request) {
		capturedAuthEndpoint = r.Header.Get("X-Authorization-Endpoint")
	})
	defer srv.Close()

	p := NewTokenBroker()
	config := `{
		"broker_url": "` + srv.URL + `",
		"routes": {
			"rules": [
				{
					"host": "api.example.com",
					"action": "broker",
					"authorization_endpoint": "https://auth.example.com/oauth/authorize"
				},
				{
					"host": "other.example.com",
					"action": "broker"
				}
			]
		}
	}`

	if err := p.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	// Test with authorization endpoint
	pctx1 := &pipeline.Context{
		Host:    "api.example.com",
		Headers: http.Header{"Authorization": []string{"Bearer user-token"}},
	}

	action1 := p.OnRequest(context.Background(), pctx1)
	if action1.Type != pipeline.Continue {
		t.Errorf("OnRequest() action.Type = %v, want %v", action1.Type, pipeline.Continue)
	}

	if capturedAuthEndpoint != "https://auth.example.com/oauth/authorize" {
		t.Errorf("X-Authorization-Endpoint = %q, want %q", capturedAuthEndpoint, "https://auth.example.com/oauth/authorize")
	}

	// Test without authorization endpoint
	capturedAuthEndpoint = "should-be-empty"
	pctx2 := &pipeline.Context{
		Host:    "other.example.com",
		Headers: http.Header{"Authorization": []string{"Bearer user-token"}},
	}

	action2 := p.OnRequest(context.Background(), pctx2)
	if action2.Type != pipeline.Continue {
		t.Errorf("OnRequest() action.Type = %v, want %v", action2.Type, pipeline.Continue)
	}

	if capturedAuthEndpoint != "" {
		t.Errorf("X-Authorization-Endpoint = %q, want empty string", capturedAuthEndpoint)
	}
}

func TestTokenBroker_OnRequest_WithTokenEndpoint(t *testing.T) {
	var capturedTokenEndpoint string

	srv := createCapturingBroker(t, "test-token", func(r *http.Request) {
		capturedTokenEndpoint = r.Header.Get("X-Token-Endpoint")
	})
	defer srv.Close()

	p := NewTokenBroker()
	config := `{
		"broker_url": "` + srv.URL + `",
		"routes": {
			"rules": [
				{
					"host": "api.example.com",
					"action": "broker",
					"token_endpoint": "https://auth.example.com/oauth/token"
				},
				{
					"host": "other.example.com",
					"action": "broker"
				}
			]
		}
	}`

	if err := p.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	// Test with token endpoint
	pctx1 := &pipeline.Context{
		Host:    "api.example.com",
		Headers: http.Header{"Authorization": []string{"Bearer user-token"}},
	}

	action1 := p.OnRequest(context.Background(), pctx1)
	if action1.Type != pipeline.Continue {
		t.Errorf("OnRequest() action.Type = %v, want %v", action1.Type, pipeline.Continue)
	}

	if capturedTokenEndpoint != "https://auth.example.com/oauth/token" {
		t.Errorf("X-Token-Endpoint = %q, want %q", capturedTokenEndpoint, "https://auth.example.com/oauth/token")
	}

	// Test without token endpoint
	capturedTokenEndpoint = "should-be-empty"
	pctx2 := &pipeline.Context{
		Host:    "other.example.com",
		Headers: http.Header{"Authorization": []string{"Bearer user-token"}},
	}

	action2 := p.OnRequest(context.Background(), pctx2)
	if action2.Type != pipeline.Continue {
		t.Errorf("OnRequest() action.Type = %v, want %v", action2.Type, pipeline.Continue)
	}

	if capturedTokenEndpoint != "" {
		t.Errorf("X-Token-Endpoint = %q, want empty string", capturedTokenEndpoint)
	}
}

func TestTokenBroker_OnRequest_WithBothEndpoints(t *testing.T) {
	var capturedAuthEndpoint, capturedTokenEndpoint string

	srv := createCapturingBroker(t, "test-token", func(r *http.Request) {
		capturedAuthEndpoint = r.Header.Get("X-Authorization-Endpoint")
		capturedTokenEndpoint = r.Header.Get("X-Token-Endpoint")
	})
	defer srv.Close()

	p := NewTokenBroker()
	config := `{
		"broker_url": "` + srv.URL + `",
		"routes": {
			"rules": [
				{
					"host": "api.example.com",
					"action": "broker",
					"authorization_endpoint": "https://auth.example.com/oauth/authorize",
					"token_endpoint": "https://auth.example.com/oauth/token"
				}
			]
		}
	}`

	if err := p.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	// Test with both endpoints
	pctx := &pipeline.Context{
		Host:    "api.example.com",
		Headers: http.Header{"Authorization": []string{"Bearer user-token"}},
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Errorf("OnRequest() action.Type = %v, want %v", action.Type, pipeline.Continue)
	}

	if capturedAuthEndpoint != "https://auth.example.com/oauth/authorize" {
		t.Errorf("X-Authorization-Endpoint = %q, want %q", capturedAuthEndpoint, "https://auth.example.com/oauth/authorize")
	}

	if capturedTokenEndpoint != "https://auth.example.com/oauth/token" {
		t.Errorf("X-Token-Endpoint = %q, want %q", capturedTokenEndpoint, "https://auth.example.com/oauth/token")
	}
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

// TestTokenBroker_ServerURL_SchemeAware verifies the serverURL sent
// to the broker (via X-Server-Url) uses pctx.Scheme when populated.
// Defaults to "http" when pctx.Scheme is empty — matches the
// previous hardcoded behavior so existing callers upgrade without
// surprise. See issue #397.
func TestTokenBroker_ServerURL_SchemeAware(t *testing.T) {
	tests := []struct {
		name   string
		scheme string
		want   string
	}{
		{name: "scheme_https", scheme: "https", want: "https://api.example.com"},
		{name: "scheme_http_explicit", scheme: "http", want: "http://api.example.com"},
		{name: "scheme_empty_defaults_http", scheme: "", want: "http://api.example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotServerURL string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotServerURL = r.Header.Get("X-Server-Url")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]string{"token": "x"})
			}))
			defer srv.Close()

			p := NewTokenBroker()
			cfg := `{"broker_url": "` + srv.URL + `", "default_policy": "broker"}`
			if err := p.Configure(json.RawMessage(cfg)); err != nil {
				t.Fatalf("Configure: %v", err)
			}

			pctx := &pipeline.Context{
				Scheme: tc.scheme,
				Host:   "api.example.com",
				Headers: http.Header{
					"Authorization": []string{"Bearer user-token"},
				},
			}
			if action := p.OnRequest(context.Background(), pctx); action.Type != pipeline.Continue {
				t.Fatalf("OnRequest action = %v, want Continue", action.Type)
			}
			if gotServerURL != tc.want {
				t.Errorf("X-Server-Url = %q, want %q", gotServerURL, tc.want)
			}
		})
	}
}
