#!/usr/bin/env bash
#
# Teardown script for the multi-target demo.
# Deletes k8s resources and optionally the Keycloak demo realm.
#

set -eu

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
K8S_DIR="${SCRIPT_DIR}/k8s"

KEYCLOAK_URL="${KEYCLOAK_URL:-http://keycloak.localtest.me:8080}"
KEYCLOAK_ADMIN_USER="${KEYCLOAK_ADMIN_USER:-admin}"
KEYCLOAK_ADMIN_PASSWORD="${KEYCLOAK_ADMIN_PASSWORD:-admin}"
REALM="${REALM:-kagenti}"

echo "=== Multi-Target Demo Teardown ==="
echo ""

# Delete k8s resources
echo "Deleting Kubernetes resources..."
kubectl delete -f "${K8S_DIR}/targets.yaml" --ignore-not-found=true 2>/dev/null || true
kubectl delete -f "${K8S_DIR}/authbridge-deployment.yaml" --ignore-not-found=true 2>/dev/null || true
kubectl delete -f "${K8S_DIR}/authbridge-deployment-no-spiffe.yaml" --ignore-not-found=true 2>/dev/null || true
echo "Kubernetes resources deleted."
echo ""

# Delete Keycloak realm
echo "Deleting Keycloak realm '${REALM}' from ${KEYCLOAK_URL}..."
echo ""

# Get admin token
TOKEN_RESPONSE=$(curl -s -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -d "grant_type=password" \
  -d "client_id=admin-cli" \
  -d "username=${KEYCLOAK_ADMIN_USER}" \
  -d "password=${KEYCLOAK_ADMIN_PASSWORD}" 2>/dev/null) || {
    echo "Warning: Could not connect to Keycloak at ${KEYCLOAK_URL}"
    echo "Make sure Keycloak is port-forwarded and try again."
    exit 0
}

ACCESS_TOKEN=$(echo "${TOKEN_RESPONSE}" | jq -r '.access_token' 2>/dev/null)

if [ "${ACCESS_TOKEN}" = "null" ] || [ -z "${ACCESS_TOKEN}" ]; then
    echo "Warning: Could not authenticate to Keycloak."
    echo "Response: ${TOKEN_RESPONSE}"
    exit 1
fi

# Delete the realm
DELETE_RESPONSE=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE \
  "${KEYCLOAK_URL}/admin/realms/${REALM}" \
  -H "Authorization: Bearer ${ACCESS_TOKEN}")

case "${DELETE_RESPONSE}" in
    204)
        echo "Realm '${REALM}' deleted successfully."
        ;;
    404)
        echo "Realm '${REALM}' does not exist (already deleted)."
        ;;
    *)
        echo "Warning: Unexpected response when deleting realm: HTTP ${DELETE_RESPONSE}"
        ;;
esac

echo ""
echo "=== Teardown Complete ==="
