package bob

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/parse/inference"
	"github.com/rossoctl/cortex/core/project"
	"github.com/rossoctl/cortex/core/session"
)

// chatPath is Bob's live inference path: the OpenAI chat-completions dialect.
const chatPath = "/inference/v1/chat/completions"

// row is one row of the messages query.
type row struct {
	ID        string `json:"id"`
	Role      string `json:"role"`
	Data      string `json:"data"`
	CreatedAt int64  `json:"created_at"`
}

// data is the part of messages.data that the import reads.
type data struct {
	Content        string   `json:"content"`
	AvailableTools []string `json:"availableTools"`
	ToolCalls      []struct {
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"toolCalls"`
	Reasoning      string `json:"reasoning"`
	ThinkingBlocks []struct {
		Type      string `json:"type"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
	} `json:"thinkingBlocks"`
	Meta struct {
		StopReason string `json:"stopReason"`
		Spend      *struct {
			Cost          float64 `json:"cost"`
			ContextTokens int     `json:"contextTokens"`
		} `json:"spend"`
	} `json:"_meta"`
}

// chatMessage is one OpenAI chat-completions message.
type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

// parser holds what the tasks of one session share.
type parser struct {
	home     string
	rootDir  string
	projects map[string]string // dir -> project label
	// in interns each event when it is made, as in the Claude source: each event holds the
	// whole conversation.
	in session.Interner
}

// task makes the request and response events of the rows of one task.
func (p *parser) task(t *task, root bool, rows []row) ([]model.SessionEvent, int) {
	var events []model.SessionEvent
	var skipped int
	role, subagent, kind := model.AgentRoleMain, "", ""
	if !root {
		role, subagent, kind = model.AgentRoleSubagent, t.ID, t.TaskType
	}
	dir := t.Dir
	if dir == "" {
		dir = p.rootDir
	}
	proj := p.project(dir)
	version := ""
	if t.Version != nil {
		version = *t.Version
	}

	conv := []chatMessage{}
	var tools []chatTool
	var pending []string // the tool-call ids of the last assistant message
	next := 0
	var inputAt, prevAt time.Time // the newest non-assistant row since the last assistant row, and that row's time
	for _, r := range rows {
		var d data
		if !strings.HasPrefix(strings.TrimSpace(r.Data), "{") || json.Unmarshal([]byte(r.Data), &d) != nil {
			skipped++
			continue
		}
		at := time.UnixMilli(r.CreatedAt)
		switch r.Role {
		case "system", "user":
			conv = append(conv, chatMessage{Role: r.Role, Content: d.Content})
			if r.Role == "user" {
				tools = tools[:0]
				for _, name := range d.AvailableTools {
					var ct chatTool
					ct.Type, ct.Function.Name = "function", name
					tools = append(tools, ct)
				}
			}
			inputAt = at
		case "tool":
			m := chatMessage{Role: "tool", Content: d.Content}
			if next < len(pending) {
				m.ToolCallID = pending[next]
			}
			next++
			conv = append(conv, m)
			inputAt = at
		case "assistant":
			reqAt := inputAt
			if reqAt.IsZero() {
				reqAt = at
			}
			if reqAt.Before(prevAt) {
				reqAt = prevAt
			}
			calls := make([]toolCall, 0, len(d.ToolCalls))
			for _, c := range d.ToolCalls {
				var tc toolCall
				tc.ID, tc.Type, tc.Function.Name, tc.Function.Arguments = c.ID, "function", c.Name, argText(c.Arguments)
				calls = append(calls, tc)
			}
			msg := chatMessage{Role: "assistant", Content: d.Content, ToolCalls: calls}
			if req, resp, plugins := turn(t.Model, conv, tools, msg, &d); req != nil {
				client := func() *model.EventClient { return &model.EventClient{Name: agentName, Version: version} }
				req.AgentRole, resp.AgentRole = role, role
				events = append(events,
					model.SessionEvent{At: reqAt, Direction: model.Outbound, Phase: model.SessionRequest, RequestID: r.ID,
						Inference: req, Client: client(), Project: proj, Subagent: subagent, SubagentKind: kind},
					model.SessionEvent{At: at, Direction: model.Outbound, Phase: model.SessionResponse, RequestID: r.ID,
						Inference: resp, Plugins: plugins, Client: client(), Project: proj, Subagent: subagent, SubagentKind: kind})
				for i := len(events) - 2; i < len(events); i++ {
					p.in.InternEvent(&events[i])
				}
			} else {
				skipped++
			}
			conv = append(conv, msg)
			pending, next = pending[:0], 0
			for _, c := range calls {
				pending = append(pending, c.ID)
			}
			inputAt, prevAt = time.Time{}, at
		default:
			skipped++
		}
	}
	return events, skipped
}

// turn builds the request and response bodies of one assistant message and parses them as
// the live parser does. plugins holds Bob's cost record. req is nil when the parser does not
// claim the request.
func turn(modelID string, conv []chatMessage, tools []chatTool, msg chatMessage, d *data) (req, resp *model.InferenceExtension, plugins map[string]json.RawMessage) {
	reqBody, _ := json.Marshal(struct {
		Model    string        `json:"model"`
		Messages []chatMessage `json:"messages"`
		Tools    []chatTool    `json:"tools,omitempty"`
	}{modelID, conv, tools})
	if req = inference.ParseRequest(chatPath, nil, reqBody); req == nil {
		return nil, nil, nil
	}
	type usage struct {
		TotalTokens int `json:"total_tokens"`
	}
	var u *usage
	if d.Meta.Spend != nil {
		u = &usage{d.Meta.Spend.ContextTokens}
	}
	type choice struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason,omitempty"`
	}
	respBody, _ := json.Marshal(struct {
		Model   string   `json:"model"`
		Choices []choice `json:"choices"`
		Usage   *usage   `json:"usage,omitempty"`
	}{modelID, []choice{{msg, d.Meta.StopReason}}, u})
	// The response event carries the request's extension with the response read into it,
	// as a live response event does.
	r := *req
	resp = &r
	inference.ReadResponse(chatPath, false, respBody, resp)
	if len(d.ThinkingBlocks) > 0 || d.Reasoning != "" {
		th := &model.InferenceThinking{}
		for _, b := range d.ThinkingBlocks {
			th.Add(b.Type, len(b.Thinking), len(b.Signature), 0)
		}
		if len(d.ThinkingBlocks) == 0 {
			th.Add("thinking", len(d.Reasoning), 0, 0)
		}
		resp.ResponseThinking = th
	}
	if d.Meta.Spend != nil {
		// Bob's own figure for the turn, also when it is 0 (a free turn). importer.Run does not price it again.
		b, err := json.Marshal(event.Event{CostUSD: d.Meta.Spend.Cost, Source: event.SourceTranscript,
			Provenance: pricing.ProvAuthoritative.String(), Settled: true})
		if err != nil {
			slog.Warn("importer: Bob cost record", "error", err)
		} else {
			plugins = map[string]json.RawMessage{event.Key: b, event.PluginName: b}
		}
	}
	return req, resp, plugins
}

// argText gives tool-call arguments as the JSON string the OpenAI wire carries: the text of
// a JSON object, or the value of a JSON string.
func argText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
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
