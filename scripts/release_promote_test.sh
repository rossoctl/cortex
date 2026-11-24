#!/bin/sh
# Tests for release_promote.sh.
#
# Black-box: a `gh` stub first on PATH answers each call from a JSON fixture,
# running the script's own --jq program over it with the real jq, so the jq
# programs are tested too. It records every call, which is how a case checks
# what was edited. No network.
#
# Run: sh scripts/release_promote_test.sh
set -eu

# shellcheck disable=SC1007 # `CDPATH= cd` empties CDPATH for this one command.
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PROMOTE_SH="${SCRIPT_DIR}/release_promote.sh"
[ -f "${PROMOTE_SH}" ] || { printf 'cannot find %s\n' "${PROMOTE_SH}" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

PASS=0
FAIL=0

check() { # label expected actual
	if [ "$2" = "$3" ]; then
		PASS=$((PASS + 1))
		printf '  ok   %s\n' "$1"
	else
		FAIL=$((FAIL + 1))
		printf '  FAIL %s\n       expected %s\n       actual   %s\n' "$1" "$2" "$3"
	fi
}

check_contains() { # label needle haystack
	case "$3" in
		*"$2"*)
			PASS=$((PASS + 1))
			printf '  ok   %s\n' "$1"
			;;
		*)
			FAIL=$((FAIL + 1))
			printf '  FAIL %s\n       expected to contain %s\n       actual   %s\n' "$1" "$2" "$3"
			;;
	esac
}

mkdir -p "${TMP}/bin"
cat >"${TMP}/bin/gh" <<'EOF'
#!/bin/sh
# Answers from ${FIXTURES}; a missing fixture is a 404.
printf '%s\n' "$*" >>"${GH_LOG}"
expr="."
prev=""
for a in "$@"; do
	[ "${prev}" = "--jq" ] && expr="${a}"
	prev="${a}"
done
fixture=""
case "$1" in
	api)
		shift
		[ "$1" = "--paginate" ] && shift
		path="${1%%\?*}"
		case "${path}" in
			*/actions/runs/*) fixture="run-${path##*/}.json" ;;
			*/actions/workflows/*/runs)
				wf="${path#*/actions/workflows/}"
				fixture="wf-${wf%/runs}.json"
				;;
		esac
		;;
	release)
		case "$2" in
			view) fixture="release-$3.json" ;;
			list) fixture="releases.json" ;;
			edit) exit 0 ;;
		esac
		;;
esac
if [ -z "${fixture}" ] || [ ! -f "${FIXTURES}/${fixture}" ]; then
	echo "gh stub: HTTP 404 (no fixture for: $*)" >&2
	exit 1
fi
jq -r "${expr}" "${FIXTURES}/${fixture}"
EOF
chmod +x "${TMP}/bin/gh"
PATH="${TMP}/bin:${PATH}"
export PATH GH_LOG="${TMP}/gh.log" FIXTURES="${TMP}/fx"
unset PROMOTE_TAG SMOKE_RUN_ID SMOKE_RUN_PATH SMOKE_RUN_TITLE GITHUB_STEP_SUMMARY || true

LINUX=.github/workflows/release-smoke-linux.yaml
MACOS=.github/workflows/release-smoke-macos.yaml

# fresh: an empty call log, and fixtures holding only two full releases, so that a
# case reaching the edit can make it. Without them a case that should stop short
# also passes when the script gets that far and then fails on the 404.
fresh() {
	rm -rf "${FIXTURES}" "${GH_LOG}"
	mkdir -p "${FIXTURES}"
	: >"${GH_LOG}"
	stable v0.9.5 v0.9.4
}

# release_run <id> <head_branch> [conclusion] [path] [event]
release_run() {
	jq -n --argjson id "$1" --arg branch "$2" --arg c "${3:-success}" \
		--arg path "${4:-.github/workflows/release-binaries.yaml}" --arg ev "${5:-push}" \
		'{id: $id, path: $path, event: $ev, conclusion: $c, head_branch: $branch,
		  created_at: "2026-11-20T16:17:16Z"}' >"${FIXTURES}/run-$1.json"
}

