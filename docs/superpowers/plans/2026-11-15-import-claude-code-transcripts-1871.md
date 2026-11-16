# Import Claude Code transcripts (#1871) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `agentop import` reads Claude Code transcripts on disk and writes them into the
Cortex session archive and the cost ledger as ordinary, marked sessions.

**Architecture:** A source package parses one agent's store into `importer.Session`
values. The shared `core/session/importer` package applies the live/imported rules,
prices each turn through `settle`, writes each session to the archive with one new
blocking archive operation, and sends the new turns to the cost ledger. The proxy runs
the import in `POST /v1/import`. agentop calls the route and marks imported rows.

**Tech Stack:** Go 1.26.10, `iter.Seq2`, existing `core/session/archive`,
`core/cost/{settle,ledger,event}`, `core/parse/inference`, `core/observe/claude`.

**Spec:** issue #1871 (body), part of #1856. Read it with
`gh issue view 1871 --repo rossoctl/cortex`. This plan copies every value it needs.

## Global Constraints

- Only the proxy writes the archive. agentop never opens archive or ledger files.
- The route never accepts a path, events or files from the caller.
- No new module dependency in any module. `go mod tidy -diff` stays empty.
- One bad line or row is skipped and counted. It never stops an import.
- A session that the proxy captured live is never changed or deleted by an import.
- Each imported event has `Source: "imported:claude-code"`. Live events have `Source: ""`.
- The cost record of an imported turn has `Source: "transcript"` (new constant
  `event.SourceTranscript`). A turn with no usage has no cost record.
- Ledger rows of imported turns have `Row.Source: "transcript"`. Live rows omit it.
- Archive size headroom: an import stops before a session when
  `Stats().Bytes + session.SizeHint > MaxBytes * 9 / 10`.
- Run each module's commands with `GOWORK=off`. Give `gofmt` a path.
- Commits: `git commit -s`, and end the message with
  `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. No `Co-Authored-By`.
- Comments and docs: ASD-STE100 Simplified Technical English. Say "Cortex" for the
  product (see the root `CLAUDE.md` naming rule).

## Decisions made in this plan (with reasons)

1. **The Claude source builds events with the live parser.** For each turn it builds an
   Anthropic Messages request JSON and response JSON in memory, and calls
   `inference.ParseRequest("/v1/messages", nil, body)` and
   `inference.ParseResponse("/v1/messages", body)`. The bodies are not stored and no
   capture is written. Reason: the Timeline reads parsed fields, and the live parser is
   the one definition of those fields (`Messages`, `ToolResults`, `HistoryToolCalls`,
   thinking, counts). A second hand-written mapping would drift. The issue forbids fake
   *stored* bodies; this stores none.
2. **Each built event goes through one `session.Interner` per session, in order.**
   Reason: each turn repeats the whole conversation, so without interning one long
   session holds O(turns²) strings in memory during the import.
3. **`Session.Updated` is the latest request event's `At`.** The archive compares it with
   the archived summary's `LastRequestAt` (already in `SummaryFold`). Reason: both sides
   then come from the same events. The archive's `UpdatedAt` is the write time, which is
   wrong for this test. `--since` is first applied to the file mtime, before parse.
4. **No `Title` field on `Session`.** agentop already harvests Claude Code titles from
   the same transcripts at launch (`cmd/agentop/main.go:348`, `claude.Harvest`), keyed by
   the session id. The session fold also finds a title from the first prompt.
5. **Pricing host:** the Claude source returns `api.anthropic.com` from `PricingHost()`.
   The importer passes it to `settle.Input.Host` only. `SessionEvent.Host` stays empty,
   because the host is a proxy-only field.
6. **Agent attribution:** each imported event has
   `Client: &model.EventClient{Name: "claude-code", Version: <line "version">}` and
   `Project: project.Label(project.Root(cwd, home), home)`.
7. **The route writes the 200 header and flushes it before the work starts.**
   Reason: `apiclient.HeaderTimeout` is 2 minutes, and a large history can take longer.
   Per-agent errors go in the body. 400 and 409 are decided before the header.
8. **`--since` accepts `YYYY-MM-DD` (local midnight) and `<N>d`.** agentop converts it to
   RFC 3339 for the route.
9. **Race with a live session:** `Archive.WriteSession` checks again on the writer
   goroutine. It refuses (`ErrLive`) when the archive holds the id without the imported
   mark, or when `lastSeq[id] > 0` and the archive holds no imported session for the id
   (a live event was recorded before the write). It refuses (`ErrExists`) a plain write
   when the archive already holds the id.
10. **Two PRs.** The total estimate is about 3,100 counted lines. PR 1 is the core write
    path with a fake source in the tests. PR 2 adds the Claude source, the route, agentop
    and docs on top of it.
11. **LAST 1H does not show imported turns until the next proxy restart.** The spec
    accepts this. The importer does not feed the usage ring.
12. **`agentop import` brings an erased session back** (user request, 2026-11-16). An
    erase (`DELETE /v1/sessions/{id}`, `DELETE /v1/sessions`, agentop's `x`) removes the
    session from the store and the archive, and leaves no tombstone. So the next import
    finds no copy and writes the transcript as a new imported session (rule 4). The merged
    code already does this for the archive. Do NOT add a tombstone or an "erased" skip
    rule. What the merged code gets wrong is the cost: an erase keeps the ledger rows, and
    rule 4 sends every turn to the ledger again. The merged `toLedger` already skips a
    turn with a LIVE row (`Row.Source == ""`) near it, so an erased live session does not
    count two times. An erased IMPORTED session does (the `shortcut:` comment in `Run`).
    T11 fixes that: on rule 4 only (not on a replace), `toLedger` also skips a turn with a
    `transcript` row near it, except a row that this same `Run` call wrote. Reason: the
    user now uses erase-then-import as a workflow, so the double count shows up in
    `agentop cost`. Rows carry no session id; a session id on rows is a schema change to a
    durable file and is larger than this fix. The known limit (two imported sessions in
    one project in the same minute, imported by two separate runs, the second is
    undercounted) is the same class as the merged live-row check, and goes in a
    `shortcut:` comment.

## Review Focus

1. A transcript where Claude Code writes one assistant message as several lines (one per
   content block, same `message.id`). Expected: one turn, usage from the last line of the
   group. Test: `TestSource_SplitAssistantLinesAreOneTurn` (T6).
2. A transcript with a compaction (`type: "system"`, `subtype: "compact_boundary"`).
   Expected: the next turn's `Messages` starts again from the compact summary, not from
   the whole file. Test: `TestSource_CompactBoundaryResetsConversation` (T6).
3. A live event for the id lands while an import writes it. Expected: the import refuses
   that session and counts it as live. Test: `TestWriteSession_RefusesAfterLiveRecord` (T3).
4. The proxy restarts after an import. Expected: the imported mark survives in
   `session.json`, so rule 2 still skips. Test: `TestArchive_ImportedMarkSurvivesReopen` (T3).
5. A second `POST /v1/import` while one runs. Expected: 409, and the first one finishes.
   Test: `TestImport_SecondRequestIs409` (T7).
6. The user erases an imported session, then runs `agentop import` again. Expected: the
   session comes back, marked imported, and the ledger total does not change. Tests:
   `TestRun_ErasedImportedSessionComesBackOnce` (T11), `TestImport_ErasedSessionComesBack` (T7).

---

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| PR2 | `feat: Import Claude Code transcripts with agentop import` | "" | T6, T7, T8, T9, T10, T11 | 1890 |

**STATUS (2026-11-16): T1–T5 (PR1) are MERGED in `main` as #1876. Do not build them again.** Build only PR2 (T6–T11), on `main`. Where this plan and the merged code differ, follow the merged code. One difference: `importer.Run` sends ledger rows through `ledger.Writer.RecordBatch` (blocking), not `Record`.

## Tasks

### T1: Event `Source` field and the importer types (MERGED in #1876)

**Depends on:** none. **PR:** none: MERGED in #1876, do not build. **Est:** 110 lines.

**Files:**
- Modify: `core/model/session.go` (field on `SessionEvent`, on `sessionEventWire`, in
  `MarshalJSON` and `UnmarshalJSON`)
- Create: `core/session/importer/types.go`
- Test: `core/model/session_wire_test.go` (existing reflection tests cover the field
  lists), `core/model/session_source_test.go`

**Interfaces — Produces:**
```go
// core/model/session.go, on SessionEvent, after ParentSubagent:
// Source is "" on a live event, "imported:<agent>" on an event an import wrote.
Source string
// wire: Source string `json:"source,omitempty"`

