package anthropic

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// The request types have a fixed field order, so the bytes are the same
// each time (rule 9a). outRequest has only the top-level fields of rule
// S3, in that order. Do not add a field that is not in rule S3.
type outRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        []outBlock      `json:"system,omitempty"`
	Messages      []outMessage    `json:"messages"`
	Tools         []outTool       `json:"tools,omitempty"`
	ToolChoice    *outToolChoice  `json:"tool_choice,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	OutputConfig  *outputConfig   `json:"output_config,omitempty"`
	CacheControl  json.RawMessage `json:"cache_control,omitempty"`
}

type outMessage struct {
	Role    string     `json:"role"`
	Content []outBlock `json:"content"`
}

// outBlock is one content block of any kind. Text and Thinking are
// pointers, so that an empty text is still sent.
type outBlock struct {
	Type         string          `json:"type"`
	Text         *string         `json:"text,omitempty"`
	Thinking     *string         `json:"thinking,omitempty"`
	Signature    string          `json:"signature,omitempty"`
	Data         string          `json:"data,omitempty"`
	Source       *outSource      `json:"source,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      []outBlock      `json:"content,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type outSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type outTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type outToolChoice struct {
	Type            string `json:"type"`
	Name            string `json:"name,omitempty"`
	DisableParallel bool   `json:"disable_parallel_tool_use,omitempty"`
}

type outputConfig struct {
	Effort string `json:"effort"`
}

// apiVersion is the Anthropic-Version header that Claude Code sends (S2).
const apiVersion = "2023-06-01"

var (
	emptyObject  = json.RawMessage(`{}`)
	objectSchema = json.RawMessage(`{"type":"object"}`)
	ephemeral    = json.RawMessage(`{"type":"ephemeral"}`)
)

func refuseRequest(msg string) error {
	return &pivot.RequestError{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: msg}
}

// EncodeRequest converts a pivot request to an Anthropic Messages body
// (rules S2-S12). dropped is the sorted list of the fields that it did
// not send.
func (Protocol) EncodeRequest(req pivot.Request, h http.Header) (body []byte, dropped []string, err error) {
	if h != nil {
		h.Set("Anthropic-Version", apiVersion)
		h.Del("Anthropic-Beta")
	}
	e := encoder{dropped: map[string]bool{}}
	e.extra(req.Extra)
	out := outRequest{
		Model:         req.Model,
		MaxTokens:     req.MaxTokens,
		System:        e.system(req.System),
		Messages:      []outMessage{},
		StopSequences: req.Stop,
		Stream:        req.Stream,
	}
	if out.MaxTokens == 0 { // S4
		out.MaxTokens = 16000
		if req.Stream {
			out.MaxTokens = 64000
		}
	}
	for _, m := range req.Messages {
		e.extra(m.Extra)
		role := m.Role
		if role == "system" { // S5
			role = "user"
		}
		if content := e.blocks(m.Content); len(content) > 0 { // decision 17
			out.Messages = append(out.Messages, outMessage{Role: role, Content: content})
		}
	}
	if len(out.Messages) > 0 && out.Messages[0].Role != "user" {
		return nil, nil, refuseRequest("an Anthropic Messages server needs a user message first, but the first message of this request is an " + out.Messages[0].Role + " message")
	}
	for _, t := range req.Tools {
		e.extra(t.Extra)
		if t.Type != "" { // S8: a server tool
			e.dropped[t.Type] = true
			continue
		}
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = objectSchema
		}
		out.Tools = append(out.Tools, outTool{Name: t.Name, Description: t.Description, InputSchema: schema, CacheControl: e.mark(t.CacheControl)})
	}
	if c := req.ToolChoice; c != nil {
		out.ToolChoice = &outToolChoice{Type: c.Type, Name: c.Name, DisableParallel: c.DisableParallel && c.Type != "none"}
	}
	if req.Temperature != nil {
		e.dropped["temperature"] = true
	}
	if req.TopP != nil {
		e.dropped["top_p"] = true
	}
	if req.Thinking != nil { // S11: never send thinking
		e.dropped["thinking"] = true
	}
	if req.Effort != "" {
		if v, ok := effortOf(req.Effort); ok {
			out.OutputConfig = &outputConfig{Effort: v}
		} else {
			e.dropped["effort"] = true
		}
	}
	if !e.marked { // S12
		out.CacheControl = ephemeral
	}

	// marshal panics on raw JSON that is not valid. The decoders check
	// it, but a caller can build a pivot request by hand.
	defer func() {
		if r := recover(); r != nil {
			body, dropped, err = nil, nil, refuseRequest(fmt.Sprint("the request has a field that is not valid JSON: ", r))
		}
	}()
	body = marshal(out)
	dropped = make([]string, 0, len(e.dropped))
	for k := range e.dropped {
		dropped = append(dropped, k)
	}
	sort.Strings(dropped)
	return body, dropped, nil
}

