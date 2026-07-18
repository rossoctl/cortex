#!/usr/bin/env bash
# Alice (engineer, role.engineer) calls get_compensation. Expected:
#
#   * Gateway denies with an MCP JSON-RPC 2.0 error frame at HTTP 200:
#     error.message = the human reason, error.data.error = the cpex code
#     (cpex.<sanitized-code>), error.data.plugin = cpex. The forward proxy
#     renders MCP-protocol errors for MCP requests (see
#     authbridge/authlib/listener/httpx/render.go).
#   * No token exchange happened (Keycloak's /token endpoint
#     should NOT receive a token-exchange call for this request)
#   * MCP server NEVER sees the call (request short-circuits at policy)

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

step "Alice (engineer) → get_compensation"
note "Expected: HTTP 200 + JSON-RPC error frame, error.data.error=cpex.routes_tool_get_compensation_apl_pre_invocation_0_"
note "Triggered by: require(role.hr) deny BEFORE delegation runs"
note "Expected upstream: no inbound request (gateway short-circuited)"

ALICE=$(mint alice)
CLIENT=$(mint hr-copilot)

call_get_compensation "$ALICE" "$CLIENT" false
