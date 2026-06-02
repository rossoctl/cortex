package inferenceparser

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/internal/parsercommon"
)

// InferenceParser parses outbound OpenAI-compatible LLM inference requests
// and populates pctx.Extensions.Inference for downstream policy plugins.
type InferenceParser struct{}

func NewInferenceParser() *InferenceParser { return &InferenceParser{} }

func init() {
	plugins.RegisterPlugin("inference-parser", func() pipeline.Plugin { return NewInferenceParser() })
}

func (p *InferenceParser) Name() string { return "inference-parser" }

func (p *InferenceParser) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		ReadsBody:   true,
		Description: "Parses LLM completions into pctx.Extensions.Inference.",
	}
}

func (p *InferenceParser) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	// No Invocation recorded when the parser doesn't apply to this
	// message — wrong path (anything other than OpenAI chat/completion
	// endpoints), empty body, or non-JSON body. Operators infer
	// "inference-parser exists in this pipeline" from config, not per-
	// event rows.
	if pctx.Path != "/v1/chat/completions" && pctx.Path != "/v1/completions" {
		return pipeline.Action{Type: pipeline.Continue}
	}

	if len(pctx.Body) == 0 {
		slog.Debug("inference-parser: no body, skipping")
		return pipeline.Action{Type: pipeline.Continue}
	}

	var req inferenceRequest
	if err := json.Unmarshal(pctx.Body, &req); err != nil {
		slog.Debug("inference-parser: invalid JSON", "error", err)
		return pipeline.Action{Type: pipeline.Continue}
	}

	ext := &pipeline.InferenceExtension{
		Model:       req.Model,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		TopP:        req.TopP,
		Stream:      req.Stream,
		ToolChoice:  req.ToolChoice,
		// Every populated InferenceExtension is an outbound LLM call —
		// the agent making a real action. The "don't judge inference
		// by default" choice is operator policy, lives in IBAC's
		// judge_inference config; the classification verdict here is
		// independent of that policy.
		IsAction: true,
	}

	for _, msg := range req.Messages {
		ext.Messages = append(ext.Messages, pipeline.InferenceMessage{
			Role:    msg.Role,
			Content: msg.Content,
		})
	}

	for _, tool := range req.Tools {
		if tool.Function.Name == "" {
			continue
		}
		ext.Tools = append(ext.Tools, pipeline.InferenceTool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Parameters:  tool.Function.paramsMap(),
		})
	}

	pctx.Extensions.Inference = ext

	slog.Info("inference-parser", "model", ext.Model)
	slog.Debug("inference-parser: extracted", "model", ext.Model, "messages", len(ext.Messages), "stream", ext.Stream, "tools", len(ext.Tools))
	for i, m := range ext.Messages {
		slog.Debug("inference-parser: message", "index", i, "role", m.Role, "content", parsercommon.Truncate(m.Content, parsercommon.DebugBodyMax))
	}

	pctx.Observe("matched_" + ext.Model)
	return pipeline.Action{Type: pipeline.Continue}
}

// OnResponse populates the response-side fields (Completion, FinishReason,
// token counts) on pctx.Extensions.Inference. Handles both non-streaming
// JSON responses and SSE streams from OpenAI-compatible servers.
func (p *InferenceParser) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	// Stay silent when the request side never participated — the parser
	// recorded nothing on request, so recording on response would orphan
	// the row.
	if pctx.Extensions.Inference == nil {
		return pipeline.Action{Type: pipeline.Continue}
	}
	// We DID process the request but the response has no body — record
	// a Skip so abctl can pair the response row with the request row.
	if len(pctx.ResponseBody) == 0 {
		pctx.Skip("no_response_body")
		return pipeline.Action{Type: pipeline.Continue}
	}

	if pctx.Extensions.Inference.Stream {
		parseInferenceSSE(pctx.ResponseBody, pctx.Extensions.Inference)
	} else {
		parseInferenceJSON(pctx.ResponseBody, pctx.Extensions.Inference)
	}

	ext := pctx.Extensions.Inference
	slog.Info("inference-parser: response",
		"model", ext.Model,
		"finishReason", ext.FinishReason,
		"promptTokens", ext.PromptTokens,
		"completionTokens", ext.CompletionTokens,
	)
	slog.Debug("inference-parser: completion", "text", parsercommon.Truncate(ext.Completion, parsercommon.DebugBodyMax))
	pctx.Observe("matched_" + ext.Model + "_response")
	return pipeline.Action{Type: pipeline.Continue}
}

