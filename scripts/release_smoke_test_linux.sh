#!/bin/sh
# release_smoke_test_linux.sh — the Linux half of the release smoke test (#957).
#
# Exercises install.sh (and the agentop setup it hands off to) + agentop service
# exactly as a real user would, against
# a real published release: fresh install, upgrade over an existing install
# (config preserved, and the NEW binary actually serving), a no-op re-run, a
# switch to the cpex build and back, and a clean uninstall. Run against a real tag/release, not a local build — there
# is no supported "install from this on-disk tarball" mode in install.sh, so
# this always downloads for real.
#
# Usage: release_smoke_test_linux.sh <tag-under-test> [triggering-commit-sha]
# The sha is only needed for a main-latest run, to anchor the post-upgrade
# version check to the exact commit that triggered this run rather than
# whatever main-latest happens to point at by the time the check executes.
#
# Deliberately NOT covered here: driving a real request through the proxy and
# asserting a parsed event with a non-zero token count (#957's third bullet).
# That needs the unattended/headless capture path from #955, which does not
# exist yet — tracked there, not attempted here.
set -eu

TAG="${1:?usage: release_smoke_test_linux.sh <tag-under-test> [sha]}"
TRIGGER_SHA="${2:-}"
AGENTOP="${HOME}/.local/bin/agentop"
PROXY_BIN="${HOME}/.local/bin/cortex"
CFG="${HOME}/.cortex/config.yaml"
UNIT="${HOME}/.config/systemd/user/cortex.service"

# One temp dir, cleaned up on any exit (success, an assertion's exit 1, or an
# uncaught error under set -e) — the same pattern scripts/install_test.sh and
# scripts/dev/verify-moved-ca-diagnostics.sh already use, rather than a
# scattered rm -f after each individual mktemp that an early exit would skip.
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

# Downloads to a temp file first rather than piping curl straight into sh:
# /bin/sh on ubuntu-latest is dash, which has no pipefail, so a failed
# download (a 404, a network blip) would otherwise leave sh reading empty
# stdin and the pipeline exiting 0 — the failure would only surface later, at
# assert_healthy, pointing at "the service is unhealthy" rather than "the
# download failed."
#
# Still never invoked as a local file, though: install.sh's own re-exec
# bootstrap only re-fetches and re-runs the tagged copy of itself (matching
# the requested --ref) when $0 is not a readable file — the exact condition a
# real `curl | sh` user hits. Reading the script from a temp file via stdin
# redirection (rather than running it as `sh /path/to/tmpfile`) keeps that
# property: $0 stays "sh", not a file path.
#
# `return`, not `exit`, on failure: this function is called under `if !` at
# the no-op-re-run call site below. `exit` inside a function terminates the
# WHOLE script immediately, even when the function is the subject of an `if`
# — so an `exit` here would skip that call site's own `then` branch (which
# prints the captured output for diagnosis) entirely, leaving a bare red CI
# run with the failure message trapped in a file nobody prints. `return`
# propagates the failure to the caller instead, which every call site (both
# the bare ones under `set -e` and the one under `if !`) handles correctly.
# Arguments after the tag are passed to install.sh as they are (--cpex).
install_cortex() {
	ref="$1"
	shift
	tmp="$(mktemp "${TMP_DIR}/install.XXXXXX")"
	installer_url="https://raw.githubusercontent.com/rossoctl/cortex/main/scripts/install.sh"
	if [ "${ref}" = "main-latest" ]; then
		# Pin the installer to the exact commit that triggered this run,
		# instead of "whatever main currently looks like": raw.githubusercontent.com
		# caches responses for a few minutes, and a commit that changes
		# install.sh and the binaries together (as the rename PR did) has a
		# narrow window to be tested with a mismatched pairing — new
		# binaries served with stale installer logic, or vice versa. A
		# commit SHA in the URL is immutable content; the branch name "main"
		# is not. By the time TAG is "main-latest", an earlier check already
		# guarantees TRIGGER_SHA is non-empty.
		installer_url="https://raw.githubusercontent.com/rossoctl/cortex/${TRIGGER_SHA}/scripts/install.sh"
	fi
	if ! curl -fsSL -o "${tmp}" "${installer_url}"; then
		echo "FAIL: could not download install.sh" >&2
		return 1
	fi
	if [ ! -s "${tmp}" ]; then
		echo "FAIL: downloaded install.sh is empty" >&2
		return 1
	fi
	sh -s -- --ref="${ref}" --yes "$@" <"${tmp}"
}

