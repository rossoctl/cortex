# Session Server Switch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a user move a running session to another inference server (`S` on a session row, `agentop server move`), with the router dropping the reasoning artifacts only their producing server can validate.

**Architecture:** A `sessions:` map in the inference-router's config beats the stored pin and settles it; one per-session "reasoning origin" record in the plugin store decides whether a switched request carries foreign reasoning, and a format-level strip drops `thinking`/`redacted_thinking` blocks (Anthropic) and `reasoning` items (responses), never touching chat-format fields. agentop writes the override through its existing config-edit + reload path.

**Tech Stack:** Go 1.26.5; gjson/sjson for body rewrites (as `fit.go` uses); agentop bubbletea TUI; no new dependencies.

**Spec:** `docs/superpowers/specs/2026-10-16-session-server-switch-design.md` — the plan argues from it; read both.

## Global Constraints

- Commit with `git commit -s` and the `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>` trailer; never `Co-Authored-By` (repo CLAUDE.md).
- Run Go per module with `GOWORK=off` (e.g. `cd core && GOWORK=off go test ./plugins/inferencerouter/...`).
- Run `gofmt -l <path>` on every file you touch before committing; `go vet` too (core and cmd/agentop are vetted in CI).
- Plugin config fields are decoded by `routerconfig.Decode` with `DisallowUnknownFields`; unknown fields are refused.
- No error message or log may quote an API key or a full URL with credentials.
- The five-value invocation action vocabulary (`allow`/`deny`/`skip`/`modify`/`observe`) is fixed; new reasons must be machine-stable strings.
- Tests live beside the code they test, in the same package (`plugin_test.go` pattern), and drive the plugin through `pipeline.Run`, never by calling `OnRequest` directly.

## Review Focus

Input classes and failure modes the spec implies but no task's happy path covers — each pinned by the task named:

1. **An override naming a server that was removed** must give the existing 503 `pinned_server_removed`, not a crash or a silent passthrough → Task 2.
2. **A thinking-only assistant message** (no text, no tool_use) must be dropped whole, leaving no empty `content` array — Anthropic rejects an empty content array with a 400 → Task 3.
3. **A `tool_result` whose preceding assistant turn had thinking + tool_use** must keep its `tool_use` (strip never drops `tool_use`, and never reorders blocks) → Task 3.
4. **A request under an override while another agent's pin holds the session id** — the override wins and the pin is rewritten to the requesting agent, exactly as the spec's "an override names a session, not an agent" rules → Task 2.
5. **A chat-completions body carrying `reasoning_content`/`reasoning`/`thinking_blocks`** must pass through byte-identical — the strip must not touch this format → Task 3.
6. **A synthetic session id in `sessions:`** is refused at validation, and no origin record is ever written for one → Tasks 1 and 4.

---

### Task 1: `routerconfig` — the `sessions` field and its validation

**Files:**
- Modify: `core/plugins/inferencerouter/routerconfig/config.go`
- Test: `core/plugins/inferencerouter/routerconfig/config_test.go`

**Interfaces:**
- Consumes: existing `Config`, `Decode`, `Validate`, `CheckName`.
- Produces: `Config.Sessions map[string]string` (`json:"sessions"`), `CheckSessionID(id string) error`, and `SyntheticSessionID(id string) bool` (used by Task 2 and by agentop in Task 6).

- [ ] **Step 1: Write the failing tests**

