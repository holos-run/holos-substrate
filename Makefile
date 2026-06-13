# holos-paas Makefile. Target shapes mirror the sibling holos-controller and
# holos-console Makefiles (see ADR-12).

.PHONY: all
all: build

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint.
	golangci-lint run

.PHONY: test
test: fmt vet ## Run tests with the race detector and coverage.
	go test -race -coverprofile cover.out ./...

.PHONY: build
build: fmt vet ## Build the holos-paas binary.
	go build -o bin/holos-paas ./cmd/holos-paas

.PHONY: run
run: ## Run the webhook receiver locally.
	go run ./cmd/holos-paas webhook-receiver
