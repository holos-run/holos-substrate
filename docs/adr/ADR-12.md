# Repository Layout for Multiple Go Services

| Metadata | Value                      |
| -------- | -------------------------- |
| Date     | 2026-06-10                 |
| Author   | @jeffmccune                |
| Status   | `Approved`                 |
| Tags     | layout, conventions, build |

| Revision | Date       | Author      | Info                                                     |
| -------- | ---------- | ----------- | -------------------------------------------------------- |
| 1        | 2026-06-10 | @jeffmccune | Initial design                                           |
| 2        | 2026-06-11 | @jeffmccune | Add `holos/deploy/` and `holos/docs/` to the layout tree |
| 3        | 2026-06-14 | @jeffmccune | Note the NATS webhook receiver/subscriber/deployer were retired (HOL-1241, [ADR-16](ADR-16.md)); the single-module/single-binary layout decision stands |
| 4        | 2026-06-14 | @jeffmccune | The CLI is built with Fisk, not Cobra ([ADR-17](ADR-17.md)); the command tree lives in `internal/cli` |
| 5        | 2026-06-17 | @jeffmccune | The first concrete controller API group is `quay.holos.run` ([ADR-18](ADR-18.md)), refining the `registry.holos.run` illustration below; the multi-group `<group>.holos.run` convention is unchanged |
| 6        | 2026-06-18 | @jeffmccune | The Holos Controller (HOL-1309..HOL-1313, [ADR-18](ADR-18.md)/[ADR-19](ADR-19.md)) ships as a **second binary and image** (`cmd/holos-controller` + `Dockerfile.controller`) **within the same single module** — a bounded exception to Option A's one-binary/one-image rule for a process with a different lifecycle (controller-runtime manager). Its conventional-kubebuilder `main.go` (stdlib `flag` + zap) is a recorded **carve-out from the Fisk-only CLI guardrail** ([ADR-17](ADR-17.md)), which stays scoped to the user-facing `holos-paas` CLI. The single-module decision and `api/<group>/<version>` layout are unchanged |

> **Note (rev 3):** The NATS webhook receiver, subscriber, and deployer named
> below as motivating services were retired in HOL-1241 — deployment moved to
> Kargo plus the client-side ORAS publish workflow ([ADR-16](ADR-16.md)), and
> the `internal/webhook` / `internal/nats` packages were deleted. The layout
> **decision** this ADR records (Option A: single module, single multi-service
> binary, kubebuilder multi-group conventions, all implementation under
> `internal/`) is unchanged and still governs; the service names and
> `internal/` example paths in the sections below are the decision-time
> illustration, not a current inventory.

## Context and Problem Statement

holos-paas must host several cooperating Go services alongside the Holos CUE
configuration that renders the platform's manifests: controllers reconciling
a related set of custom resources (the platform API per [ADR-2](ADR-2.md)),
the NATS webhook receiver, webhook subscriber, and deployer
([ADR-9](ADR-9.md), [ADR-10](ADR-10.md), [ADR-11](ADR-11.md)), a reverse
proxy that authenticates requests through OIDC and uses Kubernetes
impersonation headers to grant developers `kubectl` access
([ADR-3](ADR-3.md)), and small reconcilers for Keycloak group membership and
Quay self-service resources. How should the repository be laid out so all of
this is modeled and managed through the Kubernetes API and fits the Holos
rendered-manifests pattern, without slowing down the MVP?

## References

- [Research: Repository Layouts for Multiple Go Services](../research/go-multi-service-repo-layout.md)
  — survey of Kargo, Argo CD, Crossplane, Pinniped, cert-manager,
  external-secrets, Cluster API, and Flux; this ADR's option set and the
  evidence behind the decision come from that report.
- [Holos PaaS MVP milestones](../planning/holos-paas-mvp-milestones.md) and
  the Linear *Holos PaaS* project (Layer 0 Cluster Foundation → Layer 1
  Platform Services → Layer 2 PaaS Core → Layer 3 User Workloads → Demo
  Walkthrough, target 2026-06-30).
- The existing `holos-run/holos-controller` repository: a kubebuilder v4
  scaffold (`domain: holos.run`, `api/v1alpha1`, `cmd/main.go`, `internal/`)
  that this layout absorbs.
