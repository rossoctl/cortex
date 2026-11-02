package observe

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

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

func serveMuxOpts(cfg *config.Config, stats *auth.Stats, opts ...Option) http.Handler {
	s := NewStatServer(":0", func() *config.Config { return cfg }, func() *auth.Stats { return stats }, opts...)
	return s.server.Handler
}

// WithPprof mounts Go's net/http/pprof handlers. The profiles expose the
// process's internals, so it must be possible to build the server without
// them, which is the default.
func TestStatServerPprofOffByDefault(t *testing.T) {
	handler := serveMux(newTestConfig(), auth.NewStats())
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET /debug/pprof/heap without WithPprof served %q, want the HTML index", ct)
	}
	if strings.Contains(string(body), "/debug/pprof/") {
		t.Error("index page links /debug/pprof/ without WithPprof")
	}
}

func TestStatServerPprofServesProfiles(t *testing.T) {
	handler := serveMuxOpts(newTestConfig(), auth.NewStats(), WithPprof(true))
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	req.Host = "localhost:47802" // a loopback Host, as a local client sends
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /debug/pprof/heap status = %d, want 200", resp.StatusCode)
	}
	// A gzip stream is what pprof serves; the HTML index or an error page is not.
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Error("GET /debug/pprof/heap did not return a gzip profile stream")
	}
}

// /debug/pprof/block and /mutex answer 200 but stay empty forever unless the
// runtime is told to record contention — the handlers exist either way. WithPprof
// turns the recorders on with the handlers, so the endpoints can show something.
func TestStatServerPprofEnablesContentionProfiles(t *testing.T) {
	handler := serveMuxOpts(newTestConfig(), auth.NewStats(), WithPprof(true))

	// Mutex has a getter; block does not, so block is proven by recording one
	// event: a channel receive that waits is a block, and at rate 0 it would
	// never reach the profile.
	ch := make(chan struct{})
	go func() { time.Sleep(100 * time.Millisecond); close(ch) }()
	<-ch

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/block?debug=1", nil)
	req.Host = "localhost:47802" // a loopback Host, as a local client sends
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	// The blocked receive's stack names this test; an empty profile carries only
	// the header lines.
	if !strings.Contains(w.Body.String(), "TestStatServerPprofEnablesContentionProfiles") {
		t.Error("a 100ms blocked receive is absent from /debug/pprof/block: the recorder is off")
	}
	if prev := runtime.SetMutexProfileFraction(1); prev == 0 {
		t.Error("WithPprof left mutex profiling off: /debug/pprof/mutex would stay empty")
	}
}

// A page can rebind a DNS name to 127.0.0.1 and have a browser read a loopback
// port, so the profiles refuse any Host that is not exactly loopback — the same
// defense as the session API's erase guards.
func TestStatServerPprofRefusesReboundHost(t *testing.T) {
	handler := serveMuxOpts(newTestConfig(), auth.NewStats(), WithPprof(true))
	for _, host := range []string{"evil.example:47802", "127.0.0.1.evil.example", "[::1].evil.example"} {
		req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
		req.Host = host
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Result().StatusCode != http.StatusForbidden {
			t.Errorf("GET /debug/pprof/heap with Host %q = %d, want 403", host, w.Result().StatusCode)
		}
	}
	// The loopback names themselves still pass.
	for _, host := range []string{"localhost:47802", "127.0.0.1:47802", "[::1]:47802"} {
		req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
		req.Host = host
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Result().StatusCode != http.StatusOK {
			t.Errorf("GET /debug/pprof/heap with Host %q = %d, want 200", host, w.Result().StatusCode)
		}
	}
}

// The Host check alone does not stop a browser: a simple no-cors GET from a
// page carries a loopback Host and no preflight. Browsers always send Origin,
// so its presence is the refusal — the erase guards' third check.
func TestStatServerPprofRefusesBrowserOrigin(t *testing.T) {
	handler := serveMuxOpts(newTestConfig(), auth.NewStats(), WithPprof(true))
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	req.Host = "127.0.0.1:47802"
	req.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusForbidden {
		t.Errorf("GET /debug/pprof/heap with Origin = %d, want 403", w.Result().StatusCode)
	}
}
