# AuthBridge proxy-sidecar combined image — authbridge-proxy (HTTP
# forward + reverse proxies, no Envoy / no gRPC, full plugin set) +
# spiffe-helper in a single container.
#
# The agent uses HTTP_PROXY env var to route outbound traffic through
# the forward proxy; inbound traffic flows through the reverse proxy
# that takes over the agent's original port. Spiffe-helper starts
# conditionally based on the SPIRE_ENABLED env var.
#
# Uses alpine base (~5 MB) — needs bash for the supervisor entrypoint
# and glibc-compatible libc (musl is fine for both authbridge-proxy
# (CGO_ENABLED=0 static) and spiffe-helper (statically linked Go)).
# No shell would also work via a Go supervisor, but bash matches the
# envoy-sidecar Dockerfile pattern and keeps both images consistent.
#
# Build context: ./authbridge (needs access to authlib/ and cmd/authbridge-proxy/)

# Stage 1: Build authbridge-proxy binary.
# `-ldflags="-s -w"` drops the symbol table and DWARF debug info to
# shave ~30% off the binary. Go's runtime keeps `pclntab` separately,
# so panic stack traces still show function names.
FROM golang:1.24-alpine@sha256:8bee1901f1e530bfb4a7850aa7a479d17ae3a18beb6e09064ed54cfd245b7191 AS builder

RUN apk add --no-cache git binutils

WORKDIR /app

COPY authlib/ authlib/
COPY cmd/authbridge-proxy/ cmd/authbridge-proxy/

ENV GOWORK=off
RUN cd cmd/authbridge-proxy && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /authbridge-proxy .

# Stage 2: Pull spiffe-helper from upstream image
FROM ghcr.io/spiffe/spiffe-helper:0.11.0 AS spiffe-source

# Stage 3: Strip spiffe-helper. The upstream image ships an unstripped
# binary (~23 MB); strip removes ~7 MB of symbol/debug data.
FROM builder AS spiffe-stripper
COPY --from=spiffe-source /spiffe-helper /spiffe-helper
RUN strip /spiffe-helper

# Stage 4: Runtime — alpine (has bash, ~5 MB base)
FROM alpine:3.20@sha256:beefdbd8a1da6d2915566fde36db9db0b524eb737fc57cd1367effd16dc0d06d

RUN apk add --no-cache bash ca-certificates

COPY --from=spiffe-stripper --chmod=755 /spiffe-helper /usr/local/bin/spiffe-helper
COPY --from=builder --chmod=755 /authbridge-proxy /usr/local/bin/authbridge-proxy
COPY --chmod=755 cmd/authbridge-proxy/entrypoint.sh /usr/local/bin/entrypoint.sh

USER 1001

EXPOSE 8080 8081 9091 9093 9094

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
