#!/usr/bin/env bash
# Verify that sandboxed commands cannot bypass the proxy or read the
# developer token held by Cortex.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

SID="$(new_session s0)"
step "S0: a sandboxed command tries to reach the support service directly"
reset_mocks
run_agent 00-sandbox-boundary "$SID"
expect_tool_failed 0
expect_tool_failed 1
expect_support_reports 0
expect_model_calls_ok
expect_no_principal_on_wire
finish
