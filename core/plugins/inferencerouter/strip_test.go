package inferencerouter

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"

	"github.com/tidwall/gjson"
)

func stripBody(t *testing.T, body string) (string, int) {
	t.Helper()
	out, n := stripReasoning([]byte(body))
	return string(out), n
}

func TestStrip_AnthropicThinkingBlocks(t *testing.T) {
	out, n := stripBody(t, `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"","signature":"sig"},
			{"type":"text","text":"hello"},
			{"type":"tool_use","id":"t1","name":"lookup","input":{}}
		]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}
	]}`)
	if n != 1 {
		t.Fatalf("dropped = %d, want 1", n)
	}
	var v struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(v.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(v.Messages))
	}
	if want := `[{"type":"text","text":"hello"},{"type":"tool_use","id":"t1","name":"lookup","input":{}}]`; string(v.Messages[1].Content) != want {
		t.Errorf("assistant content = %s, want %s", v.Messages[1].Content, want)
	}
}

// A thinking-only assistant turn is dropped whole: an empty content array is a
// 400 at Anthropic, and a thinking-only turn has no tool_use for a tool_result
// to reference.
func TestStrip_AThinkingOnlyMessageIsDroppedWhole(t *testing.T) {
	out, n := stripBody(t, `{"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"sig"}]},
		{"role":"assistant","content":[{"type":"redacted_thinking","data":"x"}]},
		{"role":"assistant","content":[{"type":"text","text":"hello"}]}
	]}`)
	if n != 2 {
		t.Fatalf("dropped = %d, want 2", n)
	}
	var v struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(v.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(v.Messages))
	}
}

// The user side is never touched: a user's own content, tool results included,
// passes byte-identical.
func TestStrip_LeavesUserMessagesAlone(t *testing.T) {
	body := `{"messages":[
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"},{"type":"text","text":"and this"}]}
	]}`
	out, n := stripBody(t, body)
	if n != 0 || string(out) != body {
		t.Errorf("dropped = %d, out = %s; want 0 and unchanged", n, out)
	}
}

