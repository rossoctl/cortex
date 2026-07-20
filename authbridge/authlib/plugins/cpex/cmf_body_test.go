package cpex

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// --- applyMCPRequestBodyMod ---

func TestMCPRequestBodyMod_ToolsCallArgsReplaced(t *testing.T) {
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_compensation","arguments":{"employee_id":"E001","include_ssn":true}}}`),
	}
	newArgs := map[string]any{"employee_id": "E001"} // include_ssn redacted
	mutated, err := applyMCPRequestBodyMod(pctx, "tools/call", MCPRequestBodyMod{NewArguments: newArgs, ArgsSet: true})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !mutated {
		t.Fatal("expected mutated=true")
	}
	var decoded map[string]any
	if err := json.Unmarshal(pctx.Body, &decoded); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	args := decoded["params"].(map[string]any)["arguments"].(map[string]any)
	if _, has := args["include_ssn"]; has {
		t.Fatalf("include_ssn should have been removed from args: %v", args)
	}
	if args["employee_id"] != "E001" {
		t.Fatalf("employee_id lost in rewrite: %v", args)
	}
}

func TestMCPRequestBodyMod_PromptsGetArgsReplaced(t *testing.T) {
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":{"name":"weather","arguments":{"city":"SF"}}}`),
	}
	newArgs := map[string]any{"city": "REDACTED"}
	mutated, err := applyMCPRequestBodyMod(pctx, "prompts/get", MCPRequestBodyMod{NewArguments: newArgs, ArgsSet: true})
	if err != nil || !mutated {
		t.Fatalf("mutated=%v err=%v", mutated, err)
	}
	if !strings.Contains(string(pctx.Body), `"city":"REDACTED"`) {
		t.Fatalf("city not redacted in body: %s", pctx.Body)
	}
}

func TestMCPRequestBodyMod_ResourcesReadURIReplaced(t *testing.T) {
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"file:///secret"}}`),
	}
	mutated, err := applyMCPRequestBodyMod(pctx, "resources/read", MCPRequestBodyMod{NewURI: "file:///public", URISet: true})
	if err != nil || !mutated {
		t.Fatalf("mutated=%v err=%v", mutated, err)
	}
	if !strings.Contains(string(pctx.Body), `"uri":"file:///public"`) {
		t.Fatalf("uri not rewritten: %s", pctx.Body)
	}
}

func TestMCPRequestBodyMod_UnsetArgsNoOp(t *testing.T) {
	// No ArgsSet flag (e.g. CPEX returned no tool_call part) → no-op.
	orig := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x","arguments":{"a":1}}}`)
	pctx := &pipeline.Context{Body: append([]byte(nil), orig...)}
	mutated, err := applyMCPRequestBodyMod(pctx, "tools/call", MCPRequestBodyMod{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if mutated {
		t.Fatal("expected mutated=false when ArgsSet is false")
	}
	if string(pctx.Body) != string(orig) {
		t.Fatalf("body changed despite no-op: %s", pctx.Body)
	}
}

func TestMCPRequestBodyMod_StripAllArgsApplies(t *testing.T) {
	// A "strip all arguments" redaction returns an empty args map WITH
	// ArgsSet=true. This MUST apply (clear params.arguments), not no-op:
	// inferring no-op from emptiness would forward the original secret.
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x","arguments":{"ssn":"123-45-6789"}}}`),
	}
	mutated, err := applyMCPRequestBodyMod(pctx, "tools/call",
		MCPRequestBodyMod{NewArguments: map[string]any{}, ArgsSet: true})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !mutated {
		t.Fatal("expected mutated=true: empty-but-set args is a real redaction")
	}
	if strings.Contains(string(pctx.Body), "123-45-6789") {
		t.Fatalf("original secret args forwarded despite strip-all redaction: %s", pctx.Body)
	}
	var decoded map[string]any
	if err := json.Unmarshal(pctx.Body, &decoded); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	args := decoded["params"].(map[string]any)["arguments"].(map[string]any)
	if len(args) != 0 {
		t.Fatalf("arguments should be empty after strip-all: %v", args)
	}
}

