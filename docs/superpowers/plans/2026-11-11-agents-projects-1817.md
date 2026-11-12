# Agents Pane: Expand a Row into Its Projects (#1817) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On the agentop AGENTS pane (`A`), `space` opens an agent row (or All agents) into one sub-row per project, with SESSIONS, LAST SEEN and COST per project.

**Architecture:** One `group=project` usage fetch per open row, folded across the window with the same code path as the agent rows. The table keeps one slice of row descriptors `{agent, kind, project}` that every cursor reader uses, so a sub-row always resolves to its parent agent. Everything is in `cmd/agentop/tui/` plus the README.

**Tech Stack:** Go 1.26.10, Bubble Tea, `core/cost/usage`.

**Spec:** https://github.com/rossoctl/cortex/issues/1817 (its "Decisions" 1-10 are fixed; this plan cites them as D1..D10). Parent #1739. #1815 (PR #1819, `SessionSummary.Projects`) and #1816 (PR #1821, `usage.GroupProject`) are merged into the base.

## Global Constraints

- All paths below are relative to the worktree root. Package: `cmd/agentop/tui` (package `tui`).
- Run module commands with `GOWORK=off` from `cmd/agentop`.
- Comments and commit messages in ASD-STE100 Simplified Technical English. Commits use `git commit -s` and end with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. No `Co-Authored-By`.
- `space` is the only new key (D1). Rows start closed; the open state is not persisted (D2).
- No `▸`/`▾`/indent at all when no listed session has a non-empty `Projects`: the pane is then byte-for-byte as today (D3).
- Do not raise `helpNoScrollHeight` (121, `help_overlay_test.go:557`).
- While iterating: `scripts/dev/go-test-shards.sh -short cmd/agentop`. Before the final commit: the gate command at the end.

## Decisions made in this plan (not in the issue)

1. **All projects figures.** Under a known agent, the `All projects` sub-row shows the agent's own row figures (`pickerRows` entry: COST, and `agentSessionsCell`/`agentLastSeen`), so opening a row never changes a number on screen. Under All agents, it shows the `group=project` snapshot's `Totals` with `Currencies` as units (`agentCostCellIn` prints `money.Mixed` for mixed units), and SESSIONS/LAST SEEN over every listed session that is not folded (`foldedInto(s) == ""`). Before the first reply, the All agents `All projects` row has blank COST.
2. **Fold reuse.** `foldOtherAgents` is generalised to `foldSeriesInto(snap, keep, into)`. The project `Other` label is the same string `"Other"` as `otherAgents`, so `agentRowsFromBuckets` already puts it last. A project label is a path (`/…` or `~…`), so no real project can be named `Other`.
3. **Overflow label.** `usage.overflowLabel` is unexported; the TUI declares `const projectOverflowLabel = "(other)"` with a comment that it mirrors `core/cost/usage/labels.go:44`.
4. **Where `space` is consumed.** Only when the row under the cursor can open (closed, expandable) or is an open heading or a sub-row. On every other row (`Other` agent, or no sessions with Projects) it falls through to the table, which pages down as today.
5. **Refetch hook.** Project rows refetch when an `agentRowsLoadedMsg` lands (the poll, `w`, `A` and esc-return all go through `fetchAgentRowsCmd`), so there is one hook, not four. `space` that opens a row fetches that row at once.
6. **`w` clears project data, not the open state,** so figures from the old window never show under the new title. Until the reply lands, an open row shows its heading and `All projects` only.
7. **Errors.** A failed project fetch keeps the old sub-rows and sets `m.agentProjectsErr`; any successful project fetch clears it. The pane's existing "(refresh failed: …)" line shows `m.agentsErr`, else `m.agentProjectsErr`.
8. **Footer full label** (D6): on a project sub-row the footer is `"[↑↓] nav  " + sanitizeLabel(project) + …`. `fitHintLine` drops from the front, so a narrow terminal gives up nav first, then the label.
9. **Session label match.** A project sub-row counts sessions whose `Projects` contains the usage series label exactly. A label longer than `event.MaxLabelLen` is truncated by the usage side only, so such a session counts under `Other` instead. `shortcut:` comment at the match, upgrade when a real path hits it.
10. **Older server fallback.** If `fetchUsageScoped` falls back (server without `agent=`), the snapshot has no project series; the open row then shows its heading and `All projects` only. No special code.

