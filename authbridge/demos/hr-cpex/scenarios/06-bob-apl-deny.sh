#!/usr/bin/env bash
# Bob (HR, group=hr) tries to search github repos. APL's coarse
# gate fails immediately — Bob isn't in engineering or security.
# The deny happens BEFORE Cedar runs, before any IdP call.
#
# This shows the "fast path" in the policy: cheap predicates run
# first, expensive PDP / IdP work only happens for requests that
# clear them.
#
#   Layer 1 — APL gate `require(team.engineering | team.security)`
#             → FAILS (Bob is in team.hr)
#   Layers 2-4 — never reached. Cedar never invoked, IdP never
#             called, no token-exchange round-trip.
#
# Result: HTTP 403 with JSON error body, code = the cpex.* form of the
# apl.policy step index that failed (cpex.routes_tool_search_repos_apl_policy_0_).

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

step "Bob (HR) → search_repos (gateway short-circuits at the APL gate)"
note "Expected: HTTP 403, code=cpex.routes_tool_search_repos_apl_policy_0_"
note "Triggered by: require(team.engineering | team.security) — Bob is team.hr"
note "Expected: Cedar never runs; IdP never called"

BOB=$(mint bob)
CLIENT=$(mint hr-copilot)

# Capture then truncate. Piping `curl -i | head` trips `pipefail` (head closes
# early → curl gets SIGPIPE → non-zero), so capture first and slice with sed
# (reads to EOF, never early-closes the pipe).
resp=$(curl -s -i -x "$PROXY" -X POST "$MCP_TARGET" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $CLIENT" \
  -H "X-User-Token: $BOB" \
  -d '{
    "jsonrpc": "2.0",
    "id": 1,
    "method": "tools/call",
    "params": {
      "name": "search_repos",
      "arguments": { "visibility": "internal" }
    }
  }' 2>&1)
printf '%s\n' "$resp" | sed -n '1,20p'
