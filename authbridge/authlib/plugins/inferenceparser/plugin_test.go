package inferenceparser

import (
	"context"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

func TestInferenceParser_Capabilities(t *testing.T) {
	p := NewInferenceParser()

	if p.Name() != "inference-parser" {
		t.Errorf("Name() = %q, want %q", p.Name(), "inference-parser")
	}

	caps := p.Capabilities()
	if !caps.ReadsBody {
		t.Error("ReadsBody should be true")
	}
}

func TestInferenceParser_ChatCompletions(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Body: []byte(`{
			"model": "llama3.1",
			"messages": [
				{"role": "system", "content": "You are a helpful assistant."},
				{"role": "user", "content": "What is the weather in NYC?"}
			],
			"temperature": 0.7,
			"max_tokens": 1024,
			"stream": false,
			"tools": [
				{"type": "function", "function": {"name": "get_weather"}},
				{"type": "function", "function": {"name": "get_forecast"}}
			]
		}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatal("Extensions.Inference is nil")
	}
	if ext.Model != "llama3.1" {
		t.Errorf("Model = %q, want %q", ext.Model, "llama3.1")
	}
	if len(ext.Messages) != 2 {
		t.Fatalf("Messages len = %d, want 2", len(ext.Messages))
	}
	if ext.Messages[0].Role != "system" || ext.Messages[0].Content != "You are a helpful assistant." {
		t.Errorf("Messages[0] = %+v", ext.Messages[0])
	}
	if ext.Messages[1].Role != "user" || ext.Messages[1].Content != "What is the weather in NYC?" {
		t.Errorf("Messages[1] = %+v", ext.Messages[1])
	}
	if ext.Temperature == nil || *ext.Temperature != 0.7 {
		t.Errorf("Temperature = %v, want 0.7", ext.Temperature)
	}
	if ext.MaxTokens == nil || *ext.MaxTokens != 1024 {
		t.Errorf("MaxTokens = %v, want 1024", ext.MaxTokens)
	}
	if ext.Stream {
		t.Error("Stream should be false")
	}
	if len(ext.Tools) != 2 || ext.Tools[0].Name != "get_weather" || ext.Tools[1].Name != "get_forecast" {
		t.Errorf("Tools = %v, want [get_weather get_forecast]", ext.Tools)
	}
}

func TestInferenceParser_StreamRequest(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Body: []byte(`{"model": "gpt-4", "messages": [{"role": "user", "content": "hi"}], "stream": true}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.Inference == nil {
		t.Fatal("Extensions.Inference is nil")
	}
	if !pctx.Extensions.Inference.Stream {
		t.Error("Stream should be true")
	}
}

func TestInferenceParser_SystemMessage(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Body: []byte(`{
			"model": "llama3.1",
			"messages": [
				{"role": "system", "content": "You are a weather expert."},
				{"role": "user", "content": "Tell me about hurricanes."},
				{"role": "assistant", "content": "Hurricanes are..."},
				{"role": "user", "content": "What about typhoons?"}
			]
		}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatal("Extensions.Inference is nil")
	}
	if len(ext.Messages) != 4 {
		t.Fatalf("Messages len = %d, want 4", len(ext.Messages))
	}
	if ext.Messages[0].Role != "system" {
		t.Errorf("Messages[0].Role = %q, want %q", ext.Messages[0].Role, "system")
	}
	if ext.Messages[2].Role != "assistant" {
		t.Errorf("Messages[2].Role = %q, want %q", ext.Messages[2].Role, "assistant")
	}
}

func TestInferenceParser_WithTools(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Body: []byte(`{
			"model": "llama3.1",
			"messages": [{"role": "user", "content": "check weather"}],
			"tools": [
				{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}},
				{"type": "function", "function": {"name": "search_web"}}
			]
		}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatal("Extensions.Inference is nil")
	}
	if len(ext.Tools) != 2 {
		t.Fatalf("Tools len = %d, want 2", len(ext.Tools))
	}
	if ext.Tools[0].Name != "get_weather" {
		t.Errorf("Tools[0].Name = %q, want %q", ext.Tools[0].Name, "get_weather")
	}
	if ext.Tools[1].Name != "search_web" {
		t.Errorf("Tools[1].Name = %q, want %q", ext.Tools[1].Name, "search_web")
	}
}

func TestInferenceParser_CapturesToolDescriptionAndParameters(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Body: []byte(`{
			"model": "llama3.1",
			"messages": [{"role":"user","content":"x"}],
			"tools": [{
				"type": "function",
				"function": {
					"name": "get_weather",
					"description": "Get weather info for a city",
					"parameters": {
						"type": "object",
						"properties": {"city": {"type":"string"}},
						"required": ["city"]
					}
				}
			}],
			"tool_choice": "auto",
			"top_p": 0.95
		}`),
	}
	if action := p.OnRequest(context.Background(), pctx); action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.Inference
	if ext == nil || len(ext.Tools) != 1 {
		t.Fatalf("tools not captured: %+v", ext)
	}
	tool := ext.Tools[0]
	if tool.Description != "Get weather info for a city" {
		t.Errorf("Description = %q", tool.Description)
	}
	if tool.Parameters == nil {
		t.Fatal("Parameters not captured")
	}
	if tool.Parameters["type"] != "object" {
		t.Errorf("Parameters[type] = %v", tool.Parameters["type"])
	}
	if ext.ToolChoice != "auto" {
		t.Errorf("ToolChoice = %v, want \"auto\"", ext.ToolChoice)
	}
	if ext.TopP == nil || *ext.TopP != 0.95 {
		t.Errorf("TopP = %v, want 0.95", ext.TopP)
	}
}

