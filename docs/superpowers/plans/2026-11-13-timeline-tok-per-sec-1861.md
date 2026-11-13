# Timeline tok/s column Implementation Plan (#1861)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show a live `tok/s` value for each agent row in the agentop Timeline pane.

**Architecture:** The forward proxy publishes the live byte counter of each open inference
response in the session store. The timeline endpoint converts those bytes to live tokens
with a bytes-per-token ratio from the session's long finished turns. agentop calculates
`Σ tokens / Σ turn time` over each agent's newest 60 s of model time.

**Reviewer check (2026-11-13):** gofmt and both `go vet` steps of the gate pass on the base, run from the worktree root as written. No gate change.

**Tech Stack:** Go 1.26.10, `sync/atomic`, bubbletea/lipgloss (agentop).

**Spec:** issue #1861 (`gh issue view 1861 --repo rossoctl/cortex`). The issue body is the design. Read it before you start a task.

## Global Constraints

- Go modules touched: `core/` and `cmd/agentop/`. Run every Go command with `GOWORK=off`.
- Comments, commit messages and docs: ASD-STE100 Simplified Technical English.
- Commits: `git commit -s`, and end the message with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. No `Co-Authored-By`.
- New JSON fields: `outputTokens`, `liveTokens`, both `omitempty`, on `timeline.Turn`.
- Window: 60 s of model time. Calibration: turns with `Duration − Ttfb ≥ 5 s`.
- Out of scope: envoy-sidecar (extproc), the reverse proxy, a thinking label, a status-line total, a per-model ratio, a refresh-cadence change.

## Decisions made for this plan

1. **Turns with zero tokens do not count in the window.** A failed, lost or usage-less turn has no tokens. If its time counted, one error would pull the value down. An open turn with `LiveTokens == 0` is also left out (the spec says so).
2. **The turn that makes the total reach 60 s counts in full.** The spec says "stop when the total reaches 60 s". The plan does not cut a turn into parts. This keeps the calculation simple, and a long live turn then gives the live estimate alone, which is correct.
3. **An open turn ends at the time of the fetch that gave its `LiveTokens` (`m.timeline.fetchedAt`), not at the layout's `now`.** `now` moves once each second, but `LiveTokens` changes only at a fetch. If the turn ended at `now`, the rate would fall between fetches and jump up at the next one. So the value changes only at a fetch.
4. **`live` is true when the window contains an open turn.**
5. **Format:** `fmt.Sprintf("%7s", strconv.FormatFloat(v, 'f', 0, 64))`, and 7 spaces when the row has no counted turn.
6. **`FillLive` runs before `Clip`** in `handleTimeline`, on the clone. `clone` copies `Turns` with `slices.Clone`, so the cached timeline keeps no `LiveTokens`.
7. **No forwardproxy test.** The wiring is 5 lines. The `bodycount` race test, the store tests and the sessionapi test cover the parts. The full `core` race run covers the listener.
   **As built (2026-11-13, review):** this decision is reversed. `TestStream_CountsArePublishedWhileTheResponseIsOpen` covers the wiring: it fails when the `TrackStream` call is deleted.
8. **The store map is global, keyed by request ID**, not per session. Request IDs are unique per process.

## Review Focus

1. A turn that streams for more than 60 s: the window holds that one turn, and the value is its live estimate. T6 test `TestTimelineTokPerSec_LongLiveTurnIsTheWindow`.
2. A session with no turn of 5 s or more: no `liveTokens` at all, and the column shows only finished turns. T4 test `TestFillLive_NoRatioDoesNothing`, T6 test via a row with only an open turn without `LiveTokens` (blank value).
3. Concurrent reads of the counter while the body streams must not race. T1 race test.
4. A request with `untrack` already called: `StreamBytes` returns `false`. T2 test.
5. A cache hit must not leak `liveTokens` into the cached copy. T5 test.

---

### Task T1: Atomic byte counter in bodycount

Depends on: none

