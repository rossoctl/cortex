"""ALTK LLM-client and SPARC-component construction.

Isolates every import of the heavy ``altk`` package so the rest of the service
(settings, models, API wiring) stays import-light and unit-testable without
network access or LLM credentials.
"""

from __future__ import annotations

import logging
import os
from typing import Any

from .settings import Settings

_debug_log = logging.getLogger("sparc_service.llm_debug")

# Map the configurable track names to altk Track enum members. Resolved lazily.
_TRACK_NAMES = {
    "fast_track": "FAST_TRACK",
    "slow_track": "SLOW_TRACK",
    "syntax": "SYNTAX",
    "spec_free": "SPEC_FREE",
    "transformations_only": "TRANSFORMATIONS_ONLY",
}


def resolve_track(name: str):
    """Return the altk ``Track`` enum member for a configured track name."""
    from altk.pre_tool.core import Track

    return getattr(Track, _TRACK_NAMES[name])


def _patch_watsonx_for_reasoning_models(client_cls):
    """Patch LLM client to inject schema via system prompt instead of response_format.

    Some models don't support response_format structured output (WatsonX reasoning
    models return output in reasoning_content not content; Haiku-4-5 via IBM LiteLLM
    proxy also lacks response_format support). ALTK's _parse_llm_response only reads
    content, so response_format mode always raises 'No content or tool calls found in
    response'. Injecting the schema into the system prompt (same as ALTK's Ollama
    provider) makes the model return valid JSON in content.
    See: https://github.com/kagenti/kagenti-extensions/issues/676
    """
    import functools

    original_generate = client_cls.generate
    original_generate_async = client_cls.generate_async

    @functools.wraps(original_generate)
    def patched_generate(self, *args, **kwargs):
        # Force-override (not setdefault) — ALTK passes schema_field="response_format"
        # explicitly, so setdefault would be a no-op and the reasoning model would
        # return output in reasoning_content instead of content, causing
        # "No content or tool calls found in response". See ISSUE-676.
        kwargs["schema_field"] = None
        kwargs["include_schema_in_system_prompt"] = True
        return original_generate(self, *args, **kwargs)

    @functools.wraps(original_generate_async)
    async def patched_generate_async(self, *args, **kwargs):
        kwargs["schema_field"] = None
        kwargs["include_schema_in_system_prompt"] = True
        return await original_generate_async(self, *args, **kwargs)

    client_cls.generate = patched_generate
    client_cls.generate_async = patched_generate_async


def _patch_empty_response_retry(client_cls, max_retries: int = 3):
    """Wrap generate_async to retry on 'No content or tool calls found in response'.

    ALTK's ValidatingLLMClient.generate_async retry loop catches only
    OutputValidationError. When the IBM LiteLLM proxy returns empty content,
    _parse_llm_response raises ValueError (not OutputValidationError), so all 3
    configured retries are bypassed and the metric fails immediately.

    This wrapper catches that ValueError and retries up to max_retries times with
    a short back-off before re-raising, giving the proxy a chance to succeed on a
    subsequent attempt.

    See ISSUE-019 in docs/open-issues.md and upstream ALTK tracker.
    """
    import asyncio
    import functools

    original_generate_async = client_cls.generate_async

    @functools.wraps(original_generate_async)
    async def patched_generate_async(self, *args, **kwargs):
        last_exc = None
        for attempt in range(1, max_retries + 2):  # max_retries extra attempts
            try:
                return await original_generate_async(self, *args, **kwargs)
            except ValueError as exc:
                if "No content or tool calls found in response" not in str(exc):
                    raise
                last_exc = exc
                if attempt <= max_retries:
                    _debug_log.debug(
                        "[LLM_DEBUG] empty-response retry %d/%d after ValueError: %s",
                        attempt, max_retries, exc,
                    )
                    await asyncio.sleep(0.5 * attempt)
                else:
                    _debug_log.debug(
                        "[LLM_DEBUG] empty-response retry exhausted (%d attempts)", max_retries + 1
                    )
        raise last_exc  # type: ignore[misc]

    client_cls.generate_async = patched_generate_async


def _patch_debug_logging(client_cls):
    """Wrap generate_async to log the exact prompt and raw response.

    Activated only when SPARC_DEBUG_LLM=true. Logs at DEBUG level so normal
    runs are unaffected. Each log line is prefixed [LLM_DEBUG] for easy grep:

        kubectl logs -n kagenti-system deploy/sparc-service | grep LLM_DEBUG
    """
    import functools
    import json as _json

    original_generate_async = client_cls.generate_async

    @functools.wraps(original_generate_async)
    async def patched_generate_async(self, prompt, *args, **kwargs):
        schema = kwargs.get("schema")
        schema_field = kwargs.get("schema_field", "response_format")
        retries = kwargs.get("retries", "?")

        prompt_full = _json.dumps(prompt, ensure_ascii=False) if isinstance(prompt, list) else str(prompt)
        schema_full = _json.dumps(schema, ensure_ascii=False) if isinstance(schema, dict) else str(type(schema))

        _debug_log.debug(
            "[LLM_DEBUG] >>> generate_async called\n"
            "  schema_field=%s  retries=%s\n"
            "  schema=%s\n"
            "  prompt=%s",
            schema_field, retries, schema_full, prompt_full,
        )

        try:
            result = await original_generate_async(self, prompt, *args, **kwargs)
            result_preview = _json.dumps(result, ensure_ascii=False)[:400] if isinstance(result, dict) else str(result)[:400]
            _debug_log.debug("[LLM_DEBUG] <<< generate_async SUCCESS result=%s", result_preview)
            return result
        except Exception as exc:
            _debug_log.debug("[LLM_DEBUG] <<< generate_async ERROR %s: %s", type(exc).__name__, exc)
            raise

    client_cls.generate_async = patched_generate_async


