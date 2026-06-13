# holos-paas Makefile. Target shapes mirror the sibling holos-controller and
# holos-console Makefiles (see ADR-12).

# Container image coordinates. Override IMAGE_REPO/IMAGE_TAG to publish
# elsewhere; the default targets the local k3d in-cluster registry
# (quay.holos.localhost/holos, see docs/local-cluster.md) so a pushed image
# is pullable by the cluster. PLATFORM defaults to linux/arm64 because the local
# k3d cluster runs on Apple Silicon; override for other architectures.
IMAGE_REPO ?= quay.holos.localhost/holos/holos-paas
IMAGE_TAG  ?= dev
IMAGE      ?= $(IMAGE_REPO):$(IMAGE_TAG)
PLATFORM   ?= linux/arm64

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

# The image targets use buildx so the builder stage runs on the native host
# (BUILDPLATFORM) and the Go toolchain cross-compiles to $(PLATFORM) — no target
# architecture emulation is required. docker-build loads the result into the
# local Docker daemon; docker-push publishes it to the registry.
.PHONY: docker-build
docker-build: ## Build the container image for $(PLATFORM) tagged $(IMAGE).
	docker buildx build --platform $(PLATFORM) -t $(IMAGE) --load .

.PHONY: docker-push
docker-push: ## Build for $(PLATFORM) and push $(IMAGE) to the registry.
	docker buildx build --platform $(PLATFORM) -t $(IMAGE) --push .
