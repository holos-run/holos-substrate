# Research: CUE Modules as a Package Ecosystem — Toward a Holos Platform Distribution

Research date: 2026-07-05. Web claims were verified against live sources on
that date; release versions marked with a date were checked against the
projects' GitHub releases.

## Question

The Holos platform is Kubernetes-native, built from substrate building
blocks whose primary consumers are platform and product engineers, with the
Kubernetes API as the
platform's first-class interface ([ADR-2](../adr/ADR-2.md)). We are building a
distribution of CNCF software, and a distribution needs a packaging system:
a way for platform engineers, SRE teams, security teams, and product engineers
to publish reusable configuration packages that compose — over years, across
contributors — into one robust, reliable platform, the way Puppet modules
composed on the Forge, Vox Pupuli collectively maintained them, and Debian
packages compose into a coherent operating system.

The design constraints, from the issue:

- **CUE modules** as the package unit if possible; **OCI images** as the
  primary distribution method.
- The interface layer must be **transparent and un-opinionated** — the
  `io.Reader`/`io.Writer` standard, not a framework. Enterprises must be able
  to implement their own SOC2 controls and environment handling without the
  package imposing an opinion that conflicts with another organization's.
- **SRE teams** contribute dashboards and alert thresholds; **security teams**
  contribute policy; **product and platform engineers** write and publish
  components for reuse, mixing that policy in.
- The **rendered manifest pattern** with GitOps (Argo CD + Kargo) is the
  default delivery method, and the system must **also support bypassing Git**:
  render locally, package into an OCI image, apply an Argo CD `Application`
  (or Flux `Kustomization`) directly against the apiserver.
- A **future web interface** must be supportable; the Kubernetes API is the
  primary API and Kubernetes authn/authz is the **sole** RBAC model.
- Packages must **compose into the distribution and stay maintainable**, with
  a community model like Vox Pupuli and Debian as the target.

This report surveys the prior art (§1), the current CUE module and OCI
ecosystem (§2), the Kubernetes delivery landscape (§3), then makes concrete
design recommendations for the Holos component package model and its custom
controllers (§4), proposes the community model (§5), and drafts the roadmap
from the minimal distribution to the job-scheduling/workflow use cases (§6).

## Executive summary

1. **The prior art converges on one architecture.** Every successful
   configuration ecosystem independently discovered a three-layer separation:
   a *generic package* with typed parameters and shipped defaults, a *named
   site-owned wiring layer* (Puppet profiles are the high-water mark), and
   *site data* with deterministic precedence. Ecosystems whose boundary was
   implicit or untyped (Chef attributes, Helm values sprawl) produced "wrapper
   hell"; ecosystems that named and typed the boundary aged well. The single
   most transferable unimplemented idea is Debian's promotion gate: **an
   update that breaks its reverse dependencies never ships** (britney2 +
   autopkgtest). No configuration ecosystem has matched it.

2. **CUE modules are ready.** Registry-backed CUE modules have been the
   default since v0.9.0 (June 2024) and the only supported mechanism since
   v0.11.0; modules are OCI artifacts (`application/vnd.cue.module.v1+json`)
   resolved by Minimal Version Selection against any OCI registry via
   `CUE_REGISTRY` prefix routing. The Central Registry is CUE Labs'
   commercial service (beta), but the protocol has no registry lock-in — the
   in-cluster Quay can serve a private module registry today. The gaps a
   distribution must fill itself: no signing integration in `cue mod publish`
   and no publish-time schema-compatibility gate.

3. **OCI is the neutral substrate on both GitOps engines.** Argo CD gained
   native OCI Application sources in v3.1 (Aug 2025); Flux's OCIRepository is
   GA since 2.6 (May 2025) with built-in cosign/notation verification. Kargo
   (v1.10) is the purpose-built promoter over OCI artifacts. The rendered
   manifest pattern this platform already implements ([ADR-16](../adr/archive/ADR-16.md))
   is now the mainstream position, shipped natively upstream (Argo CD's Source
   Hydrator).

4. **CUE's unification is the mechanism the interface requirement asks for.**
   Unification is commutative, associative, and idempotent — a security team's
   policy module and an SRE team's alert-threshold module unify into a
   component's output identically no matter where they land in the import
   graph, and conflicts surface as render-time errors instead of silent
   overrides. Closed definitions give packages a typed contract; `@tag()`
   gives late-binding injection. This is exactly the property set Helm values,
   Chef attributes, and ytt overlays approximate procedurally.

5. **Recommendation in one sentence:** model a distribution package as a CUE
   module that exports closed definitions and a `BuildPlan`-producing
   component (the transparent `io.Reader`-style contract), distributed as an
   OCI artifact through Quay, composed by site-owned "profile" modules that
   unify enterprise policy (environments, SOC2 controls, dashboards, alerts,
   admission policy) into rendered manifests, delivered by the existing
   Argo CD + Kargo machinery — with a Debian-style staged promotion pipeline
   (render + reverse-dependency checks as the gate) and a Vox-Pupuli-style
   collective namespace as the community institution.

---

## 1. Lessons from prior packaging ecosystems

### 1.1 Puppet: modules, the Forge, and Vox Pupuli

