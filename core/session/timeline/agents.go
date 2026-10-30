package timeline

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/pipeline"
)

// progressPrompt is the instruction Claude Code appends when it asks a model for a subagent's
// 3-5 word status line. Those calls carry the subagent's whole context and tool list, so only
// their last user message tells them apart from the subagent's real turns; Claude Code can send
// system messages after it (a <total_tokens> count). They are a third of all subagent calls on
// real sessions, and drawing them would bury the turns that matter.
const progressPrompt = "Describe your most recent action in 3-5 words"

// recapPrompt asks for the summary Claude Code shows when the user comes back to an idle
// session. The same shape as a progress label: whole context and tool list, told apart only by
// its last user message. Drawn as a turn it was the agent working while the user was away, and
// two recaps of an unchanged conversation read as a retry: 74 hours of it on one session.
const recapPrompt = "The user stepped away and is coming back"

// The two ways a background subagent's result reaches the agent that started it. Both name the
// subagent by the id Claude Code sends as X-Claude-Code-Agent-Id, the id the proxy records as
// SessionEvent.Subagent, so a return links to its row without any text matching.
var (
	agentMessageRe = regexp.MustCompile(`<agent-message from="([A-Za-z0-9_-]+)"`)
	taskIDRe       = regexp.MustCompile(`<task-notification>\s*<task-id>([A-Za-z0-9_-]+)</task-id>`)
	spawnedIDRe    = regexp.MustCompile(`agentId: ([A-Za-z0-9_-]+)`)
	// backgroundRe is Claude Code's result for a shell command it started in the background;
	// the command's <task-notification> later names the same id.
	backgroundRe = regexp.MustCompile(`^Command running in background with ID: ([A-Za-z0-9_-]+)`)
)

// failedFinish are the finish reasons of a reply that did not complete: none at all, a refusal
// (Anthropic "refusal", OpenAI chat "content_filter"), and a Responses API status of "failed" or
// "cancelled".
var failedFinish = map[string]bool{"": true, "refusal": true, "content_filter": true, "failed": true, "cancelled": true}

// previewMax bounds a tool's preview: enough to tell two Bash calls apart in a one-line strip.
const previewMax = 80

// call is one inference request and, once seen, its response.
type call struct {
	req, resp *pipeline.SessionEvent
	agent     string
}

// spawn is an Agent tool call, waiting to be matched to the subagent it started.
type spawn struct {
	tool    *Tool
	by      string // agent that made the call
	turnAt  time.Time
	desc    string
	typ     string
	childID string // from the tool's result ("agentId: …"), when the result names it
	matched bool
}

// toolArgs are the arguments a preview or an agent label can come from. Decoding into a struct
// skips everything else, so a Write call's file content costs a scan, not an allocation.
type toolArgs struct {
	Command      string `json:"command"`
	FilePath     string `json:"file_path"`
	Path         string `json:"path"`
	Pattern      string `json:"pattern"`
	URL          string `json:"url"`
	Query        string `json:"query"`
	Skill        string `json:"skill"`
	Description  string `json:"description"`
	SubagentType string `json:"subagent_type"`
	To           string `json:"to"`
}

