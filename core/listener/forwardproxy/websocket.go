package forwardproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rossoctl/cortex/core/listener/httpx"
	"github.com/rossoctl/cortex/core/listener/internal/wsframe"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// A Responses API WebSocket, carried and parsed.
//
// Codex's preferred transport is a WebSocket to its /responses endpoint. One socket
// lives for a whole session and carries many turns: the client sends a text message
// {"type":"response.create", ...the POST body's fields...} and the server answers with
// one text message per Responses stream event — the same JSON the HTTPS path streams as
// SSE `data:` — ending in response.completed. Later turns send only new input, with
// previous_response_id naming the turn before.
//
// SO EACH TURN IS RUN AS IF IT WERE THE HTTPS REQUEST. The handshake GET runs the
// pipeline once, as every bridged request does, and records a tunnel-style open. Every
// response.create then gets a pipeline.Context of its own, cloned from the handshake's
// identity, with Method POST and Body the message minus "type": inference-parser
// parses it, the turn's server messages are fed to it frame by frame exactly as
// handleStreamingResponse feeds SSE events, and the turn ends — settled, recorded,
// finished — on response.completed, response.failed, an error event, or the socket
// closing. Per-turn rows are an ordinary request/response pair, so cost, usage, the
// session timeline and agentop read them like any other inference.
//
// RELAYED, NEVER RE-ENCODED, with two exceptions. Every frame is forwarded as the bytes
// it arrived as (wsframe.Frame.Raw). A plugin that rewrites a turn's request body
// (tool-prune) is honoured by sending the rewritten message as one new client frame in
// place of the original; and a turn a plugin rejects is answered to the client with an
// error event in the shape Codex already reads for a server refusal, and never sent.
//
// permessage-deflate is stripped from the handshake, so neither end compresses and the
// relay reads plain frames. Codex offers it and does not require it.

const (
	// wsClientMessageLimit and wsServerMessageLimit are the largest messages read for
	// parsing, each direction matching what the HTTPS path parses: maxRequestBodySize
	// for what the client sends — a socket's first turn carries the whole
	// conversation, the same body an HTTPS POST would — and maxBodySize for each event
	// the origin sends back, the SSE path's per-frame cap. A larger message ends the
	// socket with close code 1009, as the HTTPS path refuses an oversized body with a
	// 413. From the client that happens before any of it is forwarded, since relaying
	// it unread would carry a turn no plugin saw and no row records. From the origin
	// the frames already sent stay sent: relayDown forwards each frame before reading
	// it, so a too-big answer reaches the client up to the frame that crossed the limit.
	wsClientMessageLimit = maxRequestBodySize
	wsServerMessageLimit = maxBodySize
	// wsFrameLimit is the largest single frame relayed. Raw is buffered whole before
	// it is forwarded, so this bounds what one frame can cost; a frame over it ends the
	// socket. Matches the request-body cap, the largest thing the HTTPS path accepts.
	wsFrameLimit = maxRequestBodySize
	// wsHandshakeTimeout bounds the upstream handshake. Codex's own connect timeout is
	// 15s; a stalled origin should not hold the client's handshake longer than that.
	wsHandshakeTimeout = 15 * time.Second
)

// wantsResponsesWebSocket reports whether r asks to open a WebSocket on OpenAI's
// Responses API — the one shape listener.responses_websockets governs.
//
// Scoped twice: to OpenAI's own hosts (isOpenAIResponsesHost), and to the Responses
// suffix inference-parser reads (/v1/responses, and Codex's
// /backend-api/codex/responses). That is the protocol this file can parse, and the one
// whose client — Codex — is known to read a 426 as "use HTTPS": every other WebSocket
// keeps the strip and whatever its origin makes of the bare GET.
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
// read from Codex's own source — and the socket this file carries is Codex's. Another
// harness speaking the same API through another host (a LiteLLM gateway, OpenCode Zen,
// a local server) may treat a 426 as a hard failure where today it falls back, so those
// keep the strip and the origin's answer. Exact names, compared case-insensitively
// without a port or a trailing dot: a subdomain is not the same service.
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

