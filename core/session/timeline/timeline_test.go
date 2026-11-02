package timeline

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// t0 is a fixed clock so every span below reads as an offset in seconds.
var t0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func at(sec float64) time.Time { return t0.Add(time.Duration(sec * float64(time.Second))) }

// rec builds a session the way the proxy records one: Seq in order, each request paired
// with its response by RequestID.
type rec struct {
	events []pipeline.SessionEvent
	n      int
}

func (r *rec) add(e pipeline.SessionEvent) *pipeline.SessionEvent {
	e.Seq = uint64(len(r.events) + 1)
	e.Direction = pipeline.Outbound // Inbound is the zero value; see inbound for the other kind
	r.events = append(r.events, e)
	return &r.events[len(r.events)-1]
}

func (r *rec) id() string { r.n++; return "r" + strings.Repeat("x", r.n) }

// tools is the manifest every agent request carries; a request without one is a side call.
var tools = []pipeline.InferenceTool{{Name: "Bash"}}

// turn records one model call by agent (empty = main), sending msgs and answering with calls.
func (r *rec) turn(agent string, start, dur float64, msgs []pipeline.InferenceMessage, results []pipeline.InferenceToolResult, finish string, calls ...pipeline.InferenceToolCall) {
	role := pipeline.AgentRoleMain
	if agent != "" {
		role = pipeline.AgentRoleSubagent
	}
	id := r.id()
	r.add(pipeline.SessionEvent{At: at(start), Phase: pipeline.SessionRequest, RequestID: id, Host: "llm.example", Subagent: agent,
		Inference: &pipeline.InferenceExtension{AgentRole: role, Tools: tools, Messages: msgs, ToolResults: results}})
	r.add(pipeline.SessionEvent{At: at(start + dur), Phase: pipeline.SessionResponse, RequestID: id, Host: "llm.example", Subagent: agent,
		StatusCode: 200, Duration: time.Duration(dur * float64(time.Second)),
		Inference: &pipeline.InferenceExtension{FinishReason: finish, ToolCalls: calls}})
}

func user(s string) pipeline.InferenceMessage {
	return pipeline.InferenceMessage{Role: "user", Content: s}
}

func result(id, content string) pipeline.InferenceToolResult {
	return pipeline.InferenceToolResult{ToolUseID: id, Content: content}
}

func findTool(t *testing.T, tl *Timeline, id string) Tool {
	t.Helper()
	for _, x := range tl.Tools {
		if x.ID == id {
			return x
		}
	}
	t.Fatalf("no tool %q in %+v", id, tl.Tools)
	return Tool{}
}

func findAgent(t *testing.T, tl *Timeline, id string) Agent {
	t.Helper()
	for _, a := range tl.Agents {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no agent %q in %+v", id, tl.Agents)
	return Agent{}
}

func TestToolSpansRunFromRequestingResponseToFirstResult(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("fix the bug")}
	r.turn("", 0, 5, h, nil, "tool_use",
		pipeline.InferenceToolCall{ID: "t1", Name: "Bash", Arguments: `{"command":"go   test\n ./..."}`},
		pipeline.InferenceToolCall{ID: "t2", Name: "Read", Arguments: `{"file_path":"/a/b.go"}`})
	// t1's result arrives first; the next request re-sends it, which must not move its end.
	r.turn("", 7, 3, h, []pipeline.InferenceToolResult{result("t1", "ok")}, "tool_use")
	r.turn("", 12, 2, h, []pipeline.InferenceToolResult{result("t1", "ok"), {ToolUseID: "t2", IsError: true}}, "end_turn")

	tl := Build("s", r.events, time.Time{})
	t1, t2 := findTool(t, tl, "t1"), findTool(t, tl, "t2")
	if !t1.Start.Equal(at(5)) || t1.End == nil || !t1.End.Equal(at(7)) {
		t.Errorf("t1 = %v..%v, want 5s..7s", t1.Start, t1.End)
	}
	if t2.End == nil || !t2.End.Equal(at(12)) || !t2.Error {
		t.Errorf("t2 end=%v error=%v, want 12s and an error", t2.End, t2.Error)
	}
	if t1.Preview != "go test ./..." || t2.Preview != "/a/b.go" {
		t.Errorf("previews = %q, %q", t1.Preview, t2.Preview)
	}
	if len(tl.Turns) != 3 || tl.Turns[0].Finish != "tool_use" || !tl.Turns[0].End.Equal(at(5)) {
		t.Errorf("turns = %+v", tl.Turns)
	}
}

// Claude Code starts a tool while the response is still streaming, so its result can be on a
// request recorded before that response finished.
func TestToolEndingBeforeItsResponseIsClampedToStart(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("go")}
	r.turn("", 0, 10, h, nil, "tool_use", pipeline.InferenceToolCall{ID: "t1", Name: "Read"})
	r.turn("", 8, 1, h, []pipeline.InferenceToolResult{result("t1", "x")}, "end_turn")
	tool := findTool(t, Build("s", r.events, time.Time{}), "t1")
	if tool.End == nil || !tool.End.Equal(tool.Start) {
		t.Errorf("end = %v, want clamped to start %v", tool.End, tool.Start)
	}
}

// A result only closes a tool in the agent that called it: a fork's first request carries its
// parent's whole history, results included.
func TestToolResultsInAnotherAgentDoNotCloseTheTool(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("go")}
	r.turn("", 0, 1, h, nil, "tool_use", pipeline.InferenceToolCall{ID: "t1", Name: "Bash"})
	r.turn("fork1", 2, 1, h, []pipeline.InferenceToolResult{result("t1", "x")}, "end_turn")
	if tool := findTool(t, Build("s", r.events, time.Time{}), "t1"); tool.End != nil {
		t.Errorf("t1 closed at %v by another agent's request", tool.End)
	}
}

func TestSideCallsAndProgressLabelsAreNotTurns(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("go")}
	r.turn("", 0, 1, h, nil, "end_turn")
	// Titler: no tool manifest.
	id := r.id()
	r.add(pipeline.SessionEvent{At: at(2), Phase: pipeline.SessionRequest, RequestID: id,
		Inference: &pipeline.InferenceExtension{AgentRole: pipeline.AgentRoleMain, Messages: h}})
	// A subagent's progress line: full context, but the last message asks for a status.
	r.turn("a1", 3, 1, []pipeline.InferenceMessage{user("work"), user("Describe your most recent action in 3-5 words.")}, nil, "end_turn")
	// Not Claude Code: no AgentRole.
	id = r.id()
	r.add(pipeline.SessionEvent{At: at(4), Phase: pipeline.SessionRequest, RequestID: id,
		Inference: &pipeline.InferenceExtension{Tools: tools, Messages: h}})

	tl := Build("s", r.events, time.Time{})
	if len(tl.Turns) != 1 {
		t.Errorf("turns = %d, want only the main agent's real turn: %+v", len(tl.Turns), tl.Turns)
	}
	if len(tl.Agents) != 1 || tl.Agents[0].ID != MainAgent {
		t.Errorf("agents = %+v, want main only", tl.Agents)
	}
}

