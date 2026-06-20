# Holos Project and Application Components (GCP Resource Model, GitOps)

| Metadata | Value                                              |
| -------- | -------------------------------------------------- |
| Date     | 2026-06-17                                         |
| Author   | @jeffmccune                                        |
| Status   | `Proposed`                                         |
| Tags     | holos, components, projects, gitops, multi-tenancy |
| Updates  | ADR-1                                              |

| Revision | Date       | Author      | Info           |
| -------- | ---------- | ----------- | -------------- |
| 1        | 2026-06-17 | @jeffmccune | Initial design |
| 2        | 2026-06-20 | @jeffmccune | HOL-1340: record that a Project's rendered manifests also include the per-Project **`keycloak.holos.run`** resources ([ADR-20](ADR-20.md)) — the nested `roles`/`custodians` `KeycloakGroup` tree, the owner's `KeycloakUser`, and the `KeycloakClient`/client-role group→claim wiring — alongside the Quay `Organization` ([ADR-19](ADR-19.md)); retie the project `ReferenceGrant` to the **`security.holos.run`** grant ([ADR-22](ADR-22.md)) authorizing each Keycloak CR's cross-namespace reference to the central `KeycloakInstance`; record the **AC #2 decision** that the umbrella-Project logical concept needs **no new ADR** (ADR-1 + ADR-21 are its home); add the **end-to-end worked example** (`projects: "my-project": owner: "bob@example.com"` → Keycloak groups → `groups` claim → Quay `syncedTeams`). Resolves ADR-20's "Relationship to Projects/Applications" open question consistently in both ADRs. Design record only. |

## Context and Problem Statement

The Holos PaaS targets a self-service "docker push to deploy" experience
([ADR-18](ADR-18.md)). The deployment half of that experience already exists —
Kargo promotion plus the client-side ORAS publish workflow ([ADR-16](ADR-16.md))
— but **standing up a new project or a new application is still bespoke**. The
[`my-project` delivery scaffold](../../holos/components/my-project/buildplan.cue)
is a single hand-authored component: every Argo CD `AppProject`, OCI-source
`Application`, Kargo `Project`/`ProjectConfig`/`Warehouse`/`Stage`, and the
registry entry in [`holos/namespaces.cue`](../../holos/namespaces.cue) was
written by hand for that one instance. A product engineer cannot stand up their
own project or app by editing one line; they (or an operator) must clone and
adapt the whole scaffold.

What gives a product engineer the one-line self-service experience — submit a
single pull request adding **one entry** to a well-known CUE collection and have
Holos render the full set of resources that compose a Project or an Application?
And, because applications do not exist in isolation, **how is a collection of
Applications unified under a Project** so the Project remains the tenant boundary
([ADR-1](ADR-1.md)) everything attaches to?

This ADR records the decision to design two Holos CUE components — a **Project
component** and an **Application component** — that generalize the single
hand-written `my-project` instance into a rendered-from-collection pattern, and
it records the containment model that unifies apps under projects following the
GCP resource hierarchy. It refines [ADR-1](ADR-1.md), whose `Project` tenant
adopted the GCP Project model but **deferred the Kubernetes mapping**. **No CUE
components are written in this phase — this is a design record only.**

## Context / References

- [ADR-1 — Project Resource](ADR-1.md): the platform tenant is a `Project`,
  adopted directly from the GCP Project resource model — the unit of ownership,
  isolation, access control, quotas, and chargeback. ADR-1 deliberately
  **deferred** the Kubernetes implementation (cluster- vs namespace-scoped,
  the `spec`/`status` schema, and how a `Project` maps onto namespaces). This ADR
  **updates** ADR-1 by recording how the `Project` tenant maps onto Kubernetes
  via a rendered Project component under the GitOps model, and is explicit about
  which ADR-1 deferrals it resolves versus leaves open.
- [ADR-18 — The Holos Controller and the GitOps Rendered-Manifest Delivery
  Model](ADR-18.md): the delivery model these components render into — Holos
  renders CUE collections to manifests, Argo CD syncs them via the ADR-16
  pipeline, and the Holos Controller reconciles the `holos.run` CRDs those
  manifests reference. ADR-18 forward-references this ADR as "the Holos
  Project/Application delivery components."
- [ADR-19 — Quay API Group (`quay.holos.run`): Organization and Repository
  CRDs](ADR-19.md): the Quay `Organization` CR the Project component emits and
  the `Repository` CR the Application component emits. These components are the
  rendered producers of the CRs ADR-19 specifies. The Organization's
  `spec.syncedTeams[]` binds the primitive-role OIDC group claim values
  (`my-project-{owner,editor,viewer}`) to Quay teams **by name** — the Quay end of
  the worked example below.
- [ADR-20 — Keycloak API Group (`keycloak.holos.run`)](ADR-20.md): the per-Project
  identity CRs the Project component emits alongside the Quay `Organization` — the
  nested `projects/<project>/{roles,custodians}/{owner,editor,viewer}`
  `KeycloakGroup` tree, the owner's `KeycloakUser` (pre-create-by-email +
  first-login auto-link), and the `KeycloakClient`/client-role wiring that surfaces
  the `my-project-<role>` values in the OIDC `groups` claim. ADR-20 names these as
  "the identity resources a project's rendered manifests would emit"; this ADR
  records that emission and resolves ADR-20's *Relationship to Projects/Applications*
  open question.
- [ADR-22 — The `security.holos.run` API Group and `ReferenceGrant`](ADR-22.md):
  the holos-owned cross-namespace-reference grant. The Project's per-Project
  Keycloak CRs reference a centrally-managed `KeycloakInstance` in a platform
  namespace; that cross-namespace reference is authorized by a `security.holos.run`
  `ReferenceGrant` placed in the instance's (referent) namespace — what the
  Project's `ReferenceGrant` resource (below) now is.
