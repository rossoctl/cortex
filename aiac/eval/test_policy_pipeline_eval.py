"""Generalized policy-pipeline evaluation harness — Scenarios 1, 3, 4, 6-10 (spec: ``docs/specs/
eval/policy-eval-scenarios.md``). Scenario numbers 2 and 5 are reserved for the
light guardrail tests (``test_guardrail_conflicts.py``/``test_guardrail_injection.py``, out of
scope here) and are deliberately skipped in this sequence.

Companion to ``test_policy_pipeline.py`` (which drives the fixed single-agent/single-tool
``github-agent`` scenario as a regression baseline — untouched by this file). This harness drives
the same identity->policy pipeline (Keycloak -> real Policy Rules Builder -> real Policy
Computation Engine -> OPA Policy Writer, nothing mocked) against eight independently-authored,
single-aspect scenarios, each in its own non-code domain (except ``baseline``, the one code
scenario):

  - ``scenario_eval_baseline`` (Scenario 1)               — software eng.; 3 users / 2 agents / 2
    tools, clean and unambiguous regression baseline at UC1 scale (reuses UC1's
    user-role-developer/user-role-tester/user-role-devops roles).
  - ``scenario_eval_agent_delegation`` (Scenario 3)       — logistics/shipping; 2 users / 2 agents
    / 1 tool, isolates the agent-to-agent ``target_scopes`` delegation mechanism. Lives under
    ``test/integration/`` (not ``eval/`` like the rest) — see the note below.
  - ``scenario_eval_unreachable_resources`` (Scenario 4)  — healthcare/clinic; 1 user / 2 agents /
    2 tools, silent gaps producing emergent unreachable agents and tools.
  - ``scenario_eval_ambiguous_clause`` (Scenario 6)       — education/registrar; 1 user / 1 agent /
    1 tool, a broad-sounding grant clause narrowed by an explicit in-clause qualifier.
  - ``scenario_eval_wildcard_grant`` (Scenario 7)         — retail/inventory; 1 user / 1 agent / 1
    tool, wildcard-phrased grant expansion.
  - ``scenario_eval_misleading_descriptions`` (Scenario 8) — hospitality/hotel; 2 users / 1 agent /
    1 tool, a name-bait role and an inert scary-named scope.
  - ``scenario_eval_confusable_agents`` (Scenario 9)      — sports/coaching; 2 users / 2 agents / 2
    tools, a confusable agent-name pair plus an identity/boundary-confusion probe.
  - ``scenario_eval_empty_descriptions`` (Scenario 10)    — agriculture/irrigation; 1 user / 1
    agent / 1 tool, every entity/role/scope description is empty.

Unlike ``test_policy_pipeline.py`` (hardcoded to the literal ``github_agent`` slug and a single
tool id), every path/query here is derived from each scenario module's own agent/tool ids via
``slugify``-equivalent ``.replace("-", "_")``, and the outbound gate is driven through the
generalized ``probe_eval.rego`` (parameterized by ``input.agent_id``) rather than the fixed
``probe.rego``. ``launcher.py`` is reused unmodified.

``scenario_eval_agent_delegation``'s data file is the one exception to the "everything lives in
``eval/``" rule — it sits at ``test/integration/scenario_eval_agent_delegation.py`` (sibling of
``launcher.py``/``scenario_uc1.py``), so each scenario's ``POLICY_FILE`` is resolved relative to
*that scenario module's own directory*, not the fixed ``eval/`` directory.

Run (needs KEYCLOAK_URL + admin creds + LLM_* exported, ``opa`` on PATH):
    .venv/bin/pytest eval/test_policy_pipeline_eval.py -m eval_extended -v
Without ``-m eval_extended`` the suite is skipped; without ``opa`` each node skips at
runtime. This suite is heavier than ``test_policy_pipeline.py`` (eight full pipeline runs, more
PRB/LLM calls) hence the separate marker.

The ``pipeline`` fixture provisions all 8 scenarios in parallel via ``ProcessPoolExecutor``
(``EVAL_PIPELINE_PARALLELISM``, default = scenario count) — separate OS processes, not threads:
each scenario mutates process-global state while it runs (``os.environ["KEYCLOAK_REALM"]``/
``["AIAC_POLICY_FILE"]``, consumed at call time by ``compute_and_apply``/
``FilePolicySource.fetch()``; a ``KeycloakAdmin`` connection's ``change_current_realm``), which
two threads sharing one process would race on but separate processes don't. Each worker also gets
its own idp/store/opa port triple (offset from the module defaults by scenario index) since fixed
ports collide regardless of thread vs. process. This is *not* the same thing as pytest-xdist's
``-n`` (still not supported/needed here — the parallelism is inside the fixture, not across
pytest workers).

A shared ``multiprocessing.Lock()`` serializes just the ``provision_keycloak_admin`` step (realm +
role/user/client creation) across workers — concurrent ``admin.create_realm(...)`` calls against a
real Keycloak instance were observed to 409 with ``"Duplicate resource error"`` even across
*distinct* realm names (an internal Keycloak race on concurrent realm creation, not a naming
collision on this fixture's side). Everything after that per scenario — the LLM-heavy
``orchestrate_prb`` calls and the idp/store/opa subprocesses — still runs fully concurrently, since
realm provisioning is a small fraction of one scenario's wall-clock next to the PRB's several
sequential LLM calls.
"""

from __future__ import annotations

import json
import logging
import multiprocessing
import os
import shutil
import subprocess
import sys
import tempfile
from concurrent.futures import ProcessPoolExecutor, as_completed
from pathlib import Path
from types import ModuleType
from typing import Any
from urllib.parse import urlsplit

import pytest

pytestmark = pytest.mark.eval_extended

