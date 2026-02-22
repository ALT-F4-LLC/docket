.PHONY: build test lint vet install clean demo

VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS := -X github.com/ALT-F4-LLC/docket/internal/cli.version=$(VERSION) -X github.com/ALT-F4-LLC/docket/internal/cli.commit=$(COMMIT) -X github.com/ALT-F4-LLC/docket/internal/cli.buildDate=$(BUILD_DATE)

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o ./bin/docket ./cmd/docket

test:
	go test ./...

lint: vet
	@command -v staticcheck >/dev/null 2>&1 && staticcheck ./... || echo "staticcheck not found, skipping"

vet:
	go vet ./...

install:
	CGO_ENABLED=0 go install -ldflags "$(LDFLAGS)" ./cmd/docket

clean:
	rm -rf ./bin/

demo: build
	@command -v vhs >/dev/null 2>&1 || { echo "vhs is required: brew install vhs"; exit 1; }
	vhs scripts/demo.tape