Add to `core/plugins/inferencerouter/routerconfig/config_test.go` (follow the file's existing table style; if it has no table helper, write plain `TestDecode_*` funcs):

```go
func TestDecode_Sessions(t *testing.T) {
	base := `{"servers":{"ete":{"url":"https://ete.example.com","key":"k","main":"opus","helper":"haiku"}}}`
	for _, tc := range []struct {
		name    string
		extra   string
		wantErr string
	}{
		{"a session to a server", `,"sessions":{"8e298f11-8bf1-4f40-a27f-ffef5876a5b9":"ete"}`, ""},
		{"a session to empty is allowed", `,"sessions":{"8e298f11-8bf1-4f40-a27f-ffef5876a5b9":""}`, ""},
		{"a session to an unknown server is refused", `,"sessions":{"s1":"nope"}`,
			`sessions.s1: "nope" is not a server listed under servers`},
		{"the default bucket is refused", `,"sessions":{"default":"ete"}`,
			`sessions.default: "default" is a synthetic session, which holds many conversations, not one to move`},
		{"a pending id is refused", `,"sessions":{"pending:claude-code":"ete"}`,
			`sessions.pending:claude-code: "pending:claude-code" is a synthetic session`},
		{"an empty id is refused", `,"sessions":{"":"ete"}`,
			`sessions.: the session id is empty`},
		{"an id with whitespace is refused", `,"sessions":{"a b":"ete"}`,
			`sessions.a b: the session id has whitespace or a control character`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Decode(json.RawMessage(base + tc.extra))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Decode: %v", err)
				}
				if c.Sessions["8e298f11-8bf1-4f40-a27f-ffef5876a5b9"] != "ete" && tc.name == "a session to a server" {
					t.Errorf("Sessions = %v", c.Sessions)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Decode error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `cd core && GOWORK=off go test ./plugins/inferencerouter/routerconfig/`
Expected: FAIL — unknown field `sessions` (DisallowUnknownFields) or compile error on `c.Sessions`.

- [ ] **Step 3: Implement**

In `config.go`, add the field to `Config` (after `Agents`):

```go
	// Sessions moves one running session to a server, by the session's id, beating
	// the pin that session already holds. "" stops routing that session. agentop
	// writes these; see CheckSessionID for what an id may look like.
	Sessions map[string]string `json:"sessions" description:"Sessions to move to a server, by session id, beating their pin. \"\" stops routing the session."`
```

Add the two helpers near `CheckAgent`:

```go
// SyntheticSessionID reports whether id is one of the session store's synthetic
// buckets — "default" and "pending:<agent>" — which hold many conversations, not
// one a move could target.
func SyntheticSessionID(id string) bool {
	return id == "default" || strings.HasPrefix(id, "pending:")
}

// CheckSessionID reports whether id can name a session in the sessions map. The
// ids the session API serves are UUIDs, but an id need only be printable, non-empty
// and free of whitespace to be one worth routing: refusing anything stricter would
// block a future agent's id shape. Synthetic ids are refused here, not later.
func CheckSessionID(id string) error {
	switch {
	case id == "":
		return errors.New("the session id is empty")
	case SyntheticSessionID(id):
		return fmt.Errorf("%q is a synthetic session, which holds many conversations, not one to move", id)
	}
	for i := 0; i < len(id); i++ {
		if id[i] <= ' ' || id[i] > '~' {
			return errors.New("the session id has whitespace or a control character")
		}
	}
	return nil
}
```

In `Validate`, after the `Agents` loop:

```go
	for _, id := range slices.Sorted(maps.Keys(c.Sessions)) {
		if err := CheckSessionID(id); err != nil {
			return fmt.Errorf("sessions.%s: %w", id, err)
		}
		if server := c.Sessions[id]; server != "" {
			if _, ok := c.Servers[server]; !ok {
				return fmt.Errorf("sessions.%s: %q is not a server listed under servers", id, server)
			}
		}
	}
```

In `applyDefaults`, add `if c.Sessions == nil { c.Sessions = map[string]string{} }`.

- [ ] **Step 4: Run to verify they pass**

Run: `cd core && GOWORK=off go test ./plugins/inferencerouter/routerconfig/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add core/plugins/inferencerouter/routerconfig/
git commit -s -m "Feat: Add a sessions map to the inference-router config"
```

### Task 2: The router honors a `sessions:` override and moves the pin

**Files:**
- Modify: `core/plugins/inferencerouter/plugin.go` (`Configure`, `serverFor`, the pin states)
- Test: `core/plugins/inferencerouter/plugin_test.go`

**Interfaces:**
- Consumes: `routerconfig.Config.Sessions` (Task 1); existing `pinning`, `pinNew`/`pinExisting`/`pinNone` constants, `pinsFor`, `synthetic`, `agentOf`.
- Produces: a new pin state `pinMoved = "moved"` appearing in the `routed` invocation's `pin` detail; `Router.sessions map[string]string` field.

- [ ] **Step 1: Write the failing tests**

Add to `plugin_test.go` (helpers: `build`, `request(store, host, ua, session)`, `run`, `assertRouted`, `assertUntouched`, `pinOf`, `assertRecord` all exist there):

```go
// sessionsConfig is routerConfig plus a sessions block.
func sessionsConfig(agents, sessions string) string {
	return `{
		"servers": {
			"ete": {"url": "https://ete.example.com", "key": "ete-key", "main": "opus", "helper": "haiku"},
			"glm": {"url": "https://glm.example.com:8443", "key": "glm-key", "main": "opus", "helper": "haiku"}
		},
		"agents": {` + agents + `},
		"sessions": {` + sessions + `}
	}`
}

// An override moves a session already pinned to another server, and the pin
// follows, so removing the override does not move it back.
func TestRouter_ASessionsOverrideMovesAPinnedSession(t *testing.T) {
	store := newStore(t)
	before := build(t, routerConfig(`"claude-code": "glm"`))
	run(t, before, request(store, eteHost, claudeUA, "running"))
	if pin, _ := pinOf(t, store, "running"); pin != "glm" {
		t.Fatalf("pin = %q, want glm", pin)
	}

	after := build(t, sessionsConfig(`"claude-code": "glm"`, `"running": "ete"`))
	moved := request(store, eteHost, claudeUA, "running")
	run(t, after, moved)
	assertRouted(t, moved, eteHost, "ete-key")
	assertRecord(t, moved, pipeline.ActionModify, "routed", map[string]string{"server": "ete", "pin": pinMoved})
	if pin, _ := pinOf(t, store, "running"); pin != "ete" {
		t.Errorf("pin = %q, want ete", pin)
	}

	// The override gone, the session stays where the override put it.
	gone := build(t, routerConfig(`"claude-code": "glm"`))
	stays := request(store, eteHost, claudeUA, "running")
	run(t, gone, stays)
	assertRouted(t, stays, eteHost, "ete-key")
	assertRecord(t, stays, pipeline.ActionModify, "routed", map[string]string{"server": "ete", "pin": pinExisting})
}

// An override routes a session whose agent is not routed at all, and settles
// the pin on the override's server.
func TestRouter_ASessionsOverrideRoutesAnUnroutedAgentsSession(t *testing.T) {
	store := newStore(t)
	p := build(t, sessionsConfig(``, `"s1": "glm"`))
	pctx := request(store, eteHost, claudeUA, "s1")
	run(t, p, pctx)
	assertRouted(t, pctx, glmHost, "glm-key")
	assertRecord(t, pctx, pipeline.ActionModify, "routed", map[string]string{"server": "glm", "pin": pinNew})
	if pin, _ := pinOf(t, store, "s1"); pin != "glm" {
		t.Errorf("pin = %q, want glm", pin)
	}
}

// "" stops routing the session: the request goes where the client sent it, and
// the pin settles not-routed.
func TestRouter_AnEmptyOverrideStopsRoutingASession(t *testing.T) {
	store := newStore(t)
	before := build(t, routerConfig(`"claude-code": "glm"`))
	run(t, before, request(store, eteHost, claudeUA, "running"))

	after := build(t, sessionsConfig(`"claude-code": "glm"`, `"running": ""`))
	pctx := request(store, eteHost, claudeUA, "running")
	run(t, after, pctx)
	assertUntouched(t, pctx, eteHost)
	assertRecord(t, pctx, pipeline.ActionSkip, "not_routed", map[string]string{"pin": pinMoved})
	if pin, _ := pinOf(t, store, "running"); pin != "" {
		t.Errorf("pin = %q, want not routed", pin)
	}
}

// An override naming a removed server gets the same 503 as a pin to one.
func TestRouter_AnOverrideToARemovedServerIsA503(t *testing.T) {
	store := newStore(t)
	p := build(t, sessionsConfig(``, `"s1": "gone"`))
	pctx := request(store, eteHost, claudeUA, "s1")
	a := run(t, p, pctx)
	if a.Type != pipeline.Deny {
		t.Fatalf("action = %+v, want Deny", a)
	}
	assertRecord(t, pctx, pipeline.ActionDeny, "pinned_server_removed", map[string]string{"server": "gone", "pin": pinMoved})
}

// The override is the user's instruction: it beats another agent's pin too, and
// the pin is rewritten to the requesting agent.
func TestRouter_AnOverrideBeatsAnotherAgentsPin(t *testing.T) {
	store := newStore(t)
	before := build(t, routerConfig(`"claude-code": "glm"`))
	run(t, before, request(store, eteHost, claudeUA, "shared"))

	after := build(t, sessionsConfig(``, `"shared": "ete"`))
	pctx := request(store, eteHost, opencodeUA, "shared")
	run(t, after, pctx)
	assertRouted(t, pctx, eteHost, "ete-key")
	if pin, _ := pinOf(t, store, "shared"); pin != "ete" {
		t.Errorf("pin = %q, want ete", pin)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `cd core && GOWORK=off go test ./plugins/inferencerouter/ -run TestRouter_.*Override -v`
Expected: FAIL — `sessionsConfig` configs are refused (unknown field) or `pinMoved` undefined; the override is ignored and the session stays on glm.

- [ ] **Step 3: Implement**

In `plugin.go`:

1. Add the state constant beside `pinNew`/`pinExisting`/`pinNone` (around `plugin.go:170`):

```go
	pinMoved    = "moved"    // a sessions: override moved the session with this request
```

2. Add the field to `Router` beside `agents`, and set it in `Configure` (where `p.agents` is assigned, ~`plugin.go:282`):

```go
	p.sessions = c.Sessions
```

3. Extend `settle` to write a moved pin:

```go
func (pn *pinning) settle(server string) {
	if pn.state == pinNew || pn.state == pinMoved {
		pn.store.save(pn.key, pin{agent: pn.agent, server: server})
	}
}
```

4. At the head of `serverFor` (after the `agentOf`/`choice` lines, before the synthetic check returns — the override check itself must skip synthetic ids, which validation already refuses, but `serverFor` also serves hand-fed configs in tests, so check anyway):

```go
	// A sessions: override is the user's explicit move: it beats the pin and every
	// unpinned rule, and the pin follows it, so removing the entry later does not
	// move the session back. pinMoved tells the record this request moved a session
	// that already held a pin; pinNew is a session that held none.
	if over, ok := p.overrideFor(pctx); ok {
		if store := p.pinsFor(pctx); store != nil {
			state := pinNew
			if pn, had := store.load(pinPrefix + pctx.Session.ID); had && pn.belongsTo(agent) && pn.server != over {
				state = pinMoved
			} else if had && pn.belongsTo(agent) && pn.server == over {
				state = pinExisting
			}
			return over, pinning{state: state, store: store, key: pinPrefix + pctx.Session.ID, agent: agent}
		}
		return over, pinning{state: pinNone}
	}
```

and the helper (near `pinnedToServer`):

```go
// overrideFor is the sessions: entry for pctx's session, when one names it. A
// synthetic session is never overridden, whatever a hand-fed config says: its id
// holds many conversations, not one to move.
func (p *Router) overrideFor(pctx *pipeline.Context) (string, bool) {
	if pctx.Session == nil || pctx.Session.ID == "" || synthetic(pctx.Session.ID) {
		return "", false
	}
	over, ok := p.sessions[pctx.Session.ID]
	return over, ok
}
```

Nothing else changes: the returned name flows through the existing redirect, key, model and limit paths, and `name == ""` takes the existing not-routed path, whose `pin.settle("")` now writes the moved pin.

- [ ] **Step 4: Run to verify they pass**

Run: `cd core && GOWORK=off go test ./plugins/inferencerouter/`
Expected: PASS — the whole package, the existing pin tests included.

- [ ] **Step 5: Commit**

```bash
git add core/plugins/inferencerouter/plugin.go core/plugins/inferencerouter/plugin_test.go
git commit -s -m "Feat: Honor a sessions override and move the session's pin"
```

### Task 3: `stripReasoning` — drop producer-bound reasoning from a body

**Files:**
- Create: `core/plugins/inferencerouter/strip.go`
- Test: `core/plugins/inferencerouter/strip_test.go`

**Interfaces:**
- Consumes: gjson/sjson (already imported by `fit.go`).
- Produces: `stripReasoning(body []byte) (out []byte, dropped int)` — pure function, no `Router` receiver.

The artifacts name themselves; no format sniffing. `thinking`/`redacted_thinking` blocks exist only in Anthropic-format assistant messages; `reasoning` items exist only in responses-format `input`. Chat-completions bodies match neither shape and are returned untouched.

- [ ] **Step 1: Write the failing tests**

Create `strip_test.go`:

```go
package inferencerouter

import (
	"encoding/json"
	"testing"
)

func stripBody(t *testing.T, body string) (string, int) {
	t.Helper()
	out, n := stripReasoning([]byte(body))
	return string(out), n
}

func TestStrip_AnthropicThinkingBlocks(t *testing.T) {
	out, n := stripBody(t, `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"","signature":"sig"},
			{"type":"text","text":"hello"},
			{"type":"tool_use","id":"t1","name":"lookup","input":{}}
		]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}
	]}`)
	if n != 1 {
		t.Fatalf("dropped = %d, want 1", n)
	}
	var v struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(v.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(v.Messages))
	}
	if string(v[1].Content) != `[{"type":"text","text":"hello"},{"type":"tool_use","id":"t1","name":"lookup","input":{}}]` {
		t.Errorf("assistant content = %s", v[1].Content)
	}
}

// A thinking-only assistant turn is dropped whole: an empty content array is a
// 400 at Anthropic, and a thinking-only turn has no tool_use for a tool_result
// to reference.
func TestStrip_AThinkingOnlyMessageIsDroppedWhole(t *testing.T) {
	out, n := stripBody(t, `{"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"sig"}]},
		{"role":"assistant","content":[{"type":"redacted_thinking","data":"x"}]},
		{"role":"assistant","content":[{"type":"text","text":"hello"}]}
	]}`)
	if n != 2 {
		t.Fatalf("dropped = %d, want 2", n)
	}
	var v struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(v.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(v.Messages))
	}
}

// The user side is never touched: a user's own content, tool results included,
// passes byte-identical.
func TestStrip_LeavesUserMessagesAlone(t *testing.T) {
	body := `{"messages":[
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"},{"type":"text","text":"and this"}]}
	]}`
	out, n := stripBody(t, body)
	if n != 0 || string(out) != body {
		t.Errorf("dropped = %d, out = %s; want 0 and unchanged", n, out)
	}
}

func TestStrip_ResponsesReasoningItems(t *testing.T) {
	out, n := stripBody(t, `{"input":[
		{"type":"reasoning","id":"rs_1","encrypted_content":"abc"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"12121"}]},
		{"role":"user","content":[{"type":"input_text","text":"add 1"}]}
	]}`)
	if n != 1 {
		t.Fatalf("dropped = %d, want 1", n)
	}
	var v struct {
		Input []struct {
			Type string `json:"type"`
		} `json:"input"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(v.Input) != 2 || v.Input[0].Type != "message" {
		t.Errorf("input = %+v", v.Input)
	}
}

