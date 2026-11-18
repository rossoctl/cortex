package anthropic

import (
	"encoding/json"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// The output types have a fixed field order, so the bytes are the same
// each time (rule 9a).

type startUsage struct {
	Input  int `json:"input_tokens"`
	Output int `json:"output_tokens"`
}

type fullUsage struct {
	Input         int `json:"input_tokens"`
	CacheCreation int `json:"cache_creation_input_tokens"`
	CacheRead     int `json:"cache_read_input_tokens"`
	Output        int `json:"output_tokens"`
}

func usageOf(u pivot.Usage) fullUsage {
	return fullUsage{Input: u.Input, CacheCreation: u.CacheCreation, CacheRead: u.CacheRead, Output: u.Output}
}

type message struct {
	ID           string  `json:"id"`
	Type         string  `json:"type"`
	Role         string  `json:"role"`
	Model        string  `json:"model"`
	Content      []any   `json:"content"`
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
	Usage        any     `json:"usage"`
}

type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type thinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type toolUseBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type kindBlock struct {
	Type string `json:"type"`
}

// block gives the Anthropic content block of one kind.
func block(kind pivot.BlockKind, text, signature, id, name string, input json.RawMessage) any {
	switch kind {
	case pivot.BlockText:
		return textBlock{Type: "text", Text: text}
	case pivot.BlockThinking:
		return thinkingBlock{Type: "thinking", Thinking: text, Signature: signature}
	case pivot.BlockToolUse:
		return toolUseBlock{Type: "tool_use", ID: id, Name: name, Input: input}
	}
	return kindBlock{Type: string(kind)}
}

type messageStart struct {
	Type    string  `json:"type"`
	Message message `json:"message"`
}

type blockStart struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock any    `json:"content_block"`
}

type blockDelta struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta any    `json:"delta"`
}

type blockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type textDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type thinkingDelta struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
}

type signatureDelta struct {
	Type      string `json:"type"`
	Signature string `json:"signature"`
}

type inputDelta struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}

type stopDelta struct {
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type messageDelta struct {
	Type  string    `json:"type"`
	Delta stopDelta `json:"delta"`
	Usage fullUsage `json:"usage"`
}

func delta(kind pivot.DeltaKind, v string) any {
	switch kind {
	case pivot.DeltaThinking:
		return thinkingDelta{Type: "thinking_delta", Thinking: v}
	case pivot.DeltaSignature:
		return signatureDelta{Type: "signature_delta", Signature: v}
	case pivot.DeltaInputJSON:
		return inputDelta{Type: "input_json_delta", PartialJSON: v}
	}
	return textDelta{Type: "text_delta", Text: v}
}

func frame(event string, v any) []byte {
	b := append([]byte("event: "+event+"\ndata: "), marshal(v)...)
	return append(b, "\n\n"...)
}

// collected is one content block of a not-streamed reply.
type collected struct {
	index                  int
	kind                   pivot.BlockKind
	text, signature, input string
	id, name               string
}

type replyEncoder struct {
	stream    bool
	done      bool // a stream has sent its error or its message_stop
	id, model string
	usage     pivot.Usage
	stop      string
	blocks    []*collected
	err       *pivot.Event
}

// NewReplyEncoder makes an encoder for one reply to the agent. With
// stream, each event gives its SSE frames at once (rule 23). Without
// stream, the events are collected and End gives one message.
func (Protocol) NewReplyEncoder(stream bool) pivot.ReplyEncoder {
	return &replyEncoder{stream: stream}
}

func (e *replyEncoder) Encode(ev pivot.Event) []byte {
	if e.stream {
		return e.encodeStream(ev)
	}
	e.collect(ev)
	return nil
}

func (e *replyEncoder) encodeStream(ev pivot.Event) []byte {
	if e.done {
		return nil
	}
	switch ev.Kind {
	case pivot.EventStart:
		return frame("message_start", messageStart{Type: "message_start", Message: message{
			ID: ev.ID, Type: "message", Role: "assistant", Model: ev.Model,
			Content: []any{}, Usage: startUsage{},
		}})
	case pivot.EventBlockStart:
		return frame("content_block_start", blockStart{Type: "content_block_start", Index: ev.Index,
			ContentBlock: block(ev.Block, "", "", ev.ToolID, ev.ToolName, json.RawMessage("{}"))})
	case pivot.EventDelta:
		return frame("content_block_delta", blockDelta{Type: "content_block_delta", Index: ev.Index, Delta: delta(ev.Delta, ev.Value)})
	case pivot.EventBlockStop:
		return frame("content_block_stop", blockStop{Type: "content_block_stop", Index: ev.Index})
	case pivot.EventUsage:
		e.usage = ev.Usage
	case pivot.EventStop:
		e.done = true
		out := frame("message_delta", messageDelta{Type: "message_delta",
			Delta: stopDelta{StopReason: ev.StopReason}, Usage: usageOf(e.usage)})
		return append(out, frame("message_stop", kindBlock{Type: "message_stop"})...)
	case pivot.EventError:
		e.done = true
		return frame("error", errorBody{Type: "error", Error: errorDetail{Type: ev.ErrorType, Message: ev.Message}})
	}
	return nil
}

func (e *replyEncoder) collect(ev pivot.Event) {
	if e.err != nil {
		return
	}
	switch ev.Kind {
	case pivot.EventStart:
		e.id, e.model = ev.ID, ev.Model
	case pivot.EventBlockStart:
		e.blocks = append(e.blocks, &collected{index: ev.Index, kind: ev.Block, id: ev.ToolID, name: ev.ToolName})
	case pivot.EventDelta:
		b := e.block(ev.Index)
		if b == nil {
			return
		}
		switch ev.Delta {
		case pivot.DeltaText, pivot.DeltaThinking:
			b.text += ev.Value
		case pivot.DeltaSignature:
			b.signature += ev.Value
		case pivot.DeltaInputJSON:
			b.input += ev.Value
		}
	case pivot.EventUsage:
		e.usage = ev.Usage
	case pivot.EventStop:
		e.stop = ev.StopReason
	case pivot.EventError:
		e.err = &ev
	}
}

func (e *replyEncoder) block(index int) *collected {
	for i := len(e.blocks) - 1; i >= 0; i-- {
		if e.blocks[i].index == index {
			return e.blocks[i]
		}
	}
	return nil
}

// End gives nothing for a stream. For a not-streamed reply it gives the
// one message, or only the error body when an error event came.
func (e *replyEncoder) End() []byte {
	if e.stream {
		return nil
	}
	if e.err != nil {
		return Protocol{}.EncodeError(e.err.Status, e.err.ErrorType, e.err.Message)
	}
	content := make([]any, 0, len(e.blocks))
	for _, b := range e.blocks {
		input := json.RawMessage(b.input)
		if b.kind == pivot.BlockToolUse {
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			if !json.Valid(input) {
				return Protocol{}.EncodeError(502, "api_error", "the inference server sent tool "+b.name+" input that is not valid JSON")
			}
		}
		content = append(content, block(b.kind, b.text, b.signature, b.id, b.name, input))
	}
	msg := message{ID: e.id, Type: "message", Role: "assistant", Model: e.model, Content: content, Usage: usageOf(e.usage)}
	if e.stop != "" {
		msg.StopReason = &e.stop
	}
	return marshal(msg)
}
