package opa

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/open-policy-agent/opa/sdk"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// mockDecider implements the decider interface for testing.
type mockDecider struct {
	result *sdk.DecisionResult
	err    error
}

func (m *mockDecider) Decision(_ context.Context, _ sdk.DecisionOptions) (*sdk.DecisionResult, error) {
	return m.result, m.err
}

func (m *mockDecider) Stop(_ context.Context) {}

// undefinedErr returns an *sdk.Error that satisfies sdk.IsUndefinedErr.
func undefinedErr() error {
	return &sdk.Error{Code: "opa_undefined_error", Message: "test undefined"}
}

// capturingDecider records the decision path for verification.
type capturingDecider struct {
	result  *sdk.DecisionResult
	capture *string
}

func (c *capturingDecider) Decision(_ context.Context, opts sdk.DecisionOptions) (*sdk.DecisionResult, error) {
	*c.capture = opts.Path
	return c.result, nil
}

func (c *capturingDecider) Stop(_ context.Context) {}

// --- Configure tests ---

func TestConfigure_MissingBundleURL(t *testing.T) {
	p := &OPA{}
	raw := json.RawMessage(`{}`)
	if err := p.Configure(raw); err == nil {
		t.Fatal("expected error for missing bundle_url")
	}
}

func TestConfigure_UnknownField(t *testing.T) {
	p := &OPA{}
	raw := json.RawMessage(`{"bundle_url":"http://server:8080","bogus":true}`)
	if err := p.Configure(raw); err == nil {
		t.Fatal("expected error for unknown field")
	}
}

