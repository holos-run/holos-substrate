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

> **Superseded — deployment pivoted to Kargo + a client-side CLI/ORAS publish
> workflow ([ADR-16](../adr/archive/ADR-16.md)).** The original NATS event-driven
> pipeline below (M0 backbone, M3 webhook receiver, M4 webhook subscriber, M5
> deployer) is **deferred / not used**: ADR-9/10/11/14 are now `Deprecated`,
> and in HOL-1241 the receiver/subscriber/deployer code and the
> `nats`/`webhook-receiver`/`webhook-subscriber` Holos components were removed
> from the platform. The deploy loop is now closed by `scripts/publish`
> (`make publish`) rendering + Kustomize-packaging + `oras push`ing rendered
> manifests, a Kargo `Warehouse`/`Stage` watching that artifact, and an
> `argocd-update` promotion pointing the Argo CD `Application` at the new
> digest. See [ADR-16](../adr/archive/ADR-16.md),
> [holos/docs/oci-publish-workflow.md](../../holos/docs/oci-publish-workflow.md),
> and the **Kargo Spike** milestone (HOL-1236). The M0/M3/M4/M5 sections below
> are retained for historical context only.

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
| 0 | Pipeline backbone (NATS JetStream) — **superseded by [ADR-16](../adr/archive/ADR-16.md)** | [ADR-6](../adr/archive/ADR-6.md)     | —          |
| 1 | KubeRay reference workload        | [ADR-7](../adr/archive/ADR-7.md)     | M0         |
| 2 | Registry & image tagging          | [ADR-8](../adr/archive/ADR-8.md)     | M1         |
| 3 | Webhook receiver (thin ingress) — **superseded by [ADR-16](../adr/archive/ADR-16.md)** | [ADR-9](../adr/archive/ADR-9.md)     | M0, M2     |
| 4 | Webhook subscriber (parse/dispatch) — **superseded by [ADR-16](../adr/archive/ADR-16.md)**| [ADR-10](../adr/archive/ADR-10.md)  | M3         |
| 5 | Deployer & Application resource — **superseded by [ADR-16](../adr/archive/ADR-16.md)**    | [ADR-11](../adr/archive/ADR-11.md)  | M4, M1     |

The original critical path was M0 → M2 → M3 → M4 → M5. M3/M4/M5 (the NATS
receiver → subscriber → deployer path) are **superseded** by the Kargo + CLI/ORAS
workflow ([ADR-16](../adr/archive/ADR-16.md)); the live deploy loop is the **Kargo
Spike** milestone (HOL-1236). M1 (workload) and M2 (registry) remain. The NATS
backbone (M0) was removed with the pipeline.

---

## M0 — Pipeline backbone (NATS JetStream)

> **Superseded by [ADR-16](../adr/archive/ADR-16.md) (deferred / not used).** The NATS
> JetStream backbone existed only to couple the receiver → subscriber → deployer
> pipeline, all now `Deprecated` (ADR-9/10/11/14). In HOL-1241 the `nats` Holos
> component, its WEBHOOKS/TASKS streams, and the `wait_nats()` apply gate were
> removed; deployment is now Kargo + the client-side CLI/ORAS workflow. The
> "Status: delivered" note below is historical.

**ADR:** [ADR-6 — MVP Heroku-Style Deployment Pipeline](../adr/archive/ADR-6.md)
**Goal:** the event-driven substrate every other milestone plugs into.

**Status: delivered, then retired (HOL-1241).** The `nats` component rendered
a file-backed JetStream `StatefulSet` and a bootstrap `Job` that created the
two WorkQueue streams, integrated into `scripts/apply` with a `wait_nats()`
gate (HOL-1192, HOL-1193). The subject naming convention followed
[ADR-13](../adr/archive/ADR-13.md) (`webhooks.quay` on `WEBHOOKS`, `tasks.render` /
`tasks.deploy` on `TASKS`). All of this was removed in HOL-1241 — see the
superseded note above.

**Work that landed:**

- Stood up a single-replica NATS JetStream `StatefulSet` usable from k3d,
  with file-backed persistence on a local-path PVC.
- Subject/stream naming convention decided in [ADR-13](../adr/archive/ADR-13.md) and
  documented as the platform contract in `holos/README.md`.
- Established the durable, file-backed **WorkQueue** streams `WEBHOOKS`
  (`webhooks.>`) and `TASKS` (`tasks.>`), created idempotently by the
  bootstrap Job.
- Connection contract (`nats://nats.nats.svc.cluster.local:4222`) documented;
  the MVP posture was **no in-cluster authentication**.
- Brought up by `scripts/apply` (no separate `make` target needed — the
  platform shares one apply path).

**Acceptance criteria (historical):**

- A message published to the ingress subject survives a broker restart.
- A single consumer receives each message exactly once and removes it on ack.

Both were verified live on the k3d-holos cluster in HOL-1193 before the NATS
pipeline was retired in HOL-1241.

---

