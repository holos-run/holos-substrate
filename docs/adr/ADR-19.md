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
  for the robot tokens and webhook secrets they manage.

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

  # (c) May org members create repositories on demand (a Quay org-level toggle)?
  # false keeps repository creation declarative — only Repository CRs make repos.
  allowRepositoryCreation: false

  # (d) Repositories to create within the org. Each entry is the minimal inline
  # form; a standalone Repository CR (below) is the equivalent for richer config.
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
| `access[]` | (b) OIDC group → Quay team + role bindings; the reconciler creates teams and team→group bindings, `FEATURE_TEAM_SYNCING` (ADR-15) syncs membership. |
| `allowRepositoryCreation` | (c) the org-level "create repos on demand" toggle. |
| `repositories[]` | (d) inline repositories to create within the org. |

| Status field | Purpose |
| --- | --- |
| `observedGeneration` | last `spec` generation reconciled. |
| `organizationName` | the observed Quay org name. |
| `conditions[]` | `Ready` (org provisioned), `Synced` (teams/bindings converged). |

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
  # (a) The owning Organization (same namespace). The repo is created as
  # <organizationRef>/<repositoryName> in Quay.
  organizationRef: my-project
  repositoryName: my-project-config
  visibility: private            # private | public

  # (b) Retention: prune old tags so the rendered-manifests repo does not grow
  # without bound. Maps to Quay's auto-prune policy on the repository.
  retention:
    keepTags: 50                 # keep the N most-recent tags
    # OR keepDays: 30            # keep tags pushed within the last N days

  # (c) A repo_push webhook to a Kargo Warehouse's receiver URL, so a push
  # notifies Kargo to create Freight (ADR-16). The URL is the hard-to-guess
  # receiver URL Kargo publishes on ProjectConfig.status.webhookReceivers[].url;
  # the secret is the Kargo receiver Secret's token.
  pushWebhook:
    url: https://kargo.holos.localhost/webhook/quay/<receiver-id>
    secretRef:
      name: my-project-quay-webhook   # key: secret
status:
  observedGeneration: 1
  repository: my-project/my-project-config
  # The Argo CD pull-credential the reconciler provisioned (robot + repo Secret).
  pullRobot: my-project+argocd
  conditions:
    - type: Ready
      status: "True"
      reason: Provisioned
