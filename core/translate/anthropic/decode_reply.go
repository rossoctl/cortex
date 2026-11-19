package anthropic

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// maxErrorBody is the most of a non-2xx body that the decoder keeps.
const maxErrorBody = 1 << 20

// maxReplyBuffer is the most of a 2xx reply that the decoder keeps: a whole
// not-streamed body, or one stream line (decision 10).
const maxReplyBuffer = 10 << 20

// maxErrorMessage is the most of a body that is not Anthropic's error
// shape that goes into the error message (rule S15).
const maxErrorMessage = 200

const msgEndedEarly = "the inference server's stream ended before the reply was complete"

// errorStatus gives the status of a stream error event from its type
// (decision 9). Any other type is 500.
var errorStatus = map[string]int{
	"invalid_request_error": 400, "authentication_error": 401, "permission_error": 403,
	"not_found_error": 404, "request_too_large": 413, "rate_limit_error": 429,
	"overloaded_error": 529,
}

// The Anthropic reply shapes. Only the fields that rules S13-S15 read.
type (
	wireUsage struct {
		Input         *int `json:"input_tokens"`
		Output        *int `json:"output_tokens"`
		CacheRead     *int `json:"cache_read_input_tokens"`
		CacheCreation *int `json:"cache_creation_input_tokens"`
	}
	wireBlock struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		Signature string          `json:"signature"`
		Data      string          `json:"data"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
	}
	// wireMessage is a not-streamed body or the message of message_start.
	wireMessage struct {
		Type       string       `json:"type"`
		ID         string       `json:"id"`
		Model      string       `json:"model"`
		Content    []wireBlock  `json:"content"`
		StopReason string       `json:"stop_reason"`
		Usage      *wireUsage   `json:"usage"`
		Error      *errorDetail `json:"error"`
	}
	wireEvent struct {
		Type         string      `json:"type"`
		Message      wireMessage `json:"message"`
		Index        int         `json:"index"`
		ContentBlock wireBlock   `json:"content_block"`
		Delta        struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			Signature   string `json:"signature"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage *wireUsage   `json:"usage"`
		Error *errorDetail `json:"error"`
	}
)

// replyDecoder converts one anthropic-messages reply to pivot events.
type replyDecoder struct {
	status int
	stream bool
	buf    []byte // a stream: the partial line; else the body

	out        []pivot.Event
	done       bool // stop or error emitted; ignore the rest
	stopReason string
	usage      pivot.Usage
	index      map[int]int // Anthropic block index to pivot index; a skipped block has none
	next       int         // the next pivot block index (decision 6)
}

// NewReplyDecoder makes a decoder for one upstream reply (rules S13-S15).
func (Protocol) NewReplyDecoder(status int, stream bool) pivot.ReplyDecoder {
	return &replyDecoder{status: status, stream: stream, index: map[int]int{}}
}

func (d *replyDecoder) ok() bool { return d.status >= 200 && d.status < 300 }

// Feed reads the next bytes of the body.
func (d *replyDecoder) Feed(p []byte) []pivot.Event {
	if d.done {
		return nil
	}
	if !d.ok() {
		if room := maxErrorBody - len(d.buf); room > 0 {
			d.buf = append(d.buf, p[:min(len(p), room)]...)
		}
		return nil
	}
	d.buf = append(d.buf, p...)
	d.out = nil
	if !d.stream {
		if len(d.buf) > maxReplyBuffer {
			d.overLimit()
		}
		return d.out
	}
	rest := d.buf
	for !d.done {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			break
		}
		d.line(rest[:i])
		rest = rest[i+1:]
	}
	if !d.done && len(rest) > maxReplyBuffer {
		d.overLimit()
		return d.out
	}
	// Keep only the partial line, in a new array, so that the stream
	// read so far is not held (#1911 rule 10).
	d.buf = append([]byte(nil), rest...)
	return d.out
}

// End reads the end of the body.
func (d *replyDecoder) End() []pivot.Event {
	if d.done {
		return nil
	}
	d.out = nil
	switch {
	case !d.ok():
		typ, msg := httpError(d.status, d.buf)
		d.fail(d.status, typ, msg)
	case !d.stream:
		d.whole(d.buf)
	default:
		if len(d.buf) > 0 {
			d.line(d.buf)
		}
		if !d.done {
			// Decision 8: a broken stream must not look complete.
			d.fail(http.StatusBadGateway, "api_error", msgEndedEarly)
		}
	}
	d.buf = nil
	return d.out
}

func (d *replyDecoder) emit(ev pivot.Event) { d.out = append(d.out, ev) }

func (d *replyDecoder) fail(status int, errType, msg string) {
	d.emit(pivot.Event{Kind: pivot.EventError, Status: status, ErrorType: errType, Message: msg})
	d.done = true
}

func (d *replyDecoder) failEvent(e *errorDetail) {
	status, ok := errorStatus[e.Type]
	if !ok {
		status = http.StatusInternalServerError
	}
	d.fail(status, e.Type, e.Message)
}

// overLimit ends a reply that is longer than the decoder keeps.
func (d *replyDecoder) overLimit() {
	d.fail(http.StatusBadGateway, "api_error", "the inference server's reply is over the "+strconv.Itoa(maxReplyBuffer)+"-byte buffer limit")
	d.buf = nil
}

// addUsage copies each member that is present (rule S14, decision 13).
func (d *replyDecoder) addUsage(u *wireUsage) {
	if u == nil {
		return
	}
	for _, f := range []struct {
		src *int
		dst *int
	}{{u.Input, &d.usage.Input}, {u.Output, &d.usage.Output}, {u.CacheRead, &d.usage.CacheRead}, {u.CacheCreation, &d.usage.CacheCreation}} {
		if f.src != nil {
			*f.dst = *f.src
		}
	}
}

