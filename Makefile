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

# Multi-arch (manifest list) coordinates. MULTIARCH_PLATFORMS is the comma-list
# of platforms baked into a single OCI image index by the docker-buildx target.
# BUILDX_BUILDER is the deterministic name of a docker-container-driver buildx
# builder shared by both the holos-paas and holos-controller multi-arch targets
# (Makefile.controller reuses it): the default docker driver stores a single
# architecture and cannot emit a manifest list, so a docker-container builder is
# required. Defining the name once here, before the include below, keeps the two
# builder-bootstrap targets pointed at the same builder.
MULTIARCH_PLATFORMS ?= linux/amd64,linux/arm64
BUILDX_BUILDER      ?= holos-paas-multiarch

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

# The publish target wraps scripts/publish: render the platform with an injected
# app image digest, package the rendered manifests through Kustomize, and oras
# push the result as an OCI artifact (see holos/docs/oci-publish-workflow.md).
# APP_IMAGE is required (tag or digest); PUBLISH_REPO defaults to the in-cluster
# Quay manifests repo, mirroring the IMAGE_REPO default above.
PUBLISH_REPO ?= quay.holos.localhost/holos/holos-paas-manifests
.PHONY: publish
publish: ## Render, Kustomize-package, and oras push the manifests artifact (set APP_IMAGE=<ref>).
	@test -n "$(APP_IMAGE)" || { echo "ERROR: set APP_IMAGE=<registry>/<app>:<tag> or <registry>/<app>@sha256:<digest>"; exit 1; }
	scripts/publish "$(APP_IMAGE)" "$(PUBLISH_REPO)"

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

# The multi-arch targets build a single OCI image index (manifest list) spanning
# $(MULTIARCH_PLATFORMS) — both the amd64 and arm64 cross-compiles in one image,
# so it runs on amd64 and arm64 clusters alike. Because the Dockerfile pins the
# builder stage to $BUILDPLATFORM and cross-compiles via the Go toolchain, no
# QEMU emulation is needed — only a docker-container-driver buildx builder, which
# docker-buildx-builder bootstraps idempotently.
.PHONY: docker-buildx-builder
docker-buildx-builder: ## Ensure the shared docker-container buildx builder $(BUILDX_BUILDER) exists.
	docker buildx inspect $(BUILDX_BUILDER) >/dev/null 2>&1 || \
		docker buildx create --name $(BUILDX_BUILDER) --driver docker-container

# A multi-platform build cannot --load into the local Docker daemon (the daemon
# stores a single architecture), so docker-buildx is push-only: it emits the
# manifest list straight to $(IMAGE). Verify with:
#   docker buildx imagetools inspect $(IMAGE)
.PHONY: docker-buildx
docker-buildx: docker-buildx-builder ## Build and push the multi-arch $(MULTIARCH_PLATFORMS) image index $(IMAGE).
	docker buildx build --builder $(BUILDX_BUILDER) --platform $(MULTIARCH_PLATFORMS) -t $(IMAGE) --push .

# The holos-controller service (ADR-18, HOL-1309) lives in this same module and
# repo but keeps its targets isolated in Makefile.controller — all namespaced
# controller-* — so they never collide with the holos-paas targets above or
# touch scripts/apply, scripts/render, or scripts/publish.
include Makefile.controller
