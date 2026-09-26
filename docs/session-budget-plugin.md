# session-budget Plugin

Enforces per-session budgets on tokens, LLM/inference calls, and wall-clock
duration. `max_calls` counts inference calls only — MCP tool calls, A2A
messages, and other outbound traffic do not count toward it.
Supports three `on_exceed` modes:

- `deny` — return 403 (default)
- `observe` — shadow mode: log without blocking, useful for calibrating limits
- `pause` — HITL: POST to a webhook for approval before continuing

A "session" is the AuthBridge session ID (typically one A2A conversation
or agent task invocation). Redis holds durable counters across pods; a
local cache serves the hot path with zero I/O.

## Build

Opt-in — build with `-tags include_plugin_sessionbudget`:

```bash
docker build -f cmd/authbridge-proxy/Dockerfile \
  --build-arg GO_BUILD_TAGS="include_plugin_sessionbudget" \
  -t authbridge:latest .
```

Same tag works for `cmd/authbridge-envoy/Dockerfile`.

## Configuration

```yaml
pipeline:
  outbound:
    plugins:
      - name: token-exchange
        config: { ... }
      - name: session-budget
        config:
          redis_url: "redis://valkey.infra.svc:6379"
          max_tokens: 50000
          max_calls: 100
          max_duration_seconds: 1800
      - name: inference-parser
```

| Field | Default | Description |
|-------|---------|-------------|
| `redis_url` | — (required) | Redis/Valkey URL |
| `max_tokens` | 0 | Cumulative token ceiling per session (all kinds summed). 0 = no limit. |
| `max_input_tokens` | 0 | Per-kind ceiling on uncached prompt tokens. 0 = no limit. |
| `max_cache_read_tokens` | 0 | Per-kind ceiling on prompt tokens served from cache. 0 = no limit. |
| `max_cache_write_tokens` | 0 | Per-kind ceiling on prompt tokens written to cache. 0 = no limit. |
| `max_output_tokens` | 0 | Per-kind ceiling on generated completion tokens. 0 = no limit. |
| `max_reasoning_tokens` | 0 | Per-kind ceiling on reasoning-only output tokens (subset of output). 0 = no limit. See note below — this limit was inert on Anthropic traffic until recently. |
| `max_calls` | 0 | LLM/inference call cap (from `inference-parser`); MCP, A2A, and other outbound traffic do not count. 0 = no limit. See note below on enforcement scope. |
| `max_duration_seconds` | 0 | Session lifetime cap (0 = no limit) |
| `on_exceed` | `deny` | `deny` (403), `observe` (log only), or `pause` (webhook) |
| `pause_webhook` | — | URL to POST on breach (required when `on_exceed=pause`) |
| `pause_timeout` | `30s` | Max wait for webhook response |
| `pause_timeout_action` | `deny` | Fallback on timeout/error: `deny` or `allow` |
| `pause_grace_period` | `5m` | Suppress repeat webhooks after approval. `0s` disables the grace window (webhook fires on every breach). |
| `session_ttl_seconds` | 7200 | Redis key TTL. Must be ≥ `max_duration_seconds` when the latter is set (rejected at Configure time otherwise). |
| `refresh_interval` | `5s` | Local-cache sync interval |
| `redis_unavailable` | `fail_open` | Only `fail_open` supported today |
| `default_session_fallback` | `false` | Pool sessionless traffic into a shared `"default"` bucket. Single-workload only — one caller exhausting the budget denies the rest. Under `max_duration_seconds`, continuous traffic refreshes the TTL, so once elapsed exceeds the limit requests stay denied until the key expires or is deleted. |

At least one of `max_tokens`, `max_input_tokens`, `max_cache_read_tokens`, `max_cache_write_tokens`, `max_output_tokens`, `max_reasoning_tokens`, `max_calls`, `max_duration_seconds` must be > 0.

**`max_reasoning_tokens` was inert on Anthropic traffic, and is not any more.**
`inference-parser` did not read Anthropic's
`usage.output_tokens_details.thinking_tokens`, so `ReasoningTokens` was always 0 on
that path and this limit could never be reached however low it was set. The parser
reads it now, which makes the counter real **without any config change of yours**.

