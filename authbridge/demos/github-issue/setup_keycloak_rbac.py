"""
setup_keycloak.py - Keycloak Setup for GitHub Issue Agent + AuthBridge Demo

This script configures Keycloak for running the GitHub Issue Agent demo with
AuthBridge transparent token exchange.

Architecture:
  UI (user) → gets token (aud: Agent's SPIFFE ID) → sends to Agent
                                                        ↓
  Agent Pod (git-issue-agent + AuthBridge sidecars)
       |
       | Agent calls GitHub Tool with user's token
       v
  AuthProxy (Envoy) - intercepts, validates inbound, exchanges outbound
       |
       | Token Exchange → audience "github-tool"
       v
  GitHub Tool (validates token, uses appropriate GitHub PAT)

Clients created (from config.yaml):
- spiffe://localtest.me/ns/team1/sa/git-issue-agent: Agent client with github-agent role
- github-tool: Target audience for token exchange with github-tool-aud and github-full-access roles

Client Scopes created:
- Per-role scopes for each client (e.g., github-full-access, github-tool-aud)
- Scopes are assigned as DEFAULT (self) or OPTIONAL (targets)

Realm Roles created:
- regular: Standard user access level
- privileged: Elevated user access level

Demo Users created:
- alice: User with 'regular' realm role
- bob: User with 'privileged' realm role

Usage:
  python setup_keycloak.py <config_file.yaml> <access_control_policy.yaml>

Arguments:
  config.yaml    Path to main configuration YAML file
  policy.yaml    Path to access control policy YAML file

Environment variables (optional, defaults provided):
  KEYCLOAK_URL                  Default: http://keycloak.localtest.me:8080
  KEYCLOAK_ADMIN_USERNAME       Default: admin
  KEYCLOAK_ADMIN_PASSWORD       Default: admin
  REALM_NAME                    Default: kagenti

Security Note:
- This script uses default Keycloak admin credentials (username: "admin", password: "admin")
  for demo and local development only. These credentials are insecure and MUST NOT be used
  in any production or internet-exposed environment.
"""

import json
import os
import sys
from pathlib import Path
from typing import Any, Dict, List, Optional

import yaml
from keycloak import KeycloakAdmin, KeycloakPostError

# Default configuration - can be overridden by environment variables
KEYCLOAK_URL = os.environ.get("KEYCLOAK_URL", "http://keycloak.localtest.me:8080")
KEYCLOAK_REALM = os.environ.get("REALM_NAME", "kagenti")
KEYCLOAK_ADMIN_USERNAME = os.environ.get("KEYCLOAK_ADMIN_USERNAME", "admin")
KEYCLOAK_ADMIN_PASSWORD = os.environ.get("KEYCLOAK_ADMIN_PASSWORD", "admin")

if KEYCLOAK_ADMIN_USERNAME == "admin" and KEYCLOAK_ADMIN_PASSWORD == "admin":
    print(
        "WARNING: Using default Keycloak admin credentials 'admin'/'admin'. "
        "These credentials are INSECURE and must NOT be used in production.",
        file=sys.stderr,
    )


def load_main_config(config_file: Path) -> Dict[str, Any]:
    """Load main configuration from YAML file."""
    if not config_file.exists():
        raise FileNotFoundError(f"Configuration file not found: {config_file}")

    with open(config_file, "r") as f:
        return yaml.safe_load(f)


def load_access_control_policy(access_control_policy_file: Path) -> Dict[str, List[Dict[str, str]]]:
    """Load access control policy (user role -> client roles).

    Returns a dictionary where each user role (realm role) maps to a list of client role mappings.
    Each mapping contains 'client' (client name) and 'role' (role name).
    """
    if not access_control_policy_file.exists():
        raise FileNotFoundError(f"Access control policy file not found: {access_control_policy_file}")

    with open(access_control_policy_file, "r") as f:
        policy_config = yaml.safe_load(f)

    policy = policy_config.get("policy", {})

    # Handle empty policy (when policy: is present but has no value)
    if policy is None:
        policy = {}

    # Validate policy structure
    for user_role, client_roles in policy.items():
        if not isinstance(client_roles, list):
            raise ValueError(f"Invalid policy for user role '{user_role}': " + "must be a list of client role mappings")
        for client_role in client_roles:
            if not isinstance(client_role, dict):
                raise ValueError(
                    f"Invalid client role mapping for user role '{user_role}':"
                    + "must be a dict with 'client' and 'role' keys"
                )
            if "client" not in client_role or "role" not in client_role:
                raise ValueError(
                    f"Invalid client role mapping for user role '{user_role}':"
                    + "must contain 'client' and 'role' keys"
                )
            if not isinstance(client_role["client"], str) or not isinstance(client_role["role"], str):
                raise ValueError(
                    f"Invalid client role mapping for user role '{user_role}':" + "'client' and 'role' must be strings"
                )

    return policy


