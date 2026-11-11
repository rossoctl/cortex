#!/bin/bash
# praxis-live-check.sh runs two scratch Cortex instances side by side, one in direct
# mode and one in praxis mode, for the praxis-mode live check (#1725).
#
# Every process runs under <scratch>/<mode>/home on the 497xx ports below. The script
# never touches the shared proxy on :47600-47606 or the real ~/.cortex.
#
#   direct: proxy 49700, session API 49701, stats 49702, transparent 49703, health 49704
#   praxis: proxy 49710, session API 49711, stats 49712, transparent 49713, health 49714,
#           praxis.listen 49715, praxis.egress 49716
#
# Usage:
#   praxis-live-check.sh build <scratch>               build cortex and cortex-gateway
#   praxis-live-check.sh up direct|praxis <scratch>    start one instance
#   praxis-live-check.sh env direct|praxis <scratch>   print exports for an agent shell
#   praxis-live-check.sh probe <scratch> <n> <url>     n interleaved latency pairs
#   praxis-live-check.sh sample <scratch> <seconds>    sample RSS, CPU and heap
#   praxis-live-check.sh ttff <scratch> <n> <url>      n pairs, time to the first frame
#   praxis-live-check.sh down direct|praxis|all <scratch>
#   praxis-live-check.sh selftest
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)

die() {
	echo "praxis-live-check: $*" >&2
	exit 1
}

# pct P: reads numbers on stdin and prints the nearest-rank percentile ceil(P/100*n).
# P is an integer.
pct() {
	sort -n | awk -v p="$1" '{a[NR] = $1} END {
		if (NR == 0) exit 1
		r = int((p * NR + 99) / 100)
		if (r < 1) r = 1
		print a[r]
	}'
}

# cputime_s T: converts a ps time (MM:SS.ss, HH:MM:SS or D-HH:MM:SS) to seconds.
cputime_s() {
	echo "$1" | awk '{
		d = 0; t = $0
		if (index(t, "-") > 0) { split(t, dp, "-"); d = dp[1]; t = dp[2] }
		n = split(t, f, ":")
		s = d * 86400 + f[1]
		if (n == 3) s = d * 86400 + f[1] * 3600 + f[2] * 60 + f[3]
		if (n == 2) s = d * 86400 + f[1] * 60 + f[2]
		if (index(f[n], ".") > 0) printf "%.2f\n", s; else printf "%d\n", s
	}'
}

# ports_ok FILE: fails when FILE names a port of the shared proxy (476xx).
ports_ok() {
	! grep -qE ':476[0-9][0-9]' "$1"
}

# pair_ok CODE CODE: succeeds when a probe pair is kept: both modes got the same code,
# and it is not 000. A praxis 503 from the hop is not the answer direct got.
pair_ok() {
	[ "$1" = "$2" ] && [ "$1" != 000 ]
}

# is_up MODE HEALTH_CODE LOG: succeeds when an instance is ready for traffic. In praxis
# mode /healthz does not wait for the hop, so also wait for the hop's ready line.
is_up() {
	[ "$2" = 200 ] || return 1
	[ "$1" != praxis ] || grep -q 'praxis hop: cortex-gateway is ready' "$3"
}

