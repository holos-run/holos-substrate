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
- [ADR-20 — Keycloak API Group](ADR-20.md): the `KeycloakGroup` (role and
  custodian trees, FGAP v2 custodian delegation), the `KeycloakUser`
  (identity pre-provisioning by email), and the **Rev 7
  transparent-controller contract** — the reconcilers enforce no naming
  policy; admission control is the designated home for tenant/platform
  disjointness.
- **HOL-1453 — the first-class `KeycloakGroupMembership` Kind** (in flight;
  its docs phase HOL-1454 records the design as an ADR-20 revision). A
  `RoleBinding`-shaped CR: an immutable `groupRef` naming the
  `KeycloakGroup` whose membership it manages, plus a `members` list
  (identified by email). Authorization is the **double-binding model**: a
  membership CR in the group's own namespace is implicitly authorized (the
  namespace owner owns both objects); a cross-namespace `groupRef` requires
  a `security.holos.run` `ReferenceGrant` in the group's namespace, denied
  fail-closed (`Ready=False`, `ReferenceNotGranted`). The same plan
  **removes `KeycloakUser.spec.groups`** (HOL-1458) — the self-asserted
  membership path flagged by security finding HOL-1435, where whoever could
  write a `KeycloakUser` could join any user to any group with no consent
  from the group's owner. This ADR builds its grant path on that Kind and
  does not redefine it; the ADR-20 revision owns the spec.
- [ADR-21 — Holos Project and Application Components](ADR-21.md): the
  rendered scaffold this ADR layers the owner-managed plane on top of,
  including the owner RoleBinding (the built-in `admin` ClusterRole bound to
  the `projects/<name>/roles/owner` group in the bare `<name>` namespace).
- [ADR-22 — `security.holos.run` ReferenceGrant](ADR-22.md): cross-namespace
  references between `holos.run` CRs require a grant in the referent
  namespace — the mechanism the membership double-binding builds on. The
  `keycloak-instance` component already grants every registered project's
  bare namespace the right to reference the central `KeycloakInstance` —
  but the grant enumerates referrer **kinds** (today `KeycloakGroup`,
  `KeycloakUser`, `KeycloakClient`), so **extending its `from[]` with
  `KeycloakGroupMembership` is an explicit prerequisite** of the grant
  path: without it, a membership CR's cross-namespace `instanceRef` fails
  `ReferenceNotGranted`. The HOL-1453 plan's CUE phase owns that grant
  update.

Group paths in `keycloak.holos.run` specs are written in the **canonical
spec form `projects/<name>/…` with no leading slash** — the form the CRD
documentation and the rendered project manifests use (e.g.
`path: projects/my-project/roles/owner`). This ADR uses that form
throughout; a policy or example that must match paths matches the canonical
form.

## Design

### Three planes of project resources

Project resources are laid out in three planes, distinguished by **source of
truth** and **who writes**:

| Plane | Resources | Source of truth | Writer |
|-------|-----------|-----------------|--------|
| 1. Rendered scaffold | Namespaces, AppProject/Application, Kargo Project/Warehouse/Stage, Quay `Organization`, the role/custodian `KeycloakGroup` trees, the project `KeycloakClient`, the standing-owner `KeycloakUser`s and their `KeycloakGroupMembership`s, the owner RoleBinding | `holos/projects/*.cue` + `holos/apps/*.cue` in the platform repository | Platform (GitOps: render → commit → apply) |
| 2. Owner-managed control plane | Additional `holos.run` CRs the owner creates in the bare `<name>` control namespace — first `KeycloakGroupMembership` (access grants, HOL-1453); later, policy-gated, `quay.holos.run` `Repository` and `Organization`, `KeycloakUser`, `KeycloakClient`, `KeycloakGroup` | The cluster (the Kubernetes API, per [ADR-2](ADR-2.md)) | Project owner (`kubectl apply`, RBAC-gated) |
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
  `KeycloakInstance` ([ADR-22](ADR-22.md)) — per enumerated referrer kind,
  so each newly-aggregated Kind (starting with `KeycloakGroupMembership`)
  must be added to the grant's `from[]` (see References).
- The controller reconciles tenant CRs with the same claim/adoption model and
  Gateway-API status reporting as rendered CRs — a rejected or conflicting
  spec is an observable `Ready=False`, not a silent failure.

**Plane 3 remains complementary, not primary.** FGAP v2 custodian delegation
lets a custodian approve a membership request in Keycloak itself with no
Kubernetes round-trip, and HOL-1453 seeds project owners into the
`projects/<name>/custodians/*` groups so that delegation is live. It stays
because [ADR-3](ADR-3.md) values keeping day-to-day approvals where
custodians already operate — but the KRM path below is the platform's
*primary* interface, per [ADR-2](ADR-2.md).

### The key question: a membership grant is a control-plane resource

