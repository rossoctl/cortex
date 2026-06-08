# CPEX Bridge Demo

An AuthBridge sidecar fronts an MCP backend on a Kagenti kind cluster
and enforces authorization with CPEX/APL.

## Why CPEX/APL, not just a PDP

A decision engine like OPA or Cedar answers one question: given this
input, is the action allowed? That verdict still has to be wired into
something. Real authorization resolves identity first, often consults
more than one engine, mints a token for the downstream call, strips
sensitive fields from the payload, and writes an audit record, all in
the right order with the right short-circuits. That orchestration is
normally bespoke code in every gateway.

APL is the connective layer that makes the orchestration itself
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

For AuthBridge, that is the difference between native Go plugins that
each make one fixed decision and an operator-editable policy that
composes identity, any PDP, token exchange, redaction, and audit into a
single declarative chain, shipped as a ConfigMap.

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
- Docker, to build the two local images. They load into kind and are
  never pushed to a registry.
- kubectl, configured for the cluster.

Everything else is vendored in this directory: the `hr-mcp` backend,
the AuthBridge manifests, and the chat agent. No sibling checkouts
needed.

The demo creates one namespace, `cpex-demo`. `make undeploy` removes it
cleanly and leaves the rest of the cluster untouched.

[kagenti]: https://github.com/kagenti/kagenti

## Quick start

```bash
make deploy         # build images, load into kind, apply manifests, wait for Ready
make port-forward   # in another terminal: Keycloak on :8081, gateway on :8082
make scenarios      # run the seven curl scenarios
```

`make scenarios` exercises three personas across allow, deny, and
redact paths:

```
01-bob-allow.sh                 PASS   SSN visible
02-alice-deny.sh                DENY   APL require(role.hr)
03-eve-redact.sh                PASS   SSN redacted in response   <- the one to watch
04-alice-internal-allow.sh      PASS   Cedar permit + token exchange
05-alice-external-cedar-deny.sh DENY   Cedar default-deny
06-bob-apl-deny.sh              DENY   APL team mismatch
07-bob-pii-deny.sh              DENY   PII validator (PII in the body)
08-bob-taint-deny.sh            DENY   taint propagation (session touched secrets)
```

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

The scenarios are deterministic curl runs. For a live demo, drive the
gateway with an LLM agent that switches persona mid-conversation.

Set up the agent once:

```bash
cd agent
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
```

Run it against a local Ollama, no API key required:

```bash
ollama pull llama3.1
GATEWAY_URL=http://localhost:8082/mcp python chat.py --persona eve
```

The agent defaults to `ollama/llama3.1`. Point `--model` at any
litellm-supported provider for stronger tool-calling:

```bash
GATEWAY_URL=http://localhost:8082/mcp python chat.py --persona bob --model gpt-4o-mini
```

In-chat commands:

```
switch <name>   swap persona and re-mint both tokens (alice, bob, eve)
relogin         refresh both tokens if they expire mid-demo
quit            exit
```

Suggested conversation:

```
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

The client sends one MCP request with two JWTs: a client token in
`Authorization` and the persona's user token in `X-User-Token`. The
AuthBridge sidecar runs a two-stage pipeline, `mcp-parser -> cpex`,
and the `cpex` plugin evaluates `cpex-policy.yaml` in order.

```
chat.py / curl (host)
    |  POST /mcp
    |    Authorization: client token   ->  jwt-client
    |    X-User-Token:   persona token ->  jwt-user
    v  port-forward :8082 -> svc/authbridge-cpex:8080
+--------------------------------------------------------------+
| authbridge-cpex pod   (namespace: cpex-demo)                 |
|                                                              |
| AuthBridge pipeline:   mcp-parser  ->  cpex                  |
|   mcp-parser: parse JSON-RPC, set mcp.method / mcp.name      |
|   cpex:       run cpex-policy.yaml in order:                 |
|                                                              |
|     1. identity    resolve subject + client from the two     |
|                    JWTs, verified against Keycloak JWKS      |
|     2. APL policy  require(...) -> cedar PDP ->              |
|                    delegate(...) -> post-check               |
|     3. plugins     pii-scan (deny), audit-log (observe)      |
|     4. body        redact(args.ssn) / redact(result.ssn)     |
+--------------------------------------------------------------+
         |                                    |
         |  delegate(): RFC 8693              |  on allow:
         |  token exchange                    |  reverse-proxy
         v                                    v
   Keycloak  svc:8080                    hr-mcp  svc:9100
   realm cpex-demo                       get_compensation
   clients: hr-copilot, cpex-gateway,    send_email
            workday-api, github-api      search_repos, get_directory
```

Response-side redaction (`result.ssn`) runs as the reply flows back
out through the pipeline, so an SSN the backend returns unsolicited is
still stripped for a caller without the permission.

### Authorization patterns

All authorization lives in the `routes:` block at the bottom of
`cpex-policy.yaml`, one route per tool, each spelling out its own chain.
That block is the story; read it first. Everything above it is the
plugin toolbox the routes pull from. Two routes show the range, from a
flat permission gate to a four-layer chain.

**Workday flow** (`get_compensation`). The IdP carries every
permission directly on the user token. APL gates on a flat predicate
and redacts the SSN on the wire when the permission is missing:

```
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
kubectl -n cpex-demo rollout restart deployment/authbridge-cpex
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
| `k8s/30-authbridge-cpex.yaml` | AuthBridge config, Service, Deployment |
| `k8s/realm-export.json` | Keycloak realm: users, clients, mappers |
| `k8s/cpex-policy.yaml` | CPEX policy: identity, PDP, delegator, validators, audit |
| `hr-mcp-server/` | Vendored FastAPI MCP backend, built into `hr-mcp:dev` |
| `agent/` | Vendored chat agent: `chat.py`, requirements, walkthrough |
| `scenarios/` | The seven curl scenarios run by `make scenarios` |
| `mint-token.sh` | Mints a persona JWT via Keycloak. Used by scenarios and the agent |
| `verify-token-exchange.sh` | Checks RFC 8693 token exchange against the realm |

## Tear down

```bash
make undeploy   # deletes the cpex-demo namespace
```

## Troubleshooting

- **Rebuilt an image but the pod still runs the old one.** A rebuild
  leaves the Deployment spec identical, so `kubectl apply` does not roll
  the pod. After `kind load docker-image authbridge-cpex:dev`, force it
  with `kubectl -n cpex-demo rollout restart deployment/authbridge-cpex`.
- **Chat agent cannot reach the gateway.** `chat.py` defaults to
  `:8090`, but `make port-forward` exposes the gateway on `:8082`. Pass
  `GATEWAY_URL=http://localhost:8082/mcp`.
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
