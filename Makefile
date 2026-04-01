BINARY = mcp-gcp-observability

.PHONY: build lint fmt clean install

build:
	go build -o $(BINARY) .

lint:
	golangci-lint run

fmt:
	golangci-lint fmt

clean:
	rm -f $(BINARY)

install:
	go install .