If you set `max_reasoning_tokens` against Claude traffic and saw no effect, that was
why — and with `on_exceed` at its `deny` default those sessions will now start
receiving 403s once they cross it. Audit any value currently set before upgrading.
OpenAI-format endpoints were never affected: that path has always read
`completion_tokens_details.reasoning_tokens`.

**`max_calls` enforcement scope.** Only inference calls surfaced by
`inference-parser` increment the counter, but the limit check runs on
every outbound request. Once the LLM counter crosses `max_calls`, the
next outbound of any kind (MCP tool call, A2A message, etc.) is the one
that gets the 403.

**Pipeline position:** must appear **before** `inference-parser` on the outbound
pipeline. Both must be present for token counting (inference-parser supplies the
token counts session-budget accumulates).

## Modes

### `deny` (default)

Returns 403 with a JSON body:

```json
{
  "error": "budget.exceeded",
  "message": "token limit reached: 50200/50000",
  "details": {
    "spent_tokens": 50200,
    "spent_calls": 42,
    "token_limit": 50000,
    "call_limit": 100,
    "duration_seconds": 1205,
    "duration_limit": 1800
  }
}
```

`duration_seconds` / `duration_limit` are included only when
`max_duration_seconds` is set.

### `observe` (shadow mode)

Counters still accumulate and limits are still evaluated, but breaches only emit
a WARN log (`"budget exceeded (shadow mode)"`) and the request continues. Use to
calibrate limits before enforcing:

1. Deploy with `on_exceed: observe` and conservative limits.
2. Watch logs for shadow-mode entries.
3. Adjust `max_tokens` / `max_calls` / `max_duration_seconds` to fit real
   workloads.
4. Flip to `on_exceed: deny` (or `pause`) once confident.

Call accounting is response-driven — `max_calls` increments on
response completion, not request entry. Cache-at-limit rejects
reliably, but concurrent requests near the limit can overshoot by up
to the in-flight count before responses catch up. Treat `max_calls`
as best-effort under bursty concurrency.

### `pause` (HITL webhook)

On breach, POST to `pause_webhook` and block the request until the webhook
responds or `pause_timeout` fires.

The webhook is any HTTP endpoint you build that speaks the contract
below — an in-cluster Service, a workflow entrypoint (Temporal, Argo,
Slack middleware), or a local stub. It must be reachable from the
AuthBridge pod and respond within `pause_timeout` (default `30s`), or
the plugin falls back to `pause_timeout_action`.

**Contract:**

| Aspect | Requirement |
|--------|-------------|
| Method | `POST` |
| URL | Exactly the `pause_webhook` value (no path templating) |
| Request `Content-Type` | `application/json` |
| Request body | See below — always the same schema |
| Success response | HTTP `200` with `application/json` body containing an `action` field |
| Response body cap | 4 KiB (larger responses are truncated at decode) |
| Latency budget | Must respond within `pause_timeout`; slow webhooks block the caller |
| Auth | None injected by the plugin — add your own (mTLS, network policy, IP allowlist) at the transport layer |
| Retries | None — the plugin calls once per breach |

**Request body:**
```json
{
  "session_id": "abc-123",
  "reason": "call limit reached: 50/50",
  "spent_tokens": 48200,
  "spent_calls": 50,
  "token_limit": 100000,
  "call_limit": 50,
  "duration_seconds": 1205,
  "duration_limit": 1800
}
```

**Expected response:**
```json
{"action": "approve"}
```
or
```json
{"action": "deny", "reason": "operator rejected"}
```

**On approval:** the request continues; subsequent requests from the
same session skip the webhook for `pause_grace_period` (default `5m`,
pod-local — each pod fires one webhook before its own grace kicks in).
Concurrent breaches during an in-flight webhook wait on the pending
call and honor its outcome — all approved together, or all denied
together.

**On timeout / non-200 / bad JSON / unreachable:** falls back to
`pause_timeout_action` (`deny` returns 403; `allow` continues). Client
disconnect mid-wait does **not** trigger this fallback — the webhook is
bounded only by `pause_timeout`, so its verdict still lands for any
followers on the same flight. If your webhook can be unhealthy and
`pause_timeout_action: deny`, an outage turns budget breaches into hard
403s.

