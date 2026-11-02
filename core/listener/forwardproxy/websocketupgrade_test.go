package forwardproxy

// #1488 end to end: a WebSocket upgrade crossing the forward proxy — plain and
// TLS-bridged — is forwarded, the 101 is spliced, frames flow both ways, and the
// audit shows one event: the request row plus its paired 101 response row.

import (
	"bufio"
	"bytes"
	"crypto/sha1" //nolint:gosec // RFC 6455 §1.3 mandates SHA-1 over key+GUID for the accept token; a sanity check, not a secret
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// wsGUID is the fixed GUID RFC 6455 §1.3 folds into the accept computation.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// The RFC 6455 §1.3 sample handshake pair. Using the spec's own key means the
// origin's accept is assertable against a published constant, which is what
// proves the 101 the client read is the origin's own answer relayed unmodified
// rather than something the proxy synthesized.
const (
	rfc6455Key    = "dGhlIHNhbXBsZSBub25jZQ=="
	rfc6455Accept = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
)

func wsAccept(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// wsEchoHandler is a minimal WebSocket echo endpoint: a real 101 (accept
// computed per RFC 6455) followed by a byte-level echo of everything after it.
// No frame parsing — the proxy under test must not parse them either, and a
// byte-level echo is what proves the splice is transparent in both directions.
func wsEchoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.Header.Get("Upgrade") != "websocket" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "expected a version-13 websocket upgrade", http.StatusBadRequest)
		return
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: %s\r\n\r\n", wsAccept(r.Header.Get("Sec-WebSocket-Key")))
	if err := brw.Flush(); err != nil {
		return
	}
	// The echo: client → origin → client, one copy. Reads from brw so bytes
	// buffered past the request line are not lost — the same rule the proxy's
	// own splice follows.
	_, _ = io.Copy(conn, brw)
}

// maskedTextFrame builds one masked RFC 6455 text frame — masking is mandatory
// client-to-server — so the exchange after the 101 has the shape of a real
// WebSocket's. A byte-level echo returns the frame unchanged, so the test
// asserts the exact bytes back.
func maskedTextFrame(payload string) []byte {
	p := []byte(payload)
	frame := []byte{0x81, 0x80 | byte(len(p))} // FIN + text opcode; MASK set, 7-bit length
	mask := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	frame = append(frame, mask[:]...)
	for i, c := range p {
		frame = append(frame, c^mask[i%4])
	}
	return frame
}

