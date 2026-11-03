# litellm-budget-track Plugin

Design document for the `litellm-budget-track` AuthBridge pipeline plugin.

**Issue:** https://github.com/rossoctl/rossoctl/issues/2177

## Overview

The `litellm-budget-track` plugin provides daily budget enforcement for AI agents
proxied through LiteLLM. It accumulates the per-request cost that
`inference-parser` settles — authoritative from LiteLLM's
`x-litellm-response-cost` header where present, modelled from token usage and
the [`pricing:`](pricing.md) table otherwise — into a per-day ledger, and
rejects requests with HTTP 429 when the configured daily budget is exceeded.

## Use Case

When running AI agents through Cortex (local budget proxy), each agent needs
a spending cap. LiteLLM returns the cost of each completion in the
`x-litellm-response-cost` response header, which `inference-parser` settles
into a per-request cost record. This plugin accumulates that record in the
AuthBridge pipeline that carries the LLM traffic (see [Pipeline placement](#pipeline-placement)),
tracks cumulative daily spend in a JSON ledger file, and blocks further requests
once the budget is exhausted.

## Architecture

```
Agent → agentop exec / sidecar (litellm-budget-track) → LiteLLM upstream
                              │
                              ├── OnRequest: check if budget exceeded → 429
                              └── OnResponseFrame: load the cost record inference-parser
                                     settled, accumulate, amend
                                     │
                                     └── spend-authbridge.json (daily ledger)
```

The plugin hooks:
- `OnRequest` — pre-flight budget check (reject if over limit).
- `OnResponseFrame` — post-flight cost accounting for **every** response. Because the
  plugin is a `StreamingResponder`, in-tree listeners route all responses through this
  hook — a buffered `application/json` body as a single terminal frame, a streamed
  `text/event-stream` body frame-by-frame — and `pipeline.RunResponse` skips the plugin
  unconditionally. (`OnResponse` remains only as a fallback for a hypothetical listener
  that calls it but never `OnResponseFrame`; no in-tree listener does.)

### Cost source (what the terminal frame charges)

**Since cortex #972 this plugin does not settle the cost itself.** `inference-parser` does,
via `core/cost/settle`, because it is the component that knows when token usage is final; this
plugin bills the published figure, adds the day's total, enforces the cap, and reports drift.
The two sources below are still the rule — they just live in one place now, shared with every
other consumer of a cost, instead of being implemented here and again in the usage
aggregator.

The cost is settled **once**, on the terminal frame, from one of two sources:

- **Response header** — `x-litellm-response-cost`, falling back to the pre-discount
  `-original` variant. Used whenever the header carries a usable positive cost. A
  header of `0` on a non-streamed response is a genuine free call (cache hit / error)
  and is charged `0` — it is **not** re-priced from usage.
- **Parsed token usage × the `pricing:` table** — used only when the cost header is
  **absent**, or the response is `text/event-stream` (LiteLLM always reports `0` in the
  header for streams, e.g. Claude Code's `/v1/messages`). The parser sums the token
  usage from the terminal SSE events and prices each **prompt-cache tier separately**:
  uncached input × `input_cost_per_million`, cache writes ×
  `cache_write_cost_per_million`, cache reads × `cache_read_cost_per_million`,
  output × `output_cost_per_million` — the top-level [`pricing:`](pricing.md) keys,
  not fields of this plugin. A tier with no rate leaves the model unpriced for it,
  named in `/v1/usage`'s `unpricedBy`.

  **Cache tiers matter.** Providers charge a premium to *write* a cache entry and a
  steep discount to *read* one, so two requests with identical prompt-token counts can
  differ ~10× in price. There is no cache-to-input fallback: a model whose
  `cache_write_cost_per_million` / `cache_read_cost_per_million` are unset is unpriced
  on those tiers, not flat-priced — set the two cache rates to your provider's real
  prices for accurate budgets. (This only affects the usage-fallback path; when
  LiteLLM's `x-litellm-response-cost` header is present it already accounts for cache
  tiers and wins.)

> **envoy-sidecar note.** Because the plugin declares `ReadsBody`, the extproc listener
> requests `ResponseBodyMode: BUFFERED` — so on the envoy-sidecar path the SSE body is
> buffered (capped at that listener's 1 MB `maxBodySize`) and "frame-by-frame" means
> re-parsed from the buffered body, not incrementally as events arrive. The proxy
> (forward/reverse) listeners stream frame-by-frame as normal.

## Files

| File | Purpose |
|------|---------|
| `core/plugins/litellm_budgettrack/plugin.go` | Plugin implementation |
| `core/plugins/all/plugins_litellm_budgettrack.go` | Registration (build-tag gated) |

## Plugin Configuration

In the AuthBridge `config.yaml` pipeline section:

```yaml
pipeline:
  inbound:
    plugins:
      - name: litellm-budget-track
        config:
          spend_file: /etc/cortex/spend-authbridge.json
          max_budget: 5.00
      # REQUIRED, and required AFTER this plugin. The response pass walks the chain
      # in reverse, so the parser must sit at a higher index to fold token counts
      # before the cost is settled. A chain without it fails to build — see
      # "Rates and ordering" below.
      - name: inference-parser
```

### Pipeline placement

The plugin is direction-agnostic — place it in whichever pipeline carries the LLM
traffic: **`inbound`** when fronting the LLM endpoint (a reverse proxy the LLM requests
arrive at, as shown above), **`outbound`** when hosting the agent via
`agentop exec -- <agent>`, whose forward proxy runs the outbound pipeline on
the agent's own egress to LiteLLM. In an `agentop exec` setup nothing reaches the
inbound pipeline, so a plugin left under `inbound:` there records `$0` — use `outbound:`.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spend_file` | string | yes | Path to the JSON ledger file (created if missing) |
| `max_budget` | float | yes | Daily budget in USD (must be > 0) |

**Rates are no longer options here.** The four `*_cost_per_token` fields were
removed; they now live in the top-level `pricing:` section. A config still carrying
them fails to start with the offending field named, rather than dropping them
silently and switching the deployment to vendor-list rates. The old
"cache rates default to `input_cost_per_token`" rule is gone too — a tier with no
rate makes the request *unpriced*, because that default overstated a cache read by
10x while still counting the request as priced.

## Ledger Format

The spend file (`spend-authbridge.json`) is a simple JSON object:

```json
{
  "date": "2026-07-09",
  "total_spend": 0.0234,
  "total_calls": 12
}
```

- Resets automatically at midnight UTC (when `date` doesn't match today)
- Written atomically after each response (no rotation needed)
- Safe for single-process use (mutex-protected in-memory + file sync)

## Behavior

### OnRequest (pre-flight check)

1. Lock mutex
2. If `date` != today UTC → reset ledger to zero
3. If `total_spend >= max_budget` → return HTTP 429 with body:
   ```
   Cortex ExceededTokenBudget: daily spend $X.XXXX exceeds budget $Y.YY. Reset at midnight UTC.
   ```
4. Otherwise → continue pipeline

### OnResponseFrame (cost accumulation)

1. Load the **cost record** `inference-parser` settled for the request
   (`settle.Load`). The parser is what reads LiteLLM's
   `X-Litellm-Response-Cost` (falling back to the pre-discount `-original`
   variant) and prices the response; this plugin holds no rates and resolves
   no headers.
2. No record, or one carrying no figure → continue: nothing to accumulate
3. Lock mutex, reset if new day
4. Add the settled cost to `total_spend`, increment `total_calls`, and
   annotate the record through `settle.Amend`; the drift check against the
   gateway's own figure runs before accumulation and WARN-logs only
5. Write ledger to disk
6. Continue pipeline

## Observability

Each priced response surfaces on the session-event stream at
`SessionEvent.Plugins["litellm-budget-track"]`, so consumers can read
per-response cost without duplicating the pricing math or reading the file
ledger.

**A zero-cost cache hit now DOES produce an event** — this line previously said the
opposite, and it was the one case that mattered. A gateway that reports a parsed,
finite, exactly-zero cost on a non-streamed response is *declaring the call free*,
which is a different answer from nobody having priced it. The event carries
`settled: true` so the usage aggregator does not fall through to its rate table and
invent a cost for it.

An event is NOT emitted when the response is genuinely unpriced: no usable header
*and* no rate covering the tiers it used. That silence is deliberate — it is what
lets `/v1/usage` name the endpoint and model in `unpricedBy`. A header that is
unparseable, negative or non-finite counts as unpriced, not as a declared zero: a
garbage header says nothing about whether the call was free. Nor does a zero on a
*streamed* response, where LiteLLM stamps 0 by design because the total is unknown
when headers are sent.

| Field | Type | Meaning |
|-------|------|---------|
| `cost_usd` | float64 | Cost of this single response, in dollars. |
| `source` | string | `"gateway-header"` (authoritative — LiteLLM stamped the header) or `"usage-fallback"` (priced from token counters, used for streamed responses whose header always reports 0). |
| `daily_total_usd` | float64 | Ledger total after this response was added. |
| `daily_max_usd` | float64 | Configured daily cap (`max_budget`). |
| `provenance` | string | Where the figure came from: `"authoritative"` (the gateway reported it), or the rate table's level — `"configured"`, `"discovered"`, `"bundled"`. Omitted when absent. |
| `settled` | bool | `true` when the producer settled this figure deliberately, **including a zero**. Distinguishes "this call was free" from "nobody priced this", which a bare `cost_usd: 0` cannot. Omitted when false. |

Both trailing fields are **additive**: the four above them are unchanged, and a
consumer that knows only those four decodes an event from either version unaltered.

See [`plugin-reference.md#emitting-session-events`](./plugin-reference.md#emitting-session-events)
for how the listener promotes `pctx.Extensions.Custom` entries to
`SessionEvent.Plugins`.

### Deny events

`OnRequest` rejects with HTTP 429 once the daily budget is exhausted,
and surfaces the denial as a `phase: "denied"` session event carrying
an `Invocation` with these `details`:

| Key | Meaning |
|-----|---------|
| `daily_total_usd` | Ledger total at denial; same quantity as the cost event's `daily_total_usd`, formatted like the 429 wire body. |
| `daily_max_usd` | Configured daily cap (`max_budget`). |
| `total_calls` | Ledger call count at denial. |

### Consumers

The event's wire shape is declared once, in `core/cost/event` (`event.Event`,
published under `event.PluginName`). Producer and consumers share that one
declaration rather than each keeping a private copy, so a field rename is a
compile error rather than a silently blank column:

- **The usage aggregator** (`core/cost/usage`) records `cost_usd` into
  `Counts.CostMicros` and increments `Counts.PricedRequests`, so `/v1/usage`
  reports the same figure this plugin enforces its budget against.
- **`agentop`** renders the per-request figure in its events pane, and the window
  total plus coverage in the usage footer. Its `tui.costEvent` is a type *alias*
  for `event.Event`, not a copy.

Two consequences for reading `/v1/usage`. Cost is settled by
`inference-parser` and reaches `/v1/usage` whether or not this plugin is
configured, so requests no rate covers appear as the gap between
`totals.pricedRequests` and `totals.priceableRequests` — a client rendering a
dollar total from a window where those differ must present it as partial, not
complete. And `priced:false` means nothing at all was priced: render "cost
unavailable", never `$0.00`, which would read as "this traffic was free".

Modelled rates for traffic with no cost event arrive with the pricing resolver —
see [`superpowers/specs/2026-09-09-pricing-consolidation-design.md`](./superpowers/specs/2026-09-09-pricing-consolidation-design.md).

## Build

Every plugin is opt-in. This one is carried by the `full`, `lite` and `envoy` profiles;
to link it explicitly:

```bash
go build -tags include_plugin_litellm_budgettrack ./cmd/cortex/
```

The registration file uses the standard build-tag pattern:

```go
//go:build include_plugin_litellm_budgettrack

package all

import _ "github.com/rossoctl/cortex/core/plugins/litellm_budgettrack"
```

## Configuring it

The plugin is configured through the runtime config — the
`authbridge-runtime-config` ConfigMap in a cluster, `~/.cortex/config.yaml` on a
local install — with the `spend_file` path and `max_budget` in its `config:`
block:

```yaml
pipeline:
  inbound:
    plugins:
      - name: litellm-budget-track
        config:
          spend_file: /var/lib/authbridge/spend-authbridge.json
          max_budget: 5.00
```

For CONNECT-tunneled traffic that flows through the TLS bridge, this plugin is
the only cost-tracking mechanism — see [`docs/pricing.md`](pricing.md) for how
the cost pipeline prices the same traffic from model rates.

## Testing

```bash
cd core/plugins/litellm_budgettrack

# Run the plugin in a test pipeline
go test -v ./...

# Or build cortex with the plugin and test end-to-end, from the repo root:
go build ./cmd/cortex/
./cortex --config test-config.yaml
# Send requests with x-litellm-response-cost header in responses
```

## Future Work

- Per-agent budget tracking (separate ledger files per agent identity)
- Budget alerts at configurable thresholds (e.g., 80% warning)
- Weekly/monthly budget periods (not just daily)
- Integration with cortex control API for real-time budget queries


## Rates and ordering (changed)

Rates are no longer plugin options. The four `*_cost_per_token` knobs are gone; the
plugin prices the per-tier token counts `inference-parser` publishes, through the
top-level `pricing:` section. Its own SSE token parser is gone with them — one
parser, one rate table, one place tokens become dollars.

**BREAKING — `inference-parser` must appear AFTER this plugin in the chain.**

That reads backwards and is not a typo. The response passes walk the chain in
reverse (`pipeline.RunResponseFrame`), so the parser needs a *higher* index to fold
each frame before this plugin settles the cost on the terminal one. The requirement
is declared as `RequiresLater`, and a chain that gets it wrong now fails to build.
Before that check existed, the wrong order built cleanly and silently unpriced every
streamed response — the counts simply were not there yet.

```yaml
pipeline:
  outbound:
    plugins:
      - name: litellm-budget-track   # settles cost LAST on the response pass
      - name: inference-parser       # folds token counts FIRST on the response pass
```

Two further behaviour changes:

- **Cache tiers with no rate are unpriced, not defaulted.** The old default charged
  them at the uncached input rate, overstating a cache read by 10x while still
  counting the request as priced — so the error was invisible. A tier that carried
  tokens without a rate now makes the request unpriced, which is a visible gap.
- **The ledger quantizes to micros**, because `pricing.Cost` returns integer
  millionths so bucket addition stays exact. It costs a millionth of a dollar per
  request and makes the ledger agree with `/v1/usage` to the last digit.

The cost event gained a `provenance` field, **additively**: the original
`cost_usd` / `source` / `daily_total_usd` / `daily_max_usd` tags are unchanged and a
consumer that knows only those four still decodes. `source` is retained rather than
replaced — it says which *path* priced the request (gateway header vs token counts),
where `provenance` says how much to trust the rates.


## Drift detection

A non-streamed response carries both the gateway's own settled cost and the token counts,
so the modelled figure can be checked against the real one for free. When they diverge by
more than 5%, the plugin warns once per endpoint and model:

```
WARN pricing: the rate table disagrees with what the gateway charged
     endpoint=gw.internal model=claude-opus-5 modelled_usd=0.085000
     gateway_usd=0.064600 ratio=1.316x
     effect=overstating every request this table prices, including streamed ones
     fix=set pricing.endpoints[].multiplier for this endpoint ...
```

That matters because **streamed responses have no authoritative cost** — the gateway
reports 0 in the header by design, since the total is unknown when headers are sent. So a
misconfigured rate table is invisible on exactly the traffic an agent generates. The
occasional non-streamed call is the only place the error is observable, and this is what
it looks like.

Once per endpoint and model, not per request: an agent makes thousands of calls, and a
per-request warning would bury every other line in the log.

The ledger always uses the authoritative figure. Drift is a diagnostic about the rate
table, never a reason to distrust the gateway's own number.
