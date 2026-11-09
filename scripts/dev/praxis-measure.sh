#!/bin/bash
# praxis-measure.sh measures praxis mode against direct mode, for each process.
#
# Praxis mode: client -> cortex-gateway -> mock origin, and the gateway streams its
# rows to a scratch Cortex (praxis.enabled) on POST /v1/ingest.
# Direct mode: client -> scratch Cortex forward proxy -> mock origin.
#
# Both runs send the same traffic: N requests, 7 MiB and 12 MiB request bodies in
# turn, two X-Claude-Code-Agent-Id values in turn, one session. The origin answers
# each one with the largest claude-code fixture response.
#
# The script prints, for each process: the CPU time (ps -o time), the peak RSS
# (sampled every 0.2 s) and, for Cortex, the heap in use after a GC, from
# /debug/pprof/heap?gc=1.
# cortex-gateway serves no pprof.
#
# Every process runs under a temp HOME on its own ports. The script never touches
# the shared proxy on :47600 or ~/.cortex.
#
# Usage: scripts/dev/praxis-measure.sh [-n N] [-c CONCURRENCY]
#   -n N  the number of requests in each run (default 200)
#   -c C  the number of requests in flight at one time (default 4)
# Set CORTEX_BIN or GATEWAY_BIN to use a binary you built. Otherwise the script
# builds cortex (profile full) and cortex-gateway (release).
set -euo pipefail

n=200
conc=4
while [ $# -gt 0 ]; do
	case "$1" in
	-n) n="$2"; shift 2 ;;
	-c) conc="$2"; shift 2 ;;
	*) echo "praxis-measure: unknown argument $1" >&2; exit 2 ;;
	esac
done

root=$(cd "$(dirname "$0")/../.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/praxis-measure.XXXXXX")
pids=""
cleanup() {
	for p in $pids; do kill "$p" 2>/dev/null || true; done
	wait 2>/dev/null || true
	if [ -n "${KEEP:-}" ]; then echo "logs kept in $tmp" >&2; else rm -rf "$tmp"; fi
}
trap cleanup EXIT

if [ -z "${CORTEX_BIN:-}" ]; then
	tags=$(go -C "$root/scripts/profile-tags" run . full)
	echo "building cortex (profile full)" >&2
	(cd "$root/cmd/cortex" && GOWORK=off go build -tags "$tags" -o "$tmp/cortex" .)
	CORTEX_BIN="$tmp/cortex"
fi
if [ -z "${GATEWAY_BIN:-}" ]; then
	echo "building cortex-gateway (release)" >&2
	(cd "$root/praxis" && cargo build --release --locked -p cortex-gateway >&2)
	GATEWAY_BIN="$root/praxis/target/release/cortex-gateway"
fi

fixtures="$root/core/plugins/inferenceparser/testdata/claude-code"
# shellcheck disable=SC2012 # fixture names have no spaces
response=$(ls -S "$fixtures"/*/response.sse "$fixtures"/*/response.json | head -1)
fixture=$(dirname "$response")
echo "fixture: $(basename "$fixture") ($(wc -c <"$response" | tr -d ' ') response bytes)" >&2

free_port() {
	python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'
}

# wait_http URL CODE: waits up to 30 s for URL to answer CODE.
wait_http() {
	for _ in $(seq 300); do
		if [ "$(curl -s --noproxy '*' -o /dev/null -w '%{http_code}' "$1" || true)" = "$2" ]; then
			return 0
		fi
		sleep 0.1
	done
	echo "praxis-measure: $1 did not answer $2" >&2
	return 1
}

# sample_rss PID FILE: writes the peak RSS in KiB of PID to FILE until PID ends.
sample_rss() {
	peak=0
	while kill -0 "$1" 2>/dev/null; do
		r=$(ps -o rss= -p "$1" 2>/dev/null | tr -d ' ' || true)
		if [ -n "$r" ] && [ "$r" -gt "$peak" ]; then
			peak=$r
			echo "$peak" >"$2"
		fi
		sleep 0.2
	done
}

cat >"$tmp/origin.py" <<'EOF'
import http.server, sys
port, body_path, ctype = int(sys.argv[1]), sys.argv[2], sys.argv[3]
body = open(body_path, "rb").read()

