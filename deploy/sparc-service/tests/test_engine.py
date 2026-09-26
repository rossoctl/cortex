"""Engine IO mapping, using a fake SPARC component (no network)."""

from __future__ import annotations

import json

from sparc_service.models import ReflectRequest

from tests.conftest import make_engine, make_reflection_output

FINANCE_TOOL_SPECS = [
    {
        "type": "function",
        "function": {
            "name": "get_transaction",
            "description": "Fetch transaction details by transaction id.",
            "parameters": {
                "type": "object",
                "properties": {"transaction_id": {"type": "string"}},
                "required": ["transaction_id"],
            },
        },
    }
]


def _request(args: dict) -> ReflectRequest:
    return ReflectRequest(
        messages=[{"role": "user", "content": "Refund transaction TX482."}],
        tool_specs=FINANCE_TOOL_SPECS,
        tool_calls=[
            {
                "id": "call_1",
                "type": "function",
                "function": {"name": "get_transaction", "arguments": json.dumps(args)},
            }
        ],
        session_id="s1",
    )


def test_approve_maps_cleanly():
    engine = make_engine(make_reflection_output("approve", overall_avg_score=4.5))
    resp = engine.reflect(_request({"transaction_id": "TX4827"}))
    assert resp.decision == "approve"
    assert resp.issues == []
    assert resp.overall_avg_score == 4.5
    assert resp.execution_time_ms == 12.3


def test_reject_carries_issue_and_correction():
    output = make_reflection_output(
        "reject",
        issues=[
            {
                "issue_type": "semantic_function",
                "metric_name": "function_selection_appropriateness",
                "explanation": "get_transaction does not process refunds.",
                "correction": {"corrected_function_name": "no_function"},
            }
        ],
        overall_avg_score=2.0,
    )
    engine = make_engine(output)
    resp = engine.reflect(_request({"transaction_id": "TX4821"}))
    assert resp.decision == "reject"
    assert len(resp.issues) == 1
    issue = resp.issues[0]
    assert issue.metric_name == "function_selection_appropriateness"
    assert "refund" in issue.explanation
    assert issue.correction["corrected_function_name"] == "no_function"
    assert resp.overall_avg_score == 2.0


def test_error_decision_passthrough():
    engine = make_engine(make_reflection_output("error"))
    resp = engine.reflect(_request({"transaction_id": "TX4821"}))
    assert resp.decision == "error"


def test_raw_pipeline_suppressed_when_disabled():
    from sparc_service.settings import Settings

    settings = Settings(provider="watsonx", wx_api_key="k", wx_project_id="p", include_raw_response=False)
    engine = make_engine(make_reflection_output("approve", overall_avg_score=5.0), settings)
    resp = engine.reflect(_request({"transaction_id": "TX4827"}))
    # Score is still surfaced, but the verbose raw pipeline is withheld.
    assert resp.overall_avg_score == 5.0
    assert resp.raw_pipeline_result is None


def test_run_input_receives_request_fields():
    engine = make_engine(make_reflection_output("approve"))
    engine.reflect(_request({"transaction_id": "TX4827"}))
    # The fake records the run_input the engine handed to SPARC.
    component = next(iter(engine._components.values()))  # noqa: SLF001 - test introspection
    assert len(component.calls) == 1
    run_input = component.calls[0]
    # All three SPARC inputs reach the component unchanged.
    assert run_input.messages == [{"role": "user", "content": "Refund transaction TX482."}]
    assert run_input.tool_specs == FINANCE_TOOL_SPECS
    assert run_input.tool_calls[0]["function"]["name"] == "get_transaction"
    assert run_input.tool_calls[0]["function"]["arguments"] == json.dumps({"transaction_id": "TX4827"})


def test_unsupported_track_rejected():
    engine = make_engine(make_reflection_output("approve"))
    request = _request({"transaction_id": "TX4827"})
    request.track = "made_up_track"
    try:
        engine.reflect(request)
    except ValueError as exc:
        assert "unsupported track" in str(exc)
    else:
        raise AssertionError("expected ValueError for an unsupported track")