```

| Spec field | Purpose |
| --- | --- |
| `organizationRef` | (a) the owning `Organization` in this namespace. |
| `repositoryName` | the repository name; full path is `<org>/<repositoryName>`. |
| `visibility` | `private` or `public`. |
| `retention` | (b) auto-prune policy — `keepTags` count or `keepDays` window. |
| `pushWebhook` | (c) `repo_push` webhook to the Kargo Warehouse receiver `url`, authenticated by `secretRef`. |

| Status field | Purpose |
| --- | --- |
| `observedGeneration` | last `spec` generation reconciled. |
| `repository` | the observed `<org>/<repo>` path. |
| `pullRobot` | the robot account backing the Argo CD pull-credential Secret. |
| `conditions[]` | `Ready` once the repo, retention, robot, pull Secret, and webhook are converged. |

### Out of scope (deliberately)

Quay's API exposes far more than the delivery loop needs. The following are
**explicitly out of scope** for these CRDs until a concrete requirement appears —
keeping the schema minimal and goal-driven, per [ADR-18](ADR-18.md):

- Per-repository permissions beyond the org `access[]` team model, repository
  mirroring, and image-security/vulnerability scanning configuration.
- Webhook event types other than `repo_push` (the only event the Kargo loop
  consumes), and multiple webhooks per repository.
- Org-level quotas, billing, and proxy-cache configuration.
- Multiple pull/push robots per repository — one Argo CD **pull** robot per repo
  (plus the push robot the publish workflow uses) is all the loop requires.

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

**Organization reconcile** maps `spec` to these Quay calls (idempotently — adopt
on conflict, never error on already-exists):

- `POST /api/v1/organization/` to create `organizationName` (or adopt it if it
  already exists; `FEATURE_SUPERUSERS_FULL_ACCESS` permits adoption).
- For each `access[]` entry: create the Quay **team** (`PUT
  /api/v1/organization/<org>/team/<team>`) at its `role`, and bind it to the
  Keycloak group (the team→group binding) so `FEATURE_TEAM_SYNCING` keeps
  membership eventually consistent from the `groups` claim (ADR-15).
- Set the org's "create repos on demand" toggle from `allowRepositoryCreation`.
- For each inline `repositories[]` entry, the same repository provisioning the
  Repository reconciler performs.

**Repository reconcile** maps `spec` to:

- `POST /api/v1/repository` (or adopt) to create `<org>/<repositoryName>` at
  `visibility`.
- Apply the `retention` auto-prune policy to the repository.
- Provision the Argo CD **pull robot** and write the repository pull-credential
  **Secret** in the `argocd` namespace — the `holos+robot`-shaped Secret
  ([argocd-application-source.md](../../holos/docs/argocd-application-source.md):
  `username`, robot `password`, `insecure: "true"`) so Argo CD's repo-server can
  pull the OCI artifact. The robot token is **generated once** and the Secret is
  created if absent and left untouched thereafter, per
  [secret-handling.md](../../holos/docs/secret-handling.md) — a rotation would
  break in-flight pulls.
- Register the `repo_push` **webhook** at `pushWebhook.url` authenticated by
  `pushWebhook.secretRef` — the Kargo Warehouse receiver URL Kargo publishes on
  `ProjectConfig.status.webhookReceivers[].url`, the value the
  [`my-project` scaffold](../../holos/components/my-project/buildplan.cue)
  currently has an operator register by hand.

**Idempotency and generate-once.** Reconciles converge to the declared state
without duplicating objects: create-or-adopt for orgs/repos/teams, upsert for
retention and webhook, and **create-if-absent** for any Secret material (robot
tokens, the webhook secret). Status carries `observedGeneration` and `conditions`
so the desired/observed gap is legible, consistent with the platform's
generate-once secret posture ([secret-handling.md](../../holos/docs/secret-handling.md)).

### Replacing the manual provisioning

These two CRDs replace the by-hand provisioning the
[credentials runbook](../runbooks/quay-resource-controller-credentials.md) and the
[`my-project` delivery scaffold](../../holos/components/my-project/buildplan.cue)
document as deferred. Each deferred object becomes a reconciled field:

| Manual step today (runbook / `my-project` scaffold) | Reconciled by |
| --- | --- |
| Create the Quay **org** (e.g. `my-project`) | `Organization` (`organizationName`) |
| Create the **`my-project-config` repo** | `Repository` (`repositoryName`) |
| Create the Argo CD **pull robot** + the repository pull-credential **Secret** in `argocd` ([argocd-application-source.md](../../holos/docs/argocd-application-source.md)) | `Repository` reconcile (status `pullRobot`) |
| Register the **`repo_push` webhook** to the Kargo receiver URL ([oci-publish-workflow.md](../../holos/docs/oci-publish-workflow.md)) | `Repository` (`pushWebhook`) |
| Map **OIDC groups → Quay teams/roles** in the org | `Organization` (`access[]`) |

The Kargo-side webhook **receiver** (the `ProjectConfig` `webhookReceivers` block
and its receiver-token Job) stays where it is in the `my-project` component — it
needs no Quay credential. Only the Quay-**side** registration of that receiver URL
(plus the org, repo, robot, and pull Secret) moves to these CRDs. Once the
controller ships, the runbook's by-hand steps become the historical record of the
interim, exactly as [ADR-18](ADR-18.md) anticipates.

## Decision

1. The **`quay.holos.run`** API group gains two namespaced CRDs reconciled by the
   Holos Controller ([ADR-18](ADR-18.md)): **Organization** and **Repository**,
   scoped to the in-cluster Quay registry.
2. **Organization** carries: the org name/email (a); OIDC group → Quay team/role
   `access[]` bindings (b); an `allowRepositoryCreation` toggle (c); and inline
   `repositories[]` (d) — with a `status` of `observedGeneration`, observed org
   name, and `Ready`/`Synced` conditions.
3. **Repository** carries: an `organizationRef` and repository name (a);
   `retention` (b); and a `repo_push` `pushWebhook` to a Kargo Warehouse receiver
   (c) — with a `status` of `observedGeneration`, observed repo path, the
   provisioned `pullRobot`, and a `Ready` condition. Fields beyond the
   docker-push-to-deploy goal are **out of scope** (see above).
4. The reconcilers call the **Quay REST API** using the superuser
   OAuth-Application token from [ADR-15](ADR-15.md) Rev 4–5 / the
   [credentials runbook](../runbooks/quay-resource-controller-credentials.md)
   (`super:user`/`org:admin`/`repo:create`, `FEATURE_SUPERUSERS_FULL_ACCESS` for
   adoption), are **idempotent** (create-or-adopt, upsert), and treat all Secret
   material as **generate-once / create-if-absent** per
   [secret-handling.md](../../holos/docs/secret-handling.md).
5. These CRDs **replace** the manual `repo_push` webhook + pull-robot + Argo CD
   repository-Secret + org/repo provisioning the credentials runbook and the
   `my-project` scaffold defer; the Kargo receiver stays in the `my-project`
   component.
6. This is a **design record only** — no CRD Go types or controller code are
   written in this phase.

## Consequences

- **The manual Quay data-plane provisioning is retired** once the controller
  reconciles these CRDs. [ADR-15](ADR-15.md) Revisions 4–5 and the
  [credentials runbook](../runbooks/quay-resource-controller-credentials.md)
  remain operative until then and become the historical record afterward; this
  ADR's `Updates: ADR-15` records that supersession.
- **The controller's superuser credential is load-bearing.** Org/repo/robot/
  webhook reconciliation all flow through the single
  `svc-quay-resource-controller` OAuth-Application token (with
  `FEATURE_SUPERUSERS_FULL_ACCESS` for adoption). Its scope and lifetime are a
  security-sensitive dependency carried over from ADR-15; the controller reads it
  from the `quay` namespace Secret and never commits it.
- **New robot tokens and webhook secrets are generated at runtime.** The
  Repository reconciler creates the Argo CD pull-credential Secret and registers
  the webhook secret following the generate-once guardrail
  ([secret-handling.md](../../holos/docs/secret-handling.md)); rotation is a
  deliberate, separate operation because the receiver URL and in-flight pulls
  depend on stability.
- **Minimal schema is a maintenance choice.** Keeping spec/status to the
  docker-push-to-deploy surface keeps the first CRDs reviewable and avoids
  modeling Quay features the platform does not use; the cost is that genuinely new
  requirements (extra webhook events, mirroring, per-repo permissions) reopen this
  ADR rather than slotting into pre-built fields.
- **Foundation for the Keycloak group (ADR-20).** The `access[]` model here keys
  on the same `groups`-claim group names the Keycloak CRDs (ADR-20) will own;
  the ownership boundary between the controller's Keycloak group and the existing
  `keycloak-config-cli` reconciliation is ADR-20's to settle, as ADR-18 notes.
