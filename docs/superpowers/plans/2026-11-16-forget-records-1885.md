# Forget Records for Removed Sessions (#1885) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When the archive forgets a session (`x`, clear, day limit, size limit), it keeps a forget record. `agentop import` then brings the session back and does not count its cost again.

**Architecture:** The archive writes one JSON line per forgotten session to `<archive root>/forgotten.jsonl` and keeps a map `id -> {from, to}` in memory. `importer.Run` asks `Archive.Forgotten(id)` for a session that the archive does not hold, and sends to the ledger only the turns outside that span. The `written` guess from #1886 goes. The live-row guess stays for sessions with no record.

**Tech Stack:** Go 1.26.10, module `core` (`core/session/archive`, `core/session/importer`), module `cmd/cortex`.

**Spec:** issue #1885 (`gh api repos/rossoctl/cortex/issues/1885 --jq .body`). Background: plan `docs/superpowers/plans/2026-11-15-import-claude-code-transcripts-1871.md`, PR #1886.

## Global Constraints

- Record line format, exactly: `{"id":"<session id>","from":<unix ms>,"to":<unix ms>}`.
- `from` = `sessionMeta.CreatedAt`. `to` = `SessionSummary.LastRequestAt`. Both as Unix milliseconds.
- Write a record only when the ledger retention is > 0, `LastRequestAt` is not zero, and the summary has `TotalTokens > 0 || CostMicros > 0`.
- File: `<archive root>/forgotten.jsonl` (next to `data/`, not inside it). Mode `fileMode`. A clear does not touch it.
- Write and fsync the record BEFORE the session leaves the disk.
- `WriteSession`'s own delete (replace, and the `fail` path) writes no record.
- A record expires when `to` is before `expireBefore`, where `y, m, d := now.Date(); expireBefore := time.Date(y, m, d, 0, 0, 0, 0, now.Location()).AddDate(0, 0, -a.ledgerDays)` and `now = a.now()`. The archive package has no midnight helper; write this inline in `compactForgotten`. (Ledger cutoff is `midnight(now) - (ledgerDays-1) days`. The extra day covers whole-day files.)
- Do not change the cost ledger, the session API, agentop, or the import report.
- Comments, commit messages: ASD-STE100 plain English. Commits: `git commit -s`, trailer `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`.

## Decisions made for this plan

1. **CreatedAt check (the issue asks for it first): done.** `prune` (`archive.go` `prune`) removes segments and edits only `meta.Segments`. Nothing but `write` (new session), `renameEntry` (rekey) and `writeSession` (import) sets `CreatedAt`. So `from` = `meta.CreatedAt` is the first time the session was archived. No extra field.
2. **A failed record write does not stop the remove.** The record goes into the in-memory map first, so this process still uses it. The archive logs a WARN and counts `Stats.ForgetWriteErrors`. Reason: the size limit and a clear are how disk space comes back after ENOSPC. A remove that waits for a disk write would keep the disk full. The cost: after a restart that record is lost, and a later import can count that session again.
3. **Where the hook goes.** `take(opDelete)` calls a new `a.forget(...)` for the session and then `deleteSession`. `writeSession` calls `deleteSession` directly, so it writes no record. `clearAll` and `prune` call `forget` with all their sessions in one batch (one write, one fsync), before the rename or the `RemoveAll`.
4. **One-minute slack before `from`.** The importer drops a turn with request time in `[from - 1 minute, to]`. Reason: Claude Code writes a transcript time before the proxy sees the request, so the first live turn can have a transcript time a little before `from`. The cost: a turn from before Cortex in the minute before the first live event is not counted. No slack after `to`: for a live session the transcript time of each turn is before the proxy's time, so it is inside the span.
5. **With a record, the importer does no ledger read** (`w.Window`). The record replaces the guess for that session.
6. **The transcript-row guess goes completely** (the `written` map, the `newSession && !written[k]` branch, the `SourceTranscript` row handling). An imported session that was erased after #1886 and before this change has no record and can count again. #1886 merged one day ago, so this case is very small.
7. **The live-row guess stays** (`r.Source == ""` rows near a turn), for sessions without a record. Mark it with `// shortcut: the guess is for sessions forgotten before forget records; 32 days after an upgrade it has nothing to do. Remove it in a later release.`
8. **Map type** `map[string][2]int64` (from, to in ms), guarded by a new `fgMu sync.RWMutex`. `Forgotten` runs on the importer goroutine. The writer goroutine changes the map.
9. **Compaction:** at `Open` (after `prune`), and on `tick` when `now - lastCompact >= 24h`. Drop expired entries from the map. Rewrite the file when dead lines (expired, duplicate ids, lines that do not parse) are at least half of its lines: temp file `.forgotten.jsonl.tmp`, fsync, rename, `syncDir(root)`.
10. **Records survive a reopen.** `Open` reads the file BEFORE its `prune`, so the prune at `Open` merges into loaded records.
11. **The `skippedExpired` rule already covers the day limit** in most cases, because the importer skips a transcript older than the archive retention. The records cover the day limit too (for a transcript that is newer than its archived segments).
12. **The test helper `openArchiveAt` in `core/session/importer/importer_test.go` (which `openArchive` calls) passes `archive.WithLedgerRetentionDays(31)` before the caller's options**, so every importer test runs with records on.
13. One PR. The total estimate is about 1,000 counted lines.