func TestBackgroundSubagentLinksBySpawnResultAndReturnsByMarker(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("survey this")}
	r.turn("", 0, 4, h, nil, "tool_use",
		pipeline.InferenceToolCall{ID: "spawn1", Name: "Agent", Arguments: `{"description":"Survey the code","subagent_type":"Explore","prompt":"..."}`})
	// The subagent's first request precedes the spawning response's end: the tool runs mid-stream.
	r.turn("a7f3", 3, 2, []pipeline.InferenceMessage{user("...")}, nil, "end_turn")
	h2 := append(h, user("ack"))
	r.turn("", 6, 1, h2, []pipeline.InferenceToolResult{result("spawn1", "Async agent launched successfully.\nagentId: a7f3 (internal ID)")}, "end_turn")
	// The result arrives later as a new message in main's history; the next request re-sends it.
	h3 := append(h2, user(`Another Claude session sent a message:
<agent-message from="a7f3">
[Subagent hand-back] done`))
	r.turn("", 20, 1, h3, nil, "end_turn")
	r.turn("", 30, 1, append(h3, user("thanks")), nil, "end_turn")

	tl := Build("s", r.events, time.Time{})
	a := findAgent(t, tl, "a7f3")
	if a.Parent != MainAgent || a.SpawnTool != "spawn1" || a.Label != "Survey the code" || a.Type != "Explore" {
		t.Errorf("agent = %+v", a)
	}
	if a.SpawnedAt == nil || !a.SpawnedAt.Equal(at(3)) {
		t.Errorf("spawnedAt = %v, want the child's first request at 3s", a.SpawnedAt)
	}
	if len(tl.Returns) != 1 || tl.Returns[0].Agent != "a7f3" || tl.Returns[0].Into != MainAgent || !tl.Returns[0].At.Equal(at(20)) {
		t.Errorf("returns = %+v, want one into main at 20s", tl.Returns)
	}
	if a.ReturnedAt == nil || !a.ReturnedAt.Equal(at(20)) {
		t.Errorf("returnedAt = %v", a.ReturnedAt)
	}
}

func TestTaskNotificationIsAReturn(t *testing.T) {
	var r rec
	r.turn("", 0, 1, []pipeline.InferenceMessage{user("go")}, nil, "tool_use",
		pipeline.InferenceToolCall{ID: "s1", Name: "Agent", Arguments: `{"description":"d"}`})
	r.turn("b2", 1, 1, []pipeline.InferenceMessage{user("p")}, nil, "end_turn")
	r.turn("", 5, 1, []pipeline.InferenceMessage{user("go"),
		{Role: "system", Content: "<task-notification>\n<task-id>b2</task-id>\n<status>completed</status>"}}, nil, "end_turn")
	tl := Build("s", r.events, time.Time{})
	if len(tl.Returns) != 1 || tl.Returns[0].Agent != "b2" {
		t.Errorf("returns = %+v", tl.Returns)
	}
}

// A fork's result names no agent id, so it links to its parent's earliest unmatched spawn
// whose turn began before the fork's first request. Two forks from one turn take one each.
func TestForksLinkToTheirOwnSpawnsInOrder(t *testing.T) {
	var r rec
	r.turn("", 0, 1, []pipeline.InferenceMessage{user("go")}, nil, "tool_use",
		pipeline.InferenceToolCall{ID: "s1", Name: "Agent", Arguments: `{"description":"builder"}`})
	r.turn("p1", 2, 1, []pipeline.InferenceMessage{user("build")}, nil, "tool_use",
		pipeline.InferenceToolCall{ID: "f1", Name: "Agent", Arguments: `{"description":"fork one"}`},
		pipeline.InferenceToolCall{ID: "f2", Name: "Agent", Arguments: `{"description":"fork two"}`})
	r.turn("", 3, 1, []pipeline.InferenceMessage{user("go")}, []pipeline.InferenceToolResult{result("s1", "agentId: p1")}, "end_turn")
	r.turn("k1", 4, 1, []pipeline.InferenceMessage{user("build"), user("fork directive one")}, nil, "end_turn")
	r.events[len(r.events)-2].ParentSubagent = "p1"
	r.turn("k2", 4.5, 1, []pipeline.InferenceMessage{user("build"), user("fork directive two")}, nil, "end_turn")
	r.events[len(r.events)-2].ParentSubagent = "p1"
	r.turn("p1", 6, 1, []pipeline.InferenceMessage{user("build")},
		[]pipeline.InferenceToolResult{result("f1", "Fork started — processing in background"), result("f2", "Fork started — processing in background")}, "end_turn")

	tl := Build("s", r.events, time.Time{})
	for id, want := range map[string]struct{ parent, spawn, label string }{
		"p1": {MainAgent, "s1", "builder"},
		"k1": {"p1", "f1", "fork one"},
		"k2": {"p1", "f2", "fork two"},
	} {
		a := findAgent(t, tl, id)
		if a.Parent != want.parent || a.SpawnTool != want.spawn || a.Label != want.label {
			t.Errorf("%s = parent %q spawn %q label %q, want %+v", id, a.Parent, a.SpawnTool, a.Label, want)
		}
	}
}

func TestSubagentWithNoMatchingSpawnStillGetsARow(t *testing.T) {
	var r rec
	r.turn("", 0, 1, []pipeline.InferenceMessage{user("go")}, nil, "end_turn")
	r.turn("zz99887766", 2, 1, []pipeline.InferenceMessage{user("p")}, nil, "end_turn")
	a := findAgent(t, Build("s", r.events, time.Time{}), "zz99887766")
	if a.Parent != MainAgent || a.SpawnTool != "" || a.Label != "subagent zz998877" {
		t.Errorf("agent = %+v", a)
	}
}

// Before #1459 the proxy did not record which subagent sent a request.
func TestSubagentTrafficWithoutAnIDIsOneRow(t *testing.T) {
	var r rec
	r.turn("", 0, 1, []pipeline.InferenceMessage{user("go")}, nil, "end_turn")
	id := r.id()
	r.add(pipeline.SessionEvent{At: at(2), Phase: pipeline.SessionRequest, RequestID: id,
		Inference: &pipeline.InferenceExtension{AgentRole: pipeline.AgentRoleSubagent, Tools: tools, Messages: []pipeline.InferenceMessage{user("p")}}})
	a := findAgent(t, Build("s", r.events, time.Time{}), UnseparatedAgent)
	if a.Parent != MainAgent || a.Label != "subagents (not separated)" {
		t.Errorf("agent = %+v", a)
	}
}

// The scan covers only messages new to the agent, so a marker re-sent on every later turn is
// one return, and an assistant message quoting the marker is none.
func TestReturnsAreCountedOnceAndNotFromAssistantText(t *testing.T) {
	var r rec
	r.turn("", 0, 1, []pipeline.InferenceMessage{user("go")}, nil, "tool_use",
		pipeline.InferenceToolCall{ID: "s1", Name: "Agent"})
	r.turn("c3", 1, 1, []pipeline.InferenceMessage{user("p")}, nil, "end_turn")
	h := []pipeline.InferenceMessage{user("go"), {Role: "assistant", Content: `I will wait for <agent-message from="c3">`}}
	r.turn("", 3, 1, h, nil, "end_turn")
	h = append(h, user(`<agent-message from="c3"> result`))
	for i := 0; i < 3; i++ {
		r.turn("", float64(5+i*2), 1, h, nil, "end_turn")
	}
	if got := Build("s", r.events, time.Time{}).Returns; len(got) != 1 || !got[0].At.Equal(at(5)) {
		t.Errorf("returns = %+v, want one at 5s", got)
	}
}

