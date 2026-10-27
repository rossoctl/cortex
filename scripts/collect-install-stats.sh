#!/bin/sh
# Collect per-platform release-asset download counts into a daily time series.
#
# GitHub reports `download_count` as a LIFETIME total per asset, so a daily
# figure only exists as a difference between two snapshots. This script records
# one snapshot per run; the deltas are computed at render time (see
# docs/install-stats/index.html) rather than stored, so a backfilled or
# corrected snapshot does not leave a stale delta behind it.
#
# No retroactive history is possible: the series begins at the first run.
#
# What the numbers do and do not mean
# -----------------------------------
# Platform is NOT a usable humans-vs-CI split, and release-smoke-linux.yaml says
# so itself: it installs "exactly as a real user would (curl | sh, downloading
# from GitHub Releases)". It is deliberately indistinguishable from a Linux user
# by behaviour. Treating linux_amd64 as "CI" would therefore erase every real
# Linux user -- confidently wrong rather than merely noisy.
#
# What the smoke test IS, is countable: ONE install per JOB that ran the smoke
# test, as far as these figures are concerned.
#
# Per job, not per run, because the workflow runs an architecture matrix and each
# leg installs independently. A run is worth however many legs got that far, which
# is why this script records that number per run instead of letting the page
# multiply by a constant -- see the query below.
#
# Each job itself installs three times -- a fresh install of the newest stable tag,
# an upgrade to the tag under test, and a no-op re-run of it -- so it fetches more
# than one checksums.txt. But on a push to main the tag under test is main-latest,
# which is excluded from .totals below (--clobber resets its download_count on
# every push, so its counts mean "since the last push" and its contribution is
# noise that can go negative when a real --ref=main install is erased). One fetch
# per job therefore lands where this page counts.
#
#   CI installs per run = its jobs that ran the smoke-test step
#
# The live figures settled the per-job figure rather than the reading of the
# workflow, back when a run WAS one job: v0.8.1 had 71 checksums fetches against 51
# successful runs since it published. At 2 per run, CI alone would have been 102 --
# more than the release had ever been fetched. At 1, 51 CI + 20 people = 71, and the
# Linux tarballs agreed independently (112 / 2 = 56 = 51 + 5). That is the
# measurement the matrix multiplies, not replaces.
#
# A TAG-push run is the exception: its upgrade leg lands on a real release, so the
# people figure can run 1-2 low per job on a day that cut a tag. Stated in the page
# footnote rather than modelled.
#
# Nothing here separates maintainers from strangers -- at a base of ~16, a few
# contributors on a few machines each could account for all of it. Label any
# rendering accordingly; see the <h1> and the caveat block in index.html.
#
# Usage:
#   scripts/collect-install-stats.sh [--out PATH] [--repo OWNER/NAME]
#
# Requires: gh (authenticated), jq.

set -eu

OUT="docs/install-stats/data.json"
REPO="rossoctl/cortex"

while [ $# -gt 0 ]; do
	case "$1" in
	--out)
		[ $# -ge 2 ] || { echo "--out requires a value" >&2; exit 2; }
		OUT="$2"
		shift 2
		;;
	--repo)
		[ $# -ge 2 ] || { echo "--repo requires a value" >&2; exit 2; }
		REPO="$2"
		shift 2
		;;
	-h | --help)
		# Every comment line before the first line of code, rather than a
		# hardcoded range: a range silently stops matching when the header grows,
		# which is how --help came to end mid-sentence and never reach Usage.
		#
		# grep-then-stop rather than a sed range, so a blank line anywhere in or
		# after the header cannot truncate the output early: awk tracks whether it
		# is still in the leading block and exits at the first non-comment,
		# non-blank line.
		awk 'NR == 1 { next }
		     /^#/ { sub(/^# ?/, ""); print; next }
		     /^[[:space:]]*$/ { print ""; next }
		     { exit }' "$0"
		exit 0
		;;
	*)
		echo "unknown argument: $1" >&2
		exit 2
		;;
	esac
done

warn() { echo "warning: $*" >&2; }

command -v gh >/dev/null 2>&1 || { echo "gh is required" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }

today=$(date -u +%Y-%m-%d)
taken_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
tmp=$(mktemp)
snap=$(mktemp)
runs=$(mktemp)
latest=$(mktemp)
merged=$(mktemp)
# The smoke-run query's working files: the raw API response, the id/created_at
# pairs taken from it, the id->installs counts already in the series, and the
# finished rows.
runs_api=$(mktemp)
pairs=$(mktemp)
known=$(mktemp)
rows=$(mktemp)
trap 'rm -f "${tmp}" "${snap}" "${runs}" "${latest}" "${merged}" \
	"${runs_api}" "${pairs}" "${known}" "${rows}"' EXIT INT TERM

# --paginate to cover every release, not just the first page: lifetime totals on
# older tags still move (mirrors, stragglers) and dropping them would silently
# understate the all-time figure.
gh api "repos/${REPO}/releases" --paginate >"${tmp}"

# Smoke runs, as {id, created_at, installs, installs_rule} rather than per-day
# counts.
#
# Timestamps, not calendar dates, because a snapshot interval is not a day. The
# cron fires at 03:17 UTC, so a row covers (previous 03:17, this 03:17] -- and
# only 6 of 50 historical runs started before 03:17. Bucketing by calendar date
# therefore put ~88% of each row's runs in the wrong row, an error larger than the
# ~2.5 installs/day the row reports. Recording the timestamp lets the page bucket
# into the interval it actually drew its downloads from.
#
# ids, because snapshots are MERGED rather than replaced (see the jq below). The
# query returns the last 100 runs, which at ~10/day is about ten days, so a run
# visible today is gone within a fortnight -- and a page that read only the newest
# snapshot would see 0 for older intervals and subtract nothing, silently. Merging
# on id keeps each run exactly once, for as long as the series is kept.
#
# installs, because ONE RUN IS NO LONGER ONE INSTALL. The workflow runs an arch
# matrix, and every leg fresh-installs the current stable release, so a run now
# fetches checksums.txt once per leg. Counting runs and multiplying by a constant
# was right while there was one job and is a phantom install per run now -- and a
# constant of 2 would be just as wrong, retroactively, for every run already in
# the series. So the count is RECORDED PER RUN, from the run's own jobs, and the
# page multiplies by nothing. Adding or removing a matrix leg then needs no change
# on either side.
#
# A job counts when its smoke-test step RAN, whether or not it passed. The fresh
# install is that step's first action, and every check comes after it, so a job
# that fails has almost always installed already (#1455). Counting successful
# jobs missed exactly those: run 38583376563 installed v0.8.1, failed its upgrade
# on an HTTP 500 from GitHub, and was recorded as 0, so its install read as a
# person. A skipped job runs no steps and counts 0. The one error left: a job
# that fails inside the step BEFORE the fresh install (resolving tags, say) counts
# 1 having installed nothing. Exact would need the install in its own step.
#
# Every COMPLETED run is queried whatever its conclusion, because fail-fast: false
# lets one arch fail while the other installs, and a run conclusion filter would
# drop both.
#
# Queried by workflow FILE rather than by filtering every run in the repo: the
# repo-wide /actions/runs endpoint needs --paginate over thousands of runs and
# takes minutes, while this one returns the workflow's own runs in a single page.
# total_count stays the count of ALL runs, unfiltered, because the page uses it to
# decide whether the first snapshot's page of 100 held the whole history.
SMOKE_WORKFLOW="release-smoke-linux.yaml"
if ! gh api \
	"repos/${REPO}/actions/workflows/${SMOKE_WORKFLOW}/runs?per_page=100" \
	>"${runs_api}" 2>/dev/null; then
	# A missing or renamed workflow must not take the whole snapshot down: the
	# download series is the primary signal and stands on its own.
	#
	# null, NOT []: an empty list is a measurement ("no runs"), and a failed query
	# is not one. The page renders null as "—" and a list as a number, so the two
	# stay distinguishable instead of both reading as zero.
	warn "could not read ${SMOKE_WORKFLOW} runs; recording CI as unknown for today"
	echo 'null' >"${runs}"
