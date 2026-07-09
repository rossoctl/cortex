# litellm-budget-track Plugin

Design document for the `litellm-budget-track` AuthBridge inbound pipeline plugin.

**Issue:** https://github.com/kagenti/kagenti/issues/2177

## Overview

The `litellm-budget-track` plugin provides daily budget enforcement for AI agents
proxied through LiteLLM. It reads the `x-litellm-response-cost` header from
upstream LiteLLM responses, accumulates per-day spending, and rejects requests
with HTTP 429 when the configured daily budget is exceeded.

## Use Case

When running AI agents through RossoCortex (local budget proxy), each agent needs
a spending cap. LiteLLM returns the cost of each completion in the
`x-litellm-response-cost` response header. This plugin reads that header in the
AuthBridge inbound pipeline, tracks cumulative daily spend in a JSON ledger file,
and blocks further requests once the budget is exhausted.

## Architecture

```
Agent → rossocortex.py → AuthBridge (litellm-budget-track) → LiteLLM upstream
                                    │
                                    ├── OnRequest: check if budget exceeded → 429
                                    └── OnResponse: read x-litellm-response-cost, accumulate
                                           │
                                           └── spend-authbridge.json (daily ledger)
```

The plugin runs in the **inbound** pipeline direction:
- `OnRequest` — pre-flight budget check (reject if over limit)
- `OnResponse` — post-flight cost accumulation (read header, update ledger)

## Files

| File | Purpose |
|------|---------|
| `authbridge/authlib/plugins/litellm_budgettrack/plugin.go` | Plugin implementation |
| `authbridge/cmd/authbridge-proxy/plugins_litellm_budgettrack.go` | Registration (build-tag gated) |

## Plugin Configuration

In the AuthBridge `config.yaml` pipeline section:

```yaml
pipeline:
  inbound:
    plugins:
      - name: litellm-budget-track
        config:
          spend_file: /etc/rossocortex/spend-authbridge.json
          max_budget: 5.00
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spend_file` | string | yes | Path to the JSON ledger file (created if missing) |
| `max_budget` | float | yes | Daily budget in USD (must be > 0) |

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
   Rossocortex ExceededTokenBudget: daily spend $X.XXXX exceeds budget $Y.YY. Reset at midnight UTC.
   ```
4. Otherwise → continue pipeline

### OnResponse (cost accumulation)

1. Read `X-Litellm-Response-Cost` header from upstream response
2. If missing or non-positive → continue (no cost to track)
3. Lock mutex, reset if new day
4. Add cost to `total_spend`, increment `total_calls`
5. Write ledger to disk
6. Continue pipeline

## Build

The plugin is included by default in `authbridge-proxy` builds. To exclude:

```bash
go build -tags exclude_plugin_litellm_budgettrack ./cmd/authbridge-proxy/
```

The registration file uses the standard build-tag pattern:

```go
//go:build !exclude_plugin_litellm_budgettrack

package main

import _ "github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/litellm_budgettrack"
```

## Integration with RossoCortex

RossoCortex generates the AuthBridge config on startup, embedding the plugin
in the inbound pipeline with the correct `spend_file` path and `max_budget`
from the CLI flags:

```bash
# rossoctlx.py start --budget 5.00
# → generates config.yaml with litellm-budget-track plugin
#   spend_file = ~/.config/rossocortex/spend-authbridge.json
#   max_budget = 5.00
```

The plugin complements rossocortex.py's own budget tracking (which reads
the same `x-litellm-response-cost` header on direct HTTP requests). For
CONNECT-tunneled traffic that flows through AuthBridge's TLS bridge,
the plugin is the only cost-tracking mechanism.

## Testing

```bash
cd authbridge/authlib/plugins/litellm_budgettrack

# Run the plugin in a test pipeline
go test -v ./...

# Or build authbridge-proxy with the plugin and test end-to-end:
cd authbridge
go build ./cmd/authbridge-proxy/
./authbridge-proxy --config test-config.yaml
# Send requests with x-litellm-response-cost header in responses
```

## Future Work

- Per-agent budget tracking (separate ledger files per agent identity)
- Budget alerts at configurable thresholds (e.g., 80% warning)
- Weekly/monthly budget periods (not just daily)
- Integration with rossocortex control API for real-time budget queries
