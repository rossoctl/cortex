# Link, don't adopt: process attribution as a pure module

**Date:** 2026-10-28
**Status:** design approved in conversation, spec under review
**Related:** #1187 (per-process attribution), #1498 (`core/attribution`), #1437 / #1491 (Praxis)

## Problem

agentop's Other row on a live laptop store (2026-10-28) held 35
`pending:<agent>@<pid>.<start>` rows plus `default`. Each pending row is a process's
header-less calls that were never adopted into the session they belong to.

Adoption is the only way such a row joins its session, and it mostly fails:

- 7 buckets were adopted, 35 were not. All 7 were fresh one-shot processes.
- **Target already holds events** (10 logged refusals). `claude --resume` and parallel
  pr-review runs claim a session that already exists, and merging two numbered histories
  is refused.
- **Sibling process.** The process that names the session is not the bucket's process or
  its ancestor (Claude daemon: bucket pid 34922, session pid 34943).
- **No header ever.** An SDK probe (pid 8513, `max_tokens: 1`), OpenCode's TUI-to-service
  calls.
- **Restart.** `adoptLocked` checks memory only, so an archive-only bucket can never be
  adopted. No log line.

Two more costs follow from the design, not from these cases:

- **Other row.** The server sends pending buckets with an empty `agent`, so agentop's
  `inOtherAgents` pools them, though the id names the agent.
- **Attribution cannot leave the store.** A header claim renames store entries (`Claim`
  → `adoptLocked` → `rekeyLocked` → `Rekeyer` → archive `opRename`), and liveness checks
  read the store. #1498 notes `Resolve` "is not a pure query". A module in another process,
  which Praxis needs, can do neither.

## Root cause

A row's session is decided at write time as a guess, then renamed if the guess was wrong.
For the rows above there is no right answer at write time: an MCP connection belongs to the
process, not to a conversation; a resumed run's startup traffic belongs to the new process,
not the old session; an SDK probe never had a conversation.

## Design

Attribution stamps a final value on each request. The store records it and never re-files.
Linking a process's pre-session rows to its session happens at read time, from data already
recorded.

### Components

**1. `core/attribution` (new package, no store writes)**

```go
type Attribution struct {
	Agent        string // canonical agent name, "" for none
	Proc         string // "<pid>.<start>", "" when the lookup failed
	Session      string // a real session id, "default", or "" (not yet named)
	Header       string // the header that named Session, if any
	Source       Source // header | process | affinity | active | none
	ChainSession string // the topmost ancestor's session (cpex taint)
}

func (a *Attributor) Resolve(conn ConnKey, h http.Header, m *Memo) Attribution
```

- Owns the claims table now held in `session.Store` (`procs`, `owners`).
- Liveness is its own: a claim lapses after `procIdleTTL` (24h) without a request from
  its process. It does not ask the store whether a session exists.
- Never returns a bucket id. An agent process that has named nothing gets `Session: ""`
  with `Proc` set. The listener derives the bucket id from `Agent` and `Proc`
  (`session.PendingProcessID`), so attribution holds no store naming.
- `default` stays the answer for non-agent traffic while an agent is active.
- `Program` and `SelfTraffic` move with it, as #1498 proposes.

**2. Wire contract (`core/attribution/wire.go`)**

- Headers: `x-cortex-agent`, `x-cortex-proc`, `x-cortex-session`, `x-cortex-source`.
- **Strip, then inject.** Every listener removes these from client requests. `Decode`
  runs only for a hop configured as trusted. None is configured in this PR.
- This PR ships the header names and `Strip` only. `Encode` and `Decode` land with the
  relay that is their first caller.
