# Multi-stage build for the echo demo agent.
# The agent has zero non-stdlib dependencies, so the build context is
# just main.go + go.mod.

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY agent/main.go ./agent/main.go
RUN CGO_ENABLED=0 go build -o /out/agent ./agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agent /agent
# The agent listens on whatever PORT env says, defaulting to 8080.
# In the demo Pod we set PORT=8000 to leave 8080 for the authbridge
# sidecar's reverse proxy. EXPOSE is documentation-only; leaving it
# off avoids implying a port the operator hasn't picked.
ENTRYPOINT ["/agent"]
