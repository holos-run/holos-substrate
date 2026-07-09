# Core Platform Principles

| Metadata | Value           |
| -------- | --------------- |
| Date     | 2026-06-06      |
| Author   | @jeffmccune     |
| Status   | `Accepted`      |
| Tags     | api, principles |

| Revision | Date       | Author      | Info           |
| -------- | ---------- | ----------- | -------------- |
| 1        | 2026-06-06 | @jeffmccune | Initial design |

## Context and Problem Statement

The platform needs a small set of foundational principles that every later
design decision can be measured against. Without them, decisions about the API
surface, authorization, tenancy, and billing drift apart and contributors have
no shared criteria for evaluating proposals. What principles govern the design
of the platform's API?

This ADR establishes the core principles. It fully decides the API strategy
(the Kubernetes Resource Model as the primary API) and adopts the remaining
principles, each of which is specified in its own ADR so it can evolve and be
tracked independently.

## References

- [Kubernetes API Concepts](https://kubernetes.io/docs/reference/using-api/api-concepts/)
- [Kubernetes Resource Model (KRM)](https://github.com/kubernetes/design-proposals-archive/blob/main/architecture/resource-management.md)
- Related principle ADRs, which refine and depend on the decisions here:
  - [ADR-3 — Authorization via Kubernetes RBAC and Group Membership](ADR-3.md)
  - [ADR-4 — Multi-Tenancy](archive/ADR-4.md)
  - [ADR-5 — Chargeback, Quotas, and Limits (GCP Model)](archive/ADR-5.md)

## Design

The platform is **Kubernetes-native**. The Kubernetes API and the Custom
Resource Definitions the platform installs — collectively the Kubernetes
Resource Model (KRM) — are the primary, supported interface that platform
consumers program against. Building on the KRM lets the platform reuse the
Kubernetes control plane, its declarative reconciliation model, its API
conventions, and its surrounding ecosystem (RBAC, admission, quotas, tooling)
rather than reinventing them.

Some capabilities may eventually need an interface other than the KRM (for
example a web console, a CLI, or a reporting API). Such interfaces are permitted
only after the KRM has been eliminated as a viable solution. Because the KRM
evolves, that elimination must be written down so it can be revisited: any ADR
proposing an alternative interface must state specifically why the KRM is not
fit for the purpose, allowing the decision to be re-evaluated as the KRM grows
new capabilities.

The remaining principles are stated here as adopted tenets and specified in
detail in their own ADRs so each can carry its own status and revision history:

- **Authorization** uses Kubernetes RBAC, with access obtained through group
  membership — see [ADR-3](ADR-3.md).
- **Multi-tenancy** is a first-class requirement of the API — see
  [ADR-4](archive/ADR-4.md).
- **Chargeback, quotas, and limits** follow the GCP model — see
  [ADR-5](archive/ADR-5.md).

## Decision

1. The platform is Kubernetes-native.
2. **The Kubernetes API and custom resource definitions (collectively the KRM)
   are the primary API offered by the platform to its consumers.**
3. **Alternative interfaces are offered only after eliminating the Kubernetes
   API as a viable solution.** Any ADR proposing an alternative interface must
   specify the reasons the KRM is not fit for purpose, so the decision can be
   re-evaluated as the KRM evolves.
4. The platform additionally adopts the principles specified in
   [ADR-3](ADR-3.md) (authorization), [ADR-4](archive/ADR-4.md) (multi-tenancy), and
   [ADR-5](archive/ADR-5.md) (chargeback, quotas, and limits). These ADRs depend on and
   refine the principles established here.

## Consequences

- The platform inherits the Kubernetes control plane, API conventions, and
  ecosystem, reducing the amount of bespoke API machinery that must be built and
  maintained.
- Platform capabilities are expressed as custom resources reconciled by
  controllers; designs are constrained to what the KRM can express, which is an
  intentional forcing function.
- Alternative interfaces carry an explicit documentation burden: each must
  justify, in an ADR, why the KRM was insufficient. This keeps the door open to
  collapsing alternatives back into the KRM as it gains capabilities.
- The principles in ADR-3 through ADR-5 become binding inputs to every resource
  design; changing one of them is an ADR-level change, not an implementation
  detail.
