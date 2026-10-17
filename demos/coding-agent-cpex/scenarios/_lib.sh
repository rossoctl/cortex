# shellcheck shell=bash
# Shared helpers for the scenarios. Source from each script:
#
#   source "$(dirname "$0")/_lib.sh"
#
# Scenarios check tool results, the Cortex session timeline, and mock request
# logs. Start the demo with run.sh up first.

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=../env.sh
source "$DEMO_DIR/env.sh"

# Fixture secret and developer token used by the leak checks below.
SECRET="AKIAIOSFODNN7EXAMPLE"
DEV_JWT="$(cat "$IDENTITY_DIR/developer.jwt" 2>/dev/null)"

# step <title>: print the header and title the next run_agent's session.
step() {
  SESSION_TITLE="$*"
  if presenter; then
    # The caption already names the step; print only a separator.
    outcome_flush
    echo
    return
  fi
  echo
  echo "============================================================"
  echo "$@"
  echo "============================================================"
}

# PRESENTER=1 summarizes passing checks and prints failures in full.
# All checks still run.
presenter() { [ "${PRESENTER:-0}" = 1 ]; }

note() { presenter || echo "  ▸ $*"; }

FAILURES=0
CHECKS=0
OUTCOME_REFUSED=()
OUTCOME_CODES=()
OUTCOME_ALLOWED=()
OUTCOME_FACTS=()
pass_() { CHECKS=$((CHECKS + 1)); presenter || printf '  \033[1;32m✓\033[0m %s\n' "$*"; }
fail_() { printf '  \033[1;31m✗\033[0m %s\n' "$*"; FAILURES=$((FAILURES + 1)); }

# Collect denial codes, allowed status codes, and delivery results for the summary.
outcome_refused() {
  case "$1" in
    cpex.*) _outcome_add OUTCOME_CODES "${1#cpex.}" ;;
    *) _outcome_add OUTCOME_REFUSED "$1" ;;
  esac
}
outcome_allowed() { _outcome_add OUTCOME_ALLOWED "$1"; }
outcome_fact() { _outcome_add OUTCOME_FACTS "$1"; }
_outcome_add() {
  local arr="$1[@]" x
  for x in ${!arr+"${!arr}"}; do [ "$x" = "$2" ] && return 0; done
  eval "$1+=(\"\$2\")"
}

# join <sep> <items...>: the items joined by sep.
join() {
  local sep="$1" out="" x
  shift
  for x in "$@"; do out+="${out:+$sep}$x"; done
  printf '%s' "$out"
}

# outcome_flush: print and reset the current step's summary.
outcome_flush() {
  presenter || return 0
  local parts=() line mark
  if [ "${#OUTCOME_CODES[@]}" -gt 0 ]; then
    mark='\033[1;31m✗\033[0m'
    line="refused by Cortex · $(join ', ' "${OUTCOME_CODES[@]}")"
  elif [ "${#OUTCOME_REFUSED[@]}" -gt 0 ]; then
    mark='\033[1;31m✗\033[0m'
    line="$(join ', ' "${OUTCOME_REFUSED[@]}")"
  elif [ "${#OUTCOME_ALLOWED[@]}" -gt 0 ]; then
    mark='\033[1;32m✓\033[0m'
    line="allowed · $(join ', ' "${OUTCOME_ALLOWED[@]}")"
  elif [ "$CHECKS" -gt 0 ]; then
    mark='\033[1;32m✓\033[0m'
    line="as expected"
  else
    return 0
  fi
  # Two lines, to fit a narrow pane: the verdict, then what arrived.
  local n="$CHECKS checks"; [ "$CHECKS" = 1 ] && n="1 check"
  parts=(${OUTCOME_FACTS[@]+"${OUTCOME_FACTS[@]}"} "$n passed")
  printf '\n  %b \033[1m%s\033[0m\n' "$mark" "$line"
  printf '    \033[2m%s\033[0m\n' "$(join ' · ' "${parts[@]}")"
  OUTCOME_REFUSED=(); OUTCOME_CODES=(); OUTCOME_ALLOWED=(); OUTCOME_FACTS=(); CHECKS=0
}

