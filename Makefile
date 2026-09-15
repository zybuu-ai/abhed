# Abhed Community Edition. Every target is what CI runs, so a green `make check`
# here is a green pull request there.
GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -s -w -X main.version=$(VERSION)

.PHONY: build test vet lint check clean docs image run help

build: docs       ## Build the abhed binary for this machine into ./bin, docs embedded
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/abhed ./cmd/abhed

docs:             ## Render docs/ into the tree the binary embeds (needs python3)
	python3 scripts/docsite/build.py --embed-only

test:             ## Run the test suite
	$(GO) test ./... -count=1

vet:              ## go vet
	$(GO) vet ./...

lint:             ## golangci-lint (installs nothing globally; runs the pinned version)
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 run ./...

check: vet test   ## What CI runs

image:            ## Build the container image
	podman build -t abhed:local . 2>/dev/null || docker build -t abhed:local .

run: build        ## Start a single-tenant server on :8080 with local accounts
	./bin/abhed serve -addr 127.0.0.1:8080

clean:
	rm -rf bin dist

help:             ## This list
	@grep -E '^[a-z]+:.*## ' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'
