# agentop names a session's server by name (#1978) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** agentop names a session's server from `SessionSummary.InferenceServer`, and `agentop server add` accepts a second server on one host.

**Architecture:** One new helper, `servers.ForSession`, returns the summary's `InferenceServer` when the router config has a server of that name, and else falls back to `servers.ForHost`. The four places that name a session's server call it. The same-host refusal in `server add` is deleted.

**Tech Stack:** Go 1.26.10, module `cmd/agentop`.

**Spec:** issue #1978 (part A2 of #1975). A1 (#1977, PR #1983) is merged: `core/session/summary.go:188` has `InferenceServer model.LenientString \`json:"inferenceServer,omitempty"\``, and the router accepts two servers on one URL.

## Global Constraints

- `ForHost` stays. It is the fallback for a summary from a proxy that has no `inferenceServer` yet.
- A summary with no `inferenceServer` gives the same answer as today, at every call site.
- The SERVER cell keeps its trust rules (`tui/server_route.go:150-164`): a server name goes through `sanitizeLabel`, and a host is shown only when `isHostPort` accepts it.
- Code comments, commit messages and the PR body use ASD-STE100 Simplified Technical English. Commits use `git commit -s`.
- Test commands: `scripts/dev/go-test-shards.sh -short cmd/agentop` while you work. Before the PR: `scripts/dev/go-test-shards.sh -race cmd/agentop`, and from `cmd/agentop`: `go test -race $(go list ./... | grep -v 'agentop$')`.

## Decisions

- **Signature:** `func ForSession(c routerconfig.Config, s session.SessionSummary) (name string, ok bool)` in `cmd/agentop/servers/servers.go`. A value, not a pointer, because every caller has a value or a slice element, and the call sites stay one line. `servers` may import `github.com/rossoctl/cortex/core/session`: core does not import agentop, so there is no cycle.
- **An `InferenceServer` that names no configured server** (the server was removed or renamed) falls back to `ForHost`. Reason: the issue says "else ForHost", and a stale name must not show as a configured server.
- **`ForSession` does not check `InferenceHost` when `InferenceServer` names a server.** `sessionServerCell` keeps its `host == ""` check first, so a session with no host still shows `emptyCell`. Reason: A1 sets the two fields together, and the cell's empty rule belongs to the cell.
- **`sessionServerCell` takes `s session.SessionSummary`** in place of `host string`: `sessionServerCell(router routerconfig.Config, s session.SessionSummary, w int) string`. It reads `s.InferenceHost` where it read `host`.

## Review Focus

1. A summary with `inferenceServer` that names a removed server: the cell must not show the stale name. Pinned by T1 and T3 tests (case "gone").
2. Two servers on one host and an unrouted session (`inferenceServer` empty): the answer is `ForHost`'s, the first name in sorted order, as today. Pinned by T1, T2 and T3 tests.
3. A hostile `InferenceHost` with a valid `InferenceServer`: the cell shows the sanitised server name and no part of the host. Pinned by the T3 test case with host `sk-ant-api03-secret:x@evil.com` and server `team-b`.
4. A control character in `InferenceServer` that is also a configured name: the cell still sanitises it, because the name goes through `sanitizeLabel`. Pinned by the existing `"\x1b[31mglm"` case in T3.
5. `server add` replace of an existing name on its own URL still asks first. Pinned by the existing `TestServerAdd_ReplacingAsksFirst`, which must still pass.

---

### Task T1: `servers.ForSession`

**Depends on:** none.

**Files:**
- Modify: `cmd/agentop/servers/servers.go:153-170`
- Test: `cmd/agentop/servers/servers_test.go`

**Interfaces:**
- Produces: `func ForSession(c routerconfig.Config, s session.SessionSummary) (name string, ok bool)`.

