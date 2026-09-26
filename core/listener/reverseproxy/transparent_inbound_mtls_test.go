package reverseproxy

// Assembled-stack coverage for transparent inbound with mTLS ON.
//
// Everything in transparent_inbound_test.go drives srv.Handler() behind an
// httptest server with a stand-in for the listener, and every constructor there
// passes nil MTLSOptions. That leaves the shape the operator actually deploys —
// bootstrap.StartTransparentInboundServer, which is
// WrapListener(NewInboundListener(tcpLn)) + http.Server{ConnContext} — with no
// Go coverage at all once mTLS is on. A regression there fails only rossoctl's
// e2e suite, in a different repo, which is a slow and easily-skipped signal.
//
// These tests bind a real TCP listener, wrap it with the real WrapListener and a
// real authtls.ServerConfig, serve the real handler over a real http.Server, and
// drive it with real TLS clients. One step of the production chain is
// necessarily substituted: SO_ORIGINAL_DST cannot be exercised in a unit test
// (on darwin it is a build-tagged error stub, and on a non-NATed Linux loopback
// connection getsockopt returns the socket's own address, which CheckDst
// correctly rejects as self-referential — so a real InboundListener drops every
// connection a test can make). The destination is therefore injected at the
// ConnContext seam, which is the exact channel InboundListener +
// ConnContextHook populate. The other half — that the destination survives the
// real tlssniff/tls.Conn wrapper chain — is covered in
// transparentproxy/inbound_mtls_chain_test.go, where the unexported Conn can be
// built with a chosen destination.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/auth"
	"github.com/rossoctl/cortex/core/listener/transparentproxy"
	"github.com/rossoctl/cortex/core/plugins/jwtvalidation/validation"
	authtls "github.com/rossoctl/cortex/core/tlsconfig"
)

// --- fixtures ----------------------------------------------------------------

// testSVIDSource is an in-memory spiffe.X509Source: one CA, one leaf carrying a
// SPIFFE ID as its URI SAN. It stands in for spiffe-helper's on-disk SVID so the
// real authtls.ServerConfig / authtls.ClientConfig can be used unmodified.
type testSVIDSource struct {
	cert *tls.Certificate
	pool *x509.CertPool
}

func (s *testSVIDSource) Certificate() (*tls.Certificate, error) { return s.cert, nil }
func (s *testSVIDSource) TrustBundle() (*x509.CertPool, error)   { return s.pool, nil }

// newTestSVIDSource mirrors the cert shape in core/tlsconfig/testhelpers_test.go
// (ECDSA P256, ClientAuth+ServerAuth EKU, SPIFFE ID as URI SAN) — test helpers
// can't cross a package boundary, so the generation is repeated rather than
// exported into the production tree.
func newTestSVIDSource(t *testing.T, spiffeID string) *testSVIDSource {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "inbound-mtls-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}

	uri, err := url.Parse(spiffeID)
	if err != nil {
		t.Fatalf("spiffe id: %v", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "inbound-mtls-test-leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		URIs:         []*url.URL{uri},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return &testSVIDSource{
		cert: &tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey},
		pool: pool,
	}
}

// countingVerifier admits every token and records how many times it was asked.
// It exists so a test can assert a request was rejected *before* the inbound
// pipeline ran, rather than settling for a status code that a later layer
// happens to produce too.
type countingVerifier struct {
	mu    sync.Mutex
	calls int
}

func (v *countingVerifier) Verify(_ context.Context, _ string, _ []string) (*validation.Claims, error) {
	v.mu.Lock()
	v.calls++
	v.mu.Unlock()
	return &validation.Claims{Subject: "user"}, nil
}

func (v *countingVerifier) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

// inboundHarness is the assembled listener under test plus the app behind it.
type inboundHarness struct {
	addr    string
	appPort string
	seen    *observed
	src     *testSVIDSource
	metrics *authtls.Metrics
}

