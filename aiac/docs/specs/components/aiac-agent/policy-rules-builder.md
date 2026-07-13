# Sub-PRD: AIAC Agent — Policy Rules Builder

## Description

The **Policy Rules Builder** (PRB) is a shared module at `agent/policy_rules_builder/`. It
exposes two module-level functions that producing sub-agents call directly. Each function
internally runs a LangGraph `StateGraph`; callers are decoupled from LangGraph mechanics. The
PRB fetches its own policy context (see **Policy source** below), reasons over it with an LLM,
and emits `list[PolicyRule]` scoped to the input. It does **not** call
`aiac.pdp.policy.library` or `aiac.policy.store.library` directly; only the PCE does.

---

## Entry points

```python
def build_role_rules(role: Role, scopes: list[Scope]) -> list[PolicyRule]: ...
def build_scope_rules(roles: list[Role], scope: Scope) -> list[PolicyRule]: ...
```

**`build_role_rules`** — role-centric: "given this role, which scopes does it get?"
Used for UC3 (Role Update). Called once per role with the full set of scopes relevant to the trigger.

**`build_scope_rules`** — scope-centric: "given this scope, which roles may access it?"
Used as one of the calls for UC1 (Service Onboarding). See the Controller sub-PRD for the full UC1 dispatch pattern.

Each call handles exactly **one focal entity** (the singular argument) against a list of
candidate counterparts; the caller (UC handler) does all iteration.

---

## Policy source (two phases)

Policy context is fetched behind a `PolicySource` seam, so the retrieval mechanism can change
without touching the rest of the graph.

- **Phase 1 (current):** the entire access-control policy lives in a **single file**; the PRB
  reads the whole file into the proposer prompt. No ChromaDB, no domain-knowledge collection.
  Located via `AIAC_POLICY_FILE` (default `/etc/aiac/policy.md`), read as UTF-8; a
  missing/unreadable file raises.
- **Phase 2 (later issue):** policy **and** domain knowledge live in a ChromaDB vector store;
  the PRB does RAG retrieval over the `aiac-policies` and `aiac-domain-knowledge` collections
  (query text derived from the focal entity), respecting `CHROMA_N_RESULTS`. This swaps in a
  `ChromaPolicySource` at the same seam.

---

## Contract

| Aspect | Decision |
|---|---|
| Structure | LangGraph `StateGraph` — nodes `fetch → propose → precheck → audit → build`; `audit → propose` retry edge; two typed graphs (role / scope) sharing node helpers |
| Context retrieval | Two-phase via a `PolicySource` seam — Phase 1 whole-file read; Phase 2 ChromaDB RAG (both collections). See **Policy source** |
| Realm parameter | None — inputs are pre-resolved typed objects; the policy source is not realm-scoped |
| Trigger type in state | None — the function name encodes the direction; no routing field in state |
| Output shape | Proposer emits **names** (via `with_structured_output`); the PRB rebuilds `PolicyRule`s from the **typed inputs** filtered by name — never from LLM-produced fields |
| Dedup | PRB generates a full rule set; the PCE's additive merge handles dedup on write |
| LLM call pattern | **Propose → LLM auditor** (2 structured calls). Auditor rejection feeds its reason back into propose (bounded fix-and-retry, `MAX_AUDIT_RETRIES = 3`); raises on exhaustion |
| Empty result | An auditor-**approved** empty selection is a valid `[]` (deny-by-default). Empty proposals are still audited |
| Error contract | Raises on policy-source failure, LLM failure, or audit-budget exhaustion — no silent empty-list returns |

---

## Internal graph design

Both entry points compile the same node shape (two typed graphs sharing pure node helpers):

```
fetch ─► propose ─► precheck ─► audit ─┬─ approved ─► build ─► END
          ▲                            │
          └───────── retry ────────────┘   (audit feeds its reason back to propose)
                                        │
                    rejected & budget exhausted ─► RAISE (inside audit node)
```

- **fetch** — `PolicySource.fetch()` → `policy_text` (Phase 1: whole file).
- **propose** — proposer messages (policy + focal + candidates + any `audit_feedback`);
  `with_structured_output(Selection)` → selected names + reasoning.
- **precheck** — deterministic: keep only names present in the candidate set (drop hallucinated
  names; log drops). No LLM.
