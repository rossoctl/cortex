"""Settings parsing and provider-credential validation."""

from __future__ import annotations

from sparc_service.settings import Settings


def test_defaults_to_watsonx(clean_env):
    clean_env.setenv("WX_API_KEY", "key")
    clean_env.setenv("WX_PROJECT_ID", "proj")
    s = Settings.from_env()
    assert s.provider == "watsonx"
    assert s.registry_id == "litellm.watsonx.output_val"
    assert s.model == "mistral-large-2512"
    assert s.credentials_present()


def test_watsonx_missing_creds_records_error(clean_env):
    s = Settings.from_env()
    assert not s.credentials_present()
    assert any("WX_API_KEY" in e for e in s.errors)


def test_watsonx_fallback_env_names(clean_env):
    clean_env.setenv("WATSONX_API_KEY", "key")
    clean_env.setenv("WATSONX_PROJECT_ID", "proj")
    s = Settings.from_env()
    assert s.wx_api_key == "key"
    assert s.wx_project_id == "proj"
    assert s.credentials_present()


def test_ollama_provider_needs_no_watsonx(clean_env):
    clean_env.setenv("SPARC_LLM_PROVIDER", "ollama")
    s = Settings.from_env()
    assert s.provider == "ollama"
    assert s.registry_id == "litellm.ollama.output_val"
    assert s.model == "llama3.2:3b"
    assert s.credentials_present()


def test_openai_provider_requires_key(clean_env):
    clean_env.setenv("SPARC_LLM_PROVIDER", "openai")
    s = Settings.from_env()
    assert not s.credentials_present()
    clean_env.setenv("OPENAI_API_KEY", "sk-x")
    assert Settings.from_env().credentials_present()


def test_unsupported_provider_records_error(clean_env):
    clean_env.setenv("SPARC_LLM_PROVIDER", "bogus")
    s = Settings.from_env()
    assert any("unsupported SPARC_LLM_PROVIDER" in e for e in s.errors)


def test_model_override(clean_env):
    clean_env.setenv("SPARC_LLM_PROVIDER", "ollama")
    clean_env.setenv("SPARC_MODEL", "qwen2.5:7b")
    assert Settings.from_env().model == "qwen2.5:7b"


def test_generic_litellm_provider(clean_env):
    # The generic escape hatch supports any LiteLLM model (anthropic/gemini/...).
    clean_env.setenv("SPARC_LLM_PROVIDER", "litellm")
    clean_env.setenv("SPARC_MODEL", "anthropic/claude-3-5-sonnet")
    clean_env.setenv("SPARC_LLM_KWARGS_JSON", '{"api_key": "sk-ant-x"}')
    s = Settings.from_env()
    assert s.registry_id == "litellm.output_val"
    assert s.model == "anthropic/claude-3-5-sonnet"
    assert s.llm_kwargs == {"api_key": "sk-ant-x"}
    assert s.credentials_present()


def test_litellm_provider_requires_model(clean_env):
    clean_env.setenv("SPARC_LLM_PROVIDER", "litellm")
    s = Settings.from_env()
    assert any("requires SPARC_MODEL" in e for e in s.errors)


def test_azure_provider(clean_env):
    clean_env.setenv("SPARC_LLM_PROVIDER", "azure")
    clean_env.setenv("SPARC_MODEL", "azure/my-deployment")
    clean_env.setenv(
        "SPARC_LLM_KWARGS_JSON",
        '{"api_base": "https://x.openai.azure.com", "api_version": "2024-06-01", "api_key": "k"}',
    )
    s = Settings.from_env()
    assert s.registry_id == "litellm.output_val"
    assert s.llm_kwargs["api_version"] == "2024-06-01"
    assert s.credentials_present()


def test_invalid_kwargs_json_records_error(clean_env):
    clean_env.setenv("SPARC_LLM_PROVIDER", "litellm")
    clean_env.setenv("SPARC_MODEL", "anthropic/claude-3-5-sonnet")
    clean_env.setenv("SPARC_LLM_KWARGS_JSON", "{not json}")
    s = Settings.from_env()
    assert any("SPARC_LLM_KWARGS_JSON" in e for e in s.errors)


def test_registry_id_override(clean_env):
    clean_env.setenv("WX_API_KEY", "key")
    clean_env.setenv("WX_PROJECT_ID", "proj")
    clean_env.setenv("SPARC_LLM_REGISTRY_ID", "azure_openai.sync.output_val")
    assert Settings.from_env().registry_id == "azure_openai.sync.output_val"
