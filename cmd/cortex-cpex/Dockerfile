# AuthBridge CPEX-enabled sidecar image — cortex (HTTP forward + reverse
# proxies) built with cgo and the `cpex` tag, so the `cpex` plugin can route
# hooks through the CPEX framework (APL DSL + named CPEX policy plugins). Links
# libcpex_ffi.a from a pinned CPEX release.
#
# Build context: . (repository root; needs core/, cmd/cortex/ and the pins in
# cmd/cortex-cpex/)
#
# Why not just the authbridge image: that one is pure-Go, CGO_ENABLED=0, and
# CPEX requires cgo. Operators switch images (not build flags) to enable CPEX.
# The binary is cmd/cortex, the same main as the authbridge image; it is
# installed as cortex-cpex so the entrypoint and container args are unchanged.

# ---------- Stage 1: fetch libcpex_ffi.a from the pinned release ----
# scripts/fetch-cpex-ffi.sh does the download and every check: the tarball's
# digest must match cmd/cortex-cpex/CPEX_FFI_SHA256, its cosign signature
# must come from CPEX's release workflow at the pinned tag, and its VERSION
# and FFI_ABI must equal CPEX_FFI_VERSION and CPEX_FFI_ABI. A mismatch fails
# the build rather than linking a lib whose ABI the Go bindings don't expect.
# The bindings also assert the ABI at init, so drift is caught at boot too.
FROM alpine:3.20@sha256:beefdbd8a1da6d2915566fde36db9db0b524eb737fc57cd1367effd16dc0d06d AS ffi

RUN apk add --no-cache bash curl ca-certificates tar cosign

ARG TARGETARCH
ARG TARGETOS=linux
# LIBC selects which CPEX FFI asset variant to link. We default to `musl`
# so the runtime stage can be Alpine (~8 MB) instead of debian:trixie-slim
# (~80-100 MB) — the base image, not the FFI binary, dominates this image's
# size. Override to `gnu` only for a glibc runtime (then also flip the
# builder/runtime bases below back to bookworm/debian).
ARG CPEX_LIBC=musl

WORKDIR /src
COPY scripts/fetch-cpex-ffi.sh scripts/
COPY cmd/cortex-cpex/CPEX_FFI_VERSION cmd/cortex-cpex/CPEX_FFI_ABI cmd/cortex-cpex/CPEX_FFI_SHA256 cmd/cortex-cpex/
RUN CPEX_FFI_TARGET="${TARGETOS}-${TARGETARCH}-${CPEX_LIBC}" CPEX_FFI_DEST=/ffi \
    bash scripts/fetch-cpex-ffi.sh

# ---------- Stage 2: build cmd/cortex with cgo + -tags cpex ----
# golang:alpine (musl) so we link the musl CPEX FFI asset and run on the
# small Alpine base. CGO needs a C compiler + musl headers (gcc, musl-dev).
#
# IMPORTANT: this pins the multi-arch *index* digest, not a single-arch
# sub-manifest. Unlike the pure-Go images (CGO_ENABLED=0, which cross-compile
# from any builder arch), this build links a native-arch libcpex_ffi.a, so the
# builder must run on the target arch. An amd64-only digest would make the
# arm64 build either fail to find an arm64 builder or link an incompatible .a.
FROM golang:1.26-alpine@sha256:9ce231b28998b32e116927c61aa5c406183061d4f3f2d61b292c88e7f6bdf7ca AS builder

RUN apk add --no-cache git ca-certificates gcc musl-dev

WORKDIR /app

COPY core/ core/
COPY cmd/cortex/ cmd/cortex/

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
# base image may lag the version cmd/cortex/go.mod asks for. GOTOOLCHAIN=auto means "download
# the version go.mod asks for" — keeps the base image pin stable across
# toolchain bumps.
ENV GOTOOLCHAIN=auto
# Plugins are all opt-in, so the plugin set has to be named explicitly: build
# with GO_BUILD_TAGS from the `cpex` profile (scripts/profile-tags).
# The `cpex` tag itself is separate and always required — it selects the
# FFI-backed cpex manager instead of a stub that refuses every config — so it
# is appended rather than left to the caller.
ARG GO_BUILD_TAGS=""
# Required, not optional: `cpex` alone registers no plugins, which is never a
# valid artifact. Fail here rather than shipping an image that crash-loops on the
# first config naming a plugin.
RUN test -n "${GO_BUILD_TAGS}" || { \
      echo "GO_BUILD_TAGS is required: plugins are opt-in and the cpex tag alone registers none." >&2; \
      echo "Pass a profile, e.g. --build-arg GO_BUILD_TAGS=\"\$(go -C scripts/profile-tags run . cpex)\"" >&2; \
      exit 1; }
# -trimpath drops absolute build paths for reproducibility; -s -w drop the
# Go symbol table + DWARF (panic traces still resolve via pclntab).
RUN cd cmd/cortex && \
    go build -tags "cpex${GO_BUILD_TAGS:+,${GO_BUILD_TAGS}}" -trimpath -ldflags="-s -w" -o /cortex-cpex .

# ---------- Stage 3: runtime ----------------------------------------
# Alpine — same base + digest the cortex / -lite images pin. The
# musl FFI asset links against musl, so no glibc floor and no debian base.
# entrypoint.sh is bash, so we install bash (matching the proxy image).
FROM alpine:3.20@sha256:beefdbd8a1da6d2915566fde36db9db0b524eb737fc57cd1367effd16dc0d06d

# libgcc provides libgcc_s.so.1 — the C++/Rust stack unwinder the FFI links
# against (the CPEX .a is built panic=unwind, so it references _Unwind_*).
# glibc bases ship it implicitly; on Alpine it must be installed explicitly.
RUN apk add --no-cache bash ca-certificates libgcc

COPY --from=builder --chmod=755 /cortex-cpex /usr/local/bin/cortex-cpex
COPY --chmod=755 cmd/cortex-cpex/entrypoint.sh /usr/local/bin/entrypoint.sh

USER 1001

EXPOSE 8080 8081 9091 9093 9094

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
