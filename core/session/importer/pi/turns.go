package pi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/parse/inference"
	"github.com/rossoctl/cortex/core/project"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/importer"
)

// messagesPath is the path the live parser reads an Anthropic Messages body for.
const messagesPath = "/v1/messages"

// The text of Pi's synthetic user messages, from Pi 1.4.0 dist/core/messages.js.
const (
	compactionPrefix = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n"
	compactionSuffix = "\n</summary>"
	branchPrefix     = "The following is a summary of a branch that this conversation came back from:\n\n<summary>\n"
	branchSuffix     = "</summary>" // Pi's branch suffix has no newline before </summary>
)

// entry is the part of one Pi session line that the import reads.
type entry struct {
	Type      string    `json:"type"`
	ID        string    `json:"id"`
	ParentID  string    `json:"parentId"`
	Timestamp time.Time `json:"timestamp"`
	Message   *message  `json:"message"`
	// compaction and branch_summary
	Summary          string   `json:"summary"`
	FirstKeptEntryID string   `json:"firstKeptEntryId"`
	SystemMessage    *message `json:"systemMessage"` // compaction: the system message at the time of the compaction
	// context_edit: a JSON null Replacement is "null" and removes the target.
	TargetID    string          `json:"targetId"`
	Replacement json.RawMessage `json:"replacement"`
	// custom_message
	Content json.RawMessage `json:"content"`
}

type message struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	Sections     sections        `json:"sections"`
	ToolsAdded   []piTool        `json:"toolsAdded"`
	ToolsRemoved []piTool        `json:"toolsRemoved"`
	Model        string          `json:"model"`
	Usage        usage           `json:"usage"`
	StopReason   string          `json:"stopReason"`
	ResponseID   string          `json:"responseId"`
	Timestamp    int64           `json:"timestamp"` // ms since the epoch: the request start
	ToolCallID   string          `json:"toolCallId"`
	IsError      bool            `json:"isError"`
}

// sections are the named parts of a Pi system prompt, in the key order of the JSON object.
// A nil value is a JSON null: the section is removed.
type sections []struct {
	name  string
	value *string
}

func (s *sections) UnmarshalJSON(b []byte) error {
	*s = nil
	d := json.NewDecoder(bytes.NewReader(b))
	tok, err := d.Token()
	if err != nil || tok == nil { // null
		return err
	}
	if tok != json.Delim('{') {
		return fmt.Errorf("sections: want an object, got %v", tok)
	}
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return err
		}
		var v *string
		if err := d.Decode(&v); err != nil {
			return err
		}
		*s = append(*s, struct {
			name  string
			value *string
		}{k.(string), v})
	}
	return nil
}

// ordered is a map that keeps the order of first insertion, as a JavaScript Map does.
type ordered[V any] struct {
	keys []string
	vals map[string]V
}

func (o *ordered[V]) set(k string, v V) {
	if o.vals == nil {
		o.vals = map[string]V{}
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *ordered[V]) del(k string) {
	if _, ok := o.vals[k]; ok {
		delete(o.vals, k)
		o.keys = slices.DeleteFunc(o.keys, func(x string) bool { return x == k })
	}
}

type piTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
}

// convMessage is one Anthropic Messages message.
type convMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// parser holds what the sessions of one import share.
type parser struct {
	home     string
	projects map[string]string // cwd -> project label
}

