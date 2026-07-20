"""
setup_keycloak.py - Keycloak Setup for GitHub Issue Agent + AuthBridge Demo

This script supports two modes:

  Manual mode (no config file):
    Configures Keycloak for running the GitHub Issue Agent demo with
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

    Clients created:
    - github-tool: Target audience for token exchange (the MCP GitHub tool)

    Client Scopes created:
    - agent-<ns>-<sa>-aud: Adds Agent's SPIFFE ID to token audience (realm DEFAULT)
    - github-tool-aud: Adds "github-tool" to exchanged tokens (realm OPTIONAL)
    - github-full-access: Optional scope for privileged GitHub API access (realm OPTIONAL)

    Demo Users created:
    - alice: Regular user — tokens requested without github-full-access scope → public access
    - bob: Privileged user — tokens requested with scope=github-full-access → full access

    Note on scope model:
      github-full-access is a realm OPTIONAL scope. Optional scopes are NOT automatically
      included in tokens — the client must explicitly request them via the "scope" parameter
      in the token request. In a production system you would enforce per-user scope access
      via role-based policies. In this demo the calling client controls which scope to
      request for each user (see demo-manual.md Step 9).

    Usage:
      python setup_keycloak.py
      python setup_keycloak.py --namespace myns --service-account mysa

  RBAC mode (config file supplied via -rbac flag):
    Creates a realm with clients, roles, client scopes, audience mappers,
    and users to demonstrate role-based access control through OAuth2 token exchange.

    Before provisioning, existing artifacts declared in the config (clients,
    client roles, realm roles, and client scopes) are deleted so re-running
    the script starts from a clean slate. Operator-registered scopes (e.g.
    agent-*-aud created by the operator's ClientRegistrationReconciler)
    are intentionally left alone.

    Usage:
        python setup_keycloak.py -rbac <path-to>/config.yaml [-policy <path-to>/policy.yaml] [--reset-only]

    Arguments:
        -rbac <path-to>/config_file.yaml    Path to main configuration YAML file
        -policy <path-to>/policy.yaml       Path to access control policy YAML file (optional).
                                  Makes realm roles composites of the client roles
                                  declared in the policy, implementing RBAC through
                                  OAuth2 token scopes.

    Flags:
        --reset-only        Run the cleanup pass (initialize_realm_state) and exit
                            without provisioning. Useful for tearing the demo
                            state down between runs.

    Environment variables loaded from .env file

    Configuration files: (assumed to be under 'aiac' directory relative to script dir)
        .env           - Keycloak connection settings and realm name
        config.yaml    - Main configuration (clients, roles, users, scope_to_client)
        policy.yaml    - Access control policy (realm role -> client role mappings)

Security Note:
- This script uses default Keycloak admin credentials (username: "admin", password: "admin")
  for demo and local development only. These credentials are insecure and MUST NOT be used
  in any production or internet-exposed environment.
"""

import json
import os
import sys
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple

import yaml
from dotenv import load_dotenv
from keycloak import KeycloakAdmin, KeycloakGetError, KeycloakPostError

# ===========================================================================
# Manual setup — module-level configuration
# ===========================================================================

# Default configuration
KEYCLOAK_URL = os.environ.get("KEYCLOAK_URL", "http://keycloak.localtest.me:8080")
KEYCLOAK_REALM = os.environ.get("KEYCLOAK_REALM", "rossoctl")
KEYCLOAK_ADMIN_USERNAME = os.environ.get("KEYCLOAK_ADMIN_USERNAME", "admin")
KEYCLOAK_ADMIN_PASSWORD = os.environ.get("KEYCLOAK_ADMIN_PASSWORD", "admin")

if KEYCLOAK_ADMIN_USERNAME == "admin" and KEYCLOAK_ADMIN_PASSWORD == "admin":
    print(
        "WARNING: Using default Keycloak admin credentials 'admin'/'admin'. "
        "These credentials are INSECURE and must NOT be used in production.",
        file=sys.stderr,
    )

DEFAULT_NAMESPACE = "team1"
DEFAULT_SERVICE_ACCOUNT = "git-issue-agent"
SPIFFE_TRUST_DOMAIN = "localtest.me"
UI_CLIENT_ID = os.environ.get("UI_CLIENT_ID", "rossoctl")

