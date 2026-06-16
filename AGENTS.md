# holos-paas

The Holos PaaS: a Kubernetes-native platform delivering a minimum viable
Heroku experience — push a tagged image, get a deploy — managed entirely
through the Kubernetes API and rendered with the
[Holos](https://holos.run/) rendered-manifests pattern.

## Repository layout

The authoritative layout is defined in
[ADR-12 — Repository Layout for Multiple Go Services](docs/adr/ADR-12.md):
a single-module Go monorepo with one multi-service binary (`cmd/holos-paas`,
one subcommand per service), kubebuilder multi-group API conventions
(`api/<group>/<version>`), all implementation under `internal/`, and the
Holos CUE deployment configuration and policy under `holos/`. Read ADR-12
before adding a service, an API group, or moving directories. The evidence
behind the layout is in
[Research: Repository Layouts for Multiple Go Services](docs/research/go-multi-service-repo-layout.md).

Deployment is owned by **Kargo plus the client-side build-and-publish
workflow** ([ADR-16](docs/adr/ADR-16.md)): `scripts/publish` (`make publish`)
renders the platform with an injected app image digest, packages the rendered
manifests with Kustomize, and `oras push`es the OCI artifact to the in-cluster
Quay registry; a Kargo `Warehouse` watches that repository, creates `Freight`,
and a `Stage` promotion runs `argocd-update` to point the Argo CD `Application`
at the new digest. See
[holos/docs/oci-publish-workflow.md](holos/docs/oci-publish-workflow.md) and
[holos/docs/argocd-application-source.md](holos/docs/argocd-application-source.md).

```text
cmd/holos-paas/            # the multi-service binary (Fisk root command, ADR-17)
internal/cli/              # the Fisk command tree (one register* func per command)
internal/                  # all implementation
Makefile                   # go fmt/vet/test and the container image targets
Dockerfile                 # two-stage cross-compile → distroless runtime
holos/                     # Holos CUE deployment configuration and policy
```

The earlier NATS event-driven deployment pipeline — the **webhook receiver**
([ADR-9](docs/adr/ADR-9.md)), the **webhook subscriber**
([ADR-10](docs/adr/ADR-10.md)), and the deployer/render-task path
([ADR-11](docs/adr/ADR-11.md), [ADR-14](docs/adr/ADR-14.md)) — was retired in
HOL-1241. Those ADRs are now `Deprecated` and superseded by ADR-16; the
receiver/subscriber subcommands, their `internal/` packages, the NATS pipeline
protobuf schemas, and the `nats`/`webhook-receiver`/`webhook-subscriber` Holos
components have been removed. Git history preserves them.

## Documentation index

- [docs/adr/](docs/adr/README.md) — Architecture Decision Records: the
  binding design decisions. Start with the index; follow
  [writing-adrs.md](docs/adr/writing-adrs.md) before adding or revising one.
- [docs/cli-guardrails.md](docs/cli-guardrails.md) — **binding guardrail** for
  the holos-paas CLI: every command, subcommand, and flag is added with Fisk
  (not Cobra), in `internal/cli`, following the `deploy` command as the
  template ([ADR-17](docs/adr/ADR-17.md)). Read it before touching the CLI.
- [docs/planning/holos-paas-mvp-milestones.md](docs/planning/holos-paas-mvp-milestones.md)
  — the MVP plan; mirrors the Linear *Holos PaaS* project milestones.
- [docs/research/](docs/research/) — research reports informing decisions.
- [docs/demo/](docs/demo/README.md) — demo walkthroughs.
- [docs/local-cluster.md](docs/local-cluster.md) — the quick-start guide:
  create the local k3d cluster with DNS and trusted TLS, then apply the
  platform — the Layer 0 foundation and the Layer 1 services (Postgres,
  Keycloak, Quay, Argo CD) — with `scripts/apply`.
- [holos/README.md](holos/README.md) — orientation to the Holos CUE
  directory: layout, clusters, how rendered manifests are applied (the
  apply-order rationale), and the Keycloak, Postgres, Quay, and Argo CD
  verification steps and contracts.
- [holos/docs/component-guidelines.md](holos/docs/component-guidelines.md)
  — how to add a Holos component: anatomy, guardrails, and the
  render-then-commit workflow.
- [holos/docs/secret-handling.md](holos/docs/secret-handling.md) — **binding
  guardrail**: secrets are created at runtime (an `ExternalSecret` or a
  create-if-absent bootstrap `Job`) and never committed to the repo. Read it
  before resolving any acceptance criterion about a `Secret` — it makes the
  ambiguous "render a committed Secret" AC unambiguous (resolve it at runtime
  directly; never defer to `needs-human-review`) and forbids carrying unread
  Secret keys "for AC compliance".
- [holos/docs/mesh-enrollment.md](holos/docs/mesh-enrollment.md) — the
  ambient mesh enrollment convention for platform namespaces, how to verify
  it, and the exceptions.
- [holos/docs/keycloak-clients.md](holos/docs/keycloak-clients.md) — the
  declarative Keycloak OIDC client pattern: the `keycloak-config-cli`
  reconciliation mechanism and apply-gate, public vs confidential PKCE clients
  (argocd vs quay), the runtime client-secret bootstrap, the three protocol
  mappers that feed the shared `groups` claim, the realm/client role model
  (including `platform-owner` into the quay client), the Quay-superuser
  limitation, and the guardrail checklist for adding a new PKCE client.
- [holos/docs/argocd-application-source.md](holos/docs/argocd-application-source.md)
  — the MVP Argo CD `Application` source pattern: OCI rendered-manifests
  artifacts in the in-cluster Quay registry, the repository credential
  Secret shape, and how the repo-server reaches Quay.
- [holos/docs/kargo-keycloak-oidc.md](holos/docs/kargo-keycloak-oidc.md) — the
  Kargo↔Keycloak OIDC (PKCE) integration: the public kargo client and
  groups-claim role mapping, issuer-cert trust via the local-ca cabundle, and
  the verification/maintenance runbook.
- [docs/runbooks/quay-keycloak-oidc.md](docs/runbooks/quay-keycloak-oidc.md) —
  operational runbook for the Quay↔Keycloak OIDC SSO integration: how the
  confidential `quay` client and the `quay-oidc` secret bootstrap are wired, the
  documented **no-PKCE exception** (Quay is the one relying party without PKCE,
  unlike public `argocd`/`kargo`), grant/rotate/reconcile operations, and
  troubleshooting the `code exchange: 400` login failure. Companion to
  [ADR-15](docs/adr/ADR-15.md).
- [holos/docs/oci-publish-workflow.md](holos/docs/oci-publish-workflow.md)
  — the client-side build-and-publish workflow (`scripts/publish` /
  `make publish`): render the platform with an injected app image digest,
  package the rendered manifests with Kustomize, and `oras push` the OCI
  artifact, with the deterministic input-addressed tagging convention and
  required push credentials. Replaces the deferred in-cluster render
  subscriber.
- [holos/docs/placeholders.md](holos/docs/placeholders.md) — stubs for
  out-of-MVP-scope concerns: ArgoCD gitops delivery (the `argoAppDisabled`
  flip), observability dashboards, the Gateway route-attachment policy,
  Keycloak realm reconciliation, Quay OIDC login, node-level registry
  trust for in-cluster pulls, NATS in-cluster authentication, production
  deployment area.

## Conventions

- Decisions live in ADRs; revise the existing ADR (and its revision table)
  rather than writing a new one for a refinement.
- Every platform capability is modeled as Kubernetes resources
  ([ADR-2](docs/adr/ADR-2.md)); integrations like Keycloak group membership
  and Quay self-service are CRDs with reconcilers, not imperative tools. The
  Keycloak OIDC clients (argocd, quay) are reconciled declaratively by the
  `keycloak-config` keycloak-config-cli Job; the conventional declarative-client
  pattern and the guardrails for adding another PKCE client are in
  [holos/docs/keycloak-clients.md](holos/docs/keycloak-clients.md).
- Deployment configuration and policy are CUE rendered with
  `holos render platform`; `scripts/render` renders and verifies the
  committed `holos/deploy/` tree is diff-clean.
- Go code lives in the single root module `github.com/holos-run/holos-paas`
  laid out per [ADR-12](docs/adr/ADR-12.md): the multi-service binary under
  `cmd/holos-paas/` (one subcommand per service) and all
  implementation under `internal/`. `make test` (gofmt, `go vet`, then the
  race-enabled test suite) is the entry point; the `Go` job in
  [.github/workflows/ci.yaml](.github/workflows/ci.yaml) runs it alongside
  `golangci-lint`.
- The CLI is built with **Fisk, not Cobra** ([ADR-17](docs/adr/ADR-17.md)) so
  commands, subcommands, and flags are self-documenting and legible to AI
  coding agents (`--help-llm`, `--fisk-introspect`, cheats). Add every command
  and flag with Fisk in `internal/cli`, following the `deploy` command as the
  template — see [docs/cli-guardrails.md](docs/cli-guardrails.md). Never
  reintroduce Cobra or `pflag`.
- Label and annotation keys owned by the platform configuration layer —
  aspects of the holos configuration itself, independent of site-specific
  configuration — default to the `holos.run` domain (e.g.
  `app.holos.run/component.name`). `materia.ai` keys must never appear in
  the holos configuration or Go code; the `Guardrails` job in
  [.github/workflows/ci.yaml](.github/workflows/ci.yaml) enforces this.
- Merge pull requests with a **squash merge** (`gh pr merge --squash`) —
  never a merge commit or a rebase merge — so code-review fix commits
  (e.g. `fix: address code review round 1 findings`) are squashed away.
  Clean up the squash commit message before merging: one
  conventional-commit subject and body describing the final change, with
  the review-round noise removed.
