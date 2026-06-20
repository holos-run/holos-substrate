# Holos PaaS

A Kubernetes-native platform delivering a minimum viable Heroku
experience — push a tagged image, get a deploy — managed entirely through
the Kubernetes API and rendered with the [Holos](https://holos.run/)
rendered-manifests pattern.

## Quick Start

Follow [docs/local-cluster.md](docs/local-cluster.md) — the canonical
guide from zero to a running platform — then verify the smoke test
answers at `https://echo.holos.localhost/` and Keycloak serves the
`holos` realm at `https://auth.holos.localhost/`. In summary:

```bash
scripts/local-dns    # one-time DNS setup (macOS, requires sudo)
scripts/local-k3d    # create the local k3d cluster
scripts/local-ca     # install the mkcert root CA (requires sudo)
scripts/apply        # apply the platform components in order
```

## Container image

The `holos-paas` binary ships as a single multi-service image (ADR-12): the
`ENTRYPOINT` is the binary, and the running service is selected by the
subcommand argument. The image is built from a
two-stage `Dockerfile` — a `golang` builder that cross-compiles a static,
distroless-ready binary, and a `gcr.io/distroless/static:nonroot` runtime.

The webhook-receiver and webhook-subscriber subcommands that previously
realized this binary were retired in HOL-1241 when deployment moved to Kargo
plus the client-side ORAS publish workflow (ADR-16); the root command
currently registers no service subcommands, and later phases add them back.

The build cross-compiles via `TARGETOS`/`TARGETARCH` with `CGO_ENABLED=0`,
so the same `Dockerfile` produces any platform. **The local k3d cluster runs
on Apple Silicon**, so the Make targets default to `PLATFORM=linux/arm64`;
override `PLATFORM` for other architectures.

```bash
make docker-build                    # build quay.holos.localhost/holos/holos-paas:dev (linux/arm64)
make docker-push                     # build and push to the local k3d registry
make docker-build IMAGE_TAG=v0.1.0   # override the tag
make docker-build PLATFORM=linux/amd64
```

`IMAGE_REPO` defaults to `quay.holos.localhost/holos/holos-paas`, the
in-cluster registry created by `scripts/local-k3d` (see
[docs/local-cluster.md](docs/local-cluster.md)). Images pushed there are
pullable by the k3d cluster, so `make docker-push` makes the image available
to the deploy phase. `docker-push` uses `docker buildx build --push` so the
cross-built single-`PLATFORM` image is published directly.

Verify the image locally without the cluster:

```bash
docker run --rm quay.holos.localhost/holos/holos-paas:dev --help
```

### Multi-arch images

`docker-build`/`docker-push` produce a single-`PLATFORM` image. To publish a
multi-arch image — one OCI image index (manifest list) spanning both
`linux/amd64` and `linux/arm64`, so it runs on amd64 and arm64 clusters alike
(GKE, EKS) — use the `docker-buildx` targets:

```bash
make docker-buildx                 # build+push the multi-arch holos-paas index
make controller-docker-buildx      # build+push the multi-arch holos-controller index
```

These are **push-only**: a multi-platform build cannot `--load` into the local
Docker daemon (which stores a single architecture), so each emits its manifest
list straight to the registry. Both targets share one
`docker-container`-driver buildx builder (`docker-buildx-builder` bootstraps it
idempotently; the controller target depends on the same builder), required
because the default `docker` driver cannot emit a manifest list. No QEMU is
needed — the `Dockerfile` pins the builder stage to `$BUILDPLATFORM` and the Go
toolchain cross-compiles to each target arch. Override
`MULTIARCH_PLATFORMS`/`CONTROLLER_MULTIARCH_PLATFORMS` to change the platform
set, and `IMAGE_REPO`/`IMAGE_TAG` (or `CONTROLLER_IMAGE_REPO`/
`CONTROLLER_IMAGE_TAG`) to publish elsewhere. Verify both platforms landed:

```bash
docker buildx imagetools inspect quay.holos.localhost/holos/holos-paas:dev
```

### Publishing images from CI

The [`.github/workflows/images.yaml`](.github/workflows/images.yaml) **Images**
workflow builds and publishes the multi-arch images from GitHub Actions. Each
image is a **discrete job** (sharing the reusable
[`build-image.yaml`](.github/workflows/build-image.yaml) workflow), so the
`image` input lets you publish `holos-paas` only, `holos-controller` only, or
both — building one never forces the other. It is **manual-only**
(`workflow_dispatch` — never on push, pull request, or tag) and each build job
runs inside a `publish-images` GitHub Environment. Because the workflow builds
the caller-supplied `ref` (it checks out `inputs.ref`, not the workflow run
ref), **configure that Environment with required reviewers** — required
reviewers are the control that gates an arbitrary `ref`, so every dispatch is
approved by a human before the publish job runs. Environment branch/tag
deployment policies constrain only the workflow run ref, not the `ref` checkout
input, so they are not a sufficient boundary here. It drives the same `make
docker-buildx` / `make controller-docker-buildx` targets, so the build logic is
single-sourced between local hosts and CI. Trigger it from the Actions tab or
with `gh`:

```bash
gh workflow run images.yaml -f ref=main                                  # both images
gh workflow run images.yaml -f image=holos-controller -f ref=main        # one image only
gh workflow run images.yaml -f ref=v0.1.0 -f tag=v0.1.0
```

Inputs:

- `image` (required, default `both`) — which image(s) to build: `both`,
  `holos-paas`, or `holos-controller`. Each runs as its own job; select one to
  skip building the other.
- `ref` (required, default `main`) — the Git reference to build: a commit SHA,
  a branch (`main` or `refs/heads/main`), or a tag (`v0.1.0` or
  `refs/tags/v0.1.0`). Passed to `actions/checkout`.
- `tag` (optional) — the image tag to publish; defaults to the short SHA of the
  resolved ref when left blank.
- `registry` (optional, default `ghcr.io`) — an allowlisted registry host;
  authenticates with the built-in `GITHUB_TOKEN`.

It publishes the selected image(s) to `ghcr.io/<owner>/holos-paas` and/or
`ghcr.io/<owner>/holos-controller` (owner derived from the repository), each a
multi-arch index covering `linux/amd64` and `linux/arm64`.

## Documentation

- [AGENTS.md](AGENTS.md) — project conventions and the documentation
  index.
- [docs/adr/](docs/adr/README.md) — Architecture Decision Records: the
  binding design decisions.
- [docs/local-cluster.md](docs/local-cluster.md) — the local quick start:
  create the cluster and apply the platform.
- [holos/README.md](holos/README.md) — the Holos CUE deployment
  configuration: layout, apply-order rationale, caveats, and the platform
  service contracts.

## License

Proprietary — see [LICENSE](LICENSE).
