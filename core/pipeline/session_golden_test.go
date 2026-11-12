package pipeline

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

// goldenEvent sets every field of SessionEvent and of the structs below it, so that
// every key of the wire form is in the golden file. unsetFields checks this.
func goldenEvent() SessionEvent {
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	temp, topP, maxTokens := 0.7, 0.9, 4096
	thinking := func(n int) *InferenceThinking {
		return &InferenceThinking{Blocks: n, Redacted: 1, TextBytes: 300, SignatureBytes: 120, RedactedBytes: 40}
	}
	return SessionEvent{
		SessionID: "s1", Seq: 7, At: at, Direction: Outbound, Phase: SessionResponse, RequestID: "req-7",
		A2A: &A2AExtension{
			Method: "message/send", RPCID: "a2a-1", SessionID: "ctx-1", MessageID: "msg-1", TaskID: "task-1",
			Role: "user", Parts: []A2APart{{Kind: "text", Content: "hi"}},
			FinalStatus: "failed", Artifact: "report", ErrorMessage: "tool failed", IsAction: true,
		},
		MCP: &MCPExtension{
			Method: "tools/call", RPCID: "7", Params: map[string]any{"name": "get_weather"},
			Result: map[string]any{"content": "sunny"}, Err: &MCPError{Code: -32000, Message: "rate limited", Data: "retry"},
			IsAction: true,
		},
		Inference: &InferenceExtension{
			Model: "claude-opus-5",
			Messages: []InferenceMessage{{
				Role: "user", Content: "q", ContentBytes: 42, ToolUseID: "toolu_1",
				ToolCallNames: "get_weather", ToolUseIDs: "toolu_0",
			}},
			Temperature: &temp, MaxTokens: &maxTokens, TopP: &topP, Stream: true,
			Tools:          []InferenceTool{{Name: "get_weather", Description: "weather", Parameters: `{"type":"object"}`}},
			ToolChoice:     "auto",
			RequestedModel: "opus", ServedModel: "claude-opus-5-20261101",
			ToolResults:      []InferenceToolResult{{ToolUseID: "toolu_1", Content: "sunny", IsError: true}},
			HistoryToolCalls: []InferenceToolCall{{ID: "toolu_0", Name: "get_weather", Arguments: `{"city":"Rome"}`}},
			ThinkingType:     "adaptive", ThinkingDisplay: "summarized", Effort: "high", AnthropicBeta: "context-1m",
			HistoryThinking: thinking(2), ResponseThinking: thinking(1),
			MessageCount: 3, ToolCount: 1, ToolCallCount: 1, AgentRole: AgentRoleSubagent, StreamedResponse: true,
			Completion: "done", FinishReason: "tool_use",
			ToolCalls:    []InferenceToolCall{{ID: "toolu_2", Name: "get_weather", Arguments: `{"city":"Oslo"}`}},
			PromptTokens: 1000, CompletionTokens: 200, TotalTokens: 1200, InputTokens: 100,
			CacheReadTokens: 800, CacheWriteTokens: 100, OutputTokens: 200, ReasoningTokens: 50,
			PresentKinds: 3, IsAction: true,
		},
		Invocations: &Invocations{
			Inbound: []Invocation{{
				Plugin: "jwt-validation", Action: ActionAllow, Phase: InvocationPhaseRequest, Reason: "valid",
				Path: "/v1/messages", Details: map[string]string{"iss": "kc"}, Shadow: true, Late: true,
			}},
			Outbound: []Invocation{{
				Plugin: "token-exchange", Action: ActionModify, Phase: InvocationPhaseResponse, Reason: "exchanged",
				Path: "/v1/messages", Details: map[string]string{"aud": "x"}, Shadow: true, Late: true,
			}},
		},
		Plugins:         map[string]json.RawMessage{"cost": json.RawMessage(`{"usd":0.01}`)},
		Identity:        &EventIdentity{Subject: "alice", Scopes: []string{"openid"}, ClientID: "agent", AgentID: "a1"},
		StatusCode:      503,
		Error:           &EventError{Kind: "upstream", Code: "503", Message: "unavailable"},
		Host:            "api.example:443",
		RequestedHost:   "asked.example:443",
		Duration:        1500 * time.Millisecond,
		Ttfb:            200 * time.Millisecond,
		TLS:             &EventTLS{Version: "TLS 1.3", CipherSuite: "TLS_AES_128_GCM_SHA256", PeerSPIFFEID: "spiffe://example/agent"},
		Tunnel:          true,
		TunnelReason:    TunnelClientRejectedCA,
		BytesUp:         100,
		BytesDown:       2000,
		HTTPMethod:      "POST",
		HTTPPath:        "/v1/messages",
		UpgradeStripped: "websocket",
		Client:          &EventClient{Name: "claude-code", Version: "2.1.14", Raw: "claude-cli/2.1.14"},
		Subagent:        "a1a2e36cb5946f31c",
		ParentSubagent:  "a0974b0b8591dc76b",
		SubagentKind:    SubagentKindSubtask,
		Proc:            "/bin/claude",
		ClientProc:      "/bin/zsh",
		SessionHeader:   "x-session-id",
		DialAddr:        "203.0.113.7:443",
	}
}

// The event wire form is a contract with agentop and the session archive. A change to
// it changes the golden file, and so it shows in review. Run with -update to rewrite it.
func TestSessionEventWire_Golden(t *testing.T) {
	ev := goldenEvent()
	for _, path := range unsetFields(reflect.ValueOf(ev), "SessionEvent") {
		t.Errorf("goldenEvent leaves %s unset: set it, then run with -update", path)
	}
	b, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	const path = "testdata/session-event.golden.json"
	if *update {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to write it): %v", err)
	}
	if !bytes.Equal(b, golden) {
		t.Errorf("wire form differs from %s (run with -update if the change is deliberate)\n got: %s\nwant: %s", path, b, golden)
	}
	// Decode the compact form: RawJSON and json.RawMessage keep their bytes as written.
	var compact bytes.Buffer
	if err := json.Compact(&compact, golden); err != nil {
		t.Fatal(err)
	}
	var back SessionEvent
	if err := json.Unmarshal(compact.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, ev) {
		t.Errorf("golden decodes to a different event\n got: %+v\nwant: %+v", back, ev)
	}
}

// unsetFields returns the path of each exported field below v that has its zero value.
// It goes into the structs of this package through pointers, slice elements and map
// values. It stops at other types, such as time.Time and json.RawMessage.
func unsetFields(v reflect.Value, path string) []string {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return []string{path}
		}
		return unsetFields(v.Elem(), path)
	case reflect.Slice, reflect.Map:
		if v.Len() == 0 {
			return []string{path}
		}
		if v.Kind() == reflect.Slice {
			return unsetFields(v.Index(0), path+"[0]")
		}
		iter := v.MapRange()
		iter.Next()
		return unsetFields(iter.Value(), path+"[]")
	case reflect.Struct:
		if v.Type().PkgPath() != reflect.TypeOf(SessionEvent{}).PkgPath() {
			break
		}
		var out []string
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if f.IsExported() && f.Tag.Get("json") != "-" {
				out = append(out, unsetFields(v.Field(i), path+"."+f.Name)...)
			}
		}
		return out
	}
	if v.IsZero() {
		return []string{path}
	}
	return nil
}
