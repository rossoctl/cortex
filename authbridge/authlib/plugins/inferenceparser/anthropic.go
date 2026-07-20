package inferenceparser

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
)

// anthropicMessagesPath is the Anthropic Messages API endpoint. Clients
// (e.g. claude-code via a LiteLLM/Anthropic-compatible gateway) POST here
// instead of the OpenAI /v1/chat/completions endpoint, so the parser must
// recognize both dialects.
const anthropicMessagesPath = "/v1/messages"

// --- request ---

// anthropicRequest is the subset of the Anthropic Messages request we surface.
// Unlike OpenAI, the system prompt is a top-level field (string or text-block
// array), not a message with role "system".
type anthropicRequest struct {
	Model       string                `json:"model"`
	Messages    []anthropicReqMessage `json:"messages"`
	System      json.RawMessage       `json:"system"`
	Temperature *float64              `json:"temperature"`
	MaxTokens   *int                  `json:"max_tokens"`
	TopP        *float64              `json:"top_p"`
	Stream      bool                  `json:"stream"`
	Tools       []anthropicTool       `json:"tools"`
	ToolChoice  any                   `json:"tool_choice"`
}

// anthropicTool is an Anthropic tool definition. The schema lives under
// input_schema (vs OpenAI's nested function.parameters).
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicReqMessage flattens the request message content to text. Anthropic
// content is a string or an array of content blocks (text / image / tool_use /
// tool_result); reuse flattenContent, which keeps text blocks and drops the
// rest — the same {"type":"text","text":...} shape OpenAI uses.
type anthropicReqMessage struct {
	Role    string
	Content string
}

func (m *anthropicReqMessage) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.Content = flattenContent(raw.Content)
	return nil
}

// parseAnthropicRequest builds an InferenceExtension from an Anthropic Messages
// request body. Returns nil for an empty or non-JSON body (caller treats nil as
// "not an inference request we can parse" and continues).
func parseAnthropicRequest(body []byte) *pipeline.InferenceExtension {
	if len(body) == 0 {
		return nil
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}

	ext := &pipeline.InferenceExtension{
		Model:       req.Model,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		TopP:        req.TopP,
		Stream:      req.Stream,
		ToolChoice:  req.ToolChoice,
		// Every populated InferenceExtension is an outbound LLM call — an
		// agent action. Same classification as the OpenAI path.
		IsAction: true,
	}

	// Anthropic carries the system prompt top-level, not as a message role.
	// Surface it as a leading system message so downstream policy plugins
	// (IBAC, etc.) see it the same way they see OpenAI's system message.
	if sys := flattenContent(req.System); sys != "" {
		ext.Messages = append(ext.Messages, pipeline.InferenceMessage{Role: "system", Content: sys})
	}
	for _, msg := range req.Messages {
		ext.Messages = append(ext.Messages, pipeline.InferenceMessage{Role: msg.Role, Content: msg.Content})
	}
	for _, tool := range req.Tools {
		if tool.Name == "" {
			continue
		}
		ext.Tools = append(ext.Tools, pipeline.InferenceTool{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  rawMessageToMap(tool.InputSchema),
		})
	}
	return ext
}

// --- usage (shared by response + streaming) ---

// anthropicUsage mirrors the Messages API usage block. The true input size is
// input_tokens + cache_creation_input_tokens + cache_read_input_tokens (cached
// context still counts as input); promptTotal sums them.
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

func (u anthropicUsage) promptTotal() int {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// --- non-streaming response ---

type anthropicResponse struct {
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsage          `json:"usage"`
}

type anthropicContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// parseAnthropicJSON parses a non-streaming Messages response: text blocks ->
// completion, tool_use blocks -> tool calls, usage -> token counts.
func parseAnthropicJSON(body []byte, ext *pipeline.InferenceExtension) {
	var resp anthropicResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return
	}
	var b strings.Builder
	for _, blk := range resp.Content {
		switch blk.Type {
		case "text":
			if blk.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(blk.Text)
			}
		case "tool_use":
			ext.ToolCalls = append(ext.ToolCalls, pipeline.InferenceToolCall{
				ID:        blk.ID,
				Name:      blk.Name,
				Arguments: string(blk.Input),
			})
		}
	}
	ext.Completion = b.String()
	if resp.StopReason != "" {
		ext.FinishReason = resp.StopReason
	}
	ext.PromptTokens = resp.Usage.promptTotal()
	ext.CompletionTokens = resp.Usage.OutputTokens
	ext.TotalTokens = ext.PromptTokens + ext.CompletionTokens
}

// --- streaming ---

// anthropicStreamEvent is one SSE event's data payload. The Messages stream is
// a sequence of typed events (vs OpenAI's uniform chat.completion.chunk):
// message_start (carries usage.input_tokens), content_block_delta (text_delta /
// input_json_delta / thinking_delta), message_delta (delta.stop_reason +
// cumulative usage.output_tokens), message_stop, plus ping/content_block_*.
type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	Delta *struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anthropicUsage `json:"usage"`
}

// foldAnthropicFrame folds one Messages SSE event into the running stream state.
// input_tokens come from message_start; the completion accumulates from
// text_delta blocks; stop_reason and the cumulative output_tokens arrive in
// message_delta. Unknown events (ping, content_block_start/stop, message_stop)
// are ignored.
func foldAnthropicFrame(frame []byte, state *inferenceStreamState, ext *pipeline.InferenceExtension) {
	var ev anthropicStreamEvent
	if err := json.Unmarshal(frame, &ev); err != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			state.usage.PromptTokens = ev.Message.Usage.promptTotal()
		}
	case "content_block_delta":
		if ev.Delta != nil && ev.Delta.Type == "text_delta" {
			state.completion.WriteString(ev.Delta.Text)
		}
	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			ext.FinishReason = ev.Delta.StopReason
		}
		if ev.Usage != nil && ev.Usage.OutputTokens > 0 {
			// usage.output_tokens in message_delta is cumulative — take the
			// latest. TotalTokens must be non-zero for the shared finalize
			// block to copy the counts onto the extension.
			state.usage.CompletionTokens = ev.Usage.OutputTokens
			state.usage.TotalTokens = state.usage.PromptTokens + ev.Usage.OutputTokens
		}
	}
}

// parseAnthropicSSE folds a fully-buffered Messages SSE body. Mirrors
// parseInferenceSSE for the legacy OnResponse path; the live listener uses
// foldAnthropicFrame via OnResponseFrame instead.
func parseAnthropicSSE(body []byte, ext *pipeline.InferenceExtension) {
	state := &inferenceStreamState{}
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 {
			continue
		}
		foldAnthropicFrame(data, state, ext)
	}
	ext.Completion = state.completion.String()
	if state.usage.TotalTokens > 0 {
		ext.PromptTokens = state.usage.PromptTokens
		ext.CompletionTokens = state.usage.CompletionTokens
		ext.TotalTokens = state.usage.TotalTokens
	}
}

// rawMessageToMap decodes a JSON object into a map, returning nil for an absent
// or non-object value (so a non-object input_schema doesn't fail the parse).
func rawMessageToMap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}
