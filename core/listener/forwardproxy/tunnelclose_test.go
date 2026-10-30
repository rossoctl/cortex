package forwardproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// Tests for #1199: an opaque tunnel records a response row when it closes, and a failed
// dial records a request and a 502 instead of nothing.

const (
	tunnelPing = "ping-12345"      // client → destination
	tunnelPong = "pong-1234567890" // destination → client
)

// pingPongOrigin accepts one connection, reads tunnelPing, answers tunnelPong and closes.
// Closing from the origin side is what ends the tunnel, so a test knows the exact byte
// counts the close row must report.
func pingPongOrigin(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen origin: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		buf := make([]byte, len(tunnelPing))
		if _, err := io.ReadFull(c, buf); err != nil {
			return
		}
		_, _ = c.Write([]byte(tunnelPong))
	}()
	return ln.Addr().String()
}

// closedAddr is a loopback address nothing listens on, so dialing it is refused at once.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// connectProxy serves s over HTTP and returns its address plus a channel that receives
// once per handled request — for a CONNECT, only after the tunnel has ended, because
// handleConnect blocks for the tunnel's whole life. That is what lets a test assert on
// the close row without sleeping.
func connectProxy(t *testing.T, s *Server) (string, <-chan struct{}) {
	t.Helper()
	done := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleRequest(w, r)
		done <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), done
}

// sendConnect writes a CONNECT for target and reads the proxy's answer.
func sendConnect(t *testing.T, proxyAddr, target string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	raw, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	return raw, br, resp
}

// pingPong runs the tunnelPing/tunnelPong exchange over an open tunnel and waits for the
// origin to close it.
func pingPong(t *testing.T, raw net.Conn, br *bufio.Reader) {
	t.Helper()
	if _, err := raw.Write([]byte(tunnelPing)); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	got, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read through tunnel: %v", err)
	}
	if string(got) != tunnelPong {
		t.Fatalf("read %q through the tunnel, want %q", got, tunnelPong)
	}
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the proxy never finished handling the connection")
	}
}

// tunnelRows returns sid's tunnel rows split by phase.
func tunnelRows(store *session.Store, sid string) (opens, closes []pipeline.SessionEvent) {
	v := store.View(sid)
	if v == nil {
		return nil, nil
	}
	for _, e := range v.Events {
		if !e.Tunnel {
			continue
		}
		switch e.Phase {
		case pipeline.SessionRequest:
			opens = append(opens, e)
		case pipeline.SessionResponse:
			closes = append(closes, e)
		}
	}
	return opens, closes
}

// onePair asserts sid holds exactly one tunnel open and one close, paired by RequestID,
// and returns them.
func onePair(t *testing.T, store *session.Store, sid string) (open, closed pipeline.SessionEvent) {
	t.Helper()
	opens, closes := tunnelRows(store, sid)
	if len(opens) != 1 || len(closes) != 1 {
		t.Fatalf("session %q: %d tunnel opens and %d closes, want exactly 1 of each", sid, len(opens), len(closes))
	}
	open, closed = opens[0], closes[0]
	if open.RequestID == "" || closed.RequestID != open.RequestID {
		t.Errorf("close RequestID %q does not pair with open %q; agentop pairs the two by it",
			closed.RequestID, open.RequestID)
	}
	return open, closed
}

func TestTunnel_CountsBytesEachWay(t *testing.T) {
	agent, clientSide := net.Pipe()
	upstreamSide, origin := net.Pipe()

	go func() {
		defer func() { _ = origin.Close() }()
		buf := make([]byte, len(tunnelPing))
		if _, err := io.ReadFull(origin, buf); err != nil {
			return
		}
		_, _ = origin.Write([]byte(tunnelPong))
	}()
	go func() {
		_, _ = agent.Write([]byte(tunnelPing))
		_, _ = io.Copy(io.Discard, agent)
	}()

	up, down := tunnel(clientSide, upstreamSide)
	if up != int64(len(tunnelPing)) || down != int64(len(tunnelPong)) {
		t.Errorf("tunnel counted up=%d down=%d, want up=%d down=%d",
			up, down, len(tunnelPing), len(tunnelPong))
	}
}

