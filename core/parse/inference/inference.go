// Package inference parses LLM inference requests and responses from plain
// values: Anthropic Messages, OpenAI Chat Completions and the Responses API.
// It computes no cost and does not use the plugin engine (#1503). The
// inference-parser plugin is a thin wrapper over it, and prices what it parses.
package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/rossoctl/cortex/core/internal/parsercommon"
	"github.com/rossoctl/cortex/core/model"
)

// ParseRequest parses one inference request from plain values. path must be
// query-free. It returns nil when the parser does not claim the request.
func ParseRequest(path string, headers http.Header, body []byte) *model.InferenceExtension {
	// Decode once for every dialect: no listener decompresses a request body.
	body = parsercommon.DecodeRequestBody(headers, body)
	var ext *model.InferenceExtension
	switch dialectFor(path) {
	case dialectAnthropic:
		ext = parseAnthropicRequest(body)
	case dialectOpenAI:
		ext = parseOpenAIRequest(body)
	case dialectResponses:
		ext = parseResponsesRequest(body)
	default:
		return nil
	}
	if ext == nil {
		// "no telemetry", not "skipping": this request is still charged if the gateway
		// says it cost something. Wording that describes the whole request as dropped would
		// make an operator hunting for missing spend rule out the one path that still
		// records some.
		slog.Debug("inference-parser: no/invalid body; no request telemetry, response still priced from the gateway header", "path", model.SanitizeLogValue(path))
		return nil
	}

	// Which caller made this request, read off the system prompt — both dialects, one call,
	// because by here they have converged on a parsed extension whose system message is
	// flattened the same way. See agentRole for the marker and what empty means.
	ext.AgentRole = agentRole(ext.Messages)
	ext.AnthropicBeta = anthropicBeta(headers)

	slog.Info("inference-parser", "model", model.SanitizeLogValue(ext.Model))
	slog.Debug("inference-parser: extracted", "model", model.SanitizeLogValue(ext.Model), "messages", len(ext.Messages), "stream", ext.Stream, "tools", len(ext.Tools))
	// Guarded: the arguments are evaluated even when Debug is off, and sanitizing allocates.
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		for i, m := range ext.Messages {
			slog.Debug("inference-parser: message", "index", i, "role", model.SanitizeLogValue(m.Role),
				"content", model.SanitizeLogValue(parsercommon.Truncate(m.Content, parsercommon.DebugBodyMax)))
		}
	}

	return ext
}

// ParseResponse reads a whole response body into a fresh extension, and
// computes no cost. It is for a caller that has the body but not the request's
// extension, and needs the normalized completion and tool calls.
// The bytes decide the shape: a body framed as SSE is read as a
// stream (and marks StreamedResponse), anything else as JSON. It returns nil
// when path is not an inference endpoint.
func ParseResponse(path string, body []byte) *model.InferenceExtension {
	path, _, _ = strings.Cut(path, "?")
	if dialectFor(path) == dialectNone {
		return nil
	}
	stream := CarriesSSEFraming(body)
	ext := &model.InferenceExtension{Stream: stream}
	ReadResponse(path, stream, body, ext)
	return ext
}

// ReadResponse reads a whole response body into ext, the extension of its request.
// sse says which reader runs: the SSE reader for a stream delivered whole, with its
// wire framing, else the JSON reader. The caller decides it, because the evidence is
// the caller's: the Content-Type, the request's stream flag, or CarriesSSEFraming.
// path must be query-free. A path with no dialect gets the OpenAI readers.
func ReadResponse(path string, sse bool, body []byte, ext *model.InferenceExtension) {
	switch d := dialectFor(path); {
	case sse && d == dialectAnthropic:
		parseAnthropicSSE(body, ext)
	case sse && d == dialectResponses:
		parseResponsesSSE(body, ext)
	case sse:
		parseInferenceSSE(body, ext)
	case d == dialectAnthropic:
		parseAnthropicJSON(body, ext)
	case d == dialectResponses:
		parseResponsesJSON(body, ext)
	default:
		parseInferenceJSON(body, ext)
	}
}

// Stream folds one response that a listener hands over frame by frame, each frame
// one SSE event's payload with the framing stripped. Fold each frame, then call
// Finish once: the usage and the tool calls reach ext only there.
type Stream struct {
	d     dialect
	state inferenceStreamState
}

// NewStream starts the fold of a streamed response to a request on path, which
// must be query-free.
func NewStream(path string) *Stream { return &Stream{d: dialectFor(path)} }

