package timeline

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session/toolpair"
)

// spawnedIDRe reads the subagent id out of an Agent call's result ("agentId: …"), which is how
// a spawned subagent is linked to the call that started it.
//
// THE ONLY MARKER PATTERN LEFT HERE. The agent-message, task-notification, background-job and
// TaskStop patterns moved to core/session/toolpair with the scan that uses them. They are NOT
// duplicated back: Go does not report an unused package-level var, so a copy here would compile,
// pass vet, read as live beside this one — and silently do nothing when someone widened it,
// because the pattern that runs is in the other package. Same for the progress and recap prompt
// literals, which are matched against live traffic and change when Claude Code rewords them.
var spawnedIDRe = regexp.MustCompile(`agentId: ([A-Za-z0-9_-]+)`)

// failedFinish are the finish reasons of a reply that did not complete: none at all, a refusal
// (Anthropic "refusal", OpenAI chat "content_filter"), and a Responses API status of "failed" or
// "cancelled".
var failedFinish = map[string]bool{"": true, "refusal": true, "content_filter": true, "failed": true, "cancelled": true}

// previewMax bounds a tool's preview: enough to tell two Bash calls apart in a one-line strip.
const previewMax = 80

// call is one inference request and, once seen, its response.
type call struct {
	req, resp *model.SessionEvent
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

// lostFor maps a toolpair reason to this package's Lost bool.
//
// THE ONE PLACE THE TWO VOCABULARIES MEET. toolpair names WHY a call got no result (spec T31);
// Tool.Lost only says that it did not, because that is what agentop decodes and its JSON is a
// wire contract.
//
// DERIVED FROM HasEnd RATHER THAN A LIST OF REASONS, so that it FAILS CLOSED. Naming the
// no-result reasons here means a reason added to toolpair later matches none of them and reads
// as false — and a lost call with Lost false renders as still running, forever, on a pane whose
// whole job is showing what finished. Asking "does this reason carry an end" instead inverts
// that: a new reason is lost until someone says otherwise, which is the safe default and needs
// no edit here.
//
// The empty reason is the one that must not be lost: it means still open, which is a different
// fact — no End at all rather than an end equal to its start.
func lostFor(r toolpair.EndReason) bool { return r != "" && !r.HasEnd() }

func buildAgents(t *Timeline, events []model.SessionEvent) {
	// The tool calls, the background jobs and the subagent returns all come from one walk over
	// the same events, in core/session/toolpair — shared with the export, which needs the same
	// pairing and the same five closing cases. Everything below is what only a timeline wants:
	// turns, the agent tree, and the spans' previews.
	paired := toolpair.Calls(events, toolpair.Options{Returns: true, Arguments: true, AgentResults: true})
	if len(paired.Turns) == 0 {
		return
	}
	// The same pairs toolpair already resolved: pairing decides each request's agent, drops the
	// agent's own housekeeping calls and resolves a reused request id, and doing it twice over
	// one session measured ~30% of a whole build.
	calls := make([]call, len(paired.Turns))
	for i, tn := range paired.Turns {
		calls[i] = call{req: tn.Req, resp: tn.Resp, agent: tn.Agent}
	}
	t.scanned = paired.ScannedBytes
	for _, j := range paired.Jobs {
		t.Jobs = append(t.Jobs, Job{Agent: j.Agent, ID: j.ID, ToolUseID: j.ToolUseID, Start: j.Start, End: j.End, Seq: j.Seq})
	}
	for _, r := range paired.Returns {
		t.Returns = append(t.Returns, Return{Agent: r.Agent, Into: r.Into, At: r.At, Seq: r.Seq})
	}
	var spawns []*spawn
	// EXACT CAPACITY, AND A SPAWN HOLDS A POINTER INTO THIS SLICE. One Tool per paired call, so
	// the append below never reallocates and the &t.Tools[…] a spawn keeps stays valid until
	// linkAgents has consumed it. Growing this slice later, or appending to it anywhere else in
	// this function, would leave those pointers addressing a stale array.
	t.Tools = make([]Tool, 0, len(paired.Calls))
	for i := range paired.Calls {
		k := &paired.Calls[i]
		var a toolArgs
		_ = json.Unmarshal([]byte(k.Arguments), &a)
		lost := lostFor(k.EndReason)
		end := k.End
		if lost && end == nil {
			// A LOST SPAN ENDS AT ITS START HERE, where toolpair leaves it open. The two
			// consumers want opposite things from the same fact: this pane draws a lost call as
			// a tick on the axis, so it needs an end, while the export must NOT carry a
			// zero-length duration — a 0 ms call passes a `durationMs > 0` filter and drags a
			// p50 toward zero (spec T16). So the zero-length end is this projection's, not the
			// pairing's. TestLostSpansSurviveClip and the four TestToolOf…IsLost tests pin it.
			at := k.Start
			end = &at
		}
		t.Tools = append(t.Tools, Tool{
			Agent: k.Agent, ID: k.ToolUseID, Name: k.Name, Start: k.Start, End: end,
			Preview: preview(k.Name, a), Error: k.IsError, Lost: lost, Seq: k.CallSeq,
		})
		if k.Name == "Agent" || k.Name == "Task" {
			sp := &spawn{by: k.Agent, turnAt: k.AskedAt, desc: a.Description, typ: a.SubagentType, tool: &t.Tools[len(t.Tools)-1]}
			if m := spawnedIDRe.FindStringSubmatch(paired.AgentResults[k.ToolUseID]); m != nil {
				sp.childID = m[1]
			}
			spawns = append(spawns, sp)
		}
	}

	type agentState struct {
		first time.Time
		prev  []model.InferenceMessage
	}
	agents := map[string]*agentState{}
	var order []string
	var calBytes, calTokens int64 // sums over the turns that set t.bytesPerToken
	var mains []promptCall        // the main agent's calls, for buildPrompts

	for _, c := range calls {
		inf := c.req.Inference
		st, known := agents[c.agent]
		if !known {
			st = &agentState{first: c.req.At}
			agents[c.agent] = st
			order = append(order, c.agent)
		}

		// A call that sends its predecessor's conversation unchanged added nothing: a retry.
		// commonPrefix is cheap here: the store interns message content, so equal strings
		// usually share a backing array.
		msgs := inf.Messages
		k := toolpair.CommonPrefix(st.prev, msgs)
		resent := known && len(msgs) > 0 && k == len(msgs) && k == len(st.prev) && c.agent != UnseparatedAgent
		st.prev = msgs

		turn := Turn{Agent: c.agent, Start: c.req.At, Resent: resent, Seq: c.req.Seq, Model: inf.Model, requestID: c.req.RequestID}
		if c.agent == MainAgent {
			mains = append(mains, promptCall{turn: len(t.Turns), starts: startsPrompt(msgs, k), side: isSuggestionCall(msgs), queued: queuedMessage(msgs)})
		}
		if c.resp == nil {
			t.Turns = append(t.Turns, turn)
			continue
		}
		end := toolpair.ResponseEnd(c.req, c.resp)
		turn.End = &end
		if out := c.resp.Inference; out != nil {
			turn.Finish = out.FinishReason
			turn.OutputTokens = out.OutputTokens
			// Only a streamed response: the ratio estimates the tokens behind a streamed body,
			// and a gateway can answer a streaming request with a buffered one.
			if out.OutputTokens > 0 && out.StreamedResponse && c.resp.Duration-c.resp.Ttfb >= calibrateMinStream {
				calBytes += c.resp.BytesDown
				calTokens += int64(out.OutputTokens)
			}
		}
		// No finish reason is a reply that never finished: the proxy records a stream cut
		// mid-reply as the 200 it started as. Only a stream can stop like that. A buffered
		// reply with no choice is complete: Bob's router answers with a model name, not a
		// reply. A refusal is a call that failed too.
		failed := failedFinish[turn.Finish]
		if turn.Finish == "" && c.resp.Inference != nil && !inf.Stream && !c.resp.Inference.StreamedResponse {
			failed = false
		}
		turn.Error = c.resp.Phase == model.SessionDenied || c.resp.Error != nil || c.resp.StatusCode >= 400 || failed
		t.Turns = append(t.Turns, turn)
	}

	if calTokens > 0 {
		t.bytesPerToken = float64(calBytes) / float64(calTokens)
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
	buildPrompts(t, mains)
}

// linkAgents builds the agent tree. A subagent is linked to the Agent call that started it by
// the id the call's result names; a fork's result names none ("Fork started"), so a fork takes
// the earliest unmatched call from its parent whose turn began before the fork's first request.
// Matching on the turn's start, not the tool's, is deliberate: the fork's first request often
// precedes the end of the response that spawned it.
func linkAgents(order []string, first map[string]time.Time, calls []call, spawns []*spawn, returns []Return) []Agent {
	parentHint := map[string]string{}
	subtask := map[string]bool{}
	for _, c := range calls {
		if c.req.ParentSubagent != "" && parentHint[c.agent] == "" {
			parentHint[c.agent] = c.req.ParentSubagent
		}
		if c.req.SubagentKind == model.SubagentKindSubtask {
			subtask[c.agent] = true
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
		if subtask[id] {
			a.Label, a.Type = "subtask "+shortID(id), model.SubagentKindSubtask
		}
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