// upgradesToWebSocket reports whether h's Upgrade names websocket. RFC 9110 §7.8 makes
// Upgrade a comma-separated list, matched case-insensitively and in any repeat of the
// header, so "websocket, h2c" asks for a WebSocket as surely as "websocket" does. A
// token may carry a version ("websocket/13"); only the name before it is compared.
func upgradesToWebSocket(h http.Header) bool {
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

// cannotCarryWebSocket says why this socket cannot be carried and parsed, or "" when it
// can. Each reason is a shape the relay below does not handle; the caller declines
// those with a 426 rather than forwarding them, so a parse-mode client is never left
// sitting through the origin's 405.
//
// An HTTP/2 client never gets here: x/net/http2 refuses any request carrying Upgrade
// with a 400 before the handler runs, so that is what an h2 client sees.
func (s *Server) cannotCarryWebSocket(tl *tunnelLog, p *pipeline.Pipeline) string {
	if tl == nil || s.TLSBridge == nil {
		// A plaintext proxied request: no TLS bridge to re-originate over, and no
		// Responses endpoint is served over plain HTTP.
		return "not a bridged request"
	}
	return pipelineCannotCarry(p)
}

// pipelineCannotCarry says why p cannot run a carried socket's turns, or "" when it
// can. Checked at the handshake and again at every turn, since a hot reload can change
// the answer under a socket that is already open.
func pipelineCannotCarry(p *pipeline.Pipeline) string {
	switch {
	case p.WritesResponseBody():
		// A response-body writer rewrites a whole response; a turn's response is a
		// live stream of frames already on their way to the client. The SSE path
		// buffers instead, and a WebSocket has no buffered form.
		return "a plugin rewrites response bodies"
	case p.WritesDestination():
		// A destination writer (inference-router) sends a request to another server.
		// A turn rides a socket already connected to OpenAI, so it cannot be sent
		// elsewhere; over HTTPS each turn is its own request, which can.
		return "a plugin redirects requests"
	}
	return ""
}

// serveResponsesWebSocket completes the upgrade with the origin, takes over the client's
// connection, and relays the socket until either end closes it, parsing every turn.
//
// pctx is the handshake's, after the pipeline ran on it. Its rows are the socket's open
// (now) and close (when it ends), sharing its RequestID the way an opaque tunnel's do;
// the turns record their own pairs between them.
func (s *Server) serveResponsesWebSocket(w http.ResponseWriter, r *http.Request, pctx *pipeline.Context, tl *tunnelLog, sid string, chain []session.Proc) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		s.declineResponsesWebSocket(w, r, pctx, tl, sid, chain)
		return
	}

	// The handshake as the origin should see it: the client's headers after the
	// pipeline, minus what is the proxy's own — and minus the extension offer, so
	// neither end compresses frames the relay has to read.
	up := r.Clone(r.Context())
	syncHeaders(up, pctx.Headers)
	for _, h := range []string{"Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection", "TE", "Trailer", "Transfer-Encoding", "Sec-Websocket-Extensions"} {
		up.Header.Del(h)
	}
	up.Header.Set("Connection", "Upgrade")
	up.Header.Set("Upgrade", "websocket")
	up.RequestURI = ""
	up.Body = nil
	up.ContentLength = 0
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), wsHandshakeTimeout)
	up = up.WithContext(ctx)

	client := s.TLSBridge.Upstream
	resp, err := client.Do(up)
	if err == nil {
		// The origin answered the upgrade, so it was forwarded, whatever it answered:
		// the request row must not say otherwise. A dial that failed forwarded nothing,
		// and keeps the marker.
		pctx.UpgradeStripped = ""
	}
	if err != nil {
		cancel()
		status, fail := pipeline.ExchangeFailure(r.Context(), err)
		slog.Warn("forward-proxy: WebSocket handshake with the origin failed", "host", r.Host, "kind", fail.Kind, "error", fail.Message)
		http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
		s.recordWebSocketRefusal(pctx, tl, sid, chain, status, fail)
		return
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		// The origin refused the upgrade (401, 403, 426…): relay its answer as an
		// ordinary response, so the client sees what it would have without Cortex.
		// The handshake's deadline still covers the body: cancelled before the copy, it
		// would cut the answer off part way.
		defer cancel()
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		n, _ := io.Copy(w, io.LimitReader(resp.Body, maxBodySize))
		pctx.ResponseBytes = n
		pctx.StatusCode = resp.StatusCode
		s.recordWebSocketRefusal(pctx, tl, sid, chain, resp.StatusCode, nil)
		return
	}
	// A 101: the deadline was for the handshake, and the socket outlives it.
	cancel()
	upConn, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		_ = resp.Body.Close()
		http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
		s.recordWebSocketRefusal(pctx, tl, sid, chain, http.StatusBadGateway, &pipeline.EventError{Kind: "upstream_error", Message: "the origin's 101 carried no writable connection"})
		return
	}
	if ext := resp.Header.Get("Sec-Websocket-Extensions"); ext != "" {
		// It accepted an extension nobody offered. Frames would arrive compressed and
		// unreadable, so end the socket now rather than relay what cannot be parsed.
		_ = upConn.Close()
		http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
		s.recordWebSocketRefusal(pctx, tl, sid, chain, http.StatusBadGateway, &pipeline.EventError{Kind: "upstream_error", Message: "the origin negotiated a WebSocket extension that was not offered"})
		return
	}

	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		_ = upConn.Close()
		slog.Warn("forward-proxy: could not take over the client connection for a WebSocket", "host", r.Host, "error", err)
		return
	}
	// The bridge's handshake deadline was cleared before serving; nothing sets one
	// again, so the socket lives as long as both ends keep it.
	_ = clientConn.SetDeadline(time.Time{})

	// The 101 as the origin sent it, minus the hop-by-hop headers the proxy owns.
	var hdr bytes.Buffer
	fmt.Fprintf(&hdr, "HTTP/1.1 101 Switching Protocols\r\n")
	for k, vs := range resp.Header {
		switch http.CanonicalHeaderKey(k) {
		case "Keep-Alive", "Proxy-Connection", "Transfer-Encoding", "Content-Length":
			continue
		}
		for _, v := range vs {
			fmt.Fprintf(&hdr, "%s: %s\r\n", k, v)
		}
	}
	hdr.WriteString("\r\n")
	if _, err := clientConn.Write(hdr.Bytes()); err != nil {
		_ = clientConn.Close()
		_ = upConn.Close()
		return
	}

	pctx.StatusCode = http.StatusSwitchingProtocols
	ctx, stop := context.WithCancel(context.WithoutCancel(r.Context()))
	defer stop()
	sock := &wsSocket{
		s: s, r: r, hs: pctx, tl: tl, sid: sid, chain: chain,
		client: clientConn, clientR: clientBuf.Reader, up: upConn,
		start: time.Now(), ctxv: ctx, stop: stop,
	}
	s.recordWebSocketOpen(sock)
	sock.run()
}

