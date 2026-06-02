package mcpparser

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// configured returns an MCPParser with paths configured. Most existing
// tests use NewMCPParser() unconfigured (paths matcher is nil), which
// is fine because they only exercise body-having JSON-RPC parsing —
// the path matcher is only consulted on body-less requests.
func configured(t *testing.T, paths ...string) *MCPParser {
	t.Helper()
	p := NewMCPParser()
	cfg := struct {
		Paths []string `json:"paths"`
	}{Paths: paths}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal cfg: %v", err)
	}
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return p
}

func TestMCPParser_Capabilities(t *testing.T) {
	p := NewMCPParser()

	if p.Name() != "mcp-parser" {
		t.Errorf("Name() = %q, want %q", p.Name(), "mcp-parser")
	}

	caps := p.Capabilities()
	if !caps.ReadsBody {
		t.Error("ReadsBody should be true")
	}
}

func TestMCPParser_ToolCall(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"get_weather","arguments":{"city":"NYC"}}}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP == nil {
		t.Fatal("Extensions.MCP is nil")
	}
	if pctx.Extensions.MCP.Method != "tools/call" {
		t.Errorf("Method = %q, want %q", pctx.Extensions.MCP.Method, "tools/call")
	}
	if pctx.Extensions.MCP.RPCID != float64(1) {
		t.Errorf("RPCID = %v, want 1", pctx.Extensions.MCP.RPCID)
	}
	if pctx.Extensions.MCP.Params["name"] != "get_weather" {
		t.Errorf("Params[name] = %v, want %q", pctx.Extensions.MCP.Params["name"], "get_weather")
	}
	args, ok := pctx.Extensions.MCP.Params["arguments"].(map[string]any)
	if !ok {
		t.Fatal("Params[arguments] is not a map")
	}
	if args["city"] != "NYC" {
		t.Errorf("Params[arguments][city] = %v, want %q", args["city"], "NYC")
	}
}

func TestMCPParser_ResourceRead(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"resources/read","id":2,"params":{"uri":"file:///tmp/data.csv"}}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP == nil {
		t.Fatal("Extensions.MCP is nil")
	}
	if pctx.Extensions.MCP.Method != "resources/read" {
		t.Errorf("Method = %q, want %q", pctx.Extensions.MCP.Method, "resources/read")
	}
	if pctx.Extensions.MCP.Params["uri"] != "file:///tmp/data.csv" {
		t.Errorf("Params[uri] = %v, want %q", pctx.Extensions.MCP.Params["uri"], "file:///tmp/data.csv")
	}
}

func TestMCPParser_PromptGet(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"prompts/get","id":3,"params":{"name":"summarize","arguments":{"style":"brief"}}}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP == nil {
		t.Fatal("Extensions.MCP is nil")
	}
	if pctx.Extensions.MCP.Method != "prompts/get" {
		t.Errorf("Method = %q, want %q", pctx.Extensions.MCP.Method, "prompts/get")
	}
	if pctx.Extensions.MCP.Params["name"] != "summarize" {
		t.Errorf("Params[name] = %v, want %q", pctx.Extensions.MCP.Params["name"], "summarize")
	}
	args, ok := pctx.Extensions.MCP.Params["arguments"].(map[string]any)
	if !ok {
		t.Fatal("Params[arguments] is not a map")
	}
	if args["style"] != "brief" {
		t.Errorf("Params[arguments][style] = %v, want %q", args["style"], "brief")
	}
}

func TestMCPParser_AnyMethod(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"notifications/initialized","id":4}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP == nil {
		t.Fatal("Extensions.MCP is nil")
	}
	if pctx.Extensions.MCP.Method != "notifications/initialized" {
		t.Errorf("Method = %q, want %q", pctx.Extensions.MCP.Method, "notifications/initialized")
	}
}

func TestMCPParser_NilBody(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{Body: nil}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP != nil {
		t.Error("Extensions.MCP should be nil when body is nil")
	}
}

func TestMCPParser_EmptyBody(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{Body: []byte{}}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP != nil {
		t.Error("Extensions.MCP should be nil when body is empty")
	}
}

