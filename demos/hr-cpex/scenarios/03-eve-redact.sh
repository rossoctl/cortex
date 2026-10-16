#!/usr/bin/env bash
# Eve (HR, no view_ssn) makes the call Bob made. The policy redacts
# args.ssn on the request and result.ssn on the response for a caller
# without perm.view_ssn.
#
# The route delegates, so by default the call fails closed like
# scenario 01 and the redaction is checked by the engine test
# (core/plugins/cpex, TestHRDemoEveRedaction). With
# CPEX_DELEGATION=available it is asserted on hr-mcp and in the reply.

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

step "Eve (HR, no view_ssn) → get_compensation (include_ssn=true)"

EVE=$(mint eve)
CLIENT=$(mint hr-copilot)

reset_upstream
before=$(kc_exchange_count)
call_get_compensation "$EVE" "$CLIENT" true

if delegation_available; then
  expect_status 200
  expect_rpc_ok
  expect_upstream_audience workday-api
  expect_upstream_arg ssn "[REDACTED]"
  expect_upstream_arg employee_id EMP-001234
  expect_result_field ssn "[REDACTED]"
else
  expect_delegation_pending "$before"
fi
finish
