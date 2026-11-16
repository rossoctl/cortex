# Import Bob transcripts (#1872) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `agentop import --agent bob` imports Bob's task history from `~/.bob/db/bob.db`.

**Architecture:** A new importer source `core/session/importer/bob` reads `bob.db` through
the `sqlite3` CLI (a new shared helper `core/session/importer/sqlitecli`, which #1874
OpenCode can use too). It rebuilds each model turn as an OpenAI chat-completions request
and response body, and runs them through `inference.ParseRequest` / `inference.ReadResponse`,
as the Claude source does with Anthropic bodies. Bob's own per-turn cost goes on the
response event as a cost record before `importer.Run` prices it, and `price` then leaves
that event alone.

**Tech Stack:** Go 1.26.10, `os/exec`, `sqlite3 -readonly -json`.

**Spec:** issue #1872 (body), and the shared path of #1871 as merged
(`core/session/importer/types.go`, `importer.go`, `cost.go`).

## Global Constraints

- Do not add a SQLite driver to any module. Use the `sqlite3` program as a subprocess.
- A source only parses. It never touches the archive, the store or the ledger.
- Implement `importer.Source` exactly as merged in `core/session/importer/types.go`
  (`Agent()`, `PricingHost()`, `Sessions(since, skip)`). Do not change that interface.