// Regression: an OpenAI chat/completions body is valid JSON but not
// JSON-RPC. Before the fix, mcp-parser ran first in the outbound pipeline,
// Unmarshal'd the body into a zero-value jsonRPCRequest, and attached an
// empty MCPExtension{Method:""} to every inference request/response —
// polluting the session store with a phantom "mcp: {}" on each event.
func TestMCPParser_SkipsJSONThatIsNotJSONRPC(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`),
	}
	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP != nil {
		t.Errorf("MCP should remain nil for non-JSON-RPC JSON, got %+v", pctx.Extensions.MCP)
	}
}

func TestMCPParser_InvalidJSON(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{Body: []byte("not json")}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP != nil {
		t.Error("Extensions.MCP should be nil for invalid JSON")
	}
}

func TestMCPParser_MissingParams(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"tools/call","id":5}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP == nil {
		t.Fatal("Extensions.MCP is nil")
	}
	if pctx.Extensions.MCP.Method != "tools/call" {
		t.Errorf("Method = %q, want %q", pctx.Extensions.MCP.Method, "tools/call")
	}
	if pctx.Extensions.MCP.Params != nil {
		t.Errorf("Params = %v, want nil when params not present", pctx.Extensions.MCP.Params)
	}
}

func TestMCPParser_OnResponse_NoRequestContext(t *testing.T) {
	// Request phase didn't run (no MCP extension populated): OnResponse
	// stays silent — no Invocation recorded — so the response event
	// doesn't appear as an MCP row at all.
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Direction:    pipeline.Outbound,
		ResponseBody: []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
	}
	action := p.OnResponse(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP != nil {
		t.Error("MCP extension should remain nil when request was not parsed")
	}
	if pctx.Extensions.Invocations != nil &&
		(len(pctx.Extensions.Invocations.Inbound)+len(pctx.Extensions.Invocations.Outbound)) > 0 {
		t.Errorf("non-MCP response should not record any Invocation; got %+v",
			pctx.Extensions.Invocations)
	}
}

// TestMCPParser_OnResponse_EmptyBody is the regression test for the
// notifications/initialized pairing bug: when the request side parsed
// the message (Extensions.MCP populated) but the response body is empty
// (HTTP 202 ack), the parser must record a Skip so abctl can pair the
// response row with the request row in the events timeline.
func TestMCPParser_OnResponse_EmptyBody(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Direction:  pipeline.Outbound,
		Extensions: pipeline.Extensions{MCP: &pipeline.MCPExtension{Method: "notifications/initialized"}},
	}
	action := p.OnResponse(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP.Result != nil {
		t.Error("Result should remain nil when response body is empty")
	}
	if pctx.Extensions.Invocations == nil {
		t.Fatal("expected a Skip Invocation, got none")
	}
	invs := pctx.Extensions.Invocations.Outbound
	if len(invs) != 1 {
		t.Fatalf("expected 1 Invocation, got %d", len(invs))
	}
	if invs[0].Action != pipeline.ActionSkip {
		t.Errorf("Action = %q, want skip", invs[0].Action)
	}
	if invs[0].Reason != "no_response_body" {
		t.Errorf("Reason = %q, want no_response_body", invs[0].Reason)
	}
}

func TestMCPParser_OnResponse_ToolsList(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Extensions:   pipeline.Extensions{MCP: &pipeline.MCPExtension{Method: "tools/list"}},
		ResponseBody: []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"get_weather"},{"name":"get_news"}]}}`),
	}

	action := p.OnResponse(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP.Result == nil {
		t.Fatal("Result should be populated")
	}
	tools, ok := pctx.Extensions.MCP.Result["tools"].([]any)
	if !ok {
		t.Fatalf("Result[tools] should be []any, got %T", pctx.Extensions.MCP.Result["tools"])
	}
	if len(tools) != 2 {
		t.Errorf("expected 2 tools, got %d", len(tools))
	}
}