HERE = Path(__file__).resolve().parent  # aiac/eval/
REPO_ROOT = HERE.parent  # -> aiac/
SRC = REPO_ROOT / "src"
sys.path.insert(0, str(REPO_ROOT))  # so ``import test.integration.*``/``eval.*`` resolves
sys.path.insert(0, str(SRC))  # so ``import aiac.*`` resolves

from eval.scenarios import scenario_eval_ambiguous_clause as scn_ambiguous_clause  # noqa: E402
from eval.scenarios import scenario_eval_baseline as scn_baseline  # noqa: E402
from eval.scenarios import scenario_eval_confusable_agents as scn_confusable_agents  # noqa: E402
from eval.scenarios import scenario_eval_empty_descriptions as scn_empty_descriptions  # noqa: E402
from eval.scenarios import (  # noqa: E402
    scenario_eval_misleading_descriptions as scn_misleading_descriptions,
)
from eval.scenarios import (  # noqa: E402
    scenario_eval_unreachable_resources as scn_unreachable_resources,
)
from eval.scenarios import scenario_eval_wildcard_grant as scn_wildcard_grant  # noqa: E402
from test.integration import scenario_eval_agent_delegation as scn_agent_delegation  # noqa: E402
from test.integration.launcher import (  # noqa: E402
    Service,
    require_env,
    running_services,
)

# --- Resolve config + set env BEFORE importing aiac (the libraries read env at import time) ---
DEFAULT_IDP_PORT = 7071
DEFAULT_OPA_PORT = 7072
DEFAULT_STORE_PORT = 7074
os.environ.setdefault("AIAC_PDP_CONFIG_URL", f"http://127.0.0.1:{DEFAULT_IDP_PORT}")
os.environ.setdefault("AIAC_POLICY_STORE_URL", f"http://127.0.0.1:{DEFAULT_STORE_PORT}")
os.environ.setdefault("AIAC_PDP_POLICY_URL", f"http://127.0.0.1:{DEFAULT_OPA_PORT}")
os.environ.setdefault("KEYCLOAK_ADMIN_REALM", "master")  # inherited by the IdP subprocess

from keycloak import KeycloakAdmin  # noqa: E402
from keycloak.exceptions import KeycloakError  # noqa: E402

from aiac.agent.policy_rules_builder.graph import ROLE_GRAPH, SCOPE_GRAPH  # noqa: E402
from aiac.idp.configuration.api import Configuration  # noqa: E402
from aiac.idp.configuration.models import Role, Scope  # noqa: E402
from aiac.policy.computation.engine import compute_and_apply  # noqa: E402
from aiac.policy.model.models import PolicyRule  # noqa: E402

log = logging.getLogger(__name__)

SCENARIOS: dict[str, ModuleType] = {
    "baseline": scn_baseline,
    "agent_delegation": scn_agent_delegation,
    "unreachable_resources": scn_unreachable_resources,
    "ambiguous_clause": scn_ambiguous_clause,
    "wildcard_grant": scn_wildcard_grant,
    "misleading_descriptions": scn_misleading_descriptions,
    "confusable_agents": scn_confusable_agents,
    "empty_descriptions": scn_empty_descriptions,
}


# ======================================================================================
# Ported/generalized helpers (test_policy_pipeline.py's helpers, looped over N entities)
# ======================================================================================


def _host_port(url: str, default_port: int) -> tuple[str, int]:
    parts = urlsplit(url)
    return parts.hostname or "127.0.0.1", parts.port or default_port


def _connect_admin() -> KeycloakAdmin:
    """Connect to the admin realm so the harness can create/delete each scenario's test realm."""
    creds = require_env("KEYCLOAK_URL", "KEYCLOAK_ADMIN_USERNAME", "KEYCLOAK_ADMIN_PASSWORD")
    admin_realm = os.environ["KEYCLOAK_ADMIN_REALM"]
    return KeycloakAdmin(
        server_url=creds["KEYCLOAK_URL"],
        realm_name=admin_realm,
        user_realm_name=admin_realm,
        username=creds["KEYCLOAK_ADMIN_USERNAME"],
        password=creds["KEYCLOAK_ADMIN_PASSWORD"],
    )


def provision_keycloak_admin(admin: KeycloakAdmin, test_realm: str, scenario: ModuleType) -> None:
    """Provision the realm via ``python-keycloak`` (idempotent: delete-if-exists, then create).

    Driven entirely off the scenario module: creates the realm, every ``scenario.USER_ROLES``
    realm role, every ``scenario.USERS`` user (with role assignment), and one client per agent and
    per tool (each with a service account so client roles can be assigned to it later).
    """
    try:
        admin.delete_realm(test_realm)
    except KeycloakError:
        pass  # realm absent — nothing to delete
    admin.create_realm({"realm": test_realm, "enabled": True})
    admin.change_current_realm(test_realm)

    for name, description in scenario.USER_ROLES.items():
        # aiac.managed marker required: the IdP service only populates actorIds (member usernames)
        # for managed roles, and the PCE needs actorIds to build the subject_roles map in the APM.
        admin.create_realm_role(
            {"name": name, "description": description, "attributes": {"aiac.managed": ["true"]}},
            skip_exists=True,
        )

    for username, role_name in scenario.USERS.items():
        user_id = admin.create_user({"username": username, "enabled": True}, exist_ok=True)
        admin.set_user_password(user_id, scenario.USER_PASSWORD, temporary=False)
        admin.assign_realm_roles(user_id, [admin.get_realm_role(role_name)])

    def _client(client_id: str, description: str) -> dict:
        return {
            "clientId": client_id,
            "enabled": True,
            "description": description,
            "protocol": "openid-connect",
            "publicClient": False,  # confidential — required for a service account
            "serviceAccountsEnabled": True,
            "standardFlowEnabled": False,
        }

    for agent_id, agent in scenario.AGENTS.items():
        admin.create_client(_client(agent_id, agent["description"]), skip_exists=True)
    for tool_id, tool in scenario.TOOLS.items():
        admin.create_client(_client(tool_id, tool["description"]), skip_exists=True)


