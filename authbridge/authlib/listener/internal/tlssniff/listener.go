// Package tlssniff provides a net.Listener wrapper that detects
// whether each accepted connection is a TLS handshake or a plain
// HTTP request, and dispatches accordingly.
//
// Used by the reverse-proxy listener under mTLS mode to serve both
// TLS and plaintext callers on a single port:
//
//   - permissive: TLS first byte (0x16) → tls.Server(conn, cfg);
//     anything else → conn unchanged
//   - strict:     TLS first byte → tls.Server(conn, cfg);
//     anything else → connection closed immediately
//
// internal because no other listener should pick up this trick
// without explicit thought — TLS sniffing has subtle failure modes
// (slow clients holding goroutines, ALPN handshake handling) that
// don't generalize to other protocols.
package tlssniff

import (
	"bufio"
	"crypto/tls"
	"errors"
	"net"
	"time"
)

// Mode controls whether plain (non-TLS) first bytes are passed
// through (permissive) or rejected (strict). Must align with the
// MTLSMode values in authlib/config — duplicated here to avoid
// importing config from a deep listener-internal package.
type Mode int

const (
	// ModePermissive: serve plain HTTP and TLS on the same port.
	ModePermissive Mode = iota
	// ModeStrict: serve only TLS; close non-TLS connections.
	ModeStrict
)

// peekTimeout caps how long we wait for the first byte. Slow clients
// that don't send anything would otherwise hold a goroutine
// indefinitely. The deadline is removed once the byte arrives so
// it doesn't affect downstream IO.
const peekTimeout = 5 * time.Second

// Listener wraps a net.Listener and dispatches each accepted
// connection through tls.Server when the first byte indicates a TLS
// handshake. The underlying connection is replaced with a buffered
// wrapper so the peeked byte is preserved for the eventual reader.
type Listener struct {
	inner     net.Listener
	tlsConfig *tls.Config
	mode      Mode
	// onPlainRejected, when set, is called with the rejected conn
	// before it's closed (strict mode). Hook for metrics counters
	// without coupling tlssniff to the metrics package.
	onPlainRejected func(net.Conn)
}

// New constructs a TLS-sniffing listener that wraps inner. cfg is
// the *tls.Config to use when wrapping accepted conns as TLS servers.
// mode controls plain-byte fallback behavior.
func New(inner net.Listener, cfg *tls.Config, mode Mode) *Listener {
	return &Listener{inner: inner, tlsConfig: cfg, mode: mode}
}

// SetOnPlainRejected installs a callback for strict-mode rejections.
// Useful for incrementing a metrics counter. Idempotent; can be set
// to nil to clear.
func (l *Listener) SetOnPlainRejected(fn func(net.Conn)) {
	l.onPlainRejected = fn
}

// Accept returns the next connection. The caller (typically
// http.Server.Serve) doesn't need to know whether the returned conn
// is plain or TLS — the standard library detects *tls.Conn via type
// assertion and populates Request.TLS accordingly.
//
// Strict-mode rejection of a non-TLS conn produces a transient
// net-error result that http.Server.Serve treats as a soft failure
// (logs + continue). The rejected client sees a connection close
// before any HTTP exchange — the cleanest signal that its
// scheme/protocol is unwelcome here.
func (l *Listener) Accept() (net.Conn, error) {
	for {
		conn, err := l.inner.Accept()
		if err != nil {
			return nil, err
		}
		wrapped, ok := l.dispatch(conn)
		if !ok {
			// Strict-mode rejection: close and try the next connection.
			// We DO NOT propagate as an Accept error because that would
			// shut down the http.Server.Serve loop.
			continue
		}
		return wrapped, nil
	}
}

// dispatch peeks the first byte of conn, then either:
//   - wraps in tls.Server (TLS handshake byte, both modes)
//   - returns the conn unchanged (non-TLS byte, permissive)
//   - closes the conn and returns ok=false (non-TLS byte, strict)
func (l *Listener) dispatch(conn net.Conn) (net.Conn, bool) {
	if err := conn.SetReadDeadline(time.Now().Add(peekTimeout)); err != nil {
		conn.Close()
		return nil, false
	}
	br := bufio.NewReader(conn)
	first, err := br.Peek(1)
	if err != nil {
		// Slow / dead client whose first byte never arrived (peek
		// timeout) or whose connection was already closed. The
		// bufio.Reader holds no buffered bytes (Peek failed), and
		// even if it had buffered any they'd be GC'd along with br
		// when this scope exits — no leak. The client gets a
		// connection close; if they wanted to send data, they'll
		// reconnect.
		conn.Close()
		return nil, false
	}
	// Clear the deadline now that we have the byte; downstream IO
	// applies its own deadlines via http.Server.ReadHeaderTimeout etc.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, false
	}

	pc := &peekedConn{Conn: conn, br: br}

	// 0x16 is the TLS Content-Type byte for Handshake records (RFC 8446).
	// Plain HTTP requests start with an ASCII method letter (G, P, H, ...),
	// well outside the 0x14-0x18 TLS-record-type range.
	if first[0] == 0x16 {
		return tls.Server(pc, l.tlsConfig), true
	}

	if l.mode == ModeStrict {
		if l.onPlainRejected != nil {
			l.onPlainRejected(pc)
		}
		conn.Close()
		return nil, false
	}
	return pc, true
}

// Close shuts down the underlying listener.
func (l *Listener) Close() error { return l.inner.Close() }

// Addr returns the listener's bind address.
func (l *Listener) Addr() net.Addr { return l.inner.Addr() }

// peekedConn forwards Read through the bufio.Reader so the peeked
// byte is delivered to the first downstream read. All other Conn
// methods pass through to the underlying connection.
type peekedConn struct {
	net.Conn
	br *bufio.Reader
}

// Read drains buffered bytes from the bufio.Reader (the peeked byte
// plus anything else it eagerly read) before returning to direct
// socket reads via net.Conn embedding.
func (c *peekedConn) Read(p []byte) (int, error) { return c.br.Read(p) }

// ErrUnexpected is returned when the listener encounters a state it
// can't recover from (e.g. SetReadDeadline failed). Callers don't
// match against it — it surfaces only via Accept's error return.
var ErrUnexpected = errors.New("tlssniff: unexpected dispatch error")
