# Plan: show a project's spend by model in the agentop drawer (#2034)

Source: issue #2034 (part 2 of 3 of #2032). The design is in #2032. The task text after
the line `---` is the issue body. This header adds the decisions, the dependencies, the
PR list and one docs task (T7).

Base: `cd2720a5226cfad9ccc9aa403a030c01ccc47a1d` (upstream `main`). Part 1 (#2033) merged
in PR #2038 before this base. So `project=` on `/v1/usage` and `Snapshot.Project` are on
the base, and the PR has no parent PR.

## Decisions made while planning

1. **No parent PR.** The issue says "stack the PR on part 1". Part 1 is merged (PR #2038),
   so the PR goes on `main`.
2. **The echo field type.** `Snapshot.Project` is `model.LenientString`
   (`core/cost/usage/snapshot.go:177`). `Snapshot.Agent` is `string` (`:174`). Task 5
   compares `string(snap.Project) == project`.
3. **Three existing tests forbid a `project` key. Task 5 changes them.** They contradict
   Task 5:
   - `TestFetchUsageScopedProject_AsksGroupProjectAndNoProjectKey`
     (`cmd/agentop/tui/agent_scope_test.go:381`). Delete its first half (the project
     scope). Keep the second half (no project: one query, no `group=`) as
     `TestFetchUsageScopedProject_NoProjectIsTheAgentRead`. The four new Task 5 tests
     cover the project scope.
   - The fixture `projectsModel` (`cmd/agentop/tui/agents_projects_test.go:129`) and the
     fixture `cursorScopeModel` (`cmd/agentop/tui/agents_cursor_scope_test.go:25`). Each
     has `t.Errorf` when a query has `project`. Replace each guard with a guard that fails
     only when the `project` value is `Other` or `(other)`. Do not echo `project` in these
     fixtures. Then they act as an older proxy, and every existing test reads through the
     fallback with the figures it reads today.
   Reason: the issue's Task 5 sends `project=`. These guards pinned part 0's "no
   `project=` key" rule, which #2032 replaces.
4. **Two existing drawer tests contradict Task 6. Task 6 changes them** (in
   `cmd/agentop/tui/project_scope_spend_test.go`):
   - `TestProjectScope_DrawerHasNoAxesAndSaysSo` (`:142`). Rename it
     `TestProjectScope_OlderProxyDrawerSaysNoBreakdown`. Assert that `spendAxes()` is
     `[model endpoint]`, and that the lines still say `no breakdown for this scope in`
     (the fixture does not echo, so the reply is the narrowed `GroupNone` one). Remove the
     "no `[a]`" assertion.
   - `TestProjectScope_DrawerAxisKeyIsInert` (`:160`). Rename it
     `TestProjectScope_DrawerAxisKeyCyclesModelAndEndpoint`. Assert that `a` gives a
     command, and that two presses go model, endpoint, model, never agent.
5. **`spendAxis` under a project scope.** `spendAxis` (`cmd/agentop/tui/agent_scope.go`)
   maps the agent axis to the first axis only when an agent is scoped. Task 6 makes it
   do that also when `spendScopeProject() != ""`. Reason: Task 6 gives a project scope the
   axes of an agent scope, which has no agent axis. A project under All agents has
   `spendScopeAgent() == ""`, and without this change the drawer could ask for
   `group=agent`.
6. **The usage pane also sends `project=` after Task 5.** `usage_pane.go:249` calls
   `fetchUsageScopedProject` with `usage.GroupNone`. It gets the same echo check and the
   same fallback. No code change there. The figures stay the same.
7. **README text is stale after this PR. T7 updates it.** `cmd/agentop/README.md:988-994`
   says that "the server has no `project=` parameter" and that "the drawer shows no
   breakdown". The issue does not name this file. T7 is a docs task with no dependency.
   It must state only these facts:
   - agentop sends `project=<display name>` (with `agent=` when an agent is scoped) and
     uses the reply only when the reply echoes `project`.
   - A proxy that does not echo `project` (older than #2033) gets the old read:
     `group=project`, narrowed by agentop.
   - agentop never sends `project=Other` or `project=(other)`. Those scopes use the old
     read.
   - Under a project scope the drawer axes are `model` and `endpoint`, and `a` cycles
     them. The project `Other` has no axes.
   - The drawer says `no breakdown for this scope in <window>` only when the reply has
     no breakdown (an older proxy, or a window the proxy cannot break down).
   - The spend band shows each unit (for example `$12.00 + 0.95 Bobcoins`) under a
     project scope too.
   Keep the other sentences of that paragraph (`[b]` is inert on the usage pane, latency
   is not available per project, the sessions list rules).
8. **One PR.** The estimated total is about 640 counted lines, under the 2,000 limit.

## Tasks, dependencies and estimates

| Task | Title | Depends on | Est. lines |
|---|---|---|---|
| T5 | Send `project=`, trust only the echo | none | 300 |
| T6 | Drawer axes and band units under a project scope | T5 | 320 |
| T7 | `cmd/agentop/README.md`: the project scope reads `project=` | none | 20 |

T6 depends on T5: its tests need the server reply that T5's `fetchUsageScopedProject`
uses, and Task 6's `fetchSpendSpan` change calls it with `project`.

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | feat: Show a project's spend by model in agentop | "" | T5, T6, T7 | 640 |

## Gate

From the worktree root:

```sh
GOWORK=off go -C cmd/agentop vet ./... && test -z "$(gofmt -l cmd/agentop)" && GOWORK=off go -C cmd/agentop test -race ./tui/ ./apiclient/ -count=1 && scripts/dev/go-test-shards.sh -short cmd/agentop && pre-commit run --from-ref cd2720a5226cfad9ccc9aa403a030c01ccc47a1d --to-ref HEAD
```

The plan changes only the `cmd/agentop` module (`tui/`, `apiclient/`, `README.md`).
`go-test-shards.sh` tests only the root package, so the gate also runs the two changed
packages with `-race`.

## Task T7: `cmd/agentop/README.md`

**Files:** Modify `cmd/agentop/README.md` (the paragraph that starts
"**`↵` on a project sub-row scopes to that project.**", near line 988).

**Depends on:** none.

- [ ] **Step 1:** Rewrite the sentences that decision 7 names, with the facts in
  decision 7 only.
- [ ] **Step 2:** Run `pre-commit run --files cmd/agentop/README.md`.
- [ ] **Step 3: Commit** `docs: Say that agentop asks the proxy for one project's usage`.

## For a human after review

Nothing. This part has no step outside the repository. Opening the PR is the workflow's
own step.

---

Part 2 of 3 of #2032. The design, its anti-goals and its facts are in #2032. This part needs part 1 (the `project=` parameter and the `Snapshot.Project` echo). Stack the PR on part 1.

## Global Constraints

- Project ring cap: **32**, a constant (`maxProjectRings`), not config.
- Ring keys: `("", project)` for every event; `(agent, project)` only when `model.IsKnownAgent(agent)`.
- The project label is `projectLabel(e)` (ring) and `labelFor(r, usage.GroupProject)` (ledger). The server matches the label the client got from `group=project`, never a raw path.
- New wire field: `Snapshot.Project string \`json:"project,omitempty"\``. Set ONLY when the filter was applied. Absent means "not applied", the same contract as `Snapshot.Agent`.
- agentop never sends `project=Other` (`otherProjects`) or `project=(other)` (`projectOverflowLabel`).
- Out of scope: Sessions pane cost cell, `agentop cost --project`, new drawer axes.
- Every commit: `git commit -s`, message ends with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. PR titles `feat: <Uppercase subject>`. Each PR ≤ 2,000 lines.
- Before each push: `GOWORK=off go vet ./...` and `gofmt -l .` in each touched module (`core`, `cmd/agentop`).

## Review Focus

1. **An older proxy ignores `project=`.** It returns the agent's whole traffic with no `project` echo. agentop must fall back to client-side narrowing, never show it under the project label. Test: Task 5, `TestFetchUsageScopedProject_NoEchoFallsBack`.
2. **A project ring was evicted and comes back.** It has fewer minutes than the window. The server must serve the narrowed, group-none reply. Test: Task 2, `TestProjectSnapshot_EvictedRingIsServedNarrowed`.
3. **Startup replay with more than 32 projects.** The merge must keep the cap. Test: Task 1, `TestFinishReplay_KeepsTheProjectRingCap`.
4. **`project=` with an unrecognised agent on a ring window.** No agent×project data exists. The reply must not echo `project`. Test: Task 2, `TestProjectSnapshot_UnknownAgentDoesNotEcho`.
5. **An 80-column terminal with SERVER shown.** A mixed cell does not fit. It must show `(mixed)`, and AGENT must not get narrower than today. Test: Task 7, `TestAgentsPane_MixedCellFallsBackAt80`.


## PR 2: agentop drawer and band (Tasks 5–6), stacked on PR 1

### Task 5 (T5): Send `project=`, trust only the echo

**Depends on:** none. Also apply decisions 2, 3 and 6.

**Files:**
- Modify: `cmd/agentop/apiclient/client.go` (`GetUsageWindowForAgent` at `:597`), `cmd/agentop/tui/agent_scope.go` (`fetchUsageScopedProject` at `:72`)
- Test: `cmd/agentop/tui/agent_scope_project_test.go`, `cmd/agentop/apiclient/client_test.go`

**Interfaces:**
- Produces: `func (c *Client) GetUsageWindowScoped(ctx context.Context, window string, resolution time.Duration, sessionID, agent, project string, group usage.Group) (*usage.Snapshot, error)`. `GetUsageWindowForAgent` calls it with `project ""`. Sets `project` in the query when non-empty.
- `fetchUsageScopedProject` (same signature as today):
  1. `project == ""` → unchanged.
  2. `project == otherProjects || project == projectOverflowLabel || agent == otherAgents` → today's path (group=project, `narrowToProject`).
  3. Otherwise `GetUsageWindowScoped(..., agent, project, group)`. If `snap.Project == project` (and, when `agent != ""`, `snap.Agent == agent`), return it. Else today's path.

- [ ] **Step 1: Write the failing tests** with an `httptest.Server` that records the query:
  - `TestGetUsageWindowScoped_SendsProject`: query has `project=/repo/a` and `agent=claude-code`.
  - `TestFetchUsageScopedProject_UsesTheEchoedReply`: server echoes `project` with `group=model` series → returned snapshot has `Group == GroupModel`, one request made.
  - `TestFetchUsageScopedProject_NoEchoFallsBack`: server ignores `project=` (no echo) → a second request with `group=project`, result narrowed to `/repo/a` (totals = that series only).
  - `TestFetchUsageScopedProject_NeverSendsOther`: `project=Other` and `project=(other)` → no request carries a `project` parameter.
- [ ] **Step 2: Run, expect FAIL.** `GOWORK=off go -C cmd/agentop test ./tui/ ./apiclient/ -run 'UsageWindowScoped|FetchUsageScopedProject' -count=1`
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `feat: Ask the proxy for one project's usage`.

### Task 6 (T6): Drawer axes and band units under a project scope

**Depends on:** T5. Also apply decisions 4 and 5.

**Files:**
- Modify: `cmd/agentop/tui/agent_scope.go` (`spendAxes` at `:104`), `cmd/agentop/tui/spend_drawer.go` (`drawerScopeNote` at `:1170`), `cmd/agentop/tui/spend.go` (`fetchSpendSpan` guard at `:984`)
- Test: `cmd/agentop/tui/spend_drawer_project_test.go`

**Interfaces:**
- Consumes: Task 5's `fetchUsageScopedProject`.
- `spendAxes`: a project scope returns the same axes as an agent scope (model, endpoint), except `nil` for `otherProjects`.
- `drawerScopeNote`: drop the `project != ""` early return. For a project scope, show `"no breakdown for this scope in " + windowLabel` only when `snap.Group == usage.GroupNone` and `asked` is not none. Keep the agent cases as they are.
- `fetchSpendSpan`: remove `&& project == ""`. The split request goes through `fetchUsageScopedProject(..., agent, project, usage.GroupCurrency)`.

- [ ] **Step 1: Write the failing tests** (drive the model as the existing `spend_drawer_test.go` tests do):
  - `TestSpendDrawer_ProjectScopeShowsModelRows`: a project row scoped, a reply with `Group == GroupModel` and two model series → the drawer renders both model labels and no "no breakdown" line.
  - `TestSpendDrawer_ProjectScopeGroupNoneShowsNote`: a reply with `Group == GroupNone` → the line `no breakdown for this scope in 1H` (use the label the drawer prints for the window).
  - `TestSpendDrawer_OtherProjectHasNoAxes`: scope `otherProjects` → `spendAxes()` is nil.
  - `TestSpendBand_ProjectScopeShowsEachUnit`: a project scope, a `GroupNone` reply with `Currencies: [Bobcoins USD]`, and a `GroupCurrency` split → the band cell is `$12.00 + 0.95 Bobcoins` (values from the fixture).
- [ ] **Step 2: Run, expect FAIL.** `GOWORK=off go -C cmd/agentop test ./tui/ -run 'SpendDrawer_Project|SpendDrawer_OtherProject|SpendBand_Project' -count=1`
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run, expect PASS.** Then `scripts/dev/go-test-shards.sh -short cmd/agentop`.
- [ ] **Step 5: Commit** `feat: Break down a project's spend in the agentop drawer`. Open PR 2: `feat: Show a project's spend by model in agentop`.
