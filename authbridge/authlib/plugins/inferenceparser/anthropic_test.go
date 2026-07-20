package inferenceparser

import (
	"context"
	"testing"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
)

func TestInferenceParser_AnthropicMessages_Request(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/messages",
		Body: []byte(`{
			"model": "claude-opus-4-8",
			"system": "You are a helpful assistant.",
			"messages": [
				{"role": "user", "content": "What is the weather in NYC?"}
			],
			"max_tokens": 1024,
			"temperature": 0.7,
			"stream": false,
			"tools": [
				{"name": "get_weather", "description": "Get weather", "input_schema": {"type": "object"}}
			]
		}`),
	}

	if action := p.OnRequest(context.Background(), pctx); action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatal("Extensions.Inference is nil — /v1/messages not parsed")
	}
	if ext.Model != "claude-opus-4-8" {
		t.Errorf("Model = %q, want claude-opus-4-8", ext.Model)
	}
	if !ext.IsAction {
		t.Error("IsAction should be true for an inference request")
	}
	// system (top-level) is surfaced as a leading system message, then the user turn.
	if len(ext.Messages) != 2 || ext.Messages[0].Role != "system" || ext.Messages[1].Role != "user" {
		t.Fatalf("Messages = %+v, want [system, user]", ext.Messages)
	}
	if ext.Messages[0].Content != "You are a helpful assistant." {
		t.Errorf("system content = %q", ext.Messages[0].Content)
	}
	if ext.MaxTokens == nil || *ext.MaxTokens != 1024 {
		t.Errorf("MaxTokens = %v, want 1024", ext.MaxTokens)
	}
	if len(ext.Tools) != 1 || ext.Tools[0].Name != "get_weather" {
		t.Fatalf("Tools = %+v, want [get_weather]", ext.Tools)
	}
}

func TestInferenceParser_AnthropicMessages_ContentBlockArray(t *testing.T) {
	// Anthropic content can be a block array; flatten text blocks like OpenAI.
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/messages",
		Body: []byte(`{
			"model": "claude-opus-4-8",
			"max_tokens": 64,
			"messages": [
				{"role": "user", "content": [
					{"type": "text", "text": "part one"},
					{"type": "text", "text": "part two"}
				]}
			]
		}`),
	}
	p.OnRequest(context.Background(), pctx)
	ext := pctx.Extensions.Inference
	if ext == nil || len(ext.Messages) != 1 {
		t.Fatalf("ext/messages = %+v", ext)
	}
	if ext.Messages[0].Content != "part one\npart two" {
		t.Errorf("flattened content = %q, want \"part one\\npart two\"", ext.Messages[0].Content)
	}
}

func TestInferenceParser_AnthropicMessages_NonStreamingResponse(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/v1/messages"}
	// non-streaming: ext.Stream == false → one-shot last=true frame is parsed as JSON.
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-opus-4-8", IsAction: true}

	body := []byte(`{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-4-8",
		"content": [
			{"type": "text", "text": "It is sunny."},
			{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "NYC"}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 25, "output_tokens": 8, "cache_read_input_tokens": 2}
	}`)
	p.OnResponseFrame(context.Background(), pctx, body, true)

	ext := pctx.Extensions.Inference
	if ext.Completion != "It is sunny." {
		t.Errorf("Completion = %q", ext.Completion)
	}
	if ext.FinishReason != "tool_use" {
		t.Errorf("FinishReason = %q, want tool_use", ext.FinishReason)
	}
	if len(ext.ToolCalls) != 1 || ext.ToolCalls[0].Name != "get_weather" {
		t.Fatalf("ToolCalls = %+v", ext.ToolCalls)
	}
	// PromptTokens = input_tokens + cache_read (25 + 2); CompletionTokens = output_tokens.
	if ext.PromptTokens != 27 || ext.CompletionTokens != 8 || ext.TotalTokens != 35 {
		t.Errorf("tokens = prompt %d / completion %d / total %d, want 27/8/35",
			ext.PromptTokens, ext.CompletionTokens, ext.TotalTokens)
	}
}

func TestInferenceParser_AnthropicMessages_StreamFoldsEvents(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/v1/messages"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-opus-4-8", Stream: true, IsAction: true}

	frames := [][]byte{
		[]byte(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","usage":{"input_tokens":25,"output_tokens":1}}}`),
		[]byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		[]byte(`{"type":"ping"}`),
		[]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`),
		[]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`),
		[]byte(`{"type":"content_block_stop","index":0}`),
		[]byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":15}}`),
		[]byte(`{"type":"message_stop"}`),
	}
	for _, f := range frames {
		if action := p.OnResponseFrame(context.Background(), pctx, f, false); action.Type != pipeline.Continue {
			t.Fatalf("frame action = %v, want Continue", action.Type)
		}
	}
	// Mid-stream: not finalized yet.
	if pctx.Extensions.Inference.Completion != "" {
		t.Errorf("Completion populated mid-stream = %q", pctx.Extensions.Inference.Completion)
	}
	// Finalize.
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ext := pctx.Extensions.Inference
	if ext.Completion != "Hello world" {
		t.Errorf("Completion = %q, want \"Hello world\"", ext.Completion)
	}
	if ext.FinishReason != "end_turn" {
		t.Errorf("FinishReason = %q, want end_turn", ext.FinishReason)
	}
	// input_tokens from message_start; cumulative output_tokens from message_delta.
	if ext.PromptTokens != 25 || ext.CompletionTokens != 15 || ext.TotalTokens != 40 {
		t.Errorf("tokens = prompt %d / completion %d / total %d, want 25/15/40",
			ext.PromptTokens, ext.CompletionTokens, ext.TotalTokens)
	}
}
