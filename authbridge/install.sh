#!/bin/sh
# install.sh — one-line installer for Cortex on a local machine.
#
#   curl -fsSL https://raw.githubusercontent.com/rossoctl/cortex/main/authbridge/install.sh | sh
#
# Detects your OS/arch, downloads the prebuilt `abctl` and `authbridge-proxy`
# binaries for the newest release, verifies their SHA-256 checksums, installs
# them to ~/.local/bin, and starts Cortex in the background — then prints the
# commands to watch traffic and point an agent at it, plus how to stop it.
# macOS + Linux, amd64 + arm64. No cluster, Keycloak, or SPIRE needed.
#
# It installs, starts Cortex with its built-in config in ~/.cortex, and prints the
# command to send an agent through it. Traffic is decrypted and parsed for viewing;
# nothing is rewritten. Cutting Claude Code's token cost is one opt-in command
# afterwards, printed at the end.
#
# Options (pass through the pipe with `sh -s --`, e.g.
#   curl -fsSL ...install.sh | sh -s -- --install-only):
#
#   --install-only   install the binaries and stop
#   --claude-code    after starting, offer to write the three env vars Claude Code
#                    needs into ~/.claude/settings.json, so it runs as plain
#                    `claude`. Prompts before changing anything.
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
# By default this script re-runs the copy from the newest RELEASE rather than
# executing whatever is currently on main — main is unstable by definition, and a
# `curl | sh` should not be the first thing to run a change nobody has released.
# --ref=main opts back in; --ref=vX.Y.Z pins.
#
# Every path here installs a RELEASE. To install what is in a checkout instead,
# use `make dev-install` from the repo root: it compiles both binaries, writes them
# to the same ~/.local/bin this script uses, and restarts the service. --ref=main
# is not that — it only chooses which copy of this script runs, and that copy still
# downloads a build.
#
# Environment (maintainer testing only — not part of the documented interface):
#   AUTHBRIDGE_SKIP_DOWNLOAD=1  use the already-installed binaries in ~/.local/bin
#                               instead of downloading (re-run setup offline)
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
# PATH_MARKER identifies our block in a shell profile, so a re-run does not add a
# second copy and a person can find what to delete.
PATH_MARKER="# added by Cortex (rossoctl/cortex) — delete these two lines to undo"

# installed_version prints the version of an already-installed binary, or nothing.
#
# Both binaries print "<name> vX.Y.Z"; the tag is the last field. Anything unexpected —
# missing binary, a build that does not know --version, a quarantined binary that will
# not run — prints nothing, which reads as "not this version" and re-installs. Erring
# toward re-installing is right: a wrong skip leaves someone on an old build believing
# they upgraded.
installed_version() {
	[ -x "${BIN_DIR}/$1" ] || return 0
	"${BIN_DIR}/$1" --version 2>/dev/null | awk 'NR==1{print $NF}'
}
# SUPERVISOR_NAME is the human label; SUPERVISOR_CMD is the actual command the
# messages name, so "launchctl may not be used here" reads as the thing the user
# would otherwise reach for.
case "$(uname -s)" in
	Darwin) SUPERVISOR_NAME="launchd user agent"; SUPERVISOR_CMD="launchctl" ;;
	*) SUPERVISOR_NAME="systemd user unit"; SUPERVISOR_CMD="systemctl --user" ;;
esac

info() { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

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
# handle what actually fails: supervisor_usable() (a real `launchctl list` /
# `systemctl --user` probe) plus the service-install attempt fall back to a plain
# background process with a clear message, and ensure_tmpdir writes-then-falls-back to
# a scratch dir under ~/.cortex. Nothing assumes the sandbox is restrictive; nothing
# assumes it is permissive either.

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
  curl -fsSL https://raw.githubusercontent.com/rossoctl/cortex/main/authbridge/install.sh | sh
  curl -fsSL ...install.sh | sh -s -- [option]

Installs abctl and authbridge-proxy to ~/.local/bin, starts the proxy with its
built-in config in ~/.cortex, and prints the command to send an agent through it.
Traffic is decrypted and parsed for viewing; nothing is rewritten.

Options:
  --install-only   install the binaries and stop
  --claude-code    after starting, offer to configure Claude Code to use it, so
                   it runs as plain `claude` with no environment variables
  --local          the default, spelled out
  --no-service     do not use the OS service supervisor (launchd/systemd); start
                   the proxy directly as a background process instead. Chosen
                   automatically when the supervisor turns out to be unavailable
                   (e.g. a macOS seatbelt sandbox that blocks launchctl), and always
                   prints the environment variables to point ANY AI harness at
                   Cortex, not just Claude Code
  --stop           stop a running Cortex and exit — the supervised service if one
                   is installed, and the background proxy from its pidfile. Does
                   not download, install, or start anything. Safe to re-run.
  --yes, -y        do not prompt; answer yes to configuring Claude Code
  --ref=REF        install from a git ref instead of the newest release — both
                   this script and the binaries (e.g. --ref=main for unreleased
                   changes, --ref=v0.7.0-alpha.4 to pin)
  -h, --help       this text

After installing, to cut Claude Code's token cost:
  abctl tools scan --write ~/.cortex/config.yaml   (proposes which tools to prune)
Then watch the $ saved on every prompt Claude Code sends, live in:
  abctl
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
for arg in "$@"; do
	case "$arg" in
		--install-only) MODE=install-only ;;
		--claude-code) WIRE_CLAUDE_CODE=1 ;;
		--yes | -y) ASSUME_YES=1 ;;
		--ref=*) WANT_REF="${arg#*=}" ;;
		--no-service) NO_SERVICE=1 ;;
		--stop) MODE=stop ;;
		# --local is the default; accepted so writing it out explicitly works, and
		# so it mirrors the proxy flag of the same name.
		--local) MODE=local ;;
		-h | --help)
			usage
			exit 0
			;;
		*) die "unknown option: $arg (try --claude-code, --install-only, --local, --no-service, --stop, --ref=REF, --yes, or no argument)" ;;
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

