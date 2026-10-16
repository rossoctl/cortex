#!/usr/bin/env bash
# Record the walkthrough, policy rules, and agentop in a tmux video.
# Captions appear at the top, with the walkthrough below on the left.
# Policy rules and agentop share the right column.
# Rules start dimmed; each decision highlights the matching rule.
# Agentop follows the current session.
#
#   demo/record_tmux.sh [--pace N] [--hold N] [--turn N] [--only NN]... [--out PATH]
#
#   --pace N   seconds the caption shows before the agent acts (default 2)
#   --hold N   seconds a step's outcome stays up before the next (default 4)
#   --turn N   seconds between agent turns (default 3.5)
#              gives viewers time to read each decision
#   --only NN  record only the scenarios whose names start NN (repeatable)
#   --out PATH default .sketchpad/demo-video/coding-agent-cpex-tmux
#
# Writes PATH.cast (asciinema), PATH.gif (agg) and PATH.mp4 (ffmpeg).
# Needs `./run.sh up`, tmux, asciinema 3, agg, ffmpeg and bin/agentop
# (`make agentop`). Clears the demo Cortex's session list before recording.
# Agentop uses a HOME under .state/, separate from ~/.cortex and ~/.claude.
# tmux uses a separate socket.
#
# The walkthrough and the agent call this script as their WALKTHROUGH_HOOK:
#   demo/record_tmux.sh scenario <name>        caption and policy switch
#   demo/record_tmux.sh verdict <kind> <code> <label>
#                                              the policy pane shows the verdict
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
COLS=160
ROWS=42

die() { echo "record_tmux.sh: $*" >&2; exit 1; }
tm() { tmux -L "$SOCK" "$@"; }

# --- the hook ----------------------------------------------------------------

# Write the step on line 1 and decision on line 2. Rename the completed file
# so readers never see a partial update.
case "${1:-}" in
  scenario) echo "$2" >"$REC/slide.tmp" && mv "$REC/slide.tmp" "$REC/slide"; exit 0 ;;
  verdict)
    { head -1 "$REC/slide" 2>/dev/null; echo "${*:2}"; } >"$REC/slide.tmp" && mv "$REC/slide.tmp" "$REC/slide"
    exit 0 ;;
  session) echo "$3" >"$REC/session"; exit 0 ;;
esac

# --- agentop follows the session the walkthrough opened -------------------------

AGENTOP_PANE=""  # its pane id, once the panes exist

# Agentop's header shows the endpoint in list view or the title and ID in events view.
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

# show <id>: wait for the session, then open the newest row or search by ID.
# Keep the previous session visible until the new one is listed.
# Sessions open at the newest event and follow new events.
show() {
  local id="$1" _
  for _ in $(seq 1 50); do session_listed "$id" && break; sleep 0.1; done
  to_list
  # Clear any previous search so it cannot pin the selection.
  if tm capture-pane -p -t "$AGENTOP_PANE" | grep -qF '[/'; then
    tm send-keys -t "$AGENTOP_PANE" Escape; sleep 0.1
  fi
  tm send-keys -t "$AGENTOP_PANE" g Enter; sleep 0.4
  agentop_header | grep -qF "$id" && { quiet; return 0; }
  for _ in 1 2; do
    to_list
    tm send-keys -t "$AGENTOP_PANE" / && sleep 0.05
    tm send-keys -t "$AGENTOP_PANE" -l "$id" && sleep 0.05
    tm send-keys -t "$AGENTOP_PANE" Enter && sleep 0.1
    tm send-keys -t "$AGENTOP_PANE" Enter && sleep 0.3
    agentop_header | grep -qF "$id" && { quiet; return 0; }
  done
  return 0
}

# quiet: press agentop's `s` once to hide exchanges with no plugin action.
# The setting persists across sessions.
quiet() {
  [ -f "$REC/quiet" ] && return 0
  tm send-keys -t "$AGENTOP_PANE" s
  touch "$REC/quiet"
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

PACE=2
HOLD=4
TURN=3.5
ONLY=()
OUT="$REPO/.sketchpad/demo-video/coding-agent-cpex-tmux"
while [ $# -gt 0 ]; do
  case "$1" in
    --pace) PACE="${2:?--pace needs seconds}"; shift ;;
    --hold) HOLD="${2:?--hold needs seconds}"; shift ;;
    --turn) TURN="${2:?--turn needs seconds}"; shift ;;
    --only) ONLY+=(--only "${2:?--only needs a scenario prefix}"); shift ;;
    --out) OUT="${2:?--out needs a path}"; shift ;;
    -h|--help) sed -n '2,29p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
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
rm -f "$REC/slide" "$REC/session" "$REC/go" "$REC/quiet"
# Use a compact agentop table for the recording.
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
    - {name: PLUGIN, visible: false}
