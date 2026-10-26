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
	// the origin sends back, the SSE path's per-frame cap. A larger message is relayed
	// whole and not read: its turn is still recorded, just without a parse.
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
func (s *Server) cannotCarryWebSocket(r *http.Request, tl *tunnelLog) string {
	switch {
	case tl == nil || s.TLSBridge == nil:
		// A plaintext proxied request: no TLS bridge to re-originate over, and no
		// Responses endpoint is served over plain HTTP.
		return "not a bridged request"
	case r.ProtoMajor != 1:
		// An h2 extended CONNECT (RFC 8441) carries the socket as a stream, not a
		// connection to take over. tungstenite, Codex's client, does not send one.
		return "not HTTP/1.1"
	case s.OutboundPipeline.WritesResponseBody():
		// A response-body writer rewrites a whole response; a turn's response is a
		// live stream of frames already on their way to the client. The SSE path
		// buffers instead, and a WebSocket has no buffered form.
		return "a plugin rewrites response bodies"
	}
	// A connection that cannot be taken over is the one case left, and only the
	// ResponseWriter can say; serveResponsesWebSocket declines it there.
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
	cancel()
	if err != nil {
		status, fail := pipeline.ExchangeFailure(r.Context(), err)
		slog.Warn("forward-proxy: WebSocket handshake with the origin failed", "host", r.Host, "kind", fail.Kind, "error", fail.Message)
		http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
		s.recordWebSocketRefusal(pctx, tl, sid, chain, status, fail)
		return
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		// The origin refused the upgrade (401, 403, 426…): relay its answer as an
		// ordinary response, so the client sees what it would have without Cortex.
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
	sock := &wsSocket{
		s: s, r: r, hs: pctx, tl: tl, sid: sid, chain: chain,
		client: clientConn, clientR: clientBuf.Reader, up: upConn,
		start: time.Now(),
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

	// turnMu guards turn, which both relays touch: the client side opens one, the
	// server side feeds and ends it.
	turnMu sync.Mutex
	turn   *wsTurn
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
}

func (ws *wsSocket) run() {
	done := make(chan struct{}, 2)
	go func() { ws.relayUp(); done <- struct{}{} }()
	go func() { ws.relayDown(); done <- struct{}{} }()
	<-done
	// One side is gone: close both, so the other relay's read returns.
	_ = ws.client.Close()
	_ = ws.up.Close()
	<-done

	ws.turnMu.Lock()
	t := ws.turn
	ws.turn = nil
	ws.turnMu.Unlock()
	if t != nil {
		ws.endTurn(t, http.StatusOK, &pipeline.EventError{Kind: "upstream_error", Message: "the WebSocket closed before the turn completed"})
	}
	ws.recordClose()
}

// relayUp carries client frames to the origin, reading each data message on the way.
func (ws *wsSocket) relayUp() {
	fr := wsframe.NewReader(ws.clientR, wsClientMessageLimit, wsFrameLimit)
	msg := wsframe.NewMessage(wsClientMessageLimit)
	// held collects a data message's frames until it is whole: a turn a plugin
	// rejects or rewrites must not have reached the origin first.
	var held [][]byte
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			ws.logEnd("client", err)
			return
		}
		ws.bytesUp.Add(int64(len(f.Raw)))
		if f.IsControl() {
			if !ws.writeUp(f.Raw) {
				return
			}
			continue
		}
		op, payload, readable, complete, err := msg.Add(f)
		if err != nil {
			ws.logEnd("client", err)
			return
		}
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
				return
			}
		}
	}
}

