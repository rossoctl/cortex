#!/usr/bin/env bash
# Bob (HR, perm.view_ssn) calls get_compensation. The route passes
# require(role.hr) and delegate(...) exchanges Bob's token for a
# workday-api token at Keycloak.
#
# By default CPEX does not hand the minted token to Cortex, so the sidecar
# refuses to forward the call (cpex.delegation_unavailable) rather than
# send Bob's own bearer to the tool. With CPEX_DELEGATION=available the
# scenario asserts the full outcome: 200, hr-mcp holds the minted token,
# and args.ssn is intact.

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

step "Bob (HR) → get_compensation (include_ssn=true)"

BOB=$(mint bob)
CLIENT=$(mint hr-copilot)

reset_upstream
before=$(kc_exchange_count)
call_get_compensation "$BOB" "$CLIENT" true

if delegation_available; then
  expect_status 200
  expect_rpc_ok
  expect_upstream_calls 1
  expect_upstream_audience workday-api
  expect_upstream_arg ssn would-be-removed-if-redact-fires
else
  expect_delegation_pending "$before"
fi
finish
