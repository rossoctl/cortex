#!/bin/sh
# install.sh — one-line installer for Cortex on a local machine.
#
#   curl -fsSL https://www.rossoctl.dev/cortex/install.sh | sh
#
# Detects your OS/arch, downloads the prebuilt `agentop` and `cortex` binaries for
# the newest stable release, verifies their SHA-256 checksums, and stages them in a
# temporary directory. Then it hands off to `agentop setup --from <stage>`, which
# lists what it will change, asks once, installs to ~/.local/bin, starts Cortex with
# its built-in config in ~/.cortex, undoes what it did if a step fails, and deletes
# the stage. macOS + Linux, amd64 + arm64. No cluster, Keycloak, or SPIRE needed.
#
# This script keeps only what has to happen before there is an agentop to run: the
# bootstrap below, the download, the checksums, and the extract. It prints little of
# its own: the download's progress (on a terminal a title and a bar, which stay on
# screen; one plain line otherwise), errors, and a few warnings and notices from those
# steps, such as a dump helper it could not fetch or a TMPDIR that would not run
# programs. The rest comes from setup. Traffic is decrypted and parsed for viewing;
# nothing is rewritten.
#
# Options (pass through the pipe with `sh -s --`, e.g.
#   curl -fsSL ...install.sh | sh -s -- --install-only):
#
#   --install-only     install the binaries and PATH, and stop
#   --claude-code      also route Claude Code through Cortex, so it runs as plain
#                      `claude`. Setup asks before changing anything.
#   --no-modify-path   never edit a shell profile
#
# The rest are in `--help`. Each one this script shares with setup is passed on.
#
# There is deliberately only one config. It carries the parsers AND tool-prune,
# and the proxy preserves edits to it, so a second "cost-optimised" config had
# nothing to do that filling in one list did not already do — while costing a
# second CA, a second set of paths, and a second page of instructions that read
# identically to the first.
#
# No compatibility aliases here: this script accepted no flags at all until now,
# so there is no earlier spelling for anyone to still be using. (The proxy's
# --demo -> --local alias is different: that flag really did ship.)
#
# Flags rather than env vars: written `VAR=1 curl ... | sh` the variable reaches
# curl, not sh, so the script runs without it — the failure mode is silent, and
# `sh -s -- --flag` does not have it. That is why the env aliases for --ref and
# --install-only were removed rather than kept as a second spelling: they were the
# form most likely to be typed and least likely to work.
#
# By default this script re-runs the copy from the newest stable RELEASE rather than
# executing whatever is currently on main — main is unstable by definition, and a
# `curl | sh` should not be the first thing to run a change nobody has released.
# --ref=main opts back in; --ref=vX.Y.Z pins, and is the only way to get a release
# candidate (--ref=vX.Y.Z-rc.N), which the default never picks.
#
# Every path here installs a RELEASE. To install what is in a checkout instead,
# use `make dev-install` from the repo root: it compiles both binaries to ./bin and
# runs the same handoff, `./bin/agentop setup --from ./bin`. --ref=main is not that —
# it only chooses which copy of this script runs, and that copy still downloads a
# build.
#
# Environment (maintainer testing only — not part of the documented interface):
#   AUTHBRIDGE_SKIP_DOWNLOAD=1  download nothing; hand the agentop already in
#                               ~/.local/bin to `agentop setup`, which re-checks
#                               and repairs what is installed
# set -eu, not -euo pipefail: this is POSIX sh (the documented entry point is
# `curl ... | sh`), and `pipefail` is a bashism that would abort the script under
# dash/ash. The repo-wide `set -euo pipefail` convention applies to bash scripts.
set -eu

REPO="rossoctl/cortex"
# CHANNEL_TAG is the release the developer channel's assets hang off. Deliberately
# NOT "main": a GitHub release needs a git tag, and a tag named `main` would collide
# with the branch. Verified consequences of that collision — `git rev-parse main`
# resolves to the TAG, not the branch, and every git command warns "refname 'main' is
# ambiguous". The tag would also freeze at the first publish (uploading assets does
# not move it) while the branch moved on, so anything resolving `main` as a revision
# would silently read a stale commit. `--ref=main` is still what people type; only
# the tag underneath differs.
CHANNEL_TAG="main-latest"
BIN_DIR="${HOME}/.local/bin"
# Every file Cortex writes for this user lives here: config, CA, keys, logs,
# pidfiles. One directory to inspect, back up, or delete.
CORTEX_DIR="${HOME}/.cortex"

# SUPERVISOR_NAME is the human label --stop names the service by.
case "$(uname -s)" in
	Darwin) SUPERVISOR_NAME="launchd user agent" ;;
	*) SUPERVISOR_NAME="systemd user unit" ;;
esac

info() { printf '%s\n' "$*"; }
# A warning or an error ends the download bar's line first when one is open, so the
# message starts on a line of its own. bar_shown is set only once bar_end is defined.
warn() { [ -z "${bar_shown:-}" ] || bar_end; printf 'warning: %s\n' "$*" >&2; }
die()  { [ -z "${bar_shown:-}" ] || bar_end; printf 'error: %s\n' "$*" >&2; exit 1; }

# There is deliberately NO sandbox-detection probe here. We never try to GUESS whether
# a sandbox will block launchd/systemd or /tmp — a passive probe cannot tell (a
# permissive `(allow default)` seatbelt profile is a real sandbox that still lets
# launchctl and /tmp work, while a restrictive one does not), and the earlier
# heuristics (/tmp writability, `getconf DARWIN_USER_DIR` returning EIO) reported the
# wrong thing under a permissive profile. The reliable kernel-level answer,
# sandbox_check(getpid(), NULL, 0) == 1 (what Chromium's Seatbelt::IsSandboxed uses),
# tells you that you ARE sandboxed but NOT whether any given operation is denied —
# which is the only thing this installer cares about.
#
# So instead of detecting and pre-deciding, we ATTEMPT each restricted operation and
# handle what actually fails: ensure_tmpdir writes-then-falls-back to a scratch dir
# under ~/.cortex, the exec probe re-stages there when TMPDIR will not run programs,
# and agentop setup does the same for the supervisor (a real `launchctl list` /
# `systemctl --user` probe, then a plain background process). Nothing assumes the
# sandbox is restrictive; nothing assumes it is permissive either.

# ensure_tmpdir guarantees TMPDIR names a directory we can actually write, and is
# exported. mktemp (used by the bootstrap and the download) and the release-tag scratch
# file all honour TMPDIR; in a sandbox that denies /tmp, an unset or /tmp-based TMPDIR
# makes every one of them fail. Falling back under CORTEX_DIR keeps all scratch state in
# the one directory this installer already owns.
#
# An existing TMPDIR is used AS-IS — never overwritten with a normalised copy. We only
# choose a value when the environment gave us none. Either way TMPDIR is exported on the
# success path, so a later "${TMPDIR}" is safe under `set -u` even where the environment
# never set it (routine on Linux; launchd hides this on macOS by setting a per-user one).
ensure_tmpdir() {
	# _probe is only for the write test — mkdir (atomic, fails on a pre-existing path)
	# rather than a truncating `: >`, so a planted symlink at the predictable name cannot
	# be followed (CWE-59). Strip a trailing slash from the probe path only, so a TMPDIR
	# like "/tmp/" does not become "/tmp//.cortex-w..."; TMPDIR itself is left untouched.
	if [ -n "${TMPDIR:-}" ]; then
		_probe="${TMPDIR%/}/.cortex-w.$$.d"
		if mkdir "${_probe}" 2>/dev/null; then
			rmdir "${_probe}"
			export TMPDIR
			return 0
		fi
	else
		# No TMPDIR in the environment: adopt the conventional /tmp if it is writable.
		if mkdir "/tmp/.cortex-w.$$.d" 2>/dev/null; then
			rmdir "/tmp/.cortex-w.$$.d"
			TMPDIR="/tmp"
			export TMPDIR
			return 0
		fi
	fi
	TMPDIR="${CORTEX_DIR}/tmp"
	mkdir -p "${TMPDIR}" 2>/dev/null || die "no writable TMPDIR (${TMPDIR})"
	export TMPDIR
	info "Using ${TMPDIR} for temporary files (the default was not writable)."
}

