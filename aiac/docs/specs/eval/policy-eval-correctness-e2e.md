# Eval Spec: policy-eval-correctness-e2e — `test_policy_pipeline_correctness_e2e.py`

> **One spec among several.** This document specifies **one** integration test.
> Eval specs live **one spec per test** under `docs/specs/eval/`
> (a sibling of `components/`), and the master PRD's *Integration test specifications* section
> ([../PRD.md](../PRD.md)) is the index of them. This is a **companion to**, not a replacement
> for, [policy-eval-scenarios.md](policy-eval-scenarios.md) and
> [policy-eval-correctness-prb.md](policy-eval-correctness-prb.md): all three families reuse the
> same eight-scenario corpus (`SCENARIOS`, `truth`, from `eval/test_policy_pipeline_eval.py`),
> unmodified, but each isolates a different property or layer of the pipeline.

## Location

- `aiac/eval/correctness_scorer.py` — the reusable scorer (`score_gate`/`score_scenario`),
  reused **unmodified** from #2089 — see
  [policy-eval-correctness-prb.md § Scorer design](policy-eval-correctness-prb.md#scorer-design)
  for the scorer itself; this spec does not repeat it.
- `aiac/eval/correctness_e2e_helpers.py` — this suite's own pure-logic helpers (`_pairs_from_map`,
  `_user_role_rows`, `_accumulate_agent_gates`); `aiac/eval/test_correctness_e2e_helpers.py` —
  their unmarked unit tests, runs in the default fast pass. Mirrors
  `eval/correctness_scorer.py`/`eval/test_correctness_scorer.py`'s logic-module/test-module split.
- `aiac/eval/test_policy_pipeline_correctness_e2e.py` — the suite itself,
  `@pytest.mark.eval_correctness_e2e`.