// recordWebSocketRefusal records the handshake's request row and the answer it got, for
// a socket that never opened. The same pair an ordinary request records.
func (s *Server) recordWebSocketRefusal(pctx *pipeline.Context, tl *tunnelLog, sid string, chain []session.Proc, status int, fail *pipeline.EventError) {
	if s.Sessions == nil {
		return
	}
	pctx.OutboundSessionID = sid
	if chain != nil {
		defer s.Sessions.TouchProcess(sid, chain)
	}
	s.recordOutboundRequestEvent(tl, pctx, sid)
	s.recordOutboundResponseEvent(pctx, status, fail)
}

// recordWebSocketOpen records the socket's open row: the handshake GET, marked as a
// tunnel whose reason is the WebSocket, so agentop pairs it with the close the way it
// pairs any tunnel's.
func (s *Server) recordWebSocketOpen(ws *wsSocket) {
	if s.Sessions == nil {
		return
	}
	ws.hs.OutboundSessionID = ws.sid
	ev := s.tunnelOpenEvent(ws.hs, pipeline.TunnelWebSocket)
	ev.HTTPMethod, ev.HTTPPath = ws.hs.Method, ws.hs.Path
	s.appendOutbound(ws.tl, ws.sid, ev)
}

// wsSocket is one carried Responses WebSocket.
type wsSocket struct {
	s     *Server
	r     *http.Request
	hs    *pipeline.Context // the handshake's, for the socket's own rows
	tl    *tunnelLog
	sid   string
	chain []session.Proc

	client  net.Conn
	clientR *bufio.Reader // the hijack's buffer: bytes the server read past the request
	up      io.ReadWriteCloser
	start   time.Time

	bytesUp, bytesDown atomic.Int64

	// clientMu serialises writes to the client: the downstream relay and a rejected
	// turn's error event both write there.
	clientMu sync.Mutex
	// upMu does the same for the origin: the upstream relay, and a rewritten turn
	// written in place of the client's frame.
	upMu sync.Mutex

	// turnMu guards turns, which both relays touch. The client side appends a turn
	// when it sends a response.create; the server side alone ends one, on that turn's
	// terminal event. A turn is ended where its last event is parsed, never from the
	// client side, so a response.create that arrives while relayDown is still parsing
	// the previous turn's response.completed cannot end that turn early, and an answer
	// cannot be fed to a turn it does not belong to: the server answers turns in the
	// order they were sent, which is the order of this queue.
	turnMu sync.Mutex
	turns  []*wsTurn

	// ctxv is the context every turn's plugins run under: the handshake's values
	// without its cancellation, cancelled by stop when the socket ends, so a plugin
	// waiting on something (a budget pause, a slow script) is released with it.
	ctxv context.Context
	stop context.CancelFunc

	// endedBy is the side whose relay stopped first, "client" or "origin", which
	// decides how a turn still open at that moment is recorded. Set once, by the
	// first relay to return; the other only ever sees the connection closed under it.
	endedBy atomic.Value

	// closedBy is the side that sent the first close frame, and closeNote its code and
	// reason. A close frame is the sender saying it is ending the socket, which is
	// better evidence than which relay happened to stop first: when one side closes
	// cleanly the other answers with a close of its own, and that answer can make its
	// relay stop first. Set once, by the first close frame either relay sees.
	closedBy  atomic.Value
	closeNote atomic.Value
}

