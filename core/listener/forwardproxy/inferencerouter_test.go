package forwardproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/memstore"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferenceparser"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/storage/filestore"
)

// seen is every request an origin received, by method and Authorization header —
// HEADs included, unlike origin.gets, so a key on any request is caught.
type seen struct {
	mu    sync.Mutex
	calls []string
}

func (s *seen) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls = append(s.calls, r.Method+" "+r.Header.Get("Authorization"))
		s.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (s *seen) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// recordingOrigin is newOrigin with every request's method and Authorization kept.
func recordingOrigin(t *testing.T, name string) (*origin, *seen) {
	t.Helper()
	s := &seen{}
	o := newOrigin(t, name, func(srv *httptest.Server) {
		srv.Config.Handler = s.record(srv.Config.Handler)
		srv.StartTLS()
	})
	return o, s
}

// A client that CONNECTs to one host and names an inference server's host inside the
// bridged TLS must not get that server's key sent to the host it CONNECTed to. The
// router decides on the Host header, which on a bridged request is the client's word,
// while the forward proxy dials the CONNECT authority unless a redirect says
// otherwise. The listener now refuses such a request with a 421 before any plugin
// runs, so the key goes nowhere; the router's own redirect is the second line, which
// TestConnectBridge_RedirectSendsTheDecryptedRequestToTheTarget still covers.
func TestConnectBridge_TheRoutersKeyGoesOnlyToItsServer(t *testing.T) {
	const key = "ete-secret-key"
	ete, eteSeen := recordingOrigin(t, "FROM-ETE")
	attacker, attackerSeen := recordingOrigin(t, "FROM-ATTACKER")

	router := inferencerouter.New()
	if err := router.Configure(json.RawMessage(`{
		"servers": {"ete": {"url": "` + ete.URL + `", "key": "` + key + `", "main": "opus", "helper": "haiku"}},
		"agents": {"claude-code": "ete"}
	}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	p, err := pipeline.New([]pipeline.Plugin{router})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	// The bridge intercepts the attacker's port, the one the client CONNECTs to.
	// httptest gives every TLS server one certificate, so trusting the attacker's
	// trusts ete's: the bridge would complete either handshake, and only the dial
	// target decides who gets the request.
	engine := redirectBridge(t, portOf(attacker.authority()), certPEM(attacker.Server))
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	shared := memstore.New()
	defer shared.Close()
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Shared:           shared,
		Client:           http.DefaultClient,
		TLSBridge:        engine,
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	status, body := bridgedGet(t, proxy, engine.CAPEM, attacker.authority(), "/v1/messages", func(r *http.Request) {
		r.Host = ete.authority()
		r.Header.Set("User-Agent", "claude-cli/2.1.0 (external, cli)")
		r.Header.Set("Authorization", "Bearer client-key")
	})

	for _, call := range attackerSeen.all() {
		if call != http.MethodHead+" " {
			t.Errorf("the CONNECT authority received %q; only the bridge's keyless HEAD probe may reach it", call)
		}
	}
	if hosts, _ := attacker.gets(); len(hosts) != 0 {
		t.Errorf("the CONNECT authority served the request (hosts %v): the router's key went where the client CONNECTed", hosts)
	}
	if status != http.StatusMisdirectedRequest {
		t.Errorf("response = %d %q, want 421: the Host names another server than the tunnel's", status, body)
	}
	for _, call := range eteSeen.all() {
		if call != http.MethodHead+" " {
			t.Errorf("ete saw %q; a misdirected request must reach no server", call)
		}
	}
}

// keyOrigin is a plain-http origin that records the Authorization of every request
// it serves, and answers with its name.
func keyOrigin(t *testing.T, name string) (*httptest.Server, *seen) {
	t.Helper()
	s := &seen{}
	srv := httptest.NewServer(s.record(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(name))
	})))
	t.Cleanup(srv.Close)
	return srv, s
}

// The documented first setup, through the listener: a Claude Code session is
// already talking to ete when the router is first added and claude-code routed to
// glm, and sends nothing in between. The router has never seen that session, so it
// holds no pin; the session store's history does, and the next turn stays on ete,
// with ete's key, rather than moving to glm mid-conversation. A session that starts
// afterwards goes to glm. Both servers are plain http on loopback, which the router
// sends to without a TLS bridge; 127.0.0.1 and localhost are two hosts to it.
func TestForwardProxy_ASessionRunningBeforeRoutingWasConfiguredStaysOnItsServer(t *testing.T) {
	ete, eteSeen := keyOrigin(t, "FROM-ETE")
	glm, glmSeen := keyOrigin(t, "FROM-GLM")
	eteURL := ete.URL
	glmURL := strings.Replace(glm.URL, "127.0.0.1", "localhost", 1)

	parserOnly, err := pipeline.New([]pipeline.Plugin{inferenceparser.NewInferenceParser()})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	holder := pipeline.NewHolder(parserOnly)
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	shared := memstore.New()
	defer shared.Close()
	srv := &Server{
		OutboundPipeline: holder,
		Sessions:         store,
		Shared:           shared,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader},
		Client:           http.DefaultClient,
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	turn := func(sessionID string) string {
		t.Helper()
		body := `{"model":"claude-opus-5-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
		req, err := http.NewRequest(http.MethodPost, eteURL+"/v1/messages", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "claude-cli/2.1.286 (external, cli)")
		req.Header.Set("Authorization", "Bearer client-key")
		req.Header.Set(session.ClaudeCodeSessionHeader, sessionID)
		resp, err := proxyClient(proxy, nil).Do(req)
		if err != nil {
			t.Fatalf("request through the proxy: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	// Before the router: the session talks to ete with its own key.
	if got := turn("running"); got != "FROM-ETE" {
		t.Fatalf("first turn reached %q, want ete", got)
	}

	// agentop server add ete, add glm, use glm --agent claude-code: one reload.
	router := inferencerouter.New()
	if err := router.Configure(json.RawMessage(`{
		"servers": {"ete": {"url": "` + eteURL + `", "key": "ete-key", "main": "opus", "helper": "haiku"},
			"glm": {"url": "` + glmURL + `", "key": "glm-key", "main": "glm", "helper": "nemotron"}},
		"agents": {"claude-code": "glm"}
	}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	routed, err := pipeline.New([]pipeline.Plugin{inferenceparser.NewInferenceParser(), router})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	holder.Store(routed)

	if got := turn("running"); got != "FROM-ETE" {
		t.Errorf("the running session's next turn reached %q, want ete: it moved mid-conversation", got)
	}
	if got := turn("fresh"); got != "FROM-GLM" {
		t.Errorf("a new session reached %q, want glm", got)
	}
	if got, want := eteSeen.all(), []string{"POST Bearer client-key", "POST Bearer ete-key"}; !slices.Equal(got, want) {
		t.Errorf("ete saw %q, want %q: the running session keeps its server and gets that server's key", got, want)
	}
	if got, want := glmSeen.all(), []string{"POST Bearer glm-key"}; !slices.Equal(got, want) {
		t.Errorf("glm saw %q, want only the new session's %q", got, want)
	}
}

// moveOriginSrv is an origin that records each request's body, so a test can
// assert on what the destination received.
type moveOriginSrv struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func moveOrigin(t *testing.T, name string) *moveOriginSrv {
	t.Helper()
	o := &moveOriginSrv{}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		o.mu.Lock()
		o.bodies = append(o.bodies, string(b))
		o.mu.Unlock()
		_, _ = w.Write([]byte(name))
	}))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *moveOriginSrv) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.bodies)
}

func (o *moveOriginSrv) lastBody() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.bodies) == 0 {
		return ""
	}
	return o.bodies[len(o.bodies)-1]
}

// A session moved by a sessions: override lands on the new server with the new
// server's key, the old server's thinking dropped and the new server's kept, one
// request per turn, and stays there once the override is gone — the pin the move
// settled. A move to its own provider gets back the thinking that provider wrote.
func TestForwardProxy_ASessionsOverrideMovesARunningSession(t *testing.T) {
	ete := moveOrigin(t, "FROM-ETE")
	glm := moveOrigin(t, "FROM-GLM")
	eteURL := ete.srv.URL
	glmURL := strings.Replace(glm.srv.URL, "127.0.0.1", "localhost", 1)

	router := inferencerouter.New()
	parserOnly, err := pipeline.New([]pipeline.Plugin{inferenceparser.NewInferenceParser()})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	holder := pipeline.NewHolder(parserOnly)
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	shared := memstore.New()
	defer shared.Close()
	srv := &Server{
		OutboundPipeline: holder,
		Sessions:         store,
		Shared:           shared,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader},
		Client:           http.DefaultClient,
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// Each turn's body is the conversation so far: the client appends every
	// answer, its thinking block included, and re-sends the whole history.
	var turns []string
	turn := func(sessionID, answerSig string) string {
		t.Helper()
		msgs := []string{`{"role":"user","content":"hi"}`}
		for _, sig := range turns {
			msgs = append(msgs, `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"`+sig+`"},{"type":"text","text":"hello"}]}`,
				`{"role":"user","content":"again"}`)
		}
		body := `{"model":"claude-opus-5-5","max_tokens":8,"messages":[` + strings.Join(msgs, ",") + `]}`
		req, err := http.NewRequest(http.MethodPost, eteURL+"/v1/messages", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "claude-cli/2.1.286 (external, cli)")
		req.Header.Set("Authorization", "Bearer client-key")
		req.Header.Set(session.ClaudeCodeSessionHeader, sessionID)
		resp, err := proxyClient(proxy, nil).Do(req)
		if err != nil {
			t.Fatalf("request through the proxy: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		turns = append(turns, answerSig)
		return string(b)
	}
	configure := func(sessions string) {
		t.Helper()
		if err := router.Configure(json.RawMessage(`{
			"servers": {"ete": {"url": "` + eteURL + `", "key": "ete-key", "main": "opus", "helper": "haiku"},
				"glm": {"url": "` + glmURL + `", "key": "glm-key", "main": "glm", "helper": "nemotron"}},
			"agents": {"claude-code": "ete"},
			"sessions": {` + sessions + `}
		}`)); err != nil {
			t.Fatalf("Configure: %v", err)
		}
	}
	signatures := func(body string) []string {
		var out []string
		for _, part := range strings.Split(body, `"signature":"`)[1:] {
			out = append(out, part[:strings.Index(part, `"`)])
		}
		return out
	}

	// The session runs on ete for two turns, and ete gets its own thinking back.
	configure(``)
	routed, err := pipeline.New([]pipeline.Plugin{inferenceparser.NewInferenceParser(), router})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	holder.Store(routed)
	turn("running", "e1")
	if got := turn("running", "e2"); got != "FROM-ETE" {
		t.Fatalf("turn reached %q, want ete", got)
	}
	if got := signatures(ete.lastBody()); !slices.Equal(got, []string{"e1"}) {
		t.Errorf("ete received %v, want its own e1", got)
	}

	// agentop server move to glm: one request, ete's thinking dropped before it is
	// sent — only ete can read it — and the assistant text kept.
	configure(`"running": "glm"`)
	if got := turn("running", "g1"); got != "FROM-GLM" {
		t.Fatalf("moved turn reached %q, want glm", got)
	}
	if body := glm.lastBody(); len(signatures(body)) != 0 || !strings.Contains(body, `"hello"`) {
		t.Errorf("glm received %s, want ete's thinking gone and the text kept", body)
	}

	// The override gone, the session stays on glm, and glm keeps its own thinking.
	configure(``)
	if got := turn("running", "g2"); got != "FROM-GLM" {
		t.Fatalf("post-override turn reached %q, want glm: the pin must follow the move", got)
	}
	if got := signatures(glm.lastBody()); !slices.Equal(got, []string{"g1"}) {
		t.Errorf("glm received %v, want only its own g1", got)
	}
	if n := glm.count(); n != 2 {
		t.Errorf("glm received %d requests, want one per turn", n)
	}

	// own: the entry STAYS as "" — removing it would let the pin route the session
	// right back — and the request goes where the client sent it, ete, which gets
	// back the thinking it wrote and none of glm's.
	configure(`"running": ""`)
	if got := turn("running", "o1"); got != "FROM-ETE" {
		t.Fatalf("own turn reached %q, want ete: the empty entry must stop the routing", got)
	}
	if got := signatures(ete.lastBody()); !slices.Equal(got, []string{"e1", "e2"}) {
		t.Errorf("ete's own turn received %v, want its own e1 and e2", got)
	}
}

const routedKey = "s-key"

// routedServer is an upstream for the router's api tests. It lists claude-sonnet-5 at
// /v1/models, answers every other request with answer, and keeps those requests.
func routedServer(t *testing.T, answer http.HandlerFunc) *chatServer {
	t.Helper()
	return newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"data":[{"id":"claude-sonnet-5"}]}`)
			return
		}
		answer(w, r)
	})
}

// sent are the requests up got, without the router's model-list fetches.
func sent(up *chatServer) []upstreamRequest {
	var out []upstreamRequest
	for _, r := range up.got() {
		if r.path != "/v1/models" {
			out = append(out, r)
		}
	}
	return out
}

// routedProxy is a forward proxy with inference-parser and an inference router that
// sends claude-code, opencode and codex to up, a server with the extra fields given. The
// router's model list for up is claude-sonnet-5 from the start, so that a resend has
// a substitute.
func routedProxy(t *testing.T, up *chatServer, fields string) (*httptest.Server, *session.Store) {
	t.Helper()
	st, err := filestore.Open(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_ = st.Set(context.Background(), inferencerouter.Name+"/models/s", `["claude-sonnet-5"]`, 0)
	router := inferencerouter.New()
	router.SetStore(st)
	if err := router.Configure(json.RawMessage(`{
		"servers": {"s": {"url": "` + up.URL + `", "key": "` + routedKey + `", "main": "sonnet", "helper": "sonnet"` + fields + `}},
		"agents": {"claude-code": "s", "opencode": "s", "codex": "s"}
	}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := router.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Shutdown(context.Background()) })
	p, err := pipeline.New([]pipeline.Plugin{inferenceparser.NewInferenceParser(), router})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	store := newStore(t)
	shared := memstore.New()
	t.Cleanup(shared.Close)
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Shared:           shared,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader},
		Client:           http.DefaultClient,
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	return proxy, store
}