# smoke <workflow file> <id:status:conclusion:release run id[:attempt]>...: that
# workflow's runs, each titled the way its run-name titles it. A conclusion of "null"
# is null, and the attempt is 1 when left out.
smoke() {
	wf="$1"
	shift
	for r in "$@"; do
		printf '%s\n' "${r}"
	done | jq -R -s --arg wf "${wf}" '{workflow_runs: [split("\n")[] | select(length > 0)
		| split(":") | {id: (.[0] | tonumber), status: .[1],
		  conclusion: (if .[2] == "null" then null else .[2] end),
		  run_attempt: ((.[4] // "1") | tonumber),
		  display_title: "Release Smoke Test (\($wf)) for v0.9.6, release run \(.[3])"}]}' \
		>"${FIXTURES}/wf-${wf}.json"
}

# release <tag> <isDraft> <isPrerelease>
release() {
	printf '{"tagName":"%s","isDraft":%s,"isPrerelease":%s}\n' "$1" "$2" "$3" \
		>"${FIXTURES}/release-$1.json"
}

# stable <tag>...: the full releases gh release list returns.
stable() {
	printf '%s\n' "$@" | jq -R -s '[split("\n")[] | select(length > 0) | {tagName: .}]' \
		>"${FIXTURES}/releases.json"
}

# after_smoke <path> <id> <tag> <release run id> [attempt]: run the script as the
# workflow_run trigger does. Sets status, out and edits.
after_smoke() {
	status=0
	out=$(REPO=o/r SMOKE_RUN_PATH="$1" SMOKE_RUN_ID="$2" SMOKE_RUN_ATTEMPT="${5:-1}" \
		SMOKE_RUN_TITLE="Release Smoke Test (x) for $3, release run $4" \
		sh "${PROMOTE_SH}" 2>&1) || status=$?
	edits=$(grep '^release edit' "${GH_LOG}" || true)
}

# by_hand <tag>: run the script as workflow_dispatch does.
by_hand() {
	status=0
	out=$(REPO=o/r PROMOTE_TAG="$1" sh "${PROMOTE_SH}" 2>&1) || status=$?
	edits=$(grep '^release edit' "${GH_LOG}" || true)
}

echo "both smoke tests passed"
fresh
release_run 100 v0.9.6
smoke release-smoke-linux.yaml 201:completed:success:100
smoke release-smoke-macos.yaml 301:completed:success:100
release v0.9.6 false true
stable v0.9.5 v0.9.4
after_smoke "${MACOS}" 301 v0.9.6 100
check "exit 0" 0 "${status}"
check "promoted and made Latest" "release edit v0.9.6 --repo o/r --prerelease=false --latest" "${edits}"
check_contains "every stable release is read" "--exclude-pre-releases --limit 10000" "$(cat "${GH_LOG}")"
check_contains "smoke runs listed from the release run's start" \
	"event=workflow_run&created=%3E%3D2026-11-20T16:17:16Z" "$(cat "${GH_LOG}")"

echo "the other smoke test is still running"
fresh
release_run 100 v0.9.6
smoke release-smoke-linux.yaml 201:completed:success:100
smoke release-smoke-macos.yaml 301:in_progress:null:100
release v0.9.6 false true
after_smoke "${LINUX}" 201 v0.9.6 100
check "exit 0" 0 "${status}"
check "nothing edited" "" "${edits}"
check_contains "says what it waits for" "Waiting for release-smoke-macos.yaml(run 301)" "${out}"

echo "the other smoke test has not started"
fresh
release_run 100 v0.9.6
smoke release-smoke-linux.yaml 201:completed:success:100
smoke release-smoke-macos.yaml
release v0.9.6 false true
after_smoke "${LINUX}" 201 v0.9.6 100
check "exit 0" 0 "${status}"
check "nothing edited" "" "${edits}"
check_contains "says it has not started" "release-smoke-macos.yaml(not started)" "${out}"

echo "a run for another release run does not count, even one whose id ends the same"
fresh
release_run 100 v0.9.6
smoke release-smoke-linux.yaml 201:completed:success:100
smoke release-smoke-macos.yaml 301:completed:success:9100 302:completed:success:1000
release v0.9.6 false true
after_smoke "${LINUX}" 201 v0.9.6 100
check "exit 0" 0 "${status}"
check "nothing edited" "" "${edits}"
check_contains "waits for its own" "release-smoke-macos.yaml(not started)" "${out}"

