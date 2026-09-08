"""Pure-logic helper for ``test_policy_pipeline_eval.py``'s ``_invoke_graph`` — no LLM, no
Keycloak, no ``opa``, no I/O. Mirrors ``correctness_e2e_helpers.py``'s split from its own test
module: logic lives here (no ``test_`` prefix, not collected by pytest), unit tests live in
``test_best_effort_rules.py`` (unmarked, runs in the default fast pass).
"""

from __future__ import annotations

from typing import Any

from aiac.agent.policy_rules_builder.graph import _denied_names
from aiac.policy.model.models import PolicyRule, RuleEffect


def _best_effort_rules(entity: dict[str, Any], state: dict[str, Any]) -> list[PolicyRule]:
    """Replicate ``graph.py``'s ``build`` node logic (role-focal or scope-focal, whichever
    ``entity``'s shape indicates) against a proposal the auditor never approved — same
    ``_denied_names()``-driven exclusivity-complement + explicit-prohibition set, same
    ALLOW-then-DENY ``PolicyRule`` shape (``graph.py``'s ``build_role_graph``/``build_scope_graph``
    closures, not importable — they're nested — hence replicated here rather than reused).

    Only ever called from ``eval.test_policy_pipeline_eval._invoke_graph`` after catching a
    rejection; the caller is responsible for flagging the result as best-effort (not a real,
    auditor-approved decision) — see ``orchestrate_prb``'s ``best_effort_notes``.
    """
    selected = set(state.get("selected_names", []))
    denied_explicit = state.get("denied_names", [])
    exclusive = bool(state.get("exclusive", False))
    if "role" in entity:  # ROLE_GRAPH shape: role-focal
        role = entity["role"]
        candidate_scopes = entity["scopes"]
        denied = _denied_names(denied_explicit, exclusive, [sc.name for sc in candidate_scopes], selected)
        allows = [
            PolicyRule(role=role, scope=sc, effect=RuleEffect.ALLOW) for sc in candidate_scopes if sc.name in selected
        ]
        denies = [
            PolicyRule(role=role, scope=sc, effect=RuleEffect.DENY) for sc in candidate_scopes if sc.name in denied
        ]
        return allows + denies
    # SCOPE_GRAPH shape: scope-focal
    scope = entity["scope"]
    candidate_roles = entity["roles"]
    denied = _denied_names(denied_explicit, exclusive, [r.name for r in candidate_roles], selected)
    allows = [PolicyRule(role=r, scope=scope, effect=RuleEffect.ALLOW) for r in candidate_roles if r.name in selected]
    denies = [PolicyRule(role=r, scope=scope, effect=RuleEffect.DENY) for r in candidate_roles if r.name in denied]
    return allows + denies
