# Authorization via Kubernetes RBAC and Group Membership

| Metadata | Value                |
|----------|----------------------|
| Date     | 2026-06-06           |
| Author   | @jeffmccune          |
| Status   | `Proposed`           |
| Tags     | rbac, authz, security |
| Updates  | ADR-2                |

| Revision | Date       | Author      | Info           |
|----------|------------|-------------|----------------|
| 1        | 2026-06-06 | @jeffmccune | Initial design |

## Context and Problem Statement

The platform exposes the KRM as its primary API (see [ADR-2](ADR-2.md)).
Consumers of that API must be authenticated and authorized. How should the
platform authorize access without taking on the cost of building and operating a
second authorization system alongside Kubernetes?

## References

- [ADR-2 — Core Platform Principles](ADR-2.md)
- [Kubernetes RBAC](https://kubernetes.io/docs/reference/access-authn-authz/rbac/)
- [Kubernetes Authentication](https://kubernetes.io/docs/reference/access-authn-authz/authentication/)
- [ADR-4 — Multi-Tenancy](ADR-4.md) (tenant-scoped authorization)
- [ADR-1 — Project Resource](ADR-1.md) (the tenant authorization is scoped to)

## Design

Because the platform is Kubernetes-native, the cluster already enforces
authorization on every API request through RBAC. Introducing a separate
authorization system would duplicate that machinery and create two sources of
truth for who may do what. Instead, the platform relies on Kubernetes RBAC as
its sole authorization mechanism: platform capabilities are expressed as
resources, and access to those resources is granted with `Role`/`ClusterRole`
and the corresponding bindings.

The platform assumes its consumers already have Kubernetes API server access
(credentials that authenticate them to the cluster). Authorization is then a
matter of group membership: a consumer requests membership in the group(s) that
grant the access they need, and the relevant custodians approve the request.
Groups are mapped to RBAC roles through `RoleBinding`/`ClusterRoleBinding`
subjects of kind `Group`. This keeps day-to-day authorization changes in the
identity/group system where custodians already operate, rather than requiring
bespoke platform workflows.

## Decision

1. **Kubernetes RBAC is the authorization system for the platform.** No separate
   RBAC system is built or maintained.
2. **The platform assumes consumers have Kubernetes API server access** and may
   obtain authorization efficiently by requesting membership in the relevant
   groups, which are approved by the appropriate custodians.
3. Group membership is mapped to access through RBAC bindings whose subjects are
   `Group`s.

## Consequences

- There is a single authorization model and a single audit surface — the
  Kubernetes API server — for all platform access.
- The platform depends on the cluster's authentication layer and on an external
  identity/group system; provisioning and custodianship of groups is a
  prerequisite, not something the platform implements.
- Authorization granularity is bounded by what RBAC can express (verbs on
  resources, optionally namespaced). Designs that need finer-grained controls
  must model that within RBAC or justify, per [ADR-2](ADR-2.md), why the KRM and
  RBAC are not fit for purpose.
- Tenant-scoped authorization (see [ADR-4](ADR-4.md)) is expressed with RBAC
  roles and group bindings scoped to the `Project` ([ADR-1](ADR-1.md)); the
  precise scoping mechanism follows the `Project` implementation, which is
  deferred in [ADR-1](ADR-1.md).