- [ADR-16 — Kargo-Driven Promotion with a Client-Side CLI Build-and-Publish
  (ORAS) Workflow](ADR-16.md): the promotion pipeline an Application's
  `Warehouse`/`Stage` participate in — a `repo_push` notifies a `Warehouse`,
  which creates `Freight` and triggers a `Stage` promotion that patches the Argo
  CD `Application`'s OCI `targetRevision`.
- [ADR-3 — Authorization via Kubernetes RBAC and Group Membership](ADR-3.md):
  the access-control model the Project's `AppProject` (OIDC-group access) and the
  owner `RoleBinding` key on — access granted per `Project` by group membership.
- [`holos/components/my-project/buildplan.cue`](../../holos/components/my-project/buildplan.cue):
  the current hand-authored reference scaffold the two components generalize.
- [`holos/namespaces.cue`](../../holos/namespaces.cue): the central namespace
  registry (the mandatory `_ambient` field, the `#RegisteredNamespace`
  constraint, the Kargo adoption label and keep-namespace annotation) the Project
  component must integrate with.
- [`holos/docs/component-guidelines.md`](../../holos/docs/component-guidelines.md):
  the component authoring rules — no inline `Namespace` emission, the
  render-then-commit workflow — the design respects.
- [`holos/docs/oci-publish-workflow.md`](../../holos/docs/oci-publish-workflow.md)
  and
  [`holos/docs/argocd-application-source.md`](../../holos/docs/argocd-application-source.md):
  how an Application's `Warehouse`/`Stage`/Argo CD `Application` + the Quay
  repository/webhook tie together in the publish → Freight → promotion → sync
  loop.

## Design

### The GCP containment model, mapped to Kubernetes

[ADR-1](ADR-1.md) adopts the **GCP resource hierarchy**: resources live in a
**Project**, and the Project is the base-level entity that owns them and is the
unit of isolation and access control. This ADR maps that model onto Kubernetes
by making the Project the **Namespace security boundary**: a `Project` named
`my-project` is realized as the Kubernetes Namespace `my-project`, and every
Application that belongs to that Project renders its workload resources **into
that same Namespace**. The Namespace *is* the GCP Project's containment boundary
— RBAC, `ReferenceGrant` scope, and the Argo CD `AppProject` `destinations` all
key on it.

This mirrors the established `my-project` posture, where the Kargo Project
namespace **doubles as the workload namespace**
([`holos/namespaces.cue`](../../holos/namespaces.cue), the `my-project` entry):
there is no separate `kargo-project-*` sibling. The Project component
generalizes exactly that single-namespace shape — one Namespace per Project,
holding both the project-level control resources and every member Application's
workloads.

### Two CUE collections and the one-line registration UX

The platform gains two well-known CUE collections at the `holos/` root, each a
map keyed by a stable name. A product engineer's entire self-service action is a
**single pull request adding one entry** to the appropriate collection.

**Projects** — `holos/projects/*.cue`:

```cue
// holos/projects/my-project.cue
projects: "my-project": owners: "bob@example.com": _
```

The registration field is **`owners`** — a CUE map keyed by the owner's email
(`projects.<name>.owners.<email>`), so a project may name one or several owners.
A single-owner registration like `projects: "my-project": owners: "bob@example.com": _`
is the common case; the worked example below threads exactly that one owner —
`bob@example.com` — end to end. (Where prose refers to "the project `owner`" it
means a member of this `owners` map; the field is plural to admit more than one.)

**Applications** — `holos/apps/*.cue`:

```cue
// holos/apps/my-app.cue
apps: "my-app": project: "my-project"
```

Holos renders each `projects.<name>` entry into the full set of project-level
resources (the Project component) and each `apps.<name>` entry into the full set
of application-level resources (the Application component). The collections are
CUE maps, so a registration is a one-line addition that unifies with the
component's schema; an invalid entry (a malformed name, an app naming a
non-existent project) fails at **render time**, before it can produce a broken
manifest — the same render-time-failure discipline `#RegisteredNamespace`
already enforces for namespaces.

The resource lists below are presented **illustratively** — the CR shapes and
field names trace to the `my-project` scaffold and the ADR-19 CRDs, but the
components themselves are a later phase. This ADR records *which* resources each
entry composes and *how* they fit together, not the CUE that emits them.

### The Project component: project-level resources per `projects.<name>` entry

