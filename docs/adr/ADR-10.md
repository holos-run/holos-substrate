# Webhook Subscriber: Parse and Dispatch

| Metadata | Value                     |
|----------|---------------------------|
| Date     | 2026-06-09                |
| Author   | @jeffmccune               |
| Status   | `Proposed`                |
| Tags     | webhook, nats, subscriber |
| Updates  | ADR-6                     |

| Revision | Date       | Author      | Info           |
|----------|------------|-------------|----------------|
| 1        | 2026-06-09 | @jeffmccune | Initial design |

## Context and Problem Statement

The receiver ([ADR-9](ADR-9.md)) stores raw webhook bodies without
interpretation. Something must turn a raw registry webhook into an actionable
instruction for the deployer ([ADR-11](ADR-11.md)). Where does parsing happen,
and what message does the deployer consume?

## Context / References

- [ADR-6 — MVP Heroku-Style Deployment Pipeline](ADR-6.md)
- [ADR-9 — Webhook Receiver: Thin NATS Ingress](ADR-9.md) (produces raw events)
- [ADR-11 — Deployer Task Subscriber and the Application Resource](ADR-11.md)
  (consumes the deployer task)
- [ADR-8 — Container Registry and Image Tagging](ADR-8.md) (defines the payload
  shape to parse)

## Design

The subscriber consumes raw webhook events from the receiver's WorkQueue stream,
**parses** them, and publishes a **single well-known message** — a *deployer
task* — to a processor subject that the deployer ([ADR-11](ADR-11.md)) consumes.
This is the stage where registry-specific payloads ([ADR-8](ADR-8.md)) are
normalized into the platform's own vocabulary (which application, which image,
which tag/version).

For the MVP the subscriber **directly creates a deployer task event and
publishes it** — there is no intermediate workflow engine. Parse, build the
deployer task, publish, ack.

Two simplifications are **deliberately deferred**:

- **Body-copy with a quantity-based limit for user feedback.** A future revision
  may copy the webhook body onto a separate, quantity-bounded queue so users can
  get feedback / observe recent events with backpressure. The MVP does not do
  this; it goes straight from parsed event to deployer task.
- Any richer routing, fan-out, or per-application workflow beyond emitting one
  deployer task.

> **Planning note for the milestone:** define the deployer task message schema
> (a stable, versioned contract: application identity, image reference, tag,
> source event metadata). Carry enough to resolve the **rendered-manifests
> artifact version** (digest/tag), not only the app image tag, so the deployer
> can set the Argo CD `Application`'s `targetRevision` directly — see the
> [research report](../research/argocd-oci-image-tag-updates.md) and
> [ADR-11](ADR-11.md). Also define the processor subject/stream and its
> retention, the
> parser for the chosen registry's payload ([ADR-8](ADR-8.md)), idempotency keys
> so a redelivered raw event does not double-dispatch, and the failure path for
> unparseable or unknown payloads (dead-letter vs. ack-and-drop).

## Decision

1. **Parsing happens in the subscriber**, not the receiver — the receiver stays
   thin ([ADR-9](ADR-9.md)).
2. The subscriber publishes **one well-known deployer task message** to a
   processor subject consumed by the deployer ([ADR-11](ADR-11.md)).
3. For the MVP the subscriber **directly creates and publishes the deployer
   task**; no workflow engine sits between parse and dispatch.
4. The **body-copy queue with a quantity-based limit** for user feedback is
   **deferred** to a follow-up revision.

## Consequences

- Registry-specific knowledge is isolated in one place (the parser), so adding or
  changing a registry touches only the subscriber.
- The deployer task message becomes a stable internal contract between this stage
  and the deployer; changing it is an ADR-level change.
- Without the deferred body-copy queue, users get no rich per-event feedback in
  the MVP beyond what the deployer surfaces on the `Application` resource.
- Because delivery is at-least-once ([ADR-9](ADR-9.md)), the deployer task must
  carry an idempotency key or the deployer must be idempotent ([ADR-11](ADR-11.md)).
