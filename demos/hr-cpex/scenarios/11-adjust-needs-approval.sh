#!/usr/bin/env bash
# Over $10,000, adjust_compensation waits for Bob's manager (Alice) to
# approve over OIDC CIBA. The sidecar suspends the call with JSON-RPC
# -32120 and an elicitation id; Bob retries with the id in
# X-Policy-Elicitation-Id until Alice answers. Alice answers through the
# approval channel's API, so the run is unattended.
#
# The approval belongs to Bob, this route and this tool, is spent by the
# call it lets through, and is re-checked against the amount
# (scope: at most $25,000 either way).

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

BOB=$(mint bob)
CLIENT=$(mint hr-copilot)
STEP_DENY=cpex.routes_tool_adjust_compensation_pre_invocation_2_

step "1 · Bob → adjust_compensation +\$15,000: suspended for Alice's approval"
reset_upstream
seen=$(approvals_seen)
call_adjust_compensation "$BOB" "$CLIENT" 15000
expect_pending
expect_upstream_calls 0
EID="$LAST_EID"

step "2 · Alice approves; Bob's retry applies the change"
answer_approval approve alice "$seen"
retry_until_answered "$BOB" "$CLIENT" 15000 "$EID"
expect_rpc_ok
expect_result_field status applied
expect_upstream_calls 1
expect_upstream_arg amount 15000

step "3 · The spent approval does not authorize +\$20,000"
reset_upstream
ELICITATION_ID="$EID" call_adjust_compensation "$BOB" "$CLIENT" 20000
expect_deny cpex.elicitation_unknown_id
expect_upstream_calls 0

step "4 · A new +\$20,000 change asks again; Alice denies it"
reset_upstream
seen=$(approvals_seen)
call_adjust_compensation "$BOB" "$CLIENT" 20000
expect_pending
if [ "$LAST_EID" != "$EID" ]; then pass_ "a new approval was requested"; else fail_ "the spent approval was reused"; fi
EID="$LAST_EID"
answer_approval deny alice "$seen"
retry_until_answered "$BOB" "$CLIENT" 20000 "$EID"
expect_deny "$STEP_DENY"
expect_upstream_calls 0

step "5 · An approval for +\$30,000 is outside the scope (≤ \$25,000)"
reset_upstream
seen=$(approvals_seen)
call_adjust_compensation "$BOB" "$CLIENT" 30000
expect_pending
EID="$LAST_EID"
answer_approval approve alice "$seen"
retry_until_answered "$BOB" "$CLIENT" 30000 "$EID"
expect_deny "$STEP_DENY"
expect_upstream_calls 0
finish
