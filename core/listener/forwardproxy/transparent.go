package forwardproxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/rossoctl/cortex/core/listener/egressguard"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/tlsbridge"
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
				"host", pipeline.SanitizeLogValue(name), "dst", dst)
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
		Path:      "", // no request line to read one from; explicit, as in handleConnect
		Headers:   http.Header{},
		Shared:    s.Shared,
		StartedAt: time.Now(),
	}
	// Pins ABSENCE here, and that is the point. A transparently redirected connection has
	// no HTTP request to read a header from, so the honest answer is "no client" — and
	// without the pin a plugin that wrote a User-Agent into these empty headers would give
	// the tunnel row an agent that never existed. An invented agent in a cost table reads
	// as a real program that spent real money, which is the failure
	// TestForwardProxy_NoUserAgentStaysAbsent exists for.
	pctx.ResolveClient()
	defer func() {
		s.OutboundPipeline.RunFinish(ctx, pctx, pipeline.OutcomeFromContext(pctx))
	}()

	// One identity for this connection, resolved once, exactly as on the request
	// and CONNECT paths. What an opaque redirected connection lacks is a
	// CLIENT-ASSERTED id — not an identity: ActiveSession() at the moment the
	// connection is gated is the answer, and resolving it again at recording time
	// would reopen the same flip window the other paths were fixed for, reduced to
	// ActiveSession@T0 versus ActiveSession@T1. resolvePluginSessionID with nil
	// headers is that same resolver with nothing to read a header from.
	sessionID := s.hydrateSession(pctx, "", nil, nil)

	// Gate on host/identity before opening the tunnel — identical to the
	// CONNECT path. Parsers see no body and degrade gracefully.
	action := s.OutboundPipeline.Run(ctx, pctx)
	if action.Type == pipeline.Reject {
		// Carry the identity this connection was gated under, so the denial lands
		// in the session whose traffic it was.
		s.recordOutboundReject(pctx, action, s.tunnelSessionID(sessionID, nil, nil))
		slog.Warn("transparent-proxy: outbound rejected by policy", "host", pipeline.SanitizeLogValue(host))
		return
	}

	// See handleConnect's pin: same rule, with no headers to read.
	if s.ClientAffinity && s.Sessions != nil && sessionID != "" {
		pctx.OutboundSessionID = sessionID
	}

	// This connection's two timeline rows; see handleConnect's tl. Never skipped here:
	// SkipHosts is matched on the CONNECT path only.
	tl := s.newTunnelLog(pctx, false)

	// Always dial the original IP (dst), never the sniffed name — the agent
	// already chose the IP, and re-resolving the name could diverge from it.
	//
	// The egress guard applies here too, dst being a literal IP: its own
	// check is over the literal, so no resolution can intervene. The refusal
	// is recorded but never SENT — this path speaks no HTTP to the client —
	// the same way the dial failure's synthetic 502 is.
	dialTarget := dst
	if s.EgressGuard != nil {
		checked, cerr := s.EgressGuard.CheckAddr(ctx, "tcp", dst)
		if cerr != nil {
			var ref *egressguard.Refusal
			if errors.As(cerr, &ref) {
				slog.Warn("transparent-proxy: destination refused by egress guard",
					// host is sniffed off the client's own bytes (SNI or Host header),
					// so it gets the sanitizer; dst is the kernel's SO_ORIGINAL_DST
					// literal and ref.Addr the guard's parsed address — neither is
					// client-controlled.
					"host", pipeline.SanitizeLogValue(host),
					"dst", dst, "addr", ref.Addr.String())
				// The 426 rule, as on both CONNECT paths: a non-zero StatusCode is
				// what OutcomeFromContext reads as an allow, and the guard did what
				// it was configured to. Without it the RunFinish defer above sees
				// StatusCode 0 and hands every Finisher an OutcomeError, so the
				// three listener paths disagreed about the same refusal — this one
				// reporting an error for what the docs call a delivered 403. No
				// status reaches the client here (this path speaks no HTTP to it);
				// the field exists for the Finishers and the row (#1539 review).
				pctx.StatusCode = http.StatusForbidden
				tl.open(pipeline.TunnelDestPrivate)
				tl.close(http.StatusForbidden, &pipeline.EventError{
					Kind:    pipeline.ErrorKindDestinationPrivate,
					Message: ref.Error(),
				}, 0, 0)
				return
			}
			tl.open(pipeline.TunnelDialFailed)
			tl.close(http.StatusBadGateway, dialError(cerr), 0, 0)
			return
		}
		dialTarget = checked
		pctx.DialedAddr = checked
	}
	upstream, err, memoized := s.dialUpstream(dialTarget, s.bridgeable(dst))
	// dialFail remembers a pre-dial that failed where the bridge could take the
	// connection: the connection continues with no upstream, because the bridge
	// can still take it and a plugin redirect can send the decrypted request to a
	// live server (#1595) — the same rule as handleConnect's CONNECT path.
	// freshDialFail is the failure when it came from a dial this connection made
	// itself, not a memo hit: a re-dial would only repeat it, so tunnelOrDial
	// closes with it instead.
	var dialFail, freshDialFail error
	if err != nil {
		if !s.bridgeable(dst) {
			slog.Warn("transparent-proxy: upstream dial failed", "host", pipeline.SanitizeLogValue(host), "dst", dst, "error", err)
			// The 502 is synthetic — this path sends the client no status at all — but the
			// failure is recorded as it is on CONNECT rather than leaving no trace.
			tl.open(pipeline.TunnelDialFailed)
			tl.close(http.StatusBadGateway, dialError(err), 0, 0)
			return
		}
		slog.Warn("transparent-proxy: upstream dial failed; trying the TLS bridge",
			"host", pipeline.SanitizeLogValue(host), "dst", dst, "error", err)
		dialFail = err
		if !memoized {
			freshDialFail = err
		}
	}
	defer func() {
		if upstream != nil {
			_ = upstream.Close()
		}
	}()

	if upstream != nil {
		enableKeepalive(upstream)
	}

	// The transparent path records before its own bridge decision for now, so it
	// reports only that the tunnel opened. Threading the reason through here too
	// means the same restructuring done for handleConnect; deliberately left for
	// a follow-up rather than half-done, since this listener is off by default
	// (--local skips it) and every reason it could report is already correct in
	// the log.
	tl.open("")

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
				if upstream != nil {
					_ = upstream.Close() // bridgeServe dials its own verified upstream; drop the pre-dial
				}
				// The open is already recorded above, so bridgeServe's own open calls are
				// no-ops here; what it still records through tl is the close.
				if s.bridgeServe(clientConn, host, key, tl, dialFail != nil) {
					return
				}
				// bridgeServe fell open (upstream-verify failed) → re-dial for the tunnel.
				// dialTarget: the checked address, never re-resolved — same rule as
				// handleConnect's fall-open re-dial. freshDialFail, when set, skips
				// the re-dial: the pre-dial already failed on a real dial.
				s.tunnelOrDial(clientConn, dialTarget, tl, freshDialFail)
				return
			}
			slog.Info("tls-bridge passthrough", "host", pipeline.SanitizeLogValue(key), "reason", reason)
		}
	}
	// The pre-dial failed (or was skipped as memoized-dead) and the bridge
	// declined the connection: dial now — a plain tunnel still needs a real
	// upstream, the same rule as handleConnect's. A fresh failure needs no second
	// dial; a memo hit is up to dialFailMemoTTL old, and the origin may have
	// come back.
	if upstream == nil {
		s.tunnelOrDial(clientConn, dialTarget, tl, freshDialFail)
		return
	}
	sent, received := tunnel(clientConn, upstream)
	tl.close(http.StatusOK, nil, sent, received)
}