func TestConfigure_DefaultAgentIDFile(t *testing.T) {
	p := &OPA{}
	raw := json.RawMessage(`{"bundle_url":"http://server:8080"}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.cfg.AgentIDFile != "/shared/client-id.txt" {
		t.Errorf("expected default agent_id_file, got %q", p.cfg.AgentIDFile)
	}
}

func TestConfigure_InlineAgentID(t *testing.T) {
	p := &OPA{}
	raw := json.RawMessage(`{"bundle_url":"http://server:8080","agent_id":"my-agent"}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.agentID != "my-agent" {
		t.Errorf("expected agent_id my-agent, got %q", p.agentID)
	}
	if p.cfg.AgentIDFile != "" {
		t.Errorf("expected empty agent_id_file when agent_id is set, got %q", p.cfg.AgentIDFile)
	}
}

func TestConfigure_DefaultPollingDelays(t *testing.T) {
	p := &OPA{}
	raw := json.RawMessage(`{"bundle_url":"http://server:8080"}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.cfg.PollingMinDelay != 10 {
		t.Errorf("expected polling_min_delay 10, got %d", p.cfg.PollingMinDelay)
	}
	if p.cfg.PollingMaxDelay != 120 {
		t.Errorf("expected polling_max_delay 120, got %d", p.cfg.PollingMaxDelay)
	}
}

func TestConfigure_CustomPollingDelays(t *testing.T) {
	p := &OPA{}
	raw := json.RawMessage(`{"bundle_url":"http://server:8080","polling_min_delay":5,"polling_max_delay":60}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.cfg.PollingMinDelay != 5 {
		t.Errorf("expected polling_min_delay 5, got %d", p.cfg.PollingMinDelay)
	}
	if p.cfg.PollingMaxDelay != 60 {
		t.Errorf("expected polling_max_delay 60, got %d", p.cfg.PollingMaxDelay)
	}
}

func TestConfigure_IncludeList(t *testing.T) {
	p := &OPA{}
	raw := json.RawMessage(`{"bundle_url":"http://server:8080","include":["a2a.content","mcp.params","inference.messages"]}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !p.inc.has("a2a.content") {
		t.Error("expected include set to contain a2a.content")
	}
	if !p.inc.has("mcp.params") {
		t.Error("expected include set to contain mcp.params")
	}
	if !p.inc.has("inference.messages") {
		t.Error("expected include set to contain inference.messages")
	}
	// Default-on keys
	if !p.inc.has("mcp.params.name") {
		t.Error("expected default-on mcp.params.name")
	}
	if !p.inc.has("mcp.params.uri") {
		t.Error("expected default-on mcp.params.uri")
	}
}

func TestConfigure_DefaultIncludeSet(t *testing.T) {
	p := &OPA{}
	raw := json.RawMessage(`{"bundle_url":"http://server:8080"}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !p.inc.has("mcp.params.name") {
		t.Error("expected default-on mcp.params.name")
	}
	if !p.inc.has("mcp.params.uri") {
		t.Error("expected default-on mcp.params.uri")
	}
	if p.inc.has("a2a.content") {
		t.Error("a2a.content should not be in default include set")
	}
}

// --- Ready tests ---

func TestReady_FalseBeforeInit(t *testing.T) {
	p := &OPA{}
	if p.Ready() {
		t.Fatal("expected Ready() false before Init")
	}
}

// --- Name and Capabilities tests ---

func TestName(t *testing.T) {
	p := &OPA{}
	if p.Name() != "opa" {
		t.Errorf("expected name opa, got %q", p.Name())
	}
}

func TestCapabilities(t *testing.T) {
	p := &OPA{}
	caps := p.Capabilities()

	expectedRequires := []string{"jwt-validation"}
	if len(caps.Requires) != len(expectedRequires) {
		t.Fatalf("expected Requires=%v, got %v", expectedRequires, caps.Requires)
	}
	for i, v := range expectedRequires {
		if caps.Requires[i] != v {
			t.Errorf("Requires[%d]: expected %q, got %q", i, v, caps.Requires[i])
		}
	}

	expectedRequiresAny := []string{"a2a-parser", "mcp-parser", "inference-parser"}
	if len(caps.RequiresAny) != len(expectedRequiresAny) {
		t.Fatalf("expected RequiresAny=%v, got %v", expectedRequiresAny, caps.RequiresAny)
	}
	for i, v := range expectedRequiresAny {
		if caps.RequiresAny[i] != v {
			t.Errorf("RequiresAny[%d]: expected %q, got %q", i, v, caps.RequiresAny[i])
		}
	}

	if caps.Description != "OPA policy enforcement for inbound and outbound requests." {
		t.Errorf("unexpected Description: %q", caps.Description)
	}
}

// --- includeSet tests ---

func TestIncludeSet_DefaultKeys(t *testing.T) {
	s := newIncludeSet(nil)
	if !s.has("mcp.params.name") {
		t.Error("expected default mcp.params.name")
	}
	if !s.has("mcp.params.uri") {
		t.Error("expected default mcp.params.uri")
	}
}

func TestIncludeSet_HasParamKey(t *testing.T) {
	s := newIncludeSet([]string{"mcp.params.metadata"})
	if !s.hasParamKey("name") {
		t.Error("expected hasParamKey(name) true (default)")
	}
	if !s.hasParamKey("metadata") {
		t.Error("expected hasParamKey(metadata) true (configured)")
	}
	if s.hasParamKey("arguments") {
		t.Error("expected hasParamKey(arguments) false")
	}
}

func TestIncludeSet_HasParamKey_FullParams(t *testing.T) {
	s := newIncludeSet([]string{"mcp.params"})
	if !s.hasParamKey("anything") {
		t.Error("expected hasParamKey(anything) true when mcp.params is included")
	}
}

// --- interpretDecision tests ---

func TestInterpretDecision_BoolTrue(t *testing.T) {
	allowed, reason := interpretDecision(&sdk.DecisionResult{Result: true})
	if !allowed || reason != "" {
		t.Errorf("expected allow, got allowed=%v reason=%q", allowed, reason)
	}
}

func TestInterpretDecision_BoolFalse(t *testing.T) {
	allowed, reason := interpretDecision(&sdk.DecisionResult{Result: false})
	if allowed {
		t.Error("expected deny for false")
	}
	if reason != "policy denied" {
		t.Errorf("expected reason 'policy denied', got %q", reason)
	}
}

func TestInterpretDecision_MapAllowTrue(t *testing.T) {
	allowed, _ := interpretDecision(&sdk.DecisionResult{
		Result: map[string]any{"allow": true},
	})
	if !allowed {
		t.Error("expected allow for {allow: true}")
	}
}

func TestInterpretDecision_MapAllowFalseWithReason(t *testing.T) {
	allowed, reason := interpretDecision(&sdk.DecisionResult{
		Result: map[string]any{"allow": false, "reason": "access denied for tool"},
	})
	if allowed {
		t.Error("expected deny")
	}
	if reason != "access denied for tool" {
		t.Errorf("expected reason from policy, got %q", reason)
	}
}

func TestInterpretDecision_MapAllowFalseNoReason(t *testing.T) {
	allowed, reason := interpretDecision(&sdk.DecisionResult{
		Result: map[string]any{"allow": false},
	})
	if allowed {
		t.Error("expected deny")
	}
	if reason != "policy denied" {
		t.Errorf("expected generic reason, got %q", reason)
	}
}

func TestInterpretDecision_NilResult(t *testing.T) {
	allowed, reason := interpretDecision(nil)
	if allowed {
		t.Error("expected deny for nil result")
	}
	if reason != "no decision result" {
		t.Errorf("expected 'no decision result', got %q", reason)
	}
}

func TestInterpretDecision_NilResultField(t *testing.T) {
	allowed, reason := interpretDecision(&sdk.DecisionResult{Result: nil})
	if allowed {
		t.Error("expected deny for nil Result field")
	}
	if reason != "no decision result" {
		t.Errorf("expected 'no decision result', got %q", reason)
	}
}

func TestInterpretDecision_UnexpectedType(t *testing.T) {
	allowed, reason := interpretDecision(&sdk.DecisionResult{Result: "unexpected"})
	if allowed {
		t.Error("expected deny for unexpected type")
	}
	if reason == "" {
		t.Error("expected non-empty reason for unexpected type")
	}
}

func TestInterpretDecision_UnrecognizedMap(t *testing.T) {
	allowed, reason := interpretDecision(&sdk.DecisionResult{
		Result: map[string]any{"foo": "bar"},
	})
	if allowed {
		t.Error("expected deny for unrecognized map shape")
	}
	if reason != "unrecognized policy result shape" {
		t.Errorf("expected 'unrecognized policy result shape', got %q", reason)
	}
}

// --- buildInput tests (lean mode, default include set) ---

func TestBuildInput_Basic(t *testing.T) {
	inc := newIncludeSet(nil)
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "POST",
		Path:      "/api/v1/invoke",
		Host:      "my-service",
		Headers:   http.Header{"Content-Type": {"application/json"}},
	}
	input := buildInput(pctx, inc)
	if input["direction"] != "inbound" {
		t.Errorf("expected inbound, got %v", input["direction"])
	}
	if input["method"] != "POST" {
		t.Errorf("expected POST, got %v", input["method"])
	}
	if input["path"] != "/api/v1/invoke" {
		t.Errorf("expected /api/v1/invoke, got %v", input["path"])
	}
	if input["host"] != "my-service" {
		t.Errorf("expected my-service, got %v", input["host"])
	}
	headers := input["headers"].(map[string]string)
	if headers["content-type"] != "application/json" {
		t.Errorf("expected application/json, got %v", headers["content-type"])
	}
	if _, ok := input["identity"]; ok {
		t.Error("expected no identity when pctx.Identity is nil")
	}
}

type testIdentity struct{}

func (i testIdentity) Subject() string  { return "user-123" }
func (i testIdentity) ClientID() string { return "client-abc" }
func (i testIdentity) Scopes() []string { return []string{"openid", "profile"} }

func TestBuildInput_WithIdentity(t *testing.T) {
	inc := newIncludeSet(nil)
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    "GET",
		Path:      "/data",
		Host:      "backend",
		Headers:   http.Header{},
		Identity:  testIdentity{},
	}
	input := buildInput(pctx, inc)
	id, ok := input["identity"].(map[string]any)
	if !ok {
		t.Fatal("expected identity map")
	}
	if id["subject"] != "user-123" {
		t.Errorf("expected user-123, got %v", id["subject"])
	}
	if id["client_id"] != "client-abc" {
		t.Errorf("expected client-abc, got %v", id["client_id"])
	}
}

func TestBuildInput_WithAgent(t *testing.T) {
	inc := newIncludeSet(nil)
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "GET",
		Path:      "/",
		Host:      "svc",
		Headers:   http.Header{},
		Agent:     &pipeline.AgentIdentity{ClientID: "agent-x"},
	}
	input := buildInput(pctx, inc)
	agent, ok := input["agent"].(map[string]any)
	if !ok {
		t.Fatal("expected agent map")
	}
	if agent["client_id"] != "agent-x" {
		t.Errorf("expected agent-x, got %v", agent["client_id"])
	}
}

