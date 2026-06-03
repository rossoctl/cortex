"""ALTK LLM-client and SPARC-component construction.

Isolates every import of the heavy ``altk`` package so the rest of the service
(settings, models, API wiring) stays import-light and unit-testable without
network access or LLM credentials.
"""

from __future__ import annotations

import os
from typing import Any

from .settings import Settings

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

    if native and settings.provider == "watsonx":
        return client_cls(
            model_name=settings.model,
            api_key=settings.wx_api_key,
            project_id=settings.wx_project_id,
            api_base=settings.wx_url,
            timeout=settings.llm_timeout_seconds,
        )

    if native and settings.provider == "ollama":
        # Point every LiteLLM Ollama call at the configured server. We pass
        # api_url EXPLICITLY (the ALTK ollama client's param) so ALTK's internal
        # metric sub-clients inherit it — relying on the OLLAMA_API_BASE env var
        # alone is not enough (those sub-calls otherwise fall back to
        # localhost:11434). The env vars are set too as a belt-and-suspenders.
        os.environ["OLLAMA_API_BASE"] = settings.ollama_base_url
        os.environ["OLLAMA_BASE_URL"] = settings.ollama_base_url
        return client_cls(
            model_name=settings.model,
            api_key=settings.ollama_api_key,
            api_url=settings.ollama_base_url,
        )

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
    return client_cls(model_name=settings.model, **lite_kwargs)


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