def provision_via_config(config: Configuration, scenario: ModuleType) -> None:
    """Provision client roles + scopes and their service mappings through the aiac IdP library.

    Generalizes ``test_policy_pipeline.py``'s single-agent/single-tool version to loop over every
    agent's ``inbound_scopes`` + ``delegation_scopes`` + ``roles`` and every tool's ``scopes``. NOT
    idempotent — call exactly once per realm.
    """
    inbound_scopes: dict[str, Scope] = {}
    delegation_scopes: dict[str, Scope] = {}
    agent_roles: dict[str, Role] = {}
    tool_scopes: dict[str, Scope] = {}

    for agent in scenario.AGENTS.values():
        for name, desc in agent["inbound_scopes"].items():
            inbound_scopes[name] = config.create_scope(name, desc)
        for name, desc in agent.get("delegation_scopes", {}).items():
            delegation_scopes[name] = config.create_scope(name, desc)
        for name, desc in agent["roles"].items():
            agent_roles[name] = config.create_role(name, desc)
    for tool in scenario.TOOLS.values():
        for name, desc in tool["scopes"].items():
            tool_scopes[name] = config.create_scope(name, desc)

    services = {svc.serviceId: svc for svc in config.get_services()}

    for agent_id, agent in scenario.AGENTS.items():
        agent_svc = services[agent_id]
        for name in agent["inbound_scopes"]:
            config.map_scope_to_service(agent_svc, inbound_scopes[name])
        for name in agent.get("delegation_scopes", {}):
            config.map_scope_to_service(agent_svc, delegation_scopes[name])
        for name in agent["roles"]:
            config.map_role_to_service(agent_svc, agent_roles[name])
        config.set_service_type(agent_svc, "Agent")

    for tool_id, tool in scenario.TOOLS.items():
        tool_svc = services[tool_id]
        for name in tool["scopes"]:
            config.map_scope_to_service(tool_svc, tool_scopes[name])
        config.set_service_type(tool_svc, "Tool")


def _read_back(config: Configuration) -> tuple[dict[str, Role], dict[str, Scope]]:
    """Read roles + scopes back through the IdP library (carrying real ids + descriptions).

    Scopes are sourced from each service's scope list (not the standalone get_scopes()), so that
    scope.serviceId is populated — a required input for the PCE's SPM routing.
    """
    roles = {r.name: r for r in config.get_roles()}
    scopes: dict[str, Scope] = {}
    for svc in config.get_services():
        for s in svc.scopes:
            scopes.setdefault(s.name, s)  # first owner wins; each scope has exactly one owner
    return roles, scopes


def _invoke_graph(graph: Any, **entity: object) -> tuple[list[PolicyRule], str]:
    """Same state shape ``build_scope_rules``/``build_role_rules`` build internally, invoked
    directly so the final state's ``reasoning`` string (discarded by the wrapper) comes back too.

    ``entity`` carries the one field that differs between the two graphs: ``roles``+``scope`` for
    ``SCOPE_GRAPH``, ``role``+``scopes`` for ``ROLE_GRAPH``.
    """
    state = {
        **entity,
        "policy_text": "",
        "selected_names": [],
        "denied_names": [],
        "conflict_names": [],
        "exclusive": False,
        "reasoning": "",
        "approved": False,
        "audit_feedback": None,
        "retry_count": 0,
        "rules": [],
    }
    out = graph.invoke(state)
    return out["rules"], out["reasoning"]


def orchestrate_prb(
    roles: dict[str, Role], scopes: dict[str, Scope], scenario: ModuleType
) -> tuple[list[PolicyRule], dict[str, str], dict[str, str]]:
    """Run the three PRB mappings against the real LLM and concatenate the rules, generalized over
    every agent's inbound/target scopes and every tool's scopes.

    Mirrors ``test_policy_pipeline.py``'s three loops: (a) user role -> each agent's inbound
    scope, (b) user role -> each tool/agent-target scope, (c) each agent role -> all tool/
    agent-target scopes. Agent-target scopes (e.g. ``code-delegation``) are folded into the same
    "target" candidate set as tool scopes for (b)/(c) — from the PRB/PCE's perspective a target
    scope owned by another agent is handled identically to one owned by a tool.

    Also returns each call's real LLM ``reasoning`` string, for the eval report (see
    ``conftest.py``): ``reasoning_by_scope`` (one entry per scope decided by an (a)/(b) call) and
    ``reasoning_by_agent_role`` (one entry per agent role decided by a (c) call). Invokes
    ``ROLE_GRAPH``/``SCOPE_GRAPH`` directly (via ``_invoke_role_graph``/``_invoke_scope_graph``)
    instead of ``build_role_rules``/``build_scope_rules`` purely to get that reasoning back —
    those wrapper functions discard it, and are shared production code used elsewhere, so they are
    not modified.
    """
    user_roles = [roles[name] for name in scenario.USER_ROLES]

    inbound_scope_names = [n for agent in scenario.AGENTS.values() for n in agent["inbound_scopes"]]
    target_scope_names = [n for tool in scenario.TOOLS.values() for n in tool["scopes"]]
    target_scope_names += [n for agent in scenario.AGENTS.values() for n in agent.get("delegation_scopes", {})]
    agent_role_names = [n for agent in scenario.AGENTS.values() for n in agent["roles"]]

    inbound_scopes = [scopes[n] for n in inbound_scope_names]
    target_scopes = [scopes[n] for n in target_scope_names]
    agent_roles = [roles[n] for n in agent_role_names]

    rules: list[PolicyRule] = []
    reasoning_by_scope: dict[str, str] = {}
    reasoning_by_agent_role: dict[str, str] = {}
    for agent_scope in inbound_scopes:  # (a) user role -> agent inbound scope
        scope_rules, reasoning = _invoke_graph(SCOPE_GRAPH, roles=user_roles, scope=agent_scope)
        rules += scope_rules
        reasoning_by_scope[agent_scope.name] = reasoning
    for target_scope in target_scopes:  # (b) user role -> tool/agent-target scope
        scope_rules, reasoning = _invoke_graph(SCOPE_GRAPH, roles=user_roles, scope=target_scope)
        rules += scope_rules
        reasoning_by_scope[target_scope.name] = reasoning
    for agent_role in agent_roles:  # (c) agent role -> all tool/agent-target scopes
        role_rules, reasoning = _invoke_graph(ROLE_GRAPH, role=agent_role, scopes=target_scopes)
        rules += role_rules
        reasoning_by_agent_role[agent_role.name] = reasoning
    return rules, reasoning_by_scope, reasoning_by_agent_role


