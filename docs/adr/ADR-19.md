# Quay API Group (`quay.holos.run`): Organization and Repository CRDs

| Metadata | Value                          |
| -------- | ------------------------------ |
| Date     | 2026-06-17                     |
| Author   | @jeffmccune                    |
| Status   | `Proposed`                     |
| Tags     | api, controller, quay, registry |
| Updates  | ADR-15                         |

| Revision | Date       | Author      | Info           |
| -------- | ---------- | ----------- | -------------- |
| 1        | 2026-06-17 | @jeffmccune | Initial design |

## Context and Problem Statement

The [Holos Controller](ADR-18.md) is the in-cluster controller that fills the
data-plane gaps the upstream Quay and Keycloak operators leave open, so product
engineers get a self-service "docker push to deploy" experience. Its first API
group is **`quay.holos.run`**. This ADR is the detailed design specification for
that group's two custom resources, both scoped to the in-cluster Quay registry:
an **Organization** and a **Repository**.

Today the Quay data plane a project needs — its organization, the
rendered-manifests repository, a pull robot, the Argo CD repository pull-credential
Secret, and the `repo_push` webhook that notifies Kargo — is provisioned **by
hand**. [ADR-15](ADR-15.md) Revisions 4–5 made the Keycloak `holos` realm Quay's
sole identity store (`AUTHENTICATION_TYPE: OIDC`), which removed the local `admin`
user and the headless token-mint path, so an operator now follows the
[Quay Resource Controller credentials runbook](../runbooks/quay-resource-controller-credentials.md)
to mint a superuser OAuth-Application credential and click through the rest. The
[`my-project` delivery scaffold](../../holos/components/my-project/buildplan.cue)
documents the surface that procedure must cover: it emits the Kargo control plane
and the Argo CD Application but **explicitly defers** the Quay org, repo, robot,
Argo CD pull-credential Secret, and `repo_push` webhook registration "to a future
Quay Resource Controller."

That future controller is the Holos Controller ([ADR-18](ADR-18.md)). This ADR
designs the two `quay.holos.run` resources that replace the by-hand provisioning
with reconciled custom resources. The scope is deliberately narrow: design **just
enough** schema to reach the docker-push-to-deploy goal, not a complete model of
Quay's API. **No code or CRD Go types are written here — this is the design record
(spec/status shape and reconciler behavior) only.**

## References

- [ADR-18 — The Holos Controller and the GitOps Rendered-Manifest Delivery
  Model](ADR-18.md): names the controller, its `holos-controller` namespace, and
  the `<group>.holos.run` convention whose first group is `quay.holos.run`. This
  ADR specifies that group's CRDs; ADR-18 has the forward cross-reference to this
  one.
- [ADR-15 — Quay↔Keycloak OIDC SSO](ADR-15.md), Revisions 4–5: the identity and
  credential model these reconcilers run within — `AUTHENTICATION_TYPE: OIDC`, the
  two superusers (`svc-quay-resource-controller`, `quay-admin`), the
  `groups`-claim → Quay-team syncing this ADR's access model keys on, and
  `FEATURE_SUPERUSERS_FULL_ACCESS` that lets the controller adopt orgs it did not
  create. This ADR **updates** ADR-15: the manual data-plane stop-gap ADR-15
  defers becomes these reconciled resources.
- [Quay Resource Controller credentials runbook](../runbooks/quay-resource-controller-credentials.md):
  the exact manual procedure (OAuth Application in the `platform-automation` org,
  scopes `super:user`/`org:admin`/`repo:create`, token-as-Secret) the Organization
  and Repository reconcilers automate. The credential it mints by hand becomes the
  controller's service-account credential.
- [ADR-8 — Container registry and image tagging](ADR-8.md): the registry these
  CRDs provision orgs and repositories in, and its digest-pinning convention the
  Kargo/Argo CD flow downstream of the `repo_push` webhook relies on.
- [ADR-16 — Kargo-Driven Promotion](ADR-16.md): the promotion pipeline the
  Repository's `repo_push` webhook feeds — a push notifies a Kargo `Warehouse`,
  which creates `Freight` and triggers a `Stage` promotion.
