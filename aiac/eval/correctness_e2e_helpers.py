"""Pure-logic helpers for ``test_policy_pipeline_correctness_e2e.py`` — no LLM, no Keycloak, no
``opa``, no I/O. Mirrors ``correctness_scorer.py``'s split from its own test module
(``test_correctness_scorer.py``): logic lives here (no ``test_`` prefix, not collected by
pytest), unit tests live in ``test_correctness_e2e_helpers.py`` (unmarked, runs in the default
fast pass).
"""

from __future__ import annotations

Pair = tuple[str, str]


def _pairs_from_map(role_to_scopes: dict[str, list[str]]) -> set[Pair]:
    """Flatten a rendered Rego ``{role_name: [scope_name, ...]}`` map (e.g.
    ``subject_role_allow_scopes``) into a set of ``(role, scope)`` pairs."""
    return {(role, scope) for role, scopes in role_to_scopes.items() for scope in scopes}


def _user_role_rows(role_to_scopes: dict[str, list[str]], user_roles: set[str]) -> dict[str, list[str]]:
    """Keep only the rows of a rendered ``subject_role_*_scopes`` map whose role is a genuine user
    role of the scenario. That map's "subject" isn't exclusively human — an agent calling another
    agent (outbound-target, ``agent_role_scopes``) is rendered into the *same*
    ``subject_role_allow_scopes`` document as real user-role grants, since the outbound Rego's
    ``subject_role`` doesn't distinguish the caller's role by human-vs-agent, only by role name.
    Without this filter, every agent-role row double-counts as an ``outbound_subject`` over-grant
    even though it's already correctly counted under ``outbound_target`` — confirmed empirically:
    a real run's every over-grant was exactly its scenario's ``outbound_target`` true positives,
    reappearing here. Mirrors the ``role.name in user_role_names`` discrimination
    ``eval.test_policy_pipeline_eval.grant_sets`` already applies to the PRB's raw rules.

    Also applied to the inbound maps (``subject_role_allow/deny_scopes`` from the *inbound* Rego)
    for the same reason, though it's a no-op there in practice: the inbound Rego has no
    agent-calling-agent concept to begin with, so its ``subject_role`` rows are user roles only —
    nothing gets filtered out. Applying the same call uniformly to both directions, rather than
    conditionally skipping it for inbound, avoids two different call shapes for what is
    conceptually the same "keep user rows only" step."""
    return {role: scopes for role, scopes in role_to_scopes.items() if role in user_roles}


def _accumulate_agent_gates(
    granted: dict[str, set[Pair]],
    denied: dict[str, set[Pair]],
    *,
    inbound_allow: dict[str, list[str]],
    inbound_deny: dict[str, list[str]],
    outbound_subject_allow: dict[str, list[str]],
    outbound_subject_deny: dict[str, list[str]],
    outbound_target_allow: dict[str, list[str]],
) -> None:
    """Union one agent's rendered Rego maps into the three top-level gate buckets
    (``inbound``/``outbound_subject``/``outbound_target``), in place. No ``outbound_target_deny``
    parameter — the outbound Rego generator never renders that map (see
    ``test_policy_pipeline_correctness_e2e.py``'s module docstring)."""
    granted.setdefault("inbound", set()).update(_pairs_from_map(inbound_allow))
    denied.setdefault("inbound", set()).update(_pairs_from_map(inbound_deny))
    granted.setdefault("outbound_subject", set()).update(_pairs_from_map(outbound_subject_allow))
    denied.setdefault("outbound_subject", set()).update(_pairs_from_map(outbound_subject_deny))
    granted.setdefault("outbound_target", set()).update(_pairs_from_map(outbound_target_allow))
