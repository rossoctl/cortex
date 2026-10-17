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
	"sync"
	"testing"
	"time"

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
