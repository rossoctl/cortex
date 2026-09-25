#!/bin/sh
# Tests for authbridge/install.sh.
#
# install.sh is a curl|sh entry point, so its failure modes are other people's
# first experience of Cortex. Until this file existed the only automated check was
# `shellcheck --severity=error`, which is why the tag-parser bug (returning the
# OLDEST release from compact JSON) shipped in #902 without a test.
#
# Approach: source a single function out of install.sh with `curl` replaced by one
# that prints a fixture. No network, no GitHub, no downloads. Plain POSIX sh
# because the repo has no bats or shunit2 and a framework is not worth it for a
# handful of functions.
#
# Run: sh authbridge/install_test.sh
set -eu

# shellcheck disable=SC1007 # `CDPATH= cd` is deliberate, not a typo: it empties
# CDPATH for this one command. The documented invocation is `sh
# authbridge/install_test.sh`, so $0 is RELATIVE — with a CDPATH set, `cd` can
# resolve it against a CDPATH entry and land somewhere else entirely.
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
INSTALL_SH="${SCRIPT_DIR}/install.sh"
[ -f "${INSTALL_SH}" ] || { printf 'cannot find %s\n' "${INSTALL_SH}" >&2; exit 1; }

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

check_fails() { # label status
	if [ "$2" != "0" ]; then
		PASS=$((PASS + 1))
		printf '  ok   %s (exit %s)\n' "$1" "$2"
	else
		FAIL=$((FAIL + 1))
		printf '  FAIL %s: expected non-zero exit, got 0\n' "$1"
	fi
}

fixture() { # name; body on stdin. Sets $FIXTURE to the path.
	FIXTURE="${TMP}/$1"
	cat >"${FIXTURE}"
}

# emit_curl_stub prints a `curl` replacement that dispatches on the URL.
#
# One definition, used by every probe. newest_release() has two sources now, and a stub
# serving one body to both could not tell them apart: the feed fallback would look
# exercised while never running. Writing this twice is the drift that has already cost
# this file two vacuous passes.
# shellcheck disable=SC2016 # every expression below is literal on purpose: it is
# expanded by the generated probe, not here. One directive for the whole function beats
# five identical ones inline.
emit_curl_stub() { # api-fixture feed-fixture api-http-code
	printf 'curl() {\n'
	printf '  _u=""; for _a in "$@"; do case "$_a" in http*) _u="$_a" ;; esac; done\n'
	printf '  _o=""; _p=""; for _a in "$@"; do [ "${_p}" = "-o" ] && _o="$_a"; _p="$_a"; done\n'
	printf '  case "${_u}" in\n'
	printf '    *api.github.com*)\n'
	printf '      [ -z "${_o}" ] || cat "%s" > "${_o}"\n' "$1"
	printf '      [ -n "${_o}" ] || cat "%s"\n' "$1"
	printf '      printf "%%s" "%s"\n' "$3"
	printf '      [ "%s" = "200" ] || return 22\n' "$3"
	printf '      ;;\n'
	printf '    *releases.atom*) cat "%s" ;;\n' "$2"
	printf '  esac\n'
	printf '}\n'
}

# with_newest_release runs install.sh's newest_release() against a fixture.
#
# The function is extracted by line range rather than sourcing install.sh, because
# sourcing would run the whole installer. `curl` is replaced by a function so the
# extracted code is unmodified — testing what ships, not a copy of it.
with_newest_release() { # api-fixture-path [feed-fixture-path] [api-http-code]
	_f=$1
	_feed=${2:-/dev/null}
	_code=${3:-200}
	{
		printf 'REPO=rossoctl/cortex\n'
		printf 'warn() { printf "warning: %%s\\n" "$*" >&2; }\n'
		emit_curl_stub "${_f}" "${_feed}" "${_code}"
		sed -n '/^newest_release()/,/^}/p' "${INSTALL_SH}"
		sed -n '/^release_tag_from_api()/,/^}/p' "${INSTALL_SH}"
		sed -n '/^release_tag_from_feed()/,/^}/p' "${INSTALL_SH}"
		printf 'newest_release\n'
	} >"${TMP}/probe.sh"
	sh "${TMP}/probe.sh" 2>/dev/null
}

printf 'install.sh tests\n'

# --- newest_release: the shape it is documented to handle ---

fixture pretty.json <<'EOF'
[
  {
    "tag_name": "v0.7.0-alpha.7",
    "name": "v0.7.0-alpha.7"
  },
  {
    "tag_name": "v0.3.1"
  }
]
EOF
check "pretty JSON resolves the newest tag" "v0.7.0-alpha.7" "$(with_newest_release "${FIXTURE}")"

fixture compact.json <<'EOF'
[{"tag_name":"v0.7.0-alpha.7"},{"tag_name":"v0.7.0-alpha.6"},{"tag_name":"v0.3.1"}]
EOF
check "compact JSON resolves the newest tag, not the oldest" "v0.7.0-alpha.7" "$(with_newest_release "${FIXTURE}")"

# --- newest_release: the main channel must not hijack the default ---
#
# The rolling `main` release sorts first until the next tagged release, because the
# API sorts by created_at and created_at is fixed at creation. Unfiltered, the
# v[0-9]* shape check then rejects it and the whole default install dies.

fixture main_first.json <<'EOF'
[
  {
    "tag_name": "main",
    "prerelease": true
  },
  {
    "tag_name": "v0.7.0-alpha.7"
  }
]
EOF
check "a main release sorting first is skipped" "v0.7.0-alpha.7" "$(with_newest_release "${FIXTURE}")"

fixture main_first_compact.json <<'EOF'
[{"tag_name":"main"},{"tag_name":"v0.7.0-alpha.7"},{"tag_name":"v0.3.1"}]
EOF
check "main skipped in compact JSON too" "v0.7.0-alpha.7" "$(with_newest_release "${FIXTURE}")"

fixture only_non_v.json <<'EOF'
[{"tag_name":"main"},{"tag_name":"nightly"}]
EOF
set +e
_out=$(with_newest_release "${FIXTURE}"); _st=$?
set -e
check_fails "a page with no v-tag returns non-zero rather than guessing" "${_st}"
check "and prints nothing on stdout" "" "${_out}"

# --- newest_release: hostile inputs still fail closed ---

fixture ratelimit.json <<'EOF'
{"message":"API rate limit exceeded","documentation_url":"https://x"}
EOF
set +e
_out=$(with_newest_release "${FIXTURE}"); _st=$?
set -e
check_fails "rate-limit body fails" "${_st}"

fixture html.json <<'EOF'
<html><body>502 Bad Gateway</body></html>
EOF
set +e
_st=0; _out=$(with_newest_release "${FIXTURE}") || _st=$?
set -e
check_fails "an HTML error page fails" "${_st}"

fixture empty.json </dev/null
set +e
_st=0; _out=$(with_newest_release "${FIXTURE}") || _st=$?
set -e
check_fails "an empty body fails" "${_st}"

# --- resolve_version: --ref=X selects binaries too ---
#
# --ref was inconsistent: `--ref=v0.7.0-alpha.4` set script AND binaries, while
# `--ref=main` set only the script, because there was no main release to download
# from. One rule now covers both.

with_resolve_version() { # version_ref fixture-path
	_ref=$1; _f=$2
	{
		printf 'REPO=rossoctl/cortex\n'
		# CHANNEL_TAG is read out of install.sh, not restated here, so this probe
		# cannot disagree with the script about what the channel is called.
		sed -n '/^CHANNEL_TAG=/p' "${INSTALL_SH}"
		printf 'warn() { printf "warning: %%s\\n" "$*" >&2; }\n'
		printf 'die() { printf "error: %%s\\n" "$*" >&2; exit 1; }\n'
		# info() is stubbed to match install.sh's definition EXACTLY — stdout, not
		# stderr. Stubbing it silent (`info() { :; }`) would leave the
		# "stdout carries no progress text" case below unable to fail, which is the
		# one thing it exists to catch.
		printf 'info() { printf "%%s\\n" "$*"; }\n'
		emit_curl_stub "${_f}" /dev/null 200
		sed -n '/^newest_release()/,/^}/p' "${INSTALL_SH}"
		sed -n '/^release_tag_from_api()/,/^}/p' "${INSTALL_SH}"
		sed -n '/^release_tag_from_feed()/,/^}/p' "${INSTALL_SH}"
		sed -n '/^resolve_version()/,/^}/p' "${INSTALL_SH}"
		printf 'resolve_version "%s"\n' "${_ref}"
	} >"${TMP}/rv.sh"
	# stderr goes to a file rather than being discarded or leaked: two cases below assert
	# on the warnings it carries, and letting it reach the terminal would scatter
	# "Resolving newest release..." through the suite's own output.
	sh "${TMP}/rv.sh" 2>"${TMP}/rv.err"
}

