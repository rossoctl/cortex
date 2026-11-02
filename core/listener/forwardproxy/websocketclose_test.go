package forwardproxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// wsClosingHandler writes the 101, waits, then closes — an origin that hangs up
// FIRST. That is RFC 6455 §7.1.1's server-initiated close (FIN after the close
// frame), and also what an origin restart or an LB idle timeout looks like.
//
// Every other upgrade test has the CLIENT close first, which is exactly why the
// bridged half-close gap survived: a client-initiated close is driven from the
// agent side and never needs the origin's FIN to travel back.
func wsClosingHandler(delay time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "expected a websocket upgrade", http.StatusBadRequest)
			return
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: %s\r\n\r\n", wsAccept(r.Header.Get("Sec-WebSocket-Key")))
		if err := brw.Flush(); err != nil {
			return
		}
		time.Sleep(delay)
		_ = conn.Close() // FIN toward the proxy; the splice must forward it
	}
}

// readAfterOriginClose reads until the read fails and reports the error. A
// forwarded close surfaces as EOF (or a reset); a close that was NOT forwarded
// leaves the client blocked until its own deadline, so the error is a timeout.
func readAfterOriginClose(t *testing.T, r io.Reader) error {
	t.Helper()
	buf := make([]byte, 64)
	for {
		if _, err := r.Read(buf); err != nil {
			return err
		}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// THE REGRESSION. On the TLS-bridged path the origin's half-close must reach the
// agent. It did not: tlsbridge.ServeConn serves the terminated conn through
// newOneConnListener, which hands Hijack a *notifyConn — and notifyConn embeds
// the net.Conn INTERFACE, so CloseWrite is not in its method set whatever the
// concrete type underneath is. The splice's
//
//	if wc, ok := conn.(interface{ CloseWrite() error }); ok
//
// therefore failed on the bridge and the goroutine sent nil, forwarding nothing.
// The agent got neither close_notify nor FIN and waited on a WebSocket whose
// other end was gone, while the other copy stayed blocked on the silent agent —
// so the 101 row, recorded after BOTH directions, never landed either. Same
// one-event-per-upgrade loss as the RST case, on the clean-close path.
//
// The plain path was always fine: there conn is a *net.TCPConn, which has
// CloseWrite.
//
// Both paths are asserted here, in one test, because the bug WAS the asymmetry —
// splitting them invites fixing one and leaving the other (#1549 review).
func TestWebSocketSplice_OriginClosesFirst(t *testing.T) {
	const originDelay = 100 * time.Millisecond

	t.Run("plain", func(t *testing.T) {
		store := session.New(5*time.Minute, 100, 0)
		t.Cleanup(store.Close)
		origin := httptest.NewServer(wsClosingHandler(originDelay))
		t.Cleanup(origin.Close)

		p, err := pipeline.New(nil)
		if err != nil {
			t.Fatalf("pipeline.New: %v", err)
		}
		srv := &Server{
			OutboundPipeline: pipeline.NewHolder(p),
			Sessions:         store,
			Client:           http.DefaultClient,
		}
		proxy := httptest.NewServer(srv.Handler())
		t.Cleanup(proxy.Close)

		originURL, err := url.Parse(origin.URL)
		if err != nil {
			t.Fatalf("parse origin URL: %v", err)
		}
		proxyURL, err := url.Parse(proxy.URL)
		if err != nil {
			t.Fatalf("parse proxy URL: %v", err)
		}
		conn, err := net.Dial("tcp", proxyURL.Host)
		if err != nil {
			t.Fatalf("dial proxy: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		// Generous relative to the origin's 100ms, so a timeout here means the
		// close was never forwarded rather than that the test was impatient.
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

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

		if err := readAfterOriginClose(t, br); isTimeout(err) {
			t.Fatalf("plain: client never saw the origin's close (%v) — the splice dropped the half-close", err)
		}
	})

	t.Run("bridged", func(t *testing.T) {
		st := newClosingBridgeStack(t, originDelay)

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

		// The headline assertion: the origin hung up, so this read must END.
		// Before the fix it blocked for the full deadline and reported a
		// timeout, with the agent still holding a dead WebSocket.
		if err := readAfterOriginClose(t, br); isTimeout(err) {
			t.Fatalf("bridged: client never saw the origin's close (%v) — notifyConn has no CloseWrite, so the splice forwarded nothing", err)
		}

		// And the 101 row lands. The client closes on seeing EOF, as a real
		// WebSocket client does once the peer has gone — the row is recorded
		// after BOTH directions, and the client→origin copy is still blocked
		// on this (silent) client until it does. The point of the assertion
		// is that the close reaching the client is what UNBLOCKS that: before
		// the fix the client never learned to close, so the row never landed
		// at all until the client's own deadline expired.
		//
		// assertUpgradedRows is not reused here: it requires non-zero byte
		// counts both ways, and this session deliberately exchanges no frames
		// (the origin closes straight after the 101). What matters on this row
		// is that it EXISTS and reports a clean close, so the fields are
		// checked directly.
		_ = tconn.Close()
		events := waitForUpgradedRows(t, st.store)
		var row *pipeline.SessionEvent
		for i := range events {
			if events[i].Phase == pipeline.SessionResponse && events[i].StatusCode == http.StatusSwitchingProtocols {
				row = &events[i]
				break
			}
		}
		if row == nil {
			t.Fatalf("no 101 response row among %d events — one event per upgrade is the contract", len(events))
		}
		// An origin closing first is an ORDINARY end to a WebSocket, not a
		// broken relay, so the row must not be marked. proxy_error here would
		// make every server-initiated close read as a failure.
		if row.Error != nil {
			t.Errorf("101 row carries error %+v — an origin-initiated close is a clean end", row.Error)
		}
	})
}

// newClosingBridgeStack is newBridgeStack with an origin that closes first. Kept
// separate rather than parameterising newBridgeStack, so the existing tests'
// setup is untouched.
func newClosingBridgeStack(t *testing.T, delay time.Duration) *bridgeStack {
	t.Helper()
	st := &bridgeStack{}
	origin := httptest.NewUnstartedServer(wsClosingHandler(delay))
	origin.EnableHTTP2 = true
	origin.StartTLS()

	originURL, err := url.Parse(origin.URL)
	if err != nil {
		origin.Close()
		t.Fatalf("parse origin URL: %v", err)
	}
	originCAPEM := certPEM(origin)

	src, err := tlsbridge.NewEphemeralSource()
	if err != nil {
		origin.Close()
		t.Fatalf("NewEphemeralSource: %v", err)
	}
	up, err := tlsbridge.NewUpstreamClient(originCAPEM, false)
	if err != nil {
		origin.Close()
		t.Fatalf("NewUpstreamClient: %v", err)
	}
	engine := &tlsbridge.Engine{
		Decision: mustBridgeDecision(t, portOf(originURL.Host)),
		Term:     tlsbridge.NewTerminator(tlsbridge.NewMinter(src, tlsbridge.MinterOpts{})),
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
