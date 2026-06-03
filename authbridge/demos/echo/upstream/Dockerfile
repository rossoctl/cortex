FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY upstream/main.go ./upstream/main.go
RUN CGO_ENABLED=0 go build -o /out/upstream ./upstream

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/upstream /upstream
EXPOSE 8080
ENTRYPOINT ["/upstream"]
