package inferencerouter

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
	"github.com/rossoctl/cortex/core/translate"
)

// routerconfig keeps its own list, because it must not import core/translate
// (decision 15). This pins the two lists equal.
func TestAPIProtocolsMatchRegistry(t *testing.T) {
	if !slices.Equal(routerconfig.APIProtocols, translate.Names()) {
		t.Errorf("routerconfig.APIProtocols = %v, translate.Names() = %v; want equal", routerconfig.APIProtocols, translate.Names())
	}
}

func TestPlan(t *testing.T) {
	for _, tc := range []struct {
		name string
		api  map[string]string
		path string
		want apiPlan
	}{
		{"empty map", nil, "/v1/messages", apiPlan{kind: planAsIs}},
		{"chat only", map[string]string{"chat-completions": "/v1/chat/completions"}, "/v1/messages",
			apiPlan{kind: planTranslate, source: "anthropic-messages", target: "chat-completions", path: "/v1/chat/completions"}},
		{"anthropic at its own path", map[string]string{"anthropic-messages": "/anthropic/v1/messages"}, "/v1/messages",
			apiPlan{kind: planPath, path: "/anthropic/v1/messages"}},
		{"responses only", map[string]string{"responses": "/v1/responses"}, "/v1/messages",
			apiPlan{kind: planRefuse, source: "anthropic-messages"}},
		// Rule 7: a chat request cannot be decoded, so it is not translated.
		{"no decoder", map[string]string{"anthropic-messages": "/v1/messages", "responses": "/v1/responses"}, "/v1/chat/completions",
			apiPlan{kind: planAsIs}},
		{"count_tokens to a chat server", map[string]string{"chat-completions": "/v1/chat/completions"}, "/v1/messages/count_tokens",
			apiPlan{kind: planCountTokens}},
		{"count_tokens to an anthropic server", map[string]string{"anthropic-messages": "/v1/messages"}, "/v1/messages/count_tokens",
			apiPlan{kind: planAsIs}},
		{"codex to chat only", map[string]string{"chat-completions": "/v1/chat/completions"}, "/backend-api/codex/responses",
			apiPlan{kind: planTranslate, source: "responses", target: "chat-completions", path: "/v1/chat/completions"}},
		{"codex to responses", map[string]string{"responses": "/v1/responses"}, "/backend-api/codex/responses",
			apiPlan{kind: planPath, path: "/v1/responses"}},
		{"DeepSeek chat", map[string]string{"chat-completions": "/chat/completions", "anthropic-messages": "/anthropic/v1/messages"}, "/v1/chat/completions",
			apiPlan{kind: planPath, path: "/chat/completions"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (route{api: tc.api}).plan(tc.path); got != tc.want {
				t.Errorf("plan(%q) = %+v, want %+v", tc.path, got, tc.want)
			}
		})
	}
}

// apiConfig is one server, "s", with the extra server fields given, and claude-code
// routed to it.
func apiConfig(fields string) string {
	return `{
		"servers": {"s": {"url": "https://s.example.com", "key": "s-key", "main": "opus", "helper": "haiku"` + fields + `}},
		"agents": {"claude-code": "s"}
	}`
}

const chatAPI = `, "api": {"chat-completions": "/v1/chat/completions"}`

