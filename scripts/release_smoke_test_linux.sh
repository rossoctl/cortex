#!/bin/sh
# release_smoke_test_linux.sh — the Linux half of the release smoke test (#957).
#
# Exercises install.sh (and the agentop setup it hands off to) + agentop service
# exactly as a real user would, against
# a real published release: fresh install, upgrade over an existing install
# (config preserved, and the NEW binary actually serving), a no-op re-run, and
# a clean uninstall. Run against a real tag/release, not a local build — there
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
install_cortex() {
	tmp="$(mktemp "${TMP_DIR}/install.XXXXXX")"
	if ! curl -fsSL -o "${tmp}" https://raw.githubusercontent.com/rossoctl/cortex/main/scripts/install.sh; then
		echo "FAIL: could not download install.sh" >&2
		return 1
	fi
	if [ ! -s "${tmp}" ]; then
		echo "FAIL: downloaded install.sh is empty" >&2
		return 1
	fi
	sh -s -- --ref="$1" --yes <"${tmp}"
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

# Both wordings: the fresh install below runs the PREVIOUS release's agentop, which
# says "healthy: <url>", where this one says "Cortex is healthy according to <url>".
# GNU grep's \| alternation; this runs on ubuntu.
assert_healthy() {
	out="$(mktemp "${TMP_DIR}/status.XXXXXX")"
	"${AGENTOP}" service status | tee "${out}"
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

# Confirms the running binary's own reported version matches what this run
# installed — a second, independent signal alongside the inode check above,
# and the one that also ties a main-latest run to the exact commit that
# triggered it (see TRIGGER_SHA below): a later merge can re-point main-latest
# and clobber its assets while this job is still running, so without this a
# pass could be describing the NEXT build rather than the one actually under
# test.
assert_running_version_is() {
	got="$("${PROXY_BIN}" --version)"
	if [ "${got}" != "cortex $1" ]; then
		echo "FAIL: ${PROXY_BIN} --version printed '${got}', want 'cortex $1'" >&2
		exit 1
	fi
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

# The most recent STABLE release created strictly BEFORE the tag under test —
# the realistic "what a user who hasn't upgraded in a while" starting point.
# Not hardcoded: a fixed "known good" version would drift out of the release
# list over time and stop being the second-most-recent release, silently
# testing a narrower jump than intended.
#
# "Before TAG", not just "the most recent other release": this workflow can be
# triggered by re-running an OLD completed Release Binaries run, at which
# point newer stable releases may already exist. Picking "the most recent
# other release" in that case would select something NEWER than TAG, silently
# turning the "upgrade" step into an unlabeled downgrade — and for a release
# old enough (pre-rename artifact names, no install.sh at any path it
# probes), the fresh install of that "older" release would fail outright.
#
# createdAt, not publishedAt: release-binaries.yaml PATCHes main-latest's own
# tag ref to the triggering commit on every push to main, and a release's
# createdAt follows the commit its tag points at — the lightweight
# main-latest tag's commit date for that one, an annotated v* tag's tagger
# date for the rest. publishedAt stays frozen at whenever the release object
# was first created and does not move with it, confirmed live across two
# different days (main-latest's createdAt advanced from 2026-09-30T13:43:16Z
# to 2026-10-01T15:37:57Z as main kept moving; publishedAt stayed at
# 2026-09-09T21:11:29Z throughout). Anchoring on publishedAt would permanently
# exclude every stable release published after that first day from ever being
# picked for a main-latest run.
#
# set -eu alone won't catch a failure inside a pipeline under dash (no
# pipefail), so each gh call below is checked explicitly rather than trusted
# to abort the script on its own.
if ! tag_created_at="$(gh release view "${TAG}" --json createdAt -q '.createdAt')"; then
	echo "FAIL: could not resolve the creation date for release ${TAG}" >&2
	exit 1
fi

if ! OLDER_TAG="$(gh release list --exclude-drafts --exclude-pre-releases --limit 20 \
	--json tagName,createdAt \
	-q "[.[] | select(.createdAt < \"${tag_created_at}\")] | sort_by(.createdAt) | last | .tagName // \"\"")"; then
	echo "FAIL: gh release list failed" >&2
	exit 1
fi

log "Testing ${TAG} (upgrading from: ${OLDER_TAG:-none found; first release})"

# No compatibility path for a pre-rename INSTALL_TAG (e.g. re-testing v0.8.0
# itself, whose own "older" release resolves to the pre-rename v0.7.0):
# matches this repo's own clean-break policy for the abctl->agentop rename
# ("no alias, no compatibility code" — see
# docs/superpowers/specs/2026-09-30-abctl-to-agentop-rename-design.md). If
# INSTALL_TAG predates the rename, this fails here with a plain
# "command not found" rather than a clearer message — accepted, since it only
# affects re-testing that one already-superseded release, a cost that stops
# being reachable at all once a newer stable release exists to upgrade from.
INSTALL_TAG="${OLDER_TAG:-${TAG}}"
log "Fresh install: ${INSTALL_TAG}"
install_cortex "${INSTALL_TAG}"
assert_healthy

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

log "Linux release smoke test passed for ${TAG}"
