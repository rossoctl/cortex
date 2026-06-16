#!/usr/bin/env bash
# Copyright 2026
# SPDX-License-Identifier: Apache-2.0
#
# Cross-principal session-taint isolation (security review Finding 2).
#
# Session-scoped taint (scenario 08) persists in CPEX's session store
# keyed by the session id AuthBridge threads from the X-Session-Id
# header. If that key were the raw header value, ANY caller could read
# or pollute another principal's session labels just by reusing their
# session id — e.g. send mail "from" a session someone else tainted, or
# escape a deny by guessing a clean id. The fix (../cpex
# session_resolver.rs) binds the key to the authenticated subject:
# sha256(subject_id : X-Session-Id). So the SAME session id under two
# different users resolves to two different buckets.
#
# This scenario proves it with two principals sharing ONE session id:
#   victim   = eve  (role.hr → may call get_compensation, taints session)
#   attacker = bob  (perm.email_send → may call send_email)
#
# Steps:
#   S1  bob → send_email, fresh session, clean body   → 200  (baseline)
#   S2  eve → get_compensation, session SHARED        → 200  (taints EVE's
#                                                             session "secret")
#   S3  bob → send_email, SAME SHARED session, clean  → 200  (bob does NOT
#                                                             inherit eve's
#                                                             taint — the proof)
#
# Contrast with scenario 08, where bob reusing his OWN tainted session
# is denied (cpex.session_tainted_secret). Same id, different subject =
# different outcome. Pre-fix, S3 would have been denied.
#
# Watch the gateway with:  kubectl -n cpex-demo logs -f deploy/hr-cpex-agent -c authbridge-cpex

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

EVE=$(mint eve)
BOB=$(mint bob)
CLIENT=$(mint hr-copilot)

CLEAN_BODY="Quarterly planning sync moved to Thursday."
SID_BASELINE="xprin-baseline-$$-${RANDOM}"
SID_SHARED="xprin-shared-$$-${RANDOM}"

step "S1 · Bob → send_email (fresh session, clean body)"
note "Session: $SID_BASELINE (never touched secret data)"
note "Expected: 200 OK — baseline, bob can send mail from a clean session"
SESSION_ID="$SID_BASELINE" call_send_email "$BOB" "$CLIENT" "$CLEAN_BODY"

step "S2 · Eve → get_compensation (taints EVE's session)"
note "Session: $SID_SHARED  (this id is bound to eve's subject)"
note "Expected: 200 OK — eve has role.hr; taint(secret, session) marks H(eve:$SID_SHARED)"
SESSION_ID="$SID_SHARED" call_get_compensation "$EVE" "$CLIENT" true

step "S3 · Bob → send_email, SAME session id as eve used, clean body"
note "Session: $SID_SHARED  (same string — but bound to bob's subject now)"
note "Expected: 200 OK — bob's key H(bob:$SID_SHARED) != eve's H(eve:$SID_SHARED),"
note "so bob does NOT inherit eve's \"secret\" taint. THIS is the Finding 2 fix."
note "Pre-fix (raw session key) this would have been denied (cpex.session_tainted_secret)."
SESSION_ID="$SID_SHARED" call_send_email "$BOB" "$CLIENT" "$CLEAN_BODY"
