#!/usr/bin/env bash
# Run the coding-agent demo on this machine.
#
#   ./run.sh up [--claude-code] [--custody]
#                                start Cortex and the mock services
#   ./run.sh scenarios           run every scenario (needs `up`)
#   ./run.sh walkthrough [--fast|--pace N]  the narrated run (needs `up`)
#   ./run.sh check               check scenario lists, captions, and their tests
#   ./run.sh down                stop what `up` started
#   ./run.sh status
#
# --claude-code allows model API, package registry, and source control hosts.
# See claude-code/README.md. --custody starts Vault (docker-compose.yml)
# to hold the support credential. Cortex retrieves it only for approved
# reports. Custody requires a Cortex build linked against CPEX
# 0.3.1 or later.
#
# Listeners use loopback ports 4770x and separate state under .state/.
# The demo leaves ~/.cortex and installed services unchanged. CORTEX_BIN
# selects the binary; otherwise use a `cortex` on PATH reporting
# `cortex (cpex)`, then bin/cortex-cpex/cortex from `make cortex-cpex`.

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
# Match the command's demo path to avoid acting on a reused PID.
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

# Refuse to start if another process holds a demo port.
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

# Generate the developer key pair and JWT. Cortex holds the token;
# policy verifies it with the public key.
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
  # Prepare the workspace with .env and two report fixtures.
  cp "$DIR/fixtures/env.fixture" "$WORKSPACE/.env"
  cp "$DIR/fixtures/report-clean.json" "$DIR/fixtures/report-with-key.json" "$WORKSPACE/"
  # Restrict commands to the proxy and workspace; deny reads of identity files.
  jq -n --arg ws "$WORKSPACE" --arg id "$IDENTITY_DIR" --arg st "$STATE_DIR" '{
    network: {allowedDomains: [], deniedDomains: [], httpProxyPort: 47700, socksProxyPort: 47700},
    filesystem: {denyRead: ["~/.ssh", $id, ($st + "/mock-ca"), ($st + "/support-token")],
                 allowWrite: [$ws], denyWrite: []}
  }' >"$SRT_SETTINGS"
}

# Derive Claude Code config from config/. Inspect model API traffic and
# allow package registries and source control hosts without inspection.
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

# --- credential custody ----------------------------------------------------

compose() { VAULT_ROOT_TOKEN="$(cat "$STATE_DIR/vault-root-token")" docker compose -f "$DIR/docker-compose.yml" "$@"; }

# vault <method> <path>: a root-token call, body on stdin.
vault() {
  curl -fsS -X "$1" -H "X-Vault-Token: $(cat "$STATE_DIR/vault-root-token")" \
    -H 'content-type: application/json' --data-binary @- "$VAULT_ADDR/v1/$2"
}

# Vault JWT auth trusts the developer key and limits reads to agents/<sub>/*.
# Store the support credential under dev-alice. Scenarios count audited
# requests from the container's stdout.
seed_vault() {
  local accessor
  curl -fsS -H "X-Vault-Token: $(cat "$STATE_DIR/vault-root-token")" "$VAULT_ADDR/v1/sys/auth" \
    | jq -e '.data["jwt/"]' >/dev/null || echo '{"type":"jwt"}' | vault POST sys/auth/jwt >/dev/null
  jq -n --rawfile k "$IDENTITY_DIR/developer.pub.pem" \
    '{jwt_validation_pubkeys: [$k], bound_issuer: "https://idp.coding-agent-demo.invalid"}' \
    | vault POST auth/jwt/config >/dev/null
  accessor="$(curl -fsS -H "X-Vault-Token: $(cat "$STATE_DIR/vault-root-token")" "$VAULT_ADDR/v1/sys/auth" \
    | jq -er '.data["jwt/"].accessor')"
  printf 'path "secret/data/agents/{{identity.entity.aliases.%s.name}}/*" {\n  capabilities = ["read"]\n}\n' "$accessor" \
    | jq -Rs '{policy: .}' | vault PUT sys/policies/acl/support-reader >/dev/null
  jq -n '{role_type: "jwt", user_claim: "sub", bound_audiences: ["cortex-coding-agent-demo"],
          token_policies: ["support-reader"], token_ttl: "5m", token_max_ttl: "5m"}' \
    | vault POST auth/jwt/role/developer >/dev/null
  jq -n --rawfile t "$STATE_DIR/support-token" '{data: {token: ($t | rtrimstr("\n"))}}' \
    | vault PUT secret/data/agents/dev-alice/support >/dev/null
  curl -fsS -H "X-Vault-Token: $(cat "$STATE_DIR/vault-root-token")" "$VAULT_ADDR/v1/sys/audit" \
    | jq -e '.data["stdout/"]' >/dev/null \
    || echo '{"type":"file","options":{"file_path":"stdout"}}' | vault PUT sys/audit/stdout >/dev/null
  info "seeded Vault: jwt auth for the developer key, the support credential for dev-alice"
}

start_vault() {
  command -v docker >/dev/null || die "docker is required for --custody"
  [ -f "$STATE_DIR/vault-root-token" ] || (umask 077; openssl rand -hex 24 >"$STATE_DIR/vault-root-token")
  compose up -d --quiet-pull >/dev/null 2>&1 || compose up -d
  wait_for "Vault" "$VAULT_ADDR/v1/sys/health"
  seed_vault
}

# Add custody settings to the selected runtime config.
gen_custody_config() {
  local src="$1" out="$STATE_DIR/custody"
  mkdir -p "$out"
  python3 - "$src" "$out" "$VAULT_ADDR" <<'PY'
import re, sys
from pathlib import Path
src, out, vault_addr = Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3]

