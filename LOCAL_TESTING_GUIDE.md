# Local Testing Guide for JWT-SVID Authentication

> **⚠️ This guide is stale after kagenti-extensions#411.** It was written
> for the pre-#411 multi-sidecar shape (separate `client-registration`,
> `envoy-with-processor`, and standalone `spiffe-helper` containers) and
> several of its YAML examples reference images that no longer publish.
> The image inventory at the top, the in-line pod spec, and the
> "Check ... logs" sections all assume the legacy shape.
>
> **What still works**: the script invocation in §1 — `local-build-and-test.sh`
> has been updated to build the three combined images (`authbridge`,
> `authbridge-envoy`, `authbridge-lite`) plus `proxy-init`. The rest of
> the guide needs a re-author against the operator-injected combined
> sidecar shape; track via a follow-up issue.
>
> **Working alternatives in the meantime**:
> - `authbridge/demos/weather-agent/demo-ui-advanced.md` — current
>   reference for the combined-sidecar flow with SPIFFE.
> - `authbridge/demos/github-issue/demo-ui.md` — full token-exchange
>   flow with scope-based access control.

This guide walks you through testing JWT-SVID authentication using local images (no push to ghcr.io).

## ⚠️ Important: Use the Build Script

**CRITICAL**: You MUST run the `./local-build-and-test.sh` script to build all required images. Do NOT build images individually with `docker build` or `podman build` commands, as this will miss critical images like `spiffe-idp-setup:local` (located in the kagenti repo).

The script:
- Builds images from **both** kagenti and kagenti-extensions repositories
- Automatically detects Docker vs Podman
- Loads all images into your Kind cluster
- Ensures consistent image tags and pull policies

## Prerequisites

- Docker or Podman running
- Kind CLI installed
- Both `kagenti` and `kagenti-extensions` repositories cloned

## Step 0: Create Kind Cluster

The Kagenti ansible installer can create a Kind cluster automatically, but for local image testing, it's better to create it manually first:

```bash
# Create a Kind cluster with the correct name
kind create cluster --name kagenti-dev --config - <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraPortMappings:
  - containerPort: 30080
    hostPort: 8080
    protocol: TCP
  - containerPort: 30443
    hostPort: 8443
    protocol: TCP
EOF

# Verify cluster is running
kubectl cluster-info --context kind-kagenti-dev
```

## Step 1: Build and Load Local Images

**⚠️ REQUIRED: Run the automated build script**

The `local-build-and-test.sh` script is the **only supported way** to build local images for testing. It builds images from both repositories and ensures everything is loaded correctly.

```bash
cd kagenti-extensions

# Make the script executable (first time only)
chmod +x local-build-and-test.sh

# Build all images and load into Kind cluster
# For Podman users: set KIND_EXPERIMENTAL_PROVIDER
export KIND_EXPERIMENTAL_PROVIDER=podman  # Only needed for Podman
./local-build-and-test.sh

# If using a different cluster name:
# CLUSTER_NAME=my-cluster ./local-build-and-test.sh
```

### What the script builds and loads:

**From kagenti repo** (critical - often missed!):
- `ghcr.io/kagenti/kagenti/spiffe-idp-setup:local` - Configures SPIFFE Identity Provider in Keycloak

**From kagenti-extensions repo**:
- `ghcr.io/kagenti/kagenti-extensions/client-registration:local` - Registers agents as Keycloak clients
- `ghcr.io/kagenti/kagenti-extensions/envoy-with-processor:local` - Envoy proxy with token exchange
- `ghcr.io/kagenti/kagenti-extensions/proxy-init:local` - iptables initialization

### Common Mistakes to Avoid:

❌ **DON'T** run individual `docker build` or `podman build` commands
❌ **DON'T** skip building images from the kagenti repo
❌ **DON'T** forget to set `KIND_EXPERIMENTAL_PROVIDER=podman` if using Podman

✅ **DO** run `./local-build-and-test.sh` every time you need to rebuild
✅ **DO** verify all images are loaded: `kind get images --name kagenti-dev | grep :local`

**Note for Podman users:** The script automatically detects Podman and uses tar archives to load images into Kind, since `kind load docker-image` doesn't work with Podman's image store.

## Step 2: Install Kagenti with Ansible

