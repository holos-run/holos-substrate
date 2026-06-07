# Chargeback, Quotas, and Limits (GCP Model)

| Metadata | Value                  |
|----------|------------------------|
| Date     | 2026-06-06             |
| Author   | @jeffmccune            |
| Status   | `Proposed`             |
| Tags     | api, billing, quotas   |
| Updates  | ADR-2                  |

| Revision | Date       | Author      | Info           |
|----------|------------|-------------|----------------|
| 1        | 2026-06-06 | @jeffmccune | Initial design |

## Context and Problem Statement

The platform is multi-tenant ([ADR-4](ADR-4.md)) and runs on shared
infrastructure, so tenants must be held accountable for what they consume and
prevented from exhausting shared capacity. How should the platform account for
consumption and constrain it, in a way that is accurate, timely, and not
cognitively expensive for consumers to reason about?

## References

- [ADR-2 — Core Platform Principles](ADR-2.md)
- [ADR-1 — Project Resource](ADR-1.md) (the tenant these are scoped to)
- [ADR-4 — Multi-Tenancy](ADR-4.md)
- [GCP: Working with quotas](https://cloud.google.com/docs/quotas)
- [GCP: Quotas and limits model](https://cloud.google.com/docs/quotas/overview)

## Design

The platform must support effective, near real-time chargeback accounting:
consumption is attributed to a `Project` — the platform's tenant
([ADR-1](ADR-1.md), [ADR-4](ADR-4.md)) — as it happens, so that cost can be
reported back with low latency rather than reconciled long after the fact.
Accounting supports per-tenant discounts, quotas, and limits.

For quotas and limits the platform adopts the **GCP model**, deliberately, to
reduce cognitive load: consumers and operators already reason about cloud quotas
this way, so reusing the model avoids inventing platform-specific concepts.
Concretely, the GCP model contributes:

- **Allocation quotas** — caps on the number of a resource a `Project` may hold
  concurrently (for example, how many of a given resource exist at once).
- **Rate quotas** — caps on how often an operation may be performed over a time
  window.
- **Scope** — quotas are defined per `Project` (the platform's tenant,
  [ADR-1](ADR-1.md), adopted from the GCP Project) and may be further scoped (for
  example per region), with sensible defaults that custodians can adjust per
  `Project`.
- **Adjustability** — quotas have defaults and can be increased or decreased per
  `Project` through a request/approval flow, mirroring GCP quota increase
  requests.

Per-Project discounts are applied in the chargeback accounting layer rather than
by altering quotas or limits, keeping pricing concerns separate from capacity
controls.

## Decision

1. **The platform API and resource usage must support effective, near real-time
   chargeback accounting**, with consumption attributed per `Project`
   ([ADR-1](ADR-1.md)).
2. Accounting supports **per-tenant (per-`Project`) discounts, quotas, and
   limits**.
3. **Quotas and limits follow the GCP model** (allocation quotas, rate quotas,
   per-tenant/per-scope definitions with adjustable defaults) to reduce
   cognitive load.

## Consequences

- Resource designs must emit the usage signals needed for per-tenant, near
  real-time accounting; a resource whose consumption cannot be attributed and
  metered is incomplete.
- Adopting the GCP model gives consumers a familiar mental model and gives the
  platform a ready vocabulary (allocation vs. rate quotas, scopes, quota
  adjustments) instead of a bespoke one.
- The platform must implement metering, a chargeback/pricing layer (including
  per-tenant discounts), and quota/limit enforcement; the detailed API surface
  for each is specified in follow-up ADRs that build on this decision.
- Quota and limit enforcement depends on reliable `Project` identity from
  [ADR-1](ADR-1.md) / [ADR-4](ADR-4.md) and is authorized through
  [ADR-3](ADR-3.md).
