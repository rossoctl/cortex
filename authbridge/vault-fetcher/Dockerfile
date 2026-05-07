# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /build

# Copy go module files from authlib
COPY authbridge/authlib/go.mod authbridge/authlib/go.sum authbridge/authlib/
WORKDIR /build/authbridge/authlib
RUN go mod download

# Copy authlib source
COPY authbridge/authlib/ /build/authbridge/authlib/

# Copy vault-fetcher source
WORKDIR /build/authbridge/vault-fetcher
COPY authbridge/vault-fetcher/ ./

# Initialize go module for vault-fetcher
RUN go mod init github.com/kagenti/kagenti-extensions/authbridge/vault-fetcher || true
RUN go mod edit -replace github.com/kagenti/kagenti-extensions/authbridge/authlib=../authlib
RUN go get gopkg.in/yaml.v3
RUN go mod tidy

# Build binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags="-s -w" -o vault-fetcher .

# Runtime stage - distroless for minimal attack surface
FROM gcr.io/distroless/static-debian12:nonroot

# Copy binary from builder
COPY --from=builder /build/authbridge/vault-fetcher/vault-fetcher /usr/local/bin/vault-fetcher

# Run as non-root user (automatically set by distroless/nonroot)
# UID/GID: 65532:65532

ENTRYPOINT ["/usr/local/bin/vault-fetcher"]
CMD ["--config=/etc/vault-fetcher/config.yaml"]
