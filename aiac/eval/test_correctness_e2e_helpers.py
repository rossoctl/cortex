"""Unit tests for ``correctness_e2e_helpers.py`` (spec: ``docs/specs/eval/
policy-eval-correctness-e2e.md``).

Pure-logic, unmarked — runs in the default fast pass (``testpaths`` already includes ``eval/``),
mirroring ``test_correctness_scorer.py``'s status as a separate, unmarked file. No LLM, no
Keycloak, no ``opa``, no fixtures beyond plain dicts/sets — the module-level
``pytestmark = pytest.mark.eval_correctness_e2e`` in ``test_policy_pipeline_correctness_e2e.py``
would otherwise wrongly sweep these up and exclude them from the default pass.
"""

from __future__ import annotations

from eval.correctness_e2e_helpers import _accumulate_agent_gates, _pairs_from_map, _user_role_rows


def test_pairs_from_map_flattens_role_scope_map() -> None:
    got = _pairs_from_map({"role-a": ["scope-x", "scope-y"], "role-b": ["scope-x"]})
    assert got == {("role-a", "scope-x"), ("role-a", "scope-y"), ("role-b", "scope-x")}


def test_pairs_from_map_empty_map_yields_no_pairs() -> None:
    assert _pairs_from_map({}) == set()


def test_user_role_rows_drops_agent_role_rows() -> None:
    rows = {
        "user-role-inventory-manager": ["tool-scope-inventory-check"],
        "agent-role-inventory-operations": ["tool-scope-inventory-check"],
    }
    got = _user_role_rows(rows, user_roles={"user-role-inventory-manager"})
    assert got == {"user-role-inventory-manager": ["tool-scope-inventory-check"]}


def test_user_role_rows_empty_map_yields_empty_map() -> None:
    assert _user_role_rows({}, user_roles={"user-role-x"}) == {}


def test_accumulate_agent_gates_classifies_into_three_buckets() -> None:
    granted: dict[str, set[tuple[str, str]]] = {}
    denied: dict[str, set[tuple[str, str]]] = {}

    _accumulate_agent_gates(
        granted,
        denied,
        inbound_allow={"user-role-developer": ["agent-scope-repo-access"]},
        inbound_deny={"user-role-devops": ["agent-scope-repo-access"]},
        outbound_subject_allow={"user-role-developer": ["tool-scope-repo-read"]},
        outbound_subject_deny={},
        outbound_target_allow={"agent-role-repo-operations": ["tool-scope-repo-read"]},
    )

    assert granted == {
        "inbound": {("user-role-developer", "agent-scope-repo-access")},
        "outbound_subject": {("user-role-developer", "tool-scope-repo-read")},
        "outbound_target": {("agent-role-repo-operations", "tool-scope-repo-read")},
    }
    assert denied == {
        "inbound": {("user-role-devops", "agent-scope-repo-access")},
        "outbound_subject": set(),
    }
    assert "outbound_target" not in denied  # never rendered — see module docstring


def test_accumulate_agent_gates_unions_across_multiple_agents() -> None:
    granted: dict[str, set[tuple[str, str]]] = {}
    denied: dict[str, set[tuple[str, str]]] = {}

    _accumulate_agent_gates(
        granted,
        denied,
        inbound_allow={"user-role-developer": ["agent-scope-repo-access"]},
        inbound_deny={},
        outbound_subject_allow={},
        outbound_subject_deny={},
        outbound_target_allow={},
    )
    _accumulate_agent_gates(
        granted,
        denied,
        inbound_allow={"user-role-tester": ["agent-scope-tracker-access"]},
        inbound_deny={},
        outbound_subject_allow={},
        outbound_subject_deny={},
        outbound_target_allow={},
    )

    assert granted["inbound"] == {
        ("user-role-developer", "agent-scope-repo-access"),
        ("user-role-tester", "agent-scope-tracker-access"),
    }