- Purpose: a local relay can later run `Resolve` at accept time and stamp these, with the
  recorder and agentop unchanged (#1437's relay, #1491's strip-then-inject contract).

**3. `session.Store`: records, never re-files**

- `SessionEvent` gains `proc` (`"<pid>.<start>"`, omitempty).
- A row with `Session == ""` is filed under `pending:<agent>@<proc>` (today's id format,
  kept so the archive's existing rows still read). Never renamed.
- With no `Proc` (lookup failed, affinity-only): `pending:<agent>`, never linked.
- **Moved to attribution:** `Claim`, `ClaimProcess`, `SessionForClient`,
  `SessionForProcess`, `TouchProcess`, `ChainSession`, `ProcessHints`, `IsAgentProcess`.
- **Deleted:** `adoptLocked`, `followAdoptedLocked`, the `adopted` map,
  `SessionSummary.Adopted`.
- **Kept:** `Rekey` and `Rekeyer`, for the in-cluster `default` → A2A contextId rekey.

**4. Read-time link**

- A session's fold records `procs: {"<pid>.<start>": firstAt}` — each process that sent
  a headered request into it, and when it first did. The archive persists it with the
  summary.
- **Rule:** an event in process P's row belongs to the first session P claimed after that
  event's time. A response goes where its request went. Events after P's last claim stay
  in P's row.
- `GET /v1/sessions` marks P's row `linkedTo: [S, …]`.

**5. agentop (option i: fold)**

- A process row whose every event is linked is hidden from the list.
- Opening a session pages in its linked process rows' events and shows them first,
  marked "before session".
- An unlinked process row is listed under its agent (the agent comes from the summary,
  or from the id for archived rows written before this change). Never under Other.
- `default` stays under Other.

**6. Plugins**

- The inference-router, sessionbudget and cpex test `pending:` today. They switch to one
  helper, `session.IsConversation(id)`: false for `""`, `default` and any `pending:` id.

### Data flow

1. The listener calls `attribution.Resolve` and gets `{Agent, Proc, Session, Source}`.
2. The pipeline runs with `pctx.Session` set as today: a not-yet-named agent process's
   request carries its bucket id with `SessionSourcePending`, so no plugin sees a change.
3. The recorder files the event under `Session`, else under `pending:<agent>@<proc>`.
   The event carries `proc` either way.
4. A response is filed under its request's id. Nothing redirects it.
5. A process's first headered request into a session adds `{proc: firstAt}` to that
   session's fold.

### Cases

| Case | Result |
|---|---|
| Fresh launch: MCP handshake, then the first prompt | Linked and folded in (what adoption did) |
| `--resume` into a session with 5,764 events | Linked. No merge, so nothing is refused |
| Parallel pr-review runs naming one session | Both process rows linked |
| A process that never names a session (SDK probe, OpenCode TUI calls) | Unlinked row under its agent |
| Daemon sibling (34922 vs 34943) | Unlinked row under claude-code |
| Proxy restart | The key is stable and the link is computed from recorded data, so it still links |
| Long MCP stream opened before the claim | Its response follows its request: linked |
| `/clear` | Rows before S1 go to S1; rows between S1 and S2 go to S2 |
| `default`, and anything in-cluster | Unchanged |
| No process lookup | `pending:<agent>`, never linked |

### Known limits

- **Archived buckets written before this change stay unlinked.** Their events have no
  `proc`. They show under their agent until retention (30 days) removes them.
- **A linked row's cost** is added to the session's row by agentop, but `/v1/usage?session=S`
  still excludes it. Of the 35 rows on 2026-10-28, the only one with a cost is unlinked.
- **Claim lifetime.** After a `DELETE /v1/sessions/{id}`, a header-less call from the
  claiming process files under the deleted id again until the claim lapses, as a headered
  call already does.

## Error handling

- **Process lookup fails:** `Proc` is empty, so the request goes to `pending:<agent>` and
  is never linked. A link without a process would be a guess.
- **Wire headers from a client:** always stripped. `Decode` only for a trusted hop.
- **PID reuse:** the key is `pid.start`.
- **Old summaries without `procs`:** link nothing.

## Cost

Measured 2026-10-28 on the live store: 693 sessions, 126,531 events, 221 MB of archive;
pending buckets are 1.1% of events. `GET /v1/sessions?archived=true` takes 4–9 ms;
one archive page 1.3 ms.

- **Per request:** unchanged. The process lookup is once per connection (~325 µs macOS,
  ~1 ms Linux, from #1229).
- **Memory:** one string field per event for `proc` (a 16-byte header; the text is
  shared per process through the interner). Process rows count against
  `max_sessions` as pending buckets do today.
- **Disk:** about 0 after zstd; a few bytes per session for `procs`.
- **Under the store lock:** less. No rename that rewrites every event's id; no archive
  re-fold or directory move.
- **Reads:** the list joins processes to sessions (one map over the summaries).
  Opening a linked session costs one more archive page.

## Testing

- **`core/attribution`:** table tests of the resolution order from #1498, with no
  `http.Request` and no listener. Wire round-trip and strip tests.
- **The link rule:** a table test covering fresh launch, resume, parallel runs, `/clear`
  between two claims, a response following its request, and an archive-only bucket after
  a restart.
- **agentop:** the fold, tested against the list's real wire projection (`view=summary`
  strips fields that fixtures supply).
- **Live acceptance** on a scratch proxy (`:477xx`): a fresh `claude`, then `/clear`, then
  `claude --resume`. Each launch's MCP handshake folds into its session; an SDK probe stays
  a row under claude-code.

## Delivery: one PR

About +2.5k–3.3k / −1.6k–2k across ~30–35 files. Commits are ordered so each one reviews
on its own:

1. **Move into `core/attribution`, no behaviour change.** The claims table from
   `session/process.go` and `affinity.go`, and the resolve and filing rules from
   `forwardproxy/server.go`. Adoption still calls the store from its new home until
   commit 4. Checkable with `git diff --color-moved`.
2. **Events carry `proc`; the fold records `procs`; the archive persists both.**
3. **Read-time link, `linkedTo`, agentop's fold.**
4. **Remove adoption.** `Resolve` stops writing to the store; plugins use
   `session.IsConversation`.
5. **Wire contract.** `wire.go` with round-trip and strip tests.
6. **Docs.** `CLAUDE.md`'s `/v1/sessions` field list (drop `adopted`, add `linkedTo`,
   `procs`; event `proc`), `docs/session-api.md`, and the bucket wording in
   `docs/cpex-plugin.md` and `docs/inference-router-plugin.md`.

Why one PR: each line changes once (moved, then changed in its new home), no stacked
branches on the busy `forwardproxy/server.go`, one live acceptance run.

## Out of scope

- A relay process that runs `Resolve` and stamps the wire headers (#1437).
- ext_proc using `Resolve` (#1498 question 3). It keeps `ActiveSession()`.
- Counting a linked row's cost in `/v1/usage?session=`.
