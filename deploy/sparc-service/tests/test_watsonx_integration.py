"""Opt-in integration test against real watsonx.

Skipped unless RUN_WATSONX_TESTS=1 and WX_* credentials are present. Validates
the canonical finance scenario end-to-end through the real SPARC engine:
an ungrounded/inappropriate tool call must be rejected.

Run with:
    RUN_WATSONX_TESTS=1 pytest tests/test_watsonx_integration.py -v
"""

from __future__ import annotations

import json
import os

import pytest
from sparc_service.engine import ReflectionEngine
from sparc_service.models import ReflectRequest
from sparc_service.settings import Settings

pytestmark = pytest.mark.skipif(
    os.getenv("RUN_WATSONX_TESTS") != "1" or not (os.getenv("WX_API_KEY") or os.getenv("WATSONX_API_KEY")),
    reason="set RUN_WATSONX_TESTS=1 and WX_* creds to run watsonx integration tests",
)

TOOL_SPECS = [
    {
        "type": "function",
        "function": {
            "name": "get_transaction",
            "description": "Fetch transaction details by transaction id.",
            "parameters": {
                "type": "object",
                "properties": {"transaction_id": {"type": "string", "description": "Exact transaction identifier."}},
                "required": ["transaction_id"],
            },
        },
    }
]


def _engine() -> ReflectionEngine:
    return ReflectionEngine(Settings.from_env())


def test_ungrounded_partial_id_is_rejected():
    """User gives partial 'TX482'; model hallucinates 'TX4821' -> reject."""
    req = ReflectRequest(
        messages=[
            {"role": "user", "content": "Refund transaction TX482 because it was a duplicate charge."},
            {"role": "assistant", "content": "I will process the refund."},
        ],
        tool_specs=TOOL_SPECS,
        tool_calls=[
            {
                "id": "call_1",
                "type": "function",
                "function": {"name": "get_transaction", "arguments": json.dumps({"transaction_id": "TX4821"})},
            }
        ],
        session_id="itest-reject",
    )
    resp = _engine().reflect(req)
    assert resp.decision in {"reject", "error"}
    assert resp.decision == "reject", f"expected reject, got {resp.decision}: {resp.issues}"
    assert resp.issues, "reject should carry at least one issue"


def test_grounded_full_id_is_approved():
    """Once the user provides the exact id, the same call is grounded -> approve."""
    req = ReflectRequest(
        messages=[
            {"role": "user", "content": "Look up transaction TX4827 for me."},
        ],
        tool_specs=TOOL_SPECS,
        tool_calls=[
            {
                "id": "call_1",
                "type": "function",
                "function": {"name": "get_transaction", "arguments": json.dumps({"transaction_id": "TX4827"})},
            }
        ],
        session_id="itest-approve",
    )
    resp = _engine().reflect(req)
    assert resp.decision == "approve", f"expected approve, got {resp.decision}: {resp.issues}"