- [ ] **Step 1: Write the failing test** `TestForSession` in `servers_test.go`. Config: `team-a` and `team-b`, both `URL: "https://shared.example.com"`. Table (summary → want name, ok):
  - `{InferenceHost: "shared.example.com", InferenceServer: "team-b"}` → `"team-b", true`
  - `{InferenceHost: "shared.example.com"}` → `"team-a", true` (same as `ForHost`)
  - `{InferenceHost: "shared.example.com", InferenceServer: "gone"}` → `"team-a", true`
  - `{InferenceHost: "other.example.com"}` → `"", false`
  - `{}` → `"", false`
- [ ] **Step 2: Run** `cd cmd/agentop && go test ./servers -run TestForSession`. Expect FAIL: `ForSession` is undefined.
- [ ] **Step 3: Implement `ForSession`** next to `ForHost`. Use `c.Servers[string(s.InferenceServer)]` with the comma-ok form, else `return ForHost(c, s.InferenceHost)`.
- [ ] **Step 4: Rewrite the `ForHost` comment at `servers.go:155-157`.** The sentence "Unambiguous because the router refuses two servers on one host" is now false. Say: with two servers on one host, `ForHost` returns the first name in sorted order, so to name a session's server, use `ForSession`. Also give `ForSession` a doc comment that says the fallback is for a proxy that does not send `inferenceServer`.
- [ ] **Step 5: Run** `go test ./servers`. Expect PASS. Commit.

### Task T2: `server add` accepts a second server on one host, and `server remove` counts by name

**Depends on:** T1.

**Files:**
- Modify: `cmd/agentop/cmd_server_write.go:144-150` (delete), `:435` (`runningOn`)
- Test: `cmd/agentop/cmd_server_write_test.go:162` and near `:685`

**Interfaces:**
- Consumes: `servers.ForSession` (T1).

- [ ] **Step 1: Replace `TestServerAdd_RefusesASecondServerOnOneHost`** with `TestServerAdd_AddsASecondServerOnOneHost`. Run `add east https://ete.example.com --main opus --helper haiku --key-stdin --yes` against `routerBlock` (which has `ete` on `https://ete.example.com`), set up as `TestServerAdd_WritesMainAndHelperAndSaysWhatTheyName` sets up its model list. Assert exit 0, and that the written config has both `ete` and `east`, each with URL `https://ete.example.com`.
- [ ] **Step 2: Add `TestServerRemove_CountsTheSessionsByServerName`.** Router block: `ete` and `team-b` both on `https://ete.example.com`, `glm` on `https://glm.example.com:8443`, `agents: {claude-code: glm}` (so `remove` of `ete` or `team-b` is not refused). Sessions API: one request routed to `ete.example.com` with server `team-b`, one routed with server `ete`, one unrouted to `ete.example.com`. Add a test helper `routedInferenceTo(host, server string) pipeline.SessionEvent` beside `inferenceTo` (`cmd_server_write_test.go:642`). It adds `Invocations: &pipeline.Invocations{Outbound: []pipeline.Invocation{{Plugin: "inference-router", Action: pipeline.ActionModify, Reason: "routed", Phase: pipeline.InvocationPhaseRequest, Details: map[string]string{"server": server}}}}`, as `core/session/hosts_test.go:41-47` does. Extend `sessionsAPI` only if that is shorter than a new store in the test. Assert: `remove team-b` warns of `1 running session` (singular text as in `TestServerRemove_WarnsOfOneSessionInTheSingularAndShowsNoCredential`). In a fresh config, `remove ete` warns of `2 running sessions` (the routed `ete` one and the unrouted one, which `ForHost` gives to `ete`, first in sorted order).
- [ ] **Step 3: Run** `scripts/dev/go-test-shards.sh -short cmd/agentop`. Expect both tests to FAIL.
- [ ] **Step 4: Delete the refusal** at `cmd_server_write.go:144-150` (comment and `if` block). In `runningOn`, change `servers.ForHost(c, s.InferenceHost)` to `servers.ForSession(c, s)`. Update the `runningOn` doc comment if it says the count is by host.
- [ ] **Step 5: Run** the shards again. Expect PASS. Commit.

### Task T3: the TUI names a session's server by name

**Depends on:** T1.

