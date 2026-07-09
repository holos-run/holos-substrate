# Research: Performing the Re-render + ORAS Publish Step in the Event-Driven Pipeline

> **Archived (PaaS era).** This document was written for the Holos PaaS
> prototype and was archived during the Holos Substrate rebrand. It is kept
> for the historical record; see [docs/](../) for the documentation that
> covers the substrate.

| Metadata | Value                                        |
|----------|----------------------------------------------|
| Date     | 2026-06-09                                   |
| Author   | @jeffmccune                                  |
| Status   | Informational (informs ADR-6, 8, 10, 11)     |
| Tags     | holos, oras, oci, render, nats, research     |

> Follow-up to
> [Research: Handling Image-Tag Updates in Argo CD with an OCI Manifest Source](../research/argocd-oci-image-tag-updates.md),
> which concluded Argo CD should sync rendered manifests from an OCI artifact
> and the deployer should set `Application.targetRevision`. That left a gap:
> *who produces the new rendered-manifests artifact when a new app image tag
> arrives?* This report answers that question.

**Question:** When a new app image tag arrives, how should the pipeline
re-render manifests with `holos render platform` and publish the
rendered-manifests OCI artifact that Argo CD syncs?

## 1. Summary

**Recommendation: add a dedicated render-task subscriber as its own stage in the
existing NATS JetStream pipeline.** The webhook subscriber
([ADR-10](../adr/archive/ADR-10.md)) stays a thin parser that emits a *render task*; a
new subscriber consumes it from its own WorkQueue stream, runs
`holos render platform --inject <app-image-tag>` against platform CUE
configuration sourced from an OCI artifact (or baked into its container image —
no GitHub), packages `deploy/` with ORAS (the `oras-go/v2` library, in-process),
pushes the rendered-manifests artifact, resolves its digest, and publishes the
*deployer task* carrying that digest; the deployer ([ADR-11](../adr/archive/ADR-11.md))
remains a tiny, idempotent KRM patcher that sets the Argo CD
`Application.targetRevision`. This is verified feasible: `holos render platform`
supports `--inject key=value` CUE tag injection (confirmed in the holos source
at v0.106), ORAS pushes arbitrary directories and returns the digest, and Argo
CD's OCI source accepts a digest in `targetRevision`. The survey shows no
off-the-shelf tool does this for Holos/CUE without Git or GitHub: Argo CD's
source hydrator only writes hydrated manifests to Git; OSS Kargo's render steps
are Kustomize/Helm-only and its `oci-push` only copies existing artifacts
(arbitrary-container custom steps are Kargo *enterprise-only* as of v1.10); CI
rendering is excluded by the no-GitHub constraint. A subscriber in the pipeline
we already operate — with `MaxAckPending=1`, `InProgress()` heartbeats during
slow renders, an input-addressed artifact tag for idempotency, and an explicit
DLQ publish on permanent render failure — is the smallest correct design and
grows cleanly into Kargo later.

## 2. State of the art survey

### 2.1 CI-based rendering (GitHub Actions and similar)

