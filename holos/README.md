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
cluster — see [docs/local-cluster.md](../docs/local-cluster.md) for creating
it with `scripts/local-k3d`. The MVP demo target is a single Apple Silicon
Mac ([ADR-7](../docs/adr/ADR-7.md)).

A production deployment area is planned but not yet established: production
clusters will be registered alongside `k3d-holos` in
`platform/platform.cue`, and each registered cluster renders its own
`deploy/clusters/<cluster>/` tree. See
[docs/placeholders.md](docs/placeholders.md#production-deployment-area).

## How rendered manifests reach the cluster

During Layer 0 bootstrap there is no gitops controller in the cluster yet, so
rendered manifests are applied directly with server-side apply, one component
at a time, CRD components first:

```bash
kubectl apply --server-side --force-conflicts -f holos/deploy/clusters/k3d-holos/components/<name>/
```

`--force-conflicts` is safe here because the rendered manifests in git are
the source of truth for these resources and, with one exception, no other
controller manages their fields during bootstrap; do not copy it into
contexts where another field manager owns the resources.

The exception is Istio's webhook reconciliation: the rendered
`ValidatingWebhookConfiguration`s (`istiod-default-validator` in
`istio-base`, `istio-validator-istio-system` in `istiod`) set
`failurePolicy: Ignore`, and istiod patches the field to `Fail` once it is
ready to serve admission requests. Re-applying either component with
`--force-conflicts` seizes the field back and downgrades it to `Ignore`
until istiod re-patches it — expect that transient enforcement gap (and the
resulting field-manager churn) on every re-apply of those two components.

ArgoCD-based delivery is planned to replace manual apply once ArgoCD is
deployed to the platform — until then every component renders with
`argoAppDisabled: true` and no Application resources are emitted. See
[docs/placeholders.md](docs/placeholders.md#argocd-gitops-delivery).