// core/model: const ImportedSourcePrefix = "imported:"
func ImportedSource(agent string) string        // "imported:" + agent
func (e *SessionEvent) Imported() bool           // strings.HasPrefix(e.Source, ImportedSourcePrefix)

// core/session/importer/types.go
type Source interface {
    Agent() string       // "claude-code"
    PricingHost() string // host for settle.Input.Host, e.g. "api.anthropic.com"
    // Sessions yields newest first (by file mtime). since zero = all. A non-nil error
    // fails this agent only; Run stops and reports it.
    Sessions(since time.Time) iter.Seq2[Session, error]
}
type Session struct {
    ID       string               // the Cortex session id
    Updated  time.Time            // At of the latest request event
    SizeHint int64                // bytes of the files read, for the archive size check
    Events   []model.SessionEvent // request/response pairs in order; Seq unset
    Skipped  int                  // lines not recognized
}
type Request struct {           // POST /v1/import body
    Agent  string    `json:"agent,omitempty"`  // "" = every registered source
    Since  time.Time `json:"since,omitempty"`
    DryRun bool      `json:"dryRun,omitempty"`
}
type AgentReport struct {
    Agent          string `json:"agent"`
    Imported       int    `json:"imported"`
    Replaced       int    `json:"replaced"`
    SkippedLive    int    `json:"skippedLive"`
    SkippedCurrent int    `json:"skippedCurrent"`
    SkippedLines   int    `json:"skippedLines"`
    SkippedExpired int    `json:"skippedExpired,omitempty"` // older than the archive's retention
    NotImported    int    `json:"notImported,omitempty"` // left after an archive-full stop
    Stopped        string `json:"stopped,omitempty"`     // "archive full" or ""
    Error          string `json:"error,omitempty"`
}
type Report struct {
    DryRun bool          `json:"dryRun,omitempty"`
    Agents []AgentReport `json:"agents"`
}
```
`types.go` imports only `iter`, `time` and `core/model`. agentop imports the whole
`importer` package for `Request` and `Report`. That is accepted: agentop already links
`archive`, `ledger` and `settle` (checked with `go list -deps`), so no `go.mod` changes.

- [ ] **Step 1:** Write `TestSessionEvent_SourceRoundTrips`: an event with
  `Source: model.ImportedSource("claude-code")` marshals to `"source":"imported:claude-code"`
  and decodes back; an event with `Source: ""` has no `"source"` key; `Imported()` is
  true only for the first.
- [ ] **Step 2:** Run `GOWORK=off go -C core test ./model/ -run 'SessionEvent_Source|Wire'`. Expected: FAIL (no field).
- [ ] **Step 3:** Add the field, the wire field, both copy lines, the helpers, and `types.go`.
- [ ] **Step 4:** Run the same command. Expected: PASS. Run `GOWORK=off go -C core vet ./model/ ./session/importer/`.
- [ ] **Step 5:** Commit `feat(model): Add SessionEvent.Source and the importer types`.

### T2: The imported mark in the session summary (MERGED in #1876)

**Depends on:** none (T1–T5 are merged). **PR:** none: MERGED in #1876, do not build. **Est:** 170 lines.

**Files:**
- Modify: `core/session/fold.go` (`SummaryFold`, `Add`, `Summary`, `Clone`, `foldJSON`,
  `MarshalJSON`, `UnmarshalJSON`)
- Modify: `core/session/summary.go` (`SessionSummary.Source`)
- Test: `core/session/fold_imported_test.go`

**Interfaces — Produces:**
```go
// SessionSummary: Source is "imported:<agent>" while every event of the session is
// imported; "" otherwise.
Source string `json:"source,omitempty"`
```
Fold state: `imported string` (the first imported event's Source) and `live bool`
(true after any event with `Source == ""`). `Summary` sets `Source = f.imported` only
when `!f.live`. `foldJSON` gets `Imported string \`json:"imported,omitempty"\`` and
`Live bool \`json:"live,omitempty"\``. `Store.ListSessions` does not set `Source`: store
entries are live. `withPriorLocked` (`core/session/history.go:171`) copies named fields
only, so it does not copy `Source` onto a live store row. Do not change it.

