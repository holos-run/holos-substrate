# Holos PaaS MVP — Project Plan & Milestones

A project planning document for the **minimum viable Heroku experience**: a
developer or coding agent builds an app, pushes a tagged image, and the new
version is deployed automatically.

This document is structured for direct use as a **Linear project** with one
**milestone per pipeline stage**. Each milestone below maps to an Architecture
Decision Record under [`docs/adr/`](../adr/README.md), which holds the binding
design decision. This file holds the *plan* — scope, sequence, hints, and
acceptance criteria — that the decisions feed into.

- **North star:** push a tag → get a deploy, with no manual deployment steps.
- **Demo target:** runnable end to end on a single Apple Silicon Mac (k3d).
- **Backbone:** NATS JetStream couples the stages ([ADR-6](../adr/ADR-6.md)).

## How to use this with Linear

1. Create a Linear **Project** named "Holos PaaS MVP".
2. Create one **Milestone** per section below (M1–M6), in order.
3. Turn each "Planning hints / work to flesh out" bullet into an **Issue** under
   its milestone; they are intentionally sized to become tickets.
4. Link each milestone's issues back to its ADR for the rationale.
5. Use the **Dependencies** line to set milestone ordering / blocking.

> The ADRs are the source of truth for *decisions*. When work reveals a better
> decision, revise the ADR (and its revision table) rather than editing only
> this plan — see [writing-adrs.md](../adr/writing-adrs.md).

## Milestone overview

| # | Milestone                         | ADR                          | Depends on |
|---|-----------------------------------|------------------------------|------------|
| 0 | Pipeline backbone (NATS JetStream)| [ADR-6](../adr/ADR-6.md)     | —          |
| 1 | KubeRay reference workload        | [ADR-7](../adr/ADR-7.md)     | M0         |
| 2 | Registry & image tagging          | [ADR-8](../adr/ADR-8.md)     | M1         |
| 3 | Webhook receiver (thin ingress)   | [ADR-9](../adr/ADR-9.md)     | M0, M2     |
| 4 | Webhook subscriber (parse/dispatch)| [ADR-10](../adr/ADR-10.md)  | M3         |
| 5 | Deployer & Application resource    | [ADR-11](../adr/ADR-11.md)  | M4, M1     |

The critical path is M0 → M2 → M3 → M4 → M5. M1 (workload) can proceed in
parallel once M0 exists and is the input to M2 and M5.

---

## M0 — Pipeline backbone (NATS JetStream)

**ADR:** [ADR-6 — MVP Heroku-Style Deployment Pipeline](../adr/ADR-6.md)
**Goal:** the event-driven substrate every other milestone plugs into.