# usage is a heredoc rather than sed over "$0": piped as `curl ... | sh -s -- --help`
# the script has no file to read ($0 is "sh"), so the previous version printed
# nothing at all — for the one flag someone is most likely to try before running an
# installer they piped from the internet.
usage() {
	cat <<'USAGE'
install.sh — install Cortex on a local machine (macOS/Linux, amd64/arm64).

Usage:
  curl -fsSL https://www.rossoctl.dev/cortex/install.sh | sh
  curl -fsSL ...install.sh | sh -s -- [option]

Downloads agentop and cortex for the newest stable release, verifies their checksums
and stages them, then hands off to `agentop setup`. Setup lists what it will change,
asks once, installs to ~/.local/bin, and starts the proxy with its built-in config
in ~/.cortex. Traffic is decrypted and parsed for viewing; nothing is rewritten.

Options:
  --install-only     install the binaries and PATH, and stop
  --claude-code      also configure Claude Code to use it, so it runs as plain
                     `claude` with no environment variables
  --local            the default, spelled out
  --no-service       do not use the OS service supervisor (launchd/systemd); run
                     the proxy directly as a background process instead. Chosen
                     automatically when the supervisor turns out to be unavailable
                     (e.g. a macOS seatbelt sandbox that blocks launchctl)
  --no-modify-path   never edit a shell profile; only say so when ~/.local/bin is
                     not on PATH
  --stop             stop a running Cortex and exit — the supervised service if one
                     is installed, and the background proxy from its pidfile. Does
                     not download, install, or start anything. Safe to re-run.
  --yes, -y          do not ask; apply the changes setup lists
  --ref=REF          install from a git ref instead of the newest stable release —
                     both this script and the binaries (e.g. --ref=main for
                     unreleased changes, --ref=v0.8.1 to pin). The only way to
                     get a release candidate: --ref=v0.9.0-rc.1
  -h, --help         this text

Undo any time:
  agentop uninstall

After installing, to cut Claude Code's token cost:
  agentop tools scan --write ~/.cortex/config.yaml   (proposes which tools to prune)
Then watch the $ saved on every prompt Claude Code sends, live in:
  agentop

The session store is memory-only. To write it to files before it is lost:
  cortex-session-dump --out ./cortex-dump
USAGE
}

# --- mode selection ---
MODE=local
WIRE_CLAUDE_CODE=""
ASSUME_YES=""
WANT_REF=""
# NO_SERVICE forces the proxy to run as a plain background process instead of under
# the OS supervisor. There is no sandbox flag and no auto-detection: we attempt each
# restricted operation (the supervisor, a writable TMPDIR) and fall back on whatever
# actually fails, rather than deciding up front that this is a sandbox.
NO_SERVICE=""
# NO_MODIFY_PATH keeps setup out of every shell profile. This loop runs before the
# bootstrap, so main's copy must accept the flag for a piped install to pass it on.
NO_MODIFY_PATH=""
for arg in "$@"; do
	case "$arg" in
		--install-only) MODE=install-only ;;
		--claude-code) WIRE_CLAUDE_CODE=1 ;;
		--yes | -y) ASSUME_YES=1 ;;
		--ref=*) WANT_REF="${arg#*=}" ;;
		--no-service) NO_SERVICE=1 ;;
		--no-modify-path) NO_MODIFY_PATH=1 ;;
		--stop) MODE=stop ;;
		# --local is the default; accepted so writing it out explicitly works, and
		# so it mirrors the proxy flag of the same name.
		--local) MODE=local ;;
		-h | --help)
			usage
			exit 0
			;;
		*) die "unknown option: $arg (try --claude-code, --install-only, --local, --no-modify-path, --no-service, --stop, --ref=REF, --yes, or no argument)" ;;
	esac
done
# Removed knobs die rather than being ignored. Left set in someone's shell,
# AUTHBRIDGE_INSTALL_ONLY=1 would silently do a FULL install and AUTHBRIDGE_VERSION
# would silently install the newest release instead of the pin. Both are wrong answers,
# and this script's whole standard is that a surprise becomes an error instead. Each
# message names the flag that replaced it, echoing the value back so the fix is
# copy-pasteable.
[ -z "${AUTHBRIDGE_INSTALL_ONLY:-}" ] || die "AUTHBRIDGE_INSTALL_ONLY is no longer read. Pass --install-only instead."
[ -z "${AUTHBRIDGE_VERSION:-}" ] || die "AUTHBRIDGE_VERSION is no longer read. Pass --ref=${AUTHBRIDGE_VERSION} instead."
[ -z "${AUTHBRIDGE_REF:-}" ] || die "AUTHBRIDGE_REF is no longer read. Pass --ref=${AUTHBRIDGE_REF} instead."

ensure_tmpdir

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v tar  >/dev/null 2>&1 || die "tar is required"

# newest_release prints the tag of the release GitHub marks Latest: the newest STABLE
# release. GitHub never gives that badge to a prerelease, and release-binaries.yaml
# publishes every -rc, -alpha and -beta tag as one, the main-latest channel too — so a
# release candidate reaches only someone who names it with --ref.
#
# This used to list releases and take the newest, whatever its flags. That was right
# while every release was a prerelease, when Latest named a tag from January; once
# stable releases existed, it handed every release candidate to everyone running the
# plain one-liner.
#
# TWO sources, because one was not enough. api.github.com allows 60 requests per hour
# per IP unauthenticated, shared by everyone behind one NAT. Exhausting it killed the
# documented one-liner outright and told the person to go find a version and pass
# --ref, which is the opposite of a one-line install. Observed on a normal laptop, not
# contrived. So the redirect, which that quota does not cover, goes first.
#
# The shape check is a backstop: each source prints a version tag or nothing. Reaching
# it EMPTY means neither answered — an error page, a rate-limit body, a schema change —
# so fail rather than build a download URL out of it. The caller dies with actionable
# advice.
newest_release() {
	_tag=$(release_tag_from_redirect)
	[ -n "${_tag}" ] || _tag=$(release_tag_from_api)
	case "${_tag}" in
		v[0-9]*) ;;
		*) return 1 ;;
	esac
	printf '%s\n' "${_tag}"
}

# release_tag_from_redirect prints the Latest release's tag, read off where
# github.com/<repo>/releases/latest redirects, or nothing.
#
# The redirect target is the answer, so it is not followed (no -L) and the page behind
# it is never fetched. Only this repository's tag page counts: a repository with no
# Latest release redirects to /releases instead, and neither that nor anything else
# may turn into a download URL.
release_tag_from_redirect() {
	_loc=$(curl -s -o /dev/null -w '%{redirect_url}' \
		"https://github.com/${REPO}/releases/latest" 2>/dev/null) || _loc=""
	case "${_loc}" in
		"https://github.com/${REPO}/releases/tag/"v[0-9]*)
			printf '%s\n' "${_loc#"https://github.com/${REPO}/releases/tag/"}"
			;;
	esac
}

# release_tag_from_api prints the Latest release's tag per the releases API, or nothing.
#
# Not jq (not installed everywhere) and not gh (a far larger dependency than a curl|sh
# installer should require; this script needs curl, tar and a checksum tool).
release_tag_from_api() {
	# The status is captured rather than discarded so a 403 can be NAMED. Nearly always
	# that is the unauthenticated rate limit, which is a wait-or-pin situation and not a
	# bug — and the old message said only "could not resolve the newest release", which
	# told nobody that waiting would fix it.
	_body="${TMPDIR:-/tmp}/cortex-rel.$$"
	_code=$(curl -sSL -o "${_body}" -w '%{http_code}' \
		"https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null) || _code="000"
	if [ "${_code}" = "403" ] || [ "${_code}" = "429" ]; then
		# Only worth saying if it is really the quota; a 403 for another reason should
		# not be mislabelled.
		if grep -q 'rate limit' "${_body}" 2>/dev/null; then
			warn "GitHub's API rate limit for this network is exhausted (60/hour per IP, unauthenticated)"
		fi
	fi
	# Four steps, each doing one thing that cannot silently go wrong:
	#
	#   tr ',{}' '\n'   put every JSON field on its own line, so the parse does not
	#                   depend on how GitHub formats the response: pretty-printed or
	#                   compact, each field is a line, and nothing greedy can run past
	#                   the field it was aimed at.
	#   grep '"tag_name":'
	#                   match tag_name only where it is a KEY (anchored, colon after),
	#                   never text inside a value.
	#   cut -d'"' -f4   take the value by position, not by pattern.
	#   grep -m1 '^v[0-9]'
	#                   only a version tag is an answer.
	if [ "${_code}" = "200" ]; then
		tr ',{}' '\n' <"${_body}" \
			| grep '^[[:space:]]*"tag_name"[[:space:]]*:' \
			| cut -d'"' -f4 \
			| grep -m1 '^v[0-9]' || true
	fi
	rm -f "${_body}"
}

# resolve_version prints the release tag whose binaries should be installed, given the
# ref the USER asked to install (VERSION_REF). Not the ref this script came from —
# conflating those two is what let the default one-liner reach the channel, so the
# distinction is worth keeping visible in the name.
#
# One rule: --ref=X installs X. That was not true before — `--ref=v0.7.0-alpha.4`
# set script and binaries, while `--ref=main` set only the script, because there
# was no `main` release to download from. Now that the developer channel publishes
# one, the special case disappears rather than growing a second flag.
resolve_version() { # version_ref
	# Takes VERSION_REF, not SCRIPT_REF. Empty means "nobody named anything
	# installable" — resolve the newest release, and fail loudly if that is not
	# possible. It must never mean "fall back to the channel": rate limiting alone
	# would then hand unreleased builds to people who ran the plain one-liner.
	#
	# Both channel spellings land on CHANNEL_TAG. The assets live there rather than on
	# a tag called `main` — see its definition for why — so the ref someone types, the
	# tag the assets hang off, and the binary's own stamp are three different strings,
	# deliberately.
	case "$1" in
		main | "${CHANNEL_TAG}") printf '%s\n' "${CHANNEL_TAG}" ;;
		v*) printf '%s\n' "$1" ;;
		*)
			# A ref that is neither channel nor release tag: a branch, or a SHA. No
			# binaries are published for those, so the newest release is the only
			# option — but say so. Silent, this is byte-identical to the plain
			# one-liner, and someone testing a feature branch gets that branch's
			# SCRIPT against release BINARIES with nothing to attribute it to. Same
			# principle the 404 fallback states: name the surprise.
			[ -z "$1" ] || warn "no binaries are published for ${1}; using this script from ${1} with binaries from the newest release"
			# >&2 deliberately: this function's stdout IS the resolved version, and
			# info() writes to stdout (see its definition above). Without the
			# redirect the progress line lands inside `version` and corrupts every
			# download URL.
			info "Resolving newest release..." >&2
			newest_release || return 1
			;;
	esac
}

