# Import Codex transcripts (#1873) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `agentop import --agent codex` imports Codex history from
`$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl` (default `~/.codex`).

**Architecture:** A new importer source `core/session/importer/codex`, built like
`core/session/importer/claude`. It reads each rollout file line by line. It keeps the
`response_item` payloads as the Responses API `input` array of the conversation, and closes
one model turn at each `token_count` event (or at the next input item). It runs each turn
through `inference.ParseRequest` / `inference.ReadResponse` with the path `/v1/responses`,
as the live proxy parses Codex. `importer.Run` prices the turns from the rate table.

**Tech Stack:** Go 1.26.10, `encoding/json`, `bufio`, `github.com/klauspost/compress/zstd`
(already a direct requirement of `core/go.mod`).

**Spec:** issue #1873 (body), and the shared path of #1871 as merged
(`core/session/importer/types.go`, `importer.go`, `cost.go`). Template sources:
`core/session/importer/claude/{claude.go,turns.go}`.

## Global Constraints

- A source only parses. It never touches the archive, the store or the ledger.
- Implement `importer.Source` exactly as it is in `core/session/importer/types.go` on the
  base (`Agent()`, `PricingHost()`, `Sessions(since, skip)`). Do not change that interface.
- Agent name: `codex` (the canonical client name, `core/model/client.go`: `"codex_exec": "codex"`).
- Honour `CODEX_HOME`: when it is set and not empty, the store is `$CODEX_HOME/sessions`.
  Else `~/.codex/sessions`.
- Price through the existing pricing table (`importer.price` does it; the source sets no
  cost record). A turn with no usage gets no cost record.
- Do not add a module requirement. `go mod tidy -diff` must stay clean in `core`.
- Run Go commands with `GOWORK=off`. Log with `log/slog`.
- Commits: `git commit -s`, and end the message with
  `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. No `Co-Authored-By`.
- Write prose and comments in ASD-STE100 Simplified Technical English.

## Facts from the Codex source (openai/codex tag `rust-v0.169.0`, read 2026-11-16)

Codex is not installed on the laptop that wrote this plan. These facts come from the
Rust source, not from a real rollout file. See "For a human after review".

- **Line shape** (`codex-rs/history/src/lib.rs` `RolloutLine`,
  `rollout_payload.rs` `RolloutItemWire`, `#[serde(tag = "type", rename_all = "snake_case")]`):
  `{"timestamp":"<RFC 3339>","ordinal":<n, optional>,"type":"<kind>","payload":{...}}`.
  Kinds: `session_meta`, `response_item`, `event_msg`, `turn_context`, `compacted`,
  `token_usage_record`, `dynamic_tools_updated`, `inter_agent_communication`, `world_state`,
  `retained_context`, `security_risk_score`, `realtime_item`, and others.
- **`session_meta` payload** (`protocol.rs` `SessionMeta`): `id` (thread id, a UUID),
  `session_id` ("equal to the root thread's ID"; when absent, Codex reads it as `id`),
  `timestamp`, `cwd`, `originator`, `cli_version`, `source`, `model_provider`,
  `agent_role`, `agent_nickname`, `parent_thread_id`, `forked_from_id`. `source` is a
  string (`"cli"`, `"vscode"`, `"exec"`, `"mcp"`) or an object
  (`{"subagent": {"thread_spawn": {"parent_thread_id": ..., "depth": 1, "agent_role": ...}}}`,
  `{"subagent": "review"}`, `{"internal": ...}`).
- **`turn_context` payload**: `cwd`, `model` (for example `gpt-5-codex`), `turn_id`, `effort`.
- **`response_item` payload** is one Responses API item (`models.rs` `ResponseItem`):
  `message` (`role`, `content: [{type: input_text|output_text, text}]`), `reasoning`
  (`summary: [{type: summary_text, text}]`, `content`, `encrypted_content`),
  `function_call` (`name`, `arguments` string, `call_id`), `function_call_output`
  (`call_id`, `output`), `custom_tool_call` (`name`, `input`, `call_id`),
  `custom_tool_call_output`, `local_shell_call`, `web_search_call`, `tool_search_call`,
  `tool_search_output`, `image_generation_call`, `additional_tools` (`role`, `tools`),
  `agent_message`, `configuration_update`, `compaction`, `context_compaction`.
