# Webhook Subscriber: Parse and Dispatch

| Metadata | Value                     |
| -------- | ------------------------- |
| Date     | 2026-06-09                |
| Author   | @jeffmccune               |
| Status   | `Approved`                |
| Tags     | webhook, nats, subscriber |
| Updates  | ADR-6                     |

| Revision | Date       | Author      | Info                                                                                                                                                                                                                                                                                               |
| -------- | ---------- | ----------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1        | 2026-06-09 | @jeffmccune | Initial design                                                                                                                                                                                                                                                                                     |
| 2        | 2026-06-12 | @jeffmccune | Refined by [ADR-13](ADR-13.md): the subscriber routes by KRM match, emitting a render task or a deployer task                                                                                                                                                                                      |
| 3        | 2026-06-13 | @jeffmccune | The task message schema planning note is resolved by [ADR-14](ADR-14.md): messages are ConnectRPC protobuf definitions with the `.proto` as the source of truth                                                                                                                                    |
| 4        | 2026-06-13 | @jeffmccune | Records the shipped MVP slice (HOL-1201): direct parse → `DeployTask` → publish on `tasks.deploy`, with KRM-matching routing, digest resolution, and a durable dead-letter subject explicitly deferred — see [Implemented MVP slice and deferred scope](#implemented-mvp-slice-and-deferred-scope) |

## Context and Problem Statement

The receiver ([ADR-9](ADR-9.md)) stores raw webhook bodies without
interpretation. Something must turn a raw registry webhook into an actionable
instruction for the downstream stages — the render subscriber
([ADR-13](ADR-13.md)) and the deployer ([ADR-11](ADR-11.md)). Where does
parsing happen, and what messages do those stages consume?

## Context / References

- [ADR-6 — MVP Heroku-Style Deployment Pipeline](ADR-6.md)
- [ADR-9 — Webhook Receiver: Thin NATS Ingress](ADR-9.md) (produces raw events)
- [ADR-11 — Deployer Task Subscriber and the Application Resource](ADR-11.md)
  (consumes the deployer task)
- [ADR-13 — End-to-End MVP Deployment Flow: Two Registry-Event Loops](ADR-13.md)
  (the routing rules and the render task consumer)
- [ADR-8 — Container Registry and Image Tagging](ADR-8.md) (defines the payload
  shape to parse)

## Design

The subscriber consumes raw webhook events from the receiver's WorkQueue stream,
**parses** them, **matches** the event's repository against `Application`
resources, and publishes a **single well-known task message** per event
([ADR-13](ADR-13.md)):

- a *render task* (`tasks.render`) when the repository matches an
  `Application`'s application-image repository, consumed by the render
  subscriber;
- a *deployer task* (`tasks.deploy`) when it matches an `Application`'s
  configuration-image repository, consumed by the deployer
  ([ADR-11](ADR-11.md)).

This is the stage where registry-specific payloads ([ADR-8](ADR-8.md)) are
normalized into the platform's own vocabulary (which application, which image,
which tag/version).

For the MVP the subscriber **directly creates the task event and publishes
it** — there is no intermediate workflow engine, and the subscriber performs
**no rendering** ([ADR-13](ADR-13.md) keeps the slow render stage separate).
Parse, match, build the task, publish, ack.

Two simplifications are **deliberately deferred**:

- **Body-copy with a quantity-based limit for user feedback.** A future revision
  may copy the webhook body onto a separate, quantity-bounded queue so users can
  get feedback / observe recent events with backpressure. The MVP does not do
  this; it goes straight from parsed event to task.
- Any richer routing, fan-out, or per-application workflow beyond emitting one
  task per matched event.

> **Planning note for the milestone:** define the render task and deployer task
> message schemas (stable, versioned contracts: application identity, image
> reference, tag, source event metadata). The deployer task carries the
> **rendered-manifests artifact version** (digest/tag) so the controller can
> set the Argo CD `Application`'s `targetRevision` — the render stage
> *produces* that version and the config-image push event carries it back
> through this subscriber; see the
> [research report](../research/argocd-oci-image-tag-updates.md) and
> [ADR-13](ADR-13.md). Also define the task subject/stream and its retention,
> the parser for the chosen registry's payload ([ADR-8](ADR-8.md)),
> idempotency keys so a redelivered raw event does not double-dispatch, and
> the failure path for unparseable or unknown payloads (dead-letter vs.
> ack-and-drop).

