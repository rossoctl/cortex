# Eval Spec: policy-eval-robustness-consistency — `test_policy_pipeline_consistency.py` + `test_policy_pipeline_robustness.py`

> **One spec among several.** This document specifies a **family** of integration tests.
> Eval specs live **one spec per test** under `docs/specs/eval/`
> (a sibling of `components/`), and the master PRD's *Integration test specifications* section
> ([../PRD.md](../PRD.md)) is the index of them. This is the **policy-eval-robustness-consistency**
> family — it is a **companion to**, not a replacement for,
> [policy-eval-scenarios.md](policy-eval-scenarios.md): that family's eight heavy scenarios (full
> pipeline, `opa eval` truth tables) and two light scenarios (guardrail-contract `xfail`s) are
> reused here as the scenario **corpus**, unmodified, but neither this family's two suites touch
> Keycloak, the PCE, `opa`, or the filesystem Rego stub — they call the Policy Rules Builder (PRB)
> directly and compare its raw output.

## Location

Both suites live under `aiac/eval/`, alongside `policy-eval-scenarios.md`'s heavy
scenarios, and reuse that family's scenario corpus rather than defining their own:

- `aiac/eval/test_policy_pipeline_consistency.py` — the consistency suite,
  `@pytest.mark.eval_consistency`.
- `aiac/eval/test_policy_pipeline_robustness.py` — the robustness suite,
  `@pytest.mark.eval_robustness`.