// A malformed `parameters` value (string instead of an object) must not
// take down the whole inference capture. The tool name and description
// still land on the extension; parameters are simply nil.
func TestInferenceParser_ToolParametersNotObject(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Body: []byte(`{
			"model": "llama3.1",
			"messages": [{"role":"user","content":"x"}],
			"tools": [{
				"type": "function",
				"function": {
					"name": "get_weather",
					"description": "Get weather info",
					"parameters": "not-an-object"
				}
			}]
		}`),
	}
	if action := p.OnRequest(context.Background(), pctx); action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatal("Inference extension should be captured even with malformed parameters")
	}
	if len(ext.Tools) != 1 {
		t.Fatalf("Tools len = %d, want 1", len(ext.Tools))
	}
	if ext.Tools[0].Name != "get_weather" {
		t.Errorf("Name = %q, want get_weather", ext.Tools[0].Name)
	}
	if ext.Tools[0].Description != "Get weather info" {
		t.Errorf("Description = %q", ext.Tools[0].Description)
	}
	if ext.Tools[0].Parameters != nil {
		t.Errorf("Parameters should be nil for non-object input, got %+v", ext.Tools[0].Parameters)
	}
}

func TestInferenceParser_OnResponse_CapturesToolCalls(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{Model: "gpt-4"}},
		ResponseBody: []byte(`{
			"choices":[{
				"message":{
					"role":"assistant",
					"content":null,
					"tool_calls":[
						{"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"NYC\"}"}},
						{"id":"call_def","type":"function","function":{"name":"search_web","arguments":"{\"q\":\"hi\"}"}}
					]
				},
				"finish_reason":"tool_calls"
			}],
			"usage":{"prompt_tokens":42,"completion_tokens":18,"total_tokens":60}
		}`),
	}
	if action := p.OnResponse(context.Background(), pctx); action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.Inference
	if ext.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", ext.FinishReason)
	}
	if len(ext.ToolCalls) != 2 {
		t.Fatalf("ToolCalls len = %d, want 2", len(ext.ToolCalls))
	}
	if ext.ToolCalls[0].ID != "call_abc" || ext.ToolCalls[0].Name != "get_weather" {
		t.Errorf("ToolCalls[0] = %+v", ext.ToolCalls[0])
	}
	if ext.ToolCalls[0].Arguments != `{"city":"NYC"}` {
		t.Errorf("ToolCalls[0].Arguments = %q", ext.ToolCalls[0].Arguments)
	}
	if ext.ToolCalls[1].Name != "search_web" {
		t.Errorf("ToolCalls[1].Name = %q", ext.ToolCalls[1].Name)
	}
}

// TestInferenceParser_VlessPath covers OpenAI-compatible proxies (e.g. opencode via
// litellm) that strip the /v1 prefix and post directly to /chat/completions or
// /completions. Without these path variants the parser falls to the default arm
// and records no inference telemetry.
func TestInferenceParser_VlessPath_ChatCompletions(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/chat/completions",
		Body: []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":false}`),
	}
	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatal("Extensions.Inference is nil for /chat/completions")
	}
	if ext.Model != "gpt-4" {
		t.Errorf("Model = %q, want gpt-4", ext.Model)
	}
	if !ext.IsAction {
		t.Error("IsAction should be true")
	}
}