// Fold folds one frame into the stream. It also marks ext.StreamedResponse, even
// for an empty frame: the response was observed streaming, which is what
// pricing.IncompleteReason reads to decide whether an output tally is final.
// The request's own stream flag cannot answer that: a gateway may answer a
// non-streaming request with SSE.
func (s *Stream) Fold(frame []byte, ext *model.InferenceExtension) {
	ext.StreamedResponse = true
	if len(frame) > 0 {
		foldResponseFrame(s.d, frame, &s.state, ext)
	}
}

// Finish copies what the stream assembled onto ext. A second Finish writes the
// same values again, so it never double-counts.
func (s *Stream) Finish(ext *model.InferenceExtension) { s.state.finalize(ext) }

// parseOpenAIRequest builds an InferenceExtension from an OpenAI
// chat/completions (or completions) request body. Returns nil for an empty or
// non-JSON body, and for one that is not an inference request. Every populated
// extension is an outbound LLM call — an agent action (IsAction); the "don't
// judge inference by default" choice is operator policy in IBAC, independent of
// this classification.
func parseOpenAIRequest(body []byte) *model.InferenceExtension {
	if len(body) == 0 {
		return nil
	}
	var req inferenceRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	// THE PATH SAID OPENAI; THE BODY HAS TO AGREE. dialectFor matches how a path ends, which
	// reaches every provider's prefix and also any endpoint that merely ends the same way,
	// and every extension built here is read downstream as an agent's LLM call. It does more
	// than label the request: it exempts it from IBAC's judge unless judge_inference is on,
	// even under unclassified_policy: judge, and from settle's cap on an implausible gateway
	// cost (implausibleUnparsedCost). So one is built only for a body carrying what such a
	// call cannot do without: a messages array, or a legacy completions prompt. A model is
	// not required — Azure names the deployment in the path and sends none.
	if req.Messages == nil && !jsonPresent(req.Prompt) {
		return nil
	}
	ext := &model.InferenceExtension{
		Model:       req.Model,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		TopP:        req.TopP,
		Stream:      req.Stream,
		ToolChoice:  req.ToolChoice,
		IsAction:    true,
		Effort:      jsonString(req.ReasoningEffort),
	}
	for _, msg := range req.Messages {
		ext.Messages = append(ext.Messages, model.InferenceMessage{
			Role:          msg.Role,
			Content:       msg.Content,
			ContentBytes:  msg.ContentBytes,
			ToolUseID:     msg.ToolCallID,
			ToolCallNames: msg.ToolCallNames,
			ToolUseIDs:    msg.ToolUseIDs,
		})
		ext.HistoryToolCalls = append(ext.HistoryToolCalls, msg.ToolCalls...)
	}
	// A chat-completions tool result is a "role":"tool" message naming the call it answers
	// by tool_call_id — the pairing key the timeline's tool spans close on. Anthropic and
	// the Responses API surface theirs through the same ToolResults field.
	for _, msg := range req.Messages {
		if msg.Role == "tool" && msg.ToolCallID != "" {
			ext.ToolResults = append(ext.ToolResults, model.InferenceToolResult{
				ToolUseID: msg.ToolCallID,
				Content:   msg.Content,
			})
		}
	}
	for _, tool := range req.Tools {
		if tool.Function.Name == "" {
			continue
		}
		ext.Tools = append(ext.Tools, model.InferenceTool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Parameters:  schemaObject(tool.Function.Parameters),
		})
	}
	return ext
}

// jsonPresent reports whether a field was sent with a value. An absent field decodes to an
// empty RawMessage and an explicit null to the bytes "null"; neither counts.
func jsonPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

// inferenceStreamState is the scratch state kept on pctx.Extensions.Custom
// for the duration of a streaming response. Provider-specific fold
// functions normalize their wire format into the neutral usage field;
// hasUsage flags whether any event carried usage counts (some providers
// omit the block unless the client opts in).
//
// A streamed tool call is spread over many frames on both dialects that
// fragment one — id and name up front, arguments as fragments after — so it
// has to be assembled here rather than read off any single frame. The
// Anthropic fields keep emission order (toolCalls), resolve a fragment to
// its call by the block index the provider stamps on each frame
// (toolsByIndex), and fall back to the most recently opened call for a
// provider that omits the index (openTool).
//
// openAIToolCalls/openAIByIndex are the same assembly for OpenAI Chat
// Completions streams, keyed by the index each delta.tool_calls fragment
// carries. Separate fields rather than shared ones: the two indexes are
// different namespaces, and only one dialect ever runs against one state —
// keeping them apart costs nothing and means an OpenAI fragment can never be
// resolved into a half-open Anthropic block by a misrouted frame.
//
// responsesToolCalls holds the Responses API's tool calls instead — a
// different shape with the same destination. A call opens on
// response.output_item.added and is completed whole by its
// response.output_item.done (see responses.go); one whose done never arrives
// stays as the stream left it, as an Anthropic block cut mid-arguments does.
type inferenceStreamState struct {
	completion strings.Builder
	usage      parsercommon.TokenUsage
	hasUsage   bool

	toolCalls    []*toolCallState
	toolsByIndex map[int]*toolCallState
	openTool     *toolCallState

	openAIToolCalls []*toolCallState
	openAIByIndex   map[int]*toolCallState

	responsesToolCalls []*responsesCallState

	// thinking counts the Anthropic thinking blocks: opened on content_block_start, their
	// bytes added by thinking_delta and signature_delta.
	thinking model.InferenceThinking
}