// The open's own at-most-once test is TestTunnelLog_OpenRecordsAtMostOnce; this is the
// close's, plus the reason it inherits so the close row explains itself too.
func TestTunnelLog_CloseRecordsAtMostOnce(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}
	tl := s.newTunnelLog(&pipeline.Context{Direction: pipeline.Outbound, Host: "example.com:443"}, false)

	tl.open(pipeline.TunnelSkipCached)
	tl.close(http.StatusOK, nil, 1, 2)
	tl.close(http.StatusBadGateway, nil, 3, 4) // a second close must be swallowed

	_, closed := onePair(t, store, session.DefaultSessionID)
	if closed.StatusCode != http.StatusOK || closed.BytesUp != 1 || closed.BytesDown != 2 {
		t.Errorf("close = {status %d, up %d, down %d}, want the FIRST close's {200, 1, 2}",
			closed.StatusCode, closed.BytesUp, closed.BytesDown)
	}
	if closed.TunnelReason != pipeline.TunnelSkipCached {
		t.Errorf("close reason = %q, want the open's %q", closed.TunnelReason, pipeline.TunnelSkipCached)
	}
}

// A close with no open would be a response row pairing with nothing.
func TestTunnelLog_CloseWithoutOpenRecordsNothing(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}
	s.newTunnelLog(&pipeline.Context{Direction: pipeline.Outbound, Host: "h:443"}, false).close(http.StatusOK, nil, 1, 1)

	if v := store.View(session.DefaultSessionID); v != nil && len(v.Events) != 0 {
		t.Errorf("a close with no open recorded %d event(s); want none", len(v.Events))
	}
}

// The issue's own case: an opaque tunnel that worked must get STATUS, DURATION and the
// bytes it carried, on a row paired with the open.
func TestHandleConnect_OpaqueTunnelRecordsItsClose(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := connectServer(t, store, nil) // no bridge: bridge-disabled
	proxyAddr, done := connectProxy(t, s)
	target := pingPongOrigin(t)

	raw, br, resp := sendConnect(t, proxyAddr, target)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	pingPong(t, raw, br)
	waitDone(t, done)

	open, closed := onePair(t, store, session.DefaultSessionID)
	if open.TunnelReason != pipeline.TunnelBridgeDisabled {
		t.Errorf("open reason = %q, want %q", open.TunnelReason, pipeline.TunnelBridgeDisabled)
	}
	if closed.StatusCode != http.StatusOK || closed.Error != nil {
		t.Errorf("close = {status %d, error %+v}, want {200, nil}", closed.StatusCode, closed.Error)
	}
	if closed.Duration <= 0 {
		t.Errorf("close duration = %v, want how long the tunnel stayed open", closed.Duration)
	}
	if closed.BytesUp != int64(len(tunnelPing)) || closed.BytesDown != int64(len(tunnelPong)) {
		t.Errorf("close bytes = up %d down %d, want up %d down %d",
			closed.BytesUp, closed.BytesDown, len(tunnelPing), len(tunnelPong))
	}
	if closed.Host != target || closed.HTTPMethod != http.MethodConnect || closed.TunnelReason != open.TunnelReason {
		t.Errorf("close = {host %q, method %q, reason %q}, want {%q, CONNECT, %q}",
			closed.Host, closed.HTTPMethod, closed.TunnelReason, target, open.TunnelReason)
	}
}

