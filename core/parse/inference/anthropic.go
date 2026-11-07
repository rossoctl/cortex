package inference

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/rossoctl/cortex/core/internal/parsercommon"
	"github.com/rossoctl/cortex/core/model"
)

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
	// Raw, so a value of another type leaves the request parsed; see stringField.
	Thinking     json.RawMessage `json:"thinking"`
	OutputConfig json.RawMessage `json:"output_config"`
}

// stringField returns the string value of key in the JSON object raw, or "" when raw is not an
// object or the value is not a string. For fields read for display only: a client that sends
// another type must not cost the request its parse.
func stringField(raw json.RawMessage, key string) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	return jsonString(obj[key])
}

// jsonString is the value of the JSON string raw, or "" when raw is not a string.
func jsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// rawStringLen is the length of the JSON string raw, or 0 when it is not a string.
func rawStringLen(raw json.RawMessage) int { return len(jsonString(raw)) }

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
//
// ContentBytes records the size of what was there before that reduction, so
// the blocks flattenContent drops are still accounted for. In an agent loop
// the dropped blocks are the bulk of the conversation: every tool result comes
// back as a tool_result block, so a turn that reads a large file shows up as an
// empty Content and would otherwise look free.
//
// ToolResults keeps the one dropped block type worth reading back: tool_result, the output
// of an earlier tool call. It goes to InferenceExtension.ToolResults, never into Content.
// ToolCalls keeps the tool_use blocks, for InferenceExtension.HistoryToolCalls, and
// ToolCallNames their names, so a turn that only called tools does not read as empty.
// ToolUseIDs keys the message to both.
type anthropicReqMessage struct {
	Role          string
	Content       string
	ContentBytes  int
	ToolResults   []model.InferenceToolResult
	ToolCalls     []model.InferenceToolCall
	ToolCallNames string
	ToolUseIDs    string
	Thinking      model.InferenceThinking
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
	m.Content, m.ToolResults, m.ToolCalls, m.Thinking = flattenAnthropicContent(raw.Content)
	names := make([]string, 0, len(m.ToolCalls))
	ids := make([]string, 0, len(m.ToolCalls)+len(m.ToolResults))
	for _, c := range m.ToolCalls {
		names, ids = append(names, c.Name), append(ids, c.ID)
	}
	for _, r := range m.ToolResults {
		ids = append(ids, r.ToolUseID)
	}
	m.ToolCallNames, m.ToolUseIDs = strings.Join(names, ", "), strings.Join(ids, ", ")
	m.ContentBytes = contentBytes(raw.Content)
	return nil
}

