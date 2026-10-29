package forwardproxy

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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

// wsHost is the name the origin is addressed by. It must be one of OpenAI's: the
// WebSocket path applies only there (isOpenAIResponsesHost).
const wsHost = "chatgpt.com"

// newWSOrigin starts the origin with a certificate for wsHost, from a CA of its own —
// so the bridge's upstream check verifies it as it would OpenAI's — and returns that
// CA's PEM for the bridge to trust.
func newWSOrigin(t *testing.T) (*wsOrigin, []byte) {
	t.Helper()
	o := &wsOrigin{}
	o.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	ca, err := tlsbridge.NewEphemeralSource()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := tlsbridge.NewMinter(ca, tlsbridge.MinterOpts{}).GetCertificateForHost(wsHost)
	if err != nil {
		t.Fatal(err)
	}
	o.TLS = &tls.Config{Certificates: []tls.Certificate{*leaf}}
	o.StartTLS()
	t.Cleanup(o.Close)
	return o, ca.CACertPEM()
}

// originPort is the port the origin listens on, which requests name with wsHost.
func (o *wsOrigin) originPort() string {
	_, port, _ := net.SplitHostPort(o.Listener.Addr().String())
	return port
}

// authority is how a request addresses the origin: wsHost at its port.
func (o *wsOrigin) authority() string { return net.JoinHostPort(wsHost, o.originPort()) }

func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}

func (o *wsOrigin) serve(conn net.Conn, br *bufio.Reader) {
	fr := wsframe.NewReader(br, 64<<20, 64<<20)
	msg := wsframe.NewMessage(64 << 20)
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
	// holder is the outbound pipeline the proxy runs, so a test can swap it as a hot
	// reload does.
	holder *pipeline.Holder
}

