VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOARCH  ?= $(shell go env GOARCH)
BINARY  := dist/incus-ha-agent-linux-$(GOARCH)

.PHONY: help build test lint vuln e2e-build nix clean

help:
	@echo "make build    static binary in dist/ (GOARCH=amd64|arm64, VERSION=...)"
	@echo "make test     unit and integration tests with the race detector"
	@echo "make lint     all pre-commit hooks, including golangci-lint"
	@echo "make vuln     govulncheck"
	@echo "make nix      nix flake check"

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath \
		-ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) ./cmd/incus-ha-agent

test:
	go test -race -count=1 ./...

lint:
	pre-commit run --all-files

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

nix:
	nix flake check --print-build-logs --no-write-lock-file

clean:
	rm -rf dist e2e.test
