FROM golang:1.23-alpine AS builder

ARG TARGETARCH=amd64

WORKDIR /app

COPY go.mod go.sum* ./

COPY go-processor/ ./go-processor/

RUN go mod tidy && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o /go-processor ./go-processor

FROM --platform=linux/${TARGETARCH} alpine:3.21

RUN apk --no-cache add ca-certificates

WORKDIR /root/

COPY --from=builder /go-processor .

EXPOSE 9090

CMD ["./go-processor"]
