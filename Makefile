# aft-ops — developer tasks.
#
# GoReleaser (.goreleaser.yaml) is the source of truth for release builds;
# the targets here are thin wrappers for day-to-day work and delegate to it
# for anything release-shaped. Local `build` relies on version.go's build-info
# fallback, so no ldflags are duplicated here.

BINARY := aft-ops

.DEFAULT_GOAL := help
.PHONY: help build install test vet check tidy fmt demo snapshot config-check clean

help: ## List available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

build: ## Build the binary (version resolved from Go build info)
	go build -o $(BINARY) ./cmd/aft-ops

install: ## Install the binary into GOBIN
	go install ./cmd/aft-ops

test: ## Run tests
	go test ./...

vet: ## Run go vet
	go vet ./...

check: vet test ## Run go vet and tests

tidy: ## Tidy go.mod / go.sum
	go mod tidy

fmt: ## Format Go sources
	gofmt -l -w .

demo: ## Re-record the README demo GIFs from docs/demo/*.tape (needs vhs)
	bash docs/demo/record.sh

snapshot: ## Local multi-platform release build via GoReleaser (no publish)
	goreleaser release --snapshot --clean --skip=publish

config-check: ## Validate .goreleaser.yaml
	goreleaser check

clean: ## Remove build artifacts
	rm -rf $(BINARY) dist/
