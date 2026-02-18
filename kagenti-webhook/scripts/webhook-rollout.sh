#!/bin/bash
set -euo pipefail

# ==========================================
# Validation Functions
# ==========================================

# Validate Kubernetes namespace/name format (DNS-1123 label)
# Must be lowercase alphanumeric or '-', start with alphanumeric, max 63 chars
validate_k8s_name() {
    local name="$1"
    local label="$2"
    if [[ ! "$name" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]]; then
        echo "Error: Invalid ${label}: '${name}'" >&2
        echo "Must be a valid DNS-1123 label: lowercase alphanumeric or '-'," >&2
        echo "must start and end with alphanumeric, max 63 characters." >&2
        exit 1
    fi
}

# ==========================================
# Container Runtime Detection
# ==========================================
detect_impl() {
    # Allow explicit override via environment variable
    if [ -n "${DOCKER_IMPL-}" ]; then
        printf '%s\n' "${DOCKER_IMPL}"
        return
    fi

    # Try podman first if present
    if command -v podman >/dev/null 2>&1; then
        out=$(podman info 2>/dev/null || true)
        if printf '%s' "$out" | grep -Ei 'apiversion|buildorigin|libpod|podman|version:' >/dev/null 2>&1; then
            printf 'podman\n'
            return
        fi
    fi

    # Try docker
    if command -v docker >/dev/null 2>&1; then
        out=$(docker info 2>/dev/null || true)
        # If docker info looks like Docker Engine, classify as docker
        if printf '%s' "$out" | grep -Ei 'client: docker engine|docker engine - community|server:' >/dev/null 2>&1; then
            printf 'docker\n'
            return
        fi
        # If docker info contains podman/libpod markers, it's actually Podman (symlink case)
        if printf '%s' "$out" | grep -Ei 'apiversion|buildorigin|libpod|podman|version:' >/dev/null 2>&1; then
            printf 'podman\n'
            return
        fi
    fi

    printf 'unknown\n'
}

# ==========================================
# Configuration
# ==========================================
CLUSTER=${CLUSTER:-kagenti}
NAMESPACE=${NAMESPACE:-kagenti-webhook-system}
TAG=$(date +%Y%m%d%H%M%S)

# Detect container runtime
DETECTED=$(detect_impl)

# Set image name based on detected runtime
if [ "${DETECTED}" = "podman" ]; then
    IMAGE_NAME="localhost/kagenti-webhook:${TAG}"
else
    IMAGE_NAME="local/kagenti-webhook:${TAG}"
fi

# AuthBridge demo configuration
AUTHBRIDGE_DEMO=${AUTHBRIDGE_DEMO:-false}
AUTHBRIDGE_NAMESPACE=${AUTHBRIDGE_NAMESPACE:-team1}
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Override AUTHBRIDGE_K8S_DIR to point at a different demo's k8s manifests.
# Defaults to the single-target demo. For the github-issue demo, run:
#   AUTHBRIDGE_K8S_DIR=AuthBridge/demos/github-issue/k8s ./scripts/webhook-rollout.sh
AUTHBRIDGE_K8S_DIR="${AUTHBRIDGE_K8S_DIR:-${SCRIPT_DIR}/../../AuthBridge/demos/single-target/k8s}"