func TestInferenceParser_VlessPath_Completions(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/completions",
		Body: []byte(`{"model":"codellama","messages":[{"role":"user","content":"hi"}]}`),
	}
	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.Inference == nil {
		t.Fatal("Extensions.Inference is nil for /completions")
	}
	if pctx.Extensions.Inference.Model != "codellama" {
		t.Errorf("Model = %q, want codellama", pctx.Extensions.Inference.Model)
	}
}

// TestInferenceParser_BobPath covers IBM Bob, which mounts an OpenAI-dialect
// inference API under an /inference prefix. The dispatch switch is exact-match, so
// without the path the parser falls to the default arm and records no telemetry at
// all — the body is never even looked at.
func TestInferenceParser_BobPath_ChatCompletions(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: bobPath,
		Body: []byte(`{"model":"granite-3-8b-instruct","messages":[{"role":"user","content":"hi"}],"stream":false}`),
	}
	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatalf("Extensions.Inference is nil for %s", bobPath)
	}
	if ext.Model != "granite-3-8b-instruct" {
		t.Errorf("Model = %q, want granite-3-8b-instruct", ext.Model)
	}
	if len(ext.Messages) != 1 || ext.Messages[0].Content != "hi" {
		t.Errorf("Messages = %+v, want one user message \"hi\"", ext.Messages)
	}
	if !ext.IsAction {
		t.Error("IsAction should be true: an outbound LLM call is an agent action")
	}
}

// A query string must not defeat the match. endpointPath cuts at "?", and this is
// the failure mode that made /v1/messages?beta=true invisible on the extproc path.
func TestInferenceParser_BobPath_WithQueryString(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: bobPath + "?api-version=2024-02-01",
		Body: []byte(`{"model":"granite-3-8b-instruct","messages":[{"role":"user","content":"hi"}]}`),
	}
	p.OnRequest(context.Background(), pctx)
	if pctx.Extensions.Inference == nil {
		t.Fatalf("Extensions.Inference is nil for %s with a query string", bobPath)
	}
}

// Bob's responses must parse too. The response side branches on "is this
// Anthropic", so an OpenAI-dialect path falls through to parseInferenceJSON — this
// pins that, since a request-only path would give an operator a model name and no
// token counts.
func TestInferenceParser_BobPath_Response(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: bobPath}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{
		Model: "granite-3-8b-instruct", IsAction: true,
	}
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"hello"},` +
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`)

	p.OnResponseFrame(context.Background(), pctx, body, true)

	ext := pctx.Extensions.Inference
	if ext.Completion != "hello" {
		t.Errorf("Completion = %q, want hello", ext.Completion)
	}
	if ext.PromptTokens != 11 || ext.CompletionTokens != 3 {
		t.Errorf("tokens = %d/%d, want 11/3", ext.PromptTokens, ext.CompletionTokens)
	}
	if ext.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", ext.FinishReason)
	}
}

// The sibling Bob endpoints seen alongside the inference one are NOT inference and
// must stay unmatched — an exact-match switch is what keeps /admin/v1/profile from
// being mistaken for a chat completion.
func TestInferenceParser_BobNonInferencePathsAreIgnored(t *testing.T) {
	for _, path := range []string{
		"/admin/v1/profile",
		"/inference/v1/model/info",
		"/inference/v1/chat",              // prefix of the real path
		"/inference/v1/chat/completions/", // trailing slash
	} {
		p := NewInferenceParser()
		pctx := &pipeline.Context{
			Path: path,
			Body: []byte(`{"model":"granite-3-8b-instruct","messages":[{"role":"user","content":"hi"}]}`),
		}
		p.OnRequest(context.Background(), pctx)
		if pctx.Extensions.Inference != nil {
			t.Errorf("%s matched as inference; only %s should", path, bobPath)
		}
	}
}

