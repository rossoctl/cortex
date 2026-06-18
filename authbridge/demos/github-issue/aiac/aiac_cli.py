#!/usr/bin/env python3
"""
AIAC CLI - AI Access Control Command Line Interface

This CLI orchestrates the complete end-to-end workflow for generating and applying
AI generated access control policies to Keycloak realms. It integrates the policy
generation agent with Keycloak management operations.

Full Pipeline Workflow:
    1. Export Configuration: Extract current Keycloak realm structure (e.g. clients, roles)
    2. Generate Policy: Use AIAC agent to convert natural language description to abstract policy
    3. Clean State: Remove existing mappings from realm roles
    4. Apply Policy: Apply the newly generated policy

"""

import argparse
import os
import sys
from pathlib import Path

# Add current directory to path to allow importing local modules
sys.path.insert(0, str(Path(__file__).parent))

from aiac_agent.agent.graph import PolicyBuilder
from aiac_agent.config import create_llm
from dotenv import load_dotenv
from keycloak_ops import (  # type: ignore[import-not-found]
    apply_access_control_policy,
    delete_access_control_policy,
    export_config,
    get_client_ids,
)

load_dotenv(dotenv_path="aiac.env", override=True)


class Colors:
    RED = "\033[0;31m"
    GREEN = "\033[0;32m"
    YELLOW = "\033[1;33m"
    BLUE = "\033[0;34m"
    NC = "\033[0m"


def print_step(message: str) -> None:
    print(f"{Colors.BLUE}{'=' * 51}{Colors.NC}")
    print(f"{Colors.BLUE}{message}{Colors.NC}")
    print(f"{Colors.BLUE}{'=' * 51}{Colors.NC}")


def print_success(message: str) -> None:
    print(f"{Colors.GREEN}✓ {message}{Colors.NC}")


def print_error(message: str) -> None:
    print(f"{Colors.RED}✗ {message}{Colors.NC}")


def print_info(message: str) -> None:
    print(f"{Colors.YELLOW}ℹ {message}{Colors.NC}")


def generate_policy_only(policy_file: Path, config_path: Path, output_file: str) -> None:
    """Run only the agent's natural-language → YAML step (no Keycloak)."""
    if not policy_file.exists():
        raise FileNotFoundError(f"Policy file not found: {policy_file}")

    with open(policy_file, "r") as f:
        policy_text = f.read().strip()

    # Load default model name from llm_conf.yaml
    import yaml

    llm_models_path = Path(__file__).parent / "aiac_agent" / "config" / "llm_conf.yaml"
    with open(llm_models_path) as f:
        llm_config = yaml.safe_load(f)
    default_model = llm_config.get("default_model", "gpt-oss")

    # Create LLM instance from llm_models.yaml using default model
    llm = create_llm(model_name=default_model, verbose=False)

    # Create PolicyBuilder with the LLM instance
    builder = PolicyBuilder(config_path=config_path, llm=llm)

    print("=" * 80)
    print("Generating access rule from textual policy...")
    print("=" * 80)
    print(f"\nPolicy file: {policy_file}")
    print(f"\nDescription:\n{policy_text}\n")

    result = builder.generate_policy(description=policy_text)

    if result["success"]:
        print("✓ Access rules generated successfully!\n")
        print("Generated YAML:")
        print("-" * 80)
        print(result["yaml_output"])
        print("-" * 80)
        builder.save_policy(result["yaml_output"], output_file)
    else:
        print("✗ Policy generation failed with errors:")
        for error in result["errors"]:
            print(f"  - {error}")

    print("\n" + "=" * 80)
    print("Parsed Role-to-Client-Role Mappings:")
    print("=" * 80)
    for role_mapping in result["parsed_scopes"]:
        realm_role = role_mapping["role"]
        client_roles = role_mapping.get("client_roles", [])
        print(f"  {realm_role}:")
        for cr in client_roles:
            print(f"    - {cr['client']}: {cr['role']}")


def _confirm_apply(policy_file: Path) -> bool:
    """Print the generated policy and ask for confirmation. Returns True to proceed."""
    print()
    print_step("Generated policy — review before applying to Keycloak")
    try:
        print(policy_file.read_text())
    except OSError as e:
        print_error(f"Could not read generated policy: {e}")
        return False
    print()
    try:
        answer = input("Apply this policy to Keycloak? [y/N] ").strip().lower()
    except EOFError:
        answer = ""
    return answer in ("y", "yes")