// --- A2A input tests ---

func TestBuildInput_A2A_LeanMode(t *testing.T) {
	inc := newIncludeSet(nil)
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "POST",
		Path:      "/",
		Host:      "svc",
		Headers:   http.Header{},
	}
	pctx.Extensions.A2A = &pipeline.A2AExtension{
		Method:       "invoke",
		SessionID:    "sess-1",
		MessageID:    "msg-1",
		TaskID:       "task-1",
		Role:         "user",
		Parts:        []pipeline.A2APart{{Kind: "text", Content: "big prompt text"}},
		Artifact:     "some artifact",
		ErrorMessage: "some error",
	}
	input := buildInput(pctx, inc)
	a2a := input["a2a"].(map[string]any)

	// Always present
	if a2a["method"] != "invoke" {
		t.Errorf("expected method invoke, got %v", a2a["method"])
	}
	if a2a["session_id"] != "sess-1" {
		t.Errorf("expected session_id sess-1, got %v", a2a["session_id"])
	}
	if a2a["task_id"] != "task-1" {
		t.Errorf("expected task_id task-1, got %v", a2a["task_id"])
	}
	if a2a["role"] != "user" {
		t.Errorf("expected role user, got %v", a2a["role"])
	}

	// Removed fields
	if _, ok := a2a["message_id"]; ok {
		t.Error("message_id should not be in lean input")
	}

	// Content fields should be absent in lean mode
	if _, ok := a2a["parts"]; ok {
		t.Error("parts should not be in lean input")
	}
	if _, ok := a2a["artifact"]; ok {
		t.Error("artifact should not be in lean input")
	}
	if _, ok := a2a["error_message"]; ok {
		t.Error("error_message should not be in lean input")
	}
}

