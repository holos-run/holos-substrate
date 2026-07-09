# Research: The Custom NATS Pipeline (ADR 1–14) vs. a CLI + Kargo + Argo CD

| Metadata | Value                                                        |
|----------|--------------------------------------------------------------|
| Date     | 2026-06-14                                                   |
| Author   | @jeffmccune                                                  |
| Status   | Informational — recommendation (informs ADR-6, 9, 10, 11, 13, 14) |
| Tags     | kargo, argocd, pipeline, cli, mvp, oci, research, recommendation |

> **TL;DR — Recommendation:** **Pivot the MVP delivery pipeline to a CLI +
> Kargo + Argo CD.** Replace the custom webhook receiver (ADR-9), webhook
> subscriber (ADR-10), deployer (ADR-11), the NATS JetStream backbone (ADR-6),
> and the protobuf task contract (ADR-14) with **Kargo** (registry-watching,
> webhook ingest, and Argo CD `Application` promotion) and move the irreducible
> `holos render` + OCI-publish step into a **client/CI-side CLI**. Keep the
> Holos rendering layer, the `Project`/`Application` KRM model, and the
> Keycloak/Quay identity machinery (ADR-1–5, 12, 15) **unchanged** — Kargo does
> not touch them. This removes roughly three custom Go services plus a stateful
> message bus from the MVP while preserving a "push to deploy" experience and
> the Holos config-management story. ADR-11 already names Kargo as the growth
> path; this report argues the growth path *is* the cheaper MVP. See
> [§7 Recommendation](#7-recommendation) and [§8 Trade-offs](#8-trade-offs-honest-accounting).

## 1. Why this report exists

The project maintainer is concerned the MVP is **over-engineered**: it builds
and operates a multi-service, event-driven pipeline (a webhook receiver, a
webhook subscriber, a render subscriber, a deployer, and a NATS JetStream
backbone with a versioned protobuf message contract) to accomplish what is, at
its core, "a new image was pushed → render its manifests → tell Argo CD to sync
the new version."

This report compares the **current ADR-defined architecture** against the
alternative the maintainer proposed:

> A command-line tool builds and pushes OCI images to Quay; **Kargo + Argo CD**
> then progressively deliver the artifacts.

It evaluates both against the MVP's four stated goals, recommends a direction,
and records the rationale and trade-offs. It builds directly on two prior
reports that already studied the delivery mechanism:

- [Research: Handling Image-Tag Updates in Argo CD with an OCI Manifest Source](argocd-oci-image-tag-updates.md)
  — concluded "native OCI + controller patch" for the MVP, with **"Kargo as the
  designated growth path."**
- [Research: Performing the Re-render + ORAS Publish Step](rendered-manifests-publish-pipeline.md)
  — established the decisive constraint reused throughout this report: **no
  off-the-shelf tool renders Holos CUE to an OCI artifact without Git, so
  someone must run `holos render` + publish regardless.**

## 2. The MVP goals (the rubric)

From HOL-1234, the MVP must:

1. **Heroku-like simplicity** — push to deploy and nothing more.
2. **Holos config layering** — Security, Platform, and SRE teams must have a
   clear place to compose their own configuration and constraints (not built for
   the demo, but the story must be obvious).
3. **May use a local CLI** to build the app image, call `holos` to render a new
   version, build an OCI image of plain k8s manifests, and let **Kargo receive
   the Quay webhook** to progressively deliver.
4. **Self-service tenancy** — a user calls a self-service API to get a project
   (Namespace) they can write to via the k8s API, using an OIDC ID token from
   the Keycloak `holos` realm and a minimal service that authenticates the token
   and injects Kubernetes impersonation headers.

Goals **1–3** are about the *delivery pipeline* — the surface this report
proposes to change. Goal **4** is about *tenancy and identity*, which is
**orthogonal** to the pipeline choice and is preserved unchanged (see [§6](#6-what-does-not-change-tenancy-identity-self-service)).

## 3. What the current architecture actually builds

The current ADRs decide a six-stage, event-driven pipeline on NATS JetStream,
with **Quay as the single event source** feeding two consecutive loops
(ADR-13). The end-to-end flow:

```
Loop 1 (render):
  docker push app:v2
    → Quay app repo fires repo_push webhook
    → Webhook Receiver (ADR-9): POST /webhooks/quay → publish RAW body to
      webhooks.quay on the WEBHOOKS WorkQueue stream (file-backed JetStream)
    → Webhook Subscriber (ADR-10): drain WEBHOOKS, parse Quay JSON, match
      Application, publish a RenderTask (protobuf, ADR-14) to tasks.render
    → Render Subscriber (ADR-13): resolve tag→digest, run
      `holos render platform --inject app_image=...@sha256:...`,
      `oras push` the rendered deploy/ tree to a sibling <app>-config Quay repo
    ── the config-image push re-enters the pipeline ──

Loop 2 (deploy):
    → Quay config repo fires repo_push webhook → Webhook Receiver → WEBHOOKS
    → Webhook Subscriber: parse, resolve digest, publish a DeployTask to tasks.deploy
    → Deployer (ADR-11): patch the Application CR's config version, ack
    → Application Controller: patch the Argo CD Application targetRevision = digest
    → Argo CD: pull oci://.../<app>-config@sha256:..., apply, new version rolls out
```

The **custom code and operational surface** this implies:

| Piece | ADR | What it is | Status |
|---|---|---|---|
| Webhook receiver | 9 | Custom Go service: HTTP → raw body → NATS `webhooks.<source>` | **Built & deployed** (HOL-1196/1198) |
| Webhook subscriber | 10 | Custom Go service: parse Quay JSON, match, dispatch task | **Built** (HOL-1201/1206) |
| Render subscriber | 13 | Custom Go service: `holos render` + `oras push` (the slow stage) | Designed |
| Deployer | 11 | Custom Go service: patch `Application` config version | Designed |
| Application controller | 11 | Custom CRD + reconciler: patch Argo CD `targetRevision` | Designed |
| NATS JetStream backbone | 6 | StatefulSet + bootstrap Job; `WEBHOOKS` & `TASKS` WorkQueue streams; PVC; no in-cluster auth (MVP); per-service `AuthorizationPolicy` | **Deployed** |
| Task contract | 14 | `.proto` source of truth, generated Go, `make generate`, `buf breaking` CI gate; `QuayRepositoryPush`/`RenderTask`/`DeployTask` | Partially built |

That is **three-to-four custom Go microservices, a stateful message broker, and
a maintained wire protocol** — all in service of moving one fact ("a new tag
exists") from Quay to Argo CD. This is the over-engineering the maintainer is
reacting to, and the reaction is well-founded: the receiver/subscriber/deployer
are generic plumbing, not platform-differentiating logic.

### 3.1 The one piece that is genuinely irreducible

The [rendered-manifests-publish-pipeline report](rendered-manifests-publish-pipeline.md)
established the load-bearing constraint:

> Holos renders to a local directory; publishing is explicitly someone else's
> job. **No surveyed tool does CUE-render-to-OCI off the shelf without Git** …
> the comparison is really about *where our code runs*, not *whether to write
> it.*

So **the render+publish step must be custom code in any design.** The question
this report answers is therefore not "custom code or no custom code," but **how
much** custom code, **where it runs** (a server-side subscriber vs. a
client/CI-side CLI), and **how much of the surrounding plumbing can be deleted.**

## 4. The alternative: a CLI + Kargo + Argo CD

The proposed architecture collapses the two loops into **one client action and
one off-the-shelf controller**:

```
Developer / CI runs ONE command:  holos-paas deploy   (or: holos-paas up)
  1. docker build + docker push  app:v2              → Quay app repo
  2. holos render platform --inject app_image=...@sha256:<digest>
  3. oras push the rendered deploy/ tree             → Quay <app>-config repo
        (steps 2–3 are the irreducible render+publish, now CLIENT-SIDE)

Kargo (off the shelf, declarative CRDs — no custom code):
  → Quay webhook receiver refreshes the Warehouse on the push
  → Warehouse produces Freight for the new artifact
  → Stage auto-promotes: the argocd-update step sets the Argo CD
    Application's targetRevision and triggers a sync
  → Argo CD: pull oci://.../<app>-config@<digest>, apply, roll out
```

What this **deletes** from the MVP:

- ❌ Webhook receiver (ADR-9) — Kargo has a **native Quay webhook receiver**
  (`ProjectConfig.spec.webhookReceivers[].quay`), introduced in Kargo **v1.6**
  (2025-07). **Caveat on the semantics:** webhook support landed as beta with
  filtering limitations, and the receiver **triggers an immediate Warehouse
  refresh** (a poll-now signal) rather than durably persisting the raw event the
  way the NATS WorkQueue receiver does. So it replaces the *function* (registry
  push → pipeline reacts) but not the *durability contract* — Kargo's safety net
  is the Warehouse poll interval, not a retained at-least-once queue. For the MVP
  that is an acceptable trade (the polling fallback still catches a missed
  webhook), but it is not a byte-for-byte equivalent and should be verified
  against the installed version's stability guarantees.
- ❌ Webhook subscriber (ADR-10) — Kargo's **Warehouse** subscribes to the
  registry and produces **Freight** automatically; no custom parser/matcher.
- ❌ Deployer + the deploy half of the Application controller (ADR-11) — Kargo's
  **`argocd-update`** promotion step writes `Application.spec.source.targetRevision`
  and forces a sync, **fully Git-free**. This is exactly the deployer's job.
- ❌ NATS JetStream backbone (ADR-6) — the event bus existed only to couple the
  custom stages; Kargo's controller + webhook receiver replace it.
- ❌ The protobuf task contract + buf toolchain (ADR-14) — `RenderTask`/`DeployTask`
  exist only to carry messages between custom stages; they disappear with the
  stages.

What this **keeps** (the irreducible + the differentiating):

- ✅ The **`holos render` + OCI publish** step — moved into the CLI (client/CI
  side) instead of a server-side subscriber.
- ✅ The **Holos rendering layer** and the entire `holos/` CUE tree — untouched.
- ✅ **Argo CD** with OCI sources — already an MVP dependency (ADR-13).
- ✅ **Quay** as registry and event source — unchanged.
- ✅ Tenancy/identity (ADR-1–5, 12, 15) — unchanged (see [§6](#6-what-does-not-change-tenancy-identity-self-service)).

### 4.1 Minimal Kargo configuration (zero custom code)

Per the Kargo research, a "push a tag → it deploys" experience is a Warehouse +
project-level auto-promotion + a single Stage with `argocd-update` + a Quay
webhook receiver:

```yaml
# Warehouse: watch the rendered-manifests artifact in Quay
apiVersion: kargo.akuity.io/v1alpha1
kind: Warehouse
metadata: { name: checkout, namespace: acme-store }
spec:
  interval: 5m            # poll fallback; the webhook makes it near-instant
  subscriptions:
  - chart:                # see §5: package rendered manifests as an OCI Helm chart
      repoURL: oci://quay.holos.internal/acme-store/checkout-config
      semverConstraint: ">=0.0.0"
---
apiVersion: kargo.akuity.io/v1alpha1
kind: ProjectConfig
metadata: { name: acme-store, namespace: acme-store }
spec:
  promotionPolicies:
  - stageSelector: { name: prod }   # field spelling has drifted across releases; verify against the installed CRD
    autoPromotionEnabled: true
  webhookReceivers:
  - name: quay
    quay: { secretRef: { name: quay-webhook-secret } }
---
apiVersion: kargo.akuity.io/v1alpha1
kind: Stage
metadata: { name: prod, namespace: acme-store }
spec:
  requestedFreight:
  - origin: { kind: Warehouse, name: checkout }
    sources: { direct: true }
  promotionTemplate:
    spec:
      steps:
      - uses: argocd-update
        config:
          apps:
          - name: checkout
            namespace: argocd
            sources:
            - repoURL: oci://quay.holos.internal/acme-store/checkout-config
              desiredRevision: ${{ chartFrom("oci://quay.holos.internal/acme-store/checkout-config").Version }}
              updateTargetRevision: true
```

The Argo CD `Application` must carry `kargo.akuity.io/authorized-stage:
"acme-store:prod"`. Adding a `staging` Stage upstream of `prod`, manual
promotion, and an `AnalysisTemplate` verification gate is incremental
configuration — not new code — which is precisely the deferred separation-of-duty
and multi-environment story (ADR-11) arriving for free.

## 5. The critical compatibility gap (and how to close it)

Kargo Warehouse subscriptions support **only images, OCI Helm charts, and Git**
— **not arbitrary OCI manifest bundles**
([akuity/kargo#1099](https://github.com/akuity/kargo/issues/1099), open/low-priority).
The current design publishes rendered manifests as a *generic* single-layer OCI
artifact (`application/vnd.oci.image.layer.v1.tar+gzip`, per
[argocd-application-source.md](../../holos/docs/argocd-application-source.md)),
which a Warehouse **cannot watch today.** This is the load-bearing risk in the
pivot, and there are three clean resolutions:

1. **Package rendered manifests as an OCI Helm chart** (a supported Warehouse
   subscription type). Holos can render into a trivial chart wrapper (a
   `Chart.yaml` plus the rendered YAML under `templates/`, no values
   templating). This is the cleanest fit and is the assumption in the §4.1
   sketch. **Recommended — with one constraint:** Kargo `chart` subscriptions
   select by **SemVer** (`semverConstraint`), but ADR-8 makes the *image tag*
   the version and does **not** require SemVer (git SHAs, `latest`, and other
   immutable non-SemVer tags are allowed). So the CLI must stamp the wrapper
   chart with a **SemVer `version`** — either by requiring SemVer app tags, or
   by deriving a synthetic chart version (e.g. a monotonic build number or a
   SemVer+build-metadata encoding of the digest) and recording the mapping back
   to the source tag. Without that mapping scheme the "cleanest fit" does not
   hold for arbitrary tags; pin this down in the spike. (Triggering on the app
   *image* via resolution 2 sidesteps the chart-version requirement but
   reintroduces the bundle/image-digest decoupling.)
2. **Trigger off the application image instead of the manifest bundle.** The
   Warehouse watches `quay.../checkout` (an image — natively supported); the
   `argocd-update` step bumps the OCI-sourced `Application`. The downside is the
   *bundle digest* and the *image tag* are decoupled — Kargo knows the image,
   not the rendered-manifests digest. Workable but less precise.
3. **Wait for/contribute generic-OCI Warehouse support** (#1099). Not advisable
   to depend on for the MVP.

A second item to **verify in a test cluster, not assume:** `argocd-update`
writing `targetRevision` against an **OCI-sourced** Argo CD `Application`. The
step is documented for Git/Kustomize/Helm sources; OCI is `targetRevision`-based
and *should* work, but the official step doc does not enumerate OCI sources. A
half-day spike on k3d confirms or refutes this and is the single most important
de-risking task before committing.

> Both of these are exactly what the **"Kargo Spike"** milestone this issue
> belongs to is for.

## 6. What does NOT change: tenancy, identity, self-service

The pivot is scoped to the **delivery pipeline**. The MVP's self-service tenancy
story (goal 4) is untouched and still requires the platform's own code:

- **`Project` CRD + reconciler** (ADR-1/4) — the tenant boundary that maps to a
  Namespace; the unit of RBAC, quota, and chargeback. **No Kargo/Argo CD
  analog.**
- **authproxy** (ADR-3/12) — the minimal service that authenticates the
  Keycloak-issued OIDC ID token and injects Kubernetes impersonation headers so
  `kubectl` writes directly to the API server as the user. **This is goal 4's
  "minimal service" verbatim, and it is unaffected by the pipeline choice.**
- **Kubernetes RBAC + Keycloak group membership** (ADR-3) — authorization scoped
  to the `Project`.
- **Keycloak/Quay self-service reconcilers** (ADR-12) and **Quay↔Keycloak OIDC
  SSO** (ADR-15, `Implemented`) — identity integration, orthogonal to delivery.

Kargo and Argo CD both integrate with the existing Keycloak identity layer
rather than competing with it — they consume OIDC SSO and map group claims to
their own RBAC. **Argo CD is already wired** to the `holos` realm in this
project; **Kargo is not** — it currently exists only as planned/aspirational
docs and vendored generated CUE types, with no deployed Holos component or
Keycloak client. Adopting Kargo therefore includes a one-time wiring cost: a
Holos component to deploy it and a Keycloak OIDC client plus group→role mapping,
following the same declarative pattern already used for Argo CD and Quay
(ADR-15). This is configuration, not custom Go code, but it is real scope and is
accounted for in the trade-offs ([§8](#8-trade-offs-honest-accounting)).

This matters for the over-engineering question: the genuinely
platform-differentiating custom code (tenancy, identity, self-service) is the
*least* built today, while the *most* built code (the pipeline plumbing) is the
*most* replaceable. The pivot lets the team **stop investing in the replaceable
half and redirect that effort to the differentiating half.**

## 7. Recommendation

**Pivot the MVP to a CLI + Kargo + Argo CD.** Concretely:

1. **Adopt Kargo** for registry-watching, webhook ingest, and Argo CD
   `Application` promotion. Retire the webhook receiver (ADR-9), webhook
   subscriber (ADR-10), deployer (ADR-11), NATS JetStream backbone (ADR-6), and
   the protobuf task contract (ADR-14) from the MVP scope.
2. **Move `holos render` + OCI publish into a CLI** (`holos-paas deploy`) that
   runs in the developer's or CI's context: build+push the app image, render,
   and push the rendered-manifests artifact **as an OCI Helm chart** (per
   [§5](#5-the-critical-compatibility-gap-and-how-to-close-it), resolution 1).
3. **Keep** the Holos rendering layer, Argo CD OCI sources, Quay, and the entire
   tenancy/identity stack (ADR-1–5, 12, 15) unchanged.
4. **De-risk first:** spend the Kargo Spike milestone confirming (a)
   `argocd-update` against an OCI-sourced `Application`, and (b) a Warehouse
   subscription to the rendered-manifests OCI Helm chart. These two facts
   determine whether the pivot is as clean as it appears.

### 7.1 Why the CLI, and how it honors "push to deploy"

Goal 1 ("docker push and nothing more") and goal 3 ("may use a CLI") are in mild
tension only if read literally. The render step is irreducible custom code that
must run *somewhere*; the choice is **a server-side render subscriber** (more
operational surface, but pure `docker push`) vs. **a client/CI-side CLI** (zero
custom server code, but the dev runs one command). For an MVP whose explicit
aim is the *minimum* viable experience and whose maintainer is worried about
over-engineering, **the CLI is the smaller, faster, more honest choice**:

- It deletes **all** custom server processes from the deploy path (nothing to
  run, scale, monitor, or secure server-side except Kargo, which is a product).
- A single `holos-paas deploy` is still "push to deploy" in the developer's
  mental model — the `git push`-equivalent one-liner — with no tickets, no YAML
  editing, no `kubectl`. The Heroku CLI itself is a CLI.
- It keeps the irreducible render where it is cheapest to operate (the build
  context already has the source and the image).

If "**pure** `docker push` as the *only* developer action" later becomes a hard
requirement, the fallback is a **thin render service** triggered by Kargo (the
one custom service that survives), with Kargo still replacing the receiver,
subscriber, deployer, and NATS. That is option **C** below — strictly more
server code than the recommended CLI, kept as a documented escape hatch.

### 7.2 The three options, side by side

| | A. Stay the course (current ADRs) | **B. CLI + Kargo (recommended)** | C. Pure docker-push + Kargo |
|---|---|---|---|
| Dev action | `docker push` | `holos-paas deploy` (one cmd) | `docker push` |
| Custom server code | receiver, subscriber, render sub, deployer, NATS, protobuf | **none** (render runs in CLI/CI) | **one** render service |
| Off-the-shelf delivery | none (Argo CD sync only) | Kargo + Argo CD | Kargo + Argo CD |
| Replaces deferred SoD / multi-env story | build it later | **free with Kargo Stages/gates** | free with Kargo |
| Operational surface | highest | **lowest** | low |
| Tenancy/identity (goal 4) | unchanged | unchanged | unchanged |
| Conflicts with ADR-2 (KRM-as-primary-API) | no | **yes — needs an ADR** | minor (render trigger only) |

## 8. Trade-offs (honest accounting)

**In favor of the pivot:**

- **Far less custom code to write and operate.** Three-to-four Go services + a
  stateful broker + a wire protocol become declarative Kargo CRDs.
- **The deferred roadmap arrives for free.** Kargo Stages, promotion policies,
  Argo Rollouts `AnalysisTemplate` verification, and manual approval gates
  directly realize ADR-11's deferred separation-of-duty and a future
  dev→staging→prod story — with no additional code.
- **A UI, RBAC, and audit trail** come with Kargo (Apache-2.0 OSS, GA since
  2024-10, current ~v1.10), versus a headless custom pipeline.
- **ADR-11 already blessed this direction** as the growth path; this report's
  contribution is showing the growth path is *cheaper than the MVP it would
  replace*, so there is little reason to build the interim plumbing first.

**Against / risks:**

- **It contradicts ADR-2** (KRM is the primary, supported API; a non-KRM
  interface such as a CLI requires an ADR that eliminates the KRM as viable).
  Adopting a deploy CLI is therefore an **ADR-level reversal**, not a config
  tweak — it needs a new/revised ADR with the rationale. (Note: the *platform*
  API stays KRM — `Project`/`Application` CRDs, `kubectl` via authproxy. Only
  the *deploy trigger* becomes a CLI, and Kargo's own surface is itself KRM
  CRDs, so the KRM-native spirit is largely preserved.)
- **New dependency, mental model, and wiring cost.** Kargo is another controller
  to run and a Warehouse/Freight/Stage model to learn, and — unlike Argo CD — it
  is **not yet deployed or wired** in this project: adopting it adds a Holos
  component to deploy it plus a Keycloak OIDC client and group→role mapping
  (configuration following the existing Argo CD/Quay pattern, not custom Go).
  For a one-machine k3d demo this is real but modest, and Kargo is a *product*
  the team consumes rather than *code* the team maintains.
- **The OCI-manifest-bundle gap** ([§5](#5-the-critical-compatibility-gap-and-how-to-close-it))
  requires repackaging rendered manifests as an OCI Helm chart (or triggering on
  the image). This is small but must be validated in the spike.
- **Two unverified facts** (`argocd-update` on OCI sources; Helm-chart Warehouse
  of rendered manifests) gate the clean path. Both are spike-sized.
- **Sunk cost.** The receiver and subscriber are already built and deployed
  (HOL-1196/1198/1201/1206). Retiring working code is a real (if healthy) cost.

**Net:** the trade is "**learn and run one off-the-shelf product**" in exchange
for "**delete three-to-four custom services, a message broker, and a wire
protocol, and get progressive delivery for free.**" For an MVP explicitly
chartered as *minimum* viable, this is the right trade — contingent on the two
spike validations.

## 9. How Holos config layering still fits (goal 2)

The pivot does **not** disturb the Holos config-management story; it clarifies
where it lives. The Holos render is the single point where the **Security,
Platform, and SRE** layers compose, and its output — the rendered-manifests OCI
artifact — is what Kargo/Argo CD deliver. The reference platform at
`~/workspace/holos-reference/holos` demonstrates the idiom this MVP should grow
into:

- **Cross-cutting layers are ordinary Holos components** attached to every
  cluster inside a `for CLUSTER in config.clusters` loop, parameterized by
  `config`, gated by CUE conditionals, and addressable as a whole via
  `holos.run/component-set.<name>` labels with `component-seq` ordering
  annotations (`platform/platform.cue`).
- **Security** composes in two places: **render-time** Holos CUE validators
  (typed schema policy — e.g. reject raw `Secret`s, require `ExternalSecret`)
  and **admission-time** policy (the reference platform uses Istio ambient-mesh
  mTLS/`AuthorizationPolicy` + Wiz/SentinelOne agents; the demo narrative adds
  Kyverno + cosign). Either way the policy is *layered into the rendered bundle*
  by Holos, so it travels with every deploy.
- **SRE/observability** composes the same way: the reference platform attaches a
  Datadog component (with an OTLP receiver) to every cluster and weaves
  Unified Service Tagging labels into every workload via shared templates and a
  mandatory conformance checklist. Prometheus/Grafana would slot in identically
  as components.
- **Platform** owns the bootstrap ordering, the cluster registry, namespace mesh
  enrollment (`istio.io/dataplane-mode`), and the golden-path render pipeline.

In the recommended design, **the CLI's `holos render platform` step is the seam
where all three layers are mixed in**: the developer supplies only the app
image; Security/Platform/SRE configuration is composed by the platform's CUE and
emerges in the rendered bundle that Kargo delivers. That is the "clear place and
story" goal 2 asks for — and it is identical whether delivery is the custom NATS
pipeline or Kargo. The pivot costs nothing here.

## 10. Suggested next steps

1. **Run the Kargo Spike** (this milestone) on k3d to validate the two facts in
   [§5](#5-the-critical-compatibility-gap-and-how-to-close-it): `argocd-update`
   against an OCI-sourced `Application`, and a Warehouse subscription to the
   rendered-manifests OCI Helm chart.
2. **Prototype `holos-paas deploy`** — wrap `docker build/push` + `holos render
   platform --inject` + ORAS publish (as an OCI Helm chart) into one command.
3. **Write an ADR** recording the decision to adopt a deploy CLI + Kargo,
   explicitly reconciling it with ADR-2 (KRM-as-primary-API) and superseding the
   pipeline scope in ADR-6/9/10/11/13/14. Revise those ADRs' status/revision
   tables rather than leaving them as the implied MVP.
4. **Keep ADR-1–5, 12, 15 as-is** and continue the tenancy/identity/self-service
   work — that is where the platform's differentiating value lives.

## Sources

- Internal: [ADR-6](../adr/archive/ADR-6.md), [ADR-9](../adr/archive/ADR-9.md),
  [ADR-10](../adr/archive/ADR-10.md), [ADR-11](../adr/archive/ADR-11.md),
  [ADR-13](../adr/archive/ADR-13.md), [ADR-14](../adr/archive/ADR-14.md),
  [ADR-1](../adr/archive/ADR-1.md)–[ADR-5](../adr/archive/ADR-5.md), [ADR-12](../adr/ADR-12.md),
  [ADR-15](../adr/ADR-15.md); [argocd-oci-image-tag-updates.md](argocd-oci-image-tag-updates.md);
  [rendered-manifests-publish-pipeline.md](rendered-manifests-publish-pipeline.md);
  [holos/docs/argocd-application-source.md](../../holos/docs/argocd-application-source.md);
  [holos/docs/placeholders.md](../../holos/docs/placeholders.md);
  reference platform `~/workspace/holos-reference/holos` (`platform/platform.cue`,
  `components/datadog/`, `docs/component-guidelines.md`).
- Kargo: [Architecture](https://docs.kargo.io/operator-guide/architecture) ·
  [Working with Warehouses](https://docs.kargo.io/user-guide/how-to-guides/working-with-warehouses) ·
  [Working with Stages](https://docs.kargo.io/user-guide/how-to-guides/working-with-stages) ·
  [`argocd-update` step](https://docs.kargo.io/user-guide/reference-docs/promotion-steps/argocd-update) ·
  [Webhook Receivers](https://docs.kargo.io/user-guide/reference-docs/webhook-receivers/) ·
  [Quay Receiver](https://docs.kargo.io/user-guide/reference-docs/webhook-receivers/quay/) ·
  [Verification](https://docs.kargo.io/user-guide/how-to-guides/verification/) ·
  [What's New in v1.6 (webhooks)](https://akuity.io/blog/what-s-new-in-kargo-v1-6) ·
  [Kargo 1.0 GA](https://akuity.io/blog/announcing-kargo-version-1-0-now-generally-available-on-the-akuity-platform) ·
  [akuity/kargo#1099 (generic OCI subscriptions)](https://github.com/akuity/kargo/issues/1099).
- Argo CD: [OCI source support (≥ 3.1)](https://argo-cd.readthedocs.io/) ·
  Argo Rollouts [Analysis](https://argo-rollouts.readthedocs.io/en/stable/features/analysis/).
