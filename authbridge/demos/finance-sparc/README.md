# finance-sparc demo — SPARC pre-tool reflection on a regular agent

This demo shows the AuthBridge [`sparc` plugin](../../docs/sparc-plugin.md) catching a
hallucinated tool argument before it executes. SPARC's clarification is returned to the agent
as an MCP tool result, so the agent asks the user for the missing detail and then runs the
corrected call.

The agent is an ordinary kagenti agent: it discovers its tools via MCP `tools/list`, reasons
with an Ollama model, and acts through an MCP finance backend. SPARC only sees what the agent
itself produces — the full conversation (including the system prompt), the discovered tool
specs in OpenAI format, and the proposed tool call. Those are collected by the parsers from the
agent's own LLM call; nothing is hardcoded or passed to SPARC out of band.

## The scenario (two turns)

1. "Refund my duplicate $450 subscription charge from last week."
   The user gives no transaction id and there is no search tool, so the model invents a value
   (e.g. `"$450"`) for the required `transaction_id` and calls `issue_refund`. SPARC
   (`fast_track`) finds the argument ungrounded and rejects it; the plugin returns a
   clarification as the tool result, and the agent relays it and asks for the exact id.
2. "The transaction id is TX4827. Please proceed."
   Now the id is grounded in the conversation, SPARC approves, and the refund is issued.

Example run (watsonx reflection):

```
Turn 1  agent> This tool call was not executed because it could not be verified... ask for the exact transaction_id
Turn 2  agent> Your transaction with ID TX4827 has been successfully refunded...

SPARC modify/reflected  tool=issue_refund  score=0.00   # Turn 1: ungrounded "$450" → reject + clarify
SPARC allow/grounded    tool=issue_refund  score=1.00   # Turn 2: grounded TX4827 → approve → refund
```

## Prerequisites

- A **kagenti kind cluster** with the AuthBridge operator. Core + SPIRE is enough:
  ```bash
  # from the kagenti repo:
  scripts/kind/setup-kagenti.sh --with-spire
  ```
