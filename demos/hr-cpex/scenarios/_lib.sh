# shellcheck shell=bash
# Shared helpers for the scenario scripts. Source from each script:
#
#   source "$(dirname "$0")/_lib.sh"
#
# The scenarios drive the tool through the sidecar's FORWARD proxy, the
# same egress path the agent's own tool calls take: curl uses the proxy
# with the absolute in-cluster MCP URL, and the proxy runs the outbound
# mcp-parser -> cpex pipeline.
#
#   PROXY        sidecar forward proxy (make port-forward: 8083 -> pod 8081)
#   MCP_TARGET   absolute URL the proxy forwards to (cluster DNS)
#   UPSTREAM     hr-mcp, for its request recorder (make port-forward: 9100)
#   SESSION_API  the sidecar's session API (make port-forward: 9094)
#   KEYCLOAK_HOST  Keycloak (make port-forward: 8081)
#
#   CPEX_DELEGATION  `pending` (default) or `available`. CPEX 0.3.0 mints
#                    a delegated token but does not hand it to Cortex, so
#                    a call whose route delegates fails closed with
#                    cpex.delegation_unavailable. `available` asserts the
#                    full outcomes instead, for a CPEX that returns it.
PROXY="${PROXY:-http://localhost:8083}"
MCP_TARGET="${MCP_TARGET:-http://hr-mcp.cpex-demo:9100/mcp}"
UPSTREAM="${UPSTREAM:-http://localhost:9100}"
SESSION_API="${SESSION_API:-http://localhost:9094}"
KEYCLOAK_HOST="${KEYCLOAK_HOST:-http://localhost:8081}"
CPEX_DELEGATION="${CPEX_DELEGATION:-pending}"
DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export KEYCLOAK_HOST

mint() {
  "$DEMO_DIR/mint-token.sh" "$1"
}

delegation_available() { [ "$CPEX_DELEGATION" = "available" ]; }

