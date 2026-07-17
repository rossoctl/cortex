"""
LLM Configuration Module


Supports multiple backends: RITS, ete-litellm, Ollama, and other OpenAI-compatible endpoints.
"""

import os
import warnings
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, Optional

import yaml
from dotenv import load_dotenv
from langchain_core.language_models import BaseChatModel
from langchain_openai import ChatOpenAI
from pydantic import SecretStr

# Default generation parameters
DEFAULT_TEMPERATURE = 0.0
DEFAULT_MAX_TOKENS = 8192
DEFAULT_TIMEOUT = 360
DEFAULT_MAX_RETRIES = 2


@dataclass
class LLMConfig:
    """Configuration for LLM."""

    model: str
    endpoint: Optional[str]
    api_key: Optional[str]
    temperature: float
    max_tokens: int
    timeout: int
    retries: int


def load_llm_models_yaml(yaml_path: Optional[Path] = None) -> Dict[str, Any]:
    """
    Load LLM models configuration from YAML file.

    Args:
        yaml_path: Path to llm_conf.yaml file (optional, defaults to llm_conf.yaml in config dir)

    Returns:
        Dict containing models configuration

    Raises:
        FileNotFoundError: If YAML file doesn't exist
        ValueError: If YAML is invalid
    """
    if yaml_path is None:
        config_dir = Path(__file__).parent
        yaml_path = config_dir / "llm_conf.yaml"

    if not yaml_path.exists():
        raise FileNotFoundError(f"LLM models configuration file not found: {yaml_path}")

    with open(yaml_path, "r") as f:
        config = yaml.safe_load(f)

    if not config or "models" not in config:
        raise ValueError(f"Invalid LLM models configuration in {yaml_path}")

    return config


def load_llm_config_from_yaml(model_name: str, yaml_path: Optional[Path] = None) -> LLMConfig:
    """
    Load LLM configuration for a specific model from YAML file.

    Args:
        model_name: Name of the model to load (e.g., 'claude-haiku', 'gpt-nano')
        yaml_path: Path to llm_conf.yaml file (optional)

    Returns:
        LLMConfig: Configuration object

    Raises:
        FileNotFoundError: If YAML file doesn't exist
        ValueError: If model not found in configuration
    """
    config = load_llm_models_yaml(yaml_path)

    if model_name not in config["models"]:
        available = ", ".join(config["models"].keys())
        raise ValueError(f"Model '{model_name}' not found in configuration. Available models: {available}")

    model_config = config["models"][model_name]

    return LLMConfig(
        model=model_config["model"],
        endpoint=model_config.get("endpoint"),
        api_key=model_config.get("api_key"),
        temperature=model_config.get("temperature", DEFAULT_TEMPERATURE),
        max_tokens=model_config.get("max_tokens", DEFAULT_MAX_TOKENS),
        timeout=model_config.get("timeout", DEFAULT_TIMEOUT),
        retries=model_config.get("max_retries", DEFAULT_MAX_RETRIES),
    )


def load_llm_config_from_env(env_path: Optional[Path] = None) -> LLMConfig:
    """
    Load LLM configuration from environment file (llm.env) or environment variables.

    Supports multiple backends:
    - RITS (IBM): Uses RITS_API_KEY header
    - ete-litellm: Standard OpenAI-compatible
    - Ollama: Local OpenAI-compatible API

    Args:
        env_path: Path to llm.env file (optional, defaults to llm.env in config dir)
                  If the path doesn't exist, will read from environment variables only.

    Returns:
        LLMConfig: Configuration object

    Raises:
        FileNotFoundError: If default llm.env file doesn't exist (when env_path is None)
        ValueError: If required configuration is missing
    """
    # Determine env file path
    if env_path is None:
        config_dir = Path(__file__).parent
        env_path = config_dir / "llm.env"
        # Only raise error for default path
        if not env_path.exists():
            raise FileNotFoundError(f"LLM configuration file not found: {env_path}")

    # Load environment variables from llm.env if file exists
    if env_path.exists():
        load_dotenv(dotenv_path=env_path, override=True)

    # Read configuration from environment variables
    model = os.getenv("LLM_MODEL", "").strip()
    endpoint = os.getenv("LLM_ENDPOINT", "").strip() or None
    api_key = os.getenv("LLM_API_KEY", "").strip() or None

    if not model:
        raise ValueError("LLM_MODEL not set in environment")

    # Read generation parameters with defaults
    temperature_str = os.getenv("LLM_TEMPERATURE", "").strip()
    max_tokens_str = os.getenv("LLM_MAX_TOKENS", "").strip()
    timeout_str = os.getenv("LLM_TIMEOUT", "").strip()
    retries_str = os.getenv("LLM_MAX_RETRIES", "").strip()

    temperature = float(temperature_str) if temperature_str else DEFAULT_TEMPERATURE
    max_tokens = int(max_tokens_str) if max_tokens_str else DEFAULT_MAX_TOKENS
    timeout = int(timeout_str) if timeout_str else DEFAULT_TIMEOUT
    retries = int(retries_str) if retries_str else DEFAULT_MAX_RETRIES

    return LLMConfig(
        model=model,
        endpoint=endpoint,
        api_key=api_key,
        temperature=temperature,
        max_tokens=max_tokens,
        timeout=timeout,
        retries=retries,
    )