# with_resolve_version_stderr is the same probe, returning stderr instead of stdout —
# the warnings are the behaviour under test here, and they must not reach stdout because
# stdout IS the resolved version.
with_resolve_version_stderr() { # version_ref fixture-path
	with_resolve_version "$1" "$2" >/dev/null
	cat "${TMP}/rv.err"
}

fixture rv_releases.json <<'EOF'
[{"tag_name":"main"},{"tag_name":"v0.7.0-alpha.7"}]
EOF
# --ref=main resolves to the channel tag, not the literal string "main": a
# release tagged `main` would collide with the branch. See CHANNEL_TAG in
# install.sh. The harness must pick the constant up from the script rather than
# hardcode it, or renaming the channel silently passes a stale test.
CHANNEL_TAG=$(sed -n 's/^CHANNEL_TAG="\(.*\)"$/\1/p' "${INSTALL_SH}")
check "install.sh defines a non-colliding CHANNEL_TAG" "1" "$(printf '%s' "${CHANNEL_TAG}" | grep -c '^main-' || true)"
check "--ref=main installs the channel tag" "${CHANNEL_TAG}" "$(with_resolve_version main "${FIXTURE}")"

# Both channel spellings resolve identically — CHANNEL_TAG is the title on the Releases
# page, so it is what someone types after seeing it there.
check "--ref=CHANNEL_TAG resolves the same" "${CHANNEL_TAG}" "$(with_resolve_version "${CHANNEL_TAG}" "${FIXTURE}")"

check "--ref=v0.7.0-alpha.4 installs that release" "v0.7.0-alpha.4" "$(with_resolve_version v0.7.0-alpha.4 "${FIXTURE}")"

# An empty VERSION_REF means "nobody named anything installable" and must resolve the
# newest RELEASE. It must never fall through to the channel: that is what the bootstrap
# passes when the API was unreachable, and the point is that a rate-limited plain
# one-liner does not silently receive an unreleased build. Asserted here at the function
# level and again against the real bootstrap block further down.
check "empty version ref resolves a RELEASE, never the channel" "v0.7.0-alpha.7" "$(with_resolve_version "" "${FIXTURE}")"

# A branch or a SHA has no published binaries, so the newest release is the only option
# — but it must SAY so, or it is indistinguishable from the plain one-liner and someone
# testing a branch gets a mixed set with nothing to attribute it to.
_st=0; _err=$(with_resolve_version_stderr feat/my-branch "${FIXTURE}") || _st=$?
check "a branch ref warns that binaries come from a release" "1" "$(printf '%s\n' "${_err}" | grep -c 'no binaries are published for feat/my-branch')"
check "an empty ref warns nothing" "0" "$(printf '%s\n' "$(with_resolve_version_stderr "" "${FIXTURE}")" | grep -c 'no binaries are published')"

# stdout must be the version and nothing else. info() writes to stdout
# (install.sh:81), so an un-redirected progress line inside resolve_version would be
# captured into $version and corrupt every download URL — a one-line mistake that
# breaks every install and no other test would catch.
# set +e around the capture: under `set -e` a bare assignment from a failing command
# substitution aborts the whole suite, so a regression that broke resolve_version
# would kill the run at this line instead of reporting a FAIL and carrying on.
set +e
_out=$(with_resolve_version "" "${FIXTURE}")
set -e
check "stdout carries no progress text" "1" "$(printf '%s\n' "${_out}" | wc -l | tr -d ' ')"
check "stdout is exactly the version" "v0.7.0-alpha.7" "${_out}"

# --- removed knobs leave no stale advice ---
#
# The failure mode is not "the var still works" — it is a message telling someone
# to set a var the script no longer reads. Three sites advised AUTHBRIDGE_VERSION.

# Each removed var is now mentioned on exactly ONE line, and that line rejects it. The
# earlier "zero occurrences" assertion was the wrong shape: ignoring a var someone still
# has set is a silent wrong answer, which is what this script's style exists to avoid, so
# a guard that dies is correct and has to be allowed to mention the name.
for _v in AUTHBRIDGE_VERSION AUTHBRIDGE_REF AUTHBRIDGE_INSTALL_ONLY; do
	_guard=$(grep -c "^\[ -z \"\${${_v}:-}\" \] || die" "${INSTALL_SH}" || true)
	check "${_v} has a guard that dies" "1" "${_guard}"
	# Expanded ONLY on the guard line. A line count would fail on prose that merely
	# names the var, which comments legitimately do; what matters is that nothing else
	# reads it to decide anything.
	_elsewhere=$(grep -v "^\[ -z \"\${${_v}:-}\" \] || die" "${INSTALL_SH}" | grep -c "\${${_v}" || true)
	check "${_v} is not expanded anywhere else" "0" "${_elsewhere}"
done

for _v in AUTHBRIDGE_SKIP_DOWNLOAD AUTHBRIDGE_SCRIPT_REF; do
	_hits=$(grep -c "${_v}" "${INSTALL_SH}" || true)
	check_fails "${_v} is still present" "${_hits}"
done


# --- bootstrap: which script runs vs which binaries get installed ---
#
# These two questions have different answers on every fallback, and conflating them let
# the DEFAULT one-liner install channel binaries. The resolve_version cases above cannot
# catch that: in isolation main -> CHANNEL_TAG is correct. The bug was in the bootstrap
# deciding to pass "main" at all. So extract the bootstrap block itself and assert the
# pair it produces.
with_bootstrap() { # want_ref http_code_for_script_fetch newest_release_output [run_as=pipe|file]
	_want=$1; _http=$2; _newest=$3; _runas=${4:-pipe}
	_start=$(awk '/^SCRIPT_REF="\$\{AUTHBRIDGE_SCRIPT_REF:-\}"/{print NR; exit}' "${INSTALL_SH}")
	_end=$(awk -v s="${_start}" 'NR>=s && /^fi$/{print NR; exit}' "${INSTALL_SH}")
	{
		printf 'REPO=rossoctl/cortex\n'
		sed -n '/^CHANNEL_TAG=/p' "${INSTALL_SH}"
		printf 'info() { :; }\nwarn() { :; }\ndie() { printf "DIED\\n"; exit 1; }\n'
		# newest_release: empty output + non-zero mimics an unreachable/rate-limited API.
		printf 'newest_release() { [ -n "%s" ] || return 1; printf "%%s\\n" "%s"; }\n' "${_newest}" "${_newest}"
		# curl here is only the raw.githubusercontent fetch of the released script; -w
		# makes the real one print the status code, so the stub prints the scenario's.
		#
		# It must also WRITE the file, because the real curl does (-o "${boot}") and the
		# branch under test is `[ "${http}" = "200" ] && [ -s "${boot}" ]`. A stub that
		# only echoed the code left ${boot} empty, so the 200 arm was unreachable and
		# every scenario silently fell through to the failure handling below — including
		# the one asserting that a transport failure refuses to run main.
		printf 'curl() {\n'
		printf '  _out=""; _prev=""\n'
		# shellcheck disable=SC2016 # literal on purpose: expanded by the probe, not here.
		printf '  for _a in "$@"; do [ "${_prev}" = "-o" ] && _out="${_a}"; _prev="${_a}"; done\n'
		# shellcheck disable=SC2016 # same.
		printf '  [ -z "${_out}" ] || printf "#!/bin/sh\\nprintf \\"REEXECED\\\\n\\"\\nexit 0\\n" > "${_out}"\n'
		printf '  printf "%%s" "%s"\n' "${_http}"
		printf '}\n'
		# shellcheck disable=SC2016 # literal on purpose: these expand in the
		# generated probe, not here. Same reason install.sh disables SC2016.
		printf 'mktemp() { printf "%%s\\n" "${TMPDIR:-/tmp}/boot.$$"; }\n'
		printf 'AUTHBRIDGE_SCRIPT_REF=""\nWANT_REF="%s"\n' "${_want}"
		sed -n "${_start},${_end}p" "${INSTALL_SH}"
		# shellcheck disable=SC2016 # same: the probe prints its own variables.
		printf 'printf "script=%%s version=%%s\\n" "${SCRIPT_REF}" "${VERSION_REF}"\n'
	} >"${TMP}/bs.sh"
	if [ "${_runas}" = file ]; then
		# $0 is a readable file -> the re-exec must be skipped (local clone / edit).
		sh "${TMP}/bs.sh" 2>/dev/null
	else
		# Simulate the documented `curl … | sh` pipe: the script arrives on stdin, so $0
		# is "sh" with no file at that path — the case the re-exec bootstrap targets. cd
		# to TMP so no stray "sh" file in the caller's cwd can spoof the $0-is-a-file test.
		( cd "${TMP}" && sh ) < "${TMP}/bs.sh" 2>/dev/null
	fi
}

