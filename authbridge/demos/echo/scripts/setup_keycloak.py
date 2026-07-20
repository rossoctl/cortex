"""
setup_keycloak.py - Keycloak Setup for the Echo (credential placeholder-swap) Demo

This script configures Keycloak for running the echo demo with AuthBridge's
credential placeholder swap + transparent token exchange.

Architecture:
  UI (user) → gets token (aud: Agent's SPIFFE ID) → sends to Agent
                                                        ↓
  Agent Pod (echo-agent + AuthBridge sidecar)
       |  inbound jwt-validation runs in placeholder_mode: it validates
       |  the user's token, stashes the real token in the shared store,
       |  and forwards an opaque `abph_...` placeholder to the agent.
       |
       | Agent echoes the placeholder back via the upstream call.
       v
  AuthProxy (forward proxy) - intercepts outbound, resolves the
       placeholder back to the real token, then exchanges it.
       |
       | Token Exchange → audience "echo-upstream"
       v
  echo-upstream (plain HTTP) - returns whatever Authorization header it
       received (the exchanged token), proving the swap end to end.

Clients created:
- echo-upstream: Target audience for token exchange (the echo upstream)

Client Scopes created:
- agent-<ns>-<sa>-aud: Adds Agent's SPIFFE ID to token audience (realm DEFAULT)
- echo-upstream-aud: Adds "echo-upstream" to exchanged tokens (realm OPTIONAL)

Demo Users created:
- alice: alice123
- bob:   bob123
  (both get the realm "admin" role so the rossoctl UI lists agents/tools)

Usage:
  python setup_keycloak.py
  python setup_keycloak.py --namespace myns --service-account mysa

Security Note:
- This script uses default Keycloak admin credentials (username: "admin", password: "admin")
  for demo and local development only. These credentials are insecure and MUST NOT be used
  in any production or internet-exposed environment.
"""

import argparse
import os
import sys

from keycloak import KeycloakAdmin, KeycloakGetError, KeycloakPostError

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
DEFAULT_SERVICE_ACCOUNT = "echo-agent"
SPIFFE_TRUST_DOMAIN = "localtest.me"
UI_CLIENT_ID = os.environ.get("UI_CLIENT_ID", "rossoctl")

# The token-exchange target audience (the echo upstream). Mirrors the
# `target_audience` / `token_scopes` in k8s/echo-patch.yaml's route.
ECHO_UPSTREAM_CLIENT_ID = "echo-upstream"
ECHO_UPSTREAM_SCOPE = "echo-upstream-aud"

DEMO_USERS = [
    {
        "username": "alice",
        "email": "alice@example.com",
        "firstName": "Alice",
        "lastName": "Demo",
        "password": "alice123",
        "description": "Regular demo user",
    },
    {
        "username": "bob",
        "email": "bob@example.com",
        "firstName": "Bob",
        "lastName": "Demo",
        "password": "bob123",
        "description": "Second demo user",
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


def main():
    parser = argparse.ArgumentParser(description="Setup Keycloak for the Echo (credential placeholder-swap) demo")
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
    print("Echo (credential placeholder-swap) + AuthBridge - Keycloak Setup")
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

    # ---------------------------------------------------------------
    # Create echo-upstream client (target audience for token exchange)
    # ---------------------------------------------------------------
    print("\n--- Creating echo-upstream client ---")
    print("This client represents the echo upstream as a token exchange target")
    get_or_create_client(
        keycloak_admin,
        {
            "clientId": ECHO_UPSTREAM_CLIENT_ID,
            "name": "Echo Upstream",
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

    # 1. agent-spiffe-aud scope: adds Agent's SPIFFE ID to all tokens (realm default).
    #    Inbound jwt-validation checks the audience against the agent's SPIFFE ID,
    #    so the UI's tokens must carry it or chat requests are rejected.
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

    # 2. echo-upstream-aud scope: adds "echo-upstream" to exchanged tokens (optional).
    #    This is the target_audience referenced by the echo-patch.yaml route's
    #    token_scopes ("openid echo-upstream-aud").
    print("\nCreating scope for echo-upstream audience...")
    echo_upstream_scope_id = get_or_create_client_scope(
        keycloak_admin,
        {
            "name": ECHO_UPSTREAM_SCOPE,
            "protocol": "openid-connect",
            "attributes": {
                "include.in.token.scope": "true",
                "display.on.consent.screen": "true",
            },
        },
    )
    add_audience_mapper(keycloak_admin, echo_upstream_scope_id, ECHO_UPSTREAM_SCOPE, ECHO_UPSTREAM_CLIENT_ID)

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

    # echo-upstream-aud as realm optional (requested during token exchange)
    try:
        keycloak_admin.add_default_optional_client_scope(echo_upstream_scope_id)
        print(f"Added '{ECHO_UPSTREAM_SCOPE}' as realm OPTIONAL scope.")
    except Exception as e:
        print(f"Note: Could not add '{ECHO_UPSTREAM_SCOPE}' as optional: {e}")

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
    # handles this automatically (rossoctl/cortex#169).
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
    # Explicitly add the scopes so the outbound exchange to
    # audience=echo-upstream (scope=openid echo-upstream-aud) succeeds.
    print("\n--- Adding scopes to agent client (if registered) ---")
    agent_internal_id = keycloak_admin.get_client_id(agent_spiffe_id)
    if agent_internal_id:
        # Add the agent's own audience scope as a default so tokens issued
        # via the exchange include the agent's SPIFFE ID in `aud`.
        try:
            keycloak_admin.add_client_default_client_scope(agent_internal_id, agent_spiffe_scope_id, {})
            print(f"Added '{scope_name}' as default scope on agent client.")
        except Exception as e:
            print(f"Note: Could not add '{scope_name}' to agent client: {e}")

        # Add the echo-upstream-aud scope as optional so the token exchange
        # request with scope=echo-upstream-aud succeeds.
        try:
            keycloak_admin.add_client_optional_client_scope(agent_internal_id, echo_upstream_scope_id, {})
            print(f"Added '{ECHO_UPSTREAM_SCOPE}' as optional scope on agent client.")
        except Exception as e:
            print(f"Note: Could not add '{ECHO_UPSTREAM_SCOPE}' to agent client: {e}")
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
Keycloak is configured for the Echo credential placeholder-swap demo.

Created:
  Realm:    {KEYCLOAK_REALM}
  Clients:  {ECHO_UPSTREAM_CLIENT_ID} (target audience for token exchange)
  Scopes:   {scope_name} (realm DEFAULT - auto-adds Agent's SPIFFE ID to aud)
            {ECHO_UPSTREAM_SCOPE} (realm OPTIONAL - for exchanged tokens)
  Users:    alice (alice123), bob (bob123) — both with admin role

Token flow:
  1. UI gets token for user (aud includes Agent's SPIFFE ID via default scope)
  2. UI sends request to Agent with token
  3. AuthBridge inbound jwt-validation validates the token, stashes the real
     token in the shared store, and forwards an `abph_...` placeholder
  4. Agent echoes the placeholder out to echo-upstream
  5. AuthBridge outbound token-exchange resolves the placeholder back to the
     real token, then exchanges it: aud={agent_spiffe_id} → aud=echo-upstream
  6. echo-upstream returns the exchanged token in its response body

Note (gotcha): the agent's Keycloak client is registered dynamically when the
agent pod starts. If you ran this script BEFORE the agent client existed,
re-run it after the agent is up so the optional scopes attach to the agent
client (see "Adding scopes to agent client" above).
"""
    )


if __name__ == "__main__":
    main()
