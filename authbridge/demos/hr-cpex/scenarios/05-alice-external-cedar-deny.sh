#!/usr/bin/env bash
# Alice (engineering) asks for an EXTERNAL repo. APL's coarse gate
# passes (she IS in engineering), but Cedar's policy doesn't permit
# engineering on external visibility — only security can read those.
# The denial happens at the gateway BEFORE any IdP call.
#
#   Layer 1 — APL gate → passes (team.engineering)
#   Layer 2 — Cedar → DENIES (engineering policy when-clause fails:
#             resource.visibility == "external", not "internal")
#   Layers 3-4 — never reached. No token exchange. GitHub never sees
#             the request.
#
# Result: HTTP 403 with JSON error body, code = cpex.cedar_default_deny
# — a CPEX policy deny is surfaced by AuthBridge as a transport-level
# 403 (not a 500, not an MCP JSON-RPC envelope).

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

step "Alice (engineering) → search_repos(visibility='external')"
note "Expected: HTTP 403, code=cpex.cedar_default_deny"
note "Triggered by: Cedar denies — engineering can't read external repos"
note "Expected upstream: no inbound request (gateway short-circuits at PDP)"

ALICE=$(mint alice)
CLIENT=$(mint hr-copilot)

curl -s -X POST "$GATEWAY/mcp" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $CLIENT" \
  -H "X-User-Token: $ALICE" \
  -d '{
    "jsonrpc": "2.0",
    "id": 1,
    "method": "tools/call",
    "params": {
      "name": "search_repos",
      "arguments": { "repo_name": "partner-sdk", "visibility": "external" }
    }
  }' -i 2>&1 | head -20
