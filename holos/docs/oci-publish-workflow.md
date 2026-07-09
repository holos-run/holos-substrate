# Client-Side Build-and-Publish Workflow (Kustomize + ORAS)

The command-line "build and publish" workflow that turns a new application
container image into a deployable, digest-pinned **OCI rendered-manifests
artifact**. A developer or CI runs one command —
[`scripts/publish`](../../scripts/publish) (or `make publish`) — that renders
the platform with the new app image, packages the rendered manifests with
**Kustomize**, and `oras push`es the result to the registry, printing the
pushed artifact digest.

This is the **client-side half of the Kargo pivot** (parent HOL-1236). It
**replaces the previously-planned in-cluster render-task subscriber**: instead
of a NATS-driven subscriber re-rendering and pushing inside the cluster, the
render + package + push step now runs from the CLI, and the next phase's Kargo
Warehouse watches the artifact this workflow produces. See the original
subscriber design — now superseded — in
[Research: rendered-manifests publish pipeline](../../docs/archive/rendered-manifests-publish-pipeline.md)
(§4.4 tag injection and §4.5 input-addressed idempotency are the directly
reusable parts, now run from the CLI rather than a subscriber).

The published artifact is consumed exactly as documented in
[argocd-application-source.md](argocd-application-source.md): an Argo CD
`Application` with an `oci://` `repoURL` and a digest `targetRevision`.

## What the command does

`scripts/publish <app-image-ref> [oci-repo]` performs five steps:

1. **Resolve the app image tag → digest** (`oras resolve`). A reference that
   already contains `@sha256:…` is used as-is. Resolving the tag first pins the
   input, so the rendered YAML is exact and the same invocation always renders
   the same image even if the tag is later moved.
2. **Render the platform** with the digest-pinned image injected:
   `holos render platform --inject app_image=<repo>@sha256:<digest>`. The
   platform CUE declares `_AppImage: string @tag(app_image)` in
   [`holos/tags.cue`](../tags.cue) and the echo component (the Layer 3 sample
   workload) sets its container image from it; `--inject` is forwarded to every
   component render.
3. **Package with Kustomize** — *not* a Helm chart wrapper (the explicit
   packaging decision from the parent issue). The script writes a
   `kustomization.yaml` at the root of the rendered tree listing every rendered
   manifest as a resource, then runs `kustomize build` to produce the final,
   normalized manifest set.
4. **Push the OCI artifact** — a single layer holding a gzipped tarball of the
   kustomize output, with the layer media type
   `application/vnd.oci.image.layer.v1.tar+gzip` that Argo CD's OCI source
   accepts.
5. **Print the pushed digest** (`oras push --format go-template='{{.digest}}'`)
   as the last line on stdout, so a caller can capture it:
   `DIGEST=$(scripts/publish …)`.

## Example invocation

```bash
# By tag (resolved to a digest before rendering):
scripts/publish quay.holos.internal/holos/echo:v1

# By digest, to an explicit target repo:
scripts/publish \
  quay.holos.internal/holos/echo@sha256:9afa…5ba \
  quay.holos.internal/holos/holos-substrate-manifests

# Via make (APP_IMAGE required; PUBLISH_REPO optional):
make publish APP_IMAGE=quay.holos.internal/holos/echo:v1

# Capture the pushed digest for downstream use:
DIGEST=$(scripts/publish quay.holos.internal/holos/echo:v1)
echo "$DIGEST"   # sha256:1a88…dad
```

On success the script prints progress and the consumption hint to **stderr** and
the bare artifact digest to **stdout**:

```
Published quay.holos.internal/holos/holos-substrate-manifests@sha256:1a88…dad (tag render-6727d8e9f33c-9afa9311ba1d)
Consume it as an Argo CD OCI source by digest:
  repoURL:        oci://quay.holos.internal/holos/holos-substrate-manifests
  targetRevision: sha256:1a88…dad
```

## Platform config bundle (`holos-substrate-config`)

Distinct from the per-app `scripts/publish` path above, the **platform config
bundle** packages the **whole rendered `holos/deploy/` tree as-is** into one OCI
artifact under a mutable `:dev` tag, for Argo CD to **bootstrap the entire
platform** from an App-of-Apps (HOL-1373/HOL-1374 — the producer side of the
bootstrap; later phases consume it). [`scripts/publish-config`](../../scripts/publish-config)
and the `make config-build` / `make config-push` targets implement it.

It is deliberately **not** `scripts/publish`:

| | `scripts/publish` (`make publish`) | `scripts/publish-config` (`make config-build`/`config-push`) |
| --- | --- | --- |
| Input | `holos render platform --inject app_image=…` (re-renders) | the **committed** `holos/deploy/` tree, **as-is** (no render) |
| Packaging | Kustomize `build` → one flat `manifests.yaml` | a straight `tar` of the deploy tree (no Kustomize) |
| Repo | `quay.holos.internal/holos/holos-substrate-manifests` | `quay.holos.internal/holos/holos-substrate-config` |
| Tag | immutable, input-addressed `render-<config12>-<appimage12>` | mutable `dev` |
| Consumer | Kargo `Warehouse` → per-app delivery | Argo CD App-of-Apps → platform bootstrap |
| Layer media type | `application/vnd.oci.image.layer.v1.tar+gzip` | same |
| Transport (`*.holos.internal` → `--insecure`, etc.) | same `run_oras_dest` machinery | same (reused) |