// sawClose records a close frame from side, if none has been seen yet.
func (ws *wsSocket) sawClose(side string, f *wsframe.Frame) {
	if !ws.closedBy.CompareAndSwap(nil, side) {
		return
	}
	ws.closeNote.Store(wsframe.DescribeClose(f.Payload))
}

// wsEnd records who ended the socket, if no one has yet.
func (ws *wsSocket) wsEnd(side string) {
	ws.endedBy.CompareAndSwap(nil, side)
}

// wsTurn is one response.create and the server messages that answer it.
type wsTurn struct {
	pctx *pipeline.Context
	// p is the pipeline the turn began on, and every later step of the turn runs on
	// it. A hot reload swaps OutboundPipeline for new requests and keeps the old one
	// running for its drain window so requests already in flight finish where they
	// started; an HTTPS turn does that by lasting seconds. A socket outlives the
	// window, so a turn looking the pipeline up again at each step would end on one
	// whose parser never saw it begin — no tokens, no cost.
	p *pipeline.Pipeline
	// prewarm is a generate:false response.create: run through the request phase, not
	// recorded, and its answer not parsed.
	prewarm bool
}

func (ws *wsSocket) run() {
	done := make(chan struct{}, 2)
	go func() { ws.wsEnd(ws.relayUp()); done <- struct{}{} }()
	go func() { ws.wsEnd(ws.relayDown()); done <- struct{}{} }()
	<-done
	// One side is gone: close both, so the other relay's read returns.
	_ = ws.client.Close()
	_ = ws.up.Close()
	<-done

	// Both relays have returned, so nothing else touches the queue: every turn still
	// in it was sent and never answered in full. A prewarm in it is left alone: it was
	// finished when it was sent and has no request row, so ending it here would run
	// its Finishers twice and record a response row with nothing to pair with.
	ws.turnMu.Lock()
	open := ws.turns
	ws.turns = nil
	ws.turnMu.Unlock()
	status, fail := ws.unfinishedTurn()
	for _, t := range open {
		if !t.prewarm {
			ws.endTurn(t, status, fail)
		}
	}
	ws.recordClose()
	// Released last: endTurn's teardown contexts derive from it.
	ws.stop()
}

// unfinishedTurn is how a turn still open when the socket ended is recorded, by who
// ended it — the WebSocket counterpart of what the HTTPS path records for a stream that
// stops before its last event.
//
// THE CLIENT CLOSING IS NOT A FAILURE. It is what Codex does on Esc: it drops the turn's
// stream, which closes this socket, and opens another for the next prompt. Over HTTPS
// the same Esc ends a stream the origin had already answered 200, and that is how it is
// recorded — the upstream's 200 and no error — so an interrupted turn counts in
// /v1/usage as priceable-but-unpriced rather than as an error. A turn ended here is
// recorded the same way: 200, with the reason in Message only, under a kind that
// countsAsError leaves alone. The origin closing first, or the socket breaking on a
// frame the relay cannot read, is what upstream_error is for.
//
// A close frame decides first, when either side sent one: whoever sent the first close
// ended the socket, and its code and reason go into the message. Without one — a
// connection dropped, which is what Codex's Esc does — the relay that stopped first
// decides. Either way the statuses are the same two; a close code is evidence about
// who, recorded beside the status, not a status of its own.
func (ws *wsSocket) unfinishedTurn() (int, *pipeline.EventError) {
	side, _ := ws.closedBy.Load().(string)
	note, _ := ws.closeNote.Load().(string)
	if side == "" {
		side, _ = ws.endedBy.Load().(string)
	}
	suffix := ""
	if note != "" {
		suffix = " (" + note + ")"
	}
	if side == "client" {
		return http.StatusOK, &pipeline.EventError{
			Kind:    pipeline.ErrorKindTurnInterrupted,
			Message: "the client closed the WebSocket before the turn completed" + suffix,
		}
	}
	return http.StatusBadGateway, &pipeline.EventError{
		Kind:    "upstream_error",
		Message: "the origin closed the WebSocket before the turn completed" + suffix,
	}
}

