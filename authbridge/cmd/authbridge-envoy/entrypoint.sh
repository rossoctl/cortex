#!/bin/bash
set -eu

# AuthBridge envoy-sidecar combined entrypoint with process supervision.
# Manages: spiffe-helper (optional), authbridge-envoy (ext_proc), envoy.
#
# Startup order:
#   1. spiffe-helper (background, only when SPIRE_ENABLED=true)
#   2. authbridge-envoy (background) — gRPC ext_proc listener
#   3. envoy (background) — calls authbridge-envoy over ext_proc
#
# Process management: PID 1 (this shell) supervises every long-running
# critical process. If any critical process exits, the others are killed
# and the container exits non-zero so Kubernetes restarts it. SIGTERM /
# SIGINT are forwarded for graceful shutdown.

CRITICAL_PIDS=""

cleanup() {
  echo "[entrypoint] Received signal, shutting down..."
  # shellcheck disable=SC2086
  kill $CRITICAL_PIDS 2>/dev/null || true
  wait
  exit 0
}
trap cleanup TERM INT

# --- Phase 1: spiffe-helper (conditional) ---
if [ "${SPIRE_ENABLED:-}" = "true" ]; then
  echo "[entrypoint] Starting spiffe-helper..."
  /usr/local/bin/spiffe-helper -config=/etc/spiffe-helper/helper.conf run &
  CRITICAL_PIDS="$CRITICAL_PIDS $!"
fi

# --- Phase 2: authbridge-envoy (ext_proc gRPC server) ---
echo "[entrypoint] Starting authbridge-envoy..."
/usr/local/bin/authbridge-envoy "$@" &
CRITICAL_PIDS="$CRITICAL_PIDS $!"

# Give authbridge-envoy a moment to bind the gRPC listener before Envoy connects
sleep 2

# --- Phase 3: Envoy ---
echo "[entrypoint] Starting Envoy..."
/usr/local/bin/envoy -c /etc/envoy/envoy.yaml \
  --service-cluster auth-proxy --service-node auth-proxy &
CRITICAL_PIDS="$CRITICAL_PIDS $!"

# Block until any critical process exits, then terminate the container
# so Kubernetes restarts the pod.
# shellcheck disable=SC2086
wait -n $CRITICAL_PIDS
EXIT_CODE=$?
echo "[entrypoint] A critical process exited unexpectedly (exit code $EXIT_CODE), terminating container"
# shellcheck disable=SC2086
kill $CRITICAL_PIDS 2>/dev/null || true
wait
exit 1
