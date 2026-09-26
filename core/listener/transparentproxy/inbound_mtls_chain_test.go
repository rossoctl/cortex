package transparentproxy

// Chain coverage for the recovered destination under mTLS, against the real
// wrapper types.
//
// TestOrigDstFromConn_UnwrapsThroughWrappers proves the walk works against a
// hand-written double whose own comment says it "mimics tlssniff's peekedConn".
// That is the shape of test that stays green while the thing it stands for
// changes: if peekedConn stopped exposing NetConn, or tlssniff started handing
// http.Server a type that hides the transparent conn, the double would still
// unwrap fine and every real request would fail closed with 502.
//
// These tests drive the production chain instead — real internal/tlssniff, real
// crypto/tls, real http.Server + ConnContextHook, real TLS client, real
// authtls.ServerConfig — and assert the destination arrives in the handler's
// context. Both mTLS modes are covered, because the wrapper depth differs
// between them: tls.Conn -> peekedConn -> Conn under TLS, and peekedConn -> Conn
// for a plaintext caller in permissive mode.
//
// The one step that cannot be real is the SO_ORIGINAL_DST syscall: on darwin it
// is a build-tagged error stub, and on a non-NATed Linux loopback connection
// getsockopt returns the socket's own address, which CheckDst correctly rejects
// as self-referential — an InboundListener would drop every connection a unit
// test can make. capturedListener therefore performs the one step Accept()
// performs after the syscall: hand up a *Conn carrying the destination.

import (
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
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/listener/internal/tlssniff"
	authtls "github.com/rossoctl/cortex/core/tls"
)

const chainDst = "10.244.0.5:8000"

// --- fixtures ----------------------------------------------------------------

// chainSVID is an in-memory spiffe.X509Source, so the real authtls configs can
// be used rather than a hand-rolled *tls.Config that might diverge from them.
type chainSVID struct {
	cert *tls.Certificate
	pool *x509.CertPool
}

func (s *chainSVID) Certificate() (*tls.Certificate, error) { return s.cert, nil }
func (s *chainSVID) TrustBundle() (*x509.CertPool, error)   { return s.pool, nil }

func newChainSVID(t *testing.T) *chainSVID {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "inbound-chain-test-ca"},
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

	uri, err := url.Parse("spiffe://example.org/ns/team1/sa/agent")
	if err != nil {
		t.Fatalf("spiffe id: %v", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "inbound-chain-test-leaf"},
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
	return &chainSVID{
		cert: &tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey},
		pool: pool,
	}
}

// capturedListener yields connections in the state InboundListener.Accept()
// returns them in: a *Conn wrapping the raw TCP conn and carrying the recovered
// destination. It replaces only the syscall (see the file comment); every layer
// above it in these tests is the production type.
type capturedListener struct {
	*net.TCPListener
	dst string
}

func (l *capturedListener) Accept() (net.Conn, error) {
	c, err := l.AcceptTCP()
	if err != nil {
		return nil, err
	}
	return &Conn{TCPConn: c, dst: l.dst}, nil
}

// chainHandler reports what the handler actually saw, through the response body,
// so the test needs no cross-goroutine synchronization to read it.
func chainHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dst, ok := OrigDstFromContext(r.Context())
		body, _ := io.ReadAll(r.Body)
		_, _ = fmt.Fprintf(w, "%s %s dst=%s ok=%v tls=%v body=%s",
			r.Method, r.URL.Path, dst, ok, r.TLS != nil, body)
	})
}

// startChain assembles capturedListener -> real tlssniff -> real http.Server
// with the production ConnContextHook, and returns the bound address.
func startChain(t *testing.T, mode tlssniff.Mode, src *chainSVID) (addr string, plainRejected <-chan struct{}) {
	t.Helper()

	cfg, err := authtls.ServerConfig(src)
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	tcpLn, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	sniffed := tlssniff.New(&capturedListener{TCPListener: tcpLn, dst: chainDst}, cfg, mode)

	rejected := make(chan struct{}, 4)
	sniffed.SetOnPlainRejected(func(_ net.Conn) {
		select {
		case rejected <- struct{}{}:
		default:
		}
	})

	hs := &http.Server{
		Handler:           chainHandler(),
		ConnContext:       ConnContextHook,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = hs.Serve(sniffed) }()
	t.Cleanup(func() { _ = hs.Close() })

	return sniffed.Addr().String(), rejected
}

func post(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	resp, err := client.Post(url, "text/plain", strings.NewReader("ping"))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return string(body)
}

func tlsClient(t *testing.T, src *chainSVID) *http.Client {
	t.Helper()
	cfg, err := authtls.ClientConfig(src)
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: cfg},
		Timeout:   10 * time.Second,
	}
}

// --- tests -------------------------------------------------------------------