// errorEventStatus is the HTTP status recorded for a turn the origin refused with an
// error event.
//
// AN OPEN WEBSOCKET HAS NO HTTP STATUS: the handshake's 101 was the last one. So the
// origin reports a refused turn inside a message, and the status field is optional —
// Codex's own fixtures carry "status": 429 on a usage limit, while a live quota refusal
// arrived with none. The row records the status the same refusal answers over HTTPS,
// so a WebSocket turn and an HTTPS one count alike in /v1/usage: the event's own status
// when it has one; else the one its error code is answered with over HTTPS; else 500.
// Codes are the Responses API's, as Codex's source maps them (codex-api's api_bridge).
func errorEventStatus(status int, code, errType string) int {
	if status >= 400 && status <= 599 {
		return status
	}
	for _, name := range []string{code, errType} {
		switch name {
		case "rate_limit_exceeded", "insufficient_quota", "usage_limit_reached",
			"usage_not_included", "websocket_connection_limit_reached", "slow_down":
			return http.StatusTooManyRequests
		case "context_length_exceeded", "invalid_prompt", "invalid_request_error",
			"previous_response_not_found":
			return http.StatusBadRequest
		case "server_is_overloaded":
			return http.StatusServiceUnavailable
		}
	}
	return http.StatusInternalServerError
}

// firstNonEmpty is the first of vs that is not empty, or "".
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// capErrorName bounds an origin-supplied error code before it reaches a row an
// operator reads in a terminal: printable ASCII only, 64 bytes at most.
func capErrorName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= 64 {
			break
		}
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// relayUp carries client frames to the origin, reading each data message on the way.
// It returns the side that stopped it: the client when reading from it failed, the
// origin when writing to it did.
func (ws *wsSocket) relayUp() (endedBy string) {
	fr := wsframe.NewReader(ws.clientR, wsClientMessageLimit, wsFrameLimit)
	msg := wsframe.NewMessage(wsClientMessageLimit)
	// held collects a data message's frames until it is whole: a turn a plugin
	// rejects or rewrites must not have reached the origin first.
	var held [][]byte
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			ws.logEnd("client", err)
			return "client"
		}
		ws.bytesUp.Add(int64(len(f.Raw)))
		if f.IsControl() {
			if f.Opcode == wsframe.OpClose {
				ws.sawClose("client", f)
			}
			if !ws.writeUp(f.Raw) {
				return "origin"
			}
			continue
		}
		op, payload, readable, complete, err := msg.Add(f)
		if err != nil {
			if errors.Is(err, wsframe.ErrMessageTooBig) {
				// Neither relayed unread nor held any longer: see wsClientMessageLimit.
				ws.closeBoth(wsframe.CloseMessageTooBig, "message too big for the proxy to read")
			}
			ws.logEnd("client", err)
			return "client"
		}
		// Bounded by Message: it ends the message before its frames pass the limit.
		held = append(held, f.Raw)
		if !complete {
			continue
		}
		out := held
		held = nil
		if op == wsframe.OpText && readable {
			replace, refuse := ws.clientMessage(payload)
			if refuse {
				continue
			}
			if replace != nil {
				out = [][]byte{wsframe.Encode(wsframe.OpText, replace, true)}
			}
		}
		for _, raw := range out {
			if !ws.writeUp(raw) {
				return "origin"
			}
		}
	}
}

// relayDown carries origin frames to the client, feeding each data message to the turn
// it belongs to. It returns the side that stopped it: the origin when reading from it
// failed, the client when writing to it did.
func (ws *wsSocket) relayDown() (endedBy string) {
	fr := wsframe.NewReader(ws.up, wsServerMessageLimit, wsFrameLimit)
	msg := wsframe.NewMessage(wsServerMessageLimit)
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			ws.logEnd("origin", err)
			return "origin"
		}
		ws.bytesDown.Add(int64(len(f.Raw)))
		// Forwarded first, as handleStreamingResponse writes a block after its
		// plugins have seen it only so a Reject can keep it off the wire. A
		// WebSocket turn cannot be retracted once it started, so the frame goes
		// now and the parse follows.
		if !ws.writeClient(f.Raw) {
			return "client"
		}
		if f.IsControl() {
			if f.Opcode == wsframe.OpClose {
				ws.sawClose("origin", f)
			}
			continue
		}
		op, payload, readable, complete, err := msg.Add(f)
		if err != nil {
			if errors.Is(err, wsframe.ErrMessageTooBig) {
				ws.closeBoth(wsframe.CloseMessageTooBig, "message too big for the proxy to read")
			}
			ws.logEnd("origin", err)
			return "origin"
		}
		if complete && op == wsframe.OpText && readable {
			ws.serverMessage(payload)
		}
	}
}

