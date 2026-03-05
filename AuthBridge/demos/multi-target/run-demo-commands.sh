#!/usr/bin/env bash
##
# Run the commands outlined after initial setup and deployment in `demo.md`
#

set -eu



## Step 1. Get Token and Show Exchange

kubectl exec deployment/agent -n authbridge -c agent -- sh -c '
CLIENT_ID=$(cat /shared/client-id.txt)
CLIENT_SECRET=$(cat /shared/client-secret.txt)
TOKEN_URL="http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token"

# Get original token response (includes scope in response)
ORIG_RESP=$(curl -s $TOKEN_URL \
  -d "grant_type=client_credentials" \
  -d "client_id=$CLIENT_ID" \
  -d "client_secret=$CLIENT_SECRET")

TOKEN=$(echo "$ORIG_RESP" | jq -r ".access_token")
ORIG_SCOPE=$(echo "$ORIG_RESP" | jq -r ".scope")

echo "=== Original Token (before exchange) ==="
echo "  Client ID:  $CLIENT_ID"
echo "  Audience:   (none - client credentials token)"
echo "  Scopes:     $ORIG_SCOPE"
echo ""

# Manually exchange for target-alpha to show the transformation
EXCHANGE_RESP=$(curl -s $TOKEN_URL \
  -d "grant_type=urn:ietf:params:oauth:grant-type:token-exchange" \
  -d "client_id=$CLIENT_ID" \
  -d "client_secret=$CLIENT_SECRET" \
  -d "subject_token=$TOKEN" \
  -d "subject_token_type=urn:ietf:params:oauth:token-type:access_token" \
  -d "audience=target-alpha" \
  -d "scope=openid target-alpha-aud")

EXCHANGED_SCOPE=$(echo "$EXCHANGE_RESP" | jq -r ".scope")

echo "=== After Token Exchange (for target-alpha) ==="
echo "  Audience:   target-alpha"
echo "  Scopes:     $EXCHANGED_SCOPE"
echo ""

echo "=== Calling All Targets (AuthBridge exchanges automatically) ==="
echo ""

echo "Target Alpha:"
curl -s -H "Authorization: Bearer $TOKEN" http://target-alpha-service:8081/test
echo ""

echo "Target Beta:"
curl -s -H "Authorization: Bearer $TOKEN" http://target-beta-service:8081/test
echo ""

echo "Target Gamma:"
curl -s -H "Authorization: Bearer $TOKEN" http://target-gamma-service:8081/test
echo ""
'


## Step 2. Check AuthBridge Logs
echo ""
echo "=== AuthBridge Token Exchange Logs ==="
kubectl logs deployment/agent -n authbridge -c envoy-proxy --tail=500 2>&1 | \
  grep -E "Host.*matched|Using route target_audience|Successfully exchanged token," | \
  tail -12
