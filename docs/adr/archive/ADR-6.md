# MVP Heroku-Style Deployment Pipeline

> **Archived (PaaS era).** This ADR records a decision made for the Holos PaaS
> prototype and was archived during the Holos Substrate rebrand. It is kept for the
> historical record; see the [active decision log](../README.md)
> for the ADRs that govern the substrate.

| Metadata | Value                         |
| -------- | ----------------------------- |
| Date     | 2026-06-09                    |
| Author   | @jeffmccune                   |
| Status   | `Deprecated`                  |
| Tags     | pipeline, mvp, nats, deployer |

| Revision | Date       | Author      | Info                                                                                             |
| -------- | ---------- | ----------- | ------------------------------------------------------------------------------------------------ |
| 1        | 2026-06-09 | @jeffmccune | Initial design                                                                                   |
| 2        | 2026-06-12 | @jeffmccune | Refined by [ADR-13](ADR-13.md): end-to-end two-loop flow; render & publish becomes a sixth stage |
| 3        | 2026-06-14 | @jeffmccune | Deprecated; superseded by [ADR-16](ADR-16.md). The six-stage in-cluster NATS pipeline is replaced by a client-side CLI build-and-publish (ORAS) workflow plus Kargo-driven promotion |

> **Deprecated — superseded by [ADR-16](ADR-16.md).** The six-stage in-cluster
> NATS JetStream pipeline described below is no longer the MVP deployment path.
> Rendering and publishing move client-side (a CLI build-and-publish ORAS
> workflow), and Kargo watches the registry and patches the Argo CD `Application`.
> This document is kept for the historical record.

## Context and Problem Statement

The platform's first milestone is a **minimum viable Heroku experience**: a
developer (or a coding agent acting on their behalf) builds an application,
pushes a container image, and the new version is deployed — without operating
any bespoke deployment machinery by hand. What is the end-to-end shape of that
pipeline for the MVP, and how do its stages communicate?

This ADR establishes the pipeline as a whole and the event-driven backbone it
runs on. The individual stages are specified in their own ADRs so each can carry
its own status and revision history.

## Context / References

- [ADR-2 — Core Platform Principles](../ADR-2.md) — the KRM is the primary API; the
  deployer ultimately reconciles a custom resource.
- [ADR-1 — Project Resource](ADR-1.md) — the tenant that owns an application and
  its deployments.
- Per-stage ADRs that refine this one:
  - [ADR-7 — KubeRay Reference Workload on k3d](ADR-7.md)
  - [ADR-8 — Container Registry and Image Tagging](ADR-8.md)
  - [ADR-9 — Webhook Receiver: Thin NATS Ingress](ADR-9.md)
  - [ADR-10 — Webhook Subscriber: Parse and Dispatch](ADR-10.md)
  - [ADR-11 — Deployer Task Subscriber and the Application Resource](ADR-11.md)
- [ADR-13 — End-to-End MVP Deployment Flow: Two Registry-Event Loops](ADR-13.md)
  — records the complete flow this pipeline carries, inserting a **render &
  publish** stage between parse and deploy (six stages total)
- [NATS JetStream](https://docs.nats.io/nats-concepts/jetstream)
- [JetStream WorkQueue retention](https://docs.nats.io/nats-concepts/jetstream/streams#retentionpolicy)
- [Research: Handling Image-Tag Updates in Argo CD with an OCI Manifest Source](../../research/argocd-oci-image-tag-updates.md)
  (how the terminal stage reaches Argo CD without Git)

## Design

The MVP pipeline has six stages, each owned by its own ADR. The end-to-end
flow they carry — two registry-event loops connected by the configuration
image push — is recorded in [ADR-13](ADR-13.md):

1. **Build a sample app** ([ADR-7](ADR-7.md)) — a real workload (KubeRay) built
   with a multi-stage container build, runnable on k3d on Apple Silicon. This is
   the reference workload that exercises the pipeline end to end.
2. **Push the container to a registry** ([ADR-8](ADR-8.md)) — the image is
   pushed with a tag; the tag *is* the version that gets deployed. The registry
   emits a webhook when a new tag is pushed.
3. **Webhook receiver** ([ADR-9](ADR-9.md)) — a thin HTTP endpoint that does no
   parsing. It writes the raw webhook body to a NATS subject backed by a durable,
   file-backed **WorkQueue** stream and acknowledges the sender immediately.
4. **Webhook subscriber** ([ADR-10](ADR-10.md)) — consumes raw webhook events,
   parses them, matches the event repository against `Application` resources,
   and publishes a single well-known task message: a **render task** for an
   application-image push or a **deployer task** for a configuration-image
   push ([ADR-13](ADR-13.md)).
5. **Render subscriber** ([ADR-13](ADR-13.md)) — consumes render tasks, renders
   the platform CUE with the new tag injected, and publishes the rendered
   manifests as an OCI **configuration image**; that push re-enters the
   pipeline through stage 3 as the deploy trigger.
6. **Deployer task subscriber** ([ADR-11](ADR-11.md)) — consumes deployer task
   messages and, for the MVP, updates an `Application` resource to the new
   configuration-image version. Reconciling the cluster to that resource — for
   the MVP, patching the Argo CD `Application`'s `targetRevision` — is the
   controller's job.

The stages communicate through **NATS JetStream**, not direct calls. The seam
between every stage is a durable subject:

- Ingress is decoupled from processing, so a burst of webhooks or a slow
  downstream never blocks the registry's webhook delivery.
- Each stage can be developed, deployed, scaled, and retried independently.
- A durable, file-backed stream means an in-flight deployment survives a restart
  of any single component.

The **WorkQueue** retention policy is chosen deliberately for the receiver's
stream: each raw webhook is delivered to exactly one consumer and removed once
acknowledged, giving at-least-once processing with no unbounded backlog of
already-handled events.

This ADR intentionally **defers** the production-grade concerns to the per-stage
ADRs and to follow-up work, notably: Git write-back of desired state
([ADR-11](ADR-11.md), [ADR-13](ADR-13.md)), separation-of-duty controls for
production promotion ([ADR-11](ADR-11.md)), and per-user feedback backpressure
on the webhook body ([ADR-10](ADR-10.md)).

## Decision

1. The MVP delivers a **minimum viable Heroku experience** as a six-stage
   pipeline: build → push → receive → parse/route → render & publish → deploy
   ([ADR-13](ADR-13.md)).
2. **NATS JetStream is the communication backbone** between stages; stages are
   decoupled through durable subjects rather than direct synchronous calls.
3. The webhook ingress stream uses a **durable, file-backed WorkQueue** stream so
   events are processed at-least-once and survive restarts.
4. For the MVP the terminal stage **updates an `Application` resource**;
   reconciling actual cluster state to that resource is the controller's
   responsibility ([ADR-11](ADR-11.md)).
5. Production concerns (Git write-back, separation of duty, body
   backpressure) are **deferred** and tracked in the per-stage ADRs.

## Consequences

- The platform gains a clear, event-driven deployment path that a coding agent
  can drive end to end: push a tag, get a deploy.
- A NATS JetStream deployment becomes an operational dependency of the platform;
  it must be provisioned, monitored, and backed by durable storage.
- Because stages are decoupled, failures are isolated and retryable, but the
  system is eventually consistent: "pushed" and "deployed" are distinct
  observable states with lag between them.
- The MVP deliberately stops at updating the `Application` resource; the
  controller and Argo CD carry it to a running deployment ([ADR-13](ADR-13.md)).
  The gap between that and a Git-backed, separation-of-duty-gated production
  deployment is real and is carried as deferred work in [ADR-11](ADR-11.md).