// The close is recorded minutes after the open for a long tunnel, by which time the
// session that spoke last may be another one entirely. It must land where the open did.
func TestHandleConnect_CloseLandsInTheOpensSession(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	store.Append("sess-A", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest})
	s := connectServer(t, store, nil)
	proxyAddr, done := connectProxy(t, s)

	raw, br, _ := sendConnect(t, proxyAddr, pingPongOrigin(t))
	waitOpen(t, store, "sess-A")
	// Another session speaks while the tunnel is open.
	store.Append("sess-B", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest})
	speakerAt := updatedAt(t, store, "sess-A")

	pingPong(t, raw, br)
	waitDone(t, done)

	onePair(t, store, "sess-A")
	if opens, closes := tunnelRows(store, "sess-B"); len(opens)+len(closes) != 0 {
		t.Errorf("sess-B, which spoke while the tunnel was open, got %d tunnel rows; want 0", len(opens)+len(closes))
	}
	// Landing in sess-A is not sess-A speaking. Every reader of "who spoke last" —
	// ActiveSession's fallback, the session list's order, eviction, affinity's newest
	// session — must still see sess-B.
	if got := store.ActiveSession(); got != "sess-B" {
		t.Errorf("ActiveSession() = %q after the close; want sess-B, which spoke last", got)
	}
	if got := updatedAt(t, store, "sess-A"); !got.Equal(speakerAt) {
		t.Errorf("sess-A UpdatedAt moved from %v to %v on the close; a close is not activity", speakerAt, got)
	}
	if list := store.ListSessions(); len(list) == 0 || list[0].ID != "sess-B" {
		t.Errorf("session list leads with %+v after the close; want sess-B", list)
	}
}

// waitOpen waits for sid to hold exactly one tunnel open: handleConnect records it on
// its own goroutine, after answering the CONNECT.
func waitOpen(t *testing.T, store *session.Store, sid string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if opens, _ := tunnelRows(store, sid); len(opens) == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the tunnel open never landed in %s", sid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func updatedAt(t *testing.T, store *session.Store, sid string) time.Time {
	t.Helper()
	for _, s := range store.ListSessions() {
		if s.ID == sid {
			return s.UpdatedAt
		}
	}
	t.Fatalf("no session %q", sid)
	return time.Time{}
}

// A first-turn tunnel opens under the default bucket, and the A2A path rekeys that bucket
// to the conversation's contextId while the tunnel is still open. The close must follow its
// open there, not re-create a default bucket for the next conversation's rekey to inherit.
func TestHandleConnect_CloseFollowsARekeyedOpen(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := connectServer(t, store, nil)
	proxyAddr, done := connectProxy(t, s)

	raw, br, _ := sendConnect(t, proxyAddr, pingPongOrigin(t))
	waitOpen(t, store, session.DefaultSessionID)
	store.Rekey(session.DefaultSessionID, "ctx-1")
	// The next conversation's first turn opens a default bucket of its own.
	store.Append(session.DefaultSessionID, pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Inbound, Phase: pipeline.SessionRequest})

	pingPong(t, raw, br)
	waitDone(t, done)

	onePair(t, store, "ctx-1")
	if opens, closes := tunnelRows(store, session.DefaultSessionID); len(opens)+len(closes) != 0 {
		t.Errorf("the new default bucket got %d tunnel rows; want 0 — they belong to ctx-1", len(opens)+len(closes))
	}
}

// A session evicted while its tunnel was open is gone, open row included. Its close would
// be a response pairing with nothing, and re-creating the session to hold it would evict
// another one in its place.
func TestHandleConnect_CloseOfAnEvictedSessionRecordsNothing(t *testing.T) {
	store := session.New(5*time.Minute, 100, 1)
	defer store.Close()
	store.Append("sess-A", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest})
	s := connectServer(t, store, nil)
	proxyAddr, done := connectProxy(t, s)

	raw, br, _ := sendConnect(t, proxyAddr, pingPongOrigin(t))
	waitOpen(t, store, "sess-A")
	store.Append("sess-B", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest})
	if store.View("sess-A") != nil {
		t.Fatal("sess-A survived; the test needs it evicted while the tunnel is open")
	}

	pingPong(t, raw, br)
	waitDone(t, done)

	if v := store.View("sess-A"); v != nil {
		t.Errorf("the close re-created evicted sess-A holding %d event(s); want it left gone", len(v.Events))
	}
	if store.View("sess-B") == nil {
		t.Error("sess-B was evicted to make room for the close of a session that no longer exists")
	}
}

