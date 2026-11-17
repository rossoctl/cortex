# Import Pi transcripts (#1875) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `agentop import --agent pi` imports Pi history from
`~/.pi/agent/sessions/<cwd dir>/<time>_<id>.jsonl`.

**Architecture:** A new importer source `core/session/importer/pi`. It reads each Pi
session file, walks the entry tree, and rebuilds each assistant message as an Anthropic
Messages request and response body. It runs the bodies through `inference.ParseRequest` /
`inference.ReadResponse` at `/v1/messages`, as the Claude Code and OpenCode sources do.
`importer.Run` then prices each response through the pricing table.

**Tech Stack:** Go 1.26.10, standard library only.

**Spec:** issue #1875 (body), and the shared path of #1871 as merged
(`core/session/importer/types.go`, `importer.go`, `cost.go`). The Claude Code source
(`core/session/importer/claude/claude.go`, `turns.go`) is the pattern to follow.

## Global Constraints

- Implement `importer.Source` exactly as merged in `core/session/importer/types.go`:
  `Agent()`, `PricingHost()`,
  `Sessions(since, skip func(id string, updated time.Time) bool, progress func(done, total int))`.
  Do not change that interface or any code in `core/session/importer/*.go`.
- A source only parses. It never touches the archive, the store or the ledger.
- Agent name: `pi` (the canonical client name, `core/model/client.go:413`).
- Do not use Pi's own `usage.cost`. The issue says: price through the pricing table.
- Run Go commands with `GOWORK=off`. Log with `log/slog`.
- Commits: `git commit -s`, and end the message with
  `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. No `Co-Authored-By`.
- Write prose and comments in ASD-STE100 Simplified Technical English.

## Facts measured on Pi 1.4.0 (2026-11-16)

Source read: `~/.pi/agent/install/releases/1.4.0/node_modules/@earendil-works/pi-coding-agent/dist/core/session-manager.js`,
and 8 real session files (78 lines) under `~/.pi/agent/sessions/`.

- **The id check passes.** The extension from `agentop configure pi` sends
  `ctx.sessionManager.getSessionId()`. `getSessionId()` returns `this.sessionId`, and
  `_loadEntries` sets it to `header.id`, the `id` of the file's `type: "session"` line.
  `newSession` writes the file as `<iso time with : and . as ->_<sessionId>.jsonl`. So the
  Cortex session id is the header `id`. Put this in the PR body.
- Directory: `<agent dir>/sessions/--<cwd with leading / removed and / \ : as ->--/`.
  The agent dir is `$PI_CODING_AGENT_DIR` (a leading `~` means HOME), else `~/.pi/agent`
  (`piExtensionPath` in `cmd/agentop/cmd_pi.go` has the same rule).
- Line 1 is the header:
  `{"type":"session","version":3,"id":"01a1ac8e-...","timestamp":"2026-11-05T16:33:01.612Z","cwd":"/private/tmp/bt/c2/demo"}`.
  `parentSession` is optional (a fork). The directory name is lossy, so the project
  comes from `cwd`, not from the directory.
- Every other line is an entry: `{"type", "id" (8 hex), "parentId" (id or null), "timestamp" (ISO)}`
  and fields by type. The entries form a **tree**: a branch is two entries with one
  `parentId`. Types: `message`, `model_change`, `thinking_level_change`, `compaction`,
  `branch_summary`, `custom`, `custom_message`, `label`, `session_info`, `context_edit`.
- `message` entries hold `message.role`:
  - `system`: `content` (string, `""` on all 8 real files), `sections` (object),
    `toolsAdded` (array of `{name, description, parameters}`).
  - `user`: `content` (string, or array of `{type:"text",text}` / `{type:"image",data,mimeType}`).
  - `assistant`: `content` array of `{type:"text",text}`, `{type:"thinking",thinking,thinkingSignature}`,
    `{type:"toolCall",id,name,arguments}` (`arguments` is a JSON object); `api`,
    `provider`, `model` (for example `aws/claude-opus-5-5`, `claude-sonnet-5-5`),
    `usage {input, output, cacheRead, cacheWrite, totalTokens, cost{...}}`,
    `stopReason` (`stop`, `toolUse`, `length`, `error`, `aborted`), `responseId`
    (missing on an error turn), `errorMessage`, and `timestamp` (milliseconds since the
    epoch, the time the request started). The entry `timestamp` is the time Pi wrote the
    finished message.
  - `toolResult`: `toolCallId`, `toolName`, `content` (array of text/image blocks), `isError`.
  - Other roles (`bashExecution`, `custom`, ...) exist in Pi's types.
- `usage.input` excludes the cached tokens on every provider (Anthropic semantics).
- An error turn: `content: []`, every usage figure 0, `stopReason: "error"`.
- Pi builds the model context by `buildSessionContext` (session-manager.js lines 124-285):
  1. Path = the entry, then each `parentId` up to the root, reversed.
  2. If the path has a `compaction` entry, take the **last** one, C. Context entries = C,
     then the path entries before C from `C.firstKeptEntryId` on (leave out `system`
     messages), then every path entry after C.
  3. Each `context_edit` in the context entries edits the entry `targetId`:
     `replacement: null` removes that entry's messages. Otherwise the entry's
     `user`/`assistant`/`toolResult` message gets `content = replacement.content`
     (a string for assistant/toolResult becomes `[{type:"text",text:<string>}]`).
  4. Entries to messages: `message` → its message. `custom_message` → a user message with
     `content`. `branch_summary` with `summary` → a user message with the summary text.
     `compaction` → a user message with `summary`. Other types → nothing.

## Decisions (made here, with the reason)

1. **Session id** = the header `id`. Reason: it is the `x-session-id` that live capture
   records (fact above). A file whose first parsed line is not a `session` header gives
   `importer.Session{ID: <file name after the first "_", without ".jsonl">, Unreadable: true}`.
   Reason: Pi gives such a file a new random id when it loads it, so no live id can match.
2. **One file is one session.** `parentSession` (a fork) is not followed. Reason: a fork
   is its own Pi session with its own id on the wire. Pi has no built-in subagent, so
   `Subagent` stays empty and `AgentRole` is `model.AgentRoleMain` on every event.
3. **One turn for each `message` entry with role `assistant`**, error turns included.
   Reason: each one is one API call. An error turn has zero usage, so the parser sets no
   token counts and `price` gives it no cost record (the issue's rule).
4. **Request `Messages`** = the context of the assistant entry's `parentId`, built by the
   four Pi steps above, then converted (Decision 6). Reason: this is what Pi sent, also
   on a branch and after a compaction.
5. **Text of the synthetic user messages** (Pi step 4), copied from Pi 1.4.0's
   `dist/core/messages.js` (`COMPACTION_SUMMARY_PREFIX`/`_SUFFIX`, `BRANCH_SUMMARY_PREFIX`/`_SUFFIX`).
   Each is one user message with one `text` block:
   - compaction: `"The conversation history before this point was compacted into the following summary:\n\n<summary>\n" + summary + "\n</summary>"`
   - branch summary: `"The following is a summary of a branch that this conversation came back from:\n\n<summary>\n" + summary + "</summary>"`
     (Pi's branch suffix has no newline before `</summary>`).
   - `custom_message`: a user message with its `content` (a string becomes one `text` block).
   A message entry with a role other than `system`, `user`, `assistant`, `toolResult`
   (for example `bashExecution`) is left out, and is not counted as skipped. Reason: its
   text form is Pi-internal (`bashExecutionToText`). Put this in the PR body.
6. **Wire shape: Anthropic Messages at `/v1/messages`.** Reason: Pi's `usage.input`
   excludes cached tokens, as Anthropic's `input_tokens` does, and Anthropic content
   blocks hold thinking, tool calls and tool results with no loss. Conversion:
   - `system` messages → request `system` (string) and `tools`, replayed in order as
     Pi's `getCurrentSystemMessage` does. Each tool is `{name, description, input_schema: parameters}`.
   - `user`: string stays a string. `text` → `{type:"text",text}`. `image` →
     `{type:"image",source:{type:"base64",media_type:mimeType,data}}`.
   - `assistant`: `text` → `text`. `thinking` → `{type:"thinking",thinking,signature:thinkingSignature}`.
     `toolCall` → `{type:"tool_use",id,name,input:arguments}`.
   - Consecutive `toolResult` messages → ONE user message of
     `{type:"tool_result",tool_use_id:toolCallId,content:[blocks],is_error:isError}`.
   - Response body: `{model, content, stop_reason, usage}` with
     `stop_reason`: `stop`→`end_turn`, `toolUse`→`tool_use`, `length`→`max_tokens`,
     any other value as given. `usage`:
     `{input_tokens: input, output_tokens: output, cache_read_input_tokens: cacheRead, cache_creation_input_tokens: cacheWrite}`,
     and the whole `usage` key is left out when all four are 0.
   - Do not set `TotalTokens` by hand. The parser computes it, as for the Claude source.
7. **Model** = the assistant message's `model`, on the request and the response.
8. **Pricing host** = `pi.dev`. Reason: Pi's `provider` is a free-form name (the real
   files have `ete-int`, a LiteLLM gateway). No pricing row is scoped to `pi.dev`, so
   every turn prices by model id through the `*` rows. The table accepts a
   provider-prefixed name such as `aws/claude-opus-5-5` (`gPrefixed` in
   `core/cost/pricing/table.go`).
9. **Request id** = the assistant message's `responseId` when it is not empty, else
   `<session id>/<entry id>`. Reason: `importer.Run` drops a second turn with the same
   request id across all sessions of one agent (`sent` in `toLedger`). Entry ids are only
   8 hex digits and can collide across sessions. A fork copies the entries with their
   `responseId`, so a forked turn is in the ledger once.
10. **Times:** response `At` = the entry `timestamp`. Request `At` = `message.timestamp`
    (milliseconds) when it is > 0 and not after the response `At`, else the response `At`.
    Sort the events of a session with `slices.SortStableFunc` by `At` (request first,
    then its response, on a tie). `Updated` = the newest `At`.
11. **Project** = `project.Label(project.Root(header.cwd, home), home)`, cached per cwd,
    as `claude/turns.go` `parser.project` does.
12. **Client** = `&model.EventClient{Name: "pi"}`. No version: the header `version` is the
    file format version (3), not the Pi version.
13. **Skipped lines:** a line that is not JSON, or a `message` entry with no `message`
    object, counts 1 in `Skipped`. Other entry types are not counted. A turn whose
    request `inference.ParseRequest` gives nil counts 1 in `Skipped`.
14. **Skip before the parse:** follow `claude.go`. List the files, sort by mtime (newest
    first, then by path). For each file: call `progress(i, n)`. Drop it when `since` is
    set and the mtime is before `since`. Read the header line for the id. Call
    `skip(id, mtime)`. Then a cheap pass (`newestAt`): decode only
    `{type, timestamp, message.role}` and take the newest `timestamp` of an assistant
    `message` entry. Call `skip(id, newest)`. Only then parse. Reason: Pi writes the file
    a few milliseconds after the entry timestamp, so `skip(id, mtime)` alone never finds
    a current copy, and each run would parse and replace every session.
15. **Interning:** intern each event with one `session.Interner` per session as
    `claude/turns.go` does (`p.in.InternEvent`). Reason: each request holds the whole
    conversation.
16. **SizeHint** = the size in bytes of the session file (`os.Stat` or the bytes read),
    as `claude.go` sets it. Reason: `importer.Run` uses it for the archive size check.
17. **Title** comes from the events. `importer.Session` has no `Title` field. Put this
    in the PR body.

## Review Focus

1. A branch turn sees only its own path. Test: `TestSource_BranchUsesItsOwnPath` (T1).
2. A compaction gives the summary plus the kept entries, not the whole path. Test:
   `TestSource_CompactionKeepsFromFirstKept` (T1).
3. `skip` gets a time that is not before `Updated`, and a current session is not parsed.
   Test: `TestSessions_SkipBeforeTheParse` (T1).
4. An error turn gets no token counts (so no cost record). Test:
   `TestSource_ErrorTurnHasNoUsage` (T1).
5. Parallel tool results are one user message with two `tool_result` blocks. Test:
   `TestSource_ToolCallsAndResults` (T1).

---

### Task T1: The Pi source, its tests, and its registration

Depends on: none

**Files:**
- Create: `core/session/importer/pi/pi.go` (the `Source`, file listing, `newestAt`)
- Create: `core/session/importer/pi/turns.go` (the parse: tree, context, conversion)
- Create: `core/session/importer/pi/pi_test.go`
- Create: `core/session/importer/pi/timeline_test.go`
- Create: `core/session/importer/pi/testdata/sessions/--tmp-demo--/2026-11-01T10-00-00-000Z_aaaaaaaa-0000-4000-8000-000000000001.jsonl` (plain)
- Create: `core/session/importer/pi/testdata/sessions/--tmp-demo--/2026-11-01T11-00-00-000Z_aaaaaaaa-0000-4000-8000-000000000002.jsonl` (tools)
- Create: `core/session/importer/pi/testdata/sessions/--tmp-demo--/2026-11-01T12-00-00-000Z_aaaaaaaa-0000-4000-8000-000000000003.jsonl` (branch, compaction, context_edit, error turn)
- Modify: `cmd/cortex/main.go` (add `piimport.New("")` to `Sources`, after `bobimport.New("")`)

**Interfaces:**
- Produces: `package pi`, `func New(agentDir string) importer.Source`. `agentDir ""`
  resolves `$PI_CODING_AGENT_DIR` (leading `~` = HOME), else `~/.pi/agent`, when
  `Sessions` runs. `Agent()` = `"pi"`, `PricingHost()` = `"pi.dev"`. A missing
  `<agentDir>/sessions` gives no sessions and no error. An unreadable `sessions`
  directory yields that error once.
- Import name in `cmd/cortex/main.go`: `piimport "github.com/rossoctl/cortex/core/session/importer/pi"`.

**Fixtures** (scrubbed, hand-written in the real Pi 1.4.0 shape above, header `cwd`
`/tmp/demo`, model `claude-sonnet-4-5`):
- File 1 (plain): header, `model_change`, `thinking_level_change`, user text, assistant
  text (usage input 100, output 20, cacheRead 50, cacheWrite 10), user text, assistant
  text (usage non-zero), and one line `this is not json`.
- File 2 (tools): header, `system` message (`content` `"You are pi."`, `toolsAdded`
  `read` and `bash`), user text, assistant with a `thinking` block and two `toolCall`
  blocks (`read`, `bash`, `stopReason` `toolUse`), two `toolResult` entries, assistant
  text.
- File 3 (tree): header, user `u1`, assistant `a1`, user `u2` (parent `a1`), assistant
  `a2` (parent `u2`), user `u3` (parent `a1`, a branch), assistant `a3` (parent `u3`),
  then a `compaction` entry (parent `a3`, `summary` `"S"`, `firstKeptEntryId` `u3`),
  user `u4`, an assistant `a4e` error turn (all usage 0, `stopReason` `error`, no
  `responseId`), a `context_edit` (`targetId` `a4e`, `replacement` null), user `u5`,
  assistant `a5`.
- Every assistant entry except `a4e` has a distinct `responseId`.
- In every file, the first entry after the header has `parentId` null, and each other
  entry has the entry on the line before it as `parentId`, unless the list above names
  another parent. So in file 3: `u4`'s parent is the `compaction` entry, the
  `context_edit`'s parent is `a4e`, and `u5`'s parent is the `context_edit`.

- [ ] **Step 1: Write the failing tests** in `pi_test.go` (helpers as in
  `claude/claude_test.go`: `fixtures` copies `testdata` to `t.TempDir()`, `readAll`,
  `sessionByID`, `requests`, `responses`):
  - `TestSource_AgentAndHost`: `"pi"`, `"pi.dev"`.
  - `TestSource_NoSessionsDirYieldsNothing`.
  - `TestSource_PlainSession`: 4 events, `Skipped == 1`, id = header id, `Project` =
    the label of `/tmp/demo`, `Client.Name == "pi"`, model `claude-sonnet-4-5`; turn 1
    `InputTokens == 100`, `CompletionTokens == 20`, `CacheReadTokens == 50`,
    `CacheWriteTokens == 10`; request 2 has 3 `Messages`; request `At` <= response `At`;
    `RequestID` = the `responseId`; `SizeHint` = the file size.
  - `TestSource_ToolCallsAndResults`: response 1 has 2 `ToolCalls` (`read`, `bash`) and
    `FinishReason` `tool_use`, and `ResponseThinking` counts 1 block; request 1 has 2
    `Tools`; request 2 has 2 `ToolResults` with the two call ids, in ONE user message.
  - `TestSource_BranchUsesItsOwnPath`: the request of `a3` has `u1, a1, u3` and not `u2`.
  - `TestSource_CompactionKeepsFromFirstKept`: the request of `a5` starts with the
    summary user message, then `u3`, `a3`, `u4`, `u5`, and has no `a4e` (context_edit).
  - `TestSource_ErrorTurnHasNoUsage`: the response of `a4e` has
    `pricing.UsageFromInference(e.Inference) == pricing.Usage{}`, and `RequestID` =
    `<session id>/<a4e entry id>`.
  - `TestSource_HeaderlessFileIsUnreadable`: a file in the temp dir whose first line is
    a `message` entry gives `Unreadable: true` with the id from the file name.
  - `TestSource_NewestFirstAndSince`: set mtimes with `os.Chtimes`; the order follows the
    mtime; `since` drops the older files.
  - `TestSessions_SkipBeforeTheParse`: `skip` records each `(id, time)`; for each yielded
    session, the last time given for its id is not before `Updated`; a `skip` that
    answers true for one id leaves that id out.
  - `TestSource_HonoursPiCodingAgentDir`: `t.Setenv("PI_CODING_AGENT_DIR", dir)` with
    `New("")` finds the fixtures.
  In `timeline_test.go`: `TestTimeline_ImportedSessionShowsTurnsAndTools`, as in
  `claude/timeline_test.go`: take the session of file 2 (tools), set `Seq` and
  `Source = model.ImportedSource("pi")`, run `timeline.Build`, check 2 main-agent turns
  (`turns[timeline.MainAgent] == 2`, one for each assistant message) and tools `read`
  and `bash`.
- [ ] **Step 2:** Run `GOWORK=off go -C core test ./session/importer/pi/...`. Expect FAIL (no package).
- [ ] **Step 3:** Write `pi.go` and `turns.go` by the Decisions above.
- [ ] **Step 4:** Run the tests again. Expect PASS.
- [ ] **Step 5:** Register the source in `cmd/cortex/main.go`. Run the gate command.
- [ ] **Step 6:** Commit: `feat: Import Pi transcripts with agentop import`.

Estimated counted lines: about 900 (code 450, tests 400, fixtures 45, main.go 2).

### Task T2: Docs

Depends on: none

**Files:**
- Modify: `docs/session-api.md` (the **Sources.** paragraph near line 504, and a new
  paragraph after the Bob source paragraph)
- Modify: `docs/agents/pi.md` (a short "Import history" section)
- Modify: `cmd/agentop/README.md` (one example line after the `--agent bob` line, near line 789)
- Modify: `docs/laptop-service.md` ("Importing history from before Cortex", near line 601)

Facts to write (all from this plan):
- `agentop import --agent pi` imports Pi sessions. The source reads
  `~/.pi/agent/sessions/<dir>/<time>_<id>.jsonl`, or `$PI_CODING_AGENT_DIR/sessions/`
  when that variable is set in the proxy's environment.
- One Pi session file is one session. The session id is the file's header `id`, the same
  id that the `agentop configure pi` extension sends as `x-session-id`.
- The project is the header's `cwd`. Each event has the `pi` client.
- Each assistant message is one turn, error turns included. The request holds the
  conversation that Pi sent: the turn's own branch, and after a compaction the summary
  and the kept entries.
- A Pi turn is priced at today's rates through the pricing table, by model id. Pi's own
  `usage.cost` is not used. A turn with no token counts has no cost.
- A file with no session header is counted as unreadable.
- A fork (`parentSession`) is its own session.
- In `docs/session-api.md`, the supported list becomes Claude Code, Bob and Pi. Keep the
  sentence that names the agents that are not yet supported, without `pi`.
- In `docs/laptop-service.md`, add "Pi" where the paragraph lists the sources, and add
  "A Pi turn is priced at today's rates." after the Claude Code pricing sentence.

- [ ] **Step 1:** Make the edits. Wrap lines near 80 characters, as the text near them.
- [ ] **Step 2:** Commit: `docs: Document agentop import --agent pi`.

Estimated counted lines: about 40.

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: Import Pi transcripts with agentop import` | (none) | T1, T2 | 940 |