# The plain one-liner while the release API is unreachable. VERSION_REF must be empty so
# resolution still hunts for a release and fails loudly — NOT the channel. This is the
# regression: before the channel existed this path died with "could not resolve the
# newest release"; keying resolution on SCRIPT_REF turned it into a silent unreleased
# install. Unauthenticated api.github.com is 60 req/hr per IP, so it is routine from
# behind NAT, not a corner case.
check "API unreachable, no --ref: script=main, nothing to install" \
	"script=main version=" "$(with_bootstrap "" 000 "")"

# --ref=main, the documented spelling.
check "--ref=main: script=main, install main" \
	"script=main version=main" "$(with_bootstrap main 000 v0.7.0-alpha.7)"

# --ref=<CHANNEL_TAG>, which is the title shown on the Releases page, so someone will
# type it after seeing it there. It must mean the same thing as --ref=main rather than
# bootstrapping that tag's frozen script and then installing newest-release binaries.
check "--ref=CHANNEL_TAG behaves as --ref=main" \
	"script=main version=main" "$(with_bootstrap "${CHANNEL_TAG}" 000 v0.7.0-alpha.7)"

# A pinned release whose tag predates authbridge/install.sh: fall back to main's SCRIPT,
# but keep the pin for the BINARIES. Losing it here would break "--ref=X installs X" on
# the one path where the user was most explicit about X.
check "--ref=v0.5.0 with a 404 script: script=main, install v0.5.0" \
	"script=main version=v0.5.0" "$(with_bootstrap v0.5.0 404 v0.7.0-alpha.7)"

# A transport failure is NOT a 404. We cannot tell whether a released installer exists,
# so running main instead would break the exact guarantee the bootstrap provides — the
# script has to refuse. This is the security-relevant arm and it was unreachable through
# the harness until the curl stub started writing the file the real one writes.
check "--ref=v0.5.0 with a transport failure refuses to run main" \
	"DIED" "$(with_bootstrap v0.5.0 000 v0.7.0-alpha.7)"

# HTTP 200 re-execs the released copy and exits with its status rather than continuing in
# this process. Asserted on the child's OWN output, not on the absence of the probe's:
# an empty result would also be produced by the probe dying early, which is exactly the
# kind of vacuous pass that hid the unreachable arm above.
check "--ref=v0.5.0 with a 200 script re-execs into it" \
	"REEXECED" "$(with_bootstrap v0.5.0 200 v0.7.0-alpha.7)"

# --- run as a LOCAL FILE: never re-exec the released copy (the reported bug) ---
#
# Running ./install.sh from a clone (or an edit under test) must run THAT file, not
# silently re-fetch the newest release and run it — which is what produced "Using the
# installer from vX", ran the OLD released code, and died. $0 is a readable file here,
# so the re-exec is skipped: no REEXECED, no DIED, SCRIPT_REF stays empty. Binaries
# still follow --ref (or resolve newest when unset), so `--ref=X` pins X's binaries even
# though this local script — not X's — is what runs.
check "local file, --ref=v0.5.0: run THIS script, pin v0.5.0 binaries, no re-exec" \
	"script= version=v0.5.0" "$(with_bootstrap v0.5.0 200 v0.7.0-alpha.7 file)"
check "local file, no --ref: run THIS script, resolve binaries later, no re-exec" \
	"script= version=" "$(with_bootstrap "" 200 v0.7.0-alpha.7 file)"

# --- the one-liner survives an exhausted API quota ---
#
# This is the reason the feed source exists. 60 requests/hour per IP, unauthenticated,
# shared behind NAT, two spent per install: exhausting it used to kill the documented
# one-liner and tell the person to look up a version and pass --ref, which is the
# opposite of a one-line install. The API failing must be invisible, not fatal.

fixture feed.xml <<'EOF'
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Release notes from cortex</title>
  <entry>
    <title>v0.7.0-alpha.8</title>
    <content type="html">&lt;p&gt;Prebuilt binaries&lt;/p&gt;</content>
  </entry>
  <entry>
    <title>v0.7.0-alpha.7</title>
  </entry>
</feed>
EOF
FEED="${FIXTURE}"

fixture ratelimit403.json <<'EOF'
{"message":"API rate limit exceeded for 203.0.113.7.","documentation_url":"https://x"}
EOF
check "API 403 falls back to the feed" "v0.7.0-alpha.8" \
	"$(with_newest_release "${FIXTURE}" "${FEED}" 403)"

# The feed's own <title> is the repo's, not a release, and must not be mistaken for one.
check "the feed's own title is not mistaken for a release" "v0.7.0-alpha.8" \
	"$(with_newest_release "${FIXTURE}" "${FEED}" 403)"

# A channel release appears in the feed too, and must be skipped there exactly as it is
# in the API response — otherwise the fallback would reintroduce the bug the v-tag filter
# exists to prevent.
fixture feed_channel.xml <<'EOF'
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Release notes from cortex</title>
  <entry><title>main-latest</title></entry>
  <entry><title>v0.7.0-alpha.8</title></entry>
</feed>
EOF
check "the feed skips the channel release" "v0.7.0-alpha.8" \
	"$(with_newest_release "${TMP}/ratelimit403.json" "${FIXTURE}" 403)"

# Release notes are ours to author and live in the same document, so a version-shaped
# line inside them must not win. The parse is anchored to the <title> element for this
# reason; notes arrive HTML-escaped and cannot forge one.
fixture feed_poisoned.xml <<'EOF'
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Release notes from cortex</title>
  <entry>
    <title>v0.7.0-alpha.8</title>
    <content type="html">v9.9.9-evil is not a release
&lt;p&gt;notes&lt;/p&gt;</content>
  </entry>
</feed>
EOF
check "notes text cannot pose as a release title" "v0.7.0-alpha.8" \
	"$(with_newest_release "${TMP}/ratelimit403.json" "${FIXTURE}" 403)"

# Both sources down is the only remaining failure, and it must stay a failure rather
# than guess.
fixture empty_feed.xml </dev/null
set +e
_st=0; _out=$(with_newest_release "${TMP}/ratelimit403.json" "${FIXTURE}" 403) || _st=$?
set -e
check_fails "both sources failing still fails" "${_st}"
check "and prints nothing" "" "${_out}"

# A healthy API must still be used — the fallback is a fallback, not a replacement.
fixture api_ok.json <<'EOF'
[{"tag_name":"v0.7.0-alpha.8"},{"tag_name":"v0.7.0-alpha.7"}]
EOF
check "a healthy API is used without touching the feed" "v0.7.0-alpha.8" \
	"$(with_newest_release "${FIXTURE}" /dev/null 200)"