# newest_release prints the newest VERSION release tag, prereleases included.
# `releases/latest` excludes prereleases and this project ships them, so list
# releases (newest first) and take the first tag that looks like a version.
newest_release() {
	# Four steps, each doing one thing that cannot silently go wrong:
	#
	#   tr ',{}' '\n'   put every JSON field on its own line, so nothing greedy can
	#                   run past the field it was aimed at. Without this the old
	#                   `sed 's/.*"tag_name": *"//'` depended on GitHub pretty-printing:
	#                   against a COMPACT response the whole array is one line, the
	#                   greedy .* runs to the LAST tag_name, and it returns the OLDEST
	#                   release. Verified — it yields v0.3.1 from compact JSON.
	#   grep '"tag_name":'
	#                   match tag_name only where it is a KEY (anchored, colon after).
	#                   An unanchored match is hijacked by any release whose name or
	#                   body contains the text "tag_name", and release bodies are ours
	#                   to author. Every tag, not just the first — the -m1 belongs on
	#                   the value filter below, so that what gets picked is the first
	#                   VERSION tag rather than merely the first tag.
	#   cut -d'"' -f4   take the value by position, not by pattern.
	#   grep -m1 '^v[0-9]'
	#                   the developer channel's rolling `main` release sorts first
	#                   until the next tagged release (the API sorts by created_at,
	#                   which is fixed at creation). Taking the first entry blindly
	#                   would hand `main` to someone who never asked for it, and the
	#                   shape check below would then reject it and kill the install
	#                   outright. Skipping non-version tags keeps the channel
	#                   invisible to the default path.
	#
	# ?per_page=10 for the same reason: one page has to contain a version tag even
	# with rolling releases ahead of it. Ten is headroom, not a calculation — there is
	# one rolling release, so two would do.
	#
	# The shape check is now a backstop rather than the filter. Reaching it with a
	# non-version tag is impossible; reaching it EMPTY is not, and means no version tag
	# in ten releases — an error page, a rate-limit body, a schema change. Fail rather
	# than build a download URL out of it. No warning here: the only reachable failure
	# is "nothing matched", and naming `main` at someone who never mentioned it is
	# noise. The caller already dies with actionable advice.
	#
	# Not jq (not installed everywhere) and not gh (a far larger dependency than a
	# curl|sh installer should require; this script needs curl, tar and a checksum tool).
	# Not /releases/latest either: it excludes prereleases, and this project ships them,
	# so it names a tag from January. Listing releases asks what we actually mean — the
	# newest release, whatever its flags.
	# TWO sources, because one was not enough. api.github.com allows 60 requests per
	# hour per IP unauthenticated — shared by everyone behind one NAT, and each install
	# spends two. Exhausting it killed the documented one-liner outright and told the
	# person to go find a version and pass --ref, which is the opposite of a one-line
	# install. Observed on a normal laptop, not contrived.
	_tag=$(release_tag_from_api)
	[ -n "${_tag}" ] || _tag=$(release_tag_from_feed)
	case "${_tag}" in
		v[0-9]*) ;;
		*) return 1 ;;
	esac
	printf '%s\n' "${_tag}"
}

# release_tag_from_api prints the newest v-tag per the releases API, or nothing.
release_tag_from_api() {
	# The status is captured rather than discarded so a 403 can be NAMED. Nearly always
	# that is the unauthenticated rate limit, which is a wait-or-pin situation and not a
	# bug — and the old message said only "could not resolve the newest release", which
	# told nobody that waiting would fix it.
	_body="${TMPDIR:-/tmp}/cortex-rel.$$"
	_code=$(curl -sSL -o "${_body}" -w '%{http_code}' \
		"https://api.github.com/repos/${REPO}/releases?per_page=10" 2>/dev/null) || _code="000"
	if [ "${_code}" = "403" ] || [ "${_code}" = "429" ]; then
		# Only worth saying if it is really the quota; a 403 for another reason should
		# not be mislabelled.
		if grep -q 'rate limit' "${_body}" 2>/dev/null; then
			warn "GitHub's API rate limit for this network is exhausted (60/hour per IP, unauthenticated); trying the releases feed instead"
		fi
	fi
	if [ "${_code}" = "200" ]; then
		tr ',{}' '\n' <"${_body}" \
			| grep '^[[:space:]]*"tag_name"[[:space:]]*:' \
			| cut -d'"' -f4 \
			| grep -m1 '^v[0-9]' || true
	fi
	rm -f "${_body}"
}

