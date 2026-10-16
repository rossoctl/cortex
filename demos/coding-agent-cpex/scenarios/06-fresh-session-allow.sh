#!/usr/bin/env bash
# The taint belongs to the session that read the secret, not to the
# developer: the same report from a new session goes through.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

SID="$(new_session s3)"
step "S3: a new session files the same clean report"
[ -f "$STATE_DIR/tainted-session" ] && note "tainted session: $(cat "$STATE_DIR/tainted-session")"
reset_mocks
run_agent 06-fresh-session "$SID"
expect_tool_status 0 201
expect_cpex "$SID" allow "all policies passed" /projects/A/reports
expect_support_reports 1
expect_no_principal_on_wire
finish
