#!/bin/bash
# Patch the operator-rendered authbridge ConfigMap to add the IBAC
# pipeline (a2a-parser inbound + mcp-parser + ibac outbound).
#
# Usage: patch-ibac-config.sh <namespace> <agent-name>
#
# The kagenti operator creates `authbridge-config-<agent>` when the
# agent's pod is admitted (server-side apply, line 682 of pod_mutator.go).
# Its default pipeline has only `jwt-validation` inbound and
# `token-exchange` outbound. This script:
#
#   1. Extracts the operator's config.yaml from the ConfigMap
#   2. Reads our additions from k8s/ibac-patch.yaml
#   3. Merges them via python3 + PyYAML (PyYAML is widely available;
#      we error out with an actionable hint if it's missing)
#   4. Replaces the ConfigMap's data.config.yaml with the merged version
#
# Authbridge's filesystem-watch hot-reload picks up the change without
# a Pod restart (the operator-injected sidecar doesn't set
# readOnlyRootFilesystem so fsnotify can see the symlink swap). On a
# re-run where the CM is already patched (e.g. previous demo run +
# fresh rollout-restart that booted with the patched content), the
# script short-circuits — no apply, no reload-wait.

set -euo pipefail

NAMESPACE=${1:-team1}
AGENT_NAME=${2:-email-agent}
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
DEMO_DIR=$(dirname "$SCRIPT_DIR")
PATCH_FILE="$DEMO_DIR/k8s/ibac-patch.yaml"
CM_NAME="authbridge-config-$AGENT_NAME"

if ! python3 -c 'import yaml' 2>/dev/null; then
  cat <<'EOF' >&2
ERROR: python3-yaml (PyYAML) is required.
  Install with one of:
    pip3 install --user pyyaml
    brew install libyaml && pip3 install pyyaml      # macOS
    sudo apt install python3-yaml                    # Debian/Ubuntu
EOF
  exit 1
fi

if ! kubectl -n "$NAMESPACE" get configmap "$CM_NAME" >/dev/null 2>&1; then
  echo "ERROR: ConfigMap $NAMESPACE/$CM_NAME not found." >&2
  echo "       The operator should create this when the agent pod is admitted." >&2
  echo "       Check: kubectl -n $NAMESPACE get pods -l app.kubernetes.io/name=$AGENT_NAME" >&2
  exit 1
fi

# The merge runs in scripts/ibac-merge.py: reads the patch from a
# file (argv[1]), reads the operator's current config.yaml from stdin,
# emits merged YAML to stdout. Idempotent: re-running is a no-op once
# the plugins are present.
#
# Implemented as a separate .py file rather than an inline heredoc
# because heredoc + piped stdin clash — python3 with `<<EOF` reads
# its script from the heredoc, which silently drops the kubectl pipe.
echo "[*] Merging IBAC additions into $CM_NAME ..."
CURRENT_YAML=$(
  kubectl -n "$NAMESPACE" get configmap "$CM_NAME" \
      -o jsonpath='{.data.config\.yaml}'
)
MERGED_YAML=$(
  printf '%s' "$CURRENT_YAML" \
    | python3 "$SCRIPT_DIR/ibac-merge.py" "$PATCH_FILE"
)

if [[ -z "$MERGED_YAML" ]]; then
  echo "ERROR: merge produced empty output" >&2
  exit 1
fi

# No-op short-circuit: if the patch wouldn't change anything (typical
# on a re-run where a previous demo invocation already patched the CM
# and the agent pod booted with that content baked in), skip the apply
# AND skip the reload wait. Otherwise we block forever waiting for a
# swap event that will never fire — kubectl apply is a no-op, kubelet
# has nothing to sync, the reloader sees no fs event.
if [[ "$CURRENT_YAML" == "$MERGED_YAML" ]]; then
  echo "[*] $CM_NAME already contains IBAC config — nothing to patch."
  echo "[*] Active plugins:"
  printf '%s' "$CURRENT_YAML" | python3 -c '
