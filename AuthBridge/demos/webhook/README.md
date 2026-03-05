# AuthBridge Webhook Demo

This guide demonstrates how to use the **kagenti-webhook** to automatically inject AuthBridge sidecars into your deployments for transparent OAuth 2.0 token exchange.

## Overview

The kagenti-webhook watches for deployments with the `kagenti.io/inject: enabled` label and automatically injects:

| Container | Purpose |
|-----------|---------|
| `proxy-init` | Init container that sets up iptables to redirect inbound and outbound traffic |
| `spiffe-helper` | Fetches SPIFFE credentials from SPIRE (only with `kagenti.io/spire: enabled`) |
| `kagenti-client-registration` | Registers the workload with Keycloak (using SPIFFE ID or static client ID) |
| `envoy-proxy` | Intercepts inbound HTTP requests (JWT validation) and outbound requests (HTTP: token exchange; HTTPS: TLS passthrough) |

## Architecture

```
┌────────────────────────────────────────────────────────────────────┐
│                        Agent Pod                                   │
│  ┌─────────────┐  ┌──────────────┐  ┌────────────────────────────┐ │
│  │   agent     │  │spiffe-helper │  │keycloak-client-registration│ |
│  │ (your app)  │  │              │  │                            │ │
│  └──────┬──────┘  └──────────────┘  └────────────────────────────┘ │
│         │                                                          │
│         │ HTTP Request with Token (aud: agent-spiffe-id)           │
│         ▼                                                          │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │                    envoy-proxy                              │   │
│  │  Inbound (port 15124):                                      │   │
│  │    1. Intercepts incoming traffic (via iptables PREROUTING) │   │
│  │    2. Validates JWT (signature + issuer via JWKS)            │   │
│  │    3. Returns 401 if invalid, forwards if valid              │   │
│  │  Outbound (port 15123):                                     │   │
│  │    1. Intercepts outbound traffic (via iptables OUTPUT)     │   │
│  │    2. Detects protocol via tls_inspector                    │   │
│  │    HTTP: Extracts Bearer token, exchanges via Keycloak,     │   │
│  │          replaces token in request                          │   │
│  │    HTTPS: Passes through as-is (TLS passthrough)            │   │
│  └─────────────────────────────────────────────────────────────┘   │
└────────────────────────────────────────────────────────────────────┘
                              │
                              │ HTTP Request with Exchanged Token
                              ▼
                    ┌─────────────────┐
                    │   auth-target   │
                    │ (validates aud: │
                    │  auth-target)   │
                    └─────────────────┘
```

## Prerequisites

1. **Kubernetes cluster** with the kagenti-webhook installed
2. **Keycloak** deployed in the `keycloak` namespace
3. **SPIRE** deployed (optional, for SPIFFE-based identity)
4. **AuthBridge images** available from GitHub Container Registry:
   - `ghcr.io/kagenti/kagenti-extensions/proxy-init:latest`
   - `ghcr.io/kagenti/kagenti-extensions/envoy-with-processor:latest`
   - `ghcr.io/kagenti/kagenti-extensions/demo-app:latest`
   - `ghcr.io/kagenti/kagenti-extensions/client-registration:latest`

---

## Deploy Webhook

Deploy the webhook and its prerequisites with a single command:

```bash
cd kagenti-webhook

# Deploy webhook + create namespace + apply ConfigMaps
AUTHBRIDGE_DEMO=true ./scripts/webhook-rollout.sh
```

Or specify a custom namespace:

```bash
AUTHBRIDGE_DEMO=true AUTHBRIDGE_NAMESPACE=myapp ./scripts/webhook-rollout.sh
```

This automatically:
1. Builds and deploys the kagenti-webhook
2. Creates the namespace with `kagenti-enabled=true` label
3. Applies all required ConfigMaps (environments, authbridge-config, envoy-config, spiffe-helper-config)

**Note for custom deployments:** If deploying to a different namespace or using a different service account, update the `EXPECTED_AUDIENCE` value in `configmaps-webhook.yaml` to match your agent's SPIFFE ID:
```yaml
EXPECTED_AUDIENCE: "spiffe://localtest.me/ns/<your-namespace>/sa/<your-service-account>"
```

The ConfigMaps include:

