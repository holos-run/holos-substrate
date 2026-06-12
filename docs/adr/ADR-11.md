# Deployer Task Subscriber and the Application Resource

| Metadata | Value                          |
|----------|--------------------------------|
| Date     | 2026-06-09                     |
| Author   | @jeffmccune                    |
| Status   | `Proposed`                     |
| Tags     | api, deployer, gitops          |
| Updates  | ADR-6                          |

| Revision | Date       | Author      | Info           |
|----------|------------|-------------|----------------|
| 1        | 2026-06-09 | @jeffmccune | Initial design |
| 2        | 2026-06-12 | @jeffmccune | Refined by [ADR-13](ADR-13.md): OCI rendered-manifests delivery moves into the MVP; only Git write-back and SoD gating remain deferred |

## Context and Problem Statement

The subscriber emits a deployer task message ([ADR-10](ADR-10.md)) carrying the
application and the image version to deploy. Something must act on it and make
the new version the desired state. What does the deployer do for the MVP, and how
does it relate to the KRM ([ADR-2](ADR-2.md)) and to production-grade GitOps?

## Context / References

- [ADR-6 — MVP Heroku-Style Deployment Pipeline](ADR-6.md)
- [ADR-10 — Webhook Subscriber: Parse and Dispatch](ADR-10.md) (produces the
  deployer task)
- [ADR-2 — Core Platform Principles](ADR-2.md) (the KRM is the primary API)
- [ADR-1 — Project Resource](ADR-1.md) (the tenant that owns the `Application`)
- [ADR-13 — End-to-End MVP Deployment Flow: Two Registry-Event Loops](ADR-13.md)
  — the end-to-end flow this deployer terminates; moves the OCI
  rendered-manifests delivery described below from deferred into MVP scope
- [Holos](https://holos.run/) — `holos render platform` as the GitOps render step
- [Research: Handling Image-Tag Updates in Argo CD with an OCI Manifest Source](../research/argocd-oci-image-tag-updates.md)
  — informs the deferred GitOps mechanism below

## Design

The deployer task subscriber consumes deployer task messages and, for the MVP,
**updates an `Application` resource** to the new image version. The `Application`
is a custom resource ([ADR-2](ADR-2.md)) owned by a `Project`
([ADR-1](ADR-1.md)); writing the new version to its `spec` makes that version the
desired state, and the controller's reconcilers drive the cluster to match.
Keeping the deployer's job to "update one KRM resource" keeps this stage small
and idempotent.

Two layers are **deliberately deferred** beyond the MVP:

- **GitOps reconciliation of desired state.** Rather than the deployer writing
  the resource directly, a production pipeline would unify *current* state into
  the *desired*-state config as a pre-step, run **`holos render platform`** as
  the main step, and require an **automated commit** of the rendered manifests.
  The deployer's direct write is the MVP shortcut for this loop. When this work
  is taken up it **must remain GitHub-independent for the MVP**: the research
  ([report](../research/argocd-oci-image-tag-updates.md)) concludes that Argo CD
  should sync the rendered manifests from an **OCI artifact**, and the deployer
  should update the Argo CD `Application`'s **`targetRevision`** to the new
  artifact digest — Argo CD native OCI source, no Git write-back. **Kargo** is
  the chosen growth path for registry-watching and the separation-of-duty gate
  below; **Argo CD Image Updater is not used** with rendered manifests because
  its Git-free method updates Helm/Kustomize parameters only, not
  `targetRevision`.
- **Separation of duty for production promotion.** A production deploy could
  require an out-of-band approval — for example a **+1 reaction on a chat
  message** — to satisfy the separation-of-duty control before the change is
  promoted. The MVP performs no such gating.

> **Planning note for the milestone:** specify the `Application` custom resource
> (`spec`/`status`, how the image/tag version is represented, `Project`
> ownership per [ADR-1](ADR-1.md)) — likely its own ADR; the deployer's
> consumer/ack semantics and idempotency (a redelivered task must not thrash the
> resource); the reconciler that turns `Application.spec` into running workload
> (for the MVP, the KubeRay `RayCluster` from [ADR-7](ADR-7.md)); RBAC for the
> deployer's write access scoped to the `Project`; and the conditions/events
> written back to `Application.status` for user feedback.

## Decision

1. For the MVP the deployer task subscriber **updates an `Application` resource**
   to the new image version; the controller reconciles the cluster to that
   resource.
2. The `Application` is a **KRM custom resource** ([ADR-2](ADR-2.md)) owned by a
   `Project` ([ADR-1](ADR-1.md)). Its detailed schema is to be specified in a
   follow-up ADR (see the planning note).
3. **GitOps reconciliation** — unify current into desired state, then
   `holos render platform` with an automated commit — is **deferred** beyond the
   MVP. When undertaken it stays **GitHub-independent**: Argo CD syncs rendered
   manifests from an **OCI artifact** and the deployer updates the Argo CD
   `Application`'s `targetRevision`, per the
   [research report](../research/argocd-oci-image-tag-updates.md).
4. **Separation-of-duty gating** for production promotion (e.g. a +1 chat
   reaction) is **deferred** beyond the MVP.

## Consequences

- The MVP closes the loop — a pushed tag becomes a running version — with the
  smallest possible terminal stage: a single KRM write.
- Modeling the deploy target as an `Application` resource keeps the platform
  Kubernetes-native ([ADR-2](ADR-2.md)) and reuses the controller's
  reconciliation, RBAC, and audit surfaces.
- Writing the resource directly bypasses Git as the source of truth; the deferred
  GitOps loop (`holos render platform` + automated commit) is required before the
  pipeline is safe for production.
- Without separation-of-duty gating, the MVP must not be pointed at a production
  environment; promotion controls are a prerequisite for that, not an
  enhancement.
- The deployer must be idempotent because delivery is at-least-once
  ([ADR-10](ADR-10.md)); repeated tasks for the same version must converge, not
  oscillate.
