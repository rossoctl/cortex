package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// The refusals of rules K3 and K4 of #1966, with their exact text.
var (
	errFileID   = refuse("this inference server cannot read uploaded files (file_id)")
	errAudio    = refuse("this inference server cannot read audio")
	errLegacy   = refuse("Cortex cannot translate legacy function_call messages. Use tool_calls.")
	errChoices  = refuse("Cortex can translate only one choice (n = 1).")
	errFormat   = refuse("Cortex cannot translate structured output (response_format) yet.")
	errImageURL = refuse("an image_url must have a base64 data URL or an http(s) URL")
	errPDF      = refuse("Cortex can translate only PDF files (file_data as a data:application/pdf;base64 URL).")
)

// present is the Extra value of a dropped member that has no raw value.
var present = json.RawMessage("true")

// inMessage is one message of a request. The members that change type
// are any, not json.RawMessage: each raw layer scans its bytes again
// (rule K11). Members that rule K3 does not name are not read.
type inMessage struct {
	Role             string          `json:"role"`
	Content          any             `json:"content"`
	Name             json.RawMessage `json:"name"`
	ReasoningContent any             `json:"reasoning_content"`
	Reasoning        any             `json:"reasoning"`
	Refusal          any             `json:"refusal"`
	ThinkingBlocks   []thinkingBlock `json:"thinking_blocks"`
	ToolCalls        []inToolCall    `json:"tool_calls"`
	ToolCallID       string          `json:"tool_call_id"`
	FunctionCall     any             `json:"function_call"`
}

type inToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments any    `json:"arguments"`
	} `json:"function"`
}

type inTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      json.RawMessage `json:"strict"`
	} `json:"function"`
}

// DecodeRequest converts a chat-completions request body to the pivot
// form (rules K3 and K4 of #1966). Each top-level field that the pivot has
// no place for goes to Extra under "chat-completions.<field>", so that the
// target encoder names it in dropped. A body that is not a valid request
// gives a *pivot.RequestError with status 400.
func (Protocol) DecodeRequest(body []byte) (pivot.Request, error) {
	var d requestDecoder
	if err := d.request(body); err != nil {
		var re *pivot.RequestError
		if errors.As(err, &re) {
			return pivot.Request{}, err
		}
		return pivot.Request{}, refuse("the request body is not a valid Chat Completions request: " + err.Error())
	}
	return d.req, nil
}

// requestDecoder holds the state of one pass over a request. The name
// decoder is the reply decoder.
type requestDecoder struct {
	req       pivot.Request
	started   bool // a user, assistant or tool message came
	afterTool bool // the last message was a tool message
}

func (d *requestDecoder) request(body []byte) error {
	m, msgs, err := members(body)
	if err != nil {
		return err
	}
	if m["functions"] != nil || m["function_call"] != nil {
		return errLegacy
	}
	var maxCompletion, n *int
	var parallel *bool
	var stop, tools, choice json.RawMessage
	var streamOpts map[string]json.RawMessage
	var format *struct {
		Type string `json:"type"`
	}
	r := &d.req
	nRaw := m["n"]
	err = errors.Join(
		take(m, "model", &r.Model),
		take(m, "stream", &r.Stream),
		take(m, "max_completion_tokens", &maxCompletion),
		take(m, "max_tokens", &r.MaxTokens),
		take(m, "temperature", &r.Temperature),
		take(m, "top_p", &r.TopP),
		take(m, "reasoning_effort", &r.Effort),
		take(m, "parallel_tool_calls", &parallel),
		take(m, "n", &n),
		take(m, "response_format", &format),
		take(m, "stream_options", &streamOpts),
		take(m, "stop", &stop),
		take(m, "tools", &tools),
		take(m, "tool_choice", &choice),
	)
	if err != nil {
		return err
	}
	if maxCompletion != nil {
		r.MaxTokens = *maxCompletion
	}
	if n != nil && *n > 1 {
		return errChoices
	}
	if n != nil && *n < 1 {
		d.dropRaw("n", nRaw)
	}
	if format != nil && format.Type != "text" {
		return errFormat
	}
	for k, v := range streamOpts {
		if k != "include_usage" {
			k = "stream_options." + k
		}
		if !isNull(v) {
			d.dropRaw(k, v)
		}
	}
	for k, v := range m {
		d.dropRaw(k, v)
	}
	if r.Stop, err = stops(stop); err != nil {
		return err
	}
	if err := d.tools(tools); err != nil {
		return err
	}
	if err := d.toolChoice(choice); err != nil {
		return err
	}
	if parallel != nil && !*parallel {
		if r.ToolChoice == nil {
			r.ToolChoice = &pivot.ToolChoice{Type: "auto"}
		}
		r.ToolChoice.DisableParallel = true
	}
	for i := range msgs {
		if err := d.message(&msgs[i]); err != nil {
			return err
		}
	}
	return nil
}

