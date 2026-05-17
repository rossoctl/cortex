#!/usr/bin/env bash
# Deploy the Weather advanced AuthBridge demo and verify Keycloak token exchange
# plus MCP ingress JWT validation on the tool.
#
# Prerequisites:
#   - kubectl configured for a cluster with Kagenti + Keycloak + SPIRE + webhook
#   - Namespace team1 (or override NAMESPACE) with installer ConfigMaps
#   - jq installed on the machine running this script
#   - Python 3.10+ with AuthBridge/requirements.txt (for setup_keycloak_weather_advanced.py)
#   - Optional: Ollama on the host with the model from weather-service-advanced for full A2A
#
# Usage:
#   ./deploy_and_verify_advanced.sh
#   NAMESPACE=team1 ./deploy_and_verify_advanced.sh
#   SKIP_DEPLOY=1 ./deploy_and_verify_advanced.sh    # verify only (resources must exist)
#
# Timeouts (optional, for slow clusters / GitHub Kind: image pull + many sidecars):
#   WEATHER_TOOL_ROLLOUT_TIMEOUT  kubectl rollout status for the tool (default: 1800s;
#                                 should be >= spec.progressDeadlineSeconds in the YAML)
#   WEATHER_AGENT_ROLLOUT_TIMEOUT kubectl rollout status for the agent (default: 1800s)
#   WEATHER_TOOL_KC_CLIENT_SEC    setup_keycloak --tool-client-timeout, seconds (default: 900)
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AUTHBRIDGE_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
NAMESPACE="${NAMESPACE:-team1}"
SKIP_DEPLOY="${SKIP_DEPLOY:-0}"
AGENT_SA="${AGENT_SA:-weather-service-advanced}"
TOOL_SA="${TOOL_SA:-weather-tool-advanced}"
SPIFFE_DOMAIN="${SPIFFE_DOMAIN:-localtest.me}"
TOOL_SPIFFE="spiffe://${SPIFFE_DOMAIN}/ns/${NAMESPACE}/sa/${TOOL_SA}"
AGENT_SPIFFE="spiffe://${SPIFFE_DOMAIN}/ns/${NAMESPACE}/sa/${AGENT_SA}"
KC_INTERNAL="${KC_INTERNAL:-http://keycloak-service.keycloak.svc:8080}"
KC_REALM="${KC_REALM:-kagenti}"
# Default matches client created by setup_keycloak_weather_advanced.py (kagenti UI
# client often has direct access grants disabled).
KC_USER_CLIENT_ID="${KC_USER_CLIENT_ID:-weather-advanced-e2e}"
# For confidential "kagenti" UI client, set KC_USER_CLIENT_SECRET in the environment.
#
# Rollout: align with spec.progressDeadlineSeconds: 1800 on the tool/agent Deployments (Kind
# can exceed 600s default) and with kubectl --timeout below.
WEATHER_TOOL_ROLLOUT_TIMEOUT="${WEATHER_TOOL_ROLLOUT_TIMEOUT:-1800s}"
WEATHER_AGENT_ROLLOUT_TIMEOUT="${WEATHER_AGENT_ROLLOUT_TIMEOUT:-1800s}"
WEATHER_TOOL_KC_CLIENT_SEC="${WEATHER_TOOL_KC_CLIENT_SEC:-900}"