- Do not price Bob through the pricing table.
- Agent name: `bob` (the canonical client name, `core/model/client.go`).
- Run Go commands with `GOWORK=off`. Log with `log/slog`.
- Commits: `git commit -s`, and end the message with
  `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. No `Co-Authored-By`.
- Write prose and comments in ASD-STE100 Simplified Technical English.

## Facts measured on a real `bob.db` (2026-11-16, 81 tasks, 604 messages)

- Tables: `tasks(id, project_id, parent_id, title, directory, version, env, costs,
  created_at, updated_at, task_type, ...)`, `messages(id, task_id, role, data, created_at)`.
  `created_at` and `updated_at` are integers in **milliseconds** since the epoch.
- `task_type` is `normal`, `subtask` or `subagent`. No tree is deeper than 2 levels here.
- `tasks.directory` is `''` on **every** task. `json_extract(env,'$.workspace')` holds an
  absolute path on 75 of 81 tasks. `tasks.version` is NULL on every task.
- `json_extract(env,'$.model.id')` is `premium-ide` (NULL on 6 tasks).
- **Do not read the whole `env` column.** Select only the two `json_extract` values.
- 93 groups of messages share one `(task_id, created_at)`. Order by `created_at, rowid`.
- `data` is a JSON object (text). Keys by role:
  - `system`: `content` (string).
  - `user`: `content` (string), `availableTools` (array of tool-name strings), `envContext`.
  - `assistant`: `content` (string), `toolCalls` (array of `{id, name, arguments}`, where
    `arguments` is a JSON **object**), `reasoning` (string), `thinkingBlocks` (array of
    `{type, thinking, signature}`), `stop` (bool), `_meta.stopReason` (string, for example
    `user_cancelled`), `_meta.spend` (`{cost, contextTokens}`; `cost` can be `0`).
  - `tool`: `content` (string), `toolUsage`, `_meta.subagentId` on a spawn result.
- A `tool` message carries **no tool-call id**. Bob writes one `tool` message for each
  call, in call order, after the assistant message. So the k-th `tool` message after an
  assistant message answers that message's `toolCalls[k]`.
- 5 assistant messages have no `_meta.spend`.
- 25 tasks have no messages.
- Bob Shell check (for the PR body): **yes, Bob Shell sessions land in `bob.db`.** Tasks
  from a bob-shell 2.1.0 run (4430cc6e, child 9e048d7d) and from a Bob IDE 2.2.3 run
  (root f40f1df7, subtask e988d23c) are both in `bob.db`. The IDE's
  `globalStorage/ibm.bob-code/tasks` holds 1 task from 2026-01-17, not in `bob.db`; the
  `~/.bob/tmp/*/chats/*.json` files stop at 2026-03-11. Both old stores stay out of scope.

## Decisions (made here, with the reason)

1. **Title comes from the events, not from `tasks.title`.** `importer.Session` as merged
   has no `Title` field, and the archive folds the title from the user messages by the
   same rule as a live Bob session. So an imported task shows the same title that live
   capture gives it. Put this in the PR body.
2. **Project:** `tasks.directory`, else `json_extract(env,'$.workspace')`, else the root
   task's value. Reason: `directory` is empty on every real task. The label is
   `project.Label(project.Root(dir, home), home)`, as `claude/turns.go` makes it.
3. **Wire path** for the parser: `/inference/v1/chat/completions` (Bob's live path, the
   OpenAI dialect). `PricingHost()` gives `api.us-east.bob.ibm.com`.
4. **Cost:** when `_meta.spend` exists, the response event gets
   `event.Event{CostUSD: spend.cost, Source: event.SourceTranscript, Provenance:
   pricing.ProvAuthoritative.String(), Settled: true}`, also when `cost` is 0 (Bob says
   the turn was free). `TotalTokens = contextTokens`. With no `_meta.spend`, the response
   has no token counts, so `price` gives it no record.
5. **`price` change:** `price` returns at once when the event already has a cost record
   (`event.Record(e)` gives ok). Reason: this is the smallest change that lets a source
   supply its own figure. Claude events never carry a record before `price`, so they do
   not change.
6. **Request id:** `messages.id` of the assistant row. It is unique.
7. **Times:** response `At` = assistant `created_at`. Request `At` = `created_at` of the
   newest non-assistant message since the previous assistant message, else the assistant's
   own; never before the previous response's `At`.
8. **Child tasks:** follow `parent_id` to the top (stop at a missing parent or a cycle).
   Each child event gets `Subagent` = the child's own id, `SubagentKind` = its
   `task_type`, and `Inference.AgentRole = model.AgentRoleSubagent`. Root events get
   `AgentRoleMain`. `ParentSubagent` stays empty, as on the live wire (every child names
   the root).
9. **Thinking:** `ResponseThinking` counts each `thinkingBlocks` entry with
   `InferenceThinking.Add(type, len(thinking), len(signature), 0)`. With no blocks and a
   non-empty `reasoning`, it counts one `thinking` block of `len(reasoning)` bytes. No
   thinking text is kept, as for live OpenAI traffic.
10. **FinishReason** = `_meta.stopReason` as given.
11. **Tests build the fixture DB at test time** from `testdata/bob.sql` with
    `sqlite3 <tmp>/bob.db < bob.sql`, and `t.Skip` when `sqlite3` is not on PATH. Reason:
    no binary file in git. `ubuntu-latest` and `macos-latest` have `sqlite3`.
12. **One subprocess for the task list, and one for the messages of each task.** Reason:
    the `IN (...)` list needs no quoting logic beyond `Quote`, and the cost is small
    (about 80 tasks).

## Review Focus

1. `sqlite3 -json` prints **nothing** (not `[]`) for zero rows. `Query` must give no rows
   and no error. Test: `TestQuery_NoRowsIsEmpty` (T1).
2. A missing `bob.db` file gives no sessions and no error (Bob is not installed). Test:
   `TestSource_NoDatabaseYieldsNothing` (T2).
3. A tool message with no open tool call (more tool rows than calls) is kept as a tool
   message with an empty `tool_call_id`, and is not a parse failure. Test: the
   `tool-extra` row in the fixture, asserted in `TestSource_ToolCallsAndResults` (T2).
4. A task id with a `'` in it must not break the SQL. Test: `TestQuote` (T1).
5. A child whose `parent_id` names a missing task files as its own root. Test:
   `TestSource_OrphanChildIsItsOwnRoot` (T2).

---

### Task T1: Shared pieces — the `sqlite3` helper and the `price` skip

Depends on: none

**Files:**
- Create: `core/session/importer/sqlitecli/sqlitecli.go`
- Create: `core/session/importer/sqlitecli/sqlitecli_test.go`
- Modify: `core/session/importer/cost.go`
- Modify: `core/session/importer/cost_test.go`

**Interfaces:**
- Produces:
  - `package sqlitecli`
  - `const Program = "sqlite3"`
  - `func Query(prog, db, query string, out any) error` — runs
    `prog -readonly -json db query`, decodes stdout (a JSON array) into `out`. Empty stdout
    leaves `out` unchanged and gives nil. When `exec.LookPath(prog)` fails, the error text
    is `<prog> is not installed or not on PATH: <err>`. When the program exits non-zero,
    the error includes its trimmed stderr.
  - `func Quote(s string) string` — gives `'` + s with each `'` doubled + `'`.
  - In `package importer`: `price` does nothing when `event.Record(e)` already gives ok.

- [ ] **Step 1: Write the failing tests** in `sqlitecli_test.go`. Each test that runs
  `sqlite3` calls `t.Skip` when `exec.LookPath(Program)` fails. Build a DB in
  `t.TempDir()` by running `sqlite3 <db> "create table t(a text, b integer); insert into t values('x''y', 2);"`.
  - `TestQuery_DecodesRows`: `Query(Program, db, "select a, b from t", &rows)` with
    `rows []struct{A string `json:"a"`; B int `json:"b"`}` gives one row `{x'y, 2}`.
  - `TestQuery_NoRowsIsEmpty`: `select a from t where b = 99` gives nil error and `len(rows) == 0`.
  - `TestQuery_MissingProgramNamesIt`: `Query("sqlite3-not-here-1872", db, "select 1", &rows)`
    gives an error whose text contains `sqlite3-not-here-1872` and `not installed`.
  - `TestQuery_BadSQLGivesStderr`: `select * from nope` gives an error containing `nope`.
  - `TestQuote`: `Quote("a'b") == "'a''b'"`, and
    `Query(Program, db, "select a from t where a = "+Quote("x'y"), &rows)` gives one row.
- [ ] **Step 2:** Add to `cost_test.go`: `TestPrice_KeepsASourcesRecord` — an event from
  `fakeSession("s1", start, 1).Events[1]` that already has
  `e.Plugins[event.Key]` = marshalled `event.Event{CostUSD: 0.5, Source: event.SourceTranscript, Provenance: "authoritative", Settled: true}`;
  after `price(&e, "api.anthropic.com", testRates(t))`, `event.Record(&e)` gives
  `CostUSD == 0.5` and `Provenance == "authoritative"`.
- [ ] **Step 3:** Run `GOWORK=off go -C core test ./session/importer/...` — the new tests fail.
- [ ] **Step 4:** Implement `sqlitecli.go` (use `exec.Command(...).Output()`; read stderr
  from `*exec.ExitError`). Add the early return to `price` in `cost.go`, with a one-line
  comment: a source that has the agent's own figure (Bob) sets the record first.
- [ ] **Step 5:** Run `GOWORK=off go -C core test -race ./session/importer/...` — PASS.
- [ ] **Step 6:** Commit: `feat: Add a sqlite3 helper for importer sources and keep a source's cost record`.

### Task T2: The Bob source, its fixture tests, and its registration

Depends on: T1

**Files:**
- Create: `core/session/importer/bob/bob.go` (the source, the task tree, `Sessions`)
- Create: `core/session/importer/bob/turns.go` (messages of one task → events)
- Create: `core/session/importer/bob/testdata/bob.sql`
- Create: `core/session/importer/bob/bob_test.go`
- Create: `core/session/importer/bob/timeline_test.go`
- Modify: `cmd/cortex/main.go` (import `bobimport "github.com/rossoctl/cortex/core/session/importer/bob"`;
  `Sources: []importer.Source{claudeimport.New(""), bobimport.New("")}`)

**Interfaces:**
- Consumes: `sqlitecli.Query`, `sqlitecli.Quote`, `sqlitecli.Program` (T1); the `price` skip (T1).
- Produces: `func New(dbPath string) importer.Source`. `dbPath ""` resolves
  `~/.bob/db/bob.db` when `Sessions` runs. The source struct has an unexported
  `prog string` (default `sqlitecli.Program`) that tests in the package set.
  `Agent()` = `"bob"`, `PricingHost()` = `"api.us-east.bob.ibm.com"`.

**Queries** (the exact column set; nothing else is read):

```sql
select t.id, t.parent_id, t.task_type, t.version,
  coalesce(nullif(t.directory,''), json_extract(t.env,'$.workspace'), '') as dir,
  json_extract(t.env,'$.model.id') as model,
  (select max(m.created_at) from messages m where m.task_id = t.id) as last
from tasks t
```

```sql
select id, role, data, created_at from messages where task_id = <Quote(id)> order by created_at, rowid
```

**`Sessions(since, skip)` rule:**
1. `os.Stat(db)`: not exist → yield nothing. Other stat error → `yield(Session{}, err)`.
2. Task query error (this includes a missing `sqlite3`) → `yield(Session{}, err)` and return.
3. Group tasks by root (Decision 8). Decode `last` and `version` as pointers
   (`*int64`, `*string`): `sqlite3 -json` writes SQL NULL as JSON `null`. Tree time = the max `last` of its tasks. Drop a tree
   whose tree time is NULL (no messages). Sort trees by tree time newest first, then id.
4. When `since` is not zero and the tree time is before `since`, skip the tree.
5. Call `skip(rootID, treeTime)`; when true, do not read the tree.
6. Read the messages of each task. A failed messages query of the **root** → yield
   `Session{ID: root, Unreadable: true}` (with `slog.Warn`). A failed query of a child →
   `Skipped++` and go on.
7. Events of all tasks are sorted stable by `At`; `Updated` = the max event `At`;
   `SizeHint` = the sum of `len(data)` of the rows read; `ID` = the root id.

**Turn rule (`turns.go`)**, per task, over its rows in order. Keep `conv` (the OpenAI
`messages` array so far), `tools` (latest `availableTools`), `pending` (tool-call ids
of the last assistant message) and the next pending index:
- A row whose `data` is not a JSON object, or whose role is not one of the four → `Skipped++`.
- `system`, `user` → append `{role, content}`. `user` also replaces `tools`.
- `tool` → append `{role: "tool", content, tool_call_id: pending[k]}` (empty when no
  call is left), k++.
- `assistant` → build the request body `{model, messages: conv, tools: [{type:"function",function:{name}}...]}`
  and run `inference.ParseRequest(path, nil, body)`; nil → `Skipped++` and go on. Build
  the response body `{model, choices:[{message:{role:"assistant",content,tool_calls:[{id,type:"function",function:{name,arguments: <the arguments object as a JSON string>}}]},finish_reason}], usage:{total_tokens: contextTokens}}`
  (no `usage` when there is no `_meta.spend`); copy the request extension and run
  `inference.ReadResponse(path, false, body, &resp)`. Set `AgentRole`, thinking
  (Decision 9), and the cost record (Decision 4; write `e.Plugins[event.Key]` and
  `e.Plugins[event.PluginName]`, as `importer.price` does). Both events: `Direction:
  model.Outbound`, `RequestID` (Decision 6), times (Decision 7),
  `Client: &model.EventClient{Name: "bob", Version: <tasks.version>}`, `Project`
  (Decision 2), `Subagent`/`SubagentKind` (Decision 8). Intern both with one
  `session.Interner` for the session (`InternEvent`), as `claude/turns.go` does. Then
  append the assistant message (with its `tool_calls`) to `conv` and reset `pending`.

**Fixture `testdata/bob.sql`** (scrubbed; plain SQL; `create table` statements copied
from the column lists above, with only the columns the queries read plus `title`):
- Task `t-plain` (`normal`, `directory` `''`, `env` `{"workspace":"/tmp/demo","model":{"id":"premium-ide"}}`,
  version `2.1.0`): system, user `hello bob`, assistant `hi` with
  `_meta.spend {cost: 0.000018, contextTokens: 9}`.
- Task `t-tools` (`normal`, `directory` `/tmp/tools`): user (with `availableTools`
  `["read_file","list_files"]`), assistant with two `toolCalls` (`c1` `read_file`, `c2`
  `list_files`, `arguments` objects) and `thinkingBlocks` of one `thinking` block, two
  `tool` rows, a third `tool` row `tool-extra`, then a final assistant with spend
  `{cost: 0, contextTokens: 120}`; plus one message row whose `data` is `not json`.
- Task `t-child` (`subagent`, `parent_id` `t-tools`, no directory, no workspace): user,
  assistant with spend `{cost: 0.0001, contextTokens: 50}`.
- Task `t-orphan` (`subtask`, `parent_id` `t-gone`, which does not exist): user, assistant (no spend).
- Task `t-empty` (`normal`): no messages.
- Times: give each tree a distinct newest `created_at`, `t-tools` newest.

- [ ] **Step 1: Write the failing tests** in `bob_test.go`. A helper `fixtureDB(t)` skips
  without `sqlite3` and builds `<tmp>/bob.db` from `testdata/bob.sql`. A helper
  `readAll(t, src, since)` collects `Sessions(since, func(string, time.Time) bool { return false })`.
  - `TestSource_AgentAndHost`: `"bob"`, `"api.us-east.bob.ibm.com"`.
  - `TestSource_NoDatabaseYieldsNothing`: `New(<tmp>/missing.db)` yields 0 sessions, no error.
  - `TestSource_MissingSqlite3FailsWithItsName`: build the fixture DB with `fixtureDB(t)`
    (so `os.Stat` passes), then make the source in the package with that path and
    `prog: "sqlite3-not-here-1872"`. It yields exactly one error whose text contains
    `sqlite3-not-here-1872`, and no session.
  - `TestSource_PlainTask`: session `t-plain` has 2 events (request, response, one
    `RequestID`); request `Inference.Model == "premium-ide"`, messages system + user;
    response `Completion == "hi"`, `TotalTokens == 9`; `Client.Name == "bob"`,
    `Client.Version == "2.1.0"`; `Project` is the label of `/tmp/demo`; `event.Record`
    gives `CostUSD == 0.000018`, `Source == event.SourceTranscript`,
    `Provenance == "authoritative"`; the response has `Inference.AgentRole == model.AgentRoleMain`.
  - `TestSource_ToolCallsAndResults`: in `t-tools`, the first response has `ToolCalls`
    ids `c1`,`c2` with names and `Arguments` that is the JSON of the object;
    `ResponseThinking.Blocks == 1`; the second request has `ToolResults` with ids `c1`,`c2`;
    its `Tools` names are `read_file`,`list_files`; the `tool-extra` row is in its
    `Messages` as role `tool`; the second response's record has `CostUSD == 0` and
    `Settled == true`; `Skipped == 1` (the `not json` row).
  - `TestSource_ChildTaskInRootSession`: no session has ID `t-child`; `t-tools` holds the
    child's events with `Subagent == "t-child"`, `SubagentKind == "subagent"`,
    `Inference.AgentRole == model.AgentRoleSubagent`; the child's `Project` is the root's.
  - `TestSource_OrphanChildIsItsOwnRoot`: a session `t-orphan` exists; its response has no
    cost record and `TotalTokens == 0`.
  - `TestSource_NewestFirstSinceAndSkip`: ids in order start with `t-tools`; `t-empty` is
    not yielded; a `since` after `t-plain`'s newest message drops `t-plain`; a `skip`
    that gives true for `t-tools` stops its read and it is not yielded. Read once with a
    `skip` that gives false to get `t-tools`'s `Updated`; then assert that the time `skip`
    got for `t-tools` is not before that `Updated`.
  - `TestSource_UnreadableRoot`: build the fixture, then run
    `sqlite3 <db> "alter table messages rename column role to role_gone"`. Every tree
    yields `Unreadable == true`, and the source gives no error. Do not drop the
    `messages` table: the task query reads `messages` in its `last` subquery, so a
    dropped table makes the task query fail, not the messages query. The rename breaks
    only the messages query (it reads `role`).
- [ ] **Step 2:** In `timeline_test.go`, `TestTimeline_ImportedTaskShowsToolsAndChild`
  (copy the shape of `claude/timeline_test.go`): set `Seq = uint64(i + 1)` and
  `Source = model.ImportedSource("bob")` on `t-tools`'s events, run `timeline.Build`;
  assert tools `read_file` and `list_files` are in `tl.Tools`, and `tl.Agents` has a row
  with `ID == "t-child"`.
- [ ] **Step 3:** Run `GOWORK=off go -C core test ./session/importer/bob/` — FAIL (no package).
- [ ] **Step 4:** Implement `bob.go` and `turns.go`. Register the source in `cmd/cortex/main.go`.
- [ ] **Step 5:** Run `GOWORK=off go -C core test -race ./session/importer/...` and
  `GOWORK=off go -C cmd/cortex build ./...` — PASS.
- [ ] **Step 6:** Commit: `feat: Import Bob tasks from bob.db`.

### Task T3: Docs

Depends on: none

**Files:**
- Modify: `docs/session-api.md` (the `**Sources.**` paragraph near line 459, and the
  cost bullet "Cortex prices it at today's rates" near line 443)
- Modify: `cmd/agentop/README.md` (the `--agent` row near line 794, and the example block near line 788)
- Modify: `docs/laptop-service.md` ("Importing history from before Cortex", near line 590)

Facts to write (all from this plan):
- Supported sources now: `claude-code` and `bob`. #1873 to #1875 add the others
  (codex, opencode, pi). The proxy answers the others with an error that names the
  supported agents.
- The Bob source reads `~/.bob/db/bob.db` through `sqlite3 -readonly -json` as a
  subprocess. When `sqlite3` is not on PATH, Bob fails with an error that names the
  program, and the other agents continue. A missing `bob.db` imports nothing.
- One Bob task is one session, with the task id (`X-Task-Id`) as its session id. A child
  task (`subtask` or `subagent`) is in its root task's session, with `subagent` set to
  the child's id.
- Each Bob event has the `bob` client, the model alias that Bob records (for example
  `premium-ide`), and the project of `tasks.directory`, else of the task's workspace.
- A Bob turn's cost is Bob's own figure (`_meta.spend.cost`), with `source:
  "transcript"` and `provenance: "authoritative"`. Cortex does not price Bob turns from
  its rate table. `totalTokens` is Bob's `contextTokens`; Bob records no input/output split.
- Bob Shell and Bob IDE both write `bob.db`. The older `~/.bob/tmp/*/chats/*.json` files
  and the IDE's `globalStorage/ibm.bob-code/tasks` are not read.
- laptop-service.md: say that `agentop import` adds Claude Code sessions and Bob tasks,
  and that a Bob turn keeps Bob's own cost instead of today's rates.

- [ ] **Step 1:** Edit the three files. Keep the existing style; STE prose.
- [ ] **Step 2:** Commit: `docs: Describe the Bob import source`.

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: Import Bob transcripts with agentop import` | (none) | T1, T2, T3 | 900 |

Estimates: T1 about 120, T2 about 750 (source about 330, fixture about 60, tests about
360), T3 about 30. The total is below 2000, so one PR.

## Gate

From the worktree root:

```sh
GOWORK=off go -C core vet ./... && GOWORK=off go -C core test -race ./session/importer/... ./sessionapi/... && GOWORK=off go -C cmd/cortex vet ./... && GOWORK=off go -C cmd/cortex build ./... && test -z "$(gofmt -l core/session/importer cmd/cortex)"
```

## Gate check (run on the base, 2026-11-16)

From the worktree root, each vet and format step of the gate ran once on the unchanged
base and passed: `GOWORK=off go -C core vet ./...`, `GOWORK=off go -C cmd/cortex vet ./...`,
`test -z "$(gofmt -l core/session/importer cmd/cortex)"`. No change to how they are called.

## For a human after review

- Run `agentop import --agent bob --dry-run`, then `agentop import --agent bob`, against
  the real local Cortex and the real `~/.bob/db/bob.db`. Open one imported task in the
  Timeline and check its tool calls and child tasks, and check that its cost equals
  `tasks.costs.cost`. Needs T1 and T2 merged and a Cortex built from them (this restarts
  the shared proxy: coordinate first).
- Put the Bob Shell check (section "Facts measured", last bullet) and Decision 1 (title
  from events) in the PR body.
