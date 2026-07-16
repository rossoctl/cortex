# Component PRD: PDP Policy Writer (OPA)

## Location
`aiac/src/aiac/pdp/service/policy/opa/`

## Description
A FastAPI web service that translates a **Policy Model** into OPA Rego packages and writes them to an `AuthorizationPolicy` Kubernetes Custom Resource. The OPA plugin embedded in each AuthBridge instance fetches the Rego packages relevant to its pod from this CR at startup.

The service is deployed as a container in the **Kagenti Interface Pod** alongside the IdP Configuration Service, behind the `aiac-pdp-policy-service:7072` ClusterIP.

The service has no dependency on Keycloak. All Keycloak operations (entity reads) are handled by the **IdP Configuration Service** and its library (`aiac.idp.library.configuration`).

---

## Pydantic models (`aiac.policy.model.models`)

The Policy Writer deserializes the **canonical** `PolicyModel` / `AgentPolicyModel` / `PolicyRule` defined in [policy-model.md](policy-model.md) and imported from `aiac.policy.model.models`. This service does **not** define its own copies; the tables below summarize the fields the Rego generator consumes. (The former `aiac.pdp.library.models` module is deprecated — see policy-model.md "Replaces".)

All models use `model_config = ConfigDict(extra='ignore')`.

### `PolicyRule`

A single access rule pairing a typed role with a typed scope. Used in both inbound and outbound rule sets.

| Field | Type |
|-------|------|
| `role` | `Role` |
| `scope` | `Scope` |

`Role` and `Scope` are the typed models from `aiac.idp.configuration.models`. The Rego generator emits their `.name` as the string literal OPA matches against.

### `AgentPolicyModel`

Complete policy definition for a single agent (service). Contains two sets of `PolicyRule` entries plus supporting data maps used by the Rego packages.

| Field | Type | Description |
|-------|------|-------------|
| `agent_id` | `str` | Service ID from the AIAC trigger event (`aiac.apply.service.{id}`) |
| `agent_roles` | `list[Role]` | Realm roles assigned to this agent |
| `agent_scopes` | `list[Scope]` | Scopes this agent exposes |
| `source_roles` | `dict[str, list[Role]]` | Inbound: source (calling service) **id** → roles held. **Optional** gate input — an absent source passes. |
| `subject_roles` | `dict[str, list[Role]]` | Inbound: subject (end-user) **id** → roles held. **Mandatory** gate input. |
| `target_scopes` | `dict[str, list[Scope]]` | Outbound: target service **id** → scopes this agent may request on it |
| `inbound_rules` | `list[PolicyRule]` | Who may call this agent: `(subject_role, agent_scope)` tuples |
| `outbound_rules` | `list[PolicyRule]` | What this agent may call: `(this_agent_role, target_scope)` tuples |
| `outbound_subject_rules` | `list[PolicyRule]` | Which users may reach the agent's targets: `(user_role, tool_scope)` tuples. Defaults to `[]`. |

**`agent_roles` / `agent_scopes` provenance:** these carry the agent's **own** identity — the service-account realm roles it holds and the scopes it exposes. The Policy Computation Engine resolves them from the agent's IdP `Service` record (P2) and embeds them on every agent model it writes; a realm-level agent with no owning service keeps `[]`.

**Inbound rule semantics:** a subject holding realm role `role` is permitted to invoke this agent for the agent scope `scope`. Grouped by role, these rules become the `role_scopes` map (role → agent scopes) that the inbound package evaluates.

**Outbound rule semantics:** this agent acting as realm role `role` is permitted to request the target scope `scope`. Grouped by role, these rules become the `agent_role_scopes` map (agent role → target scopes) that the outbound package evaluates.

**Outbound subject rule semantics:** a subject holding realm role `role` (a **user** role) is permitted to reach a **tool** exposing scope `scope`. Grouped by role, these rules become the `outbound_subject_role_scopes` map (user role → tool scopes) that the **outbound** package's subject gate evaluates against `target_scopes[input.target]`. This is distinct from `inbound_rules` (user → *agent* scope): the outbound subject gate answers "may this user reach the tool?", not "may this user call the agent?".

