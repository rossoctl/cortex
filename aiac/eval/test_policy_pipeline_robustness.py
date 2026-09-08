"""PRB robustness-to-perturbation suite (spec: ``docs/specs/eval/
policy-eval-robustness-consistency.md``).

Checks whether the LLM-backed Policy Rules Builder's grant decisions are unchanged under small,
meaning-preserving input perturbations — a vision-adversarial-robustness analogy applied to
access-control text. Two independent tiers, both checked per scenario:

1. **Mechanical** — a runtime, deterministic (no RNG) transform (``_mangle_text``) bundling
   whitespace/newline noise, casing noise, and punctuation noise, applied to the policy text and
   every candidate ``Role``/``Scope`` description, plus candidate-list reordering (``_reordered``).
2. **Semantic** — a hand-authored, meaning-preserving reworded sibling scenario module from
   ``eval/scenarios_perturbed/`` (different phrasing throughout, identical structure/ground truth).

Both variants' grant sets are compared against the *original* scenario's truth table (names are
guaranteed identical between a scenario and its perturbed sibling, so this comparison needs no
special-casing). Scoped to the PRB's raw output only — see ``test_policy_pipeline_consistency.py``
for the same no-Keycloak rationale, which applies here unchanged.

Run (needs LLM_BASE_URL/LLM_MODEL/LLM_API_KEY exported; no Keycloak/opa needed):
    .venv/bin/pytest eval/test_policy_pipeline_robustness.py \
        -m eval_robustness -v
"""

from __future__ import annotations

import sys
from pathlib import Path
from types import ModuleType, SimpleNamespace

import pytest

pytestmark = pytest.mark.eval_robustness

HERE = Path(__file__).resolve().parent  # aiac/eval/
REPO_ROOT = HERE.parent  # -> aiac/
SRC = REPO_ROOT / "src"
sys.path.insert(0, str(REPO_ROOT))  # so ``import test.integration.*``/``eval.*`` resolves
sys.path.insert(0, str(SRC))  # so ``import aiac.*`` resolves

from eval.prb_direct import build_roles_and_scopes  # noqa: E402
from eval.scenarios_perturbed import (  # noqa: E402
    scenario_eval_agent_delegation_perturbed,
    scenario_eval_ambiguous_clause_perturbed,
    scenario_eval_baseline_perturbed,
    scenario_eval_confusable_agents_perturbed,
    scenario_eval_empty_descriptions_perturbed,
    scenario_eval_misleading_descriptions_perturbed,
    scenario_eval_unreachable_resources_perturbed,
    scenario_eval_wildcard_grant_perturbed,
)
from eval.test_policy_pipeline_eval import (  # noqa: E402
    SCENARIOS,
    grant_sets,
    orchestrate_prb,
    truth,
)
from test.integration.launcher import require_env  # noqa: E402

PERTURBED_SCENARIOS: dict[str, ModuleType] = {
    "baseline": scenario_eval_baseline_perturbed,
    "agent_delegation": scenario_eval_agent_delegation_perturbed,
    "unreachable_resources": scenario_eval_unreachable_resources_perturbed,
    "ambiguous_clause": scenario_eval_ambiguous_clause_perturbed,
    "wildcard_grant": scenario_eval_wildcard_grant_perturbed,
    "misleading_descriptions": scenario_eval_misleading_descriptions_perturbed,
    "confusable_agents": scenario_eval_confusable_agents_perturbed,
    "empty_descriptions": scenario_eval_empty_descriptions_perturbed,
}


def _mangle_text(text: str) -> str:
    """One deterministic pure-function bundle of whitespace/newline noise, casing noise (every
    3rd word forced upper, every 5th forced lower, by word index), and punctuation noise (spaced
    out sentence/list punctuation). Deterministic by construction (word index, not randomness) so
    re-running this suite is itself perfectly reproducible."""
    words = text.split(" ")
    noisy_words = []
    for i, word in enumerate(words):
        if word and i % 3 == 0:
            word = word.upper()
        elif word and i % 5 == 0:
            word = word.lower()
        noisy_words.append(word)
    mangled = "  ".join(noisy_words)
    mangled = mangled.replace(".", " . ").replace(",", " , ")
    mangled = mangled.replace("\n", "\n\n   ")
    return mangled


def _reverse_dict(d: dict) -> dict:
    return dict(reversed(list(d.items())))


