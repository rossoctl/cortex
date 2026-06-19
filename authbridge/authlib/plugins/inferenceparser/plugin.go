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
	// Dispatch by endpoint dialect: OpenAI chat/completions vs Anthropic
	// Messages. No Invocation is recorded when the parser doesn't apply
	// (unrecognized path, empty body, or non-JSON body) — operators infer
	// "inference-parser is in this pipeline" from config, not per-event rows.
	var ext *pipeline.InferenceExtension
	switch pctx.Path {
	case "/v1/chat/completions", "/v1/completions":
		ext = parseOpenAIRequest(pctx.Body)
	case anthropicMessagesPath:
		ext = parseAnthropicRequest(pctx.Body)
	default:
		return pipeline.Action{Type: pipeline.Continue}
	}
	if ext == nil {
		slog.Debug("inference-parser: no/invalid body, skipping", "path", pctx.Path)
		return pipeline.Action{Type: pipeline.Continue}
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

// parseOpenAIRequest builds an InferenceExtension from an OpenAI
// chat/completions (or completions) request body. Returns nil for an empty or
// non-JSON body. Every populated extension is an outbound LLM call — an agent
// action (IsAction); the "don't judge inference by default" choice is operator
// policy in IBAC, independent of this classification.
func parseOpenAIRequest(body []byte) *pipeline.InferenceExtension {
	if len(body) == 0 {
		return nil
	}
	var req inferenceRequest
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
		IsAction:    true,
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
	return ext
}

// OnResponse is the legacy buffered-path response hook. Because this
// plugin implements StreamingResponder, pipeline.RunResponse skips it
// and OnResponseFrame is the dispatch path under all listeners — this
// method is unreachable from a normal listener. Kept for tests and
// hypothetical pipelines that call OnResponse directly without going
// through RunResponse, with a defensive guard against re-recording if
// the streaming path has already populated state.
func (p *InferenceParser) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if pctx.Extensions.Inference == nil {
		return pipeline.Action{Type: pipeline.Continue}
	}
	ext := pctx.Extensions.Inference
	if ext.Completion != "" || ext.FinishReason != "" || ext.TotalTokens > 0 {
		return pipeline.Action{Type: pipeline.Continue}
	}
	if len(pctx.ResponseBody) == 0 {
		pctx.Skip("no_response_body")
		return pipeline.Action{Type: pipeline.Continue}
	}

	if ext.Stream {
		if pctx.Path == anthropicMessagesPath {
			parseAnthropicSSE(pctx.ResponseBody, ext)
		} else {
			parseInferenceSSE(pctx.ResponseBody, ext)
		}
	} else {
		if pctx.Path == anthropicMessagesPath {
			parseAnthropicJSON(pctx.ResponseBody, ext)
		} else {
			parseInferenceJSON(pctx.ResponseBody, ext)
		}
	}

	logInferenceFinalized(ext)
	pctx.Observe("matched_" + ext.Model + "_response")
	return pipeline.Action{Type: pipeline.Continue}
}

// inferenceStreamState is the scratch state kept on the extension for
// the duration of a streaming response. Lives in pctx.Extensions.Custom
// under a private key — kept off the public InferenceExtension shape so
// the API stays clean. The struct accumulates the in-progress
// completion until last=true triggers finalization.
type inferenceStreamState struct {
	completion strings.Builder
	usage      inferenceUsage
}

// streamStateKey scopes the scratch state to this plugin in
// pctx.Extensions.Custom. Other plugins see pctx.Extensions.Custom
// keys but won't collide with this one.
const streamStateKey = "inference-parser/stream-state"

