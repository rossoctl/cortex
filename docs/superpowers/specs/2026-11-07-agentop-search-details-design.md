# agentop: search the details of a session's events — design

Date: 2026-11-07
Issue: rossoctl/cortex#1680
Branch: `docs/search-bodies-1680`
Revision 2: the proxy searches the bodies. Revision 1 (agentop fetches one event at a
time) was built and measured, and was too slow. See "Measurements".

## Problem

On the events pane, `/` searches only the table columns. The operator cannot
find the turn that holds a given prompt or reply. They must open the events one
by one.

The events list fetches `view=summary`, which drops the message bodies. That is
why a session opens about 163x faster (#1339). This design must keep it.

## The rule

**A row matches if its detail view would show the query.**

The search reads every field that the detail pane draws, the `raw` part
included. So a match is always visible when the operator opens the row.

A request carries the whole conversation again. So a prompt from turn 3 also
matches turn 4, 5 and every later turn, because their detail views show it too.
That is the rule working as intended.

## Measurements

All on this laptop's largest session: 10,767 events, 40 MB compressed and
414 MB uncompressed on disk. "No match" is the worst case: every event is read.

| Approach | "No match" time | Peak heap |
|---|---|---|
| Rev. 1: agentop fetches one event at a time, with `raw` | did not finish in 580 s, over the 2000 events agentop holds | — |
| A: agentop fetches pages of 200 full events (no `raw`) | 41 s, 2000 events | 764 MB in agentop (upper bound) |
| A: pages of 50 | 73 s, 2000 events | 218 MB in agentop (upper bound) |
| **B: the proxy makes one pass** | **3.9 s, all 10,767 events** | **133 MB in the proxy** |
| (a plain archive read now, for comparison) | 2.3 s | 129 MB |

Single fetches, for the reason rev. 1 failed: one archived event costs 1.4–1.6 s
(4.9–5.3 s with `raw`), because the archive reads a segment from its start for
each event. One event in memory costs 0.04 s (0.45 s with `raw`).

B's figure comes from an in-process test on a copy of the archive, with no HTTP,
a simpler case fold, and capture blocks checked as raw text. The real pass also
decodes each distinct block, so it is slower. The plan measures it again.

## Decision: the proxy searches what the summary drops

The work splits along the line that `view=summary` already draws:

- **agentop checks what it holds.** The summary keeps every field but the
  bodies. agentop runs `detailHas` on each summary row it holds, with no fetch.
  That covers the header blocks too, because they come from summary fields.
- **The proxy checks what the summary drops**, and the capture. It sends back the
  seqs that match. This is one pass over memory and the archive.

A row matches when either side matches it. Neither side can match text the
detail view does not show, so the union is the rule.

## What the operator sees

### The prompt

- On the events pane, the `/` prompt shows one box:
  `/ hello▏   [ ] details  (tab)`.
- Tab turns the box on or off. Typed letters go into the query.
- When the box is off, `/`, `n` and `N` work exactly as they do now.
- Other panes do not show the box.

### When `details` is on

- Typing does not search. The footer says `[/hello details: Enter to search]`.
- Enter sends the search. The footer says `[/hello details: searching…]`.
- When the answer arrives, the cursor goes to the first match at or after the
  row where `/` was pressed, wrapping. The footer says `[/hello details 1/12]`,
  as `/` does now.
- On no match, the footer says `[/hello details: no match]`.
- When the proxy holds matches older than what agentop holds, the footer adds
  ` · 3 older (o)`.
- Esc while it searches drops the answer. The cursor has not moved.

### After the answer

- `n` and `N` go to the next and the previous match at once, wrapping. The
  footer shows `k/N`.
- When the session has new events since the search, `n` and `N` send the search
  again first.
- Enter opens the row. The detail search gets the same query, so the match is
  highlighted.

### Limits

- It moves only to rows agentop holds. Older matches need `o` first.
- It searches the rows the pane shows. Rows that `s` hides are not searched.
- The box stays on with the query while the operator stays in this session.
  Opening another session clears both. Neither is saved.
- `raw` exists only where the archive is on (a laptop install). Elsewhere the
  detail view shows no `raw`, and the search does not search it either.
- A proxy without the route answers 404. The footer then says
  `[/hello details: this Cortex has no details search]`.

## How it works

### The proxy: `GET /v1/sessions/{id}/search?q=<text>`

**Answer.** `200` with `{"seqs":[12,14,40],"lastSeq":980,"schemaVersion":1}`.
`seqs` is ascending: every event of the session that matches, in memory and in
the archive. `lastSeq` is the newest seq the pass read. An empty `q` is `400`.
An unknown session is `404` with the body `{"error":"session not found"}`. That
body is how agentop tells "no session" from "a proxy without the route", whose
404 is the mux's plain text.

**What it matches**, with the case fold of agentop's `find.Contains` (rune by
rune, `unicode.ToLower`):

1. The fields `summarizeEvent` drops, kept or dropped per phase as the detail
   view keeps or drops them:
   - request: `inference.messages`, `inference.tools`, `inference.toolResults`,
     `inference.historyToolCalls`, `mcp.params`
   - response: `inference.toolCalls`, `mcp.result`, `a2a.artifact`

   Both the values and the key names in those subtrees match, because the
   detail tree shows both. A tool's `parameters` is JSON, so its keys and
   values match as JSON. Numbers match as the tree writes them.
2. The capture, where the archive holds one (request events and response
   events):
   - each body block, one time for each segment: its JSON keys and values when
     the block is valid JSON alone, its text when not. Image and document
     blocks are skipped, because the detail shows them as a label.
   - the text between blocks, the headers (names and values) and the query
   - a response's status, headers and body. A binary or compressed body is
     skipped, because the detail shows it as a label.
   - an event matches when its capture uses a matching block or holds matching
     text.

