package responses

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

func refuse(msg string) error {
	return &pivot.RequestError{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: msg}
}

// The refusals of rules R5, R6, R7 and R10, with the exact text of #1964.
var (
	errState          = refuse("Cortex cannot translate a Responses request that refers to server state (previous_response_id). Send the full conversation.")
	errTextFormat     = refuse("Cortex cannot translate structured output (text.format) yet.")
	errFiles          = refuse("this inference server cannot read files")
	errCompacted      = refuse("This Codex session was compacted by OpenAI. Cortex cannot send it to another server. Start a new Codex session.")
	errCompactTrigger = refuse("Cortex cannot compact a Codex conversation on this server. Start a new Codex session, or move this agent to an OpenAI server with agentop server use.")
)

// present is the Extra value of a dropped item, part or tool member.
var present = json.RawMessage("true")

// DecodeRequest converts a Responses request body to the pivot form. Each
// field and item that the pivot has no place for goes to the request's
// Extra under "responses.<name>", so that the target encoder names it in
// dropped. A body that is not a valid request gives a *pivot.RequestError
// with status 400.
func (Protocol) DecodeRequest(body []byte) (pivot.Request, error) {
	d := decoder{origin: &origin{tools: map[string]toolRef{}}, droppedCalls: map[string]bool{}}
	if err := d.request(body); err != nil {
		var re *pivot.RequestError
		if errors.As(err, &re) {
			return pivot.Request{}, err
		}
		return pivot.Request{}, refuse("the request body is not a valid Responses request: " + err.Error())
	}
	d.req.Origin = d.origin
	return d.req, nil
}

// decoder holds the state of one pass over a request.
type decoder struct {
	req          pivot.Request
	origin       *origin
	droppedCalls map[string]bool // call_ids of dropped calls (rule R7)
	started      bool            // a user, assistant, call or output item came
	afterOutput  bool            // the last item was a tool output
}

// drop records name in Extra, for the dropped list.
func (d *decoder) drop(name string) { d.dropRaw(name, present) }

func (d *decoder) dropRaw(name string, v json.RawMessage) {
	if d.req.Extra == nil {
		d.req.Extra = pivot.Extra{}
	}
	d.req.Extra[extraPrefix+name] = v
}

