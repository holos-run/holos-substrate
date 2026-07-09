# Project Resource

> **Archived (PaaS era).** This ADR records a decision made for the Holos PaaS
> prototype and was archived during the Holos Substrate rebrand. It is kept for the
> historical record; see the [active decision log](../README.md)
> for the ADRs that govern the substrate.

| Metadata | Value              |
| -------- | ------------------ |
| Date     | 2026-06-06         |
| Author   | @jeffmccune        |
| Status   | `Accepted`         |
| Tags     | api, multi-tenancy |

| Revision | Date       | Author      | Info           |
| -------- | ---------- | ----------- | -------------- |
| 1        | 2026-06-06 | @jeffmccune | Initial design |
| 2        | 2026-06-17 | @jeffmccune | HOL-1306: record that the deferred Kubernetes mapping of the `Project` tenant is now designed by [ADR-21](ADR-21.md) (the Holos Project component) under the GitOps rendered-manifest delivery model ([ADR-18](../ADR-18.md)); add forward cross-links. The original GCP-Project tenant decision is unchanged. |
| 3        | 2026-06-20 | @jeffmccune | HOL-1340: cross-reference how the Project tenant's `owner`/`editor`/`viewer` primitive roles are **realized in the identity system** — the `projects/<project>/roles/{owner,editor,viewer}` Keycloak groups ([ADR-20](../ADR-20.md)) whose membership flows via the OIDC `groups` claim into per-Project access ([ADR-3](../ADR-3.md)) and Quay teams ([ADR-19](../ADR-19.md)). Adds a forward cross-link to the identity realization; the GCP-model tenant decision is unchanged. |

## Context and Problem Statement

The platform must support multiple tenants ([ADR-4](ADR-4.md)). A tenant needs a
single concrete representation that everything else attaches to: authorization
([ADR-3](../ADR-3.md)) and chargeback, quotas, and limits ([ADR-5](ADR-5.md)) are
all defined per tenant. What is the platform's tenant model?

## References

- [ADR-2 — Core Platform Principles](../ADR-2.md)
- [ADR-4 — Multi-Tenancy](ADR-4.md) (the requirement this resource fulfills)
- [ADR-3 — Authorization via Kubernetes RBAC and Group Membership](../ADR-3.md)
- [ADR-5 — Chargeback, Quotas, and Limits (GCP Model)](ADR-5.md)
- [ADR-18 — The Holos Controller and the GitOps Rendered-Manifest Delivery
  Model](../ADR-18.md): the delivery model the Project mapping is realized under.
- [ADR-21 — Holos Project and Application Components](ADR-21.md): refines this
  ADR (`Updates: ADR-1`) by mapping the `Project` tenant onto Kubernetes (the
  Namespace-as-security-boundary model) via the Holos Project component — including
  the per-Project `keycloak.holos.run` resources that realize the tenant's
  primitive roles in the identity system.
- [ADR-19 — Quay API Group CRDs](../ADR-19.md): the Project's Quay `Organization`,
  whose `spec.syncedTeams[]` binds the primitive-role OIDC group claim values
  (`my-project-owner`/`-editor`/`-viewer`) to Quay teams by name.
- [ADR-20 — Keycloak API Group (`keycloak.holos.run`)](../ADR-20.md): the identity
  realization of the primitive roles — the `projects/<project>/roles/{owner,editor,viewer}`
  Keycloak groups whose membership surfaces as the per-Project OIDC `groups` claim.