def add_client_role_to_realm_role_composite(
    admin: KeycloakAdmin, realm: str, realm_role_name: str, client_id: str, client_role_name: str
):
    """Add a client role to a realm role's composite roles."""
    # Get the client role
    client_role = admin.get_client_role(client_id, client_role_name)

    # Get the realm role
    realm_role = admin.get_realm_role(realm_role_name)

    # Add client role to realm role's composites
    url = f"{admin.connection.base_url}/admin/realms/{realm}/roles-by-id/{realm_role['id']}/composites"
    admin.connection.raw_post(url, data=json.dumps([client_role]))


def apply_access_control_policy(
    admin: KeycloakAdmin,
    realm: str,
    access_control_policy_file: Path,
    client_ids: Dict[str, str],
    scope_ids: Optional[Dict[str, str]] = None,
) -> None:
    """Load and apply access control policy to realm roles.

    Makes realm roles composites of client roles. This ensures users with a realm role
    automatically get all the client roles mapped to that realm role in the policy.
    This implements role-based access control by controlling which client roles users receive.

    Args:
        admin: Keycloak admin instance
        realm: Realm name
        access_control_policy_file: Path to policy YAML file
        client_ids: Mapping of client names to client IDs
        scope_ids: Optional mapping of scope names to scope IDs (unused, kept for compatibility)
    """
    user_role_to_client_roles = load_access_control_policy(access_control_policy_file)

    # Make realm roles composites of client roles
    # This ensures users with realm roles automatically get the mapped client roles
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


def create_client_role_safe(
    admin: KeycloakAdmin, client_id: str, role_name: str, client_name: str | None = None, description: str | None = None
) -> bool:
    """
    Create a client role with proper error handling.

    Args:
        admin: Keycloak admin instance
        client_id: The client ID where the role should be created
        role_name: Name of the role to create
        client_name: Optional display name for logging purposes
        description: Optional description for the role

    Returns:
        bool: True if role was created or already exists, False on error
    """
    display_name = client_name or client_id
    try:
        role_payload = {"name": role_name, "clientRole": True}
        if description:
            role_payload["description"] = description

        admin.create_client_role(client_id, role_payload, skip_exists=True)
        desc_info = f" ({description})" if description else ""
        print(f"  ✓ Created client role: {role_name} for {display_name}{desc_info}")
        return True
    except Exception as e:
        # Log the error but don't fail - role might already exist
        print(f"  ℹ Client role {role_name} for {display_name} already exists or error: {e}")
        return True  # Consider existing role as success


def assign_client_role_to_client_scope(admin: KeycloakAdmin, realm: str, scope_id: str, client_id: str, role_name: str):
    """Assign a client role to a client scope's scope-mappings for role-gating."""
    role = admin.get_client_role(client_id, role_name)
    url = (
        f"{admin.connection.base_url}/admin/realms/{realm}/client-scopes/{scope_id}/scope-mappings/clients/{client_id}"
    )
    admin.connection.raw_post(url, data=json.dumps([role]))


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