echo "a smoke workflow that cannot be read fails rather than waits"
fresh
release_run 100 v0.9.6
smoke release-smoke-linux.yaml 201:completed:success:100
release v0.9.6 false true
after_smoke "${LINUX}" 201 v0.9.6 100
check "fails" 1 "${status}"
check_contains "names it" "cannot list the runs of release-smoke-macos.yaml" "${out}"
check "nothing edited" "" "${edits}"

echo "the other smoke test failed"
fresh
release_run 100 v0.9.6
smoke release-smoke-linux.yaml 201:completed:failure:100
smoke release-smoke-macos.yaml 301:completed:success:100
release v0.9.6 false true
after_smoke "${MACOS}" 301 v0.9.6 100
check "exit 0: the failed smoke run is the red one" 0 "${status}"
check "nothing edited" "" "${edits}"
check_contains "says it stays a prerelease" "v0.9.6 stays a prerelease: release-smoke-linux.yaml run 201 ended 'failure'" "${out}"

echo "the API has not caught up with the run that triggered this one"
fresh
release_run 100 v0.9.6
smoke release-smoke-linux.yaml 201:completed:success:100
smoke release-smoke-macos.yaml 301:in_progress:null:100
release v0.9.6 false true
stable v0.9.5
after_smoke "${MACOS}" 301 v0.9.6 100
check "the trigger's own success is taken from the event" \
	"release edit v0.9.6 --repo o/r --prerelease=false --latest" "${edits}"

echo "the API lists only an older failed run of the triggering workflow"
# A re-run of the release run after a failed smoke run: the new smoke run (310)
# passed and started this, but the API still lists only the old failure (301).
fresh
release_run 100 v0.9.6
smoke release-smoke-linux.yaml 201:completed:success:100
smoke release-smoke-macos.yaml 301:completed:failure:100
release v0.9.6 false true
after_smoke "${MACOS}" 310 v0.9.6 100
check "exit 0" 0 "${status}"
check "the trigger, newer than anything listed, decides" \
	"release edit v0.9.6 --repo o/r --prerelease=false --latest" "${edits}"

echo "a re-run of the triggering run failed before this read the API"
# Attempt 1 of 301 passed and started this; attempt 2 has failed since.
fresh
release_run 100 v0.9.6
smoke release-smoke-linux.yaml 201:completed:success:100
smoke release-smoke-macos.yaml 301:completed:failure:100:2
release v0.9.6 false true
after_smoke "${MACOS}" 301 v0.9.6 100 1
check "exit 0" 0 "${status}"
check "nothing edited: the later attempt decides" "" "${edits}"
check_contains "names the attempt" "release-smoke-macos.yaml run 301 attempt 2 ended 'failure'" "${out}"

echo "a re-run of the triggering run is still running"
fresh
release_run 100 v0.9.6
smoke release-smoke-linux.yaml 201:completed:success:100
smoke release-smoke-macos.yaml 301:in_progress:null:100:2
release v0.9.6 false true
after_smoke "${MACOS}" 301 v0.9.6 100 1
check "exit 0" 0 "${status}"
check "nothing edited" "" "${edits}"
check_contains "waits for it" "release-smoke-macos.yaml(run 301 attempt 2)" "${out}"

echo "the API still lists an earlier attempt of the triggering run"
# Attempt 2 of 301, a re-run, passed and started this; the API still shows attempt 1's
# failure.
fresh
release_run 100 v0.9.6
smoke release-smoke-linux.yaml 201:completed:success:100
smoke release-smoke-macos.yaml 301:completed:failure:100:1
release v0.9.6 false true
after_smoke "${MACOS}" 301 v0.9.6 100 2
check "exit 0" 0 "${status}"
check "the trigger's attempt decides" \
	"release edit v0.9.6 --repo o/r --prerelease=false --latest" "${edits}"

echo "a newer smoke run of the triggering workflow is still running"
fresh
release_run 100 v0.9.6
smoke release-smoke-linux.yaml 201:completed:success:100
smoke release-smoke-macos.yaml 301:completed:success:100 305:queued:null:100
release v0.9.6 false true
after_smoke "${MACOS}" 301 v0.9.6 100
check "exit 0" 0 "${status}"
check "nothing edited: the newest run decides" "" "${edits}"
check_contains "waits for the newest" "release-smoke-macos.yaml(run 305)" "${out}"

