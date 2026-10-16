#!/usr/bin/env bash
# Record the walkthrough as a three-pane tmux video: the narrated run on the
# left, agentop following the current session top right, and the policy
# excerpt for the current scenario bottom right.
#
#   demo/record_tmux.sh [--pace N] [--out PATH]   # default: --pace 4,
#                                                 # .sketchpad/demo-video/coding-agent-cpex-tmux
#
# Writes PATH.cast (asciinema), PATH.gif (agg) and PATH.mp4 (ffmpeg).
# Needs `./run.sh up`, tmux, asciinema 3, agg, ffmpeg and bin/agentop
# (`make agentop`). It clears the demo cortex's session list first, and
# runs agentop with a HOME under .state/, so it reads nothing in ~/.cortex
# or ~/.claude. tmux runs on its own socket.
#
# The walkthrough calls this script as its WALKTHROUGH_HOOK:
#   demo/record_tmux.sh scenario <name>        the policy pane switches
#   demo/record_tmux.sh session <label> <id>   agentop moves to that session

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEMO_DIR="$(cd "$HERE/.." && pwd)"
REPO="$(cd "$DEMO_DIR/../.." && pwd)"
# shellcheck source=env.sh
source "$DEMO_DIR/env.sh"

SOCK=coding-agent-demo
NAME=coding-agent-demo
REC="$STATE_DIR/record"
AGENTOP="${AGENTOP_BIN:-$REPO/bin/agentop}"
COLS=200
ROWS=50

die() { echo "record_tmux.sh: $*" >&2; exit 1; }
tm() { tmux -L "$SOCK" "$@"; }

# --- the hook ----------------------------------------------------------------

case "${1:-}" in
  scenario) echo "$2" >"$REC/slide"; exit 0 ;;
  session) echo "$3" >"$REC/session"; exit 0 ;;
esac

# --- agentop follows the session the walkthrough opened -------------------------

AGENTOP_PANE="$NAME:0.1"

# The first line agentop draws: its endpoint on the sessions list, the
# session's title and id on its events view.
agentop_header() { tm capture-pane -p -t "$AGENTOP_PANE" 2>/dev/null | head -1; }

session_listed() {
  curl -fsS --max-time 2 "$SESSION_API/v1/sessions" 2>/dev/null \
    | jq -e --arg id "$1" 'any(.sessions[]; .id == $id)' >/dev/null
}

# to_list: back out to the sessions list.
to_list() {
  local _
  for _ in 1 2 3; do
    agentop_header | grep -qF "$SESSION_API" && return 0
    tm send-keys -t "$AGENTOP_PANE" Escape; sleep 0.3
  done
}

# show <id>: once the session is listed, switch to it in one quick move. The
# previous session stays on screen until then. The new one is the newest, so
# it is the top row; search for it only if something newer got there first.
# A session opens at its newest event and follows the tail from there.
show() {
  local id="$1" _
  for _ in $(seq 1 50); do session_listed "$id" && break; sleep 0.1; done
  to_list
  # A search left over from a fallback below would pin the cursor; clear it.
  if tm capture-pane -p -t "$AGENTOP_PANE" | grep -qF '[/'; then
    tm send-keys -t "$AGENTOP_PANE" Escape; sleep 0.1
  fi
  tm send-keys -t "$AGENTOP_PANE" g Enter; sleep 0.4
  agentop_header | grep -qF "$id" && return 0
  for _ in 1 2; do
    to_list
    tm send-keys -t "$AGENTOP_PANE" / && sleep 0.05
    tm send-keys -t "$AGENTOP_PANE" -l "$id" && sleep 0.05
    tm send-keys -t "$AGENTOP_PANE" Enter && sleep 0.1
    tm send-keys -t "$AGENTOP_PANE" Enter && sleep 0.3
    agentop_header | grep -qF "$id" && return 0
  done
  return 0
}

follow() {
  local current="" id
  while tm has-session -t "$NAME" 2>/dev/null; do
    id="$(cat "$REC/session" 2>/dev/null || true)"
    if [ -n "$id" ] && [ "$id" != "$current" ]; then
      current="$id"
      show "$id"
    fi
    sleep 0.1
  done
}

# --- recording -----------------------------------------------------------------

