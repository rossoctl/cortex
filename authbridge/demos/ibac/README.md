# IBAC demo — Intent-Based Access Control via the kagenti UI

> Conceptual overview (threat model, configuration reference, operator deployment guidance): [`authbridge/docs/ibac-plugin.md`](../../docs/ibac-plugin.md).

An email-summarization agent receives a prompt-injection inside one of its emails and is tricked into POSTing data to an external server. With IBAC in the agent's outbound authbridge pipeline, the LLM judge denies the misaligned action and the exfiltration is blocked.

## Prerequisites

- A [kagenti install](https://github.com/kagenti/kagenti/blob/main/docs/install.md) on a kind cluster (operator + Keycloak + UI).
- ollama on the host with `llama3.2:3b` pulled, reachable from cluster pods at `host.docker.internal:11434`.
- `kubectl`, `kind`, `podman` (or `docker`), `python3` with `PyYAML` (`pip3 install --user pyyaml`).

## Quick start

```sh
cd authbridge/demos/ibac

make demo-ibac          # build + load demo images, deploy, patch CM, ready-to-chat banner

# Open http://kagenti-ui.localtest.me:8080, find `email-agent`, chat:
#   "Summarize my emails."

make show-result        # forensic of the most recent session
make undeploy           # remove the demo's resources from team1
```

Expected chat response — the platform's verbatim 403, no email contents leaked:

```
Tool call blocked by platform:

> {"error":"ibac.blocked","message":"<judge's reason>","plugin":"ibac"}
```

`make show-result` exits `0` (`ATTACK BLOCKED`), `1` (`IBAC FAILED` — real bug), or `2` (`ATTACK MISFIRED` — small-LLM non-determinism, re-chat).

## Architecture

```
            kagenti UI (browser)            Agent Pod (team1, operator-injected sidecar)
       ┌────────────────────────┐         ┌──────────────────────────────────────────────┐
       │ user types             │         │                                              │
       │ "Summarize my emails." │ A2A POST│  authbridge-proxy :8000 ──▶ agent :8001      │
       │                        │ ──────▶ │  (reverse proxy + a2a-parser inbound +       │
       │                        │ Bearer  │   jwt-validation; populates Session.Intents) │
       │                        │ token   │                                              │
       │ chat response with     │ ◀────── │                       │ outbound HTTP via    │
       │ ⚠️ Security event…    │         │                       │ HTTP_PROXY=:8081     │
       └────────────────────────┘         │                       ▼                      │
                                          │  authbridge-proxy :8081 (forward proxy +     │
                                          │   token-exchange + mcp-parser + ibac)        │
                                          │                       │                      │
                                          └───────────────────────┼──────────────────────┘
                                                                  │
                                                                  ▼
                                       ┌───────────────────────────────────────────────┐
                                       │  ibac-email-server:8888 (poisoned content)    │
                                       │  ibac-evil-server :9999 (exfil target)        │
                                       │  host.docker.internal:11434 (ollama judge)    │
                                       └───────────────────────────────────────────────┘
```

`make demo-ibac` patches `authbridge-config-email-agent` to add `a2a-parser` inbound (populates `Session.Intents` from the user's chat message) and `mcp-parser` + `ibac` outbound (judges every outbound call against the recorded intent). The agent itself is unmodified — IBAC is a transparent gate, the agent only sees a 403.

## Customizing the judge's system prompt

The plugin ships with a conservative `defaultSystemPrompt` (`authbridge/authlib/plugins/ibac/judge.go`) that emits `{"verdict": ..., "reason": ...}` JSON. Override via `system_prompt` in the IBAC plugin's `config:` block — see the commented example in [`k8s/ibac-patch.yaml`](k8s/ibac-patch.yaml). The prompt MUST still instruct the model to emit the same JSON shape; anything else routes through `ibac.judge_uncertain` (fail-closed deny).