### Build / push split

The two steps are **separate** targets, mirroring `docker-build` / `docker-push`
— `config-build` produces a local artifact with **no network I/O**, `config-push`
depends on it and `oras push`es it:

```bash
# Build the local artifact tarball (holos/deploy.tar.gz, gitignored). No network.
make config-build

# Build (if stale) then push to quay.holos.internal/holos/holos-substrate-config:dev.
make config-push

# Override the target. CONFIG_REPO is the bare repo; CONFIG_TAG the tag.
make config-push CONFIG_REPO=localhost:5099/holos/holos-substrate-config CONFIG_TAG=dev

# The script directly (the Make targets are thin wrappers):
scripts/publish-config --build                       # tar holos/deploy/
scripts/publish-config --push                        # oras push the built tarball
DIGEST=$(scripts/publish-config --push)              # capture the pushed digest
```

`config-push` declares `config-build` as a prerequisite, so a single `make
config-push` builds then pushes; the build alone never reaches the network. Like
`scripts/publish`, `--push` prints progress and the consumption hint to
**stderr** and the bare pushed digest to **stdout** (the last line).

### Tarball layout (what Phase 3 references)

The artifact is a single `application/vnd.oci.image.layer.v1.tar+gzip` layer
holding `holos/deploy.tar.gz`. The tar is rooted at `holos/deploy/`, so the
member paths inside the tarball begin at **`clusters/`**:

```
clusters/k3d-holos/components/<component>/<resource>.yaml
```

An Argo CD child `Application` consuming this bundle therefore selects a
per-component subpath with:

```yaml
spec:
  source:
    repoURL: oci://quay.holos.internal/holos/holos-substrate-config
    targetRevision: dev   # the mutable bootstrap tag (or pin a digest)
    path: clusters/k3d-holos/components/<component>
```

The bundle contains the **full** `holos/deploy/` tree (every component, all 30+
under `clusters/k3d-holos/components/`), so the App-of-Apps can fan out one child
Application per component by `source.path`. The build packages the **committed**
tree at `HEAD` (`git archive HEAD:holos/deploy`), **not** the working tree — so
an uncommitted or unstaged local render can never leak into the mutable `:dev`
bootstrap tag (the platform Argo CD bootstraps from must be a reviewed, committed
render), a stray local file never appears, and the archive is **byte-reproducible**
— sorted member order, zeroed ownership, a pinned `--mtime` epoch (a tree-ish has
no commit timestamp, so without the pin `git archive` would stamp the current
time and mint a new digest on every build), and `gzip -n` to drop gzip's own
timestamp. Identical committed config therefore always produces an identical
tarball. `config-build` fails loudly if `holos/deploy/` is absent from `HEAD`
rather than pushing an empty bundle.

### The App-of-Apps that consumes the bundle (Phase 3, HOL-1376)

The `app-of-apps` component (`holos/components/app-of-apps/buildplan.cue`) is the
consumer that makes Argo CD reconcile the platform from this `:dev` bundle. It
renders into `clusters/k3d-holos/components/app-of-apps/`:

- **A root `Application` `platform-bootstrap`** (`application-bootstrap.yaml`) —
  `spec.project: platform` (the AppProject `argocd-projects` stood up in
  HOL-1375), source `oci://quay.holos.internal/holos/holos-substrate-config` at
  `targetRevision: dev`, and `source.path:
  clusters/k3d-holos/components/app-of-apps/children` with
  `directory.recurse: true`. It reconciles **only** the `children/` subdirectory,
  so it never manages itself (it lives one level up, outside that path).
- **One child `Application` per system component** under `children/` —
  `platform-<component>`, each `spec.project: platform`, source the same bundle
  at `:dev`, `source.path: clusters/k3d-holos/components/<component>`, destination
  the in-cluster API server. The **system set** is exactly the components
  registered in `holos/platform/platform.cue` **before** the `project`/`application`
  collection components, which is the ordered `COMPONENTS=(…)` array in
  `scripts/apply` (`keycloak-instance`/`project`/`application` are excluded — they
  carry an apply-time-injected `caBundle` and are applied by
  `scripts/apply-projects`, not the master apply). The component **enumerates**
  that list (each component renders in isolation, so the `platform.components`
  registry is not reachable from inside the buildplan); keep the list in lock-step
  with `scripts/apply` when adding/removing/renaming a system component.

**Apply ordering** is preserved via `argocd.argoproj.io/sync-wave` annotations:
each child's wave is its index in the system list (ascending `0…N`), mirroring
`scripts/apply` (CRDs/namespaces before controllers, Istio base before
istiod/cni/ztunnel, cert-manager before local-ca, operators before instances).
The root carries wave `-1` so it settles before the children fan out.