// session parses one Pi session file.
func (p *parser) session(id, path string) importer.Session {
	b, err := os.ReadFile(path)
	if err != nil {
		return unreadable(id, path, err)
	}
	s := importer.Session{ID: id, SizeHint: int64(len(b))}
	var (
		hdr     *header
		entries []*entry
		byID    = map[string]*entry{}
	)
	r := bufio.NewReader(bytes.NewReader(b))
	for {
		raw, rerr := r.ReadBytes('\n')
		if raw = bytes.TrimSpace(raw); len(raw) > 0 {
			var e entry
			switch {
			case json.Unmarshal(raw, &e) != nil:
				s.Skipped++
			case hdr == nil:
				// headerID already checked that the first parsed line is the header.
				hdr = &header{}
				_ = json.Unmarshal(raw, hdr)
			case e.Type == "message" && e.Message == nil:
				s.Skipped++
			default:
				entries = append(entries, &e)
				byID[e.ID] = &e
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return unreadable(id, path, rerr)
			}
			break
		}
	}
	if hdr == nil {
		return unreadable(id, path, errNoHeader)
	}

	var in session.Interner // one per session: each request holds the whole conversation
	proj := p.project(hdr.Cwd)
	client := func() *model.EventClient { return &model.EventClient{Name: agentName} }
	for _, e := range entries {
		if e.Type != "message" || e.Message.Role != "assistant" {
			continue
		}
		m := e.Message
		reqBody, _ := json.Marshal(request(m.Model, buildContext(byID, e.ParentID, len(entries))))
		req := inference.ParseRequest(messagesPath, nil, reqBody)
		if req == nil {
			s.Skipped++
			continue
		}
		req.AgentRole = model.AgentRoleMain
		resp := *req
		inference.ReadResponse(messagesPath, false, responseBody(m), &resp)

		rid := m.ResponseID
		if rid == "" {
			// Entry ids are 8 hex digits and can repeat across sessions.
			rid = id + "/" + e.ID
		}
		respAt := e.Timestamp
		reqAt := respAt
		if m.Timestamp > 0 {
			if t := time.UnixMilli(m.Timestamp).UTC(); !t.After(respAt) {
				reqAt = t
			}
		}
		s.Events = append(s.Events,
			model.SessionEvent{At: reqAt, Direction: model.Outbound, Phase: model.SessionRequest, RequestID: rid,
				Inference: req, Client: client(), Project: proj},
			model.SessionEvent{At: respAt, Direction: model.Outbound, Phase: model.SessionResponse, RequestID: rid,
				Inference: &resp, Client: client(), Project: proj})
		for i := len(s.Events) - 2; i < len(s.Events); i++ {
			in.InternEvent(&s.Events[i])
		}
	}
	// Stable: on a tie a request stays before its response.
	slices.SortStableFunc(s.Events, func(a, b model.SessionEvent) int { return a.At.Compare(b.At) })
	for _, e := range s.Events {
		if e.At.After(s.Updated) {
			s.Updated = e.At
		}
	}
	return s
}

// buildContext gives the entries that Pi sends as the context of leaf, by buildSessionContext
// (Pi 1.4.0 session-manager.js): the path from the root to leaf, cut at the last compaction.
func buildContext(byID map[string]*entry, leaf string, n int) []*entry {
	var path []*entry
	for id := leaf; id != "" && len(path) <= n; { // the length check stops a parentId loop
		e := byID[id]
		if e == nil {
			break
		}
		path = append(path, e)
		id = e.ParentID
	}
	slices.Reverse(path)
	ci := -1
	for i, e := range path {
		if e.Type == "compaction" {
			ci = i
		}
	}
	if ci < 0 {
		return path
	}
	c := path[ci]
	out := []*entry{c}
	kept := false
	for _, e := range path[:ci] {
		kept = kept || e.ID == c.FirstKeptEntryID
		if kept && (e.Type != "message" || e.Message.Role != "system") {
			out = append(out, e)
		}
	}
	return append(out, path[ci+1:]...)
}

// request makes the Anthropic Messages request body of the context entries.
func request(modelName string, ctx []*entry) any {
	edits := map[string]json.RawMessage{}
	for _, e := range ctx {
		if e.Type == "context_edit" {
			edits[e.TargetID] = e.Replacement
		}
	}
	var (
		sysText []string
		secs    ordered[string]
		tools   ordered[piTool]
		msgs    = []convMessage{}
		results []json.RawMessage
	)
	add := func(role string, content json.RawMessage) {
		if len(results) > 0 {
			// Consecutive tool results are one user message, as Anthropic wants them.
			msgs = append(msgs, convMessage{Role: "user", Content: marshal(results)})
			results = nil
		}
		if content != nil {
			msgs = append(msgs, convMessage{Role: role, Content: content})
		}
	}
	// addSystem replays one system message, as getCurrentSystemMessage (Pi 1.4.0 pi-ai
	// utils/transcript.js) does for a provider with no mid-conversation system messages.
	addSystem := func(m *message) {
		if t := contentText(m.Content); t != "" {
			sysText = append(sysText, t)
		}
		for _, sec := range m.Sections {
			if sec.value == nil {
				secs.del(sec.name)
			} else {
				secs.set(sec.name, *sec.value)
			}
		}
		for _, t := range m.ToolsRemoved {
			tools.del(t.Name)
		}
		for _, t := range m.ToolsAdded {
			tools.set(t.Name, t)
		}
	}
	for i, e := range ctx {
		switch e.Type {
		case "message":
			m := e.Message
			content := m.Content
			if r, ok := edits[e.ID]; ok {
				var rep *struct {
					Content json.RawMessage `json:"content"`
				}
				if json.Unmarshal(r, &rep) != nil || rep == nil {
					continue // replacement null: Pi removes the entry's messages
				}
				content = rep.Content
			}
			switch m.Role {
			case "system":
				addSystem(m)
			case "user":
				if isString(content) {
					add("user", content)
				} else {
					add("user", marshal(blocks(content)))
				}
			case "assistant":
				add("assistant", marshal(blocks(content)))
			case "toolResult":
				results = append(results, marshal(map[string]any{
					"type": "tool_result", "tool_use_id": m.ToolCallID, "content": blocks(content), "is_error": m.IsError,
				}))
			}
			// Other roles (bashExecution, custom, ...) have a text form internal to Pi: left out.
		case "custom_message":
			add("user", marshal(blocks(e.Content)))
		case "branch_summary":
			if e.Summary != "" {
				add("user", textBlocks(branchPrefix+e.Summary+branchSuffix))
			}
		case "compaction":
			if i > 0 {
				continue // an older compaction in the kept range: Pi gives it no messages
			}
			if e.SystemMessage != nil {
				addSystem(e.SystemMessage)
			}
			add("user", textBlocks(compactionPrefix+e.Summary+compactionSuffix))
		}
	}
	add("", nil)
	// The text of the system prompt, as getSystemMessageText (pi-ai utils/text.js) renders it.
	parts := []string{strings.Join(sysText, "\n\n")}
	for _, k := range secs.keys {
		parts = append(parts, secs.vals[k])
	}
	system := strings.Join(slices.DeleteFunc(parts, func(p string) bool { return p == "" }), "\n\n")
	var atools []anthropicTool
	for _, k := range tools.keys {
		t := tools.vals[k]
		atools = append(atools, anthropicTool{Name: t.Name, Description: t.Description, InputSchema: t.Parameters})
	}
	return struct {
		Model    string          `json:"model"`
		System   string          `json:"system,omitempty"`
		Tools    []anthropicTool `json:"tools,omitempty"`
		Messages []convMessage   `json:"messages"`
	}{modelName, system, atools, msgs}
}