log() { printf '\n== %s ==\n' "$1"; }

assert_contains() {
	# assert_contains <file> <needle> <description>
	if ! grep -q -- "$2" "$1"; then
		echo "FAIL: $3" >&2
		echo "--- $1 ---" >&2
		cat "$1" >&2
		exit 1
	fi
}

# Checked directly, not piped through tee: a pipeline's exit status under
# dash (no pipefail) is tee's, not agentop's — a missing agentop binary
# (exit 127) would still let the pipeline "succeed" with an empty ${out},
# so the real cause (command not found) gets reported as the unrelated
# "did not report healthy" instead.
#
# Both wordings in the needle: the fresh install below runs the PREVIOUS
# release's agentop, which says "healthy: <url>", where this one says
# "Cortex is healthy according to <url>". GNU grep's \| alternation; this
# runs on ubuntu.
assert_healthy() {
	out="$(mktemp "${TMP_DIR}/status.XXXXXX")"
	if ! "${AGENTOP}" service status >"${out}" 2>&1; then
		cat "${out}" >&2
		echo "FAIL: agentop service status exited non-zero" >&2
		exit 1
	fi
	cat "${out}"
	assert_contains "${out}" 'healthy:\|Cortex is healthy according to' "agentop service status did not report healthy"
}

# Confirms the systemd unit is actually running the binary just installed, not
# a stale process left over from before the upgrade — exactly the regression
# #1203's own systemd fix addressed (pre-fix, agentop service install under
# systemd ran enable --now even when a unit was already active, leaving the
# OLD process serving under a unit that still reports healthy). assert_healthy
# and the config-marker check both pass in that exact broken state, since
# neither looks at which process is actually behind the port — this is the
# one check in this script that would have caught it.
#
# Compares /proc/<pid>/exe rather than trusting `agentop service status` or
# `systemctl is-active`: both report on the UNIT, which stays "active"
# whether systemd started a fresh process or simply never noticed the old one
# needed replacing.
assert_running_binary_is_current() {
	pid="$(systemctl --user show -p MainPID --value cortex.service)"
	if [ -z "${pid}" ] || [ "${pid}" = "0" ]; then
		echo "FAIL: cortex.service reports no MainPID" >&2
		exit 1
	fi
	exe="$(readlink "/proc/${pid}/exe" 2>/dev/null || true)"
	want="$(readlink -f "${PROXY_BIN}")"
	if [ "${exe}" != "${want}" ]; then
		echo "FAIL: cortex.service (pid ${pid}) runs '${exe}', not the upgraded ${want}" >&2
		exit 1
	fi
}

# NOT independent of the check above — this runs the binary sitting at
# PROXY_BIN on disk, not the service's own process, so on its own it would
# pass even in the exact #1203 stale-process bug this script exists to catch
# (the disk binary IS the new one in that state; only the running process
# is stale). It only says something about the SERVICE because the preceding
# assert_running_binary_is_current call already proved the service's
# /proc/<pid>/exe points at this same PROXY_BIN path. Call both, in this
# order, or this one is checking the wrong thing.
#
# What it adds beyond that: the exe-path check only proves "the service runs
# the file at this path"; this proves that file's own self-reported version
# is the one this run installed — which also ties a main-latest run to the
# exact commit that triggered it (see TRIGGER_SHA below), since a later merge
# can re-point main-latest and clobber its assets while this job still runs.
#
# The cpex build prints "cortex (cpex) <version>"; pass "(cpex) <version>" for it.
assert_running_version_is() {
	got="$("${PROXY_BIN}" --version)"
	if [ "${got}" != "cortex $1" ]; then
		echo "FAIL: ${PROXY_BIN} --version printed '${got}', want 'cortex $1'" >&2
		exit 1
	fi
}

# The session API's plugin catalog (the built-in config's :47601) lists every plugin
# the running binary registered, configured or not, so it tells the cpex build from
# the default one by what the service is running rather than by the file on disk.
# Retried briefly: the proxy can be up a moment before its session API answers.
plugins_list_cpex() {
	cat_out="$(mktemp "${TMP_DIR}/plugins.XXXXXX")"
	i=0
	until curl -fsS -o "${cat_out}" http://127.0.0.1:47601/v1/plugins; do
		i=$((i + 1))
		if [ "${i}" -ge 10 ]; then
			echo "FAIL: GET /v1/plugins on :47601 did not answer" >&2
			exit 1
		fi
		sleep 1
	done
	grep -q '"name": *"cpex"' "${cat_out}"
}