def build_llm_client(settings: Settings):
    """Construct a validating ALTK LLM client for the configured provider.

    watsonx (default) and ollama use ALTK's provider-native LiteLLM validating
    clients. Every other provider — openai, azure (Azure OpenAI), and the
    generic ``litellm`` escape hatch — uses ALTK's generic LiteLLM validating
    client (``litellm.output_val``), where the model string selects the provider
    and any extra client kwargs come from ``SPARC_LLM_KWARGS_JSON``. SPARC
    requires a *validating* client, so every branch returns one.
    """
    from altk.core.llm import get_llm

    client_cls = get_llm(settings.registry_id)

    # An explicit SPARC_LLM_REGISTRY_ID override means the caller picked the ALTK
    # client class directly, so its constructor may not accept the provider-native
    # kwargs below (e.g. watsonx's project_id). In that case skip the native
    # branches and use the generic LiteLLM-kwargs path, where everything the
    # client needs comes from SPARC_LLM_KWARGS_JSON.
    native = not settings.llm_registry_id

    debug = os.getenv("SPARC_DEBUG_LLM", "").strip().lower() in ("1", "true", "yes")

    if native and settings.provider in ["watsonx", "litellm.watsonx"]:
        client = client_cls(
            model_name=settings.model,
            api_key=settings.wx_api_key,
            project_id=settings.wx_project_id,
            api_base=settings.wx_url,
            timeout=settings.llm_timeout_seconds,
        )
        _patch_watsonx_for_reasoning_models(client_cls)
        _patch_empty_response_retry(client_cls, max_retries=settings.retries)
        if debug:
            _patch_debug_logging(client_cls)
        return client

    if native and settings.provider == "ollama":
        # Point every LiteLLM Ollama call at the configured server. We pass
        # api_url EXPLICITLY (the ALTK ollama client's param) so ALTK's internal
        # metric sub-clients inherit it — relying on the OLLAMA_API_BASE env var
        # alone is not enough (those sub-calls otherwise fall back to
        # localhost:11434). The env vars are set too as a belt-and-suspenders.
        os.environ["OLLAMA_API_BASE"] = settings.ollama_base_url
        os.environ["OLLAMA_BASE_URL"] = settings.ollama_base_url
        client = client_cls(
            model_name=settings.model,
            api_key=settings.ollama_api_key,
            api_url=settings.ollama_base_url,
        )
        if debug:
            _patch_debug_logging(client_cls)
        return client

    # Generic path (openai / azure / litellm, or any provider when
    # SPARC_LLM_REGISTRY_ID is set). LiteLLM routes by the model string and reads
    # provider API keys from the environment (OPENAI_API_KEY, AZURE_API_KEY,
    # ANTHROPIC_API_KEY, ...) when not supplied; SPARC_LLM_KWARGS_JSON supplies
    # anything else (api_base, api_version, api_key, deployment, ...). When a
    # registry override selects a non-LiteLLM client, supply its constructor
    # kwargs via SPARC_LLM_KWARGS_JSON.
    lite_kwargs: dict[str, Any] = dict(settings.llm_kwargs)
    lite_kwargs.setdefault("timeout", settings.llm_timeout_seconds)
    if settings.provider == "openai" and settings.openai_base_url and "api_base" not in lite_kwargs:
        lite_kwargs["api_base"] = settings.openai_base_url
    client = client_cls(model_name=settings.model, **lite_kwargs)
    # IBM LiteLLM proxy (and other non-native providers) have the same empty-content
    # issue as WatsonX reasoning models: ALTK passes schema_field="response_format"
    # explicitly, but the proxy returns an empty content field when response_format
    # is active, causing "No content or tool calls found in response". Apply the same
    # system-prompt injection patch so the model returns JSON in content.
    if settings.provider == "litellm":
        _patch_watsonx_for_reasoning_models(client_cls)
        _patch_empty_response_retry(client_cls, max_retries=settings.retries)
    if debug:
        _patch_debug_logging(client_cls)
    return client


def build_component(settings: Settings):
    """Construct the SPARC reflection component from settings.

    This performs provider authentication eagerly for watsonx/openai (the
    validating clients authenticate on construction), so callers should treat a
    raised exception as "not ready".
    """
    # Mirror the cost-map flag the reference reflector sets for LiteLLM.
    os.environ.setdefault("LITELLM_LOCAL_MODEL_COST_MAP", "True")

    from altk.core.toolkit import ComponentConfig
    from altk.pre_tool.core import SPARCExecutionMode
    from altk.pre_tool.sparc import SPARCReflectionComponent

    config = ComponentConfig(llm_client=build_llm_client(settings))
    component = SPARCReflectionComponent(
        config=config,
        track=resolve_track(settings.track),
        execution_mode=SPARCExecutionMode.ASYNC,
        include_raw_response=settings.include_raw_response,
        retries=settings.retries,
        max_parallel=settings.max_parallel,
    )
    init_error = getattr(component, "_initialization_error", None)
    if init_error:
        raise RuntimeError(f"SPARC component failed to initialize: {init_error}")
    return component
