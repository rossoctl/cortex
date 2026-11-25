#!/bin/sh
# check.sh compares one run of moca's egress conformance suite with the cases
# known to fail. It fails on a failing case that is not listed, on a listed case
# that passes or no longer runs, and on a runner that exits with anything but
# 0 (every case passed) or 1 (a case failed).
#
# Usage: sh scripts/moca-conformance/check.sh <known-failures> <runner-output> <runner-exit>
set -eu

[ $# -eq 3 ] || { echo "usage: check.sh <known-failures> <runner-output> <runner-exit>" >&2; exit 2; }
known=$1 out=$2 code=$3

case "${code}" in
0 | 1) ;;
*) echo "moca-conformance: the runner exited ${code}, so the suite did not complete" >&2; exit 1 ;;
esac

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

# One case name per line; # starts a comment.
sed 's/#.*//; s/[[:space:]]//g; /^$/d' "${known}" | sort -u >"${TMP}/listed"
tr -d '\r' <"${out}" >"${TMP}/out"
sed -nE 's/^FAIL ([^ :]+):.*/\1/p' "${TMP}/out" | sort -u >"${TMP}/failed"
sed -nE 's/^(PASS|FAIL) ([^ :]+).*/\2/p' "${TMP}/out" | sort -u >"${TMP}/ran"

if [ ! -s "${TMP}/ran" ]; then
	echo "moca-conformance: the runner reported no case" >&2
	exit 1
fi
# The exit code and the parsed failures agree, so a line this script misreads fails the job.
if { [ "${code}" -eq 1 ] && [ ! -s "${TMP}/failed" ]; } || { [ "${code}" -eq 0 ] && [ -s "${TMP}/failed" ]; }; then
	echo "moca-conformance: the runner exited ${code}, which its FAIL lines do not match" >&2
	exit 1
fi

comm -23 "${TMP}/failed" "${TMP}/listed" >"${TMP}/new"
comm -23 "${TMP}/listed" "${TMP}/failed" >"${TMP}/stale"
status=0
if [ -s "${TMP}/new" ]; then
	echo "moca-conformance: these cases fail and are not in ${known}:" >&2
	sed 's/^/  /' "${TMP}/new" >&2
	status=1
fi
if [ -s "${TMP}/stale" ]; then
	echo "moca-conformance: these cases are in ${known} but pass or no longer run; remove them:" >&2
	sed 's/^/  /' "${TMP}/stale" >&2
	status=1
fi
[ "${status}" -ne 0 ] || echo "moca-conformance: $(wc -l <"${TMP}/ran" | tr -d ' ') cases ran, $(wc -l <"${TMP}/failed" | tr -d ' ') known failures"
exit "${status}"
