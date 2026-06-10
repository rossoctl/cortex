"""Setup script for Keycloak Token Exchange Demo.

Creates a realm with clients, roles, client scopes, audience mappers,
and users to demonstrate role-based access control through OAuth2 token exchange.

Before provisioning, existing artifacts declared in the config (clients,
client roles, realm roles, and client scopes) are deleted so re-running
the script starts from a clean slate. Operator-registered scopes (e.g.
agent-*-aud created by the kagenti-operator's ClientRegistrationReconciler)
are intentionally left alone.

Usage:
    python setup_keycloak.py <config_file.yaml> [--reset-only]

Arguments:
    config_file.yaml    Path to main configuration YAML file

Flags:
    --reset-only        Run the cleanup pass (initialize_realm_state) and exit
                        without provisioning. Useful for tearing the demo
                        state down between runs.

Environment variables (from .env file):
    KEYCLOAK_URL
    KEYCLOAK_ADMIN_USERNAME
    KEYCLOAK_ADMIN_PASSWORD
    REALM_NAME

Configuration files:
    .env           - Keycloak connection settings and realm name
    config.yaml    - Main configuration (clients, roles, users, scope_to_client)
"""

import json
import os
import sys
from pathlib import Path
from typing import Any, Dict, List, Tuple

import yaml
from dotenv import load_dotenv
from keycloak import KeycloakAdmin, KeycloakPostError

# ---------------------------------------------------------------------------
# 1. Configuration loading
# ---------------------------------------------------------------------------


def load_main_config(config_file: Path) -> Dict[str, Any]:
    """Load main configuration from YAML file."""
    if not config_file.exists():
        raise FileNotFoundError(f"Configuration file not found: {config_file}")

    with open(config_file, "r") as f:
        return yaml.safe_load(f)


def get_config_value(config: Dict[str, Any], *keys, default=None, env_var=None) -> Any:
    """Get configuration value with fallback to environment variable and default."""
    if env_var and os.environ.get(env_var):
        return os.environ.get(env_var)

    value = config
    for key in keys:
        if isinstance(value, dict) and key in value:
            value = value[key]
        else:
            return default

    return value if value != config else default


def load_keycloak_env() -> Tuple[str, str, str, str]:
    """Read and validate Keycloak environment variables."""
    keycloak_url = os.getenv("KEYCLOAK_URL")
    admin_username = os.getenv("KEYCLOAK_ADMIN_USERNAME")
    admin_password = os.getenv("KEYCLOAK_ADMIN_PASSWORD")
    realm = os.getenv("REALM_NAME")

    if not all([keycloak_url, admin_username, admin_password, realm]):
        raise ValueError(
            "Missing required environment variables. Please ensure .env file contains "
            "KEYCLOAK_URL, KEYCLOAK_ADMIN_USERNAME, KEYCLOAK_ADMIN_PASSWORD, and REALM_NAME"
        )

    assert isinstance(keycloak_url, str)
    assert isinstance(admin_username, str)
    assert isinstance(admin_password, str)
    assert isinstance(realm, str)
    return keycloak_url, admin_username, admin_password, realm


def connect_admin(server_url: str, username: str, password: str, realm_name: str) -> KeycloakAdmin:
    """Build a KeycloakAdmin client authenticating against the master realm."""
    return KeycloakAdmin(
        server_url=server_url,
        username=username,
        password=password,
        realm_name=realm_name,
        user_realm_name="master",
    )


# ---------------------------------------------------------------------------
# 2. Realm creation
# ---------------------------------------------------------------------------


def create_realm(admin: KeycloakAdmin, realm: str) -> None:
    """Create the demo realm if it does not already exist."""
    print(f"\n=== Creating realm: {realm} ===")
    try:
        admin.create_realm(
            {
                "realm": realm,
                "enabled": True,
                "accessTokenLifespan": 600,
                "verifyEmail": False,
                "registrationEmailAsUsername": False,
            }
        )
        print(f"  Created realm: {realm}")
    except KeycloakPostError:
        print(f"  Realm {realm} already exists, continuing...")


