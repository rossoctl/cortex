# agentop Tool Rows Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show each tool call as its own row in agentop's events table, under the exchange whose response asked for it, with a measured duration that the existing sort and search use.

**Architecture:** The events pane runs the Timeline pane's existing fetch chain for the selected session. Each fetched `*timeline.Timeline` is indexed once into a map from response seq to tool rows. `rebuildEventsTable` adds the tool rows after their exchange row. A tool row has cells up to DURATION and one last cell that spans the rest of the table (a new `tui/table` rule).

**Tech Stack:** Go 1.26.10, Bubble Tea, the repo's own `cmd/agentop/tui/table` package.

**Spec:** issue rossoctl/cortex#1776, section "Spec (v1)" (rules 1-16, facts 1-8, tasks 1-6). Read it with `gh issue view 1776 -R rossoctl/cortex`. This plan refers to its rules by number ("rule 8").

## Global Constraints

- No new API field and no change under `core/`. All data comes from `GET /v1/sessions/{id}/timeline` (`apiclient.Client.GetTimeline`).
- Not in v1: Enter on a tool row, totals per tool name, a "waiting for you" label.
- End-of-run tool names are exactly `StructuredOutput` and `SubagentHandback`. Do not add others.
- Tool rows come from the timeline's `tools[]`, never from events' `ToolCalls`/`ToolResults` (agentop loads `view=summary`, which strips them).
- Log nothing new. No new dependency.
- Code comments, commit messages and PR body in ASD-STE100 Simplified Technical English. Commits use `git commit -s` and end with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. No `Co-Authored-By`.
- Run `cmd/agentop` tests while you work with `scripts/dev/go-test-shards.sh -short cmd/agentop` (from the repo root).

## Decisions made for this plan (with reasons)

1. **One shared `timelineState`.** The events pane uses `m.timeline`, not a second state. Reason: the spec (task 1) says so, and a second chain doubles the fetches when the user opens `T` from the events pane. Setting `m.timeline.session = m.selectedSess` adds no #1542 retention: `watchingSession` already holds `selectedSess`.
2. **Chain runs on `paneEvents` and `paneDetail`** when `m.timeline.session == m.selectedSess`. Reason: Esc from detail returns to the table, and the rows must be current there. On any other pane the tick stops, as now.
3. **Join check.** A tool joins a row only when `exchangeResp(row) != nil`, `exchangeResp(row).Seq == tool.Seq` and `eventSession(m.selectedSess, resp) == m.selectedSess`. Reason: linked-bucket events carry seqs of another session (`linked.go`), and the timeline is for `selectedSess` only.
4. **`error` STATUS is plain text.** Reason: the events table has no per-cell color (see `newEventsTable`). "The error style that exchange rows use" is therefore no style.
5. **Tool row cells.** Walk the shown columns in order. Columns `#`, TIME, DIR, ACTION, PLUGIN, METHOD, STATUS, DURATION get the tool cell (rules 2-8). At the first shown column outside that set, append the trailing cell (rule 10) and stop. If no such column is shown, the trailing text is not drawn (search still sees it). Reason: these eight columns are first in `eventColumns` display order, so the cells stay parallel to `cols` when the picker or the fit drops a column.
6. **Sort key for "other columns".** `sortValue{numeric: k.numeric}`, where `k` is `sortCol.sortKey(ctxs[0])`. Reason: `sortValue.less` reads `numeric` from its receiver, so a non-numeric zero beside numeric keys gives an inconsistent order. Tool rows exist only when `ctxs` is not empty.
7. **Pin key.** Add `tool, toolAgent string` to `eventKey`. A new `pinKeyOf(er eventRow) eventKey` returns `{tool: id, toolAgent: agent}` for a tool row and `keyOf(er.event)` otherwise. Every `keyOf(m.selectedEvent())` and `keyOf(m.selectedEventOn(...))` caller changes to `pinKeyOf` of the same row. Reason: rule 14, and `keyOf(nil)` is the zero (unpinned) key. Do not name it `rowKey`: `events_columns.go` already declares `type rowKey struct` (the sort key), so that name does not compile.
8. **Index location.** The index is a field `tools map[uint64][]toolRow` on `timelineState`, set in `handleTimelineLoaded` on success and cleared with `tl` when the session changes. Reason: spec task 2, "once per fetched timeline".
9. **`now`** for `running`, `run ⋯` and `bg ⋯` is `m.clock()` read once per rebuild. Reason: the model already injects a clock for tests.
10. **One PR.** The estimate is about 1,400 counted lines, under the 2,000 limit.

