#!/bin/bash
# go-coverage.sh runs the unit tests of the Go modules that ship in a binary, and
# optionally reports statement coverage for the CLI and for the proxy.
#
# Usage: scripts/dev/go-coverage.sh [-cover] [-race] [-short]
#   -cover   write a profile per module and print the coverage of each group
#   -race    build with the race detector (CI does; it is slower)
#   -short   pass -short to go test. It skips cmd/agentop's setup, service, doctor
#            and uninstall scenes, which take about 6 minutes on a Mac
#
# This is behind `make test` and `make coverage`. Without -cover, cmd/agentop runs
# through scripts/dev/go-test-shards.sh. Two groups are reported:
#   CLI    cmd/agentop
#   proxy  core, core/storage/redis, cmd/cortex, cmd/cortex-envoy
# The proxy's code lives mostly in core/, which the two cmd/ modules only wire up, so
# the group is every module the proxy binaries are built from. cmd/cortex and
# cmd/cortex-envoy link no plugins without build tags, so each runs under the profile
# it ships with (scripts/profile-tags), as CI's "Build + test each shipped profile"
# step does. scripts/profile-tags and scripts/readme-demo are not included.
#
# A group's figure is statements covered over statements in total, summed over its
# module profiles. A module counts only its own packages: no -coverpkg, so a statement
# in core/ run by a cmd/cortex test is not counted a second time.
set -euo pipefail

cover="" race="" short=""
while [ $# -gt 0 ]; do
	case "$1" in
	-cover) cover=1 ;;
	-race) race=1 ;;
	-short) short=1 ;;
	*) echo "usage: $0 [-cover] [-race] [-short]" >&2; exit 2 ;;
	esac
	shift
done

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# <group>:<module dir>:<profile>. An empty profile means no build tags.
modules="
cli:cmd/agentop:
proxy:core:
proxy:core/storage/redis:
proxy:cmd/cortex:full
proxy:cmd/cortex-envoy:envoy
"

# "covered total" statement counts from the profiles named on the command line.
# A profile line is "file:start,end numStmts count"; "mode:" lines are skipped.
totals() {
	awk '$1 !~ /^mode:/ { all += $2; if ($3 > 0) hit += $2 } END { printf "%d %d\n", hit, all }' "$@"
}

pct() { awk -v h="$1" -v a="$2" 'BEGIN { printf "%.1f%%", a ? 100 * h / a : 0 }'; }

failed=0
cli_profiles=() proxy_profiles=()
for entry in $modules; do
	IFS=: read -r group dir profile <<<"$entry"
	args=(-count=1)
	[ -n "$race" ] && args+=(-race)
	[ -n "$short" ] && args+=(-short)
	if [ -n "$profile" ]; then
		args+=(-tags "$(go -C "$root/scripts/profile-tags" run . "$profile")")
	fi
	out="$tmp/$(echo "$dir" | tr / _).cover"
	[ -n "$cover" ] && args+=(-coverprofile="$out")
	echo "→ $dir${profile:+ (profile $profile)}"
	# GOWORK=off, as CI does, so the module resolves its own replace directives.
	if [ "$dir" = cmd/agentop ] && [ -z "$cover" ]; then
		# agentop's root package runs its tests one at a time, which is most of the
		# time this script takes. go-test-shards.sh splits them across processes,
		# but tests only the root package and writes no profile, so the other
		# packages run with plain go test, and -cover keeps the unsharded path.
		shard_args=()
		[ -n "$race" ] && shard_args+=(-race)
		[ -n "$short" ] && shard_args+=(-short)
		# Both run even when the first fails, so one run reports every failure. The
		# package list is read first, so a failing go list fails the run instead of
		# leaving go test with no packages. A loop, not mapfile: macOS ships bash 3.2.
		if ! "$root/scripts/dev/go-test-shards.sh" ${shard_args[@]+"${shard_args[@]}"} "$root/$dir"; then
			failed=1
		fi
		if pkgs=$(cd "$root/$dir" && GOWORK=off go list ./...); then
			subpkgs=()
			while IFS= read -r pkg; do
				case "$pkg" in
				*/agentop) ;; # the root package, sharded above
				*) subpkgs+=("$pkg") ;;
				esac
			done <<<"$pkgs"
			if ! (cd "$root/$dir" && GOWORK=off go test "${args[@]}" "${subpkgs[@]}"); then
				failed=1
			fi
		else
			failed=1
		fi
		continue
	fi
	if ! (cd "$root/$dir" && GOWORK=off go test "${args[@]}" ./...); then
		failed=1
		continue
	fi
	[ -z "$cover" ] && continue
	case "$group" in
	cli) cli_profiles+=("$out") ;;
	proxy) proxy_profiles+=("$out") ;;
	esac
done

if [ "$failed" -ne 0 ]; then
	echo "tests failed" >&2
	exit 1
fi
if [ -n "$cover" ]; then
	echo
	read -r hit all < <(totals "${cli_profiles[@]}")
	echo "CLI (agentop) coverage:   $(pct "$hit" "$all") of statements ($hit/$all)"
	read -r hit all < <(totals "${proxy_profiles[@]}")
	echo "proxy (cortex) coverage:  $(pct "$hit" "$all") of statements ($hit/$all)"
fi
