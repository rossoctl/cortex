package forwardproxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/attribution"
	"github.com/rossoctl/cortex/core/listener/egressguard"
	"github.com/rossoctl/cortex/core/listener/httpx"
	"github.com/rossoctl/cortex/core/listener/internal/sessionevent"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// dialFailMemo is a memoized failed CONNECT pre-dial: the error and when it was
// taken. dialFailMemoTTL bounds how long it is believed.
type dialFailMemo struct {
	err error
	at  time.Time
}

// dialFailMemoTTL is how long a failed pre-dial is remembered, so a destination
// that drops packets — where a dial burns the whole connectDialTimeout — is not
// re-dialed per CONNECT. Bridge-enabled CONNECTs only: there a skipped dial is
// re-attempted wherever its result would have been used (the plain-tunnel path
// dials fresh in tunnelOrDial; the bridge path re-originates to a different host
// and dials that), so the staleness it can buy is one skipped dial, never a
// misrouted request. Without the bridge the 502 is the last word, so a memo hit
// would keep answering 502 for an origin that came back within the TTL. A stale
// hit also skips the origin probe (bridgeServe), not just the dial: an origin
// that comes back within the TTL with a certificate the bridge cannot verify is
// bridged rather than passed through, and the request fails at re-origination —
// bounded by the TTL, and the failure is loud rather than silent.
const dialFailMemoTTL = 30 * time.Second

// dialTCP is net.DialTimeout; a variable so tests can count dials.
var dialTCP = net.DialTimeout

// dialUpstream is a CONNECT's pre-dial, memoizing failures per target for
// dialFailMemoTTL (#1595: a client that opens a CONNECT per request must not pay
// a dead destination's dial timeout every time). bridgeable gates the memo: where
// the bridge cannot take the connection the failure is answered with a 502, not
// skipped, so remembering it would only pin that 502 past an origin that came
// back. The final return says whether the failure came from the memo rather than
// a dial this call made — a caller that would re-dial (tunnelOrDial) should not,
// because a failure from its own dial is already the answer.
func (s *Server) dialUpstream(dialTarget string, bridgeable bool) (net.Conn, error, bool) {
	if bridgeable {
		s.dialMu.Lock()
		if s.dialFails == nil {
			s.dialFails = make(map[string]dialFailMemo)
		}
		if m, ok := s.dialFails[dialTarget]; ok {
			if time.Since(m.at) < dialFailMemoTTL {
				s.dialMu.Unlock()
				return nil, m.err, true
			}
			delete(s.dialFails, dialTarget)
		}
		s.dialMu.Unlock()
	}

	conn, err := dialTCP("tcp", dialTarget, connectDialTimeout)
	if err == nil {
		return conn, nil, false
	}
	if bridgeable {
		s.dialMu.Lock()
		// Sweep while inserting: an entry is otherwise deleted only when its own
		// target is CONNECTed again after the TTL, so distinct dead targets — a
		// scanner's stock in trade — would grow the map without bound.
		now := time.Now()
		for k, m := range s.dialFails {
			if now.Sub(m.at) >= dialFailMemoTTL {
				delete(s.dialFails, k)
			}
		}
		s.dialFails[dialTarget] = dialFailMemo{err: err, at: now}
		s.dialMu.Unlock()
	}
	return nil, err, false
}

// tunnelOrDial serves a tunnel whose pre-dialed upstream is gone — the bridge
// declined the connection, or fell open after the origin probe failed — by dialing
// fresh: a working dial tunnels normally, a failing one closes the client with the
// dial error on the close row. The client already has its 200, so that stays the
// status and the error says why the tunnel died. fail, when non-nil, is a failure
// the pre-dial produced on a real dial: re-dialing would only repeat it, so the
// tunnel closes with it instead.
func (s *Server) tunnelOrDial(clientConn net.Conn, dialTarget string, tl *tunnelLog, fail error) {
	if fail != nil {
		_ = clientConn.Close()
		tl.close(http.StatusOK, dialError(fail), 0, 0)
		return
	}
	up2, derr := dialTCP("tcp", dialTarget, connectDialTimeout)
	if derr != nil {
		_ = clientConn.Close()
		tl.close(http.StatusOK, dialError(derr), 0, 0)
		return
	}
	sent, received := tunnel(clientConn, up2)
	_ = up2.Close()
	tl.close(http.StatusOK, nil, sent, received)
}

// misdirectedBody is the 421's body. Static: the Host it refuses is the client's own
// input, and echoing it back would make the proxy a reflector.
const misdirectedBody = "misdirected request: the Host header does not name this tunnel's destination\n"

