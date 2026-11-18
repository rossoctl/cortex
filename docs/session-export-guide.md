# Export your session data: a walkthrough

This page tells you how to get an export and how to read it correctly.
[`session-export-reference.md`](session-export-reference.md) is the reference. It tells
you what each field means.

Every command and every output on this page was run against a served archive.

**Terms used on this page.** A **record** is one line of the export. An **event** is one
request or response that Cortex captured. One event becomes one record. The **store** is
what Cortex holds in memory. The **archive** is what Cortex has written to disk.

- [What an export contains](#what-an-export-contains)
- [Step 1: list the sessions](#step-1-list-the-sessions)
- [Step 2: export](#step-2-export)
- [Step 3: select less than everything](#step-3-select-less-than-everything)
- [Step 4: answer a question with the export](#step-4-answer-a-question-with-the-export)
- [Three traps](#three-traps)
- [When the proxy has no archive](#when-the-proxy-has-no-archive)
- [Use the export in a script](#use-the-export-in-a-script)
- [Read the refusals](#read-the-refusals)

## What an export contains

An export carries the **shape** of your traffic. It omits the content. An export tells you
how much, how fast, how often, and to where. It cannot tell you what was said.

Cortex does record content. It holds it in the store and in the archive, and
[`session-dump.md`](session-dump.md) is how you get it. The export is the path that leaves
the content behind.

Read this section before you export. It decides whether this is the right tool.

### What a record carries

One record is one event. A record holds 36 fields. Cortex caps each label at 96 bytes, so
every field is a number or a short label. `sessionId` is the exception: see
[One field is not short](#one-field-is-not-short).

| Group | Fields |
|---|---|
| Identity | `sessionId`, `seq`, `requestId`, `at` |
| Destination | `host`, `requestedHost`, `httpMethod` |
| Outcome | `statusCode`, `errorKind`, `errorCode`, `finishReason` |
| Timing | `durationMs`, `ttfbMs` |
| Volume | `bytesUp`, `bytesDown` |
| Tokens | `inputTokens`, `cacheReadTokens`, `cacheWriteTokens`, `outputTokens`, `reasoningTokens`, `presentKinds` |
| Model | `model`, `requestedModel`, `servedModel`, `stream`, `streamedResponse` |
| Counts | `messageCount`, `toolCount`, `toolCallCount` |
| Caller | `client`, `agentRole` |
| Transport | `direction`, `phase`, `tunnel`, `tunnelReason`, `schema` |

`client` is the product label from the User-Agent header, such as `claude-cli`. It is not
an authenticated client id. `agentRole` comes from the inference metadata. Neither one
names an authenticated principal.

### One field is not short

`sessionId` is the value a client asserted. Cortex copies it through unchanged, up to 256
bytes. The 96-byte label cap does not apply to it.

A client is therefore free to use a sentence as its session id. If a client does that, the
export carries that sentence.

**Read the `sessionId` values before you share an export.** This is the one field where
client text can reach a record.

### What a record never carries

A record never contains the content below. Cortex does not remove it. Cortex never copies
it in. A record is an allowlist: a field must be added deliberately to appear.

- Prompts and user queries
- Model completions and reasoning text
- Tool arguments, such as a `Bash` command line
- Tool results, and tool names
- MCP parameters and results, and A2A message parts and artifacts
- Request and response bodies, and request headers
- Authenticated identity: subject, client id, agent id, scopes, SPIFFE id
- URL paths

A test holds this contract. `TestRecord_CarriesNoContent` puts 24 distinct secrets in the
content-bearing fields of an event. It then encodes the record. The test fails if any
secret reaches the encoded bytes, which fails CI.

The count above is pinned. `TestDocGuideSecretCountMatchesAllSecrets` fails if this page
and the test disagree.

### Questions an export answers

- Token spend per model
- Latency percentiles, including time to first byte
- Cache hit rates
- Which hosts an agent reached
- Error rates and refusal rates
- Agent-version adoption
- How traffic changed across a release

### Questions an export cannot answer

- What a user asked
- What the model replied
- Which file a tool touched
- Why a request was made
- Who the user was
- Which tools ran. A record counts them in `toolCallCount`. It never names them.

Tool names are not refused on privacy grounds. A name is low risk, unlike an argument: a
`Bash` command line is a secret, and arguments stay out permanently. Two other reasons keep
names out for now:

- A record is a flat list of scalars. A name list would be the first array, which changes
  the shape every consumer parses.
- Names alone would not give per-tool latency. A tool result arrives in a later request,
  and the key that pairs it with its call is the tool-use id, which no record carries. So a
  name list says only which tools a response asked for.

Use `GET /v1/sessions/{id}/timeline` for names today. It pairs each call with its result by
tool-use id, and it returns named spans.
[#1906](https://github.com/rossoctl/cortex/issues/1906) tracks adding names here.

An export is **content-free**. It is **not anonymous**. It keeps hosts, models, session
ids and timestamps. Together these show who called what, and when. Read an export before
you send it somewhere.

If you need any of the content above, use [`session-dump.md`](session-dump.md) instead.

## Step 1: list the sessions

Ask the proxy which sessions the archive holds:

```sh
curl -s 'http://127.0.0.1:47601/v1/sessions?archived=true' \
  | jq -r '.sessions[]
           | [.id, .eventCount, (.resident != false), (.title // "(untitled)")[0:50]]
           | @tsv' \
  | column -t -s $'\t'
```

```
849102c5-e59f-404c-8b2e-3dde35e052bb  412  true   please address the review comments
59046505-b6f3-43f5-a2ef-9071c9a847a2  188  false  https://github.com/rossoctl/cortex/pull/1854
0e2f9a41-7c3d-4b18-9f52-6a1e4d7b2c90  24   false  (untitled)
```

`column -t` aligns the columns. Without it `@tsv` separates the fields with tabs, and a
short event count moves the later columns out of line.

`-s $'\t'` is not optional. `column` splits on any whitespace by default, so a title with
spaces becomes several columns.

Each row is one session: its id, its event count, whether the store still holds it, and
its title.

The third column is `resident`. Test it with `!= false`, not with `// true`. In `jq` the
`//` operator treats `false` as absent, so `// true` reports every session as resident.

The values mean:

- **`true`**, or the field is absent: the store still holds this session. You can read it
  in the `agentop` viewer.
- **`false`**: the store has dropped this session, and the archive supplied this row. The
  viewer no longer shows it. The export still carries it.

A `resident` session is not always in the archive yet. Cortex writes to the archive a tick
behind the store, so a session that started seconds ago can be listed here and still be
absent from the export. Give it a moment and export again.

Cortex drops a session from the store when the store reaches its session limit, or when
the session expires. The archive keeps the session after that. This is the reason to
export: the archive holds history that memory has lost.

A proxy with no archive still answers this command. It ignores `archived=true` and returns
the store's sessions, each one `resident`. Check the `archive` key to tell the two apart:

```sh
curl -s 'http://127.0.0.1:47601/v1/sessions?archived=true' | jq '.archive'
```

A reply of `null` means this proxy has no archive. The rows above then come from memory
only, and the export cannot read them. See
[When the proxy has no archive](#when-the-proxy-has-no-archive).

### Copy the id you need

`--session` takes a **session id**. It matches that id exactly. It does not match a title.
It does not match a prefix.

One exception: the flag splits its value at commas, because it accepts a comma-separated
list. So `--session task,one` asks for two sessions, `task` and `one`. To export an id that
contains a comma, call the endpoint directly. `?session=` reads the whole value as one id:

```sh
curl -s 'http://127.0.0.1:47601/v1/export?session=task%2Cone' > one.ndjson
```

A session id is the value a client asserted. It is often a UUID. It can also be a plain
word, such as `default`.

A session has no title until its events carry one: a `/rename`, a user query, or ordinary
prose. Tunnel-only traffic and utility clients therefore stay untitled. Keep those rows.
They are the ones you most need an id for.

> **A title is user content. An export is not.**
>
> Cortex derives a title from the session's own events: a `/rename`, else a user query,
> else ordinary conversation. A title therefore contains prompt text.
>
> The export excludes prompt text. The command above does not. Run it in your own
> terminal. Do not paste its output where you would paste an export.

The export carries no title. A record identifies its session by `sessionId` only. To label
a report, join the ids back to the titles yourself.

## Step 2: export

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

Each record is one event, reduced to numbers and short labels:

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
`"SECRET-PROMPT-DO-NOT-EXPORT"`. The record has `"messageCount": 1` in its place.

The command refuses to overwrite a file:

```sh
$ agentop export --out usage.ndjson
agentop export: usage.ndjson already exists; --force overwrites it
```

Add `--force` to overwrite it.

## Step 3: select less than everything

Select by time:

```sh
agentop export --since 7d
```

Select by session id. Get the id from
[Step 1](#copy-the-id-you-need):

```sh
agentop export --session 849102c5-e59f-404c-8b2e-3dde35e052bb
```

Select more than one session. Repeat the flag, or separate the ids with commas and no
spaces:

```sh
agentop export --session 849102c5-… --session 59046505-…
agentop export --session 849102c5-…,59046505-…
```

A space after a comma needs quotes. Without them the shell splits the list, and the second
id becomes a stray argument that the command refuses with exit 2:

```sh
agentop export --session 'a, b'     # two sessions
agentop export --session a, b       # exit 2: unexpected argument "b"
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

## Step 4: answer a question with the export

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

**p50** is the median: half the requests were faster, half slower. **p95** means 95% of
requests were faster than this value. **max** is the slowest single request.

Read the gap between p50 and p95, not either one alone. `p50=200ms p95=240ms` is a
consistent system. `p50=200ms p95=9000ms` is a system that stalls. Both can have the same
average, which is why this recipe does not report one: a single timeout moves an average
and tells you nothing about the typical request.

The command works in four steps:

- `.durationMs // empty` takes the duration and drops a record that has none. A dropped
  record is not a zero. See [Trap 1](#trap-1-an-absent-field-is-not-a-zero).
- `sort -n` sorts numerically. Plain `sort` is alphabetical, which puts `1000` before `9`.
- `a[NR]=$1` stores each value in order. `NR` is awk's line number, so the array runs from
  1 to the number of values.
- `int((NR-1)*0.95)+1` is the index of the p95 value.

That index arithmetic is awkward because the array starts at 1. For 100 values,
`int(99*0.95)+1` is index 95. Without the `+1` you read the value one place too low, and at
`n=1` you read `a[0]`, which is unset. awk prints that as `0`, so the recipe would report
`p50=0ms` with no error.

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

The columns are:

| Column | Field | Meaning |
|---|---|---|
| `SEQ` | `seq` | The event's position in the session. Higher is later. |
| `MS` | `durationMs` | The whole request, in milliseconds. |
| `TTFB` | `ttfbMs` | Time to the first byte of the response. `MS` − `TTFB` is the time the body took. |
| `INPUT` | `inputTokens` | Prompt tokens the model read fresh. |
| `CACHE_R` | `cacheReadTokens` | Prompt tokens served from the cache, and far cheaper. |
| `CACHE_W` | `cacheWriteTokens` | Prompt tokens written to the cache for a later turn. |
| `OUTPUT` | `outputTokens` | Tokens the model generated. |
| `TOOL_CALLS` | `toolCallCount` | Tool calls this response asked for. Never their names. |

Two rules for reading the token columns:

- **The three prompt columns are a split, not a total.** Add `INPUT` + `CACHE_R` +
  `CACHE_W` to get the whole prompt. No field holds that sum, which stops a consumer
  counting cached tokens twice.
- **`CACHE_R` dwarfing `INPUT` is normal and good.** The rows above re-read about 190,000
  cached tokens and paid for 1,200. A low `CACHE_R` on a long conversation is the problem
  worth chasing.

Select `phase=="response"`. A request-phase record carries no token counts, because the
model reports usage on the response.

`map(. // "-")` is not cosmetic. An absent field writes an empty cell, and `column`
merges adjacent blanks, so every later value moves one column left under the wrong
header — with no error. A response with no `cacheWriteTokens` is ordinary, so this
happens on real data. `-` makes the absence visible, which is also what Trap 1 asks for.

`printf` rather than `echo -e`: under `dash`, which is `/bin/sh` on Debian and Ubuntu,
`echo -e` prints a literal `-e`.

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

Two ids are never one unit of work. `default` is the shared bucket. An id starting with
`pending:` is a holding bucket: an agent's calls from before its process named a session.
`codex exec` leaves one on every run. Exclude both from an aggregate, or a run's startup
counts twice — once here, once in the session it named:

```sh
agentop export | jq -c 'select(.sessionId | startswith("pending:") or . == "default" | not)'
```

[`session-api.md`](session-api.md#event-schema) explains how a bucket links to the session
it named.

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

## When the proxy has no archive

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

A reply of `null` means this proxy has no archive.

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

- [`session-export-reference.md`](session-export-reference.md) — the fields of a record, and the full reading
  rules
- [`session-api.md`](session-api.md) — every endpoint, `GET /v1/export` included
- [`session-dump.md`](session-dump.md) — use this when you want the content: prompts,
  completions and tool results
- [`pricing.md`](pricing.md) — use `agentop cost` when the question is spend