func TestInferenceParser_NonMatchingPath(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/api/other",
		Body: []byte(`{"model": "llama3.1", "messages": [{"role": "user", "content": "hi"}]}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.Inference != nil {
		t.Error("Extensions.Inference should be nil for non-matching path")
	}
}

func TestInferenceParser_NilBody(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Body: nil,
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.Inference != nil {
		t.Error("Extensions.Inference should be nil when body is nil")
	}
}

func TestInferenceParser_EmptyBody(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Body: []byte{},
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.Inference != nil {
		t.Error("Extensions.Inference should be nil when body is empty")
	}
}

func TestInferenceParser_InvalidJSON(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Body: []byte("not valid json"),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.Inference != nil {
		t.Error("Extensions.Inference should be nil for invalid JSON")
	}
}

func TestInferenceParser_LegacyCompletions(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/completions",
		Body: []byte(`{"model": "codellama", "prompt": "Write a function that", "max_tokens": 256}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatal("Extensions.Inference is nil")
	}
	if ext.Model != "codellama" {
		t.Errorf("Model = %q, want %q", ext.Model, "codellama")
	}
	if ext.MaxTokens == nil || *ext.MaxTokens != 256 {
		t.Errorf("MaxTokens = %v, want 256", ext.MaxTokens)
	}
}

func TestInferenceParser_OnResponse_NoRequestContext(t *testing.T) {
	// Without an Inference extension (e.g., path didn't match on the request),
	// OnResponse should be a no-op.
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		ResponseBody: []byte(`{"choices":[{"message":{"content":"hi"}}]}`),
	}
	action := p.OnResponse(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.Inference != nil {
		t.Error("Inference extension should remain nil when request was not parsed")
	}
}

// TestInferenceParser_OnResponse_EmptyBody locks the regression: when
// the request side parsed (Extensions.Inference populated) but the
// response body is empty, the parser MUST record a Skip so abctl pairs
// the timeline rows.
func TestInferenceParser_OnResponse_EmptyBody(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Direction:  pipeline.Outbound,
		Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{Model: "gpt-4"}},
	}
	action := p.OnResponse(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.Inference.Completion != "" {
		t.Errorf("Completion = %q, want empty", pctx.Extensions.Inference.Completion)
	}
	if pctx.Extensions.Invocations == nil {
		t.Fatal("expected a Skip Invocation, got none")
	}
	invs := pctx.Extensions.Invocations.Outbound
	if len(invs) != 1 || invs[0].Action != pipeline.ActionSkip || invs[0].Reason != "no_response_body" {
		t.Fatalf("expected single Skip/no_response_body, got %+v", invs)
	}
}

func TestInferenceParser_OnResponse_NonStreaming(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{Model: "gpt-4"}},
		ResponseBody: []byte(`{
			"choices":[{"message":{"role":"assistant","content":"Hello, world!"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17}
		}`),
	}
	action := p.OnResponse(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.Inference
	if ext.Completion != "Hello, world!" {
		t.Errorf("Completion = %q, want %q", ext.Completion, "Hello, world!")
	}
	if ext.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", ext.FinishReason, "stop")
	}
	if ext.PromptTokens != 12 || ext.CompletionTokens != 5 || ext.TotalTokens != 17 {
		t.Errorf("Tokens = (%d, %d, %d), want (12, 5, 17)", ext.PromptTokens, ext.CompletionTokens, ext.TotalTokens)
	}
}

func TestInferenceParser_OnResponse_SSE(t *testing.T) {
	p := NewInferenceParser()
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\", \"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"world!\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3,\"total_tokens\":13}}\n\n" +
		"data: [DONE]\n\n"
	pctx := &pipeline.Context{
		Extensions:   pipeline.Extensions{Inference: &pipeline.InferenceExtension{Model: "gpt-4", Stream: true}},
		ResponseBody: []byte(body),
	}
	action := p.OnResponse(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.Inference
	if ext.Completion != "Hello, world!" {
		t.Errorf("Completion = %q, want %q", ext.Completion, "Hello, world!")
	}
	if ext.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", ext.FinishReason, "stop")
	}
	if ext.TotalTokens != 13 {
		t.Errorf("TotalTokens = %d, want 13", ext.TotalTokens)
	}
}

func TestInferenceParser_OnResponse_InvalidJSON(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Extensions:   pipeline.Extensions{Inference: &pipeline.InferenceExtension{Model: "gpt-4"}},
		ResponseBody: []byte("not json"),
	}
	action := p.OnResponse(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.Inference.Completion != "" {
		t.Error("Completion should remain empty on parse failure")
	}
}

