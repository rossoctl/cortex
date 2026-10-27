# CPEX Bridge Demo

An LLM **agent** runs as a container exposing **A2A** (`message/send`),
with an **authbridge-cpex sidecar enforcing CPEX/APL on the agent's
outbound tool calls** to an MCP backend, on a Rossoctl kind cluster. CPEX
is a transparent **egress guardrail on the agent**: identity, Cedar PDP,
RFC 8693 delegation, redaction, a PII check, session taint, and
identity assertions all fire when the agent calls a tool — not at a gateway in front of it.

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
sensitive fields from the payload, and records the outcome, all in
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
  field on the wire, a `cel:` step denies a PII shape, and
  `taint(...)` marks the session for later calls. The decision and
  what to do about it live in the same place.
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
   post-check on the minted token. Every decision is recorded on the
   Cortex session timeline, which is the demo's audit trail.
3. **A human in the loop.** A compensation change over $10,000 waits for
   the requester's manager to approve over OIDC CIBA. The call is
   suspended, not denied, and the approval covers only that call.
4. **Policy as data.** The gateway is a generic AuthBridge sidecar.
   Every rule lives in `cpex-policy.yaml`; operators ship changes by
   updating a ConfigMap, not by releasing code.

## Prerequisites

The demo runs inside an existing Rossoctl environment. It does not
install Rossoctl or create a cluster. You need:

- A Rossoctl kind cluster named `rossoctl`. Install it from the
  [Rossoctl repository][rossoctl]. Override `KIND_CLUSTER` in the
  Makefile if yours has a different name.
- Docker, to build the four local images (`hr-cpex-agent`,
  `authbridge-cpex`, `hr-mcp`, `auth-channel`). They load into kind and
  are never pushed to a registry.
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
cleanly and leaves the rest of the cluster untouched. If you ran
`make spire`, run `make spire-undeploy` first: `make undeploy` leaves the
`spire` namespace and the `*-cpex-demo` ClusterRoles and
ClusterRoleBindings behind. The demo's SPIRE agent listens in its own host
directory, `/run/spire/cpex-demo-sockets`, so it does not replace the
socket of a SPIRE that Rossoctl installed.

[rossoctl]: https://github.com/rossoctl/rossoctl

## Quick start

```bash
make deploy         # build images (agent + authbridge-cpex + hr-mcp + auth-channel), load into kind, apply, wait for Ready
make port-forward   # in another terminal: Keycloak :8081, agent :8082, sidecar forward proxy :8083, session API :9094, hr-mcp :9100, approvals :5001 (+ SPIRE :8443 after `make spire`)
make scenarios      # run the curl scenarios
```

To use a kind cluster without switching your current kube context, pass
`KIND_CLUSTER=<name> KUBE_CONTEXT=kind-<name>` to each target. If a local
port is taken, port-forward yourself and point the scenarios at it with
`PROXY`, `KEYCLOAK_HOST`, `SESSION_API`, `UPSTREAM`, `AUTH_CHANNEL` and
`SPIRE_OIDC`.

