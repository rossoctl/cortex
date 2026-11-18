# Export sessions for analysis

`GET /v1/export` writes the session archive as NDJSON. One line per archived event. It is
on the [Session Events API](session-api.md). `agentop export` is the same endpoint as a
command.

A month of traffic is a file a laptop can process. 30 days measured about 12 MB on the
install used for development. The same events as full snapshots are multiple gigabytes.

Use it to post-process outside the tool. It answers questions that need a month of
records: latency percentiles, token spend per model, agent-version adoption, cache-hit
rates, tool failure rates. A live TUI cannot answer these.
[`agentop cost`](pricing.md) answers a few of them inside the tool. This endpoint is the
layer behind anything else.

The commands and outputs on this page come from a served archive.

**Terms used on this page.** A **record** is one line of the export. An **event** is one
request or response that Cortex captured. One event becomes one record. The **store** is
what Cortex holds in memory. The **archive** is what Cortex has written to disk.

- [Step 1: confirm the proxy has an archive](#step-1-confirm-the-proxy-has-an-archive)
- [Step 2: list what the archive holds](#step-2-list-what-the-archive-holds)
  - [Find the id of the session you want](#find-the-id-of-the-session-you-want)
- [Step 3: export it](#step-3-export-it)
- [Step 4: select less than everything](#step-4-select-less-than-everything)
- [Answer a question with the export](#answer-a-question-with-the-export)
- [Three traps](#three-traps)
- [Export from a cluster sidecar](#export-from-a-cluster-sidecar)
- [Use the export in a script](#use-the-export-in-a-script)
- [Read the refusals](#read-the-refusals)
- [What a record is](#what-a-record-is)
- [Reading rules](#reading-rules)
- [Selection](#selection)
- [`agentop export`](#agentop-export)
- [What it does not carry, and why](#what-it-does-not-carry-and-why)
- [Truncation, not corruption](#truncation-not-corruption)
- [See also](#see-also)

## Step 1: confirm the proxy has an archive

The export reads the archive on disk. It does not read the store in memory. A laptop
install has an archive. Most other deployments do not.

Ask the proxy what its archive holds:

```sh
curl -s 'http://127.0.0.1:47601/v1/sessions?archived=true' | jq '.archive'
```

```
{
  "bytes": 493,
  "maxBytes": 2147483648,
  "retentionDays": 30
}
```

`retentionDays` is how long the archive keeps a session. `maxBytes` is its size limit.

If the reply is `null`, this proxy has no archive. Go to
[Export from a cluster sidecar](#export-from-a-cluster-sidecar).

## Step 2: list what the archive holds

```sh
curl -s 'http://127.0.0.1:47601/v1/sessions?archived=true' \
  | jq -r '.sessions[] | [.id, .eventCount, (.resident != false)] | @tsv'
```

```
old-session      3   false
today-session    4   false
```

The third column is `resident`. Test it with `!= false`, not with `// true`: in `jq` the
`//` operator treats `false` as absent, so `// true` would report every session as
resident.

The values mean:

- **`true`**, or the field is absent: the store still holds this session. You can read it
  in the `agentop` viewer.
- **`false`**: the store has dropped this session, and the archive supplied this row. The
  viewer no longer shows it. The export still carries it.

Cortex drops a session from the store when the store reaches its session limit, or when
the session expires. The archive keeps the session after that. This is the reason to
export: the archive holds history that memory has lost.

### Find the id of the session you want

`--session` takes a **session id**, and it matches that id exactly. It does not match a
title, and it does not match a prefix.

A session id is the value a client asserted. It is often a UUID. It can also be a plain
word, such as `default`.

An id alone does not tell you which session it is. List the ids with their titles:

```sh
curl -s 'http://localhost:47601/v1/sessions?archived=true' \
  | jq -r '.sessions[] | "\(.id)  \((.title // "(untitled)")[0:60])"'
```

```
849102c5-e59f-404c-8b2e-3dde35e052bb  please address the review comments
59046505-b6f3-43f5-a2ef-9071c9a847a2  https://github.com/rossoctl/cortex/pull/1854
24e624cb-b578-42c4-9c8b-9bfbe29341ab  please address Alek's comments
17051bcc-5ae8-40eb-a975-48488242b93d  Our rossoctl org has several repos
0e2f9a41-7c3d-4b18-9f52-6a1e4d7b2c90  (untitled)
```

A session has no title until its events carry one: a `/rename`, a user query, or ordinary
prose. Tunnel-only traffic and utility clients therefore stay untitled. Keep those rows —
they are the ones you most need an id for.

Copy the id from the first column.

> **A title is user content. An export is not.**
>
> Cortex derives a title from the session's own events: a `/rename`, else a user query,
> else ordinary conversation. A title therefore contains prompt text.
>
> The export excludes prompt text by design. The command above does not. Use it in your
> own terminal. Do not paste its output where you would paste an export.

The export carries no title. A record identifies its session by `sessionId` only. To
label a report, join the ids back to the titles yourself.

## Step 3: export it

```sh
agentop export --out usage.ndjson
```

```
  7 records, 1.6kB → usage.ndjson
```

The summary goes to stderr. The records go to stdout. The two never mix, because stdout
can be the export itself:

```sh
agentop export | jq -r '.model // empty' | sort | uniq -c
```

Each record is one event, reduced to about 30 numbers and short strings. This record
comes from a larger install than the 7-record archive above:

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

Look at what the record does not have. The source event held 385 messages of
conversation. The record has `"messageCount":385` in their place. The export carries no
prompts, no completions and no tool arguments. This is why you can share an export where
you cannot share the full events.

An export is not anonymous. Read
[What a record is](#what-a-record-is) before you send it somewhere.

The command refuses to overwrite a file:

```sh
$ agentop export --out usage.ndjson
agentop export: usage.ndjson already exists; --force overwrites it
```

Add `--force` to overwrite it.

## Step 4: select less than everything

Select by time:

```sh
agentop export --since 7d
```

Select by session id. Get the id from
[Step 2](#find-the-id-of-the-session-you-want):

```sh
agentop export --session 849102c5-e59f-404c-8b2e-3dde35e052bb
```

Select more than one session. Repeat the flag, or separate the ids with commas:

```sh
agentop export --session 849102c5-… --session 59046505-…
agentop export --session 849102c5-…,59046505-…
```

An id that the archive does not hold is a refusal, not an empty result. See
[Read the refusals](#read-the-refusals).

`--since` accepts a day count (`7d`, `30d`) or a Go duration (`168h`, `90m`). It measures
back from now.

`--since` selects events, not sessions:

```sh
$ agentop export --since 7d
  4 records, 1.1kB → (stdout)
```

The archive holds 7 records. One session is 8 days old, so `--since 7d` excludes it.

A session can appear in an export with fewer events than it has. Example: a session
started a month ago, and it ran again this morning. `--since 24h` carries this morning's
events from it. `--since 24h` excludes its older events.

## Answer a question with the export

The export is NDJSON. One record per line. `jq` reads it directly.

**Count tokens per model.** The `// 0` matters — see [Three traps](#three-traps):

```sh
agentop export --since 30d \
  | jq -r 'select(.model) | [.model, (.inputTokens // 0), (.outputTokens // 0)] | @tsv' \
  | awk -F'\t' '{i[$1]+=$2; o[$1]+=$3}
                END {for (m in i) printf "%-18s in=%-8d out=%d\n", m, i[m], o[m]}'
```

```
claude-opus-5      in=4860     out=198
```

**Count requests per host:**

```sh
agentop export | jq -r '.host // empty' | sort | uniq -c | sort -rn
```

```
   4 api.anthropic.com
   3 api.openai.com
```

**Find the slow requests:**

```sh
agentop export --since 7d | jq -r '.durationMs // empty' | sort -n \
  | awk '{a[NR]=$1} END {printf "n=%d p50=%dms p95=%dms max=%dms\n",
          NR, a[int((NR-1)*0.5)+1], a[int((NR-1)*0.95)+1], a[NR]}'
```

```
n=4 p50=1237ms p95=1274ms max=1311ms
```

**Query it with SQL.** DuckDB reads NDJSON directly:

```sh
agentop export --since 30d --out usage.ndjson
```

```sh
duckdb -c "
  SELECT model, count(*) AS calls, sum(inputTokens) AS in_tok, median(durationMs) AS p50
  FROM read_json_auto('usage.ndjson')
  WHERE model IS NOT NULL
  GROUP BY model ORDER BY calls DESC"
```

**See every request in one session, with its token split.** This is the per-request table,
from one request and with no temporary files:

```sh
ID=849102c5-e59f-404c-8b2e-3dde35e052bb

curl -s "http://localhost:47601/v1/export?session=$ID" \
  | jq -r 'select(.phase=="response" and .inputTokens)
           | [.seq, .durationMs, .ttfbMs, .inputTokens, .cacheReadTokens,
              .cacheWriteTokens, .outputTokens, .toolCallCount]
           | map(. // "-") | @tsv' \
  | (printf 'SEQ\tMS\tTTFB\tINPUT\tCACHE_R\tCACHE_W\tOUTPUT\tTOOL_CALLS\n'; cat) \
  | column -t
```

```
SEQ  MS    TTFB  INPUT  CACHE_R  CACHE_W  OUTPUT  TOOL_CALLS
6    4500  1020  1280   191800   520      74      2
4    4250  960   1240   190900   510      67      2
2    4000  900   1200   190000   500      60      2
```

Select `phase=="response"`. A request-phase record carries no token counts, because the
model reports usage on the response.

`map(. // "-")` is not cosmetic. An absent field writes an empty cell, and `column`
merges adjacent blanks, so every later value moves one column left under the wrong
header — with no error. A response with no `cacheWriteTokens` is ordinary, so this
happens on real data. `-` makes the absence visible, which is also what Trap 1 asks for.

`printf` rather than `echo -e`: under `dash`, which is `/bin/sh` on Debian and Ubuntu,
`echo -e` prints a literal `-e`.

Remember [Trap 1](#three-traps): the `.inputTokens` test is what keeps non-inference
records out of the table.

**Do not use the timeline for token counts.** `GET /v1/sessions/{id}/timeline` carries
`outputTokens` and `liveTokens` only. It has no `inputTokens` and no cache counts. The
export has all four.

Use each endpoint for what it holds:

| You want | Use | Why |
|---|---|---|
| Token counts, latency, status | the export | It has all four token fields |
| Tool names and spans | the timeline | The export has `toolCallCount`, never names |
| Both at once | join them on `seq` | No single endpoint has both |

The timeline's `tools` array holds the tool calls that each model response asked for. A
tool's `seq` is its **response's** `seq`, so join a tool to the export's
`phase=="response"` record.

A turn's `seq` is its **request's** seq, not its response's, and the two are **not
adjacent**. `seq` counts every event in the session, so tunnel rows, other hosts and the
side calls the timeline drops take the numbers in between. A join on a turn's `seq`, or on
the export's request record, finds no tool.

To get from a request record to its response, match on `requestId`. That is the stable
key, and it is what the timeline itself pairs on.

**Load it into pandas:**

```python
import pandas as pd

df = pd.read_json("usage.ndjson", lines=True)
df["at"] = pd.to_datetime(df["at"], utc=True, format="ISO8601")
# Do not fill with 0. An absent field means "not recorded", and min_count=1 keeps an
# all-absent group as NaN instead of reporting it as zero.
print(df.groupby("model")[["inputTokens", "outputTokens"]].sum(min_count=1))
```

## Three traps

Each trap gives you a wrong answer. None of them gives you an error. Read this section
before you report a number from an export.

### Trap 1: an absent field is not a zero

Cortex omits a field that it did not record. `jq` then reads it as `null`. pandas reads it
as `NaN`. Neither reads it as `0`.

Three of the seven records in this archive have no `inputTokens` field. They are not
inference calls.

This is the careless average:

```sh
$ agentop export | jq -s 'map(.inputTokens) | add / length'
694.2857142857143
```

This is the correct average:

```sh
$ agentop export | jq -s 'map(select(.inputTokens)) | (map(.inputTokens) | add) / length'
1215
```

The careless version is 43% low on this small archive. On a real archive most events are
not inference calls, so the error is larger.

A real `0` and an unrecorded field look the same in this format. To tell them apart, test
for the field instead of its value:

```sh
agentop export | jq -r 'select(has("inputTokens")) | .seq'
```

### Trap 2: sort by `at`, then by `seq`

`seq` counts from 1 within one run of a session. Cortex restarts the count at 1 if a
session is dropped and then created again under the same id. No field tells you that this
happened.

`at` alone is also not enough. A fast local proxy produces events that share a timestamp.
`seq` breaks the tie inside one session.

**Do not sort `at` as a string.** Go trims trailing zeros from the fraction. So
`…:11Z`, `…:11.8Z` and `…:11.84114Z` are the same second in the wrong byte order. A
string sort returns them backwards. That is exactly the tie `seq` exists to break.

Sort on the parsed value, and use `seq` as the tiebreak:

`at` always ends in `Z`, but the fraction width varies, and `fromdateiso8601` rejects a
fraction. Strip it for the seconds, then add it back:

```sh
agentop export \
  | jq -rs 'def ts(s): (s | sub("\\.[0-9]+Z$";"Z") | fromdateiso8601)
                      + ((s | capture("\\.(?<f>[0-9]+)Z$") | .f // "0"
                             | "0." + . | tonumber) // 0);
            sort_by(ts(.at), .seq)[]
            | [.at, (.seq|tostring), .sessionId] | @tsv'
```

```
2026-11-07T23:34:12Z           1   default
2026-11-07T23:34:12.102885Z    3   default
2026-11-07T23:34:12.8Z         2   default
```

Note the output order: `12Z`, then `12.102885Z`, then `12.8Z`. A string sort returns
these as 3, 2, 1.

### Trap 3: a session id is not a unit of work

A client can reuse one session id for as long as it likes. The id `default` can cover
weeks of unrelated work.

Do not group by `sessionId` and call the result a task. The average covers unrelated work.

Cortex has no field for "one sitting" yet
([#1739](https://github.com/rossoctl/cortex/issues/1739)). Until it does, find the gaps in
`at` yourself. Each line of output below starts a new sitting:

```sh
# A gap of more than 30 minutes starts a new sitting.
agentop export --session default \
  | jq -rs 'def ts(s): (s | sub("\\.[0-9]+Z$";"Z") | fromdateiso8601)
                      + ((s | capture("\\.(?<f>[0-9]+)Z$") | .f // "0"
                             | "0." + . | tonumber) // 0);
            sort_by(ts(.at)) | . as $r | range(1; length) as $i
            | (ts($r[$i].at) - ts($r[$i-1].at)) as $gap
            | select($gap > 1800)
            | "\($r[$i].at)  gap=\(($gap/3600)|floor)h"'
```

```
2026-11-16T23:08:45.84114Z  gap=191h
```

## Export from a cluster sidecar

An injected sidecar normally runs no archive. The endpoint then answers 404:

```sh
$ agentop export --endpoint http://127.0.0.1:9094
agentop export: /v1/export: apiclient: not found
  this proxy has no session archive, or predates /v1/export (`agentop update`); a cluster sidecar never runs one
```

The command exits 1. Over HTTP the same refusal looks like this:

```
HTTP/1.1 404 Not Found
Content-Type: application/json

{"error":"session archive not enabled"}
```

That message gives two possible causes. They need different actions:

- The proxy runs no archive. Set `session.archive.dir` to give it one.
- The proxy is older than the endpoint. Upgrade the proxy.

To get history out of a cluster that has no archive, use
[`cortex-session-dump`](session-dump.md). It reads the store in memory.

Where the sidecar does have an archive, forward the port first:

```sh
kubectl port-forward -n team1 deploy/weather-agent 9094:9094 &
```

Then export through it:

```sh
agentop export --endpoint http://127.0.0.1:9094 --since 24h --out team1.ndjson
```

If you have no `agentop` binary, call the endpoint directly:

```sh
curl -s 'http://127.0.0.1:9094/v1/export?since=24h' > team1.ndjson
curl -s 'http://127.0.0.1:9094/v1/export?session=abc&session=def' > two.ndjson
```

The session API has no authentication. Bind it to loopback, or reach it through a port
forward. Never put it behind an ingress.

## Use the export in a script

Read the exit code. It is the contract:

| Exit | Meaning | What to do |
|---|---|---|
| `0` | The export is complete. | Use the output. |
| `1` | The request failed, or the walk stopped early. | Discard the output. It is incomplete. |
| `2` | A usage error: a bad flag, an empty filter, a stray argument. | Fix the command. Nothing was requested. |

These are the measured codes:

```
  --since 7d             exit=0
  --session nope         exit=1
  --since bogus          exit=1
  --since ''             exit=2
  extra-positional       exit=2
```

**A short export always exits non-zero.** The server resets the connection if its walk
fails part way through. The client then reports a read error:

```
agentop export: unexpected EOF
  the walk failed part-way; usage.ndjson holds every complete record before it
```

Every complete record already written stays valid. You keep what arrived. You cannot
mistake it for the whole archive.

Check the exit code. Do not check that the file is non-empty:

```sh
set -euo pipefail

if ! agentop export --since 7d --out "$OUT" --force; then
  echo "export incomplete; not publishing $OUT" >&2
  exit 1
fi
```

**An empty filter is a refusal, not a wildcard.** This matters when a shell builds the
arguments:

```sh
$ WINDOW=""
$ agentop export --since "$WINDOW"
agentop export: --since needs a value, such as 168h or 7d; omit it to export every event
```

The command exits 2. It sends no request. An empty `--since` could mean "no floor", and
that reading would export the whole archive from a broken variable. `--session ""` is
refused in the same way.

To export everything, omit the flag.

A large archive takes time to walk. The command waits up to ten minutes.

## Read the refusals

**An unknown session.** The refusal names the sessions the archive does hold:

```
$ agentop export --session nope
agentop export: /v1/export?session=nope: apiclient: bad request: sessionexport: no archived session nope (archived: old-session, today-session)
```

The proxy refuses before it writes any record. A typo never creates your `--out` file. A
typo never truncates it either.

A wide archive has more ids than the refusal can carry. The refusal then lists what fits
and ends with a count of the rest. It always names at least one id.

If the refusal is too long for the command to read, the command tells you how to list the
ids instead:

```
  the proxy did not name the sessions it holds; list them with:
    curl -s 'http://127.0.0.1:47601/v1/sessions?archived=true'
```

**A malformed `--since`.** The refusal names the accepted forms:

```
$ agentop export --session today-session --since bogus
agentop export: /v1/export?session=today-session&since=bogus: apiclient: bad request: since must be a duration or Nd, e.g. 168h or 7d
```

This refusal says nothing about sessions, because `--since` is the problem.

The accepted forms are in the [flag table](#agentop-export). Cortex refuses zero and
negative values. It also refuses a sign, so `+7d` is invalid.

**A malformed query.** A query that Cortex cannot decode is a 400:

```sh
$ curl -s -i 'http://127.0.0.1:47601/v1/export?session=%GG' | head -2
HTTP/1.1 400 Bad Request
Content-Type: application/json
```

Cortex never treats a broken filter as an absent filter. `agentop export` cannot produce
this error, because it escapes its own query. A hand-written `curl` can.

## What a record is

One line is one event. A record is content-free by construction. It is an allowlist of
about 30 scalars projected from the event. It is never the event itself.
[Step 3](#step-3-export-it) shows one record.

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

- **An absent number means "not recorded". It never means zero.** See
  [Trap 1](#trap-1-an-absent-field-is-not-a-zero).
- **Order by `at`, then `seq`.** The walk emits the newest session and the newest event
  first. See [Trap 2](#trap-2-sort-by-at-then-by-seq).
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

`since` floors on **event time**, not on session age
([Step 4](#step-4-select-less-than-everything) has an example). Cortex skips a session that
it last touched before the floor, and does not read its segments.

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
- **A session "incarnation" key.** See
  [Trap 3](#trap-3-a-session-id-is-not-a-unit-of-work).
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

## See also

- [`session-api.md`](session-api.md) — every endpoint, `GET /v1/export` included
- [`session-dump.md`](session-dump.md) — use this when you want the content: prompts,
  completions and tool results
- [`pricing.md`](pricing.md) — use `agentop cost` when the question is spend