// finish emits usage, then stop (decision 5).
func (d *replyDecoder) finish() {
	reason := d.stopReason
	if reason == "" {
		reason = "end_turn"
	}
	d.emit(pivot.Event{Kind: pivot.EventUsage, Usage: d.usage})
	d.emit(pivot.Event{Kind: pivot.EventStop, StopReason: reason})
	d.done = true
}

func (d *replyDecoder) delta(index int, k pivot.DeltaKind, v string) {
	if v != "" {
		d.emit(pivot.Event{Kind: pivot.EventDelta, Index: index, Delta: k, Value: v})
	}
}

// openBlock emits the start of block ai, and the text that its start
// carries (decision 12). It gives false for a skipped block (rule S13).
func (d *replyDecoder) openBlock(ai int, b wireBlock) (int, bool) {
	ev := pivot.Event{Kind: pivot.EventBlockStart, Index: d.next}
	switch b.Type {
	case "text":
		ev.Block = pivot.BlockText
	case "thinking":
		ev.Block = pivot.BlockThinking
	case "redacted_thinking":
		ev.Block, ev.Data = pivot.BlockRedactedThinking, b.Data
	case "tool_use":
		ev.Block, ev.ToolID, ev.ToolName = pivot.BlockToolUse, b.ID, b.Name
	default:
		return 0, false // a server-tool block (decision 7)
	}
	d.index[ai] = d.next
	d.next++
	d.emit(ev)
	d.delta(ev.Index, pivot.DeltaText, b.Text)
	d.delta(ev.Index, pivot.DeltaThinking, b.Thinking)
	d.delta(ev.Index, pivot.DeltaSignature, b.Signature)
	return ev.Index, true
}

// line reads one SSE line of a stream (decision 11).
func (d *replyDecoder) line(l []byte) {
	l = bytes.TrimSuffix(l, []byte("\r"))
	payload, ok := bytes.CutPrefix(l, []byte("data:"))
	if !ok {
		return // a blank line, a comment or an event: line
	}
	var e wireEvent
	if err := json.Unmarshal(bytes.TrimSpace(payload), &e); err != nil {
		d.fail(http.StatusBadGateway, "api_error", "the inference server sent a stream event that is not valid JSON")
		return
	}
	switch e.Type {
	case "message_start":
		d.emit(pivot.Event{Kind: pivot.EventStart, ID: e.Message.ID, Model: e.Message.Model})
		d.addUsage(e.Message.Usage)
	case "content_block_start":
		d.openBlock(e.Index, e.ContentBlock)
	case "content_block_delta":
		i, ok := d.index[e.Index]
		if !ok {
			return
		}
		switch e.Delta.Type {
		case "text_delta":
			d.delta(i, pivot.DeltaText, e.Delta.Text)
		case "thinking_delta":
			d.delta(i, pivot.DeltaThinking, e.Delta.Thinking)
		case "signature_delta":
			d.delta(i, pivot.DeltaSignature, e.Delta.Signature)
		case "input_json_delta":
			d.delta(i, pivot.DeltaInputJSON, e.Delta.PartialJSON)
		}
	case "content_block_stop":
		if i, ok := d.index[e.Index]; ok {
			d.emit(pivot.Event{Kind: pivot.EventBlockStop, Index: i})
			delete(d.index, e.Index)
		}
	case "message_delta":
		if e.Delta.StopReason != "" {
			d.stopReason = e.Delta.StopReason
		}
		d.addUsage(e.Usage)
	case "message_stop":
		d.finish()
	case "error":
		if e.Error == nil {
			e.Error = &errorDetail{Type: "api_error", Message: "the inference server sent an error event with no error"}
		}
		d.failEvent(e.Error)
	}
	// ping and unknown event types are skipped.
}

// whole converts a not-streamed 2xx body (rule S15).
func (d *replyDecoder) whole(body []byte) {
	var m wireMessage
	if err := json.Unmarshal(body, &m); err != nil {
		d.fail(http.StatusBadGateway, "api_error", "the inference server sent a reply that is not valid JSON")
		return
	}
	if m.Type == "error" && m.Error != nil {
		d.failEvent(m.Error)
		return
	}
	d.emit(pivot.Event{Kind: pivot.EventStart, ID: m.ID, Model: m.Model})
	for ai, b := range m.Content {
		i, ok := d.openBlock(ai, b)
		if !ok {
			continue
		}
		if b.Type == "tool_use" {
			d.delta(i, pivot.DeltaInputJSON, string(b.Input))
		}
		d.emit(pivot.Event{Kind: pivot.EventBlockStop, Index: i})
	}
	d.addUsage(m.Usage)
	d.stopReason = m.StopReason
	d.finish()
}

// httpError gives the type and the message of a non-2xx reply (rule S15).
func httpError(status int, body []byte) (string, string) {
	var e wireMessage
	if json.Unmarshal(body, &e) == nil && e.Error != nil && e.Error.Type != "" {
		if e.Error.Message == "" {
			return e.Error.Type, http.StatusText(status)
		}
		return e.Error.Type, e.Error.Message
	}
	t := strings.TrimSpace(string(body))
	if len(t) > maxErrorMessage {
		n := maxErrorMessage
		for n > 0 && !utf8.RuneStart(t[n]) {
			n--
		}
		t = t[:n]
	}
	if t == "" {
		t = http.StatusText(status)
	}
	return "api_error", t
}
