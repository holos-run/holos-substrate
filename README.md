# Holos PaaS

A Kubernetes-native platform delivering a minimum viable Heroku
experience — push a tagged image, get a deploy — managed entirely through
the Kubernetes API and rendered with the [Holos](https://holos.run/)
rendered-manifests pattern.

## Quick Start

Follow [docs/local-cluster.md](docs/local-cluster.md) — the canonical
guide from zero to a running Layer 0 platform — then verify the smoke
test answers at `https://echo.holos.localhost/`. In summary:

```bash
scripts/local-dns    # one-time DNS setup (macOS)
scripts/local-k3d    # create the local k3d cluster
scripts/local-ca     # install the mkcert root CA into the cluster
scripts/apply        # apply the Layer 0 components in order
```

## Documentation

- [AGENTS.md](AGENTS.md) — project conventions and the documentation
  index.
- [docs/adr/](docs/adr/README.md) — Architecture Decision Records: the
  binding design decisions.
- [docs/local-cluster.md](docs/local-cluster.md) — the local quick start:
  create the cluster and apply the platform.
- [holos/README.md](holos/README.md) — the Holos CUE deployment
  configuration: layout, apply-order rationale, and caveats.

## License

Proprietary — see [LICENSE](LICENSE).
