# Quay API Group (`quay.holos.run`): Organization and Repository CRDs

| Metadata | Value                          |
| -------- | ------------------------------ |
| Date     | 2026-06-17                     |
| Author   | @jeffmccune                    |
| Status   | `Implemented`                  |
| Tags     | api, controller, quay, registry |
| Updates  | ADR-15                         |

| Revision | Date       | Author      | Info           |
| -------- | ---------- | ----------- | -------------- |
| 1        | 2026-06-17 | @jeffmccune | Initial design |
| 2        | 2026-06-18 | @jeffmccune | Reconcile to the **as-built** `quay.holos.run/v1alpha1` schema shipped across HOL-1309..HOL-1313 (AC #10, authoritative). The implemented Organization/Repository specs are narrower than Revision 1's illustrative design: no inline `repositories[]` (AC #9), no `access[]`/`allowRepositoryCreation`, no `retention`/robot fields; the webhook is an inline `url`/`urlSecretRef` decoupled from Kargo (AC #8), not a `kargoProjectConfigRef`. Record the **API-group dependency boundary** (AC #7), the `credentialsSecretRef` → `holos-controller-quay-creds` in `holos-controller` credential wiring, and the Gateway-API conditions/reasons actually defined. Status `Proposed` → `Implemented` |
| 3        | 2026-06-18 | @jeffmccune | Document the **durable server-side ownership marker** (HOL-1315): the controller stamps a `<org>+holos-owner` robot whose `description` carries the owning CR's UID and keys create/heal/delete on it (not solely on `status.created`), closing the two HOL-1311 races. Record that the Organization reconciler applies `spec.email` drift via `UpdateOrganization`. |
| 4        | 2026-06-18 | @jeffmccune | Remove `spec.displayName` from the Organization CRD (HOL-1316). Quay 3.17.3 organizations have no display-name/description field, so the value was never programmable; the field is dropped entirely (unreleased — no migration or backwards compatibility). |
| 5        | 2026-06-18 | @jeffmccune | Document the **`caBundle` spec field** (HOL-1320/HOL-1321): both Organization and Repository carry an identical `caBundle []byte` (JSON `caBundle,omitempty`, `+optional`) — a PEM/base64 trust anchor for the in-cluster Quay registry's local-CA serving cert, threaded into the Quay client's TLS `RootCAs`; empty means use the controller pod's system trust store. The shared shape is defined once in `api/quay/v1alpha1/common_types.go` and re-used by both Kinds (the cross-Kind convention ADR-18 Revision 3 states controller-wide). |
| 6        | 2026-06-19 | @jeffmccune | Document **Organization synced teams** ([HOL-1325](https://linear.app/holos-run/issue/HOL-1325), shipped HOL-1326..HOL-1328): the Organization gains `spec.syncedTeams[]` (`name`, `oidcGroup`, `role` ∈ `admin`/`creator`/`member`, optional `repositoryPermission` ∈ `read`/`write`/`admin`) and `status.managedTeams`. Records the **two distinct Quay concepts** (the team *org role* vs. the org *default repository permission*/prototype), the preserved **API-group dependency boundary** (OIDC groups referenced by name as data — no Keycloak import, AC #7), **non-exclusive management** + **adoption-is-an-error** with a future per-team `adopt` escape hatch, the `status.managedTeams` ownership marker and the durable-description **heal rule**, and the **GCP-style primitive-role use case** (owner/editor/viewer). Moves org group→team/role bindings out of *Out of scope* into the implemented design. |

## Context and Problem Statement

The [Holos Controller](ADR-18.md) is the in-cluster controller that fills the
data-plane gaps the upstream Quay and Keycloak operators leave open, so product
engineers get a self-service "docker push to deploy" experience. Its first API
group is **`quay.holos.run`**. This ADR is the design specification for that
group's two custom resources, both scoped to the in-cluster Quay registry: an
**Organization** and a **Repository**.

Before this controller, the Quay data plane a project needs — its organization
and the rendered-manifests repository (and, per a project's wiring, a `repo_push`
webhook that notifies Kargo) — was provisioned **entirely by hand**.
[ADR-15](ADR-15.md) Revisions 4–5 made the Keycloak `holos` realm Quay's sole
identity store (`AUTHENTICATION_TYPE: OIDC`), which removed the local `admin` user
and the headless token-mint path, so an operator follows the
[Quay Resource Controller credentials runbook](../runbooks/quay-resource-controller-credentials.md)
to mint a superuser OAuth-Application credential and click through the rest. The
[`my-project` delivery scaffold](../../holos/components/my-project/buildplan.cue)
documents the surface that procedure must cover: it emits the Kargo control plane
and the Argo CD Application but **explicitly defers** the Quay org, repo, and
`repo_push` webhook registration "to a future Quay Resource Controller." The
**org/repo/webhook** part of that surface is what these CRDs now reconcile; the
robots and pull-credential Secrets remain a manual step (see *Out of scope*),
and minting the controller's superuser credential stays manual by design.

That future controller is the Holos Controller ([ADR-18](ADR-18.md)), now
**shipped** as the `holos-controller` service in this repository. This ADR
specifies the two `quay.holos.run` resources that replace the by-hand
provisioning with reconciled custom resources, **as implemented** across
HOL-1309..HOL-1313. The scope is deliberately narrow: just enough schema to reach
the docker-push-to-deploy goal, not a complete model of Quay's API.

> **Revision 2 reconciliation note (AC #10).** Revision 1 of this ADR was a
> forward-looking design sketch with a richer, illustrative schema (inline
> `repositories[]`, `access[]` group→team bindings, `allowRepositoryCreation`,
> `retention`, provisioned pull/push robots, and a `pushWebhook.kargoProjectConfigRef`
> that read the receiver URL from a Kargo `ProjectConfig.status`). What was
> actually **built** is narrower and is what this revision documents. Per the
> parent issue's authoritative acceptance criteria (HOL-1308 AC #10), the ADRs
> are reconciled against the implementation, which overrides Revision 1's
> sketch and ADR-16/ADR-18's GitOps framing where they conflict (notably AC #7
> below). The deferred-but-not-built fields (org `access[]` bindings, repo
> retention, controller-owned robot Secrets) are out of scope for `v1alpha1`
> and are re-introduced only by **revising this ADR** with a concrete
> requirement, never by speculative schema.

## References

- [ADR-18 — The Holos Controller and the GitOps Rendered-Manifest Delivery
  Model](ADR-18.md): names the controller, its `holos-controller` namespace, and
  the `<group>.holos.run` convention whose first group is `quay.holos.run`. This
  ADR specifies that group's CRDs; ADR-18's forward reference to "ADR-19 — the
  Quay API group CRDs" resolves here. ADR-18 Revision 2 records the same AC #7
  API-group dependency boundary this ADR details.
- [ADR-15 — Quay↔Keycloak OIDC SSO](ADR-15.md), Revisions 4–5: the identity and
  credential model these reconcilers run within — `AUTHENTICATION_TYPE: OIDC`, the
  two superusers (`svc-quay-resource-controller`, `quay-admin`), and
  `FEATURE_SUPERUSERS_FULL_ACCESS` that lets the controller adopt orgs it did not
  create. This ADR **updates** ADR-15: the manual data-plane stop-gap ADR-15
  defers becomes these reconciled resources.
- [Quay Resource Controller credentials runbook](../runbooks/quay-resource-controller-credentials.md):
  the manual procedure that mints the superuser OAuth-Application token the
  controller authenticates with (OAuth Application in the `platform-automation`
  org, scopes including `super:user`/`org:admin`/`repo:create`). The token is
  stored as the `holos-controller-quay-creds` Secret in the `holos-controller`
  namespace (keys `url`/`token`/optional `username`) — the credential the
  resources' `credentialsSecretRef` resolves.
- [`docs/runbooks/holos-controller.md`](../runbooks/holos-controller.md): how to
  wire the controller to that credential Secret and the assumption that a
  **superuser-account** OAuth-Application token authenticates all
  controller-managed Quay operations (AC #3).
- [ADR-12 — Repository Layout for Multiple Go Services](ADR-12.md), Revision 6:
  records the second binary/image (`cmd/holos-controller`, `Dockerfile.controller`)
  and the conventional-kubebuilder `main.go` carve-out from the Fisk CLI
  guardrail (ADR-17) the controller manager process is built with.
- [ADR-8 — Container registry and image tagging](ADR-8.md): the registry these
  CRDs provision orgs and repositories in.
- [ADR-16 — Kargo-Driven Promotion](ADR-16.md): the promotion pipeline a
  Repository's `repo_push` webhook feeds — a push notifies a Kargo `Warehouse`.
  **Boundary (AC #7):** the `quay.holos.run` *API group* takes no Kargo
  dependency; the webhook URL is delivered to the Repository as an opaque
  inline `url` or via a `urlSecretRef`, so Kargo's hard-to-guess receiver URL
  can be carried without importing a Kargo type.

## Design

Both resources are **namespaced** custom resources in the `quay.holos.run/v1alpha1`
API group, reconciled by the Holos Controller against the in-cluster Quay registry
(`quay.holos.internal`). They model **only** what the docker-push-to-deploy goal
requires. The schemas below are the **as-built** Go types
(`api/quay/v1alpha1/organization_types.go`, `repository_types.go`,
`common_types.go`); the YAML is illustrative of those types.

### API-group dependency boundary (AC #7)

This is the load-bearing structural decision and the authoritative refinement of
the parent issue's AC #7 (overriding ADR-16/ADR-18's GitOps framing per AC #10):

- **The `quay.holos.run` API group (`api/quay/v1alpha1`) imports no Kargo or
  Argo CD types.** Its only external dependencies are `k8s.io/api` /
  `k8s.io/apimachinery` (for `metav1`) — the CRs reach Quay **solely** through the
  credential named by `credentialsSecretRef` and, for a webhook, the URL named
  inline or by `urlSecretRef`. There is no `import` of a Kargo `ProjectConfig`, an
  Argo CD `Application`, or any Quay client type in the API package. A Repository
  that wires a Kargo receiver does so by carrying that receiver's URL as data
  (a Secret reference), never by referencing a Kargo object.
- **The controller binary may depend on Kargo/Argo CD** if a future need arises;
  that dependency lives in `cmd/holos-controller` / `internal/controller`, never
  in `api/quay/...`. Keeping the API package free of those imports keeps the CRD
  types extractable into their own module and keeps the data-plane CRs legible
  and reusable independent of the delivery pipeline.

This boundary is why the implemented webhook is a plain `url`/`urlSecretRef`
(below) rather than Revision 1's `kargoProjectConfigRef` — the latter would have
coupled the API group to Kargo.

### Credential resolution (`credentialsSecretRef`)

Each resource's spec carries an optional `credentialsSecretRef` (a
`SecretReference`: `name`, optional `key`). It names the Secret holding the Quay
**superuser OAuth-Application** credential the reconciler authenticates with:

- It **defaults to `holos-controller-quay-creds`** when omitted
  (`DefaultCredentialsSecretName`, `+kubebuilder:default`).
- The controller resolves it in **its own namespace** — `holos-controller`, read
  from the `POD_NAMESPACE` downward-API env (default `holos-controller`), not the
  resource's namespace — so one operator-managed credential serves every tenant
  resource.
- The Secret carries keys **`url`** (Quay API base URL) and **`token`** (the
  superuser OAuth token), plus an optional **`username`** (informational — the
  identity the token acts as). `SecretReference.key`, when set, narrows the
  token lookup to a specific key; `url`/`username` always use the conventional
  keys.
- The credential is created at runtime by the
  [`scripts/apply-svc-quay-resource-controller-creds`](../../scripts/apply-svc-quay-resource-controller-creds)
  helper and **never committed** (the runtime-secret guardrail,
  [secret-handling.md](../../holos/docs/secret-handling.md)). When the Secret or a
  required key is missing the reconciler sets `Programmed`/`Ready` `False`
  (reason `CredentialsNotFound`) and requeues.

### CA bundle (`caBundle`) — a standardized, cross-Kind field

The in-cluster Quay registry (`quay.holos.internal`) serves TLS with a
certificate signed by the platform's **per-cluster mkcert local CA**, not a
public root. That CA is not in the controller pod's system trust store, so the
reconcilers' first attempt to reach Quay failed with
`x509: certificate signed by unknown authority` (HOL-1319). The fix
(HOL-1320) is a spec-supplied trust anchor: an optional **`caBundle`** field
that the controller appends to its trust roots when establishing TLS to Quay.

**Both Kinds carry the identical field**, and its semantics and serialization
are described **once** and re-used — not redefined per Kind:

- The shared convention is documented in
  [`api/quay/v1alpha1/common_types.go`](../../api/quay/v1alpha1/common_types.go)
  (the *CABundle convention* doc block); each spec's field godoc refers back to
  it rather than restating the format.
- `OrganizationSpec.CABundle` is in
  [`api/quay/v1alpha1/organization_types.go`](../../api/quay/v1alpha1/organization_types.go);
  `RepositorySpec.CABundle` is in
  [`api/quay/v1alpha1/repository_types.go`](../../api/quay/v1alpha1/repository_types.go).
  Both declare `CABundle []byte` with JSON tag `caBundle,omitempty` and
  `+optional`.

**Field shape (the standardized convention).** `caBundle` follows the upstream
Kubernetes `caBundle` convention: a Go `[]byte` holding one or more PEM-encoded
x509 CA certificates concatenated, which marshals to a **single base64 string**
in JSON (the generated CRD property is `type: string, format: byte`). It is
**configuration carried on the spec, not a credential** — the Quay API token
lives in the `credentialsSecretRef` Secret; the CA bundle does not.

| Spec field | Purpose |
| --- | --- |
| `caBundle` | optional PEM/base64 (`[]byte`) bundle of x509 CA certs the controller trusts **in addition to** its system store when reaching the Quay API. Identical on Organization and Repository; shared semantics in `common_types.go`. **Empty/omitted** ⇒ use the controller pod's system trust store unchanged (the historical behavior — the field is purely additive). |

**Consumption (threaded into the Quay client's TLS `RootCAs`).** The reconcilers
pass `spec.caBundle` to the `internal/quay` client, which builds an
`*http.Client` whose transport's `TLSClientConfig.RootCAs` is the system pool
(`x509.SystemCertPool()`, falling back to `x509.NewCertPool()` when it errors or
returns nil) with the bundle appended via `AppendCertsFromPEM`
([`internal/quay/client.go`](../../internal/quay/client.go):
`NewClientWithCABundle` / `httpClientForCABundle`, with `ValidateCABundle`
rejecting a non-empty-but-unparseable bundle up front). An empty bundle yields a
`nil` `*http.Client`, so the Quay client falls back to `NewClient`'s default
(system trust only, default transport) — behavior is unchanged.

This is a **cross-Kind controller convention**, not a one-off: **every** Kind in
the controller's API groups that talks to a TLS endpoint should accept a
`caBundle` field of this same standardized shape. The convention is stated
controller-wide in [ADR-18](ADR-18.md) (Revision 3); this section is the Quay
group's worked instance of it.

### Organization

An `Organization` names and applies a Quay organization. It **does not** inline
repositories (AC #9) and carries no Kargo/Argo CD coupling.

```yaml
apiVersion: quay.holos.run/v1alpha1
kind: Organization
metadata:
  name: my-project
  namespace: my-project
spec:
  # The Quay organization name to create or adopt. Immutable; required.
  # Conventionally set to metadata.name (the controller does not default it).
  name: my-project
  # Quay requires every namespace to have a unique email.
  email: my-project@holos.internal
  # The Quay superuser credential Secret (defaults to holos-controller-quay-creds
  # in the controller's holos-controller namespace).
  credentialsSecretRef:
    name: holos-controller-quay-creds
  # Opt-in to adopting a pre-existing, externally-created org of this name.
  # Default false: an org this CR did not create is a Conflict, never seized.
  adopt: false
  # OIDC-synced Quay teams. Each entry's membership tracks an OIDC groups-claim
  # value; the controller upserts the team, binds syncing to oidcGroup, sets the
  # org role, and (optionally) manages an org default repository permission.
  syncedTeams:
    - name: my-project-owner       # the Quay team name (the +listMapKey)
      oidcGroup: my-project-owner  # the OIDC groups-claim value, by name only
      role: admin                  # team org role: admin | creator | member
    - name: my-project-editor
      oidcGroup: my-project-editor
      role: member
      repositoryPermission: write  # org default repo permission: read|write|admin
    - name: my-project-viewer
      oidcGroup: my-project-viewer
      role: member
      repositoryPermission: read
status:
  observedGeneration: 2
  # Ownership marker: true = this CR created the Quay org; false = adopted.
  created: true
  # The teams this CR created and manages (the team-level analog of `created`).
  managedTeams:
    - my-project-owner
    - my-project-editor
    - my-project-viewer
  conditions:
    - type: Accepted
      status: "True"
      reason: Created
    - type: Programmed
      status: "True"
      reason: Created
    - type: Ready
      status: "True"
      reason: Created
```

| Spec field | Purpose |
| --- | --- |
| `name` | the Quay org to create or adopt; **immutable**, required (no defaulting). |
| `email` | unique namespace email Quay requires. Mutable: the reconciler pushes `spec.email` drift to Quay via `UpdateOrganization` (`PUT /api/v1/organization/{org}`) before marking an owned org Ready. |
| `credentialsSecretRef` | the Quay superuser credential Secret; defaults to `holos-controller-quay-creds` in `holos-controller`. The resource's **only** auth dependency (AC #7). |
| `adopt` | opt-in (default `false`) to take ownership of a pre-existing unmarked org; without it such an org is a `Conflict`. An adopted org is *released*, not deleted, on CR removal. |
| `caBundle` | optional PEM/base64 CA trust anchor for the Quay registry's local-CA serving cert; see *CA bundle* above. Identical shape on both Kinds. |
| `syncedTeams[]` | the OIDC-synced Quay teams this org manages; a keyed list (`+listMapKey=name`). Each entry carries `name`, `oidcGroup`, `role`, and optional `repositoryPermission` — see *Synced teams* below. Empty/omitted ⇒ no managed teams. |

| `syncedTeams[]` entry | Purpose |
| --- | --- |
| `name` | the Quay team name to create and manage within the org; the list's `+listMapKey` (unique per org). Required (`MinLength=1`). |
| `oidcGroup` | the OIDC groups-claim **value** (a plain string) this team's membership is synced from; the reconciler enables Quay team syncing bound to it. Referenced **by name only** — no Keycloak type is imported (AC #7). Required (`MinLength=1`). |
| `role` | the team's **org-level** Quay role — `admin`, `creator`, or `member`. Required, no default (intent is always explicit). This is the team *org role*, distinct from `repositoryPermission` below. |
| `repositoryPermission` | optional org **default repository permission** (a Quay *prototype*) delegating a repo role — `read`, `write`, or `admin` — to this team on repositories **subsequently created** in the org (a Quay prototype applies to new repos, not retroactively to pre-existing ones). A nil pointer ⇒ no default permission managed for this team. |

| Status field | Purpose |
| --- | --- |
| `observedGeneration` | last `spec` generation reconciled. |
| `created` | the durable ownership marker of the claim model: `true` if this CR created the Quay org, `false` if it adopted one. The finalizer deletes the Quay org only when `created: true`. |
| `managedTeams[]` | the Quay team **names** this CR created and manages — the team-level analog of `created`. Underpins non-exclusive management (a team dropped from the spec is de-provisioned only if it appears here) and adoption-is-an-error (a spec team that exists in Quay but is absent here **and** lacks this CR's durable description marker → `TeamConflict`; a team carrying this CR's marker is healed back in, not a conflict). See *Synced teams* below. |
| `conditions[]` | Gateway-API `Accepted`/`Programmed`/`Ready` (see *Status conditions*). |

There is **no** `access[]`, `allowRepositoryCreation`, or inline `repositories[]`
field — but OIDC group→Quay-team/role bindings **are** modeled, via the
`syncedTeams[]` field above (*Synced teams*). Quay's `FEATURE_TEAM_SYNCING`
([ADR-15](ADR-15.md)) keeps each synced team's **membership** tracking the
`groups` claim; the Organization CRD declares **which** teams exist, their org
role, and their optional default repository permission. Repositories are a
separate `Repository` resource (AC #9).

### Synced teams (`spec.syncedTeams`)

An Organization may declare a set of **OIDC-synced Quay teams**. The reconciler,
after the org itself is provisioned (Created or Adopted), drives each entry into
Quay: it upserts the named team with its org role, enables Quay team syncing
bound to the team's `oidcGroup`, and — when `repositoryPermission` is set —
maintains an org default-permission *prototype* delegating that repo role to the
team. The shipped types are
[`SyncedTeam`/`OrganizationSpec.SyncedTeams`/`OrganizationStatus.ManagedTeams`](../../api/quay/v1alpha1/organization_types.go)
and the reconcile logic is in
[`internal/controller/quay/teams.go`](../../internal/controller/quay/teams.go).

#### Two distinct Quay concepts (team org role vs. default repository permission)

`syncedTeams` touches **two** different Quay enums that are easy to conflate (the
original design recollection of "admin/writer/reader" conflated them — neither
enum is `writer`/`reader`):

- **`role` — the team's _org-level_ role** (`OrganizationTeamRole`). The role a
  team holds *within the organization*, mapping to the Quay team `role` field
  (`PUT /api/v1/organization/{org}/team/{team}`). The enum is **`admin` /
  `creator` / `member`**: `admin` is full administrative control of the org,
  `creator` may create repositories, `member` is plain membership with no
  creation or admin rights. Required, no default — the intent is always explicit.
- **`repositoryPermission` — an org _default repository permission_** (a Quay
  *prototype*; `RepositoryRole`). An optional repo role granted to the team on
  repositories **subsequently created** in the org, via an org default-permission
  prototype delegating to the team. A Quay prototype applies to **newly created**
  repositories, not retroactively to ones that already exist. The enum is
  **`read` / `write` / `admin`**: `read` is pull, `write` is pull+push, `admin` is
  full repo control. Omitted (nil) ⇒ no default permission is managed for the team.

The two are independent: `role` is *org governance* (who may administer or create
in the org), `repositoryPermission` is *data-plane access* (what the team can do
to repositories created in the org). A team can be a plain `member` (no org
rights) yet hold a `write` default permission (push to new repos as they are
created), which is exactly the primitive-role model below.

#### API-group dependency boundary (AC #7), reaffirmed

Synced teams **do not** breach the dependency boundary. `oidcGroup` is a plain
group-**name string** — the value of the OIDC `groups` claim — carried as data,
exactly like the webhook `url`. `api/quay/v1alpha1` imports **no** Keycloak,
Kargo, or Argo CD type to express it; the binding works against whatever OIDC
provider Quay is configured with, named only by string. This keeps the API
package extractable and the identity coupling out of the Quay group, consistent
with the *API-group dependency boundary* section above.

#### Non-exclusive management and adoption-is-an-error

Quay teams within an org are a shared namespace — an operator or another tool may
create teams the controller knows nothing about. Management is therefore
**non-exclusive**, gated by the `status.managedTeams` ownership record (the
team-level analog of `status.created`):

- **Non-exclusive (AC #5).** The reconciler manages exactly the teams it created
  — those in `status.managedTeams` plus those it creates this pass — and leaves
  every other team in the org untouched. A team that is neither in the spec nor
  in `status.managedTeams` is ignored. A team **removed from the spec** is
  de-provisioned (disable syncing, delete its default-permission prototype,
  delete the team) **only if it appears in `status.managedTeams`** — never a
  foreign team of the same name.
- **Adoption-is-an-error (AC #6).** A `syncedTeams` entry that names a team which
  **already exists in Quay** but is **not** controller-managed is a **Conflict**:
  the reconciler refuses to modify it, sets `Ready=False` with reason
  `TeamConflict`, and emits a Warning event. It is never silently seized.
  Adoption is implemented as a reconcile-time error *only* — **no API field
  forbids it** — so a future optional per-team `adopt` bool on `SyncedTeam` can
  flip the conflict path to adoption **without an API break**. The schema is
  deliberately free of any required field or validation that adding `adopt` would
  have to change.

This mirrors the org-level claim model: full-access credentials *can* seize a
foreign team, so the controller deliberately refuses to.

#### Ownership tracking (`status.managedTeams`) and the heal rule

`status.managedTeams` is the durable owner record, but a status write can be lost
after a successful Quay create (the team-level analog of the org-marker race in
*Durable server-side ownership marker* above). To survive that, every team the
controller creates also carries a **durable, server-side ownership marker** in
its Quay team **description**: the prefix `managed by holos-controller ` followed
by the owning CR's opaque ownership token (its `metadata.uid`). Because the token
embeds the CR UID, the marker is **resource-specific and unforgeable** — a
manually-created team cannot accidentally carry another CR's marker.

The **heal rule (AC #4):** on reconcile, a Quay team whose description carries
**this CR's** marker is treated as controller-managed and healed back into
`status.managedTeams`, even if a prior status write was lost. A team that exists
but lacks this CR's marker is a `TeamConflict`, never adopted — **even if it
happens to be bound to the desired `oidcGroup`** (binding alone is not proof of
ownership; only the unforgeable marker is). The rule is documented in a code
comment on the reconcile helper.

Org deletion needs no separate team finalizer: when `status.created == true` the
org finalizer deletes the Quay org, which cascades its teams. An **adopted** org
(`created: false`) is released, not deleted, so controller-created teams inside an
adopted org are not individually deleted — an edge case noted in the controller's
code.

#### Use case: GCP-style primitive roles for a logical project

The motivating use case is a **GCP-style primitive-role model** (owner / editor /
viewer) for a logical project. A project `my-project` would have three
OIDC groups — `my-project-owner`, `my-project-editor`, `my-project-viewer`
(future Keycloak-managed; see below) — each mapped to a synced team in the
project's Quay org:

| OIDC group | Synced team `role` | `repositoryPermission` | Effect |
| --- | --- | --- | --- |
| `my-project-owner` | `admin` | — (full via org admin) | administer the org and its repos (org admin reaches all repos) |
| `my-project-editor` | `member` | `write` | push/pull repos created in the org, no org admin |
| `my-project-viewer` | `member` | `read` | pull repos created in the org, read-only |

`owner` gets the org `admin` role (org governance + implicit full repo access);
`editor` and `viewer` are plain org `member`s whose data-plane reach comes from
the org default repository permission — `write` and `read` respectively. This is
the worked example in the Organization YAML above.

The **Keycloak side** of this — the `my-project-{owner,editor,viewer}` groups
themselves, their membership custodians, and the per-project OIDC client/role
model — is **future work**, specified in the proposed
[ADR-20](ADR-20.md) (the Keycloak API group: per-project Client, `owner`/`editor`/`viewer`
Client Roles, and custodian-managed Group creation/membership) and
[ADR-21](ADR-21.md) (the Holos Project/Application components that would emit the
Organization with its `syncedTeams` alongside the Keycloak groups). Until those
land, the OIDC groups are provisioned by hand (or do not yet exist) and the
Organization's `syncedTeams` references them **by name** — which is precisely why
the dependency boundary names them as data: the Quay group does not need the
Keycloak group to exist or be imported to declare the binding.

### Repository

A `Repository` is a single repository within an owning `Organization`, optionally
with a `repo_push` webhook. It references its organization **by CR name**, never
inlined into the Organization (AC #9).

```yaml
apiVersion: quay.holos.run/v1alpha1
kind: Repository
metadata:
  name: my-project-config
  namespace: my-project
spec:
  # The owning Organization CR in this namespace (by metadata.name). The
  # reconciler resolves it to that Organization's spec.name for the Quay path:
  # <Organization.spec.name>/<name>. Immutable.
  organizationRef: my-project
  # The repository name within the org. Immutable.
  name: my-project-config
  visibility: private            # private (default) | public
  description: Rendered manifests for my-project
  # Same Quay credential Secret as Organization (defaults to
  # holos-controller-quay-creds in holos-controller).
  credentialsSecretRef:
    name: holos-controller-quay-creds
  # Optional repo_push webhook. Exactly ONE of url or urlSecretRef.
  webhook:
    # (a) inline URL, OR
    url: https://kargo.holos.internal/webhook/quay/<receiver-id>
    # (b) a Secret in THIS resource's namespace holding the URL (use for the
    # hard-to-guess Kargo receiver URL that must not be committed):
    # urlSecretRef:
    #   name: my-project-quay-webhook-url
    #   key: url
status:
  observedGeneration: 1
  # The resolved <org>/<repo> path, recorded on first create so the finalizer
  # deletes exactly this path even if the owning Organization CR is gone.
  quayRepository: my-project/my-project-config
  conditions:
    - type: Accepted
      status: "True"
      reason: Reconciled
    - type: Programmed
      status: "True"
      reason: Created
    - type: Ready
      status: "True"
      reason: Reconciled
    - type: WebhookConfigured
      status: "True"
      reason: WebhookConfigured
```

| Spec field | Purpose |
| --- | --- |
| `organizationRef` | (AC #9) the owning `Organization` CR in this namespace (by `metadata.name`); resolved to its `spec.name` for the Quay path. **Immutable**. A Repository can only target a Quay org a same-namespace `Organization` has claimed — it never names a Quay org by string (which would bypass the claim/adopt guardrail). |
| `name` | the repository name; full path is `<Organization.spec.name>/<name>`. **Immutable** — `(organizationRef, name)` is the repo's durable identity. |
| `visibility` | `private` (default) or `public`. |
| `description` | optional human-friendly description. |
| `credentialsSecretRef` | the Quay superuser credential Secret; defaults to `holos-controller-quay-creds`. Distinct from the webhook `urlSecretRef`. |
| `webhook` | (AC #8) optional `repo_push` webhook; **exactly one** of inline `url` or `urlSecretRef` (a Secret `name`/`key` in the resource's own namespace holding the URL). A CEL `XValidation` enforces the mutual exclusion at admission. |
| `caBundle` | optional PEM/base64 CA trust anchor for the Quay registry's local-CA serving cert; see *CA bundle* above. Identical shape to the Organization field. |

| Status field | Purpose |
| --- | --- |
| `observedGeneration` | last `spec` generation reconciled. |
| `quayRepository` | the resolved `<org>/<repo>` path, recorded on first create. |
| `conditions[]` | Gateway-API `Accepted`/`Programmed`/`Ready` plus the Repository-only `WebhookConfigured` (see below). |

### Webhook: `url` vs `urlSecretRef` (AC #8)

The Repository's optional `webhook` registers a Quay `repo_push` notification so a
push notifies a downstream receiver (the Kargo `Warehouse`, [ADR-16](ADR-16.md)).
The target URL is supplied **exactly one** of two ways, enforced by a CEL
`+kubebuilder:validation:XValidation` on the `webhook` object:

- **`url`** — an inline literal URL (non-empty), for a URL that is not sensitive.
- **`urlSecretRef`** — a `{name, key}` reference to a Secret **in the
  Repository's own namespace** holding the URL. This is how Kargo's dynamically
  generated, **hard-to-guess** receiver URL is wired without committing it: an
  operator (or, later, a delivery component) stores the URL in a Secret and the
  Repository references it. `urlSecretRef` is deliberately **decoupled from
  Kargo** — it is an opaque URL-bearing Secret, satisfying AC #7. If the
  `urlSecretRef` Secret or key is missing, the reconciler sets
  `WebhookConfigured=False` (reason `WebhookURLNotFound`) and requeues rather
  than guessing.

This replaces Revision 1's `pushWebhook.kargoProjectConfigRef`, which would have
read the receiver URL from a Kargo `ProjectConfig.status` and thereby imported a
Kargo coupling into the API group — forbidden by AC #7.

### Status conditions (Gateway-API model, AC #2)

Both resources report status as a slice of standard `metav1.Condition` following
the Gateway-API convention (`+listType=map`, `+listMapKey=type`, merge-patch on
`type`), plus `observedGeneration`. The condition **types** and **reasons** are
defined once in `internal/controller/quay/conditions.go` (mirroring the constants
on the API types) and shared by both reconcilers:

| Condition type | Meaning |
| --- | --- |
| `Accepted` | the spec was understood and claimed by this resource. |
| `Programmed` | the desired state was written into Quay. |
| `Ready` | the resource is fully provisioned and usable. |
| `WebhookConfigured` | (Repository only) the `repo_push` notification reflects the desired URL; surfaced distinctly so a provisioned-but-webhookless repo is legible from a fully-wired one. |

| Reason | Applies to | Meaning |
| --- | --- | --- |
| `Created` | Org/Repo | newly created in Quay. |
| `Adopted` | Org | a pre-existing Quay org adopted via `spec.adopt`. |
| `Conflict` | Org | a pre-existing, externally-created org of the same name exists and `adopt` was not set; never silently seized (claim model). |
| `TeamConflict` | Org | a `spec.syncedTeams` entry names a pre-existing Quay team this CR did not create (absent from `status.managedTeams` and lacking this CR's team description marker); `Ready=False` with a Warning event. Adoption-is-an-error (AC #6); never silently seized. |
| `Released` | Org | an adopted org released (finalizer dropped without deleting) on CR removal — adoption is non-destructive. |
| `Reconciled` | Repo | the repository is in steady state. |
| `OrganizationNotReady` | Repo | the owning `Organization` is not yet provisioned. |
| `CredentialsNotFound` | Org/Repo | the credential Secret or a required key is missing. |
| `QuayError` | Org/Repo | a Quay API call failed. |
| `WebhookConfigured` / `WebhookNotConfigured` | Repo | the webhook reflects (or intentionally lacks) a target URL. |
| `WebhookURLNotFound` | Repo | the webhook `urlSecretRef` Secret or key is missing. |
| `InvalidWebhook` | Repo | the webhook violates the `url`/`urlSecretRef` mutual-exclusion rule (defense in depth behind the CEL validation). |

### Ownership and the claim model

`Organization` CRs are namespaced, but Quay orgs are a **single, global
namespace**, and the controller's credential carries
`FEATURE_SUPERUSERS_FULL_ACCESS` (instance-wide write). A naive "adopt any
existing org" rule would let an `Organization` in one tenant namespace silently
seize another project's Quay org. The reconciler therefore enforces a **claim**,
recorded by the durable `status.created` ownership marker:

- **Org does not exist** → **create** it and set `created: true` (the clean
  GitOps path; condition reason `Created`).
- **Org exists and this CR created it** (`created: true`) → reconcile normally.
- **Org exists, externally created, and `spec.adopt: true`** → **adopt** it
  (`created: false`, reason `Adopted`); released, not deleted, on CR removal.
- **Org exists, externally created, and `adopt` is unset** → **Conflict**: the
  reconciler refuses to write, sets `Ready=False` reason `Conflict`, and emits an
  event. An externally-created org is never silently seized just because the
  credential *can* write to it.

The finalizer deletes the Quay org only when `created: true`; an adopted org is
released. This bounds the `FEATURE_SUPERUSERS_FULL_ACCESS` blast radius to orgs
the platform created or was explicitly told to adopt.

#### Durable server-side ownership marker

`status.created` lives only on the CR, so two narrow races remain if it is the
*sole* owner record: (a) a successful Quay create whose `created: true`
status-write is lost could let a later reconcile mis-classify the org as foreign
and release (leak) it; (b) a finalizer delete that succeeds but whose finalizer
*removal* fails, racing another actor that recreates the same global org name in
the gap, could let the retried delete destroy the recreated org. To close both,
the reconciler also stamps a **durable, server-side ownership marker on the Quay
org itself** and keys the create/heal/delete decisions on it — the exact
mechanism is an implementation detail of the controller, not API surface:

- The marker is a dedicated robot account, **`<org>+holos-owner`**, whose
  free-text `description` holds an opaque, controller-managed token: the owning
  `Organization` CR's `metadata.uid` (stable for the CR's lifetime, unique across
  CRs, never reused). Quay 3.17.3 organizations expose no label/annotation/
  description field of their own, so a marker robot is the durable per-org record
  available through the standard org API. The marker is stamped immediately after
  a clean create.
- **Create / heal:** an existing org whose marker token matches this CR is owned
  (its `status.created` is healed to `true` if a prior write was lost — closing
  race (a) — rather than released). An existing org with no marker but
  `status.created == true` is re-stamped and kept. An org whose marker holds a
  *different* token is owned by another claim and is a `Conflict`, never seized —
  even with `spec.adopt`.
- **Delete:** before deleting, the finalizer re-reads the marker and deletes the
  Quay org only when it still names this CR; if the marker is absent or holds a
  foreign token (the org was recreated by another actor in the delete gap), it
  **releases** instead of deleting — closing race (b).

Adopted orgs (claimed via `spec.adopt`, `created: false`) are **not** marked:
adoption is non-destructive, the finalizer releases them without touching Quay,
and stamping a marker would wrongly arm them for deletion.

### Out of scope (deliberately)

Quay's API exposes far more than the delivery loop needs, and the `v1alpha1`
schema is intentionally minimal. The following are **not** modeled by these CRDs
in `v1alpha1` — including several fields Revision 1 sketched but that were **not
built** — until a concrete requirement appears:

- The `allowRepositoryCreation` toggle (Revision 1's sketch). OIDC group→Quay-team
  **role and default-permission** bindings are now **in scope** — modeled by
  `spec.syncedTeams` (*Synced teams* above, Revision 6) — while
  `FEATURE_TEAM_SYNCING` ([ADR-15](ADR-15.md)) keeps each synced team's
  *membership* tracking the `groups` claim. Per-team **adoption** of a
  pre-existing externally-created team (a future optional per-team `adopt`) and a
  free-form Revision 1-style `access[]` binding list remain deferred.
- **Inline `repositories[]` on the Organization** — forbidden by AC #9;
  repositories are provisioned **only** by the `Repository` resource.
- **Repository `retention`/auto-prune** policy.
- **Controller-provisioned robots and the credential Secrets minted from them**
  (the Argo CD repository pull Secret, the Kargo image-credential Secret, the
  `scripts/publish` push Secret). The webhook the Repository registers carries no
  copied secret — the hard-to-guess URL is itself the shared secret on the
  notification path.
- Webhook event types other than `repo_push`, and multiple webhooks per
  repository; org-level quotas/billing/proxy-cache; repository mirroring and
  image-security scanning.

A field beyond what docker-push-to-deploy requires is added by **revising this
ADR** with a new requirement, not by speculative schema.

### Reconciler behavior

Both reconcilers translate their CR's `spec` into **Quay REST API** calls
(via the `internal/quay` client, HOL-1310) using the superuser OAuth-Application
token resolved from `credentialsSecretRef`, and are **idempotent** — a
re-reconcile of an unchanged `spec` makes no further changes. They authenticate
with the **superuser** credential described in [ADR-15](ADR-15.md) Revisions 4–5
and the [credentials runbook](../runbooks/quay-resource-controller-credentials.md);
because the token acts as `svc-quay-resource-controller` (a `SUPER_USERS` member)
and `FEATURE_SUPERUSERS_FULL_ACCESS: true` is set, the controller can both create
new orgs and adopt orgs other identities created.

- **Organization reconcile** resolves the credential, then create-or-claims the
  org per the claim model above (create + `created: true`, reconcile an owned
  org, adopt on `spec.adopt`, or `Conflict`). **After** the org is Created or
  Adopted, it reconciles `spec.syncedTeams` non-exclusively (*Synced teams*
  above): upsert each team with its role, bind syncing to `oidcGroup`, manage the
  optional default-permission prototype, heal/track ownership in
  `status.managedTeams`, de-provision teams dropped from the spec, and surface a
  `TeamConflict` (with a Warning event) for a pre-existing unmanaged team. It sets
  `Accepted`/`Programmed`/`Ready` accordingly. A finalizer deletes a created org
  (cascading its teams) or releases an adopted one.
- **Repository reconcile** resolves the credential and the owning `Organization`
  CR (requeuing with `OrganizationNotReady` until the org is provisioned),
  create-or-adopts `<org>/<name>` at `visibility`, applies `description`, and —
  when `webhook` is set — resolves the URL (inline or from `urlSecretRef`) and
  upserts the Quay `repo_push` notification, setting `WebhookConfigured`. A
  finalizer deletes exactly the `status.quayRepository` path.

## Decision

1. The **`quay.holos.run/v1alpha1`** API group has two namespaced CRDs reconciled
   by the Holos Controller ([ADR-18](ADR-18.md)): **Organization** and
   **Repository**, scoped to the in-cluster Quay registry.
2. **The API group (`api/quay/v1alpha1`) imports no Kargo or Argo CD types**
   (AC #7, authoritative per AC #10): the CRs depend **only** on the Quay API
   (reached via the `credentialsSecretRef` credential) and, for a webhook, the
   inline `url` / `urlSecretRef`. The **controller binary** may depend on
   Kargo/Argo CD; the API package must not.
3. **Organization** carries `name` (immutable), `email`,
   `credentialsSecretRef` (defaulting to `holos-controller-quay-creds`), an
   `adopt` opt-in, and `syncedTeams[]` (OIDC-synced Quay teams: `name`,
   `oidcGroup` by name, an org `role` ∈ `admin`/`creator`/`member`, and an
   optional `repositoryPermission` ∈ `read`/`write`/`admin` default permission);
   status carries `observedGeneration`, the `created` ownership marker,
   `managedTeams[]`, and Gateway-API `Accepted`/`Programmed`/`Ready` conditions.
   It has **no** inline repositories (AC #9) or `allowRepositoryCreation`. The
   reconciler enforces an **ownership/claim model** at both org and team level:
   create + stamp `created: true`, reconcile an owned org, adopt an external org
   only on `adopt: true`, treat any other pre-existing org as a `Conflict`; and
   manage synced teams **non-exclusively** (tracked in `status.managedTeams`),
   treating a pre-existing externally-created team named in the spec as a
   `TeamConflict` rather than adopting it.
4. **Repository** carries `organizationRef` (immutable; the owning Organization CR
   by name — repos are provisioned **only** here, never inlined, AC #9), `name`
   (immutable), `visibility`, `description`, `credentialsSecretRef`, and an
   optional `webhook` with **exactly one** of inline `url` or `urlSecretRef`
   (AC #8, CEL-enforced). Status carries `observedGeneration`, the resolved
   `quayRepository` path, and `Accepted`/`Programmed`/`Ready`/`WebhookConfigured`
   conditions.
5. The reconcilers call the **Quay REST API** with the superuser OAuth-Application
   token from `credentialsSecretRef` (defaulting to `holos-controller-quay-creds`
   in `holos-controller`, keys `url`/`token`/optional `username`), are
   **idempotent**, and follow the Gateway-API conditions/reasons defined in
   `internal/controller/quay/conditions.go`.
6. Fields beyond the docker-push-to-deploy goal — org `access[]` bindings,
   repository `retention`, controller-provisioned robots and their Secrets — are
   **out of scope** for `v1alpha1` and are re-introduced only by **revising this
   ADR**.

## Consequences

- **The manual Quay org/repo/webhook provisioning is retired** for objects these
  CRDs cover once the controller reconciles them. [ADR-15](ADR-15.md) Revisions
  4–5 and the
  [credentials runbook](../runbooks/quay-resource-controller-credentials.md)
  remain operative for **minting the controller's credential** and become the
  historical record for the data-plane steps the CRDs now automate; this ADR's
  `Updates: ADR-15` records that supersession.
- **The controller's superuser credential is load-bearing, and full access cuts
  both ways.** Org/repo/webhook reconciliation all flow through the single
  `svc-quay-resource-controller` OAuth-Application token resolved from
  `holos-controller-quay-creds`, and `FEATURE_SUPERUSERS_FULL_ACCESS` gives it
  instance-wide write. That reach is what makes the ownership/claim model
  mandatory: without the claim check, a namespaced `Organization` could seize a
  foreign org. The credential is read from the `holos-controller` namespace and
  never committed.
- **The API group stays free of pipeline coupling.** Because `api/quay/...`
  imports no Kargo/Argo CD types and the webhook carries an opaque URL, the CRD
  types remain extractable into their own module and the data-plane CRs are
  legible independent of the delivery pipeline — at the cost of the Repository
  not "knowing" about Kargo (the URL must be supplied as data).
- **Minimal schema is a maintenance choice.** Keeping `v1alpha1` to the
  docker-push-to-deploy surface keeps the first CRDs reviewable; the cost is that
  genuinely new requirements (org access bindings, retention, robots, extra
  webhook events) reopen this ADR rather than slotting into pre-built fields.
- **Foundation for the Keycloak group (ADR-20) and the delivery components
  (ADR-21).** The Quay CRDs are the registry half of the per-project data plane
  the Project/Application components (ADR-21) emit; the Keycloak group (ADR-20)
  is the identity half. `spec.syncedTeams` is the Quay-side landing pad for the
  **GCP-style primitive roles** (*Synced teams* above): it already maps
  owner/editor/viewer **by OIDC group name**, so once ADR-20's Keycloak group and
  custodian CRDs provision `my-project-{owner,editor,viewer}`, no Quay-side API
  change is needed to bind them — the two halves meet at the group-name string.