## Decision

1. **Parsing happens in the subscriber**, not the receiver — the receiver stays
   thin ([ADR-9](ADR-9.md)).
2. The subscriber publishes **one well-known task message per matched event**,
   routed by KRM match ([ADR-13](ADR-13.md)): a render task for an
   application-image push, or a deployer task for a configuration-image push
   consumed by the deployer ([ADR-11](ADR-11.md)).
3. For the MVP the subscriber **directly creates and publishes the task**; no
   workflow engine sits between parse and dispatch, and no rendering happens in
   this stage.
4. The **body-copy queue with a quantity-based limit** for user feedback is
   **deferred** to a follow-up revision.

## Consequences

- Registry-specific knowledge is isolated in one place (the parser), so adding or
  changing a registry touches only the subscriber.
- The render task and deployer task messages become stable internal contracts
  between this stage and its consumers; changing them is an ADR-level change.
- Without the deferred body-copy queue, users get no rich per-event feedback in
  the MVP beyond what the deployer surfaces on the `Application` resource.
- Because delivery is at-least-once ([ADR-9](ADR-9.md)), the deployer task must
  carry an idempotency key or the deployer must be idempotent ([ADR-11](ADR-11.md)).

## Implemented MVP slice and deferred scope

The shipped subscriber (HOL-1201, code in `internal/webhook/subscriber` and the
`DeployTask` contract in `internal/task`) implements a deliberately narrow slice
of the design above: it **directly parses** each raw Quay `repo_push` event into
one `DeployTask` per pushed tag and **publishes every task to `tasks.deploy`** on
the `TASKS` stream — it does not yet perform the KRM match this ADR describes.
The `DeployTask` field table, the durability/retry story (durable consumer on
`WEBHOOKS`, ack-after-publish, bounded `Nak` redelivery, `Term`-and-log on poison
messages, and `Nats-Msg-Id` publish dedupe), and the verification steps are
documented in
[holos/README.md](../../holos/README.md#webhook-subscriber-and-deploytask-contract).

Three pieces of the eventual design are **explicitly deferred** pending the
`Application` resource and the deployer ([ADR-11](ADR-11.md)):

- **KRM-matching routing.** The split between a `RenderTask` (`tasks.render`,
  for an application-image push) and a `DeployTask` (`tasks.deploy`, for a
  configuration-image push) per [ADR-13](ADR-13.md) requires matching the
  event's repository against `Application` resources. Until that lands, the
  subscriber always emits a `DeployTask`; the `App` field is a mechanical
  last-segment derivation, not a matched `Application` identity.
- **Registry digest resolution.** Quay's `repo_push` payload carries no manifest
  digest, so the `digest` field is currently empty (`omitempty`); resolving and
  populating it is deferred.
- **Durable dead-lettering.** Poison messages are `Term`'d and logged
  (base64-encoded under `raw_base64`) rather than written to a durable
  dead-letter subject/stream. The dedicated dead-letter subject is a larger,
  ADR-scoped addition deferred beyond this phase.
- **Protobuf message schema ([ADR-14](ADR-14.md)).** The shipped slice marshals
  the `DeployTask` Go struct (`internal/task`) to **JSON** on `tasks.deploy`.
  ADR-14 (Proposed) decides messages will be ConnectRPC protobuf with the
  `.proto` as the source of truth and a binary payload; migrating the wire form
  to protobuf is deferred. Until then `internal/task/task.go` is the authoritative
  schema for the shipped slice.

None of the deferred work is a contract break: the `DeployTask` shape already
carries `digest` and a `schemaVersion`, so the deferred resolution and routing
can populate or extend tasks without bumping the schema (the protobuf migration
is a deliberate ADR-14 wire-format change, tracked there).
