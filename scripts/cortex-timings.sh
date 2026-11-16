#!/usr/bin/env bash
# Tool and inference timings out of the Cortex session API.
#
# Read-only: every call is a GET against the session API, which on a local install
# binds 127.0.0.1:47601. Nothing is written and no session is modified.
#
#   cortex-timings.sh sessions                 # sessions by event count
#   cortex-timings.sh tools    [session]       # per-tool latency, one session
#   cortex-timings.sh tools-all                # per-tool latency, every session
#   cortex-timings.sh slow     <tool> [session]# slowest calls of one tool, with previews
#   cortex-timings.sh models   [session]       # inference duration and TTFB by model
#   cortex-timings.sh gaps     [session]       # where the wall-clock went
#   cortex-timings.sh turns    [session]       # model calls: duration, errors, retries
#   cortex-timings.sh bash     [session]       # FULL Bash commands + duration, no prompts
#
# With no [session], the commands that take one use the session with the most events.
#
# Environment knobs: CORTEX_API (API base, default http://127.0.0.1:47601),
# CORTEX_TIMEOUT (curl --max-time seconds, default 90) and CORTEX_GAP_CAP
# (gap ceiling in seconds for `gaps`, default 1800).
#
# WHY /timeline AND NOT RAW EVENTS. The timeline endpoint already pairs a tool call
# with the request carrying its result, which is the hard part: a tool runs from the
# response that asked for it to the agent's next request carrying its toolUseId. It
# also handles the cases that make a naive pairing wrong — a result recorded before
# its own start (clamped), a CONNECT failure that ends a tool, and a still-running
# tool with no end at all.
#
# A TOOL WITH NO `end` IS STILL RUNNING, not a zero-duration call, so every
# aggregation below drops those rather than counting them as 0. The same is true
# of a span the timeline marks `lost`: the agent moved on without a result, so the
# timeline closes it at its own start (`end == start`). Those are dropped from the
# durations too and counted in a LOST column instead — an abandoned tool is a
# signal, not a 0 ms sample.
set -euo pipefail

API="${CORTEX_API:-http://127.0.0.1:47601}"
CURL=(curl -sS --max-time "${CORTEX_TIMEOUT:-90}")

die() { printf '%s\n' "$*" >&2; exit 1; }

command -v jq >/dev/null || die "jq is required (brew install jq / apt install jq)"
command -v column >/dev/null || die "column is required (util-linux / bsdextrautils)"

