package forwardproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/listener/egressguard"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// redirectPlugin redirects every request to target and records what Redirect
// answered, separately for CONNECTs and for the requests the proxy re-originates.
// It declares WritesDestination, as a routing plugin does.
type redirectPlugin struct {
	target string

	mu         sync.Mutex
	sawConnect bool
	connectErr error
	sawRequest bool
	requestErr error
}

func (p *redirectPlugin) Name() string { return "redirector" }
func (p *redirectPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{WritesDestination: true, Description: "test"}
}
func (p *redirectPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	u, err := url.Parse(p.target)
	if err != nil {
		panic(err)
	}
	err = pctx.Redirect(u)
	p.mu.Lock()
	defer p.mu.Unlock()
	if pctx.Method == http.MethodConnect {
		p.sawConnect, p.connectErr = true, err
	} else {
		p.sawRequest, p.requestErr = true, err
	}
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *redirectPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// denyPlugin rejects every request, so a test can read the denied row a redirected
// request leaves.
type denyPlugin struct{}

func (denyPlugin) Name() string { return "denier" }
func (denyPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{Description: "test"}
}
func (denyPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	return pctx.DenyAndRecord("test_deny", "test.denied", "denied by the test")
}
func (denyPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// hostWriter writes pctx.Host without declaring WritesDestination, as any plugin can:
// the field is exported. It must not be able to move a request.
type hostWriter struct{ host string }

func (hostWriter) Name() string { return "host-writer" }
func (hostWriter) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{Description: "test"}
}
func (w hostWriter) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.Host = w.host
	return pipeline.Action{Type: pipeline.Continue}
}
func (hostWriter) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// origin is an httptest server that answers every request with its name and records
// the Host header and path of each GET. HEADs — the TLS bridge's upstream probe —
// are answered and not recorded.
type origin struct {
	*httptest.Server
	name string

	mu    sync.Mutex
	hosts []string
	paths []string
}

func newOrigin(t *testing.T, name string, start func(*httptest.Server)) *origin {
	t.Helper()
	o := &origin{name: name}
	o.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			o.mu.Lock()
			o.hosts = append(o.hosts, r.Host)
			o.paths = append(o.paths, r.URL.Path)
			o.mu.Unlock()
		}
		_, _ = io.WriteString(w, name)
	}))
	start(o.Server)
	t.Cleanup(o.Close)
	return o
}

func (o *origin) gets() (hosts, paths []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.hosts...), append([]string(nil), o.paths...)
}

func (o *origin) authority() string { return mustParseURL(o.URL).Host }

// newRedirectProxy serves a forward proxy whose outbound pipeline is plugins. store
// and bridge may be nil.
func newRedirectProxy(t *testing.T, store *session.Store, bridge *tlsbridge.Engine, plugins ...pipeline.Plugin) *httptest.Server {
	t.Helper()
	p, err := pipeline.New(plugins)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           http.DefaultClient,
		TLSBridge:        bridge,
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	return proxy
}

func proxyClient(proxy *httptest.Server, tlsConf *tls.Config) *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(mustParseURL(proxy.URL)),
		TLSClientConfig: tlsConf,
	}}
}

// redirectBridge is a TLS bridge that intercepts CONNECTs to port and whose
// upstream client trusts exactly trustPEM.
func redirectBridge(t *testing.T, port int, trustPEM []byte) *tlsbridge.Engine {
	t.Helper()
	src, err := tlsbridge.NewEphemeralSource()
	if err != nil {
		t.Fatalf("NewEphemeralSource: %v", err)
	}
	up, err := tlsbridge.NewUpstreamClient(trustPEM, false)
	if err != nil {
		t.Fatalf("NewUpstreamClient: %v", err)
	}
	return &tlsbridge.Engine{
		Decision: mustBridgeDecision(t, port),
		Term:     tlsbridge.NewTerminator(tlsbridge.NewMinter(src, tlsbridge.MinterOpts{})),
		Skip:     tlsbridge.NewSkipSet(),
		Upstream: up,
		CAPEM:    src.CACertPEM(),
	}
}

func certPEM(s *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
}