func TestMCPParser_OnResponse_ToolsCall(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Extensions:   pipeline.Extensions{MCP: &pipeline.MCPExtension{Method: "tools/call"}},
		ResponseBody: []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"sunny, 72F"}]}}`),
	}

	action := p.OnResponse(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP.Result == nil {
		t.Fatal("Result should be populated")
	}
	if _, ok := pctx.Extensions.MCP.Result["content"]; !ok {
		t.Error("Result should contain content key")
	}
}

func TestMCPParser_OnResponse_Error(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Extensions:   pipeline.Extensions{MCP: &pipeline.MCPExtension{Method: "tools/call"}},
		ResponseBody: []byte(`{"jsonrpc":"2.0","id":3,"error":{"code":-32601,"message":"Method not found"}}`),
	}

	action := p.OnResponse(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP.Result != nil {
		t.Errorf("Result should be nil on error response, got %v", pctx.Extensions.MCP.Result)
	}
	if pctx.Extensions.MCP.Err == nil {
		t.Fatal("Err should be populated")
	}
	if pctx.Extensions.MCP.Err.Code != -32601 {
		t.Errorf("Err.Code = %d, want -32601", pctx.Extensions.MCP.Err.Code)
	}
	if pctx.Extensions.MCP.Err.Message != "Method not found" {
		t.Errorf("Err.Message = %q, want %q", pctx.Extensions.MCP.Err.Message, "Method not found")
	}
}

func TestMCPParser_OnResponse_InvalidJSON(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Extensions:   pipeline.Extensions{MCP: &pipeline.MCPExtension{Method: "tools/list"}},
		ResponseBody: []byte("not json"),
	}

	action := p.OnResponse(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP.Result != nil {
		t.Error("Result should remain nil for invalid JSON")
	}
}

func TestMCPParser_OnResponse_SSE(t *testing.T) {
	// MCP's Streamable HTTP transport returns SSE (event: / data: lines)
	// instead of plain JSON-RPC when the client sends Accept: text/event-stream.
	p := NewMCPParser()
	body := "event: message\r\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"tools\":[{\"name\":\"get_weather\"}]}}\r\n\r\n"
	pctx := &pipeline.Context{
		Extensions:   pipeline.Extensions{MCP: &pipeline.MCPExtension{Method: "tools/list"}},
		ResponseBody: []byte(body),
	}

	action := p.OnResponse(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP.Result == nil {
		t.Fatal("Result should be populated from SSE data frame")
	}
	if _, ok := pctx.Extensions.MCP.Result["tools"]; !ok {
		t.Error("Result should contain tools from SSE data")
	}
}

func TestMCPParser_OnResponse_SSE_Error(t *testing.T) {
	p := NewMCPParser()
	body := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"error\":{\"code\":-32601,\"message\":\"not found\"}}\n\n"
	pctx := &pipeline.Context{
		Extensions:   pipeline.Extensions{MCP: &pipeline.MCPExtension{Method: "tools/call"}},
		ResponseBody: []byte(body),
	}

	_ = p.OnResponse(context.Background(), pctx)
	if pctx.Extensions.MCP.Err == nil {
		t.Fatal("expected Err populated from SSE error frame")
	}
	if pctx.Extensions.MCP.Err.Message != "not found" {
		t.Errorf("Err.Message = %q, want %q", pctx.Extensions.MCP.Err.Message, "not found")
	}
}

func TestMCPParser_OnResponse_SSE_SkipsMalformedFramesUntilGoodOne(t *testing.T) {
	// A broken upstream emits a garbage data: line before the valid one.
	// Malformed frames should be logged at DEBUG, not abort parsing.
	p := NewMCPParser()
	body := "data: not-json\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n\n"
	pctx := &pipeline.Context{
		Extensions:   pipeline.Extensions{MCP: &pipeline.MCPExtension{Method: "tools/list"}},
		ResponseBody: []byte(body),
	}
	_ = p.OnResponse(context.Background(), pctx)
	if pctx.Extensions.MCP.Result == nil || pctx.Extensions.MCP.Result["ok"] != true {
		t.Errorf("expected result from second SSE frame, got %v", pctx.Extensions.MCP.Result)
	}
}

// --- Configure ---

// Default paths value: omitting the config gives ["/mcp"], which
// matches the standard MCP Streamable HTTP setup. Most operators
// won't override this.
func TestConfigure_DefaultPaths(t *testing.T) {
	p := NewMCPParser()
	if err := p.Configure(nil); err != nil {
		t.Fatalf("Configure(nil): %v", err)
	}
	// Probe the matcher with a body-less GET on /mcp; if the default
	// is right, the synthetic transport extension fires.
	pctx := &pipeline.Context{Method: "GET", Path: "/mcp", Headers: http.Header{}}
	_ = p.OnRequest(context.Background(), pctx)
	if pctx.Extensions.MCP == nil {
		t.Fatal("default paths should match /mcp; MCP extension was nil")
	}
	if pctx.Extensions.MCP.Method != syntheticTransportStream {
		t.Errorf("Method = %q, want %q", pctx.Extensions.MCP.Method, syntheticTransportStream)
	}
}

func TestConfigure_RejectsBadPattern(t *testing.T) {
	p := NewMCPParser()
	err := p.Configure(json.RawMessage(`{"paths":["[bad"]}`))
	if err == nil {
		t.Fatal("expected error for invalid path glob")
	}
	if !strings.Contains(err.Error(), "paths") {
		t.Errorf("error should mention paths; got %q", err.Error())
	}
}

func TestConfigure_RejectsUnknownFields(t *testing.T) {
	p := NewMCPParser()
	err := p.Configure(json.RawMessage(`{"paths":["/mcp"],"unknown":"x"}`))
	if err == nil {
		t.Error("expected error for unknown field")
	}
}

// --- IsAction classification on body-having JSON-RPC requests ---

// Action methods (tools/call, prompts/get, resources/read) get
// IsAction=true so guardrails know to judge them. Everything else
// stays at the default false (protocol mechanics).
func TestOnRequest_Classification_ActionMethods(t *testing.T) {
	cases := []struct {
		method  string
		body    string
		isAction bool
	}{
		// Action methods — judge.
		{"tools/call", `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"x"}}`, true},
		{"prompts/get", `{"jsonrpc":"2.0","method":"prompts/get","id":2,"params":{"name":"x"}}`, true},
		{"resources/read", `{"jsonrpc":"2.0","method":"resources/read","id":3,"params":{"uri":"x"}}`, true},
		// Protocol-mechanics methods — bypass.
		{"initialize", `{"jsonrpc":"2.0","method":"initialize","id":4}`, false},
		{"ping", `{"jsonrpc":"2.0","method":"ping","id":5}`, false},
		{"tools/list", `{"jsonrpc":"2.0","method":"tools/list","id":6}`, false},
		{"prompts/list", `{"jsonrpc":"2.0","method":"prompts/list","id":7}`, false},
		{"resources/list", `{"jsonrpc":"2.0","method":"resources/list","id":8}`, false},
		{"resources/templates/list", `{"jsonrpc":"2.0","method":"resources/templates/list","id":9}`, false},
		{"resources/subscribe", `{"jsonrpc":"2.0","method":"resources/subscribe","id":10,"params":{"uri":"x"}}`, false},
		{"resources/unsubscribe", `{"jsonrpc":"2.0","method":"resources/unsubscribe","id":11,"params":{"uri":"x"}}`, false},
		{"completion/complete", `{"jsonrpc":"2.0","method":"completion/complete","id":12}`, false},
		{"logging/setLevel", `{"jsonrpc":"2.0","method":"logging/setLevel","id":13}`, false},
		{"notifications/initialized", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, false},
		{"notifications/cancelled", `{"jsonrpc":"2.0","method":"notifications/cancelled"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			p := NewMCPParser()
			pctx := &pipeline.Context{Body: []byte(tc.body)}
			_ = p.OnRequest(context.Background(), pctx)
			if pctx.Extensions.MCP == nil {
				t.Fatalf("MCP extension nil for method %q", tc.method)
			}
			if pctx.Extensions.MCP.IsAction != tc.isAction {
				t.Errorf("IsAction = %v, want %v for method %q",
					pctx.Extensions.MCP.IsAction, tc.isAction, tc.method)
			}
		})
	}
}

