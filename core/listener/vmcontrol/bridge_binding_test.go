package vmcontrol

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/listener/forwardproxy"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// newBridgedVMEgressStack is newVMEgressStack with a TLS bridge configured, so
// a guest's CONNECT to the origin is decrypted and its inner requests run
// through the forward proxy's own handler rather than being relayed opaquely.
// Returns the stack plus the origin's authority and the root pool a guest needs
// to trust the forged leaf.
func newBridgedVMEgressStack(t *testing.T) (*Registry, *captureSink, *session.Store, *sessionProbe, string, *x509.CertPool, *http.Header) {
	t.Helper()
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	probe := &sessionProbe{}
	p, err := pipeline.New([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatal(err)
	}

	var originMu sync.Mutex
	originSaw := http.Header{}
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originMu.Lock()
		for k, v := range r.Header {
			originSaw[k] = v
		}
		originMu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(origin.Close)
	authority := strings.TrimPrefix(origin.URL, "https://")

	originCAPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})
	src, err := tlsbridge.NewEphemeralSource()
	if err != nil {
		t.Fatalf("NewEphemeralSource: %v", err)
	}
	up, err := tlsbridge.NewUpstreamClient(originCAPEM, false)
	if err != nil {
		t.Fatalf("NewUpstreamClient: %v", err)
	}
	port, err := strconv.Atoi(portOfAuthority(authority))
	if err != nil {
		t.Fatalf("port of %q: %v", authority, err)
	}
	decision, err := tlsbridge.NewDecision(tlsbridge.DecisionOpts{Ports: map[int]bool{port: true}})
	if err != nil {
		t.Fatalf("NewDecision: %v", err)
	}
	engine := &tlsbridge.Engine{
		Decision: decision,
		Term:     tlsbridge.NewTerminator(tlsbridge.NewMinter(src, tlsbridge.MinterOpts{})),
		Skip:     tlsbridge.NewSkipSet(),
		Upstream: up,
		CAPEM:    src.CACertPEM(),
	}
	issuer, _ := src.Issuer()
	roots := x509.NewCertPool()
	roots.AddCert(issuer)
	roots.AddCert(origin.Certificate())

	fp := &forwardproxy.Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           http.DefaultClient,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader, session.SessionIDHeader},
		TrustedSessionID: SessionIDFromRequest,
		// The wiring under test, matching cmd/cortex/vmcontrol_unix.go.
		AdoptBridgedRequest: AdoptBridgedRequest,
		TLSBridge:           engine,
	}
	sink := &captureSink{}
	return NewRegistry(fp, sink), sink, store, probe, authority, roots, &originSaw
}

// bridgedGuestGet opens a CONNECT through a VM socket, completes TLS against
// the bridge's forged leaf, and sends one request inside the decrypted stream
// carrying the given headers. It returns the inner response.
func bridgedGuestGet(t *testing.T, vmAddr, authority string, roots *x509.CertPool, path string, headers map[string]string) *http.Response {
	t.Helper()
	raw := guest(t, vmAddr)
	resp := guestConnect(t, raw, authority)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT answered %d, want 200", resp.StatusCode)
	}

	tc := tls.Client(raw, &tls.Config{ServerName: hostOf(authority), RootCAs: roots})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("TLS handshake over the tunnel: %v", err)
	}
	t.Cleanup(func() { _ = tc.Close() })

	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\nHost: %s\r\n", path, authority)
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("Connection: close\r\n\r\n")
	if _, err := tc.Write([]byte(b.String())); err != nil {
		t.Fatalf("write inner request: %v", err)
	}
	inner, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatalf("read inner response: %v", err)
	}
	return inner
}

func hostOf(authority string) string {
	if h, _, err := net.SplitHostPort(authority); err == nil {
		return h
	}
	return authority
}

func portOfAuthority(authority string) string {
	if _, p, err := net.SplitHostPort(authority); err == nil {
		return p
	}
	return "443"
}

// A DECRYPTED request from a VM guest must be bound to its VM exactly as a
// plaintext one is. The bridged path parses its requests off the forged TLS
// connection inside a fresh http.Server (tlsbridge.ServeConn), so the request
// context the per-VM wrapper stamped on the CONNECT does not reach them on its
// own — and without the binding, hydrateSession falls through to header
// resolution and a guest's own X-Claude-Code-Session-Id picks the session.
//
// That is the whole point of the feature inverted: the descriptor is supposed
// to BE the identity. The plaintext case is covered by
// TestRegistryIdentityFromConnection; this is the same contract one layer
// down, where TestRegistryIdentityFromConnection's stack has no bridge and so
// could never have caught it (#1569 review).
func TestBridgedRequestKeepsTheVMBinding(t *testing.T) {
	reg, sink, store, probe, authority, roots, originSaw := newBridgedVMEgressStack(t)
	vmA := registerTCPVM(t, reg, "vm-a", "sid-a")

	resp := bridgedGuestGet(t, vmA, authority, roots, "/bridged", map[string]string{
		// Everything a guest might assert to escape its own session.
		session.ClaudeCodeSessionHeader: "forged-cc",
		session.SessionIDHeader:         "forged-x",
		"X-Moca-Session":                "sid-b",
		"X-Moca-Vm-Id":                  "vm-b",
		"Proxy-Authorization":           "Basic Zm9yZ2Vk",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("inner response %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// The pipeline must have heard the REGISTERED sid, from the connection.
	probe.mu.Lock()
	ids, sources, headers := probe.ids, probe.sources, probe.headers
	probe.mu.Unlock()
	if len(ids) == 0 {
		t.Fatal("no pipeline pass recorded for the decrypted request")
	}
	for i, id := range ids {
		if id != "sid-a" || sources[i] != pipeline.SessionSourceConnection || headers[i] != "" {
			t.Errorf("decrypted request %d: plugin saw session %q via %q/%q, want sid-a via connection",
				i, id, sources[i], headers[i])
		}
	}

	// Nothing the guest named may hold rows.
	for _, forged := range []string{"forged-cc", "forged-x", "sid-b", "default"} {
		if v := store.View(forged); v != nil && len(v.Events) > 0 {
			t.Errorf("forged session %q has %d rows — the guest moved its traffic", forged, len(v.Events))
		}
	}
	if v := store.View("sid-a"); v == nil || len(v.Events) == 0 {
		t.Error("sid-a has no rows; the decrypted request was filed elsewhere")
	}

	// The origin must not see the guest's identity assertions: the wrapper
	// strips them on the decrypted request too, not only on the CONNECT.
	for _, name := range []string{
		session.ClaudeCodeSessionHeader, session.SessionIDHeader,
		"X-Moca-Session", "X-Moca-Vm-Id", "Proxy-Authorization",
	} {
		if got := originSaw.Get(name); got != "" {
			t.Errorf("origin saw guest header %s: %q", name, got)
		}
	}

	// And the audit line names the VM that owns the socket.
	//
	// POLLED, not read once: emit runs in the server's handler goroutine after
	// inner.ServeHTTP returns, which is not ordered against the client having
	// its response — so reading the sink straight after resp.Body.Close() is a
	// race the client usually wins on a fast machine and loses on a loaded CI
	// runner (observed as "no audit event for vm-a" in CI while passing 20/20
	// locally). Same deadline loop as the tunnel-audit test above, for the same
	// reason (#1569 review).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(sink.forVM("vm-a")) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if events := sink.forVM("vm-a"); len(events) == 0 {
		t.Error("no audit event for vm-a")
	}
	for _, e := range sink.forVM("vm-b") {
		t.Errorf("audit event attributed to vm-b: %+v", e)
	}
}