// TestInboundChain_OrigDstSurvivesTLS is the assertion the hand-written double
// cannot make: the destination recovered before any protocol byte was read still
// reaches the handler after the real tlssniff peek and a real TLS handshake have
// both wrapped the connection.
//
// Both modes are exercised because the mTLS posture is a per-deployment setting,
// and the two dispatch branches return different types.
func TestInboundChain_OrigDstSurvivesTLS(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode tlssniff.Mode
	}{
		{"permissive", tlssniff.ModePermissive},
		{"strict", tlssniff.ModeStrict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := newChainSVID(t)
			addr, _ := startChain(t, tc.mode, src)

			got := post(t, tlsClient(t, src), "https://"+addr+"/api/data")
			want := fmt.Sprintf("POST /api/data dst=%s ok=true tls=true body=ping", chainDst)
			if got != want {
				t.Errorf("handler saw %q, want %q", got, want)
			}
		})
	}
}

// TestInboundChain_OrigDstSurvivesPermissivePlaintext covers the shallower chain
// and, at the same time, the interference risk the two mechanisms create for each
// other: destination recovery happens on the raw connection, before tlssniff
// peeks the first byte. A response that parses at all is the proof the peeked
// byte was handed back — had it been consumed, the server would have read "OST
// /api/data" and answered nothing.
func TestInboundChain_OrigDstSurvivesPermissivePlaintext(t *testing.T) {
	src := newChainSVID(t)
	addr, rejected := startChain(t, tlssniff.ModePermissive, src)

	got := post(t, &http.Client{Timeout: 10 * time.Second}, "http://"+addr+"/api/data")
	want := fmt.Sprintf("POST /api/data dst=%s ok=true tls=false body=ping", chainDst)
	if got != want {
		t.Errorf("handler saw %q, want %q", got, want)
	}
	select {
	case <-rejected:
		t.Error("permissive mode reported a plaintext rejection")
	default:
	}
}

// TestInboundChain_StrictRejectionKeepsListenerServing pins the reason both
// Accept loops drop connections instead of returning an error. A plaintext probe
// against a strict listener — a kubelet TCP check, a port scan, a misconfigured
// caller — must not terminate http.Server.Serve and take the pod's inbound path
// down with it. The second request is the real assertion: the listener is still
// serving, and still recovers the destination.
func TestInboundChain_StrictRejectionKeepsListenerServing(t *testing.T) {
	src := newChainSVID(t)
	addr, rejected := startChain(t, tlssniff.ModeStrict, src)

	// Raw dial rather than http.Client: when a server closes before reading
	// anything, net/http treats the request as retryable and re-dials, which
	// would turn a single deterministic rejection into a retry sequence.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	if _, err := c.Write([]byte("GET /api/data HTTP/1.1\r\nHost: authbridge\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 256)
	n, err := c.Read(buf)
	_ = c.Close()
	if err == nil {
		t.Errorf("strict mode answered a plaintext caller: %q", buf[:n])
	}
	if n != 0 {
		t.Errorf("strict mode wrote %q to a plaintext caller, want nothing", buf[:n])
	}

	select {
	case <-rejected:
	case <-time.After(3 * time.Second):
		t.Error("plaintext rejection was not reported to the metrics callback")
	}

	// The listener must still be serving, with the chain intact.
	got := post(t, tlsClient(t, src), "https://"+addr+"/api/data")
	want := fmt.Sprintf("POST /api/data dst=%s ok=true tls=true body=ping", chainDst)
	if got != want {
		t.Errorf("after a rejection, handler saw %q, want %q", got, want)
	}
}

// TestInboundChain_PeekedConnExposesNetConn is the narrow regression guard for
// what the double in inbound_test.go stands in for: tlssniff's plaintext wrapper
// must expose the connection underneath it, or the unwrap walk terminates one
// layer too early and every permissive-mode plaintext request fails closed.
func TestInboundChain_PeekedConnExposesNetConn(t *testing.T) {
	src := newChainSVID(t)
	cfg, err := authtls.ServerConfig(src)
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	tcpLn, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = tcpLn.Close() }()

	sniffed := tlssniff.New(&capturedListener{TCPListener: tcpLn, dst: chainDst}, cfg, tlssniff.ModePermissive)

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := sniffed.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()

	client, err := net.Dial("tcp", tcpLn.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()
	// One plaintext byte is what tlssniff peeks to classify the connection.
	if _, err := client.Write([]byte("G")); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case c, ok := <-accepted:
		if !ok {
			t.Fatal("Accept failed")
		}
		defer func() { _ = c.Close() }()
		if _, isConn := c.(*Conn); isConn {
			t.Fatal("tlssniff returned the transparent conn unwrapped — this test would pass vacuously")
		}
		dst, found := OrigDstFromConn(c)
		if !found || dst != chainDst {
			t.Fatalf("OrigDstFromConn(%T) = (%q, %v), want (%q, true)", c, dst, found, chainDst)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Accept")
	}
}