func TestMCPRequestBodyMod_UnsupportedMethodNoOp(t *testing.T) {
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`),
	}
	mutated, err := applyMCPRequestBodyMod(pctx, "initialize", MCPRequestBodyMod{NewArguments: map[string]any{"x": 1}, ArgsSet: true})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if mutated {
		t.Fatal("expected mutated=false for unsupported method")
	}
}

func TestMCPRequestBodyMod_MalformedJSONError(t *testing.T) {
	pctx := &pipeline.Context{Body: []byte(`{not json`)}
	_, err := applyMCPRequestBodyMod(pctx, "tools/call", MCPRequestBodyMod{NewArguments: map[string]any{"a": 1}, ArgsSet: true})
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
}

func TestMCPRequestBodyMod_EmptyBodyNoOp(t *testing.T) {
	pctx := &pipeline.Context{}
	mutated, err := applyMCPRequestBodyMod(pctx, "tools/call", MCPRequestBodyMod{NewArguments: map[string]any{"a": 1}, ArgsSet: true})
	if err != nil || mutated {
		t.Fatalf("mutated=%v err=%v on empty body", mutated, err)
	}
}

func TestMCPRequestBodyMod_NoParamsObjectNoOp(t *testing.T) {
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
	}
	mutated, err := applyMCPRequestBodyMod(pctx, "tools/call", MCPRequestBodyMod{NewArguments: map[string]any{"a": 1}, ArgsSet: true})
	if err != nil || mutated {
		t.Fatalf("mutated=%v err=%v on body without params", mutated, err)
	}
}

// --- applyMCPResponseBodyMod ---

func TestMCPResponseBodyMod_TextBlockReplaced(t *testing.T) {
	pctx := &pipeline.Context{
		ResponseBody: []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"original payload with ssn:123-45-6789"}]}}`),
	}
	newContent := map[string]any{
		"employee_id": "E001",
		"name":        "Jane Smith",
		// SSN removed
	}
	mutated, err := applyMCPResponseBodyMod(pctx, "tools/call", newContent)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !mutated {
		t.Fatal("expected mutated=true")
	}
	// The new text block is JSON-stringified newContent (a nested
	// JSON-in-JSON shape), so the inner quotes appear escaped.
	body := string(pctx.ResponseBody)
	if !strings.Contains(body, `\"employee_id\"`) || !strings.Contains(body, `\"Jane Smith\"`) {
		t.Fatalf("new content not present in response: %s", body)
	}
	if strings.Contains(body, "123-45-6789") {
		t.Fatalf("old SSN still present in rewritten response: %s", body)
	}
}

func TestMCPResponseBodyMod_MultiTextBlockFailsClosed(t *testing.T) {
	// A tools/call result with ≥2 text blocks is an ambiguous rewrite:
	// the read side (extractToolResultContent) only surfaced the first
	// block to the policy, and we hold one redacted value. Rewriting only
	// block[0] would forward block[1] (here carrying the same secret)
	// verbatim. Must fail closed.
	pctx := &pipeline.Context{
		ResponseBody: []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"first block ssn:123-45-6789"},{"type":"text","text":"second block ssn:123-45-6789"}]}}`),
	}
	newContent := map[string]any{"name": "Jane Smith"} // SSN removed
	mutated, err := applyMCPResponseBodyMod(pctx, "tools/call", newContent)
	if err == nil {
		t.Fatal("expected fail-closed error for multi-text-block response, got nil")
	}
	if mutated {
		t.Fatal("mutated=true on multi-block fail-closed")
	}
	// The body must be untouched — and critically, the planted secret in
	// the un-inspected blocks must NOT have been forwarded as a "success".
	// (We assert via the returned error contract; the body stays original.)
	if !strings.Contains(string(pctx.ResponseBody), "second block ssn:123-45-6789") {
		t.Fatalf("response body unexpectedly altered: %s", pctx.ResponseBody)
	}
}

func TestMCPResponseBodyMod_StructuredContentUpdated(t *testing.T) {
	pctx := &pipeline.Context{
		ResponseBody: []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"x"}],"structuredContent":{"ssn":"123"}}}`),
	}
	newContent := map[string]any{"name": "Jane"}
	if _, err := applyMCPResponseBodyMod(pctx, "tools/call", newContent); err != nil {
		t.Fatalf("err: %v", err)
	}
	var decoded map[string]any
	json.Unmarshal(pctx.ResponseBody, &decoded)
	result := decoded["result"].(map[string]any)
	sc := result["structuredContent"].(map[string]any)
	if sc["ssn"] != nil {
		t.Fatalf("structuredContent.ssn should be gone: %v", sc)
	}
	if sc["name"] != "Jane" {
		t.Fatalf("structuredContent.name not updated: %v", sc)
	}
}

