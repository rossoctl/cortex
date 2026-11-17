#!/usr/bin/env bash
# verify-contextguru-keepalive.sh — live end-to-end check of the context-guru
# keep-alive, run against the REAL cortex binary and a fake upstream LLM.
#
# What it proves: a session's second tracked Anthropic request, sent through
# cortex's forward proxy, is followed after keep_alive.idle_seconds by exactly
# one ping that the plugin sends straight to the upstream:
#   - POST /v1/messages, body = request 2 with "max_tokens":1 and "stream":false,
#   - the caller's own credential (Authorization: Bearer ...) and the
#     X-Context-Guru-LLM sentinel header,
#   - the ping is logged as "context-guru keep-alive ping",
#   - and it does NOT re-enter cortex: the session API still lists only the two
#     client requests for that session.
#
# Design choices:
#   - Plain HTTP to 127.0.0.1. The forward proxy forwards absolute-form plain-HTTP
#     requests and runs the outbound pipeline on them, so no TLS bridge and no CA
#     are needed. Its loopback special case (selfTraffic in
#     core/listener/forwardproxy/process.go) only hides an agent process talking
#     to its OWN listener, which a python3 server reached by curl is not.
#   - The "no re-entry" check reads the session API rather than the log: it
#     counts recorded request events with httpPath /v1/messages across EVERY
#     session the proxy holds, which is exact. Counting only the test session
#     would not do: the ping does not carry X-Session-Id (keepalive.PingHeaders
#     keeps an allowlist), so a ping that came back through the proxy would be
#     filed under some other session. The proxy records a sentinel-marked request
#     like any other (the plugins only skip it), so a re-entered ping would show
#     up as a third request event. Pointing cortex's own HTTP_PROXY at itself
#     would not strengthen the check: Go's ProxyFromEnvironment never proxies
#     127.0.0.1, so the env is simply cleared.
#   - The upstream reports cache_read_input_tokens, because the keeper only tracks
#     a request that read or wrote a cache entry, and min_prefix_tokens: 1 keeps
#     the 48000-token fake prefix above the gate. The engine skips a session's
#     first request, so request 1 is never pinged; request 2 is.
#   - Isolation from a live install: HOME is a temp dir (cortex cannot touch
#     ~/.cortex), every listener is pinned to a free 127.0.0.1 port in
#     48600-48699 (never 47600-47604), the cost ledger and session archive are
#     off, and the trap kills only the PIDs this script started.
#
# Usage: scripts/dev/verify-contextguru-keepalive.sh   (from anywhere in the repo)
# Needs: go, python3, curl on PATH.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/cg-keepalive.XXXXXX")"
FAKE_PID=""
CORTEX_PID=""
FAILED=0

cleanup() {
  local rc=$?
  for pid in "$CORTEX_PID" "$FAKE_PID"; do
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  rm -rf "$WORK"
  exit "$rc"
}
trap cleanup EXIT INT TERM

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; FAILED=1; }
die() {
  echo "FAIL: $*"
  if [[ -f "$WORK/cortex.log" ]]; then
    echo "--- cortex log (last 40 lines) ---"
    tail -n 40 "$WORK/cortex.log"
  fi
  exit 1
}

# Six free loopback ports in 48600-48699.
read -r FAKE_PORT FWD_PORT SESS_PORT HEALTH_PORT STATS_PORT TRANS_PORT < <(python3 - <<'EOF'
import socket
free = []
for p in range(48600, 48700):
    s = socket.socket()
    try:
        s.bind(("127.0.0.1", p))
        free.append(p)
    except OSError:
        pass
    finally:
        s.close()
    if len(free) == 6:
        break
if len(free) < 6:
    raise SystemExit("not enough free ports in 48600-48699")
print(*free)
EOF
)

# 1. Build cortex with the full profile plus context-guru.
echo "building cortex into $WORK ..."
TAGS="$(go -C "$REPO_ROOT/scripts/profile-tags" run . full),include_plugin_contextguru"
(cd "$REPO_ROOT/cmd/cortex" && GOWORK=off go build -tags "$TAGS" -o "$WORK/cortex" .) \
  || die "go build cmd/cortex"
pass "built cortex (tags: full + include_plugin_contextguru)"