# release-binaries.yaml stamps a v* build with the tag itself, and a
# main-latest build with "main-<7-char sha>" of the commit that triggered it
# (ldflags -X main.version). Resolving the expected string here, once, rather
# than at each call site.
case "${TAG}" in
	main-latest)
		if [ -z "${TRIGGER_SHA}" ]; then
			echo "FAIL: main-latest run needs the triggering commit sha as \$2" >&2
			exit 1
		fi
		expected_version="main-$(printf '%s' "${TRIGGER_SHA}" | cut -c1-7)"
		;;
	*)
		expected_version="${TAG}"
		;;
esac

# The most recent STABLE release OLDER than the tag under test — specifically
# the IMMEDIATELY preceding one, not just any older release. That's the N-1 ->
# N transition every tag goes through, and the one most likely to contain a
# regression THIS release just introduced: N-2 -> N-1 was already exercised
# as a smoke test when N-1 itself shipped, so testing a wider jump re-covers
# already-checked ground instead of the actual diff under test. Not
# hardcoded: a fixed "known good" version would drift out of the release list
# over time and stop being the second-most-recent release, silently testing
# a narrower jump than intended.
#
# Compared by VERSION NUMBER, not by date — and not by any other timestamp
# either (a tag's own commit date has the identical problem below; this is
# categorical, not about which clock you pick). This repo keeps maintenance
# branches alive (release-0.6, release-0.7 both exist), so a patch tagged on
# an older line can be CREATED later in calendar time than a newer line's
# release — e.g. v0.7.1 tagged after v0.8.1 already exists. Any date-based
# "older" would then pick v0.8.1 as the "older" release for v0.7.1, an
# unlabeled downgrade: no timestamp, however it's obtained, carries the
# "which product line does this belong to" information that only the
# dotted version number preserves. Walking git ancestry instead of release
# metadata doesn't fix this either — a maintenance branch commit is often not
# even an ancestor of the newer line's commit in the DAG, so ancestry would
# answer "what's upstream of this commit" rather than "what's the previous
# version in this product line," a different (and here, wrong) question.
# Comparing version numbers directly sidesteps all of this: v0.7.1 sorts
# after v0.7.0 and before v0.8.0 regardless of when or where either was
# actually created.
#
# --limit 200: gh release list's --limit counts STABLE releases, not raw API
# results — it drops drafts and prereleases on the client and keeps paging
# until it collects that many (cli/cli's fetchReleases), so this is "the 200
# most recent stable releases," comfortably beyond any realistic release
# count for a long while. gh has no --paginate flag for this subcommand, so a
# fixed high limit stands in for real pagination on a problem this project
# will not hit for years.
#
# set -eu alone won't catch a failure inside a pipeline under dash (no
# pipefail), so the gh call is checked explicitly.
if ! all_stable="$(gh release list --exclude-drafts --exclude-pre-releases \
	--limit 200 --json tagName -q '.[].tagName')"; then
	echo "FAIL: gh release list failed" >&2
	exit 1
fi

