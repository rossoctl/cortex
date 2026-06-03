"""HTTP surface tests via FastAPI TestClient with an injected fake engine."""

from __future__ import annotations

import json

from fastapi.testclient import TestClient
from sparc_service.api import create_app
from sparc_service.settings import Settings

from tests.conftest import make_engine, make_reflection_output

REQUEST_BODY = {
    "messages": [{"role": "user", "content": "Refund transaction TX482."}],
    "tool_specs": [
        {
            "type": "function",
            "function": {
                "name": "get_transaction",
                "parameters": {
                    "type": "object",
                    "properties": {"transaction_id": {"type": "string"}},
                    "required": ["transaction_id"],
                },
            },
        }
    ],
    "tool_calls": [
        {
            "id": "c1",
            "type": "function",
            "function": {"name": "get_transaction", "arguments": json.dumps({"transaction_id": "TX4821"})},
        }
    ],
    "session_id": "s1",
}


def _client(output) -> TestClient:
    return TestClient(create_app(engine=make_engine(output)))


def test_reflect_reject():
    output = make_reflection_output(
        "reject",
        issues=[
            {"issue_type": "semantic_function", "metric_name": "m", "explanation": "no refund tool", "correction": None}
        ],
        overall_avg_score=2.0,
    )
    resp = _client(output).post("/reflect", json=REQUEST_BODY)
    assert resp.status_code == 200
    body = resp.json()
    assert body["decision"] == "reject"
    assert body["overall_avg_score"] == 2.0
    assert body["issues"][0]["explanation"] == "no refund tool"


def test_reflect_requires_tool_calls():
    bad = dict(REQUEST_BODY, tool_calls=[])
    resp = _client(make_reflection_output("approve")).post("/reflect", json=bad)
    assert resp.status_code == 422  # pydantic validation: min_length=1


def test_healthz_reports_provider():
    resp = _client(make_reflection_output("approve")).get("/healthz")
    assert resp.status_code == 200
    assert resp.json()["provider"] == "watsonx"


def test_readyz_ok_with_buildable_component():
    resp = _client(make_reflection_output("approve")).get("/readyz")
    assert resp.status_code == 200
    assert resp.json()["status"] == "ready"


def test_readyz_503_when_config_invalid():
    # Settings with a recorded error -> not ready, never builds component.
    settings = Settings(provider="watsonx", errors=("provider=watsonx requires WX_API_KEY and WX_PROJECT_ID",))
    engine = make_engine(make_reflection_output("approve"), settings)
    client = TestClient(create_app(engine=engine))
    resp = client.get("/readyz")
    assert resp.status_code == 503


def test_reflect_502_hides_backend_error():
    # Provider exception text can embed endpoints/credentials, so /reflect must
    # never echo it — it returns a stable message and logs the detail instead.
    class BoomEngine:
        settings = Settings(provider="watsonx", wx_api_key="k", wx_project_id="p")

        def reflect(self, request):
            raise RuntimeError("watsonx exploded with key sk-secret-123")

        def ready(self):
            return True, None

    client = TestClient(create_app(engine=BoomEngine()))
    resp = client.post("/reflect", json=REQUEST_BODY)
    assert resp.status_code == 502
    body = json.dumps(resp.json())
    assert "sk-secret-123" not in body
    assert "watsonx exploded" not in body
    assert resp.json()["detail"]["error"] == "reflection failed"


def test_reflect_400_on_unsupported_track():
    bad = dict(REQUEST_BODY, track="not_a_track")
    resp = _client(make_reflection_output("approve")).post("/reflect", json=bad)
    assert resp.status_code == 400
