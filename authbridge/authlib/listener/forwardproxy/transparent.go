package forwardproxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/tlsbridge"
)

// HandleTransparentConn processes one outbound connection captured by an
// iptables REDIRECT (proxy-sidecar enforce-redirect mode). It is the
// transparent-listener analogue of handleConnect, and shares its semantics:
// the same outbound pipeline gates the connection on destination/identity, and
// the bytes are then blind-tunnelled, preserving the agent's end-to-end TLS
// (token-exchange and protocol parsers are no-ops on opaque TLS, exactly as on
// the CONNECT path).
//
// The crucial difference from handleConnect: there is NO HTTP CONNECT request.
// The agent believes it is talking directly to dst, so the proxy must emit no
// protocol bytes back — no "200 Connection Established", no hijack. It simply
// gates, dials dst, and copies bytes both ways. dst is "host:port" recovered
// from SO_ORIGINAL_DST by the transparent listener.
//
// HOSTNAME RECOVERY: CONNECT carries a hostname in r.Host, but SO_ORIGINAL_DST
// yields only an IP:port. To give host/domain egress policy parity with the
// CONNECT path, we sniff the connection's first bytes for the destination name
// — the TLS ClientHello SNI for HTTPS, or the HTTP Host header for plaintext
// HTTP — and use it as pctx.Host. If neither can be recovered we fall back to
// the IP. The dial target ALWAYS stays the SO_ORIGINAL_DST IP (dst); the name is
// only the policy key.
//
// Trust caveat (relevant before enforce-redirect goes always-on): for captured
// traffic the agent controls both the SNI/Host and, separately, the IP the
// bytes actually go to, so a *malicious* agent could present an allowed name
// while connecting to another IP. Name-based policy here is therefore reliable
// against a cooperative/misconfigured agent (the motivating case) but is not a
// hard control against a hostile one — only the IP is ground truth. Hard
// enforcement would need IP-set allowlists or SNI/cert cross-checks.
//
// SkipHosts is intentionally NOT consulted here. listener.skip_hosts is an
// ops convenience for the cooperative-egress paths (forward proxy + ext_proc),
// where bypassing the pipeline on infrastructure traffic is fine because the
// agent is trusted to honor HTTP_PROXY anyway. The transparent path exists
// precisely as the hard egress guard against agents that route around the
// cooperative paths, and pctx.Host here is recovered from agent-controlled
// SNI/Host bytes on the wire — making it self-exemptable would defeat the
// reason this listener exists. If you find yourself wanting to add a
// SkipHosts check here to "match the other listeners," don't — that's the
// failure mode this comment is explicitly trying to prevent.
//
// HandleTransparentConn owns clientConn's lifecycle and always closes it.
func (s *Server) HandleTransparentConn(clientConn net.Conn, dst string) {
	defer func() { _ = clientConn.Close() }()

	// Keepalive on the raw client conn before sniffing wraps it (the wrapper is
	// not a *net.TCPConn, so enableKeepalive would no-op on it).
	enableKeepalive(clientConn)

	// Recover the destination hostname for policy parity with CONNECT. Gated to
	// HTTP/TLS ports so non-HTTP protocols are not delayed by the peek. The dial
	// target stays dst (the IP); only pctx.Host gets the recovered name.
	host := dst
	// Sniff on the standard HTTP/TLS ports, OR on whatever ports the TLS bridge
	// is configured to intercept — so a configured non-standard bridge port
	// (e.g. 9443) still gets the peekable conn the bridge branch needs. The
	// bridge's own port set is the single source of truth (no drift with
	// shouldSniff's heuristic list).
	if shouldSniff(dst) || (s.TLSBridge != nil && s.TLSBridge.Decision.HandlesPort(portOf(dst))) {
		name, wrapped := sniffHost(clientConn)
		clientConn = wrapped
		if name != "" {
			if _, port, err := net.SplitHostPort(dst); err == nil {
				host = net.JoinHostPort(name, port)
			}
			slog.Debug("transparent-proxy: recovered destination host for policy",
				"host", name, "dst", dst)
		}
	}

	// Background context: there is no inbound *http.Request to tie cancellation
	// to. Tunnel teardown (either side closing) is what ends the connection;
	// the pipeline Run/Finish calls are short and don't need request scoping.
	ctx := context.Background()

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    http.MethodConnect, // synthetic: opaque tunnel, parity with handleConnect
		Scheme:    "tcp",              // marker: bytes are opaque, not HTTP
		Host:      host,
		Headers:   http.Header{},
		Shared:    s.Shared,
		StartedAt: time.Now(),
	}
	defer func() {
		s.OutboundPipeline.RunFinish(ctx, pctx, pipeline.OutcomeFromContext(pctx))
	}()

	if s.Sessions != nil {
		if aid := s.Sessions.ActiveSession(); aid != "" {
			pctx.Session = s.Sessions.View(aid)
		}
	}

	// Gate on host/identity before opening the tunnel — identical to the
	// CONNECT path. Parsers see no body and degrade gracefully.
	action := s.OutboundPipeline.Run(ctx, pctx)
	if action.Type == pipeline.Reject {
		s.recordOutboundReject(pctx, action)
		slog.Warn("transparent-proxy: outbound rejected by policy", "host", host)
		return
	}

	// Always dial the original IP (dst), never the sniffed name — the agent
	// already chose the IP, and re-resolving the name could diverge from it.
	upstream, err := net.DialTimeout("tcp", dst, connectDialTimeout)
	if err != nil {
		slog.Warn("transparent-proxy: upstream dial failed", "host", host, "dst", dst, "error", err)
		return
	}
	defer func() { _ = upstream.Close() }()

	enableKeepalive(upstream)

	s.recordTunnelOpened(pctx)

	if s.TLSBridge != nil {
		// host is the policy authority: "<sniffed-SNI>:port" when a name was
		// recovered, else dst ("<dial-IP>:port"). key is the SNI name or dial IP.
		key := hostOnly(host)
		var first []byte
		if pc, ok := clientConn.(*peekedConn); ok {
			first, _ = pc.Peek(5)
		}
		if !s.TLSBridge.Skip.Contains(key) {
			v, reason := s.TLSBridge.Decision.Classify(key, portOf(dst), first)
			if v == tlsbridge.Terminate {
				_ = upstream.Close() // bridgeServe dials its own verified upstream; drop the pre-dial
				if s.bridgeServe(clientConn, host, key) {
					return
				}
				// bridgeServe fell open (upstream-verify failed) → re-dial for the tunnel.
				if up2, derr := net.DialTimeout("tcp", dst, connectDialTimeout); derr == nil {
					tunnel(clientConn, up2)
					_ = up2.Close()
				}
				return
			}
			slog.Info("tls-bridge passthrough", "host", key, "reason", reason)
		}
	}
	tunnel(clientConn, upstream)
}