// recordTunnelOpened emits the SessionRequest event for an opened opaque
// tunnel (CONNECT or transparent-redirect). Shared by handleConnect and
// HandleTransparentConn. MCP/Inference snapshots are nil by definition (the
// bytes are opaque); Invocations from gate plugins and plugin-public Plugins
// entries are still meaningful.
//
// It returns the bucket the row went to, so the tunnel's close row can land beside it;
// nil when session tracking is off.
func (s *Server) recordTunnelOpened(pctx *pipeline.Context, reason pipeline.TunnelReason) *session.Bucket {
	if s.Sessions == nil {
		return nil
	}
	// Always record the tunnel-open so passthrough/non-bridged tunnels (no
	// plugin activity) are still visible. For a TLS-bridged call agentop folds
	// this CONNECT event into the decrypted inner-request row.
	return s.appendTunnelOpen(pctx, s.tunnelOpenEvent(pctx, reason))
}

// appendTunnelOpen records an already-built open row ev under the session a tunnel's own
// row goes to, and returns its bucket; nil when session tracking is off. It is
// recordTunnelOpened's filing rule on its own, for a bridged tunnel that settles with no
// request having taken its deferred open (tunnelLog.settleLocked). Of pctx it reads only
// OutboundSessionID — which the listener pins before bridging and nothing writes after —
// and SessionSource, to recognize the connection's own trusted answer.
func (s *Server) appendTunnelOpen(pctx *pipeline.Context, ev pipeline.SessionEvent) *session.Bucket {
	if s.Sessions == nil {
		return nil
	}
	// Without client affinity this reads ActiveSession() at recording time, as it always
	// has, ignoring the identity the tunnel was gated under (#1187). With it, the pin set
	// after gating wins; see handleConnect. Process attribution pins it too, with the
	// client process's answer, and so does the connection itself: a session resolved from
	// how the connection arrived is not a knob like affinity, so its pin is honored even
	// with affinity and process attribution both off (#1569 review).
	var sid string
	if s.ClientAffinity || s.processesOn() || pctx.SessionSource == pipeline.SessionSourceConnection {
		sid = pctx.OutboundSessionID
	}
	if sid == "" {
		sid = s.Sessions.ActiveSession()
	}
	if sid == "" {
		sid = session.DefaultSessionID
	}
	return s.Sessions.AppendBucket(sid, ev)
}

