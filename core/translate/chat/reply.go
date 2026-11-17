package chat

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

// maxErrorMessage is the most of a non-JSON error body that goes into the
// error message (Review Focus 4).
const maxErrorMessage = 512

const msgEndedEarly = "the inference server's stream ended before the reply was complete"

// The Anthropic error types. An upstream type in this set is kept.
var anthropicErrorTypes = map[string]bool{
	"invalid_request_error": true, "authentication_error": true, "permission_error": true,
	"not_found_error": true, "request_too_large": true, "rate_limit_error": true,
	"api_error": true, "overloaded_error": true,
}

type openKind int

const (
	kindNone openKind = iota
	kindThinking
	kindText
	kindTool
)

// decoder converts one chat-completions reply to pivot events.
type decoder struct {
	status int
	stream bool
	buf    []byte // a stream: the partial line; else the body

	out        []pivot.Event
	started    bool
	finished   bool // finish_reason came
	done       bool // stop or error emitted; ignore the rest
	stopReason string
	usage      pivot.Usage

	open      openKind
	openIndex int             // the pivot block index of the open block
	openTool  int             // the chat tool-call index of the open tool_use block
	openID    string          // the tool-call id of the open tool_use block
	next      int             // the next pivot block index
	closed    map[int]bool    // chat tool-call indexes whose block ended
	closedIDs map[string]bool // tool-call ids whose block ended
	sawTool   bool            // a tool_use block was opened
}

// NewReplyDecoder makes a decoder for one upstream reply (decision 3).
func (Protocol) NewReplyDecoder(status int, stream bool) pivot.ReplyDecoder {
	return &decoder{status: status, stream: stream, closed: map[int]bool{}, closedIDs: map[string]bool{}}
}

func (d *decoder) ok() bool { return d.status >= 200 && d.status < 300 }

// Feed reads the next bytes of the body.
func (d *decoder) Feed(p []byte) []pivot.Event {
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
	if !d.stream {
		return nil
	}
	d.out = nil
	rest := d.buf
	for !d.done {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			break
		}
		d.line(rest[:i])
		rest = rest[i+1:]
	}
	// Keep only the partial line, in a new array, so that the stream
	// read so far is not held (rule 10).
	d.buf = append([]byte(nil), rest...)
	return d.out
}

// End reads the end of the body.
func (d *decoder) End() []pivot.Event {
	if d.done {
		return nil
	}
	d.out = nil
	switch {
	case !d.ok():
		d.fail(d.status, pivot.ErrorType(d.status), errorMessage(d.status, d.buf))
	case !d.stream:
		d.whole(d.buf)
	default:
		if len(d.buf) > 0 {
			d.line(d.buf)
		}
		d.finish()
	}
	d.buf = nil
	return d.out
}

func (d *decoder) emit(ev pivot.Event) { d.out = append(d.out, ev) }

func (d *decoder) fail(status int, errType, msg string) {
	d.emit(pivot.Event{Kind: pivot.EventError, Status: status, ErrorType: errType, Message: msg})
	d.done = true
}

// finish ends a stream at [DONE] or at the end of the body (decision 5).
func (d *decoder) finish() {
	if d.done {
		return
	}
	if !d.finished {
		d.fail(http.StatusBadGateway, "api_error", msgEndedEarly)
		return
	}
	d.closeOpen()
	// vLLM and OpenAI send finish_reason stop for a forced named tool.
	if d.sawTool && d.stopReason == "end_turn" {
		d.stopReason = "tool_use"
	}
	d.emit(pivot.Event{Kind: pivot.EventUsage, Usage: d.usage})
	d.emit(pivot.Event{Kind: pivot.EventStop, StopReason: d.stopReason})
	d.done = true
}

func (d *decoder) start(id, model string) {
	if !d.started {
		d.started = true
		d.emit(pivot.Event{Kind: pivot.EventStart, ID: id, Model: model})
	}
}

func (d *decoder) openBlock(k openKind, block pivot.BlockKind, toolIndex int, id, name string) {
	d.closeOpen()
	d.emit(pivot.Event{Kind: pivot.EventBlockStart, Index: d.next, Block: block, ToolID: id, ToolName: name})
	d.open, d.openIndex, d.openTool, d.openID = k, d.next, toolIndex, id
	d.sawTool = d.sawTool || k == kindTool
	d.next++
}

func (d *decoder) closeOpen() {
	if d.open == kindNone {
		return
	}
	d.emit(pivot.Event{Kind: pivot.EventBlockStop, Index: d.openIndex})
	if d.open == kindTool {
		d.closed[d.openTool] = true
		d.closedIDs[d.openID] = true
	}
	d.open = kindNone
}