- [`holos/docs/oci-publish-workflow.md`](../../holos/docs/oci-publish-workflow.md):
  how the repository, the robot pull-credential, and the `repo_push` webhook
  participate in the publish → Warehouse → Freight → promotion → Argo CD sync
  loop. The "Downstream: the `my-project` delivery scaffold" section enumerates
  the deferred Quay objects these CRDs provision.
- [`holos/docs/argocd-application-source.md`](../../holos/docs/argocd-application-source.md):
  the Argo CD repository pull-credential Secret shape (`holos+robot` username, the
  robot token, `insecure: "true"`) the Repository reconciler emits so Argo CD's
  repo-server can pull the artifact.
- [`holos/docs/secret-handling.md`](../../holos/docs/secret-handling.md): the
  runtime-secret guardrail — secret material is created at runtime and never
  committed; bootstrap is generate-once and idempotent. The reconcilers honor this
  for the **robot-token** Secrets they own (the Argo CD repository pull-credential
  and the Kargo image-credential Secrets); the Kargo webhook-receiver Secret is
  not theirs — it stays owned by the `ProjectConfig`.

## Design

Both resources are **namespaced** custom resources in the `quay.holos.run` API
group, reconciled by the Holos Controller against the in-cluster Quay registry
(`quay.holos.localhost`). They model **only** what the docker-push-to-deploy goal
requires; every field below traces to a concrete object the
[credentials runbook](../runbooks/quay-resource-controller-credentials.md) or the
[`my-project` scaffold](../../holos/components/my-project/buildplan.cue) provisions
by hand today. Spec and status are presented as **illustrative YAML and field
tables**, not Go types — the schemas are the design, the types are implementation.

### Organization

An `Organization` names and applies a Quay organization, maps OIDC group
membership to Quay roles within it, and governs repository creation inside it.

```yaml
apiVersion: quay.holos.run/v1alpha1
kind: Organization
metadata:
  name: my-project
  namespace: my-project
spec:
  # (a) The Quay organization name to create/adopt. Defaults to metadata.name.
  organizationName: my-project
  # Quay requires every namespace to have a unique email.
  email: my-project@holos.localhost
  # Opt-in to adopting a pre-existing, externally-created (unmarked) org. Default
  # false: an unmarked org is a Conflict, never silently seized (see claim model).
  adopt: false

  # (b) Access via OIDC group membership: which Keycloak group (as it appears in
  # the `groups` claim — ADR-15) is bound to which Quay team, at which role.
  # The reconciler creates the team and the team→group binding; FEATURE_TEAM_SYNCING
  # (ADR-15 Rev 4) then keeps membership eventually consistent from the claim.
  access:
    - group: my-project-admins   # Keycloak group / role name in the groups claim
      team: admins               # Quay team in this org (created if absent)
      role: admin                # admin | member | creator
    - group: my-project-devs
      team: developers
      role: member

  # (c) May org members create repositories ad hoc through Quay itself (the
  # Quay org-level toggle)? This governs only the Quay UI / first-push creation
  # path; it never blocks the declarative paths in (d). false keeps repositories
  # declarative — the only repos that exist are those the platform reconciles.
  allowRepositoryCreation: false

  # (d) Repositories to create within the org, declared inline. An inline entry
  # and a standalone Repository CR (below) are the SAME provisioning path. To
  # avoid two owners for one Quay repo, a given <org>/<repo> is declared EITHER
  # inline here OR by a standalone Repository CR, never both — the reconciler
  # treats a collision as a conflict and surfaces it on status rather than
  # racing two writers. Inline is the minimal form (name + visibility); a
  # standalone Repository CR is the form when retention or a repo_push webhook
  # is needed.
  repositories:
    - name: my-project-config
      visibility: private        # private | public
status:
  observedGeneration: 2
  organizationName: my-project
  conditions:
    - type: Ready
      status: "True"
      reason: Provisioned
    - type: Synced              # teams/bindings reconciled from spec.access
      status: "True"
```

