"""
setup_keycloak.py - AuthBridge Demo Setup

This script configures Keycloak for the AuthBridge demo that combines:
1. Client Registration with SPIFFE ID (for the Agent pod identity)
2. AuthProxy sidecar for token exchange (using the auto-registered client)
3. Auth Target (target server) that validates exchanged tokens

Architecture:
  Caller → gets token (aud: Agent's SPIFFE ID) → passes to Agent
                                              ↓
  Agent Pod (Agent + SPIFFE Helper + Client Registration + AuthProxy)
       |
       | Agent calls Auth Target with Caller's token
       v
  AuthProxy (Envoy) - validates token, exchanges using Agent's own credentials
       |
       | Token Exchange → audience "auth-target"
       v
  Auth Target (validates token has aud=auth-target)

Clients created:
- auth-target: Target audience for token exchange (required by Keycloak)

Client Scopes created:
- agent-spiffe-aud: Adds Agent's SPIFFE ID to token audience (realm DEFAULT - auto-included)
- auth-target-aud: Adds "auth-target" to token audience (realm OPTIONAL - for token exchange only)

Demo Users created:
- alice: Demo user to demonstrate subject (sub) claim preservation through token exchange
  Username: alice, Password: alice123

Note: The Agent workload is auto-registered by the client-registration container
using the SPIFFE ID as the client ID. The agent-spiffe-aud scope adds the Agent's
SPIFFE ID to all tokens as audience. This allows the AuthProxy (using the same
auto-registered credentials) to exchange tokens.

IMPORTANT: The SPIFFE ID is hardcoded for the demo:
  spiffe://localtest.me/ns/authbridge/sa/agent
If your namespace or service account differs, update AGENT_SPIFFE_ID below.
"""

from keycloak import KeycloakAdmin, KeycloakPostError
import sys

KEYCLOAK_URL = "http://keycloak.localtest.me:8080"
KEYCLOAK_REALM = "kagenti"
KEYCLOAK_ADMIN_USERNAME = "admin"
KEYCLOAK_ADMIN_PASSWORD = "admin"

# SPIFFE ID for the Agent pod (namespace: authbridge, serviceAccount: agent)
# Update this if your deployment uses different namespace/serviceAccount
AGENT_SPIFFE_ID = "spiffe://localtest.me/ns/authbridge/sa/agent"

# Demo user for demonstrating subject preservation
DEMO_USER = {
    "username": "alice",
    "email": "alice@example.com",
    "firstName": "Alice",
    "lastName": "Demo",
    "password": "alice123"
}


def get_or_create_realm(keycloak_admin, realm_name):
    """Create realm if it doesn't exist."""
    try:
        realms = keycloak_admin.get_realms()
        for realm in realms:
            if realm['realm'] == realm_name:
                print(f"Realm '{realm_name}' already exists.")
                return
        keycloak_admin.create_realm({
            "realm": realm_name,
            "enabled": True,
            "displayName": realm_name,
        })
        print(f"Created realm '{realm_name}'.")
    except Exception as e:
        print(f"Error checking/creating realm: {e}")


def get_or_create_client(keycloak_admin, client_payload):
    """Create client if doesn't exist, return internal client ID."""
    client_id = client_payload['clientId']
    existing_client_id = keycloak_admin.get_client_id(client_id)
    if existing_client_id:
        print(f"Client '{client_id}' already exists.")
        return existing_client_id
    internal_id = keycloak_admin.create_client(client_payload)
    print(f"Created client '{client_id}'.")
    return internal_id


def get_or_create_client_scope(keycloak_admin, scope_payload):
    """Create client scope if doesn't exist, return scope ID."""
    scope_name = scope_payload.get("name")
    scopes = keycloak_admin.get_client_scopes()
    for scope in scopes:
        if scope['name'] == scope_name:
            print(f"Client scope '{scope_name}' already exists with ID: {scope['id']}")
            return scope['id']

    try:
        scope_id = keycloak_admin.create_client_scope(scope_payload)
        print(f"Created client scope '{scope_name}': {scope_id}")
        return scope_id
    except KeycloakPostError as e:
        print(f"Could not create client scope '{scope_name}': {e}")
        raise


def add_audience_mapper(keycloak_admin, scope_id, mapper_name, audience):
    """Add audience protocol mapper to a client scope."""
    mapper_payload = {
        "name": mapper_name,
        "protocol": "openid-connect",
        "protocolMapper": "oidc-audience-mapper",
        "consentRequired": False,
        "config": {
            "included.custom.audience": audience,
            "id.token.claim": "false",
            "access.token.claim": "true",
            "userinfo.token.claim": "false"
        }
    }
    
    try:
        keycloak_admin.add_mapper_to_client_scope(scope_id, mapper_payload)
        print(f"Added audience mapper '{mapper_name}' for audience '{audience}'")
    except Exception as e:
        # Mapper might already exist
        print(f"Note: Could not add mapper '{mapper_name}' (might already exist): {e}")