## Review Focus

1. A timeline that arrives before the snapshot (no event rows yet): no panic, no tool rows; the rows show on the next rebuild. Test in Task 4 (`TestToolRows_TimelineBeforeEvents`).
2. A timeline from a previous session left in `m.timeline` after the user opens another session: no tool rows of the old session. Test in Task 1 (`TestEventsTimeline_SessionChangeDropsTools`).
3. A tool whose `Seq` matches no visible row (evicted or older page not loaded): it is not shown and nothing breaks. Test in Task 4 (`TestToolRows_OrphanToolNotShown`).
4. A narrow terminal that drops DURATION and the columns after it: tool rows still have no more cells than columns (the table panics on more). Test in Task 4 (`TestToolRows_NarrowTerminalCellCount`).
5. Detail search (`/` then tab) and `n`/`N` step over tool rows without a nil dereference. Test in Task 4 (`TestToolRows_DetailSearchSkipsToolRows`).

---

### Task 1: Timeline data on the events pane

**Depends on:** none

**Files:**
- Modify: `cmd/agentop/tui/timeline_pane.go` (`timelineState`, `openTimeline`, `handleTimelineLoaded`, `handleTimelineTick`, `openEventsAtSeq`)
- Modify: `cmd/agentop/tui/keys.go` (Enter on a session near line 718; Esc from the timeline near line 565; Esc/back from detail)
- Modify: `cmd/agentop/tui/app.go` (stale marking near line 2414; `timelineLoadedMsg` case)
- Test: `cmd/agentop/tui/events_timeline_test.go` (new)

**Interfaces:**
- Produces: `func (m *model) followEventsTimeline() tea.Cmd` — sets `m.timeline.session = m.selectedSess` (resetting `tl`, `err`, `fails`, `retryAt`, `cursor=-1`, `scroll=0` when the session changes) and returns `m.resumeTimeline()`. Returns nil when `m.selectedSess == ""`.
- Produces: `func (m *model) timelineChainLive() bool` — true when `m.pane == paneTimeline`, or `m.pane` is `paneEvents`/`paneDetail` and `m.timeline.session == m.selectedSess != ""`.
- Produces: `handleTimelineLoaded` calls `m.rebuildEventsTable()` after a successful load when `m.pane == paneEvents && m.timeline.session == m.selectedSess`. This task does not add the `tools` field; Task 4 adds it, fills it with `indexTools` (Task 2), and adds `m.timeline.tools = nil` to `followEventsTimeline`, so Task 1 and Task 2 stay independent.

- [ ] **Step 1: Write the failing tests** in `events_timeline_test.go`, built like `TestTimelineRefresh` in `timeline_pane_test.go`:
  - `TestEventsTimeline_ChainStartsOnEventsPane`: Enter on a session in the sessions pane returns a cmd; after it, `m.timeline.session == m.selectedSess` and a `timelineTickMsg{gen: m.timeline.tickGen}` handled on `paneEvents` returns a non-nil cmd (the next tick).
  - `TestEventsTimeline_StreamMarksStale`: on `paneEvents`, a streamed event for `selectedSess` sets `m.timeline.stale`; an event of another session does not.
  - `TestEventsTimeline_NotFoundGivesNoError`: a `timelineLoadedMsg{err: apiclient.ErrNotFound}` on the events pane leaves `m.timeline.tl == nil` and puts no flash or error in the events view (`m.View()` does not contain "timeline").
  - `TestEventsTimeline_TickStopsOffPane`: on `paneSessions`, `handleTimelineTick` returns nil.
  - `TestEventsTimeline_EscFromTimelineResumes`: open `T` from the events pane, then Esc: pane is `paneEvents`, `m.timeline.session == m.selectedSess`, and a cmd is returned.
  - `TestEventsTimeline_SessionChangeDropsTools`: with `tl` loaded for session "a", opening session "b" sets `m.timeline.tl == nil`.