finish() {
  outcome_flush
  if [ "$FAILURES" -ne 0 ]; then
    printf '\n  \033[1;31m%s expectation(s) failed\033[0m\n' "$FAILURES"
    exit 1
  fi
  presenter || printf '\n  \033[1;32mall expectations met\033[0m\n'
}

# walkthrough_hook <event> <args...>: call the optional recording hook.
# Ignore its output and failures.
walkthrough_hook() {
  [ -n "${WALKTHROUGH_HOOK:-}" ] || return 0
  "$WALKTHROUGH_HOOK" "$@" </dev/null >/dev/null 2>&1 || true
}

# new_session <label>: generate <label>-<time>-<pid>-<n>.
new_session() {
  local id
  id="$1-$(date +%s)-$$-${RANDOM}"
  walkthrough_hook session "$(echo "${1%%-*}" | tr '[:lower:]' '[:upper:]')" "$id"
  echo "$id"
}

# run_agent <scenario> <session> [--stream]: save the agent's record to $AGENT_OUT.
run_agent() {
  AGENT_OUT="$STATE_DIR/runs/$1-$2.json"
  mkdir -p "$STATE_DIR/runs"
  python3 "$DEMO_DIR/agent/agent.py" --scenario "$1" --session "$2" --out "$AGENT_OUT" \
    ${SESSION_TITLE:+--title "$SESSION_TITLE"} "${@:3}"
}

# --- the agent's tool results --------------------------------------------

tool_output() { jq -r --argjson i "$1" '.tools[$i].output // empty' "$AGENT_OUT"; }

# expect_tool_status <n> <http status>: check the nth tool's HTTP status.
expect_tool_status() {
  local got
  got="$(tool_output "$1" | sed -n 's/^HTTP \([0-9]*\)$/\1/p' | tail -1)"
  if [ "$got" = "$2" ]; then pass_ "tool call $1: HTTP $2"; outcome_allowed "$2"; else fail_ "tool call $1: expected HTTP $2, got '${got:-none}'"; fi
}

# expect_tool_denied <n> <code>: check for a Cortex 403 with this code.
expect_tool_denied() {
  local out code
  out="$(tool_output "$1")"
  code="$(printf '%s\n' "$out" | sed '/^HTTP /d' | jq -r '.error // empty' 2>/dev/null | head -1)"
  if printf '%s' "$out" | grep -q '^HTTP 403$' && [ "$code" = "$2" ]; then
    pass_ "tool call $1: denied with $2"; outcome_refused "$2"
  else
    fail_ "tool call $1: expected a 403 $2, got: $(printf '%s' "$out" | tail -2 | tr '\n' ' ')"
  fi
}

# expect_tool_connect_refused <n>: the tunnel itself was refused.
expect_tool_connect_refused() {
  if tool_output "$1" | grep -q 'CONNECT tunnel failed, response 403'; then
    pass_ "tool call $1: tunnel refused with 403"; outcome_refused "connection refused by Cortex"
  else
    fail_ "tool call $1: expected the CONNECT to be refused, got: $(tool_output "$1" | tail -1)"
  fi
}

# expect_tool_refused_disguised <n>: check that a mismatched Host was blocked
# at CONNECT (403) or inside the tunnel (421).
expect_tool_refused_disguised() {
  local out
  out="$(tool_output "$1")"
  if printf '%s' "$out" | grep -q 'CONNECT tunnel failed, response 403'; then
    pass_ "tool call $1: tunnel refused with 403"; outcome_refused "connection refused by Cortex"
  elif printf '%s' "$out" | grep -q '^HTTP 421$'; then
    pass_ "tool call $1: 421, the Host is not the tunnel's"; outcome_refused "421 misdirected request"
  else
    fail_ "tool call $1: expected the disguised upload to be refused, got: $(printf '%s' "$out" | tail -1)"
  fi
}

expect_tool_failed() {
  local code
  code="$(jq -r --argjson i "$1" '.tools[$i].exit' "$AGENT_OUT")"
  if [ "$code" != "0" ] && [ -n "$code" ]; then
    pass_ "tool call $1 failed (exit $code): $(tool_output "$1" | tail -1)"; outcome_refused "blocked by the sandbox"
  else
    fail_ "tool call $1: expected a failure, got exit ${code:-none}"
  fi
}

