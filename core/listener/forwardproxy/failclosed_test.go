package forwardproxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// failClosedScene is a fail-closed bridge in front of a TLS origin that counts
// the requests it serves.
type failClosedScene struct {
	proxyAddr string
	target    string
	engine    *tlsbridge.Engine
	store     *session.Store
	done      <-chan struct{}
	hits      *atomic.Int32
}

// newFailClosedScene builds the scene. trustOrigin says whether the bridge's
// upstream client trusts the origin; skipHosts is tls_bridge.passthrough_hosts.
func newFailClosedScene(t *testing.T, trustOrigin bool, skipHosts []string) *failClosedScene {
	t.Helper()
	hits := &atomic.Int32{}
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(origin.Close)
	target := strings.TrimPrefix(origin.URL, "https://")

	var upstreamCA []byte
	if trustOrigin {
		upstreamCA = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})
	}
	engine := failClosedEngine(t, portOf(target), upstreamCA, skipHosts)
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	proxyAddr, done := connectProxy(t, connectServer(t, store, engine))
	return &failClosedScene{proxyAddr: proxyAddr, target: target, engine: engine, store: store, done: done, hits: hits}
}

// failClosedEngine is bridgeEngine with tls_bridge.fail_closed on.
func failClosedEngine(t *testing.T, originPort int, upstreamCA []byte, skipHosts []string) *tlsbridge.Engine {
	t.Helper()
	engine := bridgeEngine(t, originPort, upstreamCA)
	d, err := tlsbridge.NewDecision(tlsbridge.DecisionOpts{
		Ports: map[int]bool{originPort: true}, SkipHosts: skipHosts, FailClosed: true,
	})
	if err != nil {
		t.Fatalf("NewDecision: %v", err)
	}
	engine.Decision = d
	engine.FailClosed = true
	return engine
}

// assertRefusal checks the scene's one tunnel pair: the open's reason and the
// close's status and error kind.
func (sc *failClosedScene) assertRefusal(t *testing.T, reason pipeline.TunnelReason, status int, kind string) {
	t.Helper()
	open, closed := onePair(t, sc.store, session.DefaultSessionID)
	if open.TunnelReason != reason {
		t.Errorf("open reason = %q, want %q", open.TunnelReason, reason)
	}
	if closed.StatusCode != status || closed.Error == nil || closed.Error.Kind != kind {
		t.Errorf("close = {status %d, error %+v}, want {%d, %s}", closed.StatusCode, closed.Error, status, kind)
	}
}

