# Demo Spec: `github-tool` (MCP) — minimal source + issue tool for UC-1 onboarding

> **Status:** spec (design source of truth). This document describes the files to be built;
> it does not create them.
>
> **This is a demo/reference tool**, not part of the AIAC service tree (`src/aiac/`). It is a
> self-contained, deployable MCP tool server whose sole job is to make **UC-1 service onboarding**
> (`../components/aiac-agent/uc1-service-onboarding.md`, node `analyze_tool`) discover exactly the
> four canonical `github-tool` scenario scopes, deterministically. It is the tool sibling of the
> agent spec at [`github-agent.md`](github-agent.md).

---

## 1. Purpose & scenario mapping

The AIAC phase-1 deliverable ([`../../gh-issues/sub-issue-phase-1.md`](../../gh-issues/sub-issue-phase-1.md))
demonstrates **service onboarding (UC-1)** end-to-end for one agent (`github-agent`) and one tool
(`github-tool`): AIAC classifies each service, discovers its capabilities, provisions the identities
they need, models access, and emits rules — which are then validated by **rule evaluation**, not by
live traffic. Phase 1 is explicitly **"Deploy + discover + evaluate": no live A2A traffic and no live
enforcement.** The tool is onboarded and its scopes are evaluated; it is **never actually driven by
the agent.**

This spec defines the **tool half** of that demo: a **real, deployable MCP endpoint** that answers
`tools/list` with exactly four tools, so that UC-1's `analyze_tool` node derives exactly the four
canonical scenario tool scopes.

### How UC-1 consumes this tool

From `analyze_tool` (`../components/aiac-agent/uc1-service-onboarding.md`):

1. **`classify_service`** resolves identity from the Keycloak client's `client.name` (the operator sets
   it to `"{namespace}/{workload_name}"`), splits on the first `/` → `namespace` + `workload_name`,
   LISTs pods in the namespace, finds the pod owned by `workload_name`, and reads the `rossoctl.io/type`
   pod label. It **must be `tool`** → routes to `analyze_tool`.
2. **`analyze_tool`** does `read_service(workload_name, namespace)` (the K8s Service named
   `workload_name`), **requires the `protocol.rossoctl.io/mcp` label** on that Service (a deploy-time
   prerequisite — the operator does **not** stamp it), takes the Service's **first port**, and POSTs
   JSON-RPC `tools/list` to
   `http://{workload_name}.{namespace}.svc.cluster.local:{port}/mcp`.
3. UC-1 derives **one scope per returned tool**:
   `ScopeDefinition(name=f"{workload_name}.{tool.name}", description=tool.description)`.

So with workload `github-tool` and a tool named `source-read`, the provisioned scope is
`github-tool.source-read` with `description =` that tool's description.

### Mapping to the policy-pipeline scenario

The canonical scenario ([`../../../test/integration/scenario.py`](../../../test/integration/scenario.py),
`TOOL_SCOPES`) and its spec
([`../integration-test/policy-pipeline.md`](../integration-test/policy-pipeline.md),
*Role & scope descriptions → Tool scopes*) fix **four** `github-tool` scopes. This tool exposes
exactly the four MCP tools whose names + descriptions make UC-1 reproduce them:

| Scenario tool scope | MCP tool name | UC-1-derived scope (`workload_name=github-tool`) |
|---|---|---|
| `source-read` | `source-read` | `github-tool.source-read` |
| `source-write` | `source-write` | `github-tool.source-write` |
| `issues-read` | `issues-read` | `github-tool.issues-read` |
| `issues-write` | `issues-write` | `github-tool.issues-write` |

The tool does **not** know or enforce these scopes — AIAC/OPA + AuthBridge do (in later phases).
Phase 1 only discovers and evaluates them.

### Related artefacts
- Phase-1 deliverable: [`../../gh-issues/sub-issue-phase-1.md`](../../gh-issues/sub-issue-phase-1.md)
- UC-1 `analyze_tool`: [`../components/aiac-agent/uc1-service-onboarding.md`](../components/aiac-agent/uc1-service-onboarding.md)
- Scenario fixture (`TOOL_SCOPES`): [`../../../test/integration/scenario.py`](../../../test/integration/scenario.py)
- Scenario spec: [`../integration-test/policy-pipeline.md`](../integration-test/policy-pipeline.md)
- Sibling agent spec: [`github-agent.md`](github-agent.md)
- Reference deployment: `cortex/authbridge/demos/github-issue/k8s/`

---

## 2. Relationship to the real `github-tool` (deliberate divergence)