# release_tag_from_feed prints the newest v-tag per the releases Atom feed, or nothing.
#
# github.com, not api.github.com: the feed is not bound by the API's 60/hour, which is
# the whole reason it is here. It lists releases newest-first and includes prereleases,
# so it answers the same question the API does.
#
# The parse is anchored to the <title> ELEMENT rather than grepping for a version-shaped
# line. Release notes are ours to author and appear in the same document, so an
# unanchored match could be hijacked by a notes line that happens to start with a
# version — the same shape of bug as the unanchored "tag_name" match this file already
# guards against. Notes arrive HTML-escaped (&lt;p&gt;), so they cannot forge a <title>.
release_tag_from_feed() {
	curl -fsSL "https://github.com/${REPO}/releases.atom" 2>/dev/null \
		| sed -n 's|.*<title>\(v[0-9][^<]*\)</title>.*|\1|p' \
		| head -1
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
# pattern for abctl_v0.7.0-alpha.3_..tar.gz also accepted
# abctl_v0X7X0-alpha_3_..Xtar.gz. Nothing exploitable followed — the count check
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
# did nothing — the "Using the installer from vX" line ran the OLD released code and
# died. `[ -f "$0" ] && [ -r "$0" ]` is the discriminator: a readable file at $0 means a
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
		# already is main, so there is nothing to re-exec.
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
		url="https://raw.githubusercontent.com/${REPO}/${want_ref}/authbridge/install.sh"
		# Capture the status code rather than collapsing every failure into one
		# branch. A 404 means that ref genuinely predates this script — fall back.
		# A transport error means we could not ask, and silently dropping to main
		# there would break the exact guarantee this bootstrap exists to give.
		# On a transport failure curl still prints "000" via -w AND exits non-zero,
		# so appending our own default produced "HTTP 000000". Overwrite instead.
		http=$(curl -sSL -o "${boot}" -w '%{http_code}' "${url}" 2>/dev/null) || http="000"
		[ -n "${http}" ] || http="000"
		if [ "${http}" = "200" ] && [ -s "${boot}" ]; then
			info "Using the installer from ${want_ref}."
			# set -e would abort the parent on a non-zero child before any of the
			# lines below ran, leaking the downloaded script on every failed
			# install. The if/else keeps the status and still cleans up.
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
			warn "${want_ref} has no authbridge/install.sh (HTTP 404); continuing with the copy from main"
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

# ca_fingerprint prints a hash of the bridge CA, or nothing when there is no CA yet.
# Same tool preference as sha_check: shasum on macOS, sha256sum elsewhere. Prints
# nothing rather than failing when neither exists — this drives one advisory message,
# and an installer must not die over that.
ca_fingerprint() {
	[ -f "${ca_dir}/ca.crt" ] || return 0
	if command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "${ca_dir}/ca.crt" 2>/dev/null | cut -d' ' -f1
	elif command -v sha256sum >/dev/null 2>&1; then
		sha256sum "${ca_dir}/ca.crt" 2>/dev/null | cut -d' ' -f1
	fi
	# Explicit, because the caller assigns this in a bare `fp="$(ca_fingerprint)"` and a
	# non-zero status there aborts under set -e. An if/elif with no matching branch
	# happens to yield 0 today, but that stops being true the moment someone adds an
	# else — too subtle a thing to leave the installer's survival resting on.
	return 0
}

# Demo listener ports — loopback, and deliberately uncommon to avoid colliding
# with common dev tools. Keep in sync with the built-in config in
# authbridge/cmd/authbridge-proxy/local.go.
DEMO_FORWARD_PORT=47600
DEMO_SESSION_PORT=47601
DEMO_STATS_PORT=47602
# Bound too, and previously missing from the preflight — an occupied 47604 let the
# download finish and then killed the proxy during startup.
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

# demo_ports_busy exits 0 if ANY of the local listener ports is already in use. Used
# to tell a real "no supervisor" case (ports free -> safe to run unsupervised) from
# the benign upgrade race (old proxy still holds the ports -> starting a second one
# would just crash on the bind).
demo_ports_busy() {
	for _p in "${DEMO_FORWARD_PORT}" "${DEMO_SESSION_PORT}" "${DEMO_STATS_PORT}" "${DEMO_HEALTH_PORT}"; do
		port_in_use "${_p}" && return 0
	done
	return 1
}

# service_install_action classifies the outcome of `abctl service install` into one
# word, so the decision is one testable place instead of a chain of greps inline.
#   $1 = abctl's exit status   $2 = abctl's combined stdout+stderr
# Prints exactly one of:
#   supervised — it worked.
#   refused    — abctl declined ON PURPOSE (a `refus`* message, e.g. a config that
#                would expose a listener). This is the one failure we must NOT paper
#                over: running the same proxy unsupervised would defeat that check.
#   ports-busy — non-zero, but our listener ports are held: the benign upgrade race
#                (the old proxy is still draining). Falling back would crash a second
#                proxy on the bound ports, so tell the user to wait and re-run.
#   fallback   — any other non-zero: the OS supervisor simply cannot take the job here
#                (launchd EIO in a sandbox, an absent systemd user bus, ...). Do what
#                --no-service does and run the proxy directly, rather than die after a
#                clean install. This is the default, so a NEW failure mode falls back
#                (Cortex runs) instead of leaving it down.
# The distinction is by exit + `refus` + ports, NOT a positive match on the failure
# text: abctl prints the launchd EIO to stdout, so a stderr-only signature missed it
# and the installer died where it should have fallen back.
service_install_action() { # status output
	[ "$1" = "0" ] && { printf 'supervised\n'; return 0; }
	if printf '%s' "$2" | grep -qi 'refus'; then printf 'refused\n'; return 0; fi
	if demo_ports_busy; then printf 'ports-busy\n'; return 0; fi
	printf 'fallback\n'
}

# Where the unsupervised proxy records its pid, so a service-less install still has
# exactly one process to find, check, and stop.
PROXY_PIDFILE="${CORTEX_DIR}/proxy.pid"

# supervisor_usable exits 0 when the OS service supervisor can actually be driven
# here. This is the explicit access check to run BEFORE install, so a supervisor we
# cannot use becomes a clean fall-through to the background start instead of a fatal
# `die` after the binaries are already on disk.
#
#   macOS: in a seatbelt sandbox `launchctl bootstrap` fails with an I/O error
#   (exit 5) that abctl surfaces as a plain non-zero exit. `launchctl print` on our
#   own GUI domain can answer positively even when bootstrap cannot, so probe
#   `launchctl list`, which needs a real, reachable user domain and fails when
#   confined.
#
#   Linux: abctl installs a systemd *user* unit, which needs both systemctl and a
#   running per-user manager (a session/D-Bus). That manager is absent in many
#   containers, minimal images, and non-systemd inits (OpenRC, runit, s6), so a bare
#   `command -v systemctl` is not enough — `systemctl --user show-environment` is a
#   read-only call that succeeds only when the user manager is actually reachable.
#
# Only a preflight: the install attempt below still catches a supervisor that passes
# this check but fails for another reason.
supervisor_usable() {
	if [ "$os" = "darwin" ]; then
		command -v launchctl >/dev/null 2>&1 || return 1
		launchctl list >/dev/null 2>&1
	else
		command -v systemctl >/dev/null 2>&1 || return 1
		systemctl --user show-environment >/dev/null 2>&1
	fi
}

# proxy_running exits 0 if the proxy we recorded in the pidfile is still alive AND is
# actually our proxy. Validating both matters because stop_cortex signals this pid:
#   - a non-numeric or negative pidfile would make `kill` parse its argument wrong;
#   - after an unclean shutdown the OS can recycle the pid onto an unrelated process
#     of the same user, which we must not SIGTERM/SIGKILL.
# Where `ps` can name the process we require it to be authbridge-prox(y) — the same
# check abctl's runningPID uses, so `abctl service install` and this script agree on
# what counts as "our proxy". Where the sandbox hides processes from `ps`, ps prints
# nothing and the pidfile remains the only handle, so we keep the kill -0 result.
proxy_running() {
	_pid=$(cat "${PROXY_PIDFILE}" 2>/dev/null) || return 1
	case "${_pid}" in
		"" | *[!0-9]*) return 1 ;;
	esac
	kill -0 "${_pid}" 2>/dev/null || return 1
	_comm=$(ps -o comm= -p "${_pid}" 2>/dev/null) || return 0
	[ -z "${_comm}" ] && return 0
	case "${_comm}" in
		*authbridge-prox*) return 0 ;;
		*) return 1 ;;
	esac
}

