package responses

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// msgEndedEarly is the message of a reply that ends with no stop event
// (decision 11).
const msgEndedEarly = "the inference server's stream ended before the reply was complete"

// The output types have a fixed field order, so the bytes are the same
// each time (rule 9a of #1911).

type usageDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

type replyUsage struct {
	InputTokens        int          `json:"input_tokens"`
	InputTokensDetails usageDetails `json:"input_tokens_details"`
	OutputTokens       int          `json:"output_tokens"`
	TotalTokens        int          `json:"total_tokens"`
}

// usageOf converts pivot usage, which has the Anthropic meaning, to the
// Responses meaning: input_tokens includes both cache counts (rule R11).
func usageOf(u pivot.Usage) *replyUsage {
	in := u.Input + u.CacheRead + u.CacheCreation
	return &replyUsage{
		InputTokens:        in,
		InputTokensDetails: usageDetails{CachedTokens: u.CacheRead, CacheWriteTokens: u.CacheCreation},
		OutputTokens:       u.Output,
		TotalTokens:        in + u.Output,
	}
}

type incomplete struct {
	Reason string `json:"reason"`
}

type replyError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type response struct {
	ID                string      `json:"id"`
	Object            string      `json:"object"`
	Model             string      `json:"model"`
	Status            string      `json:"status"`
	Output            any         `json:"output,omitempty"` // nil only on response.created
	Usage             *replyUsage `json:"usage,omitempty"`
	IncompleteDetails *incomplete `json:"incomplete_details,omitempty"`
	Error             *replyError `json:"error,omitempty"`
}

type responseEvent struct {
	Response response `json:"response"`
}

type itemEvent struct {
	OutputIndex int `json:"output_index"`
	Item        any `json:"item"`
}

type textDelta struct {
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Delta        string `json:"delta"`
}

type textDone struct {
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Text         string `json:"text"`
}

type summaryDelta struct {
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	SummaryIndex int    `json:"summary_index"`
	Delta        string `json:"delta"`
}

type summaryDone struct {
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	SummaryIndex int    `json:"summary_index"`
	Text         string `json:"text"`
}

type inputDeltaEvent struct {
	ItemID      string `json:"item_id"`
	OutputIndex int    `json:"output_index"`
	Delta       string `json:"delta"`
}

type outputText struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type messageItem struct {
	Type    string       `json:"type"`
	ID      string       `json:"id"`
	Role    string       `json:"role"`
	Status  string       `json:"status"`
	Content []outputText `json:"content"`
}

type summaryText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type reasoningItem struct {
	Type             string        `json:"type"`
	ID               string        `json:"id"`
	Summary          []summaryText `json:"summary"`
	EncryptedContent string        `json:"encrypted_content,omitempty"`
}

type functionCallItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Arguments string `json:"arguments"`
}

type customCallItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"` // so that the next request gives the same flat name
	Input     string `json:"input"`
}

// openItem is the one item that is open. Only its content is kept.
type openItem struct {
	index, out             int // the pivot block index, the output index
	kind                   pivot.BlockKind
	ref                    toolRef // tool_use
	id, callID, data       string
	text, signature, input strings.Builder
}

type replyEncoder struct {
	stream    bool
	done      bool // the stop or the error came
	tools     map[string]toolRef
	id, model string
	usage     pivot.Usage
	next      int // the next output index
	open      *openItem
	out       []byte // the frames of the current call
	output    []any  // not streamed: the closed items
	stop      string // not streamed
	failure   *replyError
	errType   string // not streamed: the type of the failure
}

// NewReplyEncoder makes an encoder for one reply to the agent. req.Origin
// maps the flat tool names back to the agent's tools (rule R9). With
// stream, each event gives its frames at once (rule R11). Without stream,
// End gives one response object (rule R13).
func (Protocol) NewReplyEncoder(req pivot.Request, stream bool) pivot.ReplyEncoder {
	e := &replyEncoder{stream: stream, model: req.Model}
	if o, ok := req.Origin.(*origin); ok && o != nil {
		e.tools = o.tools
	}
	return e
}

