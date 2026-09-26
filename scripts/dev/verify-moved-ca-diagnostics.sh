#!/usr/bin/env bash
# Verify the moved-CA diagnostics from issue #1033 on a real machine.
#
# The bug needs two $HOME values and a client process that outlives a CA swap, so
# the Go tests cover the logic but cannot reproduce the symptom. This does, using
# two throwaway HOMEs under a temp dir — it never touches your real ~/.cortex, and
# it starts its proxy on an unused high port so a running Cortex is unaffected.
#
# Usage:  scripts/dev/verify-moved-ca-diagnostics.sh [path-to-authbridge-proxy]
#
# With no argument it builds the binary from this checkout.
set -euo pipefail

PORT="${PORT:-47690}"   # deliberately not 47600; must not disturb a real install
HOST="${HOST:-example.com}"

cd "$(dirname "$0")/../.."   # repo root

WORK="$(mktemp -d)"
# Kill the proxy BEFORE removing the tree: the config lives under $WORK and the
# proxy watches it for hot-reload, so deleting it out from under a live watcher
# first is a self-inflicted error in a script whose job is to be trustworthy.
# Every step is `|| true`: with PROXY_PID unset, `wait ""` fails, and under set -e
# that aborted the handler before the rm — so the script leaked its temp dir on the
# go-build and --write-config failure paths. (The `[[ ]] && kill` form was already
# safe: AND-OR lists are exempt from set -e. The bare `wait` was not.)
cleanup() {
  if [[ -n "${PROXY_PID:-}" ]]; then
    kill "$PROXY_PID" 2>/dev/null || true
    wait "$PROXY_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT
HOME_A="$WORK/home-a"   # stands in for your real $HOME
HOME_B="$WORK/home-b"   # stands in for a sandbox's redirected $HOME
mkdir -p "$HOME_A" "$HOME_B"

BIN="${1:-}"
if [[ -z "$BIN" ]]; then
  echo "==> building authbridge-proxy"
  # Inside $WORK so cleanup removes it; a separate mktemp -d would leak a
  # directory on every run.
  BIN="$WORK/authbridge-proxy"
  # Plugins are all opt-in build tags, and the built-in --local config names four
  # of them. Without the `local` profile's tags the binary starts, fails to build
  # its pipeline, and exits — so the tags are required, not an optimisation.
  go build -tags "$(go -C scripts/profile-tags run . local)" -o "$BIN" ./cmd/authbridge-proxy
fi

# start_proxy <home> [ca-dir] — boots against that HOME on our own ports.
#
# There is no flag for the listener addresses: --local pins them (47600-47604) via
# its built-in config. So use --write-config to materialise that config, rewrite
# every 476NN port into our own range, and start from it with --config. That keeps
# this script off the ports a real install is using.
start_proxy() {
  local home="$1" cadir="${2:-}" tag
  tag="$(basename "$home")${cadir:+-cadir}"
  local args=(--local --write-config)
  [[ -n "$cadir" ]] && args+=(--ca-dir "$cadir")
  HOME="$home" "$BIN" "${args[@]}" >"$WORK/write-$tag.log" 2>&1 || {
    echo "FAIL: --write-config failed" >&2; cat "$WORK/write-$tag.log" >&2; exit 1; }

  local cfg="$home/.cortex/config.yaml"
  # 47600->$PORT, and the sibling ports to $PORT+1.. so nothing collides either.
  local i
  for i in 0 1 2 3 4; do
    sed -i.bak "s/127\.0\.0\.1:4760$i/127.0.0.1:$((PORT + i))/g" "$cfg"
  done
  rm -f "$cfg.bak"

  HOME="$home" "$BIN" --config "$cfg" >"$WORK/proxy-$tag.log" 2>&1 &
  PROXY_PID=$!
  LAST_LOG="$WORK/proxy-$tag.log"
  for _ in $(seq 1 50); do
    nc -z 127.0.0.1 "$PORT" 2>/dev/null && return 0
    sleep 0.1
  done
  echo "FAIL: proxy did not open $PORT" >&2
  cat "$LAST_LOG" >&2
  exit 1
}

stop_proxy() {
  [[ -n "${PROXY_PID:-}" ]] || return 0
  kill "$PROXY_PID" 2>/dev/null || true
  wait "$PROXY_PID" 2>/dev/null || true
  PROXY_PID=""
}

fingerprint() {
  openssl x509 -in "$1" -noout -fingerprint -sha256 2>/dev/null | cut -d= -f2
}

echo "==> 1. boot against HOME_A, capture its CA"
start_proxy "$HOME_A"
CA_A="$HOME_A/.cortex/ca/ca.crt"
[[ -f "$CA_A" ]] || { echo "FAIL: no CA generated at $CA_A" >&2; exit 1; }
FP_A="$(fingerprint "$CA_A")"
echo "    HOME_A CA: $FP_A"

echo "==> 2. a client holding HOME_A's CA succeeds (the control)"
if curl -sS --max-time 10 --proxy "http://127.0.0.1:$PORT" --cacert "$CA_A" \
     -o /dev/null -w '    HTTP %{http_code} via HOME_A CA\n' "https://$HOST/"; then
  :
else
  echo "    NOTE: curl failed — needs outbound network to $HOST; the CA checks below still hold" >&2
fi

stop_proxy

echo "==> 3. reboot against HOME_B (the redirected-\$HOME case)"
start_proxy "$HOME_B"
CA_B="$HOME_B/.cortex/ca/ca.crt"
FP_B="$(fingerprint "$CA_B")"
echo "    HOME_B CA: $FP_B"

if [[ "$FP_A" == "$FP_B" ]]; then
  echo "FAIL: both HOMEs produced the same CA; the repro is not set up correctly" >&2
  exit 1
fi
echo "    -> two different CAs, and note both are named:"
openssl x509 -in "$CA_A" -noout -subject | sed 's/^/       A: /'
openssl x509 -in "$CA_B" -noout -subject | sed 's/^/       B: /'
echo "       (identical subjects — only the fingerprint tells them apart, which is the point)"

echo "==> 4. the stale client (still holding HOME_A's CA) is now rejected"
set +e
curl -sS --max-time 10 --proxy "http://127.0.0.1:$PORT" --cacert "$CA_A" \
  -o /dev/null "https://$HOST/" 2>"$WORK/curl-err.txt"
rc=$?
set -e
if [[ $rc -eq 0 ]]; then
  echo "FAIL: the stale CA was accepted; interception is not happening" >&2
  exit 1
fi
echo "    curl rejected it as expected:"
sed 's/^/       /' "$WORK/curl-err.txt"

echo "==> 5. the proxy log must name the CA, not just the cutoff"
LOG="$LAST_LOG"
grep -q 'client-rejected-ca' "$LOG" || {
  echo "FAIL: no client-rejected-ca in the log" >&2; tail -20 "$LOG" >&2; exit 1; }
for field in ca_not_before ca_fingerprint ca_file; do
  if grep 'client-rejected-ca' "$LOG" | grep -q "$field="; then
    echo "    OK   $field= present"
  else
    echo "    FAIL $field= missing — this is the #1033 diagnostic gap" >&2
    exit 1
  fi
done

echo "    the logged fingerprint must be HOME_B's (the CA actually in force):"
if grep 'client-rejected-ca' "$LOG" | grep -qF "$FP_B"; then
  echo "    OK   matches HOME_B  $FP_B"
else
  echo "    FAIL logged fingerprint is not HOME_B's" >&2
  grep 'client-rejected-ca' "$LOG" | tail -2 >&2
  exit 1
fi

echo "==> 6. --ca-dir is the fix: point HOME_B's proxy at HOME_A's CA"
stop_proxy
start_proxy "$HOME_B" "$HOME_A/.cortex/ca"
if [[ "$(fingerprint "$HOME_A/.cortex/ca/ca.crt")" == "$FP_A" ]]; then
  echo "    OK   HOME_A's CA reused unchanged ($FP_A) — the client that trusts it keeps working"
else
  echo "    FAIL --ca-dir regenerated the CA it was pointed at" >&2
  exit 1
fi

echo
echo "PASS: the rejection now names the CA in force, and --ca-dir shares one CA across HOMEs."