// bridgedGet CONNECTs to authority through proxy, completes the agent-side TLS
// handshake against the bridge's CA, and GETs path over the bridged connection —
// the sequence TestConnectBridge drives inline. It returns the status and body.
// edits change the decrypted request before it is sent: its Host header, which a
// client may set to something other than the CONNECT authority, or its headers.
func bridgedGet(t *testing.T, proxy *httptest.Server, bridgeCA []byte, authority, path string, edits ...func(*http.Request)) (int, string) {
	t.Helper()
	rawConn, err := net.Dial("tcp", mustParseURL(proxy.URL).Host)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = rawConn.Close() })
	_ = rawConn.SetDeadline(time.Now().Add(10 * time.Second))

	connectReq, err := http.NewRequest(http.MethodConnect, "//"+authority, nil)
	if err != nil {
		t.Fatalf("new CONNECT request: %v", err)
	}
	connectReq.Host = authority
	connectReq.URL.Host = authority
	if err := connectReq.Write(rawConn); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(rawConn)
	connectResp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	_ = connectResp.Body.Close()
	if connectResp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", connectResp.StatusCode)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bridgeCA) {
		t.Fatal("bridge CA PEM did not parse")
	}
	host := hostOnly(authority)
	tconn := tls.Client(&bufferedConn{Conn: rawConn, r: br}, &tls.Config{
		ServerName: host,
		RootCAs:    pool,
		NextProtos: []string{"http/1.1"},
	})
	if err := tconn.Handshake(); err != nil {
		t.Fatalf("agent-side TLS handshake through the bridge: %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, "https://"+authority+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for _, edit := range edits {
		edit(req)
	}
	// Write from a goroutine: the proxy makes a blocking upstream round trip between
	// reading the request and answering, as in TestConnectBridge.
	writeErr := make(chan error, 1)
	go func() { writeErr <- req.Write(tconn) }()
	resp, err := http.ReadResponse(bufio.NewReader(tconn), req)
	if err != nil {
		t.Fatalf("read response over the bridged connection: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if werr := <-writeErr; werr != nil {
		t.Fatalf("write request over the bridged connection: %v", werr)
	}
	return resp.StatusCode, strings.TrimSpace(string(body))
}

func eventsOf(t *testing.T, store *session.Store) []pipeline.SessionEvent {
	t.Helper()
	v := store.View(session.DefaultSessionID)
	if v == nil {
		t.Fatal("no events recorded")
	}
	return v.Events
}

func TestForwardProxy_RedirectSendsAPlainRequestToTheTarget(t *testing.T) {
	a := newOrigin(t, "FROM-A", (*httptest.Server).Start)
	b := newOrigin(t, "FROM-B", (*httptest.Server).Start)
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	plug := &redirectPlugin{target: b.URL}
	proxy := newRedirectProxy(t, store, nil, plug)

	resp, err := proxyClient(proxy, nil).Get(a.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK || string(body) != "FROM-B" {
		t.Fatalf("response = %d %q, want 200 FROM-B", resp.StatusCode, body)
	}
	if plug.requestErr != nil {
		t.Fatalf("Redirect on a plain proxied request: %v", plug.requestErr)
	}
	if hosts, _ := a.gets(); len(hosts) != 0 {
		t.Errorf("origin a served %v; the redirect did not take", hosts)
	}
	hosts, paths := b.gets()
	if len(hosts) != 1 || hosts[0] != b.authority() || paths[0] != "/v1/messages" {
		t.Errorf("origin b saw hosts %v paths %v, want one GET for %s /v1/messages", hosts, paths, b.authority())
	}
	for _, ev := range eventsOf(t, store) {
		if ev.Host != b.authority() || ev.RequestedHost != a.authority() {
			t.Errorf("%s row: host %q requested %q, want %q and %q", ev.Phase, ev.Host, ev.RequestedHost, b.authority(), a.authority())
		}
	}
}

// The listener applies the target Redirect validated, not the exported Host a later,
// undeclared plugin wrote; and the request row names where the bytes went.
func TestForwardProxy_ALaterHostWriteDoesNotSteerARedirect(t *testing.T) {
	a := newOrigin(t, "FROM-A", (*httptest.Server).Start)
	b := newOrigin(t, "FROM-B", (*httptest.Server).Start)
	c := newOrigin(t, "FROM-C", (*httptest.Server).Start)
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	proxy := newRedirectProxy(t, store, nil, &redirectPlugin{target: b.URL}, hostWriter{host: c.authority()})

	resp, err := proxyClient(proxy, nil).Get(a.URL + "/x")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK || string(body) != "FROM-B" {
		t.Fatalf("response = %d %q, want 200 FROM-B: only the redirect may choose the host", resp.StatusCode, body)
	}
	if hosts, _ := c.gets(); len(hosts) != 0 {
		t.Errorf("origin c served %v; an undeclared plugin steered the request", hosts)
	}
	var found bool
	for _, ev := range eventsOf(t, store) {
		if ev.Phase != pipeline.SessionRequest {
			continue
		}
		found = true
		if ev.Host != b.authority() || ev.RequestedHost != a.authority() {
			t.Errorf("request row: host %q requested %q, want %q and %q", ev.Host, ev.RequestedHost, b.authority(), a.authority())
		}
	}
	if !found {
		t.Error("no request row")
	}
}

func TestForwardProxy_NoRedirectRecordsNoRequestedHost(t *testing.T) {
	a := newOrigin(t, "FROM-A", (*httptest.Server).Start)
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	proxy := newRedirectProxy(t, store, nil, &bridgeProbePlugin{})

	resp, err := proxyClient(proxy, nil).Get(a.URL + "/x")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	resp.Body.Close()
	for _, ev := range eventsOf(t, store) {
		if ev.RequestedHost != "" {
			t.Errorf("%s row records requestedHost %q for a request nothing redirected", ev.Phase, ev.RequestedHost)
		}
	}
}

func TestConnectBridge_RedirectSendsTheDecryptedRequestToTheTarget(t *testing.T) {
	a := newOrigin(t, "FROM-A", (*httptest.Server).StartTLS)
	b := newOrigin(t, "FROM-B", (*httptest.Server).StartTLS)
	// httptest gives every TLS server the same certificate, so trusting a's trusts
	// b's. This test is about where the request goes; the next is about what the
	// bridge verifies when it gets there.
	engine := redirectBridge(t, portOf(a.authority()), certPEM(a.Server))
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	plug := &redirectPlugin{target: b.URL}
	proxy := newRedirectProxy(t, store, engine, plug)

	status, body := bridgedGet(t, proxy, engine.CAPEM, a.authority(), "/secret")

	if status != http.StatusOK || body != "FROM-B" {
		t.Fatalf("response = %d %q, want 200 FROM-B", status, body)
	}
	if hosts, _ := a.gets(); len(hosts) != 0 {
		t.Errorf("origin a served GETs %v; the redirect did not take", hosts)
	}
	if hosts, paths := b.gets(); len(hosts) != 1 || hosts[0] != b.authority() || paths[0] != "/secret" {
		t.Errorf("origin b saw hosts %v paths %v, want one GET for %s /secret", hosts, paths, b.authority())
	}
	plug.mu.Lock()
	defer plug.mu.Unlock()
	if !plug.sawConnect || plug.connectErr == nil || !strings.Contains(plug.connectErr.Error(), "cannot be redirected") {
		t.Errorf("Redirect on the CONNECT = %v; the CONNECT is dialed where the client named, so it must be refused", plug.connectErr)
	}
	if !plug.sawRequest || plug.requestErr != nil {
		t.Errorf("Redirect on the decrypted request = %v (seen: %v), want it accepted", plug.requestErr, plug.sawRequest)
	}
	var found bool
	for _, ev := range eventsOf(t, store) {
		if ev.Phase != pipeline.SessionRequest || ev.HTTPPath != "/secret" {
			continue
		}
		found = true
		// The decrypted request's own Host header names the tunnel's authority, so
		// that is the host the client named.
		if ev.Host != b.authority() || ev.RequestedHost != a.authority() {
			t.Errorf("request row: host %q requested %q, want %q and %q", ev.Host, ev.RequestedHost, b.authority(), a.authority())
		}
	}
	if !found {
		t.Error("no request row for /secret")
	}
}

// The inference router's local-gateway case — a bridged https request sent to a
// plaintext http server — and the only test that pins the listener's scheme copy.
func TestConnectBridge_RedirectToAPlaintextTargetChangesTheScheme(t *testing.T) {
	a := newOrigin(t, "FROM-A", (*httptest.Server).StartTLS)
	b := newOrigin(t, "FROM-B", (*httptest.Server).Start) // plaintext
	engine := redirectBridge(t, portOf(a.authority()), certPEM(a.Server))
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	proxy := newRedirectProxy(t, store, engine, &redirectPlugin{target: b.URL})
	if status, body := bridgedGet(t, proxy, engine.CAPEM, a.authority(), "/v1/messages"); status != 200 || body != "FROM-B" {
		t.Fatalf("got %d %q, want 200 FROM-B", status, body)
	}
}

func TestConnectBridge_RedirectVerifiesTheTargetsCertificate(t *testing.T) {
	a := newOrigin(t, "FROM-A", (*httptest.Server).StartTLS)
	// b presents a certificate from a CA the bridge's upstream client does not trust.
	other, err := tlsbridge.NewEphemeralSource()
	if err != nil {
		t.Fatalf("NewEphemeralSource: %v", err)
	}
	leaf, err := tlsbridge.NewMinter(other, tlsbridge.MinterOpts{}).GetCertificateForHost("127.0.0.1")
	if err != nil {
		t.Fatalf("mint b's certificate: %v", err)
	}
	b := newOrigin(t, "FROM-B", func(s *httptest.Server) {
		s.TLS = &tls.Config{Certificates: []tls.Certificate{*leaf}}
		s.StartTLS()
	})
	engine := redirectBridge(t, portOf(a.authority()), certPEM(a.Server))
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	proxy := newRedirectProxy(t, store, engine, &redirectPlugin{target: b.URL})

	status, _ := bridgedGet(t, proxy, engine.CAPEM, a.authority(), "/secret")

	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: the redirect target's certificate is untrusted", status)
	}
	if hosts, _ := b.gets(); len(hosts) != 0 {
		t.Errorf("origin b served %v despite an untrusted certificate", hosts)
	}
	var kind string
	for _, ev := range eventsOf(t, store) {
		if ev.Phase == pipeline.SessionResponse && ev.Error != nil {
			kind = ev.Error.Kind
		}
	}
	if kind != "upstream_tls" {
		t.Errorf("response row error kind = %q, want upstream_tls", kind)
	}
}

func TestForwardProxy_RedirectOnAConnectIsRefusedAndTheTunnelGoesWhereTheClientAsked(t *testing.T) {
	a := newOrigin(t, "FROM-A", (*httptest.Server).StartTLS)
	b := newOrigin(t, "FROM-B", (*httptest.Server).StartTLS)
	plug := &redirectPlugin{target: b.URL}
	proxy := newRedirectProxy(t, nil, nil, plug) // no bridge: the CONNECT is an opaque tunnel

	pool := x509.NewCertPool()
	pool.AddCert(a.Certificate())
	resp, err := proxyClient(proxy, &tls.Config{RootCAs: pool}).Get(a.URL + "/x")
	if err != nil {
		t.Fatalf("GET through the tunnel: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != "FROM-A" {
		t.Errorf("body = %q, want FROM-A: a tunnel goes where the client asked", body)
	}
	if hosts, _ := b.gets(); len(hosts) != 0 {
		t.Errorf("origin b served %v", hosts)
	}
	plug.mu.Lock()
	defer plug.mu.Unlock()
	if !plug.sawConnect || plug.connectErr == nil {
		t.Errorf("Redirect on a CONNECT = %v (seen: %v), want a refusal", plug.connectErr, plug.sawConnect)
	}
	if plug.sawRequest {
		t.Error("the pipeline saw a decrypted request on a tunnel with no bridge")
	}
}

func TestForwardProxy_ADeniedRedirectRecordsBothHosts(t *testing.T) {
	a := newOrigin(t, "FROM-A", (*httptest.Server).Start)
	b := newOrigin(t, "FROM-B", (*httptest.Server).Start)
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	proxy := newRedirectProxy(t, store, nil, &redirectPlugin{target: b.URL}, denyPlugin{})

	resp, err := proxyClient(proxy, nil).Get(a.URL + "/x")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode < 400 {
		t.Fatalf("status = %d, want a rejection", resp.StatusCode)
	}
	var found bool
	for _, ev := range eventsOf(t, store) {
		if ev.Phase != pipeline.SessionDenied {
			continue
		}
		found = true
		if ev.Host != b.authority() || ev.RequestedHost != a.authority() {
			t.Errorf("denied row: host %q requested %q, want %q and %q", ev.Host, ev.RequestedHost, b.authority(), a.authority())
		}
	}
	if !found {
		t.Error("no denied row")
	}
	if hosts, _ := a.gets(); len(hosts) != 0 {
		t.Errorf("origin a served a denied request: %v", hosts)
	}
	if hosts, _ := b.gets(); len(hosts) != 0 {
		t.Errorf("origin b served a denied request: %v", hosts)
	}
}

// #1595's headline: a session redirected away from a server that has since died.
// The pre-forge probe of the dead origin must not fall back to a plain tunnel — a
// plain tunnel dials the same dead origin, and an opaque tunnel is the one shape a
// redirect cannot reach. The bridge proceeds, and the redirect sends the request to
// the live server.
func TestConnectBridge_RedirectWorksWhenTheNamedOriginIsDead(t *testing.T) {
	a := newOrigin(t, "FROM-A", (*httptest.Server).StartTLS)
	b := newOrigin(t, "FROM-B", (*httptest.Server).StartTLS)
	engine := redirectBridge(t, portOf(a.authority()), certPEM(a.Server))
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	proxy := newRedirectProxy(t, store, engine, &redirectPlugin{target: b.URL})

	a.Close() // the server the client still names is gone

	status, body := bridgedGet(t, proxy, engine.CAPEM, a.authority(), "/secret")
	if status != http.StatusOK || body != "FROM-B" {
		t.Fatalf("response = %d %q, want 200 FROM-B — the redirect never applied", status, body)
	}
	if hosts, _ := b.gets(); len(hosts) != 1 || hosts[0] != b.authority() {
		t.Errorf("origin b saw GETs %v, want one for %s", hosts, b.authority())
	}
}

// The passthrough fallback keeps its reason to exist: an origin that is reachable
// but whose certificate the bridge cannot verify. Bridging would re-originate into
// the same verification failure, so the connection is tunnelled untouched and the
// agent's own e2e TLS decides.
func TestConnectBridge_UntrustedOriginStillFallsOpenToTunnel(t *testing.T) {
	a := newOrigin(t, "FROM-A", (*httptest.Server).StartTLS)
	foreign, err := tlsbridge.NewEphemeralSource()
	if err != nil {
		t.Fatalf("NewEphemeralSource: %v", err)
	}
	// The bridge's upstream client trusts a CA that is not the origin's.
	engine := redirectBridge(t, portOf(a.authority()), foreign.CACertPEM())
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	proxy := newRedirectProxy(t, store, engine, &redirectPlugin{target: a.URL})

	rawConn, err := net.Dial("tcp", mustParseURL(proxy.URL).Host)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = rawConn.Close() }()
	_ = rawConn.SetDeadline(time.Now().Add(10 * time.Second))
	connectReq, err := http.NewRequest(http.MethodConnect, "//"+a.authority(), nil)
	if err != nil {
		t.Fatalf("new CONNECT request: %v", err)
	}
	connectReq.Host = a.authority()
	connectReq.URL.Host = a.authority()
	if err := connectReq.Write(rawConn); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(rawConn)
	connectResp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	_ = connectResp.Body.Close()
	if connectResp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", connectResp.StatusCode)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(engine.CAPEM) {
		t.Fatal("bridge CA PEM did not parse")
	}
	tconn := tls.Client(&bufferedConn{Conn: rawConn, r: br}, &tls.Config{
		ServerName: hostOnly(a.authority()),
		RootCAs:    pool,
		NextProtos: []string{"http/1.1"},
	})
	if err := tconn.Handshake(); err == nil {
		_ = tconn.Close()
		t.Fatal("handshake against a leaf the bridge did not forge succeeded; the connection should have been tunnelled to the real origin, whose cert this client does not trust")
	}
	eventually(t, func() bool {
		return len(tunnelReasons(store, session.DefaultSessionID)) > 0
	}, "the passthrough tunnel's open row to be recorded")
	for _, reason := range tunnelReasons(store, session.DefaultSessionID) {
		if reason != pipeline.TunnelOriginUnverified {
			t.Errorf("tunnel reason = %q, want %q", reason, pipeline.TunnelOriginUnverified)
		}
	}
}