// An origin whose certificate does not verify is refused on the CONNECT itself,
// in the MOCA refusal shape, and serves nothing.
func TestFailClosed_UnverifiedOriginIs502(t *testing.T) {
	sc := newFailClosedScene(t, false, nil)

	_, _, resp := sendConnect(t, sc.proxyAddr, sc.target)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("CONNECT status = %d, want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	waitDone(t, sc.done)

	if got := resp.Header.Get("X-Moca-Egress-Error"); got != pipeline.ErrorKindUpstreamUnverified {
		t.Errorf("X-Moca-Egress-Error = %q, want %q", got, pipeline.ErrorKindUpstreamUnverified)
	}
	if want := "upstream_unverified: 127.0.0.1\n"; string(body) != want {
		t.Errorf("body = %q, want %q", body, want)
	}
	if n := sc.hits.Load(); n != 0 {
		t.Errorf("origin served %d requests, want 0", n)
	}
	sc.assertRefusal(t, pipeline.TunnelOriginUnverified, http.StatusBadGateway, pipeline.ErrorKindUpstreamUnverified)
}

// A ClientHello whose SNI names another host fails its handshake with a 421 row.
func TestFailClosed_SNIMismatchIs421(t *testing.T) {
	sc := newFailClosedScene(t, true, nil)

	raw, br, resp := sendConnect(t, sc.proxyAddr, sc.target)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(sc.engine.CAPEM)
	tc := tls.Client(&bufferedConn{Conn: raw, r: br}, &tls.Config{ServerName: "evil.test", RootCAs: pool})
	if err := tc.Handshake(); err == nil {
		t.Fatal("handshake with SNI evil.test succeeded, want it refused")
	}
	waitDone(t, sc.done)

	sc.assertRefusal(t, pipeline.TunnelSNIMismatch, http.StatusMisdirectedRequest, pipeline.ErrorKindMisdirectedRequest)
}

// A client that rejects the CA is refused each time: the bridge remembers
// nothing, so the retry is bridged again rather than tunnelled.
func TestFailClosed_CARejectionIsNotRemembered(t *testing.T) {
	sc := newFailClosedScene(t, true, nil)

	for i := 0; i < 2; i++ {
		raw, br, _ := sendConnect(t, sc.proxyAddr, sc.target)
		tc := tls.Client(&bufferedConn{Conn: raw, r: br}, &tls.Config{
			ServerName: hostOnly(sc.target), RootCAs: x509.NewCertPool(),
		})
		_ = tc.Handshake()
		_ = raw.Close()
		waitDone(t, sc.done)
	}

	opens, _ := tunnelRows(sc.store, session.DefaultSessionID)
	if len(opens) != 2 {
		t.Fatalf("%d tunnel opens, want 2", len(opens))
	}
	for i, o := range opens {
		if o.TunnelReason != pipeline.TunnelClientRejectedCA {
			t.Errorf("attempt %d reason = %q, want %q", i+1, o.TunnelReason, pipeline.TunnelClientRejectedCA)
		}
	}
	if sc.engine.Skip.Contains(hostOnly(sc.target)) {
		t.Error("the rejection was recorded in the skip set")
	}
}

// A host already in the skip set is bridged all the same.
func TestFailClosed_IgnoresSkipMemory(t *testing.T) {
	sc := newFailClosedScene(t, true, nil)
	sc.engine.Skip.Fail(hostOnly(sc.target))

	raw, br, _ := sendConnect(t, sc.proxyAddr, sc.target)
	tc := bridgedTLS(t, raw, br, sc.target, sc.engine.CAPEM)
	_ = tc.Close()
	waitDone(t, sc.done)
}

// Bytes that are not a TLS handshake are refused on an intercepted host...
func TestFailClosed_RefusesNonTLS(t *testing.T) {
	sc := newFailClosedScene(t, true, nil)

	raw, br, _ := sendConnect(t, sc.proxyAddr, sc.target)
	if _, err := io.WriteString(raw, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 64)
	n, _ := br.Read(buf)
	waitDone(t, sc.done)

	if n != 0 {
		t.Errorf("the client read %q, want the connection closed unanswered", buf[:n])
	}
	sc.assertRefusal(t, pipeline.TunnelRefusedNonTLS, http.StatusOK, pipeline.ErrorKindNotTLS)
}

// ...and tunnelled to an explicit passthrough host, which fail-closed still honours.
func TestFailClosed_ExplicitPassthroughTunnels(t *testing.T) {
	sc := newFailClosedScene(t, false, []string{"127.0.0.1"})

	raw, br, resp := sendConnect(t, sc.proxyAddr, sc.target)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	if _, err := io.WriteString(raw, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The TLS origin answers plaintext with a 400, which only a tunnel can carry.
	if got, _ := bufio.NewReader(br).ReadString('\n'); !strings.Contains(got, "400") {
		t.Errorf("first line = %q, want the origin's 400", got)
	}
}

// The transparent listener refuses non-TLS bytes to an intercepted host too.
func TestFailClosed_TransparentRefusesNonTLS(t *testing.T) {
	ln := listenSniffable()
	if ln == nil {
		t.Skip("transparent bridge test needs a free sniffable port (8443 or 8080)")
	}
	hits := &atomic.Int32{}
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	_ = origin.Listener.Close()
	origin.Listener = ln
	origin.Start()
	defer origin.Close()
	dst := strings.TrimPrefix(origin.URL, "http://")

	engine := failClosedEngine(t, portOf(dst), nil, nil)
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	srv := connectServer(t, store, engine)

	clientSide, serverSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.HandleTransparentConn(serverSide, dst)
	}()
	go func() { _, _ = io.WriteString(clientSide, "GET / HTTP/1.1\r\nHost: x\r\n\r\n") }()
	buf := make([]byte, 64)
	n, _ := clientSide.Read(buf)
	_ = clientSide.Close()
	<-done

	if n != 0 || hits.Load() != 0 {
		t.Errorf("client read %q and the origin served %d, want neither", buf[:n], hits.Load())
	}
	open, closed := onePair(t, store, session.DefaultSessionID)
	if open.TunnelReason != pipeline.TunnelRefusedNonTLS || closed.TunnelReason != pipeline.TunnelRefusedNonTLS {
		t.Errorf("reasons = {open %q, close %q}, want %q", open.TunnelReason, closed.TunnelReason, pipeline.TunnelRefusedNonTLS)
	}
	if closed.Error == nil || closed.Error.Kind != pipeline.ErrorKindNotTLS {
		t.Errorf("close error = %+v, want %s", closed.Error, pipeline.ErrorKindNotTLS)
	}
}

// The transparent listener's refusals of a TLS client carry their reason on both rows.
func TestFailClosed_TransparentRefusalReasons(t *testing.T) {
	for name, tc := range map[string]struct {
		trustOrigin bool
		reason      pipeline.TunnelReason
		status      int
		kind        string
	}{
		"unverified origin": {false, pipeline.TunnelOriginUnverified, http.StatusBadGateway, pipeline.ErrorKindUpstreamUnverified},
		"CA rejected":       {true, pipeline.TunnelClientRejectedCA, http.StatusOK, "tls_handshake"},
	} {
		t.Run(name, func(t *testing.T) {
			ln := listenSniffable()
			if ln == nil {
				t.Skip("transparent bridge test needs a free sniffable port (8443 or 8080)")
			}
			origin := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			_ = origin.Listener.Close()
			origin.Listener = ln
			origin.StartTLS()
			defer origin.Close()
			dst := strings.TrimPrefix(origin.URL, "https://")

			var upstreamCA []byte
			if tc.trustOrigin {
				upstreamCA = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})
			}
			store := session.New(5*time.Minute, 100, 0)
			defer store.Close()
			srv := connectServer(t, store, failClosedEngine(t, portOf(dst), upstreamCA, nil))

			clientSide, serverSide := net.Pipe()
			done := make(chan struct{})
			go func() {
				defer close(done)
				srv.HandleTransparentConn(serverSide, dst)
			}()
			client := tls.Client(clientSide, &tls.Config{ServerName: hostOnly(dst), RootCAs: x509.NewCertPool()})
			_ = client.Handshake()
			_ = clientSide.Close()
			<-done

			open, closed := onePair(t, store, session.DefaultSessionID)
			if open.TunnelReason != tc.reason || closed.TunnelReason != tc.reason {
				t.Errorf("reasons = {open %q, close %q}, want %q", open.TunnelReason, closed.TunnelReason, tc.reason)
			}
			if closed.StatusCode != tc.status || closed.Error == nil || closed.Error.Kind != tc.kind {
				t.Errorf("close = {status %d, error %+v}, want {%d, %s}", closed.StatusCode, closed.Error, tc.status, tc.kind)
			}
		})
	}
}

