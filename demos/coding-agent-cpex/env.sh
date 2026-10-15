# shellcheck shell=bash disable=SC2034
# Addresses and paths shared by run.sh, the agent and the scenarios.
DEMO_DIR="${DEMO_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
STATE_DIR="$DEMO_DIR/.state"
IDENTITY_DIR="$DEMO_DIR/config/identity"

CORTEX_PORTS="47700 47701 47702 47703 47704"
LLM_PORT=47710
SUPPORT_PORT=47711
VAULT_PORT=47712

export DEMO_DIR
export DEMO_STATE="$STATE_DIR"
export DEMO_PROXY="http://127.0.0.1:47700"
export SESSION_API="http://127.0.0.1:47701"
export HEALTH_URL="http://127.0.0.1:47703"
export LLM_URL="https://llm.localtest.me:$LLM_PORT"
export SUPPORT_URL="https://support.localtest.me:$SUPPORT_PORT"
export DEMO_CA="$STATE_DIR/cortex-ca/ca.crt"
export MOCK_CA="$STATE_DIR/mock-ca/ca.crt"
export SRT_SETTINGS="$STATE_DIR/srt-settings.json"
export WORKSPACE="$STATE_DIR/workspace"
export VAULT_ADDR="http://127.0.0.1:$VAULT_PORT"
# Present while `run.sh up --custody` is in effect.
export CUSTODY_MARK="$STATE_DIR/custody/enabled"
