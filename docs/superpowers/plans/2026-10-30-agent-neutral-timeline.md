# Agent-Neutral Timeline Attribution — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Non-Claude-Code agents (Bob first) get turns, tools and a main row on `GET /v1/sessions/{id}/timeline`, attributed by known client instead of Claude Code markers.

**Architecture:** One gate changes — `agentOf` in `core/session/timeline/agents.go` falls back to `MainAgent` when the event's client is a known agent. The side-call filter is scoped to Claude Code (`AgentRole != ""`), a `Model` field is added to `Turn`, and the inference parser maps chat-completions `role:"tool"` + `tool_call_id` results onto `ToolResults` so tool spans close. agentop names the model in the timeline cursor strip.

**Tech Stack:** Go 1.26.10, stdlib `encoding/json`; modules `core` and `cmd/agentop`.

**Spec:** `docs/superpowers/specs/2026-10-30-agent-neutral-timeline-design.md`

## Global Constraints

- Work only in the worktree `.worktrees/timeline-bob`, branch `feat/agent-neutral-timeline`.
- Every commit: `git commit -s` (DCO) + `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`; never `Co-Authored-By`.
- Run Go per-module with `GOWORK=off` for `cmd/agentop`; `core` may use the workspace. Run `gofmt -l <path>` and `go vet` on every touched module before pushing.
- Red-first: each test is written, run, and seen to fail before its implementation.
- Claude Code's existing timeline output must not change — the existing suite in `core/session/timeline` is the regression gate.

## Review Focus

- **Unknown client** (curl, scripts) sending an inference-parseable body must NOT get a turn — the timeline must not claim agency. Test in Task 2.
- **`role:"tool"` without `tool_call_id`** must not produce a `ToolResults` entry — an empty id pairs with nothing and would poison the ends map. Test in Task 1.
- **Nil `Client`** (events from proxies predating client capture, or skipped capture) must not attribute and must not panic. Test in Task 2.
- **Bob side calls with no tools** must remain turns, not be filtered by the no-tools rule. Test in Task 2.
- **A `role:"tool"` message must keep its Content in `Messages`** — settle's cost math reads message content sizes; dropping the text would change recorded usage. Test in Task 1.

---

### Task 1: Parser maps chat-completions tool results

**Files:**
- Modify: `core/plugins/inferenceparser/plugin.go:962-982` (`inferenceMessage` + its `UnmarshalJSON`) and `plugin.go:117-165` (`parseOpenAIRequest`)
- Test: `core/plugins/inferenceparser/toolresults_test.go` (new)

**Interfaces:**
- Consumes: `pipeline.InferenceToolResult{ToolUseID, Content, IsError}` (`core/pipeline/extensions.go:414`).
- Produces: `InferenceExtension.ToolResults` populated on chat-completions requests — consumed by Task 3's tool-span pairing via the existing `ends` map in `buildAgents`. No new exported API.

- [ ] **Step 1: Write the failing test**