// Chat completions carries reasoning as plain-text fields no measured server
// rejects; the strip must not touch this format at all.
func TestStrip_LeavesChatCompletionsAlone(t *testing.T) {
	body := `{"messages":[
		{"role":"assistant","content":"12121","reasoning_content":"17*23=391","reasoning":"...","thinking_blocks":[{"signature":"sig"}]},
		{"role":"user","content":"add 1"}
	]}`
	out, n := stripBody(t, body)
	if n != 0 || string(out) != body {
		t.Errorf("dropped = %d; want 0 and byte-identical body", n)
	}
}

// A body with nothing strippable is returned unchanged, so the wiring in Task 4
// can skip SetBody entirely.
func TestStrip_NothingToDropReturnsTheBody(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hi"}],"other":[1,2]}`
	out, n := stripBody(t, body)
	if n != 0 || string(out) != body {
		t.Errorf("dropped = %d; want 0 and unchanged", n)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `cd core && GOWORK=off go test ./plugins/inferencerouter/ -run TestStrip_ -v`
Expected: FAIL — `stripReasoning` undefined.

- [ ] **Step 3: Implement**

Create `strip.go`:

```go
package inferencerouter

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// stripReasoning drops from body the reasoning only its producer can validate,
// and returns the body it made with how many artifacts it dropped. Anthropic
// messages carry it as thinking and redacted_thinking blocks in an assistant
// message's content; responses carries it as items of type reasoning in input.
// Chat completions carries reasoning as plain-text fields every measured server
// accepts, and matches neither shape, so it is left alone — the artifacts name
// themselves; no format is sniffed. An assistant message the drop empties goes
// whole: an empty content array is refused at Anthropic, and a thinking-only
// turn carries no tool_use for a tool_result to reference.
func stripReasoning(body []byte) ([]byte, int) {
	dropped := 0
	if msgs := gjson.GetBytes(body, "messages"); msgs.IsArray() {
		for i := len(msgs.Array()) - 1; i >= 0; i-- { // reverse: deletions shift no later path
			m := msgs.Array()[i]
			if m.Get("role").String() != "assistant" {
				continue
			}
			content := m.Get("content")
			if !content.IsArray() {
				continue
			}
			blocks := content.Array()
			kept := make([]string, 0, len(blocks))
			for _, b := range blocks {
				switch b.Get("type").String() {
				case "thinking", "redacted_thinking":
					dropped++
				default:
					kept = append(kept, b.Raw)
				}
			}
			if len(kept) == len(blocks) {
				continue
			}
			if len(kept) == 0 {
				body, _ = sjson.DeleteBytes(body, "messages."+strconv.Itoa(i))
				continue
			}
			body, _ = sjson.SetRawBytes(body, "messages."+strconv.Itoa(i)+".content",
				[]byte("["+strings.Join(kept, ",")+"]"))
		}
	}
	if items := gjson.GetBytes(body, "input"); items.IsArray() {
		for i := len(items.Array()) - 1; i >= 0; i-- {
			if items.Array()[i].Get("type").String() == "reasoning" {
				body, _ = sjson.DeleteBytes(body, "input."+strconv.Itoa(i))
				dropped++
			}
		}
	}
	return body, dropped
}
```

(Add `"strconv"` to the imports.) Note the two `msgs.Array()` calls — gjson caches per-`Result`, but hoist `arr := msgs.Array()` once per loop for clarity if vet or review prefers.

- [ ] **Step 4: Run to verify they pass**

Run: `cd core && GOWORK=off go test ./plugins/inferencerouter/ -run TestStrip_ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add core/plugins/inferencerouter/strip.go core/plugins/inferencerouter/strip_test.go
git commit -s -m "Feat: Strip producer-bound reasoning from a switched request's body"
```

### Task 4: The origin record, wired into `OnRequest`

**Files:**
- Modify: `core/plugins/inferencerouter/strip.go` (state machine + wiring helper)
- Modify: `core/plugins/inferencerouter/plugin.go` (`OnRequest` calls it beside `applyLimits`)
- Test: `core/plugins/inferencerouter/strip_test.go`

**Interfaces:**
- Consumes: `stripReasoning` (Task 3), `switched(pctx)` (fit.go), `pinsFor`/`pctx.Shared` store pattern, `pinPrefix`-style key constants.
- Produces: `originPrefix`, `originForeign`, `originKey(pctx *pipeline.Context) string`, `(p *Router) stripForeign(pctx *pipeline.Context, server string)`, and the `modify/reasoning_stripped {blocks: N}` invocation.

- [ ] **Step 1: Write the failing tests**

Add to `strip_test.go` — these drive the whole plugin through `build`/`run` like `plugin_test.go` does:

```go
// A session's first routed request records the server as its reasoning origin and
// strips nothing: nothing foreign can be in the history yet.
func TestRouter_AFirstRoutedRequestRecordsItsOrigin(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := request(store, eteHost, claudeUA, "s1")
	pctx.Body = anthropicBodyWithThinking()
	run(t, p, pctx)
	if v, ok := store.Get(originPrefix + "s1"); !ok || v != "glm" {
		t.Errorf("origin = %v %v, want glm", v, ok)
	}
	assertNoInvocation(t, pctx, "reasoning_stripped")
}

// A request to a server other than the recorded origin strips, and the origin
// goes foreign — permanently, because the client's history keeps the old
// server's blocks however many turns pass on the new one.
func TestRouter_AMovedSessionStripsAndGoesForeign(t *testing.T) {
	store := newStore(t)
	before := build(t, routerConfig(`"claude-code": "glm"`))
	run(t, before, request(store, eteHost, claudeUA, "s1"))

	after := build(t, sessionsConfig(`"claude-code": "glm"`, `"s1": "ete"`))
	pctx := request(store, eteHost, claudeUA, "s1")
	pctx.Body = anthropicBodyWithThinking()
	run(t, after, pctx)
	assertRecord(t, pctx, pipeline.ActionModify, "reasoning_stripped", map[string]string{"blocks": "1"})
	if v, _ := store.Get(originPrefix + "s1"); v != originForeign {
		t.Errorf("origin = %v, want %q", v, originForeign)
	}
	if gjson.Get(string(pctx.Body), "messages.1.content.0.type").String() != "text" {
		t.Errorf("body = %s, thinking still present", pctx.Body)
	}

	// And the next request, same server, still strips: the history is the
	// client's, and it has not forgotten.
	next := request(store, eteHost, claudeUA, "s1")
	next.Body = anthropicBodyWithThinking()
	run(t, after, next)
	assertRecord(t, next, pipeline.ActionModify, "reasoning_stripped", map[string]string{"blocks": "1"})
}

// Steady state on one server keeps its own reasoning: the ordinary routed agent
// keeps its tool loops whole.
func TestRouter_SteadyStateKeepsItsOwnReasoning(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	first := request(store, eteHost, claudeUA, "s1")
	first.Body = anthropicBodyWithThinking()
	run(t, p, first)
	second := request(store, eteHost, claudeUA, "s1")
	second.Body = anthropicBodyWithThinking()
	run(t, p, second)
	assertNoInvocation(t, second, "reasoning_stripped")
}

// Synthetic sessions are never recorded: their ids hold many conversations.
func TestRouter_NoOriginForSyntheticSessions(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := request(store, eteHost, claudeUA, session.DefaultSessionID)
	pctx.Body = anthropicBodyWithThinking()
	run(t, p, pctx)
	if _, ok := store.Get(originPrefix + session.DefaultSessionID); ok {
		t.Error("an origin was recorded for the default bucket")
	}
}
```

And the helpers, at the bottom of the same file:

```go
// anthropicBodyWithThinking is a two-turn Anthropic body whose assistant turn
// carries a signed thinking block.
func anthropicBodyWithThinking() []byte {
	return []byte(`{"model":"claude-opus-5-5","max_tokens":1024,"messages":[
		{"role":"user","content":"Compute 17*23*31."},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"","signature":"sig"},
			{"type":"text","text":"12121"}
		]},
		{"role":"user","content":"Add 1."}
	]}`)
}

// assertNoInvocation fails when pctx holds an invocation with reason. The access
// path is the one assertRecord reads: pctx.Extensions.Invocations.Outbound.
func assertNoInvocation(t *testing.T, pctx *pipeline.Context, reason string) {
	t.Helper()
	invs := pctx.Extensions.Invocations
	if invs == nil {
		return
	}
	for _, inv := range invs.Outbound {
		if inv.Plugin == Name && inv.Reason == reason {
			t.Errorf("unexpected %s invocation: %+v", reason, inv)
		}
	}
}
```

