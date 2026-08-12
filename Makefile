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

# What CI runs. Two parts of it are not optional extras: internal/invariants is
# the guard on this repo having no per-provider code, and cmd/sal's txtar
# scripts are where the command grammar is actually asserted.
check: vet test

# Run a single txtar script:  make script SCRIPT=grammar
script:
	$(GO) test ./cmd/sal/ -run 'TestScripts/$(SCRIPT)' -v

# Update txtar scripts in place from actual output, for when a deliberate
# change to output makes several scripts stale at once. Read the diff.
script-update:
	$(GO) test ./cmd/sal/ -run TestScripts -update-scripts

snapshot:
	goreleaser release --snapshot --clean

clean:
	rm -rf bin dist