# ======================================================================================
# OPA evaluation
# ======================================================================================


def opa_bin() -> str:
    """Path to the ``opa`` binary, or skip the calling test if it cannot be found."""
    found = os.environ.get("OPA_BIN") or shutil.which("opa")
    if not found:
        pytest.skip("opa binary not found (set OPA_BIN or add opa to PATH)")
    return found


def opa_eval(rego_paths: list[Path], query: str, input_doc: dict) -> Any:
    """Evaluate ``query`` against the given Rego file(s) with ``input_doc`` on stdin; return the
    decoded JSON result — a ``bool`` for a decision query (e.g. ``...request.allow``), or a data
    document (e.g. a ``{role: [scope, ...]}`` map) for a plain declaration. Raises (via
    ``check=True``) if OPA rejects the Rego or the query errors."""
    cmd = [
        opa_bin(),
        "eval",
        "-f",
        "json",
        *sum((["-d", str(p)] for p in rego_paths), []),
        "--stdin-input",
        query,
    ]
    out = subprocess.run(
        cmd, input=json.dumps(input_doc), capture_output=True, text=True, check=True
    ).stdout
    return json.loads(out)["result"][0]["expressions"][0]["value"]


def _rego_path(rego_dir: Path, agent_id: str, direction: str) -> Path:
    """Path to a generated policy file, matching ``main.py``'s
    ``<rego_dir>/<namespace>/<name>/{inbound,outbound}/request.rego`` layout."""
    namespace, _, name = agent_id.partition("/")
    return rego_dir / namespace / name / direction / "request.rego"


# ======================================================================================
# Expected-verdict oracle (pure functions over each scenario's truth table)
# ======================================================================================


def _agent_inbound_scope_names(scenario: ModuleType) -> set[str]:
    return {n for agent in scenario.AGENTS.values() for n in agent["inbound_scopes"]}


def _agent_delegation_scope_names(scenario: ModuleType) -> set[str]:
    return {n for agent in scenario.AGENTS.values() for n in agent.get("delegation_scopes", {})}


def _tool_scope_names(scenario: ModuleType) -> set[str]:
    return {n for tool in scenario.TOOLS.values() for n in tool["scopes"]}


def _user_role_names(scenario: ModuleType) -> set[str]:
    return set(scenario.USER_ROLES)


def _agent_role_names(scenario: ModuleType) -> set[str]:
    return {n for agent in scenario.AGENTS.values() for n in agent["roles"]}


def _scope_owner(scenario: ModuleType, scope_name: str) -> str:
    """Return the serviceId (agent or tool id) that owns ``scope_name``."""
    for agent_id, agent in scenario.AGENTS.items():
        if scope_name in agent["inbound_scopes"] or scope_name in agent.get("delegation_scopes", {}):
            return agent_id
    for tool_id, tool in scenario.TOOLS.items():
        if scope_name in tool["scopes"]:
            return tool_id
    raise KeyError(f"scope {scope_name!r} not owned by any agent/tool in this scenario")


def expected_inbound(scenario: ModuleType, subject: str, agent_id: str) -> bool:
    """A user may call ``agent_id`` iff their realm role holds a scope this agent owns. This
    includes both the agent's ``inbound_scopes`` (via ``INBOUND_PAIRS``) and its
    ``delegation_scopes`` (via ``OUTBOUND_SUBJECT_PAIRS``): the provisioning step maps both onto
    the same Keycloak client, so the generated inbound Rego's ``agent_scopes`` — and therefore its
    audience gate — cannot distinguish "may call me directly" from "may reach me only as a
    delegation target through another agent". A role granted a delegation scope for delegation
    purposes necessarily also passes the owning agent's own inbound gate; there is no mechanism in
    the two-layer policy model that would keep the two separate."""
    role = scenario.USERS[subject]
    agent = scenario.AGENTS[agent_id]
    agent_scopes = set(agent["inbound_scopes"]) | set(agent.get("delegation_scopes", {}))
    via_inbound = any(r == role and s in agent_scopes for r, s in scenario.INBOUND_PAIRS)
    via_target_delegation = any(
        r == role and s in agent_scopes for r, s in scenario.OUTBOUND_SUBJECT_PAIRS
    )
    return via_inbound or via_target_delegation


def expected_outbound(scenario: ModuleType, subject: str, agent_id: str, scope: str) -> bool:
    """A subject's call through ``agent_id`` resolves to ``scope`` iff the subject is entitled to
    it (``OUTBOUND_SUBJECT_PAIRS``) *and* one of ``agent_id``'s OWN roles is entitled to it
    (``OUTBOUND_PAIRS``) — per-agent, since each agent's outbound gate only sees its own roles."""
    role = scenario.USERS[subject]
    subject_ok = (role, scope) in set(scenario.OUTBOUND_SUBJECT_PAIRS)
    agent_role_names = set(scenario.AGENTS[agent_id]["roles"])
    agent_ok = any(r in agent_role_names and s == scope for r, s in scenario.OUTBOUND_PAIRS)
    return subject_ok and agent_ok


