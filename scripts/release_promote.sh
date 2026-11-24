#!/bin/sh
# release_promote.sh — makes a staged stable release a full release (#956).
#
# release-binaries.yaml publishes a new stable tag (vX.Y.Z) as a PRERELEASE, so that
# nothing installs it before it is tested: the plain one-liner, `agentop update` and
# the auto-updater all follow GitHub's Latest badge, and GitHub never gives that badge
# to a prerelease. release-promote.yaml runs this script after each release smoke
# run. When the Linux and the macOS smoke tests have both passed for the release run
# that published the tag, it clears the prerelease flag. It also gives the release
# the Latest badge, but only when no higher stable release exists: a patch to an
# older line must not take Latest from a newer one.
#
# Two ways in:
#   - After a smoke run passed (the workflow_run trigger). SMOKE_RUN_ID,
#     SMOKE_RUN_ATTEMPT, SMOKE_RUN_PATH and SMOKE_RUN_TITLE describe that run.
#     The title is the smoke workflow's run-name, "... for <tag>, release run
#     <id>": a run started by workflow_run records nothing else that names the run
#     that started it.
#   - By hand (workflow_dispatch). PROMOTE_TAG names the tag, and the smoke tests
#     are NOT checked: this is the override for when they cannot pass for a reason
#     that is not the release's, such as a runner outage.
#
# Either way, only a stable tag is promoted, and only while its release is a
# prerelease. A release that is already a full release is left alone.
#
# Env: REPO (owner/name), and the variables above. Needs gh, authenticated with
# contents: write and actions: read, and jq.
#
# Tests: scripts/release_promote_test.sh.
set -eu

REPO="${REPO:?REPO is not set}"
RELEASE_WORKFLOW=".github/workflows/release-binaries.yaml"
# Every smoke workflow that must pass. Each needs the run-name format above.
SMOKE_WORKFLOWS="release-smoke-linux.yaml release-smoke-macos.yaml"

say() { printf '%s\n' "$*"; }
die() {
	printf '::error::%s\n' "$*" >&2
	exit 1
}
# summary prints a result and adds it to the job summary. warn_summary does the same
# as a warning, so it also shows on the run's page.
summary() {
	say "$*"
	[ -z "${GITHUB_STEP_SUMMARY:-}" ] || printf '%s\n' "$*" >>"${GITHUB_STEP_SUMMARY}"
}
warn_summary() {
	printf '::warning::%s\n' "$*"
	[ -z "${GITHUB_STEP_SUMMARY:-}" ] || printf '%s\n' "$*" >>"${GITHUB_STEP_SUMMARY}"
}

# is_stable: vX.Y.Z and nothing else. The case rejects any other character first,
# a newline included, so the grep sees exactly one line.
is_stable() {
	case "$1" in
		'' | *[!v0-9.]*) return 1 ;;
	esac
	printf '%s\n' "$1" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'
}

# smoke_state <workflow file> <release run id> prints the newest run of that smoke
# workflow for that release run as "<id> <attempt> <status> <conclusion>", or
# nothing. There can be more than one: re-running the release run starts a second
# smoke run. The newest decides. Re-running a smoke run keeps its id and raises its
# attempt, and the API lists only its latest attempt. A workflow it cannot read is
# an error, not "not started": that would wait for ever without a word.
smoke_state() {
	_runs=$(gh api --paginate \
		"repos/${REPO}/actions/workflows/$1/runs?event=workflow_run&created=%3E%3D${release_created}&per_page=100" \
		--jq ".workflow_runs[] | select(.display_title | endswith(\", release run $2\"))
		      | \"\(.id) \(.run_attempt // 1) \(.status) \(.conclusion // \"none\")\"") ||
		die "cannot list the runs of $1"
	[ -z "${_runs}" ] || printf '%s\n' "${_runs}" | sort -n | tail -n 1
}

if [ -n "${PROMOTE_TAG:-}" ]; then
	tag="${PROMOTE_TAG}"
	is_stable "${tag}" || die "${tag} is not a stable tag (vX.Y.Z). Only a stable release is promoted."
	say "Promoting ${tag} by hand. The smoke tests are not checked."