**Files:**
- Modify: `cmd/agentop/tui/server_route.go:150-176` (`sessionServerCell`), `cmd/agentop/tui/sessions_pane.go:330-332` (both callers), `cmd/agentop/tui/server_picker.go:230` and `:485`
- Test: `cmd/agentop/tui/sessions_server_test.go`, `cmd/agentop/tui/server_picker_test.go`

**Interfaces:**
- Consumes: `servers.ForSession` (T1).
- Produces: `sessionServerCell(router routerconfig.Config, s session.SessionSummary, w int) string`.

- [ ] **Step 1: Change `TestSessionServerCell_ShowsOnlyAHostOfAHost`** (`sessions_server_test.go:169`) to pass `session.SessionSummary{InferenceHost: host}`. Its expectations do not change.
- [ ] **Step 2: Add `TestSessionServerCell_NamesTheSessionsServer`.** Router: `team-a` and `team-b` on `https://shared.example.com`. Cases (summary → cell at `sessionsServerWidth`):
  - host `shared.example.com`, server `team-b` → `team-b`
  - host `shared.example.com`, no server → `team-a`
  - host `shared.example.com`, server `gone` → `team-a`
  - no host, server `team-b` → `emptyCell`
  - host `sk-ant-api03-secret:x@evil.com`, server `team-b` → `team-b`, and the cell does not contain `secret`
- [ ] **Step 3: Add `TestServerPicker_OpensOnTheSessionsNamedServer`** beside `TestServerPicker_OpensOnTheSessionInForceServer` (`server_picker_test.go:825`). Give the model a router with `team-a` and `team-b` on `https://shared.example.com`, the way the nearby tests build theirs, and a session row with host `shared.example.com` and `InferenceServer: "team-b"`. Press `S` on that row. Assert `p.entries[p.cursor].name == "team-b"`.
- [ ] **Step 4: Add `TestServerPicker_TheFlashCountsBySessionServerName`** beside `TestServerPicker_TheFlashCountsTheRunningSessionsThatStay` (`:209`). Router as in step 3, plus a third server to switch to. Sessions for `claude-code`: one with server `team-b`, one with server `team-a`, both on host `shared.example.com`. Switch `claude-code` to `team-b`. Assert the flash says `1 running session stays where it is.` (use the exact singular text that `switchedText` writes).
- [ ] **Step 5: Run** `scripts/dev/go-test-shards.sh -short cmd/agentop`. Expect a compile failure, then FAIL.
- [ ] **Step 6: Implement.** `sessionServerCell` takes the summary and calls `servers.ForSession(router, s)`. Both callers in `sessions_pane.go` pass `s`. `server_picker.go:230`: `inForce, _ = servers.ForSession(router, *summary)`. `server_picker.go:485`: `on, ok := servers.ForSession(router, s)`. Update the `sessionServerCell` doc comment: the cell names the session's server, from its name when the proxy sent one, else from its host.
- [ ] **Step 7: Run** the shards again. Expect PASS. Commit.

### Task T4: README

**Depends on:** none.

**Files:**
- Modify: `cmd/agentop/README.md:1297-1299`

- [ ] **Step 1:** Delete the sentence "A server on a host another server already has, on any port, is refused, except on a loopback host such as `localhost`, where each port is a host of its own." In its place, say: two servers can have one URL (for example, two teams' keys for one gateway), and agentop names a session's server from the server name the proxy records for the session. A proxy older than this change does not record that name, and for it agentop names the server from the host, so with two servers on one host it shows the first name in alphabetical order.
- [ ] **Step 2:** Commit.

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: agentop names a session's server by name, and adds a second server on one URL` | (none) | T1, T2, T3, T4 | 260 |

Estimates: T1 60, T2 90, T3 100, T4 10. The total is under 2000, so there is one PR.

## For a human after review

- Open the PR. The body says `Part of #1975` and `Closes #1978`. Needs T1-T4.
- On a laptop install, after the PR merges: run `agentop server add team-b <url of an existing server> …`, send one request through each, and check the SERVER cell in the Sessions pane. Needs T1-T4. This uses real credentials, so it is not a task.