// OnResponseFrame folds each SSE-data chunk into the running
// completion. On last=true the finalized result is written to the
// public InferenceExtension fields (Completion / FinishReason /
// token counts) and the Observe row is recorded.
//
// Application/json responses are delivered as a single last=true
// frame containing the full JSON body — the dual path keeps one
// code path for both shapes.
func (p *InferenceParser) OnResponseFrame(_ context.Context, pctx *pipeline.Context, frame []byte, last bool) pipeline.Action {
	if pctx.Extensions.Inference == nil {
		return pipeline.Action{Type: pipeline.Continue}
	}
	ext := pctx.Extensions.Inference

	// application/json one-shot: single last=true frame carrying the
	// complete envelope. Streaming responses arrive as multiple frames
	// where ext.Stream==true; tell them apart by the request-side flag.
	if last && !ext.Stream {
		if len(frame) == 0 {
			pctx.Skip("no_response_body")
			return pipeline.Action{Type: pipeline.Continue}
		}
		if pctx.Path == anthropicMessagesPath {
			parseAnthropicJSON(frame, ext)
		} else {
			parseInferenceJSON(frame, ext)
		}
		logInferenceFinalized(ext)
		pctx.Observe("matched_" + ext.Model + "_response")
		return pipeline.Action{Type: pipeline.Continue}
	}

	// Streaming path. Lazily allocate the per-stream scratch, then fold this
	// frame into it via the dialect-specific handler.
	state := getOrCreateStreamState(pctx)

	if len(frame) > 0 {
		if pctx.Path == anthropicMessagesPath {
			foldAnthropicFrame(frame, state, ext)
		} else {
			foldOpenAIFrame(frame, state, ext)
		}
	}

	if last {
		ext.Completion = state.completion.String()
		if state.usage.TotalTokens > 0 {
			ext.PromptTokens = state.usage.PromptTokens
			ext.CompletionTokens = state.usage.CompletionTokens
			ext.TotalTokens = state.usage.TotalTokens
		}
		// Empty stream with no body and no chunks — record Skip to
		// pair the response row with the request row.
		if ext.Completion == "" && ext.FinishReason == "" && ext.TotalTokens == 0 {
			pctx.Skip("no_response_body")
			return pipeline.Action{Type: pipeline.Continue}
		}
		logInferenceFinalized(ext)
		pctx.Observe("matched_" + ext.Model + "_response")
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// foldOpenAIFrame folds one OpenAI streaming chunk (data: {choices,usage}) into
// the running stream state. The "[DONE]" sentinel and malformed chunks are
// skipped. Usage arrives (cumulative) when the client set
// stream_options.include_usage.
func foldOpenAIFrame(frame []byte, state *inferenceStreamState, ext *pipeline.InferenceExtension) {
	if bytes.Equal(bytes.TrimSpace(frame), []byte("[DONE]")) {
		return
	}
	var chunk inferenceStreamChunk
	if err := json.Unmarshal(frame, &chunk); err != nil {
		slog.Debug("inference-parser: malformed streaming chunk, skipping", "error", err)
		return
	}
	for _, c := range chunk.Choices {
		if c.Delta.Content != "" {
			state.completion.WriteString(c.Delta.Content)
		}
		if c.FinishReason != "" {
			ext.FinishReason = c.FinishReason
		}
	}
	if chunk.Usage.TotalTokens > 0 {
		state.usage = chunk.Usage
	}
}

func getOrCreateStreamState(pctx *pipeline.Context) *inferenceStreamState {
	if s := pipeline.GetState[inferenceStreamState](pctx, streamStateKey); s != nil {
		return s
	}
	s := &inferenceStreamState{}
	pipeline.SetState(pctx, streamStateKey, s)
	return s
}

// logInferenceFinalized emits the operator-facing INFO log + Observe
// once a response is finalized — shared by OnResponse and
// OnResponseFrame so streaming and buffered finalize identically.
func logInferenceFinalized(ext *pipeline.InferenceExtension) {
	slog.Info("inference-parser: response",
		"model", ext.Model,
		"finishReason", ext.FinishReason,
		"promptTokens", ext.PromptTokens,
		"completionTokens", ext.CompletionTokens,
	)
	slog.Debug("inference-parser: completion", "text", parsercommon.Truncate(ext.Completion, parsercommon.DebugBodyMax))
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