**Status: delivered.** The `nats` component
([`holos/components/nats/`](../../holos/components/nats/buildplan.cue)) renders
a file-backed JetStream `StatefulSet` and a bootstrap `Job` that creates the
two WorkQueue streams, integrated into `scripts/apply` with a `wait_nats()`
gate (HOL-1192, HOL-1193). The subject hierarchy, stream definitions, MVP
auth posture, and in-cluster connection contract are documented in
[`holos/README.md`](../../holos/README.md#nats-jetstream-backbone-and-connection-contract);
the subject naming convention follows [ADR-13](../adr/ADR-13.md)
(`webhooks.quay` on `WEBHOOKS`, `tasks.render` / `tasks.deploy` on `TASKS`).

**Work that landed:**

- Stood up a single-replica NATS JetStream `StatefulSet` usable from k3d,
  with file-backed persistence on a local-path PVC.
- Subject/stream naming convention decided in [ADR-13](../adr/ADR-13.md) and
  documented as the platform contract in `holos/README.md`.
- Established the durable, file-backed **WorkQueue** streams `WEBHOOKS`
  (`webhooks.>`) and `TASKS` (`tasks.>`), created idempotently by the
  bootstrap Job.
- Connection contract (`nats://nats.nats.svc.cluster.local:4222`) documented;
  the MVP posture is **no in-cluster authentication** (deferred — see
  [placeholders.md](../../holos/docs/placeholders.md#nats-in-cluster-authentication)).
- Brought up by `scripts/apply` (no separate `make` target needed — the
  platform shares one apply path).

**Acceptance criteria:**

- A message published to the ingress subject survives a broker restart.
- A single consumer receives each message exactly once and removes it on ack.

Both verified live on the k3d-holos cluster in HOL-1193; the verification
commands are in
[`holos/README.md`](../../holos/README.md#nats-jetstream-backbone-and-connection-contract).

---

## M1 — KubeRay reference workload

**ADR:** [ADR-7 — KubeRay Reference Workload on k3d](../adr/ADR-7.md)
**Goal:** a real workload (KubeRay) that exercises the pipeline end to end,
built with a multi-stage container build and runnable on Apple Silicon k3d.

**Planning hints / work to flesh out** (replace ADR-7's sketch with concrete
minimums):

- arm64 base images and the arm64 Ray runtime build.
- Multi-stage `Dockerfile` (builder → slim runtime) with an image-size budget.
- k3d cluster shape: server/agent count, exposed ports, local registry wiring.
- KubeRay operator install method + a minimal `RayCluster` manifest.
- Laptop-sized CPU/memory requests; assume CPU-only (note GPU caveats).
- A smoke test proving the Ray cluster is healthy after deploy.

**Acceptance criteria:**

- `docker build` produces an arm64 image within the size budget.
- The `RayCluster` reaches ready on k3d and passes the smoke test.

---

## M2 — Registry & image tagging

**ADR:** [ADR-8 — Container Registry and Image Tagging](../adr/ADR-8.md)
**Goal:** publish the image with a tag (the tag *is* the version) and make the
push observable via a webhook.

**Planning hints / work to flesh out:**

- Choose the registry (local registry wired into k3d, and/or GHCR/Harbor).
- Define the tagging convention (prefer immutable tags; SHA/semver/build-no.).
- Confirm the registry's webhook capability and capture its payload shape — this
  is the contract M4's parser depends on.
- Wire push auth (build step) and pull auth (k3d/KubeRay).

**Acceptance criteria:**

- Pushing a new tag is observable: the registry fires a webhook to the receiver.
- k3d can pull the pushed image with the configured credentials.

---

## M3 — Webhook receiver (thin NATS ingress)

**ADR:** [ADR-9 — Webhook Receiver: Thin NATS Ingress](../adr/ADR-9.md)
**Goal:** a thin HTTP endpoint that writes the **raw** webhook body to the NATS
WorkQueue stream and acks the sender — no parsing.

**Status: delivered.** The webhook receiver is implemented as the
`webhook-receiver` subcommand of the `holos-paas` binary
([`internal/webhook/receiver/`](../../internal/webhook/receiver/receiver.go),
HOL-1196), shipped as an arm64 distroless image (HOL-1197), and deployed as the
`webhook-receiver` Holos component on the shared Gateway at
`hooks.holos.localhost`, with the NATS `AuthorizationPolicy` extended to admit
its namespace (HOL-1198). It publishes the raw body to `webhooks.<source>` on the
`WEBHOOKS` WorkQueue stream and acks the sender only after the JetStream
`PubAck`. The ADR-9 milestone planning note (subject naming, body+headers
framing, ack semantics, edge-auth location) is resolved in
[ADR-9](../adr/ADR-9.md) revision 2; the service contract, durability story, and
unauthenticated local-only posture are documented in
[`holos/README.md`](../../holos/README.md#webhook-receiver-and-service-contract),
with end-to-end verification in
[`docs/local-cluster.md`](../local-cluster.md#verify-the-webhook-receiver).

**Original planning hints (all resolved — see ADR-9 revision 2):**

- Subject name + stream config (replicas, max age/bytes, duplicate window) →
  `webhooks.<source>` on the `WEBHOOKS` WorkQueue stream; explicit stream
  limits owned by the NATS backbone issues.
- Framing: raw body as payload, curated HTTP headers as NATS headers.
- Edge auth / signature verification → deferred to the subscriber for the MVP;
  edge verification tracked as [HOL-1200](https://linear.app/holos-run/issue/HOL-1200).
- Ack semantics: `202` only after the JetStream `PubAck`, `503` (sender retry)
  when the publish fails.

**Acceptance criteria:**

- A webhook POST is persisted to JetStream and acked before downstream runs.
- If JetStream is unavailable, the receiver returns an error so the sender retries.

Both verified live on the k3d-holos cluster in HOL-1198. For the MVP the
receiver is **unauthenticated**: from outside the cluster it is exposed only at
`hooks.holos.localhost` through the shared Gateway, and its in-cluster ClusterIP
Service has no ingress policy (consistent with the no-in-cluster-auth posture).
Edge signature verification is a deferred future enhancement
([HOL-1200](https://linear.app/holos-run/issue/HOL-1200)).

---

## M4 — Webhook subscriber (parse & dispatch)

**ADR:** [ADR-10 — Webhook Subscriber: Parse and Dispatch](../adr/ADR-10.md)
**Goal:** consume raw events, parse them, and publish one well-known **deployer
task** message to the processor subject.

**Planning hints / work to flesh out:**

- Define the deployer task schema: a stable, versioned contract (application
  identity, image reference, tag, source event metadata).
- Processor subject/stream + retention.
- Parser for the chosen registry's payload (from M2).
- Idempotency keys so a redelivered raw event does not double-dispatch.
- Failure path for unparseable/unknown payloads (dead-letter vs. ack-and-drop).
- **Deferred (track as future issues, not MVP):** body-copy queue with a
  quantity-based limit for user feedback.

**Acceptance criteria:**

- A raw registry webhook produces exactly one deployer task with correct fields.
- A redelivered raw event does not produce a duplicate deployer task.

---

## M5 — Deployer & Application resource

**ADR:** [ADR-11 — Deployer Task Subscriber and the Application Resource](../adr/ADR-11.md)
**Goal:** consume deployer tasks and update the `Application` resource to the new
version; the controller reconciles the cluster to it.

**Planning hints / work to flesh out:**

- Specify the `Application` custom resource (`spec`/`status`, version
  representation, `Project` ownership per [ADR-1](../adr/ADR-1.md)) — likely its
  own ADR.
- Deployer consumer/ack semantics and idempotency (no resource thrash on
  redelivery).
- Reconciler turning `Application.spec` into the running workload (the KubeRay
  `RayCluster` from M1).
- RBAC for the deployer's write access, scoped to the `Project`.
- Conditions/events on `Application.status` for user feedback.
- Delivery mechanism: when GitOps is introduced it must stay **GitHub-free** —
  Argo CD syncs rendered manifests from an **OCI artifact** and the deployer sets
  the Argo CD `Application`'s `targetRevision`. See
  [Research: ArgoCD OCI image-tag updates](../research/argocd-oci-image-tag-updates.md)
  (recommends native OCI + controller patch; Kargo as the growth path).
- **Deferred (track as future issues, not MVP):**
  - GitOps reconciliation — unify current → desired, run `holos render platform`
    as the main step, automated commit of rendered manifests.
  - Separation-of-duty gating for production promotion (e.g. a +1 chat reaction).

**Acceptance criteria:**

- A deployer task updates `Application.spec` to the pushed tag.
- The controller reconciles KubeRay to the new version; status reflects success.
- Re-processing the same task converges (no oscillation).

---

## Deferred beyond the MVP (backlog)

These are decided-as-deferred in the ADRs; create them as backlog issues, not
MVP milestones:

- **User-feedback body queue** with a quantity-based limit ([ADR-10](../adr/ADR-10.md)).
- **GitOps production pipeline:** current→desired unification, `holos render
  platform`, automated commit ([ADR-11](../adr/ADR-11.md)).
- **Separation-of-duty control** for production promotion ([ADR-11](../adr/ADR-11.md)).

## Definition of done (MVP demo)

On an Apple Silicon Mac with k3d: building the KubeRay image and pushing a new
tag results — with no further manual steps — in the controller reconciling the
KubeRay workload to that version, demonstrably driven by a coding agent.