// writeUpgradeRequest sends the upgrade GET by hand: no stock Go client will
// both speak to a raw TCP peer and surrender its connection after a 101, which
// is exactly the behaviour under test. target is the request target as the
// receiving server must see it — absolute-form through the plain forward proxy,
// origin-form on the bridged connection — and host the Host header.
func writeUpgradeRequest(w io.Writer, target, host string) {
	fmt.Fprintf(w, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", target, host, rfc6455Key)
}

// echoRound sends one masked frame over conn and reads what comes back on br,
// asserting the echo is byte-identical. Used by both the plain and bridged
// tests, after each has read the 101 its own way.
func echoRound(t *testing.T, conn io.Writer, br io.Reader) {
	t.Helper()
	frame := maskedTextFrame("hello through the proxy")
	if _, err := conn.Write(frame); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	echo := make([]byte, len(frame))
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(echo, frame) {
		t.Errorf("echo = %x, want the frame back unchanged: %x", echo, frame)
	}
}

// waitForUpgradedRows polls the store until the 101's response row arrives.
// That row is recorded only after BOTH splice directions end — i.e. after the
// client closes — so waiting for event COUNT is not enough: the tunnel-open and
// request rows land before client.Do even returns, and a count-based poll would
// race the splice's teardown.
func waitForUpgradedRows(t *testing.T, store *session.Store) []pipeline.SessionEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		v := store.View(session.DefaultSessionID)
		if v != nil {
			for _, ev := range v.Events {
				if ev.Phase == pipeline.SessionResponse && ev.StatusCode == http.StatusSwitchingProtocols {
					return v.Events
				}
			}
		}
		if time.Now().After(deadline) {
			if v == nil {
				t.Fatal("no events recorded within 5s")
			}
			for _, ev := range v.Events {
				t.Logf("  phase=%v tunnel=%v path=%q status=%d", ev.Phase, ev.Tunnel, ev.HTTPPath, ev.StatusCode)
			}
			t.Fatalf("no 101 response row within 5s; %d events recorded", len(v.Events))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// assertUpgradedRows checks the audit shape #1488 promises: a request row with
// no strip marker, paired by RequestID with a 101 response row carrying both
// directions' byte counts — the one response row that does, like a tunnel
// close, because after the 101 the proxy does not parse what crosses.
func assertUpgradedRows(t *testing.T, events []pipeline.SessionEvent, path string) {
	t.Helper()
	var req *pipeline.SessionEvent
	var resp *pipeline.SessionEvent
	for i := range events {
		ev := &events[i]
		switch {
		case ev.Phase == pipeline.SessionRequest && !ev.Tunnel && ev.HTTPPath == path:
			if req == nil {
				req = ev
			}
		case ev.Phase == pipeline.SessionResponse && ev.StatusCode == http.StatusSwitchingProtocols:
			if resp == nil {
				resp = ev
			}
		}
	}
	if req == nil {
		for i := range events {
			t.Logf("  phase=%v tunnel=%v path=%q status=%d", events[i].Phase, events[i].Tunnel, events[i].HTTPPath, events[i].StatusCode)
		}
		t.Fatalf("no request row for %q", path)
	}
	if resp == nil {
		t.Fatalf("no 101 response row among %d events", len(events))
	}
	if req.UpgradeStripped != "" {
		t.Errorf("request row carries UpgradeStripped=%q — a forwarded upgrade must not be marked stripped", req.UpgradeStripped)
	}
	if req.RequestID != resp.RequestID {
		t.Errorf("request row id %q does not pair with the 101 row id %q", req.RequestID, resp.RequestID)
	}
	if resp.BytesUp == 0 || resp.BytesDown == 0 {
		t.Errorf("101 row bytes: up=%d down=%d — both directions carried the frame, both must be counted", resp.BytesUp, resp.BytesDown)
	}
	// A cleanly closed session carries NO error. The counterpart assertion lives
	// in TestForwardProxy_WebSocketBrokenDirectionDoesNotStrandTheOther, which
	// requires one; both halves are needed, because a marker that is always set
	// distinguishes nothing and would make every WebSocket read as broken.
	if resp.Error != nil {
		t.Errorf("101 row carries error %+v on a clean session — the marker must mean a relay that failed", resp.Error)
	}
}

// THE PLAIN PATH. A ws:// upgrade arrives as an absolute-form GET on the
// forward proxy's ordinary listener; the 101 comes back over the same
// connection, hijacked and spliced, and the echo crosses both ways through the
// proxy's own process. The done-when of #1488, on the non-intercepted side.
func TestForwardProxy_WebSocketEcho(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	origin := httptest.NewServer(http.HandlerFunc(wsEchoHandler))
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}

	p, err := pipeline.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           http.DefaultClient,
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	writeUpgradeRequest(conn, origin.URL+"/ws", originURL.Host)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d, want 101", resp.StatusCode)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != rfc6455Accept {
		t.Errorf("Sec-WebSocket-Accept = %q, want the RFC 6455 sample %q — the 101 must be the origin's own, relayed unmodified", got, rfc6455Accept)
	}

	echoRound(t, conn, br)

	// Closing is what ends the splice's two copies, and the 101 row records
	// after they both do.
	_ = conn.Close()
	assertUpgradedRows(t, waitForUpgradedRows(t, store), "/ws")
}