case "${TAG}" in
	main-latest)
		# main-latest always means "whatever's on main right now", so the
		# single most recent stable release is unambiguously the one before
		# it. sort -V | tail -1 picks the highest VERSION, not whichever
		# release gh happened to create most recently — gh release list
		# orders by creation date, and a maintenance-branch patch can be
		# created after a newer line's release (the same v0.7.1-after-v0.8.1
		# case above), which would make a date-ordered "newest" pick wrong
		# here too.
		OLDER_TAG="$(printf '%s\n' "${all_stable}" | sort -V | tail -n 1)"
		;;
	*)
		# sort -V orders the full line "vX.Y.Z" the same way it would order
		# bare dotted numbers — the "v" prefix is common to every line, so it
		# never affects relative order. The release immediately before TAG in
		# that order is the one with the highest version strictly less than
		# it.
		#
		# TAG itself is injected into the sorted list (not just looked up in
		# it) because --exclude-pre-releases already dropped it if TAG is a
		# prerelease (an -rc/-alpha tag) — without this, $0 == want never
		# matches for any prerelease TAG, OLDER_TAG always comes out empty,
		# and the upgrade leg silently never runs for the common case this
		# repo actually tags with (v0.7.0 alone shipped 10 alphas and 3 RCs).
		# `--exclude-pre-releases` still has to stay on for fetching the POOL,
		# though — this injection only gives the lookup a landmark to match
		# against, it does not make prereleases eligible ANSWERS. We never
		# want OLDER_TAG itself to be a prerelease (nobody's real upgrade
		# baseline is an RC), and the algorithm can't accidentally pick TAG
		# as its own answer either way: the exact-match branch exits before
		# `prev` is updated to TAG's own line, and `sort -V -u` drops the
		# duplicate on the rare case TAG is also already a real stable
		# release in the list.
		#
		# The first `-` is mapped to `~` before sorting because plain sort -V
		# orders an RC AFTER its own GA (v0.9.0-rc.2 sorts after v0.9.0), so
		# looking up v0.9.0-rc.2 unmapped would resolve to v0.9.0 once v0.9.0
		# ships — an unlabeled downgrade in the other direction from the one
		# above. GNU sort -V orders `~` before end-of-string, so the mapped
		# `v0.9.0~rc.2` correctly sorts before `v0.9.0`; every stable tag
		# today is a plain `vX.Y.Z` with no `-`, so the substitution is a
		# no-op for them. Not an invented trick: this is the same
		# `~`-sorts-first convention Debian's dpkg has used for prerelease
		# version comparison for years, and GNU sort -V deliberately
		# replicates it — reused here for the thing it was designed for, not
		# repurposed. It does assume every tag is plain `vX.Y.Z` or
		# `vX.Y.Z-suffix` with the hyphen only ever marking a prerelease,
		# which is true of every tag this project has cut so far and is this
		# project's own naming convention to keep true.
		want="$(printf '%s' "${TAG}" | sed 's/-/~/')"
		OLDER_TAG="$( { printf '%s\n' "${all_stable}"; printf '%s\n' "${want}"; } | sort -V -u | awk -v want="${want}" '
			$0 == want { print prev; exit }
			{ prev = $0 }
		')"
		;;
esac

log "Testing ${TAG} (upgrading from: ${OLDER_TAG:-none found; first release})"

# No compatibility path for a pre-rename INSTALL_TAG (e.g. v0.8.0's own
# "older" release resolving to the pre-rename v0.7.0): matches this repo's
# own clean-break policy for the abctl->agentop rename — "There is no abctl
# alias" and "No compatibility code for on-disk state either" (see
# docs/superpowers/specs/2026-09-30-abctl-to-agentop-rename-design.md). Not
# actually reachable in practice: the workflow's checkout is pinned to TAG's
# own commit, and TAG=v0.8.0 predates this script — a run testing v0.8.0
# fails at "sh: 0: cannot open scripts/release_smoke_test_linux.sh: No such
# file" (dash's exact wording, checked under ubuntu:24.04) before any of
# this executes. Kept rather than deleted: a future release old enough to
# have this script but still pre-rename would reach here for real.
INSTALL_TAG="${OLDER_TAG:-${TAG}}"
log "Fresh install: ${INSTALL_TAG}"
install_cortex "${INSTALL_TAG}"
assert_healthy
assert_running_binary_is_current
# INSTALL_TAG is OLDER_TAG when it's set — always a real v* tag, never
# main-latest, so its own version string IS the tag; expected_version (which
# resolves main-latest's main-<sha> form) only applies when INSTALL_TAG == TAG.
assert_running_version_is "${OLDER_TAG:-${expected_version}}"

if [ -n "${OLDER_TAG}" ]; then
	# A marker only this test writes, to prove config survives the upgrade
	# untouched (beyond migrateConfig's own additive listener pins).
	marker="smoke-test-marker-${TAG}"
	printf '# %s\n' "${marker}" >>"${CFG}"

	log "Upgrade: ${INSTALL_TAG} -> ${TAG}"
	install_cortex "${TAG}"
	assert_contains "${CFG}" "${marker}" "config marker did not survive the upgrade"
	assert_healthy
	assert_running_binary_is_current
	assert_running_version_is "${expected_version}"
fi

