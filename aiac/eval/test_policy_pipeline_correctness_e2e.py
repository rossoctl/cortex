"""End-to-end correctness suite (spec: ``docs/specs/eval/policy-eval-correctness-e2e.md``).

Runs the same primary 8-scenario correctness corpus as
``test_policy_pipeline_correctness_prb.py`` (#2089), but through the **full** pipeline this
suite's sibling ``test_policy_pipeline_eval.py`` already drives — real Keycloak provisioning,
real Policy Rules Builder, real Policy Computation Engine, real ``opa eval`` against the rendered
Rego — instead of calling the PRB directly. Scores the result against each scenario's
hand-authored truth table using the same reusable, effect-aware ``eval.correctness_scorer`` the
PRB-level suite uses, unmodified.

Distinguished from ``test_policy_pipeline_correctness_prb.py`` (PRB-direct, no Keycloak/OPA/PCE in
the loop) and from ``test_policy_pipeline_eval.py``'s own ``test_grant_set_matches_truth_table``
(plain grant-set equality, no precision/recall, no awareness of ``PolicyRule.effect``). This is the
only level that can catch integration bugs between layers (PCE merge logic, Rego rendering, OPA
semantics) that a PRB-only check can't see.

Gate: zero-tolerance on over-grants (any over-granted pair in any gate fails the test).
Under-grants and incorrectly-denied pairs are reported via ``record_property`` and a printed
summary line, never gating — same philosophy as the PRB-level suite.

Known gap: the ``outbound_target`` gate's denial side (``AgentPolicyModel.outbound_target_deny_rules``)
is computed by the PCE but never rendered into the outbound Rego by
``aiac.pdp.service.policy.opa.rego.generate_outbound_rego`` — only the ALLOW side
(``agent_role_scopes``) is emitted. So for that one gate ``denied`` is always empty and its
``denial_precision`` reads vacuously ``1.0``; ``over_grants``/``under_grants`` for that gate are
unaffected (they depend only on ``granted``/``expected``). See
``docs/specs/eval/policy-eval-correctness-e2e.md`` for the full writeup — this is a pre-existing
production Rego-generator gap, not something this suite introduces or is scoped to fix.

Run (needs KEYCLOAK_URL + admin creds + LLM_* exported, ``opa`` on PATH). Unlike the PRB-level
suite, this one is NOT `-n`-parallelizable (all 8 scenarios share one session-scoped ``pipeline``
fixture) — but that fixture now parallelizes its own scenario provisioning internally via
``ProcessPoolExecutor`` (see ``eval/test_policy_pipeline_eval.py``'s module docstring), so `-n`
was never needed for wall-clock gains here:
    .venv/bin/pytest eval/test_policy_pipeline_correctness_e2e.py \
        -m eval_correctness_e2e -v -s
"""

from __future__ import annotations

import sys
from pathlib import Path
from types import ModuleType

import pytest

pytestmark = pytest.mark.eval_correctness_e2e

HERE = Path(__file__).resolve().parent  # aiac/eval/
REPO_ROOT = HERE.parent  # -> aiac/
SRC = REPO_ROOT / "src"
sys.path.insert(0, str(REPO_ROOT))  # so ``import test.integration.*``/``eval.*`` resolves
sys.path.insert(0, str(SRC))  # so ``import aiac.*`` resolves

from eval.correctness_e2e_helpers import _accumulate_agent_gates, _user_role_rows  # noqa: E402
from eval.correctness_scorer import score_scenario  # noqa: E402
from eval.test_policy_pipeline_eval import (  # noqa: E402
    SCENARIOS,
    _rego_path,
    _require_scenario,
    _user_role_names,
    opa_bin,
    opa_eval,
    truth,
)
from eval.test_policy_pipeline_eval import pipeline as pipeline  # noqa: E402,F401 - re-exported as a fixture
from test.integration.launcher import require_env  # noqa: E402

Pair = tuple[str, str]


# ======================================================================================
# opa eval wiring — thin, untested-in-isolation (same status as opa_eval itself, reused
# unmodified from eval.test_policy_pipeline_eval)
# ======================================================================================


