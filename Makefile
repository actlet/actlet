.DEFAULT_GOAL := help

BIN_DIR := bin
BINARY  := $(BIN_DIR)/actlet
PKG     := ./cmd/actlet

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

GOLANGCI_LINT         ?= golangci-lint
GOLANGCI_LINT_VERSION ?= v2.15.0

ARGS ?= help

##@ General

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

.PHONY: setup
setup: ## Download modules and install development tools
	go mod download
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

##@ Development

.PHONY: build
build: ## Build the actlet binary into bin/
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

.PHONY: run
run: build ## Build and run actlet (pass arguments with ARGS="...")
	./$(BINARY) $(ARGS)

.PHONY: test
test: ## Run tests with the race detector
	go test -race ./...

.PHONY: check
check: ## Check formatting, vet, lint and go.mod tidiness
	@unformatted="$$(gofmt -l .)"; \
		if [ -n "$$unformatted" ]; then echo "gofmt needed for:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	$(GOLANGCI_LINT) run
	go mod tidy -diff

.PHONY: clean
clean: ## Remove build and test output
	rm -rf $(BIN_DIR) coverage.*