DEMO_USERS = [
    {
        "username": "alice",
        "email": "alice@example.com",
        "firstName": "Alice",
        "lastName": "Demo",
        "password": "alice123",
        "description": "Regular user - request token without github-full-access scope",
    },
    {
        "username": "bob",
        "email": "bob@example.com",
        "firstName": "Bob",
        "lastName": "Admin",
        "password": "bob123",
        "description": "Privileged user - request token with scope=github-full-access",
    },
]


# ===========================================================================
# Manual helper functions
# ===========================================================================


def get_spiffe_id(namespace: str, service_account: str) -> str:
    return f"spiffe://{SPIFFE_TRUST_DOMAIN}/ns/{namespace}/sa/{service_account}"


def ensure_admin_in_realm(keycloak_admin, username, password):
    """Ensure the admin user exists in the target realm with a non-temporary password.

    Fixes 'invalid_grant: Invalid user credentials' when requesting tokens directly
    from the realm — the admin user lives in master but not in the target realm.
    """
    user_id = keycloak_admin.get_user_id(username)
    if not user_id:
        keycloak_admin.create_user({"username": username, "enabled": True, "emailVerified": True})
        user_id = keycloak_admin.get_user_id(username)
        print(f"Created user '{username}' in realm '{KEYCLOAK_REALM}'.")
    keycloak_admin.set_user_password(user_id, password, temporary=False)
    print(f"Password set as non-temporary for '{username}' in realm '{KEYCLOAK_REALM}'.")


def get_or_create_realm(keycloak_admin, realm_name):
    try:
        realms = keycloak_admin.get_realms()
        for realm in realms:
            if realm["realm"] == realm_name:
                print(f"Realm '{realm_name}' already exists.")
                return
        keycloak_admin.create_realm({"realm": realm_name, "enabled": True, "displayName": realm_name})
        print(f"Created realm '{realm_name}'.")
    except Exception as e:
        print(f"Error checking/creating realm: {e}", file=sys.stderr)
        raise


def get_or_create_client(keycloak_admin, client_payload):
    client_id = client_payload["clientId"]
    existing_client_id = keycloak_admin.get_client_id(client_id)
    if existing_client_id:
        print(f"Client '{client_id}' already exists.")
        return existing_client_id
    internal_id = keycloak_admin.create_client(client_payload)
    print(f"Created client '{client_id}'.")
    return internal_id


def get_or_create_client_scope(keycloak_admin, scope_payload):
    scope_name = scope_payload.get("name")
    scopes = keycloak_admin.get_client_scopes()
    for scope in scopes:
        if scope["name"] == scope_name:
            print(f"Client scope '{scope_name}' already exists with ID: {scope['id']}")
            return scope["id"]
    try:
        scope_id = keycloak_admin.create_client_scope(scope_payload)
        print(f"Created client scope '{scope_name}': {scope_id}")
        return scope_id
    except KeycloakPostError as e:
        print(f"Could not create client scope '{scope_name}': {e}")
        raise


def add_audience_mapper(keycloak_admin, scope_id, mapper_name, audience):
    mapper_payload = {
        "name": mapper_name,
        "protocol": "openid-connect",
        "protocolMapper": "oidc-audience-mapper",
        "consentRequired": False,
        "config": {
            "included.custom.audience": audience,
            "id.token.claim": "false",
            "access.token.claim": "true",
            "userinfo.token.claim": "false",
        },
    }
    try:
        keycloak_admin.add_mapper_to_client_scope(scope_id, mapper_payload)
        print(f"Added audience mapper '{mapper_name}' for audience '{audience}'")
    except Exception as e:
        print(f"Note: Could not add mapper '{mapper_name}' (might already exist): {e}")


def get_or_create_user(keycloak_admin, user_config):
    username = user_config["username"]
    users = keycloak_admin.get_users({"username": username})
    existing_user = next(
        (user for user in users if user.get("username") == username),
        None,
    )
    if existing_user:
        print(f"User '{username}' already exists.")
        return existing_user["id"]
    try:
        user_id = keycloak_admin.create_user(
            {
                "username": username,
                "email": user_config["email"],
                "firstName": user_config["firstName"],
                "lastName": user_config["lastName"],
                "enabled": True,
                "emailVerified": True,
                "credentials": [
                    {
                        "type": "password",
                        "value": user_config["password"],
                        "temporary": False,
                    }
                ],
            }
        )
        print(f"Created user '{username}' with ID: {user_id}")
        return user_id
    except KeycloakPostError as e:
        print(f"Could not create user '{username}': {e}")
        raise


