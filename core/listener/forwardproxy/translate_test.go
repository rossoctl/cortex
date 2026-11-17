package forwardproxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferenceparser"
	"github.com/rossoctl/cortex/core/session"
)

// translator redirects every request to target and asks the listener to translate
// it from Anthropic Messages to chat completions, as the inference router does. When
// translate is false it only redirects. headers, when set, go to SetUpstreamHeaders.
// resend, when set, is the answer to each Resend, and heads keeps what it was shown.
type translator struct {
	target    string
	translate bool
	headers   http.Header
	resend    bool

	mu    sync.Mutex
	heads []string
}

func (p *translator) Name() string { return "translator" }
func (p *translator) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{WritesDestination: true, ReadsBody: true, Description: "test"}
}
func (p *translator) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	u, _ := url.Parse(p.target)
	if err := pctx.Redirect(u); err != nil {
		panic(err)
	}
	if p.translate {
		if err := pctx.Translate("anthropic-messages", "chat-completions", "/v1/chat/completions"); err != nil {
			panic(err)
		}
	}
	if p.headers != nil {
		if err := pctx.SetUpstreamHeaders(p.headers); err != nil {
			panic(err)
		}
	}
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *translator) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// resendingTranslator is a translator that is also a Resender.
type resendingTranslator struct{ *translator }

func (p resendingTranslator) Resend(_ context.Context, _ *pipeline.Context, _ int, body []byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.heads = append(p.heads, string(body))
	return p.resend
}

// upstreamRequest is one request the chat server got.
type upstreamRequest struct {
	path, rawQuery string
	header         http.Header
	body           []byte
}

// chatServer serves answer for every request and keeps what it got.
type chatServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []upstreamRequest
}

func newChatServer(t *testing.T, answer http.HandlerFunc) *chatServer {
	t.Helper()
	c := &chatServer{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.reqs = append(c.reqs, upstreamRequest{r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body})
		c.mu.Unlock()
		answer(w, r)
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *chatServer) got() []upstreamRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]upstreamRequest(nil), c.reqs...)
}

// replyFixture is a hand-written chat reply from the translate package's testdata.
func replyFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../translate/testdata/reply/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const anthropicRequest = `{"model":"claude-opus-5-5","max_tokens":1024,"messages":[{"role":"user","content":"is 391 prime?"}]}`

const anthropicStreamRequest = `{"model":"claude-opus-5-5","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"is 391 prime?"}]}`

// postAnthropic sends an Anthropic Messages request through proxy to the agent's
// provider. edit, when set, changes the request before it is sent.
func postAnthropic(t *testing.T, proxy *httptest.Server, body string, edit func(*http.Request)) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://api.anthropic.example/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	if edit != nil {
		edit(req)
	}
	resp, err := proxyClient(proxy, nil).Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func replyText(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	return string(b)
}

// translatedInvocation is the forward proxy's modify/translated record on a request
// row of store, or nil.
func translatedInvocation(store *session.Store) (*pipeline.SessionEvent, *pipeline.Invocation) {
	for _, s := range store.ListSessions() {
		v := store.View(s.ID)
		if v == nil {
			continue
		}
		for i := range v.Events {
			e := &v.Events[i]
			if e.Phase != pipeline.SessionRequest || e.Invocations == nil {
				continue
			}
			for j := range e.Invocations.Outbound {
				inv := &e.Invocations.Outbound[j]
				if inv.Plugin == "forward-proxy" && inv.Action == pipeline.ActionModify && inv.Reason == "translated" {
					return e, inv
				}
			}
		}
	}
	return nil, nil
}

func newStore(t *testing.T) *session.Store {
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	return store
}