// startMTLSInbound assembles exactly what StartTransparentInboundServer
// assembles: a transparent Server with mTLS on, its WrapListener over a real
// TCP listener, and an http.Server carrying the recovered destination into the
// request context.
//
// dst == "" installs the production ConnContextHook, so nothing is recovered —
// the state a connection that did not arrive via the REDIRECT lands in. A
// non-empty dst is injected at the same seam, standing in for the syscall.
func startMTLSInbound(t *testing.T, strict bool, a *auth.Auth, dstHost string, appHandler http.HandlerFunc) *inboundHarness {
	t.Helper()

	seen := &observed{}
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		appHandler(w, r)
	}))
	t.Cleanup(app.Close)
	appPort := portOfURL(t, app.URL)

	src := newTestSVIDSource(t, "spiffe://example.org/ns/team1/sa/agent")
	metrics := &authtls.Metrics{}
	srv, err := NewTransparentServer(inboundPipelineFromAuth(t, a), nil, &MTLSOptions{
		Source:  src,
		Strict:  strict,
		Metrics: metrics,
	})
	if err != nil {
		t.Fatalf("NewTransparentServer: %v", err)
	}

	// ListenTCP, matching what the production path binds: the transparent inbound
	// listener needs a *net.TCPListener to recover SO_ORIGINAL_DST before any
	// bytes are read.
	tcpLn, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln := srv.WrapListener(tcpLn)

	connCtx := transparentproxy.ConnContextHook
	if dstHost != "" {
		dst := net.JoinHostPort(dstHost, appPort)
		connCtx = func(ctx context.Context, _ net.Conn) context.Context {
			return transparentproxy.ContextWithOrigDst(ctx, dst)
		}
	}
	hs := &http.Server{
		Handler:           srv.Handler(),
		ConnContext:       connCtx,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = hs.Serve(ln) }()
	t.Cleanup(func() { _ = hs.Close() })

	return &inboundHarness{
		addr:    ln.Addr().String(),
		appPort: appPort,
		seen:    seen,
		src:     src,
		metrics: metrics,
	}
}

func okApp(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("app-ok"))
}

func unreachableApp(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		t.Error("app must not be reached")
		w.WriteHeader(http.StatusOK)
	}
}

// tlsClient dials the harness presenting a valid SVID, through the same
// authtls.ClientConfig the forward proxy uses.
func (h *inboundHarness) tlsClient(t *testing.T) *http.Client {
	t.Helper()
	cfg, err := authtls.ClientConfig(h.src)
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: cfg},
		Timeout:   10 * time.Second,
	}
}

// requireTLSTerminates completes one handshake with a valid SVID.
//
// Every rejection test below concludes something from a handshake that failed,
// and a listener serving plaintext — the exact regression
// TestWrapListener_MTLSOnWraps guards — fails those handshakes too, for the
// opposite reason. Proving the port terminates TLS first is what makes the
// later failure attributable to the client's own certificate.
func (h *inboundHarness) requireTLSTerminates(t *testing.T) {
	t.Helper()
	valid, err := authtls.ClientConfig(h.src)
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	c, err := tls.Dial("tcp", h.addr, valid)
	if err != nil {
		t.Fatalf("a valid SVID could not complete a handshake: %v", err)
	}
	_ = c.Close()
}

// expectPeerRejected asserts the server never answers a client dialing with cfg.
//
// Under TLS 1.3 the client finishes its side before the server evaluates the
// client certificate, so the rejection surfaces either at Dial or on the first
// read. Both are the same verdict, and which one happens is a timing detail.
func (h *inboundHarness) expectPeerRejected(t *testing.T, cfg *tls.Config, peer string) {
	t.Helper()
	c, err := tls.Dial("tcp", h.addr, cfg)
	if err != nil {
		// Rejected during the handshake — the strongest form of the same result.
		return
	}
	defer func() { _ = c.Close() }()
	if resp, _ := rawExchange(t, c); resp != "" {
		t.Errorf("server answered %s: %q", peer, resp)
	}
}

// rawExchange writes a minimal HTTP/1.1 request over an already-connected conn
// and returns whatever came back before the server closed.
//
// A raw dial rather than http.Client, because the rejection paths are what is
// being asserted: when a server closes before reading anything, net/http's
// transport treats the request as retryable and re-dials, so a deterministic
// "connection was refused at the TLS layer" becomes a retry sequence bounded
// only by a client timeout. Reading the socket directly removes that.
func rawExchange(t *testing.T, c net.Conn) (string, error) {
	t.Helper()
	if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return "", err
	}
	req := "GET /api/data HTTP/1.1\r\nHost: authbridge\r\nAuthorization: Bearer valid-token\r\n\r\n"
	if _, err := c.Write([]byte(req)); err != nil {
		return "", err
	}
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	return string(buf[:n]), err
}

