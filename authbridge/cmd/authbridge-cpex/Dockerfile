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
# The CPEX_FFI_VERSION file at the root of this directory is the
# single source of truth for which CPEX FFI ABI this binary expects.
# A pre-commit hook (TODO follow-up) enforces it matches the go.mod
# pin for the cpex Go bindings.
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
# This stage fetches the tarball + its .sha256 sidecar, verifies the
# checksum, extracts the .a. Cosign verification is a follow-up (we
# have the .sig/.crt to wire it up); for now the sha256 is the
# integrity check.
FROM alpine:3.20@sha256:beefdbd8a1da6d2915566fde36db9db0b524eb737fc57cd1367effd16dc0d06d AS ffi

RUN apk add --no-cache curl ca-certificates tar

ARG CPEX_FFI_VERSION
ARG TARGETARCH
ARG TARGETOS=linux
# LIBC defaults to gnu (Debian/Ubuntu container hosts). Override to
# `musl` for Alpine-based deployments.
ARG CPEX_LIBC=gnu
ARG FFI_ASSET=cpex-ffi-${CPEX_FFI_VERSION}-${TARGETOS}-${TARGETARCH}-${CPEX_LIBC}.tar.gz

WORKDIR /ffi
RUN set -eu; \
    BASE="https://github.com/contextforge-org/cpex/releases/download/${CPEX_FFI_VERSION}"; \
    echo "[ffi] downloading ${BASE}/${FFI_ASSET}"; \
    curl -fsSLo asset.tar.gz "${BASE}/${FFI_ASSET}"; \
    curl -fsSLo asset.tar.gz.sha256 "${BASE}/${FFI_ASSET}.sha256"; \
    # sha256 file format: "<hash>  <filename>" — rename our local copy \
    # so the line matches, then verify. \
    expected_hash=$(awk '{print $1}' asset.tar.gz.sha256); \
    actual_hash=$(sha256sum asset.tar.gz | awk '{print $1}'); \
    if [ "$expected_hash" != "$actual_hash" ]; then \
        echo "[ffi] sha256 mismatch: expected $expected_hash, got $actual_hash" >&2; \
        exit 1; \
    fi; \
    echo "[ffi] sha256 verified"; \
    tar -xzf asset.tar.gz; \
    test -f libcpex_ffi.a || { echo "[ffi] libcpex_ffi.a missing from tarball" >&2; exit 1; }; \
    echo "[ffi] extracted libcpex_ffi.a + VERSION=$(cat VERSION) + FFI_ABI=$(cat FFI_ABI)"

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
FROM debian:trixie-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        bash ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder --chmod=755 /authbridge-cpex /usr/local/bin/authbridge-cpex
COPY --chmod=755 cmd/authbridge-cpex/entrypoint.sh /usr/local/bin/entrypoint.sh

USER 1001

EXPOSE 8080 8081 9091 9093 9094

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
