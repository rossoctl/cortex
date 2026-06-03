# Kagenti SPARC reflection service

A thin, in-process HTTP wrapper around the **SPARC** pre-tool reflection component
(`altk.pre_tool.sparc.SPARCReflectionComponent`) from the
[`agent-lifecycle-toolkit`](https://pypi.org/project/agent-lifecycle-toolkit/) (ALTK)
PyPI package.

AuthBridge's Go [`sparc` plugin](../authlib/plugins/sparc) calls this service to decide
whether a proposed tool call is **grounded** in the conversation and the available tool
specs. SPARC catches hallucinated / ungrounded arguments (e.g. an invented transaction
ID) and inappropriate function selection.

This service is deliberately *just* a reflection wrapper:

- It consumes ALTK **directly from PyPI** — no vendored copy of the SPARC source.
- It calls SPARC **in process** — no relay queue, observer, or host worker (those exist
  only in the original `ibac` reference repo as a `kind`-egress workaround).
- It returns SPARC's verdict **faithfully**. All enforcement policy (observe / inject /
  deny, score thresholds) lives in the AuthBridge plugin, not here.

## API

### `POST /reflect`

Request:

```jsonc
{
  "messages":   [ { "role": "user", "content": "Refund transaction TX482." } ],
  "tool_specs": [ { "type": "function", "function": { "name": "get_transaction", ... } } ],
  "tool_calls": [ { "id": "c1", "type": "function",
                    "function": { "name": "get_transaction",
                                  "arguments": "{\"transaction_id\": \"TX4821\"}" } } ],
  "session_id": "optional-correlation-id",
  "track":      "fast_track"   // optional per-request override
}
```

Response:

```jsonc
{
  "decision": "approve | reject | error",
  "issues": [ { "issue_type": "...", "metric_name": "...",
                "explanation": "...", "correction": { ... } } ],
  "overall_avg_score": 0.4,        // SPARC grounding score, normalized 0..1 (higher = better), when available
  "execution_time_ms": 4600.0,
  "raw_pipeline_result": { ... }   // present only if SPARC_INCLUDE_RAW_RESPONSE=true
}
```

### `GET /healthz` — liveness. `GET /readyz` — readiness (config valid + component buildable).

On failure `/reflect` returns a stable message (`{"error": "reflection failed"}`, or `400` for a
bad request such as an unsupported `track`); the underlying provider exception is logged, never
returned to the caller — provider errors can embed endpoints or credentials.

## Security / network posture

Deploy this as an **in-cluster backend only** — no ingress, never public. `/reflect` is
unauthenticated by default, so the network boundary is the control: restrict callers with a
`NetworkPolicy` (allow only the authbridge sidecars). Any pod that can POST to `/reflect` can
trigger LLM calls billed to the configured credentials. For an additional hop, set the plugin's
`reflector_bearer` and verify it here (or at a fronting sidecar). The service runs as a non-root
user and makes its own egress to the LLM provider; it stays out of the agent-side ambient mesh.

## Configuration (environment)

**watsonx is the default.** Switching providers is a config change — no rebuild. `watsonx`
and `ollama` use ALTK's provider-native clients; `openai`, `azure` (Azure OpenAI), and the
generic `litellm` provider use ALTK's generic LiteLLM client, so **any provider LiteLLM/ALTK
supports** works by setting the model string and (optionally) extra client kwargs.

| Variable | Default | Notes |
| --- | --- | --- |
| `SPARC_LLM_PROVIDER` | `watsonx` | `watsonx` \| `ollama` \| `openai` \| `azure` \| `litellm` (generic) |
| `SPARC_MODEL` | per-provider | watsonx: `mistral-large-2512`, ollama: `llama3.2:3b`, openai: `gpt-4o-mini`. **Required** for `azure`/`litellm` (e.g. `azure/<deployment>`, `anthropic/claude-3-5-sonnet`, `gemini/gemini-1.5-pro`, `bedrock/...`). |
| `SPARC_LLM_KWARGS_JSON` | — | JSON object of extra LiteLLM client kwargs for `openai`/`azure`/`litellm`, e.g. `{"api_base":"...","api_version":"...","api_key":"..."}`. |
| `SPARC_LLM_REGISTRY_ID` | — | Advanced: override the ALTK client registry id directly (any ALTK client). |
| `SPARC_TRACK` | `fast_track` | `fast_track` \| `slow_track` \| `syntax` \| `spec_free` \| `transformations_only` |
| `SPARC_LLM_TIMEOUT` | `120` | seconds |
| `SPARC_RETRIES` | `3` | SPARC retries |
| `SPARC_MAX_PARALLEL` | `2` | SPARC metric parallelism |
| `SPARC_INCLUDE_RAW_RESPONSE` | `true` | include `raw_pipeline_result` in responses |
| `WX_API_KEY` / `WX_PROJECT_ID` / `WX_URL` | — | watsonx creds (also accepts `WATSONX_*`) |
| `OLLAMA_BASE_URL` | `http://localhost:11434` | ollama endpoint |
| `OPENAI_API_KEY` / `OPENAI_BASE_URL` | — | openai creds (LiteLLM also reads provider keys from env: `OPENAI_API_KEY`, `AZURE_API_KEY`, `ANTHROPIC_API_KEY`, …) |
| `HOST` / `PORT` | `0.0.0.0` / `8090` | bind address |

### Provider examples

```bash
# watsonx (default)
SPARC_LLM_PROVIDER=watsonx WX_API_KEY=... WX_PROJECT_ID=...

# OpenAI
SPARC_LLM_PROVIDER=openai SPARC_MODEL=gpt-4o-mini OPENAI_API_KEY=sk-...

# Azure OpenAI
SPARC_LLM_PROVIDER=azure SPARC_MODEL=azure/<deployment> \
  SPARC_LLM_KWARGS_JSON='{"api_base":"https://<res>.openai.azure.com","api_version":"2024-06-01","api_key":"..."}'

# Anything else LiteLLM supports (Anthropic, Gemini, Bedrock, ...)
SPARC_LLM_PROVIDER=litellm SPARC_MODEL=anthropic/claude-3-5-sonnet \
  SPARC_LLM_KWARGS_JSON='{"api_key":"sk-ant-..."}'

# Local Ollama
SPARC_LLM_PROVIDER=ollama SPARC_MODEL=llama3.2:3b OLLAMA_BASE_URL=http://host:11434
```

## Develop & test

```bash
python -m venv .venv && . .venv/bin/activate
pip install -e ".[dev]"

# Fast, offline unit tests (fake SPARC component — no network, no creds):
pytest

# Opt-in integration test against real watsonx:
RUN_WATSONX_TESTS=1 WX_API_KEY=... WX_PROJECT_ID=... pytest tests/test_watsonx_integration.py -v
```

## Run

```bash
SPARC_LLM_PROVIDER=watsonx WX_API_KEY=... WX_PROJECT_ID=... python -m sparc_service
# or against local Ollama:
SPARC_LLM_PROVIDER=ollama SPARC_MODEL=llama3.2:3b python -m sparc_service
```

## Build the image

```bash
docker build -t kagenti-sparc-service:latest .
```
