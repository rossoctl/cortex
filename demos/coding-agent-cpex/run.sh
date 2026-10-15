#!/usr/bin/env bash
# Run the coding-agent demo on this machine.
#
#   ./run.sh up [--claude-code]  start the demo cortex and the mock services
#   ./run.sh scenarios           run every scenario (needs `up`)
#   ./run.sh walkthrough [--fast|--pace N]  the narrated run (needs `up`)
#   ./run.sh check               the walkthrough's drift guard and its test
#   ./run.sh down                stop what `up` started
#   ./run.sh status
#
# --claude-code also lets the hosts a real Claude Code session needs
# through; see claude-code/README.md.
#
# Everything runs on 4770x and loopback, from its own config and state
# directory (.state/). It never touches ~/.cortex or an installed cortex
# service. CORTEX_BIN picks the binary; otherwise a `cortex` on PATH that
# reports `cortex (cpex)`, then bin/cortex-cpex/cortex from `make cortex-cpex`.

set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$DIR/../.." && pwd)"
# shellcheck source=env.sh
source "$DIR/env.sh"

PIDS="$STATE_DIR/pids"
LOGS="$STATE_DIR/logs"

die() { echo "run.sh: $*" >&2; exit 1; }
info() { echo "  ▸ $*"; }

# --- prerequisites ---------------------------------------------------------

is_cpex() { [ -x "$1" ] && "$1" --version 2>/dev/null | grep -q '^cortex (cpex) '; }

find_cortex() {
  local c
  if [ -n "${CORTEX_BIN:-}" ]; then
    is_cpex "$CORTEX_BIN" || die "CORTEX_BIN=$CORTEX_BIN does not report \`cortex (cpex)\`; build one with \`make cortex-cpex\` or install one with \`install.sh --cpex\`"
    echo "$CORTEX_BIN"; return
  fi
  c="$(command -v cortex 2>/dev/null || true)"
  if [ -n "$c" ] && is_cpex "$c"; then echo "$c"; return; fi
  if is_cpex "$REPO/bin/cortex-cpex/cortex"; then echo "$REPO/bin/cortex-cpex/cortex"; return; fi
  die "no cpex build of cortex found. Build one with \`make cortex-cpex\` (from $REPO), install one with \`install.sh --cpex\`, or set CORTEX_BIN."
}

check_tools() {
  local t
  for t in python3 openssl curl jq lsof gzip; do
    command -v "$t" >/dev/null || die "$t is required"
  done
  command -v srt >/dev/null || die "srt (Anthropic sandbox runtime) is required: npm install -g @anthropic-ai/sandbox-runtime"
}

# --- processes -------------------------------------------------------------

