package reverseproxy

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/rossoctl/cortex/core/auth"
	"github.com/rossoctl/cortex/core/listener/transparentproxy"
	"github.com/rossoctl/cortex/core/plugins/jwtvalidation/validation"
)

// observed records what a test backend saw, under a mutex.
//
// The values are written in the server goroutine and read after
// http.DefaultClient.Do returns. That ordering holds in practice, but it rests on
// net/http internals rather than a synchronization edge the race detector is
// guaranteed to observe — so without this the tests are a latent -race flake
// rather than a genuine data race today.
type observed struct {
	mu   sync.Mutex
	host string
	xff  string
}

func (o *observed) record(r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.host = r.Host
	o.xff = r.Header.Get("X-Forwarded-For")
}

func (o *observed) get() (host, xff string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.host, o.xff
}

// withOrigDst stands in for http.Server.ConnContext + the transparent inbound
// listener: it injects a recovered original destination into the request
// context, which is the only channel the Director reads it from.
func withOrigDst(dst string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(transparentproxy.ContextWithOrigDst(r.Context(), dst)))
	})
}

func allowAllAuth() *auth.Auth {
	return auth.New(auth.Config{
		Verifier: &mockVerifier{claims: &validation.Claims{Subject: "user"}},
		Identity: auth.IdentityConfig{Audiences: []string{"my-app"}},
	})
}

// portOfURL extracts the port an httptest server bound, so a test can build a
// realistic "podIP:appPort" destination whose PORT resolves to that server.
func portOfURL(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("splitting %q: %v", u.Host, err)
	}
	return port
}

// TestTransparentInbound_ForwardsToRecoveredPort is the core behavior: the
// forwarding target comes from the destination the client addressed, not from
// config. The recovered destination names a pod IP that does not exist in the
// test environment — proving the Director rewrote the host to loopback while
// keeping the PORT, which is exactly the on-cluster behavior (the egress guard
// RETURNs loopback, so the hop can't be re-captured).
func TestTransparentInbound_ForwardsToRecoveredPort(t *testing.T) {
	var seen observed
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("app-ok"))
	}))
	defer app.Close()

	srv, err := NewTransparentServer(inboundPipelineFromAuth(t, allowAllAuth()), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// 10.244.0.5 is a plausible pod IP and is NOT where the app listens; only
	// the port is carried over.
	dst := net.JoinHostPort("10.244.0.5", portOfURL(t, app.URL))
	proxy := httptest.NewServer(withOrigDst(dst, srv.Handler()))
	defer proxy.Close()

	req, _ := http.NewRequest("GET", proxy.URL+"/api/data", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (Director should have rewritten to loopback:%s)",
			resp.StatusCode, portOfURL(t, app.URL))
	}
	gotHost, gotXFF := seen.get()
	wantHost := net.JoinHostPort("127.0.0.1", portOfURL(t, app.URL))
	if gotHost != wantHost {
		t.Errorf("app saw Host = %q, want %q (loopback, recovered port)", gotHost, wantHost)
	}
	// REDIRECT preserves the client's source IP, so the app must still be able
	// to see it after the loopback hop.
	if gotXFF == "" {
		t.Error("X-Forwarded-For must reach the app so the real client IP survives the loopback hop")
	}
}

// TestTransparentInbound_NoDestinationFailsClosed locks the fail-closed
// contract: a request the listener cannot attribute to a captured destination
// must be rejected, not forwarded to a guessed target. Without this, the parked
// sentinel backend (or worse, a real one) would receive unvalidated traffic.
func TestTransparentInbound_NoDestinationFailsClosed(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("app must not be reached when no destination was recovered")
		w.WriteHeader(http.StatusOK)
	}))
	defer app.Close()

	srv, err := NewTransparentServer(inboundPipelineFromAuth(t, allowAllAuth()), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// No withOrigDst wrapper: the context carries nothing.
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	req, _ := http.NewRequest("GET", proxy.URL+"/api/data", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (fail closed with no recovered destination)", resp.StatusCode)
	}
}