## Review Focus

1. A replace import (`WriteSession(..., true)`) must leave no record. Test: `TestWriteSession_ReplaceWritesNoRecord` (T1).
2. Open with a `forgotten.jsonl` that ends in a torn line (crash during append). Expected: the good lines load, the torn line is counted, `Open` succeeds. Test: `TestForgotten_CorruptLineSkippedAndCounted` (T1) uses a torn last line.
3. Two removes of one id (live again after the first remove). Expected: one map entry, smallest `from`, largest `to`. Test: `TestForgotten_SecondRecordMerges` (T1).
4. A clear with the ledger off writes no file. Test: `TestForgotten_LedgerOffWritesNone` (T1).
5. A record for a session that is still in the archive (crash between record and remove). Expected: the import follows the archive (skip live or replace). Test: `TestRun_RecordWithSessionOnDiskFollowsArchive` (T2).

---

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: Keep forget records so an import does not count a removed session again` | "" | T1, T2, T3, T4 | 1000 |

---

### Task T1: Forget records in the archive

Depends on: none. estLines: 560.

**Files:**
- Create: `core/session/archive/forgotten.go`
- Create: `core/session/archive/forgotten_test.go`
- Modify: `core/session/archive/archive.go` (new option, `Archive` fields, `Stats` fields, `Open`, `take`, `tick`, `prune`)
- Modify: `core/session/archive/clear.go` (`clearAll`)

**Interfaces:**
- Produces:
  - `func WithLedgerRetentionDays(n int) Option` — 0 (the default) means no ledger: no records are written. Values over 3650 are clamped to 3650, and negative values are 0. Use a package constant `maxLedgerDays = 3650` with the comment `// ledger.MaxRetentionDays; the archive does not import the ledger.` Do not import `core/cost/ledger` from the archive.
  - `func (a *Archive) Forgotten(id string) (from, to time.Time, ok bool)` — reads the in-memory map under `fgMu.RLock`.
  - `Stats.ForgetRecords int` (entries in the map), `Stats.BadForgetLines uint64` (lines skipped at load), `Stats.ForgetWriteErrors uint64`.
  - Internal: `forgetFile = "forgotten.jsonl"`, `type forgetRecord struct { ID string \`json:"id"\`; From int64 \`json:"from"\`; To int64 \`json:"to"\` }`, `func (a *Archive) forget(ss []*sessionState)` (writer goroutine only), `func (a *Archive) loadForgotten()`, `func (a *Archive) compactForgotten(now time.Time)`.