# port_busy PORT: succeeds when something accepts a TCP connection on 127.0.0.1:PORT.
port_busy() {
	(exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

# sample_check FILE: fails when FILE has no rows, or has praxis cortex rows but no
# cortex-gateway rows. A pgrep pattern that matches nothing must not look like a pass.
sample_check() {
	[ -s "$1" ] || { echo "no process was sampled" >&2; return 1; }
	! awk '$2 == "praxis" && $3 == "cortex" {c = 1} $3 == "cortex-gateway" {g = 1} END {exit !(c && !g)}' "$1" ||
		{ echo "praxis cortex was sampled, but no cortex-gateway" >&2; return 1; }
}

selftest() {
	fail=0
	check() {
		if [ "$2" != "$3" ]; then
			echo "selftest: $1: got '$2', want '$3'" >&2
			fail=1
		fi
	}
	check "pct 50 of 4" "$(printf '1\n2\n3\n4\n' | pct 50)" 2
	check "pct 95 of 4" "$(printf '1\n2\n3\n4\n' | pct 95)" 4
	check "pct 50 of 1" "$(printf '7\n' | pct 50)" 7
	check "pct 95 of 20" "$(seq 20 | pct 95)" 19
	check "pct 50 unsorted" "$(printf '9\n1\n5\n' | pct 50)" 5
	check "cputime 0:01.50" "$(cputime_s 0:01.50)" 1.50
	check "cputime 01:02:03" "$(cputime_s 01:02:03)" 3723
	check "cputime 1-00:00:01" "$(cputime_s 1-00:00:01)" 86401
	t=$(mktemp -d "${TMPDIR:-/tmp}/praxis-live-check-selftest.XXXXXX")
	echo "  forward_proxy_addr: 127.0.0.1:47600" >"$t/bad.yaml"
	echo "  forward_proxy_addr: 127.0.0.1:49700" >"$t/good.yaml"
	if ports_ok "$t/bad.yaml"; then echo "selftest: ports_ok passed :47600" >&2; fail=1; fi
	if ! ports_ok "$t/good.yaml"; then echo "selftest: ports_ok refused :49700" >&2; fail=1; fi
	# Two pids under one key (a restart): CPU is (3-1) + (2-0.5), not 2-1.
	printf '1\tpraxis\tcortex-gateway\t10\t100\t1\t-\n1\tpraxis\tcortex-gateway\t10\t300\t3\t-\n2\tpraxis\tcortex-gateway\t11\t200\t0.5\t-\n2\tpraxis\tcortex-gateway\t11\t200\t2\t-\n' >"$t/sample.tsv"
	check "sample_summary restart" "$(s=$t sample_summary)" "praxis cortex-gateway peak_rss_kb 300 peak_heap_inuse_bytes - cpu_s 3.50"
	if ! pair_ok 401 401; then echo "selftest: pair_ok refused 401 401" >&2; fail=1; fi
	if pair_ok 401 000; then echo "selftest: pair_ok kept 401 000" >&2; fail=1; fi
	# A local 503 from the praxis hop is not the upstream answer direct got.
	if pair_ok 401 503; then echo "selftest: pair_ok kept 401 503" >&2; fail=1; fi
	if pair_ok 503 401; then echo "selftest: pair_ok kept 503 401" >&2; fail=1; fi
	: >"$t/cortex.log"
	if ! is_up direct 200 "$t/cortex.log"; then echo "selftest: is_up refused direct 200" >&2; fail=1; fi
	if is_up direct 000 "$t/cortex.log"; then echo "selftest: is_up passed direct 000" >&2; fail=1; fi
	# /healthz does not wait for the praxis hop; the hop logs once when its probe passes.
	if is_up praxis 200 "$t/cortex.log"; then echo "selftest: is_up passed praxis before the hop is ready" >&2; fail=1; fi
	echo 'level=INFO msg="praxis hop: cortex-gateway is ready" gateway=127.0.0.1:49715' >"$t/cortex.log"
	if ! is_up praxis 200 "$t/cortex.log"; then echo "selftest: is_up refused praxis with the hop ready" >&2; fail=1; fi
	: >"$t/empty.tsv"
	if sample_check "$t/empty.tsv" 2>/dev/null; then echo "selftest: sample_check passed no rows" >&2; fail=1; fi
	printf '1\tpraxis\tcortex\t10\t100\t1\t5\n' >"$t/nogw.tsv"
	if sample_check "$t/nogw.tsv" 2>/dev/null; then echo "selftest: sample_check passed praxis with no gateway" >&2; fail=1; fi
	printf '1\tdirect\tcortex\t9\t100\t1\t5\n' >"$t/direct.tsv"
	if ! sample_check "$t/direct.tsv"; then echo "selftest: sample_check refused direct only" >&2; fail=1; fi
	if ! sample_check "$t/sample.tsv"; then echo "selftest: sample_check refused praxis with a gateway" >&2; fail=1; fi
	rm -rf "$t"
	[ "$fail" = 0 ] || exit 1
	echo "selftest: ok"
}

# abs DIR: the physical path, so pgrep patterns match what os.Executable gives.
abs() {
	[ -d "$1" ] || die "no directory $1"
	(cd "$1" && pwd -P)
}

# setports MODE: sets the port variables of one instance.
setports() {
	case "$1" in
	direct) base=49700 ;;
	praxis) base=49710 ;;
	*) die "mode must be direct or praxis, not '$1'" ;;
	esac
	proxy=$base
	api=$((base + 1))
	stats=$((base + 2))
	trans=$((base + 3))
	health=$((base + 4))
}

build() {
	s=$(abs "$1")
	mkdir -p "$s/bin"
	tags=$(go -C "$root/scripts/profile-tags" run . full)
	echo "building cortex (profile full)" >&2
	(cd "$root/cmd/cortex" && GOWORK=off go build -tags "$tags" -o "$s/bin/cortex" .)
	echo "building cortex-gateway (release)" >&2
	(cd "$root/praxis" && cargo build --release --locked -p cortex-gateway >&2)
	cp "$root/praxis/target/release/cortex-gateway" "$s/bin/"
	echo "built $s/bin/cortex $s/bin/cortex-gateway"
}