// relayDown carries origin frames to the client, feeding each data message to the turn
// it belongs to.
func (ws *wsSocket) relayDown() {
	fr := wsframe.NewReader(ws.up, wsServerMessageLimit, wsFrameLimit)
	msg := wsframe.NewMessage(wsServerMessageLimit)
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			ws.logEnd("origin", err)
			return
		}
		ws.bytesDown.Add(int64(len(f.Raw)))
		// Forwarded first, as handleStreamingResponse writes a block after its
		// plugins have seen it only so a Reject can keep it off the wire. A
		// WebSocket turn cannot be retracted once it started, so the frame goes
		// now and the parse follows.
		if !ws.writeClient(f.Raw) {
			return
		}
		if f.IsControl() {
			continue
		}
		op, payload, readable, complete, err := msg.Add(f)
		if err != nil {
			ws.logEnd("origin", err)
			return
		}
		if complete && op == wsframe.OpText && readable {
			ws.serverMessage(payload)
		}
	}
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
	if !generate {
		// A prewarm: generate:false asks the server to set up state and answer at
		// once, with no model output. Not inference, so not a turn.
		return nil, false
	}

	ws.turnMu.Lock()
	prev := ws.turn
	ws.turn = nil
	ws.turnMu.Unlock()
	if prev != nil {
		// Codex holds the socket for one response at a time, so a second
		// response.create means the first one ended without a terminal event.
		ws.endTurn(prev, http.StatusOK, &pipeline.EventError{Kind: "upstream_error", Message: "a new turn began before this one completed"})
	}

	pctx := ws.newTurnContext(body)
	t := &wsTurn{pctx: pctx, p: ws.s.OutboundPipeline.Load()}
	action := t.p.Run(ws.ctx(), pctx)
	if action.Type == pipeline.Reject {
		ws.s.recordOutboundRejectIn(nil, pctx, action, ws.sid)
		ws.refuseTurn(action)
		t.p.RunFinish(ws.ctx(), pctx, pipeline.OutcomeFromContext(pctx))
		return nil, true
	}
	if ws.s.Sessions != nil {
		pctx.OutboundSessionID = ws.sid
		ws.s.Sessions.Append(ws.sid, ws.turnRequestEvent(pctx))
	}
	ws.turnMu.Lock()
	ws.turn = t
	ws.turnMu.Unlock()

	if pctx.BodyMutated() {
		return withType(pctx.Body), false
	}
	return nil, false
}

// serverMessage feeds one complete text message from the origin to the open turn, and
// ends the turn on a terminal event.
func (ws *wsSocket) serverMessage(payload []byte) {
	ws.turnMu.Lock()
	t := ws.turn
	ws.turnMu.Unlock()
	if t == nil {
		// The answer to a prewarm, or to a turn already ended. Nothing to parse into.
		return
	}
	var env struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
	}
	_ = json.Unmarshal(payload, &env)
	if env.Type == "error" {
		// Codex's wrapped error event — a refused turn (usage limit, bad input), with
		// the HTTP status the HTTPS path would have answered.
		status := env.Status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		ws.finishTurn(t, status, &pipeline.EventError{Kind: "backend_error", Code: fmt.Sprintf("%d", status), Message: "the origin answered the turn with an error event"})
		return
	}
	t.p.RunResponseFrame(ws.ctx(), t.pctx, payload, false)
	t.pctx.ResponseBytes += int64(len(payload))
	// The three events inference-parser reads usage and the finish reason from; each
	// ends a response, as response.completed ends Codex's own read loop.
	switch env.Type {
	case "response.completed", "response.incomplete", "response.failed":
		ws.finishTurn(t, http.StatusOK, nil)
	}
}

// finishTurn ends t if it is still the open turn.
func (ws *wsSocket) finishTurn(t *wsTurn, status int, fail *pipeline.EventError) {
	ws.turnMu.Lock()
	if ws.turn != t {
		ws.turnMu.Unlock()
		return
	}
	ws.turn = nil
	ws.turnMu.Unlock()
	ws.endTurn(t, status, fail)
}

// endTurn finalizes a turn: the terminal frame that settles its cost, its response
// row, and the Finisher dispatch — the three things handleStreamingResponse's deferred
// block does at the end of an SSE stream, in the same order.
func (ws *wsSocket) endTurn(t *wsTurn, status int, fail *pipeline.EventError) {
	finalCtx, cancelFinal := httpx.TeardownContext(ws.ctx())
	defer cancelFinal()
	t.pctx.MarkResponseDelivered()
	if fail == nil {
		t.pctx.StatusCode = status
	}
	t.p.RunResponseFrame(finalCtx, t.pctx, nil, true)
	ws.s.recordOutboundResponseEvent(t.pctx, status, fail)
	t.p.RunFinish(finalCtx, t.pctx, pipeline.OutcomeFromContext(t.pctx))
}

// ctx is the context every turn's pipeline runs under: the handshake request's values
// (the client's process, for attribution) without its cancellation, since the handshake
// request ended when the upgrade was answered and the socket outlives it.
func (ws *wsSocket) ctx() context.Context {
	return context.WithoutCancel(ws.r.Context())
}

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
		Session:       hs.Session,
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

// withType turns a rewritten request body back into a response.create message.
func withType(body []byte) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return body
	}
	fields["type"] = json.RawMessage(`"response.create"`)
	out, err := json.Marshal(fields)
	if err != nil {
		return body
	}
	return out
}