// members reads the members of the request object in one pass. messages
// goes straight to its structs, and each other member to m as raw JSON.
// A null member is left out.
func members(body []byte) (map[string]json.RawMessage, []inMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, nil, errors.New("must be a JSON object")
	}
	m := map[string]json.RawMessage{}
	var msgs []inMessage
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, _ := t.(string) // a member name, as the object has no other tokens here
		if key == "messages" {
			msgs = nil
			err = dec.Decode(&msgs)
		} else {
			var raw json.RawMessage
			if err = dec.Decode(&raw); err == nil && !isNull(raw) {
				m[key] = raw
			} else {
				delete(m, key)
			}
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", key, err)
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, nil, errors.New("data after the request object")
	}
	return m, msgs, nil
}

// dropRaw records the field name in the request's Extra.
func (d *requestDecoder) dropRaw(name string, v json.RawMessage) {
	setExtra(&d.req.Extra, name, v)
}

func setExtra(x *pivot.Extra, name string, v json.RawMessage) {
	if *x == nil {
		*x = pivot.Extra{}
	}
	(*x)[extraPrefix+name] = v
}

// mergeExtra copies each key of from to to.
func mergeExtra(to *pivot.Extra, from pivot.Extra) {
	for k, v := range from {
		if *to == nil {
			*to = pivot.Extra{}
		}
		(*to)[k] = v
	}
}

// stops reads stop: a string or a list of strings.
func stops(raw json.RawMessage) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil, nil
		}
		return []string{s}, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, errors.New("stop must be a string or a list of strings")
	}
	return list, nil
}

// tools converts tools by rule K4.
func (d *requestDecoder) tools(raw json.RawMessage) error {
	if raw == nil {
		return nil
	}
	var tools []inTool
	if err := json.Unmarshal(raw, &tools); err != nil {
		return fmt.Errorf("tools: %w", err)
	}
	for _, t := range tools {
		if t.Type != "function" {
			return refuse(fmt.Sprintf("Cortex cannot translate a tool of type %q.", t.Type))
		}
		f := t.Function
		out := pivot.Tool{Name: f.Name, Description: f.Description}
		if !isNull(f.Parameters) {
			out.InputSchema = f.Parameters
		}
		if !isNull(f.Strict) {
			setExtra(&out.Extra, "strict", f.Strict)
		}
		d.req.Tools = append(d.req.Tools, out)
	}
	return nil
}

// toolChoice converts tool_choice by rule K4 and decision 22.
func (d *requestDecoder) toolChoice(raw json.RawMessage) error {
	if raw == nil {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t := map[string]string{"auto": "auto", "none": "none", "required": "any"}[s]; t != "" {
			d.req.ToolChoice = &pivot.ToolChoice{Type: t}
			return nil
		}
		d.dropRaw("tool_choice", raw)
		return nil
	}
	var o struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return fmt.Errorf("tool_choice: %w", err)
	}
	if o.Type != "function" || o.Function.Name == "" {
		d.dropRaw("tool_choice", raw)
		return nil
	}
	d.req.ToolChoice = &pivot.ToolChoice{Type: "tool", Name: o.Function.Name}
	return nil
}

