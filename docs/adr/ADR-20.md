# Keycloak API Group: OIDC Client, Client Role, Realm Role, and Group CRDs

| Metadata | Value                              |
| -------- | ---------------------------------- |
| Date     | 2026-06-17                         |
| Author   | @jeffmccune                        |
| Status   | `Proposed`                         |
| Tags     | api, controller, keycloak, oidc, rbac |
| Updates  | ADR-3                              |

| Revision | Date       | Author      | Info           |
| -------- | ---------- | ----------- | -------------- |
| 1        | 2026-06-17 | @jeffmccune | Initial design |

## Context and Problem Statement

The [Holos Controller](ADR-18.md) is the in-cluster controller that fills the
data-plane gaps the upstream Quay and Keycloak operators leave open, so product
engineers get a self-service "docker push to deploy" experience. Its first API
group — `quay.holos.run` — is specified in [ADR-19](ADR-19.md). This ADR records
the **second** group the controller should eventually own: a **Keycloak** API
group for the per-project, tenant-facing identity primitives a product engineer
needs to self-service. Concretely, four concepts:

1. an **OIDC Client** for a project's service (its client ID and secret delivered
   into the project namespace);
2. **Client Roles** — the primitive `owner`/`editor`/`viewer` triad scoped to
   that one client;
3. **Realm Roles** and the realm-role → client-role mapping (so a broad realm
   role like "core services developer" maps onto a service-scoped client role
   like "my-app editor"); and
4. **Group creation and membership** under a custodian-managed model, so that
   authenticating against a given OIDC client auto-assigns the right client
   roles.

Today the `holos` realm — its clients, roles, groups, default group membership,
and protocol mappers — is **fully declarative but platform-owned**: it is
authored in CUE and reconciled on every `scripts/apply` by the
`keycloak-config-cli` Job
([holos/components/keycloak/realm-config/buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue),
[holos/docs/keycloak-clients.md](../../holos/docs/keycloak-clients.md)). That
mechanism is excellent for the platform's *own* realm configuration, but it is
**not** a per-project, KRM-native self-service path: a product engineer cannot
declare "my service needs an OIDC client with owner/editor/viewer roles" as a
Kubernetes custom resource and have it reconciled. [ADR-18](ADR-18.md) names this
gap and explicitly leaves the ownership boundary between a future Keycloak API
group and the existing `keycloak-config-cli` Job for **this ADR to resolve**.

The issue this ADR records also marks the Keycloak resources as **lower priority
than the Quay CRDs and needing more design**. Accordingly, this ADR is
intentionally a `Proposed`, direction-setting record: it fixes *what* the
controller should own and *why*, sketches an illustrative schema to make the
concepts concrete, and enumerates the **open questions** — but it does **not**
lock a final schema, and **no code or CRD Go types are written**.

## References

- [ADR-18 — The Holos Controller and the GitOps Rendered-Manifest Delivery
  Model](ADR-18.md): names the controller, its `holos-controller` namespace, and
  the `<group>.holos.run` API-group convention. ADR-18 names the Keycloak
  data-plane gap (clients, roles, groups) and states that the ownership boundary
  between the controller's Keycloak group and the existing `keycloak-config-cli`
  reconciliation is **"a question ADR-20 must resolve"**. This ADR is that
  resolution-in-progress; ADR-18 carries the forward cross-reference to it.
- [ADR-3 — Authorization via Kubernetes RBAC and Group Membership](ADR-3.md): the
  platform authorizes via Kubernetes RBAC, mapping **group membership** to access
  through `RoleBinding`/`ClusterRoleBinding` subjects of kind `Group`, with
  **custodians** approving membership requests. ADR-3 explicitly treats group
  **provisioning and custodianship** as an *external* prerequisite — "not
  something the platform implements." This ADR **`Updates: ADR-3`** on exactly
  that point: a controller that creates Keycloak groups and reconciles
  custodian-approved membership makes the platform the provisioning mechanism for
  the **identity-system side** of ADR-3's groups, rather than assuming an external
  one. ADR-3's authorization *model* is unchanged — RBAC bindings with `Group`
  subjects, membership a custodian approves; this ADR only changes **who
  provisions the groups and runs the approval**, and that change is deferred (the
  custodian mechanism is an open question below, and the status stays `Proposed`).
