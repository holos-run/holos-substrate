# Message Schemas via ConnectRPC Protobuf Definitions

| Metadata | Value                            |
| -------- | -------------------------------- |
| Date     | 2026-06-13                       |
| Author   | @jeffmccune                      |
| Status   | `Accepted`                       |
| Tags     | api, nats, protobuf, conventions |
| Updates  | ADR-10, ADR-13                   |

| Revision | Date       | Author      | Info           |
| -------- | ---------- | ----------- | -------------- |
| 1        | 2026-06-13 | @jeffmccune | Initial design |

## Context and Problem Statement

The MVP pipeline ([ADR-6](ADR-6.md)) moves work between stages as **messages on
NATS JetStream** ([ADR-13](ADR-13.md)): raw webhook bodies on `webhooks.>` and
normalized **render** and **deploy** tasks on `tasks.>`. Both
[ADR-10](ADR-10.md) and [ADR-13](ADR-13.md) leave a planning note to "define the
message schema" — the deployer task, the render task, their fields, and their
versioned contract — but **no ADR records *how* a message schema is specified or
what tool produces it**. Without a single answer, each stage would hand-roll Go
structs and ad-hoc JSON, the producer/consumer contract between the subscriber
and the deployer would drift, and there would be no mechanical check that a
schema change stays backward compatible across independently deployed,
at-least-once stages.

What is the platform's standard for specifying the schema of messages that flow
through NATS JetStream, and what tool produces it?

## Context / References

- [ADR-6 — MVP Heroku-Style Deployment Pipeline](ADR-6.md) (the NATS backbone)
- [ADR-9 — Webhook Receiver: Thin NATS Ingress](ADR-9.md) (publishes the raw
  webhook body verbatim; performs no parsing)
- [ADR-10 — Webhook Subscriber: Parse and Dispatch](ADR-10.md) (planning note:
  "define the deployer task message schema … a stable, versioned contract")
- [ADR-11 — Deployer Task Subscriber and the Application Resource](ADR-11.md)
  (consumes the deploy task)
- [ADR-12 — Repository Layout for Multiple Go Services](ADR-12.md) (single Go
  module; all implementation under `internal/`)
- [ADR-13 — End-to-End MVP Deployment Flow](ADR-13.md) (names the `RenderTask`
  and `DeployTask` messages and the `tasks.render` / `tasks.deploy` subjects)