- [ ] **Step 1: Write the failing tests** in `forgotten_test.go`. Use the package's existing fixtures (`fixture_test.go`) to record a session with a response event that has `Inference.TotalTokens > 0`. Open archives with `WithLedgerRetentionDays(31)` unless the test says otherwise. Read the file with a helper `readForgotten(t, root) []forgetRecord`.
  - `TestForgotten_DeleteWritesRecord`: record a session, `Deleted(id)`, `AwaitDelete`. One line. `ID == id`, `From == meta CreatedAt` in ms (take it from `Lookup(id).CreatedAt` before the delete), `To == Lookup(id).LastRequestAt` in ms. `Forgotten(id)` returns the same times and `ok`.
  - `TestForgotten_ClearWritesOnePerSession`: 3 sessions with tokens, `Cleared()`, `AwaitClear`. 3 lines, one per id. The file still exists after the clear, and `data/` is empty.
  - `TestForgotten_DayLimitWritesRecord`: record a session, `Close`, reopen the same root with `WithClock` 31 days in the future (default archive retention is 30). The session is gone and the record exists. Open both archives with `WithLedgerRetentionDays(40)`, so that the compaction at the second `Open` (clock + 31 days) does not expire the new record.
  - `TestForgotten_SizeLimitWritesRecord`: record a session, `Close`, reopen the same root with `WithMaxBytes(1)` and the same clock. The session is gone and the record exists. Open both archives with `WithLedgerRetentionDays(31)`.
  - `TestForgotten_NoInferenceWritesNone`: a session with only events that carry no tokens and no cost. Delete it. No file, or an empty file. `Forgotten` is not `ok`.
  - `TestWriteSession_ReplaceWritesNoRecord`: `WriteSession(id, evs, false)`, then `WriteSession(id, evs2, true)`. No record.
  - `TestForgotten_LedgerOffWritesNone`: no `WithLedgerRetentionDays`. Delete and clear sessions with tokens. No file.
  - `TestForgotten_SecondRecordMerges`: write a file by hand with two lines for one id (`from` 100/50, `to` 200/300). Open. `Forgotten` gives from=50 ms, to=300 ms. `Stats().ForgetRecords == 1`.
  - `TestForgotten_CorruptLineSkippedAndCounted`: file with one good line, one line `not json`, and a torn last line `{"id":"x","fr`. Open succeeds. `Stats().BadForgetLines == 2`. The good record is `ok`.
  - `TestForgotten_ExpireAndCompact`: `WithLedgerRetentionDays(3)`, fixed `WithClock(now)`. Write a file by hand with 3 expired records (`to` = now − 10 days) and 1 live record (`to` = now − 1 hour). Open. The live record is `ok`, the expired ones are not. The file holds exactly 1 line (3 of 4 dead ≥ half, so rewritten).
  - `TestForgotten_SurvivesReopen`: delete a session, `Close`, reopen. `Forgotten` is `ok`.

- [ ] **Step 2: Run the tests and check that they fail**

Run: `GOWORK=off go -C core test ./session/archive/ -run 'Forgotten|ReplaceWritesNoRecord'`
Expected: FAIL (build errors: `WithLedgerRetentionDays`, `Forgotten` undefined).

- [ ] **Step 3: Implement**
  - `forgotten.go`: `loadForgotten` reads the file line by line (`bufio.Scanner` with a 64 KiB buffer is enough: an id is ≤ 45 characters). A line that does not unmarshal, or has an empty `id`, increments `badForgetLines`. Merge rule: min `from`, max `to`. Count all lines in `forgetLines` (writer-only int) for compaction.
  - `forget(ss)`: return at once when `ledgerDays == 0`. For each session, compute `sum := s.meta.Summary.Summary(s.meta.ID)`. Skip it unless `!sum.LastRequestAt.IsZero() && (sum.TotalTokens > 0 || sum.CostMicros > 0)`. Merge each record into the map first. Then open with `O_WRONLY|O_CREATE|O_APPEND`, write all lines, `Sync`, `Close`. If the file was new, `syncDir(a.root)`. On any error: `slog.Warn` and `forgetWriteErrors.Add(1)`, then return (Decision 2).
  - `compactForgotten(now)`: compute `expireBefore` as the Global Constraints say. Drop map entries with `to` before it. Then `dead := forgetLines - len(map)` (expired, duplicate and bad lines). When `dead*2 >= forgetLines` and `forgetLines > 0`, rewrite the file (Decision 9) from the map and set `forgetLines = len(map)`. With `ledgerDays == 0`, do nothing.
  - `archive.go`:
    - In `Open`: after `removeTrash`, call `loadForgotten()`. After `a.prune(now)`, call `compactForgotten(now)` and set `lastCompact = now`.
    - In `take`: `case opDelete:` — `if s := a.sessions[o.id]; s != nil { a.forget([]*sessionState{s}) }`, then `deleteSession`.
    - In `prune`: collect the sessions that the final loop removes (`s.w == nil && len(s.meta.Segments) == 0`), call `forget` with them once, then do the `RemoveAll` loop that exists now.
    - In `tick`: `if now.Sub(a.lastCompact) >= 24*time.Hour { a.compactForgotten(now); a.lastCompact = now }`.
    - In `Stats`: fill the three new fields (`ForgetRecords` under `fgMu.RLock`).
  - `clear.go` `clearAll`: after the `closeWriter` loop and before `os.Rename(a.data, trash)`, call `a.forget(<all sessions>)`.

- [ ] **Step 4: Run the tests and check that they pass**

Run: `GOWORK=off go -C core test -race ./session/archive/`
Expected: PASS (new and old tests).

- [ ] **Step 5: Commit** — `feat: Write a forget record when the archive removes a session`.

---

### Task T2: The importer uses the forget records

Depends on: T1. estLines: 330.

**Files:**
- Modify: `core/session/importer/importer.go` (`Run`, `toLedger`, doc comments)
- Modify: `core/session/importer/importer_test.go`

