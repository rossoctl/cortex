package opencode

import (
	"cmp"
	"encoding/json"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/parse/inference"
	"github.com/rossoctl/cortex/core/project"
	"github.com/rossoctl/cortex/core/session"
)

// messagesPath is the Anthropic Messages path. OpenCode's token counts have the Anthropic
// meaning, and importer.Run prices the response at this path.
const messagesPath = "/v1/messages"

// message is one row of the messages query.
type message struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	TimeCreated int64  `json:"time_created"`
	TimeUpdated int64  `json:"time_updated"`
	Data        string `json:"data"`
}

// data is the part of session_message.data that the import reads.
type data struct {
	Text  string `json:"text"` // a user row
	Model struct {
		ID string `json:"id"`
	} `json:"model"`
	Content   []part          `json:"content"`
	Finish    string          `json:"finish"`
	RawFinish string          `json:"rawFinish"`
	Error     json.RawMessage `json:"error"`
	Tokens    *struct {
		Input     int `json:"input"`
		Output    int `json:"output"`
		Reasoning int `json:"reasoning"`
		Cache     struct {
			Read  int `json:"read"`
			Write int `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Time struct {
		Completed int64 `json:"completed"`
	} `json:"time"`
}

// part is one content part of an assistant row.
type part struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	State struct {
		Status  string          `json:"status"`
		Input   json.RawMessage `json:"input"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"state"`
}

// convMessage is one Anthropic Messages message.
type convMessage struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

// block is one Anthropic content block.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  *string         `json:"thinking,omitempty"`
	Signature *string         `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   *string         `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// parser holds what the session rows of one group share.
type parser struct {
	home     string
	projects map[string]string // directory -> project label
	// in interns each event when it is made: each event holds the whole conversation.
	in session.Interner
}

// session makes the request and response events of the messages of one session row.
func (p *parser) session(r *sessionRow, msgs []message) ([]model.SessionEvent, int) {
	role, subagent := model.AgentRoleMain, ""
	if r.ParentID != nil && *r.ParentID != "" {
		role, subagent = model.AgentRoleSubagent, r.ID
	}
	proj := p.project(r.Directory)
	fork := r.ForkID != nil && *r.ForkID != ""
	var events []model.SessionEvent
	var skipped int
	conv := []convMessage{}
	for _, m := range msgs {
		var d data
		if !strings.HasPrefix(strings.TrimSpace(m.Data), "{") || json.Unmarshal([]byte(m.Data), &d) != nil {
			skipped++
			continue
		}
		switch m.Type {
		case "user":
			conv = append(conv, convMessage{Role: "user", Content: []block{{Type: "text", Text: d.Text}}})
		case "idle", "model-switched", "system":
			// OpenCode's own rows: no model completion.
		case "assistant":
			if len(d.Error) > 0 && string(d.Error) != "null" && len(d.Content) == 0 {
				continue // a failed call: no completion
			}
			resp, said, results := blocks(d.Content)
			// A fork's rows from before the fork are copies of its source's turns.
			if m.TimeCreated == 0 || m.TimeUpdated == 0 {
				skipped++ // a time that is not a number
			} else if !fork || m.TimeCreated >= r.TimeCreated {
				if req, out := turn(d, conv, resp); req == nil {
					skipped++
				} else {
					req.AgentRole, out.AgentRole = role, role
					respAt := m.TimeCreated
					if d.Time.Completed > 0 {
						respAt = d.Time.Completed
					}
					respAt = min(respAt, m.TimeUpdated)
					client := func() *model.EventClient { return &model.EventClient{Name: agentName, Version: r.Version} }
					events = append(events,
						model.SessionEvent{At: time.UnixMilli(m.TimeCreated), Direction: model.Outbound, Phase: model.SessionRequest,
							RequestID: m.ID, Inference: req, Client: client(), Project: proj, Subagent: subagent},
						model.SessionEvent{At: time.UnixMilli(respAt), Direction: model.Outbound, Phase: model.SessionResponse,
							RequestID: m.ID, Inference: out, Client: client(), Project: proj, Subagent: subagent})
					for i := len(events) - 2; i < len(events); i++ {
						p.in.InternEvent(&events[i])
					}
				}
			}
			conv = append(conv, convMessage{Role: "assistant", Content: said})
			if len(results) > 0 {
				conv = append(conv, convMessage{Role: "user", Content: results})
			}
		default:
			skipped++
		}
	}
	return events, skipped
}

// blocks gives the response content of the parts, the content the assistant message has in
// the conversation (no thinking), and the tool results.
func blocks(parts []part) (resp, said, results []block) {
	resp, said = []block{}, []block{}
	for _, pt := range parts {
		switch pt.Type {
		case "reasoning":
			sig := ""
			resp = append(resp, block{Type: "thinking", Thinking: &pt.Text, Signature: &sig})
		case "text":
			b := block{Type: "text", Text: pt.Text}
			resp, said = append(resp, b), append(said, b)
		case "tool":
			in := pt.State.Input
			if len(in) == 0 || string(in) == "null" {
				in = json.RawMessage("{}")
			}
			b := block{Type: "tool_use", ID: pt.ID, Name: pt.Name, Input: in}
			resp, said = append(resp, b), append(said, b)
			switch pt.State.Status {
			case "completed":
				texts := make([]string, 0, len(pt.State.Content))
				for _, c := range pt.State.Content {
					texts = append(texts, c.Text)
				}
				out := strings.Join(texts, "\n")
				results = append(results, block{Type: "tool_result", ToolUseID: pt.ID, Content: &out})
			case "error":
				out := pt.State.Error.Message
				results = append(results, block{Type: "tool_result", ToolUseID: pt.ID, Content: &out, IsError: true})
			}
		}
	}
	return resp, said, results
}

// turn builds the request and response bodies of one assistant row and parses them as the
// live parser does. req is nil when the parser does not claim the request.
func turn(d data, conv []convMessage, content []block) (req, resp *model.InferenceExtension) {
	reqBody, _ := json.Marshal(struct {
		Model    string        `json:"model"`
		Messages []convMessage `json:"messages"`
	}{d.Model.ID, conv})
	if req = inference.ParseRequest(messagesPath, nil, reqBody); req == nil {
		return nil, nil
	}
	var u *usage
	if t := d.Tokens; t != nil {
		// OpenCode bills reasoning at the output rate.
		u = &usage{t.Input, t.Output + t.Reasoning, t.Cache.Read, t.Cache.Write}
	}
	stop := cmp.Or(d.RawFinish, d.Finish)
	respBody, _ := json.Marshal(struct {
		Model      string  `json:"model"`
		Content    []block `json:"content"`
		StopReason string  `json:"stop_reason,omitempty"`
		Usage      *usage  `json:"usage,omitempty"`
	}{d.Model.ID, content, stop, u})
	// The response event carries the request's extension with the response read into it,
	// as a live response event does.
	r := *req
	inference.ReadResponse(messagesPath, false, respBody, &r)
	return req, &r
}

func (p *parser) project(dir string) string {
	if dir == "" {
		return ""
	}
	label, ok := p.projects[dir]
	if !ok {
		label = project.Label(project.Root(dir, p.home), p.home)
		p.projects[dir] = label
	}
	return label
}