// tunnelOpenEvent is a tunnel's open row, for recordTunnelOpened and for a bridged
// tunnel's deferred open, which tunnelLog.deferOpen builds before any handler runs.
func (s *Server) tunnelOpenEvent(pctx *pipeline.Context, reason pipeline.TunnelReason) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		At:          time.Now(),
		Direction:   pipeline.Outbound,
		Phase:       pipeline.SessionRequest,
		RequestID:   pctx.RequestID(),
		Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Plugins:     pipeline.SnapshotPlugins(pctx.Extensions.Custom),
		Identity:    pipeline.SnapshotIdentity(pctx),
		Host:        pctx.Host,
		// Method is CONNECT — real on a proxied CONNECT, synthetic on a
		// transparently redirected connection — and Path is empty: the bytes
		// are opaque, so there is no request line to read one from. A blank
		// path on this row is the accurate answer, not a gap.
		HTTPMethod: pctx.Method,
		HTTPPath:   pctx.Path,
		// Explicit opaque-tunnel marker so agentop can fold this CONNECT into
		// the decrypted inner request without inferring "tunnel" from shape.
		Tunnel: true,
		// Why the bytes stayed opaque. The caller knows; this function does
		// not, which is why it is a parameter rather than something derived
		// here from host shape.
		TunnelReason: reason,
		// The address the guard checked and the tunnel dialed — set only when
		// the egress guard is on, else absent (SessionEvent.DialAddr). On the
		// open row, which is where a reader pairs it with the Host the client
		// named; the close row pairs by RequestID and needs no copy.
		DialAddr:       pctx.DialedAddr,
		Client:         pctx.ClientInfo(),
		Subagent:       pctx.Subagent(),
		ParentSubagent: pctx.ParentSubagent(),
		Proc:           pctx.AttributedProc(),
	}
}