// Today an unreachable destination leaves no trace. It must record the attempt and the
// 502 the client was sent, with the dial error saying why.
func TestHandleConnect_DialFailureRecordsA502(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := connectServer(t, store, nil)
	proxyAddr, done := connectProxy(t, s)
	target := closedAddr(t)

	_, _, resp := sendConnect(t, proxyAddr, target)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("CONNECT status = %d, want 502", resp.StatusCode)
	}
	waitDone(t, done)

	open, closed := onePair(t, store, session.DefaultSessionID)
	if open.TunnelReason != pipeline.TunnelDialFailed {
		t.Errorf("open reason = %q, want %q", open.TunnelReason, pipeline.TunnelDialFailed)
	}
	if closed.StatusCode != http.StatusBadGateway {
		t.Errorf("close status = %d, want 502 — the status the client was sent", closed.StatusCode)
	}
	if closed.Error == nil || closed.Error.Kind != "dial_failed" || closed.Error.Message == "" {
		t.Errorf("close error = %+v, want kind dial_failed with the dial error as its message", closed.Error)
	}
	if closed.BytesUp != 0 || closed.BytesDown != 0 {
		t.Errorf("a tunnel that never opened reported bytes: up %d down %d", closed.BytesUp, closed.BytesDown)
	}
}

// bridgeEngine terminates TLS on originPort. upstreamCA is what the bridge's own
// upstream client trusts: the origin's certificate for a bridge that works, nil for one
// whose upstream verification fails and falls open to a plain tunnel.
func bridgeEngine(t *testing.T, originPort int, upstreamCA []byte) *tlsbridge.Engine {
	t.Helper()
	src, err := tlsbridge.NewEphemeralSource()
	if err != nil {
		t.Fatalf("NewEphemeralSource: %v", err)
	}
	up, err := tlsbridge.NewUpstreamClient(upstreamCA, false)
	if err != nil {
		t.Fatalf("NewUpstreamClient: %v", err)
	}
	return &tlsbridge.Engine{
		Decision: mustBridgeDecision(t, originPort),
		Term:     tlsbridge.NewTerminator(tlsbridge.NewMinter(src, tlsbridge.MinterOpts{})),
		Skip:     tlsbridge.NewSkipSet(),
		Upstream: up,
		CAPEM:    src.CACertPEM(),
	}
}

// The bridge fall-open re-dial IS an opaque tunnel, so it closes like one.
func TestHandleConnect_FallOpenTunnelRecordsItsClose(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer origin.Close()
	target := strings.TrimPrefix(origin.URL, "https://")

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := connectServer(t, store, bridgeEngine(t, portOf(target), nil)) // trusts nothing: verify fails
	proxyAddr, done := connectProxy(t, s)

	raw, br, _ := sendConnect(t, proxyAddr, target)
	if _, err := raw.Write(tlsRecordHead); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _ = io.ReadAll(br) // the origin rejects the empty record and hangs up
	waitDone(t, done)

	open, closed := onePair(t, store, session.DefaultSessionID)
	if open.TunnelReason != pipeline.TunnelOriginUnverified {
		t.Errorf("open reason = %q, want %q", open.TunnelReason, pipeline.TunnelOriginUnverified)
	}
	if closed.StatusCode != http.StatusOK || closed.Error != nil {
		t.Errorf("close = {status %d, error %+v}, want {200, nil}", closed.StatusCode, closed.Error)
	}
	if closed.BytesUp != int64(len(tlsRecordHead)) {
		t.Errorf("close BytesUp = %d, want the %d peeked bytes the re-dialled tunnel replayed",
			closed.BytesUp, len(tlsRecordHead))
	}
}