# start_unsupervised runs the proxy as a plain background process for environments
# without a usable launchd/systemd. It uses the proxy's own --supervise restart loop
# (built for exactly this — "launchd cannot be relied on"), so a crash still comes
# back, and records the pid so stop/status have one process to target. Verified: on
# a clean SIGTERM to this pid the listeners close and no child is left behind.
start_unsupervised() {
	if proxy_running; then
		info "Cortex is already running (pid $(cat "${PROXY_PIDFILE}"))."
		return 0
	fi
	nohup "${BIN_DIR}/authbridge-proxy" --local --supervise \
		>>"${CORTEX_DIR}/proxy.log" 2>&1 &
	_pid=$!
	printf '%s\n' "${_pid}" > "${PROXY_PIDFILE}"
	# Poll the health port rather than sleeping a fixed guess: come up fast, and fail
	# fast and loudly if the proxy exits on startup (a taken port, a bad config).
	_i=0
	while [ "${_i}" -lt 10 ]; do
		if ! kill -0 "${_pid}" 2>/dev/null; then
			rm -f "${PROXY_PIDFILE}"
			warn "the proxy exited immediately; see ${CORTEX_DIR}/proxy.log"
			return 1
		fi
		port_in_use "${DEMO_HEALTH_PORT}" && return 0
		_i=$((_i + 1))
		sleep 1
	done
	# Alive but the health port never opened. Unusual, and worth surfacing, but the
	# process is up — let the caller point at the log rather than kill something that
	# may still be finishing startup.
	warn "proxy started (pid ${_pid}) but the health port ${DEMO_HEALTH_PORT} did not open in time; check ${CORTEX_DIR}/proxy.log"
	return 0
}