One Project entry renders the resources below, all anchored on the Project's
Namespace as the security boundary. Two of them — the `AppProject` and the
project-level `Application` — are Argo CD objects that, following the
[`my-project` scaffold](../../holos/components/my-project/buildplan.cue), are
**namespaced into `argocd`** alongside the Argo CD controller (their *destination*
is the Project's Namespace); the per-Project `keycloak.holos.run` CRs
([ADR-20](ADR-20.md), items 9–11) live in the Project's Namespace and reference a
centrally-managed `KeycloakInstance` in a platform namespace; the rest belong to
(or target) the Project's Namespace.

Items 1–8 are the original Kubernetes/Quay/gateway resources; items 9–11 are the
**Keycloak (identity) resources** this revision adds so a Project's rendered
manifests provision the identity half of its primitive-role model
([ADR-20](ADR-20.md)) alongside the Quay half ([ADR-19](ADR-19.md)):

1. **k8s `Namespace`** `my-project` — the Project's containment boundary. The
   Project component does **not** emit this `Namespace` inline (the
   [component guidelines](../../holos/docs/component-guidelines.md)
   no-inline-Namespace guardrail); instead a one-line project registration
   **derives a central [`holos/namespaces.cue`](../../holos/namespaces.cue)
   registry entry** with **ambient mode** enabled (`_ambient: true`, the
   `istio.io/dataplane-mode: ambient` label), and the `namespaces` component
   renders the actual `Namespace` manifest. The Project component references the
   registered name and unifies it with `#RegisteredNamespace`. The Namespace is
   also **wired for external-secrets** so the member Applications'
   `ExternalSecret`s (Application resource 8) resolve — but the registry today
   models only namespace metadata (`_ambient`, labels, annotations), and the
   platform ships no external-secrets installation, `SecretStore`/
   `ClusterSecretStore`, or per-namespace enablement yet. Standing up the
   external-secrets controller and the store the Project's `ExternalSecret`s
   target is therefore a **prerequisite the component phase must add** (an open
   item, not something namespace registration alone provides). Generalizing the
   registry itself from a rendered collection is a further design constraint — see
   *Unifying applications under their project* below.
2. **Kargo `Project`** — the cluster-scoped Kargo `Project` whose name maps to the
   same-named Namespace; that Namespace carries the
   `kargo.akuity.io/project: "true"` adoption label (and the
   `kargo.akuity.io/keep-namespace` annotation) in the registry so the Kargo
   Project controller **adopts** it rather than refusing or deleting it. As in the
   [`my-project` scaffold](../../holos/components/my-project/buildplan.cue), the
   Kargo `Project` brings its namespaced **`ProjectConfig`** (the auto-promotion
   policy and the native Quay webhook **receiver**) and the generate-once
   receiver-token bootstrap `Job` — project-level Kargo wiring shared by every
   Application in the Project (the Applications' `Warehouse`s and `repo_push`
   webhook registrations consume it; see the Application component below).
3. **ArgoCD `AppProject`** — namespaced into `argocd`; scopes what the Project's
   Argo CD `Application`s may deploy, with **access control by OIDC group
   membership** ([ADR-3](ADR-3.md)): `sourceRepos` constrained to the Project's
   Quay org path and `destinations` constrained to the Project's Namespace.
4. **ArgoCD `Application`** — namespaced into `argocd` (its `destination` is the
   Project's Namespace); manages the **project-level** resources (the project's
   own rendered manifests), distinct from the per-Application Argo CD
   `Application`s that member apps render (item 5 of the Application component).
5. **`RoleBinding`** — grants the Project's `owners` access to the Namespace
   (group-membership access control, [ADR-3](ADR-3.md)); the owners list comes
   straight from the one-line `projects.<name>.owners` registration.
6. **Quay `Organization`** — the `quay.holos.run` `Organization` CR from
   [ADR-19](ADR-19.md), naming the Project's Quay org and governing repository
   creation within it. Its `spec.syncedTeams[]` maps the primitive-role OIDC group
   claim values (`my-project-{owner,editor,viewer}`) to Quay teams **by name**
   (owner → team `role: admin`; editor → `member` + `repositoryPermission: write`;
   viewer → `member` + `read`, the ADR-19 worked example), the Quay end of the
   identity flow the Keycloak resources (items 9–11) produce. The Holos Controller
   ([ADR-18](ADR-18.md)) reconciles it.
7. **`ReferenceGrant`(s)** — the grant(s) authorizing the cross-namespace
   references the Project needs. **Two distinct grant kinds apply, and they
   coexist** ([ADR-22](ADR-22.md)):

   - **A `security.holos.run` `ReferenceGrant` for the Keycloak → `KeycloakInstance`
     reference (this revision).** The Project's per-Project Keycloak CRs (items 9–11)
     live in the Project Namespace and reference a centrally-managed
     `KeycloakInstance` ([ADR-20](ADR-20.md)) in a platform namespace (e.g.
     `keycloak`) — a cross-namespace CR-to-CR reference. Per the guard rail
     ([ADR-22](ADR-22.md)), that reference is authorized by a **`security.holos.run`**
     `ReferenceGrant` placed in the **instance's (referent) namespace**, with
     `spec.from[]` naming the Project Namespace's `keycloak.holos.run` Kinds and
     `spec.to[]` naming the local `KeycloakInstance`. This grant is created by the
     platform owner of the `KeycloakInstance` namespace, **not** rendered into the
     Project Namespace by the Project component — the referent-namespace placement
     rule means the grant lives where the `KeycloakInstance` lives. An ungranted
     reference is rejected by the Keycloak CR's reconciler (`Ready=False`, reason
     `RefNotPermitted`), never silently honored. This is the holos-owned grant
     ([ADR-22](ADR-22.md)), distinct from Gateway API's.
   - **A Gateway-API `ReferenceGrant` for cross-namespace *route/backend* object
     references (unchanged from Revision 1).** Two clarifications on that mechanism,
     so this ADR records it correctly:
     - **Route-to-Gateway attachment is not gated by `ReferenceGrant`.** Whether an
       `HTTPRoute` may attach to the shared Gateway is governed by the listener's
       `allowedRoutes` (today `namespaces: from: "All"` in
       [`holos/components/istio-gateway/buildplan.cue`](../../holos/components/istio-gateway/buildplan.cue),
       which the route-attachment placeholder in
       [`holos/docs/placeholders.md`](../../holos/docs/placeholders.md) flags must
       tighten to a label/namespace allow-list before tenant namespaces land). What a
       `ReferenceGrant` *does* authorize is a cross-namespace object reference — an
       `HTTPRoute`'s `backendRefs` pointing at a `Service` in another namespace, or a
       listener's `certificateRefs` pointing at a `Secret` in another namespace (the
       case the Gateway's own certificate comment in `istio-gateway/buildplan.cue`
       calls out).
     - **A `ReferenceGrant` lives in the *referent* (target) namespace — the
       namespace that holds the object being referenced — not in the referrer's
       namespace.** So the grant's namespace depends on the direction of the
       cross-namespace reference, and the two reference kinds differ: an `HTTPRoute`'s
       cross-namespace object reference is its `backendRefs` (e.g. a `Service`), so a
       project route referencing a backend `Service` in `istio-gateways` needs the
       grant **in `istio-gateways`**; a cross-namespace **TLS `Secret`** reference is
       a *Gateway listener* `certificateRefs` concern (not an `HTTPRoute` field), so
       if the shared Gateway in `istio-gateways` referenced a `Secret` in the Project
       Namespace the grant would go **in the Project Namespace**. (The platform's
       current shared Gateway keeps its certificate co-namespaced precisely to avoid
       such a grant — the certificate comment in `istio-gateway/buildplan.cue`.) The
       general rule holds either way: the grant lives where the referenced object
       lives. This item is the per-project grant for whichever such reference the
       Project actually needs, placed in the target namespace accordingly; the
       **attachment** policy remains the listener's `allowedRoutes`, recorded here so
       the two mechanisms are not conflated.

   The same referent-namespace placement rule governs **both** grant kinds — the
   `security.holos.run` grant lives in the `KeycloakInstance`'s namespace, the
   Gateway-API grant lives in the referenced route/backend object's namespace — and
   the two never substitute for each other (a holos CR-to-CR reference is **never**
   authorized by Gateway API's grant, which governs only its fixed route/backend
   kinds; [ADR-22](ADR-22.md)).
