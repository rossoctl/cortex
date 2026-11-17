# Export sessions for analysis

`GET /v1/export` writes the session archive as NDJSON. One line per archived event. It is
on the [Session Events API](session-api.md).

A month of traffic is a file a laptop can process. 30 days measured about 12 MB on the
install used for development. The same events as full snapshots are multiple gigabytes.

`agentop export` is the same endpoint as a command:

```sh
agentop export --out export.ndjson
agentop export --since 168h          # only the last week
curl -s localhost:47601/v1/export    # the same stream, raw
curl -s "localhost:47601/v1/export?since=168h" | jq -c 'select(.model)'
```

Use it to post-process outside the tool. It answers questions that need a month of
records: latency percentiles, token spend per model, agent-version adoption, cache-hit
rates, tool failure rates. A live TUI cannot answer these.
[`agentop cost`](pricing.md) answers a few of them inside the tool. This endpoint is the
layer behind anything else.

For a step-by-step walkthrough, see
[`session-export-guide.md`](session-export-guide.md). This page is the reference.

## What a record is

One line is one event. A record is content-free by construction. It is an allowlist of
about 30 scalars projected from the event. It is never the event itself.

```json
{"schema":1,"sessionId":"45fa31b2-…","seq":2077,"requestId":"a0-9e37cd",
 "at":"2026-11-05T15:52:26.102885Z","direction":"outbound","phase":"response",
 "host":"litellm.cb.ete.res.ibm.com","httpMethod":"POST","statusCode":200,
 "durationMs":4279,"ttfbMs":3182,"bytesDown":3142,"client":"claude-code/2.1.270",
 "agentRole":"main","model":"rits/zai-org/glm-5-3","servedModel":"hosted_vllm/zai-org/GLM-5.3",
 "stream":true,"streamedResponse":true,"finishReason":"end_turn",
 "inputTokens":1249,"cacheReadTokens":196096,"outputTokens":49,
 "messageCount":385,"toolCount":24,"toolCallCount":11}
```

**A record never carries** message content, completions, tool-call names, tool-call
arguments, tool results, MCP params, request paths, headers or authenticated identity.

The field list is an allowlist. A field added to the event later does not reach an export
on its own. Someone must add it to the projection first. This is what makes the endpoint
safe to pipe off the machine that must keep the full events.

**Content-free is not detail-free.** A record carries hosts, models, client identifiers,
session identifiers and timestamps. These describe where an agent talked, and how much.
An external analysis wants exactly this. Review a file before it leaves your hands.

`client` needs its own warning. Cortex parses it from the User-Agent. It is a
cost-attribution key and a display key. **It is never an authorization subject.** An
external collector is where someone will treat it as identity.

## Reading rules

- **An absent number means "not recorded". It never means zero.** Cortex omits a numeric
  field when the value is unknown. Do not render an absent figure as 0. That reports an
  unmeasured request as one that took no time and sent no bytes.
- **Order by `at`, then `seq`.** The walk emits the most recently updated session first.
  Within a session it emits the newest event first. A consumer sorts its own timeline.
  `seq` is unique within one run of a session, not forever. A session re-created under the
  same id restarts at 1.
- **Never subtract timestamps to get a latency.** Same-instant events are common on a fast
  local proxy. Use `durationMs` for the measurement. Use `durationMs` − `ttfbMs` for the
  time the body took.
- **Token counts are the split only.** `inputTokens` is the uncached prompt. To get a
  prompt total, add `inputTokens` + `cacheReadTokens` + `cacheWriteTokens` yourself. No
  aggregate field exists, and that is deliberate. A total next to its own components makes
  the first consumer count every cached token twice.
- **No tool-result count exists.** Every request re-sends the whole conversation. A sum of
  raw tool results therefore overcounts by about 272x on real traffic. `toolCallCount` is the
  count of calls the model emitted. It is exported, and this problem does not affect it.

The doc comments on `core/sessionexport/record.go` are the full field list. They carry
these rules per field. This page is the summary.

