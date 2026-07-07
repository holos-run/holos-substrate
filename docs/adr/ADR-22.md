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
| 2        | 2026-07-04 | @jeffmccune | **Design-record only: add drift-observability status fields for external-resource CRs (HOL-1454).** Every `holos.run` CR whose reconciler fronts an external system (Keycloak, Quay, and future peers) carries `status.lastValidatedTime`, `status.lastMutatedTime`, `status.lastMutationReason`, and `status.lastDriftTime` in addition to the Gateway-API conditions/`observedGeneration` model. The fields distinguish the last successful remote validation from the last actual remote mutation, and classify that mutation as `SpecChange` (intentional/spec-driven) or `DriftRemediation` (corrective/out-of-band drift healed), borrowing Puppet's corrective-vs-intentional change model and Argo CD self-heal semantics. Read-only validators that never mutate the remote system, such as `KeycloakInstance`, carry `lastValidatedTime` only. No CRD or reconciler behavior changes in this revision; `Status: Proposed` unchanged |
| 3        | 2026-07-04 | @jeffmccune | **Implement the drift-observability retrofit for shipped external-resource Kinds (HOL-1459).** The already-shipped `keycloak.holos.run` Kinds (`KeycloakInstance`, `KeycloakGroup`, `KeycloakUser`, `KeycloakClient`) and `quay.holos.run` Kinds (`Organization`, `Repository`) now expose the Rev 2 status timestamps and `Validated` printer columns in their CRDs. `KeycloakInstance` remains validation-only (`lastValidatedTime` only); the mutating Kinds report `lastValidatedTime`, `lastMutatedTime`, `lastMutationReason`, and `lastDriftTime` with the canonical `SpecChange` / `DriftRemediation` values. Their reconcilers update validation only after successful remote verification, stamp mutation only after successful remote changes, return bounded steady-state resyncs, and use generation-changed primary watches to avoid status-write hot loops. |
| 4        | 2026-07-07 | @jeffmccune | **Design-record only: add the Adopt & Preserve lifecycle contract for external-resource CRs (HOL-1533).** Every `holos.run` CR that fronts a nameable external resource acquires pre-existing resources only with explicit `spec.adopt`, derives external identity from immutable spec fields rather than `metadata.name`, and carries `spec.deletionPolicy` with omitted/`Delete`/`Orphan` semantics that preserve the shipped non-destructive adoption invariant while adding an explicit abandon/transfer path. Read-only validators and set-membership managers get narrow exemptions as described below. Design-record only — no CRD or reconciler behavior changes in this revision; `Status: Proposed` unchanged |

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

### Drift-observability status for external-resource CRs

The Gateway-API condition model answers "is this object Ready?" and records when
that condition last **transitioned**. It does not answer two operational
questions platform engineers need for controllers that front external systems:
when did the controller last verify the remote resource still matched the CR, and
when did it last actually change the remote system? A resource can remain
`Ready=True` for weeks, so `conditions[].lastTransitionTime` is not a freshness or
drift signal.

External-resource CRs therefore carry a small, uniform set of drift-observability
status fields. The model borrows from Puppet's per-resource reporting and its
corrective-vs-intentional change distinction, and maps corrective changes to the
same operational idea as Argo CD self-heal: the declared state did not change,
but the remote object drifted and the controller healed it back.

| Field | Type | Semantics |
| -- | -- | -- |
| `status.lastValidatedTime` | `metav1.Time`, optional | Last reconcile that successfully **read** the remote API and confirmed or restored the declared state. Set on every successful reconcile, including no-op verifications. **Never** set when the remote read/verification failed; a stale value must remain visible as staleness. |
| `status.lastMutatedTime` | `metav1.Time`, optional | Last time the controller actually **changed** the external system for this resource: create, update, delete, assign, remove, or an equivalent remote mutation. **Check-then-apply**: an unconditional idempotent write counts only when observed remote state differed. |
| `status.lastMutationReason` | string enum `SpecChange` / `DriftRemediation`, optional | Which side changed to cause the last mutation. `SpecChange` means the CR spec changed (a new `metadata.generation`) and the controller configured the external resource to match the new desired state. `DriftRemediation` means the external resource changed out-of-band under an unchanged generation and the controller healed it back to the declared state. Always written in the same status update as `lastMutatedTime`; absent until the first mutation. |
| `status.lastDriftTime` | `metav1.Time`, optional | Last **corrective** change: validation found the remote resource out of sync while `metadata.generation == status.observedGeneration`, so third-party drift was remediated. Preserved across later intentional changes; it answers "when did drift last occur" while `lastMutationReason` classifies only the most recent mutation. |