- [ ] **Step 1:** Write tests:
  - `TestFold_AllImportedIsMarked`: two events with `Source "imported:claude-code"` →
    `Summary(id).Source == "imported:claude-code"`.
  - `TestFold_OneLiveEventClearsTheMark`: imported, imported, then one `Source ""` → `""`.
  - `TestFold_MarkSurvivesJSON`: marshal and unmarshal both fold states; `Summary` agrees.
  - `TestFold_CloneKeepsMark`.
- [ ] **Step 2:** Run `GOWORK=off go -C core test ./session/ -run Fold_`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `GOWORK=off go -C core test ./session/`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(session): Mark a session as imported while all its events are`.

### T3: Blocking whole-session write and lookup in the archive (MERGED in #1876)

**Depends on:** none (T1–T5 are merged). **PR:** none: MERGED in #1876, do not build. **Est:** 330 lines.

**Files:**
- Create: `core/session/archive/import.go`
- Test: `core/session/archive/import_test.go`

**Interfaces — Produces:**
```go
var ErrLive   = errors.New("session archive: the session has live events")
var ErrExists = errors.New("session archive: the session is already archived")

// Lookup returns the archived summary of id from the reader index.
func (a *Archive) Lookup(id string) (session.SessionSummary, bool)

// WriteSession writes events as the whole session id and returns when they are on disk
// (segment closed and fsynced, session.json written, bytes refreshed). It numbers Seq
// from 1 in order and sets SessionID. With replace, it first deletes the archived
// session (deleteSession) in the same writer op, so no live event lands between.
// Refusals, checked on the writer goroutine: ErrLive (see plan decision 9);
// ErrExists when !replace and the id is archived. A closed archive returns an error.
func (a *Archive) WriteSession(id string, events []model.SessionEvent, replace bool) error
```
Implementation (decided, do not change):
- Use the existing `opFunc` kind with a closure and a `done` channel, as `inWriter` does.
  Do not add an `opKind`.
- Before the send, return `errors.New("the session archive is closed")` when
  `a.closed.Load()`. Send with `select { case a.ops <- o: case <-a.done: return <closed error> }`
  and wait with `select { case <-o.done: case <-a.done: return <closed error> }`. A bare
  `a.ops <- o` blocks forever after `Close`, because the writer stops reading.
- In the closure, on the writer goroutine, take `a.mu` ONCE for the check and the
  reservation: read `a.lastSeq[id]` and the archived state, decide `ErrLive`/`ErrExists`
  (decision 9), and when the write may go on, set `a.lastSeq[id] = uint64(len(events))`
  before you release `a.mu`. Then a live `Record` that runs during the write numbers after
  the import (SeqSeeder), not from 1. When the disk write fails, put the old `lastSeq`
  value back and return the error.
- Replace: call `a.deleteSession(id, &pendingClear{done: make(chan struct{}), seqs: map[string]uint64{}})`
  first. Give it an EMPTY `seqs` map, so its `forgetSeqs` does not drop the `lastSeq`
  value reserved above. Return its error, if any, before the write.
- After the events: `closeWriter`, `writeMeta`, `publish`, `refreshBytes`.

- [ ] **Step 1:** Write tests (open the archive in `t.TempDir()` like `archive_test.go`):
  - `TestWriteSession_WritesAndNumbers`: 4 events → `Page` returns Seq 1..4,
    `Lookup` has `EventCount 4` and `Source "imported:claude-code"`.
  - `TestWriteSession_ReplaceDeletesFirst`: write 4, replace with 6 → 6 events, Seq 1..6.
  - `TestWriteSession_RefusesLiveSession`: `Record` one live event for the id, wait
    (`inWriter`), then `WriteSession(..., true)` → `errors.Is(err, ErrLive)`, and the
    live event is unchanged.
  - `TestWriteSession_RefusesAfterLiveRecord`: `Record` a live event for a NEW id and
    call `WriteSession(id, ..., false)` at once → `ErrLive` or `ErrExists`; never a second
    event with Seq 1.
  - `TestWriteSession_PlainWriteRefusesExisting` → `ErrExists`.
  - `TestWriteSession_LastSeqContinues`: after a write of 4, `LastSeq(id) == 4`.
  - `TestArchive_ImportedMarkSurvivesReopen`: write, `Close`, `Open` again → `Lookup(id).Source` is still imported.
  - `TestWriteSession_UpdatesBytes`: `Stats().Bytes` grows after the call returns.
- [ ] **Step 2:** Run `GOWORK=off go -C core test ./session/archive/ -run 'WriteSession|ImportedMark'`. Expected: FAIL.
- [ ] **Step 3:** Implement `import.go`.
- [ ] **Step 4:** Run `GOWORK=off go -C core test -race ./session/archive/`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(archive): Add a blocking whole-session write for imports`.

### T4: `transcript` cost source and `Row.Source` in the ledger (MERGED in #1876)

