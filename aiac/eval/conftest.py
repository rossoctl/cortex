"""Per-run pass/fail/skip report for the policy-eval-scenarios, policy-eval-robustness,
policy-eval-consistency, policy-eval-correctness-prb, and policy-eval-correctness-e2e suites
(spec: ``docs/specs/eval/policy-eval-scenarios.md``, ``docs/specs/eval/
policy-eval-robustness-consistency.md``, ``docs/specs/eval/policy-eval-correctness-prb.md``, and
``docs/specs/eval/policy-eval-correctness-e2e.md``).

Every run of ``test_policy_pipeline_eval.py`` (``@pytest.mark.eval_extended``),
``test_policy_pipeline_consistency.py`` (``@pytest.mark.eval_consistency``),
``test_policy_pipeline_robustness.py`` (``@pytest.mark.eval_robustness``),
``test_policy_pipeline_correctness_prb.py`` (``@pytest.mark.eval_correctness_prb``), or
``test_policy_pipeline_correctness_e2e.py`` (``@pytest.mark.eval_correctness_e2e``) writes a
Markdown report to ``reports/`` listing every collected test's outcome — passed, failed, skipped,
xfailed, xpassed, or a setup/collection error. All six sections are always present (even empty) so
a reader can see at a glance that nothing was silently omitted. Failed/error entries carry the
assertion's crash message (pytest's own computed diff, e.g. "assert True == False" or a custom
mismatch message with expected/actual sets); skipped/xfailed entries carry the skip reason; every
entry carries the test function's docstring so a reader doesn't have to open the source file to
know what was actually being checked. The report is scoped to these five markers (not just "any
test collected while this conftest happens to be loaded"), so running the whole repo's test suite
from a parent directory does not pull unrelated tests into this suite's report.

Filename: ``reports/report_<DD_MM_HH_MM_SS>.md``, timestamped in UTC (override via
``EVAL_REPORT_TZ``, e.g. ``Asia/Jerusalem``), e.g. ``report_04_08_16_37_22.md`` for 04 Aug at
16:37:22 UTC. Regenerated (not appended) per run — old reports are left on disk for history but
are gitignored, same as ``rego_out/``.

``test_inbound``/``test_outbound`` (the per-cell tests sweeping every scenario x agent x
subject[/scope] combination) additionally ``record_property`` a concrete per-cell description plus
an expected/actual boolean and explanation -- read back here via ``report.user_properties`` and
rendered as "What it tests" / "Expected output" / "Output" instead of the generic docstring +
crash-message fallback used by every other test in this suite (see ``_render_entry``).

``test_prb_correctness`` (the correctness-prb suite) and ``test_e2e_correctness`` (the
correctness-e2e suite) similarly ``record_property``s precision/recall/denial-precision plus the
over-grants/under-grants/incorrectly-denied pair breakdown per scenario -- rendered as its own
metrics + detail block, always (pass or fail), since the tracked-but-non-gating under-grant/denial
detail is otherwise invisible on a passing run. The render branch dispatches generically on the
presence of ``precision``/``recall`` properties, so it covers both suites with no per-suite
special-casing.

A scenario whose own *setup* fails (a Keycloak/PRB/PCE error before ``score_scenario`` ever runs --
these two suites isolate a failing scenario's setup per-scenario, so this is common, not
exceptional) never gets those properties recorded at all. Such an entry still gets the crash detail
*and* the same six-field metrics block, values marked ``unavailable`` with why -- identified by
nodeid (``::test_prb_correctness[``/``::test_e2e_correctness[``, see ``_CORRECTNESS_TEST_MARKERS``)
since there are no properties to dispatch on -- rather than silently falling back to the generic
docstring + crash-message rendering every other test in this suite gets.

Both suites also ``record_property("best_effort_notes", ...)`` -- a ``{scope_or_role_name:
reason}`` dict naming every decision that fell back to a never-approved PRB proposal instead of
aborting the scenario (``eval.test_policy_pipeline_eval.orchestrate_prb``'s ``best_effort=True``,
by explicit user request so a scenario the auditor partly rejects still scores). When non-empty,
``_render_metrics_block`` appends one more field listing them, with an explicit caveat that those
pairs don't represent real production behavior.
"""

from __future__ import annotations

import os
from datetime import datetime
from pathlib import Path
from zoneinfo import ZoneInfo

import pytest
from dotenv import load_dotenv

HERE = Path(__file__).resolve().parent
REPORTS_DIR = HERE / "reports"
REPORT_TZ = ZoneInfo(os.environ.get("EVAL_REPORT_TZ", "UTC"))
MARKERS = {
    "eval_extended",
    "eval_consistency",
    "eval_robustness",
    "eval_correctness_prb",
    "eval_correctness_e2e",
}

