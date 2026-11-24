#!/bin/sh
# release_smoke_test_macos.sh — the macOS half of the release smoke test (#956).
#
# The same journey as release_smoke_test_linux.sh, against a real published
# release, installed exactly as a user does (curl | sh): a fresh install of the
# previous stable release, an upgrade to the release under test (config kept, and
# the NEW binary serving), one request through Cortex with its tokens recorded
# (smoke_inference_check.sh), a no-op re-run, and a clean uninstall. Read the Linux
# script for the reasoning shared by both, chiefly how the previous release is
# chosen; this one explains only where macOS differs.
#
# Where macOS differs:
#   - launchd, not systemd. Cortex is the launch agent io.rossoctl.cortex in the
#     logged-in user's desktop session (gui/<uid>). GitHub's macOS runners log their
#     user in automatically, so that session exists and nothing needs setting up
#     first, where the Linux runner needs lingering turned on.
#   - The job launchd runs is Cortex's supervisor (`cortex --supervise`), and the
#     proxy is its child.
#   - No /proc. Which binary a process runs is proven from times instead (see
#     assert_running_binary_is_current).
#   - BSD tools: grep -E for alternation, date -j to read ps start times.
#
# Usage: release_smoke_test_macos.sh <tag-under-test> [triggering-commit-sha]
set -eu

TAG="${1:?usage: release_smoke_test_macos.sh <tag-under-test> [sha]}"
TRIGGER_SHA="${2:-}"
HERE=$(cd "$(dirname "$0")" && pwd)
AGENTOP="${HOME}/.local/bin/agentop"
PROXY_BIN="${HOME}/.local/bin/cortex"
CFG="${HOME}/.cortex/config.yaml"
UNIT="${HOME}/Library/LaunchAgents/io.rossoctl.cortex.plist"
JOB="gui/$(id -u)/io.rossoctl.cortex"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

# As in the Linux script: the installer is read from a temp file on stdin, so $0
# stays "sh" and install.sh behaves as it does under a real `curl | sh`.
install_cortex() {
	ref="$1"
	shift
	tmp="$(mktemp "${TMP_DIR}/install.XXXXXX")"
	installer_url="https://raw.githubusercontent.com/rossoctl/cortex/main/scripts/install.sh"
	if [ "${ref}" = "main-latest" ]; then
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
	# assert_contains <file> <extended regex> <description>
	if ! grep -E -q -- "$2" "$1"; then
		echo "FAIL: $3" >&2
		echo "--- $1 ---" >&2
		cat "$1" >&2
		exit 1
	fi
}

# Both wordings: the fresh install runs the PREVIOUS release's agentop.
assert_healthy() {
	out="$(mktemp "${TMP_DIR}/status.XXXXXX")"
	if ! "${AGENTOP}" service status >"${out}" 2>&1; then
		cat "${out}" >&2
		echo "FAIL: agentop service status exited non-zero" >&2
		exit 1
	fi
	cat "${out}"
	assert_contains "${out}" 'healthy:|Cortex is healthy according to' "agentop service status did not report healthy"
}

job_pid() { launchctl print "${JOB}" 2>/dev/null | awk '/^\tpid =/{print $3}'; }

# A pid's start time, in epoch seconds. ps pads a single-digit day with a space.
started_at() {
	date -j -f "%a %b %d %T %Y" "$(ps -o lstart= -p "$1" | tr -s ' ' | sed 's/^ //; s/ $//')" +%s
}

# The Linux script's check that the service runs the binary just installed, not a
# stale process left from before the upgrade (#1203). There it compares
# /proc/<pid>/exe. macOS has no /proc, so it compares times: exec reads the file
# when a process starts, so a process that started at or after the binary on disk
# last changed is running that binary, and a stale one started before it. ctime,
# not mtime, because unpacking a tarball can set mtime to the build's date, and
# nothing can set ctime. Both processes count: the job launchd runs, which is the
# supervisor, and the proxy under it, which is what serves.
assert_running_binary_is_current() {
	sup="$(job_pid)"
	if [ -z "${sup}" ]; then
		echo "FAIL: launchd reports no running ${JOB}" >&2
		launchctl print "${JOB}" >&2 2>/dev/null || true
		exit 1
	fi
	# By name, not "the first child": in praxis mode the supervisor also runs
	# cortex-gateway, which -x leaves out. Exactly one, or the check below could
	# compare the wrong process and pass.
	proxy="$(pgrep -x -P "${sup}" cortex || true)"
	if [ -z "${proxy}" ]; then
		echo "FAIL: the supervisor (pid ${sup}) has no cortex proxy under it" >&2
		ps -o pid=,command= -p "$(pgrep -P "${sup}" | paste -s -d, -)" >&2 2>/dev/null || true
		exit 1
	fi
	if [ "$(printf '%s\n' "${proxy}" | wc -l)" -ne 1 ]; then
		echo "FAIL: the supervisor (pid ${sup}) has more than one cortex child: $(printf '%s' "${proxy}" | tr '\n' ' ')" >&2
		exit 1
	fi
	changed="$(stat -f %c "${PROXY_BIN}")"
	for pid in "${sup}" "${proxy}"; do
		if [ "$(started_at "${pid}")" -lt "${changed}" ]; then
			echo "FAIL: pid ${pid} ($(ps -o command= -p "${pid}")) started before ${PROXY_BIN} last changed, so it runs an older binary" >&2
			exit 1
		fi
	done
}

