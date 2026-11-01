#!/bin/bash
# Launch agentop against the IBAC agent's authbridge sidecar.
#
# Since agentop now ships with a built-in Namespaces → Pods picker that
# spawns its own kubectl port-forward, this script is a thin wrapper
# that just verifies prerequisites and runs the binary. The picker
# discovers the agents automatically; the user picks team1, then a pod.

set -euo pipefail

NAMESPACE=${1:-team1}
AGENT_NAME=${2:-email-agent}
AGENTOP_BIN=${AGENTOP_BIN:-/tmp/agentop-ibac-demo}

# 1. The Makefile's build-agentop target builds the binary on demand.
#    If it's still missing, the user invoked the script directly —
#    point them at the right entry point.
if [[ ! -x "$AGENTOP_BIN" ]]; then
  echo "ERROR: agentop binary not found at $AGENTOP_BIN." >&2
  echo "       Run \`make show-result\` (which depends on build-agentop)" >&2
  echo "       or \`make build-agentop\` first." >&2
  exit 1
fi

# 2. Verify the agent pod is up — friendlier failure than dropping the
#    user into an empty picker.
if ! kubectl -n "$NAMESPACE" get deploy "$AGENT_NAME" >/dev/null 2>&1; then
  echo "ERROR: deployment $NAMESPACE/$AGENT_NAME not found." >&2
  echo "       Run 'make demo-ibac' first." >&2
  exit 1
fi

# 3. Run agentop. The picker handles port-forward setup + teardown.
#    --kubernetes: without it, a Cortex installed on this machine answers
#    first and agentop shows the laptop's sessions instead of the cluster's.
"$AGENTOP_BIN" observe --kubernetes