// Rule C2: the key goes in one header, and the agent's other auth headers are gone.
func TestRouter_SetAuth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields string
		sent   http.Header
		want   http.Header // every auth header the request must carry
	}{
		{"not translated, empty auth_header: setKey", "", http.Header{"X-Api-Key": {"client-key"}},
			http.Header{"X-Api-Key": {"s-key"}}},
		{"translated to chat", chatAPI, http.Header{"X-Api-Key": {"client-key"}, "Api-Key": {"client-key"}},
			http.Header{"Authorization": {"Bearer s-key"}}},
		{"path entry, not translated: setKey", `, "api": {"anthropic-messages": "/x/v1/messages"}`, http.Header{"Authorization": {"Bearer client-key"}},
			http.Header{"Authorization": {"Bearer s-key"}}},
		{"auth_header x-api-key", chatAPI + `, "auth_header": "x-api-key"`, http.Header{"Authorization": {"Bearer client-key"}},
			http.Header{"X-Api-Key": {"s-key"}}},
		{"auth_header api-key", `, "auth_header": "api-key"`, http.Header{"X-Api-Key": {"client-key"}, "Authorization": {"Bearer client-key"}},
			http.Header{"Api-Key": {"s-key"}}},
		{"auth_header authorization", `, "auth_header": "authorization"`, http.Header{"X-Api-Key": {"client-key"}},
			http.Header{"Authorization": {"Bearer s-key"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := build(t, apiConfig(tc.fields))
			pctx := request(newStore(t), "api.anthropic.com", claudeUA, "s1")
			pctx.Headers = tc.sent
			pctx.Headers.Set("User-Agent", claudeUA)
			if a := run(t, p, pctx); a.Type != pipeline.Continue {
				t.Fatalf("action = %+v, want Continue", a)
			}
			for _, h := range []string{"Authorization", "X-Api-Key", "Api-Key"} {
				if got, want := pctx.Headers.Values(h), tc.want.Values(h); !slices.Equal(got, want) {
					t.Errorf("%s = %q, want %q", h, got, want)
				}
			}
		})
	}
}

// A request translated to Anthropic Messages gets the key in X-Api-Key only. A
// Responses request can be translated to it. This test calls setAuth directly.
func TestSetAuth_TranslatedToAnthropic(t *testing.T) {
	pctx := &pipeline.Context{Headers: http.Header{"Authorization": {"Bearer client-key"}, "Api-Key": {"client-key"}}}
	setAuth(pctx, route{key: "s-key"}, "anthropic-messages")
	if got := pctx.Headers; len(got) != 1 || got.Get("X-Api-Key") != "s-key" {
		t.Errorf("headers = %v, want only X-Api-Key: s-key", got)
	}
}

// Rule 5: the router does not change Path and does not call SetRedirectPath.
func TestRouter_ATranslatedRequestKeepsItsPath(t *testing.T) {
	p := build(t, apiConfig(chatAPI))
	pctx := request(newStore(t), "api.anthropic.com", claudeUA, "s1")
	pctx.Path = "/zen/v1/messages"
	run(t, p, pctx)

	if pctx.Path != "/zen/v1/messages" {
		t.Errorf("Path = %q, want the client's path", pctx.Path)
	}
	if path, ok := pctx.RedirectPath(); ok {
		t.Errorf("RedirectPath = %q, want none for a translated request", path)
	}
	src, tgt, path, ok := pctx.TranslateTarget()
	if !ok || src != "anthropic-messages" || tgt != "chat-completions" || path != "/v1/chat/completions" {
		t.Errorf("TranslateTarget = %q %q %q %v, want anthropic-messages to chat-completions at /v1/chat/completions", src, tgt, path, ok)
	}
	assertRecord(t, pctx, pipeline.ActionModify, "routed", map[string]string{"server": "s"})
}

func TestRouter_APathEntrySetsTheRedirectPath(t *testing.T) {
	p := build(t, apiConfig(`, "api": {"anthropic-messages": "/anthropic/v1/messages"}`))
	pctx := request(newStore(t), "api.anthropic.com", claudeUA, "s1")
	run(t, p, pctx)

	if path, ok := pctx.RedirectPath(); !ok || path != "/anthropic/v1/messages" {
		t.Errorf("RedirectPath = %q %v, want /anthropic/v1/messages", path, ok)
	}
	if _, _, _, ok := pctx.TranslateTarget(); ok {
		t.Error("TranslateTarget is set, want no translation")
	}
}