// A failed pre-dial is memoized: a client that opens a CONNECT per request
// (undici) must not pay a dead destination's dial cost — on a filtered
// destination, its full connectDialTimeout — on every one of them.
func TestConnectBridge_DeadOriginDialIsMemoized(t *testing.T) {
	a := newOrigin(t, "FROM-A", (*httptest.Server).StartTLS)
	b := newOrigin(t, "FROM-B", (*httptest.Server).StartTLS)
	engine := redirectBridge(t, portOf(a.authority()), certPEM(a.Server))
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	proxy := newRedirectProxy(t, store, engine, &redirectPlugin{target: b.URL})

	var dials struct {
		mu sync.Mutex
		n  int
	}
	inner := dialTCP
	dialTCP = func(network, addr string, d time.Duration) (net.Conn, error) {
		dials.mu.Lock()
		dials.n++
		dials.mu.Unlock()
		return inner(network, addr, d)
	}
	t.Cleanup(func() { dialTCP = inner })

	a.Close() // the server the client still names is gone
	for i := 0; i < 2; i++ {
		if status, body := bridgedGet(t, proxy, engine.CAPEM, a.authority(), "/secret"); status != http.StatusOK || body != "FROM-B" {
			t.Fatalf("request %d: response = %d %q, want 200 FROM-B", i, status, body)
		}
	}
	dials.mu.Lock()
	defer dials.mu.Unlock()
	if dials.n != 1 {
		t.Errorf("pre-dial ran %d times for 2 connections, want 1 (failures are memoized)", dials.n)
	}
}

