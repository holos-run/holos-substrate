# Project Resource Model: the Owner-Managed Control Plane

| Metadata | Value                                    |
|----------|------------------------------------------|
| Date     | 2026-07-04                               |
| Author   | @jeffmccune                              |
| Status   | `Proposed`                               |
| Tags     | api, multi-tenancy, rbac, self-service   |
| Updates  | ADR-21                                   |

| Revision | Date       | Author      | Info           |
|----------|------------|-------------|----------------|
| 1        | 2026-07-04 | @jeffmccune | Initial design |

## Context and Problem Statement

A project's resources today are entirely **platform-rendered**: a one-line
registration in `holos/projects/<name>.cue` drives the Project component
([ADR-21](ADR-21.md)), which renders the project's control-plane custom
resources — the `keycloak.holos.run` role/custodian groups, the owner
`KeycloakUser`, the project `KeycloakClient`, the Quay `Organization`, and the
Kargo/Argo CD wiring — into the bare `<name>` control namespace, applied by
`scripts/apply-projects` or the `<project>-control-plane` App-of-Apps root.
Every change flows through the platform repository.

That model answers *registration* well but leaves the **day-2 management
question** open: when a project owner wants to grant a colleague access to
their project — the concrete case is adding a person to a Keycloak role group
such as `projects/my-project/roles/editor` — what do they touch? Should
control-plane resources like a Keycloak group membership be **manageable by
the project owner directly**, through the platform API, or must every access
change round-trip through the platform repository or the Keycloak console?

This ADR lays out the overall resource model: which project resources live
where, who owns each layer, and — the key question — whether a membership
grant is a control-plane resource the project owner manages.

## References

- [ADR-2 — Core Platform Principles](ADR-2.md): the KRM is the platform's
  primary API; alternative interfaces are permitted only after the KRM has
  been eliminated in writing. This is the forcing function behind the
  decision here.
- [ADR-3 — Authorization via Kubernetes RBAC and Group Membership](ADR-3.md):
  access is group membership approved by custodians; RBAC bindings with
  `Group` subjects map membership to access.
- [ADR-1 — Project Resource](ADR-1.md): the Project tenant and its
  `owner`/`editor`/`viewer` primitive roles.
- [ADR-20 — Keycloak API Group](ADR-20.md): the `KeycloakUser` Kind
  (pre-create by email + claim-scoped group membership), the FGAP v2
  custodian delegation, and the **Rev 7 transparent-controller contract** —
  the reconcilers enforce no naming policy; admission control is the
  designated home for tenant/platform disjointness.
- [ADR-21 — Holos Project and Application Components](ADR-21.md): the
  rendered scaffold this ADR layers the owner-managed plane on top of,
  including the owner RoleBinding (the built-in `admin` ClusterRole bound to
  the `projects/<name>/roles/owner` group in the bare `<name>` namespace).
- [ADR-22 — `security.holos.run` ReferenceGrant](ADR-22.md): cross-namespace
  references between `holos.run` CRs require a grant in the referent
  namespace. The `keycloak-instance` component already grants **every
  registered project's bare namespace** the right to reference the central
  `KeycloakInstance`, so owner-created CRs in that namespace are already
  authorized referrers.
- `internal/controller/keycloak/user_controller.go`: the as-built
  `KeycloakUser` membership semantics this design relies on — the reconciler
  joins every group in `spec.groups` and prunes **only** memberships it
  previously recorded in `status.managedGroups` (UUID-pinned), never
  memberships added out-of-band.

## Design

### Three planes of project resources

Project resources are laid out in three planes, distinguished by **source of
truth** and **who writes**:

| Plane | Resources | Source of truth | Writer |
|-------|-----------|-----------------|--------|
| 1. Rendered scaffold | Namespaces, AppProject/Application, Kargo Project/Warehouse/Stage, Quay `Organization`, the role/custodian `KeycloakGroup` trees, the project `KeycloakClient`, the standing-owner `KeycloakUser`s, the owner RoleBinding | `holos/projects/*.cue` + `holos/apps/*.cue` in the platform repository | Platform (GitOps: render → commit → apply) |
| 2. Owner-managed control plane | Additional `holos.run` CRs the owner creates in the bare `<name>` control namespace — first `KeycloakUser` (access grants), then `quay.holos.run` `Repository`; later, policy-gated, `KeycloakClient`/`KeycloakGroup` | The cluster (the Kubernetes API, per [ADR-2](ADR-2.md)) | Project owner (`kubectl apply`, RBAC-gated) |
| 3. Identity-system day-2 operations | Ad-hoc group-membership approvals in the Keycloak console | Keycloak | Custodians, via FGAP v2 delegation ([ADR-20](ADR-20.md)) |

**Plane 1 is platform-owned and read-only to tenants in practice.** Rendered
resources are reconciled from the committed deploy tree; an in-cluster edit is
drift that the delivery path reverts. Changing plane 1 means changing the
registration (a platform-repo PR today; the deferred self-service
`ProjectRequest` API later).

**Plane 2 is the owner's management surface.** Owner-created CRs are
*additive*: they carry names distinct from the rendered set, they are not part
of any Argo CD Application, and nothing prunes them. They land in the same
bare `<name>` control namespace as the rendered control-plane CRs — one
legible home for a project's control plane regardless of which plane wrote
each object. The existing machinery already accommodates them:

- The owner RoleBinding ([ADR-21](ADR-21.md)) binds the
  `projects/<name>/roles/owner` group to the built-in `admin` ClusterRole in
  that namespace.
- The `keycloak-instance` component's `ReferenceGrant` already authorizes
  every registered project's bare namespace to reference the central
  `KeycloakInstance` ([ADR-22](ADR-22.md)).
- The controller reconciles tenant CRs with the same claim/adoption model and
  Gateway-API status reporting as rendered CRs — a rejected or conflicting
  spec is an observable `Ready=False`, not a silent failure.

**Plane 3 remains complementary, not primary.** FGAP v2 custodian delegation
lets a custodian approve a membership request in Keycloak itself with no
Kubernetes round-trip. It stays because [ADR-3](ADR-3.md) values keeping
day-to-day approvals where custodians operate — but the KRM path below is the
platform's *primary* interface, per [ADR-2](ADR-2.md).

### The key question: a membership grant is a control-plane resource

**Yes.** When a project owner wants to grant a colleague access to their
project, they create a `KeycloakUser` CR in the project's control namespace:

```yaml
apiVersion: keycloak.holos.run/v1alpha1
kind: KeycloakUser
metadata:
  name: alice-example-com
  namespace: my-project
spec:
  instanceRef:
    name: holos-keycloak
    namespace: keycloak
  email: alice@example.com
  # The prescribed day-2 grant form: adopt the colleague's existing Keycloak
  # user rather than conflicting on it. Without adopt, a pre-existing user is
  # a terminal Conflict (the as-built claim model); with it, the CR adopts the
  # user and manages ONLY the memberships declared below.
  adopt: true
  groups:
    - /projects/my-project/roles/editor
```

The owner is authorized by the owner RoleBinding, the cross-namespace
`instanceRef` is authorized by the existing `ReferenceGrant`, the controller
creates the user by email — or, with `adopt: true`, adopts a pre-existing one
— and joins the declared role groups, and membership surfaces in the OIDC
`groups` claim exactly as for rendered users — Quay teams, Argo CD RBAC, and
Kubernetes RBAC `Group` subjects all follow with no additional wiring.
Adoption is per-CR (each CR tracks its own `status.managedGroups` edge), so
two projects granting the same person each hold their own adopting CR and
never contend.

**Revocation semantics follow the created/adopted distinction**, and the ADR
prescribes them explicitly because they are not symmetric:

- **Revoking a single role**: drop the group path from `spec.groups`. The
  reconciler prunes exactly that managed membership (UUID-pinned) and
  touches nothing else. This is the safe, always-correct revocation.
- **Deleting an *adopting* CR** (the prescribed grant form above): the
  finalizer **releases** the user — it prunes the memberships and IdP link
  this CR manages and leaves the Keycloak identity, and every membership it
  never managed, intact. Safe.
- **Deleting a CR that *created* the user** (`status.created: true`): the
  finalizer **deletes the Keycloak user entirely** — the identity and all of
  its memberships, including any accrued through other projects' CRs or
  custodian action. This is correct for the rendered standing-owner CRs
  (the platform provisioned the identity) but is a destructive footgun for a
  day-2 grant. This is why the grant form prescribes `adopt: true`: when the
  colleague does not yet exist in Keycloak, the CR falls back to creating
  them and records `created: true` — revoking such a grant means emptying
  `spec.groups`, and the phase-1 *created-identity deletion guard* (below)
  denies a tenant delete of a `created: true` CR outright, so the
  destructive path is closed by admission, not left to operator care.

The rationale:

- **[ADR-2](ADR-2.md) requires it.** The KRM is the primary platform API.
  Making the Keycloak console or a platform-repo PR the *only* grant path is
  an alternative interface, which ADR-2 permits only after eliminating the
  KRM in writing — and the KRM is demonstrably fit for this: the Kind, the
  reconciler, the RBAC hook, and the reference grant all exist.
- **It is auditable and observable.** The grant is a declarative object with
  `status.conditions` and `observedGeneration` (the ADR-22 status guardrail),
  admission-logged in the Kubernetes audit trail, and legible with
  `kubectl get keycloakusers -n my-project` — the single audit surface
  [ADR-3](ADR-3.md) wants.
- **It composes with the custodian path.** The `KeycloakUser` reconciler's
  membership prune is claim-scoped and UUID-pinned: it removes only
  memberships it previously recorded in `status.managedGroups`. A membership
  a custodian added in the Keycloak console is never revoked by a CR
  reconcile, and a CR-declared membership is re-ensured if a custodian
  removes it. The two planes have disjoint write-sets by construction.
- **One Kind, one owner per membership edge.** `KeycloakUser` already models
  exactly this (email + group paths); a person's project access is one CR in
  one namespace. No new Kind is required.

### RBAC: aggregated ClusterRoles, enabled per Kind in phases

Today the owner RoleBinding grants less than it appears to: the built-in
`admin` ClusterRole reaches custom resources only through **aggregation**,
and no `holos.run` CRD ships an aggregated ClusterRole — so a project owner
currently cannot create *any* `holos.run` CR. Plane 2 is enabled by shipping
aggregated ClusterRoles with the controller's RBAC manifests:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: holos-keycloakuser-admin
  labels:
    # admin ONLY — deliberately NOT aggregate-to-edit. A membership grant is
    # an authorization change; the owner RoleBinding grants admin, and a
    # subject holding mere edit in the namespace must not be able to mint
    # grants.
    rbac.authorization.k8s.io/aggregate-to-admin: "true"
