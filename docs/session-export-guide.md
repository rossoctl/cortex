# Export your session data: a walkthrough

[`session-export.md`](session-export.md) is the reference. It tells you what each field
means. This page tells you how to get an export and how to read it correctly.

Every command and every output on this page was run against a served archive.

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

## Step 1: confirm the proxy has an archive

The export reads the archive on disk. It does not read the store in memory. A laptop
install has an archive. Most other deployments do not.

Ask the proxy what its archive holds:

```sh
curl -s 'http://127.0.0.1:47601/v1/sessions?archived=true' | jq '.archive'
```

```json
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
  | jq -r '.sessions[] | select(.title) | "\(.id)  \(.title[0:60])"'
```

```
849102c5-e59f-404c-8b2e-3dde35e052bb  please address the review comments
59046505-b6f3-43f5-a2ef-9071c9a847a2  https://github.com/rossoctl/cortex/pull/1854
24e624cb-b578-42c4-9c8b-9bfbe29341ab  please address Alek's comments
17051bcc-5ae8-40eb-a975-48488242b93d  Our rossoctl org has several repos
```

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

Each record is one event, reduced to about 30 numbers and short strings:

```json
{
  "schema": 1,
  "sessionId": "today-session",
  "seq": 4,
  "at": "2026-11-16T23:08:45.84114Z",
  "direction": "outbound",
  "phase": "request",
  "host": "api.anthropic.com",
  "httpMethod": "POST",
  "durationMs": 1311,
  "model": "claude-opus-5",
  "inputTokens": 1230,
  "outputTokens": 51,
  "messageCount": 1
}
```

Look at what the record does not have. The source event held this message content:
`"SECRET-PROMPT-DO-NOT-EXPORT"`. The record has `"messageCount": 1` in its place. The
export carries no prompts, no completions and no tool arguments. This is why you can
share an export where you cannot share the full events.

An export is not anonymous. It keeps hosts, models, session ids and timestamps. Together
these show who called what, and when. Read an export before you send it somewhere.

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
              .cacheWriteTokens, .outputTokens, .toolCallCount] | @tsv' \
  | (echo -e "SEQ\tMS\tTTFB\tINPUT\tCACHE_R\tCACHE_W\tOUTPUT\tTOOL_CALLS"; cat) \
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

The timeline's `tools` array holds tool spans: real tool traffic that passed through the
proxy. It is not the list of `toolCalls` that a model asked for. A join on a model call's
`seq` therefore finds no tool name. This is expected, and it is not a bug.

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

Accepted forms are a day count (`7d`) or a Go duration (`168h`). Cortex refuses zero and
negative values. It also refuses a sign, so `+7d` is invalid.

**A malformed query.** A query that Cortex cannot decode is a 400:

```sh
$ curl -s -i 'http://127.0.0.1:47601/v1/export?session=%GG' | head -2
HTTP/1.1 400 Bad Request
Content-Type: application/json
```

Cortex never treats a broken filter as an absent filter. `agentop export` cannot produce
this error, because it escapes its own query. A hand-written `curl` can.

## See also

- [`session-export.md`](session-export.md) — the fields of a record, and the full reading
  rules
- [`session-api.md`](session-api.md) — every endpoint, `GET /v1/export` included
- [`session-dump.md`](session-dump.md) — use this when you want the content: prompts,
  completions and tool results
- [`pricing.md`](pricing.md) — use `agentop cost` when the question is spend