// A transparent client that stops short of a record header is bridged, as on
// CONNECT, so its handshake names the failure rather than a non-TLS refusal.
func TestFailClosed_TransparentShortPeekIsBridged(t *testing.T) {
	ln := listenSniffable()
	if ln == nil {
		t.Skip("transparent bridge test needs a free sniffable port (8443 or 8080)")
	}
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	_ = origin.Listener.Close()
	origin.Listener = ln
	origin.StartTLS()
	defer origin.Close()
	dst := strings.TrimPrefix(origin.URL, "https://")

	upstreamCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	srv := connectServer(t, store, failClosedEngine(t, portOf(dst), upstreamCA, nil))

	clientSide, serverSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.HandleTransparentConn(serverSide, dst)
	}()
	if _, err := clientSide.Write([]byte{0x16, 0x03}); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = clientSide.Close()
	<-done

	open, closed := onePair(t, store, session.DefaultSessionID)
	if open.TunnelReason != pipeline.TunnelClientHungUp || closed.TunnelReason != pipeline.TunnelClientHungUp {
		t.Errorf("reasons = {open %q, close %q}, want %q", open.TunnelReason, closed.TunnelReason, pipeline.TunnelClientHungUp)
	}
	if closed.StatusCode != http.StatusOK || closed.Error == nil || closed.Error.Kind != "tls_handshake" {
		t.Errorf("close = {status %d, error %+v}, want {200, tls_handshake}", closed.StatusCode, closed.Error)
	}
}

