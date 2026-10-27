#!/usr/bin/env bash
# Narrated run of every hr-cpex scenario, in acts, with what to watch for
# before each one. Each scenario asserts its own outcome; the walkthrough
# ends with a summary and exits non-zero if any scenario failed.
#
#   ./walkthrough.sh             # pause for Enter between beats (q quits)
#   ./walkthrough.sh --fast      # no pauses
#   ./walkthrough.sh --pace 3    # advance on its own every 3 seconds
#
# Assumes `make deploy` and `make port-forward`; takes the same endpoint
# variables as the scenarios (PROXY, KEYCLOAK_HOST, SESSION_API, UPSTREAM,
# AUTH_CHANNEL, SPIRE_OIDC, CPEX_DELEGATION). `make walkthrough` sets them.
#
# For an audience, a second terminal on what reached the tool helps:
#   kubectl -n cpex-demo logs -f deploy/hr-mcp

set -euo pipefail

MODE="wait"
PACE=0
while [ $# -gt 0 ]; do
  case "$1" in
    --fast) MODE=fast ;;
    --pace) MODE=pace; PACE="${2:?--pace needs seconds}"; shift ;;
    -h|--help) sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
  shift
done

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scenarios/_lib.sh
source "$DIR/scenarios/_lib.sh"

cyan() { printf '\033[36m%s\033[0m' "$*"; }
dim()  { printf '\033[2m%s\033[0m' "$*"; }

act() {
  echo
  echo "$(cyan ════════════════════════════════════════════════════════════)"
  echo "$(cyan "  $*")"
  echo "$(cyan ════════════════════════════════════════════════════════════)"
}

beat() { note "$@"; }

pause() {
  case "$MODE" in
    fast) return ;;
    pace) sleep "$PACE"; return ;;
  esac
  echo
  printf '  %s ' "$(dim '[enter to continue, q to quit]')"
  local ans=""
  read -r ans || true
  if [ "$ans" = "q" ]; then exit 0; fi
}

PASSED=()
FAILED=()

# run <scenario file>: run one scenario and record its result.
run() {
  pause
  if bash "$DIR/scenarios/$1"; then PASSED+=("$1"); else FAILED+=("$1"); fi
}

# ── Pre-flight ──────────────────────────────────────────────────────

act "Pre-flight"
ok=1
if curl -fsS --max-time 3 -o /dev/null "$SESSION_API/healthz" 2>/dev/null; then
  pass_ "sidecar session API at $SESSION_API"
else
  fail_ "no sidecar session API at $SESSION_API (is \`make port-forward\` running?)"; ok=0
fi
if [ -n "$(mint bob 2>/dev/null)" ]; then
  pass_ "Keycloak mints persona tokens at $KEYCLOAK_HOST"
else
  fail_ "Keycloak at $KEYCLOAK_HOST did not mint a token for bob"; ok=0
fi
if curl -fsS --max-time 3 -o /dev/null "$UPSTREAM/_requests" 2>/dev/null; then
  pass_ "hr-mcp recorder at $UPSTREAM"
else
  fail_ "no hr-mcp recorder at $UPSTREAM"; ok=0
fi
[ "$ok" = 1 ] || { echo; echo "  Fix the above and re-run."; exit 1; }
beat "Every call below goes through the agent's sidecar forward proxy,"
beat "the same egress path the agent's own tool calls take."
beat "Delegation mode: $CPEX_DELEGATION"

# ── Act 1 ───────────────────────────────────────────────────────────

act "Act 1 · Workday: a role gate, a token exchange, redaction"
beat "01 Bob (HR) reads compensation. require(role.hr) passes and"
beat "   delegate() exchanges his token for a workday-api token at Keycloak."
delegation_available || beat "   by default CPEX keeps the minted token, so the sidecar fails closed."
run 01-bob-allow.sh

beat "02 Alice (engineering) asks the same. The role gate denies first:"
beat "   no exchange at Keycloak, nothing reaches hr-mcp."
run 02-alice-deny.sh

beat "03 Eve (HR, no view_ssn) sends Bob's exact request. The policy"
beat "   redacts the SSN in her arguments and in the tool's result."
run 03-eve-redact.sh

# ── Act 2 ───────────────────────────────────────────────────────────

act "Act 2 · GitHub: APL, then Cedar, then the IdP"
beat "04 Alice searches an internal repo: the team gate, Cedar"
beat "   (engineer x internal), the exchange and the granted-scope check."
run 04-alice-internal-allow.sh

beat "05 Alice searches an external repo. Cedar has no permit for it,"
beat "   so it denies by default, before any exchange."
run 05-alice-external-cedar-deny.sh

beat "06 Bob searches repos. The cheap APL team gate stops him;"
beat "   Cedar never runs."
run 06-bob-apl-deny.sh

# ── Act 3 ───────────────────────────────────────────────────────────

act "Act 3 · Email: content checks and session taint"
beat "07 Bob emails an SSN. A CEL step matches its shape and denies,"
beat "   and the deny lands on the Cortex session timeline."
run 07-bob-pii-deny.sh

beat "08 Reading compensation taints the session; a clean email from"
beat "   that session is then refused. The session is tainted, not the text."
run 08-bob-taint-deny.sh

beat "09 Taint is keyed by subject and session id: Eve tainting a shared"
beat "   id does not stop Bob."
run 09-cross-principal-taint-isolation.sh

# ── Act 4 ───────────────────────────────────────────────────────────

act "Act 4 · A human in the loop"
beat "10 A \$5,000 change applies at once. A string amount, a NaN or UTF-16"
beat "   body only a lenient parser reads, and an unrouted tool are refused."
run 10-adjust-under-threshold.sh

beat "11 A \$15,000 change waits for Bob's manager, Alice, over CIBA."
beat "   The call is suspended (-32120), not denied; the approval is"
beat "   spent by the call it lets through and capped by the route's scope."
run 11-adjust-needs-approval.sh

# ── Act 5 ───────────────────────────────────────────────────────────

act "Act 5 · What the tool is told"
beat "12 The engine asserts the caller to hr-mcp in x-auth-* headers and"
beat "   strips the raw user token; a spoofed x-auth-user-id is overwritten."
run 12-assertions.sh

# ── Act 6 ───────────────────────────────────────────────────────────

act "Act 6 · The agent as itself"
beat "13 No person behind the call: the sidecar presents the agent's SPIFFE"
beat "   JWT-SVID, which Keycloak takes as a client assertion before the"
beat "   exchange. Skipped without \`make spire\`."
run 13-workload-delegation.sh

# ── Summary ─────────────────────────────────────────────────────────

act "Summary"
for s in ${PASSED[@]+"${PASSED[@]}"}; do pass_ "$s"; done
for s in ${FAILED[@]+"${FAILED[@]}"}; do fail_ "$s"; done
echo
echo "  ${#PASSED[@]} passed, ${#FAILED[@]} failed"
[ "${#FAILED[@]}" -eq 0 ]
