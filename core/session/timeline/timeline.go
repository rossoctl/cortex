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
	// Error is a call that failed: an upstream error, a refusal, or the client giving up.
	Error bool   `json:"error,omitempty"`
	Lost  bool   `json:"lost,omitempty"`
	Seq   uint64 `json:"seq"`
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
// now ages out requests that never got an answer; see hostLostAfter.
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
	buildHosts(t, events, now)
	return t
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
}

// HostKey is the row a host lands in: lowercased, with the default HTTPS port dropped, so a
// request to h and an opaque tunnel to h:443 share one row. Other ports stay distinct.
func HostKey(host string) string {
	return strings.ToLower(strings.TrimSuffix(host, ":443"))
}