// --- tests -------------------------------------------------------------------

// TestWrapListener_MTLSOnWraps is the missing half of
// TestWrapListener_NoMTLSIsPassthrough: with mTLS configured the listener MUST
// be replaced by the sniffing wrapper, or the transparent path would serve
// plaintext while reporting mtls=true at startup.
func TestWrapListener_MTLSOnWraps(t *testing.T) {
	src := newTestSVIDSource(t, "spiffe://example.org/ns/team1/sa/agent")
	srv, err := NewTransparentServer(inboundPipelineFromAuth(t, allowAllAuth()), nil,
		&MTLSOptions{Source: src})
	if err != nil {
		t.Fatal(err)
	}
	if !srv.MTLSEnabled() {
		t.Error("MTLSEnabled() = false with a source configured")
	}
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	if got := srv.WrapListener(ln); got == net.Listener(ln) {
		t.Error("WrapListener with mTLS on returned the bare listener — plaintext would be served on the mTLS port")
	}
}

// TestTransparentInboundMTLS_StrictRejectsPlaintext is the transport-level
// boundary: in strict mode a plaintext caller must be dropped before the HTTP
// server ever sees a request, and the rejection must be counted.
func TestTransparentInboundMTLS_StrictRejectsPlaintext(t *testing.T) {
	h := startMTLSInbound(t, true, allowAllAuth(), "10.244.0.5", unreachableApp(t))

	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	resp, err := rawExchange(t, c)
	if err == nil {
		t.Errorf("plaintext caller got a response in strict mode: %q", resp)
	}
	if resp != "" {
		t.Errorf("strict mode answered a plaintext caller with %q, want nothing", resp)
	}

	// The counter is the operator-visible signal that a caller was turned away;
	// a silent drop looks identical to a network fault from outside.
	deadline := time.Now().Add(3 * time.Second)
	for h.metrics.InboundPlainRejected.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.metrics.InboundPlainRejected.Load(); got != 1 {
		t.Errorf("InboundPlainRejected = %d, want 1", got)
	}
}

// TestTransparentInboundMTLS_StrictRejectsPeerWithoutCert covers the narrowest
// of the transport rejections: TLS alone is not enough, the peer must present a
// certificate at all. ServerConfig sets RequireAndVerifyClientCert, and this is
// the assertion that the transparent path inherits it.
func TestTransparentInboundMTLS_StrictRejectsPeerWithoutCert(t *testing.T) {
	h := startMTLSInbound(t, true, allowAllAuth(), "10.244.0.5", unreachableApp(t))
	h.requireTLSTerminates(t)

	// Server verification is deliberately skipped: this client's *own* missing
	// certificate is the subject of the test.
	//nolint:gosec // G402: test client asserts server-side client-cert enforcement
	h.expectPeerRejected(t, &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
	}, "a peer with no client certificate")
}

// TestTransparentInboundMTLS_StrictRejectsForeignTrustDomain is what actually
// makes the inbound port unusable to a workload outside the trust domain. Such a
// workload does not arrive with no certificate — it arrives with a perfectly
// valid one that its own CA signed.
//
// authtls.verifyPeerChain checks the chain against the trust bundle and nothing
// else: no SPIFFE-ID or trust-domain comparison, because the bundle *is* the
// policy (CLAUDE.md, "Trust model"). That single check is therefore
// the whole boundary, and until now nothing drove it through the assembled
// listener. newTestSVIDSource mints a fresh CA per call, so a second source is
// an independent trust domain by construction.
func TestTransparentInboundMTLS_StrictRejectsForeignTrustDomain(t *testing.T) {
	h := startMTLSInbound(t, true, allowAllAuth(), "10.244.0.5", unreachableApp(t))
	h.requireTLSTerminates(t)

	foreign := newTestSVIDSource(t, "spiffe://foreign.example/ns/other/sa/attacker")
	foreignCert, err := foreign.Certificate()
	if err != nil {
		t.Fatalf("foreign Certificate: %v", err)
	}

	// Hand-built rather than authtls.ClientConfig(foreign), and GetClientCertificate
	// rather than Certificates, because both defaults would make the *client* the
	// one that refuses:
	//   - ClientConfig verifies the server against the foreign bundle, which has
	//     never seen our CA, so the handshake would fail before the server ever
	//     evaluated the client certificate;
	//   - the Certificates path filters candidates against the CertificateRequest's
	//     acceptable-CA list (ServerConfig populates ClientCAs), so the client
	//     would send an empty certificate and this would collapse into the
	//     no-certificate test above. GetClientCertificate is returned as-is.
	//nolint:gosec // G402: test client asserts the server's verdict on a foreign chain
	cfg := &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return foreignCert, nil
		},
	}
	h.expectPeerRejected(t, cfg, "a peer holding a certificate from a foreign trust domain")
}

