"""Apply an access control policy to Keycloak realm roles.

Loads user-role -> client-role mappings from a policy YAML and applies them
as composite role mappings on realm roles.
"""

import json
from pathlib import Path
from typing import Dict, List, Optional

import yaml
from keycloak import KeycloakAdmin


def load_access_control_policy(
    access_control_policy_file: Path,
) -> Dict[str, List[Dict[str, str]]]:
    """Load and validate the access control policy YAML."""
    if not access_control_policy_file.exists():
        raise FileNotFoundError(f"Access control policy file not found: {access_control_policy_file}")

    with open(access_control_policy_file, "r") as f:
        policy_config = yaml.safe_load(f)

    policy = policy_config.get("policy", {}) or {}

    for user_role, client_roles in policy.items():
        if not isinstance(client_roles, list):
            raise ValueError(f"Invalid policy for user role '{user_role}': must be a list of client role mappings")
        for client_role in client_roles:
            if not isinstance(client_role, dict):
                raise ValueError(
                    f"Invalid client role mapping for user role '{user_role}': "
                    "must be a dict with 'client' and 'role' keys"
                )
            if "client" not in client_role or "role" not in client_role:
                raise ValueError(
                    f"Invalid client role mapping for user role '{user_role}': must contain 'client' and 'role' keys"
                )
            if not isinstance(client_role["client"], str) or not isinstance(client_role["role"], str):
                raise ValueError(
                    f"Invalid client role mapping for user role '{user_role}': 'client' and 'role' must be strings"
                )

    return policy


def get_client_ids(admin: KeycloakAdmin) -> Dict[str, str]:
    """Return a mapping of client name -> Keycloak internal id."""
    clients = admin.get_clients()
    return {client["clientId"]: client["id"] for client in clients}


def add_client_role_to_realm_role_composite(
    admin: KeycloakAdmin,
    realm: str,
    realm_role_name: str,
    client_id: str,
    client_role_name: str,
) -> None:
    """Add a single client role to a realm role's composite list."""
    client_role = admin.get_client_role(client_id, client_role_name)
    realm_role = admin.get_realm_role(realm_role_name)
    url = f"{admin.connection.base_url}/admin/realms/{realm}/roles-by-id/{realm_role['id']}/composites"
    admin.connection.raw_post(url, data=json.dumps([client_role]))


def apply_access_control_policy(
    admin: KeycloakAdmin,
    realm: str,
    access_control_policy_file: Path,
    client_ids: Dict[str, str],
    scope_ids: Optional[Dict[str, str]] = None,
) -> None:
    """Apply the policy by making realm roles composites of client roles."""
    user_role_to_client_roles = load_access_control_policy(access_control_policy_file)

    print("\n=== Making realm roles composites of client roles ===")
    for user_role, client_role_mappings in user_role_to_client_roles.items():
        print(f"\nProcessing realm role '{user_role}':")
        for mapping in client_role_mappings:
            client_name = mapping["client"]
            role_name = mapping["role"]

            if client_name not in client_ids:
                print(f"  Warning: Client '{client_name}' not found")
                continue

            client_id = client_ids[client_name]
            try:
                add_client_role_to_realm_role_composite(admin, realm, user_role, client_id, role_name)
                print(f"  ✓ Added client role '{client_name}.{role_name}' to realm role '{user_role}'")
            except Exception as e:
                print(f"  ℹ Client role '{client_name}.{role_name}' already in composite or error: {e}")
