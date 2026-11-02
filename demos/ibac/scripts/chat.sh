#!/bin/bash
# Send "Summarize my emails." to one demo agent, the way a real client
# would: from inside the cluster, through the agent's Service, with a
# Keycloak token the agent's jwt-validation accepts.
#
# Usage: chat.sh <namespace> <agent-name> [session-id]
#
# Why not curl through `kubectl port-forward`? A port-forward lands on the
# pod's loopback and skips the AuthBridge sidecar's inbound interception,
# so a2a-parser records no intent and IBAC skips (`no_user_context`).
#
# The token comes from the agent's own Keycloak client (client_credentials
# with the credentials the operator mounted at /shared/), because that
# client's token carries the agent's own audience. The request runs from a
# small curl pod (ibac-chat) the script starts on first use; `make undeploy`
# removes it.
#
# The session id becomes the A2A contextId, which is the session the
# sidecar files the chat under — the name you pick in agentop.

set -euo pipefail

NAMESPACE=${1:-team1}
AGENT_NAME=${2:-email-agent}
SESSION_ID=${3:-$AGENT_NAME-$(date +%H%M%S)}
KEYCLOAK_URL=${KEYCLOAK_URL:-http://keycloak.localtest.me:8080}
REALM=${KEYCLOAK_REALM:-rossoctl}
CHAT_POD=ibac-chat

# The curl pod. IfNotPresent: Docker Hub rate-limits anonymous pulls, and
# a node that already has the image should not ask.
if ! kubectl -n "$NAMESPACE" get pod "$CHAT_POD" >/dev/null 2>&1; then
  echo "[*] Starting the $CHAT_POD curl pod ..." >&2
  kubectl -n "$NAMESPACE" run "$CHAT_POD" --image=curlimages/curl:latest \
    --image-pull-policy=IfNotPresent --restart=Never \
    --labels=app.kubernetes.io/part-of=ibac-demo \
    --command -- sleep infinity >/dev/null
fi
kubectl -n "$NAMESPACE" wait --for=condition=Ready "pod/$CHAT_POD" --timeout=120s >/dev/null

CLIENT_ID=$(kubectl -n "$NAMESPACE" exec "deploy/$AGENT_NAME" -c authbridge-proxy -- cat /shared/client-id.txt)
CLIENT_SECRET=$(kubectl -n "$NAMESPACE" exec "deploy/$AGENT_NAME" -c authbridge-proxy -- cat /shared/client-secret.txt)
TOKEN=$(curl -sf -m 10 \
    --data-urlencode grant_type=client_credentials \
    --data-urlencode "client_id=$CLIENT_ID" \
    --data-urlencode "client_secret=$CLIENT_SECRET" \
    "$KEYCLOAK_URL/realms/$REALM/protocol/openid-connect/token" \
  | python3 -c 'import json, sys; print(json.load(sys.stdin)["access_token"])') || {
  echo "ERROR: could not get a token for $CLIENT_ID from $KEYCLOAK_URL." >&2
  echo "       Is Keycloak reachable from this host? Override with KEYCLOAK_URL=..." >&2
  exit 1
}

BODY=$(python3 -c 'import json, sys
sid = sys.argv[1]
print(json.dumps({"jsonrpc": "2.0", "id": 1, "method": "message/send", "params": {"message": {
    "role": "user", "messageId": "msg-" + sid, "contextId": sid,
    "parts": [{"kind": "text", "text": "Summarize my emails."}]}}}))' "$SESSION_ID")

echo "[*] Asking $AGENT_NAME \"Summarize my emails.\" (session $SESSION_ID) ..." >&2
REPLY=$(kubectl -n "$NAMESPACE" exec "$CHAT_POD" -- curl -s -m 240 \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "$BODY" "http://$AGENT_NAME.$NAMESPACE.svc.cluster.local:8080/")

printf '%s' "$REPLY" | python3 -c 'import json, sys
raw = sys.stdin.read()
try:
    print(json.loads(raw)["result"]["status"]["message"]["parts"][0]["text"])
except Exception:
    print("unexpected reply: " + raw[:500])
    sys.exit(1)'