def main_manual(namespace: str = DEFAULT_NAMESPACE, service_account: str = DEFAULT_SERVICE_ACCOUNT):
    agent_spiffe_id = get_spiffe_id(namespace, service_account)

    print("=" * 70)
    print("GitHub Issue Agent + AuthBridge - Keycloak Setup")
    print("=" * 70)
    print(f"\nNamespace:       {namespace}")
    print(f"Service Account: {service_account}")
    print(f"SPIFFE ID:       {agent_spiffe_id}")

    # Connect to Keycloak
    print(f"\nConnecting to Keycloak at {KEYCLOAK_URL}...")
    try:
        master_admin = KeycloakAdmin(
            server_url=KEYCLOAK_URL,
            username=KEYCLOAK_ADMIN_USERNAME,
            password=KEYCLOAK_ADMIN_PASSWORD,
            realm_name="master",
            user_realm_name="master",
        )
    except Exception as e:
        print(f"Failed to connect to Keycloak: {e}")
        print("\nMake sure Keycloak is running and accessible at:")
        print(f"  {KEYCLOAK_URL}")
        print("\nIf using port-forward, run:")
        print("  kubectl port-forward service/keycloak-service -n keycloak 8080:8080")
        sys.exit(1)

    # Ensure admin password is non-temporary
    admin_user_id = master_admin.get_user_id(KEYCLOAK_ADMIN_USERNAME)
    if admin_user_id:
        master_admin.set_user_password(admin_user_id, KEYCLOAK_ADMIN_PASSWORD, temporary=False)
        print(f"Admin password set as non-temporary for '{KEYCLOAK_ADMIN_USERNAME}'.")

    # Create realm
    print(f"\n--- Setting up realm: {KEYCLOAK_REALM} ---")
    get_or_create_realm(master_admin, KEYCLOAK_REALM)

    # Switch to target realm
    keycloak_admin = KeycloakAdmin(
        server_url=KEYCLOAK_URL,
        username=KEYCLOAK_ADMIN_USERNAME,
        password=KEYCLOAK_ADMIN_PASSWORD,
        realm_name=KEYCLOAK_REALM,
        user_realm_name="master",
    )

    # Ensure admin user exists in the rossoctl realm so direct token requests succeed
    ensure_admin_in_realm(keycloak_admin, KEYCLOAK_ADMIN_USERNAME, KEYCLOAK_ADMIN_PASSWORD)

    # ---------------------------------------------------------------
    # Create github-tool client (target audience for token exchange)
    # ---------------------------------------------------------------
    print("\n--- Creating github-tool client ---")
    print("This client represents the GitHub MCP tool as a token exchange target")
    get_or_create_client(
        keycloak_admin,
        {
            "clientId": "github-tool",
            "name": "GitHub Tool",
            "enabled": True,
            "publicClient": False,
            "standardFlowEnabled": False,
            "serviceAccountsEnabled": True,
            "attributes": {"standard.token.exchange.enabled": "true"},
        },
    )

    # ---------------------------------------------------------------
    # Create client scopes
    # ---------------------------------------------------------------
    print("\n--- Creating client scopes ---")

    # 1. agent-spiffe-aud scope: adds Agent's SPIFFE ID to all tokens (realm default)
    scope_name = f"agent-{namespace}-{service_account}-aud"
    print(f"\nCreating scope for Agent's SPIFFE ID audience: {scope_name}")
    agent_spiffe_scope_id = get_or_create_client_scope(
        keycloak_admin,
        {
            "name": scope_name,
            "protocol": "openid-connect",
            "attributes": {
                "include.in.token.scope": "true",
                "display.on.consent.screen": "true",
            },
        },
    )
    add_audience_mapper(keycloak_admin, agent_spiffe_scope_id, scope_name, agent_spiffe_id)

    # 2. github-tool-aud scope: adds "github-tool" to exchanged tokens (optional)
    print("\nCreating scope for github-tool audience...")
    github_tool_scope_id = get_or_create_client_scope(
        keycloak_admin,
        {
            "name": "github-tool-aud",
            "protocol": "openid-connect",
            "attributes": {
                "include.in.token.scope": "true",
                "display.on.consent.screen": "true",
            },
        },
    )
    add_audience_mapper(keycloak_admin, github_tool_scope_id, "github-tool-aud", "github-tool")

    # 3. github-full-access scope: optional scope for privileged access
    print("\nCreating scope for privileged GitHub access...")
    github_full_access_scope_id = get_or_create_client_scope(
        keycloak_admin,
        {
            "name": "github-full-access",
            "protocol": "openid-connect",
            "attributes": {
                "include.in.token.scope": "true",
                "display.on.consent.screen": "true",
            },
        },
    )

    # ---------------------------------------------------------------
    # Assign scopes at realm level
    # ---------------------------------------------------------------
    print("\n--- Assigning scopes ---")

    # agent-spiffe-aud as realm default (all tokens get Agent's SPIFFE ID in audience)
    try:
        keycloak_admin.add_default_default_client_scope(agent_spiffe_scope_id)
        print(f"Added '{scope_name}' as realm default scope.")
    except Exception as e:
        print(f"Note: Could not add '{scope_name}' as realm default: {e}")

    # github-tool-aud as realm optional (available for token exchange requests)
    try:
        keycloak_admin.add_default_optional_client_scope(github_tool_scope_id)
        print("Added 'github-tool-aud' as realm OPTIONAL scope.")
    except Exception as e:
        print(f"Note: Could not add 'github-tool-aud' as optional: {e}")

    # github-full-access as realm optional (must be requested explicitly in token request)
    try:
        keycloak_admin.add_default_optional_client_scope(github_full_access_scope_id)
        print("Added 'github-full-access' as realm OPTIONAL scope.")
        print("  → Tokens will only include this scope when explicitly requested")
        print("    via scope=github-full-access in the token request.")
    except Exception as e:
        print(f"Note: Could not add 'github-full-access' as optional: {e}")

    # ---------------------------------------------------------------
    # Add agent audience scope to the Rossoctl UI client
    # ---------------------------------------------------------------
    # Keycloak only auto-assigns realm default scopes to NEW clients.
    # The UI client was created during install (before this scope existed),
    # so we must add it explicitly. Without this, the UI's tokens won't
    # include the agent's SPIFFE ID in the audience, and AuthBridge will
    # reject UI chat requests with "invalid audience".
    #
    # TODO: Remove this workaround once the client-registration sidecar
    # handles this automatically (rossoctl/rossocortex#169).
    print(f"\n--- Adding agent audience scope to UI client '{UI_CLIENT_ID}' ---")
    ui_client_internal_id = keycloak_admin.get_client_id(UI_CLIENT_ID)
    if ui_client_internal_id:
        try:
            keycloak_admin.add_client_default_client_scope(ui_client_internal_id, agent_spiffe_scope_id, {})
            print(f"Added '{scope_name}' as default scope on client '{UI_CLIENT_ID}'.")
            print("  → UI tokens will now include the agent's SPIFFE ID in audience.")
            print("  → Users must log out and back in for the new scope to take effect.")
        except Exception as e:
            print(f"Note: Could not add scope to '{UI_CLIENT_ID}' client: {e}")
    else:
        print(
            f"Warning: UI client '{UI_CLIENT_ID}' not found in realm "
            f"'{KEYCLOAK_REALM}'. UI chat with this agent will require "
            f"manually adding the '{scope_name}' scope to the UI client."
        )

    # ---------------------------------------------------------------
    # Add token exchange scopes to the agent's client (if it exists)
    # ---------------------------------------------------------------
    # The agent's Keycloak client is created dynamically by the
    # client-registration sidecar when the agent pod starts. If the
    # client already exists (from a prior deployment), realm-level
    # optional scopes added after client creation won't be inherited.
    # Explicitly add the scopes so client_credentials grants with
    # scope=github-tool-aud+github-full-access succeed.
    print("\n--- Adding scopes to agent client (if registered) ---")
    agent_internal_id = keycloak_admin.get_client_id(agent_spiffe_id)
    if agent_internal_id:
        # Add the agent's own audience scope as a default so tokens issued
        # via client_credentials include the agent's SPIFFE ID in `aud`.
        try:
            keycloak_admin.add_client_default_client_scope(agent_internal_id, agent_spiffe_scope_id, {})
            print(f"Added '{scope_name}' as default scope on agent client.")
            print("  → client_credentials tokens will include the agent's SPIFFE ID in aud.")
        except Exception as e:
            print(f"Note: Could not add '{scope_name}' to agent client: {e}")

        # Add token exchange scopes as optional so client_credentials grants
        # with scope=github-tool-aud+github-full-access succeed.
        for exchange_scope_name, exchange_scope_id in [
            ("github-tool-aud", github_tool_scope_id),
            ("github-full-access", github_full_access_scope_id),
        ]:
            try:
                keycloak_admin.add_client_optional_client_scope(agent_internal_id, exchange_scope_id, {})
                print(f"Added '{exchange_scope_name}' as optional scope on agent client.")
            except Exception as e:
                print(f"Note: Could not add '{exchange_scope_name}' to agent client: {e}")
    else:
        print(
            f"Agent client '{agent_spiffe_id}' not yet registered.\n"
            f"  The scopes are realm-level defaults/optionals and will be inherited\n"
            f"  when client-registration creates the client. If the agent was deployed\n"
            f"  before this script, re-run it after the agent is running."
        )

    # ---------------------------------------------------------------
    # Create demo users and assign the admin realm role
    # ---------------------------------------------------------------
    print("\n--- Creating demo users ---")
    for user in DEMO_USERS:
        print(f"\n  {user['username']}: {user['description']}")
        get_or_create_user(keycloak_admin, user)

    # The Rossoctl backend uses the "admin" realm role for RBAC. Without
    # it, users can log in but see no agents or tools in the UI.
    print("\n--- Assigning 'admin' realm role to demo users ---")
    try:
        admin_role = keycloak_admin.get_realm_role("admin")
    except KeycloakGetError:
        admin_role = None
    if admin_role:
        for user in DEMO_USERS:
            user_id = keycloak_admin.get_user_id(user["username"])
            try:
                keycloak_admin.assign_realm_roles(user_id, [admin_role])
                print(f"Assigned 'admin' role to '{user['username']}'.")
            except Exception as e:
                print(f"Note: Could not assign 'admin' role to '{user['username']}' (might already have it): {e}")
    else:
        print(
            "Warning: 'admin' realm role not found. Demo users will not "
            "be able to see agents/tools in the UI. Ensure the Rossoctl "
            "platform is installed before running this script."
        )

    # ---------------------------------------------------------------
    # Summary
    # ---------------------------------------------------------------
    print("\n" + "=" * 70)
    print("SETUP COMPLETE")
    print("=" * 70)
    print(
        f"""
Keycloak is configured for the GitHub Issue Agent + AuthBridge demo.

Created:
  Realm:    {KEYCLOAK_REALM}
  Clients:  github-tool (target audience for token exchange)
  Scopes:   {scope_name} (realm DEFAULT - auto-adds Agent's SPIFFE ID to aud)
            github-tool-aud (realm OPTIONAL - for exchanged tokens)
            github-full-access (realm OPTIONAL - for privileged access)
  Users:    alice (public access), bob (privileged access) — both with admin role

Scope model:
  github-full-access is OPTIONAL — it must be explicitly requested in the
  token request (scope=github-full-access). To test:
    - alice: request token WITHOUT github-full-access → PUBLIC_ACCESS_PAT
    - bob:   request token WITH scope=github-full-access → PRIVILEGED_ACCESS_PAT

Token flow:
  1. UI gets token for user (aud includes Agent's SPIFFE ID via default scope)
  2. UI sends request to Agent with token
  3. AuthBridge validates inbound token (aud = Agent's SPIFFE ID)
  4. Agent calls GitHub tool
  5. AuthBridge exchanges token: aud={agent_spiffe_id} → aud=github-tool
  6. GitHub tool validates exchanged token and uses appropriate PAT

Next steps:
  1. Deploy operator:    See https://github.com/rossoctl/operator for webhook setup
  2. Apply ConfigMaps:   kubectl apply -f demos/github-issue/k8s/configmaps.yaml
  3. Create PAT secret:  kubectl create secret generic github-tool-secrets -n {namespace} \\
                           --from-literal=INIT_AUTH_HEADER="Bearer <PRIVILEGED_PAT>" \\
                           --from-literal=UPSTREAM_HEADER_TO_USE_IF_IN_AUDIENCE="Bearer <PRIVILEGED_PAT>" \\
                           --from-literal=UPSTREAM_HEADER_TO_USE_IF_NOT_IN_AUDIENCE="Bearer <PUBLIC_PAT>"
  4. Deploy tool:        kubectl apply -f demos/github-issue/k8s/github-tool-deployment.yaml
  5. Deploy agent:       kubectl apply -f demos/github-issue/k8s/git-issue-agent-deployment.yaml
"""
    )