One PR: the total is under 2000 counted lines.

## Gate

From the worktree root. The plan review ran the vet and gofmt steps on the base
(5392467be) on 2026-11-16, unchanged, and all passed:

```sh
GOWORK=off go -C core vet ./... && GOWORK=off go -C core test -race ./session/importer/... ./sessionapi/... && GOWORK=off go -C cmd/cortex vet ./... && GOWORK=off go -C cmd/cortex build ./... && test -z "$(gofmt -l core/session/importer cmd/cortex)"
```

## For a human after review

- Needs T1 merged and a Cortex built from it. A dev install restarts the shared proxy:
  coordinate first. Then run `agentop import --agent pi --dry-run`, then
  `agentop import --agent pi`, against the real `~/.pi/agent/sessions`. Open
  `01a1ac8e-7cac-7385-8291-bd9de8210c8b` in the Timeline and check its tool calls
  (`read`, `edit`, `bash`), project `/private/tmp/bt/c2/demo`, tokens and cost.
- In the PR body: the id check result (header `id` = `getSessionId()`, from Pi 1.4.0
  source), Decision 8 (pricing host `pi.dev`), Decision 17 (title from events), and
  that no real file has a branch or a compaction (those rules come from Pi's source).
- PRs #1908 (Codex) and #1913 (OpenCode) edit the same `Sources` line in
  `cmd/cortex/main.go` and the same doc lines. The PR that merges last resolves the conflict.