func (d *decoder) request(body []byte) error {
	m, items, err := members(body)
	if err != nil {
		return err
	}
	var instructions, prev string
	var input, tools, choice, conversation json.RawMessage
	var parallel *bool
	var text, reasoning map[string]json.RawMessage
	r := &d.req
	err = errors.Join(
		take(m, "model", &r.Model),
		take(m, "instructions", &instructions),
		takeRaw(m, "input", &input),
		takeRaw(m, "tools", &tools),
		takeRaw(m, "tool_choice", &choice),
		take(m, "parallel_tool_calls", &parallel),
		take(m, "max_output_tokens", &r.MaxTokens),
		take(m, "temperature", &r.Temperature),
		take(m, "top_p", &r.TopP),
		take(m, "stream", &r.Stream),
		take(m, "previous_response_id", &prev),
		takeRaw(m, "conversation", &conversation),
		take(m, "text", &text),
		take(m, "reasoning", &reasoning),
	)
	if err != nil {
		return err
	}
	// Rule R5.
	if prev != "" || !isNull(conversation) && string(conversation) != `""` {
		return errState
	}
	if err := d.text(text); err != nil {
		return err
	}
	if err := take(reasoning, "effort", &r.Effort); err != nil {
		return fmt.Errorf("reasoning.%w", err)
	}
	for k, v := range reasoning {
		d.dropRaw("reasoning."+k, v)
	}
	for k, v := range m {
		d.dropRaw(k, v)
	}
	if instructions != "" {
		r.System = []pivot.Block{{Kind: pivot.BlockText, Text: instructions}}
	}
	if err := d.tools(tools, ""); err != nil {
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
	return d.input(input, items)
}

// members reads the members of the request object. An input list goes
// straight to items and not to m: then the history is scanned two times,
// not four (rule R15).
func members(body []byte) (map[string]json.RawMessage, []item, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, nil, errors.New("must be a JSON object")
	}
	m := map[string]json.RawMessage{}
	var items []item
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, _ := t.(string) // a member name, as the object has no other tokens here
		if key == "input" && bytes.HasPrefix(bytes.TrimLeft(body[dec.InputOffset():], " \t\r\n:"), []byte("[")) {
			delete(m, key)
			items = nil
			err = dec.Decode(&items)
		} else {
			var raw json.RawMessage
			err = dec.Decode(&raw)
			m[key] = raw
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
	return m, items, nil
}

// text refuses structured output, and records the other members of text
// (rule R6).
func (d *decoder) text(text map[string]json.RawMessage) error {
	var format struct {
		Type string `json:"type"`
	}
	if err := take(text, "format", &format); err != nil {
		return fmt.Errorf("text.%w", err)
	}
	if format.Type != "" && format.Type != "text" {
		return errTextFormat
	}
	for k, v := range text {
		d.dropRaw("text."+k, v)
	}
	return nil
}

// toolChoice converts tool_choice by rule R6. Call it after tools.
func (d *decoder) toolChoice(raw json.RawMessage) error {
	if isNull(raw) {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		t := map[string]string{"auto": "auto", "none": "none", "required": "any"}[s]
		if t == "" {
			d.drop("tool_choice")
			return nil
		}
		d.req.ToolChoice = &pivot.ToolChoice{Type: t}
		return nil
	}
	var o struct{ Type, Name, Namespace string }
	if err := json.Unmarshal(raw, &o); err != nil {
		return fmt.Errorf("tool_choice: %w", err)
	}
	if o.Type != "function" && o.Type != "custom" {
		d.drop("tool_choice")
		return nil
	}
	d.req.ToolChoice = &pivot.ToolChoice{Type: "tool", Name: flatName(o.Namespace, o.Name)}
	return nil
}

// item is one input item. The members that differ in type between item
// types are any, and are read only for the item types that use them. Not
// json.RawMessage: each raw layer scans its bytes again, and the tool
// outputs hold most of a request (rule R15).
type item struct {
	Type             string          `json:"type"`
	Role             string          `json:"role"`
	Content          any             `json:"content"`
	CallID           string          `json:"call_id"`
	Name             string          `json:"name"`
	Namespace        string          `json:"namespace"`
	Arguments        any             `json:"arguments"`
	Input            any             `json:"input"`
	Output           any             `json:"output"`
	EncryptedContent any             `json:"encrypted_content"`
	Tools            json.RawMessage `json:"tools"`
}

// input converts input by rules R6 and R7, in one pass over the items.
// raw is set when input is not a list.
func (d *decoder) input(raw json.RawMessage, items []item) error {
	if !isNull(raw) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("input: %w", err)
		}
		d.add("user", false, pivot.Block{Kind: pivot.BlockText, Text: s})
		return nil
	}
	for i := range items {
		if err := d.item(&items[i]); err != nil {
			var re *pivot.RequestError
			if errors.As(err, &re) {
				return err
			}
			return fmt.Errorf("input[%d]: %w", i, err)
		}
	}
	return nil
}

func (d *decoder) item(it *item) error {
	switch it.Type {
	case "message", "":
		return d.message(it)
	case "reasoning":
		enc, err := str(it.EncryptedContent, "encrypted_content")
		if err != nil {
			return err
		}
		b, ok := decodeReasoning(enc)
		if !ok {
			d.drop("reasoning")
			return nil
		}
		d.add("assistant", true, b)
	case "function_call":
		args, err := str(it.Arguments, "arguments")
		if err != nil {
			return err
		}
		name := flatName(it.Namespace, it.Name)
		var input json.RawMessage
		if strings.TrimSpace(args) != "" {
			if !json.Valid([]byte(args)) {
				return refuse(fmt.Sprintf("the arguments of a call to the tool %q are not valid JSON", name))
			}
			input = json.RawMessage(args)
		}
		d.add("assistant", true, pivot.Block{Kind: pivot.BlockToolUse, ID: it.CallID, Name: name, Input: input})
	case "custom_tool_call":
		s, err := str(it.Input, "input")
		if err != nil {
			return err
		}
		// The value holds only a string, so marshal cannot fail.
		input, _ := marshal(struct {
			Input string `json:"input"`
		}{s})
		d.add("assistant", true, pivot.Block{Kind: pivot.BlockToolUse, ID: it.CallID, Name: flatName(it.Namespace, it.Name), Input: input})
	case "function_call_output", "custom_tool_call_output":
		if d.droppedCalls[it.CallID] {
			return nil
		}
		content, err := d.output(it.Output)
		if err != nil {
			return err
		}
		d.add("user", true, pivot.Block{Kind: pivot.BlockToolResult, ToolUseID: it.CallID, Content: content})
		d.afterOutput = true
	case "additional_tools":
		return d.tools(it.Tools, "")
	case "compaction_trigger":
		return errCompactTrigger
	case "compaction", "context_compaction":
		return errCompacted
	default:
		// web_search_call, local_shell_call and every type that a Chat or
		// Anthropic server cannot carry. Its output goes too.
		d.drop(it.Type)
		if it.CallID != "" {
			d.droppedCalls[it.CallID] = true
		}
	}
	return nil
}