def _inbound_explanation(scenario: ModuleType, subject: str, agent_id: str) -> str:
    """Human-readable mirror of ``expected_inbound``'s logic, for the eval report's 'Expected
    output' field — names which pair matched (direct inbound grant or target-scope delegation), or
    which of the agent's scopes came up empty."""
    role = scenario.USERS[subject]
    agent = scenario.AGENTS[agent_id]
    agent_scopes = set(agent["inbound_scopes"]) | set(agent.get("delegation_scopes", {}))
    for r, s in scenario.INBOUND_PAIRS:
        if r == role and s in agent_scopes:
            return f"role '{role}' holds this agent's own scope '{s}' (direct inbound grant)"
    for r, s in scenario.OUTBOUND_SUBJECT_PAIRS:
        if r == role and s in agent_scopes:
            return (
                f"role '{role}' holds this agent's target scope '{s}' (reachable via delegation, "
                "which also satisfies the agent's own inbound gate)"
            )
    checked = ", ".join(sorted(agent_scopes)) or "(none)"
    return f"role '{role}' holds none of this agent's scopes ({checked}) — denied by default"


def _outbound_explanation(scenario: ModuleType, subject: str, agent_id: str, scope: str) -> str:
    """Human-readable mirror of ``expected_outbound``'s logic, for the eval report's 'Expected
    output' field — names whether the subject-side and agent-role-side conditions each held."""
    role = scenario.USERS[subject]
    subject_ok = (role, scope) in set(scenario.OUTBOUND_SUBJECT_PAIRS)
    agent_role_names = set(scenario.AGENTS[agent_id]["roles"])
    agent_ok = any(r in agent_role_names and s == scope for r, s in scenario.OUTBOUND_PAIRS)
    if subject_ok and agent_ok:
        return f"role '{role}' is entitled to scope '{scope}' AND '{agent_id}' is entitled to it"
    if not subject_ok and not agent_ok:
        return (
            f"role '{role}' is not entitled to scope '{scope}', and neither is '{agent_id}' — "
            "denied on both sides"
        )
    if not subject_ok:
        return f"role '{role}' is not entitled to scope '{scope}' (even though '{agent_id}' is)"
    return f"'{agent_id}' is not entitled to scope '{scope}' (even though role '{role}' is)"


def reformat_function_name(scope: str) -> str:
    """Render a scope as a differently-cased/separated ``function_name`` to exercise the probe's
    token soft-match: ``source-read`` -> ``Source.Read``."""
    return ".".join(part.capitalize() for part in scope.split("-"))


# --- Grant-set extraction (per-scenario semantic oracle) -------------------------------------


def grant_sets(scenario: ModuleType, rules: list[PolicyRule]) -> dict[str, set[tuple[str, str]]]:
    """Classify a flat PRB rule list into the three gate grant sets, each a set of
    ``(role_name, scope_name)`` pairs: ``inbound`` (user role -> agent inbound scope),
    ``outbound_subject`` (user role -> tool/agent-target scope), ``outbound_target`` (agent role ->
    tool/agent-target scope). ``user_role_names``/``agent_role_names`` are disjoint by
    construction, so classification order does not matter."""
    user_role_names = _user_role_names(scenario)
    agent_role_names = _agent_role_names(scenario)
    agent_inbound_names = _agent_inbound_scope_names(scenario)
    target_names = _tool_scope_names(scenario) | _agent_delegation_scope_names(scenario)

    sets: dict[str, set[tuple[str, str]]] = {
        "inbound": set(),
        "outbound_subject": set(),
        "outbound_target": set(),
    }
    for r in rules:
        pair = (r.role.name, r.scope.name)
        if r.role.name in user_role_names and r.scope.name in agent_inbound_names:
            sets["inbound"].add(pair)
        elif r.role.name in user_role_names and r.scope.name in target_names:
            sets["outbound_subject"].add(pair)
        elif r.role.name in agent_role_names and r.scope.name in target_names:
            sets["outbound_target"].add(pair)
    return sets


def truth(scenario: ModuleType) -> dict[str, set[tuple[str, str]]]:
    return {
        "inbound": set(scenario.INBOUND_PAIRS),
        "outbound_subject": set(scenario.OUTBOUND_SUBJECT_PAIRS),
        "outbound_target": set(scenario.OUTBOUND_PAIRS),
    }


# ======================================================================================
# ProcessPoolExecutor worker init — a multiprocessing.Lock() can only reach a worker via the
# pool's initializer/initargs ("inheritance" at process-start time), not as a regular per-task
# submit() argument — passing one through the ongoing call queue instead raises "Lock objects
# should only be shared between processes through inheritance" under the forkserver/spawn start
# methods (this Python's default is forkserver; plain fork wouldn't need this, but forkserver
# does). See ``_provision_scenario`` for why the lock exists at all.
# ======================================================================================

_REALM_LOCK: Any = None


def _init_worker(realm_lock: Any) -> None:
    global _REALM_LOCK
    _REALM_LOCK = realm_lock


# ======================================================================================
# Session fixture — one pipeline run per scenario
# ======================================================================================