func TestBuildInput_A2A_WithContent(t *testing.T) {
	inc := newIncludeSet([]string{"a2a.content"})
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "POST",
		Path:      "/",
		Host:      "svc",
		Headers:   http.Header{},
	}
	pctx.Extensions.A2A = &pipeline.A2AExtension{
		Method:       "invoke",
		SessionID:    "sess-1",
		TaskID:       "task-1",
		Role:         "user",
		Parts:        []pipeline.A2APart{{Kind: "text", Content: "big prompt text"}},
		Artifact:     "artifact data",
		ErrorMessage: "err msg",
	}
	input := buildInput(pctx, inc)
	a2a := input["a2a"].(map[string]any)

	parts, ok := a2a["parts"].([]map[string]any)
	if !ok {
		t.Fatal("expected parts array")
	}
	if len(parts) != 1 || parts[0]["content"] != "big prompt text" {
		t.Errorf("expected parts with content, got %v", parts)
	}
	if parts[0]["kind"] != "text" {
		t.Errorf("expected kind text, got %v", parts[0]["kind"])
	}
	if a2a["artifact"] != "artifact data" {
		t.Errorf("expected artifact, got %v", a2a["artifact"])
	}
	if a2a["error_message"] != "err msg" {
		t.Errorf("expected error_message, got %v", a2a["error_message"])
	}
}

// --- MCP input tests ---

func TestBuildInput_MCP_LeanMode(t *testing.T) {
	inc := newIncludeSet(nil)
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "POST",
		Path:      "/",
		Host:      "svc",
		Headers:   http.Header{},
	}
	pctx.Extensions.MCP = &pipeline.MCPExtension{
		Method: "tools/call",
		RPCID:  1,
		Params: map[string]any{
			"name":      "create_issue",
			"uri":       "file:///main.go",
			"arguments": map[string]any{"title": "Bug", "body": "large body..."},
		},
		Result: map[string]any{"content": "result data"},
		Err:    &pipeline.MCPError{Code: -1, Message: "fail", Data: "detail"},
	}
	input := buildInput(pctx, inc)
	mcp := input["mcp"].(map[string]any)

	if mcp["method"] != "tools/call" {
		t.Errorf("expected method tools/call, got %v", mcp["method"])
	}

	// rpc_id removed
	if _, ok := mcp["rpc_id"]; ok {
		t.Error("rpc_id should not be in input")
	}

	// params should only contain name and uri (defaults)
	params, ok := mcp["params"].(map[string]any)
	if !ok {
		t.Fatal("expected params map")
	}
	if params["name"] != "create_issue" {
		t.Errorf("expected params.name, got %v", params["name"])
	}
	if params["uri"] != "file:///main.go" {
		t.Errorf("expected params.uri, got %v", params["uri"])
	}
	if _, ok := params["arguments"]; ok {
		t.Error("params.arguments should not be in lean input")
	}

	// result and error should be absent
	if _, ok := mcp["result"]; ok {
		t.Error("result should not be in lean input")
	}
	if _, ok := mcp["error"]; ok {
		t.Error("error should not be in lean input")
	}
}

func TestBuildInput_MCP_FullParams(t *testing.T) {
	inc := newIncludeSet([]string{"mcp.params"})
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "POST",
		Path:      "/",
		Host:      "svc",
		Headers:   http.Header{},
	}
	pctx.Extensions.MCP = &pipeline.MCPExtension{
		Method: "tools/call",
		Params: map[string]any{
			"name":      "create_issue",
			"arguments": map[string]any{"title": "Bug"},
		},
	}
	input := buildInput(pctx, inc)
	mcp := input["mcp"].(map[string]any)
	params := mcp["params"].(map[string]any)
	if _, ok := params["arguments"]; !ok {
		t.Error("expected full params including arguments when mcp.params is included")
	}
}

func TestBuildInput_MCP_CustomParamKey(t *testing.T) {
	inc := newIncludeSet([]string{"mcp.params.metadata"})
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "POST",
		Path:      "/",
		Host:      "svc",
		Headers:   http.Header{},
	}
	pctx.Extensions.MCP = &pipeline.MCPExtension{
		Method: "tools/call",
		Params: map[string]any{
			"name":      "read_resource",
			"metadata":  map[string]any{"source": "git"},
			"arguments": map[string]any{"large": "data"},
		},
	}
	input := buildInput(pctx, inc)
	mcp := input["mcp"].(map[string]any)
	params := mcp["params"].(map[string]any)
	if params["name"] != "read_resource" {
		t.Errorf("expected name (default-on), got %v", params["name"])
	}
	if _, ok := params["metadata"]; !ok {
		t.Error("expected metadata (custom include)")
	}
	if _, ok := params["arguments"]; ok {
		t.Error("arguments should not be included")
	}
}