// THE BRIDGED PATH, the one #1488 is about: the agent CONNECTs, the bridge
// terminates the TLS, and the upgrade GET arrives decrypted. The origin is
// h2-capable on purpose, and the test asserts it still served the upgrade over
// HTTP/1.1 — the load-bearing stdlib behavior the whole forwarding rests on
// (net/http Request.requiresHTTP1 pins a WebSocket upgrade to HTTP/1.1, so the
// bridge's h2-negotiating upstream client needs no upgrade-specific twin). The
// echo crosses two splices (agent↔bridge and bridge↔origin) in each direction.
func TestConnectBridge_WebSocketEcho(t *testing.T) {
	st := newBridgeStack(t)

	tconn := st.connect(t)
	writeUpgradeRequest(tconn, "/ws", st.originHostPort)
	br := bufio.NewReader(tconn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read upgrade response over the bridge: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d, want 101 through the bridge", resp.StatusCode)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != rfc6455Accept {
		t.Errorf("Sec-WebSocket-Accept = %q, want the RFC 6455 sample %q — the 101 must be the origin's own, relayed unmodified", got, rfc6455Accept)
	}

	echoRound(t, tconn, br)

	_ = tconn.Close()
	if proto := st.upgradeProto(); proto != "HTTP/1.1" {
		t.Errorf("the origin served the upgrade over %s — net/http should have pinned the exchange to HTTP/1.1 (Request.requiresHTTP1)", proto)
	}
	events := waitForUpgradedRows(t, st.store)
	// The bridged tunnel's open rides along with the request row, so three
	// rows: tunnel open, request, 101.
	var tunnelOpen bool
	for _, ev := range events {
		if ev.Tunnel && ev.Phase == pipeline.SessionRequest {
			tunnelOpen = true
		}
	}
	if !tunnelOpen {
		t.Errorf("no tunnel-open row among %d events — a bridged upgrade's tunnel must still be audited", len(events))
	}
	assertUpgradedRows(t, events, "/ws")
}

// The CONNECT-host check applies to the upgrade request too (MOCA MI1 §8.5
// item 5): an upgrade GET whose Host names another authority than its tunnel's
// CONNECT target is refused 421 before anything is forwarded.
func TestConnectBridge_WebSocketMisdirectedHost(t *testing.T) {
	st := newBridgeStack(t)

	tconn := st.connect(t)
	writeUpgradeRequest(tconn, "/ws", "other.example:443")
	resp, err := http.ReadResponse(bufio.NewReader(tconn), nil)
	if err != nil {
		t.Fatalf("read response over the bridge: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("misdirected upgrade status = %d, want 421", resp.StatusCode)
	}
}

// bridgeStack is the CONNECT-bridge scaffolding the WebSocket tests share: an
// h2-capable TLS origin running the echo handler, a bridge engine, a forward
// proxy wired to it, and the session store the rows land in.
type bridgeStack struct {
	store          *session.Store
	proxy          *httptest.Server
	originHostPort string
	engine         *tlsbridge.Engine

	// protoMu guards originProto, written by the origin's handler goroutine
	// and read by the test after the response — a response arriving over a
	// socket is not a happens-before edge Go's memory model recognises.
	protoMu     sync.Mutex
	originProto string
}

// upgradeProto reports the protocol the origin served the upgrade over,
// recorded by the handler below. Empty when the origin never saw an upgrade
// request.
func (st *bridgeStack) upgradeProto() string {
	st.protoMu.Lock()
	defer st.protoMu.Unlock()
	return st.originProto
}

func newBridgeStack(t *testing.T) *bridgeStack {
	t.Helper()

	st := &bridgeStack{}
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" {
			st.protoMu.Lock()
			st.originProto = r.Proto
			st.protoMu.Unlock()
		}
		wsEchoHandler(w, r)
	}))
	origin.EnableHTTP2 = true
	origin.StartTLS()

	originURL, err := url.Parse(origin.URL)
	if err != nil {
		origin.Close()
		t.Fatalf("parse origin URL: %v", err)
	}

	originCAPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: origin.Certificate().Raw,
	})

	src, err := tlsbridge.NewEphemeralSource()
	if err != nil {
		origin.Close()
		t.Fatalf("NewEphemeralSource: %v", err)
	}
	minter := tlsbridge.NewMinter(src, tlsbridge.MinterOpts{})
	up, err := tlsbridge.NewUpstreamClient(originCAPEM, false)
	if err != nil {
		origin.Close()
		t.Fatalf("NewUpstreamClient: %v", err)
	}
	engine := &tlsbridge.Engine{
		Decision: mustBridgeDecision(t, portOf(originURL.Host)),
		Term:     tlsbridge.NewTerminator(minter),
		Skip:     tlsbridge.NewSkipSet(),
		Upstream: up,
		CAPEM:    src.CACertPEM(),
	}

	store := session.New(5*time.Minute, 100, 0)
	p, err := pipeline.New(nil)
	if err != nil {
		origin.Close()
		store.Close()
		t.Fatalf("pipeline.New: %v", err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           http.DefaultClient,
		TLSBridge:        engine,
	}
	proxy := httptest.NewServer(srv.Handler())

	t.Cleanup(func() {
		proxy.Close()
		store.Close()
		origin.Close()
	})
	st.store = store
	st.proxy = proxy
	st.originHostPort = originURL.Host
	st.engine = engine
	return st
}

