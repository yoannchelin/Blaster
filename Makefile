.PHONY: build test fmt vet tidy install bin

GOFLAGS ?=

build: bin

bin: bin/blast bin/blast-mcp

bin/blast: $(shell find cmd/blast internal -name '*.go')
	@mkdir -p bin
	go build $(GOFLAGS) -tags fts5 -o bin/blast ./cmd/blast

bin/blast-mcp: $(shell find cmd/blast-mcp internal -name '*.go')
	@mkdir -p bin
	go build $(GOFLAGS) -tags fts5 -o bin/blast-mcp ./cmd/blast-mcp

test:
	go test -tags fts5 ./...

fmt:
	gofmt -s -w .

vet:
	go vet -tags fts5 ./...

tidy:
	go mod tidy

install:
	go install -tags fts5 ./cmd/blast
	go install -tags fts5 ./cmd/blast-mcp