**Interfaces:**
- Consumes: `archive.WithLedgerRetentionDays(n int) Option`, `(*archive.Archive).Forgotten(id string) (from, to time.Time, ok bool)` from T1.
- Produces: `func toLedger(ctx context.Context, w *ledger.Writer, s Session, prior *priorTurns, sent map[string]bool, span *forgotSpan) error` with `type forgotSpan struct{ from, to time.Time }`. Package-private.

- [ ] **Step 1: Change the tests**
  - `openArchiveAt`: prepend `archive.WithLedgerRetentionDays(31)` to `opts` (Decision 12).
  - Delete `TestRun_ErasedImportedSessionComesBackOnce`. Add `TestRun_ErasedImportedSessionComesBackThroughRecord` with the same assertions (Imported 1 twice, `Source == "imported:claude-code"`, `sumCost` unchanged), plus `_, _, ok := a.Forgotten("A"); ok` after the erase.
  - Keep `TestRun_TwoNewSessionsInOneMinuteBothCount`, `TestRun_ReplaceIgnoresOwnTranscriptRows`, `TestRun_CancelAfterTheArchiveWriteCountsOnce` and `TestRun_ReplaceDoesNotHideAnErasedSessionsRow` unchanged. They must pass through the record now.
  - `TestRun_ErasedLiveSessionWithEarlierTurns`: open the archive with `archive.WithClock(func() time.Time { return start.Add(10 * time.Minute) })`. Reason: `write` sets a new session's `CreatedAt` from `a.now()`, not from the event time, so without the clock `from` is the real now and every transcript turn is before it. Record a live session "A" in the archive with 2 request/response pairs at `start+10m` and `start+11m` (response with `Inference.TotalTokens: 1100`). Wait for `LastSeq`. Erase. Import `fakeSession("A", start, 12)` (turns at `start .. start+11m`). Expected: `Imported == 1`, and the ledger gets exactly the turns with request time before `start+9m` (Decision 4): 9 requests, 9900 tokens.
  - `TestRun_ErasedThenGrownCountsOnlyNewTurns`: import `fakeSession("A", start, 2)`, erase, import `fakeSession("A", start, 5)`. The ledger has 5 requests total (2 + 3 new), not 7.
  - `TestRun_SizeLimitPruneThenImportCountsOnce`: import into an archive at dir D, `Close`. Reopen D with `WithMaxBytes(1)` (removes the session, writes the record), `Close`. Reopen D with default bytes, import again with the same ledger. `Imported == 1`, ledger unchanged.
  - `TestRun_RecordWithSessionOnDiskFollowsArchive`: import `fakeSession("A", start, 2)`, `Close`. Append a record line for "A" to `D/forgotten.jsonl` by hand (span covering all turns). Reopen. Import `fakeSession("A", start, 3)`. Expected: `Replaced == 1`, and the ledger gains exactly 1 request (the new turn). The record does not hide it.
  - `TestRun_NoRecordKeepsLiveGuess`: keep `TestRun_LedgerSkipsTurnsRecordedLive` as it is. It is that test.

- [ ] **Step 2: Run and check that the new tests fail**

Run: `GOWORK=off go -C core test ./session/importer/ -run 'Erased|Record|SizeLimit'`
Expected: FAIL on `TestRun_ErasedLiveSessionWithEarlierTurns` and `TestRun_RecordWithSessionOnDiskFollowsArchive` (or build failure before T1 is in).

- [ ] **Step 3: Implement**
  - In `Run`: remove `written`. Before `toLedger`, when `!found`: `if f, t, ok := o.Archive.Forgotten(s.ID); ok { span = &forgotSpan{f, t} }`.
  - In `toLedger`: in the turn loop, when `span != nil`, skip a turn whose request time `at` is in `[span.from.Add(-time.Minute), span.to]`. When `span != nil`, return after `RecordBatch` with no `w.Window` read (Decision 5). Without a span, keep only the live-row check (`r.Source == ""`). Remove the `SourceTranscript` branch and every use of `written` (Decision 6). Add the `shortcut:` comment of Decision 7.
  - Rewrite the doc comments of `Run` and `toLedger` to describe the record rule. Remove the `written` shortcut comment.

- [ ] **Step 4: Run and check that all pass**

Run: `GOWORK=off go -C core test -race ./session/importer/... ./sessionapi/...`
Expected: PASS. `core/sessionapi` `TestImport_ErasedSessionComesBack` passes with no change.

- [ ] **Step 5: Commit** — `feat: Count a removed session's cost once, from its forget record`.

---

### Task T3: cmd/cortex passes the ledger retention

Depends on: T1. estLines: 60.

