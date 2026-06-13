# AuthBridge CPEX-enabled sidecar image — authbridge-proxy (HTTP
# forward + reverse proxies) plus the `cpex` plugin which routes
# hooks through the CPEX framework (APL DSL + named CPEX policy
# plugins). Built with -tags cpex; links libcpex_ffi.a from a
# pinned CPEX release.
#
# Build context: ./authbridge (needs authlib/ and cmd/authbridge-cpex/)
#
# Why not just authbridge-proxy: that image is pure-Go, CGO_ENABLED=0.
# CPEX requires cgo. Operators switch images (not build flags) to
# enable CPEX.

# ---------- Stage 1: fetch libcpex_ffi.a from the pinned release ----
# The CPEX_FFI_VERSION file at the root of this directory pins the
# release tag; CPEX_FFI_ABI pins the FFI ABI integer the linked lib must
# report. Both are asserted at build time below: the extracted tarball's
# VERSION must equal CPEX_FFI_VERSION and its FFI_ABI must equal
# CPEX_FFI_ABI, so a renamed/mismatched asset fails the build rather than
# linking a lib whose ABI the Go bindings don't expect. The bindings
# additionally assert the ABI at runtime (go/cpex/abi.go panics at init()
# if the linked lib's cpex_ffi_abi_version() != its expected value), so a
# drift is caught both at build and at boot.
#
# Asset naming on the cpex release page:
#   cpex-ffi-${VERSION}-${OS}-${ARCH}-${LIBC}.tar.gz   (tarball)
#   cpex-ffi-${VERSION}-${OS}-${ARCH}-${LIBC}.tar.gz.sha256
#   cpex-ffi-${VERSION}-${OS}-${ARCH}-${LIBC}.tar.gz.sig   (cosign)
#   cpex-ffi-${VERSION}-${OS}-${ARCH}-${LIBC}.tar.gz.crt   (cosign)
#
# Tarball contents:
#   ./libcpex_ffi.a
#   ./VERSION
#   ./FFI_ABI
#   ./LICENSE
#
# This stage fetches the tarball + its .sha256 + cosign signature,
# verifies the checksum AND the cosign signature, then extracts the .a.
FROM alpine:3.20@sha256:beefdbd8a1da6d2915566fde36db9db0b524eb737fc57cd1367effd16dc0d06d AS ffi

RUN apk add --no-cache curl ca-certificates tar cosign

ARG CPEX_FFI_VERSION
# CPEX_FFI_ABI pins the FFI ABI integer the linked lib must report. The
# Makefile sources it from the CPEX_FFI_ABI file (same dir as
# CPEX_FFI_VERSION); the extract step below asserts the tarball matches.
ARG CPEX_FFI_ABI
ARG TARGETARCH
ARG TARGETOS=linux
# LIBC defaults to gnu (Debian/Ubuntu container hosts). Override to
# `musl` for Alpine-based deployments.
ARG CPEX_LIBC=gnu
ARG FFI_ASSET=cpex-ffi-${CPEX_FFI_VERSION}-${TARGETOS}-${TARGETARCH}-${CPEX_LIBC}.tar.gz

ARG CPEX_COSIGN_IDENTITY=https://github.com/contextforge-org/cpex/.github/workflows/release-ffi.yaml@refs/tags/${CPEX_FFI_VERSION}
ARG CPEX_COSIGN_ISSUER=https://token.actions.githubusercontent.com