- [ADR-19 — Quay API Group CRDs](ADR-19.md): the sibling first group. Its
  `Organization.spec.access[]` already keys on **Keycloak group names** as they
  appear in the shared `groups` claim — so the Quay access model and this
  Keycloak group model meet at the same group names. This ADR's group resource is
  the declarative source of those names.
- [holos/docs/keycloak-clients.md](../../holos/docs/keycloak-clients.md): the
  conventional declarative OIDC-client pattern — the `keycloak-config-cli`
  reconciliation mechanism and its apply-gate, **public vs confidential PKCE
  clients** (`argocd`/`kargo` public, `quay` confidential), the runtime
  client-secret bootstrap, the **three protocol mappers that feed the shared
  `groups` claim**, and the realm/client role model. A CRD-driven path must not
  contradict this; it abstracts over it.
- [holos/components/keycloak/realm-config/buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue):
  the authoritative source for how clients, realm roles (`roles.realm`), client
  roles (`roles.client.<clientId>`), groups (`groups`), default group membership
  (`defaultGroups: ["/authenticated"]`), realm users, the three `groups`-claim
  mappers (`oidc-group-membership-mapper`, `oidc-usermodel-realm-role-mapper`,
  `oidc-usermodel-client-role-mapper`), and the `quay-oidc` confidential
  client-secret bootstrap are declared and reconciled today.
- [holos/docs/secret-handling.md](../../holos/docs/secret-handling.md): the
  runtime-secret guardrail — secret material is created at runtime (an
  `ExternalSecret` or a generate-once create-if-absent bootstrap Job) and never
  committed. Delivering a confidential client's secret into a project namespace
  must honor this, exactly as the platform's own `quay-oidc` bootstrap does.

## Design

The four concepts below are presented as **illustrative** namespaced custom
resources in a Keycloak API group (working name **`keycloak.holos.run`**),
reconciled by the Holos Controller against the Keycloak admin API. The YAML is a
sketch to make the concepts concrete and reviewable — **the schemas are not
final**; the open questions in the *Decision* section bound what is decided.
Every resource keys on the same shared **`groups` claim** the platform already
uses, so a CRD-driven path composes with — rather than replaces — the relying
parties (Argo CD, Quay, Kargo) that already consume that claim.

### Why each resource

| Concept | Why the controller should own it |
| --- | --- |
| **OIDC Client** | A project's service needs its own OIDC client (a confidential client with a delivered secret, or a public PKCE client) to authenticate users. Today only the platform's own clients (`argocd`/`quay`/`kargo`) exist, declared centrally. A product engineer needs to declare their service's client as a custom resource and receive the client ID/secret **in their project namespace**, consistent with [secret-handling.md](../../holos/docs/secret-handling.md). |
| **Client Roles** | The `owner`/`editor`/`viewer` triad scoped to a single client is the primitive RBAC vocabulary for one service. It mirrors how the `quay` client today defines `platform-admin`/`project-admin` client roles ([keycloak-clients.md](../../holos/docs/keycloak-clients.md)); the controller would let a project declare its own service-scoped triad rather than hand-editing the central realm config. |
| **Realm Roles** | A realm role is the cross-service identity (e.g. "core services developer") that a person carries. Mapping a realm role onto a client role (e.g. realm "core services developer" → client "my-app editor") lets broad organizational roles compose down to per-service access without enumerating every person on every client. The platform already emits the `platform-owner` realm role into the `groups` claim via the realm-role mapper; this generalizes that to project-defined realm roles. |
| **Group + membership** | Per [ADR-3](ADR-3.md), authorization is group membership a **custodian** approves. The controller would create the project's groups and reconcile membership so that **authenticating against the service's OIDC client auto-assigns the client roles** the group carries — the group is the join point between ADR-3's custodian-approved membership and a service's client roles. This is also the source of the group names [ADR-19](ADR-19.md)'s `Organization.spec.access[]` already binds to Quay teams. |

