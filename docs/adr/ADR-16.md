# Kargo-Driven Promotion with a Client-Side CLI Build-and-Publish (ORAS) Workflow

| Metadata | Value                                              |
| -------- | -------------------------------------------------- |
| Date     | 2026-06-14                                         |
| Author   | @jeffmccune                                        |
| Status   | `Approved`                                         |
| Tags     | pipeline, kargo, oci, oras, kustomize, argocd, mvp |
| Updates  | ADR-6, ADR-13                                      |

| Revision | Date       | Author      | Info           |
| -------- | ---------- | ----------- | -------------- |
| 1        | 2026-06-14 | @jeffmccune | Initial design |
| 2        | 2026-06-14 | @jeffmccune | Note the NATS pipeline retirement landed (HOL-1241): receiver/subscriber code, the pipeline protobuf, and the nats/webhook-* components removed; operational docs updated to this path |
| 3        | 2026-06-21 | @jeffmccune | HOL-1373/HOL-1378: add the **App-of-Apps OCI config-image bootstrap** — the complement to the per-app Kargo delivery above. The whole committed `holos/deploy/` tree is published as a single OCI bundle (`holos-paas-config:dev`, mutable tag) and two root Argo CD `Application`s reconcile the platform from it under two AppProjects: `platform` (the system components, root `platform-bootstrap`) and `projects` (the project/application collection resources, root `projects-bootstrap`). `scripts/apply` brings Argo CD up imperatively (the bootstrap floor), then publishes the bundle and applies the two roots so Argo CD takes over ongoing reconciliation. See *Bootstrap delivery — the App-of-Apps OCI config bundle* below. Built across HOL-1374 (the `holos-paas-config` bundle + `make config-build`/`config-push`), HOL-1375 (the `platform`/`projects` AppProjects + repo-credential bootstrap), HOL-1376 (the platform root), HOL-1377 (the projects root), and HOL-1378 (the `scripts/apply` wiring + these docs). |

## Context and Problem Statement

[ADR-6](ADR-6.md) and [ADR-13](ADR-13.md) designed the MVP deployment path as a
six-stage, in-cluster NATS JetStream pipeline: a thin webhook receiver
([ADR-9](ADR-9.md)), a parse-and-route webhook subscriber ([ADR-10](ADR-10.md)),
a render subscriber that runs `holos render platform` and an ORAS push, and a
deployer task subscriber that patches the Argo CD `Application`
([ADR-11](ADR-11.md)) — all driven by registry push notifications, with task
messages defined as ConnectRPC protobuf ([ADR-14](ADR-14.md)). That design works,
but it requires the platform to **build, operate, and secure four bespoke
in-cluster services** plus a NATS JetStream backbone before a single application
can deploy.

The decisive constraint surfaced during the render+publish research
([report](../research/rendered-manifests-publish-pipeline.md), §2.4): **OSS Kargo
cannot host the Holos render step.** Kargo's built-in render steps are
`kustomize-build` and `helm-template` only, its `oci-push` step copies or retags
*existing* artifacts but cannot package a local directory of rendered YAML, and
arbitrary-command custom promotion steps are an enterprise/Akuity-Platform-only
feature. So the part of the pipeline that is genuinely bespoke — turning a new app
image tag into a rendered-manifests OCI artifact — has no off-the-shelf home and
must be written by us regardless of where it runs.

Given that, do we need the whole in-cluster NATS pipeline to host that one bespoke
step? Or can the render-and-publish move to the **client side** (a command-line
build-and-publish workflow run by the engineer or their coding agent), leaving the
in-cluster system to do only what mature off-the-shelf tools already do well —
**watch the registry and promote**? This ADR records the decision to make that
pivot.

## Context / References

- [ADR-6 — MVP Heroku-Style Deployment Pipeline](ADR-6.md) — the six-stage NATS
  pipeline this ADR supersedes (now `Deprecated`).
- [ADR-13 — End-to-End MVP Deployment Flow: Two Registry-Event Loops](ADR-13.md)
  — the two-loop NATS flow this ADR supersedes (now `Deprecated`).
- [ADR-8 — Container Registry and Image Tagging](ADR-8.md) — the registry as
  artifact store and event source; revised so the registry is watched by a Kargo
  `Warehouse` rather than emitting a webhook to the receiver.
