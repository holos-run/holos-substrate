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
under `deploy/clusters/<cluster>/gitops/`. Until then this projection emits
no Application resources, and the platform's own components are applied per
[`holos/README.md`](../README.md#how-rendered-manifests-reach-the-cluster)
rather than reconciled by ArgoCD.

> **Not to be confused with the hand-authored sample Applications.** This
> deferred stub concerns the **per-component `argoAppDisabled` projection**
> that would reconcile *the platform's own components*. It is distinct from the
> **hand-authored** Argo CD `Application`s the Kargo delivery pipelines own —
> `echo` ([`components/kargo-echo/`](../components/kargo-echo/buildplan.cue))
> and `my-project`
> ([`components/my-project/`](../components/my-project/buildplan.cue), see
> [holos/README.md → The `my-project` delivery scaffold](../README.md#the-my-project-delivery-scaffold)).
> Those Applications carry an **OCI** source pointing at a rendered-manifests
> artifact and are reconciled by ArgoCD today (once their artifact is
> published); the deferred projection would emit a **git**-source Application
> per component, which is the wrong shape for Kargo to patch. The two are
> independent: enabling the projection does not change the hand-authored
> sample Applications, and the sample Applications do not depend on it.

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

**Resolved.** The `holos` realm is created declaratively at bootstrap by the
`KeycloakRealmImport` CR in
[`components/keycloak/instance/`](../components/keycloak/instance/buildplan.cue),
but that import is bootstrap-only: the operator's import Job skips when the
realm already exists, so post-bootstrap changes to the CR are not reconciled
into the live realm. The reconciliation mechanism is now the
[`keycloak-config`](../components/keycloak/realm-config/buildplan.cue)
component — an idempotent
[keycloak-config-cli](https://github.com/adorsys/keycloak-config-cli) `Job`
that converges the realm against the live admin API on every `scripts/apply`.
It manages the platform's realm roles, the `authenticated` default group, and
the Argo CD OIDC client declaratively, so realm changes land by editing the
import document and re-applying rather than by manual admin-console edits. The
`KeycloakRealmImport` CR still bootstraps the realm shell on a clean cluster;
the `keycloak-config` Job layers managed objects onto it and keeps them
converged. Realm objects the Job does not declare are left untouched
(keycloak-config-cli's default no-delete managed-import behavior; full-realm
purge is deliberately not enabled).

## Quay OIDC login against the Keycloak `holos` realm

**Resolved.** Quay now signs users in through the Keycloak `holos` realm with
the Authorization Code flow, using the confidential `quay` client (authenticated
by its client secret, with **no** PKCE — ADR-15 Revision 7 / HOL-1317) reconciled by the
`keycloak-config` Job. Quay runs `AUTHENTICATION_TYPE: OIDC`, so the realm is the
sole identity store. The username is taken from the ID token's
`preferred_username` claim with no customization, and the `quay` client roles
(`platform-admin`, `project-admin`) plus Keycloak groups are emitted in the
shared `groups` claim. Automatic group→Quay-team synchronization is **on**
(`FEATURE_TEAM_SYNCING: true`, `TEAM_RESYNC_STALE_TIME: 30m`, ADR-15 Revision 4):
under the OIDC backend the user handler syncs OIDC groups, so Quay team
membership tracks the claim on the 30-minute resync cadence. The design is
recorded in [ADR-15](../../docs/adr/ADR-15.md) and the verification steps are in
[Verify Quay](../../docs/local-cluster.md#verify-quay); the
[`holos/README.md`](../README.md#quay-oidc-sso-and-roles) Quay overview is the
operator-facing companion. There is no local
`admin` user; the seeded superusers are the two Keycloak realm users
`svc-quay-resource-controller` (a service account) and `quay-admin` (a human
administrator) in `SUPER_USERS`.

The bootstrap `KeycloakRealmImport` CR
([`components/keycloak/instance/`](../components/keycloak/instance/buildplan.cue))
creates only the realm shell; the live `quay` client is owned and reconciled
by the `keycloak-config` Job. The earlier disabled placeholder client in that
import was removed in HOL-1221 so the two never disagree.

### Deferred: automatic `platform-owner` → Quay superuser sync

**Deferred.** Granting a user the `platform-owner` realm role does **not**
automatically make them a Quay **superuser**. As of HOL-1245 the `quay` client
emits the `platform-owner` realm role into the shared `groups` claim (the
realm-role mapper, mirroring the `argocd` client), and the
[Keycloak realm reconciliation](#keycloak-realm-reconciliation) Job keeps that
mapper converged — but the claim only carries the role *name*. It
confers no superuser status, because Quay's `SUPER_USERS` is a **static
username list in `config.yaml`** with no claim-driven superuser sync: there is
no mechanism for Quay to promote a user to superuser from an OIDC claim.

What exists today: the realm-role→`groups`-claim mapper (HOL-1245) emits
`platform-owner` into the shared `groups` claim (which Quay team syncing consumes
on the `TEAM_RESYNC_STALE_TIME` cadence — `FEATURE_TEAM_SYNCING: true` under the
OIDC backend, ADR-15 Revision 4 — for team membership, though **not** superuser),
and the **manual `SUPER_USERS` bootstrap** is the supported path to grant
superuser — add the user's `preferred_username` to `SUPER_USERS` in
[`components/quay/buildplan.cue`](../components/quay/buildplan.cue) and
re-render/apply. The seeded superusers are the two Keycloak realm users
`svc-quay-resource-controller` and `quay-admin` in `SUPER_USERS`.

Why deferred: closing the gap means a claim-driven superuser reconciler (Quay
exposes no such hook today, so it would be custom automation against Quay's
admin API), which is out of MVP scope. The full role/superuser model and the
client pattern are documented in
[keycloak-clients.md](keycloak-clients.md) (see *The Quay-superuser limitation
(not automatic)*); the operator-facing summary is the **Superusers** bullet in
[`holos/README.md`](../README.md#quay-oidc-sso-and-roles). If a future issue
adds the sync, replace this stub with a link to the real documentation.

## Node-level registry trust for in-cluster pulls

Pushes to `quay.holos.localhost` from the host work (the host resolves
`*.holos.localhost` and trusts the mkcert root CA), but containerd on the
k3d nodes can neither resolve nor trust the registry hostname, so pods
cannot run images pushed to Quay. The gap must close before the cluster can
pull and run the application images published to Quay — likely k3d
`registries`/hosts configuration plus CA trust on the nodes.
[HOL-1184](https://linear.app/holos-run/issue/HOL-1184/featquay-in-cluster-image-pulls-from-quayholoslocalhost)
tracks it; the scope boundary is noted in the
[Verify Quay](../../docs/local-cluster.md#verify-quay) section of the
local cluster guide.

## NATS in-cluster authentication and webhook edge verification — retired

Two earlier placeholders covered the NATS event-driven deployment pipeline: the
unauthenticated `nats` JetStream backbone (NKEY/credentials auth deferred) and
the thin webhook receiver's deferred edge signature verification
([ADR-9](../../docs/adr/ADR-9.md)/[ADR-10](../../docs/adr/ADR-10.md)).

That pipeline was **retired in HOL-1241**: ADR-9/10/11/14 are now `Deprecated`
and superseded by [ADR-16](../../docs/adr/ADR-16.md), and the
`nats`/`webhook-receiver`/`webhook-subscriber` components, their Go code, and the
`wss://nats.holos.localhost` debug endpoint were removed. Deployment is now
driven by Kargo plus the client-side build-and-publish workflow
([`oci-publish-workflow.md`](oci-publish-workflow.md)) — there is no inbound
webhook ingress to authenticate and no in-cluster NATS surface to harden, so both
placeholders no longer apply. If a messaging backbone is reintroduced later, its
authentication posture should be recorded as a fresh placeholder.

## Production deployment area

The only registered cluster is the local `k3d-holos` development cluster.
Production will be additional clusters registered in
[`platform/platform.cue`](../platform/platform.cue) — the `clusters` struct
already supports this (`clusters: "prod-us-east-1": _`), and every registered
cluster renders its own `deploy/clusters/<cluster>/` tree from the same
components. Establishing the production area means registering the clusters,
deciding any per-cluster parameterization (versions, sizing), and documenting
the promotion flow from local to production.
