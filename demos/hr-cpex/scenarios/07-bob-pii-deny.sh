#!/usr/bin/env bash
# Bob (HR, perm.email_send) sends an email whose body carries an SSN. The
# role check passes; the CEL step on send_email matches the SSN shape and
# denies. The deny is recorded on the Cortex session timeline, which is
# the demo's audit trail.

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

step "Bob (HR + email_send) → send_email with an SSN in the body"
note "Denied by the CEL PII step; the timeline records the decision"

BOB=$(mint bob)
CLIENT=$(mint hr-copilot)

reset_upstream
before=$(timeline_denies cpex.pii_detected)
call_send_email "$BOB" "$CLIENT" \
  "FYI — Jane Smith. Her SSN is 555-12-3456 if you need to update payroll." \
  "compensation update" "external@example.com"

expect_deny cpex.pii_detected
expect_upstream_calls 0
expect_timeline_deny cpex.pii_detected "$before"
finish
