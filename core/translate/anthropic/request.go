package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"

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

func decodeRequest(body []byte) (pivot.Request, error) {
	m, err := object(body)
	if err != nil {
		return pivot.Request{}, err
	}
	var req pivot.Request
	var system, messages, tools json.RawMessage
	var choice *struct {
		Type            string `json:"type"`
		Name            string `json:"name"`
		DisableParallel bool   `json:"disable_parallel_tool_use"`
	}
	var thinking *struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	}
	err = errors.Join(
		take(m, "model", &req.Model),
		take(m, "system", &system),
		take(m, "messages", &messages),
		take(m, "tools", &tools),
		take(m, "tool_choice", &choice),
		take(m, "max_tokens", &req.MaxTokens),
		take(m, "stop_sequences", &req.Stop),
		take(m, "temperature", &req.Temperature),
		take(m, "top_p", &req.TopP),
		take(m, "stream", &req.Stream),
		take(m, "thinking", &thinking),
		takeEffort(m, &req.Effort),
	)
	if err != nil {
		return pivot.Request{}, err
	}
	if choice != nil {
		req.ToolChoice = &pivot.ToolChoice{Type: choice.Type, Name: choice.Name, DisableParallel: choice.DisableParallel}
	}
	if thinking != nil {
		req.Thinking = &pivot.Thinking{Type: thinking.Type, BudgetTokens: thinking.BudgetTokens}
	}
	if req.System, err = decodeContent(system); err != nil {
		return pivot.Request{}, fmt.Errorf("system: %w", err)
	}
	if req.Messages, err = decodeMessages(messages); err != nil {
		return pivot.Request{}, err
	}
	if req.Tools, err = decodeTools(tools); err != nil {
		return pivot.Request{}, err
	}
	req.Extra = extra(m)
	return req, nil
}

// takeEffort moves output_config.effort to effort. The other members of
// output_config stay in m, so they go to Extra as anthropic.output_config.
func takeEffort(m map[string]json.RawMessage, effort *string) error {
	var cfg map[string]json.RawMessage
	if err := take(m, "output_config", &cfg); err != nil || cfg == nil {
		return err
	}
	if err := take(cfg, "effort", effort); err != nil {
		return fmt.Errorf("output_config.%w", err)
	}
	if len(cfg) > 0 {
		m["output_config"], _ = json.Marshal(cfg) // a map of raw JSON that was just parsed
	}
	return nil
}

func decodeMessages(raw json.RawMessage) ([]pivot.Message, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(orNull(raw), &items); err != nil {
		return nil, fmt.Errorf("messages: %w", err)
	}
	var out []pivot.Message
	for i, item := range items {
		m, err := object(item)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		var msg pivot.Message
		var content json.RawMessage
		if err := errors.Join(take(m, "role", &msg.Role), take(m, "content", &content)); err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		if msg.Content, err = decodeContent(content); err != nil {
			return nil, fmt.Errorf("messages[%d].content: %w", i, err)
		}
		msg.Extra = extra(m)
		out = append(out, msg)
	}
	return out, nil
}

// decodeContent reads content that is a string (one text block) or a list
// of blocks.
func decodeContent(raw json.RawMessage) ([]pivot.Block, error) {
	if isNull(raw) {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []pivot.Block{{Kind: pivot.BlockText, Text: s}}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errors.New("must be a string or a list of blocks")
	}
	out := make([]pivot.Block, 0, len(items))
	for i, item := range items {
		b, err := decodeBlock(item)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		out = append(out, b)
	}
	return out, nil
}

func decodeBlock(raw json.RawMessage) (pivot.Block, error) {
	m, err := object(raw)
	if err != nil {
		return pivot.Block{}, err
	}
	var b pivot.Block
	var kind string
	if err := errors.Join(take(m, "type", &kind), takeRaw(m, "cache_control", &b.CacheControl)); err != nil {
		return pivot.Block{}, err
	}
	if kind == "" {
		return pivot.Block{}, errors.New("a block has no type")
	}
	b.Kind = pivot.BlockKind(kind)
	switch b.Kind {
	case pivot.BlockText:
		err = take(m, "text", &b.Text)
	case pivot.BlockImage, pivot.BlockDocument:
		err = takeSource(m, &b)
	case pivot.BlockToolUse:
		err = errors.Join(take(m, "id", &b.ID), take(m, "name", &b.Name), takeRaw(m, "input", &b.Input))
	case pivot.BlockToolResult:
		var content json.RawMessage
		err = errors.Join(take(m, "tool_use_id", &b.ToolUseID), take(m, "content", &content), take(m, "is_error", &b.IsError))
		if err == nil {
			b.Content, err = decodeContent(content)
		}
	case pivot.BlockThinking:
		err = errors.Join(take(m, "thinking", &b.Text), take(m, "signature", &b.Signature))
	case pivot.BlockRedactedThinking:
		err = take(m, "data", &b.Data)
	}
	if err != nil {
		return pivot.Block{}, fmt.Errorf("%s block: %w", kind, err)
	}
	b.Extra = extra(m)
	return b, nil
}

// takeSource reads the source of an image or a document. The members of
// source that the pivot has no place for stay in m as source.
func takeSource(m map[string]json.RawMessage, b *pivot.Block) error {
	var src map[string]json.RawMessage
	if err := take(m, "source", &src); err != nil || src == nil {
		return err
	}
	if err := errors.Join(
		take(src, "type", &b.SourceType),
		take(src, "media_type", &b.MediaType),
		take(src, "data", &b.Data),
		take(src, "url", &b.URL),
	); err != nil {
		return fmt.Errorf("source.%w", err)
	}
	if len(src) > 0 {
		m["source"], _ = json.Marshal(src) // a map of raw JSON that was just parsed
	}
	return nil
}

func decodeTools(raw json.RawMessage) ([]pivot.Tool, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(orNull(raw), &items); err != nil {
		return nil, fmt.Errorf("tools: %w", err)
	}
	var out []pivot.Tool
	for i, item := range items {
		m, err := object(item)
		if err != nil {
			return nil, fmt.Errorf("tools[%d]: %w", i, err)
		}
		var t pivot.Tool
		if err := errors.Join(
			take(m, "name", &t.Name),
			take(m, "description", &t.Description),
			takeRaw(m, "input_schema", &t.InputSchema),
			take(m, "type", &t.Type),
			takeRaw(m, "cache_control", &t.CacheControl),
		); err != nil {
			return nil, fmt.Errorf("tools[%d]: %w", i, err)
		}
		if t.Type == "custom" { // Anthropic's name for a function tool
			t.Type = ""
		}
		t.Extra = extra(m)
		out = append(out, t)
	}
	return out, nil
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

// extra gives the members left in m as Extra keys, or nil when none.
func extra(m map[string]json.RawMessage) pivot.Extra {
	if len(m) == 0 {
		return nil
	}
	e := make(pivot.Extra, len(m))
	for k, v := range m {
		e[extraPrefix+k] = v
	}
	return e
}

func isNull(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }

func orNull(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}