// message converts one message by rule K3.
func (d *requestDecoder) message(m *inMessage) error {
	var extra pivot.Extra
	if !isNull(m.Name) {
		setExtra(&extra, "name", m.Name)
	}
	switch m.Role {
	case "system", "developer":
		blocks, err := parts(m.Content, &extra)
		if err != nil {
			return err
		}
		if d.started {
			d.add("system", false, extra, blocks)
			return nil
		}
		// The pivot system has only text blocks.
		for _, b := range blocks {
			if b.Kind == pivot.BlockText {
				d.req.System = append(d.req.System, b)
			} else {
				d.dropRaw(string(b.Kind), present)
			}
		}
		mergeExtra(&d.req.Extra, extra)
	case "user":
		blocks, err := parts(m.Content, &extra)
		if err != nil {
			return err
		}
		d.add("user", d.afterTool, extra, blocks)
	case "assistant":
		blocks, err := assistant(m, &extra)
		if err != nil {
			return err
		}
		d.add("assistant", false, extra, blocks)
	case "tool":
		var content []pivot.Block
		if m.Content != "" {
			var err error
			if content, err = parts(m.Content, &extra); err != nil {
				return err
			}
		}
		b := pivot.Block{Kind: pivot.BlockToolResult, ToolUseID: m.ToolCallID, Content: content}
		// Decision 18: the marker of the last part goes to the result.
		if n := len(content); n > 0 {
			b.CacheControl = content[n-1].CacheControl
			for i := range content {
				content[i].CacheControl = nil
			}
		}
		d.add("user", d.afterTool, extra, []pivot.Block{b})
		d.afterTool = true
	default:
		return fmt.Errorf("a message has the role %q", m.Role)
	}
	return nil
}

// add appends blocks to the last message when join is true, else starts
// a new message. A message with no blocks is left out, and its Extra goes
// to the request (decision 20).
func (d *requestDecoder) add(role string, join bool, extra pivot.Extra, blocks []pivot.Block) {
	if len(blocks) == 0 {
		mergeExtra(&d.req.Extra, extra)
		return
	}
	d.started, d.afterTool = true, false
	msgs := d.req.Messages
	if n := len(msgs); join && n > 0 && msgs[n-1].Role == role {
		msgs[n-1].Content = append(msgs[n-1].Content, blocks...)
		mergeExtra(&msgs[n-1].Extra, extra)
		return
	}
	d.req.Messages = append(msgs, pivot.Message{Role: role, Content: blocks, Extra: extra})
}

// assistant converts an assistant message by rule K3: thinking, then
// text, then tool_use.
func assistant(m *inMessage, extra *pivot.Extra) ([]pivot.Block, error) {
	if m.FunctionCall != nil {
		return nil, errLegacy
	}
	var out []pivot.Block
	for _, t := range m.ThinkingBlocks {
		switch t.Type {
		case "thinking":
			var text string
			if t.Thinking != nil {
				text = *t.Thinking
			}
			out = append(out, pivot.Block{Kind: pivot.BlockThinking, Text: text, Signature: t.Signature})
		case "redacted_thinking":
			out = append(out, pivot.Block{Kind: pivot.BlockRedactedThinking, Data: t.Data})
		default:
			setExtra(extra, "thinking_blocks", present)
		}
	}
	if len(m.ThinkingBlocks) == 0 {
		for _, r := range []struct {
			name string
			v    any
		}{{"reasoning_content", m.ReasoningContent}, {"reasoning", m.Reasoning}} {
			s, ok := r.v.(string)
			if !ok && r.v != nil {
				setExtra(extra, r.name, present)
			}
			if s != "" {
				out = append(out, pivot.Block{Kind: pivot.BlockThinking, Text: s})
				break
			}
		}
	}
	text, err := parts(m.Content, extra)
	if err != nil {
		return nil, err
	}
	for _, b := range text {
		switch {
		case b.Kind != pivot.BlockText:
			setExtra(extra, string(b.Kind), present)
		case b.Text != "":
			// An empty text block is left out: Anthropic refuses it, and
			// the Chat encoder sends no content for it.
			out = append(out, b)
		}
	}
	if m.Refusal != nil {
		setExtra(extra, "refusal", present)
	}
	for _, c := range m.ToolCalls {
		args, err := str(c.Function.Arguments, "function.arguments")
		if err != nil {
			return nil, err
		}
		var input json.RawMessage
		if strings.TrimSpace(args) != "" {
			if !json.Valid([]byte(args)) {
				return nil, refuse(fmt.Sprintf("the arguments of a call to the tool %q are not valid JSON", c.Function.Name))
			}
			input = json.RawMessage(args)
		}
		// Rule 20a of #1911: the id is kept byte for byte.
		out = append(out, pivot.Block{Kind: pivot.BlockToolUse, ID: c.ID, Name: c.Function.Name, Input: input})
	}
	return out, nil
}

