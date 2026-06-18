package forwardproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/plugintesting"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/tlsbridge"
)

// bridgeProbePlugin records the decrypted request method/path/headers it sees.
// It proves the UNCHANGED outbound pipeline runs on the plaintext request the
// TLS bridge produced after terminating the agent's TLS — i.e. the bridge
// decrypted the bytes and handed them to the pipeline, not an opaque tunnel.
type bridgeProbePlugin struct {
	gotMethod string
	gotPath   string
	gotHeader http.Header
	calls     int
}

func (p *bridgeProbePlugin) Name() string { return "bridge-probe" }
func (p *bridgeProbePlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *bridgeProbePlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.calls++
	p.gotMethod = pctx.Method
	p.gotPath = pctx.Path
	p.gotHeader = pctx.Headers.Clone()
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *bridgeProbePlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// TestTransparentBridge drives the full decrypt → pipeline → re-originate loop
// through HandleTransparentConn. An httptest TLS origin stands in for the real
// upstream. The agent side speaks TLS to the proxy trusting the bridge's
// ephemeral CA; the proxy forges a leaf, terminates, runs the pipeline (probe
// records the plaintext), and re-originates to the real origin over the
// upstream client (trusting the origin's own CA). The body must arrive intact.
func TestTransparentBridge(t *testing.T) {
	// 1) TLS origin: records the decrypted path it actually received and
	//    returns a known body for /secret.
	var gotOriginPath string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOriginPath = r.URL.Path
		if r.URL.Path == "/secret" {
			_, _ = w.Write([]byte("OK-SECRET"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	// The transparent listener only sniffs — and therefore only bridges — on the
	// ports shouldSniff() accepts {80,443,8080,8443}; the bridge piggybacks on the
	// sniffed *peekedConn for its record-header peek. Bind the origin to a non-root
	// sniffable port so HandleTransparentConn engages the bridge rather than tunneling.
	var ln net.Listener
	for _, p := range []string{"8443", "8080"} {
		if l, e := net.Listen("tcp", "127.0.0.1:"+p); e == nil {
			ln = l
			break
		}
	}
	if ln == nil {
		t.Skip("transparent bridge test needs a free sniffable port (8443 or 8080)")
	}
	origin := httptest.NewUnstartedServer(handler)
	_ = origin.Listener.Close()
	origin.Listener = ln
	origin.StartTLS()
	defer origin.Close()

	originCAPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: origin.Certificate().Raw,
	})

	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatalf("parse origin URL: %v", err)
	}
	originHostPort := originURL.Host // "127.0.0.1:port"

	// 2) Build the bridge Engine. ScopeAll so the loopback origin isn't
	//    treated as in-cluster and skipped.
	src, err := tlsbridge.NewEphemeralSource()
	if err != nil {
		t.Fatalf("NewEphemeralSource: %v", err)
	}
	minter := tlsbridge.NewMinter(src, tlsbridge.MinterOpts{})
	up, err := tlsbridge.NewUpstreamClient(originCAPEM)
	if err != nil {
		t.Fatalf("NewUpstreamClient: %v", err)
	}
	engine := &tlsbridge.Engine{
		Decision: tlsbridge.NewDecision(tlsbridge.DecisionOpts{
			Scope: tlsbridge.ScopeAll,
			Ports: map[int]bool{portOf(originHostPort): true},
		}),
		Term:     tlsbridge.NewTerminator(minter),
		Skip:     tlsbridge.NewSkipSet(),
		Upstream: up,
		CAPEM:    src.CACertPEM(),
	}

	// 3) Server wired with a real OutboundPipeline carrying the recording
	//    probe plugin (mirrors server_test.go's plugintesting.BuildPipeline +
	//    pipeline.NewHolder construction).
	probe := &bridgeProbePlugin{}
	p, err := plugintesting.BuildPipeline([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatalf("BuildPipeline: %v", err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Client:           http.DefaultClient,
		TLSBridge:        engine,
	}

	// 4) Drive HandleTransparentConn over an in-memory conn pair. The server
	//    side believes it captured a connection whose SO_ORIGINAL_DST is the
	//    origin's host:port.
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.HandleTransparentConn(serverSide, originHostPort)
	}()

	// Agent-side TLS: trust the bridge's ephemeral CA, set SNI to 127.0.0.1
	// (the Minter forges an IP-SAN leaf for it; httptest's cert also has a
	// 127.0.0.1 SAN so the upstream HEAD/relay verifies too).
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(engine.CAPEM) {
		t.Fatalf("failed to append bridge CA PEM to pool")
	}
	host := hostOnly(originHostPort) // "127.0.0.1"
	// Offer only http/1.1 so ALPN matches the h1.1 round-trip below. (The h2
	// serving path is covered by the tlsbridge ServeConn unit tests.)
	tconn := tls.Client(clientSide, &tls.Config{
		ServerName: host,
		RootCAs:    pool,
		NextProtos: []string{"http/1.1"},
	})
	_ = tconn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tconn.Handshake(); err != nil {
		t.Fatalf("agent-side TLS handshake through bridge failed: %v", err)
	}

	// Send GET /secret over the now-decryptable TLS conn. net.Pipe is a
	// synchronous, unbuffered, full-duplex pair: a Write blocks until the
	// peer Reads. Driving the request write and the response read from the
	// same goroutine — as http.Transport.RoundTrip does internally — can
	// wedge here, because the server side (ServeConn → serveOutbound) makes
	// a blocking upstream round-trip to the origin between reading the
	// request and writing the response, and the http/1.1 client write isn't
	// guaranteed to have fully drained into the server before the server
	// stops reading and starts its upstream call. Write the request in its
	// own goroutine and read the response in this one, so both pipe
	// directions can make progress independently. This mirrors the
	// client/server concurrency split in tlsbridge's known-good
	// TestServeConn_HTTP11KeepAlive (server serves in a goroutine; the
	// client drives req.Write + http.ReadResponse manually).
	req, err := http.NewRequest(http.MethodGet, "https://"+host+"/secret", nil)
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
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if werr := <-writeErr; werr != nil {
		t.Fatalf("write request over bridged TLS conn: %v", werr)
	}

	// Assertions.
	if probe.calls == 0 {
		t.Fatalf("probe plugin never ran — pipeline did not see decrypted request (bridge branch missing?)")
	}
	if probe.gotPath != "/secret" {
		t.Errorf("probe recorded path = %q, want /secret", probe.gotPath)
	}
	if probe.gotMethod != http.MethodGet {
		t.Errorf("probe recorded method = %q, want GET", probe.gotMethod)
	}
	if gotOriginPath != "/secret" {
		t.Errorf("origin received path = %q, want /secret", gotOriginPath)
	}
	if got := strings.TrimSpace(string(body)); got != "OK-SECRET" {
		t.Errorf("response body = %q, want OK-SECRET", got)
	}

	_ = clientSide.Close()
	_ = serverSide.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("HandleTransparentConn did not return after conns closed")
	}
}