func TestBuildInput_MCP_WithResultAndError(t *testing.T) {
	inc := newIncludeSet([]string{"mcp.result", "mcp.error"})
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "POST",
		Path:      "/",
		Host:      "svc",
		Headers:   http.Header{},
	}
	pctx.Extensions.MCP = &pipeline.MCPExtension{
		Method: "tools/call",
		Params: map[string]any{"name": "tool"},
		Result: map[string]any{"content": "data"},
		Err:    &pipeline.MCPError{Code: -1, Message: "fail", Data: "detail"},
	}
	input := buildInput(pctx, inc)
	mcp := input["mcp"].(map[string]any)
	if _, ok := mcp["result"]; !ok {
		t.Error("expected result when mcp.result is included")
	}
	errMap, ok := mcp["error"].(map[string]any)
	if !ok {
		t.Fatal("expected error map when mcp.error is included")
	}
	if errMap["code"] != -1 {
		t.Errorf("expected error code -1, got %v", errMap["code"])
	}
	if errMap["data"] != "detail" {
		t.Errorf("expected error data, got %v", errMap["data"])
	}
}

// --- Inference input tests ---

func TestBuildInput_Inference_LeanMode(t *testing.T) {
	inc := newIncludeSet(nil)
	maxTokens := 4000
	temp := 0.7
	topP := 0.9
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "POST",
		Path:      "/",
		Host:      "svc",
		Headers:   http.Header{},
	}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{
		Model:       "gpt-4",
		Stream:      true,
		MaxTokens:   &maxTokens,
		Temperature: &temp,
		TopP:        &topP,
		Messages: []pipeline.InferenceMessage{
			{Role: "user", Content: "big conversation history..."},
		},
		Tools: []pipeline.InferenceTool{
			{Name: "create_issue", Description: "Creates issues", Parameters: map[string]any{"type": "object"}},
			{Name: "list_issues", Description: "Lists issues"},
		},
		ToolChoice:       "auto",
		Completion:       "LLM response text",
		FinishReason:     "stop",
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
		ToolCalls: []pipeline.InferenceToolCall{
			{ID: "tc1", Name: "create_issue", Arguments: `{"title":"Bug"}`},
		},
	}
	input := buildInput(pctx, inc)
	inf := input["inference"].(map[string]any)

	// Always present
	if inf["model"] != "gpt-4" {
		t.Errorf("expected model gpt-4, got %v", inf["model"])
	}
	if inf["stream"] != true {
		t.Errorf("expected stream true, got %v", inf["stream"])
	}
	if inf["max_tokens"] != 4000 {
		t.Errorf("expected max_tokens 4000, got %v", inf["max_tokens"])
	}
	// Tools as names-only string array
	tools, ok := inf["tools"].([]string)
	if !ok {
		t.Fatalf("expected tools as []string, got %T", inf["tools"])
	}
	if len(tools) != 2 || tools[0] != "create_issue" || tools[1] != "list_issues" {
		t.Errorf("expected [create_issue, list_issues], got %v", tools)
	}

	// Removed fields
	if _, ok := inf["temperature"]; ok {
		t.Error("temperature should not be in input")
	}
	if _, ok := inf["top_p"]; ok {
		t.Error("top_p should not be in input")
	}
	if _, ok := inf["tool_choice"]; ok {
		t.Error("tool_choice should not be in input")
	}
	if _, ok := inf["finish_reason"]; ok {
		t.Error("finish_reason should not be in input")
	}
	if _, ok := inf["prompt_tokens"]; ok {
		t.Error("prompt_tokens should not be in input")
	}
	if _, ok := inf["completion_tokens"]; ok {
		t.Error("completion_tokens should not be in input")
	}
	if _, ok := inf["total_tokens"]; ok {
		t.Error("total_tokens should not be in input")
	}

	// Content fields should be absent
	if _, ok := inf["messages"]; ok {
		t.Error("messages should not be in lean input")
	}
	if _, ok := inf["completion"]; ok {
		t.Error("completion should not be in lean input")
	}
	if _, ok := inf["tool_calls"]; ok {
		t.Error("tool_calls should not be in lean input")
	}
}