**Yes.** When a project owner wants to grant a colleague access to their
project, they create (or edit) a `KeycloakGroupMembership` CR — the
first-class, `RoleBinding`-shaped membership Kind HOL-1453 introduces — in
the project's control namespace:

```yaml
apiVersion: keycloak.holos.run/v1alpha1
kind: KeycloakGroupMembership
metadata:
  name: my-project-editors
  namespace: my-project
spec:
  instanceRef:
    name: holos-keycloak
    namespace: keycloak
  # The KeycloakGroup whose membership this CR manages (immutable). A
  # same-namespace groupRef is implicitly authorized: the namespace owner
  # owns both the group CR and this membership CR — the double-binding
  # model's local case.
  groupRef:
    name: my-project-roles-editor
  members:
    - alice@example.com
```

The owner is authorized by the owner RoleBinding, the membership is
authorized by the **double-binding model** (HOL-1453, building on
[ADR-22](ADR-22.md)): the `groupRef` names a `KeycloakGroup` in the same
namespace, which is implicitly consented — the group's owner and the
membership's author are the same namespace owner. A cross-namespace
`groupRef` (granting into *another* project's or the platform's group tree)
is denied fail-closed unless the group's namespace carries a
`ReferenceGrant` authorizing it. Authorization is therefore **structural**
— it follows from object placement and the referent namespace's consent —
not from an admission policy inspecting group-path strings.

Membership then surfaces in the OIDC `groups` claim as the project-prefixed
client-role values ([ADR-20](ADR-20.md)), and the relying parties wired
today follow: **Quay synced teams** (`Organization.spec.syncedTeams[]`,
[ADR-19](ADR-19.md)) and the **project and app clients' tokens**. Kubernetes
RBAC and Argo CD RBAC follow only where a binding exists — as built, the
Project component binds only `projects/<name>/roles/owner` (to the
namespace-`admin` RoleBinding), and Argo CD RBAC maps only platform roles;
editor/viewer bindings on those surfaces are future Project-component
wiring, not something a grant conjures on its own.

**Revocation is symmetric and never touches identities.** Removing the email
from `members` prunes exactly that membership; deleting the CR prunes the
memberships it manages. A `KeycloakGroupMembership` creates and deletes
**membership edges only** — never Keycloak users — so the grant path carries
no identity-lifecycle footgun. (Contrast the retired alternative below: when
membership rode on `KeycloakUser`, deleting a grant CR that had *created*
the user deleted the whole identity.)

**Identity pre-provisioning is a separate, optional step.** A membership
member is identified by email; if no Keycloak user with that email exists
yet, the membership reports `Ready=False` and converges once the user
exists (first login via the `esso` broker, or a `KeycloakUser`
pre-provisioning the identity — [ADR-20](ADR-20.md)). With
`KeycloakUser.spec.groups` removed (HOL-1458), `KeycloakUser` is purely the
identity Kind; granting access never requires writing one.

The rationale:

- **[ADR-2](ADR-2.md) requires it.** The KRM is the primary platform API.
  Making the Keycloak console or a platform-repo PR the *only* grant path is
  an alternative interface, which ADR-2 permits only after eliminating the
  KRM in writing — and the KRM is demonstrably fit for this: HOL-1453
  supplies the Kind, the reconciler, the double-binding authorization, and
  the delegation RBAC.
- **It is auditable and observable.** The grant is a declarative object with
  `status.conditions` and `observedGeneration` (the ADR-22 status
  guardrail) plus the drift-observability timestamps HOL-1453 mandates
  (`lastValidatedTime`/`lastMutatedTime`/`lastMutationReason`/
  `lastDriftTime`), admission-logged in the Kubernetes audit trail, and
  legible with `kubectl get keycloakgroupmemberships -n my-project` — the
  single audit surface [ADR-3](ADR-3.md) wants.
- **It composes with the custodian path.** A membership CR manages exactly
  the member edges it declares — multiple CRs may target the same group,
  and a membership a custodian added in the Keycloak console belongs to no
  CR and is never pruned by one, while a CR-declared membership a custodian
  removes out-of-band is healed back (`DriftRemediation`). The two planes
  have disjoint write-sets by construction.
- **The group's owner consents by construction.** The double-binding model
  closes the self-asserted-privilege hole (HOL-1435): a tenant can bind
  members only into group trees their namespace owns, or into groups whose
  owner granted them a `ReferenceGrant`. No admission string-matching on
  group paths is needed for this core boundary.

### RBAC: aggregated ClusterRoles, enabled per Kind in phases