// flattenAnthropicContent is flattenContent for an Anthropic message, which can also carry
// tool_result blocks. One decode of the block array yields both the text and the results,
// so a request full of tool output is not parsed twice. A result's own content is a string
// or text blocks, which flattenContent already reads; its non-text blocks (an image a Read
// returned) are dropped, as flattenContent drops them everywhere. The tool_use blocks give
// the calls, arguments as sent. The thinking blocks are counted, not kept.
func flattenAnthropicContent(raw json.RawMessage) (string, []model.InferenceToolResult, []model.InferenceToolCall, model.InferenceThinking) {
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ID        json.RawMessage `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
		Thinking  json.RawMessage `json:"thinking"`
		Signature json.RawMessage `json:"signature"`
		Data      json.RawMessage `json:"data"`
	}
	var thinking model.InferenceThinking
	if err := json.Unmarshal(raw, &blocks); err != nil {
		// A plain string, null, or a shape we do not model: none of them holds a tool block.
		return flattenContent(raw), nil, nil, thinking
	}
	var text strings.Builder
	var results []model.InferenceToolResult
	var calls []model.InferenceToolCall
	for _, b := range blocks {
		thinking.Add(b.Type, rawStringLen(b.Thinking), rawStringLen(b.Signature), rawStringLen(b.Data))
		switch b.Type {
		case "text":
			if b.Text != "" {
				if text.Len() > 0 {
					text.WriteByte('\n')
				}
				text.WriteString(b.Text)
			}
		case "tool_result":
			results = append(results, model.InferenceToolResult{
				ToolUseID: b.ToolUseID, Content: flattenContent(b.Content), IsError: b.IsError,
			})
		case "tool_use":
			calls = append(calls, model.InferenceToolCall{ID: rawText(b.ID), Name: b.Name, Arguments: string(b.Input)})
		}
	}
	return text.String(), results, calls, thinking
}

// parseAnthropicRequest builds an InferenceExtension from an Anthropic Messages
// request body. Returns nil for an empty or non-JSON body, or one without a
// messages array (caller treats nil as "not an inference request we can parse"
// and continues).
func parseAnthropicRequest(body []byte) *model.InferenceExtension {
	if len(body) == 0 {
		return nil
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}

	// The path said Anthropic; the body has to agree — see parseOpenAIRequest. A messages
	// array is the one thing every Messages API request carries.
	if req.Messages == nil {
		return nil
	}

	ext := &model.InferenceExtension{
		Model:       req.Model,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		TopP:        req.TopP,
		Stream:      req.Stream,
		ToolChoice:  req.ToolChoice,
		// Every populated InferenceExtension is an outbound LLM call — an
		// agent action. Same classification as the OpenAI path.
		IsAction: true,

		ThinkingType:    stringField(req.Thinking, "type"),
		ThinkingDisplay: stringField(req.Thinking, "display"),
		Effort:          stringField(req.OutputConfig, "effort"),
	}

	// Anthropic carries the system prompt top-level, not as a message role.
	// Surface it as a leading system message so downstream policy plugins
	// (IBAC, etc.) see it the same way they see OpenAI's system message.
	if sys := flattenContent(req.System); sys != "" {
		ext.Messages = append(ext.Messages, model.InferenceMessage{
			Role: "system", Content: sys, ContentBytes: contentBytes(req.System),
		})
	}
	var history model.InferenceThinking
	for _, msg := range req.Messages {
		ext.Messages = append(ext.Messages, model.InferenceMessage{
			Role: msg.Role, Content: msg.Content, ContentBytes: msg.ContentBytes,
			ToolCallNames: msg.ToolCallNames, ToolUseIDs: msg.ToolUseIDs,
		})
		ext.ToolResults = append(ext.ToolResults, msg.ToolResults...)
		ext.HistoryToolCalls = append(ext.HistoryToolCalls, msg.ToolCalls...)
		history.Merge(msg.Thinking)
	}
	ext.HistoryThinking = history.OrNil()
	for _, tool := range req.Tools {
		if tool.Name == "" {
			continue
		}
		ext.Tools = append(ext.Tools, model.InferenceTool{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  schemaObject(tool.InputSchema),
		})
	}
	return ext
}

// --- usage (shared by response + streaming) ---

// anthropicUsage mirrors the Messages API usage block. The true input size is
// input_tokens + cache_creation_input_tokens + cache_read_input_tokens (cached
// context still counts as input); promptTotal sums them.
//
// Cache fields are *int so an omitted field on the wire (e.g. message_start
// on the ?beta=true path) stays absent in Present rather than being asserted
// as a reported zero. OutputTokensDetails is a pointer for the same reason, and
// it needs it more: it is absent on message_start and on message_stop, and
// present only on message_delta.
type anthropicUsage struct {
	InputTokens              int  `json:"input_tokens"`
	OutputTokens             int  `json:"output_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`

	// OutputTokensDetails splits output_tokens by what generated it. Anthropic reports
	// one sub-field, thinking_tokens: the share spent on internal reasoning, a SUBSET of
	// OutputTokens rather than a sibling.
	OutputTokensDetails *struct {
		ThinkingTokens *int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

// toNeutral maps Anthropic's usage onto TokenUsage. Input and Output are
// always emitted by the Messages API; cache sub-fields and the output-token
// details are observed via their pointers so an absent field stays absent in
// Present.
//
// Reasoning comes from output_tokens_details.thinking_tokens, verified against a live
// claude-opus-5 turn and documented at
// https://platform.claude.com/docs/en/build-with-claude/thinking-steering-and-cost
func (u anthropicUsage) toNeutral() parsercommon.TokenUsage {
	n := parsercommon.TokenUsage{
		Input:   u.InputTokens,
		Output:  u.OutputTokens,
		Present: parsercommon.KindInput | parsercommon.KindOutput,
	}
	if u.CacheReadInputTokens != nil {
		n.CacheRead = *u.CacheReadInputTokens
		n.Present |= parsercommon.KindCacheRead
	}
	if u.CacheCreationInputTokens != nil {
		n.CacheWrite = *u.CacheCreationInputTokens
		n.Present |= parsercommon.KindCacheWrite
	}
	// BOTH POINTERS: a details object forwarded without the count inside it reports
	// nothing, and must not set the bit.
	if u.OutputTokensDetails != nil && u.OutputTokensDetails.ThinkingTokens != nil {
		n.Reasoning = *u.OutputTokensDetails.ThinkingTokens
		n.Present |= parsercommon.KindReasoning
	}
	return n
}

// --- non-streaming response ---

type anthropicResponse struct {
	Model      string                  `json:"model"`
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
	// Raw for the reason anthropicRequest.Thinking is.
	Thinking  json.RawMessage `json:"thinking"`
	Signature json.RawMessage `json:"signature"`
	Data      json.RawMessage `json:"data"`
}

// parseAnthropicJSON parses a non-streaming Messages response: text blocks ->
// completion, tool_use blocks -> tool calls, usage -> token counts.
func parseAnthropicJSON(body []byte, ext *model.InferenceExtension) {
	var resp anthropicResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return
	}
	noteBodyModel(ext, resp.Model)
	var b strings.Builder
	var thinking model.InferenceThinking
	for _, blk := range resp.Content {
		thinking.Add(blk.Type, rawStringLen(blk.Thinking), rawStringLen(blk.Signature), rawStringLen(blk.Data))
		switch blk.Type {
		case "text":
			if blk.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(blk.Text)
			}
		case "tool_use":
			ext.ToolCalls = append(ext.ToolCalls, model.InferenceToolCall{
				ID:        blk.ID,
				Name:      blk.Name,
				Arguments: string(blk.Input),
			})
		}
	}
	ext.Completion = b.String()
	ext.ResponseThinking = thinking.OrNil()
	if resp.StopReason != "" {
		ext.FinishReason = resp.StopReason
	}
	resp.Usage.toNeutral().Fill(ext)
}

