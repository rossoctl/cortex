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
  "at": "2026-10-15T20:01:02Z", "msgs": 12}, "inferenceHost": "api.anthropic.com"
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
  upstream, that is how a hung one usually ends.
- `requestedHost` — present only when a plugin redirected the request: the
  host the client asked for. `host` is where the request actually went, and
  usage, the cost ledger and pricing all follow `host`.
- `inference.requestedModel` — present only when a plugin changed the model:
  the model the client asked for. `inference.model` is the model the request
  was sent for, which settlement prices.
- `bytesUp`, `bytesDown` — **body** bytes, one direction per row: a request row
  carries `bytesUp`, the body as forwarded (so a rewrite is reflected); a
  response row carries `bytesDown`, the body as it came back from the
  destination. Headers are not counted. A tunnel close is the one row carrying
  both. **Zero means the listener counted nothing, not that the body was
  empty** — every listener buffers bodies only when a plugin asks, so an
  unbuffered request reports zero.
- `tunnel`, `tunnelReason` — an opaque CONNECT (or transparent-redirect) tunnel
  records two rows sharing a `requestId`: the open (`phase: "request"`,
  `tunnelReason` saying why the bytes stayed opaque) and the close (`phase:
  "response"`), carrying the CONNECT's own status, the duration, and the bytes
  each way. It is not the destination's status: that travels inside the
  client's end-to-end TLS. Tunnel rows are kept out of `/v1/usage`: a tunnel's
  lifetime is not a request latency.
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