The sibling agent spec ([`github-agent.md`](github-agent.md) §2, §5, §7) is written against a
**production** `github-tool`: a real MCP server reachable at `github-tool-mcp:9090/mcp` that federates
**44 real GitHub tools** and proxies calls to the GitHub API (swapping the exchanged token for a GitHub
PAT by scope in a MitM). That tool is the runtime target for live A2A traffic in later phases.

**This spec deliberately diverges from that production tool** in three ways, and the divergences are
intentional:

1. **Four tools, not 44.** This tool exposes exactly `source-read`, `source-write`, `issues-read`,
   `issues-write` — one per canonical scenario scope — so UC-1 discovery yields *exactly* the four
   scenario tool scopes and nothing else. The production 44-tool catalog would yield 44 fine-grained
   scopes that do not match the scenario truth table.
2. **Stub handlers, no GitHub.** Tool **calls** are trivial no-op/echo stubs; there are no real GitHub
   API calls, no PAT, and no MitM. Phase 1 drives no live traffic, so nothing calls the tools.
3. **Workload name `github-tool`, not `github-tool-mcp`.** See §6 (naming invariants). The scenario
   entity id is `github-tool`, which keeps UC-1 identity resolution clean.

In short: this is a **purpose-built, minimal stand-in** used ONLY to make UC-1's discovery deterministic
for the phase-1 demo. It is distinct from — and not a replacement for — the production 44-tool
`github-tool` referenced by [`github-agent.md`](github-agent.md). When later phases wire live traffic,
they use the production tool; this stand-in serves the discover-and-evaluate loop only.

---

## 3. The four MCP tools (verbatim scenario descriptions)

Exactly four tools. Names are chosen so UC-1 derives `github-tool.<name>`; descriptions are copied
**verbatim** from `scenario.py::TOOL_SCOPES` / the policy-pipeline spec's *Tool scopes* section (each is
≤255 chars, authored to be generic/keyword-free so the PRB maps them correctly — do not paraphrase):

| Tool name | Description (verbatim) |
|---|---|
| `source-read` | `Read source repository contents: file listings and file bodies. Read-only.` |
| `source-write` | `Create, modify, or delete source repository contents; commit file changes.` |
| `issues-read` | `Read issues and their comment threads. Read-only.` |
| `issues-write` | `Create and update issues: open, edit, comment, and close.` |

Each tool has a **minimal / empty** `inputSchema` (`{"type": "object", "properties": {}}`) — phase 1
inspects only `name` + `description`, never arguments.

> **Invariant:** the description strings here must remain byte-identical to `TOOL_SCOPES` in
> `scenario.py`. UC-1 copies them into `ScopeDefinition.description`, and the PRB LLM maps roles→scopes
> from that text; a drift would change the provisioned scope descriptions and could move the truth
> table. If `TOOL_SCOPES` changes, change this table with it.

---

## 4. MCP server (minimal, real, deployable)

A tiny, self-contained MCP server that serves **JSON-RPC 2.0 over HTTP at path `/mcp`** and correctly
answers `tools/list` with the four tools of §3.

- **Transport / protocol:** JSON-RPC 2.0 over HTTP `POST /mcp` (the MCP streamable-HTTP endpoint
  convention `analyze_tool` posts to). It must respond to a `tools/list` request with the four tools
  (each `name` + `description` + minimal `inputSchema`). Implementing the MCP `initialize` handshake is
  fine but not required by UC-1, which posts `tools/list` directly.
- **Stack:** a small Python server, consistent with the repo (Python 3.12). Either the official MCP
  Python SDK in streamable-HTTP mode, or a hand-rolled minimal ASGI/HTTP handler for `POST /mcp` — pick
  whichever keeps the image smallest and the `tools/list` contract obvious. No CrewAI, no A2A, no LLM.
- **Tool registry:** a single editable constant — the four `(name, description, inputSchema)` tuples of
  §3 — so the list stays legible and auditable against `scenario.py::TOOL_SCOPES`.
- **Tool-CALL handlers:** trivial **no-op / echo stubs**. A `tools/call` for any of the four returns a
  stub result (e.g. a text content block echoing the tool name + received arguments, or a fixed
  `"stub: not implemented in phase-1 demo"` message). They perform **no** GitHub work. This is
  acceptable because phase 1 drives no live traffic — but the endpoint must still be a **real,
  deployable MCP server** that answers `tools/list`.
- **Location:** self-contained under `aiac/demo/tools/github_tool/`, mirroring the agent's
  `aiac/demo/agents/github_agent/` layout. Ships its own `Dockerfile`, dependency manifest, and the
  `k8s/` manifests of §7.