(`session.DefaultSessionID` needs the `session` import, already used by `plugin_test.go`; these tests are in `strip_test.go`, same package.)

- [ ] **Step 2: Run to verify they fail**

Run: `cd core && GOWORK=off go test ./plugins/inferencerouter/ -run 'TestRouter_(AFirst|AMoved|Steady|NoOrigin)' -v`
Expected: FAIL — `originPrefix`/`originForeign` undefined, no stripping happens.

- [ ] **Step 3: Implement**

In `strip.go`:

```go
const (
	// originPrefix is where the router records, per session, the server that
	// produced the reasoning its requests carry. It lives where pins do, with the
	// pin's TTL, and X does not clear it.
	originPrefix = "inference-router/origin/"
	// originForeign marks a session whose reasoning has come from more than one
	// server. It is sticky: the client's history is its own, so blocks from the
	// old server stay in it however many turns pass on the new one, and the origin
	// must never resolve back to a single server.
	originForeign = "(foreign)"
)

// originKey is the store key for pctx's session's origin, "" when it has none a
// record could name.
func originKey(pctx *pipeline.Context) string {
	if pctx.Session == nil || pctx.Session.ID == "" || synthetic(pctx.Session.ID) {
		return ""
	}
	return originPrefix + pctx.Session.ID
}

// stripForeign drops the reasoning a request carries that its target cannot have
// produced, and records the target as the session's reasoning origin. It runs on
// switched requests only — the same predicate as limit fitting — for a session's
// first routed request it records and strips nothing, and once a session's target
// has differed from its recorded origin the origin is foreign for good and every
// switched request strips, the target's own reasoning included: a server that
// cannot verify foreign blocks may not get them, and one that can loses only
// reasoning text, which no measured continuation needed.
func (p *Router) stripForeign(pctx *pipeline.Context, server string) {
	if !switched(pctx) {
		return
	}
	key := originKey(pctx)
	if key == "" {
		return
	}
	if origin := p.origin(pctx, key); origin != "" {
		if origin == server {
			return
		}
		p.keepOrigin(pctx, key, originForeign)
	} else {
		p.keepOrigin(pctx, key, server)
		return
	}
	body, dropped := stripReasoning(pctx.Body)
	if dropped == 0 {
		return
	}
	if pctx.SetBody(body) {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionModify, Reason: "reasoning_stripped",
			Details: map[string]string{"blocks": strconv.Itoa(dropped)}})
	}
}
```

