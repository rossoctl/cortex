# Attribute a headless agent run to the session that started it (#1991) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `claude -p` run that another session starts is filed under the project of the session that started it, and agentop shows which session started it.

**Architecture:** Attribution already computes `ChainSession` (the topmost ancestor's live session). It now also names the process that holds it (`ChainProc`). The forward proxy turns that into a parent session and a parent project on the pipeline context. Each event carries both. Every reader that groups by project reads `RollupProject()`. The session summary carries `parentSession`, and agentop marks those sessions.

**Tech Stack:** Go 1.26.10. Modules `core/` and `cmd/agentop/`. One line list in the Rust crate `praxis/filters`.

**Spec:** issue rossoctl/cortex#1991. Read it with `gh issue view 1991 --repo rossoctl/cortex`. The section numbers below ("issue §3") are that issue's.

## Global Constraints

- No rule in `core/attribution/process.go` changes. Which session a row is filed in does not change. Only the project it rolls up to changes.
- `Project` on an event keeps the row's own directory. `ParentProject` is the extra field.
- The parent applies only when `ChainSession != ""` and `ChainSession != <the row's session id>`.
- No parent on the trusted path (vmcontrol), with `chain == nil`, or with `!s.processesOn()`.
- `SessionSummary.ParentSession` is never set on the default bucket or on a pending bucket.
- The archive does not intern the new fields.
- No backfill of old data. No merge of child cost into the parent session's totals.
- Comments and docs in ASD-STE100 Simplified Technical English. Commits use `git commit -s` and end with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. No `Co-Authored-By`.
- Go commands per module with `GOWORK=off`. `gofmt -l <path>` always with a path.

## Decisions made in this plan

1. **No change to `attribution.Wire` or `hop.go`.** The issue says hop.go carries both values "the same way it carries ChainSession and Project". hop.go carries no `Project` today: the praxis gateway does not set `project` (`praxis/filters/src/record/line.rs`, `NOT_SET_IN_PRAXIS_MODE`). So the gateway does not set the parent fields either. T2 adds `"parentSession"` and `"parentProject"` to that list, next to `"project"`, under the same comment. Reason: the Rust test `every_session_event_field_is_set_or_listed` fails on a new Go event field that is neither set nor listed.
2. **Refusal rows get the parent too.** `attributeForRecording` (`identity.go:76`) sets the project for rows that never reach hydration. It also sets the parent, with the record id it returns as the row's session id. Reason: a refusal row with no parent puts the child's own directory into the child session's `Projects`, and then the agents pane counts the session under the child directory again.
3. **One helper for both paths:** `func (s *Server) parentFor(trusted string, m *attribution.Memo, chain []attribution.Proc, at attribution.Attribution, sid string) (session, project string)` in `identity.go`. Both `hydrateSession` and `attributeForRecording` call `pctx.SetParent(s.parentFor(...))` right after `SetProject`, always (also with `"", ""`), so the write-once slot is pinned as `SetProject` pins it.
4. **The parent project is memoised on the connection** with a new `Memo.ParentProject(compute func() string) string` in `core/attribution/conn.go`, with its own `sync.Once`, the same shape as `Memo.Project`. It is computed only when the parent applies. When no chain entry has `Key() == at.ChainProc`, the project is `""` and `ProjectOf` is not called. `s.ProjectOf == nil` or `m == nil` gives `""` project; the parent session still applies.
5. **`RollupProject` returns `ParentProject` when it is not `""`, else `Project`.** So a parent with no known project falls back to the row's own project.
6. **`ParentSession` on the summary: first non-empty wins.** Helper `func applyParent(cur, sessionID, parent string) string` in `core/session/fold.go`, next to `applyProject`: returns `cur` when `cur != ""`, when `parent == ""`, or when `sessionID` is `DefaultSessionID` or has `PendingPrefix`; else `parent`. The store's append, the store's rekey loop (`store.go:989`), and `SummaryFold` all call it. In the history merge (`history.go`, after the projects merge), the earlier history's `parentSession` wins when it is not `""`, because it is earlier.
7. **agentop marker:** `const childMarker = "↳ "` in `cmd/agentop/tui/sessions_context.go`, next to `importedMarker`. In `sessions_pane.go`, the imported check stays first; `else if s.ParentSession != ""` sets `mark = childMarker`. `markW` becomes `lipgloss.Width(mark)` for both marks (it is `len()` today, and `↳` is 3 bytes but 1 column). An imported session never has a parent, so the order does not matter in practice.
8. **agentop header:** a new `func (m *model) parentSessionOf(id string) string` in `cmd/agentop/tui/session_metadata.go` next to `servedTitle`, same loop over `m.sessions`. In `sessionHeader`, when it is not `""`, append `" · started by " + p` to `tail` after the `suffix` part and before `m.scopeSuffix(...)`, where `p` is `sanitizeLabel(parent)` cut to its first 8 runes. Reason for `sanitizeLabel`: the id comes from a client header.
9. **Known limit, no code:** the transcript importer's live-row check (`core/session/importer/importer.go:272`) matches ledger rows by minute and project. A child's live ledger rows now carry the parent's project, so an import of the child's transcript after its archive copy is pruned can count those turns again. Rule 1 of the importer skips any session that has live events, so this needs an archive prune first. Accepted; the review must not ask to fix it here.

## Review Focus

1. A tool process (for example `git`) bound to the parent session: `ChainSession == sid`, so no parent fields (T3 test `TestParent_AToolOnTheParentSessionCarriesNone`).
2. A pending row of the child before it names a session: it carries the parent, and after the rekey into the named session that session's `Projects` and `ParentSession` are the parent's (T3 test `TestParent_PendingRowsCarryTheParent`, T4 test `TestStoreRekey_ParentSession`).
3. A keep-alive connection: `ProjectOf` is called at most once for the parent per connection (T3 test `TestParent_ProjectOncePerConnection`).
4. A parent whose session was deleted (evicted): `ChainSession` and `ChainProc` are both `""` (T1 test).
5. A parent id with an escape sequence: the header shows no control byte (T5 test `TestSessionHeader_StartedBySanitises`).

---

### Task T1: Attribution names the process that holds the chain session

Depends on: none

**Files:**
- Modify: `core/attribution/process.go:162-181` (`chainSessionLocked`)
- Modify: `core/attribution/attribution.go:67-69` (field), `:161-163` (`Resolve`)
- Test: `core/attribution/attribution_test.go` (next to `TestResolve_ChainSessionIsTheTopmostAncestorsClaim`, line 235)

**Interfaces:**
- Produces: `Attribution.ChainProc string` — the `Proc.Key()` of the chain entry whose claim gave `ChainSession`; `""` when `ChainSession` is `""`. Doc comment: "ChainProc is the process that holds ChainSession, as Proc.Key. "" when ChainSession is ""."
- `func (a *Attributor) chainSessionLocked(chain []Proc, now time.Time) (string, int)` returns the session and the chain index; index `-1` with `""`. Its only caller is `Resolve` (`attribution.go:162`).

- [ ] **Step 1: Write the failing test** `TestResolve_ChainProcIsTheProcessThatHoldsTheChainSession`, with the helpers `newTest`, `req`, `pchain`, `record`, `claudeUA` from this file:
  - Parent: `a.Resolve(req(claudeUA, "s1"), pchain(100, 50))`, `record(a, clk, "s1")`.
  - Child chain `pchain(400, 300, 200, 100, 50)` (child agent 400, two shells 300 and 200, parent 100). `a.Resolve(req(claudeUA, "c1"), child)`, `record(a, clk, "c1")`. Assert `Session == "c1"`, `ChainSession == "s1"`, `ChainProc == pchain(100)[0].Key()`.
  - A tool under the child, `pchain(500, 400, 300, 200, 100, 50)`, `a.Resolve(nil, …)`: `ChainSession == "s1"`, `ChainProc == pchain(100)[0].Key()`.
  - A plain agent with no claiming ancestor, `pchain(700, 70)` with `req(claudeUA, "x1")`: `ChainSession == ""` and `ChainProc == ""`.
  - `a.Evicted("s1")`, then the child again: `ChainSession == ""` and `ChainProc == ""`.
- [ ] **Step 2:** Run `cd core && GOWORK=off go test ./attribution/ -run ChainProc`. Expected: FAIL (unknown field `ChainProc`).
- [ ] **Step 3:** Implement. `Resolve` sets `out.ChainSession, i := …` and, when `i >= 0`, `out.ChainProc = chain[i].Key()`.
- [ ] **Step 4:** Run `cd core && GOWORK=off go test ./attribution/`. Expected: PASS.
- [ ] **Step 5:** Commit: `feat: Name the process that holds the chain session`.

### Task T2: Context and event carry the parent session and project

Depends on: none

**Files:**
- Modify: `core/pipeline/attributed.go` (add `SetParent`, `ParentSession`, `ParentProject` after `Project`)
- Modify: `core/pipeline/context.go:180-182` (fields `parentSession`, `parentProject string`, `parentSet bool`)
- Modify: `core/model/session.go` (`SessionEvent` fields after `Project`, line 470; wire struct line 724; `MarshalJSON` and `UnmarshalJSON`; method `RollupProject`)
- Modify: `core/listener/internal/sessionevent/sessionevent.go:79,123,164,210,245` (each builder that sets `Project: pctx.Project()` also sets `ParentSession: pctx.ParentSession(), ParentProject: pctx.ParentProject()`)
- Modify: `core/pipeline/session_golden_test.go:85` (`goldenEvent` sets `ParentSession: "s-parent"`, `ParentProject: "~/src/parent"`), then rewrite `core/pipeline/testdata/session-event.golden.json` with `-update`
- Modify: `praxis/filters/src/record/line.rs:43-44` (add `"parentSession",` and `"parentProject",` after `"project",`; change the comment above to "The Go listener sets these four from its pipeline context. The gateway does not.")
- Test: `core/pipeline/attributed_test.go` (create), `core/model/session_wire_test.go` (add; its reflection tests `TestSessionEventWire_HasEveryDomainField` and `TestSessionEventWireCoversEveryField` also check that the wire struct gets both fields), `core/listener/internal/sessionevent/sessionevent_test.go:445` (`TestBuilders_CopyProject`)

**Interfaces:**
- Produces: `func (c *Context) SetParent(session, project string)` — FIRST CALL WINS, own flag, like `SetProject`. `func (c *Context) ParentSession() string`, `func (c *Context) ParentProject() string`.
- Produces: `SessionEvent.ParentSession string`, `SessionEvent.ParentProject string`, wire tags `json:"parentSession,omitempty"` and `json:"parentProject,omitempty"`, with the doc comments from issue §3. `func (e *SessionEvent) RollupProject() string` (Decision 5).

- [ ] **Step 1: Write the failing tests**
  - `TestSetParent_FirstCallWins`: `SetParent("p1", "~/a")` then `SetParent("p2", "~/b")` gives `"p1"`, `"~/a"`. `SetParent("", "")` first, then `SetParent("p2", "~/b")`, gives `""`, `""`.
  - `TestSessionEvent_RollupProject`: `{Project: "~/c", ParentProject: "~/p"}` gives `"~/p"`; `{Project: "~/c"}` gives `"~/c"`; `{}` gives `""`.
  - `TestSessionEvent_ParentFieldsRoundTrip`: marshal an event with both fields, unmarshal, both equal; the JSON contains `"parentSession":"s-parent"`. An event with neither has no `parentSession` key.
  - Extend `TestBuilders_CopyProject`: also `pctx.SetParent("s-parent", "~/parent")`; every builder's event has both values.
- [ ] **Step 2:** Run `cd core && GOWORK=off go test ./pipeline/ ./model/ ./listener/internal/sessionevent/`. Expected: FAIL (undefined).
- [ ] **Step 3:** Implement. Then run `cd core && GOWORK=off go test ./pipeline/ -run TestSessionEventWire_Golden -update` once, and check the golden diff adds only the two keys.
- [ ] **Step 4:** Run `cd core && GOWORK=off go test ./pipeline/ ./model/ ./listener/...` and `cd praxis && cargo test --locked -p cortex-praxis-filters`. Expected: PASS. If a reflection test such as `TestDeny_PopulatesEveryFieldTheContextCarries` reports the new fields as unset, set them in that test's context.
- [ ] **Step 5:** Commit: `feat: Carry the parent session and project on each event`.

### Task T3: Forward proxy sets the parent on the context

Depends on: T1, T2

**Files:**
- Modify: `core/attribution/conn.go:44-46,83-90` (`Memo` fields `parentOnce sync.Once`, `parentProject string`; method `ParentProject`)
- Modify: `core/listener/forwardproxy/identity.go` (`parentFor` after `projectFor`; calls in `attributeForRecording` line 83 and `hydrateSession` line 180)
- Modify: `core/listener/forwardproxy/websocket.go:841` (add `pctx.SetParent(hs.ParentSession(), hs.ParentProject())`)
- Test: `core/listener/forwardproxy/parent_test.go` (create; reuse `newFakeProcs`, `fproc`, `newProcessProxy`, `sendAs`, `procClaudeUA` from `project_test.go` and the files it uses)

**Interfaces:**
- Consumes: `Attribution.ChainProc` (T1); `pctx.SetParent` (T2).
- Produces: `func (m *Memo) ParentProject(compute func() string) string`; `parentFor` (Decision 3). Rule: return `"", ""` when `trusted != ""`, `chain == nil`, `!s.processesOn()`, `at.ChainSession == ""` or `at.ChainSession == sid`. Else return `at.ChainSession` and the project from Decision 4. In `hydrateSession` `sid` is the `PluginSession` answer, so move the `SetParent` call after that line. In `attributeForRecording` `sid` is the `RecordSession` answer.

- [ ] **Step 1: Write the failing tests.** Build a process tree in the fake: parent claude `fproc(100, 50, "/bin/claude")`, shells `fproc(200, 100, "/bin/bash")`, `fproc(300, 200, "/bin/bash")`, child claude `fproc(400, 300, "/bin/claude")`, a tool `fproc(500, 100, "/usr/bin/git")`. Use a map-based `ProjectOf`: 100 → `"~/parent"`, 400 → `"~/child"`, else `"~/other"`, which counts calls. The parent sends one request with session header `s1` first.
  - `TestParent_ChildRowsCarryTheParent`: the child sends with header `c1`. The request and response rows under `c1` have `Project == "~/child"`, `ParentSession == "s1"`, `ParentProject == "~/parent"`.
  - `TestParent_PendingRowsCarryTheParent`: the child sends with no header first. The row in its pending bucket has `ParentSession == "s1"` and `ParentProject == "~/parent"`.
  - `TestParent_TunnelRowsCarryTheParent`: a CONNECT from the child (as the existing tunnel project tests do); the tunnel rows carry both values.
  - `TestParent_AToolOnTheParentSessionCarriesNone`: pid 500 sends with no header; its row is in `s1` and has both fields `""`.
  - `TestParent_TrustedPathCarriesNone`: `s.TrustedSessionID` returns `"vm-1"`; the child's row has both `""`.
  - `TestParent_ProjectOncePerConnection`: the child sends two requests on one connection; `ProjectOf` is called for pid 100 once.
  - `TestParent_RefusalRowCarriesTheParent`: follow `TestProject_RefusalRow` (`project_test.go:121`) with the child as the client.
- [ ] **Step 2:** Run `cd core && GOWORK=off go test ./listener/forwardproxy/ -run TestParent`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `cd core && GOWORK=off go test -race ./attribution/ ./listener/forwardproxy/`. Expected: PASS.
- [ ] **Step 5:** Commit: `feat: Record the session that started a headless run on its rows`.

### Task T4: Group by the rolled-up project, and add `parentSession` to the summary

Depends on: T2

**Files:**
- Modify: `core/cost/usage/usage.go:571-576` (`projectLabel` reads `e.RollupProject()`)
- Modify: `core/cost/ledger/writer.go:479` (`projectRowLabel(e.RollupProject())`)
- Modify: `core/session/fold.go` (`applyParent` next to `applyProject`, line 118; `SummaryFold` field `parentSession` near line 265; line 338 uses `e.RollupProject()` and calls `applyParent`; the summary build near line 383; `foldJSON` field `ParentSession string \`json:"parentSession,omitempty"\`` with the comment "A session.json written before it existed decodes to \"\"."; the encode near line 482 and decode near line 516)
- Modify: `core/session/store.go` (entry field `parentSession string` near line 143; line 562 uses `event.RollupProject()` and calls `applyParent`; line 989 uses `.RollupProject()` and calls `applyParent`)
- Modify: `core/session/summary.go` (field `ParentSession string \`json:"parentSession,omitempty"\`` after `Projects`, line 221, doc: "ParentSession is the session of the agent that started this session's agent: the first non-empty model.SessionEvent.ParentSession. Absent on default and on pending buckets."; set it at line 287)
- Modify: `core/session/history.go:228-235` (Decision 6)
- Test: `core/cost/usage/project_test.go`, `core/cost/ledger/project_test.go`, `core/session/projects_test.go`

**Interfaces:**
- Consumes: `SessionEvent.ParentSession`, `SessionEvent.ParentProject`, `RollupProject()` (T2).
- Produces: `session.SessionSummary.ParentSession string` (JSON `parentSession`), read by T5.

- [ ] **Step 1: Write the failing tests**
  - `TestSnapshot_GroupProjectUsesTheParentProject` (usage): one priced event `{Project: "~/child", ParentProject: "~/parent"}` and one `{Project: "~/solo"}`; group=project has series `~/parent` and `~/solo`, no `~/child`.
  - `TestWriter_GroupProjectUsesTheParentProject` (ledger): the same two events; the rows' `Project` are `~/parent` and `~/solo`.
  - `TestApplyParent`: `("", "c1", "s1")` → `"s1"`; `("s0", "c1", "s1")` → `"s0"`; `("", "c1", "")` → `""`; `("", DefaultSessionID, "s1")` → `""`; `("", PendingPrefix+"claude-code@1.2", "s1")` → `""`.
  - `TestStoreListSessions_ParentSession`: append to `c1` an event with `{Project: "~/child", ParentSession: "s1", ParentProject: "~/parent"}`; summary `Projects == ["~/parent"]`, `ParentSession == "s1"`. A session with no parent fields keeps `Projects == ["~/x"]` and `ParentSession == ""`.
  - `TestSummaryFold_ParentSessionMatchesTheStore`: the same events through `SummaryFold`; `Projects` and `ParentSession` equal the store's (follow `TestSummaryFold_Projects`, line 75).
  - `TestSummaryFold_JSONRoundTripKeepsParentSession`: follow `TestSummaryFold_JSONRoundTripKeepsProjects` (line 85); also decode a `session.json` with no `parentSession` key and get `""`.
  - `TestStoreRekey_ParentSession`: follow `TestStoreRekey_Projects` (line 127); a pending bucket's events with the parent fields, rekeyed to `c1`, give `ParentSession == "s1"` and `Projects == ["~/parent"]`.
  - `TestListSessions_ParentSessionFromTheEarlierHistory`: follow `TestListSessions_ProjectsMergeTheEarlierHistory` (line 111); the history's `parentSession` shows on the resident summary.
- [ ] **Step 2:** Run `cd core && GOWORK=off go test ./cost/... ./session/ -run 'Parent'`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `cd core && GOWORK=off go test ./cost/... ./session/... ./sessionapi/`. Expected: PASS.
- [ ] **Step 5:** Commit: `feat: File a headless run's cost and session under its parent's project`.

### Task T5: agentop shows which session started a run

Depends on: T4

**Files:**
- Modify: `cmd/agentop/tui/sessions_context.go:166` (`childMarker`)
- Modify: `cmd/agentop/tui/sessions_pane.go:298-310` (Decision 7)
- Modify: `cmd/agentop/tui/session_metadata.go:167` (`parentSessionOf`)
- Modify: `cmd/agentop/tui/app.go:2857-2864` (Decision 8)
- Test: `cmd/agentop/tui/imported_test.go` (next to `TestSessionTitleCell_ImportedPrefix`), `cmd/agentop/tui/sessions_title_test.go` (next to the `sessionHeader` tests at line 319)

**Interfaces:**
- Consumes: `session.SessionSummary.ParentSession` (T4).

- [ ] **Step 1: Write the failing tests**
  - `TestSessionTitleCell_ChildPrefix`: `fitModel(t, paneSessions, 200, 40, nil)`, sessions `{ID: "child-1", Title: "fix the build", ParentSession: "parent-123456789"}` and `{ID: "top-1", Title: "fix the build"}`. The child's title cell has the prefix `childMarker + "fix the build"`; the other has no `childMarker`.
  - `TestSessionIDCell_ChildPrefixWithNoTitleColumn`: follow `TestSessionIDCell_ImportedPrefixWithoutTitle` (`imported_test.go:130`), with `fitModel(t, paneSessions, 60, 40, nil)`, where TITLE is hidden; the SESSION cell of the child starts with `childMarker`, the other has no `childMarker`.
  - `TestSessionHeader_StartedBy`: with `m.sessions` holding `{ID: "id-1", Title: "fix the parser", ParentSession: "abcdef0123456789"}`, `m.sessionHeader("id-1", "event")` contains `" · event · started by abcdef01"`. A session with no parent: the header contains no `started by`.
  - `TestSessionHeader_StartedBySanitises`: `ParentSession: "ab\x1b[31mcd"`; the header has no `\x1b`.
- [ ] **Step 2:** Run `scripts/dev/go-test-shards.sh -short cmd/agentop`. Expected: FAIL on the new tests.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `scripts/dev/go-test-shards.sh -short cmd/agentop`. Expected: PASS.
- [ ] **Step 5:** Commit: `feat: Show which session started a headless run in agentop`.

### Task T6: Docs

Depends on: none

**Files:**
- Modify: `docs/session-api.md` (summary list near line 89; event field list near line 1252)

- [ ] **Step 1:** In the summary field list, change the `projects` item to say that each label is the event's `parentProject` when it has one, else its `project`. After it, add: "**`parentSession`** — the session of the agent that started this session's agent, for a headless run (for example `claude -p`) that another session's process started. It is the first non-empty `parentSession` of the session's events. Absent on `default` and on pending buckets. Rows recorded before this field existed have none."
- [ ] **Step 2:** After the `project` event item, add two items:
  - "`parentSession` — the session that the topmost process above this row's agent named, when it is not this row's session. Set only by the forward proxy, with `session.process_attribution` on. Absent on the VM control path, on a tool process filed in that same session, and in praxis mode's ingested rows."
  - "`parentProject` — the project of the process that holds `parentSession`, read once per connection. Absent with `parentSession`. `/v1/usage?group=project`, the cost ledger and the summary's `projects` use it in place of `project` when it is set."
- [ ] **Step 3:** Run `pre-commit run check-doc-links --files docs/session-api.md`. Expected: Passed.
- [ ] **Step 4:** Commit: `docs: Describe parentSession and parentProject`.

## PRs

One PR. Estimated counted lines (the golden file and this plan do not count): T1 ≈ 80, T2 ≈ 130, T3 ≈ 230, T4 ≈ 200, T5 ≈ 110, T6 ≈ 25. Total ≈ 775, under the 2000 limit.

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: Attribute a headless agent run to the session that started it` | none | T1, T2, T3, T4, T5, T6 | 775 |

## Gate

From the worktree root. Checked on the base (a698b60eb) on 2026-11-19: `gofmt -l`, the three `go vet` steps and `cargo fmt --check` pass as written. No change to how they are called.

```sh
(cd core && GOWORK=off go vet ./... && GOWORK=off go test -race ./attribution/ ./pipeline/ ./model/ ./listener/... ./cost/... ./session/... ./sessionapi/) && (cd cmd/cortex && GOWORK=off go vet ./...) && (cd cmd/agentop && GOWORK=off go vet ./... && GOWORK=off go test -race $(GOWORK=off go list ./... | grep -v 'agentop$')) && scripts/dev/go-test-shards.sh -race cmd/agentop && test -z "$(gofmt -l core cmd/cortex cmd/agentop)" && (cd praxis && cargo fmt --check && cargo test --locked -p cortex-praxis-filters) && pre-commit run --from-ref a698b60eb1c6d30f76b4e9ba4ce84b49983d3a87 --to-ref HEAD
```

## For a human after review

Needs: T1 to T5 merged and installed on a laptop.

1. Live check (issue Tests item 6). From a Claude Code session, run `claude -p --model haiku "say hi"` in a temporary git directory (for example `/private/tmp/parenttest` after `git init`).
2. `curl -s localhost:47601/v1/sessions | jq '.[] | select(.parentSession != null) | {id, parentSession, projects}'`. Expected: the child session has `parentSession` set to the parent's id and `projects` holds the parent's project.
3. In agentop: the agents pane shows no new project row for `/private/tmp/parenttest`, and the sessions pane shows `↳ ` on the child. Its event screen header says `started by <first 8 of the parent id>`.
4. Close #1991.
