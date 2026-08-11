GO      ?= go
BIN     := bin/sal
PKG     := github.com/lpezet/secure-agent-lab-cli
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null)

LDFLAGS := -X $(PKG)/internal/version.version=$(VERSION) \
           -X $(PKG)/internal/version.commit=$(COMMIT)

.PHONY: all build test vet fmt check clean snapshot

all: check build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/sal

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

# What CI runs. The invariants package is part of it, not an optional extra:
# it is the guard on this repo having no per-provider code.
check: vet test

snapshot:
	goreleaser release --snapshot --clean

clean:
	rm -rf bin dist
