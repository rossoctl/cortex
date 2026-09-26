"""Shared test fixtures.

The unit tests never touch the network or LLM credentials: they inject a fake
SPARC component (built from real altk result types) into the engine.
"""

from __future__ import annotations

import os
from types import SimpleNamespace
from typing import Any

import pytest
from sparc_service.engine import ReflectionEngine
from sparc_service.settings import Settings


def make_reflection_output(
    decision: str,
    issues: list[dict[str, Any]] | None = None,
    overall_avg_score: float | None = None,
    execution_time_ms: float = 12.3,
) -> Any:
    """Build a real altk reflection output object for fakes."""
    from altk.pre_tool.core import (
        SPARCReflectionDecision,
        SPARCReflectionIssue,
        SPARCReflectionResult,
    )

    issue_objs = [SPARCReflectionIssue(**i) for i in (issues or [])]
    reflection = SPARCReflectionResult(decision=SPARCReflectionDecision(decision), issues=issue_objs)
    raw_pipeline = {}
    if overall_avg_score is not None:
        raw_pipeline["overall_avg_score"] = overall_avg_score
    return SimpleNamespace(
        output=SimpleNamespace(
            reflection_result=reflection,
            execution_time_ms=execution_time_ms,
            raw_pipeline_result=raw_pipeline,
        )
    )


class FakeComponent:
    """Stands in for SPARCReflectionComponent; returns a canned verdict."""

    def __init__(self, output: Any) -> None:
        self._output = output
        self.calls: list[Any] = []

    def process(self, run_input: Any, phase: Any) -> Any:
        self.calls.append(run_input)
        return self._output


def make_engine(output: Any, settings: Settings | None = None) -> ReflectionEngine:
    settings = settings or Settings(provider="watsonx", wx_api_key="k", wx_project_id="p")
    component = FakeComponent(output)
    return ReflectionEngine(settings, component_factory=lambda _track: component)


@pytest.fixture
def clean_env(monkeypatch):
    for key in list(os.environ):
        if key.startswith(("SPARC_", "WX_", "WATSONX_", "OLLAMA_", "OPENAI_")):
            monkeypatch.delenv(key, raising=False)
    yield monkeypatch
