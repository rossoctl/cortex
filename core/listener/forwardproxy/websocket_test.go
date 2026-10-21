package forwardproxy

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/listener/internal/wsframe"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferenceparser"
	"github.com/rossoctl/cortex/core/plugins/plugintesting"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// End to end: a Codex-shaped client opens a WebSocket through a real CONNECT bridge to
// a TLS origin that speaks the Responses WebSocket protocol, and every turn on it comes
// out of the session store parsed — model, tokens, completion — the way the same turn
// over HTTPS would.

const wsPath = "/backend-api/codex/responses"

// wsOrigin is a fake chatgpt.com: it accepts a WebSocket on wsPath and answers each
// response.create with a stream of Responses events, one per text message.
type wsOrigin struct {
	*httptest.Server
	mu sync.Mutex
	// creates are the response.create payloads the origin received, in order.
	creates []map[string]any
	// sawExtensions is the Sec-WebSocket-Extensions offer the handshake reached it with.
	sawExtensions string
}

func newWSOrigin(t *testing.T) *wsOrigin {
	t.Helper()
	o := &wsOrigin{}
	o.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			return // the bridge's upstream-verify probe
		}
		if r.URL.Path != wsPath || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		o.mu.Lock()
		o.sawExtensions = r.Header.Get("Sec-Websocket-Extensions")
		o.mu.Unlock()
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		accept := wsAccept(r.Header.Get("Sec-Websocket-Key"))
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n")
		_ = brw.Flush()
		o.serve(conn, brw.Reader)
	}))
	t.Cleanup(o.Close)
	return o
}

func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}

func (o *wsOrigin) serve(conn net.Conn, br *bufio.Reader) {
	fr := wsframe.NewReader(br, 1<<20, 1<<20)
	msg := wsframe.NewMessage(1 << 20)
	turn := 0
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			return
		}
		if f.Opcode == wsframe.OpPing {
			_, _ = conn.Write(wsframe.Encode(wsframe.OpPong, f.Payload, false))
			continue
		}
		if f.Opcode == wsframe.OpClose {
			_, _ = conn.Write(wsframe.Encode(wsframe.OpClose, f.Payload, false))
			return
		}
		_, payload, readable, ok, _ := msg.Add(f)
		if !ok || !readable {
			continue
		}
		var m map[string]any
		if json.Unmarshal(payload, &m) != nil || m["type"] != "response.create" {
			continue
		}
		o.mu.Lock()
		o.creates = append(o.creates, m)
		o.mu.Unlock()
		if g, ok := m["generate"].(bool); ok && !g {
			// A prewarm: completed at once, no output, no usage.
			_, _ = conn.Write(wsframe.Encode(wsframe.OpText, []byte(`{"type":"response.completed","response":{"id":"warm","status":"completed"}}`), false))
			continue
		}
		turn++
		for _, ev := range turnEvents(turn) {
			_, _ = conn.Write(wsframe.Encode(wsframe.OpText, []byte(ev), false))
		}
	}
}