# ---------------------------------------------------------------------------
# 3. Reset realm state
# ---------------------------------------------------------------------------


def preserve_client_secrets(admin: KeycloakAdmin, clients_config: List[Dict[str, Any]]) -> Dict[str, str]:
    """Preserve secrets from existing clients that don't have explicit secrets in config.

    Returns:
        Dict mapping client_id to secret for clients that should preserve their secret
    """
    print("\nPreserving client secrets:")
    preserved_secrets: Dict[str, str] = {}

    for client_config in clients_config:
        client_id = client_config["client_id"]
        # Only preserve if no explicit secret in config
        if not client_config.get("secret"):
            existing_secret = get_existing_client_secret(admin, client_id)
            if existing_secret:
                preserved_secrets[client_id] = existing_secret
                print(f"  ✓ Preserved secret for: {client_id}")
            else:
                print(f"  - No existing secret for: {client_id}")

    return preserved_secrets


def initialize_realm_state(admin: KeycloakAdmin, main_config: Dict[str, Any]) -> Dict[str, str]:
    """Delete client roles, clients, realm roles, and client scopes defined in config so re-provisioning \
starts clean.

    Returns:
        Dict mapping client_id to preserved secret (for clients without explicit secret in config)
    """
    clients_config = main_config.get("clients", [])
    realm_roles_config = main_config.get("realm_roles", [])

    # Preserve secrets before deleting clients
    preserved_secrets = preserve_client_secrets(admin, clients_config)

    delete_client_roles(admin, clients_config)
    delete_clients(admin, clients_config)
    delete_realm_roles(admin, realm_roles_config)
    delete_client_scopes(admin, clients_config)

    return preserved_secrets


def delete_client_scopes(admin: KeycloakAdmin, clients_config: List[Dict[str, Any]]) -> None:
    """Delete client scopes whose names match roles declared in clients_config.

    Operator-registered scopes (e.g. agent-*-aud) are not in clients_config roles
    and are intentionally left alone.
    """
    print("\nDeleting client scopes:")
    if not clients_config:
        print("  No clients in configuration")
        return
    scope_names = {
        role["name"] if isinstance(role, dict) else role for c in clients_config for role in c.get("roles", ["access"])
    }
    try:
        existing = {s["name"]: s["id"] for s in admin.get_client_scopes()}
    except Exception as e:
        print(f"  ✗ Failed to list client scopes: {e}")
        return
    for name in scope_names:
        if name not in existing:
            print(f"  - Scope not found: {name}")
            continue
        try:
            admin.delete_client_scope(existing[name])
            print(f"  ✓ Deleted client scope: {name}")
        except Exception as e:
            print(f"  ✗ Failed to delete scope {name}: {e}")


def delete_client_roles(admin: KeycloakAdmin, clients_config: List[Dict[str, Any]]) -> None:
    print("\nDeleting client roles:")
    if not clients_config:
        print("  No clients in configuration")
        return
    for client_config in clients_config:
        client_id = client_config["client_id"]
        client_roles = client_config.get("roles", ["access"])
        try:
            internal_id = admin.get_client_id(client_id)
        except Exception as e:
            print(f"  - Skipping roles for {client_id}: {e}")
            continue
        if not internal_id:
            print(f"  - Client not found, skipping roles: {client_id}")
            continue
        for role in client_roles:
            role_name = role["name"] if isinstance(role, dict) else role
            try:
                admin.delete_client_role(internal_id, role_name)
                print(f"  ✓ Deleted client role: {client_id}.{role_name}")
            except Exception as e:
                print(f"  - Role not found or error: {client_id}.{role_name} ({e})")


def delete_clients(admin: KeycloakAdmin, clients_config: List[Dict[str, Any]]) -> None:
    print("\nDeleting clients:")
    if not clients_config:
        print("  No clients in configuration")
        return
    for client_config in clients_config:
        client_id = client_config["client_id"]
        try:
            internal_id = admin.get_client_id(client_id)
            if internal_id:
                admin.delete_client(internal_id)
                print(f"  ✓ Deleted client: {client_id}")
            else:
                print(f"  - Client not found: {client_id}")
        except Exception as e:
            print(f"  ✗ Failed to delete client {client_id}: {e}")


