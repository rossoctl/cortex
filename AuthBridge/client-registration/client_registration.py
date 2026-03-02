"""
client_registration.py

Registers a Keycloak client and stores its secret in a file.
Also creates an audience scope for the agent and adds it to
platform clients (e.g., the UI client) so they can reach
AuthBridge-protected agents without manual Keycloak configuration.

Idempotent:
- Creates the client if it does not exist.
- If the client already exists, reuses it.
- Always retrieves and stores the client secret.
- Creates audience scope if it does not exist.
- Adds audience scope to platform clients if not already assigned.
"""

import os
from typing import Any
import jwt
from keycloak import KeycloakAdmin, KeycloakPostError


def get_env_var(name: str, default: str | None = None) -> str:
    """
    Fetch an environment variable or return default if provided.
    Raise ValueError if missing and no default is set.
    """
    value = os.environ.get(name)
    if value is not None and value != "":
        return value
    if default is not None:
        return default
    raise ValueError(f"Missing required environment variable: {name}")


def write_client_secret(
    keycloak_admin: KeycloakAdmin,
    internal_client_id: str,
    client_name: str,
    secret_file_path: str = "secret.txt",
) -> None:
    """
    Retrieve the secret for a Keycloak client and write it to a file.
    """
    try:
        # There will be a value field if client authentication is enabled
        # client authentication is enabled if "publicClient" is False
        secret = keycloak_admin.get_client_secrets(internal_client_id)["value"]
        print(f'Successfully retrieved secret for client "{client_name}".')
    except KeycloakPostError as e:
        print(f"Could not retrieve secret for client '{client_name}': {e}")
        return

    try:
        with open(secret_file_path, "w") as f:
            f.write(secret)
        print(f'Secret written to file: "{secret_file_path}"')
    except OSError as ose:
        print(f"Error writing secret to file: {ose}")


# TODO: refactor this function so kagenti-client-registration image can use it
def register_client(keycloak_admin: KeycloakAdmin, client_id: str, client_payload: dict[str, Any]) -> str:
    """
    Ensure a Keycloak client exists.
    Returns the internal client ID.
    """
    internal_client_id = keycloak_admin.get_client_id(client_id)
    if internal_client_id:
        print(f'Client "{client_id}" already exists with ID: {internal_client_id}')
        return internal_client_id

    # Create client
    try:
        internal_client_id = keycloak_admin.create_client(client_payload)

        print(f'Created Keycloak client "{client_id}": {internal_client_id}')
        return internal_client_id
    except KeycloakPostError as e:
        print(f'Could not create client "{client_id}": {e}')
        raise


def get_client_id() -> str:
    """
    Read the SVID JWT from file and extract the client ID from the "sub" claim.
    """
    # Read SVID JWT from file to get client ID
    jwt_file_path = "/opt/jwt_svid.token"
    content = None
    try:
        with open(jwt_file_path, "r") as file:
            content = file.read()

    except FileNotFoundError:
        print(f"Error: The file {jwt_file_path} was not found.")
    except Exception as e:
        print(f"An error occurred: {e}")

    if content is None or content.strip() == "":
        raise Exception("No content read from SVID JWT.")

    # Decode JWT to get client ID
    decoded = jwt.decode(content, options={"verify_signature": False})
    if "sub" not in decoded:
        raise Exception('SVID JWT does not contain a "sub" claim.')
    return decoded["sub"]


