# Component PRD: AIAC Policy Store

## Problem Statement

The AIAC Agent's Policy Computation Engine and Policy Builder sub-agent produce and merge `AgentPolicyModel` objects representing the access control policy for each service. The PDP Policy Writer translates these into Rego packages and writes them to an `AuthorizationPolicy` Kubernetes CR — but this derived artifact cannot be reverse-engineered back into structured `AgentPolicyModel` data. Without a durable structured policy store:

- The Policy Computation Engine cannot read current policy state for additive merging — it must re-derive the full state from the PDP snapshot on every trigger.
- Off-boarded agents leave no structured record of their removal.
- Pod restarts lose any in-flight policy construction context.

## Solution

A dedicated **AIAC Policy Store** owns an in-memory `PolicyModel` cache backed by a SQLite database for durability. A companion library [`aiac.policy.store.library`](library-policy-store.md) exposes module-level typed functions matching the `aiac.pdp.policy.library` pattern, used by the Policy Computation Engine to read and write policy state without any storage-layer boilerplate.

The PDP Policy Writer retains sole ownership of the `AuthorizationPolicy` CR (Rego packages) and has no dependency on the Policy Store. The two persistence artifacts serve distinct purposes and are owned by distinct services:

| Artifact | Owner | Contents |
|---|---|---|
| SQLite `agent_policies` table | Policy Store | Structured `AgentPolicyModel` — source of truth (cache-first, write-through) |
| `AuthorizationPolicy` CR (one total) | PDP Policy Writer | Derived Rego packages — OPA runtime artifact |

---

## User Stories

1. As the Policy Computation Engine, I want to read the current `AgentPolicyModel` for a specific agent, so that I can additively append new rules without overwriting existing ones.
2. As the Policy Computation Engine, I want to read the full `PolicyModel` (all agents), so that I can execute a whole-system policy rebuild.
3. As the Policy Computation Engine, I want to write an `AgentPolicyModel` to persistent storage, so that the current policy state survives pod restarts.
4. As the Policy Computation Engine, I want to delete a specific agent's policy on off-boarding, so that decommissioned services are removed from the structured policy store.
5. As the Policy Computation Engine, I want to clear all agent policies in a single call, so that a full policy rebuild can start from a clean state.
6. As a consumer of the Policy Store library, I want a typed Python library that returns `AgentPolicyModel` and `PolicyModel` objects directly, so that I can work with structured policy data without writing storage client code.
7. As an operator, I want the Policy Store deployed as its own single-replica StatefulSet with a dedicated PVC, so that its storage and restart lifecycle is decoupled from the stateless policy services.

---

## Implementation Decisions

### Policy Store Service

**Location:** `aiac/src/aiac/policy/store/service/`

**Port:** `0.0.0.0:7074`

**ClusterIP Service:** `aiac-policy-store-service:7074`

**Deployment:** dedicated single-replica `StatefulSet` `aiac-policy-store`, with a `volumeClaimTemplate` PVC (1 Gi, `ReadWriteOnce`, cluster-default StorageClass) mounted at `/data`. Fronted by a headless Service for stable pod DNS plus the `aiac-policy-store-service:7074` ClusterIP for clients. Not co-located with IdP Configuration / PDP Policy Writer.

**Framework:** FastAPI + uvicorn. **Base image:** `python:3.12-slim`.

**Storage backend:** SQLite via `sqlite3` stdlib (zero extra dependency — `sqlite3` ships with the Python standard library). Database file: `AGENTPOLICY_DB_PATH` (default `/data/policy_model.db`).

**Schema:**

```sql
CREATE TABLE IF NOT EXISTS agent_policies (
    agent_id TEXT PRIMARY KEY,
    spec     TEXT NOT NULL   -- AgentPolicyModel.model_dump() as JSON
);
```

**In-memory cache:** the service owns a full `PolicyModel` in memory as the authoritative serving layer:
- All `GET` requests served from memory (storage never queried at runtime).
- Every mutation writes through to SQLite synchronously before returning `204`.
- On pod restart: load all rows from SQLite → populate cache → begin serving.

**Transaction strategy:**
- Full rebuild (`POST /policy`): `BEGIN` → `DELETE FROM agent_policies` → per-agent `INSERT` → `COMMIT`. Crash before `COMMIT` leaves the prior state intact (SQLite WAL rollback).
- Per-agent upsert (`POST /policy/agents/{id}`): `INSERT OR REPLACE INTO agent_policies VALUES (?, ?)`.

**Future normalization:** migrate to `agent_policies` + `policy_rules(agent_id, role, scope)` tables once `AgentPolicyModel`/`PolicyRule` schema stabilizes — a future observability UI will need queryable columns. JSON column in the current schema avoids migration churn during active development.

**Endpoints:**

| Method | Path | Body | Returns |
|---|---|---|---|
| `GET` | `/policy` | — | `PolicyModel` (all agents) |
| `GET` | `/policy/agents/{agent_id}` | — | `AgentPolicyModel` |
| `POST` | `/policy` | `PolicyModel` | `204 No Content` |
| `POST` | `/policy/agents/{agent_id}` | `AgentPolicyModel` | `204 No Content` |
| `DELETE` | `/policy/agents/{agent_id}` | — | `204 No Content` |
| `DELETE` | `/policy` | — | `204 No Content` |
| `GET` | `/health` | — | `200` / `503` |