- [Kubebuilder multi-group layout](https://book.kubebuilder.io/migration/multi-group.html),
  [Go: managing module source](https://go.dev/doc/modules/managing-source).

## Options Considered

- **Option A — Single module, single binary, subcommand per service.** The
  Kargo/Crossplane/Pinniped shape: kubebuilder multi-group conventions, one
  `cmd/`, one container image, each Deployment runs the image with a
  different subcommand.
- **Option B — Single module, binary and image per service.** The
  cert-manager shape (without per-binary modules): `cmd/<service>/` each
  with its own Dockerfile and image.
- **Option C — Multi-module monorepo.** Root module plus a separate `api/`
  module now (Crossplane/external-secrets shape), `go.work` for local
  development.
- **Option D — Polyrepo.** The Flux shape: one repo per service,
  holos-paas keeps only the CUE configuration, `holos-controller` continues
  as a separate repository.

## Ranking Against the MVP Milestone Goals

The MVP goals that discriminate between the options: Layer 2 delivers four
cooperating services in roughly one week (2026-06-17 → 06-24); the Demo
Walkthrough requires a tight build → push → deploy loop on k3d (Apple
Silicon); dependencies must stay minimal; the API surface is still unstable
(`v1alpha1`); one developer plus coding agents do all the work; and every
capability must surface as Kubernetes resources rendered by Holos.

1. **Option A — ranked first.** One `go build`, one Dockerfile, one image to
   push for the Demo Walkthrough; the deployer pins a single image tag in
   the Holos config. A change that touches the API types, two services, and
   the CUE components lands as one atomic commit — the common case while
   ADRs 6–11 are being implemented. This is the dominant pattern among the
   surveyed control planes (Kargo, Argo CD, Crossplane, Pinniped,
   external-secrets all converge on it).
2. **Option B — second.** Clean service separation, but N Dockerfiles and N
   image pushes in the inner loop, multiplied by every Layer 2 iteration.
   Worth revisiting per-service only if a component needs a pruned
   dependency tree (cert-manager's reason: its acmesolver runs inside user
   workloads — no holos-paas service does).
3. **Option C — third.** A separate `api/` module serves external Go
   consumers, which do not exist yet, and costs prefixed version tags and
   replace-directive upkeep immediately. Kargo, Crossplane, and
   external-secrets all extracted their API module *after* consumers
   appeared; the layout below keeps `api/` extractable so we can do the
   same.
4. **Option D — last.** Flux's polyrepo exists so third parties can consume
   controllers independently — not an MVP goal — and costs sequenced
   multi-repo releases, a version matrix, and duplicated scaffolding that a
   solo developer cannot amortize before 2026-06-30.

## Design

Option A, laid out with kubebuilder multi-group conventions:

```text
holos-paas/
├── AGENTS.md                  # entry point for coding agents; indexes docs
├── Dockerfile                 # one multi-stage build → one image
├── Makefile                   # build, test, codegen, k3d targets
├── go.mod                     # single module: github.com/holos-run/holos-paas
├── cmd/
│   └── holos-paas/
│       └── main.go            # entry point → internal/cli (Fisk root, ADR-17)
├── api/                       # CRD types: api/<group>/<version>
│   └── paas/v1alpha1/         # e.g. paas.holos.run: Project, Application
├── internal/                  # all implementation; no pkg/ directory
│   ├── controller/            # reconcilers, one package per area
│   │   ├── application/       # ADR-11 Application reconciler
│   │   ├── project/           # ADR-1 Project reconciler
│   │   ├── keycloak/          # Keycloak group membership reconcilers
│   │   └── quay/              # Quay self-service reconcilers
│   ├── webhook/
│   │   ├── receiver/          # ADR-9 thin NATS ingress
│   │   └── subscriber/        # ADR-10 parse & dispatch
│   ├── deployer/              # ADR-11 deployer task subscriber
│   ├── authproxy/             # OIDC → impersonation-headers reverse proxy
│   ├── keycloak/              # Keycloak admin API client
│   ├── quay/                  # Quay API client
│   └── nats/                  # ADR-6 JetStream connection/stream helpers
├── holos/                     # Holos CUE: deployment config and policy
│   ├── cue.mod/
│   ├── platform/
│   ├── components/            # one component per Deployment + paas CRDs
│   ├── deploy/                # rendered manifests, committed
│   └── docs/                  # component guidelines and placeholders
├── hack/                      # scripts, boilerplate, k3d helpers
└── docs/                      # adr/, planning/, research/, demo/
```

The load-bearing choices:

- **One binary, one image.** `cmd/holos-paas/main.go` exposes subcommands:
  `controller` (a single controller-runtime manager running all
  reconcilers), `webhook-receiver`, `webhook-subscriber`, `deployer`, and
  `authproxy`. Each is a separate Deployment — a Holos component under
  `holos/components/` — running the same image with different args, so the
  rendered manifests pin exactly one image reference.
- **`api/<group>/<version>` in the root module.** Multi-group from day one
  (`paas.holos.run` first; additional groups such as `registry.holos.run`
  get sibling directories — the first concrete controller group is
  `quay.holos.run`, the registry data plane, per [ADR-18](ADR-18.md)). API
  packages import only
  `k8s.io/api`/`apimachinery`, keeping the tree extractable into its own
  module later without import-path churn beyond the module prefix.
- **Everything else under `internal/`.** Following Crossplane and Russ Cox's
  guidance rather than golang-standards/project-layout: no `pkg/` directory;
  the repository's public Go surface is empty until there is a consumer.
- **Keycloak and Quay self-service are CRDs, not CLIs.** Group membership
  and registry resources are declared as custom resources and reconciled by
  controllers registered in the same manager, mirroring how Pinniped manages
  its own impersonation proxy via a controller. This keeps ADR-2's
  KRM-as-primary-API principle intact: the "small tools" are control loops.
- **Codegen lands in the Holos config.** `make generate` runs controller-gen
  for deepcopy; `make manifests` emits CRD YAML into the directory consumed
  by a `holos/components/` CRDs component (the same shape as Kargo emitting
  CRDs into its Helm chart), so `holos render platform` always renders the
  CRDs matching the compiled types.
- **Holos CUE lives in `holos/`.** Deployment configuration and policy are
  isolated from Go code at the top level: `cue.mod/`, `platform/`,
  `resources.cue`, `schema.cue`, and `tags.cue` live under `holos/` (moved
  from the repository root in commit `f948372`).

### Second binary: the Holos Controller (Revision 6)

Option A's "one binary, one image" choice is the default, not an absolute. The
**Holos Controller** ([ADR-18](ADR-18.md), [ADR-19](ADR-19.md); shipped in
HOL-1309..HOL-1313) is a deliberate, bounded exception within the **same single
module** `github.com/holos-run/holos-paas`:

- **A second binary and image.** `cmd/holos-controller/main.go` builds to its own
  container image via **`Dockerfile.controller`** (a two-stage cross-compile →
  distroless build, distinct from `holos-paas`'s `Dockerfile`). The controller is
  a long-running controller-runtime **manager** with a different operational
  lifecycle (leader election, CRD/RBAC install, metrics scrape) than the
  user-facing `holos-paas` CLI, which is why it is its own image rather than a
  subcommand. Its build/deploy targets live in a separate **`Makefile.controller`**
  (`controller-build`/`controller-test`/`controller-manifests`/`controller-deploy`,
  etc.), cleanly isolated from `scripts/apply`, `scripts/render`, and the
  `holos-paas` targets. The single-module rule is unchanged — both binaries share
  one `go.mod` and the `api/<group>/<version>` tree.
- **Conventional-kubebuilder `main.go` is a carve-out from the Fisk guardrail.**
  [ADR-17](ADR-17.md) requires the user-facing **`holos-paas` CLI** to be built
  with **Fisk, not Cobra**, for LLM-legible help and introspection. The
  controller's `main.go` instead uses the conventional kubebuilder shape —
  stdlib **`flag`** plus controller-runtime **`zap`** flags
  (`--metrics-bind-address`, `--health-probe-bind-address`, `--leader-elect`,
  `--metrics-secure`, and the zap logging flags) — because it is a manager
  process, not an interactive CLI: its "flags" are deployment-time process
  configuration set once in the Deployment manifest, not a command surface a human
  or agent explores. This carve-out is **scoped to the controller manager
  process**; it does not relax the Fisk requirement for the `holos-paas` CLI in
  `internal/cli`. The controller emits **JSON logs** (zap production config) for
  Datadog/LGTM ingestion (ADR-18/ADR-19).

## Decision

Adopt Option A: a single-module monorepo with one multi-service binary and
one container image, kubebuilder multi-group conventions
(`api/<group>/<version>`, `internal/controller/<area>`), Holos CUE under
`holos/`, and CRD codegen emitted into the Holos components. Absorb the
`holos-run/holos-controller` scaffold into this layout (`api/v1alpha1` →
`api/paas/v1alpha1`, `cmd/main.go` → the `controller` subcommand).

## Consequences

- **Migration:** the root-level CUE files moved into `holos/` (commit
  `f948372`); CI and developer invocations of `holos render platform` run
  from that directory. The `holos-controller` repository is archived after
  its scaffold is absorbed.
- **Coupled releases, with one deliberate split:** the `holos-paas` services
  ship in one image on one version. The Holos Controller (Revision 6) is the one
  component promoted to its own binary/image (`Dockerfile.controller`) because its
  manager lifecycle differs from the CLI's; both still share the single module, so
  the split is the additive "promote a component to its own image" path this ADR's
  *A future split stays cheap* consequence anticipated, not a reshape of the tree.
- **Two CLI styles, by design:** the `holos-paas` CLI is Fisk
  ([ADR-17](ADR-17.md)); the controller manager's `main.go` is conventional
  kubebuilder (stdlib `flag` + zap). The carve-out is recorded so the Fisk
  guardrail is not misread as forbidding the standard controller-runtime entry
  point — it governs the interactive CLI, not a manager process.
- **No importable Go API:** external consumers cannot `go get` the CRD types
  until `api/` is extracted into its own module. The layout makes that a
  mechanical change (add `api/go.mod`, prefixed tags) when a consumer
  appears.
- **A future split stays cheap:** because each service is an `internal/`
  package wired to a subcommand, promoting one to its own binary, image, or
  repository (Option B/D) is additive and does not reshape the tree.
- **RBAC:** the single manager's ServiceAccount aggregates the RBAC of all
  reconcilers; per-service ServiceAccounts apply to the receiver, subscriber,
  deployer, and authproxy Deployments individually via their Holos
  components.