// --- Body-less transport-layer detection ---

// MCP Streamable HTTP session termination: DELETE on the configured
// path with the Mcp-Session-Id header. The header is set by the MCP
// client SDK and is the precise distinguisher from a real "delete
// resource" call.
func TestOnRequest_TransportTerminate(t *testing.T) {
	p := configured(t, "/mcp")
	pctx := &pipeline.Context{
		Method:  "DELETE",
		Path:    "/mcp",
		Headers: http.Header{"Mcp-Session-Id": []string{"abc-123"}},
	}
	_ = p.OnRequest(context.Background(), pctx)
	if pctx.Extensions.MCP == nil {
		t.Fatal("expected synthetic MCP extension")
	}
	if pctx.Extensions.MCP.Method != syntheticTransportTerminate {
		t.Errorf("Method = %q, want %q", pctx.Extensions.MCP.Method, syntheticTransportTerminate)
	}
	if pctx.Extensions.MCP.IsAction {
		t.Error("IsAction should be false for transport terminate")
	}
}

// MCP Streamable HTTP SSE channel-open: body-less GET on the
// configured path.
func TestOnRequest_TransportStream(t *testing.T) {
	p := configured(t, "/mcp")
	pctx := &pipeline.Context{
		Method:  "GET",
		Path:    "/mcp",
		Headers: http.Header{},
	}
	_ = p.OnRequest(context.Background(), pctx)
	if pctx.Extensions.MCP == nil {
		t.Fatal("expected synthetic MCP extension")
	}
	if pctx.Extensions.MCP.Method != syntheticTransportStream {
		t.Errorf("Method = %q, want %q", pctx.Extensions.MCP.Method, syntheticTransportStream)
	}
	if pctx.Extensions.MCP.IsAction {
		t.Error("IsAction should be false for transport stream")
	}
}

