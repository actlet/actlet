.DEFAULT_GOAL := help

BIN_DIR := bin

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
BUILDINFO := github.com/actlet/actlet/internal/buildinfo
LDFLAGS := -s -w -X $(BUILDINFO).Version=$(VERSION) -X $(BUILDINFO).Commit=$(COMMIT) -X $(BUILDINFO).Date=$(DATE)

# Release binaries are static and CGO-free so they run on any Linux base
# image. Tests keep CGO, which the race detector needs.
DIST_DIR       := dist
DIST_PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
DIST_BINARIES  := actlet actlet-cli actlet-devgw
IMAGE          ?= actlet:dev
DEVGW_IMAGE    ?= actlet-devgw:dev

GOLANGCI_LINT         ?= golangci-lint
GOLANGCI_LINT_VERSION ?= v2.15.0
PROTOC_GEN_GO_VERSION      ?= v1.36.12
PROTOC_GEN_GO_GRPC_VERSION ?= v1.6.2
# Public contracts live under proto/ and are imported by their path below
# it; internal schemas are generated from the repository root.
PUBLIC_PROTOS   := $(shell find proto -name '*.proto')
INTERNAL_PROTOS := $(shell find internal -name '*.proto')

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
	go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)

##@ Development

.PHONY: build
build: ## Build static actlet and actlet-devgw binaries for this machine into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/ ./cmd/...

.PHONY: dist
dist: ## Cross-build static binaries of every command into dist/ (DIST_PLATFORMS)
	@mkdir -p $(DIST_DIR)
	@for p in $(DIST_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ "$$os" = windows ] && ext=.exe; \
		for b in $(DIST_BINARIES); do \
			out=$(DIST_DIR)/$${b}_$${os}_$${arch}$$ext; echo "$$out"; \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $$out ./cmd/$$b || exit 1; \
		done; \
	done

.PHONY: image
image: ## Build the actlet container image (IMAGE, default actlet:dev)
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) -t $(IMAGE) .

.PHONY: image-devgw
image-devgw: ## Build the development gateway image (DEVGW_IMAGE, default actlet-devgw:dev)
	docker build -f Dockerfile.devgw --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) -t $(DEVGW_IMAGE) .

.PHONY: run
run: build ## Build and run actlet (pass arguments with ARGS="...")
	./$(BIN_DIR)/actlet $(ARGS)

FUZZ_TARGETS := FuzzScan FuzzOpen
JOURNAL_PKG  := ./internal/journal
FUZZTIME     ?= 2m

.PHONY: test
test: ## Run tests with the race detector, plus a short fuzz run
	go test -race ./...
	@for t in $(FUZZ_TARGETS); do \
		go test -run '^$$' -fuzz "^$$t$$" -fuzztime 5s $(JOURNAL_PKG) || exit 1; \
	done

.PHONY: test-docker
test-docker: ## Kill a writer container and recover from its volume (needs Docker)
	@if ! docker info >/dev/null 2>&1; then echo "test-docker: Docker isn't available, skipping"; exit 0; fi; \
		$(MAKE) image && ACTLET_IMAGE=$(IMAGE) go test -tags dockertest -run '^TestDocker' -count=1 -v $(JOURNAL_PKG)

.PHONY: fuzz
fuzz: ## Fuzz the journal reader (FUZZTIME per target, default 2m)
	@for t in $(FUZZ_TARGETS); do \
		go test -run '^$$' -fuzz "^$$t$$" -fuzztime $(FUZZTIME) $(JOURNAL_PKG) || exit 1; \
	done

.PHONY: bench
bench: ## Run benchmarks (BENCH=regexp selects them)
	go test -run '^$$' -bench '$(or $(BENCH),.)' ./...

.PHONY: generate
generate: ## Regenerate protobuf and gRPC code (needs protoc)
	protoc -I proto --go_out=. --go_opt=module=github.com/actlet/actlet \
		--go-grpc_out=. --go-grpc_opt=module=github.com/actlet/actlet $(PUBLIC_PROTOS)
	protoc -I . -I proto --go_out=. --go_opt=module=github.com/actlet/actlet $(INTERNAL_PROTOS)

.PHONY: check
check: ## Check formatting, vet, lint and go.mod tidiness
	@unformatted="$$(gofmt -l .)"; \
		if [ -n "$$unformatted" ]; then echo "gofmt needed for:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	GOOS=windows go vet ./...  # keep the code portable to platforms without flock
	$(GOLANGCI_LINT) run
	go mod tidy -diff

.PHONY: clean
clean: ## Remove build and test output
	rm -rf $(BIN_DIR) $(DIST_DIR) coverage.*