func TestMCPResponseBodyMod_NoStructuredContentNotIntroduced(t *testing.T) {
	// When the original response didn't have structuredContent, don't
	// introduce it on rewrite — clients sniffing for the new shape
	// would be surprised.
	pctx := &pipeline.Context{
		ResponseBody: []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"x"}]}}`),
	}
	if _, err := applyMCPResponseBodyMod(pctx, "tools/call", map[string]any{"name": "Jane"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Contains(string(pctx.ResponseBody), "structuredContent") {
		t.Fatalf("structuredContent introduced when original didn't have it: %s", pctx.ResponseBody)
	}
}

func TestMCPResponseBodyMod_NotToolsCallNoOp(t *testing.T) {
	pctx := &pipeline.Context{
		ResponseBody: []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
	}
	mutated, err := applyMCPResponseBodyMod(pctx, "tools/list", map[string]any{"x": 1})
	if err != nil || mutated {
		t.Fatalf("mutated=%v err=%v for non-tools/call method", mutated, err)
	}
}

func TestMCPResponseBodyMod_EmptyBodyNoOp(t *testing.T) {
	pctx := &pipeline.Context{}
	mutated, err := applyMCPResponseBodyMod(pctx, "tools/call", map[string]any{"x": 1})
	if err != nil || mutated {
		t.Fatalf("mutated=%v err=%v on empty body", mutated, err)
	}
}

func TestMCPResponseBodyMod_NilContentNoOp(t *testing.T) {
	pctx := &pipeline.Context{
		ResponseBody: []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"x"}]}}`),
	}
	mutated, err := applyMCPResponseBodyMod(pctx, "tools/call", nil)
	if err != nil || mutated {
		t.Fatalf("mutated=%v err=%v on nil newContent", mutated, err)
	}
}

func TestMCPResponseBodyMod_MalformedJSONError(t *testing.T) {
	// Mirrors TestMCPRequestBodyMod_MalformedJSONError on the response
	// side: an unparseable response body is an error, not a silent no-op.
	pctx := &pipeline.Context{ResponseBody: []byte(`{not json`)}
	_, err := applyMCPResponseBodyMod(pctx, "tools/call", map[string]any{"x": 1})
	if err == nil {
		t.Fatal("expected error on malformed response JSON")
	}
}

func TestMCPResponseBodyMod_NoTextBlockNoMutation(t *testing.T) {
	// Response had only image/audio blocks; no text block to replace
	// and no structuredContent to update → no rewrite.
	pctx := &pipeline.Context{
		ResponseBody: []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"image","data":"abc"}]}}`),
	}
	mutated, err := applyMCPResponseBodyMod(pctx, "tools/call", map[string]any{"x": 1})
	if err != nil || mutated {
		t.Fatalf("mutated=%v err=%v when no text block to replace", mutated, err)
	}
}

// --- stringifyForTextBlock ---

func TestStringifyForTextBlock(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"plain text", "plain text"},
		{map[string]any{"k": "v"}, "{\n  \"k\": \"v\"\n}"},
		{[]int{1, 2}, "[\n  1,\n  2\n]"},
	}
	for _, tc := range cases {
		got := stringifyForTextBlock(tc.in)
		if got != tc.want {
			t.Errorf("stringifyForTextBlock(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- mcpToCMFPart: request phase ---

func TestMCPToCMFPart_ToolsCallRequest(t *testing.T) {
	mcp := &pipeline.MCPExtension{
		Method: "tools/call",
		RPCID:  float64(7), // JSON numbers decode to float64
		Params: map[string]any{
			"name":      "get_compensation",
			"arguments": map[string]any{"employee_id": "EMP-1", "include_ssn": true},
		},
	}
	got := mcpToCMFPart(mcp, false, []byte(`{"raw":"req"}`), nil)
	if got.Kind != cmfPartToolCall {
		t.Fatalf("Kind = %v, want cmfPartToolCall", got.Kind)
	}
	if got.Name != "get_compensation" {
		t.Errorf("Name = %q, want get_compensation", got.Name)
	}
	if got.CorrelationID != "7" {
		t.Errorf("CorrelationID = %q, want 7", got.CorrelationID)
	}
	if got.Arguments["employee_id"] != "EMP-1" {
		t.Errorf("Arguments lost employee_id: %v", got.Arguments)
	}
}

func TestMCPToCMFPart_PromptsGetRequest(t *testing.T) {
	mcp := &pipeline.MCPExtension{
		Method: "prompts/get",
		RPCID:  "abc",
		Params: map[string]any{"name": "weather", "arguments": map[string]any{"city": "SF"}},
	}
	got := mcpToCMFPart(mcp, false, nil, nil)
	if got.Kind != cmfPartPromptRequest {
		t.Fatalf("Kind = %v, want cmfPartPromptRequest", got.Kind)
	}
	if got.Name != "weather" || got.Arguments["city"] != "SF" || got.CorrelationID != "abc" {
		t.Errorf("unexpected part: %+v", got)
	}
}

func TestMCPToCMFPart_ResourcesReadRequest(t *testing.T) {
	mcp := &pipeline.MCPExtension{
		Method: "resources/read",
		Params: map[string]any{"uri": "file:///secret"},
	}
	got := mcpToCMFPart(mcp, false, nil, nil)
	if got.Kind != cmfPartResourceRef {
		t.Fatalf("Kind = %v, want cmfPartResourceRef", got.Kind)
	}
	if got.URI != "file:///secret" {
		t.Errorf("URI = %q, want file:///secret", got.URI)
	}
}

func TestMCPToCMFPart_NonActionFallsBackToText(t *testing.T) {
	mcp := &pipeline.MCPExtension{Method: "tools/list"}
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	got := mcpToCMFPart(mcp, false, body, nil)
	if got.Kind != cmfPartText {
		t.Fatalf("Kind = %v, want cmfPartText", got.Kind)
	}
	if got.Text != string(body) {
		t.Errorf("Text = %q, want raw body", got.Text)
	}
}

func TestMCPToCMFPart_NilExtensionFallsBackToText(t *testing.T) {
	got := mcpToCMFPart(nil, false, []byte("opaque"), nil)
	if got.Kind != cmfPartText || got.Text != "opaque" {
		t.Fatalf("nil MCP ext: got %+v, want text 'opaque'", got)
	}
}

// --- mcpToCMFPart: response phase ---

func TestMCPToCMFPart_ToolsCallResponse(t *testing.T) {
	// Method/Params persist from the request; the tool result is parsed
	// from the response body (NOT mcp.Result — cpex runs before mcp-parser
	// on the response phase, so Result isn't populated yet).
	mcp := &pipeline.MCPExtension{
		Method: "tools/call",
		RPCID:  float64(1),
		Params: map[string]any{"name": "get_compensation"},
	}
	respBody := []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"salary\":125000,\"ssn\":\"123-45-6789\"}"}]}}`)
	got := mcpToCMFPart(mcp, true, nil, respBody)
	if got.Kind != cmfPartToolResult {
		t.Fatalf("Kind = %v, want cmfPartToolResult", got.Kind)
	}
	if got.Name != "get_compensation" {
		t.Errorf("ToolName = %q, want get_compensation (preserved from request params)", got.Name)
	}
	inner, ok := got.Content.(map[string]any)
	if !ok {
		t.Fatalf("Content not parsed to object: %T %v", got.Content, got.Content)
	}
	if inner["ssn"] != "123-45-6789" {
		t.Errorf("inner result missing ssn: %v", inner)
	}
}

func TestMCPToCMFPart_ToolsCallResponseError(t *testing.T) {
	// A tools/call response carrying a JSON-RPC error (mcp.Err != nil)
	// must surface as a ToolResult part with IsError=true so APL
	// post-invoke policies can branch on tool failure.
	mcp := &pipeline.MCPExtension{
		Method: "tools/call",
		RPCID:  float64(1),
		Params: map[string]any{"name": "get_compensation"},
		Err:    &pipeline.MCPError{Code: -32001, Message: "denied"},
	}
	respBody := []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"denied"}}`)
	got := mcpToCMFPart(mcp, true, nil, respBody)
	if got.Kind != cmfPartToolResult {
		t.Fatalf("Kind = %v, want cmfPartToolResult", got.Kind)
	}
	if !got.IsError {
		t.Errorf("IsError = false, want true for a JSON-RPC error response")
	}
}

func TestMCPToCMFPart_NonToolsCallResponseFallsBackToText(t *testing.T) {
	mcp := &pipeline.MCPExtension{Method: "resources/read"}
	respBody := []byte(`{"jsonrpc":"2.0","id":1,"result":{"contents":[]}}`)
	got := mcpToCMFPart(mcp, true, nil, respBody)
	if got.Kind != cmfPartText || got.Text != string(respBody) {
		t.Fatalf("want text fallback with response body, got %+v", got)
	}
}

func TestExtractToolResultFromBody(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"ssn\":\"x\",\"salary\":1}"}]}}`)
	got := extractToolResultFromBody(body)
	obj, ok := got.(map[string]any)
	if !ok || obj["ssn"] != "x" {
		t.Fatalf("expected inner {ssn:x,...}, got %v", got)
	}
	if extractToolResultFromBody(nil) != nil {
		t.Error("nil body → nil")
	}
	if extractToolResultFromBody([]byte("not json")) != nil {
		t.Error("malformed body → nil")
	}
}

// --- extractToolResultContent ---

func TestExtractToolResultContent_StructuredContentPreferred(t *testing.T) {
	result := map[string]any{
		"structuredContent": map[string]any{"salary": 100},
		"content":           []any{map[string]any{"type": "text", "text": `{"salary":999}`}},
	}
	got := extractToolResultContent(result)
	obj, ok := got.(map[string]any)
	if !ok || obj["salary"] != 100 {
		t.Fatalf("structuredContent should win: %v", got)
	}
}

func TestExtractToolResultContent_TextBlockParsedAsJSON(t *testing.T) {
	result := map[string]any{
		"content": []any{map[string]any{"type": "text", "text": `{"ssn":"x"}`}},
	}
	got := extractToolResultContent(result)
	obj, ok := got.(map[string]any)
	if !ok || obj["ssn"] != "x" {
		t.Fatalf("text block should parse as JSON object: %v", got)
	}
}

func TestExtractToolResultContent_NonJSONTextWrapped(t *testing.T) {
	result := map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "not json"}},
	}
	got := extractToolResultContent(result)
	obj, ok := got.(map[string]any)
	if !ok || obj["text"] != "not json" {
		t.Fatalf("non-JSON text should wrap as {text:...}: %v", got)
	}
}

func TestExtractToolResultContent_Nil(t *testing.T) {
	if got := extractToolResultContent(nil); got != nil {
		t.Fatalf("nil result → nil content, got %v", got)
	}
}

// --- cmfEntity (route-selection coordinates) ---

func TestCMFEntity(t *testing.T) {
	cases := []struct {
		name               string
		part               cmfPart
		wantType, wantName string
	}{
		{"tool_call", cmfPart{Kind: cmfPartToolCall, Name: "get_compensation"}, "tool", "get_compensation"},
		{"tool_result", cmfPart{Kind: cmfPartToolResult, Name: "get_compensation"}, "tool", "get_compensation"},
		{"prompt", cmfPart{Kind: cmfPartPromptRequest, Name: "weather"}, "prompt", "weather"},
		{"resource", cmfPart{Kind: cmfPartResourceRef, URI: "file:///x"}, "resource", "file:///x"},
		{"text_fallback", cmfPart{Kind: cmfPartText, Text: "x"}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			et, en := cmfEntity(tc.part)
			if et != tc.wantType || en != tc.wantName {
				t.Errorf("cmfEntity(%s) = (%q,%q), want (%q,%q)", tc.name, et, en, tc.wantType, tc.wantName)
			}
		})
	}
}

// --- stringifyRPCID ---

func TestStringifyRPCID(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"req-9", "req-9"},
		{float64(1), "1"},
		{float64(1234567), "1234567"},
		{json.Number("42"), "42"},
	}
	for _, tc := range cases {
		if got := stringifyRPCID(tc.in); got != tc.want {
			t.Errorf("stringifyRPCID(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- pctxToCMFParts selector ---

func TestPctxToCMFParts_SelectsByPopulatedExtension(t *testing.T) {
	t.Run("mcp", func(t *testing.T) {
		pctx := &pipeline.Context{
			Extensions: pipeline.Extensions{MCP: &pipeline.MCPExtension{
				Method: "tools/call",
				Params: map[string]any{"name": "get_compensation", "arguments": map[string]any{"id": "E1"}},
			}},
		}
		parts, et, en := pctxToCMFParts(pctx, false)
		if len(parts) != 1 || parts[0].Kind != cmfPartToolCall {
			t.Fatalf("mcp parts: %+v", parts)
		}
		if et != "tool" || en != "get_compensation" {
			t.Fatalf("mcp entity = (%q,%q)", et, en)
		}
	})

	t.Run("inference", func(t *testing.T) {
		pctx := &pipeline.Context{
			Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{
				Model:    "gpt-4o",
				Messages: []pipeline.InferenceMessage{{Role: "user", Content: "hi"}},
			}},
		}
		parts, et, en := pctxToCMFParts(pctx, false)
		if len(parts) != 1 || parts[0].Text != "hi" {
			t.Fatalf("inference parts: %+v", parts)
		}
		if et != "model" || en != "gpt-4o" {
			t.Fatalf("inference entity = (%q,%q)", et, en)
		}
	})

	t.Run("a2a", func(t *testing.T) {
		pctx := &pipeline.Context{
			Extensions: pipeline.Extensions{A2A: &pipeline.A2AExtension{
				Method: "message/send",
				Parts:  []pipeline.A2APart{{Kind: "text", Content: "yo"}},
			}},
		}
		parts, et, en := pctxToCMFParts(pctx, false)
		if len(parts) != 1 || parts[0].Text != "yo" {
			t.Fatalf("a2a parts: %+v", parts)
		}
		if et != "a2a_method" || en != "message/send" {
			t.Fatalf("a2a entity = (%q,%q)", et, en)
		}
	})

	t.Run("none falls back to opaque text", func(t *testing.T) {
		pctx := &pipeline.Context{Body: []byte("raw body")}
		parts, et, en := pctxToCMFParts(pctx, false)
		if len(parts) != 1 || parts[0].Kind != cmfPartText || parts[0].Text != "raw body" {
			t.Fatalf("fallback parts: %+v", parts)
		}
		if et != "" || en != "" {
			t.Fatalf("fallback should set no entity, got (%q,%q)", et, en)
		}
	})
}

func TestInferenceHistory_RoleTagged(t *testing.T) {
	inf := &pipeline.InferenceExtension{Messages: []pipeline.InferenceMessage{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "u"},
		{Role: "assistant", Content: ""}, // dropped
	}}
	hist := inferenceHistory(inf)
	if len(hist) != 2 {
		t.Fatalf("want 2 history entries, got %d: %+v", len(hist), hist)
	}
	if hist[0]["role"] != "system" || hist[0]["content"] != "s" {
		t.Fatalf("entry0 = %+v", hist[0])
	}
	if hist[1]["role"] != "user" || hist[1]["content"] != "u" {
		t.Fatalf("entry1 = %+v", hist[1])
	}
}

func TestSessionIDFromHeaders(t *testing.T) {
	h := http.Header{}
	if got := sessionIDFromHeaders(h); got != "" {
		t.Fatalf("absent header should yield empty, got %q", got)
	}
	h.Set("X-Session-Id", "taint-demo-42")
	if got := sessionIDFromHeaders(h); got != "taint-demo-42" {
		t.Fatalf("got %q, want taint-demo-42", got)
	}
}

// --- isStreamingResponseGap ---

func TestIsStreamingResponseGap_SSEBodyWithProtocolExtension(t *testing.T) {
	pctx := &pipeline.Context{
		ResponseBody: []byte("data: {\"result\":{}}\n\n"),
		Extensions: pipeline.Extensions{
			A2A: &pipeline.A2AExtension{Method: "message/stream"},
		},
	}
	if !isStreamingResponseGap(pctx, true, 0) {
		t.Fatal("expected streaming gap: SSE body, A2A extension, zero parts")
	}
}

func TestIsStreamingResponseGap_InferenceSSE(t *testing.T) {
	pctx := &pipeline.Context{
		ResponseBody: []byte("data: {\"choices\":[{\"delta\":{}}]}\n\n"),
		Extensions: pipeline.Extensions{
			Inference: &pipeline.InferenceExtension{Model: "gpt-4"},
		},
	}
	if !isStreamingResponseGap(pctx, true, 0) {
		t.Fatal("expected streaming gap for inference SSE")
	}
}

func TestIsStreamingResponseGap_NotOnRequestPhase(t *testing.T) {
	pctx := &pipeline.Context{
		ResponseBody: []byte("data: something\n\n"),
		Extensions: pipeline.Extensions{
			A2A: &pipeline.A2AExtension{Method: "message/stream"},
		},
	}
	if isStreamingResponseGap(pctx, false, 0) {
		t.Fatal("request phase should never report a streaming gap")
	}
}

func TestIsStreamingResponseGap_PartsPresent(t *testing.T) {
	pctx := &pipeline.Context{
		ResponseBody: []byte(`{"result":{"artifacts":[{"parts":[{"kind":"text","text":"ok"}]}]}}`),
		Extensions: pipeline.Extensions{
			A2A: &pipeline.A2AExtension{Method: "message/send"},
		},
	}
	if isStreamingResponseGap(pctx, true, 1) {
		t.Fatal("should not report gap when parts were successfully extracted")
	}
}

func TestIsStreamingResponseGap_EmptyBodyNoGap(t *testing.T) {
	pctx := &pipeline.Context{
		ResponseBody: nil,
		Extensions: pipeline.Extensions{
			A2A: &pipeline.A2AExtension{Method: "message/send"},
		},
	}
	if isStreamingResponseGap(pctx, true, 0) {
		t.Fatal("empty body should not be a streaming gap")
	}
}

func TestIsStreamingResponseGap_NoProtocolExtension(t *testing.T) {
	pctx := &pipeline.Context{
		ResponseBody: []byte("data: something\n\n"),
	}
	if isStreamingResponseGap(pctx, true, 0) {
		t.Fatal("no protocol extension → no gap (unrecognised traffic)")
	}
}
