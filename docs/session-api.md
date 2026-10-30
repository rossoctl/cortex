# Session Events API

When `session.enabled` is true (the default) and `listener.session_api_addr`
is non-empty (default `:9094`; `:47601` on a local install), the proxy exposes
the captured session store over HTTP. Intended for operators debugging the
plugin pipeline — via `kubectl port-forward` in a cluster, or directly on a
laptop — and for the `agentop` TUI.

**Trust model:** no authentication. Bind only on in-cluster addresses, never
behind ingress. Payloads may contain raw user messages, LLM completions, and
tool results.

## Endpoints

| Method & Path | Format | Purpose |
|---|---|---|
| `GET /` | text | One-line-per-endpoint index — answers "is this the session API, and on the right port?" |
| `GET /v1/sessions` | `application/json` | List active sessions (see below) |
| `GET /v1/sessions/{id}` | `application/json` | The session's most recent events; `?limit=N` and `?before=<seq>` page backward |
| `GET /v1/sessions/{id}/events/{seq}` | `application/json` | One event by its sequence number |
| `GET /v1/sessions/{id}/timeline` | `application/json` | The session as spans: Claude Code agents, tool calls, subagent spawns and returns, and hosts (see below) |
| `DELETE /v1/sessions` | `application/json` | Clear every session: the store, and the session archive where one runs. The cost ledger is kept |
| `DELETE /v1/sessions/{id}` | `application/json` | Erase one session: the store's entry and, where the archive runs, its files (see below) |
| `GET /v1/events` | `text/event-stream` | SSE stream of new events; `?session=<id>` filters to one session. Heartbeat every 30s |
| `GET /v1/pipeline` | `application/json` | Active pipeline composition, per plugin: name, direction, position, capabilities, runtime `config` |
| `GET /v1/plugins` | `application/json` | Catalog of every registered plugin, active or not |
| `GET /v1/usage` | `application/json` | Windowed usage aggregation — the aggregate behind `agentop cost`; parameters and semantics in [`pricing.md`](pricing.md#reading-usage-over-http) |
| `GET /healthz` | text | Liveness probe |

## `GET /v1/sessions`

```json
{"sessions": [{
  "id": "…", "createdAt": "…", "updatedAt": "…", "eventCount": 42, "active": true,
  "title": "…", "agent": "claude-code", "totalTokens": 1186300, "costMicros": 254600,
  "avoidedMicros": 3700, "promptContext": {"tokens": 16384, "stated": true,
  "at": "2026-10-15T20:01:02Z", "msgs": 12}, "inferenceHost": "api.anthropic.com",
  "procs": ["41963.1791718279766260000"], "lastRequestAt": "…"
}, {
  "id": "pending:claude-code@41963.1791718279766260000", "eventCount": 12, "…": "…",
  "linkedTo": [{"session": "…", "first": "…", "last": "…"}], "linkedAll": true
}]}
```

`id`, `createdAt`, `updatedAt`, `eventCount` and `active` are always present;
every other field is omitted rather than zero when unknown. The fields worth
knowing by name:

- **`title`** is a suggestion, not an identifier — derived from the session's
  own events (a `/rename`, else a `<user_query>`, else ordinary user prose,
  with `<system-reminder>` blocks excised). Folded at append time and
  first-wins, except that a `/rename` always overrides. Nothing addresses a
  session by its title.
- **`inferenceHost`** — the host of the session's most recent request that
  carried an inference parse; after a redirect, the server it was sent to
  rather than the host the client named. `inferenceHostFromHistory` is `true`
  when that host came only from the archive's fold, because no request the
  proxy's current entry for the session holds has set it.
- **`procs`** — the processes, `<pid>.<start>`, whose own session header named
  this session. Recorded from each request's `proc` and `sessionHeader`, and
  continued from the archive's fold like the other figures.
- **Pending buckets.** A coding agent's calls before its process names a session
  are recorded under `pending:<agent>@<pid>.<start>` (or `pending:<agent>` when
  the process could not be looked up), carrying their `proc`. Nothing renames a
  bucket. Instead, when the list is read, a bucket whose process later named a
  session gets **`linkedTo`** — the sessions it named that its calls belong to,
  each with the `first` and `last` request that named it; a call belongs to the
  first session its process named at or after it — and **`linkedAll`** when every
  request in it falls before one of them. A response goes where its request went. agentop hides a
  `linkedAll` bucket that links to one session and shows its calls inside it; a
  bucket linked to two sessions, or with calls after its process's last claim,
  stays a row of its own.
- **`?archived=true`** adds history where the proxy runs a session archive
  (`session.archive`, laptop only): every session the archive holds and memory
  does not, marked `resident: false`, plus a top-level `archive` object —
  `{bytes, maxBytes, retentionDays}` and, only when nonzero, `droppedEvents`,
  `writeErrors`, `droppedRenames`, `paused`, any of which means the history has
  gaps. A session in both is listed once, as its resident row, and that row
  covers the session's whole history: `eventCount`, `totalTokens`, `costMicros`,
  `title`, `agent`, `createdAt` and the rest continue from the archive's fold of
  what came before this process's entry. Without an archive the parameter is
  ignored and no `archive` object is sent — which is how a client tells "no
  history here" from "no history yet".

## `GET /v1/sessions/{id}` — snapshots and paging

The session's most recent events. `?limit=N` (default 500, max 2000) sets the
window; `?before=<seq>` returns the page ending just before that event, so the
whole session is reachable by paging backward from the tail. `totalEvents` is
the session's true length and `oldestSeq` the oldest event the store still
holds — both present only when this response is not the whole session, so a
client can tell "this is the beginning" from "there is more behind me" without
a second request. 404 for an unknown or expired session.

**One response is not a full snapshot:** with `session.max_events` unset a
session can hold thousands of events, and one real session's whole history
encoded to over a gigabyte. The response is written one event at a time rather
than encoded whole, so serving it costs the proxy heap proportional to one
event — but a client that reads it all still pays for all of it. Page with
`before` rather than raising `limit`.

**With a session archive, a page continues from disk** below the oldest event
memory holds: a session resumed after a restart pages back through its whole
history, one evicted from memory is served instead of 404'd, and
`totalEvents`/`oldestSeq` describe the merged history. `GET
/v1/sessions/{id}/events/{seq}` falls back to the archive the same way, and
answers 404 rather than a neighbour when neither holds that seq.

## `GET /v1/sessions/{id}/timeline` — agents, tools and hosts on one time axis

The session's history as spans, for agentop's Timeline pane. Computed from the
whole session: memory first, then the archive behind it (a 6,000-event session
takes a few milliseconds; reading one that is only on disk takes over a second).
The result is cached per session until its newest event changes, however long
that takes; an unanswered request still ages out on each read, as it would on a
fresh compute. A pane refreshing every second costs one compute per new event,
and a session only on disk is read once. Reads at most 50,000 events, newest first;
`truncated: true` says older ones exist. 404 for an unknown session. `?from=` and
`?to=` (RFC 3339) keep only the spans that overlap the range; `agents` is always
the whole list.

```json
{"id": "…", "lastSeq": 1225, "start": "…", "end": "…",
 "agents":  [{"id": "main", "label": "main agent"},
             {"id": "a1bf9707ee0e71012", "parent": "main", "label": "Survey the code",
              "type": "Explore", "spawnTool": "toolu_…", "spawnedAt": "…", "returnedAt": "…"}],
 "turns":   [{"agent": "main", "start": "…", "end": "…", "finish": "tool_use", "seq": 12}],
 "jobs":    [{"agent": "main", "id": "bk42", "start": "…", "end": "…", "seq": 40}],
 "tools":   [{"agent": "main", "id": "toolu_…", "name": "Bash", "start": "…", "end": "…",
              "preview": "go test ./...", "seq": 13}],
 "returns": [{"agent": "a1bf9707ee0e71012", "into": "main", "at": "…", "seq": 90}],
 "hosts":   [{"host": "api.github.com", "kind": "tunnel", "start": "…", "end": "…", "status": 200, "seq": 7}]}
```

**Turns and tools are attributed to any known client** (Claude Code, Bob, Codex,
OpenCode, Pi — the canonical name parsed from the User-Agent); each gets a
single `main` row, and every model call is a turn, including classifier side
calls, labeled by `model`. Claude Code additionally gets its subagent tree,
returns and background jobs — those key on the agent ids and marker strings
(`agentRole`, `subagent`, `parentSubagent`) no other client sends. Requests
from unknown clients render as host rows only.

- A **turn** is one model call, request to end of response. Left out:
  progress-label and away-recap calls (matched by their prompt text in the
  last non-system message — Claude's own prompt strings, applied to every
  client), and Claude Code's tool-less side calls (titler, classifier).
  Another client's side calls are kept, cost and all. `error`
  marks a call that failed: an upstream error, a refusal, the client giving up,
  a finish of `content_filter`, `failed` or `cancelled`, or a response that
  ended with no finish reason, which is a stream cut mid-reply. `resent` marks
  a call that sent its agent's previous call's conversation again unchanged: a
  retry.
- A **tool** runs from the response that requested it to the agent's first
  request carrying its result (`inference.toolResults[].toolUseId`). A tool
  Claude Code started while the response was still streaming can have its
  result recorded first; its end is clamped to its start. A CONNECT to a model
  host that could not be dialed, after the tool started and before its result,
  ends it there unless its agent, or an agent below it, made a successful turn
  in between: the agent tried to report back and the network was gone, so the
  result arrived only when it came back, or never. A failure the agent recovered
  from is a blip and ends nothing. A tool with neither has no `end` and is still
  running. `preview` is up to 80 characters of the command, path or description.
- An **agent** links to the `Agent` tool call that started it by the id the
  call's result names (`agentId: …`). A fork's result names none, so a fork
  takes its parent's earliest unmatched spawn. Requests recorded before the
  proxy captured agent ids are one `subagents` agent.
- A **return** is a subagent's result reaching another agent:
  `<agent-message from="…">` or `<task-notification>` in a message that agent
  had not sent before.
- A **job** is a background shell command, from the tool result that
  acknowledged it (`Command running in background with ID: …`) to the first
  `<task-notification>` naming that id. Its tool call ends at the
  acknowledgement, so the job is what shows it still running. A job with no
  `end` is one no notification named: still running, or never reported, as a
  server left running when its agent exited is. A job whose agent was cut off
  ends where its tools do: at the first failed dial to a model host after the
  agent's last call. Absent when the session started none.

**Hosts** are every outbound request paired with its response, every opaque
tunnel, and every denial. `kind` is `llm`, `mcp`, `http` or `tunnel`, and
`h:443` shares `h`'s row.

**`end` absent means still in flight.** A span that never got an answer but
provably stopped is `lost` and ends where it started: a turn or tool whose agent
moved on, a `SubagentHandback` (it ends the run), or a request more than ten
minutes old.

## `DELETE /v1/sessions`

Clears every session: the in-memory store, and the session archive where one
runs. **The cost ledger is kept** — spend totals, no content. Answers
`{sessions, archivedSessions, bytes}`; a failed disk half is a 500 adding
`archiveError`, with memory cleared regardless.

This is the one state-changing route pair on an unauthenticated API, so both
are guarded against a browser: DELETE only (a cross-site DELETE needs a
preflight this server never approves), a loopback `Host` (`localhost`,
`127.0.0.1` or `::1`, which defeats DNS rebinding), no `Origin` header, and
the server must have been built with clearing allowed
(`listener.bind_loopback_only` on a local install). A refusal is a 403
`{"error": ...}`. agentop's `X` drives it.

## `DELETE /v1/sessions/{id}` — erase one session

Erases one session: the store's entry and, where the archive runs, its files —
a session only the archive holds too. The same four guards as
`DELETE /v1/sessions` (pinned in `core/sessionapi/delete_test.go`) and the
same answer shape, each count 0 or 1. **404** when neither half held the id;
**500** with `archiveError` when the disk half failed (memory is erased
regardless). The cost ledger, usage totals and inference-router pins are
kept. An agent still using the session brings it back with its next request.
agentop's `x` drives it, one request per session for an agent-wide erase —
see [`laptop-service.md`](laptop-service.md) for the user-facing story.

## Quick examples

The `agentop` TUI handles port-forward + connection automatically — pick a
pod from the Namespaces → Pods picker. For raw HTTP exploration, set up your
own port-forward first:

```sh
POD=$(kubectl get pod -n team1 -l app.kubernetes.io/name=weather-agent \
  -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward -n team1 $POD 9094:9094 &

# List sessions
curl -s http://localhost:9094/v1/sessions | jq

# Snapshot the most recently updated session
SID=$(curl -s http://localhost:9094/v1/sessions | jq -r '.sessions[0].id')
curl -s "http://localhost:9094/v1/sessions/$SID" | jq

# Page backward: the oldest event this response carries is the next cursor.
# Repeat until the first event's seq equals oldestSeq — that is the beginning.
curl -s "http://localhost:9094/v1/sessions/$SID?limit=500" \
  | jq '{cursor: .events[0].seq, oldestSeq, totalEvents}'
curl -s "http://localhost:9094/v1/sessions/$SID?limit=500&before=<cursor>" | jq '.events|length'

# Live tail every event
curl -N http://localhost:9094/v1/events

# Live tail a single session
curl -N "http://localhost:9094/v1/events?session=$SID"
```

## Event schema

Every event on `/v1/sessions/{id}` and `/v1/events` carries:

- `at`, `direction`, `phase` — when, which side, what stage. `phase` is one of
  `"request"`, `"response"`, or `"denied"` (terminal denial from a pipeline
  plugin — typically a jwt-validation failure).
- `seq` — the event's position in its session, counting from 1, and the cursor
  `?before=` takes. Gaps are normal: FIFO eviction drops a prefix, and a pinned
  intent can sit far ahead of the retained tail. Unique within one incarnation
  of a session, not forever: trimming never reuses numbers, but whole-session
  eviction deletes the counter with the entry, so a session re-created under
  the same id restarts at 1. A paging client must order pages by `at`, not by
  `seq`. A session archive moves that boundary out to disk: numbering continues
  across restarts for as long as the archive holds the session.
- `a2a` / `mcp` / `inference` — protocol parser payloads (one at most).
- `invocations` — per-plugin invocation records for every plugin that ran on
  the pipeline pass, structured `{inbound: [...], outbound: [...]}`. Each entry
  carries `plugin`, `action` (see the vocabulary below), `reason`
  (machine-stable code), and optional plugin-specific context.
- `plugins` — escape-hatch map for plugin-specific observability: keys are
  plugin names, values the raw JSON each plugin emitted. See
  [`plugin-reference.md`](plugin-reference.md#emitting-session-events) for the
  producer contract.
- `identity`, `host`, `statusCode`, `error`, `durationMs` — request-level
  context. **A 502 here is not always the upstream's answer:** when the
  exchange with the upstream fails — timeout, refusal, DNS, TLS, a body that
  breaks off mid-read — the proxy synthesizes the 502 itself, with `error.kind`
  saying which (`upstream_timeout`, `upstream_refused`, `upstream_dns`,
  `upstream_tls`, else `upstream_error`) and `error.message` naming the address
  but never the request URL, whose query string can carry an API key.
  `error.kind: "proxy_error"` is the one that is the proxy's own: a response
  over the listener's body-size cap. A 499 with `error.kind: "client_canceled"`
  is the client hanging up first — since neither proxy times out a slow
  upstream, that is how a hung one usually ends. A 426 with `error.kind:
  "upgrade_declined"` is the forward proxy's own answer to a WebSocket upgrade on
  OpenAI's Responses API (`chatgpt.com` or `api.openai.com`): nothing is forwarded,
  and a client such as Codex reads it as "use HTTPS" and retries at once over
  HTTPS. It is not counted as an error in `/v1/usage`. Every other WebSocket
  upgrade is forwarded (#1488): the 101 the origin answers with is spliced onto
  the client's connection, the frames after it are relayed uninspected, and the
  session records the request row plus a **101 response row** paired to it by
  `requestId`, carrying `bytesUp` and `bytesDown` for the whole WebSocket
  session — the one response row besides a tunnel close that carries both —
  with `durationMs` covering the session and `ttfbMs` the handshake. Because
  that `durationMs` is a session lifetime rather than a request latency, the
  101 row is **excluded from `/v1/usage`** exactly as a tunnel row is: folding
  it would drag the latency mean toward WebSocket lifetimes (one 10-minute
  session moved a host's mean from 500ms to 6.4s in testing), its frames are
  opaque so there is no model, token or cost to attribute, and counting it
  would add one Request whose latency is its whole lifetime. On the timeline
  the lifetime is the point, so the field stays. **That exclusion is outbound
  only.** The reverse proxy also records a 101, but at *handshake* time
  (`ReverseProxy` hands off the connection instead of copying a body, so a
  deferred record would never fire), so its `durationMs` is the handshake
  latency — an ordinary request figure. An inbound WebSocket handshake is
  therefore still counted as a Request. A relay that ended in an
  error carries `error.kind: "proxy_error"` on the 101 row, so a session that
  broke mid-stream is distinguishable from one that closed cleanly; an
  ordinary close is not marked. A **403**
  with `error.kind: "destination_private"` is the egress guard's refusal
  (`listener.egress_guard`): the destination resolved into loopback,
  link-local, private or this host's own address space and no permit covered
  it. Also the proxy's own answer rather than the upstream's, and also not
  counted as an error — the guard did what it was configured to.
- `ttfbMs` — on a delivered response row, the wall time from request entry to
  the upstream's response **headers** arriving. `durationMs` − `ttfbMs` is then
  how long the body took — for a streamed completion, the stream time. The pair
  separates a slow-to-answer origin from one that answered and then trickled,
  which `durationMs` alone cannot (#1251). The row is recorded when the listener
  is finished with the body — after the read on a buffered path, after the relay
  on a streamed or unbuffered one — so the difference really is the body's time.
  The one exception is the envoy-sidecar listener with a pipeline no plugin of
  which reads bodies: the row is recorded at the headers while Envoy relays the
  body unseen, and ttfb is left absent there rather than reported as a figure
  equal to the whole duration. Absent when the exchange failed
  before headers (the 502 cases above), on request rows, and on tunnel rows —
  an opaque tunnel has no headers to time. agentop's detail pane renders the
  split as a `timing:` line.
- `requestedHost` — present only when a plugin redirected the request: the
  host the client asked for. `host` is where the request actually went, and
  usage, the cost ledger and pricing all follow `host`.
- `inference.requestedModel` — present only when a plugin changed the model:
  the model the client asked for. `inference.model` is the model the request
  was sent for, which settlement prices.
- `bytesUp`, `bytesDown` — **body** bytes, one direction per row: a request row
  carries `bytesUp`, the body as forwarded (so a rewrite is reflected); a
  response row carries `bytesDown`, the body as it came back from the
  destination. Headers are not counted. A tunnel close and a forwarded
  WebSocket's 101 response row are the rows carrying both. **Zero means the
  listener counted nothing, not that the body was
  empty** — every listener buffers bodies only when a plugin asks, so an
  unbuffered request reports zero. A response relayed straight through is
  counted, not zero: both proxies record the row after they finish relaying
  it (#1251), so the bytes that crossed their own process are on the row. The
  one arm that still reports zero is the envoy-sidecar listener with a
  body-free pipeline — it asked Envoy not to send it the bytes, so zero there
  means "not asked to see", not "not counted".
- `tunnel`, `tunnelReason` — an opaque CONNECT (or transparent-redirect) tunnel
  records two rows sharing a `requestId`: the open (`phase: "request"`,
  `tunnelReason` saying why the bytes stayed opaque) and the close (`phase:
  "response"`), carrying the CONNECT's own status, the duration, and the bytes
  each way. It is not the destination's status: that travels inside the
  client's end-to-end TLS. Tunnel rows are kept out of `/v1/usage`: a tunnel's
  lifetime is not a request latency. `tunnelReason: "dest-private"` is the
  egress guard's refusal: the close row carries 403 and
  `error.kind: "destination_private"`, and no tunnel was ever opened to the
  destination — the same refusal a plain request gets, one layer down.
- `dialAddr` — present on a tunnel's open row when the egress guard is on:
  the address the guard checked and the proxy dialed, `"ip:port"`. Together
  with `host` it is the audit trail against DNS rebinding — "asked for the
  name, dialed this address" — because under the guard the proxy never dials
  an address it did not check. Absent when the guard is off and on rows that
  dialed nothing.
- `httpMethod`, `httpPath` — the HTTP verb and query-stripped,
  percent-decoded path, so a request no parser recognized is still
  identifiable. On an opaque tunnel `httpMethod` is `CONNECT` and `httpPath`
  is absent. Note a secret in a path *segment* (a bot token, a webhook path)
  does survive on this unauthenticated surface — worth knowing before
  exporting events off-box.
- `subagent` — present on rows from a Claude Code subagent or agent-team
  teammate: the `X-Claude-Code-Agent-Id` the request carried. For a subagent it
  is the same id that names its files under
  `~/.claude/projects/<dir>/<session>/subagents/agent-<id>.*`. Absent on the
  main conversation's rows and on every other client's, and absent too when
  the value was not usable (a control character, over 128 bytes), which is
  refused rather than cleaned up. The row is still filed in its parent
  conversation's session. **This key names an agent; it does not say the row
  is a subagent's.** `inference.agentRole` says that, and on a teammate's row
  it is `main` while `subagent` is set.
- `parentSubagent` — the agent that started the one `subagent` names, from
  `X-Claude-Code-Parent-Agent-Id`, under the same rules. Claude Code sends it
  only when the spawner was itself an agent, so on a row that names a
  `subagent` an absent `parentSubagent` means the conversation started it. The
  two keys together give a row's place in the session's tree of agents.

## Invocation action vocabulary

Every plugin emits one of these 5 action values per invocation, so operators
can scan a timeline without memorizing plugin-specific verbs:

| `action` | Meaning | Example |
|---|---|---|
| `allow` | Gate plugin permitted the request | jwt-validation on valid token |
| `deny` | Gate plugin rejected the request; pipeline stops | jwt-validation on bad token, token-exchange on IdP failure |
| `skip` | Plugin ran but didn't act on this message | jwt-validation on a bypass path; parser whose body didn't match |
| `modify` | Plugin mutated the message | token-exchange replaced the Authorization header |
| `observe` | Plugin attached diagnostic data without changing flow | a2a-parser, mcp-parser, inference-parser when they match |

Use `reason` to discriminate within an action — `skip/path_bypass` vs
`skip/no_matching_route` tell different stories at the detail-pane level but
both scan as "skip" in the at-a-glance timeline.

agentop's ACTION column adds two values that are rendering, not plugin output:
`—` when nothing acted, and `tunnel` for an opaque CONNECT. Neither is ever
emitted by a plugin, and neither is a verdict on the request.

> **Producer-side contract:** the authoritative definition of the 5-value
> vocabulary, the `Invocation` struct fields, and which diagnostic fields each
> plugin type populates lives in
> [`plugin-reference.md`](plugin-reference.md#emitting-session-events). This
> table is the consumer-side summary.

## Gotchas

- **Denied requests** (401 / 503) land as `phase: "denied"` events when at
  least one pipeline plugin appended an Invocation before rejecting. If you're
  debugging an unauthorized-access pattern, the default-session bucket
  (`GET /v1/sessions/default`) is where denial events aggregate.
- **Disabling:** set `session.enabled: false` to turn off the store (and
  implicitly the API). The session archive is separate — `session.archive.enabled: false`
  stops it without touching the in-memory store, and turning the store off
  with the archive explicitly on is refused at load. Setting
  `listener.session_api_addr: ""` alone is not supported as a selective
  disable; if you need store-on-API-off, raise an issue.
- **Memory:** the store keeps every event a session produces by default —
  `session.max_events` is unset and `session.ttl` defaults to never. What
  bounds memory by default is `session.max_sessions` (100, oldest-used evicted
  first). See [`laptop-service.md`](laptop-service.md) for the retention
  story and the archive's bounds.