echo "a patch to an older line does not take Latest"
fresh
release_run 100 v0.8.4
smoke release-smoke-linux.yaml 201:completed:success:100
smoke release-smoke-macos.yaml 301:completed:success:100
release v0.8.4 false true
stable v0.9.5 v0.8.3 v0.10.0
after_smoke "${LINUX}" 201 v0.8.4 100
check "promoted, Latest left alone" "release edit v0.8.4 --repo o/r --prerelease=false --latest=false" "${edits}"
check_contains "names the higher release (by version, not by text)" "Latest stays v0.10.0" "${out}"

echo "a release candidate and main-latest are not promoted"
for t in v0.10.0-rc.3 main; do
	fresh
	after_smoke "${LINUX}" 201 "${t}" 100
	check "${t}: exit 0" 0 "${status}"
	check "${t}: gh not called" "" "$(cat "${GH_LOG}")"
done

echo "the release run named in the title must be that tag's successful push"
for bad in "v0.9.7" "v0.9.6 failure" "v0.9.6 success .github/workflows/other.yaml" \
	"v0.9.6 success .github/workflows/release-binaries.yaml workflow_dispatch"; do
	fresh
	# shellcheck disable=SC2086 # split on purpose: the fixture's fields.
	release_run 100 ${bad}
	smoke release-smoke-linux.yaml 201:completed:success:100
	smoke release-smoke-macos.yaml 301:completed:success:100
	release v0.9.6 false true
	after_smoke "${LINUX}" 201 v0.9.6 100
	check "[${bad}]: fails" 1 "${status}"
	check_contains "[${bad}]: on the release run" "is not a successful" "${out}"
	check "[${bad}]: nothing edited" "" "${edits}"
done

echo "an already full release is left alone"
fresh
release_run 100 v0.9.6
smoke release-smoke-linux.yaml 201:completed:success:100
smoke release-smoke-macos.yaml 301:completed:success:100
release v0.9.6 false false
after_smoke "${LINUX}" 201 v0.9.6 100
check "exit 0" 0 "${status}"
check "nothing edited" "" "${edits}"

echo "a draft is refused"
fresh
release_run 100 v0.9.6
smoke release-smoke-linux.yaml 201:completed:success:100
smoke release-smoke-macos.yaml 301:completed:success:100
release v0.9.6 true true
after_smoke "${LINUX}" 201 v0.9.6 100
check "fails" 1 "${status}"
check_contains "says it is a draft" "is a draft" "${out}"
check "nothing edited" "" "${edits}"

echo "a trigger that is not a smoke workflow, or a title it cannot read, fails"
fresh
after_smoke .github/workflows/ci.yaml 201 v0.9.6 100
check "not a smoke workflow: fails" 1 "${status}"
fresh
status=0
out=$(REPO=o/r SMOKE_RUN_PATH="${LINUX}" SMOKE_RUN_ID=201 SMOKE_RUN_TITLE="Release Smoke Test (Linux)" \
	sh "${PROMOTE_SH}" 2>&1) || status=$?
check "a title without the release run: fails" 1 "${status}"
fresh
after_smoke "${LINUX}" "201 x" v0.9.6 100
check "a run id that is not a number: fails" 1 "${status}"
check_contains "says so" "SMOKE_RUN_ID is not a run id" "${out}"
fresh
after_smoke "${LINUX}" 201 v0.9.6 100 "2x"
check "a run attempt that is not a number: fails" 1 "${status}"
check_contains "says so" "SMOKE_RUN_ATTEMPT is not a run attempt" "${out}"

echo "by hand"
fresh
release v0.9.6 false true
stable v0.9.5
by_hand v0.9.6
check "exit 0" 0 "${status}"
check "promoted without asking about smoke runs" \
	"release edit v0.9.6 --repo o/r --prerelease=false --latest" "${edits}"
check "no smoke run looked up" "" "$(grep 'actions/' "${GH_LOG}" || true)"
fresh
by_hand v0.10.0-rc.3
check "a release candidate: refused" 1 "${status}"
check "a release candidate: gh not called" "" "$(cat "${GH_LOG}")"
fresh
by_hand "$(printf 'v0.9.6\nv1.0.0')"
check "a tag with a newline: refused" 1 "${status}"

printf '\n%d passed, %d failed\n' "${PASS}" "${FAIL}"
[ "${FAIL}" -eq 0 ]