## Selection

| Parameter | Meaning |
|---|---|
| *(none)* | Every archived session, every event |
| `?session=<id>` | Only the named sessions. Repeatable: `&session=a&session=b`. An unknown id is a 400 (see below) |
| `?since=<duration>` | Only events newer than that far back. Use `168h` (a Go duration) or `7d` (a day count) |

Cortex parses both parameters before it writes the first byte. A bad value is therefore a
real 400, not a truncated stream. A repeated `since` is also a 400.

An unknown `?session=` is a 400 that names the archived sessions. The list is bounded in
bytes, not in ids. This keeps the answer inside what a client reads of a 400 body, however
long the ids are. The refusal lists as many ids as fit, then a count of the rest.

`since` floors on **event time**, not on session age. Example: a session is a month old,
and it has traffic from this morning. `?since=24h` carries this morning's events. It
excludes the older ones. Cortex skips a session that it last touched before the floor, and
does not read its segments.

## `agentop export`

The command wraps the endpoint for the shell. The parameters above become flags. The
output goes to a file or to a pipe. A summary line goes to stderr, for example
`3 records, 231B → (stdout)`. The summary never mixes into the stream.

| Flag | Meaning |
|---|---|
| `--out FILE` | Write to a file rather than stdout. An existing file is refused without `--force` |
| `--session ID` | Repeatable and comma-separated: `--session a --session b` or `--session a,b` |
| `--since DUR` | `168h` or `7d`, as on the endpoint |
| `--endpoint URL` | Ask a specific proxy rather than the local one |

A refused query fails before the command creates the output file. An unknown `--session`
and a malformed `--since` are both refused this way. The command prints the proxy's own
words. The command opens the file only after the proxy answers, so a typo costs nothing.

The byte budget is what keeps those words readable. The command reads 512 bytes of a
refusal. A count-based cap would overrun that on long ids. An older proxy still caps by id
count. The command then prints the `curl` that lists the archived sessions, so a typo
always has an answer.

## What it does not carry, and why

- **Events that are still only in memory.** This endpoint is a backfill of the durable
  archive. It is at most one tick behind the open segments, which is the same gap the
  paging endpoints document. A session with nothing flushed yet does not appear. For the
  live tail, use `GET /v1/events`.
- **A session "incarnation" key.** A client asserts its own session id and can reuse it.
  The id `default` can cover weeks of unrelated work. A consumer that wants "one sitting"
  must infer it from gaps in `at`
  ([#1739](https://github.com/rossoctl/cortex/issues/1739)).
- **Full-fidelity capture.** The exact request and response bytes stay on
  `GET /v1/sessions/{id}/events/{seq}?raw=1`.
  [`cortex-session-dump`](session-dump.md) keeps whole sessions as files. Use the dump
  when you want the content. Use the export when you want the measurements. A dump and an
  export of the same traffic answer different questions. Neither one replaces the other.

## Truncation, not corruption

A walk that fails past its first record **resets the connection**. The status line is
already gone by then, and a 200 cannot be taken back.

NDJSON keeps every complete line valid, so a consumer keeps every record that arrived. The
read ends in an error, not in a clean end of body. A truncated export therefore cannot be
mistaken for a whole one.

`agentop export` turns that error into a non-zero exit. It also reports that the file holds
every complete record written before the failure. A `curl > file` leaves the partial file
and returns a non-zero exit of its own. The server logs the failure as
`sessionapi: export truncated`.

A failure before any record answers a status instead:

- A refused query is a 400. This covers an unknown session and a bad `since`.
- An archive whose segments cannot be opened or read is a 500.

Cortex tolerates a segment whose *content* is damaged. It serves the segment up to its
readable prefix, as the paging endpoints do. A segment that is unreadable throughout
contributes nothing. A partly readable segment contributes what it has. An export is
therefore not a way to detect a corrupt archive
([#1855](https://github.com/rossoctl/cortex/issues/1855)).
