#!/bin/bash
set -euo pipefail

# Local Build and Test Script for JWT-SVID Authentication
# This script builds all necessary images locally and loads them into Kind

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KAGENTI_DIR="${KAGENTI_DIR:-$(cd "$SCRIPT_DIR/../kagenti" 2>/dev/null && pwd || echo "")}"
if [ -z "$KAGENTI_DIR" ] || [ ! -d "$KAGENTI_DIR" ]; then
    echo "ERROR: Set KAGENTI_DIR to point to your kagenti repo clone"
    exit 1
fi
CLUSTER_NAME="${CLUSTER_NAME:-kagenti-dev}"

# Auto-detect container runtime (Podman or Docker)
# If KIND_EXPERIMENTAL_PROVIDER is set to podman, use it regardless of what's installed
if [ "${KIND_EXPERIMENTAL_PROVIDER:-}" = "podman" ]; then
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
        local tar_file="/tmp/$(echo "${image_name}" | sed 's|[:/]|-|g').tar"
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

# Build authbridge (proxy-sidecar combined: authbridge-proxy + spiffe-helper)
# Default deployment shape — used when the workload's mode is proxy-sidecar.
# After kagenti-extensions#411 the unified binary was split into three
# mode-specific binaries; each has its own Dockerfile under cmd/authbridge-*/.
echo "=========================================="
echo "Building authbridge (proxy-sidecar combined)"
echo "=========================================="
cd "${SCRIPT_DIR}/authbridge"
${CONTAINER_RUNTIME} build -f cmd/authbridge-proxy/Dockerfile -t ghcr.io/kagenti/kagenti-extensions/authbridge:local .
load_image_to_kind ghcr.io/kagenti/kagenti-extensions/authbridge:local
echo "✅ Built and loaded: authbridge:local"
echo ""

# Build authbridge-envoy (envoy-sidecar combined: Envoy + ext_proc + spiffe-helper)
echo "=========================================="
echo "Building authbridge-envoy (envoy-sidecar combined)"
echo "=========================================="
cd "${SCRIPT_DIR}/authbridge"
${CONTAINER_RUNTIME} build -f cmd/authbridge-envoy/Dockerfile -t ghcr.io/kagenti/kagenti-extensions/authbridge-envoy:local .
load_image_to_kind ghcr.io/kagenti/kagenti-extensions/authbridge-envoy:local
echo "✅ Built and loaded: authbridge-envoy:local"
echo ""

# Build authbridge-lite: the same authbridge-proxy binary/Dockerfile built
# with exclude_plugin_* tags so only jwt-validation + token-exchange compile
# in (drops the OPA SDK + parsers). A build variant, not a separate binary.
echo "=========================================="
echo "Building authbridge-lite (proxy build variant: auth-only plugins)"
echo "=========================================="
cd "${SCRIPT_DIR}/authbridge"
${CONTAINER_RUNTIME} build -f cmd/authbridge-proxy/Dockerfile \
  --build-arg GO_BUILD_TAGS="exclude_plugin_a2aparser,exclude_plugin_ibac,exclude_plugin_inferenceparser,exclude_plugin_mcpparser,exclude_plugin_opa,exclude_plugin_sparc,exclude_plugin_tokenbroker" \
  -t ghcr.io/kagenti/kagenti-extensions/authbridge-lite:local .
load_image_to_kind ghcr.io/kagenti/kagenti-extensions/authbridge-lite:local
echo "✅ Built and loaded: authbridge-lite:local"
echo ""

# Build proxy-init (iptables init container, used by envoy-sidecar mode only)
echo "=========================================="
echo "Building proxy-init"
echo "=========================================="
cd "${SCRIPT_DIR}/authbridge/proxy-init"
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
echo "  - ghcr.io/kagenti/kagenti-extensions/authbridge:local"
echo "  - ghcr.io/kagenti/kagenti-extensions/authbridge-envoy:local"
echo "  - ghcr.io/kagenti/kagenti-extensions/authbridge-lite:local"
echo "  - ghcr.io/kagenti/kagenti-extensions/proxy-init:local"
echo ""
echo "Next steps:"
echo "  1. Update values files to use :local tag"
echo "  2. Run: cd ${KAGENTI_DIR} && deployments/ansible/run-install.sh --env dev"
echo "  3. Verify SPIRE and Keycloak are running"
echo "  4. Run the AuthBridge demo"
echo ""
