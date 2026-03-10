#!/bin/bash
set -e

# Verification script for SPIRE and Keycloak setup
echo "=========================================="
echo "Verifying SPIRE and Keycloak Setup"
echo "=========================================="
echo ""

# Check SPIRE
echo "1. Checking SPIRE server..."
if kubectl get pods -n zero-trust-workload-identity-manager -l app.kubernetes.io/name=server | grep -q "Running"; then
    echo "✅ SPIRE server is running"
else
    echo "❌ SPIRE server is not running"
    kubectl get pods -n zero-trust-workload-identity-manager
    exit 1
fi
echo ""

# Check SPIRE OIDC discovery provider
echo "2. Checking SPIRE OIDC discovery provider..."
if kubectl get pods -n zero-trust-workload-identity-manager -l app.kubernetes.io/name=spiffe-oidc-discovery-provider | grep -q "Running"; then
    echo "✅ SPIRE OIDC discovery provider is running"
else
    echo "❌ SPIRE OIDC discovery provider is not running"
    kubectl get pods -n zero-trust-workload-identity-manager
    exit 1
fi
echo ""

# Check SPIRE JWKS endpoint
echo "3. Checking SPIRE JWKS endpoint..."
JWKS_URL="http://spire-spiffe-oidc-discovery-provider.zero-trust-workload-identity-manager.svc.cluster.local/keys"
echo "Testing URL: ${JWKS_URL}"

# Clean up any existing test pod
kubectl delete pod curl-test 2>/dev/null || true

# Run curl test (without --rm since we delete it manually)
kubectl run curl-test --restart=Never --image=curlimages/curl:latest -- curl -s "${JWKS_URL}" >/dev/null 2>&1
sleep 2
JWKS_OUTPUT=$(kubectl logs curl-test 2>&1 || echo "Failed to get logs")
kubectl delete pod curl-test 2>/dev/null || true

if echo "${JWKS_OUTPUT}" | grep -q '"use"'; then
    echo "✅ SPIRE JWKS has 'use' field"
else
    echo "❌ SPIRE JWKS missing 'use' field"
    echo "JWKS output (first 500 chars):"
    echo "${JWKS_OUTPUT}" | head -c 500
    echo ""
    echo "This means SPIRE needs to be patched. Check SPIFFE_KEYCLOAK_SETUP.md"
fi
echo ""

# Check Keycloak
echo "4. Checking Keycloak..."
if kubectl get pods -n keycloak -l app=keycloak | grep -q "Running"; then
    echo "✅ Keycloak is running"
else
    echo "❌ Keycloak is not running"
    kubectl get pods -n keycloak
    exit 1
fi
echo ""

# Check Keycloak admin secret
echo "5. Checking Keycloak admin secret..."
if kubectl get secret -n keycloak keycloak-initial-admin &>/dev/null; then
    echo "✅ Keycloak admin secret exists"
    echo "Username: $(kubectl get secret -n keycloak keycloak-initial-admin -o jsonpath='{.data.username}' | base64 -d)"
else
    echo "❌ Keycloak admin secret not found"
    exit 1
fi
echo ""

# Check SPIFFE IdP setup job
echo "6. Checking SPIFFE IdP setup job..."
if kubectl get job -n kagenti-system kagenti-spiffe-idp-setup-job &>/dev/null; then
    JOB_STATUS=$(kubectl get job -n kagenti-system kagenti-spiffe-idp-setup-job -o jsonpath='{.status.conditions[?(@.type=="Complete")].status}')
    if [ "$JOB_STATUS" == "True" ]; then
        echo "✅ SPIFFE IdP setup job completed successfully"
        echo ""
        echo "Job logs:"
        kubectl logs -n kagenti-system job/kagenti-spiffe-idp-setup-job --tail=20
    else
        echo "⚠️  SPIFFE IdP setup job has not completed yet"
        kubectl get job -n kagenti-system kagenti-spiffe-idp-setup-job
        echo ""
        echo "Job logs:"
        kubectl logs -n kagenti-system job/kagenti-spiffe-idp-setup-job --tail=50
    fi
else
    echo "⚠️  SPIFFE IdP setup job not found (may not be deployed yet)"
fi
echo ""

echo "=========================================="
echo "✅ Verification Complete"
echo "=========================================="
echo ""
echo "To access Keycloak Admin Console:"
echo "  1. Open: http://keycloak.localtest.me:8080/admin"
echo "  2. Switch to the 'kagenti' realm"
echo "  3. Check Identity Providers → Should see 'spire-spiffe' (Type: SPIFFE)"
echo ""