// When the fall-open re-dial itself fails, the client already has its 200. The close row
// keeps that status and says why the tunnel died, and the client's connection is closed
// rather than left hanging.
func TestHandleConnect_FallOpenRedialFailureRecordsTheError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	target := ln.Addr().String()
	gone := make(chan struct{})
	go func() {
		// Accept handleConnect's first dial, then disappear, so the upstream
		// verification and the fall-open re-dial are both refused.
		if c, err := ln.Accept(); err == nil {
			_ = c.Close()
		}
		_ = ln.Close()
		close(gone)
	}()

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := connectServer(t, store, bridgeEngine(t, portOf(target), nil))
	proxyAddr, done := connectProxy(t, s)

	raw, br, _ := sendConnect(t, proxyAddr, target)
	<-gone
	if _, err := raw.Write(tlsRecordHead); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := io.ReadAll(br); err != nil {
		t.Fatalf("read: %v", err)
	}
	waitDone(t, done)

	_, closed := onePair(t, store, session.DefaultSessionID)
	if closed.StatusCode != http.StatusOK {
		t.Errorf("close status = %d, want 200 — the status the client had already been sent", closed.StatusCode)
	}
	if closed.Error == nil || closed.Error.Kind != "dial_failed" {
		t.Errorf("close error = %+v, want kind dial_failed", closed.Error)
	}
}

// bridgingProxy is a proxy whose TLS bridge terminates CONNECTs to a trusted TLS origin.
func bridgingProxy(t *testing.T, store *session.Store) (proxyAddr, target string, bridgeCA []byte, done <-chan struct{}) {
	t.Helper()
	return bridgingProxyWith(t, store, nil)
}

// bridgingProxyWith is bridgingProxy with configure applied to the server before it
// serves anything.
func bridgingProxyWith(t *testing.T, store *session.Store, configure func(*Server)) (proxyAddr, target string, bridgeCA []byte, done <-chan struct{}) {
	t.Helper()
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	// handleConnect dials the origin before it decides to bridge and closes that
	// connection unused, which the origin logs as a TLS handshake error. Expected, and
	// only noise in the test output.
	origin.Config.ErrorLog = log.New(io.Discard, "", 0)
	origin.StartTLS()
	t.Cleanup(origin.Close)
	originCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})
	u, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatalf("parse origin URL: %v", err)
	}
	engine := bridgeEngine(t, portOf(u.Host), originCA)
	s := connectServer(t, store, engine)
	if configure != nil {
		configure(s)
	}
	proxyAddr, done = connectProxy(t, s)
	return proxyAddr, u.Host, engine.CAPEM, done
}

// bridgedTLS completes the agent side of a bridged handshake over an open tunnel.
func bridgedTLS(t *testing.T, raw net.Conn, br *bufio.Reader, target string, bridgeCA []byte) *tls.Conn {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bridgeCA) {
		t.Fatal("bad bridge CA")
	}
	tc := tls.Client(&bufferedConn{Conn: raw, r: br}, &tls.Config{
		ServerName: hostOnly(target), RootCAs: pool, NextProtos: []string{"http/1.1"},
	})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("bridged handshake: %v", err)
	}
	return tc
}

