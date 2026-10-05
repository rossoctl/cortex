package forwardproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/plugintesting"
	"github.com/rossoctl/cortex/core/session"
	"golang.org/x/net/http2"
)

// Tests for #1187's first part: a bridged tunnel's open row is recorded with the
// tunnel's first decrypted request that records a row — in that row's session and
// directly before it — rather than under ActiveSession() at handshake time.

func decryptedRequest(path string) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Host: "example.com", HTTPMethod: http.MethodGet, HTTPPath: path,
	}
}

// deferredTunnel is a bridged tunnel's log as bridgeServe leaves it after a
// successful handshake.
func deferredTunnel(s *Server) *tunnelLog {
	tl := s.newTunnelLog(&pipeline.Context{Direction: pipeline.Outbound, Method: http.MethodConnect, Host: "example.com:443"}, false)
	tl.deferOpen()
	return tl
}

// openThen asserts sid holds exactly one tunnel open, directly followed by another
// event for the same host and stamped with that event's time, and returns the two. Every
// caller's open was recorded by recordWith, which copies its row's time, so a different
// stamp means some other same-host row came between them.
func openThen(t *testing.T, store *session.Store, sid string) (open, next pipeline.SessionEvent) {
	t.Helper()
	v := store.View(sid)
	if v == nil {
		t.Fatalf("session %q holds nothing", sid)
	}
	at := -1
	for i, e := range v.Events {
		if e.Tunnel && e.Phase == pipeline.SessionRequest {
			if at >= 0 {
				t.Fatalf("session %q holds more than one tunnel open", sid)
			}
			at = i
		}
	}
	if at < 0 || at+1 >= len(v.Events) {
		t.Fatalf("session %q: no tunnel open followed by an event (%d events)", sid, len(v.Events))
	}
	open, next = v.Events[at], v.Events[at+1]
	if next.Tunnel || hostOnly(next.Host) != hostOnly(open.Host) {
		t.Fatalf("event after the open = %+v, want the decrypted request to %s", next, open.Host)
	}
	if open.TunnelReason != "" {
		t.Errorf("bridged open carries reason %q; agentop folds an open into the request after it only when its reason is empty", open.TunnelReason)
	}
	if !open.At.Equal(next.At) {
		t.Errorf("open stamped %s, want its row's %s: a later stamp reads to agentop's pager as a restarted session, and any other means a row came between", open.At, next.At)
	}
	return open, next
}

func TestTunnelLog_FirstRecordedRowCarriesTheOpen(t *testing.T) {
	store := session.New(0, 0, 0)
	defer store.Close()
	store.Append("other", decryptedRequest("/unrelated")) // what ActiveSession() answers
	s := &Server{Sessions: store}
	tl := deferredTunnel(s)

	if !tl.admit() {
		t.Fatal("a request on a live tunnel was refused")
	}
	s.appendOutbound(tl, "sess-1", decryptedRequest("/first"))
	s.appendOutbound(tl, "sess-1", decryptedRequest("/second"))
	tl.release()
	tl.finish()

	if _, next := openThen(t, store, "sess-1"); next.HTTPPath != "/first" {
		t.Errorf("open precedes %q, want the first recorded request", next.HTTPPath)
	}
	if v := store.View("sess-1"); len(v.Events) != 3 {
		t.Errorf("sess-1 holds %d events, want the open and both requests", len(v.Events))
	}
	if opens, closes := tunnelRows(store, "other"); len(opens)+len(closes) != 0 {
		t.Error("a tunnel row landed in the session that was merely active")
	}
	if _, closes := tunnelRows(store, "sess-1"); len(closes) != 0 {
		t.Errorf("a tunnel its requests answered recorded %d close(s)", len(closes))
	}
}

