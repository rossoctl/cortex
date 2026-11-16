# Import agent transcripts into the session archive (#1856)

## Why

agentop shows only the sessions that went through the proxy. A new user sees an empty
Sessions pane, even when the laptop holds months of agent history. An existing user
loses every session from before the install, from times when the proxy was off, and
from other machines.

The coding agents already write full transcripts to disk. This feature reads them and
writes them into the Cortex session archive as ordinary sessions. agentop then lists
them, and the Timeline opens them, next to live sessions.

## Goals

- Import the history of every agent that `agentop configure` supports: Claude Code,
  Bob, Codex, OpenCode and Pi.
- Full content: user messages, assistant text, thinking, tool calls and tool results,
  subagents. The Timeline must work on an imported session.
- Tokens and cost on each turn, so the Sessions, Agents and cost views include history.
- Idempotent. A second import adds no duplicates.
- A session that the proxy captured live is never changed by an import.
- An imported session is visibly marked. A field that only the proxy measures shows
  "n/a", not 0.

## Non-goals

- A background or automatic import. The user runs a command. (The issue suggests a
  background mode in `observe`. It is left out until someone asks for it.)
- Merging transcript turns into a live session, for example the turns from before the
  proxy started. The live record wins entirely. See "Live sessions".
- Agents that `agentop configure` does not support.
- Kubernetes sidecars. Transcripts exist only on a laptop.
- Export (#1810 is the other direction).

## Anti-goals

- An API route that accepts events or files from the caller. The session API has no
  authentication, so such a route would let any local process add made-up sessions.
- A second process that writes the archive. The proxy is the only writer.
- An import that stops on one bad line. Transcript formats are undocumented and change
  between agent versions.
- A large new dependency in the proxy binary (for example a pure-Go SQLite driver).

## Constraints found in the code

- **The Timeline reads parsed fields, not HTTP bodies.** `core/session/timeline` reads
  `Inference.Messages`, `ToolCalls`, `ToolResults`, `Tools`, `Model`, `AgentRole`,
  `Stream`/`StreamedResponse` and the token fields of `model.InferenceExtension`. An
  importer fills in those fields. It does not build fake request bodies.
- **Session ids already match.** The id that Cortex files a live session under is the
  agent's own id (checked on this laptop for Claude Code and Bob):

  | Agent | Store on disk | Cortex session id |
  |---|---|---|
  | Claude Code | `~/.claude/projects/<project>/<uuid>.jsonl`, subagents in `<uuid>/subagents/agent-*.jsonl`. Honours `CLAUDE_CONFIG_DIR` | the file uuid (`X-Claude-Code-Session-Id`) |
  | Bob | `~/.bob/db/bob.db`, SQLite, tables `tasks` and `messages` (WAL mode) | `tasks.id` (`X-Task-Id`). A child task files in its root task, through `parent_id` |
  | Codex | `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` (honours `CODEX_HOME`) | the session uuid (`Session-Id`) |
  | OpenCode | `~/.local/share/opencode/opencode.db`, SQLite (`session_v2`, `session_message`, ...) | the affinity id (`X-Session-Id`): the parent's id for a subagent, the source's id for a fork, else its own |
  | Pi | `~/.pi/agent/sessions/<cwd>/<time>_<uuid>.jsonl` | the Pi session id (`x-session-id`, from the extension that `agentop configure pi` installs) |

  So "is this id already in the archive or the live store?" is the whole duplicate test.
- **Retention counts from the write time.** `planPrune` compares `SegmentInfo.LastWrite`,
  which is the time that the segment was written. An imported session is kept for
  `retention_days` (30) after the import, not after the session date. When it expires,
  a new import writes it again. The day limit needs no change.
- **The size limit does need a rule.** Over `max_bytes` (2 GiB), `planPrune` deletes
  the segments with the oldest `LastWrite` first (`retention.go:46-60`). Imported
  segments have the newest `LastWrite`. So a large import would make the archive delete
  live captures. See "Archive size".
- **Cost is a plugin record on the response event.** `session.moneyOf` reads it through
  `cost/event.Record`. An importer writes the same record. It does not invent a second
  cost path.
- **Only the proxy writes the archive** (`core/session/archive`, a single writer
  goroutine). `Archive.Record` never blocks. When its queue is full, it drops the event
  and counts it (`archive.go:328-353`). Live events get their `Seq` from the store
  (`SeqSeeder`, `store.go:477`). Both are wrong for a bulk import. See "Writing to the
  archive".
- **The cost windows do not read the archive.** Today, 7d and month come from the cost
  ledger (`sessionapi/usage.go:216-234`). LAST 1H comes from the usage ring. An import
  that writes only the archive changes session rows and their cost, not those windows.
  See "Cost windows (the ledger)".

  The Claude Code and Bob rows were checked against this laptop's archive. The Codex,
  OpenCode and Pi rows come from `core/session/idheader.go` and `cmd/agentop/cmd_pi.go`.
  Each agent's PR must check its row against a real store before it relies on it.

## Approaches

**A. The proxy imports (chosen).** `agentop import` calls a new session API route. The
proxy reads the transcripts from their fixed paths, builds events, and writes them
through its own archive writer.
- The proxy stays the only writer.
- The route takes an agent name and options, never data. So it gives a local process
  no way to add made-up sessions.
- The proxy runs as the user on a laptop, so it can read the agents' files.

**B. agentop parses and POSTs events to the proxy.** Rejected: an unauthenticated route
that takes events lets any local process add any session.

**C. agentop writes the archive directly, with the proxy stopped.** Rejected: every
import stops the proxy and cuts every attached session.

The user picked A.

## Design

### Command

```
agentop import [--agent claude-code|bob|codex|opencode|pi] [--since DATE] [--dry-run]
```

- No `--agent` imports every agent whose store exists.
- `--since` skips sessions whose last activity is before DATE.
- `--dry-run` reports what an import would do and writes nothing.
- Output, one line per agent: sessions imported, replaced, skipped as live, skipped as
  current, and lines or rows skipped as not recognized.
- It is a top-level command, not `agentop experimental`. It changes nothing outside
  the archive, and the archive is the only thing it writes.

### Route

`POST /v1/import` on the session API, with a JSON body
`{"agent": "...", "since": "...", "dryRun": bool}`. The response carries the per-agent
counts above.

- The route is registered only when the archive is on (`WithArchive` non-nil) and the
  session API binds loopback only. Elsewhere it is 404. (This is new: no route
  depends on the archive today, `sessionapi/server.go:223-235`.)
- The proxy reads only the fixed store paths of the table above, resolved in the proxy's
  own environment (`HOME`, `CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `XDG_DATA_HOME`). The
  request cannot name a path.
- One import at a time. A second request while one runs gets 409.
- The work runs in the request. A large history can take time, so agentop gives the
  call a long timeout and prints a spinner.

### Package layout

- `core/session/importer/`: the shared part. It runs a source, applies the
  live/imported rules below, and writes events to the archive. It also builds the cost
  record from tokens through the existing pricing table.
- `core/session/importer/<agent>/` (one package per agent): reads one store and yields
  sessions. A source implements one small interface, about:

  ```go
  type Source interface {
      Agent() string               // "claude-code", "bob", ...
      Sessions(since time.Time) iter.Seq2[Session, error]
  }
  type Session struct {
      ID       string    // the Cortex session id, see the table above
      Updated  time.Time // last activity, for --since and for the replace rule
      Project  string    // cwd
      Title    string    // when the store has one
      Events   []*model.SessionEvent
      Skipped  int       // lines or rows not recognized
  }
  ```

  The exact shape is the builder's call. The rule that matters: a source only parses.
  It never touches the archive or the store.
- Claude Code reuses the discovery in `core/observe/claude/harvest.go` (config dir,
  `CLAUDE_CONFIG_DIR`). The lock in that package guards the metadata file, which the
  importer does not write, so the importer does not take it.

### Events a source builds

For each model turn the source builds a request event and a response event with the
same `RequestID`, both with `Direction` outbound and an `Inference` extension:

- Request: `Model`, `Messages` (the whole conversation up to this turn, as the agent
  sent it), `Tools` when the store records them, `ToolResults` for the tool results that
  this turn sends.
- Response: `Completion`, `ToolCalls`, `ResponseThinking`, `FinishReason`, and the token
  fields that the store records (`InputTokens`, `CompletionTokens`, `CacheReadTokens`,
  `CacheWriteTokens`, `TotalTokens`).
- `At` is the store's timestamp for the turn.
- A subagent's events go in its root session with `SessionEvent.Subagent` set to the
  subagent's own id, and `AgentRole` set too. The Timeline tells subagents apart by
  `Subagent` (`timeline.go:412-416`). `AgentRole` alone merges them into one row.

The archive interns on disk the same fields that the store interns, so the repeated
`Messages` prefix costs about what it costs for a live session.

### Writing to the archive

The import does not go through the session store, and it does not use `Archive.Record`.

- The store would load months of history into memory and evict live sessions
  (`max_sessions`).
- `Record` drops events when its queue is full. An import fills the queue at once.

Add one blocking operation to the archive writer: write one whole session, return when
it is on disk. It assigns `Seq` from 1 in event order. Replace (rule 3 below) is a
delete and then this write, in the same writer goroutine, so a live event cannot land
between them. The importer sends one session at a time, so live events wait for at
most one session's write.

### Archive size

An import must not make the archive delete live captures.

- Before each session, the importer checks the archive size against `max_bytes` with
  10% headroom. When the next session would pass it, the import stops and reports
  "stopped: archive full, N sessions not imported".
- Sources yield newest sessions first, so a stopped import keeps the most recent
  history.
- The day limit stays as it is (see "Constraints").

### The imported mark

Each imported event carries `Source: "imported:<agent>"` (a new `SessionEvent` field,
omitted when empty). The session fold carries it into the session summary, so the
Sessions list can mark the row without reading events.

**A session is imported only while all its events are imported.** One live event clears
the mark in the summary. This covers `claude --resume` of an imported session, or a Bob
task that the user opens again: the live turns land in the archived session, the mark
goes, and rule 1 then skips that session on every later import. So a replace can never
delete live turns, also after a proxy restart.

agentop:
- Marks an imported session row (for example with a dim `imp` tag, the builder's call).
- Shows "n/a" for proxy-only fields on an imported event or session: TTFB, duration,
  bytes up and down, hosts, HTTP status, plugin invocations.
- Opens an imported event's raw request as "not captured: imported from <agent>".
  There is no raw capture to show.

### Live sessions and re-import

For each session that a source yields:

1. The id is in the live store, or in the archive without the imported mark: **skip**.
   The proxy captured it, so the proxy's record wins. The transcript fills in nothing.
2. The id is in the archive with the imported mark, and the source's `Updated` is not
   newer than the archive's last event: **skip** (current).
3. The id is in the archive with the imported mark and the source is newer: **replace**.
   Delete the archived session, then write the full import again. This is how a
   session that continued after the last import gets its new turns.
4. Otherwise: **import**.

Rule 3 deletes only imported sessions. A live session is never deleted by an import.

A live session that started before the install loses its earlier turns. That is the
cost of rule 1. Merging the two is a non-goal.

### Cost

The cost record has no "estimated" flag. Its `Source` field names how the cost was
found (`gateway-header` or `usage-fallback` today, `cost/event/event.go:98-114`). Add
one value, `transcript`. A consumer that reads `Source` can then tell an imported cost.

- Claude Code, Codex, OpenCode, Pi: price the turn's tokens through the existing
  pricing table, at today's rates (the table has no history). `Source: transcript`,
  `Provenance` from the rate table as for live traffic.
- Bob: `messages.data._meta.spend.cost` is Bob's own price for the turn (flat
  $2/Mtok, checked against Bob's ledger in September). Write it as the settled cost,
  `Source: transcript`, `Provenance: authoritative`.
  `contextTokens` equals Cortex's `TotalTokens`. Bob records no input/output split.
- A turn with no usage gets no cost record. It is not priced as zero.

### Cost windows (the ledger)

The user chose to put imported cost into the today, 7d and month windows too. So the
importer also sends each imported response event to the cost ledger, through the
existing `ledger.Writer.Record`.

What the ledger already does, so no new write path is needed:
- `Record` files a row in the day file named from the event's `At`
  (`ledger/writer.go:420`). A back-dated event goes to its own day.
- A row for a minute that is already on disk is written straight out, not folded into
  the open minute (`writer.go:661-666`). So old minutes are safe.

What changes:
- **A new `Row.Source` field**, `transcript` on an imported row, omitted on a live
  row, and part of the row key. A reader that ignores it still sums correctly. The
  cost views can then say how much of a window comes from transcripts.
- **Ledger rows have no session id.** They are per minute, per model, agent and
  project. So a replace cannot remove one session's old rows. Rule: the ledger gets
  only the turns that the archive did not have before. On a first import that is every
  turn. On a replace it is only the turns after the archived session's last event.
  The archive is rewritten whole, but the ledger gets the new turns only, so no turn is
  counted twice.
- **Turns older than the ledger's retention cutoff** (`RetentionCutoff`, the month
  window by default) go to the archive only. A row there would be pruned on the next
  tick.
- **Dry run** writes no ledger rows.
- LAST 1H reads the usage ring, not the ledger. The importer does not feed the ring. A
  turn from the last hour shows in LAST 1H only after the next proxy restart replays it.
  That is accepted.

Rule 1 (skip live sessions) keeps live and imported cost from overlapping. A resumed
imported session is live from its first live event, so its live turns reach the ledger
the normal way and the import never sends them.

### Reading SQLite stores (Bob, OpenCode)

Run `sqlite3 -readonly -json <db> <query>` as a subprocess. macOS includes `sqlite3`.
When it is missing, that agent's import fails with an error that names the missing
program, and the other agents continue. This keeps a SQLite driver out of the proxy.
`-readonly` reads a WAL database safely while the agent writes it.

### Bad input

- A line or row that the source does not recognize is skipped and counted. It does not
  stop the import.
- A session where nothing parsed is not written.
- A file or database that cannot be opened fails that agent only, with its error in
  the response.

### Tests

- Each source has fixture tests from real, scrubbed transcripts: a plain session, a
  session with tool calls and results, a subagent, and a file with one line it does not
  recognize.
- One importer test per rule in "Live sessions and re-import", one for a live event
  that clears the mark (then a re-import must skip), and one for the stop at the
  size limit.
- Ledger: an imported row lands in its own day with `Source: transcript`. A replace
  sends only the new turns. A turn past the retention cutoff writes no row.
- One test that runs the Timeline over an imported fixture session and checks the
  turns and tool calls appear.
- One test that the route is absent when the archive is off or the API is not
  loopback-only.

## Delivery

One issue and PR per agent, in this order. Each one is sized for `/sdd-fast`.

1. **Claude Code, plus the shared part**: route, command, importer rules, ledger rows, the
   `Source` mark, agentop's "n/a" display, cost from tokens. The shared code lands with
   its first caller.
2. **Bob**: SQLite read, child tasks, Bob's own cost.
3. **Codex**.
4. **OpenCode**: SQLite read (reuses Bob's helper), affinity ids for subagents and forks.
5. **Pi**.

2 to 5 depend on 1 only, so they can run in parallel after 1 merges.

## Left to the builder

- The exact `Source` interface and package names.
- How agentop draws the imported mark.
- Each agent's field mapping, after a look at that agent's real store.
- Whether a `--since` with no date part (for example `30d`) is worth accepting.