- [GCP resource hierarchy](https://cloud.google.com/resource-manager/docs/cloud-platform-resource-hierarchy)
- [GCP: Creating and managing projects](https://cloud.google.com/resource-manager/docs/creating-managing-projects)

## Design

The platform's tenant is a **`Project`**, a first-class KRM resource
([ADR-2](../ADR-2.md)) whose model is adopted directly from the **GCP Project
resource**. Because [ADR-5](ADR-5.md) already adopts the GCP model for quotas and
limits, adopting the GCP Project as the tenant keeps a single, familiar mental
model across tenancy, quotas, and billing. No platform-specific analog is
introduced — the `Project` *is* the tenant.

In the GCP model, the Project is the base-level entity that owns resources and is
the unit of:

- **Ownership and isolation** — every platform resource belongs to exactly one
  `Project`; the `Project` is the boundary tenants are isolated along
  ([ADR-4](ADR-4.md)).
- **Access control** — access is granted per Project. In GCP this is IAM bound at
  the project; on this platform the same role is filled by Kubernetes RBAC and
  group membership scoped to the `Project` ([ADR-3](../ADR-3.md)). The Project adopts
  the GCP **primitive roles** `owner` / `editor` / `viewer` as its per-Project
  access tiers — see *How the primitive roles are realized in the identity system*
  below.
- **Quotas and limits** — quotas and limits are defined and enforced per
  `Project` ([ADR-5](ADR-5.md)).
- **Chargeback / billing** — consumption is accounted and charged back per
  `Project`, with per-tenant discounts applied per `Project` ([ADR-5](ADR-5.md)).
- **Identity and lifecycle** — following GCP, a Project carries a stable,
  immutable identifier distinct from a mutable, human-friendly display name, and
  a lifecycle (for example active versus deletion-requested).

This ADR establishes that the `Project` resource is the tenant model and that its
semantics follow the GCP Project. It deliberately **defers the Kubernetes
implementation design** to follow-up revisions or ADRs, including but not limited
to (the namespace-mapping and isolation questions below are now designed by
[ADR-21](ADR-21.md) under the GitOps rendered-manifest model — see Revision 2):

- whether `Project` is a **cluster-scoped or namespace-scoped** custom resource;
- the `spec`/`status` schema, including the immutable identifier versus display
  name scheme and the lifecycle states;
- how a `Project` maps onto Kubernetes constructs such as namespaces, and the
  isolation guarantees between Projects;
- whether the levels above a Project in the GCP hierarchy (folders and
  organization) are adopted;
- naming and uniqueness constraints and the admission/validation rules.

### How the primitive roles are realized in the identity system

This ADR fixes the *tenant* decision — the Project adopts the GCP primitive roles
`owner` / `editor` / `viewer` as its per-Project access tiers — and cross-references
(without re-opening the GCP-model decision) **how those roles are realized** once a
Project is rendered onto Kubernetes ([ADR-21](ADR-21.md)) under the identity model
([ADR-3](../ADR-3.md)):

- **Roles are Keycloak groups, provisioned per Project.** A Project `my-project`'s
  primitive roles are the nested Keycloak groups
  `projects/my-project/roles/{owner,editor,viewer}` reconciled by the
  `keycloak.holos.run` API group ([ADR-20](../ADR-20.md)). A person holds a primitive
  role by being a **member** of the matching `roles/<role>` group; per-role
  **custodians** (`projects/my-project/custodians/{owner,editor,viewer}`) manage
  that membership, the native realization of [ADR-3](../ADR-3.md)'s
  custodian-approved-group-membership model.
- **Membership flows to access via the OIDC `groups` claim.** Membership in a
  role group surfaces in the shared OIDC `groups` claim as the flat, project-prefixed
  value `my-project-{owner,editor,viewer}`. [ADR-20](../ADR-20.md) carries this value as
  a **client role on the consumer's own client** (so it is collision-safe across
  Projects, and because the `oidc-usermodel-client-role-mapper` is **per client**):
  the value appears in a given relying party's token only when that role is bound on
  **that** client and the client carries the role mapper. The worked example
  ([ADR-21](ADR-21.md)) binds the **Quay client**, so `my-project-owner` reaches
  Quay's token and scopes the `Organization.spec.syncedTeams[]` team mapping
  ([ADR-19](../ADR-19.md)); a different consumer (a project service's own client) gets
  the value by binding the role on **its** client. Kubernetes RBAC `Group` subjects
  ([ADR-3](../ADR-3.md)) key on whatever group/claim values the cluster's OIDC
  authenticator is configured to read — the same per-Project values, surfaced per
  the consumer's client wiring ([ADR-20](../ADR-20.md)).
- **The boundary stays single and legible.** The Project remains the unit access is
  granted per (this ADR); ADR-20 only supplies *who provisions the groups and runs
  the custodian approval*, and ADR-3's authorization model — RBAC bindings with
  `Group` subjects — is unchanged. [ADR-21](ADR-21.md) renders the per-Project
  Keycloak resources alongside the Quay `Organization`, completing the
  registration → groups → claim → access path end-to-end (see the worked example in
  [ADR-21 *End-to-end worked example*](ADR-21.md#end-to-end-worked-example-from-cue-registration-to-quay-teams)).

## Decision

1. **The platform's tenant model is a `Project` resource**, adopted directly from
   the GCP Project resource model.
2. The `Project` is the per-tenant unit of ownership and isolation
   ([ADR-4](ADR-4.md)), access control ([ADR-3](../ADR-3.md)), and quotas, limits,
   and chargeback ([ADR-5](ADR-5.md)).
3. The Kubernetes implementation of the `Project` — notably whether it is
   **cluster-scoped or namespace-scoped**, and its `spec`/`status` schema — is
   **deferred** to follow-up design.

## Consequences

- The platform has a single tenant concept, the `Project`, reused across
  authorization, quotas, limits, and chargeback, mirroring how GCP organizes
  resources and reducing cognitive load.
- The "tenant" referenced by ADR-3, ADR-4, and ADR-5 resolves to the `Project`
  resource defined here.
- Because the implementation is deferred, this ADR will be refined.
  [ADR-21](ADR-21.md) now designs the **namespace mapping and isolation** (the
  `Project` maps onto a Kubernetes Namespace acting as the security boundary,
  rendered under the GitOps model of [ADR-18](../ADR-18.md)); what remains deferred
  is the first-class resource scope/schema and the GCP-hierarchy (folder/org)
  questions, so ADR-1 stays a living record a future `Project` CRD ADR may refine.
- Adopting the GCP Project model sets expectations — an immutable ID distinct
  from a display name, a lifecycle, and per-Project isolation — that the deferred
  implementation must honor.
- The GCP **primitive roles** the Project adopts are now realized end-to-end in the
  identity system: [ADR-20](../ADR-20.md) provisions the per-Project Keycloak role and
  custodian groups, [ADR-3](../ADR-3.md) maps their membership to access, and
  [ADR-21](ADR-21.md) renders those resources per Project alongside the Quay
  `Organization` ([ADR-19](../ADR-19.md)). The tenant decision here is unchanged; those
  ADRs supply its identity realization.