func newWSHarness(t *testing.T, mode string, plugins ...pipeline.Plugin) *wsHarness {
	t.Helper()
	origin, originCAPEM := newWSOrigin(t)
	h := &wsHarness{store: session.New(5*time.Minute, 1000, 0), origin: origin}
	t.Cleanup(h.store.Close)

	src, err := tlsbridge.NewEphemeralSource()
	if err != nil {
		t.Fatal(err)
	}
	up, err := tlsbridge.NewUpstreamClient(originCAPEM, false)
	if err != nil {
		t.Fatal(err)
	}
	// The bridge's upstream client resolves wsHost to the local origin, as DNS would
	// resolve chatgpt.com to OpenAI's.
	originAddr := h.origin.Listener.Addr().String()
	toOrigin := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, originAddr)
	}
	up.Transport.(*http.Transport).DialContext = toOrigin
	engine := &tlsbridge.Engine{
		Decision: mustBridgeDecision(t, portOf(h.origin.authority())),
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
	h.holder = pipeline.NewHolder(p)
	srv := &Server{
		OutboundPipeline: h.holder,
		Sessions:         h.store,
		Client:           http.DefaultClient,
		TLSBridge:        engine,
		// Codex's session header, as a laptop install reads it.
		SessionIDHeaders:    []string{session.CodexSessionHeader},
		ResponsesWebSockets: mode,
		dialTunnel: func(string, time.Duration) (net.Conn, error) {
			return net.DialTimeout("tcp", originAddr, 5*time.Second)
		},
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
	authority := h.origin.authority()
	raw, err := net.Dial("tcp", pu.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	_ = raw.SetDeadline(time.Now().Add(15 * time.Second))
	connect, _ := http.NewRequest(http.MethodConnect, "//"+authority, nil)
	connect.Host, connect.URL.Host = authority, authority
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
		ServerName: wsHost,
		RootCAs:    pool,
		NextProtos: []string{"http/1.1"},
	})
	if err := conn.Handshake(); err != nil {
		t.Fatalf("TLS through the bridge: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, "https://"+authority+wsPath, nil)
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
	deadline := time.Now().Add(10 * time.Second)
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
			// Failing here, not returning what there is: a slow run would otherwise
			// fail later, on an assertion about rows that had not landed yet.
			t.Fatalf("session %q has no WebSocket close row after 10s (%d events)", sid, len(v.Events))
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

	// After the socket has ended, so a forwarded frame would have been read by now.
	events := h.events(t, codexSID, true)
	h.origin.mu.Lock()
	n := len(h.origin.creates)
	h.origin.mu.Unlock()
	if n != 0 {
		t.Errorf("origin received %d response.create; a rejected turn must not be forwarded", n)
	}
	var denied bool
	for _, e := range events {
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

// The same handshake to a host that is not OpenAI's is neither carried nor declined:
// it is forwarded with Upgrade stripped, as every WebSocket was, and gets its origin's
// own answer. What a 426 means is only known for Codex against OpenAI.
func TestResponsesWebSocket_OtherHostsAreForwardedAsBefore(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	var reached atomic.Int32
	var sawUpgrade atomic.Value
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		sawUpgrade.Store(r.Header.Get("Upgrade"))
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	t.Cleanup(origin.Close)
	p, err := plugintesting.BuildPipeline([]pipeline.Plugin{inferenceparser.NewInferenceParser()})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline:    pipeline.NewHolder(p),
		Sessions:            store,
		Client:              http.DefaultClient,
		ResponsesWebSockets: config.ResponsesWebSocketsParse,
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	pu, _ := url.Parse(proxy.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}

	req, _ := http.NewRequest(http.MethodGet, origin.URL+"/v1/responses", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || reached.Load() != 1 {
		t.Errorf("status %d, origin reached %d times; want the origin's own 405", resp.StatusCode, reached.Load())
	}
	if got, _ := sawUpgrade.Load().(string); got != "" {
		t.Errorf("origin received Upgrade %q; it must still be stripped", got)
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

// A client that hangs up mid-turn — Codex does this on Esc, closing the socket rather
// than interrupting over it — still gets that turn recorded with what had arrived, as a
// 200 turn_interrupted the way an HTTPS stream the client leaves keeps its 200, not as an
// upstream failure: the origin did nothing wrong.
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
	if res.Inference == nil || res.Inference.Completion != "par" {
		t.Errorf("interrupted turn completion = %+v, want the partial text", res.Inference)
	}
	// As HTTPS records a stream the client leaves part way: the 200 the origin was
	// answering with, and a kind /v1/usage does not count as an error.
	if res.StatusCode != http.StatusOK || res.Error == nil || res.Error.Kind != pipeline.ErrorKindTurnInterrupted {
		t.Errorf("interrupted turn = status %d error %+v, want 200 %s", res.StatusCode, res.Error, pipeline.ErrorKindTurnInterrupted)
	}
}

// The origin dropping the socket mid-turn is the upstream failure: a 502 upstream_error,
// as the HTTPS path records a stream that breaks off from the server's side.
func TestResponsesWebSocket_OriginDropFinalizesTheOpenTurnAsUpstreamError(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsParse)
	h.origin.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			return
		}
		conn, brw, _ := w.(http.Hijacker).Hijack()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + wsAccept(r.Header.Get("Sec-Websocket-Key")) + "\r\n\r\n")
		_ = brw.Flush()
		fr := wsframe.NewReader(brw.Reader, 1<<20, 1<<20)
		if _, err := fr.ReadFrame(); err != nil {
			_ = conn.Close()
			return
		}
		_, _ = conn.Write(wsframe.Encode(wsframe.OpText, []byte(`{"type":"response.created","response":{"id":"r","status":"in_progress"}}`), false))
		_, _ = conn.Write(wsframe.Encode(wsframe.OpText, []byte(`{"type":"response.output_text.delta","item_id":"m","output_index":0,"content_index":0,"delta":"par"}`), false))
		_ = conn.Close() // the origin goes away mid-turn
	})
	c := h.dial(t, nil)
	c.send(responseCreate("hello"))
	c.readUntil("response.output_text.delta")

	var res *pipeline.SessionEvent
	for _, e := range h.events(t, codexSID, true) {
		if e.HTTPMethod == http.MethodPost && e.Phase == pipeline.SessionResponse {
			e := e
			res = &e
		}
	}
	if res == nil {
		t.Fatal("the dropped turn recorded no response row")
	}
	if res.StatusCode != http.StatusBadGateway || res.Error == nil || res.Error.Kind != "upstream_error" {
		t.Errorf("dropped turn = status %d error %+v, want 502 upstream_error", res.StatusCode, res.Error)
	}
	_ = c.conn.Close()
}

// A clean close mid-turn names who ended the socket even when the other side's relay
// stops first. Here the client sends a close frame, the origin answers with one of its
// own and drops the connection at once, so the relay reading from the origin is the
// first to see its connection gone — timing alone would blame the origin. The client
// sent the first close, so the turn is the client's: 200 turn_interrupted, with the code
// it closed with.
func TestResponsesWebSocket_FirstCloseFrameDecidesWhoEndedTheTurn(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsParse)
	h.origin.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			return
		}
		conn, brw, _ := w.(http.Hijacker).Hijack()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + wsAccept(r.Header.Get("Sec-Websocket-Key")) + "\r\n\r\n")
		_ = brw.Flush()
		fr := wsframe.NewReader(brw.Reader, 1<<20, 1<<20)
		if _, err := fr.ReadFrame(); err != nil {
			_ = conn.Close()
			return
		}
		_, _ = conn.Write(wsframe.Encode(wsframe.OpText, []byte(`{"type":"response.created","response":{"id":"r","status":"in_progress"}}`), false))
		_, _ = conn.Write(wsframe.Encode(wsframe.OpText, []byte(`{"type":"response.output_text.delta","item_id":"m","output_index":0,"content_index":0,"delta":"par"}`), false))
		// Answer the client's close with our own, then drop the connection at once.
		for {
			f, err := fr.ReadFrame()
			if err != nil {
				_ = conn.Close()
				return
			}
			if f.Opcode == wsframe.OpClose {
				_, _ = conn.Write(wsframe.Encode(wsframe.OpClose, f.Payload, false))
				_ = conn.Close()
				return
			}
		}
	})
	c := h.dial(t, nil)
	c.send(responseCreate("hello"))
	c.readUntil("response.output_text.delta")
	_, _ = c.conn.Write(wsframe.Encode(wsframe.OpClose, wsframe.ClosePayload(1001, "user left"), true))

	var res *pipeline.SessionEvent
	for _, e := range h.events(t, codexSID, true) {
		if e.HTTPMethod == http.MethodPost && e.Phase == pipeline.SessionResponse {
			e := e
			res = &e
		}
	}
	_ = c.conn.Close()
	if res == nil {
		t.Fatal("the closed turn recorded no response row")
	}
	if res.StatusCode != http.StatusOK || res.Error == nil || res.Error.Kind != pipeline.ErrorKindTurnInterrupted {
		t.Fatalf("turn = status %d error %+v, want 200 %s: the client sent the first close", res.StatusCode, res.Error, pipeline.ErrorKindTurnInterrupted)
	}
	if !strings.Contains(res.Error.Message, "close code 1001 going away: user left") {
		t.Errorf("message %q does not name the client's close code and reason", res.Error.Message)
	}
}