**Note on `target_scopes` direction:** the map is keyed by **target service id → allowed scopes** (the inverse of the former `scope_targets`, which was `scope → targets`). The outbound Rego generator emits this map **verbatim** and evaluates `target_scopes[input.target]` directly — there is no inversion (see below).

### `PolicyModel`

A partial or full system policy model. When sent to the PDP Policy Writer, contains only the agents whose policies have changed.

| Field | Type |
|-------|------|
| `agents` | `list[AgentPolicyModel]` |

### Usage

```python
from aiac.policy.model.models import PolicyModel, AgentPolicyModel, PolicyRule
```

---

## Endpoints

No `?realm=` parameter — the service operates on a Kubernetes CR, not a Keycloak realm.

| Method | Path | Body | Operation |
|--------|------|------|-----------|
| `POST` | `/policy` | `PolicyModel` | Upsert Rego packages for all agents in the partial model |
| `POST` | `/policy/agents/{agent_id}` | `AgentPolicyModel` | Upsert Rego packages for a single agent |
| `DELETE` | `/policy/agents/{agent_id}` | — | Remove all Rego packages for a specific agent (off-boarding) |
| `DELETE` | `/policy` | — | Clear all Rego packages from the CR (rebuild pre-step) |
| `GET` | `/health` | — | Readiness probe |

### Status codes

| Endpoint | Success | Error |
|----------|---------|-------|
| `POST /policy` | `204 No Content` | `502 Bad Gateway` with `{"error": "..."}` if CR write fails |
| `POST /policy/agents/{agent_id}` | `204 No Content` | `502 Bad Gateway` if CR write fails |
| `DELETE /policy/agents/{agent_id}` | `204 No Content` | `502 Bad Gateway` if CR write fails |
| `DELETE /policy` | `204 No Content` | `502 Bad Gateway` if CR write fails |
| `GET /health` | `200 OK` `{"status": "ok"}` | `503 Service Unavailable` if CR is unreachable |

---

## Rego package structure

For each `AgentPolicyModel`, the service generates **two Rego packages**: one for the inbound pipeline and one for the outbound pipeline. The `agent_id` is slugified for use in the package name (hyphens → underscores, lowercase).

**Input is identifiers only.** Both packages receive an input document of **IDs**, never roles or scopes: inbound input is `{subject, source}`, outbound input is `{subject, target}` (`subject` is the end-user id, `source` the calling service id, `target` the called service id). Every role/scope mapping is therefore **embedded in the package itself**, and the `allow` logic resolves IDs → roles → scopes internally. Because no per-request scope is supplied, the decision is **coarse**: a principal passes when it has access to **at least one** relevant scope.

The generator embeds these symbols, derived from the `AgentPolicyModel`:

| Rego symbol | Source | Shape |
|-------------|--------|-------|
| `subject_roles` | `model.subject_roles` | subject id → `[role.name, …]` |
| `source_roles` | `model.source_roles` | source id → `[role.name, …]` |
| `agent_scopes` | `model.agent_scopes` | `[scope.name, …]` |
| `agent_roles` | `model.agent_roles` | `[role.name, …]` |
| `role_scopes` | grouped `model.inbound_rules` | role.name → `[scope.name, …]` (agent scopes granted per subject role) — **inbound package only** |
| `outbound_subject_role_scopes` | grouped `model.outbound_subject_rules` | role.name → `[scope.name, …]` (tool scopes granted per user role) — **outbound package only** |
| `agent_role_scopes` | grouped `model.outbound_rules` | role.name → `[scope.name, …]` (tool scopes per agent role) |
| `target_scopes` | `model.target_scopes` | target id → `[scope.name, …]` |

### Inbound package: `authz.{agent_slug}.inbound`

Evaluated by the AuthBridge OPA plugin in the **inbound pipeline** — "who may call this agent". Input document: `{subject, source}` (IDs). **`subject` is mandatory; `source` is optional** (an absent source passes). A principal passes when it holds a role that grants at least one of the agent's own scopes (`agent_scopes`).

