# holos-paas Makefile. Target shapes mirror the sibling holos-controller and
# holos-console Makefiles (see ADR-12).

# Container image coordinates. Override IMAGE_REPO/IMAGE_TAG to publish
# elsewhere; the default targets the local k3d in-cluster registry
# (quay.holos.internal/holos, see docs/local-cluster.md) so a pushed image
# is pullable by the cluster. PLATFORM defaults to linux/arm64 because the local
# k3d cluster runs on Apple Silicon; override for other architectures.
IMAGE_REPO ?= quay.holos.internal/holos/holos-paas
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

# VERSION is the build version stamped into both binaries at link time via
# -ldflags (see the build / controller-build targets) and into the container
# images via the VERSION build-arg. It is the output of `git describe`:
#   --tags   considers lightweight tags too, not only annotated ones, so a tag
#            made without -a still names the build;
#   --always falls back to a bare abbreviated SHA when no tag is reachable;
#   --dirty  appends -dirty when the working tree has uncommitted changes.
# The tagging convention is a leading v on MAJOR.MINOR.PATCH (e.g. v0.2.0); on a
# tagged commit `git describe` returns exactly that tag, and past it the
# vX.Y.Z-<n>-g<sha> form. Defined here, before the include, so both the
# holos-paas and holos-controller targets stamp the identical value. Override
# VERSION to stamp an explicit value (e.g. a Docker build with no .git context).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

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

# PAAS_LDFLAGS stamps the build VERSION into the holos-paas binary by overriding
# internal/cli.version (the var the CLI reports via app.Version). The Dockerfile
# applies the same -X through its VERSION build-arg.
PAAS_LDFLAGS ?= -X github.com/holos-run/holos-paas/internal/cli.version=$(VERSION)

.PHONY: build
build: fmt vet ## Build the holos-paas binary.
	go build -ldflags "$(PAAS_LDFLAGS)" -o bin/holos-paas ./cmd/holos-paas

# version prints the build version that `make build` stamps in — the same
# `git describe` output the binaries report at runtime.
.PHONY: version
version: ## Print the build version (git describe).
	@echo $(VERSION)

# version-bump-minor creates an annotated tag that bumps the minor component of
# the most recent vMAJOR.MINOR.PATCH tag and resets the patch to 0 (v0.2.x ->
# v0.3.0), following the leading-v convention. The most recent tag is selected by
# version sort (not commit date), so it is independent of checkout history; with
# no existing version tag it starts at v0.1.0. It creates the tag locally only —
# review with `git show <tag>` and publish with `git push origin <tag>`.
.PHONY: version-bump-minor
version-bump-minor: ## Tag an annotated minor-version bump (vX.Y.0).
	@set -e; \
	current="$$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -1)"; \
	if [ -z "$$current" ]; then \
		next="v0.1.0"; \
	else \
		ver="$${current#v}"; \
		major="$$(printf '%s' "$$ver" | cut -d. -f1)"; \
		minor="$$(printf '%s' "$$ver" | cut -d. -f2)"; \
		next="v$$major.$$((minor + 1)).0"; \
	fi; \
	echo "Creating annotated tag $$next (bumped from $${current:-<none>})"; \
	git tag -a "$$next" -m "$$next"

# The publish target wraps scripts/publish: render the platform with an injected
# app image digest, package the rendered manifests through Kustomize, and oras
# push the result as an OCI artifact (see holos/docs/oci-publish-workflow.md).
# APP_IMAGE is required (tag or digest); PUBLISH_REPO defaults to the in-cluster
# Quay manifests repo, mirroring the IMAGE_REPO default above.
PUBLISH_REPO ?= quay.holos.internal/holos/holos-paas-manifests
.PHONY: publish
publish: ## Render, Kustomize-package, and oras push the manifests artifact (set APP_IMAGE=<ref>).
	@test -n "$(APP_IMAGE)" || { echo "ERROR: set APP_IMAGE=<registry>/<app>:<tag> or <registry>/<app>@sha256:<digest>"; exit 1; }
	scripts/publish "$(APP_IMAGE)" "$(PUBLISH_REPO)"

# The config-build / config-push targets wrap scripts/publish-config: bundle the
# committed holos/deploy/ tree AS-IS into a single OCI artifact (no render, no
# digest injection, no Kustomize) and publish it under a mutable :dev tag as the
# platform-config bundle the App-of-Apps bootstrap consumes (HOL-1373/HOL-1374).
# The build/push split mirrors docker-build/docker-push: config-build produces a
# local tarball with NO network I/O; config-push oras-pushes it. This bundle is
# distinct from the publish target above (per-app, input-addressed manifests for
# Kargo) — see holos/docs/oci-publish-workflow.md. CONFIG_REPO/CONFIG_TAG default
# to the in-cluster Quay config repo, mirroring the IMAGE_REPO default.
CONFIG_REPO ?= quay.holos.internal/holos/holos-paas-config
CONFIG_TAG  ?= dev
.PHONY: config-build
config-build: ## Bundle holos/deploy/ into a local OCI artifact tarball (no network).
	scripts/publish-config --build "$(CONFIG_REPO):$(CONFIG_TAG)"