def _rego_map(rego: Path, doc: str) -> dict[str, list[str]]:
    """Query one rendered Rego data document under ``data.authbridge.client.<doc>`` (e.g.
    ``inbound.request.subject_role_allow_scopes``) with no input — these are plain declarations,
    not decision rules, so they need none. ``{}`` when ``rego`` has no file on disk (an agent with
    no rendered rego for this direction — see ``_e2e_grant_sets``)."""
    if not rego.is_file():
        return {}
    return opa_eval([rego], f"data.authbridge.client.{doc}", {})


def _e2e_grant_sets(pipeline_result: dict, scenario: ModuleType) -> tuple[dict[str, set[Pair]], dict[str, set[Pair]]]:
    """Build the ``(granted, denied)`` gate dicts ``score_scenario`` expects, sourced from the
    rendered Rego of every agent in ``scenario`` (real Keycloak+PCE+OPA output, not the PRB's raw
    rules) — see the module docstring for the per-gate map table and the ``outbound_target``
    denial gap. Agents with no rego on disk (declared/emergent ``EXPECT_NO_REGO``) contribute
    nothing, same as ``test_inbound``/``test_outbound`` already handle it."""
    rego_dir = pipeline_result["rego_dir"]
    user_roles = _user_role_names(scenario)
    granted: dict[str, set[Pair]] = {}
    denied: dict[str, set[Pair]] = {}

    for agent_id in scenario.AGENTS:
        inbound_rego = _rego_path(rego_dir, agent_id, "inbound")
        outbound_rego = _rego_path(rego_dir, agent_id, "outbound")
        _accumulate_agent_gates(
            granted,
            denied,
            inbound_allow=_user_role_rows(
                _rego_map(inbound_rego, "inbound.request.subject_role_allow_scopes"), user_roles
            ),
            inbound_deny=_user_role_rows(
                _rego_map(inbound_rego, "inbound.request.subject_role_deny_scopes"), user_roles
            ),
            outbound_subject_allow=_user_role_rows(
                _rego_map(outbound_rego, "outbound.request.subject_role_allow_scopes"), user_roles
            ),
            outbound_subject_deny=_user_role_rows(
                _rego_map(outbound_rego, "outbound.request.subject_role_deny_scopes"), user_roles
            ),
            outbound_target_allow=_rego_map(outbound_rego, "outbound.request.agent_role_scopes"),
        )
    return granted, denied


# ======================================================================================
# The test
# ======================================================================================


@pytest.mark.parametrize("scenario_name", sorted(SCENARIOS))
def test_e2e_correctness(pipeline: dict[str, dict], scenario_name: str, record_property) -> None:
    """The full Keycloak+PCE+OPA pipeline's rendered grant/deny output, scored against the
    scenario's truth table, has zero over-grants (security-critical, gates this test) —
    under-grants and incorrect denials are tracked/reported only (spec: threshold TBD,
    deferred), same philosophy as the PRB-level suite."""
    require_env(
        "KEYCLOAK_URL",
        "KEYCLOAK_ADMIN_USERNAME",
        "KEYCLOAK_ADMIN_PASSWORD",
        "LLM_BASE_URL",
        "LLM_MODEL",
        "LLM_API_KEY",
    )
    opa_bin()  # skips cleanly if opa is not on PATH / OPA_BIN unset
    scenario = SCENARIOS[scenario_name]
    scenario_result = _require_scenario(pipeline, scenario_name)

    granted, denied = _e2e_grant_sets(scenario_result, scenario)
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
    print(
        f"[correctness-e2e] {scenario_name}: precision={score.precision:.3f} "
        f"recall={score.recall:.3f} denial_precision={score.denial_precision:.3f}\n"
        f"  over_grants={over_grants or '{}'}\n"
        f"  under_grants={under_grants or '{}'}\n"
        f"  incorrectly_denied={incorrectly_denied or '{}'}"
    )

    assert score.passed, (
        f"E2E pipeline over-granted for scenario '{scenario_name}' — zero-tolerance gate: {over_grants}"
    )
