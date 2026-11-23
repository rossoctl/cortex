# Plan: show the agentop Agents pane cost in each unit (#2035)

Source: issue #2035 (part 3 of 3 of #2032). The design is in #2032. The text after the
line `---` is the issue body. Its Task 7 is task T7 here. This header adds the decisions,
the dependencies, the PR list and one docs task (T8). Where the issue body and a decision
below differ, the decision applies. Two such places: do not add `agentRow.byUnit`
(decision 2), and do not hard-code AGENT width 26 (decision 7).

Base: `08146f192a770865222892abcba5abfe44f05289` (upstream `main`). Part 1 (#2033, PR #2038)
and part 2 (#2034, PR #2040) merged before this base. So `fetchUsageScopedProject` sends
`project=` on the base, and the PR has no parent PR.

## Decisions made while planning

1. **No parent PR.** The issue says "stack the PR on part 2". Part 2 is merged (PR #2040),
   so the PR goes on `main`.
2. **One store for the split: `m.agentsSplits`.** The issue lists both `agentRow.byUnit`
   and `model.agentsSplits`. Keep only `m.agentsSplits map[agentsSplitKey]map[string]int64`
   and read it in `rebuildAgentsTable` / `addProjectRows` by the row's key. Do not add
   `agentRow.byUnit`. Reason: each `agentRowsLoadedMsg` and `agentProjectsLoadedMsg`
   replaces the rows, so a field on the row is lost on every poll and the cell flickers to
   `(mixed)` until the next split lands. Names are left to the builder in #2032.
3. **The split key.** `agentsSplitKey{agent, project, window}`:
   - `agent` is the row's agent label (`""` for All agents).
   - `project` is the series label (`agentRowDesc.project`, the label from
     `group=project`), never the display name from `projectDisplayNames`. `""` for an agent
     row and for an "All projects" sub-row.
   - `window` is `m.agentsSpanDef().window`, captured on the update goroutine when the
     command is built.
   An "All projects" sub-row under an agent uses the agent row's key `(agent, "")`. The
   "All projects" sub-row under All agents uses `("", "")` and fetches
   `fetchUsageScoped(..., "", usage.GroupCurrency)`.
4. **Which rows fetch.** Only rows whose `len(units) > 1`. Never a row with agent
   `otherAgents` and never a project row with label `otherProjects` or
   `projectOverflowLabel`. Those rows keep `(mixed)`. No split fetch for an "Other" row.
5. **Stale replies.** The message handler stores a split only when `key.window ==
   m.agentsSpanDef().window`. When it stores, it deletes every key whose window differs,
   so the map holds one window at a time. A failed fetch (error) stores nothing and
   deletes nothing. After it stores, it calls `m.rebuildAgentsTable()`.
6. **Column width.** In `rebuildAgentsTable`, render the cost cells before the columns are
   fit:
   - `floor = max(agentsCostWidth, len(title))`.
   - `free = m.innerWidth() − tableWidth(every column but COST, at its declared width:
     AGENT 34, SESSIONS 8, LAST SEEN agentsLastSeenWidth, SERVER agentsServerWidth) −
     cellPadding`.
   - `budget = max(floor, free)`. Render each cost cell with `budget`.
   - `costWidth = max(floor, min(budget, widest rendered cost cell))`.
   - Then `agentsColumns(window, costWidth)` and `fitTableColumns` as today.
   At 80 columns `free < floor`, so `costWidth == floor` and the fitter gives AGENT the
   same width as on the base. Pass `budget` to `agentCostCellIn` in `addProjectRows` too
   (add a `budget int` parameter to `addProjectRows`). `newAgentsTable` calls
   `agentsColumns(label, max(agentsCostWidth, len(title)))`, which is today's width.
7. **`TestAgentsPane_MixedCellFallsBackAt80` does not hard-code 26.** The issue says AGENT
   is 26 at 80 columns. The comment on `TestAgentsPane_TheAgentsNameSurvivesTheServerColumnAt80`
   (`cmd/agentop/tui/server_route_test.go:461`) says 16. The test measures AGENT with
   `sessionsColumnWidth(m.agentsTbl.Columns(), "AGENT")` BEFORE the split message, applies
   the split, and asserts the width is the same and the cell is `(mixed)`. Reason: the base
   is the reference, and a hard-coded number that the plan cannot confirm is a guess.
8. **`unitsAmount` signature.** `func unitsAmount(units []string, byUnit map[string]int64)
   (string, bool)` is the multi-unit half of `bandAmount` (`spend_band.go:484`): a nil
   `byUnit` or a unit missing from it gives `"", false`, USD (`money.IsDefault`) first, then
   the others in `units` order, each `money.Relabel(formatUSDTotalMicros(v), u, 0)`, joined
   by `" + "`. `bandAmount` keeps its one-unit branch and calls `unitsAmount(r.Units,
   r.ByUnit)` for the rest. No band output changes.
9. **`agentCostCellIn` keeps its one-unit behavior.** New signature `agentCostCellIn(c
   usage.Counts, units []string, byUnit map[string]int64, budget int) string`. Only when
   `len(units) > 1` and the cell is not `emptyCell`: `unitsAmount(units, byUnit)`; if it
   fails or `lipgloss.Width(out) > budget` (use the width helper the package already uses
   for cells), return `money.Mixed`. Every other case is today's code with `budget`. Every
   caller passes `nil` for `byUnit` where it has no key (the Other rows).
10. **Docs task T8.** `cmd/agentop/README.md:408-409` says "A row that itself spans units
    reads `(mixed)`". After T7 that is false for the Agents pane. T8 changes that sentence
    to state only these facts:
    - In the Agents pane, an agent row or a project row that spans units shows each unit's
      amount, dollars first: `$1,065.52 + 0.95 Bobcoins`.
    - The COST column grows into the free width. When the terminal is too narrow for the
      cell, the cell reads `(mixed)`.
    - The Other rows and the Sessions pane still read `(mixed)` for a row that spans units.
    - A single foreign unit is no longer cut when the pane has free width
      (`15.90 Bobcoins`).
    T8 does not change `docs/pricing.md`: its `(mixed)` sentence (`:463`) is about
    `agentop observe` in general and stays true for the Sessions pane.

## Tasks and dependencies

- **T7** — A cost in each unit, in the free width (the issue's Task 7, with decisions 2-9).
  Files: `cmd/agentop/tui/spend_band.go`, `cmd/agentop/tui/agents_pane.go`,
  `cmd/agentop/tui/agents_projects.go`, `cmd/agentop/tui/app.go` (the new message case,
  and the split fetches after `agentRowsLoadedMsg` at `:1567` and `agentProjectsLoadedMsg`
  at `:1627`), new test `cmd/agentop/tui/agents_cost_units_test.go`. Existing tests that
  call `agentCostCellIn` or `agentsColumns` get the new arguments.
  Depends on: none. Estimate: 450 counted lines (about 170 code, 280 tests).
- **T8** — Docs: the Agents pane cost cell in `cmd/agentop/README.md` (decision 10).
  Depends on: none. Estimate: 10 counted lines.

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: Show the Agents pane cost in each unit` | none (on `main`) | T7, T8 | 460 |

One PR: the total is under 2,000 lines.

## Gate

From the worktree root:

```sh
GOWORK=off go -C cmd/agentop vet ./... && test -z "$(gofmt -l cmd/agentop)" && scripts/dev/go-test-shards.sh -race cmd/agentop && (cd cmd/agentop && export GOWORK=off && go test -race $(go list ./... | grep -v 'agentop$')) && pre-commit run --from-ref 08146f192a770865222892abcba5abfe44f05289 --to-ref HEAD
```

Checked on the unchanged base (2026-11-20): `GOWORK=off go -C cmd/agentop vet ./...` and
`test -z "$(gofmt -l cmd/agentop)"` pass from the worktree root. No change to the gate.

## For a human after review

Nothing. The PR is opened by the workflow. No step posts outside the repository.

---

Part 3 of 3 of #2032. The design, its anti-goals and its facts are in #2032. This part needs part 2 (`fetchUsageScopedProject` with `project=`). Stack the PR on part 2.

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


## PR 3: Agents pane COST cell (Task 7), stacked on PR 2

### Task 7: A cost in each unit, in the free width

**Files:**
- Modify: `cmd/agentop/tui/spend_band.go` (`bandAmount` at `:483`), `cmd/agentop/tui/agents_pane.go` (`agentRow` at `:20`, `agentsColumns` at `:282`, `rebuildAgentsTable` at `:346`, `agentCostCellIn` at `:498`), `cmd/agentop/tui/agents_projects.go` (cost cells at `:282-317`, fetch at `:87`)
- Test: `cmd/agentop/tui/agents_cost_units_test.go`

**Interfaces:**
- Consumes: Task 5's `fetchUsageScopedProject`, `fetchUsageScoped`; `unitCosts` (`spend.go:877`).
- Produces:
  - `func unitsAmount(units []string, byUnit map[string]int64) (string, bool)`: the `" + "` join that `bandAmount` does today, USD first, moved out so `bandAmount` and the cell share it. `bandAmount` calls it. No band output changes.
  - `agentRow.byUnit map[string]int64`: nil until a split arrives.
  - `type agentsSplitKey struct{ agent, project, window string }` and `model.agentsSplits map[agentsSplitKey]map[string]int64`.
  - `agentsUnitSplitLoadedMsg{key agentsSplitKey; byUnit map[string]int64}`.
  - `agentCostCellIn(c usage.Counts, units []string, byUnit map[string]int64, budget int) string`: for more than one unit, `unitsAmount`; if it fails or is wider than `budget`, `money.Mixed`. One unit: as today, with `budget`.
  - `agentsColumns(window string, costWidth int)`: COST width = `costWidth`.

Rules:
- After `agentRowsLoadedMsg` and `agentProjectsLoadedMsg`, issue one split fetch for each row with `len(units) > 1` that is not `Other`: agent rows with `fetchUsageScoped(..., agent, usage.GroupCurrency)`, project rows with `fetchUsageScopedProject(..., agent, project, usage.GroupCurrency)`. Store `unitCosts(reply)`. A failed fetch stores nothing. A `w` window change drops stale replies (the key has the window).
- Column width: `costWidth = min(widestCell, max(agentsCostWidth, len(title), innerWidth − widths of all other columns − the table's gaps))`, where the other columns keep their today widths (AGENT 34 before the fitter). Cells are rendered with that budget, so a cell that does not fit falls back.

- [ ] **Step 1: Write the failing tests:**
  - `TestUnitsAmount_DollarsFirst`: `units [Bobcoins USD]`, `byUnit {USD: 1_065_520_000, Bobcoins: 950_000}` → `"$1,065.52 + 0.95 Bobcoins"`, true. Use the format `bandAmount` produces today for the exact string. A unit missing from `byUnit` → false.
  - `TestBandAmount_Unchanged`: the existing band tests pass without edits (run them).
  - `TestAgentCostCellIn_EachUnit`: the same input, budget 40 → `"$1,065.52 + 0.95 Bobcoins"`. Budget 12 → `"(mixed)"`. `byUnit` nil → `"(mixed)"`.
  - `TestAgentsPane_MixedRowShowsEachUnitWhenWide`: 160 columns, router on, a claude-code row with two units and a split reply → the row text contains `$1,065.52 + 0.95 Bobcoins`.
  - `TestAgentsPane_ForeignUnitIsNotCut`: 160 columns, a bob row in `Bobcoins` only, 15.90 → the row contains `15.90 Bobcoins`, not `…`.
  - `TestAgentsPane_MixedCellFallsBackAt80`: 80 columns, router on, SESSIONS and LAST SEEN shown → the mixed row shows `(mixed)`, and the AGENT column width equals the width it has today at 80 columns (26, per `TestAgentsPane_TheAgentsNameSurvivesTheServerColumnAt80`).
  - `TestAgentsPane_ProjectRowSplitUsesProject`: an open claude-code row with a mixed project row → the split request has `project=<label>` and `group=currency`.
  - `TestAgentsPane_StaleSplitIsDropped`: a split for window `7d` arrives after `w` moved to `month` → not used.
- [ ] **Step 2: Run, expect FAIL.** `GOWORK=off go -C cmd/agentop test ./tui/ -run 'UnitsAmount|AgentCostCellIn|AgentsPane_(Mixed|Foreign|ProjectRowSplit|StaleSplit)' -count=1`
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run, expect PASS.** Then the full suite once: `scripts/dev/go-test-shards.sh -race cmd/agentop` and, from `cmd/agentop`, `go test -race $(go list ./... | grep -v 'agentop$')`. Then `GOWORK=off go -C cmd/agentop vet ./...`, `gofmt -l cmd/agentop`.
- [ ] **Step 5: Commit** `feat: Show each unit's cost in the agentop Agents pane`. Open PR 3: `feat: Show the Agents pane cost in each unit`.


Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