func (d *decoder) delta(k pivot.DeltaKind, v string) {
	d.emit(pivot.Event{Kind: pivot.EventDelta, Index: d.openIndex, Delta: k, Value: v})
}

// The chat-completions reply shapes. Only the fields that rules 20-24 read.
type (
	chatError struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
	}
	chatUsage struct {
		PromptTokens        int  `json:"prompt_tokens"`
		CompletionTokens    int  `json:"completion_tokens"`
		CacheRead           *int `json:"cache_read_input_tokens"`
		CacheCreation       int  `json:"cache_creation_input_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	replyThinking struct {
		Type      string `json:"type"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
	}
	replyToolCall struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	// replyMessage is a stream delta or a whole message. It does not read
	// provider_specific_fields (rule 23).
	replyMessage struct {
		Content          string          `json:"content"`
		Reasoning        string          `json:"reasoning"`
		ReasoningContent string          `json:"reasoning_content"`
		ThinkingBlocks   []replyThinking `json:"thinking_blocks"`
		ToolCalls        []replyToolCall `json:"tool_calls"`
	}
	replyChoice struct {
		Delta        replyMessage `json:"delta"`
		Message      replyMessage `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}
	reply struct {
		ID      string        `json:"id"`
		Model   string        `json:"model"`
		Choices []replyChoice `json:"choices"`
		Usage   *chatUsage    `json:"usage"`
		Error   *chatError    `json:"error"`
	}
)

// line reads one SSE line of a stream (rule 23).
func (d *decoder) line(l []byte) {
	l = bytes.TrimSuffix(l, []byte("\r"))
	payload, ok := bytes.CutPrefix(l, []byte("data:"))
	if !ok {
		return // a blank line, a comment (": ping") or another field
	}
	payload = bytes.TrimSpace(payload)
	if string(payload) == "[DONE]" {
		d.finish()
		return
	}
	var c reply
	if err := json.Unmarshal(payload, &c); err != nil {
		d.fail(http.StatusBadGateway, "api_error", "the inference server sent a stream chunk that is not valid JSON")
		return
	}
	if c.Error != nil {
		d.chunkError(c.Error)
		return
	}
	d.start(c.ID, c.Model)
	if c.Usage != nil {
		d.usage = convertUsage(c.Usage)
	}
	if len(c.Choices) == 0 {
		return
	}
	ch := c.Choices[0]
	d.streamDelta(ch.Delta)
	if ch.FinishReason != "" && !d.done {
		d.closeOpen()
		d.finished = true
		d.stopReason = stopReason(ch.FinishReason)
	}
}

func (d *decoder) streamDelta(m replyMessage) {
	// Thinking text comes only from reasoning or reasoning_content; Bob
	// repeats it in thinking_blocks (rule 23).
	think := m.Reasoning
	if think == "" {
		think = m.ReasoningContent
	}
	if think != "" {
		if d.open != kindThinking {
			d.openBlock(kindThinking, pivot.BlockThinking, 0, "", "")
		}
		d.delta(pivot.DeltaThinking, think)
	}
	for _, tb := range m.ThinkingBlocks {
		if tb.Signature == "" {
			continue
		}
		// A signature with no open thinking block opens an empty one.
		if d.open != kindThinking {
			d.openBlock(kindThinking, pivot.BlockThinking, 0, "", "")
		}
		d.delta(pivot.DeltaSignature, tb.Signature)
	}
	if m.Content != "" {
		if d.open != kindText {
			d.openBlock(kindText, pivot.BlockText, 0, "", "")
		}
		d.delta(pivot.DeltaText, m.Content)
	}
	for _, tc := range m.ToolCalls {
		// Rule 20a: the id identifies the call, so a new id on the open
		// index is a new call, not more of the open one.
		isOpen := d.open == kindTool && d.openTool == tc.Index && (tc.ID == "" || tc.ID == d.openID)
		if tc.ID != "" && !isOpen {
			// Key by id, not index: servers that put every call at index 0
			// close that index once per call.
			if d.closedIDs[tc.ID] {
				d.fail(http.StatusBadGateway, "api_error", "the inference server sent tool call "+tc.ID+" again after that tool call ended")
				return
			}
			// Rule 20a: the id goes to the agent byte-exact.
			d.openBlock(kindTool, pivot.BlockToolUse, tc.Index, tc.ID, tc.Function.Name)
			isOpen = true
		}
		if tc.Function.Arguments == "" {
			continue
		}
		switch {
		case isOpen:
			d.delta(pivot.DeltaInputJSON, tc.Function.Arguments)
		case d.closed[tc.Index]:
			// Decision 6.
			d.fail(http.StatusBadGateway, "api_error", "the inference server sent arguments for tool call "+strconv.Itoa(tc.Index)+" after that tool call ended")
			return
		default:
			d.fail(http.StatusBadGateway, "api_error", "the inference server sent arguments for tool call "+strconv.Itoa(tc.Index)+" before its id")
			return
		}
	}
}

// chunkError converts an {"error":…} chunk or body at status 2xx.
func (d *decoder) chunkError(e *chatError) {
	status := http.StatusInternalServerError
	if n, err := strconv.Atoi(string(e.Code)); err == nil && n >= 400 && n < 600 {
		status = n
	}
	t := "api_error"
	if anthropicErrorTypes[e.Type] {
		t = e.Type
	}
	d.fail(status, t, e.Message)
}

// whole converts a not-streamed 2xx body by rule 20.
func (d *decoder) whole(body []byte) {
	var r reply
	if err := json.Unmarshal(body, &r); err != nil {
		d.fail(http.StatusBadGateway, "api_error", "the inference server sent a reply that is not valid JSON")
		return
	}
	if r.Error != nil {
		d.chunkError(r.Error)
		return
	}
	if len(r.Choices) == 0 {
		d.fail(http.StatusBadGateway, "api_error", "the inference server sent a reply with no choices")
		return
	}
	m := r.Choices[0].Message
	// Decision 7: check every tool input before any event goes out.
	ids := map[string]bool{}
	for _, tc := range m.ToolCalls {
		if a := tc.Function.Arguments; a != "" && !json.Valid([]byte(a)) {
			d.fail(http.StatusBadGateway, "api_error", "the inference server sent tool "+tc.Function.Name+" arguments that are not valid JSON")
			return
		}
		// Rule 20a: the agent's tool_result finds its call by id.
		if tc.ID == "" {
			d.fail(http.StatusBadGateway, "api_error", "the inference server sent tool "+tc.Function.Name+" with no id")
			return
		}
		if ids[tc.ID] {
			d.fail(http.StatusBadGateway, "api_error", "the inference server sent tool call "+tc.ID+" twice")
			return
		}
		ids[tc.ID] = true
	}
	d.start(r.ID, r.Model)
	if len(m.ThinkingBlocks) > 0 {
		for _, tb := range m.ThinkingBlocks {
			// shortcut: a redacted_thinking entry is not sent, because the
			// pivot events have no field for its data; add one when a
			// server sends it.
			if tb.Type != "thinking" {
				continue
			}
			d.openBlock(kindThinking, pivot.BlockThinking, 0, "", "")
			if tb.Thinking != "" {
				d.delta(pivot.DeltaThinking, tb.Thinking)
			}
			if tb.Signature != "" {
				d.delta(pivot.DeltaSignature, tb.Signature)
			}
		}
	} else if think := cmpOr(m.Reasoning, m.ReasoningContent); think != "" {
		d.openBlock(kindThinking, pivot.BlockThinking, 0, "", "")
		d.delta(pivot.DeltaThinking, think)
	}
	if m.Content != "" {
		d.openBlock(kindText, pivot.BlockText, 0, "", "")
		d.delta(pivot.DeltaText, m.Content)
	}
	for _, tc := range m.ToolCalls {
		d.openBlock(kindTool, pivot.BlockToolUse, tc.Index, tc.ID, tc.Function.Name)
		if tc.Function.Arguments != "" {
			d.delta(pivot.DeltaInputJSON, tc.Function.Arguments)
		}
	}
	if r.Usage != nil {
		d.usage = convertUsage(r.Usage)
	}
	d.finished = true
	d.stopReason = stopReason(r.Choices[0].FinishReason)
	d.finish()
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// stopReason maps finish_reason by rule 21.
func stopReason(r string) string {
	switch r {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "refusal"
	}
	return "end_turn"
}

// convertUsage converts usage to the Anthropic meaning by rule 22.
func convertUsage(u *chatUsage) pivot.Usage {
	read := 0
	if u.CacheRead != nil {
		read = *u.CacheRead
	} else if u.PromptTokensDetails != nil {
		read = u.PromptTokensDetails.CachedTokens
	}
	return pivot.Usage{
		Input:         max(0, u.PromptTokens-read-u.CacheCreation),
		Output:        u.CompletionTokens,
		CacheRead:     read,
		CacheCreation: u.CacheCreation,
	}
}

// errorMessage gives the message of a non-2xx reply (decision 3).
func errorMessage(status int, body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
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
		return http.StatusText(status)
	}
	return t
}
