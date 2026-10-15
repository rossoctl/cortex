#!/usr/bin/env bash
# Point a real Claude Code session at the demo cortex, and check it arrived.
#
#   claude-code/preflight.sh settings   write .state/claude-code/settings.json
#                                       from settings.json.example
#   claude-code/preflight.sh wait       block until the demo session API
#                                       holds a session with a model reply
#                                       from a real Claude Code process
#   claude-code/preflight.sh check      the same, once; exit 1 if not yet
#
# Assumes `./run.sh up --claude-code`. CLAUDE_INFERENCE_HOST names the
# model API host when it is not api.anthropic.com (a gateway, say).

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEMO_DIR="$(cd "$HERE/.." && pwd)"
# shellcheck source=env.sh
source "$DEMO_DIR/env.sh"

INFERENCE_HOST="${CLAUDE_INFERENCE_HOST:-api.anthropic.com}"
SETTINGS="$STATE_DIR/claude-code/settings.json"

die() { echo "preflight: $*" >&2; exit 1; }
info() { echo "  ▸ $*"; }

demo_up() {
  curl -fsS --max-time 3 "$SESSION_API/v1/plugins" 2>/dev/null \
    | jq -e '.plugins[] | select(.name == "cpex")' >/dev/null \
    || die "no demo cortex with cpex at $SESSION_API; run ./run.sh up --claude-code"
  pgrep -f "$STATE_DIR/claude-code/cortex.yaml" >/dev/null \
    || die "the demo cortex is not running the Claude Code config; run ./run.sh down, then ./run.sh up --claude-code"
}

settings() {
  demo_up
  [ -f "$DEMO_CA" ] || die "no demo CA at $DEMO_CA"
  mkdir -p "$(dirname "$SETTINGS")"
  sed "s#__DEMO_DIR__#$DEMO_DIR#g" "$HERE/settings.json.example" >"$SETTINGS"
  jq -e . "$SETTINGS" >/dev/null || die "$SETTINGS is not valid JSON"
  info "wrote $SETTINGS"
  echo
  echo "  Start Claude Code from the demo workspace with it:"
  echo
  echo "    cd $WORKSPACE && claude --settings $SETTINGS"
  echo
  echo "  then run: $0 wait"
}

# The session ids whose agent is Claude Code, whose model calls went to the
# real API, and which got a 200 back. A curl naming itself claude-cli and
# sending an unauthenticated request gets a 401, so it does not count.
claude_sessions() {
  local sid
  curl -fsS --max-time 5 "$SESSION_API/v1/sessions" \
    | jq -r --arg h "$INFERENCE_HOST" '.sessions[]
        | select(.agent == "claude-code" and ((.inferenceHost // "") | split(":")[0]) == $h) | .id' \
    | while read -r sid; do
        curl -fsS --max-time 5 "$SESSION_API/v1/sessions/$sid?limit=2000" \
          | jq -e --arg h "$INFERENCE_HOST" '[.events[]
              | select(.phase == "response" and .inference != null and .statusCode == 200
                       and (.host | split(":")[0]) == $h)] | length > 0' >/dev/null \
          && echo "$sid"
      done
}

found() {
  local sids
  sids="$(claude_sessions)"
  [ -n "$sids" ] || return 1
  pgrep -x claude >/dev/null || info "note: no process named claude on this machine"
  info "Claude Code session(s) on the demo cortex:"
  # shellcheck disable=SC2086 # one id per line
  printf '      %s\n' $sids
  echo
  echo "  Taint and the reports it refuses are keyed on that session. Follow it:"
  echo "    curl -N \"$SESSION_API/v1/events?session=$(echo "$sids" | head -1)\""
}

wait_for_session() {
  demo_up
  info "waiting for a Claude Code session with a model reply from $INFERENCE_HOST (Ctrl-C to stop)"
  until found; do sleep 2; done
}

check() {
  demo_up
  found || die "no Claude Code session with a model reply from $INFERENCE_HOST on the demo cortex yet"
}

case "${1:-}" in
  settings) settings ;;
  wait) wait_for_session ;;
  check) check ;;
  *) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