| Spec field | Purpose |
| --- | --- |
| `organizationName` | (a) the Quay org to create or adopt; defaults to `metadata.name`. |
| `email` | unique namespace email Quay requires. |
| `adopt` | opt-in (default `false`) to take ownership of a pre-existing unmarked org; without it an unmarked org is a `Conflict`. |
| `access[]` | (b) OIDC group → Quay team + role bindings; the reconciler creates teams and team→group bindings, `FEATURE_TEAM_SYNCING` (ADR-15) syncs membership. |
| `allowRepositoryCreation` | (c) the Quay org-level toggle for ad-hoc repo creation via the UI/first-push; does not affect the declarative paths in (d). |
| `repositories[]` | (d) inline repositories to create within the org; a given repo is declared inline OR by a standalone `Repository`, never both. |

| Status field | Purpose |
| --- | --- |
| `observedGeneration` | last `spec` generation reconciled. |
| `organizationName` | the observed Quay org name. |
| `conditions[]` | `Ready` (org provisioned; `Ready=False reason: Conflict` when a foreign CR already owns the org — see the claim model), `Synced` (teams/bindings converged). |

### Repository

A `Repository` is a single repository within an owning `Organization`, with the
retention and webhook configuration the delivery loop needs.

```yaml
apiVersion: quay.holos.run/v1alpha1
kind: Repository
metadata:
  name: my-project-config
  namespace: my-project
spec:
  # (a) The owning Organization CR in this namespace (by metadata.name). The
  # reconciler resolves it to that Organization's actual Quay org —
  # Organization.spec.organizationName (which may differ from its metadata.name)
  # — and creates the repo as <resolved-org>/<repositoryName>. The CR reference
  # and the Quay org name are kept distinct on purpose.
  organizationRef: my-project
  repositoryName: my-project-config
  visibility: private            # private | public

  # (b) Retention: prune old tags so the rendered-manifests repo does not grow
  # without bound. Maps to Quay's auto-prune policy on the repository.
  retention:
    keepTags: 50                 # keep the N most-recent tags
    # OR keepDays: 30            # keep tags pushed within the last N days

  # (c) Register a repo_push webhook in Quay so a push notifies a Kargo
  # Warehouse to create Freight (ADR-16). The target URL is the hard-to-guess
  # receiver URL Kargo derives from the ProjectConfig's webhook-receiver Secret
  # and publishes on ProjectConfig.status.webhookReceivers[].url — it is NOT
  # known at render time, so the reconciler READS it from that status rather
  # than taking a literal URL in spec. The CR points at the ProjectConfig
  # whose receiver to wire; the receiver Secret stays owned by Kargo's
  # ProjectConfig (key `secret`) and is never copied into the Quay webhook.
  pushWebhook:
    kargoProjectConfigRef:
      name: my-project          # ProjectConfig in this namespace
      receiver: quay            # which webhookReceivers[].name to wire
status:
  observedGeneration: 1
  repository: my-project/my-project-config
  # The receiver URL the reconciler read from the ProjectConfig status and
  # registered the Quay webhook against (resolved, not render-time input).
  webhookURL: https://kargo.holos.localhost/webhook/quay/<receiver-id>
  # The Quay robots the reconciler provisioned: the pull robot backs both
  # pull-credential Secrets; the push robot backs scripts/publish's write Secret.
  pullRobot: my-project+argocd
  pushRobot: my-project+publish
  conditions:
    - type: Ready
      status: "True"
      reason: Provisioned
    - type: WebhookRegistered   # set False+reason if the receiver URL is not
      status: "True"            # yet published on the ProjectConfig status
```

| Spec field | Purpose |
| --- | --- |
| `organizationRef` | (a) the owning `Organization` CR (by `metadata.name`); resolved to its `spec.organizationName` for the Quay path. |
| `repositoryName` | the repository name; full path is `<resolved-org>/<repositoryName>`. |
| `visibility` | `private` or `public`. |
| `retention` | (b) auto-prune policy — `keepTags` count or `keepDays` window. |
| `pushWebhook` | (c) the Kargo `ProjectConfig` + receiver to wire; the reconciler reads the receiver URL from that ProjectConfig's `status` (not a render-time literal) and registers a `repo_push` webhook against it. |