// A bridged tunnel's decrypted requests carry their own responses and agentop folds the
// CONNECT into the first of them, so a close row there would render as an orphan.
func TestHandleConnect_BridgedTunnelWithRequestsRecordsNoClose(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	proxyAddr, target, ca, done := bridgingProxy(t, store)

	raw, br, _ := sendConnect(t, proxyAddr, target)
	tc := bridgedTLS(t, raw, br, target, ca)
	req, _ := http.NewRequest(http.MethodGet, "https://"+hostOnly(target)+"/x", nil)
	go func() { _ = req.Write(tc) }()
	resp, err := http.ReadResponse(bufio.NewReader(tc), req)
	if err != nil {
		t.Fatalf("bridged request: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = tc.Close()
	waitDone(t, done)

	opens, closes := tunnelRows(store, session.DefaultSessionID)
	if len(opens) != 1 || len(closes) != 0 {
		t.Errorf("bridged tunnel with a request: %d opens, %d closes; want 1 and 0", len(opens), len(closes))
	}
}

// ...but a bridged tunnel that carried no request has nothing to fold into, and would
// otherwise be the one tunnel row left looking like it never finished.
func TestHandleConnect_BridgedTunnelWithNoRequestRecordsItsClose(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	proxyAddr, target, ca, done := bridgingProxy(t, store)

	raw, br, _ := sendConnect(t, proxyAddr, target)
	_ = bridgedTLS(t, raw, br, target, ca).Close()
	waitDone(t, done)

	_, closed := onePair(t, store, session.DefaultSessionID)
	if closed.StatusCode != http.StatusOK || closed.Error != nil {
		t.Errorf("close = {status %d, error %+v}, want {200, nil}", closed.StatusCode, closed.Error)
	}
}

// On h2 each request's handler runs on its own goroutine, and ServeConn returns without
// waiting for one to start. So a request can reach the handler after the tunnel's close
// was recorded for serving nothing. Its client has gone — ServeConn cancelled its stream
// before returning — and serving it would put a decrypted request beside a close row that
// says the tunnel carried none.
func TestBridgedHandler_RequestAfterTheCloseRecordsNothing(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := connectServer(t, store, nil)
	tl := s.newTunnelLog(&pipeline.Context{Direction: pipeline.Outbound, Host: "example.com:443"}, false)
	tl.open("")
	tl.finish() // ServeConn returned before any handler started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "https://example.com/x", nil).WithContext(ctx)
	s.bridgedHandler("example.com:443", tl).ServeHTTP(httptest.NewRecorder(), req)

	onePair(t, store, session.DefaultSessionID)
	if v := store.View(session.DefaultSessionID); len(v.Events) != 2 {
		t.Errorf("%d events after a request reached a closed tunnel; want only its open and close", len(v.Events))
	}
}

// The transparent listener records its open before it bridges, so a bridged tunnel there
// has no open left to defer: markBridged must change nothing, and its decrypted requests
// record as plain rows. The first of them still answers the tunnel, so no close follows.
func TestTunnelLog_EagerOpenIsNotDeferred(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}
	tl := s.newTunnelLog(&pipeline.Context{Direction: pipeline.Outbound, Method: http.MethodConnect, Host: "example.com:443"}, false)
	tl.open("")    // HandleTransparentConn, before bridgeServe
	tl.deferOpen() // markBridged, once the forged handshake succeeds

	if !tl.admit() {
		t.Fatal("a request on a live tunnel was refused")
	}
	s.appendOutbound(tl, "sess-1", decryptedRequest("/x"))
	tl.release()
	tl.finish()

	// The eager open went where recordTunnelOpened files it: ActiveSession() was empty
	// when it recorded, so default.
	opens, closes := tunnelRows(store, session.DefaultSessionID)
	if len(opens) != 1 {
		t.Errorf("default holds %d tunnel open(s), want the one recorded before bridging", len(opens))
	}
	if len(closes) != 0 {
		t.Errorf("default holds %d close(s); a tunnel its request answered records none", len(closes))
	}
	v := store.View("sess-1")
	if v == nil || len(v.Events) != 1 || v.Events[0].Tunnel || v.Events[0].HTTPPath != "/x" {
		t.Fatalf("sess-1 = %+v, want only the decrypted request — no second open, no close", v)
	}
}

// A failed forged handshake kills the connection, so its tunnel row gets a close that
// says so rather than looking still open.
func TestBridgeServe_HandshakeFailureRecordsItsClose(t *testing.T) {
	s, store, authority := bridgeForRejectTest(t)
	tl := s.newTunnelLog(&pipeline.Context{Direction: pipeline.Outbound, Host: authority}, false)

	if !s.bridgeServe(rejectingClient(t), authority, hostOnly(authority), tl, false) {
		t.Fatal("bridgeServe returned false; the connection is dead post-forge")
	}

	open, closed := onePair(t, store, session.DefaultSessionID)
	if open.TunnelReason != pipeline.TunnelClientRejectedCA {
		t.Errorf("open reason = %q, want %q", open.TunnelReason, pipeline.TunnelClientRejectedCA)
	}
	if closed.StatusCode != http.StatusOK {
		t.Errorf("close status = %d, want 200 — the CONNECT was answered before the handshake", closed.StatusCode)
	}
	if closed.Error == nil || closed.Error.Kind != "tls_handshake" || closed.Error.Message == "" {
		t.Errorf("close error = %+v, want kind tls_handshake with the handshake error", closed.Error)
	}
}

func TestHandleTransparentConn_RecordsItsClose(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := connectServer(t, store, nil)
	dst := pingPongOrigin(t)

	agent, proxySide := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.HandleTransparentConn(proxySide, dst)
	}()
	_ = agent.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := agent.Write([]byte(tunnelPing)); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _ := io.ReadAll(agent)
	if string(got) != tunnelPong {
		t.Fatalf("read %q, want %q", got, tunnelPong)
	}
	waitClosed(t, done)

	_, closed := onePair(t, store, session.DefaultSessionID)
	if closed.StatusCode != http.StatusOK {
		t.Errorf("close status = %d, want 200", closed.StatusCode)
	}
	if closed.BytesUp != int64(len(tunnelPing)) || closed.BytesDown != int64(len(tunnelPong)) {
		t.Errorf("close bytes = up %d down %d, want up %d down %d",
			closed.BytesUp, closed.BytesDown, len(tunnelPing), len(tunnelPong))
	}
}

