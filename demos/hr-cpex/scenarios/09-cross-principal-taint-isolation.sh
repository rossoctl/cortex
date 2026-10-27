#!/usr/bin/env bash
# Session taint is bound to the subject: CPEX keys session state on
# H(subject : session id), so the same X-Session-Id under two users is
# two sessions.
#
#   S1  bob, fresh session, send_email          -> sent (baseline)
#   S2  eve, shared id, get_compensation        -> taints eve's session
#   S3  bob, same shared id, send_email         -> sent: bob does not
#                                                  inherit eve's taint
#
# Contrast scenario 08, where Bob reusing his own tainted session is
# denied. By default, S2 fails closed on its delegation after writing
# the taint.

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

EVE=$(mint eve)
BOB=$(mint bob)
CLIENT=$(mint hr-copilot)
CLEAN_BODY="Quarterly planning sync moved to Thursday."
SID_BASELINE="xprin-baseline-$$-${RANDOM}"
SID_SHARED="xprin-shared-$$-${RANDOM}"

step "S1 · Bob → send_email (fresh session, clean body)"
reset_upstream
SESSION_ID="$SID_BASELINE" call_send_email "$BOB" "$CLIENT" "$CLEAN_BODY"
expect_rpc_ok
expect_upstream_calls 1

step "S2 · Eve → get_compensation (taints eve's session $SID_SHARED)"
reset_upstream
before=$(kc_exchange_count)
SESSION_ID="$SID_SHARED" call_get_compensation "$EVE" "$CLIENT" true
if delegation_available; then
  expect_rpc_ok
else
  expect_delegation_pending "$before"
fi

step "S3 · Bob → send_email, same session id as eve, clean body"
note "Bob's session H(bob:id) is not eve's H(eve:id), so it is clean"
reset_upstream
SESSION_ID="$SID_SHARED" call_send_email "$BOB" "$CLIENT" "$CLEAN_BODY"
expect_rpc_ok
expect_upstream_calls 1
finish