// refuseMisdirected answers a bridged request whose Host names another authority than
// the tunnel's with 421 Misdirected Request (RFC 9110 §15.5.20), without running the
// pipeline or contacting any upstream. The rows it records name the tunnel's authority
// as the host, since that is where the request would have gone; the Host the client
// sent goes in the error message only, and carries no path or query.
func (s *Server) refuseMisdirected(w http.ResponseWriter, r *http.Request, pctx *pipeline.Context, tl *tunnelLog) {
	claimed := r.Host
	if len(claimed) > 255 {
		claimed = claimed[:255]
	}
	slog.Warn("forward-proxy: refused a bridged request whose Host is not the tunnel's",
		"tunnel", pipeline.SanitizeLogValue(r.URL.Host), "host", pipeline.SanitizeLogValue(claimed), "method", r.Method)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusMisdirectedRequest)
	_, _ = io.WriteString(w, misdirectedBody)

	if s.Sessions == nil {
		return
	}
	pctx.Host = r.URL.Host
	chain := s.clientChain(r)
	sid := s.attributeForRecording(pctx, s.trustedSessionOf(r), r.Header, chain)
	pctx.OutboundSessionID = sid
	if chain != nil {
		defer s.touch(sid, chain)
	}
	s.recordOutboundRequestEvent(tl, pctx, sid)
	s.recordOutboundResponseEvent(pctx, http.StatusMisdirectedRequest, &pipeline.EventError{
		Kind:    "misdirected_request",
		Message: fmt.Sprintf("Host %q does not name the tunnel's destination %s", claimed, r.URL.Host),
	})
}

// tunnelAuthorityMatches reports whether host, a bridged request's Host, names the same
// authority as tunnel, the CONNECT target or transparent destination it was decrypted
// from. Hosts compare case-insensitively without a trailing dot, IP literals by value,
// and a missing port is 443, the tunnel being TLS.
func tunnelAuthorityMatches(host, tunnel string) bool {
	h, hp, ok := splitAuthority(host)
	if !ok {
		return false
	}
	t, tp, ok := splitAuthority(tunnel)
	return ok && h == t && hp == tp
}

func splitAuthority(a string) (host string, port int, ok bool) {
	host, p, err := net.SplitHostPort(a)
	if err != nil {
		// No port: a name, an IPv4 literal, or a bracketed IPv6 literal.
		host, p = a, ""
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			host = host[1 : len(host)-1]
		}
	}
	port = 443
	if p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, false
		}
		port = n
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.Unmap().String()
	}
	return host, port, host != ""
}

// connectDialTimeout bounds the upstream TCP dial for a CONNECT tunnel.
// Once the tunnel is open the timeout no longer applies — the agent's TLS
// handshake and subsequent traffic flow at their own pace.
//
// 10s, not the 30s it long was, because the only dials that wait it out are
// black-holed ones (#1251): a refused or unroutable destination answers a TCP
// SYN with an RST in well under a second, while a host that drops the SYN
// silently — off the VPN that reaches it, behind a dead NAT — consumes the
// whole budget and the agent spends it staring at nothing. By 10s the initial
// SYN and its first four scheduled retransmits (1s, 2s, 4s, 8s) have all gone
// unanswered; a handshake that misses five straight tries is not being saved
// by a sixth at 16s, so the rest of the budget bought no connection, only
// latency before the 502. Matches the budget the bridge already trusts for
// its own upstream probe (upstreamVerifyTimeout) and the mTLS dialer's plain
// dial, which had settled on 10s independently.
const connectDialTimeout = 10 * time.Second

