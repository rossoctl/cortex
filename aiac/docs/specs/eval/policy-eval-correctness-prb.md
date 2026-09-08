# Eval Spec: policy-eval-correctness-prb — `test_policy_pipeline_correctness_prb.py`

> **One spec among several.** This document specifies **one** integration test.
> Eval specs live **one spec per test** under `docs/specs/eval/`
> (a sibling of `components/`), and the master PRD's *Integration test specifications* section
> ([../PRD.md](../PRD.md)) is the index of them. This is a **companion to**, not a replacement
> for, [policy-eval-scenarios.md](policy-eval-scenarios.md) and
> [policy-eval-robustness-consistency.md](policy-eval-robustness-consistency.md): all three
> families reuse the same eight-scenario corpus (`SCENARIOS`, `orchestrate_prb`, `grant_sets`,
> `truth`, from `eval/test_policy_pipeline_eval.py`), unmodified, but each isolates a different
> property of the PRB's grant decisions — correctness (this family), consistency, and robustness.

## Location

- `aiac/eval/correctness_scorer.py` — the reusable scorer: `score_gate`/`score_scenario`,
  `GateScore`/`ScenarioScore`. Pure logic, no I/O, no LLM — generic over any caller's own
  `granted`/`denied`/`expected` gate dicts, not PRB-specific (see
  [Scorer design](#scorer-design)).
- `aiac/eval/test_correctness_scorer.py` — unmarked unit tests for the scorer (runs in the
  default fast pass; `testpaths` already includes `eval/`).
- `aiac/eval/test_policy_pipeline_correctness_prb.py` — the suite itself,
  `@pytest.mark.eval_correctness_prb`.
- Reuses `aiac/eval/prb_direct.py`'s `build_roles_and_scopes` (the same no-Keycloak,
  synthetic-`Role`/`Scope` builder `policy-eval-robustness-consistency.md`'s two suites use) and
  imports `SCENARIOS`, `orchestrate_prb`, `grant_sets`, `truth` from
  `eval.test_policy_pipeline_eval` unmodified.
- `aiac/eval/best_effort_rules.py` — the best-effort fallback's own pure-logic helper
  (`_best_effort_rules`, called from `orchestrate_prb`/`_invoke_graph`); `aiac/eval/
  test_best_effort_rules.py` — its unmarked unit tests. See [Best-effort
  proposals](#best-effort-proposals).

## Description

`policy-eval-scenarios.md`'s heavy scenarios already check grant-set equality against the truth
table (`test_grant_set_matches_truth_table`), but downstream of the full Keycloak+PCE+OPA pipeline,
via a coarse set-equality assertion with no precision/recall breakdown and no awareness of
`PolicyRule.effect` (a `Deny` rule is invisible to that check — it neither helps nor hurts a set
comparison keyed only on `(role, scope)`). This suite adds a **PRB-level, effect-aware** scoring
pass over the same corpus:

1. **PRB-direct, no Keycloak/OPA/k8s.** Same no-Keycloak design as
   [policy-eval-robustness-consistency.md](policy-eval-robustness-consistency.md#no-keycloak-design):
   synthetic `Role`/`Scope` via `prb_direct.build_roles_and_scopes`, `orchestrate_prb()` called
   directly. Needs only `LLM_BASE_URL`/`LLM_MODEL`/`LLM_API_KEY`.
2. **Effect-aware.** The PRB's rules are split by `PolicyRule.effect` before classification:
   `ALLOW` rules go through `grant_sets()` to build `granted`; `DENY` rules go through the same
   `grant_sets()` call to build a separate `denied` dict. `truth(scenario)` remains the `expected`
   oracle, unchanged.
3. **Precision and recall, tracked separately, never blended** — per gate and aggregated across
   all three gates (`inbound`/`outbound_subject`/`outbound_target`), via the shared
   `correctness_scorer` (see [Scorer design](#scorer-design)).
4. **A tracked, non-gating denial-precision figure** for the PRB's explicit `Deny` rules — see
   [Denial precision](#denial-precision).
5. **Zero-tolerance over-grant gate; under-grants and incorrect denials are reported only.**

## Scorer design

`correctness_scorer.py` is deliberately generic over gate-classified pair sets, not PRB-specific:

```python
def score_gate(gate: str, granted: set[tuple[str, str]], denied: set[tuple[str, str]],
               expected: set[tuple[str, str]]) -> GateScore: ...
def score_scenario(scenario: str, granted: dict[str, set], denied: dict[str, set],
                    expected: dict[str, set]) -> ScenarioScore: ...
```

`score_gate` classifies one gate's three input sets into:

| Field | Definition | Meaning |
|---|---|---|
| `true_positives` | `granted & expected` | Correctly granted |
| `over_grants` | `granted - expected` | False positive — security-critical |
| `under_grants` | `expected - granted` | False negative — availability, not gated |
| `correctly_denied` | `denied - expected` | An explicit `Deny` for a pair that should NOT be granted |
| `incorrectly_denied` | `denied & expected` | An explicit `Deny` for a pair that SHOULD be granted — always a subset of `under_grants` |

...plus `precision` (`TP/(TP+FP)`, vacuous `1.0` when `granted` is empty), `recall`
(`TP/(TP+FN)`, vacuous `1.0` when `expected` is empty), and `denial_precision`
(`|correctly_denied|/|denied|`, vacuous `1.0` when `denied` is empty).

`score_scenario` scores every gate present across the three input dicts and aggregates
precision/recall/denial-precision across each gate's pairs **unioned together** (not averaged
per-gate) — so a scenario with an uneven pair count per gate isn't skewed by treating every gate
as equally weighted. `ScenarioScore.passed` is `True` iff no gate has any `over_grants` —
zero-tolerance, matching the spec's own security-first philosophy (an over-grant is a genuine
security defect; an under-grant is, at worst, an availability defect, and the spec's own
threshold for tolerating those is still TBD/deferred).

This module is designed to be reused, unmodified, by the future end-to-end correctness suite
(#2090) — that suite need only build its own `granted`/`denied`/`expected` dicts from whatever it
observes downstream of Keycloak+OPA and hand them to the same `score_scenario`.

## Denial precision

The PRB genuinely emits explicit `Deny` rules (`aiac.agent.policy_rules_builder.graph`'s
`build_scope_graph`/`build_role_graph` — the same graphs `orchestrate_prb()` invokes). Before this
suite, nothing in the eval corpus tracked them separately: `grant_sets()` (reused as-is, both
here and in every sibling suite) classifies by `(role, scope)` name alone, blind to
`PolicyRule.effect` — so a `Deny(role, scope)` rule would silently count as though it were granted
in any set-equality check that doesn't also filter by effect first.

This suite filters `ALLOW` and `DENY` rules into two separate `grant_sets()` calls before scoring,
so a `Deny` on an unexpected pair is now visible as `correctly_denied` (the mechanism doing its
job) and a `Deny` on an expected pair is visible as `incorrectly_denied` (still a subset of
`under_grants` — reported, not gated, per the philosophy above: an incorrect denial is just a
more precisely diagnosed under-grant, not a new failure class). `denial_precision` is reported per
scenario and per gate via `record_property`, never blended into grant precision/recall, and never
gates the test.

## Best-effort proposals

The PRB's generate→audit loop (`aiac.agent.policy_rules_builder.graph`'s `_audit`) can reject a
scope/role decision outright — a genuine contradiction (`PolicyContradictionError`, fails closed
immediately) or an exhausted retry budget (`PolicyRulesBuilderError`, after `MAX_AUDIT_RETRIES=3`).
By default this aborts `orchestrate_prb()` entirely, discarding every other decision's
already-approved rules along with it — one bad scope used to mean the whole scenario showed
"setup failed" with no precision/recall at all.

This suite calls `orchestrate_prb(..., best_effort=True)`: a rejected decision instead falls back
to whatever was last proposed (before the auditor rejected it) — a real, if never-approved, guess
at the grant/deny set for that one scope/role, built the same way the PRB's own `build` node would
have (`aiac.agent.policy_rules_builder.graph._denied_names` reused; see
`eval.best_effort_rules._best_effort_rules`, unit-tested unmarked in
`eval/test_best_effort_rules.py`). Every other decision in the scenario proceeds normally.

**This is an explicit, user-requested tradeoff, not free lunch**: a best-effort pair may not
represent what a real deployment would ever contain — the auditor rejected it for a reason, and a
real pipeline run would never emit it. `record_property("best_effort_notes", ...)` — a
`{scope_or_role_name: reason}` dict — and the printed summary line name exactly which decisions
this applies to, so a reader can tell which numbers are "real" and which are best-effort. The
report (`eval/conftest.py`) renders this as an extra field with an explicit caveat whenever
non-empty. `eval_consistency`/`eval_robustness` keep `best_effort=False` (the default, at their
own direct `orchestrate_prb` call sites) — a rejected decision still aborts their scenario/repeat
as before. `eval_extended` is the one exception: it shares the same session-scoped `pipeline`
fixture `test_e2e_correctness` uses, and that fixture's `orchestrate_prb` call hardcodes
`best_effort=True` unconditionally (confirmed with the user — see
[policy-eval-correctness-e2e.md § Best-effort
proposals](policy-eval-correctness-e2e.md#best-effort-proposals) and
`eval.test_policy_pipeline_eval`'s module docstring for why this couldn't cleanly be made
e2e-only), so `eval_extended`'s own per-cell tests are affected too, not just this suite or
`test_e2e_correctness`.

## Expected output

Parametrized over all 8 scenario names (`sorted(SCENARIOS)`); expects **all 8 to pass** (zero
over-grants) given a well-behaved LLM endpoint. Each test case `record_property`s `precision`,
`recall`, `denial_precision`, `over_grants`, `under_grants`, `incorrectly_denied` (each of the
latter three as `{gate: sorted(pairs)}`), and `best_effort_notes` (see [Best-effort
proposals](#best-effort-proposals)), and prints a one-line summary:

```text
[correctness] wildcard_grant: precision=1.000 recall=1.000 denial_precision=1.000
```

A failing case's assertion message names the scenario and the exact over-granted `(role, scope)`
pairs per gate.

## Taxonomy cross-check

Issue #2089's originating epic (#2087) references a legacy adversarial-authoring taxonomy:
ambiguity resolution, wildcard expansion, adversarial/misleading naming, empty descriptions,
identity/boundary confusion, delegation, prompt injection, and direct contradiction. No new
scenario was authored for this ticket — the taxonomy maps onto the existing corpus as follows:

| Taxonomy theme | Covered by |
|---|---|
| Ambiguity resolution | `ambiguous_clause` |
| Wildcard expansion | `wildcard_grant` |
| Adversarial / misleading naming | `misleading_descriptions` |
| Empty descriptions | `empty_descriptions` |
| Identity / boundary confusion | `confusable_agents` |
| Delegation | `agent_delegation` |
| Prompt injection | Scenario 5 (`test_guardrail_rejects_prompt_injection_document`) |
| Direct contradiction | Scenario 2 (`test_guardrail_rejects_direct_grant_revoke_contradiction`) |

`baseline` and `unreachable_resources` round out the 8 `SCENARIOS` as non-taxonomy, fresh-
derivation additions (a clean regression baseline and an emergent-unreachability probe,
respectively) — see [policy-eval-scenarios.md § Scenario](policy-eval-scenarios.md#scenario) for
their full entity lists.

Prompt injection and direct contradiction are **not** part of this suite's own parametrization:
both are whole-document `xfail` rejection contracts (the PRB refuses to produce any rules at all
for the document), and precision/recall is structurally inapplicable to a rejection with no
partial grant set to score — there is nothing for `score_scenario` to compare. Those two remain
covered exactly as `policy-eval-scenarios.md` already documents them
(`test_guardrail_conflicts.py`/`test_guardrail_injection.py`, `@pytest.mark.integration`,
`xfail`-pinned).

## Policy-text rewrite

As part of this ticket, all 8 scenarios' policy `.md` files (`eval/scenarios/policy.eval_*.md`,
`test/integration/policy.eval_agent_delegation.md`) and their 8 `eval/scenarios_perturbed/`
semantic siblings were rewritten to read like real human-authored access-control policy rather
than the pipeline's own internal vocabulary reflected back into the document a policy author would
supposedly have written. The previous text opened with the literal internal-modeling phrase
`"(role, scope) pair"` and organized every scenario under three headers named for the pipeline's
own three gates (`Users → agent capabilities (inbound...)`, etc.) — exposing implementation
structure in a document that, in a real deployment, a human policy author writes without any
notion of "gates" at all. Nouns were also generic ("a resource agent", "a device") rather than
scenario-domain-concrete ("the inventory agent", "irrigation valves") — a real signal loss for
`empty_descriptions` specifically, since every `Role`/`Scope` description there is deliberately
`""`, leaving the policy text as the only semantic signal available to the PRB.

The rewrite collapses each scenario to the fewest natural sentences that state every distinct
fact once — no header labels, no restating the same fact three ways just because the pipeline
internally has three gates. When a scenario's user-facing access and the agent's own capability
are identical (the common case), one sentence covers all three gates and the PRB is trusted to
derive inbound + outbound-subject + outbound-target from it. Scenarios where the human's access
and the agent's own capability genuinely diverge (`ambiguous_clause`) or where two roles'
real access differs even though their agent access looks similar (`misleading_descriptions`,
`agent_delegation`) still get multiple sentences — that's a real fact, not pipeline-structure
mirroring.

**No `.py` file changed as part of this rewrite.** `INBOUND_PAIRS`/`OUTBOUND_PAIRS`/
`OUTBOUND_SUBJECT_PAIRS` truth tables, agent/tool/scope ids, and the `AGENTS`/`TOOLS`/
`USER_ROLES` description dicts are untouched — only the natural-language `.md` policy text
changed. This is shared infrastructure: the rewritten `.md` files are read by this suite,
`policy-eval-scenarios.md`'s heavy scenarios, and both of
`policy-eval-robustness-consistency.md`'s suites (`eval_extended`/`eval_consistency`/
`eval_robustness`) alike, since all four families read the same `policy.eval_*.md` files off disk
via `AIAC_POLICY_FILE`. The regression risk of the rewrite — that a reworded, header-free document
might produce different grant decisions than the old mechanical-template text did — is exactly
what running those other suites against the rewritten text checks (see
[Runbook](#runbook)).

## Configuration (env)

| Variable | Purpose |
|---|---|
| `LLM_BASE_URL` / `LLM_MODEL` / `LLM_API_KEY` | The only required variables — the suite calls the PRB directly against a real LLM endpoint. |
| `AIAC_POLICY_FILE` | Set per test call (via `monkeypatch.setenv`), pointed at the scenario's own `policy.eval_<name>.md`. |

No `KEYCLOAK_URL`, Keycloak admin creds, `AIAC_PDP_CONFIG_URL`/`AIAC_POLICY_STORE_URL`/
`AIAC_PDP_POLICY_URL`, or `OPA_BIN` are read — see
[policy-eval-robustness-consistency.md § No-Keycloak design](policy-eval-robustness-consistency.md#no-keycloak-design),
which applies here unchanged.

## Runbook

```bash
# TDD the scorer first (pure logic, no live infra, runs in the default fast pass):
.venv/bin/pytest eval/test_correctness_scorer.py -v

# The suite itself — needs only LLM_BASE_URL/LLM_MODEL/LLM_API_KEY, no Keycloak/opa:
.venv/bin/pytest eval/test_policy_pipeline_correctness_prb.py -m eval_correctness_prb -v -s

# Regression-check the rewritten policy text against the suites that already depend on this
# corpus (the real risk of the rewrite — that reworded text still produces the same decisions):
.venv/bin/pytest eval/test_policy_pipeline_consistency.py -m eval_consistency -v
.venv/bin/pytest eval/test_policy_pipeline_robustness.py -m eval_robustness -v
```

Like every sibling suite, `require_env("LLM_BASE_URL", "LLM_MODEL", "LLM_API_KEY")` is the first
line of the parametrized test function, raising `SystemExit(2)` if any is unset/empty.

## Test report

Widens `eval/conftest.py`'s Markdown report described in
[policy-eval-scenarios.md § Test report](policy-eval-scenarios.md#test-report) to also collect
`eval_correctness_prb`-marked tests (`eval/conftest.py`'s `MARKERS` set now covers all four
markers). Unlike `eval_consistency`/`eval_robustness` (which fall through to that report's generic
docstring + crash-message rendering), `test_prb_correctness` gets its **own** render branch: each
entry shows precision, recall, and denial precision, plus the over-grants/under-grants/
incorrectly-denied pair breakdown per gate — always, pass or fail, since the tracked-but-non-gating
under-grant/incorrect-denial detail (the whole point of this suite over a plain grant-set-equality
check) is otherwise invisible on a passing run. The same detail is also printed unconditionally to
stdout per scenario (`-s`) for live inspection without waiting on the written report file.

## Relationship to other integration tests

This is **one** integration-test spec among several indexed by the master PRD
([../PRD.md](../PRD.md), § *Integration test specifications*).

- **Companion to, not a replacement for, [policy-eval-scenarios.md](policy-eval-scenarios.md) and
  [policy-eval-robustness-consistency.md](policy-eval-robustness-consistency.md).** All three
  reuse the same 8-scenario corpus and PRB entry points; each isolates a different property
  (correctness with a precision/recall/denial-precision breakdown here, vs. plain grant-set
  equality downstream of the full pipeline in the former, vs. consistency/robustness in the
  latter).
- **New marker, registered in `pyproject.toml`** (`eval_correctness_prb`), distinct from
  `eval_extended`/`eval_consistency`/`eval_robustness`, named deliberately to leave room for a
  future `eval_correctness_e2e` marker (#2090) without ambiguity between the two correctness
  suites' infra requirements.

## Out of Scope

- **A committed trend log** (tracking precision/recall/denial-precision across runs over time, as
  opposed to the per-run Markdown report — see [Test report](#test-report), which **is** wired
  in). Deferred to #2091.
- **End-to-end (Keycloak+OPA) correctness scoring.** Deferred to #2090 — `correctness_scorer.py`
  is designed to be reusable there; the wiring itself is not this ticket's scope.
- **An under-grant tolerance threshold.** Per the originating spec, still TBD — under-grants are
  tracked/reported via `record_property` and the printed summary line, never gated.
- **New scenarios.** The taxonomy cross-check above confirms the existing 8-scenario corpus
  already covers every taxonomy theme; none is needed.
- **Fixing the PRB audit/retry-convergence bug** that causes the auditor to reject a scope/role
  decision (`PolicyContradictionError`/`PolicyRulesBuilderError`) for a variable subset of
  scenarios depending on LLM sampling — see [Best-effort proposals](#best-effort-proposals), which
  changes how a rejection is *reported*, not the underlying bug. Already deferred by the user as a
  separate follow-up to `aiac.agent.policy_rules_builder.graph._audit` — not this ticket's job.

## Blocked-by

Same PRB prerequisites as
[policy-eval-robustness-consistency.md](policy-eval-robustness-consistency.md#blocked-by) — the
PRB entry points (`orchestrate_prb`, itself built on `build_role_rules`/`build_scope_rules`) and a
live LLM. No Keycloak, PCE, OPA, or Policy Store dependency.
