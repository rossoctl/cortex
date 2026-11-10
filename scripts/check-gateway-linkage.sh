#!/bin/sh
# Check that a cortex-gateway release binary is standalone and has the
# release version.
#
# Linux: the ELF must have no NEEDED entry (static musl).
# macOS: every library in `otool -L` must be below /usr/lib/ or /System/Library/.
# Both: `<binary> --version` must print exactly `cortex-gateway <version>`,
# because `cortex` refuses a gateway whose version differs from its own.
#
# Usage: sh scripts/check-gateway-linkage.sh <binary> <version>
# Exit 0 when the binary passes. Exit 1 otherwise, with the reason on stderr.
set -eu

die() { printf 'check-gateway-linkage: %s\n' "$*" >&2; exit 1; }

[ $# -eq 2 ] || die "usage: $0 <binary> <version>"
bin=$1
version=$2
[ -f "${bin}" ] || die "${bin}: no such file"

case $(uname -s) in
Linux)
	# A readelf failure must not read as "no NEEDED lines".
	out=$(readelf -d "${bin}") || die "readelf -d ${bin} failed"
	[ -n "${out}" ] || die "readelf -d ${bin} printed nothing"
	needed=$(printf '%s\n' "${out}" | grep -F '(NEEDED)' || true)
	[ -z "${needed}" ] || die "${bin} links shared libraries:
${needed}"
	;;
Darwin)
	out=$(otool -L "${bin}") || die "otool -L ${bin} failed"
	[ -n "${out}" ] || die "otool -L ${bin} printed nothing"
	# The first line is the binary's own path.
	bad=$(printf '%s\n' "${out}" | sed 1d | awk '{print $1}' |
		grep -v -e '^/usr/lib/' -e '^/System/Library/' || true)
	[ -z "${bad}" ] || die "${bin} links libraries outside /usr/lib/ and /System/Library/:
${bad}"
	;;
*)
	die "unsupported OS: $(uname -s)"
	;;
esac

got=$("${bin}" --version) || die "${bin} --version failed"
[ "${got}" = "cortex-gateway ${version}" ] ||
	die "${bin} --version printed '${got}', want 'cortex-gateway ${version}'"
