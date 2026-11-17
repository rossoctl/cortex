# Import OpenCode transcripts (#1874) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `agentop import --agent opencode` imports OpenCode history from `opencode.db`.

**Architecture:** A new importer source `core/session/importer/opencode` reads
`opencode.db` through the existing helper `core/session/importer/sqlitecli` (added by
#1872). It rebuilds each OpenCode assistant row as an Anthropic Messages request and
response body, and runs them through `inference.ParseRequest` / `inference.ReadResponse`
at `/v1/messages`, as the Claude source does. `importer.Run` then prices each response
through the pricing table. The source copies the shape of `core/session/importer/bob`.

**Tech Stack:** Go 1.26.10, `os/exec` through `sqlitecli`, `sqlite3 -readonly -json`.

**Spec:** issue #1874 (body), and the shared path as merged
(`core/session/importer/types.go`, `importer.go`, `cost.go`). The Bob source
(`core/session/importer/bob/`) is the model to copy.

## Global Constraints

- Do not add a SQLite driver to any module. Use `sqlitecli.Query` and `sqlitecli.Quote`.
- A source only parses. It never touches the archive, the store or the ledger.
- Implement `importer.Source` exactly as merged in `core/session/importer/types.go`:
  `Agent()`, `PricingHost()`,
  `Sessions(since time.Time, skip func(id string, updated time.Time) bool, progress func(done, total int))`.
  Do not change that interface. Do not change `sqlitecli` or `cost.go`.
- Agent name: `opencode` (the canonical client name in `core/model/client.go`).
- Price through the pricing table (the issue's Cost section). The source sets no cost record.
- Run Go commands with `GOWORK=off`. Log with `log/slog`.
- Commits: `git commit -s`, and end the message with
  `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. No `Co-Authored-By`.
- Write prose and comments in ASD-STE100 Simplified Technical English.

## Facts measured on a real `opencode.db` (2026-11-16, OpenCode v2.0.26, 31 sessions, 426 messages)

- Path: `~/.local/share/opencode/opencode.db`, with `-wal` and `-shm` files beside it.
  `sqlite3 -readonly -json` reads it while the WAL holds data.
- `session_v2` columns that the import reads: `id` (`ses_…`), `parent_id`,
  `fork_session_id`, `directory` (absolute path, never empty), `version` (for example
  `2.0.26`), `time_created`. All times in OpenCode are integers in **milliseconds**.
  30 sessions have no parent. 1 session has a `parent_id` (a subagent). 0 sessions have a
  `fork_session_id`. No session has zero messages.
- `session_message(id, session_id, type, seq, time_created, time_updated, data)`.
  `(session_id, seq)` is unique. Order by `seq`. `data` is a JSON object (text).
- Message `type` values and counts: `assistant` 275, `user` 71, `idle` 71,
  `model-switched` 6, `system` 3.
- `user` data: `text` (string), `files`, `agents`, `time.created`.
- `assistant` data: `model {id, providerID, variant}`, `agent`, `content` (array of
  parts), `finish` (`tool-calls`, `stop`, `error`, or absent), `rawFinish`
  (`tool_calls`, `stop`, `end_turn`, `tool_use`), `tokens {input, output, reasoning,
  cache {read, write}}` (absent on 16 rows), `cost`, `error {type, message, status}`
  (15 rows), `time {created, streamed, completed}`.
- Content part `type` values: `reasoning` (`text`), `text` (`text`), `tool`
  (`id`, `name`, `state.status` = `completed` or `error`, `state.input` (object),
  `state.content` (array of `{type: "text", text}`) as the tool result,
  `state.error.message` on an error).
- **A tool result is inside the same assistant row as its call.** OpenCode does not
  write a separate tool-result row. The next assistant row is the next model turn.
- The subagent tool is named `subagent` (v2.0.26). Its result text starts
  `<subagent sessionID="ses_…" state="completed">`. The child's own turns are in the
  child session's rows.
- Token semantics (measured against `cost` on 4 priced turns, exact to the last digit):
  `tokens.input` excludes cache reads and writes (the Anthropic meaning), and OpenCode
  bills `output + reasoning` at the output rate. Example: gpt-5 on Zen, input 4384,
  output 8, reasoning 14 → `cost` 0.00487788 = 4384 × $1.07/M + 22 × $8.50/M.
- `session_message.time_updated >= data.time.completed` on 426 of 426 rows.
  `data.time.created == time_created` on 426 of 426 rows.
- An error row (`finish: "error"`) has `content: []` and no `tokens`.
- `system` rows hold an OpenCode notice (`"Today's date is now: …"`), not the system prompt.
  OpenCode does not store the system prompt or the tool list.
- **Affinity id** (from the OpenCode v2.0.26 binary): the header code is
  `"X-Session-Id": CP(session)` with `CP = (e) => e.parentID ?? e.fork?.sessionID ?? e.id`.
  It is **one step**: a subagent of a subagent sends its own parent's id, not the root's.
  The columns are `session_v2.parent_id` (parent) and `session_v2.fork_session_id`
  (fork source). Put both column names in the PR body, as the issue asks.
- Fork rows (inferred from OpenCode's own SQL, not measured: no fork exists in the real
  DB): OpenCode's usage queries filter `session.fork_session_id IS NULL OR
  message.time_created >= session.time_created`. So a fork holds copies of its source's
  earlier rows, with times before the fork's own `time_created`.

## Decisions (made here, with the reason)

1. **Session id** = `parent_id`, else `fork_session_id`, else `id` (each one only when it
   is not NULL and not `''`). One step, not a walk to the top. Reason: this is the value
   OpenCode sends as `X-Session-Id` (Facts), so an import files a turn where live
   capture files it.
2. **Group:** all `session_v2` rows with the same session id form one importer session.
   Read the session whose own `id` equals the session id first (when it exists), then the
   others by `time_created`, then `id`. When no row has that `id` (the parent was
   deleted), the group still has that session id.
3. **Subagent:** a row with a `parent_id` gives events with `Subagent` = its own `id` and
   `Inference.AgentRole = model.AgentRoleSubagent`. `SubagentKind` stays `""`: its doc
   comment in `core/model/session.go` says only Bob sets it. All other events get
   `model.AgentRoleMain` and no `Subagent`.
4. **Fork:** a row with a `fork_session_id` and no `parent_id` gives main-agent events
   (no `Subagent`), as live capture gives them. Its rows with `time_created` before the
   fork session's `time_created` are copies: they go into the conversation (`conv`) but
   give no events. Reason: the source session already has those turns.
5. **Wire dialect:** Anthropic Messages at `/v1/messages`. Reason: `tokens` has the
   Anthropic meaning (Facts), and `importer.price` settles at `/v1/messages`.
6. **Tokens:** response body `usage` = `{input_tokens: input, output_tokens: output +
   reasoning, cache_read_input_tokens: cache.read, cache_creation_input_tokens:
   cache.write}`. No `usage` when `tokens` is absent, so `price` gives no record. Reason:
   OpenCode bills reasoning at the output rate (Facts).
7. **Pricing host:** `PricingHost()` = `opencode.ai` (OpenCode Zen). Reason: most turns go
   to Zen (`providerID` `opencode`, 273 of 275), and Zen's `*-free` models then price as
   zero through `bundledFreeRates`. Other providers price by model id through the `*`
   rows. A non-Zen model whose name ends in `-free` also prices as zero: accept this,
   and put it in the PR body.
8. **Model** = `data.model.id` of the assistant row, for request and response.
9. **Request id** = `session_message.id` of the assistant row.
10. **Times:** request `At` = the assistant row's `time_created`. Response `At` =
    `data.time.completed` when it is a positive integer, else `time_created`; then at most
    the row's `time_updated`. Reason: the skip contract needs every event `At` at or
    before the tree time, and the tree time is the max `time_updated` (Facts).
11. **Conversation:** a `user` row appends `{role: "user", content: [{type: "text",
    text}]}`. An assistant row that gives a turn appends `{role: "assistant", content:
    <its text and tool_use blocks>}` (no thinking blocks), then, when it has tool results,
    one `{role: "user", content: [tool_result…]}`. A tool part with `state.status`
    `completed` gives `{type: "tool_result", tool_use_id: id, content: <the texts of
    state.content joined by "\n">}`. `error` gives the same with `is_error: true` and
    `content` = `state.error.message`. Another status gives the `tool_use` and no result.
12. **Response body content:** `reasoning` part → `{type: "thinking", thinking: text,
    signature: ""}`. `text` part → `{type: "text", text}`. `tool` part → `{type:
    "tool_use", id, name, input: state.input}` (`{}` when absent). `stop_reason` =
    `rawFinish`, else `finish`. A part of another type is left out and is not counted.
13. **Rows that give no turn and are not counted:** `idle`, `model-switched`, `system`,
    and an assistant row with an `error` and an empty `content`. Reason: OpenCode wrote
    them on purpose. They are recognized, and none is a model completion.
14. **Rows counted in `Skipped`:** `data` that is not a JSON object, a `type` not named in
    this plan, and an assistant row for which `inference.ParseRequest` gives nil.
15. **Title** comes from the events. `importer.Session` has no `Title` field (same as
    Bob's Decision 1). Put this in the PR body.
16. **Project** = `project.Label(project.Root(directory, home), home)` of each row's own
    `directory`, cached per directory (copy `bob`'s `parser.project`).
17. **Client** = `&model.EventClient{Name: "opencode", Version: session_v2.version}`.
18. **Database path:** `$XDG_DATA_HOME/opencode/opencode.db` when `XDG_DATA_HOME` is
    set and not empty, else `~/.local/share/opencode/opencode.db`. Resolve it when
    `Sessions` runs.
19. **Tests build the fixture DB at test time** from `testdata/opencode.sql` with
    `sqlite3 <tmp>/opencode.db < opencode.sql`, and `t.Skip` when `sqlite3` is not on
    PATH. Reason: no binary file in git (same as Bob).
20. **One subprocess for the session list, and one for the messages of each session row**
    (same as Bob).

## Review Focus

1. A missing `opencode.db` gives no sessions and no error (OpenCode is not installed).
   Test: `TestSource_NoDatabaseYieldsNothing` (T1).
2. `XDG_DATA_HOME` moves the database. Test: `TestSource_HonoursXDGDataHome` (T1).
3. A fork's copied rows must not give a second copy of the source's turns. Test:
   `TestSource_ForkFilesUnderSourceWithoutCopies` (T1).
4. A session whose parent row does not exist still imports, under the parent id. Test:
   `TestSource_MissingParentKeepsAffinityID` (T1).
5. A tool call with `state.status: "error"` gives a `tool_result` with `is_error`, and
   the next request carries it. Test: `TestSource_ToolCallsAndResults` (T1).

---

### Task T1: The OpenCode source, its fixture tests, and its registration

Depends on: none

**Files:**
- Create: `core/session/importer/opencode/opencode.go` (the source, the groups, `Sessions`)
- Create: `core/session/importer/opencode/turns.go` (rows of one session → events)
- Create: `core/session/importer/opencode/testdata/opencode.sql`
- Create: `core/session/importer/opencode/opencode_test.go`
- Create: `core/session/importer/opencode/timeline_test.go`
- Modify: `cmd/cortex/main.go` (import
  `opencodeimport "github.com/rossoctl/cortex/core/session/importer/opencode"`, and add
  `opencodeimport.New("")` to the `Sources` list near line 1283, after `bobimport.New("")`)

**Interfaces:**
- Consumes: `sqlitecli.Query(prog, db, query string, out any) error`,
  `sqlitecli.Quote(s string) string`, `sqlitecli.Program` (all existing).
- Produces: `func New(dbPath string) importer.Source`. `dbPath ""` resolves the path of
  Decision 18 when `Sessions` runs. The source struct has an unexported `prog string`
  (default `sqlitecli.Program`) that tests in the package set.
  `Agent()` = `"opencode"`, `PricingHost()` = `"opencode.ai"`.

**Queries** (the exact column set; nothing else is read):

```sql
select s.id, s.parent_id, s.fork_session_id, s.directory, s.version, s.time_created,
  (select max(time_updated) from session_message where session_id = s.id) as last,
  (select count(*) from session_message where session_id = s.id) as messages
from session_v2 s
```

```sql
select id, type, time_created, time_updated, data from session_message
where session_id = <Quote(id)> order by seq
```

Decode `parent_id`, `fork_session_id` and `last` as pointers (`*string`, `*int64`):
`sqlite3 -json` writes SQL NULL as JSON `null`.

**`Sessions(since, skip, progress)` rule** (copy `bob.Sessions`):
1. Resolve the path (Decision 18). `os.Stat`: not exist → yield nothing. Other error →
   `yield(Session{}, err)`.
2. Session query error (this includes a missing `sqlite3`) → `yield(Session{}, err)` and return.
3. Group the rows (Decisions 1 and 2). Group time = the max `last` of its rows. Drop a
   group with no messages. Sort groups by group time newest first, then id.
4. For each group: `progress(i, len(groups))`. When `since` is not zero and the group
   time is before `since`, skip it. Call `skip(id, time.UnixMilli(groupTime))`; when true,
   do not read it. After the loop, `progress(len, len)`.
5. Read the rows of each session row. A failed messages query of the row whose `id`
   equals the group id → yield `Session{ID: id, Unreadable: true}` (with `slog.Warn`).
   Do not read the other rows of that group. A failed query of another row →
   `Skipped++` and go on. When the group has no row whose `id` equals the group id (the
   parent was deleted) and the query of every row of the group fails → yield
   `Session{ID: id, Unreadable: true}` too.
6. Sort the events of the group stable by `At`. `Updated` = the max event `At`.
   `SizeHint` = the sum of `len(data)` of the rows read. `ID` = the group id.
   Intern every event with one `session.Interner` for the group (`InternEvent`), as
   `bob/turns.go` does.

**Turn rule (`turns.go`):** Decisions 3 to 14 and 16 to 17. Both events of a turn:
`Direction: model.Outbound`, the same `RequestID`, `Client`, `Project`, `Subagent`, and
`AgentRole` on both `Inference` extensions. Request body: `{model, messages: conv}`.
Response body: `{model, content, stop_reason, usage}`. The response extension is a copy
of the request extension with `inference.ReadResponse("/v1/messages", false, body, &resp)`
read into it, as in `claude/turns.go`.

**Fixture `testdata/opencode.sql`** (scrubbed; plain SQL). `create table session_v2`
with only `id, parent_id, fork_session_id, directory, version, time_created`, and
`create table session_message` with the 7 columns of the Facts. Rows:
- `ses_plain` (directory `/tmp/demo`, version `2.0.26`): user `hello opencode`; assistant
  (model `fledge-alpha-free`, providerID `opencode`) with a `reasoning` part and a `text`
  part `hi`, `rawFinish` `stop`, tokens `{input: 193, output: 154, reasoning: 0, cache:
  {read: 7040, write: 0}}`; an `idle` row.
- `ses_tools` (directory `/tmp/tools`), newest group: user; assistant A with two `tool`
  parts: `c1` `read` (`completed`, `state.content` `[{type:"text",text:"file body"}]`)
  and `c2` `shell` (`error`, `state.error.message` `"exit 1"`), and `subagent` call `c3`
  whose result text names `ses_child`, tokens with `reasoning: 14`; assistant B with a
  `text` part and tokens; a `model-switched` row; one row whose `data` is `not json`;
  one row of type `bogus`; an error assistant row (`finish: "error"`, `content: []`, no tokens).
- `ses_child` (`parent_id` `ses_tools`): user; assistant with tokens.
- `ses_fork` (`fork_session_id` `ses_plain`, `time_created` after `ses_plain`'s rows):
  copies of `ses_plain`'s user and assistant rows (new ids, original times), then its own
  user `forked` and assistant `fork reply` with tokens.
- `ses_orphan` (`parent_id` `ses_gone`, which does not exist): user; assistant with no tokens.
- `ses_empty`: no messages.
- Give each group a distinct newest `time_updated`.

- [ ] **Step 1: Write the failing tests** in `opencode_test.go`. Copy `fixtureDB`,
  `readAll` and `sessionByID` from `bob/bob_test.go` (file `opencode.sql`, DB `opencode.db`).
  - `TestSource_AgentAndHost`: `"opencode"`, `"opencode.ai"`.
  - `TestSource_NoDatabaseYieldsNothing`: `New(<tmp>/missing.db)` yields 0 sessions, no error.
  - `TestSource_HonoursXDGDataHome`: `t.Setenv("XDG_DATA_HOME", dir)`, copy the fixture DB
    to `dir/opencode/opencode.db`; `New("")` yields `ses_plain`.
  - `TestSource_MissingSqlite3FailsWithItsName`: fixture path with
    `prog: "sqlite3-not-here-1874"` yields exactly one error that contains
    `sqlite3-not-here-1874`, and no session.
  - `TestSource_PlainSession`: the first two events of `ses_plain` are a request and a
    response with one `RequestID`; request `Inference.Model == "fledge-alpha-free"`, one
    user message; response `Completion == "hi"`, `InputTokens == 193`,
    `CompletionTokens == 154`, `CacheReadTokens == 7040`, `ResponseThinking.Blocks == 1`,
    `FinishReason == "stop"`; `Client.Name == "opencode"`, `Client.Version == "2.0.26"`;
    `Project` is the label of `/tmp/demo`; `AgentRole == model.AgentRoleMain`; no cost
    record (`event.Record` gives ok false: `importer.Run` prices it).
  - `TestSource_ToolCallsAndResults`: in `ses_tools`, response A has `ToolCalls` ids
    `c1`, `c2`, `c3` with names `read`, `shell`, `subagent`, and `CompletionTokens` =
    output + 14; request B has `ToolResults` for `c1`, `c2`, `c3`, and the `c2` result is
    an error; `Skipped == 2` (the `not json` row and the `bogus` row); the error row gives
    no event (`ses_tools` main events = 4).
  - `TestSource_SubagentInParentSession`: no session has ID `ses_child`; `ses_tools` holds
    the child's events with `Subagent == "ses_child"` and
    `Inference.AgentRole == model.AgentRoleSubagent`.
  - `TestSource_ForkFilesUnderSourceWithoutCopies`: no session has ID `ses_fork`;
    `ses_plain` has 4 events (its own turn and the fork's one turn); the fork's request
    `Messages` holds the copied user and assistant messages and `forked`; the fork's
    events have `Subagent == ""`.
  - `TestSource_MissingParentKeepsAffinityID`: a session `ses_gone` exists; its response
    has `TotalTokens == 0` and `Subagent == "ses_orphan"`.
  - `TestSource_NewestFirstSinceAndSkip`: ids in order start with `ses_tools`; `ses_empty`
    is not yielded; a `since` after `ses_gone`'s newest row drops it; a `skip` that gives
    true for `ses_tools` stops its read and it is not yielded; the time `skip` got for each
    group is not before that group's `Updated` from a full read.
  - `TestSource_UnreadableRoot`: build the fixture, then run
    `sqlite3 <db> "alter table session_message rename column type to type_gone"`. The
    groups `ses_tools`, `ses_plain` and `ses_gone` each yield `Unreadable == true`
    (`ses_gone` by the no-root rule of step 5), `ses_empty` is not yielded, and the
    source gives no error. (The session query
    does not read `type`, so only the messages query fails.)
- [ ] **Step 2:** In `timeline_test.go`, `TestTimeline_ImportedSessionShowsToolsAndSubagent`
  (copy the shape of `bob/timeline_test.go`): set `Seq = uint64(i + 1)` and
  `Source = model.ImportedSource("opencode")` on `ses_tools`'s events, run
  `timeline.Build`; assert tools `read` and `subagent` are in `tl.Tools`, and `tl.Agents`
  has a row with `ID == "ses_child"`.
- [ ] **Step 3:** Run `GOWORK=off go -C core test ./session/importer/opencode/` — FAIL (no package).
- [ ] **Step 4:** Implement `opencode.go` and `turns.go`. Register the source in `cmd/cortex/main.go`.
- [ ] **Step 5:** Run `GOWORK=off go -C core test -race ./session/importer/...` and
  `GOWORK=off go -C cmd/cortex build ./...` — PASS.
- [ ] **Step 6:** Commit: `feat: Import OpenCode sessions from opencode.db`.

### Task T2: Docs

Depends on: none

**Files:**
- Modify: `docs/session-api.md` (the `**Sources.**` paragraph near line 504, and a new
  OpenCode paragraph after the Bob bullets, before `## POST /v1/ingest`)
- Modify: `cmd/agentop/README.md` (the example block near line 787 and the `--agent` row near line 795)
- Modify: `docs/laptop-service.md` ("Importing history from before Cortex", near line 599)
- Modify: `docs/agents/opencode.md` (a new section `## Importing history` just before
  `## Known issues`: one short paragraph that points to `agentop import --agent opencode`
  and to the `agentop import` section of `cmd/agentop/README.md`)

Facts to write (all from this plan):
- Supported sources now: `claude-code`, `bob` and `opencode`. Change "#1873 to #1875 add
  the other agents (`codex`, `opencode`, `pi`)" to name only `codex` and `pi` (#1873, #1875).
  The proxy answers the others with an error that names the supported agents.
- The OpenCode source reads `$XDG_DATA_HOME/opencode/opencode.db`, or
  `~/.local/share/opencode/opencode.db` when `XDG_DATA_HOME` is not set. It runs
  `sqlite3 -readonly -json` as a subprocess, as the Bob source does. When `sqlite3` is
  not on `PATH`, the `opencode` result has an error that names the program. When the
  database does not exist, the source imports nothing.
- The session id is the id that OpenCode sends as `X-Session-Id`: `parent_id` for a
  subagent, `fork_session_id` for a fork, else the session's own id. A subagent's events
  are in its parent's session with `subagent` set to the subagent's own id. A fork's own
  turns are in its source's session. The fork's copies of the source's earlier messages
  give no turns.
- Each event has the `opencode` client with OpenCode's version, the model id that
  OpenCode records, and the project of the session's directory.
- Cost: Cortex prices each turn at today's rates, with the OpenCode Zen endpoint
  (`opencode.ai`) for the rate lookup. A Zen `*-free` model prices as zero. Output tokens
  include OpenCode's reasoning tokens. A turn with no token counts gets no cost.
- OpenCode does not store its system prompt or its tool list, so an imported request has
  neither.
- README: add `agentop import --agent opencode   # OpenCode, from ~/.local/share/opencode/opencode.db`;
  in the `--agent` row, say `claude-code`, `bob` and `opencode` are supported now, and that
  `bob` and `opencode` need the `sqlite3` program on `PATH`.

- [ ] **Step 1:** Edit the four files. Keep the existing style. STE prose.
- [ ] **Step 2:** Commit: `docs: Describe the OpenCode import source`.

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: Import OpenCode transcripts with agentop import` | (none) | T1, T2 | 1050 |

Estimates: T1 about 1000 (source about 420, fixture about 80, tests about 500), T2 about
50. The total is below 2000, so one PR.

## Gate

From the worktree root:

```sh
GOWORK=off go -C core vet ./... && GOWORK=off go -C core test -race ./session/importer/... ./sessionapi/... && GOWORK=off go -C cmd/cortex vet ./... && GOWORK=off go -C cmd/cortex build ./... && test -z "$(gofmt -l core/session/importer cmd/cortex)"
```

Gate check (2026-11-16, on the unchanged base 5392467be): each `go vet` step and the
`gofmt` step ran from the worktree root and passed. No step was changed.

## For a human after review

- Run `agentop import --agent opencode --dry-run`, then `agentop import --agent opencode`,
  against the real local Cortex and the real `opencode.db`. Open
  `ses_f00ff1689ffeGD4X9nXaHggqh1` in the Timeline and check its tool calls and the
  subagent row `ses_f00cfe772ffeq85psJNpVLmC69`. Needs T1 merged and a Cortex built from
  it (this restarts the shared proxy: coordinate first).
- In the PR body: the columns `session_v2.parent_id` (parent) and
  `session_v2.fork_session_id` (fork source), the one-step affinity rule from the
  OpenCode binary, Decision 7's `-free` caveat, Decision 15 (title from events), and that
  the fork rule is inferred (no fork in the real DB).
- PR #1908 (Codex) edits the same `Sources` line and the same doc lines. The PR that
  merges second resolves the conflict.
