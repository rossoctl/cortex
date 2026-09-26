package tlsbridge

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestTerminator_AgentTrustingCAHandshakes(t *testing.T) {
	src, _ := NewEphemeralSource()
	m := NewMinter(src, MinterOpts{})
	term := NewTerminator(m)

	c1, c2 := net.Pipe()
	defer func() { _ = c1.Close() }()
	defer func() { _ = c2.Close() }()

	errc := make(chan error, 1)
	go func() {
		tconn, err := term.Terminate(c2, "api.example.com")
		if err != nil {
			errc <- err
			return
		}
		_ = tconn.Close()
		errc <- nil
	}()

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(src.CACertPEM())
	client := tls.Client(c1, &tls.Config{ServerName: "api.example.com", RootCAs: pool, NextProtos: []string{"h2", "http/1.1"}})
	_ = c1.SetDeadline(time.Now().Add(2 * time.Second))
	if err := client.Handshake(); err != nil {
		t.Fatalf("client handshake failed (agent should trust minted leaf): %v", err)
	}
	if alpn := client.ConnectionState().NegotiatedProtocol; alpn != "h2" && alpn != "http/1.1" {
		t.Errorf("unexpected ALPN %q", alpn)
	}
	// Close the client's underlying pipe before reading <-errc so the
	// server's tconn.Close() close_notify write unblocks immediately
	// (with io.ErrClosedPipe) instead of stalling on its 5s write
	// deadline against a non-reading net.Pipe peer. The deferred
	// c1.Close() still runs (closing twice is harmless).
	_ = c1.Close()
	if err := <-errc; err != nil {
		t.Fatalf("terminator: %v", err)
	}
}

// TestServeConn_HTTP11KeepAlive drives two sequential HTTP/1.1 requests over a
// single terminated TLS conn, proving ServeConn keeps the conn alive between
// requests (the oneConnListener path).
func TestServeConn_HTTP11KeepAlive(t *testing.T) {
	src, _ := NewEphemeralSource()
	m := NewMinter(src, MinterOpts{})
	term := NewTerminator(m)

	c1, c2 := net.Pipe()
	defer func() { _ = c1.Close() }()
	defer func() { _ = c2.Close() }()

	var hits int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})

	// Server side: terminate, then serve with keep-alive.
	go func() {
		tconn, err := term.Terminate(c2, "api.example.com")
		if err != nil {
			return
		}
		ServeConn(tconn, handler)
	}()

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(src.CACertPEM())
	// Force http/1.1 so we exercise the oneConnListener keep-alive path.
	client := tls.Client(c1, &tls.Config{ServerName: "api.example.com", RootCAs: pool, NextProtos: []string{"http/1.1"}})
	_ = c1.SetDeadline(time.Now().Add(5 * time.Second))
	if err := client.Handshake(); err != nil {
		t.Fatalf("client handshake failed: %v", err)
	}
	if alpn := client.ConnectionState().NegotiatedProtocol; alpn != "http/1.1" {
		t.Fatalf("expected http/1.1, got %q", alpn)
	}

	br := bufio.NewReader(client)
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodGet, "https://api.example.com/", nil)
		// Default keep-alive (no Connection: close) so the server reuses the conn.
		if err := req.Write(client); err != nil {
			t.Fatalf("request %d write: %v", i, err)
		}
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			t.Fatalf("request %d read response: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status %d", i, resp.StatusCode)
		}
		// Fully drain and close the body so the reader is positioned at the
		// start of the next response (chunked encoding leaves a trailer).
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("expected handler to be hit twice on one conn, got %d", got)
	}
}