def _reordered(scenario: ModuleType) -> SimpleNamespace:
    """A view of ``scenario`` with every candidate list's dict-iteration order reversed
    (``USER_ROLES``, ``AGENTS`` and each agent's ``inbound_scopes``/``delegation_scopes``/``roles``,
    ``TOOLS`` and each tool's ``scopes``), so ``orchestrate_prb`` sees candidates in reordered
    order with zero production-code changes. Name-keyed pair lists are order-insensitive
    (``grant_sets``/``truth`` compare them as sets) so they're copied through unchanged."""
    agents = {
        agent_id: {
            **agent,
            "inbound_scopes": _reverse_dict(agent["inbound_scopes"]),
            "delegation_scopes": _reverse_dict(agent.get("delegation_scopes", {})),
            "roles": _reverse_dict(agent["roles"]),
        }
        for agent_id, agent in reversed(list(scenario.AGENTS.items()))
    }
    tools = {
        tool_id: {**tool, "scopes": _reverse_dict(tool["scopes"])}
        for tool_id, tool in reversed(list(scenario.TOOLS.items()))
    }
    return SimpleNamespace(
        REALM_DEFAULT=scenario.REALM_DEFAULT,
        POLICY_FILE=scenario.POLICY_FILE,
        AGENTS=agents,
        TOOLS=tools,
        USERS=dict(scenario.USERS),
        USER_PASSWORD=scenario.USER_PASSWORD,
        USER_ROLES=_reverse_dict(scenario.USER_ROLES),
        INBOUND_PAIRS=list(scenario.INBOUND_PAIRS),
        OUTBOUND_PAIRS=list(scenario.OUTBOUND_PAIRS),
        OUTBOUND_SUBJECT_PAIRS=list(scenario.OUTBOUND_SUBJECT_PAIRS),
    )


@pytest.mark.parametrize("scenario_name", sorted(SCENARIOS))
def test_prb_robust_to_perturbation(
    scenario_name: str, monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """Check the PRB's grant decision is unchanged under (1) a mechanical perturbation of the
    policy text, candidate descriptions, and candidate-list order, and (2) a hand-reworded
    semantic-sibling scenario with identical meaning/structure — both compared against the
    original scenario's truth table. A single combined pass/fail per scenario; if mechanical
    passes but semantic fails (or vice versa), the scenario reports as robustness-failed overall."""
    require_env("LLM_BASE_URL", "LLM_MODEL", "LLM_API_KEY")
    scenario = SCENARIOS[scenario_name]
    want = truth(scenario)
    failures: list[str] = []

    # --- mechanical tier ---
    roles, scopes = build_roles_and_scopes(scenario)
    mech_roles = {
        name: role.model_copy(update={"description": _mangle_text(role.description or "")})
        for name, role in roles.items()
    }
    mech_scopes = {
        name: scope.model_copy(update={"description": _mangle_text(scope.description or "")})
        for name, scope in scopes.items()
    }
    policy_path = Path(scenario.__file__).resolve().parent / scenario.POLICY_FILE
    mech_policy_path = tmp_path / f"{scenario_name}.mechanical.md"
    mech_policy_path.write_text(_mangle_text(policy_path.read_text(encoding="utf-8")))
    monkeypatch.setenv("AIAC_POLICY_FILE", str(mech_policy_path))
    mech_rules, _, _, _ = orchestrate_prb(mech_roles, mech_scopes, _reordered(scenario))
    mech_got = grant_sets(scenario, mech_rules)
    for gate in ("inbound", "outbound_subject", "outbound_target"):
        diff = want[gate] ^ mech_got[gate]
        if diff:
            failures.append(f"mechanical tier, gate={gate}: mismatching pairs={sorted(diff)}")

    # --- semantic tier ---
    perturbed = PERTURBED_SCENARIOS[scenario_name]
    p_roles, p_scopes = build_roles_and_scopes(perturbed)
    p_policy_path = Path(perturbed.__file__).resolve().parent / perturbed.POLICY_FILE
    monkeypatch.setenv("AIAC_POLICY_FILE", str(p_policy_path))
    sem_rules, _, _, _ = orchestrate_prb(p_roles, p_scopes, perturbed)
    sem_got = grant_sets(scenario, sem_rules)
    for gate in ("inbound", "outbound_subject", "outbound_target"):
        diff = want[gate] ^ sem_got[gate]
        if diff:
            failures.append(f"semantic tier, gate={gate}: mismatching pairs={sorted(diff)}")

    assert not failures, (
        f"PRB was not robust to perturbation for scenario '{scenario_name}':\n"
        + "\n".join(failures)
    )
