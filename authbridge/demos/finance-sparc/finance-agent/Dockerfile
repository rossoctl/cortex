# Multi-stage build for the finance-sparc demo agent (stdlib only).
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY finance-agent/main.go ./finance-agent/main.go
RUN CGO_ENABLED=0 go build -o /out/agent ./finance-agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agent /agent
# Listens on $PORT (default 8080). The demo Pod sets PORT=8000 to leave
# 8080 for the operator-injected authbridge sidecar's reverse proxy.
ENTRYPOINT ["/agent"]
