package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// DecodeRequest converts an Anthropic Messages request body to the pivot
// form. Each field that the pivot has no place for goes to the Extra of
// its request, message or block, under "anthropic.<field>". A body that
// is not a valid request gives a *pivot.RequestError with status 400.
func (Protocol) DecodeRequest(body []byte) (pivot.Request, error) {
	req, err := decodeRequest(body)
	if err != nil {
		return pivot.Request{}, &pivot.RequestError{
			Status:  400,
			Type:    "invalid_request_error",
			Message: "the request body is not a valid Anthropic Messages request: " + err.Error(),
		}
	}
	return req, nil
}

// walker reads a JSON body in one pass (rule S16). Each member goes
// straight to its Go field. A nested json.RawMessage layer scans the same
// bytes again at each level, and a long session has many levels.
type walker struct {
	dec  *json.Decoder
	body []byte
}

func newWalker(body []byte) *walker {
	return &walker{dec: json.NewDecoder(bytes.NewReader(body)), body: body}
}

func decodeRequest(body []byte) (pivot.Request, error) {
	w := newWalker(body)
	var req pivot.Request
	var choice *struct {
		Type            string `json:"type"`
		Name            string `json:"name"`
		DisableParallel bool   `json:"disable_parallel_tool_use"`
	}
	var thinking *struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	}
	err := w.object(false, func(key string) error {
		switch key {
		case "model":
			return w.value(key, &req.Model)
		case "system":
			var err error
			if req.System, err = w.content(); err != nil {
				return fmt.Errorf("system: %w", err)
			}
			return nil
		case "messages":
			req.Messages = nil
			return w.list(key, func() error {
				msg, err := w.message()
				req.Messages = append(req.Messages, msg)
				return err
			})
		case "tools":
			req.Tools = nil
			return w.list(key, func() error {
				t, err := w.tool()
				req.Tools = append(req.Tools, t)
				return err
			})
		case "tool_choice":
			return w.value(key, &choice)
		case "max_tokens":
			return w.value(key, &req.MaxTokens)
		case "stop_sequences":
			return w.value(key, &req.Stop)
		case "temperature":
			return w.value(key, &req.Temperature)
		case "top_p":
			return w.value(key, &req.TopP)
		case "stream":
			return w.value(key, &req.Stream)
		case "thinking":
			return w.value(key, &thinking)
		case "output_config":
			return w.outputConfig(&req)
		}
		return w.extra(&req.Extra, key)
	})
	if err != nil {
		return pivot.Request{}, err
	}
	if _, err := w.dec.Token(); err != io.EOF {
		return pivot.Request{}, errors.New("data after the request object")
	}
	if choice != nil {
		req.ToolChoice = &pivot.ToolChoice{Type: choice.Type, Name: choice.Name, DisableParallel: choice.DisableParallel}
	}
	if thinking != nil {
		req.Thinking = &pivot.Thinking{Type: thinking.Type, BudgetTokens: thinking.BudgetTokens}
	}
	return req, nil
}

// outputConfig moves output_config.effort to effort. The other members of
// output_config go to Extra as anthropic.output_config.
func (w *walker) outputConfig(req *pivot.Request) error {
	var rest map[string]json.RawMessage
	err := w.object(true, func(key string) error {
		if key == "effort" {
			return w.value(key, &req.Effort)
		}
		return w.rest(&rest, key)
	})
	if err != nil {
		return fmt.Errorf("output_config.%w", err)
	}
	setRest(&req.Extra, "output_config", rest)
	return nil
}

func (w *walker) message() (pivot.Message, error) {
	var msg pivot.Message
	err := w.object(false, func(key string) error {
		switch key {
		case "role":
			return w.value(key, &msg.Role)
		case "content":
			var err error
			if msg.Content, err = w.content(); err != nil {
				return fmt.Errorf("content: %w", err)
			}
			return nil
		}
		return w.extra(&msg.Extra, key)
	})
	return msg, err
}

// content reads content that is a string (one text block) or a list of
// blocks.
func (w *walker) content() ([]pivot.Block, error) {
	t, err := w.dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := t.(type) {
	case nil:
		return nil, nil
	case string:
		return []pivot.Block{{Kind: pivot.BlockText, Text: t}}, nil
	case json.Delim:
		if t != '[' {
			break
		}
		out := []pivot.Block{}
		for i := 0; w.dec.More(); i++ {
			b, err := w.block()
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out = append(out, b)
		}
		_, err := w.dec.Token()
		return out, err
	}
	return nil, errors.New("must be a string or a list of blocks")
}

