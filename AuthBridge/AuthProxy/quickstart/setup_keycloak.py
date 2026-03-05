from keycloak import KeycloakAdmin, KeycloakGetError, KeycloakPostError

KEYCLOAK_URL = "http://keycloak.localtest.me:8080"
KEYCLOAK_REALM = "kagenti"
KEYCLOAK_ADMIN_USERNAME = "admin"
KEYCLOAK_ADMIN_PASSWORD = "admin"

# Helper functions
def get_or_create_user(keycloak_admin, username):
    users = keycloak_admin.get_users({"username": username})
    user_id = None
    if users:
        # Filter strictly because search is fuzzy
        existing_user = next((u for u in users if u['username'] == username), None)
        if existing_user:
            user_id = existing_user['id']
            print(f"User '{username}' already exists.")
    if not user_id:
        user_id = keycloak_admin.create_user({
            "username": username,
            "enabled": True,
            "email": f"{username}@test.com",
            "emailVerified": True,
            "firstName": username,
            "lastName": username,
        }, True)
        print(f"Created user '{username}'.")
    return user_id

def get_or_create_client(keycloak_admin, client_payload):
    existing_client_id = keycloak_admin.get_client_id(client_payload["clientId"])
    if existing_client_id:
        print(f"Client '{client_payload["clientId"]}' already exists.")
        return existing_client_id
    client_id = keycloak_admin.create_client(client_payload)
    print(f"Created client '{client_payload["clientId"]}'.")
    return client_id

def get_or_create_client_scope(keycloak_admin, scope_payload):
    """
    Creates a client scope if it doesn't exist, or returns the ID of the existing one.
    """
    scope_name = scope_payload.get("name")
    
    # Keycloak python wrapper doesn't have a direct 'get_scope_id', so we list and filter
    scopes = keycloak_admin.get_client_scopes()
    for scope in scopes:
        if scope['name'] == scope_name:
            print(f"Client scope '{scope_name}' already exists with ID: {scope['id']}")
            return scope['id']

    # Create new scope
    try:
        scope_id = keycloak_admin.create_client_scope(scope_payload)
        print(f"Created client scope '{scope_name}': {scope_id}")
        return scope_id
    except KeycloakPostError as e:
        print(f"Could not create client scope '{scope_name}': {e}")
        raise

def add_audience_mapper(keycloak_admin, scope_id, mapper_name, audience):
    """
    Adds an audience protocol mapper to a client scope if it doesn't already exist.
    """
    # Note: we do not pre-check for existing mappers here; Keycloak will handle duplicates or raise errors.

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
        print(f"Failed to add mapper '{mapper_name}': {e}")

# initialize keycloak admin client
print(f"Connecting to Keycloak at {KEYCLOAK_URL} as {KEYCLOAK_ADMIN_USERNAME}...")
keycloak_admin = KeycloakAdmin(
    server_url=KEYCLOAK_URL,
    username=KEYCLOAK_ADMIN_USERNAME,
    password=KEYCLOAK_ADMIN_PASSWORD,
    realm_name=KEYCLOAK_REALM,
    user_realm_name="master"
)

# create test-user
test_user_name = "test-user"
user_id = get_or_create_user(keycloak_admin, test_user_name)
keycloak_admin.set_user_password(user_id, "password", temporary=False)
print(f"Set password for '{test_user_name}'.")

# Create application-caller Client
app_caller_id = get_or_create_client(keycloak_admin, {
    "clientId": "application-caller",
    "name": "Application Caller",
    "enabled": True,
    "publicClient": False,  # Creates a confidential client (Client Auth)
    "directAccessGrantsEnabled": True,
    "standardFlowEnabled": False
})

# Create authproxy Client
authproxy_id = get_or_create_client(keycloak_admin, {
    "clientId": "authproxy",
    "name": "Auth Proxy",
    "enabled": True,
    "publicClient": False,  # Confidential client
    "standardFlowEnabled": False,
    "serviceAccountsEnabled": True,
    "attributes": {
        "standard.token.exchange.enabled": "true"
    }
})

# Create demoapp Client (target service for token exchange)
demoapp_id = get_or_create_client(keycloak_admin, {
    "clientId": "demoapp",
    "name": "Demo App",
    "enabled": True,
    "publicClient": False,  # Confidential client
    "standardFlowEnabled": False,
    "serviceAccountsEnabled": True,
})

# Create `authproxy-aud` Client scope
authproxy_scope_id = get_or_create_client_scope(keycloak_admin, {
    "name": "authproxy-aud",
    "protocol": "openid-connect",
    "attributes": {
        "include.in.token.scope": "true",
        "display.on.consent.screen": "true"
    }
})
add_audience_mapper(keycloak_admin, authproxy_scope_id, "authproxy-aud", "authproxy")

# Create `demoapp-aud` Client scope
demoapp_scope_id = get_or_create_client_scope(keycloak_admin, {
    "name": "demoapp-aud",
    "protocol": "openid-connect",
    "attributes": {
        "include.in.token.scope": "true",
        "display.on.consent.screen": "true"
    }
})
add_audience_mapper(keycloak_admin, demoapp_scope_id, "demoapp-aud", "demoapp")

# Assign default scopes
try:
    keycloak_admin.add_client_default_client_scope(app_caller_id, authproxy_scope_id, {})
    print("Assigned 'authproxy-aud' as default scope to 'application-caller'.")
except Exception as e:
    # Keycloak might raise error if already assigned
    print(f"Note: Could not assign 'authproxy-aud' scope (might already exist): {e}")

# Add 'demoapp-aud' to 'authproxy' as default
try:
    keycloak_admin.add_client_default_client_scope(authproxy_id, demoapp_scope_id, {})
    print("Assigned 'demoapp-aud' as default scope to 'authproxy'.")
except Exception as e:
    print(f"Note: Could not assign 'demoapp-aud' scope (might already exist): {e}")

print("-" * 50)
try:
    secret = keycloak_admin.get_client_secrets(app_caller_id)['value']
    print(f"Run the following command to set the client secret:")
    print(f"export CLIENT_SECRET={secret}")
except Exception as e:
    print(f"Could not retrieve secret: {e}")
print("-" * 50)