func buildAgents(t *Timeline, events []pipeline.SessionEvent) {
	calls := pairCalls(events)
	if len(calls) == 0 {
		return
	}

	type agentState struct {
		first time.Time
		prev  []pipeline.InferenceMessage
	}
	agents := map[string]*agentState{}
	jobs := map[string]int{} // background job id → index in t.Jobs
	var order []string
	ends := map[string]map[string]*pipeline.InferenceToolResult{} // agent → tool id → first result
	endAt := map[string]map[string]time.Time{}
	results := map[string]string{} // tool id → result text, for Agent calls only
	agentTools := map[string]bool{}
	seenReturn := map[string]bool{}
	var spawns []*spawn

	for _, c := range calls {
		inf := c.req.Inference
		st, known := agents[c.agent]
		if !known {
			st = &agentState{first: c.req.At}
			agents[c.agent] = st
			order = append(order, c.agent)
		}

		// Tool ends: the first request in this agent that carries a tool's result.
		for i := range inf.ToolResults {
			r := &inf.ToolResults[i]
			if ends[c.agent] == nil {
				ends[c.agent], endAt[c.agent] = map[string]*pipeline.InferenceToolResult{}, map[string]time.Time{}
			}
			if _, seen := ends[c.agent][r.ToolUseID]; !seen {
				ends[c.agent][r.ToolUseID], endAt[c.agent][r.ToolUseID] = r, c.req.At
				if agentTools[r.ToolUseID] {
					results[r.ToolUseID] = r.Content
				}
				if m := backgroundRe.FindStringSubmatch(r.Content); m != nil {
					if _, dup := jobs[m[1]]; !dup {
						jobs[m[1]] = len(t.Jobs)
						t.Jobs = append(t.Jobs, Job{Agent: c.agent, ID: m[1], Start: c.req.At, Seq: c.req.Seq})
					}
				}
			}
		}

		// Returns: scan only the messages this agent has not sent before. A conversation
		// re-sends its whole history every turn, so scanning all of it is quadratic in turns
		// (0.26-0.47GB of text on real sessions against 35-42MB new).
		//
		// Not for pre-#1459 subagent traffic: every subagent shares that one bucket, so each
		// request's history differs from the last one's and every scan would be a full one.
		msgs := inf.Messages
		k := commonPrefix(st.prev, msgs)
		// A call that sends its predecessor's conversation unchanged added nothing: a retry.
		resent := known && len(msgs) > 0 && k == len(msgs) && k == len(st.prev) && c.agent != UnseparatedAgent
		if c.agent == UnseparatedAgent {
			k = len(msgs)
		}
		for mi, m := range msgs[k:] {
			if m.Role == "assistant" {
				continue // an agent quoting the markers is not a delivery
			}
			t.scanned += len(m.Content)
			for _, re := range []*regexp.Regexp{agentMessageRe, taskIDRe} {
				for _, idx := range re.FindAllStringSubmatchIndex(m.Content, -1) {
					from := m.Content[idx[2]:idx[3]]
					tail := m.Content[idx[0]:min(len(m.Content), idx[1]+200)]
					// Keyed by the message's position in the history as well as its text: a
					// fork re-sends its parent's deliveries at the same positions, while a
					// second, identically worded hand-back lands at a new one.
					key := fmt.Sprintf("%s\x00%d\x00%s", from, k+mi, tail)
					if seenReturn[key] {
						continue // a fork inherits its parent's history, deliveries included
					}
					seenReturn[key] = true
					t.Returns = append(t.Returns, Return{Agent: from, Into: c.agent, At: c.req.At, Seq: c.req.Seq})
					// A background job ends at the first notification naming it.
					if j, ok := jobs[from]; ok && t.Jobs[j].End == nil {
						at := c.req.At
						t.Jobs[j].End = &at
					}
				}
			}
		}
		st.prev = msgs

		turn := Turn{Agent: c.agent, Start: c.req.At, Resent: resent, Seq: c.req.Seq, Model: inf.Model}
		if c.resp == nil {
			t.Turns = append(t.Turns, turn)
			continue
		}
		end := responseEnd(c.req, c.resp)
		turn.End = &end
		if c.resp.Inference != nil {
			turn.Finish = c.resp.Inference.FinishReason
		}
		// No finish reason is a reply that never finished: the proxy records a stream cut
		// mid-reply as the 200 it started as. A refusal is a call that failed too.
		turn.Error = c.resp.Phase == pipeline.SessionDenied || c.resp.Error != nil || c.resp.StatusCode >= 400 ||
			failedFinish[turn.Finish]
		t.Turns = append(t.Turns, turn)
		if c.resp.Inference == nil {
			continue
		}
		for _, tc := range c.resp.Inference.ToolCalls {
			var a toolArgs
			_ = json.Unmarshal([]byte(tc.Arguments), &a)
			tool := Tool{Agent: c.agent, ID: tc.ID, Name: tc.Name, Start: end, Preview: preview(tc.Name, a), Seq: c.resp.Seq}
			t.Tools = append(t.Tools, tool)
			if tc.Name == "Agent" || tc.Name == "Task" {
				agentTools[tc.ID] = true
				spawns = append(spawns, &spawn{by: c.agent, turnAt: c.req.At, desc: a.Description, typ: a.SubagentType})
			}
		}
	}

	// An unanswered turn followed by another turn of the same agent is a stream that died.
	lastTurn := map[string]time.Time{}
	for i := len(t.Turns) - 1; i >= 0; i-- {
		tu := &t.Turns[i]
		if tu.End == nil && lastTurn[tu.Agent].After(tu.Start) {
			at := tu.Start // a copy: Clip moves spans, and &tu.Start would then point at a neighbour
			tu.End, tu.Lost = &at, true
		}
		if lastTurn[tu.Agent].Before(tu.Start) {
			lastTurn[tu.Agent] = tu.Start
		}
	}

	// Close each tool on its agent's first request carrying its result. Claude Code runs a
	// tool while the response that asked for it is still streaming, so that request can be
	// recorded before the response finished: clamp rather than draw a negative span.
	si := 0
	for i := range t.Tools {
		tool := &t.Tools[i]
		if at, ok := endAt[tool.Agent][tool.ID]; ok {
			if at.Before(tool.Start) {
				at = tool.Start
			}
			tool.End = &at
			tool.Error = ends[tool.Agent][tool.ID].IsError
		} else if tool.Name == "SubagentHandback" || lastTurn[tool.Agent].After(tool.Start) {
			// The agent moved on without the result, or the tool ends the run by design.
			at := tool.Start
			tool.End, tool.Lost = &at, true
		}
		if tool.Name == "Agent" || tool.Name == "Task" {
			sp := spawns[si]
			si++
			sp.tool = tool
			if m := spawnedIDRe.FindStringSubmatch(results[tool.ID]); m != nil {
				sp.childID = m[1]
			}
		}
	}

	first := make(map[string]time.Time, len(agents))
	for id, st := range agents {
		first[id] = st.first
	}
	t.Agents = linkAgents(order, first, calls, spawns, t.Returns)

	// A marker can name an agent the session never recorded: one from another session quoted
	// into this one, or a subagent whose every request was a side call.
	known := make(map[string]bool, len(t.Agents))
	for _, a := range t.Agents {
		known[a.ID] = true
	}
	t.Returns = slices.DeleteFunc(t.Returns, func(r Return) bool { return !known[r.Agent] || r.Agent == r.Into })
}

