package client

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
	largeToken := strings.Repeat("x", 1024*1024) // 1MB token
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"token": largeToken})
	}))
	defer srv.Close()

	client := NewClient()
	token, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.example.com", "", "")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != largeToken {
		t.Errorf("token length = %d, want %d", len(token), len(largeToken))
	}
}

// =============================================================================
// URL and Encoding Tests
// =============================================================================

func TestClient_AcquireToken_TrailingSlashBrokerURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": "slash-token"})
	}))
	defer srv.Close()

	client := NewClient()
	token, err := client.AcquireToken(context.Background(), srv.URL+"/", "user-token", "https://api.example.com", "", "")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "slash-token" {
		t.Errorf("token = %q, want slash-token", token)
	}
}

func TestClient_AcquireToken_ServerURLEncoding(t *testing.T) {
	var capturedServerURL string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedServerURL = r.Header.Get("X-Server-Url")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": "encoded-token"})
	}))
	defer srv.Close()

	tests := []struct {
		name      string
		serverURL string
		want      string
	}{
		{"simple URL", "https://api.example.com", "https://api.example.com"},
		{"URL with path", "https://api.example.com/v1/resource", "https://api.example.com/v1/resource"},
		{"URL with query", "https://api.example.com?param=value", "https://api.example.com?param=value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient()
			_, err := client.AcquireToken(context.Background(), srv.URL, "user-token", tt.serverURL, "", "")

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if capturedServerURL != tt.want {
				t.Errorf("X-Server-Url = %q, want %q", capturedServerURL, tt.want)
			}
		})
	}
}

func TestClient_AcquireToken_EmptyParameters(t *testing.T) {
	tests := []struct {
		name               string
		brokerURL          string
		userToken          string
		serverURL          string
		authEndpoint       string
		tokenEndpoint      string
		expectError        bool
		errorShouldContain string
		useMockServer      bool
	}{
		{
			name:               "empty broker URL",
			brokerURL:          "",
			userToken:          "token",
			serverURL:          "https://api.example.com",
			expectError:        true,
			errorShouldContain: "cannot be empty",
			useMockServer:      false,
		},
		{
			name:          "empty user token",
			brokerURL:     "http://broker:8080",
			userToken:     "",
			serverURL:     "https://api.example.com",
			expectError:   false,
			useMockServer: true,
		},
		{
			name:          "empty server URL",
			brokerURL:     "http://broker:8080",
			userToken:     "token",
			serverURL:     "",
			expectError:   false,
			useMockServer: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var srv *httptest.Server
			if tt.useMockServer {
				helper := NewTestHelper(t)
				srv = helper.NewSuccessBroker("test-token")
				defer srv.Close()
			}

			brokerURL := tt.brokerURL
			if tt.useMockServer && strings.Contains(brokerURL, "broker:8080") {
				brokerURL = srv.URL
			}

			client := NewClient()
			_, err := client.AcquireToken(context.Background(), brokerURL, tt.userToken, tt.serverURL, tt.authEndpoint, tt.tokenEndpoint)

			if tt.expectError && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.expectError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tt.expectError && err != nil && tt.errorShouldContain != "" {
				if !strings.Contains(err.Error(), tt.errorShouldContain) {
					t.Errorf("error = %q, should contain %q", err.Error(), tt.errorShouldContain)
				}
			}
		})
	}
}

// =============================================================================
// Concurrency Tests
// =============================================================================

func TestClient_AcquireToken_ConcurrentRequests(t *testing.T) {
	helper := NewTestHelper(t)
	srv := helper.NewSuccessBroker("concurrent-token")
	defer srv.Close()

	client := NewClient()
	const numRequests = 50

	var wg sync.WaitGroup
	errors := make(chan error, numRequests)

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.example.com", "", "")
			if err != nil {
				errors <- err
			}
		}()
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Errorf("concurrent request failed: %v", err)
	}
}

func TestClient_AcquireToken_ConcurrentDifferentServers(t *testing.T) {
	helper := NewTestHelper(t)
	srv1 := helper.NewSuccessBroker("token1")
	srv2 := helper.NewSuccessBroker("token2")
	defer srv1.Close()
	defer srv2.Close()

	client := NewClient()
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			client.AcquireToken(context.Background(), srv1.URL, "user-token", "https://api1.example.com", "", "")
		}()
		go func() {
			defer wg.Done()
			client.AcquireToken(context.Background(), srv2.URL, "user-token", "https://api2.example.com", "", "")
		}()
	}

	wg.Wait()
}

func TestClient_AcquireToken_RaceConditions(t *testing.T) {
	helper := NewTestHelper(t)
	srv := helper.NewSuccessBroker("race-token")
	defer srv.Close()

	client := NewClient()
	var counter atomic.Int64

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.example.com", "", "")
			if err == nil {
				counter.Add(1)
			}
		}()
	}

	wg.Wait()

	if counter.Load() != 100 {
		t.Errorf("successful requests = %d, want 100", counter.Load())
	}
}

func TestClient_AcquireToken_ConnectionReuse(t *testing.T) {
	var requestCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": "reuse-token"})
	}))
	defer srv.Close()

	client := NewClient()

	// Make multiple requests
	for i := 0; i < 10; i++ {
		_, err := client.AcquireToken(context.Background(), srv.URL, "user-token", "https://api.example.com", "", "")
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
	}

	if requestCount.Load() != 10 {
		t.Errorf("request count = %d, want 10", requestCount.Load())
	}
}