def is_rits_endpoint(endpoint: str) -> bool:
    """Check if endpoint is a RITS endpoint based on hostname."""
    return "rits.fmaas" in endpoint.lower()


def create_llm(
    model_name: Optional[str] = None,
    env_path: Optional[Path] = None,
    yaml_path: Optional[Path] = None,
    verbose: bool = True,
) -> BaseChatModel:
    """
    Create and configure a LangChain LLM instance from configuration.

    Supports two configuration methods:
    1. YAML-based: Pass model_name to load from llm_conf.yaml
    2. ENV-based: Pass env_path to load from .env file (legacy)

    If neither is specified, defaults to llm.env in config directory.

    Automatically detects backend type (RITS, litellm, Ollama) and configures accordingly.

    Args:
        model_name: Name of model from llm_conf.yaml (e.g., 'claude-haiku', 'gpt-nano')
        env_path: Path to llm.env file (optional, for legacy .env-based config)
        yaml_path: Path to llm_conf.yaml file (optional, defaults to config dir)
        verbose: If True, print initialization messages. If False, suppress output.

    Returns:
        BaseChatModel: Configured LangChain LLM instance

    Raises:
        ValueError: If required configuration is missing
        FileNotFoundError: If configuration file doesn't exist
    """
    # Load LLM configuration
    if model_name:
        # Load from YAML
        llm_config = load_llm_config_from_yaml(model_name, yaml_path)
    else:
        # Load from .env (legacy)
        llm_config = load_llm_config_from_env(env_path)

    # Validate required fields for create_llm
    if not llm_config.endpoint:
        raise ValueError("LLM_ENDPOINT is required to create an LLM instance")
    if not llm_config.api_key:
        raise ValueError("LLM_API_KEY is required to create an LLM instance")

    if verbose:
        print("🤖 Initializing LLM")
        print(f"   Model: {llm_config.model}")
        print(f"   Endpoint: {llm_config.endpoint}")
        print(f"   Temperature: {llm_config.temperature}")
        print(f"   Max Tokens: {llm_config.max_tokens}")
        print(f"   Timeout: {llm_config.timeout}s")
        print(f"   Max Retries: {llm_config.retries}")

    # Detect if this is a RITS endpoint
    is_rits = is_rits_endpoint(llm_config.endpoint)

    # Create ChatOpenAI instance with appropriate configuration
    # Suppress the UserWarning about max_tokens in model_kwargs
    with warnings.catch_warnings():
        warnings.filterwarnings("ignore", message="Parameters .* should be specified explicitly", category=UserWarning)

        if is_rits:
            # RITS uses custom header for API key
            if verbose:
                print("   Backend: RITS (using RITS_API_KEY header)")
            llm = ChatOpenAI(
                model=llm_config.model,
                temperature=llm_config.temperature,
                max_retries=llm_config.retries,
                timeout=llm_config.timeout,
                api_key=SecretStr("none"),  # Not used, RITS uses header
                base_url=llm_config.endpoint,
                default_headers={"RITS_API_KEY": llm_config.api_key},
                model_kwargs={"max_tokens": llm_config.max_tokens},
            )
        else:
            # Standard OpenAI-compatible endpoint (litellm, Ollama, etc.)
            if verbose:
                backend_type = "Ollama" if "ollama" in llm_config.api_key.lower() else "OpenAI-compatible"
                print(f"   Backend: {backend_type}")
            llm = ChatOpenAI(
                model=llm_config.model,
                temperature=llm_config.temperature,
                max_retries=llm_config.retries,
                timeout=llm_config.timeout,
                api_key=SecretStr(llm_config.api_key),
                base_url=llm_config.endpoint,
                model_kwargs={"max_tokens": llm_config.max_tokens},
            )

    if verbose:
        print("✅ LLM initialized successfully")
    return llm


# Note: No global LLM instance created here to avoid "Already borrowed" errors
# Each PolicyBuilder should create its own LLM instance by calling create_llm()
# or by not passing an llm parameter (which will call create_llm() internally)

# For backward compatibility with code that imports llm directly,
# we create it on demand, but this should be avoided in favor of
# creating fresh instances per PolicyBuilder
llm = None  # Will be created on first use


def get_default_llm() -> BaseChatModel:
    """
    Get the default LLM instance, creating it if needed.
    Note: For better isolation, prefer creating new instances with create_llm().

    Returns:
        BaseChatModel: Default LLM instance
    """
    global llm
    if llm is None:
        llm = create_llm()
    return llm
