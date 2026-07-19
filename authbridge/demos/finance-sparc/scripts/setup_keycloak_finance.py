#!/usr/bin/env python3
"""Keycloak setup for the finance-sparc demo.

Makes the finance-agent's inbound jwt-validation pass for tokens minted by the
demo (scripted ROPC) and by the rossoctl UI, by creating an audience client
scope that puts the agent's SPIFFE id in the token `aud`, and a public
direct-access (ROPC) client + a demo user `alice`.

Idempotent. Run via:  uv run --with python-keycloak python setup_keycloak_finance.py
Env overrides: KEYCLOAK_URL, KEYCLOAK_REALM, KEYCLOAK_ADMIN_USERNAME/PASSWORD,
UI_CLIENT_ID, NAMESPACE, AGENT_SA.
"""
import os
import sys

from keycloak import KeycloakAdmin

KEYCLOAK_URL = os.environ.get("KEYCLOAK_URL", "http://keycloak.localtest.me:8080")
KEYCLOAK_REALM = os.environ.get("KEYCLOAK_REALM", "rossoctl")
ADMIN_USER = os.environ.get("KEYCLOAK_ADMIN_USERNAME", "admin")
ADMIN_PASS = os.environ.get("KEYCLOAK_ADMIN_PASSWORD", "admin")
UI_CLIENT_ID = os.environ.get("UI_CLIENT_ID", "rossoctl")
SPIFFE_TRUST_DOMAIN = os.environ.get("SPIFFE_TRUST_DOMAIN", "localtest.me")
NAMESPACE = os.environ.get("NAMESPACE", "team1")
AGENT_SA = os.environ.get("AGENT_SA", "finance-agent")
ROPC_CLIENT_ID = os.environ.get("ROPC_CLIENT_ID", "finance-sparc-e2e")
USER_NAME = "alice"
USER_PASS = "alice123"
USER = {"username": USER_NAME, "email": "alice@example.com",
        "enabled": True, "emailVerified": True, "firstName": "Alice", "lastName": "Demo"}


def main() -> int:
    agent_spiffe = f"spiffe://{SPIFFE_TRUST_DOMAIN}/ns/{NAMESPACE}/sa/{AGENT_SA}"
    scope_name = f"agent-{NAMESPACE}-{AGENT_SA}-aud"
    print(f"Keycloak: {KEYCLOAK_URL}  realm={KEYCLOAK_REALM}  agent aud={agent_spiffe}")

    kc = KeycloakAdmin(server_url=KEYCLOAK_URL, username=ADMIN_USER, password=ADMIN_PASS,
                       realm_name=KEYCLOAK_REALM, user_realm_name="master")

    # 1) audience client scope + mapper → agent SPIFFE id in aud
    scope_id = next((s["id"] for s in kc.get_client_scopes() if s["name"] == scope_name), None)
    if not scope_id:
        scope_id = kc.create_client_scope({
            "name": scope_name, "protocol": "openid-connect",
            "attributes": {"include.in.token.scope": "true", "display.on.consent.screen": "false"},
        }, skip_exists=True)
    print(f"  client scope {scope_name} -> {scope_id}")
    try:
        kc.add_mapper_to_client_scope(scope_id, {
            "name": scope_name + "-aud-mapper", "protocol": "openid-connect",
            "protocolMapper": "oidc-audience-mapper",
            "config": {"included.custom.audience": agent_spiffe,
                       "id.token.claim": "false", "access.token.claim": "true"},
        })
    except Exception as e:
        print(f"  (mapper exists or: {e})")
    try:
        kc.add_default_default_client_scope(scope_id)
        print(f"  '{scope_name}' is now a realm default scope")
    except Exception as e:
        print(f"  (realm default: {e})")

    # 2) public ROPC client for scripted drive
    if not kc.get_client_id(ROPC_CLIENT_ID):
        kc.create_client({"clientId": ROPC_CLIENT_ID, "name": "finance-sparc E2E (direct access)",
                          "enabled": True, "publicClient": True, "standardFlowEnabled": True,
                          "directAccessGrantsEnabled": True}, skip_exists=True)
    ropc_internal = kc.get_client_id(ROPC_CLIENT_ID)
    for label, cid in [("ROPC " + ROPC_CLIENT_ID, ropc_internal),
                       ("UI " + UI_CLIENT_ID, kc.get_client_id(UI_CLIENT_ID))]:
        if cid:
            try:
                kc.add_client_default_client_scope(cid, scope_id, {})
                print(f"  added aud scope as default on {label}")
            except Exception as e:
                print(f"  ({label} default scope: {e})")
        else:
            print(f"  note: client {label} not found (ok)")

    # 3) demo user alice (admin role → backend operator role for /api/chat)
    uid = kc.get_user_id(USER_NAME)
    if not uid:
        uid = kc.create_user(USER, exist_ok=True)
    kc.set_user_password(uid, USER_PASS, temporary=False)
    try:
        kc.assign_realm_roles(uid, [kc.get_realm_role("admin")])
    except Exception as e:
        print(f"  (assign admin role: {e})")
    print(f"  user alice ready ({uid})")

    print("Keycloak setup complete.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