- `kubectl`, `kind`, `docker` (or `podman`), `python3`, `uv`.
- **Ollama models.** What you need depends on the reflection provider:
  - **`make demo` (watsonx reflection):** just the agent's reasoning model in your host
    Ollama store — `ollama pull llama3.2:1b`. The demo stages it into an *in-cluster* Ollama
    (no host networking needed for this path).
  - **`make demo PROVIDER=ollama` (local reflection):** a **running host Ollama** reachable
    from the cluster, with a reasoning model and a **capable** reflection model:
    ```bash
    ollama pull llama3.2:3b     # reasoning (clean tool calls)
    ollama pull gemma4:e4b      # reflection — needs a capable instruct model; 1b/3b
                                # hallucinate ALTK's function-selection metric. Override
                                # with SPARC_MODEL=<your capable model>.
    ```
    Why the host Ollama for this path: ALTK reflects on ollama via **free-form
    generate-then-validate** (it disables guided decoding for ollama), which is fast on a
    Metal/GPU host but ~140s/call on a CPU-only in-cluster node. See [Provider notes](#provider-notes).

## Run it

```bash
cd authbridge/demos/finance-sparc

# Option A — watsonx as the reflection LLM (best quality, fastest reflection):
set -a; . /path/to/your/.env; set +a     # provides WX_API_KEY / WX_PROJECT_ID
make demo

# Option B — fully local, no credentials (Ollama reflection on your host Ollama):
make demo PROVIDER=ollama
#   Rancher Desktop / colima: the host Ollama isn't at host.docker.internal, so:
#   make demo PROVIDER=ollama HOST_OLLAMA_URL=http://192.168.5.2:11434
```

`make demo` is one-shot and idempotent. It: checks cluster egress (a no-op on healthy
networks), builds + loads images (including an authbridge image carrying the `sparc` plugin),
stages + warms the in-cluster Ollama model(s), deploys the SPARC service + finance MCP server +
agent, enables the pipeline, configures Keycloak, and drives the two-turn scenario **through
real inbound auth** (no jwt bypass), printing SPARC's verdicts.

## How to read the output

- **The two turns** print as `user>` / `agent>`. Turn 1's `agent>` is SPARC's clarification
  (relayed by the agent); Turn 2's `agent>` is the successful refund.
- **The verdict summary** at the end (`SPARC <action>/<reason> tool=… score=…`) is read straight
  from the agent's AuthBridge session API:
  - `modify/reflected` — SPARC rejected an ungrounded call and the clarification was returned.
  - `allow/grounded` — SPARC approved a grounded call; the tool ran.
  - `score` is SPARC's confidence (0=worst..1=best); shown only when the model returns one.
- **Full forensic view** — the per-plugin pipeline timeline and the structured SPARC event:
  ```bash
  make show-result      # abctl TUI against the agent's session API
  make logs-sparc       # the SPARC reflection service logs
  make logs-agent       # agent + authbridge sidecar logs
  ```
- **Re-run just the conversation** (cluster already set up): `make drive`.
- **Tear down the demo** (kagenti install untouched): `make undeploy`.

## How it works (and why it's generic)

- The agent's LLM call goes through the AuthBridge forward proxy, where **`inference-parser`**
  captures the messages (incl. the system prompt) and the OpenAI tool specs, and **`mcp-parser`**
  captures the outbound `tools/call`. The **`sparc`** plugin correlates those into SPARC's three
  inputs — no per-agent wiring.
- On a reject, the plugin returns SPARC's clarification as a JSON-RPC **MCP tool result** marked
  `_meta.sparc.reflected`. The agent surfaces that to the user and stops re-trying the call
  (honoring its system instruction to relay "could not verify / clarify" results) — exactly how
  a well-behaved agent should treat a tool that asks for clarification.
- Reflection runs **out-of-band** (the plugin calls the SPARC service directly), so it is *not*
  bound by the proxy's 30s upstream timeout. `make demo` sets a generous reflection `timeout_ms`
  automatically for `PROVIDER=ollama`.

## What's in here

| Path | What |
|---|---|
| `finance-agent/` | A regular A2A agent: Ollama reasoning, MCP `tools/list` discovery, MCP tool execution. |
| `finance-mcp/` | MCP server: `get_transaction`, `lookup_customer`, `issue_refund`, `get_invoice`, `list_currencies` over a small multi-record dataset (real id: `TX4827`). |
| `k8s/sparc-patch.yaml` | Pipeline additions: `a2a-parser` inbound; `inference-parser` + `mcp-parser` + `sparc` outbound (mcp mode, fast_track, reflect, `skip_tools: [list_currencies]`). |
| `k8s/{agent,finance-mcp,sparc-service}.yaml` | Workload manifests. |
| `k8s/ollama.yaml` | In-cluster Ollama for the agent's reasoning model on the **watsonx** path; model staged onto the node — no host networking. (The `PROVIDER=ollama` path uses your host Ollama instead.) |
| `scripts/host-setup.sh` | Generic egress check (no-op on healthy networks; portable MSS clamp only if the link blackholes PMTUD). |
| `scripts/stage-ollama-model.sh` | Copies a model from the host Ollama store onto the kind node (local, no pull). |
| `scripts/setup_keycloak_finance.py` | Audience scope (realm default) + ROPC client + `alice` so inbound auth passes (scripted and UI). |
| `scripts/drive-demo.sh` | Gets a real user token and runs the two-turn scenario, printing SPARC verdicts. |
| `scripts/patch-sparc-config.sh` | Merges the pipeline patch into the operator's authbridge ConfigMap (hot-reload); substitutes the provider-tuned reflection timeout. |
| `Makefile` | `demo` (one-shot), `drive`, `show-result`, `undeploy`, `logs-*`. |

## Provider notes

- **watsonx (default)** — reflection on watsonx (`mistral-large-2512`, ~5s); the agent reasons
  with an in-cluster Ollama (`llama3.2:1b`). Fastest, highest-quality, no host Ollama needed.
  Verified: Turn 1 `modify/reflected score=0.00`, Turn 2 `allow/grounded score=1.00`.
- **ollama (`PROVIDER=ollama`)** — both reasoning and reflection run on your **host** Ollama.
  Verified: Turn 1 `modify/reflected score=0.50`, Turn 2 `allow/grounded score=1.00`.
  Two things make the host the right place for this path:
  - **Reflection latency.** ALTK reflects on ollama via free-form generate-then-validate
    (guided decoding is disabled for ollama in ALTK). That's ~140s/call on a CPU-only in-cluster
    node (over the agent's 240s deadline → fails open), but a few seconds on a Metal/GPU host.
  - **Reflection quality.** The function-selection metric needs a **capable** model. `llama3.2`
    1b/3b *hallucinate* it (they invent unrelated tools and wrongly reject grounded calls);
    a larger instruct model (e.g. `gemma4:e4b`, the verified default) judges correctly. Override
    with `SPARC_MODEL=<model your host Ollama has>`.
  - The agent's reasoning model on this path is `llama3.2:3b` (a clean tool-caller; 1b tends to
    emit malformed tool calls). Override with `AGENT_OLLAMA_MODEL=...`.
  - `make demo PROVIDER=ollama` raises the reflection `timeout_ms` (200s) and per-turn timeout
    (600s) to absorb first-call model loads.
- Small models are non-deterministic. The agent stops and relays on SPARC's clarification, so a
  run won't loop; if the model ever declines to fabricate an id on Turn 1, re-run `make drive`.

## Troubleshooting

- **Image pulls fail with DNS errors** (`server misbehaving`, `could not resolve host`): your
  container runtime's VM resolver went stale after a network change (common with Docker
  Desktop / Rancher Desktop / colima after switching Wi-Fi or VPN). Restart the runtime, or
  point its VM and the kind node at a public resolver (`8.8.8.8`). This is a host-runtime issue,
  not a demo or kagenti one — the demo scripts contain no machine-specific networking.