```rego
package authz.{agent_slug}.inbound

agent_scopes := ["{scope.name}", ...]                # from agent_scopes

subject_roles := { "{subject_id}": ["{role.name}", ...], ... }
source_roles  := { "{source_id}":  ["{role.name}", ...], ... }

role_scopes := { "{role.name}": ["{scope.name}", ...], ... }   # from inbound_rules

subject_ok if {
    some role in subject_roles[input.subject]
    some scope in role_scopes[role]
    scope in agent_scopes
}
source_ok if { not input.source }                    # optional: absent source passes
source_ok if {
    some role in source_roles[input.source]
    some scope in role_scopes[role]
    scope in agent_scopes
}

default allow := false
allow if { subject_ok; source_ok }
```

### Outbound package: `authz.{agent_slug}.outbound`

Evaluated by the AuthBridge OPA plugin in the **outbound pipeline** — "what this agent may call". Input document: `{subject, target}` (IDs). The gate requires **both** the subject and the agent to pass, but the outbound **subject** gate is user→**tool** (distinct from the inbound user→agent gate): the subject must hold a role granting at least one **tool** scope the `target` accepts (via `outbound_subject_role_scopes`, grouped from `outbound_subject_rules`), **and** the agent (via its own `agent_roles`) must be permitted at least one scope that the `target` accepts. Both gates match against `target_scopes[input.target]`; `target_scopes` is consumed **directly** (target id → scopes) — it is not inverted. The inbound `role_scopes`/`agent_scopes` subject gate is **not** used here.

```rego
package authz.{agent_slug}.outbound

agent_roles  := ["{role.name}", ...]                 # from agent_roles
agent_scopes := ["{scope.name}", ...]                # from agent_scopes

subject_roles := { "{subject_id}": ["{role.name}", ...], ... }

outbound_subject_role_scopes := { "{role.name}": ["{scope.name}", ...], ... }   # from outbound_subject_rules (user role → tool scopes)
agent_role_scopes            := { "{role.name}": ["{scope.name}", ...], ... }   # from outbound_rules (agent role → tool scopes)
target_scopes                := { "{target_id}": ["{scope.name}", ...], ... }   # from target_scopes

# user may reach the tool: holds a role granting >=1 tool scope the target accepts
subject_ok if {
    some role in subject_roles[input.subject]
    some scope in outbound_subject_role_scopes[role]
    scope in target_scopes[input.target]
}
# agent may reach the tool: agent role grants >=1 tool scope the target accepts
target_ok if {
    some role in agent_roles
    some scope in agent_role_scopes[role]
    scope in target_scopes[input.target]
}

default allow := false
allow if { subject_ok; target_ok }
```

A worked example (agent `github-agent`, users `developer`/`tester`, tool `github-tool`) is maintained alongside the tests.

---

## Library: `aiac.pdp.library.policy`

HTTP client module wrapping the PDP Policy Writer REST API. Exposes four module-level functions. Service URL is read from the `AIAC_PDP_POLICY_URL` environment variable (default: `http://127.0.0.1:7072`). All functions raise `RuntimeError` on non-2xx response.

```python
def apply_policy(model: PolicyModel) -> None
    # POST /policy

def apply_agent_policy(agent_id: str, model: AgentPolicyModel) -> None
    # POST /policy/agents/{agent_id}

def delete_agent_policy(agent_id: str) -> None
    # DELETE /policy/agents/{agent_id}

def delete_policy() -> None
    # DELETE /policy
```

### Dependencies

```
requests
pydantic
python-dotenv
```

### Usage

```python
from aiac.pdp.library.policy import apply_policy, apply_agent_policy, delete_agent_policy, delete_policy
from aiac.policy.model.models import PolicyModel, AgentPolicyModel, PolicyRule

apply_agent_policy("weather-agent", agent_model)
delete_policy()
apply_policy(full_model)
```

---

## Configuration

| Variable | Required | Description |
|----------|----------|-------------|
| `AUTHORIZATION_POLICY_NAME` | TBD | Name of the `AuthorizationPolicy` CR to patch |
| `AUTHORIZATION_POLICY_NAMESPACE` | TBD | Namespace of the `AuthorizationPolicy` CR |