WORKDIR /ffi
RUN set -eu; \
    BASE="https://github.com/contextforge-org/cpex/releases/download/${CPEX_FFI_VERSION}"; \
    echo "[ffi] downloading ${BASE}/${FFI_ASSET}"; \
    curl -fsSLo asset.tar.gz "${BASE}/${FFI_ASSET}"; \
    curl -fsSLo asset.tar.gz.sha256 "${BASE}/${FFI_ASSET}.sha256"; \
    curl -fsSLo asset.tar.gz.sig "${BASE}/${FFI_ASSET}.sig"; \
    curl -fsSLo asset.tar.gz.crt "${BASE}/${FFI_ASSET}.crt"; \
    # SHA-256 integrity check. \
    expected_hash=$(awk '{print $1}' asset.tar.gz.sha256); \
    actual_hash=$(sha256sum asset.tar.gz | awk '{print $1}'); \
    if [ "$expected_hash" != "$actual_hash" ]; then \
        echo "[ffi] sha256 mismatch: expected $expected_hash, got $actual_hash" >&2; \
        exit 1; \
    fi; \
    echo "[ffi] sha256 verified"; \
    # Cosign signature verification — proves the asset was produced by \
    # the cpex release workflow and hasn't been tampered with. \
    cosign verify-blob \
        --signature asset.tar.gz.sig \
        --certificate asset.tar.gz.crt \
        --certificate-identity "${CPEX_COSIGN_IDENTITY}" \
        --certificate-oidc-issuer "${CPEX_COSIGN_ISSUER}" \
        asset.tar.gz \
    && echo "[ffi] cosign signature verified" \
    || { echo "[ffi] cosign verification FAILED" >&2; exit 1; }; \
    tar -xzf asset.tar.gz; \
    test -f libcpex_ffi.a || { echo "[ffi] libcpex_ffi.a missing from tarball" >&2; exit 1; }; \
    # Assert the tarball's pinned identity matches what we expect. A \
    # mismatched VERSION means the asset was renamed/swapped; a mismatched \
    # FFI_ABI means the lib's C surface differs from what the Go bindings \
    # were generated against (a CGO mis-marshal waiting to happen). \
    # VERSION is a multi-line key=value manifest (version=, git_sha=, \
    # tuple=, …); pull the version= line. FFI_ABI is a bare integer. \
    got_version=$(grep -E '^version=' VERSION | head -1 | cut -d= -f2- | tr -d '[:space:]'); \
    if [ "$got_version" != "$CPEX_FFI_VERSION" ]; then \
        echo "[ffi] VERSION mismatch: tarball says '$got_version', expected '$CPEX_FFI_VERSION'" >&2; \
        exit 1; \
    fi; \
    got_abi=$(tr -d '[:space:]' < FFI_ABI); \
    if [ -n "$CPEX_FFI_ABI" ] && [ "$got_abi" != "$CPEX_FFI_ABI" ]; then \
        echo "[ffi] FFI_ABI mismatch: tarball says '$got_abi', expected '$CPEX_FFI_ABI' (bump CPEX_FFI_ABI + the Go binding)" >&2; \
        exit 1; \
    fi; \
    echo "[ffi] extracted libcpex_ffi.a + VERSION=$got_version + FFI_ABI=$got_abi (asserted)"

# ---------- Stage 2: build authbridge-cpex with cgo + -tags cpex ----
# golang:bookworm (glibc) — alpine + musl would need extra ring/
# tokio cross-compile setup that the design doc defers to PR 2.
FROM golang:1.25-bookworm@sha256:bb979b278ffb8d31c8b07336fd187ef8fafc8766ebeaece524304483ea137e96 AS builder

RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY authlib/ authlib/
COPY cmd/authbridge-cpex/ cmd/authbridge-cpex/

COPY --from=ffi /ffi/libcpex_ffi.a /libs/libcpex_ffi.a

ENV GOWORK=off
ENV CGO_ENABLED=1
ENV CGO_LDFLAGS="-L/libs -lcpex_ffi -lm -ldl"
# Let Go auto-download the toolchain version go.mod requires. The
# pinned base image carries 1.25.1; cpex bindings need 1.25.4 (and
# transitively bumps our cmd/authbridge-cpex/go.mod). GOTOOLCHAIN=auto
# means "download the version go.mod asks for" — keeps the base image
# pin stable across toolchain bumps.
ENV GOTOOLCHAIN=auto
RUN cd cmd/authbridge-cpex && \
    go build -tags cpex -ldflags="-s -w" -o /authbridge-cpex .

# ---------- Stage 3: runtime ----------------------------------------
# debian:trixie (Debian 13) for GLIBC 2.39+ — the cpex release .a is
# built against that floor. bookworm (Debian 12, GLIBC 2.36) link-loads
# but fails at runtime with "GLIBC_2.39 not found". When we eventually
# move to a statically-linked CGO build (or the musl asset variant),
# we can downgrade this back to the smaller base.
FROM debian:trixie-slim@sha256:b6e2a152f22a40ff69d92cb397223c906017e1391a73c952b588e51af8883bf8

RUN apt-get update && apt-get install -y --no-install-recommends \
        bash ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder --chmod=755 /authbridge-cpex /usr/local/bin/authbridge-cpex
COPY --chmod=755 cmd/authbridge-cpex/entrypoint.sh /usr/local/bin/entrypoint.sh

USER 1001

EXPOSE 8080 8081 9091 9093 9094

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