**Depends on:** none (T1–T5 are merged). **PR:** none: MERGED in #1876, do not build. **Est:** 140 lines.

**Files:**
- Modify: `core/cost/event/event.go` (const next to `SourceUsageFallback`; update the
  `Event.Source` doc comment to name three values)
- Modify: `core/cost/ledger/row.go` (`Row.Source`, `key.source`, `Row.key()`)
- Modify: `core/cost/ledger/writer.go` (`Record` sets `Row.Source`)
- Test: `core/cost/ledger/source_test.go`

**Interfaces — Produces:**
```go
const SourceTranscript = "transcript" // core/cost/event
// ledger.Row: Source string `json:"source,omitempty"`; "transcript" when the event is
// imported (e.Imported()), else "". Part of the row key.
```

- [ ] **Step 1:** Write tests (use the writer setup that `writer_test.go` uses):
  - `TestRecord_ImportedRowLandsInItsOwnDayWithSource`: an imported response event with a
    priced cost record and `At` 3 days ago → after `Flush`, the row is in that day's file
    with `Source "transcript"`.
  - `TestRecord_LiveRowHasNoSource`: the JSON row has no `"source"` key.
  - `TestRecord_SourceSplitsTheKey`: one live and one imported event in the same minute,
    same model → two rows; the query total sums both.
- [ ] **Step 2:** Run `GOWORK=off go -C core test ./cost/ledger/ -run 'Record_.*Source|ImportedRow'`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `GOWORK=off go -C core test ./cost/...`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(ledger): Tag imported cost rows with Source transcript`.

### T5: The importer run (MERGED in #1876)

**Depends on:** none (T1–T5 are merged). **PR:** none: MERGED in #1876, do not build. **Est:** 600 lines.

**Files:**
- Create: `core/session/importer/importer.go`, `core/session/importer/cost.go`
- Test: `core/session/importer/importer_test.go`, `core/session/importer/cost_test.go`

**Interfaces — Consumes:** T1 types, `Archive.Lookup`/`WriteSession`/`Stats`,
`SessionSummary.Source`/`LastRequestAt`, `event.SourceTranscript`, `ledger.Writer.Record`
and `RetentionCutoff()`, `settle.SettleInput`, `settle.NewRecord`.

**Interfaces — Produces:**
```go
type Options struct {
    Archive *archive.Archive  // required
    Store   *session.Store    // live store; may be nil in tests
    Ledger  *ledger.Writer    // nil: no ledger rows
    Rates   pricing.Resolver
    Since   time.Time
    DryRun  bool
}
// Run imports one source. It never panics on a session; ctx cancel stops between sessions.
func Run(ctx context.Context, src Source, o Options) AgentReport

// price adds the cost record to a response event in place: settle.SettleInput with
// Host = host, Path "/v1/messages", Inference = e.Inference; skip when the response has
// no token counts; NewRecord; set Source = event.SourceTranscript; store it in
// e.Plugins[event.Key] and e.Plugins[event.PluginName] (the ingest.go pattern).
func price(e *model.SessionEvent, host string, rates pricing.Resolver)
```
Rules in `Run`, for each session, in this order:
1. `Store.ViewTail(id, 1) != nil` (`Store` non-nil; `ViewTail` copies one event, `View`
   copies all of them), or `Lookup(id)` is found with `Source == ""` → `SkippedLive++`.
2. Found and imported, and `!s.Updated.After(sum.LastRequestAt)` → `SkippedCurrent++`.
2a. `s.Updated` is before now minus the archive's `retention_days` → `SkippedExpired++`.
   The next prune would delete it. Its turns do not reach the ledger.
3. Found and imported, and newer → replace. 4. Not found → import.
5. `len(s.Events) == 0` → nothing written, not counted as imported.
6. Before a write: when `Stats().Bytes + s.SizeHint > Stats().MaxBytes*9/10`, set
   `Stopped = "archive full"`, count this and every later session in `NotImported`, stop.
7. Write with `WriteSession(id, events, replace)`. `ErrLive`/`ErrExists` → `SkippedLive++`.
8. Ledger (not on dry run, and only when `Ledger != nil`): each response event whose
   paired request `At` is after the old `LastRequestAt` (all events on rule 4) and whose
   `At` is not before `Ledger.RetentionCutoff()` → `Ledger.Record(id, &e)`.
9. Dry run: apply rules 1–6 and 2a and count, but call neither `WriteSession` nor the ledger.
Set `Source = model.ImportedSource(src.Agent())` on every event before the write.
`SkippedLines` sums `s.Skipped`. An iterator error sets `Error` and ends the run.

- [ ] **Step 1:** Write tests with a `fakeSource` in the test file (fixed sessions, each a
  request/response pair with `Inference{Model: "claude-sonnet-4-5", InputTokens: 1000,
  CompletionTokens: 100, TotalTokens: 1100}`), a temp archive, a temp ledger
  (`ledger.New(t.TempDir())`) and a resolver from the bundled table:
  `tab, err := pricing.NewTable(pricing.Bundled()); rates := pricing.NewRegistry(tab)`
  (`pricing.NewRegistry(nil)` prices nothing):
  - `TestRun_ImportsNewSession` (rule 4): `Imported 1`; archive holds it, marked.
  - `TestRun_SkipsLiveInStore` (rule 1, store): `SkippedLive 1`, archive untouched.
  - `TestRun_SkipsLiveInArchive` (rule 1, archive without mark).
  - `TestRun_SkipsCurrent` (rule 2): second run → `SkippedCurrent 1`, archive bytes equal.
  - `TestRun_ReplacesNewer` (rule 3): the source grows by one turn → `Replaced 1`,
    archive has all turns.
  - `TestRun_LiveEventClearsMarkThenSkips`: import, `Record` one live event, re-run with a
    newer source → `SkippedLive 1`, the live event is still there.
  - `TestRun_StopsAtSizeLimit`: `archive.WithMaxBytes` small, 3 sessions →
    `Stopped "archive full"`, `NotImported >= 1`, `Stats().Bytes <= MaxBytes`.
  - `TestRun_DryRunWritesNothing`: counts as a real run; archive has no session; ledger
    has no rows.
  - `TestRun_LedgerGetsAllTurnsOnImport`, `TestRun_LedgerGetsOnlyNewTurnsOnReplace`
    (ledger row tokens equal the new turn only), `TestRun_LedgerSkipsTurnsPastCutoff`.
  - `TestRun_EmptySessionNotWritten`, `TestRun_SourceErrorFailsAgentOnly`.
  - `cost_test.go`: `TestPrice_TokensGetTranscriptRecord` (`event.Record` finds it,
    `Source "transcript"`, `Priced()` true), `TestPrice_NoUsageNoRecord`.
- [ ] **Step 2:** Run `GOWORK=off go -C core test ./session/importer/`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `GOWORK=off go -C core test -race ./session/importer/ && GOWORK=off go -C core vet ./...`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(importer): Add the shared transcript import run`.

