#!/usr/bin/env bash
# Block a model response proposing a secret leak after the session reads .env.
# A report without secrets from a separate session succeeds. Test both JSON
# and SSE responses; blocked tool calls never run.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

for mode in json sse; do
  flag=()
  [ "$mode" = sse ] && flag=(--stream)

  SID="$(new_session s4-$mode)"
  step "S4 ($mode): a tainted session's model proposes pasting the key"
  reset_mocks
  run_agent 09-intent-deny "$SID" ${flag[@]+"${flag[@]}"}
  expect_model_denied 1 cpex.secret_in_tool_call
  expect_tools_run 1
  expect_cpex "$SID" observe cpex.secret_in_tool_result /v1/messages
  expect_cpex "$SID" deny cpex.secret_in_tool_call /v1/messages
  expect_secret_absent "cpex's invocations" "$(cpex_invocations "$SID")"
  expect_support_reports 0

  SID="$(new_session s5-$mode)"
  step "S5 ($mode): an untainted session files a clean report"
  reset_mocks
  run_agent 09-intent-allow "$SID" ${flag[@]+"${flag[@]}"}
  expect_model_calls_ok
  expect_tools_run 1
  expect_tool_status 0 201
  expect_cpex "$SID" allow "all policies passed" /projects/A/reports
  expect_support_reports 1
done
expect_no_principal_on_wire
finish
