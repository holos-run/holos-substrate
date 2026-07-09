# holos-paas root Makefile: the shared go fmt/vet/test entry points, the OCI
# metadata helpers, and the publish/config-bundle targets. The two service
# binaries keep their targets isolated in Makefile.controller and
# Makefile.authenticator (see ADR-12).

# BUILDX_BUILDER is the deterministic name of a docker-container-driver buildx
# builder shared by the holos-controller and holos-authenticator multi-arch
# targets (Makefile.controller and Makefile.authenticator reuse it): the
# default docker driver stores a single architecture and cannot emit a manifest
# list, so a docker-container builder is required. Defining the name once here,
# before the includes below, keeps the builder-bootstrap targets pointed at the
# same builder.
BUILDX_BUILDER ?= holos-paas-multiarch

# VERSION is the build version stamped into both service binaries at link time
# via -ldflags (see the controller-build / authenticator-build targets) and
# into the container images via the VERSION build-arg. It is the output of
# `git describe`:
#   --tags   considers lightweight tags too, not only annotated ones, so a tag
#            made without -a still names the build;
#   --always falls back to a bare abbreviated SHA when no tag is reachable;
#   --dirty  appends -dirty when the working tree has uncommitted changes.
# The tagging convention is a leading v on MAJOR.MINOR.PATCH (e.g. v0.2.0); on a
# tagged commit `git describe` returns exactly that tag, and past it the
# vX.Y.Z-<n>-g<sha> form. Defined here, before the includes, so both service
# targets stamp the identical value. Override VERSION to stamp an explicit
# value (e.g. a Docker build with no .git context).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# OCI image metadata (org.opencontainers.image.*). Single-sourced HERE and
# threaded two ways: (1) into each Dockerfile's runtime-stage LABELs via the
# --build-arg flags produced by $(call oci-build-args,<PREFIX>), and (2) onto the
# MULTI-ARCH image INDEX via the --annotation flags produced by
# $(call oci-index-annotations,<PREFIX>). BOTH are required: a per-platform
# config LABEL is what Quay and a single-arch ghcr package render, but ghcr reads
# a MULTI-ARCH package's description / source / license from the image INDEX
# annotations, NOT the child image configs — without (2) the multi-arch package
# shows "No description provided" and is unlinked from its GitHub repo (the exact
# symptom this addresses). source/url/license/vendor are identical across the
# two images; title/description/documentation are per-image and defined beside
# each image's coordinates in Makefile.controller / Makefile.authenticator.
OCI_SOURCE   ?= https://github.com/holos-run/holos-paas
OCI_URL      ?= https://github.com/holos-run/holos-paas
OCI_LICENSES ?= Apache-2.0
OCI_VENDOR   ?= Open Infrastructure Services LLC

# $(call oci-build-args,<PREFIX>) → the --build-arg flags that thread
# <PREFIX>_IMAGE_TITLE/DESCRIPTION/DOCUMENTATION plus the shared OCI_* constants
# and VERSION into a Dockerfile's runtime-stage LABELs. The PREFIX (e.g.
# CONTROLLER) is passed — NOT the values — because $(call) splits its arguments
# on commas and a description may contain one; dereferencing $($(1)_IMAGE_*)
# sidesteps that.
# Values are single-quoted so embedded spaces/em-dashes/commas survive the shell.
oci-build-args = \
  --build-arg VERSION=$(VERSION) \
  --build-arg 'IMAGE_TITLE=$($(1)_IMAGE_TITLE)' \
  --build-arg 'IMAGE_DESCRIPTION=$($(1)_IMAGE_DESCRIPTION)' \
  --build-arg 'IMAGE_SOURCE=$(OCI_SOURCE)' \
  --build-arg 'IMAGE_URL=$(OCI_URL)' \
  --build-arg 'IMAGE_DOCUMENTATION=$($(1)_IMAGE_DOCUMENTATION)' \
  --build-arg 'IMAGE_LICENSES=$(OCI_LICENSES)' \
  --build-arg 'IMAGE_VENDOR=$(OCI_VENDOR)'