// On h2 several decrypted requests share one tunnel and run concurrently. Exactly one
// of them takes the open, and nothing records between the two.
func TestTunnelLog_ConcurrentRequestsShareOneOpen(t *testing.T) {
	store := session.New(0, 0, 0)
	defer store.Close()
	s := &Server{Sessions: store}
	tl := deferredTunnel(s)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if !tl.admit() {
				t.Error("a request on a live tunnel was refused")
				return
			}
			defer tl.release()
			s.appendOutbound(tl, "sess-1", decryptedRequest(fmt.Sprint("/", i)))
		}(i)
	}
	wg.Wait()
	tl.finish()

	openThen(t, store, "sess-1")
	if v := store.View("sess-1"); len(v.Events) != 9 {
		t.Errorf("sess-1 holds %d events, want one open and eight requests", len(v.Events))
	}
}

// A request admitted but never recorded — denied by a plugin that appended no
// invocation, say — answers nothing, so the tunnel still owes its open and a close.
func TestTunnelLog_UnrecordedRequestStillSettlesTheTunnel(t *testing.T) {
	store := session.New(0, 0, 0)
	defer store.Close()
	s := &Server{Sessions: store}
	tl := deferredTunnel(s)

	if !tl.admit() {
		t.Fatal("a request on a live tunnel was refused")
	}
	tl.release()
	tl.finish()

	if _, closed := onePair(t, store, session.DefaultSessionID); closed.StatusCode != http.StatusOK {
		t.Errorf("close status %d, want 200", closed.StatusCode)
	}
}

// On h2 ServeConn returns without waiting for handlers. The tunnel is settled by the
// last one to finish, so its row cannot land after a close saying it carried nothing.
func TestTunnelLog_HandlerOutlivingServeConnSettlesOnRelease(t *testing.T) {
	store := session.New(0, 0, 0)
	defer store.Close()
	s := &Server{Sessions: store}
	tl := deferredTunnel(s)

	if !tl.admit() {
		t.Fatal("a request on a live tunnel was refused")
	}
	tl.finish() // ServeConn returned; the handler is still running
	if n := len(store.ListSessions()); n != 0 {
		t.Fatalf("%d session(s) recorded while a handler could still record", n)
	}
	tl.release()

	onePair(t, store, session.DefaultSessionID)
}