// A refused turn's status follows the error event's own status when it has one, and
// otherwise the status its error code is answered with over HTTPS — so a quota refusal,
// which arrived live with no status, counts as the 429 it is and not as a 500.
func TestErrorEventStatus(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		code, errType string
		want          int
	}{
		{"the event's own status wins", 429, "server_is_overloaded", "", 429},
		{"quota, no status", 0, "insufficient_quota", "", 429},
		{"quota as the type", 0, "", "insufficient_quota", 429},
		{"rate limit", 0, "rate_limit_exceeded", "", 429},
		{"usage limit", 0, "", "usage_limit_reached", 429},
		{"socket limit", 0, "websocket_connection_limit_reached", "", 429},
		{"context too long", 0, "context_length_exceeded", "", 400},
		{"stale previous response", 0, "previous_response_not_found", "", 400},
		{"overloaded", 0, "server_is_overloaded", "", 503},
		{"unknown code", 0, "something_new", "", 500},
		{"nothing at all", 0, "", "", 500},
		{"a non-error status is not taken", 200, "insufficient_quota", "", 429},
	}
	for _, tc := range cases {
		if got := errorEventStatus(tc.status, tc.code, tc.errType); got != tc.want {
			t.Errorf("%s: errorEventStatus(%d, %q, %q) = %d, want %d", tc.name, tc.status, tc.code, tc.errType, got, tc.want)
		}
	}
}

