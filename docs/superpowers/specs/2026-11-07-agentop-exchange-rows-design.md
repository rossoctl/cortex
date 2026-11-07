# agentop: one row per exchange — design

Date: 2026-11-07
Issue: rossoctl/cortex#1681
Branch: `feat/exchange-rows`

## Problem

The events pane of `agentop observe` shows a request and its response as two
rows. They share an exchange number in the `#` column, but other rows can land
between them. To follow one call, the reader matches `#` values by eye. To read
what one call cost, the reader adds two rows. The prompt figures are on the
request row. The status, duration and output figures are on the response row.

## Measured on real traffic

Seven sessions from a laptop Cortex (22,583 events: Claude Code with subagents,
Bob, OpenCode, and the `default` session). The script applied agentop's own row
rules: the CONNECT fold, RequestID pairing, the guessed pairing, the ACTION
headline and the subagent lanes.

| Question | Result |
|---|---|
| Do pairs match exactly? | Every response paired by RequestID. 0 guessed pairs, 0 duplicate responses. 1 to 11 single rows per session (calls in flight, open tunnels). |
| How often is a pair split by other rows? | With subagent blocks collapsed, 72–98% of Claude Code main-lane pairs are adjacent. OpenCode 42%, Bob 63%. |
| What splits them? | Background polling: OpenCode `/health` and `/api/v1/models`, Bob `apprapp` telemetry. |
| Do merged totals equal the halves? | Tokens: prompt + output = total in 5,727 of 5,727. Cost: prompt_usd + output_usd within 1% of cost_usd in 5,704 of 5,707. |
| Which half do plugins act on? | All 8,914 `modify` actions are request-side. No `deny`. |
| Where does a response land for a reader at the tail? | On a row 0–5 rows above the tail at p90. 0.3% land 30 or more rows up. |

The laptop data has no inbound, A2A or response-side guardrail traffic.

## Goals

- One row per exchange: one line says what the call sent, how long it took, what
  came back and what it cost.
- A call that has no response yet is visible as such.
- The detail pane shows both halves at full width, and the reader always knows
  which half is on screen.
- No loss of anything that has no other place to go.

## Non-goals

- A key or setting to go back to two rows per exchange. The measurement shows no
  loss that the merged row, the detail pane or the Timeline does not cover.
- Grouping the exchanges of one user prompt.
- Stable `#` numbers across `o` (load older page). They change there today, and
  this design does not change that.
- Changes to the proxy, the session API, the archive or the wire format. All the
  work is in `cmd/agentop/tui`.

## Design

### 1. Rows

`eventRow` gets one more field, `resp *pipeline.SessionEvent`. `event` stays the
row's first event: the request, or the single event of a row with one half.

After `computeEventPairs`, `rebuildEventsTable` folds each pair into the row of
its request:

- **Exact pairs only.** A pair merges only when both halves carry the same
  RequestID: the same guard `pairedResponse` applies to TOKENS and COST today. A
  guessed pair (an older proxy) stays two rows, because a wrong merge would show
  one call's status and duration on another call's row.
- **Position.** The merged row sits where its request arrived. A response
  changes that row in place and adds no row.
- **Single rows stay.** Each of these keeps one row:
  - a request in flight
  - a denial (`SessionDenied`)
  - an opaque tunnel that has not closed
  - a fire-and-forget request
  - a response whose request is not held (a page edge before `o`)
- **The CONNECT fold is unchanged.** A bridged row holds `tunnel`, `event` and
  `resp`.
- **Opaque tunnels merge.** Their open and close rows share a RequestID, so they
  become one row with both byte counts and the time the tunnel stayed open.

Every structure that is indexed by row moves to the merged rows: `ctxs`,
`partner` lookups in cells, `subagentBlock.rows`, `blockIDs`, `visibleRows` and
the search rows. `rowInactiveHidden` (`s`) keeps its meaning: an exchange is
hidden only when no plugin acted on either half.

### 2. Columns

| Column | Merged row | Sort key |
|---|---|---|
| `#` | The exchange number `computeEventPairs` gives today, so `#29` names the same call before and after this change. Numbers go up row by row, because a pair takes its number at its request. Gaps stay: they are subagent exchanges and rows `s` hides. Subagent blocks keep their own numbering from 1. | none (as today) |
| TIME | The request's time. | request time |
| DIR | Unchanged. | unchanged |
| PHASE | **Removed.** On merged rows it would always read the same. A blank STATUS says that no response arrived yet. | — |
| ACTION / PLUGIN | `eventAction` over the invocations of both halves and the folded tunnel. PLUGIN counts **distinct** plugin names: the same two plugins observe both halves of most inference calls, and a plain union reads "4 plugins". | the shown value |
| METHOD | The request's value. | unchanged |
| STATUS | The response's status. Blank while in flight. A denial shows its own status. | response status |
| DURATION | The response's duration. | response duration |
| BYTES | `↑<request> ↓<response>`, the format `bytesCell` already uses for tunnels. | sum |
| TOKENS | `<prompt>→<output>` in compact form, `70.6k→9`. With a tool-prune saving, `70.6k(−3.1k)→9`. Blank until the response arrives. | total tokens |
| COST | The exchange total (`cost_usd`), with the saving in parentheses. | total |
| HOST | The request's host. | unchanged |