func TestBuildInput_Inference_WithMessages(t *testing.T) {
	inc := newIncludeSet([]string{"inference.messages"})
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "POST",
		Path:      "/",
		Host:      "svc",
		Headers:   http.Header{},
	}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{
		Model: "gpt-4",
		Messages: []pipeline.InferenceMessage{
			{Role: "user", Content: "Hello"},
			{Role: "assistant", Content: "Hi there"},
		},
	}
	input := buildInput(pctx, inc)
	inf := input["inference"].(map[string]any)
	messages, ok := inf["messages"].([]map[string]any)
	if !ok {
		t.Fatal("expected messages array")
	}
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
	if messages[0]["role"] != "user" || messages[0]["content"] != "Hello" {
		t.Errorf("unexpected first message: %v", messages[0])
	}
}

func TestBuildInput_Inference_WithToolsDetail(t *testing.T) {
	inc := newIncludeSet([]string{"inference.tools.detail"})
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "POST",
		Path:      "/",
		Host:      "svc",
		Headers:   http.Header{},
	}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{
		Model: "gpt-4",
		Tools: []pipeline.InferenceTool{
			{Name: "create_issue", Description: "Creates issues", Parameters: map[string]any{"type": "object"}},
		},
	}
	input := buildInput(pctx, inc)
	inf := input["inference"].(map[string]any)
	tools, ok := inf["tools"].([]map[string]any)
	if !ok {
		t.Fatalf("expected tools as []map[string]any with detail, got %T", inf["tools"])
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0]["name"] != "create_issue" {
		t.Errorf("expected name create_issue, got %v", tools[0]["name"])
	}
	if tools[0]["description"] != "Creates issues" {
		t.Errorf("expected description, got %v", tools[0]["description"])
	}
	if _, ok := tools[0]["parameters"]; !ok {
		t.Error("expected parameters in detailed tools")
	}
}

func TestBuildInput_Inference_WithCompletion(t *testing.T) {
	inc := newIncludeSet([]string{"inference.completion"})
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "POST",
		Path:      "/",
		Host:      "svc",
		Headers:   http.Header{},
	}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{
		Model:      "gpt-4",
		Completion: "The answer is 42",
	}
	input := buildInput(pctx, inc)
	inf := input["inference"].(map[string]any)
	if inf["completion"] != "The answer is 42" {
		t.Errorf("expected completion, got %v", inf["completion"])
	}
}

func TestBuildInput_Inference_WithToolCalls(t *testing.T) {
	inc := newIncludeSet([]string{"inference.tool_calls"})
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "POST",
		Path:      "/",
		Host:      "svc",
		Headers:   http.Header{},
	}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{
		Model: "gpt-4",
		ToolCalls: []pipeline.InferenceToolCall{
			{ID: "tc1", Name: "create_issue", Arguments: `{"title":"Bug"}`},
		},
	}
	input := buildInput(pctx, inc)
	inf := input["inference"].(map[string]any)
	tcs, ok := inf["tool_calls"].([]map[string]any)
	if !ok {
		t.Fatal("expected tool_calls array")
	}
	if len(tcs) != 1 || tcs[0]["name"] != "create_issue" {
		t.Errorf("unexpected tool_calls: %v", tcs)
	}
	if tcs[0]["arguments"] != `{"title":"Bug"}` {
		t.Errorf("expected arguments, got %v", tcs[0]["arguments"])
	}
	if tcs[0]["id"] != "tc1" {
		t.Errorf("expected id tc1, got %v", tcs[0]["id"])
	}
}

// --- flattenHeaders tests ---

func TestFlattenHeaders_Nil(t *testing.T) {
	if flattenHeaders(nil) != nil {
		t.Error("expected nil for nil headers")
	}
}

func TestFlattenHeaders_MultiValue(t *testing.T) {
	h := http.Header{"Accept": {"text/html", "application/json"}}
	flat := flattenHeaders(h)
	if flat["accept"] != "text/html,application/json" {
		t.Errorf("expected joined values, got %q", flat["accept"])
	}
}

// --- OnRequest tests (with mock decider) ---

func TestOnRequest_NotReady(t *testing.T) {
	p := &OPA{}
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "GET",
		Path:      "/",
		Host:      "svc",
		Headers:   http.Header{},
	}
	pctx.SetCurrentPlugin("opa", "request")
	defer pctx.ClearCurrentPlugin()
	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Reject {
		t.Fatal("expected reject when decider is nil")
	}
	if action.Violation.Status != 503 {
		t.Errorf("expected 503, got %d", action.Violation.Status)
	}
}

func TestOnRequest_Allow(t *testing.T) {
	var dec decider = &mockDecider{result: &sdk.DecisionResult{Result: true}}
	p := &OPA{
		inc: newIncludeSet(nil),
	}
	p.decider.Store(&dec)
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "GET",
		Path:      "/",
		Host:      "svc",
		Headers:   http.Header{},
	}
	pctx.SetCurrentPlugin("opa", "request")
	defer pctx.ClearCurrentPlugin()
	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatal("expected continue for allow decision")
	}
}

