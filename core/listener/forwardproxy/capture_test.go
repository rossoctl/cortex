package forwardproxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferenceparser"
	"github.com/rossoctl/cortex/core/session"
)

// captures is a session.CaptureRecorder that keeps what it is handed.
type captures struct {
	mu  sync.Mutex
	got map[string]*pipeline.RequestCapture  // request captures, by request id
	res map[string]*pipeline.ResponseCapture // response captures, by request id
}

func (c *captures) Record(string, *pipeline.SessionEvent) {}

func (c *captures) RecordCapture(_ string, e *pipeline.SessionEvent, rc *pipeline.RequestCapture) {
	if rc == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.got == nil {
		c.got, c.res = map[string]*pipeline.RequestCapture{}, map[string]*pipeline.ResponseCapture{}
	}
	if rc.Response != nil {
		c.res[e.RequestID] = rc.Response
		return
	}
	c.got[e.RequestID] = rc
}

func (c *captures) all() []*pipeline.RequestCapture {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*pipeline.RequestCapture
	for _, rc := range c.got {
		out = append(out, rc)
	}
	return out
}

// proxyWithCaptures is a forward proxy running plugins, whose store hands captures to the
// returned recorder.
func proxyWithCaptures(t *testing.T, plugins ...pipeline.Plugin) (*httptest.Server, *captures) {
	t.Helper()
	p, err := pipeline.New(plugins)
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	rec := &captures{}
	store.AddRecorder(rec)
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Sessions: store, Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	return proxy, rec
}

func postThrough(t *testing.T, proxy *httptest.Server, url, body string, header http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	for k, vs := range header {
		req.Header[k] = vs
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// A request whose body a plugin rewrote is captured as both: the bytes the agent sent and the
// bytes the origin got. Its headers come without credentials, and its query comes whole.
func TestCapture_RecordsOriginalForwardedHeadersAndQuery(t *testing.T) {
	var forwarded []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = readAllBody(r)
	}))
	defer backend.Close()
	proxy, rec := proxyWithCaptures(t, &bodyMutatorPlugin{newBody: []byte(`{"rewritten":true}`)})

	orig := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	postThrough(t, proxy, backend.URL+"/v1/messages?beta=true&key=AIza-secret", orig, http.Header{
		"Anthropic-Beta": {"a", "b"},
		"Authorization":  {"Bearer secret"},
		"X-Api-Key":      {"sk-secret"},
		"Cookie":         {"c=secret"},
	})

	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("%d captures, want 1", len(got))
	}
	c := got[0]
	if string(c.Original) != orig {
		t.Errorf("original = %q, want what the agent sent", c.Original)
	}
	if !bytes.Equal(c.Forwarded, forwarded) || string(c.Forwarded) != `{"rewritten":true}` {
		t.Errorf("forwarded = %q, want what the origin got (%q)", c.Forwarded, forwarded)
	}
	if c.Query != "beta=true" {
		t.Errorf("query = %q, want beta=true without the key", c.Query)
	}
	if v := c.Header.Values("Anthropic-Beta"); len(v) != 2 {
		t.Errorf("Anthropic-Beta = %v, want both values", v)
	}
	for _, h := range []string{"Authorization", "X-Api-Key", "Cookie"} {
		if c.Header.Get(h) != "" {
			t.Errorf("%s was captured: %q", h, c.Header.Get(h))
		}
	}
}

// A body no plugin rewrote is captured once, as the original: nil Forwarded says the origin got
// the same bytes.
func TestCapture_AnUnrewrittenBodyHasNoForwardedCopy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer backend.Close()
	proxy, rec := proxyWithCaptures(t, &bodyRecorderPlugin{})

	postThrough(t, proxy, backend.URL+"/v1/messages", `{"a":1}`, nil)
	got := rec.all()
	if len(got) != 1 || string(got[0].Original) != `{"a":1}` || got[0].Forwarded != nil {
		t.Fatalf("captures %+v, want the original alone", got)
	}
}