// handleConnect tunnels HTTPS (and any other TLS-wrapped protocol) through
// the forward proxy as raw TCP. Mirrors the TLS-passthrough behavior of
// envoy-sidecar mode: bytes are opaque to the proxy, so token-exchange and
// the protocol parsers (mcp-parser, inference-parser) are no-ops by
// definition. Pipeline gates (ibac, jwt-validation bypass logic, etc.)
// still run on the CONNECT request itself so they can reject based on
// destination host before the tunnel opens.
//
// mTLS is intentionally NOT applied to the upstream dial — the bytes
// flowing through this tunnel ARE the agent's own end-to-end TLS, and
// terminating that with sidecar-to-sidecar mTLS would break the agent's
// trust path. CONNECT targets are opaque externals (LiteMaaS, Bedrock,
// GitHub API, etc.) where the agent's existing TLS is the right answer.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	s.noteTunnel()
	attribution.Strip(r.Header)
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    r.Method, // always "CONNECT" here, but populated for parity with handleRequest
		Scheme:    "tcp",    // marker: bytes are opaque, not HTTP
		Host:      r.Host,
		Path:      "",
		Headers:   r.Header.Clone(),
		StartedAt: time.Now(),
	}
	// At construction, as in serveOutbound: the gate plugins run on this CONNECT before
	// recordTunnelOpened builds its event, so the pin is what keeps the tunnel row
	// attributed to the agent that opened it.
	pctx.ResolveClient()

	// The connection's own answer, resolved once for this CONNECT — the egress
	// refusal below is recorded before hydration, so it needs the same trusted
	// sid the hydrated path gets (see serveOutbound's, same reasoning).
	trusted := s.trustedSessionOf(r)

	// Egress allow list (#1483): when on, an off-list CONNECT target is refused
	// here — before the SkipHosts match, so a skip entry cannot exempt an
	// off-list host (the allowlist is the outer boundary), and before the
	// pipeline and the dial, so the destination is neither resolved nor
	// contacted: the refusal happens at a point Go's resolver has not run
	// (it runs inside net.DialTimeout, which this return never reaches), which
	// is the conformance contract's "no DNS, no connect" by construction. The
	// named 403 is written with plain WriteRejection, never
	// WriteRejectionForRequest: the egress boundary's answer is the contract,
	// and the MCP JSON-RPC 200 rewrite would hide it behind a protocol frame.
	if s.EgressAllow != nil && !s.EgressAllow.Allows(r.Host, "tcp") {
		host := hostOnly(r.Host)
		action := egressAllowRefusal(pctx, host)
		// Session resolution the recordUnbufferedRequest way — pre-pipeline,
		// from the connection's own answer first, then the client's headers and
		// process — so the denied row lands in the bucket the request would have
		// recorded under. The trusted sid has to come first here for the same
		// reason it does on the plain path: this CONNECT is refused before
		// hydration, and a VM's denied row filed by header or process lands in
		// another VM's session (#1569 review).
		chain := s.clientChain(r)
		s.recordOutboundReject(pctx, action, s.recordingSessionID(trusted, r.Header, chain))
		// Sanitized like the serveOutbound refusal: host is client-controlled and
		// lands in a log file.
		slog.Info("forward-proxy: CONNECT destination refused by egress allow list",
			"host", pipeline.SanitizeLogValue(host))
		httpx.WriteRejection(w, action)
		return
	}

	// SkipHosts short-circuit: open the tunnel without running the
	// pipeline or recording a session event. The pipeline never ran,
	// so there's nothing to RunFinish — defer is suppressed. Mirrors
	// handleRequest's skip path so HTTP and CONNECT-tunnel destinations
	// that match a skip pattern behave identically. Note the gate
	// plugin loss this implies: if your skip-host list includes a
	// destination you'd want IBAC or token-exchange to deny on, that
	// denial does not happen — the SkipHosts list is a "trusted
	// infrastructure" surface, not a generic per-route policy knob.
	//
	// CONNECT is safer-by-construction than the HTTP path: r.Host on
	// CONNECT is the dial target, so a forged Host header cannot
	// skip-match while dialing elsewhere — the proxy dials the same
	// "host:port" it matched. We still emit an audit log so a
	// successful skip leaves a trace.
	pat, skipped := s.SkipHosts.MatchPattern(pctx.Host)
	if skipped {
		slog.Info("forward-proxy: skip_hosts match (CONNECT) — opening tunnel without pipeline + recording",
			"host", pipeline.SanitizeLogValue(pctx.Host), "pattern", pat)
	}

	var sessionID string
	var chain []attribution.Proc
	if !skipped {
		defer func() {
			s.OutboundPipeline.RunFinish(r.Context(), pctx, pipeline.OutcomeFromContext(pctx))
		}()

		// Same one-identity rule as the request path, and the same local-variable
		// reason: the plugins that gate this tunnel and the bucket its denial
		// records under must be the same session, and the pipeline runs in
		// between, so the answer cannot live in a field a plugin can write. A
		// CONNECT request carries the client's own headers, so a client that
		// announces its session on CONNECT is honored; most do not, and those
		// fall through to ActiveSession() exactly as before.
		chain = s.clientChain(r)
		// Same widening as the request path: the trusted answer needs no store,
		// and a VM's tunnel must carry its identity even with the store off.
		if s.Sessions != nil || trusted != "" {
			sessionID = s.hydrateSession(pctx, trusted, r.Header, chain)
		}

		// Run the outbound pipeline. Plugins that policy on host/identity
		// (ibac, content gates) still get to allow/deny; plugins that need
		// HTTP body (parsers) see no body, which they handle gracefully.
		action := s.OutboundPipeline.Run(r.Context(), pctx)
		if action.Type == pipeline.Reject {
			s.recordOutboundReject(pctx, action, s.tunnelSessionID(sessionID, r.Header, chain))
			// Render as a JSON-RPC error frame when the rejected
			// request was MCP JSON-RPC, so the agent's MCP client
			// surfaces this as one failed tool call rather than a
			// transport break. Falls through to plain HTTP-level
			// rejection for non-MCP traffic.
			httpx.WriteRejectionForRequest(w, action, pctx)
			return
		}
	}

	// Under client affinity an opaque tunnel's row joins the session this CONNECT was
	// gated under, not ActiveSession() at recording time. A bridged tunnel's open follows
	// its first recorded request instead (tunnelLog.recordWith); only one that records no
	// request files under this pin, when it settles. Pinned only now, after the pipeline,
	// for the reason sessionID is a local; left unpinned where affinity had no answer, for
	// the reason in tunnelSessionID. See recordTunnelOpened. With process attribution the
	// pin is the process's answer, which is never empty. A trusted connection answer is
	// pinned ahead of both, whatever those knobs say: identity from the connection must
	// not need session.client_affinity to reach a tunnel's own rows (#1569 review).
	if !skipped && s.Sessions != nil {
		switch {
		case trusted != "":
			pctx.OutboundSessionID = trusted
		case chain != nil && s.processesOn():
			// The client's process is known, so the tunnel's own row goes where that
			// process's requests go — default included, for a process of no agent.
			pctx.OutboundSessionID = s.recordingSessionID(sessionID, r.Header, chain)
		case s.ClientAffinity && sessionID != "":
			pctx.OutboundSessionID = sessionID
		}
	}

	// Verify hijack capability BEFORE dialing upstream. If hijacking
	// isn't supported the failure mode should be a 500 to the client,
	// not a half-opened TCP connection to the upstream. The actual
	// Hijack() call happens after dial succeeds — http.Error needs an
	// un-hijacked ResponseWriter to deliver the dial-failure 502.
	if _, ok := w.(http.Hijacker); !ok {
		slog.Error("forward-proxy: ResponseWriter does not support hijacking", "host", pipeline.SanitizeLogValue(r.Host))
		http.Error(w, `{"error":"connect not supported by listener"}`, http.StatusInternalServerError)
		return
	}

	// tl records this CONNECT's two timeline rows. The open is recorded exactly once, on
	// whichever path the CONNECT takes, carrying the reason the bytes stayed opaque; it
	// is not recorded here because the reason is not known yet — recording before the
	// bridge decision made the two most useful reasons unrepresentable, since both are
	// discovered inside bridgeServe. The close is recorded when the tunnel ends, or at
	// once when it never opened. Neither is recorded for a SkipHosts destination: no
	// plugin ran, so there is nothing to attribute them to.
	tl := s.newTunnelLog(pctx, skipped)
	tl.conn = connProcOf(r.Context())
	// Kept for AdoptBridgedRequest: the requests this tunnel decrypts are parsed
	// fresh and inherit nothing from here, so a connection-derived identity has
	// to be re-stamped from the CONNECT onto each of them.
	tl.connect = r

	// Plain TCP dial. See package-level comment on why mTLS doesn't
	// apply here. r.Host on a CONNECT carries "host:port" already.
	//
	// dialTarget is what actually gets dialed: r.Host itself when the guard is
	// off, else the address the guard checked — resolved once here, and reused
	// verbatim by the bridge-fall-open re-dial below, so no second resolution
	// can ever re-answer between check and connect (the DNS-rebinding window).
	dialTarget := r.Host
	bridgeable := s.bridgeable(r.Host)
	var resolveErr error // a non-refusal CheckAddr failure: the name would not resolve
	if s.EgressGuard != nil {
		checked, cerr := s.EgressGuard.CheckAddr(r.Context(), "tcp", r.Host)
		if cerr != nil {
			var ref *egressguard.Refusal
			if errors.As(cerr, &ref) {
				slog.Warn("forward-proxy: CONNECT destination refused by egress guard",
					// Sanitized like the Debug line in serveOutbound: r.Host is
					// client-controlled and lands in a log file.
					"host", pipeline.SanitizeLogValue(r.Host), "addr", ref.Addr.String())
				// A policy answer, not a failed exchange. skip_hosts destinations
				// are refused too — a bypass must not become an SSRF bypass — and
				// tl's rows are no-ops for them, exactly as the dial failure's are.
				httpx.WriteRejection(w, pipeline.DenyStatus(http.StatusForbidden, "destination_private",
					fmt.Sprintf("refusing to reach private destination %s (%s)", ref.Host, ref.Addr)))
				// The 426 rule again: a delivered 403 the Finisher reads as an
				// allow, because the guard did what it was configured to.
				pctx.StatusCode = http.StatusForbidden
				tl.open(pipeline.TunnelDestPrivate)
				tl.close(http.StatusForbidden, &pipeline.EventError{
					Kind:    pipeline.ErrorKindDestinationPrivate,
					Message: ref.Error(),
				}, 0, 0)
				return
			}
			// Not a refusal: the name would not resolve. Same shape as a dial
			// failure — nothing was dialed, and the client gets the 502. Where the
			// bridge could take the connection, fall through instead, carrying the
			// resolution error as the pre-dial's failure: the bridge can still
			// bridge it, and a redirect can move the request to a name that
			// resolves (#1595). The empty checked address is never dialed.
			// Both values sanitized: the name is client-controlled, and the error
			// embeds it (net.DNSError quotes the name it failed to resolve).
			if !bridgeable {
				slog.Warn("forward-proxy: CONNECT destination resolution failed",
					"host", pipeline.SanitizeLogValue(r.Host),
					"error", pipeline.SanitizeControlRunes(cerr.Error()))
				http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
				tl.open(pipeline.TunnelDialFailed)
				tl.close(http.StatusBadGateway, dialError(cerr), 0, 0)
				return
			}
			resolveErr = cerr
		}
		dialTarget = checked
		// The audit trail against rebinding: "asked for the name, dialed this
		// address" — moca's egress conformance reads it off the tunnel open row.
		pctx.DialedAddr = checked
	}
	var upstream net.Conn
	var memoized bool
	err := resolveErr
	if err == nil {
		upstream, err, memoized = s.dialUpstream(dialTarget, bridgeable)
	}
	// dialFail remembers a pre-dial that failed where the bridge could take the
	// connection: the connection continues with no upstream, because the bridge
	// can still take it — a plugin redirect (inference-router moving a pinned
	// session) sends the decrypted request to a live server, and an opaque tunnel
	// to a dead origin helps nobody (#1595). Every path below that would have used
	// the upstream handles a nil one by closing with this error or re-dialing
	// (tunnelOrDial). freshDialFail is the failure when it came from a dial this
	// connection made itself, not a memo hit: re-dialing would only repeat it
	// behind the 200 the client already has, so tunnelOrDial closes with it.
	var dialFail, freshDialFail error
	if err != nil {
		if !bridgeable {
			slog.Warn("forward-proxy: CONNECT upstream dial failed", "host", pipeline.SanitizeLogValue(r.Host), "error", err)
			http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
			// Recorded rather than dropped: an unreachable destination used to leave no
			// trace in the timeline at all.
			tl.open(pipeline.TunnelDialFailed)
			tl.close(http.StatusBadGateway, dialError(err), 0, 0)
			return
		}
		slog.Warn("forward-proxy: CONNECT upstream dial failed; trying the TLS bridge",
			"host", pipeline.SanitizeLogValue(r.Host), "error", err)
		dialFail = err
		if !memoized {
			freshDialFail = err
		}
	}

	clientConn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		if upstream != nil {
			_ = upstream.Close()
		}
		slog.Error("forward-proxy: CONNECT hijack failed", "host", pipeline.SanitizeLogValue(r.Host), "error", err)
		return
	}

	// TCP keepalive on both ends. Streaming LLM completions can hold
	// the tunnel open for minutes; without keepalives, a vanished peer
	// (network partition, NAT entry expiry, peer reboot) parks the
	// io.Copy goroutines until the OS finally times the socket out.
	// 30s is loose enough to not perturb idle traffic and tight enough
	// that operators get prompt cleanup on dead connections.
	if upstream != nil {
		enableKeepalive(upstream)
	}
	enableKeepalive(clientConn)

	// Tell the agent the tunnel is up. Per RFC 7231 §4.3.6 a 200 to
	// CONNECT signals "tunnel established"; the body is empty and any
	// subsequent bytes from either side are application data.
	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = clientConn.Close()
		if upstream != nil {
			_ = upstream.Close()
		}
		slog.Debug("forward-proxy: CONNECT 200 write failed", "host", pipeline.SanitizeLogValue(r.Host), "error", err)
		return
	}

	reason := pipeline.TunnelBridgeDisabled

	if s.TLSBridge != nil {
		pc := &peekedConn{Conn: clientConn, r: bufio.NewReaderSize(clientConn, sniffBufSize)}
		clientConn = pc // replay peeked bytes into whichever path runs
		// A server-first protocol (SSH, SMTP) sends nothing until it reads the
		// banner, so bound the peek: a short or empty first classifies as
		// not-TLS and the tunnel takes the opaque path. Shorter than
		// sniffHost's 5s because this delay is user-visible — the banner only
		// flows once this returns.
		_ = clientConn.SetReadDeadline(time.Now().Add(connectSniffTimeout))
		first, _ := pc.Peek(5)
		_ = clientConn.SetReadDeadline(time.Time{})
		authority := r.Host // CONNECT target is already host:port
		key := hostOnly(r.Host)
		var bridge bool
		reason, bridge = s.bridgeVerdict(r, tl, key, first)
		if bridge {
			s.noteBridgeAttempt()
			if upstream != nil {
				_ = upstream.Close() // bridgeServe dials its own verified upstream
			}
			if s.bridgeServe(clientConn, authority, key, tl, dialFail != nil) {
				return
			}
			// fell open → re-dial for the tunnel. That tunnel is opaque, so it
			// closes like any other opaque one. dialTarget, not r.Host: the address
			// was already checked once, and re-resolving here would reopen the
			// rebinding window the check closed. freshDialFail, when set, skips the
			// re-dial: the pre-dial already failed on a real dial of this target.
			s.tunnelOrDial(clientConn, dialTarget, tl, freshDialFail)
			return
		}
	}

	// Every path that reaches here left the bytes opaque without entering
	// bridgeServe, so this is where their reason gets recorded — except a
	// certainly-dead origin, which keeps the dial-failed reason the docs and
	// dashboards key on, whatever made the bridge decline.
	// The pre-dial failed (or was skipped as memoized-dead) and the bridge
	// declined the connection: dial now — the memo's optimism was for the bridge
	// path, and a plain tunnel still needs a real upstream. A fresh failure needs
	// no second dial; a memo hit is up to dialFailMemoTTL old, and the origin may
	// have come back.
	if upstream == nil {
		if freshDialFail != nil {
			tl.open(pipeline.TunnelDialFailed)
		} else {
			tl.open(reason)
		}
		s.tunnelOrDial(clientConn, dialTarget, tl, freshDialFail)
		return
	}
	tl.open(reason)

	// Bidirectional copy until either side closes.
	sent, received := tunnel(clientConn, upstream)
	tl.close(http.StatusOK, nil, sent, received)
}