# ere_escape quotes the ERE metacharacters in a literal so it matches exactly.
# Archive names contain dots, and an unescaped "." matches any character: the
# pattern for agentop_v0.7.0-alpha.3_..tar.gz also accepted
# agentop_v0X7X0-alpha_3_..Xtar.gz. Nothing exploitable followed — the count check
# or sha_check rejected it — but this script's whole subject is precision here.
ere_escape() {
	# shellcheck disable=SC2016 # the sed script is literal on purpose
	printf '%s' "$1" | sed 's/[].[^$()*+?{}|\\]/\\&/g'
}

# --- run the released copy of this script, not the one from main ---
#
# The documented command fetches this file from main, which is whatever landed
# last: an unreviewed or half-finished change there runs on someone's laptop
# immediately. Releases are tested, so by default this bootstrap re-runs the copy
# from the newest release and hands it the same arguments.
#
# SCRIPT_REF names the ref this copy came from and doubles as the recursion guard:
# the child sees it set and does not bootstrap again.
SCRIPT_REF="${AUTHBRIDGE_SCRIPT_REF:-}"
# VERSION_REF answers "what did the user ask to INSTALL". SCRIPT_REF answers a
# different question — "which copy of this script is running" — and the two diverge on
# every fallback below. Reading one for the other is how the default one-liner ended up
# able to install channel binaries: three separate situations all set SCRIPT_REF=main,
# and only one of them was a request for unreleased builds.
VERSION_REF="${SCRIPT_REF}"
# Decide whether to re-exec the RELEASED copy of this script. That bootstrap exists for
# the documented `curl … | sh` pipe: the code arrives over stdin ($0 is "sh", no file
# on disk), and running the newest TESTED release beats running whatever landed on main.
#
# It must NOT fire when the script is run as a LOCAL FILE — a clone, or a fix under test
# (`./install.sh`, `sh install.sh`). That file is what the user chose to run; silently
# re-fetching the newest release and running THAT instead is why a fix in a cloned repo
# did nothing — the bootstrap quietly ran the OLD released code instead, and it died. `[ -f "$0" ] && [ -r "$0" ]` is the discriminator: a readable file at $0 means a
# real local script (skip the re-exec), while the pipe leaves $0 as "sh" with no such
# file. Also skipped: offline (AUTHBRIDGE_SKIP_DOWNLOAD=1, nothing to fetch), --stop
# (fetches nothing, and the released target predates the flag), and the re-exec'd child
# or an AUTHBRIDGE_SCRIPT_REF pin (SCRIPT_REF already set).
#
# install_test.sh slices from `SCRIPT_REF=...` to the first line that is exactly `fi`,
# so keep every `fi` below indented until the re-exec block's own closing `fi`.
_reexec=1
[ -n "${SCRIPT_REF}" ] && _reexec=""
[ "${AUTHBRIDGE_SKIP_DOWNLOAD:-}" = "1" ] && _reexec=""
[ "${MODE}" = "stop" ] && _reexec=""
{ [ -f "$0" ] && [ -r "$0" ]; } && _reexec=""
# When we are NOT re-execing and no script ref was pinned, the binaries still follow
# --ref (WANT_REF), or the newest release when it is empty — so `--ref=X` selects X's
# binaries even though this local script, not X's, is the one running.
[ -z "${_reexec}" ] && [ -z "${SCRIPT_REF}" ] && VERSION_REF="${WANT_REF}"
if [ -n "${_reexec}" ]; then
	want_ref="${WANT_REF:-}"
	if [ -z "${want_ref}" ]; then
		want_ref="$(newest_release)" || true
	fi
	if [ -z "${want_ref}" ]; then
		# Could not ask: offline, rate-limited, 5xx. Fall back to THIS copy of the
		# script, but deliberately NOT to channel binaries. Whoever ran the plain
		# one-liner asked for a release, so VERSION_REF is cleared and resolution below
		# still hunts for the newest v-tag — failing loudly if it cannot find one,
		# which beats silently installing an unreleased build.
		warn "could not resolve the newest release; continuing with the copy from main"
		SCRIPT_REF="main"
		VERSION_REF=""
	elif [ "${want_ref}" = "main" ] || [ "${want_ref}" = "${CHANNEL_TAG}" ]; then
		# Explicitly asked for the channel, by either name. `main` is the documented
		# spelling; CHANNEL_TAG is what the Releases page shows, so someone who saw the
		# title there will type that instead and must get the same thing. This copy
		# already is main — piped from www.rossoctl.dev, main as of the site's last
		# deploy — so there is nothing to re-exec.
		SCRIPT_REF="main"
		VERSION_REF="main"
	else
		# Rebuild the argument list without --ref: it is meta, consumed here, and a
		# released script from before --ref existed rejects it as an unknown option.
		# Rotating the positional parameters keeps arguments with spaces intact,
		# which building a string would not.
		argc=$#
		argi=0
		while [ "${argi}" -lt "${argc}" ]; do
			a="$1"
			shift
			argi=$((argi + 1))
			case "$a" in
				--ref=*) ;;
				*) set -- "$@" "$a" ;;
			esac
		done

		boot=$(mktemp)
		# Three paths, because a pinned ref may predate this script's move under
		# scripts/ (it lived at the repository root from the #1134 flatten until
		# then) or predate the flatten itself (authbridge/install.sh). Try the
		# current layout first so a current ref never pays for a legacy probe; fall
		# back only on a clean 404, never on a transport error.
		url="https://raw.githubusercontent.com/${REPO}/${want_ref}/scripts/install.sh"
		# Capture the status code rather than collapsing every failure into one
		# branch. A 404 means that ref genuinely predates this script — fall back.
		# A transport error means we could not ask, and silently dropping to main
		# there would break the exact guarantee this bootstrap exists to give.
		# On a transport failure curl still prints "000" via -w AND exits non-zero,
		# so appending our own default produced "HTTP 000000". Overwrite instead.
		http=$(curl -sSL -o "${boot}" -w '%{http_code}' "${url}" 2>/dev/null) || http="000"
		[ -n "${http}" ] || http="000"
		if [ "${http}" = "404" ]; then
			url="https://raw.githubusercontent.com/${REPO}/${want_ref}/install.sh"
			http=$(curl -sSL -o "${boot}" -w '%{http_code}' "${url}" 2>/dev/null) || http="000"
			[ -n "${http}" ] || http="000"
		fi
		if [ "${http}" = "404" ]; then
			url="https://raw.githubusercontent.com/${REPO}/${want_ref}/authbridge/install.sh"
			http=$(curl -sSL -o "${boot}" -w '%{http_code}' "${url}" 2>/dev/null) || http="000"
			[ -n "${http}" ] || http="000"
		fi
		if [ "${http}" = "200" ] && [ -s "${boot}" ]; then
			# Not announced: once the child resolves the release, its "Downloading vX" or
			# "Already at vX" line names it, so saying the same thing first was noise.
			#
			# set -e would abort the parent on a non-zero child before any of the
			# lines below ran, leaking the downloaded script on every failed
			# install. The if/else keeps the status and still cleans up.
			#
			# A signal would too. A ^C during setup reaches this shell as well as the
			# child, which catches it: at the prompt it exits 3, and mid-install it
			# rolls back first. dash dies of an INT it does not catch even while its
			# child handles it, so the user's shell got 130, the script stayed in
			# TMPDIR, and the prompt could come back while setup was still printing. A
			# TERM to the group, or a closed terminal's HUP, kills bash that way too,
			# not only dash. Caught, each waits here for the child, and this shell
			# exits with its status. A no-op, not '': an ignored signal is inherited,
			# and the child could not catch it then.
			trap : INT TERM HUP
			if AUTHBRIDGE_SCRIPT_REF="${want_ref}" sh "${boot}" "$@"; then
				status=0
			else
				status=$?
			fi
			rm -f "${boot}"
			exit "${status}"
		fi
		rm -f "${boot}"
		if [ "${http}" = "404" ]; then
			# A release from before this script existed under that name. Falling
			# back beats refusing to install, but name the copy that is running so
			# a surprise is attributable.
			#
			# VERSION_REF keeps the pin: running main's SCRIPT is the fallback,
			# changing which BINARIES get installed is not. `--ref=X installs X`
			# has to survive this branch or the flag means nothing here.
			warn "${want_ref} has no install.sh (HTTP 404); continuing with the copy from main"
			SCRIPT_REF="main"
			VERSION_REF="${want_ref}"
		else
			# Blocked, offline, rate-limited, proxied, 5xx. We cannot tell whether a
			# released installer exists, so do not quietly run main instead.
			die "could not fetch the installer for ${want_ref} (HTTP ${http}) from ${url}.
  Check the network, or choose explicitly:
    --ref=main       run the copy from main (unreleased changes)
    --ref=vX.Y.Z     use a specific release"
		fi
	fi