YAML
# Start from an empty session list, so the video shows only its own sessions.
curl -fsS -X DELETE --max-time 15 "$SESSION_API/v1/sessions" >/dev/null || die "could not clear the session list"

q() { printf '%q' "$1"; }
only=""
for o in ${ONLY[@]+"${ONLY[@]}"}; do only+=" $(q "$o")"; done
# The title card shows for 3s before the walkthrough starts; the closing
# card is the last frame, held by agg. The cursor is hidden in every pane.
walk="cd $(q "$DEMO_DIR") && while [ ! -f $(q "$REC/go") ]; do sleep 0.2; done; clear; printf %s $(q $'\033[?25l'); \
AGENT_TURN_DELAY=$(q "$TURN") AGENT_OPEN_DELAY=$(q "$TURN") WALKTHROUGH_HOOK=$(q "$HERE/record_tmux.sh") \
./walkthrough.sh --presenter --pace $(q "$PACE") --hold $(q "$HOLD")$only; \
sleep 1; tmux -L $SOCK kill-session -t $NAME"
agentop="HOME=$(q "$REC/home") CLAUDE_CONFIG_DIR=$(q "$REC/home/.claude") $(q "$AGENTOP") observe \
--skip-claude-metadata --endpoint $(q "$SESSION_API") --prefs $(q "$REC/agentop.yaml")"
slide="python3 $(q "$HERE/policy_slide.py") --watch $(q "$REC/slide")"
caption="python3 $(q "$HERE/caption_bar.py") --watch $(q "$REC/slide")"

# Layout: captions above an 84-column walkthrough and a right column with
# policy rules above agentop. Scenario 04 needs the policy pane's full 25 rows.
tm -f /dev/null new-session -d -s "$NAME" -x "$COLS" -y "$ROWS" "bash -c $(q "$walk")"
tm set -g status off
tm set -g pane-border-status top
tm set -g pane-border-format '#{?pane_title, #{pane_title} ,}'
tm set -g pane-active-border-style fg=colour240
tm set -g pane-border-style fg=colour240
WALK_PANE="$(tm display -p -t "$NAME:0.0" '#{pane_id}')"
CAPTION_PANE="$(tm split-window -P -F '#{pane_id}' -v -b -f -l 4 -t "$WALK_PANE" "$caption")"
SLIDE_PANE="$(tm split-window -P -F '#{pane_id}' -h -l 75 -t "$WALK_PANE" "$slide")"
AGENTOP_PANE="$(tm split-window -P -F '#{pane_id}' -v -l 11 -t "$SLIDE_PANE" "$agentop")"
tm select-pane -t "$CAPTION_PANE" -T "" -P 'bg=colour24'
tm select-pane -t "$WALK_PANE" -T "agent"
tm select-pane -t "$SLIDE_PANE" -T "CPEX policy"
tm select-pane -t "$AGENTOP_PANE" -T "agentop"
tm select-pane -t "$WALK_PANE"

follow &
FOLLOW=$!
trap 'kill "$FOLLOW" 2>/dev/null || true; tm kill-session -t "$NAME" 2>/dev/null || true' EXIT
(sleep 3; touch "$REC/go") &

asciinema rec --overwrite --headless --window-size "${COLS}x${ROWS}" --idle-time-limit 6 \
  -c "tmux -L $SOCK attach -t $NAME" "$OUT.cast"
# Remove tmux's exit screen so the video ends on the closing card.
python3 - "$OUT.cast" <<'PY'
import json, sys
path = sys.argv[1]
lines = open(path).read().splitlines()
head, events = lines[0], [json.loads(x) for x in lines[1:]]
cut = next((i for i, e in enumerate(events) if e[1] == "o" and "[exited]" in e[2]), len(events))
clear = [i for i, e in enumerate(events[:cut]) if e[1] == "o" and "\x1b[H\x1b[2J" in e[2]]
if clear and cut < len(events):
    cut = clear[-1]
with open(path, "w") as f:
    f.write("\n".join([head] + [json.dumps(e) for e in events[:cut]]) + "\n")
PY
echo "cast: $OUT.cast"

# Cap the frame rate to reduce flicker from pane redraws.
agg -q --font-size 20 --fps-cap 12 --idle-time-limit 6 --last-frame-duration 6 "$OUT.cast" "$OUT.gif"
echo "gif:  $OUT.gif"
ffmpeg -y -loglevel error -i "$OUT.gif" -movflags faststart -pix_fmt yuv420p -r 12 \
  -vf "scale=trunc(iw/2)*2:trunc(ih/2)*2" "$OUT.mp4"
echo "mp4:  $OUT.mp4"
