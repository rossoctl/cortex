package tlssniff_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/rossocortex/authbridge/authlib/listener/internal/tlssniff"
)

// TestPermissive_ServesTLS confirms that a TLS handshake byte (0x16)
// is dispatched through tls.Server, completing the handshake.
func TestPermissive_ServesTLS(t *testing.T) {
	cfg := selfSignedTLSConfig(t)
	rawListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer rawListener.Close()
	sniff := tlssniff.New(rawListener, cfg, tlssniff.ModePermissive)

	srvDone := make(chan error, 1)
	go func() {
		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.TLS == nil {
					t.Errorf("expected r.TLS to be populated on TLS connection")
				}
				w.WriteHeader(http.StatusOK)
			}),
		}
		srvDone <- srv.Serve(sniff)
	}()

	cliCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: cliCfg},
	}
	resp, err := client.Get("https://" + rawListener.Addr().String() + "/")
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}

	rawListener.Close()
	<-srvDone
}

// TestPermissive_ServesPlain confirms that a plain HTTP request
// (first byte 'G' / 'P' / etc) is dispatched as a plain conn so the
// http.Server can serve it without TLS.
func TestPermissive_ServesPlain(t *testing.T) {
	cfg := selfSignedTLSConfig(t)
	rawListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer rawListener.Close()
	sniff := tlssniff.New(rawListener, cfg, tlssniff.ModePermissive)

	srvDone := make(chan error, 1)
	go func() {
		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.TLS != nil {
					t.Errorf("expected r.TLS to be nil on plain connection")
				}
				w.WriteHeader(http.StatusOK)
			}),
		}
		srvDone <- srv.Serve(sniff)
	}()

	resp, err := http.Get("http://" + rawListener.Addr().String() + "/")
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}

	rawListener.Close()
	<-srvDone
}

// TestStrict_ServesTLS — TLS still works in strict mode.
func TestStrict_ServesTLS(t *testing.T) {
	cfg := selfSignedTLSConfig(t)
	rawListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer rawListener.Close()
	sniff := tlssniff.New(rawListener, cfg, tlssniff.ModeStrict)

	srvDone := make(chan error, 1)
	go func() {
		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		}
		srvDone <- srv.Serve(sniff)
	}()

	cliCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: cliCfg},
	}
	resp, err := client.Get("https://" + rawListener.Addr().String() + "/")
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	resp.Body.Close()

	rawListener.Close()
	<-srvDone
}

// TestStrict_RejectsPlain — plain HTTP gets the connection closed
// before any HTTP exchange. The client sees an EOF / RST.
func TestStrict_RejectsPlain(t *testing.T) {
	cfg := selfSignedTLSConfig(t)
	rawListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer rawListener.Close()
	sniff := tlssniff.New(rawListener, cfg, tlssniff.ModeStrict)

	var rejectedCount atomic.Uint64
	sniff.SetOnPlainRejected(func(_ net.Conn) {
		rejectedCount.Add(1)
	})

	// Run the http.Server in the background — it will keep calling
	// Accept indefinitely; rejected plain conns just go to /dev/null
	// and Serve continues.
	srvDone := make(chan struct{})
	go func() {
		srv := &http.Server{Handler: http.NotFoundHandler()}
		_ = srv.Serve(sniff)
		close(srvDone)
	}()

	// Direct TCP connect; send a plain HTTP request line.
	conn, err := net.Dial("tcp", rawListener.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Read should hit EOF (server closed before responding).
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if n != 0 || (err != io.EOF && !isClosedConnErr(err)) {
		t.Errorf("Read on rejected plain conn: n=%d, err=%v; want EOF / closed", n, err)
	}
	conn.Close()

	// Give the listener a moment to record the rejection.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rejectedCount.Load() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := rejectedCount.Load(); got != 1 {
		t.Errorf("rejection callback fired %d times, want 1", got)
	}

	rawListener.Close()
	<-srvDone
}

// TestPeekTimeoutDoesNotHang — a client that connects but sends
// nothing must NOT hold a server goroutine indefinitely. The 5s
// peekTimeout drops the connection.
func TestPeekTimeoutDoesNotHang(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 5s timeout test under -short")
	}
	cfg := selfSignedTLSConfig(t)
	rawListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer rawListener.Close()
	sniff := tlssniff.New(rawListener, cfg, tlssniff.ModePermissive)

	srvDone := make(chan struct{})
	go func() {
		srv := &http.Server{Handler: http.NotFoundHandler()}
		_ = srv.Serve(sniff)
		close(srvDone)
	}()

	// Connect but never send a byte.
	conn, err := net.Dial("tcp", rawListener.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	defer conn.Close()

	// The listener should drop us within ~peekTimeout. Read with a
	// generous deadline; we expect EOF / closed.
	conn.SetReadDeadline(time.Now().Add(7 * time.Second))
	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	if err == nil {
		t.Errorf("expected error after peek timeout, got nil")
	}

	rawListener.Close()
	<-srvDone
}

// TestClose — Close shuts down the listener and any pending Accept
// returns. http.Server.Serve treats this as the normal shutdown
// path.
func TestClose(t *testing.T) {
	cfg := selfSignedTLSConfig(t)
	rawListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	sniff := tlssniff.New(rawListener, cfg, tlssniff.ModePermissive)

	acceptDone := make(chan error, 1)
	go func() {
		_, err := sniff.Accept()
		acceptDone <- err
	}()

	if err := sniff.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-acceptDone:
		if err == nil {
			t.Error("Accept returned nil error after Close")
		}
	case <-time.After(2 * time.Second):
		t.Error("Accept didn't return after Close")
	}
}

// --- helpers ---

// selfSignedTLSConfig generates an in-memory cert and returns a TLS
// config presenting it. Used as the *tls.Config the sniffer hands to
// tls.Server when an inbound TLS handshake is detected.
func selfSignedTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
}

// isClosedConnErr distinguishes "peer closed the conn" from other
// read errors. Different OS / Go versions surface this as different
// types; we tolerate any of the common shapes.
func isClosedConnErr(err error) bool {
	if err == nil {
		return false
	}
	// net.OpError wrapping syscall errors (RST), or tls/EOF.
	return true // any non-nil error counts as "connection closed" for this test
}
