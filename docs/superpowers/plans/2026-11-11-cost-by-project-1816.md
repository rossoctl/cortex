# Cost by project (#1816) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `/v1/usage?group=project` and `agentop cost --by project` answer "what did each project cost in this window?".

**Architecture:** Add one axis, `usage.GroupProject = "project"`, the same kind of axis as `GroupAgent`: a `byProject` label map on the ring's `bucket` (so the per-agent rings get it too), and a `Project` field in the ledger row and its key. The session API needs no code: it already parses `group=` with `usage.ParseGroup`. agentop adds `project` to `--by` and sizes its LABEL column to the longest label.

**Tech Stack:** Go 1.26.10, modules `core` and `cmd/agentop`.

**Spec:** https://github.com/rossoctl/cortex/issues/1816 (parent #1739). Its "Decisions" are fixed. `SessionEvent.Project` (`core/model/session.go:710`, json `project`) landed in #1815 / PR #1819 and is in the base commit.

## Global Constraints

- An empty project is the series label `unknown`, in the ring and in the ledger. A real label always starts with `/` or `~`, so it cannot collide.
- No `project=` query parameter. No per-project ring. No TUI change (#1817, #1818).
- `--agent` together with `--by` stays refused (`cmd/agentop/cmd_cost.go:122`).
- The caps stay as they are: ring `maxLabelsPerBucket` = 64 with overflow `(other)`, `MaxSeriesInResponse` = 16, ledger `maxLabelsPerMinute` = 64 joint rows per minute.
- Ledger schema rule (row.go:41-44): add, never rename. A row with no `project` decodes as "" and reads as `unknown`.
- Ledger stores absence as "" on disk (like `Row.Agent`); only `labelFor` maps "" to `unknown`.
- Run per-module Go commands with `GOWORK=off`. Always give `gofmt` a path.
- Commits: `git commit -s`, ASD-STE100 text, end with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. No `Co-Authored-By`.

## Decisions made for this plan

1. **The `unknown` label is one exported constant, `usage.UnknownProjectLabel = "unknown"`**, in `core/cost/usage/snapshot.go` beside `GroupProject`. The ledger uses it. Reason: one spelling for both halves, as `model.UnknownClientLabel` does for agents; the ledger already imports `usage`.
2. **Project labels use the existing label caps:** `ringLabel` in the ring and `rowLabel` in the ledger. Both sanitise and cut at 96 bytes (`event.MaxLabelLen`), keeping the head. Reason: the same cut in both halves keeps `group=project` spelled the same from either source, and the ledger line-length bound (row.go `maxLabelLen` comment) depends on it. A path longer than 96 bytes is cut; this is a known limit, said in the docs task. The comment "Four labels at 96 bytes" in row.go becomes "Five".
3. **`group=project` is reconcilable** (`Group.Reconcilable` already returns true for every axis but none and plugin) and **ledger-servable** (`ledger.Groupable` true). So `ungroupedCostMicros` is computed, and agentop's `ledgerServedAxes` literal adds `project`.
4. **`unitAxes` gains `GroupProject`, and `SeriesCurrenciesIn` covers it.** Reason: a project can call two gateways in two units; the per-row "(mixed)" cell in agentop needs this.
5. **One PR.** The estimate is about 650 counted lines, well under 2000.

## Review Focus

1. A label longer than 34 runes in `agentop cost --by project`: every row and the header keep their columns aligned, and nothing is cut. Test in T4.
2. A minute with more than 64 (endpoint, model, agent, provenance, project) tuples: the excess folds into one row with every label, project included, `(other)`, and no cost is lost. Test in T2.
3. An old ledger line with no `project` key: it reads as `unknown` for `group=project` and counts in the total. Test in T2.
4. A ring bucket merge (`bucket.add` in `rekey.go`) must carry `byProject`, or a session rename or replay merge drops the project series. Test in T1 (replay test reaches `FinishReplay`, which merges buckets).
5. `agent=claude-code&group=project` on a ring window returns per-project series for that agent only, not every agent's. Tests in T1 and T3.

---

### Task T1: Project axis in the usage ring

**Depends on:** none

**Files:**
- Modify: `core/cost/usage/snapshot.go` (Group consts ~line 67-79, `ParseGroup` 82-114)
- Modify: `core/cost/usage/usage.go` (`bucket` struct ~504-566, `unitAxes` 571, `foldInto` ~1379)
- Modify: `core/cost/usage/rekey.go` (`bucket.add`, ~line 74)
- Modify: `core/cost/usage/series.go` (`bucket.series`, 244-276)
- Test: `core/cost/usage/project_test.go` (new)

**Interfaces:**
- Produces: `usage.GroupProject Group = "project"`; `usage.UnknownProjectLabel = "unknown"` (untyped string const); `ParseGroup("project")` returns `GroupProject, nil`; the `ParseGroup` error text becomes `unknown group (want none, model, endpoint, session, agent, project, currency, status, plugin or host; method is accepted as an alias for model)`.

- [ ] **Step 1: Write the failing tests** in `core/cost/usage/project_test.go`, following `agent_test.go` and `agent_snapshot_test.go` for event construction:
  - `TestParseGroup_AcceptsProject`: `ParseGroup("project")` == `GroupProject`, no error; the error for `"bogus"` contains `project`.
  - `TestSnapshot_GroupProjectBreaksDownByProject`: three priced response events with `Project` `/repo/a`, `/repo/b` and `""`, client `claude-code`. `Snapshot` with `GroupProject` has series keys exactly `/repo/a`, `/repo/b`, `unknown` with one request each, and the series sum to `Totals`.
  - `TestAgentSnapshot_GroupProjectOnTheAgentRing`: the same three events from `claude-code` plus one event from `bob` with `Project` `/repo/c`. `AgentSnapshot(window, res, "", "claude-code", GroupProject)` has series `/repo/a`, `/repo/b`, `unknown` and no `/repo/c`.
  - `TestSnapshot_GroupProjectReportsSeriesCurrencies`: one project with events in two units (follow the existing SeriesCurrencies test for GroupAgent) gives `SeriesCurrencies["/repo/a"]` with both units.
  - `TestReplay_RebuildsTheProjectSeries`: record the events through the replay path (`BeginReplay`, replay records, `FinishReplay`, as `replay_test.go` `TestFinishReplay_AddsTheReplayToLiveTraffic` does), then `Snapshot` with `GroupProject` has the `/repo/a`, `/repo/b`, `unknown` series.

- [ ] **Step 2: Run them, expect FAIL** (undefined `GroupProject`).
  Run: `cd core && GOWORK=off go test ./cost/usage/ -run 'Project' -v`

- [ ] **Step 3: Implement.**
  - `snapshot.go`: `GroupProject Group = "project"` with a short godoc (the git repo root of the agent process's cwd, from `SessionEvent.Project`; empty is `UnknownProjectLabel`); `UnknownProjectLabel = "unknown"`; a `case GroupProject` in `ParseGroup`; the new error text above.
  - `usage.go`: field `byProject map[string]Counts` on `bucket`; `unitAxes` becomes `[...]Group{GroupModel, GroupEndpoint, GroupAgent, GroupProject}`; helper `func projectLabel(e *model.SessionEvent) string` returns `UnknownProjectLabel` for `""`, else `ringLabel(e.Project)`; in `foldInto` after the agent line: `b.noteUnit(GroupProject, addLabel(&b.byProject, projectLabel(e), one), unit)` (unconditional, like the agent axis, so the series sum to the total).
  - `rekey.go` `bucket.add`: `addLabels(&b.byProject, o.byProject, b.unitsFrom(o, GroupProject))`.
  - `series.go`: `case GroupProject: src = b.byProject`.
  - Check every other place that resets or copies a bucket's label maps (grep `byAgent` in `core/cost/usage/`) and add `byProject` beside each one.

- [ ] **Step 4: Run, expect PASS**, then the whole package.
  Run: `cd core && GOWORK=off go test -race ./cost/usage/ && GOWORK=off go vet ./cost/usage/`

- [ ] **Step 5: Commit** `feat: Add the project axis to the usage ring`.

### Task T2: Project in the cost ledger

**Depends on:** T1 (uses `usage.GroupProject`, `usage.UnknownProjectLabel`)

**Files:**
- Modify: `core/cost/ledger/row.go` (`Row` 66-120, `key` 148-166, `maxLabelLen` comment, `overflowKeyFor`/`overflow` 240-260)
- Modify: `core/cost/ledger/writer.go` (`Record`, ~454)
- Modify: `core/cost/ledger/query.go` (`Groupable` 453, `labelFor` 493-540, `SeriesCurrenciesIn` 602)
- Test: `core/cost/ledger/group_test.go` (`allGroups`), `core/cost/ledger/query_test.go` (`TestRecord_LabelsAreCappedSoALineCanNeverExceedTheReadLimit`), new `core/cost/ledger/project_test.go`

**Interfaces:**
- Consumes: `usage.GroupProject`, `usage.UnknownProjectLabel` from T1.
- Produces: `Row.Project string` json `project,omitempty`; `ledger.Groupable(usage.GroupProject) == true`.

- [ ] **Step 1: Write the failing tests.**
  - `group_test.go`: add `usage.GroupProject` (and the missing `usage.GroupCurrency`, `usage.GroupHost`) to `allGroups`; set `Project: "/repo/a"` in `fullRow()`. `TestGroupable_MatchesWhatLabelForCanActuallyProduce` must pass after Step 3.
  - `query_test.go` `TestRecord_LabelsAreCapped...`: set `e.Project = huge` and add `"Project": rows[0].Project` to the checked map.
  - `project_test.go`:
    - `TestWriter_GroupProjectOverAMonth`: record priced events with projects `/repo/a`, `/repo/b`, `""` on two different days of one month (use `newTestWriter` and the clock pattern of `query_test.go`), flush, then `w.Snapshot(ctx, <month spec>, usage.GroupProject, "")` has series `/repo/a`, `/repo/b`, `unknown` with the right costs, and `Group == usage.GroupProject`.
    - `TestRow_AHistoricalRowWithNoProjectReadsAsUnknown`: decode a JSON line with no `project` key (copy the line shape of `TestRow_AHistoricalRowWithNoCurrencyDecodesAsUSD`), then `labelFor(row, usage.GroupProject)` returns `"unknown", true`, and `Row.Project == ""`.
    - `TestWriter_OverflowFoldsTheProjectToo`: in one minute record `maxLabelsPerMinute + 1` events that differ only in `Project`; after flush there are `maxLabelsPerMinute + 1` rows, at most one row has labels `(other)`, and that row has `Endpoint`, `Model`, `Agent`, `Provenance` and `Project` all `(other)`; the sum of `CostMicros` equals the sum recorded.
    - `TestSnapshot_AgentAndGroupProject`: rows from `claude-code` with projects `/repo/a`, `/repo/b` and from `bob` with `/repo/c`; `w.Snapshot(ctx, spec, usage.GroupProject, "claude-code")` has series `/repo/a`, `/repo/b` only.
    - `TestSeriesCurrenciesIn_GroupProject`: one project in two units gives both units (follow `currency_test.go:488`).

- [ ] **Step 2: Run, expect FAIL.** Run: `cd core && GOWORK=off go test ./cost/ledger/ -run 'Project|Groupable|LabelsAreCapped' -v`

- [ ] **Step 3: Implement.**
  - `row.go`: `Project string \`json:"project,omitempty"\`` after `Agent`, godoc: from `SessionEvent.Project`, stored as written ("" for absent, never `unknown`), mapped to `usage.UnknownProjectLabel` by `labelFor`. Add `project` to `key` (after `agent`), to `key()`, to `overflowKeyFor` (`overflowLabel`) and to `overflow` (`r.Project = overflowLabel`). Update the "ALL FOUR labels" and "Four labels at 96 bytes" comments to five. Update the `Row` godoc key tuple and `maxLabelsPerMinute` comment tuple to include project.
  - `writer.go` `Record`: `Project: rowLabel(e.Project),` beside `Agent`.
  - `query.go`: add `usage.GroupProject` to `Groupable`'s true case and to `SeriesCurrenciesIn`'s case; in `labelFor`, `case usage.GroupProject: if r.Project == "" { return usage.UnknownProjectLabel, true }; return r.Project, true`.
  - Fix `writer_test.go:978`'s keyed `key{...}` literal only if it fails to compile (it uses field names, so it should not).

- [ ] **Step 4: Run, expect PASS**, then the package. Run: `cd core && GOWORK=off go test -race ./cost/... && GOWORK=off go vet ./cost/...`

- [ ] **Step 5: Commit** `feat: Add the project to the cost ledger row`.

### Task T3: Session API tests for group=project

**Depends on:** T1, T2

**Files:**
- Modify: `core/sessionapi/usage.go` (the doc comment at line ~37 that lists groups: add `project`)
- Test: `core/sessionapi/usage_project_test.go` (new)

- [ ] **Step 1: Write the tests**, using `newTestLedger`, `usage.New()`, `newTestServer(t, WithUsage(agg), WithCostLedger(led))`, `fetchUsage` and the `agentResponse` helper from `usage_agent_test.go` (set `.Project` on the returned event). Events: `claude-code` with `/repo/a` ($0.25) and `""` ($0.50), `bob-shell` with `/repo/c` ($0.01). Record each into `led` and `agg`.
  - `TestHandleUsage_GroupProjectOnTheRing`: `?window=1h&group=project` gives `group == "project"` and folded series (`usage.FoldSeriesAcrossWindow`) `/repo/a`, `unknown`, `/repo/c`.
  - `TestHandleUsage_GroupProjectOnTheLedger`: `?window=today&group=project` gives the same three labels and costs.
  - `TestHandleUsage_AgentAndGroupProject`: for both `window=today` and `window=1h`, `?agent=claude-code&group=project` gives `group == "project"`, `agent == "claude-code"`, and series `/repo/a` and `unknown` only.

- [ ] **Step 2: Run, expect PASS** (T1 and T2 did the work). Run: `cd core && GOWORK=off go test -race ./sessionapi/ -run 'Project' -v`. If one fails, fix the cause in T1/T2 code, not in the test.

- [ ] **Step 3: Commit** `test: Pin group=project on the usage API`.

### Task T4: `agentop cost --by project`

**Depends on:** T1 (`usage.ParseGroup` must accept `project`)

**Files:**
- Modify: `cmd/agentop/cmd_cost.go` (`costByAxes` 1148, `ledgerServedAxes` 1163, usage text ~70-77, `writeCostBreakdown` 1190-1275)
- Test: `cmd/agentop/cmd_cost_test.go`

- [ ] **Step 1: Write the failing tests** with `fakeUsageServer`, following `TestRunCost_ByAgentPrintsARowPerAgent` (line 1813) for the reply shape:
  - `TestRunCost_ByProjectPrintsARowPerProject`: reply `"group":"project"` with one bucket whose series are `/repo/a`, `unknown` and a label of 60 runes such as `~/works/go/src/github.com/example/a-rather-long-repository-name`. `runCost` with `--by project` exits 0; output has a `PROJECT` header and one row for each label, the 60-rune label printed whole.
  - `TestRunCost_ByAlignsALongLabel`: from the same output, the byte offset of `REQUESTS`' right edge in the header line equals the offset of each row's requests cell right edge (compare `strings.Index` of the column end, or split on the two-space-padded label width). Also a reply with only short labels keeps the label column at 34 (header begins `"\n  " + fmt.Sprintf("%-34s", "AGENT")`).
  - `TestRunCost_UnknownByNamesTheAcceptedAxes` (existing, line ~2061): add a check that `costByAxes` and `ledgerServedAxes` both contain `project`.

- [ ] **Step 2: Run, expect FAIL.** Run: `scripts/dev/go-test-shards.sh -short cmd/agentop` (or `cd cmd/agentop && GOWORK=off go test -run 'RunCost_By' .`)

- [ ] **Step 3: Implement.**
  - `costByAxes = "agent, project, model, endpoint, currency, session, status, plugin or host"`.
  - `ledgerServedAxes = "agent, project, model, endpoint and currency"`; in its godoc, "naming all four of ledger.Groupable's axes" becomes "naming all five of ledger.Groupable's axes"; the usage text "breaks down by agent, model and endpoint" becomes "breaks down by agent, project, model, endpoint and currency"; add the example line `  agentop cost --by project        a row per project (git repo), costliest first`.
  - `writeCostBreakdown`: `width := 34`, raised to the largest `utf8.RuneCountInString(label)` among `labels` and of `asked`; print header and rows with `%-*s` and `width`. `%-*s` pads by runes, which is what is wanted. Truncate nothing. Applies to every axis.

- [ ] **Step 4: Run, expect PASS.** Run: `scripts/dev/go-test-shards.sh -short cmd/agentop && cd cmd/agentop && GOWORK=off go vet ./...`

- [ ] **Step 5: Commit** `feat: Add project to agentop cost --by`.

### Task T5: Docs

**Depends on:** none (every fact is in this plan)

**Files:**
- Modify: `docs/pricing.md` ("Reading usage over HTTP", line ~246-292)
- Modify: `docs/session-api.md` (the `project` field entry, line ~801)
- Modify: `cmd/agentop/README.md` (the `agentop cost` text: where `--by agent` is named, line ~1582, and any list of `--by` axes)

- [ ] **Step 1: Edit.**
  - `docs/pricing.md`: in the `group` row add `project` after `agent`. Add one sentence: `group=project` breaks the window down by the `project` label of each request (the git repo root of the agent process's cwd); a request with no project is the series `unknown`. A label longer than 96 bytes is cut to its first 96 bytes, in the ring and in the ledger alike. In the "ledger-backed window" paragraph, the grouped axes become `endpoint`, `agent`, `project`, `model` and `currency`. Add `agent=claude-code&group=project` as an example: a recognised agent with no `session=` keeps per-project series on a ring window too.
  - `docs/session-api.md`: at the end of the `project` entry add: "`/v1/usage?group=project` totals cost by this label; an event with no `project` is counted under `unknown`. See [pricing.md](pricing.md#reading-usage-over-http)."
  - `cmd/agentop/README.md`: say `agentop cost --by project` gives a row per project, costliest first, with `unknown` for traffic that had no project; the LABEL column widens to the longest label and cuts nothing; `--by` with `--agent` stays refused.

- [ ] **Step 2: Check** that no other doc lists the `group` values without `project`: `grep -rn "plugin, .host\|currency, .status" docs cmd/agentop/README.md`.

- [ ] **Step 3: Commit** `docs: Document cost by project`.

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: Cost by project in usage, ledger and agentop cost (#1816)` | "" | T1, T2, T3, T4, T5 | 650 |

Estimates: T1 ~160, T2 ~220, T3 ~90, T4 ~120, T5 ~30 (counted lines, tests included; this plan file does not count).

## Gate

Run from the worktree root:

```sh
test -z "$(gofmt -l core cmd/agentop cmd/cortex)" && (cd core && GOWORK=off go vet ./... && GOWORK=off go test -race ./cost/... ./sessionapi/... && GOWORK=off go test ./... && GOWORK=off go mod tidy -diff) && (cd cmd/cortex && TAGS=$(GOWORK=off go -C ../../scripts/profile-tags run . local) && GOWORK=off go vet -tags "$TAGS" ./... && GOWORK=off go test -tags "$TAGS" ./...) && (cd cmd/agentop && GOWORK=off go vet ./... && GOWORK=off go mod tidy -diff && GOWORK=off go test -race $(GOWORK=off go list ./... | grep -v 'agentop$')) && GOWORK=off scripts/dev/go-test-shards.sh -race cmd/agentop
```

Gate check (plan review, on base 97526c02): `gofmt -l core cmd/agentop cmd/cortex` printed nothing; `go vet` in `core`, `cmd/cortex` (with the `local` profile tags) and `cmd/agentop` passed; `go mod tidy -diff` in `core` and `cmd/agentop` passed. The gate needed no change. Test steps were not run here.

## For a human after review

- Live check on the laptop service (needs a `make dev-install`, which restarts the shared proxy on `:47600` and cuts every attached session; coordinate first). Needs P1 merged or checked out. Steps: run an agent in two repos, then `curl --noproxy '*' -s 'localhost:47601/v1/usage?window=today&group=project' | jq '.group, (.buckets[0].series|keys)'` shows both repo roots, and `agentop cost --by project` prints a row for each.
- Close #1816 when P1 merges, and update the #1739 checklist.