func TestReturnsNamingUnknownAgentsAreDropped(t *testing.T) {
	var r rec
	r.turn("", 0, 1, []pipeline.InferenceMessage{user(`<agent-message from="elsewhere"> quoted from another session`)}, nil, "end_turn")
	if got := Build("s", r.events, time.Time{}).Returns; len(got) != 0 {
		t.Errorf("returns = %+v, want none", got)
	}
}

func TestHostsPairByRequestAndFoldBridgedOpens(t *testing.T) {
	var r rec
	// A bridged CONNECT open (no reason), then the decrypted request on the same host.
	r.add(pipeline.SessionEvent{At: at(0), Phase: pipeline.SessionRequest, RequestID: "c1", Host: "api.example.com:443", Tunnel: true})
	r.add(pipeline.SessionEvent{At: at(0.1), Phase: pipeline.SessionRequest, RequestID: "h1", Host: "api.example.com"})
	r.add(pipeline.SessionEvent{At: at(9), Phase: pipeline.SessionResponse, RequestID: "h1", Host: "api.example.com", StatusCode: 502, Duration: 2 * time.Second})
	// An opaque tunnel to the same host, by its :443 form.
	r.add(pipeline.SessionEvent{At: at(3), Phase: pipeline.SessionRequest, RequestID: "t1", Host: "API.example.com:443", Tunnel: true, TunnelReason: pipeline.TunnelReason("passthrough-host")})
	r.add(pipeline.SessionEvent{At: at(4), Phase: pipeline.SessionResponse, RequestID: "t1", Host: "api.example.com:443", Tunnel: true})
	// Never answered.
	r.add(pipeline.SessionEvent{At: at(5), Phase: pipeline.SessionRequest, RequestID: "o1", Host: "localhost:11434"})
	// Inbound traffic is not egress.
	r.add(pipeline.SessionEvent{At: at(6), Phase: pipeline.SessionRequest, RequestID: "i1", Host: "me"}).Direction = pipeline.Inbound

	hs := Build("s", r.events, time.Time{}).Hosts
	if len(hs) != 3 {
		t.Fatalf("hosts = %+v, want 3", hs)
	}
	if h := hs[0]; h.Host != "api.example.com" || h.Kind != KindHTTP || !h.Error || h.Status != 502 || !h.End.Equal(at(2.1)) {
		t.Errorf("decrypted request = %+v, want an error ending at start+duration", h)
	}
	if h := hs[1]; h.Host != "api.example.com" || h.Kind != KindTunnel || h.Error {
		t.Errorf("tunnel = %+v", h)
	}
	if h := hs[2]; h.Host != "localhost:11434" || h.End != nil {
		t.Errorf("unanswered = %+v, want open", h)
	}
}

// The forward proxy records a bridged CONNECT's open together with the tunnel's first row,
// whichever that is: a decrypted request, one a plugin redirected (Host is the target and
// RequestedHost the CONNECT's host), or a denial. Each folds the open away. A row naming
// another host does not.
func TestBridgedOpenFoldsIntoTheRowRecordedWithIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		next   []pipeline.SessionEvent
		hosts  int
		tunnel bool
	}{
		{"decrypted request", []pipeline.SessionEvent{
			{At: at(1), Phase: pipeline.SessionRequest, RequestID: "h", Host: "api.example.com"},
			{At: at(2), Phase: pipeline.SessionResponse, RequestID: "h", Host: "api.example.com", StatusCode: 200},
		}, 1, false},
		{"redirected request", []pipeline.SessionEvent{
			{At: at(1), Phase: pipeline.SessionRequest, RequestID: "h", Host: "gateway.example", RequestedHost: "api.example.com"},
			{At: at(2), Phase: pipeline.SessionResponse, RequestID: "h", Host: "gateway.example", RequestedHost: "api.example.com", StatusCode: 200},
		}, 1, false},
		{"denial", []pipeline.SessionEvent{
			{At: at(1), Phase: pipeline.SessionDenied, RequestID: "h", Host: "api.example.com", StatusCode: 403},
		}, 1, false},
		{"redirected denial", []pipeline.SessionEvent{
			{At: at(1), Phase: pipeline.SessionDenied, RequestID: "h", Host: "gateway.example", RequestedHost: "api.example.com", StatusCode: 403},
		}, 1, false},
		{"request to another host", []pipeline.SessionEvent{
			{At: at(1), Phase: pipeline.SessionRequest, RequestID: "h", Host: "other.example"},
			{At: at(2), Phase: pipeline.SessionResponse, RequestID: "h", Host: "other.example", StatusCode: 200},
		}, 2, true},
		{"denial for another host", []pipeline.SessionEvent{
			{At: at(1), Phase: pipeline.SessionDenied, RequestID: "h", Host: "other.example", StatusCode: 403},
		}, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r rec
			r.add(pipeline.SessionEvent{At: at(0), Phase: pipeline.SessionRequest, RequestID: "c", Host: "api.example.com:443", Tunnel: true})
			for _, e := range tc.next {
				r.add(e)
			}
			hs := Build("s", r.events, time.Time{}).Hosts
			tunnel := slices.ContainsFunc(hs, func(h Host) bool { return h.Kind == KindTunnel })
			if len(hs) != tc.hosts || tunnel != tc.tunnel {
				t.Errorf("hosts = %+v, want %d with tunnel %v", hs, tc.hosts, tc.tunnel)
			}
		})
	}
}

// A tunnel with a recorded close stayed opaque: the proxy records no close for a bridged one.
// Whatever row follows its open, the tunnel keeps its span.
func TestClosedTunnelIsNeverFolded(t *testing.T) {
	for name, next := range map[string]pipeline.SessionEvent{
		"request":    {At: at(1), Phase: pipeline.SessionRequest, RequestID: "h", Host: "api.example.com"},
		"redirected": {At: at(1), Phase: pipeline.SessionRequest, RequestID: "h", Host: "gw.example", RequestedHost: "api.example.com"},
		"denial":     {At: at(1), Phase: pipeline.SessionDenied, RequestID: "d", Host: "api.example.com", StatusCode: 403},
	} {
		t.Run(name, func(t *testing.T) {
			var r rec
			r.add(pipeline.SessionEvent{At: at(0), Phase: pipeline.SessionRequest, RequestID: "c", Host: "api.example.com:443", Tunnel: true})
			r.add(next)
			r.add(pipeline.SessionEvent{At: at(5), Phase: pipeline.SessionResponse, RequestID: "c", Host: "api.example.com:443", Tunnel: true, StatusCode: 200})
			hs := Build("s", r.events, time.Time{}).Hosts
			if !slices.ContainsFunc(hs, func(h Host) bool { return h.Kind == KindTunnel && h.End != nil && h.End.Equal(at(5)) }) {
				t.Errorf("hosts = %+v, want the tunnel kept and closed", hs)
			}
		})
	}
}