rules:
  - apiGroups: [keycloak.holos.run]
    resources: [keycloakusers]
    verbs: [create, delete, get, list, patch, update, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: holos-view
  labels:
    rbac.authorization.k8s.io/aggregate-to-view: "true"
rules:
  - apiGroups: [keycloak.holos.run, quay.holos.run]
    resources: ["*"]
    verbs: [get, list, watch]
```

Write access is aggregated **per Kind, in phases**, gated on the hazard each
Kind carries under the transparent controller ([ADR-20](ADR-20.md) Rev 7).
Read access (`view` aggregation) covers all `holos.run` Kinds immediately;
**no write phase ships without its admission-policy prerequisite**:

1. **Phase 1 — `KeycloakUser`** (this ADR's grant path) and `quay.holos.run`
   `Repository`, **prerequisite: the three phase-1 admission policies
   below.**
2. **Phase 2 — `KeycloakClient`, `KeycloakGroup`, `Organization`,
   prerequisite: the full tenant/platform disjointness policy set.** The
   transparent controller writes group paths, client IDs, and role names
   verbatim; a tenant-created `KeycloakGroup` could confer roles on the
   `argocd` client, and a tenant `KeycloakClient` could claim a reserved
   client ID. Those Kinds stay platform-rendered until the
   `ValidatingAdmissionPolicy`/webhook effort ADR-20 Rev 7 designates
   enforces disjointness across the whole surface.

**The admission-control effort is therefore load-bearing for plane 2 from
phase 1 onward — a prerequisite, not a follow-up.** `KeycloakUser` is not a
safe Kind on its own: `spec.groups` is not confined to the owner's own
project, the controller joins any group path that exists, and Kubernetes
RBAC, Quay synced teams, and Argo CD all key on the resulting `groups` claim
— an unconfined tenant `KeycloakUser` is a cross-project privilege
escalation. Three policies are the phase-1 prerequisite, all expressible as
`ValidatingAdmissionPolicy` CEL rules. Every exemption below keys on
**requester identity** (`request.userInfo` — the platform's delivery-path
subjects, e.g. the Argo CD applier service account and the platform-admin
group), **never on object state a tenant can write**:

- **Group-subtree confinement**: on create/update by a non-platform subject,
  every entry in `KeycloakUser.spec.groups` must match
  `/projects/<metadata.namespace>/…` — a tenant CR grants roles only within
  its own project's subtree. Platform subjects are exempt (preserving the
  rendered CRs and any future platform-level grants); the exemption is the
  requester's identity, not a marker on the object, precisely so a tenant
  cannot self-apply an exemption.
- **Rendered-object protection**: the rendered scaffold carries a
  platform-ownership marker label (the concrete key is settled by the
  implementation issue; the Project component already labels its resources).
  The policy (a) denies update/delete of marker-carrying objects by
  non-platform subjects, and (b) denies a non-platform subject setting,
  changing, or removing the marker on create or update — without (b) the
  marker would be forgeable and (a) meaningless as a boundary. Without this
  policy, the aggregated verbs would let an owner mutate or delete the
  *rendered* CRs in their namespace — including deleting a standing-owner
  `KeycloakUser` whose finalizer would delete the owner's Keycloak identity.
  With it, "rendered resources are read-only to tenants" is enforced, not
  aspirational.
- **Created-identity deletion guard**: deny a non-platform subject deleting a
  `KeycloakUser` whose `status.created` is `true` (on `DELETE` the policy
  evaluates `oldObject.status`), with a message directing the owner to empty
  `spec.groups` instead. This closes the destructive path the revocation
  semantics above document: a grant CR that fell back to *creating* the
  colleague's identity cannot be deleted by a tenant, because its finalizer
  would delete the Keycloak identity and every membership it accrued —
  including other projects' grants. Deleting an *adopting* CR (release
  semantics) remains permitted; a `created=true` grant is removed by a
  platform operator, or the guard is relaxed if a future
  `deletionPolicy: Retain`-style API refinement makes tenant deletes
  uniformly release-only.

### What the rendered `owners` map means now

The registration's `owners` map ([ADR-21](ADR-21.md)) remains the **standing
owner set**: the bootstrap grant that exists before any owner can act, and
the platform's durable record of who ultimately holds the project. It is not
the general grant path. Editor/viewer grants, additional owners, and
revocations are plane-2 operations. A person declared in both places is
harmless: group joins are idempotent, and the two CRs manage their own
membership edges independently.

### Rejected alternatives

- **Git-only grants (edit the registration / a platform-repo PR per grant).**
  Makes the platform team the choke point for every tenant access change and
  turns a day-2 operation into a platform deployment. Rejected as the
  *primary* path; the registration remains the standing-owner record.
- **Keycloak-console-only grants (FGAP custodians as the sole path).** No KRM
  record, a second audit surface, and an alternative interface adopted
  without the written KRM elimination [ADR-2](ADR-2.md) requires. Kept as a
  complement (plane 3), not the primary.
- **A new membership Kind (e.g. `KeycloakGroupMembership`).** One grant per
  CR is appealing, but `KeycloakUser` already carries the email + groups
  edge with claim-scoped pruning; a second Kind would create two owners for
  the same membership edge and a conflict surface between them. Revisit only
  if per-grant approval workflow (request/approve CRs, the
  `custodianDelegation: controller` alternative in [ADR-20](ADR-20.md))
  is built.

## Decision

1. **Project resources are laid out in three planes**: the platform-rendered
   scaffold (git-owned, [ADR-21](ADR-21.md)), the owner-managed control plane
   (KRM CRs in the bare `<name>` control namespace), and identity-system
   day-2 operations (FGAP v2 custodians, [ADR-20](ADR-20.md)).
2. **Control-plane resources are the project owner's management surface — a
   membership grant is a control-plane resource.** A project owner grants a
   colleague access by creating a `KeycloakUser` CR with `adopt: true`
   (email + role-group paths) in the project's control namespace; revocation
   is dropping the group from `spec.groups`, or deleting an adopting CR
   (release semantics — the identity survives; a tenant delete of a CR that
   *created* the user is denied by the phase-1 deletion guard, because its
   finalizer would delete the identity).
3. **Plane 2 is enabled by aggregated ClusterRoles (`admin`-aggregated only,
   never `edit`), per Kind, in phases, each phase gated on its
   admission-policy prerequisite**: `KeycloakUser` and `Repository` first
   (plus `view` on all `holos.run` Kinds), prerequisite the group-subtree
   confinement, rendered-object protection (marker-forgery-proof,
   identity-keyed exemptions), and created-identity deletion guard policies;
   `KeycloakClient`/`KeycloakGroup`/`Organization` only after the
   admission-control policies of [ADR-20](ADR-20.md) Rev 7 enforce full
   tenant/platform disjointness.
4. **Rendered resources stay platform-owned**; owner-created resources are
   additive and never collide with the rendered set. The registration's
   `owners` map remains the standing-owner record, not the general grant
   path.
5. **The FGAP v2 custodian path remains complementary**, composing with the
   KRM path through the `KeycloakUser` reconciler's claim-scoped,
   UUID-pinned membership pruning.

## Consequences

- **The platform repository stops being the choke point for access grants.**
  Day-2 grants become tenant self-service through the platform's primary API,
  honoring [ADR-2](ADR-2.md) without building any new interface.
- **New RBAC manifests ship with the controller**: aggregated ClusterRoles
  per phase-1 Kind plus the `view` aggregation. No change to the Project
  component's owner RoleBinding — aggregation makes the existing binding
  sufficient.
- **Admission control becomes load-bearing from phase 1.** The downstream
  `ValidatingAdmissionPolicy` effort ([ADR-20](ADR-20.md) Rev 7) is a
  **prerequisite** for every write phase: the group-subtree confinement,
  rendered-object protection, and created-identity deletion guard policies
  must ship before `KeycloakUser` write access is aggregated, and the full
  disjointness set before the phase-2 Kinds. No tenant write access is
  enabled ahead of its policies, and every policy exemption keys on
  requester identity, never on tenant-writable object state.
- **Two grant paths exist by design** (KRM and FGAP), with defined
  composition semantics; operators auditing "who has access" must consult
  Keycloak group membership as the effective state, with `KeycloakUser` CRs
  as the declarative subset the platform manages.
- **ADR-21's `owners` map semantics are narrowed** to the standing-owner
  record (`Updates: ADR-21`); the deferred `ProjectRequest` API and the
  admission policies are the natural follow-ups, and this ADR is a design
  record only — the aggregated ClusterRoles and any admission policy land in
  follow-up implementation issues.