// connect does the CONNECT and the agent-side TLS handshake, returning the
// decrypted connection the test speaks HTTP/1.1 on — the same sequence
// TestConnectBridge walks, kept here so each WebSocket test starts at the
// upgrade GET.
func (st *bridgeStack) connect(t *testing.T) *tls.Conn {
	t.Helper()
	proxyURL, err := url.Parse(st.proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	rawConn, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	_ = rawConn.SetDeadline(time.Now().Add(10 * time.Second))

	connectReq, err := http.NewRequest(http.MethodConnect, "//"+st.originHostPort, nil)
	if err != nil {
		t.Fatalf("new CONNECT request: %v", err)
	}
	connectReq.Host = st.originHostPort
	connectReq.URL.Host = st.originHostPort
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
	if !pool.AppendCertsFromPEM(st.engine.CAPEM) {
		t.Fatalf("failed to append bridge CA PEM to pool")
	}
	tconn := tls.Client(&bufferedConn{Conn: rawConn, r: br}, &tls.Config{
		ServerName: hostOnly(st.originHostPort),
		RootCAs:    pool,
		NextProtos: []string{"http/1.1"},
	})
	_ = tconn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tconn.Handshake(); err != nil {
		t.Fatalf("agent-side TLS handshake through the bridge failed: %v", err)
	}
	return tconn
}

// A FAILED SPLICE MUST NOT LEAK THE ORIGIN'S CONNECTION. relayUpgradedWebSocket
// owns resp.Body once the origin answered 101 — serveOutbound deliberately skips
// its Close defer, because closing the body would close the WebSocket — and the
// goroutine that closes the origin on context-done is armed only after the
// hijack succeeds. So the two early returns before it have to close the origin
// themselves, or the origin is left holding a connection it believes is an
// established WebSocket while the client already has a 502. Nothing would ever
// come back for it; it sits until the origin's own idle timeout reaps it.
//
// Driven through the hijack-failure path, which is reachable with a plain
// ResponseWriter: httptest.NewRecorder does not implement http.Hijacker. The
// non-writable-body path cannot be reached through net/http's own transport at
// all (it hands a 101 an io.ReadWriteCloser), so it is covered by inspection and
// the same close, not by a test.
//
// The assertion is the ORIGIN's view, not ours: its handler hijacks and blocks
// in a copy, which returns only when our end of that connection closes. So "the
// copy returned" is the only honest evidence that we released it (#1549 review).
func TestForwardProxy_WebSocketSpliceFailureClosesTheOrigin(t *testing.T) {
	copyReturned := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("origin could not hijack: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: %s\r\n\r\n", wsAccept(r.Header.Get("Sec-WebSocket-Key")))
		if err := brw.Flush(); err != nil {
			return
		}
		// Blocks until the proxy's end of this connection is closed.
		_, _ = io.Copy(io.Discard, brw)
		close(copyReturned)
	}))
	defer origin.Close()

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	p, err := pipeline.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           origin.Client(),
	}

	// httptest.NewRecorder is not an http.Hijacker, so the hijack fails after
	// the origin has already answered 101 — the state this test is about.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, origin.URL+"/ws", nil)
	req.Host = req.URL.Host
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Key", rfc6455Key)
	req.Header.Set("Sec-WebSocket-Version", "13")

	srv.serveOutbound(rec, req, nil)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("client status = %d, want 502 — a failed splice is a bad gateway", rec.Code)
	}
	select {
	case <-copyReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("the origin's copy never returned: its connection was left open after the splice failed")
	}
}

