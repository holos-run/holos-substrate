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

The `holos-paas` binary ships as a single multi-service image: the
`ENTRYPOINT` is the binary, and the running service is selected by the
subcommand argument (e.g. `webhook-receiver`). The image is built from a
two-stage `Dockerfile` — a `golang` builder that cross-compiles a static,
distroless-ready binary, and a `gcr.io/distroless/static:nonroot` runtime.

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
cross-built `linux/arm64` image is published directly.

Verify the image locally without the cluster:

```bash
docker run --rm quay.holos.localhost/holos/holos-paas:dev webhook-receiver --help
```

With a reachable NATS (e.g. `docker run --rm -p 4222:4222 nats -js`), the
service answers `200` on `/healthz` and, once connected, `200` on `/readyz`.

## Documentation

- [AGENTS.md](AGENTS.md) — project conventions and the documentation
  index.
- [docs/adr/](docs/adr/README.md) — Architecture Decision Records: the
  binding design decisions.
- [docs/local-cluster.md](docs/local-cluster.md) — the local quick start:
  create the cluster and apply the platform.
- [holos/README.md](holos/README.md) — the Holos CUE deployment
  configuration: layout, apply-order rationale, caveats, and the platform
  service contracts (including the
  [webhook receiver](holos/README.md#webhook-receiver-and-service-contract)).

## License

Proprietary — see [LICENSE](LICENSE).