// foldOpenAIToolCallFragment folds one delta.tool_calls fragment into the call it
// belongs to, opening one at that index on first sight. Only the first fragment for
// an index carries the id and function name; later ones carry only argument
// fragments, so each field is written when present rather than required.
func (s *inferenceStreamState) foldOpenAIToolCallFragment(tc *inferenceStreamToolCall) {
	acc := s.openAIByIndex[tc.Index]
	if acc == nil {
		acc = &toolCallState{}
		s.openAIToolCalls = append(s.openAIToolCalls, acc)
		if s.openAIByIndex == nil {
			s.openAIByIndex = map[int]*toolCallState{}
		}
		s.openAIByIndex[tc.Index] = acc
	}
	if tc.ID != "" {
		acc.id = tc.ID
	}
	if tc.Function.Name != "" {
		acc.name = tc.Function.Name
	}
	acc.args.WriteString(tc.Function.Arguments)
}

// finalize copies the accumulated stream state onto the public extension
// fields. Every write is an assignment rather than an accumulation, so a
// second finalize on the same state (the buffered OnResponse path running
// after a streaming pass) is a no-op instead of a double-count.
func (s *inferenceStreamState) finalize(ext *model.InferenceExtension) {
	ext.Completion = s.completion.String()
	if s.hasUsage {
		s.usage.Fill(ext)
	}
	if t := s.thinking; t.Blocks > 0 {
		ext.ResponseThinking = &t
	}
	calls := make([]model.InferenceToolCall, 0, len(s.toolCalls)+len(s.openAIToolCalls)+len(s.responsesToolCalls))
	for _, tc := range s.toolCalls {
		calls = append(calls, model.InferenceToolCall{
			ID:        tc.id,
			Name:      tc.name,
			Arguments: tc.args.String(),
		})
	}
	for _, tc := range s.openAIToolCalls {
		calls = append(calls, model.InferenceToolCall{
			ID:        tc.id,
			Name:      tc.name,
			Arguments: tc.args.String(),
		})
	}
	for _, tc := range s.responsesToolCalls {
		calls = append(calls, model.InferenceToolCall{
			ID:        tc.id,
			Name:      tc.name,
			Arguments: tc.args.String(),
		})
	}
	if len(calls) == 0 {
		return
	}
	ext.ToolCalls = calls
}

// CarriesSSEFraming reports whether a frame holds RAW SSE WIRE BYTES — it has at least one
// `data:` field — rather than one payload with the framing already stripped.
//
// The question only arises on the terminal call, and the answer decides which parser can
// read it. A listener that buffered a text/event-stream response hands over the whole wire
// body, framing included; every per-frame dispatch goes through sseframe.Reader, which
// strips the framing and hands over the payload alone. Feeding wire bytes to the chunk fold
// parses nothing, and feeding a bare payload to the SSE reader parses nothing — each of
// them silently, which is why the shapes are told apart here instead.
func CarriesSSEFraming(frame []byte) bool {
	frame = parsercommon.NormalizeSSE(frame)
	if bytes.HasPrefix(bytes.TrimLeft(frame, " \t\r\n"), []byte("data:")) {
		return true
	}
	return bytes.Contains(frame, []byte("\ndata:"))
}