log() { printf '%s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

need_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

need_cmd kubectl
need_cmd jq
need_cmd curl

if [[ "$SKIP_DEPLOY" != "1" ]]; then
  log "Applying authproxy-routes ConfigMap..."
  kubectl apply -f "$SCRIPT_DIR/k8s/configmaps-advanced.yaml"

  log "Deploying weather tool (advanced)..."
  kubectl apply -f "$SCRIPT_DIR/k8s/weather-tool-advanced.yaml"
  log "Applying AgentRuntime (required in Kagenti 0.2+ for AuthBridge injection)..."
  if kubectl get crd agentruntimes.agent.kagenti.dev &>/dev/null; then
    kubectl apply -f "$SCRIPT_DIR/k8s/agentruntime-weather-tool-advanced.yaml"
    log "Recreating tool pods so the webhook can inject now that AgentRuntime exists..."
    kubectl rollout restart "deployment/weather-tool-advanced" -n "$NAMESPACE"
  else
    log "WARNING: agentruntimes.agent.kagenti.dev CRD not found — sidecars may not inject."
  fi
  kubectl rollout status "deployment/weather-tool-advanced" -n "$NAMESPACE" --timeout="$WEATHER_TOOL_ROLLOUT_TIMEOUT"

  log "Running Keycloak setup (wait for tool client + enable token exchange)..."
  (
    cd "$AUTHBRIDGE_ROOT"
    if [[ ! -d venv ]]; then
      python3 -m venv venv
    fi
    # shellcheck disable=SC1091
    source venv/bin/activate
    pip install -q -r requirements.txt
    python demos/weather-agent/setup_keycloak_weather_advanced.py \
      -n "$NAMESPACE" \
      -a "$AGENT_SA" \
      -t "$TOOL_SA" \
      --wait-tool-client \
      --tool-client-timeout "$WEATHER_TOOL_KC_CLIENT_SEC"
  )

  log "Deploying weather agent (advanced)..."
  kubectl apply -f "$SCRIPT_DIR/k8s/weather-service-advanced.yaml"
  if kubectl get crd agentruntimes.agent.kagenti.dev &>/dev/null; then
    kubectl apply -f "$SCRIPT_DIR/k8s/agentruntime-weather-service-advanced.yaml"
    kubectl rollout restart "deployment/weather-service-advanced" -n "$NAMESPACE"
  fi
  kubectl rollout status "deployment/weather-service-advanced" -n "$NAMESPACE" --timeout="$WEATHER_AGENT_ROLLOUT_TIMEOUT"

  log "Re-running Keycloak setup so the agent client receives optional exchange scopes..."
  (
    cd "$AUTHBRIDGE_ROOT"
    # shellcheck disable=SC1091
    source venv/bin/activate
    python demos/weather-agent/setup_keycloak_weather_advanced.py \
      -n "$NAMESPACE" \
      -a "$AGENT_SA" \
      -t "$TOOL_SA"
  )
else
  log "SKIP_DEPLOY=1 — assuming ConfigMaps and Deployments already exist"
fi

log "Discovering AuthBridge log container name (envoy-proxy vs combined authbridge)..."
ADV_TOOL_POD=$(kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/name=weather-tool-advanced \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
[[ -n "$ADV_TOOL_POD" ]] || die "no weather-tool-advanced pod found"

ADV_AGENT_POD=$(kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/name=weather-service-advanced \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
[[ -n "$ADV_AGENT_POD" ]] || die "no weather-service-advanced pod found"

pick_log_container() {
  local pod=$1
  local names
  names=$(kubectl get pod -n "$NAMESPACE" "$pod" -o jsonpath='{.spec.containers[*].name}' | tr ' ' '\n')
  # Container name depends on the resolved AuthBridge mode (per
  # kagenti-operator/internal/webhook/injector/container_builder.go):
  #   proxy-sidecar (cluster default after kagenti-operator#361):
  #     AuthBridgeProxyContainerName = "authbridge-proxy"
  #   envoy-sidecar:
  #     EnvoyProxyContainerName = "envoy-proxy"
  # The earlier `combinedSidecar` feature gate produced a bare
  # "authbridge" container, but that gate (and the constant) were
  # removed in #361 — and this script already requires the post-#361
  # AgentRuntime CRD just above, so there's no pre-#361 path to
  # support here.
  for c in authbridge-proxy envoy-proxy; do
    if echo "$names" | grep -qx "$c"; then
      echo "$c"
      return 0
    fi
  done
  die "pod $pod has no AuthBridge sidecar (expected one of authbridge-proxy / envoy-proxy; got: $(echo $names))"
}

TOOL_LOG_C=$(pick_log_container "$ADV_TOOL_POD")
AGENT_LOG_C=$(pick_log_container "$ADV_AGENT_POD")
log "Using log containers: tool=$TOOL_LOG_C agent=$AGENT_LOG_C"

log "Running in-cluster verification (Keycloak + MCP) via netshoot..."
VERIFY_SCRIPT=$(cat <<VERIFYEOS
set -euo pipefail
KC_INTERNAL='${KC_INTERNAL}'
KC_REALM='${KC_REALM}'
AGENT_SPIFFE='${AGENT_SPIFFE}'
TOOL_SPIFFE='${TOOL_SPIFFE}'
KC_USER_CLIENT_ID='${KC_USER_CLIENT_ID}'
KC_USER_CLIENT_SECRET='${KC_USER_CLIENT_SECRET:-}'
ADMIN_USER='${KEYCLOAK_ADMIN_USERNAME:-admin}'
ADMIN_PASS='${KEYCLOAK_ADMIN_PASSWORD:-admin}'

echo "Fetching admin token..."
ADMIN_TOKEN=\$(curl -sS -X POST "\${KC_INTERNAL}/realms/master/protocol/openid-connect/token" \\
  -d "grant_type=password" -d "client_id=admin-cli" \\
  -d "username=\${ADMIN_USER}" -d "password=\${ADMIN_PASS}" | jq -r .access_token)
test -n "\$ADMIN_TOKEN" && test "\$ADMIN_TOKEN" != "null"

echo "Resolving agent client id..."
CLIENT_JSON=\$(curl -sS -G "\${KC_INTERNAL}/admin/realms/\${KC_REALM}/clients" \\
  -H "Authorization: Bearer \${ADMIN_TOKEN}" --data-urlencode "clientId=\${AGENT_SPIFFE}")
AGENT_CLIENT_UUID=\$(echo "\$CLIENT_JSON" | jq -r '.[0].id // empty')
test -n "\$AGENT_CLIENT_UUID"

echo "Reading agent client secret..."
SECRET_JSON=\$(curl -sS "\${KC_INTERNAL}/admin/realms/\${KC_REALM}/clients/\${AGENT_CLIENT_UUID}/client-secret" \\
  -H "Authorization: Bearer \${ADMIN_TOKEN}")
AGENT_CLIENT_SECRET=\$(echo "\$SECRET_JSON" | jq -r '.value // empty')
test -n "\$AGENT_CLIENT_SECRET"

echo "Password grant (alice)..."
if test -n "\$KC_USER_CLIENT_SECRET"; then
  USER_JSON=\$(curl -sS -u "\${KC_USER_CLIENT_ID}:\${KC_USER_CLIENT_SECRET}" -X POST \\
    "\${KC_INTERNAL}/realms/\${KC_REALM}/protocol/openid-connect/token" \\
    -d "grant_type=password" -d "username=alice" -d "password=alice123")
else
  USER_JSON=\$(curl -sS -X POST "\${KC_INTERNAL}/realms/\${KC_REALM}/protocol/openid-connect/token" \\
    -d "grant_type=password" -d "client_id=\${KC_USER_CLIENT_ID}" \\
    -d "username=alice" -d "password=alice123")
fi
USER_ACCESS=\$(echo "\$USER_JSON" | jq -r .access_token)
if test -z "\$USER_ACCESS" || test "\$USER_ACCESS" = "null"; then
  echo "\$USER_JSON" | jq . >&2 || true
  echo "alice password grant failed" >&2
  exit 1
fi

echo "RFC 8693 token exchange to tool SPIFFE audience..."
# Use form client_id + client_secret (Keycloak rejects Basic auth usernames with '/' in SPIFFE).
EXCHANGE_JSON=\$(curl -sS -X POST "\${KC_INTERNAL}/realms/\${KC_REALM}/protocol/openid-connect/token" \\
  -H "Content-Type: application/x-www-form-urlencoded" \\
  --data-urlencode "client_id=\${AGENT_SPIFFE}" \\
  --data-urlencode "client_secret=\${AGENT_CLIENT_SECRET}" \\
  --data-urlencode "grant_type=urn:ietf:params:oauth:grant-type:token-exchange" \\
  --data-urlencode "requested_token_type=urn:ietf:params:oauth:token-type:access_token" \\
  --data-urlencode "subject_token=\${USER_ACCESS}" \\
  --data-urlencode "subject_token_type=urn:ietf:params:oauth:token-type:access_token" \\
  --data-urlencode "audience=\${TOOL_SPIFFE}" \\
  --data-urlencode "scope=openid weather-tool-exchange-aud")
EXCHANGED=\$(echo "\$EXCHANGE_JSON" | jq -r .access_token)
if test -z "\$EXCHANGED" || test "\$EXCHANGED" = "null"; then
  echo "\$EXCHANGE_JSON" | jq . >&2 || true
  echo "token exchange failed" >&2
  exit 1
fi

echo "POST /mcp with exchanged token (streamable HTTP; expect 2xx)..."
# FastMCP streamable transport rejects requests unless the client advertises support
# for both JSON and SSE (HTTP 406 otherwise).
HTTP_CODE=\$(curl -sS -o /tmp/mcp.body -w '%{http_code}' \\
  -H "Authorization: Bearer \${EXCHANGED}" \\
  -H "Content-Type: application/json" \\
  -H "Accept: application/json, text/event-stream" \\
  -X POST "http://weather-tool-advanced-mcp:8000/mcp" \\
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"verify","version":"0.1"}}}')
echo "MCP_HTTP_CODE=\${HTTP_CODE}"
if test "\$HTTP_CODE" = "401"; then
  echo "MCP returned 401" >&2
  exit 1
fi
if ! [[ \${HTTP_CODE} =~ ^2[0-9][0-9]\$ ]]; then
  echo "MCP returned HTTP \${HTTP_CODE}" >&2
  head -c 2000 /tmp/mcp.body >&2 || true
  exit 1
fi

echo "POST /mcp without Authorization (expect 401 from AuthBridge)..."
NEG_CODE=\$(curl -sS -o /tmp/mcp.neg -w '%{http_code}' \\
  -H "Content-Type: application/json" \\
  -H "Accept: application/json, text/event-stream" \\
  -X POST "http://weather-tool-advanced-mcp:8000/mcp" \\
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"verify-neg","version":"0.1"}}}')
echo "MCP_NEG_HTTP_CODE=\${NEG_CODE}"
if test "\$NEG_CODE" != "401"; then
  echo "expected HTTP 401 without bearer token, got \${NEG_CODE}" >&2
  head -c 2000 /tmp/mcp.neg >&2 || true
  exit 1
