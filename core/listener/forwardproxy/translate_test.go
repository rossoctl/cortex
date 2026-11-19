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
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferenceparser"
	"github.com/rossoctl/cortex/core/session"
)

// translator redirects every request to target and asks the listener to translate
// it from source (Anthropic Messages when empty) to chat completions, as the
// inference router does. When translate is false it only redirects. headers, when set, go to SetUpstreamHeaders.
// resend, when set, is the answer to each Resend, and heads keeps what it was shown.
type translator struct {
	target    string
	source    string
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
		src := p.source
		if src == "" {
			src = "anthropic-messages"
		}
		if err := pctx.Translate(src, "chat-completions", "/v1/chat/completions"); err != nil {
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

// resendingTranslator is a translator that is also a Resender. When substitute is set,
// each Resend sets the request's model to it. When body is set, each Resend then sets
// the request body to it.
type resendingTranslator struct {
	*translator
	substitute string
	body       string
}

func (p resendingTranslator) Capabilities() pipeline.PluginCapabilities {
	c := p.translator.Capabilities()
	c.WritesRequestBody = p.substitute != "" || p.body != ""
	return c
}

func (p resendingTranslator) Resend(_ context.Context, pctx *pipeline.Context, _ int, body []byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.heads = append(p.heads, string(body))
	if p.substitute != "" {
		if err := pctx.SetRequestModel(p.substitute); err != nil {
			panic(err)
		}
	}
	if p.body != "" {
		pctx.SetBody([]byte(p.body))
	}
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

// outboundRows are store's outbound rows of phase, in the order they were recorded.
func outboundRows(store *session.Store, phase pipeline.SessionPhase) []pipeline.SessionEvent {
	var out []pipeline.SessionEvent
	for _, s := range store.ListSessions() {
		v := store.View(s.ID)
		if v == nil {
			continue
		}
		for _, e := range v.Events {
			if e.Direction == pipeline.Outbound && e.Phase == phase {
				out = append(out, e)
			}
		}
	}
	return out
}

// waitRows waits for n outbound rows of phase: the last response row can be recorded
// after the client has read the reply.
func waitRows(t *testing.T, store *session.Store, phase pipeline.SessionPhase, n int) []pipeline.SessionEvent {
	t.Helper()
	eventually(t, store, func() bool { return len(outboundRows(store, phase)) >= n }, "the outbound rows")
	return outboundRows(store, phase)
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
	// The upstream holds the rest of its reply, so message_start arrives only if it is
	// relayed before the upstream finishes.
	if got := <-done; got == "" {
		close(release)
		t.Fatal("the reply ended with no message_start")
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
	var inf *pipeline.InferenceExtension
	eventually(t, store, func() bool {
		inf = lastInferenceResponse(t, store)
		return inf != nil && inf.Completion != ""
	}, "the response row's inference")
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
	denied := waitRows(t, store, pipeline.SessionDenied, 1)
	if len(denied) != 1 {
		t.Fatalf("%d denied rows, want 1", len(denied))
	}
	d := denied[0]
	if d.StatusCode != http.StatusBadRequest || d.Error == nil || d.Error.Code != "inference.translation_refused" {
		t.Errorf("denied row = status %d error %+v, want 400 inference.translation_refused", d.StatusCode, d.Error)
	}
	var found bool
	if d.Invocations != nil {
		for _, inv := range d.Invocations.Outbound {
			found = found || (inv.Plugin == "forward-proxy" && inv.Action == pipeline.ActionDeny && inv.Reason == "translation_refused")
		}
	}
	if !found {
		t.Errorf("denied row invocations = %+v, want forward-proxy deny/translation_refused", d.Invocations)
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

// refusedThenAnswered serves a LiteLLM 403 to the first request and the chat reply to
// every later one.
func refusedThenAnswered(t *testing.T) *chatServer {
	var calls int
	var mu sync.Mutex
	return newChatServer(t, func(w http.ResponseWriter, _ *http.Request) {
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
}

func TestTranslate_ResendSeesRawReply(t *testing.T) {
	up := refusedThenAnswered(t)
	tr := &translator{target: up.URL, translate: true, resend: true, headers: http.Header{"User-Agent": {"bob-shell"}}}
	store := newStore(t)
	rec := &captures{}
	store.AddRecorder(rec)
	proxy := newRedirectProxy(t, store, nil, inferenceparser.NewInferenceParser(), resendingTranslator{translator: tr, substitute: "claude-sonnet-5"})
	resp := postAnthropic(t, proxy, anthropicRequest, func(r *http.Request) { r.Header.Set("User-Agent", "claude-cli/2") })
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
		// The upstream headers go on the resent request too, after the header sync.
		if got := r.header.Values("User-Agent"); len(got) != 1 || got[0] != "bob-shell" {
			t.Errorf("upstream request %d User-Agent = %q, want only bob-shell", i, got)
		}
	}
	// Each attempt's row holds the chat body that attempt sent: the refused row the
	// first model, the resent row the substitute.
	caps := rec.all()
	if len(caps) != 2 {
		t.Fatalf("%d request captures, want 2", len(caps))
	}
	var translated []string
	for _, c := range caps {
		translated = append(translated, string(c.Translated))
	}
	sort.Strings(translated)
	want := []string{string(reqs[0].body), string(reqs[1].body)}
	sort.Strings(want)
	if !slices.Equal(translated, want) || want[0] == want[1] {
		t.Errorf("Translated = %q, want the two chat bodies sent upstream, %q", translated, want)
	}
	if !bytes.Contains(reqs[1].body, []byte(`"model":"claude-sonnet-5"`)) {
		t.Errorf("resent body = %s, want the substitute model", reqs[1].body)
	}
	// Decision 19: the refused attempt's row has the refusal in the agent's protocol.
	// The raw body's type is team_model_access_denied; Anthropic's for a 403 is
	// permission_error.
	rows := waitRows(t, store, pipeline.SessionResponse, 2)
	if len(rows) != 2 {
		t.Fatalf("%d response rows, want 2", len(rows))
	}
	refused := rows[0]
	if refused.StatusCode != http.StatusForbidden || refused.Error == nil || refused.Error.Message != "permission_error" {
		t.Errorf("refused row = status %d error %+v, want 403 with permission_error", refused.StatusCode, refused.Error)
	}
	if refused.Inference == nil || refused.Inference.Model != "claude-opus-5-5" {
		t.Errorf("refused row inference = %+v, want the refused model claude-opus-5-5", refused.Inference)
	}
	if rows[1].StatusCode != http.StatusOK {
		t.Errorf("resent row status = %d, want 200", rows[1].StatusCode)
	}
}

// A Resender asks for a resend, but its change makes a request that cannot be
// converted. The refused answer then goes to the agent, translated, and nothing is
// sent again.
func TestTranslate_ResendNotConvertible(t *testing.T) {
	up := refusedThenAnswered(t)
	doc := `{"model":"claude-sonnet-5","max_tokens":1024,"messages":[{"role":"user","content":[` +
		`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}]}]}`
	tr := &translator{target: up.URL, translate: true, resend: true}
	store := newStore(t)
	proxy := newRedirectProxy(t, store, nil, inferenceparser.NewInferenceParser(),
		resendingTranslator{translator: tr, substitute: "claude-sonnet-5", body: doc})
	resp := postAnthropic(t, proxy, anthropicRequest, nil)
	body := replyText(t, resp)

	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, `"permission_error"`) {
		t.Errorf("reply = %d %q, want the 403 as an Anthropic permission_error", resp.StatusCode, body)
	}
	if n := len(up.got()); n != 1 {
		t.Errorf("upstream got %d requests, want 1", n)
	}
	rows := waitRows(t, store, pipeline.SessionResponse, 1)
	if len(rows) != 1 {
		t.Fatalf("%d response rows, want 1", len(rows))
	}
	// The row names the model the upstream refused, not the Resender's change.
	if r := rows[0]; r.StatusCode != http.StatusForbidden || r.Inference == nil || r.Inference.Model != "claude-opus-5-5" {
		t.Errorf("response row = status %d inference %+v, want 403 for claude-opus-5-5", r.StatusCode, r.Inference)
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

func TestTranslate_NotStreamedErrorStatus(t *testing.T) {
	for _, c := range []struct {
		name, body string
		want       int
	}{
		{"error body at 200", replyFixture(t, "chunk_error.json"), http.StatusInternalServerError},
		{"not JSON", "not json", http.StatusBadGateway},
		// Valid JSON: only the limit makes it an error.
		{"over the limit", replyFixture(t, "bob_shape1.json") + strings.Repeat(" ", maxBodySize), http.StatusBadGateway},
	} {
		t.Run(c.name, func(t *testing.T) {
			up := newChatServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, c.body)
			})
			store := newStore(t)
			proxy := newRedirectProxy(t, store, nil, &translator{target: up.URL, translate: true})
			resp := postAnthropic(t, proxy, anthropicRequest, nil)
			body := replyText(t, resp)

			if resp.StatusCode != c.want || !strings.HasPrefix(body, `{"type":"error"`) {
				t.Errorf("reply = %d %q, want %d with an Anthropic error body", resp.StatusCode, body, c.want)
			}
			if rows := waitRows(t, store, pipeline.SessionResponse, 1); rows[0].StatusCode != c.want {
				t.Errorf("response row status = %d, want %d", rows[0].StatusCode, c.want)
			}
		})
	}
}

func TestTranslate_EmptyReplyCapturesUpstream(t *testing.T) {
	up := newChatServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
	})
	proxy, rec := proxyWithCaptures(t, inferenceparser.NewInferenceParser(), &translator{target: up.URL, translate: true})
	_ = replyText(t, postAnthropic(t, proxy, anthropicStreamRequest, nil))

	res := rec.responses()
	if len(res) != 1 {
		t.Fatalf("%d response captures, want 1", len(res))
	}
	for _, r := range res {
		if r.Upstream == nil {
			t.Errorf("Upstream is nil, want an empty capture of the server's reply")
		}
	}
}

// codexHeaders are headers that Codex sends and that only an OpenAI server reads
// (rule R3).
var codexHeaders = []string{"OpenAI-Beta", "Originator", "ChatGPT-Account-Id", "X-Codex-Turn"}

// postCodex sends a Codex-shaped Responses request through proxy to chatgpt.com.
func postCodex(t *testing.T, proxy *httptest.Server, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://chatgpt.example/backend-api/codex/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex-tui/0.162.1 (Mac OS 26.3.0; arm64) xterm-256color (codex-tui; 0.162.1)")
	for _, h := range codexHeaders {
		req.Header.Set(h, "v")
	}
	resp, err := proxyClient(proxy, nil).Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

const codexRequest = `{"model":"gpt-5","instructions":"be brief","stream":true,"store":false,` +
	`"tools":[{"type":"function","name":"Bash","description":"run","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}],` +
	`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"is 391 prime?"}]}]}`

// Rules R1, R3 and R11 end to end: a Codex request to a Chat server.
func TestTranslate_CodexToChat(t *testing.T) {
	// bob_shape1 is text and one Bash call. Its usage line is replaced with one that
	// carries cached_tokens and cache_creation_input_tokens.
	var sse []string
	for _, chunk := range strings.SplitAfter(replyFixture(t, "bob_shape1.sse"), "\n\n") {
		if strings.Contains(chunk, `"usage"`) {
			chunk = `data: {"id":"chatcmpl-b1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":450,"completion_tokens":20,` +
				`"total_tokens":470,"cache_creation_input_tokens":50,"prompt_tokens_details":{"cached_tokens":300}}}` + "\n\n"
		}
		sse = append(sse, chunk)
	}
	up := newChatServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join(sse, ""))
	})
	store := newStore(t)
	proxy := newRedirectProxy(t, store, nil, inferenceparser.NewInferenceParser(),
		&translator{target: up.URL, source: "responses", translate: true})

	resp := postCodex(t, proxy, codexRequest)
	body := replyText(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	created, completed := strings.Index(body, "event: response.created\n"), strings.Index(body, "event: response.completed\n")
	if created != 0 || completed < 0 {
		t.Errorf("reply does not go from response.created to response.completed:\n%s", body)
	}

	reqs := up.got()
	if len(reqs) != 1 || reqs[0].path != "/v1/chat/completions" {
		t.Fatalf("upstream requests = %+v, want one to /v1/chat/completions", reqs)
	}
	for _, h := range codexHeaders {
		if v := reqs[0].header.Values(h); len(v) != 0 {
			t.Errorf("upstream saw %s: %q", h, v)
		}
	}
	var chatBody struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(reqs[0].body, &chatBody); err != nil || len(chatBody.Messages) != 2 ||
		chatBody.Messages[0].Role != "system" || chatBody.Messages[1].Role != "user" || len(chatBody.Tools) != 1 {
		t.Errorf("upstream body is not the chat request: %s", reqs[0].body)
	}

	var inf *pipeline.InferenceExtension
	eventually(t, store, func() bool {
		inf = lastInferenceResponse(t, store)
		return inf != nil && inf.OutputTokens != 0
	}, "the response row's inference")
	if inf == nil {
		t.Fatal("no response inference")
	}
	if inf.Completion != "391 is not prime" || len(inf.ToolCalls) != 1 || inf.ToolCalls[0].Name != "Bash" {
		t.Errorf("response inference: completion %q, tool calls %+v; want the text and one Bash", inf.Completion, inf.ToolCalls)
	}
	if inf.InputTokens != 100 || inf.CacheReadTokens != 300 || inf.CacheWriteTokens != 50 || inf.OutputTokens != 20 {
		t.Errorf("tokens: input %d, cache read %d, cache write %d, output %d; want 100, 300, 50, 20",
			inf.InputTokens, inf.CacheReadTokens, inf.CacheWriteTokens, inf.OutputTokens)
	}
}

// assertCodexRefused checks a rule R5 or R10 refusal: 400, the OpenAI error body
// with the exact text, and nothing upstream.
func assertCodexRefused(t *testing.T, req, msg string) {
	t.Helper()
	up := newChatServer(t, func(w http.ResponseWriter, _ *http.Request) {})
	proxy := newRedirectProxy(t, newStore(t), nil, &translator{target: up.URL, source: "responses", translate: true})
	resp := postCodex(t, proxy, req)
	body := replyText(t, resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	want, _ := json.Marshal(msg)
	if body != `{"error":{"message":`+string(want)+`,"type":"invalid_request_error","code":null}}` {
		t.Errorf("reply = %s", body)
	}
	if n := len(up.got()); n != 0 {
		t.Errorf("upstream got %d requests, want 0", n)
	}
}

func TestTranslate_CodexStateRefused(t *testing.T) {
	assertCodexRefused(t, `{"model":"gpt-5","previous_response_id":"resp_1","input":[]}`,
		"Cortex cannot translate a Responses request that refers to server state (previous_response_id). Send the full conversation.")
}

func TestTranslate_CodexCompactRefused(t *testing.T) {
	assertCodexRefused(t, `{"model":"gpt-5","input":[{"type":"message","role":"user","content":"hi"},{"type":"compaction_trigger"}]}`,
		"Cortex cannot compact a Codex conversation on this server. Start a new Codex session, or move this agent to an OpenAI server with agentop server use.")
}