### OIDC Client (illustrative)

A `KeycloakClient` declares one project OIDC client and delivers its credentials
into the project namespace.

```yaml
apiVersion: keycloak.holos.run/v1alpha1
kind: KeycloakClient
metadata:
  name: my-app
  namespace: my-project
spec:
  # The Keycloak client ID to create in the holos realm.
  clientId: my-app
  # public (SPA/CLI, no secret, PKCE S256) | confidential (delivered secret).
  # Mirrors the public (argocd/kargo) vs confidential (quay) distinction in
  # keycloak-clients.md; PKCE S256 is the default for both.
  type: confidential
  redirectUris:
    - https://my-app.holos.localhost/oauth2/callback
  webOrigins:
    - https://my-app.holos.localhost
  # The default owner/editor/viewer client-role triad scoped to THIS client.
  # See KeycloakClientRole below; declaring them inline is one open question.
  defaultRoles: [owner, editor, viewer]
  # For a confidential client, where to deliver the generated client secret.
  # The reconciler writes a generate-once, create-if-absent Secret per
  # secret-handling.md — it is never committed, mirroring the platform's own
  # quay-oidc bootstrap.
  secretRef:
    name: my-app-oidc           # Secret in metadata.namespace
    key: client_secret
status:
  observedGeneration: 1
  clientId: my-app
  conditions:
    - type: Ready
      status: "True"
      reason: Provisioned
    - type: SecretDelivered     # confidential clients only
      status: "True"
```

### Client Roles and Realm Roles (illustrative)

A `KeycloakClientRole` is a role scoped to one client; a `KeycloakRealmRole`
carries a realm role and the **realm-role → client-role** mapping that lets a
broad role compose down onto a service.

```yaml
apiVersion: keycloak.holos.run/v1alpha1
kind: KeycloakClientRole
metadata:
  name: my-app-editor
  namespace: my-project
spec:
  clientRef: my-app             # the KeycloakClient this role is scoped to
  role: editor                  # owner | editor | viewer (the primitive triad)
---
apiVersion: keycloak.holos.run/v1alpha1
kind: KeycloakRealmRole
metadata:
  name: core-services-developer
  namespace: my-project
spec:
  realmRole: core-services-developer
  # Composite mapping: this realm role grants the named client roles. A person
  # who carries the realm role thereby holds "my-app editor" without being named
  # on the client directly — the realm-role mapper already folds realm-role
  # names into the shared `groups` claim.
  mapsTo:
    - clientRef: my-app
      role: editor
```

### Group + membership (illustrative)

A `KeycloakGroup` creates a project group and reconciles a **custodian-managed**
membership model. A member of the group holds the group's client roles, and
authenticating against the owning client carries those roles into **that
client's own** token.

How the roles actually surface in tokens matters, and the platform's existing
mapper wiring constrains the design — the claim is **not** that a `my-app` client
role automatically appears in a *different* client's token (Quay's or Argo CD's)
just because a user logs in. In the current realm
([keycloak-clients.md](../../holos/docs/keycloak-clients.md),
[buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue)) the
client-role mapper is **per client** — the `quay` client carries an
`oidc-usermodel-client-role-mapper` keyed to `usermodel.clientRoleMapping.clientId:
QUAY_CLIENT_ID`, and Argo CD carries **no** client-role mapper at all. So:

