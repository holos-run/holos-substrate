# Webhook Receiver: Thin NATS Ingress

| Metadata | Value                   |
|----------|-------------------------|
| Date     | 2026-06-09              |
| Author   | @jeffmccune             |
| Status   | `Proposed`              |
| Tags     | webhook, nats, ingress  |
| Updates  | ADR-6                   |

| Revision | Date       | Author      | Info           |
|----------|------------|-------------|----------------|
| 1        | 2026-06-09 | @jeffmccune | Initial design |

## Context and Problem Statement

A registry push emits a webhook ([ADR-8](ADR-8.md)) that must enter the pipeline
([ADR-6](ADR-6.md)). The sender (the registry) expects a fast response and will
retry or drop on slow replies. How should the platform accept webhooks without
coupling delivery latency to downstream processing?

## Context / References

- [ADR-6 — MVP Heroku-Style Deployment Pipeline](ADR-6.md)
- [ADR-8 — Container Registry and Image Tagging](ADR-8.md) (the webhook source)
- [ADR-10 — Webhook Subscriber: Parse and Dispatch](ADR-10.md) (the consumer)
- [JetStream WorkQueue retention](https://docs.nats.io/nats-concepts/jetstream/streams#retentionpolicy)
- [JetStream file storage](https://docs.nats.io/nats-concepts/jetstream/streams#storage)

## Design

The receiver is deliberately **thin**. It is an HTTP endpoint whose only job is
to take the **raw webhook body** and publish it, unmodified, to a NATS subject,
then acknowledge the sender. It performs **no parsing**, no validation of the
payload's meaning, and no business logic — every interpretation is deferred to
the subscriber ([ADR-10](ADR-10.md)).

The target subject is backed by a **durable, file-backed WorkQueue stream**:

- **Durable + file-backed** so an accepted webhook survives a receiver or broker
  restart — once the receiver has acked the registry, the event is not lost.
- **WorkQueue retention** so each raw event is delivered to exactly one consumer
  and removed on acknowledgement, giving at-least-once processing without an
  unbounded backlog.

Keeping the receiver thin means the only way it can fail is failing to persist to
JetStream; in that case it returns an error so the registry retries. This is the
narrowest possible ingress surface and the easiest stage to make highly
available.

> **Planning note for the milestone:** specify the subject name and stream
> configuration (replicas, max age/bytes, duplicate window), how the raw body and
> useful HTTP headers are framed into the NATS message (e.g. body as payload,
> headers as NATS headers), webhook authentication/signature verification at the
> edge (even though parsing is deferred, rejecting forged senders is cheap here),
> and the ack semantics the chosen registry expects.

## Decision

1. The webhook receiver is a **thin HTTP ingress** that publishes the **raw,
   unparsed body** to a NATS subject and acknowledges the sender.
2. The subject is backed by a **durable, file-backed WorkQueue stream**, giving
   at-least-once delivery to a single consumer with persistence across restarts.
3. **No parsing or business logic** happens in the receiver; all interpretation
   is deferred to the subscriber ([ADR-10](ADR-10.md)).

## Consequences

- Webhook delivery latency is decoupled from downstream processing; a slow or
  failed deployer never causes the registry to time out or retry-storm.
- The ingress surface is minimal and easy to make highly available, since it has
  almost no logic to get wrong.
- Storing raw, unauthenticated-by-meaning bodies means signature verification (if
  any) must happen either at the edge or in the subscriber; the milestone must
  choose and document where.
- A WorkQueue stream gives exactly-one-consumer semantics, which is correct for
  the MVP but means scaling the subscriber requires JetStream consumer groups
  rather than naive fan-out.