func TestOnRequest_Deny(t *testing.T) {
	var dec decider = &mockDecider{result: &sdk.DecisionResult{Result: false}}
	p := &OPA{
		inc: newIncludeSet(nil),
	}
	p.decider.Store(&dec)
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "GET",
		Path:      "/secret",
		Host:      "svc",
		Headers:   http.Header{},
	}
	pctx.SetCurrentPlugin("opa", "request")
	defer pctx.ClearCurrentPlugin()
	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Reject {
		t.Fatal("expected reject for deny decision")
	}
	if action.Violation.Code != "policy.forbidden" {
		t.Errorf("expected policy.forbidden, got %q", action.Violation.Code)
	}
}

func TestOnRequest_DecisionError(t *testing.T) {
	var dec decider = &mockDecider{err: fmt.Errorf("bundle not loaded")}
	p := &OPA{
		inc: newIncludeSet(nil),
	}
	p.decider.Store(&dec)
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "GET",
		Path:      "/",
		Host:      "svc",
		Headers:   http.Header{},
	}
	pctx.SetCurrentPlugin("opa", "request")
	defer pctx.ClearCurrentPlugin()
	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Reject {
		t.Fatal("expected reject on decision error")
	}
}

func TestOnRequest_UndefinedSkips(t *testing.T) {
	var dec decider = &mockDecider{err: undefinedErr()}
	p := &OPA{
		inc: newIncludeSet(nil),
	}
	p.decider.Store(&dec)
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "GET",
		Path:      "/",
		Host:      "svc",
		Headers:   http.Header{},
	}
	pctx.SetCurrentPlugin("opa", "request")
	defer pctx.ClearCurrentPlugin()
	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatal("expected continue (skip) for undefined path")
	}
}

func TestOnRequest_OutboundUsesCorrectPath(t *testing.T) {
	var capturedPath string
	var dec decider = &capturingDecider{
		result:  &sdk.DecisionResult{Result: true},
		capture: &capturedPath,
	}
	p := &OPA{
		inc: newIncludeSet(nil),
	}
	p.decider.Store(&dec)
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    "GET",
		Path:      "/api",
		Host:      "external",
		Headers:   http.Header{},
	}
	pctx.SetCurrentPlugin("opa", "request")
	defer pctx.ClearCurrentPlugin()
	p.OnRequest(context.Background(), pctx)
	if capturedPath != "authbridge/outbound/request" {
		t.Errorf("expected outbound/request path, got %q", capturedPath)
	}
}

func TestOnRequest_InboundUsesCorrectPath(t *testing.T) {
	var capturedPath string
	var dec decider = &capturingDecider{
		result:  &sdk.DecisionResult{Result: true},
		capture: &capturedPath,
	}
	p := &OPA{
		inc: newIncludeSet(nil),
	}
	p.decider.Store(&dec)
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "POST",
		Path:      "/invoke",
		Host:      "agent",
		Headers:   http.Header{},
	}
	pctx.SetCurrentPlugin("opa", "request")
	defer pctx.ClearCurrentPlugin()
	p.OnRequest(context.Background(), pctx)
	if capturedPath != "authbridge/inbound/request" {
		t.Errorf("expected inbound/request path, got %q", capturedPath)
	}
}

// --- OnResponse tests ---

func TestOnResponse_NotReady(t *testing.T) {
	p := &OPA{}
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "GET",
		Path:      "/",
		Host:      "svc",
		Headers:   http.Header{},
	}
	pctx.SetCurrentPlugin("opa", "response")
	defer pctx.ClearCurrentPlugin()
	action := p.OnResponse(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatal("expected continue (skip) when decider is nil on response path")
	}
}

func TestOnResponse_Allow(t *testing.T) {
	var dec decider = &mockDecider{result: &sdk.DecisionResult{Result: true}}
	p := &OPA{
		inc: newIncludeSet(nil),
	}
	p.decider.Store(&dec)
	pctx := &pipeline.Context{
		Direction:       pipeline.Inbound,
		Method:          "GET",
		Path:            "/",
		Host:            "svc",
		Headers:         http.Header{},
		StatusCode:      200,
		ResponseHeaders: http.Header{"Content-Type": {"application/json"}},
	}
	pctx.SetCurrentPlugin("opa", "response")
	defer pctx.ClearCurrentPlugin()
	action := p.OnResponse(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatal("expected continue for allow decision on response")
	}
}

func TestOnResponse_Deny(t *testing.T) {
	var dec decider = &mockDecider{result: &sdk.DecisionResult{Result: false}}
	p := &OPA{
		inc: newIncludeSet(nil),
	}
	p.decider.Store(&dec)
	pctx := &pipeline.Context{
		Direction:  pipeline.Inbound,
		Method:     "GET",
		Path:       "/",
		Host:       "svc",
		Headers:    http.Header{},
		StatusCode: 200,
	}
	pctx.SetCurrentPlugin("opa", "response")
	defer pctx.ClearCurrentPlugin()
	action := p.OnResponse(context.Background(), pctx)
	if action.Type != pipeline.Reject {
		t.Fatal("expected reject for deny decision on response")
	}
}