// encoder collects the names of the dropped fields and blocks, and tells
// if any cache marker was sent.
type encoder struct {
	dropped map[string]bool
	marked  bool
}

// extra names each Extra key, without its protocol prefix. Rule S3 sends
// only the fixed fields, so no Extra key is sent (decision 21).
func (e *encoder) extra(x pivot.Extra) {
	for k := range x {
		if _, field, ok := strings.Cut(k, "."); ok {
			k = field
		}
		e.dropped[k] = true
	}
}

// mark gives the cache marker to send, and records that one is sent.
func (e *encoder) mark(cc json.RawMessage) json.RawMessage {
	if len(cc) > 0 {
		e.marked = true
	}
	return cc
}

func (e *encoder) system(blocks []pivot.Block) []outBlock {
	var out []outBlock
	for _, b := range blocks {
		e.extra(b.Extra)
		if b.Kind != pivot.BlockText {
			e.dropped[string(b.Kind)] = true
			continue
		}
		out = append(out, e.text(b))
	}
	return out
}

func (e *encoder) text(b pivot.Block) outBlock {
	return outBlock{Type: "text", Text: &b.Text, CacheControl: e.mark(b.CacheControl)}
}

func (e *encoder) media(b pivot.Block) outBlock {
	src := &outSource{Type: b.SourceType, URL: b.URL}
	if b.SourceType != "url" {
		src.MediaType, src.Data = b.MediaType, b.Data
	}
	return outBlock{Type: string(b.Kind), Source: src, CacheControl: e.mark(b.CacheControl)}
}

// noCache drops the cache marker of a thinking block (decision 14).
func (e *encoder) noCache(b pivot.Block) {
	if len(b.CacheControl) > 0 {
		e.dropped["cache_control"] = true
	}
}

// blocks converts the content of one message by rule S6.
func (e *encoder) blocks(blocks []pivot.Block) []outBlock {
	var out []outBlock
	for _, b := range blocks {
		e.extra(b.Extra)
		switch b.Kind {
		case pivot.BlockText:
			out = append(out, e.text(b))
		case pivot.BlockImage, pivot.BlockDocument:
			out = append(out, e.media(b))
		case pivot.BlockToolUse:
			input := b.Input
			if len(input) == 0 {
				input = emptyObject
			}
			out = append(out, outBlock{Type: "tool_use", ID: toolID(b.ID), Name: b.Name, Input: input, CacheControl: e.mark(b.CacheControl)})
		case pivot.BlockToolResult:
			var content []outBlock
			for _, c := range b.Content {
				e.extra(c.Extra)
				switch c.Kind {
				case pivot.BlockText:
					content = append(content, e.text(c))
				case pivot.BlockImage:
					content = append(content, e.media(c))
				default:
					e.dropped[string(c.Kind)] = true
				}
			}
			out = append(out, outBlock{Type: "tool_result", ToolUseID: toolID(b.ToolUseID), Content: content, IsError: b.IsError, CacheControl: e.mark(b.CacheControl)})
		case pivot.BlockThinking:
			if b.Signature == "" { // Anthropic refuses a thinking block with no signature
				e.dropped["thinking"] = true
				continue
			}
			e.noCache(b)
			out = append(out, outBlock{Type: "thinking", Thinking: &b.Text, Signature: b.Signature})
		case pivot.BlockRedactedThinking:
			e.noCache(b)
			out = append(out, outBlock{Type: "redacted_thinking", Data: b.Data})
		default:
			e.dropped[string(b.Kind)] = true
		}
	}
	return out
}

// toolID gives the id to send for a tool id (rule S7): the id when it
// matches ^[a-zA-Z0-9_-]+$, else "cx_" and the base64url form of its
// bytes, with no padding.
func toolID(id string) string {
	valid := id != ""
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			valid = false
			break
		}
	}
	if valid {
		return id
	}
	return "cx_" + base64.RawURLEncoding.EncodeToString([]byte(id))
}

// effortOf maps a pivot effort to an Anthropic effort (rule S11). false
// when the effort is not in the table.
func effortOf(e string) (string, bool) {
	switch e {
	case "none", "minimal", "low":
		return "low", true
	case "medium", "high", "xhigh", "max":
		return e, true
	}
	return "", false
}