Sync waves only gate ordering if Argo CD waits for an earlier wave to become
**Healthy** before applying the next. Argo CD removed built-in health assessment
of the `Application` kind in 1.8, so the `argocd` controller component restores it
with a `resource.customizations.health.argoproj.io_Application` Lua check in
`argocd-cm` (`APP_HEALTH_LUA` in
`holos/components/argocd/controller/buildplan.cue`): a child Application reports
Healthy only once its own `status.health.status` is Healthy. Without that
customization the root App-of-Apps would treat every child as Healthy on creation
and apply all waves at once, making the annotations cosmetic and racing
crds-before-controllers ordering. The Lua is mandatory for the wave ordering to
hold.

**Scope of the ordering guarantee.** The sync waves serialize the **bootstrap**
rollout — the order in which the root first *creates* the child Applications and
their resources (CRDs/operators before the controllers/CRs that need them), which
is what this phase requires. They do **not** serialize steady-state `:dev`
*updates*: each child independently tracks `targetRevision: dev` with its own
`automated` sync (the "Always" policy), so when the tag moves the children
re-resolve and sync in parallel, not in wave order. That is the intrinsic
tradeoff of the per-child `:dev`-tracking design — a wave-serialized *update*
rollout would require the root to be the only object that changes per release
(digest-pinned children), contradicting the committed `targetRevision: dev` on
the children. Each child's `selfHeal` converges regardless of arrival order; a
serialized cross-component *update* rollout, if ever needed, is a separate design
(staged promotion or digest-pinned children) outside this bootstrap.

**Server-side apply parity:** every Application sets `syncOptions:
[ServerSideApply=true, CreateNamespace=false]` so Argo CD's reconciliation
matches how `scripts/apply` applies the same manifests
(`kubectl apply --server-side --force-conflicts`) and namespaces stay owned by the
`namespaces` child (wave 0), never created ad hoc.

**"Always" re-pull of the mutable `:dev` tag — the exact mechanism.** Argo CD
caches an OCI tag's *resolved manifest* in the repo-server's repo cache, so a
re-pushed `:dev` is not re-pulled until that entry expires. Argo CD 3.4.2 (chart
`9.5.15`) exposes **no** OCI-tag-specific expiration knob — the only OCI
`argocd-cmd-params-cm` keys the vendored chart wires are
`reposerver.oci.manifest.max.extracted.size` and
`reposerver.oci.layer.media.types` (size/format limits, **not** a TTL). The
applicable mechanism is therefore the repo-cache TTL
**`reposerver.repo.cache.expiration`** (`ARGOCD_REPO_CACHE_EXPIRATION`, default
`24h`), which the `argocd` controller component
(`holos/components/argocd/controller/buildplan.cue`) shortens to **`1m0s`**.
Within that minute the moved `:dev` is re-resolved; each child's
`syncPolicy.automated` (`prune` + `selfHeal`) then reconciles the new manifests
to the cluster — the "Always" image-tag update policy. **Tradeoff:** a shorter
TTL means more frequent re-resolution work on the repo-server; `1m` is
comfortable for this single-instance laptop cluster. A digest-pinned
`targetRevision` would make the cache moot, but the bootstrap deliberately tracks
the mutable `:dev` tag.

**Self-management note.** The `namespaces` and `argocd-*` children reconcile the
very Namespaces and Argo CD install Argo CD runs from. The server-side-apply
posture and `CreateNamespace=false` above are what keep this stable — the children
adopt the objects `scripts/apply` already applied rather than fighting them. No
system component is excluded from Argo CD management today; if self-management of
the argocd controller proves unstable on a given cluster, drop its child from the
`SYSTEM_COMPONENTS` list and apply it only via `scripts/apply` (record the
exclusion here and in the component comments).

### How the bootstrap runs the handoff (`scripts/apply-platform-app-of-apps`)

`scripts/apply` brings the foundation + Argo CD + Kargo up imperatively (the
bootstrap floor) and **stops there**, with Quay and Keycloak ready for manual
setup. The PLATFORM handoff — publishing `holos-substrate-config:dev` and applying the
**platform** root `Application` (`platform-bootstrap`) above — is a **separate
script, `scripts/apply-platform-app-of-apps`** (HOL-1382, split out of the former
`scripts/apply-app-of-apps`), because the publish needs the holos Quay
**organization** (the public `holos-substrate-config` repository and a push-capable Quay
robot credential) configured first; on a freshly rebuilt cluster that organization
does not exist yet, so publishing from `scripts/apply` raced the manual Quay setup
and failed (HOL-1379, [ADR-16 Rev 4](../../docs/adr/archive/ADR-16.md)). The repository is
**public** (HOL-1381), so Argo CD pulls the bundle **anonymously** — no pull
credential, and the `argocd-projects` component registers it with Argo CD via a
credential-less repository Secret (carrying only `url`/`type`/`insecure`) committed
to the deploy tree. After the operator configures the Quay org,
`scripts/apply-platform-app-of-apps` runs `scripts/publish-config --build`/`--push`
then `kubectl apply --server-side` of the `app-of-apps` root component dir
(idempotent; server-side apply converges). This is the **clean cut line**: the
platform is fully bootstrapped here. Because its whole purpose **is** the handoff, a
missing prerequisite — `oras`/Quay/the push credential absent, or the Quay org not
yet configured — is a **hard error** with guidance, not a graceful skip; applying
the root is gated on a successful publish (`PLATFORM_APP_OF_APPS_FORCE_ROOT=1`
forces the root against a known-current `:dev`).

