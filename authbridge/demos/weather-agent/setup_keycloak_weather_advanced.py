"""
setup_keycloak_weather_advanced.py — Keycloak setup for the Weather Agent
advanced AuthBridge demo (outbound token exchange + AuthBridge on the tool).

Token exchange targets the weather tool's OAuth client ID, which matches its
SPIFFE ID (written to /shared/client-id.txt by client-registration). That way
tokens arriving at the tool pass AuthBridge inbound audience checks.

Usage:
  cd AuthBridge && source venv/bin/activate && pip install -r requirements.txt
  python demos/weather-agent/setup_keycloak_weather_advanced.py

  python demos/weather-agent/setup_keycloak_weather_advanced.py --wait-tool-client

Security: uses default Keycloak admin credentials for local demos only.
"""

from __future__ import annotations

import argparse
import os
import sys
import time

from keycloak import KeycloakAdmin, KeycloakGetError, KeycloakPostError

KEYCLOAK_URL = os.environ.get("KEYCLOAK_URL", "http://keycloak.localtest.me:8080")
KEYCLOAK_REALM = os.environ.get("KEYCLOAK_REALM", "rossoctl")
KEYCLOAK_ADMIN_USERNAME = os.environ.get("KEYCLOAK_ADMIN_USERNAME", "admin")
KEYCLOAK_ADMIN_PASSWORD = os.environ.get("KEYCLOAK_ADMIN_PASSWORD", "admin")

SPIFFE_TRUST_DOMAIN = "localtest.me"
UI_CLIENT_ID = os.environ.get("UI_CLIENT_ID", "rossoctl")

# Public client for automated tests (ROPC / direct access) — the UI client often
# has direct access grants disabled.
E2E_ROPC_CLIENT_ID = "weather-advanced-e2e"

DEMO_USER = {
    "username": "alice",
    "email": "alice@example.com",
    "firstName": "Alice",
    "lastName": "Demo",
    "password": "alice123",
}


def get_spiffe_id(namespace: str, service_account: str) -> str:
    return f"spiffe://{SPIFFE_TRUST_DOMAIN}/ns/{namespace}/sa/{service_account}"


def get_or_create_realm(keycloak_admin, realm_name: str) -> None:
    realms = keycloak_admin.get_realms()
    for realm in realms:
        if realm["realm"] == realm_name:
            print(f"Realm '{realm_name}' already exists.")
            return
    keycloak_admin.create_realm({"realm": realm_name, "enabled": True, "displayName": realm_name})
    print(f"Created realm '{realm_name}'.")


def get_or_create_client_scope(keycloak_admin, scope_payload: dict) -> str:
    scope_name = scope_payload.get("name")
    scopes = keycloak_admin.get_client_scopes()
    for scope in scopes:
        if scope["name"] == scope_name:
            print(f"Client scope '{scope_name}' already exists (id={scope['id']}).")
            return scope["id"]
    scope_id = keycloak_admin.create_client_scope(scope_payload)
    print(f"Created client scope '{scope_name}': {scope_id}")
    return scope_id


def add_audience_mapper(keycloak_admin, scope_id: str, mapper_name: str, audience: str) -> None:
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
        print(f"Note: could not add mapper '{mapper_name}' (may already exist): {e}")


def get_or_create_client(keycloak_admin, client_payload: dict) -> str:
    client_id = client_payload["clientId"]
    internal_id = keycloak_admin.get_client_id(client_id)
    if internal_id:
        print(f"Client '{client_id}' already exists.")
        return internal_id
    internal_id = keycloak_admin.create_client(client_payload)
    print(f"Created client '{client_id}'.")
    return internal_id


def get_or_create_user(keycloak_admin, user_config: dict) -> str:
    username = user_config["username"]
    users = keycloak_admin.get_users({"username": username})
    existing = next((u for u in users if u.get("username") == username), None)
    if existing:
        print(f"User '{username}' already exists.")
        return existing["id"]
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
    print(f"Created user '{username}' with id={user_id}")
    return user_id


def enable_token_exchange_on_client(keycloak_admin, client_identifier: str) -> bool:
    """Enable standard OAuth token exchange for a realm client (by clientId string)."""
    internal_id = keycloak_admin.get_client_id(client_identifier)
    if not internal_id:
        return False
    try:
        client = keycloak_admin.get_client(internal_id)
        attrs = dict(client.get("attributes") or {})
        if attrs.get("standard.token.exchange.enabled") == "true":
            print(f"Token exchange already enabled for client '{client_identifier}'.")
            return True
        attrs["standard.token.exchange.enabled"] = "true"
        keycloak_admin.update_client(internal_id, {"attributes": attrs})
        print(f"Enabled token exchange for client '{client_identifier}'.")
        return True
    except Exception as e:
        print(f"Note: could not enable token exchange on '{client_identifier}': {e}")
        return False