def create_single_client_scope(
    admin: KeycloakAdmin,
    realm: str,
    scope_name: str,
    target_client: str,
    target_client_id: str,
    role_name: str,
    default_attributes: Dict[str, str],
    default_mapper_config: Dict[str, str],
) -> str:
    """Create client scope with audience mapper for a specific role.

    The scope will be assigned as optional and conditionally included based on client role.
    """
    # Convert snake_case to dot.notation for Keycloak
    keycloak_attributes = {key.replace("_", "."): value for key, value in default_attributes.items()}

    scope_id = admin.create_client_scope(
        {
            "name": scope_name,
            "protocol": "openid-connect",
            "attributes": keycloak_attributes,
        },
        skip_exists=True,
    )

    # Disable Full Scope Allowed on the client scope to enable role-based filtering
    # This ensures the scope is only included if the user has the mapped role
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

    # Add audience mapper - will add the audience to tokens
    try:
        # Convert snake_case to dot.notation for Keycloak
        keycloak_mapper_config = {key.replace("_", "."): value for key, value in default_mapper_config.items()}
        keycloak_mapper_config["included.client.audience"] = target_client

        admin.add_mapper_to_client_scope(
            scope_id,
            {
                "name": f"{target_client}-audience",
                "protocol": "openid-connect",
                "protocolMapper": "oidc-audience-mapper",
                "consentRequired": False,
                "config": keycloak_mapper_config,
            },
        )
        print(f"    Added audience mapper -> {target_client}")
    except Exception as e:
        print(f"    Audience mapper already exists for {target_client}: {e}")

    return scope_id