## M1 — KubeRay reference workload

**ADR:** [ADR-7 — KubeRay Reference Workload on k3d](../adr/archive/ADR-7.md)
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

**ADR:** [ADR-8 — Container Registry and Image Tagging](../adr/archive/ADR-8.md)
**Goal:** publish the image with a tag (the tag *is* the version) so a new
version is publishable and pullable.

> **Note (HOL-1241):** the original plan made a tag push *observable via a
> webhook* feeding the NATS receiver. That trigger path was superseded by
> [ADR-16](../adr/archive/ADR-16.md): the client-side `scripts/publish` workflow renders
> and `oras push`es the manifests artifact, and a Kargo `Warehouse` watches the
> registry for new artifacts. The registry/tagging substrate below stands; the
> webhook trigger does not.

**Planning hints / work to flesh out:**

- Choose the registry (local registry wired into k3d, and/or GHCR/Harbor).
- Define the tagging convention (prefer immutable tags; SHA/semver/build-no.).
- Wire push auth (build step) and pull auth (k3d/KubeRay).

**Acceptance criteria:**

- Pushing a new tag is publishable and the artifact is discoverable (now by a
  Kargo `Warehouse`, per [ADR-16](../adr/archive/ADR-16.md)).
- k3d can pull the pushed image with the configured credentials.

---

## M3 — Webhook receiver (thin NATS ingress)

> **Superseded by [ADR-16](../adr/archive/ADR-16.md) (deferred / not used).** ADR-9 is
> `Deprecated`; the `webhook-receiver` subcommand, its HTTP handler, and the
> `webhook-receiver` Holos component were removed in HOL-1241. There is no
> inbound webhook ingress in the MVP — deployment is driven by the client-side
> `scripts/publish` workflow and Kargo. Retained below for historical context.

**ADR:** [ADR-9 — Webhook Receiver: Thin NATS Ingress](../adr/archive/ADR-9.md)
**Goal:** a thin HTTP endpoint that writes the **raw** webhook body to the NATS
WorkQueue stream and acks the sender — no parsing.

**Status: delivered, then retired (HOL-1241).** The webhook receiver was
implemented as the `webhook-receiver` subcommand of the `holos-paas` binary
(`internal/webhook/receiver/`, HOL-1196), shipped as an arm64 distroless image
(HOL-1197), and deployed as the `webhook-receiver` Holos component on the shared
Gateway at `hooks.holos.internal` (HOL-1198). It published the raw body to
`webhooks.<source>` on the `WEBHOOKS` WorkQueue stream. The subcommand, its
package, and the component were removed in HOL-1241 — see the superseded note
above.

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
`hooks.holos.internal` through the shared Gateway, and its in-cluster ClusterIP
Service has no ingress policy (consistent with the no-in-cluster-auth posture).
Edge signature verification is a deferred future enhancement
([HOL-1200](https://linear.app/holos-run/issue/HOL-1200)).

---

## M4 — Webhook subscriber (parse & dispatch)

> **Superseded by [ADR-16](../adr/archive/ADR-16.md) (deferred / not used).** ADR-10 is
> `Deprecated`; the `webhook-subscriber` subcommand, its `internal/` packages,
> and the `webhook-subscriber` Holos component were removed in HOL-1241. Parsing
> a registry push into a deploy is now the client-side `scripts/publish`
> workflow feeding a Kargo `Warehouse`. Retained below for historical context.

**ADR:** [ADR-10 — Webhook Subscriber: Parse and Dispatch](../adr/archive/ADR-10.md)
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

> **Superseded by [ADR-16](../adr/archive/ADR-16.md) (deferred / not used).** ADR-11 is
> `Deprecated`; the deployer task subscriber was never built and the NATS path
> was retired in HOL-1241. A Kargo `Stage` promotion now runs `argocd-update` to
> set the Argo CD `Application`'s `targetRevision` to the published Freight
> digest. Retained below for historical context.

**ADR:** [ADR-11 — Deployer Task Subscriber and the Application Resource](../adr/archive/ADR-11.md)
**Goal:** consume deployer tasks and update the `Application` resource to the new
version; the controller reconciles the cluster to it.

**Planning hints / work to flesh out:**

- Specify the `Application` custom resource (`spec`/`status`, version
  representation, `Project` ownership per [ADR-1](../adr/archive/ADR-1.md)) — likely its
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

- **User-feedback body queue** with a quantity-based limit ([ADR-10](../adr/archive/ADR-10.md)).
- **GitOps production pipeline:** current→desired unification, `holos render
  platform`, automated commit ([ADR-11](../adr/archive/ADR-11.md)).
- **Separation-of-duty control** for production promotion ([ADR-11](../adr/archive/ADR-11.md)).

## Definition of done (MVP demo)

On an Apple Silicon Mac with k3d: building the KubeRay image and pushing a new
tag results — with no further manual steps — in the controller reconciling the
KubeRay workload to that version, demonstrably driven by a coding agent.