// A capture is taken before the pipeline runs: a plugin that edits the request's headers
// changes what goes upstream, not what the agent is recorded as having sent.
func TestCapture_HeadersAreTheAgents(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer backend.Close()
	proxy, rec := proxyWithCaptures(t, &headerSetter{name: "X-Added", value: "by-plugin"}, &bodyRecorderPlugin{})

	postThrough(t, proxy, backend.URL+"/v1/messages", `{"a":1}`, http.Header{"X-Agent": {"yes"}})
	got := rec.all()
	if len(got) != 1 || got[0].Header.Get("X-Agent") != "yes" || got[0].Header.Get("X-Added") != "" {
		t.Fatalf("captured headers %v, want the agent's", got[0].Header)
	}
}

type headerSetter struct{ name, value string }

func (p *headerSetter) Name() string { return "header-setter" }
func (p *headerSetter) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *headerSetter) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.Headers.Set(p.name, p.value)
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *headerSetter) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func readAllBody(r *http.Request) []byte {
	var b bytes.Buffer
	b.ReadFrom(r.Body)
	return b.Bytes()
}

// A Responses WebSocket turn is captured as the message the agent sent, byte for byte, and as
// the message the origin got when a plugin rewrote it. Its headers are the handshake's, without
// credentials.
func TestCapture_WebSocketTurn(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsParse, &modelRewriter{})
	rec := &captures{}
	h.store.AddRecorder(rec)
	c := h.dial(t, http.Header{"Authorization": {"Bearer secret"}, "X-Agent": {"codex"}})
	if c.status != http.StatusSwitchingProtocols {
		t.Fatalf("handshake answered %d", c.status)
	}
	sent := responseCreate("hello")
	c.send(sent)
	c.readUntil("response.completed")
	c.close()
	h.events(t, codexSID, true)

	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("%d captures, want 1", len(got))
	}
	if string(got[0].Original) != sent {
		t.Errorf("original = %s, want the message as sent", got[0].Original)
	}
	if !strings.Contains(string(got[0].Forwarded), "rewritten-model") || !strings.Contains(string(got[0].Forwarded), `"response.create"`) {
		t.Errorf("forwarded = %s, want the rewritten response.create", got[0].Forwarded)
	}
	if got[0].Header.Get("X-Agent") != "codex" || got[0].Header.Get("Authorization") != "" {
		t.Errorf("headers = %v, want the handshake's without credentials", got[0].Header)
	}
}

// A request a Resender sends again is captured again, under the new attempt's row: the agent's
// bytes once more, and the body the second attempt carried.
func TestCapture_AResentRequestCarriesItsOwnForwardedBody(t *testing.T) {
	upstream := &modelServer{served: "glm-5-3"}
	backend := httptest.NewServer(upstream)
	defer backend.Close()
	proxy, rec := proxyWithCaptures(t, &substituter{target: backend.URL, sub: "glm-5-3", resend: true})

	sent := `{"model":"exo-free","messages":[]}`
	postThrough(t, proxy, "http://agent-provider.example/v1/chat/completions", sent, nil)
	got := rec.all()
	if len(got) != 2 {
		t.Fatalf("%d captures, want one per attempt", len(got))
	}
	var resent int
	for _, c := range got {
		if string(c.Original) != sent {
			t.Errorf("original = %s, want the agent's body", c.Original)
		}
		switch {
		case c.Forwarded == nil:
		case strings.Contains(string(c.Forwarded), `"glm-5-3"`):
			resent++
		default:
			t.Errorf("forwarded = %s, want nil or the resent body", c.Forwarded)
		}
	}
	// The first attempt went as the agent sent it. A capture shared with the second attempt
	// would show the second attempt's body on both rows.
	if resent != 1 {
		t.Errorf("%d captures carry the resent body, want 1", resent)
	}
}

