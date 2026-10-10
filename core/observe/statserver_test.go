package observe

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/auth"
	"github.com/rossoctl/cortex/core/config"
)

func newTestConfig() *config.Config {
	return &config.Config{
		Mode: "proxy-sidecar",
		Listener: config.ListenerConfig{
			ForwardProxyAddr:    ":15123",
			ReverseProxyAddr:    ":15124",
			ReverseProxyBackend: "http://localhost:8080",
		},
		Stats: config.StatsConfig{
			StatsAddress: ":9093",
		},
	}
}

func serveMux(cfg *config.Config, stats *auth.Stats) http.Handler {
	// Provider closes over a fixed Stats so the tests can control
	// what /stats returns. Production callers pass a provider that
	// merges per-plugin Stats at request time; see main.go's
	// statsProvider closure.
	s := NewStatServer(":0", func() *config.Config { return cfg }, func() *auth.Stats { return stats })
	return s.server.Handler
}

func TestRootHandler(t *testing.T) {
	handler := serveMux(newTestConfig(), auth.NewStats())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}
	html := string(body)
	if !strings.Contains(html, "/config") {
		t.Error("root page missing /config link")
	}
	if !strings.Contains(html, "/stats") {
		t.Error("root page missing /stats link")
	}
}

// The /config endpoint rendering today emits the runtime config
// verbatim. Per-plugin config subtrees are opaque json.RawMessage
// blobs; operators are expected to keep secrets out of the runtime
// YAML (reference file paths like client_secret_file instead). A
// future redaction pass can walk known-sensitive plugin fields, but
// is not in this PR's scope.
func TestConfigEndpointShape(t *testing.T) {
	cfg := newTestConfig()
	handler := serveMux(cfg, auth.NewStats())

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /config status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got config.Config
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode config response: %v", err)
	}
	if got.Mode != cfg.Mode {
		t.Errorf("Mode = %q, want %q", got.Mode, cfg.Mode)
	}
	if got.Stats.StatsAddress != cfg.Stats.StatsAddress {
		t.Errorf("Stats.StatsAddress = %q, want %q", got.Stats.StatsAddress, cfg.Stats.StatsAddress)
	}
}

func TestStatsEndpointEmptyStats(t *testing.T) {
	stats := auth.NewStats()
	handler := serveMux(newTestConfig(), stats)

	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /stats status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got map[string]map[string]int
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, key := range []string{"inbound_approvals", "inbound_denials", "outbound_approvals", "outbound_denials"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing key %q in stats response", key)
		}
	}
}

func TestStatsEndpointValidJSON(t *testing.T) {
	stats := auth.NewStats()
	handler := serveMux(newTestConfig(), stats)

	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	body, _ := io.ReadAll(w.Result().Body)
	if !json.Valid(body) {
		t.Fatalf("stats endpoint returned invalid JSON: %s", body)
	}
}

func TestNewStatServerSetsAddr(t *testing.T) {
	s := NewStatServer(":9093", func() *config.Config { return newTestConfig() }, func() *auth.Stats { return auth.NewStats() })
	if s.server.Addr != ":9093" {
		t.Errorf("server.Addr = %q, want :9093", s.server.Addr)
	}
}

func TestNewStatServerCustomAddr(t *testing.T) {
	s := NewStatServer("127.0.0.1:8888", func() *config.Config { return newTestConfig() }, func() *auth.Stats { return auth.NewStats() })
	if s.server.Addr != "127.0.0.1:8888" {
		t.Errorf("server.Addr = %q, want 127.0.0.1:8888", s.server.Addr)
	}
}

// Close is the stop a local install uses: it releases the port at once rather than
// waiting on requests in flight, so the proxy replacing this one can bind it.
func TestStatServerClose_ReleasesItsPort(t *testing.T) {
	s := NewStatServer(":0", func() *config.Config { return newTestConfig() }, func() *auth.Stats { return auth.NewStats() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	served := make(chan error, 1)
	go func() { served <- s.Serve(ln) }()

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := <-served; err != http.ErrServerClosed {
		t.Errorf("Serve returned %v, want http.ErrServerClosed", err)
	}
	again, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port still held after Close: %v", err)
	}
	_ = again.Close()
}

func TestStatServer_TLSBridgeUnread(t *testing.T) {
	served := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"programs":[],"hosts":[]}`)) })
	s := NewStatServer(":0", func() *config.Config { return newTestConfig() }, func() *auth.Stats { return auth.NewStats() },
		WithTLSBridgeUnread(served))
	w := httptest.NewRecorder()
	s.server.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tls-bridge/unread", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"programs"`) {
		t.Errorf("GET /tls-bridge/unread = %d %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.server.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(w.Body.String(), "/tls-bridge/unread") {
		t.Error("the index does not link /tls-bridge/unread")
	}

	// Without the option there is no such endpoint: the index page must not answer for it.
	bare := serveMux(newTestConfig(), auth.NewStats())
	w = httptest.NewRecorder()
	bare.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tls-bridge/unread", nil))
	if strings.Contains(w.Body.String(), `"programs"`) {
		t.Error("a server built without the option served the report")
	}
}