func TestInferenceParser_MultipartContent(t *testing.T) {
	// Follow-up requests after tool calls often carry `content` as an array
	// of parts. The parser must accept this shape without failing the whole
	// unmarshal — previously the entire request was silently dropped because
	// Content was typed as string.
	p := NewInferenceParser()
	body := `{
		"model": "gpt-4",
		"messages": [
			{"role": "user", "content": "what's the weather?"},
			{"role": "tool", "content": [{"type":"text","text":"sunny, 72F"}]},
			{"role": "user", "content": [
				{"type":"text","text":"hello"},
				{"type":"image_url","image_url":{"url":"http://x"}},
				{"type":"text","text":"there"}
			]}
		]
	}`
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Body: []byte(body),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.Inference == nil {
		t.Fatal("Inference extension should be populated despite array content")
	}
	msgs := pctx.Extensions.Inference.Messages
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	if msgs[0].Content != "what's the weather?" {
		t.Errorf("msg[0].Content = %q, want plain string", msgs[0].Content)
	}
	if msgs[1].Content != "sunny, 72F" {
		t.Errorf("msg[1].Content = %q, want flattened text part", msgs[1].Content)
	}
	// Non-text parts dropped, multiple text parts joined with newline.
	if msgs[2].Content != "hello\nthere" {
		t.Errorf("msg[2].Content = %q, want %q", msgs[2].Content, "hello\nthere")
	}
}

// TestInferenceParser_ContentBytes covers the OpenAI dialect's own
// ContentBytes accounting — a separate UnmarshalJSON from the Anthropic path.
// A tool-result message whose content is an array flattens to text only for
// the text parts; the dropped parts (images, tool payloads) still cost prompt
// tokens, and ContentBytes is what they contribute.
func TestInferenceParser_ContentBytes(t *testing.T) {
	p := NewInferenceParser()
	body := `{
		"model": "gpt-4",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "tool", "content": [{"type":"image_url","image_url":{"url":"http://x/very/long/path"}}]},
			{"role": "assistant", "content": null, "tool_calls": []}
		]
	}`
	pctx := &pipeline.Context{Path: "/v1/chat/completions", Body: []byte(body)}
	p.OnRequest(context.Background(), pctx)

	msgs := pctx.Extensions.Inference.Messages
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	// `"hi"` on the wire — the quotes are part of the JSON value.
	if msgs[0].ContentBytes != 4 {
		t.Errorf("msgs[0].ContentBytes = %d, want 4", msgs[0].ContentBytes)
	}
	// No text parts, so Content is empty — but the message is far from free.
	if msgs[1].Content != "" || msgs[1].ContentBytes <= msgs[0].ContentBytes {
		t.Errorf("msgs[1] = %q / %d bytes, want empty text and > %d bytes",
			msgs[1].Content, msgs[1].ContentBytes, msgs[0].ContentBytes)
	}
	// content: null has no content to size — reporting the 4 bytes of the
	// literal would make an empty assistant turn look like a small payload.
	if msgs[2].ContentBytes != 0 {
		t.Errorf("msgs[2].ContentBytes = %d, want 0 for null content", msgs[2].ContentBytes)
	}
}

func TestInferenceParser_NullContent(t *testing.T) {
	// Assistant messages that only carry tool_calls have content: null.
	p := NewInferenceParser()
	body := `{
		"model": "gpt-4",
		"messages": [
			{"role": "assistant", "content": null, "tool_calls": []}
		]
	}`
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Body: []byte(body),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.Inference == nil {
		t.Fatal("Inference extension should be populated despite null content")
	}
	if pctx.Extensions.Inference.Messages[0].Content != "" {
		t.Errorf("Content = %q, want empty for null", pctx.Extensions.Inference.Messages[0].Content)
	}
}

// IsAction is unconditionally true on populated InferenceExtension.
// Every outbound LLM call is, on the wire, an action — the operator
// policy "don't judge inference by default" is encoded separately in
// IBAC's judge_inference config, not in the classification.
func TestInferenceParser_AlwaysClassifiesAsAction(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Body: []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`),
	}
	_ = p.OnRequest(context.Background(), pctx)
	if pctx.Extensions.Inference == nil {
		t.Fatal("Inference extension is nil")
	}
	if !pctx.Extensions.Inference.IsAction {
		t.Error("IsAction = false, want true (every populated inference call is an action)")
	}
}