up() {
	mode=$1
	setports "$mode"
	s=$(abs "$2")
	[ -x "$s/bin/cortex" ] || die "no $s/bin/cortex; run build first"
	d="$s/$mode"
	home="$d/home"
	cfg="$home/.cortex/config.yaml"
	if [ -f "$d/supervisor.pid" ] && kill -0 "$(cat "$d/supervisor.pid")" 2>/dev/null; then
		die "$mode is already up (pid $(cat "$d/supervisor.pid"))"
	fi
	mkdir -p "$home/.cortex"
	praxis_block=""
	if [ "$mode" = praxis ]; then
		praxis_block="praxis:
  enabled: true
  listen: 127.0.0.1:$((base + 5))
  egress: 127.0.0.1:$((base + 6))"
	fi
	cat >"$cfg" <<EOF
mode: proxy-sidecar
listener:
  roles: [forward]
  bind_loopback_only: true
  forward_proxy_addr: 127.0.0.1:$proxy
  session_api_addr: 127.0.0.1:$api
  transparent_proxy_addr: 127.0.0.1:$trans
  health_addr: 127.0.0.1:$health
stats:
  address: 127.0.0.1:$stats
tls_bridge:
  mode: enabled
  generate_ca: true
  ca_dir: $home/.cortex/ca
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: mcp-parser
      - name: a2a-parser
$praxis_block
EOF
	ports_ok "$cfg" || die "refusing to start: $cfg names a port of the shared proxy (476xx)"
	last=$((base + 4))
	[ "$mode" != praxis ] || last=$((base + 6))
	for p in $(seq "$base" "$last"); do
		! port_busy "$p" || die "port $p is in use; run down, or stop the other instance"
	done
	# The child must not reach the shared proxy or trust its CA from the caller's shell.
	env -u HTTPS_PROXY -u HTTP_PROXY -u https_proxy -u http_proxy -u ALL_PROXY -u all_proxy \
		-u NO_PROXY -u no_proxy -u SSL_CERT_FILE -u SSL_CERT_DIR \
		HOME="$home" "$s/bin/cortex" -supervise --config "$cfg" </dev/null >"$d/cortex.log" 2>&1 &
	echo $! >"$d/supervisor.pid"
	for _ in $(seq 300); do
		if is_up "$mode" "$(curl -s --noproxy '*' -o /dev/null -w '%{http_code}' "http://127.0.0.1:$health/healthz" || true)" "$d/cortex.log"; then
			echo "$mode up: pid $(cat "$d/supervisor.pid"), proxy 127.0.0.1:$proxy, session API 127.0.0.1:$api"
			return 0
		fi
		sleep 0.1
	done
	tail -20 "$d/cortex.log" >&2
	die "$mode: not ready in 30 s (/healthz 200, and in praxis mode the hop's ready line)"
}

env_cmd() {
	setports "$1"
	s=$(abs "$2")
	home="$s/$1/home"
	ca="$home/.cortex/ca"
	[ -f "$ca/ca.crt" ] && [ -f "$ca/bundle.crt" ] || die "no $ca/ca.crt or $ca/bundle.crt; is $1 up?"
	p="http://127.0.0.1:$proxy"
	echo "export HOME='$home'"
	for v in HTTPS_PROXY HTTP_PROXY https_proxy http_proxy; do echo "export $v='$p'"; done
	echo "export NO_PROXY='127.0.0.1,localhost'"
	echo "export NODE_EXTRA_CA_CERTS='$ca/ca.crt'"
	for v in SSL_CERT_FILE CURL_CA_BUNDLE REQUESTS_CA_BUNDLE GIT_SSL_CAINFO; do echo "export $v='$ca/bundle.crt'"; done
}

# order I: the two modes of pair I, in the order this pair sends them.
order() {
	if [ $(($1 % 2)) = 1 ]; then echo "direct praxis"; else echo "praxis direct"; fi
}

