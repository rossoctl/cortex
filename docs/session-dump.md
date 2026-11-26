# Dumping sessions to files

> **On a laptop the proxy now keeps sessions itself.** A local install writes every
> session to `~/.cortex/sessions` ([#901](https://github.com/rossoctl/cortex/issues/901);
> see [the laptop guide](laptop-service.md#session-history-is-kept-in-cortexsessions)),
> so a restart no longer loses them, and agentop keeps listing them. This script is still
> how you get sessions *out* as files. It is also the only way to keep anything from a
> cluster sidecar, which has no archive. A whole dump covers the sessions in memory.
> `--session <id>` also reaches a session that only the archive holds, because the
> endpoint it reads pages into the archive.

Outside a local install Cortex keeps intercepted sessions **in memory only**, so a
proxy restart loses every session it was holding. `cortex-session-dump`
walks the [Session Events API](session-api.md) and writes what is
currently resident to files.

Use it for three things. Keep a session past a restart. Hand traffic to someone who
cannot reach the proxy. Analyse many sessions at once. The `agentop` TUI shows none of
these.

It is a snapshot tool, not storage. Two limits follow. It captures only what the store
holds at the moment it runs. You must run it before the thing you want is gone. On a
laptop the session archive closes that gap. In a cluster the gap is still open.

A dump is the *content* — prompts, completions, tool results. When what you want is the
*measurements* over a month of traffic, [`GET /v1/export`](session-export.md) writes the
archive as content-free records instead: no prompts, completions or tool results, which
is what makes it shareable where a dump is not. It is not anonymous, though — hosts,
models, session ids and timestamps remain, and together they describe who talked to what
and when. Read one before it goes anywhere the traffic pattern itself is sensitive.

## Install

`scripts/install.sh` fetches it with the release. `agentop setup` installs it next to
`agentop` and `cortex`, as `~/.local/bin/cortex-session-dump`. Any install from the first
release after this change therefore has it.

The fetch is never fatal. If it fails, the installer warns you and installs the rest. `make dev-install` installs only
the two binaries it builds, and `agentop uninstall` removes all three. Check with:

```sh
cortex-session-dump --help
```

If that prints nothing, your Cortex predates it; use the standalone install below.

### Standalone, for a Cortex that is already running

Nothing needs restarting — the script only reads the API.

```sh
curl -fsSL https://raw.githubusercontent.com/rossoctl/cortex/main/scripts/dev/cortex-session-dump.py \
  -o ~/.local/bin/cortex-session-dump
chmod +x ~/.local/bin/cortex-session-dump
```

Needs `python3` and nothing else — no pip install, standard library only.

Swap `main` for a tag (`.../cortex/v0.7.0/scripts/...`) to take the copy matching
a specific release rather than the tip.

## Use

```sh
cortex-session-dump                          # every session, no message bodies
cortex-session-dump --full                   # include prompts and completions
cortex-session-dump --active                 # only sessions still receiving traffic
cortex-session-dump --session <id> --full    # one session, repeatable
cortex-session-dump --out ~/cortex-snapshots/2026-09-30
```

Give each run its own `--out`. The script refuses a directory that already holds a dump.
The reason: a `--session` or `--active` run writes only its own selection. Reusing a
previous full dump would leave session files from that earlier run, next to a
`sessions.json` that no longer lists them. Pass `--reuse-dir` to override the refusal.

The script exits non-zero if any selected session failed. A cron job or a `&&` chain
therefore cannot mistake a partial dump for a complete one.

An id that the index does not list also counts as a failure. Naming an id is a claim that
it exists. A typo must not look like success, and neither must a session that was evicted
since you read the list.

`--active` that matches nothing is a legitimate empty result. It exits 0.

It finds the API itself, probing `127.0.0.1:47601` (what `cortex
--local` pins) then `127.0.0.1:9094` (the mode presets' default). For anything
else, or an in-cluster proxy, pass `--api`:

```sh
kubectl port-forward -n team1 deploy/weather-agent 9094:9094 &
cortex-session-dump --api http://127.0.0.1:9094
```

## Where the files land

`--out` defaults to `./cortex-dump`:

```
cortex-dump/
├── sessions.json              the session index: ids, timestamps, token + cost totals
├── usage.json                 time-bucketed usage aggregates
├── pipeline.json              the active plugin pipeline
├── plugins.json               every registered plugin
└── sessions/
    └── <session-id>.jsonl     one JSON event per line, oldest first
```

The `.jsonl` files are the interesting part — one event per line, so they stream
without loading whole:

```sh
# Which models did this session use?
jq -r '.inference.model // empty' cortex-dump/sessions/<id>.jsonl | sort | uniq -c

# Total cost of a session, in dollars
jq -s 'map(.plugins.cost.cost_usd // 0) | add' cortex-dump/sessions/<id>.jsonl

# Anything a plugin denied
jq 'select(.phase == "denied")' cortex-dump/sessions/<id>.jsonl

# Slowest requests first
jq -s 'sort_by(-(.durationMs // 0)) | .[:10] | .[] | {durationMs, httpPath}' \
  cortex-dump/sessions/<id>.jsonl
```

Events are ordered by `at`, then `seq`. `at` alone is not unique. `seq` restarts at 1 if
Cortex evicts a session and then creates it again under the same id. `seq` is therefore
not a stable sort key across the whole life of a session.

For the per-field meaning of `inference`, `mcp`, `a2a`, `invocations` and
`plugins`, see the event schema in [`session-api.md`](session-api.md#event-schema) and
[`plugin-reference.md`](plugin-reference.md#emitting-session-events).

## `--full` writes prompts and completions to disk

Without `--full` you get metadata: models, token counts, costs, latencies, plugin
decisions, HTTP method and path. With it you also get the message bodies — user
prompts, LLM completions, tool arguments and results.

That is usually the reason to dump at all, and it is also the reason to be careful.
Expect the file to be much larger. How much larger depends on the message sizes.

Two measurements, both from a live proxy. One 65-event Claude Code session measured 58 KB
as a summary against 3.9 MB as a full dump, which is **~68x**. The estimate in
`core/sessionapi/server.go` is **~163x**, over conversations with larger message bodies.

Treat either number as an order of magnitude, not as a constant. Expect the output to contain
whatever went through the proxy: source code,
customer data, credentials pasted into a prompt. A secret in a URL *path segment*
survives too — the API strips query strings but not path segments.

So:

- Keep dumps out of git. Cortex's own `~/.cortex/` is not a repo; if you dump
  into a working tree, add the output directory to `.gitignore` first.
- Treat a `--full` dump as you would the conversation itself before sharing it.
- Prefer the default view when you only need cost, latency or model attribution.

The API has no authentication of its own. Bind it to loopback, or reach it through a port
forward. Anything that can reach the port can read its payloads. A dump does not change
that. It only makes a copy that outlives the process.

## For timings and tool names, use the timeline instead

**A dump is the wrong tool for "how long did each tool take, and what did it run".**

Without `--full` the script asks the API for `view=summary`. That strips `toolCalls`, so
you get `toolCallCount` but no tool names. With `--full` you get the names and the
arguments. You also get every prompt, completion and tool result, which is usually more
than the question needs.

`GET /v1/sessions/{id}/timeline` answers it directly, and carries no prompt text. It returns
named tool spans. Each span has `start`, `end` and a `preview` of the tool's own argument:
the Bash command, the file path, or the URL. It also returns one span per model call.

See [Reading timings out](session-api.md#reading-timings-out) for worked queries.

Reach for a dump when you want the session's *contents* as files, which is what it is
for.

## What a dump does and does not capture

A dump is a point-in-time copy of what the store still holds. That is not everything
that ever happened.

The default `session:` config sets `max_sessions` to 100. Past that, Cortex evicts the
least-recently-updated session. `max_events` and `ttl` are unlimited, so Cortex does not
trim an individual session. Tune these values in your config; see `CLAUDE.md`.

A dump cannot recover a session that was already evicted. Nothing survives a restart.

For continuous capture, use the SSE stream at `GET /v1/events`. A small consumer can
append it to a rolling file. This script does not do that.

An external consumer is also not the plan.
[#901](https://github.com/rossoctl/cortex/issues/901) proposes that the proxy persist
sessions itself, which removes the need for one.

## Where the logs are

Distinct from session data, and often what you actually want:

| What | Laptop install | In-cluster |
|---|---|---|
| Proxy log (stdout/stderr) | `~/.cortex/proxy.log` | `kubectl logs <pod> -c authbridge` |
| Active config | `~/.cortex/config.yaml` | `kubectl get cm authbridge-runtime-config -o yaml` |
| Config as loaded | `curl 127.0.0.1:47602/config` | `curl <pod>:9093/config` |
| Hot-reload status | `curl 127.0.0.1:47602/reload/status` | `curl <pod>:9093/reload/status` |
| Live session view | `agentop` | `agentop` (picks a pod, port-forwards for you) |

The stats port is `47602` on a laptop install and `9093` in-cluster, mirroring
the session API's `47601` / `9094` split.

## Where the data comes from

The script makes read-only `GET` requests against the Session Events API. It pages
backward from each session's newest event with `?before=<seq>`:

| Endpoint | Written to |
|---|---|
| `GET /v1/sessions` | `sessions.json` |
| `GET /v1/sessions/{id}` | `sessions/<id>.jsonl` |
| `GET /v1/usage` | `usage.json` |
| `GET /v1/pipeline` | `pipeline.json` |
| `GET /v1/plugins` | `plugins.json` |

`GET /` on the same port returns an index, one line per endpoint. It is the quickest way
to confirm that you reached the session API. [`session-api.md`](session-api.md) documents
every endpoint in full.
