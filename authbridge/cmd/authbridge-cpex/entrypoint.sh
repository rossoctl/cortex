#!/bin/bash
set -euo pipefail

# AuthBridge CPEX-enabled sidecar entrypoint with process supervision.
# Manages: authbridge-cpex (authbridge-proxy + CPEX plugin).
#
# Identical to cmd/authbridge-proxy/entrypoint.sh except for the
# binary name; promote to a shared script when one is extracted.

CRITICAL_PIDS=""

cleanup() {
  echo "[entrypoint] Received signal, shutting down..."
  # shellcheck disable=SC2086
  kill $CRITICAL_PIDS 2>/dev/null || true
  wait
  exit 0
}
trap cleanup TERM INT

echo "[entrypoint] Starting authbridge-cpex..."
/usr/local/bin/authbridge-cpex "$@" &
CRITICAL_PIDS="$CRITICAL_PIDS $!"

# shellcheck disable=SC2086
wait -n $CRITICAL_PIDS
EXIT_CODE=$?
echo "[entrypoint] A critical process exited unexpectedly (exit code $EXIT_CODE), terminating container"
# shellcheck disable=SC2086
kill $CRITICAL_PIDS 2>/dev/null || true
wait
exit 1