def get_or_create_audience_scope(
    keycloak_admin: KeycloakAdmin, scope_name: str, audience: str
) -> str | None:
    """
    Create a client scope with an audience mapper if it doesn't exist.
    Returns the scope ID, or None on failure.
    """
    scopes = keycloak_admin.get_client_scopes()
    for scope in scopes:
        if scope["name"] == scope_name:
            print(f'Audience scope "{scope_name}" already exists with ID: {scope["id"]}')
            return scope["id"]

    try:
        scope_id = keycloak_admin.create_client_scope(
            {
                "name": scope_name,
                "protocol": "openid-connect",
                "attributes": {
                    "include.in.token.scope": "true",
                    "display.on.consent.screen": "true",
                },
            }
        )
        print(f'Created audience scope "{scope_name}": {scope_id}')
    except KeycloakPostError as e:
        print(f'Could not create audience scope "{scope_name}": {e}')
        return None

    # Add audience mapper to the scope
    mapper_payload = {
        "name": scope_name,
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
        print(f'Added audience mapper for "{audience}" to scope "{scope_name}"')
    except Exception as e:
        print(f'Note: Could not add audience mapper (might already exist): {e}')

    return scope_id


def add_scope_to_platform_clients(
    keycloak_admin: KeycloakAdmin,
    scope_id: str,
    scope_name: str,
    platform_client_ids: list[str],
) -> None:
    """
    Add an audience scope as a default client scope on each platform client.
    This ensures existing clients (like the UI) include the agent's audience
    in their tokens without requiring manual Keycloak configuration.
    """
    for platform_client_id in platform_client_ids:
        internal_id = keycloak_admin.get_client_id(platform_client_id)
        if not internal_id:
            print(
                f'Platform client "{platform_client_id}" not found in realm. '
                f'Skipping scope assignment.'
            )
            continue
        try:
            keycloak_admin.add_client_default_client_scope(
                internal_id, scope_id, {}
            )
            print(
                f'Added scope "{scope_name}" to platform client "{platform_client_id}".'
            )
        except Exception as e:
            # 409 Conflict means it's already assigned — that's fine
            if "409" in str(e) or "already" in str(e).lower():
                print(
                    f'Scope "{scope_name}" already assigned to "{platform_client_id}".'
                )
            else:
                print(
                    f'Could not add scope "{scope_name}" to "{platform_client_id}": {e}'
                )


client_name = get_env_var("CLIENT_NAME")

# If SPIFFE is enabled, use the client ID from the SVID JWT.
# Otherwise, use the client name as the client ID.
if get_env_var("SPIRE_ENABLED", "false").lower() == "true":
    client_id = get_client_id()
else:
    client_id = client_name

try:
    KEYCLOAK_URL = get_env_var("KEYCLOAK_URL")
    KEYCLOAK_TOKEN_EXCHANGE_ENABLED = (
        get_env_var("KEYCLOAK_TOKEN_EXCHANGE_ENABLED", "true").lower() == "true"
    )
    KEYCLOAK_CLIENT_REGISTRATION_ENABLED = (
        get_env_var("KEYCLOAK_CLIENT_REGISTRATION_ENABLED", "true").lower() == "true"
    )
    # CLIENT_AUTH_TYPE controls how the client authenticates to Keycloak:
    # - "client-secret": Traditional client_secret authentication (default)
    # - "federated-jwt": JWT-SVID authentication via SPIFFE identity provider
    CLIENT_AUTH_TYPE = get_env_var("CLIENT_AUTH_TYPE", "client-secret")
except ValueError as e:
    print(
        f"Expected environment variable missing. Skipping client registration of {client_id}."
    )
    print(e)
    exit(1)

if not KEYCLOAK_CLIENT_REGISTRATION_ENABLED:
    print(
        f"Client registration (KEYCLOAK_CLIENT_REGISTRATION_ENABLED=false) disabled. Skipping registration of {client_id}."
    )
    exit(0)

keycloak_admin = KeycloakAdmin(
    server_url=KEYCLOAK_URL,
    username=get_env_var("KEYCLOAK_ADMIN_USERNAME"),
    password=get_env_var("KEYCLOAK_ADMIN_PASSWORD"),
    realm_name=get_env_var("KEYCLOAK_REALM"),
    user_realm_name="master",
)

# Build client payload based on authentication type
client_payload = {
    "name": client_name,
    "clientId": client_id,
    "standardFlowEnabled": True,
    "directAccessGrantsEnabled": True,
    "serviceAccountsEnabled": True,  # Required for client_credentials grant
    "fullScopeAllowed": False,
    "publicClient": False,  # Enable client authentication
    # Enable token exchange for this client.
    # Token exchange allows this client to exchange tokens for other tokens, potentially across different clients.
    # Use case: [EXPLAIN THE SPECIFIC USE CASE HERE, e.g., "Required for service-to-service authentication in microservices architecture."]
    # Security considerations: Ensure only trusted clients have this capability, restrict scopes and permissions as needed,
    # and audit usage to prevent privilege escalation or unauthorized access.
    "attributes": {
        "standard.token.exchange.enabled": str(
            KEYCLOAK_TOKEN_EXCHANGE_ENABLED
        ).lower(),  # Enable token exchange
    },
}

# Configure client authentication type
if CLIENT_AUTH_TYPE == "federated-jwt":
    print(f"Configuring client for JWT-SVID authentication (federated-jwt)")
    client_payload["clientAuthenticatorType"] = "federated-jwt"
    # Add federated JWT attributes for SPIFFE authentication
    # These tell Keycloak to validate JWT-SVIDs from the SPIFFE identity provider
    spiffe_idp_alias = get_env_var("SPIFFE_IDP_ALIAS", "spire-spiffe")
    client_payload["attributes"].update({
        "jwt.credential.issuer": spiffe_idp_alias,
        "jwt.credential.sub": client_id,  # Must match JWT sub claim (SPIFFE ID)
    })
else:
    print(f"Configuring client for client-secret authentication")
    client_payload["clientAuthenticatorType"] = "client-secret"

internal_client_id = register_client(
    keycloak_admin,
    client_id,
    client_payload,
)

try:
    secret_file_path = get_env_var("SECRET_FILE_PATH")
except ValueError:
    secret_file_path = "/shared/secret.txt"
print(
    f'Writing secret for client ID: "{client_id}" (internal client ID: "{internal_client_id}") to file: "{secret_file_path}"'
)
write_client_secret(
    keycloak_admin,
    internal_client_id,
    client_name,
    secret_file_path=secret_file_path,
)

# --- Audience scope management ---
# Create an audience scope for this agent and add it to platform clients
# so their tokens include this agent's audience (required by AuthBridge).
AUDIENCE_SCOPE_ENABLED = (
    get_env_var("KEYCLOAK_AUDIENCE_SCOPE_ENABLED", "true").lower() == "true"
)

if AUDIENCE_SCOPE_ENABLED:
    # Derive scope name from client_name (namespace/sa → agent-namespace-sa-aud)
    scope_name = "agent-" + client_name.replace("/", "-") + "-aud"

    print(f'\n--- Audience scope management for "{scope_name}" ---')

    scope_id = get_or_create_audience_scope(keycloak_admin, scope_name, client_id)

    if scope_id:
        # Add as realm default so new clients automatically get this scope
        try:
            keycloak_admin.add_default_default_client_scope(scope_id)
            print(f'Added "{scope_name}" as realm default scope.')
        except Exception as e:
            print(f'Note: Could not add "{scope_name}" as realm default (might already exist): {e}')

        # Add to platform clients (e.g., the UI client)
        platform_clients_raw = get_env_var("PLATFORM_CLIENT_IDS", "kagenti")
        platform_client_ids = [
            c.strip() for c in platform_clients_raw.split(",") if c.strip()
        ]
        if platform_client_ids:
            print(f"Adding scope to platform clients: {platform_client_ids}")
            add_scope_to_platform_clients(
                keycloak_admin, scope_id, scope_name, platform_client_ids
            )
    else:
        print(f'Warning: Could not create audience scope "{scope_name}". '
              f'Platform clients will not automatically include this agent\'s audience.')

print("Client registration complete.")
