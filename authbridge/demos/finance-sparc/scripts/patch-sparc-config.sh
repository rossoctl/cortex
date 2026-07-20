#!/bin/bash
# Patch the operator-rendered authbridge ConfigMap to add the SPARC
# pipeline (a2a-parser inbound + inference-parser + mcp-parser + sparc
# outbound).
#
# Usage: patch-sparc-config.sh <namespace> <agent-name>
#
# The rossoctl operator creates `authbridge-config-<agent>` when the agent
# pod is admitted (default pipeline: jwt-validation inbound, token-exchange
# outbound). This script extracts that config, merges k8s/sparc-patch.yaml
# via scripts/pipeline-merge.py, re-applies it, and blocks until the
# running authbridge process reports the matching config SHA (handles both
# the hot-reload and pod-roll convergence paths).
set -euo pipefail

NAMESPACE=${1:-team1}
AGENT_NAME=${2:-finance-agent}
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
DEMO_DIR=$(dirname "$SCRIPT_DIR")
PATCH_FILE="$DEMO_DIR/k8s/sparc-patch.yaml"
CM_NAME="authbridge-config-$AGENT_NAME"

# Resolve a python with PyYAML: prefer system python3, else use uv ephemerally.
if python3 -c 'import yaml' 2>/dev/null; then
  PY="python3"
elif command -v uv >/dev/null 2>&1; then
  PY="uv run --quiet --with pyyaml python3"
else
  echo "ERROR: need PyYAML (pip3 install --user pyyaml) or uv on PATH." >&2
  exit 1
fi

# Render ${...} placeholders in the patch fragment (provider-tunable knobs).
# Defaults keep watsonx behavior identical to before.
SPARC_TIMEOUT_MS=${SPARC_TIMEOUT_MS:-30000}
SPARC_REFLECTOR_ENDPOINT=${SPARC_REFLECTOR_ENDPOINT:-http://sparc-service.rossoctl-system.svc.cluster.local:8090}
RENDERED_PATCH=$(mktemp)
trap 'rm -f "$RENDERED_PATCH"' EXIT
sed -e "s|\${SPARC_TIMEOUT_MS}|${SPARC_TIMEOUT_MS}|g" \
    -e "s|\${SPARC_REFLECTOR_ENDPOINT}|${SPARC_REFLECTOR_ENDPOINT}|g" \
    "$PATCH_FILE" > "$RENDERED_PATCH"
PATCH_FILE="$RENDERED_PATCH"
echo "[*] SPARC reflection: endpoint=$SPARC_REFLECTOR_ENDPOINT timeout_ms=$SPARC_TIMEOUT_MS"

if ! kubectl -n "$NAMESPACE" get configmap "$CM_NAME" >/dev/null 2>&1; then
  echo "ERROR: ConfigMap $NAMESPACE/$CM_NAME not found." >&2
  echo "       The operator creates it when the agent pod is admitted." >&2
  echo "       Check: kubectl -n $NAMESPACE get pods -l app.kubernetes.io/name=$AGENT_NAME" >&2
  exit 1
fi

echo "[*] Merging SPARC additions into $CM_NAME ..."
CURRENT_YAML=$(kubectl -n "$NAMESPACE" get configmap "$CM_NAME" -o jsonpath='{.data.config\.yaml}')
MERGED_YAML=$(printf '%s' "$CURRENT_YAML" | $PY "$SCRIPT_DIR/pipeline-merge.py" "$PATCH_FILE")

if [[ -z "$MERGED_YAML" ]]; then
  echo "ERROR: merge produced empty output" >&2
  exit 1
fi

show_plugins() {
  $PY -c '
import yaml, sys
c = yaml.safe_load(sys.stdin) or {}
for d in ("inbound", "outbound"):
    names = [p["name"] for p in c.get("pipeline", {}).get(d, {}).get("plugins", [])]
    print(f"      {d}: {names}")
'
}

if [[ "$CURRENT_YAML" == "$MERGED_YAML" ]]; then
  echo "[*] $CM_NAME already contains the SPARC pipeline — nothing to patch."
  printf '%s' "$CURRENT_YAML" | show_plugins
  exit 0
fi

echo "[*] Applying patched ConfigMap ..."
TMP_CONFIG=$(mktemp)
trap 'rm -f "$TMP_CONFIG" "$RENDERED_PATCH"' EXIT
printf '%s' "$MERGED_YAML" >"$TMP_CONFIG"
kubectl -n "$NAMESPACE" create configmap "$CM_NAME" \
    --from-file=config.yaml="$TMP_CONFIG" --dry-run=client -o yaml | kubectl apply -f -

echo "[*] Patched. Active plugins now:"
printf '%s' "$MERGED_YAML" | show_plugins

# Block until the running authbridge process reports the SHA we applied.
WANT_SHA=$(printf '%s' "$MERGED_YAML" | sha256sum | awk '{print $1}')
TIMEOUT=${RELOAD_TIMEOUT:-180}
DEADLINE=$(( $(date +%s) + TIMEOUT ))
echo "[*] Waiting for authbridge to load the patched config (timeout ${TIMEOUT}s)"
echo "    target SHA: $WANT_SHA"
ACTIVE_SHA=""
while [[ $(date +%s) -lt $DEADLINE ]]; do
  ACTIVE_SHA=$(kubectl -n "$NAMESPACE" exec deploy/"$AGENT_NAME" -c authbridge-proxy -- \
      wget -q -O - http://localhost:9093/reload/status 2>/dev/null | \
      python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("active_config_sha256",""))
except Exception: pass' 2>/dev/null || true)
  if [[ "$ACTIVE_SHA" == "$WANT_SHA" ]]; then
    echo "[*] Active config SHA matches — SPARC pipeline is live."
    exit 0
  fi
  sleep 3
done

echo "ERROR: authbridge active config did not match patched SHA within ${TIMEOUT}s." >&2
echo "       want:        $WANT_SHA" >&2
echo "       last active: ${ACTIVE_SHA:-<none>}" >&2
kubectl -n "$NAMESPACE" logs deploy/"$AGENT_NAME" -c authbridge-proxy --tail=20 >&2 || true
exit 1
