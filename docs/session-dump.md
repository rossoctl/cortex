# Dumping sessions to files

> **On a laptop the proxy now keeps sessions itself.** A local install writes every
> session to `~/.cortex/sessions` ([#901](https://github.com/rossoctl/cortex/issues/901);
> see [the laptop guide](laptop-service.md#session-history-is-kept-in-cortexsessions)),
> so a restart no longer loses them and agentop keeps listing them. What this script is
> still for is getting sessions *out* as files — and it is the only way to keep anything
> from a cluster sidecar, which has no archive. A whole dump covers the sessions in
> memory; `--session <id>` also reaches one only the archive holds, because the
> endpoint it reads pages into the archive.

Outside a local install Cortex keeps intercepted sessions **in memory only**, so a
proxy restart loses every session it was holding. `cortex-session-dump`
walks the [Session Events API](session-api.md) and writes what is
currently resident to files.

Reach for it when you want to keep a session past a restart, hand traffic to
someone who cannot reach the proxy, or analyse many sessions at once — anything
beyond what the `agentop` TUI shows live.

Its limits follow from being a snapshot tool rather than storage: it captures
only what the store still holds at the moment it runs, and it has to be run
before the thing you want to keep is gone. On a laptop the session archive closes that
gap; in a cluster it is still open.

## Install

`scripts/install.sh` fetches it with the release, and `agentop setup` installs it
alongside `agentop` and `cortex`, as `~/.local/bin/cortex-session-dump` — so any install
from the first release after this change lands already has it. The fetch is never fatal:
when it fails, the installer warns and installs the rest. `make dev-install` installs only
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

Each run wants its own `--out`. Writing into a directory that already holds a
dump is refused, because a `--session` or `--active` run only writes its own
selection — so reusing a previous full dump would leave session files this run
did not produce, beside a `sessions.json` that no longer lists them. Pass
`--reuse-dir` to override.

It exits nonzero if any selected session failed, so a partial dump is not
mistaken for a complete one in a cron job or a `&&` chain. An id passed to
`--session` that the index does not list counts as a failure too — naming an id
is a claim that it exists, and a typo or a session evicted since you read it
should not look like success. `--active` matching nothing is a legitimate empty
result and exits 0.

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

Events are ordered by `at`, then `seq`. `at` alone is not unique, and `seq`
restarts at 1 if a session is evicted and later re-created under the same id,
so it is not a stable sort key across a session's whole life.

For the per-field meaning of `inference`, `mcp`, `a2a`, `invocations` and
`plugins`, see the event schema in [`session-api.md`](session-api.md#event-schema) and
[`plugin-reference.md`](plugin-reference.md#emitting-session-events).

## `--full` writes prompts and completions to disk

Without `--full` you get metadata: models, token counts, costs, latencies, plugin
decisions, HTTP method and path. With it you also get the message bodies — user
prompts, LLM completions, tool arguments and results.

That is usually the reason to dump at all, and it is also the reason to be careful.
Expect a large multiple of the size, varying with how big the messages are: one
65-event Claude Code session measured 58 KB summary against 3.9 MB full (**~68x**),
while the server's own estimate in `core/sessionapi/server.go` is **~163x**, also
from a live proxy but over conversations with larger message bodies. Treat either
as an order of magnitude rather than a constant. Expect the output to contain
whatever went through the proxy: source code,
customer data, credentials pasted into a prompt. A secret in a URL *path segment*
survives too — the API strips query strings but not path segments.

So:

- Keep dumps out of git. Cortex's own `~/.cortex/` is not a repo; if you dump
  into a working tree, add the output directory to `.gitignore` first.
- Treat a `--full` dump as you would the conversation itself before sharing it.
- Prefer the default view when you only need cost, latency or model attribution.

The API has no authentication of its own — it is expected to be bound to
loopback or reached through a port-forward, and its payloads are readable by
anything that can reach the port. Dumping to a file does not change that; it
just makes a copy that outlives the process.

## What a dump does and does not capture

A dump is a point-in-time copy of what the store still holds, which is not the
same as everything that ever happened. With the default `session:` config,
`max_sessions` is 100 and the least-recently-updated session is evicted past
that; `max_events` and `ttl` are unlimited, so individual sessions are not
trimmed. Tune those in your config (see `CLAUDE.md`) — a dump cannot recover what
was already evicted, and nothing survives a restart.

For continuous capture rather than snapshots, the API also exposes an SSE stream
(`GET /v1/events`) that a small consumer can append to a rolling file. That is
not what this script does, and it is not the plan either —
[#901](https://github.com/rossoctl/cortex/issues/901) proposes the proxy persist
sessions itself, which removes the need for an external consumer altogether.

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

Read-only `GET`s against the Session Events API, paging backward from each
session's newest event with `?before=<seq>`:

| Endpoint | Written to |
|---|---|
| `GET /v1/sessions` | `sessions.json` |
| `GET /v1/sessions/{id}` | `sessions/<id>.jsonl` |
| `GET /v1/usage` | `usage.json` |
| `GET /v1/pipeline` | `pipeline.json` |
| `GET /v1/plugins` | `plugins.json` |

`GET /` on the same port returns a one-line-per-endpoint index, which is the
quickest way to confirm you are talking to the session API at all. The endpoints
are documented in full in [`session-api.md`](session-api.md).