| Status field | Purpose |
| --- | --- |
| `observedGeneration` | last `spec` generation reconciled. |
| `repository` | the observed `<org>/<repo>` path. |
| `webhookURL` | the receiver URL read from the ProjectConfig status and registered with Quay. |
| `pullRobot` | the Quay robot backing both pull-credential Secrets (the Argo CD repository Secret and the Kargo image-credential Secret). |
| `pushRobot` | the Quay robot backing the push-credential Secret `scripts/publish` writes the artifact with. |
| `conditions[]` | `Ready` (repo/retention/robots + pull and push Secrets converged) and `WebhookRegistered` (the receiver URL was published and the Quay webhook registered). |

### Out of scope (deliberately)

Quay's API exposes far more than the delivery loop needs. The following are
**explicitly out of scope** for these CRDs until a concrete requirement appears —
keeping the schema minimal and goal-driven, per [ADR-18](ADR-18.md):

- Per-repository permissions beyond the org `access[]` team model, repository
  mirroring, and image-security/vulnerability scanning configuration.
- Webhook event types other than `repo_push` (the only event the Kargo loop
  consumes), and multiple webhooks per repository.
- Org-level quotas, billing, and proxy-cache configuration.
- Multiple robots per repository — one **pull** robot (backing both pull Secrets)
  and one **push** robot (for `scripts/publish` to write the artifact) per repo is
  all the loop requires; additional robots are out of scope.

A field beyond what docker-push-to-deploy requires is added by **revising this
ADR** with a new requirement, not by speculative schema.

### Reconciler behavior

Both reconcilers translate their CR's `spec` into **Quay REST API** calls using
the controller's superuser OAuth-Application token, and are **idempotent** — a
re-reconcile of an unchanged `spec` makes no further changes.

**Credential.** The reconcilers authenticate to Quay with the **superuser
OAuth-Application token** described in [ADR-15](ADR-15.md) Revisions 4–5 and the
[credentials runbook](../runbooks/quay-resource-controller-credentials.md): a
token generated as the `svc-quay-resource-controller` realm user (a `SUPER_USERS`
member), carrying the `super:user`, `org:admin`, and `repo:create` scopes, stored
as the `quay-resource-controller` Secret (key `token`) in the `quay` namespace.
Because the token acts **as** `svc-quay-resource-controller` and
`FEATURE_SUPERUSERS_FULL_ACCESS: true` is set, the controller can both create new
orgs and **adopt** orgs other identities created, reconciling them through the
normal (non-`superuser`) endpoints. The controller reads this Secret per the
runtime-secret guardrail; it is never committed.

**Ownership and the claim model.** `Organization` CRs are namespaced, but Quay
orgs are a **single, global namespace**, and the controller's credential carries
`FEATURE_SUPERUSERS_FULL_ACCESS` (instance-wide write). A naive "adopt any
existing org" rule would let an `Organization` in one tenant namespace silently
seize another project's Quay org and overwrite its teams, repos, and webhooks.
The reconciler therefore enforces a **claim**:

- The controller stamps each org it owns with an **ownership marker** recording
  the claiming CR's identity (namespace/name) — e.g. a reserved metadata field on
  the org, or a dedicated controller-managed record. (The exact mechanism is an
  implementation detail; the *requirement* is a durable owner record.)
- On reconcile, the controller acts on exactly three cases:
  - **Org does not exist** → **create** it and stamp this CR's ownership marker
    (the clean GitOps path; the creating user owns it).
  - **Org exists and its marker names *this* CR** → reconcile it normally
    (the steady-state path).
  - **Org exists with *no* marker, or a marker naming a *different* CR** →
    **conflict**: the reconciler refuses to write, sets `Ready=False`
    `reason: Conflict`, and emits an event. A marker-absent org is treated as a
    conflict, **not** auto-adopted — an externally-created org is never silently
    seized just because the credential *can* write to it.
- Adopting a pre-existing, externally-created org (marker absent) is therefore an
  **explicit, opt-in** act: the CR must set `adopt: true` (and only then does the
  controller stamp its marker and take ownership). Default reconciliation never
  adopts an unmarked org. This bounds the `FEATURE_SUPERUSERS_FULL_ACCESS` blast
  radius to orgs the platform created or was explicitly told to adopt.

**Organization reconcile** then maps `spec` to these Quay calls (idempotently,
within the claim model above — never error on an org *this CR* already owns):