# --- the channel tag is one string, in two files ---
#
# install.sh owns CHANNEL_TAG and every test above reads it out of the script rather than
# restating it, so a rename cannot leave a stale expectation passing. That discipline
# stopped at the workflow boundary: CI had its own bare literal with a comment asking a
# human to keep them equal. Rename CHANNEL_TAG and every test here would still pass while
# the channel broke in the only way users see — the installer requesting main-next_*
# assets from a release publishing main-latest_*, i.e. a 404 on download.
#
# The workflow is also where the one destructive operation lives: the tag move is guarded
# by that same literal, so a drift makes a v* release movable.
WORKFLOW="${SCRIPT_DIR}/../.github/workflows/release-binaries.yaml"
if [ -f "${WORKFLOW}" ]; then
	check "CI publishes the tag install.sh asks for" "1" \
		"$(grep -c "TAG=\"${CHANNEL_TAG}\"" "${WORKFLOW}" || true)"
	# Every `[ "${TAG}" = ... ]` in the workflow must name CHANNEL_TAG. Counting matches
	# would be brittle — there are legitimately two today, the release-notes switch and
	# the tag-move guard — so assert the absence of any comparison against a DIFFERENT
	# literal instead. That is the invariant: no branch keyed on a stale channel name.
	check "no TAG comparison names a different tag" "0" \
		"$(grep -oE '\[ "\$\{TAG\}" = "[^"]*" \]' "${WORKFLOW}" | grep -cv "\"${CHANNEL_TAG}\"" || true)"
	check "at least one TAG comparison names CHANNEL_TAG" "1" \
		"$(grep -oE '\[ "\$\{TAG\}" = "[^"]*" \]' "${WORKFLOW}" | grep -c "\"${CHANNEL_TAG}\"" | awk '$1>0{print 1; exit} {print 0}')"
else
	check "release-binaries.yaml is where expected" "found" "missing at ${WORKFLOW}"
fi

# --- port_in_use: the ss branch counts only loopback / wildcard binds ---
#
# ss reports the Local Address:Port in column 4. A listener on an EXTERNAL interface
# does not make the loopback port unavailable, so port_in_use must not count it —
# matching it would make start_unsupervised's health poll return early and stop_cortex
# warn about a proxy that is not there. IPv6 loopback ([::1]) is the case a naive IPv4
# pattern misses: the proxy can bind ::1 only, and reading that as "free" makes the
# health poll spin its full 10s. `command` is stubbed so lsof is "absent" and ss is
# "present", forcing the ss path; ss is stubbed to emit the fixture (the real ss
# filters by sport, but the awk address-column check is the code under test).
with_port_in_use_ss() { # port  ss-listing-fixture
	{
		printf 'command() { case "$2" in ss) return 0 ;; *) return 1 ;; esac; }\n'
		printf 'ss() { cat "%s"; }\n' "$2"
		sed -n '/^port_in_use()/,/^}/p' "${INSTALL_SH}"
		printf 'if port_in_use "%s"; then echo busy; else echo free; fi\n' "$1"
	} >"${TMP}/pin.sh"
	sh "${TMP}/pin.sh" 2>/dev/null
}

fixture ss_v4loop.txt <<'EOF'
LISTEN 0 4096 127.0.0.1:47600 0.0.0.0:*
EOF
check "ss: IPv4 loopback bind reads busy" "busy" "$(with_port_in_use_ss 47600 "${FIXTURE}")"

fixture ss_v6loop.txt <<'EOF'
LISTEN 0 4096 [::1]:47600 [::]:*
EOF
check "ss: IPv6 loopback [::1] bind reads busy" "busy" "$(with_port_in_use_ss 47600 "${FIXTURE}")"

fixture ss_wild4.txt <<'EOF'
LISTEN 0 4096 0.0.0.0:47600 0.0.0.0:*
EOF
check "ss: IPv4 wildcard bind reads busy" "busy" "$(with_port_in_use_ss 47600 "${FIXTURE}")"

fixture ss_wild6.txt <<'EOF'
LISTEN 0 4096 [::]:47600 [::]:*
EOF
check "ss: IPv6 wildcard bind reads busy" "busy" "$(with_port_in_use_ss 47600 "${FIXTURE}")"

fixture ss_external.txt <<'EOF'
LISTEN 0 4096 192.168.1.5:47600 0.0.0.0:*
EOF
check "ss: external-only bind reads free (loopback port still available)" "free" "$(with_port_in_use_ss 47600 "${FIXTURE}")"

fixture ss_none.txt </dev/null
check "ss: nothing listening reads free" "free" "$(with_port_in_use_ss 47600 "${FIXTURE}")"

# The awk anchors the port to end-of-field, so a loopback bind on a DIFFERENT port
# (or a port that is a substring, e.g. :47600 inside :476000) must not count.
fixture ss_otherport.txt <<'EOF'
LISTEN 0 4096 127.0.0.1:9999 0.0.0.0:*
EOF
check "ss: a loopback bind on another port reads free" "free" "$(with_port_in_use_ss 47600 "${FIXTURE}")"

# --- supervisor_usable: attempt-and-detect, with launchctl/systemctl mocked ---
#
# The preflight that decides whether to reach for the OS supervisor. The seatbelt
# sandbox that motivated this PR is "launchctl is present but `launchctl list` fails"
# — mocked here, plus the systemd-user equivalent, absence, and the working case.
with_supervisor_usable() { # os(darwin|linux)  present(1|0)  probe_exit
	_absent=1; [ "$2" = 1 ] && _absent=0
	{
		printf 'os=%s\n' "$1"
		printf 'command() { case "$2" in launchctl|systemctl) return %s ;; *) return 0 ;; esac; }\n' "${_absent}"
		printf 'launchctl() { return %s; }\n' "$3"
		printf 'systemctl() { return %s; }\n' "$3"
		sed -n '/^supervisor_usable()/,/^}/p' "${INSTALL_SH}"
		printf 'if supervisor_usable; then echo usable; else echo unusable; fi\n'
	} >"${TMP}/su.sh"
	sh "${TMP}/su.sh" 2>/dev/null
}
check "supervisor: darwin, launchctl absent -> unusable" "unusable" "$(with_supervisor_usable darwin 0 0)"
check "supervisor: darwin, launchctl present but 'list' fails (sandbox) -> unusable" "unusable" "$(with_supervisor_usable darwin 1 1)"
check "supervisor: darwin, launchctl present and 'list' ok -> usable" "usable" "$(with_supervisor_usable darwin 1 0)"
check "supervisor: linux, systemctl absent -> unusable" "unusable" "$(with_supervisor_usable linux 0 0)"
check "supervisor: linux, user bus unreachable -> unusable" "unusable" "$(with_supervisor_usable linux 1 1)"
check "supervisor: linux, systemctl reachable -> usable" "usable" "$(with_supervisor_usable linux 1 0)"

# --- proxy_running: five branches, and it gates a kill ---
#
# stop_cortex signals the pid this returns, so every branch matters: a non-numeric,
# empty, missing, or dead pid must read stopped (never SIGTERM a recycled pid), and
# where the sandbox blinds `ps` we keep the kill -0 result rather than refuse. kill and
# ps are mocked; the pidfile is a real file so the cat + `case` parsing is shipped code.
with_proxy_running() { # pidfile-content(__MISSING__ for none)  kill_exit  ps_mode(fail|empty|COMM)
	_pf="${TMP}/pr_pidfile"
	if [ "$1" = "__MISSING__" ]; then rm -f "${_pf}"; else printf '%s\n' "$1" >"${_pf}"; fi
	{
		printf 'PROXY_PIDFILE=%s\n' "${_pf}"
		printf 'kill() { return %s; }\n' "$2"
		case "$3" in
			fail)  printf 'ps() { return 1; }\n' ;;
			empty) printf 'ps() { return 0; }\n' ;;
			*)     printf 'ps() { printf "%%s\\n" "%s"; }\n' "$3" ;;
		esac
		sed -n '/^proxy_running()/,/^}/p' "${INSTALL_SH}"
		printf 'if proxy_running; then echo running; else echo stopped; fi\n'
	} >"${TMP}/prun.sh"
	sh "${TMP}/prun.sh" 2>/dev/null
}
check "proxy_running: missing pidfile -> stopped" "stopped" "$(with_proxy_running __MISSING__ 0 authbridge-proxy)"
check "proxy_running: non-numeric pid -> stopped" "stopped" "$(with_proxy_running abc 0 authbridge-proxy)"
check "proxy_running: empty pid -> stopped" "stopped" "$(with_proxy_running '' 0 authbridge-proxy)"
check "proxy_running: dead pid (kill -0 fails) -> stopped" "stopped" "$(with_proxy_running 12345 1 authbridge-proxy)"
check "proxy_running: alive, ps blind (sandbox) -> running" "running" "$(with_proxy_running 12345 0 fail)"
check "proxy_running: alive, ps empty comm -> running" "running" "$(with_proxy_running 12345 0 empty)"
check "proxy_running: alive, ps names authbridge-proxy -> running" "running" "$(with_proxy_running 12345 0 authbridge-proxy)"
check "proxy_running: alive, ps names another process -> stopped" "stopped" "$(with_proxy_running 12345 0 sshd)"

