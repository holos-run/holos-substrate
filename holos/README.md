# holos/ — Deployment Configuration and Policy

The Holos CUE configuration that renders this platform's Kubernetes
manifests using the [Holos](https://holos.run/) rendered-manifests pattern.
This directory is isolated from the Go code per
[ADR-12](../docs/adr/ADR-12.md).

To add or change a component, read
[docs/component-guidelines.md](docs/component-guidelines.md). Components
whose namespaces carry workloads must follow the ambient mesh enrollment
convention in [docs/mesh-enrollment.md](docs/mesh-enrollment.md).
Out-of-scope concerns with a planned home are stubbed in
[docs/placeholders.md](docs/placeholders.md).

## Directory layout

```text
holos/
├── cue.mod/         # CUE module: schemas vendored from holos and k8s APIs
├── platform/        # the Platform spec: registered clusters and components
├── components/      # one directory per component (BuildPlan definitions)
├── deploy/          # rendered manifests, committed: clusters/<cluster>/components/<name>/
└── docs/            # operational guidelines for this directory
```

- **`platform/platform.cue`** registers clusters and components. Every
  cluster in the `clusters` struct gets every registered component,
  parameterized by the `clusterName` tag.
- **`components/<name>/`** holds each component's `buildplan.cue` and
  boilerplate. See the
  [component guidelines](docs/component-guidelines.md#component-directory-anatomy)
  for the anatomy.
- **`deploy/`** is generated output — never edit it by hand. Render with
  `holos render platform` from this directory and commit the result; the
  tree must be diff-clean on re-render. `scripts/render` (from the repo
  root) checks exactly that: it removes `deploy/`, re-renders, and fails
  if anything under `holos/` is modified, deleted, or untracked — catching
  stale edits and orphaned manifests alike.

## Clusters: local development now, production later

The only registered cluster is **`k3d-holos`**, the local development
cluster — [docs/local-cluster.md](../docs/local-cluster.md) is the
quick-start guide for creating it and applying the platform to it. The MVP
demo target is a single Apple Silicon Mac ([ADR-7](../docs/adr/ADR-7.md)).

A production deployment area is planned but not yet established: production
clusters will be registered alongside `k3d-holos` in
`platform/platform.cue`, and each registered cluster renders its own
`deploy/clusters/<cluster>/` tree. See
[docs/placeholders.md](docs/placeholders.md#production-deployment-area).

## How rendered manifests reach the cluster

During bootstrap there is no gitops controller in the cluster yet, so
rendered manifests are applied directly with server-side apply.
`scripts/apply` (from the repo root) applies every platform component — the
Layer 0 foundation and the Layer 1 services — to the
current kubectl context in the correct order:

```bash
scripts/apply
```

This section is the canonical explanation of *why* the apply order is what
it is and the caveats that come with force-applying. For the step-by-step path
from nothing to a running platform — DNS setup, cluster creation, trusted
TLS, then this apply step — follow the quick-start guide,
[docs/local-cluster.md](../docs/local-cluster.md).

The script is idempotent: server-side apply and `kubectl wait` both
converge, so re-running it against a fresh, partially applied, or fully
applied cluster is safe. As a guard against force-applying to the wrong
cluster, it refuses to run when the current context is not `k3d-holos`
unless `KUBE_CONTEXT` is set explicitly, and pins every kubectl call to
the resolved context. Per component it runs

```bash
kubectl apply --server-side --force-conflicts -f holos/deploy/clusters/k3d-holos/components/<name>/
```

and waits only on the critical dependencies between components — CRD
establishment, the istiod rollout, the ambient data-plane DaemonSets, the
cert-manager webhook rollout, the CNPG operator rollout, the Postgres
`Cluster` Ready conditions, and the Keycloak operator rollout — plus waits
on the `echo` Deployment, the `Keycloak` CR Ready and realm import Done
conditions, and the `quay` Deployment rollout as smoke checks; nothing
else.

Apply order matters beyond "CRD components first". The script applies the
platform components in this order — everything through `echo` is the
Layer 0 cluster foundation; everything from `cnpg-crds` on is a Layer 1
platform service:

1. `namespaces` — every platform Namespace, from the central registry
   ([namespaces.cue](namespaces.cue)); labeled `namespaces: "true"` so apply
   tooling can select it
2. `gateway-api` — Gateway API standard channel CRDs (`crds: "true"`)
3. `cert-manager-crds` — cert-manager CRDs (`crds: "true"`)
4. `istio-base` — Istio CRDs and validation webhook (`crds: "true"`)
5. `istiod` — the Istio control plane
6. `istio-cni` — the node agent that redirects ambient pod traffic to ztunnel
7. `istio-ztunnel` — the ambient node proxy
8. `cert-manager` — the certificate controller, webhook, and cainjector
9. `local-ca` — the CA `ClusterIssuer` that signs all platform certificates
10. `istio-gateway` — the shared Gateway all platform services attach
    `HTTPRoute`s to, and its wildcard TLS certificate
11. `echo` — the permanent smoke-test workload and its `HTTPRoute`
12. `cnpg-crds` — CloudNativePG CRDs (`crds: "true"`), filtered out of the
    single upstream release manifest
13. `cnpg` — the CloudNativePG operator, the platform's single Postgres
    operator
14. `cnpg-clusters` — the per-service Postgres `Cluster` resources
    (`keycloak-db`, `quay-db`), each in its consuming service's namespace
15. `keycloak-operator-crds` — Keycloak operator CRDs (`crds: "true"`),
    fetched as the two separate upstream single-CRD manifests
16. `keycloak-operator` — the Keycloak operator, in the `keycloak`
    namespace (deliberately not ambient-enrolled, see
    [namespaces.cue](namespaces.cue))
17. `keycloak` — the Keycloak server instance: the `Keycloak` CR backed by
    the `keycloak-db` Postgres `Cluster`, its TLS `Certificate`, the
    declarative `holos` realm import, the `HTTPRoute` attaching it to the
    shared Gateway at `auth.holos.localhost`, and the `DestinationRule`
    that re-encrypts the Gateway→Keycloak hop
18. `quay` — the Quay registry: the Quay `Deployment` backed by the
    `quay-db` Postgres `Cluster` and a minimal `quay-redis` Deployment,
    with blob storage on a local-path PVC and the `HTTPRoute` pair
    attaching it to the shared Gateway at `quay.holos.localhost`

The order encodes six rules: the `namespaces` component applies first, so
every Namespace exists before any component that populates it;
CRD components (labeled `crds: "true"`) apply before the controllers that
depend on their types; `istiod` applies before
the Gateway, because the `istio` GatewayClass must exist and istiod must be
running to program the Gateway; `istio-cni` and `istio-ztunnel` apply before
ambient-enrolled workloads like `cert-manager`, `echo`, and `cnpg`, because
they must be capturing traffic when those workloads start (the Gateway
itself is deliberately not enrolled, see
[docs/mesh-enrollment.md](docs/mesh-enrollment.md)); components with
fail-closed admission webhooks apply — and their Deployments are waited on —
before the components that create the resources they admit: `cert-manager`
before the `cert-manager.io` resources (`local-ca`'s `ClusterIssuer`,
`istio-gateway`'s `Certificate`), with a retry on the transient x509
admission error while cainjector injects the webhook's CA bundle, and the
`cnpg` operator before the `postgresql.cnpg.io` `Cluster` resources the
`cnpg-clusters` component creates — with the same shape of retry, because
the operator's webhook may briefly reject admission after its rollout is
Available; and the Gateway applies before components that attach
routes to it. `cnpg-crds` and `cnpg` trail `echo` because CNPG depends only
on its own CRDs (and, being ambient-enrolled, the data plane), so appending
them keeps the established order stable. `cnpg-clusters` trails `cnpg` and
is gated on each `Cluster`'s `Ready` condition because the Keycloak phase
applies a Keycloak CR that needs a reachable database.
`keycloak-operator-crds` and `keycloak-operator` trail `cnpg-clusters`: the
Keycloak operator depends only on its own CRDs and the `keycloak`
namespace, and the `keycloak` component applies `Keycloak` and
`KeycloakRealmImport` CRs that need both the operator reconciling — hence
the gate on its Deployment rollout — and the `keycloak-db` `Cluster`
reachable, so appending the pair after the database keeps the dependency
chain linear. `keycloak` trails the operator: its CRs need everything
above, and it creates a `cert-manager.io` `Certificate`, so its apply
retries through the same transient webhook admission window as `local-ca`
and `istio-gateway`. Its gate waits on the `Keycloak` CR Ready condition
and then on the `holos` `KeycloakRealmImport` Done condition as the Layer 1
smoke check, so a bootstrap cannot report success while the realm import
Job is still running or has failed — the first start pulls the server
image and runs the database schema migrations, so each wait gets a more
generous timeout (`KEYCLOAK_TIMEOUT`, default 600s) than the rollout
gates. `quay` applies last because it needs the `quay-db` `Cluster`
reachable — already gated Ready in the `cnpg-clusters` step — and its gate
waits on the secret-keys bootstrap Job and then on the `quay` Deployment
rollout with its own generous timeout (`QUAY_TIMEOUT`, default 900s),
since the first pull of the Quay image is large and the first start runs
Quay's database schema migrations.

The first rule exists because nothing orders an apply batch by kind:
kubectl submits the files sequentially in lexical order, so a single
server-side apply that carries a Namespace alongside its namespaced
resources fails with `NotFound` on the first apply whenever a namespaced
resource sorts ahead of its Namespace. The last rule is for verifiability
rather than correctness — route attachment is level-triggered, so an
`HTTPRoute` applied early simply reports unattached until the Gateway
exists — but applying `echo` after the Gateway means the smoke test
exercises a complete traffic path immediately. Certificate issuance is
level-triggered the same way: the Gateway's HTTPS listener reports an
unresolved certificate ref only until cert-manager writes the wildcard
certificate's Secret.

`--force-conflicts` is safe here because the rendered manifests in git are
the source of truth for these resources and, with the exceptions below, no
other controller manages their fields during bootstrap; do not copy it into
contexts where another field manager owns the resources.

cert-manager's cainjector manages `webhooks[].clientConfig.caBundle` on the
rendered cert-manager webhook configurations at runtime. Unlike the Istio
exception below, the field is absent from the rendered manifests, so a
re-apply with `--force-conflicts` never claims or strips it — no enforcement
gap results. The CNPG operator manages the `caBundle` on its own webhook
configurations the same way, and the field is likewise absent from the
rendered `cnpg` manifests.

The other exception is Istio's webhook reconciliation: the rendered
`ValidatingWebhookConfiguration`s (`istiod-default-validator` in
`istio-base`, `istio-validator-istio-system` in `istiod`) set
`failurePolicy: Ignore`, and istiod patches the field to `Fail` once it is
ready to serve admission requests. Re-applying either component with
`--force-conflicts` seizes the field back and downgrades it to `Ignore`
until istiod re-patches it — expect that transient enforcement gap (and the
resulting field-manager churn) on every re-apply of those two components,
including every re-run of `scripts/apply`.

ArgoCD-based delivery is planned to replace the direct apply performed by
`scripts/apply` once ArgoCD is deployed to the platform — until then every
component renders with
`argoAppDisabled: true` and no Application resources are emitted. See
[docs/placeholders.md](docs/placeholders.md#argocd-gitops-delivery).

### Keycloak admin credentials and verification

The Keycloak operator bootstraps the initial admin user itself and stores
the generated credentials in the `keycloak-initial-admin` Secret (keys
`username` and `password`) on first reconcile — no credentials are
committed to this repository. Retrieve them:

```bash
kubectl -n keycloak get secret keycloak-initial-admin -o json \
  | jq '.data | map_values(@base64d)'
```

Verify Keycloak on the live cluster after `scripts/apply`:

```bash
kubectl -n keycloak wait keycloak/keycloak --for=condition=Ready --timeout=600s
curl -fsSI https://auth.holos.localhost/        # trusted chain via the mkcert root
curl -fs https://auth.holos.localhost/realms/holos/.well-known/openid-configuration | jq .issuer
# log in to https://auth.holos.localhost/admin/ with the credentials above
```

State lives in the `keycloak-db` Postgres `Cluster`, not the pod: deleting
the Keycloak pod (`kubectl -n keycloak delete pod -l
app.kubernetes.io/managed-by=keycloak-operator`) loses nothing — after the
operator restarts it, the `holos` realm and admin login still work. Note
the realm import is bootstrap-only: the operator's import Job skips when
the realm already exists, so post-bootstrap realm changes are not
reconciled from the `KeycloakRealmImport` CR — see the caveat in
[components/keycloak/instance/buildplan.cue](components/keycloak/instance/buildplan.cue)
and the stub in
[docs/placeholders.md](docs/placeholders.md#keycloak-realm-reconciliation).

### Postgres credentials and connection contract

The `cnpg-clusters` component provisions one Postgres `Cluster` per
consuming service, in that service's namespace. CNPG generates the
credentials and connection endpoints with conventional names — this is the
contract the Keycloak and Quay components consume:

| Cluster | Namespace | Credentials Secret | Read-write Service |
|---------|-----------|--------------------|--------------------|
| `keycloak-db` | `keycloak` | `keycloak-db-app` | `keycloak-db-rw.keycloak.svc:5432` |
| `quay-db` | `quay` | `quay-db-app` | `quay-db-rw.quay.svc:5432` |

Each `<cluster>-app` Secret carries the keys `username`, `password`,
`dbname`, `host`, `port`, `uri`, and `jdbc-uri`.

Verify the databases on the live cluster after `scripts/apply`:

```bash
kubectl get cluster -A                       # both: Cluster in healthy state
kubectl -n keycloak get secret keycloak-db-app
kubectl -n quay get secret quay-db-app
kubectl -n keycloak exec keycloak-db-1 -- psql -U postgres -c 'SELECT 1'
kubectl -n quay exec quay-db-1 -- psql -U postgres -c 'SELECT 1'
```

To exercise the same path the consuming service uses — the `-rw` Service
with the `-app` credentials — run a short-lived client pod with the `uri`
key from the Secret:

```bash
URI=$(kubectl -n keycloak get secret keycloak-db-app -o jsonpath='{.data.uri}' | base64 -d)
kubectl -n keycloak run psql-verify --rm -i --restart=Never \
  --image=ghcr.io/cloudnative-pg/postgresql:18.1 --env="URI=$URI" -- \
  psql "$URI" -c 'SELECT current_user, current_database()'
```
