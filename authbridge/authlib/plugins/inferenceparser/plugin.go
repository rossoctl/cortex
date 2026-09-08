package inferenceparser

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins/internal/parsercommon"
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

// endpointPath returns pctx.Path with any query string removed.
//
// Every listener now guarantees Path is query-free (see pipeline.Context.Path),
// so this is defense in depth for contexts constructed outside a listener
// (tests, future transports). It stays because the failure mode it guards is
// silent: Claude Code posts to /v1/messages?beta=true, and with a query
// attached the exact-match dialect dispatch below falls to the default arm and
// records no inference telemetry at all — or worse, sends an Anthropic stream
// to the OpenAI parser.
func endpointPath(pctx *pipeline.Context) string {
	path, _, _ := strings.Cut(pctx.Path, "?")
	return path
}

// bobPath is IBM Bob's inference endpoint. It speaks the OPENAI dialect — the
// body is {model, messages, ...} — despite the path not matching any of the
// OpenAI spellings above, because Bob mounts the API under an /inference prefix.
//
// A named const rather than another string in the switch: the prefix is what
// makes it non-obvious that this is OpenAI-shaped, and the name is where that
// gets said. It lives here, beside parseOpenAIRequest, rather than in
// anthropic.go — routing an Anthropic-file const to the OpenAI parser reads as a
// mistake even when it is not.
const bobPath = "/inference/v1/chat/completions"

func (p *InferenceParser) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	// Dispatch by endpoint dialect: OpenAI chat/completions vs Anthropic
	// Messages. No Invocation is recorded when the parser doesn't apply
	// (unrecognized path, empty body, or non-JSON body) — operators infer
	// "inference-parser is in this pipeline" from config, not per-event rows.
	var ext *pipeline.InferenceExtension
	switch endpointPath(pctx) {
	case "/v1/chat/completions", "/v1/completions", "/chat/completions", "/completions", bobPath:
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
			Role:         msg.Role,
			Content:      msg.Content,
			ContentBytes: msg.ContentBytes,
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
		if endpointPath(pctx) == anthropicMessagesPath {
			parseAnthropicSSE(pctx.ResponseBody, ext)
		} else {
			parseInferenceSSE(pctx.ResponseBody, ext)
		}
	} else {
		if endpointPath(pctx) == anthropicMessagesPath {
			parseAnthropicJSON(pctx.ResponseBody, ext)
		} else {
			parseInferenceJSON(pctx.ResponseBody, ext)
		}
	}

	logInferenceFinalized(ext)
	pctx.Observe("matched_" + ext.Model + "_response")
	return pipeline.Action{Type: pipeline.Continue}
}

// inferenceStreamState is the scratch state kept on pctx.Extensions.Custom
// for the duration of a streaming response. Provider-specific fold
// functions normalize their wire format into the neutral usage field;
// hasUsage flags whether any event carried usage counts (some providers
// omit the block unless the client opts in).
//
// A streamed Anthropic tool call is spread over many frames — id and name
// on the opening frame, arguments as fragments after it — so it has to be
// assembled here rather than read off any single frame. toolCalls keeps
// emission order; toolsByIndex resolves a fragment to its call, since
// interleaved blocks (a text block and two tool calls) are only
// distinguishable by the block index the provider stamps on each frame.
// openTool is the fallback for a provider that omits the index.
type inferenceStreamState struct {
	completion strings.Builder
	usage      parsercommon.TokenUsage
	hasUsage   bool

	toolCalls    []*anthropicToolCallState
	toolsByIndex map[int]*anthropicToolCallState
	openTool     *anthropicToolCallState
}

