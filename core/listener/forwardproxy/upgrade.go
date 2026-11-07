package forwardproxy

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/rossoctl/cortex/core/attribution"
	"github.com/rossoctl/cortex/core/pipeline"
)

// declinedUpgradeBody is the 426's body. Static, like misdirectedBody: nothing the
// client sent is echoed back.
const declinedUpgradeBody = `{"detail":"WebSocket transport is not available through Cortex; use HTTPS"}` + "\n"

// wantsResponsesWebSocket reports whether r asks to open a WebSocket on an OpenAI
// Responses API path — the one shape listener.responses_websockets governs: carried
// and parsed (serveResponsesWebSocket), declined 426 (declineResponsesWebSocket), or
// forwarded and spliced like any other.
//
// Scoped twice: to OpenAI's own hosts (isOpenAIResponsesHost), and to the Responses
// suffix inference-parser reads (/v1/responses, and Codex's
// /backend-api/codex/responses). That is the protocol websocket.go can parse, and a
// 426 is an answer only Codex is known to read as "use HTTPS", and only against
// OpenAI is that known: every other WebSocket is forwarded (#1488), whatever its
// origin makes of the upgrade.
func wantsResponsesWebSocket(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		upgradesToWebSocket(r.Header) &&
		isOpenAIResponsesHost(r.URL.Host) &&
		strings.HasSuffix(r.URL.Path, "/responses")
}

// isOpenAIResponsesHost reports whether authority is one of OpenAI's own Responses API
// hosts: chatgpt.com, which serves Codex on a ChatGPT login, and api.openai.com, which
// serves it on an API key and is the public /v1/responses.
//
// OpenAI's only, because what a 426 means to a client is only known for Codex — it is
// read from Codex's own source. Another harness speaking the same API through another
// host (a LiteLLM gateway, OpenCode Zen, a local server) may treat a 426 as a hard
// failure where today it falls back, so those keep the strip and the origin's answer.
// Exact names, compared case-insensitively without a port or a trailing dot: a
// subdomain is not the same service.
//
// authority is r.URL.Host, the address the request is dialed to — on a bridged request
// the tunnel's own, which tunnelAuthorityMatches has already held the Host header to,
// and after a redirect the redirect's — so the judgement follows where the bytes go.
func isOpenAIResponsesHost(authority string) bool {
	switch strings.TrimSuffix(strings.ToLower(hostOnly(authority)), ".") {
	case "chatgpt.com", "api.openai.com":
		return true
	}
	return false
}

// upgradesToWebSocket reports whether h is a WebSocket upgrade REQUEST. RFC 9110
// §7.8 makes Upgrade a comma-separated list, matched case-insensitively and in any
// repeat of the header, so "websocket, h2c" asks for a WebSocket as surely as
// "websocket" does. A token may carry a version ("websocket/13"); only the name
// before it is compared.
//
// CONNECTION IS CHECKED FIRST, and it is not a formality. RFC 9110 §7.8 is
// explicit that a sender MUST send Connection: upgrade with the Upgrade header,
// and that a recipient ignores Upgrade when Connection does not list it — which is
// exactly what net/http's own httpguts-based upgradeType does before
// ReverseProxy honours an upgrade at all.
//
// Keying on Upgrade alone made this proxy SYNTHESIZE an upgrade the client had not
// requested: stripForUpstream sets the normalized Connection/Upgrade pair whenever
// keepUpgrade is true, so a request carrying a bare "Upgrade: websocket" and no
// Connection token reached the origin as a well-formed handshake, and an origin
// that answered 101 got its connection hijacked and spliced — turning a request
// the client expected an ordinary HTTP response to into an opaque byte pipe whose
// frames no plugin inspects. Measured end to end before this check existed
// (TestForwardProxy_UpgradeWithoutConnectionTokenIsStripped) (#1549 review).
func upgradesToWebSocket(h http.Header) bool {
	if !headerListContainsToken(h, "Connection", "upgrade") {
		return false
	}
	for _, line := range h.Values("Upgrade") {
		for _, tok := range strings.Split(line, ",") {
			name, _, _ := strings.Cut(strings.TrimSpace(tok), "/")
			if strings.EqualFold(name, "websocket") {
				return true
			}
		}
	}
	return false
}

// headerListContainsToken reports whether the named header — in any repeat, as a
// comma-separated list — carries token, compared case-insensitively. The shape
// RFC 9110 gives Connection, and the check net/http makes with
// httpguts.HeaderValuesContainsToken, which is not importable from here.
func headerListContainsToken(h http.Header, name, token string) bool {
	for _, line := range h.Values(name) {
		for _, tok := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), token) {
				return true
			}
		}
	}
	return false
}

