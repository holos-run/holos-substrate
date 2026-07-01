# Build the holos-paas binary. Two stages: a golang builder that cross-compiles
# a static binary, and a distroless non-root runtime. This mirrors the sibling
# holos-controller Dockerfile pattern; holos-paas has no UI and no cgo, so there
# is no Node/UI stage (cf. holos-console's three-stage build).
# Pin the builder to the native build platform (BUILDPLATFORM) and let the Go
# toolchain cross-compile to the requested TARGETOS/TARGETARCH. This avoids
# emulating the target architecture for the builder's RUN steps, so the same
# Dockerfile builds linux/arm64 (the Apple Silicon k3d cluster) from any host.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
# VERSION is the build version stamped into the binary (mirrors `make build`).
# The build context excludes .git, so `git describe` cannot run here — the
# Makefile docker targets pass it through as --build-arg VERSION=$(VERSION); it
# defaults to "dev" for a bare `docker build` with no build-arg.
ARG VERSION=dev

WORKDIR /workspace
# Copy the Go module manifests and download dependencies first so this layer is
# cached unless go.mod/go.sum change.
COPY go.mod go.mod
COPY go.sum go.sum
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy the source. cmd/holos-paas is the multi-service binary and internal/
# holds all implementation (ADR-12) — the Fisk command tree lives in
# internal/cli (ADR-17), which cmd/holos-paas/main.go imports, so internal/
# must be copied for the build to resolve it.
COPY cmd/ cmd/
COPY internal/ internal/

# Build a static, trimmed binary. CGO is disabled for a small, portable image;
# TARGETOS/TARGETARCH (set by buildx, defaulting to linux) drive
# cross-compilation so the same Dockerfile builds linux/arm64 on Apple Silicon
# and linux/amd64 elsewhere.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags "-X github.com/holos-run/holos-paas/internal/cli.version=${VERSION}" \
    -o /holos-paas ./cmd/holos-paas

# Use distroless as the minimal base image to package the binary.
# https://github.com/GoogleContainerTools/distroless
FROM gcr.io/distroless/static:nonroot
# OCI image metadata (org.opencontainers.image.*) is single-sourced in the root
# Makefile and threaded in as --build-args (see $(call oci-build-args,PAAS));
# VERSION carries the git-describe version the same way. These LABELs stamp the
# per-platform image config — what Quay and a single-arch ghcr package render. A
# MULTI-ARCH ghcr package instead reads these from the image INDEX, which the
# Makefile's docker-buildx target stamps with buildx --annotation. Values default
# empty; the sanctioned build path is always the Makefile (as with VERSION=dev).
ARG VERSION=dev
ARG IMAGE_TITLE=
ARG IMAGE_DESCRIPTION=
ARG IMAGE_SOURCE=
ARG IMAGE_URL=
ARG IMAGE_DOCUMENTATION=
ARG IMAGE_LICENSES=
ARG IMAGE_VENDOR=
WORKDIR /
COPY --from=build /holos-paas /holos-paas
USER 65532:65532

LABEL org.opencontainers.image.title="${IMAGE_TITLE}" \
      org.opencontainers.image.description="${IMAGE_DESCRIPTION}" \
      org.opencontainers.image.source="${IMAGE_SOURCE}" \
      org.opencontainers.image.url="${IMAGE_URL}" \
      org.opencontainers.image.documentation="${IMAGE_DOCUMENTATION}" \
      org.opencontainers.image.licenses="${IMAGE_LICENSES}" \
      org.opencontainers.image.vendor="${IMAGE_VENDOR}" \
      org.opencontainers.image.version="${VERSION}"

# The running service is selected by subcommand args (ADR-12). The root
# command registers no service subcommands yet — the NATS pipeline services
# were retired in HOL-1241; later phases add their subcommands back.
ENTRYPOINT ["/holos-paas"]
