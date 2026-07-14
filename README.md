# Holos Substrate

The substrate building blocks for the [holos](https://holos.run/) open
source platform, implemented as Kubernetes custom resources:

- **Declarative Quay and Keycloak management** — the `quay.holos.run`
  `Organization` and `Repository` CRDs and the `keycloak.holos.run`
  `Instance`, `Group`, `GroupMembership`,
  `User`, and `Client` CRDs, reconciled by
  `holos-controller` ([ADR-18](docs/adr/ADR-18.md),
  [ADR-19](docs/adr/ADR-19.md), [ADR-20](docs/adr/ADR-20.md)) so
  registry organizations and identity primitives are managed through
  the Kubernetes API.
- **Cross-namespace authorization** — the `security.holos.run`
  `ReferenceGrant` convention ([ADR-22](docs/adr/ADR-22.md)): every
  cross-namespace reference between `holos.run` custom resources is
  authorized by an explicit grant in the referent namespace, never
  silently honored.
- **The Holos Authenticator** — `holos-authenticator`
  ([ADR-23](docs/adr/ADR-23.md)), an Istio gRPC `ext_authz` controller
  that authenticates users to any conformant Kubernetes cluster relying
  only on an OIDC identity provider, of which Keycloak is the reference
  implementation.

Everything is managed through the Kubernetes API and rendered with the
[Holos](https://holos.run/) rendered-manifests pattern. Each custom
resource is documented in-cluster — `kubectl explain
organizations.quay.holos.run --recursive`, `kubectl explain
clients.keycloak.holos.run`, and friends — and the
[runbooks](docs/runbooks/) are the operational entry points for wiring
credentials, renaming external resources, and verifying each service.

## Quick Start

Follow [docs/local-cluster.md](docs/local-cluster.md) — the canonical
guide from zero to the substrate's local development and verification
cluster — then verify the smoke test answers at
`https://echo.holos.internal/` and Keycloak serves the `holos` realm at
`https://auth.holos.internal/`. In summary:

```bash
scripts/local-dns    # one-time DNS setup (macOS, requires sudo)
scripts/local-k3d    # create the local k3d cluster
scripts/local-ca     # install the mkcert root CA (requires sudo)
scripts/apply        # apply the platform components in order
```

## Container images

Two service images ship from this repository: `holos-controller` (ADR-18)
and `holos-authenticator` (ADR-23). Each is built from its own two-stage
Dockerfile — `Dockerfile.controller` and `Dockerfile.authenticator` — with a
`golang` builder that cross-compiles a static, distroless-ready binary and a
`gcr.io/distroless/static:nonroot` runtime.

Each build cross-compiles via `TARGETOS`/`TARGETARCH` with `CGO_ENABLED=0`,
so the same Dockerfile produces any platform. **The local k3d cluster runs
on Apple Silicon**, so the Make targets default to
`CONTROLLER_PLATFORM=linux/arm64` / `AUTHENTICATOR_PLATFORM=linux/arm64`;
override them for other architectures.

```bash
make controller-docker-build                        # build quay.holos.internal/holos/holos-controller:dev (linux/arm64)
make controller-docker-push                         # build and push to the local k3d registry
make controller-docker-build CONTROLLER_IMAGE_TAG=v0.1.0
make controller-docker-build CONTROLLER_PLATFORM=linux/amd64
make authenticator-docker-build                     # the authenticator image, same shapes
```

`CONTROLLER_IMAGE_REPO` / `AUTHENTICATOR_IMAGE_REPO` default to
`quay.holos.internal/holos/holos-{controller,authenticator}`, the in-cluster
registry created by `scripts/local-k3d` (see
[docs/local-cluster.md](docs/local-cluster.md)). Images pushed there are
pullable by the k3d cluster, so the `*-docker-push` targets make the image
available to the deploy phase. They use `docker buildx build --push` so the
cross-built single-platform image is published directly.

### Build version

Both service binaries are stamped with a build version at link time, derived from
`git describe --tags --always --dirty`: the most recent tag, plus the
commits-since and abbreviated SHA when `HEAD` is past it, plus `-dirty` for an
uncommitted working tree. `--tags` honors lightweight tags too (not only
annotated ones), and `--always` falls back to a bare SHA when no tag is
reachable. The tagging convention is a leading `v` on `MAJOR.MINOR.PATCH`
(e.g. `v0.2.0`); on a tagged commit `git describe` returns exactly that tag.

The version flows into `holos-controller` and `holos-authenticator`, which log
it once at manager startup:

```json
{"level":"info","ts":"...","logger":"setup","msg":"starting manager","version":"v0.2.0"}
```

`make controller-build` / `make authenticator-build` stamp it via `-ldflags`;
the `*-docker-*` targets pass it into the Dockerfile builds as
`--build-arg VERSION=$(VERSION)` (the build context excludes `.git`). Useful
targets:

```bash
make version              # print the version that builds will stamp
make version-bump-minor   # create an annotated vX.(Y+1).0 tag (local only)
```

`version-bump-minor` selects the highest existing `vX.Y.Z` tag by version sort,
bumps the minor and resets the patch to `0`, and creates an **annotated** tag
(starting at `v0.1.0` when none exist). It tags locally only — review with
`git show <tag>` and publish with `git push origin <tag>`. Override `VERSION`
to stamp an explicit value (e.g. in CI).

### Multi-arch images

The `*-docker-build`/`*-docker-push` targets produce a single-platform image.
To publish a multi-arch image — one OCI image index (manifest list) spanning
both `linux/amd64` and `linux/arm64`, so it runs on amd64 and arm64 clusters
alike (GKE, EKS) — use the `*-docker-buildx` targets:

```bash
make controller-docker-buildx      # build+push the multi-arch holos-controller index
make authenticator-docker-buildx   # build+push the multi-arch holos-authenticator index
```

These are **push-only**: a multi-platform build cannot `--load` into the local
Docker daemon (which stores a single architecture), so each emits its manifest
list straight to the registry. Both targets share one
`docker-container`-driver buildx builder (`docker-buildx-builder` bootstraps it
idempotently; the controller and authenticator targets depend on the same
builder), required because the default `docker` driver cannot emit a manifest
list. No QEMU is needed — each Dockerfile pins the builder stage to
`$BUILDPLATFORM` and the Go toolchain cross-compiles to each target arch.
Override
`CONTROLLER_MULTIARCH_PLATFORMS`/`AUTHENTICATOR_MULTIARCH_PLATFORMS`
to change the platform set, and
`CONTROLLER_IMAGE_REPO`/`CONTROLLER_IMAGE_TAG` (or
`AUTHENTICATOR_IMAGE_REPO`/`AUTHENTICATOR_IMAGE_TAG`) to publish elsewhere. The
`holos-authenticator` image ([ADR-23](docs/adr/ADR-23.md), the Istio
`ext_authz` authorizer) builds from its own `Dockerfile.authenticator`. Verify
both platforms landed:

```bash
docker buildx imagetools inspect quay.holos.internal/holos/holos-controller:dev
```

### Publishing images from CI

The [`.github/workflows/images.yaml`](.github/workflows/images.yaml) **Images**
workflow builds and publishes the multi-arch images from GitHub Actions. Each
image is a **discrete job** (sharing the reusable
[`build-image.yaml`](.github/workflows/build-image.yaml) workflow), so the
`image` input lets you publish `holos-controller` only,
`holos-authenticator` only, or all — building one never forces another. It is
**manual-only**
(`workflow_dispatch` — never on push, pull request, or tag) and each build job
runs inside a `publish-images` GitHub Environment. Because the workflow builds
the caller-supplied `ref` (it checks out `inputs.ref`, not the workflow run
ref), **configure that Environment with required reviewers** — required
reviewers are the control that gates an arbitrary `ref`, so every dispatch is
approved by a human before the publish job runs. Environment branch/tag
deployment policies constrain only the workflow run ref, not the `ref` checkout
input, so they are not a sufficient boundary here. It drives the same `make
controller-docker-buildx` / `make authenticator-docker-buildx` targets, so the
build logic is single-sourced between local hosts and CI. Trigger it from the
Actions tab or with `gh`:

```bash
gh workflow run images.yaml -f ref=main                                  # all images
gh workflow run images.yaml -f image=holos-controller -f ref=main        # one image only
gh workflow run images.yaml -f image=holos-authenticator -f ref=main     # the authenticator only
gh workflow run images.yaml -f ref=v0.1.0 -f tag=v0.1.0
```

Inputs:

- `image` (required, default `all`) — which image(s) to build: `all`,
  `holos-controller`, or `holos-authenticator` (`all` builds both). Each runs
  as its own job; select one to skip building the other.
- `ref` (required, default `main`) — the Git reference to build: a commit SHA,
  a branch (`main` or `refs/heads/main`), or a tag (`v0.1.0` or
  `refs/tags/v0.1.0`). Passed to `actions/checkout`.
- `tag` (optional) — the image tag to publish; defaults to the short SHA of the
  resolved ref when left blank.
- `registry` (optional, default `ghcr.io`) — an allowlisted registry host;
  authenticates with the built-in `GITHUB_TOKEN`.

It publishes the selected image(s) to `ghcr.io/<owner>/holos-controller`
and/or `ghcr.io/<owner>/holos-authenticator` (owner derived from the
repository), each a multi-arch index covering `linux/amd64` and `linux/arm64`.

## Documentation

- [AGENTS.md](AGENTS.md) — project conventions and the documentation
  index.
- [docs/adr/](docs/adr/README.md) — Architecture Decision Records: the
  binding design decisions.
- [docs/local-cluster.md](docs/local-cluster.md) — the local quick start:
  create the cluster and apply the platform.
- [docs/runbooks/](docs/runbooks/) — operational runbooks: the
  [holos-controller](docs/runbooks/holos-controller.md) and
  [holos-authenticator](docs/runbooks/holos-authenticator.md) service
  runbooks, credential wiring, OIDC integrations, and the
  [external-resource rename](docs/runbooks/external-resource-rename.md)
  procedure.
- [holos/README.md](holos/README.md) — the Holos CUE deployment
  configuration: layout, apply-order rationale, caveats, and the platform
  service contracts.