# summarize FILE COLUMN NAMES: prints "<mode> <p50> [<p95>]" per mode and the delta.
summarize() {
	f=$1 col=$2
	shift 2
	for m in direct praxis; do
		line="$m"
		for p in "$@"; do line="$line $(awk -v m="$m" -v c="$col" '$2 == m {print $c}' "$f" | pct "$p")"; done
		echo "$line"
	done
	line="delta"
	for p in "$@"; do
		dv=$(awk -v c="$col" '$2 == "direct" {print $c}' "$f" | pct "$p")
		pv=$(awk -v c="$col" '$2 == "praxis" {print $c}' "$f" | pct "$p")
		line="$line $(awk -v a="$pv" -v b="$dv" 'BEGIN {printf "%.1f", a - b}')"
	done
	echo "$line"
}

probe() {
	s=$(abs "$1")
	n=$2
	url=$3
	body="$s/body.json"
	if [ ! -f "$body" ]; then
		# 513 turns of 2 KiB, not one 1 MiB string: the parsers work per message.
		awk 'BEGIN {
			x = sprintf("%2048s", ""); gsub(/ /, "x", x)
			printf "{\"model\":\"claude-opus-4-6\",\"max_tokens\":16,\"stream\":true,\"messages\":["
			for (i = 1; i <= 513; i++) printf "%s{\"role\":\"%s\",\"content\":\"%s\"}", (i > 1 ? "," : ""), (i % 2 ? "user" : "assistant"), x
			printf "]}"
		}' >"$body"
	fi
	out="$s/probe.tsv"
	: >"$out"
	dropped=0
	for i in $(seq "$n"); do
		pair=""
		codes=""
		for m in $(order "$i"); do
			setports "$m"
			# --noproxy '': NO_PROXY must not send this past the proxy. Expect: no 100-continue wait.
			r=$(curl -s -o /dev/null -w '%{http_code} %{time_total}' --noproxy '' --proxy "http://127.0.0.1:$proxy" \
				--cacert "$s/$m/home/.cortex/ca/bundle.crt" -H 'x-api-key: invalid' -H 'Expect:' \
				-H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' \
				--data-binary @"$body" "$url/v1/messages" || true)
			codes="$codes ${r%% *}"
			pair="$pair$(echo "$r" | awk -v i="$i" -v m="$m" '{printf "%s\t%s\t%s\t%.1f\n", i, m, $1, $2 * 1000}')
"
		done
		# shellcheck disable=SC2086 # two codes, one argument each
		if pair_ok $codes; then printf '%s' "$pair" >>"$out"; else dropped=$((dropped + 1)); fi
	done
	[ -s "$out" ] || die "probe: every pair was dropped ($dropped)"
	summarize "$out" 4 50 95
	echo "codes $(cut -f3 "$out" | sort | uniq -c | awk '{printf "%s%s:%s", (NR > 1 ? " " : ""), $2, $1 / 2}')"
	echo "dropped $dropped"
}

# first_frame_ms CMD...: runs CMD and prints the ms to its first output line that
# starts with "data:". Exit 1 when no such line comes.
first_frame_ms() {
	perl -MTime::HiRes=time -e '
		my $t = time;
		my $pid = open(my $f, "-|", @ARGV) or exit 1;
		while (<$f>) {
			if (/^data:/) { printf "%.1f\n", (time - $t) * 1000; kill "TERM", $pid; exit 0 }
		}
		exit 1' "$@"
}

ttff() {
	[ -n "${ANTHROPIC_AUTH_TOKEN:-}" ] || {
		echo "praxis-live-check: ttff needs ANTHROPIC_AUTH_TOKEN" >&2
		exit 2
	}
	s=$(abs "$1")
	n=$2
	url=$3
	small='{"model":"claude-opus-4-6","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"Say hi."}]}'
	out="$s/ttff.tsv"
	: >"$out"
	dropped=0
	for i in $(seq "$n"); do
		pair=""
		bad=0
		for m in $(order "$i"); do
			setports "$m"
			ms=$(first_frame_ms curl -sN --noproxy '' --proxy "http://127.0.0.1:$proxy" \
				--cacert "$s/$m/home/.cortex/ca/bundle.crt" \
				-K <(printf 'header = "Authorization: Bearer %s"\n' "$ANTHROPIC_AUTH_TOKEN") \
				-H 'anthropic-version: 2023-06-01' \
				-H 'content-type: application/json' --data "$small" "$url/v1/messages" || true)
			[ -n "$ms" ] || bad=1
			pair="$pair$i	$m	$ms
"
		done
		if [ "$bad" = 1 ]; then dropped=$((dropped + 1)); else printf '%s' "$pair" >>"$out"; fi
	done
	[ -s "$out" ] || die "ttff: every pair was dropped ($dropped)"
	summarize "$out" 3 50
	echo "dropped $dropped"
}

