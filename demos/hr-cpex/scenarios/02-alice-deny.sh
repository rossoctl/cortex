#!/usr/bin/env bash
# Alice (engineer) calls get_compensation. require(role.hr) denies before
# delegation, so Keycloak sees no token exchange and hr-mcp no request.

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

step "Alice (engineer) → get_compensation"
note "Denied by require(role.hr), the route's first step"

ALICE=$(mint alice)
CLIENT=$(mint hr-copilot)

reset_upstream
before=$(kc_exchange_count)
call_get_compensation "$ALICE" "$CLIENT" false

expect_deny cpex.routes_tool_get_compensation_pre_invocation_0_
expect_exchanges "$before" 0
expect_upstream_calls 0
finish
