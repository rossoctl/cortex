package chat

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// request is the chat-completions body. It has only the top-level fields
// of rule 11, in that order. Do not add a field that is not in rule 11.
type request struct {
	Model             string          `json:"model"`
	Messages          []message       `json:"messages"`
	MaxTokens         int             `json:"max_tokens,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	StreamOptions     *streamOptions  `json:"stream_options,omitempty"`
	Tools             []tool          `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Stop              []string        `json:"stop,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	ReasoningEffort   string          `json:"reasoning_effort,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// message is one chat message. Content is a []part for system, user and
// tool messages (rule 9a), and a string or nil for assistant messages.
type message struct {
	Role           string          `json:"role"`
	Content        any             `json:"content,omitempty"`
	ThinkingBlocks []thinkingBlock `json:"thinking_blocks,omitempty"`
	ToolCalls      []toolCall      `json:"tool_calls,omitempty"`
	ToolCallID     string          `json:"tool_call_id,omitempty"`
}

type part struct {
	Type         string          `json:"type"`
	Text         *string         `json:"text,omitempty"`
	ImageURL     *imageURL       `json:"image_url,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

// thinkingBlock is LiteLLM's thinking_blocks entry (rule 15).
type thinkingBlock struct {
	Type      string  `json:"type"`
	Thinking  *string `json:"thinking,omitempty"`
	Signature string  `json:"signature,omitempty"`
	Data      string  `json:"data,omitempty"`
}

// toolCall has no index field: Bob answers 422 to it (rule 15).
type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type tool struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

func refuse(msg string) error {
	return &pivot.RequestError{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: msg}
}

var errDocument = refuse("this inference server cannot read documents (PDF)")

// EncodeRequest converts a pivot request to a chat-completions body
// (rules 11-19). It removes the Anthropic headers from h. dropped is the
// sorted list of the fields that it did not send.
func (Protocol) EncodeRequest(req pivot.Request, h http.Header) ([]byte, []string, error) {
	h.Del("Anthropic-Version")
	h.Del("Anthropic-Beta")

	e := encoder{dropped: map[string]bool{}}
	e.extra(req.Extra)
	out := request{
		Model:       req.Model,
		Messages:    []message{},
		MaxTokens:   req.MaxTokens,
		Stream:      req.Stream,
		Stop:        req.Stop,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}
	if req.Stream {
		out.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	if len(req.System) > 0 {
		out.Messages = append(out.Messages, message{Role: "system", Content: e.textParts(req.System)})
	}
	for _, m := range req.Messages {
		e.extra(m.Extra)
		var msgs []message
		var err error
		if m.Role == "assistant" {
			msgs = []message{e.assistant(m.Content)}
		} else {
			// Rule 13a: a system message after the first message is
			// sent as a user message.
			msgs, err = e.user(m.Content)
		}
		if err != nil {
			return nil, nil, err
		}
		out.Messages = append(out.Messages, msgs...)
	}
	for _, t := range req.Tools {
		if t.Type != "" {
			return nil, nil, refuse(`this inference server cannot run the server tool "` + t.Name + `" (type ` + t.Type + `)`)
		}
		// Rule 16: no cache_control on a tool.
		out.Tools = append(out.Tools, tool{Type: "function", Function: toolFunction{Name: t.Name, Description: t.Description, Parameters: t.InputSchema}})
	}
	if c := req.ToolChoice; c != nil {
		out.ToolChoice = toolChoice(*c)
		if c.DisableParallel {
			f := false
			out.ParallelToolCalls = &f
		}
	}
	out.ReasoningEffort = effort(req)

	body, err := marshal(out)
	if err != nil {
		return nil, nil, refuse("the request has a field that is not valid JSON: " + err.Error())
	}
	dropped := make([]string, 0, len(e.dropped))
	for k := range e.dropped {
		dropped = append(dropped, k)
	}
	sort.Strings(dropped)
	return body, dropped, nil
}

// encoder collects the names of the dropped fields (decision 11).
type encoder struct {
	dropped map[string]bool
}

// extra names each field of another protocol, without its prefix.
func (e *encoder) extra(x pivot.Extra) {
	for k := range x {
		proto, field, ok := strings.Cut(k, ".")
		if ok && proto != "chat-completions" {
			e.dropped[field] = true
		}
	}
}

func (e *encoder) textParts(blocks []pivot.Block) []part {
	parts := make([]part, 0, len(blocks))
	for _, b := range blocks {
		e.extra(b.Extra)
		parts = append(parts, textPart(b.Text, b.CacheControl))
	}
	return parts
}

func textPart(text string, cc json.RawMessage) part {
	return part{Type: "text", Text: &text, CacheControl: cc}
}

func imagePart(b pivot.Block) part {
	url := b.URL
	if b.SourceType == "base64" {
		url = "data:" + b.MediaType + ";base64," + b.Data
	}
	return part{Type: "image_url", ImageURL: &imageURL{URL: url}, CacheControl: b.CacheControl}
}

// user converts a user (or system) turn by rule 14: the tool messages
// first, then a user message with the images of the tool results, then a
// user message with the other content.
func (e *encoder) user(blocks []pivot.Block) ([]message, error) {
	var msgs []message
	var toolImages, rest []part
	for _, b := range blocks {
		e.extra(b.Extra)
		switch b.Kind {
		case pivot.BlockText:
			rest = append(rest, textPart(b.Text, b.CacheControl))
		case pivot.BlockImage:
			rest = append(rest, imagePart(b))
		case pivot.BlockDocument:
			return nil, errDocument
		case pivot.BlockToolResult:
			if b.IsError {
				e.dropped["is_error"] = true
			}
			var text []part
			for _, c := range b.Content {
				e.extra(c.Extra)
				switch c.Kind {
				case pivot.BlockText:
					text = append(text, textPart(c.Text, nil))
				case pivot.BlockImage:
					toolImages = append(toolImages, imagePart(c))
				case pivot.BlockDocument:
					return nil, errDocument
				}
			}
			if len(text) == 0 {
				// One empty part keeps the content a list and gives the
				// cache marker a place.
				text = []part{textPart("", nil)}
			}
			text[len(text)-1].CacheControl = b.CacheControl
			msgs = append(msgs, message{Role: "tool", Content: text, ToolCallID: b.ToolUseID})
		}
	}
	if len(toolImages) > 0 {
		msgs = append(msgs, message{Role: "user", Content: toolImages})
	}
	if len(rest) > 0 {
		msgs = append(msgs, message{Role: "user", Content: rest})
	}
	return msgs, nil
}

// assistant converts an assistant turn by rule 15 and decisions 9-10.
func (e *encoder) assistant(blocks []pivot.Block) message {
	m := message{Role: "assistant"}
	var text strings.Builder
	for _, b := range blocks {
		e.extra(b.Extra)
		switch b.Kind {
		case pivot.BlockText:
			text.WriteString(b.Text)
		case pivot.BlockToolUse:
			args := string(b.Input)
			if args == "" {
				args = "{}"
			}
			// Rule 20a: the id goes back byte-exact.
			m.ToolCalls = append(m.ToolCalls, toolCall{ID: b.ID, Type: "function", Function: functionCall{Name: b.Name, Arguments: args}})
		case pivot.BlockThinking:
			if b.Signature == "" {
				e.dropped["thinking"] = true
				continue
			}
			t := b.Text
			m.ThinkingBlocks = append(m.ThinkingBlocks, thinkingBlock{Type: "thinking", Thinking: &t, Signature: b.Signature})
		case pivot.BlockRedactedThinking:
			m.ThinkingBlocks = append(m.ThinkingBlocks, thinkingBlock{Type: "redacted_thinking", Data: b.Data})
		}
	}
	if text.Len() > 0 {
		m.Content = text.String()
	}
	return m
}

// toolChoice converts a tool choice by rule 17.
func toolChoice(c pivot.ToolChoice) json.RawMessage {
	switch c.Type {
	case "auto":
		return json.RawMessage(`"auto"`)
	case "any":
		return json.RawMessage(`"required"`)
	case "none":
		return json.RawMessage(`"none"`)
	case "tool":
		b, _ := marshal(struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}{Type: "function", Function: struct {
			Name string `json:"name"`
		}{c.Name}})
		return b
	}
	return nil
}

// effort gives reasoning_effort by rule 19.
func effort(req pivot.Request) string {
	switch req.Effort {
	case "low", "medium", "high":
		return req.Effort
	case "":
	default:
		return "high"
	}
	if req.Thinking != nil && (req.Thinking.Type == "enabled" || req.Thinking.Type == "adaptive") {
		return "medium"
	}
	return ""
}
