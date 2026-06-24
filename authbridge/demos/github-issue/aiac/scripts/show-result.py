#!/usr/bin/env python3
"""
show-result — display composite-role mappings currently active in Keycloak.

Reads aiac.env for connection details, config.yaml for the expected realm-role
list, then queries the Keycloak admin API for the live composite mappings.
Also prints the last generated policy file for side-by-side comparison.

Run via: make show-result
"""

import argparse
import os
import sys
from pathlib import Path

AIAC_DIR = Path(__file__).parent.parent
sys.path.insert(0, str(AIAC_DIR))

import yaml  # noqa: E402
from dotenv import load_dotenv  # noqa: E402
from keycloak import KeycloakAdmin  # noqa: E402

load_dotenv(dotenv_path=AIAC_DIR / "aiac.env", override=True)

REALM = os.getenv("REALM_NAME", "kagenti")
KEYCLOAK_URL = os.getenv("KEYCLOAK_URL")
KEYCLOAK_USER = os.getenv("KEYCLOAK_ADMIN_USERNAME")
KEYCLOAK_PASS = os.getenv("KEYCLOAK_ADMIN_PASSWORD")

if not all([KEYCLOAK_URL, KEYCLOAK_USER, KEYCLOAK_PASS]):
    print("ERROR: KEYCLOAK_URL, KEYCLOAK_ADMIN_USERNAME, KEYCLOAK_ADMIN_PASSWORD must be set in aiac.env")
    sys.exit(1)

BOLD = "\033[1m"
CYAN = "\033[36m"
GREEN = "\033[0;32m"
YELLOW = "\033[1;33m"
RED = "\033[0;31m"
RESET = "\033[0m"


def section(title: str) -> None:
    print(f"\n{BOLD}{CYAN}{'─' * 58}{RESET}")
    print(f"{BOLD}{CYAN}  {title}{RESET}")
    print(f"{BOLD}{CYAN}{'─' * 58}{RESET}")


def main() -> None:
    parser = argparse.ArgumentParser(description="Display composite-role mappings currently active in Keycloak")
    parser.add_argument(
        "--config-path",
        type=Path,
        help="Path to the RBAC configuration YAML file",
    )
    args = parser.parse_args()

    try:
        admin = KeycloakAdmin(
            server_url=KEYCLOAK_URL,
            username=KEYCLOAK_USER,
            password=KEYCLOAK_PASS,
            realm_name=REALM,
            user_realm_name="master",
        )
    except Exception as e:
        print(f"{RED}ERROR: Could not connect to Keycloak at {KEYCLOAK_URL}: {e}{RESET}")
        sys.exit(1)

    config_path = args.config_path
    if not config_path.exists():
        print(f"{RED}ERROR: Config file not found: {config_path}{RESET}")
        sys.exit(1)

    with open(config_path) as f:
        config = yaml.safe_load(f)

    realm_role_names = [r["name"] for r in config.get("realm_roles", [])]

    section(f"Active composite-role mappings  (realm: {REALM})")

    for role_name in realm_role_names:
        try:
            realm_role = admin.get_realm_role(role_name)
            url = f"{admin.connection.base_url}/admin/realms/{REALM}/roles-by-id/{realm_role['id']}/composites"
            response = admin.connection.raw_get(url)
            composites = response.json() if response.content else []
        except Exception as e:
            print(f"  {YELLOW}[!] Could not fetch role '{role_name}': {e}{RESET}")
            continue

        print(f"\n  {BOLD}{role_name}{RESET}")
        if not composites:
            print(f"    {YELLOW}(no composite roles assigned){RESET}")
        else:
            for c in sorted(composites, key=lambda x: (x.get("containerName", ""), x.get("name", ""))):
                container = c.get("containerName", "?")
                name = c.get("name", "?")
                print(f"    {GREEN}✓{RESET}  {container}.{name}")

    gen_dir = AIAC_DIR / "config"
    policy_files = list(gen_dir.glob("*_policy.yaml")) if gen_dir.exists() else []
    if policy_files:
        latest = max(policy_files, key=lambda p: p.stat().st_mtime_ns)
        section(f"Last generated policy: {latest.name}")
        print(latest.read_text())
    else:
        print(f"\n{YELLOW}(no generated policy files — run 'make apply-policy' first){RESET}")


if __name__ == "__main__":
    main()