# Auto-load eval/.env so LLM_BASE_URL/KEYCLOAK_URL/etc. are set without having to
# `set -a; . eval/.env; set +a` before invoking pytest. Existing environment
# variables take precedence (override=False), so CI/shell exports still win.
load_dotenv(HERE / ".env", override=False)

_docstrings: dict[str, str] = {}
_reports: dict[str, pytest.TestReport] = {}


def pytest_collection_modifyitems(session: pytest.Session, config: pytest.Config, items: list) -> None:
    for item in items:
        if not (MARKERS & set(item.keywords)):
            continue
        func = getattr(item, "obj", None)
        doc = (getattr(func, "__doc__", None) or "").strip()
        if doc:
            # First paragraph only — the rest is often maintainer-facing rationale.
            _docstrings[item.nodeid] = " ".join(doc.split("\n\n")[0].split())


def pytest_runtest_logreport(report: pytest.TestReport) -> None:
    if report.when == "teardown" and report.outcome == "passed":
        return
    if not (MARKERS & set(report.keywords)):
        return
    # A later phase (call) supersedes an earlier one (setup) for the same nodeid; a setup or
    # teardown failure has no later phase to supersede it.
    _reports[report.nodeid] = report


def _categorize(report: pytest.TestReport) -> str:
    wasxfail = getattr(report, "wasxfail", None) is not None
    if report.when in ("setup", "teardown") and report.outcome == "failed":
        return "error"
    if report.outcome == "passed":
        return "xpassed" if wasxfail else "passed"
    if report.outcome == "failed":
        return "xpassed" if wasxfail else "failed"  # strict-xfail unexpected pass -> reported failed
    if report.outcome == "skipped":
        return "xfailed" if wasxfail else "skipped"
    return report.outcome


def _detail(report: pytest.TestReport, category: str) -> str | None:
    """Full crash/skip detail — pytest's own computed expected-vs-actual diff for failures, the
    literal ``pytest.skip()``/``xfail()`` reason for skips."""
    longrepr = report.longrepr
    if longrepr is None:
        return None
    if category in ("failed", "error"):
        crash = getattr(longrepr, "reprcrash", None)
        if crash is not None:
            return str(crash.message).strip()
        return str(longrepr).strip().splitlines()[-1]
    if category in ("skipped", "xfailed"):
        if isinstance(longrepr, tuple) and len(longrepr) == 3:
            reason = str(longrepr[2])
            for prefix in ("Skipped: ", "XFAIL: ", "XFAIL "):
                if reason.startswith(prefix):
                    reason = reason[len(prefix):]
            return reason
        return str(longrepr).strip()
    return None


def _render_field(lines: list[str], label: str, text: str) -> None:
    """Append a ``- **label:** text`` bullet, code-fencing ``text`` if it spans multiple lines."""
    if "\n" in text:
        lines.append(f"- **{label}:**")
        lines.append("  ```")
        lines.extend(f"  {line}" for line in text.splitlines())
        lines.append("  ```")
    else:
        lines.append(f"- **{label}:** {text}")


def _format_pairs_dict(pairs_by_gate: dict) -> str:
    """Render a ``{gate: [(role, scope), ...]}`` dict (as produced by ``ScenarioScore.over_grants``
    /``under_grants``/``incorrectly_denied``) as one line per non-empty gate, or ``"none"``."""
    if not pairs_by_gate:
        return "none"
    return "\n".join(
        f"{gate}: " + ", ".join(f"({r}, {s})" for r, s in pairs) for gate, pairs in sorted(pairs_by_gate.items())
    )


# Nodeid substrings identifying the two correctness suites' single test function each (parametrized
# by scenario name) — used to give a scenario whose *setup* failed (before score_scenario ever ran,
# so none of precision/recall/etc got record_property'd) the same six-field metrics shape every
# other entry gets, instead of silently omitting it. See `_render_entry`'s middle branch.
_CORRECTNESS_TEST_MARKERS = ("::test_prb_correctness[", "::test_e2e_correctness[")


def _format_best_effort_notes(notes: dict[str, str]) -> str:
    """Render a ``{scope_or_role_name: reason}`` dict (``orchestrate_prb``'s ``best_effort_notes``)
    as one line per entry, or ``"none"``."""
    if not notes:
        return "none"
    return "\n".join(f"{name}: {reason}" for name, reason in sorted(notes.items()))