// closeBoth sends each end a close frame with code and reason before the socket is torn
// down, so both see why rather than a dropped connection. Best effort: a write that fails
// is the connection already gone.
func (ws *wsSocket) closeBoth(code uint16, reason string) {
	payload := wsframe.ClosePayload(code, reason)
	ws.writeClient(wsframe.Encode(wsframe.OpClose, payload, false))
	ws.writeUp(wsframe.Encode(wsframe.OpClose, payload, true))
}

func (ws *wsSocket) writeUp(b []byte) bool {
	ws.upMu.Lock()
	defer ws.upMu.Unlock()
	_, err := ws.up.Write(b)
	return err == nil
}

func (ws *wsSocket) writeClient(b []byte) bool {
	ws.clientMu.Lock()
	defer ws.clientMu.Unlock()
	_, err := ws.client.Write(b)
	return err == nil
}

func (ws *wsSocket) logEnd(side string, err error) {
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return
	}
	if errors.Is(err, wsframe.ErrProtocol) {
		slog.Warn("forward-proxy: ending a WebSocket on a frame the relay cannot read", "host", ws.r.Host, "side", side, "error", err)
		return
	}
	slog.Debug("forward-proxy: WebSocket ended", "host", ws.r.Host, "side", side, "error", err)
}

// wsEnvelope is the one field every message on the socket carries.
type wsEnvelope struct {
	Type string `json:"type"`
}

// clientMessage handles one complete text message from the client. It returns a
// replacement to send instead of the original when a plugin rewrote the turn, and
// refuse when a plugin rejected it — in which case the client has already been told.
func (ws *wsSocket) clientMessage(payload []byte) (replace []byte, refuse bool) {
	var env wsEnvelope
	if json.Unmarshal(payload, &env) != nil || env.Type != "response.create" {
		// response.interrupt, and anything else, is protocol between the two ends.
		return nil, false
	}
	body, generate := responseCreateBody(payload)

	p := ws.s.OutboundPipeline.Load()
	if why := pipelineCannotCarry(p); why != "" {
		// A reload since the handshake made this socket one the proxy would have
		// declined. End it with "try again later": Codex reconnects, and the new
		// handshake is answered 426 and falls back to HTTPS, where the plugin applies.
		slog.Info("forward-proxy: ending a carried WebSocket after a reload", "host", ws.r.Host, "reason", why)
		ws.closeBoth(wsframe.CloseTryAgainLater, "the proxy's pipeline changed; reconnect")
		return nil, true
	}

	pctx := ws.newTurnContext(body)
	t := &wsTurn{pctx: pctx, p: p, prewarm: !generate}
	action := p.Run(ws.ctx(), pctx)
	if action.Type == pipeline.Reject {
		ws.s.recordOutboundRejectIn(nil, pctx, action, ws.sid)
		ws.refuseTurn(action)
		p.RunFinish(ws.ctx(), pctx, pipeline.OutcomeFromContext(pctx))
		return nil, true
	}
	if t.prewarm {
		// generate:false asks the server to set up state and answer at once, with no
		// model output. It carries the same prompt a real turn does — the turn after it
		// sends only what is new — so its request phase runs, and a plugin can refuse
		// or rewrite it. But it is not inference: no row, and its answer is not a
		// turn's response.
		p.RunFinish(ws.ctx(), pctx, pipeline.OutcomeFromContext(pctx))
	} else if ws.s.Sessions != nil {
		pctx.OutboundSessionID = ws.sid
		ws.s.Sessions.Append(ws.sid, ws.turnRequestEvent(pctx))
	}
	ws.turnMu.Lock()
	ws.turns = append(ws.turns, t)
	ws.turnMu.Unlock()

	if pctx.BodyMutated() {
		return withType(pctx.Body, !generate), false
	}
	return nil, false
}

