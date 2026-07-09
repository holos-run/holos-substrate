# Placeholders — Deferred Scope

Stubs for concerns deliberately deferred, so future work has a
clear home. Each entry states the intent and where the work will land — no
speculative design. When an item is implemented, replace its stub with a link
to the real documentation.

## ArgoCD gitops delivery

**Platform self-delivery is resolved — via an OCI App-of-Apps, not the
deferred git-source projection.** ArgoCD is installed (the `argocd-crds` and
`argocd` components, [`components/argocd/`](../components/argocd/argocd.cue),
UI at `https://argocd.holos.internal`), and the platform's own rendered
manifests are now reconciled by ArgoCD from an **OCI App-of-Apps over the
`holos-substrate-config:dev` config bundle** (HOL-1373, [ADR-16 Rev 3](../../docs/adr/archive/ADR-16.md)):

- The committed `holos/deploy/` tree is published as one OCI bundle
  (`holos-substrate-config:dev`, mutable tag) by `scripts/publish-config`
  (`make config-build`/`config-push`).
- The **platform** root `Application` (`platform-bootstrap`, AppProject
  **`platform`**) reconciles the system components from this bundle, tracking
  `targetRevision: dev` with an "Always" re-pull (the `argocd` component shortens
  the repo-cache TTL to `1m`). Tenant projects no longer share this bundle: each
  project has its **own** per-project bundle and `<project>-control-plane` /
  `<project>-workload` roots under AppProject **`projects`** (HOL-1382, the
  `project-app-of-apps` component).
- `scripts/apply` brings ArgoCD up imperatively (the bootstrap floor) and stops
  there; the separate `scripts/apply-platform-app-of-apps` then publishes the
  bundle and applies the platform root so ArgoCD takes over ongoing reconciliation
  — the chicken-and-egg handoff (ArgoCD must exist before it can self-manage),
  split out of `scripts/apply` in HOL-1379 because the publish needs the holos Quay
  organization configured first. Tenant projects are bootstrapped separately by
  `scripts/apply-projects-app-of-apps` (per-project bundles + control-plane roots)
  and `scripts/apply-project-workload-app-of-apps <project>` (HOL-1382). The
  detailed mechanism is in
  [oci-publish-workflow.md](oci-publish-workflow.md) (*Platform config bundle* /
  *The App-of-Apps that consumes the bundle*) and
  [argocd-application-source.md](argocd-application-source.md).

The earlier **per-component `git`-source projection** — the `userDefinedBuildPlan`
adapter's `argoAppDisabled: bool | *true` gate
([`components/user-defined-build-plan.cue`](../components/user-defined-build-plan.cue),
which would have emitted a git-source `Application` per component under
`deploy/clusters/<cluster>/gitops/`) — is **superseded for the platform** by this
OCI App-of-Apps and is **not used**. The gate stays at its `*true` default
(dormant, emitting nothing); the affordance is retained only for a hypothetical
future git-source need and is **not** the platform's delivery mechanism. Do not
flip it to deliver the platform — the OCI App-of-Apps already does that.

