package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/parse/inference"
	"github.com/rossoctl/cortex/core/project"
	"github.com/rossoctl/cortex/core/session"
)

// messagesPath is the path the live parser reads an Anthropic Messages body for.
const messagesPath = "/v1/messages"

// line is the part of one transcript line that the import reads.
type line struct {
	Type      string    `json:"type"`
	Subtype   string    `json:"subtype"`
	Timestamp time.Time `json:"timestamp"`
	RequestID string    `json:"requestId"`
	Version   string    `json:"version"`
	Cwd       string    `json:"cwd"`
	Message   *struct {
		ID         string          `json:"id"`
		Model      string          `json:"model"`
		Content    json.RawMessage `json:"content"`
		StopReason json.RawMessage `json:"stop_reason"`
		Usage      json.RawMessage `json:"usage"`
	} `json:"message"`
}

// convMessage is one message of the conversation, as Claude Code sends it.
type convMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// fileResult is what one transcript file gave.
type fileResult struct {
	events  []model.SessionEvent
	skipped int
	size    int64
}

// parser holds what the files of one session share.
type parser struct {
	home     string
	projects map[string]string // cwd -> project label
	// in interns each event when emit makes it. Each event holds the whole conversation,
	// so an event kept un-interned until the merge makes the parse O(turns^2) in memory.
	// File order is the order inside each agent, and the tables are per conversation, so
	// the merge after the parse does not change what is shared.
	in session.Interner
}

// turn is one assistant message: the assistant lines with the same message.id, with only user lines between them.
type turn struct {
	blocks  []json.RawMessage
	last    *line
	firstAt time.Time
}

// file parses one transcript. subagent is "" for the main transcript.
func (p *parser) file(path, subagent string) (fileResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return fileResult{}, err
	}
	defer f.Close()
	var res fileResult
	if fi, err := f.Stat(); err == nil {
		res.size = fi.Size()
	}
	role := model.AgentRoleMain
	if subagent != "" {
		role = model.AgentRoleSubagent
	}

	conv := []convMessage{}
	var userAt, prevAt time.Time // the last user line, and the previous turn's response
	var cur *turn
	// held keeps the user lines read while a turn is open. Claude Code writes the
	// tool_result of a parallel tool call before the next line of the same message.
	var held []*line
	emit := func(t *turn) {
		content, _ := json.Marshal(t.blocks)
		reqAt := userAt
		if reqAt.IsZero() {
			reqAt = t.firstAt
		}
		// A turn with no user line since the previous one must not sort before that turn's response.
		if reqAt.Before(prevAt) {
			reqAt = prevAt
		}
		reqBody, _ := json.Marshal(struct {
			Model    string        `json:"model"`
			Messages []convMessage `json:"messages"`
		}{t.last.Message.Model, conv})
		req := inference.ParseRequest(messagesPath, nil, reqBody)
		if req == nil {
			res.skipped++
			return
		}
		req.AgentRole = role
		respBody, _ := json.Marshal(struct {
			Model      string          `json:"model"`
			Content    json.RawMessage `json:"content"`
			StopReason json.RawMessage `json:"stop_reason,omitempty"`
			Usage      json.RawMessage `json:"usage,omitempty"`
		}{t.last.Message.Model, content, t.last.Message.StopReason, t.last.Message.Usage})
		// The response event carries the request's extension with the response read into it,
		// as a live response event does (SnapshotInference copies the request's extension).
		resp := *req
		inference.ReadResponse(messagesPath, false, respBody, &resp)

		rid := t.last.RequestID
		if rid == "" {
			rid = t.last.Message.ID
		}
		client := func() *model.EventClient { return &model.EventClient{Name: agentName, Version: t.last.Version} }
		proj := p.project(t.last.Cwd)
		res.events = append(res.events,
			model.SessionEvent{At: reqAt, Direction: model.Outbound, Phase: model.SessionRequest, RequestID: rid,
				Inference: req, Client: client(), Project: proj, Subagent: subagent},
			model.SessionEvent{At: t.last.Timestamp, Direction: model.Outbound, Phase: model.SessionResponse, RequestID: rid,
				Inference: &resp, Client: client(), Project: proj, Subagent: subagent})
		for i := len(res.events) - 2; i < len(res.events); i++ {
			p.in.InternEvent(&res.events[i])
		}
		prevAt = t.last.Timestamp
		conv = append(conv, convMessage{Role: "assistant", Content: content})
	}
	flush := func() {
		if cur == nil {
			return
		}
		emit(cur)
		cur = nil
		for _, u := range held {
			conv = append(conv, convMessage{Role: "user", Content: u.Message.Content})
			userAt = u.Timestamp
		}
		held = nil
	}

	r := bufio.NewReader(f)
	for {
		raw, rerr := r.ReadBytes('\n')
		if raw = bytes.TrimSpace(raw); len(raw) > 0 {
			var l line
			switch {
			case json.Unmarshal(raw, &l) != nil:
				res.skipped++
			case l.Type == "system" && l.Subtype == "compact_boundary":
				flush()
				conv = []convMessage{}
			case l.Type != "user" && l.Type != "assistant":
				// summary, file-history-snapshot, other system lines: not part of the conversation.
			case l.Message == nil:
				res.skipped++
			case l.Type == "user" && cur != nil:
				held = append(held, &l)
			case l.Type == "user":
				conv = append(conv, convMessage{Role: "user", Content: l.Message.Content})
				userAt = l.Timestamp
			default:
				var blocks []json.RawMessage
				if json.Unmarshal(l.Message.Content, &blocks) != nil {
					res.skipped++
					break
				}
				// Claude Code writes one assistant message as one line per content block.
				if cur == nil || l.Message.ID == "" || l.Message.ID != cur.last.Message.ID {
					flush()
					cur = &turn{firstAt: l.Timestamp}
				}
				cur.blocks = append(cur.blocks, blocks...)
				cur.last = &l
			}
		}
		if rerr != nil {
			flush()
			if errors.Is(rerr, io.EOF) {
				return res, nil
			}
			return res, rerr
		}
	}
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