// declineResponsesWebSocket answers a WebSocket upgrade on a Responses API path with
// 426 Upgrade Required, without contacting the origin.
//
// THE EXPENSIVE ANSWER IS THE ONE A STRIPPED HANDSHAKE DREW. Before upgrades were
// forwarded (#1488) the strip turned this handshake into a bare GET on an endpoint
// that takes a POST or an upgrade, and the origin answered 405. Codex reads a 405 as
// transient: it retries five times on a 200ms-doubling backoff, about six seconds,
// printing `Reconnecting… n/5`, before falling back to HTTPS. A 426 is the one status
// it reads as "this endpoint wants
// HTTP": it switches at once, for the rest of the session, with no retry and no warning
// (codex-rs/core/src/client.rs, the UPGRADE_REQUIRED arms). Measured on Codex 0.163.0,
// eight runs each of a one-word `codex exec` turn: 10.2–15.1s (median 11.0s) through the
// 405, 2.4–6.4s (median 3.0s) through this, with one handshake attempt instead of seven.
//
// TODAY THE DECLINE IS A STEER RATHER THAN A LIMIT: a forwarded Responses WebSocket
// works (relayUpgradedWebSocket), but its frames are opaque to every plugin, so a
// Codex session moved onto one stops being parseable — no token counts, no cost. The
// 426 keeps it on HTTPS, where its traffic stays observable.
//
// RFC 9110 gives 426 the opposite sense — the server requires the client to upgrade.
// OpenAI's backend uses it this way and Codex reads it this way, and a client that reads
// neither treats it as an ordinary failed handshake, which is what the 405 already was.
//
// The rows are an ordinary request and response, so the timeline still shows the
// attempt; the request row keeps its upgradeStripped marker, since the Upgrade was not
// forwarded. error.kind "upgrade_declined" says the 426 is ours, as misdirected_request
// does for the 421.
func (s *Server) declineResponsesWebSocket(w http.ResponseWriter, r *http.Request, pctx *pipeline.Context, tl *tunnelLog, sid string, chain []attribution.Proc) {
	slog.Debug("forward-proxy: declined a WebSocket upgrade on a Responses path; the client falls back to HTTPS",
		"host", pipeline.SanitizeLogValue(r.Host), "path", pipeline.SanitizeLogValue(r.URL.Path))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUpgradeRequired)
	_, _ = io.WriteString(w, declinedUpgradeBody)
	// A delivered answer, not a failed exchange: the deferred RunFinish reads zero as
	// OutcomeError, and nothing went wrong here.
	pctx.StatusCode = http.StatusUpgradeRequired

	if s.Sessions == nil {
		return
	}
	pctx.OutboundSessionID = sid
	if chain != nil {
		defer s.touch(sid, chain)
	}
	s.recordOutboundRequestEvent(tl, pctx, sid)
	s.recordOutboundResponseEvent(pctx, http.StatusUpgradeRequired, &pipeline.EventError{
		Kind:    pipeline.ErrorKindUpgradeDeclined,
		Message: "WebSocket upgrade answered 426 by the proxy; the client falls back to HTTPS",
	})
}