- `POST /api/v1/organization/` to create `organizationName` when absent (stamping
  the ownership marker); reconcile an existing org only when its marker names this
  CR, or when the org is unmarked **and** `spec.adopt: true` was set
  (`FEATURE_SUPERUSERS_FULL_ACCESS` makes the write possible, the ownership marker
  + explicit opt-in make it *safe*). Otherwise → `Conflict`.
- For each `access[]` entry: create the Quay **team** (`PUT
  /api/v1/organization/<org>/team/<team>`) at its `role`, and bind it to the
  Keycloak group (the team→group binding) so `FEATURE_TEAM_SYNCING` keeps
  membership eventually consistent from the `groups` claim (ADR-15).
- Set the org's ad-hoc-repo-creation toggle from `allowRepositoryCreation`.
- For each inline `repositories[]` entry, **only** create-or-adopt the Quay repo
  at the entry's `visibility` — the minimal `name`+`visibility` form, nothing
  more. Inline entries deliberately do **not** carry retention, a `repo_push`
  webhook, or pull/push credentials; a repo that needs any of those is declared as
  a standalone `Repository` CR (which owns the full reconcile in *Repository
  reconcile* below). The single-owner rule applies: an inline entry and a
  standalone `Repository` must not target the same repo.

**Repository reconcile** maps `spec` to:

- `POST /api/v1/repository` (or adopt) to create `<org>/<repositoryName>` at
  `visibility`.
- Apply the `retention` auto-prune policy to the repository.
- Provision the Quay **pull robot** once, then write **two** credential Secrets
  from that one robot token (both create-if-absent, per
  [secret-handling.md](../../holos/docs/secret-handling.md) — rotation would break
  in-flight pulls and Warehouse discovery):
  - the **Argo CD repository Secret** in the `argocd` namespace — the
    `holos+robot`-shaped Secret
    ([argocd-application-source.md](../../holos/docs/argocd-application-source.md):
    `username`, robot `password`, `insecure: "true"`) so Argo CD's repo-server can
    pull the OCI artifact, **and**
  - the **Kargo image-credential Secret** in the project (Warehouse) namespace —
    a Secret labeled `kargo.akuity.io/cred-type: image` with `repoURL`/`username`/
    `password`, so the Kargo `Warehouse` controller can authenticate to the
    private Quay repo and discover Freight. Without it, Warehouse discovery
    `401`s and **no Freight is created**
    ([oci-publish-workflow.md](../../holos/docs/oci-publish-workflow.md) — the
    `kargo.akuity.io/cred-type: image` Secret the `my-project` verification
    creates by hand today). The project namespace the Secret lands in is the
    owning `Organization`/`Repository` CR's namespace (the Kargo Project
    namespace), read from the referenced ProjectConfig.
- Provision a Quay **push robot** (write scope on the repo) so `scripts/publish`
  can write the rendered-manifests artifact, and write its token as a
  create-if-absent **push-credential Secret** in the CR's namespace. This is the
  reconciled replacement for the **destination push credential** that
  [oci-publish-workflow.md](../../holos/docs/oci-publish-workflow.md) defers "to a
  future Quay Resource Controller" — this controller is that controller (ADR-18),
  so it owns the push robot rather than leaving it deferred. (Distinct from the
  pull robot above; one pull + one push robot per repo, per *Out of scope*.)
- Register the `repo_push` **webhook** pointing at the Kargo receiver URL. The
  controller **reads** that URL from the referenced ProjectConfig's
  `status.webhookReceivers[].url` (the value Kargo derives from its own receiver
  Secret and publishes asynchronously) — it is not a render-time input. The Kargo
  receiver Secret (key `secret`) **stays owned by the ProjectConfig** and is the
  Kargo side of the contract; it is *not* copied into the Quay webhook
  registration. The hard-to-guess URL is itself the shared secret on the
  notification path, so the Quay webhook simply POSTs to it — exactly what the
  [`my-project` scaffold](../../holos/components/my-project/buildplan.cue) and
  [oci-publish-workflow.md](../../holos/docs/oci-publish-workflow.md) describe an
  operator registering by hand. If the receiver URL is not yet published, the
  reconciler sets `WebhookRegistered=False` and requeues rather than guessing.