else
	# A completed run's job outcomes rarely change, so a count already in the series
	# is reused rather than re-fetched. Without this the jobs query below would run
	# ~100 times a day forever to re-derive numbers that have not moved; with it,
	# steady state is the handful of runs since yesterday.
	#
	# Only counts carrying installs_rule "step" are reused. A count recorded without
	# it was taken under the old successful-jobs rule (#1455), so a run still in the
	# last-100 window is queried once more, which costs ~100 queries on the first
	# run after the rule changed. A run already outside the window keeps its old
	# count for good. None of those had failed, so the two rules agree on all of them.
	#
	# "Rarely", not never: "Re-run failed jobs" keeps the run id, so a leg re-run
	# after its run was counted installs again, and this loop never asks about that
	# run again. That install reads as a person. No smoke run had been re-run as of
	# 2026-10-21. Re-querying when run_attempt rises is not enough on its own: the
	# jobs endpoint defaults to filter=latest, which lists every job under the new
	# attempt, including copies of the jobs that were not re-run, so a failed leg
	# that installed and was then re-run counts once instead of twice. Counting
	# exactly needs filter=all, dropping each copy (a job whose name and started_at
	# match a job in an earlier attempt).
	if [ -f "${OUT}" ]; then
		jq -r '[.snapshots[]?.smoke_runs[]?
		        | select(type == "object" and (.installs | type) == "number"
		                 and .installs_rule == "step")
		        | "\(.id) \(.installs)"]
		       | unique | .[]' "${OUT}" >"${known}" 2>/dev/null || : >"${known}"
	else
		: >"${known}"
	fi

	jq -r '.workflow_runs[]
	       | select(.status == "completed")
	       | "\(.id)\t\(.created_at)"' "${runs_api}" >"${pairs}"
	total_count=$(jq -r '.total_count' "${runs_api}")

	: >"${rows}"
	queried=0
	jobs_failed=0
	tab=$(printf '\t')
	while IFS="${tab}" read -r id created; do
		[ -n "${id}" ] || continue
		n=$(awk -v id="${id}" '$1 == id { print $2; exit }' "${known}")
		if [ -z "${n}" ]; then
			queried=$((queried + 1))
			# A job counts when its smoke-test step ran (see the comment above the
			# runs query). The step name must match release-smoke-linux.yaml. If it
			# drifts, `conclusion == "success"` still counts every passing job, so a
			# rename brings back #1455 rather than zeroing CI.
			#
			# per_page=100 because the endpoint defaults to 30 jobs, and a run with
			# more legs than that would be undercounted without any error.
			if ! n=$(gh api "repos/${REPO}/actions/runs/${id}/jobs?per_page=100" \
				--jq '[.jobs[] | select(.conclusion == "success"
				        or any(.steps[]?; .name == "Run the Linux release smoke test"
				                         and .conclusion != "skipped"))] | length' \
				2>/dev/null); then
				# One unreadable run is not worth discarding the rest. It is simply
				# absent from today's snapshot and gets queried again tomorrow, which
				# is the same self-repair the merge-on-id already provides.
				jobs_failed=$((jobs_failed + 1))
				continue
			fi
		fi
		# A run that installed nothing is recorded as 0 rather than omitted. It
		# subtracts nothing either way, so this is not about the arithmetic — it is
		# about the reuse above being able to see it. Omitted, a skipped run is
		# absent from the series, so it looks newly-seen every single day and gets
		# re-queried forever: 33 of the 100 in today's window are skipped, which was
		# 33 pointless requests a day until this line stopped dropping them. The page
		# filters zeros out when it counts runs.
		printf '%s\t%s\t%s\n' "${id}" "${created}" "${n}" >>"${rows}"
	done <"${pairs}"

	[ "${jobs_failed}" -eq 0 ] ||
		warn "could not read jobs for ${jobs_failed} of ${queried} newly-seen run(s); they are omitted from today and retried tomorrow"

	if [ ! -s "${rows}" ] && [ "${jobs_failed}" -gt 0 ]; then
		# Nothing measured and a known reason why. Recording [] here would read as
		# "no CI ran", which is the silent zero the null above exists to avoid.
		warn "no run could be read; recording CI as unknown for today"
		echo 'null' >"${runs}"
	else
		jq -Rn --argjson total "${total_count}" '
		  {total_count: $total,
		   runs: [inputs
		          | split("\t")
		          | {id: (.[0] | tonumber),
		             created_at: .[1],
		             installs: (.[2] | tonumber),
		             installs_rule: "step"}]}
		' <"${rows}" >"${runs}"
	fi
fi