# stop_cortex stops a running Cortex — the supervised service if abctl installed one,
# and the background proxy recorded in the pidfile — then reports on the ports so
# "stopped" is verified rather than assumed. Re-runnable and safe: with nothing
# running it says so and exits 0 rather than failing. This backs `install.sh --stop`,
# and matters most in a sandbox, where `ps`/`pkill` are blind and the pidfile is the
# only reliable handle on the process.
stop_cortex() {
	_stopped=""
	# Supervised: hand it back to abctl, which owns the launchd/systemd unit. `service
	# stop` exits non-zero with "no service installed" when there is none — that is a
	# normal state here, not an error, so only a DIFFERENT failure is surfaced.
	if [ -x "${BIN_DIR}/abctl" ]; then
		if _svc_out=$("${BIN_DIR}/abctl" service stop 2>&1); then
			info "Stopped the supervised service (${SUPERVISOR_NAME})."
			_stopped=1
		elif ! printf '%s' "${_svc_out}" | grep -qi "no service installed"; then
			warn "abctl service stop reported: ${_svc_out}"
		fi
	fi
	# Unsupervised: kill the process recorded in the pidfile — the only handle that
	# works where the sandbox hides other processes from ps/pkill.
	if proxy_running; then
		_pid=$(cat "${PROXY_PIDFILE}")
		info "Stopping the background proxy (pid ${_pid})..."
		kill "${_pid}" 2>/dev/null || true
		# Wait longer than the proxy's own 15s graceful drain before escalating, so a
		# normal shutdown is never cut short into a SIGKILL that drops in-flight
		# requests. This matches abctl's stopPID, which waits 18s for the same reason.
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

# print_env_instructions prints the environment variables that point ANY tool or AI
# harness at Cortex — not just Claude Code, and not only via ~/.claude/settings.json.
# ca_dir and the ports are read from the surrounding script at call time.
print_env_instructions() {
	info "  Point ANY AI tool / harness at Cortex with these environment variables:"
	info "    HTTPS_PROXY=http://localhost:${DEMO_FORWARD_PORT}"
	info "    HTTP_PROXY=http://localhost:${DEMO_FORWARD_PORT}"
	info "    NODE_EXTRA_CA_CERTS=${ca_dir}/ca.crt        # Node tools (Claude Code, Codex, etc.); EXTENDS trust"
	info ""
	info "  Tools that REPLACE the trust store need the CA+roots bundle, not ca.crt:"
	info "    SSL_CERT_FILE=${ca_dir}/bundle.crt          # Go, OpenSSL, and most others (Linux)"
	info "    REQUESTS_CA_BUNDLE=${ca_dir}/bundle.crt     # Python requests / httpx"
	info "    CURL_CA_BUNDLE=${ca_dir}/bundle.crt         # curl"
	info "    GIT_SSL_CAINFO=${ca_dir}/bundle.crt         # git"
	info ""
	info "  Example — send one command through Cortex (works for any harness):"
	info "    HTTPS_PROXY=http://localhost:${DEMO_FORWARD_PORT} \\"
	info "      NODE_EXTRA_CA_CERTS=${ca_dir}/ca.crt \\"
	info "      SSL_CERT_FILE=${ca_dir}/bundle.crt <your-agent-command>"
	if [ "$(uname -s)" = "Darwin" ]; then
		info ""
		info "  On macOS, Go tools (go, gh) ignore SSL_CERT_FILE — trust the CA instead:"
		info "    security add-trusted-cert -k ~/Library/Keychains/login.keychain-db \\"
		info "      -p ssl ${ca_dir}/ca.crt"
	fi
}

# print_local_start_help prints how to start/stop/inspect the proxy when it runs
# unsupervised (no launchd/systemd), using the pidfile start_unsupervised wrote.
print_local_start_help() {
	info "  This environment has no usable OS service supervisor, so Cortex runs as a"
	info "  plain background process (it will NOT restart after a reboot or logout):"
	info "    start:   \"${BIN_DIR}/authbridge-proxy\" --local --supervise   (backgrounded)"
	info "    stop:    kill \$(cat ${PROXY_PIDFILE})"
	info "    status:  curl -fsS http://localhost:${DEMO_HEALTH_PORT}/ >/dev/null && echo up || echo down"
	info "    logs:    tail -f ${CORTEX_DIR}/proxy.log"
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

# --- preflight: fail early (before downloading) if a listener port is taken ---
if [ "$MODE" = "local" ]; then
	# A Cortex of ours holding these ports is fine — `abctl service install` adopts
	# it, and keeping a second copy of that narrow "is this pid really ours" check
	# here would only let the two drift. This probe is for a FOREIGN listener, and
	# it runs before the download so the failure is early and cheap.
	for p in "$DEMO_FORWARD_PORT" "$DEMO_SESSION_PORT" "$DEMO_STATS_PORT" "$DEMO_HEALTH_PORT"; do
		if port_in_use "$p"; then
			if [ -f "${CORTEX_DIR}/config.yaml" ]; then
				# Ours, most likely: let abctl adopt it rather than refusing here.
				continue
			fi
			die "port ${p} is already in use by something else. Free it, or change the ports in ${CORTEX_DIR}/config.yaml, then re-run."
		fi
	done
fi

# --- skip the download entirely when asked (offline re-run) ---
if [ "${AUTHBRIDGE_SKIP_DOWNLOAD:-}" = "1" ]; then
	for b in abctl authbridge-proxy; do
		[ -x "${BIN_DIR}/${b}" ] || die "AUTHBRIDGE_SKIP_DOWNLOAD=1 but ${BIN_DIR}/${b} is missing"
	done
	version="already installed"
	skip_install=1
	info "Using the binaries already in ${BIN_DIR}"
else

# --- resolve the release tag ---
# The binaries default to the same ref this script came from, so the script and the
# binaries it installs are one tested set rather than two independently-moving things.
version=$(resolve_version "${VERSION_REF}") \
	|| die "could not resolve the newest release (pass --ref=vX.Y.Z to pin one)"

# --- download + verify ---
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

base="https://github.com/${REPO}/releases/download/${version}"
abctl_tgz="abctl_${version}_${os}_${arch}.tar.gz"
proxy_tgz="authbridge-proxy_${version}_${os}_${arch}.tar.gz"

# Already at this version? Then there is nothing to download, and nothing to
# overwrite. Re-running the one-liner is how people upgrade, so it runs constantly
# against installs that are already current — it should cost nothing and change
# nothing. Both binaries must match: replacing one and not the other is the version
# skew that put an older proxy in the launchd unit.
if installed_version abctl | grep -qx "${version}" &&
	installed_version authbridge-proxy | grep -qx "${version}"; then
	info "Already at ${version} — not re-downloading."
	skip_install=1
fi

if [ -z "${skip_install:-}" ]; then
info "Downloading ${version} for ${os}/${arch}..."
curl -fsSL "${base}/${abctl_tgz}" -o "${tmp}/${abctl_tgz}" || die "download failed: ${abctl_tgz}"
curl -fsSL "${base}/${proxy_tgz}" -o "${tmp}/${proxy_tgz}" || die "download failed: ${proxy_tgz}"
curl -fsSL "${base}/checksums.txt" -o "${tmp}/checksums.txt" || die "download failed: checksums.txt"

# One grep per archive, not an alternation. An alternation SUCCEEDS on a single
# match, so a checksums.txt missing one entry — a truncated or partly-generated
# release build — passed the guard, sha_check verified only the file that was
# listed, and the UNVERIFIED binary was installed anyway. This is the one step
# whose whole job is not to fail open.
#
# Matching is anchored to end-of-line so an unrelated future artifact in
# checksums.txt can't make verification fail on a file we never fetched.
: > "${tmp}/checksums.filtered"
for archive in "${abctl_tgz}" "${proxy_tgz}"; do
	# The name may be preceded by whitespace, sha256sum's binary-mode "*", or a
	# path component: the release workflow runs `sha256sum ./*.tar.gz`, so every
	# real line reads "HASH  ./abctl_....tar.gz". An earlier version of this
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
if ! ( cd "$tmp" && sha_check checksums.filtered >"${tmp}/sha.out" 2>&1 ); then
	cat "${tmp}/sha.out" >&2
	die "checksum verification failed — do NOT use these binaries"
fi

# --- extract + install ---
mkdir -p "$BIN_DIR"
tar -xzf "${tmp}/${abctl_tgz}" -C "$tmp"
tar -xzf "${tmp}/${proxy_tgz}" -C "$tmp"
for b in abctl authbridge-proxy; do
	[ -f "${tmp}/${b}" ] || die "archive did not contain expected binary: ${b}"
	chmod +x "${tmp}/${b}"
	mv -f "${tmp}/${b}" "${BIN_DIR}/${b}"
done

# macOS: clear the quarantine flag so Gatekeeper doesn't block the unsigned binaries.
if [ "$os" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
	xattr -dr com.apple.quarantine "${BIN_DIR}/abctl" "${BIN_DIR}/authbridge-proxy" 2>/dev/null || true
fi

rm -rf "$tmp"
trap - EXIT
fi # end of the skip-if-already-at-this-version guard
fi # end of download block

# offer_path_setup adds BIN_DIR to the shell profile, with consent.
#
# Warning and printing a line to paste was not enough: the first thing a real user hit
# was `abctl: command not found`, before any of the actual bugs. An install that
# succeeds and then cannot run the command it just told you to run is the worst first
# impression available, and the most common one.
#
# Consent, a backup, and a guarded block, matching what `abctl configure claude-code enable` does
# to settings.json — same pattern, no new concept. Declining keeps the old advice.
offer_path_setup() {
	_profile=""
	case "$(basename "${SHELL:-}")" in
		zsh) _profile="${HOME}/.zshrc" ;;
		bash)
			# bash reads .bash_profile for login shells on macOS and .bashrc elsewhere;
			# .profile is read by both when the others are absent, so prefer whichever
			# already exists rather than creating a file the shell may never read.
			for _c in "${HOME}/.bash_profile" "${HOME}/.bashrc" "${HOME}/.profile"; do
				[ -f "$_c" ] && _profile="$_c" && break
			done
			[ -n "${_profile}" ] || _profile="${HOME}/.bash_profile"
			;;
	esac

	if [ -z "${_profile}" ]; then
		# An unknown shell: we do not know which file it reads, and guessing would edit
		# the wrong one.
		warn "${BIN_DIR} is not on your PATH."
		warn "Add it for future sessions:  export PATH=\"${BIN_DIR}:\$PATH\""
		return 0
	fi

	# Already done by an earlier run.
	if [ -f "${_profile}" ] && grep -q "${PATH_MARKER}" "${_profile}" 2>/dev/null; then
		info "${BIN_DIR} is in ${_profile} but not in this shell yet. For this terminal:"
		info "  export PATH=\"${BIN_DIR}:\$PATH\""
		return 0
	fi

	info ""
	info "${BIN_DIR} is not on your PATH, so \`abctl\` will not be found."
	info "This adds two lines to ${_profile}:"
	info "  ${PATH_MARKER}"
	info "  export PATH=\"${BIN_DIR}:\$PATH\""
	if [ -z "${ASSUME_YES}" ]; then
		if [ ! -r /dev/tty ]; then
			warn "no terminal to ask on; add it yourself:  export PATH=\"${BIN_DIR}:\$PATH\""
			return 0
		fi
		printf 'Apply? [y/N] ' > /dev/tty
		read -r _ans < /dev/tty || _ans=""
		case "${_ans}" in
			y | Y | yes | YES) ;;
			*)
				info "Not changed. For this terminal:  export PATH=\"${BIN_DIR}:\$PATH\""
				return 0
				;;
		esac
	fi

	[ -f "${_profile}" ] && cp "${_profile}" "${_profile}.bak"
	{
		printf '\n%s\n' "${PATH_MARKER}"
		# shellcheck disable=SC2016 # $PATH must stay literal: it is expanded by the
		# shell at startup, not by this script now.
		printf 'export PATH="%s:$PATH"\n' "${BIN_DIR}"
	} >> "${_profile}" || {
		warn "could not write ${_profile}; add it yourself:  export PATH=\"${BIN_DIR}:\$PATH\""
		return 0
	}
	info "Added to ${_profile}. It applies to new terminals; for this one:"
	info "  export PATH=\"${BIN_DIR}:\$PATH\""
}