// finalize copies the accumulated stream state onto the public extension
// fields. Every write is an assignment rather than an accumulation, so a
// second finalize on the same state (the buffered OnResponse path running
// after a streaming pass) is a no-op instead of a double-count.
func (s *inferenceStreamState) finalize(ext *pipeline.InferenceExtension) {
	ext.Completion = s.completion.String()
	if s.hasUsage {
		s.usage.Fill(ext)
	}
	if len(s.toolCalls) == 0 {
		return
	}
	calls := make([]pipeline.InferenceToolCall, 0, len(s.toolCalls))
	for _, tc := range s.toolCalls {
		calls = append(calls, pipeline.InferenceToolCall{
			ID:        tc.id,
			Name:      tc.name,
			Arguments: tc.args.String(),
		})
	}
	ext.ToolCalls = calls
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
		if endpointPath(pctx) == anthropicMessagesPath {
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
		if endpointPath(pctx) == anthropicMessagesPath {
			foldAnthropicFrame(frame, state, ext)
		} else {
			foldOpenAIFrame(frame, state, ext)
		}
	}

	if last {
		state.finalize(ext)
		// Empty stream with no body and no chunks — record Skip to
		// pair the response row with the request row.
		//
		// Tool calls count as a body. A turn cancelled while the model was
		// still emitting tool arguments has no completion text, no finish
		// reason, and no usage block, but finalize has captured the call —
		// so skipping here would label a stream that demonstrably carried
		// content as having none, and drop it out of any timeline filtered
		// on observe.
		if ext.Completion == "" && ext.FinishReason == "" && ext.TotalTokens == 0 &&
			len(ext.ToolCalls) == 0 {
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
	// OpenAI streams cumulative usage: each usage-bearing chunk restates
	// the full totals, so replacing state.usage with the latest chunk's
	// neutral form is correct. Gate on hasAny so chunks with no usage
	// block (every non-final chunk) don't clear an accumulator that a
	// prior chunk populated.
	if chunk.Usage.hasAny() {
		state.usage = chunk.Usage.toNeutral()
		state.hasUsage = true
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

// logInferenceFinalized emits the operator-facing INFO log once a
// response is finalized; shared by the buffered and streaming paths.
// Split counters render -1 when ext.PresentKinds says the provider
// did not expose that sub-kind, distinct from a reported 0.
func logInferenceFinalized(ext *pipeline.InferenceExtension) {
	tok := func(bit parsercommon.Kind, v int) int {
		if ext.PresentKinds&uint8(bit) == 0 {
			return -1
		}
		return v
	}
	slog.Info("inference-parser: response",
		"model", ext.Model,
		"finishReason", ext.FinishReason,
		"promptTokens", ext.PromptTokens,
		"completionTokens", ext.CompletionTokens,
		"inputTokens", tok(parsercommon.KindInput, ext.InputTokens),
		"cacheReadTokens", tok(parsercommon.KindCacheRead, ext.CacheReadTokens),
		"cacheWriteTokens", tok(parsercommon.KindCacheWrite, ext.CacheWriteTokens),
		"outputTokens", tok(parsercommon.KindOutput, ext.OutputTokens),
		"reasoningTokens", tok(parsercommon.KindReasoning, ext.ReasoningTokens),
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
	// No usage block: leave PresentKinds at 0 (matches SSE path).
	if resp.Usage.hasAny() {
		resp.Usage.toNeutral().Fill(ext)
	}
}

// parseInferenceSSE concatenates content deltas across SSE events and captures
// the last finish_reason and usage block (sent when stream_options.include_usage
// is set). The stream terminates with a "data: [DONE]" marker which is skipped.
//
// OpenAI streams cumulative usage: each usage-bearing chunk restates the full
// totals, so the latest chunk's neutral form is authoritative. Accumulate into
// a local TokenUsage and Fill once at the end — matching foldOpenAIFrame's
// contract, so PresentKinds and ReportedTotal reflect only the final chunk.
func parseInferenceSSE(body []byte, ext *pipeline.InferenceExtension) {
	var completion strings.Builder
	var usage parsercommon.TokenUsage
	var hasUsage bool
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
		if chunk.Usage.hasAny() {
			usage = chunk.Usage.toNeutral()
			hasUsage = true
		}
	}
	ext.Completion = completion.String()
	if hasUsage {
		usage.Fill(ext)
	}
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

// inferenceUsage decodes the OpenAI usage block. All fields are pointers
// so "key absent" is distinguishable from "key present with value 0" —
// a total-only response must not assert KindInput/KindOutput.
type inferenceUsage struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	TotalTokens      *int `json:"total_tokens"`

	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// hasAny reports whether any recognized field was on the wire. Call
// sites gate toNeutral on this so an omitted or empty usage block
// doesn't overwrite a prior chunk's state.
func (u inferenceUsage) hasAny() bool {
	return u.PromptTokens != nil || u.CompletionTokens != nil || u.TotalTokens != nil ||
		u.PromptTokensDetails != nil || u.CompletionTokensDetails != nil
}

// toNeutral maps OpenAI's usage onto TokenUsage. prompt_tokens includes
// cached_tokens on the wire — subtract to get uncached input and clamp
// at 0 for malformed responses. CacheWrite stays absent (OpenAI bills
// cache writes as ordinary input). Each Present bit is set only when
// its key was on the wire, so an absent key stays "not exposed"
// (-1 sentinel) rather than "reported zero."
func (u inferenceUsage) toNeutral() parsercommon.TokenUsage {
	usage := parsercommon.TokenUsage{}
	if u.TotalTokens != nil {
		usage.ReportedTotal = *u.TotalTokens
	}
	if u.PromptTokens != nil {
		usage.Input = *u.PromptTokens
		usage.Present |= parsercommon.KindInput
	}
	if u.CompletionTokens != nil {
		usage.Output = *u.CompletionTokens
		usage.Present |= parsercommon.KindOutput
	}
	if u.PromptTokensDetails != nil {
		cached := u.PromptTokensDetails.CachedTokens
		usage.Input -= cached
		if usage.Input < 0 {
			usage.Input = 0
		}
		usage.CacheRead = cached
		usage.Present |= parsercommon.KindCacheRead
	}
	if u.CompletionTokensDetails != nil {
		usage.Reasoning = u.CompletionTokensDetails.ReasoningTokens
		usage.Present |= parsercommon.KindReasoning
	}
	return usage
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
//
// ContentBytes records the size of the content value before that reduction,
// so a message the model was billed for doesn't read as empty just because
// none of it was text.
type inferenceMessage struct {
	Role         string
	Content      string
	ContentBytes int
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
	m.ContentBytes = contentBytes(raw.Content)
	return nil
}

// contentBytes is the wire size of a message's content value, and the source
// of InferenceMessage.ContentBytes. Absent and null content report 0 rather
// than the 4 bytes the literal `null` occupies — the field is a size signal
// for content that exists, and an assistant turn that carries only tool_calls
// has none.
//
// raw is the client's bytes verbatim, so the count includes any whitespace the
// client's serializer emitted. That is deliberate: this measures what was
// sent. Compacting first would buy comparability across clients at the cost of
// an allocation per message on every request-body parse, and would no longer
// answer "how big was this on the wire".
func contentBytes(raw json.RawMessage) int {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0
	}
	return len(raw)
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