> **Not to be confused with the OCI Applications.** The dormant `argoAppDisabled`
> projection would emit a **git**-source Application per component — the wrong
> shape both for Kargo to patch and for the OCI-bundle bootstrap. It is distinct
> from the Argo CD `Application`s that deliver the platform and apps today, all of
> which carry an **OCI** source: the **platform App-of-Apps root** above
> (`platform-bootstrap`, sourcing `holos-substrate-config:dev`) plus each project's
> per-project `<project>-control-plane`/`<project>-workload` roots (sourcing
> `holos/<project>-config:dev`, HOL-1382);
> the **hand-authored** Kargo-driven pipeline Applications — `echo`
> ([`components/kargo-echo/`](../components/kargo-echo/buildplan.cue)) and the
> per-project/app Applications rendered by
> [`components/project/`](../components/project/buildplan.cue) /
> [`components/application/`](../components/application/buildplan.cue) from the
> `projects`/`apps` collections (see
> [holos/README.md → The `my-project` delivery scaffold](../README.md#the-my-project-delivery-scaffold))
> — which source per-app rendered-manifests artifacts and whose `targetRevision`
> Kargo owns. The dormant git projection does not change any of these, and they do
> not depend on it.

## Project/Application templates: deferred follow-ups

The collection-driven Project and Application components
([ADR-21](../../docs/adr/archive/ADR-21.md), `Implemented` as of HOL-1358 — see the
[authoring guide](project-and-application-templates.md)) ship the one-line
self-service registration and a single wired delivery path. ADR-21's
"scaffold all envs, wire one delivery path" scope leaves three follow-ups
explicitly deferred:

- **Full `ci → qa → prod` Kargo promotion chain + progressive delivery.** Every
  project derives its `ci-/qa-/prod-<name>` namespace set (topology, RBAC
  boundaries, and Kargo adoption labels), but only the bare-`<name>` delivery path
  is wired today. The cross-environment promotion stages across the three
  namespaces, and the **blue-green progressive-delivery** primitives the
  Application resource set still lacks — an Argo Rollouts `Rollout` with a second
  "color" workload/`Service` and a traffic-switching step (or an equivalent staged
  Kargo `Stage` pipeline) instead of the current single `argocd-update` hard
  cutover (ADR-21, *The Application component*, item 4) — are follow-on work the
  scaffolded namespaces make possible, not yet built.
- **External-secrets store/controller prerequisite.** ADR-21 envisioned an app
  `ExternalSecret` (ADR-21, Application resource 8) for runtime secret material in
  the project namespace, but the Application component **does not emit one today**:
  the platform ships **no** external-secrets installation, no
  `SecretStore`/`ClusterSecretStore`, and no per-namespace enablement, so there is
  nothing for an `ExternalSecret` to resolve against. Standing up the
  external-secrets controller and store, and then adding the `ExternalSecret` to
  the app resource set, is the deferred prerequisite. The namespace registry models
  only namespace metadata (`_ambient`/labels/annotations) today.
- **Self-service `ProjectRequest` API.** A registration is currently a reviewed
  pull request adding a `holos/projects/*.cue` (and optionally `holos/apps/*.cue`)
  entry. The first-class `Project` CRD and a `ProjectRequest` API that generates
  the same rendered resource set on request remain **open** (ADR-1/ADR-21 left this
  deferred deliberately — ADR-21, *Left open*). `my-project` is the reference
  instance and the template for that future generator.

When any of these lands, replace its bullet with a link to the real
documentation.

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
It manages the platform's realm roles, the `authenticated` default group, the
OIDC clients (`argocd`/`quay`/`kargo`), and — as of HOL-1369 — the realm's
`identityProviders[]` (the `esso` OIDC broker) and its custom first-broker-login
auto-link flow, all declaratively, so realm changes land by editing the import
document and re-applying rather than by manual admin-console edits. The
`KeycloakRealmImport` CR still bootstraps the realm shell on a clean cluster (and
owns only the realm's `enabled` flag, declaring no identity providers, so the two
paths own disjoint fields); the `keycloak-config` Job layers managed objects onto
it and keeps them converged. Realm objects the Job does not declare are left
untouched (keycloak-config-cli's default no-delete managed-import behavior;
full-realm purge is deliberately not enabled). The brokering topology — the
second `esso` enterprise-SSO realm and the holos OIDC broker — is documented in
the [esso ↔ holos IdP runbook](../../docs/runbooks/esso-keycloak-idp.md) and
[ADR-20](../../docs/adr/ADR-20.md).

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
admin API), which is deferred. The full role/superuser model and the
client pattern are documented in
[keycloak-clients.md](keycloak-clients.md) (see *The Quay-superuser limitation
(not automatic)*); the operator-facing summary is the **Superusers** bullet in
[`holos/README.md`](../README.md#quay-oidc-sso-and-roles). If a future issue
adds the sync, replace this stub with a link to the real documentation.

## Node-level registry trust for in-cluster pulls

Pushes to `quay.holos.internal` from the host work (the host resolves
`*.holos.internal` and trusts the mkcert root CA), but containerd on the
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
([ADR-9](../../docs/adr/archive/ADR-9.md)/[ADR-10](../../docs/adr/archive/ADR-10.md)).

That pipeline was **retired in HOL-1241**: ADR-9/10/11/14 are now `Deprecated`
and superseded by [ADR-16](../../docs/adr/archive/ADR-16.md), and the
`nats`/`webhook-receiver`/`webhook-subscriber` components, their Go code, and the
`wss://nats.holos.internal` debug endpoint were removed. Deployment is now
driven by Kargo plus the client-side build-and-publish workflow
([`oci-publish-workflow.md`](oci-publish-workflow.md)) — there is no inbound
webhook ingress to authenticate and no in-cluster NATS surface to harden, so both
placeholders no longer apply. If a messaging backbone is reintroduced later, its
authentication posture should be recorded as a fresh placeholder.

## Holos Authenticator: L7 enforcement topology and tenant-policy guard

The `holos-authenticator` component (HOL-1389, [ADR-23](../../docs/adr/ADR-23.md),
finalized in HOL-1390) wires the Holos Authenticator into the platform: the
manager Deployment + RBAC + Service + Backend CRD, the `envoyExtAuthzGrpc`
extension provider in `istiod`'s `MeshConfig`, a `CUSTOM` `AuthorizationPolicy`,
and two example `Backend`s (the discovery-based `example` and the static-JWKS
`remote-cluster-a`). The **in-cluster wiring** is built and documented in
the operator runbook ([`docs/runbooks/holos-authenticator.md`](../../docs/runbooks/holos-authenticator.md));
ADR-23 is `Implemented`. ADR-23 Revision 3 (HOL-1392..HOL-1395) added **KSA /
static-JWKS backends** — an additive `spec.oidc.jwks` validates service-account
ID tokens minted by a remote cluster **offline** against a static JWKS (no OIDC
discovery), then impersonates the SA on the management cluster (`spec.server.url`);
the remote cluster is only the token issuer/JWKS source, with one `Backend` per
remote cluster keyed 1:1 by host. The following enforcement, tuning, and
hardening concerns are deliberately deferred to later phases:

- **Static-JWKS key selection / per-key algorithm enforcement (HOL-1396).** The
  static-JWKS verifier (ADR-23 Rev 3) was built to **parity with the OIDC
  discovery path**: it accepts the global supported-algorithm set with **no
  per-`kid` key selection and no per-key algorithm enforcement**. Adding per-`kid`
  key selection and per-key alg enforcement to **both** the static and discovery
  paths together is tracked in **HOL-1396** and intentionally out of scope for the
  Rev 3 docs/finalize phase.

- **L7 enforcement requires a waypoint.** In ambient mode ztunnel is L4-only, so
  the `CUSTOM` `AuthorizationPolicy` only takes effect once a **waypoint** fronts
  the protected workload. For an **external** API-server `Backend` target, a
  `ServiceEntry` + waypoint fronts it. The example `Backend` points at the
  in-cluster API server and the example policy selects the authenticator's own
  pods; the full waypoint / `ServiceEntry` egress topology is not yet built.

- **Lua filters to reject inbound impersonation headers and split the comma-joined
  groups header (ADR-23 Rev 7, HOL-1416; supersedes Rev 6/HOL-1413).** The
  authorizer emits the mapped groups as a **single comma-joined overwrite/set
  header** under a configurable name (default `X-Impersonate-Groups`,
  `--impersonate-groups-header`) — not per-group `Impersonate-Group` append options,
  which Envoy's ext_authz path silently drops when the request does not already
  carry the header. The required companions are two Envoy **Lua HTTP filters**: a
  **reject** filter (`INSERT_BEFORE` `envoy.filters.http.ext_authz`) refusing any
  client-supplied groups or `Impersonate-*` header, and a **split** filter
  (`INSERT_AFTER` ext_authz) that unpacks the comma list into one `Impersonate-Group`
  per group before egress — the worked `EnvoyFilter`/Lua is in the [runbook's
  *Splitting the comma-joined groups
  header*](../../docs/runbooks/holos-authenticator.md#splitting-the-comma-joined-groups-header).
  Like the `CUSTOM` `AuthorizationPolicy` they only have an effect once a **waypoint**
  fronts the protected route and must target that same waypoint, so they ship with
  the deferred waypoint topology above rather than the in-cluster wiring today. When
  that topology is built, **verify the end-to-end behavior against the deployed
  Envoy/Istio version** — that a multi-group token yields one `Impersonate-Group`
  header **per group** at the API server, and that a client-supplied
  `X-Impersonate-Groups` is rejected — since whether Envoy materializes the groups
  header as a comma-joined value or duplicate entries is version/transport dependent
  and is not provable by the unit tests (which cover only the ext_authz response
  options). The split filter is written to handle both forms; the runtime proof
  closes the gap.

- **Tenant use of the extension provider must be enforced, not just documented.**
  The provider is registered mesh-wide, so a `CUSTOM` `AuthorizationPolicy`
  naming `provider.name: holos-authenticator` could in principle be attached to a
  tenant-controlled waypoint/workload to receive the authorizer's injected
  privileged `Authorization` header. Today this is mitigated by **construction**
  — no waypoint is deployed (the provider is inert), the manager's cache is
  scoped to the `holos-authenticator` namespace (tenant `Backend`s are never
  reconciled), the impersonator credential resolves only from the
  `holos-authenticator` namespace, and that namespace is denied to tenant Argo CD
  projects — but there is **no positive enforcement** preventing a tenant
  `AuthorizationPolicy` from referencing the provider once a waypoint exists. The
  deferred work is to **enforce** it: either restrict `security.istio.io`
  `AuthorizationPolicy` (or this `provider.name`) in the tenant `projects`
  AppProject / via an admission policy, or have the authorizer verify the request
  destination is a platform-owned protected route before returning credentials.
  Until then, keep every protected workload, its `Backend`, and its policy in
  platform-owned namespaces.

- **External-egress waypoint / `ServiceEntry` topology for an external API
  server.** The as-built ships only the in-cluster wiring (the example `Backend`
  points at `https://kubernetes.default.svc`). Fronting an **external** API
  server requires a `ServiceEntry` for the off-cluster endpoint and a waypoint
  that egresses to it, plus the out-of-band impersonator credential for that
  remote cluster. Building the full external-egress topology — and a worked
  external `Backend` example — is deferred.

- **Token-refresh / caching tuning.** The authorizer validates each OIDC token
  on every request against the issuer's JWKS, and resolves the impersonator
  credential per request. JWKS caching, issuer-discovery refresh intervals, and
  credential-Secret caching tuning (TTLs, negative caching, refresh on rotation)
  are left at conservative defaults; performance tuning of these is deferred.

- **`serviceAccountRef` token binding and cache tuning (ADR-23 Rev 4).** The
  `serviceAccountRef` credential source (HOL-1399..HOL-1402) mints the outbound
  impersonator token via TokenRequest **without a `BoundObjectRef`** (matching
  `kubectl create token`), caches it keyed by name + audience + expirationSeconds,
  and rotates it with a fixed margin (the smaller of 5m or 20% of lifetime).
  Tightening these — binding the minted token to a bound object (e.g. the manager
  Pod or a Secret) so it is invalidated on object deletion, and making the cache /
  rotation-margin parameters tunable rather than hard-coded — is deferred. The
  shipped defaults are deliberately conservative and require no configuration.

- **Per-identity impersonate scope is operator-applied, not yet templated
  (ADR-23 Rev 4).** The shipped default `holos-authenticator-impersonator`
  ClusterRole is impersonate-only and bounded to the SA virtual groups
  `system:authenticated`/`system:serviceaccounts` (the ratified narrowing of the
  parent AC's literal `users`/`groups`/`serviceaccounts` request). A real
  `Backend` that impersonates a specific user, a per-namespace
  `system:serviceaccounts:<ns>` group, or a specific ServiceAccount needs an
  **operator-applied per-`Backend`** Role/ClusterRole (the worked example is in
  the runbook). Generating that per-`Backend` impersonation RBAC from the
  `Backend`/`groupMapping` spec — so the scope is declared once and rendered, not
  hand-authored — is a possible future follow-up; today it is intentionally manual
  to keep the privileged grant under explicit operator control.

- **Per-request CEL features beyond the default groups mapping.** The
  `groupMapping.celExpression` maps validated claims to a Kubernetes group list;
  the default is `claims["<groupsClaim>"]` (i.e. `claims["groups"]`). Richer
  per-request CEL — deriving the impersonated username via CEL, emitting
  `Impersonate-Uid`/`Impersonate-Extra-*`, request-attribute-aware mapping, or a
  policy DSL over claims plus request metadata — is out of scope for this phase
  and deferred.

## Production deployment area

The only registered cluster is the local `k3d-holos` development cluster.
Production will be additional clusters registered in
[`platform/platform.cue`](../platform/platform.cue) — the `clusters` struct
already supports this (`clusters: "prod-us-east-1": _`), and every registered
cluster renders its own `deploy/clusters/<cluster>/` tree from the same
components. Establishing the production area means registering the clusters,
deciding any per-cluster parameterization (versions, sizing), and documenting
the promotion flow from local to production.