// relayUpgradedWebSocket splices a 101 Switching Protocols answer onto the
// client's connection and blocks until either side closes (#1488). It mirrors
// httputil.ReverseProxy's handleUpgradeResponse — hijack the client connection,
// write the origin's 101 through it, then copy both ways — with two things that
// proxy does not need and this one does: per-direction byte counts for the
// session row, and the row itself.
//
// resp.Body is the origin's connection once the origin answered 101 (the
// transport hands back an io.ReadWriteCloser or it never sent a 101), and this
// function owns it: serveOutbound returns without its resp.Body.Close() defer
// running, because closing the body here would close the WebSocket.
//
// THE ROWS: the request row was recorded before client.Do, as for any request.
// Its pair is recorded here, after the splice ends, carrying the 101 and both
// directions' byte counts — the one response row with bytesUp on it, like a
// tunnel close, because after the 101 the bytes are protocol frames the proxy
// does not parse and there is no body/header split to report. Duration covers
// the whole WebSocket session, not the handshake; ttfb is the handshake, set
// where client.Do returned. That is one audit event per upgrade, the request row
// plus its paired 101 (MOCA MI1 §8.5 item 5).
//
// Response plugins never see the 101 — pctx.ResponseHeaders is not set from it,
// so no RunResponse runs on this answer: the frames after a successful upgrade
// belong to the client and the origin ("streamed, never inspected or buffered",
// the same contract), and a response mutator would have nothing to act on. A
// non-101 answer never reaches here; it takes the ordinary response path.
//
// record=false is skip_hosts: the splice still happens — a skipped request is
// forwarded as a transparent proxy or not at all — but no row is appended, which
// is what skip_hosts promises.
func (s *Server) relayUpgradedWebSocket(w http.ResponseWriter, r *http.Request, pctx *pipeline.Context, resp *http.Response, record bool) {
	// The host as this function logs it: r.Host is client-controlled.
	host := pipeline.SanitizeLogValue(r.Host)
	// BOTH early returns below must close the origin themselves. This function
	// owns resp.Body (serveOutbound skipped its Close defer, see the doc above),
	// and the goroutine that closes it on context-done is only armed further
	// down — so returning before that point without closing here leaves the
	// origin holding a connection it believes is an established WebSocket, until
	// its own idle timeout reaps it. The client meanwhile has a 502, so nothing
	// will ever come back for it (#1549 review).
	// THE ORIGIN MUST BE SWITCHING TO THE PROTOCOL THE CLIENT ASKED FOR. A 101 is
	// not self-describing: it says "switching", and its own Upgrade header says to
	// what. net/http's ReverseProxy compares the two (handleUpgradeResponse refuses
	// when resUpType != reqUpType), and this function is modelled on it, but the
	// comparison was the one part left out — so an origin answering "101 Upgrade:
	// h2c" to a WebSocket handshake got spliced anyway, handing the client an
	// opaque pipe speaking a protocol it never negotiated and cannot parse. The
	// client is the one that would have to detect that, after the proxy already
	// committed its connection. Refuse before the hijack, while a 502 is still
	// writable (#1549 review).
	if !upgradesToWebSocket(resp.Header) {
		got := pipeline.SanitizeLogValue(resp.Header.Get("Upgrade"))
		slog.Error("forward-proxy: origin switched to another protocol than the WebSocket requested — not relaying",
			"host", host, "upgrade", got)
		_ = resp.Body.Close()
		s.failUpgradedWebSocket(w, pctx, record,
			fmt.Sprintf("origin switched to %q, not the websocket requested", got))
		return
	}
	backConn, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		// Not reachable through net/http's own transport, which gives a 101 a
		// writable body; a custom RoundTripper could violate it.
		slog.Error("forward-proxy: 101 switching protocols with a non-writable body — cannot relay the upgrade", "host", host)
		// Not an io.ReadWriteCloser, but still an io.Closer: the Body contract
		// guarantees that much, so the origin's end can be released.
		_ = resp.Body.Close()
		s.failUpgradedWebSocket(w, pctx, record, "101 switching protocols with a non-writable body")
		return
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		// The host is fenced above; the hijack error is the runtime's own text.
		slog.Error("forward-proxy: hijack failed relaying a WebSocket upgrade", "host", host, "error", err)
		_ = backConn.Close()
		s.failUpgradedWebSocket(w, pctx, record, fmt.Sprintf("hijack failed relaying the upgrade: %v", err))
		return
	}
	// The origin's end closes when the client's request context is done — which
	// covers a client hang-up the copies have not noticed yet (golang.org/issue
	// 35559, as in ReverseProxy) — or when the splice below ends, whichever is
	// first; the deferred close of conn takes care of the client's end.
	backConnCloseCh := make(chan struct{})
	go func() {
		select {
		case <-r.Context().Done():
		case <-backConnCloseCh:
		}
		backConn.Close()
	}()
	defer close(backConnCloseCh)
	defer conn.Close()

	// The origin's 101, as it wrote it, onto the hijacked connection. Moving
	// resp.Header into w's still-empty header map first is ReverseProxy's trick:
	// resp.Write then serializes status line and headers as one response, and
	// nil'ing the body stops it at the blank line.
	for k, vv := range resp.Header {
		w.Header()[k] = vv
	}
	resp.Header = w.Header()
	resp.Body = nil
	if err := resp.Write(brw); err != nil {
		slog.Error("forward-proxy: writing the 101 failed", "host", host, "error", err)
		s.recordSpliceFailure(pctx, record, fmt.Sprintf("writing the 101 failed: %v", err))
		return
	}
	if err := brw.Flush(); err != nil {
		slog.Error("forward-proxy: flushing the 101 failed", "host", host, "error", err)
		s.recordSpliceFailure(pctx, record, fmt.Sprintf("flushing the 101 failed: %v", err))
		return
	}

	// A delivered answer from here: the deferred RunFinish reads any non-zero
	// status as OutcomeAllow, and 101 is what the client got.
	pctx.StatusCode = http.StatusSwitchingProtocols

	// Both directions, each one io.Copy so its return count needs no second
	// counter. Up is client → origin, read from brw rather than conn so bytes
	// the hijack buffered are not lost; down is origin → client, written
	// straight to conn, which has no write buffer to strand. An EOF propagates
	// as CloseWrite where the connection has it (ReverseProxy's
	// switchProtocolCopier), so a half-closed WebSocket shuts down instead of
	// hanging on the open half.
	var up, down int64
	errc := make(chan error, 2)
	go func() {
		var err error
		if up, err = io.Copy(backConn, brw); err != nil {
			errc <- err
			return
		}
		if wc, ok := backConn.(interface{ CloseWrite() error }); ok {
			errc <- wc.CloseWrite()
			return
		}
		errc <- nil
	}()
	go func() {
		var err error
		if down, err = io.Copy(conn, backConn); err != nil {
			errc <- err
			return
		}
		if wc, ok := conn.(interface{ CloseWrite() error }); ok {
			errc <- wc.CloseWrite()
			return
		}
		errc <- nil
	}()
	// Both, not one: the counts are read below, and a goroutine's send on errc
	// is what publishes its write.
	//
	// But a FAILED direction has to tear both ends down before we wait for the
	// second, or that wait never returns. A clean EOF propagates as CloseWrite
	// above, which is what lets a half-closed WebSocket finish; an error has no
	// such signal, so the peer of the broken direction stays blocked in Read
	// with nothing coming. Measured: an origin that RSTs mid-session ends the
	// origin→client copy with an error while client→origin stays blocked on a
	// silent client, and the 101 row — recorded after both — is never written at
	// all. That is the one-event-per-upgrade promise lost on exactly the
	// connections worth auditing.
	//
	// ReverseProxy avoids this by returning after the FIRST result and letting
	// its deferred closes unblock the other; this function waits for both
	// because it needs their byte counts, so it closes explicitly instead. The
	// counts stay correct: io.Copy returns what it copied before the error, and
	// the closes only release a direction that is already finished or broken
	// (#1549 review).
	spliceErr := <-errc
	if spliceErr != nil {
		_ = conn.Close()
		_ = backConn.Close()
	}
	if err := <-errc; spliceErr == nil {
		spliceErr = err
	}

	if !record || s.Sessions == nil {
		return
	}
	// THE ROW SAYS WHETHER THE SESSION ENDED CLEANLY. Both directions have
	// finished either way and the status is still 101 — the upgrade did succeed,
	// and the client got its 101 — but a relay that ended in an error is not the
	// same event as one that closed cleanly, and without this the only symptom
	// was truncated byte counts: an operator auditing a WebSocket that broke
	// mid-session read a successful upgrade. One event per upgrade is the MOCA
	// contract; one event that cannot distinguish the two outcomes only half
	// keeps it.
	//
	// NOT EVERY ERROR HERE IS A FAILURE, and net.ErrClosed is the one that is not.
	// A WebSocket normally ends with one side closing; the deferred conn.Close()
	// and the context goroutine's backConn.Close() then race the direction still
	// blocked in Read, which returns "use of closed network connection". That is
	// the ordinary shutdown of a healthy session — marking it would flag EVERY
	// WebSocket as broken, which is worse than the gap this closes. Caught by
	// assertUpgradedRows' own "no error on a clean session" assertion, which is
	// why that one exists alongside the broken-direction test: a marker that is
	// always set distinguishes nothing.
	//
	// proxy_error otherwise, the kind the two pre-splice failure paths already
	// use, because a relay that broke is on our side. Sanitized: a network
	// error's text can carry a client-supplied address (#1549 review).
	var fail *pipeline.EventError
	if spliceErr != nil && !errors.Is(spliceErr, net.ErrClosed) {
		fail = &pipeline.EventError{
			Kind:    "proxy_error",
			Message: "websocket relay ended in error: " + pipeline.SanitizeControlRunes(spliceErr.Error()),
		}
	}
	ev := outboundResponseEvent(pctx, http.StatusSwitchingProtocols, fail)
	ev.BytesUp, ev.BytesDown = up, down
	s.sink().Append(attribution.FiledSession(pctx.OutboundSessionID, s.sink().ActiveSession), ev)
}

// recordSpliceFailure records the response row for a 101 whose relay broke after
// the hijack — the connection is the client's problem now, so there is no status
// to write it, but the request row still exists and a row with no pair reads as a
// request still in flight (#1045). 502 with proxy_error, as failUpgradedWebSocket:
// this failure is ours.
func (s *Server) recordSpliceFailure(pctx *pipeline.Context, record bool, cause string) {
	if !record {
		return
	}
	s.recordOutboundResponseEvent(pctx, http.StatusBadGateway, &pipeline.EventError{
		Kind:    "proxy_error",
		Message: cause,
	})
}

// failUpgradedWebSocket answers a 101 this proxy could not relay and records the
// failure row. The status stays off pctx (zero reads as OutcomeError to the
// deferred RunFinish) for the same reason the client.Do error path in
// serveOutbound keeps its 502 off it: nothing succeeded here.
func (s *Server) failUpgradedWebSocket(w http.ResponseWriter, pctx *pipeline.Context, record bool, cause string) {
	http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
	s.recordSpliceFailure(pctx, record, cause)
}
