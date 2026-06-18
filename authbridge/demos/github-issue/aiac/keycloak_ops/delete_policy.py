"""Delete an applied access control policy from a Keycloak realm.

Removes all composite role mappings from realm roles assigned to users in the
config. The realm role assignments to users themselves are left intact.
"""

import json
from pathlib import Path
from typing import Any, Dict, List, Set

import yaml
from keycloak import KeycloakAdmin


def load_main_config(config_file: Path) -> Dict[str, Any]:
    if not config_file.exists():
        raise FileNotFoundError(f"Configuration file not found: {config_file}")
    with open(config_file, "r") as f:
        return yaml.safe_load(f)


def get_realm_role_composites(admin: KeycloakAdmin, realm: str, realm_role_name: str) -> List[Dict[str, Any]]:
    try:
        realm_role = admin.get_realm_role(realm_role_name)
        url = f"{admin.connection.base_url}/admin/realms/{realm}/roles-by-id/{realm_role['id']}/composites"
        response = admin.connection.raw_get(url)

        if hasattr(response, "json"):
            if response.status_code == 404:
                return []
            response.raise_for_status()
            return response.json()
        if isinstance(response, bytes):
            return json.loads(response.decode("utf-8"))
        if isinstance(response, list):
            return response
        return []
    except Exception as e:
        print(f"  Warning: Could not get composites for role '{realm_role_name}': {e}")
        return []


def remove_all_composites_from_realm_role(admin: KeycloakAdmin, realm: str, realm_role_name: str) -> None:
    try:
        composite_roles = get_realm_role_composites(admin, realm, realm_role_name)
        if not composite_roles:
            print(f"  No composite roles to remove from '{realm_role_name}'")
            return

        realm_role = admin.get_realm_role(realm_role_name)
        url = f"{admin.connection.base_url}/admin/realms/{realm}/roles-by-id/{realm_role['id']}/composites"
        admin.connection.raw_delete(url, data=json.dumps(composite_roles))

        composite_names = [
            f"{(r.get('clientRole', False) and r.get('containerId', 'client')) or 'realm'}.{r['name']}"
            for r in composite_roles
        ]
        print(
            f"  ✓ Removed {len(composite_roles)} composite role(s) from "
            f"'{realm_role_name}': {', '.join(composite_names)}"
        )
    except Exception as e:
        print(f"  ✗ Failed to remove composites from '{realm_role_name}': {e}")


def delete_access_control_policy(admin: KeycloakAdmin, realm: str, config_file: Path) -> None:
    """Remove all composite role mappings from realm roles used by config users."""
    main_config = load_main_config(config_file)
    users_config = main_config.get("users", [])

    if not users_config:
        print("No users found in configuration")
        return

    user_realm_roles: Set[str] = set()
    for user_config in users_config:
        user_realm_roles.update(user_config.get("roles", []))

    if not user_realm_roles:
        print("No realm roles assigned to users in configuration")
        return

    print(f"\n=== Removing composite role mappings from {len(user_realm_roles)} realm role(s) ===")
    print(f"Realm roles to process: {', '.join(sorted(user_realm_roles))}")

    for realm_role_name in sorted(user_realm_roles):
        print(f"\nProcessing realm role '{realm_role_name}':")
        remove_all_composites_from_realm_role(admin, realm, realm_role_name)
