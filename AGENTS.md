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

### No raw inline YAML/JSON in CUE — marshal it
- **Rule:** Embedded YAML or JSON config documents in a `.cue` file MUST be authored as a CUE struct and serialized with `encoding/yaml.Marshal()` or `encoding/json.Marshal()`. Never hand-write the config as a triple-quoted string with `\(...)` interpolation — indentation and types must be correct by construction, not by hand. The only sanctioned exception is shell/script heredocs (which are not YAML/JSON documents).
- **Why:** A marshalled CUE struct is type-checked, correctly indented, and free of interpolation-injection and whitespace bugs; a triple-quoted blob is none of those and silently drifts. The platform already standardizes on this: argocd's `OIDC_CONFIG` (the argocd-cm `oidc.config` block) uses `yaml.Marshal`, keycloak's `REALM_CONFIG` (the keycloak-config-cli import document) uses `json.Marshal`, and the refactored quay `CONFIG` (the `config.yaml` ConfigMap) uses `yaml.Marshal`.
- **How to apply:** Author the config as a CUE struct (a `let` binding or field), then set the consuming field to `yaml.Marshal(THAT_STRUCT)` (for a `.yaml`/`.yml` document) or `json.Marshal(THAT_STRUCT)` (for a `.json` document). Import `"encoding/yaml"` / `"encoding/json"` as needed. After editing, run `scripts/render` per the *CUE Component Rendering* guardrail.
- **Reference:** `holos/components/argocd/controller/buildplan.cue` (`OIDC_CONFIG` → `yaml.Marshal`), `holos/components/keycloak/realm-config/buildplan.cue` (`REALM_CONFIG` → `json.Marshal`), `holos/components/quay/buildplan.cue` (`CONFIG` → `yaml.Marshal`).

### Known Issues & Workarounds