## Review Focus

1. **A session in two projects** counts in both project sub-rows (D8) — test `TestAgentsProjects_SessionInTwoProjectsCountsInBoth` in T3.
2. **The cursor on a sub-row when its parent agent vanishes** from a refetch (the agent left the window and has no sessions) — the cursor must land on a real row, never out of range; test `TestAgentsProjects_ParentGoneKeepsTheCursorInRange` in T3.
3. **A project label equal to a suffix of another** (`~/a/b` and `~/x/a/b`) — the shorter one shows its full label, the longer one a unique tail; test in T1 `TestProjectDisplayNames`.
4. **A project label with control characters** — the cell passes through `sanitizeLabel`, the footer too; covered by the same test as D6 in T3 (`TestAgentsProjects_FooterNamesTheFullLabel` uses a label with `\x1b`).
5. **`x` on a project sub-row** must never open the agent erase confirmation (data loss) — test `TestAgentsProjects_EraseAndServerDoNothingOnAProjectRow` in T3.

---

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: Expand an Agents row into its projects (#1817)` | "" | T1, T2, T3, T4 | 1170 |

One PR: the total is under 2,000 counted lines, and T3 needs T1 and T2.

## Tasks

| Id | Title | Depends on | estLines |
|---|---|---|---|
| T1 | Project rows and display names | — | 230 |
| T2 | Row descriptors for the agents table | — | 120 |
| T3 | Open and close rows with space | T1, T2 | 790 |
| T4 | README: space on the Agents pane | — | 30 |

T1, T2 and T4 can run at the same time.

---

### Task T1: Project rows and display names

Depends on: none.

**Files:**
- Modify: `cmd/agentop/tui/agents_other.go` (`foldOtherAgents`, lines ~49-120)
- Modify: `cmd/agentop/tui/agents_pane.go` (`agentRowsFromSnapshot`, lines 55-66)
- Create: `cmd/agentop/tui/agents_projects.go`
- Test: `cmd/agentop/tui/agents_projects_test.go` (create)

**Interfaces:**
- Produces:
  - `func foldSeriesInto(snap *usage.Snapshot, keep func(label string) bool, into string) *usage.Snapshot` — the current `foldOtherAgents` body with `knownAgentLabel` replaced by `keep` and `otherAgents` by `into`, SeriesCurrencies rule unchanged. `foldOtherAgents(snap)` becomes `return foldSeriesInto(snap, knownAgentLabel, otherAgents)`.
  - `func seriesRowsFromSnapshot(snap *usage.Snapshot) []agentRow` — today's `agentRowsFromSnapshot` without the fold. `agentRowsFromSnapshot(snap)` becomes `seriesRowsFromSnapshot(foldOtherAgents(snap))`.
  - `const otherProjects = otherAgents` and `const projectOverflowLabel = "(other)"` in `agents_projects.go`.
  - `func projectRowsFromSnapshot(snap *usage.Snapshot) []agentRow` — `seriesRowsFromSnapshot(foldSeriesInto(snap, namedProject, otherProjects))`, where `namedProject(l)` is `l != usage.UnknownProjectLabel && l != projectOverflowLabel`. Ordered by `usage.SortSeriesLabels`, `Other` last. `~` is a named project.
  - `func projectDisplayNames(labels []string) []string` — same length and order as `labels`. For each label, the shortest tail of `/`-separated segments that no other label in the slice has as its tail of the same segment count; if no tail is unique, the full label. A label with one segment (`~`) is itself.

- [ ] **Step 1: Write the failing tests** in `agents_projects_test.go`:

```go
func TestProjectDisplayNames(t *testing.T) {
	cases := []struct{ in, want []string }{
		{[]string{"~/works/cortex", "~/moca"}, []string{"cortex", "moca"}},
		{[]string{"~"}, []string{"~"}},
		{[]string{"~/works/go/src/github.com/kagenti/kagenti-extensions", "~/git/kagenti/kagenti-extensions"},
			[]string{"github.com/kagenti/kagenti-extensions", "git/kagenti/kagenti-extensions"}},
		{[]string{"~/a/b", "~/x/a/b"}, []string{"~/a/b", "x/a/b"}},
		{[]string{"/srv/app", "~/app", "~/tools"}, []string{"srv/app", "~/app", "tools"}},
	}
	// assert projectDisplayNames(c.in) equals c.want for each case
}

func TestProjectRowsFromSnapshot_FoldsUnknownAndOverflowIntoOther(t *testing.T) {
	// two buckets; series "~/works/cortex" 97_400_000 micros, "~/moca" 38_110_000,
	// usage.UnknownProjectLabel 10_000_000, "(other)" 5_520_000, "~" 1_000 (all PricedRequests>0)
	// want labels in order: "~/works/cortex", "~/moca", "~", "Other"; Other CostMicros 15_520_000
}

func TestProjectRowsFromSnapshot_UnitsFollowTheSeries(t *testing.T) {
	// SeriesCurrencies {"~/moca": {"credits"}}, Currencies {"USD","credits"}:
	// row "~/moca" units == ["credits"]; a series with no entry gets snap.Currencies
}
```

- [ ] **Step 2: Run** `cd cmd/agentop && GOWORK=off go test ./tui -run 'TestProjectDisplayNames|TestProjectRowsFromSnapshot' ` — expected: FAIL, undefined names.
- [ ] **Step 3: Implement** the Interfaces above. Existing tests in `agents_other_test.go` guard the refactor of `foldOtherAgents`.
- [ ] **Step 4: Run** the same command, then `GOWORK=off go test ./tui -run 'Other|AgentRows'` — expected: PASS.
- [ ] **Step 5: Commit** `feat: Fold project series into rows for the Agents pane`.

---

### Task T2: Row descriptors for the agents table

Depends on: none. No change in behavior.

**Files:**
- Modify: `cmd/agentop/tui/app.go:854-855` (field `agentRowLabels`)
- Modify: `cmd/agentop/tui/agents_pane.go` (`rebuildAgentsTable` 318-391, `selectedAgentScope` 481-499)
- Modify tests that set rows by hand: `cmd/agentop/tui/agents_cursor_scope_test.go:43-46`, `cmd/agentop/tui/spend_test.go:634-638`, `cmd/agentop/tui/spend_drawer_test.go:1182-1184` — replace the `m.agentsTbl.SetRows(...)` call with `m.rebuildAgentsTable()` (keep the following `setCursorVisible`), and fix the comment at `spend_test.go:634`.
- Test: `cmd/agentop/tui/agents_pane_test.go`

**Interfaces:**
- Produces (in `agents_pane.go`):

```go
type agentRowKind int

const (
	agentRowAgent        agentRowKind = iota // All agents ("") or an agent, closed
	agentRowHeading                          // an open agent row, figures blank
	agentRowAllProjects                      // first sub-row
	agentRowProject                          // one project
	agentRowProjectOther                     // the project Other sub-row
)

type agentRowDesc struct {
	agent   string // parent agent label; "" is All agents
	kind    agentRowKind
	project string // the usage series label; set only for agentRowProject
}
```

  - Model field `agentRowDescs []agentRowDesc` replaces `agentRowLabels`.
  - `func (m *model) selectedAgentRow() (agentRowDesc, bool)` — `m.agentRowDescs[m.agentsTbl.Cursor()]`, false when out of range.
  - `selectedAgentScope()` keeps its signature and returns `selectedAgentRow().agent`.
  - `rebuildAgentsTable` restores the cursor by exact descriptor match; when none matches, by the first descriptor with the same `agent`; else it keeps the index (clamped by `setCursorVisible`).

- [ ] **Step 1: Write the failing test** `TestAgentsPane_SelectedRowReadsTheDescriptors`: a model with `agents: navRows()`, `rebuildAgentsTable()`; cursor 0 → `agentRowDesc{agent: "", kind: agentRowAgent}`; cursor 2 → `{agent: "bob-shell/2.0.5", kind: agentRowAgent}`; cursor 9 → ok false.
- [ ] **Step 2: Run** `GOWORK=off go test ./tui -run TestAgentsPane_SelectedRowReadsTheDescriptors` — FAIL.
- [ ] **Step 3: Implement** the Interfaces. Update the comments on `rebuildAgentsTable` and `selectedAgentScope` (they now read descriptors, not `pickerRows` by index).
- [ ] **Step 4: Run** `scripts/dev/go-test-shards.sh -short cmd/agentop` from the worktree root — PASS.
- [ ] **Step 5: Commit** `refactor: Read the Agents rows through one descriptor slice`.

---

### Task T3: Open and close rows with space

Depends on: T1, T2.

**Files:**
- Modify: `cmd/agentop/tui/agents_projects.go` (from T1): state helpers, fetch, message, cells, toggle
- Modify: `cmd/agentop/tui/agents_pane.go` (`rebuildAgentsTable`, `cycleAgentsWindow`)
- Modify: `cmd/agentop/tui/app.go`: model fields beside `agentsErr` (~856); the `agentRowsLoadedMsg` arm (~1559-1610) adds `m.fetchOpenProjectsCmd()` to the commands it returns; a new `agentProjectsLoadedMsg` arm; the render at ~2662-2674 shows `m.agentsErr`, else `m.agentProjectsErr`
- Modify: `cmd/agentop/tui/keys.go`: `space` in the `paneAgents` fall-through arm (~1085, before `m.agentsTbl.Update`); footer `paneAgents` (~1424-1452)
- Modify: `cmd/agentop/tui/clear.go:80-89` (`openAgentErase`), `cmd/agentop/tui/server_picker.go:129-140` (`serverKeyOffered`, `openServerPicker`)
- Modify: `cmd/agentop/tui/help_overlay.go:400-434`
- Test: `cmd/agentop/tui/agents_projects_test.go`

**Interfaces:**
- Consumes: T1 `projectRowsFromSnapshot`, `projectDisplayNames`, `otherProjects`; T2 `agentRowDesc`, kinds, `selectedAgentRow`, `agentRowDescs`.
- Produces:
  - model fields: `agentsExpanded map[string]bool` (key: agent label, "" = All agents), `agentProjects map[string]agentProjectsData`, `agentProjectsErr error`.
  - `type agentProjectsData struct { all agentRow; rows []agentRow }` — `all` is `{Counts: snap.Totals, units: snap.Currencies}`, `rows` is `projectRowsFromSnapshot(snap)`.
  - `type agentProjectsLoadedMsg struct { agent string; data agentProjectsData; err error; window string }`. The arm drops a reply whose `window != m.agentsSpanDef().window`; on error sets `agentProjectsErr` and keeps old data; on success stores data, clears the error; rebuilds the table when `m.pane == paneAgents`.
  - `func (m *model) fetchAgentProjectsCmd(agent string) tea.Cmd` — nil without a client; window and resolution from `m.agentsSpanDef()` captured on the update goroutine; timeout `agentsFetchTimeout`; agent "" → `client.GetUsageWindow(ctx, w, res, "", usage.GroupProject)`; else `fetchUsageScoped(ctx, client, w, res, agent, usage.GroupProject)`.
  - `func (m *model) fetchOpenProjectsCmd() tea.Cmd` — `tea.Batch` of the above for each open label.
  - `func (m *model) sessionsNameProjects() bool` — beside `sessionsNameAgents`: any `m.sessions` entry with `len(s.Projects) > 0`.
  - `func (m *model) agentRowExpandable(label string) bool` — `label != otherAgents && m.sessionsNameProjects()`.
  - `func (m *model) subRowSessions(d agentRowDesc) (n int, last time.Time)` — over `m.sessions` with `foldedInto(s) == ""`; agent test `agentRowLists(s, d.agent)` unless `d.agent == ""`; then `agentRowProject`: `slices.Contains(s.Projects, d.project)`; `agentRowProjectOther`: `len(s.Projects) == 0`; `agentRowAllProjects`: no project test.
  - `func (m *model) toggleAgentRow() (tea.Cmd, bool)` — closed expandable agent row: open, rebuild, return `fetchAgentProjectsCmd(agent), true`. Heading or any sub-row: close `d.agent`, rebuild, put the cursor on the parent's row, return `nil, true`. Else `nil, false`.
  - `func (m *model) selectedAgentActs() (string, bool)` — `selectedAgentScope`, but false on `agentRowProject` and `agentRowProjectOther`. `openAgentErase`, `serverKeyOffered` and `openServerPicker` call it instead of `selectedAgentScope`. Enter (`keys.go:642`) and `spendScopeAgent` (`spend.go:1032`) keep `selectedAgentScope`, which gives the parent (D9, D10).

**Rows** (`rebuildAgentsTable`), when `m.sessionsNameProjects()`:
- AGENT cell prefix: `"▸ "` closed expandable, `"▾ "` open, `"  "` not expandable; sub-rows `"    "` + label. All cells through `sanitizeLabel`. With no sessions naming projects: no prefix, rows as today.
- An open row (agent or All agents) is a heading: `▾ label` and every other cell `""` (D4). Then sub-rows: `All projects`; one row per `agentProjects[agent].rows` entry except `Other`, labelled by `projectDisplayNames` over those labels; then `Other` unless its `CostMicros == 0` and `subRowSessions` counts 0 (D5).
- COST of a project/Other sub-row: `agentCostCellIn(row.Counts, row.units, agentsCostWidth)`. All projects: decision 1 above. SESSIONS: `emptyCell` when n is 0, else `formatCount(n)`; LAST SEEN `relTime(now, last)`. SERVER column (when routed): the agent's `agentServerCell` on All projects under a known agent, `""` on every other sub-row and on headings.
- An open label whose agent has no row any more is skipped.

**Keys and footer:**
- `space` (`msg.String() == " "`): `if cmd, ok := m.toggleAgentRow(); ok { return cmd }` before the table update. The search prompt already returns earlier (`keys.go:41`).
- `cycleAgentsWindow` sets `m.agentProjects = nil` (decision 6).
- Footer: after `enterHint`, add `"  [space] projects"` on a closed expandable row and `"  [space] close"` on a heading or sub-row. On `agentRowProject`, insert `"  " + sanitizeLabel(d.project)` right after `"[↑↓] nav"`. `enterHint` reads the parent (unchanged code path; on a sub-row under All agents it says `[↵] all agents`).

**Help overlay** (`help_overlay.go` paneAgents): add binding `{"space", "open or close this row's projects"}` after `↵`. Replace the note with one that also says, briefly: project COST adds up to All projects, a session in two projects counts in both, and spend from before the upgrade is in Other. Shorten wording until `TestHelpOverlayScrollHint_AbsentWhenEverythingFits` passes without raising `helpNoScrollHeight`. Example: `"Opens by itself when two or more agents have been seen. It scopes the sessions list, the usage pane and the spend band. Other pools unrecognised agents. Projects: COST adds up to All projects, SESSIONS counts a session in each of its projects; spend from before the upgrade is in Other."`

- [ ] **Step 1: Write the failing tests** in `agents_projects_test.go`. Add a helper `projectsModel(t, series map[string]string) (*model, func() []string)`: an `httptest` server that records `r.URL.RawQuery`, echoes `window`, `group` and `agent`, and returns as one bucket's `series` the JSON for the request's agent (`series[q.Get("agent")]`, or `{}`), with matching `totals`; model as `cursorScopeModel` (width 120, height 40, `paneAgents`, `newAgentsTable()`), `agents` with `claude-code` (CostMicros 151_030_000, PricedRequests 5) and `codex` (30_900_000, PricedRequests 2) and `Other`, `sessions` where claude-code sessions have `Projects: ["~/works/cortex"]`, `["~/moca"]`, `["~/works/cortex", "~/moca"]`, `nil`; then `rebuildAgentsTable()`. A helper `deliver(m, cmd)` runs the cmd and passes each `agentProjectsLoadedMsg` through `m.Update`. Tests:
  - `TestAgentsProjects_NoProjectsNoMarkerAndSpacePagesDown` — sessions without Projects: no cell contains `▸`; `space` returns no fetch and the cursor moves as the table's page-down (compare with pressing `pgdown` on a copy).
  - `TestAgentsProjects_SpaceOnAnAgentFetchesItsProjects` — cursor on claude-code; `space`; recorded query contains `agent=claude-code` and `group=project`; after delivery AGENT cells are `"▾ claude-code"`, `"    All projects"`, `"    cortex"`, `"    moca"`, `"    Other"`; COST cells blank on the heading, `$151.03` on All projects, `$97.40`, `$38.11`, `$15.52` (series: `~/works/cortex` 97_400_000, `~/moca` 38_110_000, `unknown` 10_000_000, `(other)` 5_520_000).
  - `TestAgentsProjects_SpaceOnAllAgentsFetchesWithoutAgent` — query has `group=project` and no `agent=`.
  - `TestAgentsProjects_SharedTailsShowTheShortestUniqueTail` — series with the two `kagenti-extensions` labels from D6; cells `"    github.com/kagenti/kagenti-extensions"` and `"    git/kagenti/kagenti-extensions"`.
  - `TestAgentsProjects_OtherIsAbsentWhenZeroAndEmpty` — no `unknown`/`(other)` series and every session has Projects: no `"    Other"` row.
  - `TestAgentsProjects_OpenStateAndCursorSurviveRefetchAndW` — open claude-code, cursor on `moca`; deliver an `agentRowsLoadedMsg` (rows reordered), then press `w` and deliver; row still open, cursor still on the `moca` descriptor.
  - `TestAgentsProjects_SpaceOnASubRowClosesTheParent` — cursor on `cortex`; `space`; row closed, cursor on `"▸ claude-code"`.
  - `TestAgentsProjects_EnterOnAProjectRowScopesTheParent` — Enter on `cortex`; `m.agentScope == "claude-code"`, `m.pane == paneSessions`.
  - `TestAgentsProjects_EraseAndServerDoNothingOnAProjectRow` — `x` and `S` on `cortex` and on `Other` sub-row: `m.clearConfirm == nil`, `m.serverNotice == ""`, footer has no `[S]`; on `All projects` `x` opens `clearConfirm` with `agent: "claude-code"`.
  - `TestAgentsProjects_BandFollowsTheParentAgent` — cursor on `moca`: `m.spendScopeAgent() == "claude-code"`, and `bandAgents` of the recorded queries after a band fetch is `["claude-code"]`.
  - `TestAgentsProjects_OtherAgentNeverExpands` — `Other` agent row AGENT cell starts with `"  "`, not `▸`; `space` there opens nothing.
  - `TestAgentsProjects_SessionInTwoProjectsCountsInBoth` — SESSIONS on `cortex` is `2`, on `moca` `2`, on `Other` `1`, on `All projects` `4`.
  - `TestAgentsProjects_ParentGoneKeepsTheCursorInRange` — cursor on `moca`, then an `agentRowsLoadedMsg` without claude-code and sessions cleared: `selectedAgentRow` ok.
  - `TestAgentsProjects_FooterNamesTheFullLabel` — project label `"~/works/cor\x1btex"`: footer contains `sanitizeLabel` of it and `[space] close`; on a closed claude-code row the footer contains `[space] projects`.
  - `TestAgentsProjects_FailedFetchKeepsTheRows` — open, deliver success, then deliver an error reply: rows unchanged, view contains `refresh failed`.
- [ ] **Step 2: Run** `cd cmd/agentop && GOWORK=off go test ./tui -run TestAgentsProjects` — FAIL.
- [ ] **Step 3: Implement** the Interfaces, rows, keys, footer and help overlay above. Update the comment at `keys.go:1082-1092` (space now opens rows here).
- [ ] **Step 4: Run** `GOWORK=off go test ./tui -run 'TestAgentsProjects|TestHelpOverlay|TestAgentsPane|TestSpend'`, then `scripts/dev/go-test-shards.sh -short cmd/agentop` from the worktree root — PASS.
- [ ] **Step 5: Commit** `feat: Open an Agents row into its projects with space`.

---

### Task T4: README: space on the Agents pane

Depends on: none.

**Files:**
- Modify: `cmd/agentop/README.md` — the keybindings table (~1515-1535) and the Agents prose (~1590-1612)

Facts to write (from the issue and this plan):
- In the `P` row (~1522), change "`space` and `d` are the table's page-down and half-page-down" to say that `space` and `d` page the table, except on the agents pane, where `space` opens projects and `d`, `PgDn` and `f` still page down.
- Add a row after the `↑↓` / `jk` agents row: `` | `space` | agents | open the row under the cursor into its projects, or close it; on a sub-row it closes the parent and moves the cursor there. Offered only when sessions record a project; the `Other` row never opens | ``.
- In the `↵` agents row, add: on a project sub-row, `↵` scopes to the parent agent. `x` and `S` act on the parent on the open row and on `All projects`, and do nothing on a project or `Other` sub-row.
- One paragraph in the Agents prose: an open row shows `All projects` (the agent's figures), one row per project (the last path segment, or the shortest unique tail of segments when two share it, for example `github.com/kagenti/kagenti-extensions` and `git/kagenti/kagenti-extensions`), then `Other`. `Other` holds traffic with no project (no PID, or an app started by macOS with cwd `/`) and the overflow past 16 series. COST of the projects and `Other` adds up to `All projects`; SESSIONS does not, because a session that used two projects counts under both. Spend recorded before the upgrade has no project and is in `Other`: for the month window that can be most of the spend for up to a month. Rows start closed and stay open across the 20s refresh, `w` and `A`, not across a restart. The spend band and drawer follow the parent agent on a sub-row.

- [ ] **Step 1:** Edit the README as above.
- [ ] **Step 2:** `pre-commit run --files cmd/agentop/README.md` (if installed) — PASS.
- [ ] **Step 3: Commit** `docs: Describe projects on the Agents pane`.

---

## For a human after review

- **Live check on the laptop Cortex** (needs T1-T3 merged or built): `make dev-install` restarts the shared proxy on `:47600` and cuts every attached session, so coordinate first. Then run `agentop`, press `A`, put the cursor on `claude-code`, press `space`, and check that the projects match `agentop cost --by project` for the same window; press `w` and check that the rows stay open.

## Gate

From the worktree root:

```sh
(cd cmd/agentop && GOWORK=off go vet ./... && test -z "$(gofmt -l .)") && scripts/dev/go-test-shards.sh -race cmd/agentop && (cd cmd/agentop && GOWORK=off go test -race $(GOWORK=off go list ./... | grep -v 'agentop$'))
```
