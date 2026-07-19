#!/bin/bash
# Patch the namespace's `authbridge-runtime-config` ConfigMap to add
# an `mtls:` block at the top level. Idempotent — re-running with the
# same mode produces no change.
#
# Why the namespace CM and not the per-agent CM:
#
#   The operator's pod-mutating webhook builds each per-agent
#   `authbridge-config-<name>` CM by reading the namespace
#   `authbridge-runtime-config` as baseYAML and overlaying values
#   resolved from the AgentRuntime CR (or defaults when no CR exists).
#   The webhook's `ensurePerAgentConfigMap` actively scrubs the
#   `mtls:` block when `Spec.MTLSMode` is unset on the CR — that
#   scrub is intentional, not a bug, and means a kubectl-patch on the
#   per-agent CM gets reverted on the next pod admission.
#
#   Patching the namespace CM puts the `mtls:` value where the
#   resolution chain reads it (via the operator's ExtractMTLSMode
#   helper). The webhook then propagates it into every per-agent CM
#   in that namespace at admission time. One patch, one CM, all
#   workloads in the namespace pick it up on next pod restart.
#
# Usage: patch-mtls-config.sh <namespace> <mode>
#   mode is "permissive" or "strict"

set -euo pipefail

NAMESPACE=${1:?usage: patch-mtls-config.sh <namespace> <mode>}
MODE=${2:?usage: patch-mtls-config.sh <namespace> <mode>}

case "$MODE" in
  permissive|strict) ;;
  *) echo "ERROR: mode must be permissive or strict, got: $MODE" >&2; exit 1 ;;
esac

CM="authbridge-runtime-config"
HERE=$(dirname "$0")

echo "[*] Patching $CM in $NAMESPACE with mtls.mode=$MODE"

ORIG=$(kubectl -n "$NAMESPACE" get cm "$CM" -o jsonpath='{.data.config\.yaml}')
if [[ -z "$ORIG" ]]; then
  echo "ERROR: ConfigMap $CM has no config.yaml entry — is the rossoctl chart installed in this namespace?" >&2
  exit 1
fi

MERGED=$(echo "$ORIG" | python3 "$HERE/mtls-merge.py" "$MODE")

# kubectl apply with a fresh ConfigMap manifest preserves the
# operator's ownership labels; the dry-run-then-apply pattern keeps
# this idempotent.
kubectl -n "$NAMESPACE" create configmap "$CM" \
  --from-literal=config.yaml="$MERGED" \
  --dry-run=client -o yaml | \
  kubectl -n "$NAMESPACE" apply -f - >/dev/null

echo "[*] Patched. Restart any agent pods in $NAMESPACE to pick up the new mtls posture."