**Idempotency and generate-once.** Reconciles converge to the declared state
without duplicating objects: create-or-claim for orgs (per the ownership model
above), create-or-adopt for repos/teams, upsert for retention and webhook, and
**create-if-absent** for the Secret material the controller itself owns — the two
pull-credential Secrets minted from the one Quay pull-robot token (the Argo CD
repository Secret in `argocd` and the Kargo image-credential Secret in the project
namespace), plus the push-credential Secret minted from the push-robot token. The
Kargo *receiver* Secret is owned by the ProjectConfig, not by these reconcilers.
Status carries `observedGeneration` and
`conditions` so the desired/observed gap is legible, consistent with the
platform's generate-once secret posture
([secret-handling.md](../../holos/docs/secret-handling.md)).

### Replacing the manual provisioning

These two CRDs replace the by-hand provisioning the
[credentials runbook](../runbooks/quay-resource-controller-credentials.md) and the
[`my-project` delivery scaffold](../../holos/components/my-project/buildplan.cue)
document as deferred. Each deferred object becomes a reconciled field:

| Manual step today (runbook / `my-project` scaffold) | Reconciled by |
| --- | --- |
| Create the Quay **org** (e.g. `my-project`) | `Organization` (`organizationName`) |
| Create the **`my-project-config` repo** | `Repository` (`repositoryName`) |
| Create the Quay **pull robot** + the Argo CD repository pull-credential **Secret** in `argocd` ([argocd-application-source.md](../../holos/docs/argocd-application-source.md)) | `Repository` reconcile (status `pullRobot`) |
| Create the **Kargo image-credential Secret** (`kargo.akuity.io/cred-type: image`) in the project namespace so the Warehouse can pull the private repo ([oci-publish-workflow.md](../../holos/docs/oci-publish-workflow.md)) | `Repository` reconcile (from the same pull-robot token) |
| Provision the **push robot** + push-credential Secret for `scripts/publish` (the destination push credential [oci-publish-workflow.md](../../holos/docs/oci-publish-workflow.md) defers to the future controller) | `Repository` reconcile (status `pushRobot`) |
| Register the **`repo_push` webhook** to the Kargo receiver URL ([oci-publish-workflow.md](../../holos/docs/oci-publish-workflow.md)) | `Repository` (`pushWebhook`); reconciler reads the URL from `ProjectConfig.status` |
| Map **OIDC groups → Quay teams/roles** in the org | `Organization` (`access[]`) |

The Kargo-side webhook **receiver** (the `ProjectConfig` `webhookReceivers` block
and its receiver-token Job) stays where it is in the `my-project` component — it
needs no Quay credential. Everything else moves to these CRDs: the org, the repo,
the pull and push robots, the Argo CD repository Secret, the Kargo
image-credential Secret, the push-credential Secret, and the Quay-**side**
registration of the receiver URL — exactly the rows in the table above. Once the
controller ships, the runbook's by-hand steps become the historical record of the
interim, exactly as [ADR-18](ADR-18.md) anticipates.

## Decision

1. The **`quay.holos.run`** API group gains two namespaced CRDs reconciled by the
   Holos Controller ([ADR-18](ADR-18.md)): **Organization** and **Repository**,
   scoped to the in-cluster Quay registry.
2. **Organization** carries: the org name/email and an `adopt` opt-in (a); OIDC
   group → Quay team/role `access[]` bindings (b); an `allowRepositoryCreation`
   toggle (c); and inline `repositories[]` (d) — with a `status` of
   `observedGeneration`, observed org name, and `Ready`/`Synced` conditions.
   Because Quay orgs are a global namespace, the reconciler enforces an
   **ownership/claim model**: it creates and stamps an org it owns, reconciles one
   whose marker names this CR, and treats an unmarked or foreign-owned org as a
   `Conflict` — adopting an unmarked org only when the CR sets `adopt: true`. It
   never silently seizes a foreign org.