// message converts a message item by rule R7.
func (d *decoder) message(it *item) error {
	blocks, err := d.content(it.Content)
	if err != nil {
		return err
	}
	if len(blocks) == 0 {
		return nil
	}
	switch it.Role {
	case "user":
		d.add("user", d.afterOutput, blocks...)
	case "assistant":
		d.add("assistant", true, blocks...)
	case "developer", "system":
		if !d.started {
			d.req.System = append(d.req.System, blocks...)
			return nil
		}
		d.add("system", false, blocks...)
	default:
		return fmt.Errorf("a message has the role %q", it.Role)
	}
	return nil
}

// add appends blocks to the last message when join is true and it has
// this role. Otherwise it starts a new message.
func (d *decoder) add(role string, join bool, blocks ...pivot.Block) {
	d.started, d.afterOutput = true, false
	msgs := d.req.Messages
	if n := len(msgs); join && n > 0 && msgs[n-1].Role == role {
		msgs[n-1].Content = append(msgs[n-1].Content, blocks...)
		return
	}
	d.req.Messages = append(msgs, pivot.Message{Role: role, Content: blocks})
}

// content converts message content: a string (decision 8) or a list of
// parts.
func (d *decoder) content(v any) ([]pivot.Block, error) {
	switch v := v.(type) {
	case nil:
		return nil, nil
	case string:
		return []pivot.Block{{Kind: pivot.BlockText, Text: v}}, nil
	case []any:
		out := make([]pivot.Block, 0, len(v))
		for _, p := range v {
			b, ok, err := d.part(p)
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

// part converts one content part. false when the part is dropped.
func (d *decoder) part(v any) (pivot.Block, bool, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return pivot.Block{}, false, errors.New("a content part must be an object")
	}
	kind, err := str(m["type"], "type")
	if err != nil {
		return pivot.Block{}, false, err
	}
	switch kind {
	case "input_text", "output_text":
		text, err := str(m["text"], "text")
		return pivot.Block{Kind: pivot.BlockText, Text: text}, err == nil, err
	case "input_image":
		b, err := image(m)
		return b, err == nil, err
	case "input_file":
		return pivot.Block{}, false, errFiles
	}
	d.drop(kind) // refusal, and parts with no pivot form
	return pivot.Block{}, false, nil
}

// output converts a tool output: a string, or a list of parts. An empty
// string gives no blocks (decision 10).
func (d *decoder) output(v any) ([]pivot.Block, error) {
	if v == "" {
		return nil, nil
	}
	return d.content(v)
}

// image converts an input_image part by rule R7.
func image(m map[string]any) (pivot.Block, error) {
	u, err := str(m["image_url"], "image_url")
	if err != nil {
		return pivot.Block{}, err
	}
	if m["file_id"] != nil || u == "" {
		return pivot.Block{}, errFiles
	}
	if rest, ok := strings.CutPrefix(u, "data:"); ok {
		meta, data, ok := strings.Cut(rest, ",")
		media, b64 := strings.CutSuffix(meta, ";base64")
		if !ok || !b64 {
			return pivot.Block{}, refuse("an input_image data URL must be base64")
		}
		return pivot.Block{Kind: pivot.BlockImage, SourceType: "base64", MediaType: media, Data: data}, nil
	}
	if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		return pivot.Block{Kind: pivot.BlockImage, SourceType: "url", URL: u}, nil
	}
	return pivot.Block{}, refuse("an input_image must have a data URL or an http(s) URL")
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

// object parses a JSON object into its members.
func object(raw []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("must be a JSON object")
	}
	return m, nil
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

// takeRaw moves the member key of m to dst as raw JSON. A null member
// gives nil.
func takeRaw(m map[string]json.RawMessage, key string, dst *json.RawMessage) error {
	raw, ok := m[key]
	delete(m, key)
	if ok && !isNull(raw) {
		*dst = raw
	}
	return nil
}

func isNull(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }

func orNull(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}
