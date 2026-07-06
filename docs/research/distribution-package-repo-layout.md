# Proposal: Team Repository Layout for Integrating Software into the Distribution

Date: 2026-07-05. Companion to
[Research: CUE Modules as a Package Ecosystem](cue-module-distribution.md);
bare section references (§) throughout point into that report. This document
turns its design recommendations into a concrete repository layout a
contributing team can adopt on day one.

## Question

A team wants to integrate software into the Holos platform distribution with
low friction and keep it maintainable over years: an SRE team contributing
dashboards and alert thresholds, a security team contributing admission
policy, or a platform/product team packaging an upstream component (Valkey,
Temporal, KubeRay) for reuse. How should that team lay out its repository —
what goes in it, what must stay out of it, how packages are tested and
published, and when the layout should change shape?

## Constraints the layout must satisfy

These fall directly out of the research and are restated here as layout
requirements:

1. **The publish unit is the CUE module, not the repository** (§2.1, §4.1).
   Each package is one module with its own `cue.mod/module.cue`, published
   with `cue mod publish` as an OCI artifact under an immutable semver tag.
   Consumers resolve module *paths* against a registry — they never see the
   repository. This is the load-bearing property: repo layout is a private
   choice the team can revise later without breaking a single consumer.
2. **Packages carry no site opinions** (§4.2, §4.3). No environment names, no
   promotion order, no SOC2 control mappings, no org-chart role names. The
   repo layout should make this structurally obvious: there is no place in a
   package module where site data would even live.
3. **Renders are hermetic** (§4.2). A component reads nothing outside its
   module; all variance arrives through `#Config` or `@tag()` injection.
   Anything the component consumes — vendored upstream Helm charts, Kustomize
   bases, CRD schemas — is committed inside the module.
4. **Mixins are separable** (§4.4). Dashboards/alerts/policy that unify over
   another package's output are their own packages, so a consumer can take
   the component without the mixin (or the mixin against an existing install).
5. **CI is stamped, not hand-rolled** (§4.9). Lint, compat-gate, test, and
   publish workflows come from the distribution's shared template repo
   (modulesync-style), so a fleet-wide toolchain change is one upstream PR.
6. **Examples are the test suite** (§4.9). Each package renders reference
   configurations in CI; the distribution's promotion pipeline (§4.8) re-runs
   those renders as the reverse-dependency gate when the package's own
   dependencies move.

## Recommendation in one sentence

Start every team on a **single package monorepo** — one repository holding
one CUE module per package under `modules/`, with committed golden renders as
tests, per-module release tags, and CI stamped from the distribution template —
and rely on the module/registry indirection to split packages out into
collective-owned repos later, invisibly to consumers, when a package
graduates to community maintenance (§5).

## The layout

```text
acme-sre-packages/                     # one repo per contributing team
├── README.md                          # what this team publishes, and where
├── LICENSE                            # Apache-2.0 (§5 neutrality)
├── .github/
│   └── workflows/                     # STAMPED from the distribution
│       ├── lint.yaml                  #   template repo — do not hand-edit
│       ├── test.yaml                  #   (modulesync model, §4.9)
│       └── publish.yaml
├── .managed-files.yaml                # manifest of template-owned files
├── Makefile                           # lint / test / publish entry points
└── modules/
    ├── valkey/                        # ── a COMPONENT package (§4.1) ──
    │   ├── cue.mod/
    │   │   └── module.cue             # pkg.acme.example/valkey@v1
    │   ├── README.md                  # interface docs: every #Config field
    │   ├── config.cue                 # closed #Config — the typed interface
    │   ├── component.cue              # #Config → Holos BuildPlan
    │   ├── vendor/
    │   │   └── charts/
    │   │       └── valkey-3.0.5.tgz   # vendored upstream chart (hermetic)
    │   └── examples/
    │       ├── default/
    │       │   ├── example.cue        # a reference #Config instance
    │       │   └── deploy/            # committed golden render (CI: diff-clean)
    │       └── ha-tls/
    │           ├── example.cue
    │           └── deploy/
    ├── valkey-observability/          # ── a MIXIN package (§4.4) ──
    │   ├── cue.mod/module.cue         # pkg.acme.example/valkey-observability@v1
    │   ├── README.md
    │   ├── thresholds.cue             # exported #Thresholds (overridable defaults)
    │   ├── dashboards.cue             # GrafanaDashboard CRs
    │   ├── alerts.cue                 # PrometheusRule / ServiceMonitor CRs
    │   └── examples/…
    └── valkey-crds/                   # ── a SCHEMA package (optional) ──
        ├── cue.mod/module.cue         # imported CRD types, no component
        └── …
```

Rationale for each choice follows.

### One repo per team, one module per package