func TestClipKeepsOverlappingSpansAndEveryAgent(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("go")}
	r.turn("", 0, 1, h, nil, "tool_use", pipeline.InferenceToolCall{ID: "long", Name: "Bash"})
	r.turn("", 100, 1, h, []pipeline.InferenceToolResult{result("long", "")}, "end_turn")
	r.turn("x1", 200, 1, []pipeline.InferenceMessage{user("p")}, nil, "end_turn")
	tl := Build("s", r.events, time.Time{})
	tl.Clip(at(50), at(150))
	if len(tl.Tools) != 1 || tl.Tools[0].ID != "long" {
		t.Errorf("tools = %+v, want the one spanning the window", tl.Tools)
	}
	if len(tl.Turns) != 1 || !tl.Turns[0].Start.Equal(at(100)) {
		t.Errorf("turns = %+v", tl.Turns)
	}
	if len(tl.Agents) != 2 {
		t.Errorf("agents = %+v, want both kept", tl.Agents)
	}
}

func TestEmptySession(t *testing.T) {
	tl := Build("s", nil, time.Time{})
	if tl.Agents == nil || tl.Turns == nil || tl.Tools == nil || tl.Returns == nil || tl.Hosts == nil {
		t.Errorf("nil slices encode as null: %+v", tl)
	}
}

func TestPreviewIsBounded(t *testing.T) {
	got := preview("Bash", toolArgs{Command: strings.Repeat("é", 200)})
	if n := len([]rune(got)); n != previewMax || !strings.HasSuffix(got, "…") {
		t.Errorf("preview has %d runes: %q", n, got)
	}
}

// A fork's first request carries its parent's whole history, deliveries included. Those are
// the parent's returns, already counted, not the fork's.
func TestForkInheritingADeliveryDoesNotRepeatTheReturn(t *testing.T) {
	var r rec
	r.turn("", 0, 1, []pipeline.InferenceMessage{user("go")}, nil, "tool_use",
		pipeline.InferenceToolCall{ID: "s1", Name: "Agent"}, pipeline.InferenceToolCall{ID: "s2", Name: "Agent"})
	r.turn("c3", 1, 1, []pipeline.InferenceMessage{user("p")}, nil, "end_turn")
	h := []pipeline.InferenceMessage{user("go"), user(`<agent-message from="c3"> result`)}
	r.turn("", 5, 1, h, nil, "end_turn")
	r.turn("fk", 7, 1, append(h, user("fork directive")), nil, "end_turn")
	if got := Build("s", r.events, time.Time{}).Returns; len(got) != 1 || got[0].Into != MainAgent {
		t.Errorf("returns = %+v, want only main's", got)
	}
}

// A subagent whose spawn was never parsed (its turn crossed an opaque tunnel) must not take a
// spawn its parent made later, which belongs to a later child.
func TestSubagentDoesNotClaimALaterSpawn(t *testing.T) {
	var r rec
	r.turn("", 0, 1, []pipeline.InferenceMessage{user("go")}, nil, "end_turn")
	r.turn("orphan", 1, 1, []pipeline.InferenceMessage{user("p")}, nil, "end_turn")
	r.turn("", 5, 1, []pipeline.InferenceMessage{user("go")}, nil, "tool_use",
		pipeline.InferenceToolCall{ID: "late", Name: "Agent", Arguments: `{"description":"later child"}`})
	r.turn("child", 6, 1, []pipeline.InferenceMessage{user("q")}, nil, "end_turn")
	tl := Build("s", r.events, time.Time{})
	if a := findAgent(t, tl, "orphan"); a.SpawnTool != "" {
		t.Errorf("orphan claimed %q", a.SpawnTool)
	}
	if a := findAgent(t, tl, "child"); a.SpawnTool != "late" {
		t.Errorf("child spawn = %q, want late", a.SpawnTool)
	}
}

// Each turn re-sends the whole conversation; only what is new to the agent may be scanned.
func TestOnlyNewMessagesAreScanned(t *testing.T) {
	var r rec
	big := user(strings.Repeat("x", 10_000))
	h := []pipeline.InferenceMessage{big}
	for i := 0; i < 20; i++ {
		h = append(h, user(strings.Repeat("y", 100)))
		r.turn("", float64(i), 0.5, slices.Clone(h), nil, "end_turn")
	}
	total := Build("s", r.events, time.Time{}).scanned
	if want := 10_000 + 20*100; total != want {
		t.Errorf("scanned %d bytes, want %d (each message once)", total, want)
	}
}

func TestCommonPrefix(t *testing.T) {
	a, b, c := user("a"), user("b"), user("c")
	for _, tc := range []struct {
		prev, cur []pipeline.InferenceMessage
		want      int
	}{
		{nil, []pipeline.InferenceMessage{a}, 0},
		{[]pipeline.InferenceMessage{a, b}, []pipeline.InferenceMessage{a, b, c}, 2},
		{[]pipeline.InferenceMessage{a, b}, []pipeline.InferenceMessage{a, c}, 1},    // history rewritten
		{[]pipeline.InferenceMessage{a, b, c}, []pipeline.InferenceMessage{a, b}, 2}, // compacted shorter
		{[]pipeline.InferenceMessage{{Role: "system", Content: "a"}}, []pipeline.InferenceMessage{a}, 0},
	} {
		if got := commonPrefix(tc.prev, tc.cur); got != tc.want {
			t.Errorf("commonPrefix(%v, %v) = %d, want %d", tc.prev, tc.cur, got, tc.want)
		}
	}
}

// A parent can resume a subagent with SendMessage after its first hand-back; the row runs to
// the last one.
func TestReturnedAtIsTheLastReturn(t *testing.T) {
	var r rec
	r.turn("", 0, 1, []pipeline.InferenceMessage{user("go")}, nil, "tool_use", pipeline.InferenceToolCall{ID: "s1", Name: "Agent"})
	r.turn("c3", 1, 1, []pipeline.InferenceMessage{user("p")}, nil, "end_turn")
	h := []pipeline.InferenceMessage{user("go"), user(`<agent-message from="c3"> first`)}
	r.turn("", 5, 1, h, nil, "end_turn")
	r.turn("", 9, 1, append(h, user(`<agent-message from="c3"> second`)), nil, "end_turn")
	if a := findAgent(t, Build("s", r.events, time.Time{}), "c3"); a.ReturnedAt == nil || !a.ReturnedAt.Equal(at(9)) {
		t.Errorf("returnedAt = %v, want 9s", a.ReturnedAt)
	}
}

// Clip moves spans within their slices; a lost span's End must not point into one.
func TestLostSpansSurviveClip(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("go")}
	// A turn with no response, then a later turn of the same agent: the first is lost.
	r.add(pipeline.SessionEvent{At: at(0), Phase: pipeline.SessionRequest, RequestID: "dead", Host: "llm.example",
		Inference: &pipeline.InferenceExtension{AgentRole: pipeline.AgentRoleMain, Tools: tools, Messages: h}})
	// An answered tool and turn before the window, so Clip has to shift the lost ones down.
	r.turn("", 10, 1, h, nil, "tool_use", pipeline.InferenceToolCall{ID: "t0", Name: "Read"})
	r.turn("", 20, 1, h, []pipeline.InferenceToolResult{result("t0", "")}, "end_turn")
	r.turn("", 100, 1, h, nil, "tool_use", pipeline.InferenceToolCall{ID: "t1", Name: "Bash"})
	r.turn("", 200, 1, h, nil, "end_turn") // t1 never answered, and the agent moved on
	tl := Build("s", r.events, time.Time{})
	tl.Clip(at(50), at(150))
	for _, x := range tl.Turns {
		if x.Lost && !x.End.Equal(x.Start) {
			t.Errorf("lost turn ends %v, want its start %v", x.End, x.Start)
		}
	}
	for _, x := range tl.Tools {
		if x.Lost && (x.End == nil || !x.End.Equal(x.Start)) {
			t.Errorf("lost tool %s ends %v, want its start %v", x.ID, x.End, x.Start)
		}
	}
	for _, x := range tl.Hosts {
		if x.Lost && (x.End == nil || !x.End.Equal(x.Start)) {
			t.Errorf("lost host span ends %v, want its start %v", x.End, x.Start)
		}
	}
	if len(tl.Tools) != 1 || !tl.Tools[0].Lost {
		t.Fatalf("tools = %+v, want lost t1", tl.Tools)
	}
}