def _provision_scenario(name: str, idp_port: int, store_port: int, opa_port: int) -> dict:
    """Provision one scenario's realm and run the real PRB+PCE pipeline, leaving ``.rego`` on disk
    under ``rego_out/policy_pipeline_eval/<scenario>/``. Returns ``{"rego_dir": Path, "rules":
    list[PolicyRule], "reasoning_by_scope": dict[str, str], "reasoning_by_agent_role": dict[str,
    str]}`` (the two reasoning dicts feed the eval report's per-cell "Output" field, see
    ``conftest.py``), or ``{"error": <str>}`` on failure.

    Fully self-contained — own ``KeycloakAdmin`` connection, own env-var writes, own
    idp/store/opa ports — so it can run as an independent ``ProcessPoolExecutor`` worker (see the
    module docstring for why a *shared* admin object / shared ``os.environ`` would race under
    threads). Takes ``name`` and looks ``SCENARIOS[name]`` up itself rather than receiving the
    scenario ``ModuleType`` as a parameter — module objects aren't picklable, and every argument
    here has to survive being pickled across the process boundary to the worker. The error return
    is stringified for the same reason: not every exception type is guaranteed picklable back.

    ``_REALM_LOCK`` (a ``multiprocessing.Lock()`` set once per worker by ``_init_worker``, see
    above) serializes just ``provision_keycloak_admin`` — concurrent ``admin.create_realm(...)``
    calls against this Keycloak instance were observed to 409 with ``"Duplicate resource error"``
    even across *distinct* realm names, i.e. an internal Keycloak race on concurrent realm
    creation, not a bug in this fixture's realm naming. Everything after that (the LLM-heavy
    ``orchestrate_prb`` calls, the idp/store/opa subprocesses) stays fully concurrent — realm
    provisioning is a small fraction of one scenario's wall-clock next to the PRB's several
    sequential LLM calls.
    """
    try:
        scenario = SCENARIOS[name]
        idp_host, _ = _host_port(os.environ["AIAC_PDP_CONFIG_URL"], DEFAULT_IDP_PORT)
        store_host, _ = _host_port(os.environ["AIAC_POLICY_STORE_URL"], DEFAULT_STORE_PORT)
        opa_host, _ = _host_port(os.environ["AIAC_PDP_POLICY_URL"], DEFAULT_OPA_PORT)

        admin = _connect_admin()
        os.environ["KEYCLOAK_REALM"] = scenario.REALM_DEFAULT  # PCE reads this back
        with _REALM_LOCK:
            provision_keycloak_admin(admin, scenario.REALM_DEFAULT, scenario)

        rego_dir = HERE / "rego_out" / "policy_pipeline_eval" / name
        if rego_dir.exists():
            shutil.rmtree(rego_dir)
        rego_dir.mkdir(parents=True)
        db_path = Path(tempfile.mkdtemp(prefix=f"aiac-store-eval-{name}-")) / "policy_model.db"
        scenario_dir = Path(scenario.__file__).resolve().parent
        os.environ["AIAC_POLICY_FILE"] = str(scenario_dir / scenario.POLICY_FILE)
        os.environ["AIAC_PDP_CONFIG_URL"] = f"http://{idp_host}:{idp_port}"
        os.environ["AIAC_POLICY_STORE_URL"] = f"http://{store_host}:{store_port}"
        # The model-store client actually reads AIAC_POLICY_MODEL_STORE_URL, not
        # AIAC_POLICY_STORE_URL (which nothing consumes) — set both so this worker's PCE calls
        # land on its own store subprocess instead of every worker colliding on the hardcoded
        # 127.0.0.1:7074 default once ports diverge per worker.
        os.environ["AIAC_POLICY_MODEL_STORE_URL"] = f"http://{store_host}:{store_port}"
        os.environ["AIAC_PDP_POLICY_URL"] = f"http://{opa_host}:{opa_port}"
        log.info(
            "scenario %s: realm=%s policy=%s rego_dir=%s ports=(idp=%d store=%d opa=%d)",
            name,
            scenario.REALM_DEFAULT,
            os.environ["AIAC_POLICY_FILE"],
            rego_dir,
            idp_port,
            store_port,
            opa_port,
        )

        idp = Service("aiac.idp.service.configuration.keycloak.main:app", port=idp_port, host=idp_host)
        store = Service(
            "aiac.policy.model_store.service.main:app",
            port=store_port,
            host=store_host,
            env={"SERVICEPOLICY_DB_PATH": str(db_path)},
        )
        opa = Service(
            "aiac.pdp.service.policy.opa.main:app",
            port=opa_port,
            host=opa_host,
            env={"REGO_OUTPUT_DIR": str(rego_dir), "POLICY_WRITER_DUMP_REGO": "true"},
        )
        with running_services([idp, store, opa], src=SRC):
            config = Configuration.for_realm(scenario.REALM_DEFAULT)
            provision_via_config(config, scenario)  # exactly once — not idempotent
            roles, scopes = _read_back(config)
            rules, reasoning_by_scope, reasoning_by_agent_role = orchestrate_prb(roles, scopes, scenario)
            compute_and_apply(rules, override=False)

        # Assert every agent's rego actually landed here at setup — EXCEPT agents the scenario
        # itself declares as deliberately/emergently unreachable (Scenario 4), so a real
        # pipeline failure still surfaces as one clear error instead of cryptic per-test skips.
        allow_missing = set(getattr(scenario, "EXPECT_NO_REGO", frozenset()))
        expected = [
            _rego_path(rego_dir, agent_id, direction)
            for agent_id in scenario.AGENTS
            for direction in ("inbound", "outbound")
            if agent_id not in allow_missing
        ]
        missing = [str(p.relative_to(rego_dir)) for p in expected if not p.is_file()]
        if missing:
            raise RuntimeError(
                f"scenario {name!r}: compute_and_apply produced no {missing} in {rego_dir} "
                f"(PRB returned {len(rules)} rule(s)); the pipeline failed silently — "
                f"check the compute_and_apply logs above for a swallowed exception."
            )
        return {
            "rego_dir": rego_dir,
            "rules": rules,
            "reasoning_by_scope": reasoning_by_scope,
            "reasoning_by_agent_role": reasoning_by_agent_role,
        }
    except Exception as exc:  # noqa: BLE001 - isolate one scenario's setup failure from the rest
        log.exception("scenario %s: setup failed, isolating from the rest of the session", name)
        return {"error": f"{exc!r}"}