// claudeCodeRequest is Claude Code's request header set, with its X-Api-Key.
func claudeCodeRequest(r *http.Request) {
	r.Header.Set("User-Agent", "claude-cli/2.1.286 (external, cli)")
	r.Header.Set("X-Api-Key", "client-key")
	r.Header.Set(session.ClaudeCodeSessionHeader, "s1")
}

// Rule C2 and the auth test: Claude Code's X-Api-Key request reaches a chat server
// with only Authorization: Bearer <key>, and a chat body.
func TestRouterAPI_ChatServerGetsBearerAndAChatBody(t *testing.T) {
	up := routedServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, replyFixture(t, "bob_shape1.json"))
	})
	proxy, _ := routedProxy(t, up, `, "api": {"chat-completions": "/v1/chat/completions"}`)
	resp := postAnthropic(t, proxy, anthropicRequest, claudeCodeRequest)
	body := replyText(t, resp)

	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"type":"message"`) {
		t.Errorf("reply = %d %s, want an Anthropic message", resp.StatusCode, body)
	}
	reqs := sent(up)
	if len(reqs) != 1 {
		t.Fatalf("upstream got %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.path != "/v1/chat/completions" || !bytes.Contains(r.body, []byte(`"messages":[{"role":"user","content":[`)) {
		t.Errorf("upstream got %s %s, want a chat body at /v1/chat/completions", r.path, r.body)
	}
	if got := r.header.Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+routedKey {
		t.Errorf("Authorization = %q, want only Bearer %s", got, routedKey)
	}
	for _, h := range []string{"X-Api-Key", "Api-Key", "Anthropic-Version"} {
		if got := r.header.Values(h); len(got) != 0 {
			t.Errorf("%s = %q, want none", h, got)
		}
	}
}

// Rules S2, S3 and S12 end to end: a Codex request goes to a server that serves only
// Anthropic Messages, with the router's key in X-Api-Key and no Codex header, and the
// Anthropic stream comes back to Codex as a Responses stream.
func TestRouterAPI_CodexToAnthropicServer(t *testing.T) {
	sse, err := os.ReadFile("../../translate/testdata/responses-anthropic/reply/two_tools.sse")
	if err != nil {
		t.Fatal(err)
	}
	up := routedServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(sse)
	})
	proxy, _ := routedProxy(t, up, `, "api": {"anthropic-messages": "/v1/messages"}`)
	req, _ := http.NewRequest(http.MethodPost, "http://chatgpt.example/backend-api/codex/responses?x=1", strings.NewReader(codexRequest))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex-tui/0.162.1 (Mac OS 26.3.0; arm64) xterm-256color (codex-tui; 0.162.1)")
	for _, h := range codexHeaders {
		req.Header.Set(h, "v")
	}
	req.Header.Set("Authorization", "Bearer client-key")
	resp, err := proxyClient(proxy, nil).Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	body := replyText(t, resp)

	reqs := sent(up)
	if len(reqs) != 1 {
		t.Fatalf("upstream got %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.path != "/v1/messages" || r.rawQuery != "" {
		t.Errorf("upstream got path %q query %q, want /v1/messages and no query", r.path, r.rawQuery)
	}
	if got := r.header.Values("X-Api-Key"); len(got) != 1 || got[0] != routedKey {
		t.Errorf("X-Api-Key = %q, want only %s", got, routedKey)
	}
	if got := r.header.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Errorf("Anthropic-Version = %q, want 2023-06-01", got)
	}
	for _, h := range append([]string{"Authorization"}, codexHeaders...) {
		if got := r.header.Values(h); len(got) != 0 {
			t.Errorf("upstream saw %s: %q", h, got)
		}
	}
	if !bytes.Contains(r.body, []byte(`"messages":[{"role":"user"`)) ||
		!bytes.HasSuffix(r.body, []byte(`,"cache_control":{"type":"ephemeral"}}`)) {
		t.Errorf("upstream body is not an Anthropic body with the top-level cache marker: %s", r.body)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, "event: response.created\n") ||
		!strings.Contains(body, "event: response.completed\n") {
		t.Errorf("reply = %d %s, want a Responses stream", resp.StatusCode, body)
	}
}

// Rule 4a: nothing goes upstream, and the agent gets an Anthropic api_error.
func TestRouterAPI_ResponsesOnlyServerIsA502(t *testing.T) {
	up := routedServer(t, func(w http.ResponseWriter, _ *http.Request) { t.Error("the server got a request") })
	proxy, _ := routedProxy(t, up, `, "api": {"responses": "/v1/responses"}`)
	resp := postAnthropic(t, proxy, anthropicRequest, claudeCodeRequest)
	body := replyText(t, resp)

	const msg = "Cortex cannot translate anthropic-messages to responses yet. Use a translation server for this pair " +
		"(see https://github.com/rossoctl/cortex/blob/main/docs/translation-server.md)."
	var got struct {
		Type  string `json:"type"`
		Error struct{ Type, Message string }
	}
	if resp.StatusCode != http.StatusBadGateway || json.Unmarshal([]byte(body), &got) != nil ||
		got.Type != "error" || got.Error.Type != "api_error" || got.Error.Message != msg {
		t.Errorf("reply = %d %s, want a 502 Anthropic api_error with %q", resp.StatusCode, body, msg)
	}
	if reqs := sent(up); len(reqs) != 0 {
		t.Errorf("upstream got %d requests, want none", len(reqs))
	}
}

// Rule C1: the agent's protocol is a key, so the request goes untranslated to its path.
func TestRouterAPI_AnthropicPathEntry(t *testing.T) {
	up := routedServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","role":"assistant","content":[]}`)
	})
	proxy, _ := routedProxy(t, up, `, "api": {"anthropic-messages": "/anthropic/v1/messages"}`)
	resp := postAnthropic(t, proxy, anthropicRequest, claudeCodeRequest)
	_ = replyText(t, resp)

	reqs := sent(up)
	if len(reqs) != 1 || reqs[0].path != "/anthropic/v1/messages" || string(reqs[0].body) != anthropicRequest {
		t.Fatalf("upstream got %+v, want the body unchanged at /anthropic/v1/messages", reqs)
	}
	// Not translated, empty auth_header: setKey keeps the agent's header.
	if got := reqs[0].header.Get("X-Api-Key"); got != routedKey {
		t.Errorf("X-Api-Key = %q, want %s", got, routedKey)
	}
}