- [ ] **Step 2: Run them, expect FAIL.** `go test ./tui -run TestEventsTimeline -count=1` from `cmd/agentop`.

- [ ] **Step 3: Implement.**
  - `handleTimelineTick`: replace `m.pane != paneTimeline` with `!m.timelineChainLive()`.
  - `app.go` stale marking: replace `m.pane == paneTimeline && m.timeline.session == e.SessionID` with `m.timelineChainLive() && m.timeline.session == e.SessionID`.
  - Enter on a session (keys.go, after `m.rebuildEventsTable()`): batch `m.followEventsTimeline()` with the snapshot cmd it returns; also return it on the cached-only path.
  - `openEventsAtSeq`: replace `m.timeline.session = ""` with a call to `followEventsTimeline()` and batch its cmd with the snapshot cmd. Update the comment: the session stays the same as `selectedSess`, so it adds no retention.
  - Esc from the timeline (keys.go ~565): after the pane is restored, if it is `paneEvents` or `paneDetail` and `m.selectedSess != ""`, return `m.followEventsTimeline()` instead of clearing `m.timeline.session`; else keep today's clear.
  - Back from detail to events (keys.go `case paneDetail: m.pane = paneEvents`): no change needed, the chain runs on both panes.
  - `handleTimelineLoaded`: after success, rebuild the events table as in Interfaces. `timelineLoadedMsg` case in `app.go` keeps `harvestForTimeline`.

- [ ] **Step 4: Run tests, expect PASS**, and run the existing `go test ./tui -run 'TestTimeline' -count=1` (the Timeline pane must not change).

- [ ] **Step 5: Commit** `feat: Fetch the timeline while the events pane shows a session`.

---

### Task 2: Tool index

**Depends on:** none

**Files:**
- Create: `cmd/agentop/tui/tool_rows.go`
- Test: `cmd/agentop/tui/tool_rows_test.go`

**Interfaces:**
- Produces:

```go
// toolRow is one tool call of the timeline, with the figures its events-table row shows.
type toolRow struct {
	timeline.Tool
	Parallel bool       // another tool of the same response has the same End
	EndsRun  bool       // Lost and Name is StructuredOutput or SubagentHandback
	RunFrom  *time.Time // SpawnedAt of the agent whose SpawnTool is this tool's ID
	RunTo    *time.Time // its ReturnedAt; nil while the subagent runs
	BgFrom   *time.Time // Start of this Bash tool's job (fact 4)
	BgTo     *time.Time // the job's End; nil while it runs
}

func indexTools(tl *timeline.Timeline) map[uint64][]toolRow // key: Tool.Seq; order: tl.Tools order; nil for nil tl
func (t toolRow) status() string                            // rule 7: "running", "error", "lost", "ok" or ""
func (t toolRow) duration(now time.Time) time.Duration      // rule 11 DURATION sort value
func (t toolRow) durationCell(now time.Time) string         // rule 8
func (t toolRow) trailing(now time.Time) string             // rule 10, parts joined with " · "
```

