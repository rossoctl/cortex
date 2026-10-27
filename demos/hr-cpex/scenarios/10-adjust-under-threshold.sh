#!/usr/bin/env bash
# adjust_compensation needs a manager's approval only over $10,000. At
# $5,000, Bob (HR) passes the role gate and the change applies at once;
# Alice (engineering) is stopped by the role gate. A string amount, a
# body only a lenient parser reads (a NaN, or UTF-16), and a tool name
# with no route are all refused.

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

BOB=$(mint bob)
ALICE=$(mint alice)
CLIENT=$(mint hr-copilot)

step "Bob (HR) → adjust_compensation +\$5,000"
note "require(role.hr) passes; under the threshold, so no approval is asked"
reset_upstream
seen=$(approvals_seen)
call_adjust_compensation "$BOB" "$CLIENT" 5000

expect_status 200
expect_rpc_ok
expect_result_field status applied
expect_upstream_calls 1
expect_upstream_arg amount 5000
if [ "$(approvals_seen)" = "$seen" ]; then pass_ "no approval was requested"; else fail_ "an approval was requested"; fi

step "Alice (engineering) → adjust_compensation +\$5,000"
note "Denied by the role gate, the route's first step"
reset_upstream
call_adjust_compensation "$ALICE" "$CLIENT" 5000

expect_deny cpex.routes_tool_adjust_compensation_pre_invocation_0_
expect_upstream_calls 0

step "Bob → adjust_compensation \"15000\" (a string)"
note "Refused before the threshold reads it, so it cannot skip the approval"
reset_upstream
seen=$(approvals_seen)
call_adjust_compensation "$BOB" "$CLIENT" '"15000"'

expect_deny cpex.routes_tool_adjust_compensation_args_amount
expect_upstream_calls 0
if [ "$(approvals_seen)" = "$seen" ]; then pass_ "no approval was requested"; else fail_ "an approval was requested"; fi

step "Bob → adjust_compensation +\$50,000, with a NaN beside it"
note "No JSON parser on the sidecar reads NaN; Python's would, so it is refused"
reset_upstream
seen=$(approvals_seen)
call_tool "$BOB" "$CLIENT" adjust_compensation '{"employee_id":"EMP-001234","amount":50000,"note":NaN}'

expect_http_deny cpex.unparseable_request
expect_upstream_calls 0
if [ "$(approvals_seen)" = "$seen" ]; then pass_ "no approval was requested"; else fail_ "an approval was requested"; fi

step "Alice (engineering) → adjust_compensation +\$50,000, sent as UTF-16"
note "Python's json reads UTF-16; the sidecar's parsers read UTF-8 only, so it is refused"
reset_upstream
utf16="$(mktemp)"
_tool_body adjust_compensation '{"employee_id":"EMP-001234","amount":50000}' | iconv -f UTF-8 -t UTF-16BE >"$utf16"
_print_response "$(curl -isS --max-time 10 -x "$PROXY" -X POST "$MCP_TARGET" \
  -H "Content-Type: application/json" -H "Authorization: Bearer $CLIENT" \
  -H "X-User-Token: $ALICE" --data-binary @"$utf16")"
rm -f "$utf16"

expect_http_deny cpex.unparseable_request
expect_upstream_calls 0

step "Bob → Adjust_Compensation (a tool name with no route)"
note "A tool without its own route is refused, not passed with no policy"
reset_upstream
call_tool "$BOB" "$CLIENT" Adjust_Compensation '{"employee_id":"EMP-001234","amount":50000}'

expect_deny cpex.tool_unrouted
expect_upstream_calls 0
finish