- `aiac/eval/best_effort_rules.py` — the best-effort fallback's own pure-logic helper
  (`_best_effort_rules`); `aiac/eval/test_best_effort_rules.py` — its unmarked unit tests. See
  [Best-effort proposals](#best-effort-proposals).
- Reuses, unmodified, from `eval.test_policy_pipeline_eval`: `SCENARIOS`, the `pipeline` fixture
  (re-exported by import — see [Runbook](#runbook)), `_require_scenario`, `_rego_path`, `opa_bin`,
  `opa_eval`, `truth`.

## Description

`policy-eval-correctness-prb.md`'s suite scores the PRB's raw grant/deny output directly — no
Keycloak, no PCE, no OPA. This suite scores the **same** primary corpus's grant/deny output one
layer further downstream: real Keycloak provisioning → real Policy Rules Builder → real Policy
Computation Engine → real `opa eval` against the rendered Rego. It is the only correctness check
that can catch an integration bug between those layers (PCE merge logic, Rego rendering, OPA
semantics) that a PRB-only check structurally cannot see — the PRB could return exactly the right
rules and a bug in the PCE's merge or in `rego.py`'s rendering could still produce the wrong
Rego.

Same scoring philosophy as the PRB-level suite: precision and recall tracked separately per gate
and aggregated (never blended), a non-gating denial-precision figure, zero-tolerance on
over-grants, under-grants/incorrect denials reported only.

### Scoring source: the rendered Rego data maps, not per-pair decision probing

`opa eval` is queried directly against the plain data documents the Rego generator already
declares — `subject_role_allow_scopes`/`subject_role_deny_scopes` (inbound and outbound-subject)
and `agent_role_scopes` (outbound-target, ALLOW only) — rather than exhaustively probing every
`(role, scope)` pair's `allow`/`deny_ok` decision. Two `opa eval` calls per agent per direction
return the entire role→scopes table in one shot; `_pairs_from_map` flattens each into `(role,
scope)` pairs, and `_accumulate_agent_gates` unions every agent's pairs into the three top-level
gate buckets `score_scenario` expects. Both maps are already keyed by bare (deprefixed) role/scope
names — the same names the truth tables use — so no re-prefixing logic is needed.

| Gate | Direction | ALLOW map | DENY map |
|---|---|---|---|
| `inbound` | inbound rego | `subject_role_allow_scopes` | `subject_role_deny_scopes` |
| `outbound_subject` | outbound rego | `subject_role_allow_scopes` | `subject_role_deny_scopes` |
| `outbound_target` | outbound rego | `agent_role_scopes` (deprefixed `outbound_target_allow_rules`) | **none** — see below |

### Known gap: `outbound_target` denial is unrenderable, not just untested

`AgentPolicyModel.outbound_target_deny_rules` is computed by the PCE, but
`aiac.pdp.service.policy.opa.rego.generate_outbound_rego` never renders it into any queryable Rego
document — only the ALLOW side (`agent_role_scopes`) is emitted. So for the `outbound_target` gate,
`denied` is always `set()`, and that gate's `denial_precision` reads vacuously `1.0`.
`over_grants`/`under_grants` for `outbound_target` are unaffected (they depend only on
`granted`/`expected`, not `denied`). **This is a pre-existing production Rego-generator gap, not
introduced by this suite and not fixed by it** — see [Out of Scope](#out-of-scope).

## Configuration (env)

| Variable | Purpose |
|---|---|
| `KEYCLOAK_URL` / `KEYCLOAK_ADMIN_USERNAME` / `KEYCLOAK_ADMIN_PASSWORD` | Real Keycloak admin API — the `pipeline` fixture provisions one realm per scenario. |
| `LLM_BASE_URL` / `LLM_MODEL` / `LLM_API_KEY` | The PRB's real LLM calls (`orchestrate_prb`, same as every other suite reusing this fixture). |
| `OPA_BIN` (optional) | Path to the `opa` binary; falls back to `opa` on `PATH`. The suite skips cleanly (not a failure) if neither resolves. |
| `EVAL_PIPELINE_PARALLELISM` (optional, default = scenario count, currently 8) | Max concurrent `ProcessPoolExecutor` workers in the shared `pipeline` fixture — see [Parallelization](#parallelization). An escape hatch, not a tuning knob with a "correct" lower value; lower it only if the LLM endpoint rate-limits under full concurrency. |

`eval/conftest.py` auto-loads `eval/.env` (gitignored, override=False) if present, so a local
`eval/.env` with the above removes the need to `export`/source anything before invoking `pytest`
directly.

## Parallelization

The shared `pipeline` fixture (`eval/test_policy_pipeline_eval.py`, also used by the `eval_extended`
suite) provisions all 8 scenarios **concurrently**, one `ProcessPoolExecutor` worker per scenario —
not `pytest-xdist`'s `-n` (still unsupported/unneeded here: the parallelism lives inside the
fixture, across scenarios, not across pytest's own test-collection workers).

Separate **processes**, not threads, because the fixture's per-scenario setup mutates
process-global state while it runs — `os.environ["KEYCLOAK_REALM"]`/`["AIAC_POLICY_FILE"]`
(read at call time by `compute_and_apply`/`FilePolicySource.fetch()`) and a `KeycloakAdmin`
connection's `change_current_realm` — which two *threads* sharing one process would race on, but
separate OS processes don't (each gets its own `os.environ` and its own `KeycloakAdmin`). Each
worker also gets its own idp/store/opa port triple (offset from the module defaults by scenario
index), since fixed ports collide regardless of thread vs. process.

One further constraint, discovered empirically against a real Keycloak instance: concurrent
`admin.create_realm(...)` calls 409 with `"Duplicate resource error"` even across *distinct* realm
names — an internal Keycloak race on concurrent realm creation, not a naming collision on this
fixture's side. A `multiprocessing.Lock()`, passed to every worker via the pool's
`initializer`/`initargs` (a plain `multiprocessing.Lock()` cannot be passed as a regular per-task
argument under the `forkserver`/`spawn` start methods — only through that inheritance path),
serializes just the realm-provisioning step (`provision_keycloak_admin`). Everything after that per
scenario — the LLM-heavy `orchestrate_prb` calls and the idp/store/opa subprocess trio — still runs
fully concurrently; realm provisioning is a small fraction of one scenario's wall-clock next to the
PRB's several sequential LLM calls.

This benefits every suite that shares the `pipeline` fixture (`eval_extended` primarily, plus this
suite), not just `eval_correctness_e2e` — see [Relationship to other integration
tests](#relationship-to-other-integration-tests).

## Best-effort proposals

The PRB's generate→audit loop (`aiac.agent.policy_rules_builder.graph`'s `_audit`) can reject a
scope/role decision outright — a genuine contradiction (`PolicyContradictionError`) or an
exhausted retry budget (`PolicyRulesBuilderError`, after `MAX_AUDIT_RETRIES=3`). By default this
aborts `orchestrate_prb()` entirely for that scenario — `compute_and_apply` never runs, so there's
no Rego to score at all, and the scenario's report entry shows "setup failed" with nothing else.

The shared `pipeline` fixture instead calls `orchestrate_prb(..., best_effort=True)` (see
[Parallelization](#parallelization) — same fixture, applies to both its consumers, confirmed with
the user): a rejected decision falls back to whatever was last proposed before the auditor
rejected it, built the same way the PRB's own `build` node would have
(`eval.best_effort_rules._best_effort_rules`, unit-tested unmarked). That best-effort rule flows
into `compute_and_apply` and gets rendered into real Rego like any other rule — every other
decision in the scenario proceeds normally, and the scenario as a whole now completes and scores.

**This is an explicit, user-requested tradeoff**: the rendered Rego for a scenario with a
best-effort decision can contain a rule a real deployment never would (the auditor rejected it —
a real pipeline run aborts instead). `record_property("best_effort_notes", ...)` — a
`{scope_or_role_name: reason}` dict — and the printed summary line name exactly which decisions
this applies to; the report renders it as an extra field with an explicit caveat whenever
non-empty. See [`policy-eval-correctness-prb.md` §Best-effort
proposals](policy-eval-correctness-prb.md#best-effort-proposals) for the sibling suite's identical
mechanism (same underlying `orchestrate_prb` parameter).

## Runbook

```bash
# Unit-test the pure-logic helpers first (no live infra, runs in the default fast pass):
.venv/bin/pytest eval/test_correctness_e2e_helpers.py -v

# The suite itself — needs KEYCLOAK_URL + admin creds + LLM_* + opa on PATH:
.venv/bin/pytest eval/test_policy_pipeline_correctness_e2e.py -m eval_correctness_e2e -v -s

# Sanity-check the fixture's primary consumer still passes against the now-parallelized fixture:
.venv/bin/pytest eval/test_policy_pipeline_eval.py -m eval_extended -v
```

`require_env(...)` is the first line of the parametrized test function; `opa_bin()` skips the test
cleanly (not a failure) if `opa` isn't resolvable. Same skip-clean philosophy as every sibling
suite — this suite never false-passes when its infra isn't available.

## Expected output

Parametrized over all 8 scenario names (`sorted(SCENARIOS)`); expects all 8 to pass (zero
over-grants) given a healthy Keycloak instance and a well-behaved LLM endpoint. Before
`best_effort=True` was wired in (see [Best-effort proposals](#best-effort-proposals)), a real run
against the rossoctl kind cluster showed 6/8 passing and 2/8 failing at the *setup* stage
(`PolicyContradictionError`/`PolicyRulesBuilderError` from the PRB's own audit/retry loop,
`aiac.agent.policy_rules_builder.graph._audit`) — the **pre-existing, already-deferred**
audit/retry-convergence bug (confirmed to reproduce identically in the PRB-direct
`test_policy_pipeline_correctness_prb.py` suite, no Keycloak/PCE/OPA involved at all — unrelated
to this suite or its parallelization). `best_effort=True` doesn't fix that bug (still not this
ticket's job — see [Out of Scope](#out-of-scope)), it changes what a rejected scenario reports:
instead of "setup failed" with nothing else, it now scores (real, possibly imperfect
precision/recall) with `best_effort_notes` naming exactly which decisions weren't auditor-approved.
Each test case `record_property`s `precision`, `recall`, `denial_precision`, `over_grants`,
`under_grants`, `incorrectly_denied` (the latter three as `{gate: sorted(pairs)}`), and
`best_effort_notes`, and prints:

```text
[correctness-e2e] wildcard_grant: precision=1.000 recall=1.000 denial_precision=1.000
  over_grants={}
  under_grants={}
  incorrectly_denied={}
  best_effort_notes={}
```

A failing case's assertion message names the scenario and the exact over-granted `(role, scope)`
pairs per gate.

## Test report

Covered by `eval/conftest.py`'s existing Markdown report and its generic
`"precision" in props and "recall" in props` render-branch dispatch (already used by
`test_prb_correctness`) — no per-suite special-casing was needed; `eval_correctness_e2e` was simply
added to the `MARKERS` set. See
[policy-eval-correctness-prb.md § Test report](policy-eval-correctness-prb.md#test-report) for the
render behavior itself.

## Relationship to other integration tests

This is **one** integration-test spec among several indexed by the master PRD
([../PRD.md](../PRD.md), § *Integration test specifications*).

- **Companion to, not a replacement for,
  [policy-eval-correctness-prb.md](policy-eval-correctness-prb.md).** Same corpus, same scorer,
  same philosophy — the PRB-level suite isolates the PRB's own grant decisions with no
  Keycloak/PCE/OPA in the loop; this suite scores the same corpus one layer further downstream,
  through the full pipeline, and is the only one of the two that can catch a PCE-merge or
  Rego-rendering bug.
- **Shares the `pipeline` fixture with `eval_extended`** (`test_policy_pipeline_eval.py`) — the
  parallelization work described above (see [Parallelization](#parallelization)) speeds up both
  suites, since it lives in the shared fixture, not in this suite's own file. The
  [best-effort fallback](#best-effort-proposals) is the same story: it's a property of the shared
  fixture, so `eval_extended`'s own per-cell tests (`test_inbound`/`test_outbound`/
  `test_grant_set_matches_truth_table`) are affected too, not just `test_e2e_correctness` — a
  scenario whose PRB rejects a decision no longer gets a clean scenario-level skip; it runs
  through with a best-effort fallback for *that* decision only, so those specific per-cell
  assertions can show a real (and possibly wrong) result instead of a skip. Confirmed as the
  accepted tradeoff with the user, not treated as a regression to fix.
- **New marker, registered in `pyproject.toml`** (`eval_correctness_e2e`), distinct from
  `eval_extended`/`eval_consistency`/`eval_robustness`/`eval_correctness_prb`.

## Out of Scope

- **Fixing the PRB audit/retry-convergence bug** (`aiac.agent.policy_rules_builder.graph._audit`,
  around line 200-220) that causes `agent_delegation`/`unreachable_resources` (or, on other runs,
  `confusable_agents` — which scenario(s) hit it varies with LLM sampling, but at least one of
  `agent_delegation`/`confusable_agents` reproduces consistently) to reject a scope/role decision
  with `PolicyContradictionError`/`PolicyRulesBuilderError`. Confirmed pre-existing and unrelated
  to this suite (reproduces identically in the PRB-direct `eval_correctness_prb` suite).
  `best_effort=True` (see [Best-effort proposals](#best-effort-proposals)) changes how a rejection
  is *reported* (scored with a caveat instead of "setup failed") — it does not fix the underlying
  bug. The user has already deferred fixing `graph.py` itself as a separate follow-up — not this
  ticket's job.
- **Fixing the `outbound_target` denial-rendering gap** in
  `aiac.pdp.service.policy.opa.rego.generate_outbound_rego` — see
  [Known gap](#known-gap-outbound_target-denial-is-unrenderable-not-just-untested). This is
  production code, unrelated to this eval suite's own scope; the gap is documented and reported
  (vacuous `denial_precision=1.0` for that one gate), not silently hidden.
- **A committed trend log** — same deferral as
  [policy-eval-correctness-prb.md § Out of Scope](policy-eval-correctness-prb.md#out-of-scope),
  #2091.
- **An under-grant tolerance threshold.** Still TBD per the originating spec; under-grants are
  tracked/reported only, never gated.
- **New scenarios.** Reuses the existing 8-scenario corpus unmodified — see
  [policy-eval-correctness-prb.md § Taxonomy cross-check](policy-eval-correctness-prb.md#taxonomy-cross-check).
- **Parallelizing across pytest itself (`-n`/xdist).** The parallelism lives inside the shared
  `pipeline` fixture (see [Parallelization](#parallelization)), which already gets the wall-clock
  benefit without the 8x duplicated-provisioning problem `-n` would cause for a suite built on one
  session-scoped fixture.

## Blocked-by

None — #2089 (the PRB-level suite and `correctness_scorer.py`) is done, and this suite is the last
consumer that `correctness_scorer.py` was explicitly designed to support.