// parseInferenceJSON parses a non-streaming OpenAI chat/completions response.
func parseInferenceJSON(body []byte, ext *pipeline.InferenceExtension) {
	var resp inferenceResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		slog.Debug("inference-parser: invalid response JSON", "error", err)
		return
	}
	if len(resp.Choices) > 0 {
		c := resp.Choices[0]
		ext.Completion = c.Message.Content
		ext.FinishReason = c.FinishReason
		for _, tc := range c.Message.ToolCalls {
			ext.ToolCalls = append(ext.ToolCalls, pipeline.InferenceToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}
	ext.PromptTokens = resp.Usage.PromptTokens
	ext.CompletionTokens = resp.Usage.CompletionTokens
	ext.TotalTokens = resp.Usage.TotalTokens
}

// parseInferenceSSE concatenates content deltas across SSE events and captures
// the last finish_reason and usage block (sent when stream_options.include_usage
// is set). The stream terminates with a "data: [DONE]" marker which is skipped.
func parseInferenceSSE(body []byte, ext *pipeline.InferenceExtension) {
	var completion strings.Builder
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var chunk inferenceStreamChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			slog.Debug("inference-parser: skipping malformed SSE data frame", "error", err, "data", parsercommon.Truncate(string(data), 128))
			continue
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				completion.WriteString(c.Delta.Content)
			}
			if c.FinishReason != "" {
				ext.FinishReason = c.FinishReason
			}
		}
		if chunk.Usage.TotalTokens > 0 {
			ext.PromptTokens = chunk.Usage.PromptTokens
			ext.CompletionTokens = chunk.Usage.CompletionTokens
			ext.TotalTokens = chunk.Usage.TotalTokens
		}
	}
	ext.Completion = completion.String()
}

type inferenceResponse struct {
	Choices []inferenceChoice `json:"choices"`
	Usage   inferenceUsage    `json:"usage"`
}

type inferenceChoice struct {
	Message      inferenceRespMessage `json:"message"`
	FinishReason string               `json:"finish_reason"`
}

// inferenceRespMessage is the response-side message shape. Separate from
// the request-side inferenceMessage (which has the multi-part content
// Unmarshaler) because responses only carry plain-string content + an
// optional tool_calls array.
type inferenceRespMessage struct {
	Role      string                  `json:"role"`
	Content   string                  `json:"content"`
	ToolCalls []inferenceRespToolCall `json:"tool_calls"`
}

// inferenceRespToolCall matches OpenAI's tool-call shape:
//
//	{"id":"call_123","type":"function","function":{"name":"...","arguments":"..."}}
type inferenceRespToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // raw JSON string
	} `json:"function"`
}

type inferenceStreamChunk struct {
	Choices []inferenceStreamChoice `json:"choices"`
	Usage   inferenceUsage          `json:"usage"`
}

type inferenceStreamChoice struct {
	Delta        inferenceDelta `json:"delta"`
	FinishReason string         `json:"finish_reason"`
}

type inferenceDelta struct {
	Content string `json:"content"`
}

type inferenceUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type inferenceRequest struct {
	Model       string             `json:"model"`
	Messages    []inferenceMessage `json:"messages"`
	Temperature *float64           `json:"temperature"`
	MaxTokens   *int               `json:"max_tokens"`
	TopP        *float64           `json:"top_p"`
	Stream      bool               `json:"stream"`
	Tools       []inferenceTool    `json:"tools"`
	ToolChoice  any                `json:"tool_choice"` // "auto"/"none" or object
}

// inferenceMessage accepts both OpenAI content shapes:
//   - "content": "plain string"
//   - "content": [{"type":"text","text":"..."}, {"type":"image_url",...}, ...]
//
// The array form is used for multi-modal input and tool-result messages.
// Non-text parts (image_url, tool_use objects, etc.) are dropped since the
// parser only exposes text for downstream policy plugins.
type inferenceMessage struct {
	Role    string
	Content string
}

func (m *inferenceMessage) UnmarshalJSON(data []byte) error {
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

// flattenContent returns the text representation of an OpenAI content value.
// Returns "" when content is absent, null, or contains no text parts.
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

type inferenceTool struct {
	Type     string            `json:"type"`
	Function inferenceFunction `json:"function"`
}

// inferenceFunction decodes the function object within an OpenAI tool
// definition. Parameters is deliberately a json.RawMessage rather than a
// map[string]any so a non-object value (string / number / null) does not
// fail the whole request decode — we fall back to nil parameters but still
// capture the tool name and description.
type inferenceFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// paramsMap decodes Parameters into a map. Returns nil if the value is
// absent or not a JSON object (e.g. a string or number); callers treat nil
// as "no schema captured" without failing the whole inference parse.
func (f inferenceFunction) paramsMap() map[string]any {
	if len(f.Parameters) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(f.Parameters, &m); err != nil {
		return nil
	}
	return m
}
