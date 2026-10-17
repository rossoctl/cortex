#!/usr/bin/env bash
# What the sidecar tells the tool. `global.assertions.request` renders
# engine-derived identity into request headers hr-mcp can read, and
# strips the user's raw token.
#
#   1. Bob sends an email. hr-mcp sees x-auth-user-id (Bob's subject),
#      x-auth-username and x-auth-roles, and no x-user-token.
#   2. Bob sends one spoofing x-auth-user-id: root. Each assertion removes
#      its header before setting it, so hr-mcp still sees Bob's subject.
#
# send_email is used because its route does not delegate: by default
# a delegating route fails closed before anything is forwarded.
#
# The headers are unsigned. hr-mcp trusts them only because nothing
# between the sidecar and it can set them.

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

BOB=$(mint bob)
CLIENT=$(mint hr-copilot)
BOB_SUB="$(token_sub "$BOB")"
# A fresh session, so a taint left by an earlier scenario does not apply.
export SESSION_ID="assertions-$$-${RANDOM}"

step "Bob → send_email, with identity asserted to the tool"
note "Bob's subject id: $BOB_SUB"
reset_upstream
call_send_email "$BOB" "$CLIENT"
expect_rpc_ok
expect_upstream_calls 1
expect_upstream_header x-auth-user-id "$BOB_SUB"
expect_upstream_header x-auth-username bob
expect_upstream_header x-auth-roles hr
expect_upstream_no_header x-user-token

step "The same call, with Bob spoofing x-auth-user-id: root"
reset_upstream
SPOOF_HEADER="x-auth-user-id: root" call_send_email "$BOB" "$CLIENT"
expect_rpc_ok
expect_upstream_calls 1
expect_upstream_header x-auth-user-id "$BOB_SUB"
finish