@pytest.fixture(scope="session")
def pipeline() -> dict[str, dict]:
    """Provision Keycloak and run the real PRB+PCE pipeline once per scenario — in parallel, one
    ``ProcessPoolExecutor`` worker per scenario (see the module docstring) — leaving ``.rego`` on
    disk under ``rego_out/policy_pipeline_eval/<scenario>/``. Returns ``{scenario_name:
    _provision_scenario(...)}`` for every scenario.

    Each scenario gets its own realm (``scenario.REALM_DEFAULT``), its own fresh IdP/Store/OPA
    subprocess trio, and its own port triple (offset from the module defaults by scenario index) —
    unlike ``test_policy_pipeline.py``'s two variants (which share one realm and reuse a single IdP
    process), these scenarios' realms differ, so nothing can safely be kept warm across them.
    """
    require_env(
        "KEYCLOAK_URL",
        "KEYCLOAK_ADMIN_USERNAME",
        "KEYCLOAK_ADMIN_PASSWORD",
        "LLM_BASE_URL",
        "LLM_MODEL",
        "LLM_API_KEY",
    )

    max_workers = int(os.environ.get("EVAL_PIPELINE_PARALLELISM", str(len(SCENARIOS))))
    realm_lock = multiprocessing.Lock()  # serializes admin.create_realm — see _provision_scenario
    results: dict[str, dict] = {}
    with ProcessPoolExecutor(max_workers=max_workers, initializer=_init_worker, initargs=(realm_lock,)) as executor:
        futures = {
            executor.submit(
                _provision_scenario,
                name,
                DEFAULT_IDP_PORT + i * 10,
                DEFAULT_STORE_PORT + i * 10,
                DEFAULT_OPA_PORT + i * 10,
            ): name
            for i, name in enumerate(SCENARIOS)
        }
        for future in as_completed(futures):
            name = futures[future]
            try:
                results[name] = future.result()
            except Exception as exc:  # noqa: BLE001 - isolate one scenario's worker crash from the rest
                log.exception("scenario %s: worker crashed, isolating from the rest of the session", name)
                results[name] = {"error": f"{exc!r}"}

    yield results


def _require_scenario(pipeline: dict[str, dict], scenario_name: str) -> dict:
    """Fetch a scenario's pipeline results, failing this one test clearly if that scenario's own
    setup raised (see the ``pipeline`` fixture's per-scenario try/except) — instead of letting one
    broken scenario cascade into an ``ERROR`` for the whole session."""
    result = pipeline[scenario_name]
    if "error" in result:
        pytest.fail(f"scenario {scenario_name!r} setup failed: {result['error']}")
    return result


# ======================================================================================
# Tests
# ======================================================================================


def _scenario_ids() -> list[str]:
    return list(SCENARIOS)


def _scenario_agent_cases() -> list[tuple[str, str]]:
    return [(name, agent_id) for name, scenario in SCENARIOS.items() for agent_id in scenario.AGENTS]


def _inbound_cases() -> list[tuple[str, str, str]]:
    return [
        (name, agent_id, subject)
        for name, scenario in SCENARIOS.items()
        for agent_id in scenario.AGENTS
        for subject in scenario.USERS
    ]


@pytest.mark.parametrize("scenario_name,agent_id,subject", _inbound_cases())
def test_inbound(
    pipeline: dict[str, dict], scenario_name: str, agent_id: str, subject: str, record_property
) -> None:
    """The generated inbound gate allows a user iff their role may reach that agent's own scope."""
    scenario = SCENARIOS[scenario_name]
    role = scenario.USERS[subject]
    agent = scenario.AGENTS[agent_id]
    agent_scopes = sorted(set(agent["inbound_scopes"]) | set(agent.get("delegation_scopes", {})))
    scenario_result = _require_scenario(pipeline, scenario_name)
    reasoning_by_scope = scenario_result["reasoning_by_scope"]

    expected = expected_inbound(scenario, subject, agent_id)
    record_property(
        "description",
        f"Can '{subject}' (subject, role '{role}') access '{agent_id}' (agent) in the "
        f"'{scenario_name}' scenario?",
    )
    record_property("expected", expected)
    record_property("expected_explanation", _inbound_explanation(scenario, subject, agent_id))

    rego = _rego_path(scenario_result["rego_dir"], agent_id, "inbound")
    if not rego.is_file():
        # Only agents the scenario declared as emergently unreachable can be missing here (the
        # fixture already raised for anything else) — confirm ground truth agrees no one reaches it.
        record_property("output", None)
        record_property("llm_reasoning", f"'{agent_id}' produced no inbound rego (declared unreachable)")
        assert not expected, f"{agent_id} produced no inbound rego but {subject} is expected to reach it"
        return

    allowed = opa_eval(
        [rego], "data.authbridge.client.inbound.request.allow", {"identity": {"subject": subject}}
    )
    record_property("output", allowed)
    record_property(
        "llm_reasoning",
        "\n".join(
            f"scope '{s}': {reasoning_by_scope.get(s, 'no reasoning recorded')}" for s in agent_scopes
        ),
    )
    assert allowed == expected


def _outbound_cases() -> list[tuple[str, str, str, str]]:
    cases = []
    for name, scenario in SCENARIOS.items():
        target_scopes = sorted(_tool_scope_names(scenario) | _agent_delegation_scope_names(scenario))
        for agent_id in scenario.AGENTS:
            for subject in scenario.USERS:
                for scope in target_scopes:
                    cases.append((name, agent_id, subject, scope))
    return cases