pid_of() { cat "$PIDS/$1.pid" 2>/dev/null || true; }
# A pid file counts only while its process is still one this demo started:
# every one runs with a path under this directory on its command line.
alive() {
  local p; p="$(pid_of "$1")"
  [ -n "$p" ] && ps -o command= -p "$p" 2>/dev/null | grep -qF "$DIR/"
}
our_pids() {
  local f
  for f in "$PIDS"/*.pid; do
    [ -e "$f" ] && alive "$(basename "$f" .pid)" && pid_of "$(basename "$f" .pid)"
  done
  return 0
}
listener() { lsof -nP -iTCP:"$1" -sTCP:LISTEN -t 2>/dev/null | head -1 || true; }

# Refuse to start when a process this demo did not start holds a demo port.
check_ports() {
  local p pid ours
  ours=" $(our_pids | tr '\n' ' ') "
  for p in $CORTEX_PORTS $LLM_PORT $SUPPORT_PORT; do
    pid="$(listener "$p")"
    if [ -n "$pid" ] && [[ "$ours" != *" $pid "* ]]; then
      die "port $p is held by pid $pid ($(ps -o comm= -p "$pid" 2>/dev/null)), which this demo did not start"
    fi
  done
}

start() {
  local name="$1"; shift
  if alive "$name"; then info "$name already running (pid $(pid_of "$name"))"; return; fi
  nohup "$@" >"$LOGS/$name.log" 2>&1 &
  echo $! >"$PIDS/$name.pid"
  info "$name started (pid $!, log .state/logs/$name.log)"
}

wait_for() {
  local what="$1" url="$2"
  for _ in $(seq 1 60); do
    curl -fsS --max-time 2 -o /dev/null "${@:3}" "$url" 2>/dev/null && return 0
    sleep 0.5
  done
  die "$what did not come up at $url; see .state/logs/"
}

# --- generated material ----------------------------------------------------

b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }

gen_mock_certs() {
  local d="$STATE_DIR/mock-ca" h
  [ -f "$d/ca.crt" ] && return
  mkdir -p "$d" && chmod 700 "$d"
  openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=coding-agent-demo mock CA" \
    -keyout "$d/ca.key" -out "$d/ca.crt" 2>/dev/null
  for h in llm support; do
    openssl req -newkey rsa:2048 -nodes -subj "/CN=$h.localtest.me" \
      -keyout "$d/$h.key" -out "$d/$h.csr" 2>/dev/null
    printf 'subjectAltName=DNS:%s.localtest.me\n' "$h" >"$d/$h.ext"
    openssl x509 -req -in "$d/$h.csr" -CA "$d/ca.crt" -CAkey "$d/ca.key" -CAcreateserial \
      -days 30 -extfile "$d/$h.ext" -out "$d/$h.crt" 2>/dev/null
  done
  chmod 600 "$d"/*.key
  info "generated the mock services' certificates"
}

# The developer's key pair and a token signed with it. Cortex holds the
# token; the policy verifies it with the public key.
gen_identity() {
  local d="$IDENTITY_DIR" now hdr payload sig
  [ -f "$d/developer.jwt" ] && return
  mkdir -p "$d" && chmod 700 "$d"
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$d/developer.key" 2>/dev/null
  chmod 600 "$d/developer.key"
  openssl pkey -in "$d/developer.key" -pubout -out "$d/developer.pub.pem"
  now="$(date +%s)"
  hdr="$(printf '{"alg":"RS256","typ":"JWT"}' | b64url)"
  payload="$(printf '{"iss":"https://idp.coding-agent-demo.invalid","aud":"cortex-coding-agent-demo","sub":"dev-alice","preferred_username":"alice","teams":["A"],"iat":%s,"exp":%s}' \
    "$now" "$((now + 30 * 86400))" | b64url)"
  sig="$(printf '%s.%s' "$hdr" "$payload" | openssl dgst -sha256 -sign "$d/developer.key" | b64url)"
  (umask 077; printf '%s.%s.%s' "$hdr" "$payload" "$sig" >"$d/developer.jwt")
  info "generated the developer identity (config/identity/)"
}

prepare_state() {
  mkdir -p "$STATE_DIR" "$PIDS" "$LOGS" "$STATE_DIR/home" "$WORKSPACE" "$STATE_DIR/runs"
  chmod 700 "$STATE_DIR"
  [ -f "$STATE_DIR/support-token" ] || (umask 077; openssl rand -hex 24 >"$STATE_DIR/support-token")
  gen_mock_certs
  gen_identity
  # The agent's working tree: a project with a .env and two reports.
  cp "$DIR/fixtures/env.fixture" "$WORKSPACE/.env"
  cp "$DIR/fixtures/report-clean.json" "$DIR/fixtures/report-with-key.json" "$WORKSPACE/"
  # The sandbox the agent's commands run in: network only through the
  # demo cortex, writes only in the workspace, no reading the identity.
  jq -n --arg ws "$WORKSPACE" --arg id "$IDENTITY_DIR" --arg st "$STATE_DIR" '{
    network: {allowedDomains: [], deniedDomains: [], httpProxyPort: 47700, socksProxyPort: 47700},
    filesystem: {denyRead: ["~/.ssh", $id, ($st + "/mock-ca"), ($st + "/support-token")],
                 allowWrite: [$ws], denyWrite: []}
  }' >"$SRT_SETTINGS"
}

# For a real Claude Code session: its model API joins the policy's
# allowlist, and package registries and VCS hosts are allowed and tunnelled
# unopened, so installs and clones work. Generated from config/, so the
# scripted policy stays the one source.
CLAUDE_CODE_LLM_HOSTS="${CLAUDE_CODE_LLM_HOSTS:-api.anthropic.com}"
CLAUDE_CODE_PASSTHROUGH_HOSTS="${CLAUDE_CODE_PASSTHROUGH_HOSTS:-registry.npmjs.org pypi.org files.pythonhosted.org proxy.golang.org sum.golang.org github.com codeload.github.com objects.githubusercontent.com}"

gen_claude_code_config() {
  local out="$STATE_DIR/claude-code"
  mkdir -p "$out"
  python3 - "$DIR/config" "$out" "$CLAUDE_CODE_LLM_HOSTS" "$CLAUDE_CODE_PASSTHROUGH_HOSTS" <<'PY'
import json, sys
from pathlib import Path
src, out, llm, passthrough = Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3].split(), sys.argv[4].split()

def sub(text, old, new):
    if text.count(old) != 1:
        sys.exit(f"run.sh: expected one {old!r} in {src}; update gen_claude_code_config")
    return text.replace(old, new)

allowed = '["support.localtest.me", "llm.localtest.me"]'
policy = sub((src / "cpex-policy.yaml").read_text(), f"allowed: {allowed}",
             f"allowed: {json.dumps(json.loads(allowed) + llm + passthrough)}")
(out / "cpex-policy.yaml").write_text(policy)
cfg = sub((src / "cortex.yaml").read_text(), "passthrough_hosts: []", f"passthrough_hosts: {json.dumps(passthrough)}")
cfg = sub(cfg, '"${DEMO_DIR}/config/cpex-policy.yaml"', '"${DEMO_STATE}/claude-code/cpex-policy.yaml"')
(out / "cortex.yaml").write_text(cfg)
PY
  info "generated the Claude Code config (.state/claude-code/)"
}

# --- commands --------------------------------------------------------------

service_pid() { listener 47600; }

up() {
  local bin before config="$DIR/config/cortex.yaml"
  check_tools
  bin="$(find_cortex)"
  before="$(service_pid)"
  check_ports
  prepare_state
  if [ "${1:-}" = "--claude-code" ]; then
    gen_claude_code_config
    config="$STATE_DIR/claude-code/cortex.yaml"
  elif [ -n "${1:-}" ]; then
    die "unknown option to up: $1"
  fi
  info "cortex: $bin ($("$bin" --version))"

  start mock-llm python3 "$DIR/mocks/llm/server.py" --port "$LLM_PORT" \
    --cert "$STATE_DIR/mock-ca/llm.crt" --key "$STATE_DIR/mock-ca/llm.key"
  SUPPORT_TOKEN="$(cat "$STATE_DIR/support-token")" start mock-support python3 "$DIR/mocks/support/server.py" \
    --port "$SUPPORT_PORT" --cert "$STATE_DIR/mock-ca/support.crt" --key "$STATE_DIR/mock-ca/support.key"
  # cortex runs from config/ so the policy's relative key path resolves,
  # and with HOME inside .state so nothing it writes lands in ~/.cortex.
  (cd "$DIR/config" && HOME="$STATE_DIR/home" start cortex "$bin" --config "$config")

  wait_for "the mock LLM" "$LLM_URL/_requests" --cacert "$MOCK_CA"
  wait_for "the mock support service" "$SUPPORT_URL/_requests" --cacert "$MOCK_CA"
  wait_for "cortex" "$HEALTH_URL/readyz"
  curl -fsS "$SESSION_API/v1/plugins" | jq -e '.plugins[] | select(.name == "cpex")' >/dev/null \
    || die "cortex is up but does not list the cpex plugin"
  info "cortex ready: forward proxy $DEMO_PROXY, session API $SESSION_API"
  [ "$(service_pid)" = "$before" ] || die "the process on :47600 changed while starting the demo"
  [ -z "$before" ] || info "installed cortex service on :47600 (pid $before) untouched"
}

down() {
  local name pid before
  before="$(service_pid)"
  for name in cortex mock-support mock-llm; do
    pid="$(pid_of "$name")"
    if alive "$name"; then
      kill "$pid" && info "stopped $name (pid $pid)"
    fi
    rm -f "$PIDS/$name.pid"
  done
  [ "$(service_pid)" = "$before" ] || die "the process on :47600 changed while stopping the demo"
}

status() {
  local name
  for name in cortex mock-llm mock-support; do
    if alive "$name"; then info "$name running (pid $(pid_of "$name"))"; else info "$name stopped"; fi
  done
}

scenarios() {
  local s passed=() failed=()
  alive cortex || die "the demo is not up; run ./run.sh up"
  for s in "$DIR"/scenarios/[0-9][0-9]-*.sh; do
    if bash "$s"; then passed+=("$(basename "$s")"); else failed+=("$(basename "$s")"); fi
  done
  echo
  for s in ${passed[@]+"${passed[@]}"}; do printf '  \033[1;32m✓\033[0m %s\n' "$s"; done
  for s in ${failed[@]+"${failed[@]}"}; do printf '  \033[1;31m✗\033[0m %s\n' "$s"; done
  echo "  ${#passed[@]} passed, ${#failed[@]} failed"
  [ "${#failed[@]}" -eq 0 ]
}

check() {
  python3 "$DIR/demo/record_demo.py" --check
  (cd "$DIR/demo" && python3 -m unittest -q test_record_demo)
}

case "${1:-}" in
  up) up "${@:2}" ;;
  walkthrough) alive cortex || die "the demo is not up; run ./run.sh up"; "$DIR/walkthrough.sh" "${@:2}" ;;
  check) check ;;
  down) down ;;
  status) status ;;
  scenarios) scenarios ;;
  *) sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
