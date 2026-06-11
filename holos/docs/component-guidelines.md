# Component Guidelines

How to add a Holos component to this repository, and the guardrails every
component must satisfy before it merges. Written for component authors and
reviewers. For orientation — what lives where under `holos/` and how rendered
manifests reach a cluster — start with [`holos/README.md`](../README.md).

These are operational guidelines, not decisions. Decisions live in ADRs (see
[ADR-12](../../docs/adr/ADR-12.md) for the repository layout and
[ADR-2](../../docs/adr/ADR-2.md) for the platform principles); revise the
relevant ADR rather than this document when a decision changes.

The worked example throughout is the real
[`gateway-api`](../components/gateway-api/) component — snippets below are
copied from its files, not paraphrased.

## Component directory anatomy

Each component is a directory under `holos/components/<name>/`:

```text
holos/components/gateway-api/
├── buildplan.cue        # the component definition: version pin, generators, transformers
├── typemeta.cue         # boilerplate: BuildContext tag + typemeta.yaml embed
├── typemeta.yaml        # kind: BuildPlan, apiVersion: v1alpha6
├── read-thru-cache      # optional: executable fetch-and-cache script
└── manifests/           # optional: committed cache of fetched upstream manifests
    └── bundle.1.4.1.yaml
```

- **`buildplan.cue`** defines the component through the `userDefinedBuildPlan`
  adapter (see
  [`components/user-defined-build-plan.cue`](../components/user-defined-build-plan.cue)).
  Every component under `components/` MUST integrate through
  `userDefinedBuildPlan` — the adapter unconditionally defines `holos:` as a
  BuildPlan, so the author-style wrappers in
  [`holos/schema.cue`](../schema.cue) (`#Kubernetes`, `#Kustomize`, `#Helm`)
  conflict with it and are usable only under a non-default
  `#ComponentTemplate` `inputs.prefix`.
- **`typemeta.cue` and `typemeta.yaml`** are per-component boilerplate: copy
  them verbatim from an existing component. `typemeta.cue` decodes the
  `holos_build_context` tag into `BuildContext` and embeds `typemeta.yaml`
  (`kind: BuildPlan`, `apiVersion: v1alpha6`).
- **`read-thru-cache` + `manifests/`** apply to components that fetch upstream
  manifests. The script downloads once and caches the result as
  `manifests/bundle.<VERSION>.yaml`; the cache is **committed** so rendering
  is reproducible offline and any change in fetched content is visible in
  review. Keep the script concurrency-safe (unique temp files, atomic `mv`)
  and executable (`chmod +x`). See
  [`components/gateway-api/read-thru-cache`](../components/gateway-api/read-thru-cache)
  for the canonical shape.

## One file per resource (kubectl-slice guardrail)

Components that emit multiple Kubernetes resources MUST slice their output
into one file per resource with a `holos kubectl-slice` `Command` transformer.
Never write multiple resources to a single artifact file: generators in
general (Helm, Resources, upstream bundles) do not guarantee a stable
resource order, so bundled files produce noisy diffs and false drift on
re-render, and per-resource files diff cleanly and let apply tools prune.