// Rule R4: a Codex request to a server that serves Responses goes to its path,
// with its body not changed and no translation.
func TestRouter_CodexToAResponsesServerKeepsItsBody(t *testing.T) {
	p := build(t, `{
		"servers": {"s": {"url": "https://s.example.com", "key": "s-key", "main": "gpt-5", "helper": "gpt-5-mini", "api": {"responses": "/v1/responses"}}},
		"agents": {"codex": "s"}
	}`)
	pctx := request(newStore(t), "chatgpt.com", "codex-tui/0.162.1 (Mac OS 26.3.0; arm64) xterm-256color (codex-tui; 0.162.1)", "s1")
	pctx.Path = "/backend-api/codex/responses"
	body := []byte(`{"model":"gpt-5","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"store":false}`)
	pctx.Body = slices.Clone(body)
	run(t, p, pctx)

	if path, ok := pctx.RedirectPath(); !ok || path != "/v1/responses" {
		t.Errorf("RedirectPath = %q %v, want /v1/responses", path, ok)
	}
	if _, _, _, ok := pctx.TranslateTarget(); ok {
		t.Error("TranslateTarget is set, want no translation")
	}
	if pctx.BodyMutated() || string(pctx.Body) != string(body) {
		t.Errorf("body = %s (mutated %v), want the agent's body", pctx.Body, pctx.BodyMutated())
	}
}

func TestRouter_HeadersReachUpstreamHeaders(t *testing.T) {
	p := build(t, apiConfig(`, "headers": {"user-agent": "bob-shell"}`))
	pctx := request(newStore(t), "api.anthropic.com", claudeUA, "s1")
	run(t, p, pctx)

	if got := pctx.UpstreamHeaders().Values("User-Agent"); !slices.Equal(got, []string{"bob-shell"}) {
		t.Errorf("UpstreamHeaders User-Agent = %q, want bob-shell", got)
	}
}

func TestRouter_NoHeadersSetsNoUpstreamHeaders(t *testing.T) {
	p := build(t, apiConfig(""))
	pctx := request(newStore(t), "api.anthropic.com", claudeUA, "s1")
	run(t, p, pctx)

	if h := pctx.UpstreamHeaders(); h != nil {
		t.Errorf("UpstreamHeaders = %v, want nil", h)
	}
}

// Rule 4a, with the decision 13 message.
func TestRouter_RefusesAPairItCannotTranslate(t *testing.T) {
	p := build(t, apiConfig(`, "api": {"responses": "/v1/responses"}`))
	pctx := request(newStore(t), "api.anthropic.com", claudeUA, "s1")
	a := run(t, p, pctx)

	const msg = "Cortex cannot translate anthropic-messages to responses yet. Use a translation server for this pair " +
		"(see https://github.com/rossoctl/cortex/blob/main/docs/translation-server.md)."
	if a.Type != pipeline.Reject || a.Violation == nil {
		t.Fatalf("action = %+v, want Reject", a)
	}
	v := a.Violation
	if v.Code != "inference.translation_unavailable" || v.Status != http.StatusBadGateway || v.Reason != msg || v.BodyType != "application/json" {
		t.Errorf("violation = %+v, want a 502 inference.translation_unavailable with the decision 13 message", v)
	}
	var body struct {
		Type  string `json:"type"`
		Error struct{ Type, Message string }
	}
	if err := json.Unmarshal(v.Body, &body); err != nil || body.Type != "error" || body.Error.Type != "api_error" || body.Error.Message != msg {
		t.Errorf("body = %s (%v), want an Anthropic api_error with the message", v.Body, err)
	}
	assertRecord(t, pctx, pipeline.ActionDeny, "cannot_translate", map[string]string{"server": "s", "from": "anthropic-messages"})
}

func TestRouter_TheRefusalNamesEveryAPIKey(t *testing.T) {
	r := route{api: map[string]string{"responses": "/v1/responses", "anthropic-messages": "/v1/messages"}}
	got := cannotTranslate("chat-completions", r)
	if !strings.HasPrefix(got, "Cortex cannot translate chat-completions to anthropic-messages or responses yet.") {
		t.Errorf("message = %q, want the sorted keys joined with or", got)
	}
}

// Rule 6: count_tokens to a server with no anthropic-messages entry is answered here.
func TestRouter_AnswersCountTokensLocally(t *testing.T) {
	p := build(t, apiConfig(chatAPI))
	pctx := request(newStore(t), "s.example.com", claudeUA, "s1")
	pctx.Path, pctx.Extensions.Inference = "/v1/messages/count_tokens", nil
	a := run(t, p, pctx)

	if a.Type != pipeline.Reject || a.Violation == nil || a.Violation.Status != http.StatusNotFound {
		t.Fatalf("action = %+v, want a 404 Reject", a)
	}
	if !strings.Contains(string(a.Violation.Body), `"not_found_error"`) ||
		!strings.Contains(string(a.Violation.Body), "this inference server does not count tokens") {
		t.Errorf("body = %s, want an Anthropic not_found_error", a.Violation.Body)
	}
	assertRecord(t, pctx, pipeline.ActionDeny, "count_tokens_local", map[string]string{"server": "s"})
}

