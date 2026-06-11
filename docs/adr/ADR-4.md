# Multi-Tenancy

| Metadata | Value                |
|----------|----------------------|
| Date     | 2026-06-06           |
| Author   | @jeffmccune          |
| Status   | `Proposed`           |
| Tags     | api, multi-tenancy   |
| Updates  | ADR-2                |

| Revision | Date       | Author      | Info           |
|----------|------------|-------------|----------------|
| 1        | 2026-06-06 | @jeffmccune | Initial design |

## Context and Problem Statement

The platform serves more than one consumer organization from shared
infrastructure. If multi-tenancy is treated as something to add later, tenant
boundaries leak into resource designs that did not anticipate them and become
expensive to retrofit. Should the platform API support multiple tenants, and is
that a first-class requirement?

## References

- [ADR-2 — Core Platform Principles](ADR-2.md)
- [ADR-1 — Project Resource](ADR-1.md) (the tenant model adopted here)
- [ADR-3 — Authorization via Kubernetes RBAC and Group Membership](ADR-3.md)
- [ADR-5 — Chargeback, Quotas, and Limits (GCP Model)](ADR-5.md)

## Design

Multi-tenancy is a first-class requirement of the platform API. Every resource
type and controller is designed so that it can be attributed to, and isolated
by, a tenant. The tenant is the unit that authorization is granted against (see
[ADR-3](ADR-3.md)) and that accounting, quotas, and limits are applied against
(see [ADR-5](ADR-5.md)); consistent tenant identity is therefore a shared
concern across these principles.

The platform's tenant model is the **`Project`** resource defined in
[ADR-1](ADR-1.md), adopted directly from the GCP Project resource. This ADR
establishes the multi-tenancy requirement and adopts the `Project` as the tenant
model; the concrete Kubernetes implementation — whether the `Project` is
cluster-scoped or namespace-scoped, how it maps onto namespaces, and the
isolation guarantees between Projects — is deferred to [ADR-1](ADR-1.md) and its
follow-ups.

## Decision

1. **The platform API must support multiple tenants.**
2. **The tenant model is the `Project` resource ([ADR-1](ADR-1.md)), adopted
   from the GCP Project.** Multi-tenancy is a first-class design constraint:
   every resource type must be attributable to a `Project` and isolable by
   `Project` boundary.
3. The Kubernetes implementation of the `Project` (resource scope, namespace
   mapping, and isolation guarantees) is deferred to [ADR-1](ADR-1.md).

## Consequences

- Resource designs must account for tenant identity and isolation from the
  outset; a design that cannot be scoped to a `Project` is not acceptable without
  an ADR justifying the exception.
- Tenant identity is a shared contract consumed by authorization
  ([ADR-3](ADR-3.md)) and by chargeback, quotas, and limits
  ([ADR-5](ADR-5.md)); all three resolve "tenant" to the `Project`
  ([ADR-1](ADR-1.md)).
- The tenant is now a concrete resource — the `Project` — but its detailed
  isolation model and Kubernetes scope are deferred to [ADR-1](ADR-1.md), so both
  ADRs will be refined before the resource is built.
