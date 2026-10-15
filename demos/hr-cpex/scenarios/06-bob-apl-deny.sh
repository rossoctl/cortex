#!/usr/bin/env bash
# Bob (HR) searches repos. The cheap APL gate
# require(team.engineering | team.security) denies him before Cedar or
# the IdP is consulted.

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

step "Bob (HR) → search_repos"
note "Denied by the APL team gate, the route's first step"

BOB=$(mint bob)
CLIENT=$(mint hr-copilot)

reset_upstream
before=$(kc_exchange_count)
call_search_repos "$BOB" "$CLIENT" internal

expect_deny cpex.routes_tool_search_repos_pre_invocation_0_
expect_exchanges "$before" 0
expect_upstream_calls 0
finish
