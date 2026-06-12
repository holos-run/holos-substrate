# Local Cluster

<!-- Originally vendored from https://github.com/holos-run/holos/blob/main/doc/md/topics/local-cluster.mdx; has since diverged — do not overwrite with a re-vendor. -->

Set up a local k3d cluster for development and testing, then apply the
platform to it. This is the canonical quick-start guide: after completing
it you'll have a running platform — the Layer 0 foundation (a Kubernetes
API server with proper DNS and TLS certificates, serving the platform's
components on ports 80 and 443) plus the Layer 1 services:
CloudNativePG-managed Postgres, Keycloak with the `holos` realm at
`https://auth.holos.localhost`, and the Quay registry at
`https://quay.holos.localhost`.

This is the foundation for the Holos PaaS MVP — see
[Holos PaaS MVP Milestones](planning/holos-paas-mvp-milestones.md)
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
5. [jq](https://jqlang.org/) — JSON processing (used by cluster scripts)
6. [Homebrew](https://brew.sh/) — macOS only, required by `scripts/local-dns`

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

With Traefik disabled, nothing answers on ports 80/443 until the platform's
Layer 0 components are applied in the
[Apply the Platform](#apply-the-platform) step below. The shared Istio
Gateway (the `istio-gateway` component) then serves ports 80 and 443, and
platform services attach `HTTPRoute`s to it. The HTTPS listener terminates
TLS with a wildcard `*.holos.localhost` certificate issued by cert-manager's
`local-ca` ClusterIssuer from the mkcert root CA installed in the
[Setup Trusted TLS](#setup-trusted-tls) step below — the same root CA your
host trusts, so browsers accept the certificate without warnings. See
[mesh-enrollment.md](../holos/docs/mesh-enrollment.md) for how workload
namespaces enroll in the ambient mesh.

The static cluster shape — port mappings, k3s args — is defined in
[`k3d/config.yaml`](../k3d/config.yaml), which is the source of truth for
cluster structure. The cluster name, registry hostname, and registry port are
passed at runtime by `scripts/local-k3d` (positional name argument and
`--registry-use`), so `CLUSTER_NAME`, `REGISTRY_NAME`, and `REGISTRY_PORT`
are fully honored without editing `k3d/config.yaml`.

**Environment variable overrides:**

| Variable        | Default                     | Description                                  |
|-----------------|-----------------------------|----------------------------------------------|
| `CLUSTER_NAME`  | `holos`                     | Cluster name (honored at creation and reset) |
| `REGISTRY_NAME` | `registry.holos.localhost`  | Registry hostname (honored at creation)      |
| `REGISTRY_PORT` | `5100`                      | Registry host port (honored at creation)     |

> **Note:** only one cluster created from this config can exist at a time —
> the config binds host ports 80 and 443, so creating a second cluster under a
> different `CLUSTER_NAME` fails with a port conflict until the first is
> deleted.

## Setup Trusted TLS

Install the mkcert root CA into the cluster so cert-manager can issue trusted
certificates for `*.holos.localhost`:

```bash
sudo -v
scripts/local-ca
```

**Run this each time you recreate the cluster.**

This installs the mkcert root CA into the host trust store (the one-liner
`mkcert --install`, run by the script), creates the `cert-manager` namespace,
and applies the `local-ca` Secret (`type: kubernetes.io/tls`) that
cert-manager's `local-ca` ClusterIssuer references.
The generated `namespace.yaml` and `local-ca.yaml` are saved to
`$(mkcert -CAROOT)` (mode 0600 for the key material) so you can reapply them
after a cluster reset without re-running `mkcert --install`.

## Apply the Platform

Apply the rendered platform manifests to the cluster:

```bash
scripts/apply
```

The script applies every platform component in dependency order — the
Layer 0 foundation (namespaces, the Istio ambient mesh, cert-manager, the
shared Gateway) followed by the Layer 1 services (CloudNativePG Postgres,
the Keycloak operator and instance, and the Quay registry) — starting
with the
`namespaces` component, so every namespace exists before any namespaced
resource applies. It is idempotent, so it is safe to re-run at any time.
See
[How rendered manifests reach the cluster](../holos/README.md#how-rendered-manifests-reach-the-cluster)
for the apply-order rationale and the `--force-conflicts` and webhook
caveats. The late gates wait for the Keycloak CR to report Ready, the
`holos` realm import to complete, and the `quay` Deployment to roll out,
so the first run takes several minutes while images pull and Keycloak and
Quay run their database schema migrations (`KEYCLOAK_TIMEOUT`, default
600s; `QUAY_TIMEOUT`, default 900s).

When the script completes, the platform serves ports 80 and 443 and the
`echo` smoke-test workload answers at `https://echo.holos.localhost/`
with a browser-trusted certificate:

```bash
curl https://echo.holos.localhost/
```

## Verify Keycloak

The full bootstrap from zero — `scripts/local-dns` (one-time), then
`scripts/local-k3d`, `scripts/local-ca`, and `scripts/apply` — brings up
Keycloak with the `holos` realm and no manual steps. Verify it the same
way as the echo smoke test: the console answers at
`https://auth.holos.localhost/` with a browser-trusted certificate, and
the `holos` realm serves its OIDC discovery document:

```bash
curl -fsSI https://auth.holos.localhost/
curl -fs https://auth.holos.localhost/realms/holos/.well-known/openid-configuration | jq .issuer
```

The Keycloak operator generates the initial admin credentials on first
reconcile and stores them in the `keycloak-initial-admin` Secret — no
credentials are committed to this repository. Retrieve them and log in to
the admin console at `https://auth.holos.localhost/admin/`:

```bash
kubectl -n keycloak get secret keycloak-initial-admin -o json \
  | jq '.data | map_values(@base64d)'
```

Keycloak's state lives in the `keycloak-db` Postgres `Cluster`, not the
pod, and the `holos` realm import is bootstrap-only: the operator's
import Job skips when the realm already exists, so post-bootstrap realm
changes are not reconciled from the `KeycloakRealmImport` CR (see the
caveat in
[`holos/components/keycloak/instance/buildplan.cue`](../holos/components/keycloak/instance/buildplan.cue)).
For the full verification steps — including the pod restart-survival
check — see
[Keycloak admin credentials and verification](../holos/README.md#keycloak-admin-credentials-and-verification);
for the database contract, see
[Postgres credentials and connection contract](../holos/README.md#postgres-credentials-and-connection-contract).

## Verify Quay

`scripts/apply` brings the Quay registry up at
`https://quay.holos.localhost`, but a fresh registry has no users.
Bootstrap it once:

```bash
scripts/quay-init
```

The script is idempotent — a second run exits 0 without changing state.
It creates the initial `admin` user, the `holos` organization, and the
`holos+robot` robot account (a member of a `creators` team, so pushes
auto-create repositories under `holos/`), and stores the generated
credentials in Secrets in the `quay` namespace — nothing secret is
committed to this repository, mirroring the Keycloak
`keycloak-initial-admin` pattern:

```bash
kubectl -n quay get secret quay-initial-admin -o json \
  | jq '.data | map_values(@base64d)'      # admin UI login + API token
kubectl -n quay get secret quay-robot-pull  # dockerconfigjson for pull consumers
```

Verify the registry with a push from the host. Log in as the robot —
the one-liner extracts the robot token from the pull Secret:

```bash
kubectl -n quay get secret quay-robot-pull -o jsonpath='{.data.\.dockerconfigjson}' \
  | base64 -d | jq -r '.auths["quay.holos.localhost"].auth' | base64 -d | cut -d: -f2- \
  | docker login quay.holos.localhost -u 'holos+robot' --password-stdin
```

Then push — the repository does not exist yet; the robot's `creators`
team membership auto-creates it:

```bash
docker pull busybox
docker tag busybox quay.holos.localhost/holos/sample:test
docker push quay.holos.localhost/holos/sample:test
```

Confirm the pushed tag in the Quay UI: log in at
`https://quay.holos.localhost` with the `quay-initial-admin` credentials
and find `holos/sample` under the `holos` organization (repositories
auto-created by push are private).

> **Docker trust note:** on the MVP target — OrbStack on Apple silicon
> ([ADR-7](adr/ADR-7.md)) — OrbStack syncs the macOS keychain trust store
> into its Docker daemon, so the mkcert root installed by
> `scripts/local-ca` is already trusted and `docker push` just works. With
> Docker Desktop instead place the CA at
> `~/.docker/certs.d/quay.holos.localhost/ca.crt`
> (`mkcert -CAROOT` prints the directory containing `rootCA.pem`).

In-cluster pulls of `quay.holos.localhost/...` images by the k3d nodes'
containerd are out of scope here — node-level DNS and CA trust for the
registry hostname is a separate concern, tracked by
[HOL-1184](https://linear.app/holos-run/issue/HOL-1184/featquay-in-cluster-image-pulls-from-quayholoslocalhost)
and stubbed in
[placeholders.md](../holos/docs/placeholders.md#node-level-registry-trust-for-in-cluster-pulls).
`scripts/quay-init` only provisions the credentials and the
`quay-robot-pull` pull Secret.

## Reset the Cluster

To reset to a clean state:

```bash
scripts/local-k3d    # Deletes and recreates the cluster (5-second abort window)
sudo -v
scripts/local-ca     # Re-installs the mkcert root CA
```

Alternatively, reapply the manifests saved by the previous `scripts/local-ca`
run without re-running `mkcert --install`:

```bash
scripts/local-k3d
kubectl apply --server-side=true -f "$(mkcert -CAROOT)/namespace.yaml"
kubectl apply --server-side=true -f "$(mkcert -CAROOT)/local-ca.yaml"
```

Either way, the new cluster is empty — re-run `scripts/apply` (see
[Apply the Platform](#apply-the-platform)) to bring the platform
back up.

## Clean Up

Remove the cluster entirely:

```bash
k3d cluster delete holos
k3d registry delete registry.holos.localhost
```
