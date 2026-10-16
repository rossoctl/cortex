#!/usr/bin/env bash
# Alice (engineering) searches an INTERNAL repo. All four layers pass:
# the APL team gate, Cedar (engineer x internal), the exchange to
# github-api, and the check that the IdP granted repo:read:internal.
#
# The route delegates, so by default the call fails closed after the
# exchange. With CPEX_DELEGATION=available it asserts the allow.

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

step "Alice (engineering) → search_repos(repo_name='web-app', visibility='internal')"

ALICE=$(mint alice)
CLIENT=$(mint hr-copilot)

reset_upstream
before=$(kc_exchange_count)
call_search_repos "$ALICE" "$CLIENT" internal web-app

if delegation_available; then
  expect_status 200
  expect_rpc_ok
  expect_upstream_calls 1
  expect_upstream_audience github-api
else
  expect_delegation_pending "$before"
fi
finish