def delete_realm_roles(admin: KeycloakAdmin, realm_roles_config: List[str]) -> None:
    print("\nDeleting realm roles:")
    if not realm_roles_config:
        print("  No realm roles in configuration")
        return
    for role_name in realm_roles_config:
        try:
            admin.delete_realm_role(role_name)
            print(f"  ✓ Deleted realm role: {role_name}")
        except Exception as e:
            print(f"  - Realm role not found or error: {role_name} ({e})")


# ---------------------------------------------------------------------------
# 4. Create clients
# ---------------------------------------------------------------------------


def get_existing_client_secret(admin: KeycloakAdmin, client_id: str) -> str | None:
    """Retrieve the secret of an existing client, if it exists.

    Returns:
        The client secret if the client exists and has a secret, None otherwise.
    """
    try:
        internal_id = admin.get_client_id(client_id)
        if not internal_id:
            return None

        # Get the full client representation which includes the secret
        client_repr = admin.get_client(internal_id)
        return client_repr.get("secret")
    except Exception:
        # Client doesn't exist or error retrieving it
        return None


def create_clients(
    admin: KeycloakAdmin, clients_config: List[Dict[str, Any]], preserved_secrets: Dict[str, str]
) -> Dict[str, Dict[str, Any]]:
    """Create every client in config and return a {client_id: {id, secret, roles}} map.

    Args:
        admin: Keycloak admin client
        clients_config: List of client configurations
        preserved_secrets: Dict of client_id -> secret for clients to preserve
    """
    print("\n=== Creating clients ===")
    client_ids: Dict[str, Dict[str, Any]] = {}
    for client_config in clients_config:
        client_id = client_config["client_id"]
        client_secret = client_config.get("secret")
        direct_access_enabled = client_config.get("direct_access_grants", False)

        # If no secret is provided in config, use preserved secret if available
        if not client_secret and client_id in preserved_secrets:
            client_secret = preserved_secrets[client_id]

        payload = build_client_payload(client_id, client_secret, direct_access_enabled)
        internal_id = create_client_idempotent(admin, payload)

        if client_secret:
            if client_config.get("secret"):
                print(f"    Secret: {client_secret}")
            else:
                print("    Secret: (preserved from existing client)")
        else:
            print("    Secret: (auto-generated by Keycloak)")
        if direct_access_enabled:
            print("    Direct access grants: enabled")

        client_ids[client_id] = {
            "id": internal_id,
            "secret": client_secret,
            "roles": client_config.get("roles", ["access"]),
        }
    return client_ids


def build_client_payload(client_id: str, client_secret: str | None, direct_access_enabled: bool) -> Dict[str, Any]:
    """Build the Keycloak client representation payload."""
    payload: Dict[str, Any] = {
        "clientId": client_id,
        "publicClient": False,
        "serviceAccountsEnabled": True,
        "directAccessGrantsEnabled": direct_access_enabled,
        "standardFlowEnabled": False,
        "fullScopeAllowed": False,
        "attributes": {"standard.token.exchange.enabled": "true"},
    }
    if client_secret:
        payload["secret"] = client_secret
    return payload


def create_client_idempotent(admin: KeycloakAdmin, payload: dict) -> str:
    """Create a client or return existing internal ID."""
    client_id = payload["clientId"]
    try:
        internal_id = admin.create_client(payload)
        print(f"  Created client: {client_id}")
        return internal_id
    except KeycloakPostError:
        internal_id = admin.get_client_id(client_id)
        if internal_id is None:
            raise ValueError(f"Client '{client_id}' not found and could not be created")
        print(f"  Using existing client: {client_id}")
        return internal_id


# ---------------------------------------------------------------------------
# 5. Create client roles
# ---------------------------------------------------------------------------


