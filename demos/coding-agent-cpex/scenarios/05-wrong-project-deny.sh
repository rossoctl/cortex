#!/usr/bin/env bash
# A clean report to project B. The support credential reaches both
# projects; the developer is on A only, so policy refuses it.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

SID="$(new_session s1)"
step "S1 · the agent files a clean report with project B"
reset_mocks
run_agent 05-wrong-project "$SID"
expect_tool_denied 0 cpex.project_not_authorized
expect_cpex "$SID" deny cpex.project_not_authorized /projects/B/reports
expect_support_reports 0
expect_no_principal_on_wire
finish