else
	title="${SMOKE_RUN_TITLE:?SMOKE_RUN_TITLE is not set}"
	trigger_id="${SMOKE_RUN_ID:?SMOKE_RUN_ID is not set}"
	trigger_attempt="${SMOKE_RUN_ATTEMPT:-1}"
	trigger_path="${SMOKE_RUN_PATH:?SMOKE_RUN_PATH is not set}"
	case "${trigger_id}" in
		*[!0-9]*) die "SMOKE_RUN_ID is not a run id: '${trigger_id}'" ;;
	esac
	case "${trigger_attempt}" in
		'' | *[!0-9]*) die "SMOKE_RUN_ATTEMPT is not a run attempt: '${trigger_attempt}'" ;;
	esac
	case " ${SMOKE_WORKFLOWS} " in
		*" ${trigger_path#.github/workflows/} "*) ;;
		*) die "${trigger_path} is not a release smoke workflow (${SMOKE_WORKFLOWS})" ;;
	esac
	release_run=$(printf '%s\n' "${title}" | sed -n 's/^.*, release run \([0-9][0-9]*\)$/\1/p')
	tag=$(printf '%s\n' "${title}" | sed -n 's/^.* for \(.*\), release run [0-9][0-9]*$/\1/p')
	if [ -z "${release_run}" ] || [ -z "${tag}" ]; then
		die "cannot read the tag and the release run from the smoke run's title: '${title}'"
	fi
	# main-latest and release candidates are smoke-tested too, and stay prereleases.
	if ! is_stable "${tag}"; then
		say "Nothing to promote: ${tag} is not a stable tag."
		exit 0
	fi

	# The title is only a claim. The release run it names must be the tag push
	# that published this tag, and it must have succeeded.
	release=$(gh api "repos/${REPO}/actions/runs/${release_run}" \
		--jq '"\(.path) \(.event) \(.conclusion // "none") \(.head_branch) \(.created_at)"')
	# shellcheck disable=SC2086 # split on purpose: five space-free fields.
	set -- ${release}
	if [ "$#" -ne 5 ] || [ "$1" != "${RELEASE_WORKFLOW}" ] || [ "$2" != push ] ||
		[ "$3" != success ] || [ "$4" != "${tag}" ]; then
		die "release run ${release_run} is not a successful ${RELEASE_WORKFLOW} tag push for ${tag}: ${release}"
	fi
	release_created="$5"

	waiting=""
	for wf in ${SMOKE_WORKFLOWS}; do
		state=$(smoke_state "${wf}" "${release_run}")
		# The run that started this one passed: the trigger fires on success only.
		# Taken from the event, not the API, so that the API catching up late
		# cannot lose the promotion: when the newest run the API lists is older than
		# the trigger, or is the trigger at the same attempt or an earlier one, the
		# trigger decides. Run ids and attempts only go up, so a newer run, or a later
		# attempt of this one (a re-run that failed, say), still wins.
		if [ ".github/workflows/${wf}" = "${trigger_path}" ]; then
			if [ -z "${state}" ]; then
				state="${trigger_id} ${trigger_attempt} completed success"
			else
				listed_id="${state%% *}"
				listed_rest="${state#* }"
				listed_attempt="${listed_rest%% *}"
				if [ "${listed_id}" -lt "${trigger_id}" ] ||
					{ [ "${listed_id}" -eq "${trigger_id}" ] && [ "${listed_attempt}" -le "${trigger_attempt}" ]; }; then
					state="${trigger_id} ${trigger_attempt} completed success"
				fi
			fi
		fi
		# "run <id>", with the attempt when it is not the first.
		run="run ${state%% *}"
		if [ -n "${state}" ]; then
			attempt_rest="${state#* }"
			[ "${attempt_rest%% *}" = 1 ] || run="${run} attempt ${attempt_rest%% *}"
		fi
		case "${state}" in
			*" completed success") say "${wf}: passed (${run})" ;;
			*" completed "*)
				warn_summary "${tag} stays a prerelease: ${wf} ${run} ended '${state##* }'. If the failure is not the release's, re-run the failed jobs. If ${tag} is still a prerelease after they pass, run release-promote.yaml for it by hand."
				exit 0
				;;
			'') waiting="${waiting} ${wf}(not started)" ;;
			*) waiting="${waiting} ${wf}(${run})" ;;
		esac
	done
	if [ -n "${waiting}" ]; then
		say "Waiting for${waiting}: the last smoke run to pass promotes ${tag}."
		exit 0
	fi
fi

flags=$(gh release view "${tag}" --repo "${REPO}" --json isDraft,isPrerelease \
	--jq '"\(.isDraft) \(.isPrerelease)"')
case "${flags}" in
	"false true") ;;
	"false false")
		say "Nothing to do: ${tag} is already a full release."
		exit 0
		;;
	*) die "${tag} is a draft. Publish it before promoting it." ;;
esac

# Latest goes to the highest stable version, not to whichever was promoted last.
# Every stable release, so --limit is far past any real count: gh pages until it runs
# out, and the highest version need not be among the newest releases.
stable=$(gh release list --repo "${REPO}" --exclude-drafts --exclude-pre-releases \
	--limit 10000 --json tagName --jq '.[].tagName')
highest=$(printf '%s\n%s\n' "${stable}" "${tag}" | while IFS= read -r t; do
	if is_stable "${t}"; then printf '%s\n' "${t}"; fi
done | sort -V | tail -n 1)
if [ "${highest}" = "${tag}" ]; then
	gh release edit "${tag}" --repo "${REPO}" --prerelease=false --latest >/dev/null
	summary "Promoted ${tag}: a full release, and Latest."
else
	gh release edit "${tag}" --repo "${REPO}" --prerelease=false --latest=false >/dev/null
	summary "Promoted ${tag}: a full release. Latest stays ${highest}, a higher version."
fi
