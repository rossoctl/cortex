# agentop: search the details of a session's events — design

Date: 2026-11-07
Issue: rossoctl/cortex#1680
Branch: `docs/search-bodies-1680`

## Problem

On the events pane, `/` searches only the table columns. The operator cannot
find the turn that holds a given prompt or reply. They must open the events one
by one.

The events list fetches `view=summary`, which drops the message bodies. That is
why a session opens about 163x faster (#1339). This design must keep it.

## The rule

**A row matches if its detail view would show the query.**

The search reads every field that the detail pane draws, the `raw` part
included. It does not use a list of fields of its own. So a match is always
visible when the operator opens the row.

A request carries the whole conversation again. So a prompt from turn 3 also
matches turn 4, 5 and every later turn, because their detail views show it too.
That is the rule working as intended.

## Why agentop searches, one event at a time

Measured on this laptop's largest archived session: 10,767 events, 40 MB
compressed and 414 MB uncompressed on disk. The archive and the in-memory store
keep each distinct string one time. One full event averages about 209 KB
(sourced: `core/sessionapi/summary.go`), so all events in full come to about
2.2 GB (inferred).

A search that finds **all** matches at once must read the whole session. A
search that finds the **next** match stops at the first one. Because history
repeats, the next match is usually close. So the cost depends on how far the
next match is, not on the size of the session.

Three ways were compared:

| Approach | Result |
|---|---|
| The proxy finds all matches, with the detail rule moved into core | Exact, but needs a new route, a field list in core, and a walk that does not expand shared strings |
| The proxy finds all matches in every field | A response event also holds its request's messages, which the detail view hides. Rows then match where the detail view shows nothing. Rejected |
| **agentop finds the next match, one event at a time** | Exact, because it uses the detail view's own code. The proxy does not change, so it works with proxies that are installed now. Chosen |

The worst case is "no match": then agentop fetches every event it holds, one
at a time. A progress line and Esc make that case safe. If it is too slow, a
proxy route that finds the next match on the server is the next step. It is not
in this design.

## What the operator sees

### The prompt

- On the events pane, the `/` prompt shows one box:
  `/ hello▏   [ ] details`.
- Tab turns the box on or off. Typed letters go into the query.
- When the box is off, `/`, `n` and `N` work exactly as they do now.
- Other panes do not show the box.

### When `details` is on

- Typing does not search. The footer says `Enter: search details`.
- Enter starts the search at the row where `/` was pressed, and goes down.
- The cursor stops on the first matching row.
- While it searches, the footer says
  `[/hello details: searching 340 of 1987…]`.
- On a match, the footer says `[/hello details: #123]`.
- On no match, the footer says `[/hello details: no match in 1987 events]`.
- Esc stops the search and puts the cursor back where `/` was pressed.

### After a match

- `n` finds the next match below the cursor. `N` finds the next match above
  it. Both wrap past the end.
- An event that was checked with no match is not fetched again for the same
  query. So a second pass is fast.
- Enter opens the row. The detail search gets the same query, so the match is
  highlighted.

### Limits

- It searches the events agentop holds: the newest 2000, plus the pages loaded
  with `o`. Older events need `o` first.
- It searches the rows the pane shows. Rows that `s` hides are not searched.
- The box stays on with the query while the operator stays in this session.
  Opening another session clears both, as it clears the query now. Neither is
  saved.

## How it works

All new code is in `cmd/agentop/tui`. The proxy and the session API do not
change.

### One function for the detail view's text

- Move the first half of `showDetail` (`detail_pane.go`) into
  `detailContent(row eventRow) (head []string, root *node)`. That half builds
  the header blocks, then runs `filterForDetail`, `withRaw` and
  `treeFromJSON`.
- `showDetail` calls `detailContent`. The detail view does not change.
- `detailHas(row eventRow, q string) bool` draws that tree with every fold open
  (`renderTree` with `foldState{all: true}`). It checks the header lines and
  each tree line with `find.Contains`, the matcher the detail search uses.
- So "matches" and "the detail view shows it" come from one piece of code.

### The search loop

New file `tui/search_details.go`.

1. Start at the cursor row. Go through the visible rows in the order the pane
   shows them. An event with more than one row is checked one time.
2. For each event:
   - If agentop already holds the full event (`fullFetched`), or the event has
     no body (`hasBody` is false), or the proxy does not project
     (`serverProjects` is false), check it at once with no fetch.
   - Otherwise, fetch it with `GetEventRaw`, as `fetchDetailEventCmd` does,
     and check the reply.
3. On a match, put the cursor on that row and pin it by event key, as
   `eventsSurface.reveal` does.
4. On no match, record the seq as checked and go to the next event.
5. When the loop comes back to the start row, report no match.

One fetch runs at a time.

### Memory rule

- A fetched event lives only in its reply message. agentop checks it and drops
  it.
- The search does not call `replaceHeldEvent` or `markFullFetched`. Opening a
  row calls both, and so keeps the full event. If the search did the same,
  agentop would keep every event it checked, up to the 2.2 GB above.
- The only state that stays is a set of checked seqs. It is cleared when the
  query changes.

### Stopping and stale replies

- Each search has a generation number. Esc, a new query, a new session, or
  leaving the pane increases it.
- A reply with an old generation is dropped.
- A fetch error stops the search. The footer shows the error.
- An event that left the held list during the search (eviction, `t`) is
  skipped.

### Keeping the screen live

Checks that need no fetch run at most 20 per update. Then the loop returns a
command that continues it, so keys and the stream still work during a long
search against a proxy that does not project.

### State on the model

- `m.searchDetails bool`: the box. It is cleared where the events pane's query
  is cleared now: `keys.go:708`, `timeline_pane.go:481`, `app.go:1153`.
- `m.detailScan`: the running search (query, generation, direction, start row,
  checked seqs, progress count), or nil.
- `searchKey`: Tab toggles the box when the target is `paneEvents`. Enter with
  the box on commits the query and starts the loop.
- `searchStep`: with the box on and a committed query on `paneEvents`, `n` and
  `N` start the loop. Otherwise they work as now.
- `searchStatus`: on `paneEvents` with the box on, it gives the footer lines
  above.
- Opening a row while the box is on sets the detail pane's query to the events
  pane's query.

## Tests

With a fake client that counts fetches:

1. The search stops at the first match. It fetches only the events between the
   cursor and that match.
2. After a search, `m.events` and `m.fullFetched` are the same as before. This
   pins the memory rule.
3. A prompt matches the request row and not its response row. This uses the
   real `filterForDetail`.
4. Text that is only in `raw` matches.
5. `n` wraps past the end. A second pass makes no fetch.
6. Esc, and a new query, drop a late reply. The cursor goes back to where `/`
   was pressed.
7. No match reports the count of events checked.
8. With the box off, `/`, `n` and `N` behave as now. The current search tests
   pass with no change.

## Measurement before the PR

Run a "no match" search on the 10,767-event session. Record in the PR body the
time it takes, and agentop's peak heap from pprof (`AGENTOP_PPROF_ADDR`).

## Docs

- `cmd/agentop/README.md`: the `/` row in Keybindings, and the `view=summary`
  note.
- The `?` help overlay.
- The header comment of `search.go`. Its rule "a pane says what it draws" stays
  true: with `details` on, the events pane searches what its detail view draws.

## Out of scope

- A total match count, and a mark on every matching row.
- Search across sessions on the sessions pane.
- A search route on the proxy.
- Searching as you type with `details` on.
- Events older than what agentop holds.

## Noted, not changed here

`core/sessionapi/summary.go` keeps `Inference.Completion` and A2A `Parts` in
the summary for `eventHaystack`, a filter that no longer exists. Its comment and
`TestSummarizeEvent_KeepsEverythingTheFilterSearches` still name it. The PR body
mentions this. This design does not change it.