class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *a): pass
    def do_POST(self):
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            while True:
                size = int(self.rfile.readline().split(b";")[0], 16)
                self.rfile.read(size + 2)
                if size == 0:
                    break
        else:
            left = int(self.headers.get("Content-Length", "0"))
            while left > 0:
                left -= len(self.rfile.read(min(left, 1 << 20)))
        self.send_response(200)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

http.server.ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
EOF

cat >"$tmp/client.py" <<'EOF'
import concurrent.futures, http.client, json, sys
mode, target, origin, n, conc, fixture, token = sys.argv[1:8]
n, conc = int(n), int(conc)
headers = {}
for line in open(fixture + "/request.headers").read().splitlines()[1:]:
    if ":" in line:
        k, v = line.split(":", 1)
        if k.strip().lower() not in ("host", "content-length"):
            headers[k.strip()] = v.strip()
base = json.load(open(fixture + "/request.json"))

def body(size):
    # A valid inference request: the fixture plus one user message that pads it to size.
    req = dict(base)
    req["messages"] = list(base.get("messages", [])) + [{"role": "user", "content": ""}]
    pad = size - len(json.dumps(req).encode())
    req["messages"][-1]["content"] = "x" * max(pad, 0)
    return json.dumps(req).encode()

bodies = [body(7 << 20), body(12 << 20)]
path = "/v1/messages?beta=true"

def one(i):
    h = dict(headers)
    h["X-Claude-Code-Agent-Id"] = "agent-%d" % (i % 2)
    host, port = target.split(":")
    c = http.client.HTTPConnection(host, int(port), timeout=120)
    if mode == "praxis":
        h["X-Cortex-Token"] = token
        url = path
    else:
        url = "http://" + origin + path
    c.request("POST", url, body=bodies[i % 2], headers=h)
    r = c.getresponse()
    r.read()
    c.close()
    return r.status

with concurrent.futures.ThreadPoolExecutor(conc) as ex:
    codes = list(ex.map(one, range(n)))
bad = [c for c in codes if c != 200]
if bad:
    sys.exit("%d of %d requests did not answer 200: %s" % (len(bad), n, sorted(set(bad))))
EOF

origin_port=$(free_port)
ctype="text/event-stream; charset=utf-8"
case "$response" in *.json) ctype="application/json" ;; esac
if [ -f "$fixture/response.headers" ]; then
	ct=$(grep -i '^content-type:' "$fixture/response.headers" | head -1 | cut -d: -f2- | sed 's/^ *//' || true)
	[ -n "$ct" ] && ctype=$ct
fi
python3 "$tmp/origin.py" "$origin_port" "$response" "$ctype" &
pids="$pids $!"

token="measure-$$"
echo "$token" >"$tmp/token"

results=""

