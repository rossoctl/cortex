# Component PRD: PDP Policy Library (`aiac.pdp.policy.library`)

HTTP client module wrapping the PDP Policy Writer (OPA) REST API. These modules have no dependency on Keycloak — all IdP operations use `aiac.idp.configuration`.

## Location
`aiac/src/aiac/pdp/policy/library/`

## Package structure

```
aiac/src/aiac/pdp/policy/
└── library/
    ├── __init__.py     # empty
    └── api.py          # apply_policy, apply_agent_policy, delete_agent_policy, delete_policy
```

All `__init__.py` files are empty. Callers use explicit submodule paths:

```python
from aiac.pdp.policy.library.api import apply_policy, apply_agent_policy, delete_agent_policy, delete_policy
from aiac.policy.model.models import PolicyModel, AgentPolicyModel
```

---

## Submodule: `aiac.pdp.policy.library.api`

### Description
HTTP client module wrapping the PDP Policy Writer REST API. Exposes four module-level functions. Service URL is read from the `AIAC_PDP_POLICY_URL` environment variable (default: `http://127.0.0.1:7072`). All functions raise `RuntimeError` on non-2xx response.

No `realm` parameter — the PDP Policy Writer operates on a Kubernetes CR, not a Keycloak realm.

**Primary consumer:** `aiac.policy.computation` — the Policy Computation Engine is the only caller. AIAC Agent sub-UC agents do not call this library directly; they call `compute_and_apply` instead.

### Dependencies
```
requests
pydantic
python-dotenv
```

### Functions

```python
def apply_policy(model: PolicyModel) -> None
    # POST /policy — upsert Rego packages for all agents in the partial model

def apply_agent_policy(agent_id: str, model: AgentPolicyModel) -> None
    # POST /policy/agents/{agent_id} — upsert Rego packages for a single agent

def delete_agent_policy(agent_id: str) -> None
    # DELETE /policy/agents/{agent_id} — remove all Rego packages for agent (off-boarding)

def delete_policy() -> None
    # DELETE /policy — clear all Rego packages (rebuild pre-step)
```

### Configuration

Read from `AIAC_PDP_POLICY_URL` environment variable (or `.env` file co-located with `api.py`). Falls back to the default if absent.

| Variable | Default |
|----------|---------|
| `AIAC_PDP_POLICY_URL` | `http://127.0.0.1:7072` |

### Usage

```python
from aiac.pdp.policy.library.api import apply_policy, apply_agent_policy, delete_agent_policy, delete_policy
from aiac.policy.model.models import PolicyModel, AgentPolicyModel

# Single-agent update (called by Policy Computation Engine)
apply_agent_policy("weather-agent", agent_model)

# Full rebuild pre-step: clear all, then reapply
delete_policy()
apply_policy(full_model)

# Off-boarding
delete_agent_policy("weather-agent")
```

---

## Testing Decisions

**Seam:** HTTP boundary — mock responses from `AIAC_PDP_POLICY_URL`.

**Prior art:** `3.14-unit-tests-write-api.md` (mock PDP Policy Writer HTTP; cover module-level functions).

Key behaviors to assert:
- `apply_policy(model)` issues `POST /policy` with serialized `PolicyModel`.
- `apply_agent_policy(id, model)` issues `POST /policy/agents/{id}` with serialized `AgentPolicyModel`.
- `delete_agent_policy(id)` issues `DELETE /policy/agents/{id}`.
- `delete_policy()` issues `DELETE /policy`.
- Any non-2xx response raises `RuntimeError`.
- `AIAC_PDP_POLICY_URL` is read from env; falls back to `http://127.0.0.1:7072`.

---

## Out of Scope

- **Keycloak interaction:** this library never calls Keycloak directly. All IdP operations go through `aiac.idp.configuration`.
- **Policy computation:** translating `list[PolicyRule]` into `AgentPolicyModel` objects is the responsibility of `aiac.policy.computation`, not this library.
- **Policy persistence:** the Policy Store (`aiac.policy.store`) owns structured `AgentPolicyModel` durability. This library targets the OPA runtime only.

---

## Further Notes

- The `aiac.pdp.library.policy` module (old path) is deprecated. All consumers must update imports to `aiac.pdp.policy.library.api`.
- Models (`PolicyModel`, `AgentPolicyModel`) are imported from `aiac.policy.model.models`, not from the deprecated `aiac.pdp.library.models`.