- **Listen:** binds `0.0.0.0` on a container `PORT` (see §5) and serves `/mcp`.

```
 UC-1 analyze_tool ──(JSON-RPC POST /mcp: tools/list)──► github-tool (:PORT) ──► 4 tools
   (resolves http://github-tool.team1.svc.cluster.local:{first-port}/mcp)
 (phase 1: no tools/call traffic — stub handlers are never exercised by the agent)
```

---

## 5. Configuration

Minimal env, adapted to the tiny server:

| Variable | Description | Default |
|---|---|---|
| `PORT` | HTTP listen port serving `/mcp` | `9090` |
| `LOG_LEVEL` | Log level | `INFO` |

No Keycloak, LLM, GitHub PAT, JWKS, or audience config — the demo tool performs no auth and no upstream
calls (contrast the github-issue demo's `github-tool`, which needs PATs + issuer/JWKS/audience). Any
auth enforcement in front of this tool is AuthBridge/MitM's job in later phases, not this container's.

---

## 6. Naming & consistency invariants

State these explicitly; UC-1 identity resolution depends on all of them holding:

- **One name everywhere:** workload name == K8s `Deployment` name == K8s `Service` name == the
  `AgentRuntime` `targetRef.name` == the Keycloak client's workload segment == **`github-tool`**.
  Consequently:
  - the operator sets the Keycloak client `client.name = "team1/github-tool"`, so `classify_service`
    splits it into `namespace=team1`, `workload_name=github-tool`;
  - `analyze_tool` resolves the endpoint to
    `http://github-tool.team1.svc.cluster.local:{first-port}/mcp`;
  - UC-1 derives scopes `github-tool.source-read`, `github-tool.source-write`,
    `github-tool.issues-read`, `github-tool.issues-write`.
- **Keycloak client id == `github-tool`** — matches `scenario.py`'s `TOOL_ID = "github-tool"`, so the
  scenario/integration test and this deployed tool agree on the entity id.
- **Divergence from the github-issue demo (`github-tool-mcp`) is intentional.** The github-issue demo
  and [`github-agent.md`](github-agent.md) use a Service named **`github-tool-mcp`** on port `9090`.
  This demo names both the workload and the Service **`github-tool`** because:
  - the canonical scenario entity id is `github-tool` (`scenario.py` `TOOL_ID`), and UC-1 derives the
    scope prefix from the **workload name** — a `github-tool-mcp` workload would yield
    `github-tool-mcp.source-read`, not the canonical `github-tool.source-read`;
  - `analyze_tool` relies on the operator convention **Service name == workload name**, so the Service
    must also be `github-tool` for endpoint resolution to work off `client.name`.
  Net: keeping a single `github-tool` name across workload / Service / Keycloak client keeps UC-1
  resolution clean and the derived scopes canonical. (The production live-traffic path continues to use
  `github-tool-mcp:9090/mcp` per the agent spec; the two are separate deployments.)

---

## 7. Deployment (aiac level)

Manifests live under `aiac/demo/tools/github_tool/k8s/`, adapted from the sibling agent's §7 and the
github-issue demo's `github-tool-deployment.yaml`. Namespace **`team1`** (installer-provided
ConfigMaps/secrets assumed present), consistent with the agent spec.

- **`github-tool-deployment.yaml`** — `Deployment` + `Service` + `AgentRuntime`:
  - **`Deployment`** named `github-tool`. Container port serves `/mcp` (default `9090`). Env `PORT`,
    `LOG_LEVEL`. Image `github-tool:latest`, `imagePullPolicy: IfNotPresent` (kind-load; name is a
    documented knob). No GitHub PAT / issuer / JWKS / audience env (unlike the github-issue tool).
  - **Pod label `rossoctl.io/type: tool`** — this is what `classify_service` reads. Applied by the
    operator via the `AgentRuntime` (see below); relying on the operator to stamp it, rather than
    hand-setting it, keeps it consistent with the operator's own discriminator.
  - **`Service`** named `github-tool` (ClusterIP), selecting the Deployment's pods. Its **first port**
    maps to the container's `/mcp` port (e.g. `port: 9090 → targetPort: 9090`). `analyze_tool` uses the
    Service's **first** port, so keep `/mcp`'s port first.
  - **Service label `protocol.rossoctl.io/mcp` MUST be present** — a **deploy-time prerequisite** for
    `analyze_tool` (the operator does **not** stamp it; `analyze_tool` returns `502` if absent). Set it
    explicitly on the Service metadata (e.g. `protocol.rossoctl.io/mcp: "true"`).
  - **`AgentRuntime{ type: tool, targetRef: { apiVersion: apps/v1, kind: Deployment, name: github-tool } }`**
    — enrolls the workload so the operator applies `rossoctl.io/type=tool` to the pod and
    registers a Keycloak client with `client.name = "team1/github-tool"`. Unlike the github-issue demo
    (which omits `rossoctl.io/type` to skip AuthBridge entirely), this demo **needs** `type: tool` so the
    pod carries `rossoctl.io/type=tool` for UC-1 `classify_service`.

