"""Environment-driven configuration for the SPARC reflection service.

Configuration is intentionally read from the environment (12-factor) so the
service can be tuned via a Kubernetes ConfigMap/Secret without rebuilding the
image. Watsonx credentials follow the same ``WX_*`` (with ``WATSONX_*``
fallbacks) convention used elsewhere in Kagenti.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field

# Supported SPARC LLM providers and the ALTK validating-client registry id each
# maps to. All entries are *validating* clients, which SPARC requires.
#
# watsonx (default) and ollama use ALTK's provider-native LiteLLM clients.
# Everything else — openai, azure (Azure OpenAI), and the generic "litellm"
# escape hatch — uses ALTK's generic LiteLLM client (litellm.output_val), which
# routes by the model string (e.g. "gpt-4o-mini", "azure/<deployment>",
# "anthropic/claude-3-5-sonnet", "gemini/gemini-1.5-pro", "bedrock/..."). This
# means ANY provider LiteLLM/ALTK supports works via config alone — set
# SPARC_LLM_PROVIDER=litellm, SPARC_MODEL=<litellm model>, and pass any extra
# client kwargs (api_base, api_version, api_key, ...) via SPARC_LLM_KWARGS_JSON.
# For full control you can also set SPARC_LLM_REGISTRY_ID to any ALTK registry id.
PROVIDER_REGISTRY_IDS: dict[str, str] = {
    "watsonx": "litellm.watsonx.output_val",
    "ollama": "litellm.ollama.output_val",
    "openai": "litellm.output_val",
    "azure": "litellm.output_val",
    "litellm": "litellm.output_val",
}

# Per-provider default model id (empty → SPARC_MODEL is required).
PROVIDER_DEFAULT_MODELS: dict[str, str] = {
    "watsonx": "mistral-large-2512",
    "ollama": "llama3.2:3b",
    "openai": "gpt-4o-mini",
    "azure": "",
    "litellm": "",
}

# SPARC reflection tracks exposed via configuration. Mapped to altk Track enum
# lazily in providers.py to avoid importing heavy altk modules at settings time.
SUPPORTED_TRACKS = {"fast_track", "slow_track", "syntax", "spec_free", "transformations_only"}


def _truthy(value: str) -> bool:
    return value.strip().lower() in {"1", "true", "yes", "on"}


def _int_env(name: str, default: int) -> int:
    raw = os.getenv(name, "").strip()
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError:
        return default


@dataclass(frozen=True)
class Settings:
    """Immutable, validated view of the service configuration."""

    provider: str = "watsonx"
    model: str = "mistral-large-2512"
    track: str = "fast_track"

    # SPARC execution tuning.
    llm_timeout_seconds: int = 120
    retries: int = 3
    max_parallel: int = 2
    include_raw_response: bool = True

    # Watsonx credentials (provider == "watsonx").
    wx_api_key: str = ""
    wx_project_id: str = ""
    wx_url: str = "https://us-south.ml.cloud.ibm.com"

    # Ollama (provider == "ollama").
    ollama_base_url: str = "http://localhost:11434"
    ollama_api_key: str = "ollama"

    # OpenAI (provider == "openai").
    openai_api_key: str = ""
    openai_base_url: str = ""

    # Generic LiteLLM client kwargs (provider in {openai, azure, litellm}), e.g.
    # {"api_base": "...", "api_version": "...", "api_key": "..."}. Parsed from
    # SPARC_LLM_KWARGS_JSON. Passed straight to the ALTK litellm client.
    llm_kwargs: dict = field(default_factory=dict)
    # Optional explicit ALTK registry id override (advanced; any ALTK client).
    llm_registry_id: str = ""

    # HTTP server.
    host: str = "0.0.0.0"
    port: int = 8090

    # Validation errors collected at load time (provider creds missing, etc.).
    errors: tuple[str, ...] = field(default_factory=tuple)

    @property
    def registry_id(self) -> str:
        return self.llm_registry_id or PROVIDER_REGISTRY_IDS[self.provider]

    def credentials_present(self) -> bool:
        return not self.errors

    @classmethod
    def from_env(cls) -> "Settings":
        provider = os.getenv("SPARC_LLM_PROVIDER", "watsonx").strip().lower()
        errors: list[str] = []
        if provider not in PROVIDER_REGISTRY_IDS:
            errors.append(
                f"unsupported SPARC_LLM_PROVIDER={provider!r}; expected one of {sorted(PROVIDER_REGISTRY_IDS)}"
            )
            provider = "watsonx"

        track = os.getenv("SPARC_TRACK", "fast_track").strip().lower()
        if track not in SUPPORTED_TRACKS:
            errors.append(f"unsupported SPARC_TRACK={track!r}; expected one of {sorted(SUPPORTED_TRACKS)}")
            track = "fast_track"

        wx_api_key = os.getenv("WX_API_KEY") or os.getenv("WATSONX_API_KEY") or ""
        wx_project_id = os.getenv("WX_PROJECT_ID") or os.getenv("WATSONX_PROJECT_ID") or ""
        wx_url = os.getenv("WX_URL") or os.getenv("WATSONX_URL") or "https://us-south.ml.cloud.ibm.com"

        model = os.getenv("SPARC_MODEL") or os.getenv("WX_MODEL_ID") or PROVIDER_DEFAULT_MODELS.get(provider, "")

        # Optional generic LiteLLM client kwargs (azure/openai/litellm).
        llm_kwargs: dict = {}
        raw_kwargs = os.getenv("SPARC_LLM_KWARGS_JSON", "").strip()
        if raw_kwargs:
            try:
                parsed = json.loads(raw_kwargs)
                if isinstance(parsed, dict):
                    llm_kwargs = parsed
                else:
                    errors.append("SPARC_LLM_KWARGS_JSON must be a JSON object")
            except ValueError as exc:
                errors.append(f"SPARC_LLM_KWARGS_JSON is not valid JSON: {exc}")

        # Provider-specific credential / config validation.
        if provider == "watsonx" and (not wx_api_key or not wx_project_id):
            errors.append("provider=watsonx requires WX_API_KEY and WX_PROJECT_ID")
        if provider == "openai" and not (os.getenv("OPENAI_API_KEY") or "api_key" in llm_kwargs):
            errors.append("provider=openai requires OPENAI_API_KEY (or api_key in SPARC_LLM_KWARGS_JSON)")
        if provider in ("azure", "litellm") and not model:
            errors.append(
                f"provider={provider} requires SPARC_MODEL (e.g. azure/<deployment> or anthropic/claude-3-5-sonnet)"
            )

        return cls(
            provider=provider,
            model=model,
            track=track,
            llm_timeout_seconds=_int_env("SPARC_LLM_TIMEOUT", 120),
            retries=_int_env("SPARC_RETRIES", 3),
            max_parallel=_int_env("SPARC_MAX_PARALLEL", 2),
            include_raw_response=_truthy(os.getenv("SPARC_INCLUDE_RAW_RESPONSE", "true")),
            wx_api_key=wx_api_key,
            wx_project_id=wx_project_id,
            wx_url=wx_url,
            ollama_base_url=os.getenv("OLLAMA_BASE_URL", "http://localhost:11434"),
            ollama_api_key=os.getenv("OLLAMA_API_KEY", "ollama"),
            openai_api_key=os.getenv("OPENAI_API_KEY", ""),
            openai_base_url=os.getenv("OPENAI_BASE_URL", ""),
            llm_kwargs=llm_kwargs,
            llm_registry_id=os.getenv("SPARC_LLM_REGISTRY_ID", "").strip(),
            host=os.getenv("HOST", "0.0.0.0"),
            port=_int_env("PORT", 8090),
            errors=tuple(errors),
        )
