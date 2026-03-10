#!/bin/bash
set -e

# Local Build and Test Script for JWT-SVID Authentication
# This script builds all necessary images locally and loads them into Kind

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KAGENTI_DIR="/Users/alan/Documents/Work/kagenti"
CLUSTER_NAME="${CLUSTER_NAME:-kagenti-dev}"

# Auto-detect container runtime (Podman or Docker)
# If KIND_EXPERIMENTAL_PROVIDER is set to podman, use it regardless of what's installed
if [ "${KIND_EXPERIMENTAL_PROVIDER}" = "podman" ]; then
    CONTAINER_RUNTIME="${CONTAINER_RUNTIME:-podman}"
elif ! command -v docker &> /dev/null && command -v podman &> /dev/null; then
    # Docker not available but Podman is
    CONTAINER_RUNTIME="${CONTAINER_RUNTIME:-podman}"
else
    CONTAINER_RUNTIME="${CONTAINER_RUNTIME:-docker}"
fi

echo "Using container runtime: ${CONTAINER_RUNTIME}"

# Function to load image into Kind (handles Podman vs Docker)
load_image_to_kind() {
    local image_name="$1"
    if [ "${CONTAINER_RUNTIME}" = "podman" ]; then
        # Podman: save to tar and load
        # Replace colons and slashes to make valid filename
        local tar_file="/tmp/$(echo ${image_name} | sed 's|[:/]|-|g').tar"
        ${CONTAINER_RUNTIME} save "${image_name}" -o "${tar_file}"
        kind load image-archive "${tar_file}" --name "${CLUSTER_NAME}"
        rm -f "${tar_file}"
    else
        # Docker: direct load
        kind load docker-image "${image_name}" --name "${CLUSTER_NAME}"
    fi
}

echo "=========================================="
echo "Building Local Images for JWT-SVID Testing"
echo "Cluster: ${CLUSTER_NAME}"
echo "=========================================="

# Check if cluster exists
if ! kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
    echo "❌ Kind cluster '${CLUSTER_NAME}' not found"
    echo "Please create it first or set CLUSTER_NAME environment variable"
    exit 1
fi

echo "✅ Found Kind cluster: ${CLUSTER_NAME}"
echo ""

# Build spiffe-idp-setup (NEW - from kagenti repo)
echo "=========================================="
echo "Building spiffe-idp-setup"
echo "=========================================="
cd "${KAGENTI_DIR}/kagenti/auth/spiffe-idp-setup"
${CONTAINER_RUNTIME} build -t ghcr.io/kagenti/kagenti/spiffe-idp-setup:local .
load_image_to_kind ghcr.io/kagenti/kagenti/spiffe-idp-setup:local
echo "✅ Built and loaded: spiffe-idp-setup:local"
echo ""

# Build client-registration (CHANGED - UID 1337)
echo "=========================================="
echo "Building client-registration"
echo "=========================================="
cd "${SCRIPT_DIR}/AuthBridge/client-registration"
${CONTAINER_RUNTIME} build -t ghcr.io/kagenti/kagenti-extensions/client-registration:local .
load_image_to_kind ghcr.io/kagenti/kagenti-extensions/client-registration:local
echo "✅ Built and loaded: client-registration:local"
echo ""

# Build kagenti-webhook (CHANGED - container_builder.go)
echo "=========================================="
echo "Building kagenti-webhook"
echo "=========================================="
cd "${SCRIPT_DIR}/kagenti-webhook"
make docker-build IMG=ghcr.io/kagenti/kagenti-extensions/kagenti-webhook:local CONTAINER_TOOL=${CONTAINER_RUNTIME}
load_image_to_kind ghcr.io/kagenti/kagenti-extensions/kagenti-webhook:local
echo "✅ Built and loaded: kagenti-webhook:local"
echo ""

# Build envoy-with-processor (verify UID 1337)
echo "=========================================="
echo "Building envoy-with-processor"
echo "=========================================="
cd "${SCRIPT_DIR}/AuthBridge/AuthProxy"
${CONTAINER_RUNTIME} build -f Dockerfile.envoy -t ghcr.io/kagenti/kagenti-extensions/envoy-with-processor:local .
load_image_to_kind ghcr.io/kagenti/kagenti-extensions/envoy-with-processor:local
echo "✅ Built and loaded: envoy-with-processor:local"
echo ""

# Build proxy-init
echo "=========================================="
echo "Building proxy-init"
echo "=========================================="
cd "${SCRIPT_DIR}/AuthBridge/AuthProxy"
${CONTAINER_RUNTIME} build -f Dockerfile.init -t ghcr.io/kagenti/kagenti-extensions/proxy-init:local .
load_image_to_kind ghcr.io/kagenti/kagenti-extensions/proxy-init:local
echo "✅ Built and loaded: proxy-init:local"
echo ""

echo "=========================================="
echo "✅ All images built and loaded successfully!"
echo "=========================================="
echo ""
echo "Images loaded into cluster '${CLUSTER_NAME}':"
echo "  - ghcr.io/kagenti/kagenti/spiffe-idp-setup:local"
echo "  - ghcr.io/kagenti/kagenti-extensions/client-registration:local"
echo "  - ghcr.io/kagenti/kagenti-extensions/kagenti-webhook:local"
echo "  - ghcr.io/kagenti/kagenti-extensions/envoy-with-processor:local"
echo "  - ghcr.io/kagenti/kagenti-extensions/proxy-init:local"
echo ""
echo "Next steps:"
echo "  1. Update values files to use :local tag"
echo "  2. Run: cd ${KAGENTI_DIR} && deployments/ansible/run-install.sh --env dev"
echo "  3. Verify SPIRE and Keycloak are running"
echo "  4. Run the AuthBridge demo"
echo ""