import yaml, sys
c = yaml.safe_load(sys.stdin)
for d in ("inbound", "outbound"):
    names = [p["name"] for p in c.get("pipeline", {}).get(d, {}).get("plugins", [])]
    print(f"      {d}: {names}")
'
  exit 0
fi

# Apply the patched ConfigMap. Using `kubectl create configmap
# --from-file --dry-run=client -o yaml | kubectl apply` is the
# conflict-free pattern: it sidesteps the resource-version mismatch
# you get when piping the existing CM through edits and re-applying.
echo "[*] Applying patched ConfigMap ..."
TMP_CONFIG=$(mktemp)
trap 'rm -f "$TMP_CONFIG"' EXIT
printf '%s' "$MERGED_YAML" >"$TMP_CONFIG"

kubectl -n "$NAMESPACE" create configmap "$CM_NAME" \
    --from-file=config.yaml="$TMP_CONFIG" \
    --dry-run=client -o yaml \
  | kubectl apply -f -

echo "[*] Patched. Active plugins now:"
kubectl -n "$NAMESPACE" get configmap "$CM_NAME" \
    -o jsonpath='{.data.config\.yaml}' \
  | python3 -c '
import yaml, sys
c = yaml.safe_load(sys.stdin)
for d in ("inbound", "outbound"):
    names = [p["name"] for p in c.get("pipeline", {}).get(d, {}).get("plugins", [])]
    print(f"      {d}: {names}")
'

# Block until the running authbridge process is using a config whose
# SHA-256 matches what we just applied. The sidecar exposes its
# active config's SHA at :9093/reload/status (`active_config_sha256`);
# we compare it to the SHA of the merged YAML. This handles both
# convergence pathways uniformly:
#
#   - Hot-reload: same pod, the reloader detects the projected-volume
#     symlink swap (kubelet syncs every ~60s) and rebuilds pipelines;
#     active_config_sha256 advances on swap completion.
#   - Pod-roll: a fresh pod (e.g. operator's reconciler restarted the
#     deployment) boots with the patched ConfigMap mounted from the
#     start, so its initial active_config_sha256 already matches.
#
# Tailing logs for "reloader: pipelines swapped" only catches the
# hot-reload path and misses the pod-roll path entirely (the new
# pod's startup never logs a "swap" — it loaded the right config at
# boot). The SHA check is correct in both cases.
WANT_SHA=$(printf '%s' "$MERGED_YAML" | sha256sum | awk '{print $1}')
TIMEOUT=${RELOAD_TIMEOUT:-180}
DEADLINE=$(( $(date +%s) + TIMEOUT ))
echo "[*] Waiting for authbridge to load the patched config (timeout ${TIMEOUT}s)"
echo "    target SHA: $WANT_SHA"

ACTIVE_SHA=""
while [[ $(date +%s) -lt $DEADLINE ]]; do
  ACTIVE_SHA=$(kubectl -n "$NAMESPACE" exec deploy/"$AGENT_NAME" -c authbridge-proxy -- \
      wget -q -O - http://localhost:9093/reload/status 2>/dev/null | \
      python3 -c 'import json, sys
try:
    print(json.load(sys.stdin).get("active_config_sha256", ""))
except Exception:
    pass' 2>/dev/null || true)
  if [[ "$ACTIVE_SHA" == "$WANT_SHA" ]]; then
    echo "[*] Active config SHA matches — patch is live."
    exit 0
  fi
  sleep 3
done

echo "ERROR: authbridge active config did not match patched SHA within ${TIMEOUT}s." >&2
echo "       want:        $WANT_SHA" >&2
echo "       last active: ${ACTIVE_SHA:-<none>}" >&2
echo "       Last 20 lines of the authbridge container:" >&2
kubectl -n "$NAMESPACE" logs deploy/"$AGENT_NAME" -c authbridge-proxy --tail=20 >&2 || true
echo >&2
echo "       Likely causes:" >&2
echo "         - ConfigMap parse error (look for 'reload failed' above)" >&2
echo "         - kubelet sync slow (retry: RELOAD_TIMEOUT=300 make patch-config)" >&2
echo "         - operator reconciler reverted the patch (re-run patch-config)" >&2
exit 1