// block reads one content block. Which members a block has depends on its
// type, so a byte scan finds the type before the walk. Only when the scan
// finds no "type" name in plain letters is the block read once as raw JSON
// to find its type, and then read again.
func (w *walker) block() (pivot.Block, error) {
	var b pivot.Block
	var kind string
	src := w
	if t, ok := w.scanType(); ok {
		if err := json.Unmarshal(t, &kind); err != nil {
			return b, fmt.Errorf("type: %w", err)
		}
	} else {
		var raw json.RawMessage
		if err := w.dec.Decode(&raw); err != nil {
			return b, err
		}
		// A map, not a struct: struct decoding matches "Type" or "TYPE" too.
		var head map[string]json.RawMessage
		if err := json.Unmarshal(raw, &head); err != nil {
			return b, err
		}
		if t, ok := head["type"]; ok {
			if err := json.Unmarshal(t, &kind); err != nil {
				return b, fmt.Errorf("type: %w", err)
			}
		}
		src = newWalker(raw)
	}
	err := src.object(false, func(key string) error {
		switch key {
		case "type":
			var t string
			if err := src.value(key, &t); err != nil {
				return err
			}
			// A second type that differs would leave members read for the wrong kind.
			if kind != "" && t != kind {
				return fmt.Errorf("type %q differs from type %q", t, kind)
			}
			kind = t
			return nil
		case "cache_control":
			return src.raw(&b.CacheControl)
		}
		ok, err := src.blockMember(&b, pivot.BlockKind(kind), key)
		if !ok {
			return src.extra(&b.Extra, key)
		}
		if err != nil {
			return fmt.Errorf("%s block: %w", kind, err)
		}
		return nil
	})
	if err != nil {
		return pivot.Block{}, err
	}
	if kind == "" {
		return pivot.Block{}, errors.New("a block has no type")
	}
	b.Kind = pivot.BlockKind(kind)
	return b, nil
}

// scanType finds the first member named "type" in the object that is the
// next value, at the top level of that object, and gives its value when
// that value is a JSON string. It decodes nothing. It gives false when the
// next value is not an object, when no member name is "type" in plain
// letters, when the value is not a string, or when the bytes end first.
func (w *walker) scanType() ([]byte, bool) {
	rest := bytes.TrimLeft(w.body[w.dec.InputOffset():], " \t\r\n,")
	if len(rest) == 0 || rest[0] != '{' {
		return nil, false
	}
	depth, name := 1, true // name: the next string at depth 1 is a member name
	for i := 1; i < len(rest); i++ {
		switch rest[i] {
		case '"':
			end := stringEnd(rest, i)
			if end < 0 {
				return nil, false
			}
			if depth == 1 && name {
				if string(rest[i:end]) == `"type"` {
					v := bytes.TrimLeft(rest[end:], " \t\r\n")
					if len(v) == 0 || v[0] != ':' {
						return nil, false
					}
					v = bytes.TrimLeft(v[1:], " \t\r\n")
					if len(v) == 0 || v[0] != '"' {
						return nil, false
					}
					if e := stringEnd(v, 0); e > 0 {
						return v[:e], true
					}
					return nil, false
				}
				name = false
			}
			i = end - 1
		case '{', '[':
			depth++
		case '}', ']':
			if depth--; depth == 0 {
				return nil, false
			}
		case ',':
			if depth == 1 {
				name = true
			}
		}
	}
	return nil, false
}

// stringEnd gives the index after the closing quote of the JSON string
// that starts at b[i], or -1 when b ends first.
func stringEnd(b []byte, i int) int {
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return -1
}

// blockMember reads the member key of a block of this kind. It gives
// false, and reads nothing, when the kind has no such member.
func (w *walker) blockMember(b *pivot.Block, kind pivot.BlockKind, key string) (bool, error) {
	switch kind {
	case pivot.BlockText:
		if key == "text" {
			return true, w.value(key, &b.Text)
		}
	case pivot.BlockImage, pivot.BlockDocument:
		if key == "source" {
			return true, w.source(b)
		}
	case pivot.BlockToolUse:
		switch key {
		case "id":
			return true, w.value(key, &b.ID)
		case "name":
			return true, w.value(key, &b.Name)
		case "input":
			return true, w.raw(&b.Input)
		}
	case pivot.BlockToolResult:
		switch key {
		case "tool_use_id":
			return true, w.value(key, &b.ToolUseID)
		case "is_error":
			return true, w.value(key, &b.IsError)
		case "content":
			var err error
			if b.Content, err = w.content(); err != nil {
				return true, fmt.Errorf("content: %w", err)
			}
			return true, nil
		}
	case pivot.BlockThinking:
		switch key {
		case "thinking":
			return true, w.value(key, &b.Text)
		case "signature":
			return true, w.value(key, &b.Signature)
		}
	case pivot.BlockRedactedThinking:
		if key == "data" {
			return true, w.value(key, &b.Data)
		}
	}
	return false, nil
}

