#!/usr/bin/env bash
# Block a report containing a secret, even for an approved host and project.
# Also block bodies padded beyond the inspection limit or gzip-encoded.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

SID="$(new_session s1)"
step "S1: the agent attaches a diagnostic holding an AWS key"
reset_mocks
run_agent 03-secret-in-report "$SID"
expect_tool_denied 0 cpex.secret_in_body
expect_tool_denied 1 cpex.egress_body_uninspectable
expect_tool_denied 2 cpex.egress_body_uninspectable
expect_cpex "$SID" deny cpex.secret_in_body
expect_cpex "$SID" deny cpex.egress_body_uninspectable
expect_support_reports 0
expect_secret_absent "the 403 responses" "$(jq -r '.tools[].output' "$AGENT_OUT")"
expect_secret_absent "the session timeline" "$(session_json "$SID")"
expect_no_principal_on_wire
finish