- `environments` - Keycloak connection settings for client-registration
- `authbridge-config` - Token exchange and inbound validation configuration for envoy-proxy:
  - `TOKEN_URL` - Keycloak token endpoint for token exchange
  - `ISSUER` - Expected JWT issuer for inbound validation (required)
  - `EXPECTED_AUDIENCE` - Expected audience for inbound validation (optional, if not set audience validation is skipped)
  - `TARGET_AUDIENCE` - Target audience for outbound token exchange
  - `TARGET_SCOPES` - Scopes for exchanged tokens
- `spiffe-helper-config` - SPIFFE helper configuration (for SPIRE mode)
- `envoy-config` - Envoy proxy configuration

## Labels Reference

| Label | Value | Description |
|-------|-------|-------------|
| `kagenti.io/type` | `agent` | **Required**: Identifies workload as an agent |
| `kagenti.io/inject` | `enabled` | Enable AuthBridge sidecar injection |
| `kagenti.io/inject` | `disabled` | Disable injection (for target services) |
| `kagenti.io/spire` | `enabled` | Enable SPIFFE-based identity with SPIRE |
| `kagenti.io/spire` | `disabled` | Use static client ID (no SPIRE) |

**Note**: All labels must be on the **Pod template** (`spec.template.metadata.labels`), not the Deployment metadata.

