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

## Guard Rails

### CUE Component Rendering
- **Rule:** All changes to files under `holos/components/` MUST be followed by running `scripts/render` to regenerate the corresponding manifests under `holos/deploy/`.
- **Why:** The render script enforces that the committed deploy tree matches the CUE source exactly. Drift between source and deployed manifests can mask outdated or broken configurations.
- **How to apply:** After editing any `.cue` file:
  1. Commit the CUE changes
  2. Run `scripts/render` (it will fail if holos/ has uncommitted changes)
  3. Commit the regenerated YAML in `holos/deploy/` together with the source changes
  - See `holos/docs/component-guidelines.md` for full workflow details.

### Known Issues & Workarounds

#### Quay OIDC PKCE — disabled (HOL-1233, resolved by HOL-1257)
- **Issue:** Quay's OIDC client does not fully implement PKCE — it fails to send the code_verifier during token exchange, producing "code_verifier_missing" / `Got non-2XX response for code exchange: 400` SSO failures.
- **Resolution (HOL-1257):** PKCE is **disabled** for the `quay` client on both ends. The Keycloak `quay` client carries **no** `pkce.code.challenge.method` attribute (Keycloak treats a client that sets it as *requiring* PKCE), and Quay's `KEYCLOAK_LOGIN_CONFIG` no longer sets `USE_PKCE`/`PKCE_METHOD`. Quay authenticates as a plain confidential client with its client secret — Red Hat's recommended baseline integration.
- **History:** An earlier workaround kept PKCE *optional* (advertising `pkce.code.challenge.method: "S256"` while relying on Keycloak's default `pkce.force: "false"`). HOL-1257 superseded that by removing the attribute entirely, which is the current state.
- **Status:** Resolved. If a future Quay release fully implements PKCE, consider re-enabling it by restoring the `pkce.code.challenge.method: "S256"` attribute on the client and `USE_PKCE`/`PKCE_METHOD` in Quay's config.
- **Related:** `holos/components/keycloak/realm-config/buildplan.cue` (the `quay` client — no `attributes`), `holos/components/quay/buildplan.cue` (the `KEYCLOAK_LOGIN_CONFIG` block), `docs/adr/ADR-15.md` (Revision 2), `holos/docs/keycloak-clients.md` (the PKCE guardrail checklist), and `docs/runbooks/quay-keycloak-oidc.md` (the operational runbook, including the no-PKCE exception and the `code exchange: 400` troubleshooting).

### Keycloak Configuration as Code
- **Pattern:** The holos realm (users, groups, clients, roles, protocol mappers) is fully declarative, reconciled on every `scripts/apply` via a keycloak-config-cli Job.
- **Scope:** The Job imports only `realm: "holos"` — it does NOT manage `enabled` or `identity-provider` fields, which are owned by the KeycloakRealmImport CR in the instance component. This prevents contention between the two reconciliation paths.
- **Generate-once guarantee:** Secrets generated at runtime (e.g., Quay OIDC client secret) are created once and never rotated, so they remain stable across reconciles. Bootstrap Jobs idempotently check for existing secrets before creating.

### Runtime Secret Handling (binding guardrail)
- **Rule:** A Secret's *material* is created at runtime — via an `ExternalSecret` or a create-if-absent bootstrap `Job` — and never committed to the repo. The deploy tree under `holos/deploy/` never holds a Secret's material. (Chart-owned empty Secrets a controller populates in place — e.g. `kargo-api`, `argocd-secret` — are fine; what's forbidden is committing the value, or an empty-data placeholder for a bootstrap-Job-managed Secret, which would defeat the Job.)
- **Resolving ambiguous AC:** When an acceptance criterion asks a `Secret` to "render"/"be committed"/appear under `holos/deploy/`, **resolve it directly by handling the Secret at runtime** — do NOT mark the issue `needs-human-review` or leave it open for a human to confirm. The tradeoff is already decided by this guardrail.
- **Key naming:** Write only the key(s) the consumer actually reads (verified against its contract, e.g. the vendored CRD). Never carry an extra unread key "for AC compliance" — it is dead code. If an AC names a wrong key, use the correct one and drop the named one.
- **Reference:** `holos/docs/secret-handling.md` (the full guardrail, indexed below).

### OIDC Client Secrets
- **Rule:** OIDC client secrets are generated at runtime, never committed. (A specific case of *Runtime Secret Handling* above.)
- **Pattern:** A bootstrap Job generates the secret once and writes it to both the owning component's namespace and any consuming namespace (e.g., keycloak and quay for the Quay OIDC secret).
- **Reference:** `holos/components/keycloak/realm-config/buildplan.cue`, QUAY_OIDC_BOOTSTRAP section

### Project Delivery Scaffold (my-project pattern)
- **Pattern:** A project that receives Kargo-driven OCI delivery is laid down as a **single component** that emits, together, a hand-authored Argo CD AppProject + OCI-source Application (with `kargo.akuity.io/authorized-stage` and `targetRevision` omitted so Kargo owns it), the Kargo Project/ProjectConfig/Warehouse/Stage, and a Quay org/repo/webhook/pull-robot **bootstrap Job**. The project Namespace is **not** emitted by the component — it is registered centrally in `holos/namespaces.cue` (never inline, per the component guidelines) and referenced by name. The Kargo Project namespace doubles as the workload namespace (no separate `kargo-project-*` sibling — that split is only the echo spike). `my-project` is the reference instance and the template for a future self-service `ProjectRequest`.
- **Quay org/repo/webhook bootstrap Job convention:** Model it on `holos/components/quay/buildplan.cue`'s `BOOTSTRAP_JOB`/`BOOTSTRAP_SCRIPT` and `scripts/quay-init`. Run it in the **`quay` namespace** because the admin OAuth token it authenticates with lives there (`quay-initial-admin` Secret, key `token`); it talks to Quay over the plain-HTTP in-cluster Service (`http://quay.quay.svc.cluster.local:8080`), so it needs **no** local-CA trust (the `quay-local-ca` cert is only for callers using the public `https://quay.holos.localhost` hostname). Make every step idempotent (check-then-create), write the Argo CD pull-credential repository Secret into `argocd`, and register a `repo_push` webhook pointing at the Kargo receiver URL read from `ProjectConfig.status`. Order the component **last** in `scripts/apply` (after `quay`, `argocd`, `kargo`, and any Kargo pipeline), with a `pre_*` Job-delete hook and a `wait_*` completion gate (the `pre_keycloak_config`/`wait_keycloak_config` precedent). The Application stays `Unknown`/`Missing` until the first config artifact is published — expected scaffolding.
- **Hand-authored Application vs. the deferred projection:** The sample Applications (`echo`, `my-project`) are hand-authored **OCI**-source Applications, distinct from the deferred per-component `argoAppDisabled` **git**-source projection (`holos/docs/placeholders.md` → *ArgoCD gitops delivery*). Do not conflate them.
- **Reference:** `holos/components/my-project/buildplan.cue`, `holos/README.md` (*The `my-project` delivery scaffold*), `holos/docs/oci-publish-workflow.md` (*Downstream: the `my-project` delivery scaffold*), `docs/adr/ADR-16.md`.

### Adding a Keycloak OIDC (PKCE) Client
- **Pattern:** The realm's OIDC clients (argocd, quay) are declared in `realm-config/buildplan.cue` and reconciled by the `keycloak-config` keycloak-config-cli Job. The conventional declarative-client pattern — public vs confidential decision, the `S256` attribute, the confidential secret-bootstrap Job, `IMPORT_VARSUBSTITUTION_ENABLED`, the three mappers that feed the shared `groups` claim, the role model, and the render-then-commit workflow — is documented as a guardrail checklist.
- **Before adding another PKCE client:** Read `holos/docs/keycloak-clients.md` and follow its guardrail checklist rather than rediscovering the pattern. Relax or skip requiring PKCE only for a client with a demonstrated implementation gap — the `quay` client is the documented exception (HOL-1257 disabled PKCE for it entirely; see the *Quay OIDC PKCE* note above, the runbook `docs/runbooks/quay-keycloak-oidc.md`, and `docs/adr/ADR-15.md`). The public `argocd` and `kargo` clients keep `pkce.code.challenge.method: "S256"`.
- **Reference:** `holos/docs/keycloak-clients.md`, `docs/runbooks/quay-keycloak-oidc.md`

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
