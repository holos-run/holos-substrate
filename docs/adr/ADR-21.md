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
  rendered producers of the CRs ADR-19 specifies.
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
projects: "my-project": owners: "jeff@openinfrastructure.co": _
```

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

### The Project component: 8 resources per `projects.<name>` entry

One Project entry renders these **8** resources, all anchored on the Project's
Namespace as the security boundary. Two of them — the `AppProject` and the
project-level `Application` — are Argo CD objects that, following the
[`my-project` scaffold](../../holos/components/my-project/buildplan.cue), are
**namespaced into `argocd`** alongside the Argo CD controller (their *destination*
is the Project's Namespace); the rest belong to (or target) the Project's
Namespace:

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
   [ADR-19](ADR-19.md), naming the Project's Quay org, mapping its OIDC groups to
   Quay teams/roles, and governing repository creation within it. The Holos
   Controller ([ADR-18](ADR-18.md)) reconciles it.
7. **`ReferenceGrant`** — the Gateway-API grant authorizing the cross-namespace
   **object** references the Project needs. Two clarifications on the mechanism, so
   this ADR records it correctly:
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
     cross-namespace reference: if a project `HTTPRoute` (in the Project Namespace)
     references a backend `Service` or TLS `Secret` that lives in `istio-gateways`,
     the grant goes **in `istio-gateways`** (the issue's "in the Istio gateway
     namespace" case); if instead the Gateway/Istio in `istio-gateways` references
     an object in the Project Namespace, the grant goes **in the Project
     Namespace**. This item is the per-project grant for whichever such reference
     the Project actually needs, placed in the target namespace accordingly; the
     **attachment** policy remains the listener's `allowedRoutes`, recorded here so
     the two mechanisms are not conflated.
8. **`HTTPRoute`** — the Project's route attaching to the shared Gateway (via the
   listener's `allowedRoutes`), exposing the Project's services through the
   platform ingress.

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
  directory layout alone. Given that wiring: each `projects.<name>` renders the 8
  project-level resources, each
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
  requires.
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

## Decision

1. **The platform gains two Holos CUE components** — a **Project component** and
   an **Application component** — driven by two well-known root CUE collections,
   `holos/projects/*.cue` and `holos/apps/*.cue`. A product engineer's
   self-service action is a **single pull-request entry**: `projects: "my-project":
   owners: …` or `apps: "my-app": project: "my-project"`.
2. **One `projects.<name>` entry renders 8 project-level resources:** the
   registry-integrated k8s `Namespace` (ambient-enrolled, and wired for
   external-secrets — the store/controller wiring an open prerequisite), the Kargo
   `Project` (adopted via the namespace label), the Argo CD `AppProject` (OIDC-group
   access), the project-level Argo CD `Application`, the owner `RoleBinding`, the
   Quay `Organization` ([ADR-19](ADR-19.md)), the `ReferenceGrant` (placed in the
   target namespace of whichever cross-namespace object reference the Project needs
   — `istio-gateways` when the project route references a Service/Secret there; not
   the route-attachment mechanism, which is the listener's `allowedRoutes`), and
   the `HTTPRoute`.
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
5. **This refines [ADR-1](ADR-1.md):** the `Project` tenant maps onto Kubernetes
   via this Project component under the GitOps rendered-manifest model. It
   **resolves** ADR-1's namespace-mapping, isolation, and access-control deferrals
   and **leaves open** whether a first-class `Project` CRD (with ADR-1's scope,
   schema, and lifecycle) exists, the GCP folder/organization levels, and quota
   enforcement.
6. **This is a design record only — no CUE components are written in this phase.**

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
  and the Application's `Repository` are reconciled by the Holos Controller
  ([ADR-18](ADR-18.md)) against the `quay.holos.run` group ([ADR-19](ADR-19.md));
  until the controller ships, those CRs have no reconciler and a project's registry
  data plane stays in the manual-stop-gap state ADR-19 describes. The rendered
  manifests are correct ahead of the controller; they simply do not converge until
  it exists.
- **ADR-1 is partially resolved, not closed.** This ADR answers ADR-1's
  namespace-mapping and access-control deferrals but intentionally leaves the
  first-class `Project` CRD question open, so ADR-1 remains a living record that a
  future `Project`/`ProjectRequest` ADR may refine further.