# GitHub's own answer to "which release does the plain installer take", recorded
# rather than derived. /releases/latest is the contract the installer follows, and
# it is the only thing that gets it right: sorting tags lexically puts v0.9.0 above
# v0.10.0, and sorting by date marks a v0.7.x backport published after v0.8.1 as
# current (release-0.6 and release-0.7 are both still live, so that is a real case
# here, not a hypothetical).
#
# `--jq .tag_name` would print a BARE string, which is not valid JSON for
# --slurpfile; `{tag_name}` keeps it an object the jq below can index.
if ! gh api "repos/${REPO}/releases/latest" --jq '{tag_name}' >"${latest}" 2>/dev/null; then
	warn "could not read the latest release; the page will mark no release current"
	echo 'null' >"${latest}"
fi

# Per-asset counts are collapsed into per-platform sums here rather than stored
# raw: keeping every asset name would grow the file without adding signal. The
# platform is kept because it says WHERE a release is being pulled, not who by --
# it cannot separate CI from people, for the reason in the header.
#
# checksums.txt is the primary install signal, and the reason is arithmetic: the
# installer fetches TWO tarballs per install (cortex + agentop) and verifies both
# against ONE checksums file. So a platform count double-counts an install while
# checksums counts it once, and anything derived from platform counts has to be
# halved to compare with it.
#
# That 1:2 ratio is stable across every release with traffic -- v0.8.1 66:133,
# v0.8.0 10:22, v0.7.0 18:37 -- which is what makes checksums trustworthy as an
# install count rather than a coincidence. A sustained drift away from ~2.0 means
# the installer changed what it fetches, or something is pulling tarballs
# directly (v0.7.0-alpha.7 reads 3.8x, which is the latter).
#
# (An earlier version of this comment claimed checksums EQUALLED the platform sum
# on v0.8.1. That was a mid-release snapshot read as a validating identity; it is
# 1:2, not 1:1.)
jq --arg date "${today}" \
   --arg taken_at "${taken_at}" \
   --slurpfile smoke "${runs}" \
   --slurpfile latest "${latest}" '
  def platform_of($name):
    if   $name | test("darwin_arm64") then "darwin_arm64"
    elif $name | test("darwin_amd64") then "darwin_amd64"
    elif $name | test("linux_arm64")  then "linux_arm64"
    elif $name | test("linux_amd64")  then "linux_amd64"
    # Unanchored: v0.1 through v0.4 named this file
    # kagenti-extensions_<ver>_checksums.txt, which an anchored ^checksums missed.
    # Those 24 releases were filed under "other" and read 0 installs apiece -- 79
    # downloads invisible, and the all-time total understated by 24% (257 against
    # 338). The platform tests above come first and no published asset has ever had
    # "checksums" in a tarball name, so widening this reclassifies nothing else.
    elif $name | test("checksums")    then "checksums"
    else "other"
    end;

  {
    date: $date,

    # The instant this snapshot was taken. The downloads in it accrued over
    # (previous taken_at, this taken_at], so this is what the page buckets CI runs
    # into. A calendar date cannot stand in for it: the cron fires at 03:17 UTC,
    # and almost every run lands after that.
    taken_at: $taken_at,

    # Every completed smoke run, as {id, created_at, installs, installs_rule}, or
    # null when the query failed. The page merges these across snapshots on id, so
    # a run stays counted after it falls out of the last-100 window the API
    # returns, and it ignores the installs:0 rows when counting runs.
    #
    # The installs field counts the jobs of that run that ran the smoke-test step,
    # and installs_rule "step" marks it as counted that way (an older record without
    # it counted successful jobs). Runs recorded before the arch matrix landed have
    # no installs field, and the page reads a missing one as 1 -- exactly right for
    # them, since the workflow then had one job.
    #
    # (No apostrophes in this jq program, as the note further down says: it is
    # single-quoted in the shell, so one ends the quote and shellcheck reports it as
    # a mysterious function-definition error. Bitten a 4th time writing this.)
    smoke_runs: ($smoke[0].runs // null),

    # How many runs the workflow has EVER had, successful or not, per the API.
    #
    # Lets the page tell a truncated history from a complete one. A page of 100 is
    # all the API gives, so when total_count is at or below 100 the first snapshot
    # holds the entire history for this workflow and no run is missing -- without
    # this, a release published before the workflow existed gets a floor marker for
    # runs that never happened.
    #
    # (No apostrophes in this jq program: it is single-quoted in the shell, so one
    # would end the quote and produce a confusing jq syntax error. Bitten 3x.)
    smoke_runs_total_count: ($smoke[0].total_count // null),

    # The tag GitHub serves as /releases/latest -- the release the plain installer
    # takes, and so the one CI installs. Recorded rather than derived: see the
    # query above for why neither tag sort nor date sort gets this right.
    latest_release: ($latest[0].tag_name // null),

    # Released tags only. A draft has no public download path, so counting it
    # would add a row that can never move.
    #
    # main-latest is dropped. release-binaries.yaml re-uploads it with --clobber on
    # every push to main, which RESETS download_count, so its counts mean "since the
    # last push" rather than lifetime. Summed into .totals it contributes noise
    # around zero that can go negative when a real --ref=main install is erased.
    releases: [
      .[]
      | select(.draft | not)
      | select(.tag_name != "main-latest")
      | {
        tag: .tag_name,
        published_at: .published_at,
        prerelease: .prerelease,
        platforms: (
          reduce (.assets[] | {p: platform_of(.name), c: .download_count}) as $a
            ({}; .[$a.p] = ((.[$a.p] // 0) + $a.c))
        ),
      }
    ],
  }
  # Totals are derived once here so every consumer agrees on them.
  | .totals = (
      reduce (.releases[].platforms | to_entries[]) as $e
        ({}; .[$e.key] = ((.[$e.key] // 0) + $e.value))
    )
' "${tmp}" >"${snap}"

mkdir -p "$(dirname "${OUT}")"
[ -f "${OUT}" ] || echo '{"snapshots":[]}' >"${OUT}"

# Re-running on the same day replaces that day's snapshot instead of appending a
# second one. Makes the job idempotent, so a manual workflow_dispatch after a
# failed cron run is safe and leaves exactly one row per date.
#
# Snapshots written before the checksums pattern was unanchored are repaired on the
# way past. Those misclassified downloads were counted -- just filed under "other"
# instead of "checksums" -- so moving them is exact arithmetic, not a guess, and it
# is what keeps the series continuous rather than showing a one-day jump of ~79 in a
# column whose daily deltas are 1-5.
#
# Keyed on the presence of "other": a snapshot taken after the fix has none, so this
# is a no-op from then on and needs no version flag or cutoff date. The per-release
# figures inside each snapshot are left alone -- they are what a future reader would
# recompute from, and they are already right under the new pattern.
jq --slurpfile snap "${snap}" '
  def repair:
    if (.totals.other // 0) > 0 then
      .totals.checksums = ((.totals.checksums // 0) + .totals.other)
      | del(.totals.other)
    else . end;

  .snapshots
  |= ( map(select(.date != $snap[0].date) | repair) + $snap
       | sort_by(.date) )
' "${OUT}" >"${merged}"

# cat rather than mv: OUT may live on a different filesystem than TMPDIR (it does
# under the Actions worktree), where mv across devices is not atomic anyway, and
# leaving the file in place keeps the trap responsible for removing it.
cat "${merged}" >"${OUT}"

days=$(jq '.snapshots | length' "${OUT}")
echo "recorded ${today} in ${OUT} (${days} day(s) of history)"
# How many runs needed a jobs query. Reported because it is the only visible sign
# that reusing counts from the series still works: steady state is the handful of
# runs since yesterday, and a hundred every day means the reuse broke and the job
# is re-deriving numbers that cannot have changed. Nothing fails when that happens
# -- it just takes 90s instead of 30s, which no one would look into.
echo "  newly-queried runs: ${queried:-0}"
jq -r '.snapshots[-1]
       | "  cumulative: darwin_arm64=\(.totals.darwin_arm64 // 0)"
       + " linux_amd64=\(.totals.linux_amd64 // 0)"
       + " checksums=\(.totals.checksums // 0)",
         "  latest release: \(.latest_release // "unknown")",
         "  smoke runs in window: \(
            if .smoke_runs == null then "unknown (query failed)"
            else ([.smoke_runs[] | .installs // 1] | map(select(. > 0))) as $i
                 | "\($i | length) installing run(s) of \(.smoke_runs | length) completed, "
                 + "\($i | add // 0) CI install(s)" end)"' "${OUT}"