# 2. Fake upstream: Anthropic-shaped answer, every request logged as one JSON line.
cat >"$WORK/fake_upstream.py" <<'EOF'
import json, sys
from http.server import ThreadingHTTPServer, BaseHTTPRequestHandler

port, log_path = int(sys.argv[1]), sys.argv[2]

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n).decode("utf-8", "replace")
        with open(log_path, "a") as f:
            f.write(json.dumps({"path": self.path, "headers": dict(self.headers.items()),
                                "body": body}) + "\n")
        resp = json.dumps({
            "id": "msg_fake", "type": "message", "role": "assistant",
            "model": "claude-sonnet-4-5",
            "content": [{"type": "text", "text": "ok"}],
            "stop_reason": "end_turn", "stop_sequence": None,
            "usage": {"input_tokens": 3, "cache_read_input_tokens": 48000,
                      "cache_creation_input_tokens": 0, "output_tokens": 5},
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(resp)))
        self.end_headers()
        self.wfile.write(resp)

    def log_message(self, *a):
        pass

ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
EOF
: >"$WORK/upstream.jsonl"
python3 "$WORK/fake_upstream.py" "$FAKE_PORT" "$WORK/upstream.jsonl" &
FAKE_PID=$!

# 3. Minimal cortex config on private ports.
cat >"$WORK/config.yaml" <<EOF
mode: proxy-sidecar
listener:
  roles: [forward]
  bind_loopback_only: true
  forward_proxy_addr: 127.0.0.1:$FWD_PORT
  session_api_addr: 127.0.0.1:$SESS_PORT
  health_addr: 127.0.0.1:$HEALTH_PORT
  transparent_proxy_addr: 127.0.0.1:$TRANS_PORT
stats:
  address: 127.0.0.1:$STATS_PORT
cost_ledger:
  enabled: false
session:
  archive:
    enabled: false
  id_headers:
    - "X-Session-Id"
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: context-guru
        config:
          engine:
            preset: "off"
          keep_alive:
            enabled: true
            idle_seconds: 3
            max_pings: 1
            min_prefix_tokens: 1
EOF

# 4. Start cortex with an isolated HOME and wait for health.
mkdir -p "$WORK/home"
(cd "$WORK" && HOME="$WORK/home" HTTPS_PROXY="" HTTP_PROXY="" https_proxy="" http_proxy="" \
  exec "$WORK/cortex" --config "$WORK/config.yaml") >"$WORK/cortex.log" 2>&1 &
CORTEX_PID=$!
# --noproxy '*' on every plain-http call below is deliberate: a lowercase
# http_proxy in the developer's own shell (unlike HTTP_PROXY, cleared above
# only for the cortex process) would otherwise route these health/readiness/
# session-API calls through the shared live Cortex instead of this script's
# own private one.
for _ in $(seq 1 100); do
  curl -sf --noproxy '*' "http://127.0.0.1:$HEALTH_PORT/healthz" >/dev/null 2>&1 && break
  kill -0 "$CORTEX_PID" 2>/dev/null || die "cortex exited during startup"
  sleep 0.1
done
curl -sf --noproxy '*' "http://127.0.0.1:$HEALTH_PORT/healthz" >/dev/null || die "cortex health endpoint never came up"
fake_ready=0
for _ in $(seq 1 50); do
  curl -sf --noproxy '*' -o /dev/null "http://127.0.0.1:$FAKE_PORT/" -X POST -d '' 2>/dev/null && { fake_ready=1; break; }
  sleep 0.1
done
[[ "$fake_ready" == "1" ]] || die "fake upstream on :$FAKE_PORT never became ready within 5s"
: >"$WORK/upstream.jsonl" # drop the readiness probe
pass "cortex healthy (forward :$FWD_PORT, session API :$SESS_PORT, fake upstream :$FAKE_PORT)"

# 5. Two requests through the forward proxy, same session.
SID="cg-keepalive-$$-$RANDOM"
BODY1='{"model":"claude-sonnet-4-5","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}'
BODY2='{"model":"claude-sonnet-4-5","max_tokens":1024,"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"ok"},{"role":"user","content":"and again"}]}'
for body in "$BODY1" "$BODY2"; do
  code=$(curl -s -o /dev/null -w '%{http_code}' --noproxy '' -x "http://127.0.0.1:$FWD_PORT" \
    -H "Content-Type: application/json" -H "anthropic-version: 2023-06-01" \
    -H "Authorization: Bearer fake-test-key" -H "X-Session-Id: $SID" \
    --data "$body" "http://127.0.0.1:$FAKE_PORT/v1/messages")
  [[ "$code" == "200" ]] || die "request through proxy answered $code"