8. **`HTTPRoute`** — the Project's route attaching to the shared Gateway (via the
   listener's `allowedRoutes`), exposing the Project's services through the
   platform ingress.
9. **`KeycloakGroup`** (`keycloak.holos.run`, [ADR-20](ADR-20.md)) — the per-Project
   nested role/custodian group tree. Reconciled by the Holos Controller, it creates
   `projects/<project>/roles/{owner,editor,viewer}` (the groups a person is a member
   of to hold a primitive role) and `projects/<project>/custodians/{owner,editor,viewer}`
   (whose members manage the matching `roles/*` group's membership via FGAP-v2
   delegation). Its `clientRoleBindings` names the **consumer client** — the Quay
   client `https://quay.holos.localhost` for the [ADR-19](ADR-19.md) `syncedTeams`
   case — so each `roles/<role>` group carries the `my-project-<role>` client role
   that the consumer's existing `oidc-usermodel-client-role-mapper` emits into the
   `groups` claim. This CR is the **authoritative** owner of the role→claim binding
   ([ADR-20](ADR-20.md), *Claim value via a client role*). It references the central
   `KeycloakInstance` via `instanceRef` (the cross-namespace grant, item 7).
10. **`KeycloakUser`** (`keycloak.holos.run`, [ADR-20](ADR-20.md)) — pre-provisions
    the Project **owner** by email (`projects.<name>.owners`, e.g.
    `bob@example.com`) *only if necessary* and assigns initial group membership in
    `projects/<project>/roles/owner`, so the owner holds the role before first
    login. The realm's first-broker-login flow **auto-links** Bob's federated login
    to this pre-created record (platform realm/IdP config the `KeycloakRealmImport`
    CR owns, **not** the Project component — [ADR-20](ADR-20.md), *KeycloakUser*).
    References the `KeycloakInstance` via `instanceRef`. (Editors/viewers are
    typically added by custodians post-hoc rather than pre-provisioned, so the
    component renders a `KeycloakUser` for the registered owner(s); broader
    membership flows through the custodian groups.)
11. **`KeycloakClient`** (`keycloak.holos.run`, [ADR-20](ADR-20.md)) — *only when the
    Project runs its own OIDC service whose token must carry the project roles*: a
    per-project OIDC client named by its URL, with `emitProjectRolesInGroupsClaim`
    wiring the per-client role mapper so the `my-project-<role>` value surfaces in
    **that** client's token. **The ADR-19 Quay use case needs no project
    `KeycloakClient`** — the consumer is the platform Quay client, whose mapper
    already exists — so this resource is conditional, rendered per the project's
    declared services. The role→consumer-client binding stays on the `KeycloakGroup`
    (item 9) regardless. References the `KeycloakInstance` via `instanceRef`.

### The Application component: 11 resources per `apps.<name>` entry

One Application entry renders these **11** resources. The Kargo `Warehouse` and
`Stage` are namespaced into the Project's Kargo Project namespace (the Project's
Namespace, per the single-namespace shape); the Argo CD `Application` is
namespaced into `argocd` (its `destination` is the Project's Namespace,
following the [`my-project` scaffold](../../holos/components/my-project/buildplan.cue));
the `Deployment`/`Service`/`ExternalSecret`/`ConfigMap`/`ServiceAccount`/
`RoleBinding` workload objects are rendered **into the Namespace of the Project
named by `apps.<name>.project`**. The Kargo control plane these resources plug
into — the cluster-scoped Kargo `Project`, the namespaced `ProjectConfig`
carrying the auto-promotion policy and the native Quay webhook **receiver**, and
the receiver-token bootstrap `Job` — is supplied by the **Project** component
(it is project-level, shared by every app in the Project), not re-emitted per
app; the Application's `Warehouse` and the `Repository`'s `repo_push` webhook
registration both depend on it (ADR-19's `Repository.pushWebhook` reads the
receiver URL from `ProjectConfig.status`):

1. **Quay `Repository` CR** — the `quay.holos.run` `Repository` from
   [ADR-19](ADR-19.md), the app's rendered-manifests repository within the
   Project's Quay org.
2. **Kargo `Warehouse` CR** — namespaced into the Project's Kargo Project
   namespace; linked to the Quay `Repository`, watching the OCI artifact and
   creating `Freight`, driven by the `repo_push` **webhook notification** the
   `Repository` registers against the Project's `ProjectConfig` receiver (with the
   polling interval as the fallback) ([ADR-16](ADR-16.md)).
3. **Kargo `Stage` CR** — the promotion target whose `argocd-update` step repoints
   the app's Argo CD `Application` at each promoted `Freight` digest.