// responses is the response captures, keyed by request id. It waits a little for the first: the
// proxy records a response's row after its last byte reaches the client, so a client can read
// the whole body before the row exists.
func (c *captures) responses() map[string]*pipeline.ResponseCapture {
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		c.mu.Lock()
		out := map[string]*pipeline.ResponseCapture{}
		for id, r := range c.res {
			out[id] = r
		}
		c.mu.Unlock()
		if len(out) > 0 || time.Now().After(deadline) {
			return out
		}
	}
}

// A response is captured as the origin sent it, on every arm the proxy relays it through:
// streamed with a parser, streamed through, buffered, and copied straight through. It pairs with
// its request by request id.
func TestCapture_ResponseOnEveryArm(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ctype   string
		plugins []pipeline.Plugin
	}{
		{"streamed with a parser", "text/event-stream", []pipeline.Plugin{inferenceparser.NewInferenceParser()}},
		{"streamed through", "text/event-stream", []pipeline.Plugin{&bodyRecorderPlugin{}}},
		{"buffered", "application/json", []pipeline.Plugin{inferenceparser.NewInferenceParser()}},
		{"copied through", "application/json", []pipeline.Plugin{&bodyMutatorPlugin{newBody: []byte(`{}`)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := anthropicSSEBody
			if tc.ctype == "application/json" {
				body = `{"type":"message","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":1}}`
			}
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.ctype)
				w.Header().Set("Set-Cookie", "s=secret")
				w.Header().Set("X-Request-Id", "req_1")
				w.WriteHeader(http.StatusCreated)
				io.WriteString(w, body)
			}))
			defer backend.Close()
			proxy, rec := proxyWithCaptures(t, tc.plugins...)
			postThrough(t, proxy, backend.URL+"/v1/messages", `{"model":"m","stream":true,"messages":[]}`, nil)

			got := rec.responses()
			if len(got) != 1 {
				t.Fatalf("%d response captures, want 1", len(got))
			}
			for id, r := range got {
				rec.mu.Lock()
				paired := rec.got[id] != nil
				rec.mu.Unlock()
				if !paired || r == nil {
					t.Fatalf("response capture %s does not pair with a request capture", id)
				}
				if r.Status != http.StatusCreated || string(r.Body) != body || r.Truncated || r.Incomplete {
					t.Fatalf("response %d %q truncated=%v incomplete=%v, want 201 and the body", r.Status, r.Body, r.Truncated, r.Incomplete)
				}
				if r.Header.Get("X-Request-Id") != "req_1" || r.Header.Get("Set-Cookie") != "" {
					t.Fatalf("response headers %v, want the origin's without credentials", r.Header)
				}
			}
		})
	}
}

// A body past the cap is kept up to the cap and marked truncated; the client still gets it all.
func TestCapture_ResponsePastTheCapIsTruncated(t *testing.T) {
	big := strings.Repeat("x", pipeline.MaxResponseCapture+100)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, big)
	}))
	defer backend.Close()
	proxy, rec := proxyWithCaptures(t, &bodyMutatorPlugin{newBody: []byte(`{}`)})
	postThrough(t, proxy, backend.URL+"/v1/messages", `{}`, nil)
	for _, r := range rec.responses() {
		if !r.Truncated || len(r.Body) != pipeline.MaxResponseCapture {
			t.Fatalf("truncated=%v len=%d, want the first %d bytes, truncated", r.Truncated, len(r.Body), pipeline.MaxResponseCapture)
		}
		return
	}
	t.Fatal("no response capture")
}

// A stream the origin breaks off is captured as far as it got, and marked incomplete.
func TestCapture_BrokenStreamIsIncomplete(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer backend.Close()
	proxy, rec := proxyWithCaptures(t, inferenceparser.NewInferenceParser())
	postThrough(t, proxy, backend.URL+"/v1/messages", `{"model":"m","stream":true,"messages":[]}`, nil)
	for _, r := range rec.responses() {
		if !r.Incomplete || !strings.HasPrefix(string(r.Body), "event: message_start") {
			t.Fatalf("incomplete=%v body=%q, want the part that arrived, marked incomplete", r.Incomplete, r.Body)
		}
		return
	}
	t.Fatal("no response capture")
}