- **`authbridge-config-<agent>` reverted to defaults**: the operator rewrites it whenever the
  agent pod rolls. `make demo` (and `make patch-config`) re-apply the SPARC pipeline and wait
  for the live config SHA to converge, so just re-run the relevant target.
- **Agent pods stuck `ContainerCreating` on a missing `kagenti-keycloak-client-credentials-*`
  secret** (operator log: *"waiting for KEYCLOAK_URL/KEYCLOAK_REALM in authbridge-config"* or
  *"waiting for keycloak-admin-secret"*): a platform/operator-version skew — the operator
  registers the agent's Keycloak client from resources a matching chart version provides. This
  affects **every** agent, not just this demo. Provide them once per cluster:
  ```bash
  kubectl -n team1 create configmap authbridge-config \
    --from-literal=KEYCLOAK_URL=http://keycloak-service.keycloak:8080 \
    --from-literal=KEYCLOAK_REALM=kagenti --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n kagenti-system create secret generic keycloak-admin-secret \
    --from-literal=KEYCLOAK_ADMIN_USERNAME=admin \
    --from-literal=KEYCLOAK_ADMIN_PASSWORD=admin --dry-run=client -o yaml | kubectl apply -f -
  ```
  A kagenti install whose chart and operator versions are aligned creates these automatically.
- **`PROVIDER=ollama`: `reflector_unavailable` / can't reach the host Ollama**: the cluster
  must reach your host Ollama. Run it with `OLLAMA_HOST=0.0.0.0`, and set `HOST_OLLAMA_URL`
  correctly (Docker Desktop: `http://host.docker.internal:11434`; Rancher Desktop / colima:
  `http://192.168.5.2:11434`). `make demo PROVIDER=ollama` probes reachability and warns if it
  fails. A `reject` on the grounded turn usually means too small a reflection model — set
  `SPARC_MODEL` to a capable instruct model your host Ollama has pulled.

## See also
- [`docs/sparc-plugin.md`](../../docs/sparc-plugin.md) — plugin reference (config, modes).
- [`sparc-service/README.md`](../../sparc-service/README.md) — the reflection service.
- [`demos/ibac/`](../ibac/README.md) — complementary intent control (SPARC verifies grounding; IBAC verifies intent).