func TestTranslate_StreamedReplyRecordedAndIncremental(t *testing.T) {
	chunks := strings.SplitAfter(replyFixture(t, "bob_shape1.sse"), "\n\n")
	release := make(chan struct{})
	up := newChatServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// The ping comment and the first two chunks, then wait.
		_, _ = io.WriteString(w, strings.Join(chunks[:3], ""))
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, strings.Join(chunks[3:], ""))
	})
	store := newStore(t)
	proxy := newRedirectProxy(t, store, nil, inferenceparser.NewInferenceParser(), &translator{target: up.URL, translate: true})

	resp := postAnthropic(t, proxy, anthropicStreamRequest, nil)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	br := bufio.NewReader(resp.Body)
	done := make(chan string, 1)
	go func() {
		for {
			line, err := br.ReadString('\n')
			if line == "event: message_start\n" {
				done <- line
				return
			}
			if err != nil {
				done <- ""
				return
			}
		}
	}()
	select {
	case got := <-done:
		if got == "" {
			t.Fatal("the reply ended with no message_start")
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("message_start did not reach the client before the upstream finished")
	}
	close(release)
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "event: message_stop") {
		t.Errorf("the reply has no message_stop: %q", rest)
	}

	// The request on the wire is chat-shaped and has no Anthropic headers.
	reqs := up.got()
	if len(reqs) != 1 || reqs[0].path != "/v1/chat/completions" {
		t.Fatalf("upstream requests = %+v, want one to /v1/chat/completions", reqs)
	}
	if reqs[0].header.Get("Anthropic-Version") != "" {
		t.Errorf("upstream saw Anthropic-Version %q", reqs[0].header.Get("Anthropic-Version"))
	}
	var chatBody struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(reqs[0].body, &chatBody); err != nil || len(chatBody.Messages) == 0 {
		t.Errorf("upstream body is not a chat request: %s", reqs[0].body)
	}

	// The response row has the Anthropic inference.
	deadline := time.Now().Add(5 * time.Second)
	var inf *pipeline.InferenceExtension
	for time.Now().Before(deadline) {
		if inf = lastInferenceResponse(t, store); inf != nil && inf.Completion != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if inf == nil || inf.Completion != "391 is not prime" {
		t.Fatalf("response inference = %+v, want completion %q", inf, "391 is not prime")
	}
	if len(inf.ToolCalls) != 1 || inf.ToolCalls[0].Name != "Bash" {
		t.Errorf("tool calls = %+v, want one Bash", inf.ToolCalls)
	}

	_, inv := translatedInvocation(store)
	if inv == nil {
		t.Fatal("no modify/translated invocation on the request row")
	}
	want := map[string]string{"from": "anthropic-messages", "to": "chat-completions", "path": "/v1/chat/completions"}
	for k, v := range want {
		if inv.Details[k] != v {
			t.Errorf("Details[%q] = %q, want %q", k, inv.Details[k], v)
		}
	}
	if _, ok := inv.Details["dropped"]; ok {
		t.Errorf("Details has dropped %q for a request that dropped nothing", inv.Details["dropped"])
	}
}

func TestTranslate_NoQueryString(t *testing.T) {
	up := newChatServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, replyFixture(t, "bob_shape1.json"))
	})
	store := newStore(t)
	proxy := newRedirectProxy(t, store, nil, &translator{target: up.URL, translate: true})
	resp := postAnthropic(t, proxy, anthropicRequest, func(r *http.Request) { r.URL.RawQuery = "beta=true" })
	_ = replyText(t, resp)

	reqs := up.got()
	if len(reqs) != 1 || reqs[0].path != "/v1/chat/completions" || reqs[0].rawQuery != "" {
		t.Fatalf("upstream requests = %+v, want /v1/chat/completions with no query", reqs)
	}
	ev, _ := translatedInvocation(store)
	if ev == nil || ev.HTTPPath != "/v1/messages" {
		t.Fatalf("request row = %+v, want path /v1/messages", ev)
	}
}

func TestTranslate_UpstreamHeadersReplace(t *testing.T) {
	up := newChatServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, replyFixture(t, "bob_shape1.json"))
	})
	proxy := newRedirectProxy(t, nil, nil, &translator{target: up.URL, translate: true,
		headers: http.Header{"User-Agent": {"bob-shell"}}})
	resp := postAnthropic(t, proxy, anthropicRequest, func(r *http.Request) { r.Header.Set("User-Agent", "claude-cli/2") })
	_ = replyText(t, resp)

	reqs := up.got()
	if len(reqs) != 1 {
		t.Fatalf("upstream got %d requests, want 1", len(reqs))
	}
	if got := reqs[0].header.Values("User-Agent"); len(got) != 1 || got[0] != "bob-shell" {
		t.Errorf("upstream User-Agent = %q, want only bob-shell", got)
	}
}