# sample_summary: per mode and process, peak RSS, peak heap and CPU seconds used.
# CPU is summed per pid: a restarted process starts its ps time again at 0.
sample_summary() {
	awk '{
		k = $2 " " $3
		p = k " " $4
		if (!(k in rss)) { rss[k] = 0; keys[++nk] = k }
		if (!(p in first)) { first[p] = $6; pk[p] = k }
		last[p] = $6
		if ($5 > rss[k]) rss[k] = $5
		if ($7 != "-" && $7 > heap[k]) heap[k] = $7
	} END {
		for (p in first) cpu[pk[p]] += last[p] - first[p]
		for (i = 1; i <= nk; i++) {
			k = keys[i]
			printf "%s peak_rss_kb %d peak_heap_inuse_bytes %s cpu_s %.2f\n", k, rss[k], (k in heap) ? heap[k] : "-", cpu[k]
		}
	}' "$s/sample.tsv" | tee "$s/sample-summary.txt"
}

# sample_one MODE PROC PID: appends one sample line.
sample_one() {
	ps_out=$(ps -o rss=,time= -p "$3" 2>/dev/null || true)
	[ -n "$ps_out" ] || return 0
	# shellcheck disable=SC2086 # split "rss time" into two fields
	set -- "$1" "$2" "$3" $ps_out
	heap=-
	if [ "$2" = cortex ]; then
		setports "$1"
		# No gc=1: a forced GC every 5 s adds CPU to both cortex rows and skews C4.
		heap=$(curl -s --noproxy '*' "http://127.0.0.1:$stats/debug/pprof/heap?debug=1" |
			awk '/^# HeapInuse = / {print $4}' || true)
		[ -n "$heap" ] || heap=-
	fi
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$(date +%s)" "$1" "$2" "$3" "$4" "$(cputime_s "$5")" "$heap" >>"$s/sample.tsv"
}

sample() {
	s=$(abs "$1")
	secs=$2
	trap 'sample_summary; sample_check "$s/sample.tsv" || exit 1; exit 0' TERM INT
	: >"$s/sample.tsv" # one run per summary; an earlier run's rows would skew it
	start=$(date +%s)
	while :; do
		for m in direct praxis; do
			pid=$(pgrep -f "$s/bin/cortex --config $s/$m/" | head -1 || true)
			[ -n "$pid" ] && sample_one "$m" cortex "$pid"
			if [ "$m" = praxis ]; then
				gpid=$(pgrep -f "$s/bin/cortex-gateway -c $s/$m/" | head -1 || true)
				[ -n "$gpid" ] && sample_one "$m" cortex-gateway "$gpid"
			fi
		done
		[ $(($(date +%s) - start)) -lt "$secs" ] || break
		sleep 5 &
		wait $! || true
	done
	sample_summary
	sample_check "$s/sample.tsv" || exit 1
}

down() {
	s=$(abs "$2")
	case "$1" in
	direct | praxis) modes=$1 pattern="$s/bin/cortex.* .*$s/$1/" ;;
	all) modes="direct praxis" pattern="$s/bin/" ;;
	*) die "down: mode must be direct, praxis or all" ;;
	esac
	for m in $modes; do
		if [ -f "$s/$m/supervisor.pid" ]; then
			kill "$(cat "$s/$m/supervisor.pid")" 2>/dev/null || true
			rm -f "$s/$m/supervisor.pid"
		fi
	done
	for _ in $(seq 50); do
		pgrep -f "$pattern" >/dev/null || { echo "down: $1 stopped"; return 0; }
		sleep 0.1
	done
	echo "down: killing what is left after 5 s:" >&2
	pgrep -fl "$pattern" >&2 || true
	pkill -9 -f "$pattern" || true
	sleep 0.5
	! pgrep -f "$pattern" >/dev/null || die "down: processes still run under $s/bin/"
	echo "down: $1 stopped"
}

usage() {
	sed -n '12,20p' "$0" | sed 's/^# //' >&2
	exit 2
}

case "${1:-}" in
build) [ $# = 2 ] || usage; build "$2" ;;
up) [ $# = 3 ] || usage; up "$2" "$3" ;;
env) [ $# = 3 ] || usage; env_cmd "$2" "$3" ;;
probe) [ $# = 4 ] || usage; probe "$2" "$3" "$4" ;;
sample) [ $# = 3 ] || usage; sample "$2" "$3" ;;
ttff) [ $# = 4 ] || usage; ttff "$2" "$3" "$4" ;;
down) [ $# = 3 ] || usage; down "$2" "$3" ;;
selftest) selftest ;;
*) usage ;;
esac