Import `pipeline` in `strip.go` (and `strconv`, already needed by Task 3).

In `plugin.go`'s `OnRequest`, call it immediately after `chooseModel` and **before** `applyLimits`, so limits are fitted to the stripped body:

```go
	p.chooseModel(pctx, name)
	p.stripForeign(pctx, name)
	p.applyLimits(pctx, name, true)
```

Origin storage follows the substitutes' pattern exactly (`models.go:347-400`): the durable store when `p.store != nil` (a plain string value, `p.store.Set(ctx, key, origin, pinTTL)` / `p.store.Get(ctx, key)`), else `pctx.Shared.Put(key, origin, pinTTL)` / `pctx.Shared.Get(key)`. Write it as two small helpers beside `originKey`:

```go
// origin is the session's recorded reasoning origin, "" when none is.
func (p *Router) origin(pctx *pipeline.Context, key string) string {
	if p.store != nil {
		v, err := p.store.Get(context.Background(), key)
		if err != nil {
			return ""
		}
		return v
	}
	if pctx.Shared != nil {
		if v, ok := pctx.Shared.Get(key); ok {
			s, _ := v.(string)
			return s
		}
	}
	return ""
}

// keepOrigin records v as the session's reasoning origin, for as long as a pin is
// kept: the origin outlives a restart beside the pin it explains.
func (p *Router) keepOrigin(pctx *pipeline.Context, key, v string) {
	if p.store != nil {
		if err := p.store.Set(context.Background(), key, v, pinTTL); err != nil {
			slog.Warn("inference-router: could not record a session's reasoning origin", "error", err)
		}
		return
	}
	if pctx.Shared != nil {
		pctx.Shared.Put(key, v, pinTTL)
	}
}
```

and `stripForeign` reads and writes through them, dropping the direct `pctx.Shared` calls sketched above. The Task 4 tests use `newStore(t)` (a `memstore.Store`, which is a `pipeline.SharedStore`), so they read `p.store == nil` — the `pctx.Shared` arm — and `store.Get(originPrefix+"s1")` works as written.

- [ ] **Step 4: Run to verify they pass**

Run: `cd core && GOWORK=off go test ./plugins/inferencerouter/`
Expected: PASS, whole package.

- [ ] **Step 5: Commit**

```bash
git add core/plugins/inferencerouter/strip.go core/plugins/inferencerouter/strip_test.go core/plugins/inferencerouter/plugin.go
git commit -s -m "Feat: Strip foreign reasoning from switched requests, tracked per session"
```

### Task 5: Forwardproxy integration — a session moves mid-run

**Files:**
- Test: `core/listener/forwardproxy/inferencerouter_test.go` (add to it; read its existing helpers first — `inferencerouter_test.go:61` and `:144` show the two-pipeline-over-one-store pattern)

**Interfaces:**
- Consumes: everything from Tasks 1–4.
- Produces: nothing other packages use; this task is the end-to-end gate.

- [ ] **Step 1: Write the failing test**

Add to `inferencerouter_test.go`, modeled on `TestForwardProxy_ASessionRunningBeforeRoutingWasConfiguredStaysOnItsServer` (`:144`) — same `Server` wiring, same `turn` helper, but the origins record bodies too:

