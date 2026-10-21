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
GO_IMAGE_VERSION ?= 1.27
DEVGW_IMAGE    ?= actlet-devgw:dev
TOOLS_IMAGE    ?= actlet-tools:dev

GOLANGCI_LINT         ?= golangci-lint
GOLANGCI_LINT_VERSION ?= v2.15.0
PROTOC_GEN_GO_VERSION      ?= v1.36.12
PROTOC_GEN_GO_GRPC_VERSION ?= v1.6.2
GOVULNCHECK_VERSION        ?= v1.9.0
GOVULNCHECK                ?= govulncheck
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
	go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

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

# Fuzz targets, as package:target.
FUZZ_TARGETS := ./internal/journal:FuzzScan ./internal/journal:FuzzOpen \
	./internal/attach:FuzzReadFile ./internal/attach:FuzzCommand ./sdk:FuzzWorkerMessages
JOURNAL_PKG  := ./internal/journal
FUZZTIME     ?= 2m

.PHONY: test
test: ## Run tests with the race detector, plus a short fuzz run
	go test -race ./...
	@for t in $(FUZZ_TARGETS); do \
		go test -run '^$$' -fuzz "^$${t##*:}$$" -fuzztime 5s "$${t%%:*}" || exit 1; \
	done

.PHONY: test-docker
test-docker: ## Kill a writer container and recover from its volume (needs Docker)
	@if ! docker info >/dev/null 2>&1; then echo "test-docker: Docker isn't available, skipping"; exit 0; fi; \
		$(MAKE) image && ACTLET_IMAGE=$(IMAGE) go test -tags dockertest -run '^TestDocker' -count=1 -v $(JOURNAL_PKG)

.PHONY: test-linux
test-linux: ## Run the tests of Linux-only behaviour, such as supervising processes as PID 1, in a container (needs Docker)
	@if ! docker info >/dev/null 2>&1; then echo "test-linux: Docker isn't available, skipping"; exit 0; fi; \
		docker run --rm -v "$(CURDIR)":/src -w /src -v actlet-gomod:/go/pkg/mod -v actlet-gocache:/root/.cache/go-build \
			golang:$(GO_IMAGE_VERSION) sh -c 'go test -race -count=1 ./internal/supervise/ ./internal/tools/ ./cmd/actlet/ && \
			CGO_ENABLED=0 go test -c -o /tmp/supervise.test ./internal/supervise && cp /tmp/supervise.test /src/bin/supervise.linux.test' && \
		docker run --rm -v "$(CURDIR)/bin":/t --entrypoint /t/supervise.linux.test golang:$(GO_IMAGE_VERSION) -test.run '^TestAsPID1$$' -test.v -test.count=1

.PHONY: test-k8s
test-k8s: ## Run the Kubernetes path test in a throwaway kind cluster (needs Docker and kind)
	@if ! command -v kind >/dev/null || ! docker info >/dev/null 2>&1; then echo "test-k8s: kind or Docker isn't available, skipping"; exit 0; fi; \
		$(MAKE) image image-devgw && \
		docker build -t $(TOOLS_IMAGE) --build-arg BASE=$(IMAGE) -f test/k8s/Dockerfile.tools test/k8s && \
		ACTLET_IMAGE=$(IMAGE) ACTLET_DEVGW_IMAGE=$(DEVGW_IMAGE) ACTLET_TOOLS_IMAGE=$(TOOLS_IMAGE) go test -tags k8stest -run TestKubernetesPath -count=1 -v -timeout 30m ./test/k8s

.PHONY: demo-tui
demo-tui: build ## Try the TUI: a scripted demo session with the viewer in the foreground
	@examples/demo/demo.sh tui

.PHONY: demo-attach
demo-attach: build ## Attach the TUI to the running demo, from a second terminal (or start one)
	@examples/demo/demo.sh attach

.PHONY: smoke-providers
smoke-providers: ## One tiny real call per route of PROVIDERS=<file>; needs the keys, costs a little
	@test -n "$(PROVIDERS)" || { echo "usage: make smoke-providers PROVIDERS=providers.yaml (see docs/providers.md)"; exit 2; }
	go test -tags smoke -run '^TestSmoke$$' -count=1 -v ./sdk/adapters/router -args -providers="$(abspath $(PROVIDERS))"

.PHONY: fuzz
fuzz: ## Fuzz the journal reader and the attach and gateway handlers (FUZZTIME per target, default 2m)
	@for t in $(FUZZ_TARGETS); do \
		go test -run '^$$' -fuzz "^$${t##*:}$$" -fuzztime $(FUZZTIME) "$${t%%:*}" || exit 1; \
	done

.PHONY: bench
bench: build ## Run the benchmarks (BENCH=regexp selects them), and measure the binaries as processes
	go test -run '^$$' -bench '$(or $(BENCH),.)' ./...
	go run test/bench/process.go -bin $(BIN_DIR)

.PHONY: bench-linux
bench-linux: ## Run make bench in a Linux container (needs Docker)
	@if ! docker info >/dev/null 2>&1; then echo "bench-linux: Docker isn't available, skipping"; exit 0; fi; \
		docker run --rm -v "$(CURDIR)":/src -w /src -v actlet-gomod:/go/pkg/mod -v actlet-gocache:/root/.cache/go-build \
			golang:$(GO_IMAGE_VERSION) sh -c 'git config --global --add safe.directory /src; make bench BIN_DIR=bin-linux'

.PHONY: generate
generate: ## Regenerate protobuf and gRPC code (needs protoc)
	protoc -I proto --go_out=. --go_opt=module=github.com/actlet/actlet \
		--go-grpc_out=. --go-grpc_opt=module=github.com/actlet/actlet $(PUBLIC_PROTOS)
	protoc -I . -I proto --go_out=. --go_opt=module=github.com/actlet/actlet $(INTERNAL_PROTOS)

.PHONY: screenshots
screenshots: ## Draw the README's screenshots of the viewer into docs/images
	go run test/screenshots/generate.go docs/images

.PHONY: check
check: ## Check formatting, vet, lint, go.mod tidiness and known vulnerabilities
	@unformatted="$$(gofmt -l .)"; \
		if [ -n "$$unformatted" ]; then echo "gofmt needed for:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	GOOS=windows go vet ./...  # keep the code portable to platforms without flock
	@# Lint as every platform CI tests on, whichever this machine is: each
	@# lints its own build-tagged files.
	for os in linux darwin windows; do GOOS=$$os $(GOLANGCI_LINT) run || exit 1; done
	go mod tidy -diff
	@$(MAKE) --no-print-directory vulncheck

.PHONY: vulncheck
vulncheck: ## Look for known vulnerabilities the code reaches (needs the network; VULNCHECK=0 skips it)
	@if [ "$(VULNCHECK)" = "0" ]; then echo "vulncheck: skipped (VULNCHECK=0)"; exit 0; fi; \
		if ! command -v $(GOVULNCHECK) >/dev/null; then echo "vulncheck: govulncheck isn't installed (make setup), skipping"; exit 0; fi; \
		$(GOVULNCHECK) ./...

.PHONY: clean
clean: ## Remove build and test output
	rm -rf $(BIN_DIR) $(DIST_DIR) coverage.*