`pairFold` and `foldPairKeys` are removed: every row is one exchange, so there
is nothing left to fold. The sort's exchange tie-break stays, keyed on the row's
own arrival index.

The compact TOKENS cell (`70.6k(−3.1k)→9` is 14 wide) stays under today's cap
of 18. Removing PHASE gives 8 columns back to the others. With savings, the
default columns need 200 columns today. At 160, TOKENS and COST drop off.
`measureColumns` measures the merged TOKENS and COST cells, as it measures the
current ones.

The `#` column's picker description changes to "exchange number: one per request
and its response". Tests that name PHASE change with it.

A saved `agentop-config.yaml` that names PHASE needs no migration.
`columnSelection` walks the columns this build has, so a `PHASE` entry is
ignored and the next save drops it. `sortSelection` validates against
`eventColumns`, so `sortColumn: PHASE` falls back to chronological. A test pins
both.

### 3. Cursor, pin and reveal

Three lookups compare only `r.event`. Each one must match **either half**:

- `findByKey` (`events_pane.go`): the cursor pin. A response whose request is
  not held is a single row, and its key is the response's. After `o` loads the
  request, the row merges and `event` becomes the request. A lookup on `event`
  alone then finds no row, and the code moves the cursor to row 0 and clears
  the pin.
- `eventsSurface.lineOf` (`search_surfaces.go`): the search position, for the
  same case.
- `revealPendingSeq` (`timeline_pane.go`): Enter in the Timeline. A tool's seq is
  the seq of the **response** that called it (`Tool.Seq = c.resp.Seq` in
  `core/session/timeline/agents.go`), so without this change the reveal never
  lands.

Tail-follow is unchanged. The bottom row is now the newest call to start.

### 4. Detail pane

Enter on a row opens one page, at full width, that scrolls as one document.

```
 REQUEST │ #29 · claude-sonnet-5 · 200 · 2.25s · ↑200.8kB ↓545B · 70.6k→9 tok · $0.0142
━━ REQUEST · 20:35:12.21 · observe inference-parser, thinking-fill ━━━━━━━━━━━━━
▌▾ inference
     model: claude-sonnet-5
   ▸ messages (41)
   ▸ tools (30)
━━ RESPONSE · 20:35:14.45 · observe thinking-fill, inference-parser ━━━━━━━━━━━━
   served:      bedrock/us.anthropic.claude-sonnet-5
   timing:      ttfb 1.10s · body 1.15s of 2.25s total
 ▾ inference
     finishReason: end_turn
     outputTokens: 9
```

Rules:

1. **One fixed line above the viewport.** It starts with the name of the
   section that holds the top visible line, then gives the merged row's
   figures. The reader always sees which half is on screen, and never scrolls
   to the response for its figures. The viewport is one line shorter for it.
2. **Two sections, REQUEST then RESPONSE,** in arrival order. Each section is a
   top node of the one detail tree (`detailTree`). Its title line names the half,
   its time, and the plugins that acted on that half with their actions. That
   restores the per-half view that the row's ACTION cell gives up.
3. **Each section runs today's header helpers on its own event:** `tlsHeader`,
   `rewriteHeader`, `servedHeader`, `timingHeader`, and `tunnelHeader` for a
   folded CONNECT. Each helper already returns nothing for an event that lacks
   its field, so no helper needs to know which half it is on. The header lines
   sit under the section title, and `filterForDetail` filters each half by its
   own phase, as it does today.
4. **The section name on the fixed line follows the scroll.** Deep in a long
   request it reads `REQUEST`. When the response title scrolls to the top, it
   reads `RESPONSE`. The section is found from the top row with `lineAtRow`.
5. **`Tab` moves the cursor to the other section's title** and scrolls to it.
   `Tab` is bound nowhere in agentop today. `←`/`h` stay "back" and `→`/`l` stay
   unbound in this pane.