fi

# Verify the checklist file passed as $1 (run from the directory holding the
# files). shasum is preferred: it's always present on macOS and its -c reads the
# GNU-style checksums.txt reliably, whereas some non-GNU sha256sum builds reject
# -c. Linux without shasum falls back to sha256sum (GNU coreutils).
sha_check() {
	if command -v shasum >/dev/null 2>&1; then
		shasum -a 256 -c "$1"
	elif command -v sha256sum >/dev/null 2>&1; then
		sha256sum -c "$1"
	else
		die "need shasum or sha256sum to verify downloads"
	fi
}

# Demo listener ports — loopback, and deliberately uncommon to avoid colliding
# with common dev tools. Keep in sync with the built-in config in
# cmd/cortex/local.go. --stop checks each one is free once it has stopped Cortex.
DEMO_FORWARD_PORT=47600
DEMO_SESSION_PORT=47601
DEMO_STATS_PORT=47602
DEMO_HEALTH_PORT=47604

# port_in_use exits 0 if something is already listening on the given loopback port.
# Best-effort across platforms: lsof (macOS + many Linux), then ss (iproute2, the
# default on modern Linux where lsof is often not installed), then nc; if none
# exists, it assumes free. lsof is absent in some macOS sandboxes and ss is absent
# on macOS, so trying all three is what makes one probe work everywhere.
port_in_use() {
	if command -v lsof >/dev/null 2>&1; then
		lsof -nP -iTCP@127.0.0.1:"$1" -sTCP:LISTEN >/dev/null 2>&1
	elif command -v ss >/dev/null 2>&1; then
		# -Hlnt: no header, LISTEN state, numeric, TCP. `sport = :$1` filters by port
		# but ignores the local ADDRESS, so restrict the Local Address:Port column ($4)
		# to loopback (IPv4 127.0.0.1 or IPv6 [::1]) or a wildcard bind — a listener on
		# an external interface only does not make the loopback port unavailable, and
		# matching it would make the health poll return early and stop_cortex warn about
		# a proxy that is not there. IPv6 loopback matters because the proxy may bind
		# ::1 only: ss prints that as [::1]:PORT, which the IPv4 and wildcard patterns
		# both miss, so without this the health poll would spin its full 10s.
		ss -Hlnt "sport = :$1" 2>/dev/null \
			| awk -v p=":$1" '$4 ~ ("(^|[^0-9])127\\.0\\.0\\.1"p"$")||($4 ~ ("^\\[::1\\]"p"$")||($4 ~ ("^(0\\.0\\.0\\.0|\\*|\\[::\\]|::)"p"$"))){f=1} END{exit !f}'
	elif command -v nc >/dev/null 2>&1; then
		nc -z 127.0.0.1 "$1" >/dev/null 2>&1
	else
		return 1
	fi
}

# pid_exe_path prints the full executable path of a pid, or nothing when it cannot
# be resolved. `ps -o comm=` is NOT usable here: on Linux `comm` is the kernel's
# comm field — argv[0]'s basename capped at 15 chars (TASK_COMM_LEN-1) — so it
# prints a bare name (the old 16-char one came out cut to 15) and never a path. Only macOS
# prints a path there. The callers below compare against a full install path, so
# a truncated name would compare unequal every time.
#
# Two sources, in order of trustworthiness: /proc/<pid>/exe is the kernel's own
# link to the running image on Linux, and lsof's `txt` descriptor is the macOS
# equivalent. Both name the executable itself. `ps -o args=` is the last resort
# and the weakest — argv[0] is whatever the caller chose, may be relative, and a
# path containing spaces cannot be recovered from it — so it is used only to
# avoid returning nothing at all.
pid_exe_path() { # pid
	# -h (the symlink itself), NOT -r: `-r` follows the link, and the link dangles
	# in exactly the case that matters most — a binary replaced under a running
	# process, which is what an upgrade does and when this code runs. Testing -r
	# there would skip the kernel's own answer and fall through to a weaker source.
	if [ -h "/proc/$1/exe" ] && _pep=$(readlink "/proc/$1/exe" 2>/dev/null) \
		&& [ -n "${_pep}" ]; then
		# A replaced/deleted binary reads as "<path> (deleted)"; keep the path.
		printf '%s\n' "${_pep% (deleted)}"
		return 0
	fi
	if command -v lsof >/dev/null 2>&1; then
		# -d txt is the mapped executable; -Fn gives one n<name> line per record,
		# stable across lsof versions where the columnar output is not.
		#
		# -a is REQUIRED, not decoration: lsof ORs its list-selection options by
		# default, so `-p <pid> -d txt` means "files of this pid OR any txt
		# descriptor on the system" — which lists every process's executable, and
		# `head -1` would then take whichever came first. On macOS this is the
		# source foreign_proxy_holder judges, so a stray first record would name
		# some other binary and classify our own managed proxy as foreign.
		# (-sTCP:LISTEN elsewhere needs no -a: a state list is a filter, not an
		# ORed selection set.)
		_pep=$(lsof -p "$1" -a -d txt -Fn 2>/dev/null | sed -n 's/^n//p' | head -1)
		[ -n "${_pep}" ] && { printf '%s\n' "${_pep}"; return 0; }
	fi
	_pep=$(ps -p "$1" -o args= 2>/dev/null | head -1)
	_pep=${_pep%% *}
	[ -n "${_pep}" ] && { printf '%s\n' "${_pep}"; return 0; }
	return 1
}

# Where the unsupervised proxy records its pid, so a service-less install still has
# exactly one process to find, check, and stop.
PROXY_PIDFILE="${CORTEX_DIR}/proxy.pid"

# proxy_running exits 0 if the proxy we recorded in the pidfile is still alive AND is
# actually our proxy. Validating both matters because stop_cortex signals this pid:
#   - a non-numeric or negative pidfile would make `kill` parse its argument wrong;
#   - after an unclean shutdown the OS can recycle the pid onto an unrelated process
#     of the same user, which we must not SIGTERM/SIGKILL.
# Where `ps` can name the process we require its basename to be exactly cortex — the
# same check agentop's runningPID uses, so `agentop service install` and this script agree
# on what counts as "our proxy". Exact, not a pattern: macOS prints the full path, and
# a path under ~/.cortex or any checkout of this repo contains the word, so a looser
# match would claim a stranger's pid for stop_cortex to signal. Where the sandbox hides processes from `ps`, ps prints
# nothing and the pidfile remains the only handle, so we keep the kill -0 result.
proxy_running() {
	_pid=$(cat "${PROXY_PIDFILE}" 2>/dev/null) || return 1
	case "${_pid}" in
		"" | *[!0-9]*) return 1 ;;
	esac
	kill -0 "${_pid}" 2>/dev/null || return 1
	_comm=$(ps -o comm= -p "${_pid}" 2>/dev/null) || return 0
	[ -z "${_comm}" ] && return 0
	case "${_comm##*/}" in
		cortex) return 0 ;;
		*) return 1 ;;
	esac
}