// enableKeepalive turns on TCP keepalive with a 30s probe interval on
// the underlying *net.TCPConn, if conn unwraps to one. No-op on other
// connection types (notably *tls.Conn, which doesn't apply on the
// CONNECT path since the bytes through the tunnel are already TLS).
func enableKeepalive(conn net.Conn) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tcp.SetKeepAlive(true)
	_ = tcp.SetKeepAlivePeriod(30 * time.Second)
}

// hostOnly strips the port from an authority ("h:443" → "h"); returns input if no port.
func hostOnly(authority string) string {
	if h, _, err := net.SplitHostPort(authority); err == nil {
		return h
	}
	return authority
}

// portOf returns the port from an authority, defaulting to 443.
func portOf(authority string) int {
	if _, p, err := net.SplitHostPort(authority); err == nil {
		if n, err := strconv.Atoi(p); err == nil {
			return n
		}
	}
	return 443
}

// tunnelWarnThreshold is how many tunnels may open with nothing decrypted
// before the listener speaks up. A handful is normal — passthrough hosts, a
// non-HTTPS CONNECT, the first request racing startup — so warning on the
// first one would cry wolf. By this many, with zero bridged requests, the
// client is not trusting the CA.
const tunnelWarnThreshold = 5

// noteTunnel counts a CONNECT tunnel. It deliberately does NOT warn: a CONNECT
// says nothing about bridge health yet, because the destination may be in
// TLSBridge.Skip or classified as passthrough on purpose. Warning here counted
// intentional opaque tunnels as evidence of a broken CA — and because the
// warning is once-only, those false positives then masked the real failure when
// it happened later. The warning lives on the bridge-eligible path instead.
func (s *Server) noteTunnel() { s.tunnelsOpened.Add(1) }