Today the owner RoleBinding grants less than it appears to: the built-in
`admin` ClusterRole reaches custom resources only through **aggregation**,
and no `holos.run` CRD ships an aggregated ClusterRole — so a project owner
currently cannot create *any* `holos.run` CR. HOL-1456 ships the first
aggregated ClusterRole (for `keycloakgroupmemberships`) as part of the
membership plan; this ADR generalizes that mechanism to the plane-2 surface:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: holos-keycloakgroupmembership-admin
  labels:
    # admin ONLY — deliberately NOT aggregate-to-edit. A membership grant is
    # an authorization change; the owner RoleBinding grants admin, and a
    # subject holding mere edit in the namespace must not be able to mint
    # grants.
    rbac.authorization.k8s.io/aggregate-to-admin: "true"
rules:
  - apiGroups: [keycloak.holos.run]
    resources: [keycloakgroupmemberships]
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

1. **Phase 1 — `KeycloakGroupMembership` only** (this ADR's grant path,
   HOL-1453), **prerequisite: the rendered-object protection policy
   below.** Membership needs no group-path admission policy — the
   double-binding model bounds it structurally, its reconciler manages
   membership edges only, and its deletion deletes nothing but those edges.
   No other Kind qualifies for phase 1: every other `holos.run` Kind's
   finalizer or reconciler can mutate or destroy a **shared external
   object** a second CR can name.
2. **Phase 2 — `Repository`, `KeycloakUser`, `KeycloakClient`,
   `KeycloakGroup`, `Organization`, prerequisite: the full tenant/platform
   disjointness policy set plus per-Kind external-identity guards.** The
   transparent controller writes group paths, client IDs, and role names
   verbatim; a tenant-created `KeycloakGroup` could confer roles on the
   `argocd` client, and a tenant `KeycloakClient` could claim a reserved
   client ID. `Repository` needs an **external-identity collision guard**
   before tenant writes: the as-built reconciler manages whatever Quay repo
   `organizationRef`/`name` denote — an owner's second, unmarked CR naming
   a rendered repo (e.g. `my-app-config`) would manage it and its finalizer
   would **delete the backing Quay repository** on CR deletion; the marker
   policy protects the rendered CR object, not the external identity. The
   guard is a durable claim marker on the Quay side (the Organization's
   adoption model extended to repositories) or an admission uniqueness rule
   across CRs. `KeycloakUser` additionally needs the **created-identity
   deletion guard**: deleting a CR whose finalizer *created* the Keycloak
   user (`status.created: true`) deletes the identity and every membership
   it accrued, so a tenant delete of such a CR must be admission-denied (on
   `DELETE` the policy evaluates `oldObject.status`). These Kinds stay
   platform-rendered until the `ValidatingAdmissionPolicy`/webhook effort
   designated by ADR-20 Rev 7 enforces disjointness across the whole
   surface.

**The admission-control effort is load-bearing for plane 2 from phase 1
onward — a prerequisite, not a follow-up** — though the membership
double-binding shrinks what phase 1 needs from it to a single policy.
Every exemption keys on **requester identity** (`request.userInfo` — the
platform's delivery-path subjects, e.g. the Argo CD applier service account
and the platform-admin group), **never on object state a tenant can
write**:

- **Rendered-object protection** (the phase-1 prerequisite): the rendered
  scaffold carries a platform-ownership marker label (the concrete key is
  settled by the implementation issue; the Project component already labels
  its resources). The policy (a) denies update/delete of marker-carrying
  objects by non-platform subjects, and (b) denies a non-platform subject
  setting, changing, or removing the marker on create or update — without
  (b) the marker would be forgeable and (a) meaningless as a boundary.
  Without this policy, the aggregated verbs would let an owner mutate or
  delete the *rendered* CRs of the aggregated Kinds in their namespace —
  in phase 1, the rendered standing-owner membership CRs; in later phases,
  worse (a rendered `Repository` CR's finalizer deletes the backing Quay
  repository). With it, "rendered resources are read-only to tenants" is
  enforced, not aspirational.

### What the rendered `owners` map means now

