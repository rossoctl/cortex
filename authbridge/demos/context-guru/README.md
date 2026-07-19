# context-guru demo — context engineering that keeps an agent under its window

This demo runs the **context-guru** AuthBridge plugin in front of a real Rossoctl
agent and shows the plugin doing real context engineering: it compacts the
agent's growing tool-output context before it reaches the LLM, so a task whose
raw context **exceeds the model's window** still fits — and the agent gets the
right answer *because of* the compaction.

Same agent, same model, same window. The only variable is context-guru:

| mode | context-guru | request the model sees | agent answer |
|------|-------------|------------------------|--------------|
| **off** | disabled (kill-switch) | raw **~18K tok** → **truncated** to the 12K window | ❌ misses the anomaly, hallucinates a wrong refund |
| **observe** | shadow (measures, doesn't apply) | raw ~18K tok (truncated); logs it *would* save 52KB→30KB | ❌ same wrong answer — proves the measurement is free |
| **enforce** | applied | compacted **~10K tok** → **fits** | ✅ finds the TX4827 duplicate, clears the others |

## Architecture

context-guru is an **in-process AuthBridge plugin** (not a sidecar service). The
agent's outbound LLM calls are routed through AuthBridge's forward proxy
(`HTTP_PROXY=:8081`); the plugin runs in the **outbound** pipeline and rewrites
the request body before it leaves the pod.

```
             one Kubernetes pod (cg-finance-agent, namespace team1)
 ┌───────────────────────────────────────────────────────────────────────┐
 │  agent container                    authbridge-proxy container          │
 │  ┌───────────────┐  HTTP_PROXY      ┌────────────────────────────────┐  │
 │  │ finance-agent │  =127.0.0.1:8081 │  forward proxy :8081            │  │
 │  │  (A2A server, │ ────────────────▶│    │ OUTBOUND pipeline          │  │
 │  │  Ollama tool- │  POST /v1/chat/  │    ▼                            │  │
 │  │  calling)     │  completions     │  inference-parser  (reads body) │  │
 │  └──────┬────────┘                  │  context-guru      (PLUGIN)     │  │
 │         │ MCP tool calls            │    ├ dedup      ─┐               │  │
 │         │ (finance-mcp)             │    ├ extract:code│ compacts the  │  │
 │         ▼                           │    └ collapse   ─┘ tool context  │  │
 │   finance-mcp svc                   │        │ SetBody(compacted)      │  │
 │   (large audit logs)                └────────┼────────────────────────┘  │
 └────────────────────────────────────────────┼─────────────────────────────┘
                                               ▼  compacted request
                                    Ollama  (llama3.2, 12K-token window)
```

The pipeline is `inference-parser → context-guru`. context-guru is the single
outbound `WritesBody` plugin (mutually exclusive with `sparc`).

## The engine: 2 deterministic reducers + extract-code

context-guru's own engine (configured under `context-guru.config.engine`) runs
`[dedup, extract, collapse]` over the message array:

- **dedup** *(deterministic)* — replaces a tool output byte-identical to an earlier
  one with a short pointer.
- **extract** `strategy: code` *(LLM-backed)* — a cheap model writes a **sandboxed,
  deletion-only, containment-proven** projection of each large tool output (keeps
  the query-relevant lines, drops the noise). Falls back to a deterministic
  projection if the model is unavailable or its output fails the proof.
- **collapse** *(deterministic)* — a gentle head/tail net for anything extract left.

`marker_mode: summary` (v1) means a dropped span leaves a `⟪cg⟫` breadcrumb and is
**not** stashed for restoration (there is no expand loop yet — see *Not in this
integration*). Use `full` once restoration lands to make it reversible.

## The task

One A2A `message/send` asks the agent to audit **three** transactions
(`TX4827`, `TX5310`, `TX2981`) for duplicate settlements. The agent pulls each
one's audit trail (`get_transaction_audit`, ~11 KB of mostly heartbeat noise) plus
a customer ledger (`get_customer_ledger`, ~8 KB) via MCP. **Only `TX4827`** has a
planted `ANOMALY: duplicate settlement` line buried mid-log — the needle in ~18K
tokens of haystack.

## What context-guru does (observed)

From the AuthBridge sidecar logs (the plugin logs every compaction):

```
context-guru configured        paths="[/v1/chat/completions /v1/messages]" modelConfigured=true
context-guru compacted request  session=default tokensBefore=18399 tokensAfter=9938 pctSaved=46.0% components=[extract]
context-guru rewrote request body  provider=openai bytesBefore=51935 bytesAfter=30005 pctSaved=42.2%
```

And the observed answers:

- **enforce:** *"there is a duplicate settlement for TX4827 … a refund is warranted for
  TX4827. For TX5310 and TX2981, no duplicate settlements were found."* ✅
- **off / observe:** *"The refund for TX2981 has been issued …"* ❌ — the raw context was
  truncated at the window, the TX4827 audit fell out, and the small model
  hallucinated.

## Run it

```bash
export CG_MODEL_KEY=<api key for the extract-code model>       # e.g. an OpenAI-compatible key
export CG_MODEL_BASE=https://api.openai.com                    # any OpenAI-wire endpoint
./run.sh all        # setup + drive off, observe, enforce and print the comparison
# or step by step:
./run.sh setup
./run.sh drive enforce      # (or observe / off)
```

`run.sh setup` builds the `authbridge-proxy` image **with the context-guru plugin**
(`-tags include_plugin_contextguru` — see *Build integration* below), loads it + the
enhanced `finance-mcp` into the `rossoctl` kind cluster, creates a 12,288-token-window
Ollama model (`llama3.2-ctx12k`) so the raw request truncates, and deploys the agent
+ sidecar. `run.sh drive <mode>` flips `on_error`, restarts, drives the audit, and
prints the agent's answer + the byte/token gain from the sidecar session API (`:9094`).

## Build integration

context-guru is **opt-in**: unlike the other plugins (compiled in by default,
dropped via `-tags exclude_plugin_*`), it is linked only when the binary is built
with `-tags include_plugin_contextguru`. Its embedded engine pulls a large
transitive dependency set (bifrost/core, tiktoken-go, tree-sitter grammars,
starlark), so the default `authbridge-proxy`/`authbridge-envoy` binaries stay lean
and a deployment that doesn't want compaction never pays for it.

```bash
cd authbridge && podman build -f cmd/authbridge-proxy/Dockerfile \
  --build-arg GO_BUILD_TAGS=include_plugin_contextguru -t authbridge-cg:latest .
```

## Configuration reference

`k8s/authbridge-config.yaml` — the sidecar config. Key knobs:

| field | meaning |
|-------|---------|
| `pipeline.outbound.plugins` | `inference-parser` then `context-guru` (order matters; parser first) |
| `context-guru.on_error` | `enforce` (apply) / `observe` (shadow-measure) / `off` (kill-switch) |
| `config.paths` | request paths to compact; others pass through untouched |
| `config.model.{base_url,model,api_key}` | the cheap OpenAI-wire model for `extract:code`; `api_key` from a Secret via `${CG_MODEL_KEY}` |
| `config.engine.pipeline` | context-guru components in run order |
| `config.engine.components.extract.strategy` | `deterministic` \| `code` \| `rlm` |
| `config.engine.components.extract.marker_mode` | `full` (reversible, stashes) \| `summary` (breadcrumb) \| `off` (silent) |
| `config.engine.components.*.trigger` / thresholds | per-component gates (min tokens, head/tail lines) |

`k8s/agent.yaml` deploys the agent + sidecar with **no `rossoctl.io/inject` label**,
so the demo owns both containers and the config (the operator webhook doesn't
inject a second sidecar). The extract-code key lives in the `cg-model-key` Secret.

## Notes & knobs

- **Window is the lever.** Host Ollama loads `llama3.2:3b` with a 131072 window, so
  nothing truncates by default; the demo pins `num_ctx=12288` (via `llama3.2-ctx12k`)
  to make "exceeds the window" concrete. Point `OLLAMA_MODEL` back at `llama3.2:3b`
  to see the compaction gain without truncation.
- **observe mode** is the safe way to quantify the gain on production traffic before
  enforcing — it records `body-mutation` (bytes before/after) and the `context-guru
  compacted request` log line without altering the request.
- **collapse stays gentle** (`head/tail: 12`); `extract` (query-aware) is the primary
  reducer that preserves the mid-log needle. Very aggressive collapse can drop it.
- **context-guru + SPARC are mutually exclusive** on the outbound chain (one WritesBody slot).

## Files

- `k8s/authbridge-config.yaml` — sidecar config (pipeline, engine, the 3 modes).
- `k8s/agent.yaml` — agent + context-guru sidecar Deployment/Service.
- `run.sh` — `setup` / `drive <mode>` / `all`.