# $(call oci-index-annotations,<PREFIX>) → the matching --annotation flags that
# stamp the SAME metadata onto the multi-arch image INDEX. The "index:" selector
# targets the manifest list itself (an unprefixed --annotation would land on the
# child manifests, which is not what ghcr reads for the package page).
oci-index-annotations = \
  --annotation 'index:org.opencontainers.image.title=$($(1)_IMAGE_TITLE)' \
  --annotation 'index:org.opencontainers.image.description=$($(1)_IMAGE_DESCRIPTION)' \
  --annotation 'index:org.opencontainers.image.source=$(OCI_SOURCE)' \
  --annotation 'index:org.opencontainers.image.url=$(OCI_URL)' \
  --annotation 'index:org.opencontainers.image.documentation=$($(1)_IMAGE_DOCUMENTATION)' \
  --annotation 'index:org.opencontainers.image.licenses=$(OCI_LICENSES)' \
  --annotation 'index:org.opencontainers.image.vendor=$(OCI_VENDOR)' \
  --annotation 'index:org.opencontainers.image.version=$(VERSION)'

.PHONY: all
all: test

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

# version prints the build version that the service build targets stamp in —
# the same `git describe` output the binaries report at runtime.
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

# The multi-arch service image targets (controller-docker-buildx and
# authenticator-docker-buildx in the included makefiles) build a single OCI
# image index (manifest list) — both the amd64 and arm64 cross-compiles in one
# image, so it runs on amd64 and arm64 clusters alike. Because each Dockerfile
# pins the builder stage to $BUILDPLATFORM and cross-compiles via the Go
# toolchain, no QEMU emulation is needed — only a docker-container-driver
# buildx builder, which docker-buildx-builder bootstraps idempotently.
#
# The builder runs with --driver-opt network=host because the default image
# coordinates live in the local quay.holos.internal /
# k3d-registry.holos.internal registries, which a docker-container builder
# cannot resolve from its isolated network without host networking (see
# docs/build-registry.md). TLS trust for the mkcert-signed local registry comes
# from the host Docker daemon's trust store (OrbStack syncs the macOS keychain;
# Docker Desktop reads ~/.docker/certs.d) per docs/local-cluster.md — the
# builder needs no separate CA config.
#
# The guard recreates the builder unless it already exists as a docker-container
# builder running on the host network: a leftover builder of the same name on the
# wrong driver (e.g. the default docker driver, which cannot emit a manifest list)
# or without host networking (which cannot reach the local registry) would
# otherwise be reused silently. `docker buildx inspect` reports the driver on a
# `Driver:` line and the host-network mode as a `network: host` driver-opt; the
# guard requires both. Recreating is safe and idempotent.
#
# This is the single bootstrap for the shared $(BUILDX_BUILDER): the
# controller's and authenticator's multi-arch builder targets depend on THIS
# target rather than duplicating the guard, so the shared builder is created
# exactly once and a parallel
# `make -j controller-docker-buildx authenticator-docker-buildx` cannot race
# two recipes mutating the same builder.
.PHONY: docker-buildx-builder
docker-buildx-builder: ## Ensure the shared docker-container buildx builder $(BUILDX_BUILDER) exists.
	@info="$$(docker buildx inspect $(BUILDX_BUILDER) 2>/dev/null)"; \
	if echo "$$info" | grep -q '^Driver: *docker-container' && echo "$$info" | grep -q 'network.*host'; then \
		echo "buildx builder $(BUILDX_BUILDER) already present (docker-container, host network)"; \
	else \
		docker buildx rm $(BUILDX_BUILDER) >/dev/null 2>&1 || true; \
		docker buildx create --name $(BUILDX_BUILDER) --driver docker-container --driver-opt network=host; \
	fi

# The holos-controller service (ADR-18, HOL-1309) lives in this same module and
# repo but keeps its targets isolated in Makefile.controller — all namespaced
# controller-* — so they never collide with the shared targets above or touch
# scripts/apply, scripts/render, or scripts/publish.
include Makefile.controller

# The holos-authenticator service (ADR-23, HOL-1385) likewise lives in this
# module but keeps its targets isolated in Makefile.authenticator — all
# namespaced authenticator-* — so they never collide with the shared or
# controller-* targets above. It reuses the shared $(BUILDX_BUILDER) defined
# above for its multi-arch image build.
include Makefile.authenticator