// The same for a lost turn: one the clip shifts down past an answered turn it drops.
func TestLostTurnSurvivesClip(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("go")}
	r.turn("", 0, 1, h, nil, "end_turn")
	r.add(pipeline.SessionEvent{At: at(100), Phase: pipeline.SessionRequest, RequestID: "dead", Host: "llm.example",
		Inference: &pipeline.InferenceExtension{AgentRole: pipeline.AgentRoleMain, Tools: tools, Messages: h}})
	r.turn("", 200, 1, h, nil, "end_turn")
	tl := Build("s", r.events, time.Time{})
	tl.Clip(at(50), at(150))
	if len(tl.Turns) != 1 || !tl.Turns[0].Lost || !tl.Turns[0].End.Equal(at(100)) {
		t.Errorf("turns = %+v, want the lost one ending at 100s", tl.Turns)
	}
}

// A request refused before it was recorded leaves only the denial.
func TestDenialWithoutARequestIsAHostSpan(t *testing.T) {
	var r rec
	r.add(pipeline.SessionEvent{At: at(1), Phase: pipeline.SessionDenied, RequestID: "x", Host: "evil.example:443", StatusCode: 403})
	hs := Build("s", r.events, time.Time{}).Hosts
	if len(hs) != 1 || hs[0].Host != "evil.example" || !hs[0].Error || hs[0].End == nil {
		t.Errorf("hosts = %+v, want one refused span", hs)
	}
}

// The transparent listener records every tunnel's open with no reason. One that a decrypted
// request does not follow is an opaque tunnel, not a bridged CONNECT.
func TestReasonlessOpenWithoutADecryptedRequestIsATunnel(t *testing.T) {
	var r rec
	r.add(pipeline.SessionEvent{At: at(0), Phase: pipeline.SessionRequest, RequestID: "t", Host: "github.com:443", Tunnel: true})
	r.add(pipeline.SessionEvent{At: at(5), Phase: pipeline.SessionResponse, RequestID: "t", Host: "github.com:443", Tunnel: true, BytesDown: 9})
	hs := Build("s", r.events, time.Time{}).Hosts
	if len(hs) != 1 || hs[0].Kind != KindTunnel || hs[0].End == nil || !hs[0].End.Equal(at(5)) {
		t.Errorf("hosts = %+v, want the tunnel kept and closed", hs)
	}
}

// A reused or missing request id must not leave the earlier request drawn as in flight forever.
func TestReusedRequestIDLosesTheEarlierRequest(t *testing.T) {
	var r rec
	r.add(pipeline.SessionEvent{At: at(0), Phase: pipeline.SessionRequest, RequestID: "same", Host: "a.example"})
	r.add(pipeline.SessionEvent{At: at(1), Phase: pipeline.SessionRequest, RequestID: "same", Host: "a.example"})
	r.add(pipeline.SessionEvent{At: at(2), Phase: pipeline.SessionResponse, RequestID: "same", Host: "a.example", StatusCode: 200})
	r.add(pipeline.SessionEvent{At: at(3), Phase: pipeline.SessionRequest, Host: "b.example"})
	r.add(pipeline.SessionEvent{At: at(4), Phase: pipeline.SessionResponse, Host: "b.example", StatusCode: 200})
	hs := Build("s", r.events, time.Time{}).Hosts
	if len(hs) != 3 || !hs[0].Lost || hs[1].End == nil || !hs[1].End.Equal(at(2)) {
		t.Errorf("hosts = %+v, want the first lost and the second answered", hs)
	}
}

// A hung request that is itself the session's last event ages out against now.
func TestHungLastRequestIsLostAgainstNow(t *testing.T) {
	var r rec
	r.add(pipeline.SessionEvent{At: at(0), Phase: pipeline.SessionRequest, RequestID: "h", Host: "slow.example"})
	if hs := Build("s", r.events, at(30)).Hosts; hs[0].Lost {
		t.Errorf("lost after 30s; want in flight")
	}
	if hs := Build("s", r.events, at(3600)).Hosts; !hs[0].Lost {
		t.Errorf("still in flight after an hour; want lost")
	}
}

// Two identically worded hand-backs at different points in the history are two returns.
func TestRepeatedIdenticalHandBackIsTwoReturns(t *testing.T) {
	var r rec
	r.turn("", 0, 1, []pipeline.InferenceMessage{user("go")}, nil, "tool_use", pipeline.InferenceToolCall{ID: "s1", Name: "Agent"})
	r.turn("c3", 1, 1, []pipeline.InferenceMessage{user("p")}, nil, "end_turn")
	same := user(`<agent-message from="c3"> done`)
	h := []pipeline.InferenceMessage{user("go"), same}
	r.turn("", 5, 1, h, nil, "end_turn")
	r.turn("", 9, 1, append(h, user("again"), same), nil, "end_turn")
	if got := Build("s", r.events, time.Time{}).Returns; len(got) != 2 {
		t.Errorf("returns = %+v, want 2", got)
	}
}

// A 200 that ends with no finish reason is a stream cut mid-reply, and a refusal is a call that
// failed; neither reads as a clean reply.
func TestReplyWithoutAFinishIsAnError(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("go")}
	r.turn("", 0, 1, h, nil, "")
	r.turn("", 2, 1, h, nil, "refusal")
	r.turn("", 4, 1, h, nil, "end_turn")
	r.turn("", 6, 1, h, nil, "content_filter")
	r.turn("", 8, 1, h, nil, "failed")
	r.turn("", 10, 1, h, nil, "cancelled")
	r.turn("", 12, 1, h, nil, "incomplete")
	tl := Build("s", r.events, time.Time{})
	want := []bool{true, true, false, true, true, true, false}
	for i, w := range want {
		if tl.Turns[i].Error != w {
			t.Errorf("turn %d finish %q: error = %v, want %v", i, tl.Turns[i].Finish, tl.Turns[i].Error, w)
		}
	}
}