- **audit** — auditor messages; `with_structured_output(AuditVerdict)` → `{approved, reason}`.
  Approved → continue; rejected → feed the reason back and retry, or raise once
  `MAX_AUDIT_RETRIES` is exhausted. Empty proposals are audited too.
- **build** — reconstruct `PolicyRule`s from the typed inputs filtered by the approved names.

### Structured-output schemas

```python
class RoleSelection(BaseModel):     # build_role_rules (role focal, scope candidates)
    granted_scope_names: list[str]
    reasoning: str

class ScopeSelection(BaseModel):    # build_scope_rules (scope focal, role candidates)
    roles_with_access_names: list[str]
    reasoning: str

class AuditVerdict(BaseModel):
    approved: bool
    reason: str | None
```

The PRB rebuilds rules from the typed inputs, e.g.
`[PolicyRule(role=role, scope=s) for s in scopes if s.name in granted_scope_names]`.

### State fields

```python
class _PRBWorking(TypedDict):
    policy_text: str
    selected_names: list[str]
    reasoning: str
    approved: bool
    audit_feedback: str | None
    retry_count: int
    rules: list[PolicyRule]

class RoleRulesState(_PRBWorking):   # role: Role; scopes: list[Scope]
    ...
class ScopeRulesState(_PRBWorking):  # roles: list[Role]; scope: Scope
    ...
```

### Prompts

Lean — task framing, the structured-output contract, and two **safety** meta-rules
(**deny-by-default / policy-silence** — grant a pair only if the policy supports it — and
**scope-strictly-to-focal**). On top of those, two shared **mapping** rules (`_MAPPING_RULES`)
govern how evidence becomes a grant:

- **Capability projection** — a scope names a *set* of operations; any one covered operation
  established for a candidate (by the policy or by the focal/candidate descriptions) grants the
  whole scope, so partial (e.g. read-only) access still earns it.
- **Relationship scoping** — a policy may state several access relationships over the same
  entities; each grant is judged only by evidence about *that* candidate and the focal entity, and
  a statement about an entity that is neither the focal nor a candidate (even a same-theme one) is a
  different relationship that never counts either way.

No worked examples or domain heuristics; all substantive reasoning is deferred to the
(user-authored) policy content and the entity descriptions. The **proposer and auditor share the
same rule set** — both make the same grant decision, so a rule on only one side lets the two
diverge (they did: see issue 3.20 *Follow-up: cross-variant convergence*). The auditor adds only
its framing: approve only if every granted pair is policy-supported and nothing unsupported
slipped in.

### LLM + retries

`ChatOpenAI(base_url=LLM_BASE_URL, model=LLM_MODEL, api_key=LLM_API_KEY, temperature=0)`. Two
retry layers, kept distinct:

- **`MAX_AUDIT_RETRIES`** (module constant, default `3`) — the semantic fix-and-retry loop
  between audit and propose.
- **`UPSTREAM_MAX_RETRIES`** (env, default `3`) — tenacity (`stop_after_attempt`, exponential
  backoff, `reraise=True`) around each LLM call for transport failures. The Phase-1 file read
  does **not** retry; it raises directly.

---

## Use-case dispatch

| Use Case | Caller | Function(s) called |
|---|---|---|
| UC1 — Service Onboarding | Service Policy Builder sub-agent | `build_scope_rules(other_roles, scope)` per agent/tool scope + `build_role_rules(role, other_scopes)` per agent role (agent path only) |
| UC2 — Policy Update (Build) | Build sub-agent | TBD |
| UC3 — Role Update | Role sub-agent | `build_role_rules(role, all_scopes)` — one call |

---

## Configuration

| Variable | Used for | Phase |
|---|---|---|
| `AIAC_POLICY_FILE` | Path to the whole-file access policy (default `/etc/aiac/policy.md`) | 1 |
| `LLM_BASE_URL`, `LLM_MODEL`, `LLM_API_KEY` | LLM calls | 1 |
| `UPSTREAM_MAX_RETRIES` | Transport retry budget for LLM (and, in Phase 2, ChromaDB) calls (tenacity, default `3`) | 1 |
| `AIAC_CHROMADB_URL` | ChromaDB endpoint | 2 |
| `CHROMA_N_RESULTS` | Number of results per ChromaDB query (default `10`) | 2 |

`MAX_AUDIT_RETRIES` (default `3`) is a module constant, not an env var.