The registration's `owners` map ([ADR-21](ADR-21.md)) remains the **standing
owner set**: the bootstrap grant that exists before any owner can act, and
the platform's durable record of who ultimately holds the project. Under
HOL-1457 it renders as `KeycloakGroupMembership` CRs seeding the owners into
`projects/<name>/roles/owner` and the `projects/<name>/custodians/*` groups
(plus the owners' `KeycloakUser` identity CRs). It is not the general grant
path: editor/viewer grants, additional owners, and revocations are plane-2
operations. A person declared in both places is harmless: membership joins
are idempotent, and each CR manages its own member edges independently.

### Rejected alternatives

- **Git-only grants (edit the registration / a platform-repo PR per grant).**
  Makes the platform team the choke point for every tenant access change and
  turns a day-2 operation into a platform deployment. Rejected as the
  *primary* path; the registration remains the standing-owner record.
- **Keycloak-console-only grants (FGAP custodians as the sole path).** No KRM
  record, a second audit surface, and an alternative interface adopted
  without the written KRM elimination [ADR-2](ADR-2.md) requires. Kept as a
  complement (plane 3), not the primary.
- **Membership as a field on the user (`KeycloakUser.spec.groups`).** The
  as-built model this plan retires (HOL-1458): whoever can write a
  `KeycloakUser` can join any user to any group path, with no consent from
  the group's owner — the self-asserted privilege escalation of security
  finding HOL-1435. It also entangles grant lifecycle with identity
  lifecycle (deleting a grant CR that created the user deletes the
  identity), and confining it would have required admission policies
  string-matching group paths per tenant. The first-class membership Kind
  inverts the authorization direction — the *group's* side consents via
  placement or `ReferenceGrant` — and scopes deletion to membership edges.

## Decision

1. **Project resources are laid out in three planes**: the platform-rendered
   scaffold (git-owned, [ADR-21](ADR-21.md)), the owner-managed control plane
   (KRM CRs in the bare `<name>` control namespace), and identity-system
   day-2 operations (FGAP v2 custodians, [ADR-20](ADR-20.md)).
2. **Control-plane resources are the project owner's management surface — a
   membership grant is a control-plane resource.** A project owner grants a
   colleague access by creating or editing a `KeycloakGroupMembership` CR
   (HOL-1453) in the project's control namespace; revocation is removing the
   member or deleting the CR, which prunes membership edges only and never
   deletes identities. Authorization is the double-binding model:
   same-namespace `groupRef` implicitly consented, cross-namespace denied
   fail-closed without a `ReferenceGrant` in the group's namespace.
3. **Plane 2 is enabled by aggregated ClusterRoles (`admin`-aggregated only,
   never `edit`), per Kind, in phases, each phase gated on its
   admission-policy prerequisite**: `KeycloakGroupMembership` alone first
   (plus `view` on all `holos.run` Kinds), prerequisite the
   marker-forgery-proof, identity-keyed rendered-object protection policy;
   `Repository` (with an external-identity collision guard), `KeycloakUser`
   (with the created-identity deletion guard), `KeycloakClient`,
   `KeycloakGroup`, and `Organization` only after the admission-control
   policies of [ADR-20](ADR-20.md) Rev 7 enforce full tenant/platform
   disjointness.
4. **Rendered resources stay platform-owned**; owner-created resources are
   additive and never collide with the rendered set. The registration's
   `owners` map remains the standing-owner record (rendered as membership
   CRs per HOL-1457), not the general grant path.
5. **The FGAP v2 custodian path remains complementary**, composing with the
   KRM path because a membership CR manages only its declared member edges:
   custodian-added memberships are never pruned, CR-declared memberships
   removed out-of-band are healed (`DriftRemediation`).

## Consequences

- **The platform repository stops being the choke point for access grants.**
  Day-2 grants become tenant self-service through the platform's primary API,
  honoring [ADR-2](ADR-2.md) without building any new interface.
- **This ADR depends on HOL-1453 landing.** The grant path is the
  `KeycloakGroupMembership` Kind, its double-binding authorization, the
  removal of `KeycloakUser.spec.groups`, and the delegation RBAC —
  designed and implemented under HOL-1453..HOL-1459 (the ADR-20 revision
  owns the Kind's spec). This ADR contributes the surrounding resource
  model: the three planes, the phased aggregation, and the rendered-object
  boundary.
- **New RBAC manifests ship with the controller**: aggregated ClusterRoles
  per phase-1 Kind (HOL-1456 ships the membership one) plus the `view`
  aggregation. No change to the Project component's owner RoleBinding —
  aggregation makes the existing binding sufficient.
- **Admission control remains load-bearing, but phase 1 needs only one
  policy.** The membership double-binding replaces group-path admission
  string-matching for the grant path; the rendered-object protection policy
  must still ship before any tenant write access is aggregated, and the
  full disjointness set (including the created-identity deletion guard)
  before the phase-2 Kinds. Every policy exemption keys on requester
  identity, never on tenant-writable object state.
- **Two grant paths exist by design** (KRM and FGAP), with defined
  composition semantics; operators auditing "who has access" must consult
  Keycloak group membership as the effective state, with
  `KeycloakGroupMembership` CRs as the declarative subset the platform
  manages — made legible by the HOL-1453 drift-observability timestamps.
- **A grant's blast radius today is Quay teams and the project/app client
  tokens.** Kubernetes RBAC and Argo CD RBAC follow a role grant only once
  bindings for the editor/viewer roles are rendered — future
  Project-component wiring this ADR does not design.
- **ADR-21's `owners` map semantics are narrowed** to the standing-owner
  record (`Updates: ADR-21`); the deferred `ProjectRequest` API and the
  admission policies are the natural follow-ups, and this ADR is a design
  record only — the aggregated ClusterRoles beyond HOL-1456's and the
  admission policy land in follow-up implementation issues.