pre_rename_proxy_running() {
	_pid=$(cat "${PROXY_PIDFILE}" 2>/dev/null) || return 1
	case "${_pid}" in
		"" | *[!0-9]*) return 1 ;;
	esac
	kill -0 "${_pid}" 2>/dev/null || return 1
	_pre_exe=$(pid_exe_path "${_pid}") || return 1
	_pre_old="${BIN_DIR}/authbridge-proxy"
	[ "${_pre_exe}" = "${_pre_old}" ] && return 0
	command -v readlink >/dev/null 2>&1 || return 1
	_pre_a=$(readlink -f "${_pre_exe}" 2>/dev/null || true)
	_pre_b=$(readlink -f "${_pre_old}" 2>/dev/null || true)
	[ -n "${_pre_a}" ] && [ "${_pre_a}" = "${_pre_b}" ]
}

stop_pidfile_proxy() {
	_pid=$(cat "${PROXY_PIDFILE}")
	info "Stopping the background proxy (pid ${_pid})..."
	kill "${_pid}" 2>/dev/null || true
	# Wait longer than the proxy's own 15s graceful drain before escalating, so a
	# normal shutdown is never cut short into a SIGKILL that drops in-flight
	# requests. This matches agentop's stopPID, which waits 18s for the same reason.
	_i=0
	while [ "${_i}" -lt 18 ] && kill -0 "${_pid}" 2>/dev/null; do
		_i=$((_i + 1))
		sleep 1
	done
	if kill -0 "${_pid}" 2>/dev/null; then
		warn "pid ${_pid} did not exit after 18s; sending SIGKILL"
		kill -9 "${_pid}" 2>/dev/null || true
	fi
	rm -f "${PROXY_PIDFILE}"
}

# stop_cortex stops a running Cortex — the supervised service if agentop installed one,
# and the background proxy recorded in the pidfile — then reports on the ports so
# "stopped" is verified rather than assumed. Re-runnable and safe: with nothing
# running it says so and exits 0 rather than failing. This backs `install.sh --stop`,
# and matters most in a sandbox, where `ps`/`pkill` are blind and the pidfile is the
# only reliable handle on the process.
stop_cortex() {
	_stopped=""
	# Supervised: hand it back to agentop, which owns the launchd/systemd unit. `service
	# stop` exits non-zero with "no service installed" when there is none — that is a
	# normal state here, not an error, so only a DIFFERENT failure is surfaced.
	if [ -x "${BIN_DIR}/agentop" ]; then
		if _svc_out=$("${BIN_DIR}/agentop" service stop 2>&1); then
			info "Stopped the supervised service (${SUPERVISOR_NAME})."
			_stopped=1
		elif ! printf '%s' "${_svc_out}" | grep -qi "no service installed"; then
			warn "agentop service stop reported: ${_svc_out}"
		fi
	fi
	# Unsupervised: kill the process recorded in the pidfile — the only handle that
	# works where the sandbox hides other processes from ps/pkill.
	if proxy_running || pre_rename_proxy_running; then
		stop_pidfile_proxy
		_stopped=1
	elif [ -f "${PROXY_PIDFILE}" ]; then
		# A stale pidfile from a proxy that already died: clear it so status stays honest.
		rm -f "${PROXY_PIDFILE}"
	fi
	# Verify against the ports rather than trusting the kill. A port still held after
	# we stopped what we know about means a foreign proxy this script did not start —
	# worth naming, since in a sandbox it cannot be found through ps.
	_busy=""
	for _p in "${DEMO_FORWARD_PORT}" "${DEMO_SESSION_PORT}" "${DEMO_STATS_PORT}" "${DEMO_HEALTH_PORT}"; do
		port_in_use "${_p}" && _busy="${_busy} ${_p}"
	done
	if [ -n "${_stopped}" ]; then
		if [ -n "${_busy}" ]; then
			warn "Cortex asked to stop, but these ports are still in use:${_busy}"
			warn "  A proxy this script did not start may be holding them."
		else
			info "Cortex stopped; all ports free."
		fi
	elif [ -n "${_busy}" ]; then
		warn "No Cortex service or live pidfile found, but these ports are in use:${_busy}"
		warn "  If a proxy is running, stop it by its pid (this sandbox hides it from ps)."
	else
		info "Cortex is not running (no service, no live pidfile, ports free)."
	fi
}

# probe_setup AGENTOP asks AGENTOP whether it has the setup command this script hands
# off to, and exits with what `setup --help` exited with: 0 when it has it; 126 when
# the file cannot be run at all, typically a TMPDIR mounted noexec; anything else
# when it is an agentop from before setup, which exits 2 on an unknown subcommand.
# --help, because that is the one thing setup answers without looking at the machine.
probe_setup() {
	"$1" setup --help >/dev/null 2>&1
}

# no_setup AGENTOP dies, saying AGENTOP predates setup and what to run instead.
#
# Only a release tag gets the one-liner. That release's own installer drives its own
# agentop, and piping it is what makes the bootstrap re-exec into it; a local copy of
# this script never re-execs, so --ref there would pin only the binaries and land back
# here. Anything else (the channel build, or the agentop AUTHBRIDGE_SKIP_DOWNLOAD
# found installed) has no release to name, so the message names the agentop instead.
no_setup() {
	case "${version:-}" in
		v*)
			die "the ${version} agentop has no 'setup' command, which this installer needs.
  Use that release's own installer:
    curl -fsSL https://raw.githubusercontent.com/${REPO}/main/scripts/install.sh | sh -s -- --ref=${version}"
			;;
		*)
			die "the agentop at $1 has no 'setup' command, which this installer needs.
  Install one that has it: drop AUTHBRIDGE_SKIP_DOWNLOAD, or pass
  --ref=<a release that has it>."
			;;
	esac
}

# restage copies the stage at $1 to a fresh dir under ~/.cortex/tmp and prints that
# dir. It is ensure_tmpdir's fallback again, for a TMPDIR that is writable but mounted
# noexec: the download landed, but nothing in it can run. Only the binaries move, with
# their modes; the archives go with the old stage.
restage() {
	mkdir -p "${CORTEX_DIR}/tmp" || return 1
	_rs=$(mktemp -d "${CORTEX_DIR}/tmp/stage.XXXXXX") || return 1
	for _rs_b in agentop cortex cortex-session-dump; do
		[ ! -f "$1/${_rs_b}" ] || cp -p "$1/${_rs_b}" "${_rs}/${_rs_b}" || {
			rm -rf "${_rs}"
			return 1
		}
	done
	printf '%s\n' "${_rs}"
}

# --- session dump helper ---
#
# cortex-session-dump writes the in-memory session store to files. The store is
# memory-only, so without it a restart is unrecoverable data loss and the only
# readers are agentop and raw curl.
#
# STOPGAP: rossoctl/cortex#901 ("persist sessions") is the real fix -- the proxy
# writing sessions itself, rather than a helper someone has to remember to run.
# Expect this whole block to be removed when that lands.
#
# Fetched from the repo at ${version} rather than added to the release tarballs:
# the checksum step asserts EXACTLY two verified archives and refuses to install
# when it sees anything else, so a third asset would mean reworking the one step
# whose whole job is not to fail open. A helper script does not justify that.
#
# Staged beside the binaries; setup installs it when it is there. Never fatal: a
# missing dump helper must not fail an install that otherwise produces a working
# proxy. Requires python3, which is not a dependency of anything else here -- so the
# absence of it is reported, not repaired.
stage_session_dump() {
	_dump_url="https://raw.githubusercontent.com/${REPO}/${version}/scripts/dev/cortex-session-dump.py"
	_dump_dest="${tmp}/cortex-session-dump"
	_dump_tmp="${tmp}/cortex-session-dump.part"

	if ! curl -fsSL "${_dump_url}" -o "${_dump_tmp}" 2>/dev/null; then
		rm -f "${_dump_tmp}"
		warn "could not fetch cortex-session-dump (skipping; the proxy is unaffected)"
		return 0
	fi
	# A 404 body would otherwise install as a "script" that fails on first run.
	if ! head -n 1 "${_dump_tmp}" | grep -q '^#!/usr/bin/env python3'; then
		rm -f "${_dump_tmp}"
		warn "fetched cortex-session-dump did not look like the expected script (skipping)"
		return 0
	fi
	chmod +x "${_dump_tmp}"
	mv -f "${_dump_tmp}" "${_dump_dest}"
	if [ "$os" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
		xattr -d com.apple.quarantine "${_dump_dest}" 2>/dev/null || true
	fi
	command -v python3 >/dev/null 2>&1 \
		|| warn "cortex-session-dump needs python3, which is not on your PATH"
	return 0
}

# --- the download's progress bar ---
#
# On a terminal the download draws two lines on stderr: a title naming the release
# and the platform, then a bar redrawn in place. Neither is erased. The bar ends on its
# 100% frame, and once the checksums verify its line is drawn a last time, with
# "· sha256 ✓" and how long the download took, and ended. The two lines stand in for
# setup's header and its "✓ downloaded" row, and hand_off tells setup so.
#
# Off a terminal (a log, CI, a pipe) a redrawn line would be noise, so the download
# prints one plain line instead. So it does on a terminal the bar cannot use:
# TERM=dumb, which cannot redraw a line, or one too narrow for the title or for the
# bar's figures.
#
# The sizes are in setup's units (downloadSize in cmd/agentop/cmd_setup.go), so the
# figures agree with setup's. The bar's full cells are in the accent unless NO_COLOR
# is set to anything but empty, which is how setup reads it too. The accent here is one
# 256-colour red: setup's is one shade for a light terminal and another for a dark one,
# and a shell cannot tell which it is drawing on.

# size_text N prints N bytes as setup does: B under 1000, kB under 999950 (so never
# "1000.0 kB"), MB from there, to the nearest tenth. Integers only, as sh has no others.
size_text() {
	if [ "$1" -lt 1000 ]; then
		printf '%s B\n' "$1"
	elif [ "$1" -lt 999950 ]; then
		_st=$((($1 + 50) / 100))
		printf '%s.%s kB\n' "$((_st / 10))" "$((_st % 10))"
	else
		_st=$((($1 + 50000) / 100000))
		printf '%s.%s MB\n' "$((_st / 10))" "$((_st % 10))"
	fi
}

# progress_line DONE TOTAL WIDTH prints the bar's line for DONE of TOTAL bytes: a bar of
# WIDTH cells, the percentage, and both sizes. With TOTAL unknown (empty, or 0) there is
# nothing to take a percentage of, so it prints the bytes so far alone. DONE past TOTAL,
# a server that sized the file wrong, shows as 100% rather than a bar that overflows.
# WIDTH 0 is the figures without a bar. Pure: no terminal, and no escapes but the
# accent's when bar_sgr holds one, so the suite checks it as text.
progress_line() {
	case "$2" in
		'' | 0)
			size_text "$1"
			return 0
			;;
	esac
	_pl_done=$1
	[ "${_pl_done}" -le "$2" ] || _pl_done=$2
	printf '%s%3d%%  %s / %s\n' "$(bar_text "$((_pl_done * $3 / $2))" "$3")" \
		"$((_pl_done * 100 / $2))" "$(size_text "$1")" "$(size_text "$2")"
}

