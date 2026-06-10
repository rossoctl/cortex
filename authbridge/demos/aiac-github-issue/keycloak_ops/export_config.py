"""Export a Keycloak realm's configuration to a config.yaml-compatible file."""

import os
from pathlib import Path
from typing import Any, Dict, List

import yaml
from dotenv import load_dotenv
from keycloak import KeycloakAdmin


def get_client_roles(admin: KeycloakAdmin, client_id: str) -> List[str]:
    """Get all roles for a specific client."""
    try:
        roles = admin.get_client_roles(client_id)
        return [role["name"] for role in roles]
    except Exception as e:
        print(f"  Warning: Could not get roles for client {client_id}: {e}")
        return []


def get_realm_roles(admin: KeycloakAdmin) -> List[str]:
    """Get all realm roles, excluding default Keycloak roles."""
    try:
        roles = admin.get_realm_roles()
        # Filter out default Keycloak roles
        default_roles = {"default-roles-", "offline_access", "uma_authorization"}
        custom_roles = []
        for role in roles:
            role_name = role["name"]
            # Skip default roles and roles that start with default-roles-
            if not any(role_name.startswith(dr) or role_name == dr for dr in default_roles):
                custom_roles.append(role_name)
        return custom_roles
    except Exception as e:
        print(f"  Warning: Could not get realm roles: {e}")
        return []


def get_client_scope_mappings(admin: KeycloakAdmin, realm: str, client_id: str) -> Dict[str, List[str]]:
    """Get scope mappings for a client (which target client roles are in scope)."""
    try:
        url = f"{admin.connection.base_url}/admin/realms/{realm}/clients/{client_id}/scope-mappings/clients"
        response = admin.connection.raw_get(url)

        # Handle different response types from raw_get
        import json

        if hasattr(response, "json"):
            # It's a Response object
            if response.status_code == 404:
                # No scope mappings exist for this client
                return {}
            response.raise_for_status()
            data = response.json()
        elif isinstance(response, bytes):
            data = json.loads(response.decode("utf-8"))
        else:
            data = response

        scope_mappings: Dict[str, List[str]] = {}
        if data and isinstance(data, list):
            for target_client in data:
                if not isinstance(target_client, dict):
                    continue
                target_client_id = target_client.get("id")
                target_client_name = target_client.get("client")

                if not target_client_id or not target_client_name:
                    continue

                # Get the roles mapped for this target client
                roles_url = f"{url}/{target_client_id}"
                roles_response = admin.connection.raw_get(roles_url)

                # Handle different response types
                if hasattr(roles_response, "json"):
                    if roles_response.status_code == 404:
                        continue
                    roles_response.raise_for_status()
                    roles_data = roles_response.json()
                elif isinstance(roles_response, bytes):
                    roles_data = json.loads(roles_response.decode("utf-8"))
                else:
                    roles_data = roles_response

                if roles_data and isinstance(roles_data, list):
                    role_names = []
                    for role in roles_data:
                        if isinstance(role, dict):
                            role_name = role.get("name")
                            if role_name:
                                role_names.append(role_name)
                    if role_names:
                        scope_mappings[target_client_name] = role_names

        return scope_mappings
    except Exception as e:
        print(f"  Warning: Could not get scope mappings for client: {e}")
        return {}


def get_client_default_scopes(admin: KeycloakAdmin, client_id: str) -> List[str]:
    """Get default client scopes assigned to a client."""
    try:
        scopes = admin.get_client_default_client_scopes(client_id)
        return [scope["name"] for scope in scopes]
    except Exception as e:
        print(f"  Warning: Could not get default scopes: {e}")
        return []


def get_client_optional_scopes(admin: KeycloakAdmin, client_id: str) -> List[str]:
    """Get optional client scopes assigned to a client."""
    try:
        scopes = admin.get_client_optional_client_scopes(client_id)
        return [scope["name"] for scope in scopes]
    except Exception as e:
        print(f"  Warning: Could not get optional scopes: {e}")
        return []


