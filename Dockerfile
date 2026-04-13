FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -o mcp-gcp-observability .

FROM alpine:latest

WORKDIR /root/

COPY --from=builder /app/mcp-gcp-observability .

ENTRYPOINT ["./mcp-gcp-observability"]