// linkAgents builds the agent tree. A subagent is linked to the Agent call that started it by
// the id the call's result names; a fork's result names none ("Fork started"), so a fork takes
// the earliest unmatched call from its parent whose turn began before the fork's first request.
// Matching on the turn's start, not the tool's, is deliberate: the fork's first request often
// precedes the end of the response that spawned it.
func linkAgents(order []string, first map[string]time.Time, calls []call, spawns []*spawn, returns []Return) []Agent {
	parentHint := map[string]string{}
	for _, c := range calls {
		if c.req.ParentSubagent != "" && parentHint[c.agent] == "" {
			parentHint[c.agent] = c.req.ParentSubagent
		}
	}
	byChild := map[string]*spawn{}
	for _, sp := range spawns {
		if sp.childID != "" && byChild[sp.childID] == nil {
			byChild[sp.childID], sp.matched = sp, true
		}
	}

	out := make([]Agent, 0, len(order))
	for _, id := range order {
		a := Agent{ID: id, Label: id}
		switch id {
		case MainAgent:
			a.Label = "main agent"
			out = append(out, a)
			continue
		case UnseparatedAgent:
			a.Label, a.Parent = "subagents (not separated)", MainAgent
			out = append(out, a)
			continue
		}
		parent := parentHint[id]
		if parent == "" {
			parent = MainAgent
		}
		sp := byChild[id]
		if sp == nil {
			for _, cand := range spawns {
				if !cand.matched && cand.by == parent && !cand.turnAt.After(first[id]) {
					sp, cand.matched = cand, true
					break
				}
			}
		}
		a.Label = "subagent " + shortID(id)
		if sp != nil {
			parent = sp.by
			a.SpawnTool, a.Type = sp.tool.ID, sp.typ
			if sp.desc != "" {
				a.Label = sp.desc
			}
			at := sp.tool.Start
			if f := first[id]; f.Before(at) {
				at = f
			}
			a.SpawnedAt = &at
		}
		a.Parent = parent
		// The LAST return: a parent can resume a subagent with SendMessage after its first
		// hand-back, and the row should run until the final one.
		for _, r := range returns {
			if r.Agent == id && r.Into == parent {
				at := r.At
				a.ReturnedAt = &at
			}
		}
		out = append(out, a)
	}
	return out
}

