#!/usr/bin/env bash
# adjust_compensation needs a manager's approval only over $10,000. At
# $5,000, Bob (HR) passes the role gate and the change applies at once;
# Alice (engineering) is stopped by the role gate.

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
finish