def get_or_create_user(keycloak_admin, user_config):
    """Create a demo user if it doesn't exist."""
    username = user_config["username"]
    
    # Check if user exists (get_users may be fuzzy, so filter for exact username)
    users = keycloak_admin.get_users({"username": username})
    exact_users = [u for u in users if u.get("username") == username]
    if exact_users:
        print(f"User '{username}' already exists.")
        return exact_users[0]["id"]
    
    # Create user
    try:
        user_id = keycloak_admin.create_user({
            "username": username,
            "email": user_config["email"],
            "firstName": user_config["firstName"],
            "lastName": user_config["lastName"],
            "enabled": True,
            "emailVerified": True,
            "credentials": [{
                "type": "password",
                "value": user_config["password"],
                "temporary": False
            }]
        })
        print(f"Created user '{username}' with ID: {user_id}")
        return user_id
    except KeycloakPostError as e:
        print(f"Could not create user '{username}': {e}")
        raise


def main():
    print("=" * 60)
    print("AuthBridge Demo - Keycloak Setup")
    print("=" * 60)
    print(f"\nAgent SPIFFE ID: {AGENT_SPIFFE_ID}")
    
    # Connect to Keycloak master realm first
    print(f"\nConnecting to Keycloak at {KEYCLOAK_URL}...")
    try:
        master_admin = KeycloakAdmin(
            server_url=KEYCLOAK_URL,
            username=KEYCLOAK_ADMIN_USERNAME,
            password=KEYCLOAK_ADMIN_PASSWORD,
            realm_name="master",
            user_realm_name="master"
        )
    except Exception as e:
        print(f"Failed to connect to Keycloak: {e}")
        print("\nMake sure Keycloak is running and accessible at:")
        print(f"  {KEYCLOAK_URL}")
        print("\nIf using port-forward, run:")
        print("  kubectl port-forward service/keycloak-service -n keycloak 8080:8080")
        sys.exit(1)
    
    # Create demo realm if needed
    print(f"\n--- Setting up realm: {KEYCLOAK_REALM} ---")
    get_or_create_realm(master_admin, KEYCLOAK_REALM)
    
    # Switch to demo realm
    keycloak_admin = KeycloakAdmin(
        server_url=KEYCLOAK_URL,
        username=KEYCLOAK_ADMIN_USERNAME,
        password=KEYCLOAK_ADMIN_PASSWORD,
        realm_name=KEYCLOAK_REALM,
        user_realm_name="master"
    )
    
    # Create auth-target client (required as token exchange audience target)
    print("\n--- Creating auth-target client ---")
    print("This client is required as the target audience for token exchange")
    auth_target_id = get_or_create_client(keycloak_admin, {
        "clientId": "auth-target",
        "name": "Auth Target",
        "enabled": True,
        "publicClient": False,
        "standardFlowEnabled": False,
        "serviceAccountsEnabled": True,
        "attributes": {
            "standard.token.exchange.enabled": "true"
        }
    })
    
    # Create client scopes
    print("\n--- Creating client scopes ---")
    
    # agent-spiffe-aud scope - adds Agent's SPIFFE ID to token audience (realm default)
    # This allows the auto-registered Agent client to exchange tokens
    print(f"\nCreating scope for Agent's SPIFFE ID audience...")
    agent_spiffe_scope_id = get_or_create_client_scope(keycloak_admin, {
        "name": "agent-spiffe-aud",
        "protocol": "openid-connect",
        "attributes": {
            "include.in.token.scope": "true",
            "display.on.consent.screen": "true"
        }
    })
    add_audience_mapper(keycloak_admin, agent_spiffe_scope_id, "agent-spiffe-aud", AGENT_SPIFFE_ID)
    
    # auth-target-aud scope - added to exchanged tokens
    # This makes the AuthProxy's exchanged token valid for auth-target
    auth_target_scope_id = get_or_create_client_scope(keycloak_admin, {
        "name": "auth-target-aud",
        "protocol": "openid-connect",
        "attributes": {
            "include.in.token.scope": "true",
            "display.on.consent.screen": "true"
        }
    })
    add_audience_mapper(keycloak_admin, auth_target_scope_id, "auth-target-aud", "auth-target")
    
    # Assign scopes
    print("\n--- Assigning scopes ---")
    
    # Add agent-spiffe-aud as realm default scope
    # This ensures all clients (including auto-registered Agent) get tokens with
    # the Agent's SPIFFE ID in the audience, allowing AuthProxy to exchange them
    try:
        keycloak_admin.add_default_default_client_scope(agent_spiffe_scope_id)
        print("Added 'agent-spiffe-aud' as realm default scope (all clients will get it).")
    except Exception as e:
        print(f"Note: Could not add 'agent-spiffe-aud' as realm default (might already exist): {e}")
    
    # Add auth-target-aud as realm OPTIONAL scope (not default!)
    # - OPTIONAL means: available to clients for explicit requests, but NOT auto-included in tokens
    # - This allows token exchange to request this scope without polluting the first token
    try:
        keycloak_admin.add_default_optional_client_scope(auth_target_scope_id)
        print("Added 'auth-target-aud' as realm OPTIONAL scope (available for token exchange, not auto-included).")
    except Exception as e:
        print(f"Note: Could not add 'auth-target-aud' as optional scope (might already exist): {e}")
    
    # Create demo user for demonstrating subject preservation
    print("\n--- Creating demo user ---")
    print("This user demonstrates how the subject (sub) claim is preserved during token exchange")
    get_or_create_user(keycloak_admin, DEMO_USER)
    
    # Retrieve and display info
    print("\n" + "=" * 60)
    print("SETUP COMPLETE")
    print("=" * 60)
    
    print("\n" + "=" * 60)
    print("NEXT STEPS")
    print("=" * 60)
    
    print("\n1. Deploy the AuthBridge demo:")
    print("\n   # With SPIFFE (requires SPIRE)")
    print("   kubectl apply -f demos/single-target/k8s/authbridge-deployment.yaml")
    print("\n   # OR without SPIFFE")
    print("   kubectl apply -f demos/single-target/k8s/authbridge-deployment-no-spiffe.yaml\n")
    
    print("2. Wait for pods to be ready:")
    print("\n   kubectl wait --for=condition=available --timeout=120s deployment/agent -n authbridge")
    print("   kubectl wait --for=condition=available --timeout=120s deployment/auth-target -n authbridge\n")
    
    print("3. Test from inside the agent pod:")
    print(f"""
   kubectl exec -it deployment/agent -n authbridge -c agent -- sh
   
   # Inside the container (credentials are auto-populated by client-registration):
   CLIENT_ID=$(cat /shared/client-id.txt)
   CLIENT_SECRET=$(cat /shared/client-secret.txt)
   
   # Get a token (simulating what a Caller would do)
   # The token will have aud: {AGENT_SPIFFE_ID}
   TOKEN=$(curl -sX POST \\
     http://keycloak-service.keycloak.svc:8080/realms/demo/protocol/openid-connect/token \\
     -d 'grant_type=client_credentials' \\
     -d "client_id=$CLIENT_ID" \\
     -d "client_secret=$CLIENT_SECRET" | jq -r '.access_token')
   
   # Verify token audience (should be the Agent's SPIFFE ID)
   echo $TOKEN | cut -d'.' -f2 | tr '_-' '/+' | {{ read p; echo "${{p}}=="; }} | base64 -d | jq '{{aud, azp, scope}}'
   
   # Agent calls auth-target (AuthProxy will exchange token for aud: auth-target)
   curl -H "Authorization: Bearer $TOKEN" http://auth-target-service:8081/test
   # Expected: "authorized"
""")
    
    print("4. Test with a USER TOKEN (demonstrates subject preservation):")
    print(f"""
   # Get a token for demo user 'alice' using password grant
   # This demonstrates how the user's identity (sub claim) is preserved during exchange
   
   USER_TOKEN=$(curl -sX POST \\
     http://keycloak-service.keycloak.svc:8080/realms/demo/protocol/openid-connect/token \\
     -d 'grant_type=password' \\
     -d "client_id=$CLIENT_ID" \\
     -d "client_secret=$CLIENT_SECRET" \\
     -d 'username={DEMO_USER["username"]}' \\
     -d 'password={DEMO_USER["password"]}' | jq -r '.access_token')
   
   # Check the ORIGINAL token - note the 'sub' claim contains alice's user ID
   # and 'preferred_username' shows 'alice'
   echo "=== ORIGINAL TOKEN (user: alice) ==="
   echo $USER_TOKEN | cut -d'.' -f2 | tr '_-' '/+' | {{ read p; echo "${{p}}=="; }} | base64 -d | jq '{{sub, preferred_username, aud, azp}}'
   
   # Call auth-target - token exchange preserves the subject!
   curl -H "Authorization: Bearer $USER_TOKEN" http://auth-target-service:8081/test
   # Expected: "authorized"
   
   # Check auth-target logs to see alice's subject in the exchanged token:
   kubectl logs deployment/auth-target -n authbridge | grep -A5 "JWT Debug" | tail -10
""")
    
    print("\n" + "-" * 60)
    print("HOW IT WORKS")
    print("-" * 60)
    print(f"""
1. Agent pod starts and registers with Keycloak using its SPIFFE ID:
   client_id = {AGENT_SPIFFE_ID}

2. Credentials are saved to /shared/client-id.txt and /shared/client-secret.txt

3. When a Caller gets a token, it has:
   aud: {AGENT_SPIFFE_ID}  (via agent-spiffe-aud realm default scope)

4. AuthProxy intercepts outgoing requests and exchanges the token using
   the same credentials from /shared/ (matching the token's audience)

5. The exchanged token has aud: auth-target

No pre-configured 'agent' client needed - the agent registers itself
dynamically and AuthProxy uses the resulting client credentials!
""")


if __name__ == "__main__":
    main()