// pairCalls returns the session's Claude Code inference calls in request order, each with the
// agent that made it. Progress-label calls and tool-less side calls (titler, classifier,
// security monitor) are dropped: they are not an agent's work, and on a live session they were
// more than half of the main role's requests.
func pairCalls(events []pipeline.SessionEvent) []call {
	var calls []call
	byReq := map[string]int{}
	for i := range events {
		e := &events[i]
		if e.Direction != pipeline.Outbound {
			continue
		}
		switch e.Phase {
		case pipeline.SessionRequest:
			if e.Inference == nil {
				continue
			}
			id, ok := agentOf(e)
			if !ok || isSideCall(e.Inference) {
				continue
			}
			byReq[e.RequestID] = len(calls)
			calls = append(calls, call{req: e, agent: id})
		case pipeline.SessionResponse, pipeline.SessionDenied:
			if j, ok := byReq[e.RequestID]; ok && calls[j].resp == nil {
				calls[j].resp = e
			}
		}
	}
	return calls
}

// agentOf names the agent that sent a request. Claude Code's requests carry their own
// markers: AgentRole from its cc_is_subagent header, Subagent from its agent id header.
// Every other known client gets the single main row — nothing on the wire says which of
// its calls belong together beyond that.
func agentOf(e *pipeline.SessionEvent) (string, bool) {
	switch {
	case e.Subagent != "":
		return e.Subagent, true
	case e.Inference.AgentRole == pipeline.AgentRoleSubagent:
		return UnseparatedAgent, true
	case e.Inference.AgentRole == pipeline.AgentRoleMain:
		return MainAgent, true
	case e.Client != nil && pipeline.IsKnownAgent(e.Client.Name):
		return MainAgent, true
	}
	return "", false
}

// isSideCall drops a call that is not an agent's work. The no-tools rule is Claude Code only
// (AgentRole set): it holds because Claude always sends a manifest on real turns, while another
// client's tool-less calls (Bob's security classifier) are real turns with real cost. The prompt
// literals run for every client: they match Claude's own progress and recap prompt text, which
// no other client sends, and a recap storm reads as turns on any session that carries one —
// including pre-marker Claude Code traffic, attributed by client name alone.
func isSideCall(inf *pipeline.InferenceExtension) bool {
	if inf.AgentRole != "" && len(inf.Tools) == 0 && inf.ToolCount == 0 {
		return true
	}
	for i := len(inf.Messages) - 1; i >= 0; i-- {
		if m := inf.Messages[i]; m.Role != "system" {
			return strings.Contains(m.Content, progressPrompt) || strings.Contains(m.Content, recapPrompt)
		}
	}
	return false
}

// commonPrefix is how many leading messages cur shares with prev. The store interns repeated
// message content, so equal strings usually share a backing array and compare in O(1).
func commonPrefix(prev, cur []pipeline.InferenceMessage) int {
	k := 0
	for k < len(prev) && k < len(cur) && prev[k].Role == cur[k].Role && prev[k].Content == cur[k].Content {
		k++
	}
	return k
}

func responseEnd(req, resp *pipeline.SessionEvent) time.Time {
	if resp.Duration > 0 {
		return req.At.Add(resp.Duration)
	}
	return resp.At
}

func preview(name string, a toolArgs) string {
	var s string
	switch {
	case a.Command != "":
		s = a.Command
	case a.FilePath != "":
		s = a.FilePath
	case a.Pattern != "":
		s = a.Pattern
	case a.URL != "":
		s = a.URL
	case a.Query != "":
		s = a.Query
	case a.Skill != "":
		s = a.Skill
	case a.Description != "":
		s = a.Description
	case a.Path != "":
		s = a.Path
	case a.To != "":
		s = "to " + a.To
	}
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > previewMax {
		r := []rune(s)
		s = string(r[:previewMax-1]) + "…"
	}
	return s
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