The dominant published pattern: CI renders manifests (Helm/Kustomize/CUE/
jsonnet) and pushes the result as an OCI artifact. Flux explicitly documents
this — "run the generators in CI and publish the resulting manifests as OCI
artifacts" — and provides a GitHub Action for it
([Flux OCI cheatsheet](https://fluxcd.io/flux/cheatsheets/oci-artifacts/),
[OneUptime: OCI artifact build & push in CI for Flux](https://oneuptime.com/blog/post/2026-03-13-oci-artifact-build-push-ci-flux-cd/view)).
Mature and well-trodden, but it is triggered by Git events and almost
universally hosted on GitHub Actions. **Excluded for the MVP by the explicit
no-GitHub constraint**, and it also wouldn't be triggered by our event source (a
registry webhook, not a commit).

### 2.2 Flux: `flux push artifact` + OCIRepository

Flux has the most mature "rendered manifests as OCI" story.
`flux push artifact oci://… --path ./deploy --source … --revision …` "creates a
tarball from the given directory … and uploads the artifact to an OCI
repository," supports `--output json` to extract the pushed digest, sets
provenance annotations (`org.opencontainers.image.source`/`revision`/`created`),
and authenticates via docker config, `--creds`, or cloud `--provider`
([flux push artifact reference](https://fluxcd.io/flux/cmd/flux_push_artifact/)).
`OCIRepository` can pin to tags, semver, or digests. Maturity: high. Fit: the
*packaging* half is directly reusable (the `flux` CLI is a fine alternative to
`oras` as the push tool), but the *sync* engine in our architecture is Argo CD,
not Flux, per the [prior research report](../research/argocd-oci-image-tag-updates.md); and
Flux says nothing about *who triggers* the render — people run it from CI. Note
the cheatsheet does **not** document reproducible/deterministic tarball digests,
and the `org.opencontainers.image.created` annotation means re-pushing identical
content generally produces a *different manifest digest* — relevant to
idempotency (§4.5).

### 2.3 Argo CD source hydrator / rendered-manifests tooling

Argo CD's first-party Source Hydrator (alpha, behind `hydrator.enabled`)
implements the rendered manifests pattern: it watches a "dry" Git revision,
hydrates with Helm/Kustomize, and **pushes the hydrated manifests to a Git
branch** (push-to-deploy or push-to-stage) via a commit-server component
([Source Hydrator docs](https://argo-cd.readthedocs.io/en/latest/user-guide/source-hydrator/),
[manifest hydrator proposal](https://github.com/argoproj/argo-cd/blob/master/docs/proposals/manifest-hydrator.md)).
Two disqualifiers for us: the hydrated destination is **Git only** (no OCI
destination), and the hydration tools are Argo CD's config-management plugins,
not Holos. It validates the pattern conceptually but is unusable under "no Git
write-back." Argo CD ≥ 3.1 *consuming* OCI sources is first-class
([Argo CD OCI user guide](https://argo-cd.readthedocs.io/en/latest/user-guide/oci/),
[InfoQ on v3.1](https://www.infoq.com/news/2025/08/argocd-oci-support-new-ui/))
— `targetRevision` accepts "the desired image tag or digest," and the default
accepted layer media type for plain manifests is
`application/vnd.oci.image.layer.v1.tar+gzip` (ORAS-compatible).

### 2.4 Kargo promotion steps

Verified against current docs
([promotion steps reference](https://docs.kargo.io/user-guide/reference-docs/promotion-steps)):

- Built-in render steps are **`kustomize-build` and `helm-template` only** —
  there is no CUE or arbitrary-command built-in step. Most documented flows pair
  these with `git-clone`/`git-commit`/`git-push`.
- **`oci-push` (new in v1.10) copies or retags *existing* OCI artifacts**
  between registries (`srcRef` → `destRef`, images and OCI Helm charts,
  multi-arch indexes) and outputs `image`/`digest`/`tag`
  ([oci-push step](https://docs.kargo.io/user-guide/reference-docs/promotion-steps/oci-push)).
  It **cannot package a local directory of rendered YAML as a new artifact**.
- **Custom promotion steps** (arbitrary OCI images run as pods, registered via
  `CustomPromotionStep`, alpha) would be able to run `holos render` +
  `oras push` — but per Kargo's own docs and the Akuity v1.10 announcement they
  are an **enterprise/Akuity-Platform-only feature**, not OSS Kargo
  ([custom-steps docs](https://docs.kargo.io/user-guide/reference-docs/promotion-steps/custom-steps),
  [Akuity v1.10 blog](https://akuity.io/blog/kargo-v1-10-custom-steps-http-notifications-new-promotions)).

Conclusion: OSS Kargo today can watch the registry (`Warehouse`) and patch the
Argo CD Application (`argocd-update`), matching the prior research's "growth
path" verdict — but it **cannot perform the Holos render+publish step itself**
without the enterprise custom-steps feature or an external service it calls.
This strengthens, not weakens, the case for owning the render step in our own
subscriber.

### 2.5 In-cluster render Jobs, Tekton, Argo Workflows (+ Argo Events)

The generic Kubernetes answer: an event spawns a `Job`/`PipelineRun`/`Workflow`
pod that runs the renderer and pushes the artifact. Argo Events can trigger Argo
Workflows or Tekton from webhooks/NATS
([Argo Events triggers](https://argoproj.github.io/argo-events/sensors/triggers/build-your-own-trigger/));
Tekton+Argo CD pipelines are well documented
([DigitalOcean Tekton/Argo CD guide](https://www.digitalocean.com/community/developer-center/kubernetes-ci-cd-using-tekton-argo-cd-and-knative-serverless-applications)).
Maturity: high as building blocks; but there is no off-the-shelf "render CUE and
push ORAS" task — you write the container either way. The cost is a whole
workflow engine (CRDs, controllers, webhook plumbing) duplicating the eventing
the NATS pipeline already provides, plus pod-startup latency per event. Best
regarded as a *scaling* option (per-event isolation of CUE memory) rather than
an MVP option.

### 2.6 Holos CLI capabilities (verified against source, v0.106)

Checked directly in the workspace copy of
[`github.com/holos-run/holos`](https://github.com/holos-run/holos):

- **Tag injection exists and is the intended mechanism**:
  `holos render platform --inject key=value` injects CUE build tags; the
  platform command forwards `--inject` to every per-component
  `holos render component` subprocess (`internal/cli/render/render.go`,
  `internal/component/component.go`: "All versions allow tags explicitly
  injected using the --inject flag"). Platform CUE can declare
  `@tag(app_image_tag)` and reference it where the image is set.
- **No native OCI publish**: the CLI subcommands are `compare`, `compile`,
  `init`, `render`, `show`, `slice`, `txtar` — there is no
  `push`/`publish`/`artifact` command. `oras.land/oras-go/v2` appears in
  `go.mod` only as an **indirect** dependency. **Holos renders to a local
  directory; publishing is explicitly someone else's job** (today: ORAS or
  `flux push artifact`). No holos docs describe native OCI publishing; treat
  "holos pushes OCI" as unavailable.
- **CUE evaluation cost is real**: `holos render platform` execs one
  `holos render component` *subprocess per component* because "Cue is not safe
  for concurrent use within the same process" (comment in `render.go`). A full
  platform render is CPU- and memory-intensive and takes seconds to minutes
  depending on platform size (estimate, not benchmarked).

### 2.7 ORAS CLI / library

`oras push` "pushes files to a registry or an OCI image layout," supports
per-file media types (default `application/vnd.oci.image.layer.v1.tar`;
directories are packed as `…tar+gzip`, which matches Argo CD's accepted media
type), manifest annotations, multiple tags, and digest retrieval via
`--format json` / `--format go-template='{{.digest}}'` (experimental)
([oras push docs](https://oras.land/docs/commands/oras_push),
[ORAS artifact concepts](https://oras.land/docs/concepts/artifact/)). For a Go
subscriber, `oras-go/v2` does the same in-process and returns the pushed
descriptor (digest) directly — no shelling out, no experimental flag. Caveat:
the docs do not promise deterministic digests for repeated pushes of identical
directory content (tar/gzip metadata, timestamps); do not rely on byte-identical
reproducibility (§4.5).

## 3. Comparison: subscriber-inline render vs alternatives

| Dimension | A. Inline in webhook subscriber (ADR-10) | B. Inline in deployer subscriber (ADR-11) | **C. Dedicated render-task subscriber (recommended)** | D. K8s Job per event | E. Tekton / Argo Workflows + Argo Events | F. Kargo step | G. CI (GitHub Actions) |
|---|---|---|---|---|---|---|---|
| Latency (event→artifact) | Best (no extra hop) | Best | +1 NATS hop (~ms); render dominates anyway | + pod schedule/start (seconds) + image pull | Same as D plus engine overhead | Warehouse poll interval + pod start | Minutes; wrong trigger source |
| At-least-once / redelivery safety | Must mix fast-parse and slow-render ack semantics in one consumer | Must mix slow-render and fast-KRM-patch semantics | Clean: one stream tuned for slow work (`AckWait`+`InProgress`, `MaxAckPending=1`); idempotent via input-addressed tag | Good: Job is the retry unit; needs Job-dedup keyed on input | Good; engines have retries | Promotion record retries; custom steps doc warns steps may run more than once | Re-run semantics external |
| Resource footprint (CUE cost) | Bloats a stage meant to be light; parse backlog stalls behind renders | Bloats the terminal stage; a stuck render blocks `targetRevision` patches | One pod sized for CUE; `MaxAckPending=1` caps concurrent CUE evals | Best isolation: memory per pod, dies with the Job | Same as D | Pod per promotion | N/A (hosted) |
| Failure isolation | Render bug breaks parsing of *all* webhooks | Render bug breaks all deploys, incl. ones needing no render | Render failures contained to one stage; parse and deploy keep working | Strongest (process-level) | Strong | Strong | Strong |
| Registry **push** credential placement | In the parse stage (broad exposure) | In the same pod that holds K8s patch RBAC (worst combination) | **Only** the render subscriber holds push creds; deployer holds only K8s RBAC; receiver holds nothing | In Job pods (secret template sprawl) | In workflow service accounts | In Kargo project secrets | In GitHub secrets (forbidden) |
| MVP simplicity | Fewest deployables (2 subscribers total) | Few deployables | One more small Go consumer + one stream — on infrastructure ADR-6 already mandates | Requires Job-spawner logic + RBAC + GC + completion-watching (a mini-controller) | Two new engines to operate | New controller; **render step needs enterprise custom steps — not possible in OSS Kargo today** | Violates constraint |
| Growth path | Refactor out later (likely) | Refactor out later | Stage boundary already exists; swap implementation for Jobs (D) or Kargo-calls-us later without touching neighbors | Natural scale-up of C | Heavyweight unless already adopted | The designated future, but blocked on enterprise custom steps or an external render service (i.e., C anyway) | — |

**Prose.** The decisive observations:

1. **Someone must run `holos render` + push regardless** — no surveyed tool
   does CUE-render-to-OCI off the shelf without Git (hydrator: Git-only; Kargo
   OSS: Kustomize/Helm only, `oci-push` copies but can't create; Flux: provides
   the push verb but not the trigger). So the comparison is really about *where
   our code runs*, not *whether to write it*.
2. **Inline-in-an-existing-subscriber (A/B) saves one deployment but couples
   mismatched workloads.** [ADR-9](../adr/archive/ADR-9.md)/[ADR-10](../adr/archive/ADR-10.md)'s
   whole design argument is decoupling fast ingress from slow downstream; a
   multi-minute CUE render inside the parse stage recreates exactly the
   head-of-line blocking the WorkQueue was introduced to prevent. B is worse on
   security: the one pod would hold both registry push credentials and
   Kubernetes patch rights.
3. **A dedicated subscriber (C) costs almost nothing extra** — NATS is already
   the backbone, ADR-10 already publishes to a "processor subject"; C just means
   the deployer's queue is fed by the renderer instead of directly by the parser
   (or equivalently, a render queue is inserted before the deploy queue). It
   keeps each stage's ack semantics, resource profile, and credentials narrow.
4. **Job-per-event (D) and workflow engines (E) buy isolation the MVP doesn't
   need yet** at the cost of a controller's worth of glue (spawn, dedup, watch,
   GC). They are the documented scale-out path if CUE memory or render
   concurrency becomes a problem.
5. **Kargo (F) remains the growth path, not the MVP render engine**: in OSS
   Kargo the render step would have to call out to… a service like C. Adopting C
   now is *compatible* with Kargo later (Warehouse detects the image → promotion
   step POSTs/publishes a render task → C renders/pushes → `argocd-update` sets
   `targetRevision`).

## 4. Ideal design: a render-task subscriber produces the rendered-manifests ORAS artifact

### 4.1 Which subscriber

**A new, dedicated render-task subscriber** — not the webhook subscriber (keep
[ADR-10](../adr/archive/ADR-10.md) thin: parse, normalize, dispatch) and not the
deployer (keep [ADR-11](../adr/archive/ADR-11.md) a single idempotent KRM write). It is
"an existing subscriber pattern" in the architectural sense: the same
Go-binary-consuming-a-WorkQueue shape as ADR-10/11, on the same JetStream
backbone, fed by the queue the webhook subscriber already publishes to. The
ADR-10 "deployer task" subject becomes (or is preceded by) a **render task**
subject.

### 4.2 Message flow

```
registry push (new app image tag)                                [ADR-8]
   │ webhook
   ▼
webhook receiver ── publish raw body ──▶ WEBHOOK_RAW (WorkQueue) [ADR-9]
   ▼
webhook subscriber: parse registry payload → normalize           [ADR-10]
   │ publish RenderTask{app, image, tag, digest, idempotency-key}
   ▼
RENDER_TASKS (WorkQueue, file-backed)
   ▼
render-task subscriber:                                           [new]
   1. resolve app image tag → digest (registry HEAD)              (pin input)
   2. materialize platform config (image-baked or `oras pull` by digest)
   3. compute artifact tag T = f(config-digest, app-image-digest)
   4. if registry already has tag T → skip to 7 (idempotent replay)
   5. holos render platform --inject app_image=<repo@digest> → deploy/
   6. oras-go push deploy/ → manifests artifact; capture DIGEST
   7. publish DeployTask{app, manifestsArtifact: repo@DIGEST, tag T, key}
   8. Ack   (steps 5–6 bracketed by InProgress() heartbeats)
   ▼
DEPLOY_TASKS (WorkQueue)
   ▼
deployer task subscriber: patch Application targetRevision=DIGEST [ADR-11]
   ▼
Argo CD pulls oci://…@DIGEST and syncs                            [prior report]
```

### 4.3 Sourcing the Holos/CUE configuration without GitHub

Two GitHub-free options, in order of MVP preference:

1. **Baked into the subscriber's container image.** The platform CUE tree
   (`platform/`, `components/`, `cue.mod/` with vendored deps — no network
   module fetch at render time) is `COPY`'d into the render-subscriber image at
   build time. Simplest possible MVP: config version = image version, rollback
   = redeploy previous image. Limitation: changing platform config requires
   rebuilding/redeploying the subscriber.
2. **Platform-config as its own OCI artifact** (the natural second step): push
   the CUE tree with ORAS to `oci://registry/platform-config`, reference it **by
   digest** in the subscriber's configuration (env var / ConfigMap / future
   field on a CRD). At render time the subscriber `oras pull`s it to a temp dir
   (cached by digest). This makes the platform config itself versioned,
   auditable, and registry-native — symmetric with the other two artifacts,
   still no Git. The RenderTask can optionally carry a config-digest override.

(A PVC or git-server-in-cluster also works but adds state or reintroduces git
operations; not recommended for MVP.)

### 4.4 Injecting the new app image tag

Use holos's verified tag-injection mechanism: the platform CUE declares a tag,
e.g.

```cue
_AppImage: string @tag(app_image)
```

and the component sets the container image from `_AppImage`. The subscriber
runs:

```
holos render platform --inject app_image=registry.example/app@sha256:<digest>
```

`--inject` is forwarded to every component render (verified in
`internal/cli/render/render.go`). Inject the **digest-pinned reference**, not
the mutable tag — the subscriber resolves tag→digest once (step 1) so the
rendered YAML is exact and the same RenderTask always renders the same image
reference even if the tag is later moved. (Multi-app future: `--inject` a JSON
map of app→digest, or per-app tags.)

### 4.5 Idempotency and digest-pinned output

- **Do not rely on byte-identical reproducibility.** Neither ORAS nor Flux
  documents deterministic digests for re-pushed identical content (tar/gzip
  timestamps; Flux even stamps `org.opencontainers.image.created`). Same input
  can yield a different artifact digest on re-push.
- **Instead, make the artifact tag input-addressed**:
  `T = render-<config-digest-12>-<appimage-digest-12>` (or simply the app image
  tag when config is image-baked). On redelivery, the subscriber first resolves
  tag `T` in the registry (ORAS resolve); if present, it **skips the render
  entirely**, re-resolves the existing digest, and re-publishes the DeployTask.
  Same input → same artifact, achieved by check-then-render rather than by
  hoping for bit-reproducibility. Semantically the artifact remains
  content-addressed *downstream*: the DeployTask and `targetRevision` carry the
  immutable **digest**, satisfying [ADR-8](../adr/archive/ADR-8.md)'s digest-pinning
  preference.
- The DeployTask carries the idempotency key; the deployer's patch is naturally
  idempotent (same `targetRevision` value → no-op), satisfying
  [ADR-11](../adr/archive/ADR-11.md)'s "converge, not oscillate."

### 4.6 Ack semantics for a slow consumer

Render is the pipeline's only slow stage; tune its consumer specifically (all
standard JetStream mechanics,
[NATS consumer docs](https://docs.nats.io/nats-concepts/jetstream/consumers),
[model deep dive](https://docs.nats.io/using-nats/developer/develop_jetstream/model_deep_dive)):

- **Pull consumer, `AckExplicit`, `MaxAckPending = 1`** — serializes renders,
  which both bounds CUE memory (one render at a time) and prevents two
  concurrent renders racing to publish conflicting DeployTasks out of order.
- **`AckWait` ~60s plus periodic `msg.InProgress()`** every ~AckWait/3 during
  render+push — the documented long-running-task pattern: InProgress resets the
  AckWait timer, so a 5-minute render never gets redelivered mid-flight while a
  crashed subscriber's message is redelivered within a minute. (Don't set
  AckWait to worst-case render time; that delays crash recovery.)
- **`MaxDeliver` ~5 with a backoff schedule** for transient failures (registry
  blips, OOM).
- **Ack only after the DeployTask publish succeeds** (publish-then-ack). A crash
  between push and ack causes a redelivery that hits the tag-exists fast path
  and republishes the DeployTask — at-least-once on the deploy queue, absorbed
  by the idempotent deployer.
- **Coalescing (nice-to-have):** before rendering, check the queue/stream for a
  newer RenderTask for the same app and `Term()` the stale one — under bursts,
  render only the latest.

### 4.7 Credentials and RBAC

| Component | Needs | Notably does NOT need |
|---|---|---|
| Webhook receiver | NATS publish | registry creds, K8s API |
| Webhook subscriber | NATS consume/publish | registry creds, K8s API |
| **Render subscriber** | NATS consume/publish; **registry push** secret (dockerconfig, scoped to the manifests repo + read on app/config repos) | Kubernetes API access (none at all) |
| Deployer subscriber | NATS consume; K8s RBAC: `patch`/`update` on `applications.argoproj.io` (Argo CD ns) or the holos `Application` CR | registry creds |
| Argo CD repo-server | registry **pull** credential for `oci://…` repo (type `oci`) | push |

The render subscriber is the *only* holder of a write credential to the
registry, and it holds no cluster write access — a clean separation that inline
options A/B cannot achieve.

### 4.8 Failure modes

| Failure | Handling |
|---|---|
| CUE eval error / unparseable config (deterministic) | Don't burn 5 redeliveries on a deterministic failure: publish `{task, error, logs-tail}` to a `deploy.dlq.render` subject (Limits-retention stream, bounded), then `Term()` the message. Surface on `Application.status` later. |
| Registry push transient failure | `Nak()` with delay → redelivery with backoff, up to MaxDeliver. |
| MaxDeliver exhausted | `$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.<stream>.<consumer>` advisory; capture into the DLQ stream. **Caveat:** advisories carry only sequence metadata, and there are open reports of message loss with WorkQueue retention + R3 + max-deliver ([nats-server #7817](https://github.com/nats-io/nats-server/issues/7817)) — hence the explicit-DLQ-publish-then-Term pattern above is the primary mechanism, advisories the backstop. |
| Subscriber crash mid-render | AckWait expires (no more InProgress) → redelivery; tag-exists check makes the retry cheap if push had completed. |
| Stale/duplicate webhook (redelivered raw event upstream) | Idempotency key from [ADR-10](../adr/archive/ADR-10.md) + tag-exists fast path → re-publish same DeployTask → deployer no-op. |
| Renders pile up under burst | WorkQueue absorbs the backlog by design ([ADR-6](../adr/archive/ADR-6.md)/[ADR-9](../adr/archive/ADR-9.md)); MaxAckPending=1 + coalescing keeps the renderer at one render at a time on the latest version. |

### 4.9 How the deployer sets `targetRevision`

Unchanged from the [prior research](../research/argocd-oci-image-tag-updates.md)/ADR-11
direction, now with a concrete input: the DeployTask carries
`manifestsArtifact: oci://registry/rendered-manifests@sha256:<digest>` (plus the
human-readable tag `T` for status display). The deployer patches the Argo CD
`Application` (or the holos `Application` CR whose reconciler does so):
`spec.source.repoURL: oci://registry/rendered-manifests`,
`spec.source.targetRevision: sha256:<digest>` — Argo CD's OCI source accepts
"the desired image tag or digest"
([Argo CD OCI guide](https://argo-cd.readthedocs.io/en/latest/user-guide/oci/)).
Digest-pinning means Argo CD needs no registry polling and "what is deployed"
is exact.

## 5. Implications for the ADRs

- **[ADR-6](../adr/archive/ADR-6.md) (pipeline):** the MVP pipeline gains a sixth stage
  between parse and deploy: **render & publish**. The seam is, as everywhere
  else, a durable WorkQueue subject. The "five stages" enumeration and the
  diagram should be updated; the deferral language in ADR-6/11 ("GitOps
  reconciliation … deferred") is partially superseded — *Git* write-back stays
  deferred, but *re-render + OCI publish* moves into the MVP, because without it
  a new app tag has no rendered-manifests artifact to point Argo CD at.
- **[ADR-8](../adr/archive/ADR-8.md) (registry):** now **three** artifacts live in the
  registry: app image, rendered-manifests artifact, and (option 2) the
  platform-config artifact. The registry needs push credentials for the render
  subscriber and the planning note should add: repository layout/naming for the
  manifests and config artifacts, and the input-addressed tagging convention
  `render-<config>-<image>`.
- **[ADR-9](../adr/archive/ADR-9.md) (receiver):** unchanged.
- **[ADR-10](../adr/archive/ADR-10.md) (webhook subscriber):** the subscriber's output
  message becomes (or is joined by) a **RenderTask**, not a deploy-ready task —
  it cannot know the manifests artifact digest, which only exists after
  rendering. The planning note's requirement to "carry enough to resolve the
  rendered-manifests artifact version" is satisfied structurally: the render
  stage *produces* that version. ADR-10 should also state explicitly that the
  subscriber performs **no rendering** (the thin-stage rationale).
- **[ADR-11](../adr/archive/ADR-11.md) (deployer):** the deployer consumes the render
  stage's DeployTask and patches `targetRevision` to the digest — confirming the
  prior research's Option 1 and giving it its missing piece ("the controller
  must own the logic to map new app image tag → manifests artifact version,
  including re-rendering/publishing" — that logic now has a home). Record the
  Kargo finding: OSS Kargo cannot host the render step (custom steps are
  enterprise-only as of v1.10), so the growth path is *Kargo for
  watch/promote/gate, calling our render stage*, not Kargo replacing it.
- **New ADR needed:** "Render-Task Subscriber: Re-render and Publish the
  Rendered-Manifests Artifact" — covering the stream/subject, consumer tuning
  (MaxAckPending=1, AckWait+InProgress, MaxDeliver/backoff), config sourcing
  (image-baked → OCI-artifact), `--inject` contract with the platform CUE,
  tagging/idempotency, DLQ, and credentials.

**Uncertainties stated honestly:** holos has no native OCI publish (verified
absent in v0.106 source; could appear upstream later — worth a check before
implementation). ORAS/Flux artifact digests are not documented as reproducible
for identical input; the design therefore does not depend on that. Kargo
custom-steps enterprise-only status is per current docs/blog and could change.
Argo CD OCI digest-`targetRevision` is documented but the exact accepted digest
syntax (`sha256:…` vs full ref) should be verified against the deployed Argo CD
version during implementation. Render duration/memory figures are estimates,
not benchmarks.

## 6. Sources

**Primary documentation**

- [Argo CD — OCI source user guide](https://argo-cd.readthedocs.io/en/latest/user-guide/oci/)
- [Argo CD — Source Hydrator](https://argo-cd.readthedocs.io/en/latest/user-guide/source-hydrator/)
- [Argo CD — Manifest Hydrator proposal](https://github.com/argoproj/argo-cd/blob/master/docs/proposals/manifest-hydrator.md)
- [Kargo — Promotion steps reference](https://docs.kargo.io/user-guide/reference-docs/promotion-steps)
- [Kargo — oci-push step](https://docs.kargo.io/user-guide/reference-docs/promotion-steps/oci-push)
- [Kargo — custom steps](https://docs.kargo.io/user-guide/reference-docs/promotion-steps/custom-steps)
- [Flux — flux push artifact](https://fluxcd.io/flux/cmd/flux_push_artifact/)
- [Flux — OCI artifacts cheatsheet](https://fluxcd.io/flux/cheatsheets/oci-artifacts/)
- [ORAS — oras push](https://oras.land/docs/commands/oras_push) and [OCI artifact concepts](https://oras.land/docs/concepts/artifact/)
- [NATS — JetStream consumers](https://docs.nats.io/nats-concepts/jetstream/consumers) and [model deep dive (AckWait/InProgress/MaxAckPending)](https://docs.nats.io/using-nats/developer/develop_jetstream/model_deep_dive)
- [Holos source — render command & --inject](https://github.com/holos-run/holos) (verified locally at v0.106: `internal/cli/render/render.go`, `internal/component/component.go`)

**Secondary**

- [Akuity — Kargo v1.10: custom steps (enterprise) & oci-push](https://akuity.io/blog/kargo-v1-10-custom-steps-http-notifications-new-promotions)
- [Akuity — GitOps promotion with Kargo custom steps](https://akuity.io/blog/kargo-custom-steps-gitops-promotion)
- [InfoQ — Argo CD v3.1 OCI support](https://www.infoq.com/news/2025/08/argocd-oci-support-new-ui/)
- [nats-server issue #7817 — WorkQueue retention + max-deliver message loss](https://github.com/nats-io/nats-server/issues/7817)
- [Synadia — Building a job queue with NATS](https://www.synadia.com/blog/building-a-job-queue-with-nats-io-and-go)
- [OneUptime — OCI artifact build & push in CI for Flux](https://oneuptime.com/blog/post/2026-03-13-oci-artifact-build-push-ci-flux-cd/view)
- [DigitalOcean — Tekton + Argo CD on Kubernetes](https://www.digitalocean.com/community/developer-center/kubernetes-ci-cd-using-tekton-argo-cd-and-knative-serverless-applications)
- [Argo Events — triggers](https://argoproj.github.io/argo-events/sensors/triggers/build-your-own-trigger/)
- Prior internal research: [argocd-oci-image-tag-updates.md](../research/argocd-oci-image-tag-updates.md)