### Per-project config bundles and the project handoff (`scripts/apply-projects-app-of-apps`)

Tenant projects are bootstrapped **separately and independently** from the platform
(HOL-1382): there is no longer one global `projects-bootstrap` root over the shared
`holos-substrate-config` bundle. Instead, `scripts/apply-projects-app-of-apps` enumerates
the registered projects (`holos cue export ./holos/projects | jq -r '.projects |
keys[]'`) and, for **each** project, calls `scripts/apply-project-app-of-apps
<project>`, which:

1. builds that project's **own** OCI config bundle — a tar of the COMMITTED render
   of the project's control-plane subtree
   (`clusters/<cluster>/components/project/<project>/control-plane`) plus each of
   its apps' bundles (`clusters/<cluster>/components/application/<app>`, both
   `control-plane/` and `workload/`) — and pushes it to the project's own **public**
   repo `quay.holos.internal/holos/<project>-config:dev` (pre-created by
   `holos-quay-organization`, registered credential-less by `argocd-projects`); then
2. applies that project's **`<project>-control-plane`** root `Application` — Argo CD
   reconciles the bundle EXCLUDING the app workload (`directory.exclude:
   **/workload/**`), delivering only the platform-managed control plane.

The project's **service owner** then runs `scripts/apply-project-workload-app-of-apps
<project>` to apply the **`<project>-workload`** root (`directory.include:
**/workload/**`, the app `Deployment`/`Service`/`HTTPRoute`/…), reusing the bundle
the control-plane step already pushed — **after** the platform team applied the
control plane. The two top-level scripts (`apply-platform-app-of-apps`,
`apply-projects-app-of-apps`) are **completely independent and never call each
other**. The per-project bundle build/push reuses the same per-host oras transport
rules as `scripts/publish-config` (below); `PROJECT_APP_OF_APPS_FORCE_ROOT=1` forces
the per-project root against a known-current bundle.

### Registry transport and credentials

`scripts/publish-config` reuses `scripts/publish`'s `run_oras_dest` /
`transport_flags` machinery, so the **same** per-host transport rules apply: a
`*.holos.internal` host (the in-cluster Quay's mkcert-signed cert) auto-enables
`--insecure`; a `localhost`/`127.0.0.1` host auto-enables `--plain-http`. Force
either with `ORAS_INSECURE=1` / `ORAS_PLAIN_HTTP=1`. Destination push
credentials are `ORAS_USERNAME` / `ORAS_PASSWORD` (passed via
`--password-stdin`), or omit them to use oras's ambient auth — identical to the
[Registry credentials](#registry-credentials) section below.

### Verification (manual)

```bash
# 1. Build the bundle with no network access and confirm the layout.
make config-build
tar tzf holos/deploy.tar.gz | head    # clusters/k3d-holos/components/...

# 2. Against a throwaway local registry, push and round-trip it.
docker run -d --name reg -p 5099:5000 registry:2
DIGEST=$(make -s config-push CONFIG_REPO=localhost:5099/holos/holos-substrate-config)
oras manifest fetch --plain-http \
  localhost:5099/holos/holos-substrate-config:dev | jq '.layers[].mediaType'
#   "application/vnd.oci.image.layer.v1.tar+gzip"
docker rm -f reg
```

Do **not** attempt a live push to a cluster registry in CI — `config-build` is
the network-free step CI exercises.

## Deterministic, input-addressed tagging (idempotency)

The artifact is tagged input-addressed:

```
render-<config-digest-12>-<appimage-digest-12>
```

- **config-digest** is a content digest of the `holos/` CUE source tree — the
  inputs `holos render` actually reads. It hashes the **working-tree bytes** of
  every input file (so unstaged edits change the tag), enumerated from git as
  the union of tracked files and untracked-but-not-gitignored files under
  `holos/`, excluding the rendered `holos/deploy/` output. It is independent of
  file mtimes and stable across checkouts.
- **appimage-digest** is the resolved app image digest.

Together these two inputs fully determine the rendered output, so **re-publishing
the same CUE source + app image digest produces the same tag**. The script makes
this concretely idempotent with a **tag-exists fast path**: before pushing it
resolves the computed tag in the registry, and if it already exists it re-emits
the existing digest without re-pushing (set `FORCE_PUSH=1` to overwrite). The
next-phase Kargo deployment can therefore treat the tag as a stable,
deduplicated handle for an unchanged input (research §4.5).

Note the **digest is the source of truth downstream**: do not rely on
byte-identical artifact digests across re-pushes (tar/gzip timestamps mean the
same content can yield a different artifact digest), which is exactly why the
fast path reuses the existing artifact rather than re-pushing. `targetRevision`
carries the immutable *digest* the push reports (consistent with
[ADR-8](../../docs/adr/archive/ADR-8.md)'s digest-pinning preference). Override the
computed tag with `ARTIFACT_TAG=…` only when you deliberately want to break this
guarantee.

## Registry credentials

The workflow touches **two** registries with **separate** credentials, by
design — the destination push credential is never sent to the source registry
(which may be a different, untrusted registry):

- **Destination** (the manifests artifact repo) needs **push** scope. For the
  in-cluster Quay this is a push-capable Quay robot credential (the robot and this
  push Secret are not modeled by the `quay.holos.run` CRDs, ADR-19 *Out of scope*,
  so they stay manually provisioned); see the
  [repository credential Secret shape](argocd-application-source.md#repository-credential-secret)
  in argocd-application-source.md (Argo CD's repo-server uses a *pull*-scoped
  credential to fetch the same artifact).
- **Source** (the app image repo) needs **pull** scope, used only to resolve the
  app image tag → digest.

Provide credentials to `scripts/publish` either way:

- **Ambient auth** — run `oras login <registry>` (or rely on
  `~/.docker/config.json`) once for each registry, then invoke the script with
  no credential variables. This is the simplest path and works for both
  registries at once.
- **Explicit** — set credentials per registry; the script passes them via
  `--password-stdin` so the secret never appears in the process list:
  - destination push: `ORAS_USERNAME` / `ORAS_PASSWORD`
  - source pull (only needed for a **private** app image repo):
    `ORAS_SRC_USERNAME` / `ORAS_SRC_PASSWORD`

  The destination variables are used **only** against the destination repo, so a
  private source image on a different registry needs the `ORAS_SRC_*` pair (or
  ambient auth for that registry) — the destination push token is never offered
  to it.

### Transport for local registries

| Registry | Transport | Default in `scripts/publish` |
| --- | --- | --- |
| `quay.holos.internal` (in-cluster Quay) | HTTPS with a mkcert-signed cert not in the default trust store | `--insecure` (skip TLS verify) auto-enabled for `*.holos.internal` |
| `localhost:<port>` (bare dev registry) | plain HTTP | `--plain-http` auto-enabled for `localhost`/`127.0.0.1` |
| any other host | HTTPS with a trusted cert | neither flag |

Force either explicitly with `ORAS_INSECURE=1` or `ORAS_PLAIN_HTTP=1` (plain
HTTP takes precedence). `--insecure` mirrors the `insecure: "true"` field on the
Argo CD repository Secret, required for the same mkcert reason
([argocd-application-source.md](argocd-application-source.md#repository-credential-secret)).

## Consuming the artifact in Argo CD

Reference the printed digest in an `Application` source (see
[argocd-application-source.md](argocd-application-source.md) for the full
contract, including the repository credential Secret and in-cluster
reachability):

```yaml
spec:
  source:
    repoURL: oci://quay.holos.internal/holos/holos-substrate-manifests
    targetRevision: sha256:1a88…dad   # the digest scripts/publish printed
    path: .                           # manifests sit at the tarball root
```

## Prerequisites

The script checks for these on PATH and fails fast if any is missing:

- [`holos`](https://holos.run/docs/cli/) — renders the platform
- [`kustomize`](https://kustomize.io/) — assembles the final manifests
- [`oras`](https://oras.land/) — resolves the digest and pushes the artifact
- `git`, `tar`, `sha256sum`

## Verification (manual)

The repo has no harness for shell scripts; verify against a local OCI registry:

```bash
# 1. A throwaway registry and a sample "app image".
docker run -d --name reg -p 5099:5000 registry:2
printf hello > file.txt
oras push --plain-http localhost:5099/holos/echo:v1 \
  file.txt:application/vnd.oci.image.layer.v1.tar+gzip

# 2. Publish the rendered manifests for that image.
DIGEST=$(scripts/publish localhost:5099/holos/echo:v1 \
  localhost:5099/holos/holos-substrate-manifests)

# 3. The artifact exists by digest, with the expected single tar+gzip layer.
oras manifest fetch --plain-http \
  localhost:5099/holos/holos-substrate-manifests@"$DIGEST" \
  | jq '.layers[].mediaType'   # "application/vnd.oci.image.layer.v1.tar+gzip"

# 4. The published manifests carry the injected app image digest.
oras pull --plain-http -o out \
  localhost:5099/holos/holos-substrate-manifests@"$DIGEST"
tar xzf out/manifests.tar.gz -O | grep 'image:.*echo@sha256'

# 5. Re-running with the same inputs is idempotent: the same input-addressed
#    tag is produced.  Capture the tag from each run's stderr and compare.
scripts/publish localhost:5099/holos/echo:v1 \
  localhost:5099/holos/holos-substrate-manifests 2>&1 >/dev/null \
  | grep -oE 'render-[0-9a-f]{12}-[0-9a-f]{12}'   # tag run A
scripts/publish localhost:5099/holos/echo:v1 \
  localhost:5099/holos/holos-substrate-manifests 2>&1 >/dev/null \
  | grep -oE 'render-[0-9a-f]{12}-[0-9a-f]{12}'   # tag run B == tag run A

# 6. Clean up.
docker rm -f reg
```

## Downstream: the Kargo delivery pipeline (echo spike)

The artifact this workflow publishes is consumed by Kargo, which drives the
rollout that the in-cluster NATS deployer subscriber used to drive
([ADR-16](../../docs/adr/archive/ADR-16.md)). For the spike this is wired end-to-end for
**one** representative application — the **echo** sample workload
([`components/echo/`](../components/echo/buildplan.cue)) — across two components:

- [`components/kargo-project-echo/`](../components/kargo-project-echo/buildplan.cue)
  — a Kargo `Project` (mirroring the reference platform's
  `kargo-project-braintrust`) that reconciles to a **dedicated** `kargo-echo`
  namespace and carries an auto-promotion policy for the `test` Stage. A Project
  adopts a same-named namespace and adds its own `kargo.akuity.io/finalizer`, so
  it is deliberately given its own namespace rather than the echo workload
  namespace, which the namespaces component server-side-applies — sharing it
  would risk finalizer/label contention. The namespace is registered centrally
  ([`holos/namespaces.cue`](../namespaces.cue)) with the
  `kargo.akuity.io/project: "true"` adoption label so Kargo adopts it, and the
  `kargo.akuity.io/keep-namespace: "true"` annotation so Kargo never deletes it.
- [`components/kargo-echo/`](../components/kargo-echo/buildplan.cue) — the
  pipeline itself:
  - a **`Warehouse`** (`freightCreationPolicy: Automatic`, `interval: 1m`) with
    an `image` subscription to the bare rendered-manifests repo
    `quay.holos.internal/holos/holos-substrate-manifests`, scoped by
    `allowTags: ^render-[0-9a-f]{12}-[0-9a-f]{12}$` to the input-addressed tags
    this workflow mints. It uses `imageSelectionStrategy: Lexical`, not SemVer
    (the tags are not semver) and not NewestBuild (ORAS rendered-manifests
    artifacts carry no build-timestamp metadata for NewestBuild to order by),
    and `insecureSkipTLSVerify: true` for Quay's mkcert certificate.
  - a **`Stage`** (`test`) that requests Freight directly from the Warehouse and,
    on promotion, runs a single **`argocd-update`** step — **not**
    `helm-template`. No in-promotion `kustomize-build` is needed: this workflow
    already ran `kustomize build` client-side and pushed finished manifests, so
    the Stage only repoints the Application's OCI `targetRevision` to the
    Freight's **digest** (`${{ imageFrom("…/holos-substrate-manifests").Digest }}`).
  - the target Argo CD **`Application`** (`echo`, in `argocd`) with an **OCI**
    source (`oci://…/holos-substrate-manifests`) and the
    `kargo.akuity.io/authorized-stage: kargo-echo:test` annotation authorizing
    the Stage to patch it. It is authored standalone rather than through the
    `userDefinedBuildPlan` gitops projection (the `argoAppDisabled` flip in
    [`components/user-defined-build-plan.cue`](../components/user-defined-build-plan.cue)):
    that projection emits a **git**-source Application for the deferred
    whole-platform gitops delivery ([placeholders.md](placeholders.md)), which is
    the wrong shape for Kargo to patch. Its `targetRevision` is **deliberately
    omitted** from the committed manifest: Kargo's `argocd-update` step owns that
    field, and `scripts/apply` re-applies every component with `kubectl apply
    --server-side --force-conflicts`, so committing a value would seize it back on
    every run and fight Kargo. Leaving it out means apply never asserts ownership
    — the Application is Unknown until the first promotion, then Kargo is the sole
    owner of the revision, the "imperative revision, declarative Application"
    posture [argocd-application-source.md](argocd-application-source.md) documents.

### Lexical ordering caveat (spike)

The `render-<config12>-<appimage12>` tag is **input-addressed, not monotonic**,
so the lexically-greatest tag is not necessarily the most recently published.
For the single-app spike this is acceptable: `Automatic` Freight creation
produces Freight for any newly discovered tag, and the verification below
publishes one new artifact at a time. A production pipeline that needs strict
most-recent-wins ordering should switch to a monotonic tag (a zero-padded
counter or timestamp prefix) or a `Digest` strategy against a mutable tag.

### End-to-end manual verification (on the local cluster)

Prerequisites: the cluster is up and `scripts/apply` has run (Kargo, Argo CD,
Quay, and the two `kargo-*-echo` components are applied). Two **imperative,
uncommitted** credential Secrets are required (the repo's runtime-secret posture
— neither is rendered into the deploy tree):

1. **Argo CD's repo-server** PULLs the artifact using a repository credential
   Secret in the `argocd` namespace (the `holos+robot` pull credential; the robot
   and this pull Secret are not modeled by the `quay.holos.run` CRDs, ADR-19 *Out
   of scope*, so they stay manually provisioned) — see the
   [repository credential Secret](argocd-application-source.md#repository-credential-secret)
   shape.
2. **Kargo's controller** LISTs tags for the Warehouse using a separate
   **Kargo-format image credential Secret** in the `kargo-echo` Project namespace.
   Kargo discovers credentials from Secrets labeled
   `kargo.akuity.io/cred-type: image` whose `repoURL` matches (or prefixes) the
   subscription `repoURL`; without it, Warehouse discovery cannot authenticate to
   the private Quay repo and no Freight is created. Create it from the same Quay
   robot pull credential:

   ```bash
   kubectl --context k3d-holos -n kargo-echo create secret generic quay-manifests-creds \
     --from-literal=repoURL=quay.holos.internal/holos/holos-substrate-manifests \
     --from-literal=username=holos+robot \
     --from-literal=password='<robot token>'
   kubectl --context k3d-holos -n kargo-echo label secret quay-manifests-creds \
     kargo.akuity.io/cred-type=image
   ```

```bash
KCTX=k3d-holos

# 0. The pipeline objects exist.
kubectl --context "$KCTX" -n kargo-echo get warehouse,stage
kubectl --context "$KCTX" -n argocd get application echo \
  -o jsonpath='{.spec.source.targetRevision}{"\n"}'   # empty until first promotion

# 1. Publish a new rendered-manifests artifact for echo (HOL-1239).  Use the
#    in-cluster Quay app image repo; the default PUBLISH_REPO is the manifests
#    repo the Warehouse watches.
DIGEST=$(scripts/publish quay.holos.internal/holos/echo:v1)
echo "published $DIGEST"

# 2. The Warehouse discovers the artifact and creates Freight (within ~interval).
kubectl --context "$KCTX" -n kargo-echo get freight -w
#    A Freight object appears whose image digest matches $DIGEST.

# 3. Auto-promotion runs the Stage's argocd-update step; a Promotion succeeds.
kubectl --context "$KCTX" -n kargo-echo get promotion
#    PHASE column reaches Succeeded.

# 4. The Argo CD Application's OCI targetRevision is now the new digest, and it
#    syncs the rendered manifests.
kubectl --context "$KCTX" -n argocd get application echo \
  -o jsonpath='{.spec.source.targetRevision}{"  "}{.status.sync.status}{"\n"}'
#    targetRevision == $DIGEST, sync status Synced.
```

This is the loop the issue's verification AC describes: publish → Warehouse
creates Freight → Stage promotion sets the Argo CD Application `targetRevision`
→ Argo CD syncs the new version.

## Downstream: the `my-project` delivery scaffold

The `echo` spike above wires the pipeline for the platform's permanent
smoke-test workload. `my-project`
([holos/README.md → The `my-project` delivery scaffold](../README.md#the-my-project-delivery-scaffold))
— as of HOL-1357 a one-line project registration ([`projects/my-project.cue`](../projects/my-project.cue))
rendered by the collection-driven [`components/project/`](../components/project/buildplan.cue)
component (the bespoke `components/my-project` was deleted) — is the
**project-shaped** instance of the same pattern, the reference for a future
self-service `ProjectRequest`, and differs from the echo spike in two ways that
simplify the operator workflow:

- **One component, one namespace.** The Kargo `Project`, `ProjectConfig`,
  `Warehouse`, and `Stage` all live in the single `my-project` component, and
  the Kargo Project namespace *is* the workload namespace (no separate
  `kargo-project-*` sibling).
- **The Quay org is reconciled from an emitted Organization CR; repo/robot/webhook
  stay manual.** As of HOL-1322 the `my-project` component emits a
  `quay.holos.run/v1alpha1` Organization (with a per-cluster local-ca `caBundle`)
  that the shipped Holos Controller ([ADR-18](../../docs/adr/ADR-18.md)/[ADR-19](../../docs/adr/ADR-19.md))
  reconciles into the in-cluster Quay org. The `my-project/my-project-config`
  repository (a Repository CR is reconciled by the controller but **not** emitted
  by this component yet), the pull robot, the Argo CD repository Secret in
  `argocd`, and the `repo_push` webhook registration are **not** emitted — they
  were previously provisioned by an in-component `my-project-quay-bootstrap` Job
  (HOL-1272) that authenticated with the removed `quay-initial-admin` admin token;
  that Job no longer exists. Only the Kargo-side `my-project-quay-webhook-bootstrap`
  Job (the receiver token) still runs — it needs no Quay admin token. Those
  remaining Quay objects and the Argo CD repository Secret are provisioned by hand,
  so a push triggers Freight discovery only after that manual setup.
- **Applied by `scripts/apply-projects`, not `scripts/apply`.** HOL-1322 removed
  `my-project` from the master apply; the dedicated `scripts/apply-projects`
  injects the local-ca PEM as the Organization's `caBundle` at apply time and
  applies the component (Namespace, Organization, Argo CD/Kargo objects, and the
  webhook-token Job).

### Verify the scaffold, and the end-to-end contract it will satisfy

Prerequisites: the cluster is up, `scripts/apply` has run, and
`scripts/apply-projects` has been run (so the
`my-project-quay-webhook-bootstrap` receiver-token Job completed — that script
gates it, and gates the Organization reaching Ready). Two categories are **not**
created by either script and must be provisioned by hand:

- the `my-project/my-project-config` **repository** and its `repo_push` webhook —
  reconcilable by a `quay.holos.run` Repository CR, but that CR is **not** emitted
  by the component yet (the proposed Holos Project/Application components,
  [ADR-21](../../docs/adr/archive/ADR-21.md), would emit it);
- the **push robot**, the **Argo CD repository (pull) Secret** in `argocd`, and
  the **Kargo image-credential Secret** the Warehouse authenticates registry tag
  discovery with — these are **not** modeled by the `quay.holos.run` CRDs at all
  (ADR-19 *Out of scope*) and stay manual even after ADR-21. Without the Kargo
  image credential, Warehouse discovery cannot authenticate and no Freight is
  created.

> **The publish step is future work — do not run it against the platform
> render.** `scripts/publish` today packages the **whole-platform** render
> (`holos render platform` → every rendered resource, including cluster-scoped
> CRDs/ClusterRoles/Namespaces and other namespaces' objects). The `my-project`
> AppProject deliberately omits `clusterResourceWhitelist` and scopes
> destinations to the `my-project` namespace, so a whole-platform artifact
> **cannot** sync into this Application. The artifact this scaffold consumes is
> a **project-scoped** `my-project-config` bundle (manifests that fit inside the
> `my-project` namespace), which is the sample app's own future work and does
> not exist yet. So the publish command below is shown only to name the eventual
> entry point, **not** as a step to run today; steps 1–3 (publish → webhook →
> Freight → promotion) describe the contract the scaffold satisfies once that
> project-scoped artifact exists, and the step-4 `Synced` result is reachable
> only for such an artifact.

> **The per-app `<app>-config` artifact is the Application component's
> `workload/` bundle (HOL-1356) — the INTENDED publish contract, not yet wired
> into `scripts/publish`.** The Application component
> ([`holos/components/application/buildplan.cue`](../components/application/buildplan.cue))
> renders each `apps.<name>` entry into **two** separate artifact directories
> precisely so the bundles are independently packageable:
> `components/application/<app>/workload/` (the
> `Deployment`/`Service`/`HTTPRoute`/`ConfigMap`/`ServiceAccount`/`RoleBinding`)
> and `components/application/<app>/control-plane/` (the Quay `Repository`, the
> app `KeycloakClient`, the Kargo `Warehouse`/`Stage`, and the Argo CD
> `Application`). The control-plane objects are applied by the operator path
> (`scripts/apply-projects`, which applies **only** the `control-plane/` subtree).
> The per-app delivery contract is that the publish step packages **only the
> `workload/` subtree** as the `<app>-config` artifact the app's Argo CD
> `Application` syncs — so Argo CD never tries to manage the
> `Repository`/`KeycloakClient`/Kargo objects or the `Application` that points at
> itself, and the artifact is the project-scoped, namespace-fit bundle the
> whole-platform render is not. **`scripts/publish` does NOT yet implement this
> per-app packaging** — it still renders and packages the whole platform tree (the
> "publish step is future work" caveat above). Teaching `scripts/publish` to
> package a single app's `workload/` subtree as `<app>-config` is the remaining
> publish-workflow integration; the component split lands the rendered bundles
> that step will consume.

What you **can** verify today is that the scaffold is in place:

```bash
KCTX=k3d-holos

# The Kargo pipeline objects, the Argo CD Application (targetRevision empty
# until the first promotion), and the repo_push webhook receiver URL the
# bootstrap Job registered (filled in asynchronously by Kargo).
kubectl --context "$KCTX" -n my-project get projectconfig,warehouse,stage
kubectl --context "$KCTX" -n argocd get application my-project \
  -o jsonpath='{.spec.source.targetRevision}{"\n"}'   # empty until first promotion
kubectl --context "$KCTX" -n my-project get projectconfig my-project \
  -o jsonpath='{.status.webhookReceivers[?(@.name=="quay")].url}{"\n"}'
```

The full delivery loop the scaffold will drive, once a project-scoped
`my-project-config` artifact is published to the repo the Warehouse watches
(`quay.holos.internal/my-project/my-project-config` — the second positional
argument to `scripts/publish`, the publish target):

```bash
# 1. (future) Publish a PROJECT-SCOPED my-project-config artifact — see the
#    callout above; scripts/publish does not produce one today.
#    DIGEST=$(scripts/publish <app-image-ref> \
#      quay.holos.internal/my-project/my-project-config)

# 2. The repo_push webhook POSTs to the Kargo receiver; the Warehouse discovers
#    the artifact and creates Freight (immediately via the webhook, or within
#    the poll interval as a fallback).  A Freight object appears whose image
#    digest matches the published digest.
kubectl --context "$KCTX" -n my-project get freight -w

# 3. Auto-promotion runs the project-config Stage's argocd-update step; the
#    Promotion PHASE column reaches Succeeded.
kubectl --context "$KCTX" -n my-project get promotion

# 4. The Argo CD Application's OCI targetRevision is now the new digest and syncs
#    (Synced only for a project-scoped artifact — see the callout above).
kubectl --context "$KCTX" -n argocd get application my-project \
  -o jsonpath='{.spec.source.targetRevision}{"  "}{.status.sync.status}{"\n"}'
```

The path is identical in shape to the echo loop — publish → webhook →
Warehouse Freight → Stage promotion → Application sync — but driven by the real
`repo_push` webhook. It depends on the hand-provisioned Quay data plane: the
`my-project/my-project-config` repository, its `repo_push` webhook registration,
the push robot, and the Argo CD/Kargo pull-credential Secrets are **not** emitted
by the component (the Organization is, but the Repository CR, robots, and pull
Secrets remain manual — see the prerequisites above), so this loop only runs
after that manual setup. The remaining gap is the **content** of the published
artifact: discovery and promotion (steps 2–3) work for any artifact, but a clean
Application **sync** (step 4) needs a project-scoped `my-project-config` artifact,
which is the sample app's future work.