expect_model_calls_ok() {
  local bad
  bad="$(jq '[.model_calls[] | select(.status != 200)] | length' "$AGENT_OUT")"
  if [ "$bad" = "0" ]; then pass_ "every model call was forwarded ($(jq '.model_calls | length' "$AGENT_OUT"))"; else fail_ "$bad model call(s) failed"; fi
}

# expect_model_denied <n> <code>: check that the nth model call returned 403
# with this code, withholding the response from the agent.
expect_model_denied() {
  local got
  got="$(jq -r --argjson i "$1" '"\(.model_calls[$i].status) \(.model_calls[$i].error)"' "$AGENT_OUT")"
  if [ "$got" = "403 $2" ]; then pass_ "model call $1: denied with $2"; outcome_refused "$2"; else fail_ "model call $1: expected a 403 $2, got '$got'"; fi
}

# expect_tools_run <n>: the agent ran exactly n tools.
expect_tools_run() {
  local got
  got="$(jq '.tools | length' "$AGENT_OUT")"
  if [ "$got" = "$1" ]; then pass_ "the agent ran $1 tool(s)"; else fail_ "expected the agent to run $1 tool(s), it ran $got"; fi
}

# --- the mock services' recorders -----------------------------------------

_mock() { curl -fsS --max-time 5 --cacert "$MOCK_CA" "$@"; }

reset_mocks() {
  if ! { _mock -X POST "$SUPPORT_URL/_reset" && _mock -X POST "$LLM_URL/_reset"; } >/dev/null; then
    fail_ "could not reset the mock recorders"
  fi
}

support_requests() { _mock "$SUPPORT_URL/_requests"; }
llm_requests() { _mock "$LLM_URL/_requests"; }

expect_support_reports() {
  local got
  got="$(support_requests | jq '[.requests[] | select(.path | startswith("/projects/"))] | length')"
  if [ "$got" = "$1" ]; then
    if [ "$1" = 0 ]; then pass_ "the support service received nothing"; outcome_fact "support received nothing"
    else pass_ "the support service received $1 report(s)"; if [ "$1" = 1 ]; then outcome_fact "support received the report"; else outcome_fact "support received $1 reports"; fi; fi
  else
    fail_ "expected $1 report(s) at the support service, got ${got:-<unreachable>}"
  fi
}

# Check that neither mock received the developer token.
expect_no_principal_on_wire() {
  local all
  all="$(support_requests; llm_requests)"
  if [ -z "$DEV_JWT" ]; then
    fail_ "no developer token at $IDENTITY_DIR/developer.jwt"
  elif printf '%s' "$all" | grep -qF -e "$DEV_JWT" -e 'x-developer-token'; then
    fail_ "the developer token reached a mock service"
  else
    pass_ "the developer token never left cortex"
  fi
}

# --- the session timeline ---------------------------------------------------

session_json() { curl -fsS --max-time 5 "$SESSION_API/v1/sessions/$1?limit=2000"; }

# expect_cpex <session> <action> <reason> [path]: check a CPEX decision on the
# timeline, optionally filtered by request path.
expect_cpex() {
  local n
  n="$(session_json "$1" | jq --arg a "$2" --arg r "$3" --arg p "${4:-}" \
    '[.events[]? | select($p == "" or .httpPath == $p) | .invocations.outbound[]?
      | select(.plugin == "cpex" and .action == $a and .reason == $r)] | length')"
  if [ "${n:-0}" -gt 0 ]; then pass_ "timeline: cpex $2 ${4:+on $4 }($3)"; else fail_ "timeline: no cpex $2 $3 ${4:+on $4 }in session $1"; fi
}

# expect_session_host <session> <host>: a request to host in the session.
expect_session_host() {
  if session_json "$1" | jq -e --arg h "$2" '[.events[]? | select(.host == $h or (.host | startswith($h + ":")))] | length > 0' >/dev/null; then
    pass_ "timeline: the request to $2 is in session $1"
  else
    fail_ "timeline: no request to $2 in session $1"
  fi
}

# expect_no_tunnel_to <host> <session>: check the destination independently
# of the request's Host header.
expect_no_tunnel_to() {
  if session_json "$2" | jq -e --arg h "$1" '[.events[]? | select(.host == $h or (.host | startswith($h + ":")))] | length == 0' >/dev/null; then
    pass_ "timeline: nothing in session $2 went to $1"
  else
    fail_ "timeline: a request in session $2 went to $1"
  fi
}