// parts converts message content: a string, or a list of parts. A part
// that rule K3 drops is recorded in extra.
func parts(v any, extra *pivot.Extra) ([]pivot.Block, error) {
	switch v := v.(type) {
	case nil:
		return nil, nil
	case string:
		return []pivot.Block{{Kind: pivot.BlockText, Text: v}}, nil
	case []any:
		out := make([]pivot.Block, 0, len(v))
		for _, p := range v {
			b, ok, err := inPart(p, extra)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, b)
			}
		}
		return out, nil
	}
	return nil, errors.New("content must be a string or a list of parts")
}

// inPart converts one content part. false when the part is dropped.
func inPart(v any, extra *pivot.Extra) (pivot.Block, bool, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return pivot.Block{}, false, errors.New("a content part must be an object")
	}
	kind, err := str(m["type"], "type")
	if err != nil {
		return pivot.Block{}, false, err
	}
	var b pivot.Block
	switch kind {
	case "text":
		var text string
		text, err = str(m["text"], "text")
		b = pivot.Block{Kind: pivot.BlockText, Text: text}
	case "image_url":
		b, err = image(m["image_url"])
	case "file":
		b, err = file(m["file"])
	case "input_audio":
		return b, false, errAudio
	case "":
		return b, false, errors.New("a content part has no type")
	default:
		setExtra(extra, kind, present) // refusal, and parts with no pivot form
		return b, false, nil
	}
	if err != nil {
		return b, false, err
	}
	if b.CacheControl, err = cacheControl(m["cache_control"]); err != nil {
		return b, false, err
	}
	return b, true, nil
}

// image converts an image_url member: an object with url, or a URL string
// (decision 16).
func image(v any) (pivot.Block, error) {
	var b pivot.Block
	u, ok := v.(string)
	if o, isObj := v.(map[string]any); isObj {
		var err error
		if u, err = str(o["url"], "image_url.url"); err != nil {
			return b, err
		}
		if o["detail"] != nil {
			setExtra(&b.Extra, "detail", present)
		}
	} else if !ok {
		return b, errors.New("image_url must be an object or a string")
	}
	if rest, ok := strings.CutPrefix(u, "data:"); ok {
		meta, data, ok := strings.Cut(rest, ",")
		media, b64 := strings.CutSuffix(meta, ";base64")
		if !ok || !b64 {
			return b, errImageURL
		}
		b.Kind, b.SourceType, b.MediaType, b.Data = pivot.BlockImage, "base64", media, data
		return b, nil
	}
	if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		b.Kind, b.SourceType, b.URL = pivot.BlockImage, "url", u
		return b, nil
	}
	return b, errImageURL
}

// file converts a file member: only a PDF as a base64 data URL.
func file(v any) (pivot.Block, error) {
	var b pivot.Block
	o, ok := v.(map[string]any)
	if !ok {
		return b, errors.New("file must be an object")
	}
	if o["file_id"] != nil {
		return b, errFileID
	}
	fd, err := str(o["file_data"], "file.file_data")
	if err != nil {
		return b, err
	}
	data, ok := strings.CutPrefix(fd, "data:application/pdf;base64,")
	if !ok {
		return b, errPDF
	}
	if o["filename"] != nil {
		setExtra(&b.Extra, "filename", present)
	}
	b.Kind, b.SourceType, b.MediaType, b.Data = pivot.BlockDocument, "base64", "application/pdf", data
	return b, nil
}

// cacheControl writes a decoded cache_control again (decision 19). A
// marker with only type and ttl keeps its member order, so that the
// usual markers keep their bytes. Another marker has sorted keys.
func cacheControl(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	if m, ok := v.(map[string]any); ok {
		typ, ok1 := m["type"].(string)
		ttl, ok2 := m["ttl"].(string)
		if ok1 && (len(m) == 1 || len(m) == 2 && ok2) {
			return marshal(struct {
				Type string `json:"type"`
				TTL  string `json:"ttl,omitempty"`
			}{typ, ttl})
		}
	}
	return marshal(v)
}

// str gives v as a string: "" for a missing or null member.
func str(v any, name string) (string, error) {
	switch v := v.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	}
	return "", fmt.Errorf("%s must be a string", name)
}

// take parses the member key of m into dst and deletes it from m. A
// missing member leaves dst unchanged.
func take(m map[string]json.RawMessage, key string, dst any) error {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	delete(m, key)
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

func isNull(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }
