#!/usr/bin/env bash
# fetch-cpex-ffi.sh — download and verify the pinned CPEX FFI static library.
#
# Vendored from CPEX's scripts/download-ffi-artifact.sh (v0.3.0) and tightened
# for this repo. It downloads the libcpex_ffi.a release tarball for one target
# tuple and unpacks it into a directory cgo can link from. Before anything is
# unpacked, the tarball must:
#
#   - match the digest pinned in cmd/cortex-cpex/CPEX_FFI_SHA256 (and the
#     release's own .sha256 file);
#   - carry a cosign signature from CPEX's release workflow at exactly the
#     pinned tag, issued by GitHub's OIDC issuer;
#   - report VERSION == CPEX_FFI_VERSION and FFI_ABI == CPEX_FFI_ABI.
#
# On any failure the script exits non-zero and leaves no library behind.
#
# Inputs (environment):
#   CPEX_FFI_VERSION   Release tag. Default: cmd/cortex-cpex/CPEX_FFI_VERSION.
#   CPEX_FFI_ABI       Expected FFI ABI. Default: cmd/cortex-cpex/CPEX_FFI_ABI.
#   CPEX_FFI_SHA256_FILE
#                      Pinned digests, "<sha256>  <tarball>" per line.
#                      Default: cmd/cortex-cpex/CPEX_FFI_SHA256.
#   CPEX_FFI_TARGET    Tuple (linux-amd64-gnu, linux-arm64-gnu,
#                      linux-amd64-musl, linux-arm64-musl, darwin-arm64).
#                      Detected from uname and the libc when unset.
#   CPEX_FFI_DEST      Destination. Default: .cpex-ffi/<version>/<tuple>.
#   CPEX_FFI_REPO      GitHub owner/repo. Default: contextforge-org/cpex.
#   CPEX_FFI_BASE_URL  Asset URL prefix override (file:// works), for tests.
#   CPEX_FFI_SKIP_COSIGN
#                      "1" skips the signature check, for offline use only.
#                      The pinned digest is still enforced.
#
# Output: the absolute destination directory on stdout. Link with
#   CGO_LDFLAGS="-L$(scripts/fetch-cpex-ffi.sh) -lcpex_ffi"

set -euo pipefail

err() { echo "fetch-cpex-ffi: error: $*" >&2; exit 1; }
info() { echo "fetch-cpex-ffi: $*" >&2; } # stdout carries only the dest path

PIN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../cmd/cortex-cpex" 2>/dev/null && pwd || true)"
read_pin() { [[ -n "$PIN_DIR" && -f "$PIN_DIR/$1" ]] && tr -d '[:space:]' <"$PIN_DIR/$1"; }

CPEX_FFI_VERSION="${CPEX_FFI_VERSION:-$(read_pin CPEX_FFI_VERSION || true)}"
CPEX_FFI_ABI="${CPEX_FFI_ABI:-$(read_pin CPEX_FFI_ABI || true)}"
CPEX_FFI_SHA256_FILE="${CPEX_FFI_SHA256_FILE:-$PIN_DIR/CPEX_FFI_SHA256}"
CPEX_FFI_REPO="${CPEX_FFI_REPO:-contextforge-org/cpex}"
[[ -n "$CPEX_FFI_VERSION" ]] || err "CPEX_FFI_VERSION is not set and no pin file was found"
[[ -n "$CPEX_FFI_ABI" ]] || err "CPEX_FFI_ABI is not set and no pin file was found"
[[ -f "$CPEX_FFI_SHA256_FILE" ]] || err "pinned digest file not found: $CPEX_FFI_SHA256_FILE"

detect_tuple() {
    local os arch libc
    os="$(uname -s)"
    arch="$(uname -m)"
    case "$os" in
        Linux)
            if (ldd --version 2>&1 || true) | grep -qi musl; then
                libc="musl"
            elif compgen -G "/lib/ld-musl-*" >/dev/null; then
                libc="musl"
            else
                libc="gnu"
            fi
            case "$arch" in
                x86_64) echo "linux-amd64-${libc}" ;;
                aarch64 | arm64) echo "linux-arm64-${libc}" ;;
                *) err "unsupported linux arch: $arch" ;;
            esac
            ;;
        Darwin)
            case "$arch" in
                arm64) echo "darwin-arm64" ;;
                *) err "unsupported darwin arch: $arch" ;;
            esac
            ;;
        *) err "unsupported OS: $os" ;;
    esac
}

sha256_of() {
    if command -v sha256sum >/dev/null; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

manifest_version() { grep -E '^version=' "$1/VERSION" | head -n1 | cut -d= -f2- | tr -d '[:space:]'; }

CPEX_FFI_TARGET="${CPEX_FFI_TARGET:-$(detect_tuple)}"
CPEX_FFI_DEST="${CPEX_FFI_DEST:-./.cpex-ffi/${CPEX_FFI_VERSION}/${CPEX_FFI_TARGET}}"
TARBALL="cpex-ffi-${CPEX_FFI_VERSION}-${CPEX_FFI_TARGET}.tar.gz"
info "version=$CPEX_FFI_VERSION abi=$CPEX_FFI_ABI target=$CPEX_FFI_TARGET dest=$CPEX_FFI_DEST"

pinned="$(awk -v f="$TARBALL" '$2 == f {print $1}' "$CPEX_FFI_SHA256_FILE")"
[[ -n "$pinned" ]] || err "no pinned digest for $TARBALL in $CPEX_FFI_SHA256_FILE"

# A previous verified run leaves VERSION, FFI_ABI and the library together,
# with .pinned-sha256 naming the digest of the tarball they came from. A
# cache without it, or from a tarball pinned differently, is fetched again.
if [[ -f "$CPEX_FFI_DEST/VERSION" && -f "$CPEX_FFI_DEST/FFI_ABI" && -f "$CPEX_FFI_DEST/libcpex_ffi.a" ]] &&
    [[ -f "$CPEX_FFI_DEST/.pinned-sha256" && "$(tr -d '[:space:]' <"$CPEX_FFI_DEST/.pinned-sha256")" == "$pinned" ]] &&
    [[ "$(manifest_version "$CPEX_FFI_DEST")" == "$CPEX_FFI_VERSION" ]] &&
    [[ "$(tr -d '[:space:]' <"$CPEX_FFI_DEST/FFI_ABI")" == "$CPEX_FFI_ABI" ]]; then
    info "already present; skipping download"
    (cd "$CPEX_FFI_DEST" && pwd)
    exit 0
fi

# Replace only a directory this script created: one with .pinned-sha256,
# or an empty one. Anything else at that path is not ours to delete.
if [[ -e "$CPEX_FFI_DEST" || -L "$CPEX_FFI_DEST" ]]; then
    [[ -d "$CPEX_FFI_DEST" && ! -L "$CPEX_FFI_DEST" ]] ||
        err "CPEX_FFI_DEST $CPEX_FFI_DEST exists and is not a directory; refusing to replace it"
    if [[ ! -f "$CPEX_FFI_DEST/.pinned-sha256" && -n "$(ls -A "$CPEX_FFI_DEST")" ]]; then
        err "CPEX_FFI_DEST $CPEX_FFI_DEST is not empty and holds no .pinned-sha256, so this script did not create it; refusing to replace it (remove it, or pick another CPEX_FFI_DEST)"
    fi
fi

# Clear the destination now, so a failure below cannot leave an older or
# unverified library in place.
rm -rf "$CPEX_FFI_DEST"

BASE_URL="${CPEX_FFI_BASE_URL:-https://github.com/${CPEX_FFI_REPO}/releases/download/${CPEX_FFI_VERSION}}"
WORK_DIR="$(mktemp -d)"
STAGE=""
trap 'rm -rf "$WORK_DIR" ${STAGE:+"$STAGE"}' EXIT

fetch() {
    local url="${BASE_URL}/$1"
    info "  GET $url"
    if [[ "$url" == file://* ]]; then
        cp "${url#file://}" "$WORK_DIR/$1" || err "failed to copy $url"
    else
        curl -fsSL --retry 3 --retry-delay 2 -o "$WORK_DIR/$1" "$url" || err "failed to download $url"
    fi
}

fetch "$TARBALL"
fetch "$TARBALL.sha256"

actual="$(sha256_of "$WORK_DIR/$TARBALL")"
[[ "$actual" == "$pinned" ]] || err "sha256 mismatch for $TARBALL: pinned $pinned, got $actual"
published="$(awk '{print $1; exit}' "$WORK_DIR/$TARBALL.sha256")"
[[ "$published" == "$pinned" ]] || err "release .sha256 ($published) disagrees with the pinned digest ($pinned)"
info "sha256 verified"

if [[ "${CPEX_FFI_SKIP_COSIGN:-0}" == "1" ]]; then
    info "WARN: skipping cosign verification (CPEX_FFI_SKIP_COSIGN=1)"
else
    command -v cosign >/dev/null || err "cosign is required (CPEX_FFI_SKIP_COSIGN=1 skips it, offline use only)"
    fetch "$TARBALL.sig"
    fetch "$TARBALL.crt"
    cosign verify-blob \
        --certificate "$WORK_DIR/$TARBALL.crt" \
        --signature "$WORK_DIR/$TARBALL.sig" \
        --certificate-identity "https://github.com/${CPEX_FFI_REPO}/.github/workflows/release-ffi.yaml@refs/tags/${CPEX_FFI_VERSION}" \
        --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
        "$WORK_DIR/$TARBALL" >/dev/null 2>&1 ||
        err "cosign verification failed for $TARBALL"
    info "cosign signature verified"
fi

# Unpack next to the destination so the final rename is atomic.
mkdir -p "$(dirname "$CPEX_FFI_DEST")"
STAGE="$(mktemp -d "$(dirname "$CPEX_FFI_DEST")/.cpex-ffi-stage.XXXXXX")"
tar -xzf "$WORK_DIR/$TARBALL" -C "$STAGE"
[[ -f "$STAGE/libcpex_ffi.a" ]] || err "libcpex_ffi.a missing from $TARBALL"
[[ -f "$STAGE/VERSION" && -f "$STAGE/FFI_ABI" ]] || err "VERSION or FFI_ABI missing from $TARBALL"
got_version="$(manifest_version "$STAGE")"
[[ "$got_version" == "$CPEX_FFI_VERSION" ]] || err "VERSION mismatch: tarball says '$got_version', expected '$CPEX_FFI_VERSION'"
got_abi="$(tr -d '[:space:]' <"$STAGE/FFI_ABI")"
[[ "$got_abi" == "$CPEX_FFI_ABI" ]] || err "FFI_ABI mismatch: tarball says '$got_abi', expected '$CPEX_FFI_ABI' (bump CPEX_FFI_ABI and the Go module together)"

echo "$pinned" >"$STAGE/.pinned-sha256"
mv "$STAGE" "$CPEX_FFI_DEST"
STAGE=""
info "installed libcpex_ffi.a (VERSION=$got_version FFI_ABI=$got_abi)"
(cd "$CPEX_FFI_DEST" && pwd)
