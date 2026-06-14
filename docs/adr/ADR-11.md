# Deployer Task Subscriber and the Application Resource

| Metadata | Value                 |
| -------- | --------------------- |
| Date     | 2026-06-09            |
| Author   | @jeffmccune           |
| Status   | `Deprecated`          |
| Tags     | api, deployer, gitops |
| Updates  | ADR-6                 |

| Revision | Date       | Author      | Info                                                                                                                                   |
| -------- | ---------- | ----------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| 1        | 2026-06-09 | @jeffmccune | Initial design                                                                                                                         |
| 2        | 2026-06-12 | @jeffmccune | Refined by [ADR-13](ADR-13.md): OCI rendered-manifests delivery moves into the MVP; only Git write-back and SoD gating remain deferred |
| 3        | 2026-06-14 | @jeffmccune | Deprecated by [ADR-16](ADR-16.md). The NATS deployer task subscriber is not used / deferred, eschewed in favor of the client-side CLI build-and-publish ORAS workflow + Kargo: a Kargo `argocd-update` promotion step patches the Argo CD `Application` `targetRevision`. The `Application`-as-deploy-target concept survives the pivot |

> **Deprecated — see [ADR-16](ADR-16.md).** The NATS **deployer task subscriber**
> described below is **not used / deferred** for the MVP, eschewed in favor of the
> client-side CLI build-and-publish (ORAS) workflow plus Kargo. The
> `Application`-as-deploy-target concept **survives** the pivot: the deployed truth
> is still an Argo CD `Application` whose `targetRevision` selects the
> rendered-manifests artifact by digest. What changes is **who patches it** — a
> Kargo `argocd-update` promotion step, not this NATS subscriber. This document is
> kept for the historical record.

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
**updates an `Application` resource** to the new version — the
configuration-image version produced by the render stage
([ADR-13](ADR-13.md)). The `Application` is a custom resource
([ADR-2](ADR-2.md)) owned by a `Project` ([ADR-1](ADR-1.md)); writing the new
version to its `spec` makes that version the desired state, and the
controller's reconcilers drive the cluster to match — for the MVP by patching
the Argo CD `Application`'s **`targetRevision`** to the new artifact digest,
so Argo CD syncs the rendered manifests from the **OCI artifact** (Argo CD
native OCI source, no Git write-back), per the research
([report](../research/argocd-oci-image-tag-updates.md)) and
[ADR-13](ADR-13.md). Keeping the deployer's job to "update one KRM resource"
keeps this stage small and idempotent.

Two layers are **deliberately deferred** beyond the MVP:

- **Git write-back of desired state.** The MVP renders (`holos render
  platform` in the render stage, [ADR-13](ADR-13.md)) and delivers via the OCI
  artifact, but no rendered manifests are committed anywhere. A production
  pipeline would unify *current* state into the *desired*-state config as a
  pre-step and require an **automated commit** of the rendered manifests so
  Git is the auditable source of truth. The in-cluster `targetRevision` patch
  is the MVP shortcut for that loop. **Kargo** is the chosen growth path for
  registry-watching and the separation-of-duty gate below; **Argo CD Image
  Updater is not used** with rendered manifests because its Git-free method
  updates Helm/Kustomize parameters only, not `targetRevision`.
- **Separation of duty for production promotion.** A production deploy could
  require an out-of-band approval — for example a **+1 reaction on a chat
  message** — to satisfy the separation-of-duty control before the change is
  promoted. The MVP performs no such gating.

> **Planning note for the milestone:** specify the `Application` custom resource
> (`spec`/`status`, how the image/tag version is represented, `Project`
> ownership per [ADR-1](ADR-1.md)) — likely its own ADR; the deployer's
> consumer/ack semantics and idempotency (a redelivered task must not thrash the
> resource); the reconciler that turns `Application.spec` into running workload
> (for the MVP, patching the Argo CD `Application` that delivers the KubeRay
> `RayCluster` manifests from [ADR-7](ADR-7.md); see [ADR-13](ADR-13.md)); RBAC for the
> deployer's write access scoped to the `Project`; and the conditions/events
> written back to `Application.status` for user feedback.

## Decision

1. For the MVP the deployer task subscriber **updates an `Application` resource**
   to the new configuration-image version ([ADR-13](ADR-13.md)); the controller
   reconciles the cluster to that resource by patching the Argo CD
   `Application`'s `targetRevision`.
2. The `Application` is a **KRM custom resource** ([ADR-2](ADR-2.md)) owned by a
   `Project` ([ADR-1](ADR-1.md)). Its detailed schema is to be specified in a
   follow-up ADR (see the planning note).
3. **OCI rendered-manifests delivery is MVP scope** ([ADR-13](ADR-13.md)): Argo
   CD syncs rendered manifests from an **OCI artifact** and the controller
   updates the Argo CD `Application`'s `targetRevision`, per the
   [research report](../research/argocd-oci-image-tag-updates.md) — GitHub-
   independent by construction. **Git write-back** — unify current into desired
   state and commit the rendered manifests — is **deferred** beyond the MVP.
4. **Separation-of-duty gating** for production promotion (e.g. a +1 chat
   reaction) is **deferred** beyond the MVP.

## Consequences

- The MVP closes the loop — a pushed tag becomes a running version — with the
  smallest possible terminal stage: a single KRM write.
- Modeling the deploy target as an `Application` resource keeps the platform
  Kubernetes-native ([ADR-2](ADR-2.md)) and reuses the controller's
  reconciliation, RBAC, and audit surfaces.
- Writing the resource directly bypasses Git as the source of truth; the
  deferred Git write-back loop (automated commit of the rendered manifests) is
  required before the pipeline is safe for production.
- Without separation-of-duty gating, the MVP must not be pointed at a production
  environment; promotion controls are a prerequisite for that, not an
  enhancement.
- The deployer must be idempotent because delivery is at-least-once
  ([ADR-10](ADR-10.md)); repeated tasks for the same version must converge, not
  oscillate.
