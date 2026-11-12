# Exporting sessions for analysis

`GET /v1/export` on the [Session Events API](session-api.md) writes the session archive
as **NDJSON, one line per archived event** — a month of traffic as a file a laptop can
process (~12 MB for 30 days on the install it was measured on), where the same events
as full snapshots are multi-gigabyte.

```sh
curl -s localhost:47601/v1/export > export.ndjson
curl -s "localhost:47601/v1/export?since=168h" | jq -c 'select(.model)'
```

What it is for: post-processing outside the tool. Latency percentiles, token spend by
model, agent-version adoption, cache-hit rates, tool failure rates — the questions a
month of records answers and a live TUI does not. [`agentop cost`](pricing.md) answers a
few of them inside the tool; this is the layer behind anything else.

## What a record is

One line, one event, content-free by construction — an allowlist of ~30 scalars
projected from the event, never the event itself:

```json
{"schema":1,"sessionId":"45fa31b2-…","seq":2077,"requestId":"a0-9e37cd",
 "at":"2026-11-05T10:52:26.102885-05:00","direction":"outbound","phase":"response",
 "host":"litellm.cb.ete.res.ibm.com","httpMethod":"POST","statusCode":200,
 "durationMs":4279,"ttfbMs":3182,"bytesDown":3142,"client":"claude-code/2.1.270",
 "agentRole":"main","model":"rits/zai-org/glm-5-3","servedModel":"hosted_vllm/zai-org/GLM-5.3",
 "stream":true,"streamedResponse":true,"finishReason":"end_turn",
 "promptTokens":197345,"completionTokens":49,"cacheReadTokens":196096,
 "messageCount":385,"toolCount":24}
```

**Never in a record:** message content, completions, tool-call names or arguments,
tool results, MCP params, request paths, headers, authenticated identity. The field list
is an allowlist, so a field added to the event later reaches an export only when someone
adds it to the projection on purpose — that is what makes this endpoint safe to pipe
off the machine where the full events must stay.

`client` deserves its own line: it is parsed from the User-Agent, **a cost-attribution
and display key, never an authorization subject**. An external collector is exactly where
someone will be tempted to treat it as identity.

## Reading rules

- **Zero means "not recorded", never a real zero.** Every numeric field is omitted when
  unknown. A consumer rendering an absent figure as 0 reports an unmeasured request as
  one that took no time and sent no bytes.
- **Order by `at`, then `seq`.** The walk emits sessions most recently updated first,
  events newest first within a session — a consumer sorts its own timeline. `seq` is
  unique within one incarnation of a session, not forever: a session re-created under
  the same id restarts at 1.
- **Never subtract timestamps for a latency.** Same-instant events are common on a fast
  local proxy. `durationMs` is the measurement; `durationMs` − `ttfbMs` is how long the
  body took.
- **Token counts are the split only** — `inputTokens` is the uncached prompt; add
  `inputTokens` + `cacheReadTokens` + `cacheWriteTokens` yourself if you want a prompt
  total. There is no aggregate field, deliberately: a total beside its own components
  gets every cached token counted twice by the first consumer to mix them.
- **No tool-result count exists at all.** Every request re-sends the whole conversation,
  so summing raw tool results overcounts ~272x on real traffic. `toolCallCount` (calls
  the model emitted) is exported and is not affected.

The full field list with these rules on each field is the doc comments on
`core/sessionexport/record.go` — this file summarizes; that is the reference.

## Selection

| Parameter | Meaning |
|---|---|
| *(none)* | Every archived session, every event |
| `?session=<id>` | Only the named sessions; repeatable (`&session=a&session=b`). An unknown id is a 400 naming the archived ones |
| `?since=<duration>` | Only events newer than that far back — `168h` (a Go duration) or `7d` (a day count). Parsed before the first byte, so a bad value is a real 400 |

`since` floors on **event time**, not session age: a month-old session with traffic this
morning is carried by `?since=24h`, whole. Sessions the archive last touched before the
floor are skipped without reading their segments.

## What it does not carry, and why

- **Events still in memory only.** This is a backfill of the durable archive, at most a
  tick behind the open segments — the same gap the paging endpoints document. A session
  with nothing flushed yet does not appear. For the live tail, `GET /v1/events`.
- **A session "incarnation" key.** A session id is client-asserted and reused: the id
  `default` can span weeks of unrelated work. A consumer wanting "one sitting" infers it
  from gaps in `at` for now ([#1739](https://github.com/rossoctl/cortex/issues/1739)).
- **Full-fidelity capture.** The exact request and response bytes stay on
  `GET /v1/sessions/{id}/events/{seq}?raw=1`, and [`cortex-session-dump`](session-dump.md)
  is the tool for keeping whole sessions as files. Use that when you want the content;
  use this when you want the measurements — a dump and an export of the same traffic
  answer different questions, and neither substitutes for the other.

## Truncation, not corruption

A walk that fails past its first record ends the response there, because the status line
is already gone — and NDJSON keeps every complete line valid in a truncated file, so a
partial export is still a processable one. A failure before any record (unknown session,
bad `since`) answers a proper 400.