# --- report ---
proxy="${BIN_DIR}/authbridge-proxy"
ca_dir="${CORTEX_DIR}/ca" # matches defaultCortexDir()+caDirName in local.go
case ":${PATH}:" in
	*":${BIN_DIR}:"*) abctl_cmd="abctl" proxy_cmd="authbridge-proxy" ;;
	*) abctl_cmd="${BIN_DIR}/abctl" proxy_cmd="$proxy" ;;
esac

# Both skip paths have already said what they did ("Already at <v>" or "Using the
# binaries already in ..."), so saying "Installed" after them would be both redundant
# and untrue.
if [ -z "${skip_install:-}" ]; then
	info ""
	info "Installed abctl and authbridge-proxy to ${BIN_DIR}"
fi
case ":${PATH}:" in
	*":${BIN_DIR}:"*) ;;
	*)
		offer_path_setup
		;;
esac

if [ "$MODE" = "install-only" ]; then
	info ""
	info "Install-only mode. Start it with:  ${proxy_cmd} --local"
	exit 0
fi

# --- start it: under the OS supervisor when we can, as a plain process when we can't ---
#
# The preferred path hands the proxy to the OS supervisor (launchd/systemd), so it
# survives a crash and a logout — once Claude Code's settings point at the proxy, a
# proxy that dies silently stops Claude Code, most reliably right after a reboot.
#
# But not every environment HAS a usable supervisor: a macOS seatbelt sandbox, a
# container with no user systemd, a minimal or non-systemd distro. There the old flow
# installed the binaries and then died on `launchctl bootstrap failed` / a systemd
# bus error. So we check access first (supervisor_usable), fall back to a plain
# background process when it is missing, and either way print how to start it by hand
# and the environment variables any AI harness needs — not just Claude Code.

# If the supervisor was not already ruled out by --no-service, check now
# whether it can actually be driven. Unusable -> run unsupervised rather than die.
if [ -z "${NO_SERVICE}" ] && ! supervisor_usable; then
	info "Detected a restricted environment: ${SUPERVISOR_CMD} may not be used here (no reachable user session)."
	info "  Cortex will run as a plain background process instead."
	NO_SERVICE=1
fi