- [ ] **Step 1: Write failing table tests** in `tool_rows_test.go` (reuse `tlAt`/`tlPtr` from `timeline_pane_test.go`):
  - `TestIndexTools_GroupsBySeqInOrder`: tools with Seq 5,5,7 → `len(idx[5]) == 2`, names in input order.
  - `TestIndexTools_Parallel`: two tools of Seq 5 with the same End → both `Parallel`; a third tool of Seq 5 with another End → not `Parallel`; a single tool → not `Parallel`; tools with nil End are never `Parallel`.
  - `TestToolRow_DurationCell`: ended 13.21s parallel → `"∥13.21s"`; ended 300ms → `"300ms"`; running started 38s before `now` → `"⋯ 38s"` (format `"⋯ " + formatDuration(now-start)`, and `formatDuration(38s)` is `"38.00s"`, so expect `"⋯ 38.00s"`); lost → `""`; end-of-run → `""`.
  - `TestToolRow_Status`: nil End → `"running"`; `Error` → `"error"`; `Lost` + `Bash` → `"lost"`; `Lost` + `StructuredOutput` → `""`; ended → `"ok"`.
  - `TestIndexTools_AgentRun`: agent `{SpawnTool: "t1", SpawnedAt: tlPtr(10), ReturnedAt: tlPtr(358)}` → `trailing` starts `"run 5m48s · "` followed by the preview; with nil ReturnedAt and `now = tlAt(40)` → `"run ⋯ 30.00s · <preview>"`.
  - `TestIndexTools_BgJob`: Bash tool ending at 20 for agent "main", job `{Agent: "main", Start: tlAt(20), End: tlPtr(262)}` → `"bg 4m02s · <preview>"`; job with nil End → `"bg ⋯ …"`. Two parallel tools ending at 20, one `Bash` and one `Read`: only the Bash one gets `bg`. A job of another agent matches nothing.
  - `TestToolRow_EndsRunTrailing`: lost `SubagentHandback` with preview "x" → trailing `"ends run · x"`; empty preview → `"ends run"`.
  - `TestToolRow_DurationSortValue`: running → `now-start`; lost and end-of-run → 0; ended → `End-Start`.

- [ ] **Step 2: Run, expect FAIL** (`go test ./tui -run 'TestIndexTools|TestToolRow' -count=1`).

- [ ] **Step 3: Implement `tool_rows.go`.** Build a `map[string]timeline.Agent` by `SpawnTool` and index jobs by `(Agent, Start.UnixNano())` once, then one pass over `tl.Tools`. Parallel: per Seq, count End values (`UnixNano`). End-of-run names in a package-level `map[string]bool` with a one-line comment that names `core/session/timeline/agents.go` as the source. Trailing parts order: run, bg, ends run, preview; skip empty parts.

- [ ] **Step 4: Run, expect PASS.**

- [ ] **Step 5: Commit** `feat: Index timeline tools by the response that asked for them`.

---

### Task 3: Spanning last cell in `tui/table`

**Depends on:** none

**Files:**
- Modify: `cmd/agentop/tui/table/table.go` (`renderRow`, comment above it)
- Test: `cmd/agentop/tui/table/table_test.go`

**Interfaces:**
- Produces: rule "a row with 1 < len(cells) < len(cols) renders cells 0..n-2 in their columns and cell n-1 across the remaining columns, width = sum of the remaining columns' `Width` plus `2*(k-1)` cell padding for the k columns it covers". Rows of other shapes render as today.

- [ ] **Step 1: Check callers.** `grep -rn "table.Row{" cmd/agentop/tui --include=*.go | grep -v _test` and read each row builder that can produce fewer cells than columns. Expected: none outside the one-cell section rows. Write the result in the commit message body. If one exists, stop and report it in the task review: the rule would change that caller.

- [ ] **Step 2: Write failing tests** in `table_test.go`:
  - `TestRenderRow_LastCellSpans`: columns widths 4,6,6,6; row `{"a","b","long text that is twenty"}` → the rendered line (ANSI stripped) contains `"long text that is twenty"` up to the width that 2 columns plus padding give, and the line width equals a full 4-cell row's width.
  - `TestRenderRow_SpanTruncatesAtTableWidth`: a trailing cell longer than the span ends with `"…"` and the line is not wider than a full row.
  - `TestRenderRow_SpanKeepsCursorAndMatchStyles`: the cursor row's spanning cell carries the `Selected` style; a search hit inside the spanning cell is painted with `Match` (use the existing helpers in `search_test.go`).

- [ ] **Step 3: Run, expect FAIL** (`go test ./tui/table -count=1`).

- [ ] **Step 4: Implement** in `renderRow`: when `1 < len(row) < len(cols)`, compute the span width over the remaining columns with `Width > 0` and render the last cell in one box of that width; `cellSpans(r, n-1, text)` still indexes the search cell by its row position. Update the doc comment.

- [ ] **Step 5: Run, expect PASS. Commit** `feat: Let a short table row's last cell span the remaining columns`.

---

### Task 4: Tool rows in `rebuildEventsTable`