The team is the unit of maintenance (§1.6 rule 7 — Vox Pupuli, Sous Chefs,
and Debian teams all converged here), so the repository boundary follows the
team, not the package. Within it, every directory under `modules/` is a
complete, independently versioned CUE module. Nothing at the repo root is
importable; the root holds only tooling and the stamped CI.

This gives the team monorepo ergonomics where they matter — one clone, one CI
configuration, atomic pull requests that touch a component and its mixin
together, one modulesync target — while keeping the *product* (the published
module) exactly as decoupled as a polyrepo would. The failure mode this
avoids is the one Ansible hit before the 2.10 split (§1.3): a monolith whose
release cadence couples unrelated content. Because each module here publishes
and versions independently, the monorepo never becomes a release monolith.

### Module paths are registry paths, owned by the team's namespace

Each `module.cue` declares a domain-qualified path under the team's claimed
prefix, with a major-version suffix:

```cue
module: "pkg.acme.example/valkey@v1"
language: version: "v0.11.1"
source: kind: "git"
```

Consumers route the prefix to the team's Quay organization via
`CUE_REGISTRY` longest-prefix routing (§2.1):

```text
CUE_REGISTRY='pkg.acme.example=quay.holos.internal/acme-sre,registry.cue.works'
```

The Quay organization itself is provisioned declaratively — a
`quay.holos.run` Organization CR with `syncedTeams[]` mapping the team's
Keycloak groups to push rights (§4.5) — so "who may publish under this
namespace" is the same Kubernetes-RBAC answer as everything else on the
platform. Claiming the domain on the Central Registry for discoverability is
optional and additive (§2.2).

### Component, mixin, and schema packages are siblings, not subpackages

`valkey` and `valkey-observability` are separate modules even though the same
team maintains both in the same repo. The research is explicit about why
(§4.4): the mixin unifies over resources the component emits, and a consumer
must be able to adopt either without the other — thresholds against an
existing Valkey install, or the component with the consumer's own dashboards.
Folding the mixin into the component's module would also drag
prometheus-operator and grafana-operator schema dependencies into every
consumer's MVS build list whether they run those operators or not.

The same logic yields the optional schema-only package (`valkey-crds`): pure
vendored types other packages import — one of the valid degenerate package
forms named in §4.1.

### Vendored inputs live inside the module

The component wraps the upstream Helm chart with the Holos Helm generator, so
the chart archive is committed under the module's `vendor/` directory and
referenced by relative path. This is what makes the render hermetic and the
published artifact self-contained: `cue mod publish` zips the module
directory, so the consumer's render needs no network fetch, no chart
repository credential, and no "was the chart the same one CI tested?"
question. Upgrading the upstream chart is an ordinary, reviewable PR that
changes a vendored file and the golden renders together.

### Examples with committed golden renders are the test suite

Each package ships at least one `examples/<name>/` directory holding a
reference `#Config` instance and its **committed rendered output**. CI
re-renders every example and fails on any diff — the same
render-then-commit, diff-clean discipline this repository already enforces
for `holos/deploy/` via `scripts/render`, applied at package scope.

This does three jobs at once:

- **Unit test**: a schema or template regression shows up as a diff in the PR
  that caused it, with the blast radius visible in review (the rendered
  manifest pattern's core property, §3.1).
- **Documentation**: the example is a working, copy-pasteable starting point
  for the consumer's profile.
- **Promotion-gate input**: the distribution's britney-style pipeline (§4.8)
  re-renders these examples — plus every reverse dependency's — at the
  candidate version before promoting it out of `unstable`. The package repo
  does not implement that gate; it only has to keep examples honest, which
  the diff-clean CI already forces.

The golden `deploy/` trees must never contain secret material — the runtime
secret-handling guardrail applies to package examples exactly as it applies
to this repo's deploy tree.

### Stamped CI, per-module tags, signed publishes

- **Stamping**: the workflows, linter configuration, and Makefile skeleton
  are owned by the distribution's template repository and rolled across all
  package repos mechanically (§4.9). A `.managed-files.yaml` manifest marks
  which files the template owns. Teams add package-specific jobs in separate
  files rather than editing managed ones.
- **Release tagging**: a monorepo of modules needs per-module versions; use
  the Go-monorepo convention of path-prefixed tags —
  `modules/valkey/v1.4.0`, `modules/valkey-observability/v0.3.2`. The
  publish workflow maps the tag prefix to the module directory and runs the
  gates for that module only.
- **The publish job** runs, in order: the distribution lint gate (`cue vet`
  against distribution schemas, closedness and forbidden-pattern checks —
  §4.8 item 2), the compatibility gate (exported definitions must subsume the
  previous minor's, §4.8 item 3), `cue mod publish <version>`, then cosign
  signing of the pushed artifact via OCI 1.1 referrers (§4.5). Publishing
  lands the version in the `unstable` channel; promotion to
  `testing`/`stable` happens in the distribution's pipeline, not in the team
  repo.

### Local development across modules

When a PR changes `valkey` and `valkey-observability` together, the mixin's
tests must build against the sibling's working-tree state rather than a
published version. Use `cue.mod/local-module.cue` replacements (CUE ≥ v0.17,
§2.1) pointing at the sibling directory; the lint gate rejects a *published*
module containing local replacements, so they are a development-time-only
convenience — the same discipline Go enforces around `replace` directives.

## What stays out of the team's package repo

The three-layer model (§4.3) assigns each concern a home; the package repo is
the *first* layer only. Explicitly not in this repo:

| Concern | Where it lives instead |
|---|---|
| Environment names and promotion order | The adopting org's profile layer |
| `#Config` *values* for a real deployment | The org's profile modules |
| Tag-injected site data (CA bundles, digests) | Injected at the consumer's render |
| Secrets, or any Secret material in examples | Runtime (ExternalSecret / bootstrap Job) |
| The rendered platform deploy tree | The consumer's platform repo |
| Admission *bindings* to real namespaces | The org's profile / platform spec |

A useful smell test when reviewing a package PR: if a change would be wrong
for a *second* organization adopting the distribution, it belongs in a
profile, not here. (A security team's package emits the
ValidatingAdmissionPolicy with `paramKind`-separated parameters; the
*binding* and the parameter values are the consumer's.)

If the team also operates its own slice of a live platform — an SRE team
usually does — that wiring is profile-layer content and belongs in the org's
platform repo (or a separate `acme-sre-profile` module there), never mixed
into the package repo. Keeping the repos apart is what keeps the packages
publishable.

## When to change shape: the graduation path

The monorepo is the right *starting* shape, not a commitment:

- **Stay a monorepo** while the packages are few and the team is the sole
  maintainer. Atomic cross-package PRs and single-target stamping outweigh
  everything else at this scale.
- **Extract a package to its own repo** when it graduates to collective
  maintenance (the Vox Pupuli adoption path, §5) — community-maintained
  packages want per-package issue trackers, maintainer teams, and repo
  permissions. The extraction is mechanical: move the module directory with
  history, keep the module path identical, re-stamp CI, publish the next
  version from the new repo. **No consumer notices**, because consumers
  depend on `pkg.acme.example/valkey@v1` in a registry, not on a Git URL —
  this is the payoff of constraint 1, and it is the property Puppet/Ansible
  teams never had (a Forge/Galaxy listing pinned to one repo's ownership).
- **Never split by artifact kind.** A component and its examples, or a mixin
  and its thresholds, stay together; splitting the test suite from the thing
  it tests recreates the stale-example rot the Forge's long tail suffered
  (§1.1).

## Worked example: an SRE team integrates Valkey

1. **Scaffold**: create `acme-sre-packages` from the distribution template
   (stamped CI arrives with the scaffold); add `modules/valkey/` with
   `cue mod init pkg.acme.example/valkey@v1 --source=git`.
2. **Author**: vendor the upstream chart under `vendor/charts/`; write the
   closed `#Config` (image pinning, replicas, TLS, persistence — *no*
   environment field); write the component lifting the chart output into CUE;
   add `examples/default/` and commit its golden render.
3. **Mixin**: add `modules/valkey-observability/` exporting `#Thresholds`
   with defaulted, overridable values and emitting `GrafanaDashboard` +
   `PrometheusRule` CRs keyed off the component's labels.
4. **Publish**: tag `modules/valkey/v0.1.0`; the stamped workflow lints,
   checks compatibility (trivial for a first release), publishes to the
   team's Quay org, signs, and the version appears in `unstable` on the
   `Package` CR's status (§4.6).
5. **Consume**: the org's platform team adds one profile-layer entry setting
   `#Config` values and unifying the security team's constraint package over
   the output; the platform spec references the profile. Promotion to
   `testing` follows once the distribution's reverse-dependency renders pass
   (§4.8 item 4).

Total team-owned surface: one repo, two modules, zero site opinions, zero
bespoke CI.

## Open questions

- **Template-repo mechanics**: whether stamping uses a modulesync-style
  config repo, GitHub repository templates plus a sync bot, or reusable
  workflows referenced by SHA — needs a decision in the distribution's
  policy manual before the second team onboards (§4.9 says "from day one").
- **Golden-render size**: very large rendered examples (CRD-heavy operators)
  may warrant rendering to a digest manifest rather than committing full
  trees; defer until a real package hits the problem.
- **Monorepo MVS interplay**: sibling modules in one repo still depend on
  each other by *published* version outside development; teams need guidance
  (in the policy manual) on release ordering when a PR bumps both — publish
  the dependency first, then the dependent.