# RFC 3339 with fractional seconds AND a numeric offset is what the API emits
# ("2026-10-21T20:50:36.459496-04:00"). jq's fromdateiso8601 accepts neither, so
# parse the parts: seconds from the base, fraction added back as a float. Dropping
# the fraction instead costs the sub-second resolution that makes Edit (48ms) and
# Read (24ms) distinguishable from each other and from zero.
#
# The offset is honoured, not assumed UTC: the timeline may straddle a DST change
# ("2026-11-01T01:59:30-04:00" to "2026-11-01T01:00:30-05:00" is one real minute
# apart, not minus fifty-nine), so ts normalises to UTC before any subtraction.
read -r -d '' TS_DEF <<'JQ' || true
def ts: capture("^(?<b>[^.Z+]+)(\\.(?<f>[0-9]+))?(?<z>Z|[+-][0-9:]+)$")
        | (if .z == "Z" then 0
           else ((.z[0:1] == "-") | if . then -1 else 1 end)
                * (((.z[1:] | split(":") | map(tonumber)) as $p
                     | ($p[0] * 3600 + ($p[1] // 0) * 60))) end) as $off
        | ((.b + "Z" | fromdateiso8601) - $off
           + (if .f then (".\(.f)" | tonumber) else 0 end));
def pct(a; p): (a | length) as $n | if $n == 0 then null else a[(($n - 1) * p) | round] end;
JQ

# -f so an HTTP error is a failed pipeline, not an empty body that jq reads as
# success: a session whose timeline 500s must surface, not silently contribute
# zero rows. (preflight does not go through api() — it needs the raw status code.)
api() { "${CURL[@]}" -f "$API$1"; }

# The event endpoints cap one page at the API's `maxEventLimit` (2000) and return
# the TAIL of the session, so a single request silently covers only the newest
# slice — on a 2911-event session, `bash` and `models` would see a different
# population than `tools`/`turns`/`gaps` (the timeline reads to 50,000). Page
# backward with ?before= until oldestSeq, which the API sets only when events were
# omitted, so every command sees the same depth.
#
# Pages are small for the full view on purpose: a 2000-event full page can run to
# hundreds of MB on prompt-heavy sessions, and --max-time 90 does not forgive that.
fetch_events() { # <id> <view|-> <page-size> ; one event per line on stdout
  local id="$1" view="$2" size="$3" page before
  [[ "$view" == "-" ]] && view=""
  page=$(api "/v1/sessions/$id?limit=$size${view:+&view=$view}")
  printf '%s\n' "$page" | jq -c '.events[]'
  while before=$(printf '%s\n' "$page" | jq -r '.events[0].seq // empty') \
        && printf '%s\n' "$page" | jq -e '(.oldestSeq // 0) != 0 and .events[0].seq > .oldestSeq' >/dev/null; do
    page=$(api "/v1/sessions/$id?limit=$size${view:+&view=$view}&before=$before")
    printf '%s\n' "$page" | jq -c '.events[]'
  done
}

# The session API answers 403 to a Host it does not recognise on a loopback-only
# install, which looks like a crash if it is not named.
preflight() {
  local code
  code=$("${CURL[@]}" -o /dev/null -w '%{http_code}' "$API/healthz" 2>/dev/null) || code=000
  case "$code" in
    200) : ;;
    000) die "no session API at $API — is cortex running? (set CORTEX_API to override)" ;;
    403) die "$API returned 403: bind_loopback_only rejects this Host. Use 127.0.0.1, not a hostname." ;;
    *)   die "$API/healthz returned $code" ;;
  esac
}

# ?archived=true adds the sessions the archive holds but memory no longer does —
# which on a laptop is most of the history worth measuring.
sessions_json() { api '/v1/sessions?archived=true'; }

biggest_session() {
  sessions_json | jq -r '[.sessions[] | select(.id | startswith("pending:") | not)]
    | sort_by(-(.eventCount // 0)) | .[0].id // empty'
}

pick_session() {
  if [[ -n "${1:-}" ]]; then printf '%s' "$1"; return; fi
  local id; id=$(biggest_session)
  [[ -n "$id" ]] || die "no sessions found"
  printf '%s' "$id"
}

cmd_sessions() {
  sessions_json | jq -r '
    (["EVENTS","AGENT","TOKENS","COST_USD","SESSION"] | @tsv),
    ([.sessions[] | select((.eventCount // 0) > 0)]
     | map(select(.id | startswith("pending:") | not))
     | sort_by(-(.eventCount // 0)) | .[]
     | [ (.eventCount // 0), (.agent // "-"), (.totalTokens // 0),
         (((.costMicros // 0) / 1000000) * 100 | round / 100), .id ] | @tsv)
  ' | column -t -s $'\t'
}

# Per-tool latency. `n` counts only finished, not-lost calls, which is what the
# percentiles describe; `total_s` is the summed wall-clock those calls held. `lost`
# counts the calls the timeline closed at their own start (agent moved on without
# a result) — reported, not folded into the durations.
cmd_tools() {
  local id; id=$(pick_session "${1:-}")
  printf 'session %s\n\n' "$id" >&2
  api "/v1/sessions/$id/timeline" | jq -r "$TS_DEF"'
    [ .tools[] | select(.end != null)
      | {name, lost: (.lost // false), err: (.error // false),
         ms: (if (.lost // false) then null
              else ((((.end|ts) - (.start|ts)) * 1000) | round) end)} ]
    | group_by(.name)
    | map({name: .[0].name, n: (map(select(.lost | not)) | length),
           lost: (map(select(.lost)) | length),
           ms: (map(select(.lost | not) | .ms) | sort),
           errs: (map(select(.err)) | length)})
    | map({name, n, lost, errs,
           p50: (pct(.ms; 0.5) // "-"), p95: (pct(.ms; 0.95) // "-"),
           max: (.ms | max // "-"), total_s: ((.ms | add // 0) / 1000 | round)})
    | sort_by(-.total_s)
    | (["TOOL","N","LOST","ERRS","P50ms","P95ms","MAXms","TOTALs"] | @tsv),
      (.[] | [.name, .n, .lost, .errs, .p50, .p95, .max, .total_s] | @tsv)
  ' | column -t -s $'\t'
}

# Every session, folded into one table. One timeline request per session: a session
# only on disk takes ~1s, so this is minutes over a long history, not seconds.
#
# A session whose timeline fails to load (timeout on a cold archive, a 5xx) is named
# on stderr and counted, never silently skipped: totals are what this command is for,
# and they are only meaningful over a known population.
cmd_tools_all() {
  local ids id n=0 fail=0
  ids=$(sessions_json | jq -r '[.sessions[] | select((.eventCount // 0) > 50)]
        | map(select(.id | startswith("pending:") | not))
        | sort_by(-(.eventCount // 0)) | .[].id')
  [[ -n "$ids" ]] || die "no sessions with more than 50 events"
  local tmp; tmp=$(mktemp)
  trap 'rm -f "$tmp"' RETURN
  while IFS= read -r id; do
    n=$((n + 1))
    printf '\r  %d sessions…' "$n" >&2
    if ! api "/v1/sessions/$id/timeline" 2>/dev/null \
         | jq -c '.tools // [] | .[] | select(.end != null)' >>"$tmp" 2>/dev/null; then
      fail=$((fail + 1))
      printf '\n  failed to read %s\n' "$id" >&2
    fi
  done <<<"$ids"
  printf '\r  %d sessions read, %d failed\n\n' "$n" "$fail" >&2
  jq -rs "$TS_DEF"'
    [ .[] | {name, lost: (.lost // false), err: (.error // false),
              ms: (if (.lost // false) then null
                   else ((((.end|ts) - (.start|ts)) * 1000) | round) end)} ]
    | group_by(.name)
    | map({name: .[0].name, n: (map(select(.lost | not)) | length),
           lost: (map(select(.lost)) | length),
           ms: (map(select(.lost | not) | .ms) | sort),
           errs: (map(select(.err)) | length)})
    | map({name, n, lost, errs, p50: (pct(.ms; 0.5) // "-"), p95: (pct(.ms; 0.95) // "-"),
           total_s: ((.ms | add // 0) / 1000 | round)})
    | sort_by(-.total_s)
    | (["TOOL","N","LOST","ERRS","P50ms","P95ms","TOTALs"] | @tsv),
      (.[] | [.name, .n, .lost, .errs, .p50, .p95, .total_s] | @tsv)
  ' "$tmp" | column -t -s $'\t'
}

# The slowest calls of one tool, with the preview the API carries (up to 80 chars of
# the command, path or description) — which is how a slow aggregate becomes a cause.
cmd_slow() {
  local tool="${1:?usage: slow <tool> [session]}" id
  id=$(pick_session "${2:-}")
  printf 'session %s · tool %s\n\n' "$id" "$tool" >&2
  api "/v1/sessions/$id/timeline" | jq -r --arg t "$tool" "$TS_DEF"'
    [ .tools[] | select(.end != null and (.lost | not) and .name == $t)
      | {ms: ((((.end|ts) - (.start|ts)) * 1000) | round),
         agent, err: (.error // false), preview: (.preview // "")} ]
    | sort_by(-.ms) | .[0:20]
    | (["MS","AGENT","ERR","PREVIEW"] | @tsv),
      (.[] | [.ms, (.agent[0:12]), (if .err then "yes" else "no" end), (.preview[0:64])] | @tsv)
  ' | column -t -s $'\t'
}

# Inference latency by model. view=summary is safe here and ~163x smaller: it drops
# message bodies but keeps durationMs and ttfbMs, which is all this reads.
#
# TTFB is counted only where non-zero: zero means the exchange never reached response
# headers, so averaging it in would report a timeout as an instant answer.
cmd_models() {
  local id; id=$(pick_session "${1:-}")
  printf 'session %s\n\n' "$id" >&2
  fetch_events "$id" summary 2000 | jq -sr "$TS_DEF"'
    [ .[] | select(.phase == "response" and .inference != null and (.durationMs // 0) > 0)
      | {model: (.inference.model // "-"), d: .durationMs, t: (.ttfbMs // 0),
         err: (.error != null)} ]
    | group_by(.model)
    | map({model: .[0].model, n: length,
           d: (map(.d) | sort), t: (map(select(.t > 0) | .t) | sort),
           errs: (map(select(.err)) | length)})
    | map({model, n, errs,
           dur_p50: pct(.d; 0.5), dur_p95: pct(.d; 0.95),
           ttfb_p50: pct(.t; 0.5), ttfb_p95: pct(.t; 0.95)})
    | sort_by(-.n)
    | (["MODEL","N","ERRS","DUR_P50","DUR_P95","TTFB_P50","TTFB_P95"] | @tsv),
      (.[] | [.model, .n, .errs, .dur_p50, .dur_p95, (.ttfb_p50 // "-"), (.ttfb_p95 // "-")] | @tsv)
  ' | column -t -s $'\t'
}

# Model calls as the timeline sees them: one row per turn, with the flags the API
# already derives — `error` for a call that failed, `resent` for a retry that sent
# the same conversation again.
cmd_turns() {
  local id; id=$(pick_session "${1:-}")
  printf 'session %s\n\n' "$id" >&2
  api "/v1/sessions/$id/timeline" | jq -r "$TS_DEF"'
    [ .turns[] | select(.end != null and (.lost | not))
      | {ms: ((((.end|ts) - (.start|ts)) * 1000) | round),
         model: (.model // "-"), finish: (.finish // "-"),
         err: (.error // false), resent: (.resent // false)} ] as $t
    | ($t | map(.ms) | sort) as $all
    | "turns          \($t | length)",
      "p50            \(pct($all; 0.5))ms",
      "p95            \(pct($all; 0.95))ms",
      "max            \($all | max)ms",
      "total          \(($all | add) / 1000 | round)s",
      "errors         \($t | map(select(.err)) | length)",
      "retries        \($t | map(select(.resent)) | length)",
      "",
      "by finish reason:",
      ($t | group_by(.finish) | map({f: .[0].finish, n: length})
          | sort_by(-.n) | .[] | "  \(.f)  \(.n)")
  '
}

# WHERE THE WALL-CLOCK WENT. Classify each gap between one turn ending and the next
# starting by whether a tool ran inside it: a tool gap is the agent working, a gap
# with no tool is a human deciding.
#
# THE CEILING IS PUBLISHED WITH THE FIGURE, because the total is very sensitive to
# it — a session left open overnight otherwise reports the night as "user wait".
# Override with CORTEX_GAP_CAP (seconds).
cmd_gaps() {
  local id cap="${CORTEX_GAP_CAP:-1800}"
  [[ "$cap" =~ ^[0-9]+$ ]] || die "CORTEX_GAP_CAP must be seconds (integer), got: $cap"
  id=$(pick_session "${1:-}")
  printf 'session %s · gap ceiling %ss\n\n' "$id" "$cap" >&2
  local tl; tl=$(api "/v1/sessions/$id/timeline")
  # Gaps are between CONCURRENT turns merged first: a background subagent's turn
  # can start and end inside a long main turn, and without the merge the "gap"
  # that follows it lies inside a model call. The turn count and the gap count are
  # printed from the same merge, so they agree.
  printf '%s\n' "$tl" | jq -r "$TS_DEF"'
    ([ .turns[] | select(.end != null) | {s: (.start|ts), e: (.end|ts)} ] | sort_by(.s)
     | (reduce .[] as $t ({m: null, g: []};
         if .m != null and $t.s > .m then .g += [{from: .m, to: $t.s}] else . end
         | .m = ([.m, $t.e] | max)))) as $merged
    | "gaps: \([.turns[] | select(.end != null)] | length) turns → \($merged.g | length) gaps"
  ' >&2
  printf '%s\n' "$tl" | jq -r --argjson cap "$cap" "$TS_DEF"'
    ([ .turns[] | select(.end != null) | {s: (.start|ts), e: (.end|ts)} ] | sort_by(.s)) as $turns
    | (reduce $turns[] as $t ({m: null, g: []};
        if .m != null and $t.s > .m then .g += [{from: .m, to: $t.s}] else . end
        | .m = ([.m, $t.e] | max)) | .g) as $gaps
    | ([ .tools[] | {s: (.start|ts), e: (if .end then (.end|ts) else (.start|ts) end)} ]) as $tools
    | ([ $gaps[] | . + {gap: (.to - .from)} ]) as $gaps
    | [ $gaps[] | . as $g
        | ($tools | map(select(.s < $g.to and .e > $g.from)) | length) as $inside
        | {gap: (if .gap > $cap then $cap else .gap end), capped: (.gap > $cap), tools: $inside} ] as $c
    | ($c | map(select(.tools > 0))) as $work
    | ($c | map(select(.tools == 0))) as $wait
    | (["KIND", "N", "P50ms", "TOTAL_H", "SHARE"] | @tsv),
      ( ($c | map(.gap) | add) as $tot
        | ($work | map(.gap) | add // 0) as $ws
        | ($wait | map(.gap) | add // 0) as $us
        | [ "tool time", ($work | length),
            (((pct(($work | map(.gap) | sort); 0.5) // 0) * 1000) | round),
            (($ws / 3600 * 10) | round / 10),
            "\(if $tot > 0 then (($ws / $tot * 100) | round) else 0 end)%" ] | @tsv ),
      ( ($c | map(.gap) | add) as $tot
        | ($wait | map(.gap) | add // 0) as $us
        | [ "user wait", ($wait | length),
            (((pct(($wait | map(.gap) | sort); 0.5) // 0) * 1000) | round),
            (($us / 3600 * 10) | round / 10),
            "\(if $tot > 0 then (($us / $tot * 100) | round) else 0 end)%" ] | @tsv ),
      ""
  ' | column -t -s $'\t'
  cat >&2 <<'NOTE'

A gap with no tool span is a human deciding; one with a tool is the agent working.
Both are measured BETWEEN turns, so neither includes the model call itself — see
`turns` for that. TOTAL_H is sensitive to the ceiling: raise CORTEX_GAP_CAP and the
user-wait total grows without bound, so publish the ceiling with the figure.
NOTE
}

# FULL Bash commands, joined to their measured duration.
#
# The timeline's `preview` is capped at 80 characters (previewMax), so a long command
# is truncated there. The full text is on the response event, in
# inference.toolCalls[].arguments — which needs the FULL event view, not view=summary.
#
# WHY BASH SPECIFICALLY, AND WHY THIS IS STILL PROMPT-FREE. This reads only the
# `command` key out of each Bash call's arguments. Other tools' arguments are not all
# safe to dump blind: WebFetch carries a `prompt`, and Task/Agent carries the
# subagent's instructions. So this names one key of one tool rather than printing
# whatever the arguments happen to hold.
#
# Durations come from the timeline, keyed by tool-use id, because only the timeline
# knows when the result came back. A command with no measured end — still running,
# or a span the timeline closed as `lost` — is reported as "-" rather than 0.
cmd_bash() {
  local id; id=$(pick_session "${1:-}")
  printf 'session %s\n\n' "$id" >&2
  local durs; durs=$(mktemp); trap 'rm -f "$durs"' RETURN
  api "/v1/sessions/$id/timeline" | jq -c "$TS_DEF"'
    [ .tools[] | select(.name == "Bash")
      | {id, ms: (if .end and (.lost | not) then ((((.end|ts) - (.start|ts)) * 1000) | round) else null end)} ]
    | INDEX(.id) | map_values(.ms)
  ' >"$durs"

  fetch_events "$id" - 500 | jq -sr --slurpfile d "$durs" '
    ($d[0] // {}) as $ms
    | [ .[] | select(.phase == "response" and .inference.toolCalls != null)
        | .seq as $seq
        | .inference.toolCalls[]
        | select(.name == "Bash")
        | (try (.arguments | fromjson) catch {}) as $a
        | select(($a.command // "") != "")
        | {seq: $seq, ms: ($ms[.id] // null), cmd: ($a.command | gsub("\\s+"; " "))} ]
    | sort_by(-(.ms // -1))
    | (["MS","SEQ","COMMAND"] | @tsv),
      (.[] | [(.ms // "-"), .seq, (.cmd[0:150])] | @tsv)
  ' | column -t -s $'\t'
}

case "${1:-}" in
  sessions)  preflight; cmd_sessions ;;
  bash)      preflight; cmd_bash "${2:-}" ;;
  tools)     preflight; cmd_tools "${2:-}" ;;
  tools-all) preflight; cmd_tools_all ;;
  slow)      preflight; cmd_slow "${2:-}" "${3:-}" ;;
  models)    preflight; cmd_models "${2:-}" ;;
  turns)     preflight; cmd_turns "${2:-}" ;;
  gaps)      preflight; cmd_gaps "${2:-}" ;;
  ""|-h|--help|help)
    awk 'NR == 1 { next } /^[^#]/ { exit } { sub(/^# ?/, ""); print }' "$0" ;;
  *) die "unknown command: $1 (try --help)" ;;
esac