# expect_tunnels_denied <session> <host> <n>: n CONNECTs to host denied on
# the timeline.
expect_tunnels_denied() {
  local n
  n="$(session_json "$1" | jq --arg h "$2" '[.events[]? | select(.phase == "denied" and .httpMethod == "CONNECT"
    and (.host == $h or (.host | startswith($h + ":"))))] | length')"
  if [ "${n:-0}" = "$3" ]; then pass_ "timeline: $3 CONNECT(s) to $2 denied"; else fail_ "timeline: expected $3 denied CONNECT(s) to $2, got ${n:-none}"; fi
}

# Check for fixture-secret leaks in responses and recorded decisions.
# Model requests may contain the secret, so those sessions check only
# CPEX decision records, not the full timeline.
expect_secret_absent() {
  local where="$1" text="$2"
  if printf '%s' "$text" | grep -qF "$SECRET"; then fail_ "the secret appears in $where"; else pass_ "no secret in $where"; fi
}

cpex_invocations() {
  session_json "$1" | jq -c '[.events[]? | .invocations.outbound[]? | select(.plugin == "cpex")]'
}

# --- credential custody (run.sh up --custody) -------------------------------

SUPPORT_TOKEN_FILE="$STATE_DIR/support-token"

# require_custody: skip the scenario unless custody is on.
require_custody() {
  if [ ! -f "$CUSTODY_MARK" ]; then
    note "skipped: credential custody is off (./run.sh up --custody)"
    exit 0
  fi
}

# vault_logins: count JWT logins in Vault's audit log.
# CPEX logs in as the developer for each credential retrieval.
vault_logins() {
  VAULT_ROOT_TOKEN=unused docker compose -f "$DEMO_DIR/docker-compose.yml" logs --no-log-prefix vault 2>/dev/null \
    | jq -R 'fromjson? | select(.type == "request" and .request.path == "auth/jwt/login")' 2>/dev/null \
    | jq -s length
}

# expect_vault_logins <before> <n>: n logins since before.
expect_vault_logins() {
  local after
  after="$(vault_logins)"
  if [ $((after - $1)) -eq "$2" ]; then
    if [ "$2" = 0 ]; then pass_ "Vault was not consulted"; outcome_fact "Vault not consulted"
    else pass_ "Vault was consulted ($2 login(s))"; outcome_fact "credential fetched from Vault"; fi
  else
    fail_ "expected $2 Vault login(s), got $((after - $1))"
  fi
}

# expect_support_credential: verify that every received report's Authorization
# contains only the support credential from Vault.
expect_support_credential() {
  local want n bad
  want="Bearer $(cat "$SUPPORT_TOKEN_FILE")"
  n="$(support_requests | jq '[.requests[] | select(.path | startswith("/projects/"))] | length')"
  bad="$(support_requests | jq --arg w "$want" '[.requests[] | select(.path | startswith("/projects/")) | select(.headers.authorization != $w)] | length')"
  if [ "${n:-0}" -gt 0 ] && [ "$bad" = 0 ]; then
    pass_ "the support service received the Vault credential on every report ($n)"
    outcome_fact "support received the Vault credential"
  else
    fail_ "expected the Vault credential on every report: $bad of ${n:-0} without it"
  fi
}

# expect_credential_contained <session>: check for support credential leaks
# in agent records, the timeline, and .state/, excluding its own token file.
expect_credential_contained() {
  local tok file where=()
  tok="$(cat "$SUPPORT_TOKEN_FILE")"
  env | grep -qF "$tok" && where+=("the agent's environment")
  grep -qF "$tok" "$AGENT_OUT" && where+=("the agent's commands and their output")
  session_json "$1" | grep -qF "$tok" && where+=("the session timeline")
  file="$(grep -rlF "$tok" "$STATE_DIR" --exclude=support-token 2>/dev/null | head -1 || true)"
  [ -n "$file" ] && where+=("$file")
  if [ "${#where[@]}" -eq 0 ]; then
    pass_ "the credential never reached the agent, the timeline, cortex's logs or state"
    outcome_fact "the agent never held it"
  else
    fail_ "the credential appears in: ${where[*]}"
  fi
}