**One field list.** The per-phase key lists in `filterForDetail`
(`inferenceReqKeys` and the five others) move to core as
`sessionapi.DetailKeys`. agentop's `filterForDetail` and the proxy both read it,
so the two cannot keep different lists.

**The pass.**
- Memory: copy the session's events under the lock, as `ViewPage` does, then
  check them outside it. A string of at least `session.InternMinLen` bytes is
  checked one time, by its address, because the store shares it between events.
- Archive: a new `(*archive.Archive).Find(id string, event func(*model.SessionEvent) bool, text func(string) bool) ([]uint64, error)`.
  It reads each segment one time. It checks each block with `text` one time,
  and marks each event that matches by `event`, or whose capture uses a
  matching block or holds matching text. The capture format stays inside the
  archive package.
- The two lists are merged by seq. The pass stops when the request's context
  ends, so a closed connection stops the work.

### agentop

- **Client.** `(*apiclient.Client).SearchSession(ctx, id, q string) (SearchResult, error)`.
  A 404 with no JSON body is `ErrNoSearch`, so the footer can say the proxy has
  no route.
- **Enter.** One command sends one search for each session id among the held
  rows (a linked bucket's events have their own id and seq numbers). It also
  runs `detailHas` on every held row, with the capture when agentop holds one.
  The matches are the rows either side names, in the order the table shows
  them.
- **n and N** step through that list. When a session's newest held seq is
  above the `lastSeq` of its answer, the search is sent again first.
- **Memory.** The answer is a list of integers. Nothing is fetched to check a
  row.
- **Stale answers.** Each search has a generation number. Esc, a new query, a
  new session, or leaving the pane increases it, and an answer with an old
  number is dropped.
- Rev. 1's per-event scan (`detailScan`, `runDetailScan`, the 20-check batch)
  is removed. `detailContent` and `detailHas` stay. The keys, the box and the
  resets stay as built.

## Costs and accepted downsides

- **A new proxy is needed.** An older proxy answers 404, and agentop says so. On
  a laptop, the upgrade restarts the proxy, which cuts every attached session.
  In Kubernetes, the AuthBridge sidecar image must be upgraded too.
- **The proxy copies part of the detail view's rule.** A parity test guards it
  (see Tests). Known gaps, which do not match:
  - text agentop writes itself in the `raw` part: `forwarded as sent`,
    `rewritten by a plugin: …`, `[image … KB]`, `[gzip body … KB]`, and the key
    names `raw`, `request`, `response`, `forwarded`, `forwardedBody`
  - a query that spans a key and its value, such as `role: user`
  - a block that is not valid JSON alone is matched as stored text, so a query
    with a quote or a backslash can miss in it
- **The work runs in the proxy.** On the largest session, one search takes about
  4 s of one CPU and about 133 MB of heap, about the same as one archive read
  now. On a laptop with `memory.limit_mb` 2048, this adds GC pressure for that
  time.
- **More exposure, not more access.** The session API has no authentication. The
  route makes it fast to grep every conversation. `GET /events/{seq}` already
  sends every body.
- **Size.** About 900 to 1,200 lines in two modules (estimate).

## Tests

**core/sessionapi** (`search_test.go`), with a store and, where named, an
archive in `t.TempDir()`:

1. A prompt in `messages` matches the request event and not its response.
2. A response's `toolCalls` arguments match the response and not the request.
3. Key names match: `toolUseId` matches an event whose message has one.
4. A tool's `parameters` matches by a key inside the schema.
5. `mcp.params` on a request, `mcp.result` and `a2a.artifact` on a response.
6. The case fold: `İ` and `ß` fold as `find.Contains` folds them.
7. Archive: a capture body block matches every event that uses it. An image
   block does not match by its base64 text. A binary response body does not
   match.
8. Archive and memory together: each seq is in the answer one time.
9. Empty `q` is 400. An unknown session is 404 with the JSON body.
10. A cancelled request context stops the pass.

**cmd/agentop** (parity and keys):

11. Parity, through a real `sessionapi.Server` on `httptest`: for a fixture of
    request and response events with messages, tools, tool results, MCP and
    captures, and a list of queries, "the proxy's seqs or `detailHas` on the
    summary row" equals "`detailHas` on the full event with its capture", for
    every event and query, minus the named gaps.
12. Enter lands on the first match. `n` and `N` step and wrap. The footer
    shows `k/N`.
13. A 404 without the JSON body gives the "no details search" footer.
14. A new event after the search makes `n` search again.
15. Esc, a new query, and leaving the pane drop a late answer.
16. The current search tests, and Task 3's key tests, pass.

## Measurement before the PR

Run the real `Find` and matcher, in-process, on the copy of the 10,767-event
session in `/tmp/arch-measure`. Record the time and the peak heap in the PR
body, beside the table above. Do not restart the shared proxy for this.

## Docs

- `docs/session-api.md`: the new route in the Endpoints table, and its own
  section (request, answer, what matches, the gaps, the cost).
- `cmd/agentop/README.md`: the `/` row, and the summary-view note.
- The `?` help overlay.
- The header comment of `search.go`.

## Out of scope

- Search across sessions on the sessions pane. The route makes it possible
  later.
- Marks on the matching rows in the table.
- Searching as you type with `details` on.
- Moving to matches older than what agentop holds.

## Noted, not changed here

`core/sessionapi/summary.go` keeps `Inference.Completion` and A2A `Parts` in
the summary for `eventHaystack`, a filter that no longer exists. Its comment and
`TestSummarizeEvent_KeepsEverythingTheFilterSearches` still name it. With this
design, the local check reads those two fields, so they now have a reader
again. The comment should say so. That change is in this PR.
