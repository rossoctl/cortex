"""LLM-client construction: provider-native vs explicit registry override.

These tests stub ALTK's ``get_llm`` so they exercise the kwarg-selection logic
in ``build_llm_client`` without authenticating against any real provider.
"""

from __future__ import annotations

from typing import Any

import altk.core.llm as llm_mod
from sparc_service.providers import build_llm_client
from sparc_service.settings import Settings


def _capture(monkeypatch) -> dict[str, Any]:
    captured: dict[str, Any] = {}

    class FakeClient:
        def __init__(self, **kwargs: Any) -> None:
            captured.update(kwargs)

    monkeypatch.setattr(llm_mod, "get_llm", lambda _registry_id: FakeClient)
    return captured


def test_native_watsonx_passes_provider_kwargs(monkeypatch):
    captured = _capture(monkeypatch)
    s = Settings(provider="watsonx", wx_api_key="k", wx_project_id="p", model="mistral-large-2512")
    build_llm_client(s)
    assert captured["project_id"] == "p"
    assert captured["model_name"] == "mistral-large-2512"


def test_registry_override_skips_provider_kwargs(monkeypatch):
    # With an explicit registry override, the watsonx-native project_id/api_base
    # kwargs must NOT be passed (they'd break a generic LiteLLM client). Only the
    # generic kwargs from llm_kwargs reach the constructor.
    captured = _capture(monkeypatch)
    s = Settings(
        provider="watsonx",
        wx_api_key="k",
        wx_project_id="p",
        model="gpt-4o-mini",
        llm_registry_id="litellm.output_val",
        llm_kwargs={"api_key": "sk-x"},
    )
    build_llm_client(s)
    assert "project_id" not in captured
    assert "api_base" not in captured
    assert captured["model_name"] == "gpt-4o-mini"
    assert captured["api_key"] == "sk-x"