// markStreamedResponse records that the RESPONSE arrived as a stream, which is what
// pricing.IncompleteReason reads to decide whether a counted output tally is FINAL.
//
// THE PER-FRAME PATH ALREADY SAID SO AND THE BUFFERED ONE DID NOT. Stream.Fold sets the
// flag, and it is reached only from a per-frame dispatch — so a text/event-stream response that a
// listener handed over WHOLE was parsed as a stream and then described as a non-stream. Both
// proxies take that path for real: they fall back to buffering an event-stream response when a
// plugin in the chain declares WritesResponseBody. Measured on the OpenAI dialect, same bytes, same
// counters: frame by frame gave Incomplete=true "output-uncounted", buffered gave Incomplete=false
// — a floor published as a whole figure, which is the exact failure the field exists to close.
//
// ON THE EVIDENCE OF THE BYTES, NOT THE REQUEST'S FLAG, and not on the mere fact that an SSE parser
// was called. OnResponse picks its parser from ext.Stream, and a JSON reply to a streaming request
// is routine — every gateway error page is one — so marking whatever that arm parses would print
// the "+ partial" caveat over figures that are exact. A caveat on correct figures is one readers
// learn to ignore, so the claim has to come from the wire: data: fields present means the response
// really did arrive as a stream. That is the same rule the dispatch already follows.
//
// Anthropic is unaffected by the underlying gap — its Output is assigned only in the message_delta
// arm, which carries stop_reason on the same frame — but it is marked here too, because the fact is
// about the response's shape and not about who reads it.
func markStreamedResponse(body []byte, ext *model.InferenceExtension) {
	if ext != nil && CarriesSSEFraming(body) {
		ext.StreamedResponse = true
	}
}

// foldResponseFrame folds one streamed frame into the running state via the dialect d the
// endpoint speaks. Extracted so the mid-stream and terminal call sites cannot drift apart
// on which parser a path gets.
func foldResponseFrame(d dialect, frame []byte, state *inferenceStreamState, ext *model.InferenceExtension) {
	switch d {
	case dialectAnthropic:
		foldAnthropicFrame(frame, state, ext)
	case dialectResponses:
		foldResponsesFrame(frame, state, ext)
	default:
		foldOpenAIFrame(frame, state, ext)
	}
}