4. **Kargo blue-green progressive-delivery pipeline** — the **intended**
   progressive-delivery behavior expressed on the Kargo `Stage`'s
   promotion-template (configuration, not a separate Kubernetes object). Recording
   the gap honestly: the `my-project` scaffold's `Stage` runs a single
   `argocd-update` step, which is a **hard cutover** (Argo CD syncs the new digest
   into one `Deployment`) — true blue-green needs primitives this resource set does
   not yet include (an Argo Rollouts `Rollout` with a second "color" workload/
   `Service` and a traffic-switching step, or an equivalent staged Stage
   pipeline). This item names the progressive-delivery capability the Application
   component **should** render and flags those additional primitives as design
   work, rather than claiming a plain `Deployment` + `argocd-update` already
   achieves blue-green.
5. **ArgoCD `Application` CR** — namespaced into `argocd`; the OCI-source
   `Application` Kargo patches (`targetRevision` owned by Kargo, omitted from the
   committed manifest — the "imperative revision, declarative Application" posture
   the `my-project` scaffold establishes), its `destination` the Project's
   Namespace.
6. **`Deployment`** — the application workload.
7. **`Service`** — fronts the `Deployment`.
8. **`ExternalSecret`** — the app's runtime secret material, resolved by
   external-secrets in the Project Namespace (the namespace's external-secrets
   enablement from the Project component, item 1).
9. **`ConfigMap`** — the app's non-secret configuration.
10. **`ServiceAccount`** — the workload identity the `Deployment` runs as.
11. **`RoleBinding`** — grants the identity managing the application the rights it
    needs within the Project Namespace.

### Unifying applications under their project

The through-line is **containment**: an Application is contained by exactly one
Project, following the GCP model where every resource lives in a Project and the
Project is the security boundary (≈ a Kubernetes Namespace).

- **The binding field.** `apps.<name>.project` names the Project an Application
  belongs to. In CUE this is a reference that must resolve to a key in the
  `projects` collection; an app naming a non-existent project is a render-time
  failure, not a runtime `NotFound`. This is the same render-time-validation
  discipline `#RegisteredNamespace` applies to namespaces — drift between the two
  collections becomes a render error.
- **Where an app's resources land.** Because the Project is realized as its
  Namespace, the Application component's **workload** objects (the
  `Deployment`/`Service`/`ExternalSecret`/`ConfigMap`/`ServiceAccount`/`RoleBinding`)
  and its Kargo `Warehouse`/`Stage` are rendered **into the Project's Namespace**
  (resolved from `apps.<name>.project`). The two Argo CD-managed objects sit
  outside it for the same reason the `my-project` scaffold places them there: the
  app's Argo CD `Application` is namespaced into `argocd` (alongside the
  controller) with its `destination` pointing **back at** the Project's Namespace.
  The Project's `AppProject` (`destinations`) and owner `RoleBinding` already
  scope that Namespace; the app's workloads inherit the Project's boundary rather
  than declaring their own. The Application's Quay `Repository` is created **within
  the Project's Quay `Organization`** (its `organizationRef` resolves to the
  Project's org), so the registry containment mirrors the Kubernetes containment.
- **How the two collections mix at render time.** The collections live at the
  `holos/` root (the same level as [`holos/namespaces.cue`](../../holos/namespaces.cue),
  which *is* a CUE ancestor of every component instance and is how the existing
  `#RegisteredNamespace` cross-reference works). Sibling subdirectories like
  `holos/projects/` and `holos/apps/` are **not** automatically ancestors of a
  `holos/components/<name>/` build plan, so the design must wire the two
  collections into the Project and Application components **explicitly** — by
  defining `projects`/`apps` in a root-level CUE file (an ancestor, like
  `namespaces.cue`) or by having the components import the collection package.
  That wiring is the design's responsibility; this ADR records the *intent* (one
  `holos render platform` evaluates both collections together and resolves their
  cross-references as ordinary CUE unification), and names the ancestor/import
  wiring as the mechanism the component phase must establish — it is not free from
  directory layout alone. Given that wiring: each `projects.<name>` renders the
  project-level resources (the eight Kubernetes/Quay/gateway resources plus the
  per-Project `keycloak.holos.run` CRs), each
  `apps.<name>` renders the 11 application-level resources scoped to its project
  (workloads into the project's namespace, the Argo CD `Application` into `argocd`
  with that namespace as its destination), and the cross-references
  (`apps.<name>.project` → a `projects` key,
  the `Repository.organizationRef` → the project's `Organization`) are resolved as
  ordinary CUE unification. Adding an app is one line in `holos/apps/`; adding a
  project is one line in `holos/projects/`; the renderer composes them into the
  complete, validated manifest set.
- **Reference scaffold and the registry guardrail.** The components generalize the
  hand-authored
  [`holos/components/my-project/buildplan.cue`](../../holos/components/my-project/buildplan.cue)
  — the `my-project` instance is what one `projects.<name>` (plus one
  `apps.<name>`) entry must reproduce. The Project component **must integrate with
  the central [`holos/namespaces.cue`](../../holos/namespaces.cue) registry**
  rather than emitting a `Namespace` inline: per the
  [component guidelines](../../holos/docs/component-guidelines.md), namespaces are
  always registered centrally (with their `_ambient` position and any Kargo
  adoption metadata) and referenced by name. Generalizing a per-project Namespace
  into the registry from a rendered collection — so a one-line project
  registration produces a correctly-labeled, ambient-enrolled registry entry — is
  itself a design question the Project component must solve, and is called out
  here as a known constraint rather than resolved in this design record.

### Refining ADR-1

[ADR-1](ADR-1.md) established the `Project` as the GCP-model tenant and
**deferred** its Kubernetes mapping. This ADR records the mapping under the
GitOps rendered-manifest model and is explicit about the boundary:

**Resolved by this ADR (the mapping under GitOps):**

- **How a `Project` maps onto Kubernetes constructs.** A `Project` is realized as
  a **Namespace** that acts as its security boundary, plus the project-level
  control resources the Project component renders (the Kargo `Project`, the Argo
  CD `AppProject`/`Application`, the owner `RoleBinding`, the Quay `Organization`,
  the `ReferenceGrant`/`HTTPRoute`). The GCP "resources live in a Project"
  containment becomes "resources live in the Project's Namespace."
- **Isolation and access control per Project.** Isolation is the Namespace
  boundary; access control is the `AppProject` OIDC-group binding plus the owner
  `RoleBinding` ([ADR-3](ADR-3.md)) — access granted per Project, as ADR-1
  requires. The Project's **GCP primitive roles** (`owner`/`editor`/`viewer`,
  [ADR-1](ADR-1.md)) are realized in the identity system by the per-Project
  `keycloak.holos.run` resources this revision adds (items 9–11): the
  `projects/<project>/roles/*` Keycloak groups whose membership flows via the OIDC
  `groups` claim into the `AppProject` binding, the Quay teams ([ADR-19](ADR-19.md)
  `syncedTeams`), and any project-service RBAC. ADR-1's access-control decision is
  unchanged; this ADR supplies its rendered realization.
