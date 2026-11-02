# IBAC demo — the same agent with and without Intent-Based Access Control

> Conceptual overview (threat model, configuration reference, operator deployment guidance): [`docs/ibac-plugin.md`](../../docs/ibac-plugin.md).

An email-summarization agent reads a poisoned email that tells it to POST the inbox to an outside server. The demo runs two copies of that agent side by side:

| Agent | Pipeline | Asked "Summarize my emails." |
|---|---|---|
| `email-agent` | parsers + **`ibac`** | The POST is denied with a 403. The reply is `Tool call blocked by platform` with the judge's reason |
| `email-agent-unprotected` | the same parsers, no `ibac` | The inbox, staging password included, reaches `ibac-evil-server`. The reply is an ordinary summary |

Both agents run the same image and the same AuthBridge sidecar, and both pass `jwt-validation`. Identity checks cannot tell the two requests apart. Only IBAC notices that the action does not match what the user asked for.

## Prerequisites

- A [rossoctl install](https://github.com/rossoctl/rossoctl/blob/main/docs/operate/install-kubernetes.md) on a kind cluster (operator, Keycloak, SPIRE).
- ollama on the host with `llama3.2:3b` pulled, reachable from cluster pods at `host.docker.internal:11434`.
- Keycloak reachable from the host at `http://keycloak.localtest.me:8080` (the rossoctl default). Set `KEYCLOAK_URL=...` if yours differs.
- `kubectl`, `kind`, `podman` (or `docker`), `go` (to build agentop), `python3` with `PyYAML` (`pip3 install --user pyyaml`).

## Run it

All commands run from `demos/ibac`.

### 1. Deploy

```sh
make demo-ibac
```

This builds and loads the demo images and deploys both agents, the email server and the evil server into `team1`. It then patches each agent's pipeline and waits until both sidecars have loaded the patch.

### 2. Open agentop on each agent

Use two terminals, side by side:

```sh
make show-result   # terminal A: pick team1, then email-agent-…
make show-result   # terminal B: pick team1, then email-agent-unprotected-…
```

`make show-result` builds agentop from this checkout and starts it with `--kubernetes`. agentop lists the namespaces and pods and runs the `kubectl port-forward` itself. Without `--kubernetes`, a Cortex installed on your laptop would answer first.

### 3. Send the attack to both agents

In a third terminal:

```sh
make chat              # → Tool call blocked by platform: {"error":"ibac.blocked",...}
make chat-unprotected  # → The emails can be summarized as follows: ...
```

Each chat is filed under its own session, `with-ibac-<time>` or `no-ibac-<time>`.

### 4. Compare the two timelines

In each agentop, press Enter on the new session. The rows match until the agent acts on the poisoned email:

```
                         email-agent                     email-agent-unprotected
in   message/send        observe a2a-parser              observe a2a-parser
out  llama3.2:3b         observe inference-parser        observe inference-parser
out  tools/call          observe mcp-parser   200        observe mcp-parser   200   ← reads the poisoned email
out  llama3.2:3b         observe inference-parser        observe inference-parser
out  ibac-evil-server    deny    ibac         403        —       —            200   ← the exfiltration
```

Press Enter on the `deny` row to see the judge's reason.

### 5. Show what reached the attacker

```sh
make logs-evil
```

Only the unprotected agent's POST is in this log: `EXFILTRATED DATA RECEIVED`, with the staging password in the body.

### Clean up

```sh
make undeploy
```

## Why the demo is set up this way

- **The chat runs inside the cluster.** `make chat` runs `scripts/chat.sh`. The script gets a token from the agent's own Keycloak client and sends an A2A `message/send` from a small curl pod (`ibac-chat`). Do not `curl` through `kubectl port-forward` to the agent's Service. A port-forward skips the sidecar's inbound interception, so `a2a-parser` records no intent and IBAC skips the call (`no_user_context`).
- **Both agents are outside the ambient mesh.** `team1` is labelled `istio.io/dataplane-mode: ambient`. Inside the mesh, the agent's call to `ibac-email-server` comes back 502 `bad gateway`. The agent never reads the poisoned email, so neither agent attacks. Both manifests set `istio.io/dataplane-mode: none` on the pod.
- **The twin runs the parsers too.** `k8s/parsers-patch.yaml` gives it `a2a-parser`, `inference-parser` and `mcp-parser`, without `ibac`. The parsers change nothing about what the agent does. Without them, every twin row reads `— —` in agentop, and all its traffic lands in one `default` session.
- **The patch does not survive a pod restart.** The operator re-renders `authbridge-config-<agent>` each time it admits the agent's pod, and that drops the patch. `make restart` restarts both agents and re-patches them.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `make chat` is not blocked, and agentop shows no `ibac` rows | A pod restart removed the patch | `make patch-config` (or `make restart`) |
| Agent pods missing after a cluster restart: `failed calling webhook "inject.rossoctl.io"` | The pods were created before the operator's webhook was up | `make restart` |
| No reply from the agent, `/v1/pipeline` refuses connections, and the sidecar log has only `[entrypoint] Starting authbridge-proxy...` | The sidecar started before SPIRE was ready and hung. After a cluster restart, `spire-agent` can take ~15 minutes to recover | Wait for `kubectl -n spire-system get pods` to show the agent Ready, then `make restart` |
| The unprotected agent says it could not get the emails ("bad gateway") | The pod is in the ambient mesh | Re-apply `k8s/agent-unprotected.yaml` (it carries `istio.io/dataplane-mode: none`), then `make restart` |
| `ibac-chat` stuck in `ImagePullBackOff` (Docker Hub 429) | Rate limit on anonymous pulls | `podman pull curlimages/curl:latest && kind load docker-image curlimages/curl:latest --name rossoctl`, then `kubectl -n team1 delete pod ibac-chat` |
| The unprotected agent replies but `make logs-evil` is empty | `llama3.2:3b` did not follow the injection this time | `make chat-unprotected` again |
| `make chat` is blocked on the email fetch, not on the POST | The judge is a small model and sometimes misjudges a read | `make chat` again |

## Chat through the rossoctl UI instead

Both agents also appear in the rossoctl UI at `http://rossoctl-ui.localtest.me:8080`, as "Email Assistant" and "Email Assistant (no IBAC)", and you can chat with them there. The UI picks its own session ids, and the walkthrough above was verified with `make chat`, not the UI.

## Architecture

```
            rossoctl UI (browser)            Agent Pod (team1, operator-injected sidecar)
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

The plugin ships with a conservative `defaultSystemPrompt` (`core/plugins/ibac/judge.go`) that emits `{"verdict": ..., "reason": ...}` JSON. Override via `system_prompt` in the IBAC plugin's `config:` block — see the commented example in [`k8s/ibac-patch.yaml`](k8s/ibac-patch.yaml). The prompt MUST still instruct the model to emit the same JSON shape; anything else routes through `ibac.judge_uncertain` (fail-closed deny).