// turnEvents is the event sequence live Codex traffic carries for a one-word reply,
// with per-turn usage so two turns are told apart.
func turnEvents(n int) []string {
	id := map[int]string{1: "resp_1", 2: "resp_2"}[n]
	input := map[int]int{1: 12862, 2: 13002}[n]
	return []string{
		`{"type":"response.created","response":{"id":"` + id + `","status":"in_progress"}}`,
		`{"type":"codex.rate_limits","primary":{"used_percent":1.0}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg","type":"message"}}`,
		`{"type":"response.output_text.delta","item_id":"msg","output_index":0,"content_index":0,"delta":"po"}`,
		`{"type":"response.output_text.delta","item_id":"msg","output_index":0,"content_index":0,"delta":"ng"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg","type":"message"}}`,
		`{"type":"response.completed","response":{"id":"` + id + `","status":"completed","usage":{` +
			`"input_tokens":` + itoa(input) + `,"input_tokens_details":{"cached_tokens":11008},` +
			`"output_tokens":5,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":` + itoa(input+5) + `}}}`,
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// wsHarness is a proxy with the TLS bridge on, in front of a wsOrigin.
type wsHarness struct {
	store  *session.Store
	origin *wsOrigin
	proxy  *httptest.Server
	caPEM  []byte
}

func newWSHarness(t *testing.T, mode string, plugins ...pipeline.Plugin) *wsHarness {
	t.Helper()
	h := &wsHarness{store: session.New(5*time.Minute, 1000, 0), origin: newWSOrigin(t)}
	t.Cleanup(h.store.Close)

	originCAPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: h.origin.Certificate().Raw})
	src, err := tlsbridge.NewEphemeralSource()
	if err != nil {
		t.Fatal(err)
	}
	up, err := tlsbridge.NewUpstreamClient(originCAPEM, false)
	if err != nil {
		t.Fatal(err)
	}
	ou, _ := url.Parse(h.origin.URL)
	engine := &tlsbridge.Engine{
		Decision: mustBridgeDecision(t, portOf(ou.Host)),
		Term:     tlsbridge.NewTerminator(tlsbridge.NewMinter(src, tlsbridge.MinterOpts{})),
		Skip:     tlsbridge.NewSkipSet(),
		Upstream: up,
		CAPEM:    src.CACertPEM(),
	}
	h.caPEM = engine.CAPEM

	all := append([]pipeline.Plugin{inferenceparser.NewInferenceParser()}, plugins...)
	p, err := plugintesting.BuildPipeline(all)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         h.store,
		Client:           http.DefaultClient,
		TLSBridge:        engine,
		// Codex's session header, as a laptop install reads it.
		SessionIDHeaders:    []string{session.CodexSessionHeader},
		ResponsesWebSockets: mode,
	}
	h.proxy = httptest.NewServer(srv.Handler())
	t.Cleanup(h.proxy.Close)
	return h
}

// wsClient is the Codex end: a WebSocket over a CONNECT bridge through the proxy.
type wsClient struct {
	t      *testing.T
	conn   *tls.Conn
	fr     *wsframe.Reader
	msg    *wsframe.Message
	status int
	header http.Header
	body   string
}