// emit adds one SSE frame for a stream. v must give a JSON object: the
// type goes in as its first member.
func (e *replyEncoder) emit(typ string, v any) {
	if !e.stream {
		return
	}
	// The values hold no json.RawMessage, so marshal cannot fail.
	j, _ := marshal(v)
	e.out = append(e.out, "event: "+typ+"\ndata: {\"type\":\""+typ+"\""...)
	if len(j) > 2 {
		e.out = append(e.out, ',')
	}
	e.out = append(e.out, j[1:]...)
	e.out = append(e.out, "\n\n"...)
}

func (e *replyEncoder) Encode(ev pivot.Event) []byte {
	if e.done {
		return nil
	}
	e.out = nil
	switch ev.Kind {
	case pivot.EventStart:
		e.id = ev.ID
		if ev.Model != "" {
			e.model = ev.Model
		}
		e.emit("response.created", responseEvent{response{ID: e.id, Object: "response", Model: e.model, Status: "in_progress"}})
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
	it := &openItem{index: ev.Index, out: e.next, kind: ev.Block, data: ev.Data, callID: ev.ToolID}
	n := strconv.Itoa(it.out)
	var added any
	switch ev.Block {
	case pivot.BlockText:
		it.id = "msg_" + n
		added = messageItem{Type: "message", ID: it.id, Role: "assistant", Status: "in_progress", Content: []outputText{}}
	case pivot.BlockThinking, pivot.BlockRedactedThinking:
		it.id = "rs_" + n
		added = reasoningItem{Type: "reasoning", ID: it.id, Summary: []summaryText{}}
	case pivot.BlockToolUse:
		ref, ok := e.tools[ev.ToolName]
		if !ok {
			ref = toolRef{kind: "function", name: ev.ToolName}
		}
		it.ref = ref
		if ref.kind == "custom" {
			it.id = "ctc_" + n
			added = customCallItem{Type: "custom_tool_call", ID: it.id, CallID: it.callID, Name: ref.name, Namespace: ref.namespace}
		} else {
			it.id = "fc_" + n
			added = functionCallItem{Type: "function_call", ID: it.id, CallID: it.callID, Name: ref.name, Namespace: ref.namespace}
		}
	default:
		return // a block kind that a reply cannot carry
	}
	e.next++
	e.open = it
	e.emit("response.output_item.added", itemEvent{OutputIndex: it.out, Item: added})
}

func (e *replyEncoder) delta(ev pivot.Event) {
	it := e.open
	if it == nil || it.index != ev.Index {
		return
	}
	switch ev.Delta {
	case pivot.DeltaText, pivot.DeltaThinking:
		it.text.WriteString(ev.Value)
		switch it.kind {
		case pivot.BlockText:
			e.emit("response.output_text.delta", textDelta{ItemID: it.id, OutputIndex: it.out, Delta: ev.Value})
		case pivot.BlockThinking:
			e.emit("response.reasoning_summary_text.delta", summaryDelta{ItemID: it.id, OutputIndex: it.out, Delta: ev.Value})
		}
	case pivot.DeltaSignature:
		it.signature.WriteString(ev.Value)
	case pivot.DeltaInputJSON:
		it.input.WriteString(ev.Value)
		if it.kind == pivot.BlockToolUse && it.ref.kind != "custom" {
			e.emit("response.function_call_arguments.delta", inputDeltaEvent{ItemID: it.id, OutputIndex: it.out, Delta: ev.Value})
		}
	}
}

func (e *replyEncoder) closeBlock(index int) {
	it := e.open
	if it == nil || it.index != index {
		return
	}
	e.open = nil
	text := it.text.String()
	var item any
	switch it.kind {
	case pivot.BlockText:
		e.emit("response.output_text.done", textDone{ItemID: it.id, OutputIndex: it.out, Text: text})
		item = messageItem{Type: "message", ID: it.id, Role: "assistant", Status: "completed",
			Content: []outputText{{Type: "output_text", Text: text, Annotations: []any{}}}}
	case pivot.BlockThinking:
		r := reasoningItem{Type: "reasoning", ID: it.id, Summary: []summaryText{}}
		if text != "" {
			e.emit("response.reasoning_summary_text.done", summaryDone{ItemID: it.id, OutputIndex: it.out, Text: text})
			r.Summary = append(r.Summary, summaryText{Type: "summary_text", Text: text})
		}
		if sig := it.signature.String(); sig != "" {
			r.EncryptedContent = encodeReasoning(pivot.Block{Kind: pivot.BlockThinking, Text: text, Signature: sig})
		}
		item = r
	case pivot.BlockRedactedThinking:
		item = reasoningItem{Type: "reasoning", ID: it.id, Summary: []summaryText{},
			EncryptedContent: encodeReasoning(pivot.Block{Kind: pivot.BlockRedactedThinking, Data: it.data})}
	case pivot.BlockToolUse:
		input := it.input.String()
		if it.ref.kind != "custom" {
			if input == "" {
				input = "{}" // a call with no arguments: Codex parses arguments as JSON
			}
			item = functionCallItem{Type: "function_call", ID: it.id, CallID: it.callID, Name: it.ref.name, Namespace: it.ref.namespace, Arguments: input}
			break
		}
		var in struct {
			Input *string `json:"input"`
		}
		if json.Unmarshal([]byte(input), &in) != nil || in.Input == nil {
			e.fail(502, "api_error", "the inference server sent input for the tool "+it.ref.String()+
				" that is not a JSON object with a string input")
			return
		}
		e.emit("response.custom_tool_call_input.delta", inputDeltaEvent{ItemID: it.id, OutputIndex: it.out, Delta: *in.Input})
		item = customCallItem{Type: "custom_tool_call", ID: it.id, CallID: it.callID, Name: it.ref.name, Namespace: it.ref.namespace, Input: *in.Input}
	}
	if e.stream {
		e.emit("response.output_item.done", itemEvent{OutputIndex: it.out, Item: item})
	} else {
		e.output = append(e.output, item)
	}
}

// terminal gives the terminal event type, the status and the incomplete
// details for a stop reason (rule R11).
func terminal(reason string) (string, string, *incomplete) {
	switch reason {
	case "max_tokens":
		return "response.incomplete", "incomplete", &incomplete{Reason: "max_output_tokens"}
	case "refusal":
		return "response.incomplete", "incomplete", &incomplete{Reason: "content_filter"}
	}
	return "response.completed", "completed", nil
}

func (e *replyEncoder) finish(reason string) {
	e.done = true
	e.stop = reason
	typ, status, inc := terminal(reason)
	e.emit(typ, responseEvent{response{ID: e.id, Object: "response", Model: e.model, Status: status,
		Output: []any{}, Usage: usageOf(e.usage), IncompleteDetails: inc}})
}

func (e *replyEncoder) fail(status int, errType, message string) {
	e.done = true
	e.failure = &replyError{Code: errorCode(status, errType, message), Message: message}
	e.errType = errType
	e.emit("response.failed", responseEvent{response{ID: e.id, Object: "response", Model: e.model, Status: "failed",
		Output: []any{}, Error: e.failure}})
}

// contextPhrases say that the prompt is too long, in the errors that the
// Chat and Anthropic decoders pass through (decision 12).
var contextPhrases = []string{"prompt is too long", "context length", "context_length_exceeded", "maximum context"}

// errorCode gives the OpenAI error code that Codex reads (rule R12).
func errorCode(status int, errType, message string) string {
	switch {
	case errType == "rate_limit_error":
		return "rate_limit_exceeded"
	case errType == "overloaded_error" || status == 529:
		return "server_is_overloaded"
	}
	l := strings.ToLower(message)
	for _, p := range contextPhrases {
		if strings.Contains(l, p) {
			return "context_length_exceeded"
		}
	}
	return errType
}

// End gives the rest of a stream, or the one body of a not-streamed
// reply. A reply with no stop event fails (decision 11).
func (e *replyEncoder) End() []byte {
	e.out = nil
	if !e.done {
		e.fail(0, "api_error", msgEndedEarly)
	}
	if e.stream {
		return e.out
	}
	if e.failure != nil {
		type body struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		}
		b, _ := marshal(struct {
			Error body `json:"error"`
		}{body{Message: e.failure.Message, Type: e.errType, Code: e.failure.Code}})
		return b
	}
	_, status, inc := terminal(e.stop)
	output := e.output
	if output == nil {
		output = []any{}
	}
	b, _ := marshal(response{ID: e.id, Object: "response", Model: e.model, Status: status,
		Output: output, Usage: usageOf(e.usage), IncompleteDetails: inc})
	return b
}
