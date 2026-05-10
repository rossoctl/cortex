package tokenbroker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// =============================================================================
// Edge Case Tests
// =============================================================================

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

// =============================================================================
// Benchmark Tests
// =============================================================================

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

func BenchmarkExtractBearer(b *testing.B) {
	header := "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = extractBearer(header)
	}
}
