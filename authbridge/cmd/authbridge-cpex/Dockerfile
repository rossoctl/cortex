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
# LIBC selects which CPEX FFI asset variant to link. We default to `musl`
# so the runtime stage can be Alpine (~8 MB) instead of debian:trixie-slim
# (~80-100 MB) — the base image, not the FFI binary, dominates this image's
# size. Override to `gnu` only for a glibc runtime (then also flip the
# builder/runtime bases below back to bookworm/debian).
ARG CPEX_LIBC=musl
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
# golang:alpine (musl) so we link the musl CPEX FFI asset and run on the
# small Alpine base. CGO needs a C compiler + musl headers (gcc, musl-dev).
#
# IMPORTANT: this pins the multi-arch *index* digest, not a single-arch
# sub-manifest. Unlike the pure-Go images (CGO_ENABLED=0, which cross-compile
# from any builder arch), this build links a native-arch libcpex_ffi.a, so the
# builder must run on the target arch. An amd64-only digest would make the
# arm64 build either fail to find an arm64 builder or link an incompatible .a.
FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder

RUN apk add --no-cache git ca-certificates gcc musl-dev

WORKDIR /app

COPY authlib/ authlib/
COPY cmd/authbridge-cpex/ cmd/authbridge-cpex/

COPY --from=ffi /ffi/libcpex_ffi.a /libs/libcpex_ffi.a

ENV GOWORK=off
ENV CGO_ENABLED=1
# musl folds libdl/libpthread into libc, so there is no separate -ldl to
# link against (as there is on glibc). Keep -lm for the FFI's math symbols.
#
# No stack-size linker flag is needed: musl's small default thread stack
# (128 KB) used to trip cedar-policy's stacker recursion guard ("recursion
# limit reached"), but CPEX >= v0.2.0-alpha.2 wraps its cedar dispatch in
# stacker::maybe_grow, so the FFI grows its own stack regardless of the host.
ENV CGO_LDFLAGS="-L/libs -lcpex_ffi -lm"
# Let Go auto-download the toolchain version go.mod requires. The pinned
# base image carries 1.25.1; cpex bindings need 1.25.4 (and transitively
# bump our cmd/authbridge-cpex/go.mod). GOTOOLCHAIN=auto means "download
# the version go.mod asks for" — keeps the base image pin stable across
# toolchain bumps.
ENV GOTOOLCHAIN=auto
# -trimpath drops absolute build paths for reproducibility; -s -w drop the
# Go symbol table + DWARF (panic traces still resolve via pclntab).
RUN cd cmd/authbridge-cpex && \
    go build -tags cpex -trimpath -ldflags="-s -w" -o /authbridge-cpex .

# ---------- Stage 3: runtime ----------------------------------------
# Alpine — same base + digest the authbridge-proxy / -lite images pin. The
# musl FFI asset links against musl, so no glibc floor and no debian base.
# entrypoint.sh is bash, so we install bash (matching the proxy image).
FROM alpine:3.20@sha256:beefdbd8a1da6d2915566fde36db9db0b524eb737fc57cd1367effd16dc0d06d

# libgcc provides libgcc_s.so.1 — the C++/Rust stack unwinder the FFI links
# against (the CPEX .a is built panic=unwind, so it references _Unwind_*).
# glibc bases ship it implicitly; on Alpine it must be installed explicitly.
RUN apk add --no-cache bash ca-certificates libgcc

COPY --from=builder --chmod=755 /authbridge-cpex /usr/local/bin/authbridge-cpex
COPY --chmod=755 cmd/authbridge-cpex/entrypoint.sh /usr/local/bin/entrypoint.sh

USER 1001

EXPOSE 8080 8081 9091 9093 9094

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
