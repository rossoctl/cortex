// Package timeline turns one session's events into spans on a time axis: which Claude Code
// agent was generating, which tool it was running, when a subagent was spawned and when its
// result came back, and which hosts the session talked to. agentop's Timeline pane draws them.
//
// Computed on read from the whole history rather than folded at append time. A session is a
// few thousand events, and reading all of them removes every ordering problem an incremental
// fold has: a subagent's first request routinely lands before the response that spawned it is
// recorded, because Claude Code starts a tool while the response is still streaming.
//
// Build depends only on pipeline.SessionEvent, so it works on events from the store, the
// archive, or any other producer that fills the same fields.
package timeline

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// MainAgent is the agent id of the main conversation.
const MainAgent = "main"

// UnseparatedAgent holds subagent traffic recorded before the proxy captured Claude Code's
// agent id header (#1459): AgentRole says "subagent" but nothing says which one.
const UnseparatedAgent = "subagents"

// Kind classifies a host span.
type Kind string

const (
	KindLLM    Kind = "llm"
	KindMCP    Kind = "mcp"
	KindHTTP   Kind = "http"
	KindTunnel Kind = "tunnel"
)

// Timeline is one session's spans. An absent End means the span is still in flight. A span that
// never got an answer but provably stopped (its agent moved on, or the session did) is Lost:
// its End is its Start, so it draws as a tick rather than a bar that runs forever. On real
// sessions some unanswered requests were eleven days old.
type Timeline struct {
	ID        string    `json:"id"`
	LastSeq   uint64    `json:"lastSeq"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Truncated bool      `json:"truncated,omitempty"`
	Agents    []Agent   `json:"agents"`
	Turns     []Turn    `json:"turns"`
	Tools     []Tool    `json:"tools"`
	Returns   []Return  `json:"returns"`
	Hosts     []Host    `json:"hosts"`
	// Jobs are background shell commands. Omitted when empty, so an older agentop never sees it.
	Jobs []Job `json:"jobs,omitempty"`

	// scanned counts the message bytes searched for returns, so a test can pin that only
	// messages new to an agent are read.
	scanned int
}

// Agent is one row of the agent tree.
type Agent struct {
	ID     string `json:"id"`
	Parent string `json:"parent,omitempty"`
	Label  string `json:"label"`
	Type   string `json:"type,omitempty"`
	// SpawnTool is the id of the Agent tool call that started this agent.
	SpawnTool  string     `json:"spawnTool,omitempty"`
	SpawnedAt  *time.Time `json:"spawnedAt,omitempty"`
	ReturnedAt *time.Time `json:"returnedAt,omitempty"`
}

// Turn is one model call an agent made: from its request to the end of the response.
type Turn struct {
	Agent  string     `json:"agent"`
	Start  time.Time  `json:"start"`
	End    *time.Time `json:"end,omitempty"`
	Finish string     `json:"finish,omitempty"`
	// Error is a call that failed: an upstream error, the client giving up, or a finish in
	// failedFinish.
	Error bool `json:"error,omitempty"`
	Lost  bool `json:"lost,omitempty"`
	// Resent is a call that sent the agent's previous call's conversation again, unchanged: a
	// retry, so the time before it went to retrying rather than to the user, who added nothing.
	Resent bool   `json:"resent,omitempty"`
	Seq    uint64 `json:"seq"`
	// Model is the model the call asked for, so a client that runs several models side by
	// side (Bob's premium-ide turns beside its security classifier) reads apart.
	Model string `json:"model,omitempty"`
}

// Tool is one tool call: from the response that requested it to the agent's first request
// carrying its result.
type Tool struct {
	Agent   string     `json:"agent"`
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	Start   time.Time  `json:"start"`
	End     *time.Time `json:"end,omitempty"`
	Preview string     `json:"preview,omitempty"`
	Error   bool       `json:"error,omitempty"`
	Lost    bool       `json:"lost,omitempty"`
	Seq     uint64     `json:"seq"`
}

// Job is a background shell command: from the tool result that acknowledged it ("Command
// running in background with ID: …") to the <task-notification> naming that id. Its tool call
// ends at the acknowledgement, so without this the agent looks idle while the job runs.
type Job struct {
	Agent string     `json:"agent"`
	ID    string     `json:"id"`
	Start time.Time  `json:"start"`
	End   *time.Time `json:"end,omitempty"`
	Seq   uint64     `json:"seq"`
}

// Return is a subagent's result reaching another agent's context.
type Return struct {
	Agent string    `json:"agent"`
	Into  string    `json:"into"`
	At    time.Time `json:"at"`
	Seq   uint64    `json:"seq"`
}

// Host is one outbound request, or one opaque tunnel, to a host.
type Host struct {
	Host   string     `json:"host"`
	Kind   Kind       `json:"kind"`
	Start  time.Time  `json:"start"`
	End    *time.Time `json:"end,omitempty"`
	Status int        `json:"status,omitempty"`
	Error  bool       `json:"error,omitempty"`
	Lost   bool       `json:"lost,omitempty"`
	Seq    uint64     `json:"seq"`

	idx int // the request's index in the events Build read, while building
}

// Build computes the timeline of events, which must be one session's events in Seq order.
// now ages out requests that never got an answer; see LoseUnanswered.
func Build(id string, events []pipeline.SessionEvent, now time.Time) *Timeline {
	t := &Timeline{ID: id, Agents: []Agent{}, Turns: []Turn{}, Tools: []Tool{}, Returns: []Return{}, Hosts: []Host{}}
	if len(events) == 0 {
		return t
	}
	t.LastSeq = events[len(events)-1].Seq
	t.Start, t.End = events[0].At, events[0].At
	for i := range events {
		if at := events[i].At; at.Before(t.Start) {
			t.Start = at
		} else if at.After(t.End) {
			t.End = at
		}
	}
	buildAgents(t, events)
	buildHosts(t, events)
	endStrandedTools(t)
	t.LoseUnanswered(now)
	return t
}

// endStrandedTools ends a tool at the first CONNECT to a model host that could not be dialed
// after it started, when that comes before the tool's result or the tool has none, and no turn
// of the tool's agent or an agent below it succeeded between the two. The agent finished the
// tool and tried to send its result, and the network was gone: a lid closed or a VPN dropped.
// The result then reaches the proxy only once the network is back, hours later, or never. On
// real sessions a 9-minute Bash drew as 8 hours, and one with no result ran to the session's
// last unrelated event. Checked against Claude Code's transcripts, the first such failure fell
// within 0.1s of the tool's real end every time; other failures (an upstream 502, a client
// hanging up mid-handshake) did not, so only a failed dial counts. It names no agent, so it is
// matched by host: any host a turn of the session went to. A success in between is a blip that
// agent came back from, most often a subagent retrying while its parent's Agent tool is still
// open. Another branch's success does not count: a stranded subagent's tool still ends when the
// main agent carries on. A background job its agent made no turn after ends the same way.
//
// shortcut: a blip in any other agent, a parent included, ends a tool whose branch has no
// success before its result (a subagent's long Bash while the main agent retries), and two
// agents stranded by one outage both end at its first failure; upgrade if CONNECTs come to
// carry the agent id.
func endStrandedTools(t *Timeline) {
	turnSeq := map[uint64]bool{}
	lastTurn := map[string]time.Time{}
	parent := map[string]string{}
	for _, a := range t.Agents {
		parent[a.ID] = a.Parent
	}
	// An agent's successes count for every agent above it too, so an Agent tool waiting on a
	// subagent that retried past a blip stays open. The bound stops a cycle in parent ids.
	ok := map[string][]time.Time{}
	for _, x := range t.Turns {
		turnSeq[x.Seq] = true
		if x.Start.After(lastTurn[x.Agent]) {
			lastTurn[x.Agent] = x.Start
		}
		if x.End == nil || x.Error || x.Lost {
			continue
		}
		for a, n := x.Agent, 0; a != "" && n <= len(t.Agents); a, n = parent[a], n+1 {
			ok[a] = append(ok[a], x.Start)
		}
	}
	model := map[string]bool{}
	for _, h := range t.Hosts {
		if turnSeq[h.Seq] {
			model[h.Host] = true
		}
	}
	// recovered reports a success of agent's after f and before until (nil: ever).
	recovered := func(agent string, f time.Time, until *time.Time) bool {
		for _, s := range ok[agent] {
			if s.After(f) && (until == nil || s.Before(*until)) {
				return true
			}
		}
		return false
	}
	// Collected once: every tool is checked against them, and a session has thousands of hosts
	// but rarely more than a handful of failed dials.
	var dials []time.Time
	for _, h := range t.Hosts {
		if h.Kind == KindTunnel && h.Status == http.StatusBadGateway && model[h.Host] {
			dials = append(dials, h.Start)
		}
	}
	slices.SortFunc(dials, time.Time.Compare)
	// stranded is the first failed dial to a model host in (from, until) agent did not recover from.
	stranded := func(agent string, from time.Time, until *time.Time) *time.Time {
		for _, f := range dials {
			if until != nil && !f.Before(*until) {
				break
			}
			if f.After(from) && !recovered(agent, f, until) {
				return &f
			}
		}
		return nil
	}
	for i := range t.Tools {
		if x := &t.Tools[i]; !x.Lost {
			if at := stranded(x.Agent, x.Start, x.End); at != nil {
				x.End = at
			}
		}
	}
	for i := range t.Jobs {
		if j := &t.Jobs[i]; j.End == nil {
			after := j.Start
			if last := lastTurn[j.Agent]; last.After(after) {
				after = last
			}
			j.End = stranded(j.Agent, after, nil)
		}
	}
}

// LoseUnanswered ends, as lost, each request still unanswered hostLostAfter before the later
// of now and the session's newest event. The one part of a timeline that depends on the clock,
// so a cache can keep the rest until the session records something new and run this on each
// read. It writes h.End, so a cache runs it on a copy whose Hosts it owns.
func (t *Timeline) LoseUnanswered(now time.Time) {
	ref := t.End
	if now.After(ref) {
		ref = now
	}
	for i := range t.Hosts {
		if h := &t.Hosts[i]; h.End == nil && ref.Sub(h.Start) > hostLostAfter {
			loseHost(h)
		}
	}
}

// Clip keeps the spans that overlap [from, to]. Zero bounds are open. Agents are kept whole,
// so a client can lay out the tree before it has fetched every window.
func (t *Timeline) Clip(from, to time.Time) {
	in := func(start time.Time, end *time.Time) bool {
		if !to.IsZero() && start.After(to) {
			return false
		}
		return from.IsZero() || end == nil || !end.Before(from)
	}
	t.Turns = slices.DeleteFunc(t.Turns, func(s Turn) bool { return !in(s.Start, s.End) })
	t.Tools = slices.DeleteFunc(t.Tools, func(s Tool) bool { return !in(s.Start, s.End) })
	t.Returns = slices.DeleteFunc(t.Returns, func(s Return) bool { return !in(s.At, &s.At) })
	t.Hosts = slices.DeleteFunc(t.Hosts, func(s Host) bool { return !in(s.Start, s.End) })
	t.Jobs = slices.DeleteFunc(t.Jobs, func(s Job) bool { return !in(s.Start, s.End) })
}

// HostKey is the row a host lands in: lowercased, with the default HTTPS port dropped, so a
// request to h and an opaque tunnel to h:443 share one row. Other ports stay distinct.
func HostKey(host string) string {
	return strings.ToLower(strings.TrimSuffix(host, ":443"))
}