// --- streaming ---

// anthropicStreamEvent is one SSE event's data payload. The Messages stream is
// a sequence of typed events (vs OpenAI's uniform chat.completion.chunk):
// message_start (carries usage — but see below), content_block_delta (text_delta /
// input_json_delta / thinking_delta), message_delta (delta.stop_reason +
// cumulative usage.output_tokens, and on the ?beta=true path the prompt-cache
// counts too), message_stop, plus ping/content_block_*.
type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Model string         `json:"model"`
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	Delta *struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
		// PartialJSON carries a fragment of a tool call's arguments on an
		// input_json_delta. The model streams tool arguments as text that
		// is only valid JSON once every fragment is concatenated.
		PartialJSON string `json:"partial_json"`
		// A thinking_delta's text and a signature_delta's signature. Raw, so a frame
		// carrying another type still yields its usage.
		Thinking  json.RawMessage `json:"thinking"`
		Signature json.RawMessage `json:"signature"`
	} `json:"delta"`
	Usage *anthropicUsage `json:"usage"`

	// Error carries the payload of an "error" stream event (overloaded_error,
	// api_error, and friends). Anthropic can abort a stream mid-flight with
	// this instead of the usual message_delta/message_stop pair, in which case
	// no usage ever arrives — so it is the single most informative event when
	// token telemetry comes back empty, and worth surfacing rather than
	// silently ignoring.
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`

	// Index identifies which content block an event belongs to. A response
	// may contain several blocks (text plus one or more tool calls), and
	// their deltas are only distinguishable by this index.
	Index *int `json:"index"`
	// ContentBlock is the opening descriptor on a content_block_start. For a
	// tool call it carries the id and name; the arguments follow as deltas.
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
		// A redacted_thinking block arrives whole here, with no deltas after it.
		Data json.RawMessage `json:"data"`
	} `json:"content_block"`
}

// toolCallState accumulates one streamed tool call, for either dialect that
// fragments one across frames. Anthropic: the id and name arrive on
// content_block_start, the arguments as input_json_delta fragments. OpenAI
// Chat Completions: the id and name arrive on the first delta.tool_calls
// fragment for an index, the arguments as function.arguments fragments. In
// both, the arguments are only valid JSON once every fragment is
// concatenated.
type toolCallState struct {
	id   string
	name string
	args strings.Builder
}

// openAnthropicTool starts accumulating a tool call for content block index.
// The entry is pointer-held: a strings.Builder must not be copied once used,
// which a value slice would do the moment append reallocates.
func (s *inferenceStreamState) openAnthropicTool(index *int, id, name string) {
	tc := &toolCallState{id: id, name: name}
	s.toolCalls = append(s.toolCalls, tc)
	if index != nil {
		if s.toolsByIndex == nil {
			s.toolsByIndex = map[int]*toolCallState{}
		}
		s.toolsByIndex[*index] = tc
	}
	s.openTool = tc
}

// anthropicTool resolves the tool call a delta belongs to. Nil index falls
// back to the most recently opened call — blocks are emitted sequentially,
// so that is the same call the index would have named.
func (s *inferenceStreamState) anthropicTool(index *int) *toolCallState {
	if index != nil {
		if tc, ok := s.toolsByIndex[*index]; ok {
			return tc
		}
		// An indexed delta for a block we never saw open is a text block's
		// delta or a shape we don't model — not the open tool's arguments.
		return nil
	}
	return s.openTool
}

// closeAnthropicTool drops the fallback target at a content_block_stop, so a
// later unindexed delta can't append to a call that already ended.
func (s *inferenceStreamState) closeAnthropicTool() {
	s.openTool = nil
}

// foldAnthropicFrame folds one Messages SSE event into the running stream state.
// The prompt size is taken as the largest total seen, because different Messages
// API paths report it on different events: message_start on the plain path,
// message_delta on the ?beta=true path. The completion accumulates from
// text_delta blocks; tool calls accumulate from content_block_start plus
// input_json_delta; stop_reason and the cumulative output_tokens arrive in
// message_delta. Unknown events (ping, message_stop) are ignored.
func foldAnthropicFrame(frame []byte, state *inferenceStreamState, ext *model.InferenceExtension) {
	var ev anthropicStreamEvent
	if err := json.Unmarshal(frame, &ev); err != nil {
		// A malformed frame drops whatever usage it carried, so a stream can
		// finish with accumulated completion text and no token counts at all —
		// logInferenceFinalized then prints every split counter as -1 with
		// nothing to explain it. Previously a bare return, which made that
		// outcome undiagnosable. Debug, not Warn: a truncated final frame on a
		// cancelled turn is routine. Log the length, never the bytes — frames
		// carry prompt and completion content.
		slog.Debug("inference-parser: malformed Anthropic streaming frame, skipping",
			"error", err, "frameLen", len(frame))
		return
	}
	if !knownAnthropicEvents[ev.Type] {
		// The frame parses, so nothing errors, but the switch below ignores it
		// and any usage it carried is lost. That means the wire format has moved
		// ahead of this parser — the one failure mode that silently zeroes token
		// telemetry while completion text still accumulates. ev.Type is a wire
		// value, so it goes only to the log, which already carries bodies at
		// Debug level.
		slog.Debug("inference-parser: unrecognized Anthropic stream event type — token counts may be incomplete",
			"type", model.SanitizeLogValue(ev.Type))
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			noteBodyModel(ext, ev.Message.Model)
			mergeAnthropicUsageMaxSeen(state, ev.Message.Usage.toNeutral())
			state.hasUsage = true
		}
	case "content_block_start":
		// A tool call opens here and is populated by later deltas. Text
		// blocks need no setup — their deltas append to the completion.
		if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
			state.openAnthropicTool(ev.Index, ev.ContentBlock.ID, ev.ContentBlock.Name)
		}
		// A thinking block's text and signature follow as deltas, which add only bytes.
		if ev.ContentBlock != nil {
			state.thinking.Add(ev.ContentBlock.Type, 0, 0, rawStringLen(ev.ContentBlock.Data))
		}
	case "content_block_delta":
		if ev.Delta == nil {
			return
		}
		switch ev.Delta.Type {
		case "text_delta":
			state.completion.WriteString(ev.Delta.Text)
		case "input_json_delta":
			if tc := state.anthropicTool(ev.Index); tc != nil {
				tc.args.WriteString(ev.Delta.PartialJSON)
			}
		case "thinking_delta":
			state.thinking.TextBytes += rawStringLen(ev.Delta.Thinking)
		case "signature_delta":
			state.thinking.SignatureBytes += rawStringLen(ev.Delta.Signature)
		}
	case "content_block_stop":
		state.closeAnthropicTool()
	case "error":
		// An upstream abort. Not a parse failure, so it must not be reported as
		// an unrecognized type, and not silently dropped either: it explains an
		// otherwise inexplicable stream that carried no usage. Warn, unlike the
		// Debug drops above — the upstream failed the request. The error type
		// and message are provider-generated diagnostics, not user content.
		if ev.Error != nil {
			slog.Warn("inference-parser: Anthropic stream returned an error event — token counts will be incomplete",
				"errorType", model.SanitizeLogValue(ev.Error.Type), "message", model.SanitizeLogValue(ev.Error.Message))
		} else {
			slog.Warn("inference-parser: Anthropic stream returned an error event with no detail — token counts will be incomplete")
		}
	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			ext.FinishReason = ev.Delta.StopReason
		}
		if ev.Usage != nil {
			// ?beta=true path defers cache counts from message_start to
			// message_delta; non-beta path carries no input counts here.
			// Max-seen per sub-field handles both without clobbering.
			neutral := ev.Usage.toNeutral()
			mergeAnthropicUsageMaxSeen(state, neutral)
			if neutral.Output > 0 {
				state.usage.Output = neutral.Output // cumulative
			}
			state.hasUsage = true
		}
	}
}

// mergeAnthropicUsageMaxSeen updates every sub-field this function owns with
// max-seen semantics so a later event carrying zero cannot clobber an earlier
// real count. See foldAnthropicFrame for why both events need this.
//
// EVERY SUB-FIELD IT OWNS: Present is unioned here for ALL kinds, so any kind whose bit
// this sets must have its value merged here too. Split across two places, a bit arrives
// set with a value of nothing — and `agentop cost` then prints "reasoning (of output) 0",
// the claim ThinkingTokensAbsent forbids.
//
// Output is deliberately NOT here: it is cumulative on the wire rather than max-seen,
// and foldAnthropicFrame assigns it directly.
func mergeAnthropicUsageMaxSeen(state *inferenceStreamState, incoming parsercommon.TokenUsage) {
	// Presence is a union across events: once a sub-field is observed on
	// the wire, later events that omit it must not clear the bit. Kept beside the
	// value merges below so that every kind merged HERE has its bit and its value set
	// in one place.
	//
	// KindOutput is the one exception, and naming it is the point: toNeutral asserts
	// that bit unconditionally, so this union sets it while Output is filled in
	// foldAnthropicFrame instead — it is cumulative on the wire, not max-seen. That
	// split predates this function and pricing.outputUncounted depends on it. The
	// exception is written down because the reasoning bug this signature was changed
	// to fix was a bit set here and a value filled elsewhere; an unqualified claim
	// that it cannot happen would hide the one place it still does.
	state.usage.Present |= incoming.Present
	if incoming.Reasoning > state.usage.Reasoning {
		state.usage.Reasoning = incoming.Reasoning
	}
	if incoming.Input > state.usage.Input {
		state.usage.Input = incoming.Input
	}
	if incoming.CacheRead > state.usage.CacheRead {
		state.usage.CacheRead = incoming.CacheRead
	}
	if incoming.CacheWrite > state.usage.CacheWrite {
		state.usage.CacheWrite = incoming.CacheWrite
	}
}

// parseAnthropicSSE folds a fully-buffered Messages SSE body. Mirrors
// parseInferenceSSE for the legacy OnResponse path; the live listener uses
// foldAnthropicFrame via OnResponseFrame instead.
func parseAnthropicSSE(body []byte, ext *model.InferenceExtension) {
	markStreamedResponse(body, ext)
	state := &inferenceStreamState{}
	// parsercommon.NormalizeSSE for the reason given where it is defined: a BOM or CR-only
	// line endings are wire-legal, and splitting on LF alone reads such a body as one
	// unparseable line.
	for _, line := range bytes.Split(parsercommon.NormalizeSSE(body), []byte("\n")) {
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
	state.finalize(ext)
}
