"""Unit tests for ``best_effort_rules.py`` (spec: ``docs/specs/eval/policy-eval-correctness-e2e.md``).

Pure-logic, unmarked — runs in the default fast pass (``testpaths`` already includes ``eval/``).
No LLM, no Keycloak, no ``opa`` — just real ``Role``/``Scope``/``PolicyRule`` model construction
(cheap, no I/O), mirroring ``eval.prb_direct``'s synthetic-object style. Needs ``src/`` on
``sys.path`` itself (unlike ``test_correctness_e2e_helpers.py``'s sibling, whose logic module has
no ``aiac.*`` imports at all) since ``best_effort_rules.py`` imports real production types.
"""

from __future__ import annotations

import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent  # aiac/eval/
SRC = HERE.parent / "src"
sys.path.insert(0, str(SRC))  # so ``import aiac.*`` resolves

from aiac.idp.configuration.models import Role, Scope  # noqa: E402
from aiac.policy.model.models import RuleEffect  # noqa: E402
from eval.best_effort_rules import _best_effort_rules  # noqa: E402


def _role(name: str) -> Role:
    return Role(id=f"role-{name}", name=name, composite=False)


def _scope(name: str) -> Scope:
    return Scope(id=f"scope-{name}", name=name)


def test_role_focal_allows_selected_and_denies_explicit() -> None:
    role = _role("agent-role-inventory-operations")
    scopes = [_scope("tool-scope-inventory-read"), _scope("tool-scope-inventory-write")]
    state = {
        "selected_names": ["tool-scope-inventory-read"],
        "denied_names": ["tool-scope-inventory-write"],
        "exclusive": False,
    }
    rules = _best_effort_rules({"role": role, "scopes": scopes}, state)

    assert len(rules) == 2
    allow = next(r for r in rules if r.effect == RuleEffect.ALLOW)
    deny = next(r for r in rules if r.effect == RuleEffect.DENY)
    assert allow.role is role and allow.scope.name == "tool-scope-inventory-read"
    assert deny.role is role and deny.scope.name == "tool-scope-inventory-write"


def test_role_focal_exclusive_denies_the_unselected_complement() -> None:
    role = _role("agent-role-inventory-operations")
    scopes = [_scope("tool-scope-a"), _scope("tool-scope-b"), _scope("tool-scope-c")]
    state = {"selected_names": ["tool-scope-a"], "denied_names": [], "exclusive": True}
    rules = _best_effort_rules({"role": role, "scopes": scopes}, state)

    denied_scope_names = {r.scope.name for r in rules if r.effect == RuleEffect.DENY}
    assert denied_scope_names == {"tool-scope-b", "tool-scope-c"}


def test_scope_focal_allows_selected_and_denies_explicit() -> None:
    scope = _scope("agent-scope-tracker-access")
    roles = [_role("user-role-developer"), _role("user-role-tester")]
    state = {
        "selected_names": ["user-role-tester"],
        "denied_names": ["user-role-developer"],
        "exclusive": False,
    }
    rules = _best_effort_rules({"scope": scope, "roles": roles}, state)

    assert len(rules) == 2
    allow = next(r for r in rules if r.effect == RuleEffect.ALLOW)
    deny = next(r for r in rules if r.effect == RuleEffect.DENY)
    assert allow.scope is scope and allow.role.name == "user-role-tester"
    assert deny.scope is scope and deny.role.name == "user-role-developer"


def test_empty_proposal_yields_no_rules() -> None:
    role = _role("agent-role-x")
    scopes = [_scope("tool-scope-a")]
    state = {"selected_names": [], "denied_names": [], "exclusive": False}
    assert _best_effort_rules({"role": role, "scopes": scopes}, state) == []


def test_scope_focal_exclusive_denies_the_unselected_complement() -> None:
    scope = _scope("agent-scope-tracker-access")
    roles = [_role("user-role-developer"), _role("user-role-tester"), _role("user-role-manager")]
    state = {"selected_names": ["user-role-tester"], "denied_names": [], "exclusive": True}
    rules = _best_effort_rules({"scope": scope, "roles": roles}, state)

    denied_role_names = {r.role.name for r in rules if r.effect == RuleEffect.DENY}
    assert denied_role_names == {"user-role-developer", "user-role-manager"}


def test_scope_focal_exclusive_with_explicit_denied_names_is_the_same_complement() -> None:
    """A partially-approved multi-scope proposal can leave `exclusive=True` set alongside an
    explicit `denied_names` the auditor approved before rejecting the rest — exercises the
    combination `_denied_names` is designed for (explicit denials unioned with the derived
    complement), not just each independently (see `test_role_focal_exclusive_denies_the_unselected_
    complement`/`test_scope_focal_allows_selected_and_denies_explicit`, which each cover only one
    side)."""
    scope = _scope("agent-scope-tracker-access")
    roles = [_role("user-role-developer"), _role("user-role-tester"), _role("user-role-manager")]
    state = {
        "selected_names": ["user-role-tester"],
        "denied_names": ["user-role-developer"],  # explicit — already part of the complement too
        "exclusive": True,
    }
    rules = _best_effort_rules({"scope": scope, "roles": roles}, state)

    allow_role_names = {r.role.name for r in rules if r.effect == RuleEffect.ALLOW}
    denied_role_names = {r.role.name for r in rules if r.effect == RuleEffect.DENY}
    assert allow_role_names == {"user-role-tester"}
    assert denied_role_names == {"user-role-developer", "user-role-manager"}
    assert len(rules) == 3  # no duplicate DENY rule for the role in both the explicit set and the complement
