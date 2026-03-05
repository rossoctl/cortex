"""
setup_keycloak.py - Keycloak Setup for AuthBridge Webhook

This script configures Keycloak for deployments using the kagenti-webhook
to inject AuthBridge sidecars. Unlike the standalone demo, this setup supports
any namespace where the webhook is enabled.

Usage:
  python setup_keycloak.py [--namespace NAMESPACE] [--service-account SA]

Examples:
  # Default: team1 namespace, agent service account
  python setup_keycloak.py

  # Custom namespace and service account
  python setup_keycloak.py --namespace myapp --service-account mysa

Architecture:
  Workload with label 'kagenti.io/inject: enabled'
       ↓
  Webhook injects: proxy-init, spiffe-helper, client-registration, envoy-proxy
       ↓
  Client Registration registers workload with Keycloak using SPIFFE ID
       ↓
  Envoy intercepts outgoing requests and exchanges tokens

Clients created:
- auth-target: Target audience for token exchange (required by Keycloak)

Client Scopes created:
- agent-spiffe-aud: Adds Agent's SPIFFE ID to token audience (realm DEFAULT)
- auth-target-aud: Adds "auth-target" to token audience (realm OPTIONAL)

Demo Users created:
- alice: Demo user to demonstrate subject preservation (username: alice, password: alice123)

Security Note:
- This script uses default Keycloak admin credentials (username: "admin", password: "admin")
  for demo and local development only. These credentials are insecure and MUST NOT be used
  in any production or internet-exposed environment. Always override them via environment
  variables or other secure configuration when running outside a demo context.
"""

import argparse
import sys
import os
from keycloak import KeycloakAdmin, KeycloakPostError

# Default configuration
# NOTE: The default admin credentials below ("admin"/"admin") are for demo and local
# development purposes only and must not be used in production. Override them with
# environment variables when running in any non-demo environment.
KEYCLOAK_URL = os.environ.get("KEYCLOAK_URL", "http://keycloak.localtest.me:8080")
KEYCLOAK_REALM = os.environ.get("KEYCLOAK_REALM", "kagenti")
KEYCLOAK_ADMIN_USERNAME = os.environ.get("KEYCLOAK_ADMIN_USERNAME", "admin")
KEYCLOAK_ADMIN_PASSWORD = os.environ.get("KEYCLOAK_ADMIN_PASSWORD", "admin")

# Emit a warning if the insecure demo credentials are in use.
if KEYCLOAK_ADMIN_USERNAME == "admin" and KEYCLOAK_ADMIN_PASSWORD == "admin":
    print(
        "WARNING: Using default Keycloak admin credentials 'admin'/'admin'. "
        "These credentials are INSECURE and must NOT be used in production.",
        file=sys.stderr,
    )
# Default namespace and service account for webhook deployments
DEFAULT_NAMESPACE = "team1"
DEFAULT_SERVICE_ACCOUNT = "agent"
SPIFFE_TRUST_DOMAIN = "localtest.me"

# Demo user for demonstrating subject preservation
DEMO_USER = {
    "username": "alice",
    "email": "alice@example.com",
    "firstName": "Alice",
    "lastName": "Demo",
    "password": "alice123"
}