// TestTransparentInbound_StillValidatesJWT guards against the per-connection
// backend path accidentally bypassing the inbound pipeline — the entire reason
// this listener exists is that validation cannot be sidestepped.
func TestTransparentInbound_StillValidatesJWT(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("app must not be reached by an unauthenticated request")
		w.WriteHeader(http.StatusOK)
	}))
	defer app.Close()

	a := auth.New(auth.Config{
		Verifier: &mockVerifier{err: fmt.Errorf("invalid token")},
		Identity: auth.IdentityConfig{Audiences: []string{"my-app"}},
	})
	srv, err := NewTransparentServer(inboundPipelineFromAuth(t, a), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	dst := net.JoinHostPort("10.244.0.5", portOfURL(t, app.URL))
	proxy := httptest.NewServer(withOrigDst(dst, srv.Handler()))
	defer proxy.Close()

	req, _ := http.NewRequest("GET", proxy.URL+"/api/data", nil)
	req.Header.Set("Authorization", "Bearer bogus-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("status = 200, want a denial — transparent inbound must still validate")
	}
}

// TestTransparentInbound_NoFallbackCanDisarmFailClosed replaces an earlier test
// that asserted a configured fallback backend kept serving requests with no
// recovered destination. That was the footgun: the fail-closed 502 could be
// disarmed by an argument that read like it only added a backend. The constructor
// no longer accepts one, so the boundary cannot be weakened from the call site —
// this test pins that by construction.
func TestTransparentInbound_NoFallbackCanDisarmFailClosed(t *testing.T) {
	srv, err := NewTransparentServer(inboundPipelineFromAuth(t, allowAllAuth()), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !srv.transparentInbound {
		t.Fatal("a transparent server must always fail closed on an unattributable request")
	}
	if srv.backend != "http://"+unresolvableBackend {
		t.Errorf("backend = %q, want the undialable sentinel so a leak fails loudly", srv.backend)
	}
}

// TestWrapListener_NoMTLSIsPassthrough documents the split-out wrap: with mTLS
// off it must return the listener untouched, so the transparent path's
// *net.TCPListener (needed for SO_ORIGINAL_DST) is not replaced by a wrapper
// that would hide it from the unwrap walk.
func TestWrapListener_NoMTLSIsPassthrough(t *testing.T) {
	srv, err := NewTransparentServer(inboundPipelineFromAuth(t, allowAllAuth()), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	if got := srv.WrapListener(ln); got != ln {
		t.Errorf("WrapListener with mTLS off = %T, want the same listener back", got)
	}
}

// TestFixedBackendIgnoresRecoveredDestination locks the gate on the Director's
// rewrite. The closure is installed by NewServer, so without an explicit flag it
// is live on every fixed-backend server too — safe only while nothing populates
// the context key without an InboundListener, which nothing enforces. A stray
// key must not silently redirect a port-stealing deployment's traffic.
func TestFixedBackendIgnoresRecoveredDestination(t *testing.T) {
	var seen observed
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer app.Close()

	// A fixed-backend server, exactly as the reverse-proxy mechanism builds it.
	srv, err := NewServer(inboundPipelineFromAuth(t, allowAllAuth()), nil, app.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if srv.transparentInbound {
		t.Fatal("NewServer must not enable the transparent rewrite")
	}

	// Inject a destination naming a port the app is NOT on. If the rewrite were
	// ungated, the request would be sent to loopback:9 and never arrive.
	proxy := httptest.NewServer(withOrigDst("10.244.0.5:9", srv.Handler()))
	defer proxy.Close()

	req, _ := http.NewRequest("GET", proxy.URL+"/api/data", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: a fixed-backend server must ignore the recovered destination", resp.StatusCode)
	}
	reached, _ := seen.get()
	if want := portOfURL(t, app.URL); reached == "" || reached == net.JoinHostPort("127.0.0.1", "9") {
		t.Errorf("request did not reach the configured backend (app Host=%q, want the :%s backend)", reached, want)
	}
}
