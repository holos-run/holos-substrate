# Research: Handling Image-Tag Updates in Argo CD with an OCI Manifest Source

| Metadata | Value                                   |
|----------|-----------------------------------------|
| Date     | 2026-06-09                              |
| Author   | @jeffmccune                             |
| Status   | Informational (informs ADR-6, 8, 10, 11) |
| Tags     | argocd, oci, gitops, deployer, research |

> **Follow-up:** this report leaves open who *produces* the rendered-manifests
> artifact when a new app image arrives. That question is answered in
> [Research: Performing the Re-render + ORAS Publish Step in the Event-Driven Pipeline](../archive/rendered-manifests-publish-pipeline.md).

## Purpose

This report researches the state-of-the-art, open-source, Kubernetes-native
methods for updating a deployed application's **image tag** in **Argo CD**, under
the constraints the platform imposed at the time. It exists to **inform the ADRs** for
the deployment pipeline — primarily [ADR-11](../adr/archive/ADR-11.md) (deployer) and
[ADR-8](../adr/archive/ADR-8.md) (registry/tagging), and the pipeline overview in
[ADR-6](../adr/archive/ADR-6.md).

## Constraints (the problem framed for our context)

- We use the **Holos rendered-manifests pattern**: Holos renders fully-resolved
  Kubernetes YAML; there is no in-cluster templating. Manifests are normally
  committed to Git (GitHub).
- For the MVP we **defer syncing the version back to GitHub**. **The MVP must not
  depend on GitHub.**
- We therefore **assume Argo CD syncs manifests from an OCI image instead of
  GitHub**, using Argo CD's first-class OCI source support.
- We want the **top 3** methods that are **open source** and **Kubernetes
  native**, with the reasons each is picked.

### The crucial framing: two different images, one "version"

There are **two** OCI artifacts in play, and conflating them causes most of the
confusion in this space:

1. The **application image** — e.g. the KubeRay app container
   ([ADR-7](../adr/archive/ADR-7.md)), pushed to a registry with a tag
   ([ADR-8](../adr/archive/ADR-8.md)). Its tag *is* the version.
2. The **rendered-manifests artifact** — the fully-rendered YAML produced by
   `holos render`, packaged as an OCI artifact (via ORAS) and synced by Argo CD
   as its Application source.