def main(config_file: str, access_control_policy_file: str):
    """Main setup function."""
    script_dir = Path(__file__).parent

    # Load main configuration
    main_config_path = script_dir / config_file
    print(f"Loading main configuration from {main_config_path} ...")
    main_config = load_main_config(main_config_path)

    access_control_policy_path = script_dir / access_control_policy_file

    # Use global configuration variables (set from environment or defaults)
    REALM = KEYCLOAK_REALM

    print("=" * 70)
    print("GitHub Issue Agent + AuthBridge - Keycloak Setup")
    print("=" * 70)
    print(f"\nKeycloak URL: {KEYCLOAK_URL}")
    print(f"Realm:        {REALM}")

    print(f"\nConnecting to Keycloak at {KEYCLOAK_URL} ...")
    admin = KeycloakAdmin(
        server_url=KEYCLOAK_URL,
        username=KEYCLOAK_ADMIN_USERNAME,
        password=KEYCLOAK_ADMIN_PASSWORD,
        realm_name="master",
        user_realm_name="master",
    )

    # Create realm
    print(f"\n=== Creating realm: {REALM} ===")
    try:
        admin.create_realm(
            {
                "realm": REALM,
                "enabled": True,
                "accessTokenLifespan": 600,
                "verifyEmail": False,
                "registrationEmailAsUsername": False,
            }
        )
        print(f"  Created realm: {REALM}")
    except KeycloakPostError:
        print(f"  Realm {REALM} already exists, continuing...")

    # Switch to realm
    admin = KeycloakAdmin(
        server_url=KEYCLOAK_URL,
        username=KEYCLOAK_ADMIN_USERNAME,
        password=KEYCLOAK_ADMIN_PASSWORD,
        realm_name=REALM,
        user_realm_name="master",
    )

    # Create clients
    print("\n=== Creating clients ===")
    clients_config = main_config["clients"]

    client_ids = {}
    for client_config in clients_config:
        client_id = client_config["client_id"]

        # Get optional secret from config, or let Keycloak auto-generate
        client_secret = client_config.get("secret")

        # Get direct access grants setting from config, default to False
        direct_access_enabled = client_config.get("direct_access_grants", False)

        client_payload = {
            "clientId": client_id,
            "publicClient": False,
            "serviceAccountsEnabled": True,
            "directAccessGrantsEnabled": direct_access_enabled,
            "standardFlowEnabled": False,
            "fullScopeAllowed": False,
            "attributes": {"standard.token.exchange.enabled": "true"},
        }

        # Only set secret if provided in config
        if client_secret:
            client_payload["secret"] = client_secret

        internal_id = create_client_idempotent(admin, client_payload)
        if client_secret:
            print("    Secret: client_secret")
        else:
            print("    Secret: (auto-generated by Keycloak)")
        if direct_access_enabled:
            print("    Direct access grants: enabled")

        # Get roles from config - support both old format (list of strings) and new format (list of dicts)
        roles_config = client_config.get("roles", ["access"])
        client_roles = []
        for role in roles_config:
            if isinstance(role, dict):
                # New format with name and description
                client_roles.append({"name": role["name"], "description": role.get("description")})
            else:
                # Old format - just role name as string
                client_roles.append({"name": role, "description": None})

        client_ids[client_id] = {"id": internal_id, "secret": client_secret, "roles": client_roles}

    # Create client roles - create multiple roles per client based on config
    print("\n=== Creating client roles ===")
    for client_name, client_info in client_ids.items():
        client_id = client_info["id"]
        client_roles = client_info["roles"]
        for role in client_roles:
            role_name = role["name"]
            role_description = role.get("description")
            create_client_role_safe(admin, client_id, role_name, client_name, role_description)

    # NOTE: We do NOT add target client roles to source client scope mappings
    # This prevents target audiences from appearing in initial login tokens
    # Token exchange will still work because:
    # 1. Target client scopes are assigned as DEFAULT to source clients
    # 2. Scope-to-role mappings filter which scopes are included based on user's roles
    # 3. During token exchange, Keycloak uses the requested audience to determine which scopes to include
    print("\n=== Skipping target client role scope mappings (prevents initial token pollution) ===")
    print("  Target audiences will only appear during token exchange, not in initial login tokens")

    # Create realm roles
    print("\n=== Creating realm roles ===")
    roles = main_config.get("realm_roles", [])
    for role in roles:
        # Support both old format (string) and new format (dict with name and description)
        if isinstance(role, dict):
            role_name = role["name"]
            role_description = role.get("description")
        else:
            role_name = role
            role_description = None

        try:
            role_payload = {"name": role_name}
            if role_description:
                role_payload["description"] = role_description

            admin.create_realm_role(role_payload, skip_exists=True)
            desc_info = f" ({role_description})" if role_description else ""
            print(f"  Created role: {role_name}{desc_info}")
        except Exception:
            print(f"  Role {role_name} already exists")

    # Create client scopes with audience mappers - create one scope per role per client
    print("\n=== Creating client scopes ===")
    default_attributes = {
        "include_in_token_scope": "true",
        "display_on_consent_screen": "false",
        "consent_screen_text": "",
    }
    default_mapper_config = {
        "introspection_token_claim": "true",
        "userinfo_token_claim": "false",
        "id_token_claim": "false",
        "lightweight_claim": "false",
        "access_token_claim": "true",
        "lightweight_access_token_claim": "false",
    }

    scope_ids = {}
    for client_name, client_info in client_ids.items():
        client_internal_id = client_info["id"]
        client_roles = client_info["roles"]

        # Create one scope per role (scope name = role name)
        for role in client_roles:
            role_name = role["name"]
            scope_name = role_name  # No -audience suffix
            scope_id = create_single_client_scope(
                admin,
                REALM,
                scope_name,
                client_name,
                client_internal_id,
                role_name,
                default_attributes,
                default_mapper_config,
            )
            scope_ids[scope_name] = scope_id

    # Ensure login-issued tokens also include the authenticating client as audience.
    # This adds each client's own role scopes as defaults, so a token obtained for that
    # client (for example via password grant) contains aud=<requesting client> in addition
    # to any currently configured audience content. Scope-to-role filtering still applies.
    print("\n=== Assigning self audience scopes to clients as DEFAULT ===")
    for client_name, client_info in client_ids.items():
        assigned_self_scopes = []
        for role in client_info["roles"]:
            role_name = role["name"]
            scope_id = scope_ids.get(role_name)
            if not scope_id:
                continue
            try:
                admin.add_client_default_client_scope(client_info["id"], scope_id, {})
                assigned_self_scopes.append(role_name)
            except Exception:
                pass  # Already added

        if assigned_self_scopes:
            print(f"  {client_name} <- {', '.join(assigned_self_scopes)} (self default)")
        else:
            print(f"  {client_name} <- (no self scopes)")

    # Build client_ids mapping for apply_access_control_policy (client_name -> internal_id)
    client_id_mapping = {name: info["id"] for name, info in client_ids.items()}
    apply_access_control_policy(admin, REALM, access_control_policy_path, client_id_mapping, scope_ids)

    # Assign target scopes as OPTIONAL (not DEFAULT)
    # This prevents target audiences from appearing in initial login tokens
    # During token exchange, the requested audience will trigger inclusion of the appropriate scopes
    print("\n=== Assigning target client scopes to clients as OPTIONAL ===")
    client_audience_targets = main_config.get("client_audience_targets", {})

    for client_id, target_client_names in client_audience_targets.items():
        if client_id in client_ids and target_client_names:
            assigned_scopes = []
            for target_client_name in target_client_names:
                if target_client_name in client_ids:
                    target_roles = client_ids[target_client_name]["roles"]
                    for role in target_roles:
                        role_name = role["name"]
                        scope_name = role_name  # No -audience suffix
                        if scope_name in scope_ids:
                            scope_id = scope_ids[scope_name]
                            try:
                                admin.add_client_optional_client_scope(client_ids[client_id]["id"], scope_id, {})
                                assigned_scopes.append(scope_name)
                            except Exception:
                                pass  # Already added
            if assigned_scopes:
                print(f"  {client_id} <- {', '.join(assigned_scopes)} (optional)")
            else:
                print(f"  {client_id} <- (no scopes)")
        elif client_id in client_ids:
            print(f"  {client_id} <- (no target clients)")

    # Map each client scope to its corresponding client role
    # This filters DEFAULT scopes: only included if user has the specific client role
    print("\n=== Mapping client scopes to client roles for filtering ===")
    for client_name, client_info in client_ids.items():
        client_id = client_info["id"]
        client_roles = client_info["roles"]
        print(f"  {client_name}:")
        for role in client_roles:
            role_name = role["name"]
            scope_name = role_name  # No -audience suffix
            if scope_name in scope_ids:
                scope_id = scope_ids[scope_name]
                try:
                    # Add the client role to the scope's scope mappings
                    # This restricts the scope to users who have this client role
                    client_role = admin.get_client_role(client_id, role_name)
                    url = (
                        f"{admin.connection.base_url}/admin/realms/{REALM}"
                        f"/client-scopes/{scope_id}/scope-mappings/clients/{client_id}"
                    )
                    admin.connection.raw_post(url, data=json.dumps([client_role]))
                    print(f"    ✓ Restricted {scope_name} to role {client_name}.{role_name}")
                except Exception as e:
                    print(f"    ℹ Scope {scope_name} already restricted or error: {e}")

    # Create users
    print("\n=== Creating users ===")
    users_config = main_config["users"]

    for user_config in users_config:
        username = user_config["username"]
        user_roles = user_config.get("roles", [])
        password = f"{username}123"
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
                        "value": password,
                        "temporary": False,
                    }
                ],
            },
            exist_ok=True,
        )
        print(f"  Created user: {username} ")

        if user_roles:
            print(f"  Assigning roles to {username}: {', '.join(user_roles)}")
            role_representations = [admin.get_realm_role(r) for r in user_roles]
            try:
                admin.assign_realm_roles(user_id, role_representations)
                print(f"    Assigned roles: {', '.join(user_roles)}")
            except Exception as e:
                print(f"    Roles may already be assigned: {e}")
        else:
            print("    No roles assigned")

    # Summary
    print("\n" + "=" * 60)
    print("Demo realm setup complete!")
    print("=" * 60)
    print(f"\nKeycloak URL:  {KEYCLOAK_URL}")
    print(f"Realm:         {REALM}")
    print(f"Admin console: {KEYCLOAK_URL}/admin/master/console/#/{REALM}")
    print("\nUsers (password format: username123):")

    # Build a mapping of realm roles to their composite client roles for display
    composite_mappings = main_config.get("composite_role_mappings", {})
    for user_config in users_config:
        username = user_config["username"]
        user_roles = user_config.get("roles", [])
        if user_roles:
            # Show realm roles and their composite client roles
            role_details = []
            for realm_role in user_roles:
                if realm_role in composite_mappings:
                    client_roles = [
                        f"{s['client']}-{s['role'].replace(s['client'] + '-', '')}"
                        for s in composite_mappings[realm_role]
                    ]
                    role_details.append(f"{realm_role} ({', '.join(client_roles)})")
                else:
                    role_details.append(realm_role)
            print(f"  {username:8} ({', '.join(user_roles):15}) - roles: {', '.join(role_details)}")
        else:
            print(f"  {username:8} - roles: (none)")


if __name__ == "__main__":
    try:
        config_file = "config.yaml"
        policy_file = "policy.yaml"
        main(config_file, policy_file)
    except Exception as e:
        import traceback

        print(f"\nERROR: {e}", file=sys.stderr)
        traceback.print_exc()
        sys.exit(1)
