# Integration Test: uc1-onboarding-pipeline — `test_uc1_onboarding_pipeline.py`

> **One spec among several.** This document specifies a **single** integration test. Integration-test
> specs live **one spec per test** under `docs/specs/integration-test/` (a sibling of
> `components/`), and the master PRD's *Integration test specifications* section ([../PRD.md](../PRD.md))
> is the index of them. This is the **uc1-onboarding-pipeline** integration test — the phase-1
> service-onboarding demo driven end-to-end through the **real UC-1 agent** against **really-deployed**
> demo workloads — not the definition of integration testing in general.

> **Relationship to `policy-pipeline`.** This test is the **discovery-driven sibling** of
> [policy-pipeline.md](policy-pipeline.md). The *scenario facts and the truth tables are identical* (same
> three users, same role→access facts, same inbound/outbound matrices). The **difference** is provenance:
> where `policy-pipeline` *hand-provisions* the agent/tool roles and scopes with clean fixed names and
> then calls the PRB mappings directly, this test *infers them via real UC-1 onboarding* of deployed
> workloads. That inference is what makes the generated Rego **semantically similar but not byte-identical**
> to `policy-pipeline` (see *[Semantic similarity, not byte-identity](#semantic-similarity-not-byte-identity)*).

## Location

`aiac/test/integration/test_uc1_onboarding_pipeline.py` — a pytest module marked
`@pytest.mark.integration`. It imports two shared modules:

- `aiac/test/integration/scenario_uc1.py` — a **new** scenario module (sibling of the existing
  `scenario.py`, kept separate so `5.2`/`5.3` are unaffected). It carries the phase-1 users/roles, the two
  `policy.md` variants, and the pair-lists expressed over the **discovered, workload-prefixed** names
  (`github-tool.source-read`, `github-agent.source_operations`, …), which are what the generated Rego
  actually contains.
- `aiac/test/integration/launcher.py` — the shared `kubectl`/port-forward + `opa` helpers (extended from
  the `5.3` launcher; see *[Testing Decisions](#testing-decisions)*).

It also ships `aiac/test/integration/probe_uc1.rego` — a standalone Rego module used only as the outbound
verification query, adapted from `5.3`'s `probe.rego` to match against **full discovered scope names**
(no prefix-stripping soft match; see step 7).

## Description

A `@pytest.mark.integration` test that validates the **phase-1 deliverable**
([../../gh-issues/sub-issue-phase-1.md](../../gh-issues/sub-issue-phase-1.md)) and confirms the runnable
demo: it deploys the real **`github-agent`** and a simplified **`github-tool`** to a live Kagenti/Kind
cluster, drives the **real UC-1 Service Onboarding agent** (`onboard_service` via the Controller's HTTP
trigger) for each, and then **asserts** the generated Rego decides correctly by running the standalone
`opa eval` binary as its verification oracle.

Phase-1 is explicit that **live enforcement / live traffic is out of scope** — correctness is shown by
**evaluating the generated rules**, not by routing real requests. So this test is *Deploy + discover +
evaluate*: the agent and tool are really deployed and really discovered by UC-1 (classify from the
`kagenti.io/type` label, read the AgentCard, read the MCP `tools/list`, provision roles/scopes into
Keycloak, model access, emit Rego), but **no A2A message is ever sent through the agent**.

The generated Rego is the **artifact under test** — the LLM/PCE that produced it might be wrong — so the
test never trusts it. Expected verdicts are **computed from** the `scenario_uc1.py` pair-lists (the
intended policy), and the real Rego is asserted to admit/deny each scenario-derived request as the truth
table requires. A mismatch fails the test and names the exact cell.

Because it needs a live Kagenti cluster + operator + Keycloak + a real LLM, it is `@pytest.mark.integration`
and stays out of the default unit run (`-m "not integration"`); it additionally `pytest.skip`s when no
`opa` binary is found.

### Topology

- **AIAC runs on the host? No — in-cluster.** The pipeline is driven **inside the cluster** so UC-1's
  `analyze_tool` can reach the tool's MCP endpoint at its cluster-internal DNS name
  (`github-tool.{ns}.svc.cluster.local`). The test triggers onboarding over HTTP against the in-cluster
  AIAC Controller (`POST /apply/service/{service_id}`), reachable via `kubectl port-forward`.
- **Two AIAC stacks, one per policy variant.** Because the PRB reads its policy from `AIAC_POLICY_FILE`
  (a file/ConfigMap **baked into the AIAC pod**, not selectable per request), the two `policy.md`
  variants (`explicit`, `abstract`) are served by **two independent in-cluster AIAC stacks** — each an
  AIAC agent + its own Policy Store + its own OPA Policy Writer, mounting its own variant. The **IdP
  Configuration Service and Keycloak are shared** (provisioning is variant-independent and idempotent).
- **Rego capture.** Each stack's OPA Policy Writer writes `.rego` (the filesystem stub — phase-1 emits to
  a filesystem location, **not** the K8s CR) into a mounted volume. The test `kubectl cp`/`exec`s each
  variant's `.rego` out to a host temp dir `rego_out/<variant>/`, then runs the `opa eval` oracle on the
  host.

### What it does

The pipeline (deploy → onboard tool → onboard agent → emit Rego) is driven **once per `policy.md` variant**
— against that variant's AIAC stack — each writing into its own `rego_out/<variant>/`. `opa eval` then
asserts the truth table against **each** variant's Rego (step 7). Steps 1–6 describe the run.

0. **Point the demo namespace at the dedicated realm (test fixture).** Before deploying workloads, set
   `KEYCLOAK_REALM=<AIAC_TEST_REALM>` in the demo namespace's **`authbridge-config`** ConfigMap. The
   kagenti-operator reads the realm per-namespace from this key and **preserves an admin/CI-set value**
   (operator issue #433), so the operator registers *this* namespace's clients into the test realm with
   **no operator restart or cluster-wide change**. (Default without this is `kagenti`.)
1. **Provision the realm's users + realm roles (test fixture).** UC-1 does **not** create users or realm
   roles, so the fixture provisions them via **`python-keycloak` `KeycloakAdmin`** into the dedicated test
   realm (`AIAC_TEST_REALM`), **before** onboarding (the Service Policy Builder reads the full realm-role
   universe): users `dev-user`, `test-user`, `devops-user`; realm roles `developer`, `tester`, `devops`
   (with the generic ≤255-char descriptions in *[Scenario inputs](#scenario-inputs)* — the PRB reads
   them); role assignments (`devops-user`→`devops`, which maps to **no** scope — the deny case).
   Provisioning is idempotent (create-or-get); state is **left in place** on teardown (the shared realm is
   never deleted/recreated — reruns converge).

2. **Deploy the demo workloads.** `kubectl apply` the simplified **`github-tool`**
   ([../demo/github-tool.md](../demo/github-tool.md)) and the real **`github-agent`**
   ([../demo/github-agent.md](../demo/github-agent.md)) into the demo namespace. Wait until: pods Ready;
   the kagenti-operator has **registered a Keycloak client** for each (with `client.name =
   "{namespace}/{workload}"`) and applied the `kagenti.io/type` pod label (`tool`/`agent`); the
   `github-agent` **AgentCard CR** is present; the tool **Service** carries the `protocol.kagenti.io/mcp`
   label and answers `tools/list`.

3. **Onboard the tool, then the agent (real UC-1), against each variant's AIAC stack.** Order matters —
   the tool is onboarded first so its scopes exist when the agent's Service Policy Builder reads the scope
   universe:
   > **Resolving `{service_id}`.** The trigger takes the Keycloak **client id**, which is **not** the
   > string `github-tool`: the operator sets `client.name = "{ns}/{workload}"` but the client *id* is
   > `"{ns}/{workload}"` only when SPIRE is off — with `--spire-trust-domain` set it is a SPIFFE URI
   > (`spiffe://<trust-domain>/ns/{ns}/sa/{sa}`). The test resolves the id by looking up the client whose
   > **name** is `"{ns}/github-tool"` (resp. `"{ns}/github-agent"`) via the Configuration library /
   > Keycloak admin, then triggers with that id.

   - `POST /apply/service/{github-tool client id}` → UC-1 classifies it a **Tool** from the label, reads
     the MCP manifest, provisions scopes `github-tool.{source-read, source-write, issues-read,
     issues-write}` (verbatim tool descriptions), sets `client.type=Tool`. **No rules are written for the
     tool directly.**
   - `POST /apply/service/{github-agent client id}` → UC-1 classifies it an **Agent** from the label,
     reads the AgentCard, provisions role `github-agent.agent` + scopes
     `github-agent.{source_operations, issue_operations}` (from the two skills), sets `client.type=Agent`,
     then the Service Policy Builder maps the realm roles→scopes via the real PRB (real LLM,
     `temperature=0`) and the Controller calls `compute_and_apply(rules, override=False)` → the OPA writer
     emits `github_agent.inbound.rego` + `github_agent.outbound.rego`.

4. **Capture the Rego.** `kubectl cp`/`exec` each variant stack's OPA-writer output to
   `rego_out/<variant>/`.

5. **Repeat steps 3–4 for the second variant** (the other AIAC stack). Provisioning re-runs are idempotent
   and variant-independent; only the emitted Rego differs.

6. **Teardown in `finally`.** Delete the demo workloads (operator de-registers the clients); leave the
   realm, users, realm roles, and `.rego` files in place for eyeballing. AIAC stacks may be left running or
   torn down per the harness.

7. **Assert the truth table with `opa eval`.** For each variant's captured Rego, evaluate a matrix of
   **(request JSON, rego file)** tuples with the standalone `opa` binary and hard-assert each decision:
   - **`opa` discovery** — `$OPA_BIN` → `shutil.which("opa")` → `pytest.skip("opa not found")`.
   - **Inbound** — one node per `(variant × subject)`. Request `{"subject": <id>}` against
     `data.authz.github_agent.inbound.allow`. Coarse existential "can this user reach the agent at all";
     the discovered agent-scope names (`github-agent.source_operations` / `.issue_operations`) do not need
     to match anything — inbound has no function field.
   - **Outbound (user gate only)** — one node per `(variant × subject × function_name)`, where
     `function_name` is a **full discovered tool-scope name** (`github-tool.source-read`, …). Evaluated by
     the probe query `data.probe.outbound.allow` (in `probe_uc1.rego`), which binds `input.function_name`
     against the generated **user→tool** data maps (`subject_ok`) **only**. The agent→tool gate
     (`target_ok`) is **not** probed — under UC-1's single generic `github-agent.agent` role it is
     degenerate/empty (see *[The agent-to-tool gate](#the-agent-to-tool-gate-degenerate-by-design)*), matching
     phase-1's "user-gating dimension only."
   - **Name matching** — because `scenario_uc1.py` stores the **full discovered scope names**, the probe
     matches `input.function_name` to a scope by **exact string equality** (no prefix-stripping token-set
     soft match — that was `5.3`'s device for bare names; here both sides are already prefixed).
   - The expected verdict for every cell is **computed from** the `scenario_uc1.py` pair-lists, not a
     second hand-maintained copy. A failing node names the exact `variant / subject / function_name` cell.

8. **Assert grant-set equivalence (semantic, from the Rego).** The pipeline runs inside the AIAC pod, so
   the test never sees the intermediate `list[PolicyRule]`. Grant-set equivalence is therefore re-derived
   from the **generated Rego data maps**: dump each variant's user→tool grant map (via `opa eval
   data.authz.github_agent.outbound...`) and each variant's inbound `subject_roles`/`agent_scopes`, and
   assert, as order-independent `(role, scope)` **sets**, that **each variant equals the `scenario_uc1.py`
   truth table** and **the two variants equal each other**. This compares grant *sets*, not Rego text
   (formatting/name-ordering may differ; the grant set may not) — the semantic-similarity guarantee this
   test makes. It catches verdict-neutral over/under-grants the coarse `opa eval` oracle hides.

## Expected output

The test passes when `opa eval` decides every cell of the scenario truth table as follows, for **both**
policy variants. Verdicts are **computed from** the `scenario_uc1.py` pair-lists, not a hand-maintained
copy — these tables are the human-readable rendering of them. They are **identical to policy-pipeline's**
(only the underlying scope-name strings differ).

`USERS`: `dev-user`→`developer`, `test-user`→`tester`, `devops-user`→`devops`.

**Inbound allow** (`data.authz.github_agent.inbound.allow`, user-role→agent-scope, existential):

| Subject | Inbound |
|---|---|
| dev-user | ✅ |
| test-user | ✅ |
| devops-user | ❌ |

**Outbound allow(subject, function)** (`data.probe.outbound.allow`, user→tool gate only; `function_name`
is the full discovered scope name, shown here by its bare suffix for readability):

| | github-tool.source-read | github-tool.source-write | github-tool.issues-read | github-tool.issues-write |
|---|---|---|---|---|
| dev-user | ✅ | ✅ | ✅ | ❌ |
| test-user | ❌ | ❌ | ✅ | ✅ |
| devops-user | ❌ | ❌ | ❌ | ❌ |

Each variant leaves exactly **two** files on disk in its `rego_out/<variant>/`:

- `github_agent.inbound.rego` — package `authz.github_agent.inbound`; the **user→agent** gate.
  `subject_roles` = `{dev-user: [developer], test-user: [tester]}`; `agent_scopes` populated with the
  **discovered** names `[github-agent.source_operations, github-agent.issue_operations]`. (`devops-user`
  holds `devops`, which maps to no agent scope, so it is absent and denied inbound.)
- `github_agent.outbound.rego` — package `authz.github_agent.outbound`; `allow if { subject_ok; target_ok
  }`. Its **`subject_ok`** is the **user→tool** gate (populated: `subject_role_scopes` grouping
  developer/tester → `github-tool.*` scopes). Its **`target_ok`** (agent→tool) is **degenerate/empty**
  under the single generic `github-agent.agent` role — documented, not asserted (see below).

Explicitly **no** `github_tool.*.rego` — the pipeline emits no tool model (the tool is a pure target;
phase-1 acceptance requires "no rules written for the tool alone").

### Semantic similarity, not byte-identity

This test's Rego is **semantically similar** to `policy-pipeline`'s but **not byte-identical**, for two
baked-in reasons in the (frozen) UC-1 provisioning:

1. **Workload-prefixed names.** UC-1 names every scope `{workload}.{name}`, so the data maps hold
   `github-tool.source-read` / `github-agent.source_operations` where `policy-pipeline` holds bare
   `source-read` / `source-access`. Same **file set + package shapes**; different name strings.
2. **Degenerate `target_ok`.** UC-1 emits one generic `github-agent.agent` role (description `"Agent
   role"`), which the PRB cannot map to specific tool scopes under deny-by-default, so the agent→tool gate
   is empty — where `policy-pipeline`'s two operator roles populate it across all four scopes.

Both are inherent to running real UC-1; making the Rego byte-identical would require unfreezing UC-1
(dropping the prefix + deriving per-skill agent roles) or abandoning UC-1 discovery (which would collapse
this test into `policy-pipeline`). The test therefore asserts **same files + same decisions + equivalent
grant sets**, not identical text.

### The agent-to-tool gate (degenerate by design)

Phase-1 states outbound access is an **intersection** of the user→tool gate and the agent→tool gate, but
that "the agent holds all of `github-tool`'s scopes, so this demo exercises the **user-gating dimension
only**; the agent-role gate is not exercised here" (deferred to a future two-tool demo). Under real UC-1
the single generic `github-agent.agent` role yields an **empty** `target_ok`, so the generated `allow`
(`subject_ok AND target_ok`) would deny everything. The probe therefore evaluates **`subject_ok` only**,
which is exactly the user-gating slice phase-1 validates. The empty `target_ok` is documented as a known
UC-1 limitation, not a test failure.

## Scenario

The phase-1 scenario — identical role→access facts to `policy-pipeline`, driven end-to-end through real
UC-1 onboarding of deployed workloads rather than a hand-built provisioning step.

| Element | Value |
|---------|-------|
| Realm | `AIAC_TEST_REALM` (dedicated; default `aiac-uc1-e2e`) |
| Agent | `github-agent` — **discovered** role `github-agent.agent`; scopes `github-agent.source_operations`, `github-agent.issue_operations` (from AgentCard skills) |
| Tool | `github-tool` (simplified) — **discovered** scopes `github-tool.source-read`, `github-tool.source-write`, `github-tool.issues-read`, `github-tool.issues-write` (from MCP `tools/list`) |
| Users | `dev-user` (role `developer`), `test-user` (role `tester`), `devops-user` (role `devops`) |
| `developer` | source read/write + issues read |
| `tester` | issues read/write |
| `devops` | no access (inbound deny; denied every outbound function) |

Role → access (the fixed facts both `policy.md` variants and the `scenario_uc1.py` pair-lists must agree
with; the generic descriptions are not part of this triad):

- `developer` — source read/write, issues read.
- `tester` — issues read/write.
- `devops` — no access. Conveyed by the **role description only** — absent from every pair-list and both
  `policy.md` variants (deny-by-default), so denied inbound and on every outbound function.

## Configuration (env)

| Variable | Purpose | Default |
|----------|---------|---------|
| `KUBECONFIG` | Kubeconfig for the live Kagenti/Kind cluster | — (required) |
| `AIAC_DEMO_NAMESPACE` | Namespace the demo workloads deploy into | `team1` |
| `KEYCLOAK_URL` | External Keycloak base URL | — (required) |
| `KEYCLOAK_ADMIN_REALM` | Realm the admin creds live in | `master` |
| `KEYCLOAK_ADMIN_USERNAME` / `KEYCLOAK_ADMIN_PASSWORD` | Keycloak admin creds (for user/realm-role provisioning) | — (required) |
| `AIAC_TEST_REALM` | Dedicated realm the test uses; the fixture sets `KEYCLOAK_REALM` on the demo namespace's `authbridge-config` ConfigMap so the operator registers this namespace's clients into it (per-namespace, no cluster-wide change) | `aiac-uc1-e2e` |
| `AIAC_REALM` | Realm the AIAC stacks read back (= `AIAC_TEST_REALM`) | `aiac-uc1-e2e` |
| `AIAC_EXPLICIT_URL` / `AIAC_ABSTRACT_URL` | Base URL of each variant's in-cluster AIAC Controller (via port-forward) for `POST /apply/service/{id}` | `http://127.0.0.1:7070` / `:7080` |
| `AIAC_OPA_POD_EXPLICIT` / `AIAC_OPA_POD_ABSTRACT` | OPA-writer pod (or PVC) per variant to `kubectl cp` `.rego` from | — (resolved from labels) |
| `REGO_OUTPUT_DIR` | Host base dir the captured `.rego` is copied to; `rego_out/<variant>/` per variant, left on disk | operator-chosen local dir |
| `LLM_BASE_URL` / `LLM_MODEL` / `LLM_API_KEY` | PRB LLM (pinned `temperature=0`); consumed by the in-cluster AIAC pods | — (required) |
| `OPA_BIN` | Path to the standalone `opa` binary (oracle); else `PATH`, else `pytest.skip` | — (optional; PATH lookup) |

> When the test is written, confirm: the Controller trigger path and port; and the OPA writer's output
> path / how the two variant stacks are deployed and addressed. The realm mechanism (per-namespace
> `authbridge-config` `KEYCLOAK_REALM`, preserved by the operator — issue #433) and the `{service_id}`
> resolution (look up the client by `client.name = "{ns}/{workload}"`; the id is a SPIFFE URI under SPIRE)
> are **confirmed** against the operator source and reflected above.

## Runbook

Runnable only against a live Kagenti/Kind cluster (operator + Keycloak + SPIRE) **configured to register
clients into `AIAC_TEST_REALM`**, with the two AIAC variant stacks deployable, a real LLM, the demo agent
(GA-1…9) and simplified tool images built + kind-loaded, and an `opa` binary on `PATH` (or `$OPA_BIN`).

```bash
# env: KUBECONFIG + KEYCLOAK_URL + admin creds + LLM_* set; realm defaults to aiac-uc1-e2e; opa on PATH or $OPA_BIN
.venv/bin/pytest test/integration/test_uc1_onboarding_pipeline.py -m integration -v
# Parametrized nodes: (variant × subject) inbound + (variant × subject × function_name) outbound + grant-set equivalence.
# A failing node names the exact cell, e.g.:
#   test_outbound[abstract-test-user-github-tool.source-read] — expected deny, opa allowed
# The generated Rego is left on disk per variant for eyeballing:
#   rego_out/explicit/github_agent.{inbound,outbound}.rego
#   rego_out/abstract/github_agent.{inbound,outbound}.rego
#   (no github_tool.*.rego in either)
```

The suite `pytest.skip`s when no `opa` binary is found. Eyeball the persisted Rego against the ID-only
package shapes in [../components/pdp-policy-writer-opa.md](../components/pdp-policy-writer-opa.md);
optionally inspect the provisioned Keycloak realm and the discovered scopes.

## Testing Decisions

- **Highest seam available, verified by a real oracle.** Real deployed workloads + real kagenti-operator
  + real UC-1 onboarding + real PRB/PCE + real Keycloak + real LLM. The test drives the pipeline through
  its production trigger (`POST /apply/service/{id}`) and verifies the real filesystem Rego with the
  standalone `opa eval` binary. A good test here asserts only **external behavior** — the *decisions* the
  generated Rego makes — never internal Rego structure.
- **Rego is the artifact under test; the scenario is the oracle.** Expected verdicts are computed from
  `scenario_uc1.py`, not from the Rego. A wrong role→scope mapping fails the test at the exact cell.
- **Deploy + discover + evaluate, no live traffic.** Per phase-1, enforcement/token-exchange/live A2A is
  out of scope; correctness is shown by evaluating the generated rules. The agent pod need only exist
  (labelled `kagenti.io/type=agent`, AgentCard present); the simplified tool need only answer `tools/list`
  — neither is driven with real requests.
- **In-cluster AIAC for MCP reachability.** UC-1's `analyze_tool` posts `tools/list` to a
  cluster-internal DNS name, so the pipeline runs in-cluster (triggered over HTTP) rather than in-process
  on the host, which cannot resolve `*.svc.cluster.local`.
- **Two AIAC stacks for two variants.** The PRB policy is baked into the pod (`AIAC_POLICY_FILE`), so each
  `policy.md` variant is served by its own AIAC stack; Keycloak + the IdP Configuration Service are shared
  (provisioning is variant-independent and idempotent).
- **User gate only.** UC-1's single generic agent role yields an empty `target_ok`; the outbound probe
  evaluates `subject_ok` alone, matching phase-1's user-gating-only intent. The agent-role gate is
  deferred (needs a second tool the agent is not mapped to).
- **Semantic similarity, from the Rego.** Since the pipeline runs in-pod, grant-set equivalence is
  re-derived from the generated Rego data maps (not an in-process `list[PolicyRule]`), and compares grant
  *sets* across variants and vs the scenario — the semantic-similarity guarantee, not byte-identity.
- **Dedicated realm, leave-in-place.** A dedicated test realm (`AIAC_TEST_REALM`) the operator is
  configured for; the shared realm is never deleted/recreated, so cleanup is idempotent leave-in-place
  (users/roles create-or-get; UC-1 writes idempotent; demo workloads deleted so the operator de-registers
  the clients).
- **LLM nondeterminism, contained.** The PRB LLM is pinned `temperature=0`; both variants are asserted
  cell-by-cell (`opa eval`) and at the grant-set level. Some model-dependence remains, which is why the
  suite is `@pytest.mark.integration`, out of default CI.
- **Prior art, shared not copied.** Reuses the `5.3` shape (`@pytest.mark.integration`, `opa`
  discovery/skip, per-variant `rego_out/`, scenario-as-oracle, probe query) via the shared
  `launcher.py`/`scenario_uc1.py`, adapted from in-process/subprocess to deploy/port-forward/`kubectl cp`.

## Relationship to other integration tests

This is **one** integration-test spec among several indexed by the master PRD
([../PRD.md](../PRD.md), § *Integration test specifications*).

- **Discovery-driven sibling of `policy-pipeline`** ([policy-pipeline.md](policy-pipeline.md),
  `testing/5.3-policy-pipeline-integration-test.md`): identical scenario facts and truth tables, but this
  test *infers* the agent/tool entities via **real UC-1 onboarding of deployed workloads** instead of
  hand-provisioning them, so its Rego is semantically similar (not byte-identical) and it validates the
  **phase-1** deliverable end-to-end.
- Same `@pytest.mark.integration` + `opa eval` flavor as the live-Keycloak pytest tests
  (`testing/5.1-integration-tests.md`) and `policy-pipeline`; skips when `opa` is absent.

Tracking issue for this test: `testing/5.4-uc1-onboarding-integration-test.md`.

## Out of Scope

- **Writing `test_uc1_onboarding_pipeline.py`, `probe_uc1.rego`, `scenario_uc1.py`** — this spec
  *describes* the test; the test is written in a later session (tracked by
  `testing/5.4-uc1-onboarding-integration-test.md`).
- **The UC-1 agent, PRB, PCE, OPA writer, and demo `github-agent` implementations** — specified/tested by
  their own components/issues, not here. In particular UC-1's discovery naming and single-generic-role
  behavior are **fixed**; this test observes them, it does not change them.
- **Building the simplified `github-tool`** — its own build issue (a `blocked-by` of this test); the tool
  is specified in [../demo/github-tool.md](../demo/github-tool.md).
- **Live enforcement / A2A traffic / token exchange / K8s-CR Policy Writer** — Phase-2+; this test targets
  the filesystem stub and evaluates rules, per phase-1 out-of-scope.
- **Default-CI wiring** — `@pytest.mark.integration`; runs on demand.

## Further Notes

- **Fact triad.** The role→access facts are owned by three artefacts that must agree: the *Scenario*
  table, **both** `policy.md` variants, and the `scenario_uc1.py` pair-lists. Entity/role/scope
  descriptions are generic and functional and must not contradict the facts.
- **Two variants in the discovered world** (see *Scenario inputs*): an **explicit** variant that
  enumerates each `(user-role → discovered scope)` pair by its full prefixed name, and an **abstract**
  variant (phase-1's intent-only prose). **Neither names the agent role** — doing so would populate
  `target_ok` and break both the user-gate-only decision and cross-variant equivalence. Both are
  user-intent-only, both leave `target_ok` degenerate, and both must produce the **same discovered grant
  set**.
- **`devops` zero access** is conveyed by its role description only; it is absent from every pair-list and
  both `policy.md` variants (deny-by-default denies it inbound and on every outbound function).
- **Descriptions ≤255 chars, verbatim into Keycloak.** The tool-scope descriptions are the verbatim
  scenario descriptions the simplified tool returns from `tools/list`; the agent-scope descriptions come
  from the AgentCard skills; the realm-role descriptions are provisioned by the fixture.

## Blocked-by

- Simplified `github-tool` build issue (see [../demo/github-tool.md](../demo/github-tool.md)).
- Demo `github-agent` implementation — `demo/GA-1…GA-9` (deployable agent + AgentCard).
- UC-1 Service Onboarding — `agent/service-onboarding/3.6-service-onboarding-orchestrator.md` (**done**).
- The PRB / PCE / OPA-writer / Policy-Store prerequisites shared with `5.3`.
- A live Kagenti/Kind cluster + operator (registers clients into `AIAC_TEST_REALM` via the demo
  namespace's `authbridge-config` `KEYCLOAK_REALM`, set by the fixture — per-namespace, confirmed against
  the operator source); the `protocol.kagenti.io/mcp` Service label applied at deploy time
  (`../../gh-issues/kagenti-operator-mcp-label-stamping.md`); an `opa` binary at test time.

## Scenario inputs

These are **functional** inputs — the PRB reads the descriptions and the `policy.md` to produce the
role→scope mappings. The entity/role/scope descriptions are **generic and keyword-free**; client `type`
is set by UC-1 from the `kagenti.io/type` label (not description prose).

### Discovered entities (what UC-1 provisions)

- **`github-tool`** (Tool) → scopes, from the simplified tool's MCP `tools/list` (verbatim descriptions):
  - `github-tool.source-read` — "Read source repository contents: file listings and file bodies. Read-only."
  - `github-tool.source-write` — "Create, modify, or delete source repository contents; commit file changes."
  - `github-tool.issues-read` — "Read issues and their comment threads. Read-only."
  - `github-tool.issues-write` — "Create and update issues: open, edit, comment, and close."
- **`github-agent`** (Agent) → role `github-agent.agent` (description "Agent role") + scopes from the
  AgentCard skills:
  - `github-agent.source_operations` — "Browse and search code; read, create, and modify repository file contents, branches, and commits."
  - `github-agent.issue_operations` — "Read, search, create, and update issues, comments, sub-issues, and pull requests."

### Realm roles (provisioned by the fixture)

- `developer` — "Developer — an engineering user who develops the source codebase (writing and maintaining code) and fixes code defects reported in the issue tracker; works primarily in source and consults issues for defect reports."
- `tester` — "Tester — a quality-assurance user who verifies software quality and tracks defects through the issue tracker: filing, triaging, and updating issue reports; works in the issue tracker, not in source."
- `devops` — "DevOps — an operations user who manages deployment infrastructure and runtime environments; does not author source code and does not manage the issue tracker."

### `policy.md` — Version 1 (explicit)

Enumerates each `(user-role → discovered scope)` pair by name. **No agent-role→tool-scope section**
(target_ok is deferred). Deny by default.

```markdown
# Access Control Policy — github-agent / github-tool

Grant access on a least-privilege basis. Only grant a (role, scope) pair when this
policy supports it; deny by default.

## Users → agent capabilities (inbound; user may call the agent)
- developer may use github-agent.source_operations and github-agent.issue_operations.
- tester may use github-agent.issue_operations.

## Users → tool operations (outbound subject; user may reach the tool)
- developer may perform github-tool.source-read, github-tool.source-write, and github-tool.issues-read.
- tester may perform github-tool.issues-read and github-tool.issues-write.
```

### `policy.md` — Version 2 (abstract)

Phase-1's intent-only prose. Encodes the same facts; relies on the PRB/LLM to expand intent into the
discovered scopes via the entity/role descriptions.

```markdown
Grant access on a least-privilege basis: allow only what this policy states; deny by default.

- Developers may read and modify source, and read issues.
- Testers may read and modify issues.
```
