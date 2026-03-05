# AuthProxy

AuthProxy is a **token validation and exchange sidecar** for Kubernetes workloads. It enables secure service-to-service communication by:
- **Validating** incoming requests with JWT token verification (inbound)
- **Exchanging** tokens for ones with the correct audience for downstream services (outbound)

## What AuthProxy Does

AuthProxy solves a common challenge in microservices architectures: **how can a service call another service when each service expects tokens with different audiences?**

### The Problem

When a caller obtains a token, it's typically scoped to a specific audience (often the caller itself). If the caller tries to use that token to call a different service, the request will be rejected because the target service expects a different audience.

```
┌─────────────┐                      ┌──────────────┐
│   Caller    │ ── Token A ────────► │   Target     │  ❌ REJECTED
│ (aud: svc-a)│                      │ (expects     │     Wrong audience!
└─────────────┘                      │  aud: target)│
                                     └──────────────┘
```

### The Solution

AuthProxy intercepts outgoing requests and exchanges the token for a new one with the correct audience:

```
┌─────────────┐               ┌──────────────────────────┐              ┌─────────────┐
│   Caller    │ ── Token A ──►│       AuthProxy          │─ Token B ──► │   Target    │  ✅ AUTHORIZED
│             │               │  1. Intercept request    │              │             │
│ Token:      │               │  2. Exchange for new aud │              │ (expects    │
│ (aud: svc-a)│               │  3. Forward request      │              │ aud: target)│
└─────────────┘               └──────────────────────────┘              └─────────────┘
                                           │
                                           ▼
                                  ┌─────────────────┐
                                  │    Keycloak     │
                                  │ (Token Exchange)│
                                  └─────────────────┘
```

## Components

AuthProxy is a **single sidecar container** that consists of:

### Envoy Proxy with Ext Proc Filter

