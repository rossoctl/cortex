package forwardproxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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
	got map[string]*pipeline.RequestCapture // by request id
}

func (c *captures) Record(string, *pipeline.SessionEvent) {}

func (c *captures) RecordCapture(_ string, e *pipeline.SessionEvent, rc *pipeline.RequestCapture) {
	if rc == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.got == nil {
		c.got = map[string]*pipeline.RequestCapture{}
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
	proxy, rec := proxyWithCaptures(t, inferenceparser.NewInferenceParser(), &bodyMutatorPlugin{newBody: []byte(`{"rewritten":true}`)})

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
	proxy, rec := proxyWithCaptures(t, inferenceparser.NewInferenceParser())

	postThrough(t, proxy, backend.URL+"/v1/messages", inferenceBody, nil)
	got := rec.all()
	if len(got) != 1 || string(got[0].Original) != inferenceBody || got[0].Forwarded != nil {
		t.Fatalf("captures %+v, want the original alone", got)
	}
}

// A capture is taken before the pipeline runs: a plugin that edits the request's headers
// changes what goes upstream, not what the agent is recorded as having sent.
func TestCapture_HeadersAreTheAgents(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer backend.Close()
	proxy, rec := proxyWithCaptures(t, &headerSetter{name: "X-Added", value: "by-plugin"}, inferenceparser.NewInferenceParser())

	postThrough(t, proxy, backend.URL+"/v1/messages", inferenceBody, http.Header{"X-Agent": {"yes"}})
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

// Only an inference request is captured. A buffered request no parser took for inference, such
// as an OAuth token refresh, keeps its event and no capture: its body can hold credentials,
// and the capture redacts headers and the query, never the body.
func TestCapture_OnlyInferenceRequests(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer backend.Close()
	proxy, rec := proxyWithCaptures(t, inferenceparser.NewInferenceParser())

	postThrough(t, proxy, backend.URL+"/v1/oauth/token", "grant_type=refresh_token&refresh_token=rt-secret",
		http.Header{"Content-Type": {"application/x-www-form-urlencoded"}})
	postThrough(t, proxy, backend.URL+"/oauth/token", `{"code":"c-secret","code_verifier":"v-secret"}`,
		http.Header{"Content-Type": {"application/json"}})
	// The path of an inference endpoint is not enough: the body must parse as one.
	postThrough(t, proxy, backend.URL+"/v1/messages", `{"client_secret":"s-secret"}`, nil)
	if got := rec.all(); len(got) != 0 {
		t.Fatalf("%d captures of requests that are not inference, want none: %+v", len(got), got)
	}

	postThrough(t, proxy, backend.URL+"/v1/messages", inferenceBody, nil)
	if got := rec.all(); len(got) != 1 || string(got[0].Original) != inferenceBody {
		t.Fatalf("captures %+v, want the inference request", got)
	}
}

// inferenceBody is a Messages request inference-parser takes for inference.
const inferenceBody = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