### T6: The Claude Code source

**Depends on:** none (T1–T5 are merged). **PR:** PR2. **Est:** 750 lines (fixtures included).

**Files:**
- Create: `core/session/importer/claude/claude.go`, `core/session/importer/claude/turns.go`
- Create: `core/session/importer/claude/testdata/` fixtures (see Step 1)
- Test: `core/session/importer/claude/claude_test.go`,
  `core/session/importer/claude/timeline_test.go`

**Interfaces — Consumes:** T1 `importer.Source`/`Session`, `claude.DefaultConfigDir`,
`claude.ReadSessions(configDir, nil)` (id → `LogFile`, `Subagents` with `AgentType`),
`inference.ParseRequest`, `inference.ParseResponse`, `session.Interner.InternEvent`,
`project.Root`, `project.Label`.

**Interfaces — Produces:**
```go
package claude // import "github.com/rossoctl/cortex/core/session/importer/claude"
// New returns the source. configDir "" resolves claude.DefaultConfigDir() (honours
// CLAUDE_CONFIG_DIR) when Sessions runs.
func New(configDir string) importer.Source
// Agent() == "claude-code"; PricingHost() == "api.anthropic.com"
```
Rules for the parse (one transcript file = `<project>/<uuid>.jsonl`; id = file uuid):
- Do not take the harvest metadata lock. Do not write any file.
- This package is also named `claude`. Import `core/observe/claude` as `ccstore`.
- Discovery: call `ccstore.ReadSessions(dir, nil)`. A missing `projects` directory
  returns an empty map and no error: the source then yields nothing and no error, so
  `agentop import` with no `--agent` on a machine without Claude Code reports 0 sessions.
- Subagent transcripts: list `agent-*.jsonl` files with `os.ReadDir` in
  `<projDir>/<uuid>/subagents/` and in each `<projDir>/<uuid>/subagents/workflows/<run>/`
  (`projDir` = `filepath.Dir(LogFile)`). Do not use the `Subagents` map for this: it holds
  only agents that have a `.meta.json` file. The subagent id is the file name without
  `agent-` and `.jsonl` (the bare id, the same value live capture stores from
  `X-Claude-Code-Agent-Id`).
- Sort sessions by `LogModTime` newest first. Skip a session whose file mtime and every
  subagent file mtime are before `since`.
- Line `type` `user` or `assistant` is a message. `type` `system` with
  `subtype` `compact_boundary` resets the conversation (the next user line is the
  summary). Every other `type` (`summary`, `file-history-snapshot`, `system`, ...) is
  ignored and NOT counted. A line that is not JSON, or a `user`/`assistant` line with no
  `message`, is counted in `Skipped`. Lines with `isMeta: true` are kept in the
  conversation, as Claude Code sends them.
- Consecutive `assistant` lines with the same `message.id` are ONE turn: join their
  content blocks; take `message.usage` and `stop_reason` from the last line.
- For each turn, request body = `{"model": m, "messages": <conversation so far>}`, where
  the conversation is the user and assistant messages since the last reset, each as
  `{"role", "content"}`. Response body = `{"model", "content", "stop_reason", "usage"}`.
  Parse them with `inference.ParseRequest("/v1/messages", nil, b)` and
  `inference.ParseResponse("/v1/messages", b)`. Then set on the request's extension
  `AgentRole` = `model.AgentRoleMain` (main file) or `model.AgentRoleSubagent`.
- Request event: `Direction model.Outbound`, `Phase model.SessionRequest`,
  `At` = timestamp of the last user line before the turn. Response event:
  `Phase model.SessionResponse`, `At` = timestamp of the turn's last line. Both:
  `RequestID` = the line `requestId` (else `message.id`), `Client`, `Project` (decision 6).
  The importer sets `Source`, not the source.
- Subagent files `<uuid>/subagents/agent-<aid>.jsonl` and
  `<uuid>/subagents/workflows/<run>/agent-<aid>.jsonl`: parse them the same way, with
  `Subagent = "<aid>"` on each event. Merge all events of the session by `At` (stable:
  main file first on ties). Pass every event through one `session.Interner`, in the
  final order.
- `Updated` = the latest request `At`. `SizeHint` = the sum of the file sizes read.
- A file that cannot be opened: yield `(Session{}, err)` only when `ReadSessions` returns
  an error (the projects directory exists but cannot be read). One unreadable main
  transcript: do not yield that session, and log it with `slog.Warn` and its path. One
  unreadable subagent transcript: add 1 to that session's `Skipped`.

