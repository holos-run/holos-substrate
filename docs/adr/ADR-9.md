# Webhook Receiver: Thin NATS Ingress

| Metadata | Value                   |
|----------|-------------------------|
| Date     | 2026-06-09              |
| Author   | @jeffmccune             |
| Status   | `Partially Implemented` |
| Tags     | webhook, nats, ingress  |
| Updates  | ADR-6                   |

| Revision | Date       | Author      | Info           |
|----------|------------|-------------|----------------|
| 1        | 2026-06-09 | @jeffmccune | Initial design |
| 2        | 2026-06-13 | @jeffmccune | Resolved the milestone planning note: subject `webhooks.<source>` on the `WEBHOOKS` WorkQueue stream, raw body as payload with a curated header allowlist as NATS headers, ack-after-`PubAck` semantics (`202`/`503`), and edge auth deferred to the subscriber (HOL-1200). Receiver implemented (HOL-1196) and deployed (HOL-1198). |

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

> **Milestone resolution (revision 2):** the M3 planning note is resolved as
> follows; the receiver is implemented in HOL-1196 and deployed in HOL-1198.
>
> - **Subject naming.** The publish subject is `<prefix>.<source>` where
>   `<prefix>` defaults to `webhooks` and `<source>` is the `{source}` path
>   segment — e.g. `POST /webhooks/quay` publishes to `webhooks.quay`. This
>   matches [ADR-13](ADR-13.md)'s `webhooks.quay` producer subject; the
>   narrower `webhooks.raw.<source>` sketched in the M0 notes is **not** used,
>   because it predates ADR-13 and does not match ADR-13's `webhooks.quay`
>   producer subject — the receiver and subscriber must agree on a single
>   subject. (The `WEBHOOKS` stream's `webhooks.>` capture subject would match
>   either form, since terminal `>` matches all remaining tokens; the
>   constraint is the producer/consumer subject agreement, not the stream
>   capture.)
> - **Stream configuration.** The subject is backed by the `WEBHOOKS`
>   file-backed **WorkQueue** stream (`webhooks.>`) provisioned by the NATS
>   `nats-stream-bootstrap` Job ([ADR-6](ADR-6.md)). Explicit size/age limits,
>   the duplicate window, and the consumer configuration are owned by the NATS
>   backbone and subscriber issues — see the
>   [stream definitions](../../holos/README.md#nats-jetstream-backbone-and-connection-contract).
> - **Body + headers framing.** The raw request body is published verbatim as
>   the NATS message payload (no parsing). A small, provider-agnostic allowlist
>   of HTTP headers is copied onto the message as NATS headers
>   (`Content-Type`, `X-Github-Event` / `X-Event-Type`,
>   `X-Github-Delivery` / `X-Delivery-Id`,
>   `X-Hub-Signature-256` / `X-Signature`); only headers present on the request
>   are forwarded. The signature headers are carried through so the chosen
>   verification location can authenticate the sender against the raw body.
> - **Ack semantics.** The receiver acks the sender with `202 Accepted` **only
>   after** the JetStream `PubAck`, and returns `503 Service Unavailable` when
>   the publish fails or NATS is unreachable. A `5xx` makes the sender (the
>   registry) retry, so an accepted event is never silently dropped — durability
>   is owned by the file-backed WorkQueue stream and the receiver's contract is
>   "`202` means stored". An oversized body is rejected with `413` before any
>   publish.
> - **Edge auth.** Signature verification is **deferred to the subscriber**
>   ([ADR-10](ADR-10.md)) for the MVP: the receiver performs no authentication
>   and carries the signature header through verbatim. Until verification lands
>   the endpoint relies on network controls — it is reachable only at
>   `hooks.holos.localhost` (→ `127.0.0.1`) behind the ambient mesh, never
>   exposed off the local cluster — plus the configurable max-body-size bound.
>   Moving verification to the edge (reject forged senders before publishing) is
>   tracked as [HOL-1200](https://linear.app/holos-run/issue/HOL-1200) and
>   stubbed in
>   [placeholders.md](../../holos/docs/placeholders.md#webhook-edge-signature-verification).

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
  any) must happen either at the edge or in the subscriber. The milestone chose
  the subscriber for the MVP (revision 2): the receiver is unauthenticated and
  relies on network controls until edge verification lands ([HOL-1200](https://linear.app/holos-run/issue/HOL-1200)).
- A WorkQueue stream gives exactly-one-consumer semantics, which is correct for
  the MVP but means scaling the subscriber requires JetStream consumer groups
  rather than naive fan-out.
