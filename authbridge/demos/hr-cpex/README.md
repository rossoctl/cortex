# CPEX Bridge Demo

An LLM **agent** runs as a container exposing **A2A** (`message/send`),
with an **authbridge-cpex sidecar enforcing CPEX/APL on the agent's
outbound tool calls** to an MCP backend, on a Kagenti kind cluster. CPEX
is a transparent **egress guardrail on the agent**: identity, Cedar PDP,
RFC 8693 delegation, redaction, PII scanning, session taint, and audit
all fire when the agent calls a tool — not at a gateway in front of it.

> **Architecture note.** Earlier revisions of this demo ran CPEX as a
> standalone gateway *in front of* `hr-mcp` (enforcing on the inbound
> path), with the "agent" being a host-side `chat.py` script. It has been
> reworked to the agent-sidecar shape above (matching the `ibac` demo):
> the agent is now containerized, `chat.py` is a thin A2A client, and CPEX
> enforces on the agent's **outbound** path. CPEX is direction-agnostic,
> so the policy in `cpex-policy.yaml` is unchanged — only *where* it runs
> moved (inbound → outbound), expressed in the sidecar's pipeline.

## Why CPEX

A decision engine like OPA or Cedar answers one question: given this
input, is the action allowed? That verdict still has to be wired into
something. Real authorization resolves identity first, often consults
more than one engine, mints a token for the downstream call, strips
sensitive fields from the payload, and writes an audit record, all in
the right order with the right short-circuits. That orchestration is
normally bespoke code in every gateway.

CPEX APL is the connective layer that makes the orchestration itself
declarative. A policy is a per-resource chain of steps, and those steps
compose three things a plain PDP does not:

- **Embedded PDPs.** A decision engine is one kind of step, not the
  whole policy. This demo consults Cedar through a `cedar:` step; the
  same slot is designed to take OPA, AuthZen, or another engine. APL
  owns the orchestration around the engine's discrete verdict, running
  cheap predicates first and the PDP only for requests that clear them.
- **Explicit effects.** Authorization is more than allow or deny. APL
  expresses effects as first-class steps: `redact(...)` rewrites a
  field on the wire, `plugin(pii-scan)` runs a content guardrail,
  `plugin(audit-log)` records the outcome. The decision and what to do
  about it live in the same place.
- **Actions as decisions.** `delegate(...)` mints a downstream-scoped
  token (RFC 8693 token exchange) as a policy step, and a post-check
  can gate on its result, refusing to forward a token the IdP narrowed
  below what the call needs. Issuing credentials becomes part of the
  authorization decision rather than a side effect bolted on afterward.

## What this demo shows

1. **Same request, different data.** Bob (HR, with `view_ssn`) sees an
   employee SSN. Eve (HR, without `view_ssn`) makes the identical call
   and gets the record back with the SSN redacted. The redaction is an
   APL effect, not backend logic.
2. **A full pattern in one policy.** A single route chains a coarse APL
   predicate, a Cedar PDP decision, an RFC 8693 token exchange, a
   post-check on the minted token, a PII guardrail, and an audit record.
3. **Policy as data.** The gateway is a generic AuthBridge sidecar.
   Every rule lives in `cpex-policy.yaml`; operators ship changes by
   updating a ConfigMap, not by releasing code.

## Prerequisites

The demo runs inside an existing Kagenti environment. It does not
install Kagenti or create a cluster. You need:

- A Kagenti kind cluster named `kagenti`. Install it from the
  [Kagenti repository][kagenti]. Override `KIND_CLUSTER` in the
  Makefile if yours has a different name.
- Docker, to build the three local images (`hr-cpex-agent`,
  `authbridge-cpex`, `hr-mcp`). They load into kind and are never pushed
  to a registry.
- kubectl, configured for the cluster.
- An Ollama running on the host with a tool-capable model pulled
  (`ollama pull llama3`) — only needed for the interactive chat;
  the `make scenarios` curl matrix does not use the LLM. Ollama must
  listen beyond loopback so cluster pods can reach it: set
  `OLLAMA_HOST=0.0.0.0` and restart Ollama. How a pod reaches the host
  depends on your Docker provider — see **Pointing the agent at Ollama**
  below.

