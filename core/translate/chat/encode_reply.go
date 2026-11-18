package chat

import (
	"bytes"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

var _ pivot.ReplyEncoding = Protocol{}

// now gives the time of the created field. Tests set a fixed time.
var now = time.Now

// The output types have a fixed field order, so the bytes are the same
// each time (rule 9a of #1911).

type outUsageDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type outUsage struct {
	PromptTokens             int             `json:"prompt_tokens"`
	CompletionTokens         int             `json:"completion_tokens"`
	TotalTokens              int             `json:"total_tokens"`
	PromptTokensDetails      outUsageDetails `json:"prompt_tokens_details"`
	CacheReadInputTokens     int             `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int             `json:"cache_creation_input_tokens"`
}

// usageOf converts pivot usage, which has the Anthropic meaning, to the
// Chat meaning: prompt_tokens includes both cache counts (rule K6).
func usageOf(u pivot.Usage) *outUsage {
	in := u.Input + u.CacheRead + u.CacheCreation
	return &outUsage{
		PromptTokens:             in,
		CompletionTokens:         u.Output,
		TotalTokens:              in + u.Output,
		PromptTokensDetails:      outUsageDetails{CachedTokens: u.CacheRead},
		CacheReadInputTokens:     u.CacheRead,
		CacheCreationInputTokens: u.CacheCreation,
	}
}

type outSignedThinking struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type outRedactedThinking struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

type outFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type outToolCall struct {
	Index    *int        `json:"index,omitempty"` // stream only
	ID       string      `json:"id,omitempty"`
	Type     string      `json:"type,omitempty"`
	Function outFunction `json:"function"`
}

type outDelta struct {
	Role             string        `json:"role,omitempty"`
	Content          *string       `json:"content,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ThinkingBlocks   []any         `json:"thinking_blocks,omitempty"`
	ToolCalls        []outToolCall `json:"tool_calls,omitempty"`
}

type outMessage struct {
	Role             string        `json:"role"`
	Content          *string       `json:"content"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ThinkingBlocks   []any         `json:"thinking_blocks,omitempty"`
	ToolCalls        []outToolCall `json:"tool_calls,omitempty"`
}

type outChoice struct {
	Index        int         `json:"index"`
	Delta        *outDelta   `json:"delta,omitempty"`
	Message      *outMessage `json:"message,omitempty"`
	FinishReason *string     `json:"finish_reason"`
}

type outChunk struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []outChoice `json:"choices"`
	Usage   *outUsage   `json:"usage,omitempty"`
}

// encBlock is the one block that is open.
type encBlock struct {
	index           int
	kind            pivot.BlockKind
	call            int // tool_use: the tool-call index of this reply
	hasInput        bool
	text, signature strings.Builder // thinking
	input           strings.Builder // tool_use, not streamed
}

type replyEncoder struct {
	stream, includeUsage bool
	done                 bool // the stop or the error came
	id, model            string
	created              int64
	usage                pivot.Usage
	calls                int // the tool calls of this reply
	open                 *encBlock
	out                  []byte // the frames of the current call
	// Not streamed: the reply parts until End.
	text, reasoning strings.Builder
	hasText         bool
	blocks          []any
	toolCalls       []outToolCall
	stop            string
	failure         []byte
}

// NewReplyEncoder makes an encoder for one reply to a Chat agent. With
// stream, each event gives its chunks at once (rule K5). Without stream,
// End gives one chat.completion object (rule K8). The extra
// chat-completions.include_usage true turns on the separate usage chunk
// (rule K6).
func (Protocol) NewReplyEncoder(req pivot.Request, stream bool) pivot.ReplyEncoder {
	return &replyEncoder{
		stream:       stream,
		model:        req.Model,
		includeUsage: bytes.Equal(bytes.TrimSpace(req.Extra["chat-completions.include_usage"]), []byte("true")),
	}
}

// emit adds one chunk for a stream.
func (e *replyEncoder) emit(c outChunk) {
	c.ID, c.Object, c.Created, c.Model = e.id, "chat.completion.chunk", e.created, e.model
	// The values hold no json.RawMessage, so marshal cannot fail.
	j, _ := marshal(c)
	e.out = append(e.out, "data: "...)
	e.out = append(e.out, j...)
	e.out = append(e.out, "\n\n"...)
}

func (e *replyEncoder) emitDelta(d outDelta) {
	if e.stream {
		e.emit(outChunk{Choices: []outChoice{{Delta: &d}}})
	}
}

func (e *replyEncoder) Encode(ev pivot.Event) []byte {
	if e.done {
		return nil
	}
	e.out = nil
	switch ev.Kind {
	case pivot.EventStart:
		e.id, e.created = ev.ID, now().Unix()
		if ev.Model != "" {
			e.model = ev.Model
		}
		empty := ""
		e.emitDelta(outDelta{Role: "assistant", Content: &empty})
	case pivot.EventBlockStart:
		e.openBlock(ev)
	case pivot.EventDelta:
		e.delta(ev)
	case pivot.EventBlockStop:
		e.closeBlock(ev.Index)
	case pivot.EventUsage:
		e.usage = ev.Usage
	case pivot.EventStop:
		e.finish(ev.StopReason)
	case pivot.EventError:
		e.fail(ev.Status, ev.ErrorType, ev.Message)
	}
	return e.out
}

func (e *replyEncoder) openBlock(ev pivot.Event) {
	b := &encBlock{index: ev.Index, kind: ev.Block}
	switch ev.Block {
	case pivot.BlockText, pivot.BlockThinking:
	case pivot.BlockRedactedThinking:
		// The start event carries the data, so the chunk goes now (decision 4).
		r := outRedactedThinking{Type: "redacted_thinking", Data: ev.Data}
		e.keep(r)
		e.emitDelta(outDelta{ThinkingBlocks: []any{r}})
	case pivot.BlockToolUse:
		b.call = e.calls
		e.calls++
		if !e.stream {
			e.toolCalls = append(e.toolCalls, outToolCall{ID: ev.ToolID, Type: "function", Function: outFunction{Name: ev.ToolName}})
		}
		e.emitDelta(outDelta{ToolCalls: []outToolCall{{Index: &b.call, ID: ev.ToolID, Type: "function",
			Function: outFunction{Name: ev.ToolName}}}})
	default:
		return // a block kind that a Chat reply cannot carry
	}
	e.open = b
}

func (e *replyEncoder) delta(ev pivot.Event) {
	b := e.open
	if b == nil || b.index != ev.Index || ev.Value == "" {
		return
	}
	switch {
	case ev.Delta == pivot.DeltaText && b.kind == pivot.BlockText:
		if !e.stream {
			e.text.WriteString(ev.Value)
			e.hasText = true
		}
		e.emitDelta(outDelta{Content: &ev.Value})
	case ev.Delta == pivot.DeltaThinking && b.kind == pivot.BlockThinking:
		b.text.WriteString(ev.Value)
		if !e.stream {
			e.reasoning.WriteString(ev.Value)
		}
		e.emitDelta(outDelta{ReasoningContent: ev.Value})
	case ev.Delta == pivot.DeltaSignature && b.kind == pivot.BlockThinking:
		b.signature.WriteString(ev.Value)
	case ev.Delta == pivot.DeltaInputJSON && b.kind == pivot.BlockToolUse:
		b.hasInput = true
		e.toolPiece(b, ev.Value)
	}
}

// keep adds a thinking_blocks entry to a not-streamed reply.
func (e *replyEncoder) keep(t any) {
	if !e.stream {
		e.blocks = append(e.blocks, t)
	}
}

// toolPiece adds one piece of tool-call arguments.
func (e *replyEncoder) toolPiece(b *encBlock, piece string) {
	if !e.stream {
		b.input.WriteString(piece)
		return
	}
	e.emitDelta(outDelta{ToolCalls: []outToolCall{{Index: &b.call, Function: outFunction{Arguments: piece}}}})
}

func (e *replyEncoder) closeBlock(index int) {
	b := e.open
	if b == nil || b.index != index {
		return
	}
	e.open = nil
	switch b.kind {
	case pivot.BlockThinking:
		if sig := b.signature.String(); sig != "" {
			t := outSignedThinking{Type: "thinking", Thinking: b.text.String(), Signature: sig}
			e.keep(t)
			e.emitDelta(outDelta{ThinkingBlocks: []any{t}})
		}
	case pivot.BlockToolUse:
		if !b.hasInput {
			e.toolPiece(b, "{}") // Chat agents parse arguments as JSON (decision 5)
		}
		if !e.stream {
			e.toolCalls[b.call].Function.Arguments = b.input.String()
		}
	}
}

// finishReason maps an Anthropic stop reason to a Chat finish_reason
// (rule K5, decision 6).
func finishReason(r string) string {
	switch r {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	}
	return "stop"
}

func (e *replyEncoder) finish(reason string) {
	e.done = true
	e.stop = finishReason(reason)
	if !e.stream {
		return
	}
	c := outChunk{Choices: []outChoice{{Delta: &outDelta{}, FinishReason: &e.stop}}}
	if !e.includeUsage {
		c.Usage = usageOf(e.usage) // so that the cost ledger gets the tokens (rule K6)
	}
	e.emit(c)
	if e.includeUsage {
		e.emit(outChunk{Choices: []outChoice{}, Usage: usageOf(e.usage)})
	}
	e.out = append(e.out, "data: [DONE]\n\n"...)
}

// fail ends the reply with an error: in a stream, one error chunk and no
// [DONE] (rule K7).
func (e *replyEncoder) fail(status int, errType, message string) {
	e.done = true
	e.failure = Protocol{}.EncodeError(status, errType, message)
	if e.stream {
		e.out = append(e.out, "data: "...)
		e.out = append(e.out, e.failure...)
		e.out = append(e.out, "\n\n"...)
	}
}

// End gives the rest of a stream, or the one body of a not-streamed
// reply. A reply with no stop event and no error event fails (decision 8).
func (e *replyEncoder) End() []byte {
	e.out = nil
	if !e.done {
		e.fail(502, "api_error", msgEndedEarly)
	}
	if e.stream {
		return e.out
	}
	if e.failure != nil {
		return e.failure
	}
	m := &outMessage{Role: "assistant", ReasoningContent: e.reasoning.String(), ThinkingBlocks: e.blocks, ToolCalls: e.toolCalls}
	if e.hasText {
		s := e.text.String()
		m.Content = &s
	}
	b, _ := marshal(outChunk{ID: e.id, Object: "chat.completion", Created: e.created, Model: e.model,
		Choices: []outChoice{{Message: m, FinishReason: &e.stop}}, Usage: usageOf(e.usage)})
	return b
}
