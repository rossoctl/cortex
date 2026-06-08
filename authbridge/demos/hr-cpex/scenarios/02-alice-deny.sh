#!/usr/bin/env bash
# Alice (engineer, role.engineer) calls get_compensation. Expected:
#
#   * Gateway returns HTTP 403 with a JSON error body
#     {"error":"cpex.<sanitized-code>",...,"plugin":"cpex"}. A CPEX
#     policy deny is an authorization decision, surfaced by AuthBridge
#     as a transport-level 403 (not a 500, not an MCP JSON-RPC envelope)
#   * No token exchange happened (Keycloak's /token endpoint
#     should NOT receive a token-exchange call for this request)
#   * MCP server NEVER sees the call (request short-circuits at policy)

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

step "Alice (engineer) → get_compensation"
note "Expected: HTTP 403, code=cpex.routes_tool_get_compensation_apl_policy_0_"
note "Triggered by: require(role.hr) deny BEFORE delegation runs"
note "Expected upstream: no inbound request (gateway short-circuited)"

ALICE=$(mint alice)
CLIENT=$(mint hr-copilot)

call_get_compensation "$ALICE" "$CLIENT" false
