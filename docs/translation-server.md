# Use a translation server

The [inference router](inference-router-plugin.md#servers-that-serve-another-protocol)
can translate four protocol pairs: Anthropic Messages (Claude Code's request) to chat
completions (the server), OpenAI Responses (Codex's request) to chat completions,
OpenAI Responses to Anthropic Messages, and chat completions (the request of OpenCode,
Pi, Bob IDE and other Chat agents) to Anthropic Messages.
For each other pair, Cortex does not send the
request. It answers with a 502 in the agent's protocol:

> Cortex cannot translate \<source\> to \<target\> yet. Use a translation server
> for this pair (see this page).

For such a pair, run a translation server on loopback, and add it to the router as
the server for the agent.

## Run LiteLLM

This LiteLLM config worked on 2026-11-16. It accepts Anthropic Messages and sends
chat completions to the server:

```yaml
model_list:
  - model_name: <model>
    litellm_params:
      model: hosted_vllm/<model>
      api_base: <server base URL, for example https://host/v1>
      api_key: os.environ/UPSTREAM_API_KEY
      additional_drop_params: ["prompt_cache_key"]
general_settings:
  master_key: os.environ/LITELLM_MASTER_KEY
```

- Use the `hosted_vllm/` provider, not `openai/`. LiteLLM's `openai/` provider
  sent the requests to `/v1/responses`, and the server answered 403.
- `additional_drop_params` removes `prompt_cache_key`, which the server does not
  accept.

Start it:

```sh
litellm --config litellm.yaml --host 127.0.0.1 --port 4000
```

Bind it to loopback only, and always set a master key. Without them, any client that can
get to port 4000 can use your server key.

## Add it to the router

```sh
agentop server add --main <word> --helper <word> \
  --api anthropic-messages=/v1/messages <name> http://127.0.0.1:4000
```

Give the master key as the server key: at the prompt, or on stdin with
`--key-stdin`. Because `anthropic-messages` is in the server's `api` map, Cortex
sends Claude Code's requests to LiteLLM with no translation.

## What Cortex records

Cortex records only the agent's side of the hop: the request to LiteLLM and the
reply from LiteLLM. The cost and the errors of the hop from LiteLLM to the server
are LiteLLM's. Cortex does not see them.
