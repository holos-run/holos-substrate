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
    go build -trimpath -o /holos-paas ./cmd/holos-paas

# Use distroless as the minimal base image to package the binary.
# https://github.com/GoogleContainerTools/distroless
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /holos-paas /holos-paas
USER 65532:65532

# The running service is selected by subcommand args (ADR-12). The root
# command registers no service subcommands yet — the NATS pipeline services
# were retired in HOL-1241; later phases add their subcommands back.
ENTRYPOINT ["/holos-paas"]