// A BROKEN DIRECTION MUST NOT STRAND THE OTHER. The splice waits for both
// directions so it can report both byte counts, which means a direction that
// ends in an ERROR has to tear both ends down before that wait — a clean EOF
// propagates as CloseWrite and releases its peer, but an error carries no such
// signal, so the peer sits in Read with nothing coming.
//
// The cost is the audit, not just a goroutine: the 101 row is recorded after
// both directions end, so a stranded direction means the upgrade is never
// evented at all. Measured before the fix — an origin that RSTs mid-session
// against a client that has gone quiet leaves the row unwritten past any
// deadline; after it, the row lands in ~0.1s (#1549 review).
//
// The origin here is a raw listener rather than an httptest server because the
// test needs SetLinger(0) — a Close that sends RST — to make one direction fail
// rather than EOF. The client is deliberately silent after reading the 101: that
// is what keeps client→origin blocked, so the test observes the strand and not
// merely a slow teardown.
func TestForwardProxy_WebSocketBrokenDirectionDoesNotStrandTheOther(t *testing.T) {
	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = origin.Close() }()
	go func() {
		c, err := origin.Accept()
		if err != nil {
			return
		}
		req, err := http.ReadRequest(bufio.NewReader(c))
		if err != nil {
			return
		}
		fmt.Fprintf(c, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: %s\r\n\r\n", wsAccept(req.Header.Get("Sec-WebSocket-Key")))
		// RST rather than FIN, so the proxy's origin→client copy ends with an
		// error instead of the clean EOF that would CloseWrite its peer.
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		time.Sleep(100 * time.Millisecond)
		_ = c.Close()
	}()

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	p, err := pipeline.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           http.DefaultClient,
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	cc, err := net.Dial("tcp", proxy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cc.Close() }()
	writeUpgradeRequest(cc, "http://"+origin.Addr().String()+"/ws", origin.Addr().String())
	resp, err := http.ReadResponse(bufio.NewReader(cc), nil)
	if err != nil {
		t.Fatalf("reading the 101: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	// Then say nothing at all: client→origin stays blocked on this read.

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v := store.View(session.DefaultSessionID); v != nil {
			for _, e := range v.Events {
				if e.Phase == pipeline.SessionResponse && e.StatusCode == http.StatusSwitchingProtocols {
					// And the row says the session ended badly. Without this the
					// only symptom of a WebSocket that broke mid-session was
					// truncated byte counts, so an audit read it as a clean
					// upgrade — one event per upgrade that cannot tell the two
					// outcomes apart only half keeps the contract (#1549 review).
					if e.Error == nil {
						t.Fatal("the 101 row carries no error after a broken splice; a session that died " +
							"reads as one that closed cleanly")
					}
					if e.Error.Kind != "proxy_error" {
						t.Errorf("error.kind = %q, want proxy_error — the relay's failure is ours", e.Error.Kind)
					}
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the 101 response row was never recorded: a broken splice direction stranded the other, so the upgrade went unaudited")
}

// TestForwardProxy_UpgradeWithoutConnectionTokenIsStripped: an "Upgrade:
// websocket" with no Connection: upgrade token is NOT an upgrade request (RFC
// 9110 §7.8 — a recipient ignores Upgrade unless Connection lists it), so it
// must take the ordinary request path: the pair is stripped, the origin sees a
// plain GET, and the client gets that GET's response.
//
// The regression this pins is an escalation, not a missed feature.
// stripForUpstream SETS the normalized Connection/Upgrade pair for anything it
// treats as an upgrade, so keying on Upgrade alone had the proxy synthesize a
// handshake the client never sent — and when the origin answered 101, hijack
// the client's connection and splice it. A client waiting for an ordinary HTTP
// response instead got an opaque byte pipe whose frames no plugin inspects.
// Measured end to end: the client read the origin's post-101 bytes (#1549
// review).
func TestForwardProxy_UpgradeWithoutConnectionTokenIsStripped(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	seen := make(chan http.Header, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		// An origin that WOULD upgrade if asked, so the only thing keeping this
		// request on the ordinary path is the proxy's own refusal to forge the pair.
		if r.Header.Get("Upgrade") != "" {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("origin: no hijacker")
				return
			}
			c, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("origin hijack: %v", err)
				return
			}
			defer func() { _ = c.Close() }()
			_, _ = c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\n" +
				"Upgrade: websocket\r\nSec-WebSocket-Accept: " + wsAccept(rfc6455Key) + "\r\n\r\nORIGINBYTES"))
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ordinary")
	}))
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}

	p, err := pipeline.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Sessions: store, Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	// Note the absent Connection header — that is the whole point.
	fmt.Fprintf(conn, "GET %s/ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		origin.URL, originURL.Host, rfc6455Key)

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a bare Upgrade must take the ordinary path, not be spliced", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ordinary" {
		t.Errorf("body = %q, want %q", body, "ordinary")
	}
	select {
	case h := <-seen:
		if got := h.Get("Upgrade"); got != "" {
			t.Errorf("origin saw Upgrade = %q, want it stripped — the proxy must not forge a handshake", got)
		}
		if got := h.Get("Connection"); got != "" {
			t.Errorf("origin saw Connection = %q, want it stripped", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("origin never received the request")
	}
}

// TestForwardProxy_RefusesA101ToAnotherProtocol: a 101 is not self-describing —
// it says "switching", and its Upgrade header says to what. An origin that
// answers a WebSocket handshake by switching to something else (h2c here) must
// not be spliced: net/http's ReverseProxy compares the response's upgrade type
// against the request's and refuses a mismatch, and this relay is modelled on
// it, but the comparison was the part left out.
//
// Refused BEFORE the hijack, so a 502 is still writable and the client learns
// the upgrade failed rather than being handed a pipe speaking a protocol it
// never negotiated (#1549 review).
func TestForwardProxy_RefusesA101ToAnotherProtocol(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("origin: no hijacker")
			return
		}
		c, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("origin hijack: %v", err)
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\n" +
			"Upgrade: h2c\r\n\r\nNOTWEBSOCKET"))
	}))
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}

	p, err := pipeline.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Sessions: store, Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	writeUpgradeRequest(conn, origin.URL+"/ws", originURL.Host)

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 — a 101 to another protocol must not be spliced", resp.StatusCode)
	}

	// And the failure is audited: a request row with no pair reads as still in
	// flight, which is the thing recordSpliceFailure exists to prevent.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v := store.View(session.DefaultSessionID); v != nil {
			for _, e := range v.Events {
				if e.Phase == pipeline.SessionResponse && e.StatusCode == http.StatusBadGateway {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no 502 response row recorded for the refused upgrade")
}
