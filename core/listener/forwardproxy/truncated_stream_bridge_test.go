package forwardproxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/plugintesting"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// TestForwardProxy_TruncatedStream_BridgedReachesClientAsError: the #1543 fix
// through the TLS bridge — the laptop's own shape. The origin terminates TLS
// itself, sends one SSE event, then resets; the agent speaks TLS to the bridge
// and must see the break on its decrypted stream, not a clean end of body.
// Modeled on TestTransparentBridge (same net.Pipe + manual h1 client split,
// which that test's comment explains).
func TestForwardProxy_TruncatedStream_BridgedReachesClientAsError(t *testing.T) {
	// TLS origin: one flushed SSE event, then a TCP reset. The bridge's
	// upstream-verify HEAD goes to this handler too — answer it normally, or
	// the hijack+RST poisons the pooled conn the GET reuses and the proxy
	// answers a clean 502 instead of the truncation under test.
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"n\":1}\n\n")
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		// A TLS server's hijacked conn is *tls.Conn; reset the TCP conn it
		// wraps so the break reaches the bridge as a read error.
		if tcp, ok := conn.(interface{ NetConn() net.Conn }); ok {
			if raw, ok := tcp.NetConn().(*net.TCPConn); ok {
				_ = raw.SetLinger(0)
			}
		}
		_ = conn.Close()
	}))
	defer origin.Close()

	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatalf("parse origin URL: %v", err)
	}
	originHostPort := originURL.Host

	originCert := origin.Certificate()
	originCAPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: originCert.Raw})

	src, err := tlsbridge.NewEphemeralSource()
	if err != nil {
		t.Fatalf("NewEphemeralSource: %v", err)
	}
	up, err := tlsbridge.NewUpstreamClient(originCAPEM, false)
	if err != nil {
		t.Fatalf("NewUpstreamClient: %v", err)
	}
	engine := &tlsbridge.Engine{
		Decision: mustBridgeDecision(t, portOf(originHostPort)),
		Term:     tlsbridge.NewTerminator(tlsbridge.NewMinter(src, tlsbridge.MinterOpts{})),
		Skip:     tlsbridge.NewSkipSet(),
		Upstream: up,
		CAPEM:    src.CACertPEM(),
	}

	// A streaming responder so the decrypted response takes the SSE relay —
	// the arm whose clean-end bug #1543 describes.
	p, err := plugintesting.BuildPipeline([]pipeline.Plugin{newStreamingProbe(false)})
	if err != nil {
		t.Fatalf("BuildPipeline: %v", err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Client:           &http.Client{Transport: &http.Transport{Proxy: nil}},
		TLSBridge:        engine,
	}

	clientSide, serverSide := net.Pipe()
	defer func() { _ = clientSide.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.HandleTransparentConn(serverSide, originHostPort)
	}()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(engine.CAPEM) {
		t.Fatalf("failed to append bridge CA PEM to pool")
	}
	tconn := tls.Client(clientSide, &tls.Config{
		ServerName: hostOnly(originHostPort),
		RootCAs:    pool,
		NextProtos: []string{"http/1.1"},
	})
	_ = tconn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tconn.Handshake(); err != nil {
		t.Fatalf("agent-side TLS handshake through bridge failed: %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, "https://"+originHostPort+"/stream", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	writeErr := make(chan error, 1)
	go func() { writeErr <- req.Write(tconn) }()

	resp, err := http.ReadResponse(bufio.NewReader(tconn), req)
	if err != nil {
		t.Fatalf("read response over bridged TLS conn: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bridged response status = %d, want 200 — a non-200 body ends cleanly, so the truncation path never ran", resp.StatusCode)
	}
	_, readErr := io.ReadAll(resp.Body)
	if werr := <-writeErr; werr != nil {
		t.Fatalf("write request over bridged TLS conn: %v", werr)
	}
	if readErr == nil {
		t.Error("truncated upstream stream reached the agent as a clean end of body through the TLS bridge")
	}

	_ = serverSide.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("HandleTransparentConn did not return after conns closed")
	}
}
