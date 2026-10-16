#!/usr/bin/env bash
# A host outside the allowlist is refused at the tunnel. A sandbox domain
# allowlist does the same; this is parity, not the point of the demo.
#
# The second attempt opens the tunnel to the same host but names the
# support service in its Host header. Policy judges where the bytes go:
# the CONNECT is refused as before, and had it been let through, cortex
# would refuse the request inside it with a 421 rather than judge the
# Host the client wrote.
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
