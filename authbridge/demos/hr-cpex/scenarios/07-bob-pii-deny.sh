#!/usr/bin/env bash
# Copyright 2026
# SPDX-License-Identifier: Apache-2.0
#
# Bob (HR, perm.email_send) tries to send an email whose body
# carries an SSN-like pattern. APL's coarse `require(perm.email_send)`
# passes (Bob has the perm), but the PII scanner plugin walks the
# args, detects the SSN pattern, and denies the call.
#
# This demonstrates a field-level plugin in action — caught BEFORE
# the email backend is touched. The audit-logger plugin still emits
# its observation record for the deny, so operators see the event.
#
# Expected:
#   * HTTP 403 with JSON error body, code = cpex.pii_detected — a CPEX
#     policy deny is surfaced by AuthBridge as a transport-level 403
#     (not a 500, not an MCP JSON-RPC envelope)
#   * Backend (hr-mcp) NEVER receives the call — the deny is
#     enforced at the gateway plugin layer
#   * stderr (from the gateway process) shows a JSON audit record
#     describing the denied send_email attempt

set -euo pipefail
source "$(dirname "$0")/_lib.sh"

step "Bob (HR + email_send) → send_email with SSN in body"
note "Expected: HTTP 403, code=cpex.pii_detected"
note "Triggered by: pii-scan plugin catches the SSN pattern in args"
note "Expected: audit-log still emits a record describing the deny"
note "Expected upstream: no inbound request (gateway plugin denied)"

BOB=$(mint bob)
CLIENT=$(mint hr-copilot)

curl -s -X POST "$GATEWAY/mcp" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $CLIENT" \
  -H "X-User-Token: $BOB" \
  -d '{
    "jsonrpc": "2.0",
    "id": 1,
    "method": "tools/call",
    "params": {
      "name": "send_email",
      "arguments": {
        "to": "external@example.com",
        "subject": "compensation update",
        "body": "FYI — Jane Smith. Her SSN is 555-12-3456 if you need to update payroll."
      }
    }
  }' -i 2>&1 | head -20