// source reads the source of an image or a document. The members of
// source that the pivot has no place for go to Extra as
// anthropic.source.
func (w *walker) source(b *pivot.Block) error {
	var rest map[string]json.RawMessage
	err := w.object(true, func(key string) error {
		switch key {
		case "type":
			return w.value(key, &b.SourceType)
		case "media_type":
			return w.value(key, &b.MediaType)
		case "data":
			return w.value(key, &b.Data)
		case "url":
			return w.value(key, &b.URL)
		}
		return w.rest(&rest, key)
	})
	if err != nil {
		return fmt.Errorf("source.%w", err)
	}
	setRest(&b.Extra, "source", rest)
	return nil
}

func (w *walker) tool() (pivot.Tool, error) {
	var t pivot.Tool
	err := w.object(false, func(key string) error {
		switch key {
		case "name":
			return w.value(key, &t.Name)
		case "description":
			return w.value(key, &t.Description)
		case "input_schema":
			return w.raw(&t.InputSchema)
		case "type":
			return w.value(key, &t.Type)
		case "cache_control":
			return w.raw(&t.CacheControl)
		}
		return w.extra(&t.Extra, key)
	})
	if t.Type == "custom" { // Anthropic's name for a function tool
		t.Type = ""
	}
	return t, err
}

// object reads a JSON object and calls member with each member name.
// member must read the value. A null is the same as an empty object when
// nullOK is true, and an error when it is false.
func (w *walker) object(nullOK bool, member func(key string) error) error {
	t, err := w.dec.Token()
	if err != nil {
		return err
	}
	if t == nil && nullOK {
		return nil
	}
	if t != json.Delim('{') {
		return errors.New("must be a JSON object")
	}
	for w.dec.More() {
		t, err := w.dec.Token()
		if err != nil {
			return err
		}
		key, _ := t.(string) // a member name, as the object has no other tokens here
		if err := member(key); err != nil {
			return err
		}
	}
	_, err = w.dec.Token()
	return err
}

// list reads a JSON list, or a null, and calls item for each item. item
// must read the item.
func (w *walker) list(key string, item func() error) error {
	t, err := w.dec.Token()
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	if t == nil {
		return nil
	}
	if t != json.Delim('[') {
		return fmt.Errorf("%s: must be a list", key)
	}
	for i := 0; w.dec.More(); i++ {
		if err := item(); err != nil {
			return fmt.Errorf("%s[%d]: %w", key, i, err)
		}
	}
	_, err = w.dec.Token()
	return err
}

// value reads the next value into dst. A null leaves dst unchanged.
func (w *walker) value(key string, dst any) error {
	if err := w.dec.Decode(dst); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

// raw reads the next value into dst with its bytes unchanged. A null
// gives nil.
func (w *walker) raw(dst *json.RawMessage) error {
	var r json.RawMessage
	if err := w.dec.Decode(&r); err != nil {
		return err
	}
	if string(r) != "null" {
		*dst = r
	}
	return nil
}

// extra reads the next value into e under anthropic.<key>.
func (w *walker) extra(e *pivot.Extra, key string) error {
	var r json.RawMessage
	if err := w.dec.Decode(&r); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	if *e == nil {
		*e = pivot.Extra{}
	}
	(*e)[extraPrefix+key] = r
	return nil
}

// rest reads the next value into m under key.
func (w *walker) rest(m *map[string]json.RawMessage, key string) error {
	var r json.RawMessage
	if err := w.dec.Decode(&r); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	if *m == nil {
		*m = map[string]json.RawMessage{}
	}
	(*m)[key] = r
	return nil
}

// setRest puts the leftover members of a nested object in e as one JSON
// object under anthropic.<key>.
func setRest(e *pivot.Extra, key string, rest map[string]json.RawMessage) {
	if len(rest) == 0 {
		return
	}
	if *e == nil {
		*e = pivot.Extra{}
	}
	(*e)[extraPrefix+key], _ = json.Marshal(rest) // a map of raw JSON that was just read
}