// Body-less DELETE on configured path WITHOUT the Mcp-Session-Id
// header doesn't look like MCP transport — could be a real resource
// delete (DELETE /mcp/something) on an API that happens to share
// the path. Don't claim it.
func TestOnRequest_BodylessDELETEWithoutSessionHeader_NoExtension(t *testing.T) {
	p := configured(t, "/mcp")
	pctx := &pipeline.Context{
		Method:  "DELETE",
		Path:    "/mcp",
		Headers: http.Header{},
	}
	_ = p.OnRequest(context.Background(), pctx)
	if pctx.Extensions.MCP != nil {
		t.Errorf("expected no MCP extension, got %+v", pctx.Extensions.MCP)
	}
}

// Body-less request on a non-configured path: parser doesn't claim
// it. Defense in depth — without the path narrow, we'd be guessing
// that any body-less GET is MCP-shaped, which is wrong for non-MCP
// agent traffic.
func TestOnRequest_BodylessOnUnconfiguredPath_NoExtension(t *testing.T) {
	p := configured(t, "/mcp") // only /mcp is configured
	pctx := &pipeline.Context{
		Method:  "GET",
		Path:    "/some/other/api",
		Headers: http.Header{},
	}
	_ = p.OnRequest(context.Background(), pctx)
	if pctx.Extensions.MCP != nil {
		t.Errorf("expected no MCP extension on unconfigured path, got %+v", pctx.Extensions.MCP)
	}
}

// Body-less POST on configured path: not a recognized MCP transport
// shape (MCP uses POST only with bodies). Don't claim it.
func TestOnRequest_BodylessPOST_NoExtension(t *testing.T) {
	p := configured(t, "/mcp")
	pctx := &pipeline.Context{
		Method:  "POST",
		Path:    "/mcp",
		Headers: http.Header{},
	}
	_ = p.OnRequest(context.Background(), pctx)
	if pctx.Extensions.MCP != nil {
		t.Errorf("expected no MCP extension for body-less POST, got %+v", pctx.Extensions.MCP)
	}
}

// Custom paths config: operators can scope MCP detection to their
// actual MCP endpoint paths.
func TestConfigure_CustomPaths(t *testing.T) {
	p := configured(t, "/api/v1/mcp", "/legacy-mcp")
	for _, path := range []string{"/api/v1/mcp", "/legacy-mcp"} {
		pctx := &pipeline.Context{Method: "GET", Path: path, Headers: http.Header{}}
		_ = p.OnRequest(context.Background(), pctx)
		if pctx.Extensions.MCP == nil {
			t.Errorf("custom path %q should match", path)
		}
	}
	// /mcp is NOT in the custom list.
	pctx := &pipeline.Context{Method: "GET", Path: "/mcp", Headers: http.Header{}}
	_ = p.OnRequest(context.Background(), pctx)
	if pctx.Extensions.MCP != nil {
		t.Error("/mcp should NOT match when custom paths exclude it")
	}
}