func TestTranslate_GzipUpstream(t *testing.T) {
	up := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			_, _ = io.WriteString(w, replyFixture(t, "bob_shape1.json"))
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = io.WriteString(zw, replyFixture(t, "bob_shape1.json"))
		_ = zw.Close()
	})
	proxy := newRedirectProxy(t, nil, nil, &translator{target: up.URL, translate: true})
	resp := postAnthropic(t, proxy, anthropicRequest, func(r *http.Request) { r.Header.Set("Accept-Encoding", "gzip") })
	body := replyText(t, resp)

	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want none", ce)
	}
	var msg struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil || msg.Type != "message" {
		t.Fatalf("reply is not an Anthropic message (%v): %q", err, body)
	}
	if !strings.Contains(body, "391 is not prime") {
		t.Errorf("reply has no text: %s", body)
	}
}

func TestTranslate_AgentStreamsServerAnswersJSON(t *testing.T) {
	up := newChatServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, replyFixture(t, "bob_shape1.json"))
	})
	proxy := newRedirectProxy(t, nil, nil, &translator{target: up.URL, translate: true})
	resp := postAnthropic(t, proxy, anthropicStreamRequest, nil)
	body := replyText(t, resp)

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if !strings.HasPrefix(body, "event: message_start\n") || !strings.Contains(body, "event: message_stop") {
		t.Errorf("reply is not an Anthropic stream: %q", body)
	}
}

func TestTranslate_ErrorStatus(t *testing.T) {
	up := newChatServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, replyFixture(t, "error_429_openai.json"))
	})
	proxy := newRedirectProxy(t, nil, nil, &translator{target: up.URL, translate: true})
	resp := postAnthropic(t, proxy, anthropicStreamRequest, nil)
	body := replyText(t, resp)

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil || e.Type != "error" || e.Error.Type != "rate_limit_error" {
		t.Errorf("reply = %q, want an Anthropic rate_limit_error body", body)
	}
}

func TestTranslate_DocumentRefused(t *testing.T) {
	up := newChatServer(t, func(w http.ResponseWriter, _ *http.Request) {})
	store := newStore(t)
	proxy := newRedirectProxy(t, store, nil, &translator{target: up.URL, translate: true})
	doc := `{"model":"claude-opus-5-5","max_tokens":1024,"messages":[{"role":"user","content":[` +
		`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}]}]}`
	resp := postAnthropic(t, proxy, doc, nil)
	body := replyText(t, resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(body, `"invalid_request_error"`) || !strings.Contains(body, "documents") {
		t.Errorf("reply = %q, want an invalid_request_error that names documents", body)
	}
	if n := len(up.got()); n != 0 {
		t.Errorf("upstream got %d requests, want 0", n)
	}
}

func TestTranslate_DroppedRecorded(t *testing.T) {
	up := newChatServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, replyFixture(t, "bob_shape1.json"))
	})
	store := newStore(t)
	proxy := newRedirectProxy(t, store, nil, &translator{target: up.URL, translate: true})
	req := `{"model":"claude-opus-5-5","max_tokens":1024,"top_k":5,"metadata":{"user_id":"u"},` +
		`"messages":[{"role":"user","content":"hi"}]}`
	_ = replyText(t, postAnthropic(t, proxy, req, nil))

	_, inv := translatedInvocation(store)
	if inv == nil {
		t.Fatal("no modify/translated invocation")
	}
	if got := inv.Details["dropped"]; got != "metadata,top_k" {
		t.Errorf("dropped = %q, want metadata,top_k", got)
	}
}

