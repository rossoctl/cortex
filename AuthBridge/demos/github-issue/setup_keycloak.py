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

Security Note:
- This script uses default Keycloak admin credentials (username: "admin", password: "admin")
  for demo and local development only. These credentials are insecure and MUST NOT be used
  in any production or internet-exposed environment.
"""

import argparse
import sys
import os
from keycloak import KeycloakAdmin, KeycloakPostError

# Default configuration
KEYCLOAK_URL = os.environ.get("KEYCLOAK_URL", "http://keycloak.localtest.me:8080")
KEYCLOAK_REALM = os.environ.get("KEYCLOAK_REALM", "demo")
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


def get_spiffe_id(namespace: str, service_account: str) -> str:
    return f"spiffe://{SPIFFE_TRUST_DOMAIN}/ns/{namespace}/sa/{service_account}"


def get_or_create_realm(keycloak_admin, realm_name):
    try:
        realms = keycloak_admin.get_realms()
        for realm in realms:
            if realm["realm"] == realm_name:
                print(f"Realm '{realm_name}' already exists.")
                return
        keycloak_admin.create_realm(
            {"realm": realm_name, "enabled": True, "displayName": realm_name}
        )
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
        print(
            f"Note: Could not add mapper '{mapper_name}' (might already exist): {e}"
        )


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


def main():
    parser = argparse.ArgumentParser(
        description="Setup Keycloak for GitHub Issue Agent + AuthBridge demo"
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
    args = parser.parse_args()

    namespace = args.namespace
    service_account = args.service_account
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
        print(
            "  kubectl port-forward service/keycloak-service -n keycloak 8080:8080"
        )
        sys.exit(1)

    # Create realm
    print(f"\n--- Setting up realm: {KEYCLOAK_REALM} ---")
    get_or_create_realm(master_admin, KEYCLOAK_REALM)

    # Switch to demo realm
    keycloak_admin = KeycloakAdmin(
        server_url=KEYCLOAK_URL,
        username=KEYCLOAK_ADMIN_USERNAME,
        password=KEYCLOAK_ADMIN_PASSWORD,
        realm_name=KEYCLOAK_REALM,
        user_realm_name="master",
    )

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
    add_audience_mapper(
        keycloak_admin, agent_spiffe_scope_id, scope_name, agent_spiffe_id
    )

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
    add_audience_mapper(
        keycloak_admin, github_tool_scope_id, "github-tool-aud", "github-tool"
    )

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
    # Create demo users
    # ---------------------------------------------------------------
    print("\n--- Creating demo users ---")
    for user in DEMO_USERS:
        print(f"\n  {user['username']}: {user['description']}")
        get_or_create_user(keycloak_admin, user)

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
  Users:    alice (public access), bob (privileged access)

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
  1. Deploy webhook:     cd kagenti-webhook && AUTHBRIDGE_DEMO=true ./scripts/webhook-rollout.sh
  2. Apply ConfigMaps:   kubectl apply -f demos/github-issue/k8s/configmaps.yaml
  3. Create PAT secret:  kubectl create secret generic github-tool-secrets -n {namespace} \\
                           --from-literal=INIT_AUTH_HEADER="Bearer <PRIVILEGED_PAT>" \\
                           --from-literal=UPSTREAM_HEADER_TO_USE_IF_IN_AUDIENCE="Bearer <PRIVILEGED_PAT>" \\
                           --from-literal=UPSTREAM_HEADER_TO_USE_IF_NOT_IN_AUDIENCE="Bearer <PUBLIC_PAT>"
  4. Deploy tool:        kubectl apply -f demos/github-issue/k8s/github-tool-deployment.yaml
  5. Deploy agent:       kubectl apply -f demos/github-issue/k8s/git-issue-agent-deployment.yaml
"""
    )


if __name__ == "__main__":
    main()
