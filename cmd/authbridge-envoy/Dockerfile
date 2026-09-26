# AuthBridge envoy-sidecar combined image — Envoy + authbridge-envoy
# (ext_proc) in a single container.
#
# SPIFFE credentials are obtained in-process by authbridge-envoy via
# go-spiffe's WorkloadAPI client (no spiffe-helper sidecar binary).
#
# Build context: . (repository root; needs access to core/ and cmd/authbridge-envoy/)

# Stage 1: Build authbridge-envoy binary.
# `-ldflags="-s -w"` drops the symbol table and DWARF debug info to
# shave ~30% off the binary. Go's runtime keeps `pclntab` separately,
# so panic stack traces still show function names.
# core embeds github.com/rossoctl/context-guru (a published module, fetched via
# GOPROXY). It is pure-Go (skeleton cgo is behind the cg_skeleton tag), so
# CGO_ENABLED=0 stays correct.
FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder

RUN apk add --no-cache git

WORKDIR /app

COPY core/ core/
COPY storage/ storage/
COPY cmd/authbridge-envoy/ cmd/authbridge-envoy/

# Plugins are all opt-in; see the note in cmd/authbridge-proxy/Dockerfile. An
# untagged build registers none, which is never a valid artifact.
ARG GO_BUILD_TAGS=""
RUN test -n "${GO_BUILD_TAGS}" || { \
      echo "GO_BUILD_TAGS is required: plugins are opt-in and an untagged build registers none." >&2; \
      echo "Pass a profile, e.g. --build-arg GO_BUILD_TAGS=\"\$(go -C scripts/profile-tags run . envoy)\"" >&2; \
      exit 1; }
ENV GOWORK=off
RUN cd cmd/authbridge-envoy && CGO_ENABLED=0 GOOS=linux go build -tags "${GO_BUILD_TAGS}" -ldflags="-s -w" -o /authbridge-envoy .

# Stage 2: Get Envoy binary. Already stripped upstream; nothing to do.
FROM docker.io/envoyproxy/envoy:v1.37.1@sha256:29496a88fba9c4c9cdef4afe8fec70f536c5ba111b1c2bddbc5436b091ceca33 AS envoy-source

# Stage 3: Runtime — UBI9-micro (glibc-compatible, ~24 MB, has bash)
FROM registry.access.redhat.com/ubi9/ubi-micro@sha256:2173487b3b72b1a7b11edc908e9bbf1726f9df46a4f78fd6d19a2bab0a701f38

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=envoy-source --chmod=755 /usr/local/bin/envoy /usr/local/bin/envoy
COPY --from=builder --chmod=755 /authbridge-envoy /usr/local/bin/authbridge-envoy
COPY --chmod=755 cmd/authbridge-envoy/entrypoint.sh /usr/local/bin/entrypoint.sh

# Envoy writes hot-restart lock files and admin socket here
RUN mkdir -p /tmp/envoy && chmod 775 /tmp/envoy

USER 1337

EXPOSE 15123 15124 9901 9090

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