log "No-op re-run: ${TAG}"
# Redirected, not piped through tee: install_cortex now returns on failure
# instead of exiting, so its own stderr lands in reinstall_out for the
# diagnostic cat below rather than needing a pipe at all.
reinstall_out="$(mktemp "${TMP_DIR}/reinstall.XXXXXX")"
if ! install_cortex "${TAG}" >"${reinstall_out}" 2>&1; then
	echo "FAIL: re-running install failed" >&2
	cat "${reinstall_out}" >&2
	exit 1
fi
cat "${reinstall_out}"
# install.sh hands off to `agentop setup`, which plans every step first. With nothing
# to change it applies nothing and ends on its short-circuit line, "cortex <version>
# is installed and healthy." (runSetup in cmd/agentop/cmd_setup.go), where a run that
# changed something ends on "cortex <version> ready." instead. So this line is the
# no-op, and naming the version also ties it to the build under test.
assert_contains "${reinstall_out}" "cortex ${expected_version} is installed and healthy\." \
	"re-running install was not a no-op"

# The cpex build is TAG's own variant, so there is no older one to upgrade from.
log "Switch to the cpex build: ${TAG} --cpex"
install_cortex "${TAG}" --cpex
if [ -n "${OLDER_TAG}" ]; then
	assert_contains "${CFG}" "${marker}" "config did not survive the switch to the cpex build"
fi
assert_healthy
assert_running_binary_is_current
assert_running_version_is "(cpex) ${expected_version}"
if ! plugins_list_cpex; then
	echo "FAIL: /v1/plugins does not list cpex after installing the cpex build" >&2
	exit 1
fi

log "No-op re-run: ${TAG} --cpex"
cpex_reinstall_out="$(mktemp "${TMP_DIR}/reinstall-cpex.XXXXXX")"
if ! install_cortex "${TAG}" --cpex >"${cpex_reinstall_out}" 2>&1; then
	echo "FAIL: re-running install --cpex failed" >&2
	cat "${cpex_reinstall_out}" >&2
	exit 1
fi
cat "${cpex_reinstall_out}"
assert_contains "${cpex_reinstall_out}" "cortex ${expected_version} is installed and healthy\." \
	"re-running install --cpex was not a no-op"

log "Switch back to the default build: ${TAG}"
switch_out="$(mktemp "${TMP_DIR}/switch-back.XXXXXX")"
if ! install_cortex "${TAG}" >"${switch_out}" 2>&1; then
	echo "FAIL: switching back to the default build failed" >&2
	cat "${switch_out}" >&2
	exit 1
fi
cat "${switch_out}"
assert_contains "${switch_out}" "Replacing the installed cpex build of cortex with the default one" \
	"switching back printed no notice"
assert_healthy
assert_running_binary_is_current
assert_running_version_is "${expected_version}"
if plugins_list_cpex; then
	echo "FAIL: /v1/plugins still lists cpex after switching back" >&2
	exit 1
fi

log "Uninstall"
# Existence alone survives truncation or a rewrite — the promise being tested
# is that the file is left UNTOUCHED, so a checksum from just before uninstall
# is what actually proves that, not just that something is still there
# afterward.
cfg_before="$(cksum <"${CFG}")"
uninstall_out="$(mktemp "${TMP_DIR}/uninstall.XXXXXX")"
"${AGENTOP}" service uninstall --yes | tee "${uninstall_out}"
assert_contains "${uninstall_out}" "Removed" "uninstall did not report success"
if [ -f "${UNIT}" ]; then
	echo "FAIL: unit file still present after uninstall: ${UNIT}" >&2
	exit 1
fi
if [ ! -f "${CFG}" ]; then
	echo "FAIL: uninstall removed ${CFG}; it promises to leave config untouched" >&2
	exit 1
fi
cfg_after="$(cksum <"${CFG}")"
if [ "${cfg_before}" != "${cfg_after}" ]; then
	echo "FAIL: uninstall changed the contents of ${CFG}; it promises to leave it untouched" >&2
	exit 1
fi
# None of the checks above actually prove the proxy stopped. removeService
# (cmd_service.go) only LOGS a failed `systemctl --user disable --now`, then
# deletes the unit file regardless and still prints "Removed" — so all three
# checks above would pass even if the old proxy kept serving, now with no
# unit file left to manage or diagnose it. is-active asks systemd directly,
# rather than inferring from the unit file's absence or a printed message.
if systemctl --user is-active --quiet cortex.service; then
	echo "FAIL: cortex.service is still active after uninstall" >&2
	exit 1
fi

log "Linux release smoke test passed for ${TAG}"
