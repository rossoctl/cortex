# Local Testing Guide for JWT-SVID Authentication

This guide walks you through testing JWT-SVID authentication using local images (no push to ghcr.io).

## Prerequisites

- Docker or Podman running
- Kind CLI installed

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

```bash
cd kagenti-extensions

# Make the script executable
chmod +x local-build-and-test.sh

# Build all images and load into Kind cluster
# For Podman users: set KIND_EXPERIMENTAL_PROVIDER
export KIND_EXPERIMENTAL_PROVIDER=podman  # Only needed for Podman
./local-build-and-test.sh

# If using a different cluster name:
# CLUSTER_NAME=my-cluster ./local-build-and-test.sh
```

This builds and loads:
- `spiffe-idp-setup:local`
- `client-registration:local` (with UID 1337)
- `kagenti-webhook:local` (with updated container_builder.go)
- `envoy-with-processor:local`
- `proxy-init:local`

**Note for Podman users:** The script automatically detects Podman and uses tar archives to load images into Kind, since `kind load docker-image` doesn't work with Podman's image store.

## Step 2: Update Hardcoded Image Tags

The webhook's default image tags are hardcoded in [`internal/webhook/config/defaults.go`](kagenti-webhook/internal/webhook/config/defaults.go:12-15). Update them to use `:local`:

```bash
cd kagenti-extensions/kagenti-webhook

# Replace image tags (these sed commands modify defaults.go in-place)
sed -i '' 's|envoy-with-processor:latest|envoy-with-processor:local|g' internal/webhook/config/defaults.go
sed -i '' 's|proxy-init:latest|proxy-init:local|g' internal/webhook/config/defaults.go
sed -i '' 's|client-registration:latest|client-registration:local|g' internal/webhook/config/defaults.go

# Also update PullPolicy to Never (don't pull from registry)
sed -i '' 's|PullPolicy:.*corev1.PullIfNotPresent|PullPolicy:         corev1.PullNever|g' internal/webhook/config/defaults.go

# Verify changes
grep -E "envoy-with-processor|proxy-init|client-registration|PullPolicy" internal/webhook/config/defaults.go

# Rebuild the webhook image with updated tags
make docker-build IMG=ghcr.io/kagenti/kagenti-extensions/kagenti-webhook:local

# Load into Kind cluster
# For Docker users:
kind load docker-image ghcr.io/kagenti/kagenti-extensions/kagenti-webhook:local --name kagenti-dev

# For Podman users (use tar archive method):
# podman save ghcr.io/kagenti/kagenti-extensions/kagenti-webhook:local -o /tmp/webhook-local.tar
# kind load image-archive /tmp/webhook-local.tar --name kagenti-dev
# rm /tmp/webhook-local.tar
```

## Step 3: Install Kagenti with Ansible

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
  --env-file ../kagenti-extensions/federated-jwt-values.yaml
```

**About the values files:**
- `dev_values.yaml`: Base Kind development configuration (components, Keycloak, domain, basic SPIRE config, openshift: false)
- `dev_values_local_images.yaml`: **Testing-only** local image overrides for kagenti-deps (tag: local, pullPolicy: Never)
- `federated-jwt-values.yaml`: **Testing + federated-jwt overlay** for kagenti-extensions:
  - Cluster name: `kagenti-dev`
  - SPIRE enabled with correct namespace (`zero-trust-workload-identity-manager`)
  - JWT-SVID authentication (`authBridge.clientAuthType: federated-jwt`)
  - Local image tags (`:local`) for kagenti-extensions components
  - Keycloak `kagenti` realm configuration
  - Agent namespace (`team1`)
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

## Step 4: Verify SPIRE and Keycloak

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

## Step 5: Run AuthBridge Demo

### Option A: Manual Demo (Recommended First)

Follow [AuthBridge/demos/github-issue/demo-manual.md](AuthBridge/demos/github-issue/demo-manual.md):

```bash
cd /Users/alan/Documents/Work/kagenti-extensions/AuthBridge/demos/github-issue

# 1. Create namespace
kubectl create namespace team1
kubectl label namespace team1 kagenti-enabled=true

# 2. Deploy ConfigMaps (use the updated ones with kagenti-webhook-config)
kubectl apply -f k8s/configmaps.yaml

# 3. Deploy demo workload
kubectl apply -f k8s/deployment.yaml

# 4. Check client-registration sidecar logs
kubectl logs -n team1 deployment/weather-service -c kagenti-client-registration -f

# Expected (Phase 1 - client-secret):
# "Configuring client for client-secret authentication"

# Expected (Phase 2 - federated-jwt):
# "Configuring client for JWT-SVID authentication (federated-jwt)"
```

### Option B: Webhook-Based Demo

Follow [AuthBridge/demos/webhook/README.md](AuthBridge/demos/webhook/README.md) for automatic sidecar injection testing.

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
    resourceNames: ["keycloak-admin-secret"]
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
              value: "keycloak-admin-secret"
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

## Verify Federated-JWT Configuration

Since you installed with `dev_values_federated-jwt.yaml`, the system should already be configured for JWT-SVID authentication:

```bash
# 1. Verify authBridge.clientAuthType is set to federated-jwt
kubectl get configmap kagenti-webhook-config -n team1 -o jsonpath='{.data.CLIENT_AUTH_TYPE}'
# Expected: federated-jwt

# 2. Deploy an agent and check client-registration logs
# (After deploying an agent in Step 6)
kubectl logs -n team1 deployment/<your-agent> -c kagenti-client-registration -f
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