// End to end: a quota refusal with no status, as one arrived live, is recorded as 429
// with its code named, and the client receives the event unchanged.
func TestResponsesWebSocket_QuotaRefusalIsRecordedAs429(t *testing.T) {
	const refusal = `{"type":"error","error":{"type":"insufficient_quota","code":"insufficient_quota","message":"You exceeded your current quota."}}`
	h := newWSHarness(t, config.ResponsesWebSocketsParse)
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
		_, _ = conn.Write(wsframe.Encode(wsframe.OpText, []byte(refusal), false))
		_, _ = fr.ReadFrame()
	})
	c := h.dial(t, nil)
	c.send(responseCreate("hello"))
	got := c.readUntil("error")
	c.close()
	if b, _ := json.Marshal(got[len(got)-1]); !strings.Contains(string(b), `"insufficient_quota"`) {
		t.Errorf("client received %s, want the refusal unchanged", b)
	}

	var res *pipeline.SessionEvent
	for _, e := range h.events(t, codexSID, true) {
		if e.HTTPMethod == http.MethodPost && e.Phase == pipeline.SessionResponse {
			e := e
			res = &e
		}
	}
	if res == nil || res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("refused turn = %+v, want status 429", res)
	}
	if res.Error == nil || !strings.Contains(res.Error.Message, "insufficient_quota") {
		t.Errorf("refused turn error %+v does not name the code", res.Error)
	}
}

// Codex's in-band interrupt: for steering a turn already under way, or for queued
// mailbox input, on models with use_responses_lite set, it sends response.interrupt
// naming the response and waits on the same socket for the server's
// response.interrupt.accepted carrying that id, which it rejects as "unexpected" or
// "invalid" if it does not arrive intact. (Esc does not use this: it closes the socket,
// which TestResponsesWebSocket_ClientHangupFinalizesTheOpenTurn covers.) The relay must carry both unchanged, record
// the interrupted turn once with what had arrived, and not mistake the acknowledgement
// for anything that opens or ends a turn — in either order the server may send the
// acknowledgement and the turn's terminal event.
func TestResponsesWebSocket_InterruptIsRelayedAndTheTurnRecordedOnce(t *testing.T) {
	const (
		interrupt = `{"type":"response.interrupt","response_id":"resp_i","mode":"discard_partial_items"}`
		ack       = `{"type":"response.interrupt.accepted","response_id":"resp_i"}`
		terminal  = `{"type":"response.incomplete","response":{"id":"resp_i","status":"incomplete","usage":{` +
			`"input_tokens":12862,"input_tokens_details":{"cached_tokens":11008},"output_tokens":2,"total_tokens":12864}}}`
	)
	for _, order := range []struct {
		name  string
		after []string
	}{
		{"ack then terminal", []string{ack, terminal}},
		{"terminal then ack", []string{terminal, ack}},
	} {
		t.Run(order.name, func(t *testing.T) {
			h := newWSHarness(t, config.ResponsesWebSocketsParse)
			var gotInterrupt atomic.Value
			h.origin.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					return
				}
				conn, brw, _ := w.(http.Hijacker).Hijack()
				defer conn.Close()
				_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + wsAccept(r.Header.Get("Sec-Websocket-Key")) + "\r\n\r\n")
				_ = brw.Flush()
				fr := wsframe.NewReader(brw.Reader, 1<<20, 1<<20)
				if _, err := fr.ReadFrame(); err != nil { // the response.create
					return
				}
				for _, ev := range []string{
					`{"type":"response.created","response":{"id":"resp_i","status":"in_progress"}}`,
					`{"type":"response.output_text.delta","item_id":"m","output_index":0,"content_index":0,"delta":"po"}`,
				} {
					_, _ = conn.Write(wsframe.Encode(wsframe.OpText, []byte(ev), false))
				}
				f, err := fr.ReadFrame() // the interrupt
				if err != nil {
					return
				}
				gotInterrupt.Store(string(f.Payload))
				for _, ev := range order.after {
					_, _ = conn.Write(wsframe.Encode(wsframe.OpText, []byte(ev), false))
				}
				_, _ = fr.ReadFrame()
			})

			c := h.dial(t, nil)
			c.send(responseCreate("hello"))
			c.readUntil("response.output_text.delta")
			c.send(interrupt)
			var sawAck, sawTerminal string
			for sawAck == "" || sawTerminal == "" {
				for _, m := range c.readUntil(map[bool]string{true: "response.incomplete", false: "response.interrupt.accepted"}[sawTerminal == ""]) {
					b, _ := json.Marshal(m)
					switch m["type"] {
					case "response.interrupt.accepted":
						sawAck = string(b)
					case "response.incomplete":
						sawTerminal = string(b)
					}
				}
			}
			c.close()

			if got, _ := gotInterrupt.Load().(string); got != interrupt {
				t.Errorf("origin received %q, want the interrupt unchanged", got)
			}
			if !strings.Contains(sawAck, `"response_id":"resp_i"`) {
				t.Errorf("client's acknowledgement %s lost the response id Codex matches on", sawAck)
			}

			var reqs, resps []pipeline.SessionEvent
			for _, e := range h.events(t, codexSID, true) {
				if e.HTTPMethod != http.MethodPost {
					continue
				}
				if e.Phase == pipeline.SessionRequest {
					reqs = append(reqs, e)
				} else {
					resps = append(resps, e)
				}
			}
			if len(reqs) != 1 || len(resps) != 1 {
				t.Fatalf("got %d turn requests and %d responses, want one of each: the interrupt and its acknowledgement are not turns", len(reqs), len(resps))
			}
			res := resps[0]
			if res.Error != nil {
				t.Errorf("an interrupted turn was recorded as an error: %+v", res.Error)
			}
			if res.Inference == nil || res.Inference.Completion != "po" || res.Inference.FinishReason != "incomplete" || res.Inference.CompletionTokens != 2 {
				t.Errorf("interrupted turn = %+v, want the partial text, finish reason incomplete and its usage", res.Inference)
			}
		})
	}
}