6. **Enter folds a section,** the same as any node. Enter stays the only fold key
   (#1619).
7. **The cursor opens at the top,** on the REQUEST section.
8. **Search covers the page.** `/` marks matches in both sections, and `n`/`N`
   move through them in page order. A query opens the folds that hold its
   matches, as today.
9. **A missing half shows one line in its section instead of a tree:**

   | Row | Note |
   |---|---|
   | in flight | `waiting for response…` |
   | denied | `denied by <plugin>: no upstream call` |
   | response whose request is not held | `request not loaded: press o in events` |
   | opaque tunnel, not closed | `tunnel open` |

10. **A response that arrives while its detail is open fills its section in
    place.** After each events-table rebuild, the pane checks its exchange. If
    its `resp` was nil and the merged row now has one, the pane renders again
    with `showDetail(row, false)`. The cursor and the scroll stay. It then
    fetches the response's full event like any other.
11. **Fetches.** Against a projecting proxy, the pane fetches each half that
    `needsFullEvent` names, with its raw capture. `applyDetailEvent` accepts a
    reply whose seq is either half's, and writes it back to that half.

The plugin detail pane (Pipeline, Catalog) shares the viewport and is not
changed.

### 5. Yank (`y`)

`y` writes the event of the section the cursor is in: the request, or the
response. The file format stays one event per file, so nothing that reads
yanked files breaks. The footer hint names the half: `[y] yank request` or
`[y] yank response`. The "message bodies not loaded yet" note looks at the
half that `y` writes.

### 6. Documentation

`cmd/agentop/README.md`:

- The events-pane section: one row per exchange, what a blank STATUS means, and
  the TOKENS and COST figures.
- The column table: PHASE removed, and the new `#`, BYTES, TOKENS and COST text.
- The detail section: the two sections, the fixed line, `Tab`, and which half
  `y` writes.
- The keys table: `Tab` in detail.

The help overlay (`?`) lists `Tab` for the detail pane.

## Testing

Unit tests in `cmd/agentop/tui`, reading the screen off `View()` where the
behaviour is visual (the pattern `tui/table` and the events-pane tests use):

1. **Merge rules.** An exact pair renders one row at the request's position. A
   guessed pair (no RequestID) renders two. A request in flight, a denial, an
   unclosed tunnel and a response with no held request each render one row. A
   bridged CONNECT with its response renders one row.
2. **Cells.** BYTES shows both directions. TOKENS shows `prompt→output`, with and
   without a saving. COST shows the total. PLUGIN counts distinct names: the same
   two plugins on both halves read "2 plugins". A `deny` on the response half wins
   ACTION.
3. **`#`.** Numbers match today's `computeEventPairs` ids, and go up row by row.
4. **Sort.** DURATION and STATUS rank exchanges without `foldPairKeys`. TOKENS and
   COST rank by the total.
5. **`s`.** An exchange with an action on either half stays. One with none hides.
6. **Subagent blocks.** A block's rows are merged rows, and its header count
   equals the rows it shows.
7. **Cursor and reveal.** A pin on a response's key lands on its merged row.
   `revealPendingSeq` with a response seq lands on the merged row. The search
   position holds through a rebuild.
8. **Live fill.** A row in flight shows a blank STATUS. When its response
   arrives, the same row fills, the row count does not change, and a cursor on it
   stays.
9. **Detail.** Both section titles render. The fixed line names the section of
   the top line, before and after a scroll past the response title. The
   viewport is one line shorter. `Tab` moves
   between the titles. Enter folds a section. `n` moves from a match in the
   request to one in the response. A missing half shows its note. A response that
   arrives while the detail is open fills in, with the scroll kept.
10. **Fetch.** `applyDetailEvent` accepts a reply for either half and drops one
    for a row the reader left.
11. **Yank.** `y` in each section writes that half's event.
12. **Settings.** A file with a `PHASE` column entry and `sortColumn: PHASE` loads,
    and renders chronological.

Before the PR, run `go vet ./...`, `gofmt -l .` and `go test ./...` in
`cmd/agentop` with `GOWORK=off`. Then run `agentop observe` against the laptop
Cortex. Look at a Claude Code session with subagents, an OpenCode or Bob
session, and the `default` session.

## Risks

- **Cluster traffic was not measured.** Inbound A2A turns and response-side
  guardrails are where merging loses the most. A response-side `deny` shows on
  the row as `deny` with no half named. The RESPONSE section title names it.
- **The bottom row is the newest call to start, not the newest event.** A call
  that finishes changes a row 0–5 rows up (p90), with no new row as a signal.
  The Timeline (`T`) still shows when calls end.
- **The change is large.** The rows touch every column and the subagent blocks.
  The detail pane touches about 90 references to its state in 8 files, and 8
  test files read that state.
- **Rows and detail land in one PR.** A merged row opened in today's
  single-event detail shows only one half. The detail pane alone has nothing to
  show both halves of. Neither half is useful on `main` without the other.