// passthroughReason maps Decision.Classify's own reason string onto the stable
// wire vocabulary. Classify already distinguishes these cases; translating here
// rather than inventing a parallel set keeps one source of truth for WHY the
// bridge declined.
func passthroughReason(why string) pipeline.TunnelReason {
	switch why {
	case tlsbridge.ReasonPort:
		return pipeline.TunnelPassthroughPort
	case tlsbridge.ReasonNonTLS:
		return pipeline.TunnelPassthroughNonTLS
	case tlsbridge.ReasonSkip:
		return pipeline.TunnelPassthroughHost
	case "":
		// Classify pairs "" with Terminate — it is NOT declining. The caller bridges
		// and bridgeServe records that outcome, so there is no passthrough reason to
		// give and "" is right here.
		return ""
	}
	// Any OTHER value is a reason tlsbridge grew that this does not map. Never "":
	// that is how a BRIDGED row is marked, so an unmapped passthrough would render as
	// an em dash and read as "we decrypted this". Go cannot force an exhaustive
	// switch, so the fallback is a value that shows up as itself and sends the reader
	// here; TestPassthroughReasonCoversEveryClassifyReason fails when it happens.
	return pipeline.TunnelPassthroughUnknown
}

// markBridged records a successful bridge: the open row is deferred to the first
// decrypted request that records a row, and carries no reason, which is the signal
// agentop uses to fold the CONNECT row into that request. Named because
// `tl.deferOpen()` at the call site does not say why.
func markBridged(tl *tunnelLog) { tl.deferOpen() }