- [holos/README.md — NATS JetStream backbone and connection contract](../../holos/README.md#nats-jetstream-backbone-and-connection-contract)
  (the subject hierarchy and stream definitions)
- [ConnectRPC](https://connectrpc.com/), [Protocol Buffers](https://protobuf.dev/),
  and [buf](https://buf.build/docs) — the schema toolchain
- **Prior art:** holos-console is a Go + React application built **over
  ConnectRPC** ([heroku-onramp-demo.md](../demo/heroku-onramp-demo.md)); the buf
  module layout, code generation, and lint/breaking-change checks are already an
  organization-wide convention. Recent console work (e.g. the `ResolveScopedTemplates`
  RPC) adds messages by editing `.proto` and regenerating.
- Linear: HOL-1123 (webhook subscriber — produced the first `DeployTask`),
  HOL-1124 (deployment subscriber — consumes `DeployTask`).

## Design

**The platform specifies every NATS JetStream message as a Protocol Buffers
message authored as a ConnectRPC/buf protobuf definition. The `.proto` file is
the single source of truth; Go structs are generated from it and never written
by hand.**

### Why protobuf, authored the ConnectRPC way

- **It is already the house tool.** holos-console is built on ConnectRPC, so the
  buf toolchain, generation conventions, and CI lint/breaking-change checks are
  established and understood across the organization. Adopting the same tool for
  pipeline messages reuses that muscle rather than introducing a second schema
  system.
- **Schema-first, single source of truth.** The `.proto` file *is* the contract.
  Go types are generated from it, so a change to a message is a reviewed change
  to the `.proto` — there is exactly one place a field is added, renamed, or
  deprecated, and the Go struct cannot silently diverge from it.
- **Forward/backward compatibility by construction.** Field numbers and
  protobuf's compatibility rules let a producer and a consumer deployed at
  different versions still interoperate — precisely what an at-least-once,
  independently deployed, restart-surviving pipeline ([ADR-6](ADR-6.md)) needs.
  `buf breaking` makes an incompatible change a CI failure rather than a
  production surprise.
- **A NATS message is just bytes.** Binary protobuf is the natural payload
  encoding for `tasks.*`; it is compact and self-consistent with the generated
  types. The same definitions can later back a ConnectRPC service (an API to
  enqueue or inspect tasks) with **zero schema duplication**, and can be consumed
  from other languages or CUE without re-specifying the contract.

### Source of truth and generation

- **The `.proto` is authoritative; generated Go is derived.** Generated code is
  produced with buf (`buf generate`, `protoc-gen-go`) and is never hand-edited.
  The human-edited artifact is always the `.proto`; the generated Go structs are
  a build output kept in sync by a `make generate` target and verified by a CI
  diff-clean check (the same pattern `scripts/render` uses for rendered
  manifests).
- **Layout follows [ADR-12](ADR-12.md).** The single module keeps `.proto`
  sources under `proto/holos/paas/…` with a `buf.yaml` module and `buf.gen.yaml`
  at the repository root; generated Go lands under `internal/gen/…`, consistent
  with ADR-12's rule that all implementation lives under `internal/`.
- **Versioned package.** Pipeline messages live in the package
  `holos.paas.pipeline.v1alpha1` — a versioned package so the schema can evolve;
  `v1alpha1` reflects MVP maturity. A new wire-incompatible generation becomes a
  new package version, not an in-place break.

### Encoding on the wire

- **`tasks.render` and `tasks.deploy` carry binary protobuf** — the serialized
  `RenderTask` / `DeployTask` message *is* the NATS payload. The schema version
  is carried by the proto package path; a message-type NATS header MAY be set for
  routing and observability.
- **The raw webhook stays the provider's own format.** [ADR-9](ADR-9.md) keeps
  the receiver thin: it publishes Quay's raw JSON body to `webhooks.quay`
  verbatim and parses nothing. We do not control Quay's wire format, so the
  protobuf message for the raw webhook is the **typed parse target** — the schema
  the webhook subscriber decodes the raw JSON into, behind the per-source parser
  interface ([ADR-10](ADR-10.md), HOL-1123). The proto is still the source of
  truth for *our* representation of that payload; this ADR does not change the
  `webhooks.>` payload framing decided in ADR-9.

### The MVP message set

Three messages cover the MVP's two-loop flow ([ADR-13](ADR-13.md)). The
following are illustrative sketches — the committed `.proto` is the source of
truth.

**1. Raw webhook — `QuayRepositoryPush`.** The typed model of Quay's
`repository_push` notification ([ADR-8](ADR-8.md)), the parse target for the raw
`webhooks.quay` body.

```proto
// QuayRepositoryPush is our typed view of Quay's repository_push notification.
// The raw JSON body is what travels on webhooks.quay (ADR-9); the subscriber
// parses it into this message behind the per-source parser interface (ADR-10).
message QuayRepositoryPush {
  string name = 1;                  // repository short name, e.g. "sample-app"
  string repository = 2;            // "<namespace>/<name>", e.g. "holos/sample-app"
  string namespace = 3;             // Quay namespace/organization
  string docker_url = 4;            // "<registry>/<namespace>/<name>"
  string homepage = 5;              // human-facing repository URL
  repeated string updated_tags = 6; // tags pushed in this event
}
```

**2. `RenderTask` — loop 1, published on `tasks.render`.** Emitted when an event
repository matches an `Application`'s **app-image** repository
([ADR-13](ADR-13.md)). Carries the application identity, the app-image
repository and tag, an idempotency key derived from the source event, and source
metadata.

```proto
message RenderTask {
  ApplicationRef application = 1;   // matched Application (name/namespace)
  string repository = 2;            // normalized "<registry>/<namespace>/<repo>"
  string tag = 3;                   // pushed app-image tag, e.g. "v2"
  string idempotency_key = 4;       // derived from the source delivery
  string source = 5;                // event source, e.g. "quay"
  google.protobuf.Timestamp received_at = 6;
}
```

**3. `DeployTask` — loop 2, published on `tasks.deploy`.** Emitted when an event
repository matches an `Application`'s **configuration** repository
([ADR-13](ADR-13.md)). Carries the matched application identity, the
configuration repository, the tag, the **resolved immutable digest**, the
idempotency key, and source metadata.

```proto
message DeployTask {
  ApplicationRef application = 1;   // matched Application (name/namespace)
  string config_repository = 2;     // normalized "<registry>/<namespace>/<repo>-config"
  string tag = 3;                   // config-image tag (mirrors the app tag)
  string digest = 4;                // resolved "sha256:…" — the value deployed
  string idempotency_key = 5;       // derived from the source delivery
  string source = 6;                // event source, e.g. "quay"
  google.protobuf.Timestamp received_at = 7;
}

// ApplicationRef identifies the Application resource a task targets.
message ApplicationRef {
  string name = 1;
  string namespace = 2;
}
```

These fields satisfy the contracts ADR-13 and HOL-1123 call for: application
identity, repository, tag, digest (the immutable value loop 2 deploys), an
idempotency key for at-least-once redelivery, and source-event metadata. The
shared `ApplicationRef` keeps identity consistent across both task types.

### Extensibility

- **New webhook source** (e.g. GitHub): add a new source message and a parser
  behind the per-source interface; the task messages and `tasks.>` subjects are
  unchanged.
- **New task type:** add a new message and a `tasks.<type>` subject. The `TASKS`
  stream already captures `tasks.>` ([ADR-6](ADR-6.md)), so no stream
  re-provisioning is required.

## Decision

1. **Protocol Buffers, authored as ConnectRPC/buf protobuf definitions, is the
   platform standard** for the schema of every message that flows through NATS
   JetStream.
2. **The `.proto` file is the single source of truth.** Go structs are
   **generated** from it with buf (`protoc-gen-go`) and are never hand-edited;
   the generated code is kept in sync by `make generate` and a CI diff-clean
   check.
3. **`tasks.*` payloads are binary protobuf.** The raw webhook remains the
   provider's JSON on `webhooks.quay` ([ADR-9](ADR-9.md)), with a protobuf
   message as the typed parse target; this ADR does not change ADR-9's framing.
4. Messages live in the versioned package **`holos.paas.pipeline.v1alpha1`**,
   with `.proto` under `proto/` and generated Go under `internal/gen/`, per
   [ADR-12](ADR-12.md).
5. The MVP message set is **`QuayRepositoryPush`, `RenderTask`, and
   `DeployTask`**. Schema evolution follows protobuf compatibility rules,
   enforced by `buf breaking` in CI.
6. This **resolves the "define the message schema" planning notes** in
   [ADR-10](ADR-10.md) and [ADR-13](ADR-13.md).

## Consequences

- A **new build dependency**: the buf toolchain and a code-generation step in the
  Makefile and CI, plus a generated-code-in-sync check. This is the same class of
  check the repo already runs for rendered manifests.
- The subscriber↔deployer contract becomes a **reviewable, breaking-change-checked
  artifact** rather than tribal knowledge or a prose schema doc. HOL-1123's
  suggestion to check in a hand-written DeployTask schema document is
  **superseded** — the `.proto` is the schema, and the generated docs are its
  rendering.
- Reading a message means **reading the `.proto`, not a Go struct** — a small
  indirection, offset by having one canonical definition and generated
  documentation.
- Binary payloads are **not human-readable on the wire**: `nats sub tasks.>`
  shows bytes, so debugging needs `protoc --decode` or a small decode helper.
  Acceptable, because the schema is known and versioned; the raw `webhooks.quay`
  body remains plain JSON for eyeball inspection.
- Choosing protobuf now means the same definitions can later back a **ConnectRPC
  service** (an API to enqueue or inspect tasks) and non-Go consumers with no
  re-specification — a deliberate option-value payoff, not speculative scope for
  the MVP.