def _render_metrics_block(lines: list[str], props: dict, *, unavailable_reason: str | None = None) -> None:
    """Render the precision/recall/denial-precision + over-/under-grant/incorrect-denial breakdown
    ``test_prb_correctness``/``test_e2e_correctness`` record. When ``unavailable_reason`` is given
    (the scenario's own setup failed before scoring could run, so ``props`` has none of this),
    render the same six fields with a uniform placeholder instead — so a reader always sees the
    same shape, pass or fail, setup-failed or scored."""
    if unavailable_reason is not None:
        for label in ("Precision", "Recall", "Denial precision", "Over-grants", "Under-grants", "Incorrectly denied"):
            lines.append(f"- **{label}:** unavailable — {unavailable_reason}")
        return
    lines.append(f"- **Precision:** {props['precision']:.3f}")
    lines.append(f"- **Recall:** {props['recall']:.3f}")
    lines.append(f"- **Denial precision:** {props['denial_precision']:.3f}")
    _render_field(lines, "Over-grants", _format_pairs_dict(props.get("over_grants", {})))
    _render_field(lines, "Under-grants", _format_pairs_dict(props.get("under_grants", {})))
    _render_field(lines, "Incorrectly denied", _format_pairs_dict(props.get("incorrectly_denied", {})))
    best_effort_notes = props.get("best_effort_notes", {})
    if best_effort_notes:
        _render_field(
            lines,
            "Best-effort proposals used (not real production behavior — the auditor never approved these)",
            _format_best_effort_notes(best_effort_notes),
        )


def _render_entry(lines: list[str], nodeid: str, report: pytest.TestReport, category: str) -> None:
    """Per-cell tests (``test_inbound``/``test_outbound``) ``record_property`` a concrete
    description + expected/actual boolean + explanation; ``test_prb_correctness`` (correctness-prb)
    ``record_property``s precision/recall/denial-precision + the over-/under-grant/incorrect-denial
    pair breakdown; render each instead of the generic docstring + crash/skip-reason fallback every
    other test in this suite gets. A correctness-suite scenario whose *setup* failed (a pipeline
    error before ``score_scenario`` ever ran) gets the crash detail *and* the same six-field metrics
    block, marked unavailable with why — not silently dropped to the generic fallback."""
    lines.append(f"### `{nodeid}`")
    props = dict(report.user_properties)
    if "expected" in props and "output" in props:
        description = props.get("description") or _docstrings.get(nodeid)
        if description:
            lines.append(f"- **What it tests:** {description}")
        _render_field(
            lines, "Expected output", f"{props['expected']} — {props.get('expected_explanation', '')}"
        )
        _render_field(lines, "Output", f"{props['output']} — {props.get('llm_reasoning', '')}")
    elif "precision" in props and "recall" in props:
        description = _docstrings.get(nodeid)
        if description:
            lines.append(f"- **What it tests:** {description}")
        _render_metrics_block(lines, props)
    elif any(marker in nodeid for marker in _CORRECTNESS_TEST_MARKERS):
        doc = _docstrings.get(nodeid)
        if doc:
            lines.append(f"- **What it tests:** {doc}")
        detail = _detail(report, category)
        if detail:
            _render_field(lines, "Failure", detail)
        _render_metrics_block(lines, props, unavailable_reason="scenario setup failed before scoring could run")
    else:
        doc = _docstrings.get(nodeid)
        if doc:
            lines.append(f"- **What it tests:** {doc}")
        detail = _detail(report, category)
        if detail:
            label = "Reason" if category in ("skipped", "xfailed") else "Failure"
            _render_field(lines, label, detail)
    lines.append("")


def pytest_sessionfinish(session: pytest.Session, exitstatus: int) -> None:
    if not _reports:
        return  # this session collected none of this suite's tests -- nothing to report

    order = ["failed", "passed", "error", "xpassed", "xfailed", "skipped"]
    buckets: dict[str, list[tuple[str, pytest.TestReport]]] = {cat: [] for cat in order}
    for nodeid, report in _reports.items():
        buckets[_categorize(report)].append((nodeid, report))
    for cat in buckets:
        buckets[cat].sort(key=lambda pair: pair[0])

    now = datetime.now(REPORT_TZ)
    total = len(_reports)
    lines = [
        "# policy-eval-scenarios test report",
        "",
        f"Run: {now.isoformat()}",
        f"Exit status: {exitstatus}",
        f"Total: {total} — " + ", ".join(f"{cat}={len(buckets[cat])}" for cat in order),
        "",
    ]
    for cat in order:
        entries = buckets[cat]
        lines.append(f"## {cat} ({len(entries)})")
        lines.append("")
        if not entries:
            lines.append("_none_")
            lines.append("")
            continue
        for nodeid, report in entries:
            _render_entry(lines, nodeid, report, cat)

    REPORTS_DIR.mkdir(parents=True, exist_ok=True)
    suffix = now.strftime("%d_%m_%H_%M_%S")
    report_path = REPORTS_DIR / f"report_{suffix}.md"
    report_path.write_text("\n".join(lines))
    print(f"\npolicy-eval-scenarios report written to {report_path}")
