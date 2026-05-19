#!/bin/bash
# Launch abctl against the IBAC agent's authbridge sidecar.
#
# Handles the boring orchestration so `make show-result` is one
# step:
#
#   1. Build abctl if it isn't already cached (~5s first run, instant
#      after).
#   2. Detect whether something is already listening on :9094 (the
#      session API port). If not, start `kubectl port-forward` in the
#      background and tear it down when abctl exits.
#   3. Run abctl in the foreground. User quits with `q` or Ctrl-C;
#      the trap cleans up the port-forward.

set -uo pipefail

NAMESPACE=${1:-team1}
AGENT_NAME=${2:-ibac-agent}
ABCTL_BIN=${ABCTL_BIN:-/tmp/abctl-ibac-demo}

# 1. The Makefile's build-abctl target builds the binary on demand.
#    If it's still missing, the user invoked the script directly —
#    point them at the right entry point instead of trying to build
#    in here (which would couple this script to the go toolchain
#    layout in their dev environment).
if [[ ! -x "$ABCTL_BIN" ]]; then
  echo "ERROR: abctl binary not found at $ABCTL_BIN." >&2
  echo "       Run \`make show-result\` (which depends on build-abctl)" >&2
  echo "       or \`make build-abctl\` first." >&2
  exit 1
fi

# 2. Verify the agent pod is up before any port-forward attempt.
if ! kubectl -n "$NAMESPACE" get deploy "$AGENT_NAME" >/dev/null 2>&1; then
  echo "ERROR: deployment $NAMESPACE/$AGENT_NAME not found." >&2
  echo "       Run 'make demo-ibac' first." >&2
  exit 1
fi

# 3. Port-forward orchestration.
#    - If something is already listening on :9094, assume the user
#      set up the forward themselves and use it.
#    - Otherwise start one we own and clean up on exit.
PF_PID=""
cleanup() {
  if [[ -n "$PF_PID" ]]; then
    kill "$PF_PID" 2>/dev/null || true
    wait 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

PORT_IN_USE=0
if command -v lsof >/dev/null 2>&1; then
  if lsof -nP -iTCP:9094 -sTCP:LISTEN >/dev/null 2>&1; then
    PORT_IN_USE=1
  fi
elif command -v ss >/dev/null 2>&1; then
  if ss -ltn '( sport = :9094 )' 2>/dev/null | grep -q ':9094'; then
    PORT_IN_USE=1
  fi
fi

if [[ "$PORT_IN_USE" == "1" ]]; then
  echo "[*] Reusing existing listener on :9094 (assumed your port-forward)."
else
  echo "[*] Starting port-forward to deploy/$AGENT_NAME on :9094 ..."
  kubectl -n "$NAMESPACE" port-forward deploy/"$AGENT_NAME" 9094:9094 >/dev/null 2>&1 &
  PF_PID=$!
  # Wait for the listener to come up before launching the TUI.
  for _ in 1 2 3 4 5 6 7 8; do
    if curl -sf http://localhost:9094/healthz >/dev/null 2>&1; then
      break
    fi
    sleep 0.5
  done
  if ! curl -sf http://localhost:9094/healthz >/dev/null 2>&1; then
    echo "ERROR: port-forward never came up. Last kubectl output:" >&2
    kubectl -n "$NAMESPACE" port-forward deploy/"$AGENT_NAME" 9094:9094 2>&1 | head -5 >&2 || true
    exit 1
  fi
fi

# 4. Run abctl in the foreground. Important: do NOT use `exec` — it
#    replaces the shell process, which would kill our cleanup trap
#    and leak the kubectl port-forward we just started. Run as a
#    child so the trap fires on abctl's exit.
"$ABCTL_BIN"
exit_code=$?
exit "$exit_code"