# bar_text FULL WIDTH prints a bar of WIDTH cells, the first FULL of them full, and the
# two spaces before its figures; nothing for WIDTH 0. A full cell is ━, in the accent
# when bar_sgr holds one, and the rest are ─, so the two differ without colour too.
bar_text() {
	[ "$2" -gt 0 ] || return 0
	_bx_on=""
	_bx_off=""
	_bx_i=0
	while [ "${_bx_i}" -lt "$2" ]; do
		if [ "${_bx_i}" -lt "$1" ]; then _bx_on="${_bx_on}━"; else _bx_off="${_bx_off}─"; fi
		_bx_i=$((_bx_i + 1))
	done
	[ -z "${_bx_on}" ] || [ -z "${bar_sgr:-}" ] || _bx_on="${bar_sgr}${_bx_on}${bar_sgr0:-}"
	printf '%s%s  ' "${_bx_on}" "${_bx_off}"
}

# done_line BYTES SECS WIDTH prints the bar's last line, for a download of BYTES that
# took SECS whole seconds and whose checksums verified: the bar full, the size, the
# check, and the time as setup shows one. Setup shows no time of 0, and nor does this.
# The same figures as setup's "✓ downloaded" row, which this line stands in for.
done_line() {
	_dn_t=""
	[ "$2" -le 0 ] || _dn_t="  $2.0s"
	printf '%s100%%  %s · sha256 ✓%s\n' "$(bar_text "$3" "$3")" "$(size_text "$1")" "${_dn_t}"
}

# bar_cells COLS PREFIX_LEN prints how many bar cells fit on a COLS-wide terminal after
# a PREFIX_LEN-character prefix: up to 40, or 0 when fewer than 10 fit. It fails when
# even the figures do not fit, because a line wider than the terminal wraps, and a
# wrapped line cannot be redrawn in place: every redraw would leave a line behind. The
# figures take at most 33 characters, on the last line ("100%  999.9 MB · sha256 ✓
# 999.0s"), the bar 2 more than its cells, and the last column stays empty, since
# writing it can wrap the line.
bar_cells() {
	_bc=$(($1 - 1 - $2 - 33))
	[ "${_bc}" -ge 0 ] || return 1
	_bc=$((_bc - 2))
	[ "${_bc}" -le 40 ] || _bc=40
	[ "${_bc}" -ge 10 ] || _bc=0
	printf '%s\n' "${_bc}"
}