# --- ensure_tmpdir: exports TMPDIR on BOTH paths (regression: unset-TMPDIR abort) ---
#
# With `set -u`, a later "${TMPDIR}" reference (the svc_err mktemp on the supervised
# path) aborts the script if ensure_tmpdir returned without exporting TMPDIR. macOS
# hides this because launchd sets TMPDIR per user; Linux routinely has it unset — so
# the writable-base path must export TMPDIR too, not just the fallback. mkdir/rmdir are
# mocked so the result does not depend on whether /tmp is writable where the test runs;
# TMPDIR is unset for the run so the base defaults to /tmp.
with_ensure_tmpdir() { # base_writable(ok|deny)  [preset-TMPDIR]
	if [ "$1" = ok ]; then _mk='mkdir() { return 0; }'
	else _mk='mkdir() { for _a in "$@"; do [ "$_a" = "-p" ] && return 0; done; return 1; }'
	fi
	{
		printf 'CORTEX_DIR=%s\n' "${TMP}/cortexhome"
		printf 'info() { :; }\ndie() { printf "DIED\\n"; exit 1; }\nrmdir() { :; }\n'
		printf '%s\n' "${_mk}"
		sed -n '/^ensure_tmpdir()/,/^}/p' "${INSTALL_SH}"
		printf 'ensure_tmpdir\n'
		printf 'printf "%%s\\n" "${TMPDIR-__UNSET__}"\n'
	} >"${TMP}/et.sh"
	if [ -n "${2:-}" ]; then TMPDIR="$2" sh "${TMP}/et.sh" 2>/dev/null
	else env -u TMPDIR sh "${TMP}/et.sh" 2>/dev/null; fi
}
check "ensure_tmpdir: unset TMPDIR, writable /tmp -> adopt & export /tmp (else set -u aborts)" "/tmp" "$(with_ensure_tmpdir ok)"
check "ensure_tmpdir: unwritable base falls back under CORTEX_DIR and exports it" "${TMP}/cortexhome/tmp" "$(with_ensure_tmpdir deny)"
# An already-set writable TMPDIR is used AS-IS — never overwritten with a normalised
# copy. The trailing-slash case is the tell: the old code round-tripped through _base
# and stripped it; TMPDIR must come back byte-for-byte.
check "ensure_tmpdir: an already-set writable TMPDIR is kept verbatim" "/custom/t" "$(with_ensure_tmpdir ok /custom/t)"
check "ensure_tmpdir: an already-set TMPDIR keeps its trailing slash (not clobbered)" "/custom/t/" "$(with_ensure_tmpdir ok /custom/t/)"
# A set-but-unwritable TMPDIR still falls back rather than failing.
check "ensure_tmpdir: set-but-unwritable TMPDIR falls back under CORTEX_DIR" "${TMP}/cortexhome/tmp" "$(with_ensure_tmpdir deny /nope)"

# --- service_install_action: classify `abctl service install` -> what to do ---
#
# The reported bug: `abctl service install` failed with `launchctl bootstrap failed:
# ... Input/output error` (exit 1), but the installer died with "could not set up the
# service" instead of running the proxy directly. Root cause: abctl prints that on
# STDOUT, and the decision matched a signature in STDERR only. The rule is now
# exit + `refus` + ports, so the failure text's stream no longer matters, and any
# unrecognised failure falls back (Cortex runs) rather than dying. demo_ports_busy is
# mocked; grep is real.
with_service_install_action() { # status  output  ports_busy(yes|no)  [foreign-holder]
	{
		if [ "$3" = yes ]; then printf 'demo_ports_busy() { return 0; }\n'
		else printf 'demo_ports_busy() { return 1; }\n'; fi
		# Stubbed explicitly rather than left undefined: these cases are about the
		# ORDER of the verdicts, so "no foreign holder" has to be a deliberate
		# answer. $4 empty means none found (return 1, printing nothing).
		if [ -n "${4:-}" ]; then
			printf 'foreign_proxy_holder() { printf "%%s\\n" "%s"; }\n' "$4"
		else
			printf 'foreign_proxy_holder() { return 1; }\n'
		fi
		sed -n '/^service_install_action()/,/^}/p' "${INSTALL_SH}"
		printf 'service_install_action "%s" "%s"\n' "$1" "$2"
	} >"${TMP}/sia.sh"
	sh "${TMP}/sia.sh" 2>/dev/null
}
check "svc action: exit 0 -> supervised" "supervised" \
	"$(with_service_install_action 0 ok no)"
check "svc action: launchd EIO (exit 1), ports free -> fallback [the reported bug]" "fallback" \
	"$(with_service_install_action 1 'launchctl bootstrap failed: exit status 5: Input/output error' no)"
check "svc action: an abctl refusal -> refused (never fall back past a safety decision)" "refused" \
	"$(with_service_install_action 1 'abctl: refusing to expose listener' no)"
check "svc action: non-zero with ports held -> ports-busy (upgrade race)" "ports-busy" \
	"$(with_service_install_action 1 'bootstrap failed' yes)"
check "svc action: unknown non-zero, ports free -> fallback (default; Cortex still runs)" "fallback" \
	"$(with_service_install_action 7 'some unrecognised error' no)"
# A safety refusal must win over a busy-port race — never downgraded to "wait and
# re-run", which would eventually run the config abctl refused.
check "svc action: refusal wins over ports-busy" "refused" \
	"$(with_service_install_action 1 'refused: unsafe config' yes)"

# foreign-proxy is checked BEFORE ports-busy because the two are indistinguishable
# at the port level and want opposite advice: wait for it vs stop it. The verdict
# carries the pid and path through, since die() names them.
check "svc action: a foreign holder outranks ports-busy (opposite advice)" \
	"foreign-proxy 84858 /co/.local/bin/authbridge-proxy" \
	"$(with_service_install_action 1 'bind: address already in use' yes '84858 /co/.local/bin/authbridge-proxy')"
# ...but never over a safety refusal, which must still win outright.
check "svc action: refusal outranks a foreign holder" "refused" \
	"$(with_service_install_action 1 'refusing to expose listener' yes '84858 /co/.local/bin/authbridge-proxy')"
# An unidentifiable holder must fall through to ports-busy, NOT be accused.
check "svc action: no identifiable holder + ports held -> ports-busy" "ports-busy" \
	"$(with_service_install_action 1 'bind: address already in use' yes '')"
# A foreign holder is irrelevant when the install actually succeeded.
check "svc action: exit 0 wins even with a foreign holder present" "supervised" \
	"$(with_service_install_action 0 ok yes '84858 /co/.local/bin/authbridge-proxy')"