- **`event_msg` payload** has its own `type`. `token_count`: `{"type":"token_count",
  "info": null | {"total_token_usage": U, "last_token_usage": U, "model_context_window": n},
  "rate_limits": ...}`. `U` = `{input_tokens, cached_input_tokens,
  cache_write_input_tokens (default 0), output_tokens, reasoning_output_tokens, total_tokens}`.
  `input_tokens` includes `cached_input_tokens` (OpenAI semantics).
- **`token_usage_record` payload**: `{thread_id, turn_id, session_id, root_turn_id,
  response_id, usage: U, turn_token_usage: U, thread_token_usage: U}`. `usage` is the one response.
- **File names** (`rollout/src/rollout_file_name.rs`): `rollout-YYYY-MM-DDThh-mm-ss-<thread uuid>.jsonl`,
  or `...-<thread uuid>_<rollout uuid>.jsonl` for a reverted thread. A rollout older than
  7 days can be compressed to `<name>.jsonl.zst` (`rollout/src/compression.rs`).
- **The `Session-Id` header** (`codex-api/src/requests/headers.rs`, `core/src/client.rs`
  `responses_session_id`): for a non-root agent it is `metadata.session_id`; for a root
  agent it is `session_id_override`, else `"<source>:<parent>"` for an `internal` source
  with a parent thread, else `metadata.session_id`. So for a normal root session and for
  a spawned subagent, the header equals `session_meta.session_id` (the root thread id,
  which is also the root file's uuid). Cortex files Codex under this header
  (`CodexSessionHeader = "Session-Id"`, `core/session/idheader.go`).

## Decisions (made here, with the reason)

1. **Session id** = `session_meta.payload.session_id`, else `payload.id`. Reason: this is
   the value Codex sends as `Session-Id` (Facts, last bullet), so the skip-live rule of
   `importer.Run` matches a live session. A file whose `id` differs from its session id
   is a child: its events go in the session of that id with `Subagent` = the file's
   `id` and `Inference.AgentRole = model.AgentRoleSubagent`. Root events get
   `model.AgentRoleMain`. `SubagentKind` and `ParentSubagent` stay empty (those are Bob's
   and Claude Code's fields).
2. **Files read:** every regular file under `<home>/sessions` (walk all depths) whose name
   starts with `rollout-` and ends with `.jsonl` or `.jsonl.zst`. A `.zst` file is read
   through `zstd.NewReader`. `archived_sessions/` and the opt-in SQLite thread store
   (`codex migrate-rollouts`) are not read. Reason: the issue names `sessions/` only.
3. **Grouping needs the first line of every file.** The listing reads only the first line
   (the `session_meta` line) of each file. A file whose first line is not a
   `session_meta` line, or that cannot be opened, is left out with `slog.Warn` (it names
   no session, so no session can count it). Two files with one thread `id`: the newer
   mtime wins (a reverted thread, or a `.jsonl` and its `.jsonl.zst` during compression).
4. **Order, `since` and `skip`:** the session time = the newest mtime of its files. Sort
   newest first, then by id. With a non-zero `since`, drop a session whose files are all
   older than `since`. Call `skip(id, sessionTime)` before the parse. Reason: Codex writes
   each line when it happens, so no line is newer than its file (the same rule as the
   open PR #1893 uses for Claude Code). `importer.Run` calls `skip` again with the parsed
   `Updated`.
5. **Unreadable:** when the file whose `id` equals the session id fails to open or read,
   yield `Session{ID, Unreadable: true}` (with `slog.Warn` that names the path). A child
   file that fails → `Skipped++`, go on. A session with no root file (only children
   found) is yielded with the children's events.
6. **Turn rule** (per file, in line order). Keep `conv []json.RawMessage` (every
   `response_item` payload so far), `out []json.RawMessage` (the output items of the open
   turn), `model` and `cwd` (from the newest `turn_context`; `cwd` starts as
   `session_meta.cwd`), `inAt` (timestamp of the newest input item), `prevAt` (the previous
   response's `At`), and `n` (the turn number in this file).
   - Output items: `type` in `reasoning`, `function_call`, `custom_tool_call`,
     `local_shell_call`, `web_search_call`, `tool_search_call`, `image_generation_call`,
     or `type == "message"` with `role == "assistant"`. Every other `response_item` is an
     input item.
   - Input item: when `out` is not empty, close the turn with no usage. Then append the
     item to `conv` and set `inAt`.
   - Output item: append it to `out` (not to `conv` yet).
   - `event_msg` with `payload.type == "token_count"` and non-null `info`: when `out` is
     not empty, close the turn with `info.last_token_usage`. When `out` is empty, ignore
     it (Codex also sends `token_count` for rate-limit updates; this stops a double count).
   - `token_usage_record`: the same as `token_count`, with `payload.usage`.
   - `compacted`: close an open turn with no usage; then `conv` = `payload.replacement_history`
     when it is a non-empty array, else an empty array.
   - `turn_context`: set `model` and `cwd` from it.
   - A line that is not JSON, or that has no `type` → `Skipped++`. Every other kind is ignored.
   - End of file: close an open turn with no usage.
   - **Close a turn:** request body `{"model": model, "input": conv}`; `req :=
     inference.ParseRequest("/v1/responses", nil, body)`; nil → `Skipped++`, drop `out`.
     Response body `{"model": model, "output": out, "usage": {...}}`, where `usage` (only
     when the turn has a usage `U`) is `{"input_tokens": U.input_tokens,
     "input_tokens_details": {"cached_tokens": U.cached_input_tokens,
     "cache_write_tokens": U.cache_write_input_tokens}, "output_tokens": U.output_tokens,
     "output_tokens_details": {"reasoning_tokens": U.reasoning_output_tokens},
     "total_tokens": U.total_tokens}`. `resp := *req`;
     `inference.ReadResponse("/v1/responses", false, respBody, &resp)`. Set
     `AgentRole` on both. Thinking: for each `reasoning` item in `out`,
     `th.Add("thinking", <bytes of every summary[].text and content[].text>, len(encrypted_content), 0)`;
     set `resp.ResponseThinking = th` when there is at least one. No `FinishReason`: a
     rollout records none per response. Then append `out` to `conv`, clear `out`, `n++`.
   - **Event fields** (both events): `Direction: model.Outbound`, `Phase` request/response,
     `RequestID: "<file thread id>:<n>"` (unique in the session, stable across runs),
     `Client: &model.EventClient{Name: "codex", Version: session_meta.cli_version}`,
     `Project: project.Label(project.Root(cwd, home), home)` (cache by cwd, as
     `claude/turns.go` does), `Subagent` (Decision 1). Response `At` = timestamp of the line
     that closed the turn (the `token_count` line, else the last output item). Request `At` =
     `inAt`, else the first output item's timestamp; never before `prevAt`. Intern each
     event with one `session.Interner` per session (`InternEvent`), as `claude/turns.go` does.
7. **Merge:** events of all files of a session are sorted stable by `At`; `Updated` = the
   max event `At`; `SizeHint` = the sum of file sizes on disk.
8. **`PricingHost()`** = `"api.openai.com"`. Reason: the bundled table prices the GPT
   models under host `*` (`core/cost/pricing/bundled.go`, for example `gpt-5-codex`), and a
   rollout does not say whether Codex used an API key or a ChatGPT login.
9. **Title:** none from the source. The archive folds it from the user messages, as for a
   live session (`importer.Session` has no `Title`).
10. **Fixtures are synthetic,** written from the Facts above, because Codex is not
    installed here. The human step below compares them with a real, scrubbed rollout.

## Review Focus

1. A `token_count` with `info: null`, or a second `token_count` after a turn closed, must
   not make a turn or a cost. Test: `TestSource_DuplicateTokenCountIsIgnored` (T1).
2. A rollout compressed to `.jsonl.zst` must import like a plain one. Test:
   `TestSource_CompressedRollout` (T1; the test writes the `.zst` file with `zstd.NewWriter`
   in `t.TempDir()`, so no binary file is in git).
3. A spawned subagent file must not become its own session. Test:
   `TestSource_SubagentInRootSession` (T1).
4. `CODEX_HOME` set to a directory must be read in place of `~/.codex`. Test:
   `TestSource_HonoursCodexHome` (T1).
5. A missing `sessions/` directory (Codex not installed) gives no session and no error.
   Test: `TestSource_NoSessionsDirYieldsNothing` (T1).
6. Interface drift: open PR #1893 adds a `progress` argument to `Source.Sessions`. If it
   merges first, the PR rebase adds that argument here as #1893 does for Claude Code.

---

### Task T1: The Codex source, its fixture tests, and its registration

Depends on: none

**Files:**
- Create: `core/session/importer/codex/codex.go` (home dir, listing, grouping, `Sessions`)
- Create: `core/session/importer/codex/turns.go` (one rollout file → events)
- Create: `core/session/importer/codex/testdata/sessions/2026/11/02/rollout-2026-11-02T10-00-00-11111111-1111-4111-8111-111111111111.jsonl` (plain)
- Create: `core/session/importer/codex/testdata/sessions/2026/11/03/rollout-2026-11-03T09-00-00-22222222-2222-4222-8222-222222222222.jsonl` (tools)
- Create: `core/session/importer/codex/testdata/sessions/2026/11/03/rollout-2026-11-03T09-05-00-33333333-3333-4333-8333-333333333333.jsonl` (subagent of 2222…)
- Create: `core/session/importer/codex/codex_test.go`
- Create: `core/session/importer/codex/timeline_test.go`
- Modify: `cmd/cortex/main.go` (import `codeximport "github.com/rossoctl/cortex/core/session/importer/codex"`;
  `Sources: []importer.Source{claudeimport.New(""), bobimport.New(""), codeximport.New("")}`, near line 1283)

**Interfaces:**
- Produces: `func New(home string) importer.Source`. `home ""` resolves `$CODEX_HOME`, else
  `~/.codex`, when `Sessions` runs. `Agent()` = `"codex"`, `PricingHost()` = `"api.openai.com"`.

**Fixtures** (synthetic, see Decision 10; one JSON object per line, shapes from Facts):
- Plain `1111…`: `session_meta` (`id` 1111…, no `session_id`, `cwd` `/tmp/demo`,
  `cli_version` `0.169.0`, `source` `"cli"`); `turn_context` (`model` `gpt-5-codex`,
  `cwd` `/tmp/demo`); `response_item` message `developer`; `response_item` message
  `user` `hello codex`; `event_msg` `user_message` (ignored); `event_msg` `token_count`
  with `info: null`; `response_item` reasoning (one `summary_text` `thinking`, `encrypted_content` `abc`);
  `response_item` message `assistant` `hi`; `event_msg` `token_count` with
  `last_token_usage {input_tokens 1200, cached_input_tokens 1000, output_tokens 50,
  reasoning_output_tokens 20, total_tokens 1250}`; then a second identical `token_count`;
  then one line `not json`.
- Tools `2222…`: `session_meta` (`id` = `session_id` = 2222…, `cwd` `/tmp/tools`);
  `turn_context` (`gpt-5-codex`); `response_item` `additional_tools` (role `developer`,
  `tools: [{"type":"namespace","name":"functions","tools":[{"type":"function","name":"shell","description":"Run a command","parameters":{"type":"object"}}]}]`);
  user message; `function_call` `shell` `call_id` `c1`, `arguments` `{"command":["ls"]}`;
  `custom_tool_call` `apply_patch` `call_id` `c2` `input` `*** Begin Patch`;
  `token_count` (usage 500/0/30/0/530); `function_call_output` `c1` `a.txt`;
  `custom_tool_call_output` `c2` `Done`; assistant message `listed` (no `token_count`
  after it: closes at EOF with no usage).
- Subagent `3333…`: `session_meta` (`id` 3333…, `session_id` 2222…, `source`
  `{"subagent":{"thread_spawn":{"parent_thread_id":"2222…","depth":1,"agent_role":"explorer"}}}`,
  `cwd` `/tmp/tools`); `turn_context`; user message; assistant `found`; `token_count` (100/0/10/0/110).
- Timestamps: each line 1 s after the one before; the tools session's newest line is newest.
  Git does not keep mtimes, so `fixtures(t)` sets the mtime of each copied file to the
  `timestamp` of its last JSON line with `os.Chtimes`. Every test that uses `fixtures(t)`
  gets these mtimes.

- [ ] **Step 1: Write the failing tests** in `codex_test.go`. A helper `fixtures(t)` copies
  `testdata` into `t.TempDir()` and returns that home. A helper `readAll(t, src, since)`
  collects `Sessions(since, func(string, time.Time) bool { return false })` and fails on an error.
  - `TestSource_AgentAndHost`: `"codex"`, `"api.openai.com"`.
  - `TestSource_NoSessionsDirYieldsNothing`: `New(t.TempDir())` yields 0 sessions, no error.
  - `TestSource_HonoursCodexHome`: `t.Setenv("CODEX_HOME", fixtures(t))`; `New("")` yields
    the session ids `2222…` and `1111…`.
  - `TestSource_PlainSession`: session `1111…` has 2 events with one `RequestID`
    (`1111…:1`); request `Inference.Model == "gpt-5-codex"`, `Messages` roles
    `developer`, `user`; response `Completion == "hi"`, `InputTokens == 200`,
    `CacheReadTokens == 1000`, `CompletionTokens == 50`, `ReasoningTokens == 20`,
    `TotalTokens == 1250`, `ResponseThinking.Blocks == 1`;
    `Client.Name == "codex"`, `Client.Version == "0.169.0"`; `Project` is the label of
    `/tmp/demo`; `Inference.AgentRole == model.AgentRoleMain`; `Skipped == 1`.
  - `TestSource_DuplicateTokenCountIsIgnored`: in `1111…`, exactly one response event.
  - `TestSource_ToolCallsAndResults`: in `2222…`, the first response has `ToolCalls` ids
    `c1` (`shell`, `Arguments` `{"command":["ls"]}`) and `c2` (`apply_patch`); the first
    request's `Tools` names are `[shell]`; the second request's `ToolResults` ids are `c1`,
    `c2`; the second response has `Completion == "listed"` and no token counts.
  - `TestSource_SubagentInRootSession`: no session has id `3333…`; `2222…` holds events
    with `Subagent == "3333…"` and `Inference.AgentRole == model.AgentRoleSubagent`, and
    their `Project` is the label of `/tmp/tools`.
  - `TestSource_NewestFirstSinceAndSkip`: ids in order are `2222…`, `1111…`; a `since`
    1 s after the plain file's mtime (and before the tools files' mtimes) drops `1111…`; a `skip` that gives true for `2222…`
    stops it, and the time it got is not before that session's `Updated` from a full read.
  - `TestSource_CompressedRollout`: replace the plain file in the copy with
    `<name>.zst` written by `zstd.NewWriter`; the session `1111…` equals the plain read
    (same event count and `Completion`).
  - `TestSource_UnreadableRootIsReported`: the package has an unexported variable
    `var openRollout = openFile` (`func(path string) (io.ReadCloser, error)`; it handles
    `.zst`), and both the listing and the parse open files through it. The test replaces it
    with a wrapper that fails the second open of the tools root file (`2222…`). The session
    `2222…` is yielded with `Unreadable == true`, and the source gives no error.
  - `TestSource_UnlistableFileIsLeftOut`: a wrapper that fails every open of the plain file
    (`1111…`) gives no session `1111…`, no error, and the session `2222…` as before.
- [ ] **Step 2:** In `timeline_test.go`, `TestTimeline_ImportedSessionShowsToolsAndSubagent`
  (copy the shape of `claude/timeline_test.go`): set `Seq = uint64(i + 1)` and
  `Source = model.ImportedSource("codex")` on `2222…`'s events, run `timeline.Build`;
  assert tools `shell` and `apply_patch` are in `tl.Tools`, and `tl.Agents` has a row with
  `ID == "3333…"`.
- [ ] **Step 3:** Run `GOWORK=off go -C core test ./session/importer/codex/` — FAIL (no package).
- [ ] **Step 4:** Implement `codex.go` and `turns.go` (Decisions 1-9). Register the source in
  `cmd/cortex/main.go`.
- [ ] **Step 5:** Run `GOWORK=off go -C core test -race ./session/importer/...` and
  `GOWORK=off go -C cmd/cortex build ./...` — PASS. Run `GOWORK=off go -C core mod tidy -diff` — no output.
- [ ] **Step 6:** Commit: `feat: Import Codex rollout files with agentop import`.

### Task T2: Docs

Depends on: none

**Files:**
- Modify: `docs/session-api.md` (the `**Sources.**` paragraph near line 487, and the cost
  bullet "Cortex prices a Claude Code turn at today's rates" near line 470)
- Modify: `cmd/agentop/README.md` (the `--agent` row near line 794, and the example block near line 787)
- Modify: `docs/laptop-service.md` ("Importing history from before Cortex", near line 599)

Facts to write (all from this plan):
- Supported sources now: `claude-code`, `bob` and `codex`. #1874 and #1875 add the others
  (`opencode`, `pi`). The proxy answers the others with an error that names the supported agents.
- The Codex source reads `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`, or the same path
  under `CODEX_HOME` when that variable is set. It also reads a rollout that Codex
  compressed to `.jsonl.zst`. It does not read `archived_sessions/`. A missing
  `sessions/` directory imports nothing.
- The session id is the rollout's `session_meta.session_id` (else its `id`): the value
  that Codex sends in the `Session-Id` header. A subagent's rollout is in its root
  session, with `subagent` set to the subagent's thread id.
- Each Codex event has the `codex` client, the model of the turn (`turn_context.model`),
  and the project of the turn's working directory.
- A Codex turn is priced at today's rates from its `token_count` usage. A turn with no
  `token_count` has no `cost` record.
- `cmd/agentop/README.md` example line: `agentop import --agent codex   # Codex's rollouts, from ~/.codex/sessions`.
- laptop-service.md: say that `agentop import` also adds the Codex sessions in
  `~/.codex/sessions` (or under `CODEX_HOME`), and that a Codex turn is priced at today's rates.

- [ ] **Step 1:** Edit the three files. Keep the existing style; STE prose.
- [ ] **Step 2:** Commit: `docs: Describe the Codex import source`.

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: Import Codex transcripts with agentop import` | (none) | T1, T2 | 950 |

Estimates: T1 about 920 (source about 420, fixtures about 40, tests about 460), T2 about
30. The total is below 2000, so one PR.

## Gate

From the worktree root:

```sh
GOWORK=off go -C core vet ./... && GOWORK=off go -C core test -race ./session/importer/... ./sessionapi/... && GOWORK=off go -C core mod tidy -diff && GOWORK=off go -C cmd/cortex vet ./... && GOWORK=off go -C cmd/cortex build ./... && test -z "$(gofmt -l core/session/importer cmd/cortex)"
```

## Gate check (run on the base, 2026-11-16)

From the worktree root, on the unchanged base (edd296580), each format and vet step of the
gate was run alone, and each passed with no change to how it is called:
`GOWORK=off go -C core vet ./...`, `GOWORK=off go -C cmd/cortex vet ./...`,
`test -z "$(gofmt -l core/session/importer cmd/cortex)"`. The plan review did not run the
test, build and `mod tidy -diff` steps.

## For a human after review

Needs T1 merged (or the PR branch built), on a machine with Codex installed.

1. **The id check the issue asks for.** Run one Codex session through the local Cortex
   (`agentop configure codex`, then `codex exec "say hi"`). Take the session id that
   agentop shows for it. Open the newest file under `~/.codex/sessions/` and read
   `payload.session_id` (else `payload.id`) of its first line. They must be equal. If they
   differ, stop and report it in the PR body: the skip-live rule depends on it. The plan's
   claim that they are equal comes from the Codex source only (Facts, last bullet).
2. **Fixture check.** Compare one real rollout file with the fixtures in
   `core/session/importer/codex/testdata/`. If a line kind or field differs, scrub a real
   file (replace prompts, paths and ids) and replace the fixtures in a follow-up commit on the PR.
3. Run `agentop import --agent codex --dry-run`, then `agentop import --agent codex`,
   against the real local Cortex. Check that Codex sessions list with project, model,
   tokens and estimated cost, and that the Timeline opens one with its tool calls. A
   Cortex built from the branch restarts the shared proxy: coordinate first.
4. Put the result of step 1, and Decision 10 (synthetic fixtures), in the PR body.
