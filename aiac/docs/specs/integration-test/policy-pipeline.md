# Integration Test: policy-pipeline — `test_policy_pipeline.py`

> **One spec among several.** This document specifies a **single** integration test.
> Integration-test specs live **one spec per test** under `docs/specs/integration-test/`
> (a sibling of `components/`), and the master PRD's *Integration test specifications* section
> ([../PRD.md](../PRD.md)) is the index of them. This is the **policy-pipeline** integration test —
> the full identity→policy pipeline — not the definition of integration testing in general, and not
> the only integration-test PRD.

## Location
`aiac/test/integration/test_policy_pipeline.py` — a pytest module marked `@pytest.mark.integration`.
It imports two shared modules: `aiac/test/integration/scenario.py` — the canonical `github-agent`
scenario as pure data (one of the role→access fact sources the *Further Notes* mandate — the pair-lists,
alongside the *Scenario* table and both `policy.md` variants) — and `aiac/test/integration/launcher.py`
— the shared `uvicorn` subprocess-lifecycle helpers. It also ships a new
`aiac/test/integration/probe.rego` — a small standalone Rego module used only as the outbound
verification query (see *[What it does](#what-it-does)*). The `5.2` launcher
`test/pdp/policy/generate_rego.py` was refactored onto the same `launcher.py` + `scenario.py` so the two
launchers cannot drift.

## Description

A `@pytest.mark.integration` test that drives the **whole identity→policy pipeline** —
**Keycloak → PRB → PCE → OPA Policy Writer** — end-to-end, then **asserts** the generated Rego decides
correctly by running the standalone `opa eval` binary as its verification oracle. The `.rego` files are
still left on disk per policy variant, so the test doubles as the eyeball workflow: running the test
*is* the eyeball. There is no separate standalone script.

The generated Rego is the **artifact under test** — the LLM/PCE that produced it might be wrong — so the
test never trusts it. Instead it feeds `opa eval` requests derived from the **scenario spec (the
intended policy)** and asserts the real Rego admits/denies each one as the scenario truth table
requires. A mismatch fails the test and names the exact cell.

This is the same *flavor* as the PDP Policy Writer launcher
([pdp-policy-writer.md](pdp-policy-writer.md), issue `testing/5.2-pdp-writer-integration-test.md`) but
**broader**: where `5.2` hand-builds a `PolicyModel` in Python and POSTs it to the OPA stub —
deliberately bypassing Keycloak, the PRB, and the PCE — this test provisions a **live Keycloak** realm,
calls the real **Policy Rules Builder (PRB)** to map roles→scopes with a real LLM, then calls the real
**Policy Computation Engine (PCE)** to build the `PolicyModel` and drive the **OPA Policy Writer** to
emit Rego. Nothing is mocked; the only shortcut is that the OPA target is the filesystem stub
([../components/pdp-policy-writer-opa.md](../components/pdp-policy-writer-opa.md) §1.14) rather than the
Kubernetes-CR implementation, so the output is `.rego` files instead of a patched
`AuthorizationPolicy` CR — identical to `5.2`.

Because it needs a live Keycloak and a real LLM, it is `@pytest.mark.integration` and stays out of the
default unit-test run (`-m "not integration"`); it additionally `pytest.skip`s when no `opa` binary is
found.

### What it does

The pipeline (provision → PRB → PCE → OPA) is driven **once per `policy.md` variant** — `explicit` and
`abstract` — each writing into its own `rego_out/<variant>/` directory. `opa eval` then asserts the
scenario truth table against **each** variant's Rego (step 7). Steps 1–6 below describe one such run.

1. **Set service URLs in env before importing the aiac libraries.** Export `AIAC_PDP_CONFIG_URL`,
   `AIAC_POLICY_STORE_URL`, `AIAC_PDP_POLICY_URL`, and `AIAC_REALM` *before* importing the aiac
   libraries — the libraries read env at import time. This is the pattern
   `test/pdp/policy/generate_rego.py` already follows.
2. **Spawn the three services as `uvicorn` subprocesses** (no Docker) and poll each `GET /health`
   until ready, with a bounded timeout:
   - IdP Configuration Service — `aiac.idp.service.configuration.keycloak.main:app` on `7071`.
   - Policy Store — its ASGI app on `7074`, with `AGENTPOLICY_DB_PATH` pointed at a fresh temp dir.
   - OPA Policy Writer — `aiac.pdp.service.policy.opa.main:app` on `7072`, with `REGO_OUTPUT_DIR`
     (pointed at the current variant's `rego_out/<variant>/`) and the Policy Store DB path in its env.
3. **Provision Keycloak** (idempotent — delete-if-exists the realm first, then create):
   - via **`python-keycloak` `KeycloakAdmin`** (test fixture): create realm `AIAC_TEST_REALM`; create
     users `dev-user`, `test-user`, and `devops-user`; create realm roles `developer`, `tester`, and
     `devops`; assign roles to users (`devops-user`→`devops`, which maps to **no** agent/tool scope —
     the inbound deny case, see *[Scenario](#scenario)*); create the `github-agent` and `github-tool`
     clients with the descriptions in
     *[Scenario inputs](#scenario-inputs-prb-functional-inputs)* and with the `client.type`
     client attribute set to the plain string `"Agent"` / `"Tool"` respectively, so `Service` type
     resolution tags them from the attribute (not from description prose). Set the type via the product
     surface `Configuration.set_service_type(service, type)` (`POST /services/{id}/type`) or by writing
     the `client.type` attribute directly at client create. The attribute value is a plain string,
     **not** a list — a list fails the `in ("Agent","Tool")` check, resolves the type to `None`, and
     yields empty pipeline output.
   - via the **aiac IdP `Configuration` library** (the real product surface the PCE reads back): create
     the client roles (`source-operator`, `issues-operator`) and scopes (`source-access`, `issues-access`,
     `source-read`, `source-write`, `issues-read`, `issues-write`) with the descriptions in
     *[Scenario inputs](#scenario-inputs-prb-functional-inputs)*, and map roles→services and
     scopes→services so `get_services_by_role` / `get_services_by_scope` and `get_service().roles` /
     `.scopes` resolve correctly.
4. **Read-back type guard** — after provisioning, call `Configuration.get_service` for both clients and
   assert each resolved `.type` (`github-agent` ⇒ `Agent`, `github-tool` ⇒ `Tool`) **before** spawning
   the pipeline; abort with a clear message otherwise. This is a provisioning sanity check on the
   `client.type` attribute, distinct from the step-7 Rego-decision assertions.
5. **Proto-UC1 orchestration** — run the three PRB mappings against a pinned LLM (`temperature=0`) and
   concatenate the results into one `list[PolicyRule]`:
   - **(a)** `build_scope_rules(user_roles, agent_scope)` per agent scope → user→agent-scope rules.
   - **(b)** `build_scope_rules(user_roles, tool_scope)` per tool scope → user→tool-scope rules.
   - **(c)** `build_role_rules(agent_role, tool_scopes)` per agent role → agent-role→tool-scope rules.

   Concatenate into a single `list[PolicyRule]` and call
   `aiac.policy.computation.engine.compute_and_apply(rules, override=False)` against a **fresh** Policy
   Store. The PCE resolves the IdP relationships, builds the `github-agent` model (with `agent_roles` /
   `agent_scopes`; mapping (b) routed into `outbound_subject_rules`; and **no** `github-tool` model),
   writes it to the store, and pushes it to the OPA stub.
6. **Terminate the three subprocesses in `finally`.** The realm and the `.rego` files are left in
   place for eyeballing.
7. **Assert the truth table with `opa eval`.** Once both variants' Rego is on disk, evaluate a matrix of
   **(request JSON, rego file)** tuples with the standalone `opa` binary and hard-assert each decision
   against the scenario truth table (see *[Expected output](#expected-output)*):
   - **`opa` discovery** — `$OPA_BIN` → else `shutil.which("opa")` → else `pytest.skip("opa not
     found")`. Missing `opa` skips (does not fail) the suite.
   - **Inbound** — one node per `(variant × subject)`. Request `{"subject": <id>}` (source omitted, so
     the generated `source_ok` passes) is evaluated against the real
     `data.authz.github_agent.inbound.allow`. Coarse "can this user reach the agent at all" — there is
     no intent field.
   - **Outbound** — one node per `(variant × subject × function_name)`, where `function_name` is the
     agent's operation (a tool scope). Because the generated `allow` / `subject_ok` are existential and
     ignore any scope, the outbound decision is evaluated by a **probe query**,
     `data.probe.outbound.allow` (defined in `test/integration/probe.rego`), which binds
     `input.function_name` against the generated data maps and requires **both** the user→tool gate and
     the agent→tool gate to admit the function. Request shape `{"subject", "target", "function_name"}`.
   - **Soft match** `function_name`↔scope — the probe compares names by splitting **both** on `[._-]+`,
     lowercasing, and comparing as **sets** (token-set equality): `source.read`, `read_source`, and
     `Source-Read` all match `source-read`; bare `source` matches nothing.
   - The expected verdict for every cell is **computed from** the scenario pair-lists
     (`INBOUND_PAIRS` / `OUTBOUND_SUBJECT_PAIRS` / `OUTBOUND_PAIRS` in `scenario.py`), not from a second
     hand-maintained copy — a wrong LLM/PCE mapping therefore fails the test. A failing node names the
     exact `variant / subject / function_name` cell.
8. **Assert grant-set equivalence (semantic, beyond the decision oracle).** The `opa eval` matrix in
   step 7 is deliberately coarse: inbound `allow` only checks "reaches *some* agent scope," and the
   agent→tool gate covers all four scopes so only the user gate discriminates — so a **verdict-neutral**
   mapping error (a missing or spurious `(role, scope)` grant) passes step 7 unseen. To close that gap
   the test also captures the PRB's `list[PolicyRule]` per variant and asserts, as order-independent
   `(role, scope)` **sets** per gate, that **each variant equals the `scenario.py` truth table** and
   **the two variants equal each other**. This compares grant *sets*, not Rego text (formatting/ordering
   may differ; the grant set may not). This is what enforces the *both variants reproduce the same Rego*
   intent stated in *Further Notes*.

## Expected output

The test passes when `opa eval` decides every cell of the scenario truth table as follows, for **both**
policy variants. Verdicts are **computed from** the `scenario.py` pair-lists (`INBOUND_PAIRS` /
`OUTBOUND_SUBJECT_PAIRS` / `OUTBOUND_PAIRS`), not a hand-maintained copy — this table is the human-
readable rendering of them.

`USERS`: `dev-user`→`developer`, `test-user`→`tester`, `devops-user`→`devops`.

**Inbound allow** (`data.authz.github_agent.inbound.allow`, from `INBOUND_PAIRS`, user-role→agent-scope):

| Subject | Inbound |
|---|---|
| dev-user | ✅ |
| test-user | ✅ |
| devops-user | ❌ |

**Outbound allow(subject, function)** (`data.probe.outbound.allow`, from `OUTBOUND_SUBJECT_PAIRS`
user→tool; the agent→tool gate covers all four scopes, so the user gate discriminates):

| | source-read | source-write | issues-read | issues-write |
|---|---|---|---|---|
| dev-user | ✅ | ✅ | ✅ | ❌ |
| test-user | ❌ | ❌ | ✅ | ✅ |
| devops-user | ❌ | ❌ | ❌ | ❌ |

Alongside the assertions, each variant leaves exactly **two** files on disk in its
`rego_out/<variant>/` for eyeballing:

- `github_agent.inbound.rego` — package `authz.github_agent.inbound`; the **user→agent** gate.
  `subject_roles` = `{dev-user: [developer], test-user: [tester]}`; `agent_scopes` populated.
  (`devops-user` holds `devops`, which maps to no agent scope, so it is absent from `subject_roles` and
  denied inbound.)
- `github_agent.outbound.rego` — package `authz.github_agent.outbound`; `allow if { subject_ok;
  target_ok }`. Its **`subject_ok`** is the new **user→tool** gate (mapping (b), grouped from
  `outbound_subject_rules` into `outbound_subject_role_scopes`, matched against
  `target_scopes[input.target]`); its **`target_ok`** is the **agent→tool** gate (mapping (c), over
  `agent_roles` × `agent_role_scopes`). `agent_roles` and `target_scopes` are populated.

Explicitly **no** `github_tool.*.rego` — the pipeline emits no tool model. Eyeball both files against
the **ID-only** package shapes in
[../components/pdp-policy-writer-opa.md](../components/pdp-policy-writer-opa.md)
(§ *Rego package structure*), the same source of truth `5.2` uses.

## Scenario

A single agent + tool + three users, fixed so the generated Rego is reproducible and reviewable by
inspection. This is the same canonical `github-agent` worked example as `5.2`, driven end to end
through the real pipeline rather than a hand-built `PolicyModel`, plus a third `devops-user` that
exercises the deny-by-default path.

| Element | Value |
|---------|-------|
| Realm | `AIAC_TEST_REALM` (default `aiac-e2e`) |
| Agent | `github-agent` (client roles `source-operator`, `issues-operator`; scopes `source-access`, `issues-access`) |
| Tool | `github-tool` (scopes `source-read`, `source-write`, `issues-read`, `issues-write`) |
| Users | `dev-user` (role `developer`), `test-user` (role `tester`), `devops-user` (role `devops`) |
| `developer` | source read/write + issues read |
| `tester` | issues read/write |
| `devops` | no access (inbound deny; denied every outbound function) |

Role → access (confirmed with the user; the fixed facts that both `policy.md` versions below and the
`scenario.py` pair-lists must agree with — the generic descriptions are not part of this triad):

- `developer` — source read/write, issues read.
- `tester` — issues read/write.
- `devops` — no access. Conveyed by the **role description only** — it is absent from every pair-list
  and both `policy.md` variants are **unchanged** (deny-by-default), so it is denied inbound and on
  every outbound function.

## Configuration (env)

| Variable | Purpose | Default |
|----------|---------|---------|
| `KEYCLOAK_URL` | External Keycloak base URL | — (required) |
| `KEYCLOAK_ADMIN_REALM` | Realm the admin creds live in | `master` |
| `KEYCLOAK_ADMIN_USERNAME` / `KEYCLOAK_ADMIN_PASSWORD` | Keycloak admin creds | — (required) |
| `AIAC_TEST_REALM` | Realm the test provisions | `aiac-e2e` |
| `AIAC_REALM` | Realm the PCE reads back (= `AIAC_TEST_REALM`) | `aiac-e2e` |
| `AIAC_PDP_CONFIG_URL` | IdP Configuration Service base URL (set before import) | `http://127.0.0.1:7071` |
| `AIAC_POLICY_STORE_URL` | Policy Store base URL (set before import) | `http://127.0.0.1:7074` |
| `AIAC_PDP_POLICY_URL` | OPA Policy Writer base URL (set before import) | `http://127.0.0.1:7072` |
| `REGO_OUTPUT_DIR` | Base dir the OPA stub subprocess writes `.rego` to; the test points it at `rego_out/<variant>/` per variant and leaves the files on disk | operator-chosen local dir |
| `AGENTPOLICY_DB_PATH` | Policy Store DB path for the subprocess (fresh temp dir) | temp |
| `AIAC_POLICY_FILE` | PRB whole-file policy — path to the `policy.md` variant fed to the PRB; the test sets it per variant (`policy.explicit.md`, `policy.abstract.md`) | `/etc/aiac/policy.md` |
| `LLM_BASE_URL` / `LLM_MODEL` / `LLM_API_KEY` | PRB LLM (pinned `temperature=0`) | — (required) |
| `OPA_BIN` | Path to the standalone `opa` binary used as the verification oracle; else `PATH` (`shutil.which`), else the test `pytest.skip`s | — (optional; PATH lookup) |

> When the test is written, confirm the Policy Store's ASGI import path and its DB-path env-var
> name against the Policy Store component spec / issue — `AGENTPOLICY_DB_PATH` is the placeholder used
> here; use the real one. `AIAC_POLICY_FILE` selects which `policy.md` variant (see
> *[Scenario inputs](#scenario-inputs-prb-functional-inputs)*) the PRB reads.

## Runbook

Runnable only once the pipeline fixes (handoffs 01 + 02, P1–P5) have landed, and requires a live
Keycloak, a real LLM, and an `opa` binary on `PATH` (or `$OPA_BIN`).

```bash
# env: KEYCLOAK_URL + admin creds + LLM_* set; realm defaults to aiac-e2e; opa on PATH or $OPA_BIN
.venv/bin/pytest test/integration/test_policy_pipeline.py -m integration -v
# ~30 parametrized nodes (variant × subject inbound; variant × subject × function_name outbound).
# A failing node names the exact cell, e.g.:
#   test_outbound[abstract-test-user-source-read] — expected deny, opa allowed
# The generated Rego is left on disk per variant for eyeballing:
#   rego_out/explicit/github_agent.{inbound,outbound}.rego
#   rego_out/abstract/github_agent.{inbound,outbound}.rego
#   (no github_tool.*.rego in either)
```

The suite `pytest.skip`s when no `opa` binary is found (`$OPA_BIN` → `PATH`). Eyeball the persisted
Rego against the adjusted package shapes in
[../components/pdp-policy-writer-opa.md](../components/pdp-policy-writer-opa.md); optionally inspect the
Policy Store DB and the provisioned Keycloak realm.

## Testing Decisions

- **Highest seam available, verified by a real oracle.** Real libraries + real services + real Keycloak
  + real LLM. The test drives the pipeline through its real surfaces — the IdP `Configuration` library,
  the PRB entry points (`build_scope_rules` / `build_role_rules`), and the PCE's `compute_and_apply` —
  and then verifies the real filesystem output with the standalone **`opa eval`** binary. The only
  shortcut is the OPA filesystem stub (same as `5.2`). A good test here asserts only **external
  behavior** — the policy *decisions* the generated Rego makes for scenario-derived requests — never the
  internal Rego structure (which the OPA Policy Writer's own unit tests own).
- **Rego is the artifact under test; the scenario is the oracle.** The LLM/PCE that produced the Rego
  might be wrong, so the expected verdicts are **computed from** the scenario pair-lists
  (`INBOUND_PAIRS` / `OUTBOUND_SUBJECT_PAIRS` / `OUTBOUND_PAIRS`), not from a second hand-maintained
  copy or from the Rego itself. A wrong role→scope mapping therefore fails the test at the exact cell.
- **Outbound needs a probe.** The generated `allow` / `subject_ok` are existential and ignore any
  scope, so a raw query cannot answer "may this subject invoke *this* function." A small
  `test/integration/probe.rego` (`data.probe.outbound.allow`) binds `input.function_name` against the
  generated data maps and requires **both** the user→tool and agent→tool gates to admit it. Names are
  compared by **token-set equality** (split on `[._-]+`, lowercased) so `source.read` / `read_source` /
  `Source-Read` all match `source-read` while bare `source` matches nothing.
- **Attribute-based client typing + read-back guard.** Clients are typed by the `client.type`
  attribute (plain string `"Agent"` / `"Tool"`), provisioned by the test — not by description keywords.
  Because that attribute drives whether the PCE emits an agent model (and suppresses the tool model),
  the test reads each service back via `Configuration.get_service` and asserts its `.type` before
  running the pipeline, aborting on mismatch. This is a **provisioning** sanity check, distinct from the
  Rego-decision assertions.
- **Self-contained subprocess lifecycle.** The test spawns IdP (7071), Policy Store (7074), and OPA
  (7072) as `uvicorn` subprocesses, polls each `GET /health` before use, and tears them all down in
  `finally`. Keycloak and the LLM are **external** (reached via env); `opa` is an external binary.
- **LLM nondeterminism, contained.** The PRB LLM is pinned to `temperature=0`, and the **explicit**
  `policy.md` variant states each `(role, scope)` grant outright, so its mapping is stable. The
  **abstract** variant leans on the LLM to expand prose + descriptions into concrete scopes; both
  variants are asserted not only cell-by-cell via `opa eval` (step 7) but at the **grant-set** level
  (step 8) — each variant's `(role, scope)` set must equal the truth table *and* the other variant's.
  Grant-set equivalence catches the verdict-neutral under/over-grants the decision oracle hides. Some
  model-dependence remains, which is why the suite is `@pytest.mark.integration`, out of default CI.
- **Prior art, shared not copied.** `test/pdp/policy/generate_rego.py` (the `5.2` launcher) established
  the shape this test reuses — `uvicorn` subprocess spawn, `GET /health` poll, env-before-import
  ordering, and `finally` teardown. Rather than duplicate it, that machinery lives in the shared
  `test/integration/launcher.py`, and the fixed scenario lives in `test/integration/scenario.py`;
  `generate_rego.py` was refactored onto both (its `.rego` output verified byte-identical to before the
  refactor). The live-Keycloak pytest suite (`testing/5.1-integration-tests.md`) is the sibling
  marker-gated, decision-asserting counterpart for the read-side services and is the prior art for the
  `@pytest.mark.integration` + `opa eval` shape.

## Relationship to other integration tests

This is **one** integration-test spec among several indexed by the master PRD
([../PRD.md](../PRD.md), § *Integration test specifications*).

- Same flavor as the **live-Keycloak pytest integration tests** (`testing/5.1-integration-tests.md`) —
  both are `@pytest.mark.integration`, run outside the default unit run against live dependencies, and
  assert on decisions. This test additionally uses `opa eval` as its oracle and skips when `opa` is
  absent.
- **Broader than** the OPA-stub-only **PDP Policy Writer** launcher
  ([pdp-policy-writer.md](pdp-policy-writer.md), `testing/5.2-pdp-writer-integration-test.md`): `5.2`
  hand-builds a `PolicyModel`, exercises only OPA, and is still a write-only eyeball launcher; this test
  adds Keycloak provisioning + PRB + PCE in front of the **same** OPA stub and **asserts** the resulting
  decisions with `opa eval`. Both still leave `.rego` on disk against the same package shapes.

Tracking issue for this test: `testing/5.3-policy-pipeline-integration-test.md`.

## Out of Scope

- **Writing `test_policy_pipeline.py`, `probe.rego`, or any P1–P5 pipeline code** — this spec
  *describes* the test; the test itself is written in a later session against the fixed pipeline
  (tracked by `testing/5.3-policy-pipeline-integration-test.md` and the prerequisite issues).
- **The Rego generator, the canonical policy model, the PRB, and the PCE implementations** — specified
  and unit-tested by their own components ([../components/pdp-policy-writer-opa.md](../components/pdp-policy-writer-opa.md),
  [../components/policy-model.md](../components/policy-model.md),
  [../components/policy-computation-engine.md](../components/policy-computation-engine.md), and the PRB
  component spec), not here. In particular, the internal **structure** of the generated Rego is the
  Policy Writer's concern; this test asserts only the **decisions** that Rego makes.
- **The Kubernetes-CR Policy Writer (1.13)** — this test targets the filesystem **stub** (1.14) only.
- **Default-CI wiring** — the test is `@pytest.mark.integration` and requires live Keycloak + LLM + an
  `opa` binary, so it runs on demand, not in the default `-m "not integration"` unit run.

## Further Notes

- The scenario is deliberately fixed. The role→access facts are owned by **three** artefacts that must
  agree: the *Scenario* table, **both** `policy.md` versions in *Scenario inputs*, and the
  `scenario.py` pair-lists (`INBOUND_PAIRS` / `OUTBOUND_SUBJECT_PAIRS` / `OUTBOUND_PAIRS`). The
  entity/role/scope **descriptions no longer encode those facts** — they are generic and functional and
  drop out of the fact triad; they must stay generic and simply not contradict the facts. If the
  role→access facts change, update the *Scenario* table, both `policy.md` variants, and the pair-lists
  together so the eyeballed output stays reviewable.
- The least-privilege **deny-by-default** directive is supplied by the PRB prompt itself
  (`_GRANT_ACCESS` in `agent/policy_rules_builder/prompts.py`), which prepends it — followed by the
  bundled generic baseline policy (`generic_policy.md`) — ahead of the scenario `policy.md` on every
  call, so every policy decision gets it regardless of which variant is read. The **explicit** variant
  still spells the directive out (its whole point is to state everything outright); the **abstract**
  variant relies on the prompt and does not restate it — do not re-add it to the abstract variant.
- Two `policy.md` variants are shipped on purpose (see *Scenario inputs*): an **explicit** one and an
  **abstract** one. `AIAC_POLICY_FILE` selects which the PRB reads, so a reviewer can compare the PRB's
  output on explicit vs. abstract policy text against the same expected Rego. The abstract variant
  carries **no** agent-capability bullet; it relies on the elaborated `source-operator` /
  `issues-operator` role descriptions (provisioned into Keycloak) for mapping (c), so it survives
  deny-by-default and both variants reproduce the same Rego.
- Descriptions are ≤255 characters and written **verbatim** into Keycloak; there is no shortened /
  verbatim split. (Keycloak caps role and client descriptions at 255 chars, and the generic descriptions
  are authored to stay within that cap.)
- The `devops` role's **zero access** is conveyed by its **role description only**. It is absent from
  every pair-list (`INBOUND_PAIRS` / `OUTBOUND_SUBJECT_PAIRS` / `OUTBOUND_PAIRS`) and both `policy.md`
  variants are **unchanged**, so deny-by-default alone denies it inbound and on every outbound function —
  which is precisely what the truth table's `devops-user` row asserts. Because `devops-user` lives in
  the shared `scenario.py`, it also appears in the `5.2` launcher's eyeball output (denied everywhere);
  that is intentional and keeps the two launchers consistent.

## Blocked-by

The pipeline can only produce correct output once handoffs 01 (P1, P3) and 02 (P2, P4, P5) land; those
are **resolved**, so this test is ready to be written. Component prerequisites:

- PRB — `agent/3.20-policy-rules-builder.md`
- PCE — `policy/pce/8.10-policy-computation-engine.md`
- Policy model — `policy/model/8.1-policy-model.md`
- OPA filesystem stub — `pdp-policy-writer/1.14-pdp-policy-writer-opa-stub.md`
- Rego package generator — `pdp-policy-writer/1.10-rego-package-generator.md`
- pdp-policy library — `library/pdp/8.9-pdp-policy-library-rename.md`
- Policy Store library / service — `policy/store/8.7-policy-store-library.md` /
  `policy/store/8.5-policy-store-service.md`

## Scenario inputs (PRB functional inputs)

These are **functional** inputs — the LLM reads the entity/role/scope descriptions and the `policy.md`
to produce the role→scope mappings, so they are part of the fixed scenario, not decoration. Confirmed
with the user; keep them in sync with the *Scenario* table (see *Further Notes*).

### Entity descriptions

The descriptions are **generic and keyword-free** — they describe what each entity/role/scope *does*,
carry no policy grant ("Resolves to…") and no owning-client naming, and stay within Keycloak's 255-char
cap so they are written verbatim (no shortened renderings). Client `type` is **not** inferred from
description prose: the test provisions each client's `client.type` attribute (the type UC1
discovers from the agent card / `kagenti.io/type` label) as a plain string `"Agent"` / `"Tool"`, so
`Service` type resolution ([../../../src/aiac/idp/configuration/models.py:79-87](../../../src/aiac/idp/configuration/models.py#L79-L87))
tags each client from the attribute without touching the TEMP description-keyword fallback.

**`github-agent`** — client (Agent):
> Autonomous Agent acting on a user's behalf against source repositories and an issue tracker. It
> inspects and changes repository source contents and reads, creates, and updates issues and their
> threads.

**`github-tool`** — client (Tool):
> Capability provider Tool for source repositories and an issue tracker. It performs read and write
> operations on repository source contents and on issues and their comment threads.

**`developer`** — realm role (user):
> Developer — an engineering user who develops the source codebase (writing and maintaining code) and
> fixes code defects reported in the issue tracker; works primarily in source and consults issues for
> defect reports.

**`tester`** — realm role (user):
> Tester — a quality-assurance user who verifies software quality and tracks defects through the issue
> tracker: filing, triaging, and updating issue reports; works in the issue tracker, not in source.

**`devops`** — realm role (user):
> DevOps — an operations user who manages deployment infrastructure and runtime environments; does not
> author source code and does not manage the issue tracker.

> The `devops` description is deliberately **unrelated** to source and issue work, so the PRB derives no
> agent or tool scope for it and deny-by-default leaves `devops-user` denied everywhere — the inbound
> deny case. It is added to the realm-role set only; the pair-lists and both `policy.md` variants stay
> unchanged (see *Further Notes*).

### Role & scope descriptions

**Client roles (agent):**

- `source-operator` — Covers read and write access to source repository contents — listing, reading,
  creating, and modifying files.
- `issues-operator` — Covers read and write access to the issue tracker — reading, filing, updating,
  and commenting on issues and their threads.

**Agent scopes:**

- `source-access` — Scope granting use of a source-code capability — invoking source-code functions such
  as reading and changing repository contents.
- `issues-access` — Scope granting use of an issue-management capability — invoking issue functions such
  as reading and updating issues.

**Tool scopes:**

- `source-read` — Read source repository contents: file listings and file bodies. Read-only.
- `source-write` — Create, modify, or delete source repository contents; commit file changes.
- `issues-read` — Read issues and their comment threads. Read-only.
- `issues-write` — Create and update issues: open, edit, comment, and close.

### `policy.md` — Version 1 (explicit)

Each granted `(role, scope)` pair is spelled out; the three sections map 1:1 to PRB mappings (a)/(b)/(c)
and to the expected Rego gates.

```markdown
# Access Control Policy — github-agent / github-tool

Grant access on a least-privilege basis. Only grant a (role, scope) pair when this
policy supports it; deny by default.

## Users → agent capabilities (inbound; user may call the agent)
- developer may use source-access and issues-access.
- tester may use issues-access.

## Users → tool operations (outbound subject; user may reach the tool)
- developer may perform source-read, source-write, and issues-read.
- tester may perform issues-read and issues-write.

## Agent roles → tool operations (outbound target; agent may reach the tool)
- source-operator may perform source-read and source-write.
- issues-operator may perform issues-read and issues-write.
```

### `policy.md` — Version 2 (abstract)

Relies on the PRB / LLM to expand "read and modify source" into the concrete scopes. Encodes the same
role→access facts as Version 1. It carries **no** agent-capability bullet; mapping (c)
(agent-role→tool-scope) is instead derived from the elaborated `source-operator` / `issues-operator`
role descriptions (see *Role & scope descriptions*), so it survives the PRB's deny-by-default-on-silence
rule and both variants reproduce the same Rego.

```markdown
- Developers work primarily in source — writing and maintaining code — and consult the issue tracker only to follow defect reports; grant them full read and write access to source contents, and read-only access to issues.
- Testers work exclusively in the issue tracker — filing, triaging, and updating defect reports — and do not work in source; grant them full read and write access to issues, and no access to source.
```
