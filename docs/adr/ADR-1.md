# Project Resource

| Metadata | Value              |
| -------- | ------------------ |
| Date     | 2026-06-06         |
| Author   | @jeffmccune        |
| Status   | `Accepted`         |
| Tags     | api, multi-tenancy |

| Revision | Date       | Author      | Info           |
| -------- | ---------- | ----------- | -------------- |
| 1        | 2026-06-06 | @jeffmccune | Initial design |

## Context and Problem Statement

The platform must support multiple tenants ([ADR-4](ADR-4.md)). A tenant needs a
single concrete representation that everything else attaches to: authorization
([ADR-3](ADR-3.md)) and chargeback, quotas, and limits ([ADR-5](ADR-5.md)) are
all defined per tenant. What is the platform's tenant model?

## References

- [ADR-2 — Core Platform Principles](ADR-2.md)
- [ADR-4 — Multi-Tenancy](ADR-4.md) (the requirement this resource fulfills)
- [ADR-3 — Authorization via Kubernetes RBAC and Group Membership](ADR-3.md)
- [ADR-5 — Chargeback, Quotas, and Limits (GCP Model)](ADR-5.md)
- [GCP resource hierarchy](https://cloud.google.com/resource-manager/docs/cloud-platform-resource-hierarchy)
- [GCP: Creating and managing projects](https://cloud.google.com/resource-manager/docs/creating-managing-projects)

## Design

The platform's tenant is a **`Project`**, a first-class KRM resource
([ADR-2](ADR-2.md)) whose model is adopted directly from the **GCP Project
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
  group membership scoped to the `Project` ([ADR-3](ADR-3.md)).
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
to:

- whether `Project` is a **cluster-scoped or namespace-scoped** custom resource;
- the `spec`/`status` schema, including the immutable identifier versus display
  name scheme and the lifecycle states;
- how a `Project` maps onto Kubernetes constructs such as namespaces, and the
  isolation guarantees between Projects;
- whether the levels above a Project in the GCP hierarchy (folders and
  organization) are adopted;
- naming and uniqueness constraints and the admission/validation rules.

## Decision

1. **The platform's tenant model is a `Project` resource**, adopted directly from
   the GCP Project resource model.
2. The `Project` is the per-tenant unit of ownership and isolation
   ([ADR-4](ADR-4.md)), access control ([ADR-3](ADR-3.md)), and quotas, limits,
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
- Because the implementation (resource scope, schema, namespace mapping,
  hierarchy) is deferred, this ADR will be refined; a follow-up is required
  before the `Project` resource can be built.
- Adopting the GCP Project model sets expectations — an immutable ID distinct
  from a display name, a lifecycle, and per-Project isolation — that the deferred
  implementation must honor.
