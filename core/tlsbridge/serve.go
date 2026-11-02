package tlsbridge

import (
	"crypto/tls"
	"net"
	"net/http"
	"sync"

	"golang.org/x/net/http2"
)

// oneConnListener feeds exactly one already-accepted conn to http.Server.Serve.
// http.Server dispatches the conn to a goroutine and immediately calls Accept
// again; if that second Accept returned an error right away, Serve — and thus
// ServeConn — would return before the request is handled, tearing the conn down
// mid-response. So the second Accept BLOCKS until the served conn is closed,
// keeping Serve (and ServeConn) alive for the whole connection, including
// HTTP/1.1 keep-alive across multiple requests.
type oneConnListener struct {
	mu     sync.Mutex
	conn   net.Conn
	closed chan struct{}
}

func newOneConnListener(c net.Conn) *oneConnListener {
	l := &oneConnListener{closed: make(chan struct{})}
	l.conn = &notifyConn{Conn: c, closed: l.closed}
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	c := l.conn
	l.conn = nil
	l.mu.Unlock()
	if c != nil {
		return c, nil
	}
	<-l.closed // block until the served conn closes, then stop the accept loop
	return nil, net.ErrClosed
}
func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return dummyAddr{} }

// notifyConn signals the listener when the served conn is closed, so the
// listener's blocked Accept releases and Serve returns.
type notifyConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *notifyConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// CloseWrite forwards a half-close to the terminated TLS conn — a close_notify
// on the wire — so a WebSocket spliced over the bridge sees the origin's close.
//
// It has to be written out rather than inherited: notifyConn embeds the net.Conn
// INTERFACE, so only net.Conn's method set is promoted and CloseWrite is not in
// it, whatever the concrete type underneath happens to be. The forward proxy's
// splice probes its conn for `interface{ CloseWrite() error }`, and on the
// bridged path that probe is this wrapper — it failed, the goroutine forwarded
// nothing, and an origin that hung up first left the agent waiting on a dead
// WebSocket while the 101 row (recorded after BOTH directions) never landed.
// The plain path was unaffected: there the conn is a *net.TCPConn (#1549 review).
//
// nil when the wrapped conn cannot half-close, which is the right answer rather
// than an error: the splice treats a non-nil result as a broken relay and marks
// the row `proxy_error`, so reporting "could not half-close" here would flag
// every clean close on such a conn as a failed session. A *tls.Conn — what
// ServeConn is always given — does implement CloseWrite, so in practice this
// forwards.
func (c *notifyConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

type dummyAddr struct{}

func (dummyAddr) Network() string { return "tcp" }
func (dummyAddr) String() string  { return "tls-bridge" }

// ServeConn drives an already-terminated TLS conn through handler with HTTP
// keep-alive, negotiating h2 when ALPN selected it. It blocks until the
// connection is closed (like a tunnel), so callers can serve it synchronously.
func ServeConn(tconn *tls.Conn, handler http.Handler) {
	srv := &http.Server{Handler: handler}
	if tconn.ConnectionState().NegotiatedProtocol == "h2" {
		h2s := &http2.Server{}
		h2s.ServeConn(tconn, &http2.ServeConnOpts{Handler: handler, BaseConfig: srv})
		return
	}
	_ = srv.Serve(newOneConnListener(tconn))
}