# title_line COLS VERSION OS ARCH prints the line above the bar: what this is, and
# VERSION · OS/ARCH right-aligned to column 78, or to the last but one of a narrower
# terminal, so it does not wrap. It fails when the two halves do not fit 3 spaces apart.
# Widths are in characters, not bytes: each half holds one ·, 2 bytes wide in UTF-8 and
# 1 character, and the rest is ASCII, so each half is counted from its ASCII parts
# rather than measured.
title_line() {
	_tl_edge=78
	[ "$1" -ge 79 ] || _tl_edge=$(($1 - 1))
	# 26 is "  rosso cortex · installer"; 3 + 1 are the " · " and the "/".
	_tl_gap=$((_tl_edge - 26 - ${#2} - 3 - ${#3} - 1 - ${#4}))
	[ "${_tl_gap}" -ge 3 ] || return 1
	# shellcheck disable=SC2059 # the width is a number worked out here
	printf "  rosso cortex · installer%${_tl_gap}s%s\n" '' "$2 · $3/$4"
}

# term_cols prints the width of the terminal on stderr: stty's, else COLUMNS, else 80.
# stdin is the piped script, so stty reads the terminal through stderr. A size of 0, a
# pty nobody gave a size, counts as unknown.
term_cols() {
	_tc=$(stty size <&2 2>/dev/null) || _tc=""
	_tc=${_tc##* }
	case "${_tc}" in '' | *[!0-9]* | 0) _tc=${COLUMNS:-} ;; esac
	case "${_tc}" in '' | *[!0-9]* | 0) _tc=80 ;; esac
	printf '%s\n' "${_tc}"
}

# bar_draw DONE draws the line for DONE bytes over the last one: back to the start of
# the line (CR), the line, then erase whatever a longer line before it left (ESC[K).
# The first draw hides the cursor, which would otherwise sit blinking at the bar's end.
bar_draw() {
	if [ -z "${bar_shown:-}" ]; then
		bar_shown=1
		printf '\033[?25l' >&2
	fi
	printf '\r%s%s\033[K' "${bar_prefix}" "$(progress_line "$1" "${bar_total}" "${bar_w}")" >&2
}

# bar_end ends the bar's line, leaving the bar on it, and shows the cursor again, if a
# bar was drawn and its line is still open. bar_verified calls it once the checksums
# verify; before that, warn, die and the downloads' errors call it, so what they print
# starts on a line of its own; and the EXIT trap calls it, so neither a die, an INT, a
# TERM nor a HUP leaves the cursor hidden. (A KILL does: nothing can catch it.) The
# write fails when the terminal has closed, and is no error then: there is nothing
# left to end.
bar_end() {
	[ -n "${bar_shown:-}" ] || return 0
	bar_shown=""
	printf '\n\033[?25h' >&2 || :
}

# bar_verified BYTES SECS draws the bar's line a last time, as done_line has it, over
# the 100% frame, and ends it.
bar_verified() {
	[ -n "${bar_shown:-}" ] || return 0
	printf '\r%s%s\033[K' "${bar_prefix}" "$(done_line "$1" "$2" "${bar_w}")" >&2
	bar_end
}

# head_length FILE prints the size a HEAD's response, saved in FILE, gives: the
# Content-Length of the last response in its redirect chain, or nothing if that
# response is not a 2xx or has none. Reset at each response, so a final response
# without one is unknown rather than the 302's 0. Only the bar uses it, so a HEAD that
# fails costs the percentage, never the install. A file, not a pipe from curl: the
# HEAD runs in the background as curl itself, so that killing its pid stops it.
head_length() {
	tr -d '\r' 2>/dev/null <"$1" | awk '
		/^HTTP\// { st = $2; n = "" }
		tolower($1) == "content-length:" { n = $2 }
		END { if (st ~ /^2/ && n ~ /^[0-9]+$/) print n }'
}

# file_bytes FILE prints how much of FILE has landed so far, 0 before curl creates it.
file_bytes() {
	if [ -f "$1" ]; then wc -c <"$1" | tr -d '[:space:]'; else echo 0; fi
}

# stop_downloads stops any download still running, and the bar's size requests, and
# waits for them to end: the EXIT trap's first step, so a die, an INT, a TERM or a HUP
# never leaves a curl writing into a stage being deleted. A script's background jobs
# ignore INT, so a Ctrl-C does not stop them by itself.
stop_downloads() {
	# shellcheck disable=SC2086 # the pids, split into words on purpose
	[ -z "${dl_pids:-}" ] || kill ${dl_pids} 2>/dev/null || :
	# shellcheck disable=SC2086 # as above
	[ -z "${dl_pids:-}" ] || wait ${dl_pids} 2>/dev/null || :
	dl_pids=""
}

# hand_off AGENTOP STAGE execs `AGENTOP setup`, which is where this script ends: setup
# plans, asks once, installs, starts, and owns everything on screen from here. STAGE
# is the staging dir, passed as --from with the download's counts. Empty, in repair
# mode, it is neither, and setup re-checks what is installed instead.
#
# The flags are frozen. --ref=main runs this script against main-latest binaries,
# which can be a build older than it, so every flag sent here has to be one setup
# already takes. Only the ones this script shares with setup are passed on, and not
# --local, which is setup's default.
#
# --handoff-bytes and --handoff-seconds let setup's first line read "✓ downloaded".
# setup takes the run as the installer's, and deletes STAGE on its way out, only when
# one of them is above 0, so bytes is never 0, even for a sub-second download. Both
# must be whole numbers: a fraction is a usage error, which exits before setup knows
# to delete anything.
#
# exec, so nothing of this script runs after it. That includes the EXIT trap, which
# is cleared first so it holds in every shell; setup deletes the stage instead. stdin
# is the piped script itself, so setup and everything it starts get the terminal when
# there is one to open, and /dev/null when there is not, but never the rest of this
# file. setup asks on /dev/tty either way.
hand_off() {
	_ho_bin=$1
	_ho_stage=$2
	set --
	[ -z "${_ho_stage}" ] || set -- --from "${_ho_stage}"
	[ -z "${WIRE_CLAUDE_CODE}" ] || set -- "$@" --claude-code
	[ -z "${ASSUME_YES}" ] || set -- "$@" --yes
	[ -z "${NO_SERVICE}" ] || set -- "$@" --no-service
	[ "${MODE}" != "install-only" ] || set -- "$@" --install-only
	[ -z "${NO_MODIFY_PATH}" ] || set -- "$@" --no-modify-path
	[ -z "${_ho_stage}" ] || set -- "$@" "--handoff-bytes=${bytes}" "--handoff-seconds=${secs}"
	# CORTEX_INSTALLER_DREW=1 tells setup that the title and the bar are on screen, so it
	# draws neither its header nor its "✓ downloaded" row. An environment variable, not
	# a flag: the flags are frozen, and an agentop from before it ignores it. Unset when
	# the plain line was printed instead, so a value left in the environment cannot hide
	# setup's header.
	if [ -n "${bar_on:-}" ]; then
		CORTEX_INSTALLER_DREW=1
		export CORTEX_INSTALLER_DREW
	else
		unset CORTEX_INSTALLER_DREW
	fi
	trap - EXIT
	# Tried in a subshell: a redirection that fails on `exec` ends the shell it is in.
	if (exec </dev/tty) 2>/dev/null; then
		exec "${_ho_bin}" setup "$@" </dev/tty
	fi
	exec "${_ho_bin}" setup "$@" </dev/null
}

# --- detect platform ---
os=$(uname -s)
case "$os" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) die "unsupported OS: $os (the installer supports macOS and Linux)" ;;
esac

arch=$(uname -m)
case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "unsupported architecture: $arch (supported: amd64, arm64)" ;;
esac

# --- stop and exit, if asked ---
# Placed after the helpers and platform detection (so stop_cortex has what it needs)
# but before any download, preflight, or start — --stop must touch neither the
# network nor the binaries.
if [ "${MODE}" = "stop" ]; then
	stop_cortex
	exit 0
fi

# --- repair, offline: AUTHBRIDGE_SKIP_DOWNLOAD=1 hands the installed agentop to setup ---
# Nothing is downloaded, so nothing is staged: setup without --from installs no
# binaries, and re-checks and repairs the rest. All it needs from here is an agentop
# new enough to have setup; whether cortex is there is setup's to check.
if [ "${AUTHBRIDGE_SKIP_DOWNLOAD:-}" = "1" ]; then
	[ -x "${BIN_DIR}/agentop" ] || die "AUTHBRIDGE_SKIP_DOWNLOAD=1 but ${BIN_DIR}/agentop is missing"
	version=""
	probe_setup "${BIN_DIR}/agentop" || no_setup "${BIN_DIR}/agentop"
	hand_off "${BIN_DIR}/agentop" ""
fi

# --- resolve the release tag ---
# The binaries default to the same ref this script came from, so the script and the
# binaries it installs are one tested set rather than two independently-moving things.
version=$(resolve_version "${VERSION_REF}") \
	|| die "could not resolve the newest release (pass --ref=vX.Y.Z to pin one)"

# --- download + verify ---
# An explicit template under TMPDIR, not a bare `mktemp -d`: macOS's mktemp puts that
# in its per-user temp dir (_CS_DARWIN_USER_TEMP_DIR) whatever TMPDIR says, which
# would skip ensure_tmpdir's fallback, and put the stage outside the dirs setup will
# delete a stage from ($TMPDIR, /tmp, ~/.cortex/tmp) whenever TMPDIR is not the default.
tmp=$(mktemp -d "${TMPDIR%/}/cortex-stage.XXXXXX")
dl_pids=""
bar_shown=""
# The stage goes before the bar's line is ended. Ending it writes to the terminal, which
# is gone when the HUP came from closing it, and under set -e a failed write would end
# the trap there. The trap is the last thing the script runs, so a live terminal sees no
# change.
trap 'stop_downloads; rm -rf "$tmp" || :; bar_end' EXIT
# exit runs the EXIT trap, so an INT (Ctrl-C), a TERM or a HUP cleans up as a die does.
# Without these, dash dies of the signal and runs no EXIT trap at all, which leaves the
# stage behind, the downloads running, and the cursor hidden.
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

base="https://github.com/${REPO}/releases/download/${version}"
agentop_tgz="agentop_${version}_${os}_${arch}.tar.gz"
proxy_tgz="cortex_${version}_${os}_${arch}.tar.gz"

# tmp is the stage: the download lands in it, the binaries are extracted beside it,
# and setup gets it as --from. The clock starts here for --handoff-seconds.
started=$(date +%s 2>/dev/null) || started=""

# A bar when stderr is a terminal that can redraw a line and is wide enough for the
# title and the bar; the plain line otherwise.
bar_on=""
bar_prefix="  "
bar_total=""
bar_w=0
bar_sgr=""
bar_sgr0=""
if [ -t 2 ] && [ "${TERM:-}" != "dumb" ]; then
	bar_cols=$(term_cols)
	if bar_w=$(bar_cells "${bar_cols}" "${#bar_prefix}") \
		&& bar_title=$(title_line "${bar_cols}" "${version}" "${os}" "${arch}"); then
		bar_on=1
	fi
fi
if [ -n "${bar_on}" ]; then
	if [ -z "${NO_COLOR:-}" ]; then
		bar_sgr=$(printf '\033[38;5;167m')
		bar_sgr0=$(printf '\033[0m')
	fi
	printf '%s\n' "${bar_title}" >&2
	# The first frame at once, bytes only, before anything is asked for: the line is
	# never blank while the downloads and the size requests get going.
	bar_draw 0
	# curl's own errors wait in a file until the bar's line has ended, so that one
	# printed mid-download does not land inside the bar. Off a terminal fd 4 is stderr.
	exec 4>"${tmp}/curl.err"
else
	info "Downloading ${version} for ${os}/${arch}..."
	exec 4>&2
fi

# Both archives at once, each in the background; `wait` gives each one's exit status.
curl -fsSL "${base}/${agentop_tgz}" -o "${tmp}/${agentop_tgz}" 2>&4 &
pid_a=$!
dl_pids="${pid_a}"
curl -fsSL "${base}/${proxy_tgz}" -o "${tmp}/${proxy_tgz}" 2>&4 &
pid_c=$!
dl_pids="${pid_a} ${pid_c}"

# The sizes, for the percentage: asked in the background while the archives download,
# so the bar moves from the first poll on instead of waiting for them, and read once
# both have answered. Either size unknown leaves the total unknown, and the bar counts
# bytes. Each HEAD follows the redirect chain (-L) and gives up after 10s. One still out
# when the download ends is stopped then, as the size is known by that point, and the
# EXIT trap stops one still out on a die or a signal.
#
# Each is a bare curl, not a function or a pipeline: backgrounded, those run in a
# subshell, so $! names the subshell, and killing it leaves the curl under it running.
bar_sized=""
if [ -n "${bar_on}" ]; then
	curl -sIL --max-time 10 "${base}/${agentop_tgz}" >"${tmp}/head_a" 2>/dev/null &
	pid_sa=$!
	curl -sIL --max-time 10 "${base}/${proxy_tgz}" >"${tmp}/head_c" 2>/dev/null &
	pid_sc=$!
	dl_pids="${dl_pids} ${pid_sa} ${pid_sc}"
fi

# Poll until both have ended: every 0.2s, or every 1s from the first time sleep turns
# down a fraction, as a sleep that takes whole seconds only does.
poll_s=0.2
while kill -0 "${pid_a}" 2>/dev/null || kill -0 "${pid_c}" 2>/dev/null; do
	if [ -n "${bar_on}" ]; then
		if [ -z "${bar_sized}" ] && ! kill -0 "${pid_sa}" 2>/dev/null && ! kill -0 "${pid_sc}" 2>/dev/null; then
			bar_sized=1
			dl_pids="${pid_a} ${pid_c}"
			size_a=$(head_length "${tmp}/head_a")
			size_c=$(head_length "${tmp}/head_c")
			[ -z "${size_a}" ] || [ -z "${size_c}" ] || bar_total=$((size_a + size_c))
		fi
		got_a=$(file_bytes "${tmp}/${agentop_tgz}")
		got_c=$(file_bytes "${tmp}/${proxy_tgz}")
		bar_draw "$((got_a + got_c))"
	fi
	sleep "${poll_s}" 2>/dev/null || {
		poll_s=1
		sleep 1
	}
done
st_a=0
wait "${pid_a}" || st_a=$?
st_c=0
wait "${pid_c}" || st_c=$?
if [ -n "${bar_on}" ] && [ -z "${bar_sized}" ]; then
	kill "${pid_sa}" "${pid_sc}" 2>/dev/null || :
	wait "${pid_sa}" 2>/dev/null || :
	wait "${pid_sc}" 2>/dev/null || :
fi
dl_pids=""
rm -f "${tmp}/head_a" "${tmp}/head_c"
# curl_errors ends the bar's line and prints what curl said while the bar was on it.
# Never fails, so the die after it always says which download failed.
curl_errors() {
	bar_end
	[ -z "${bar_on}" ] || cat "${tmp}/curl.err" >&2 2>/dev/null || :
}
[ "${st_a}" = "0" ] || { curl_errors; die "download failed: ${agentop_tgz}"; }
[ "${st_c}" = "0" ] || { curl_errors; die "download failed: ${proxy_tgz}"; }
# The download's 100% frame, of what landed, whatever the HEADs said. It stays on
# screen, and is drawn once more when the checksums verify.
if [ -n "${bar_on}" ]; then
	bar_total=$(($(file_bytes "${tmp}/${agentop_tgz}") + $(file_bytes "${tmp}/${proxy_tgz}")))
	bar_draw "${bar_total}"
fi
curl -fsSL "${base}/checksums.txt" -o "${tmp}/checksums.txt" 2>&4 \
	|| { curl_errors; die "download failed: checksums.txt"; }
exec 4>&-
rm -f "${tmp}/curl.err"

# One grep per archive, not an alternation. An alternation SUCCEEDS on a single
# match, so a checksums.txt missing one entry — a truncated or partly-generated
# release build — passed the guard, sha_check verified only the file that was
# listed, and the UNVERIFIED binary was installed anyway. This is the one step
# whose whole job is not to fail open.
#
# Matching is anchored to end-of-line so an unrelated future artifact in
# checksums.txt can't make verification fail on a file we never fetched.
: > "${tmp}/checksums.filtered"
for archive in "${agentop_tgz}" "${proxy_tgz}"; do
	# The name may be preceded by whitespace, sha256sum's binary-mode "*", or a
	# path component: the release workflow runs `sha256sum ./*.tar.gz`, so every
	# real line reads "HASH  ./agentop_....tar.gz". An earlier version of this
	# pattern required the name immediately after whitespace or "*", which matched
	# nothing against an actual release and refused every install.
	# Anchored to the whole line and to the exact shape our own workflow emits:
	# "HASH  ./name" (from `cd dist && sha256sum ./*.tar.gz`), with a bare name and
	# binary-mode "*" also accepted.
	#
	# Deliberately NOT any path. sha_check runs from ${tmp}, so a permissive class
	# let a crafted entry like "HASH  ../name" or "HASH  /etc/name" match and be
	# verified against a file outside the download directory — passing verification
	# for something other than the archive we then extract. Only ./ and a bare name
	# are ours, so nothing else is accepted.
	archive_re=$(ere_escape "${archive}")
	grep -E "^[0-9a-fA-F]+[[:space:]]+\*?(\./)?${archive_re}\$" "${tmp}/checksums.txt" \
		>> "${tmp}/checksums.filtered" \
		|| die "checksums.txt has no usable entry for ${archive} — refusing to install it unverified"
done
# Both entries present, and exactly the two we asked for.
lines=$(wc -l < "${tmp}/checksums.filtered" | tr -d '[:space:]')
[ "${lines}" = "2" ] \
	|| die "expected 2 checksum entries, got ${lines} — refusing to install"
# Quiet on success, loud on failure. The per-archive "OK" lines are two lines saying
# what one line implies — but sending them to /dev/null took the FAILED lines (stdout)
# and shasum's "computed checksum did NOT match" warning (stderr) with them, so a
# corrupt download or a tampered release would surface as a bare "checksum verification
# failed" naming neither archive. That is the one step whose whole job is not to fail
# open; it should not also fail silently.
#
# bar_shown is cleared in the subshell, whose stderr is sha.out: a die there ends no
# bar's line. The bar's line is ended here instead, before sha.out is shown.
if ! ( bar_shown="" && cd "$tmp" && sha_check checksums.filtered >"${tmp}/sha.out" 2>&1 ); then
	bar_end
	cat "${tmp}/sha.out" >&2
	die "checksum verification failed — do NOT use these binaries"
fi

# --- the download's counts, for setup's "✓ downloaded" line ---
# At least 1 byte, and whole seconds: see hand_off for why each matters to setup. A
# clock reading that is not a number counts as 0 seconds rather than failing here.
bytes=$(cat "${tmp}/${agentop_tgz}" "${tmp}/${proxy_tgz}" | wc -c | tr -d '[:space:]')
[ "${bytes:-0}" -gt 0 ] 2>/dev/null || bytes=1
finished=$(date +%s 2>/dev/null) || finished=""
case "${started}:${finished}" in
	*[!0-9:]* | :* | *:) secs=0 ;;
	*) secs=$((finished - started)) ;;