3. **Repository** carries: an `organizationRef` (resolved to the owning
   Organization's `spec.organizationName`) and repository name (a); `retention`
   (b); and a `pushWebhook` that names a Kargo `ProjectConfig` receiver (c) — the
   reconciler reads the receiver URL from `ProjectConfig.status` and registers a
   `repo_push` webhook against it. Status carries `observedGeneration`, observed
   repo path, the provisioned `pullRobot` and `pushRobot`, the resolved
   `webhookURL`, and `Ready`/`WebhookRegistered` conditions. Fields beyond the
   docker-push-to-deploy goal are **out of scope** (see above).
4. The reconcilers call the **Quay REST API** using the superuser
   OAuth-Application token from [ADR-15](ADR-15.md) Rev 4–5 / the
   [credentials runbook](../runbooks/quay-resource-controller-credentials.md)
   (`super:user`/`org:admin`/`repo:create`, `FEATURE_SUPERUSERS_FULL_ACCESS` for
   adoption), are **idempotent** (create-or-claim for orgs, create-or-adopt for
   repos/teams, upsert), and treat the Secret material the controller owns — the
   two pull-credential Secrets from one Quay pull-robot token (the Argo CD
   repository Secret in `argocd` and the Kargo image-credential Secret in the
   project namespace) and the push-credential Secret from the push-robot token —
   as **generate-once / create-if-absent** per
   [secret-handling.md](../../holos/docs/secret-handling.md). The Kargo *receiver*
   Secret stays owned by the `ProjectConfig`, not by these reconcilers.
5. These CRDs **replace** the manual `repo_push` webhook + pull-robot + push-robot
   + Argo CD repository-Secret + Kargo image-credential-Secret + org/repo
   provisioning the credentials runbook and the `my-project` scaffold defer (the
   push credential [oci-publish-workflow.md](../../holos/docs/oci-publish-workflow.md)
   defers to this controller); the Kargo receiver (and its receiver Secret) stays
   in the `my-project` component.
6. This is a **design record only** — no CRD Go types or controller code are
   written in this phase.

## Consequences

- **The manual Quay data-plane provisioning is retired** once the controller
  reconciles these CRDs. [ADR-15](ADR-15.md) Revisions 4–5 and the
  [credentials runbook](../runbooks/quay-resource-controller-credentials.md)
  remain operative until then and become the historical record afterward; this
  ADR's `Updates: ADR-15` records that supersession.
- **The controller's superuser credential is load-bearing, and full access cuts
  both ways.** Org/repo/robot/webhook reconciliation all flow through the single
  `svc-quay-resource-controller` OAuth-Application token, and
  `FEATURE_SUPERUSERS_FULL_ACCESS` gives it instance-wide write across *every*
  Quay org. That reach is what makes the ownership/claim model mandatory rather
  than optional: without the claim check, a namespaced `Organization` could seize
  a foreign org. The credential's scope and lifetime are a security-sensitive
  dependency carried over from ADR-15; the controller reads it from the `quay`
  namespace Secret and never commits it.
- **One pull-robot token feeds two runtime Secrets; the webhook carries no copied
  secret.** From a single Quay pull-robot token the Repository reconciler creates
  *both* the Argo CD repository Secret (in `argocd`, so the repo-server can pull)
  *and* the Kargo image-credential Secret (`kargo.akuity.io/cred-type: image`, in
  the project namespace, so the Warehouse can discover Freight on the private
  repo) — both following the generate-once guardrail
  ([secret-handling.md](../../holos/docs/secret-handling.md)); rotation is a
  deliberate, separate operation because in-flight pulls and Warehouse discovery
  depend on stability. The `repo_push` webhook is registered against the
  hard-to-guess Kargo receiver URL (read from `ProjectConfig.status`) — the
  receiver Secret stays owned by the ProjectConfig and is never copied into Quay.
- **Minimal schema is a maintenance choice.** Keeping spec/status to the
  docker-push-to-deploy surface keeps the first CRDs reviewable and avoids
  modeling Quay features the platform does not use; the cost is that genuinely new
  requirements (extra webhook events, mirroring, per-repo permissions) reopen this
  ADR rather than slotting into pre-built fields.
- **Foundation for the Keycloak group (ADR-20).** The `access[]` model here keys
  on the same `groups`-claim group names the Keycloak CRDs (ADR-20) will own;
  the ownership boundary between the controller's Keycloak group and the existing
  `keycloak-config-cli` reconciliation is ADR-20's to settle, as ADR-18 notes.
