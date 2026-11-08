#!/bin/bash
# go-test-shards.sh runs the tests of one Go package as N processes at the same time.
#
# go test runs the tests of one package one at a time, in one process. Tests that
# change package variables or use t.Setenv cannot use t.Parallel. Separate
# processes do not share those, so this script builds the test binary once, gives
# the top-level tests to N shards in turn, and runs the shards together.
#
# Usage: scripts/dev/go-test-shards.sh [-n N] [-short] [-race] <package dir>
#   -n N     the number of shards (default: the number of CPUs, 8 at most)
#   -short   run each shard with -test.short
#   -race    build the test binary with the race detector
#
# The exit status is 0 only when every shard passes. The output of a failed shard
# is printed in full. Tests run in <package dir>, as with go test.
set -euo pipefail

n=$(getconf _NPROCESSORS_ONLN)
[ "$n" -gt 8 ] && n=8
short=""
race=""
while [ $# -gt 0 ]; do
	case "$1" in
	-n) n="$2"; shift 2 ;;
	-short) short=1; shift ;;
	-race) race=1; shift ;;
	-*) echo "go-test-shards: unknown flag $1" >&2; exit 2 ;;
	*) break ;;
	esac
done
if [ $# -ne 1 ] || [ ! -d "$1" ]; then
	echo "usage: $0 [-n N] [-short] [-race] <package dir>" >&2
	exit 2
fi
dir=$1

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

(cd "$dir" && GOWORK=off go test -c ${race:+-race} -o "$tmp/pkg.test" .)
if [ ! -x "$tmp/pkg.test" ]; then
	echo "go-test-shards: $dir has no test files" >&2
	exit 0
fi
(cd "$dir" && "$tmp/pkg.test" -test.list '.*') | grep -E '^(Test|Example|Fuzz)' >"$tmp/all" || true
if [ ! -s "$tmp/all" ]; then
	echo "go-test-shards: $dir has no tests" >&2
	exit 0
fi
awk -v n="$n" -v out="$tmp/list." '{ print > (out ((NR - 1) % n)) }' "$tmp/all"

# Job control. Without it, a background job starts with SIGINT ignored, and its
# children inherit that: a test that sends SIGINT to a child it started then sees
# the signal ignored. set -m cannot restore SIGINT when the caller of this script
# already ignores it. With it, each shard has its own process group, so a Ctrl-C
# at the terminal does not reach the shards: the EXIT trap stops them.
set -m
pids=""
cleanup() {
	# SIGKILL, because a shard can be in a test that catches SIGTERM itself.
	for p in $pids; do
		kill -KILL -- -"$p" 2>/dev/null || true
	done
	rm -rf "$tmp"
}
trap cleanup EXIT

start=$SECONDS
i=0
while [ "$i" -lt "$n" ]; do
	if [ -s "$tmp/list.$i" ]; then
		run="^($(paste -sd'|' "$tmp/list.$i"))\$"
		(
			rc=0
			cd "$dir" && "$tmp/pkg.test" -test.paniconexit0 -test.timeout=10m ${short:+-test.short} -test.run "$run" >"$tmp/out.$i" 2>&1 || rc=$?
			echo "$rc" >"$tmp/rc.$i"
		) &
		pids="$pids $!"
	fi
	i=$((i + 1))
done
wait
pids=""

failed=0
for rcfile in "$tmp"/rc.*; do
	rc=$(cat "$rcfile")
	if [ "$rc" -ne 0 ]; then
		failed=1
		shard=${rcfile##*.}
		echo "--- shard $shard failed (exit $rc):"
		cat "$tmp/out.$shard"
	fi
done
tests=$(wc -l <"$tmp/all" | tr -d ' ')
if [ "$failed" -ne 0 ]; then
	echo "FAIL	$dir	$tests tests in $n shards, $((SECONDS - start))s"
	exit 1
fi
echo "ok	$dir	$tests tests in $n shards, $((SECONDS - start))s"