esac
[ "${secs}" -ge 0 ] || secs=0
# The same counts on the bar's last line, which stands in for that row on a terminal.
bar_verified "${bytes}" "${secs}"

# --- extract into the stage ---
tar -xzf "${tmp}/${agentop_tgz}" -C "$tmp"
tar -xzf "${tmp}/${proxy_tgz}" -C "$tmp"
for b in agentop cortex; do
	[ -f "${tmp}/${b}" ] || die "archive did not contain expected binary: ${b}"
	chmod +x "${tmp}/${b}"
done

# macOS: clear the quarantine flag so Gatekeeper doesn't block the unsigned binaries.
if [ "$os" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
	xattr -dr com.apple.quarantine "${tmp}/agentop" "${tmp}/cortex" 2>/dev/null || true
fi

stage_session_dump

# --- can the stage run? ---
# Exit 126 means the file cannot be run at all, so stage it again under ~/.cortex/tmp
# and ask once more. Anything else non-zero is an agentop from before setup.
probe_st=0
probe_setup "${tmp}/agentop" || probe_st=$?
if [ "${probe_st}" = "126" ]; then
	# The new stage replaces the old one before the old one goes, so the EXIT trap
	# always names a stage that exists.
	restaged=$(restage "${tmp}") \
		|| die "cannot run programs from ${tmp%/*} (exit 126), and could not stage under ${CORTEX_DIR}/tmp instead"
	first_dir="${tmp%/*}"
	first_stage="${tmp}"
	tmp="${restaged}"
	rm -rf "${first_stage}"
	info "${first_dir} does not let programs run (exit 126); staged under ${CORTEX_DIR}/tmp instead."
	probe_st=0
	probe_setup "${tmp}/agentop" || probe_st=$?
	[ "${probe_st}" != "126" ] \
		|| die "cannot run programs from ${first_dir} or from ${CORTEX_DIR}/tmp (exit 126 from both); both look mounted noexec. Set TMPDIR to a directory that allows running programs, and re-run."
fi
[ "${probe_st}" = "0" ] || no_setup "${tmp}/agentop"

# --- hand off ---
hand_off "${tmp}/agentop" "${tmp}"