def create_client_roles(admin: KeycloakAdmin, client_ids: Dict[str, Dict[str, Any]]) -> None:
    print("\n=== Creating client roles ===")
    for client_name, client_info in client_ids.items():
        for role in client_info["roles"]:
            role_name = role["name"] if isinstance(role, dict) else role
            create_client_role_safe(admin, client_info["id"], role_name, client_name)

    # NOTE: We do NOT add target client roles to source client scope mappings.
    # This prevents target audiences from appearing in initial login tokens.
    # Token exchange will still work because:
    # 1. Target client scopes are assigned as DEFAULT to source clients
    # 2. Scope-to-role mappings filter which scopes are included based on user's roles
    # 3. During token exchange, Keycloak uses the requested audience to determine which scopes to include
    print("\n=== Skipping target client role scope mappings (prevents initial token pollution) ===")
    print("  Target audiences will only appear during token exchange, not in initial login tokens")


def create_client_role_safe(
    admin: KeycloakAdmin,
    client_id: str,
    role_name: str,
    client_name: str | None = None,
) -> bool:
    """Create a client role with proper error handling."""
    display_name = client_name or client_id
    try:
        admin.create_client_role(
            client_id,
            {"name": role_name, "clientRole": True},
            skip_exists=True,
        )
        print(f"  ✓ Created client role: {role_name} for {display_name}")
        return True
    except Exception as e:
        print(f"  ℹ Client role {role_name} for {display_name} already exists or error: {e}")
        return True


# ---------------------------------------------------------------------------
# 6. Create realm roles
# ---------------------------------------------------------------------------


def create_realm_roles(admin: KeycloakAdmin, realm_roles: List[str]) -> None:
    print("\n=== Creating realm roles ===")
    for role in realm_roles:
        # Extract role name and description - support both dict and string formats
        if isinstance(role, dict):
            role_payload = {"name": role["name"]}
            if "description" in role:
                role_payload["description"] = role["description"]
            role_name = role["name"]
        else:
            role_payload = {"name": role}
            role_name = role

        try:
            admin.create_realm_role(role_payload, skip_exists=True)
            print(f"  Created role: {role_name}")
        except Exception:
            print(f"  Role {role_name} already exists")


# ---------------------------------------------------------------------------
# 7. Create client scopes (one per role per client)
# ---------------------------------------------------------------------------

DEFAULT_SCOPE_ATTRIBUTES = {
    "include_in_token_scope": "true",
    "display_on_consent_screen": "false",
    "consent_screen_text": "",
}

DEFAULT_MAPPER_CONFIG = {
    "introspection_token_claim": "true",
    "userinfo_token_claim": "false",
    "id_token_claim": "false",
    "lightweight_claim": "false",
    "access_token_claim": "true",
    "lightweight_access_token_claim": "false",
}


def create_client_scopes(
    admin: KeycloakAdmin,
    client_ids: Dict[str, Dict[str, Any]],
) -> Dict[str, str]:
    """Create one scope per role per client. Returns {scope_name: scope_id}."""
    print("\n=== Creating client scopes ===")
    scope_ids: Dict[str, str] = {}
    for client_name, client_info in client_ids.items():
        for role in client_info["roles"]:
            # Extract role name - support both dict and string formats
            scope_name = role["name"] if isinstance(role, dict) else role
            scope_id = create_single_client_scope(
                admin,
                scope_name,
                client_name,
                DEFAULT_SCOPE_ATTRIBUTES,
                DEFAULT_MAPPER_CONFIG,
            )
            scope_ids[scope_name] = scope_id
    return scope_ids