// recordTunnelClosed emits the SessionResponse row for a tunnel that ended, or that
// never opened because the destination could not be dialed. It shares the open's
// RequestID, which is how agentop pairs the two into one exchange with a STATUS and a
// DURATION.
//
// b is the bucket the open was recorded under, reused rather than re-resolved: a long
// tunnel closes minutes after it opened, when whichever session spoke last says nothing
// about whose tunnel this was.
//
// statusCode is the status the proxy answered the CONNECT with — 200 once the tunnel was
// established, however it later ended, and 502 when the destination could not be dialed
// at all. The transparent listener sends no status of its own, so there it is synthetic,
// as that path's CONNECT method already is. fail says why a tunnel failed; nil when it
// simply ran until one side closed. up and down are the bytes it carried, zero where
// nothing was counted — a bridged tunnel's bytes were TLS the bridge terminated.
//
// No Invocations and no Plugins: nothing runs on a tunnel's response, so there is
// nothing to snapshot that the open row does not already carry.
func (s *Server) recordTunnelClosed(pctx *pipeline.Context, b *session.Bucket, reason pipeline.TunnelReason, statusCode int, fail *pipeline.EventError, up, down int64) {
	if s.Sessions == nil {
		return
	}
	s.Sessions.AppendTrailing(b, pipeline.SessionEvent{
		At:             time.Now(),
		Direction:      pipeline.Outbound,
		Phase:          pipeline.SessionResponse,
		RequestID:      pctx.RequestID(),
		Identity:       pipeline.SnapshotIdentity(pctx),
		Host:           pctx.Host,
		HTTPMethod:     pctx.Method,
		HTTPPath:       pctx.Path,
		StatusCode:     statusCode,
		Error:          fail,
		Duration:       pipeline.DurationSince(pctx.StartedAt),
		Tunnel:         true,
		TunnelReason:   reason,
		BytesUp:        up,
		BytesDown:      down,
		Client:         pctx.ClientInfo(),
		Subagent:       pctx.Subagent(),
		ParentSubagent: pctx.ParentSubagent(),
		Proc:           pctx.AttributedProc(),
	})
}

// tunnel bidirectionally copies between two connections, relaying each
// direction's EOF as a CloseWrite on the receiving side so a half-close
// survives. Shared by handleConnect and HandleTransparentConn.
//
// It returns how many bytes crossed each way — up is client to upstream, down
// is upstream to client — for the tunnel's close row. After the first copy
// returns, the second gets halfCloseDrainVar of idle time before both ends are
// full-closed, so a reply that keeps streaming extends the wait and finishes
// without truncation.
func tunnel(client, upstream net.Conn) (up, down int64) {
	var upBytes, downBytes atomic.Int64
	upc := make(chan int64, 1)
	downc := make(chan int64, 1)
	go func() {
		n, _ := io.Copy(&countingWriter{w: upstream, n: &upBytes}, client)
		closeWrite(upstream) // relay the client's half-close; let the reply flow back
		upc <- n
	}()
	go func() {
		n, _ := io.Copy(&countingWriter{w: client, n: &downBytes}, upstream)
		closeWrite(client)
		downc <- n
	}()
	select {
	case up = <-upc:
		down = drainTunnel(&downBytes, downc, client, upstream)
	case down = <-downc:
		up = drainTunnel(&upBytes, upc, client, upstream)
	}
	_ = client.Close()
	_ = upstream.Close()
	return up, down
}

// drainTunnel waits for the second copy, extending the wait while bytes keep
// flowing (seen via n). Idle for halfCloseDrainVar → full-close both ends to
// unblock the copy, read its total, return.
func drainTunnel(n *atomic.Int64, done <-chan int64, client, upstream net.Conn) int64 {
	last := n.Load()
	tick := time.NewTicker(halfCloseDrainVar / 4)
	defer tick.Stop()
	idle := time.Duration(0)
	for {
		select {
		case total := <-done:
			return total
		case <-tick.C:
			now := n.Load()
			if now != last {
				last, idle = now, 0
				continue
			}
			idle += halfCloseDrainVar / 4
			if idle >= halfCloseDrainVar {
				_ = client.Close()
				_ = upstream.Close()
				return <-done
			}
		}
	}
}

// countingWriter forwards writes to w and records the running byte count in n.
// Used by tunnel so drainTunnel can see progress mid-copy.
type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	m, err := c.w.Write(p)
	c.n.Add(int64(m))
	return m, err
}

// halfCloseDrainVar is how long tunnel waits for the second copy after the
// first has returned. Short enough to settle the tunnel's close row promptly,
// long enough to carry a reply the peer is actively writing. A var so a test
// can shrink it; a test that mutates it relies on package tests running
// sequentially, since tunnel reads it on every teardown.
var halfCloseDrainVar = 2 * time.Second

// closeWrite shuts down c's write side when the underlying conn supports it
// (TCPConn, UnixConn), so the peer sees EOF on its reads without losing the
// other direction. Falls back to a full close for anything that does not.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}