- **How the tenant is delivered.** Through the GitOps rendered-manifest model
  ([ADR-18](ADR-18.md)): a one-line collection entry renders the Project's
  resources, Argo CD syncs them, and the Holos Controller reconciles the
  `holos.run` CRDs (the Quay `Organization`) they reference.

**Left open (deferred, consistent with ADR-1):**

- **Whether a first-class `Project` CRD exists**, cluster- or namespace-scoped,
  with the ADR-1 immutable-ID-vs-display-name `spec`/`status` and lifecycle
  states. This ADR maps the *tenant* onto a rendered set of resources keyed by the
  `projects` collection; it does **not** introduce a `Project` custom resource or
  resolve ADR-1's scope/schema question. A self-service `ProjectRequest` API that
  generates the same resources is a natural evolution but is not decided here.
- **The GCP levels above a Project** (folders, organization) remain unadopted, as
  in ADR-1.
- **Quotas, limits, and chargeback per Project** ([ADR-5](ADR-5.md)) are
  unchanged by this ADR; the Project Namespace is where they would attach, but
  their enforcement is out of scope.

### Decision: no new umbrella-Project ADR — ADR-1 + ADR-21 are its home

A natural question this revision raises is whether the **overall umbrella
"logical Project" concept** — the thing a one-line registration names, that now
ties together a Namespace, a Kargo Project, an Argo CD AppProject/Application, a
Quay `Organization`, and the per-Project `keycloak.holos.run` resources — warrants
a **new, standalone ADR** of its own. **It does not.** The umbrella Project concept
already has its ADR home, split cleanly across two existing ADRs that this phase
**revises and cross-references** rather than supplanting:

- **[ADR-1](ADR-1.md)** owns the **tenant model** — the Project *is* the GCP-model
  tenant, the unit of ownership/isolation/access/quotas/chargeback. Its Revision 3
  cross-references how the primitive roles are realized in the identity system.
- **[ADR-21](ADR-21.md)** (this ADR) owns the **GitOps/Kubernetes realization** —
  the Project ≈ Namespace mapping and the full rendered resource set (now including
  the Keycloak resources), driven by the `holos/projects/*.cue` collection.

Per [writing-adrs.md](writing-adrs.md) ("prefer revising the existing ADR over
writing a new one for a minor decision"), creating a third ADR to restate what
these two already cover would fragment the record and create drift. The decision
recorded here is therefore explicit: **no new umbrella-Project ADR is created;
ADR-1 and ADR-21 are revised and cross-referenced** (with [ADR-19](ADR-19.md) for
the Quay half, [ADR-20](ADR-20.md) for the Keycloak half, and [ADR-3](ADR-3.md) for
the authorization model) to keep the umbrella Project concept coherent in one place.
This also resolves [ADR-20](ADR-20.md)'s open question *Relationship to
Projects/Applications (ADR-21)* — its per-Project Keycloak CRs are emitted by **this**
Project component, exactly as it emits the Quay `Organization`.

### End-to-end worked example: from CUE registration to Quay teams

This single example threads one registration —
`projects: "my-project": owner: "bob@example.com"` — all the way from the one-line
CUE entry through the Keycloak groups and the OIDC `groups` claim into the Quay
`syncedTeams`. It is the canonical illustration that the Quay half
([ADR-19](ADR-19.md)) and the Keycloak half ([ADR-20](ADR-20.md)) meet at the
project-prefixed claim-name strings. (The Keycloak-CR field shapes are
[ADR-20](ADR-20.md)'s; the Quay-team semantics are [ADR-19](ADR-19.md)'s — this
example only joins them.)

**1. Registration (one line).** A product engineer opens a pull request adding one
entry to the projects collection:

```cue
// holos/projects/my-project.cue
projects: "my-project": owners: "bob@example.com": _
```

**2. Keycloak role + custodian groups (`KeycloakGroup`).** Rendering the Project
component emits a `KeycloakGroup` ([ADR-20](ADR-20.md)) that the Holos Controller
reconciles into the shallow nested tree:

```text
projects/my-project/roles/{owner,editor,viewer}
projects/my-project/custodians/{owner,editor,viewer}
```

Its `clientRoleBindings` names the **Quay client** `https://quay.holos.localhost`
as the consumer (the [ADR-19](ADR-19.md) `syncedTeams` case needs **no** project
`KeycloakClient`), so the controller assigns a `my-project-<role>` **client role on
the Quay client** to each `roles/<role>` group.

**3. Owner pre-provisioned + auto-linked (`KeycloakUser`).** The component emits a
`KeycloakUser` for the registered owner `bob@example.com` that the controller
pre-creates by email (only if necessary) and adds to
`projects/my-project/roles/owner`. When Bob first signs in through the federated
IdP, the realm's first-broker-login flow (`Detect Existing Broker User` +
`Automatically Set Existing User` + `Trust Email`, platform realm/IdP config the
`KeycloakRealmImport` CR owns — [ADR-20](ADR-20.md)) **auto-links** his login to
this pre-created record instead of creating a duplicate.

