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
	defer func() { _ = clientSide.Close() }()

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

// TestTransparentBridge_CustomPort proves the configurable-ports path: the
// origin runs on a RANDOM ephemeral port (NOT in shouldSniff's hardcoded
// {80,443,8080,8443} set), and the bridge is configured (Decision.Ports) to
// intercept exactly that port. Before the shouldSniff↔Decision.Ports unification
// the transparent path would not sniff this port, so the bridge could never peek
// and the call would tunnel (agent handshake would fail against the origin's real
// cert). With the unification, HandlesPort(port) widens the sniff and the bridge
// engages. Asserting decryption here is the regression test for that wiring.
func TestTransparentBridge_CustomPort(t *testing.T) {
	var gotOriginPath string
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOriginPath = r.URL.Path
		if r.URL.Path == "/secret" {
			_, _ = w.Write([]byte("OK-SECRET"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer origin.Close()

	originCAPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatalf("parse origin URL: %v", err)
	}
	originHostPort := originURL.Host // 127.0.0.1:<random ephemeral port>
	customPort := portOf(originHostPort)
	if customPort == 443 || customPort == 8443 || customPort == 80 || customPort == 8080 {
		t.Skipf("random origin port %d collided with a default sniff port; rerun", customPort)
	}

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
		// Only the custom port is configured — proves both that it IS bridged
		// (despite shouldSniff not knowing it) and that the default set is replaced.
		Decision: tlsbridge.NewDecision(tlsbridge.DecisionOpts{
			Scope: tlsbridge.ScopeAll,
			Ports: map[int]bool{customPort: true},
		}),
		Term:     tlsbridge.NewTerminator(minter),
		Skip:     tlsbridge.NewSkipSet(),
		Upstream: up,
		CAPEM:    src.CACertPEM(),
	}

	probe := &bridgeProbePlugin{}
	p, err := plugintesting.BuildPipeline([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatalf("BuildPipeline: %v", err)
	}
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Client: http.DefaultClient, TLSBridge: engine}

	clientSide, serverSide := net.Pipe()
	defer func() { _ = clientSide.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.HandleTransparentConn(serverSide, originHostPort)
	}()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(engine.CAPEM) {
		t.Fatalf("append bridge CA")
	}
	host := hostOnly(originHostPort)
	tconn := tls.Client(clientSide, &tls.Config{ServerName: host, RootCAs: pool, NextProtos: []string{"http/1.1"}})
	_ = tconn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tconn.Handshake(); err != nil {
		t.Fatalf("agent handshake through bridge on custom port failed: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, "https://"+host+"/secret", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	writeErr := make(chan error, 1)
	go func() { writeErr <- req.Write(tconn) }()
	resp, err := http.ReadResponse(bufio.NewReader(tconn), req)
	if err != nil {
		t.Fatalf("read response on custom-port bridge: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if werr := <-writeErr; werr != nil {
		t.Fatalf("write request: %v", werr)
	}

	if probe.gotPath != "/secret" {
		t.Errorf("probe path = %q, want /secret (bridge did not engage on custom port)", probe.gotPath)
	}
	if gotOriginPath != "/secret" {
		t.Errorf("origin path = %q, want /secret", gotOriginPath)
	}
	if got := strings.TrimSpace(string(body)); got != "OK-SECRET" {
		t.Errorf("body = %q, want OK-SECRET", got)
	}

	_ = clientSide.Close()
	_ = serverSide.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("HandleTransparentConn did not return")
	}
}

// TestConnectBridge drives the full CONNECT → 200 → agent-TLS → decrypt →
// pipeline → re-originate loop through the PUBLIC forward-proxy HTTP handler
// (so the real net/http Hijack path runs). An httptest TLS origin stands in
// for the upstream. The agent dials the proxy, issues CONNECT, reads the 200,
// then speaks TLS over the SAME raw conn trusting the bridge's ephemeral CA;
// the proxy forges a leaf, terminates, runs the pipeline (probe records the
// plaintext), and re-originates to the real origin. Body must arrive intact.
func TestConnectBridge(t *testing.T) {
	// 1) TLS origin on a random port. CONNECT doesn't go through shouldSniff,
	//    so any port works here.
	var gotOriginPath string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOriginPath = r.URL.Path
		if r.URL.Path == "/secret" {
			_, _ = w.Write([]byte("OK-SECRET"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	origin := httptest.NewTLSServer(handler)
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
	//    treated as in-cluster and skipped. Ports must include the origin's
	//    random port (the CONNECT classify keys on portOf(r.Host)).
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

	// 3) Server wired with a real OutboundPipeline carrying the recording probe.
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

	// 4) Stand up the public forward-proxy HTTP handler so Hijack works.
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}

	// 5) Raw-dial the proxy and issue CONNECT to the origin host:port.
	rawConn, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = rawConn.Close() }()
	_ = rawConn.SetDeadline(time.Now().Add(10 * time.Second))

	connectReq, err := http.NewRequest(http.MethodConnect, "//"+originHostPort, nil)
	if err != nil {
		t.Fatalf("new CONNECT request: %v", err)
	}
	connectReq.Host = originHostPort
	connectReq.URL.Host = originHostPort
	if err := connectReq.Write(rawConn); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	// Read the 200 Connection Established. http.ReadResponse against the
	// CONNECT request consumes exactly the status line + headers (no body),
	// leaving the raw conn positioned at the first post-200 byte — which is
	// where the agent's ClientHello begins.
	br := bufio.NewReader(rawConn)
	connectResp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if connectResp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", connectResp.StatusCode)
	}
	_ = connectResp.Body.Close()

	// 6) Agent-side TLS over the SAME raw conn (wrapped so any bytes ReadResponse
	//    buffered past the 200 are replayed — there should be none, but be safe).
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(engine.CAPEM) {
		t.Fatalf("failed to append bridge CA PEM to pool")
	}
	host := hostOnly(originHostPort) // "127.0.0.1"
	tlsTransport := &bufferedConn{Conn: rawConn, r: br}
	tconn := tls.Client(tlsTransport, &tls.Config{
		ServerName: host,
		RootCAs:    pool,
		NextProtos: []string{"http/1.1"},
	})
	_ = tconn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tconn.Handshake(); err != nil {
		t.Fatalf("agent-side TLS handshake through CONNECT bridge failed: %v", err)
	}

	// 7) GET /secret over the bridged TLS conn. Same goroutine-write +
	//    http.ReadResponse split as TestTransparentBridge: the server makes a
	//    blocking upstream round-trip between reading the request and writing
	//    the response, so a single-goroutine RoundTrip can wedge.
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

	// 8) Assertions.
	if probe.calls == 0 {
		t.Fatalf("probe plugin never ran — pipeline did not see decrypted request (CONNECT bridge branch missing?)")
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
}

// bufferedConn wraps a net.Conn whose leading bytes were partly drained into a
// bufio.Reader (e.g. by http.ReadResponse on the CONNECT 200), so a subsequent
// reader (the agent's tls.Client) replays those buffered bytes before reading
// from the wire. Mirrors peekedConn's Read-replays-buffered semantics for the
// client side of the test harness.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// listenSniffable binds a listener on the first free port shouldSniff() accepts
// (8443 then 8080), so the transparent path engages the bridge instead of
// tunneling. Returns nil if none is free; callers t.Skip in that case (the
// transparent bridge path is gated on those ports — see TestTransparentBridge).
func listenSniffable() net.Listener {
	for _, p := range []string{"8443", "8080"} {
		if l, e := net.Listen("tcp", "127.0.0.1:"+p); e == nil {
			return l
		}
	}
	return nil
}

// TestBridge_UnverifiableUpstream_FallsOpenToTunnel encodes the headline
// no-broken-calls guarantee for the un-bridgeable-origin case: when the bridge's
// own Upstream client does NOT trust the origin's CA, bridgeServe's upstream-verify
// HEAD fails BEFORE any leaf is forged, bridgeServe returns false, and the
// transparent branch re-dials and tunnels. The agent's OWN end-to-end TLS then
// reaches the origin untouched — the call succeeds and the pipeline never runs.
// probe.calls == 0 is the proof the traffic was tunneled, not bridged.
func TestBridge_UnverifiableUpstream_FallsOpenToTunnel(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/secret" {
			_, _ = w.Write([]byte("OK-SECRET"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ln := listenSniffable()
	if ln == nil {
		t.Skip("transparent bridge test needs a free sniffable port (8443 or 8080)")
	}
	origin := httptest.NewUnstartedServer(handler)
	_ = origin.Listener.Close()
	origin.Listener = ln
	origin.StartTLS()
	defer origin.Close()

	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatalf("parse origin URL: %v", err)
	}
	originHostPort := originURL.Host // "127.0.0.1:port"

	// Build the bridge Engine. The Upstream client trusts ONLY the system roots
	// (NewUpstreamClient(nil)) — it does NOT trust the httptest origin's
	// self-signed CA, so bridgeServe's upstream-verify HEAD fails and the branch
	// falls open to a plain tunnel.
	src, err := tlsbridge.NewEphemeralSource()
	if err != nil {
		t.Fatalf("NewEphemeralSource: %v", err)
	}
	minter := tlsbridge.NewMinter(src, tlsbridge.MinterOpts{})
	up, err := tlsbridge.NewUpstreamClient(nil)
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

	clientSide, serverSide := net.Pipe()
	defer func() { _ = clientSide.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.HandleTransparentConn(serverSide, originHostPort)
	}()

	// Agent-side TLS trusts the ORIGIN's real CA (NOT the bridge's ephemeral CA).
	// When the branch falls open and tunnels, the agent's own TLS handshake goes
	// end-to-end to the origin and verifies cleanly — proving the fall-open
	// preserved the working call.
	pool := x509.NewCertPool()
	pool.AddCert(origin.Certificate())
	host := hostOnly(originHostPort) // "127.0.0.1"
	tconn := tls.Client(clientSide, &tls.Config{
		ServerName: host,
		RootCAs:    pool,
		NextProtos: []string{"http/1.1"},
	})
	_ = tconn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tconn.Handshake(); err != nil {
		t.Fatalf("agent-side TLS handshake (expected to tunnel to origin) failed: %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, "https://"+host+"/secret", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	writeErr := make(chan error, 1)
	go func() { writeErr <- req.Write(tconn) }()

	resp, err := http.ReadResponse(bufio.NewReader(tconn), req)
	if err != nil {
		t.Fatalf("read response over tunneled TLS conn: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if werr := <-writeErr; werr != nil {
		t.Fatalf("write request over tunneled TLS conn: %v", werr)
	}

	if got := strings.TrimSpace(string(body)); got != "OK-SECRET" {
		t.Errorf("response body = %q, want OK-SECRET (fall-open tunnel should deliver the agent's own TLS to the origin)", got)
	}
	// PROOF the traffic was tunneled, not bridged: the bridge never decrypted the
	// agent's TLS, so the pipeline never saw the inner GET /secret. HandleTransparentConn
	// always runs ONE synthetic CONNECT-style egress gate per connection (Method=CONNECT,
	// Path="") before the bridge branch — that pass is expected. What must NEVER appear is
	// the decrypted inner request: if the bridge had terminated TLS, the probe would have
	// recorded Method=GET, Path=/secret (as TestTransparentBridge asserts on the happy path).
	if probe.gotMethod != http.MethodConnect || probe.gotPath != "" {
		t.Errorf("probe saw decrypted request (method=%q path=%q) — pipeline must NOT decrypt when upstream-verify fails (traffic was tunneled, not bridged)", probe.gotMethod, probe.gotPath)
	}

	_ = clientSide.Close()
	_ = serverSide.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("HandleTransparentConn did not return after conns closed")
	}
}

// TestBridge_PinnedClient_AutoSkipsThenTunnels encodes the no-broken-calls
// guarantee for the pinned-client case: a pinned agent rejects the forged leaf,
// so attempt 1's handshake fails inside bridgeServe (which Skip.Add's the host),
// and attempt 2 short-circuits on Skip.Contains straight to a plain tunnel that
// reaches the origin via the agent's own TLS. The same Engine is reused across
// both attempts so the SkipSet persists; probe.calls stays 0 throughout (the
// pipeline never ran — the first attempt died at the forged handshake, the
// second was tunneled).
func TestBridge_PinnedClient_AutoSkipsThenTunnels(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/secret" {
			_, _ = w.Write([]byte("OK-SECRET"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ln := listenSniffable()
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

	// Upstream trusts the origin (NewUpstreamClient(originCAPEM)) so upstream-verify
	// PASSES and bridgeServe reaches the Terminate step where the pinned agent
	// rejects the minted leaf.
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

	host := hostOnly(originHostPort) // "127.0.0.1"
	// The transparent branch derives key = hostOnly(host); host is "<SNI>:port"
	// when a name was sniffed. The agent sets SNI = "127.0.0.1", so key == "127.0.0.1".
	wantKey := host

	// Pinned agent: trusts ONLY the origin's real CA, never the bridge's ephemeral
	// CA — so it rejects the minted leaf in BOTH attempts.
	pinnedPool := x509.NewCertPool()
	pinnedPool.AddCert(origin.Certificate())

	// --- Attempt 1: bridge forges, pinned agent rejects → handshake fails, host skipped. ---
	clientSide1, serverSide1 := net.Pipe()
	go srv.HandleTransparentConn(serverSide1, originHostPort)

	tconn1 := tls.Client(clientSide1, &tls.Config{
		ServerName: host,
		RootCAs:    pinnedPool,
		NextProtos: []string{"http/1.1"},
	})
	_ = tconn1.SetDeadline(time.Now().Add(10 * time.Second))
	hsErr := tconn1.Handshake()
	if hsErr == nil {
		t.Fatalf("attempt 1: pinned agent handshake unexpectedly SUCCEEDED — it must reject the bridge's minted leaf")
	}
	_ = clientSide1.Close()
	_ = serverSide1.Close()

	// The rejected handshake must have auto-skipped the host (bridgeServe Skip.Add
	// on Terminate error). Poll briefly: bridgeServe runs on the server goroutine,
	// so Skip.Add may land just after the client observes the handshake error.
	skipped := false
	for i := 0; i < 100; i++ {
		if engine.Skip.Contains(wantKey) {
			skipped = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !skipped {
		t.Fatalf("attempt 1: engine.Skip does not contain %q after pinned-client handshake failure — auto-skip did not fire", wantKey)
	}

	// --- Attempt 2: host is skipped → branch short-circuits to a plain tunnel. ---
	// The same pinned agent now handshakes end-to-end to the origin (which it
	// trusts) through the tunnel, and GET /secret succeeds.
	clientSide2, serverSide2 := net.Pipe()
	defer func() { _ = clientSide2.Close() }()
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		srv.HandleTransparentConn(serverSide2, originHostPort)
	}()

	tconn2 := tls.Client(clientSide2, &tls.Config{
		ServerName: host,
		RootCAs:    pinnedPool,
		NextProtos: []string{"http/1.1"},
	})
	_ = tconn2.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tconn2.Handshake(); err != nil {
		t.Fatalf("attempt 2: tunneled handshake to origin failed (auto-skip retry should tunnel cleanly): %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, "https://"+host+"/secret", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	writeErr := make(chan error, 1)
	go func() { writeErr <- req.Write(tconn2) }()

	resp, err := http.ReadResponse(bufio.NewReader(tconn2), req)
	if err != nil {
		t.Fatalf("attempt 2: read response over tunneled TLS conn: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("attempt 2: read response body: %v", err)
	}
	if werr := <-writeErr; werr != nil {
		t.Fatalf("attempt 2: write request over tunneled TLS conn: %v", werr)
	}

	if got := strings.TrimSpace(string(body)); got != "OK-SECRET" {
		t.Errorf("attempt 2: response body = %q, want OK-SECRET (skipped host should tunnel to origin)", got)
	}
	// PROOF neither attempt bridged: the probe never saw a decrypted inner request.
	// Each HandleTransparentConn runs one synthetic CONNECT egress gate (Method=CONNECT,
	// Path=""); attempt 1 then died at the forged handshake (before any decrypted request
	// could reach the pipeline) and attempt 2 short-circuited on Skip.Contains to a plain
	// tunnel. If either had terminated TLS, the probe would have recorded GET /secret.
	if probe.gotMethod != http.MethodConnect || probe.gotPath != "" {
		t.Errorf("probe saw decrypted request (method=%q path=%q) — neither attempt should decrypt (attempt 1 died at the forged handshake; attempt 2 was tunneled)", probe.gotMethod, probe.gotPath)
	}

	_ = clientSide2.Close()
	_ = serverSide2.Close()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatalf("attempt 2: HandleTransparentConn did not return after conns closed")
	}
}

// TestBridge_NonTLS_Passthrough encodes the no-broken-calls guarantee for
// non-TLS traffic on a bridge-eligible port: the bridge peeks 5 bytes, sees an
// HTTP method (not a TLS record), Classify returns Passthrough,"non-tls", and
// the branch tunnels. A plaintext GET /secret reaches the origin and its body is
// returned; probe.calls stays 0 (the pipeline never ran on the opaque tunnel).
func TestBridge_NonTLS_Passthrough(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/secret" {
			_, _ = w.Write([]byte("OK-SECRET"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ln := listenSniffable()
	if ln == nil {
		t.Skip("transparent bridge test needs a free sniffable port (8443 or 8080)")
	}
	// PLAINTEXT origin (not TLS) so the tunneled plaintext request reaches it cleanly.
	origin := httptest.NewUnstartedServer(handler)
	_ = origin.Listener.Close()
	origin.Listener = ln
	origin.Start()
	defer origin.Close()

	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatalf("parse origin URL: %v", err)
	}
	originHostPort := originURL.Host // "127.0.0.1:port"

	src, err := tlsbridge.NewEphemeralSource()
	if err != nil {
		t.Fatalf("NewEphemeralSource: %v", err)
	}
	minter := tlsbridge.NewMinter(src, tlsbridge.MinterOpts{})
	up, err := tlsbridge.NewUpstreamClient(nil)
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

	clientSide, serverSide := net.Pipe()
	defer func() { _ = clientSide.Close() }()
	_ = clientSide.SetDeadline(time.Now().Add(10 * time.Second))

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.HandleTransparentConn(serverSide, originHostPort)
	}()

	// Drive a PLAINTEXT GET /secret (no TLS layer): the sniff peeks an HTTP method
	// byte ('G', not 0x16), looksLikeTLSRecord is false → Passthrough,"non-tls" → tunnel.
	// Same concurrency split as the TLS tests: write the request in a goroutine,
	// read the response here, so both pipe directions progress through the tunnel.
	req, err := http.NewRequest(http.MethodGet, "http://"+originHostPort+"/secret", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	writeErr := make(chan error, 1)
	go func() { writeErr <- req.Write(clientSide) }()

	resp, err := http.ReadResponse(bufio.NewReader(clientSide), req)
	if err != nil {
		t.Fatalf("read response over plaintext tunnel: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if werr := <-writeErr; werr != nil {
		t.Fatalf("write plaintext request over tunnel: %v", werr)
	}

	if got := strings.TrimSpace(string(body)); got != "OK-SECRET" {
		t.Errorf("response body = %q, want OK-SECRET (non-TLS passthrough should tunnel to the plaintext origin)", got)
	}
	// PROOF the bytes were passthrough-tunneled, not bridged: the probe only saw the
	// synthetic CONNECT egress gate (Method=CONNECT, Path=""), never a decrypted inner
	// request. Classify returned Passthrough,"non-tls" so the bridge never forged a leaf
	// or terminated TLS; the plaintext GET went straight down the tunnel to the origin.
	if probe.gotMethod != http.MethodConnect || probe.gotPath != "" {
		t.Errorf("probe saw decrypted request (method=%q path=%q) — non-TLS traffic must passthrough-tunnel, not be bridged", probe.gotMethod, probe.gotPath)
	}

	_ = clientSide.Close()
	_ = serverSide.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("HandleTransparentConn did not return after conns closed")
	}
}