// dial opens the CONNECT, the TLS session against the bridge's forged leaf, and sends
// the WebSocket handshake with the headers Codex sends. A non-101 answer is kept on the
// client for the test to read.
func (h *wsHarness) dial(t *testing.T, extra http.Header) *wsClient {
	t.Helper()
	pu, _ := url.Parse(h.proxy.URL)
	ou, _ := url.Parse(h.origin.URL)
	raw, err := net.Dial("tcp", pu.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	_ = raw.SetDeadline(time.Now().Add(15 * time.Second))
	connect, _ := http.NewRequest(http.MethodConnect, "//"+ou.Host, nil)
	connect.Host, connect.URL.Host = ou.Host, ou.Host
	if err := connect.Write(raw); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, connect)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(h.caPEM)
	conn := tls.Client(&bufferedConn{Conn: raw, r: br}, &tls.Config{
		ServerName: hostOnly(ou.Host),
		RootCAs:    pool,
		NextProtos: []string{"http/1.1"},
	})
	if err := conn.Handshake(); err != nil {
		t.Fatalf("TLS through the bridge: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, "https://"+ou.Host+wsPath, nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Extensions", "permessage-deflate; client_max_window_bits")
	req.Header.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
	req.Header.Set("Session-Id", "11111111-2222-3333-4444-555555555555")
	req.Header.Set("User-Agent", "codex_exec/0.163.0 (Mac OS 26.5.1; arm64) unknown")
	for k, v := range extra {
		req.Header[k] = v
	}
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	cbr := bufio.NewReader(conn)
	hs, err := http.ReadResponse(cbr, req)
	if err != nil {
		t.Fatalf("reading the handshake answer: %v", err)
	}
	c := &wsClient{t: t, conn: conn, status: hs.StatusCode, header: hs.Header}
	if hs.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(hs.Body)
		c.body = string(b)
		return c
	}
	c.fr = wsframe.NewReader(cbr, 1<<20, 1<<20)
	c.msg = wsframe.NewMessage(1 << 20)
	return c
}

func (c *wsClient) send(v string) {
	c.t.Helper()
	if _, err := c.conn.Write(wsframe.Encode(wsframe.OpText, []byte(v), true)); err != nil {
		c.t.Fatal(err)
	}
}

// readUntil reads server messages until one of the given type arrives, returning all
// of them.
func (c *wsClient) readUntil(typ string) []map[string]any {
	c.t.Helper()
	var out []map[string]any
	for {
		f, err := c.fr.ReadFrame()
		if err != nil {
			c.t.Fatalf("reading from the socket: %v (after %d messages)", err, len(out))
		}
		_, payload, readable, ok, _ := c.msg.Add(f)
		if !ok || !readable {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal(payload, &m)
		out = append(out, m)
		if m["type"] == typ {
			return out
		}
	}
}

func (c *wsClient) close() {
	_, _ = c.conn.Write(wsframe.Encode(wsframe.OpClose, wsframe.ClosePayload(1000, ""), true))
	_ = c.conn.Close()
}

func responseCreate(text string) string {
	return `{"type":"response.create","model":"gpt-6-luna","stream":true,"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}]}`
}

// events reads the session's rows once the socket has closed and recorded its close.
func (h *wsHarness) events(t *testing.T, sid string, wantClose bool) []pipeline.SessionEvent {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		v := h.store.View(sid)
		if v != nil {
			if !wantClose {
				return v.Events
			}
			for _, e := range v.Events {
				if e.Tunnel && e.Phase == pipeline.SessionResponse {
					return v.Events
				}
			}
		}
		if time.Now().After(deadline) {
			if v == nil {
				t.Fatalf("no session %q", sid)
			}
			return v.Events
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const codexSID = "11111111-2222-3333-4444-555555555555"

// The headline: two turns and a prewarm on one socket become two parsed inference
// pairs, between the socket's own open and close, all in Codex's session.
func TestResponsesWebSocket_TurnsAreParsedLikeHTTPS(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsParse)
	c := h.dial(t, nil)
	if c.status != http.StatusSwitchingProtocols {
		t.Fatalf("handshake answered %d (%s), want 101", c.status, c.body)
	}

	c.send(`{"type":"response.create","model":"gpt-6-luna","stream":true,"generate":false,"input":[]}`)
	c.readUntil("response.completed")
	c.send(responseCreate("hello"))
	first := c.readUntil("response.completed")
	c.send(responseCreate("again"))
	c.readUntil("response.completed")
	c.close()

	// The client sees every event the origin sent, unchanged, codex's own included.
	var sawRateLimits bool
	for _, m := range first {
		if m["type"] == "codex.rate_limits" {
			sawRateLimits = true
		}
	}
	if !sawRateLimits {
		t.Error("the relay dropped a codex.rate_limits event")
	}

	evs := h.events(t, codexSID, true)
	// The bridged CONNECT records its own open, folded by agentop into the first
	// request it carried — here the handshake. The socket's rows are the ones with the
	// websocket reason.
	var turns, opens, closes []pipeline.SessionEvent
	for _, e := range evs {
		switch {
		case e.Tunnel && e.TunnelReason == pipeline.TunnelWebSocket && e.Phase == pipeline.SessionRequest:
			opens = append(opens, e)
		case e.Tunnel && e.TunnelReason == pipeline.TunnelWebSocket && e.Phase == pipeline.SessionResponse:
			closes = append(closes, e)
		case e.HTTPMethod == http.MethodPost:
			turns = append(turns, e)
		}
	}
	if len(opens) != 1 || opens[0].TunnelReason != pipeline.TunnelWebSocket || opens[0].HTTPMethod != http.MethodGet {
		t.Fatalf("socket open rows = %+v, want one GET tunnel row with reason websocket", opens)
	}
	if len(closes) != 1 || closes[0].RequestID != opens[0].RequestID || closes[0].BytesUp == 0 || closes[0].BytesDown == 0 {
		t.Fatalf("socket close rows = %+v, want one paired with the open, carrying the bytes", closes)
	}
	// agentop renders upgradeStripped as "websocket not forwarded"; on a socket that was
	// carried that line would be false.
	for _, e := range evs {
		if e.UpgradeStripped != "" {
			t.Errorf("row %d (%v %s) carries upgradeStripped=%q on a carried socket", e.Seq, e.Phase, e.HTTPMethod, e.UpgradeStripped)
		}
	}
	// Two generating turns; the prewarm is not inference and records nothing.
	if len(turns) != 4 {
		for _, e := range evs {
			t.Logf("  %v %s %s status=%d tunnel=%v", e.Phase, e.HTTPMethod, e.HTTPPath, e.StatusCode, e.Tunnel)
		}
		t.Fatalf("got %d turn rows, want 4 (two request/response pairs)", len(turns))
	}
	wantInput := []int{12862, 13002}
	for i := 0; i < 2; i++ {
		req, res := turns[2*i], turns[2*i+1]
		if req.Phase != pipeline.SessionRequest || res.Phase != pipeline.SessionResponse || req.RequestID != res.RequestID {
			t.Errorf("turn %d rows do not pair: %+v / %+v", i+1, req, res)
			continue
		}
		if req.Inference == nil || req.Inference.Model != "gpt-6-luna" || len(req.Inference.Messages) != 1 {
			t.Errorf("turn %d request parse = %+v, want model gpt-6-luna and its one message", i+1, req.Inference)
		}
		if res.StatusCode != http.StatusOK || res.Inference == nil {
			t.Errorf("turn %d response = status %d inference %+v", i+1, res.StatusCode, res.Inference)
			continue
		}
		inf := res.Inference
		// Cached tokens are subtracted from the prompt, as on the HTTPS path.
		if inf.Completion != "pong" || inf.CompletionTokens != 5 || inf.TotalTokens != wantInput[i]+5 {
			t.Errorf("turn %d response parse: completion=%q out=%d total=%d", i+1, inf.Completion, inf.CompletionTokens, inf.TotalTokens)
		}
	}
	if turns[0].RequestID == turns[2].RequestID {
		t.Error("the two turns share a RequestID; each needs its own")
	}
	// Turns sit between the socket's open and close in the timeline.
	if !(opens[0].Seq < turns[0].Seq && turns[3].Seq < closes[0].Seq) {
		t.Errorf("order: open %d, turns %d..%d, close %d", opens[0].Seq, turns[0].Seq, turns[3].Seq, closes[0].Seq)
	}
}

// Compression would make every frame unreadable, so the offer never reaches the origin.
func TestResponsesWebSocket_DeflateOfferIsStripped(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsParse)
	c := h.dial(t, nil)
	if c.status != http.StatusSwitchingProtocols {
		t.Fatalf("handshake answered %d", c.status)
	}
	c.close()
	h.origin.mu.Lock()
	defer h.origin.mu.Unlock()
	if h.origin.sawExtensions != "" {
		t.Errorf("origin saw Sec-WebSocket-Extensions %q; the offer must be stripped", h.origin.sawExtensions)
	}
}

// A turn a plugin rejects never reaches the origin, and the client is told in the
// error-event shape Codex reads as a server refusal.
func TestResponsesWebSocket_RejectedTurnIsAnsweredAndNotForwarded(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsParse, &turnDenier{})
	c := h.dial(t, nil)
	if c.status != http.StatusSwitchingProtocols {
		t.Fatalf("handshake answered %d", c.status)
	}
	c.send(responseCreate("forbidden"))
	msgs := c.readUntil("error")
	errEv := msgs[len(msgs)-1]
	if errEv["status"] != float64(http.StatusForbidden) {
		t.Errorf("error event status = %v, want 403", errEv["status"])
	}
	c.close()

	h.origin.mu.Lock()
	n := len(h.origin.creates)
	h.origin.mu.Unlock()
	if n != 0 {
		t.Errorf("origin received %d response.create; a rejected turn must not be forwarded", n)
	}
	var denied bool
	for _, e := range h.events(t, codexSID, true) {
		if e.Phase == pipeline.SessionDenied {
			denied = true
		}
	}
	if !denied {
		t.Error("no denied row recorded for the rejected turn")
	}
}

// A request-body writer's rewrite is what the origin receives.
func TestResponsesWebSocket_RewrittenTurnIsWhatTheOriginGets(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsParse, &modelRewriter{})
	c := h.dial(t, nil)
	if c.status != http.StatusSwitchingProtocols {
		t.Fatalf("handshake answered %d", c.status)
	}
	c.send(responseCreate("hello"))
	c.readUntil("response.completed")
	c.close()

	h.origin.mu.Lock()
	defer h.origin.mu.Unlock()
	if len(h.origin.creates) != 1 {
		t.Fatalf("origin got %d messages, want 1", len(h.origin.creates))
	}
	got := h.origin.creates[0]
	if got["model"] != "rewritten-model" || got["type"] != "response.create" {
		t.Errorf("origin got %v, want the rewritten model on a response.create", got)
	}
}