// A hot reload mid-turn must not move the rest of that turn onto the new pipeline. The
// turn's Finishers are the plugins whose OnRequest ran — lineage's span, a budget's
// reservation — and RunFinish dispatches them by their index in the pipeline it is
// called on, so ending the turn on a newer pipeline would finish the wrong plugins, or
// none, and leave the started ones open. The turn ends on the pipeline it began on; the
// next turn takes the new one.
func TestResponsesWebSocket_ReloadMidTurnFinishesOnTheTurnsPipeline(t *testing.T) {
	started := &outcomeRecorder{}
	h := newWSHarness(t, config.ResponsesWebSocketsParse, started)
	midTurn := make(chan struct{})
	resume := make(chan struct{})
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
		evs := turnEvents(1)
		for _, ev := range evs[:3] {
			_, _ = conn.Write(wsframe.Encode(wsframe.OpText, []byte(ev), false))
		}
		close(midTurn)
		<-resume
		for _, ev := range evs[3:] {
			_, _ = conn.Write(wsframe.Encode(wsframe.OpText, []byte(ev), false))
		}
		_, _ = fr.ReadFrame()
	})
	c := h.dial(t, nil)
	c.send(responseCreate("hello"))
	<-midTurn

	// The reload: a pipeline of different plugins, with a Finisher of its own that
	// never saw this turn begin.
	other := &outcomeRecorder{}
	fresh, err := plugintesting.BuildPipeline([]pipeline.Plugin{inferenceparser.NewInferenceParser(), &turnDenier{}, other})
	if err != nil {
		t.Fatal(err)
	}
	h.holder.Store(fresh)
	close(resume)
	c.readUntil("response.completed")
	c.close()

	if started.seen() == nil {
		t.Error("the turn's own Finisher never ran: the turn ended on a pipeline other than the one it began on")
	}
	h.events(t, codexSID, true) // the socket has ended, and with it every Finisher
	other.mu.Lock()
	got := other.got
	other.mu.Unlock()
	if got != nil {
		t.Error("the reloaded pipeline's Finisher ran for a turn it never saw begin")
	}
}

// A socket's first turn carries the whole conversation, so it can be far over the
// 10 MB per-event cap the origin's messages get. It is parsed up to the same 32 MiB an
// HTTPS request body is.
//
// The turn is just over that 10 MB, not far over it: the size is what the test is about,
// and parsing it is the costliest thing in this file — under -race it is ~3s on a laptop
// and was over 15s on a shared CI runner. The read deadline is raised for the same reason.
func TestResponsesWebSocket_LargeFirstTurnIsParsed(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsParse)
	c := h.dial(t, nil)
	_ = c.conn.SetDeadline(time.Now().Add(60 * time.Second))
	big := strings.Repeat("x", maxBodySize+1<<20)
	c.send(responseCreate(big))
	c.readUntil("response.completed")
	c.close()

	var req *pipeline.SessionEvent
	for _, e := range h.events(t, codexSID, true) {
		if e.HTTPMethod == http.MethodPost && e.Phase == pipeline.SessionRequest {
			e := e
			req = &e
		}
	}
	if req == nil || req.Inference == nil || req.Inference.Model != "gpt-6-luna" {
		t.Fatalf("an 11 MB first turn was not parsed: %+v", req)
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