// A WebSocket turn's response is the origin's messages for the turn, one per line, as they came.
func TestCapture_WebSocketTurnResponse(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsParse)
	rec := &captures{}
	h.store.AddRecorder(rec)
	c := h.dial(t, nil)
	c.send(responseCreate("hello"))
	c.readUntil("response.completed")
	c.close()
	h.events(t, codexSID, true)

	got := rec.responses()
	if len(got) != 1 {
		t.Fatalf("%d response captures, want 1", len(got))
	}
	for _, r := range got {
		lines := strings.Split(strings.TrimSuffix(string(r.Body), "\n"), "\n")
		if r.Status != http.StatusOK || len(lines) < 2 || !strings.Contains(lines[len(lines)-1], `"response.completed"`) {
			t.Fatalf("status %d, body %q: want the turn's messages ending in response.completed", r.Status, r.Body)
		}
	}
}

// rejecter refuses every response.
type rejecter struct{}

func (rejecter) Name() string { return "rejecter" }
func (rejecter) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{ReadsBody: true}
}
func (rejecter) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (rejecter) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	// A real refusing plugin records its decision, which is what makes the deny row.
	pctx.Record(pipeline.Invocation{Plugin: "rejecter", Action: pipeline.ActionDeny, Phase: pipeline.InvocationPhaseResponse})
	return pipeline.Action{Type: pipeline.Reject, Violation: &pipeline.Violation{Code: "no", Reason: "no", Status: 403}}
}