```go
// bodyOrigin is keyOrigin that also records each request's body, so a test can
// assert on what the destination received. The slice is mutex-guarded like the
// file's existing `seen` recorder.
func bodyOrigin(t *testing.T, name string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	bodies := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		*bodies = append(*bodies, string(b))
		mu.Unlock()
		_, _ = w.Write([]byte(name))
	}))
	t.Cleanup(srv.Close)
	return srv, bodies
}

// A session moved by a sessions: override lands on the new server with the new
// server's key, its thinking blocks stripped, and stays there on the next turn.
func TestForwardProxy_ASessionsOverrideMovesARunningSession(t *testing.T) {
	ete, eteBodies := bodyOrigin(t, "FROM-ETE")
	glm, glmBodies := bodyOrigin(t, "FROM-GLM")
	eteURL := ete.URL
	glmURL := strings.Replace(glm.URL, "127.0.0.1", "localhost", 1)

	router := inferencerouter.New()
	shared := memstore.New()
	defer shared.Close()
	// (wire Server exactly as the :144 test does: parser holder, session store,
	//  SessionIDHeaders with session.ClaudeCodeSessionHeader, proxy httptest server)

	thinking := `{"model":"claude-opus-5-5","max_tokens":8,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"sig"},{"type":"text","text":"hello"}]},
		{"role":"user","content":"again"}]}`
	turn := func(sessionID, body string) string { /* the :144 turn helper, body parameterized */ }

	// The session starts pinned to ete (agents: claude-code -> ete), one request.
	if got := turn("running", thinking); got != "FROM-ETE" {
		t.Fatalf("first turn reached %q, want ete", got)
	}

	// One reload, the same way agentop writes it: a sessions: entry moves the
	// session to glm, the agents choice unchanged.
	if err := router.Configure(json.RawMessage(`{
		"servers": {"ete": {"url": "` + eteURL + `", "key": "ete-key", "main": "opus", "helper": "haiku"},
			"glm": {"url": "` + glmURL + `", "key": "glm-key", "main": "glm", "helper": "nemotron"}},
		"agents": {"claude-code": "ete"},
		"sessions": {"running": "glm"}
	}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	holder, err := pipeline.New([]pipeline.Plugin{inferenceparser.NewInferenceParser(), router})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	// (swap the server's OutboundPipeline to holder, as the :144 test does on its reload)

	if got := turn("running", thinking); got != "FROM-GLM" {
		t.Fatalf("moved turn reached %q, want glm", got)
	}
	last := (*glmBodies)[len(*glmBodies)-1]
	if strings.Contains(last, `"thinking"`) || strings.Contains(last, "signature") {
		t.Errorf("glm received thinking blocks: %s", last)
	}
	if !strings.Contains(last, `"hello"`) {
		t.Errorf("glm lost the assistant text: %s", last)
	}
	// The pin followed: with the override removed, the next turn stays on glm.
	if err := router.Configure(json.RawMessage(`{ ...the same config with no "sessions"... }`)); err != nil {
		t.Fatal(err)
	}
	if got := turn("running", thinking); got != "FROM-GLM" {
		t.Fatalf("post-override turn reached %q, want glm", got)
	}
}
```

Where the comments say "(wire … as the :144 test does)", copy that test's setup lines verbatim rather than inventing new wiring — the point of this test is the move, not new scaffolding. Use whatever mutex-guarded recorder the file already has (`seen`); extend it with a body slice rather than writing a new one if that is a smaller change.

- [ ] **Step 2: Run to verify it fails**

Run: `cd core && GOWORK=off go test ./listener/forwardproxy/ -run TestForwardProxy_ASessionsOverride -v`
Expected: FAIL — the request stays on ete.

- [ ] **Step 3: There is no step 3** — Tasks 1–4 implement it; this test is the gate. If it fails, the bug is in Tasks 1–4, not here.

- [ ] **Step 4: Run to verify it passes**

Run: `cd core && GOWORK=off go test ./listener/forwardproxy/`
Expected: PASS, whole package.

- [ ] **Step 5: Commit**

```bash
git add core/listener/forwardproxy/inferencerouter_test.go
git commit -s -m "Test: A sessions override moves a running session end to end"
```

### Task 6: agentop — `SessionChanges`, `Verify`, and the prune

**Files:**
- Modify: `cmd/agentop/servers/servers.go`
- Test: `cmd/agentop/servers/servers_test.go` (follow its existing style)

**Interfaces:**
- Consumes: `edit.ConfigChange`/`ScalarValue`, `routerconfig.CheckSessionID` (Task 1), `apiclient`'s `ListSessions`.
- Produces: `SessionChanges(session, server string) []edit.ConfigChange`; `Verify` gains the sessions rules; `PruneSessions(cfg routerconfig.Config, live map[string]bool) []edit.ConfigChange` (removals for override ids `!live[id]`).

- [ ] **Step 1: Write the failing tests**

```go
func TestSessionChanges(t *testing.T) {
	got := SessionChanges("s1", "glm")
	want := []edit.ConfigChange{{
		Chain: "outbound", Plugin: PluginName, Path: []string{"sessions", "s1"},
		Value: edit.ScalarValue("glm"),
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SessionChanges = %+v, want %+v", got, want)
	}
	got = SessionChanges("s1", "")
	want = []edit.ConfigChange{{
		Chain: "outbound", Plugin: PluginName, Path: []string{"sessions", "s1"}, // nil Value removes
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SessionChanges(\"\") = %+v, want %+v", got, want)
	}
}

func TestPruneSessions(t *testing.T) {
	cfg := routerconfig.Config{Sessions: map[string]string{
		"live": "glm", "dead": "glm", "archived": "ete",
	}}
	changes := PruneSessions(cfg, map[string]bool{"live": true, "archived": true})
	if len(changes) != 1 || changes[0].Path[1] != "dead" {
		t.Errorf("PruneSessions = %+v, want one removal for dead", changes)
	}
}

func TestVerifyRefusesABadSessionsEntry(t *testing.T) {
	// Build a *config.Config whose outbound chain holds an inference-router
	// entry with sessions: {"s1": "no-such-server"}, the same way the file's
	// existing Verify tests build one; Verify must return an error naming
	// sessions.s1.
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `cd cmd/agentop && GOWORK=off go test ./servers/`
Expected: FAIL — `SessionChanges`/`PruneSessions` undefined, `Verify` accepts the bad entry.

- [ ] **Step 3: Implement**

In `servers.go`, beside `AgentChanges`:

```go
// SessionChanges is the config write that moves one session to server, "" to
// stop routing it. Unlike AgentChanges there are no retired spellings to fold:
// a session id is opaque.
func SessionChanges(session, server string) []edit.ConfigChange {
	c := edit.ConfigChange{Chain: "outbound", Plugin: PluginName, Path: []string{"sessions", session}}
	if server != "" {
		c.Value = edit.ScalarValue(server)
	}
	return []edit.ConfigChange{c}
}

// PruneSessions is the removals for every sessions: override whose session is
// neither live nor archived — inert config a move left behind. The caller
// decides liveness; this only maps it.
func PruneSessions(cfg routerconfig.Config, live map[string]bool) []edit.ConfigChange {
	var changes []edit.ConfigChange
	for _, id := range slices.Sorted(maps.Keys(cfg.Sessions)) {
		if !live[id] {
			changes = append(changes, edit.ConfigChange{
				Chain: "outbound", Plugin: PluginName, Path: []string{"sessions", id},
			})
		}
	}
	return changes
}
```

In `Verify` (which checks the decoded router entry after `config.Load`), add after its agents rules, matching its error style:

```go
	for _, id := range slices.Sorted(maps.Keys(c.Sessions)) {
		if err := routerconfig.CheckSessionID(id); err != nil {
			return fmt.Errorf("inference-router sessions.%s: %w", id, err)
		}
		if s := c.Sessions[id]; s != "" {
			if _, ok := c.Servers[s]; !ok {
				return fmt.Errorf("inference-router sessions.%s: %q is not a server listed under servers", id, s)
			}
		}
	}
```

- [ ] **Step 4: Run to verify they pass**

Run: `cd cmd/agentop && GOWORK=off go test ./servers/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/agentop/servers/
git commit -s -m "Feat: Write and prune session overrides from agentop"
```

### Task 7: agentop CLI — `agentop server move`

**Files:**
- Modify: `cmd/agentop/cmd_server.go` (dispatch), `cmd/agentop/cmd_server_write.go` (the action)
- Test: `cmd/agentop/cmd_server_test.go` (follow the existing `use`/`reset` tests)

**Interfaces:**
- Consumes: `SessionChanges`, `PruneSessions` (Task 6), `serverTarget`, `readRouter`, `runServerWrite`, `apiclient.ListSessions` (for the cost line and the prune's live set).
- Produces: `serverMove(args []string, stdout, stderr io.Writer) int`; the dispatch case `"move"`.

- [ ] **Step 1: Write the failing tests**

Mirror the existing `use` tests: a temp config with two servers, a `move s1 glm` call asserting the file gained `sessions: {s1: glm}` and the report lines, a `move s1 own` asserting the entry was removed, a `move s1 nope` asserting exit 1 naming the server, and a `move` with no session id asserting exit 2. Include one prune case: a pre-existing `sessions: {dead: glm}` entry is gone from the file after any move (the live set from the fake session API — the tests stub `serverTarget`'s stats URL, so follow how the existing tests make `runningOn` a no-op or a stub).

- [ ] **Step 2: Run to verify they fail**

Run: `cd cmd/agentop && GOWORK=off go test -run TestServerMove -v .`
Expected: FAIL — `move` is an unknown action (exit 2 today).

- [ ] **Step 3: Implement**

In `cmd_server.go`'s dispatch, add `"move"` beside `use`/`reset`. In `cmd_server_write.go`:

```go
// serverMove moves one running session to a server, or stops routing it with
// "own". Unlike use and reset, this moves the session the user named, not the
// agent's next one.
func serverMove(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("server move", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "Cortex config file (default ~/.cortex/config.yaml)")
	pos, code, ok := serverFlags(flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) != 2 {
		fmt.Fprintln(stderr, "agentop server move: want a session and a server: agentop server move <session-id> <name|own>")
		return 2
	}
	session, server := pos[0], pos[1]
	if err := routerconfig.CheckSessionID(session); err != nil {
		fmt.Fprintf(stderr, "agentop server move: %v\n", err)
		return 2
	}
	if server == "own" {
		server = ""
	}
	cfg, path, statsURL, err := serverTarget(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server move: %v\n", err)
		return 1
	}
	c, _, err := readRouter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server move: %v\n", err)
		return 1
	}
	if server != "" {
		if _, ok := c.Servers[server]; !ok {
			fmt.Fprintf(stderr, "agentop server move: no server named %s%s. Add it first:\n  agentop server add %s <url>\n",
				server, configuredServers(c), server)
			return 1
		}
	}
	// The cost line, from the session's own context: everything it carries is
	// re-sent uncached on the first turn at the new server.
	note := moveNote(statsURL, session, server)
	changes := SessionChanges(session, server)
	changes = append(changes, pruneFrom(statsURL, c)...)
	name := server
	if name == "" {
		name = "its own provider"
	}
	return runServerWrite(stdout, stderr, path, statsURL, changes, writeReport{
		done:      fmt.Sprintf("Session %s → %s.", session, name),
		live:      note,
		unchanged: fmt.Sprintf("Session %s already goes to %s.", session, name),
	})
}
```

`moveNote` reads the session's `promptContext` through `apiclient` (as `runningOn` reads the list) and returns "" when the proxy or the session is not visible — the move does not depend on it. `pruneFrom` builds the live set the same way and returns `PruneSessions(c, live)`; with no reachable session API it returns nothing (prune only what you can see is dead).

- [ ] **Step 4: Run to verify they pass**

Run: `cd cmd/agentop && GOWORK=off go test .`
Expected: PASS, whole module tests.

- [ ] **Step 5: Commit**

```bash
git add cmd/agentop/cmd_server.go cmd/agentop/cmd_server_write.go cmd/agentop/cmd_server_test.go
git commit -s -m "Feat: Move a running session with agentop server move"
```

### Task 8: agentop TUI — `S` on a session row

**Files:**
- Modify: `cmd/agentop/tui/keys.go` (`S` on the sessions pane; today it is a no-op there, `keys.go:851-852`)
- Modify: `cmd/agentop/tui/server_picker.go` (a session-targeted variant of `applyServerChoice` and the picker title)
- Modify: `cmd/agentop/tui/sessions_pane.go` (the read-only comment at `:317-318` is replaced)
- Test: `cmd/agentop/tui/` — follow the existing picker test file (find it with `grep -rl "openServerPicker" cmd/agentop/tui/*_test.go`)

**Interfaces:**
- Consumes: `SessionChanges` (Task 6), `edit.WritePluginConfig`, the existing picker overlay, the sessions pane's highlighted row (its session id and `promptContext`).
- Produces: no new exported API; the keybinding and the confirmation text.

- [ ] **Step 1: Write the failing tests**

First **delete** `TestSessionsTable_NoKeyChangesASessionsServer` (`sessions_server_test.go:111`) — it pins the read-only behavior this task replaces; its replacement is below. Then add to `server_picker_test.go` (reusing `newFakeProxy`, `keyRune`, `keyEnter`, `keyDown`, `keyUp`, `serverCellOf`, and a sessions-row cursor helper modeled on `onRow` — the sessions rows are addressed by `m.sessionRowIDs`):

```go
// sessionsServerModelOver is serverModel's wiring on the SESSIONS pane, so S is
// pressed on a session row, with serverSessions() as its rows.
func sessionsServerModelOver(t *testing.T, f *fakeProxy) *model {
	t.Helper()
	m := serverModel(t, f) // AGENTS pane wiring, config path and stats URL included
	m.pane = paneSessions
	m.sessions = serverSessions()
	m.rebuildSessionsTable()
	return m
}

// onSessionRow puts the sessions cursor on id's row.
func onSessionRow(t *testing.T, m *model, id string) {
	t.Helper()
	for i, rid := range m.sessionRowIDs {
		if rid == id {
			m.sessionsTbl.SetCursor(i)
			return
		}
	}
	t.Fatalf("no sessions row %q in %v", id, m.sessionRowIDs)
}

// S on a session row opens the picker titled for that session, and ↵ writes a
// sessions: entry for it — not an agents: choice.
func TestServerPicker_EnterOnAServerMovesTheSession(t *testing.T) {
	f := newFakeProxy(t, false)
	m := sessionsServerModelOver(t, f)
	onSessionRow(t, m, "on-glm")
	m.handleKey(keyRune('S'))
	p := m.serverPicker
	if p == nil {
		t.Fatalf("S on a session row opened no picker; flash %q", m.flash)
	}
	if !strings.Contains(m.View(), "Session on-glm uses") {
		t.Errorf("the picker is not titled for the session:\n%s", m.View())
	}
	cmd := m.handleKey(keyEnter)
	if cmd == nil {
		t.Fatalf("↵ started no write; flash %q", m.flash)
	}
	if _, refetch := m.Update(cmd()); refetch != nil {
		m.Update(refetch())
	}
	if !strings.Contains(f.config(t), "on-glm: ete") {
		t.Errorf("config:\n%s", f.config(t))
	}
	if m.flash != "Session on-glm → ete." {
		t.Errorf("flash %q", m.flash)
	}
}

// S on the agents pane still writes an agents: choice — the regression guard.
func TestServerPicker_SOnTheAgentsPaneStillRoutesTheAgent(t *testing.T) {
	f := newFakeProxy(t, false)
	m := serverModel(t, f)
	switchTo(t, m, "opencode/1.0.3", "glm")
	if !strings.Contains(f.config(t), "opencode: glm") {
		t.Errorf("config:\n%s", f.config(t))
	}
}

// The confirmation states the cache cost from the session's promptContext.
func TestServerPicker_AMoveStatesTheUncachedContext(t *testing.T) {
	f := newFakeProxy(t, false)
	m := sessionsServerModelOver(t, f)
	m.sessions[0].PromptContext = 81234 // field name: check SessionSummary; adjust to the real one
	onSessionRow(t, m, "on-glm")
	m.handleKey(keyRune('S'))
	if !strings.Contains(m.View(), "81234 tokens of context go to ete uncached on the first turn") &&
		!strings.Contains(m.View(), "81,234 tokens") {
		t.Errorf("the picker states no cache cost:\n%s", m.View())
	}
}
```

(Check `session.SessionSummary`'s field name for the context figure before writing the third test — the sessions list carries it, `promptContext` in the wire JSON — and use the Go name it has; the number formatting follows whatever the sessions table's TOKENS column already uses, so reuse its formatter rather than a new one.)

- [ ] **Step 2: Run to verify it fails**

Run: `cd cmd/agentop && GOWORK=off go test ./tui/ -run TestServerPicker -v` (adjust the -run to the file's naming)
Expected: FAIL — `S` on the sessions pane is a no-op.

- [ ] **Step 3: Implement**

- In `server_picker.go`, generalize `applyServerChoice` into two entry points or add a `target` struct (`kind: agent|session`, `name`), so the picker writes `AgentChanges` for an agent and `SessionChanges` for a session. The picker's title becomes `"Session <id8>… uses"` for a session (`<id8>` = first 8 chars of the id — full ids do not fit the overlay). The option list gains "own provider (unrouted)" for the `""` value; for an agent target, keep today's wording.
- The confirmation/notice panel, session target only, states the cache cost from the row's `promptContext`: `"N tokens of context go to <server> uncached on the first turn."` — reuse `sessionsStaying`'s counting shape for the flash text after the write lands (`"Session <id8> moved to <server>."`).
- In `keys.go`, route `S` on the sessions pane to `openSessionServerPicker` with the highlighted row; on the agents pane, unchanged. In `sessions_pane.go`, replace the read-only comment with one naming the picker, and expose the highlighted row's id and `promptContext` if the picker does not already receive the row.

- [ ] **Step 4: Run to verify they pass**

Run: `cd cmd/agentop && GOWORK=off go test ./tui/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/agentop/tui/
git commit -s -m "Feat: Move a session to a server from the sessions pane"
```

### Task 9: Docs

**Files:**
- Modify: `docs/inference-router-plugin.md` — the `sessions` field in the config block; a "Moving a running session" section (the override, the pin following it, the strip and its origin record, `pin: moved` and `reasoning_stripped` in the timeline); the plugin-state key list gains `origin`.
- Modify: `docs/agents/claude-code.md` — `S` on a session row and `agentop server move` beside the existing routing section; the cache-cost warning; the thinking-strip note (what is lost: nothing for Anthropic-origin turns, readable text for glm/ollama-origin ones).
- Modify: `cmd/agentop/README.md` — the `move` action in the `server` command table; `S` in the sessions-pane keybindings.
- Modify: `docs/plugin-catalog.md` — the inference-router entry gains `sessions`.
- Modify: `docs/laptop-service.md` — the plugin-state paragraph's key list gains the origin key, one sentence.

- [ ] **Step 1: Write the docs**

Each edit states behavior the tests pin; quote nothing this plan or the spec does not say. In `claude-code.md`, keep the existing "sessions already running stay where they are" sentences for `use`/`reset` — they are still true — and add the move as the deliberate exception, with the spec's reason (an explicit user instruction, not a guess).

- [ ] **Step 2: Check the spellcheck and links**

Run: `grep -rn "reasoning_stripped\|pin: moved\|server move" docs/ cmd/agentop/README.md` and confirm every mention matches the implemented reason strings.
Expected: consistent strings, no stale wording.

- [ ] **Step 3: Commit**

```bash
git add docs/ cmd/agentop/README.md
git commit -s -m "Docs: Document moving a running session to another server"
```

---

## Final checks

- [ ] `cd core && GOWORK=off go test ./...` — green.
- [ ] `cd cmd/agentop && GOWORK=off go test ./...` — green.
- [ ] `gofmt -l core/plugins/inferencerouter core/listener/forwardproxy cmd/agentop` — empty.
- [ ] `cd core && GOWORK=off go vet ./plugins/inferencerouter/... ./listener/forwardproxy/...` and `cd cmd/agentop && GOWORK=off go vet ./...` — clean.
- [ ] `go mod tidy -diff` in `core` and `cmd/agentop` — empty (no dependency changed, but the gate runs on it).
- [ ] PR title `Feat: Move a running session to another inference server`, body ending `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`, deferred list for what review finds.

## Live verification (before merge, with Hai — restarts the shared proxy)

`make dev-install` restarts the one proxy every session shares (see CLAUDE.md). With Hai's go-ahead:

1. Pick a live Claude Code session on glm; `agentop server move <id> ete-int`.
2. Confirm the next turn reaches ete-int (sessions pane SERVER column), the timeline shows `pin: moved` and `reasoning_stripped`, and the conversation continues with its context intact (the code-word test).
3. `agentop server move <id> own` — the next turn goes to the client's own provider.
4. Move a glm session with thinking blocks to ete-ext (the Bedrock strict one) — the strip must keep it off the 403.