@pytest.mark.parametrize("scenario_name,agent_id,subject,scope", _outbound_cases())
def test_outbound(
    pipeline: dict[str, dict], scenario_name: str, agent_id: str, subject: str, scope: str, record_property
) -> None:
    """The generated outbound gate (via the generalized probe) allows a subject's call through
    ``agent_id`` to a target scope iff both the subject and that specific agent's own role are
    entitled to it."""
    scenario = SCENARIOS[scenario_name]
    role = scenario.USERS[subject]
    scenario_result = _require_scenario(pipeline, scenario_name)
    reasoning_by_scope = scenario_result["reasoning_by_scope"]
    reasoning_by_agent_role = scenario_result["reasoning_by_agent_role"]
    agent_role_names = sorted(scenario.AGENTS[agent_id]["roles"])

    expected = expected_outbound(scenario, subject, agent_id, scope)
    record_property(
        "description",
        f"Can '{subject}' (subject, role '{role}') reach scope '{scope}' through '{agent_id}' "
        f"(agent) in the '{scenario_name}' scenario?",
    )
    record_property("expected", expected)
    record_property("expected_explanation", _outbound_explanation(scenario, subject, agent_id, scope))

    rego = _rego_path(scenario_result["rego_dir"], agent_id, "outbound")
    if not rego.is_file():
        record_property("output", None)
        record_property("llm_reasoning", f"'{agent_id}' produced no outbound rego (declared unreachable)")
        assert not expected, f"{agent_id} produced no outbound rego but {subject}/{scope} is expected reachable"
        return

    target = _scope_owner(scenario, scope)
    fn = reformat_function_name(scope)  # soft-match rendering, e.g. quill-read -> Quill.Read
    allowed = opa_eval(
        [rego, HERE / "probe_eval.rego"],
        "data.probe.outbound_eval.allow",
        {"subject": subject, "target": target, "function_name": fn},
    )
    record_property("output", allowed)
    reasoning_lines = [f"subject-side (scope '{scope}'): {reasoning_by_scope.get(scope, 'no reasoning recorded')}"]
    reasoning_lines += [
        f"agent-side (role '{r}'): {reasoning_by_agent_role.get(r, 'no reasoning recorded')}"
        for r in agent_role_names
    ]
    record_property("llm_reasoning", "\n".join(reasoning_lines))
    assert allowed == expected


@pytest.mark.parametrize("scenario_name,agent_id", _scenario_agent_cases())
def test_outbound_unknown_target_denied(pipeline: dict[str, dict], scenario_name: str, agent_id: str) -> None:
    """An otherwise-allowed call to an unknown target is denied (target not in target_scopes)."""
    scenario = SCENARIOS[scenario_name]
    rego = _rego_path(_require_scenario(pipeline, scenario_name)["rego_dir"], agent_id, "outbound")
    if not rego.is_file():
        pytest.skip(f"{agent_id} produced no outbound rego in scenario {scenario_name}")
    subject = next(iter(scenario.USERS))
    allowed = opa_eval(
        [rego, HERE / "probe_eval.rego"],
        "data.probe.outbound_eval.allow",
        {"subject": subject, "target": "unknown-target", "function_name": "Some.Op"},
    )
    assert allowed is False


@pytest.mark.parametrize("scenario_name,agent_id", _scenario_agent_cases())
def test_outbound_soft_match_not_overbroad(pipeline: dict[str, dict], scenario_name: str, agent_id: str) -> None:
    """A function name whose tokens match no scope is denied — guards against soft-match over-match."""
    scenario = SCENARIOS[scenario_name]
    rego = _rego_path(_require_scenario(pipeline, scenario_name)["rego_dir"], agent_id, "outbound")
    if not rego.is_file():
        pytest.skip(f"{agent_id} produced no outbound rego in scenario {scenario_name}")
    subject = next(iter(scenario.USERS))
    target = next(iter({**scenario.AGENTS, **scenario.TOOLS}))
    allowed = opa_eval(
        [rego, HERE / "probe_eval.rego"],
        "data.probe.outbound_eval.allow",
        {"subject": subject, "target": target, "function_name": "delete_everything"},
    )
    assert allowed is False


@pytest.mark.parametrize("scenario_name", _scenario_ids())
@pytest.mark.parametrize("gate", ["inbound", "outbound_subject", "outbound_target"])
def test_grant_set_matches_truth_table(pipeline: dict[str, dict], scenario_name: str, gate: str) -> None:
    """The PRB's grant set for each gate equals the scenario truth table. Catches both under-grants
    (a missing pair) and over-grants (an unsupported pair) that the coarse allow/deny oracle above
    cannot see."""
    scenario = SCENARIOS[scenario_name]
    got = grant_sets(scenario, _require_scenario(pipeline, scenario_name)["rules"])[gate]
    want = truth(scenario)[gate]
    assert got == want, f"{scenario_name} {gate}: missing={want - got} extra={got - want}"


def _identity_confusion_scenario_ids() -> list[str]:
    return [name for name, scenario in SCENARIOS.items() if getattr(scenario, "IDENTITY_CONFUSION_PROBES", [])]


@pytest.mark.parametrize("scenario_name", _identity_confusion_scenario_ids())
def test_identity_confusion_probes(pipeline: dict[str, dict], scenario_name: str) -> None:
    """Optional per-scenario hook (Scenario 9): ``scenario.IDENTITY_CONFUSION_PROBES`` is a list of
    ``(subject, agent_id, expected)`` triples where ``subject`` is another agent's own
    service-account identity, asserting the inbound gate doesn't accidentally admit an agent
    identity that looks like a subject. Only scenarios that define probes are collected."""
    scenario = SCENARIOS[scenario_name]
    probes = scenario.IDENTITY_CONFUSION_PROBES
    scenario_result = _require_scenario(pipeline, scenario_name)
    for subject, agent_id, expected in probes:
        rego = _rego_path(scenario_result["rego_dir"], agent_id, "inbound")
        allowed = opa_eval(
            [rego], "data.authbridge.client.inbound.request.allow", {"identity": {"subject": subject}}
        )
        assert allowed == expected, (
            f"{scenario_name}: identity-confusion probe subject={subject!r} agent={agent_id!r}"
        )