assert_running_version_is() {
	got="$("${PROXY_BIN}" --version)"
	if [ "${got}" != "cortex $1" ]; then
		echo "FAIL: ${PROXY_BIN} --version printed '${got}', want 'cortex $1'" >&2
		exit 1
	fi
}

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

# The previous stable release, chosen exactly as release_smoke_test_linux.sh does
# and for the reasons it gives: by version number, with TAG injected as a landmark
# and its first `-` mapped to `~` so a release candidate sorts before its release.
# macOS's sort -V orders `~` the same way GNU's does.
if ! all_stable="$(gh release list --exclude-drafts --exclude-pre-releases \
	--limit 200 --json tagName -q '.[].tagName')"; then
	echo "FAIL: gh release list failed" >&2
	exit 1
fi
case "${TAG}" in
	main-latest)
		OLDER_TAG="$(printf '%s\n' "${all_stable}" | sort -V | tail -n 1)"
		;;
	*)
		want="$(printf '%s' "${TAG}" | sed 's/-/~/')"
		OLDER_TAG="$( { printf '%s\n' "${all_stable}"; printf '%s\n' "${want}"; } | sort -V -u | awk -v want="${want}" '
			$0 == want { print prev; exit }
			{ prev = $0 }
		')"
		;;
esac

log "Testing ${TAG} on macOS $(sw_vers -productVersion) $(uname -m) (upgrading from: ${OLDER_TAG:-none found; first release})"

INSTALL_TAG="${OLDER_TAG:-${TAG}}"
log "Fresh install: ${INSTALL_TAG}"
install_cortex "${INSTALL_TAG}"
assert_healthy
assert_running_binary_is_current
assert_running_version_is "${OLDER_TAG:-${expected_version}}"

if [ -n "${OLDER_TAG}" ]; then
	marker="smoke-test-marker-${TAG}"
	printf '# %s\n' "${marker}" >>"${CFG}"

	# assert_running_binary_is_current compares whole seconds, so a stale process
	# that started in the same second as the binary changed would pass. The download
	# in between makes that all but impossible; this makes it impossible.
	sleep 1
	log "Upgrade: ${INSTALL_TAG} -> ${TAG}"
	install_cortex "${TAG}"
	assert_contains "${CFG}" "${marker}" "config marker did not survive the upgrade"
	assert_healthy
	assert_running_binary_is_current
	assert_running_version_is "${expected_version}"
fi

log "A request through Cortex: ${TAG}"
sh "${HERE}/smoke_inference_check.sh"

log "No-op re-run: ${TAG}"
reinstall_out="$(mktemp "${TMP_DIR}/reinstall.XXXXXX")"
if ! install_cortex "${TAG}" >"${reinstall_out}" 2>&1; then
	echo "FAIL: re-running install failed" >&2
	cat "${reinstall_out}" >&2
	exit 1
fi
cat "${reinstall_out}"
assert_contains "${reinstall_out}" "cortex ${expected_version} is installed and healthy\." \
	"re-running install was not a no-op"

log "Uninstall"
cfg_before="$(cksum <"${CFG}")"
uninstall_out="$(mktemp "${TMP_DIR}/uninstall.XXXXXX")"
# Redirected, not piped through tee: under set -e a pipeline's status is tee's,
# so a failing uninstall would go on to the checks below instead of failing here.
if ! "${AGENTOP}" service uninstall --yes >"${uninstall_out}" 2>&1; then
	echo "FAIL: agentop service uninstall exited non-zero" >&2
	cat "${uninstall_out}" >&2
	exit 1
fi
cat "${uninstall_out}"
assert_contains "${uninstall_out}" "Removed" "uninstall did not report success"
if [ -f "${UNIT}" ]; then
	echo "FAIL: plist still present after uninstall: ${UNIT}" >&2
	exit 1
fi
if [ ! -f "${CFG}" ]; then
	echo "FAIL: uninstall removed ${CFG}; it promises to leave config untouched" >&2
	exit 1
fi
if [ "${cfg_before}" != "$(cksum <"${CFG}")" ]; then
	echo "FAIL: uninstall changed the contents of ${CFG}; it promises to leave it untouched" >&2
	exit 1
fi
# As on Linux, none of that proves the proxy stopped, so ask launchd. Polled:
# `launchctl bootout` returns before teardown finishes, and an agentop from before
# #2031 did not wait for it.
gone=""
for _ in $(seq 1 30); do
	launchctl print "${JOB}" >/dev/null 2>&1 || { gone=yes; break; }
	sleep 1
done
if [ -z "${gone}" ]; then
	echo "FAIL: launchd still has ${JOB} loaded 30s after uninstall" >&2
	exit 1
fi

log "macOS release smoke test passed for ${TAG}"