- `aiac/eval/prb_direct.py` — shared helper, `build_roles_and_scopes(scenario)`,
  used by both suites (see [No-Keycloak design](#no-keycloak-design) below).
- `aiac/eval/scenarios_perturbed/` — eight hand-authored semantic-sibling scenario
  modules + policy `.md` files, one per `policy-eval-scenarios.md` scenario (including
  `agent_delegation`, even though its **original** lives at `test/integration/` top level, not
  under `eval/`) — used only by the robustness suite's semantic tier (see
  [Perturbation tiers](#perturbation-tiers)).
- Both suites import `SCENARIOS`, `orchestrate_prb`, `grant_sets`, `truth` from
  `eval.test_policy_pipeline_eval` **unmodified** — no changes to that module's
  own logic were needed for this work, beyond the unrelated file-reorg noted below.
- `aiac/eval/conftest.py` — the same per-run Markdown report generator
  `policy-eval-scenarios.md` documents, widened to also cover these two suites' markers (see
  [Test report](#test-report)).

**Unrelated but adjacent change, done as prerequisite cleanup for this work:** the seven
`eval/`-resident scenario modules from `policy-eval-scenarios.md` (`scenario_eval_baseline.py` and
six siblings, plus their `policy.eval_*.md` files) were moved from `eval/` directly into a new
`eval/scenarios/` subpackage, so the growing `eval/` directory doesn't flatten test modules,
scenario-data modules, and (now) two more scenario-data variants (`scenarios_perturbed/`) into one
namespace. `scenario_eval_agent_delegation.py`/`policy.eval_agent_delegation.md` are unaffected —
they already lived at `test/integration/` top level and stay there. `test_policy_pipeline_eval.py`'s
imports were updated accordingly; no test logic changed.

## Description

`policy-eval-scenarios.md` proves the PRB's grant decisions are **correct** against a truth table,
once per scenario. It does not check whether those decisions are **consistent** (same input, run
again, same output) or **robust** (a small, meaning-preserving change to the input shouldn't flip
the output). Both properties matter for an LLM-backed access-control decision-maker in a way they
would not for a deterministic one: an LLM call can vary run-to-run on identical input, and can be
sensitive to phrasing/formatting/ordering in ways a human reviewer would not expect to matter. This
family adds two suites that isolate exactly those two properties, both scoped to the PRB's raw
output only — no OPA/PCE/k8s pipeline stage is involved in either (see
[No-Keycloak design](#no-keycloak-design)).

Both suites reuse the exact same 8-scenario corpus `policy-eval-scenarios.md` already defines
(`baseline`, `agent_delegation`, `unreachable_resources`, `ambiguous_clause`, `wildcard_grant`,
`misleading_descriptions`, `confusable_agents`, `empty_descriptions`) — one parametrized test case
per scenario, per suite.

### Consistency suite

`test_prb_consistent_across_repeats` runs `orchestrate_prb()` `N` times (default 5, overridable via
`PRB_CONSISTENCY_REPEATS`) against the **same, unperturbed** scenario input, classifies each run's
rules via `grant_sets()`, and asserts every run's grant sets are exactly equal across all three
gates (`inbound`/`outbound_subject`/`outbound_target`) — run 0 is the pivot; equal-to-pivot for
every other run transitively proves all N runs pairwise equal. No tolerance, no majority vote: this
is access control, so any run-to-run disagreement is itself the finding, not noise to average away.
A failing scenario's assertion message names the offending gate, the specific `(role, scope)` pairs
that differ, and which run index disagreed with run 0.

### Robustness suite

`test_prb_robust_to_perturbation` checks the PRB's grant decision is unchanged under two
independent perturbation tiers, both applied to the **same** scenario and both compared against
that scenario's own truth table (`truth(scenario)`, from `test_policy_pipeline_eval.py`) — not
against each other. A single combined pass/fail per scenario; if one tier fails and the other
passes, the scenario still reports as robustness-failed overall, with the assertion message stating
which tier(s) failed and the mismatching pairs per gate.

#### Perturbation tiers

1. **Mechanical** — a deterministic, RNG-free transform (`_mangle_text`) applied to the policy text
   and to every candidate `Role`/`Scope` description: whitespace/newline noise, casing noise (every
   3rd word forced upper, every 5th forced lower, by word index — not randomness, so the tier is
   itself perfectly reproducible run to run), and punctuation noise (space out `.`/`,`). Combined
   with candidate-list reordering (`_reordered`): a `SimpleNamespace` view of the scenario with
   every dict-iteration-order-sensitive field (`USER_ROLES`, `AGENTS`/`TOOLS` and each entry's
   nested scope/role dicts) reversed, since `orchestrate_prb()` derives every candidate list's
   order directly from the scenario module's own dict order. Name-keyed pair lists
   (`INBOUND_PAIRS` etc.) are order-insensitive (compared as sets downstream) and are copied through
   unchanged.
2. **Semantic** — a hand-authored, meaning-preserving reworded sibling scenario module from
   `eval/scenarios_perturbed/` (different phrasing throughout every `AGENTS`/`TOOLS`/`USER_ROLES`
   description and the paired policy `.md` text; every name-keyed field — ids, role/scope names,
   `INBOUND_PAIRS`/`OUTBOUND_PAIRS`/`OUTBOUND_SUBJECT_PAIRS`, and the two scenario-specific fields
   `EXPECT_NO_REGO`/`IDENTITY_CONFUSION_PROBES` where present — is byte-identical to the original).
   `empty_descriptions`' perturbed sibling is special-cased: its descriptions stay `""` (that
   scenario's whole point is the absence of description text), only its policy `.md` is reworded.
   Because names are guaranteed identical between a scenario and its perturbed sibling, `truth()`
   and `grant_sets()`'s name-based classification apply to the perturbed sibling's rules with no
   special-casing — `grant_sets(scenario, sem_rules)` (the **original** module, not the perturbed
   one) is exactly the right call.

Neither tier changes any production code: both drive `AIAC_POLICY_FILE` (via `monkeypatch.setenv`)
and pass perturbed `Role`/`Scope`/scenario-shaped objects into the existing, unmodified
`orchestrate_prb()`.

## No-Keycloak design

Both suites build synthetic `Role`/`Scope` objects directly (`prb_direct.build_roles_and_scopes`)
instead of provisioning a live Keycloak realm the way `test_policy_pipeline_eval.py`'s heavy
scenarios do. This is deliberate, not a shortcut taken for convenience: the agreed scope for both
suites is **the PRB's raw output only** — no OPA/PCE/k8s pipeline stage is exercised, so there is
nothing downstream that needs a real IdP-backed `Role`/`Scope` (`serviceId` mappings, Keycloak
client scopes, realm roles). `orchestrate_prb()` only ever reads `.name`/`.description` off these
objects (plus the scenario module's own dict order, for candidate-list ordering) — a synthetic
`id` is sufficient for everything else on the model.

Practical consequence: both suites need only `LLM_BASE_URL`/`LLM_MODEL`/`LLM_API_KEY` — no
`KEYCLOAK_URL`, no Keycloak admin creds, no `opa` binary on `PATH`. This is a strictly lighter
prerequisite set than `policy-eval-scenarios.md`'s heavy scenarios, despite reusing the same
scenario corpus.

**The live-cluster UC1 onboarding ladder (`uc1-onboarding-pipeline.md`) is untouched and unused by
this work** — that ladder validates real in-cluster onboarding against a deployed AIAC stack, an
entirely different concern from this family's PRB-output-only scope.

## Expected output

Both suites parametrize over all 8 scenario names and expect **all 8 to pass** given a
well-behaved LLM endpoint. A failing case names the scenario, the failing tier (robustness only),
the failing gate, and the exact `(role, scope)` pairs that diverged — see
[Description](#description) above for each suite's exact failure-message shape.

Because both suites' subject is LLM behavior itself, a failure is a genuine finding about the
configured LLM's determinism or phrasing-sensitivity for this class of decision, not necessarily a
scenario-authoring defect — the same caveat `policy-eval-scenarios.md`'s
[Further Notes](policy-eval-scenarios.md#further-notes) makes about its own adversarial scenarios
applies here across the board, since every case in both suites is, by construction, comparing an
LLM decision against a fixed oracle.

## Scenario

See [policy-eval-scenarios.md § Scenario](policy-eval-scenarios.md#scenario) for the eight
underlying scenario modules' full entity lists and role→access facts — this family adds no new
ground truth, it only re-exercises the existing one under repetition (consistency) and perturbation
(robustness). The eight `eval/scenarios_perturbed/scenario_eval_*_perturbed.py` modules are each a
reworded-description mirror of their corresponding original; see each perturbed module's own
docstring for exactly what was reworded, and `scenario_eval_agent_delegation_perturbed.py`'s
docstring specifically for the note on why its perturbed sibling lives under `eval/` while its
original does not.

## Configuration (env)

| Variable | Purpose |
|---|---|
| `LLM_BASE_URL` / `LLM_MODEL` / `LLM_API_KEY` | The only required variables — both suites call the PRB directly against a real LLM endpoint. |
| `AIAC_POLICY_FILE` | Set per test call (via `monkeypatch.setenv`), not from the environment — the consistency suite points it at the scenario's own unperturbed `policy.eval_<name>.md`; the robustness suite points it at a `tmp_path`-written mangled copy (mechanical tier) or the perturbed sibling's `policy.eval_<name>_perturbed.md` (semantic tier). |
| `PRB_CONSISTENCY_REPEATS` | Optional, consistency suite only. Number of repeat PRB runs per scenario. Default `5`. |

Neither suite reads `KEYCLOAK_URL`, `KEYCLOAK_ADMIN_USERNAME`/`PASSWORD`, `AIAC_PDP_CONFIG_URL`,
`AIAC_POLICY_STORE_URL`, `AIAC_PDP_POLICY_URL`, or `OPA_BIN` — see
[No-Keycloak design](#no-keycloak-design).

## Runbook

```bash
# Both suites need only LLM_BASE_URL/LLM_MODEL/LLM_API_KEY — no Keycloak/opa:
.venv/bin/pytest eval/test_policy_pipeline_consistency.py -m eval_consistency -v
.venv/bin/pytest eval/test_policy_pipeline_robustness.py -m eval_robustness -v

# Override repeat count for the consistency suite:
PRB_CONSISTENCY_REPEATS=10 .venv/bin/pytest eval/test_policy_pipeline_consistency.py \
  -m eval_consistency -v

# A pass/fail/skip/error report for the run is written alongside the policy-eval-scenarios one:
#   eval/reports/report_<DD_MM_HH_MM>.md (Asia/Jerusalem local time)
```

Both suites call `require_env("LLM_BASE_URL", "LLM_MODEL", "LLM_API_KEY")` as the first line of
each parametrized test function (not in a fixture) — matching `test_policy_pipeline_eval.py`'s
existing pattern, this raises `SystemExit(2)` (not a `pytest.skip`) if any is unset/empty.

## Test report

Reuses the exact report described in
[policy-eval-scenarios.md § Test report](policy-eval-scenarios.md#test-report), widened to also
collect `eval_consistency`/`eval_robustness`-marked tests
(`eval/conftest.py`'s `MARKERS` set now covers all three markers). Both new suites' tests fall
through to that report's generic docstring + crash-message rendering (neither
`record_property`s a per-cell description the way `test_inbound`/`test_outbound` do) — a single
docstring per parametrized test function already names exactly what's being checked, since neither
suite sweeps a per-cell matrix the way the heavy scenarios' `test_inbound`/`test_outbound` do.

## Testing Decisions

- **Reuse the existing corpus and helpers verbatim; add nothing scenario-specific to
  `test_policy_pipeline_eval.py`.** `SCENARIOS`, `orchestrate_prb`, `grant_sets`, `truth` are
  imported, not duplicated or modified — a change to the scenario corpus or to grant-set
  classification logic automatically applies to all three suites at once.
- **No Keycloak for either suite** — see [No-Keycloak design](#no-keycloak-design). This was a
  refinement over the original design-session proposal (which assumed the existing
  Keycloak-provisioning helpers would be reused as-is); confirmed during planning that
  `orchestrate_prb()` never reads anything Keycloak-specific off `Role`/`Scope`.
- **Consistency compares runs to each other, not to a truth table.** Whether the PRB is *correct*
  is `policy-eval-scenarios.md`'s job; this suite only asks whether it's *consistent* with itself.
  A scenario could in principle be consistently wrong (100% reproducible but incorrect) and this
  suite would report it as passing — that's by design, since correctness is a separate, already-
  covered concern.
- **Robustness compares each tier to the original scenario's truth, not to each other, and not to
  the unperturbed run's actual output.** Comparing tiers to each other would only prove
  "perturbation didn't change anything relative to itself," which is a weaker and less
  interesting claim than "the perturbed input still produces the *correct* decision."
- **Deterministic (no-RNG) mechanical perturbation.** `_mangle_text`/`_reordered` are pure
  functions of their input (word index modulo checks, not `random`), so a failing mechanical-tier
  case is exactly reproducible — no need to chase a seed or accept flakiness in the perturbation
  mechanism itself. Any observed variance is attributable entirely to the LLM call.
- **`agent_delegation`'s perturbed sibling lives under `eval/scenarios_perturbed/` despite its
  original living outside `eval/`.** Keeping all eight perturbed siblings in one directory (rather
  than mirroring the split-location convention `policy-eval-scenarios.md` uses for the originals)
  keeps `PERTURBED_SCENARIOS`' construction uniform and avoids inventing a second top-level
  perturbed-scenario file just to preserve an asymmetry that has no bearing on either new suite's
  logic.

## Relationship to other integration tests

This is **one** integration-test spec (covering two suites, 16 parametrized test cases total) among
several indexed by the master PRD ([../PRD.md](../PRD.md), § *Integration test specifications*).

- **Companion to, not a replacement for, [policy-eval-scenarios.md](policy-eval-scenarios.md).**
  That family proves correctness once per scenario; this family proves consistency and robustness
  of the same decisions, reusing its corpus and helpers unmodified.
- **Independent of [policy-pipeline.md](policy-pipeline.md) and
  [uc1-onboarding-pipeline.md](uc1-onboarding-pipeline.md).** Neither suite here touches Keycloak,
  the PCE, `opa`, or a live cluster — see [No-Keycloak design](#no-keycloak-design).
- **New markers, registered in `pyproject.toml`** (`eval_consistency`,
  `eval_robustness`), distinct from `integration`/`eval_extended`, so either suite
  can be invoked independently and its (lighter) infra requirement is visible from the marker name
  alone.

## Out of Scope

- **Any OPA/PCE/k8s pipeline stage.** Both suites stop at the PRB's raw `list[PolicyRule]` output —
  see [No-Keycloak design](#no-keycloak-design).
- **New scenarios.** Both suites reuse `policy-eval-scenarios.md`'s existing 8-scenario corpus
  as-is; adding a ninth scenario there automatically extends both suites here once the perturbed
  sibling for it is authored.
- **The two light guardrail scenarios (2, 5).** Those are `xfail`-pinned document-level rejection
  contracts, not grant-decision comparisons — neither "repeat N times" nor "perturb the input"
  is a meaningful operation on a whole-document-reject assertion, so they are not part of this
  family's corpus.
- **Statistical/majority-vote tolerance.** Both suites require exact equality; introducing a
  tolerance threshold (e.g. "passes if 4 of 5 runs agree") is a policy decision explicitly left for
  future work if today's exact-equality bar proves too strict in practice.
- **Default-CI wiring.** Both markers keep this family out of the default `-m "not integration"`
  unit run, matching every other suite indexed in this PRD section.

## Blocked-by

Same PRB prerequisites as [policy-eval-scenarios.md](policy-eval-scenarios.md#blocked-by)'s light
scenarios — the PRB entry points (`orchestrate_prb`, itself built on
`build_role_rules`/`build_scope_rules`) and a live LLM. No Keycloak, PCE, OPA, or Policy Store
dependency for either suite in this family.