.PHONY: config-push
config-push: config-build ## Build then oras push the holos/deploy/ bundle to $(CONFIG_REPO):$(CONFIG_TAG).
	scripts/publish-config --push "$(CONFIG_REPO):$(CONFIG_TAG)"

# The image targets use buildx so the builder stage runs on the native host
# (BUILDPLATFORM) and the Go toolchain cross-compiles to $(PLATFORM) — no target
# architecture emulation is required. docker-build loads the result into the
# local Docker daemon; docker-push publishes it to the registry. Both build a
# single-$(PLATFORM) image; for a multi-arch manifest list spanning
# $(MULTIARCH_PLATFORMS) use the docker-buildx target below (and
# controller-docker-buildx in Makefile.controller for the controller image).
# --build-arg VERSION=$(VERSION) carries the git-describe version into the
# Dockerfile (whose build context excludes .git), where it is stamped into the
# binary via -ldflags exactly as `make build` does on the host.
.PHONY: docker-build
docker-build: ## Build the container image for $(PLATFORM) tagged $(IMAGE).
	docker buildx build --platform $(PLATFORM) --build-arg VERSION=$(VERSION) -t $(IMAGE) --load .

.PHONY: docker-push
docker-push: ## Build for $(PLATFORM) and push $(IMAGE) to the registry.
	docker buildx build --platform $(PLATFORM) --build-arg VERSION=$(VERSION) -t $(IMAGE) --push .

# The multi-arch targets build a single OCI image index (manifest list) spanning
# $(MULTIARCH_PLATFORMS) — both the amd64 and arm64 cross-compiles in one image,
# so it runs on amd64 and arm64 clusters alike. Because the Dockerfile pins the
# builder stage to $BUILDPLATFORM and cross-compiles via the Go toolchain, no
# QEMU emulation is needed — only a docker-container-driver buildx builder, which
# docker-buildx-builder bootstraps idempotently.
#
# The builder runs with --driver-opt network=host because the default IMAGE lives
# in the local quay.holos.internal / k3d-registry.holos.internal registries,
# which a docker-container builder cannot resolve from its isolated network
# without host networking (see docs/build-registry.md). TLS trust for the
# mkcert-signed local registry comes from the host Docker daemon's trust store
# (OrbStack syncs the macOS keychain; Docker Desktop reads ~/.docker/certs.d) per
# docs/local-cluster.md — the builder needs no separate CA config.
#
# The guard recreates the builder unless it already exists as a docker-container
# builder running on the host network: a leftover builder of the same name on the
# wrong driver (e.g. the default docker driver, which cannot emit a manifest list)
# or without host networking (which cannot reach the local registry) would
# otherwise be reused silently. `docker buildx inspect` reports the driver on a
# `Driver:` line and the host-network mode as a `network: host` driver-opt; the
# guard requires both. Recreating is safe and idempotent.
#
# This is the single bootstrap for the shared $(BUILDX_BUILDER): the controller's
# multi-arch builder target (Makefile.controller) depends on THIS target rather
# than duplicating the guard, so the shared builder is created exactly once and a
# parallel `make -j docker-buildx controller-docker-buildx` cannot race two
# recipes mutating the same builder.
.PHONY: docker-buildx-builder
docker-buildx-builder: ## Ensure the shared docker-container buildx builder $(BUILDX_BUILDER) exists.
	@info="$$(docker buildx inspect $(BUILDX_BUILDER) 2>/dev/null)"; \
	if echo "$$info" | grep -q '^Driver: *docker-container' && echo "$$info" | grep -q 'network.*host'; then \
		echo "buildx builder $(BUILDX_BUILDER) already present (docker-container, host network)"; \
	else \
		docker buildx rm $(BUILDX_BUILDER) >/dev/null 2>&1 || true; \
		docker buildx create --name $(BUILDX_BUILDER) --driver docker-container --driver-opt network=host; \
	fi

# A multi-platform build cannot --load into the local Docker daemon (the daemon
# stores a single architecture), so docker-buildx is push-only: it emits the
# manifest list straight to $(IMAGE). Verify with:
#   docker buildx imagetools inspect $(IMAGE)
.PHONY: docker-buildx
docker-buildx: docker-buildx-builder ## Build and push the multi-arch $(MULTIARCH_PLATFORMS) image index $(IMAGE).
	docker buildx build --builder $(BUILDX_BUILDER) --platform $(MULTIARCH_PLATFORMS) --build-arg VERSION=$(VERSION) -t $(IMAGE) --push .

# The holos-controller service (ADR-18, HOL-1309) lives in this same module and
# repo but keeps its targets isolated in Makefile.controller — all namespaced
# controller-* — so they never collide with the holos-paas targets above or
# touch scripts/apply, scripts/render, or scripts/publish.
include Makefile.controller

# The holos-authenticator service (ADR-23, HOL-1385) likewise lives in this
# module but keeps its targets isolated in Makefile.authenticator — all
# namespaced authenticator-* — so they never collide with the holos-paas or
# controller-* targets above. It reuses the shared $(BUILDX_BUILDER) defined
# above for its multi-arch image build.
include Makefile.authenticator