def get_user_realm_roles(admin: KeycloakAdmin, user_id: str) -> List[str]:
    """Get realm roles assigned to a user."""
    try:
        roles = admin.get_realm_roles_of_user(user_id)
        # Filter out default roles
        default_roles = {"default-roles-", "offline_access", "uma_authorization"}
        custom_roles = []
        for role in roles:
            role_name = role["name"]
            if not any(role_name.startswith(dr) or role_name == dr for dr in default_roles):
                custom_roles.append(role_name)
        return custom_roles
    except Exception as e:
        print(f"  Warning: Could not get user roles: {e}")
        return []


def infer_client_audience_targets(
    admin: KeycloakAdmin, realm: str, clients: List[Dict[str, Any]]
) -> Dict[str, List[str]]:
    """Infer client audience targets from optional client scopes."""
    client_audience_targets = {}

    # Build a mapping of role/scope names to client names
    scope_to_client = {}
    for client in clients:
        client_name = client["clientId"]
        try:
            roles = admin.get_client_roles(client["id"])
            for role in roles:
                role_name = role["name"]
                # Scope names match role names (no -audience suffix)
                scope_to_client[role_name] = client_name
        except Exception:
            pass

    for client in clients:
        client_id = client["id"]
        client_name = client["clientId"]

        # Get optional client scopes assigned to this client
        # These represent the target clients for audience
        try:
            optional_scopes = get_client_optional_scopes(admin, client_id)
            target_clients_set = set()

            # Each optional scope name corresponds to a role name
            # Find which client owns each role/scope
            for scope_name in optional_scopes:
                if scope_name in scope_to_client:
                    target_client = scope_to_client[scope_name]
                    # Don't include self-references
                    if target_client != client_name:
                        target_clients_set.add(target_client)

            # Get unique target clients (a client may have multiple roles/scopes)
            target_clients = []
            seen = set()
            for target in sorted(target_clients_set):
                if target not in seen:
                    target_clients.append(target)
                    seen.add(target)

            client_audience_targets[client_name] = target_clients
        except Exception as e:
            print(f"  Warning: Could not get optional scopes for {client_name}: {e}")
            client_audience_targets[client_name] = []

    return client_audience_targets


