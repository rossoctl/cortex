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

        # ``build_llm_client`` applies the retry/debug/reasoning wrappers to the
        # class, so ``generate`` and ``generate_async`` must be present on the
        # stub even though this test never invokes them.
        def generate(self, *a: Any, **kw: Any) -> Any:  # pragma: no cover
            return None

        async def generate_async(self, *a: Any, **kw: Any) -> Any:  # pragma: no cover
            return None

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


def test_empty_response_retry_recovers_after_two_failures():
    """Deterministic guard for `_patch_empty_response_retry`.

    Simulate two consecutive ``ValueError("No content or tool calls found in
    response")`` failures followed by a success, then assert the wrapper caught
    both, retried, and returned the successful result.
    """
    import asyncio

    from sparc_service.providers import _patch_empty_response_retry

    calls = {"n": 0}

    class FakeClient:
        async def generate_async(self, *args, **kwargs):
            calls["n"] += 1
            if calls["n"] < 3:
                raise ValueError("No content or tool calls found in response")
            return {"ok": True, "attempt": calls["n"]}

    _patch_empty_response_retry(FakeClient, max_retries=3)
    result = asyncio.run(FakeClient().generate_async())
    assert result == {"ok": True, "attempt": 3}
    assert calls["n"] == 3


def test_empty_response_retry_reraises_unrelated_valueerror():
    """A ValueError with a different message must NOT be swallowed by the wrapper."""
    import asyncio
    import pytest

    from sparc_service.providers import _patch_empty_response_retry

    class FakeClient:
        async def generate_async(self, *args, **kwargs):
            raise ValueError("some other error")

    _patch_empty_response_retry(FakeClient, max_retries=3)
    with pytest.raises(ValueError, match="some other error"):
        asyncio.run(FakeClient().generate_async())


def test_patches_are_idempotent():
    """All three ``_patch_*`` helpers must no-op on a client class already patched.

    Without the sentinel guards, ``ReflectionEngine``'s per-track lazy build
    would wrap the same class N times, exploding retry counts and duplicating
    debug lines. This asserts the sentinel path is taken.
    """
    from sparc_service.providers import (
        _patch_debug_logging,
        _patch_empty_response_retry,
        _patch_watsonx_for_reasoning_models,
    )

    class FakeClient:
        def generate(self, *a, **kw):
            return "raw"

        async def generate_async(self, *a, **kw):
            return "raw"

    # Apply each patch twice; second call must be a no-op (sentinel bit already set).
    _patch_watsonx_for_reasoning_models(FakeClient)
    first_generate = FakeClient.generate
    first_generate_async = FakeClient.generate_async
    _patch_watsonx_for_reasoning_models(FakeClient)
    assert FakeClient.generate is first_generate
    assert FakeClient.generate_async is first_generate_async

    _patch_empty_response_retry(FakeClient, max_retries=1)
    after_retry_wrapper = FakeClient.generate_async
    _patch_empty_response_retry(FakeClient, max_retries=1)
    assert FakeClient.generate_async is after_retry_wrapper

    _patch_debug_logging(FakeClient)
    after_debug_wrapper = FakeClient.generate_async
    _patch_debug_logging(FakeClient)
    assert FakeClient.generate_async is after_debug_wrapper