Authentication to the Kubernetes API: in-cluster service account (auto-detected by the `kubernetes` Python client). The pod's `ServiceAccount` must be bound to a `ClusterRole` granting `get`/`patch`/`update` on `AuthorizationPolicy` resources. The `ServiceAccount`, `ClusterRole`, and `ClusterRoleBinding` are declared in `pdp-interface-deployment.yaml`.

For local development, the `kubernetes` client falls back to `~/.kube/config` automatically.

> **Note:** `AuthorizationPolicy` CR schema and ConfigMap source for env vars are TBD.

---

## Runtime

- Framework: FastAPI
- Server: uvicorn
- Bind: `0.0.0.0:7072`
- Base image: `python:3.12-slim`
- Kubernetes ClusterIP Service: `aiac-pdp-policy-service:7072`
- Deployment: co-located with IdP Configuration Service as a container in the **Kagenti Interface Pod** (`pdp-interface-deployment.yaml`)

---

## Dependencies (`requirements.txt`)

```
fastapi
uvicorn[standard]
kubernetes
pydantic
```

---

## File structure

```
aiac/src/aiac/pdp/service/
├── __init__.py
└── policy/
    ├── __init__.py
    └── opa/
        ├── __init__.py
        ├── Dockerfile
        ├── requirements.txt
        └── main.py

aiac/src/aiac/pdp/
├── __init__.py
└── library/
    ├── __init__.py
    └── policy.py       # apply_policy, apply_agent_policy, delete_agent_policy, delete_policy
                        # (models now imported from aiac.policy.model.models)
```

Build command:
```bash
docker build -f aiac/src/aiac/pdp/service/policy/opa/Dockerfile \
  -t aiac-pdp-policy-opa:latest aiac/src/
```

---

## `main.py` behaviour notes

- Load Kubernetes in-cluster config at startup via `kubernetes.config.load_incluster_config()`; fall back to `kubernetes.config.load_kube_config()` for local development.
- Instantiate a `kubernetes.client.CustomObjectsApi` for all CR operations.
- `_slugify(agent_id: str) -> str`: replace hyphens with underscores, lowercase — produces a valid Rego package name segment.
- `_generate_inbound_rego(model: AgentPolicyModel) -> str`: render the inbound Rego package string. Embeds `agent_scopes`, `subject_roles`, `source_roles`, and a `role_scopes` map (grouping `inbound_rules` by role → agent scope names); emits `subject_ok` (mandatory) and `source_ok` (optional — an absent `input.source` passes); `allow if { subject_ok; source_ok }`.
- `_generate_outbound_rego(model: AgentPolicyModel) -> str`: render the outbound Rego package string. Embeds `agent_roles`, `agent_scopes`, `subject_roles`, `outbound_subject_role_scopes` (from `outbound_subject_rules`), `agent_role_scopes` (from `outbound_rules`), and `target_scopes` (consumed directly, target id → scopes — **no inversion**); emits a user→tool `subject_ok` (matching `scope in target_scopes[input.target]`, **not** `role_scopes`/`agent_scopes`) and `target_ok`; `allow if { subject_ok; target_ok }`. The inbound `role_scopes` map is **not** embedded in the outbound package.
- `_upsert_agent(agent_id: str, inbound_rego: str, outbound_rego: str)`: patch the `AuthorizationPolicy` CR to upsert the two packages for `agent_id`. Schema TBD.
- `_delete_agent(agent_id: str)`: patch the CR to remove all packages for `agent_id`.
- `_delete_all()`: patch the CR to remove all packages.
- `POST /policy`: iterate `model.agents`; for each call `_generate_inbound_rego` + `_generate_outbound_rego` + `_upsert_agent`; return `Response(status_code=204)`.
- `POST /policy/agents/{agent_id}`: call `_generate_inbound_rego` + `_generate_outbound_rego` + `_upsert_agent`; return `Response(status_code=204)`.
- `DELETE /policy/agents/{agent_id}`: call `_delete_agent(agent_id)`; return `Response(status_code=204)`.
- `DELETE /policy`: call `_delete_all()`; return `Response(status_code=204)`.
- On Kubernetes API error, return HTTP 502 with `{"error": str(e)}`.
- `GET /health`: attempt to `get` the `AuthorizationPolicy` CR; return `200 {"status": "ok"}` on success, `503` on failure.