// Rule C1 of #1911: a server that serves Chat gets a Chat request as it is.
func TestRouterAPI_AChatServerGetsTheChatBodyUnchanged(t *testing.T) {
	up := routedServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, replyFixture(t, "bob_shape1.json"))
	})
	proxy, _ := routedProxy(t, up, `, "api": {"chat-completions": "/v1/chat/completions"}`)
	const chatBody = `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, "http://api.openai.example/v1/chat/completions", strings.NewReader(chatBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "opencode/latest/2.0.21/cli")
	resp, err := proxyClient(proxy, nil).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	reqs := sent(up)
	if len(reqs) != 1 || reqs[0].path != "/v1/chat/completions" || string(reqs[0].body) != chatBody {
		t.Errorf("upstream got %+v, want the chat body unchanged at /v1/chat/completions", reqs)
	}
}

// Review Focus 5 of #1966: a Chat request goes to a server that serves only Anthropic
// Messages, with the router's key in X-Api-Key and no OpenAI header, and the Anthropic
// stream comes back to the agent as a Chat stream.
func TestRouterAPI_ChatToAnthropicServer(t *testing.T) {
	sse, err := os.ReadFile("../../translate/testdata/responses-anthropic/reply/two_tools.sse")
	if err != nil {
		t.Fatal(err)
	}
	up := routedServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(sse)
	})
	proxy, _ := routedProxy(t, up, `, "api": {"anthropic-messages": "/v1/messages"}`)
	const chatBody = `{"model":"claude-opus-5-5","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, "http://api.openai.example/v1/chat/completions", strings.NewReader(chatBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "opencode/latest/2.0.21/cli")
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("OpenAI-Beta", "v")
	resp, err := proxyClient(proxy, nil).Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	body := replyText(t, resp)

	reqs := sent(up)
	if len(reqs) != 1 {
		t.Fatalf("upstream got %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.path != "/v1/messages" {
		t.Errorf("upstream got path %q, want /v1/messages", r.path)
	}
	if got := r.header.Values("X-Api-Key"); len(got) != 1 || got[0] != routedKey {
		t.Errorf("X-Api-Key = %q, want only %s", got, routedKey)
	}
	if got := r.header.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Errorf("Anthropic-Version = %q, want 2023-06-01", got)
	}
	for _, h := range []string{"Authorization", "OpenAI-Beta"} {
		if got := r.header.Values(h); len(got) != 0 {
			t.Errorf("upstream saw %s: %q", h, got)
		}
	}
	if !bytes.Contains(r.body, []byte(`"messages":[{"role":"user"`)) {
		t.Errorf("upstream body is not an Anthropic body: %s", r.body)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{`"object":"chat.completion.chunk"`, `"finish_reason":"tool_calls"`, `"choices":[]`} {
		if !strings.Contains(body, want) {
			t.Errorf("reply has no %s:\n%s", want, body)
		}
	}
	if !strings.HasPrefix(body, `data: {"id":`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("reply is not a Chat stream that ends with [DONE]:\n%s", body)
	}
}

// #1966: the legacy completions API has no protocol, so it is not translated, also to a server
// that serves only Anthropic Messages. The request names the server's host: the router
// does not capture a legacy completions request to another host.
func TestRouterAPI_LegacyCompletionsIsNotTranslated(t *testing.T) {
	up := routedServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","choices":[{"text":"ok"}]}`)
	})
	proxy, _ := routedProxy(t, up, `, "api": {"anthropic-messages": "/v1/messages"}`)
	const body = `{"model":"claude-opus-5-5","prompt":"hi"}`
	req, _ := http.NewRequest(http.MethodPost, up.URL+"/v1/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "opencode/latest/2.0.21/cli")
	resp, err := proxyClient(proxy, nil).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	reqs := sent(up)
	if len(reqs) != 1 || reqs[0].path != "/v1/completions" || string(reqs[0].body) != body {
		t.Errorf("upstream got %+v, want the body unchanged at /v1/completions", reqs)
	}
}

