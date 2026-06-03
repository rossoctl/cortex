#!/bin/bash
# Drive the finance-sparc demo end-to-end through REAL inbound auth (no jwt
# bypass). Gets a user token from Keycloak (ROPC), then sends the two-turn
# refund scenario to the agent through its AuthBridge sidecar, and prints the
# SPARC verdicts recorded in the session.
#
# Usage: drive-demo.sh [namespace] [agent]
set -euo pipefail

NS=${1:-team1}
AGENT=${2:-finance-agent}
REALM=${KEYCLOAK_REALM:-kagenti}
ROPC_CLIENT_ID=${ROPC_CLIENT_ID:-finance-sparc-e2e}
USER=${DEMO_USER:-alice}
PASS=${DEMO_PASS:-alice123}
CTX="finance-demo-$(date +%s)"
PORT=${AGENT_PORT:-18080}
KCPORT=${KC_PORT:-18081}

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }

# Reach Keycloak via kubectl port-forward (portable; doesn't depend on the
# cluster ingress / host port-forward). Keycloak's fixed KC_HOSTNAME makes the
# token's issuer correct regardless of how it's reached.
say "[1/4] Obtaining a user token from Keycloak (via port-forward) ..."
kubectl -n keycloak port-forward svc/keycloak-service "$KCPORT:8080" >/tmp/finance-kc-pf.log 2>&1 &
KCPF=$!; trap 'kill $KCPF 2>/dev/null || true' EXIT
sleep 4
TOKEN=$(curl -s -m 20 "http://localhost:$KCPORT/realms/$REALM/protocol/openid-connect/token" \
  -d grant_type=password -d "client_id=$ROPC_CLIENT_ID" \
  -d "username=$USER" -d "password=$PASS" -d scope=openid \
  | python3 -c 'import json,sys; print(json.load(sys.stdin).get("access_token",""))' 2>/dev/null || true)
if [[ -z "$TOKEN" ]]; then
  echo "ERROR: could not get a token. Did you run 'make setup-keycloak'?" >&2
  cat /tmp/finance-kc-pf.log >&2 2>/dev/null || true
  exit 1
fi
echo "  got token ($(echo -n "$TOKEN" | wc -c | tr -d ' ') chars)"

say "[2/4] Port-forwarding the agent ..."
kubectl -n "$NS" port-forward "svc/$AGENT" "$PORT:8080" >/tmp/finance-pf.log 2>&1 &
PF=$!; trap 'kill $PF $KCPF 2>/dev/null || true' EXIT
sleep 4

send() {  # $1=turn id, $2=text
  curl -s -m "${TURN_TIMEOUT:-240}" -X POST "http://localhost:$PORT/" \
    -H 'Content-Type: application/json' -H "Authorization: Bearer $TOKEN" \
    -d "$(python3 -c 'import json,sys; print(json.dumps({"jsonrpc":"2.0","id":sys.argv[1],"method":"message/send","params":{"message":{"role":"user","parts":[{"kind":"text","text":sys.argv[2]}],"contextId":sys.argv[3]}}}))' "$1" "$2" "$CTX")"
}
reply() { python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); r=d.get("result",{}); print(r.get("status",{}).get("message",{}).get("parts",[{}])[0].get("text") or ("ERROR: "+json.dumps(d)))' "$1"; }

say "[3/4] Turn 1 — descriptive refund, NO transaction id (expect SPARC to ask for the exact id):"
echo '  user> Refund my duplicate $450 subscription charge from last week.'
send 1 'Refund my duplicate $450 subscription charge from last week.' > /tmp/finance-t1.json
echo "  agent> $(reply /tmp/finance-t1.json)"

say "[4/4] Turn 2 — supply the exact id TX4827 (expect SPARC approve -> refund):"
echo '  user> The transaction id is TX4827. Please proceed.'
send 2 'The transaction id is TX4827. Please proceed with the refund, reason: duplicate charge.' > /tmp/finance-t2.json
echo "  agent> $(reply /tmp/finance-t2.json)"

say "SPARC verdicts recorded for this session:"
SID="$CTX"
kubectl -n "$NS" port-forward "deploy/$AGENT" 19094:9094 >/tmp/finance-pf94.log 2>&1 &
PF94=$!; trap 'kill $PF $PF94 $KCPF 2>/dev/null || true' EXIT
sleep 3
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
curl -s -m 10 "http://localhost:19094/v1/sessions/$SID" 2>/dev/null | python3 "$SCRIPT_DIR/show-verdicts.py"
echo
echo "Done. Inspect the full pipeline with: make show-result"
