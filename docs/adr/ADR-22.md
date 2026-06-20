# The `security.holos.run` API Group and the `ReferenceGrant` Cross-Namespace Reference Convention

| Metadata | Value                              |
| -------- | ---------------------------------- |
| Date     | 2026-06-20                         |
| Author   | @jeffmccune                        |
| Status   | `Proposed`                         |
| Tags     | api, controller, security, references |

| Revision | Date       | Author      | Info           |
| -------- | ---------- | ----------- | -------------- |
| 1        | 2026-06-20 | @jeffmccune | Initial design |

## Context and Problem Statement

As the platform grows beyond the `quay.holos.run` group ([ADR-19](ADR-19.md))
into the Keycloak group ([ADR-20](ADR-20.md)) and the logical Project/Application
model ([ADR-21](ADR-21.md)), `holos.run` custom resources increasingly need to
**reference one another across namespace boundaries**. A `keycloak.holos.run`
`User`, `Group`, or `Client` in a project namespace must name the
`KeycloakInstance` that owns its realm — and that instance lives in a platform
namespace the project does not own. Left unconstrained, any namespace could
reference any object in any other namespace, which is precisely the
confused-deputy / silent-cross-tenant-access hazard the Kubernetes Gateway API
solved for routes and backends with its **`ReferenceGrant`**.

How should every `holos.run` custom resource that needs a cross-namespace
reference obtain one **safely** — so the namespace that *owns* the referenced
object explicitly grants access, rather than the referrer helping itself? And
how is that single convention fixed **once**, for all current and future API
groups, instead of being re-litigated per CRD?

This ADR records the decision to mint a **new `security.holos.run` API group**
whose **`ReferenceGrant`** Kind is the standard, Kubernetes-native, Gateway-API-
style mechanism authorizing cross-namespace references between `holos.run`
custom resources. It fixes the *convention*; the field-level API and the
reconciler land in later CRD-implementation issues.

## Context / References / Prior Work

- **Kubernetes Gateway API `ReferenceGrant`**
  (`gateway.networking.k8s.io/v1beta1`): the prior art. A `ReferenceGrant` is a
  **namespaced** object that lives in the **referent (target) namespace** — the
  namespace holding the object being referenced — and declares a `spec.from[]`
  (the group/kind/namespace of the referrers it authorizes) and a `spec.to[]`
  (the group/kind, optionally `name`, of the *local* objects that may be
  referenced). A cross-namespace reference with **no matching grant is denied**.
  This is the From/To shape this ADR mirrors.