# The service subcommand is only needed on the supervised path. Skip the abctl-age
# check entirely when running unsupervised — the proxy binary is all we use there.
if [ -z "${NO_SERVICE}" ]; then
	# This script starts the proxy through `abctl service`, so an abctl that predates
	# that command cannot be driven by it. Say which mismatch it is, rather than
	# letting `unknown subcommand "service"` surface as a bare non-zero exit after the
	# binaries are already installed.
	if ! "${BIN_DIR}/abctl" service status >/dev/null 2>&1 &&
		"${BIN_DIR}/abctl" service 2>&1 | grep -q "unknown subcommand"; then
		# Only offer the matching-release URL when $version really is a tag: under
		# AUTHBRIDGE_SKIP_DOWNLOAD it reads "already installed", which would otherwise
		# be spliced into a nonsense URL.
		case "${version}" in
			v*)
				die "the ${version} abctl has no 'service' command, which this installer needs
  in order to start Cortex. Either use the installer that shipped with it:
    curl -fsSL https://raw.githubusercontent.com/${REPO}/${version}/authbridge/install.sh | sh
  or install newer binaries with this script:
    --ref=<newer tag>"
				;;
			*)
				die "the abctl in ${BIN_DIR} has no 'service' command, which this installer
  needs in order to start Cortex. Install a newer one — drop
  AUTHBRIDGE_SKIP_DOWNLOAD, or pass --ref=<a release that has it>."
				;;
		esac
	fi
fi

# Materialise the config before starting the proxy either way. This used to happen as
# a side effect of starting `--local` in the background; with the service doing the
# starting, nothing else creates the file, and `abctl service install` refuses to run
# without it. The unsupervised start needs it just as much.
if [ ! -f "${CORTEX_DIR}/config.yaml" ]; then
	# Executed by explicit path, and REPORTED by the same explicit path. proxy_cmd is
	# the display form — a bare "authbridge-proxy" when BIN_DIR is on PATH — so naming
	# it here would hand back a command that resolves through PATH, which is not
	# necessarily the binary that just failed. That distinction is the whole point of
	# pinning the path: an older authbridge-proxy earlier on PATH is exactly how the
	# service came up dead in end-to-end testing.
	if ! "${BIN_DIR}/authbridge-proxy" --local --write-config; then
		die "could not write ${CORTEX_DIR}/config.yaml.
  Run this to see why:
    \"${BIN_DIR}/authbridge-proxy\" --local --write-config"
	fi
fi

# A fingerprint of the CA as it stands BEFORE anything starts the proxy, so the summary
# below can tell whether a new one was minted. Sampled here because the proxy mints on
# first start and afterwards it is too late to ask — and before BOTH start paths, since
# the --no-service path mints just as the supervised one does.
#
# A newly-minted CA invalidates every client that was already running — they read their
# CA file once, at startup — and that is invisible to them: the bridge tunnels instead
# of failing, so traffic flows and the parsers just stop seeing it.
#
# Comparing the CONTENT rather than testing for ca.crt's existence, because
# EnsureFileSource mints when ANY of tls.crt / tls.key / ca.crt is missing, not only when
# all three are. A directory holding ca.crt but no tls.key — a truncated copy, a
# half-finished cleanup — gets a brand-new CA while an existence check says "already had
# one" and suppresses the notice, which is exactly the case that needs it. Hashing also
# covers any future change to the minting condition without this line tracking it.
ca_fp_before="$(ca_fingerprint)"

# SUPERVISED records which start path actually took, so the closing summary offers
# `service stop` only where a service really exists.
SUPERVISED=""
if [ -n "${NO_SERVICE}" ]; then
	info ""
	info "Starting Cortex as a background process (no OS service supervisor here)..."
	start_unsupervised || die "could not start the proxy; see ${CORTEX_DIR}/proxy.log"
else
	info ""
	info "Setting up the ${SUPERVISOR_NAME}..."
	set +e
	# --proxy: use the binary this script just installed, not whatever happens to be
	# earlier on PATH. An end-to-end run found the unit pointing at an older
	# authbridge-proxy from another directory, which rejected --supervise and exited,
	# so the service never came up.
	#
	# Capture abctl's COMBINED output (2>&1) through tee: it stays visible live —
	# including "Waiting for the previous Cortex to stop (up to 30s)..." — while also
	# being recorded so service_install_action can read the failure reason. Capturing
	# stderr alone was the bug behind "could not set up the service (exit 1)": abctl
	# prints "launchctl bootstrap failed ... Input/output error" to STDOUT, so the old
	# stderr-only signature matched nothing and the script died instead of falling back.
	# abctl's exit status is carried through the pipe via a status file (POSIX sh has no
	# PIPESTATUS). mktemp, not a predictable "$$" name, avoids the symlink-preplant shape
	# (CWE-59); ensure_tmpdir has resolved a writable TMPDIR by now.
	svc_out_file=$(mktemp "${TMPDIR}/cortex-svc-out.XXXXXX")
	svc_st_file=$(mktemp "${TMPDIR}/cortex-svc-st.XXXXXX")
	{ "${BIN_DIR}/abctl" service install --yes --proxy "${BIN_DIR}/authbridge-proxy" 2>&1; echo $? >"${svc_st_file}"; } | tee "${svc_out_file}"
	svc_status=$(cat "${svc_st_file}" 2>/dev/null || echo 1)
	svc_out=$(cat "${svc_out_file}" 2>/dev/null || true)
	rm -f "${svc_out_file}" "${svc_st_file}"
	set -e
	case "$(service_install_action "${svc_status}" "${svc_out}")" in
		supervised)
			SUPERVISED=1
			;;
		refused)
			# abctl declined on purpose (a config that would expose a listener, say).
			# Running the same proxy unsupervised would defeat that check, so do not.
			die "abctl refused to set up the service — a safety decision, not an
  environment limit, so Cortex was NOT started. Its message was:
    ${svc_out}"
			;;
		ports-busy)
			die "the previous Cortex is still shutting down (its ports are still in use).
  This is temporary — wait a few seconds and re-run. Starting an unsupervised proxy
  now would only crash on the ports the old one still holds."
			;;
		fallback)
			# The OS supervisor cannot take the job here (launchd EIO in a sandbox, an
			# absent systemd user bus, ...). Do exactly what --no-service does rather
			# than leaving Cortex down after a clean install — no flag required. This is
			# reached even when supervisor_usable() passed but the real install failed.
			warn "${SUPERVISOR_CMD} could not set up the service (exit ${svc_status}); running the proxy directly instead"
			start_unsupervised || die "could not start the proxy; see ${CORTEX_DIR}/proxy.log"
			;;
	esac