// TestTransparentInboundMTLS_ValidPeerForwardsToRecoveredPort is the whole
// feature in one assertion, with every layer real except the syscall: mTLS
// terminates, the inbound pipeline admits the request, and the Director forwards
// to the port the client originally addressed on loopback.
func TestTransparentInboundMTLS_ValidPeerForwardsToRecoveredPort(t *testing.T) {
	h := startMTLSInbound(t, true, allowAllAuth(), "10.244.0.5", okApp)

	req, err := http.NewRequest("GET", "https://"+h.addr+"/api/data", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer valid-token")
	resp, err := h.tlsClient(t).Do(req)
	if err != nil {
		t.Fatalf("mTLS request with a valid SVID failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	gotHost, gotXFF := h.seen.get()
	if want := net.JoinHostPort("127.0.0.1", h.appPort); gotHost != want {
		t.Errorf("app saw Host = %q, want %q (loopback, recovered port)", gotHost, want)
	}
	// REDIRECT preserves the client's source IP; it must still reach the app
	// after the loopback hop, and TLS termination must not have erased it. The
	// client dialed over IPv4 loopback and sent no XFF of its own, so the Director
	// must have appended exactly that address — asserting the value rather than
	// mere non-emptiness, because an XFF carrying the wrong address is a
	// different bug from an absent one and only one of the two survives a
	// non-empty check.
	if wantXFF := "127.0.0.1"; gotXFF != wantXFF {
		t.Errorf("app saw X-Forwarded-For = %q, want %q (client IP through TLS termination + the loopback hop)", gotXFF, wantXFF)
	}
	if got := h.metrics.InboundTLSAccepted.Load(); got != 1 {
		t.Errorf("InboundTLSAccepted = %d, want 1", got)
	}
}

// TestTransparentInboundMTLS_ValidPeerStillValidatesJWT separates the two
// authorizations that share this port. A valid SVID authenticates the
// *connection*; it must not authorize the *request*. Collapsing the two would
// let any workload in the trust domain call any path with no token.
func TestTransparentInboundMTLS_ValidPeerStillValidatesJWT(t *testing.T) {
	a := auth.New(auth.Config{
		Verifier: &mockVerifier{err: fmt.Errorf("invalid token")},
		Identity: auth.IdentityConfig{Audiences: []string{"my-app"}},
	})
	h := startMTLSInbound(t, true, a, "10.244.0.5", unreachableApp(t))

	req, err := http.NewRequest("GET", "https://"+h.addr+"/api/data", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer bogus-token")
	resp, err := h.tlsClient(t).Do(req)
	if err != nil {
		t.Fatalf("handshake should succeed for a valid SVID: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Exactly 401, not merely "not 200". Anything else on this path means the
	// request never reached the validator — a transport or forwarding failure
	// reads as a pass under a `!= 200` check while proving nothing about whether
	// the token was examined.
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: a valid peer certificate must not substitute for request validation", resp.StatusCode)
	}
}

// TestTransparentInboundMTLS_HandshakeIsNotAttribution pins the fail-closed
// boundary on the mTLS path specifically. The production ConnContextHook is
// installed here and nothing is recovered — the state of a connection that did
// not arrive via the REDIRECT. A completed mutual handshake and a valid token
// must still not produce a forward, because there is no destination to forward
// to and the parked backend is undialable on purpose.
//
// The pipeline-call count is the part that pins the guard itself. 502 alone is
// ambiguous: the undialable sentinel backend produces the same status a layer
// later, so a status-only assertion holds even with the guard deleted. Counting
// verifier calls asserts what the guard is actually for — an unattributable
// request never reaches the pipeline.
func TestTransparentInboundMTLS_HandshakeIsNotAttribution(t *testing.T) {
	verifier := &countingVerifier{}
	a := auth.New(auth.Config{
		Verifier: verifier,
		Identity: auth.IdentityConfig{Audiences: []string{"my-app"}},
	})
	h := startMTLSInbound(t, true, a, "", unreachableApp(t))

	req, err := http.NewRequest("GET", "https://"+h.addr+"/api/data", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer valid-token")
	resp, err := h.tlsClient(t).Do(req)
	if err != nil {
		t.Fatalf("handshake should succeed for a valid SVID: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (no recovered destination)", resp.StatusCode)
	}
	if got := verifier.count(); got != 0 {
		t.Errorf("inbound pipeline ran %d time(s) for an unattributable request — the guard must reject before validation, not lean on the undialable sentinel for the same status", got)
	}
}

// TestTransparentInboundMTLS_PermissiveServesBothOnOnePort covers the default
// posture during a rollout: one port, byte-peek detection, both callers served
// and both still validated. The kubelet's probes and the UI reach the sidecar as
// plaintext, so a permissive mode that quietly dropped them would take the pod
// out of service.
func TestTransparentInboundMTLS_PermissiveServesBothOnOnePort(t *testing.T) {
	h := startMTLSInbound(t, false, allowAllAuth(), "10.244.0.5", okApp)

	for _, tc := range []struct {
		name   string
		scheme string
		client func() *http.Client
	}{
		{"plaintext", "http", func() *http.Client { return &http.Client{Timeout: 10 * time.Second} }},
		{"mtls", "https", func() *http.Client { return h.tlsClient(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest("GET", tc.scheme+"://"+h.addr+"/api/data", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer valid-token")
			resp, err := tc.client().Do(req)
			if err != nil {
				t.Fatalf("%s request failed: %v", tc.name, err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading body: %v", err)
			}
			if got := string(body); got != "app-ok" {
				t.Errorf("body = %q, want app-ok — the request did not reach the app", got)
			}
		})
	}

	if got := h.metrics.InboundPlainRejected.Load(); got != 0 {
		t.Errorf("InboundPlainRejected = %d, want 0 in permissive mode", got)
	}
	if got := h.metrics.InboundPlainAccepted.Load(); got != 1 {
		t.Errorf("InboundPlainAccepted = %d, want 1", got)
	}
	if got := h.metrics.InboundTLSAccepted.Load(); got != 1 {
		t.Errorf("InboundTLSAccepted = %d, want 1", got)
	}
}

// TestTransparentInboundMTLS_ConstructorRequiresSource guards the one way this
// wiring can be half-configured: an MTLSOptions with no source must fail at
// construction rather than yield a server that silently reports mTLS off.
func TestTransparentInboundMTLS_ConstructorRequiresSource(t *testing.T) {
	if _, err := NewTransparentServer(inboundPipelineFromAuth(t, allowAllAuth()), nil,
		&MTLSOptions{Strict: true}); err == nil {
		t.Fatal("NewTransparentServer accepted MTLSOptions with a nil Source")
	}
}

// TestTransparentInboundMTLS_SentinelBackendSurvivesMTLS checks that turning
// mTLS on does not disturb the two invariants the fail-closed boundary rests on:
// the parked, undialable sentinel backend and the transparent flag. Both are set
// on either side of the mTLS block in NewServer.
func TestTransparentInboundMTLS_SentinelBackendSurvivesMTLS(t *testing.T) {
	src := newTestSVIDSource(t, "spiffe://example.org/ns/team1/sa/agent")
	srv, err := NewTransparentServer(inboundPipelineFromAuth(t, allowAllAuth()), nil,
		&MTLSOptions{Source: src, Strict: true})
	if err != nil {
		t.Fatalf("NewTransparentServer with mTLS: %v", err)
	}
	if srv.backend != "http://"+unresolvableBackend {
		t.Errorf("backend = %q, want the undialable sentinel", srv.backend)
	}
	if !srv.transparentInbound {
		t.Error("transparentInbound = false")
	}
}
