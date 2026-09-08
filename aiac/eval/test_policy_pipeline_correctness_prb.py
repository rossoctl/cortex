"""PRB-level correctness suite (spec: ``docs/specs/eval/policy-eval-correctness-prb.md``).

Runs the Policy Rules Builder directly (synthetic, Keycloak-free ``Role``/``Scope`` objects via
``eval.prb_direct.build_roles_and_scopes`` — no live IdP, no OPA) against the primary
8-scenario correctness corpus (``eval.test_policy_pipeline_eval.SCENARIOS``) and scores its raw
``list[PolicyRule]`` output against each scenario's hand-authored truth table using the shared,
reusable ``eval.correctness_scorer`` — precision and recall, tracked separately, plus a
denial-precision figure for the PRB's explicit ``Deny`` rules.

Distinguished from ``test_policy_pipeline_eval.py``'s own ``test_grant_set_matches_truth_table``
(which also checks grant-set equality, but downstream of the full Keycloak+OPA pipeline, and
without effect-aware denial tracking or a reusable scorer) and from the future end-to-end
correctness suite (#2090, ``test_policy_pipeline_correctness_e2e.py`` /
``eval_correctness_e2e`` — same corpus and scorer, but through the real Keycloak+OPA pipeline).

Scoped to the PRB's raw output only (no OPA/PCE/k8s in the loop) — same no-Keycloak rationale as
``test_policy_pipeline_consistency.py``/``test_policy_pipeline_robustness.py``, which this suite
otherwise mirrors structurally.

Gate: zero-tolerance on over-grants (any over-granted pair in any gate fails the test).
Under-grants and incorrectly-denied pairs are reported via ``record_property`` and a printed
summary line, never gating (spec: under-grant threshold TBD, deferred).

Calls ``orchestrate_prb(..., best_effort=True)``: a scope/role decision the PRB's auditor rejects
(``PolicyContradictionError``/exhausted-retry ``PolicyRulesBuilderError``) contributes a
best-effort, never-approved fallback rule instead of aborting the whole scenario — by explicit
user request, so every scenario scores instead of one rejected decision hiding the rest. This
means a scored pair may not represent what a real deployment would ever contain;
``best_effort_notes`` (``record_property``'d, printed) names exactly which scope/role decisions
this applies to. See ``eval.test_policy_pipeline_eval.orchestrate_prb``'s docstring for the
mechanism.

Run (needs LLM_BASE_URL/LLM_MODEL/LLM_API_KEY exported; no Keycloak/opa needed). The 8 scenarios
are fully independent (separate synthetic Role/Scope, separate AIAC_POLICY_FILE), so always run
with ``-n 8`` for a near-linear wall-clock speedup — ``orchestrate_prb()`` makes ~5-8 sequential
LLM calls per scenario, so a plain sequential run is dominated by LLM round-trip latency (a real
run took ~36 minutes; with ``-n 8`` it's close to 1/8th that). ``pytest-xdist`` is a declared
`test`-extra dependency (``aiac/pyproject.toml``'s ``[project.optional-dependencies].test``)
picked up by a normal ``uv sync``/``pip install -e ".[test]"`` — no separate install step needed:
    .venv/bin/pytest eval/test_policy_pipeline_correctness_prb.py \
        -m eval_correctness_prb -n 8 -v -s
"""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

pytestmark = pytest.mark.eval_correctness_prb

HERE = Path(__file__).resolve().parent  # aiac/eval/
REPO_ROOT = HERE.parent  # -> aiac/
SRC = REPO_ROOT / "src"
sys.path.insert(0, str(REPO_ROOT))  # so ``import test.integration.*``/``eval.*`` resolves
sys.path.insert(0, str(SRC))  # so ``import aiac.*`` resolves

from aiac.policy.model.models import RuleEffect  # noqa: E402
from eval.correctness_scorer import score_scenario  # noqa: E402
from eval.prb_direct import build_roles_and_scopes  # noqa: E402
from eval.test_policy_pipeline_eval import (  # noqa: E402
    SCENARIOS,
    grant_sets,
    orchestrate_prb,
    truth,
)
from test.integration.launcher import require_env  # noqa: E402


@pytest.mark.parametrize("scenario_name", sorted(SCENARIOS))
def test_prb_correctness(scenario_name: str, monkeypatch: pytest.MonkeyPatch, record_property) -> None:
    """The PRB's grant/deny output, scored against the scenario's truth table, has zero
    over-grants (security-critical, gates this test) — under-grants and incorrect denials are
    tracked/reported only (spec: threshold TBD, deferred)."""
    require_env("LLM_BASE_URL", "LLM_MODEL", "LLM_API_KEY")
    scenario = SCENARIOS[scenario_name]
    roles, scopes = build_roles_and_scopes(scenario)
    policy_path = Path(scenario.__file__).resolve().parent / scenario.POLICY_FILE
    monkeypatch.setenv("AIAC_POLICY_FILE", str(policy_path))

    # best_effort=True: a scope/role decision the auditor rejects contributes a best-effort
    # (never-approved) fallback rule instead of aborting the whole scenario — see
    # orchestrate_prb's docstring. Deliberate, by explicit user request, so every scenario scores
    # instead of the whole thing showing "setup failed"; best_effort_notes names exactly which
    # scope/role decisions this applies to, reported below so a reader knows which of this
    # scenario's numbers don't represent real production behavior.
    rules, _, _, best_effort_notes = orchestrate_prb(roles, scopes, scenario, best_effort=True)
    granted = grant_sets(scenario, [r for r in rules if r.effect == RuleEffect.ALLOW])
    denied = grant_sets(scenario, [r for r in rules if r.effect == RuleEffect.DENY])
    expected = truth(scenario)
    score = score_scenario(scenario_name, granted, denied, expected)

    over_grants = {g: sorted(p) for g, p in score.over_grants.items()}
    under_grants = {g: sorted(p) for g, p in score.under_grants.items()}
    incorrectly_denied = {g: sorted(p) for g, p in score.incorrectly_denied.items()}

    record_property("precision", score.precision)
    record_property("recall", score.recall)
    record_property("denial_precision", score.denial_precision)
    record_property("over_grants", over_grants)
    record_property("under_grants", under_grants)
    record_property("incorrectly_denied", incorrectly_denied)
    record_property("best_effort_notes", best_effort_notes)
    print(
        f"[correctness] {scenario_name}: precision={score.precision:.3f} "
        f"recall={score.recall:.3f} denial_precision={score.denial_precision:.3f}\n"
        f"  over_grants={over_grants or '{}'}\n"
        f"  under_grants={under_grants or '{}'}\n"
        f"  incorrectly_denied={incorrectly_denied or '{}'}\n"
        f"  best_effort_notes={best_effort_notes or '{}'}"
    )

    assert score.passed, f"PRB over-granted for scenario '{scenario_name}' — zero-tolerance gate: {over_grants}"