def sub(text, old, new, name):
    if text.count(old) != 1:
        sys.exit(f"run.sh: expected one {old!r} in {name}; update gen_custody_config")
    return text.replace(old, new)

cfg = (src / "cortex.yaml").read_text()
cfg, n = re.subn(r'config_file: "[^"]*cpex-policy\.yaml"', 'config_file: "${DEMO_STATE}/custody/cpex-policy.yaml"', cfg)
if n != 1:
    sys.exit("run.sh: expected one cpex config_file in cortex.yaml; update gen_custody_config")
cfg = sub(cfg, "          fail_open: false\n", """          fail_open: false
          # Retrieve credentials from loopback Vault after policy approval.
          private_destinations: true
          delegated_credentials: true
          # Send support credentials only to the support service.
          delegated_credential_hosts:
            support: [support.localtest.me]
""", "cortex.yaml")
(out / "cortex.yaml").write_text(cfg)

policy = (src / "cpex-policy.yaml").read_text()
policy = sub(policy, "      claim_mapper: standard\n", f"""      claim_mapper: standard

  # Retrieve agents/<sub>/support using the developer JWT supplied by Cortex.
  - name: support-vault
    kind: delegator/vault
    hooks: [token.delegate]
    on_error: fail
    capabilities: [perform_http, read_inbound_credentials, read_subject, read_claims, write_delegated_tokens]
    config:
      vault_addr: "{vault_addr}"
      insecure_http: true
      kv_mount: secret
      secret_path_template: "agents/{{{{identity}}}}/support"
      secret_field: token
      outbound_header: Authorization
      scheme_prefix: "Bearer "
      timeout_seconds: 3
      auth:
        user:
          method: jwt
          identity_claim: sub
          mount: jwt
          role: developer
""", "cpex-policy.yaml")
# The route may close on the policy slide's end marker.
policy = re.sub(r"\n[ ]*# end-slide\s*$", "", policy.rstrip()) + "\n"
if not policy.rstrip().endswith("on_deny: [\"deny('project not authorized', 'project_not_authorized')\"]"):
    sys.exit("run.sh: the http_egress route no longer ends the policy; update gen_custody_config")
policy = policy.rstrip("\n") + """
        # Delegate only after all earlier policy steps pass.
        - when: "args.method != 'CONNECT' & args.host == 'support.localtest.me'"
          do:
            - "delegate(support-vault, target: support, audience: support.localtest.me, subject: user)"
"""
(out / "cpex-policy.yaml").write_text(policy)
PY
  info "generated the custody config (.state/custody/)"
}

# --- commands --------------------------------------------------------------

service_pid() { listener 47600; }

up() {
  local bin before config="$DIR/config/cortex.yaml" claude_code=0 custody=0 opt
  for opt in "$@"; do
    case "$opt" in
      --claude-code) claude_code=1 ;;
      --custody) custody=1 ;;
      *) die "unknown option to up: $opt" ;;
    esac
  done
  check_tools
  bin="$(find_cortex)"
  before="$(service_pid)"
  check_ports
  prepare_state
  if [ "$claude_code" = 1 ]; then
    gen_claude_code_config
    config="$STATE_DIR/claude-code/cortex.yaml"
  fi
  rm -f "$CUSTODY_MARK"
  if [ "$custody" = 1 ]; then
    gen_custody_config "$(dirname "$config")"
    config="$STATE_DIR/custody/cortex.yaml"
    start_vault
  fi
  info "cortex: $bin ($("$bin" --version))"

  start mock-llm python3 "$DIR/mocks/llm/server.py" --port "$LLM_PORT" \
    --cert "$STATE_DIR/mock-ca/llm.crt" --key "$STATE_DIR/mock-ca/llm.key"
  SUPPORT_TOKEN="$(cat "$STATE_DIR/support-token")" start mock-support python3 "$DIR/mocks/support/server.py" \
    --port "$SUPPORT_PORT" --cert "$STATE_DIR/mock-ca/support.crt" --key "$STATE_DIR/mock-ca/support.key"
  # Resolve policy paths from config/ and keep Cortex's HOME under .state/.
  (cd "$DIR/config" && HOME="$STATE_DIR/home" start cortex "$bin" --config "$config")

  wait_for "the mock LLM" "$LLM_URL/_requests" --cacert "$MOCK_CA"
  wait_for "the mock support service" "$SUPPORT_URL/_requests" --cacert "$MOCK_CA"
  wait_for "cortex" "$HEALTH_URL/readyz"
  if [ "$custody" = 1 ]; then
    # Require the support credential when custody is enabled.
    curl -fsS --cacert "$MOCK_CA" -X POST -d '{"credential_required": true}' "$SUPPORT_URL/_config" >/dev/null
    touch "$CUSTODY_MARK"
    info "custody on: Vault at $VAULT_ADDR, the support service requires the credential"
  fi
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
  rm -f "$CUSTODY_MARK"
  if [ -f "$STATE_DIR/vault-root-token" ] && command -v docker >/dev/null \
    && [ -n "$(compose ps -q 2>/dev/null)" ]; then
    compose down >/dev/null 2>&1 && info "stopped Vault"
  fi
  [ "$(service_pid)" = "$before" ] || die "the process on :47600 changed while stopping the demo"
}

status() {
  local name
  for name in cortex mock-llm mock-support; do
    if alive "$name"; then info "$name running (pid $(pid_of "$name"))"; else info "$name stopped"; fi
  done
  if [ -f "$CUSTODY_MARK" ]; then info "custody on (Vault at $VAULT_ADDR)"; fi
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
  *) sed -n '2,23p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