# ===========================================================================
# Config-file-based setup
# ===========================================================================

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


def load_keycloak_config() -> Tuple[str, str]:
    """Read and validate non-sensitive Keycloak connection settings.

    Credentials are intentionally not returned to avoid tainting non-sensitive
    variables. They are consumed directly by connect_admin.
    """
    keycloak_url = os.getenv("KEYCLOAK_URL")
    realm = os.getenv("REALM_NAME")
    admin_username = os.getenv("KEYCLOAK_ADMIN_USERNAME")
    admin_password = os.getenv("KEYCLOAK_ADMIN_PASSWORD")

    if not all([keycloak_url, realm, admin_username, admin_password]):
        raise ValueError(
            "Missing required environment variables. Please ensure aiac.env file contains "
            "KEYCLOAK_URL, KEYCLOAK_ADMIN_USERNAME, KEYCLOAK_ADMIN_PASSWORD, and REALM_NAME"
        )

    assert isinstance(keycloak_url, str)
    assert isinstance(realm, str)
    return keycloak_url, realm


def connect_admin(server_url: str, realm_name: str) -> KeycloakAdmin:
    """Build a KeycloakAdmin client authenticating against the master realm."""
    return KeycloakAdmin(
        server_url=server_url,
        username=os.environ["KEYCLOAK_ADMIN_USERNAME"],
        password=os.environ["KEYCLOAK_ADMIN_PASSWORD"],
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
                print("    Secret: (configured)")
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

    admin.create_user(
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
    user_id = admin.get_user_id(username)
    if not user_id:
        raise RuntimeError(f"User '{username}' not found after creation")
    admin.set_user_password(user_id, f"{username}123", temporary=False)
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
# 13. Access control policy
# ---------------------------------------------------------------------------


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

    if policy is None:
        policy = {}

    for user_role, client_roles in policy.items():
        if not isinstance(client_roles, list):
            raise ValueError(f"Invalid policy for user role '{user_role}': must be a list of client role mappings")
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


def _add_client_role_to_realm_role_composite(
    admin: KeycloakAdmin, realm: str, realm_role_name: str, client_id: str, client_role_name: str
) -> None:
    """Add a client role to a realm role's composite roles."""
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
    """Load and apply access control policy to realm roles.

    Makes realm roles composites of client roles so users with a realm role
    automatically get all the client roles mapped to that realm role in the policy.

    Args:
        admin: Keycloak admin instance
        realm: Realm name
        access_control_policy_file: Path to policy YAML file
        client_ids: Mapping of client names to internal Keycloak IDs
        scope_ids: Unused; kept for API compatibility
    """
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
                _add_client_role_to_realm_role_composite(admin, realm, user_role, client_id, role_name)
                print(f"  ✓ Added client role '{client_name}.{role_name}' to realm role '{user_role}'")
            except KeycloakPostError as e:
                # HTTP 409 Conflict indicates the composite relationship already exists (idempotent)
                if e.response_code == 409:
                    print(
                        f"  ℹ Client role '{client_name}.{role_name}' already in composite for realm role '{user_role}'"
                    )
                else:
                    # Re-raise other Keycloak API errors (auth failures, malformed requests, etc.)
                    raise
            except KeycloakGetError as e:
                # Role lookup failures indicate missing roles - these should not be suppressed
                raise RuntimeError(
                    f"Failed to add client role '{client_name}.{role_name}' to realm role '{user_role}': "
                    f"Role not found (client_id={client_id})"
                ) from e


# ---------------------------------------------------------------------------
# Orchestrator
# ---------------------------------------------------------------------------


def main_rbac(config_file: str, policy_file: Optional[str] = None, reset_only: bool = False):
    """Main setup function."""
    script_dir = Path(__file__).parent

    # 1. Load configuration (env + YAML)
    load_dotenv(script_dir / "aiac" / "aiac.env")
    main_config_path = script_dir / config_file
    print(f"Loading main configuration from {main_config_path} ...")
    main_config = load_main_config(main_config_path)

    keycloak_url, realm = load_keycloak_config()

    print(f"\nConnecting to Keycloak at {keycloak_url} ...")
    admin = connect_admin(keycloak_url, realm_name="master")

    # Ensure admin password is non-temporary
    admin_username = os.getenv("KEYCLOAK_ADMIN_USERNAME", "admin")
    admin_password = os.getenv("KEYCLOAK_ADMIN_PASSWORD", "admin")
    admin_user_id = admin.get_user_id(admin_username)
    if admin_user_id:
        admin.set_user_password(admin_user_id, admin_password, temporary=False)
        print(f"Admin password set as non-temporary for '{admin_username}'.")

    # 2. Create realm
    create_realm(admin, realm)

    # Switch to the new realm for the remaining provisioning steps
    admin = connect_admin(keycloak_url, realm_name=realm)

    # Ensure admin user exists in the target realm so direct token requests succeed
    ensure_admin_in_realm(admin, admin_username, admin_password)

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

    # 8.5. Apply access control policy (if provided)
    if policy_file:
        policy_path = script_dir / policy_file
        print(f"\nApplying access control policy from {policy_path} ...")
        client_id_mapping = {name: info["id"] for name, info in client_ids.items()}
        apply_access_control_policy(admin, realm, policy_path, client_id_mapping, scope_ids)

    # 9. Target scopes as OPTIONAL
    assign_target_scopes_as_optional(admin, main_config, client_ids, scope_ids)

    # 10. Scope -> role gating
    map_scopes_to_roles(admin, realm, client_ids, scope_ids)

    # 11. Create users
    users_config = main_config.get("users", [])
    create_users(admin, users_config)

    # 12. Summary
    print_summary(keycloak_url, realm, main_config, users_config)


# ===========================================================================
# Entry point
# ===========================================================================

if __name__ == "__main__":
    import argparse

    parser = argparse.ArgumentParser(
        description="Setup Keycloak for GitHub Issue Agent + AuthBridge demo",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""modes:
  Manual mode (default, no -rbac):
    python setup_keycloak.py [--namespace NS] [--service-account SA]

  RBAC mode (-rbac):
    python setup_keycloak.py -rbac <path-to>/config.yaml [-policy <path-to>/policy.yaml] [--reset-only]
""",
    )
    parser.add_argument(
        "--namespace",
        "-n",
        default=DEFAULT_NAMESPACE,
        help=f"Kubernetes namespace (default: {DEFAULT_NAMESPACE})",
    )
    parser.add_argument(
        "--service-account",
        "-s",
        default=DEFAULT_SERVICE_ACCOUNT,
        help=f"Service account name (default: {DEFAULT_SERVICE_ACCOUNT})",
    )
    parser.add_argument(
        "-rbac",
        metavar="CONFIG_FILE",
        help="Path to main YAML config file — enables RBAC mode",
    )
    parser.add_argument(
        "-policy",
        metavar="POLICY_FILE",
        help="Path to access control policy YAML (RBAC mode, optional)",
    )
    parser.add_argument(
        "--reset-only",
        action="store_true",
        help="Run cleanup pass only, skip provisioning (RBAC mode)",
    )
    args = parser.parse_args()

    if args.rbac:
        try:
            main_rbac(args.rbac, policy_file=args.policy, reset_only=args.reset_only)
        except Exception as e:
            import traceback

            print(f"\nERROR: {e}", file=sys.stderr)
            traceback.print_exc()
            sys.exit(1)
    else:
        main_manual(namespace=args.namespace, service_account=args.service_account)