# ==========================================
# Input Validation
# ==========================================
validate_k8s_name "$CLUSTER" "cluster name"
validate_k8s_name "$NAMESPACE" "namespace"
if [ "${AUTHBRIDGE_DEMO}" = "true" ]; then
    validate_k8s_name "$AUTHBRIDGE_NAMESPACE" "AuthBridge namespace"
    # Resolve relative paths against the repo root (two levels up from scripts/)
    if [[ ! "${AUTHBRIDGE_K8S_DIR}" = /* ]]; then
        AUTHBRIDGE_K8S_DIR="${SCRIPT_DIR}/../../${AUTHBRIDGE_K8S_DIR}"
    fi
    if [ ! -d "${AUTHBRIDGE_K8S_DIR}" ]; then
        echo "Error: AuthBridge k8s directory not found at '${AUTHBRIDGE_K8S_DIR}'." >&2
        echo "Set AUTHBRIDGE_K8S_DIR to the correct path. Available demos:" >&2
        ls -d "${SCRIPT_DIR}"/../../AuthBridge/demos/*/k8s 2>/dev/null | sed 's|.*/AuthBridge/|  AuthBridge/|' >&2 || true
        exit 1
    fi
fi

# ==========================================
# Deployment
# ==========================================
echo "=========================================="
echo "Full Webhook Deployment"
echo "=========================================="
echo "Cluster: ${CLUSTER}"
echo "Namespace: ${NAMESPACE}"
echo "Container runtime: ${DETECTED}"
echo "Image: ${IMAGE_NAME}"
if [ "${AUTHBRIDGE_DEMO}" = "true" ]; then
    echo "AuthBridge Demo: ${AUTHBRIDGE_DEMO}"
    echo "AuthBridge Namespace: ${AUTHBRIDGE_NAMESPACE}"
    echo "AuthBridge K8s Dir: ${AUTHBRIDGE_K8S_DIR}"
fi
echo ""

# Step 1: Build and load image
echo "[1/4] Building Docker image..."
docker build -f Dockerfile . --tag "${IMAGE_NAME}" --load

echo ""
echo "[2/4] Loading image into kind cluster..."
if ! kind load docker-image --name "${CLUSTER}" "${IMAGE_NAME}" 2>/dev/null; then
    echo "kind load failed, using docker save workaround..."
    docker save "${IMAGE_NAME}" | docker exec -i "${CLUSTER}-control-plane" ctr --namespace k8s.io images import -
fi

# Step 2: Update deployment
echo ""
echo "[3/4] Updating deployment..."
kubectl -n "${NAMESPACE}" set image deployment/kagenti-webhook-controller-manager "manager=${IMAGE_NAME}"

echo ""
echo "Waiting for rollout to complete..."
kubectl rollout status -n "${NAMESPACE}" deployment/kagenti-webhook-controller-manager --timeout=120s

# Step 3: Deploy authbridge webhook if it doesn't exist
echo ""
echo "[4/4] Ensuring authbridge webhook configuration exists..."
if kubectl get mutatingwebhookconfigurations kagenti-webhook-authbridge-mutating-webhook-configuration &>/dev/null; then
    echo "Authbridge webhook already exists, skipping..."
else
    echo "Creating authbridge webhook configuration..."
    kubectl apply -f - <<EOF
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: kagenti-webhook-authbridge-mutating-webhook-configuration
  annotations:
    cert-manager.io/inject-ca-from: ${NAMESPACE}/kagenti-webhook-serving-cert
webhooks:
- name: inject.kagenti.io
  admissionReviewVersions:
  - v1
  clientConfig:
    service:
      name: kagenti-webhook-webhook-service
      namespace: ${NAMESPACE}
      path: /mutate-workloads-authbridge
      port: 443
  failurePolicy: Fail
  timeoutSeconds: 10
  sideEffects: None
  namespaceSelector:
    matchExpressions:
      # Exclude kube-system and other critical namespaces
      - key: kubernetes.io/metadata.name
        operator: NotIn
        values:
          - kube-system
          - kube-public
          - kube-node-lease
          - ${NAMESPACE}
    matchLabels:
      # Only trigger webhook for namespaces that have opted-in
      # This aligns with NeedsMutation() which requires kagenti-enabled: true
      kagenti-enabled: "true"
  rules:
  - operations:
    - CREATE
    - UPDATE
    apiGroups:
    - apps
    apiVersions:
    - v1
    resources:
    - deployments
    - statefulsets
    - daemonsets
  - operations:
    - CREATE
    - UPDATE
    apiGroups:
    - batch
    apiVersions:
    - v1
    resources:
    - jobs
    - cronjobs
EOF
    echo "Waiting for cert-manager to inject CA bundle..."
    sleep 5
fi

echo ""
echo "=========================================="
echo "Deployment Complete!"
echo "=========================================="
echo ""
echo "Current pods:"
kubectl get -n "${NAMESPACE}" pod -l control-plane=controller-manager
echo ""
echo "Webhook configurations:"
kubectl get mutatingwebhookconfigurations | grep kagenti-webhook || true

# Optional: Setup AuthBridge demo prerequisites (namespace + ConfigMaps only)
if [ "${AUTHBRIDGE_DEMO}" = "true" ]; then
    echo ""
    echo "=========================================="
    echo "Setting up AuthBridge Demo Prerequisites"
    echo "=========================================="

    # Ensure namespace exists with required label
    echo ""
    echo "[AuthBridge 1/2] Creating namespace ${AUTHBRIDGE_NAMESPACE}..."
    kubectl create namespace "${AUTHBRIDGE_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    kubectl label namespace "${AUTHBRIDGE_NAMESPACE}" kagenti-enabled=true --overwrite

    # Apply ConfigMaps (update namespace in-place)
    # Note: AUTHBRIDGE_NAMESPACE is validated above to be a safe DNS-1123 label,
    # so it cannot contain sed metacharacters like '/' or '&'
    echo ""
    echo "[AuthBridge 2/2] Applying ConfigMaps..."
    if [ -f "${AUTHBRIDGE_K8S_DIR}/configmaps-webhook.yaml" ]; then
        sed "s/namespace: team1/namespace: ${AUTHBRIDGE_NAMESPACE}/g" \
            "${AUTHBRIDGE_K8S_DIR}/configmaps-webhook.yaml" | kubectl apply -f -
    else
        echo "Warning: ${AUTHBRIDGE_K8S_DIR}/configmaps-webhook.yaml not found"
    fi

    echo ""
    echo "=========================================="
    echo "AuthBridge Prerequisites Ready!"
    echo "=========================================="
    echo ""
    echo "See AuthBridge/demo-webhook.md for next steps"
fi

echo ""
echo "To view logs:"
echo "  kubectl logs -n ${NAMESPACE} -l control-plane=controller-manager -f"
echo ""
echo "Usage examples:"
echo "  ./scripts/webhook-rollout.sh"
echo "  DOCKER_IMPL=podman ./scripts/webhook-rollout.sh"
echo "  AUTHBRIDGE_DEMO=true ./scripts/webhook-rollout.sh"
echo "  AUTHBRIDGE_DEMO=true AUTHBRIDGE_NAMESPACE=myns ./scripts/webhook-rollout.sh"
echo "  AUTHBRIDGE_DEMO=true AUTHBRIDGE_K8S_DIR=AuthBridge/demos/github-issue/k8s ./scripts/webhook-rollout.sh"
echo ""