// bridgedRoundTrip sends req over an established bridged connection and drains the
// response.
func bridgedRoundTrip(t *testing.T, tc *tls.Conn, req *http.Request) *http.Response {
	t.Helper()
	go func() { _ = req.Write(tc) }()
	resp, err := http.ReadResponse(bufio.NewReader(tc), req)
	if err != nil {
		t.Fatalf("bridged request: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp
}

// End to end: the CONNECT names no session, the decrypted request does, and the open
// follows the request rather than the session active when the CONNECT arrived.
func TestHandleConnect_BridgedOpenJoinsItsRequestsSession(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	store.Append("other", decryptedRequest("/unrelated")) // ActiveSession() at CONNECT time
	proxyAddr, target, ca, done := bridgingProxyWith(t, store, func(s *Server) {
		s.SessionIDHeaders = []string{session.ClaudeCodeSessionHeader}
	})

	raw, br, _ := sendConnect(t, proxyAddr, target)
	tc := bridgedTLS(t, raw, br, target, ca)
	req, _ := http.NewRequest(http.MethodGet, "https://"+hostOnly(target)+"/x", nil)
	req.Header.Set(session.ClaudeCodeSessionHeader, "sess-1")
	if resp := bridgedRoundTrip(t, tc, req); resp.StatusCode != http.StatusOK {
		t.Fatalf("bridged request status %d, want 200", resp.StatusCode)
	}
	_ = tc.Close()
	waitDone(t, done)

	if _, next := openThen(t, store, "sess-1"); next.HTTPPath != "/x" || next.Phase != pipeline.SessionRequest {
		t.Errorf("open precedes %s %q, want the request to /x", next.Phase, next.HTTPPath)
	}
	if opens, _ := tunnelRows(store, "other"); len(opens) != 0 {
		t.Error("the open landed in the session that was active at CONNECT time")
	}
}

// denyDecrypted lets CONNECTs through and denies every decrypted request, so a bridged
// tunnel's first recorded row is a denial.
type denyDecrypted struct{}

func (denyDecrypted) Name() string                              { return "deny-decrypted" }
func (denyDecrypted) Capabilities() pipeline.PluginCapabilities { return pipeline.PluginCapabilities{} }
func (denyDecrypted) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if pctx.Method == http.MethodConnect {
		return pipeline.Action{Type: pipeline.Continue}
	}
	return pctx.DenyAndRecord("denied_for_test", "test.denied", "denied for the test")
}
func (denyDecrypted) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// A denial is a recorded row too, so it takes the open the same way.
func TestHandleConnect_BridgedOpenJoinsARejectedRequest(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	proxyAddr, target, ca, done := bridgingProxyWith(t, store, func(s *Server) {
		s.SessionIDHeaders = []string{session.ClaudeCodeSessionHeader}
		p, err := plugintesting.BuildPipeline([]pipeline.Plugin{denyDecrypted{}})
		if err != nil {
			t.Fatalf("BuildPipeline: %v", err)
		}
		s.OutboundPipeline = pipeline.NewHolder(p)
	})

	raw, br, _ := sendConnect(t, proxyAddr, target)
	tc := bridgedTLS(t, raw, br, target, ca)
	req, _ := http.NewRequest(http.MethodGet, "https://"+hostOnly(target)+"/x", nil)
	req.Header.Set(session.ClaudeCodeSessionHeader, "sess-1")
	if resp := bridgedRoundTrip(t, tc, req); resp.StatusCode < 400 {
		t.Fatalf("denied request got status %d", resp.StatusCode)
	}
	_ = tc.Close()
	waitDone(t, done)

	if _, next := openThen(t, store, "sess-1"); next.Phase != pipeline.SessionDenied {
		t.Errorf("open precedes a %s row, want the denial", next.Phase)
	}
	if v := store.View(session.DefaultSessionID); v != nil {
		t.Errorf("default holds %d event(s); the open belongs with the denial", len(v.Events))
	}
}

// outlivesServeConn holds a bridged tunnel's decrypted request in the pipeline until the
// CONNECT has finished, then lets it go on. On h2 that is a real order: ServeConn returns
// when the client goes, without waiting for a handler still running, and handleConnect
// then runs RunFinish on the CONNECT's pctx while the handler is alive.
//
// Its OnFinish on the CONNECT writes Extensions.Custom, as the Finisher contract allows.
// It releases the request BEFORE writing, so nothing orders that write before whatever
// the handler then reads; a real finisher has no such ordering with a handler either,
// and an order here would only hide the race from the detector.
//
// With deny set the released request is rejected without an invocation, so it records
// nothing and the handler's release settles the tunnel instead.
type outlivesServeConn struct {
	deny     bool
	entered  chan struct{} // the decrypted request is inside the pipeline
	finished chan struct{} // the CONNECT's OnFinish is running
	served   chan struct{} // the decrypted request has finished its pipeline pass
}

type outlivesServeConnState struct{ n int }

func (*outlivesServeConn) Name() string { return "outlives-serveconn" }
func (*outlivesServeConn) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *outlivesServeConn) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if pctx.Method == http.MethodConnect {
		return pipeline.Action{Type: pipeline.Continue}
	}
	close(p.entered)
	select {
	case <-p.finished:
	case <-time.After(10 * time.Second): // reported by the test's own wait, not hung on
	}
	if p.deny {
		return pipeline.Deny("test.denied", "denied without a record")
	}
	return pipeline.Action{Type: pipeline.Continue}
}
func (*outlivesServeConn) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *outlivesServeConn) OnFinish(_ context.Context, pctx *pipeline.Context) {
	if pctx.Method != http.MethodConnect {
		close(p.served)
		return
	}
	close(p.finished)
	pipeline.SetState(pctx, "outlives-serveconn", &outlivesServeConnState{n: 1})
}