```go
package inferenceparser

import (
	"encoding/json"
	"testing"
)

func TestChatCompletionsToolResultsCarryTheCallID(t *testing.T) {
	body := []byte(`{"model":"premium-ide","messages":[
		{"role":"user","content":"list the files"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function",
			"function":{"name":"read_dir","arguments":"{\"path\":\"/tmp\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"file_a\nfile_b"},
		{"role":"tool","tool_call_id":"","content":"no id"}]}`)
	ext := parseOpenAIRequest(body)
	if ext == nil {
		t.Fatal("request not parsed")
	}
	if len(ext.ToolResults) != 1 {
		t.Fatalf("ToolResults = %+v, want the one call_1 result", ext.ToolResults)
	}
	r := ext.ToolResults[0]
	if r.ToolUseID != "call_1" || r.Content != "file_a\nfile_b" {
		t.Fatalf("ToolResults[0] = %+v, want {call_1 file_a\\nfile_b}", r)
	}
	// The result text stays in Messages: settle reads message content sizes.
	var toolMsg bool
	for _, m := range ext.Messages {
		if m.Role == "tool" && m.Content == "file_a\nfile_b" {
			toolMsg = true
		}
	}
	if !toolMsg {
		t.Fatal("role:tool message content missing from Messages")
	}
	// The wire tool_calls on the assistant request message are not ToolCalls (that field is
	// response-side); nothing to assert beyond no panic.
	var _ = json.Valid
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd core && GOWORK=off go test ./plugins/inferenceparser/ -run TestChatCompletionsToolResults -v`
Expected: FAIL — `ToolResults = [], want the one call_1 result`.

- [ ] **Step 3: Implement**

In `plugin.go`, extend `inferenceMessage` and its `UnmarshalJSON`:

```go
type inferenceMessage struct {
	Role         string
	Content      string
	ContentBytes int
	// ToolCallID is the tool_call_id of a chat-completions "role":"tool" result message:
	// the id of the assistant tool_calls entry it answers. Empty on every other role.
	ToolCallID string
}

func (m *inferenceMessage) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role         string          `json:"role"`
		Content      json.RawMessage `json:"content"`
		ToolCallID   string          `json:"tool_call_id"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.Content = flattenContent(raw.Content)
	m.ContentBytes = contentBytes(raw.Content)
	m.ToolCallID = raw.ToolCallID
	return nil
}
```

In `parseOpenAIRequest`, after the `ext.Messages` loop (plugin.go:146-152), add:

```go
	// A chat-completions tool result is a "role":"tool" message naming the call it answers
	// by tool_call_id — the pairing key the timeline's tool spans close on. Anthropic and
	// the Responses API surface theirs through the same ToolResults field.
	for _, msg := range req.Messages {
		if msg.Role == "tool" && msg.ToolCallID != "" {
			ext.ToolResults = append(ext.ToolResults, pipeline.InferenceToolResult{
				ToolUseID: msg.ToolCallID,
				Content:   msg.Content,
			})
		}
	}
```

- [ ] **Step 4: Run tests**

Run: `cd core && GOWORK=off go test ./plugins/inferenceparser/`
Expected: PASS, whole package.

- [ ] **Step 5: Commit**

```bash
git add core/plugins/inferenceparser/plugin.go core/plugins/inferenceparser/toolresults_test.go
git commit -s -m "Feat: Read chat-completions tool results by tool_call_id

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 2: Attribution by known client, scoped side-call filter, Turn.Model

**Files:**
- Modify: `core/session/timeline/agents.go:361-383` (`agentOf`, `isSideCall`), `agents.go:170` (turn construction), `core/session/timeline/timeline.go:76-91` (`Turn` struct)
- Test: `core/session/timeline/timeline_test.go` (append)

**Interfaces:**
- Consumes: `pipeline.IsKnownAgent(name string) bool` (`core/pipeline/client.go:408`); `SessionEvent.Client *pipeline.EventClient` with field `Name string` (`core/pipeline/client.go:39`); Task 1's `ToolResults`.
- Produces: `Turn.Model string` (`json:"model,omitempty"`) — consumed by Task 4's cursor strip.

- [ ] **Step 1: Write the failing tests**

Append to `timeline_test.go`, reusing the file's `rec`, `at`, `user`, `result`, `tools` helpers:

```go
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

func TestUnknownClientGetsNoTurns(t *testing.T) {
	var r rec
	id := r.id()
	r.add(pipeline.SessionEvent{At: at(0), Phase: pipeline.SessionRequest, RequestID: id, Host: "llm.example",
		Client: &pipeline.EventClient{Name: ""},
		Inference: &pipeline.InferenceExtension{Model: "m", Messages: []pipeline.InferenceMessage{user("hi")}, Tools: tools}})
	r.add(pipeline.SessionEvent{At: at(5), Phase: pipeline.SessionResponse, RequestID: id, Host: "llm.example", StatusCode: 200})
	tl := Build("s", r.events, at(10))
	if len(tl.Turns) != 0 {
		t.Fatalf("Turns = %d, want 0 for an unknown client", len(tl.Turns))
	}
	// Nil Client (pre-capture events) is the same story: no attribution, no panic.
	var r2 rec
	id2 := r2.id()
	r2.add(pipeline.SessionEvent{At: at(0), Phase: pipeline.SessionRequest, RequestID: id2, Host: "llm.example",
		Inference: &pipeline.InferenceExtension{Model: "m", Messages: []pipeline.InferenceMessage{user("hi")}, Tools: tools}})
	if tl := Build("s2", r2.events, at(10)); len(tl.Turns) != 0 {
		t.Fatalf("Turns = %d, want 0 for a nil Client", len(tl.Turns))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd core && GOWORK=off go test ./session/timeline/ -run 'KnownNonClaude|UnknownClient' -v`
Expected: both FAIL — 0 turns (nothing attributed), models empty.

- [ ] **Step 3: Implement**

`timeline.go`, in `Turn` (after `Seq`):

```go
	// Model is the model the call asked for, so a client that runs several models side by
	// side (Bob's premium-ide turns beside its security classifier) reads apart.
	Model  string `json:"model,omitempty"`
```

`agents.go` — replace `agentOf` and `isSideCall`:

```go
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

// isSideCall is Claude Code only: its literals match Claude's progress and recap prompts, and
// its no-tools rule holds because Claude always sends a manifest on real turns. Another
// client's tool-less calls (Bob's security classifier) are real turns with real cost.
func isSideCall(inf *pipeline.InferenceExtension) bool {
	if inf.AgentRole == "" {
		return false
	}
	if len(inf.Tools) == 0 && inf.ToolCount == 0 {
		return true
	}
	for i := len(inf.Messages) - 1; i >= 0; i-- {
		if m := inf.Messages[i]; m.Role != "system" {
			return strings.Contains(m.Content, progressPrompt) || strings.Contains(m.Content, recapPrompt)
		}
	}
	return false
}
```

`agents.go:170` — carry the model onto the turn:

```go
		turn := Turn{Agent: c.agent, Start: c.req.At, Resent: resent, Seq: c.req.Seq, Model: inf.Model}
```

- [ ] **Step 4: Run the whole package**

Run: `cd core && GOWORK=off go test ./session/timeline/`
Expected: PASS — the new tests and every existing Claude Code test (the regression gate).

- [ ] **Step 5: Commit**

```bash
git add core/session/timeline/agents.go core/session/timeline/timeline.go core/session/timeline/timeline_test.go
git commit -s -m "Feat: Attribute timeline turns to any known client (#1559)

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 3: Bob-shaped session — tools close, returns stay empty

**Files:**
- Test: `core/session/timeline/timeline_test.go` (append)

**Interfaces:**
- Consumes: Task 1's `ToolResults` pairing and Task 2's `bobTurn`.

- [ ] **Step 1: Write the failing test**

```go
func TestBobSessionToolsCloseAndReturnsStayEmpty(t *testing.T) {
	var r rec
	r.bobTurn("premium-ide", 0, 10, []pipeline.InferenceMessage{user("list files")},
		nil, pipeline.InferenceToolCall{ID: "call_1", Name: "read_dir", Arguments: `{"path":"/tmp"}`})
	// The next request carries the tool result by tool_call_id (Task 1's parse shape).
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd core && GOWORK=off go test ./session/timeline/ -run TestBobSession -v`
Expected: FAIL — before Task 2 the call is not attributed at all, so `findTool` fatals with `no tool "call_1"`. If Tasks 1–2 are already landed this test may pass on first run; that is acceptable only after verifying it fails with `agentOf`'s new case reverted — do that once by commenting the case out, running, seeing the failure, and restoring it.

- [ ] **Step 3: Run tests**

Run: `cd core && GOWORK=off go test ./session/timeline/`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add core/session/timeline/timeline_test.go
git commit -s -m "Test: Bob-shaped session closes tools and keeps returns empty

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 4: agentop names the turn's model in the cursor strip

**Files:**
- Modify: `cmd/agentop/tui/timeline_pane.go:813-865` (`cursorStrip`)
- Test: `cmd/agentop/tui/timeline_pane_test.go` (append)

**Interfaces:**
- Consumes: `timeline.Turn.Model` from Task 2.
- Produces: nothing other tasks use.

- [ ] **Step 1: Write the failing test**

Follows the shape of `TestTimelineStrip_NamesTheRequestWhenNoToolRan` (`timeline_pane_test.go:1209`) — same model, same cell, asserting the model name joins the host:

```go
// A cell inside a turn that carries a model names it, so a client running several models
// side by side (Bob's classifier beside its main turns) reads apart.
func TestTimelineStrip_NamesTheTurnModel(t *testing.T) {
	m := timelineModel(t, 140, 46)
	m.timeline.windowIdx = 3
	lay, _ := m.timelineLayoutNow()
	m.timeline.cursor = cellOf(t, lay, 5) // main's first model call: no tool yet
	for i := range m.timeline.tl.Turns {
		if m.timeline.tl.Turns[i].Agent == timeline.MainAgent && m.timeline.tl.Turns[i].End != nil &&
			!lay.axis.cells[m.timeline.cursor].from.Before(m.timeline.tl.Turns[i].Start) &&
			lay.axis.cells[m.timeline.cursor].from.Before(*m.timeline.tl.Turns[i].End) {
			m.timeline.tl.Turns[i].Model = "premium-ide"
			break
		}
	}
	m.timeline.layoutKey = tlLayoutKey{}
	lay, _ = m.timelineLayoutNow()
	strip := stripANSI(m.cursorStrip(lay))
	if !strings.Contains(strip, "premium-ide") {
		t.Errorf("strip = %q, want the turn's model named", strip)
	}
}
```

If no main-agent turn in the sample covers the cursor cell (check by running once with a `t.Log` of `m.timeline.tl.Turns`), pick the cell of a turn that does — e.g. `cellOf(t, lay, 20)` — and keep the covering-turn lookup as written.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd cmd/agentop && GOWORK=off go test ./tui/ -run TestCursorStripNamesTheTurnModel -v`
Expected: FAIL — strip says "no tool running…", no model.

- [ ] **Step 3: Implement**

In `cursorStrip`, in the `len(parts) == 0` branch, name the covering turn's model before the host:

```go
	if len(parts) == 0 {
		suffix := ""
		for _, tu := range m.timeline.tl.Turns {
			if tu.Model != "" && !cell.from.Before(tu.Start) && (tu.End == nil || cell.from.Before(*tu.End)) {
				suffix = " " + sanitizeLabel(tu.Model) + " turn"
				break
			}
		}
		if h, ok := m.cellHost(lay, cell); ok {
			return styleMuted.Render(head + "no tool running;" + suffix + "; a request to " + sanitizeLabel(h.Host) + "  [enter] events")
		}
		return styleMuted.Render(head + "no tool running" + suffix + " in this cell")
	}
```

(Keep the exact existing phrasing when `suffix` is empty — `"no tool running; a request to …"` — so existing strip tests hold; if the `;` doubling reads badly, gate the semicolons on `suffix != ""`.)

- [ ] **Step 4: Run tests**

Run: `cd cmd/agentop && GOWORK=off go test ./tui/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/agentop/tui/timeline_pane.go cmd/agentop/tui/timeline_pane_test.go
git commit -s -m "Feat: Name the turn's model in the timeline cursor strip

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 5: Docs

**Files:**
- Modify: `docs/session-api.md:135` (the "Claude Code only" paragraph in the timeline section)
- Modify: `CLAUDE.md` — the `GET /v1/sessions/{id}/timeline` row in the Session Events API endpoint table

**Interfaces:** none.

- [ ] **Step 1: Rewrite the docs**

`docs/session-api.md`, replace the sentence beginning "**Agents, turns, tools and returns are Claude Code only.**" with:

> **Turns and tools are attributed to any known client** (Claude Code, Bob, Codex, OpenCode, Pi — the canonical name parsed from the User-Agent); each gets a single `main` row, and every model call is a turn, including classifier side calls, labeled by `turn.model`. Claude Code additionally gets its subagent tree, returns and background jobs — those key on marker strings no other client sends. Requests from unknown clients render as host rows only.

`CLAUDE.md`, in the timeline row of the endpoint table, change "Claude Code agents (main and subagents, linked by the `Agent` call that spawned them), model turns" to "agents (any known client on a single `main` row; Claude Code additionally its subagent tree), model turns".

- [ ] **Step 2: Commit**

```bash
git add docs/session-api.md CLAUDE.md
git commit -s -m "Docs: Timeline attribution is by known client, not Claude Code only

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Final verification

- [ ] `cd core && GOWORK=off go test ./...` and `cd cmd/agentop && GOWORK=off go test ./...` — both green.
- [ ] `gofmt -l core/session/timeline core/plugins/inferenceparser cmd/agentop/tui` — empty; `go vet` on both modules.
- [ ] Live check on the laptop proxy (coordinate before restarting it — see the worktree rules): the archived Bob sessions must go from host-rows-only to turns (premium-ide/security/background, labeled) and a closed tool span.