- [ ] **Step 1:** Write scrubbed fixtures under `testdata/projects/-tmp-demo/`, built
  from real Claude Code 2.x lines (no real content, paths or ids):
  `11111111-1111-4111-8111-111111111111.jsonl` (plain: 2 user prompts, 2 assistant
  turns, a `summary` line, a `file-history-snapshot` line, one non-JSON line),
  `22222222-2222-4222-8222-222222222222jsonl` (a `tool_use` turn split over 2 assistant lines with the same
  `message.id`, a `tool_result` user line, a `thinking` block, a `compact_boundary`), and
  `22222222-2222-4222-8222-222222222222/subagents/agent-a1b2c3.jsonl` + `agent-a1b2c3.meta.json`
  (`{"agentType":"Explore"}`).
- [ ] **Step 2:** Write tests:
  - `TestSource_PlainSession`: 2 turns → 4 events; `Skipped == 1`; models, `Completion`,
    `InputTokens`, `CacheReadTokens`, `CacheWriteTokens`, `CompletionTokens` from usage;
    `Client.Name == "claude-code"`.
  - `TestSource_ToolCallsAndResults`: the response has `ToolCalls[0].Name`; the next
    request has `ToolResults[0].ToolUseID` equal to its id.
  - `TestSource_SplitAssistantLinesAreOneTurn`.
  - `TestSource_CompactBoundaryResetsConversation`: the turn after the boundary has fewer
    `Messages` than the turn before.
  - `TestSource_SubagentEventsInRootSession`: subagent events have `Subagent "a1b2c3"`
    and `AgentRole subagent`; they are in session `22222222-2222-4222-8222-222222222222`.
  - `TestSource_NewestFirstAndSince`: order by mtime (set with `os.Chtimes`); `since`
    after one file's mtime drops it.
  - `TestSource_HonoursClaudeConfigDir`: `t.Setenv("CLAUDE_CONFIG_DIR", testdata)` and
    `New("")` find the sessions.
  - `timeline_test.go` `TestTimeline_ImportedSessionShowsTurnsToolsSubagents`: run
    `timeline.Build(id, events, now)` over session `22222222-2222-4222-8222-222222222222` (with Seq set 1..n) and
    check it has the main turns, the tool call by name, and a separate subagent row.
- [ ] **Step 3:** Run `GOWORK=off go -C core test ./session/importer/claude/`. Expected: FAIL.
- [ ] **Step 4:** Implement.
- [ ] **Step 5:** Run the same command with `-race`, then `GOWORK=off go -C core vet ./...`. Expected: PASS.
- [ ] **Step 6:** Commit `feat(importer): Add the Claude Code transcript source`.

### T7: `POST /v1/import` and the proxy wiring

**Depends on:** T6. **PR:** PR2. **Est:** 300 lines.

**Files:**
- Create: `core/sessionapi/import.go`
- Modify: `core/sessionapi/server.go` (option field, route registration in `New`, the
  route line in the index text near line 282)
- Modify: `cmd/cortex/main.go:1256-1275` (one option)
- Test: `core/sessionapi/import_test.go`

**Interfaces — Produces:**
```go
type Import struct {
    Rates   pricing.Resolver
    Sources []importer.Source
}
func WithImport(i Import) Option
```
- Register `POST /v1/import` only when `s.archive != nil && s.loopbackOnly &&
  len(s.importCfg.Sources) > 0`. Otherwise the route does not exist (404).
- Body is `importer.Request`, at most 4 KiB. Unknown `agent` → 400 with the list of
  supported agents. Bad JSON → 400.
- One import at a time: `sync.Mutex.TryLock`; fail → 409.
- Then `w.Header().Set("Content-Type", "application/json")`, `WriteHeader(200)`, flush
  (decision 7). Run `importer.Run` per selected source with `Archive: s.archive,
  Store: s.store, Ledger: s.ledger, Rates`, and write one `importer.Report`.
- `cmd/cortex/main.go`: add
  `sessionapi.WithImport(sessionapi.Import{Rates: pricingRegistry, Sources: []importer.Source{claudeimport.New("")}})`
  to the options list.

- [ ] **Step 1:** Write tests (store + temp archive + fake or fixture source):
  - `TestImport_RouteAbsentWithoutArchive`, `TestImport_RouteAbsentWhenNotLoopbackOnly`:
    `POST /v1/import` → 404.
  - `TestImport_RunsAndReports`: 200, `Report.Agents[0].Imported == 1`; then
    `GET /v1/sessions?archived=true` lists the id with `source "imported:claude-code"`.
  - `TestImport_UnknownAgentIs400`, `TestImport_PathFieldIgnored` (a body with an extra
    `"path"` key reads no other directory).
  - `TestImport_SecondRequestIs409`: a source that blocks on a channel; the second request
    gets 409; release; the first returns 200.
  - `TestImport_ErasedSessionComesBack` (decision 12): import, then
    `DELETE /v1/sessions/{id}` (with the clear guards satisfied: `WithClearAllowed(true)`,
    a loopback `Host`, no `Origin`), then import again → the second report has
    `Imported == 1`, and `GET /v1/sessions?archived=true` lists the id with
    `source "imported:claude-code"`.