// A socket the proxy cannot carry is declined, never forwarded: with a response-body
// writer in the chain the turn's live frames could not be rewritten.
func TestResponsesWebSocket_UncarriableSocketIsDeclined(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsParse, &responseRewriter{})
	c := h.dial(t, nil)
	if c.status != http.StatusUpgradeRequired {
		t.Fatalf("handshake answered %d, want 426", c.status)
	}
}

// Decline mode still answers 426 through the bridge, as #1462 shipped it.
func TestResponsesWebSocket_DeclineModeAnswers426(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsDecline)
	c := h.dial(t, nil)
	if c.status != http.StatusUpgradeRequired {
		t.Fatalf("handshake answered %d, want 426", c.status)
	}
}

// The origin refusing the upgrade is relayed as its own answer.
func TestResponsesWebSocket_OriginRefusalIsRelayed(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsParse)
	h.origin.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"detail":"unauthorized"}`)
	})
	c := h.dial(t, nil)
	if c.status != http.StatusUnauthorized || !strings.Contains(c.body, "unauthorized") {
		t.Fatalf("handshake answered %d %q, want the origin's 401", c.status, c.body)
	}
}

// A client that hangs up mid-turn still gets that turn recorded, with what had
// arrived, and the socket's close.
func TestResponsesWebSocket_ClientHangupFinalizesTheOpenTurn(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsParse)
	// An origin that answers a create with a partial stream and then stalls.
	h.origin.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			return
		}
		conn, brw, _ := w.(http.Hijacker).Hijack()
		defer conn.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + wsAccept(r.Header.Get("Sec-Websocket-Key")) + "\r\n\r\n")
		_ = brw.Flush()
		fr := wsframe.NewReader(brw.Reader, 1<<20, 1<<20)
		if _, err := fr.ReadFrame(); err != nil {
			return
		}
		_, _ = conn.Write(wsframe.Encode(wsframe.OpText, []byte(`{"type":"response.created","response":{"id":"r","status":"in_progress"}}`), false))
		_, _ = conn.Write(wsframe.Encode(wsframe.OpText, []byte(`{"type":"response.output_text.delta","item_id":"m","output_index":0,"content_index":0,"delta":"par"}`), false))
		_, _ = fr.ReadFrame() // until the proxy closes us
	})
	c := h.dial(t, nil)
	c.send(responseCreate("hello"))
	c.readUntil("response.output_text.delta")
	_ = c.conn.Close()

	var res *pipeline.SessionEvent
	for _, e := range h.events(t, codexSID, true) {
		if e.HTTPMethod == http.MethodPost && e.Phase == pipeline.SessionResponse {
			e := e
			res = &e
		}
	}
	if res == nil {
		t.Fatal("the interrupted turn recorded no response row")
	}
	if res.Error == nil || res.Inference == nil || res.Inference.Completion != "par" {
		t.Errorf("interrupted turn: error=%+v completion=%v, want an error and the partial text", res.Error, res.Inference)
	}
}

// --- test plugins ---

// turnDenier rejects every inference request, and nothing else, so the handshake GET
// passes and the turn is refused.
type turnDenier struct{}

func (turnDenier) Name() string                              { return "turn-denier" }
func (turnDenier) Capabilities() pipeline.PluginCapabilities { return pipeline.PluginCapabilities{} }
func (turnDenier) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if pctx.Extensions.Inference == nil {
		return pipeline.Action{Type: pipeline.Continue}
	}
	return pctx.DenyAndRecord("test_deny", "policy.forbidden", "denied by the test")
}
func (turnDenier) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// modelRewriter rewrites an inference request's model, as a request-body writer can.
type modelRewriter struct{}

func (modelRewriter) Name() string { return "model-rewriter" }
func (modelRewriter) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{ReadsBody: true, WritesRequestBody: true}
}
func (modelRewriter) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if pctx.Extensions.Inference == nil {
		return pipeline.Action{Type: pipeline.Continue}
	}
	var m map[string]any
	if json.Unmarshal(pctx.Body, &m) == nil {
		m["model"] = "rewritten-model"
		b, _ := json.Marshal(m)
		pctx.SetBody(b)
	}
	return pipeline.Action{Type: pipeline.Continue}
}
func (modelRewriter) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// responseRewriter declares a response-body write, which a live socket cannot honour.
type responseRewriter struct{}

func (responseRewriter) Name() string { return "response-rewriter" }
func (responseRewriter) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{ReadsBody: true, WritesResponseBody: true}
}
func (responseRewriter) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (responseRewriter) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