**4. Role-group → client-role → `groups` claim.** Bob is a member of
`projects/my-project/roles/owner`, which carries the `my-project-owner` **Quay
client role**. The Quay client's existing `oidc-usermodel-client-role-mapper`
(`quay-client-roles`,
[realm-config buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue))
emits that client role into Bob's `groups` claim as the flat value
**`my-project-owner`** — with **no Quay-side or new-mapper change**
([ADR-20](ADR-20.md), *Claim value via a client role*). (The bare-leaf `owner`
value the group-membership mapper also emits is an accepted, ignored byproduct —
nothing keys on it; [ADR-20](ADR-20.md).) Editor and viewer members get
`my-project-editor` / `my-project-viewer` the same way.

**5. `groups` claim → Quay `syncedTeams`.** The Project's Quay `Organization`
([ADR-19](ADR-19.md)) maps those exact claim values to Quay teams **by name** —
matching the ADR-19 worked example exactly (owner → org `role: admin`; editor →
`member` + `repositoryPermission: write`; viewer → `member` + `read`):

```yaml
apiVersion: quay.holos.run/v1alpha1
kind: Organization
metadata:
  name: my-project
spec:
  syncedTeams:
    - name: my-project-owner       # the Quay team name
      oidcGroup: my-project-owner  # the groups-claim value, by name only
      role: admin                  # org admin: governance + full repo access
    - name: my-project-editor
      oidcGroup: my-project-editor
      role: member
      repositoryPermission: write  # push/pull repos in the org, no org admin
    - name: my-project-viewer
      oidcGroup: my-project-viewer
      role: member
      repositoryPermission: read   # pull repos in the org, read-only
```

With `FEATURE_TEAM_SYNCING` on, Quay syncs Bob (carrying `my-project-owner` in his
`groups` claim) into the `my-project-owner` team, which has the org `admin` role —
so Bob administers the `my-project` Quay org and its repositories. The chain is
complete: **one line of CUE → Keycloak groups → client role → `groups` claim →
Quay team membership and role**, with the Quay end identical to
[ADR-19](ADR-19.md)'s `admin`/`member`(+`write`/`read`) semantics. (`creator` is the
third available Quay team `role` in ADR-19's enum; this primitive-role example uses
`admin`/`member`, reserving `creator` for projects that want a repo-creating team
without full org admin.)

## Decision

1. **The platform gains two Holos CUE components** — a **Project component** and
   an **Application component** — driven by two well-known root CUE collections,
   `holos/projects/*.cue` and `holos/apps/*.cue`. A product engineer's
   self-service action is a **single pull-request entry**: `projects: "my-project":
   owners: …` or `apps: "my-app": project: "my-project"`.
