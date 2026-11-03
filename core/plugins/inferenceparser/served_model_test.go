package inferenceparser

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// respond runs one buffered response through the parser the way every listener
// delivers it: a single terminal frame.
func respond(t *testing.T, path, requested string, hdr http.Header, body string) *pipeline.InferenceExtension {
	t.Helper()
	pctx := &pipeline.Context{Path: path, ResponseHeaders: hdr}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: requested, IsAction: true}
	NewInferenceParser().OnResponseFrame(context.Background(), pctx, []byte(body), true)
	return pctx.Extensions.Inference
}

func litellmHeader(model string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("x-litellm-model-name", model)
	return h
}

// The cases are the wire shapes measured on 2026-11-03: three LiteLLM gateways repeat
// the requested name in body.model and name the real model only in the header, and
// Ollama sends no header and repeats the name too.
func TestServedModel(t *testing.T) {
	const chatBody = `{"id":"x","object":"chat.completion","model":%q,"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":1,"total_tokens":9}}`
	const msgBody = `{"id":"m","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":1}}`
	chat := func(m string) string { return strings.Replace(chatBody, "%q", `"`+m+`"`, 1) }
	msg := func(m string) string { return strings.Replace(msgBody, "%q", `"`+m+`"`, 1) }

	for _, tc := range []struct {
		name, path, requested string
		hdr                   http.Header
		body, want            string
	}{
		{"litellm chat: header names the deployment, body repeats the alias",
			"/v1/chat/completions", "claude-opus-4-6", litellmHeader("bedrock/us.anthropic.claude-opus-4-6-v1"),
			chat("claude-opus-4-6"), "bedrock/us.anthropic.claude-opus-4-6-v1"},
		{"litellm messages: the header is read on the Anthropic dialect too",
			"/v1/messages", "claude-opus-5-5", litellmHeader("bedrock/us.anthropic.claude-opus-5-5"),
			msg("claude-opus-5-5"), "bedrock/us.anthropic.claude-opus-5-5"},
		{"header wins over a body that names a different model",
			"/v1/chat/completions", "alias", litellmHeader("hosted_vllm/deepseek"),
			chat("something-else"), "hosted_vllm/deepseek"},
		{"no header, body repeats the request: nothing new, nothing recorded",
			"/v1/chat/completions", "granite4.2:8b", http.Header{},
			chat("granite4.2:8b"), ""},
		{"no header, body names a dated version: the body is the source",
			"/v1/messages", "claude-sonnet-4-5", http.Header{},
			msg("claude-sonnet-4-5-20250929"), "claude-sonnet-4-5-20250929"},
		{"header equal to the requested model: nothing recorded",
			"/v1/chat/completions", "gpt-5", litellmHeader("gpt-5"),
			chat("gpt-5"), ""},
		{"neither source: nothing recorded",
			"/v1/chat/completions", "premium-ide", http.Header{},
			`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ext := respond(t, tc.path, tc.requested, tc.hdr, tc.body)
			if ext.ServedModel != tc.want {
				t.Errorf("ServedModel = %q, want %q", ext.ServedModel, tc.want)
			}
			if ext.Model != tc.requested {
				t.Errorf("Model = %q, want the requested %q unchanged: settlement prices it", ext.Model, tc.requested)
			}
		})
	}
}

// A streamed response carries the model on its first event and the header before
// the body. Both dialects' frame folds must reach it.
func TestServedModel_Streams(t *testing.T) {
	sse := http.Header{}
	sse.Set("Content-Type", "text/event-stream")

	t.Run("anthropic message_start", func(t *testing.T) {
		pctx := &pipeline.Context{Path: "/v1/messages", ResponseHeaders: sse}
		pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-sonnet-4-5", Stream: true, IsAction: true}
		p := NewInferenceParser()
		for _, f := range []string{
			`{"type":"message_start","message":{"model":"claude-sonnet-4-5-20250929","usage":{"input_tokens":5,"output_tokens":1}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		} {
			p.OnResponseFrame(context.Background(), pctx, []byte(f), false)
		}
		p.OnResponseFrame(context.Background(), pctx, nil, true)
		if got := pctx.Extensions.Inference.ServedModel; got != "claude-sonnet-4-5-20250929" {
			t.Errorf("ServedModel = %q, want the model message_start named", got)
		}
	})

	t.Run("openai chunks with a gateway header", func(t *testing.T) {
		h := litellmHeader("hosted_vllm/deepseek-ai/DeepSeek-V4.1-Flash-1017")
		h.Set("Content-Type", "text/event-stream")
		pctx := &pipeline.Context{Path: "/v1/chat/completions", ResponseHeaders: h}
		pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "deepseek", Stream: true, IsAction: true}
		p := NewInferenceParser()
		p.OnResponseFrame(context.Background(), pctx, []byte(`{"model":"deepseek","choices":[{"delta":{"content":"ok"}}]}`), false)
		p.OnResponseFrame(context.Background(), pctx, []byte(`{"model":"deepseek","choices":[{"delta":{},"finish_reason":"stop"}]}`), false)
		p.OnResponseFrame(context.Background(), pctx, nil, true)
		if got := pctx.Extensions.Inference.ServedModel; got != "hosted_vllm/deepseek-ai/DeepSeek-V4.1-Flash-1017" {
			t.Errorf("ServedModel = %q, want the header's model", got)
		}
	})
}

// The header is upstream-controlled and lands on an unauthenticated API and in a
// terminal: control runes must not survive, and the length is capped.
func TestServedModel_UntrustedHeader(t *testing.T) {
	hdr := litellmHeader("evil\x1b[31mred" + strings.Repeat("é", 400))
	ext := respond(t, "/v1/chat/completions", "m", hdr, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	if strings.ContainsRune(ext.ServedModel, '\x1b') {
		t.Errorf("ServedModel kept an escape byte: %q", ext.ServedModel)
	}
	if len(ext.ServedModel) > maxServedModelLen {
		t.Errorf("len(ServedModel) = %d, want <= %d", len(ext.ServedModel), maxServedModelLen)
	}
	if !strings.HasPrefix(ext.ServedModel, "evil") || !utf8Valid(ext.ServedModel) {
		t.Errorf("ServedModel = %q, want a valid-UTF-8 prefix of the header", ext.ServedModel)
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "\x00") == s }
