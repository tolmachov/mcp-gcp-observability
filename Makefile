BINARY  = mcp-gcp-observability
PKG     = github.com/tolmachov/mcp-gcp-observability/internal
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X $(PKG).Version=$(VERSION)

.PHONY: build lint fmt clean install test test-race test-integration

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) .

test:
	go test ./...

test-race:
	go test -race ./...

# Requires a real GCP project and credentials (loads ../.env). See test/integration_test.go.
test-integration:
	go test -tags integration ./test/...

lint:
	golangci-lint run

fmt:
	golangci-lint fmt

clean:
	rm -f $(BINARY)

install:
	go install -ldflags="$(LDFLAGS)" .
