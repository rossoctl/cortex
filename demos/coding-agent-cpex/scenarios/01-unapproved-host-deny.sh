#!/usr/bin/env bash
# A host outside the allowlist is refused at the tunnel. A sandbox domain
# allowlist does the same; this is parity, not the point of the demo.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

SID="$(new_session s1)"
step "S1 · the agent uploads a build log to paste.evil.example"
reset_mocks
run_agent 01-unapproved-host "$SID"
expect_tool_connect_refused 0
expect_cpex "$SID" deny cpex.egress_host_not_allowed
expect_support_reports 0
expect_no_principal_on_wire
finish