**IMPORTANT:** For federated-JWT testing with local images, use the unified `federated-jwt-values.yaml` overlay file from kagenti-extensions.

The ansible installer will detect the existing `kagenti-dev` cluster and install into it:

```bash
# Go to kagenti repo
cd kagenti

# Install with dev base values + TWO overlays (deps local images + extensions federated-jwt)
# --env dev                                → Loads dev_values.yaml (base Kind development config)
# --env-file deployments/envs/...         → Local images for kagenti-deps (SPIRE, Keycloak, etc.)
# --env-file ../kagenti-extensions/...    → Federated-jwt + local images for kagenti-extensions
deployments/ansible/run-install.sh --env dev \
  --env-file deployments/envs/dev_values_local_images.yaml \
  --env-file deployments/envs/dev_values_federated-jwt.yaml
```

**About the values files:**
- `dev_values.yaml`: Base Kind development configuration (components, Keycloak, domain, SPIRE config, openshift: false)
- `dev_values_local_images.yaml`: **Local testing overlay**:
  - Image tags: `:local` for spiffe-idp-setup, client-registration, envoy-proxy, proxy-init
  - Image pull policy: `Never`
  - Disables mcp-gateway component
  - Sets `create_kind_cluster: false` (assumes cluster already exists)
- `dev_values_federated-jwt.yaml`: **Federated-JWT authentication overlay** (production-ready):
  - Enables JWT-SVID authentication: `authBridge.clientAuthType: "federated-jwt"`
  - Sets SPIFFE IdP alias: `spiffeIdpAlias: "spire-spiffe"`
  - **No image overrides** (uses whatever tags are in base/other overlays)
- The installer **merges all three files** in order, each overlay adding/overriding specific values