- [ADR-9](ADR-9.md), [ADR-10](ADR-10.md), [ADR-11](ADR-11.md),
  [ADR-14](ADR-14.md) — the receiver, subscriber, deployer, and protobuf
  message-schema ADRs whose in-cluster components are eschewed/deferred under this
  pivot (now `Deprecated`). The `Application`-as-deploy-target concept from
  [ADR-11](ADR-11.md) survives the pivot (see Design).
- [Research: Performing the Re-render + ORAS Publish Step in the Event-Driven Pipeline](../research/rendered-manifests-publish-pipeline.md)
  — §2.4 establishes that OSS Kargo's `oci-push` only copies existing artifacts
  and that custom render steps are enterprise-only; §2.6–2.7 verify
  `holos render platform --inject` and ORAS directory push; the comparison table
  names Kargo as the registry-watch/promote growth path.
- [Research: Handling Image-Tag Updates in Argo CD with an OCI Manifest Source](../research/argocd-oci-image-tag-updates.md)
  — Argo CD ≥ 3.1 consuming an OCI source by **digest** in `targetRevision`; the
  two-artifact model (application image vs. rendered-manifests artifact).
- [Kargo](https://docs.kargo.io/) — `Warehouse`/`Freight`/`Stage` and the
  [`argocd-update` promotion step](https://docs.kargo.io/user-guide/reference-docs/promotion-steps/argocd-update).
- [Argo CD OCI source](https://argo-cd.readthedocs.io/en/latest/user-guide/oci/)
  (requires Argo CD ≥ 3.1).
- [Kustomize](https://kustomize.io/) and [ORAS](https://oras.land/).

## Design

The deployment path splits cleanly into two halves along the seam the research
identified — *who renders* vs. *who watches and promotes* — and assigns each half
to the tool that does it best.

### Half 1 — client-side build-and-publish (ORAS)

The engineer (or a coding agent acting on their behalf) runs a **command-line
build-and-publish workflow** on the client side, not in the cluster:

1. Build and push the application image to the registry, with a tag
   ([ADR-8](ADR-8.md)). The tag is the version.
2. Render the platform with the new image injected:
   `holos render platform --inject app_image=<repo>@sha256:<digest>` (the verified
   tag-injection mechanism — see the
   [render+publish report](../research/rendered-manifests-publish-pipeline.md)
   §2.6). Inject the **digest-pinned** reference so the rendered YAML is exact.
3. **Package the rendered output as a Kustomize-built OCI artifact**, then
   `oras push` it to the application's rendered-manifests repository.
4. Argo CD (≥ 3.1, OCI source) syncs that artifact, pinned by digest in
   `targetRevision`.

This is the same `holos render` + ORAS work the deprecated render subscriber would
have done — moved to the client, where there is no consumer to tune, no NATS
stream to provision, no in-cluster push credential to hold, and no bespoke
service to operate. A platform engineer's break-glass `oras push` and a future CI
system publish through the identical path.

#### Kustomize, not a Helm chart wrapper

The OCI artifact is produced with **Kustomize**, **not** by wrapping the rendered
output in a Helm chart. Holos already renders fully-resolved Kubernetes YAML —
there is no in-cluster templating to perform — so the artifact only needs to be
a versioned, digest-addressable bundle of plain manifests that Argo CD's OCI
source can sync. Kustomize packages that plain-manifest tree directly and is
**cleaner to produce client-side**: a `kustomization.yaml` over the rendered
`deploy/` tree needs no chart scaffolding, no `Chart.yaml`/`values.yaml`, and no
templating round trip that would re-introduce exactly the in-cluster templating
the rendered-manifests pattern exists to avoid. A Helm chart wrapper would add a
packaging layer whose only job is to carry static YAML — ceremony with no
benefit here. Argo CD's OCI source accepts ORAS's default directory layer media
type (`application/vnd.oci.image.layer.v1.tar+gzip`) for plain manifests, so a
Kustomize-built bundle needs no chart indirection to be syncable.

### Half 2 — Kargo-driven promotion

In the cluster, **Kargo** watches the registry and promotes:

- A Kargo **`Warehouse`** subscribes to the rendered-manifests repository and
  discovers new **`Freight`** when a new artifact is published — replacing the
  registry → webhook receiver → NATS path of the deprecated pipeline. The
  registry is still the event source ([ADR-8](ADR-8.md)); Kargo polls/watches it
  natively instead of the registry POSTing to an in-cluster receiver.

  > **Open validation item (resolve in implementation).** Kargo `Warehouse`
  > subscriptions are documented for **container images, Git repositories, and
  > Helm chart repositories** — not, as a first-class subscription type, an
  > arbitrary plain-manifest OCI artifact. The implementation phases that deploy
  > and configure Kargo (HOL-1238 deploys the controller/CRDs; HOL-1240 defines
  > the `Project`/`Warehouse`/`Stage`) **must verify** how the
  > Kustomize-built rendered-manifests artifact is discovered as `Freight`. The
  > most likely path is an **`image` subscription** against the
  > rendered-manifests repository (the artifact is an OCI image manifest like any
  > other tag), with the `argocd-update` step resolving the discovered digest into
  > `desiredRevision`. If Kargo cannot treat the artifact as an image
  > subscription, the recorded fallbacks are: tag the artifact so an `image`
  > subscription matches it, or interpose a thin Git/Helm shim that Kargo does
  > subscribe to. This is the one genuinely unproven assumption in the pivot and
  > is called out so the implementation does not discover it late. See the
  > [Kargo Warehouse docs](https://docs.kargo.io/user-guide/how-to-guides/working-with-warehouses)
  > and the open
  > [Kargo OCI-artifact feature request](https://github.com/akuity/kargo/issues/4864).
- A Kargo **`Stage`** runs an **`argocd-update`** promotion step that patches the
  Argo CD `Application`'s **`targetRevision`** to the new artifact's immutable
  digest. Argo CD then syncs the rendered manifests from the OCI source
  ([argocd-oci report](../research/argocd-oci-image-tag-updates.md)). Two Kargo
  prerequisites must be set up for this step to take effect, and the
  implementation phase must honor them:
  - **The target Argo CD `Application` authorizes the Stage** via the
    `kargo.akuity.io/authorized-stage: "<project>:<stage>"` annotation — Kargo
    refuses to modify an `Application` that has not opted in.
  - **The step's app `sources` entry sets `updateTargetRevision: true`** with the
    `desiredRevision` resolved from the discovered `Freight` (the artifact
    digest); `argocd-update` patches `targetRevision` **only** when that flag is
    set. See the
    [Kargo `argocd-update` reference](https://docs.kargo.io/user-guide/reference-docs/promotion-steps/argocd-update/).

Kargo does exactly what OSS Kargo *can* do well — watch (`Warehouse`/`Freight`)
and promote (`argocd-update`) — and nothing it cannot (it never renders;
rendering happened client-side in half 1). This matches the growth path both
research reports named for Kargo, adopted now as the MVP rather than deferred.

### What survives from the deprecated design

- **The `Application`-as-deploy-target concept** ([ADR-11](ADR-11.md)) survives:
  the deployed truth is still an Argo CD `Application` whose `targetRevision`
  selects the rendered-manifests artifact by digest. What changes is **who patches
  it** — the Kargo `argocd-update` promotion step, **not** the NATS deployer task
  subscriber.
- **Argo CD OCI delivery** ([ADR-13](ADR-13.md), the argocd-oci research)
  survives unchanged: Argo CD ≥ 3.1 syncs an OCI source pinned by digest.
- **`holos render platform --inject`** survives as the render mechanism — it just
  runs client-side now.
- **Digest pinning** ([ADR-8](ADR-8.md)) survives: a tag may label, but the
  immutable digest in `targetRevision` is what deploys.

### Bootstrap delivery — the App-of-Apps OCI config bundle

The two halves above deliver **one application** at a time: a per-app
rendered-manifests artifact (`holos-paas-manifests`, immutable
input-addressed tags) that Kargo watches and promotes by digest. They do
**not** deliver the **platform itself** — the Layer 0 foundation, the Layer 1
services, and the project/application control-plane resources. That is the job
of a second, complementary OCI delivery path added in HOL-1373: an Argo CD
**App-of-Apps over an OCI config bundle**.

- **The bundle (`holos-paas-config:dev`).** `scripts/publish-config`
  (`make config-build`/`config-push`) tars the **committed** `holos/deploy/`
  tree as-is — no render, no digest injection, no Kustomize — and `oras push`es
  it under a **mutable `:dev`** tag. This is deliberately distinct from the
  per-app `scripts/publish` path: that one re-renders with an injected app image
  digest and pushes an immutable input-addressed tag to `holos-paas-manifests`
  for Kargo; this one bundles the reviewed platform render under one stable
  handle for Argo CD to bootstrap the whole platform from. See
  [holos/docs/oci-publish-workflow.md](../../holos/docs/oci-publish-workflow.md)
  (*Platform config bundle*).
- **Two AppProjects, two roots — system vs. tenant separation.** Two Argo CD
  `AppProject`s split platform delivery by trust scope (the `argocd-projects`
  component): **`platform`** (broad — the system owns CRDs, ClusterRoles, and
  every namespace) and **`projects`** (tenant-scoped — denies the reserved
  platform namespaces, whitelists only the Kargo `Project` cluster-scoped kind).
  A root `Application` lives in each: **`platform-bootstrap`** (`spec.project:
  platform`, the `app-of-apps` component) fans out one child `Application` per
  system component from the bundle's per-component subpaths; **`projects-bootstrap`**
  (`spec.project: projects`, the `projects` component) fans out the
  collection-driven `project`/`application` resources. Both track
  `targetRevision: dev` and reconcile on every re-push (the "Always" repo-cache
  TTL the `argocd` component shortens to `1m`).
- **Bootstrap ordering — the chicken-and-egg.** Argo CD cannot reconcile the
  platform from the bundle until Argo CD is itself running. So `scripts/apply`
  keeps bringing the foundation + Argo CD + Kargo up **imperatively** (the
  bootstrap floor, `kubectl apply --server-side` in dependency order), then as a
  **final handoff** publishes the bundle and applies the two root `Application`s
  — after which Argo CD owns ongoing reconciliation. The imperative floor is
  never removed; the handoff is idempotent and gated (it skips gracefully when
  `oras`/Quay/the push credential are absent, leaving a usable floor). The
  per-app Kargo delivery (halves 1–2) is **unchanged and complementary**: it
  still owns each app's `Application.spec.source.targetRevision`, which the
  project/application roots deliberately do **not** manage (the
  `kargo.akuity.io/authorized-stage` posture).

This bootstrap **supersedes the deferred per-component `git`-source projection**
(`userDefinedBuildPlan`'s `argoAppDisabled` flip) **for the platform**: platform
self-delivery is now the OCI App-of-Apps, not a git-source Application per
component. See [holos/docs/placeholders.md](../../holos/docs/placeholders.md)
(*ArgoCD gitops delivery*).

### What is eschewed / deferred

The following in-cluster components from the deprecated pipeline are **not built
for the MVP** and are deferred — eschewed in favor of the client-side
build-and-publish ORAS workflow plus Kargo:

- The **webhook receiver** ([ADR-9](ADR-9.md)) — Kargo's `Warehouse` watches the
  registry directly; no thin HTTP ingress is needed.
- The **webhook subscriber** ([ADR-10](ADR-10.md)) — there is no raw webhook to
  parse and route; Kargo discovers `Freight` and routing is the `Warehouse`/
  `Stage` subscription, not a KRM-match in a Go subscriber.
- The **render subscriber** ([ADR-13](ADR-13.md)) — rendering moves client-side.
- The **deployer task subscriber** ([ADR-11](ADR-11.md)) — the `argocd-update`
  promotion step patches `targetRevision` instead.
- The **NATS JetStream task backbone and protobuf message schemas**
  ([ADR-14](ADR-14.md)) — with no in-cluster subscribers exchanging
  `RenderTask`/`DeployTask` messages, the `tasks.*` subjects and their protobuf
  contracts are not used. (The `WEBHOOKS` raw stream and any non-pipeline NATS use
  are out of scope of this ADR.)

These documents are kept for the historical record and marked `Deprecated`, per
[writing-adrs.md](writing-adrs.md) — ADRs are never deleted.

## Decision

1. The MVP deployment path is **client-side build-and-publish (ORAS) plus
   Kargo-driven promotion**, replacing the six-stage in-cluster NATS pipeline of
   [ADR-6](ADR-6.md) and the two-loop flow of [ADR-13](ADR-13.md).
2. **Rendering and publishing move client-side.** A command-line workflow runs
   `holos render platform --inject app_image=<repo>@<digest>`, packages the
   rendered output as a **Kustomize-built OCI artifact**, and `oras push`es it to
   the rendered-manifests repository.
3. **The OCI artifact is produced with Kustomize, not a Helm chart wrapper**,
   because Holos already emits fully-resolved YAML and a plain-manifest Kustomize
   bundle is cleaner to produce client-side and avoids re-introducing in-cluster
   templating.
4. **Kargo watches and promotes.** A `Warehouse` discovers new `Freight` from the
   rendered-manifests repository; a `Stage` runs an `argocd-update` promotion step
   that patches the Argo CD `Application`'s `targetRevision` to the artifact
   **digest**. Argo CD (≥ 3.1, OCI source) syncs it.
5. **The in-cluster webhook receiver ([ADR-9](ADR-9.md)), webhook subscriber
   ([ADR-10](ADR-10.md)), render subscriber ([ADR-13](ADR-13.md)), deployer task
   subscriber ([ADR-11](ADR-11.md)), and the NATS protobuf task schemas
   ([ADR-14](ADR-14.md)) are eschewed/deferred** for the MVP and their ADRs are
   marked `Deprecated`. The `Application`-as-deploy-target concept from
   [ADR-11](ADR-11.md) survives, now patched by Kargo.
6. **Git write-back and separation-of-duty promotion gating remain deferred**, as
   in the prior design — Kargo provides a natural future home for promotion gates.

## Consequences

- The platform ships a working deployment path with **far less bespoke
  in-cluster code**: no receiver, no subscribers, no NATS task backbone to build,
  operate, and secure. The growth-path research reports named Kargo for
  watch/promote; this adopts it as the MVP.
- **Kargo (controller + CRDs) and Argo CD (≥ 3.1, OCI source) become MVP
  operational dependencies**; they must be deployed and operated. NATS JetStream
  is no longer required for the deployment pipeline.
- **The render step runs on the client**, so the platform does not hold a registry
  **push** credential in-cluster; the client (engineer or coding agent) authenticates
  to the registry for `oras push`. Kargo needs **read** access to watch the
  rendered-manifests repository, and Argo CD needs a **pull** credential for the
  OCI source — a clean read-only in-cluster credential posture.
- The client side becomes load-bearing: an engineer or coding agent must run the
  CLI build-and-publish workflow, which is acceptable for the MVP's audience
  (developers and coding agents) and is the explicit Heroku-style ergonomic this
  milestone targets. Automating that workflow (a CI runner, a hosted service) is a
  later option, not MVP scope.
- The deployed truth remains an Argo CD `Application` pinned to an immutable OCI
  **digest** ([ADR-8](ADR-8.md), [ADR-13](ADR-13.md)); only the **patcher**
  changes (Kargo, not a NATS subscriber).
- The deprecated ADRs (6, 9, 10, 11, 13, 14) and their research remain the record
  of why the NATS pipeline was designed and why it was set aside; future work that
  needs an in-cluster, event-driven render path can revive them.
- **This ADR is the decision record.** The actual retirement of the NATS
  pipeline and its operational docs landed as a separate, code-touching phase
  (HOL-1241, *chore(pipeline): retire NATS pipeline*): the webhook receiver and
  subscriber subcommands, `internal/{nats,task,webhook}`, the pipeline protobuf,
  and the `nats`/`webhook-receiver`/`webhook-subscriber` Holos components were
  removed, and `holos/README.md`, the milestones plan, and the operational docs
  were updated to this Kargo + CLI/ORAS path. The deprecated ADRs' status
  banners remain the historical record of the NATS pipeline. The Kargo
  controller/CRDs (HOL-1238), the publish workflow (HOL-1239), and the
  `Project`/`Warehouse`/`Stage` configuration (HOL-1240) are the other
  implementation phases of the parent plan.