// A bridged transparent connection records its open beside its first request, as a
// bridged CONNECT does, and no close.
func TestFailClosed_TransparentBridgedFoldsOpen(t *testing.T) {
	ln := listenSniffable()
	if ln == nil {
		t.Skip("transparent bridge test needs a free sniffable port (8443 or 8080)")
	}
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	_ = origin.Listener.Close()
	origin.Listener = ln
	origin.StartTLS()
	defer origin.Close()
	dst := strings.TrimPrefix(origin.URL, "https://")

	upstreamCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})
	engine := failClosedEngine(t, portOf(dst), upstreamCA, nil)
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	srv := connectServer(t, store, engine)

	clientSide, serverSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.HandleTransparentConn(serverSide, dst)
	}()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(engine.CAPEM)
	client := tls.Client(clientSide, &tls.Config{ServerName: hostOnly(dst), RootCAs: pool, NextProtos: []string{"http/1.1"}})
	req, _ := http.NewRequest(http.MethodGet, "https://"+dst+"/", nil)
	go func() { _ = req.Write(client) }()
	resp, err := http.ReadResponse(bufio.NewReader(client), req)
	if err != nil {
		t.Fatalf("bridged request: %v", err)
	}
	_ = resp.Body.Close()
	_ = client.Close()
	<-done

	events := store.View(session.DefaultSessionID).Events
	if len(events) < 2 || !events[0].Tunnel || events[0].TunnelReason != "" || events[1].Tunnel {
		t.Fatalf("events = %+v, want the tunnel open directly before its request", events)
	}
	if _, closes := tunnelRows(store, session.DefaultSessionID); len(closes) != 0 {
		t.Errorf("%d tunnel closes, want none", len(closes))
	}
}

// A named program's remembered refusal and a predicted one are both ignored: the
// program is shown the bridge's leaf.
func TestFailClosed_IgnoresProgramMemoryAndTrust(t *testing.T) {
	helm := tlsbridge.Program{Exe: exeHelm}.Key()
	for name, configure := range map[string]func(*tlsbridge.Engine){
		"program-refused": func(e *tlsbridge.Engine) { e.Programs.Fail(helm) },
		"os-trust-only":   func(e *tlsbridge.Engine) { e.Trust = fakeTrust{exeHelm: true} },
	} {
		t.Run(name, func(t *testing.T) {
			procs := newFakeProcs(freshProc(300, 1, exeHelm))
			store := session.New(0, 0, 0)
			defer store.Close()
			sc := newBridgeScene(t, store, procs, func(s *Server) { s.TLSBridge.FailClosed = true })
			configure(sc.engine)

			if bridged, err := sc.handshake(t, procs, 300, sc.trusting); err != nil || !bridged {
				t.Errorf("helm was not bridged (bridged=%v, err=%v)", bridged, err)
			}
		})
	}
}

// A peek that comes up short is a slow client: it is bridged, and the handshake
// decides. A full record head that is not TLS is refused.
func TestFailClosed_ShortPeekIsBridged(t *testing.T) {
	engine := failClosedEngine(t, 443, nil, []string{"pinned.example.com"})
	s := &Server{TLSBridge: engine}
	for _, tc := range []struct {
		host, first string
		reason      pipeline.TunnelReason
		bridge      bool
	}{
		{"api.example.com", "", "", true},
		{"api.example.com", "GE", "", true},
		{"api.example.com", "GET /", pipeline.TunnelRefusedNonTLS, false},
		{"pinned.example.com", "GET /", pipeline.TunnelPassthroughNonTLS, false},
	} {
		r := httptest.NewRequest(http.MethodConnect, "http://"+tc.host+":443", nil)
		r.Host = tc.host + ":443"
		reason, bridge := s.bridgeVerdict(r, &tunnelLog{}, tc.host, []byte(tc.first))
		if reason != tc.reason || bridge != tc.bridge {
			t.Errorf("%s with %q: (%q, %v), want (%q, %v)", tc.host, tc.first, reason, bridge, tc.reason, tc.bridge)
		}
	}
}

// On the transparent listener the client names the SNI but not the address dialled,
// so an SNI naming a passthrough host does not get a tunnel to another address.
func TestFailClosed_TransparentSNIDoesNotPickPassthrough(t *testing.T) {
	ln := listenSniffable()
	if ln == nil {
		t.Skip("transparent bridge test needs a free sniffable port (8443 or 8080)")
	}
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	_ = origin.Listener.Close()
	origin.Listener = ln
	origin.StartTLS()
	defer origin.Close()
	dst := strings.TrimPrefix(origin.URL, "https://")

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	srv := connectServer(t, store, failClosedEngine(t, portOf(dst), nil, []string{"pinned.invalid"}))

	clientSide, serverSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.HandleTransparentConn(serverSide, dst)
	}()
	tc := tls.Client(clientSide, &tls.Config{ServerName: "pinned.invalid", InsecureSkipVerify: true})
	if err := tc.Handshake(); err == nil {
		if leaf := tc.ConnectionState().PeerCertificates[0]; bytes.Equal(leaf.Raw, origin.Certificate().Raw) {
			t.Errorf("the client reached %s through an unread tunnel", dst)
		}
	}
	_ = tc.Close()
	<-done
}