**Files:**
- Modify: `cmd/cortex/archive.go` (`openSessionArchive`)
- Modify: `cmd/cortex/main.go:797` (the one call)
- Modify: `cmd/cortex/archive_test.go` (4 calls, one new test)

**Interfaces:**
- Consumes: `archive.WithLedgerRetentionDays(n int) Option`, `Stats.ForgetRecords` from T1.
- Produces: `func openSessionArchive(cfg *config.Config, configPath string, sessions *session.Store, ledgerDays int) *archive.Archive`.

- [ ] **Step 1: Write the failing test** `TestOpenSessionArchive_LedgerDaysTurnsRecordsOn` in `archive_test.go`: open with `ledgerDays = 31` (use the setup of the test at line 101), record one session with a token-carrying response through the store, erase it with `arch.Deleted` + `AwaitDelete`, and assert `arch.Stats().ForgetRecords == 1`. Open a second one with `ledgerDays = 0` and assert `ForgetRecords == 0` after the same steps.
- [ ] **Step 2: Run** `GOWORK=off go -C cmd/cortex test -run OpenSessionArchive .`. Expected: FAIL (too many arguments).
- [ ] **Step 3: Implement.** Add `archive.WithLedgerRetentionDays(ledgerDays)` to the `archive.Open` options. In `main.go`, before the call: `ledgerDays := 0; if costLedger != nil { ledgerDays = costLedger.RetentionDays() }` (the ledger opens before the archive, and `RetentionDays()` is the resolved value, 31 by default). Pass `0` in the 4 existing test calls.
- [ ] **Step 4: Run** `GOWORK=off go -C cmd/cortex test -race ./...`. Expected: PASS.
- [ ] **Step 5: Commit** — `feat: Give the session archive the cost ledger retention`.

---

### Task T4: Docs

Depends on: none. estLines: 50.

**Files:**
- Modify: `docs/session-api.md` (the "Rule 4 also applies to an erased session" paragraph near line 433, and the notes near lines 365 and 381)
- Modify: `docs/laptop-service.md` (the archive section near line 514-575, and the import paragraph near line 599)

Facts to write (all from this plan):
- When the archive removes a session (`x`, a clear `X`, the day limit `session.archive.retention_days`, the size limit `session.archive.max_bytes`), it adds one line to `forgotten.jsonl` in the archive `dir`, next to `data/` (on a local install: `~/.cortex/sessions/forgotten.jsonl`). The line holds the session id and the time span of its turns: `{"id":"...","from":<unix ms>,"to":<unix ms>}`.
- It writes a line only for a session with tokens or cost, and only when the cost ledger is on.
- A clear does not remove `forgotten.jsonl`.
- `agentop import` brings the removed session back in full. It sends to the cost ledger only the turns outside that span: turns from before Cortex saw the session, and turns added after the remove.
- A line is kept until its span is older than `cost_ledger.retention_days` (default 31) plus 1 day. So the file stays small: it holds only the sessions removed in that time.
- Replace the sentence "An erase leaves no record of the session" in `session-api.md`: an erase keeps a forget record, and the record is why the import does not count the cost again.

- [ ] **Step 1: Edit the two files.** Keep the docs' plain style. Change only the passages above.
- [ ] **Step 2: Run** `pre-commit run --files docs/session-api.md docs/laptop-service.md`. Expected: pass (or a fixed end-of-file, then pass).
- [ ] **Step 3: Commit** — `docs: Describe the forget records that an import uses`.

---

## Gate

From the worktree root:

```sh
test -z "$(gofmt -l core cmd/cortex)" \
 && GOWORK=off go -C core vet ./... \
 && GOWORK=off go -C core test -race ./session/... ./sessionapi/... ./cost/... \
 && GOWORK=off go -C cmd/cortex vet ./... \
 && GOWORK=off go -C cmd/cortex test -race ./...
```

Proof run on the unchanged base (2026-11-16, from the worktree root): `gofmt -l core cmd/cortex` printed nothing, `GOWORK=off go -C core vet ./...` and `GOWORK=off go -C cmd/cortex vet ./...` passed. No change to how the steps are called.

## For a human after review

Needs T1-T4 merged and a laptop install of that build (`make dev-install` restarts the shared proxy, so coordinate first).

1. `agentop import --agent claude-code`. Note `agentop cost --window month`.
2. In `agentop observe`, remove a few sessions with `x`. Check that `~/.cortex/sessions/forgotten.jsonl` has one line for each removed session that had cost.
3. `agentop import --agent claude-code` again. Check that the sessions come back, marked `imp`.
4. `agentop cost --window month`. Check that the total is the same as in step 1.