// The two cleanups after a failed pre-dial — the hijack failing, and the 200
// write failing because the client is already gone — both closed the upstream,
// which is nil exactly when the pre-dial failed with the bridge enabled. A nil
// net.Conn has no method to call, so each cleanup panicked instead.
func TestHandleConnect_DeadPredialCleanupsDoNotPanic(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	target := l.Addr().String()
	_ = l.Close() // the pre-dial target: refused fast on loopback

	// The bridge must handle the target's port: the fall-through is gated on
	// that, so a Decision for a port nothing uses would take the 502 path and
	// never reach the nil-upstream cleanups this test exists to drive.
	d, err := tlsbridge.NewDecision(tlsbridge.DecisionOpts{Ports: map[int]bool{portOf(target): true}})
	if err != nil {
		t.Fatalf("NewDecision: %v", err)
	}
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := connectServer(t, store, &tlsbridge.Engine{Decision: d, Skip: tlsbridge.NewSkipSet()})

	connect := func() *http.Request {
		return &http.Request{
			Method: http.MethodConnect,
			Host:   target,
			URL:    &url.URL{Host: target},
			Header: http.Header{},
		}
	}

	t.Run("hijack fails", func(t *testing.T) {
		s.handleConnect(&hijackStubRW{hijackErr: errors.New("not now")}, connect())
	})
	t.Run("client gone before the 200", func(t *testing.T) {
		s.handleConnect(&hijackStubRW{}, connect())
	})
}