var stopReasons = map[string]string{"stop": "end_turn", "toolUse": "tool_use", "length": "max_tokens"}

// responseBody makes the Anthropic Messages response body of one assistant message.
func responseBody(m *message) []byte {
	stop := m.StopReason
	if s, ok := stopReasons[stop]; ok {
		stop = s
	}
	var u any
	if m.Usage != (usage{}) {
		u = map[string]int{
			"input_tokens": m.Usage.Input, "output_tokens": m.Usage.Output,
			"cache_read_input_tokens": m.Usage.CacheRead, "cache_creation_input_tokens": m.Usage.CacheWrite,
		}
	}
	return marshal(struct {
		Model      string            `json:"model"`
		Content    []json.RawMessage `json:"content"`
		StopReason string            `json:"stop_reason,omitempty"`
		Usage      any               `json:"usage,omitempty"`
	}{m.Model, blocks(m.Content), stop, u})
}

// blocks converts Pi content (a string, or an array of Pi blocks) to Anthropic blocks.
func blocks(content json.RawMessage) []json.RawMessage {
	if isString(content) {
		var s string
		_ = json.Unmarshal(content, &s)
		return []json.RawMessage{textBlock(s)}
	}
	var in []json.RawMessage
	_ = json.Unmarshal(content, &in)
	out := make([]json.RawMessage, 0, len(in))
	for _, raw := range in {
		var b struct {
			Type              string          `json:"type"`
			Text              string          `json:"text"`
			Thinking          string          `json:"thinking"`
			ThinkingSignature string          `json:"thinkingSignature"`
			ID                string          `json:"id"`
			Name              string          `json:"name"`
			Arguments         json.RawMessage `json:"arguments"`
			Data              string          `json:"data"`
			MimeType          string          `json:"mimeType"`
		}
		if json.Unmarshal(raw, &b) != nil {
			continue
		}
		switch b.Type {
		case "text":
			out = append(out, textBlock(b.Text))
		case "thinking":
			out = append(out, marshal(map[string]any{"type": "thinking", "thinking": b.Thinking, "signature": b.ThinkingSignature}))
		case "toolCall":
			args := b.Arguments
			if len(args) == 0 || string(args) == "null" {
				args = json.RawMessage("{}")
			}
			out = append(out, marshal(map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": args}))
		case "image":
			out = append(out, marshal(map[string]any{"type": "image",
				"source": map[string]string{"type": "base64", "media_type": b.MimeType, "data": b.Data}}))
		default:
			out = append(out, raw)
		}
	}
	return out
}

// contentText is the text of Pi content, as contentText (pi-ai utils/text.js) gives it.
func contentText(raw json.RawMessage) string {
	if isString(raw) {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var bs []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &bs)
	var texts []string
	for _, b := range bs {
		if b.Type == "text" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func textBlock(s string) json.RawMessage {
	return marshal(map[string]string{"type": "text", "text": s})
}

func textBlocks(s string) json.RawMessage { return marshal([]json.RawMessage{textBlock(s)}) }

func isString(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '"'
}

func marshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (p *parser) project(cwd string) string {
	if cwd == "" {
		return ""
	}
	label, ok := p.projects[cwd]
	if !ok {
		label = project.Label(project.Root(cwd, p.home), p.home)
		p.projects[cwd] = label
	}
	return label
}
