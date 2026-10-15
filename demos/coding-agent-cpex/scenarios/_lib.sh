# shellcheck shell=bash
# Shared helpers for the scenarios. Source from each script:
#
#   source "$(dirname "$0")/_lib.sh"
#
# A scenario runs the scripted agent against the demo cortex (run.sh up),
# then checks three records: what the agent's tools got back, the cortex
# session timeline, and what the mock services received.

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=../env.sh
source "$DEMO_DIR/env.sh"

# The synthetic secret in the fixtures, and the developer's token. Neither
# may show up anywhere the scenarios look, except where noted.
SECRET="AKIAIOSFODNN7EXAMPLE"
DEV_JWT="$(cat "$IDENTITY_DIR/developer.jwt" 2>/dev/null)"

step() {
  echo
  echo "============================================================"
  echo "$@"
  echo "============================================================"
}

note() { echo "  ▸ $*"; }

FAILURES=0
pass_() { printf '  \033[1;32m✓\033[0m %s\n' "$*"; }
fail_() { printf '  \033[1;31m✗\033[0m %s\n' "$*"; FAILURES=$((FAILURES + 1)); }

finish() {
  if [ "$FAILURES" -ne 0 ]; then
    printf '\n  \033[1;31m%s expectation(s) failed\033[0m\n' "$FAILURES"
    exit 1
  fi
  printf '\n  \033[1;32mall expectations met\033[0m\n'
}

new_session() { echo "$1-$(date +%s)-$$-${RANDOM}"; }

# run_agent <scenario> <session>: run the scripted agent; its record is
# then in $AGENT_OUT.
run_agent() {
  AGENT_OUT="$STATE_DIR/runs/$1-$2.json"
  mkdir -p "$STATE_DIR/runs"
  python3 "$DEMO_DIR/agent/agent.py" --scenario "$1" --session "$2" --out "$AGENT_OUT"
}

# --- the agent's tool results --------------------------------------------

tool_output() { jq -r --argjson i "$1" '.tools[$i].output // empty' "$AGENT_OUT"; }

# expect_tool_status <n> <http status>: the nth tool call's curl got it.
expect_tool_status() {
  local got
  got="$(tool_output "$1" | sed -n 's/^HTTP \([0-9]*\)$/\1/p' | tail -1)"
  if [ "$got" = "$2" ]; then pass_ "tool call $1: HTTP $2"; else fail_ "tool call $1: expected HTTP $2, got '${got:-none}'"; fi
}

# expect_tool_denied <n> <code>: a 403 from cortex carrying the code.
expect_tool_denied() {
  local out code
  out="$(tool_output "$1")"
  code="$(printf '%s\n' "$out" | sed '/^HTTP /d' | jq -r '.error // empty' 2>/dev/null | head -1)"
  if printf '%s' "$out" | grep -q '^HTTP 403$' && [ "$code" = "$2" ]; then
    pass_ "tool call $1: denied with $2"
  else
    fail_ "tool call $1: expected a 403 $2, got: $(printf '%s' "$out" | tail -2 | tr '\n' ' ')"
  fi
}

# expect_tool_connect_refused <n>: the tunnel itself was refused.
expect_tool_connect_refused() {
  if tool_output "$1" | grep -q 'CONNECT tunnel failed, response 403'; then
    pass_ "tool call $1: tunnel refused with 403"
  else
    fail_ "tool call $1: expected the CONNECT to be refused, got: $(tool_output "$1" | tail -1)"
  fi
}

expect_tool_failed() {
  local code
  code="$(jq -r --argjson i "$1" '.tools[$i].exit' "$AGENT_OUT")"
  if [ "$code" != "0" ] && [ -n "$code" ]; then
    pass_ "tool call $1 failed (exit $code): $(tool_output "$1" | tail -1)"
  else
    fail_ "tool call $1: expected a failure, got exit ${code:-none}"
  fi
}

expect_model_calls_ok() {
  local bad
  bad="$(jq '[.model_calls[] | select(.status != 200)] | length' "$AGENT_OUT")"
  if [ "$bad" = "0" ]; then pass_ "every model call was forwarded ($(jq '.model_calls | length' "$AGENT_OUT"))"; else fail_ "$bad model call(s) failed"; fi
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
    if [ "$1" = 0 ]; then pass_ "the support service received nothing"; else pass_ "the support service received $1 report(s)"; fi
  else
    fail_ "expected $1 report(s) at the support service, got ${got:-<unreachable>}"
  fi
}

# The developer token Cortex holds reaches neither mock, on any request.
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

# expect_cpex <session> <action> <reason> [path]: a cpex invocation on the
# timeline, on a request to path when given.
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

# The secret in what cortex returned and recorded. For a session whose
# model calls carried the secret, those calls are recorded as sent, so the
# check is narrowed to cpex's invocations.
expect_secret_absent() {
  local where="$1" text="$2"
  if printf '%s' "$text" | grep -qF "$SECRET"; then fail_ "the secret appears in $where"; else pass_ "no secret in $where"; fi
}

cpex_invocations() {
  session_json "$1" | jq -c '[.events[]? | .invocations.outbound[]? | select(.plugin == "cpex")]'
}
