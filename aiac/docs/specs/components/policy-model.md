# Component PRD: Policy Model (`aiac.policy.model`)

## Problem Statement

`PolicyRule`, `AgentPolicyModel`, and `PolicyModel` were previously defined in `aiac.pdp.library.models`. Three independent consumers now need these types:

- `aiac.pdp.policy.library` — translates `PolicyModel` into HTTP calls to the PDP Policy Writer
- `aiac.policy.store.library` — reads/writes `AgentPolicyModel` from/to the Policy Store
- `aiac.policy.computation` — builds and merges `AgentPolicyModel` objects

Keeping the canonical model definitions inside a PDP-namespaced module (`aiac.pdp.library.models`) forces both the Policy Store library and the Policy Computation Engine to take a dependency on the PDP package — a wrong-layer coupling. Any of the three consumers importing from `aiac.pdp.library.models` would create a transitive dependency on an unrelated service namespace.

Additionally, the old `PolicyRule` used plain `str` for `role` and `scope`. The PCE algorithm requires typed `Role` and `Scope` objects (from `aiac.idp.configuration.models`) to invoke `Configuration.get_services_by_role` and `Configuration.get_services_by_scope`.

Finally, the original `source_roles`, `subject_roles`, and `scope_targets` maps used pydantic model objects (`Service`, `Subject`, `Scope`) as dict keys. Model-object keys do not round-trip through `model_dump(mode="json")` / JSON without custom key handling, and they couple `aiac.policy.model` to the id-only `__hash__`/`__eq__` of the IdP models. The outbound map was also keyed by scope (`scope → targets`), whereas consumers need the inverse (`target → scopes`) to emit per-target authorization directly.

## Solution

A canonical, dependency-free model module at `aiac.policy.model` defines `PolicyRule`, `AgentPolicyModel`, and `PolicyModel` with typed fields. No HTTP client, no service code — importable by any consumer without side effects. `PolicyRule.role` and `PolicyRule.scope` are typed `Role` and `Scope` objects from `aiac.idp.configuration.models`.

The relationship maps (`source_roles`, `subject_roles`, `target_scopes`) are keyed by the string `id` of the referenced entity rather than by a typed object, so they serialize to JSON natively and carry no hashability requirement into `aiac.policy.model`. Typed `Role` / `Scope` objects are retained as the map *values* (and in `PolicyRule`), preserving the typing the PCE needs for IdP queries. The outbound map is `target_scopes` (`target service id → scopes permitted`), the inverse of the former `scope_targets`.

---

## User Stories

1. As the Policy Computation Engine, I want to import `PolicyRule`, `AgentPolicyModel`, and `PolicyModel` from a shared, neutral namespace, so that I do not take an unwanted dependency on the PDP package.
2. As the PDP Policy Library, I want to import `PolicyModel` and `AgentPolicyModel` from `aiac.policy.model`, so that my HTTP serialization logic does not duplicate model definitions.
3. As the Policy Store Library, I want to import `AgentPolicyModel` and `PolicyModel` from `aiac.policy.model`, so that response deserialization uses the same canonical types as every other consumer.
4. As an AIAC Agent sub-UC agent, I want to construct a `PolicyRule` with typed `Role` and `Scope` objects, so that the PCE can use them for IdP queries without additional type conversion.
5. As the Policy Computation Engine, I want `source_roles`, `subject_roles`, and `target_scopes` keyed by string entity IDs, so that I build them with `entity.id` and they serialize to JSON without custom key handling.
6. As a developer, I want all models to silently ignore unknown fields from API responses, so that IdP API additions do not break deserialization.
7. As the PDP Policy Library, I want outbound permissions expressed as `target service id → allowed scopes`, so that I can emit per-target authorization directly without inverting a `scope → targets` map.
8. As a consumer serializing an `AgentPolicyModel` to JSON, I want every relationship map to have string keys, so that `model_dump(mode="json")` round-trips without a custom key serializer.

---

## Implementation Decisions

### Module Identity

**Namespace:** `aiac.policy.model`

**Location:** `aiac/src/aiac/policy/model/`

**Package structure:**

```
aiac/src/aiac/policy/
└── model/
    ├── __init__.py    # empty
    └── models.py      # PolicyRule, AgentPolicyModel, PolicyModel
```

### Dependencies

| Dependency | Purpose |
|------------|---------|
| `pydantic` | `BaseModel`, `ConfigDict` |
| `aiac.idp.configuration.models` | Typed `Role`, `Scope` (as map values and in `PolicyRule`) |

No HTTP client dependency. No `requests`, no `python-dotenv`.

### Pydantic Models

All models use `model_config = ConfigDict(extra='ignore')`.

#### `PolicyRule`

A single access rule pairing a typed role with a typed scope. Used in both inbound and outbound rule sets.

| Field | Type | Description |
|-------|------|-------------|
| `role` | `Role` | Typed role from `aiac.idp.configuration.models` |
| `scope` | `Scope` | Typed scope from `aiac.idp.configuration.models` |

#### `AgentPolicyModel`

