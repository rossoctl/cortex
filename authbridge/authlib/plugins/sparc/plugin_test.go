package sparc

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
)

type fakeReflector struct {
	verdict ReflectVerdict
	err     error
	gotIn   ReflectInput
	calls   int
}

func (f *fakeReflector) Reflect(_ context.Context, in ReflectInput) (ReflectVerdict, error) {
	f.calls++
	f.gotIn = in
	return f.verdict, f.err
}

func configured(t *testing.T, cfgJSON string, fr *fakeReflector) *SPARC {
	t.Helper()
	p := NewSPARC()
	if cfgJSON == "" {
		cfgJSON = `{"reflector_endpoint":"http://sparc.invalid","timeout_ms":1000}`
	}
	if err := p.Configure(json.RawMessage(cfgJSON)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if fr != nil {
		p.reflector = fr
	}
	return p
}

func invokeReq(p pipeline.Plugin, pctx *pipeline.Context) pipeline.Action {
	pctx.SetCurrentPlugin(p.Name(), pipeline.InvocationPhaseRequest)
	defer pctx.ClearCurrentPlugin()
	return p.OnRequest(context.Background(), pctx)
}

func invokeResp(p pipeline.Plugin, pctx *pipeline.Context) pipeline.Action {
	pctx.SetCurrentPlugin(p.Name(), pipeline.InvocationPhaseResponse)
	defer pctx.ClearCurrentPlugin()
	return p.OnResponse(context.Background(), pctx)
}

// inferenceEvent builds a session inference request event (the generic source
// of conversation + tool specs).
func inferenceEvent() pipeline.SessionEvent {
	return pipeline.SessionEvent{
		Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Inference: &pipeline.InferenceExtension{
			Model: "llama3.2:3b",
			Messages: []pipeline.InferenceMessage{
				{Role: "system", Content: "You are a finance assistant."},
				{Role: "user", Content: "Refund my duplicate charge."},
			},
			Tools: []pipeline.InferenceTool{
				{Name: "issue_refund", Description: "Issue a refund", Parameters: map[string]any{"type": "object"}},
			},
		},
	}
}

// mcpPctx builds an outbound pctx for mcp mode: an action-classified MCP
// tools/call plus a session carrying one inference request event.
func mcpPctx() *pipeline.Context {
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound, Method: "POST", Scheme: "http",
		Host: "finance-mcp.team1.svc", Path: "/mcp", Headers: http.Header{},
		Session: &pipeline.SessionView{ID: "s1", Events: []pipeline.SessionEvent{inferenceEvent()}},
	}
	pctx.Extensions.MCP = &pipeline.MCPExtension{
		Method: "tools/call", RPCID: "rpc-1", IsAction: true,
		Params: map[string]any{"name": "issue_refund", "arguments": map[string]any{"transaction_id": "TX9999"}},
	}
	return pctx
}

