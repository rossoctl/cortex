#!/usr/bin/env bash
# Copyright 2026
# SPDX-License-Identifier: Apache-2.0
#
# Taint propagation across two tool calls. Reading compensation taints
# the CPEX session with the label "secret" (get_compensation policy:
# `taint(secret, session)`). The send_email policy then refuses to send
# external email from any session carrying that label
# (`session.labels contains "secret": deny`). The deny fires even when
# the email body is perfectly clean — it's the SESSION that's tainted,
# not the content. This is distinct from scenario 07, where the PII
# scanner denies based on the body's contents.
#
# Sessions are correlated by an explicit X-Session-Id header (threaded
# into CPEX's Agent.SessionID). Each run uses fresh, unique ids so the
# scenario is isolated and reproducible.
#
# Three steps:
#   S1  clean session, clean body          → 200  (baseline: email works)
#   S2  new session, get_compensation      → 200  (taints session "secret")
#   S3  SAME session as S2, clean body     → 403  cpex.session_tainted_secret
#
# Watch the gateway with:  kubectl -n cpex-demo logs -f deploy/hr-cpex-agent -c authbridge-cpex

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

BOB=$(mint bob)
CLIENT=$(mint hr-copilot)

CLEAN_BODY="Quarterly planning sync moved to Thursday."
SID_CLEAN="taint-demo-clean-$$-${RANDOM}"
SID_TAINT="taint-demo-tainted-$$-${RANDOM}"

step "S1 · Bob → send_email (untainted session, clean body)"
note "Session: $SID_CLEAN (never touched secret data)"
note "Expected: 200 OK — email allowed (require(perm.email_send) ✓, pii-scan ✓, session clean)"
SESSION_ID="$SID_CLEAN" call_send_email "$BOB" "$CLIENT" "$CLEAN_BODY"

step "S2 · Bob → get_compensation (taints the session)"
note "Session: $SID_TAINT"
note "Expected: 200 OK — and the policy's taint(secret, session) marks this session"
SESSION_ID="$SID_TAINT" call_get_compensation "$BOB" "$CLIENT" true

step "S3 · Bob → send_email (SAME session as S2, clean body)"
note "Session: $SID_TAINT (now carries label \"secret\" from S2)"
note "Expected: HTTP 403, code=cpex.session_tainted_secret"
note "Denied by the session taint — NOT by pii-scan (the body has no PII)"
note "This is the cross-tool data-flow control: looked at secrets → can't email out"
SESSION_ID="$SID_TAINT" call_send_email "$BOB" "$CLIENT" "$CLEAN_BODY"