Complete policy definition for a single agent (service). Inbound and outbound rule sets are typed collections.

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

**Inbound rule semantics:** a subject holding realm role `role` is permitted to invoke this agent for the agent scope `scope`. The PDP Policy Writer consumes `inbound_rules` as a role → agent-scope map; its inbound gate is keyed on the subject id (mandatory), with the calling source id optional.

**Outbound rule semantics:** this agent acting as realm role `role` is permitted to request the target scope `scope`. The PDP Policy Writer consumes `outbound_rules` as an agent-role → target-scope map; its outbound gate requires both the subject and the agent to be authorized.

**Outbound subject rule semantics:** `outbound_subject_rules` holds `(user role, tool scope)` pairs — the outbound subject gate; a user holding `role` may reach a tool exposing `scope`. It is the outbound counterpart of `inbound_rules` (which pairs a user role with an *agent* scope): where `inbound_rules` answers "may this user call the agent?", `outbound_subject_rules` answers "may this user reach the tool the agent targets?". The PDP Policy Writer groups it into `outbound_subject_role_scopes` (user role → tool-scope names) and matches against `target_scopes[input.target]`, not against `agent_scopes`.

#### `PolicyModel`

A partial or full system policy model. When sent to `POST /policy` on the Policy Store, it may contain only the agents whose policies have changed.

| Field | Type |
|-------|------|
| `agents` | `list[AgentPolicyModel]` |

### Map keys are string IDs

`source_roles`, `subject_roles`, and `target_scopes` are keyed by the string `id` of the referenced Keycloak entity (source service id, subject id, target service id) rather than by the typed `Service` / `Subject` / `Scope` object. Rationale:

- JSON object keys must be strings. A dict keyed by a pydantic model does not round-trip through `model_dump(mode="json")` / JSON without a custom key serializer; a `str` key serializes natively.
- The IdP models are plain pydantic models (default field-based equality, not hashable). Consumers build these maps with `entity.id` as the key.

As a result, no field in `aiac.policy.model` uses a typed object as a dict key, and this module imports only `Role` and `Scope` from `aiac.idp.configuration.models` (as map *values* and in `PolicyRule`). `Service` and `Subject` are no longer referenced here.

### Usage

```python
from aiac.policy.model.models import PolicyRule, AgentPolicyModel, PolicyModel
from aiac.idp.configuration.models import Role, Scope

role = Role(id="r1", name="weather-reader", composite=False)
scope = Scope(id="s1", name="read")

rule = PolicyRule(role=role, scope=scope)
agent_model = AgentPolicyModel(
    agent_id="weather-agent",
    agent_roles=[role],
    agent_scopes=[scope],
    source_roles={},
    subject_roles={"u1": [role]},        # keyed by subject id
    target_scopes={"github-tool": [scope]},  # target service id → scopes
    inbound_rules=[rule],
    outbound_rules=[],
    outbound_subject_rules=[],               # (user_role, tool_scope) pairs; defaults to []
)
model = PolicyModel(agents=[agent_model])
```

### Replaces

`aiac.pdp.library.models` is deprecated. All consumers must migrate their imports to `aiac.policy.model.models`.

---

## Testing Decisions

**Seam:** model instantiation and serialization — no HTTP boundary, no mocking required.

Key behaviors to assert:
- `PolicyRule` accepts typed `Role` and `Scope` objects; rejects plain `str` where `Role`/`Scope` is expected.
- `AgentPolicyModel` with string-ID keys in `source_roles`, `subject_roles`, and `target_scopes` round-trips through `model_dump(mode="json")` / `model_validate()` with the typed `Role` / `Scope` list values preserved.
- `target_scopes` maps a target service id to the list of `Scope` objects permitted on it (outbound direction is `target → scopes`, not `scope → targets`).
- `outbound_subject_rules` defaults to `[]` (constructors that omit it still validate) and round-trips through `model_dump(mode="json")` / `model_validate()` with its `(user_role, tool_scope)` `PolicyRule` values preserved.
- A relationship map keyed by a plain string serializes to a JSON object without a custom key serializer.
- `ConfigDict(extra='ignore')` causes unknown fields to be silently discarded on `model_validate()`.

---

## Out of Scope

- HTTP serialization logic — handled by `aiac.policy.store.library`, `aiac.policy.store.service`, and `aiac.pdp.policy.library`.
- IdP API integration — `Service`, `Role`, `Scope` shapes are owned by `aiac.idp.configuration.models`.
- Rule revocation semantics — TBD; no model changes required until the design is finalised.

---

## Further Notes

- Keying maps by string `id` sidesteps the previous reliance on id-only hashing of the IdP models: two records for the same Keycloak entity fetched at different times (with potentially different enrichment fields) collapse to the same string key regardless of those differences.
- `aiac/src/aiac/agent/policy/api.py` imports `PolicyRule` from `aiac.policy.model`. The `role_to_scopes` / `roles_to_scope` helpers in that file remain in place and are used by AIAC Agent sub-UC agents directly; they are not consumed by the PCE.