- [ADR-21 — Holos Project and Application Components](ADR-21.md): already
  contains the authoritative discussion of Gateway-API `ReferenceGrant`
  semantics — **a grant lives in the *referent* namespace** and authorizes
  cross-namespace **object references** (an `HTTPRoute`'s `backendRefs`, a
  listener's `certificateRefs`), **not** route-to-Gateway attachment (which is
  the listener's `allowedRoutes`). ADR-21 enumerates a `ReferenceGrant` as one
  of the 8 project-level resources the Project component renders, "placed in the
  target namespace of whichever cross-namespace object reference the Project
  needs." This ADR aligns with and **generalizes** that mechanism from the
  Gateway/Route case to arbitrary `holos.run` CR-to-CR references; it does not
  contradict ADR-21.
- [ADR-18 — The Holos Controller](ADR-18.md): the `holos-controller` binary that
  reconciles `holos.run` CRDs. A future `security.holos.run` reconciler (or the
  grant-checking logic each referrer's reconciler runs) is owned by this same
  controller.
- [ADR-19 — Quay API Group](ADR-19.md): the first shipped `holos.run` group and
  the precedent for the Gateway-API status-condition model
  (`Accepted`/`Programmed`/`Ready`, `observedGeneration`, `+listType=map` /
  `+listMapKey=type`) a denied cross-namespace reference surfaces through.
- [ADR-20 — The Keycloak API Group](ADR-20.md): the concrete motivating case — a
  `keycloak.holos.run` `User`/`Group`/`Client` in a project namespace
  referencing a `KeycloakInstance` in a platform namespace.
- [ADR-2 — Core Platform Principles](ADR-2.md): every platform capability is
  modeled as Kubernetes resources; `ReferenceGrant` is the Kubernetes-native
  expression of cross-namespace reference policy, consistent with that principle.

## Design

### A new `security.holos.run` API group

`security.holos.run` is a **new API group** owned by the platform's security and
safety conventions, reconciled (in the future) by the same `holos-controller`
binary ([ADR-18](ADR-18.md)). Its first and defining Kind is **`ReferenceGrant`**.

Crucially, `ReferenceGrant` takes **no dependency on any external system** — it
is **pure Kubernetes-native policy**. Unlike `quay.holos.run` (which reconciles
into Quay over the Quay REST API) or `keycloak.holos.run` (which reconciles into
Keycloak), a `ReferenceGrant` records nothing external: it is a declarative
authorization the referrers' reconcilers consult. This keeps the security
convention free of the credential, connectivity, and trust-anchor concerns the
data-plane groups carry.

### The `ReferenceGrant` Kind

`security.holos.run/v1alpha1` `ReferenceGrant` is **namespaced** and lives in the
**referent (target) namespace** — the namespace holding the object being
referenced. It mirrors Gateway API's `ReferenceGrant` From/To shape:

- **`spec.from[]`** — the referrers this grant authorizes. Each entry is a
  `group` / `kind` / `namespace` triple: a referrer of that group and kind, in
  that namespace, is permitted to reference into this (the grant's) namespace.
- **`spec.to[]`** — the *local* objects in this namespace that may be referenced.
  Each entry is a `group` / `kind` and an **optional** `name`: omitting `name`
  authorizes references to **all** objects of that group/kind in the namespace;
  setting it narrows the grant to a single named object.

An illustrative — concrete but not yet field-final — grant authorizing
`keycloak.holos.run` `User`, `Group`, and `Client` resources in the `my-project`
namespace to reference a specific `KeycloakInstance` in the `keycloak` namespace,
created **in `keycloak`** (the referent namespace):

```yaml
apiVersion: security.holos.run/v1alpha1
kind: ReferenceGrant
metadata:
  name: my-project-to-keycloak-instance
  namespace: keycloak            # the referent (target) namespace
spec:
  from:
    - group: keycloak.holos.run
      kind: User
      namespace: my-project      # the referrers' namespace
    - group: keycloak.holos.run
      kind: Group
      namespace: my-project
    - group: keycloak.holos.run
      kind: Client
      namespace: my-project
  to:
    - group: keycloak.holos.run
      kind: KeycloakInstance
      name: holos                # optional; omit to grant all instances here
```

The schema above is **illustrative**: this ADR fixes the convention (the From/To
shape, the referent-namespace placement, the namespaced scope, the no-external-
dependency property); a later CRD-implementation issue fixes the field-level API
(exact field names, optionality, CEL validation, printer columns).

### The trust model

The grant direction encodes a clear, asymmetric trust relationship:

- **Platform owners grant.** The owner of the **referent (instance) namespace**
  — the namespace holding the object to be referenced — creates the
  `ReferenceGrant` *in that namespace*. This is the only party with authority
  over the namespace whose objects are being exposed, so the grant is an
  affirmative act by the object's owner.
- **Platform users consume.** A platform user then references the granted object
  from CRs in their **own (project) namespace**. They cannot widen their own
  access — they can only reference what a referent-namespace owner has already
  granted.
- **No grant ⇒ rejected, never silently honored.** A cross-namespace reference
  with **no matching `ReferenceGrant`** is **rejected by the referrer's
  reconciler**, which sets a `Ready=False` status condition (the Gateway-API
  status model, see *Status reporting*) explaining the missing grant. The
  reference is never silently honored just because the controller's credential
  *could* resolve it. This is the same default-deny posture Gateway API's
  `ReferenceGrant` enforces, and the same claim-not-seize discipline ADR-19's
  Organization adoption model uses.

### Why a holos-owned grant rather than Gateway API's

The platform mints its **own** `security.holos.run` `ReferenceGrant` rather than
reusing `gateway.networking.k8s.io`'s `ReferenceGrant`. The decisive reason is
**API ownership and boundary**, not a claim that Gateway's grant is technically
incapable:

- **Ownership and API boundary (the decisive reason).** A holos-owned grant keeps
  the platform's cross-namespace-reference policy inside the `holos.run` API
  surface — reconciled by the `holos-controller` ([ADR-18](ADR-18.md)), evolving
  with the `holos.run` API groups, and free of any dependency on the Gateway API
  being installed at all. Co-opting `gateway.networking.k8s.io`'s grant for
  arbitrary `holos.run` CR-to-CR references would couple a core platform safety
  primitive to the Gateway API's release cadence, conformance surface, and
  installation, and would overload a grant the istio-gateway components already
  use for their own legitimate route/backend/certificate cases.
- **Intended scope and interpretation.** Gateway API's `ReferenceGrant` is the
  authorization primitive **for cross-namespace references made by Gateway API
  resources** — its `from`/`to` are interpreted by Gateway API controllers for
  the Gateway/Route object-reference cases (an `HTTPRoute` `backendRefs` →
  `Service`, a listener `certificateRefs` → `Secret`, and the like). While an
  implementation *may* extend the kinds it honors, no controller interprets a
  Gateway grant to authorize a `keycloak.holos.run` `User` → `KeycloakInstance`
  reference; the `holos-controller` would have to teach itself the same grant
  anyway. Owning the grant makes that interpretation explicit and unambiguous
  rather than overloading another group's primitive.
- **The platform needs CR-to-CR generality.** The references this convention must
  authorize are between **arbitrary `holos.run` custom resources** — e.g. a
  `keycloak.holos.run` `User`/`Group`/`Client` referencing a `KeycloakInstance`
  in another namespace, or any future `holos.run` CR referencing another. A
  holos-owned grant generalizes the Gateway-API From/To pattern to these CR-to-CR
  references within the platform's own API group, where the platform controls the
  semantics end to end.

The two grants therefore **coexist**: Gateway API's `ReferenceGrant` governs
route/backend/certificate references (the istio-gateway cases ADR-21 describes);
`security.holos.run`'s `ReferenceGrant` governs `holos.run` CR-to-CR references.

### Rich status reporting on all `holos.run` CRs

A denied cross-namespace reference must be **observable**, and observability is
only useful if it is *uniform* across the platform's growing set of API groups.
This ADR therefore makes a second, cross-cutting decision: **every `holos.run`
custom resource reports rich status following the Gateway-API model** that
`quay.holos.run` ([ADR-19](ADR-19.md)) already ships and the Holos Controller
([ADR-18](ADR-18.md)) reconciles. Concretely, each CR's `status` carries:

- a **`conditions[]`** slice of standard `metav1.Condition` (`+listType=map`,
  `+listMapKey=type`, merge-patch on `type`) using the standard
  **`Accepted`** (the spec was understood and claimed), **`Programmed`** (the
  desired state was written to the backend — or, for a passive policy resource,
  validated/accepted), and **`Ready`** (fully provisioned and usable) condition
  types, with Kind-specific reasons defined once in a shared constants file;
- a **`status.observedGeneration`** recording the last `spec` generation
  reconciled; and
- at least one **printer column surfacing `Ready`**.

This is bundled into this ADR (rather than a separate record) because it is the
same cross-cutting safety-convention concern as the `ReferenceGrant` — and
because the grant's enforcement *depends* on it: the referrer's reconciler
surfaces a denied reference as a **`Ready=False`** condition naming the missing
grant, so an operator sees exactly which `ReferenceGrant` to create and in which
namespace. Without a guaranteed status vocabulary, that rejection would be
invisible.

**How it applies to a passive policy resource like `ReferenceGrant`.** A
`ReferenceGrant` reconciles nothing into an external system, so its `Programmed`
condition reflects *acceptance/validation* of the grant (its `from`/`to` are
well-formed and the referent objects, if named, are resolvable in-namespace)
rather than a backend write; `Ready` reflects that the grant is in effect.
Active CRs (`quay.holos.run`, future `keycloak.holos.run`) use the same three
types with backend-write semantics for `Programmed`. The vocabulary is uniform;
the precise per-Kind reasons are fixed by each Kind's later
CRD-implementation issue.

## Decision

1. **A new `security.holos.run` API group is established**, owned by the
   platform's security/safety conventions and reconciled (in future) by the
   `holos-controller` ([ADR-18](ADR-18.md)). Its defining Kind is
   `ReferenceGrant`.
2. **`security.holos.run/v1alpha1` `ReferenceGrant` is the standard mechanism**
   authorizing cross-namespace references between `holos.run` custom resources.
   It is **namespaced**, lives in the **referent (target) namespace**, and
   declares `spec.from[]` (group/kind/namespace of authorized referrers) and
   `spec.to[]` (group/kind, optionally `name`, of the local objects that may be
   referenced) — mirroring Gateway API's From/To shape.
3. **It takes no dependency on any external system** — it is pure
   Kubernetes-native policy the referrers' reconcilers consult; it reconciles
   nothing into Quay, Keycloak, or any other backend.
4. **The trust model is asymmetric and default-deny:** platform owners create
   the grant in the instance/referent namespace; platform users then reference
   the granted object from CRs in their own project namespaces; a cross-namespace
   reference with **no matching grant is rejected** by the referrer's reconciler
   (a `Ready=False` status condition), **never silently honored**.
5. **A holos-owned grant is minted rather than reusing Gateway API's**, decided on
   **API ownership and boundary** — keeping a core platform safety primitive in
   the `holos.run` surface, free of any dependency on the Gateway API being
   installed, and without overloading a grant istio-gateway already uses — rather
   than on any claim that Gateway's grant is technically incapable. The
   `security.holos.run` grant generalizes the From/To pattern to arbitrary
   `holos.run` CR-to-CR references. The two grants coexist.
6. **Every `holos.run` custom resource reports rich status following the
   Gateway-API model:** a `status.conditions[]` of standard `metav1.Condition`
   (`+listType=map`, `+listMapKey=type`) with `Accepted`/`Programmed`/`Ready`
   types, `status.observedGeneration`, and a `Ready` printer column — the
   `quay.holos.run` ([ADR-19](ADR-19.md)) shape, generalized to all CRs. This is
   what makes a denied cross-namespace reference observable (`Ready=False`); a
   passive policy resource like `ReferenceGrant` uses `Programmed` for
   acceptance/validation rather than a backend write.
7. **This convention is a guard rail for all current and future `holos.run`
   custom resources.** It is recorded in `AGENTS.md` under *Guard Rails*; the
   API-group ADRs that consume cross-namespace references
   ([ADR-20](ADR-20.md), [ADR-21](ADR-21.md)) will reference it as they are
   revised to adopt the convention (a later phase of this work updates ADR-20).
8. **This phase fixes the convention only — no Go or CUE code.** The
   `ReferenceGrant` schema here is illustrative; the field-level API, CEL
   validation, printer columns, and the reconciler land in later
   CRD-implementation issues.

## Consequences

- **One safe cross-namespace-reference convention, fixed once.** Every API group
  that needs a cross-namespace reference — Keycloak ([ADR-20](ADR-20.md)) first,
  any future group after — uses the same `ReferenceGrant`, rather than each CRD
  inventing its own cross-namespace authorization. The convention is decided
  here and not re-litigated per group.
- **Default-deny adds an explicit grant step.** A platform user referencing an
  instance in another namespace cannot proceed until a referent-namespace owner
  has created the matching grant. This is the intended safety trade — no silent
  cross-tenant access — but it is an extra, deliberate provisioning step (one the
  Project component in [ADR-21](ADR-21.md) already anticipates emitting per
  project).
- **A new API group and reconciler to build.** The `security.holos.run` group,
  the `ReferenceGrant` CRD, and the grant-checking logic each referrer's
  reconciler runs are future work owned by the `holos-controller`
  ([ADR-18](ADR-18.md)). This ADR does not ship them.
- **A uniform status contract every CR must meet.** Mandating
  `Accepted`/`Programmed`/`Ready` + `observedGeneration` + a `Ready` printer
  column on **all** `holos.run` CRs makes status legible and Argo-CD-health
  friendly across groups, but it binds every future CRD (and any retrofit of an
  existing one) to that shape — including passive policy resources, which must
  give `Programmed`/`Ready` an acceptance/validation meaning rather than a
  backend-write one. The `quay.holos.run` CRDs ([ADR-19](ADR-19.md)) already
  conform; new groups inherit the contract from this ADR rather than re-deciding
  it.
- **Coexistence with Gateway API's grant must stay legible.** Two `ReferenceGrant`
  kinds now exist in the platform — Gateway API's (route/backend/certificate
  references) and `security.holos.run`'s (`holos.run` CR-to-CR references).
  Documentation and component code must name the group explicitly so the two are
  never conflated; [ADR-21](ADR-21.md) already keeps the route-attachment
  (`allowedRoutes`) and object-reference (`ReferenceGrant`) mechanisms distinct,
  and this ADR extends that discipline to the group boundary.