The artifact is a *directory* (the component's deploy path), and the final
transformer slices the bundle into it. From
[`components/gateway-api/buildplan.cue`](../components/gateway-api/buildplan.cue):

```cue
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [{
				kind: "Command"
				command: {
					// read-thru-cache fetches the standard channel CRDs once and
					// caches them in manifests/bundle.<VERSION>.yaml for offline
					// reproducible rendering.  The path derives from BuildContext
					// so it tracks the component directory regardless of the
					// command working directory or a metadata.name override.
					args: ["\(BuildContext.rootDir)/\(BuildContext.leafDir)/read-thru-cache", VERSION]
					isStdoutOutput: true
				}
				output: "crds-bundle.yaml"
			}]
			transformers: [
				{
					kind: "Kustomize"
					inputs: [for G in generators {G.output}]
					output: "kustomize-output-bundle.yaml"
					kustomize: kustomization: resources: inputs
				},
				{
					kind: "Command"
					inputs: [transformers[0].output]
					// this output is the artifact holos writes to the deploy
					// directory, one file per resource.
					output: artifact
					command: args: ["holos", "kubectl-slice", "-f", "\(BuildContext.tempDir)/\(inputs[0])", "-o", "\(BuildContext.tempDir)/\(artifact)"]
				},
			]
		}
```

Verify the result: every file under the component's deploy directory contains
exactly one resource (e.g. `grep -c '^kind:' <file>` is `1` for each file).

## Version pinning

Pin upstream versions **once**, in CUE, with a comment citing the
compatibility check that justifies the pin. From
[`components/gateway-api/buildplan.cue`](../components/gateway-api/buildplan.cue):

```cue
// VERSION pins the Gateway API standard channel CRDs.  Istio implements the
// Gateway API; pick a version supported by the Istio release targeted by
// HOL-1115.  Per https://istio.io/latest/docs/releases/supported-releases/
// the supported Istio releases (1.28+) all support Gateway API v1.4 — the
// Istio 1.28 change notes state "Upgraded Gateway API support to v1.4."
// Re-check the chosen Istio minor's release notes before bumping.
let VERSION = "1.4.1"
```

Rules:

- One pin per component (`let VERSION = "..."` at the top of
  `buildplan.cue`); everything else — fetch URLs, cache filenames — derives
  from it, so a bump touches exactly one line plus the regenerated cache and
  deploy files.
- The comment MUST record *why this version*: what it must be compatible
  with, where that was checked, and what to re-check before bumping.

## CRDs are isolated components

Components that ship CRDs MUST isolate them in a dedicated component labeled
`crds: "true"`, separate from the controllers and workloads that consume
them. The label identifies CRD components so they can be applied before
controllers (e.g. `holos show buildplans --selector crds==true` lists them);
apply ordering is manual today — see
[`holos/README.md`](../README.md#how-rendered-manifests-reach-the-cluster).
Keeping CRDs separate lets them roll out and verify ahead of dependent
workloads.
`gateway-api` is the worked example: it ships only the Gateway API standard
channel CRDs; the Istio control plane that implements them is a separate
component.

## Registration in the platform

A component does nothing until it is registered in
[`platform/platform.cue`](../platform/platform.cue) via `#ComponentTemplate`,
inside the `for CLUSTER in clusters` loop so every registered cluster gets
the component:

```cue
			(#ComponentTemplate & {inputs: {
				component: "gateway-api"
				cluster:   CLUSTER.name
				labels: {
					app:  "istio"
					crds: "true"
				}
			}}).output
```

Always set `cluster:` explicitly at the registration site — with a single
registered cluster the disjunction collapses to a concrete value, so an
omitted field silently binds to that cluster and breaks once a second cluster
is registered. Labels are copied to the BuildPlan and select subsets for
inspection and rendering:

```bash
holos show buildplans --selector cluster==k3d-holos
holos render platform --selector cluster==k3d-holos
```

## Render-then-commit workflow

Rendered manifests under `holos/deploy/` are build artifacts that are
**committed**. The workflow for any component change:

1. Edit the component CUE (and the cached `manifests/` bundle if the version
   changed).
2. Render from the `holos/` directory:

   ```bash
   cd holos
   holos render platform
   ```

3. Commit the CUE change **and** the regenerated `holos/deploy/` tree
   together.
4. Verify the deploy tree is diff-clean: re-running `holos render platform`
   immediately after a commit must produce no diff (`git diff --exit-code`).
   A dirty re-render means the component renders non-deterministically — fix
   that before merging.

## Conformance checklist

Before approving a component PR:

- [ ] Component integrates through `userDefinedBuildPlan` (not the
      author-style wrappers).
- [ ] Multi-resource output is sliced one file per resource via the
      `holos kubectl-slice` transformer; the artifact is a directory.
- [ ] Upstream version pinned once in CUE with a compatibility comment.
- [ ] Fetched manifests cached under `manifests/` and committed; the fetch
      script is executable and concurrency-safe.
- [ ] CRDs isolated in a dedicated component labeled `crds: "true"`.
- [ ] Registered in `platform/platform.cue` via `#ComponentTemplate` with an
      explicit `cluster:` field.
- [ ] `holos render platform` exits 0 and the committed `holos/deploy/` tree
      is diff-clean on re-render.