The sidecar runs an Envoy proxy with an external processor (ext-proc) filter:
- **Envoy Proxy** (port **15123** outbound, port **15124** inbound): Intercepts all traffic from and to the application container
- **Ext Proc Filter** (`go-processor/main.go`, port **9090**): Handles both directions:
  - **Inbound**: Validates JWT tokens (signature, issuer) using JWKS. Returns 401 Unauthorized for invalid tokens.
  - **Outbound HTTP**: Performs **OAuth 2.0 Token Exchange** ([RFC 8693](https://datatracker.ietf.org/doc/html/rfc8693)), replacing the `Authorization` header with an exchanged token for the target audience.
  - **Outbound HTTPS**: Envoy detects TLS via `tls_inspector` and passes traffic through as-is using `tcp_proxy` (no ext_proc, no token exchange). This ensures HTTPS connections are not broken by the sidecar.
  - Direction is detected via the `x-authbridge-direction` header injected by Envoy's inbound listener.

### Traffic Interception via iptables

To automatically route traffic, an **init container** (`proxy-init`) configures **iptables rules**:
- **Outbound** (OUTPUT chain): Redirects outgoing traffic to the Envoy outbound listener (port 15123)
- **Inbound** (PREROUTING chain): Redirects incoming traffic to the Envoy inbound listener (port 15124)

This ensures transparent interception in both directions without requiring any changes to the application code.

### Example Application (`main.go`)

The `main.go` file in this directory is **not** a core component of AuthProxy. It is an **example pass-through proxy** that forwards requests to a target service. JWT validation is handled entirely by the Ext Proc on the inbound path. Any application can benefit from AuthProxy simply by being deployed alongside the sidecar—no code changes required.

## Architecture

### Sidecar Deployment

When deployed as a sidecar, AuthProxy intercepts both **inbound** and **outbound** traffic via iptables:

```
                 Incoming request
                       │
                       ▼
┌───────────────────────────────────────────────────────────-───────┐
│                             POD                                   │
│                                                                   │
│  ┌─────────────┐       ┌──────────────────────────────────────┐   │
│  │ proxy-init  │       │        AuthProxy Sidecar             │   │
│  │ (iptables)  │       │                                      │   │
│  └──────┬──────┘       │  ┌────────────┐    ┌────────────┐    │   │
│         │              │  │   Envoy    │◄──►│  Ext Proc  │    │   │
│         │              │  │  :15123    │    │   :9090    │    │   │
│         ▼              │  │  :15124    │    │            │    │   │
│  ┌─────────────┐       │  └─────┬──────┘    └──────┬─────┘    │   │
│  │             │◄──────┼────────│ (inbound)        │          │   │
│  │ Application ├───────┼───────►│ (outbound)       │          │   │
│  │  (any app)  │       │        │                  │          │   │
│  └─────────────┘       └────────┼──────────────────┼──────────┘   │
│                                 │                  │              │
└─────────────────────────────────┼──────────────────┼──────────────┘
                                  │                  │
                                  ▼                  ▼
                        ┌──────────────────┐  ┌ ─ ─ ─ ─ ─ ─ ─ ─ ┐
                        │  Target Service  │      Keycloak
                        └──────────────────┘  │(token exchange) │
                                               ─ ─ ─ ─ ─ ─ ─ ─ ─
```

**How it works:**

**Inbound (incoming requests):**
1. **proxy-init** (init container): Sets up iptables PREROUTING rules to redirect incoming traffic to Envoy
2. **Envoy** (port 15124): Intercepts incoming traffic, injects `x-authbridge-direction: inbound` header, calls Ext Proc
3. **Ext Proc** (port 9090): Validates JWT token (signature + issuer via JWKS). Returns 401 if invalid.
4. **Envoy**: Forwards validated request to the application

**Outbound (outgoing requests):**
1. **proxy-init** (init container): Sets up iptables OUTPUT rules to redirect outbound traffic to Envoy
2. **Envoy** (port 15123): Intercepts outbound traffic, uses `tls_inspector` to detect the protocol:
   - **HTTP (plaintext)**: Calls Ext Proc via gRPC for token exchange, then forwards with the new Authorization header
   - **HTTPS (TLS)**: Passes traffic through as-is via `tcp_proxy` (no token exchange, preserving the original TLS connection)

The application requires **no code changes**—traffic interception is completely transparent.

## Configuration

### Token Exchange Configuration (AuthProxy Sidecar)

The Ext Proc reads token exchange configuration directly from environment variables at startup:

| Variable | Description | Source |
|----------|-------------|--------|
| `TOKEN_URL` | Keycloak token endpoint URL | Environment variable |
| `ISSUER` | Expected JWT issuer for inbound validation. Must match Keycloak's frontend URL (the `iss` claim in tokens). Required for inbound JWT validation. | Environment variable |
| `EXPECTED_AUDIENCE` | Expected audience claim for inbound JWT validation. Optional - if not set, audience validation is skipped. | Environment variable |
| `CLIENT_ID` | Client ID for token exchange | `/shared/client-id.txt` file or `CLIENT_ID` env var |
| `CLIENT_SECRET` | Client secret | `/shared/client-secret.txt` file or `CLIENT_SECRET` env var |
| `TARGET_AUDIENCE` | Target service audience for outbound token exchange | Environment variable |
| `TARGET_SCOPES` | Scopes for exchanged token | Environment variable |

> **Note:** `CLIENT_ID` and `CLIENT_SECRET` are preferentially loaded from `/shared/` files (when using dynamic client registration with SPIFFE). If files are not available, environment variables are used as fallback.

#### Configuration Secret

Token exchange is typically configured via a Kubernetes Secret:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: auth-proxy-config
stringData:
  TOKEN_URL: "http://keycloak:8080/realms/kagenti/protocol/openid-connect/token"
  ISSUER: "http://keycloak.example.com:8080/realms/demo"  # Required: must match Keycloak's frontend URL (iss claim in tokens)
  EXPECTED_AUDIENCE: "authproxy"  # Optional: expected audience for inbound requests
  CLIENT_ID: "authproxy"
  CLIENT_SECRET: "<your-client-secret>"
  TARGET_AUDIENCE: "target-service"
  TARGET_SCOPES: "openid target-service-aud"
```

## Token Exchange Flow

The Ext Proc performs OAuth 2.0 Token Exchange as defined in [RFC 8693](https://datatracker.ietf.org/doc/html/rfc8693):

```
POST /realms/kagenti/protocol/openid-connect/token
Content-Type: application/x-www-form-urlencoded

grant_type=urn:ietf:params:oauth:grant-type:token-exchange
&client_id=<client-id>
&client_secret=<client-secret>
&subject_token=<original-jwt>
&subject_token_type=urn:ietf:params:oauth:token-type:access_token
&requested_token_type=urn:ietf:params:oauth:token-type:access_token
&audience=<target-audience>
&scope=<target-scopes>
```

**Response:**

```json
{
  "access_token": "<new-jwt-with-target-audience>",
  "token_type": "Bearer",
  "expires_in": 300
}
```

## Quickstart

This section provides instructions to run the example application with the AuthProxy sidecar, without the full AuthBridge setup (no SPIFFE, no client-registration).

### Prerequisites

- Kubernetes cluster (Kind recommended)
- Keycloak deployed (or use [Kagenti installer](https://github.com/kagenti/kagenti/blob/main/docs/install.md))
- Docker/Podman for building images

### Step 1: Build and Deploy

```bash
cd AuthBridge/AuthProxy

# Build all images
make build-images

# Load into Kind cluster (set KIND_CLUSTER_NAME if not using default)
make load-images

# Deploy example app with AuthProxy sidecar
make deploy
```

This deploys:
- `auth-proxy` - Example pass-through proxy (port 8080) running alongside the AuthProxy sidecar (Envoy + Ext Proc). JWT validation is handled by the inbound Ext Proc.
- `demo-app` - Sample target application (port 8081) that validates exchanged tokens

### Step 2: Configure Keycloak

Port-forward Keycloak (in a separate terminal):

```bash
kubectl port-forward service/keycloak-service -n keycloak 8080:8080
```

Run the setup script to create necessary Keycloak clients:

```bash
cd quickstart

# Setup Python environment
python -m venv venv
source venv/bin/activate
pip install -r requirements.txt

# Configure Keycloak
python setup_keycloak.py
```

The script creates:
- `application-caller` client - for obtaining tokens (password grant)
- `authproxy` client - for token exchange
- `demoapp` client - target audience for token exchange
- `authproxy-aud` and `demoapp-aud` scopes
- A test user (`test-user` / `password`)

### Step 3: Create auth-proxy-config Secret and Test

Create the secret for token exchange credentials, then test the flow. See the [Quickstart Guide](./quickstart/README.md) for detailed instructions covering:
- Creating the `auth-proxy-config` Kubernetes Secret
- Port-forwarding the services
- Testing inbound validation (401 for missing/invalid tokens)
- Testing outbound token exchange (200 for valid tokens)

### View Logs

```bash
# Example application logs
kubectl logs deployment/auth-proxy -c auth-proxy

# Ext proc logs (inbound validation + outbound token exchange)
kubectl logs deployment/auth-proxy -c envoy-proxy

# Demo app (target service) logs
kubectl logs deployment/demo-app

# Follow ext proc logs in real-time
kubectl logs -f deployment/auth-proxy -c envoy-proxy
```

### Clean Up

```bash
# Remove deployments
make undeploy

# Delete Kind cluster (if desired)
make kind-delete
```

> **📘 For detailed standalone instructions**, see the [Quickstart Guide](./quickstart/README.md).

---

## Viewing Logs

When running the example deployment:

```bash
# Example application logs
kubectl logs <pod-name> -c auth-proxy

# AuthProxy sidecar logs (shows token exchange)
kubectl logs <pod-name> -c envoy-proxy
```

## Related Documentation

- [AuthBridge](../README.md) - Complete AuthBridge overview with token exchange flow
- [AuthBridge Demo](../demo.md) - Step-by-step demo instructions
- [Client Registration](../client-registration/README.md) - Automatic Keycloak client registration with SPIFFE
- [OAuth 2.0 Token Exchange (RFC 8693)](https://datatracker.ietf.org/doc/html/rfc8693)
- [Envoy External Processing](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_proc_filter)