Then continue with:
- [Step 1: Setup Keycloak](#step-1-setup-keycloak) - Configure Keycloak clients and scopes
- [Step 2: Deploy Auth Target and Agent](#step-2-deploy-auth-target-and-agent) - Deploy the demo workloads
- [Step 3: Test Token Exchange](#step-3-test-the-flow) - Verify the flow works

---

## Demo Deployment Steps

### Step 1: Setup Keycloak

Run the Keycloak setup script to configure the realm, clients, and scopes:

```bash
cd AuthBridge

# Activate virtual environment
python -m venv venv
source venv/bin/activate

pip install --upgrade pip
pip install -r requirements.txt

cd demos/webhook
# Run setup for webhook deployment (default: team1 namespace, agent service account)
python setup_keycloak.py
```

Or specify custom namespace/service account:

```bash
python setup_keycloak.py --namespace myapp --service-account mysa
```

This creates:

- `auth-target` client (target audience for token exchange)
- `agent-<namespace>-<sa>-aud` scope (adds agent's SPIFFE ID to token audience)
- `auth-target-aud` scope (adds "auth-target" to exchanged tokens)
- `alice` demo user (for testing subject preservation)

### Step 2: Deploy Auth Target and Agent

Deploy the target service and agent workload:

```bash
# Deploy auth-target (validates exchanged tokens)
# Note: auth-target has kagenti.io/inject: disabled to prevent sidecar injection
kubectl apply -f k8s/auth-target-deployment-webhook.yaml

# Deploy agent - choose ONE of the following:

# Option A: With SPIFFE (requires SPIRE)
kubectl apply -f k8s/agent-deployment-webhook.yaml

# Option B: Without SPIFFE (uses static client ID)
kubectl apply -f k8s/agent-deployment-webhook-no-spiffe.yaml

# Wait for the pods to be ready:
kubectl wait --for=condition=available --timeout=180s deployment/auth-target -n team1
kubectl wait --for=condition=available --timeout=180s deployment/agent -n team1
```

Verify the injected containers:

```bash
kubectl get pod -n team1 -l app=agent -o jsonpath='{.items[0].spec.containers[*].name}'
# Expected (with SPIFFE):    agent spiffe-helper kagenti-client-registration envoy-proxy
# Expected (without SPIFFE): agent kagenti-client-registration envoy-proxy
```

## Step 3: Test the Flow

These tests verify both **inbound** JWT validation and **outbound** token exchange end-to-end. By sending requests from outside the agent pod, each request exercises the full pipeline:

1. **Inbound**: Envoy intercepts the incoming request, ext-proc validates the JWT (signature + issuer)
2. **Outbound**: auth-proxy forwards to auth-target, Envoy intercepts the outgoing request, ext-proc exchanges the token

### Setup

```bash
# Start a test client pod (sends requests from outside the agent pod)
kubectl run test-client --image=nicolaka/netshoot -n team1 --restart=Never -- sleep 3600
kubectl wait --for=condition=ready pod/test-client -n team1 --timeout=30s

# Get the agent's client credentials (from envoy-proxy container which has the shared volume)
CLIENT_ID=$(kubectl exec deployment/agent -n team1 -c envoy-proxy -- cat /shared/client-id.txt)
CLIENT_SECRET=$(kubectl exec deployment/agent -n team1 -c envoy-proxy -- cat /shared/client-secret.txt)
echo "Client ID: $CLIENT_ID"

# Get a service account token (using test-client which has curl)
TOKEN=$(kubectl exec test-client -n team1 -- curl -s -X POST \
  "http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token" \
  -d "grant_type=client_credentials" \
  -d "client_id=$CLIENT_ID" \
  -d "client_secret=$CLIENT_SECRET" | jq -r '.access_token')

# Get a user token for alice (for subject preservation test)
USER_TOKEN=$(kubectl exec test-client -n team1 -- curl -s -X POST \
  "http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token" \
  -d "grant_type=password" \
  -d "client_id=$CLIENT_ID" \
  -d "client_secret=$CLIENT_SECRET" \
  -d "username=alice" \
  -d "password=alice123" | jq -r '.access_token')
```

### 5a. Inbound Rejection - No Token

```bash
kubectl exec test-client -n team1 -- curl -s http://agent-service:8080/test
# Expected: {"error":"unauthorized","message":"missing Authorization header"}
```

### 5b. Inbound Rejection - Invalid Token

```bash
kubectl exec test-client -n team1 -- curl -s -H "Authorization: Bearer invalid-token" http://agent-service:8080/test
# Expected: {"error":"unauthorized","message":"token validation failed: ..."}
```

### 5c. End-to-End with Service Account Token

Inbound validation passes, outbound token exchange converts `aud: <agent SPIFFE ID>` → `aud: auth-target`:

```bash
kubectl exec test-client -n team1 -- curl -s -H "Authorization: Bearer $TOKEN" http://agent-service:8080/test
# Expected: "authorized"
```

### 5d. End-to-End with User Token (Subject Preservation)

Same as 5c, but using alice's user token. The `sub` and `preferred_username` claims are preserved through token exchange:

```bash
kubectl exec test-client -n team1 -- curl -s -H "Authorization: Bearer $USER_TOKEN" http://agent-service:8080/test
# Expected: "authorized"
```

### Clean Up

```bash
kubectl delete pod test-client -n team1 --ignore-not-found
```

### Quick Test Commands

Run all tests as a single script:

```bash
kubectl run test-client --image=nicolaka/netshoot -n team1 --restart=Never -- sleep 3600 2>/dev/null
kubectl wait --for=condition=ready pod/test-client -n team1 --timeout=30s

CLIENT_ID=$(kubectl exec deployment/agent -n team1 -c envoy-proxy -- cat /shared/client-id.txt)
CLIENT_SECRET=$(kubectl exec deployment/agent -n team1 -c envoy-proxy -- cat /shared/client-secret.txt)

TOKEN=$(kubectl exec test-client -n team1 -- curl -s -X POST \
  "http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token" \
  -d "grant_type=client_credentials" -d "client_id=$CLIENT_ID" -d "client_secret=$CLIENT_SECRET" | jq -r '.access_token')

USER_TOKEN=$(kubectl exec test-client -n team1 -- curl -s -X POST \
  "http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token" \
  -d "grant_type=password" -d "client_id=$CLIENT_ID" -d "client_secret=$CLIENT_SECRET" \
  -d "username=alice" -d "password=alice123" | jq -r '.access_token')

echo "=== 5a. No Token (expect 401) ==="
kubectl exec test-client -n team1 -- curl -s http://agent-service:8080/test
echo ""

echo "=== 5b. Invalid Token (expect 401) ==="
kubectl exec test-client -n team1 -- curl -s -H "Authorization: Bearer invalid-token" http://agent-service:8080/test
echo ""

echo "=== 5c. Service Account Token (expect authorized) ==="
kubectl exec test-client -n team1 -- curl -s -H "Authorization: Bearer $TOKEN" http://agent-service:8080/test
echo ""

echo "=== 5d. User Token - alice (expect authorized) ==="
kubectl exec test-client -n team1 -- curl -s -H "Authorization: Bearer $USER_TOKEN" http://agent-service:8080/test
echo ""

kubectl delete pod test-client -n team1 --ignore-not-found
```

## Troubleshooting

### Check Pod Status

```bash
kubectl get pods -n team1
kubectl describe pod -l app=agent -n team1
```

### Check Container Logs

```bash
# Client registration logs
kubectl logs deployment/agent -n team1 -c kagenti-client-registration

# Envoy proxy logs (includes token exchange)
kubectl logs deployment/agent -n team1 -c envoy-proxy | grep -E "(Token Exchange|error)"

# SPIFFE helper logs
kubectl logs deployment/agent -n team1 -c spiffe-helper
```

### Common Issues

1. **"Requested audience not available: auth-target"**
   - Ensure `TARGET_SCOPES` in `authbridge-config` includes `auth-target-aud`
   - Run `setup_keycloak.py` again to create the required scopes

2. **ConfigMap not found errors**
   - Apply `k8s/configmaps-webhook.yaml` to the target namespace

3. **Image pull errors**
   - Images are automatically pulled from `ghcr.io/kagenti/kagenti-extensions/`
   - If you need to build locally for development:
     ```bash
     cd AuthBridge/AuthProxy
     make build
     # Load into Kind cluster
     kind load docker-image --name <cluster> localhost/proxy-init:latest
     kind load docker-image --name <cluster> localhost/envoy-with-processor:latest
     ```
   - Update `container_builder.go` to use `localhost/` images if testing locally

4. **SPIFFE credentials not ready**
   - Ensure SPIRE is deployed and the workload is registered
   - Check spiffe-helper logs for connection issues

## Cleanup

To remove all resources created during this demo:

### 1. Delete Deployments and Services

```bash
# Delete agent and auth-target deployments
kubectl delete deployment agent -n team1
kubectl delete deployment auth-target -n team1
kubectl delete service auth-target-service -n team1
kubectl delete serviceaccount agent -n team1
```

### 2. Delete ConfigMaps

```bash
kubectl delete configmap environments -n team1
kubectl delete configmap authbridge-config -n team1
kubectl delete configmap envoy-config -n team1
kubectl delete configmap spiffe-helper-config -n team1
```

### 3. Delete Keycloak Resources (Optional)

If you want to clean up Keycloak clients and scopes:

```bash
# Get admin token
ADMIN_TOKEN=$(curl -s http://keycloak.localtest.me:8080/realms/master/protocol/openid-connect/token \
  -d "grant_type=password" \
  -d "client_id=admin-cli" \
  -d "username=admin" \
  -d "password=admin" | jq -r ".access_token")

# Delete the dynamically registered agent client
CLIENT_ID="spiffe://localtest.me/ns/team1/sa/agent"
INTERNAL_ID=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak.localtest.me:8080/admin/realms/kagenti/clients?clientId=$CLIENT_ID" | jq -r ".[0].id")
curl -s -X DELETE -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak.localtest.me:8080/admin/realms/kagenti/clients/$INTERNAL_ID"

# Delete auth-target client
AUTH_TARGET_ID=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak.localtest.me:8080/admin/realms/kagenti/clients?clientId=auth-target" | jq -r ".[0].id")
curl -s -X DELETE -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak.localtest.me:8080/admin/realms/kagenti/clients/$AUTH_TARGET_ID"

# Delete demo user alice
ALICE_ID=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak.localtest.me:8080/admin/realms/kagenti/users?username=alice" | jq -r ".[0].id")
curl -s -X DELETE -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak.localtest.me:8080/admin/realms/kagenti/users/$ALICE_ID"

echo "Keycloak resources cleaned up"
```

### 4. Delete Namespace (Optional)

If you created a dedicated namespace for this demo:

```bash
# This will delete everything in the namespace
kubectl delete namespace team1
```

### 5. Remove Webhook (Optional)

If you want to remove the AuthBridge webhook entirely:

```bash
kubectl delete mutatingwebhookconfiguration kagenti-webhook-authbridge-mutating-webhook-configuration
```

### Quick Cleanup (Delete Everything)

For a complete cleanup including the namespace:

```bash
# Delete namespace (removes all resources inside)
kubectl delete namespace team1

# Remove webhook configuration
kubectl delete mutatingwebhookconfiguration kagenti-webhook-authbridge-mutating-webhook-configuration
```
