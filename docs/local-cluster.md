# Local Cluster

<!-- Originally vendored from https://github.com/holos-run/holos/blob/main/doc/md/topics/local-cluster.mdx; has since diverged — do not overwrite with a re-vendor. -->

Set up a local k3d cluster for development and testing, then apply the
platform to it. This is the canonical quick-start guide: after completing
it you'll have a running platform — the Layer 0 foundation (a Kubernetes
API server with proper DNS and TLS certificates, serving the platform's
components on ports 80 and 443) plus the Layer 1 services:
CloudNativePG-managed Postgres, Keycloak with the `holos` realm at
`https://auth.holos.localhost`, the Quay registry at
`https://quay.holos.localhost`, and Argo CD at
`https://argocd.holos.localhost`.

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

The single wildcard rule covers every per-service hostname the platform
serves through the shared Istio Gateway, so no per-host `/etc/hosts` entries
are needed. The hostnames in use today are `auth.holos.localhost` (Keycloak),
`quay.holos.localhost` (Quay registry), `argocd.holos.localhost` (Argo CD),
`kargo.holos.localhost` (Kargo API/UI), and `kargo-webhooks.holos.localhost`
(Kargo's external-webhooks receiver — the URL Kargo advertises in
`ProjectConfig.status.webhookReceivers[].url` for Quay to POST to). Each
resolves to `127.0.0.1` on the host via the wildcard above and to the shared
Gateway VIP in-cluster via a `ServiceEntry`.

## Create the Cluster

Create a local k3d cluster with a container registry:

```bash
scripts/local-k3d
```

This creates:

- A local registry at `quay.holos.localhost/holos`
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

Every platform component runs an upstream image, so no local image build is
required before applying. Apply the rendered platform manifests to the
cluster:

```bash
scripts/apply
```

The script applies every platform component in dependency order — the
Layer 0 foundation (namespaces, the Istio ambient mesh, cert-manager, the
shared Gateway) followed by the Layer 1 services (CloudNativePG Postgres,
the Keycloak operator and instance, the Quay registry, and Argo CD) —
starting with the `namespaces` component, so every namespace exists before
any namespaced resource applies. It is idempotent, so it is safe to re-run
at any time.
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

`scripts/apply` no longer applies the `my-project` Layer 3 delivery sample —
it was removed from the master apply (HOL-1322) because its `quay.holos.run`
Organization carries a per-cluster local-ca `caBundle` that is injected at apply
time and never committed. Apply it separately, **after** these prerequisites are
in place, with the dedicated helper:

1. **Deploy the Holos Controller** with the isolated `controller-*` targets
   (`make controller-deploy` installs the `quay.holos.run` CRDs and the manager
   into the `holos-controller` namespace) — `scripts/apply` does **not** install
   them, and `scripts/apply-projects` fails fast if the Organization CRD is
   absent.
2. **Mint and store the Quay superuser credential** the controller authenticates
   with (`scripts/apply-svc-quay-resource-controller-creds` plus the
   `platform-automation` org / OAuth token per the runbooks).

```bash
scripts/apply-projects
```