done
n=$(wc -l <"$WORK/upstream.jsonl" | tr -d ' ')
[[ "$n" == "2" ]] || die "upstream saw $n requests after the two client requests, want 2"
pass "two client requests forwarded (session $SID)"

# 6. Wait for the ping.
for _ in $(seq 1 150); do
  n=$(wc -l <"$WORK/upstream.jsonl" | tr -d ' ')
  [[ "$n" -ge 3 ]] && break
  sleep 0.1
done
[[ "$n" -ge 3 ]] || die "no ping reached the upstream within 15s"
pass "a third request (the ping) reached the upstream"

python3 - "$WORK/upstream.jsonl" <<'EOF' || FAILED=1
import json, sys
rows = [json.loads(l) for l in open(sys.argv[1])]
req2, ping = rows[1], rows[2]
h = {k.lower(): v for k, v in ping["headers"].items()}
b, b2 = json.loads(ping["body"]), json.loads(req2["body"])
ok = True
def check(cond, what):
    global ok
    print(("PASS: " if cond else "FAIL: ") + what)
    ok = ok and cond
check(ping["path"] == "/v1/messages", "ping path is /v1/messages (got %s)" % ping["path"])
check(b.get("max_tokens") == 1, "ping body has max_tokens 1 (got %r)" % b.get("max_tokens"))
check(b.get("stream") is False, "ping body has stream false (got %r)" % b.get("stream"))
check(b.get("messages") == b2.get("messages"), "ping messages equal request 2's messages")
check(h.get("authorization") == "Bearer fake-test-key", "ping carries the caller's Authorization")
check("x-context-guru-llm" in h, "ping carries X-Context-Guru-LLM")
check(len(rows) == 3, "exactly one ping (max_pings 1); upstream saw %d requests" % len(rows))
redacted = {k: (v[:6] + "...[redacted]" if k.lower() == "authorization" else v)
            for k, v in ping["headers"].items()}
print("ping headers: " + json.dumps(redacted, sort_keys=True))
print("ping body:    " + ping["body"])
sys.exit(0 if ok else 1)
EOF

if grep -q 'context-guru keep-alive ping' "$WORK/cortex.log"; then
  pass "cortex logged 'context-guru keep-alive ping'"
  grep 'context-guru keep-alive ping' "$WORK/cortex.log" | sed 's/^/  log: /'
else
  fail "cortex log has no 'context-guru keep-alive ping' line"
fi

# 7. The ping must not have come back through cortex as a recorded request.
sleep 1
curl -sf --noproxy '*' "http://127.0.0.1:$SESS_PORT/v1/sessions/$SID" >/dev/null \
  || die "session API has no session $SID"
counts=$(python3 - "$SESS_PORT" "$SID" <<'PY'
import json, sys, urllib.parse, urllib.request
port, sid = sys.argv[1], sys.argv[2]
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
base = "http://127.0.0.1:%s/v1/sessions" % port
sessions = json.load(opener.open(base)).get("sessions", [])
total = mine = 0
for s in sessions:
    ev = json.load(opener.open(base + "/" + urllib.parse.quote(s["id"], safe=""))).get("events", [])
    n = sum(1 for e in ev if e.get("phase") == "request" and e.get("httpPath") == "/v1/messages")
    total += n
    if s["id"] == sid:
        mine = n
print(len(sessions), total, mine)
PY
) || die "reading the session API failed"
read -r nsess total mine <<<"$counts"
if [[ "$total" == "2" && "$mine" == "2" ]]; then
  pass "session API records 2 /v1/messages requests across $nsess session(s), both in $SID (the ping bypassed the proxy)"
else
  fail "session API records $total /v1/messages requests across $nsess session(s), $mine in $SID; want 2 and 2"
fi

if [[ "$FAILED" -ne 0 ]]; then
  echo "--- cortex log (last 40 lines) ---"
  tail -n 40 "$WORK/cortex.log"
  echo "RESULT: FAIL"
  exit 1
fi
echo "RESULT: PASS"