// hijackStubRW hands handleConnect a hijacked connection it controls:
// hijackErr makes Hijack itself fail; otherwise the conn's Write fails, standing
// in for a client that hung up before the proxy could answer 200.
type hijackStubRW struct {
	hijackErr error
}

func (w *hijackStubRW) Header() http.Header       { return http.Header{} }
func (w *hijackStubRW) Write([]byte) (int, error) { return 0, nil }
func (w *hijackStubRW) WriteHeader(int)           {}

func (w *hijackStubRW) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.hijackErr != nil {
		return nil, nil, w.hijackErr
	}
	return &failingWriteConn{}, bufio.NewReadWriter(bufio.NewReader(strings.NewReader("")), bufio.NewWriter(io.Discard)), nil
}

// failingWriteConn is a hijacked client conn whose Write always fails and whose
// Close is a no-op; only those two are called on it before handleConnect returns.
type failingWriteConn struct{ net.Conn }

func (c *failingWriteConn) Write([]byte) (int, error) { return 0, errors.New("client gone") }
func (c *failingWriteConn) Close() error              { return nil }

// Without the bridge, a failed pre-dial must not be memoized: the 502 is the
// last word on that path — nothing re-dials where the dial's result would have
// been used — so a memo hit would keep answering 502 for an origin that came
// back within the memo's TTL.
func TestHandleConnect_NoBridgeDialsFreshAfterAFailedDial(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	target := l.Addr().String()
	_ = l.Close() // dead for the first CONNECT

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := connectServer(t, store, nil)
	proxy := httptest.NewServer(http.HandlerFunc(s.handleRequest))
	defer proxy.Close()
	proxyAddr := strings.TrimPrefix(proxy.URL, "http://")

	if _, _, resp := sendConnect(t, proxyAddr, target); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("first CONNECT status = %d, want 502 (the destination is dead)", resp.StatusCode)
	}

	// The origin comes back on the same port.
	origin, err := net.Listen("tcp", target)
	if err != nil {
		t.Fatalf("re-listen: %v", err)
	}
	defer func() { _ = origin.Close() }()
	go func() {
		for {
			c, err := origin.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	_, _, resp := sendConnect(t, proxyAddr, target)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second CONNECT status = %d, want 200 — the origin is back, so the dial must be attempted again", resp.StatusCode)
	}
}