- A project client's own **client roles** reach **that client's** token via a
  per-client client-role mapper the `KeycloakClient` reconciler provisions
  (mirroring the `quay` client's mapper). They do not leak into other clients'
  tokens.
- What **cross-service** relying parties (Quay teams, Argo CD RBAC) key on is the
  **group name** — and the platform **realm-role** names — folded into the shared
  `groups` claim by the group-membership and realm-role mappers. The group is
  therefore the cross-service join point ([ADR-19](ADR-19.md)'s
  `Organization.spec.access[]` binds to the same group name); the per-client
  client-role mapper is the per-service join point.

```yaml
apiVersion: keycloak.holos.run/v1alpha1
kind: KeycloakGroup
metadata:
  name: my-app-editors
  namespace: my-project
spec:
  groupName: my-app-editors    # folded into the shared `groups` claim by the
                               # group-membership mapper; ADR-19
                               # Organization.spec.access[] binds to this name.
  # The client roles every member of this group holds. These reach the OWNING
  # client's own token via that client's per-client client-role mapper (as the
  # quay client does today); they are NOT carried into other clients' tokens.
  # Cross-service relying parties key on `groupName` above, not on these.
  clientRoles:
    - clientRef: my-app
      role: editor
  # The custodian model (ADR-3): who approves membership requests for this
  # group. The exact mechanism — a custodian reference here vs. a separate
  # membership-request resource — is an open question below.
  custodians:
    - group: my-project-admins
status:
  observedGeneration: 1
  groupName: my-app-editors
  conditions:
    - type: Ready
      status: "True"
      reason: Provisioned
```

### Relationship to the existing `keycloak-config-cli` reconciliation

This is the boundary [ADR-18](ADR-18.md) defers to this ADR. The existing
`keycloak-config-cli` Job
([keycloak-clients.md](../../holos/docs/keycloak-clients.md)) reconciles the
**platform's own** realm objects — the `argocd`/`quay`/`kargo` clients, the
`platform-owner`/`platform-editor`/`platform-viewer` realm roles, the
`authenticated` default group, the three `groups`-claim protocol mappers, and the
two superuser realm users — on every `scripts/apply`. Its managed-import behavior
is **no-delete**: realm objects it does not declare are left untouched.

The CRD-driven path proposed here is **additive and complementary**, not a
replacement, and the intended division of ownership is:

- **`keycloak-config-cli` keeps owning the platform's own realm.** The platform
  clients, the platform realm roles, the shared `groups`-claim mappers, the
  `authenticated` default group, and the seeded superuser users remain
  config-cli's. The CRDs do **not** redeclare or fight over these.
- **The controller owns per-project, tenant-facing objects.** A project's own
  OIDC clients, its service-scoped client roles, its project realm roles, and its
  project groups are reconciled from the CRDs above. config-cli's no-delete
  posture is what makes coexistence safe: the two reconcilers operate on disjoint
  sets of realm objects, exactly as the Job today already avoids fighting the
  `KeycloakRealmImport` CR by importing `realm: "holos"` with no `enabled` or
  `identity-provider` fields.

Whether the controller eventually **supersedes** config-cli — folding even the
platform's own clients into CRDs — or the two coexist permanently is left open
below. The decision recorded now is only the **disjoint-ownership starting
boundary**: until this ADR is promoted past `Proposed` and a controller ships,
**`keycloak-config-cli` remains the sole owner of all realm clients, roles, and
groups**, exactly as [ADR-18](ADR-18.md) states.

## Decision

1. **The Holos Controller ([ADR-18](ADR-18.md)) should eventually own a Keycloak
   API group** (working name `keycloak.holos.run`) reconciling four per-project,
   tenant-facing identity concepts: an **OIDC Client** (client ID + secret
   delivered into the project namespace), **Client Roles** (the `owner`/`editor`/
   `viewer` triad scoped to one client), **Realm Roles** (with a realm-role →
   client-role mapping), and **Group creation + membership** (a custodian-managed
   model, per [ADR-3](ADR-3.md), where authenticating against the client
   auto-assigns the client roles).
2. **This group is additive to, and disjoint from, the existing
   `keycloak-config-cli` reconciliation.** config-cli keeps owning the platform's
   own realm (the `argocd`/`quay`/`kargo` clients, the platform realm roles, the
   shared `groups`-claim mappers, the `authenticated` default group, the
   superuser users); the controller owns per-project objects. config-cli's
   no-delete managed-import is what makes the coexistence safe. This resolves the
   *starting* ownership boundary [ADR-18](ADR-18.md) deferred to this ADR; until a
   controller ships, **config-cli remains the sole owner** of all realm clients,
   roles, and groups.
3. **These resources are lower priority than the Quay CRDs ([ADR-19](ADR-19.md))
   and need further design.** The schemas above are **illustrative, not final**;
   this ADR fixes the concepts and the ownership boundary, not the field-level
   API. The status stays **`Proposed`**.
4. **This is a design record only — no code or CRD Go types are written.**

### Open questions (deferred design)

The following are explicitly **not decided** here and must be resolved before
this ADR advances past `Proposed`:

- **CRD vs continued config-cli reconciliation.** Should the controller's
  Keycloak group remain permanently disjoint from `keycloak-config-cli` (the
  starting boundary above), or eventually **supersede** it — folding even the
  platform's own clients into CRDs? What is the migration path and the
  conflict-detection mechanism if both ever target an overlapping object (akin to
  [ADR-19](ADR-19.md)'s org ownership/claim model)?
- **Client-secret delivery into project namespaces.** A confidential client's
  secret must reach the project namespace as **runtime-created, never-committed**
  material per [secret-handling.md](../../holos/docs/secret-handling.md). Does the
  reconciler generate it generate-once/create-if-absent (mirroring the platform's
  `quay-oidc` bootstrap), or project it via an `ExternalSecret`? How is rotation
  handled without breaking in-flight sessions?
- **The group-custodian authorization model (the `Updates: ADR-3` boundary).**
  [ADR-3](ADR-3.md) treats group provisioning and custodianship as external; this
  ADR proposes the controller take over provisioning the Keycloak groups and
  running the approval. How is that custodian model expressed — an inline
  `custodians` reference on the group, a separate membership-request resource a
  custodian approves, or a binding to a Kubernetes `Group` subject? And how does
  group membership translate to a member holding the group's client roles —
  through Keycloak's group → role assignment plus the per-client client-role
  mapper, reconciled once at group/membership change rather than a per-login
  write? Settling this is what would advance the `Updates: ADR-3` change past
  `Proposed`.
- **Composite realm-role → client-role mapping semantics.** Is the mapping a
  Keycloak composite role, a controller-maintained association, or a protocol
  mapper change? How does it interact with the existing realm-role mapper that
  already folds realm-role names into the shared `groups` claim?
- **Relationship to the Projects/Applications components (ADR-21).**
  Are these Keycloak resources emitted by a project's rendered manifests (like the
  Quay [ADR-19](ADR-19.md) resources), and how do they compose with the
  Project/Application component model ADR-21 defines? ADR-21 is not yet written;
  this cross-reference is forward-looking.

## Consequences

- **A second controller API group, deliberately deferred.** Committing to a
  Keycloak group sets direction without committing schema. Because the design is
  `Proposed` and lower-priority than [ADR-19](ADR-19.md), the platform continues
  to rely on `keycloak-config-cli` for **all** realm management in the interim;
  nothing changes operationally until this ADR is promoted and a controller ships.
- **The ownership boundary [ADR-18](ADR-18.md) deferred now has a starting
  answer.** The disjoint-ownership rule (config-cli owns the platform realm; the
  controller owns per-project objects) gives ADR-19's `Organization.spec.access[]`
  a clear future source for its group names and removes the ambiguity ADR-18
  flagged — without forcing a migration before the design is settled.
- **New, security-sensitive responsibilities when it ships.** A controller that
  mints OIDC clients and delivers confidential secrets into project namespaces,
  and that reconciles group membership a custodian approves, becomes a
  high-privilege component against the Keycloak admin API — analogous to the
  load-bearing superuser credential [ADR-19](ADR-19.md) carries for Quay. The
  secret-delivery and custodian-authorization open questions must be answered
  before any code, precisely because they are security-sensitive.
- **Foundation for later phases.** This ADR is the keystone the Keycloak-CRD work
  and the Project/Application component ADR (ADR-21) build on.
  Advancing it past `Proposed` is itself an ADR-level change (a new revision row)
  that settles the open questions above.
