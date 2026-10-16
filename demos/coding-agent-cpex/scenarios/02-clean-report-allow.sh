#!/usr/bin/env bash
# A clean report to the developer's own project goes through. The agent's
# curl runs in the sandbox, as a child of the agent, and cortex files it
# in the agent's session by the process chain.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

SID="$(new_session s1)"
step "S1: the agent files a clean report with project A"
reset_mocks
run_agent 02-clean-report "$SID"
expect_tool_status 0 201
expect_session_host "$SID" support.localtest.me
expect_cpex "$SID" allow "all policies passed" /projects/A/reports
expect_support_reports 1
expect_no_principal_on_wire
finish
