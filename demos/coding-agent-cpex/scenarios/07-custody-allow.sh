#!/usr/bin/env bash
# With ./run.sh up --custody, Cortex fetches the required support credential
# from Vault as the developer, after policy approval. The agent never holds
# it, and Cortex replaces any Authorization value the agent supplies.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"
require_custody

SID="$(new_session s1)"
step "S1 · the agent files a clean report with project A, holding no credential"
reset_mocks
before="$(vault_logins)"
run_agent 07-custody-allow "$SID"
expect_tool_status 0 201
expect_tool_status 1 201
expect_cpex "$SID" modify "policy modified the message" /projects/A/reports
expect_support_reports 2
expect_support_credential
expect_vault_logins "$before" 2
expect_credential_contained "$SID"
expect_no_principal_on_wire
finish