def wait_for_client(keycloak_admin, client_identifier: str, timeout_s: int = 300) -> bool:
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        if keycloak_admin.get_client_id(client_identifier):
            print(f"Keycloak client '{client_identifier}' is present.")
            return True
        print(f"Waiting for Keycloak client '{client_identifier}' ... ({int(deadline - time.time())}s left)")
        time.sleep(5)
    return False


def main() -> None:
    parser = argparse.ArgumentParser(description="Keycloak setup for Weather advanced AuthBridge demo")
    parser.add_argument("-n", "--namespace", default="team1", help="Kubernetes namespace")
    parser.add_argument(
        "-a",
        "--agent-service-account",
        default="weather-service-advanced",
        help="Agent ServiceAccount name (SPIFFE sa segment)",
    )
    parser.add_argument(
        "-t",
        "--tool-service-account",
        default="weather-tool-advanced",
        help="Tool ServiceAccount name (SPIFFE sa segment)",
    )
    parser.add_argument(
        "--wait-tool-client",
        action="store_true",
        help="Wait until the tool's SPIFFE client is registered in Keycloak",
    )
    parser.add_argument(
        "--tool-client-timeout",
        type=int,
        default=300,
        help="Seconds to wait for tool client when --wait-tool-client is set",
    )
    args = parser.parse_args()

    ns = args.namespace
    agent_sa = args.agent_service_account
    tool_sa = args.tool_service_account
    agent_spiffe = get_spiffe_id(ns, agent_sa)
    tool_spiffe = get_spiffe_id(ns, tool_sa)

    if KEYCLOAK_ADMIN_USERNAME == "admin" and KEYCLOAK_ADMIN_PASSWORD == "admin":
        print(
            "WARNING: Using default Keycloak admin credentials. Not for production.",
            file=sys.stderr,
        )

    print("=" * 72)
    print("Weather Agent (advanced) — Keycloak setup")
    print("=" * 72)
    print(f"Namespace:     {ns}")
    print(f"Agent SPIFFE:  {agent_spiffe}")
    print(f"Tool SPIFFE:   {tool_spiffe}")

    print(f"\nConnecting to Keycloak at {KEYCLOAK_URL} ...")
    try:
        master_admin = KeycloakAdmin(
            server_url=KEYCLOAK_URL,
            username=KEYCLOAK_ADMIN_USERNAME,
            password=KEYCLOAK_ADMIN_PASSWORD,
            realm_name="master",
            user_realm_name="master",
        )
    except Exception as e:
        print(f"Failed to connect to Keycloak: {e}", file=sys.stderr)
        sys.exit(1)

    get_or_create_realm(master_admin, KEYCLOAK_REALM)

    keycloak_admin = KeycloakAdmin(
        server_url=KEYCLOAK_URL,
        username=KEYCLOAK_ADMIN_USERNAME,
        password=KEYCLOAK_ADMIN_PASSWORD,
        realm_name=KEYCLOAK_REALM,
        user_realm_name="master",
    )

    # --- Agent audience (realm default): tokens for the agent include agent SPIFFE in aud ---
    agent_aud_scope_name = f"agent-{ns}-{agent_sa}-aud"
    print(f"\n--- Client scope: {agent_aud_scope_name} ---")
    agent_aud_scope_id = get_or_create_client_scope(
        keycloak_admin,
        {
            "name": agent_aud_scope_name,
            "protocol": "openid-connect",
            "attributes": {
                "include.in.token.scope": "true",
                "display.on.consent.screen": "true",
            },
        },
    )
    add_audience_mapper(keycloak_admin, agent_aud_scope_id, agent_aud_scope_name, agent_spiffe)

    try:
        keycloak_admin.add_default_default_client_scope(agent_aud_scope_id)
        print(f"Added '{agent_aud_scope_name}' as realm default client scope.")
    except Exception as e:
        print(f"Note: could not add realm default scope '{agent_aud_scope_name}': {e}")

    # --- Optional scope: adds tool SPIFFE to exchanged tokens ---
    exchange_scope_name = "weather-tool-exchange-aud"
    print(f"\n--- Client scope: {exchange_scope_name} (optional) ---")
    exchange_scope_id = get_or_create_client_scope(
        keycloak_admin,
        {
            "name": exchange_scope_name,
            "protocol": "openid-connect",
            "attributes": {
                "include.in.token.scope": "true",
                "display.on.consent.screen": "true",
            },
        },
    )
    add_audience_mapper(
        keycloak_admin,
        exchange_scope_id,
        f"{exchange_scope_name}-mapper",
        tool_spiffe,
    )
    try:
        keycloak_admin.add_default_optional_client_scope(exchange_scope_id)
        print(f"Added '{exchange_scope_name}' as realm OPTIONAL client scope.")
    except Exception as e:
        print(f"Note: could not add optional scope '{exchange_scope_name}': {e}")

    # --- E2E public client (ROPC for deploy_and_verify_advanced.sh) ---
    print(f"\n--- E2E client '{E2E_ROPC_CLIENT_ID}' (ROPC / direct access) ---")
    get_or_create_client(
        keycloak_admin,
        {
            "clientId": E2E_ROPC_CLIENT_ID,
            "name": "Weather advanced E2E (direct access grants)",
            "enabled": True,
            "publicClient": True,
            "standardFlowEnabled": True,
            "directAccessGrantsEnabled": True,
        },
    )
    e2e_internal = keycloak_admin.get_client_id(E2E_ROPC_CLIENT_ID)
    if e2e_internal:
        try:
            keycloak_admin.add_client_default_client_scope(e2e_internal, agent_aud_scope_id, {})
            print(f"Added '{agent_aud_scope_name}' as default scope on '{E2E_ROPC_CLIENT_ID}'.")
        except Exception as e:
            print(f"Note: could not add default scope to E2E client: {e}")

    # --- UI client: ensure tokens include agent audience ---
    print(f"\n--- UI client '{UI_CLIENT_ID}' ---")
    ui_internal = keycloak_admin.get_client_id(UI_CLIENT_ID)
    if ui_internal:
        try:
            keycloak_admin.add_client_default_client_scope(ui_internal, agent_aud_scope_id, {})
            print(f"Added '{agent_aud_scope_name}' as default scope on UI client.")
        except Exception as e:
            print(f"Note: could not add default scope to UI client: {e}")
    else:
        print(f"Warning: UI client '{UI_CLIENT_ID}' not found.")

    # --- Tool client: token exchange target must allow exchange ---
    if args.wait_tool_client:
        if not wait_for_client(keycloak_admin, tool_spiffe, timeout_s=args.tool_client_timeout):
            print(f"Timeout waiting for tool client '{tool_spiffe}'.", file=sys.stderr)
            sys.exit(1)
    if keycloak_admin.get_client_id(tool_spiffe):
        enable_token_exchange_on_client(keycloak_admin, tool_spiffe)
    else:
        print(
            f"\nNote: tool client '{tool_spiffe}' not registered yet. "
            "Deploy the tool pod, then re-run this script with --wait-tool-client "
            "or run again after the tool registers."
        )

    # --- Agent dynamic client (if already registered) ---
    print("\n--- Agent Keycloak client (if registered) ---")
    agent_internal = keycloak_admin.get_client_id(agent_spiffe)
    if agent_internal:
        enable_token_exchange_on_client(keycloak_admin, agent_spiffe)
        try:
            keycloak_admin.add_client_default_client_scope(agent_internal, agent_aud_scope_id, {})
            print(f"Added '{agent_aud_scope_name}' as default on agent client.")
        except Exception as e:
            print(f"Note: could not add default scope on agent client: {e}")
        for name, sid in [(exchange_scope_name, exchange_scope_id)]:
            try:
                keycloak_admin.add_client_optional_client_scope(agent_internal, sid, {})
                print(f"Added '{name}' as optional scope on agent client.")
            except Exception as e:
                print(f"Note: could not add optional '{name}' on agent client: {e}")
    else:
        print(
            f"Agent client '{agent_spiffe}' not found yet. "
            "Deploy the agent, then re-run this script so optional exchange scopes apply."
        )

    # --- Demo user ---
    print("\n--- Demo user ---")
    get_or_create_user(keycloak_admin, DEMO_USER)
    try:
        uid = keycloak_admin.get_user_id(DEMO_USER["username"])
        keycloak_admin.set_user_password(uid, DEMO_USER["password"], temporary=False)
        print("Ensured demo user 'alice' credential (for E2E ROPC / direct access).")
    except Exception as e:
        print(f"Note: could not update user credential for alice: {e}")
    try:
        admin_role = keycloak_admin.get_realm_role("admin")
        uid = keycloak_admin.get_user_id(DEMO_USER["username"])
        keycloak_admin.assign_realm_roles(uid, [admin_role])
        print("Assigned realm role 'admin' to 'alice'.")
    except (KeycloakGetError, KeycloakPostError, TypeError) as e:
        print(f"Note: could not assign admin role to alice: {e}")

    print("\n" + "=" * 72)
    print("SETUP COMPLETE")
    print("=" * 72)


if __name__ == "__main__":
    main()