// A call that sends its predecessor's conversation unchanged is a retry; one that adds a message
// is not, nor is an agent's first call.
func TestResentMarksARetry(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("go")}
	r.turn("", 0, 1, h, nil, "")
	r.turn("", 5, 1, h, nil, "tool_use")
	h2 := append(slices.Clone(h), user("next"))
	r.turn("", 9, 1, h2, nil, "end_turn")
	// A compaction sends fewer messages than the call before, all of them a prefix of it.
	r.turn("", 12, 1, h, nil, "end_turn")
	tl := Build("s", r.events, time.Time{})
	if tl.Turns[0].Resent || !tl.Turns[1].Resent || tl.Turns[2].Resent || tl.Turns[3].Resent {
		t.Errorf("resent = %v %v %v %v, want the second only", tl.Turns[0].Resent, tl.Turns[1].Resent, tl.Turns[2].Resent, tl.Turns[3].Resent)
	}
}

// A background shell job runs from the result acknowledging it to the notification naming it.
func TestBackgroundJobRunsUntilItsNotification(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("go")}
	r.turn("", 0, 1, h, nil, "tool_use", pipeline.InferenceToolCall{ID: "t1", Name: "Bash"})
	r.turn("", 2, 1, h, []pipeline.InferenceToolResult{result("t1", "Command running in background with ID: bk42. Output is being written to: /tmp/x")}, "end_turn")
	// The id quoted by the agent itself is not a delivery; the notification is.
	h2 := append(h, pipeline.InferenceMessage{Role: "assistant", Content: "<task-notification><task-id>bk42</task-id>"})
	r.turn("", 30, 1, h2, nil, "end_turn")
	r.turn("", 60, 1, append(h2, pipeline.InferenceMessage{Role: "user", Content: "<task-notification>\n<task-id>bk42</task-id>\n<status>completed</status>"}), nil, "end_turn")
	tl := Build("s", r.events, time.Time{})
	if len(tl.Jobs) != 1 || tl.Jobs[0].ID != "bk42" || !tl.Jobs[0].Start.Equal(at(2)) || tl.Jobs[0].End == nil || !tl.Jobs[0].End.Equal(at(60)) {
		t.Fatalf("jobs = %+v, want bk42 from 2s to 60s", tl.Jobs)
	}
}

// A result that only quotes the acknowledgement, a file or diff read back, starts no job.
func TestQuotedBackgroundMarkerStartsNoJob(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("go")}
	r.turn("", 0, 1, h, nil, "tool_use", pipeline.InferenceToolCall{ID: "t1", Name: "Read"})
	r.turn("", 2, 1, h, []pipeline.InferenceToolResult{result("t1", `+	r.turn("", 2, 1, h, result("t1", "Command running in background with ID: bk42."))`)}, "end_turn")
	if tl := Build("s", r.events, time.Time{}); len(tl.Jobs) != 0 {
		t.Errorf("jobs = %+v, want none", tl.Jobs)
	}
}

// Aging an unanswered request is the one clock-dependent step, so a cached timeline can run it
// again later and get what a fresh Build would.
func TestLoseUnansweredMatchesAFreshBuild(t *testing.T) {
	var r rec
	r.add(pipeline.SessionEvent{At: at(0), Phase: pipeline.SessionRequest, RequestID: "h", Host: "slow.example"})
	cached := Build("s", r.events, at(30))
	if cached.Hosts[0].Lost {
		t.Fatal("lost after 30s; want in flight")
	}
	cached.LoseUnanswered(at(3600))
	if fresh := Build("s", r.events, at(3600)); cached.Hosts[0].Lost != fresh.Hosts[0].Lost || !cached.Hosts[0].Lost {
		t.Errorf("aged cached = %+v, fresh = %+v, want both lost", cached.Hosts[0], fresh.Hosts[0])
	}
}

// Claude Code can send system messages after the progress prompt (a <total_tokens> count), so
// the prompt is found in the last message that is not a system one.
func TestProgressLabelFollowedBySystemMessagesIsNotATurn(t *testing.T) {
	var r rec
	r.turn("a1", 0, 1, []pipeline.InferenceMessage{user("work")}, nil, "end_turn")
	r.turn("a1", 2, 1, []pipeline.InferenceMessage{user("work"),
		user("Describe your most recent action in 3-5 words using present tense (-ing)."),
		{Role: "system", Content: "<total_tokens>14939408 tokens left</total_tokens>"}}, nil, "end_turn")
	// The prompt quoted earlier in a real turn's history does not make it a side call.
	r.turn("a1", 4, 1, []pipeline.InferenceMessage{user("Describe your most recent action in 3-5 words"), user("next")}, nil, "end_turn")
	tl := Build("s", r.events, time.Time{})
	if len(tl.Turns) != 2 || !tl.Turns[0].Start.Equal(at(0)) || !tl.Turns[1].Start.Equal(at(4)) {
		t.Errorf("turns = %+v, want the 0s and 4s turns only", tl.Turns)
	}
}

// The away recap Claude Code asks for when the user returns carries the whole context and tool
// list, like a progress label, and is no more a turn. Two of an unchanged conversation are not a
// retry.
func TestAwayRecapIsNotATurn(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("go")}
	r.turn("", 0, 1, h, nil, "end_turn")
	recap := append(slices.Clone(h), user("The user stepped away and is coming back. Recap in under 40 words, 1-2 plain sentences."),
		pipeline.InferenceMessage{Role: "system", Content: "<total_tokens>1 tokens left</total_tokens>"})
	r.turn("", 3600, 1, recap, nil, "end_turn")
	r.turn("", 7200, 1, recap, nil, "end_turn")
	if tl := Build("s", r.events, at(7200)); len(tl.Turns) != 1 {
		t.Errorf("turns = %+v, want only the real one", tl.Turns)
	}
}

// failedConnect records a CONNECT to host that could not be dialed: no inference parse, no agent.
func (r *rec) failedConnect(host string, sec float64) {
	id := r.id()
	r.add(pipeline.SessionEvent{At: at(sec), Phase: pipeline.SessionRequest, RequestID: id, Host: host + ":443", Tunnel: true, TunnelReason: "dial-failed"})
	r.add(pipeline.SessionEvent{At: at(sec), Phase: pipeline.SessionResponse, RequestID: id, Host: host + ":443", Tunnel: true, TunnelReason: "dial-failed",
		StatusCode: 502, Error: &pipeline.EventError{Kind: "dial_failed"}})
}

// A tool whose agent never sent its result ends at the agent's first failed attempt to reach
// the model host, not at the session's last event hours later. A failure to another host is not
// that attempt, nor is a call that reached the proxy and failed.
func TestStrandedToolEndsAtTheFailedAttemptToReportBack(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("go")}
	r.turn("", 0, 1, h, nil, "tool_use", pipeline.InferenceToolCall{ID: "t1", Name: "Bash"})
	r.failedConnect("raw.githubusercontent.com", 100)
	// A side call (no tool manifest) the client gave up on is its own failure, not t1's agent
	// trying to report back.
	id := r.id()
	r.add(pipeline.SessionEvent{At: at(200), Phase: pipeline.SessionRequest, RequestID: id, Host: "llm.example",
		Inference: &pipeline.InferenceExtension{AgentRole: pipeline.AgentRoleMain, Messages: h}})
	r.add(pipeline.SessionEvent{At: at(200), Phase: pipeline.SessionResponse, RequestID: id, Host: "llm.example",
		StatusCode: 499, Error: &pipeline.EventError{Kind: "client_canceled"}, Inference: &pipeline.InferenceExtension{}})
	r.failedConnect("llm.example", 300)
	r.failedConnect("llm.example", 301)
	r.failedConnect("raw.githubusercontent.com", 36000) // the session's last event, ten hours on
	tl := Build("s", r.events, at(36000))
	if x := findTool(t, tl, "t1"); x.End == nil || !x.End.Equal(at(300)) || x.Lost {
		t.Errorf("t1 = %+v, want it ended at the first failed request to llm.example (300s)", x)
	}
}