def get_spiffe_id(namespace: str, service_account: str) -> str:
    """Generate SPIFFE ID for a given namespace and service account."""
    return f"spiffe://{SPIFFE_TRUST_DOMAIN}/ns/{namespace}/sa/{service_account}"


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
    
    # Check if user exists
    users = keycloak_admin.get_users({"username": username})
    if users:
        print(f"User '{username}' already exists.")
        return users[0]["id"]
    
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
    parser = argparse.ArgumentParser(
        description="Setup Keycloak for AuthBridge webhook deployments"
    )
    parser.add_argument(
        "--namespace", "-n",
        default=DEFAULT_NAMESPACE,
        help=f"Kubernetes namespace for the agent (default: {DEFAULT_NAMESPACE})"
    )
    parser.add_argument(
        "--service-account", "-s",
        default=DEFAULT_SERVICE_ACCOUNT,
        help=f"Service account name for the agent (default: {DEFAULT_SERVICE_ACCOUNT})"
    )
    args = parser.parse_args()

    namespace = args.namespace
    service_account = args.service_account
    agent_spiffe_id = get_spiffe_id(namespace, service_account)

    print("=" * 70)
    print("AuthBridge Webhook - Keycloak Setup")
    print("=" * 70)
    print(f"\nNamespace:       {namespace}")
    print(f"Service Account: {service_account}")
    print(f"SPIFFE ID:       {agent_spiffe_id}")
    
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
    get_or_create_client(keycloak_admin, {
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
    scope_name = f"agent-{namespace}-{service_account}-aud"
    print(f"\nCreating scope for Agent's SPIFFE ID audience: {scope_name}")
    agent_spiffe_scope_id = get_or_create_client_scope(keycloak_admin, {
        "name": scope_name,
        "protocol": "openid-connect",
        "attributes": {
            "include.in.token.scope": "true",
            "display.on.consent.screen": "true"
        }
    })
    add_audience_mapper(keycloak_admin, agent_spiffe_scope_id, scope_name, agent_spiffe_id)
    
    # auth-target-aud scope - added to exchanged tokens
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
    
    try:
        keycloak_admin.add_default_default_client_scope(agent_spiffe_scope_id)
        print(f"Added '{scope_name}' as realm default scope.")
    except Exception as e:
        print(f"Note: Could not add '{scope_name}' as realm default (might already exist): {e}")
    
    try:
        keycloak_admin.add_default_optional_client_scope(auth_target_scope_id)
        print("Added 'auth-target-aud' as realm OPTIONAL scope.")
    except Exception as e:
        print(f"Note: Could not add 'auth-target-aud' as optional scope (might already exist): {e}")
    
    # Create demo user
    print("\n--- Creating demo user ---")
    print("This user demonstrates subject preservation during token exchange")
    get_or_create_user(keycloak_admin, DEMO_USER)
    
    # Print summary and next steps
    print("\n" + "=" * 70)
    print("SETUP COMPLETE")
    print("=" * 70)
    
    print("\n" + "=" * 70)
    print("REQUIRED CONFIGMAPS")
    print("=" * 70)
    print(f"""
Create these ConfigMaps in the {namespace} namespace:

# 1. environments ConfigMap (for client-registration)
kubectl create configmap environments -n {namespace} \\
  --from-literal=SPIRE_ENABLED=true \\
  --from-literal=KEYCLOAK_URL=http://keycloak-service.keycloak.svc:8080 \\
  --from-literal=KEYCLOAK_REALM=demo \\
  --from-literal=KEYCLOAK_ADMIN_USERNAME=admin \\
  --from-literal=KEYCLOAK_ADMIN_PASSWORD=admin

# 2. authbridge-config ConfigMap (for envoy-proxy)
kubectl create configmap authbridge-config -n {namespace} \\
  --from-literal=TOKEN_URL=http://keycloak-service.keycloak.svc:8080/realms/demo/protocol/openid-connect/token \\
  --from-literal=ISSUER=http://keycloak.localtest.me:8080/realms/demo \\
  --from-literal=EXPECTED_AUDIENCE=spiffe://localtest.me/ns/{namespace}/sa/{service_account} \\
  --from-literal=TARGET_AUDIENCE=auth-target \\
  --from-literal=TARGET_SCOPES="openid auth-target-aud"

# 3. spiffe-helper-config ConfigMap (for SPIRE-enabled mode)
kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: spiffe-helper-config
  namespace: {namespace}
data:
  helper.conf: |
    agent_address = "/spiffe-workload-api/spire-agent.sock"
    cmd = ""
    cmd_args = ""
    svid_file_name = "/opt/svid.pem"
    svid_key_file_name = "/opt/svid_key.pem"
    svid_bundle_file_name = "/opt/svid_bundle.pem"
    jwt_svids = [{{jwt_audience="kagenti", jwt_svid_file_name="/opt/jwt_svid.token"}}]
    jwt_svid_file_mode = 0644
EOF

# 4. envoy-config ConfigMap (for envoy-proxy)
# Copy from AuthBridge/k8s/authbridge-deployment.yaml or use:
kubectl get configmap envoy-config -n authbridge -o yaml | \\
  sed 's/namespace: authbridge/namespace: {namespace}/' | \\
  kubectl apply -f -
""")

    print("\n" + "=" * 70)
    print("DEPLOY WITH WEBHOOK")
    print("=" * 70)
    print(f"""
# Create ServiceAccount
kubectl create serviceaccount {service_account} -n {namespace}

# Deploy with webhook injection (SPIRE enabled)
kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: agent
  namespace: {namespace}
  labels:
    app: agent
    kagenti.io/inject: enabled
    kagenti.io/spire: enabled
spec:
  replicas: 1
  selector:
    matchLabels:
      app: agent
  template:
    metadata:
      labels:
        app: agent
    spec:
      serviceAccountName: {service_account}
      containers:
        - name: agent
          image: nicolaka/netshoot:latest
          command: ["sh", "-c", "while [ ! -f /shared/client-id.txt ]; do sleep 2; done; echo Ready; tail -f /dev/null"]
          volumeMounts:
            - name: shared-data
              mountPath: /shared
EOF
""")

    print("\n" + "=" * 70)
    print("TEST THE SETUP")
    print("=" * 70)
    print(f"""
# Wait for pod to be ready
kubectl wait --for=condition=available --timeout=180s deployment/agent -n {namespace}

# Exec into the agent container
kubectl exec -it deployment/agent -n {namespace} -c agent -- sh

# Inside the container:
CLIENT_ID=$(cat /shared/client-id.txt)
CLIENT_SECRET=$(cat /shared/client-secret.txt)

# Get a token
TOKEN=$(curl -sX POST http://keycloak-service.keycloak.svc:8080/realms/demo/protocol/openid-connect/token \\
  -d 'grant_type=client_credentials' \\
  -d "client_id=$CLIENT_ID" \\
  -d "client_secret=$CLIENT_SECRET" | jq -r '.access_token')

# Verify token (should have aud: {agent_spiffe_id})
echo $TOKEN | cut -d'.' -f2 | tr '_-' '/+' | {{ read p; echo "${{p}}=="; }} | base64 -d | jq '{{aud, azp, scope}}'

# Call auth-target (token exchange happens transparently)
curl -H "Authorization: Bearer $TOKEN" http://auth-target-service.authbridge:8081/test
# Expected: "authorized"

# Test with user token (demonstrates subject preservation)
USER_TOKEN=$(curl -sX POST http://keycloak-service.keycloak.svc:8080/realms/demo/protocol/openid-connect/token \\
  -d 'grant_type=password' \\
  -d "client_id=$CLIENT_ID" \\
  -d "client_secret=$CLIENT_SECRET" \\
  -d 'username={DEMO_USER["username"]}' \\
  -d 'password={DEMO_USER["password"]}' | jq -r '.access_token')

# Check alice's subject is preserved
echo $USER_TOKEN | cut -d'.' -f2 | tr '_-' '/+' | {{ read p; echo "${{p}}=="; }} | base64 -d | jq '{{sub, preferred_username, aud}}'
""")


if __name__ == "__main__":
    main()