If a human is in the loop, bump `pause_timeout` to minutes so the
request can wait for a real approval decision.

## Failure Modes

| Scenario | Behavior |
|----------|----------|
| Redis down at startup | Fail-open until refresh populates cache |
| Redis fails mid-session | Local cache keeps enforcing; writes dropped |
| Pod restart, `pause` | Request #1 hydrates from Redis synchronously |
| Pod restart, `deny` / `observe` | Request #1 skips (`cold_cache`); see below |
| Hydrate timeout in `pause` (Redis p99 > 200ms) | Logs WARN, falls through to `cold_cache` without populating counters; every request retries the hydrate and bypasses the webhook until Redis responds within the deadline |
| Webhook unreachable | Falls back to `pause_timeout_action` |

### Cold-cache behavior

When a request arrives with no local cache entry for the session:

- **`pause`** — hydrates from Redis synchronously, so an over-budget
  session fires the webhook on request #1.
- **`deny` / `observe`** — skip with `reason=cold_cache` and continue.
  Counters populate as inference responses stream back and via the
  background refresh loop. A pre-existing over-budget session may pass
  **up to one request per pod** before enforcement resumes. Keeps Redis
  off the hot path.

Redis unavailability degrades enforcement to local-cache-only rather
than blocking requests.

**Token counting requires `usage` in provider responses.** Providers that omit
`usage` from streaming chunks (e.g. Anthropic via LiteLLM) will show
`promptTokens=0` in inference-parser logs — `max_tokens` enforcement won't
trigger, but `max_calls` and `max_duration_seconds` still apply. Ollama,
OpenAI, and Azure OpenAI include usage in streaming responses and work fully.

## Redis Keys

```text
session-budget:<session-id>   (Hash, TTL = session_ttl_seconds)
  tokens       cumulative token count
  calls        inference call count
  started_at   first-call unix timestamp
```

## Local Development

**Redis / Valkey:**

```bash
docker run -d --name valkey -p 6379:6379 valkey/valkey:latest
# redis_url: redis://localhost:6379  (or host.docker.internal from a container)
```

**Pause-mode webhook stub.** A one-liner that returns `approve` for every
request — enough to smoke-test `on_exceed: pause` end-to-end:

```bash
docker run -d --name pause-webhook -p 8888:8888 \
  python:3.12-alpine@sha256:d09d15e60962ca365d1cd544a48773bac9d33f2fb1b00f2aa0deec78ade7dc31 \
  python -c "import http.server,json; \
h=type('H',(http.server.BaseHTTPRequestHandler,),{ \
'do_POST':lambda s:(s.send_response(200), \
s.send_header('Content-Type','application/json'),s.end_headers(), \
s.wfile.write(b'{\"action\":\"approve\"}'))}); \
http.server.HTTPServer(('',8888),h).serve_forever()"

# pause_webhook: http://localhost:8888  (or http://host.docker.internal:8888)
```

Swap `approve` for `deny` to test the reject path. Logs land in
`docker logs pause-webhook`.

## In-cluster deployment note

**If your namespace runs Istio ambient mesh** (label
`istio.io/dataplane-mode: ambient`), the Valkey pod and any plain-HTTP
pause webhook need to opt out with the pod-level label
`istio.io/dataplane-mode: none`. Ambient's ztunnel only accepts HBONE
(HTTP/2 CONNECT), and Redis RESP is raw TCP — the connection is closed
before reaching Valkey. Symptom: `session-budget action=skip
reason=cold_cache` on every request even though the Redis key exists.

For an in-cluster stub, apply
`demos/session-budget/k8s/pause-webhook-stub.yaml` and set
`pause_webhook: http://pause-webhook-stub.team1.svc.cluster.local`. See
[`../demos/session-budget/README.md`](../demos/session-budget/README.md).

**Run the plugin tests:**

```bash
cd core
go test ./plugins/sessionbudget/... -v -count=1
```