**Error responses:**
- `404 Not Found` with `{"error": "agent {id} not found"}` when `GET /policy/agents/{agent_id}` finds no entry in cache.
- `502 Bad Gateway` with `{"error": "..."}` on SQLite write error for all write endpoints.
- `503 Service Unavailable` if `GET /health` cannot open or query the SQLite file.

**`main.py` functions:**

- `_get_db() -> sqlite3.Connection` — open `AGENTPOLICY_DB_PATH` with `check_same_thread=False`; run `CREATE TABLE IF NOT EXISTS` on first open.
- `_upsert_agent(agent_id: str, model: AgentPolicyModel)` — `INSERT OR REPLACE INTO agent_policies VALUES (?, ?)` with `model.model_dump_json()`.
- `_get_agent(agent_id: str) -> AgentPolicyModel` — read from in-memory cache; raise `404` if absent.
- `_list_all() -> PolicyModel` — return in-memory cache directly.
- `_delete_agent(agent_id: str)` — `DELETE FROM agent_policies WHERE agent_id = ?`; remove from cache.
- `_delete_all()` — `DELETE FROM agent_policies`; replace cache with empty `PolicyModel`.
- `_rebuild(model: PolicyModel)` — `BEGIN` → `DELETE FROM agent_policies` → per-agent `INSERT` → `COMMIT`; replace cache atomically.

**Configuration:**

| Variable | Source | Default |
|---|---|---|
| `AGENTPOLICY_DB_PATH` | ConfigMap (`aiac-policy-store-config`) | `/data/policy_model.db` |

**Dependencies:** `fastapi`, `uvicorn[standard]`, `pydantic`. `sqlite3` is stdlib (no new dependency).

**Imports:** `from aiac.policy.model import PolicyModel, AgentPolicyModel`

**File structure:**

```
aiac/src/aiac/policy/store/service/
├── __init__.py
├── Dockerfile
├── requirements.txt
└── main.py
```

Build command (run from repo root):
```bash
docker build -f aiac/src/aiac/policy/store/service/Dockerfile \
  -t aiac-policy-store:latest aiac/src/
```

---

## Testing Decisions

Good tests assert external behavior at the system boundary — not internal implementation details such as private helpers or field serialization choices.

### Policy Store Service

**Seam:** SQLite `:memory:` database — pass an in-memory connection to the service on startup instead of opening `AGENTPOLICY_DB_PATH`. All behavioral assertions remain valid; only the storage seam changes.

Key behaviors to assert:
- `GET /policy/agents/{id}`: returns `AgentPolicyModel` deserialized from cache; `404` when agent not in cache.
- `GET /policy`: returns `PolicyModel` for all agents; empty `PolicyModel(agents=[])` when cache is empty.
- `POST /policy/agents/{id}`: `spec` stored in SQLite; cache updated; `204` returned.
- `POST /policy` (rebuild): SQLite `DELETE` + per-agent `INSERT` issued inside one transaction; cache replaced atomically; `204` returned.
- `DELETE /policy/agents/{id}`: row removed from SQLite; removed from cache; `204`.
- `DELETE /policy`: all rows removed from SQLite; cache empty; `204`.
- SQLite write error on any write endpoint → `502`.
- SQLite file cannot be opened/queried on `GET /health` → `503`.

See [library-policy-store.md](library-policy-store.md) for the companion library testing decisions.

---

## Out of Scope

- **Triggering Rego generation:** the Policy Store writes structured data only. Triggering Rego generation in the PDP Policy Writer is the responsibility of `aiac.pdp.policy.library` (called by `aiac.policy.computation`).
- **Pagination:** `GET /policy` returns all agents without pagination. At target scale (hundreds of agents), the full result fits within one SQLite query and one HTTP response.
- **In-cluster mTLS between Policy Computation Engine and Policy Store:** secured by Kubernetes network policy; no application-layer auth.
- **Multi-writer / replica scale-out:** the current design is single-writer (single-replica StatefulSet, RWO PVC, SQLite). Future migration to a shared DB (e.g. PostgreSQL) is a backend swap; the HTTP contract is unchanged.

---

## Further Notes

- The K8s manifests issue must create the `aiac-policy-store` StatefulSet, its `volumeClaimTemplate` PVC (1 Gi, `ReadWriteOnce`), and a headless Service. No CRD or RBAC is needed — the service does not touch the Kubernetes API.
- `spec` fields use snake_case (matching Pydantic's `model_dump()`) — consistent with the `AuthorizationPolicy` CR convention. The JSON column avoids a translation layer.
- `agent_id` is the SQLite `PRIMARY KEY`. The `aiac.apply.service.{id}` naming convention (lowercase alphanumeric + hyphens) should be maintained for consistency with trigger events.
- K8s resource names: StatefulSet `aiac-policy-store`, ClusterIP Service `aiac-policy-store-service:7074`, env var `AIAC_POLICY_STORE_URL`.