fi

# tool-prune is in the config but INERT: its remove list is empty, so it does
# nothing until a name is added. That is deliberate for an install.
#
# Filling it here would mean a quickstart whose job is to *observe* traffic
# silently starts *rewriting* it. It is also Claude-Code-specific — the scan reads
# ~/.claude/projects — so for anyone driving a different agent it would be a
# mutation with no upside. Opting in is one command, and it belongs to the person
# who knows whether they want it.
local_cfg="${CORTEX_DIR}/config.yaml"
# Only when we are NOT about to do it ourselves: with --claude-code this told the
# reader to run the exact command that runs two lines later. The prune hint moved to
# the closing summary, so the middle of the flow carries no side quests.
if [ -f "${local_cfg}" ] && [ -z "${WIRE_CLAUDE_CODE}" ]; then
	info "  Point Claude Code at Cortex, then just run \`claude\`:"
	info "    ${abctl_cmd} configure claude-code enable"
	info ""
fi
# --claude-code: hand off to abctl, which owns the JSON merge (a shell-side edit
# of a file holding API tokens is not worth attempting) and prompts on /dev/tty —
# stdin here is the script itself when piped, so it cannot be read for an answer.
if [ -n "${WIRE_CLAUDE_CODE:-}" ]; then
	info ""
	set +e
	if [ -n "${ASSUME_YES}" ]; then
		"${BIN_DIR}/abctl" configure claude-code enable --yes
	else
		"${BIN_DIR}/abctl" configure claude-code enable
	fi
	cc_status=$?
	set -e
	case "${cc_status}" in
		0)
			info ""
			info "  \"${abctl_cmd}\"                         watch traffic — and the \$ saved on every Claude Code prompt"
			info "  \"${abctl_cmd}\" tools scan              propose unused tools to prune (the \$ saved then shows live in \"${abctl_cmd}\")"
			# `service stop` is meaningless where no service could be installed, so do
			# not offer it there — offer the pidfile kill for the unsupervised path.
			if [ -z "${SUPERVISED}" ]; then
				info "  kill \$(cat ${PROXY_PIDFILE})   stop Cortex (unsupervised)"
			else
				info "  \"${abctl_cmd}\" service stop                    stop Cortex"
			fi
			info "  \"${abctl_cmd}\" configure claude-code disable   undo"
			info ""
			# Claude Code is wired up, but other tools/harnesses on this machine still
			# need the environment variables — print them so this install is not
			# Claude-Code-only, and show the manual start when unsupervised.
			if [ -z "${SUPERVISED}" ]; then
				print_local_start_help
				info ""
			fi
			print_env_instructions
			info ""
			exit 0
			;;
		3)
			# Declined, or no terminal to ask on. A normal outcome — fall through to
			# the manual instructions below.
			info ""
			info "  Claude Code left unchanged. To do it later:"
			info "    \"${abctl_cmd}\" configure claude-code enable"
			info ""
			;;
		*)
			# Anything else went wrong (a foreign HTTPS_PROXY, unparseable settings).
			# Reporting that as "left unchanged" and exiting 0 would claim a success
			# that did not happen.
			die "abctl configure claude-code enable failed (exit ${cc_status}); Cortex is running but Claude Code is not configured for it"
			;;
	esac
fi
info "  Watch traffic:   \"${abctl_cmd}\"   (also shows the \$ saved on every Claude Code prompt)"
info "  Prune unused tools to save more:  ${abctl_cmd} tools scan --write ${CORTEX_DIR}/config.yaml"
info "    (tools scan only proposes the prune list; the actual \$ saved shows live in \"${abctl_cmd}\".)"
info ""
if [ -z "${SUPERVISED}" ]; then
	print_local_start_help
	info ""
fi
# Said only when the CA actually CHANGED just now, and said late so it is the last
# thing on screen rather than scrolled past. Silent on the common case — an upgrade
# leaves all three CA files in place, so nothing is minted and nothing already running
# is affected.
ca_fp_after="$(ca_fingerprint)"
if [ -n "${ca_fp_after}" ] && [ "${ca_fp_before}" != "${ca_fp_after}" ]; then
	info "  NOTE: a new CA was created for this machine."
	info "    Agents that were ALREADY RUNNING trust a different CA (or none) and"
	info "    cannot be observed until restarted — a client reads its CA file once,"
	info "    at startup. Traffic still flows, so nothing on their side will complain:"
	info "    it tunnels through unparsed instead. Restart them to see their traffic."
	info ""
fi
# The full, harness-agnostic environment block. `abctl configure claude-code enable` wires
# these into ~/.claude/settings.json for Claude Code specifically; the variables
# below are what every OTHER tool or agent needs, and are printed unconditionally so
# this install is never Claude-Code-only.
print_env_instructions
info ""
info "  Wire up Claude Code specifically (writes ~/.claude/settings.json):"
info "    ${abctl_cmd} configure claude-code enable"
info ""
if [ -n "${SUPERVISED}" ]; then
	info "  Stop it:         \"${abctl_cmd}\" service stop      (start / restart / status too)"
else
	info "  Stop it:         kill \$(cat ${PROXY_PIDFILE})   (running unsupervised)"
fi
info ""