func TestStrip_ResponsesReasoningItems(t *testing.T) {
	out, n := stripBody(t, `{"input":[
		{"type":"reasoning","id":"rs_1","encrypted_content":"abc"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"12121"}]},
		{"role":"user","content":[{"type":"input_text","text":"add 1"}]}
	]}`)
	if n != 1 {
		t.Fatalf("dropped = %d, want 1", n)
	}
	var v struct {
		Input []struct {
			Type string `json:"type"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(v.Input) != 2 || v.Input[0].Type != "message" {
		t.Errorf("input = %+v", v.Input)
	}
}

// Chat completions carries reasoning as plain-text fields no measured server
// rejects; the strip must not touch this format at all.
func TestStrip_LeavesChatCompletionsAlone(t *testing.T) {
	body := `{"messages":[
		{"role":"assistant","content":"12121","reasoning_content":"17*23=391","reasoning":"...","thinking_blocks":[{"signature":"sig"}]},
		{"role":"user","content":"add 1"}
	]}`
	out, n := stripBody(t, body)
	if n != 0 || string(out) != body {
		t.Errorf("dropped = %d; want 0 and byte-identical body", n)
	}
}

// A body with nothing strippable is returned unchanged, so the wiring in Task 4
// can skip SetBody entirely.
func TestStrip_NothingToDropReturnsTheBody(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hi"}],"other":[1,2]}`
	out, n := stripBody(t, body)
	if n != 0 || string(out) != body {
		t.Errorf("dropped = %d; want 0 and unchanged", n)
	}
}

// These tests drive the whole plugin through build/run, like plugin_test.go does.

// A session's first routed request records the server as its reasoning origin and
// strips nothing — when its body carries no reasoning. A first request CAN carry
// foreign reasoning (the client's own prior turns, a conversation carried to a new
// session id), which the pinless-session test covers; this is the genuinely new
// conversation.
func TestRouter_AFirstRoutedRequestRecordsItsOrigin(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := request(store, eteHost, claudeUA, "s1")
	pctx.Body = []byte(`{"model":"claude-opus-5-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	run(t, p, pctx)
	if v, ok := store.Get(originPrefix + "s1"); !ok || v != "glm" {
		t.Errorf("origin = %v %v, want glm", v, ok)
	}
	assertNoInvocation(t, pctx, "reasoning_stripped")
}

// A request to a server other than the recorded origin strips, and the origin
// goes foreign — permanently, because the client's history keeps the old
// server's blocks however many turns pass on the new one.
func TestRouter_AMovedSessionStripsAndGoesForeign(t *testing.T) {
	store := newStore(t)
	before := build(t, routerConfig(`"claude-code": "glm"`))
	run(t, before, request(store, eteHost, claudeUA, "s1"))

	after := build(t, sessionsConfig(`"claude-code": "glm"`, `"s1": "ete"`))
	pctx := request(store, eteHost, claudeUA, "s1")
	pctx.Body = anthropicBodyWithThinking()
	run(t, after, pctx)
	assertAnyRecord(t, pctx, pipeline.ActionModify, "reasoning_stripped", map[string]string{"blocks": "1"})
	if v, _ := store.Get(originPrefix + "s1"); v != originForeign {
		t.Errorf("origin = %v, want %q", v, originForeign)
	}
	if got := gjson.GetBytes(pctx.Body, "messages.1.content.0.type").String(); got != "text" {
		t.Errorf("body's first kept block = %q, want text (thinking still present?)", got)
	}

	// And the next request, same server, still strips: the history is the
	// client's, and it has not forgotten.
	next := request(store, eteHost, claudeUA, "s1")
	next.Body = anthropicBodyWithThinking()
	run(t, after, next)
	assertAnyRecord(t, next, pipeline.ActionModify, "reasoning_stripped", map[string]string{"blocks": "1"})
}

// Steady state on one server keeps its own reasoning: a conversation that began
// under routing keeps its tool loops whole. Its first request carries no
// reasoning; the turns after it carry the server's own.
func TestRouter_SteadyStateKeepsItsOwnReasoning(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	first := request(store, eteHost, claudeUA, "s1")
	first.Body = []byte(`{"model":"claude-opus-5-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	run(t, p, first)
	second := request(store, eteHost, claudeUA, "s1")
	second.Body = anthropicBodyWithThinking()
	run(t, p, second)
	assertNoInvocation(t, second, "reasoning_stripped")
}

// Synthetic sessions are never recorded: their ids hold many conversations.
func TestRouter_NoOriginForSyntheticSessions(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := request(store, eteHost, claudeUA, session.DefaultSessionID)
	pctx.Body = anthropicBodyWithThinking()
	run(t, p, pctx)
	if _, ok := store.Get(originPrefix + session.DefaultSessionID); ok {
		t.Error("an origin was recorded for the default bucket")
	}
}

// anthropicBodyWithThinking is a two-turn Anthropic body whose assistant turn
// carries a signed thinking block.
func anthropicBodyWithThinking() []byte {
	return []byte(`{"model":"claude-opus-5-5","max_tokens":1024,"messages":[
		{"role":"user","content":"Compute 17*23*31."},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"","signature":"sig"},
			{"type":"text","text":"12121"}
		]},
		{"role":"user","content":"Add 1."}
	]}`)
}

// assertAnyRecord fails unless SOME invocation of the router is action/reason with
// the details given — unlike assertRecord, which pins the last one: the routed
// record is appended after the strip's, so the strip is never last.
func assertAnyRecord(t *testing.T, pctx *pipeline.Context, action pipeline.InvocationAction, reason string, details map[string]string) {
	t.Helper()
	invs := pctx.Extensions.Invocations
	if invs == nil {
		t.Fatal("the router recorded nothing")
	}
	for _, inv := range invs.Outbound {
		if inv.Plugin != Name || inv.Action != action || inv.Reason != reason {
			continue
		}
		for k, want := range details {
			if got := inv.Details[k]; got != want {
				t.Errorf("Details[%q] = %q, want %q (all: %v)", k, got, want, inv.Details)
			}
		}
		return
	}
	t.Errorf("no %s/%s invocation among %v", action, reason, invs.Outbound)
}

// assertNoInvocation fails when pctx holds an invocation with reason. The access
// path is the one assertRecord reads: pctx.Extensions.Invocations.Outbound.
func assertNoInvocation(t *testing.T, pctx *pipeline.Context, reason string) {
	t.Helper()
	invs := pctx.Extensions.Invocations
	if invs == nil {
		return
	}
	for _, inv := range invs.Outbound {
		if inv.Plugin == Name && inv.Reason == reason {
			t.Errorf("unexpected %s invocation: %+v", reason, inv)
		}
	}
}

// A session that ran unrouted — pinned "", on the client's own provider, its
// history carrying that provider's signed thinking — strips on its first routed
// request: the first-recorded rule would seed the origin with the target and never
// strip, so a strict target 403s forever. A session with a pin has a prior
// inference life; only one with no pin at all is new.
func TestRouter_AMovedUnroutedSessionStrips(t *testing.T) {
	store := newStore(t)
	before := build(t, routerConfig(``)) // no agent routed: the session pins ""
	run(t, before, request(store, eteHost, claudeUA, "s1"))
	if pin, _ := pinOf(t, store, "s1"); pin != "" {
		t.Fatalf("pin = %q, want not routed", pin)
	}

	after := build(t, sessionsConfig(``, `"s1": "ete"`))
	pctx := request(store, eteHost, claudeUA, "s1")
	pctx.Body = anthropicBodyWithThinking()
	run(t, after, pctx)
	assertAnyRecord(t, pctx, pipeline.ActionModify, "reasoning_stripped", map[string]string{"blocks": "1"})
	if v, _ := store.Get(originPrefix + "s1"); v != originForeign {
		t.Errorf("origin = %v, want %q", v, originForeign)
	}
}

// A session that ran on its own provider and holds no pin — its requests were
// skipped at the capture gate before anything pinned it — carries that provider's
// reasoning. Its first routed request strips: reasoning already in a body the
// router never routed is reasoning it did not see produced. Both formats that
// carry producer-bound reasoning.
func TestRouter_AMovedPinlessSessionStrips(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body []byte
	}{
		{"anthropic thinking", "/v1/messages", anthropicBodyWithThinking()},
		{"responses reasoning", "/v1/responses", []byte(`{"model":"m","input":[
			{"type":"reasoning","id":"rs_1","encrypted_content":"abc"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"12121"}]},
			{"role":"user","content":[{"type":"input_text","text":"add 1"}]}]}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			// The prior life: the agent unrouted, its request to its own provider, a
			// host no server has — skipped before serverFor, so nothing is pinned.
			before := build(t, routerConfig(``))
			prior := request(store, "api.anthropic.com", claudeUA, "s1")
			prior.Path = tc.path
			run(t, before, prior)
			if _, had := pinOf(t, store, "s1"); had {
				t.Fatalf("the prior request pinned the session; this case needs none")
			}

			after := build(t, sessionsConfig(``, `"s1": "ete"`))
			pctx := request(store, "api.anthropic.com", claudeUA, "s1")
			pctx.Path, pctx.Body = tc.path, tc.body
			run(t, after, pctx)
			assertAnyRecord(t, pctx, pipeline.ActionModify, "reasoning_stripped", map[string]string{"blocks": "1"})
			if v, _ := store.Get(originPrefix + "s1"); v != originForeign {
				t.Errorf("origin = %v, want %q", v, originForeign)
			}
		})
	}
}

// A genuinely new session — no pin, no reasoning in its body — records its target
// and strips nothing: there is nothing foreign to drop.
func TestRouter_ANewSessionWithNoReasoningRecordsItsTarget(t *testing.T) {
	store := newStore(t)
	p := build(t, sessionsConfig(``, `"s1": "ete"`))
	pctx := request(store, "api.anthropic.com", claudeUA, "s1")
	pctx.Body = []byte(`{"model":"claude-opus-5-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	run(t, p, pctx)
	if v, _ := store.Get(originPrefix + "s1"); v != "ete" {
		t.Errorf("origin = %v, want ete", v)
	}
	assertNoInvocation(t, pctx, "reasoning_stripped")
}

// A moved pinless session whose first routed request is a one-shot — a title, a
// classifier, no reasoning in its body — still has a prior life: the provider
// requests the router skipped are recorded in the session's history. Seeding the
// origin with the target there would let the conversation's next turn, which does
// carry the provider's signed thinking, through unstripped, and a strict target
// would refuse it for good.
func TestRouter_AMovedPinlessSessionWhoseFirstRoutedRequestIsAOneShotSeedsForeign(t *testing.T) {
	store := newStore(t)
	// The prior life: requests to the client's own provider, recorded in the
	// session's history with their method and path, no pin — the router skipped
	// them at the capture gate. sent() carries no method or path, and serverOf
	// needs the capture shape to read a provider request as evidence.
	prior := sent("api.anthropic.com", claudeUA)
	prior.HTTPMethod, prior.HTTPPath = http.MethodPost, "/v1/messages"

	after := build(t, sessionsConfig(``, `"s1": "ete"`))
	pctx := withHistory(request(store, "api.anthropic.com", claudeUA, "s1"), prior)
	pctx.Body = []byte(`{"model":"claude-haiku-4-5","max_tokens":64,"messages":[{"role":"user","content":"name this conversation"}]}`)
	run(t, after, pctx)
	assertNoInvocation(t, pctx, "reasoning_stripped") // nothing to strip in a one-shot
	if v, _ := store.Get(originPrefix + "s1"); v != originForeign {
		t.Fatalf("origin = %v, want %q: a one-shot must not seed the target", v, originForeign)
	}

	// The conversation's next turn carries the provider's thinking; it strips.
	next := withHistory(request(store, "api.anthropic.com", claudeUA, "s1"), prior)
	next.Body = anthropicBodyWithThinking()
	run(t, after, next)
	assertAnyRecord(t, next, pipeline.ActionModify, "reasoning_stripped", map[string]string{"blocks": "1"})
}

// A session its agent pinned to the target, with no origin recorded — pinned
// before origins were, or its origin expired under a pin every request renews —
// has only ever run on that server: the reasoning in its body is the target's own,
// and nothing strips it. Both ways a request can find that pin.
func TestRouter_APinnedSessionWithNoOriginKeepsItsOwnReasoning(t *testing.T) {
	for _, tc := range []struct{ name, config string }{
		{"its pin", routerConfig(`"claude-code": "glm"`)},
		{"an override naming its pin", sessionsConfig(`"claude-code": "glm"`, `"s1": "glm"`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			first := request(store, eteHost, claudeUA, "s1")
			first.Body = []byte(`{"model":"claude-opus-5-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
			run(t, build(t, routerConfig(`"claude-code": "glm"`)), first)
			store.Delete(originPrefix + "s1")

			second := request(store, eteHost, claudeUA, "s1")
			second.Body = anthropicBodyWithThinking()
			run(t, build(t, tc.config), second)
			assertNoInvocation(t, second, "reasoning_stripped")
			if v, _ := store.Get(originPrefix + "s1"); v != "glm" {
				t.Errorf("origin = %v, want glm", v)
			}
		})
	}
}

// A pin to another server is no provenance for the target: a session moved off it
// with no origin recorded still strips.
func TestRouter_AMovedSessionWithNoOriginStrips(t *testing.T) {
	store := newStore(t)
	run(t, build(t, routerConfig(`"claude-code": "glm"`)), request(store, eteHost, claudeUA, "s1"))
	store.Delete(originPrefix + "s1")

	pctx := request(store, eteHost, claudeUA, "s1")
	pctx.Body = anthropicBodyWithThinking()
	run(t, build(t, sessionsConfig(`"claude-code": "glm"`, `"s1": "ete"`)), pctx)
	assertAnyRecord(t, pctx, pipeline.ActionModify, "reasoning_stripped", map[string]string{"blocks": "1"})
}

// A session moved to its own provider and back carries that provider's signed
// thinking: the return strips it, whichever server the session returns to.
func TestRouter_AMoveToOwnAndBackStripsTheProvidersReasoning(t *testing.T) {
	for _, back := range []string{"ete", "glm"} {
		t.Run(back, func(t *testing.T) {
			store := newStore(t)
			first := request(store, "api.anthropic.com", claudeUA, "s1")
			first.Body = []byte(`{"model":"claude-opus-5-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
			run(t, build(t, routerConfig(`"claude-code": "ete"`)), first)

			mid := request(store, "api.anthropic.com", claudeUA, "s1")
			mid.Body = anthropicBodyWithThinking()
			run(t, build(t, sessionsConfig(`"claude-code": "ete"`, `"s1": ""`)), mid)
			assertUntouched(t, mid, "api.anthropic.com")
			if v, _ := store.Get(originPrefix + "s1"); v != originForeign {
				t.Errorf("origin after the move to own = %v, want %q", v, originForeign)
			}

			last := request(store, "api.anthropic.com", claudeUA, "s1")
			last.Body = anthropicBodyWithThinking()
			run(t, build(t, sessionsConfig(`"claude-code": "ete"`, `"s1": "`+back+`"`)), last)
			assertAnyRecord(t, last, pipeline.ActionModify, "reasoning_stripped", map[string]string{"blocks": "1"})
		})
	}
}

// Under observe a move to own moves nothing, so it records no origin either.
func TestRouter_UnderObserveAMoveToOwnRecordsNoOrigin(t *testing.T) {
	store := newStore(t)
	first := request(store, eteHost, claudeUA, "s1")
	first.Body = []byte(`{"model":"claude-opus-5-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	run(t, build(t, routerConfig(`"claude-code": "ete"`)), first)

	mid := request(store, eteHost, claudeUA, "s1")
	run(t, build(t, sessionsConfig(`"claude-code": "ete"`, `"s1": ""`), pipeline.WithPolicies(pipeline.ErrorPolicyObserve)), mid)
	if v, _ := store.Get(originPrefix + "s1"); v != "ete" {
		t.Errorf("origin = %v, want ete: observe moves nothing", v)
	}
}