def create_single_client_scope(
    admin: KeycloakAdmin,
    scope_name: str,
    target_client: str,
    default_attributes: Dict[str, str],
    default_mapper_config: Dict[str, str],
) -> str:
    """Create client scope with audience mapper for a specific role.

    The scope will be assigned as optional and conditionally included based on client role.
    """
    keycloak_attributes = {key.replace("_", "."): str(value) for key, value in default_attributes.items()}

    scope_payload = {
        "name": scope_name,
        "protocol": "openid-connect",
        "attributes": keycloak_attributes,
    }

    scope_id = admin.create_client_scope(
        scope_payload,
        skip_exists=True,
    )

    _disable_full_scope_allowed(admin, scope_id, scope_name)
    _add_audience_mapper(admin, scope_id, target_client, default_mapper_config)
    return scope_id


def _disable_full_scope_allowed(admin: KeycloakAdmin, scope_id: str, scope_name: str) -> None:
    # Disable Full Scope Allowed so the scope is only included if the user has the mapped role.
    try:
        scope_representation = admin.get_client_scope(scope_id)
        if scope_representation.get("fullScopeAllowed", True):
            scope_representation["fullScopeAllowed"] = False
            admin.update_client_scope(scope_id, scope_representation)
            print(f"  Created client scope: {scope_name} (Full Scope Allowed: OFF)")
        else:
            print(f"  Created client scope: {scope_name}")
    except Exception as e:
        print(f"  Created client scope: {scope_name} (could not disable Full Scope Allowed: {e})")


def _add_audience_mapper(
    admin: KeycloakAdmin,
    scope_id: str,
    target_client: str,
    default_mapper_config: Dict[str, str],
) -> None:
    try:
        mapper_config = {key.replace("_", "."): value for key, value in default_mapper_config.items()}
        mapper_config["included.client.audience"] = target_client

        admin.add_mapper_to_client_scope(
            scope_id,
            {
                "name": f"{target_client}-audience",
                "protocol": "openid-connect",
                "protocolMapper": "oidc-audience-mapper",
                "consentRequired": False,
                "config": mapper_config,
            },
        )
        print(f"    Added audience mapper -> {target_client}")
    except Exception as e:
        print(f"    Audience mapper already exists for {target_client}: {e}")


# ---------------------------------------------------------------------------
# 8. Self scopes as DEFAULT
# ---------------------------------------------------------------------------


def assign_self_scopes_as_default(
    admin: KeycloakAdmin,
    client_ids: Dict[str, Dict[str, Any]],
    scope_ids: Dict[str, str],
) -> None:
    """Assign each client's own role-scopes as default so login-issued tokens carry aud=<client>."""
    print("\n=== Assigning self audience scopes to clients as DEFAULT ===")
    for client_name, client_info in client_ids.items():
        assigned: List[str] = []
        for role in client_info["roles"]:
            # Extract role name - support both dict and string formats
            role_name = role["name"] if isinstance(role, dict) else role
            scope_id = scope_ids.get(role_name)
            if not scope_id:
                continue
            try:
                admin.add_client_default_client_scope(client_info["id"], scope_id, {})
                assigned.append(role_name)
            except Exception:
                pass  # Already added

        if assigned:
            print(f"  {client_name} <- {', '.join(assigned)} (self default)")
        else:
            print(f"  {client_name} <- (no self scopes)")


# ---------------------------------------------------------------------------
# 9. Target scopes as OPTIONAL
# ---------------------------------------------------------------------------


def assign_target_scopes_as_optional(
    admin: KeycloakAdmin,
    main_config: Dict[str, Any],
    client_ids: Dict[str, Dict[str, Any]],
    scope_ids: Dict[str, str],
) -> None:
    """Assign target client scopes as OPTIONAL (driven by client_audience_targets).

    Optional (not default) so target audiences only appear during token exchange,
    never in initial login tokens.
    """
    print("\n=== Assigning target client scopes to clients as OPTIONAL ===")
    client_audience_targets = main_config.get("client_audience_targets", {})

    for client_id, target_client_names in client_audience_targets.items():
        if client_id not in client_ids:
            continue
        if not target_client_names:
            print(f"  {client_id} <- (no target clients)")
            continue

        assigned = _assign_target_scopes_for_client(
            admin,
            client_ids,
            scope_ids,
            client_id,
            target_client_names,
        )
        if assigned:
            print(f"  {client_id} <- {', '.join(assigned)} (optional)")
        else:
            print(f"  {client_id} <- (no scopes)")


