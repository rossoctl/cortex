package bootstrap

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/auth"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/listener/reverseproxy"
	"github.com/rossoctl/cortex/core/observe"
	"github.com/rossoctl/cortex/core/pipeline"
)

// TestInitLogging verifies InitLogging maps LOG_LEVEL to the process level and
// defaults to INFO for unset/unknown values.
func TestInitLogging(t *testing.T) {
	cases := []struct {
		env  string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug}, // case-insensitive
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"info", slog.LevelInfo},
		{"", slog.LevelInfo},         // unset -> default
		{"nonsense", slog.LevelInfo}, // unknown -> default
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", tc.env)
			InitLogging("test-binary")
			if got := LogLevel(); got != tc.want {
				t.Fatalf("LOG_LEVEL=%q: LogLevel()=%v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

func TestNewHTTPServer_AppliesItsOptions(t *testing.T) {
	type key struct{}
	srv := newHTTPServer("127.0.0.1:0", http.NotFoundHandler(), []ServerOption{
		WithConnContext(func(ctx context.Context, _ net.Conn) context.Context { return context.WithValue(ctx, key{}, true) }),
	})
	if srv.ConnContext == nil {
		t.Fatal("WithConnContext did not set ConnContext")
	}
	if v := srv.ConnContext(context.Background(), nil).Value(key{}); v != true {
		t.Errorf("ConnContext is not the one given: value %v", v)
	}
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %s, want the default kept", srv.ReadHeaderTimeout)
	}
}

func TestStartHTTPServer_PassesItsOptionsOn(t *testing.T) {
	srv, err := StartHTTPServer("test", http.NotFoundHandler(), "127.0.0.1:0",
		WithConnContext(func(ctx context.Context, _ net.Conn) context.Context { return ctx }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	if srv.ConnContext == nil {
		t.Error("StartHTTPServer dropped its options: ConnContext is nil")
	}
}

// The Serve* functions are the Start* ones minus the bind, for a caller that binds every
// port before it opens anything on disk (cmd/cortex does, so a second instance gives up
// before touching the first one's files). Each must serve exactly the listener it is handed.

func boundListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestServeHTTPServer_ServesTheListenerItIsHanded(t *testing.T) {
	ln := boundListener(t)
	srv := ServeHTTPServer("test", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}), ln, WithConnContext(func(ctx context.Context, _ net.Conn) context.Context { return ctx }))
	defer func() { _ = srv.Close() }()
	if got := getStatus(t, "http://"+ln.Addr().String()+"/"); got != http.StatusTeapot {
		t.Errorf("status = %d, want the handler's %d", got, http.StatusTeapot)
	}
	if srv.ConnContext == nil {
		t.Error("ServeHTTPServer dropped its options: ConnContext is nil")
	}
}

func TestServeHealthServer_ServesTheListenerItIsHanded(t *testing.T) {
	p, err := pipeline.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	h := pipeline.NewHolder(p)
	ln := boundListener(t)
	srv := ServeHealthServer(h, h, ln)
	defer func() { _ = srv.Close() }()
	if got := getStatus(t, "http://"+ln.Addr().String()+"/healthz"); got != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", got)
	}
}

func TestServeReverseProxyServer_ServesTheListenerItIsHanded(t *testing.T) {
	backend := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})}
	bln := boundListener(t)
	go func() { _ = backend.Serve(bln) }()
	defer func() { _ = backend.Close() }()

	p, err := pipeline.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	rp, err := reverseproxy.NewServer(pipeline.NewHolder(p), nil, "http://"+bln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ln := boundListener(t)
	srv := ServeReverseProxyServer("test", rp, ln)
	defer func() { _ = srv.Close() }()
	if got := getStatus(t, "http://"+ln.Addr().String()+"/"); got != http.StatusTeapot {
		t.Errorf("status = %d, want the backend's %d", got, http.StatusTeapot)
	}
}

// cmd/cortex hands ServeStatServer the TLS bridge's report as an extra option, which must
// reach the server alongside the two it always registers.
func TestServeStatServer_PassesItsOptionsOn(t *testing.T) {
	teapot := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	ln := boundListener(t)
	srv := ServeStatServer(func() *config.Config { return &config.Config{} }, func() *auth.Stats { return auth.NewStats() },
		teapot, teapot, ln, observe.WithTLSBridgeUnread(teapot))
	defer func() { _ = srv.Close() }()
	base := "http://" + ln.Addr().String()
	for _, path := range []string{"/tls-bridge/unread", "/reload/status", "/pricing/table"} {
		if got := getStatus(t, base+path); got != http.StatusTeapot {
			t.Errorf("%s = %d, want the handler's %d", path, got, http.StatusTeapot)
		}
	}
}