- [ ] **Step 2:** Run `GOWORK=off go -C core test ./sessionapi/ -run Import_`. Expected: FAIL.
- [ ] **Step 3:** Implement and wire `cmd/cortex`.
- [ ] **Step 4:** Run `GOWORK=off go -C core test ./sessionapi/ && GOWORK=off go -C cmd/cortex vet ./... && GOWORK=off go -C cmd/cortex build ./...`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(sessionapi): Add POST /v1/import`.

### T8: `agentop import`

**Depends on:** none (T1–T5 are merged). **PR:** PR2. **Est:** 330 lines.

**Files:**
- Create: `cmd/agentop/cmd_import.go`, `cmd/agentop/apiclient/import.go`
- Modify: `cmd/agentop/main.go` (dispatch `case "import"`, and the top-level help text
  that lists subcommands)
- Test: `cmd/agentop/cmd_import_test.go`, `cmd/agentop/apiclient/import_test.go`

**Interfaces — Produces:**
```go
func (c *Client) Import(ctx context.Context, req importer.Request) (importer.Report, error)
func runImport(args []string, stdout, stderr io.Writer) int
// apiclient/import.go:
var ErrImportRunning = errors.New("an import is already running") // 409
// 404 returns the existing ErrNotFound; 400 returns an error that wraps the existing
// ErrBadRequest and holds the response body text.
```
- `Client.Import` posts JSON with `http.NewRequestWithContext` and the client's
  `c.http` transport, the way `clear.go`'s `sendDelete` sends its request. It does not
  use `getJSON`, which sets a short default deadline.
- `cmd/agentop/main.go`: add `case "import": os.Exit(runImport(os.Args[2:], os.Stdout, os.Stderr))`
  next to `case "cost":` (line 138).
- Flags: `--agent` (`claude-code|bob|codex|opencode|pi`; the proxy answers 400 for an
  agent it has no source for), `--since` (decision 8), `--dry-run`, `--endpoint` (as
  `agentop cost`: default `localSessionEndpoint()`, same error lines).
- `context.WithTimeout` of 1 hour. While it waits, print a spinner to stderr only when
  stderr is a terminal.
- Output, one line per agent:
  `claude-code: 12 imported, 3 replaced, 40 skipped (live), 7 skipped (current), 1 skipped (expired), 2 lines not recognized`
  plus `  stopped: archive full, N sessions not imported` and `  error: ...` lines when
  set. `--dry-run` prefixes `dry run: `. Exit 1 when any agent has `Error`; 409 prints
  `agentop import: an import is already running` and exits 1; 404 prints that the proxy
  has no import route (archive off or not loopback-only) and exits 1.

- [ ] **Step 1:** Write tests with `httptest.Server`:
  - `TestClientImport_PostsBodyAndDecodes`: method POST, path `/v1/import`, body JSON
    equals the request.
  - `TestRunImport_PrintsOneLinePerAgent`, `TestRunImport_DryRunPrefix`,
    `TestRunImport_409`, `TestRunImport_404`, `TestRunImport_SinceForms`
    (`2026-10-01` → local midnight; `30d` → now − 720 h, within a second; `bad` → exit 2).
- [ ] **Step 2:** Run `GOWORK=off go -C cmd/agentop test -short -run 'Import' . ./apiclient/`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command. Expected: PASS.
- [ ] **Step 5:** Commit `feat(agentop): Add agentop import`.

### T9: Imported rows in the agentop TUI

**Depends on:** none (T1–T5 are merged). **PR:** PR2. **Est:** 260 lines.

**Files:**
- Modify: `cmd/agentop/tui/sessions_pane.go` (the sessions loop near line 302, which has
  the summary `s` in hand: when `strings.HasPrefix(s.Source, model.ImportedSourcePrefix)`,
  the title cell is a dim `imp ` followed by `m.sessionTitleCell(s.ID, s.Title, titleW-4)`.
  `sessionTitleCell` gets only an id, so do not change its signature. The no-limit
  search closure gets the same `imp ` prefix.)
- Modify: `cmd/agentop/tui/events_columns.go` (`colStatus`, `colDuration`, `colBytes`,
  `colHost`, `colPlugin` render `n/a` when `c.row.event.Imported()`; also in
  `toolCells` near line 867 for the same columns)
- Modify: `cmd/agentop/tui/detail_pane.go` (`timingHeader` near line 490: return
  `timing: n/a` when `e.Imported()`, before the `Ttfb` check)
- Modify: `cmd/agentop/tui/detail_raw.go` (`withRaw` gets the event as a third argument;
  its one caller is `detail_pane.go:111`. When `raw == nil` and `e.Imported()`, set the
  `"raw"` node to the string `not captured: imported from <agent>`, with
  `<agent> = strings.TrimPrefix(e.Source, model.ImportedSourcePrefix)`.) Do NOT change
  `detail_fetch.go`: the full-event fetch must still run, because the list projection
  drops the parsed fields (`Messages`, `ToolResults`) that the detail pane shows. The
  `?raw=1` part of that fetch returns no capture for an imported event, which is correct.
- Test: `cmd/agentop/tui/imported_test.go`

- [ ] **Step 1:** Write tests: `TestSessionTitleCell_ImportedPrefix`,
  `TestEventCells_ImportedShowNA` (each of the 5 columns renders `n/a`; tokens and cost
  render values), `TestDetailTiming_ImportedNA`, `TestRawView_ImportedNotCaptured`.
- [ ] **Step 2:** Run `GOWORK=off go -C cmd/agentop test ./tui/ -run 'Imported'`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `GOWORK=off go -C cmd/agentop test ./tui/`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(agentop): Mark imported sessions and show n/a for proxy-only fields`.

### T10: Docs

**Depends on:** none. **PR:** PR2. **Est:** 110 lines.

**Files:**
- Modify: `docs/session-api.md` (a row in the endpoint table near line 33, and a section
  `## POST /v1/import — transcript import (laptop only)`; the `source` event field and
  the summary `source` field in the event schema)
- Modify: `cmd/agentop/README.md` (the `agentop import` command)
- Modify: `docs/laptop-service.md` (one paragraph: how to import history)