def run_full_pipeline(policy_text_file: str, policy_name: str | None, yes: bool = False) -> None:
    """Export realm → generate policy → (confirm) → delete old composites → apply new policy."""
    from keycloak import KeycloakAdmin

    script_dir = Path(__file__).parent
    policy_text_path = Path(policy_text_file)

    if not policy_text_path.exists():
        print_error(f"Policy text file not found: {policy_text_file}")
        sys.exit(1)

    if policy_name is None:
        policy_name = policy_text_path.stem

    print_info(f"Policy text file: {policy_text_file}")
    print_info(f"Policy name: {policy_name}")
    print()

    generated_configs_dir = script_dir / "generated_configs"
    generated_configs_dir.mkdir(exist_ok=True)

    config_file = generated_configs_dir / f"{policy_name}_config.yaml"
    policy_file = generated_configs_dir / f"{policy_name}_policy.yaml"
    main_config = "config.yaml"

    realm_name = os.getenv("REALM_NAME", "demo")
    keycloak_url = os.getenv("KEYCLOAK_URL")
    keycloak_user = os.getenv("KEYCLOAK_ADMIN_USERNAME")
    keycloak_pass = os.getenv("KEYCLOAK_ADMIN_PASSWORD")

    if not all([keycloak_url, keycloak_user, keycloak_pass]):
        raise ValueError(
            "Missing required environment variables. Please ensure aiac.env contains "
            "KEYCLOAK_URL, KEYCLOAK_ADMIN_USERNAME, and KEYCLOAK_ADMIN_PASSWORD"
        )

    try:
        print_step("Step 1: Exporting Keycloak configuration")
        print_info(f"Generating configuration file: {config_file}")
        print_info(f"Realm: {realm_name}")
        export_config(realm_name, str(config_file))
        print_success("Configuration exported successfully")
        print()

        print_step("Step 2: Generating access control rules from textual policy")
        print_info(f"Input: {policy_text_file}")
        print_info(f"Config: {config_file}")
        print_info(f"Output: {policy_file}")

        try:
            generate_policy_only(
                policy_text_path.resolve(),
                config_file.resolve(),
                str(policy_file.resolve()),
            )
            print_success("Policy generated successfully")
        except Exception as e:
            print_error("Failed to generate policy from textual description")
            print_error(f"Error: {e}")
            print_info("Possible reasons:")
            print("  - The policy description may be ambiguous or unclear")
            print("  - The LLM failed to generate a valid policy after maximum retries")
            print("  - The generated policy failed verification checks")
            print("  - Configuration file format issues")
            sys.exit(1)
        print()

        if not yes:
            if not _confirm_apply(policy_file):
                print_info("Aborted — no changes made to Keycloak.")
                sys.exit(0)
        print()

        print_step("Step 3: Removing old access control rules from Keycloak")
        admin = KeycloakAdmin(
            server_url=keycloak_url,
            username=keycloak_user,
            password=keycloak_pass,
            realm_name=realm_name,
            user_realm_name="master",
        )
        delete_access_control_policy(admin, realm_name, script_dir / main_config)
        print_success("Old rules removed successfully")
        print()

        print_step("Step 4: Applying new access control rules to Keycloak")
        client_ids = get_client_ids(admin)
        apply_access_control_policy(admin, realm_name, policy_file, client_ids)
        print_success("New rules applied successfully")
        print()

        print_step("Access rules Update Complete!")
        print_success(f"Policy '{policy_name}' has been successfully updated in Keycloak")
        print()
        print_info("Generated files:")
        print(f"  - Configuration: generated_configs/{config_file.name}")
        print(f"  - Rules: generated_configs/{policy_file.name}")

    except Exception as e:
        print_error(f"An error occurred: {e}")
        import traceback

        traceback.print_exc()
        sys.exit(1)


def main() -> None:
    if len(sys.argv) > 1 and sys.argv[1] == "generate":
        gen_parser = argparse.ArgumentParser(
            prog="aiac generate",
            description="Run only the policy generation step (no Keycloak).",
        )
        gen_parser.add_argument("policy_file", help="Path to natural-language policy description")
        gen_parser.add_argument("config", help="Path to realm config YAML")
        gen_parser.add_argument("output", help="Output YAML path")
        gen_args = gen_parser.parse_args(sys.argv[2:])
        generate_policy_only(Path(gen_args.policy_file), Path(gen_args.config), gen_args.output)
        return

    parser = argparse.ArgumentParser(
        prog="aiac",
        description="AI Access Control CLI (full Keycloak pipeline). "
        "Use 'aiac generate ...' to run only the agent's text->YAML step.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "policy_text_file",
        nargs="?",
        help="Path to natural-language policy file",
    )
    parser.add_argument("policy_name", nargs="?", help="Optional policy name override")
    parser.add_argument(
        "--yes",
        "-y",
        action="store_true",
        default=False,
        help="Skip confirmation prompt and apply the generated policy immediately.",
    )

    args = parser.parse_args()

    if not args.policy_text_file:
        parser.print_help()
        sys.exit(1)

    run_full_pipeline(args.policy_text_file, args.policy_name, yes=args.yes)


if __name__ == "__main__":
    main()
