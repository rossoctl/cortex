#!/usr/bin/env bash
# The boundary is the sandbox, not the agent's cooperation: a command that
# ignores the proxy and dials the support service directly cannot connect,
# and the developer token cortex holds cannot be read from the sandbox.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

SID="$(new_session s0)"
step "S0 · a sandboxed command dials the support service around the proxy"
reset_mocks
run_agent 00-sandbox-boundary "$SID"
expect_tool_failed 0
expect_tool_failed 1
expect_support_reports 0
expect_model_calls_ok
expect_no_principal_on_wire
finish
