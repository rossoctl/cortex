// Package transparentproxy implements transparent (iptables-REDIRECTed) proxy
// listeners for proxy-sidecar mode. In both directions the peer believes it is
// talking directly to its chosen destination, so the listener recovers that
// destination from the kernel via SO_ORIGINAL_DST rather than from any protocol
// header. This is the Go equivalent of Envoy's original_dst listener filter +
// ORIGINAL_DST cluster used by envoy-sidecar mode.
//
// Two shapes, because the two directions have different requirements:
//
//   - Outbound (Server, enforce-redirect egress guard): dispatches each capture
//     to a ConnHandler that gates on destination and then blind-tunnels,
//     emitting no proxy-protocol bytes back to the agent. Policy is host-based,
//     so the bytes can stay opaque and the agent's end-to-end TLS is preserved.
//   - Inbound (InboundListener): a net.Listener, because inbound cannot
//     blind-tunnel. JWT validation reads the Authorization header and Path and
//     rewrites Authorization to a placeholder before forwarding, which requires
//     a real HTTP server over the connection.
//
// Both share CheckDst and the platform-specific SO_ORIGINAL_DST recovery.
package transparentproxy

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
)

// ConnHandler processes one accepted outbound connection whose original
// destination has been recovered. dst is "host:port". The handler owns the
// connection's lifecycle, including closing it.
type ConnHandler func(conn net.Conn, dst string)

// Server accepts iptables-REDIRECTed connections and dispatches them to a
// ConnHandler after recovering each connection's original destination.
type Server struct {
	handle ConnHandler
}

// NewServer returns a transparent proxy server that dispatches each accepted,
// destination-recovered connection to handle. In proxy-sidecar mode handle is
// forwardproxy.Server.HandleTransparentConn, so transparent and explicit-proxy
// egress share one auth pipeline.
func NewServer(handle ConnHandler) *Server {
	if handle == nil {
		// Defensive: a nil handler would panic at dispatch and take down the
		// process. Fall back to closing the connection so a misconfiguration
		// degrades to "no capture" rather than a crash.
		handle = func(conn net.Conn, _ string) {
			slog.Error("transparent-proxy: nil connection handler; closing connection",
				"remote", conn.RemoteAddr().String())
			_ = conn.Close()
		}
	}
	return &Server{handle: handle}
}

// Serve accepts connections on ln until it is closed, recovering each
// connection's original destination and dispatching to the handler in its own
// goroutine. Returns nil when ln is closed (graceful shutdown), or the accept
// error otherwise.
func (s *Server) Serve(ln *net.TCPListener) error {
	for {
		conn, err := ln.AcceptTCP()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.dispatch(conn)
	}
}

func (s *Server) dispatch(conn *net.TCPConn) {
	dst, err := originalDst(conn)
	if err != nil {
		// No recoverable original destination means this connection did not
		// arrive via the REDIRECT (e.g. a direct dial to the listener port).
		// Drop it rather than guess a destination — we will not blind-tunnel
		// to an attacker-chosen target.
		slog.Warn("transparent-proxy: dropping connection with no original destination",
			"remote", conn.RemoteAddr().String(), "error", err)
		_ = conn.Close()
		return
	}
	if err := CheckDst(conn.LocalAddr(), dst); err != nil {
		slog.Warn("transparent-proxy: dropping self-referential connection (would self-loop)",
			"remote", conn.RemoteAddr().String(), "dst", dst,
			"local", conn.LocalAddr().String(), "error", err)
		_ = conn.Close()
		return
	}
	s.handle(conn, dst)
}

// ErrSelfReferential reports a recovered original destination that points back
// at this proxy. Returned by CheckDst.
var ErrSelfReferential = errors.New("transparentproxy: self-referential destination")

// CheckDst is defense-in-depth against a self-redirect loop. A genuinely
// REDIRECTed connection's original destination is the address the peer chose —
// for captured egress some external host, for captured ingress this pod's own
// IP on the app's port. In neither case is it this listener's own address, and
// in neither case is it loopback:
//
//   - a loopback dst means a direct dial to 127.0.0.1:<port>. The
//     enforce-redirect rules RETURN loopback before the REDIRECT, and loopback
//     traffic never traverses PREROUTING, so a real capture never has one.
//   - the listener's own address means a self-dial that slipped past the
//     iptables RETURN/exclusion rules (e.g. a misconfigured port-exclusion
//     list that fails to exempt the transparent port itself).
//
// Handing either to a handler would spiral into ever more connections and
// goroutines — tunnelled straight back into the listener that produced them.
// The iptables layer is the primary control; this is belt-and-suspenders.
//
// local may be nil, in which case only the loopback arm is checked.
func CheckDst(local net.Addr, dst string) error {
	if local != nil && dst == local.String() {
		return fmt.Errorf("%w: dst equals listener address %s", ErrSelfReferential, dst)
	}
	host, _, err := net.SplitHostPort(dst)
	if err != nil {
		return fmt.Errorf("transparentproxy: malformed destination %q: %w", dst, err)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return fmt.Errorf("%w: loopback dst %s", ErrSelfReferential, dst)
	}
	return nil
}