`make scenarios` drives the tool through the agent's **forward-proxy**
(the same egress path the agent's own tool calls take), exercising the
personas across allow, deny, and redact paths. Each scenario asserts its
outcome against the reply, hr-mcp's request recorder (`/_requests`),
Keycloak's token-exchange events and the session timeline, and exits
non-zero when an expectation fails:

```text
01-bob-allow.sh                      DELEGATE  exchange, then fail closed (see below)
02-alice-deny.sh                     DENY      APL require(role.hr)
03-eve-redact.sh                     DELEGATE  SSN redacted on request and response
04-alice-internal-allow.sh           DELEGATE  Cedar permit + token exchange
05-alice-external-cedar-deny.sh      DENY      Cedar default-deny
06-bob-apl-deny.sh                   DENY      APL team mismatch
07-bob-pii-deny.sh                   DENY      CEL PII check; recorded on the timeline
08-bob-taint-deny.sh                 DENY      taint propagation (session touched secrets)
09-cross-principal-taint-isolation.sh PASS     taint is subject-bound (same id, different user)
10-adjust-under-threshold.sh         ALLOW     $5k change applies with no approval
11-adjust-needs-approval.sh          APPROVE   $15k suspends until the manager approves
12-assertions.sh                     PASS      engine identity headers reach the tool; spoof overwritten
13-workload-delegation.sh            DELEGATE  the agent as itself, by its SPIFFE ID (needs `make spire`)
```

**Delegation by default.** CPEX performs the token exchange but, unless
the sidecar asks for it, does not hand the minted token back to Cortex.
Rather than forward the caller's own bearer where the policy asked for
the delegated one, the sidecar denies with `cpex.delegation_unavailable`.
So every call whose route delegates (scenarios 01, 03, 04, and S2 of 08
and 09) asserts that Keycloak performed the exchange, that the call
failed closed and that nothing reached hr-mcp, and prints `forwarding:
delegated token withheld`. The taint is written before the delegation
check, so 08 and 09 keep their outcomes. Redaction is verified on the
engine by the cpex-tagged test in `core/plugins/cpex` (`TestHRDemo*`).
To forward the token, deploy with `make deploy CPEX_DELEGATION=available`
(or `make apply` with it), which turns on the cpex plugin's
`delegated_credentials`. The Makefile reads that setting back from the
live `authbridge-config`, so a plain `make scenarios` then asserts the
full outcomes, and a plain `make apply` keeps it;
`CPEX_DELEGATION=pending` turns it back off. The full outcomes are: 01
and 04 allowed with the minted token, 03 redacted at hr-mcp and in the
reply. The token goes only to the hosts
`delegated_credential_hosts` binds its target to (hr-mcp, for workday-api
and github-api alike); the same call to another host is denied with
`cpex.delegation_destination_mismatch`
(`TestHRDemoDelegatedCredentialBoundToHRMCP`).

**Workload identity (scenario 13).** `make spire` adds SPIRE to the
cluster (`k8s/spire/`), registers the sidecar of the agent's pod as
`spiffe://cpex.demo/ns/cpex-demo/sa/hr-cpex-agent`, and redeploys with a
`spiffe:` block in the sidecar's runtime config. The sidecar then fetches
its JWT-SVID in process and presents it to CPEX on `X-Workload-Token`
(the cpex plugin's `workload:` setting); a caller's own copy of that
header is dropped. `get_headcount` authenticates by the SVID alone, and
`delegate(..., subject: caller_workload)` runs two legs at Keycloak: the
SVID as an RFC 7523 client assertion for the `hr-copilot-agent` client
(SPIFFE identity provider, `federated-jwt` authenticator), then an RFC
8693 exchange for workday-api. The scenario checks both legs in
Keycloak's events, then the same fail-closed outcome as the other
delegating routes. Without SPIRE it reports itself skipped and exits 0;
`REQUIRE_SPIRE=1` makes that a failure. `make spire-undeploy` removes it.

**Presenting it.** `make walkthrough` runs the same scenarios in acts,
with a few lines before each on what to watch for, and ends with a
pass/fail summary; `WALKTHROUGH_ARGS=--fast` drops the pauses and
`--pace N` advances every N seconds. `make record` captures that run with
asciinema (and a GIF when `agg` is installed). The recorded order lives
in `demo/steps.md`; `make demo-check` fails when it, the walkthrough and
`scenarios/` disagree.

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
(`security.labels contains "secret": deny`). The deny fires **even when
the email body is clean** — it's the *session* that's tainted, not the
content, which is what distinguishes it from scenario 07's content-based
PII deny. Scenario 08 shows all three beats with fresh per-run session
ids: a clean session sends fine (S1), reading compensation taints
a second session (S2), and that session can no longer send email
(S3 → denied with an MCP JSON-RPC error frame, `cpex.session_tainted_secret`).

**Act 5 — cross-principal taint isolation (scenario 09).** Session taint
is keyed by the authenticated subject, not the raw `X-Session-Id`: the
CPEX session store hashes `sha256(subject_id : session_id)`. So the
*same* session id under two different users resolves to two different
buckets. Scenario 09 proves it — eve taints a shared session id (S2),
but bob reusing that exact id still sends mail (S3 → 200), because
`H(bob:id) ≠ H(eve:id)`. This is why identity must resolve on the
outbound path: the agent re-attaches `X-User-Token` so cpex derives the
subject that scopes (and isolates) taint.

**Identity assertions (scenario 12).** `global.assertions.request` in
the policy renders engine-derived identity into headers the tool reads
(`x-auth-user-id`, `x-auth-username`, `x-auth-roles`) and strips the
user's raw `X-User-Token`. Each entry removes its header before setting
it, so a caller sending `x-auth-user-id: root` still reaches hr-mcp as
its real subject. The headers are unsigned; hr-mcp trusts them because
nothing between the sidecar and it can set them. The scenario uses
`send_email`, whose route does not delegate.

**Manager approval (scenarios 10, 11).** `adjust_compensation` needs
`require(role.hr)` and a numeric `amount` (a string is refused before the
threshold reads it), and for a change of more than $10,000 either way a
`require_approval(...)` step asks
the requester's manager (the `manager` claim, Alice for Bob) through
Keycloak's CIBA backchannel. Keycloak delivers the request to the
`auth-channel` service, the manager's "phone": open
`http://localhost:5001` to approve or deny by hand. Until the manager
answers, the sidecar suspends the call with a JSON-RPC `-32120` error
whose `error.data.details.elicitation_id` the agent echoes in
`X-Policy-Elicitation-Id` when it retries. Scenario 11 answers through
the channel's API and checks that a $15k change applies after approval,
that the spent approval does not authorize a $20k change, that a denial
and an approval outside the route's `scope` (at most $25,000 either way)
are both refused, and that hr-mcp never sees a refused or pending call.
The approval is bound to the requester, route and tool, not to the
arguments: within the scope, a retry may carry a different amount or
employee than the one the manager was shown. CPEX 0.3 has no way to
bind it tighter.

**Unrouted tools.** A catch-all `tool: "*"` route denies any tool without
a route of its own (`cpex.tool_unrouted`), including a differently cased
name. Without it, such a tool would pass with no policy at all.
The route does not delegate, so it runs fully by default.

Scenario 03 is the headline. Bob and Eve send the byte-for-byte same
request, the same backend returns the same record, but Eve's response
comes back without the SSN because the policy redacts it (by default,
checked on the engine; see above).

## The personas

| Persona | Identity | Result |
|---|---|---|
| Bob | HR, `view_ssn` permission, manager Alice | Full compensation record, SSN included; changes over $10k need Alice's approval |
| Eve | HR, no `view_ssn` | Same record, SSN redacted |
| Alice | Engineering, Bob's manager | Denied HR tools; allowed internal repos, denied external; approves Bob's changes |

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
send an email to teammate@example.com saying the sync moved to Thursday
  -> sent; hr-mcp sees Bob's asserted identity, not his token

look up the compensation for EMP-001234, include the SSN
  -> denied (cpex.delegation_unavailable) by default; with the token
     returned, 200 OK with the SSN

switch alice
look up the compensation for EMP-001234
  -> denied. Alice is not HR.

search the internal repos for web-app
  -> Cedar permits (Alice is engineering, the repo is internal); the
     call then delegates, so by default it fails closed.

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
|        3. checks     CEL PII deny, session taint                 |
|        4. body       redact(args.ssn) / redact(result.ssn)       |
+------------------------------------------------------------------+
         |                                    |
         |  delegate(): RFC 8693              |  on allow: forward proxy
         |  token exchange;                   |  to the MCP target
         |  require_approval(): CIBA          |
         v                                    v
   Keycloak  svc:8080                    hr-mcp  svc:9100
   realm cpex-demo                       get_compensation
   clients: hr-copilot, cpex-gateway,    send_email
            workday-api, github-api,     search_repos, get_directory
            hr-copilot-agent             adjust_compensation
         |                               get_headcount
         |  CIBA: deliver each approval request
         v
   auth-channel  svc:5001
   the manager approves or denies here
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
authentication: [jwt-user, jwt-client]
authorization.pre_invocation:
  require(role.hr)
  delegate(workday-oauth, target: workday-api, permissions: [read_compensation])
  taint(secret, session)
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
| `k8s/30-agent.yaml` | Agent + authbridge-cpex sidecar: Service, Deployment |
| `k8s/authbridge.yaml` | The sidecar's runtime config (the `authbridge-config` ConfigMap) |
| `k8s/spire/` | Optional SPIRE server, agent and discovery provider, and the sidecar's SPIFFE wiring (`make spire`) |
| `k8s/40-auth-channel.yaml` | CIBA approval channel Service and Deployment |
| `k8s/realm-export.json` | Keycloak realm: users, clients, mappers |
| `k8s/cpex-policy.yaml` | CPEX policy: identity, PDPs, delegators, assertions, routes |
| `hr-mcp-server/` | Vendored FastAPI MCP backend, built into `hr-mcp:dev` |
| `auth-channel/` | CIBA approval page and API, built into `auth-channel:dev` (unauthenticated; demo only) |
| `agent/agent.py` | A2A server + litellm tool loop, built into `hr-cpex-agent:dev` |
| `agent/chat.py` | Host-side A2A client (mints tokens, drives the demo) |
| `agent/requirements.txt` | Agent (server) deps — installed into the image |
| `agent/requirements-client.txt` | Client deps — installed on the host for `chat.py` |
| `agent/Dockerfile` | Builds `hr-cpex-agent:dev` |
| `scenarios/` | The curl scenarios run by `make scenarios` |
| `walkthrough.sh` | Narrated run of every scenario (`make walkthrough`) |
| `demo/` | Recorded step list, the recorder and its drift check (`make record`, `make demo-check`) |
| `mint-token.sh` | Mints a persona JWT via Keycloak. Used by scenarios |
| `verify-token-exchange.sh` | Checks RFC 8693 token exchange against the realm |

## Tear down

```bash
make spire-undeploy   # only after `make spire`: the spire namespace and its ClusterRoles
make undeploy         # deletes the cpex-demo namespace
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
- **`cpex.auth_jwks_unavailable` while Keycloak is up.** Keycloak is a
  cluster-private address, which CPEX refuses by default; the cpex
  plugin in `k8s/authbridge.yaml` sets `private_destinations: true` for it.
- **`cpex.delegation_unavailable` on get_compensation or search_repos.**
  Expected by default; see **Delegation by default** above.

## Not covered yet

- **Rossoctl operator injection.** Pods deploy via plain `kubectl apply`,
  not the operator's sidecar-injection path. Promoting cpex into the
  operator's sidecar list is a separate change.
- **agentop TUI.** Would render each Invocation chain visually, including
  the deny short-circuit. Needs the per-sub-plugin Invocation work to
  land first.
- **Production TLS.** Keycloak runs on plaintext HTTP. Wire a cert via
  cert-manager and disable the `insecure_http` flags for real use.
- **Internal endpoint hardening.** The sidecar's session API (`:9094`) and
  stats endpoint (`:9093`) have no authentication. This demo binds both to
  `127.0.0.1` in `authbridge-config` (see `session_api_addr` /
  `stats_address` in `k8s/authbridge.yaml`), so they are reachable only from
  inside the pod and via `kubectl port-forward`, not cluster-wide. Note the
  AuthBridge default for these addresses is all-interfaces (`:9094` /
  `:9093`); a deployment that does not override them to loopback would expose
  them on the pod IP. Since the session API can surface session labels and
  request content, production should keep them loopback-bound (or add a
  NetworkPolicy) and add authentication on the session API. Do not expose
  either port through a Service.
