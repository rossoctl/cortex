#!/usr/bin/env bash
# Run all scenarios with narration and a final summary.
# Each scenario checks its outcome. The walkthrough exits non-zero
# if any scenario fails.
#
#   ./walkthrough.sh             # pause for Enter between scenarios (q quits)
#   ./walkthrough.sh --fast      # no pauses
#   ./walkthrough.sh --pace 3    # advance automatically every 3 seconds
#   ./walkthrough.sh --only 03   # only the scenarios whose names start 03
#                                # (repeatable)
#   ./walkthrough.sh --presenter # condensed output for a recording (also
#                                # PRESENTER=1); --hold N waits N seconds
#                                # after each scenario (default 4)
#
# Assumes `./run.sh up`. When WALKTHROUGH_HOOK is set, it is run as
# `$WALKTHROUGH_HOOK scenario <name>` before each scenario (and
# `scenario summary` before the summary), as
# `$WALKTHROUGH_HOOK session <label> <id>` when a scenario opens a session,
# and by the agent as `$WALKTHROUGH_HOOK verdict ...` (see agent/agent.py).

set -euo pipefail

MODE="wait"
PACE=0
HOLD=4
ONLY=()
while [ $# -gt 0 ]; do
  case "$1" in
    --fast) MODE=fast ;;
    --pace) MODE=pace; PACE="${2:?--pace needs seconds}"; shift ;;
    --only) ONLY+=("${2:?--only needs a scenario prefix}"); shift ;;
    --presenter) export PRESENTER=1 ;;
    --hold) HOLD="${2:?--hold needs seconds}"; shift ;;
    -h|--help) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
  shift
done

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scenarios/_lib.sh
source "$DIR/scenarios/_lib.sh"

bold() { printf '\033[1m%s\033[0m' "$*"; }
dim()  { printf '\033[2m%s\033[0m' "$*"; }

# Buffer narration so --only prints it only for selected scenarios.
# Presenter mode uses the recording's captions instead.
PENDING=""
section() { PENDING+=$'\n'"$(bold "$*")"$'\n'; }
say() { presenter || PENDING+="  $*"$'\n'; }
flush() { printf '%s' "$PENDING"; PENDING=""; }

selected() {
  [ "${#ONLY[@]}" -eq 0 ] && return 0
  local o
  for o in "${ONLY[@]}"; do [[ "$1" == "$o"* ]] && return 0; done
  return 1
}

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
  if ! selected "$1"; then PENDING=""; return 0; fi
  # Presenter mode starts each scenario on a clean pane.
  if presenter; then printf '\033[H\033[2J'; fi
  flush
  walkthrough_hook scenario "${1%.sh}"
  pause
  if bash "$DIR/scenarios/$1"; then PASSED+=("$1"); else FAILED+=("$1"); fi
  if presenter && [ "$MODE" != fast ]; then sleep "$HOLD"; fi
}

section "Is the demo up?"
# Presenter mode summarizes startup checks on one line.
if presenter; then PENDING=""; else flush; fi
ok=1
if curl -fsS --max-time 3 "$SESSION_API/v1/plugins" 2>/dev/null | jq -e '.plugins[] | select(.name == "cpex")' >/dev/null; then
  pass_ "demo cortex with cpex at $SESSION_API"
else
  fail_ "no demo cortex with cpex at $SESSION_API (run ./run.sh up)"; ok=0
fi
for url in "$LLM_URL" "$SUPPORT_URL"; do
  if curl -fsS --max-time 3 --cacert "$MOCK_CA" -o /dev/null "$url/_requests" 2>/dev/null; then
    pass_ "mock at $url"
  else
    fail_ "no mock at $url"; ok=0
  fi
done
[ "$ok" = 1 ] || { echo; echo "  Fix the above and re-run."; exit 1; }
presenter && printf '  \033[2mdemo is running\033[0m\n'
CHECKS=0
say "The agent's model calls and its sandboxed commands both go through the"
say "demo cortex, which holds the developer's identity."

section "The sandbox"
say "00: a command that bypasses the proxy cannot connect, and the sandbox"
say "    cannot read the developer token."
run 00-sandbox-boundary.sh

say "01: a report to an unapproved host is refused at the tunnel, as a"
say "    sandbox allowlist would refuse it, and so is the same upload whose"
say "    Host header names the support service."
run 01-unapproved-host-deny.sh

section "What leaves through an allowed host"
say "02: a clean report to project A goes through."
run 02-clean-report-allow.sh

say "03: a report carrying an AWS key is refused, and so is one padded past"
say "    the inspected excerpt or gzipped."
run 03-secret-in-report-deny.sh

say "04: the agent reads .env. That model call goes through and taints the"
say "    session; every report after it is refused."
run 04-tainted-session-deny.sh

say "05: a clean report to project B is refused: the developer is on A only."
run 05-wrong-project-deny.sh

say "06: the same report from a new session goes through: the taint belongs"
say "    to the session that read the secret."
run 06-fresh-session-allow.sh

# Credential custody runs only under ./run.sh up --custody.
if [ -f "$CUSTODY_MARK" ]; then
  section "Credential custody"
  say "07: the support service now requires a credential the agent does not"
  say "    hold. Cortex reads it from Vault after the allow and attaches it."
  run 07-custody-allow.sh

  say "08: a refused report never reaches Vault; with Vault not answering, an"
  say "    allowed report is refused rather than sent without the credential."
  run 08-custody-no-credential-on-deny.sh
fi

section "The model's reply"
say "09: in a tainted session the model proposes a curl carrying the key."
say "    cpex refuses the reply, so the agent never runs it."
run 09-intent-deny.sh

walkthrough_hook scenario summary
section "Summary"
flush
for s in ${PASSED[@]+"${PASSED[@]}"}; do pass_ "$s"; done
for s in ${FAILED[@]+"${FAILED[@]}"}; do fail_ "$s"; done
echo
echo "  ${#PASSED[@]} passed, ${#FAILED[@]} failed"
[ "${#FAILED[@]}" -eq 0 ]