PACE=4
OUT="$REPO/.sketchpad/demo-video/coding-agent-cpex-tmux"
while [ $# -gt 0 ]; do
  case "$1" in
    --pace) PACE="${2:?--pace needs seconds}"; shift ;;
    --out) OUT="${2:?--out needs a path}"; shift ;;
    -h|--help) sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
  shift
done

for t in tmux asciinema agg ffmpeg curl jq python3; do
  command -v "$t" >/dev/null || die "$t is required"
done
[ -x "$AGENTOP" ] || die "no agentop at $AGENTOP; run \`make agentop\` from $REPO"
curl -fsS --max-time 3 "$SESSION_API/v1/plugins" 2>/dev/null | jq -e '.plugins[] | select(.name == "cpex")' >/dev/null \
  || die "no demo cortex with cpex at $SESSION_API; run ./run.sh up"
tm has-session -t "$NAME" 2>/dev/null && die "a recording is already running (tmux -L $SOCK attach -t $NAME)"

mkdir -p "$REC/home" "$(dirname "$OUT")"
rm -f "$REC/slide" "$REC/session" "$REC/go"
# The table the video needs, narrower than agentop's default.
cat >"$REC/agentop.yaml" <<'YAML'
events:
  columns:
    - {name: TIME, visible: false}
    - {name: DIR, visible: false}
    - {name: METHOD, visible: false}
    - {name: DURATION, visible: false}
    - {name: BYTES, visible: false}
    - {name: TOKENS, visible: false}
    - {name: COST, visible: false}
YAML
# Start from an empty session list, so the video shows only its own sessions.
curl -fsS -X DELETE --max-time 15 "$SESSION_API/v1/sessions" >/dev/null || die "could not clear the session list"

q() { printf '%q' "$1"; }
walk="cd $(q "$DEMO_DIR") && while [ ! -f $(q "$REC/go") ]; do sleep 0.2; done; clear; \
AGENT_TURN_DELAY=1.5 AGENT_OPEN_DELAY=2 WALKTHROUGH_HOOK=$(q "$HERE/record_tmux.sh") ./walkthrough.sh --pace $(q "$PACE"); \
sleep $(q "$PACE"); tmux -L $SOCK kill-session -t $NAME"
agentop="HOME=$(q "$REC/home") CLAUDE_CONFIG_DIR=$(q "$REC/home/.claude") $(q "$AGENTOP") observe \
--skip-claude-metadata --endpoint $(q "$SESSION_API") --prefs $(q "$REC/agentop.yaml")"
slide="python3 $(q "$HERE/policy_slide.py") --watch $(q "$REC/slide")"

tm -f /dev/null new-session -d -s "$NAME" -x "$COLS" -y "$ROWS" "bash -c $(q "$walk")"
tm set -g status off
tm set -g pane-border-status top
tm set -g pane-border-format ' #{pane_title} '
tm set -g pane-active-border-style fg=colour244
tm set -g pane-border-style fg=colour240
tm split-window -h -t "$NAME:0.0" -l 90 "$agentop"
tm split-window -v -t "$NAME:0.1" -l 23 "$slide"
tm select-pane -t "$NAME:0.0" -T "walkthrough"
tm select-pane -t "$NAME:0.1" -T "agentop: the current session"
tm select-pane -t "$NAME:0.2" -T "policy in play"
tm select-pane -t "$NAME:0.0"

follow &
FOLLOW=$!
trap 'kill "$FOLLOW" 2>/dev/null || true; tm kill-session -t "$NAME" 2>/dev/null || true' EXIT
(sleep 3; touch "$REC/go") &

asciinema rec --overwrite --headless --window-size "${COLS}x${ROWS}" --idle-time-limit 5 \
  -c "tmux -L $SOCK attach -t $NAME" "$OUT.cast"
echo "cast: $OUT.cast"

agg -q --font-size 14 --idle-time-limit 5 "$OUT.cast" "$OUT.gif"
echo "gif:  $OUT.gif"
ffmpeg -y -loglevel error -i "$OUT.gif" -movflags faststart -pix_fmt yuv420p \
  -vf "scale=trunc(iw/2)*2:trunc(ih/2)*2" "$OUT.mp4"
echo "mp4:  $OUT.mp4"