**Files:**
- Modify: `core/listener/internal/bodycount/bodycount.go` (the `Read` method and the `Wrap` comment "Not safe for concurrent reads")
- Create: `core/listener/internal/bodycount/bodycount_test.go`

- [ ] **Step 1: Write the failing test** `TestWrap_CounterReadableWhileReading`: wrap an `io.NopCloser` over a reader that returns 64 KiB in small reads. One goroutine reads the body to EOF with `io.Copy(io.Discard, rc)`. A second goroutine calls `atomic.LoadInt64(&n)` in a loop until the first one ends. Assert `n == 65536` at the end. Also assert that the counts the second goroutine saw never go down.
- [ ] **Step 2: Run** `cd core && GOWORK=off go test -race ./listener/internal/bodycount/` . Expected: FAIL with `WARNING: DATA RACE`.
- [ ] **Step 3: Change** `*c.n += int64(n)` to `atomic.AddInt64(c.n, int64(n))`. Change the `Wrap` comment: the counter is safe to read with `atomic.LoadInt64` from another goroutine. Reads of the body itself are still for one goroutine.
- [ ] **Step 4: Run** the Step 2 command. Expected: PASS.
- [ ] **Step 5: Commit** `feat: Make the bodycount counter safe to read from another goroutine`.

### Task T2: Live stream registry in session.Store

Depends on: none

**Files:**
- Create: `core/session/stream.go`
- Modify: `core/session/store.go` (add the fields to `Store`)
- Create: `core/session/stream_test.go`

**Interfaces:**
- Produces:
  - `func (s *Store) TrackStream(requestID string, n *int64) (untrack func())`
  - `func (s *Store) StreamBytes(requestID string) (int64, bool)` — reads `*n` with `atomic.LoadInt64`.
  - Fields on `Store`: `streamMu sync.Mutex` and `streams map[string]*int64`. Do not use `s.mu`: the hot path must not take the store's write lock.

- [ ] **Step 1: Write the failing tests** in `stream_test.go`:
  - `TestTrackStream_ReadsTheLiveCount`: `var n int64 = 10`, `untrack := s.TrackStream("r1", &n)`. `StreamBytes("r1")` returns `(10, true)`. `atomic.StoreInt64(&n, 25)`. It now returns `(25, true)`.
  - `TestTrackStream_UntrackForgets`: after `untrack()`, `StreamBytes("r1")` returns `(0, false)`. Calling `untrack()` a second time does not panic.
  - `TestStreamBytes_UnknownID`: returns `(0, false)` on a new store.
  - `TestTrackStream_UntrackKeepsANewerEntry`: track "r1" with `&a`, track "r1" again with `&b`, call the first `untrack`. `StreamBytes("r1")` still reads `b`. (The untrack deletes only when the map still holds its own pointer.)
- [ ] **Step 2: Run** `cd core && GOWORK=off go test -race ./session/ -run 'Stream'`. Expected: FAIL (undefined `TrackStream`).
- [ ] **Step 3: Implement** the two methods in `stream.go`. Create the map on first use. Callers check `s.Sessions != nil`, so the methods need no nil check.
- [ ] **Step 4: Run** the Step 2 command. Expected: PASS.
- [ ] **Step 5: Commit** `feat: Let the session store read the byte count of an open response`.

### Task T3: Forward proxy registers each inference response

Depends on: T1, T2

**Files:**
- Modify: `core/listener/forwardproxy/server.go`, after the `if capture != nil && pctx.Extensions.Inference != nil { … WrapCapture … } else { … Wrap … }` block (about line 943).

**Interfaces:**
- Consumes: `(*session.Store).TrackStream` from T2. The counter is `&pctx.ResponseBytes`, written atomically after T1.

