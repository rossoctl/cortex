package transparentproxy

import (
	"context"
	"log/slog"
	"net"
)

// maxUnwrapDepth bounds the wrapper walk in OrigDstFromConn. The real chain is
// at most two deep (tls.Conn -> peekedConn -> Conn), so this only exists so a
// pathological or cyclic wrapper can't spin the loop forever.
const maxUnwrapDepth = 8

// Conn is a connection accepted by an InboundListener, carrying the original
// destination recovered from the kernel before any protocol bytes were read.
// It embeds *net.TCPConn so keepalive and syscall access still work upstack.
type Conn struct {
	*net.TCPConn
	dst string
}

// OrigDst returns the pre-REDIRECT destination as "host:port" — for captured
// ingress, this pod's own IP on the port the client actually addressed.
func (c *Conn) OrigDst() string { return c.dst }

// InboundListener accepts iptables-PREROUTING-REDIRECTed connections and
// recovers each one's original destination via SO_ORIGINAL_DST, so an HTTP
// server upstack can forward to the port the client actually addressed rather
// than to a single configured backend.
//
// It is a net.Listener rather than a ConnHandler dispatcher (the shape the
// outbound path uses) because inbound cannot blind-tunnel: JWT validation reads
// the Authorization header and Path, and rewrites Authorization to a
// placeholder before forwarding. That requires a real HTTP server over the
// connection, so the natural seam is http.Server.Serve — which wants a
// net.Listener.
type InboundListener struct {
	inner *net.TCPListener
}

// NewInboundListener wraps ln so each accepted connection carries its recovered
// original destination. Pair it with ConnContextHook so the destination reaches
// the HTTP handler.
func NewInboundListener(ln *net.TCPListener) *InboundListener {
	return &InboundListener{inner: ln}
}

// Accept returns the next capture whose original destination was recovered and
// passed CheckDst.
//
// Connections that fail either step are logged, closed, and skipped rather than
// surfaced as an Accept error: an error return would terminate
// http.Server.Serve and take the whole inbound listener down, turning one bad
// connection into an outage. This mirrors tlssniff.Listener.Accept's handling of
// strict-mode rejections, and Server.dispatch's per-connection drop.
func (l *InboundListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.inner.AcceptTCP()
		if err != nil {
			return nil, err
		}
		dst, err := originalDst(conn)
		if err != nil {
			// No recoverable original destination means this connection did not
			// arrive via the REDIRECT (e.g. a direct dial to the listener port).
			// Drop it rather than guess — we will not forward to a destination we
			// cannot attribute to the kernel's conntrack record.
			slog.Warn("transparent-inbound: dropping connection with no original destination",
				"remote", conn.RemoteAddr().String(), "error", err)
			_ = conn.Close()
			continue
		}
		if err := CheckDst(conn.LocalAddr(), dst); err != nil {
			slog.Warn("transparent-inbound: dropping self-referential connection",
				"remote", conn.RemoteAddr().String(), "dst", dst,
				"local", conn.LocalAddr().String(), "error", err)
			_ = conn.Close()
			continue
		}
		slog.Debug("transparent-inbound: captured connection",
			"remote", conn.RemoteAddr().String(), "dst", dst)
		return &Conn{TCPConn: conn, dst: dst}, nil
	}
}

// Close shuts down the underlying listener.
func (l *InboundListener) Close() error { return l.inner.Close() }

// Addr returns the listener's bind address.
func (l *InboundListener) Addr() net.Addr { return l.inner.Addr() }

type origDstKey struct{}

// ContextWithOrigDst returns ctx carrying dst.
func ContextWithOrigDst(ctx context.Context, dst string) context.Context {
	return context.WithValue(ctx, origDstKey{}, dst)
}

// OrigDstFromContext returns the original destination stashed by
// ConnContextHook, if any. Handlers use it to pick a per-request backend.
func OrigDstFromContext(ctx context.Context) (string, bool) {
	dst, ok := ctx.Value(origDstKey{}).(string)
	return dst, ok && dst != ""
}

// netConner is satisfied by connection wrappers that expose their underlying
// connection: *tls.Conn (stdlib) and tlssniff's peeked conn. Declared
// structurally so this package needs no import of either.
type netConner interface{ NetConn() net.Conn }

// OrigDstFromConn walks a connection's wrapper chain looking for a *Conn and
// returns its recovered original destination.
//
// The walk is necessary because the mTLS path wraps our conn twice — tlssniff
// peeks the first byte (returning a buffered wrapper) and then hands TLS
// handshakes to tls.Server — so by the time http.Server sees the connection,
// the *Conn is two layers down.
func OrigDstFromConn(c net.Conn) (string, bool) {
	for i := 0; i < maxUnwrapDepth && c != nil; i++ {
		if tc, ok := c.(*Conn); ok {
			return tc.dst, true
		}
		u, ok := c.(netConner)
		if !ok {
			return "", false
		}
		c = u.NetConn()
	}
	return "", false
}

// ConnContextHook is an http.Server.ConnContext function that stashes each
// connection's recovered original destination into the request context. Wire it
// into the http.Server that serves an InboundListener:
//
//	srv := &http.Server{Handler: h, ConnContext: transparentproxy.ConnContextHook}
//
// Connections with no recoverable destination pass through unchanged; the
// handler decides how to treat a missing destination (the reverse proxy fails
// closed with 502 when it has no configured fallback backend).
func ConnContextHook(ctx context.Context, c net.Conn) context.Context {
	if dst, ok := OrigDstFromConn(c); ok {
		return ContextWithOrigDst(ctx, dst)
	}
	return ctx
}