// bobConfig is one server, "bob", at a URL with a path prefix, with the extra server
// fields given, and claude-code routed to it.
func bobConfig(fields string) string {
	return `{
		"servers": {"bob": {"url": "https://api.us-east.bob.ibm.com/inference", "key": "b-key", "main": "opus", "helper": "haiku"` + fields + `}},
		"agents": {"claude-code": "bob"}
	}`
}

// Rule 1: a captured request goes to the prefix and then the agent's path.
func TestRoute_PrefixOnACapturedRequest(t *testing.T) {
	p := build(t, bobConfig(""))
	pctx := request(newStore(t), "api.anthropic.com", claudeUA, "s1")
	run(t, p, pctx)

	if path, ok := pctx.RedirectPath(); !ok || path != "/inference/v1/messages" {
		t.Errorf("RedirectPath = %q %v, want /inference/v1/messages", path, ok)
	}
}

func TestRoute_PrefixOnATranslatedRequest(t *testing.T) {
	p := build(t, bobConfig(chatAPI))
	pctx := request(newStore(t), "api.anthropic.com", claudeUA, "s1")
	run(t, p, pctx)

	if _, _, path, ok := pctx.TranslateTarget(); !ok || path != "/inference/v1/chat/completions" {
		t.Errorf("TranslateTarget path = %q %v, want /inference/v1/chat/completions", path, ok)
	}
}

func TestRoute_PrefixOnAnAPIPath(t *testing.T) {
	p := build(t, bobConfig(`, "api": {"anthropic-messages": "/v1/messages"}`))
	pctx := request(newStore(t), "api.anthropic.com", claudeUA, "s1")
	run(t, p, pctx)

	if path, ok := pctx.RedirectPath(); !ok || path != "/inference/v1/messages" {
		t.Errorf("RedirectPath = %q %v, want /inference/v1/messages", path, ok)
	}
}

// Review focus 1: a request that names the server's host already has the prefix.
func TestRoute_PrefixNotAddedToAnAddressedRequest(t *testing.T) {
	p := build(t, bobConfig(""))
	pctx := request(newStore(t), "api.us-east.bob.ibm.com", claudeUA, "s1")
	pctx.Path = "/inference/v1/chat/completions"
	run(t, p, pctx)

	if path, ok := pctx.RedirectPath(); ok {
		t.Errorf("RedirectPath = %q, want none for an addressed request", path)
	}
	if pctx.Path != "/inference/v1/chat/completions" {
		t.Errorf("Path = %q, want /inference/v1/chat/completions", pctx.Path)
	}
}

// A request moved off the server its host names goes to the routed server's prefix,
// not to the named server's.
func TestRoute_PrefixOnAMovedAddressedRequest(t *testing.T) {
	cfg := `{
		"servers": {
			"bob":  {"url": "https://api.us-east.bob.ibm.com/inference", "key": "b-key", "main": "opus", "helper": "haiku"},
			"lite": {"url": "http://localhost:4000", "key": "l-key", "main": "opus", "helper": "haiku"}
		},
		"sessions": {"s1": %q}
	}`
	for _, tc := range []struct{ to, host, path, want string }{
		{"bob", "localhost:4000", "/v1/messages", "/inference/v1/messages"},
		{"lite", "api.us-east.bob.ibm.com", "/inference/v1/messages", "/v1/messages"},
	} {
		p := build(t, strings.Replace(cfg, "%q", `"`+tc.to+`"`, 1))
		pctx := request(newStore(t), tc.host, claudeUA, "s1")
		pctx.Path = tc.path
		run(t, p, pctx)

		if path, ok := pctx.RedirectPath(); !ok || path != tc.want {
			t.Errorf("%s%s moved to %s: RedirectPath = %q %v, want %s", tc.host, tc.path, tc.to, path, ok, tc.want)
		}
	}
}