This installation will:
1. Detect and use the existing `kagenti-dev` Kind cluster
2. Deploy kagenti-deps (Keycloak, SPIRE, etc.) via Helm
3. **Patch SPIRE ConfigMap** with `set_key_use: true` (workaround for SPIRE Helm chart bug)
4. **Create SPIFFE IdP setup job** (configures Keycloak with SPIFFE Identity Provider)
5. Deploy kagenti chart with `authBridge.clientAuthType=federated-jwt`
6. Use `:local` image tags for all components (pulled from the cluster's local image cache)

**How SPIFFE IdP setup works:**
- Ansible creates the job AFTER patching the SPIRE ConfigMap (avoids race condition)
- Job waits for SPIRE server and OIDC discovery provider to be ready
- Job validates JWKS has required "use" field
- Job configures Keycloak with SPIFFE Identity Provider named "spire-spiffe"
- Ansible waits for job completion before proceeding

**Expected behavior:**
- Installation typically completes in 6-8 minutes
- The SPIFFE IdP job should succeed on first attempt (no CrashLoopBackOff)
- All components should be running and ready

## Step 3: Verify SPIRE and Keycloak

```bash
cd kagenti-extensions

chmod +x verify-spire-keycloak.sh
./verify-spire-keycloak.sh
```

Expected output:
- ✅ SPIRE server is running
- ✅ SPIRE OIDC discovery provider is running
- ✅ SPIRE JWKS has 'use' field
- ✅ Keycloak is running
- ✅ Keycloak admin secret exists
- ✅ SPIFFE IdP setup job completed successfully

## Step 4: Test JWT-SVID Generation and Token Exchange

This comprehensive test verifies the complete federated-JWT authentication flow:
1. **JWT-SVID Generation**: SPIRE issues JWT-SVIDs to workloads
2. **Token Exchange**: JWT-SVIDs are exchanged for Keycloak access tokens
3. **Token Validation**: Access tokens contain correct realm and identity claims

### 4.1. Create Test Deployment

Deploy a pod with spiffe-helper and client-registration to automatically register with Keycloak:

```bash
# Create namespace
kubectl create namespace agent1

# Create Keycloak admin secret
# (Kagenti automatically creates this in agent namespaces listed in agentNamespaces,
#  but agent1 is a manual test namespace not in that list)
kubectl create secret generic keycloak-admin-secret -n agent1 \
  --from-literal=KEYCLOAK_ADMIN_USERNAME=admin \
  --from-literal=KEYCLOAK_ADMIN_PASSWORD=admin

# Deploy test pod with SPIFFE helper and client registration
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: spiffe-helper-config
  namespace: agent1
data:
  helper.conf: |
    agent_address = "/spiffe-workload-api/spire-agent.sock"
    cmd = "/bin/sh"
    cmd_args = "-c echo Updated"
    cert_dir = "/opt"
    svid_file_name = "svid.pem"
    svid_key_file_name = "svid_key.pem"
    svid_bundle_file_name = "svid_bundle.pem"
    jwt_svids = [{jwt_audience = "http://keycloak.localtest.me:8080/realms/kagenti", jwt_svid_file_name = "jwt_svid.token"}]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: spiffe-demo
  namespace: agent1
spec:
  replicas: 1
  selector:
    matchLabels:
      app: spiffe-demo
  template:
    metadata:
      labels:
        app: spiffe-demo
    spec:
      serviceAccountName: default
      securityContext:
        fsGroup: 1000
      containers:
      - name: spiffe-helper
        image: ghcr.io/spiffe/spiffe-helper:0.11.0
        args: ["-config", "/etc/spiffe-helper/helper.conf"]
        securityContext:
          runAsUser: 1000
          runAsGroup: 1000
        volumeMounts:
        - name: spiffe-helper-config
          mountPath: /etc/spiffe-helper
        - name: spiffe-workload-api
          mountPath: /spiffe-workload-api
          readOnly: true
        - name: certs
          mountPath: /opt
      - name: client-registration
        image: ghcr.io/kagenti/kagenti-extensions/client-registration:local
        imagePullPolicy: Never
        command:
          - /bin/sh
          - -c
          - |
            while [ ! -f /opt/jwt_svid.token ]; do
              echo "Waiting for JWT SVID..."
              sleep 1
            done
            echo "JWT SVID found, starting client registration"
            python client_registration.py
            echo "Client registration complete, keeping container alive"
            tail -f /dev/null
        env:
          - name: SPIRE_ENABLED
            value: "true"
          - name: CLIENT_NAME
            value: "spiffe-demo"
          - name: CLIENT_AUTH_TYPE
            value: "federated-jwt"
          - name: SPIFFE_IDP_ALIAS
            value: "spire-spiffe"
          - name: TOKEN_URL
            value: "http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token"
          - name: KEYCLOAK_ADMIN_USERNAME
            valueFrom:
              secretKeyRef:
                name: keycloak-admin-secret
                key: KEYCLOAK_ADMIN_USERNAME
          - name: KEYCLOAK_ADMIN_PASSWORD
            valueFrom:
              secretKeyRef:
                name: keycloak-admin-secret
                key: KEYCLOAK_ADMIN_PASSWORD
        securityContext:
          runAsUser: 1000
          runAsGroup: 1000
        volumeMounts:
        - name: certs
          mountPath: /opt
          readOnly: true
        - name: shared-data
          mountPath: /shared
      - name: tools
        image: curlimages/curl
        command: ["sleep", "infinity"]
        securityContext:
          runAsUser: 1000
          runAsGroup: 1000
        volumeMounts:
        - name: certs
          mountPath: /opt
          readOnly: true
        - name: shared-data
          mountPath: /shared
          readOnly: true
      volumes:
      - name: spiffe-helper-config
        configMap:
          name: spiffe-helper-config
      - name: spiffe-workload-api
        csi:
          driver: csi.spiffe.io
          readOnly: true
      - name: certs
        emptyDir: {}
      - name: shared-data
        emptyDir: {}
EOF

# Wait for pod to be ready
kubectl wait -n agent1 --for=condition=ready pod -l app=spiffe-demo --timeout=120s
```

### 4.2. Verify JWT-SVID Generation and Client Registration

Check that spiffe-helper successfully obtained a JWT-SVID and client-registration registered the client:

```bash
# Check spiffe-helper logs
kubectl logs -n agent1 deployment/spiffe-demo -c spiffe-helper | tail -10

# Check client-registration logs
kubectl logs -n agent1 deployment/spiffe-demo -c client-registration

# Verify JWT-SVID file exists
kubectl exec -n agent1 deployment/spiffe-demo -c tools -- ls -lh /opt/jwt_svid.token

# Read JWT-SVID (first 100 characters)
kubectl exec -n agent1 deployment/spiffe-demo -c tools -- sh -c 'head -c 100 /opt/jwt_svid.token && echo "..."'
```

**Expected output (spiffe-helper):**
- Logs should show: `Successfully wrote JWT SVID to file`
- File should exist with read permissions
- JWT should start with `eyJ...` (base64-encoded JWT header)

**Expected output (client-registration):**
```
JWT SVID found, starting client registration
Extracted SPIFFE ID: spiffe://localtest.me/ns/agent1/sa/default
Configuring client for JWT-SVID authentication (federated-jwt)
Created Keycloak client "spiffe://localtest.me/ns/agent1/sa/default": <uuid>
Client registration complete, keeping container alive
```

**What the JWT-SVID contains:**
- **Issuer**: SPIRE OIDC discovery provider
- **Subject**: `spiffe://localtest.me/ns/agent1/sa/default`
- **Audience**: `http://keycloak.localtest.me:8080/realms/kagenti`

### 4.3. Test Token Exchange (JWT-SVID → Access Token)

Exchange the JWT-SVID for a Keycloak access token using the OAuth 2.0 client credentials flow:

```bash
# Perform token exchange
kubectl exec -n agent1 deployment/spiffe-demo -c tools -- sh -c '
JWT_SVID=$(cat /opt/jwt_svid.token)
curl -s -X POST \
  -d "grant_type=client_credentials" \
  -d "client_id=spiffe://localtest.me/ns/agent1/sa/default" \
  -d "client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-spiffe" \
  -d "client_assertion=$JWT_SVID" \
  "http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token"
'
```

**Expected output (successful):**
```json
{
  "access_token": "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCIs...",
  "expires_in": 300,
  "refresh_expires_in": 0,
  "token_type": "Bearer",
  "not-before-policy": 0,
  "scope": "profile email"
}
```

**Error outputs to watch for:**

If you see:
```json
{"error":"invalid_client","error_description":"Invalid client or Invalid client credentials"}
```

**Troubleshoot:**
1. Wrong `client_assertion_type` (must be `jwt-spiffe` not `jwt-bearer`)
2. Client not configured with `federated-jwt` authenticator
3. SPIFFE Identity Provider missing in kagenti realm
4. JWT-SVID issuer doesn't match configured IdP alias

### 4.4. Validate Access Token Claims

Decode and verify the access token contains correct realm and identity information:

```bash
# Get access token
ACCESS_TOKEN=$(kubectl exec -n agent1 deployment/spiffe-demo -c tools -- sh -c '
JWT_SVID=$(cat /opt/jwt_svid.token)
curl -s -X POST \
  -d "grant_type=client_credentials" \
  -d "client_id=spiffe://localtest.me/ns/agent1/sa/default" \
  -d "client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-spiffe" \
  -d "client_assertion=$JWT_SVID" \
  "http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token"
' | grep -o '"access_token":"[^"]*"' | cut -d'"' -f4)

# Decode token payload (requires base64 and Python)
echo "$ACCESS_TOKEN" | cut -d. -f2 | python3 -c '
import sys, base64, json
payload = sys.stdin.read().strip()
padding = len(payload) % 4
if padding: payload += "=" * (4 - padding)
decoded = json.loads(base64.b64decode(payload))
print(json.dumps(decoded, indent=2))
' | jq '{iss, azp, client_id, realm_access}'
```

**Expected token claims:**
```json
{
  "iss": "http://keycloak.localtest.me:8080/realms/kagenti",
  "azp": "spiffe://localtest.me/ns/agent1/sa/default",
  "client_id": "spiffe://localtest.me/ns/agent1/sa/default",
  "realm_access": {
    "roles": [
      "offline_access",
      "default-roles-kagenti",
      "uma_authorization"
    ]
  }
}
```

**Key verification points:**
- ✅ `iss` (issuer) contains `/realms/kagenti` (not `/realms/demo`)
- ✅ `azp` and `client_id` match the SPIFFE ID
- ✅ `realm_access.roles` contains `default-roles-kagenti`

### What This Test Verifies

| Component | Verification |
|-----------|-------------|
| **SPIRE** | Issues JWT-SVIDs with correct audience and subject |
| **spiffe-helper** | Fetches JWT-SVIDs from SPIRE agent via CSI driver |
| **Keycloak IdP** | SPIFFE Identity Provider configured in kagenti realm |
| **Client Config** | Client uses `federated-jwt` authenticator with correct issuer/subject |
| **Token Exchange** | JWT-SVID successfully exchanges for access token using `jwt-spiffe` assertion type |
| **Access Token** | Contains correct realm (kagenti), client ID, and roles |

### Cleanup

```bash
kubectl delete namespace agent1
```

### Optional: Run Full AuthBridge Demo

For testing the complete AuthBridge flow with automatic sidecar injection:

**Manual Demo:** Follow [authbridge/demos/github-issue/demo-manual.md](authbridge/demos/github-issue/demo-manual.md)

**Token-Exchange Routes:** Follow [authbridge/demos/token-exchange-routes/README.md](authbridge/demos/token-exchange-routes/README.md)

---

## Appendix: Standalone Helm Install (Without Ansible)

If you want to install kagenti-deps directly with Helm instead of using the Ansible installer, you must manually configure SPIFFE IdP support due to a bug in the SPIRE Helm chart that prevents `set_key_use` from being rendered correctly.

### Step 1: Install kagenti-deps

```bash
helm install kagenti-deps ./charts/kagenti-deps/ \
  -n kagenti-system \
  --create-namespace \
  --set spire.enabled=true \
  --set keycloak.enabled=true \
  --wait
```

### Step 2: Patch SPIRE ConfigMap

The SPIRE Helm chart doesn't render `set_key_use: true` to the ConfigMap (even when set in values). This causes the JWKS to be missing the "use" field that Keycloak 26+ requires.

```bash
# Get the SPIRE namespace (may vary)
SPIRE_NAMESPACE=zero-trust-workload-identity-manager

# Patch the ConfigMap
kubectl get configmap spire-spiffe-oidc-discovery-provider \
  -n $SPIRE_NAMESPACE -o json | \
  jq '.data["oidc-discovery-provider.conf"] |= (fromjson | .set_key_use = true | tojson)' | \
  kubectl apply -f -

# Restart OIDC provider to apply changes
kubectl rollout restart deployment/spire-spiffe-oidc-discovery-provider -n $SPIRE_NAMESPACE

# Wait for rollout to complete
kubectl rollout status deployment/spire-spiffe-oidc-discovery-provider -n $SPIRE_NAMESPACE --timeout=2m
```

### Step 3: Create SPIFFE IdP Setup Job

The job is not included in the Helm chart to avoid race conditions. Create it manually after the patch:

```bash
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: ServiceAccount
metadata:
  name: kagenti-spiffe-idp-setup
  namespace: kagenti-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kagenti-spiffe-idp-reader
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    resourceNames: ["keycloak-initial-admin"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: kagenti-spiffe-idp-keycloak-reader
  namespace: keycloak
subjects:
  - kind: ServiceAccount
    name: kagenti-spiffe-idp-setup
    namespace: kagenti-system
roleRef:
  kind: ClusterRole
  name: kagenti-spiffe-idp-reader
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: batch/v1
kind: Job
metadata:
  name: kagenti-spiffe-idp-setup-job
  namespace: kagenti-system
spec:
  backoffLimit: 10
  template:
    spec:
      serviceAccountName: kagenti-spiffe-idp-setup
      restartPolicy: OnFailure
      initContainers:
        - name: wait-for-spire
          image: bitnami/kubectl:latest
          command:
            - sh
            - -c
            - |
              echo "Waiting for SPIRE server..."
              kubectl wait --for=condition=ready pod -l app=spire-server \
                -n zero-trust-workload-identity-manager --timeout=300s
              echo "Waiting for SPIRE OIDC provider..."
              kubectl wait --for=condition=ready pod \
                -l app.kubernetes.io/name=spire-spiffe-oidc-discovery-provider \
                -n zero-trust-workload-identity-manager --timeout=300s
      containers:
        - name: setup-spiffe-idp
          image: ghcr.io/kagenti/kagenti/spiffe-idp-setup:latest
          env:
            - name: KEYCLOAK_BASE_URL
              value: "http://keycloak-service.keycloak.svc:8080"
            - name: KEYCLOAK_REALM
              value: "kagenti"
            - name: KEYCLOAK_NAMESPACE
              value: "keycloak"
            - name: KEYCLOAK_ADMIN_SECRET_NAME
              value: "keycloak-initial-admin"
            - name: KEYCLOAK_ADMIN_USERNAME_KEY
              value: "username"
            - name: KEYCLOAK_ADMIN_PASSWORD_KEY
              value: "password"
            - name: SPIFFE_TRUST_DOMAIN
              value: "spiffe://localtest.me"
            - name: SPIFFE_BUNDLE_ENDPOINT
              value: "http://spire-spiffe-oidc-discovery-provider.zero-trust-workload-identity-manager.svc.cluster.local/keys"
            - name: SPIFFE_IDP_ALIAS
              value: "spire-spiffe"
EOF

# Wait for job to complete
kubectl wait --for=condition=complete job/kagenti-spiffe-idp-setup-job \
  -n kagenti-system --timeout=5m

# Check job logs
kubectl logs job/kagenti-spiffe-idp-setup-job -n kagenti-system
```

### Step 4: Verify

```bash
# Check job status
kubectl get job kagenti-spiffe-idp-setup-job -n kagenti-system

# Verify JWKS has "use" field
kubectl run test-curl --rm -i --image=curlimages/curl --restart=Never -- \
  curl -s http://spire-spiffe-oidc-discovery-provider.zero-trust-workload-identity-manager.svc.cluster.local/keys | \
  jq '.keys[] | select(.use)'

# Should return keys with "use": "sig"
```

**Why these manual steps are needed:**

1. **SPIRE Helm chart bug**: The chart doesn't render `set_key_use` from values.yaml to the ConfigMap
2. **Keycloak 26+ requirement**: Keycloak requires JWKS keys to have a "use" field for SPIFFE authentication
3. **Race condition avoidance**: The job must run AFTER the ConfigMap is patched, not as a Helm post-install hook

**Recommendation:** Use the Ansible installer (Step 3 in main guide) instead of standalone Helm - it handles all of this automatically!

## Troubleshooting

### Issue: ErrImageNeverPull for spiffe-idp-setup

**Symptom:**
```
kagenti-spiffe-idp-setup-job-xxxxx   0/1   ErrImageNeverPull   0   5m
```

**Root Cause:** The `spiffe-idp-setup:local` image wasn't built or loaded into Kind.

**Solution:**
1. Verify you ran `./local-build-and-test.sh` (not individual docker build commands)
2. Check if the image is loaded:
   ```bash
   kind get images --name kagenti-dev | grep spiffe-idp-setup
   ```
3. If missing, rebuild:
   ```bash
   cd /Users/alan/Documents/Work/kagenti/kagenti/auth/spiffe-idp-setup

   # For Docker:
   docker build -t ghcr.io/kagenti/kagenti/spiffe-idp-setup:local .
   kind load docker-image ghcr.io/kagenti/kagenti/spiffe-idp-setup:local --name kagenti-dev

   # For Podman:
   podman build -t ghcr.io/kagenti/kagenti/spiffe-idp-setup:local .
   podman save ghcr.io/kagenti/kagenti/spiffe-idp-setup:local -o /tmp/spiffe-idp.tar
   kind load image-archive /tmp/spiffe-idp.tar --name kagenti-dev
   rm /tmp/spiffe-idp.tar
   ```
4. Delete the failing pod:
   ```bash
   kubectl delete pod -n kagenti-system -l job-name=kagenti-spiffe-idp-setup-job
   ```

### Issue: SPIFFE IdP Job Init Container CrashLoopBackOff

**Symptom:**
```
kagenti-spiffe-idp-setup-job-xxxxx   0/1   Init:CrashLoopBackOff   3   2m
```

**Root Cause:** Missing RBAC permissions to list pods in keycloak or SPIRE namespaces.

**Check:**
```bash
kubectl logs -n kagenti-system -l job-name=kagenti-spiffe-idp-setup-job -c wait-for-spire
```

**Expected error:**
```
Error from server (Forbidden): pods is forbidden: User "system:serviceaccount:kagenti-system:kagenti-spiffe-idp-setup"
cannot list resource "pods" in API group "" in the namespace "keycloak"
```

**Solution:** This should be automatically created by the Ansible installer. If missing, manually create RBAC:
```bash
# For keycloak namespace
kubectl create role kagenti-spiffe-idp-pod-reader \
  -n keycloak \
  --verb=get,list,watch \
  --resource=pods

kubectl create rolebinding kagenti-spiffe-idp-pod-reader \
  -n keycloak \
  --role=kagenti-spiffe-idp-pod-reader \
  --serviceaccount=kagenti-system:kagenti-spiffe-idp-setup
```

### Issue: Token Exchange Fails with "invalid_client"

**Symptom:**
```json
{"error":"invalid_client","error_description":"Invalid client or Invalid client credentials"}
```

**Root Causes & Solutions:**

1. **Wrong client_assertion_type**
   - ❌ DON'T use: `urn:ietf:params:oauth:client-assertion-type:jwt-bearer`
   - ✅ DO use: `urn:ietf:params:oauth:client-assertion-type:jwt-spiffe`

2. **Client not configured for federated-jwt**
   - Check client authenticator type:
     ```bash
     kubectl exec -n keycloak keycloak-0 -- sh -c \
       '/opt/keycloak/bin/kcadm.sh config credentials --server http://localhost:8080 --realm master --user admin --password admin && \
        /opt/keycloak/bin/kcadm.sh get clients -r kagenti -q clientId="spiffe://localtest.me/ns/agent1/sa/default"' | \
       jq '.[] | {clientAuthenticatorType, attributes}'
     ```
   - Should show:
     ```json
     {
       "clientAuthenticatorType": "federated-jwt",
       "attributes": {
         "jwt.credential.issuer": "spire-spiffe",
         "jwt.credential.sub": "spiffe://localtest.me/ns/agent1/sa/default"
       }
     }
     ```

3. **SPIFFE Identity Provider missing**
   - Check IdP exists:
     ```bash
     kubectl exec -n keycloak keycloak-0 -- sh -c \
       '/opt/keycloak/bin/kcadm.sh config credentials --server http://localhost:8080 --realm master --user admin --password admin && \
        /opt/keycloak/bin/kcadm.sh get identity-provider/instances -r kagenti'
     ```
   - Should show IdP with alias "spire-spiffe"

### Issue: Images Not Found After Rebuild

**Symptom:** After making code changes and rebuilding, the old images are still used.

**Solution:**
1. Delete pods to force recreation:
   ```bash
   kubectl delete pod -n <namespace> <pod-name>
   ```
2. For webhook changes, see [kagenti-operator](https://github.com/kagenti/kagenti-operator) for redeployment instructions.
3. Verify new images are loaded:
   ```bash
   kind get images --name kagenti-dev | grep :local
   ```

## Verify Federated-JWT Configuration

Since you installed with `dev_values_federated-jwt.yaml`, the system should already be configured for JWT-SVID authentication:

```bash
# 1. Verify authBridge.clientAuthType is set to federated-jwt
kubectl get configmap authbridge-config -n <namespace> -o jsonpath='{.data.CLIENT_AUTH_TYPE}'
# Expected: federated-jwt

# 2. Deploy an agent and check client-registration logs
# (After deploying an agent in Step 4)
kubectl logs -n <namespace> deployment/<your-agent> -c kagenti-client-registration -f
# Expected: "Configuring client for JWT-SVID authentication (federated-jwt)"

# 3. Verify Keycloak client uses federated-jwt authenticator
# (After agent deployment creates a Keycloak client)
kubectl run test-curl --rm -i --image=curlimages/curl --restart=Never -- sh -c "
  ADMIN_TOKEN=\$(curl -s 'http://keycloak-service.keycloak.svc:8080/realms/master/protocol/openid-connect/token' \
    -d 'grant_type=password' -d 'client_id=admin-cli' -d 'username=admin' -d 'password=admin' | jq -r '.access_token')
  curl -s -H 'Authorization: Bearer \$ADMIN_TOKEN' \
    'http://keycloak-service.keycloak.svc:8080/admin/realms/kagenti/clients' | \
    jq '.[] | select(.clientId | contains(\"spiffe\")) | {clientId, clientAuthenticatorType}'
"
# Expected: "clientAuthenticatorType": "federated-jwt"
```
