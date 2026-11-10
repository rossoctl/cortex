#!/bin/sh
# Build the cortex-gateway release binary for the host this runs on, then
# check that it is standalone (scripts/check-gateway-linkage.sh).
#
# macOS: a native build. Linux: run it in rust:alpine for a static musl binary.
# The release job, the weekly Praxis job and the local gate all run this file.
#
# Usage (from the repo root): VERSION=v1.2.3 sh scripts/build-gateway.sh
# Writes dist/cortex-gateway. Optional env: MACOSX_DEPLOYMENT_TARGET (12.0).
set -eu

[ -n "${VERSION:-}" ] || { echo "build-gateway: VERSION is required" >&2; exit 1; }

if [ -f /etc/alpine-release ]; then
	# perl and make build openssl-src, make builds jemalloc, cmake builds
	# libz-ng-sys, binutils gives readelf to the linkage check.
	apk add --no-cache musl-dev perl make cmake linux-headers binutils
fi

# rustup reads rust-toolchain.toml only from the current directory and its
# parents, not from --manifest-path.
cd praxis
# No self-update: the build needs only the pinned toolchain.
rustup toolchain install --no-self-update
# Put the pinned toolchain first, before any cargo or rustc not from rustup
# (for example Homebrew).
PATH="$(dirname "$(rustup which cargo)"):${PATH}"

triple=$(rustc -vV | sed -n 's/^host: //p')
case $(uname -s) in
Linux)
	export RUSTFLAGS="-C target-feature=+crt-static"
	case ${triple} in
	aarch64-*)
		# jemalloc fixes the page size at build time. 64 KiB works on 4 KiB
		# and 64 KiB page hosts, 4 KiB crashes on 64 KiB ones.
		export JEMALLOC_SYS_WITH_LG_PAGE=16
		;;
	esac
	;;
Darwin)
	# Go 1.26 binaries need macOS 12, so the gateway claims the same floor.
	export MACOSX_DEPLOYMENT_TARGET="${MACOSX_DEPLOYMENT_TARGET:-12.0}"
	;;
esac

# --target keeps RUSTFLAGS away from build scripts and proc-macros (which
# cannot be crt-static), and gives each host triple its own output dir.
# CORTEX_VERSION: cortex refuses a gateway whose version differs from its own.
# STRIP: the Go release strips too, and praxis/Cargo.toml stays unchanged.
CORTEX_VERSION="${VERSION}" CARGO_PROFILE_RELEASE_STRIP=symbols \
	cargo build --release --locked -p cortex-gateway --target "${triple}"

mkdir -p ../dist
cp "target/${triple}/release/cortex-gateway" ../dist/cortex-gateway
cd ..
sh scripts/check-gateway-linkage.sh dist/cortex-gateway "${VERSION}"