fi
VERIFYEOS
)

VERIFY_B64=$(printf '%s' "$VERIFY_SCRIPT" | base64 | tr -d '\n')
kubectl run "adv-verify-$$" --rm -i --restart=Never -n "$NAMESPACE" \
  --image=nicolaka/netshoot:v0.15 \
  --command -- bash -lc "echo '${VERIFY_B64}' | base64 -d | bash" | tee /tmp/adv-verify.out

grep -q 'MCP_HTTP_CODE=' /tmp/adv-verify.out || die "verify pod produced no MCP_HTTP_CODE line"
HTTP_CODE=$(grep 'MCP_HTTP_CODE=' /tmp/adv-verify.out | tail -1 | cut -d= -f2)
if [[ "$HTTP_CODE" == "401" ]]; then
  die "MCP returned 401 — tool AuthBridge rejected the exchanged token"
fi
if ! [[ "$HTTP_CODE" =~ ^2[0-9][0-9]$ ]]; then
  die "MCP returned HTTP $HTTP_CODE — expected 2xx after initialize (401=auth, 406=missing Accept: application/json, text/event-stream)"
fi
log "MCP HTTP status: ${HTTP_CODE} (2xx: JWT accepted at ingress and streamable HTTP handshake OK)"

grep -q 'MCP_NEG_HTTP_CODE=' /tmp/adv-verify.out || die "verify pod produced no MCP_NEG_HTTP_CODE line"
NEG_HTTP_CODE=$(grep 'MCP_NEG_HTTP_CODE=' /tmp/adv-verify.out | tail -1 | cut -d= -f2)
if [[ "$NEG_HTTP_CODE" != "401" ]]; then
  die "negative check: without Authorization, expected HTTP 401 from tool ingress, got $NEG_HTTP_CODE"
fi
log "Negative check: unauthenticated MCP request returned 401 (expected)"

log "Checking tool AuthBridge logs for successful inbound validation..."
sleep 3
TOOL_LOGS=$(kubectl logs -n "$NAMESPACE" "$ADV_TOOL_POD" -c "$TOOL_LOG_C" 2>&1 | tail -400 || true)
if echo "$TOOL_LOGS" | grep -qE "Token validated|\[Inbound\]"; then
  log "OK: found inbound validation log line on weather-tool-advanced."
else
  log "WARNING: could not find '[Inbound]' / 'Token validated' in recent tool logs (combined sidecar may use different text)."
fi

log "Checking agent AuthBridge logs for token exchange / outbound activity..."
AGENT_LOGS=$(kubectl logs -n "$NAMESPACE" "$ADV_AGENT_POD" -c "$AGENT_LOG_C" 2>&1 | tail -400 || true)
if echo "$AGENT_LOGS" | grep -qE "Resolver|exchange|Injecting token|Client Credentials"; then
  log "OK: found outbound / exchange related log line on weather-service-advanced."
else
  log "WARNING: no obvious outbound exchange markers in recent agent logs (traffic may not have occurred yet)."
fi

log "SUCCESS: deploy_and_verify_advanced.sh completed."
