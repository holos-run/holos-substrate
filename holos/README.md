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

During Layer 0 bootstrap there is no gitops controller in the cluster yet, so
rendered manifests are applied directly with server-side apply.
`scripts/apply` (from the repo root) applies every Layer 0 component to the
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
cert-manager webhook rollout, and the CNPG operator rollout — plus a wait
on the `echo` Deployment as a smoke check; nothing else.

Apply order matters beyond "CRD components first". The script applies the
Layer 0 components in this order:

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
`cnpg` operator before the `postgresql.cnpg.io` `Cluster` resources a later
phase introduces; and the Gateway applies before components that attach
routes to it. `cnpg-crds` and `cnpg` trail `echo` because CNPG depends only
on its own CRDs (and, being ambient-enrolled, the data plane), so appending
them keeps the established order stable.

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
