# Exporting your session data: a walkthrough

[`session-export.md`](session-export.md) is the reference — what each field means and the
rules for reading it. This page is the walkthrough: get an export, make it answer a
question, and recognise the three ways it can mislead you.

Every command and every output below was run against a served archive.

- [Check you have an archive first](#check-you-have-an-archive-first)
- [Your first export](#your-first-export)
- [Narrowing it down](#narrowing-it-down)
- [Answering actual questions](#answering-actual-questions)
- [Three traps](#three-traps)
- [From a cluster sidecar](#from-a-cluster-sidecar)
- [In a script](#in-a-script)
- [When it refuses](#when-it-refuses)

## Check you have an archive first

The export reads the **durable archive on disk**, not the session store in memory. On a
laptop install the archive is on by default; elsewhere it usually is not. Ask before you
export:

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

`"archive": null` — or no `archive` key — means this proxy has no archive, and
`agentop export` will answer 404. See [From a cluster sidecar](#from-a-cluster-sidecar).

To see what it holds, including sessions memory has already evicted (`"resident": false`):

```sh
curl -s 'http://127.0.0.1:47601/v1/sessions?archived=true' \
  | jq -r '.sessions[] | [.id, .eventCount, .resident] | @tsv'
```

```
old-session      3   false
today-session    4   false
```

## Your first export

```sh
agentop export --out usage.ndjson
```

```
  7 records, 1.6kB → usage.ndjson
```

That summary line goes to **stderr**, deliberately: stdout may be the export itself, so
nothing written for a human goes there. Without `--out` the records stream to stdout, which
is what makes piping work:

```sh
agentop export | jq -r '.model // empty' | sort | uniq -c
```

One line per archived event, each ~30 content-free scalars:

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

Note what is *not* there. The session this came from had
`"content": "SECRET-PROMPT-DO-NOT-EXPORT"` in its messages; the export contains
`"messageCount": 1` instead. No prompts, no completions, no tool arguments — that is the
property that makes the file shareable where the full events are not.

It is **not anonymous**, though. Hosts, models, session ids and timestamps remain, and
together they describe who talked to what and when. Read one before it goes anywhere the
traffic pattern itself is sensitive.

An existing `--out` file is refused rather than clobbered:

```sh
$ agentop export --out usage.ndjson
agentop export: usage.ndjson already exists; --force overwrites it
```

## Narrowing it down

```sh
agentop export --since 7d                    # a day count
agentop export --since 168h                  # or a Go duration
agentop export --session today-session        # one session
agentop export --session a --session b        # repeatable
agentop export --session a,b                  # or comma-separated
```

`--since` is measured from now, and it is applied **per event**, not per session:

```sh
$ agentop export --since 7d
  4 records, 1.1kB → (stdout)      # the 8-day-old session is gone
```

A month-old session with traffic this morning contributes this morning's events to
`--since 24h`; its older ones are not carried. So a session can appear in a filtered
export with fewer events than it really has.

## Answering actual questions

The export is NDJSON, so `jq` plus standard tools is usually enough.

**Tokens by model** — note the `// 0`, which the [traps](#three-traps) section explains:

```sh
agentop export --since 30d \
  | jq -r 'select(.model) | [.model, (.inputTokens // 0), (.outputTokens // 0)] | @tsv' \
  | awk -F'\t' '{i[$1]+=$2; o[$1]+=$3}
                END {for (m in i) printf "%-18s in=%-8d out=%d\n", m, i[m], o[m]}'
```

```
claude-opus-5      in=4860     out=198
```

**Where the traffic went:**

```sh
agentop export | jq -r '.host // empty' | sort | uniq -c | sort -rn
```

```
   4 api.anthropic.com
   3 api.openai.com
```

**Latency percentiles:**

```sh
agentop export --since 7d | jq -r '.durationMs // empty' | sort -n \
  | awk '{a[NR]=$1} END {printf "n=%d p50=%dms p95=%dms max=%dms\n",
          NR, a[int(NR*0.5)], a[int(NR*0.95)], a[NR]}'
```

```
n=4 p50=1237ms p95=1274ms max=1311ms
```

**Into DuckDB**, which reads NDJSON natively and is the fastest route to real SQL:

```sh
agentop export --since 30d --out usage.ndjson
duckdb -c "
  SELECT model, count(*) AS calls, sum(inputTokens) AS in_tok, median(durationMs) AS p50
  FROM read_json_auto('usage.ndjson')
  WHERE model IS NOT NULL
  GROUP BY model ORDER BY calls DESC"
```

**Into pandas:**

```python
import pandas as pd
df = pd.read_json("usage.ndjson", lines=True)
df["at"] = pd.to_datetime(df["at"])
# Absent fields arrive as NaN, not 0 — fill before you aggregate.
print(df.groupby("model")[["inputTokens", "outputTokens"]].sum(min_count=1))
```

## Three traps

These are the ways a reader gets a plausible-looking wrong answer. All three are
properties of the data, not bugs.

### 1. An absent field is not a zero

Fields are omitted when not recorded, so they arrive as `null` in `jq` and `NaN` in
pandas — never as `0`. Of the 7 records above, 3 have **no `inputTokens` key at all**
because they are not inference calls.

```sh
# WRONG: averages the non-inference rows in as zeroes
$ agentop export | jq -s 'map(.inputTokens) | add / length'
694.2857142857143

# RIGHT: average only the rows that have the field
$ agentop export | jq -s 'map(select(.inputTokens)) | (map(.inputTokens) | add) / length'
1215
```

On this 7-record archive the careless version is 43% low. On a real one, where most events
are not inference calls, it is far worse.

A real zero and "not recorded" are indistinguishable in this format. When you need the
distinction, test for the key's presence (`has("inputTokens")`), not its value.

### 2. Order by `at`, then `seq` — never `seq` alone

`seq` counts from 1 *within one incarnation of a session*. A session re-created under the
same id restarts at 1, and nothing in the record tells you that happened.

```sh
agentop export | jq -r '[.at, (.seq|tostring), .sessionId] | @tsv' | sort -k1,1 -k2,2n
```

`at` alone is not enough either: ties are common on a fast local proxy, and `seq` is the
tiebreak within a session.

### 3. A session id is not a unit of work

A client may reuse one id indefinitely — the id `default` can span weeks of unrelated
work. **Aggregating per `sessionId` averages across tasks that have nothing to do with
each other.** Until a derived incarnation key exists
([#1739](https://github.com/rossoctl/cortex/issues/1739)), infer "one sitting" from gaps
in `at`:

```sh
# Each line starts a new "sitting": more than 30 minutes since the previous event.
# `at` carries fractional seconds, which fromdateiso8601 rejects — hence the sub().
agentop export --session default \
  | jq -rs 'sort_by(.at) | . as $r | range(1; length) as $i
            | (($r[$i].at | sub("\\.[0-9]+";"") | fromdateiso8601)
               - ($r[$i-1].at | sub("\\.[0-9]+";"") | fromdateiso8601)) as $gap
            | select($gap > 1800)
            | "\($r[$i].at)  gap=\(($gap/3600)|floor)h"'
```

```
2026-11-16T23:08:45.84114Z  gap=191h
```

## From a cluster sidecar

An injected sidecar normally runs **no archive**, so the endpoint is a 404:

```sh
$ agentop export --endpoint http://127.0.0.1:9094
agentop export: /v1/export: apiclient: not found
  this proxy has no session archive, or predates /v1/export (`agentop update`); a cluster sidecar never runs one
exit=1
```

```
HTTP/1.1 404 Not Found
Content-Type: application/json

{"error":"session archive not enabled"}
```

That message names both causes, and they need different fixes: no archive configured
(set `session.archive.dir`), versus a proxy older than the endpoint (upgrade it). If you
need history from a cluster, the archive has to be enabled there first — otherwise use
[`cortex-session-dump`](session-dump.md), which reads what is resident in memory.

Where an archive *is* configured, port-forward and point `--endpoint` at it:

```sh
kubectl port-forward -n team1 deploy/weather-agent 9094:9094 &
agentop export --endpoint http://127.0.0.1:9094 --since 24h --out team1.ndjson
```

Or with no `agentop` binary to hand, the endpoint is plain HTTP:

```sh
curl -s 'http://127.0.0.1:9094/v1/export?since=24h' > team1.ndjson
curl -s 'http://127.0.0.1:9094/v1/export?session=abc&session=def' > two.ndjson
```

The API has **no authentication**. Bind it to loopback or reach it through a
port-forward, never through ingress.

## In a script

The exit codes are the contract:

| Exit | Meaning |
|---|---|
| `0` | The export completed. The summary line on stderr says how many records. |
| `1` | The request failed, or the walk died part-way. **Any output is incomplete.** |
| `2` | Usage error — a bad flag, an empty filter, a stray argument. Nothing was requested. |

Verified:

```
  --since 7d             exit=0
  --session nope         exit=1
  --since bogus          exit=1
  --since ''             exit=2
  extra-positional       exit=2
```

**A truncated export always exits non-zero.** If the server's walk fails after it has
started writing, it resets the connection rather than ending the body cleanly, so the
client sees a read error instead of a clean EOF:

```
agentop export: unexpected EOF
  the walk failed part-way; usage.ndjson holds every complete record before it
```

Every complete NDJSON line already written stays valid, so you keep what arrived — you
just cannot mistake it for the whole archive. This is why checking the exit code matters
more than checking the file is non-empty:

```sh
set -euo pipefail
if ! agentop export --since 7d --out "$OUT" --force; then
  echo "export incomplete; not publishing $OUT" >&2
  exit 1
fi
```

**An empty filter is refused, not ignored.** This is the case worth knowing about if you
build arguments in a shell:

```sh
$ WINDOW=""            # e.g. a lookup that returned nothing
$ agentop export --since "$WINDOW"
agentop export: --since needs a value, such as 168h or 7d; omit it to export every event
exit=2
```

Reading that as "no floor" would have exported the entire archive from a broken variable.
`--session ""` is refused the same way. Omitting the flag entirely is still the legitimate
way to say "everything".

A large archive takes a while to walk server-side; the command waits up to ten minutes
before giving up.

## When it refuses

**An unknown session names the ones it does hold:**

```
$ agentop export --session nope
agentop export: /v1/export?session=nope: apiclient: bad request: sessionexport: no archived session nope (archived: old-session, today-session)
```

The refusal arrives *before* the first byte, so a typo never creates or truncates your
`--out` file. On a wide archive the list is capped to fit, and ends with a count of the
rest; if even that does not survive, the command prints the `curl` that lists them:

```
  the proxy did not name the sessions it holds; list them with:
    curl -s 'http://127.0.0.1:47601/v1/sessions?archived=true'
```

**A malformed `--since` says what it accepts, and says nothing about sessions:**

```
$ agentop export --session today-session --since bogus
agentop export: /v1/export?session=today-session&since=bogus: apiclient: bad request: since must be a duration or Nd, e.g. 168h or 7d
```

Accepted forms are a Go duration (`168h`, `90m`) or a whole-day count (`7d`, `30d`).
Zero, negative and signed values are refused — `+7d` is not valid even though it looks it.

**A malformed query over raw HTTP is a 400, not a silent full export:**

```sh
$ curl -s -i 'http://127.0.0.1:47601/v1/export?session=%GG' | head -2
HTTP/1.1 400 Bad Request
Content-Type: application/json
```

A filter that cannot be decoded is never treated as an absent filter. `agentop export`
cannot produce this — it escapes its own query — but a hand-written `curl` can.

## See also

- [`session-export.md`](session-export.md) — the record's fields and the full reading rules
- [`session-api.md`](session-api.md) — every endpoint, including `GET /v1/export`
- [`session-dump.md`](session-dump.md) — when you want the **content** (prompts,
  completions, tool results) rather than the measurements
- [`pricing.md`](pricing.md) — `agentop cost`, when the question is spend rather than
  per-event detail