- **No `configmaps.yaml` needed here** — the tool has no `authbridge-config` / `authproxy-routes`
  dependency (it neither validates inbound JWTs nor does outbound token exchange in phase 1). The agent
  spec's `authproxy-routes` still targets the **production** `github-tool-mcp` host and is unrelated to
  this stand-in.

- **Prerequisite (reused, not created here):** a running Rossoctl cluster with the operator
  (Keycloak realm as configured by the installer, namespace `team1`), so the `AgentRuntime` is
  reconciled into a Keycloak client + pod label.

**Wiring invariant:** `AgentRuntime.targetRef.name` == `Deployment` name == `Service` name ==
`github-tool`; the `Service` carries the `protocol.rossoctl.io/mcp` label and exposes `/mcp` as its first
port; the operator-applied pod label is `rossoctl.io/type=tool`; the operator-registered Keycloak
`client.name` is `team1/github-tool`.

---

## 8. Verification

**Local (no cluster — primary gate):**
1. `cd aiac/demo/tools/github_tool` and build: `podman build -t github-tool:latest .`.
2. Run the container (`-e PORT=9090 -p 9090:9090`), then POST a JSON-RPC `tools/list` to `/mcp` and
   confirm the four tool names:
   ```bash
   curl -s -X POST localhost:9090/mcp \
     -H 'Content-Type: application/json' \
     -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' \
   | jq -r '.result.tools[].name' | sort
   # → issues-read, issues-write, source-read, source-write
   ```
3. Confirm each returned tool's `description` is byte-identical to the matching `TOOL_SCOPES` entry in
   `scenario.py` (e.g. `jq '.result.tools[] | {name, description}'`).

**Cluster (HITL — live Rossoctl + Keycloak + operator):**
4. `kind load docker-image github-tool:latest --name rossoctl`.
5. `kubectl apply -f k8s/github-tool-deployment.yaml`.
6. Confirm the operator applied the pod label: `kubectl get pod -l app=github-tool -n team1
   -o jsonpath='{.items[0].metadata.labels.rossoctl\.io/type}'` → `tool`.
7. Confirm the Service carries the MCP label: `kubectl get svc github-tool -n team1
   -o jsonpath='{.metadata.labels.protocol\.rossoctl\.io/mcp}'` → present.
8. Confirm the operator registered a Keycloak client with `client.name = "team1/github-tool"` (via the
   Keycloak admin API / IdP `Configuration` library).
9. From inside the cluster (or via `kubectl port-forward svc/github-tool 9090:9090 -n team1`), POST
   `tools/list` to `http://github-tool.team1.svc.cluster.local:9090/mcp` and confirm the four tools —
   the exact call UC-1 `analyze_tool` makes.
10. (End-to-end) Trigger UC-1 onboarding for `github-tool` and confirm it provisions scopes
    `github-tool.source-read` / `github-tool.source-write` / `github-tool.issues-read` /
    `github-tool.issues-write`, and writes **no rules for the tool alone** (per the phase-1 acceptance
    criteria).

---

## 9. Out of scope

- **Real GitHub API calls / real source & issue operations** — tool-call handlers are stubs; there is
  no GitHub PAT and no upstream.
- **Auth enforcement inside the tool** — no inbound JWT validation, no outbound token exchange, no
  audience/scope checks. AuthBridge / the MitM handle enforcement in later phases.
- **The production 44-tool federation** — this stand-in exposes only the four canonical scenario tools
  (see §2); the real `github-tool` (`github-tool-mcp:9090/mcp`) is unchanged and used by the live-traffic
  path.
- **Being agent-executable** — phase 1 drives no A2A traffic, so the tool is discovered and evaluated but
  never invoked by `github-agent`.
- **Any changes to the AIAC pipeline, UC-1, `policy-pipeline.md`, or the integration test** — this tool
  is an input to UC-1 discovery, not a change to it.
- **Building this tool into `agent-examples` CI** — aiac/demo images build independently (as with the
  agent spec).
