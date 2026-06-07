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
- [ADR-3 — Authorization via Kubernetes RBAC and Group Membership](ADR-3.md)
- [ADR-5 — Chargeback, Quotas, and Limits (GCP Model)](ADR-5.md)

## Design

Multi-tenancy is a first-class requirement of the platform API. Every resource
type and controller is designed so that it can be attributed to, and isolated
by, a tenant. A tenant is the unit that authorization is granted against (see
[ADR-3](ADR-3.md)) and that accounting, quotas, and limits are applied against
(see [ADR-5](ADR-5.md)); consistent tenant identity is therefore a shared
concern across these principles.

This ADR establishes the requirement. The concrete tenancy model — how a tenant
maps onto Kubernetes constructs (for example namespaces), how tenant identity is
represented on resources, and the isolation guarantees between tenants — is
specified in follow-up, resource-specific ADRs that build on this requirement.

## Decision

1. **The platform API must support multiple tenants.**
2. Multi-tenancy is a first-class design constraint: every resource type must be
   attributable to a tenant and isolable by tenant boundary.
3. The specific mapping of tenants onto Kubernetes constructs and the isolation
   guarantees are deferred to follow-up ADRs that refine this requirement.

## Consequences

- Resource designs must account for tenant identity and isolation from the
  outset; a design that cannot be scoped to a tenant is not acceptable without
  an ADR justifying the exception.
- Tenant identity is a shared contract consumed by authorization
  ([ADR-3](ADR-3.md)) and by chargeback, quotas, and limits
  ([ADR-5](ADR-5.md)); these principles must agree on how a tenant is named.
- Defining the detailed isolation model is deferred, so this ADR will be
  refined; until then "tenant" is a requirement, not yet a concrete API type.