// serverMessage feeds one complete text message from the origin to the open turn, and
// ends the turn on a terminal event.
func (ws *wsSocket) serverMessage(payload []byte) {
	ws.turnMu.Lock()
	var t *wsTurn
	if len(ws.turns) > 0 {
		t = ws.turns[0]
	}
	ws.turnMu.Unlock()
	if t == nil {
		// Nothing is waiting for an answer. Nothing to parse into.
		return
	}
	var env struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
		Error  struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(payload, &env)
	if t.prewarm {
		// A prewarm's answer is set-up, not output. It ends on the same events a turn
		// does, and is not recorded.
		switch env.Type {
		case "error", "response.completed", "response.incomplete", "response.failed":
			ws.popTurn(t)
		}
		return
	}
	if env.Type == "error" {
		// The origin refused the turn (a usage limit, bad input). See errorEventStatus.
		status := errorEventStatus(env.Status, env.Error.Code, env.Error.Type)
		message := "the origin answered the turn with an error event"
		if name := firstNonEmpty(env.Error.Code, env.Error.Type); name != "" {
			message += ": " + capErrorName(name)
		}
		// The origin answered, so the status is its answer, not a failure of the proxy:
		// the row carries the error, and Finishers see what HTTPS would show them for
		// the same refusal — an allow with this status, not an error with none.
		t.pctx.StatusCode = status
		ws.finishTurn(t, status, &pipeline.EventError{Kind: "backend_error", Code: strconv.Itoa(status), Message: message})
		return
	}
	// Delivered before it is read: relayDown forwarded this frame to the client already,
	// and a WebSocket answer cannot be retracted. Marked here, not only at the turn's
	// end, so a plugin rejecting a mid-turn frame records a late refusal rather than
	// turning a turn the client received into a denial.
	t.pctx.MarkResponseDelivered()
	t.p.RunResponseFrame(ws.ctx(), t.pctx, payload, false)
	t.pctx.ResponseBytes += int64(len(payload))
	// The three events inference-parser reads usage and the finish reason from; each
	// ends a response, as response.completed ends Codex's own read loop.
	switch env.Type {
	case "response.completed", "response.incomplete", "response.failed":
		ws.finishTurn(t, http.StatusOK, nil)
	}
}

// finishTurn ends t, the turn at the head of the queue. Only relayDown calls it, so no
// other path can end t while its last event is still being parsed.
func (ws *wsSocket) finishTurn(t *wsTurn, status int, fail *pipeline.EventError) {
	if ws.popTurn(t) {
		ws.endTurn(t, status, fail)
	}
}

// popTurn removes t from the head of the queue, reporting whether it was there.
func (ws *wsSocket) popTurn(t *wsTurn) bool {
	ws.turnMu.Lock()
	defer ws.turnMu.Unlock()
	if len(ws.turns) == 0 || ws.turns[0] != t {
		return false
	}
	ws.turns[0] = nil
	ws.turns = ws.turns[1:]
	return true
}

// endTurn finalizes a turn: the terminal frame that settles its cost, its response
// row, and the Finisher dispatch — the three things handleStreamingResponse's deferred
// block does at the end of an SSE stream, in the same order.
func (ws *wsSocket) endTurn(t *wsTurn, status int, fail *pipeline.EventError) {
	finalCtx, cancelFinal := httpx.TeardownContext(ws.ctx())
	defer cancelFinal()
	t.pctx.MarkResponseDelivered()
	if fail == nil || fail.Kind == pipeline.ErrorKindTurnInterrupted {
		t.pctx.StatusCode = status
	}
	t.p.RunResponseFrame(finalCtx, t.pctx, nil, true)
	ws.s.recordOutboundResponseEvent(t.pctx, status, fail)
	t.p.RunFinish(finalCtx, t.pctx, pipeline.OutcomeFromContext(t.pctx))
}

// ctx is the context every turn's pipeline runs under: the handshake request's values
// (the client's process, for attribution) without its cancellation — the handshake
// request ended when the upgrade was answered, and the socket outlives it — cancelled
// instead when the socket ends.
func (ws *wsSocket) ctx() context.Context { return ws.ctxv }

// newTurnContext builds a turn's pipeline.Context: the handshake's identity — host,
// path, headers, client, session — with the request the turn would have been over
// HTTPS, a POST carrying the message's fields as its body.
func (ws *wsSocket) newTurnContext(body []byte) *pipeline.Context {
	hs := ws.hs
	headers := ws.r.Header.Clone()
	// The handshake's own framing headers, and what it said about a body it does not
	// have: a turn's body is a WebSocket text message, never encoded, and the
	// handshake's Content-Encoding and Content-Length describe nothing the turn carries.
	// A plugin reading them would be told something false about the body it is shown.
	for _, h := range []string{"Upgrade", "Connection", "Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol", "Content-Encoding", "Content-Length"} {
		headers.Del(h)
	}
	headers.Set("Content-Type", "application/json")
	// The session as it is when this turn starts, built per turn as HTTPS builds one per
	// request: the handshake's view was taken before any turn was recorded, so a plugin
	// reading the session (an intent, a request count) would see none of the turns before
	// this one. From the handshake's resolved session, as hydrateSession resolves one for
	// an HTTPS request, so a turn has no view exactly when that request would have none.
	var view *pipeline.SessionView
	if hs.Session != nil {
		view = ws.s.sessionViewFor(hs.Session.ID)
	}
	pctx := &pipeline.Context{
		Direction:     pipeline.Outbound,
		Method:        http.MethodPost,
		Scheme:        hs.Scheme,
		Host:          hs.Host,
		Path:          hs.Path,
		Headers:       headers,
		Body:          body,
		Shared:        ws.s.Shared,
		StartedAt:     time.Now(),
		Session:       view,
		SessionSource: hs.SessionSource,
		SessionHeader: hs.SessionHeader,
		ChainSession:  hs.ChainSession,
	}
	pctx.ResolveClient()
	return pctx
}

