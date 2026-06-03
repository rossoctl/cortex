# Multi-stage build for the finance-sparc demo MCP server (stdlib only).
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY finance-mcp/main.go ./finance-mcp/main.go
RUN CGO_ENABLED=0 go build -o /out/finance-mcp ./finance-mcp

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/finance-mcp /finance-mcp
EXPOSE 8888
ENTRYPOINT ["/finance-mcp"]