# --- pid_exe_path: the full path, because `comm` cannot carry one on Linux ---
#
# The bug this replaced: the path check used `ps -o comm=`. On Linux `comm` is the
# kernel's comm field — argv[0]'s basename capped at 15 chars (TASK_COMM_LEN-1) —
# so a 16-char "authbridge-proxy" prints as "authbridge-prox" and NEVER as a path.
# Compared for equality against an install path that made every busy-port upgrade
# race on Linux classify as foreign-proxy, and die telling the user to kill their
# own proxy. macOS hid it completely: there `comm` does print a path.
#
# Three sources in order: /proc/<pid>/exe (Linux kernel truth), lsof -d txt (the
# macOS equivalent), then `ps -o args=` as a last resort. A fake /proc tree stands
# in for the real one so the readlink branch is exercised on macOS too.
with_pid_exe_path() { # proc_exe_target(empty for no /proc)  lsof_txt  ps_args
	_root="${TMP}/pep"; rm -rf "${_root}"; mkdir -p "${_root}/proc/4242"
	if [ -n "$1" ]; then ln -s "$1" "${_root}/proc/4242/exe"; fi
	{
		printf 'PROCROOT=%s\n' "${_root}"
		if [ -n "$2" ]; then
			printf 'command() { return 0; }\n'
			# Modelled on real lsof rather than echoing a fixed answer: list-selection
			# options are ORed unless -a is given, so without -a `-p <pid> -d txt`
			# lists every process's executable and head -1 takes whatever came first.
			# The stub emits an unrelated record FIRST in that case, so dropping -a
			# fails the test instead of passing silently.
			printf 'lsof() {\n'
			printf '\t_a=no; for _w in "$@"; do [ "${_w}" = "-a" ] && _a=yes; done\n'
			printf '\t[ "${_a}" = no ] && printf "n/usr/bin/some-other-process\\n"\n'
			printf '\tprintf "n%%s\\n" "%s"\n' "$2"
			printf '}\n'
		else
			printf 'command() { case "$2" in lsof) return 1 ;; *) return 0 ;; esac; }\n'
		fi
		if [ -n "$3" ]; then printf 'ps() { printf "%%s\\n" "%s"; }\n' "$3"
		else printf 'ps() { return 1; }\n'; fi
		# Rewrite the two /proc references onto the fixture tree. The logic under
		# test — the readlink, the (deleted) trim, the source ordering — is shipped
		# code; only the root moves.
		sed -n '/^pid_exe_path()/,/^}/p' "${INSTALL_SH}" \
			| sed 's#"/proc/$1/exe"#"${PROCROOT}/proc/$1/exe"#g'
		printf 'pid_exe_path 4242 || echo __NONE__\n'
	} >"${TMP}/pep.sh"
	sh "${TMP}/pep.sh" 2>/dev/null
}
check "pid_exe_path: /proc/<pid>/exe is preferred (Linux truth)" \
	"/home/u/.local/bin/authbridge-proxy" \
	"$(with_pid_exe_path /home/u/.local/bin/authbridge-proxy /lsof/path /ps/path)"
# A binary replaced under a running process reads "<path> (deleted)" — an upgrade
# in progress is exactly when this code runs, so the suffix must be trimmed off
# rather than travelling into a path comparison that would then call it foreign.
check "pid_exe_path: a deleted/replaced binary keeps its path, drops ' (deleted)'" \
	"/home/u/.local/bin/authbridge-proxy" \
	"$(with_pid_exe_path '/home/u/.local/bin/authbridge-proxy (deleted)' '' '')"
check "pid_exe_path: no /proc -> lsof txt descriptor (the macOS path)" \
	"/Users/u/.local/bin/authbridge-proxy" \
	"$(with_pid_exe_path '' /Users/u/.local/bin/authbridge-proxy /ps/path)"
# Same case, stated as the bug it guards: lsof ORs -p and -d unless -a is passed,
# so without it the query means "this pid OR any txt descriptor on the system" and
# head -1 can take another process's executable. On macOS this is the source
# foreign_proxy_holder judges, so the wrong path there classifies our OWN managed
# proxy as foreign and dies. The stub above emits a foreign record first when -a is
# missing, so this check is what fails if the flag is ever dropped.
check "pid_exe_path: the lsof query is ANDed with -a (not 'this pid OR any txt fd')" \
	"/Users/u/.local/bin/authbridge-proxy" \
	"$(with_pid_exe_path '' /Users/u/.local/bin/authbridge-proxy '')"
# Belt and braces: pin the flag in the source too, so a refactor that rewrites the
# invocation cannot quietly lose the conjunction while still passing the stub test.
check "the lsof executable query passes -a" "1" \
	"$(grep -c 'lsof -p "\$1" -a -d txt' "${INSTALL_SH}")"
# argv[0] is the weakest source (caller-chosen, possibly relative) so it is last,
# but it beats reporting nothing.
check "pid_exe_path: no /proc, no lsof -> first field of ps args" \
	"/Users/u/.local/bin/authbridge-proxy" \
	"$(with_pid_exe_path '' '' '/Users/u/.local/bin/authbridge-proxy --local --supervise')"
# Nothing can name it: must FAIL, never print a placeholder. foreign_proxy_holder
# treats any non-match as foreign, so "unknown" as a value would accuse a process
# nobody can see — the thing the previous `${_ph_cmd:-unknown}` fallback did.
check "pid_exe_path: nothing can name the pid -> fails, prints no placeholder" \
	"__NONE__" "$(with_pid_exe_path '' '' '')"

# --- foreign_proxy_holder: fails closed, and only accuses on a positive mismatch ---
#
# Its verdict gates a die(), so a false positive tells a user mid-upgrade to kill
# their own working proxy. Every "cannot tell" branch must therefore read as ours.
# port_holder is mocked (the pid/path discovery is covered above); the pidfile is a
# real file so the cat + comparison is shipped code.
with_foreign_proxy_holder() { # holder-line(empty=none)  pidfile(__MISSING__)  bin_dir
	_pf="${TMP}/fph_pidfile"
	if [ "$2" = "__MISSING__" ]; then rm -f "${_pf}"; else printf '%s\n' "$2" >"${_pf}"; fi
	{
		printf 'DEMO_FORWARD_PORT=47600\n'
		printf 'PROXY_PIDFILE=%s\n' "${_pf}"
		printf 'BIN_DIR=%s\n' "$3"
		if [ -n "$1" ]; then printf 'port_holder() { printf "%%s\\n" "%s"; }\n' "$1"
		else printf 'port_holder() { return 1; }\n'; fi
		sed -n '/^foreign_proxy_holder()/,/^}/p' "${INSTALL_SH}"
		printf 'foreign_proxy_holder || echo __OURS__\n'
	} >"${TMP}/fph.sh"
	sh "${TMP}/fph.sh" 2>/dev/null
}
# The reported bug: a proxy from a checkout, different path, never drains.
check "foreign: a holder at another path IS foreign (the reported bug)" \
	"84858 /co/.local/bin/authbridge-proxy" \
	"$(with_foreign_proxy_holder '84858 /co/.local/bin/authbridge-proxy' __MISSING__ /home/u/.local/bin)"
# The genuine upgrade race: our own managed binary restarting. Must stay ours, or
# the installer dies on an ordinary upgrade — what the PR promises not to do.
check "foreign: our own managed binary is NOT foreign (upgrade race preserved)" \
	"__OURS__" \
	"$(with_foreign_proxy_holder '29497 /home/u/.local/bin/authbridge-proxy' __MISSING__ /home/u/.local/bin)"
# The pidfile identifies our unsupervised proxy even when the path check would not.
check "foreign: the pid in our pidfile is NOT foreign, whatever its path" \
	"__OURS__" \
	"$(with_foreign_proxy_holder '777 /some/other/authbridge-proxy' 777 /home/u/.local/bin)"
# An empty pidfile must not match an empty-ish pid field or accuse blindly.
check "foreign: an empty pidfile does not make a real holder ours" \
	"84858 /co/.local/bin/authbridge-proxy" \
	"$(with_foreign_proxy_holder '84858 /co/.local/bin/authbridge-proxy' '' /home/u/.local/bin)"
# Nobody is listening: nothing to report.
check "foreign: no holder at all -> nothing reported" \
	"__OURS__" "$(with_foreign_proxy_holder '' __MISSING__ /home/u/.local/bin)"
# port_holder now refuses to emit a pid without a path, but if a bare pid ever
# reached here it must not be judged: one field means the path is unknown.
check "foreign: a pid with no path is unjudgeable, not foreign" \
	"__OURS__" "$(with_foreign_proxy_holder '84858' __MISSING__ /home/u/.local/bin)"
# A non-proxy process holding the port is still someone else's, and still blocks
# the bind — worth naming rather than calling a drain that will never finish.
check "foreign: an unrelated process holding the port is foreign too" \
	"3121 /usr/bin/python3" \
	"$(with_foreign_proxy_holder '3121 /usr/bin/python3' __MISSING__ /home/u/.local/bin)"