// clientAddr names the client end of a connection for diagnostics, tolerating a
// nil conn or nil RemoteAddr so a logging path can never panic.
func clientAddr(c net.Conn) string {
	if c == nil {
		return "unknown"
	}
	a := c.RemoteAddr()
	if a == nil {
		return "unknown"
	}
	return a.String()
}

// clientPort is the source port alone, for pasting into `lsof -nP -iTCP:<port>`.
// Returns "<port>" as a literal placeholder when it cannot be determined, so the
// suggested command still reads as a template rather than as something to run.
func clientPort(c net.Conn) string {
	if _, port, err := net.SplitHostPort(clientAddr(c)); err == nil && port != "" {
		return port
	}
	return "<port>"
}

// tunnelLog records one tunnel's rows in the session timeline: the open, with the
// reason the bytes stayed opaque, and the close, with what the tunnel ended as.
//
// Both rows are recorded at most once. The guard defends against a future exit rather
// than any current one — no path calls either twice today, which is exactly why only a
// direct test can hold it. It lives in the methods rather than in each caller, so a
// branch added later cannot be uncovered by forgetting to re-check a flag.
//
// skipped means the destination matched SkipHosts, where no plugin ran and there is
// nothing to attribute an event to: neither row is recorded.
type tunnelLog struct {
	s       *Server
	pctx    *pipeline.Context
	skipped bool
	// conn is the CONNECT's client process slot, handed to the requests a bridged tunnel
	// decrypts; nil off the CONNECT path.
	conn *connProc
	// connect is the CONNECT request itself, kept for AdoptBridgedRequest so a
	// listener that derived identity from the accepting connection can stamp it
	// onto each decrypted request too. nil off the CONNECT path.
	connect *http.Request
	// program is the client's program when the bridge rules named it, nil when they did
	// not. bridgeServe records a handshake's failure against it when set and against the
	// host when not, which is what the transparent listener and a nameless client get.
	program *tlsbridge.Program

	mu     sync.Mutex
	opened bool
	closed bool
	// bucket and reason are what the open row was recorded with. The close reuses both, so
	// it lands in the open's session and explains itself with the open's reason.
	bucket *session.Bucket
	reason pipeline.TunnelReason

	// deferred marks a bridged tunnel whose open row is not recorded yet. It waits for the
	// first decrypted request that records a row, so it can land in that row's session
	// directly before it; see recordWith. settleLocked records it on today's rule when no
	// request ever does.
	deferred bool
	// openEv is the deferred open row, built by deferOpen on the CONNECT's goroutine
	// before ServeConn. Whoever records it later runs in a handler, and on h2 a handler
	// can outlive ServeConn while handleConnect runs the CONNECT's finishers, which may
	// write pctx.Extensions; so no handler may build this row from pctx itself.
	openEv pipeline.SessionEvent
	// answered is set once a decrypted request has recorded a row. That row answers the
	// tunnel, and agentop folds the open into it, so no close is recorded.
	answered bool
	// inflight counts admitted handlers still running; done is set once ServeConn has
	// returned. The tunnel is settled only when both say nothing more can record.
	inflight int
	done     bool
}

