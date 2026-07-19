# AuthBridge proxy-sidecar combined image — authbridge-proxy (HTTP
# forward + reverse proxies, no Envoy / no gRPC, full plugin set) in
# a single container.
#
# The agent uses HTTP_PROXY env var to route outbound traffic through
# the forward proxy; inbound traffic flows through the reverse proxy
# that takes over the agent's original port. SPIFFE credentials are
# obtained in-process via go-spiffe's WorkloadAPI client (no
# spiffe-helper sidecar binary).
#
# Uses alpine base (~5 MB) — needs bash for the supervisor entrypoint.
# authbridge-proxy is CGO_ENABLED=0 static, so musl is fine.
#
# Build context: ./authbridge (needs access to authlib/ and cmd/authbridge-proxy/)
#
# authlib embeds github.com/rossoctl/context-guru (a published module, fetched via
# GOPROXY like any other dependency). It builds pure-Go — skeleton's cgo is behind
# the cg_skeleton build tag — so CGO_ENABLED=0 below stays correct.

# Stage 1: Build authbridge-proxy binary.
# `-ldflags="-s -w"` drops the symbol table and DWARF debug info to
# shave ~30% off the binary. Go's runtime keeps `pclntab` separately,
# so panic stack traces still show function names.
FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder

RUN apk add --no-cache git

WORKDIR /app

COPY authlib/ authlib/
COPY cmd/authbridge-proxy/ cmd/authbridge-proxy/

ARG GO_BUILD_TAGS=""
ENV GOWORK=off
RUN cd cmd/authbridge-proxy && CGO_ENABLED=0 GOOS=linux go build -tags "${GO_BUILD_TAGS}" -ldflags="-s -w" -o /authbridge-proxy .

# Stage 2: Runtime — alpine (has bash, ~5 MB base)
FROM alpine:3.20@sha256:beefdbd8a1da6d2915566fde36db9db0b524eb737fc57cd1367effd16dc0d06d

RUN apk add --no-cache bash ca-certificates

COPY --from=builder --chmod=755 /authbridge-proxy /usr/local/bin/authbridge-proxy
COPY --chmod=755 cmd/authbridge-proxy/entrypoint.sh /usr/local/bin/entrypoint.sh

USER 1001

EXPOSE 8080 8081 9091 9093 9094

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