#### Quay auth: OIDC sole identity store, Keycloak SSO, no PKCE + team syncing on (HOL-1293/HOL-1317, ADR-15 Revision 4)
- **Model (HOL-1293, ADR-15 Revision 4):** Quay runs `AUTHENTICATION_TYPE: OIDC` — the Keycloak `holos` realm is the **sole identity store**. There is **no** local `admin` user, and the `/api/v1/user/initialize` + `/api/v1/superuser/*` headless-bootstrap APIs are unavailable under OIDC by design. Users sign in with the **Holos SSO** button (Authorization Code flow) via the realm's confidential `quay` client. Revision 4 reverses Revision 3's brief Database-backend + federated-login model — **never** reintroduce `AUTHENTICATION_TYPE: Database`, `FEATURE_USER_INITIALIZE`, or a `quay-initial-admin`/`quay-admin-bootstrap` headless token.
- **`FEATURE_TEAM_SYNCING: true`:** team syncing is **enabled** (`FEATURE_TEAM_SYNCING: true` with `TEAM_RESYNC_STALE_TIME: 30m`). Under the OIDC backend the active user handler syncs the `groups` claim into Quay teams, so a synced team's **membership** tracks the claim automatically. (The Revision 3 `FEATURE_TEAM_SYNCING: false` workaround existed only because the Database user handler had no `sync_user_groups`; that constraint is gone with the OIDC backend.) **Which** teams exist, their **org role** (`admin`/`creator`/`member`), and their optional **org default repository permission** (`read`/`write`/`admin`) are declared **on the Organization CR** (`spec.syncedTeams[]`, HOL-1325, ADR-19 Revision 6) and reconciled by the shipped Holos Controller — Quay's `FEATURE_TEAM_SYNCING` keeps each team's membership in sync, the controller owns the team set/roles/default permissions. OIDC groups are referenced **by name** (the `oidcGroup` string); the `quay.holos.run` API group imports no Keycloak type.
- **PKCE disabled (HOL-1317):** the `quay` client uses **no** PKCE on either end — the Keycloak `quay` client sets `pkce.code.challenge.method` to the empty/"none" method (set explicitly, not omitted, so keycloak-config-cli's attribute merge overwrites any prior `S256` rather than leaving it to linger) and Quay's `KEYCLOAK_LOGIN_CONFIG` sets `USE_PKCE: false` (no `PKCE_METHOD`). Quay 3.17.3 mishandles PKCE state: it stores the `code_challenge`/verifier in the `_csrf_token` cookie and never clears it on logout, so a stale verifier is replayed on the next login and Keycloak rejects the exchange with `Got non-2XX response for code exchange: 400` (login-after-logout fails). HOL-1317 makes `quay` a deliberate PKCE exception again — the public `argocd`/`kargo` clients keep `S256`; only `quay` drops it. Do **not** reintroduce `pkce.code.challenge.method` on the Keycloak `quay` client or set `USE_PKCE: true` on the Quay side without first confirming the Quay logout-state bug is fixed (this reverses the brief HOL-1293/HOL-1294 PKCE re-enablement). The Keycloak `quay` client's `redirectUris` are the three explicit `/oauth2/keycloak/callback{,/attach,/cli}` paths from HOL-1317 (not a `/*` wildcard) with an empty `webOrigins`.
- **Superusers:** `SUPER_USERS` lists two Keycloak realm users by `preferred_username` — the service account **`svc-quay-resource-controller`** and the human **`quay-admin`** (both seeded by the keycloak phase, HOL-1294, with passwords generated once at runtime into Secrets of the same name in the `keycloak` namespace, key `password`). There is no local-`admin` break-glass account.
- **Data plane: org/repo/webhook/synced-teams now reconciled by the shipped controller; robots/pull-Secrets still manual:** the **Holos Controller** ([ADR-18](docs/adr/ADR-18.md)) has **shipped** (HOL-1309..HOL-1313, namespace `holos-controller`) with the `quay.holos.run/v1alpha1` Organization and Repository CRDs ([ADR-19](docs/adr/ADR-19.md), `Status: Implemented`, `Updates: ADR-15`), so in-cluster Quay **org/repo creation, the repo's `repo_push` webhook, and the org's OIDC-synced teams** (the Organization's `spec.syncedTeams[]` — team org role plus optional org default repository permission, HOL-1325, ADR-19 Revision 6) are reconciled declaratively. The robots and the Argo CD / Kargo pull-credential Secrets are **not** yet modeled by the `v1alpha1` CRDs (ADR-19 *Out of scope*) and stay manual for now. An operator still mints the controller's superuser OAuth-Application credential by hand (the credentials runbook below) into `holos-controller-quay-creds` (`holos-controller` namespace); the controller reads it via `credentialsSecretRef`. The removed `scripts/quay-init`/`scripts/quay-reset` helpers and the `my-project-quay-bootstrap` Job no longer exist.
- **`FEATURE_SUPERUSERS_FULL_ACCESS: true` (HOL-1299):** the `SUPER_USERS` reach is extended to orgs they neither own nor are members of, so the Holos Controller ([ADR-18](docs/adr/ADR-18.md)/[ADR-19](docs/adr/ADR-19.md)) can **adopt** and reconcile orgs created by other identities (its Organization claim model gates adoption on `spec.adopt`) — without it, `super:user` reaches only the `/api/v1/superuser/*` panel endpoints and a write inside a non-owned org `403`s. It applies to `SUPER_USERS` members only, but to **all** of their superuser sessions: both an OAuth token carrying the `super:user` scope (the controller) **and** an authenticated web/UI session (Quay grants superuser permission for `super:user` **or** the internal `direct_user_login` scope), so the human `quay-admin` signed in via "Holos SSO" also gains instance-wide read/write/delete across every org. This is not configurable per-user; it does not widen access for non-superusers. **Disambiguation (HOL-1299):** a Quay OAuth Application (and its token) can only be created inside an **organization**, never directly "for" a user; the token acts as the **user who generated it**, bounded by that user's rights and the token's scopes — the host org (the manually-created **`platform-automation`** org owned by `svc-quay-resource-controller`) is **not** a permission boundary, just where the credential record lives.
- **Related:** `holos/components/keycloak/realm-config/buildplan.cue` (the `quay` client — `pkce.code.challenge.method: ""` empty/"none" method (HOL-1317); the `svc-quay-resource-controller`/`quay-admin` realm users), `holos/components/quay/buildplan.cue` (`AUTHENTICATION_TYPE: OIDC`, `FEATURE_TEAM_SYNCING: true`, `FEATURE_SUPERUSERS_FULL_ACCESS: true`, `USE_PKCE: false` (HOL-1317), the `SUPER_USERS` list, the `KEYCLOAK_LOGIN_CONFIG` block), `docs/adr/ADR-15.md` (Revision 7), `docs/adr/ADR-18.md` (the shipped Holos Controller — the Quay Resource Controller, `Partially Implemented`), `docs/adr/ADR-19.md` (the `quay.holos.run` Organization/Repository CRDs, `Implemented`), `holos/docs/keycloak-clients.md` (the PKCE guardrail checklist), `docs/runbooks/quay-keycloak-oidc.md` (the operational runbook), `docs/runbooks/quay-resource-controller-credentials.md` (the manual superuser OAuth-Application credential procedure, including the `platform-automation` org bootstrap and the full-access semantics), and `docs/runbooks/holos-controller.md` (wiring the controller to that credential, AC #3).

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
- **Pattern:** A project that receives Kargo-driven OCI delivery is laid down as a **single component** that emits, together, a hand-authored Argo CD AppProject + OCI-source Application (with `kargo.akuity.io/authorized-stage` and `targetRevision` omitted so Kargo owns it), the Kargo Project/ProjectConfig/Warehouse/Stage, **and** — as of HOL-1322 — a `quay.holos.run/v1alpha1` **Organization** CR (`my-project`, with `spec.adopt: false`, an illustrative `spec.syncedTeams[]` set — the GCP-style primitive-role example `my-project-owner`→`admin`, `my-project-editor`→`member`+`write`, `my-project-viewer`→`member`+`read`, HOL-1330 — and a per-cluster local-ca `caBundle`) that the shipped Holos Controller ([ADR-18](docs/adr/ADR-18.md)/[ADR-19](docs/adr/ADR-19.md)) reconciles into the in-cluster Quay org (creating the org and its OIDC-synced teams). The component is **applied by `scripts/apply-my-project`, not `scripts/apply`** (HOL-1322 removed `my-project` from the master apply): that dedicated script injects the local-ca PEM at apply time via the `ca_bundle_pem` CUE tag (the `scripts/publish` `--inject` pattern), so the committed `holos/deploy/` tree carries the Organization manifest with **no** `caBundle` material. The rest of the Quay data plane (the **Repository** CR, the pull-robot, and the `repo_push` webhook **registration**, plus the Argo CD/Kargo pull-credential Secrets) is **not** emitted by the component: the Repository CR is reconciled by the controller but is not yet emitted here (only the Organization is), and the robots/pull Secrets remain manual (ADR-19 *Out of scope*). The proposed Holos Project/Application components ([ADR-21](docs/adr/ADR-21.md)) — generalizing this `my-project` scaffold — would emit the per-project/app Organization **and** Repository CRs. The removed Database-backend `my-project-quay-bootstrap` Job no longer participates. The project Namespace is **not** emitted by the component either — it is registered centrally in `holos/namespaces.cue` (never inline, per the component guidelines) and referenced by name. The Kargo Project namespace doubles as the workload namespace (no separate `kargo-project-*` sibling — that split is only the echo spike). `my-project` is the reference instance and the template for a future self-service `ProjectRequest`.
- **Org + synced teams reconciled from the emitted Organization CR; Repository/robots/Secrets still partly manual:** the in-cluster Quay data plane a project needs splits into parts. The `my-project` component **emits the Organization CR** (including its `spec.syncedTeams[]` — the owner/editor/viewer primitive-role example), which the shipped Holos Controller ([ADR-18](docs/adr/ADR-18.md)) reconciles from the `quay.holos.run` Organization CRD ([ADR-19](docs/adr/ADR-19.md), `Implemented`): it creates the org and provisions the OIDC-synced teams (team org role + optional org default repository permission) non-exclusively, tracking ownership in `status.managedTeams`. The OIDC groups are referenced by name only — `my-project-{owner,editor,viewer}` are future Keycloak work (ADR-20/ADR-21), no Keycloak dependency. The controller trusts the in-cluster Quay's mkcert-signed serving cert via the resource's `caBundle` (the standardized cross-Kind field, ADR-19) rather than the pod's system trust store. The **Repository** CR (and its `repo_push` webhook), the **push robot, the Argo CD pull-credential repository Secret in `argocd`, and the Kargo image-credential Secret** are **not** emitted by the component today: the Repository CRD is reconciled by the controller but the `my-project` component does not emit a Repository CR yet, and the robots/pull Secrets are not modeled by the `v1alpha1` CRDs at all (ADR-19 *Out of scope*) and stay manual. The proposed Holos Project/Application components ([ADR-21](docs/adr/ADR-21.md)) would emit those per project/app. The Database-backend bootstrap Job (`my-project-quay-bootstrap`), the `quay-initial-admin` superuser token it authenticated with, and the `scripts/quay-init`/`scripts/quay-reset` helpers were **removed** with the OIDC switch and no longer exist. An operator mints the controller's OAuth-Application credential (see `docs/runbooks/quay-resource-controller-credentials.md`, consumed per `docs/runbooks/holos-controller.md`), runs `scripts/apply-my-project`, and provisions the still-manual scaffolding by hand; the project's Argo CD Application stays `Unknown`/`Missing` until the first config artifact is published — expected scaffolding.
- **Hand-authored Application vs. the deferred projection:** The sample Applications (`echo`, `my-project`) are hand-authored **OCI**-source Applications, distinct from the deferred per-component `argoAppDisabled` **git**-source projection (`holos/docs/placeholders.md` → *ArgoCD gitops delivery*). Do not conflate them.
- **`caBundle` field convention (ADR-19):** the Organization's `spec.caBundle` is the standardized cross-Kind trust-anchor field — a PEM/base64 (`[]byte`) bundle of x509 CA certs the controller trusts **in addition to** its system store when reaching the Quay API; empty/omitted means use the pod's system trust store unchanged. Both `quay.holos.run` Kinds (Organization and Repository) carry the identical field with shared semantics; see [ADR-19](docs/adr/ADR-19.md) (*CA bundle (`caBundle`) — a standardized, cross-Kind field*). The `my-project` Organization fills it with the per-cluster local-ca PEM, injected at apply time by `scripts/apply-my-project` and never committed.
- **Reference:** `holos/components/my-project/buildplan.cue` (the emitted Organization CR + `spec.syncedTeams[]` + gated `spec.caBundle`), `scripts/apply-my-project` (the dedicated apply step that injects the local-ca PEM), `holos/README.md` (*The `my-project` delivery scaffold*), `holos/docs/oci-publish-workflow.md` (*Downstream: the `my-project` delivery scaffold*), `docs/adr/ADR-16.md`, `docs/adr/ADR-19.md` (the `caBundle` convention and the synced-teams design, Revision 6).

### Adding a Keycloak OIDC (PKCE) Client
- **Pattern:** The realm's OIDC clients (argocd, quay) are declared in `realm-config/buildplan.cue` and reconciled by the `keycloak-config` keycloak-config-cli Job. The conventional declarative-client pattern — public vs confidential decision, the `S256` attribute, the confidential secret-bootstrap Job, `IMPORT_VARSUBSTITUTION_ENABLED`, the three mappers that feed the shared `groups` claim, the role model, and the render-then-commit workflow — is documented as a guardrail checklist.
- **Before adding another PKCE client:** Read `holos/docs/keycloak-clients.md` and follow its guardrail checklist rather than rediscovering the pattern. Default to requiring PKCE (`pkce.code.challenge.method: "S256"`) for every client; relax it only for a client with a demonstrated implementation gap. The `argocd` and `kargo` clients use `S256`; the confidential `quay` client is the one PKCE exception — HOL-1317 dropped PKCE for it because Quay 3.17.3 replays a stale `code_verifier` after logout and breaks the next login (reversing the brief HOL-1293/HOL-1294 re-enablement). The `quay` client is confidential (authenticated by its client secret) where `argocd`/`kargo` are public. Under the OIDC backend (ADR-15 Revision 4) the Keycloak `holos` realm is Quay's sole identity store, so for `quay` the OIDC client *is* the identity backend, not merely a login overlay.
- **Reference:** `holos/docs/keycloak-clients.md`, `docs/runbooks/quay-keycloak-oidc.md`

### Quay Superuser Credential — manual OAuth-Application token (HOL-1293)
- **Rule:** Quay's REST API takes a **superuser OAuth token**, and under the OIDC backend (ADR-15 Revision 4) there is **no headless** way to mint one — the local `admin` user and the one-shot `/api/v1/user/initialize` endpoint do not exist. The credential is created **by hand**: an operator signs in via "Holos SSO" as the realm superuser `svc-quay-resource-controller` (password from its Secret in the `keycloak` namespace), creates a Quay OAuth Application, and generates a scoped token. **Do not** reintroduce a `quay-initial-admin`/`quay-admin-bootstrap` Job, the `FEATURE_USER_INITIALIZE` endpoint, or any assumption of an automatically-minted token — they were removed (HOL-1293).
- **Why manual:** the OIDC backend makes the Keycloak realm the sole identity store, which is the deliberate trade for declarative identity (no second password store, no break-glass local admin). Quay ships no operator to mint a first superuser token declaratively, so the bootstrap stays a documented manual step. The **Quay Resource Controller** has **shipped** as the **Holos Controller** ([ADR-18](docs/adr/ADR-18.md)) with the `quay.holos.run` CRDs ([ADR-19](docs/adr/ADR-19.md), `Status: Implemented`) and takes over the **org/repo/webhook provisioning** — but it still *consumes* this superuser OAuth-Application token (it authenticates to Quay with the credential the runbook mints), so the manual mint stays operationally true. The token is the controller's external credential, not one of the CRDs it reconciles; the contract is the **`holos-controller-quay-creds` Secret** (keys `url`/`token`/optional `username`) in the **`holos-controller` namespace**, which each resource's `credentialsSecretRef` defaults to. The `apply-svc-quay-resource-controller-creds` helper creates it; `docs/runbooks/holos-controller.md` documents the consumer-side wiring (AC #3).
- **The two superusers:** `SUPER_USERS` lists the Keycloak realm users `svc-quay-resource-controller` (service account — its `svc-` prefix marks it as such) and `quay-admin` (human). Both passwords are generated once at runtime into Secrets of the same name in the `keycloak` namespace (key `password`); retrieve with `kubectl -n keycloak get secret <name> -o jsonpath='{.data.password}' | base64 -d`.
- **How to apply:** Follow `docs/runbooks/quay-resource-controller-credentials.md` to create the OAuth Application, choose its scopes (e.g. `super:user`/`org:admin`/`repo:create`), generate the token, and store it (via `scripts/apply-svc-quay-resource-controller-creds`) as the `holos-controller-quay-creds` Secret (keys `url`/`token`/optional `username`) in the `holos-controller` namespace — the credential the shipped controller reads. Store the token as a Secret's *material* per the *Runtime Secret Handling* guardrail — never commit it. See `docs/runbooks/holos-controller.md` for the consumer-side wiring.
- **Reference:** `holos/components/quay/buildplan.cue` (`AUTHENTICATION_TYPE: OIDC`, `SUPER_USERS`, `FEATURE_SUPERUSERS_FULL_ACCESS: true`), `holos/components/keycloak/realm-config/buildplan.cue` (the `svc-quay-resource-controller`/`quay-admin` realm users + password Secrets), `scripts/apply-svc-quay-resource-controller-creds` (creates `holos-controller-quay-creds` in `holos-controller`), `docs/runbooks/quay-resource-controller-credentials.md` (the manual credential procedure, the `platform-automation` org bootstrap, and the full-access semantics), `docs/runbooks/holos-controller.md` (the consumer-side credential wiring + AC #3 superuser-token assumption), `docs/runbooks/quay-keycloak-oidc.md` (the OIDC model and superuser verification), `docs/adr/ADR-15.md` (Revision 7), `docs/adr/ADR-18.md`/`docs/adr/ADR-19.md` (the shipped Holos Controller + Quay CRDs that reconcile the org/repo/webhook provisioning; the controller consumes this superuser token as its external credential).

## Documentation index

- [docs/adr/](docs/adr/README.md) — Architecture Decision Records: the
  binding design decisions. Start with the index; follow
  [writing-adrs.md](docs/adr/writing-adrs.md) before adding or revising one.
  The **Holos Controller** design set lives here: ADR-18 (the controller and
  its GitOps rendered-manifest delivery model, `Partially Implemented`), ADR-19
  (`quay.holos.run` Organization/Repository CRDs, **`Implemented`** as built,
  `Updates: ADR-15`; Revision 6 adds the Organization's `syncedTeams[]` —
  OIDC-synced Quay teams with org role + optional default repository permission,
  the GCP-style owner/editor/viewer primitive-role model), ADR-20 (the Keycloak API group CRDs, `Proposed`,
  `Updates: ADR-3`), and ADR-21 (the Holos Project/Application components,
  `Proposed`, `Updates: ADR-1`). The controller (`holos-controller` namespace)
  and its Quay API group have **shipped** (HOL-1309..HOL-1313) — formerly the
  "future Quay Resource Controller"; the Keycloak group and the
  Project/Application self-service experience remain proposed.
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
  (including `platform-owner` into the quay client), the Quay superuser model
  (`SUPER_USERS` = `svc-quay-resource-controller`/`quay-admin`), and the
  guardrail checklist for adding a new PKCE client (`argocd`/`kargo` use `S256`;
  the confidential `quay` client is the lone no-PKCE exception, HOL-1317).
- [holos/docs/argocd-application-source.md](holos/docs/argocd-application-source.md)
  — the MVP Argo CD `Application` source pattern: OCI rendered-manifests
  artifacts in the in-cluster Quay registry, the repository credential
  Secret shape, and how the repo-server reaches Quay.
- [holos/docs/kargo-keycloak-oidc.md](holos/docs/kargo-keycloak-oidc.md) — the
  Kargo↔Keycloak OIDC (PKCE) integration: the public kargo client and
  groups-claim role mapping, issuer-cert trust via the local-ca cabundle, and
  the verification/maintenance runbook.
- [docs/runbooks/quay-keycloak-oidc.md](docs/runbooks/quay-keycloak-oidc.md) —
  operational runbook for the Quay↔Keycloak OIDC SSO integration: the
  **OIDC sole-identity-store** model (`AUTHENTICATION_TYPE: OIDC`, ADR-15
  Revision 4 — HOL-1293), how the confidential `quay` client and the
  `quay-oidc` secret bootstrap are wired, the two Keycloak realm superusers
  (`svc-quay-resource-controller`/`quay-admin`) and "Holos SSO" login +
  `SUPER_USERS` model, PKCE disabled for `quay` (HOL-1317 — Quay 3.17.3 replays
  a stale `code_verifier` after logout), grant/rotate/reconcile operations, and
  the `code exchange: 400` login-after-logout failure that motivated dropping
  PKCE. Companion to [ADR-15](docs/adr/ADR-15.md).
- [docs/runbooks/quay-resource-controller-credentials.md](docs/runbooks/quay-resource-controller-credentials.md)
  — the operator procedure for manually minting the Quay superuser
  OAuth-Application credential the shipped Holos Controller consumes: sign in
  via "Holos SSO" as `svc-quay-resource-controller` (password from its Secret),
  create a Quay OAuth Application, generate a scoped token, and store it as a
  Kubernetes Secret. Documents which org the Application is created under, the
  required scopes, and how to verify org-creation. Replaces the removed
  headless `quay-initial-admin` bootstrap. The token now lands as the
  `holos-controller-quay-creds` Secret (keys `url`/`token`/optional `username`)
  in the `holos-controller` namespace, which the shipped Holos Controller
  ([ADR-18](docs/adr/ADR-18.md)) reads via `credentialsSecretRef` to reconcile
  the `quay.holos.run` CRDs ([ADR-19](docs/adr/ADR-19.md)); the mint stays a
  manual step because the controller consumes (does not generate) this credential.
- [docs/runbooks/holos-controller.md](docs/runbooks/holos-controller.md) — the
  consumer-side runbook for the Holos Controller: the **AC #3** assumption that a
  single **superuser-account** OAuth-Application token authenticates all
  controller-managed Quay operations, and how to wire it — the
  `holos-controller-quay-creds` Secret (keys `url`/`token`/optional `username`)
  in the `holos-controller` namespace that each resource's `credentialsSecretRef`
  defaults to, created by `scripts/apply-svc-quay-resource-controller-creds`,
  resolved from the controller's own namespace via `POD_NAMESPACE`. Covers the
  isolated `controller-*` deploy targets, metrics verification, and the
  cluster-bring-up step — after `scripts/local-ca` and the manual credential
  mint, run `scripts/apply-my-project` to provision the `my-project` Namespace +
  Organization (carrying the local-ca `caBundle` the controller trusts the
  in-cluster Quay's serving cert with, instead of the pod's system trust store).
  Companion to [ADR-19](docs/adr/ADR-19.md) and the credentials runbook above.
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
- [*No raw inline YAML/JSON in CUE — marshal it*](#no-raw-inline-yamljson-in-cue--marshal-it)
  (Guard Rails, above) — **binding guardrail**: embedded YAML/JSON config in a
  `.cue` file is authored as a CUE struct and serialized with
  `encoding/yaml.Marshal()` / `encoding/json.Marshal()`, never a triple-quoted
  interpolated string. Precedents: argocd `OIDC_CONFIG`, keycloak
  `REALM_CONFIG`, quay `CONFIG`.
- [*Keycloak service-account naming (`svc-` prefix)*](#conventions) (Conventions,
  below) — Keycloak realm users that represent service accounts are named with
  an `svc-` prefix (e.g. `svc-quay-resource-controller`); human accounts are
  not (e.g. `quay-admin`).

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
- **Keycloak realm users that represent service accounts MUST be named with an
  `svc-` prefix** (e.g. `svc-quay-resource-controller` — the future Quay
  Resource Controller's machine identity) so they are unambiguously
  distinguishable from human users, which are **not** prefixed (e.g.
  `quay-admin`). The two superuser realm users seeded in HOL-1294 are the worked
  example: `svc-quay-resource-controller` (service account) and `quay-admin`
  (human). See `holos/components/keycloak/realm-config/buildplan.cue`.
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