// outliveTheConnect drives one h2 bridged request through probe and returns once the
// CONNECT has finished and the request has passed through the pipeline.
func outliveTheConnect(t *testing.T, store *session.Store, probe *outlivesServeConn) {
	t.Helper()
	proxyAddr, target, ca, done := bridgingProxyWith(t, store, func(s *Server) {
		s.SessionIDHeaders = []string{session.ClaudeCodeSessionHeader}
		p, err := plugintesting.BuildPipeline([]pipeline.Plugin{probe})
		if err != nil {
			t.Fatalf("BuildPipeline: %v", err)
		}
		s.OutboundPipeline = pipeline.NewHolder(p)
	})

	raw, br, _ := sendConnect(t, proxyAddr, target)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		t.Fatal("bad bridge CA")
	}
	tc := tls.Client(&bufferedConn{Conn: raw, r: br}, &tls.Config{
		ServerName: hostOnly(target), RootCAs: pool, NextProtos: []string{"h2"},
	})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("bridged handshake: %v", err)
	}
	if got := tc.ConnectionState().NegotiatedProtocol; got != "h2" {
		t.Fatalf("bridge negotiated %q; this test needs h2, where ServeConn does not wait for handlers", got)
	}
	cc, err := (&http2.Transport{}).NewClientConn(tc)
	if err != nil {
		t.Fatalf("h2 client conn: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://"+hostOnly(target)+"/x", nil)
	req.Header.Set(session.ClaudeCodeSessionHeader, "sess-1")
	go func() {
		if resp, err := cc.RoundTrip(req); err == nil {
			_ = resp.Body.Close()
		}
	}()

	waitFor(t, probe.entered, "the decrypted request never reached the pipeline")
	_ = tc.Close() // the client goes; ServeConn returns with the handler still running
	waitDone(t, done)
	waitFor(t, probe.served, "the decrypted request never finished its pipeline pass")
}

// On h2 a decrypted request can record after the CONNECT has finished, so building the
// deferred open then — or settling the tunnel then — would read the CONNECT's pctx while
// its finishers write it. The open is built when the tunnel is marked bridged; run under
// -race, both cases fail if it is not.
func TestHandleConnect_H2HandlerOutlivingTheConnectRecordsItsOpen(t *testing.T) {
	t.Run("the request records", func(t *testing.T) {
		store := session.New(5*time.Minute, 100, 0)
		defer store.Close()
		outliveTheConnect(t, store, &outlivesServeConn{
			entered: make(chan struct{}), finished: make(chan struct{}), served: make(chan struct{}),
		})

		if _, next := openThen(t, store, "sess-1"); next.HTTPPath != "/x" {
			t.Errorf("open precedes %q, want the request to /x", next.HTTPPath)
		}
	})

	t.Run("the request records nothing", func(t *testing.T) {
		store := session.New(5*time.Minute, 100, 0)
		defer store.Close()
		outliveTheConnect(t, store, &outlivesServeConn{
			deny:    true,
			entered: make(chan struct{}), finished: make(chan struct{}), served: make(chan struct{}),
		})

		// The pipeline pass ends before the handler's release, which is what settles.
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, closes := tunnelRows(store, session.DefaultSessionID); len(closes) > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the tunnel never settled after its last handler returned")
			}
			time.Sleep(5 * time.Millisecond)
		}
		open, closed := onePair(t, store, session.DefaultSessionID)
		if open.TunnelReason != "" || closed.StatusCode != http.StatusOK {
			t.Errorf("settled tunnel = {reason %q, close status %d}, want {\"\", 200}", open.TunnelReason, closed.StatusCode)
		}
		if v := store.View("sess-1"); v != nil {
			t.Errorf("sess-1 holds %d event(s); a request that recorded nothing files nothing", len(v.Events))
		}
	})
}

func waitFor(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal(msg)
	}
}
