FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
# Static, stripped binary; stamp the version into internal.Version.
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w -X github.com/tolmachov/mcp-gcp-observability/internal.Version=${VERSION}" \
    -o mcp-gcp-observability .

FROM alpine:3.21

# TLS roots for talking to the GCP APIs, plus a non-root user to run as.
RUN apk add --no-cache ca-certificates \
    && adduser -D -u 10001 app

WORKDIR /home/app

COPY --from=builder /app/mcp-gcp-observability /usr/local/bin/mcp-gcp-observability

USER app

ENTRYPOINT ["mcp-gcp-observability"]
