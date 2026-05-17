# AuthBridge envoy-sidecar combined image — Envoy + authbridge-envoy
# (ext_proc) + spiffe-helper in a single container.
#
# Spiffe-helper starts conditionally based on the SPIRE_ENABLED env var
# (set by the operator when the workload opts into SPIRE identity).
#
# Build context: ./authbridge (needs access to authlib/ and cmd/authbridge-envoy/)

# Stage 1: Build authbridge-envoy binary.
# `-ldflags="-s -w"` drops the symbol table and DWARF debug info to
# shave ~30% off the binary. Go's runtime keeps `pclntab` separately,
# so panic stack traces still show function names.
FROM golang:1.24-alpine@sha256:8bee1901f1e530bfb4a7850aa7a479d17ae3a18beb6e09064ed54cfd245b7191 AS builder

RUN apk add --no-cache git binutils

WORKDIR /app

COPY authlib/ authlib/
COPY cmd/authbridge-envoy/ cmd/authbridge-envoy/

ENV GOWORK=off
RUN cd cmd/authbridge-envoy && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /authbridge-envoy .

# Stage 2: Get Envoy binary. Already stripped upstream; nothing to do.
FROM docker.io/envoyproxy/envoy:v1.37.1 AS envoy-source

# Stage 3: Pull spiffe-helper from upstream image
FROM ghcr.io/spiffe/spiffe-helper:0.11.0 AS spiffe-source

# Stage 4: Strip spiffe-helper. The upstream image ships an unstripped
# binary (~23 MB); strip removes ~7 MB of symbol/debug data.
FROM builder AS spiffe-stripper
COPY --from=spiffe-source /spiffe-helper /spiffe-helper
RUN strip /spiffe-helper

# Stage 5: Runtime — UBI9-micro (glibc-compatible, ~24 MB, has bash)
FROM registry.access.redhat.com/ubi9/ubi-micro@sha256:2173487b3b72b1a7b11edc908e9bbf1726f9df46a4f78fd6d19a2bab0a701f38

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=envoy-source --chmod=755 /usr/local/bin/envoy /usr/local/bin/envoy
COPY --from=spiffe-stripper --chmod=755 /spiffe-helper /usr/local/bin/spiffe-helper
COPY --from=builder --chmod=755 /authbridge-envoy /usr/local/bin/authbridge-envoy
COPY --chmod=755 cmd/authbridge-envoy/entrypoint.sh /usr/local/bin/entrypoint.sh

# Envoy writes hot-restart lock files and admin socket here
RUN mkdir -p /tmp/envoy && chmod 775 /tmp/envoy

USER 1337

EXPOSE 15123 15124 9901 9090

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
