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
subscriber design — now superseded for the MVP — in
[Research: rendered-manifests publish pipeline](../../docs/research/rendered-manifests-publish-pipeline.md)
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
scripts/publish quay.holos.localhost/holos/echo:v1

# By digest, to an explicit target repo:
scripts/publish \
  quay.holos.localhost/holos/echo@sha256:9afa…5ba \
  quay.holos.localhost/holos/holos-paas-manifests

# Via make (APP_IMAGE required; PUBLISH_REPO optional):
make publish APP_IMAGE=quay.holos.localhost/holos/echo:v1

# Capture the pushed digest for downstream use:
DIGEST=$(scripts/publish quay.holos.localhost/holos/echo:v1)
echo "$DIGEST"   # sha256:1a88…dad
```

On success the script prints progress and the consumption hint to **stderr** and
the bare artifact digest to **stdout**:

```
Published quay.holos.localhost/holos/holos-paas-manifests@sha256:1a88…dad (tag render-6727d8e9f33c-9afa9311ba1d)
Consume it as an Argo CD OCI source by digest:
  repoURL:        oci://quay.holos.localhost/holos/holos-paas-manifests
  targetRevision: sha256:1a88…dad
```

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
[ADR-8](../../docs/adr/ADR-8.md)'s digest-pinning preference). Override the
computed tag with `ARTIFACT_TAG=…` only when you deliberately want to break this
guarantee.

## Registry credentials

The workflow touches **two** registries with **separate** credentials, by
design — the destination push credential is never sent to the source registry
(which may be a different, untrusted registry):

- **Destination** (the manifests artifact repo) needs **push** scope. For the
  in-cluster Quay this is the same robot account `scripts/quay-init` provisions;
  see the
  [repository credential Secret shape and Quay bootstrap](argocd-application-source.md#repository-credential-secret)
  in argocd-application-source.md (the cluster-side credential Argo CD's
  repo-server uses to *pull* the same artifact).
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
| `quay.holos.localhost` (in-cluster Quay) | HTTPS with a mkcert-signed cert not in the default trust store | `--insecure` (skip TLS verify) auto-enabled for `*.holos.localhost` |
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
    repoURL: oci://quay.holos.localhost/holos/holos-paas-manifests
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
  localhost:5099/holos/holos-paas-manifests)

# 3. The artifact exists by digest, with the expected single tar+gzip layer.
oras manifest fetch --plain-http \
  localhost:5099/holos/holos-paas-manifests@"$DIGEST" \
  | jq '.layers[].mediaType'   # "application/vnd.oci.image.layer.v1.tar+gzip"

# 4. The published manifests carry the injected app image digest.
oras pull --plain-http -o out \
  localhost:5099/holos/holos-paas-manifests@"$DIGEST"
tar xzf out/manifests.tar.gz -O | grep 'image:.*echo@sha256'

# 5. Re-running with the same inputs is idempotent: the same input-addressed
#    tag is produced.  Capture the tag from each run's stderr and compare.
scripts/publish localhost:5099/holos/echo:v1 \
  localhost:5099/holos/holos-paas-manifests 2>&1 >/dev/null \
  | grep -oE 'render-[0-9a-f]{12}-[0-9a-f]{12}'   # tag run A
scripts/publish localhost:5099/holos/echo:v1 \
  localhost:5099/holos/holos-paas-manifests 2>&1 >/dev/null \
  | grep -oE 'render-[0-9a-f]{12}-[0-9a-f]{12}'   # tag run B == tag run A

# 6. Clean up.
docker rm -f reg
```

## Downstream: the Kargo delivery pipeline (echo spike)

The artifact this workflow publishes is consumed by Kargo, which drives the
rollout that the in-cluster NATS deployer subscriber used to drive
([ADR-16](../../docs/adr/ADR-16.md)). For the spike this is wired end-to-end for
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
    `quay.holos.localhost/holos/holos-paas-manifests`, scoped by
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
    Freight's **digest** (`${{ imageFrom("…/holos-paas-manifests").Digest }}`).
  - the target Argo CD **`Application`** (`echo`, in `argocd`) with an **OCI**
    source (`oci://…/holos-paas-manifests`) and the
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
   Secret in the `argocd` namespace (the `holos+robot` pull credential
   `scripts/quay-init` provisions) — see the
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
     --from-literal=repoURL=quay.holos.localhost/holos/holos-paas-manifests \
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
DIGEST=$(scripts/publish quay.holos.localhost/holos/echo:v1)
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
smoke-test workload. The
[`my-project`](../components/my-project/buildplan.cue) component
([holos/README.md → The `my-project` delivery scaffold](../README.md#the-my-project-delivery-scaffold))
is the **project-shaped** instance of the same pattern — the reference for a
future self-service `ProjectRequest` — and differs from the echo spike in two
ways that simplify the operator workflow:

- **One component, one namespace.** The Kargo `Project`, `ProjectConfig`,
  `Warehouse`, and `Stage` all live in the single `my-project` component, and
  the Kargo Project namespace *is* the workload namespace (no separate
  `kargo-project-*` sibling).
- **Credentials and the webhook are bootstrapped automatically.** The
  `my-project-quay-bootstrap` Job (HOL-1272) provisions the Quay org, the
  `my-project/my-project-config` repository, the pull robot, the Argo CD
  repository Secret in `argocd`, **and** a `repo_push` webhook on the repo. So
  unlike the echo verification — which has you create the Argo CD repo Secret
  and the Kargo `image`-credential Secret by hand — `my-project` needs no manual
  credential Secrets, and a push triggers Freight discovery via the webhook
  immediately rather than waiting on the Warehouse poll interval.

### Verify the scaffold, and the end-to-end contract it will satisfy

Prerequisites: the cluster is up and `scripts/apply` has run (so both
`my-project` bootstrap Jobs completed — `wait_my_project` gates them — leaving
the Quay org/repo/webhook and the Argo CD repository Secret in place).

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
(`quay.holos.localhost/my-project/my-project-config` — the second positional
argument to `scripts/publish`, the publish target):

```bash
# 1. (future) Publish a PROJECT-SCOPED my-project-config artifact — see the
#    callout above; scripts/publish does not produce one today.
#    DIGEST=$(scripts/publish <app-image-ref> \
#      quay.holos.localhost/my-project/my-project-config)

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
`repo_push` webhook and requiring no hand-created credential Secrets, because
the `my-project` bootstrap Jobs provisioned them. The remaining gap is the
**content** of the published artifact: discovery and promotion (steps 2–3) work
for any artifact, but a clean Application **sync** (step 4) needs a
project-scoped `my-project-config` artifact, which is the sample app's future
work.