// inferencePctx builds a pctx for inference mode: the LLM response carries a
// proposed tool call plus the request messages+tools.
func inferencePctx() *pipeline.Context {
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound, Method: "POST", Scheme: "http",
		Host: "ollama", Path: "/v1/chat/completions", Headers: http.Header{},
		ResponseBody: []byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"issue_refund","arguments":"{\"transaction_id\":\"TX9999\"}"}}]},"finish_reason":"tool_calls"}]}`),
	}
	inf := inferenceEvent().Inference
	inf.ToolCalls = []pipeline.InferenceToolCall{{ID: "c1", Name: "issue_refund", Arguments: `{"transaction_id":"TX9999"}`}}
	pctx.Extensions.Inference = inf
	return pctx
}

func outInvs(pctx *pipeline.Context) []pipeline.Invocation {
	if pctx.Extensions.Invocations == nil {
		return nil
	}
	return pctx.Extensions.Invocations.Outbound
}

// --- Configure ---

func TestConfigure_RequiresEndpoint(t *testing.T) {
	if err := NewSPARC().Configure(json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error when reflector_endpoint missing")
	}
}

func TestConfigure_Defaults(t *testing.T) {
	p := configured(t, `{"reflector_endpoint":"http://x"}`, nil)
	if p.cfg.Enforcement != EnforcementMCP || p.cfg.Track != "fast_track" ||
		p.cfg.OnRejectAction != OnRejectReflect || p.cfg.FailPolicy != FailOpen || p.cfg.TimeoutMs != 30000 {
		t.Fatalf("defaults not applied: %+v", p.cfg)
	}
}

func TestConfigure_RejectsBadEnums(t *testing.T) {
	for _, bad := range []string{
		`{"reflector_endpoint":"http://x","enforcement":"nope"}`,
		`{"reflector_endpoint":"http://x","on_reject_action":"nope"}`,
		`{"reflector_endpoint":"http://x","fail_policy":"maybe"}`,
		`{"reflector_endpoint":"http://x","deny_score_threshold":9}`,
		`{"reflector_endpoint":"http://x","bogus":1}`,
	} {
		if err := NewSPARC().Configure(json.RawMessage(bad)); err == nil {
			t.Errorf("expected error for %s", bad)
		}
	}
}

// --- mcp mode ---

func TestMCP_ApproveContinues(t *testing.T) {
	score := 4.5
	fr := &fakeReflector{verdict: ReflectVerdict{Decision: DecisionApprove, OverallAvgScore: &score}}
	p := configured(t, "", fr)
	pctx := mcpPctx()
	if act := invokeReq(p, pctx); act.Type != pipeline.Continue {
		t.Fatalf("approve should Continue, got %+v", act)
	}
	// generic collection: system prompt + tools + tool_call all forwarded
	if len(fr.gotIn.Messages) != 2 || fr.gotIn.Messages[0]["role"] != "system" {
		t.Errorf("expected system+user messages, got %+v", fr.gotIn.Messages)
	}
	if len(fr.gotIn.ToolSpecs) != 1 || len(fr.gotIn.ToolCalls) != 1 {
		t.Errorf("expected tool_specs + tool_call, got %+v", fr.gotIn)
	}
	if fn := fr.gotIn.ToolCalls[0]["function"].(map[string]any); fn["name"] != "issue_refund" {
		t.Errorf("tool_call name = %v", fn["name"])
	}
	if i := outInvs(pctx); len(i) != 1 || i[0].Action != pipeline.ActionAllow {
		t.Errorf("expected one allow, got %+v", i)
	}
}

func TestMCP_RejectReflects(t *testing.T) {
	fr := &fakeReflector{verdict: ReflectVerdict{Decision: DecisionReject, Issues: []ReflectIssue{{Explanation: "TX9999 not grounded"}}}}
	p := configured(t, `{"reflector_endpoint":"http://x","on_reject_action":"reflect"}`, fr)
	pctx := mcpPctx()
	act := invokeReq(p, pctx)
	if act.Type != pipeline.Reject || act.Violation == nil || act.Violation.Status != 200 {
		t.Fatalf("reflect should be Reject w/ 200 MCP result, got %+v", act)
	}
	var env map[string]any
	if err := json.Unmarshal(act.Violation.Body, &env); err != nil || env["result"] == nil {
		t.Fatalf("body is not an MCP result: %v / %v", err, env)
	}
	if i := outInvs(pctx); len(i) != 1 || i[0].Action != pipeline.ActionModify {
		t.Errorf("expected modify, got %+v", i)
	}
	// structured event emitted
	if _, ok := pctx.Extensions.Custom["sparc"+pipeline.PluginEventSuffix]; !ok {
		t.Errorf("expected sparc event in Custom")
	}
}

func TestMCP_RejectObserveContinues(t *testing.T) {
	fr := &fakeReflector{verdict: ReflectVerdict{Decision: DecisionReject, Issues: []ReflectIssue{{Explanation: "x"}}}}
	p := configured(t, `{"reflector_endpoint":"http://x","on_reject_action":"observe"}`, fr)
	pctx := mcpPctx()
	if act := invokeReq(p, pctx); act.Type != pipeline.Continue {
		t.Fatalf("observe should Continue, got %+v", act)
	}
	if i := outInvs(pctx); len(i) != 1 || i[0].Action != pipeline.ActionObserve {
		t.Errorf("expected observe, got %+v", i)
	}
}

func TestMCP_RejectDeny(t *testing.T) {
	fr := &fakeReflector{verdict: ReflectVerdict{Decision: DecisionReject, Issues: []ReflectIssue{{Explanation: "blocked"}}}}
	p := configured(t, `{"reflector_endpoint":"http://x","on_reject_action":"deny"}`, fr)
	act := invokeReq(p, mcpPctx())
	if act.Type != pipeline.Reject || act.Violation == nil || act.Violation.Code != "sparc.blocked" {
		t.Fatalf("deny should Reject sparc.blocked, got %+v", act)
	}
}

func TestMCP_ScoreEscalatesToDeny(t *testing.T) {
	low := 0.5
	fr := &fakeReflector{verdict: ReflectVerdict{Decision: DecisionReject, OverallAvgScore: &low, Issues: []ReflectIssue{{Explanation: "severe"}}}}
	p := configured(t, `{"reflector_endpoint":"http://x","on_reject_action":"reflect","deny_score_threshold":0.7}`, fr)
	act := invokeReq(p, mcpPctx())
	if act.Type != pipeline.Reject || act.Violation == nil || act.Violation.Code != "sparc.blocked" {
		t.Fatalf("low score should escalate to deny, got %+v", act)
	}
}

func TestMCP_SkipTools(t *testing.T) {
	fr := &fakeReflector{verdict: ReflectVerdict{Decision: DecisionReject}}
	p := configured(t, `{"reflector_endpoint":"http://x","skip_tools":["issue_*"]}`, fr)
	if act := invokeReq(p, mcpPctx()); act.Type != pipeline.Continue {
		t.Fatalf("skipped tool should Continue, got %+v", act)
	}
	if fr.calls != 0 {
		t.Errorf("reflector should not be called for skipped tool")
	}
}

func TestMCP_ReflectToolsAllowlist(t *testing.T) {
	fr := &fakeReflector{verdict: ReflectVerdict{Decision: DecisionApprove}}
	p := configured(t, `{"reflector_endpoint":"http://x","reflect_tools":["get_*"]}`, fr)
	// issue_refund not in allow-list → skipped
	if act := invokeReq(p, mcpPctx()); act.Type != pipeline.Continue || fr.calls != 0 {
		t.Fatalf("non-allowlisted tool should be skipped, got act=%+v calls=%d", act, fr.calls)
	}
}

func TestMCP_NoInferenceContextFailsOpen(t *testing.T) {
	fr := &fakeReflector{verdict: ReflectVerdict{Decision: DecisionReject}}
	p := configured(t, "", fr) // default fail_policy: open
	pctx := mcpPctx()
	pctx.Session = &pipeline.SessionView{ID: "s1"} // no inference event
	if act := invokeReq(p, pctx); act.Type != pipeline.Continue {
		t.Fatalf("no context (fail_open) should Continue, got %+v", act)
	}
	if fr.calls != 0 {
		t.Errorf("reflector should not be called without context")
	}
}

func TestMCP_NoInferenceContextFailsClosed(t *testing.T) {
	fr := &fakeReflector{verdict: ReflectVerdict{Decision: DecisionReject}}
	p := configured(t, `{"reflector_endpoint":"http://x","fail_policy":"closed"}`, fr)
	pctx := mcpPctx()
	pctx.Session = &pipeline.SessionView{ID: "s1"} // no inference event
	act := invokeReq(p, pctx)
	if act.Type != pipeline.Reject || act.Violation == nil || act.Violation.Status != 503 {
		t.Fatalf("no context (fail_closed) should 503, got %+v", act)
	}
	if fr.calls != 0 {
		t.Errorf("reflector should not be called without context")
	}
}

func TestMCP_NotAToolCallSkips(t *testing.T) {
	fr := &fakeReflector{}
	p := configured(t, "", fr)
	pctx := mcpPctx()
	pctx.Extensions.MCP.Method = "prompts/get"
	if act := invokeReq(p, pctx); act.Type != pipeline.Continue || fr.calls != 0 {
		t.Fatalf("non tools/call should skip, got act=%+v calls=%d", act, fr.calls)
	}
}

func TestMCP_Reentrancy(t *testing.T) {
	fr := &fakeReflector{verdict: ReflectVerdict{Decision: DecisionReject}}
	p := configured(t, "", fr)
	pctx := mcpPctx()
	pctx.Headers.Set(sentinelHeader, "1")
	if act := invokeReq(p, pctx); act.Type != pipeline.Continue || fr.calls != 0 {
		t.Fatalf("reentrant call should Continue without reflecting")
	}
}

func TestMCP_FailOpenAndClosed(t *testing.T) {
	frOpen := &fakeReflector{err: ErrReflectorUnavailable}
	if act := invokeReq(configured(t, `{"reflector_endpoint":"http://x","fail_policy":"open"}`, frOpen), mcpPctx()); act.Type != pipeline.Continue {
		t.Fatalf("fail_open should Continue, got %+v", act)
	}
	frClosed := &fakeReflector{err: ErrReflectorUnavailable}
	act := invokeReq(configured(t, `{"reflector_endpoint":"http://x","fail_policy":"closed"}`, frClosed), mcpPctx())
	if act.Type != pipeline.Reject || act.Violation.Status != 503 {
		t.Fatalf("fail_closed should 503, got %+v", act)
	}
}

// --- inference mode ---

func TestInference_RejectRewritesResponse(t *testing.T) {
	fr := &fakeReflector{verdict: ReflectVerdict{Decision: DecisionReject, Issues: []ReflectIssue{{Explanation: "ungrounded id"}}}}
	p := configured(t, `{"reflector_endpoint":"http://x","enforcement":"inference","on_reject_action":"reflect"}`, fr)
	pctx := inferencePctx()
	if act := invokeResp(p, pctx); act.Type != pipeline.Continue {
		t.Fatalf("inference reject should Continue (body rewritten), got %+v", act)
	}
	// response body rewritten: tool_calls dropped, clarification in content
	var out map[string]any
	if err := json.Unmarshal(pctx.ResponseBody, &out); err != nil {
		t.Fatalf("rewritten body invalid: %v", err)
	}
	msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, hasTC := msg["tool_calls"]; hasTC {
		t.Errorf("tool_calls should be dropped, got %v", msg)
	}
	if c, _ := msg["content"].(string); c == "" {
		t.Errorf("clarification content missing")
	}
	// generic collection: all 3 inputs came straight from the inference ext
	if len(fr.gotIn.Messages) != 2 || len(fr.gotIn.ToolSpecs) != 1 || len(fr.gotIn.ToolCalls) != 1 {
		t.Errorf("expected 3 inputs co-located, got %+v", fr.gotIn)
	}
}

func TestInference_ApproveLeavesResponse(t *testing.T) {
	fr := &fakeReflector{verdict: ReflectVerdict{Decision: DecisionApprove}}
	p := configured(t, `{"reflector_endpoint":"http://x","enforcement":"inference"}`, fr)
	pctx := inferencePctx()
	orig := string(pctx.ResponseBody)
	invokeResp(p, pctx)
	if string(pctx.ResponseBody) != orig {
		t.Errorf("approve should not modify the response")
	}
}

func TestInference_MCPModeOnResponseIsNoop(t *testing.T) {
	fr := &fakeReflector{verdict: ReflectVerdict{Decision: DecisionReject}}
	p := configured(t, "", fr) // mcp mode
	if act := invokeResp(p, inferencePctx()); act.Type != pipeline.Continue || fr.calls != 0 {
		t.Fatalf("mcp-mode OnResponse should be a no-op")
	}
}

func TestCapabilities(t *testing.T) {
	caps := NewSPARC().Capabilities()
	if !caps.WritesBody || !caps.ReadsBody {
		t.Error("expected ReadsBody+WritesBody")
	}
	if len(caps.RequiresAny) == 0 {
		t.Error("expected RequiresAny parsers")
	}
}
