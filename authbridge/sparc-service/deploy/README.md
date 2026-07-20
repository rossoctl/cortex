# Install the SPARC reflection service

The AuthBridge [`sparc` plugin](../../docs/sparc-plugin.md) calls this service to decide whether an
agent's proposed tool call is grounded in the conversation. **Deploy it once per cluster — before
you enable the `sparc` plugin on any agent.** Without it, the plugin has nothing to call.

It's one command:

```bash
cd authbridge/sparc-service/deploy

# watsonx (default) — provide credentials in the environment:
export WX_API_KEY=... WX_PROJECT_ID=...
make install

# or local Ollama (no credentials):
make install PROVIDER=ollama OLLAMA_BASE_URL=http://host.docker.internal:11434

# or OpenAI / Azure / anything LiteLLM supports:
make install PROVIDER=openai            # needs OPENAI_API_KEY
make install PROVIDER=litellm MODEL=anthropic/claude-3-5-sonnet ANTHROPIC_API_KEY=...
```

`make install` creates the `sparc-service-config` ConfigMap and a `sparc-creds` Secret (from
whatever provider credentials are in your environment), deploys the service into `NAMESPACE`
(default `rossoctl-system`), and waits for it to become ready. Re-running it is idempotent.

When it's up, enable the plugin on an agent — see
[`docs/sparc-plugin.md`](../../docs/sparc-plugin.md#prerequisite-deploy-the-sparc-service).

## Image

The default image is `ghcr.io/rossoctl/cortex/sparc-service:latest` (published by CI).
On a **kind** dev cluster, build and load a local image first:

```bash
make image install        # builds sparc-service:latest, loads it into kind, then installs
```

Override with `IMAGE=<your registry>/sparc-service:<tag>`.

## Configuration

All knobs are `make` variables (mirrors of the service's [environment settings](../README.md)):

| Variable | Default | Notes |
|---|---|---|
| `PROVIDER` | `watsonx` | `watsonx` \| `ollama` \| `openai` \| `azure` \| `litellm` |
| `NAMESPACE` | `rossoctl-system` | where the service is deployed |
| `IMAGE` | ghcr `sparc-service:latest` | override for a local build or private registry |
| `MODEL` | per-provider | required for `azure`/`litellm` (e.g. `azure/<deployment>`) |
| `TRACK` | `fast_track` | SPARC reflection track |
| `OLLAMA_BASE_URL` | `http://host.docker.internal:11434` | used when `PROVIDER=ollama` |
| `LLM_KWARGS_JSON` | — | extra LiteLLM client kwargs for `openai`/`azure`/`litellm` |
| `LLM_REGISTRY_ID` | — | advanced: override the ALTK client registry id |
| `LLM_TIMEOUT` | `120` | per-call timeout (seconds) |

Credentials are read from the environment at install time (`WX_API_KEY`, `WX_PROJECT_ID`, `WX_URL`,
`OPENAI_API_KEY`, `AZURE_API_KEY`, `ANTHROPIC_API_KEY`, …) and stored in the `sparc-creds` Secret.

## Security / network posture

The service is an **in-cluster backend** — no ingress, never public; same trust model as the
authbridge session API. `/reflect` is unauthenticated by default, so restrict callers with a
`NetworkPolicy` (allow only the authbridge sidecars). It runs non-root and makes its own egress to
the LLM provider. See [`docs/sparc-plugin.md`](../../docs/sparc-plugin.md#security--network-posture).

## Other targets

```bash
make status      # show the deployment + active config
make uninstall   # remove the service, ConfigMap, and Secret
```