# run MODE: one scratch Cortex (and in praxis mode one gateway), N requests, numbers.
run() {
	mode=$1
	dir="$tmp/$mode"
	mkdir -p "$dir/home"
	proxy=$(free_port)
	api=$(free_port)
	health=$(free_port)
	trans=$(free_port)
	stats=$(free_port)
	praxis_block=""
	[ "$mode" = praxis ] && praxis_block="praxis:
  enabled: true"
	cat >"$dir/config.yaml" <<EOF
mode: proxy-sidecar
listener:
  roles: [forward]
  bind_loopback_only: true
  forward_proxy_addr: 127.0.0.1:$proxy
  session_api_addr: 127.0.0.1:$api
  health_addr: 127.0.0.1:$health
  transparent_proxy_addr: 127.0.0.1:$trans
stats:
  address: 127.0.0.1:$stats
session:
  id_headers: ["X-Claude-Code-Session-Id"]
pipeline:
  outbound:
    plugins:
      - name: inference-parser
$praxis_block
EOF
	HOME="$dir/home" CORTEX_GATEWAY_TOKEN_FILE="$tmp/token" \
		"$CORTEX_BIN" --config "$dir/config.yaml" >"$dir/cortex.log" 2>&1 &
	cortex_pid=$!
	pids="$pids $cortex_pid"
	wait_http "http://127.0.0.1:$health/healthz" 200
	sample_rss "$cortex_pid" "$dir/cortex.rss" &

	target="127.0.0.1:$proxy"
	gateway_pid=""
	if [ "$mode" = praxis ]; then
		gw=$(free_port)
		gw_metrics=$(free_port)
		cat >"$dir/gateway.yaml" <<EOF
admin:
  metrics_address: "127.0.0.1:$gw_metrics"
insecure_options:
  allow_unbounded_body: true
  allow_private_upstreams: true
  allow_private_endpoints: true
body_limits:
  max_request_bytes: 33554432
  max_response_bytes: null
listeners:
  - name: gateway
    address: "127.0.0.1:$gw"
    filter_chains: [main]
filter_chains:
  - name: main
    filters:
      - filter: cortex_attribution
        token_file: "$tmp/token"
      - filter: cortex_observe
        token_file: "$tmp/token"
        ingest: "127.0.0.1:$api"
      - filter: router
        routes:
          - path_prefix: "/"
            cluster: origin
      - filter: load_balancer
        clusters:
          - name: origin
            endpoints: ["127.0.0.1:$origin_port"]
EOF
		HOME="$dir/home" "$GATEWAY_BIN" -c "$dir/gateway.yaml" >"$dir/gateway.log" 2>&1 &
		gateway_pid=$!
		pids="$pids $gateway_pid"
		# Only the gateway answers a request without the token with 403.
		wait_http "http://127.0.0.1:$gw/" 403
		sample_rss "$gateway_pid" "$dir/gateway.rss" &
		target="127.0.0.1:$gw"
	fi

	echo "$mode: $n requests, $conc in flight" >&2
	start=$(date +%s)
	python3 "$tmp/client.py" "$mode" "$target" "127.0.0.1:$origin_port" "$n" "$conc" "$fixture" "$token"
	secs=$(($(date +%s) - start))

	if [ "$mode" = praxis ]; then
		# Each request gives a request row and a response row. Wait until all of them
		# arrive, or until no line arrives for 10 s (the emitter dropped the rest).
		last=-1
		still=0
		while [ "$still" -lt 100 ]; do
			lines=$(curl -s --noproxy '*' "http://127.0.0.1:$api/v1/ingest/stats" |
				python3 -c 'import json,sys; print(json.load(sys.stdin).get("ingest_lines_total", 0))' || echo 0)
			[ "$lines" -ge $((2 * n)) ] && break
			if [ "$lines" = "$last" ]; then still=$((still + 1)); else still=0; last=$lines; fi
			sleep 0.1
		done
		echo "praxis: $(($(date +%s) - start)) s to ingest; stats: $(curl -s --noproxy '*' "http://127.0.0.1:$api/v1/ingest/stats")" >&2
		echo "praxis: cortex-gateway emitter counters:" >&2
		curl -s --noproxy '*' "http://127.0.0.1:$gw_metrics/metrics" | grep '^cortex_' >&2 || true
	fi
	sleep 0.5 # one more RSS sample after the last request

	heap=$(curl -s --noproxy '*' "http://127.0.0.1:$stats/debug/pprof/heap?gc=1&debug=1" |
		awk '/^# HeapInuse = / {printf "%.1f", $4 / 1048576}')
	row "$mode" cortex "$cortex_pid" "$dir/cortex.rss" "${heap:-n/a}" "$secs"
	if [ -n "$gateway_pid" ]; then
		row "$mode" cortex-gateway "$gateway_pid" "$dir/gateway.rss" n/a "$secs"
		kill "$gateway_pid"
	fi
	kill "$cortex_pid"
	wait "$cortex_pid" 2>/dev/null || true
}

# row MODE PROCESS PID RSSFILE HEAP SECS: adds one table row.
row() {
	cpu=$(ps -o time= -p "$3" | tr -d ' ')
	rss=$(awk '{printf "%.1f", $1 / 1024}' "$4" 2>/dev/null || echo n/a)
	results="$results$(printf '| %s | %s | %s | %s | %s | %s |' "$1" "$2" "$cpu" "$rss" "$5" "$6")
"
}

run praxis
run direct

echo
echo "$n requests, $conc in flight, 7 and 12 MiB request bodies in turn, fixture $(basename "$fixture"):"
echo
echo "| Mode | Process | CPU time | Peak RSS (MiB) | Heap in use (MiB) | Wall (s) |"
echo "|---|---|---|---|---|---|"
printf '%s' "$results"
