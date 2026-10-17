#!/usr/bin/env bash
# Block connections to an unapproved host, including an attempt to disguise
# the destination with Host: support.localtest.me. Both CONNECTs are denied.
# Cortex also rejects a mismatched Host inside an accepted tunnel with 421.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

SID="$(new_session s1)"
step "S1: the agent uploads a build log to paste.evil.example, then disguises it as support"
reset_mocks
run_agent 01-unapproved-host "$SID"
expect_tool_connect_refused 0
expect_tool_refused_disguised 1
expect_cpex "$SID" deny cpex.egress_host_not_allowed
expect_tunnels_denied "$SID" paste.evil.example 2
expect_no_tunnel_to support.localtest.me "$SID"
expect_support_reports 0
expect_no_principal_on_wire
finish