// A tool still running when nothing has failed stays open. A result that reached the proxy only
// after a failed dial, when the network came back, ends the tool at the failure: that is when the
// agent finished and tried to report. An upstream error is not a dial failure and ends nothing.
func TestToolEndsAtAFailedDialBeforeItsResult(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("go")}
	r.turn("", 0, 1, h, nil, "tool_use", pipeline.InferenceToolCall{ID: "t1", Name: "Bash"})
	if x := findTool(t, Build("s", r.events, at(60)), "t1"); x.End != nil {
		t.Errorf("t1 = %+v, want it still running", x)
	}
	id := r.id()
	r.add(pipeline.SessionEvent{At: at(20), Phase: pipeline.SessionRequest, RequestID: id, Host: "llm.example"})
	r.add(pipeline.SessionEvent{At: at(20), Phase: pipeline.SessionResponse, RequestID: id, Host: "llm.example", StatusCode: 502,
		Error: &pipeline.EventError{Kind: "upstream_error"}})
	r.failedConnect("llm.example", 30)
	r.turn("", 28800, 1, h, []pipeline.InferenceToolResult{result("t1", "ok")}, "end_turn") // back online 8h later
	if x := findTool(t, Build("s", r.events, at(28800)), "t1"); x.End == nil || !x.End.Equal(at(30)) {
		t.Errorf("t1 = %+v, want it ended at the failed dial at 30s", x)
	}
}

// A background job no notification named ends at the first failed request to the model host
// after its agent's last turn, when the agent was cut off.
func TestStrandedJobEndsAtTheAgentsLastFailure(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("go")}
	r.turn("", 0, 1, h, nil, "tool_use", pipeline.InferenceToolCall{ID: "t1", Name: "Bash"})
	r.turn("", 2, 1, h, []pipeline.InferenceToolResult{result("t1", "Command running in background with ID: bk42.")}, "end_turn")
	r.failedConnect("llm.example", 100)                  // after the job started, before the agent's last turn: not where it stopped
	r.turn("", 500, 1, append(h, user("more")), nil, "") // cut mid-reply: the agent's last turn, and a failed one
	r.failedConnect("llm.example", 900)
	tl := Build("s", r.events, at(36000))
	if len(tl.Jobs) != 1 || tl.Jobs[0].End == nil || !tl.Jobs[0].End.Equal(at(900)) {
		t.Errorf("jobs = %+v, want bk42 ended at 900s", tl.Jobs)
	}
}

// A failure some agent recovered from is a blip, not where anyone was cut off: the parent's
// Agent tool stays open while its subagent retries past one failed CONNECT. A failed request
// that reached the model host is not a CONNECT that could not be dialed.
func TestRecoveredOrAnsweredFailureEndsNoTool(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("go")}
	r.turn("", 0, 1, h, nil, "tool_use", pipeline.InferenceToolCall{ID: "t1", Name: "Agent"})
	r.turn("a1", 2, 1, h, nil, "end_turn")
	r.failedConnect("llm.example", 10)
	r.turn("a1", 11, 1, append(h, user("x")), nil, "end_turn")
	id := r.id()
	r.add(pipeline.SessionEvent{At: at(20), Phase: pipeline.SessionRequest, RequestID: id, Host: "llm.example"})
	r.add(pipeline.SessionEvent{At: at(20), Phase: pipeline.SessionResponse, RequestID: id, Host: "llm.example", StatusCode: 404})
	tl := Build("s", r.events, at(30))
	if x := findTool(t, tl, "t1"); x.End != nil {
		t.Errorf("t1 = %+v, want it still running", x)
	}
}

// The headline case: a subagent cut off mid-tool, and the main agent carrying on once the
// network is back. The main agent's success is another branch's, so the subagent's tool still
// ends at its failed report-back.
func TestStrandedSubagentToolEndsAfterTheMainAgentRecovers(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("go")}
	r.turn("", 0, 1, h, nil, "tool_use", pipeline.InferenceToolCall{ID: "sp", Name: "Agent"})
	r.turn("a1", 2, 1, h, nil, "tool_use", pipeline.InferenceToolCall{ID: "t1", Name: "Bash"})
	r.failedConnect("llm.example", 300)
	r.turn("", 600, 1, append(h, user("agent failed")), []pipeline.InferenceToolResult{result("sp", "error")}, "end_turn")
	tl := Build("s", r.events, at(36000))
	if x := findTool(t, tl, "t1"); x.End == nil || !x.End.Equal(at(300)) {
		t.Errorf("t1 = %+v, want it ended at a1's failed report-back (300s)", x)
	}
}

// One outage strands two subagents mid-tool. Both results arrive once the network is back, and
// the first one through is not the other's recovery: each tool ends at the failed dial.
func TestOneOutageEndsEveryStrandedSiblingsTool(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("go")}
	r.turn("", 0, 1, h, nil, "tool_use", pipeline.InferenceToolCall{ID: "sp1", Name: "Agent"}, pipeline.InferenceToolCall{ID: "sp2", Name: "Agent"})
	r.turn("a1", 2, 1, h, nil, "tool_use", pipeline.InferenceToolCall{ID: "t1", Name: "Bash"})
	r.turn("a2", 2, 1, h, nil, "tool_use", pipeline.InferenceToolCall{ID: "t2", Name: "Bash"})
	r.failedConnect("llm.example", 300)
	r.turn("a2", 28800, 1, h, []pipeline.InferenceToolResult{result("t2", "ok")}, "end_turn")
	r.turn("a1", 28801, 1, h, []pipeline.InferenceToolResult{result("t1", "ok")}, "end_turn")
	tl := Build("s", r.events, at(28900))
	for _, id := range []string{"t1", "t2"} {
		if x := findTool(t, tl, id); x.End == nil || !x.End.Equal(at(300)) {
			t.Errorf("%s = %+v, want it ended at the failed dial (300s)", id, x)
		}
	}
}

// A turn whose stream died is lost, not a success, so a subagent's lost turn does not move the
// failure that ends the Agent tool waiting on it.
func TestLostTurnIsNotASuccess(t *testing.T) {
	var r rec
	h := []pipeline.InferenceMessage{user("go")}
	r.turn("", 0, 1, h, nil, "tool_use", pipeline.InferenceToolCall{ID: "sp", Name: "Agent"})
	r.turn("a1", 2, 1, h, nil, "end_turn")
	r.failedConnect("llm.example", 300)
	for _, sec := range []float64{310, 320} { // unanswered; the second makes the first lost
		r.add(pipeline.SessionEvent{At: at(sec), Phase: pipeline.SessionRequest, RequestID: r.id(), Host: "llm.example", Subagent: "a1",
			Inference: &pipeline.InferenceExtension{AgentRole: pipeline.AgentRoleSubagent, Tools: tools, Messages: append(h, user("more"))}})
	}
	r.failedConnect("llm.example", 330)
	tl := Build("s", r.events, at(36000))
	if x := findTool(t, tl, "sp"); x.End == nil || !x.End.Equal(at(300)) {
		t.Errorf("sp = %+v, want it ended at the first failure (300s)", x)
	}
}

