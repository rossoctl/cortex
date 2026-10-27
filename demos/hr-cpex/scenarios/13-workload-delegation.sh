#!/usr/bin/env bash
# The agent acts as itself, with no person behind the call. get_headcount
# authenticates by the agent's SPIFFE JWT-SVID alone: the sidecar fetches
# it from SPIRE in process and presents it to CPEX as X-Workload-Token,
# so the call below carries no user or client token. The route's
# delegate(..., subject: caller_workload) runs two legs at Keycloak: the
# SVID as an RFC 7523 client assertion authenticates the agent's client
# (hr-copilot-agent, a CLIENT_LOGIN event), then an RFC 8693 exchange
# scopes that token to workday-api (a TOKEN_EXCHANGE event).
#
# Needs SPIRE (`make spire`). Without it the scenario is skipped: exit 0,
# or 1 with REQUIRE_SPIRE=1.

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

if ! curl -fsS --max-time 3 -o /dev/null "$SPIRE_OIDC/keys" 2>/dev/null; then
  step "Agent (workload) → get_headcount: skipped"
  note "SPIRE is not running (no keys at $SPIRE_OIDC/keys)."
  note "Run \`make spire\`, then restart \`make port-forward\`."
  [ "${REQUIRE_SPIRE:-0}" = "1" ] && { fail_ "REQUIRE_SPIRE=1 and SPIRE is absent"; finish; }
  exit 0
fi

step "Agent (workload) → get_headcount with no user or client token"
note "A stray X-User-Token and a spoofed X-Auth-User-Id ride along"
reset_upstream
logins=$(kc_event_count CLIENT_LOGIN hr-copilot-agent)
exchanges=$(kc_exchange_count)
_print_response "$(curl -isS --max-time 20 -x "$PROXY" -X POST "$MCP_TARGET" \
  -H "Content-Type: application/json" \
  -H "X-User-Token: stray" \
  -H "X-Auth-User-Id: bob" \
  --data "$(_tool_body get_headcount '{}')")"

after=$(kc_event_count CLIENT_LOGIN hr-copilot-agent)
if [ "$logins" = "?" ] || [ "$after" = "?" ]; then
  fail_ "could not read Keycloak's CLIENT_LOGIN events at $KEYCLOAK_HOST"
elif [ $((after - logins)) -eq 1 ]; then
  pass_ "leg 1: hr-copilot-agent authenticated by its SVID (RFC 7523)"
else
  fail_ "expected 1 hr-copilot-agent client login, Keycloak saw $((after - logins))"
fi

if delegation_available; then
  expect_exchanges "$exchanges" 1
  expect_rpc_ok
  expect_upstream_calls 1
  expect_upstream_audience workday-api
  expect_upstream_no_header x-user-token
  expect_upstream_no_header x-workload-token
  expect_upstream_no_header x-auth-user-id
else
  # The workload identity resolved (the route has no other) and both
  # legs ran; the sidecar does not forward the minted token.
  expect_delegation_pending "$exchanges"
fi
finish