def export_config(realm_name: str, output_file: str):
    """Export Keycloak configuration to YAML format."""
    script_dir = Path(__file__).parent

    # Load environment variables from .env file
    load_dotenv(script_dir / ".env")

    KEYCLOAK_URL = os.getenv("KEYCLOAK_URL")
    KEYCLOAK_ADMIN_USERNAME = os.getenv("KEYCLOAK_ADMIN_USERNAME")
    KEYCLOAK_ADMIN_PASSWORD = os.getenv("KEYCLOAK_ADMIN_PASSWORD")

    if not all([KEYCLOAK_URL, KEYCLOAK_ADMIN_USERNAME, KEYCLOAK_ADMIN_PASSWORD]):
        raise ValueError(
            "Missing required environment variables. Please ensure .env file contains "
            "KEYCLOAK_URL, KEYCLOAK_ADMIN_USERNAME, and KEYCLOAK_ADMIN_PASSWORD"
        )

    print(f"\nConnecting to Keycloak at {KEYCLOAK_URL} ...")
    admin = KeycloakAdmin(
        server_url=KEYCLOAK_URL,
        username=KEYCLOAK_ADMIN_USERNAME,
        password=KEYCLOAK_ADMIN_PASSWORD,
        realm_name="master",
        user_realm_name="master",
    )

    # Switch to target realm
    print(f"Switching to realm: {realm_name}")
    admin = KeycloakAdmin(
        server_url=KEYCLOAK_URL,
        username=KEYCLOAK_ADMIN_USERNAME,
        password=KEYCLOAK_ADMIN_PASSWORD,
        realm_name=realm_name,
        user_realm_name="master",
    )

    config = {
        "# Keycloak RBAC Demo Configuration - Exported": None,
        "# NOTE": "Keycloak connection settings and realm name are in .env file",
        "# NOTE2": "Access control policy (user role to client role mappings) is in access_control_policy.yaml",
    }

    # Export clients
    print("\n=== Exporting clients ===")
    clients = admin.get_clients()

    # Filter out built-in clients
    builtin_clients = {
        "account",
        "account-console",
        "admin-cli",
        "broker",
        "realm-management",
        "security-admin-console",
    }

    custom_clients = [
        c for c in clients if c["clientId"] not in builtin_clients and not c["clientId"].startswith("system-")
    ]

    # Sort clients by clientId for consistent ordering
    custom_clients.sort(key=lambda c: c["clientId"])

    clients_config = []
    for client in custom_clients:
        client_id = client["clientId"]
        print(f"  Exporting client: {client_id}")

        # Get client roles and sort them alphabetically for consistent ordering
        roles = get_client_roles(admin, client["id"])
        if roles:
            roles.sort()

        client_config = {
            "client_id": client_id,
            "roles": roles if roles else ["access"],  # Default to 'access' if no roles
        }
        clients_config.append(client_config)

    config["clients"] = clients_config

    # Export realm roles
    print("\n=== Exporting realm roles ===")
    realm_roles = get_realm_roles(admin)
    if realm_roles:
        # Sort realm roles alphabetically for consistent ordering
        realm_roles.sort()
        print(f"  Found {len(realm_roles)} custom realm roles")
        config["realm_roles"] = realm_roles
    else:
        print("  No custom realm roles found")
        config["realm_roles"] = []

    # Infer client audience targets
    print("\n=== Inferring client audience targets ===")
    client_audience_targets = infer_client_audience_targets(admin, realm_name, custom_clients)
    config["client_audience_targets"] = client_audience_targets

    for client_name, targets in client_audience_targets.items():
        if targets:
            print(f"  {client_name} -> {', '.join(targets)}")
        else:
            print(f"  {client_name} -> (no targets)")

    # Export users
    print("\n=== Exporting users ===")
    users = admin.get_users({})

    users_config = []
    for user in users:
        username = user["username"]
        user_id = user["id"]

        # Skip admin user
        if username == "admin":
            continue

        print(f"  Exporting user: {username}")

        # Get user's realm roles
        user_roles = get_user_realm_roles(admin, user_id)

        user_config = {"username": username, "roles": user_roles}
        users_config.append(user_config)

    config["users"] = users_config

    # Write to YAML file
    print(f"\n=== Writing configuration to {output_file} ===")

    # Custom YAML representation to handle comments
    yaml_content = "# Keycloak RBAC Demo Configuration - Exported\n"
    yaml_content += "# NOTE: Keycloak connection settings and realm name are in .env file\n"
    yaml_content += (
        "# NOTE: Access control policy (user role to client role mappings) is in access_control_policy.yaml\n\n"
    )

    # Remove comment keys from config
    clean_config = {k: v for k, v in config.items() if not k.startswith("#")}

    # Add sections with comments
    yaml_content += "# Client configurations with roles\n"
    yaml_content += "# Each client can have multiple roles, each role gets its own audience scope\n"
    yaml_content += yaml.dump({"clients": clean_config["clients"]}, default_flow_style=False, sort_keys=False)
    yaml_content += "\n"

    yaml_content += "# Realm roles (can be composites of client roles)\n"
    yaml_content += yaml.dump({"realm_roles": clean_config["realm_roles"]}, default_flow_style=False, sort_keys=False)
    yaml_content += "\n"

    yaml_content += "# Client audience targets (which target clients each client needs audience scopes for)\n"
    yaml_content += "# For each target client, ALL role-specific scopes will be assigned\n"
    yaml_content += yaml.dump(
        {"client_audience_targets": clean_config["client_audience_targets"]}, default_flow_style=False, sort_keys=False
    )
    yaml_content += "\n"

    yaml_content += "# User configurations (only username and roles needed, rest uses defaults)\n"
    yaml_content += yaml.dump({"users": clean_config["users"]}, default_flow_style=False, sort_keys=False)

    with open(output_file, "w") as f:
        f.write(yaml_content)

    print(f"✓ Configuration exported successfully to {output_file}")
    print("\nExported:")
    print(f"  - {len(clients_config)} clients")
    print(f"  - {len(realm_roles)} realm roles")
    print(f"  - {len(users_config)} users")
    print("\nYou can now use this file with setup_demo.py")