That script reads the local-ca PEM, renders the platform with it injected via
the `ca_bundle_pem` CUE tag, and applies the `my-project` Namespace +
Organization (which the Holos Controller reconciles into the in-cluster Quay,
trusting Quay's serving cert via the `caBundle`) along with the rest of the
`my-project` component. See the
[Holos Controller runbook](runbooks/holos-controller.md) for the controller
deployment, credential wiring, and bring-up ordering.

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
pod. The `KeycloakRealmImport` CR only bootstraps the realm shell — the
operator's import Job skips when the realm already exists — so the
platform's realm roles, the `authenticated` default group, and the OIDC
clients are reconciled on every `scripts/apply` by the `keycloak-config`
keycloak-config-cli `Job` instead (see
[`holos/components/keycloak/realm-config/buildplan.cue`](../holos/components/keycloak/realm-config/buildplan.cue)
and [keycloak-config: realm reconciliation](../holos/README.md#keycloak-config-realm-reconciliation)).
For the full verification steps — including the pod restart-survival
check — see
[Keycloak admin credentials and verification](../holos/README.md#keycloak-admin-credentials-and-verification);
for the database contract, see
[Postgres credentials and connection contract](../holos/README.md#postgres-credentials-and-connection-contract).

## Verify Quay

`scripts/apply` brings the Quay registry up at `https://quay.holos.localhost`.
Quay runs `AUTHENTICATION_TYPE: OIDC` (see [ADR-15](adr/ADR-15.md)), so the
Keycloak `holos` realm is the **sole** identity store: there is no local `admin`
user, and every Quay login is "Holos SSO". The seeded superusers are two
Keycloak realm users in `SUPER_USERS` — **`svc-quay-resource-controller`** (a
service account, the shipped Holos Controller's Quay identity) and
**`quay-admin`** (a human administrator). Their passwords are generated once at
runtime by the keycloak phase (HOL-1294) into Secrets in the **`keycloak`**
namespace, one per user, under the `password` key — nothing secret is committed,
mirroring the Keycloak `keycloak-initial-admin` pattern.

Retrieve a password and base64-decode it:

```bash
# Human administrator:
kubectl -n keycloak get secret quay-admin \
  -o jsonpath='{.data.password}' | base64 -d; echo

# Service account (the Holos Controller's Quay identity):
kubectl -n keycloak get secret svc-quay-resource-controller \
  -o jsonpath='{.data.password}' | base64 -d; echo
```

In-cluster Quay org/repo/webhook provisioning is reconciled by the shipped Holos
Controller ([ADR-18](adr/ADR-18.md)) from the `quay.holos.run` CRDs
([ADR-19](adr/ADR-19.md)); the robot accounts and pull-credential Secrets stay
manual (ADR-19 *Out of scope*). The old `scripts/quay-init` org/robot bootstrap
and the `quay-initial-admin` token were removed with the Database backend
(HOL-1293). The controller **consumes** a superuser OAuth-Application credential
an operator mints by hand — see the
[Quay Resource Controller credentials runbook](runbooks/quay-resource-controller-credentials.md).

**Verify "Holos SSO" login and superuser access.** Sign in to Quay through the
Keycloak realm with the Authorization Code flow (the confidential `quay` client
the `keycloak-config` Job provisions, authenticated by its client secret **with
no PKCE** — HOL-1317, Quay 3.17.3 mishandles PKCE state across logout). The
design is in [ADR-15](adr/ADR-15.md); verify it end to end:

1. Open Quay and start SSO login:

   ```bash
   open https://quay.holos.localhost/
   ```

   Click **Sign in with Holos SSO** and authenticate as `quay-admin` (or
   `svc-quay-resource-controller`) with the password retrieved above. The local
   username/password form is hidden (`FEATURE_DIRECT_LOGIN: false`), so SSO is
   the only login path. To exercise the roles model, assign a non-superuser
   realm user the `quay` client role `platform-admin` or `project-admin` under
   **Users → (user) → Role mapping → Assign role → Filter by clients → `quay`**
   in the Keycloak admin console (`https://auth.holos.localhost/admin/`).
2. Confirm the SSO behavior:
   - **No username prompt.** Quay does not ask the user to choose or confirm a
     username (`FEATURE_USERNAME_CONFIRMATION: false`); login completes
     straight to the dashboard.
   - **Namespace matches the token.** The user's personal namespace equals
     their `preferred_username` claim — repositories live under
     `quay.holos.localhost/<preferred_username>/...`.
   - **Superuser.** Signed in as `quay-admin` or `svc-quay-resource-controller`,
     the **Super User Admin Panel** appears (both are in `SUPER_USERS`).
     Superuser status comes solely from `SUPER_USERS`, never from the `groups`
     claim or the `platform-admin` client role.
   - **Roles → teams.** The `groups` claim carries a user's `quay` client roles
     and bound Keycloak groups, and **automatic** group→team syncing is enabled
     under the OIDC backend (`FEATURE_TEAM_SYNCING: true`,
     `TEAM_RESYNC_STALE_TIME: 30m`; see [ADR-15](adr/ADR-15.md) Revision 4), so
     Quay team membership tracks the claim on the 30-minute resync cadence.

**Verify a push from the host** once an org and a push credential exist. Until
the Quay Resource Controller provisions them, create the org and a robot (or use
a superuser's personal namespace) through the Quay UI as `quay-admin`, then log
in with `docker` and push:

```bash
docker pull busybox
docker tag busybox quay.holos.localhost/<namespace>/sample:test
docker push quay.holos.localhost/<namespace>/sample:test
```

Confirm the pushed tag in the Quay UI under that namespace (repositories
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

## Verify Argo CD

`scripts/apply` brings Argo CD up at `https://argocd.holos.localhost`.
The server generates the initial `admin` password on first startup and
stores it in the `argocd-initial-admin-secret` Secret — no credentials are
committed to this repository. Verify the UI answers with a
browser-trusted certificate, then log in as the break-glass `admin`:

```bash
curl -fsSI https://argocd.holos.localhost/
kubectl -n argocd get secret argocd-initial-admin-secret \
  -o jsonpath='{.data.password}' | base64 -d; echo
```

**Verify Keycloak SSO (OIDC/PKCE).** Real users sign in through Keycloak,
not the `admin` account: Argo CD authenticates against the `holos` realm
with the Authorization Code flow plus PKCE (the public `argocd` client the
`keycloak-config` Job provisions), and maps the user's Keycloak realm role
to an Argo CD role — `platform-owner` → admin, `platform-viewer` /
`platform-editor` → read-only, and every authenticated realm user gets
baseline read-only access via the `authenticated` default group.

Create a realm user and grant it a platform role from the Keycloak admin
console at `https://auth.holos.localhost/admin/` (credentials from the
`keycloak-initial-admin` Secret, per [Verify Keycloak](#verify-keycloak)):
in the `holos` realm, **Users → Add user** (set a username, then a password
under **Credentials**, **Temporary: Off**), then **Role mapping → Assign
role → Filter by realm roles → `platform-owner`**.

Then log in to Argo CD as that user:

```bash
# Open the UI and click "LOG IN VIA Keycloak":
open https://argocd.holos.localhost/
```

After completing the Keycloak login you land back in Argo CD as an
**admin** (full access) because the user holds `platform-owner`. Repeat
with a second user granted `platform-viewer` instead and confirm that
session is **read-only** (the create/sync/delete actions are disabled).
The `admin` break-glass account above still works independently of SSO.

If the Keycloak button is missing or login fails, check the OIDC
backchannel — `argocd-server` must reach the issuer in-cluster (a
`ServiceEntry` resolves `auth.holos.localhost` to the ingress gateway):

```bash
kubectl -n argocd logs deploy/argocd-server | grep -iE 'oidc|x509|dial' || echo "no OIDC errors"
```

A clean run shows no OIDC discovery/JWKS or x509 errors.

The deferred per-component gitops projection emits no `Application`
resources until it is enabled (see
[placeholders.md](../holos/docs/placeholders.md#argocd-gitops-delivery)),
so Argo CD reconciles nothing from *that* path yet. It does own the
hand-authored sample `Application`s the Kargo delivery pipelines drive —
`echo` and `my-project` — which stay `Unknown`/`Missing` until their OCI
artifacts are published (see
[The `my-project` delivery scaffold](../holos/README.md#the-my-project-delivery-scaffold)).
For the full verification steps, the SSO/RBAC configuration, and the
service contract, see
[Argo CD admin credentials and verification](../holos/README.md#argo-cd-admin-credentials-and-verification).

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
k3d registry delete k3d-registry.holos.localhost
```
