#!/bin/sh
# Tests for check-gateway-linkage.sh.
#
# Black-box: each case puts stub `uname`, `otool` and `readelf` scripts first
# on PATH, and a stub binary that prints a given --version line. No build.
#
# Run: sh scripts/check-gateway-linkage_test.sh
set -eu

# shellcheck disable=SC1007 # `CDPATH= cd` empties CDPATH for this one command.
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
CHECK_SH="${SCRIPT_DIR}/check-gateway-linkage.sh"
[ -f "${CHECK_SH}" ] || { printf 'cannot find %s\n' "${CHECK_SH}" >&2; exit 1; }

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

PASS=0
FAIL=0
N=0

ok() { PASS=$((PASS + 1)); printf '  ok   %s\n' "$1"; }
bad() { FAIL=$((FAIL + 1)); printf '  FAIL %s: %s\n' "$1" "$2"; }

# stub <name> <exit> <output>: a stub command that prints <output> and exits.
stub() {
	printf '%s\n' "$3" >"${CASE}/$1.out"
	printf '#!/bin/sh\ncat "%s"\nexit %s\n' "${CASE}/$1.out" "$2" >"${CASE}/bin/$1"
	chmod +x "${CASE}/bin/$1"
}

# setup <os> <version-line>: a fresh case dir with a uname stub and a binary.
setup() {
	N=$((N + 1))
	CASE="${TMP}/case${N}"
	mkdir -p "${CASE}/bin"
	stub uname 0 "$1"
	printf '#!/bin/sh\necho "%s"\n' "$2" >"${CASE}/gateway"
	chmod +x "${CASE}/gateway"
}

# run_check <version>: run the check. Exit status in $STATUS, stderr in $ERR.
run_check() {
	STATUS=0
	PATH="${CASE}/bin:${PATH}" sh "${CHECK_SH}" "${CASE}/gateway" "$1" \
		>/dev/null 2>"${CASE}/err" || STATUS=$?
	ERR=$(cat "${CASE}/err")
}

expect() { # label want-status [stderr-substring]
	if [ "${STATUS}" != "$2" ]; then
		bad "$1" "exit ${STATUS}, want $2 (stderr: ${ERR})"
	elif [ -n "${3:-}" ] && ! printf '%s' "${ERR}" | grep -qF -- "$3"; then
		bad "$1" "stderr lacks '$3' (stderr: ${ERR})"
	else
		ok "$1"
	fi
}

TAB=$(printf '\t')

test_linux_static_passes() {
	setup Linux "cortex-gateway v1.2.3"
	stub readelf 0 "There is no dynamic section in this file."
	run_check v1.2.3
	expect test_linux_static_passes 0
}

test_linux_needed_fails() {
	setup Linux "cortex-gateway v1.2.3"
	stub readelf 0 " 0x0000000000000001 (NEEDED)             Shared library: [libssl.so.3]"
	run_check v1.2.3
	expect test_linux_needed_fails 1 libssl.so.3
}

test_readelf_error_fails() {
	setup Linux "cortex-gateway v1.2.3"
	stub readelf 1 ""
	run_check v1.2.3
	expect test_readelf_error_fails 1
}

test_darwin_system_libs_pass() {
	setup Darwin "cortex-gateway v1.2.3"
	stub otool 0 "${CASE}/gateway:
${TAB}/usr/lib/libSystem.B.dylib (compatibility version 1.0.0, current version 1351.0.0)
${TAB}/System/Library/Frameworks/Security.framework/Versions/A/Security (compatibility version 1.0.0, current version 61439.0.0)"
	run_check v1.2.3
	expect test_darwin_system_libs_pass 0
}

test_darwin_homebrew_ssl_fails() {
	setup Darwin "cortex-gateway v1.2.3"
	stub otool 0 "${CASE}/gateway:
${TAB}/usr/lib/libSystem.B.dylib (compatibility version 1.0.0, current version 1351.0.0)
${TAB}/opt/homebrew/opt/openssl@3/lib/libssl.3.dylib (compatibility version 3.0.0, current version 3.0.0)"
	run_check v1.2.3
	expect test_darwin_homebrew_ssl_fails 1 libssl.3.dylib
}

test_version_mismatch_fails() {
	setup Linux "cortex-gateway dev"
	stub readelf 0 "There is no dynamic section in this file."
	run_check v1.2.3
	expect test_version_mismatch_fails 1 "cortex-gateway dev"
}

test_linux_static_passes
test_linux_needed_fails
test_readelf_error_fails
test_darwin_system_libs_pass
test_darwin_homebrew_ssl_fails
test_version_mismatch_fails

printf '%s passed, %s failed\n' "${PASS}" "${FAIL}"
[ "${FAIL}" = 0 ]
