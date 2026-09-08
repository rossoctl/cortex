package inferenceparser

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins/internal/parsercommon"
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
//
// ContentBytes records the size of what was there before that reduction, so
// the blocks flattenContent drops are still accounted for. In an agent loop
// the dropped blocks are the bulk of the conversation: every tool result comes
// back as a tool_result block, so a turn that reads a large file shows up as an
// empty Content and would otherwise look free.
type anthropicReqMessage struct {
	Role         string
	Content      string
	ContentBytes int
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
	m.ContentBytes = contentBytes(raw.Content)
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
		ext.Messages = append(ext.Messages, pipeline.InferenceMessage{
			Role: "system", Content: sys, ContentBytes: contentBytes(req.System),
		})
	}
	for _, msg := range req.Messages {
		ext.Messages = append(ext.Messages, pipeline.InferenceMessage{
			Role: msg.Role, Content: msg.Content, ContentBytes: msg.ContentBytes,
		})
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
//
// Cache fields are *int so an omitted field on the wire (e.g. message_start
// on the ?beta=true path) stays absent in Present rather than being asserted
// as a reported zero.
type anthropicUsage struct {
	InputTokens              int  `json:"input_tokens"`
	OutputTokens             int  `json:"output_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
}

// toNeutral maps Anthropic's usage onto TokenUsage. Input and Output are
// always emitted by the Messages API; cache sub-fields are observed via
// their pointers so an absent field stays absent in Present. Reasoning is
// not exposed by Anthropic.
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
	return n
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
	} `json:"content_block"`
}

// anthropicToolCallState accumulates one streamed tool call. The id and name
// arrive on content_block_start; the arguments follow as a series of
// input_json_delta fragments that are only valid JSON once concatenated.
type anthropicToolCallState struct {
	id   string
	name string
	args strings.Builder
}

// openAnthropicTool starts accumulating a tool call for content block index.
// The entry is pointer-held: a strings.Builder must not be copied once used,
// which a value slice would do the moment append reallocates.
func (s *inferenceStreamState) openAnthropicTool(index *int, id, name string) {
	tc := &anthropicToolCallState{id: id, name: name}
	s.toolCalls = append(s.toolCalls, tc)
	if index != nil {
		if s.toolsByIndex == nil {
			s.toolsByIndex = map[int]*anthropicToolCallState{}
		}
		s.toolsByIndex[*index] = tc
	}
	s.openTool = tc
}

// anthropicTool resolves the tool call a delta belongs to. Nil index falls
// back to the most recently opened call — blocks are emitted sequentially,
// so that is the same call the index would have named.
func (s *inferenceStreamState) anthropicTool(index *int) *anthropicToolCallState {
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
func foldAnthropicFrame(frame []byte, state *inferenceStreamState, ext *pipeline.InferenceExtension) {
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
			"type", ev.Type)
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			mergeAnthropicPromptMaxSeen(state, ev.Message.Usage.toNeutral())
			state.hasUsage = true
		}
	case "content_block_start":
		// A tool call opens here and is populated by later deltas. Text
		// blocks need no setup — their deltas append to the completion.
		if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
			state.openAnthropicTool(ev.Index, ev.ContentBlock.ID, ev.ContentBlock.Name)
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
				"errorType", ev.Error.Type, "message", ev.Error.Message)
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
			mergeAnthropicPromptMaxSeen(state, neutral)
			if neutral.Output > 0 {
				state.usage.Output = neutral.Output // cumulative
			}
			state.hasUsage = true
		}
	}
}

// mergeAnthropicPromptMaxSeen updates prompt-side sub-fields with
// max-seen semantics so a later event carrying zero cannot clobber an
// earlier real count. See foldAnthropicFrame for why both events need
// this.
func mergeAnthropicPromptMaxSeen(state *inferenceStreamState, incoming parsercommon.TokenUsage) {
	// Presence is a union across events: once a sub-field is observed on
	// the wire, later events that omit it must not clear the bit.
	state.usage.Present |= incoming.Present
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
	state.finalize(ext)
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