The Puppet module is a directory with a conventional layout and a
`metadata.json` (name, semver version, dependencies with version ranges)
published to the central [Puppet Forge](https://forge.puppet.com/about/approved)
under a per-user namespace. The Forge layers trust signals on top: automated
quality scores, and the curated **Approved** (community, quality-reviewed) and
**Supported** (commercially supported) tiers.

Puppet's real contribution is the three-generation arc of the
generic-vs-site-specific separation — the key question for this design:

1. **`params.pp` — data embedded in code.** Every module carried a `params`
   class: a giant case statement over OS facts producing defaults. Data lived
   in code; porting to a new OS meant editing conditionals. R.I. Pienaar's
   influential 2013 posts
   ([Better Puppet Modules Using Hiera Data](https://www.devco.net/archives/2013/12/08/better-puppet-modules-using-hiera-data.php),
   [The problem with params.pp](https://www.devco.net/archives/2013/12/09/the-problem-with-params-pp.php))
   argued the fix was to get data out of code entirely.
2. **Hiera 5 data-in-modules.** Each module got its own `hiera.yaml` and
   `data/` directory — the "module layer" of a three-layer lookup (global →
   environment → module). The same lookup mechanism serves module-author
   defaults and operator overrides with well-defined precedence; the interface
   is the class's *typed parameter list*, the defaults are *data*
   ([Puppet docs on the params.pp pattern](https://help.puppet.com/core/current/Content/PuppetCore/module_data_params.htm)).
3. **Roles & profiles — the canonical architecture**
   ([the roles and profiles method](https://www.puppet.com/docs/puppet/7/the_roles_and_profiles_method.html)).
   Two *site-owned* abstraction layers sit between nodes and reusable
   component modules: **profiles** (each configures one technology stack by
   declaring component classes and setting their parameters — component
   classes are "always declared via a profile, never assigned directly to a
   node") and **roles** (pure aggregations of profiles, one per class of
   machine). The genius is that the pattern *names the boundary*: generic
   modules never absorb site logic (so they stay publishable and upgradable);
   site logic never leaks into modules (so it stays greppable in one repo);
   business data lives in Hiera, wiring lives in profiles, mechanism lives in
   modules. This is the most explicit generic/site separation any surveyed
   ecosystem achieved — by convention and documentation, not tooling
   enforcement.

**Vox Pupuli** is the community institution: "a collective of Puppet module,
tooling and documentation authors working under a shared name and namespace"
([voxpupuli.org](https://voxpupuli.org/)). Its mechanics are the model for §5:

- **A shared namespace (`puppet/`) owned by the collective, not a person.**
  No module has a single owner; if an author abandons a module, maintenance
  continues.
- **A documented [migration process](https://voxpupuli.org/docs/migrate_module/)
  for abandoned modules** — repo transfer, CI wiring, Forge deprecation of the
  stale listing.
- **Fleet-wide standardization via [modulesync](https://voxpupuli.org/docs/updating-files-managed-with-modulesync/):**
  one config repo templates the CI, test harness, and release tooling across
  ~200 module repos; one PR rolls a toolchain upgrade across the fleet.
- **The stress test:** when Perforce closed Puppet's development behind an
  EULA in late 2024, Vox Pupuli
  [declined the EULA](https://voxpupuli.org/blog/2025/05/19/perforce-eula/)
  and became the home of **OpenVox**, the community fork of the platform
  itself ([first release Jan 2025](https://voxpupuli.org/blog/2025/01/21/openvox-release/)).
  A module collective proved robust enough to fork the platform when the
  vendor rug-pulled — the strongest validation of the collective-maintenance
  model available.

What failed: the long tail of one-author Forge modules still rots (the Forge
surfaces staleness but doesn't fix it), and dependency solving across
community modules with conflicting `stdlib` bounds pushed production sites to
full version pinning in a control-repo `Puppetfile` — lockfile-style pinning
at the site, ranges as compatibility declarations only.

### 1.2 Chef: wrapper hell and the lock-and-promote correction

Chef's separation mechanism was **node attributes plus the wrapper-cookbook
pattern**: a community cookbook exposes `default` attributes; the site writes
a thin wrapper that overrides attributes and `include_recipe`s the upstream.
It was substantially more painful than Puppet's profiles, for structural
reasons worth internalizing:

- **Attribute precedence was a fifteen-cell matrix**
  ([Chef attribute precedence](https://docs.chef.io/attribute_precedence/)) —
  four precedence levels times multiple sources. "Where does this value come
  from?" had fifteen answer paths where Puppet had one.
- **Derived attributes broke under wrapping**: if upstream computes attribute
  C from A and B in its attributes file, a wrapper overriding A does not
  recompute C — the derived value freezes with the old input
  ([the classic thread](https://discourse.chef.io/t/wrapper-cookbooks-and-attribute-logic/6663)).
- **The interface was implicit** — whatever attributes the recipes happened to
  read. No typed parameter list, no schema.

Chef's two corrections both matter here. **Policyfiles** replaced Berkshelf's
run-time server-side dependency solving with solve-once, freeze an immutable
`Policyfile.lock.json`, and promote that exact artifact through policy groups
([About Policyfiles](https://docs.chef.io/policyfile/)) — the industry-wide
runtime-solve → lock-and-promote migration in miniature. **Custom resources**
replaced attributes-as-interface with typed, property-based resources that
site cookbooks *call* — composition by calling a typed interface instead of
overriding a black box from outside. The community collective **Sous Chefs**
(~100 cookbooks, 50+ maintainers, an explicit adoption process and fleet
automation — [sous-chefs.org](https://www.sous-chefs.org/)) evolved the same
anatomy as Vox Pupuli independently; the convergence is evidence that this is
the stable equilibrium for community configuration maintenance.

Chef also supplies the monetization cautionary tale: the 2019 binary-licensing
pivot spawned the CINC rebuild, Progress acquired Chef in 2020 and cut staff,
and community energy visibly drained.

### 1.3 Ansible: collections, precedence-as-contract, two-tier trust

Ansible's unit evolved from **roles** to **collections** (namespaced artifacts
bundling roles, modules, plugins; `galaxy.yml` metadata; content addressed by
FQCN like `community.general.ufw`). The **2.10 "big bang" split** moved 3,000+
modules out of the monolithic core repo into collections, decoupling release
cadence and de-anonymizing maintenance
([porting guide](https://docs.ansible.com/ansible/latest/porting_guides/porting_guide_2.10.html)).
The `ansible` community package is itself a *curated composition* with
documented [inclusion requirements](https://docs.ansible.com/projects/ansible/latest/community/collection_contributors/collection_requirements.html)
(published at ≥1.0.0, immutable released artifacts, public tagged repo) —
policy-as-gatekeeping for what ships in the distribution, directly echoing
Debian.

The generic/site mechanism is the **`defaults/` vs `vars/` split plus one
linear variable-precedence list**: a role's `defaults/main.yml` is its public,
intended-to-be-overridden interface (lowest precedence of all sources);
`vars/` is internal (high precedence, effectively private); site config lives
in inventory and wins deterministically. Unlike Chef, "who wins" is
learnable; unlike Puppet, there is no typed contract — a role's interface is
whatever its README documents, and typos in override names fail silently.

Two governance features to copy: **governed namespaces** (namespace grants are
human-reviewed; `ansible` and `community` are reserved) and the **two-tier
trust model** — community Galaxy vs Red Hat Automation Hub certified content,
same artifact format, different support contracts
([certified content FAQ](https://access.redhat.com/articles/4916901)).

### 1.4 Debian: the aspirational model

Debian 13 "trixie" ships **38,067 source packages** built by thousands of
maintainers that compose into one coherent operating system
([trixie stats](https://sources.debian.org/stats/trixie/)). The mechanisms:

1. **Policy as the composition contract.** The
   [Debian Policy Manual](https://www.debian.org/doc/debian-policy/)
   standardizes everything two packages might fight over — filesystem layout,
   config-file semantics, maintainer-script behavior, shared-library ABI
   handling. Strangers' packages compose because the interface between any two
   packages is defined by distribution-wide policy, not bilateral negotiation.
   **Lintian** makes the policy machine-checkable at archive scale
   ([Lintian manual](https://lintian.debian.org/manual/index.html)).
2. **Promotion gates, not just publication gates.** Uploads land in
   *unstable*; the **britney2** migration software promotes a package to
   *testing* only after it ages without release-critical bugs, builds on all
   architectures, breaks no other package's installability, and — since
   2018 — does not regress the **autopkgtest** results *of itself or any
   reverse dependency*
   ([the migration announcement](https://lists.debian.org/debian-devel-announce/2018/05/msg00001.html)).
   The registry itself refuses updates that break consumers. **No
   configuration ecosystem has matched this**, and it is the most transferable
   unimplemented idea in this survey.
3. **Site-owned state is structurally protected.** dpkg's conffile semantics
   guarantee a locally modified config file is never silently overwritten on
   upgrade; **debconf** separates "questions a site must answer" (preseedable,
   priority-ranked) from both package internals and site config files.
4. **Teams, not individuals, are the unit of maintenance**, with standardized
   repo conventions (DEP-14 branch layout) and institutionalized rescue paths
   for every stage of abandonment (NMUs, salvaging, orphaning to QA).

The cost is velocity: stable is famously old, and the freeze process is slow.
Debian trades speed for composability and longevity — the correct trade for a
*platform*, which is why it is the aspirational model here.

### 1.5 Helm: the values API problem and the Bitnami lesson

The chart's `values.yaml` *is* its interface — defaults shipped by the author,
overridden per site. Because values are the only interface, every knob a user
might need must be pre-anticipated as a template parameter; authors respond by
parameterizing everything, producing thousand-line untyped values files
(partially mitigated by `values.schema.json`), with errors referencing
rendered output rather than source
([helm#8632](https://github.com/helm/helm/issues/8632)). Structurally, Helm
collapses Puppet's three layers (module data / profile wiring / site data)
into one flat mechanism, so the generic/site boundary sits wherever each chart
author happened to draw it. The load-bearing escape hatches
(`extraEnvVars`, `podAnnotations`, post-render Kustomize) exist precisely
because the values interface can never anticipate every site need.

Ecosystem history offers three warnings:

- **The centralized `helm/charts` monorepo collapsed** under curation labor
  (~300 charts, maintainer burnout, deprecated Nov 2020 —
  [deprecation post](https://helm.sh/blog/charts-repo-deprecation/)).
  Centralized *curation* doesn't scale; centralized *policy* with distributed
  maintenance (Debian) does.
- **The OCI migration succeeded** (HIP-6, GA in Helm 3.8): charts stored in
  the same registries as images, unifying auth, infrastructure, and signing
  ([Helm OCI docs](https://helm.sh/docs/topics/registries/)). Artifact Hub —
  an *index*, not a registry, with automated **verified publisher** and
  reviewed **official** flags — scaled discovery without central curation
  ([Artifact Hub repos](https://artifacthub.io/docs/topics/repositories/)).
- **The Bitnami/Broadcom rug-pull (2025)** is the supply-chain lesson:
  effective August 2025 Broadcom moved most versioned Bitnami images to an
  unmaintained archive and put the hardened catalog behind a subscription
  ([bitnami/charts#35164](https://github.com/bitnami/charts/issues/35164)).
  Thousands of organizations discovered their clusters' Postgres/Redis
  defaults were a single vendor's revocable free tier. Because no
  Vox-Pupuli-style collective held the namespace, the corpus was captured, not
  forked. **Free-tier dependence on a single vendor's artifact catalog is a
  supply-chain liability; community institutions, not licenses, are the
  hedge.**

### 1.6 Synthesis: the recurring patterns

Every ecosystem converged on the same three-layer answer, differing in how
*named and typed* each layer is:

| Layer | Puppet | Chef | Ansible | Debian | Helm |
|---|---|---|---|---|---|
| Generic mechanism | component module (typed params) | cookbook (attributes → custom resources) | role/collection | package | chart templates |
| Shipped defaults | Hiera module data | `attributes/default.rb` | `defaults/main.yml` | conffiles + debconf defaults | `values.yaml` |
| Site wiring/data | **profiles + site Hiera** (named layer) | wrapper cookbooks | playbooks + inventory | preseeds + conffile edits | override values files |

The distilled rules for the design in §4:

1. **The separation succeeds in proportion to how named and typed the boundary
   is.** Typed contracts (Puppet 4 class params, Chef custom resources) aged
   well; implicit or untyped interfaces (Chef attributes, Helm values) bred
   wrapper hell and sprawl.
2. **Separate data from code, then defaults from overrides**, with one
   deterministic resolution path. Site truth must be structurally protected
   from upstream churn.
3. **Escape hatches are load-bearing.** No pre-declared interface anticipates
   every site; ecosystems without sanctioned escape hatches get forks instead.
4. **Lock and promote immutable artifacts**; never resolve dependencies at
   deploy time.
5. **Gate promotion on reverse dependencies' tests** (Debian, unmatched
   elsewhere).
6. **Namespaces encode trust**: governed or collective namespaces survive;
   flat first-come namespaces rot.
7. **The unit of sustainable maintenance is the collective with synced
   tooling** — Vox Pupuli, Sous Chefs, and Debian teams evolved the same
   anatomy independently.

---

## 2. CUE modules today

### 2.1 The module system is the default, and it is OCI-native

Registry-backed CUE modules shipped as an experiment in early 2024, became the
default in **v0.9.0 (June 2024)**, and have been the *only* supported
mechanism since **v0.11.0 (Nov 2024)**, which dropped the old registry-less
vendoring model
([new-vs-old modules FAQ](https://cuelang.org/docs/concept/faq/new-modules-vs-old-modules/)).
Current release: **v0.17.0 (2026-06-29)**, which added `cue.mod/local-module.cue`
replacement (CUE's answer to Go's `replace` for local development)
([release notes](https://github.com/cue-lang/cue/releases/tag/v0.17.0)).
The [modules reference](https://cuelang.org/docs/reference/modules/) is the
normative spec. Key properties:

- **`cue.mod/module.cue`** declares the module path (domain-qualified, with a
  major-version suffix like `@v1`), the minimum language version, a `source`
  kind (required at publish time), and `deps` — a map of dependency paths to
  concrete versions.
- **Strict semver** with Go-style **Minimal Version Selection**: the build
  list is the minimum versions satisfying all requirements, computed
  deterministically **with no lock file**.
- **A published module is an OCI artifact**: manifest artifactType
  `application/vnd.cue.module.v1+json`; layer 0 is the module zip; layer 1 is
  a standalone copy of `module.cue` so resolvers can walk the dependency graph
  without downloading module content — cheap MVS resolution by design.
- **Any OCI registry works.** `CUE_REGISTRY` supports comma-separated
  longest-prefix routing
  (`mycorp.example=registry.internal/cue,registry.cue.works`), so private
  modules route to a private registry and everything else to the default
  ([registry config](https://cuelang.org/docs/reference/command/cue-help-registryconfig/)).
  The workflow is `cue mod init --source=git`, `cue mod tidy`, `cue mod get`,
  `cue mod publish <version>`.

For this platform the immediate consequence: **the in-cluster Quay registry
can serve as the distribution's CUE module registry today**, with no new
infrastructure. Quay organizations become module namespaces; the same authn
(Keycloak OIDC, robot accounts) and the same `quay.holos.run` Organization/
Repository CRs ([ADR-19](../adr/ADR-19.md)) that govern image repositories
govern module repositories.

### 2.2 The Central Registry: useful, not load-bearing

The Central Registry (`registry.cue.works`) is operated by **CUE Labs AG**
(announced Oct 2025), free during beta with pricing to be announced
([cue.dev](https://cue.dev/products/central-registry/)). Namespace claiming is
GitHub-anchored (`github.com/<org>/<repo>` paths follow repo ownership) with
custom domains claimed via a `.well-known/cue-central-registry.json` proof.
It auto-generates docs for published modules and tiers content as Curated /
Official / community; the curated schema library (Kubernetes, Argo CD, GitHub
Actions, …) is its flagship value today. No public module count is published —
the ecosystem is real but young.

Because the module protocol is registry-agnostic, the Central Registry is a
convenience and discovery layer, not a dependency. The distribution should
**publish its public modules under its own domain** (claimable on the Central
Registry for discoverability) while resolving them from any OCI registry —
capping the vendor-dependence risk §1's rug-pull history warns about.

### 2.3 The language properties that answer the interface requirement

- **Unification is commutative, associative, and idempotent.** Merging
  configuration fragments is order-independent: a policy constraint unified
  into a tenant's config constrains it identically no matter where in the
  import graph it lands, and conflicts surface as evaluation errors, never
  silent last-writer-wins overrides. This is the property Chef's precedence
  matrix, Helm's values merging, and ytt's overlay ordering all approximate
  procedurally — and it is precisely what makes "security team contributes
  policy, SRE team contributes thresholds, both unify into the same component"
  safe.
- **Definitions are closed contracts.** A `#Config` definition rejects fields
  it does not declare; consumers cannot silently pass unknown fields, and the
  author controls the extension surface explicitly. Combined with required
  fields, this gives module authors a typed interface/implementation split
  without a separate IDL — the typed parameter list Puppet had and Helm never
  got.
- **`@tag()` injection** is the sanctioned late-binding mechanism
  ([injection how-to](https://cuelang.org/docs/howto/inject-value-into-evaluation-using-tag-attribute/)) —
  what `scripts/publish` and `scripts/apply-projects` already use to inject
  image digests and the local-CA bundle at render time without mutating
  committed sources.

### 2.4 The gaps the distribution must fill

- **No signing/provenance integration**: `cue mod publish` has no signing
  flags and the docs describe none. Because modules are plain OCI artifacts,
  cosign signing via OCI 1.1 referrers is mechanically straightforward on top —
  Timoni proves it (first-class `--sign=cosign` /
  `--verify=cosign` — [signing docs](https://timoni.sh/cue/module/signing/)) —
  but nothing is integrated in the CUE toolchain. The distribution's publish
  tooling should sign as a matter of course.
- **No publish-time compatibility gate**: the modules design envisions
  tag-time semver-compatibility checks, but as of v0.17.0 no shipped
  `cue mod` subcommand performs schema-subsumption checks on publish. CUE's
  value lattice gives a *formal* definition of backwards compatibility (the
  new schema must subsume the old, checkable via the Go API), so the
  distribution can build the gate itself — §4.8 makes this a first-class
  recommendation, because it is also the foundation for the Debian-style
  reverse-dependency promotion gate.

### 2.5 Adjacent systems

- **Timoni** (v0.27.0, 2026-07-02) is the closest direct precedent: CUE
  modules as OCI artifacts, a published `#Config` schema as the typed
  interface, instance values unified against it at install time, split
  vendor/content OCI layers for caching, cosign integration — and its own
  server-side applier. Its unresolved tension is applier-vs-GitOps: the
  documented Flux integration explicitly loses Timoni's lifecycle features
  ([Flux GitOps guide](https://timoni.sh/gitops-flux/)). It remains a
  single-maintainer project with alpha APIs after 3+ years. Lessons to take:
  the `#Config` pattern, layer splitting, and cosign; the lesson to avoid:
  owning the apply path forfeits the GitOps ecosystem — the trade Holos
  refuses by design.
- **KCL** distributes packages to OCI registries with ArtifactHub discovery
  but has slowed (last language release Apr 2025); **Pkl** is
  deliberately registry-less (GitHub releases + URL redirector); **Nickel**'s
  package manager is still experimental; **Dhall** is quiescent. CUE + OCI is
  the most complete open configuration-distribution stack as of mid-2026.
- **OCI substrate**: the OCI 1.1 artifact spec (Feb 2024) standardized
  `artifactType`, `subject`, and the referrers API for attaching signatures/
  SBOMs/attestations; cosign v3 (2025) stores signatures as OCI 1.1 referring
  artifacts by default. Quay has announced referrers API support
  ([Red Hat blog](https://www.redhat.com/en/blog/announcing-open-container-initiativereferrers-api-quayio-step-towards-enhanced-security-and-compliance)).
  Registry GC/retention that understands referrer graphs is the remaining
  industry unevenness — a consideration for the in-cluster Quay's retention
  configuration once signing lands.

---

## 3. The Kubernetes delivery landscape

### 3.1 The rendered manifest pattern went mainstream

The pattern this platform adopted in [ADR-16](../adr/archive/ADR-16.md) — render
manifests in CI or locally, ship the final plain YAML as the immutable desired
state — is now the mainstream position. Akuity's
[rendered manifests post](https://akuity.io/blog/the-rendered-manifests-pattern)
named it; Argo CD now ships it natively as the beta **Source Hydrator**
(`spec.sourceHydrator`); Flux markets the OCI-artifact variant as **"Gitless
GitOps"**. The advantages are the ones ADR-16 banked on: the true blast radius
of a change is visible in review, what was reviewed is exactly what is
applied, and no template engine runs in the cluster. The honest costs — CI
complexity, repo bloat, and the secrets constraint (rendered trees must never
hold secret material) — are already addressed here by Holos owning the render
pipeline and the runtime-secret guardrail (`holos/docs/secret-handling.md`).

### 3.2 Argo CD, Flux, and the media-type detail

- **Argo CD v3.1 (Aug 2025)** added native OCI Application sources
  (`repoURL: oci://…`), generalizing beyond Helm charts to Kustomize and
  plain-YAML artifacts ([user guide](https://argo-cd.readthedocs.io/en/latest/user-guide/oci/)).
  Default accepted layer media types are
  `application/vnd.oci.image.layer.v1.tar+gzip` and the Helm chart type,
  extensible via `ARGOCD_REPO_SERVER_OCI_LAYER_MEDIA_TYPES`; the current docs
  expect the artifact to contain a **single layer** — a packaging constraint
  the distribution's bundle format must respect (note Timoni's two-layer
  vendor/content split, §2.5, is therefore not directly consumable by
  Argo CD). Auth is registry credentials (`argocd repo add --type oci`).
  Argo CD has **no built-in signature verification of OCI sources yet**.
- **Flux OCIRepository** is GA since 2.6 (May 2025) with `flux push artifact`,
  semver tag tracking, and **built-in cosign/notation verification**
  (`spec.verify` — failed verification blocks the fetch). Flux is roughly
  three years ahead of Argo on Git-bypass maturity; its layer media type
  (`application/vnd.cncf.flux.content.v1.tar+gzip`) **differs from Argo's
  default list** — a distribution targeting both engines should publish the
  generic OCI layer type (which this repo's ORAS workflow already does) and
  document the Flux consumption path.
- **Kargo v1.10** (6-week cadence) remains the purpose-built promoter:
  Warehouse subscriptions produce immutable Freight, Stages promote via 60+
  steps including `argocd-update` and v1.10's `argocd-wait`, and Stage
  `spec.verification` runs Argo Rollouts `AnalysisTemplate`s for
  metric-driven progressive delivery — the machinery the roadmap's
  `ci → qa → prod` chain (deferred in [ADR-21](../adr/archive/ADR-21.md)) will use.
  Kargo has no generic "arbitrary OCI artifact" subscription type; this
  platform's pattern (subscribe to the rendered-manifest artifact repo as an
  image subscription with digest strategy) is the working approach.

### 3.3 Git-bypass prior art

The issue's low-friction requirement — render locally, push OCI, apply an
Application directly, no Git — has three working precedents:

- **Flux's Gitless GitOps**: CI or a laptop runs `flux push artifact` +
  `cosign sign`; the cluster reconciles from the registry. The audit trail
  moves from Git history to registry history + artifact annotations +
  signatures; provenance becomes cryptographic rather than commit-based.
- **Timoni apply**: renders CUE and applies via server-side apply with
  inventory tracking, readiness waits, and garbage collection — proof the
  direct-apply UX can be excellent, at the cost of owning an applier.
- **Carvel kapp-controller**: `App` CRs continuously fetch imgpkg bundles and
  deploy them — an in-cluster reconciler fed by registries, no Git.

The synthesized rule: **bypass Git, never bypass the reconciler.** Direct CLI
applies drift silently; the drift-corrected path keeps an in-cluster
reconciler (Argo CD) as the applier and merely changes *where the desired
state comes from* (a registry push instead of a Git merge). Control shifts
from Git merge rights to registry push rights plus signature verification —
which is why signing (§2.4) and Kubernetes RBAC over the Application objects
are prerequisites, not niceties. This platform already implements exactly this
shape: `scripts/publish` → ORAS push → Kargo Freight → `argocd-update`
([ADR-16](../adr/archive/ADR-16.md)), and the per-project App-of-Apps roots pull
public OCI bundles anonymously. What remains for the roadmap is packaging the
flow as a first-class CLI verb for product engineers (§6, Phase 3).

### 3.4 Composition competitors

| System | Package unit | OCI story | Customization interface | Maturity (mid-2026) |
|---|---|---|---|---|
| Crossplane v2 | Configuration/Function **xpkg** | xpkg is an OCI image | XRD-generated CRD; function pipeline | CNCF Graduated; v2 GA Aug 2025 |
| kro | ResourceGraphDefinition CR | none native | RGD-generated CRD, CEL wiring | Alpha (k8s SIG project) |
| OLM v1 | bundle + catalog images | fully OCI | version/channel only — no values | GA (OpenShift 4.18) |
| Carvel | Package CRs over imgpkg bundles | imgpkg + digest lock + relocation | ytt data-values schema + **arbitrary overlays** | CNCF Sandbox; post-Broadcom momentum loss |
| Timoni | CUE module | native, cosign-signed | `#Config` typed values | pre-1.0, single maintainer |

Three details matter for the design:

- **Carvel's "typed values + structural overlay escape hatch"** is the most
  honest interface design in the YAML world: packages expose an official
  data-values schema, and a sanctioned annotation attaches arbitrary ytt
  overlays for everything the author didn't anticipate — out-of-band and
  auditable. CUE subsumes both halves in one mechanism (typed defs +
  unification), but the *design acknowledgment* — no schema anticipates every
  site — must carry over (§4.4).
- **Carvel's imgpkg relocation** (copy a bundle *and every image it
  references, by digest,* into a private registry, rewriting the lock) is the
  strongest air-gap story in the ecosystem and the model for the
  distribution's future enterprise-mirror workflow.
- **Crossplane v2 and kro** both answer the interface question by *generating
  a CRD from a schema* — consumers get a real typed Kubernetes API. That is
  the right shape for the platform's *self-service* surface (ADR-24's
  `ProjectRequest` direction), but it is a control-plane concern layered above
  the package system, not a replacement for it: both delegate the "what YAML
  does this expand to" question to an in-cluster engine, reintroducing the
  in-cluster rendering opacity the rendered-manifest pattern exists to avoid.

### 3.5 Observability and policy already ship as packages

- The **monitoring mixins** ecosystem
  ([monitoring.mixins.dev](https://monitoring.mixins.dev/)) is the proven
  "observability as a contributed library" pattern: jsonnet bundles of Grafana
  dashboards + Prometheus recording/alerting rules with `_config` overrides
  and deep-merge extension, compiled by kube-prometheus into `PrometheusRule`
  CRs and dashboard ConfigMaps. Its pain — the jsonnet toolchain and compile
  step — is exactly what CUE subsumes natively: the same composition with type
  checking and no separate bundler. The contribution *format* for a
  Kubernetes-native platform is settled CRs: `ServiceMonitor`/`PodMonitor` +
  `PrometheusRule` (prometheus-operator) and `GrafanaDashboard`/
  `GrafanaFolder`/`GrafanaDatasource` (grafana-operator v5, which can
  reference dashboards inline, by URL, by grafana.com ID, or **from OCI
  references** — [operator docs](https://grafana.github.io/grafana-operator/docs/)).
- **Policy** converged on the same shape: Kyverno policies delivered as signed
  OCI artifacts via GitOps (the
  [CNCF-documented Flux pattern](https://www.cncf.io/blog/2022/09/19/managing-kyverno-policies-as-oci-artifacts-with-ocirepository-sources/));
  OPA consumes bundles from OCI registries natively; and
  **ValidatingAdmissionPolicy (CEL) went GA in Kubernetes 1.30** with
  `paramKind`/`paramRef` separating rules from tenant-specific parameters —
  policy runs in-process in the apiserver, packages as plain YAML, and CEL
  rules are string-embeddable and vet-able from CUE. This aligns exactly with
  the direction this repo already took in HOL-1421/ADR-20 Rev 7: controllers
  are transparent; naming conventions and tenancy boundaries are admission
  control's job. A security team's package in this distribution is a CUE
  module that emits VAP/Kyverno resources plus CUE-level constraints (§4.4).

### 3.6 Multi-tenancy primitives

`ResourceQuota` and `LimitRange` remain the in-tree floor. **Kueue**
(v0.17–0.18; multi-cluster dispatching via MultiKueue is still beta, with
MultiKueue UX a headline 2026 roadmap item) is the modern answer for batch/AI
quota:
`ClusterQueue` quota pools over `ResourceFlavor`s, namespace-scoped
`LocalQueue`s, cohort borrowing, priority preemption, and native integrations
for Job, JobSet, and the KubeRay CRs — quota enforced at *admission* (the
whole workload waits until it fits, then admits atomically — gang scheduling
by admission) rather than at pod creation, avoiding the partial-scheduling
deadlocks ResourceQuota causes for gang workloads. The Hierarchical Namespace
Controller is **retired**; do not build on it. These feed the roadmap's
Phase 4 (§6).

---

## 4. Design recommendations

The platform's existing architecture already implements several load-bearing
pieces: rendered manifests via Holos ([ADR-16](../adr/archive/ADR-16.md)), everything
modeled as Kubernetes resources ([ADR-2](../adr/ADR-2.md)), transparent
generic controllers with policy pushed to admission control
([ADR-20](../adr/ADR-20.md) Rev 7, HOL-1476), Gateway-API-style status on
every CR ([ADR-22](../adr/ADR-22.md)), and collection-driven Project/
Application components ([ADR-21](../adr/archive/ADR-21.md)). The recommendations
below extend that base into a package ecosystem.

### 4.1 The package unit: a CUE module exporting schema, component, and mixins

A **distribution package** is a CUE module, published as an OCI artifact via
`cue mod publish`, that exports up to three things:

1. **A closed `#Config` definition** — the package's typed interface:
   required fields, optional fields with defaults, and constraints. This is
   Puppet's typed parameter list and Timoni's `#Config`, enforced by CUE's
   closedness.
2. **A component** — a function from `#Config` values to a Holos `BuildPlan`
   (generators → transformers → validators producing plain manifests). For
   upstream software this typically wraps the vendor Helm chart or Kustomize
   base with the Holos Helm/Kustomize generators, lifting the output into CUE
   where platform-wide constraints unify over it.
3. **Optional mixin definitions** — schemas *other* packages unify against
   (dashboard bundles, alert-threshold structs, policy constraint sets; §4.4).

Not every package carries all three: a pure schema package (e.g. vendored CRD
types), a pure policy package, and a full component package are all valid.
This mirrors the useful degenerate cases of Puppet modules (data-only modules)
and Ansible collections (plugin-only collections).

### 4.2 The transparency principle: `io.Reader` for configuration

The issue's `io.Reader`/`io.Writer` analogy sets the design bar: the interface
between a package and the distribution must be **small, structural, and
silent about policy**. `io.Reader` succeeds because it says nothing about
*what* is being read, buffered, compressed, or retried — any producer composes
with any consumer through one minimal method.

The configuration equivalent, concretely:

- **A component is a pure function: CUE values in, manifests out.** The
  `BuildPlan` contract already is this — a component consumes typed values and
  emits artifact files, nothing else. No component may read the cluster, the
  environment, or the filesystem outside its module; all variance arrives
  through `#Config` fields or `@tag()` injection. (This is also what makes the
  render hermetic and cacheable.)
- **Packages must not define enterprise concepts.** A reusable component never
  declares environments, promotion order, SOC2 control mappings, compliance
  annotations, or org-chart-shaped roles. Those are *consumer-layer* concerns
  (§4.3). The repo already learned this lesson at the controller layer —
  HOL-1421 removed the project/namespace/role-name opinions from the
  `keycloak.holos.run` reconcilers precisely because hard-coded structure in a
  generic layer conflicts with the next organization's structure; the same
  rule applies to packages. A package may *accept* arbitrary labels,
  annotations, and constraint mixins through its interface; it may not
  *impose* them.
- **The escape hatch is structural, not procedural.** Because a component's
  output is CUE values before it is YAML, a consumer can unify additional
  constraints over any resource the component emits (add a label to every
  Deployment, tighten a securityContext, pin an image registry) without the
  package pre-declaring a knob — Carvel's ytt-overlay honesty (§3.4), but
  order-independent and type-checked. The distribution should bless this as
  the sanctioned escape hatch and document its boundary: unifying *additional
  constraints* is supported; *replacing* package internals means you fork the
  package.

This is the property that lets two enterprises with incompatible SOC2
programs consume the same `postgres` package: each unifies its own control
set over the component's output; neither appears in the package.

### 4.3 Roles & profiles, translated: the three-layer composition model

Adopt Puppet's named layers, in CUE, as the distribution's documented
architecture:

| Puppet | Holos distribution | Owner | Content |
|---|---|---|---|
| Component module | **Distribution package** (§4.1) | Package author (community/vendor/platform team) | `#Config` + BuildPlan component + mixin schemas; no site opinions |
| Profile | **Platform profile module** | Each adopting organization's platform team | Selects packages, sets their `#Config` values, unifies policy/observability mixins, defines *this org's* environments and SOC2 controls |
| Role / node data | **Platform spec + site data** | Platform team / project owners | The Holos Platform (which components on which clusters), per-project registrations (`holos/projects/`, `holos/apps/`), tag-injected site values |

The `holos/` tree in this repo is, in these terms, one organization's profile
layer plus platform spec — with the packages currently inlined as local
components under `holos/components/`. The packaging effort (§6, Phase 2) is
the extraction of the reusable halves of those components into published
modules, leaving this repo's profile layer as the reference consumer.

Rules carried over from the prior art, made binding:

- **Packages are consumed only through profiles** (component classes are
  "always declared via a profile"): the platform spec references profile
  modules, never distribution packages directly. This keeps every site
  opinion greppable in one layer.
- **Environments are a profile-layer concept.** The distribution defines *no*
  environment names; this repo's `ci-/qa-/prod-` prefixes stay in
  `holos/namespaces.cue` (profile layer), and another adopter with
  `dev/stage/prod-eu/prod-us` composes the same packages unchanged.
- **Site truth is structurally protected**: profile-layer values and
  tag-injected data live in the consumer's repo, never inside package
  modules — the conffile lesson. A package upgrade can tighten or extend its
  schema (surfacing as render-time errors to fix consciously) but can never
  silently overwrite a site decision, because unification has no overwrite.

### 4.4 Policy and observability as mixin packages

The SRE-team and security-team requirements resolve into the same mechanism:
**a mixin package exports CUE constraints and/or CRs that unify into other
packages' output.**

- **SRE dashboards and alert thresholds.** A package like
  `…/mixins/postgres-observability` emits `GrafanaDashboard`,
  `ServiceMonitor`, and `PrometheusRule` resources (the settled CR formats,
  §3.5) *and* exports a `#Thresholds` definition (e.g.
  `connectionSaturationWarn: *0.8 | number`) that consumers override in their
  profile. This is the monitoring-mixins model with CUE replacing jsonnet:
  same config/library separation, but the threshold override is type-checked
  and the "compile step" is the render the platform already runs. The
  workflow-platform survey (§6, Phase 4) found that none of KubeRay, Temporal,
  Prefect, or Kueue ships production alert rules and only fragmentary official
  dashboards — packaged golden observability is genuinely differentiated
  distribution value, not a repackaging exercise.
- **Security policy.** A security-team package emits admission resources
  (ValidatingAdmissionPolicy with `paramKind`-separated parameters, and/or
  Kyverno policies) for runtime enforcement, plus CUE constraints for
  render-time enforcement — the same rule expressed at both gates. Render-time
  unification catches violations in the PR diff before anything ships;
  admission catches whatever arrives by other paths. The division of labor
  established in ADR-20 Rev 7 (transparent controllers, policy in admission)
  extends naturally: **policy packages are how admission policy gets
  authored, versioned, and delivered.**
- **Composition semantics do the heavy lifting.** Because unification is
  order-independent and conflicts are errors, a profile that unifies the
  security package's "no `:latest` tags" constraint with a component that
  emits one fails the *render*, with a CUE error naming both sources — not a
  runtime surprise, not a silent override. This is the mechanical realization
  of "product engineers publish components, enabling them to mix in the
  policy" from the issue.

### 4.5 OCI distribution conventions

- **Two artifact kinds, one registry.** *CUE modules* (source packages:
  schemas, components, mixins) are published with `cue mod publish` —
  artifactType `application/vnd.cue.module.v1+json` — and consumed at render
  time via MVS. *Rendered-manifest bundles* (deploy artifacts: the existing
  `holos-substrate-config`, per-project `<project>-config`, and per-app artifacts)
  are pushed with ORAS using the generic
  `application/vnd.oci.image.layer.v1.tar+gzip` layer type Argo CD accepts by
  default, exactly as `scripts/publish`/`scripts/publish-config` do today.
  Source packages use immutable semver tags; deploy bundles keep the
  established digest-addressed tags plus mutable channel tags (`:dev`).
- **Namespaces are Quay organizations,** provisioned declaratively by the
  existing `quay.holos.run` Organization/Repository CRs with team access
  synced from Keycloak groups (ADR-19) — i.e., the registry-governance layer
  that Puppet's Forge and Ansible's Galaxy had to build bespoke falls out of
  infrastructure this platform already runs, governed by the Kubernetes RBAC
  model as required.
- **Sign at publish; verify at the gate.** The distribution's publish tooling
  wraps `cue mod publish` and `oras push` with cosign signing (OCI 1.1
  referrers, keyless in CI). Verification points must be chosen honestly per
  artifact kind: the promotion pipeline (§4.8) verifies signatures before
  promoting a package to a channel — on the Argo CD path this pipeline gate is
  the *only* config-bundle verification point today, because Argo CD has no
  OCI-source signature verification yet (watched upstream) and admission-time
  tooling does not cover config bundles (ValidatingAdmissionPolicy is
  in-process CEL and cannot fetch referrers or run cosign; Kyverno's
  `verifyImages` checks container image references in workloads, not an
  `Application`'s OCI source). Container *images* referenced by rendered
  workloads do get admission-time verification via Kyverno; config bundles get
  publish/promotion-time verification, or Flux's built-in `spec.verify` when
  the Flux consumption path is used. This fills CUE's signing gap (§2.4) with
  the Timoni-proven pattern while stating plainly where each gate applies.

### 4.6 Custom controllers: what to build (and what not to)

The controller work splits into a small set of genuinely new reconcilers plus
disciplined reuse of what exists. Guiding rule, from HOL-1476 and ADR-20
Rev 7: **low-level controllers stay generic building blocks; distribution
structure lives in packages and admission policy.**

1. **Registry/tenancy controllers — already shipped.** The
   `quay.holos.run` Organization/Repository and `keycloak.holos.run`
   reconcilers (ADR-19/ADR-20) already provision package namespaces,
   publisher credentials-by-team, and OIDC-synced access. Publishing a new
   package namespace is a one-line addition to a collection rendered into an
   Organization CR — no new controller.
2. **A `catalog.holos.run` group for the distribution index** (new, small).
   Two Kinds:
   - **`Package`** — a claim/registration of a package path (its OCI
     repository, ownership team, tier per §5, source URL). Reconciler duties:
     verify the referenced repository exists, watch published versions and
     surface them in `status`, and expose the standard `Accepted`/
     `Programmed`/`Ready` conditions plus drift-observability timestamps per
     the ADR-22 guardrails.
   - **`PackageChannel`** — the promotion state (§4.8): which version of a
     package each channel (`unstable`/`testing`/`stable`) points at, moved by
     the promotion pipeline, readable by every consumer including the web
     interface. Both Kinds are pure Kubernetes API objects — which *is* the
     future-web-interface story (§4.7).
3. **A render service, eventually, for self-service and the web path**
   (deferred, consciously). Today rendering is client-side by design
   (ADR-16's decisive constraint was that OSS Kargo cannot host the Holos
   render step). A future server-side render service — a Job
   template or a small service that renders a profile at a pinned module set
   and pushes the bundle — is the enabling piece for both the ProjectRequest
   self-service arc (ADR-24) and a web UI's "preview this change" affordance.
   It must remain exactly the CLI's render, containerized: same inputs, same
   hermetic function, no in-cluster template divergence.
4. **Per-use-case service controllers where the upstream lacks a declarative
   surface** (Phase 4): the Temporal survey shows the platform needs a
   namespace-provisioning CR (`TemporalNamespace`-shaped — the community
   operator is single-maintainer and slow-moving, so a small
   `temporal.holos.run` reconciler following the established holos-controller
   conventions is the safer path). KubeRay and Kueue need **no** new
   controllers — their CRs are already the self-service surface; the
   distribution's work there is packages, quotas, and dashboards.
5. **What not to build:** no in-cluster package-expansion engine (the
   Crossplane/kro/OLM shape) — it reintroduces runtime rendering opacity; no
   bespoke applier (the Timoni trap) — Argo CD stays the reconciler; no
   HNC-style namespace hierarchy (retired upstream).

### 4.7 Git-bypass and the web interface are the same design

Both requirements reduce to: **the Kubernetes API is the only write surface,
and the registry is the only artifact channel.**

- The low-friction CLI flow already exists in pieces:
  `scripts/publish` renders and pushes a bundle; the App-of-Apps roots are
  OCI-source Applications; Kargo moves `targetRevision`. Packaged as a single
  CLI verb (a future `deploy` command: render profile → ORAS push →
  server-side-apply one Application), a product engineer deploys without
  Git — authenticated by their kubeconfig, authorized by the same RBAC that
  governs every other write to their project namespaces, with the Holos
  Authenticator (ADR-23) providing OIDC → impersonation for humans and remote
  clusters. Nothing about the flow is imperative *state*: the Application
  lands in the project's control plane and Argo CD reconciles it, drift
  correction included ("bypass Git, never bypass the reconciler", §3.3).
- **The direct path must not bypass the trust gate.** Because Argo CD cannot
  yet verify OCI-source signatures and admission cannot cosign-check config
  bundles (§4.5), a naive render→push→apply flow would let any identity with
  registry push rights feed Argo CD an unverified bundle. The design therefore
  requires one of two enforced arrangements, and the distribution should ship
  the first: **(a) verified-by-construction repositories** — direct pushes
  land in a per-project *staging* repository that Argo CD never sources; a
  verifier (a promotion-pipeline step or a small `catalog.holos.run`-adjacent
  reconciler) cosign-verifies the artifact and copies/retags it by digest into
  the Argo-consumable repository, so every repository named in an AppProject's
  `sourceRepos` contains only verified content, with admission (AppProject
  `sourceRepos` pinning plus CEL over the Application spec requiring
  digest-pinned `targetRevision`) making the arrangement mandatory rather than
  conventional; or **(b) scope enforced verification to the Flux path** —
  where a consumer needs source-time signature enforcement *today* with no
  verifier hop, use Flux `OCIRepository.spec.verify` (§3.2), and treat the
  Argo direct path as blocked until Argo CD grows OCI-source verification
  (watched upstream). The future `deploy` verb implements (a): it signs on
  push, waits for the verifier's copy, and applies an Application pinned to
  the verified digest — one extra hop, no unverified bundle ever reconciled.
- The audit posture: registry history + cosign signatures + Kubernetes audit
  logs of the Application apply replace the PR trail. Signed publishes (§4.5)
  give the direct path artifact-level provenance comparable to the Git path,
  with the caveat §4.5 states: config-bundle signatures are verified at
  publish/promotion time (or by Flux's `spec.verify` on that path), not at
  admission — what admission can enforce today is *which registries and
  repositories* an `Application` may source from (AppProject `sourceRepos`
  plus CEL over the Application spec) and Kyverno image verification for the
  workloads the bundle deploys. Git review remains the default for
  shared/production surfaces as a policy choice enforced where policy lives
  (admission + AppProject restrictions), not as a mechanical limitation.
- A web interface then requires **no new backend API**: it is a Kubernetes
  API client (through the Authenticator's impersonation path) reading
  `catalog.holos.run` Packages/Channels, project CRs and their rich ADR-22
  status conditions, and writing the same CRs the CLI writes. The
  Package/PackageChannel status fields double as the catalog browse surface —
  the reason §4.6 puts the index in CRs rather than a database.

### 4.8 The composition gate: lintian and britney, for packages

The Debian translation, concretely, as the distribution's CI/promotion
pipeline:

1. **Policy manual** — a versioned document defining what a conformant
   package is: module layout, `#Config` conventions, no-site-opinion rules
   (§4.2), status/label conventions for emitted resources, mixin contract
   shapes. Start as an extension of `holos/docs/component-guidelines.md`.
2. **`lint` (lintian)** — machine-checkable conformance: `cue vet` against
   distribution schemas, closedness checks, forbidden-pattern checks (inline
   secrets, `:latest` references, undeclared environment concepts), docs
   presence. Runs at publish; failures block.
3. **Compatibility gate** — on every publish of `vN`, check the exported
   definitions subsume `vN-1`'s (CUE's lattice makes backwards compatibility
   formally checkable via the Go API's subsumption — the gate CUE's own
   toolchain hasn't shipped, §2.4). Incompatible schema change without a
   major-version bump: rejected.
4. **`britney` (the promotion gate)** — packages publish to the `unstable`
   channel freely (post-lint). Promotion to `testing` requires: aging without
   regressions, and — the Debian idea no configuration ecosystem has
   implemented — **re-rendering every distribution profile and reference
   consumer that depends on the package at the candidate version, and
   requiring their renders (and validators) to pass**. Reverse-dependency
   render checks are cheap (hermetic, parallelizable, no cluster needed) —
   the rendered-manifest pattern makes Debian's expensive promise
   *computationally trivial*. `stable` promotion adds human release
   management. The `PackageChannel` CR records the result; consumers pin
   channels or exact versions.

This pipeline is also where signatures are produced and verified (§4.5), and
its per-package results feed the `Package` CR status — visible in `kubectl`
and the future web catalog alike.

### 4.9 Maintainability mechanics

- **modulesync, from day one.** One shared CI/config template repo stamps
  lint, test, publish, and compat-gate workflows across all package repos;
  fleet-wide toolchain upgrades are one PR. Vox Pupuli (~200 modules) and
  Sous Chefs both proved this is what makes collective maintenance scale.
- **Reference-consumer tests as the package's test suite.** Each package
  carries example configs rendered in CI (its "unit tests"); the distribution
  repo's profiles are the integration suite via the reverse-dependency gate.
- **Deprecation policy in the policy manual**: how a package signals
  deprecation (a `Package` CR field + render-time warning), the migration
  window, and the adoption process for orphaned packages (§5).
- **The contributing team's repository layout** — the package monorepo with
  one module per package, golden-render examples as the test suite,
  per-module release tags, and the extraction path to collective-owned repos —
  is proposed concretely in
  [Team Repository Layout for Integrating Software into the Distribution](distribution-package-repo-layout.md).

---

## 5. The community model

The target — "a community like Vox Pupuli and Debian" — is an institution
design problem, and the survey's clearest finding is that the institution
matters more than the tooling: Helm lost its central catalog twice for lack of
one; Puppet's collective absorbed a platform fork. The proposal:

1. **A collective-owned namespace from the start.** One GitHub organization
   and one registry namespace for community packages, owned by the collective,
   with no package owned by an individual. Maintainer teams per package
   domain (databases, observability, workflow, security), Debian-style.
2. **Two-tier trust, one format** (the Galaxy/Automation Hub and Forge
   Approved/Supported pattern): **core** packages (the distribution's own,
   promotion-gated, release-managed — the Debian `main` analog) and
   **community** packages (lint-gated, collective-maintained). A `verified`
   flag on the `Package` CR records tier; the web catalog surfaces it. Vendors
   can publish under their own claimed namespaces (the Ansible vendor-collection
   lesson: make vendors first-class maintainers of their own content rather
   than routing everything through one team).
3. **Written processes for the whole package lifecycle** before scale forces
   them: namespace claiming (human-reviewed, the Galaxy lesson), package
   inclusion requirements (the Ansible community-package lesson), abandoned-
   package adoption (the Vox Pupuli migration-process lesson), and NMU-style
   emergency fixes by the collective.
4. **Policy before tooling, tooling before mandate** (the Debian sequence):
   write each policy-manual rule down, make it machine-checkable in the lint
   gate, *then* make it blocking.
5. **Neutrality as a structural property.** The registry protocol is
   OCI-standard and self-hostable; the packages are Apache-2.0; the promotion
   pipeline and CI templates live in the collective's org. The Bitnami test:
   if any single vendor — including the platform's own steward — withdrew
   tomorrow, the collective must retain the namespace, the CI, and the
   artifacts. Every vendor-rug-pull in §1 was survivable exactly in
   proportion to how true this already was.

---

## 6. Roadmap

Each phase produces something usable on its own; later phases depend on
earlier ones. Phases 0–1 harden what exists; 2–3 build the package system;
4 delivers the product-engineering use cases the issue names; 5 opens the
community distribution.

### Phase 0 — the minimal distribution, hardened (now)

The floor named in the issue is what already runs: **Keycloak** (identity,
OIDC for every service), **Quay** (images, deploy bundles, and — new insight
from §2 — the CUE module registry), **Istio ambient** (mesh),
**Holos Authenticator** (OIDC → impersonation; the human and remote-cluster
path to the apiserver), **Holos Controller** (quay/keycloak/security API
groups), **Argo CD** (the reconciler), **Kargo** (promotion). Phase-0 work is
consolidation, not construction:

- Finish drift-observability/status retrofits (ADR-22 guardrails) so every CR
  an operator or future web UI reads is legible.
- Add the generic Argo CD Lua health check for `holos.run` CRs keyed on the
  `Ready` condition (§3.2's one-generic-check observation).
- Land the admission-policy baseline (VAP) that took over the invariants the
  controllers shed in HOL-1421 — it is also the enforcement point every later
  policy package plugs into.

### Phase 1 — observability and quota baseline (platform-side prerequisites)

Before packages can carry dashboards and thresholds, the platform must run
the operators that give those CRs meaning:

- **kube-prometheus-stack** (prometheus-operator CRs as the scrape/alert
  contract) and **grafana-operator v5** (GrafanaDashboard/Datasource/Folder
  CRs as the dashboard contract), as ordinary Holos components delivered by
  the platform App-of-Apps.
- **Golden platform dashboards/alerts as the first mixin packages** (§4.4):
  Argo CD, Kargo, Quay, Keycloak, Istio, and the holos-controller itself —
  authored in CUE, emitted as CRs, thresholds exported as overridable
  definitions. This exercises the mixin pattern on the platform's own
  components before asking contributors to use it.
- **Namespace quota baseline**: `ResourceQuota` + `LimitRange` rendered into
  the Project component's per-environment namespaces with profile-layer
  defaults and per-project overrides — the simple floor; Kueue arrives in
  Phase 4 where gang workloads make ResourceQuota actively wrong.

### Phase 2 — the package system

- Split the CUE tree into **published modules**: distribution schemas
  (`#Config` conventions, mixin contracts), extracted reusable components
  (the §4.3 extraction — packages out of `holos/components/`, this repo's
  profile layer as reference consumer), and the platform profile. Publish to
  Quay via `cue mod publish`; wire `CUE_REGISTRY` prefix routing.
- Ship the **publish tooling**: sign-on-publish (cosign), the lint gate, and
  the compat gate (§4.8 items 1–3), with the policy manual's first version.
- Add the **`catalog.holos.run`** Package/PackageChannel CRDs and reconciler
  (§4.6), following every established controller guardrail (conditions,
  drift timestamps, ReferenceGrant for any cross-namespace reference).

### Phase 3 — delivery ergonomics

- **The Git-bypass CLI verb** (§4.7): a future `deploy` command — render, push
  (signed), wait for the verifier's copy into the Argo-consumable repository,
  apply the Application pinned to the verified digest — the
  docker-push-to-deploy experience generalized to configuration, without
  bypassing the trust gate. Fisk command per the CLI guardrails; ships with
  the verifier hop and the AppProject/CEL admission pinning from §4.7.
- **The promotion pipeline** (§4.8 item 4): channel tags, reverse-dependency
  render gates, `PackageChannel` reconciliation; wire the existing
  `ci → qa → prod` deferred work (ADR-21) to Kargo Stages with
  AnalysisTemplate verification so app promotion and package promotion share
  one mental model.
- **Web catalog read-path**: nothing to build server-side beyond Phase 2's
  CRs — validate that a UI prototype can browse packages/channels/status
  purely through the Authenticator-fronted Kubernetes API.

### Phase 4 — job scheduling and workflow use cases

The product-engineering payload, informed by the §3.6 survey and the
workflow-platform findings. The platform-vs-self-serve division for each:

- **Kueue first** — it is the shared capacity-governance layer everything else
  plugs into: platform owns ClusterQueues/ResourceFlavors/cohorts per team
  (GPU pools especially) and the per-namespace LocalQueues rendered by the
  Project component; quota moves from "namespace caps" to "admission with
  borrowing, priority, and preemption." Dashboards for queue depth/quota
  usage ship as the Kueue mixin package (upstream ships none).
- **KubeRay** (the AI-compute engine): platform runs the operator, GPU node
  pools, and the observability mixin (vendoring KubeRay's in-repo Grafana
  JSON into GrafanaDashboard CRs, PodMonitors per the documented pattern,
  DIY alerts — upstream has none); product engineers self-serve `RayJob`/
  `RayService` CRs labeled into their LocalQueue — gang-scheduled,
  quota-governed, no new controllers needed. Golden-path CR templates ship as
  a distribution package.
- **Temporal** (durable execution): platform runs the server (official Helm
  chart wrapped as a component; CloudNativePG for persistence), wires
  Keycloak OIDC claim-mapping at the frontend, and adds the small
  `temporal.holos.run` namespace-provisioning reconciler (§4.6 — namespaces
  are Temporal's tenancy unit and need a declarative surface; the community
  operator is a single-maintainer risk). Product engineers ship workers as
  ordinary app Deployments through the existing Application component,
  autoscaled by KEDA's Temporal scaler; per-team namespace CRs render from
  the Project component. Dashboards: the community server dashboard
  (grafana.com ID 20528) referenced declaratively via grafana-operator, plus
  SDK-metric dashboards in the mixin.
- **Prefect** (Python-ergonomics orchestration, optional tier): self-hosted
  Prefect OSS has no multi-user auth/RBAC/workspaces — the honest model is
  per-team server instances behind the platform's SSO proxy, packaged so a
  team requests "a Prefect" as one profile-layer line; hardened work-pool
  base job templates are the golden-path contract (schema-validated
  variables, no raw K8s YAML for engineers). Flow-run Jobs inherit namespace
  quota/Kueue admission like any Job.
- **DRA posture**: device-plugin-first through 2026, DRA (GA in k8s 1.34) as
  the forward path for MIG/fractional-GPU governance; revisit when the NVIDIA
  DRA driver and Kueue's DRA support mature.

Each of these lands as distribution packages (component + mixins + golden-path
templates) plus, for Temporal only, one small controller — the package system
carrying the use cases is the point of the phases being ordered this way.

### Phase 5 — the community distribution

- Stand up the collective org + namespace, the two-tier catalog, and the
  written lifecycle processes (§5) — seeded with the Phase 1–4 packages and
  at least one external maintainer team to force the processes to be real.
- Publish the policy manual and open the promotion pipeline to community
  packages (`unstable` self-serve post-lint; `testing`/`stable` gated).
- Success criterion, borrowed from the survey: a package whose author
  disappears is adopted, not abandoned; a component the platform team never
  wrote (a dashboard pack, a policy set, a KubeRay golden path) ships to
  `stable` through the gates without platform-team hand-holding.

## Sources

Primary sources are cited inline throughout. The heaviest-weight references:
the [Puppet roles and profiles method](https://www.puppet.com/docs/puppet/7/the_roles_and_profiles_method.html),
[Vox Pupuli](https://voxpupuli.org/) and its
[module migration process](https://voxpupuli.org/docs/migrate_module/),
the [Debian Policy Manual](https://www.debian.org/doc/debian-policy/) and the
[autopkgtest migration gate](https://lists.debian.org/debian-devel-announce/2018/05/msg00001.html),
the [CUE modules reference](https://cuelang.org/docs/reference/modules/),
[Timoni](https://timoni.sh/), the
[rendered manifests pattern](https://akuity.io/blog/the-rendered-manifests-pattern),
the [Argo CD OCI user guide](https://argo-cd.readthedocs.io/en/latest/user-guide/oci/),
[Flux v2.6 "Gitless GitOps"](https://fluxcd.io/blog/2025/05/flux-v2.6.0/),
the [Kargo docs](https://docs.kargo.io/),
[monitoring mixins](https://monitoring.mixins.dev/),
[Kueue](https://kueue.sigs.k8s.io/docs/overview/), and the repository's own
[ADR-16](../adr/archive/ADR-16.md), [ADR-18](../adr/ADR-18.md) through
[ADR-24](../adr/ADR-24.md).