2. **One `projects.<name>` entry renders the project-level resources** — the
   original eight Kubernetes/Quay/gateway resources **plus** the per-Project
   `keycloak.holos.run` identity resources ([ADR-20](ADR-20.md)): the
   registry-integrated k8s `Namespace` (ambient-enrolled, and wired for
   external-secrets — the store/controller wiring an open prerequisite), the Kargo
   `Project` (adopted via the namespace label), the Argo CD `AppProject` (OIDC-group
   access), the project-level Argo CD `Application`, the owner `RoleBinding`, the
   Quay `Organization` ([ADR-19](ADR-19.md), its `syncedTeams[]` binding the
   primitive-role claim values to Quay teams by name), the `KeycloakGroup`
   (the `projects/<project>/{roles,custodians}/{owner,editor,viewer}` tree + the
   `my-project-<role>` client-role group→claim binding), the owner's `KeycloakUser`
   (pre-create-by-email + first-login auto-link), the **conditional** `KeycloakClient`
   (only when the Project runs its own OIDC service — the ADR-19 Quay consumer needs
   none), **two `ReferenceGrant` kinds** (a **`security.holos.run`** grant
   ([ADR-22](ADR-22.md)) in the `KeycloakInstance`'s namespace authorizing the
   Keycloak CRs' cross-namespace `instanceRef`, plus the Gateway-API grant placed in
   the target namespace of whichever cross-namespace route/backend object reference
   the Project needs — e.g. `istio-gateways` when a project route's `backendRefs`
   point at a `Service` there; not the route-attachment mechanism, which is the
   listener's `allowedRoutes`), and the `HTTPRoute`.
3. **One `apps.<name>` entry renders 11 application-level resources** scoped to
   its Project (workloads into the Project's Namespace; the Kargo `Warehouse`/
   `Stage` into the Project's Kargo Project namespace; the Argo CD `Application`
   into `argocd` with the Project's Namespace as its `destination`): the Quay
   `Repository` ([ADR-19](ADR-19.md)), the Kargo `Warehouse` (linked to the
   Repository, driven by the `repo_push` webhook into the Project's `ProjectConfig`
   receiver), the Kargo `Stage`, the Kargo blue-green progressive-delivery pipeline
   (the `Stage`'s promotion-template configuration), the Argo CD `Application`, the
   `Deployment`, `Service`, `ExternalSecret`, `ConfigMap`, `ServiceAccount`, and
   `RoleBinding`. The shared Kargo `Project`/`ProjectConfig`/receiver-token
   wiring is the **Project** component's, not re-emitted per app.
4. **Applications are unified under their Project by GCP-model containment:**
   `apps.<name>.project` binds an app to a Project, the Project is realized as its
   Namespace security boundary (the `my-project` single-namespace shape, where the
   Kargo Project namespace doubles as the workload namespace), and the app's
   workload resources render into that Namespace within the Project's Quay org
   (the app's Argo CD `Application` is namespaced into `argocd` with that Namespace
   as its `destination`, per the scaffold). The two collections mix at render time
   as ordinary CUE unification **once wired as build-plan ancestors or imported by
   the components** — a mechanism the component phase must establish, since sibling
   `holos/` subdirectories are not automatically ancestors of a component build
   plan. The Project component **integrates with the central
   [`holos/namespaces.cue`](../../holos/namespaces.cue) registry and never emits a
   `Namespace` inline** (the [component guidelines](../../holos/docs/component-guidelines.md)).
5. **A Project's rendered manifests include its per-Project `keycloak.holos.run`
   resources** ([ADR-20](ADR-20.md)): the `KeycloakGroup` (the nested
   `roles`/`custodians` tree + the `my-project-<role>` client-role group→claim
   binding on the consumer client), the owner's `KeycloakUser`, and a **conditional**
   `KeycloakClient` (only for a Project running its own OIDC service — the
   [ADR-19](ADR-19.md) Quay consumer needs none). Each Keycloak CR references a
   centrally-managed `KeycloakInstance` via `instanceRef`; that cross-namespace
   reference is authorized by a **`security.holos.run`** `ReferenceGrant`
   ([ADR-22](ADR-22.md)) in the instance's namespace — distinct from, and coexisting
   with, the Gateway-API `ReferenceGrant` for route/backend references. This
   **resolves [ADR-20](ADR-20.md)'s open question** *Relationship to
   Projects/Applications (ADR-21)*: the Project component emits these CRs.
6. **No new umbrella-Project ADR is created (AC #2).** The overall logical Project
   concept already has its ADR home: **[ADR-1](ADR-1.md)** (the tenant model) and
   **[ADR-21](ADR-21.md)** (its GitOps/Kubernetes realization). Per
   [writing-adrs.md](writing-adrs.md), this phase **revises and cross-references**
   those two (with [ADR-19](ADR-19.md)/[ADR-20](ADR-20.md)/[ADR-3](ADR-3.md)) rather
   than fragmenting the record into a third ADR.
7. **The end-to-end worked example is recorded here** (*End-to-end worked example:
   from CUE registration to Quay teams*): `projects: "my-project": owner:
   "bob@example.com"` → `KeycloakGroup`s `projects/my-project/{roles,custodians}/*`
   → `KeycloakUser` for Bob in `roles/owner` (first-login auto-link) → the
   `my-project-owner` Quay **client role** → the `groups`-claim value
   `my-project-owner` → the Quay `Organization.spec.syncedTeams[]` mapping
   (owner → `admin`; editor → `member`+`write`; viewer → `member`+`read`), internally
   consistent with [ADR-19](ADR-19.md)'s `admin`/`creator`/`member` team-role
   semantics.
8. **This refines [ADR-1](ADR-1.md):** the `Project` tenant maps onto Kubernetes
   via this Project component under the GitOps rendered-manifest model. It
   **resolves** ADR-1's namespace-mapping, isolation, and access-control deferrals
   (the access-control half now including the identity realization of the primitive
   roles via [ADR-20](ADR-20.md)) and **leaves open** whether a first-class `Project`
   CRD (with ADR-1's scope, schema, and lifecycle) exists, the GCP
   folder/organization levels, and quota enforcement.
9. **This is a design record only — no CUE components are written in this phase.**

## Consequences

- **Self-service collapses to one line.** Standing up a project or an app becomes
  a single reviewable pull-request entry instead of cloning and adapting the
  `my-project` scaffold; the renderer composes and validates the full resource set.
  Cross-collection mistakes (an app naming a missing project) fail at render time,
  not at apply time.
- **The Project is unambiguously the Namespace boundary.** Adopting the GCP
  containment model as "Project ≈ Namespace" means an Application has no isolation
  of its own — it inherits the Project's Namespace, `AppProject` destinations, and
  RBAC. This keeps the tenant boundary single and legible (ADR-1) but means
  finer-than-Project isolation between two apps in one Project is not modeled.
- **A new central-registry integration burden.** The Project component must feed
  the [`holos/namespaces.cue`](../../holos/namespaces.cue) registry from a rendered
  collection while honoring the no-inline-Namespace guardrail and the mandatory
  `_ambient` position — generalizing a per-instance registry entry into a
  collection-driven one is the design's hardest constraint, called out here for the
  component phase to solve.
- **Depends on the controller and the Quay CRDs.** The Project's `Organization`
  and the Application's `Repository` are reconciled by the shipped Holos Controller
  ([ADR-18](ADR-18.md)) against the `quay.holos.run` group ([ADR-19](ADR-19.md)).
  The controller and its Quay CRDs have shipped, so an emitted `Organization` (as
  the `my-project` component already does) converges today; the parts these
  components would add but that no `quay.holos.run` CR yet covers — the robots and
  the Argo CD/Kargo pull-credential Secrets — stay in the manual-stop-gap state
  ADR-19 describes.
- **The identity half is designed but not yet reconciled.** The per-Project
  `keycloak.holos.run` resources this revision adds are reconciled by the **same**
  Holos Controller as a second API group ([ADR-20](ADR-20.md)), but that group is
  `Proposed` — its CRDs and reconcilers are future implementation work
  (HOL-1344). So a Project's Keycloak resources are part of the **designed**
  rendered set, while today the `my-project` scaffold's OIDC groups are still
  provisioned by hand and `syncedTeams` references them by name ([ADR-19](ADR-19.md)).
  The worked example shows what converges **once** the Keycloak group ships; until
  then the Quay half works against hand-provisioned groups, exactly as ADR-19
  describes. Crucially, because the claim value is carried by a client role on the
  **Quay client**, that day requires **no Quay-side change** — the existing
  `quay-client-roles` mapper already emits it.
- **ADR-1 is partially resolved, not closed.** This ADR answers ADR-1's
  namespace-mapping and access-control deferrals but intentionally leaves the
  first-class `Project` CRD question open, so ADR-1 remains a living record that a
  future `Project`/`ProjectRequest` ADR may refine further.
