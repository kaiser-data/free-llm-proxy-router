.PHONY: all build test lint clean install-tools help

GOBIN ?= $(shell go env GOPATH)/bin
GOCMD = $(shell which go || echo /home/marty/go-sdk/go/bin/go)
LDFLAGS = -ldflags="-s -w"
BINDIR = bin

all: build

build: ## Build all three binaries
	@mkdir -p $(BINDIR)
	$(GOCMD) build $(LDFLAGS) -o $(BINDIR)/picoclaw-proxy ./cmd/proxy
	$(GOCMD) build $(LDFLAGS) -o $(BINDIR)/picoclaw-scan ./cmd/scan
	$(GOCMD) build $(LDFLAGS) -o $(BINDIR)/picoclaw-bench ./cmd/bench
	@echo "Built: $(BINDIR)/picoclaw-proxy, $(BINDIR)/picoclaw-scan, $(BINDIR)/picoclaw-bench"

build-proxy: ## Build only the proxy binary
	@mkdir -p $(BINDIR)
	$(GOCMD) build $(LDFLAGS) -o $(BINDIR)/picoclaw-proxy ./cmd/proxy

build-scan: ## Build only the scan binary
	@mkdir -p $(BINDIR)
	$(GOCMD) build $(LDFLAGS) -o $(BINDIR)/picoclaw-scan ./cmd/scan

build-bench: ## Build only the bench binary
	@mkdir -p $(BINDIR)
	$(GOCMD) build $(LDFLAGS) -o $(BINDIR)/picoclaw-bench ./cmd/bench

test: ## Run unit tests
	$(GOCMD) test ./... -count=1

test-verbose: ## Run unit tests with verbose output
	$(GOCMD) test -v ./... -count=1

test-integration: ## Run integration tests (requires API keys)
	$(GOCMD) test -tags=integration ./... -count=1

vet: ## Run go vet
	$(GOCMD) vet ./...

lint: ## Run golangci-lint (install with: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)
	@which golangci-lint > /dev/null 2>&1 || (echo "golangci-lint not found; run: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest" && exit 1)
	golangci-lint run ./...

install-tools: ## Install development tools
	$(GOCMD) install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

clean: ## Remove build artifacts
	rm -rf $(BINDIR)
	$(GOCMD) clean -cache

tidy: ## Tidy go.mod and go.sum
	$(GOCMD) mod tidy

size: build ## Show binary sizes
	@ls -lh $(BINDIR)/picoclaw-*

help: ## Display this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'
