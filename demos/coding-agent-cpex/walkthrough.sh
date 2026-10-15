#!/usr/bin/env bash
# Narrated run of every coding-agent scenario, in acts, with what to watch
# for before each one. Each scenario asserts its own outcome; the
# walkthrough ends with a summary and exits non-zero if any failed.
#
#   ./walkthrough.sh             # pause for Enter between beats (q quits)
#   ./walkthrough.sh --fast      # no pauses
#   ./walkthrough.sh --pace 3    # advance on its own every 3 seconds
#
# Assumes `./run.sh up`. For an audience, a second terminal on what the
# support service received helps:
#   tail -f .state/logs/mock-support.log

set -euo pipefail

MODE="wait"
PACE=0
while [ $# -gt 0 ]; do
  case "$1" in
    --fast) MODE=fast ;;
    --pace) MODE=pace; PACE="${2:?--pace needs seconds}"; shift ;;
    -h|--help) sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
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
  cyan ════════════════════════════════════════════════════════════; echo
  cyan "  $*"; echo
  cyan ════════════════════════════════════════════════════════════; echo
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
beat "The agent's model calls go to the demo cortex over HTTPS_PROXY."
beat "Its shell commands run under srt, whose only route out is the same"
beat "cortex. The developer's identity is held by cortex, not the agent."

# ── Act 1 ───────────────────────────────────────────────────────────

act "Act 1 · The sandbox is the boundary"
beat "00 A command that ignores the proxy cannot connect, and the token"
beat "   cortex holds cannot be read from inside the sandbox."
run 00-sandbox-boundary.sh

beat "01 A report to paste.evil.example is refused at the tunnel. A"
beat "   sandbox allowlist does this too: parity, not the point."
run 01-unapproved-host-deny.sh

# ── Act 2 ───────────────────────────────────────────────────────────

act "Act 2 · Allowed destination, unauthorized disclosure"
beat "02 A clean report to project A on the support service: 201."
run 02-clean-report-allow.sh

beat "03 Same host, same project, and a report carrying an AWS key. The"
beat "   sandbox allows the connection; the policy refuses the disclosure."
beat "   Padding the key past the excerpt, or gzipping it, is refused too."
run 03-secret-in-report-deny.sh

beat "04 The agent reads .env. The model turn carrying it goes through and"
beat "   taints the session; after that even a clean report is refused,"
beat "   and a header naming a fresh session does not get out of it."
run 04-tainted-session-deny.sh

beat "05 A clean report to project B. The support service would take it;"
beat "   the developer is on A only, so the policy does not."
run 05-wrong-project-deny.sh

beat "06 The same clean report from a new session goes through: the taint"
beat "   belongs to the session that read the secret, not the developer."
run 06-fresh-session-allow.sh

# ── Act 3 ───────────────────────────────────────────────────────────

act "Act 3 · Before the agent acts"
beat "09 In a tainted session the model proposes a curl pasting the key."
beat "   cpex refuses the reply, so the agent never runs it. JSON and SSE."
run 09-intent-deny.sh

# ── Summary ─────────────────────────────────────────────────────────

act "Summary"
for s in ${PASSED[@]+"${PASSED[@]}"}; do pass_ "$s"; done
for s in ${FAILED[@]+"${FAILED[@]}"}; do fail_ "$s"; done
echo
echo "  ${#PASSED[@]} passed, ${#FAILED[@]} failed"
beat "The sandbox correctly allowed the connection; the policy correctly"
beat "rejected the disclosure."
[ "${#FAILED[@]}" -eq 0 ]