// recordTunnelOpened emits the SessionRequest event for an opened opaque
// tunnel (CONNECT or transparent-redirect). Shared by handleConnect and
// HandleTransparentConn. MCP/Inference snapshots are nil by definition (the
// bytes are opaque); Invocations from gate plugins and plugin-public Plugins
// entries are still meaningful.
func (s *Server) recordTunnelOpened(pctx *pipeline.Context) {
	if s.Sessions == nil {
		return
	}
	sid := s.Sessions.ActiveSession()
	if sid == "" {
		sid = session.DefaultSessionID
	}
	plugins := pipeline.SnapshotPlugins(pctx.Extensions.Custom)
	ev := pipeline.SessionEvent{
		At:          time.Now(),
		Direction:   pipeline.Outbound,
		Phase:       pipeline.SessionRequest,
		Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Plugins:     plugins,
		Identity:    pipeline.SnapshotIdentity(pctx),
		Host:        pctx.Host,
		// Explicit opaque-tunnel marker so abctl can fold this CONNECT into
		// the decrypted inner request without inferring "tunnel" from shape.
		Tunnel: true,
	}
	// Always record the tunnel-open so passthrough/non-bridged tunnels (no
	// plugin activity) are still visible. For a TLS-bridged call abctl folds
	// this CONNECT event into the decrypted inner-request row.
	s.Sessions.Append(sid, ev)
}

// tunnel bidirectionally copies between two connections until either side
// closes, then propagates the close to the other so both io.Copy goroutines
// exit. Close-on-each-side is idempotent on net.Conn. Shared by handleConnect
// and HandleTransparentConn.
func tunnel(a, b net.Conn) {
	go func() {
		_, _ = io.Copy(b, a)
		_ = b.Close()
		_ = a.Close()
	}()
	_, _ = io.Copy(a, b)
	_ = a.Close()
	_ = b.Close()
}