// A response a plugin refused is still the origin's answer, and is captured as the origin sent it.
func TestCapture_ARefusedResponseIsCaptured(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"answer":"the origin's"}`)
	}))
	defer backend.Close()
	proxy, rec := proxyWithCaptures(t, rejecter{})
	postThrough(t, proxy, backend.URL+"/v1/messages", `{}`, nil)
	got := rec.responses()
	if len(got) != 1 {
		t.Fatalf("%d response captures, want 1", len(got))
	}
	for _, r := range got {
		if string(r.Body) != `{"answer":"the origin's"}` || r.Incomplete {
			t.Fatalf("body %q incomplete=%v, want the origin's whole answer", r.Body, r.Incomplete)
		}
	}
}

// A body the proxy stopped reading before its end is incomplete, whatever the row's error says.
func TestCapture_AStreamEndedEarlyIsIncomplete(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		for range 2000 {
			io.WriteString(w, "data: "+strings.Repeat("x", 1000)+"\n\n")
		}
	}))
	defer backend.Close()
	proxy, rec := proxyWithCaptures(t, inferenceparser.NewInferenceParser())
	req, _ := http.NewRequest(http.MethodPost, backend.URL+"/v1/messages", strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	resp.Body.Read(buf)
	resp.Body.Close() // the client goes away mid-stream
	for _, r := range rec.responses() {
		if !r.Incomplete {
			t.Fatalf("a stream read %d bytes of ~2 MB is not marked incomplete", len(r.Body))
		}
		return
	}
	t.Fatal("no response capture")
}

// A body the transport decoded is not the wire's: the capture says so, rather than keeping a
// Content-Encoding header that no longer describes its bytes.
func TestCapture_ADecodedBodySaysSo(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		io.WriteString(zw, anthropicSSEBody)
		zw.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(buf.Bytes())
	}))
	defer backend.Close()
	proxy, rec := proxyWithCaptures(t, inferenceparser.NewInferenceParser())
	postThrough(t, proxy, backend.URL+"/v1/messages", `{"model":"m","stream":true,"messages":[]}`,
		http.Header{"Accept-Encoding": {"gzip"}})
	for _, r := range rec.responses() {
		if string(r.Body) != anthropicSSEBody || !r.Decoded {
			t.Fatalf("decoded=%v body %q, want the decoded stream, marked decoded", r.Decoded, r.Body)
		}
		return
	}
	t.Fatal("no response capture")
}

// A WebSocket turn's capture carries the status its row records.
func TestCapture_WebSocketErrorTurnKeepsItsStatus(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsParse)
	h.origin.mu.Lock()
	h.origin.answer = func(int) []string {
		return []string{`{"type":"error","status":429,"error":{"type":"usage_limit_reached","code":"rate_limit_exceeded"}}`}
	}
	h.origin.mu.Unlock()
	rec := &captures{}
	h.store.AddRecorder(rec)
	c := h.dial(t, nil)
	c.send(responseCreate("hello"))
	c.readUntil("error")
	c.close()
	h.events(t, codexSID, true)
	for _, r := range rec.responses() {
		if r.Status != http.StatusTooManyRequests {
			t.Fatalf("status %d, want 429", r.Status)
		}
		return
	}
	t.Fatal("no response capture")
}

// A resent request is two exchanges, and each is captured whole: the refused attempt's request
// and the refusal, then the resent request — with the body the plugin rewrote — and its answer.
func TestCapture_ResendCapturesBothAttempts(t *testing.T) {
	upstream := &modelServer{served: "glm-5-3"}
	backend := httptest.NewServer(upstream)
	defer backend.Close()
	proxy, rec := proxyWithCaptures(t, &substituter{target: backend.URL, sub: "glm-5-3", resend: true})
	postThrough(t, proxy, "http://agent-provider.example/v1/chat/completions",
		`{"model":"exo-free","messages":[{"role":"user","content":"hi"}]}`, nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec.mu.Lock()
		n, m := len(rec.got), len(rec.res)
		rec.mu.Unlock()
		if (n == 2 && m == 2) || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.got) != 2 || len(rec.res) != 2 {
		t.Fatalf("%d request and %d response captures, want 2 of each", len(rec.got), len(rec.res))
	}
	var statuses []int
	for id, r := range rec.res {
		if rec.got[id] == nil {
			t.Fatalf("response capture %s pairs with no request capture", id)
		}
		statuses = append(statuses, r.Status)
		if r.Status == http.StatusForbidden && !strings.Contains(string(r.Body), "team_model_access_denied") {
			t.Errorf("the refusal's capture is %q, want the origin's refusal", r.Body)
		}
		if r.Status == http.StatusOK && !strings.Contains(string(rec.got[id].Forwarded), "glm-5-3") {
			t.Errorf("the resent request's forwarded body is %q, want the substitute model", rec.got[id].Forwarded)
		}
	}
	slices.Sort(statuses)
	if !slices.Equal(statuses, []int{http.StatusOK, http.StatusForbidden}) {
		t.Fatalf("response statuses %v, want a 403 then a 200", statuses)
	}
}

// A refusal the origin broke off before its end is incomplete on the resend arm too: the resend
// read only part of it, and a short read is not the end of the body.
func TestCapture_ABrokenRefusalBeforeAResendIsIncomplete(t *testing.T) {
	refusals := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, _ := io.ReadAll(r.Body); strings.Contains(string(body), `"glm-5-3"`) {
			io.WriteString(w, `{"choices":[{"message":{"content":"PONG"}}]}`)
			return
		}
		refusals++
		w.Header().Set("Content-Length", "5000")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"type":"team_model_access_denied"`)
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer backend.Close()
	proxy, rec := proxyWithCaptures(t, &substituter{target: backend.URL, sub: "glm-5-3", resend: true})
	postThrough(t, proxy, "http://agent-provider.example/v1/chat/completions", `{"model":"exo-free","messages":[]}`, nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec.mu.Lock()
		n := len(rec.res)
		rec.mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var refused *pipeline.ResponseCapture
	for _, r := range rec.res {
		if r.Status == http.StatusForbidden {
			refused = r
		}
	}
	if refusals != 1 || refused == nil {
		t.Fatalf("%d refusals, refused capture %v: want one cut-off 403 captured", refusals, refused)
	}
	if !refused.Incomplete {
		t.Fatalf("a 403 cut off after %d of 5000 bytes is marked complete", len(refused.Body))
	}
}