// The transparent path sends no status of its own, so its 502 is synthetic — as its
// CONNECT method already is — but the failure must be just as visible as on CONNECT.
func TestHandleTransparentConn_DialFailureRecordsA502(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := connectServer(t, store, nil)

	agent, proxySide := net.Pipe()
	defer func() { _ = agent.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.HandleTransparentConn(proxySide, closedAddr(t))
	}()
	waitClosed(t, done)

	open, closed := onePair(t, store, session.DefaultSessionID)
	if open.TunnelReason != pipeline.TunnelDialFailed {
		t.Errorf("open reason = %q, want %q", open.TunnelReason, pipeline.TunnelDialFailed)
	}
	if closed.StatusCode != http.StatusBadGateway || closed.Error == nil || closed.Error.Kind != "dial_failed" {
		t.Errorf("close = {status %d, error %+v}, want {502, kind dial_failed}", closed.StatusCode, closed.Error)
	}
}

// The transparent twin of TestHandleConnect_FallOpenRedialFailureRecordsTheError. The
// destination was dialled once before the bridge fell open, so the close row is the same
// as CONNECT's for the same failure: 200 with the dial error. 502 is the status of a
// destination never reached, which is what the open's dial-failed reason says.
func TestHandleTransparentConn_FallOpenRedialFailureRecordsTheError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	dst := ln.Addr().String()
	gone := make(chan struct{})
	go func() {
		// Accept the first dial, then disappear, so the re-dial is refused.
		if c, err := ln.Accept(); err == nil {
			_ = c.Close()
		}
		_ = ln.Close()
		close(gone)
	}()
	engine := bridgeEngine(t, portOf(dst), nil)
	// The bridge verifies the origin before forging. Held until the origin has gone, so
	// the verification fails and the re-dial after it is refused, in that order.
	engine.Upstream = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		<-gone
		return nil, errors.New("origin gone")
	})}

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := connectServer(t, store, engine)

	agent, proxySide := net.Pipe()
	defer func() { _ = agent.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.HandleTransparentConn(proxySide, dst)
	}()
	_ = agent.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := agent.Write(tlsRecordHead); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _ = io.ReadAll(agent)
	waitClosed(t, done)

	_, closed := onePair(t, store, session.DefaultSessionID)
	if closed.StatusCode != http.StatusOK {
		t.Errorf("close status = %d, want 200 — the destination was reached before the bridge fell open", closed.StatusCode)
	}
	if closed.Error == nil || closed.Error.Kind != "dial_failed" {
		t.Errorf("close error = %+v, want kind dial_failed", closed.Error)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func waitClosed(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("HandleTransparentConn did not return")
	}
}