func TestOnResponse_UndefinedSkips(t *testing.T) {
	var dec decider = &mockDecider{err: undefinedErr()}
	p := &OPA{
		inc: newIncludeSet(nil),
	}
	p.decider.Store(&dec)
	pctx := &pipeline.Context{
		Direction:  pipeline.Outbound,
		Method:     "GET",
		Path:       "/",
		Host:       "svc",
		Headers:    http.Header{},
		StatusCode: 200,
	}
	pctx.SetCurrentPlugin("opa", "response")
	defer pctx.ClearCurrentPlugin()
	action := p.OnResponse(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatal("expected continue (skip) for undefined response path")
	}
}

func TestOnResponse_OutboundUsesCorrectPath(t *testing.T) {
	var capturedPath string
	var dec decider = &capturingDecider{
		result:  &sdk.DecisionResult{Result: true},
		capture: &capturedPath,
	}
	p := &OPA{
		inc: newIncludeSet(nil),
	}
	p.decider.Store(&dec)
	pctx := &pipeline.Context{
		Direction:  pipeline.Outbound,
		Method:     "GET",
		Path:       "/data",
		Host:       "external",
		Headers:    http.Header{},
		StatusCode: 200,
	}
	pctx.SetCurrentPlugin("opa", "response")
	defer pctx.ClearCurrentPlugin()
	p.OnResponse(context.Background(), pctx)
	if capturedPath != "authbridge/outbound/response" {
		t.Errorf("expected outbound/response path, got %q", capturedPath)
	}
}

// --- buildOPAConfig tests ---

func TestBuildOPAConfig(t *testing.T) {
	p := &OPA{
		cfg: opaConfig{
			BundleURL:       "http://bundle-server:8080",
			PollingMinDelay: 10,
			PollingMaxDelay: 120,
		},
		agentID: "my-agent",
	}
	data, agentID, err := p.buildOPAConfig()
	if err != nil {
		t.Fatalf("buildOPAConfig failed: %v", err)
	}
	if agentID != "my-agent" {
		t.Errorf("expected agentID 'my-agent', got %q", agentID)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	services := cfg["services"].(map[string]any)
	kagenti := services["kagenti"].(map[string]any)
	if kagenti["url"] != "http://bundle-server:8080" {
		t.Errorf("expected bundle URL, got %v", kagenti["url"])
	}
	bundles := cfg["bundles"].(map[string]any)
	authz := bundles["authz"].(map[string]any)
	if authz["resource"] != "bundles/my-agent.tar.gz" {
		t.Errorf("expected bundles/my-agent.tar.gz, got %v", authz["resource"])
	}
}
func TestBuildOPAConfig_PathEscaping(t *testing.T) {
	tests := []struct {
		name             string
		agentID          string
		wantErr          bool
		wantResourcePath string
	}{
		{
			name:             "valid agentID",
			agentID:          "my-agent",
			wantErr:          false,
			wantResourcePath: "bundles/my-agent.tar.gz",
		},
		{
			name:             "agentID with forward slash gets escaped",
			agentID:          "../evil",
			wantErr:          false,
			wantResourcePath: "bundles/..%2Fevil.tar.gz",
		},
		{
			name:             "agentID with backslash gets escaped",
			agentID:          "..\\evil",
			wantErr:          false,
			wantResourcePath: "bundles/..%5Cevil.tar.gz",
		},
		{
			name:             "agentID with special chars",
			agentID:          "agent@domain.com",
			wantErr:          false,
			wantResourcePath: "bundles/agent@domain.com.tar.gz",
		},
		{
			name:    "empty agentID",
			agentID: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &OPA{
				cfg: opaConfig{
					BundleURL:       "http://bundle-server:8080",
					PollingMinDelay: 10,
					PollingMaxDelay: 120,
				},
				agentID: tt.agentID,
			}
			data, _, err := p.buildOPAConfig()
			if (err != nil) != tt.wantErr {
				t.Errorf("buildOPAConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err == nil && tt.wantResourcePath != "" {
				var cfg map[string]any
				if err := json.Unmarshal(data, &cfg); err != nil {
					t.Fatalf("invalid JSON: %v", err)
				}
				bundles := cfg["bundles"].(map[string]any)
				authz := bundles["authz"].(map[string]any)
				if authz["resource"] != tt.wantResourcePath {
					t.Errorf("expected resource path %q, got %q", tt.wantResourcePath, authz["resource"])
				}
			}
		})
	}
}