// foldOpenAIFrame folds one OpenAI streaming chunk (data: {choices,usage}) into
// the running stream state. The "[DONE]" sentinel and malformed chunks are
// skipped. Usage arrives (cumulative) when the client set
// stream_options.include_usage. Tool calls arrive as index-keyed fragments
// and are assembled by foldOpenAIToolCallFragment.
func foldOpenAIFrame(frame []byte, state *inferenceStreamState, ext *model.InferenceExtension) {
	if bytes.Equal(bytes.TrimSpace(frame), []byte("[DONE]")) {
		return
	}
	var chunk inferenceStreamChunk
	if err := json.Unmarshal(frame, &chunk); err != nil {
		slog.Debug("inference-parser: malformed streaming chunk, skipping", "error", err)
		return
	}
	noteBodyModel(ext, chunk.Model)
	for _, c := range chunk.Choices {
		if c.Delta.Content != "" {
			state.completion.WriteString(c.Delta.Content)
		}
		for i := range c.Delta.ToolCalls {
			state.foldOpenAIToolCallFragment(&c.Delta.ToolCalls[i])
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

// parseInferenceJSON parses a non-streaming OpenAI chat/completions response.
func parseInferenceJSON(body []byte, ext *model.InferenceExtension) {
	var resp inferenceResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		slog.Debug("inference-parser: invalid response JSON", "error", err)
		return
	}
	noteBodyModel(ext, resp.Model)
	if len(resp.Choices) > 0 {
		c := resp.Choices[0]
		ext.Completion = c.Message.Content
		ext.FinishReason = c.FinishReason
		for _, tc := range c.Message.ToolCalls {
			ext.ToolCalls = append(ext.ToolCalls, model.InferenceToolCall{
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
// the shared TokenUsage and Fill once at the end — matching foldOpenAIFrame's
// contract, so PresentKinds and ReportedTotal reflect only the final chunk.
//
// Tool calls fold through the same inferenceStreamState the frame path uses,
// so the two paths cannot drift on what a fragmented call assembles into.
func parseInferenceSSE(body []byte, ext *model.InferenceExtension) {
	markStreamedResponse(body, ext)
	state := &inferenceStreamState{}
	for _, line := range bytes.Split(parsercommon.NormalizeSSE(body), []byte("\n")) {
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
			slog.Debug("inference-parser: skipping malformed SSE data frame", "error", err, "data", model.SanitizeLogValue(parsercommon.Truncate(string(data), 128)))
			continue
		}
		noteBodyModel(ext, chunk.Model)
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				state.completion.WriteString(c.Delta.Content)
			}
			for i := range c.Delta.ToolCalls {
				state.foldOpenAIToolCallFragment(&c.Delta.ToolCalls[i])
			}
			if c.FinishReason != "" {
				ext.FinishReason = c.FinishReason
			}
		}
		if chunk.Usage.hasAny() {
			state.usage = chunk.Usage.toNeutral()
			state.hasUsage = true
		}
	}
	state.finalize(ext)
}

type inferenceResponse struct {
	Model   string            `json:"model"`
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
	Model   string                  `json:"model"`
	Choices []inferenceStreamChoice `json:"choices"`
	Usage   inferenceUsage          `json:"usage"`
}

type inferenceStreamChoice struct {
	Delta        inferenceDelta `json:"delta"`
	FinishReason string         `json:"finish_reason"`
}

type inferenceDelta struct {
	Content   string                    `json:"content"`
	ToolCalls []inferenceStreamToolCall `json:"tool_calls"`
}

// inferenceStreamToolCall is one entry of a streamed delta's tool_calls array.
// Unlike inferenceRespToolCall (the non-streaming shape, one self-contained
// object per call), each entry is a FRAGMENT: the index names which
// accumulating call it belongs to, only the first fragment for an index
// carries the id and function name, and the arguments arrive as
// JSON-string fragments that are only valid once every fragment for that
// index is concatenated — the same assembly problem as Anthropic's
// input_json_delta, keyed differently. The index is what OpenAI documents
// and what every observed stream stamps on each fragment; a fragment
// without one reads as index 0.
type inferenceStreamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
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
	// Prompt is read only to tell a legacy completions request from a body that is not
	// inference at all; see parseOpenAIRequest. It is not surfaced.
	Prompt json.RawMessage `json:"prompt"`
	// Raw for the reason anthropicRequest.Thinking is.
	ReasoningEffort json.RawMessage `json:"reasoning_effort"`
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
	// ToolCallID is the tool_call_id of a chat-completions "role":"tool" result message:
	// the id of the assistant tool_calls entry it answers. Empty on every other role.
	ToolCallID string
	// ToolCallNames names an assistant message's tool_calls, joined by ", ", and
	// ToolUseIDs gives their ids the same way. ToolCalls are the calls themselves, for
	// InferenceExtension.HistoryToolCalls.
	ToolCallNames string
	ToolUseIDs    string
	ToolCalls     []model.InferenceToolCall
}

func (m *inferenceMessage) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"tool_call_id"`
		ToolCalls  []struct {
			ID       json.RawMessage `json:"id"`
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.Content = flattenContent(raw.Content)
	m.ContentBytes = contentBytes(raw.Content)
	m.ToolCallID = raw.ToolCallID
	names := make([]string, len(raw.ToolCalls))
	ids := make([]string, len(raw.ToolCalls))
	for i, c := range raw.ToolCalls {
		names[i], ids[i] = c.Function.Name, rawText(c.ID)
		m.ToolCalls = append(m.ToolCalls, model.InferenceToolCall{
			ID: ids[i], Name: c.Function.Name, Arguments: rawText(c.Function.Arguments),
		})
	}
	m.ToolCallNames, m.ToolUseIDs = strings.Join(names, ", "), strings.Join(ids, ", ")
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

// rawText is a JSON string's value, or the raw JSON text of any other value, so a field read
// only for display cannot fail the decode of the request around it. "" when absent or null.
func rawText(raw json.RawMessage) string {
	if !jsonPresent(raw) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
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

// schemaObject keeps a tool's JSON schema as it arrived, or drops it entirely.
//
// It replaces two identical map-decoding helpers (one here, one in anthropic.go) whose
// only product was a map[string]any the store no longer keeps — the schemas are held as
// model.RawJSON now so core/session can share one copy per session instead of one
// per event. See model.RawJSON for why the field is a string.
//
// Dropping a non-object preserves the previous contract rather than widening it. Those
// helpers returned nil whenever their unmarshal failed, so a schema sent as a string, a
// number, an array, or null was already absent by the time anything read it, and callers
// are written against "an object or nothing" — OPA policies index into the schema, and
// sparc forwards it to an external collector. Note null lands the same way both old and
// new: it unmarshalled into a nil map without error before, and fails the byte check now.
//
// A byte peek rather than an unmarshal, which is the point of the change: the decode this
// replaces cost time and heap proportional to the schema on every request, and a client
// re-sends its whole tool manifest on every request.
func schemaObject(raw json.RawMessage) model.RawJSON {
	// TrimLeft returns a subslice, so establishing the first meaningful byte allocates
	// nothing. The conversion below is the single copy, and it is the one the interner
	// then collapses across events.
	//
	// The trimmed value is what gets returned, not the original: a decoder never hands us
	// leading whitespace anyway, so the two are the same slice in practice, and keeping them
	// the same avoids peeking at one thing while storing another.
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return ""
	}
	return model.RawJSON(trimmed)
}
