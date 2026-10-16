#!/usr/bin/env bash
# The agent reads .env. The secret reaches the model in the tool result, and
# cpex taints the session as it goes by, without blocking the turn. From
# then on the session sends nothing, a clean report included, and naming a
# fresh session from the command does not get out of it.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

SID="$(new_session s2)"
echo "$SID" >"$STATE_DIR/tainted-session"
step "S2: the agent reads .env, then files a clean report"
reset_mocks
run_agent 04-tainted-session "$SID"
expect_model_calls_ok
if llm_requests | jq -e --arg s "$SECRET" '[.requests[] | select(.body | contains($s))] | length > 0' >/dev/null; then
  pass_ "the turn carrying the .env went on to the model"
else
  fail_ "the model never received the turn carrying the .env"
fi
expect_cpex "$SID" observe cpex.secret_in_tool_result /v1/messages
expect_tool_denied 1 cpex.session_tainted_secret
note "the same report, sent with X-Session-Id and X-Claude-Code-Session-Id: fresh-session"
expect_tool_denied 2 cpex.session_tainted_secret
expect_cpex "$SID" deny cpex.session_tainted_secret /projects/A/reports
expect_support_reports 0
expect_secret_absent "the 403 responses" "$(jq -r '.tools[1:][].output' "$AGENT_OUT")"
note "the model calls carried the .env as sent; cpex's records do not"
expect_secret_absent "cpex's invocations" "$(cpex_invocations "$SID")"
expect_no_principal_on_wire
finish