token_sub() {
  # The `sub` claim of a token. Decode only; the sidecar verifies.
  local payload
  payload="$(printf '%s' "$1" | cut -d. -f2)"
  while [ $(( ${#payload} % 4 )) -ne 0 ]; do payload="${payload}="; done
  printf '%s' "$payload" | tr '_-' '/+' | base64 -d 2>/dev/null | jq -r '.sub // empty'
}

_print_response() {
  # Print the status line, content-type and body, and keep the status
  # and body for the expect_* helpers.
  local raw="$1" status_line body
  status_line=$(printf '%s' "$raw" | awk 'NR==1 {sub(/\r$/, ""); print; exit}')
  body=$(printf '%s' "$raw" | awk 'p {print} /^\r?$/ {p=1}')
  LAST_STATUS="$(printf '%s' "$status_line" | awk '{print $2}')"
  LAST_BODY="$body"
  echo "  $status_line"
  if [ -n "$body" ]; then
    echo "  ---"
    if printf '%s' "$body" | jq . >/dev/null 2>&1; then
      printf '%s\n' "$body" | jq . | sed 's/^/  /'
    else
      printf '%s\n' "$body" | sed 's/^/  /'
    fi
  fi
}

_post_tool() {
  local user_token="$1" client_token="$2" body="$3"
  # SESSION_ID threads X-Session-Id, which the cpex plugin uses as the
  # CPEX session id, so session taint persists across calls.
  # SPOOF_HEADER adds a header the caller should not be able to set.
  local extra=()
  [ -n "${SESSION_ID:-}" ] && extra+=(-H "X-Session-Id: $SESSION_ID")
  [ -n "${SPOOF_HEADER:-}" ] && extra+=(-H "$SPOOF_HEADER")
  # `${extra[@]+"${extra[@]}"}` keeps bash 3.2 (macOS) happy under set -u.
  curl -isS --max-time 10 -x "$PROXY" -X POST "$MCP_TARGET" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $client_token" \
    -H "X-User-Token: $user_token" \
    ${extra[@]+"${extra[@]}"} \
    --data "$body"
}

_tool_body() {
  local name="$1" args="$2"
  printf '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"%s","arguments":%s}}' "$name" "$args"
}

call_tool() {
  _print_response "$(_post_tool "$1" "$2" "$(_tool_body "$3" "$4")")"
}

call_get_compensation() {
  local include_ssn="${3:-false}" employee_id="${4:-EMP-001234}"
  call_tool "$1" "$2" get_compensation \
    "{\"employee_id\":\"$employee_id\",\"include_ssn\":$include_ssn,\"ssn\":\"would-be-removed-if-redact-fires\"}"
}

call_send_email() {
  local body_text="${3:-Quarterly planning sync moved to Thursday.}"
  local subject="${4:-team update}" to="${5:-teammate@example.com}"
  call_tool "$1" "$2" send_email "{\"to\":\"$to\",\"subject\":\"$subject\",\"body\":\"$body_text\"}"
}

call_search_repos() {
  local visibility="$3" repo_name="${4:-}" args
  if [ -n "$repo_name" ]; then
    args="{\"repo_name\":\"$repo_name\",\"visibility\":\"$visibility\"}"
  else
    args="{\"visibility\":\"$visibility\"}"
  fi
  call_tool "$1" "$2" search_repos "$args"
}

step() {
  echo
  echo "============================================================"
  echo "$@"
  echo "============================================================"
}

note() {
  echo "  ▸ $*"
}

# ---------------------------------------------------------------------------
# Assertions. Failures accumulate; every scenario ends with `finish`.
# ---------------------------------------------------------------------------

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

expect_status() {
  if [ "${LAST_STATUS:-}" = "$1" ]; then pass_ "HTTP $1"; else fail_ "expected HTTP $1, got ${LAST_STATUS:-<none>}"; fi
}

expect_rpc_ok() {
  if printf '%s' "${LAST_BODY:-}" | jq -e '.result' >/dev/null 2>&1; then
    pass_ "JSON-RPC result (no error)"
  else
    fail_ "expected a JSON-RPC result, got: $(printf '%s' "${LAST_BODY:-}" | jq -c '.error // .' 2>/dev/null)"
  fi
}

# A cpex deny on an MCP call is a JSON-RPC error frame at HTTP 200 with
# the code in error.data.error.
expect_deny() {
  local want="$1" got
  got="$(printf '%s' "${LAST_BODY:-}" | jq -r '.error.data.error // empty' 2>/dev/null)"
  if [ "${LAST_STATUS:-}" = "200" ] && [ "$got" = "$want" ]; then
    pass_ "denied: $want"
  else
    fail_ "expected deny $want, got HTTP ${LAST_STATUS:-<none>} code '${got:-<none>}'"
  fi
}

# The tool result is a JSON string in result.content[0].text.
_result_text() { printf '%s' "${LAST_BODY:-}" | jq -r '.result.content[0].text // empty' 2>/dev/null; }

expect_result_field() {
  local field="$1" want="$2" got
  got="$(_result_text | jq -r --arg f "$field" '.[$f] // empty' 2>/dev/null)"
  if [ "$got" = "$want" ]; then pass_ "result.$field = $want"; else fail_ "expected result.$field = '$want', got '${got:-<none>}'"; fi
}

# --- upstream (hr-mcp's recorder) -----------------------------------------

reset_upstream() {
  curl -fsS -X POST "$UPSTREAM/_reset" >/dev/null 2>&1 \
    || note "warning: could not reach the upstream recorder at $UPSTREAM"
}

_upstream() { curl -fsS "$UPSTREAM/_requests" 2>/dev/null; }

expect_upstream_calls() {
  local want="$1" got
  got="$(_upstream | jq -r '.count // empty')"
  if [ "$got" = "$want" ]; then
    if [ "$want" = "0" ]; then pass_ "hr-mcp never saw the call"; else pass_ "hr-mcp saw $want call(s)"; fi
  else
    fail_ "expected $want hr-mcp call(s), got ${got:-<unreachable>}"
  fi
}

expect_upstream_header() {
  local name="$1" want="$2" got
  got="$(_upstream | jq -r --arg h "$name" '.requests[-1].headers[$h] // empty' 2>/dev/null)"
  if [ "$got" = "$want" ]; then pass_ "hr-mcp $name = $want"; else fail_ "expected hr-mcp $name = '$want', got '${got:-<absent>}'"; fi
}

expect_upstream_no_header() {
  local name="$1"
  if _upstream | jq -e --arg h "$name" '.requests[-1].headers | has($h)' >/dev/null 2>&1; then
    fail_ "expected hr-mcp not to see $name, but it was present"
  else
    pass_ "hr-mcp never saw $name"
  fi
}

expect_upstream_arg() {
  local name="$1" want="$2" got
  got="$(_upstream | jq -r --arg a "$name" '.requests[-1].arguments[$a] // empty' 2>/dev/null)"
  if [ "$got" = "$want" ]; then pass_ "hr-mcp args.$name = $want"; else fail_ "expected hr-mcp args.$name = '$want', got '${got:-<absent>}'"; fi
}

# The token hr-mcp was handed: the IdP-minted one for its audience.
expect_upstream_audience() {
  local want="$1" got
  got="$(_upstream | jq -r '.requests[-1].headers.authorization.jwt.aud // [] | join(",")' 2>/dev/null)"
  case ",$got," in
    *",$want,"*) pass_ "hr-mcp authorization aud=$want (minted, not the caller's)" ;;
    *) fail_ "expected hr-mcp authorization aud=$want, got '${got:-<none>}'" ;;
  esac
}

# --- Keycloak token exchanges ---------------------------------------------
# The realm records TOKEN_EXCHANGE events; reading them needs the admin API.

kc_exchange_count() {
  local admin
  admin=$(curl -fsS -X POST "$KEYCLOAK_HOST/realms/master/protocol/openid-connect/token" \
    -d grant_type=password -d client_id=admin-cli -d username=admin -d password=admin \
    2>/dev/null | jq -r '.access_token // empty')
  [ -n "$admin" ] || { echo "?"; return; }
  curl -fsS -H "Authorization: Bearer $admin" \
    "$KEYCLOAK_HOST/admin/realms/cpex-demo/events?type=TOKEN_EXCHANGE&max=10000" 2>/dev/null \
    | jq -r 'length' 2>/dev/null || echo "?"
}

# Usage: before=$(kc_exchange_count); <call>; expect_exchanges "$before" 1
expect_exchanges() {
  local before="$1" want="$2" after
  after="$(kc_exchange_count)"
  if [ "$before" = "?" ] || [ "$after" = "?" ]; then
    fail_ "could not read Keycloak's token-exchange events at $KEYCLOAK_HOST"
  elif [ $((after - before)) -eq "$want" ]; then
    pass_ "Keycloak performed $want token exchange(s)"
  else
    fail_ "expected $want token exchange(s), Keycloak saw $((after - before))"
  fi
}

# --- the session timeline (Cortex's audit trail) ---------------------------
# Counts cpex deny invocations carrying a code across every session.

timeline_denies() {
  local code="$1" total=0 sid n
  for sid in $(curl -fsS "$SESSION_API/v1/sessions" 2>/dev/null | jq -r '.sessions[].id'); do
    n=$(curl -fsS "$SESSION_API/v1/sessions/$sid?limit=2000" 2>/dev/null | jq --arg c "$code" \
      '[.events[]? | .invocations.outbound[]? | select(.plugin == "cpex" and .action == "deny" and .reason == $c)] | length' 2>/dev/null)
    total=$((total + ${n:-0}))
  done
  echo "$total"
}

# Usage: before=$(timeline_denies <code>); <call>; expect_timeline_deny <code> "$before"
expect_timeline_deny() {
  local code="$1" before="$2" after
  after="$(timeline_denies "$code")"
  if [ "$after" -gt "$before" ]; then
    pass_ "session timeline recorded the cpex deny ($code)"
  else
    fail_ "expected a cpex deny ($code) on the session timeline at $SESSION_API"
  fi
}

# The common shape of a call whose route delegates while CPEX cannot
# return the minted token: the exchange happened, the call failed closed,
# and nothing reached the tool, so the caller's bearer was never
# forwarded in place of the delegated one.
expect_delegation_pending() {
  local before="$1"
  expect_exchanges "$before" 1
  expect_deny cpex.delegation_unavailable
  expect_upstream_calls 0
  note "forwarding: pending CPEX patch"
}