**Depends on:** Task 1, Task 2, Task 3

**Files:**
- Modify: `cmd/agentop/tui/events_pane.go` (`eventKey`, `findByKey`, `eventRow`, `rebuildEventsTable`, `selectedEventOn` callers)
- Modify: `cmd/agentop/tui/events_columns.go` (tool cells and sort keys)
- Modify: `cmd/agentop/tui/timeline_pane.go` (`handleTimelineLoaded` sets `m.timeline.tools = indexTools(msg.tl)`; reset `tools` wherever `tl` is reset)
- Modify: `cmd/agentop/tui/keys.go` (Enter on a tool row; `keyOf(m.selectedEvent())` at ~1030, 1088, 1107, 1144)
- Modify: `cmd/agentop/tui/search_surfaces.go` (lines ~108, ~126), `cmd/agentop/tui/timeline_pane.go` `revealPendingSeq` (~532)
- Test: `cmd/agentop/tui/events_tool_rows_test.go` (new)

**Interfaces:**
- Consumes: `toolRow`, `indexTools`, `status`, `durationCell`, `duration`, `trailing` (Task 2); `followEventsTimeline` and the rebuild on load (Task 1); the spanning cell (Task 3).
- Produces: `eventRow.tool *toolRow`, `eventRow.parent int` (the parent exchange's index in `ctxs`), `eventRow.last bool` (last tool of its exchange); `eventKey.tool`, `eventKey.toolAgent`; `func pinKeyOf(er eventRow) eventKey`; `func (m *model) selectedRowOn(row int) eventRow`; `func toolCells(cols []eventColumn, cc cellContext, t eventRow, sorted bool, now time.Time) table.Row` in `events_columns.go`.

- [ ] **Step 1: Write failing tests** in `events_tool_rows_test.go`. Build a model like `rebuildBenchModel` (events with `Seq` set on responses) and set `m.timeline.session = "s"`, `m.timeline.tl`, `m.timeline.tools = indexTools(tl)`, and a fixed `m.clock`. Read rendered cells from `m.eventsTbl.Rows()`:
  - `TestToolRows_OneTool`: row after exchange 12 has `#` `"┗⚒"`, empty DIR, TIME = tool start in `"15:04:05.00"`, METHOD `"Bash"`, STATUS `"ok"`, DURATION `"4.21s"`, last cell = preview; `len(row) == index of first column after DURATION + 1`.
  - `TestToolRows_Parallel`: two tools → `"┣⚒"` then `"┗⚒"`, both DURATION start with `"∥"`.
  - `TestToolRows_States`: running (`"running"`, `"⋯ …"`), error (`"error"`), lost (`"lost"`, empty DURATION), end-of-run (empty STATUS and DURATION, last cell starts `"ends run"`).
  - `TestToolRows_RunAndBg`: Agent tool last cell starts `"run "`; Bash with job starts `"bg "`.
  - `TestToolRows_InExpandedBlock`: subagent tools render right after the member exchange inside the expanded block, DIR cell `"│"`; collapsed block → no tool rows.
  - `TestToolRows_HideInactiveHidesTools`: with `hideInactive` and an inactive exchange, its tools are gone too.
  - `TestToolRows_SortByDuration`: `m.sortCol = colDuration`, `m.sortDesc = true`: a 38s running tool ranks above a 4.4s exchange; its `#` is `"↳14"` (the parent's `#`).
  - `TestToolRows_SortOtherColumnIsStable`: sort by COST: tool rows keep their place after their exchange among ties (tie-break by `parent`).
  - `TestToolRows_SearchFindsPreview`: `m.search[paneEvents] = "git -C"` → the tool row index is in `m.eventsTbl.Matches()`.
  - `TestToolRows_EnterDoesNothing`: cursor on a tool row, Enter → pane stays `paneEvents`, cmd nil, `m.selectedEvent() == nil`.
  - `TestToolRows_CursorStaysOnTool`: cursor on a tool row, then a new event arrives and a sort is toggled → cursor is on the same tool (`pinKeyOf` equal).
  - `TestToolRows_TailFollowOnToolRow`: no sort, cursor on the last row, which is a tool row; a new exchange with a tool arrives → after the rebuild the cursor is on the new last row, and `m.selectedEvent() == nil` does not panic. (Yank needs no test: it runs only in the detail pane, and a tool row cannot open the detail pane, rule 13.)
  - Review Focus tests: `TestToolRows_TimelineBeforeEvents`, `TestToolRows_OrphanToolNotShown`, `TestToolRows_NarrowTerminalCellCount` (width 80: every row has `len(row) <= len(m.eventsTbl.Columns())`), `TestToolRows_DetailSearchSkipsToolRows` (start a detail search with tool rows visible; `detailMatchRows`, `detailSearchStale`, `detailNewRows` do not panic and never return a tool row index).
  - `TestToolRows_LinkedEventNoTools`: a response whose `SessionID` is another session with a matching `Seq` gets no tool rows (decision 3).

- [ ] **Step 2: Run, expect FAIL** (`go test ./tui -run TestToolRows -count=1`).

- [ ] **Step 3: Implement.**
  - `eventRow`: add `tool *toolRow`, `parent int`, `last bool`. `rowHolds` is unchanged (tool rows have nil `event` and `resp`).
  - `eventKey`/`findByKey`: a key with `tool != ""` matches the row whose `tool.ID == k.tool && tool.Agent == k.toolAgent`; other keys as today. `rowKey(er)` per decision 7. Replace the `keyOf(m.selectedEvent())`, `keyOf(m.selectedEventOn(prevRow))`, `keyOf(r.event)` pin writes listed in Files with `pinKeyOf` of the same row. Add `selectedRowOn(row int) eventRow` beside `selectedEventOn` (zero `eventRow{}` when `row` is out of range), and change `events_pane.go` ~446 to `pinKeyOf(m.selectedRowOn(prevRow))`. Change the keys.go and search_surfaces.go callers to `er, _ := m.selectedEventRow(); pinKeyOf(er)`. Leave the `keyOf(r.event)` calls in `search_details.go`: they key the detail-search hits by event, and tool rows are skipped there by `r.event == nil`. `revealPendingSeq` and `loadOlderFromRow` only ever land on event rows; leave them.
  - `renderEventRow(j)` in `rebuildEventsTable`: after the exchange row, look up `m.timeline.tools[resp.Seq]` (decision 3; only when `m.timeline.session == m.selectedSess`), and append one row per tool to `rows`, `m.visibleRows` (an `eventRow{tool: &t, parent: j, last: i == n-1}`) and, when searching, `full` (cells: tool name for METHOD, the full trailing text for the last cell, `NoMatch` false). Return the number of tool rows appended. In the main loop, when `sortCol != nil`, append one key per tool row after the exchange's key: `rowKey{val: <tool key>, exchange: j}` (the existing `rowKey` struct type; rule 11, decision 6). Member rows in blocks reach `renderEventRow` too, so rule 1 holds inside expanded blocks with no extra code.
  - `toolCells` per decision 5 and rules 2-10: `#` is `"┣⚒"`/`"┗⚒"`, or `"↳" + <parent # cell>` when `sorted`; DIR is `"│"` when the parent row is a subagent member and `m.subagentBlocks != nil`; apply each column's `align`. The parent `#` is the parent ctx's `#` column cell (`blockIDs` then `ids`).
  - Enter (keys.go ~744): after `selectedEventRow`, return nil when `er.tool != nil` (rule 13).
  - `handleTimelineLoaded`: set `m.timeline.tools = indexTools(msg.tl)` with `tl`; set it to nil wherever `tl` is set to nil (`openTimeline`, `followEventsTimeline`). On error keep the old index (rule 15: no extra error; a 404 before any load leaves it nil).

- [ ] **Step 4: Run, expect PASS.** Then `scripts/dev/go-test-shards.sh -short cmd/agentop` from the repo root: all pass.

- [ ] **Step 5: Commit** `feat: List tool calls in the events table with their duration`.

---

### Task 5: Help text

**Depends on:** none

**Files:**
- Modify: `cmd/agentop/tui/events_columns.go` (colDuration `desc`)
- Modify: `cmd/agentop/tui/help_overlay.go` (`paneEvents` bindings)
- Modify: `cmd/agentop/README.md` (Events section, after the sort paragraph near line 1110)
- Test: `cmd/agentop/tui/help_overlay_test.go`

**Interfaces:** none.

- [ ] **Step 1: Write failing test** `TestHelp_EventsExplainsToolTime`: the events help overlay text contains `"for a tool: from the reply to the next request of the same agent"`; the DURATION column's `desc` contains the same phrase.

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement.** DURATION `desc`: `"how long the exchange took; for a tool: from the reply to the next request of the same agent"`. Help overlay: add binding `{"⚒ rows", "tool calls; for a tool: from the reply to the next request of the same agent"}`. If the binding is wider than the overlay's fit tests allow, shorten the key text, not the phrase. README: one short paragraph: tool rows, `┣⚒`/`┗⚒`/`↳<n>`, and that the time is seen from outside: it includes the client's own delay and can miss a tool that started while the reply still streamed.

- [ ] **Step 4: Run, expect PASS**, including `go test ./tui -run 'TestHelp|Fit' -count=1`.

- [ ] **Step 5: Commit** `docs: Say how agentop measures a tool's duration`.

---

### Task 6: Benchmark

**Depends on:** Task 4

**Files:**
- Modify: `cmd/agentop/tui/events_rebuild_bench_test.go`

**Interfaces:** consumes `indexTools` and the Task 4 rendering.

- [ ] **Step 1:** In `rebuildBenchModel`, give each response `Seq: uint64(2*i+2)` (requests `2*i+1`) and build a `*timeline.Timeline` with one tool per response (95% of steps have one tool, fact table) and two parallel tools on every 20th response, each ended 1s after the response. Set `m.timeline.session = "s"`, `m.timeline.tl`, `m.timeline.tools = indexTools(tl)`. Add a `"sorted"` sub-benchmark with `m.sortCol = colDuration`.

- [ ] **Step 2: Run** `go test ./tui -run '^$' -bench BenchmarkRebuildEventsTable -benchmem -count=6` on this branch. For the base figures, run from the worktree root: `git worktree add --detach /tmp/tool-rows-1776-base refs/base/tool-rows-1776`, then `(cd /tmp/tool-rows-1776-base/cmd/agentop && go test ./tui -run '^$' -bench BenchmarkRebuildEventsTable -benchmem -count=6)`, then `git worktree remove --force /tmp/tool-rows-1776-base`. The base has no `"sorted"` sub-benchmark; compare only the sub-benchmarks that both have. Put both tables in the commit message body so the PR body can quote them (use `benchstat` if installed; otherwise the raw lines).

- [ ] **Step 3: Commit** `test: Add tool rows to the events-table rebuild benchmark`.

---

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: List tool calls in agentop's events table with their duration` | "" | 1, 2, 3, 4, 5, 6 | 1400 |

Estimate per task: T1 180, T2 320, T3 110, T4 700, T5 50, T6 40.

The PR body reports the Task 6 benchmark against `main` and lists the Review Focus items.

## Gate

From the worktree root. The gofmt and vet steps were run once on the unchanged base (ce712778) during plan review: both pass, with no change to how they are called.

```sh
(cd cmd/agentop && test -z "$(gofmt -l .)" && GOWORK=off go vet ./...) && scripts/dev/go-test-shards.sh -race cmd/agentop && (cd cmd/agentop && go test -race $(go list ./... | grep -v 'agentop$'))
```

## For a human after review

Needs: all tasks merged into the PR branch (P1).

1. Install the branch build of agentop only (`go build -o /tmp/agentop ./cmd/agentop`); do not run `make dev-install` (it restarts the shared proxy).
2. Run `/tmp/agentop` against the laptop service (`:47601`). Open 2 or 3 large live sessions. Check: tool rows under each exchange, `∥` on parallel tools, `running` that updates, Agent `run`, Bash `bg`, and sort by DURATION (`c`, then `s` on DURATION).
3. Watch the timeline fetch size on a large live session (up to ~470 KB per fetch, up to once a second). Decide if a smaller projection of the timeline endpoint is necessary. That decision is a new issue, not part of this PR.