> **ORAS** ([OCI Registry As Storage](https://oras.land/)) is the CNCF tool and
> set of client libraries for pushing and pulling **arbitrary artifacts** — not
> just container images — to any OCI-compliant registry. It packages content
> (here, a tarball of rendered YAML) as an OCI artifact with a media type the
> consumer recognizes; Argo CD's OCI source accepts ORAS's default layer media
> type (`application/vnd.oci.image.layer.v1.tar+gzip`) for plain manifests. ORAS
> is how the rendered-manifests artifact gets *into* the registry that Argo CD
> syncs *from*.

Because Holos **bakes the application image tag into the rendered manifests at
render time**, the manifests artifact is *specific to* an application image
version. Consequently, in this architecture the act of "update the image tag"
becomes: **produce/select the rendered-manifests artifact for the new app image,
then point the Argo CD `Application` at that artifact** — i.e. update the
Application's `targetRevision` (and/or `path`) to the new OCI tag or digest. Argo
CD's OCI source accepts **either a tag or an immutable digest** in
`targetRevision`, and supports **plain manifests** (not just Helm), which is
exactly the Holos output.

This reframing matters for ranking the options below: the methods that can change
**`targetRevision`/source** fit the rendered-manifests pattern; methods that can
only change **Helm/Kustomize parameters** do not, because there is no parameter
to change in fully-rendered YAML.

> First-class OCI source (including the plain-manifest format) is a recent Argo
> CD capability, hardened through the 3.x line (the native-OCI-support proposal
> and the 3.1 rollout). This report assumes Argo CD ≥ 3.1.

---

## Top 3 options

### Option 1 — Argo CD native OCI source + controller-driven `targetRevision` update (recommended)

**What it is.** Use Argo CD's first-class OCI source: the `Application` points at
`oci://…` with `targetRevision` set to the rendered-manifests artifact's tag or
digest. When a new app image is deployed, the **holos-controller deployer**
([ADR-11](../adr/archive/ADR-11.md)) patches the Argo CD `Application` resource's
`targetRevision` to the new artifact version. No extra deployment components; no
Git.

**How the update happens.** A Kubernetes `PATCH` to the Argo CD `Application` CR
(or to the holos-controller's own `Application` CR, whose reconciler owns the
mapping). Argo CD detects the changed `targetRevision`, pulls the new OCI
artifact, and syncs.

**Why it's picked.**
- **Most Kubernetes-native and fewest moving parts** — the update is an ordinary
  CR write, which the deployer already performs in [ADR-11](../adr/archive/ADR-11.md)
  ("for the MVP the deployer can simply update the Application resource").
- **No GitHub dependency** by construction — the source of truth for "which
  version is live" is the `Application.targetRevision`, set in-cluster.
- **Directly aligned with the Holos rendered-manifests pattern** — the version is
  the manifests artifact, and `targetRevision` selects it; nothing relies on
  in-cluster templating.
- **Uses immutable digests** for `targetRevision`, giving exact, auditable
  "what's deployed" semantics.

**Limitations / honest caveats.**
- Directly patching the Argo CD `Application` is **imperative**: if Argo CD's own
  Application manifests are themselves GitOps-managed later, the patched
  `targetRevision` must be reconciled with that source (this is precisely the Git
  write-back that the MVP defers — see [ADR-11](../adr/archive/ADR-11.md)).
- The controller must own the logic to map "new app image tag → manifests
  artifact version," including re-rendering/publishing the artifact if that is
  not done upstream.

---

### Option 2 — Kargo (promotion engine)

**What it is.** [Kargo](https://docs.kargo.io/) (open source, from the Akuity /
Argo CD team) is a Kubernetes-native multi-stage promotion engine. A
**`Warehouse`** watches an image repository and emits **`Freight`** when a new
tag appears; a **`Stage`** runs **promotion steps** to roll that Freight out and
**update the Argo CD `Application`**.

**How the update happens.** Kargo's **`argocd-update`** promotion step "updates
one or more Argo CD `Application` resources," **including `targetRevision` and
sources** — and it can run **fully Git-free** (no `git-clone`/`git-commit`/
`git-push`). Its **`oci-push`** step can copy/retag OCI artifacts between
registries. So a Git-free Kargo flow is: Warehouse detects the new image →
(optionally) re-render/retag the manifests artifact → `argocd-update` sets the
`Application`'s `targetRevision` → `argocd-wait` confirms the sync.

**Why it's picked.**
- **Purpose-built for exactly this problem** — "new image appears → promote →
  update Argo CD" — with first-class CRDs (`Warehouse`, `Freight`, `Stage`,
  `Promotion`), so it is fully Kubernetes-native and declarative.
- **Supports a Git-free path today** via `argocd-update`, satisfying the
  no-GitHub constraint without giving up an auditable promotion record (Freight
  history lives in cluster state).
- **Grows into the deferred roadmap.** Kargo's stages, verification, and manual
  approval gates map directly onto [ADR-11](../adr/archive/ADR-11.md)'s deferred
  **separation-of-duty** control and onto a future multi-environment promotion
  story — adopting it for the MVP buys optionality.
- Handles **OCI retagging** natively, which fits a registry-centric
  ([ADR-8](../adr/archive/ADR-8.md)) pipeline.

**Limitations / honest caveats.**
- **Heaviest of the three** — a new controller and a new mental model
  (Warehouse/Freight/Stage) to operate, which is a lot for an MVP whose goal is
  the *minimum* viable experience.
- The most documented Kargo patterns are **Git-write-back**; the Git-free path is
  supported but less trodden, so expect to do more design work to keep it
  GitHub-free.

---

### Option 3 — Argo CD Image Updater

**What it is.** [Argo CD Image Updater](https://argocd-image-updater.readthedocs.io/)
(argoproj-labs, open source) is the canonical tool for "watch a registry and
update Argo CD when a new tag appears." It offers two write-back methods:
**`argocd`** (in-cluster, no Git) and **`git`** (commits to the repo).

**How the update happens.** With the **`argocd` write-back method** it modifies
the `Application` in-cluster — **no Git dependency** — and is the obvious
candidate under our constraint.

**Why it's on the list.**
- It is the **state-of-the-art reference** for image-tag automation in Argo CD;
  any decision record must address why it is or isn't used.
- Its **`argocd` method is genuinely Git-free**, matching the no-GitHub
  constraint, and it is trivial to deploy.

**Why it ranks third — the fit caveat (important).**
- The `argocd` write-back method **updates Helm parameters / Kustomize images
  only**. Per the upstream docs it is "essentially equivalent to `argocd app set
  --parameter …`" and **cannot modify `targetRevision`, the OCI source, or other
  spec fields.**
- The **Holos rendered-manifests pattern produces plain YAML with the image tag
  already baked in** — there is **no Helm/Kustomize parameter for Image Updater
  to override**. So with fully-rendered manifests synced from OCI, Image
  Updater's Git-free method **has nothing to act on**.
- It would only fit if we **retained a thin Helm/Kustomize image-override layer**
  in Argo CD instead of fully-rendered manifests — which **contradicts the Holos
  rendered-manifests pattern** ([ADR-6](../adr/archive/ADR-6.md)). Its `git` method *can*
  update rendered manifests, but that **reintroduces the GitHub dependency the
  MVP forbids.**
- Its `argocd`-method changes are also **"pseudo-persistent"** (lost if the
  Application is recreated or re-synced from its declarative source).

In short: excellent tool, **mismatched with "rendered manifests + OCI + no Git."**
Documented here so the choice not to lead with it is explicit and revisitable.

---

## Comparison

| Capability                                   | 1. Native OCI + controller patch | 2. Kargo            | 3. Image Updater (`argocd`) |
|----------------------------------------------|----------------------------------|---------------------|-----------------------------|
| Open source, Kubernetes-native               | ✅                               | ✅                  | ✅                          |
| Works with **no GitHub dependency**          | ✅                               | ✅ (`argocd-update`)| ✅ (but see fit)            |
| Can set Argo CD `targetRevision` / OCI source| ✅                               | ✅                  | ❌ (params only)            |
| Fits **fully-rendered** manifests (no overlay)| ✅                              | ✅                  | ❌ (needs Helm/Kustomize)   |
| Watches registry for new tags itself         | ➖ (deployer/pipeline does)      | ✅ (`Warehouse`)    | ✅                          |
| Extra components to operate                   | None (controller only)          | Kargo controller    | Image Updater controller    |
| Path to deferred separation-of-duty           | manual                           | ✅ (Stages/gates)   | ➖                          |

## Recommendation

For the MVP, **lead with Option 1**: Argo CD native OCI source with the
holos-controller deployer updating the `Application`'s `targetRevision`. It is the
smallest, most Kubernetes-native change, it has **no GitHub dependency**, and it
is the literal realization of [ADR-11](../adr/archive/ADR-11.md)'s "the deployer can
simply update the Application resource."

**Keep Option 2 (Kargo) as the designated growth path.** When the milestone needs
registry-watching, multi-stage promotion, or the deferred separation-of-duty gate
([ADR-11](../adr/archive/ADR-11.md)), adopt Kargo's `Warehouse` + `argocd-update`
(Git-free) rather than building those primitives in the controller.

**Do not adopt Option 3 (Image Updater) for the rendered-manifests path**, because
its Git-free method cannot touch `targetRevision`/OCI and its Git method
reintroduces the forbidden GitHub dependency. Revisit only if we deliberately keep
a Helm/Kustomize image-override layer.

## How this informs the ADRs

- **[ADR-6](../adr/archive/ADR-6.md) (pipeline):** the terminal stage targets Argo CD via
  an **OCI manifest source**, not Git; the MVP is GitHub-independent end to end.
- **[ADR-8](../adr/archive/ADR-8.md) (registry/tagging):** there are **two** artifacts —
  the app image and the rendered-manifests OCI artifact; prefer **immutable
  digests** so `targetRevision` is exact. The registry is the source of truth for
  the version.
- **[ADR-10](../adr/archive/ADR-10.md) (subscriber):** the deployer-task message should
  carry enough to resolve the **manifests artifact version** (digest/tag), not
  just the app image tag, so the deployer can set `targetRevision`.
- **[ADR-11](../adr/archive/ADR-11.md) (deployer):** adopt **Option 1** for the MVP —
  patch the Argo CD `Application.targetRevision` to the new OCI artifact. Record
  **Kargo** as the chosen mechanism for the **deferred** Git write-back and
  separation-of-duty work, and record **why Image Updater is not used** with
  rendered manifests.

## Sources

- [Argo CD — OCI (user guide)](https://argo-cd.readthedocs.io/en/latest/user-guide/oci/)
- [Argo CD — first-class OCI support (proposal)](https://argo-cd.readthedocs.io/en/stable/proposals/native-oci-support/)
- [Using the new OCI support as an application source (Argo CD discussion #24488)](https://github.com/argoproj/argo-cd/discussions/24488)
- [Add OCI Generator for ApplicationSets (Argo CD issue #26055)](https://github.com/argoproj/argo-cd/issues/26055)
- [How to Use OCI Artifacts as Application Sources in ArgoCD](https://oneuptime.com/blog/post/2026-02-26-argocd-oci-artifacts-application-sources/view)
- [Argo CD Image Updater — Update methods](https://argocd-image-updater.readthedocs.io/en/stable/basics/update-methods/)
- [Argo CD Image Updater — Git write-back system (DeepWiki)](https://deepwiki.com/argoproj-labs/argocd-image-updater/2.4-git-write-back-system)
- [Kargo — Promotion steps reference](https://docs.kargo.io/user-guide/reference-docs/promotion-steps)
- [Kargo — Patterns](https://docs.kargo.io/user-guide/patterns)
- [GitOps Promotion with Kargo: Image Tag → Git Commit → Argo Sync](https://dev.to/josephcc/gitops-promotion-with-kargo-image-tag-git-commit-argo-sync-39go)
- [Holos — Holistic platform manager](https://github.com/holos-run/holos)
- [ORAS — OCI Registry As Storage](https://oras.land/)
