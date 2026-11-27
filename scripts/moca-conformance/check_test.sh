#!/bin/sh
# Tests for check.sh, on canned runner output. No build, no moca.
#
# Run: sh scripts/moca-conformance/check_test.sh
set -eu

# shellcheck disable=SC1007 # `CDPATH= cd` empties CDPATH for this one command.
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
CHECK_SH="${SCRIPT_DIR}/check.sh"
[ -f "${CHECK_SH}" ] || { printf 'cannot find %s\n' "${CHECK_SH}" >&2; exit 1; }

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

PASS=0
FAIL=0

ok() { PASS=$((PASS + 1)); printf '  ok   %s\n' "$1"; }
bad() { FAIL=$((FAIL + 1)); printf '  FAIL %s: %s\n' "$1" "$2"; }

# The runner's output for three cases, one passing.
printf '%s\n' \
	'PASS conn_ok' \
	'FAIL cred_inject: CONNECT up.test:1: status 502' \
	'FAIL audit_query: status 400' >"${TMP}/out"

# run_check <known-failures text> <runner exit> [output file]: status in $STATUS, stderr in $ERR.
run_check() {
	printf '%s\n' "$1" >"${TMP}/known"
	STATUS=0
	sh "${CHECK_SH}" "${TMP}/known" "${3:-${TMP}/out}" "$2" >/dev/null 2>"${TMP}/err" || STATUS=$?
	ERR=$(cat "${TMP}/err")
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

run_check "$(printf '# header\ncred_inject # #1\n  audit_query  \n')" 1
expect known_failures_pass 0

run_check "cred_inject" 1
expect new_failure_fails 1 "audit_query"

run_check "$(printf 'cred_inject\naudit_query\nconn_ok\ngone\n')" 1
expect listed_pass_or_missing_fails 1 "gone"

printf 'FAIL New-Case: boom\r\n' >"${TMP}/odd"
run_check "" 1 "${TMP}/odd"
expect any_name_fails 1 "New-Case"

printf 'PASS conn_ok\n' >"${TMP}/passed"
run_check "" 1 "${TMP}/passed"
expect exit_without_failure_fails 1 "do not match"

run_check "$(printf 'cred_inject\naudit_query\n')" 2
expect runner_error_fails 1 "exited 2"

: >"${TMP}/empty"
run_check "" 0 "${TMP}/empty"
expect no_cases_fails 1 "no case"

printf '%s passed, %s failed\n' "${PASS}" "${FAIL}"
[ "${FAIL}" = 0 ]
