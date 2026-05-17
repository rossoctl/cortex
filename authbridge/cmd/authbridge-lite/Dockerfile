# AuthBridge proxy-sidecar lite combined image — authbridge-lite
# (jwt-validation + token-exchange only, no parsers) + spiffe-helper
# in a single container.
#
# Same listener layout as the full authbridge-proxy image, but with
# the a2a / mcp / inference parser plugins dropped to optimize binary
# size for deployments that don't need protocol-aware session events
# in abctl. Functionally identical for inbound JWT validation +
# outbound token exchange.
#
# Spiffe-helper starts conditionally based on the SPIRE_ENABLED env var.
#
# Build context: ./authbridge (needs access to authlib/ and cmd/authbridge-lite/)

# Stage 1: Build authbridge-lite binary.
# `-ldflags="-s -w"` drops the symbol table and DWARF debug info to
# shave ~30% off the binary. Go's runtime keeps `pclntab` separately,
# so panic stack traces still show function names.
FROM golang:1.24-alpine@sha256:8bee1901f1e530bfb4a7850aa7a479d17ae3a18beb6e09064ed54cfd245b7191 AS builder

RUN apk add --no-cache git binutils

WORKDIR /app

COPY authlib/ authlib/
COPY cmd/authbridge-lite/ cmd/authbridge-lite/

ENV GOWORK=off
RUN cd cmd/authbridge-lite && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /authbridge-lite .

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
COPY --from=builder --chmod=755 /authbridge-lite /usr/local/bin/authbridge-lite
COPY --chmod=755 cmd/authbridge-lite/entrypoint.sh /usr/local/bin/entrypoint.sh

USER 1001

EXPOSE 8080 8081 9091 9093 9094

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