Everything else is vendored in this directory: the `hr-mcp` backend,
the AuthBridge manifests, the agent, and the chat client. No sibling
checkouts needed.

The demo creates one namespace, `cpex-demo`. `make undeploy` removes it
cleanly and leaves the rest of the cluster untouched.

[kagenti]: https://github.com/kagenti/kagenti

## Quick start

```bash
make deploy         # build images (agent + authbridge-cpex + hr-mcp), load into kind, apply, wait for Ready
make port-forward   # in another terminal: Keycloak :8081, agent :8082, sidecar forward proxy :8083, session API :9094
make scenarios      # run the nine curl scenarios
```

`make scenarios` drives the tool through the agent's **forward-proxy**
(the same egress path the agent's own tool calls take), exercising the
personas across allow, deny, and redact paths:

```text
01-bob-allow.sh                      PASS   SSN visible
02-alice-deny.sh                     DENY   APL require(role.hr)
03-eve-redact.sh                     PASS   SSN redacted in response   <- the one to watch
04-alice-internal-allow.sh           PASS   Cedar permit + token exchange
05-alice-external-cedar-deny.sh      DENY   Cedar default-deny
06-bob-apl-deny.sh                   DENY   APL team mismatch
07-bob-pii-deny.sh                   DENY   PII validator (PII in the body)
08-bob-taint-deny.sh                 DENY   taint propagation (session touched secrets)
09-cross-principal-taint-isolation.sh PASS  taint is subject-bound (same id, different user)
```

The scenarios are deterministic curl runs and skip the LLM entirely:
each posts an MCP `tools/call` through the sidecar forward proxy
(`curl -x http://localhost:8083 http://hr-mcp.cpex-demo:9100/mcp`),
carrying the same `Authorization` + `X-User-Token` (+ `X-Session-Id`)
the agent would. The interactive chat (below) is the full A2A path.

**Act 4 — taint propagation (scenario 08).** Reading compensation runs
`taint(secret, session)`, attaching the label `secret` to the CPEX
session (persisted in the session store, keyed by the `X-Session-Id`
the caller threads in). The `send_email` policy then refuses external
email from any session carrying that label
(`session.labels contains "secret": deny`). The deny fires **even when
the email body is clean** — it's the *session* that's tainted, not the
content, which is what distinguishes it from scenario 07's content-based
PII deny. Scenario 08 shows all three beats with fresh per-run session
ids: a clean session sends fine (S1 → 200), reading compensation taints
a second session (S2 → 200), and that session can no longer send email
(S3 → 403 `cpex.session_tainted_secret`).

**Act 5 — cross-principal taint isolation (scenario 09).** Session taint
is keyed by the authenticated subject, not the raw `X-Session-Id`: the
CPEX session store hashes `sha256(subject_id : session_id)`. So the
*same* session id under two different users resolves to two different
buckets. Scenario 09 proves it — eve taints a shared session id (S2),
but bob reusing that exact id still sends mail (S3 → 200), because
`H(bob:id) ≠ H(eve:id)`. This is why identity must resolve on the
outbound path: the agent re-attaches `X-User-Token` so cpex derives the
subject that scopes (and isolates) taint.

Scenario 03 is the headline. Bob and Eve send the byte-for-byte same
request, the same backend returns the same record, but Eve's response
comes back without the SSN because the policy redacts it.

## The personas

| Persona | Identity | Result |
|---|---|---|
| Bob | HR, `view_ssn` permission | Full compensation record, SSN included |
| Eve | HR, no `view_ssn` | Same record, SSN redacted |
| Alice | Engineering | Denied HR tools; allowed internal repos, denied external |

## Interactive chat

The scenarios are deterministic curl runs. For a live demo, talk to the
**agent over A2A** with `chat.py` — a thin client that mints persona
tokens and switches persona mid-conversation. The LLM and the tools live
in the agent container; `chat.py` only sends `message/send` and renders
the reply.

The agent itself runs an LLM (default `ollama/llama3:latest`). Make sure an
Ollama is running on the host with the model pulled and listening beyond
loopback so the cluster can reach it:

```bash
ollama pull llama3
OLLAMA_HOST=0.0.0.0 ollama serve     # or, for the macOS app:
                                     #   launchctl setenv OLLAMA_HOST 0.0.0.0 && relaunch Ollama
```

See **Pointing the agent at Ollama** below for the per-provider address;
`make ollama-check` verifies a pod can actually reach it.

Set up the **client** once (its deps are separate from the agent image —
no litellm needed host-side):

```bash
cd agent
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements-client.txt
```

Run it (the A2A endpoint is the agent Service, exposed on `:8082` by
`make port-forward`):

```bash
python chat.py --persona eve
# or point at a different endpoint:
AGENT_URL=http://localhost:8082 python chat.py --persona bob
```

To run the agent against a non-Ollama model, set `MODEL` (and the
provider's API-key env) on the `agent` container — model selection now
lives in the container, not the client. `MODEL` and `LLM_API_BASE` are
Makefile vars, e.g. `make deploy MODEL=ollama/llama3:70b`.

### Pointing the agent at Ollama

The agent's inference call goes **direct** to `LLM_API_BASE` (never
through the cpex sidecar — only the MCP tool call is proxied). How a
cluster pod reaches the host's Ollama depends on your Docker provider, so
`LLM_API_BASE` is a Makefile var (injected into the deployment on
`apply`):

| Provider | `LLM_API_BASE` |
|---|---|
| Docker Desktop / kind created with host mappings | `http://host.docker.internal:11434` (default) |
| Rancher Desktop (lima) | `http://192.168.5.2:11434` (the lima host-gateway) |

```bash
# Rancher Desktop example:
make deploy LLM_API_BASE=http://192.168.5.2:11434
make ollama-check LLM_API_BASE=http://192.168.5.2:11434   # confirms a pod can reach it
```

In all cases Ollama must be started with `OLLAMA_HOST=0.0.0.0`. If
`make ollama-check` fails, it prints exactly these two things to fix.

In-chat commands:

```text
switch <name>   swap persona and re-mint both tokens (alice, bob, eve)
relogin         refresh both tokens if they expire mid-demo
quit            exit
```

Suggested conversation:

```text
look up the compensation for EMP-001234, include the SSN
  -> 200 OK, SSN included

switch eve
look up the compensation for EMP-001234, include the SSN
  -> 200 OK, response has no SSN field. Same backend, same request,
     policy redacted it.

switch alice
look up the compensation for EMP-001234
  -> denied (cpex.denied). Alice is not HR.

search the internal repos for web-app
  -> 200 OK. Cedar permits: Alice is engineering, the repo is internal.

search external repos
  -> denied (cpex.cedar_default_deny). Cedar refuses the cross-boundary call.
```

See `agent/CHAT-WALKTHROUGH.md` for a longer script and talking points.

## How it works

The A2A client sends `message/send` to the agent with two JWTs as HTTP
headers: a client token in `Authorization` and the persona's user token
in `X-User-Token`, plus an `X-Session-Id` that scopes session taint. The
sidecar's **inbound** pipeline is empty (passthrough), so those headers
reach the agent unmodified. The agent runs its LLM loop and, for each
tool call, **re-attaches the same headers** onto an MCP `tools/call` that
egresses through the sidecar's **forward proxy** — where the `mcp-parser
-> cpex` pipeline runs and `cpex` evaluates `cpex-policy.yaml` in order.

```text
chat.py (host A2A client)
    |  message/send
    |    Authorization: client token     X-User-Token: persona token
    |    X-Session-Id: conversation id   contextId in A2A body
    v  port-forward :8082 -> svc/hr-cpex-agent:8080
+------------------------------------------------------------------+
| hr-cpex-agent pod   (namespace: cpex-demo)                       |
|                                                                  |
|  authbridge-cpex sidecar — reverse proxy :8000                   |
|    inbound pipeline: []  (passthrough — headers forwarded as-is) |
|        v                                                         |
|  agent :8001  (A2A server + litellm tool loop)                   |
|    - inference: litellm -> host.docker.internal:11434  DIRECT    |
|                 (never through the sidecar — the Ollama footgun) |
|    - tool call: POST /mcp, re-attaching Authorization +          |
|                 X-User-Token + X-Session-Id                      |
|        v  explicit per-client proxy -> :8081                     |
|  authbridge-cpex sidecar — forward proxy :8081                   |
|    outbound pipeline:  mcp-parser  ->  cpex                      |
|      mcp-parser: parse JSON-RPC, set mcp.method / mcp.name       |
|      cpex:       run cpex-policy.yaml in order:                  |
|        1. identity   resolve subject + client from the two JWTs, |
|                      verified against Keycloak JWKS (subject     |
|                      keys the session-taint bucket)              |
|        2. APL policy require(...) -> cedar PDP ->               |
|                      delegate(...) -> post-check                 |
|        3. plugins    pii-scan (deny), audit-log (observe), taint |
|        4. body       redact(args.ssn) / redact(result.ssn)       |
+------------------------------------------------------------------+
         |                                    |
         |  delegate(): RFC 8693              |  on allow: forward proxy
         |  token exchange                    |  to the MCP target
         v                                    v
   Keycloak  svc:8080                    hr-mcp  svc:9100
   realm cpex-demo                       get_compensation
   clients: hr-copilot, cpex-gateway,    send_email
            workday-api, github-api      search_repos, get_directory
```

Response-side redaction (`result.ssn`) runs as the reply flows back
out through the pipeline, so an SSN the backend returns unsolicited is
still stripped for a caller without the permission.

**Why inference must bypass the sidecar.** The agent makes two kinds of
outbound call. The MCP tool call is the one cpex should govern, so it
goes through the forward proxy (`:8081`). The LLM inference call must
*not*: it goes direct to `host.docker.internal:11434`. The container
therefore sets `MCP_PROXY` (used by the MCP client only) and deliberately
**no `HTTP_PROXY`** — a global proxy would drag inference through cpex,
which would try to evaluate model traffic as a tool call.

### Authorization patterns

All authorization lives in the `routes:` block at the bottom of
`cpex-policy.yaml`, one route per tool, each spelling out its own chain.
That block is the story; read it first. Everything above it is the
plugin toolbox the routes pull from. Two routes show the range, from a
flat permission gate to a four-layer chain.

**Workday flow** (`get_compensation`). The IdP carries every
permission directly on the user token. APL gates on a flat predicate
and redacts the SSN on the wire when the permission is missing:

```text
require(role.hr)
delegate(workday-oauth, target: workday-api, permissions: [read_compensation])
plugin(audit-log)
args.ssn   ->  str | redact(!perm.view_ssn)
result.ssn ->  str | redact(!perm.view_ssn)
```

Bob has `view_ssn`, so the SSN passes. Eve does not, so the same field
comes back `[REDACTED]` on both the request and the response.

**GitHub flow** (`search_repos`). Repo access depends on the
relationship between caller and resource, so four layers compose:

1. **APL coarse gate**: `require(team.engineering | team.security)`
2. **Cedar PDP**: `cedar: read on Repo{ visibility: ${args.visibility} }`
3. **RFC 8693 exchange**: `delegate(github-oauth, target: github-api, permissions: [repo:read:internal])`
4. **Post-check**: `!(delegation.granted.permissions contains 'repo:read:internal'): deny`

APL handles the cheap predicate, Cedar decides principal-by-resource,
delegation mints an audience-scoped token, and the post-check refuses
to forward a token the IdP narrowed below what the call needs.

## Editing policy

`k8s/cpex-policy.yaml` is the source of truth for all authorization
behavior. `make apply` recreates the ConfigMap from it on every run.
After editing:

```bash
make apply
kubectl -n cpex-demo rollout restart deployment/hr-cpex-agent
```

`make apply` regenerates the ConfigMap but does not restart the pod,
since the Deployment spec is unchanged. The rollout restart forces a
fresh pod that mounts the new policy.

## Files

| Path | Purpose |
|---|---|
| `k8s/00-namespace.yaml` | The `cpex-demo` namespace |
| `k8s/10-keycloak.yaml` | Keycloak Service and Deployment |
| `k8s/20-hr-mcp.yaml` | hr-mcp backend Service and Deployment |
| `k8s/30-agent.yaml` | Agent + authbridge-cpex sidecar: config, Service, Deployment |
| `k8s/realm-export.json` | Keycloak realm: users, clients, mappers |
| `k8s/cpex-policy.yaml` | CPEX policy: identity, PDP, delegator, validators, audit |
| `hr-mcp-server/` | Vendored FastAPI MCP backend, built into `hr-mcp:dev` |
| `agent/agent.py` | A2A server + litellm tool loop, built into `hr-cpex-agent:dev` |
| `agent/chat.py` | Host-side A2A client (mints tokens, drives the demo) |
| `agent/requirements.txt` | Agent (server) deps — installed into the image |
| `agent/requirements-client.txt` | Client deps — installed on the host for `chat.py` |
| `agent/Dockerfile` | Builds `hr-cpex-agent:dev` |
| `scenarios/` | The nine curl scenarios run by `make scenarios` |
| `mint-token.sh` | Mints a persona JWT via Keycloak. Used by scenarios |
| `verify-token-exchange.sh` | Checks RFC 8693 token exchange against the realm |

## Tear down

```bash
make undeploy   # deletes the cpex-demo namespace
```

## Troubleshooting

- **Rebuilt an image but the pod still runs the old one.** A rebuild
  leaves the Deployment spec identical, so `kubectl apply` does not roll
  the pod. After `kind load docker-image hr-cpex-agent:dev` (or
  `authbridge-cpex:dev`), force it with
  `kubectl -n cpex-demo rollout restart deployment/hr-cpex-agent`.
- **Chat client cannot reach the agent.** `chat.py` defaults to
  `http://localhost:8082`; `make port-forward` exposes the agent Service
  there. Override with `AGENT_URL=http://localhost:8082`.
- **Agent replies with an LLM/connection error** (`name resolution` or
  `all connection attempts failed`). The agent can't reach the host
  Ollama. Run `make ollama-check` — it tells you the two fixes:
  (1) start Ollama with `OLLAMA_HOST=0.0.0.0`, and (2) set `LLM_API_BASE`
  to an address your pods can route to (`host.docker.internal:11434` on
  Docker Desktop, `192.168.5.2:11434` on Rancher Desktop). See **Pointing
  the agent at Ollama**. Inference goes direct (not through the sidecar)
  by design.
- **Client gets HTTP 503 "all connection attempts failed".** A stale
  `kubectl port-forward` — the tunnel dies when the agent pod rolls (e.g.
  after `make deploy`/`set env`). Restart `make port-forward`.
- **Tool calls aren't being enforced / cpex never fires.** cpex now runs
  on the agent's **outbound** path. Confirm the MCP call traverses the
  forward proxy: `kubectl -n cpex-demo logs deploy/hr-cpex-agent -c
  authbridge-cpex`. The agent must re-attach `X-User-Token` +
  `Authorization`; without the user token cpex can't resolve a subject
  and session taint silently stops scoping.
- **Keycloak token issuer.** `KC_HOSTNAME` sets the `iss` claim to
  `http://keycloak.cpex-demo:8080`. Tokens minted through the
  port-forward at `localhost:8081` still carry that in-cluster issuer,
  and the in-cluster gateway validates it as expected.
  `KC_HOSTNAME_STRICT=false` lets clients reach Keycloak at any
  hostname.
- **Plaintext HTTP to Keycloak.** Every plugin that talks to Keycloak
  (jwt-user, jwt-client, workday-oauth, github-oauth) sets
  `insecure_http: true`, because the JWKS and token-endpoint validators
  reject plaintext URLs by default. Put TLS in front of Keycloak and
  remove these flags for any real deployment.

## Not covered yet

- **Kagenti operator injection.** Pods deploy via plain `kubectl apply`,
  not the operator's sidecar-injection path. Promoting cpex into the
  operator's sidecar list is a separate change.
- **abctl TUI.** Would render each Invocation chain visually, including
  the deny short-circuit. Needs the per-sub-plugin Invocation work to
  land first.
- **Production TLS.** Keycloak runs on plaintext HTTP. Wire a cert via
  cert-manager and disable the `insecure_http` flags for real use.
