# Placeholders — Out of MVP Scope

Stubs for concerns deliberately deferred beyond the MVP, so future work has a
clear home. Each entry states the intent and where the work will land — no
speculative design. When an item is implemented, replace its stub with a link
to the real documentation.

## ArgoCD gitops delivery

Rendered manifests will eventually be reconciled by ArgoCD instead of the
direct server-side apply performed by `scripts/apply`. The affordance
already exists: the `userDefinedBuildPlan` adapter
([`components/user-defined-build-plan.cue`](../components/user-defined-build-plan.cue))
projects an ArgoCD `Application` per component through its `gitops` artifacts,
gated by `argoAppDisabled: bool | *true`. The future ArgoCD issue deploys
ArgoCD to the platform, flips the default to `false`, and renders the
Application resources under `deploy/clusters/<cluster>/gitops/`. Until then no
Application resources are emitted and manifests are applied per
[`holos/README.md`](../README.md#how-rendered-manifests-reach-the-cluster).

## Observability dashboards

The platform has no observability stack yet — no metrics collection, no log
aggregation, no dashboards. The intent is to document, alongside the
component guidelines, what each component must expose (structured logs,
`/metrics` endpoints) and to ship dashboards for the platform's own services
once an observability stack is chosen and deployed. Until that decision is
made (it warrants an ADR), components are not required to carry
observability-specific labels or annotations.

## Shared Gateway route-attachment policy

The shared Gateway emitted by
[`components/istio-gateway/`](../components/istio-gateway/buildplan.cue) sets
`allowedRoutes.namespaces.from: All` on its listener: any namespace may attach
`HTTPRoute`s and claim hostnames under the gateway's wildcard. That is
acceptable while every namespace on the cluster is platform-managed, but it
permits hostname squatting once untrusted tenant namespaces exist (Gateway API
resolves route conflicts oldest-wins). Before tenant workloads land, tighten
the policy to `from: Selector` with a namespace label selector (or per-tenant
listeners) and document the route-attachment convention — a separate concern
from the ambient mesh enrollment convention in
[mesh-enrollment.md](mesh-enrollment.md), which is already in force.

## Keycloak realm reconciliation

The `holos` realm is created declaratively at bootstrap by the
`KeycloakRealmImport` CR in
[`components/keycloak/instance/`](../components/keycloak/instance/buildplan.cue),
but the import is bootstrap-only: the operator's import Job skips when the
realm already exists, so post-bootstrap changes to the CR — new clients,
roles, users — are not reconciled into the live realm. Until a
reconciliation mechanism is chosen (a keycloak-config-cli-style tool, a
platform reconciler per [ADR-2](../../docs/adr/ADR-2.md), or an upstream
operator feature), realm changes on an existing cluster are manual: apply
them in the admin console, or delete and re-import the realm — which
destroys realm state. The Quay issue hits this first when it enables the
placeholder `quay` OIDC client and provisions its credentials.

## Production deployment area

The only registered cluster is the local `k3d-holos` development cluster.
Production will be additional clusters registered in
[`platform/platform.cue`](../platform/platform.cue) — the `clusters` struct
already supports this (`clusters: "prod-us-east-1": _`), and every registered
cluster renders its own `deploy/clusters/<cluster>/` tree from the same
components. Establishing the production area means registering the clusters,
deciding any per-cluster parameterization (versions, sizing), and documenting
the promotion flow from local to production.
