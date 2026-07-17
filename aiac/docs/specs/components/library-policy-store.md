# Component PRD: Policy Store Library (`aiac.policy.store.library`)

Companion library for the [AIAC Policy Store](policy-store.md). Follows the same pattern as `aiac.pdp.policy.library` — module-level functions, URL from env var via `python-dotenv`, `RuntimeError` on non-2xx.

## Location
`aiac/src/aiac/policy/store/library/`

## Package structure

```
aiac/src/aiac/policy/store/
└── library/
    ├── __init__.py     # empty
    └── api.py          # six module-level functions
```

All `__init__.py` files are empty. Callers use explicit submodule paths:

```python
from aiac.policy.store.library.api import (
    get_policy, get_agent_policy,
    apply_policy, apply_agent_policy,
    delete_agent_policy, delete_policy,
)
from aiac.policy.model.models import PolicyModel, AgentPolicyModel
```

---

## Submodule: `aiac.policy.store.library.api`

### Description
HTTP client module wrapping the [AIAC Policy Store](policy-store.md) REST API. Exposes six module-level functions returning `PolicyModel` and `AgentPolicyModel` objects directly — no Kubernetes client boilerplate. Service URL is read from the `AIAC_POLICY_STORE_URL` environment variable (default: `http://127.0.0.1:7074`). All functions raise `RuntimeError` on non-2xx response.

### Dependencies
```
requests
pydantic
python-dotenv
```

### Functions

```python
def get_policy() -> PolicyModel
    # GET /policy

def get_agent_policy(agent_id: str) -> AgentPolicyModel
    # GET /policy/agents/{agent_id}

def apply_policy(model: PolicyModel) -> None
    # POST /policy

def apply_agent_policy(agent_id: str, model: AgentPolicyModel) -> None
    # POST /policy/agents/{agent_id}

def delete_agent_policy(agent_id: str) -> None
    # DELETE /policy/agents/{agent_id}

def delete_policy() -> None
    # DELETE /policy
```

### Configuration

Read from `AIAC_POLICY_STORE_URL` environment variable (or `.env` file co-located with `api.py`). Falls back to the default if absent.

| Variable | Default |
|----------|---------|
| `AIAC_POLICY_STORE_URL` | `http://127.0.0.1:7074` |

### Usage

```python
from aiac.policy.store.library.api import (
    get_policy, get_agent_policy,
    apply_policy, apply_agent_policy,
    delete_agent_policy, delete_policy,
)
from aiac.policy.model.models import PolicyModel, AgentPolicyModel

# Read current state for additive merge
current = get_agent_policy("weather-agent")

# Write updated state
apply_agent_policy("weather-agent", updated_model)

# Full rebuild
delete_policy()
apply_policy(full_model)

# Off-boarding
delete_agent_policy("weather-agent")
```

---

## Testing Decisions

**Seam:** HTTP boundary — mock responses from `AIAC_POLICY_STORE_URL`.

**Prior art:** `3.14-unit-tests-write-api.md` (mock PDP Policy Writer HTTP; cover module-level functions).

Key behaviors to assert:
- `get_policy()` issues `GET /policy`; response body deserialized to `PolicyModel`.
- `get_agent_policy(id)` issues `GET /policy/agents/{id}`; response body deserialized to `AgentPolicyModel`.
- `apply_policy(model)` issues `POST /policy` with serialized `PolicyModel`.
- `apply_agent_policy(id, model)` issues `POST /policy/agents/{id}` with serialized `AgentPolicyModel`.
- `delete_agent_policy(id)` issues `DELETE /policy/agents/{id}`.
- `delete_policy()` issues `DELETE /policy`.
- Any non-2xx response raises `RuntimeError`.
- `AIAC_POLICY_STORE_URL` is read from env; falls back to `http://127.0.0.1:7074`.