Rules:

1. `conditions[].lastTransitionTime` MUST NOT be used as a freshness or drift
   signal; these timestamps exist to close that gap.
2. **Fail-closed freshness:** an errored reconcile does not advance
   `lastValidatedTime`; a stale validation timestamp must remain visible when
   remote read/verification failed. This does not erase completed mutations: if the
   controller successfully changed the external system before a later operation
   failed, it must still record `lastMutatedTime`/`lastMutationReason` for that
   completed mutation, or retry status recording before doing more work.
3. `lastMutatedTime` and `lastMutationReason` are written together, atomically in
   the same status update. A mutation with `lastMutationReason:
   DriftRemediation` also sets `lastDriftTime` to the same instant.
4. **Bounded staleness:** reconcilers of external resources MUST re-validate
   periodically. A steady-state successful reconcile returns `RequeueAfter` with
   a resync interval, so a stale `lastValidatedTime` is an actionable alert that
   the controller has stopped verifying the external resource.
5. **Hot-loop guard:** because `lastValidatedTime` advances on most successful
   reconciles, the primary watch MUST filter to generation changes
   (`predicate.GenerationChangedPredicate` or equivalent) so status-only writes
   do not self-requeue.
6. **Scope:** every `holos.run` CR whose reconciler talks to an external system
   carries these fields: Keycloak, Quay, and future external-system groups. CRs
   with no external surface are out of scope.
7. **Read-only validators:** a reconciler that never mutates the remote system
   carries `lastValidatedTime` only and omits the mutation fields entirely. For
   example, `KeycloakInstance` checks reachability and credentials but does not
   change Keycloak state.
8. **Printer column:** external-resource CRDs SHOULD add an extended `Validated`
   printer column (`type=date`, `priority=1`, JSONPath
   `.status.lastValidatedTime`) so stale validation is visible without a custom
   query.
9. **Canonical enum values:** `SpecChange` and `DriftRemediation` are canonical
   across all API groups. Each group defines its own Go constants with these
   exact string values because API packages do not import one another.

### Adopt & Preserve lifecycle contract for external-resource CRs

External-resource CRs need a single lifecycle contract for resources that may
already exist before Kubernetes begins managing them. That contract must prevent
one namespace from silently seizing a global external name, must make rename and
transfer possible without deleting the remote object, and must keep the
already-shipped Quay and Keycloak adoption behavior non-destructive.

This ADR therefore defines the **Adopt & Preserve** contract for every
`holos.run` CR that fronts a nameable external resource:

1. **Acquisition is explicit.** A CR that can own a pre-existing nameable
   external resource carries `spec.adopt bool`, defaulting to `false`. When an
   unclaimed external resource already exists, the reconciler reports a
   `Conflict` unless `spec.adopt` is set. Omitted and explicit `false` are
   equivalent, so this field is a plain boolean rather than `*bool`.
2. **External identity comes from immutable spec fields, never
   `metadata.name`.** The external name or identity is declared in spec and made
   immutable with validation. Current examples are `Organization.spec.name`,
   `Repository.spec.organizationRef` plus `spec.name`, `KeycloakGroup.spec.path`,
   `KeycloakUser.spec.email`, and `KeycloakClient.spec.clientId`. This decouples
   Kubernetes object naming from remote identity, so a CR can be renamed by
   orphaning and re-adopting the same external resource.
3. **Deletion behavior is declarative and provenance-aware.** Each mutating
   external-resource group defines its own `DeletionPolicy` enum in
   `api/<group>/v1alpha1/common_types.go`, matching the existing per-group
   `MutationReason` precedent and preserving the ADR-18 API dependency boundary.

The canonical enum shape is:

```go
// DeletionPolicy controls what happens to the external resource a
// Kubernetes resource fronts when that Kubernetes resource is deleted.
// +kubebuilder:validation:Enum=Delete;Orphan
type DeletionPolicy string

const (
	// DeletionPolicyDelete removes the external resource when the
	// Kubernetes resource is deleted, after verifying this resource
	// still owns it.
	DeletionPolicyDelete DeletionPolicy = "Delete"
	// DeletionPolicyOrphan leaves the external resource in place when
	// the Kubernetes resource is deleted. The controller removes only
	// its own ownership marker, if any, so a replacement resource can
	// adopt the external resource later.
	DeletionPolicyOrphan DeletionPolicy = "Orphan"
)
```