Facts to write (all from this plan): the command line and its 4 flags; `--since`
accepts `YYYY-MM-DD` and `<N>d`; the route exists only when the archive is on and the
session API is loopback-only, else 404; body `{"agent","since","dryRun"}`; 409 while one
runs; the per-agent counts; the 4 rules of "Live sessions and re-import" (T5); the
Claude Code store `~/.claude/projects/<project>/<uuid>.jsonl` and `CLAUDE_CONFIG_DIR`;
imported events have `source: "imported:<agent>"`; the summary is marked only while all
its events are imported; cost `source: "transcript"` priced at today's rates; ledger
rows `source: "transcript"`; turns before the ledger cutoff reach the archive only; LAST
1H shows imported turns only after a proxy restart; the import stops at 90% of
`max_bytes`; an imported segment is kept `retention_days` from its newest event, and a session older
than that is skipped (`skippedExpired`);
only Claude Code is supported now (#1872–#1875 add the others); an erased session
(`DELETE /v1/sessions/{id}`, `DELETE /v1/sessions`, or agentop's erase) comes back on the
next `agentop import` while its transcript is still on disk, as an imported session; its
cost is not counted again, because an erase keeps the ledger rows (decision 12). Put the
erase fact in `docs/session-api.md` next to the two `DELETE` routes too.

- [ ] **Step 1:** Write the docs in ASD-STE100 style.
- [ ] **Step 2:** Run `pre-commit run --files docs/session-api.md cmd/agentop/README.md docs/laptop-service.md`. Expected: pass.
- [ ] **Step 3:** Commit `docs: Describe agentop import and POST /v1/import`.

### T11: An erased imported session comes back without a second cost

**Depends on:** none (changes only merged code). **PR:** PR2. **Est:** 140 lines.

**Files:**
- Modify: `core/session/importer/importer.go` (`Run`, `toLedger`, the two `shortcut:`
  comments about erase)
- Test: `core/session/importer/importer_test.go`

**What to change (decision 12):**
- `Run` keeps one `written := map[liveKey]bool{}` for the whole call. `toLedger` gets it
  and a `newSession bool` (`!found`) as new arguments.
- In `toLedger`, a row with `r.Source == ""` marks its key as before. When `newSession`
  is true, a row with `r.Source == event.SourceTranscript` also marks its key, unless
  `written[key]` is true. When `newSession` is false (a replace), transcript rows mark
  nothing: `after` already drops the turns of the earlier import.
- After `RecordBatch(keep)` returns nil, add the key of each kept turn to `written`
  (the same minute and `Project` form that the check uses). Reason: two new sessions in
  one project and one minute in the same run must both reach the ledger.
- Replace the `Run` comment "shortcut: the ledger keeps an imported session's rows when
  the archive loses the session ..." with: an erased or pruned imported session is new
  again; its turns are skipped when the ledger has a transcript row near them that an
  earlier run wrote. Add `shortcut: rows carry no session id, so a new session's turn is
  skipped when another session in the same project was imported by an earlier run in
  that minute. Keep the session id on ledger rows if that undercount shows up.`
- Do not change the archive, the store, the erase routes or the ledger.

- [ ] **Step 1:** Write tests with the existing helpers in `importer_test.go` (temp
  archive, `ledger.New(t.TempDir())`, the bundled resolver, `fakeSource`):
  - `TestRun_ErasedImportedSessionComesBackOnce`: `Run` imports session A (2 turns); flush
    the ledger and sum the tokens and `CostMicros` of `Window`; `AwaitDelete(A, time.Second)`
    on the archive; `Run` again with the same source → `Imported == 1`, `Lookup(A)` is
    found with `Source "imported:claude-code"`, and the ledger sums are unchanged.
  - `TestRun_ErasedLiveSessionComesBack`: `Record` one live event for A in the archive
    (no store), wait for it, `AwaitDelete(A, ...)`, `Run` → `Imported == 1`, `Lookup(A)` is
    imported.
  - `TestRun_TwoNewSessionsInOneMinuteBothCount`: sessions A and B, same project, turns in
    the same minute, one `Run` → the ledger has the tokens of both.
  - `TestRun_ReplaceIgnoresOwnTranscriptRows`: the merged
    `TestRun_LedgerGetsOnlyNewTurnsOnReplace` still passes; add one case where the new
    turn is in the same minute as the last old turn → it still reaches the ledger.
- [ ] **Step 2:** Run `GOWORK=off go -C core test ./session/importer/ -run 'Erased|TwoNewSessions|ReplaceIgnores'`. Expected: FAIL for `ErasedImportedSessionComesBackOnce` (double count) and the new-session minute case may PASS already; that is accepted.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `GOWORK=off go -C core test -race ./session/importer/`. Expected: PASS.
- [ ] **Step 5:** Commit `fix(importer): Do not count an erased imported session's cost two times`.

## Gate

From the worktree root:

```sh
test -z "$(gofmt -l core cmd/cortex cmd/agentop)" \
 && GOWORK=off go -C core vet ./... \
 && GOWORK=off go -C core test -race ./model/... ./session/... ./cost/... ./sessionapi/... ./observe/... \
 && GOWORK=off go -C core mod tidy -diff \
 && GOWORK=off go -C cmd/cortex vet ./... && GOWORK=off go -C cmd/cortex build ./... \
 && GOWORK=off go -C cmd/agentop vet ./... \
 && scripts/dev/go-test-shards.sh -short cmd/agentop \
 && GOWORK=off go -C cmd/agentop test ./apiclient/... ./tui/...
```

Checked on the base commit c50de9be, from the worktree root: the `gofmt` step and the
three `go vet` steps pass as written. No step was changed.

## For a human after review

Needs T1–T11 merged (both PRs) and a laptop install of that build.

1. `agentop import --agent claude-code --dry-run` against a real `~/.claude`. Check the
   counts look right.
2. `agentop import --agent claude-code`. Then `agentop observe`: check imported rows have
   the `imp` mark, a title, project, model, tokens and cost.
3. Open a few old sessions in the Timeline. Check turns, tool calls and subagents.
4. Run `agentop import` again. Check `0 imported`.
5. `agentop cost --window month`. Check the total includes the imported cost.
6. Check the T6 fixture lines against a current Claude Code transcript. If the line
   shapes changed, open a follow-up in this repo's tracker.
7. In `agentop observe`, erase one imported session (`x`). Run `agentop import` again.
   Check the session comes back, marked `imp`, and `agentop cost --window month` shows the
   same total as in step 5.
