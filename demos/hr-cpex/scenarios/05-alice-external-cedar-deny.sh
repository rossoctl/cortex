#!/usr/bin/env bash
# Alice (engineering) searches an EXTERNAL repo. The APL gate passes, but
# Cedar permits engineers only on internal repos, so the call is denied
# before any token exchange.

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

step "Alice (engineering) → search_repos(visibility='external')"
note "Denied by Cedar: engineering cannot read external repos"

ALICE=$(mint alice)
CLIENT=$(mint hr-copilot)

reset_upstream
before=$(kc_exchange_count)
call_search_repos "$ALICE" "$CLIENT" external partner-sdk

expect_deny cpex.cedar_default_deny
expect_exchanges "$before" 0
expect_upstream_calls 0
finish