// turnRequestEvent is a turn's request row: the one an HTTPS POST of the same body
// would have recorded.
func (ws *wsSocket) turnRequestEvent(pctx *pipeline.Context) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		At:          time.Now(),
		Direction:   pipeline.Outbound,
		Phase:       pipeline.SessionRequest,
		RequestID:   pctx.RequestID(),
		BytesUp:     int64(len(pctx.Body)),
		MCP:         pipeline.SnapshotMCP(pctx.Extensions.MCP),
		Inference:   pipeline.SnapshotInference(pctx.Extensions.Inference),
		Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Plugins:     pipeline.SnapshotPlugins(pctx.Extensions.Custom),
		Identity:    pipeline.SnapshotIdentity(pctx),
		Host:        pctx.Host,
		HTTPMethod:  pctx.Method,
		HTTPPath:    pctx.Path,
		Client:      pctx.ClientInfo(),
	}
}

// refuseTurn answers a rejected turn to the client in the shape Codex reads as a server
// refusal — {"type":"error","status":…,"error":{…}} — so it surfaces as a failed turn
// with the policy's reason rather than as a broken socket.
func (ws *wsSocket) refuseTurn(action pipeline.Action) {
	status, message, code := http.StatusForbidden, "request rejected by policy", "policy.forbidden"
	if v := action.Violation; v != nil {
		code = v.Code
		status = v.Status
		if status == 0 {
			status = pipeline.StatusFromCode(v.Code)
		}
		if v.Reason != "" {
			message = v.Reason
		}
	}
	ev, _ := json.Marshal(map[string]any{
		"type":   "error",
		"status": status,
		"error":  map[string]string{"type": "cortex_policy", "code": code, "message": message},
	})
	ws.writeClient(wsframe.Encode(wsframe.OpText, ev, false))
}

// recordClose records the socket's close row, paired with its open by RequestID.
func (ws *wsSocket) recordClose() {
	if ws.s.Sessions == nil {
		return
	}
	ev := pipeline.SessionEvent{
		At:           time.Now(),
		Direction:    pipeline.Outbound,
		Phase:        pipeline.SessionResponse,
		RequestID:    ws.hs.RequestID(),
		Identity:     pipeline.SnapshotIdentity(ws.hs),
		Host:         ws.hs.Host,
		HTTPMethod:   ws.hs.Method,
		HTTPPath:     ws.hs.Path,
		StatusCode:   http.StatusSwitchingProtocols,
		Duration:     time.Since(ws.start),
		BytesUp:      ws.bytesUp.Load(),
		BytesDown:    ws.bytesDown.Load(),
		Tunnel:       true,
		TunnelReason: pipeline.TunnelWebSocket,
		Client:       ws.hs.ClientInfo(),
	}
	ws.s.Sessions.Append(ws.sid, ev)
	if ws.chain != nil {
		ws.s.Sessions.TouchProcess(ws.sid, ws.chain)
	}
}

// responseCreateBody is a response.create message as the HTTPS request body it stands
// for — the same object without "type" — and whether it asks for generation. Codex's
// prewarm sends generate:false; everything else generates.
func responseCreateBody(payload []byte) (body []byte, generate bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil {
		return payload, true
	}
	generate = true
	if g, ok := fields["generate"]; ok && bytes.Equal(bytes.TrimSpace(g), []byte("false")) {
		generate = false
	}
	delete(fields, "type")
	delete(fields, "generate")
	out, err := json.Marshal(fields)
	if err != nil {
		return payload, generate
	}
	return out, generate
}

// withType turns a rewritten request body back into a response.create message, with
// generate:false again when it was a prewarm.
func withType(body []byte, prewarm bool) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return body
	}
	fields["type"] = json.RawMessage(`"response.create"`)
	if prewarm {
		fields["generate"] = json.RawMessage(`false`)
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return body
	}
	return out
}
