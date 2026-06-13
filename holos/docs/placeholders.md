# Placeholders — Out of MVP Scope

Stubs for concerns deliberately deferred beyond the MVP, so future work has a
clear home. Each entry states the intent and where the work will land — no
speculative design. When an item is implemented, replace its stub with a link
to the real documentation.

## ArgoCD gitops delivery

ArgoCD itself is installed: the `argocd-crds` and `argocd` components
([`components/argocd/`](../components/argocd/argocd.cue)) deploy the core
install via `scripts/apply`, with the UI at `https://argocd.holos.localhost`,
and the `Application` source pattern the delivery will use — OCI
rendered-manifests artifacts in the in-cluster Quay registry — is decided,
verified, and documented in
[argocd-application-source.md](argocd-application-source.md). What remains
deferred is the delivery itself: reconciling the platform's rendered
manifests with ArgoCD instead of the direct server-side apply performed by
`scripts/apply`. The affordance already exists: the `userDefinedBuildPlan`
adapter
([`components/user-defined-build-plan.cue`](../components/user-defined-build-plan.cue))
projects an ArgoCD `Application` per component through its `gitops`
artifacts, gated by `argoAppDisabled: bool | *true`. The future delivery
issue flips the default to `false` and renders the Application resources
under `deploy/clusters/<cluster>/gitops/`. Until then no Application
resources are emitted, ArgoCD reconciles nothing, and manifests are applied
per [`holos/README.md`](../README.md#how-rendered-manifests-reach-the-cluster).

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
destroys realm state.
[Quay OIDC login](#quay-oidc-login-against-the-keycloak-holos-realm) hits
this first when it enables the placeholder `quay` client.

## Quay OIDC login against the Keycloak `holos` realm

Quay authenticates against its local database; OIDC login via the
Keycloak `holos` realm was deliberately deferred from the MVP. The realm
carries a **disabled** placeholder `quay` client with no secret committed
([`components/keycloak/instance/`](../components/keycloak/instance/buildplan.cue)).
[HOL-1183](https://linear.app/holos-run/issue/HOL-1183/featquay-oidc-login-via-the-keycloak-holos-realm)
enables the client, provisions its secret without committing it, and sets
Quay's OIDC configuration. Because the realm import is bootstrap-only,
enabling the client on an existing cluster takes more than a CR edit — the
work is effectively blocked on the
[Keycloak realm reconciliation](#keycloak-realm-reconciliation) mechanism
above.

## Node-level registry trust for in-cluster pulls

Pushes to `quay.holos.localhost` from the host work (the host resolves
`*.holos.localhost` and trusts the mkcert root CA), but containerd on the
k3d nodes can neither resolve nor trust the registry hostname, so pods
cannot run images pushed to Quay. The gap must close before Layer 2
([ADR-13](../../docs/adr/ADR-13.md)) deploys the images the pipeline
pushes — likely k3d `registries`/hosts configuration plus CA trust on the
nodes.
[HOL-1184](https://linear.app/holos-run/issue/HOL-1184/featquay-in-cluster-image-pulls-from-quayholoslocalhost)
tracks it; the scope boundary is noted in the
[Verify Quay](../../docs/local-cluster.md#verify-quay) section of the
local cluster guide and in `scripts/quay-init`.

## NATS in-cluster authentication

The `nats` JetStream backbone ([`components/nats/`](../components/nats/buildplan.cue))
runs with **no authentication** inside the cluster for the MVP: any
in-cluster client that can reach the client port (`4222`) may publish and
subscribe. The rationale is reachability — NATS is in-cluster only (no
`HTTPRoute`, never exposed outside the cluster), the `nats` namespace is
ambient-enrolled for mTLS transport identity, and an `AuthorizationPolicy`
restricts the client and monitoring ports to same-namespace sources until the
receiver/subscriber principals are added explicitly. NATS account/user
authentication (e.g. NKEYs or a credentials file per producer/consumer) is
deferred to a later issue; the connection contract and the deferred posture
are documented in
[`holos/README.md`](../README.md#nats-jetstream-backbone-and-connection-contract).
The receiver and subscriber components (HOL-1122/1123/1124) will extend the
same `AuthorizationPolicy` to allow their specific ServiceAccounts as they
land.

## Webhook edge signature verification

The webhook receiver ([`internal/webhook/receiver/`](../../internal/webhook/receiver/receiver.go),
deployed by [`components/webhook-receiver/`](../components/webhook-receiver/buildplan.cue))
is deliberately **thin** ([ADR-9](../../docs/adr/ADR-9.md)): it publishes the
raw request body to `webhooks.<source>` and acks the sender, performing **no
authentication**. Signature verification was deferred to the subscriber
([ADR-10](../../docs/adr/ADR-10.md)) for the MVP — the receiver carries the
signature headers (`X-Hub-Signature-256` / `X-Signature`) through verbatim so a
later stage can authenticate the sender against the raw body. Until then the
endpoint relies on network controls: it is reachable only at
`hooks.holos.localhost` (→ `127.0.0.1`) behind the ambient mesh, never exposed
off the local cluster, plus the configurable max-body-size bound. Moving
verification to the **edge** — rejecting forged senders with `401`/`403` before
they are ever enqueued, with a provider-pluggable HMAC check and a configurable
secret — is tracked by
[HOL-1200](https://linear.app/holos-run/issue/HOL-1200) and recorded as the
edge-auth resolution in [ADR-9](../../docs/adr/ADR-9.md)'s revision 2.

## Production deployment area

The only registered cluster is the local `k3d-holos` development cluster.
Production will be additional clusters registered in
[`platform/platform.cue`](../platform/platform.cue) — the `clusters` struct
already supports this (`clusters: "prod-us-east-1": _`), and every registered
cluster renders its own `deploy/clusters/<cluster>/` tree from the same
components. Establishing the production area means registering the clusters,
deciding any per-cluster parameterization (versions, sizing), and documenting
the promotion flow from local to production.