func (s *Server) newTunnelLog(pctx *pipeline.Context, skipped bool) *tunnelLog {
	return &tunnelLog{s: s, pctx: pctx, skipped: skipped}
}

// open records the tunnel-open row. The first call wins; later ones are no-ops. It also
// cancels a deferred open, so a call after deferOpen cannot lead to a second open row.
func (t *tunnelLog) open(reason pipeline.TunnelReason) {
	if t.skipped {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.opened {
		return
	}
	t.opened, t.reason, t.deferred = true, reason, false
	t.bucket = t.s.recordTunnelOpened(t.pctx, reason)
}

// close records the tunnel's close row: the first call wins, and a close with no open
// records nothing, since it would be a response pairing with no request. See
// recordTunnelClosed for what each argument means.
func (t *tunnelLog) close(status int, fail *pipeline.EventError, up, down int64) {
	if t.skipped {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closeLocked(status, fail, up, down)
}

func (t *tunnelLog) closeLocked(status int, fail *pipeline.EventError, up, down int64) {
	if !t.opened || t.closed {
		return
	}
	t.closed = true
	t.s.recordTunnelClosed(t.pctx, t.bucket, t.reason, status, fail, up, down)
}

// admit counts a decrypted request the bridged tunnel is about to serve, and refuses it
// once the close has been recorded. On h2 that can happen: ServeConn returns without
// waiting for a request's handler to start, and by then the connection is closed and the
// request's context cancelled. Serving it would record a request beside a close that says
// the tunnel carried none. Every admit is matched by a release when the handler returns.
func (t *tunnelLog) admit() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.inflight++
	return true
}

// release ends an admitted handler, settling the tunnel if it was the last one running
// after ServeConn returned.
func (t *tunnelLog) release() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inflight--
	t.settleLocked()
}