func TestTranslate_ResendSeesRawReply(t *testing.T) {
	var calls int
	var mu sync.Mutex
	up := newChatServer(t, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, replyFixture(t, "error_403_litellm.json"))
			return
		}
		_, _ = io.WriteString(w, replyFixture(t, "bob_shape1.json"))
	})
	tr := &translator{target: up.URL, translate: true, resend: true}
	store := newStore(t)
	proxy := newRedirectProxy(t, store, nil, resendingTranslator{tr})
	resp := postAnthropic(t, proxy, anthropicRequest, nil)
	body := replyText(t, resp)

	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "391 is not prime") {
		t.Errorf("reply = %d %q, want the second answer, translated", resp.StatusCode, body)
	}
	tr.mu.Lock()
	heads := append([]string(nil), tr.heads...)
	tr.mu.Unlock()
	if len(heads) != 1 || !strings.Contains(heads[0], "team_model_access_denied") {
		t.Errorf("Resender saw %q, want the raw 403 body", heads)
	}
	reqs := up.got()
	if len(reqs) != 2 {
		t.Fatalf("upstream got %d requests, want 2", len(reqs))
	}
	for i, r := range reqs {
		if r.path != "/v1/chat/completions" || !bytes.Contains(r.body, []byte(`"messages":[{"role":"user","content":[`)) {
			t.Errorf("upstream request %d = %s %s, want a chat body", i, r.path, r.body)
		}
	}
}

func TestTranslate_NotTranslatedPassesBytes(t *testing.T) {
	up := newChatServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	proxy := newRedirectProxy(t, nil, nil, &translator{target: up.URL})
	_ = replyText(t, postAnthropic(t, proxy, anthropicRequest, nil))

	reqs := up.got()
	if len(reqs) != 1 || string(reqs[0].body) != anthropicRequest || reqs[0].path != "/v1/messages" {
		t.Fatalf("upstream requests = %+v, want the client body unchanged", reqs)
	}
}

// Rule 8a: the capture of a translated request holds four bodies, so a conversion bug
// can be found from the archive: what the agent sent, what the plugins made of it (the
// input of the conversion), the chat body sent upstream, and the server's raw reply
// beside the Anthropic reply that the agent got.
func TestTranslate_CaptureHoldsFourBodies(t *testing.T) {
	sse := replyFixture(t, "bob_shape1.sse")
	up := newChatServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	})
	rewritten := strings.Replace(anthropicStreamRequest, "391", "393", 1)
	proxy, rec := proxyWithCaptures(t, inferenceparser.NewInferenceParser(),
		&translator{target: up.URL, translate: true}, &bodyMutatorPlugin{newBody: []byte(rewritten)})

	resp := postAnthropic(t, proxy, anthropicStreamRequest, nil)
	reply := replyText(t, resp)
	if !strings.Contains(reply, "event: message_stop") {
		t.Fatalf("reply = %q, want an Anthropic stream", reply)
	}

	res := rec.responses()
	reqs := rec.all()
	if len(reqs) != 1 || len(res) != 1 {
		t.Fatalf("%d request and %d response captures, want 1 and 1", len(reqs), len(res))
	}
	c := reqs[0]
	if string(c.Original) != anthropicStreamRequest {
		t.Errorf("Original = %q, want the agent's request", c.Original)
	}
	if string(c.Forwarded) != rewritten {
		t.Errorf("Forwarded = %q, want the plugin's body", c.Forwarded)
	}
	sent := up.got()
	if len(sent) != 1 || !bytes.Equal(c.Translated, sent[0].body) || !strings.Contains(string(c.Translated), "is 393 prime") {
		t.Errorf("Translated = %q, want the chat body sent upstream", c.Translated)
	}
	for _, r := range res {
		if string(r.Body) != reply {
			t.Errorf("Response.Body = %q, want the Anthropic reply the agent got", r.Body)
		}
		if string(r.Upstream) != sse || r.UpstreamTruncated {
			t.Errorf("Response.Upstream = %q (truncated %v), want the server's chat stream", r.Upstream, r.UpstreamTruncated)
		}
	}
}
