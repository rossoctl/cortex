#!/usr/bin/env bash
# With ./run.sh up --custody, blocked reports never trigger a Vault lookup.
# If Vault is unavailable, otherwise valid reports are also blocked.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"
require_custody

SID="$(new_session s1)"
step "S1 · reports to project B, and with a secret, while custody is on"
reset_mocks
before="$(vault_logins)"
run_agent 08-custody-deny "$SID"
expect_tool_denied 0 cpex.project_not_authorized
expect_tool_denied 1 cpex.secret_in_body
expect_vault_logins "$before" 0
expect_support_reports 0

SID="$(new_session s1)"
step "S1 · a clean report to project A while Vault is down"
reset_mocks
compose_() { VAULT_ROOT_TOKEN=unused docker compose -f "$DEMO_DIR/docker-compose.yml" "$@"; }
trap 'compose_ unpause vault >/dev/null 2>&1 || true' EXIT
compose_ pause vault >/dev/null 2>&1
run_agent 02-clean-report "$SID"
compose_ unpause vault >/dev/null 2>&1
expect_tool_denied 0 cpex.delegation_vault_timeout
expect_support_reports 0
finish
