GO      ?= go
BIN     := bin/sal
PKG     := github.com/lpezet/secure-agent-lab-cli
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null)

LDFLAGS := -X $(PKG)/internal/version.version=$(VERSION) \
           -X $(PKG)/internal/version.commit=$(COMMIT)

.PHONY: all build test test-install test-install-containers test-compose vet fmt fmt-check check clean snapshot

all: check build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/sal

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

# The same check CI makes, rather than rewriting: `make check` failing here and
# CI failing there should be the same event. It was not, once — a stray blank
# line passed vet and tests and was caught only after a PR was open.
fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt'd:"; echo "$$unformatted"; exit 1; \
	fi

# install.sh is the one part of sal that is not Go, and the part a user meets
# first. Its refusals — a tampered archive, a release with no checksums, a
# signature that does not verify — are the whole justification for `curl | bash`
# being an acceptable install path, so they are tested rather than assumed.
test-install:
	bash tests/install/run.sh

# The same tier inside debian, ubuntu and an Alpine-based image, as root and as
# an ordinary user. Separate from `check` because it needs Docker and exits 2
# without it — but it is where the distro assumptions are actually tested:
# busybox has no long options, and GNU sha256sum has no -s.
test-install-containers:
	bash tests/install/containers.sh

# The `docker compose` behaviours sal depends on, checked against the real
# binary rather than against documentation. Separate from `check` for the same
# reason as the container tier: it needs Docker and exits 2 without it. It does
# NOT build the lab's images — this is about compose's semantics, not the stack.
test-compose:
	bash tests/compose/run.sh

# What CI runs, and it must stay that. Three parts of it are not optional
# extras: internal/invariants is the guard on this repo having no per-provider
# code, cmd/sal's txtar scripts are where the command grammar is actually
# asserted, and tests/install is where the install script's refusals are.
check: vet fmt-check test test-install

# Run a single txtar script:  make script SCRIPT=grammar
script:
	$(GO) test ./cmd/sal/ -run 'TestScripts/$(SCRIPT)' -v

# Update txtar scripts in place from actual output, for when a deliberate
# change to output makes several scripts stale at once. Read the diff.
script-update:
	$(GO) test ./cmd/sal/ -run TestScripts -update-scripts

# Everything a release would build, without publishing: four platforms, the
# archives and the checksum file. Signing is skipped because keyless signing
# needs an OIDC token that only the release workflow has — so this proves the
# BUILD half of the pipeline locally, and CI is the only place the signing half
# can be exercised.
snapshot:
	goreleaser release --snapshot --clean --skip=sign

clean:
	rm -rf bin dist