// A dead destination on a port the bridge never terminates keeps its immediate
// 502: the fall-through that lets the bridge take a dead origin exists so a
// redirect can apply, and a redirect can only apply on a port the bridge
// intercepts. Everywhere else the old answer stands.
func TestConnectBridge_UnbridgedPortKeepsItsDialFailure502(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	target := l.Addr().String()
	_ = l.Close() // dead for the pre-dial

	d, err := tlsbridge.NewDecision(tlsbridge.DecisionOpts{Ports: map[int]bool{1: true}})
	if err != nil {
		t.Fatalf("NewDecision: %v", err)
	}
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := connectServer(t, store, &tlsbridge.Engine{Decision: d, Skip: tlsbridge.NewSkipSet()})
	proxy := httptest.NewServer(http.HandlerFunc(s.handleRequest))
	defer proxy.Close()

	_, _, resp := sendConnect(t, strings.TrimPrefix(proxy.URL, "http://"), target)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("CONNECT status = %d, want 502 — a dead destination on a port the bridge cannot take is answered, not fallen through", resp.StatusCode)
	}
}

// declineAndCountDials serves a proxy whose bridge intercepts target's port but
// declines the connection — the client's post-CONNECT bytes are not a TLS
// record — over a dead target, counting every dial the proxy makes. It returns
// the proxy's address and a snapshot of the dial count.
func declineAndCountDials(t *testing.T, target string) (proxyAddr string, dials func() int) {
	t.Helper()
	d, err := tlsbridge.NewDecision(tlsbridge.DecisionOpts{Ports: map[int]bool{portOf(target): true}})
	if err != nil {
		t.Fatalf("NewDecision: %v", err)
	}
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	s := connectServer(t, store, &tlsbridge.Engine{Decision: d, Skip: tlsbridge.NewSkipSet()})

	var mu sync.Mutex
	n := 0
	inner := dialTCP
	dialTCP = func(network, addr string, d time.Duration) (net.Conn, error) {
		mu.Lock()
		n++
		mu.Unlock()
		return inner(network, addr, d)
	}
	t.Cleanup(func() { dialTCP = inner })

	proxy := httptest.NewServer(http.HandlerFunc(s.handleRequest))
	t.Cleanup(proxy.Close)
	return strings.TrimPrefix(proxy.URL, "http://"), func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// connectAndDecline sends a CONNECT to a dead target and follows it with five
// plaintext bytes, so the bridge's peek classifies the connection as non-TLS
// and declines it. It fails the test unless the CONNECT is answered 200.
func connectAndDecline(t *testing.T, proxyAddr, target string) {
	t.Helper()
	raw, br, resp := sendConnect(t, proxyAddr, target)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	if _, err := raw.Write([]byte("HELLO")); err != nil { // not a TLS record: the bridge declines
		t.Fatalf("write through tunnel: %v", err)
	}
	_, _ = io.ReadAll(br) // the proxy closes the tunnel; wait for it
}

// The bridge declined and the pre-dial already failed on a real dial: the
// client has its 200, and a second dial would only repeat the first one's
// failure — on a filtered destination, another full connectDialTimeout —
// before the close. One dial, then the close, with the dial-failed row the
// docs and dashboards key on.
func TestConnectBridge_DeclinedBridgeDoesNotRedialAFreshFailure(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	target := l.Addr().String()
	_ = l.Close() // dead for the pre-dial

	proxyAddr, dials := declineAndCountDials(t, target)
	connectAndDecline(t, proxyAddr, target)

	if n := dials(); n != 1 {
		t.Errorf("dials = %d, want 1 — the pre-dial already failed; re-dialing repeats it behind the client's 200", n)
	}
}

// A memo hit is the one decline where the re-dial is wanted: nothing was dialed
// for this connection, and the origin may have come back within the memo's TTL.
// Two CONNECTs therefore cost two dials — the first one's failed pre-dial, and
// the second one's fresh attempt — not three.
func TestConnectBridge_AMemoizedFailureRedialsWhenTheBridgeDeclines(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	target := l.Addr().String()
	_ = l.Close() // dead for every dial

	proxyAddr, dials := declineAndCountDials(t, target)
	connectAndDecline(t, proxyAddr, target)
	connectAndDecline(t, proxyAddr, target)

	if n := dials(); n != 2 {
		t.Errorf("dials = %d, want 2 — the first CONNECT's failed pre-dial, and the memo-hit CONNECT's fresh re-dial", n)
	}
}

// A resolution failure is the answer, not a dial of the empty checked address:
// with the bridge able to take the connection nothing is dialed (the bridge
// re-originates the redirected request itself), so no "missing address" error
// can mask the resolver's, and no empty memo key can pool every unresolvable
// name.
func TestConnectBridge_ResolutionFailureIsNotDialed(t *testing.T) {
	b := newOrigin(t, "FROM-B", (*httptest.Server).StartTLS)
	engine := redirectBridge(t, 443, certPEM(b.Server))
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	p, err := pipeline.New([]pipeline.Plugin{&redirectPlugin{target: b.URL}})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	s := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           http.DefaultClient,
		TLSBridge:        engine,
		// The zero Guard has no resolver, so checking the name fails with a
		// non-refusal error — the shape a real guard produces for a name that
		// will not resolve.
		EgressGuard: &egressguard.Guard{},
	}
	proxy := httptest.NewServer(s.Handler())
	defer proxy.Close()

	var mu sync.Mutex
	n := 0
	inner := dialTCP
	dialTCP = func(network, addr string, d time.Duration) (net.Conn, error) {
		mu.Lock()
		n++
		mu.Unlock()
		return inner(network, addr, d)
	}
	t.Cleanup(func() { dialTCP = inner })

	if status, body := bridgedGet(t, proxy, engine.CAPEM, "no-such-host.test:443", "/secret"); status != http.StatusOK || body != "FROM-B" {
		t.Fatalf("response = %d %q, want 200 FROM-B — the redirect never applied", status, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 0 {
		t.Errorf("dials = %d, want 0 — a resolution failure must not be dialed, least of all as an empty address", n)
	}
}

// The memo is keyed by client-named targets, and a scanner CONNECTing to many
// distinct dead names must not grow it without bound: recording a failure
// sweeps the entries the TTL has already expired.
func TestDialUpstream_SweepsExpiredMemoEntries(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := connectServer(t, store, nil)

	s.dialMu.Lock()
	s.dialFails = map[string]dialFailMemo{
		"expired:1": {err: errors.New("old"), at: time.Now().Add(-dialFailMemoTTL - time.Second)},
		"fresh:1":   {err: errors.New("recent"), at: time.Now()},
	}
	s.dialMu.Unlock()

	inner := dialTCP
	dialTCP = func(network, addr string, d time.Duration) (net.Conn, error) {
		return nil, errors.New("dead")
	}
	t.Cleanup(func() { dialTCP = inner })

	if _, err, _ := s.dialUpstream("other:1", true); err == nil {
		t.Fatal("dialUpstream succeeded against a failing dial")
	}

	s.dialMu.Lock()
	defer s.dialMu.Unlock()
	if _, ok := s.dialFails["expired:1"]; ok {
		t.Error("the expired memo entry survived recording a new failure")
	}
	for _, key := range []string{"fresh:1", "other:1"} {
		if _, ok := s.dialFails[key]; !ok {
			t.Errorf("memo entry %q missing; want only the expired one swept", key)
		}
	}
}