- [ ] **Step 1: Add** after that block:
  ```go
  if s.Sessions != nil && pctx.Extensions.Inference != nil {
  	defer s.Sessions.TrackStream(pctx.RequestID(), &pctx.ResponseBytes)()
  }
  ```
  Add a one-line comment: the Timeline reads the live count to estimate the tokens of an open turn (#1861).
- [ ] **Step 2: Run** `cd core && GOWORK=off go test -race ./listener/...`. Expected: PASS.
- [ ] **Step 3: Commit** `feat: Publish the live byte count of each inference response`.

### Task T4: Timeline token fields and FillLive

Depends on: none

**Files:**
- Modify: `core/session/timeline/timeline.go` (`Turn`, `Timeline`, new method `FillLive`)
- Modify: `core/session/timeline/agents.go` (turn construction at `turn := Turn{…}`, about line 176)
- Test: `core/session/timeline/timeline_test.go` (add tests; use the existing event helpers in `fixture_test.go` where they fit)

**Interfaces:**
- Produces:
  - `Turn.OutputTokens int` `json:"outputTokens,omitempty"` — from `c.resp.Inference.OutputTokens`.
  - `Turn.LiveTokens int` `json:"liveTokens,omitempty"` — set only by `FillLive`, only on a turn with `End == nil`.
  - unexported `Turn.requestID string` = `c.req.RequestID`.
  - unexported `Timeline.bytesPerToken float64` = `Σ resp.BytesDown / Σ resp.Inference.OutputTokens` over finished turns with `resp.Duration - resp.Ttfb >= 5*time.Second` and `OutputTokens > 0`. Zero when no such turn exists. Put the 5 s value in a named constant `calibrateMinStream`.
  - `func (t *Timeline) FillLive(bytes func(requestID string) (int64, bool))`: for each turn with `End == nil` and `!Lost`, when `bytes(requestID)` returns `ok`, set `LiveTokens = int(float64(b) / t.bytesPerToken)`. Do nothing when `bytesPerToken == 0`.

- [ ] **Step 1: Write the failing tests:**
  - `TestBuild_TurnCarriesOutputTokens`: a finished main turn whose response has `Inference.OutputTokens = 120` gives `Turns[0].OutputTokens == 120`.
  - `TestBuild_BytesPerTokenFromLongTurnsOnly`: turn A: `Duration 8s, Ttfb 1s, BytesDown 39000, OutputTokens 1000`. Turn B: `Duration 3s, Ttfb 1s, BytesDown 999999, OutputTokens 10` (ignored, it streamed 2 s). Assert `tl.bytesPerToken == 39`.
  - `TestFillLive_FillsOnlyOpenTurns`: the timeline above plus an open turn with request ID "open". `FillLive` with a func that returns `(3900, true)` for every ID. Assert the open turn has `LiveTokens == 100` and the finished turns have `LiveTokens == 0`.
  - `TestFillLive_NoRatioDoesNothing`: only short turns plus an open one. After `FillLive`, the open turn has `LiveTokens == 0`.
  - `TestFillLive_UnknownStreamLeavesZero`: the func returns `(0, false)`. `LiveTokens == 0`.
- [ ] **Step 2: Run** `cd core && GOWORK=off go test ./session/timeline/ -run 'OutputTokens|BytesPerToken|FillLive'`. Expected: FAIL (unknown fields).
- [ ] **Step 3: Implement.** Set `OutputTokens` and `requestID` where the turn is built. Add the ratio sums in the same loop: no second pass over the events.
- [ ] **Step 4: Run** `cd core && GOWORK=off go test -race ./session/timeline/`. Expected: PASS, and no existing golden data changes (the new fields are `omitempty`). If a golden file under `testdata/` changes because a fixture has output tokens, regenerate it with the test's update flag and check the diff holds only `outputTokens`.
- [ ] **Step 5: Commit** `feat: Carry output tokens and live token estimates on timeline turns`.

### Task T5: Timeline endpoint fills live tokens

Depends on: T2, T4

**Files:**
- Modify: `core/sessionapi/timeline.go` (`handleTimeline`: after `tl, ok := s.timelines.get(s, id)` and before `tl.Clip(from, to)`, call `tl.FillLive(s.store.StreamBytes)`)
- Test: `core/sessionapi/timeline_test.go`

**Interfaces:**
- Consumes: `(*session.Store).StreamBytes` (T2), `(*timeline.Timeline).FillLive` (T4).

- [ ] **Step 1: Write the failing test** `TestTimeline_LiveTokensOnOpenTurnNotCached`: append to the store one finished inference pair for the main agent (`Duration 8s`, `Ttfb 1s`, `BytesDown 39000`, `Inference.OutputTokens 1000`) and one open inference request with `RequestID "open"`. Call `store.TrackStream("open", &n)` with `n = 3900`. GET the timeline twice (the second GET is a cache hit). Each time, assert that the open turn's `liveTokens == 100` in the JSON. Set `n = 7800` before the second GET and assert `200` there. Then read `s.timelines.entries[id].tl.Turns` and assert that every `LiveTokens == 0`. Follow the setup that the other tests in this file use to build a `Server` and its events.
- [ ] **Step 2: Run** `cd core && GOWORK=off go test ./sessionapi/ -run LiveTokens`. Expected: FAIL.
- [ ] **Step 3: Implement** the one-line call with a one-line comment: live bytes change without a new event, so they go on the copy and not into the cache.
- [ ] **Step 4: Run** `cd core && GOWORK=off go test -race ./sessionapi/`. Expected: PASS.
- [ ] **Step 5: Commit** `feat: Fill live token estimates on the timeline endpoint`.

### Task T6: agentop tok/s column

Depends on: T4

**Files:**
- Modify: `cmd/agentop/tui/timeline_layout.go` (`tlAgentRow`, the `build` closure in `tlAgentRows`, new func `tokPerSec`)
- Modify: `cmd/agentop/tui/timeline_pane.go` (`timelineGutterWidth`, the header at the `"   model  tools  calls"` line, `agentLine`, `legend`, and `hostLine` if its gutter pads to the width)
- Modify: `cmd/agentop/README.md`, Timeline section (about line 1334). In the example block, change the header `model  tools  calls` to `model  tools  calls  tok/s`, and append a right-aligned 7-column value to each agent row (`     85` on `main agent`, `     92` on the subagent row). Do not change the host row. In the sentence "The gutter is the window's model time, tool time and tool calls for an agent", add: "and its output tokens for each second of model time over its last 60 s of model time (`tok/s`, in the model colour while a turn streams, blank when no turn has tokens)".
- Test: `cmd/agentop/tui/timeline_pane_test.go`

**Interfaces:**
- Consumes: `timeline.Turn.OutputTokens`, `timeline.Turn.LiveTokens` (T4).
- Produces: `tlAgentRow.tokPerSec float64`, `tlAgentRow.live bool`, `tlAgentRow.hasRate bool` (false: no counted turn, blank cell).
- `func tokPerSec(turns []timeline.Turn, agent string, now time.Time) (rate float64, live, ok bool)`. Algorithm:
  ```
  take the turns of agent, newest Start first
  skip a Lost turn; tokens = OutputTokens if End != nil, else LiveTokens; skip when tokens == 0
  d = tlEndOr(End, now) - Start; skip when d <= 0
  sumTok += tokens; sumDur += d; live ||= End == nil
  stop after the turn that makes sumDur >= 60 s (tokPerSecWindow)
  ok = sumDur > 0; rate = sumTok / sumDur.Seconds()
  ```

- [ ] **Step 1: Write the failing tests:**
  - `TestTimelineTokPerSec_Window`: main agent turns, oldest first: a 30 s turn with 900 tokens, then a 20 s tool gap (no turn), then a 20 s turn with 2000 tokens, then three 1 s burst turns with 100 tokens each, then an open turn that started 20 s before `now` with `LiveTokens 2000`. The window takes the open turn (20 s, 2000), the 3 bursts (3 s, 300) and the 20 s turn (20 s, 2000): 43 s. Then the 30 s turn makes 73 s ≥ 60 s and is counted in full: 5200 tokens / 73 s. Assert `rate ≈ 71.23` (to 0.01), `live == true`, `ok == true`.
  - `TestTimelineTokPerSec_FinishedSubagentKeepsItsRate`: a subagent with one finished 10 s turn of 850 tokens. Assert `85`, `live == false`, with `now` at the turn's end and at `now + 1h`.
  - `TestTimelineTokPerSec_OpenTurnWithoutLiveTokensNotCounted`: one open turn with `LiveTokens 0`. Assert `ok == false`.
  - `TestTimelineTokPerSec_LongLiveTurnIsTheWindow`: an open turn 90 s old with `LiveTokens 7200`, and an older 10 s turn of 5000 tokens. Assert `80`.
  - `TestTimelineRender_TokPerSecColumn`: use `timelineModel(t, 140, 46)` as `TestTimelineRender_RowsAndSpacing` does, with token counts in its fixture turns. Assert the header line contains `model  tools  calls  tok/s`, the main row ends with the formatted rate, a row with no tokens ends with 7 spaces after its calls value, and the legend contains `tok/s output per second of model time, last 60 s`. Run `TestTimelinePane_FitsEverySize` and `TestTimelineRender_HostGuttersAlign` unchanged: they must still pass with the wider gutter.
- [ ] **Step 2: Run** `cd cmd/agentop && GOWORK=off go test ./tui/ -run 'TokPerSec'`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - `tokPerSecWindow = 60 * time.Second` in `timeline_layout.go`.
  - In `build`, set the three row fields from `tokPerSec(tl.Turns, a.ID, now)`. Use all turns of the agent, not only those in `[from, to]`.
  - `timelineGutterWidth = 29`. Header: `"   model  tools  calls  tok/s"`. Update the gutter comment example.
  - `agentLine`: append the 7-column value. Style: `lipgloss.NewStyle().Foreground(colorTLModel)` when `live`, `styleMuted` otherwise.
  - `legend`: append `styleMuted.Render("   tok/s output per second of model time, last 60 s")`.
- [ ] **Step 4: Run** `cd cmd/agentop && GOWORK=off go test -race ./tui/`. Expected: PASS.
- [ ] **Step 5: Commit** `feat: Show tok/s for each agent in the Timeline pane`.

### Task T7: Document the new turn fields

Depends on: none

**Files:**
- Modify: `docs/session-api.md`, section `` ## `GET /v1/sessions/{id}/timeline` ``.

- [ ] **Step 1:** In the JSON example, add `"outputTokens": 850` to the `turns` entry.
- [ ] **Step 2:** In the **turn** bullet, add these sentences:
  - `outputTokens` is the output token count of the finished response (`inference.outputTokens` of the response row).
  - `liveTokens` is on an open turn only. It estimates the tokens that the response sent so far: the bytes the forward proxy read, divided by the session's bytes per output token. The ratio comes from the session's finished turns that streamed for 5 s or more (`durationMs − ttfbMs`). A session with no such turn has no `liveTokens`. The envoy-sidecar (ext_proc) and the reverse proxy do not give `liveTokens`.
  - `liveTokens` is calculated on each read. It is not part of the cached timeline.
- [ ] **Step 3: Commit** `docs: Document outputTokens and liveTokens on timeline turns`.

## PRs

One PR. The estimated total is about 610 counted lines, below the 2000-line limit.

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: Show live tok/s for each agent in the agentop Timeline pane` | none | T1, T2, T3, T4, T5, T6, T7 | 610 |

Task estimates: T1 40, T2 90, T3 10, T4 170, T5 70, T6 220, T7 10.

## Gate

From the worktree root:

```sh
(cd core && GOWORK=off go vet ./... && GOWORK=off go test -race ./listener/... ./session/... ./sessionapi/...) && (cd cmd/agentop && GOWORK=off go vet ./... && GOWORK=off go test -race $(GOWORK=off go list ./... | grep -v 'agentop$')) && scripts/dev/go-test-shards.sh -race cmd/agentop && test -z "$(gofmt -l core cmd/agentop)"
```

## For a human after review

- Run the new build on a laptop (`make dev-install`, which restarts the shared proxy on `:47600`). Open a live Claude Code session in agentop, open the Timeline pane, and check that `tok/s` moves while a turn streams and stays still while a tool runs. Needs all tasks.
