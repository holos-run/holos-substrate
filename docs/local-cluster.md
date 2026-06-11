# Local Cluster

<!-- Vendored from https://github.com/holos-run/holos/blob/main/doc/md/topics/local-cluster.mdx -->

Set up a local k3d cluster for development and testing. After completing this
guide you'll have a Kubernetes API server with proper DNS and TLS certificates.

This is the Layer 0 foundation for the Holos PaaS MVP — see
[docs/planning/holos-paas-mvp-milestones.md](planning/holos-paas-mvp-milestones.md)
for the full milestone plan.

> **Platform note:** `scripts/local-dns` is macOS-only. The MVP demo target is
> an Apple Silicon Mac (see [ADR-7](adr/ADR-7.md)). Linux users must configure
> dnsmasq or systemd-resolved themselves to resolve `*.holos.localhost` to
> `127.0.0.1`.

## Prerequisites

1. [OrbStack](https://docs.orbstack.dev/install) or [Docker](https://docs.docker.com/get-docker/) — container runtime (OrbStack recommended per ADR-7)
2. [k3d](https://k3d.io/#installation) — local Kubernetes via Docker
3. [kubectl](https://kubernetes.io/docs/tasks/tools/) — Kubernetes CLI
4. [mkcert](https://github.com/FiloSottile/mkcert) — trusted local TLS certificates
5. [jq](https://jqlang.github.io/jq/download/) — JSON processing (used by cluster scripts)

## One-Time DNS Setup

Configure your machine to resolve `*.holos.localhost` to your loopback
interface so requests reach the local cluster. Run this once before creating
the cluster:

```bash
scripts/local-dns
```

This installs dnsmasq via Homebrew, writes
`address=/holos.localhost/127.0.0.1` to the dnsmasq config, loads the
LaunchDaemon idempotently, and writes `/etc/resolver/holos.localhost`.
Requires `sudo` for system DNS configuration.

## Create the Cluster

Create a local k3d cluster with a container registry:

```bash
scripts/local-k3d
```

This creates:

- A local registry at `registry.holos.localhost:5100`
- A k3d cluster named `holos` with ports 80 and 443 forwarded to the load
  balancer and Traefik disabled

The static cluster shape — port mappings, k3s args — is defined in
[`k3d/config.yaml`](../k3d/config.yaml), which is the source of truth for
cluster structure. The cluster name, registry attachment, and registry port are
passed at runtime by `scripts/local-k3d` so that the `CLUSTER_NAME`,
`REGISTRY_NAME`, and `REGISTRY_PORT` environment variable overrides are fully
honored without editing `k3d/config.yaml`.

**Environment variable overrides:**

| Variable        | Default                     | Description              |
|-----------------|-----------------------------|--------------------------|
| `CLUSTER_NAME`  | `holos`                     | k3d cluster name         |
| `REGISTRY_NAME` | `registry.holos.localhost`  | Registry hostname        |
| `REGISTRY_PORT` | `5100`                      | Registry host port       |

## Setup Trusted TLS

Install the mkcert root CA into the cluster so cert-manager can issue trusted
certificates for `*.holos.localhost`:

```bash
sudo -v
scripts/local-ca
```

**Run this each time you recreate the cluster.**

This installs the mkcert root CA into the system trust store, creates the
`cert-manager` namespace, and applies the `local-ca` Secret
(`type: kubernetes.io/tls`) that cert-manager's ClusterIssuer references.
The generated `namespace.yaml` and `local-ca.yaml` are saved to
`$(mkcert -CAROOT)` (mode 0600 for the key material) so you can reapply them
after a cluster reset without re-running `mkcert --install`.

## Reset the Cluster

To reset to a clean state:

```bash
scripts/local-k3d    # Deletes and recreates the cluster (5-second abort window)
scripts/local-ca     # Re-installs the mkcert root CA
```

## Clean Up

Remove the cluster entirely:

```bash
k3d cluster delete holos
```