def _assign_target_scopes_for_client(
    admin: KeycloakAdmin,
    client_ids: Dict[str, Dict[str, Any]],
    scope_ids: Dict[str, str],
    client_id: str,
    target_client_names: List[str],
) -> List[str]:
    assigned: List[str] = []
    for target_client_name in target_client_names:
        if target_client_name not in client_ids:
            continue
        for role in client_ids[target_client_name]["roles"]:
            # Extract role name - support both dict and string formats
            role_name = role["name"] if isinstance(role, dict) else role
            scope_id = scope_ids.get(role_name)
            if not scope_id:
                continue
            try:
                admin.add_client_optional_client_scope(client_ids[client_id]["id"], scope_id, {})
                assigned.append(role_name)
            except Exception:
                pass  # Already added
    return assigned


# ---------------------------------------------------------------------------
# 10. Scope -> role gating
# ---------------------------------------------------------------------------


def map_scopes_to_roles(
    admin: KeycloakAdmin,
    realm: str,
    client_ids: Dict[str, Dict[str, Any]],
    scope_ids: Dict[str, str],
) -> None:
    """Restrict each scope to users holding the matching client role."""
    print("\n=== Mapping client scopes to client roles for filtering ===")
    for client_name, client_info in client_ids.items():
        client_id = client_info["id"]
        print(f"  {client_name}:")
        for role in client_info["roles"]:
            # Extract role name - support both dict and string formats
            role_name = role["name"] if isinstance(role, dict) else role
            scope_id = scope_ids.get(role_name)
            if not scope_id:
                continue
            try:
                assign_client_role_to_client_scope(admin, realm, scope_id, client_id, role_name)
                print(f"    ✓ Restricted {role_name} to role {client_name}.{role_name}")
            except Exception as e:
                print(f"    ℹ Scope {role_name} already restricted or error: {e}")


def assign_client_role_to_client_scope(
    admin: KeycloakAdmin, realm: str, scope_id: str, client_id: str, role_name: str
) -> None:
    """Assign a client role to a client scope's scope-mappings for role-gating."""
    role = admin.get_client_role(client_id, role_name)
    url = (
        f"{admin.connection.base_url}/admin/realms/{realm}/client-scopes/{scope_id}/scope-mappings/clients/{client_id}"
    )
    admin.connection.raw_post(url, data=json.dumps([role]))


# ---------------------------------------------------------------------------
# 11. Create users
# ---------------------------------------------------------------------------


def create_users(admin: KeycloakAdmin, users_config: List[Dict[str, Any]]) -> None:
    print("\n=== Creating users ===")
    for user_config in users_config:
        create_single_user(admin, user_config)


def create_single_user(admin: KeycloakAdmin, user_config: Dict[str, Any]) -> None:
    username = user_config["username"]
    user_roles = user_config.get("roles", [])

    user_id = admin.create_user(
        {
            "username": username,
            "email": f"{username}@example.com",
            "firstName": username.capitalize(),
            "lastName": "Demo",
            "enabled": True,
            "emailVerified": True,
            "credentials": [
                {
                    "type": "password",
                    "value": f"{username}123",
                    "temporary": False,
                }
            ],
        },
        exist_ok=True,
    )
    print(f"  Created user: {username}")

    if not user_roles:
        print("    No roles assigned")
        return

    role_representations = [admin.get_realm_role(r) for r in user_roles]
    try:
        admin.assign_realm_roles(user_id, role_representations)
        print(f"    Assigned roles: {', '.join(user_roles)}")
    except Exception as e:
        print(f"    Roles may already be assigned: {e}")


# ---------------------------------------------------------------------------
# 12. Summary
# ---------------------------------------------------------------------------