# --- foreign_proxy_holder: the readlink -f canonicalisation branch ---
#
# The subtlest branch here, and it was the one with no coverage: the cases above all use
# fictitious paths, where `readlink -f` resolves nothing and the plain text comparison
# decides the verdict on its own. So nothing exercised a holder path that differs
# TEXTUALLY from ${BIN_DIR}/authbridge-proxy while canonicalising EQUAL — which is
# precisely the false-foreign the branch exists to prevent (a symlinked $HOME, or
# /var -> /private/var on macOS, makes /proc/<pid>/exe report a different string for the
# very same file). Getting that wrong dies on an ordinary upgrade.
#
# Real files and a real symlink on disk, because readlink -f is shipped code here and a
# stub would only test the stub. hide_readlink=yes drops `readlink` from view instead, to
# pin what the `|| true` degrades TO on a platform that lacks it.
with_foreign_canonical() { # holder-path  bin_dir  hide_readlink(yes|no)
	_root="${TMP}/canon"; rm -rf "${_root}"
	mkdir -p "${_root}/real/bin" "${_root}/other/bin"
	: >"${_root}/real/bin/authbridge-proxy"
	: >"${_root}/other/bin/authbridge-proxy"
	# An alias directory reaching the SAME binary by a different string — the symlinked
	# $HOME / /private/var shape, reduced to its essentials.
	ln -s "${_root}/real" "${_root}/alias"
	{
		printf 'DEMO_FORWARD_PORT=47600\n'
		printf 'PROXY_PIDFILE=%s/no_such_pidfile\n' "${_root}"
		printf 'BIN_DIR=%s\n' "$2"
		printf 'port_holder() { printf "84858 %%s\\n" "%s"; }\n' "$1"
		if [ "$3" = yes ]; then
			# command -v readlink fails => the canonicalisation block is skipped whole.
			printf 'command() { case "$2" in readlink) return 1 ;; *) return 0 ;; esac; }\n'
		fi
		sed -n '/^foreign_proxy_holder()/,/^}/p' "${INSTALL_SH}"
		printf 'foreign_proxy_holder || echo __OURS__\n'
	} >"${TMP}/fphcanon.sh"
	sh "${TMP}/fphcanon.sh" 2>/dev/null
}
_CANON="${TMP}/canon"
# The false-foreign this branch exists to prevent: two different strings, one file.
# Without the readlink -f comparison this reads as foreign and the installer dies
# telling the user to kill their own proxy mid-upgrade.
check "foreign/canonical: a symlinked path to OUR binary is not foreign" \
	"__OURS__" \
	"$(with_foreign_canonical "${_CANON}/alias/bin/authbridge-proxy" "${_CANON}/real/bin" no)"
# ...and it holds in the other direction too, so the test is not just asserting that
# one specific spelling wins.
check "foreign/canonical: ours via the real path when BIN_DIR is the symlinked one" \
	"__OURS__" \
	"$(with_foreign_canonical "${_CANON}/real/bin/authbridge-proxy" "${_CANON}/alias/bin" no)"
# The branch must not over-broaden into "anything resolvable is ours": a genuinely
# different binary canonicalises to a different path and stays foreign.
check "foreign/canonical: a different real binary still canonicalises foreign" \
	"84858 ${_CANON}/other/bin/authbridge-proxy" \
	"$(with_foreign_canonical "${_CANON}/other/bin/authbridge-proxy" "${_CANON}/real/bin" no)"
# No readlink: `|| true` skips canonicalisation and the plain text comparison decides,
# so the symlinked spelling reads as foreign. That fails CLOSED in the loud direction
# (a false accusation, not a missed one), which is worth having visible in a test rather
# than discovered on a platform without readlink. readlink -f does work on current
# macOS and Linux, so this is the degraded path, not the usual one.
check "foreign/canonical: without readlink the symlinked path degrades to foreign" \
	"84858 ${_CANON}/alias/bin/authbridge-proxy" \
	"$(with_foreign_canonical "${_CANON}/alias/bin/authbridge-proxy" "${_CANON}/real/bin" yes)"

# --- port_holder: the lsof branch, and the four addresses it has to probe ---
#
# This is the macOS path, and the platform the bug was actually reported on — but every
# other port_holder case here either stubs lsof away (with_port_holder_ss makes
# `command -v` fail for everything but ss) or exercises the neither-tool case, so all of
# the address matching landed on the ss branch and this one had no coverage at all.
#
# The four-address loop is load-bearing rather than defensive padding: a `*:PORT`
# wildcard bind IS matched by -i@0.0.0.0:PORT and missed entirely by -i@127.0.0.1:PORT,
# so probing only loopback would report "nothing holds it" while a wildcard listener sat
# on the port. The stub below answers for ONE address, so each case proves its own probe
# runs rather than riding on a catch-all.
with_port_holder_lsof() { # port  address-that-answers  [second-address-that-answers]
	{
		printf 'command() { case "$2" in lsof) return 0 ;; *) return 1 ;; esac; }\n'
		# Modelled on real lsof's -i@<addr>:<port> selection: answer only when the
		# queried address is one this fixture is listening on. $2/$3 are matched against
		# the -iTCP@... argument, so a probe for a different address prints nothing and
		# the loop must move on to the next one.
		printf 'lsof() {\n'
		printf '\t_want=""\n'
		printf '\tfor _w in "$@"; do case "${_w}" in -iTCP@*) _want=${_w#-iTCP@} ;; esac; done\n'
		printf '\tcase "${_want}" in\n'
		printf '\t\t"%s:%s") printf "p84858\\n" ;;\n' "$2" "$1"
		if [ -n "${3:-}" ]; then
			printf '\t\t"%s:%s") printf "p99999\\n" ;;\n' "$3" "$1"
		fi
		printf '\t\t*) return 1 ;;\n'
		printf '\tesac\n'
		printf '}\n'
		printf 'pid_exe_path() { printf "/Users/u/.local/bin/authbridge-proxy\\n"; }\n'
		sed -n '/^port_holder()/,/^}/p' "${INSTALL_SH}"
		printf 'port_holder "%s" || echo __NONE__\n' "$1"
	} >"${TMP}/phlsof.sh"
	sh "${TMP}/phlsof.sh" 2>/dev/null
}
check "port_holder/lsof: IPv4 loopback -> pid from -Fp [the macOS path]" \
	"84858 /Users/u/.local/bin/authbridge-proxy" \
	"$(with_port_holder_lsof 47600 127.0.0.1)"
# Missed by the 127.0.0.1 probe alone, and it does hold the loopback port.
check "port_holder/lsof: IPv6 loopback [::1] is probed too" \
	"84858 /Users/u/.local/bin/authbridge-proxy" \
	"$(with_port_holder_lsof 47600 '[::1]')"
# The case the loop exists for: verified against real lsof that a wildcard bind answers
# -i@0.0.0.0 and is invisible to -i@127.0.0.1. Dropping that probe fails here.
check "port_holder/lsof: a wildcard 0.0.0.0 bind is found (invisible to the loopback probe)" \
	"84858 /Users/u/.local/bin/authbridge-proxy" \
	"$(with_port_holder_lsof 47600 0.0.0.0)"
check "port_holder/lsof: an IPv6 wildcard [::] bind is found" \
	"84858 /Users/u/.local/bin/authbridge-proxy" \
	"$(with_port_holder_lsof 47600 '[::]')"
# One port, one holder: the loop breaks on the first address that answers rather than
# collecting every match, so two listening addresses must still yield a single pid.
# Without the break this reads "84858 99999 <path>" or similar.
check "port_holder/lsof: the loop breaks on the first hit (one pid, not a concatenation)" \
	"84858 /Users/u/.local/bin/authbridge-proxy" \
	"$(with_port_holder_lsof 47600 127.0.0.1 '[::1]')"
# Nothing on any of the four addresses: report nothing rather than guess.
check "port_holder/lsof: nothing listening on any probed address reports nothing" \
	"__NONE__" "$(with_port_holder_lsof 47600 198.51.100.7)"

# --- port_holder: the ss fallback, and which binds count as holding the port ---
#
# lsof-only meant this detection silently never fired on modern Linux, where
# iproute2 is the default and lsof is often absent — the same platform port_in_use
# went three-way out of its way to support. ss cannot name the binary, but it
# prints the pid, and the path is resolved from the pid separately anyway.
#
# Address matching must agree with port_in_use: IPv4 loopback, IPv6 loopback and a
# wildcard bind all make the loopback port unavailable; an external-only bind does
# not and must not be reported as the holder.
with_port_holder_ss() { # port  ss-listing-fixture
	{
		printf 'command() { case "$2" in ss) return 0 ;; *) return 1 ;; esac; }\n'
		printf 'ss() { cat "%s"; }\n' "$2"
		printf 'pid_exe_path() { printf "/home/u/.local/bin/authbridge-proxy\\n"; }\n'
		sed -n '/^port_holder()/,/^}/p' "${INSTALL_SH}"
		printf 'port_holder "%s" || echo __NONE__\n' "$1"
	} >"${TMP}/phss.sh"
	sh "${TMP}/phss.sh" 2>/dev/null
}
fixture ssp_v4loop.txt <<'EOF'
LISTEN 0 4096 127.0.0.1:47600 0.0.0.0:* users:(("authbridge-prox",pid=84858,fd=7))
EOF
check "port_holder/ss: IPv4 loopback -> pid from users:((...)) [lsof absent]" \
	"84858 /home/u/.local/bin/authbridge-proxy" "$(with_port_holder_ss 47600 "${FIXTURE}")"
