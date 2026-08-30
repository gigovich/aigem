BIN     := bin/aigem
PKG     := ./cmd/aigem
UI      := internal/web/ui
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

# Every target the release builds, so a portability break shows up locally.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: help build install run test race lint lint-windows vuln fmt fmt-check vet tidy tidy-check check check-all cross docs evals clean web web-dev web-check

help: ## Show this help
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "};{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

build: ## Build the binary into bin/
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) $(PKG)

install: ## Install aigem into GOBIN
	go install -trimpath -ldflags '$(LDFLAGS)' $(PKG)

run: build ## Build and run
	$(BIN)

# The bundle is embedded from internal/web/dist but never committed, so that
# `go install github.com/gigovich/aigem/cmd/aigem@latest` keeps working on a
# machine with no node. A binary built without this says it has no UI rather
# than serving a blank page.
web: ## Build the browser UI into internal/web/dist
	cd $(UI) && npm ci && npm run build

web-dev: ## Vite dev server, proxying /api to a running `aigem web`
	cd $(UI) && npm run dev

web-check: ## Lint, typecheck and test the browser UI
	cd $(UI) && npm run lint && npm run check && npm test

test: ## Run the test suite
	go test ./...

race: ## Run the test suite under the race detector
	go test -race -timeout 15m ./...

lint: ## Run golangci-lint
	golangci-lint run

lint-windows: ## Lint the Windows-only sources (invisible to a linux lint run)
	GOOS=windows golangci-lint run

vuln: ## Scan for known vulnerabilities
	govulncheck ./...

fmt: ## Format the source
	gofmt -w .

fmt-check: ## Fail if anything is unformatted
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "needs gofmt:"; echo "$$unformatted"; exit 1; fi

vet: ## Run go vet
	go vet ./...

tidy: ## Tidy go.mod/go.sum
	go mod tidy

tidy-check: ## Fail if go.mod/go.sum are not tidy
	go mod tidy -diff

cross: ## Build every released target
	@set -e; for t in $(PLATFORMS); do \
		echo "==> $$t"; \
		GOOS=$${t%/*} GOARCH=$${t#*/} go build -o /dev/null $(PKG); \
	done

check: fmt-check vet lint race cross ## The usual pre-PR set

# npm is optional on purpose: a Go contributor without a node toolchain still
# gets a full check run, and CI runs the UI checks in a job of their own.
check-all: check lint-windows vuln tidy-check ## Everything CI runs
	@command -v npm >/dev/null 2>&1 || { echo "skipping web checks: npm not found"; exit 0; }; \
	$(MAKE) web-check

docs: ## Serve the documentation site locally
	mkdocs serve

# Real model calls, so it is deliberately outside `check`. EVAL_ARGS passes
# through, e.g. `make evals EVAL_ARGS='-model openai/gpt-5.6-sol -n 5'`.
evals: build ## Score subagent delegation against a live model (see evals/README.md)
	go run ./evals/runner $(EVAL_ARGS)

clean: ## Remove build output
	rm -rf bin dist site
	find internal/web/dist -mindepth 1 ! -name .gitkeep -exec rm -rf {} +