The canonical spec field shape is:

```go
// DeletionPolicy controls what happens to the <external resource> when
// this resource is deleted. Delete removes it from <system> after
// verifying ownership. Orphan leaves it in place, removing only this
// controller's ownership marker so a replacement resource can adopt it.
// When omitted, the behavior follows provenance: a <resource> this
// resource created is deleted, and an adopted one is released without
// being deleted.
// +optional
DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
```

The empty string is the unset sentinel; `omitempty` keeps the field off the wire
when omitted, so no pointer is needed.

| `deletionPolicy` | created by this CR | adopted by this CR |
| -- | -- | -- |
| *(omitted)* | delete (ownership-verified) — current behavior | release: prune controller-added side-effects (conferred roles, custodians, IdP links, recorded webhooks), keep the entity — current behavior |
| `Delete` | delete (ownership-verified) | delete (verified by marker / pinned UUID) |
| `Orphan` | abandon: strip only the holos ownership marker; **no other remote mutation** (no prunes) | abandon: strip marker if present; no other remote mutation |

Rules:

1. **Ownership is verified before destructive cleanup.** `Delete` never deletes a
   remote object merely because the CR has the same name. The finalizer must
   verify this CR's durable ownership marker, pinned remote UUID, or equivalent
   claim evidence before deletion.
2. **Omitted preserves shipped adoption semantics.** For resources created by the
   CR, omission keeps the current delete-on-CR-removal behavior. For resources
   adopted with `spec.adopt`, omission keeps the current release-not-delete path:
   the controller removes or prunes only the side effects it added, such as
   conferred roles, custodians, identity-provider links, or recorded webhooks,
   and leaves the external entity.
3. **`Orphan` is an abandon operation.** It strips only the holos ownership
   marker, if any, and performs no other remote mutation. It does not prune
   conferred roles, custodians, identity-provider links, webhooks, teams, or
   similar side effects. This is the transfer/rename path, so it avoids churn
   while another CR is about to adopt the same external resource.
4. **`Delete` on an adopted resource is deliberate.** It grants destructive
   cleanup authority over an external resource this CR did not originally
   create, but only after ownership can be verified by a marker, pinned UUID, or
   equivalent claim.
5. **Rename and transfer use orphan then adopt.** To move management of the same
   remote object to a different Kubernetes object name:
   1. Patch the old CR with `spec.deletionPolicy: Orphan`.
   2. Delete the old CR; the external resource is abandoned with markers stripped.
   3. Apply the CR under the new `metadata.name` with identical immutable
      identity fields and `spec.adopt: true`.
   4. Optionally set `spec.deletionPolicy: Delete` on the new CR to restore
      delete authority; because the new CR acquired the resource by adoption, its
      omitted default remains non-destructive release.
6. **Exemptions are narrow.** Read-only validators that own nothing, such as
   `KeycloakInstance`, omit both `spec.adopt` and `spec.deletionPolicy`.
   Set-membership managers with no single ownable external object, such as
   `KeycloakGroupMembership`, omit `spec.adopt` but still carry
   `spec.deletionPolicy`, because they mutate external membership edges and need
   delete-time preserve/prune semantics.
7. **New external-resource Kinds inherit this contract.** A new mutating
   external-resource CR must justify any exemption in its API-group ADR before
   shipping.

#### Sources of inspiration and deliberate deviations

This contract borrows vocabulary and lifecycle shape from established
Kubernetes-native controllers while deliberately preserving holos's tenant-safety
and non-destructive adoption model:

- **Config Connector** documents
  [`cnrm.cloud.google.com/deletion-policy`](https://docs.cloud.google.com/config-connector/docs/reference/annotations),
  acquiring existing resources by declaring them in Kubernetes
  ([managing and deleting resources](https://docs.cloud.google.com/config-connector/docs/how-to/managing-deleting-resources)),
  and immutable
  [`spec.resourceID`](https://docs.cloud.google.com/config-connector/docs/how-to/managing-resources-with-resource-ids)
  for decoupling external identity from `metadata.name`, including a rename flow
  that abandons then re-acquires the resource.
- **Crossplane managed resources** define
  [`spec.deletionPolicy: Delete|Orphan`](https://docs.crossplane.io/latest/managed-resources/managed-resources/)
  and the `crossplane.io/external-name` annotation for external identity.
- **External Secrets** separates creation ownership from deletion behavior with
  [`creationPolicy` and `deletionPolicy`](https://external-secrets.io/latest/guides/ownership-deletion-policy/).
- **cert-manager** keeps the target Secret when a Certificate is deleted unless
  certificate owner references are explicitly enabled, as documented in
  [Certificate usage](https://cert-manager.io/docs/usage/certificate/).
- **Kubernetes garbage collection** provides the upstream
  [`propagationPolicy: Orphan`](https://kubernetes.io/docs/concepts/architecture/garbage-collection/)
  vocabulary for abandoning dependents rather than deleting them.

The two deliberate deviations are:

1. **No acquire-by-default.** Config Connector acquires an existing resource by
   default when the Kubernetes object names the same remote identity. Holos
   requires `spec.adopt: true` because CRs are namespaced, several external
   namespaces are global, and the controller credential may be instance-wide
   (for example Quay's `FEATURE_SUPERUSERS_FULL_ACCESS`). A namespace-local CR
   must not silently seize a global external object.
2. **Omitted deletion policy is provenance-aware.** Crossplane defaults
   `spec.deletionPolicy` to `Delete`, including for imported resources. Holos
   keeps omitted deletion policy compatible with shipped behavior: created
   resources are deleted after ownership verification, while adopted resources
   are released without deleting the external entity. Explicit `Orphan` is
   stricter than omitted-adopted release because it performs no remote mutation
   beyond marker stripping.

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
7. **External-resource CRs also report drift-observability status.** Every
   `holos.run` CR whose reconciler fronts an external system records
   `lastValidatedTime`, `lastMutatedTime`, `lastMutationReason`, and
   `lastDriftTime` with the semantics in *Drift-observability status for
   external-resource CRs*. Read-only validators that never mutate the remote
   system carry `lastValidatedTime` only. The canonical mutation reasons are
   `SpecChange` and `DriftRemediation`.
8. **External-resource CRs follow the Adopt & Preserve lifecycle contract.**
   Every `holos.run` CR that fronts a nameable external resource acquires
   pre-existing resources only through explicit `spec.adopt`, derives external
   identity from immutable spec fields rather than `metadata.name`, and exposes
   `spec.deletionPolicy` with omitted/`Delete`/`Orphan` semantics. The omitted
   behavior preserves the current non-destructive release path for adopted
   resources, and explicit `Orphan` provides the rename/transfer path.
9. **These cross-cutting conventions are guard rails for current and future
   `holos.run` custom resources.** The ReferenceGrant, status,
   drift-observability, and Adopt & Preserve contracts are recorded in
   `AGENTS.md` under *Guard Rails*. API-group ADRs that consume them link back
   here rather than redefining them; ADR-19 and ADR-20 carry pointer rows for the
   group-specific Adopt & Preserve implementation phases.
10. **This phase fixes the convention only — no Go or CUE code.** The
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
- **External-resource status now exposes freshness and drift separately.**
  Operators can distinguish a controller that recently verified a remote object
  from one whose `Ready=True` condition is merely old, and can distinguish an
  intentional spec-driven mutation from corrective drift remediation. This adds
  implementation discipline: reconcilers must check before applying, preserve
  stale timestamps on errors, periodically re-validate, and avoid status-write
  hot loops with generation-change predicates.
- **External-resource deletion is now explicit and portable across groups.**
  API groups keep independent enum constants, but users see one
  omitted/`Delete`/`Orphan` contract across Quay, Keycloak, and future external
  systems. New reconcilers must track provenance and ownership evidence strongly
  enough to distinguish delete, release, and abandon paths.
- **Rename and transfer are supported without remote deletion.** Operators can
  move management between Kubernetes object names by orphaning the old CR and
  adopting with the new one. That path intentionally avoids side-effect pruning
  to prevent churn during handoff.
- **Coexistence with Gateway API's grant must stay legible.** Two `ReferenceGrant`
  kinds now exist in the platform — Gateway API's (route/backend/certificate
  references) and `security.holos.run`'s (`holos.run` CR-to-CR references).
  Documentation and component code must name the group explicitly so the two are
  never conflated; [ADR-21](ADR-21.md) already keeps the route-attachment
  (`allowedRoutes`) and object-reference (`ReferenceGrant`) mechanisms distinct,
  and this ADR extends that discipline to the group boundary.