// bobTurn records a chat-completions call from a known non-Claude client: no Subagent, no
// AgentRole, attributed only by Client.
func (r *rec) bobTurn(model string, start, dur float64, msgs []pipeline.InferenceMessage, results []pipeline.InferenceToolResult, calls ...pipeline.InferenceToolCall) {
	id := r.id()
	client := &pipeline.EventClient{Name: "bob"}
	r.add(pipeline.SessionEvent{At: at(start), Phase: pipeline.SessionRequest, RequestID: id, Host: "api.us-east.bob.ibm.com", Client: client,
		Inference: &pipeline.InferenceExtension{Model: model, Messages: msgs, ToolResults: results}})
	r.add(pipeline.SessionEvent{At: at(start + dur), Phase: pipeline.SessionResponse, RequestID: id, Host: "api.us-east.bob.ibm.com", Client: client,
		StatusCode: 200, Duration: time.Duration(dur * float64(time.Second)),
		Inference: &pipeline.InferenceExtension{Model: model, FinishReason: "stop", ToolCalls: calls}})
}

// A known client that sends no Claude markers still gets turns, all on the single main row —
// including its tool-less side calls, which are real model spend, labeled by model.
func TestKnownNonClaudeClientGetsTurnsOnMainRow(t *testing.T) {
	var r rec
	r.bobTurn("premium-ide", 0, 10, []pipeline.InferenceMessage{user("hi")}, nil)
	r.bobTurn("security", 12, 2, []pipeline.InferenceMessage{user("check")}, nil) // side call: no tools
	tl := Build("s", r.events, at(30))
	if len(tl.Turns) != 2 {
		t.Fatalf("Turns = %d, want 2 (side calls of a non-Claude client are turns)", len(tl.Turns))
	}
	for _, tu := range tl.Turns {
		if tu.Agent != MainAgent {
			t.Fatalf("turn agent = %q, want %q", tu.Agent, MainAgent)
		}
	}
	if tl.Turns[0].Model != "premium-ide" || tl.Turns[1].Model != "security" {
		t.Fatalf("models = %q, %q; want premium-ide, security", tl.Turns[0].Model, tl.Turns[1].Model)
	}
	if len(tl.Agents) != 1 || tl.Agents[0].ID != MainAgent {
		t.Fatalf("Agents = %+v, want one main row", tl.Agents)
	}
}

// Unknown and pre-capture clients get no turns: the timeline must not claim agency where
// there is none, and a nil Client must not panic.
func TestUnknownClientGetsNoTurns(t *testing.T) {
	var r rec
	id := r.id()
	r.add(pipeline.SessionEvent{At: at(0), Phase: pipeline.SessionRequest, RequestID: id, Host: "llm.example",
		Client:    &pipeline.EventClient{Name: ""},
		Inference: &pipeline.InferenceExtension{Model: "m", Messages: []pipeline.InferenceMessage{user("hi")}, Tools: tools}})
	r.add(pipeline.SessionEvent{At: at(5), Phase: pipeline.SessionResponse, RequestID: id, Host: "llm.example", StatusCode: 200})
	if tl := Build("s", r.events, at(10)); len(tl.Turns) != 0 {
		t.Fatalf("Turns = %d, want 0 for an unknown client", len(tl.Turns))
	}
	var r2 rec
	id2 := r2.id()
	r2.add(pipeline.SessionEvent{At: at(0), Phase: pipeline.SessionRequest, RequestID: id2, Host: "llm.example",
		Inference: &pipeline.InferenceExtension{Model: "m", Messages: []pipeline.InferenceMessage{user("hi")}, Tools: tools}})
	if tl := Build("s2", r2.events, at(10)); len(tl.Turns) != 0 {
		t.Fatalf("Turns = %d, want 0 for a nil Client", len(tl.Turns))
	}
}

// A Bob-shaped session end to end: the tool call closes on the tool_call_id result, and the
// Claude-Code marker machinery (returns, jobs) stays empty because Bob sends none of it.
func TestBobSessionToolsCloseAndReturnsStayEmpty(t *testing.T) {
	var r rec
	r.bobTurn("premium-ide", 0, 10, []pipeline.InferenceMessage{user("list files")},
		nil, pipeline.InferenceToolCall{ID: "call_1", Name: "read_dir", Arguments: `{"path":"/tmp"}`})
	// The next request carries the tool result by tool_call_id (the parser's parse shape).
	r.bobTurn("premium-ide", 12, 8, []pipeline.InferenceMessage{user("list files")},
		[]pipeline.InferenceToolResult{result("call_1", "file_a\nfile_b")})
	tl := Build("s", r.events, at(30))
	tool := findTool(t, tl, "call_1")
	if tool.Name != "read_dir" {
		t.Fatalf("tool name = %q", tool.Name)
	}
	if tool.End == nil || tool.Lost {
		t.Fatalf("tool = %+v, want closed by the tool_call_id result", tool)
	}
	if want := at(12); !tool.End.Equal(want) {
		t.Fatalf("tool end = %v, want the result-bearing request at %v", tool.End, want)
	}
	if len(tl.Returns) != 0 || len(tl.Jobs) != 0 {
		t.Fatalf("Returns = %+v, Jobs = %+v; both are Claude Code markers Bob never sends", tl.Returns, tl.Jobs)
	}
}

// A pre-marker Claude Code call (AgentRole empty, attributed by its client name) still drops
// its recap: the prompt literals are Claude's own prompt text, so they run for every client,
// while the no-tools rule stays Claude Code only.
func TestPreMarkerRecapIsNotATurn(t *testing.T) {
	var r rec
	client := &pipeline.EventClient{Name: "claude-code"}
	id := r.id()
	r.add(pipeline.SessionEvent{At: at(0), Phase: pipeline.SessionRequest, RequestID: id, Host: "llm.example", Client: client,
		Inference: &pipeline.InferenceExtension{Tools: tools, Messages: []pipeline.InferenceMessage{
			user("The user stepped away and is coming back. Recap in under 40 words.")}}})
	r.add(pipeline.SessionEvent{At: at(1), Phase: pipeline.SessionResponse, RequestID: id, Host: "llm.example", Client: client,
		StatusCode: 200, Inference: &pipeline.InferenceExtension{FinishReason: "end_turn"}})
	id = r.id()
	r.add(pipeline.SessionEvent{At: at(2), Phase: pipeline.SessionRequest, RequestID: id, Host: "llm.example", Client: client,
		Inference: &pipeline.InferenceExtension{Tools: tools, Messages: []pipeline.InferenceMessage{user("go")}}})
	r.add(pipeline.SessionEvent{At: at(3), Phase: pipeline.SessionResponse, RequestID: id, Host: "llm.example", Client: client,
		StatusCode: 200, Inference: &pipeline.InferenceExtension{FinishReason: "end_turn"}})
	tl := Build("s", r.events, at(4))
	if len(tl.Turns) != 1 || !tl.Turns[0].Start.Equal(at(2)) {
		t.Errorf("turns = %+v, want only the real call at 2s", tl.Turns)
	}
}