// finish is called once ServeConn has returned. It settles the tunnel unless an admitted
// handler is still running; on h2 ServeConn does not wait for them, and the last release
// settles it instead.
func (t *tunnelLog) finish() {
	if t.skipped {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.done = true
	t.settleLocked()
}

// settleLocked records what a bridged tunnel still owes once nothing more can record
// under it: its open, if no request took it, and a close. A tunnel some request answered
// owes nothing.
//
// A settled tunnel's open is stamped and filed now — time.Now(), under today's session
// rule as it answers at this moment — since there is no request row to take either from.
func (t *tunnelLog) settleLocked() {
	if !t.done || t.inflight > 0 || t.answered {
		return
	}
	if t.deferred {
		t.deferred, t.opened = false, true
		open := t.openEv
		open.At = time.Now()
		t.bucket = t.s.appendTunnelOpen(t.pctx, open)
	}
	t.closeLocked(http.StatusOK, nil, 0, 0)
}

// deferOpen marks t as a bridged tunnel whose open row waits for recordWith. A no-op once
// the open is recorded: the transparent listener records its own before bridging.
//
// It builds that row now, on the CONNECT's goroutine and before ServeConn starts any
// handler; see openEv. Building it also fills pctx's RequestID memo, so a close row a
// handler later records through settleLocked reads pctx without writing it, and reads
// only fields set before bridging: identity, host, method, path, start time, client.
func (t *tunnelLog) deferOpen() {
	if t.skipped {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.opened {
		t.deferred = true
		t.openEv = sessionevent.TunnelOpen(t.pctx, "")
	}
}

// recordWith records ev under sid together with the tunnel's deferred open, and reports
// whether it did. The open goes in directly before ev, in one store call, because agentop
// folds a tunnel row only into the event that follows it. It takes ev's timestamp: a row
// stamped later than the one after it reads as out of order to agentop's pager. When
// the open is not deferred it records nothing and reports false, leaving ev to the
// caller.
//
// t.mu is held across the append so that a second request multiplexed onto the same
// tunnel cannot record between this check and the pair.
func (t *tunnelLog) recordWith(sid string, ev pipeline.SessionEvent, c *pipeline.RequestCapture) bool {
	if t.skipped || t.s.Sessions == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.answered = true
	if !t.deferred {
		return false
	}
	t.deferred, t.opened = false, true
	open := t.openEv
	open.At = ev.At
	t.bucket = t.s.Sessions.AppendPairCapture(sid, open, ev, c)
	return true
}

// dialError is a close row's error for a destination the proxy could not reach. The
// dial error's text names the address and the failure — refused, timed out, no such
// host — and nothing from the request.
func dialError(err error) *pipeline.EventError {
	return &pipeline.EventError{Kind: "dial_failed", Message: err.Error()}
}
