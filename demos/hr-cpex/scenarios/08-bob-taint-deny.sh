#!/usr/bin/env bash
# Session taint across two tool calls. get_compensation runs
# taint(secret, session); send_email refuses any session carrying the
# label, even with a clean body. Sessions are named by X-Session-Id, with
# fresh ids per run.
#
#   S1  clean session, send_email        -> sent
#   S2  new session, get_compensation    -> taints the session
#   S3  same session as S2, send_email   -> cpex.session_tainted_secret
#
# By default, S2 fails closed on its delegation, but the taint is
# written before that, so S3 is still denied.

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

BOB=$(mint bob)
CLIENT=$(mint hr-copilot)
CLEAN_BODY="Quarterly planning sync moved to Thursday."
SID_CLEAN="taint-demo-clean-$$-${RANDOM}"
SID_TAINT="taint-demo-tainted-$$-${RANDOM}"

step "S1 · Bob → send_email (untainted session, clean body)"
reset_upstream
SESSION_ID="$SID_CLEAN" call_send_email "$BOB" "$CLIENT" "$CLEAN_BODY"
expect_rpc_ok
expect_upstream_calls 1

step "S2 · Bob → get_compensation (taints session $SID_TAINT)"
reset_upstream
before=$(kc_exchange_count)
SESSION_ID="$SID_TAINT" call_get_compensation "$BOB" "$CLIENT" true
if delegation_available; then
  expect_rpc_ok
else
  expect_delegation_pending "$before"
fi

step "S3 · Bob → send_email (same session as S2, clean body)"
note "Denied by the session taint, not the content"
reset_upstream
SESSION_ID="$SID_TAINT" call_send_email "$BOB" "$CLIENT" "$CLEAN_BODY"
expect_deny cpex.session_tainted_secret
expect_upstream_calls 0
finish