fixture ssp_v6loop.txt <<'EOF'
LISTEN 0 4096 [::1]:47600 [::]:* users:(("authbridge-prox",pid=84858,fd=7))
EOF
check "port_holder/ss: IPv6 loopback [::1] also holds the port" \
	"84858 /home/u/.local/bin/authbridge-proxy" "$(with_port_holder_ss 47600 "${FIXTURE}")"
fixture ssp_wild.txt <<'EOF'
LISTEN 0 4096 0.0.0.0:47600 0.0.0.0:* users:(("authbridge-prox",pid=84858,fd=7))
EOF
check "port_holder/ss: a wildcard bind holds the loopback port" \
	"84858 /home/u/.local/bin/authbridge-proxy" "$(with_port_holder_ss 47600 "${FIXTURE}")"
fixture ssp_wild6.txt <<'EOF'
LISTEN 0 4096 [::]:47600 [::]:* users:(("authbridge-prox",pid=84858,fd=7))
EOF
check "port_holder/ss: an IPv6 wildcard bind holds it too" \
	"84858 /home/u/.local/bin/authbridge-proxy" "$(with_port_holder_ss 47600 "${FIXTURE}")"
# An external-only listener leaves the loopback port free: reporting it as the
# holder would accuse an unrelated process of a conflict that does not exist.
fixture ssp_external.txt <<'EOF'
LISTEN 0 4096 192.168.1.5:47600 0.0.0.0:* users:(("nginx",pid=999,fd=7))
EOF
check "port_holder/ss: an external-only bind is not the loopback holder" \
	"__NONE__" "$(with_port_holder_ss 47600 "${FIXTURE}")"
# The port is anchored, so a loopback bind on another port must not be picked up.
fixture ssp_otherport.txt <<'EOF'
LISTEN 0 4096 127.0.0.1:9999 0.0.0.0:* users:(("something",pid=555,fd=7))
EOF
check "port_holder/ss: a loopback bind on a different port is not the holder" \
	"__NONE__" "$(with_port_holder_ss 47600 "${FIXTURE}")"
# ss without -p access (or a kernel that withholds it) prints no users:((...)).
# No pid means nothing to resolve: report nothing rather than guess.
fixture ssp_nopid.txt <<'EOF'
LISTEN 0 4096 127.0.0.1:47600 0.0.0.0:*
EOF
check "port_holder/ss: a matching bind with no pid field reports nothing" \
	"__NONE__" "$(with_port_holder_ss 47600 "${FIXTURE}")"
fixture ssp_none.txt </dev/null
check "port_holder/ss: nothing listening reports nothing" \
	"__NONE__" "$(with_port_holder_ss 47600 "${FIXTURE}")"

# Neither tool present: the no-op the PR promises. Nothing is reported, so the
# classification stays ports-busy rather than accusing an invisible process.
neither_tool_port_holder() {
	{
		printf 'command() { return 1; }\n'
		sed -n '/^port_holder()/,/^}/p' "${INSTALL_SH}"
		printf 'port_holder 47600 || echo __NONE__\n'
	} >"${TMP}/phnone.sh"
	sh "${TMP}/phnone.sh" 2>/dev/null
}
check "port_holder: no lsof and no ss -> no-op (previous behavior preserved)" \
	"__NONE__" "$(neither_tool_port_holder)"

# A pid found but unnameable must not yield "<pid> unknown": the placeholder was
# the second reported defect, since any non-match reads as foreign downstream.
pid_without_path_port_holder() {
	{
		printf 'command() { case "$2" in ss) return 0 ;; *) return 1 ;; esac; }\n'
		printf 'ss() { printf "LISTEN 0 4096 127.0.0.1:47600 0.0.0.0:* users:((\\"x\\",pid=84858,fd=7))\\n"; }\n'
		printf 'pid_exe_path() { return 1; }\n'
		sed -n '/^port_holder()/,/^}/p' "${INSTALL_SH}"
		printf 'port_holder 47600 || echo __NONE__\n'
	} >"${TMP}/phnp.sh"
	sh "${TMP}/phnp.sh" 2>/dev/null
}
check "port_holder: a pid whose path cannot be resolved reports nothing (no 'unknown')" \
	"__NONE__" "$(pid_without_path_port_holder)"

# The regression guard proper: `ps -o comm=` may appear EXACTLY ONCE in install.sh —
# the sanctioned use in proxy_running, which matches the truncated *authbridge-prox*
# on purpose and never compares a path.
#
# Counting is the whole point, and the previous form of this check is why. It grepped
# for `comm=` and `authbridge-proxy` on ONE line and asserted zero — but the defect
# spanned two lines (the `comm=` capture in port_holder and the path comparison in
# foreign_proxy_holder), so that pattern returned 0 on the buggy commit too: the exact
# value it asserted as clean. It could not fail on the bug its own comment named, which
# is worse than no check, because the comment invited readers to trust it.
#
# Verified both ways: this count is 2 on d5f8abd3 (the commit that shipped the defect —
# port_holder's capture plus proxy_running's) and 1 here. A second occurrence is not
# forbidden forever, but it has to be argued for rather than appear by accident.
#
# Comment lines are stripped first, and that is not incidental: pid_exe_path's header
# explains at length why `ps -o comm=` cannot carry a path, quoting it verbatim. Counting
# prose would make this fire on someone DOCUMENTING the hazard — the opposite of the
# intent — so only real invocations count.
check "ps -o comm= is invoked once only (proxy_running, which wants the truncated name)" "1" \
	"$(grep -v '^[[:space:]]*#' "${INSTALL_SH}" | grep -c 'ps .*-o comm=\|ps -o comm=')"

# --- the new-CA notice is gated on the CA CHANGING, not on ca.crt existing ---
#
# An existence check has a false negative that matters: EnsureFileSource mints when ANY
# of tls.crt / tls.key / ca.crt is missing, not only when all three are, so a directory
# holding ca.crt but no tls.key gets a brand-new CA while "ca.crt exists" reports the
# machine already had one — suppressing the notice in exactly the case that needs it.
# Verified against the real proxy: removing tls.key produced a different ca.crt hash.

_fp_fn=$(sed -n '/^ca_fingerprint()/,/^}/p' "${INSTALL_SH}")
check "the CA gate hashes rather than testing existence" "1" \
	"$(printf '%s' "${_fp_fn}" | grep -cE 'shasum|sha256sum' >/dev/null && echo 1 || echo 0)"
check "the notice compares before against after" "1" \
	"$(grep -c 'ca_fp_before}" != "${ca_fp_after' "${INSTALL_SH}" || true)"
check "no existence-only gate remains" "0" \
	"$(grep -c 'ca_existed' "${INSTALL_SH}" || true)"

# The fingerprint helper must not abort the installer when no checksum tool exists: the
# caller assigns it bare, and a non-zero status there dies under set -e.
_probe="${TMP}/fp.sh"
{
	printf 'set -eu\nca_dir=%s\n' "${TMP}/noca"
	printf '%s\n' "${_fp_fn}"
	printf 'fp="$(ca_fingerprint)"\nprintf "survived:[%%s]" "${fp}"\n'
} >"${_probe}"
mkdir -p "${TMP}/noca"
check "fingerprint of a missing CA is empty and does not abort" "survived:[]" "$(sh "${_probe}" 2>/dev/null)"
: >"${TMP}/noca/ca.crt"
check "fingerprint of a present CA is non-empty" "1" \
	"$(sh "${_probe}" 2>/dev/null | grep -c 'survived:\[.\+\]' || true)"

printf '\n%s passed, %s failed\n' "${PASS}" "${FAIL}"
[ "${FAIL}" = "0" ]