def print_summary(
    keycloak_url: str,
    realm: str,
    main_config: Dict[str, Any],
    users_config: List[Dict[str, Any]],
) -> None:
    print("\n" + "=" * 60)
    print("Demo realm setup complete!")
    print("=" * 60)
    print(f"\nKeycloak URL:  {keycloak_url}")
    print(f"Realm:         {realm}")
    print(f"Admin console: {keycloak_url}/admin/master/console/#/{realm}")
    print("\nUsers (password=<username>123 for each user):")

    composite_mappings = main_config.get("composite_role_mappings", {})
    for user_config in users_config:
        print(_format_user_summary(user_config, composite_mappings))


def _format_user_summary(
    user_config: Dict[str, Any],
    composite_mappings: Dict[str, Any],
) -> str:
    username = user_config["username"]
    user_roles = user_config.get("roles", [])
    if not user_roles:
        return f"  {username:8} - roles: (none)"

    role_details: List[str] = []
    for realm_role in user_roles:
        if realm_role in composite_mappings:
            client_roles = [
                f"{s['client']}-{s['role'].replace(s['client'] + '-', '')}" for s in composite_mappings[realm_role]
            ]
            role_details.append(f"{realm_role} ({', '.join(client_roles)})")
        else:
            role_details.append(realm_role)
    return f"  {username:8} ({', '.join(user_roles):15}) - roles: {', '.join(role_details)}"


# ---------------------------------------------------------------------------
# Orchestrator
# ---------------------------------------------------------------------------


def main(config_file: str, reset_only: bool = False):
    """Main setup function."""
    script_dir = Path(__file__).parent

    # 1. Load configuration (env + YAML)
    load_dotenv(script_dir / "aiac.env")
    main_config_path = script_dir / config_file
    print(f"Loading main configuration from {main_config_path} ...")
    main_config = load_main_config(main_config_path)

    keycloak_url, admin_username, admin_password, realm = load_keycloak_env()

    print(f"\nConnecting to Keycloak at {keycloak_url} ...")
    admin = connect_admin(keycloak_url, admin_username, admin_password, realm_name="master")

    # 2. Create realm
    create_realm(admin, realm)

    # Switch to the new realm for the remaining provisioning steps
    admin = connect_admin(keycloak_url, admin_username, admin_password, realm_name=realm)

    # 3. Reset prior provisioning artifacts and preserve secrets
    print(f"\n=== Initializing realm state for re-provisioning: {realm} ===")
    preserved_secrets = initialize_realm_state(admin, main_config)

    if reset_only:
        print("\nReset-only mode: skipping provisioning. Done.")
        return

    # 4. Create clients with preserved secrets
    client_ids = create_clients(admin, main_config["clients"], preserved_secrets)

    # 5. Create client roles
    create_client_roles(admin, client_ids)

    # 6. Create realm roles
    create_realm_roles(admin, main_config.get("realm_roles", []))

    # 7. Create client scopes (one per role per client) with audience mappers
    scope_ids = create_client_scopes(admin, client_ids)

    # 8. Self scopes as DEFAULT
    assign_self_scopes_as_default(admin, client_ids, scope_ids)

    # 9. Target scopes as OPTIONAL
    assign_target_scopes_as_optional(admin, main_config, client_ids, scope_ids)

    # 10. Scope -> role gating
    map_scopes_to_roles(admin, realm, client_ids, scope_ids)

    # 11. Create users
    users_config = main_config["users"]
    create_users(admin, users_config)

    # 12. Summary
    print_summary(keycloak_url, realm, main_config, users_config)


if __name__ == "__main__":
    try:
        args = [a for a in sys.argv[1:] if not a.startswith("--")]
        flags = {a for a in sys.argv[1:] if a.startswith("--")}
        if len(args) != 1:
            print("Usage: python setup_keycloak.py <config.yaml> [--reset-only]", file=sys.stderr)
            sys.exit(1)

        config_file = args[0]
        reset_only = "--reset-only" in flags
        main(config_file, reset_only=reset_only)
    except Exception as e:
        import traceback

        print(f"\nERROR: {e}", file=sys.stderr)
        traceback.print_exc()
        sys.exit(1)