// Rule 6: count_tokens routed to a chat server is answered here with a 404.
func TestRouterAPI_CountTokensIsALocal404(t *testing.T) {
	up := routedServer(t, func(w http.ResponseWriter, _ *http.Request) { t.Error("the server got a request") })
	proxy, _ := routedProxy(t, up, `, "api": {"chat-completions": "/v1/chat/completions"}`)
	req, _ := http.NewRequest(http.MethodPost, up.URL+"/v1/messages/count_tokens", strings.NewReader(anthropicRequest))
	req.Header.Set("Content-Type", "application/json")
	claudeCodeRequest(req)
	resp, err := proxyClient(proxy, nil).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound || !bytes.Contains(body, []byte(`"not_found_error"`)) {
		t.Errorf("reply = %d %s, want a 404 not_found_error", resp.StatusCode, body)
	}
	if reqs := sent(up); len(reqs) != 0 {
		t.Errorf("upstream got %d requests, want none", len(reqs))
	}
}

// A translated request that the server refuses for its model is resent with the
// router's substitute, as an untranslated one is: LiteLLM's 403 and Bob's 404.
func TestRouterAPI_ATranslatedRefusalIsResent(t *testing.T) {
	for _, refusal := range []struct {
		name    string
		status  int
		fixture string
	}{
		{"litellm 403", http.StatusForbidden, "error_403_litellm.json"},
		{"bob 404", http.StatusNotFound, "error_404_bob.json"},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			up := routedServer(t, func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				calls++
				n := calls
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if n == 1 {
					w.WriteHeader(refusal.status)
					_, _ = io.WriteString(w, replyFixture(t, refusal.fixture))
					return
				}
				_, _ = io.WriteString(w, replyFixture(t, "bob_shape1.json"))
			})
			proxy, _ := routedProxy(t, up, `, "api": {"chat-completions": "/v1/chat/completions"}`)
			resp := postAnthropic(t, proxy, anthropicRequest, claudeCodeRequest)
			body := replyText(t, resp)

			if resp.StatusCode != http.StatusOK || !strings.Contains(body, "391 is not prime") {
				t.Errorf("reply = %d %s, want the second answer, translated", resp.StatusCode, body)
			}
			reqs := sent(up)
			if len(reqs) != 2 {
				t.Fatalf("upstream got %d requests, want 2", len(reqs))
			}
			if !bytes.Contains(reqs[1].body, []byte(`"model":"claude-sonnet-5"`)) || reqs[1].path != "/v1/chat/completions" {
				t.Errorf("resent request = %s %s, want a chat body with the substitute", reqs[1].path, reqs[1].body)
			}
		})
	}
}
