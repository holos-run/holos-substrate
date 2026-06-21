# Keycloak API Group (`keycloak.holos.run`): KeycloakInstance, Client, Group, User, and Roles

| Metadata | Value                              |
| -------- | ---------------------------------- |
| Date     | 2026-06-17                         |
| Author   | @jeffmccune                        |
| Status   | `Partially Implemented`            |
| Tags     | api, controller, keycloak, oidc, rbac |
| Updates  | ADR-3                              |

| Revision | Date       | Author      | Info           |
| -------- | ---------- | ----------- | -------------- |
| 1        | 2026-06-17 | @jeffmccune | Initial design — four illustrative Kinds (`KeycloakClient`, `KeycloakClientRole`, `KeycloakRealmRole`, `KeycloakGroup`), the ownership boundary vs `keycloak-config-cli`, reserved-name + claim enforcement, and a list of open questions; schemas explicitly "illustrative, not final" |
| 2        | 2026-06-20 | @jeffmccune | Make the `## Design` concrete for the **project group-management use case**. Add the centrally-managed **`KeycloakInstance`** reference Kind (API URL, `caBundle`, admin `credentialsSecretRef`, `realm`; multiple instances per cluster; in/out-of/remote-cluster targets) and the **`KeycloakUser`** Kind (Admin-API pre-create-if-necessary + first-broker-login auto-link by email). Resolve the open questions: the **claim value** comes from a **client-role** assignment surfaced by the existing `oidc-usermodel-client-role-mapper` (rejecting the full-path Group Membership mapper and a script mapper); **nested groups** `projects/<project>/{roles,custodians}/{owner,editor,viewer}` are idiomatic in Keycloak 26.x; **custodian delegation** uses Fine-Grained Admin Permissions v2 `manage-members`/`manage-membership` group scope (KC ≥ 26.2). Every Kind references a `KeycloakInstance` and a cross-namespace reference is authorized by a `security.holos.run` `ReferenceGrant` ([ADR-22](ADR-22.md)). State the **API-group dependency boundary** (`api/keycloak/v1alpha1` imports only `k8s.io/api`/`k8s.io/apimachinery`), the **`holos_controller` metrics**, and the **Gateway-API status** contract. Reconciled by the existing `holos-controller` binary as a second API group alongside `quay.holos.run`. Keep `Status: Proposed`, `Updates: ADR-3` |
| 4        | 2026-06-21 | @jeffmccune | **Close the "Quay use case" gap (HOL-1350).** Reconcile the implemented `clientRoles` model with this ADR's proposed `clientRoleBindings`/`emitProjectRolesInGroupsClaim` design: `ClientRoleReference` now names its target client by **exactly one of** `clientRef` (a same-namespace `KeycloakClient` CR, the "project's own service" path) **or `clientId`** (a Keycloak clientId directly — the new field, CEL-enforced mutual exclusion). A `KeycloakGroup` confers a **project-prefixed** client role (`my-project-<role>`) on the **platform-reserved Quay client** (`https://quay.holos.localhost`) by naming its `clientId` directly — no tenant `KeycloakClient` CR exists for it (the reserved-name guard still forbids one). The group reconciler **ensures the project-prefixed role exists** on the named client (idempotent create) before assigning, and a **reserved-client guard** in `conferClientRoles` permits only project-prefixed names there — the platform's own reserved client-role names (`platform-admin`/`project-admin`) are refused, so the controller never overwrites a platform role binding and the `quay` **client object** stays config-cli's. The role then surfaces in Quay's `groups` claim via the already-deployed `quay-client-roles` mapper, so [ADR-19](ADR-19.md) `syncedTeams[].oidcGroup` membership populates with **no Quay-side or new-mapper change**. The `KeycloakClient` reserved-name guard (client-object create/reconfigure) is unchanged. The `my-project` role groups now confer the role on **both** the Quay client (`clientId`) and the project client (`clientRef`). `KeycloakClientRole`/`KeycloakRealmRole` remain unimplemented. `Status`/`Updates: ADR-3` unchanged |
| 3        | 2026-06-20 | @jeffmccune | **`Status: Partially Implemented`** — the API group **shipped** (HOL-1344..HOL-1348): the `api/keycloak/v1alpha1` types (`KeycloakInstance`/`Group`/`User`/`Client`), the `internal/keycloak` Admin REST client seam, and the four reconcilers (`internal/controller/keycloak`, claim model + finalizers + conditions + metrics + `ReferenceGrant` gating), with CRDs and RBAC installed by the controller's kustomize tree. Phase 6 (HOL-1348) wired the Holos CUE layer: the controller's **admin credential** (the confidential `svc-holos-controller` service-account client with scoped `realm-management` roles + a generate-once bootstrap into `holos-controller-keycloak-creds`), the realm's **first-broker-login auto-link flow** (`Detect Existing Broker User` + `Automatically Set Existing User`), the central **`KeycloakInstance`** + `security.holos.run` `ReferenceGrant` (the `keycloak-instance` component), and the **`my-project`** project CRs (the role/custodian `KeycloakGroup`s, the owner `KeycloakUser` `bob@example.com`, and the project `KeycloakClient`). **Implementation deviations from this ADR's worked examples:** the shipped `KeycloakGroup.spec.clientRoles[]` / `KeycloakClient.spec.clientRoles[]` (a `{clientRef, role}` referencing a `KeycloakClient` CR by object name) replaced the proposed `KeycloakGroup.clientRoleBindings` (a bare `clientId` list) + `KeycloakClient.emitProjectRolesInGroupsClaim` flag, and the `KeycloakClient` reconciler always ensures its own client-role→`groups` mapper. A consequence: the ADR's **"Quay use case"** (folding `my-project-<role>` into the **platform Quay** client's token with no project client) is **not yet implemented** — the reserved-name guard forbids a tenant `KeycloakClient` targeting `https://quay.holos.localhost` and `clientRef` resolves only a same-namespace CR, so today the `my-project` claim values surface in the **project's own** `https://my-project.holos.localhost` client token (the ADR's "project's own service" path). Folding onto the reserved Quay client is tracked as follow-up. `KeycloakClientRole`/`KeycloakRealmRole` remain unimplemented. `Updates: ADR-3` unchanged |

## Context and Problem Statement

The [Holos Controller](ADR-18.md) is the in-cluster controller that fills the
data-plane gaps the upstream Quay and Keycloak operators leave open, so product
engineers get a self-service "docker push to deploy" experience. Its first API
group — `quay.holos.run` — is specified in [ADR-19](ADR-19.md) and is shipped.
This ADR specifies the **second** group the controller owns: a **Keycloak** API
group (`keycloak.holos.run`) for the per-project, tenant-facing identity
primitives a product engineer needs to self-service.

The concrete, motivating use case is **project group management**. A logical
project `my-project` ([ADR-1](ADR-1.md)) needs the GCP-style primitive-role
triad — `owner` / `editor` / `viewer` — expressed as Keycloak groups whose
membership surfaces, in the shared OIDC `groups` claim, as the values
`my-project-owner` / `my-project-editor` / `my-project-viewer`. Those exact claim
values are what [ADR-19](ADR-19.md)'s `Organization.spec.syncedTeams[].oidcGroup`
already binds to Quay teams **by name**. ADR-19 built the Quay (registry) half of
the primitive-role model and explicitly deferred the **Keycloak side — the groups
themselves, their membership custodians, and the per-project OIDC client/role
model — to this ADR**. This revision makes that side concrete.

Today the `holos` realm — its clients, roles, groups, default group membership,
and protocol mappers — is **fully declarative but platform-owned**: it is
authored in CUE and reconciled on every `scripts/apply` by the
`keycloak-config-cli` Job
([holos/components/keycloak/realm-config/buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue),
[holos/docs/keycloak-clients.md](../../holos/docs/keycloak-clients.md)). That
mechanism is excellent for the platform's *own* realm configuration, but it is
**not** a per-project, KRM-native self-service path: a product engineer cannot
declare "my project needs owner/editor/viewer groups, a custodian who approves
membership, and an OIDC client that carries those groups into its token" as
Kubernetes custom resources and have it reconciled. [ADR-18](ADR-18.md) names this
gap and explicitly leaves the ownership boundary between this Keycloak API group
and the existing `keycloak-config-cli` Job for **this ADR to resolve**.

Revision 1 of this ADR fixed *what* the controller should own and *why* and
sketched an illustrative schema, but deferred a list of open questions and left
the schemas "illustrative, not final." This Revision 2 **resolves those open
questions** from the planning decisions and the supporting web research, makes the
`## Design` concrete for the group-management use case, and adds the two Kinds the
design turned out to need: a centrally-managed **`KeycloakInstance`** (the
connection/credential record for one Keycloak target) and a **`KeycloakUser`** (to
pre-provision and auto-link a person by email). The status stays **`Proposed`**:
this is a design record, **no Go or CUE code and no CRD manifests are written**
here — those land in [HOL-1344](https://linear.app/holos-run/issue/HOL-1344) and
later implementation issues.

## References

- [ADR-18 — The Holos Controller and the GitOps Rendered-Manifest Delivery
  Model](ADR-18.md): names the controller, its `holos-controller` namespace, the
  `<group>.holos.run` API-group convention, the controller-wide `caBundle`
  cross-Kind convention (Revision 3), and the AC #7 API-group dependency boundary.
  ADR-18 states the ownership boundary between this Keycloak group and
  `keycloak-config-cli` is **"a question ADR-20 must resolve"**; this revision
  resolves it concretely and ADR-18 carries the forward cross-reference.
- [ADR-19 — Quay API Group CRDs](ADR-19.md): the sibling first group, **shipped**.
  This Keycloak group **mirrors its conventions** — the `caBundle` cross-Kind
  field, the `credentialsSecretRef` defaulting into the `holos-controller`
  namespace, the Gateway-API status model (`Accepted`/`Programmed`/`Ready`,
  `observedGeneration`), and the durable ownership-marker + claim model. Most
  importantly, ADR-19's `Organization.spec.syncedTeams[].oidcGroup` keys on the
  **group-claim names** this group produces — `my-project-owner` and the rest — so
  the two groups meet at those exact strings (ADR-19 *Use case: GCP-style
  primitive roles*). This ADR is the declarative source of those names.
- [ADR-22 — The `security.holos.run` API Group and `ReferenceGrant`](ADR-22.md):
  fixes the cross-namespace-reference convention. A `keycloak.holos.run`
  `KeycloakClient`/`KeycloakGroup`/`KeycloakUser` (and the role Kinds) in a
  project namespace references a `KeycloakInstance` in a platform namespace; that
  cross-namespace reference is authorized by a `security.holos.run`
  `ReferenceGrant` placed in the **instance (referent) namespace**. This ADR
  **cites** that grant; it does **not** redefine it. ADR-22 also mandates the
  Gateway-API **status contract** every `holos.run` CR (including these) reports.
- [ADR-3 — Authorization via Kubernetes RBAC and Group Membership](ADR-3.md): the
  platform authorizes via Kubernetes RBAC, mapping **group membership** to access
  through `RoleBinding`/`ClusterRoleBinding` subjects of kind `Group`, with
  **custodians** approving membership requests. ADR-3 explicitly treats group
  **provisioning and custodianship** as an *external* prerequisite — "not
  something the platform implements." This ADR **`Updates: ADR-3`** on exactly
  that point: a controller that creates Keycloak groups and delegates
  custodian-approved membership makes the platform the provisioning mechanism for
  the **identity-system side** of ADR-3's groups, rather than assuming an external
  one. ADR-3's authorization *model* is unchanged — RBAC bindings with `Group`
  subjects, membership a custodian approves; this ADR only changes **who
  provisions the groups and runs the approval**.
- [ADR-1 — Project resource](ADR-1.md) and [ADR-21 — Holos Project/Application
  components](ADR-21.md): the logical Project tenant whose `owner`/`editor`/
  `viewer` primitive roles these groups realize, and the (proposed) CUE components
  that would **emit** these Keycloak CRs alongside the Quay
  [ADR-19](ADR-19.md) resources for each project.
- [holos/docs/keycloak-clients.md](../../holos/docs/keycloak-clients.md): the
  conventional declarative OIDC-client pattern — the `keycloak-config-cli`
  reconciliation mechanism and its apply-gate, **public vs confidential PKCE
  clients** (`argocd`/`kargo` public, `quay` confidential), the runtime
  client-secret bootstrap, the **three protocol mappers that feed the shared
  `groups` claim**, and the realm/client role model. The CRD-driven path here must
  not contradict this; it abstracts over the same realm.
- [holos/components/keycloak/realm-config/buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue):
  the authoritative source for how clients, realm roles (`roles.realm`), client
  roles (`roles.client.<clientId>`), groups (`groups`), the `authenticated`
  default group (`defaultGroups: ["/authenticated"]`), realm users, and the three
  `groups`-claim mappers (`oidc-group-membership-mapper` with `full.path: "false"`,
  `oidc-usermodel-realm-role-mapper`, `oidc-usermodel-client-role-mapper`) are
  declared today. The `quay` client's `quay-client-roles` mapper
  (`oidc-usermodel-client-role-mapper`, `usermodel.clientRoleMapping.clientId:
  https://quay.holos.localhost`) is the **precedent for the claim-value mechanism**
  this ADR adopts.
- [holos/components/keycloak/instance/buildplan.cue](../../holos/components/keycloak/instance/buildplan.cue):
  the Keycloak server instance. The operator names its Service `keycloak-service`,
  serving HTTPS on `8443` in the `keycloak` namespace (in-cluster URL
  `https://keycloak-service.keycloak.svc:8443`); the external hostname is `auth.holos.localhost`.
  The operator generates the bootstrap `keycloak-initial-admin` Secret (keys
  `username`/`password`) the config-cli Job authenticates with. The controller
  needs an **analogous, dedicated** admin credential — documented here, not
  implemented.
- [holos/docs/secret-handling.md](../../holos/docs/secret-handling.md): the
  runtime-secret guardrail — secret material is created at runtime (an
  `ExternalSecret` or a generate-once create-if-absent bootstrap Job) and never
  committed. The `KeycloakInstance` admin credential and any confidential
  `KeycloakClient` secret delivered into a project namespace must honor this,
  exactly as the platform's own `quay-oidc` bootstrap does.

### Web research backing the resolved decisions

The open questions are resolved with these findings (validated against Keycloak
26.x, the version line the platform runs — 26.6.3):

- **Native subgroups are idiomatic in Keycloak 26.x.** A group may contain nested
  child groups, addressed by path (`/projects/my-project/roles/owner`). The
  controller models a shallow, fixed hierarchy
  (`projects/<project>/{roles,custodians}/{owner,editor,viewer}`) rather than a
  deep tree — deep nesting is discouraged for performance and legibility, so the
  design keeps it shallow.
- **The Group Membership mapper emits the group *path* or *leaf name* only — it
  cannot synthesize an arbitrary claim value from a path.** With
  `full.path: "false"` the mapper emits the bare leaf (`owner`); with `"true"` it
  emits the full path (`/projects/my-project/roles/owner`). Neither yields the
  desired flat value `my-project-owner`. This is why the claim value is carried by
  a **client role** instead (below).
- **Fine-Grained Admin Permissions v2 (FGAP v2) supports a `manage-members` /
  `manage-membership` permission scoped to a group** (Keycloak ≥ 26.2, May 2025).
  A user granted that scope over a group may add/remove its members **without**
  realm-admin rights — the native mechanism for custodian delegation.
- **First-broker-login auto-link by email** — the `Detect Existing Broker User` +
  `Automatically Set Existing User` authenticators plus the IdP's `Trust Email`
  flag — links a federated login to a **pre-existing** local user with the same
  email instead of creating a duplicate. This is the basis for `KeycloakUser`'s
  pre-provision-then-auto-link behavior.
- **Prior-art CRD operators** (the official `keycloak-realm-operator`, EDP's
  Keycloak operator, RightCrowd's) validate the per-resource-CR-over-Admin-API
  approach this group takes — a Kubernetes CR per Keycloak realm object,
  reconciled through the Keycloak Admin REST API.

## Design

All Kinds below are **namespaced** custom resources in the `keycloak.holos.run/v1alpha1`
API group, reconciled by the existing `holos-controller` binary ([ADR-18](ADR-18.md))
as a **second API group alongside `quay.holos.run`** ([ADR-19](ADR-19.md)) — the
same manager process, a sibling reconciler set, not a new binary. They reach
Keycloak over its **Admin REST API**, authenticated by a per-target credential the
`KeycloakInstance` Kind holds.

The Kinds are: **`KeycloakInstance`** (the connection/credential record for one
Keycloak target), **`KeycloakClient`** (a per-project OIDC client named by its
URL, with the `groups`-claim wiring), **`KeycloakGroup`** (the nested
`roles`/`custodians` group tree and its custodian delegation), **`KeycloakUser`**
(pre-provision-by-email + first-login auto-link), and the role Kinds
**`KeycloakClientRole`** / **`KeycloakRealmRole`** (the client-scoped
`owner`/`editor`/`viewer` triad and the realm-role → client-role mapping). Every
Kind except `KeycloakInstance` carries an **`instanceRef`** naming the
`KeycloakInstance` it reconciles against.

The YAML below is **concrete but still illustrative of the field-level API** — it
fixes the field *shape and semantics*, while the exact field names, optionality,
CEL validation, and printer columns are settled by the CRD-implementation issue
([HOL-1344](https://linear.app/holos-run/issue/HOL-1344)). No Go types
or CRD manifests are written by this ADR.

### API-group dependency boundary (AC #3)

This is the load-bearing structural decision, mirroring [ADR-19](ADR-19.md)'s AC #7
boundary in reverse:

- **`api/keycloak/v1alpha1` imports only `k8s.io/api` and `k8s.io/apimachinery`**
  (for `metav1`). It imports **no** Quay, Kargo, or Argo CD type, and no Keycloak
  client/Go type either — the CRs reach Keycloak **solely** through the credential
  named by a `KeycloakInstance`'s `credentialsSecretRef`. The API package stays
  extractable into its own module and legible independent of any relying party.
- **OIDC group names consumed by Quay remain data referenced by name.** Where
  [ADR-19](ADR-19.md)'s `Organization.spec.syncedTeams[].oidcGroup` is a plain
  string with no Keycloak import, here the relationship is **symmetric in reverse**:
  this group produces the `my-project-owner` claim value, and the Quay
  Organization consumes it **by name only**. `api/keycloak/v1alpha1` takes **no**
  dependency on `api/quay/v1alpha1`, and `api/quay/v1alpha1` takes none on
  `api/keycloak/v1alpha1`. The two groups meet only at the **group-name string**
  carried in the `groups` claim — never at a Go import.
- **The controller binary may depend on more than the API packages do.** Any
  cross-group coordination lives in `cmd/holos-controller` / `internal/controller`,
  never in `api/keycloak/...`, exactly as [ADR-19](ADR-19.md) confines Quay's
  pipeline coupling to the binary.

### `KeycloakInstance` — the centrally-managed connection record (AC #4)

A `KeycloakInstance` holds everything the controller needs to reach **one**
Keycloak target and authenticate to its Admin API. It is **centrally managed** —
created by a platform owner in a platform namespace (e.g. `keycloak`), not by
tenants — and is the single object every other `keycloak.holos.run` Kind
references.

**The name.** `KeycloakInstance` (not `KeycloakTarget`, `KeycloakConnection`, or
`KeycloakServer`) is chosen because the object models exactly one **running
Keycloak instance + the realm the controller operates within it**: it parallels
the platform's own [Keycloak instance component](../../holos/components/keycloak/instance/buildplan.cue)
(which renders the running server) and reads naturally at the reference site
(`instanceRef: holos-keycloak`). "Server" would over-narrow it to the process;
"Connection"/"Target" would under-state that it also pins the realm. The Kind is a
**reference record**, akin to a kubeconfig context: connection coordinates plus a
credential plus the realm selector.

```yaml
apiVersion: keycloak.holos.run/v1alpha1
kind: KeycloakInstance
metadata:
  name: holos-keycloak
  namespace: keycloak            # a platform namespace; centrally managed
spec:
  # The Keycloak Admin API base URL (AC #4.7). In-cluster this is the operator's
  # Service, https://keycloak-service.keycloak.svc:8443; an out-of-cluster or remote-cluster
  # target is any reachable https URL (AC #4.2, #4.3).
  apiURL: https://keycloak-service.keycloak.svc:8443
  # The realm this instance operates within (AC #4). The controller reconciles
  # objects into THIS realm; multiple KeycloakInstances may target the same
  # server with different realms, or different servers entirely.
  realm: holos
  # PEM/base64 CA trust anchor for the target's serving cert, the controller-wide
  # cross-Kind caBundle convention (ADR-18 Rev 3 / ADR-19 Rev 5). Trusted IN
  # ADDITION TO the pod's system store; empty/omitted ⇒ system store unchanged
  # (AC #4.6). The in-cluster Keycloak serves a local-CA-signed cert not in the
  # pod's system store, so this carries the per-cluster local-ca PEM.
  caBundle: <base64 PEM bundle>
  # The Keycloak admin credential the controller authenticates with. A Secret in
  # the controller's holos-controller namespace by default (mirrors ADR-19's
  # credentialsSecretRef). See "Admin credential" below for the recommended auth.
  credentialsSecretRef:
    name: holos-controller-keycloak-creds
status:
  observedGeneration: 1
  conditions:
    - type: Accepted
      status: "True"
      reason: Validated
    - type: Programmed
      status: "True"
      reason: Reachable        # admin auth + realm resolution succeeded
    - type: Ready
      status: "True"
      reason: Reachable
```

| Spec field | Purpose |
| --- | --- |
| `apiURL` | the Keycloak Admin API base URL (AC #4.7). In-cluster: `https://keycloak-service.keycloak.svc:8443`; out-of-cluster / remote-cluster: any reachable `https` URL (AC #4.2, #4.3). Required. |
| `realm` | the realm the controller operates within for objects referencing this instance. Required. Lets two instances target the same server but different realms. |
| `caBundle` | optional PEM/base64 (`[]byte`) bundle of x509 CA certs trusted **in addition to** the controller pod's system store when reaching `apiURL` — the standardized cross-Kind field (ADR-18 Rev 3 / ADR-19 Rev 5), shared shape and semantics with `quay.holos.run`. Empty/omitted ⇒ system store unchanged (AC #4.6). |
| `credentialsSecretRef` | a `SecretReference` to the Keycloak **admin** credential. Resolved in the **`holos-controller` namespace** by default (the ADR-19 convention, read from `POD_NAMESPACE`), so one operator-managed credential per instance serves every tenant CR that references the instance. See *Admin credential* below. |

**Multiple instances per cluster (AC #4.2), and any target location (AC #4.3).**
Because a `KeycloakInstance` is a plain namespaced CR carrying its own `apiURL` +
credential + realm, a cluster may hold **several** — e.g. a `pre-prod-keycloak`
and a `prod-keycloak`, or one per realm. The `apiURL` may name an **in-cluster**
Service (`https://keycloak-service.keycloak.svc:8443`), an **out-of-cluster** public endpoint,
or a Keycloak in a **remote cluster** — the controller cares only that the URL is
reachable and the credential authenticates; nothing in the design assumes the
target is co-located.

**Admin credential.** The `credentialsSecretRef` Secret carries the credential the
controller uses for the Keycloak Admin REST API. The bootstrap
`keycloak-initial-admin` Secret the operator mints for `keycloak-config-cli` is
**not** reused — the controller gets its own, least-privileged, dedicated
credential. Two auth shapes are recommended, in order of preference:

1. **A confidential service-account client with `realm-management` roles**
   (preferred). A dedicated OIDC client in the realm with *Service Accounts
   Enabled* and the specific `realm-management` client roles the controller needs
   (`manage-clients`, `manage-users`, `query-groups`/`manage-realm` as scoped to
   the operations below — **not** blanket realm-admin). The Secret carries the
   client ID + client secret; the controller does a `client_credentials` grant.
   This is machine-identity-shaped, rotatable, and scoped.
2. **A realm user with `realm-management` roles** (fallback). A dedicated admin
   user (username + password in the Secret) holding the same `realm-management`
   roles, used with the Admin CLI / direct-grant flow. Simpler to bootstrap but a
   password rather than a client credential.

Either way the credential is **created at runtime and never committed** (the
runtime-secret guardrail, [secret-handling.md](../../holos/docs/secret-handling.md)),
like the platform's `quay-oidc` and the controller's
`holos-controller-quay-creds`. When the Secret or a required key is missing the
reconciler sets `Programmed`/`Ready` `False` (reason `CredentialsNotFound`) and
requeues.

### Every Kind references a `KeycloakInstance`, gated by a `ReferenceGrant` (AC #4.4, #4.5)

Every `keycloak.holos.run` Kind except `KeycloakInstance` itself carries an
**`instanceRef`** — the `KeycloakInstance` it reconciles against:

```yaml
spec:
  instanceRef:
    name: holos-keycloak
    namespace: keycloak          # cross-namespace ⇒ needs a ReferenceGrant
```

A tenant CR lives in the **project namespace** while the `KeycloakInstance` lives
in a **platform namespace** — a **cross-namespace reference**. Per the guard rail
([ADR-22](ADR-22.md)), that reference is authorized by a `security.holos.run`
`ReferenceGrant` placed **in the instance's (referent) namespace**, declaring
`from` the project namespace's `keycloak.holos.run` Kinds and `to` the
`KeycloakInstance`. A `KeycloakClient`/`Group`/`User` whose `instanceRef` crosses
a namespace boundary with **no matching grant** is **rejected** by its reconciler
(`Ready=False`, reason `RefNotPermitted`), never silently honored — the same
default-deny posture ADR-22 fixes. This ADR **cites** that grant and does **not**
redefine it; ADR-22 owns the grant's shape. (A same-namespace `instanceRef` — e.g.
a platform-owned CR in the `keycloak` namespace — needs no grant.)

### `KeycloakGroup` — the nested role/custodian group tree (AC #5)

A `KeycloakGroup` manages a project's primitive-role groups as a **shallow nested
group tree** under a per-project parent, plus the **custodian** groups that
delegate membership management:

```text
projects/
  <project>/
    roles/
      owner
      editor
      viewer
    custodians/
      owner
      editor
      viewer
```

`projects/<project>/roles/{owner,editor,viewer}` are the groups a person is a
**member** of to hold the corresponding primitive role.
`projects/<project>/custodians/{owner,editor,viewer}` are the groups whose members
**manage the membership** of the matching `roles/*` group (a custodian of
`custodians/owner` adds/removes members of `roles/owner`).

```yaml
apiVersion: keycloak.holos.run/v1alpha1
kind: KeycloakGroup
metadata:
  name: my-project-roles
  namespace: my-project
spec:
  instanceRef:
    name: holos-keycloak
    namespace: keycloak
  # The logical project this group tree belongs to. The controller creates the
  # shallow nested tree projects/<project>/{roles,custodians}/{owner,editor,viewer}.
  project: my-project
  # The primitive roles to provision (the GCP triad).
  roles: [owner, editor, viewer]
  # AUTHORITATIVE owner of the claim-value binding: which consumer client(s) carry
  # the my-project-<role> client role for each role group, so membership surfaces
  # as the flat groups-claim value in that consumer's token (the mapper is
  # per-client; see "claim value" below). The Quay client https://quay.holos.localhost
  # is the consumer for the ADR-19 syncedTeams use case — and it needs NO project
  # KeycloakClient at all, which is why this binding lives on KeycloakGroup (the
  # owner of the role groups), not on KeycloakClient. List one or more entries;
  # each names its target client by EXACTLY ONE of clientId (a Keycloak clientId
  # directly — used for the reserved Quay client, project-prefixed roles only) or
  # clientRef (a same-namespace KeycloakClient CR — a project's own client).
  #
  # IMPLEMENTED SHAPE (HOL-1347/HOL-1350): the field is spec.clientRoles[], a list
  # of {clientId|clientRef, role} — it replaced this ADR's earlier proposed
  # KeycloakGroup.clientRoleBindings (a bare clientId list). The example below uses
  # the implemented field.
  clientRoles:
    - clientId: https://quay.holos.localhost   # the ADR-19 syncedTeams consumer
      role: my-project-owner                   # project-prefixed; reserved on this client
  # Custodian delegation: members of custodians/<role> manage membership of
  # roles/<role>, via FGAP v2 manage-members/manage-membership scoped to the
  # roles/<role> group. "controller" is the fallback mechanism (see below).
  custodianDelegation: fgap-v2     # fgap-v2 | controller
status:
  observedGeneration: 1
  # The group paths this CR created and owns (the claim/ownership marker).
  managedGroups:
    - /projects/my-project/roles/owner
    - /projects/my-project/roles/editor
    - /projects/my-project/roles/viewer
    - /projects/my-project/custodians/owner
    - /projects/my-project/custodians/editor
    - /projects/my-project/custodians/viewer
  conditions:
    - type: Accepted
      status: "True"
      reason: Reconciled
    - type: Programmed
      status: "True"
      reason: Created
    - type: Ready
      status: "True"
      reason: Created
```

**Nested-groups decision.** Keycloak 26.x treats native subgroups as idiomatic, so
the group tree is modeled as real nested groups (`/projects/<project>/roles/owner`)
rather than flat, name-mangled groups (`projects-my-project-roles-owner`). The
tree is kept **shallow** (three levels: `projects` → `<project>` →
`{roles,custodians}` → leaf) because deep nesting hurts performance and
legibility; the web research confirms shallow nesting is the recommended idiom.
The `authenticated` flat default group ([realm-config buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue))
is platform-owned and **untouched** — the nested project tree is additive.

**The bare-leaf-name caveat — why the claim value comes from the client role, not
the group name.** Each client's existing `oidc-group-membership-mapper` runs with
`full.path: "false"`
([realm-config buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue)),
which emits the **bare leaf** of *every* group a user belongs to. A member of
`/projects/my-project/roles/owner` therefore also gets a generic **`owner`** value
in the `groups` claim — which would collide across projects if relying parties keyed
on it. This is precisely why the collision-safe primitive-role value is **not** the
group name but the **client role `my-project-owner`** (the *Claim value* section
below): consumers ([ADR-19](ADR-19.md) `syncedTeams[].oidcGroup`, Argo CD RBAC) key
on the **project-prefixed client-role value**, never the bare leaf. The bare leaf
(`owner`/`editor`/`viewer`) is an **accepted, ignored byproduct** of the existing
group-membership mapper — it carries no authority because nothing the platform
binds keys on it. (A future tightening could scope or drop the project subtree from
the group-membership mapper, but that is not required for correctness and is left
out of this design.)

**Custodian delegation — FGAP v2 group scope.** The custodian mechanism is
**Fine-Grained Admin Permissions v2** (`manage-members` / `manage-membership`
permission scoped to a group; Keycloak ≥ 26.2, the platform runs 26.6.3): the
controller grants `custodians/<role>`'s members the `manage-members` scope **over**
`roles/<role>`, so a custodian can add/remove members of the role group **without**
realm-admin rights, directly in Keycloak's account/admin console. This is the
native, in-Keycloak realization of [ADR-3](ADR-3.md)'s custodian-approved
membership — the controller provisions the delegation; the human custodian
performs the approval.

- **Controller-layer alternative.** Where FGAP v2 group scope is unavailable or
  the platform prefers an audit trail in Kubernetes, `custodianDelegation:
  controller` instead keeps membership management in the controller: a future
  membership-request CR (or a list on the group) is reconciled into Keycloak
  group membership, with the controller (not Keycloak FGAP) enforcing that only a
  `custodians/<role>` member may approve. The CR shape for that path is deferred
  to its own issue; `fgap-v2` is the default because it needs no extra CR and
  uses Keycloak's own permission model.

This is the change that advances the `Updates: ADR-3` boundary: ADR-3's
authorization *model* (RBAC bindings with `Group` subjects, membership a custodian
approves) is unchanged; this ADR makes the platform **provision** the Keycloak
groups and **delegate** the custodian approval rather than assuming an external
identity system does.

### Claim value via a client role — the resolved mechanism (AC #5)

The use case requires that membership in `projects/<project>/roles/owner` surface
in the shared `groups` claim as the **flat value** `my-project-owner` (likewise
editor/viewer), because that is the string [ADR-19](ADR-19.md)'s Quay
`syncedTeams[].oidcGroup` binds to. Keycloak's **Group Membership mapper cannot
synthesize that value**: with `full.path: "false"` it emits the leaf (`owner`);
with `"true"` it emits the path (`/projects/my-project/roles/owner`); neither is
`my-project-owner`.

**Decision — carry the value as a client role on the *client whose token must
carry it*.** The `oidc-usermodel-client-role-mapper` is **per client**: it folds
into the `groups` claim only the roles of the **one** client named by its
`usermodel.clientRoleMapping.clientId`. The platform's precedent mapper —
`quay-client-roles` — is scoped to `usermodel.clientRoleMapping.clientId:
https://quay.holos.localhost`
([realm-config buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue)),
so it emits **only the Quay client's** client roles into Quay's token. A client
role on a *different* (project) client would surface in **that** client's token,
**not** in Quay's. The mechanism must therefore assign the role on the **client
whose token the consumer reads**:

The **authoritative declaration** of which consumer client each role binds on is a
single field — **`KeycloakGroup.clientRoleBindings`** (it lists one or more
consumer `clientId`s), owned by the `KeycloakGroup` because the group owns the role
groups. A `KeycloakClient` does **not** own this binding; it only opts **its own**
token in via `emitProjectRolesInGroupsClaim` (ensuring its own mapper). This keeps
one owner for the binding even when, as in the Quay case, **no project
`KeycloakClient` exists at all**:

- **For the Quay use case** (`syncedTeams[].oidcGroup` reads Quay's token): the
  `KeycloakGroup` lists `clientId: https://quay.holos.localhost` in its
  `clientRoles` (the implemented field; see the example above), so each
  `roles/<role>` group is assigned a **client role `my-project-<role>` on the Quay
  client** — the client the existing `quay-client-roles` mapper already serves. A
  member of `roles/owner` thereby holds the `my-project-owner` Quay-client role
  (via Keycloak's group → role assignment), and the already-deployed
  `quay-client-roles` mapper emits `my-project-owner` into Quay's `groups` claim
  with **no Quay-side or new-mapper change** and **no project `KeycloakClient`**.
  This is the join the "no Quay-side change" consequence rests on, and it is
  **implemented as of HOL-1350** (Revision 4): the group reconciler resolves the
  named `clientId`, ensures the project-prefixed role on it, and assigns it without
  seizing the client object.
- **For a project's own service** (its token must carry its own role): the
  `KeycloakGroup` lists that project `KeycloakClient`'s `clientId` in
  `clientRoleBindings`, and the `KeycloakClient` sets
  `emitProjectRolesInGroupsClaim: true` so its reconciler ensures an
  `oidc-usermodel-client-role-mapper` scoped to **its own** `clientId` is present
  (the `quay-client-roles` shape, retargeted) and the role surfaces in **that**
  client's token.

The group is the join point; the client role is the claim value; **which client**
the role lives on is dictated by **which client's token must carry it** — assigning
it on the wrong client is exactly the mistake the per-client mapper scope makes
easy. (Where the platform `quay` client is the consumer, the controller assigns a
*Quay-client* role, which means the controller touches a client-role namespace on
the platform-owned `quay` client. That client role is itself a controller-claimed,
project-prefixed name `my-project-<role>` — distinct from the **reserved**
platform Quay client roles `platform-admin`/`project-admin` — and is governed by
the same per-CR claim model in *Ownership / disjointness* below; the `quay`
*client* object stays config-cli's, only project-prefixed client roles on it are
controller-claimed.)

**Rejected alternatives.**

- **Full-path Group Membership mapper (`full.path: "true"`).** Emits
  `/projects/my-project/roles/owner`, not `my-project-owner` — the consuming Quay
  team would have to bind to the full path, and the platform's relying parties
  (Argo CD RBAC, Quay team sync) already key on bare, flat names. Rejected: wrong
  value shape, and it would fork the claim convention.
- **Script mapper.** A JavaScript protocol mapper *could* compute
  `my-project-owner` from the path, but script mappers are **disabled by default**
  in Keycloak (require the `scripts` feature / a deployed provider), are an
  operational and security liability (arbitrary code in the token pipeline), and
  duplicate logic the client-role mapper already provides. Rejected: avoidable
  attack surface and operational burden for no gain over the client-role path.

The client-role mechanism reuses an **already-deployed mapper** with no new realm
feature, which is why it is preferred.

### `KeycloakClient` — the per-project OIDC client named by its URL (AC #5)

A `KeycloakClient` manages one project OIDC client and the `groups`-claim wiring
that carries the project's role groups into that client's tokens. The client is
**named by its URL** — its `clientId` is the service URL (e.g.
`https://quay.holos.localhost`), matching the platform's own convention where the
real Quay `clientId` **is** `https://quay.holos.localhost`
([realm-config buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue),
`QUAY_CLIENT_ID`).

```yaml
apiVersion: keycloak.holos.run/v1alpha1
kind: KeycloakClient
metadata:
  name: my-app
  namespace: my-project
spec:
  instanceRef:
    name: holos-keycloak
    namespace: keycloak
  # The Keycloak clientId — the service URL (the platform convention; the real
  # quay clientId is itself https://quay.holos.localhost).
  clientId: https://my-app.holos.localhost
  # public (SPA/CLI, PKCE S256, no secret) | confidential (delivered secret).
  # Mirrors the argocd/kargo (public) vs quay (confidential) distinction in
  # keycloak-clients.md. PKCE S256 is the default; relax only per that guardrail.
  type: confidential
  redirectUris:
    - https://my-app.holos.localhost/oauth2/callback
  webOrigins:
    - https://my-app.holos.localhost
  # Opt THIS client's token into carrying project role values: when true the
  # reconciler ensures an oidc-usermodel-client-role-mapper scoped to THIS clientId
  # is present, so any my-project-<role> client role assigned on this client (by a
  # KeycloakGroup.clientRoleBindings entry naming this clientId) surfaces in this
  # client's groups claim. This is only the mapper wiring on this client — the
  # AUTHORITATIVE binding of which role lives on which consumer client is owned by
  # KeycloakGroup.clientRoleBindings (see "claim value" above), NOT here, because
  # the ADR-19 Quay consumer needs no project KeycloakClient at all.
  emitProjectRolesInGroupsClaim: true
  # For a confidential client, where to deliver the generated secret — a
  # generate-once, create-if-absent Secret per secret-handling.md, never committed.
  secretRef:
    name: my-app-oidc
    key: client_secret
status:
  observedGeneration: 1
  conditions:
    - type: Accepted
      status: "True"
      reason: Reconciled
    - type: Programmed
      status: "True"
      reason: Created
    - type: Ready
      status: "True"
      reason: Created
    - type: SecretDelivered      # confidential clients only
      status: "True"
      reason: SecretDelivered
```

The `KeycloakClient` reconciler creates the client; when
`emitProjectRolesInGroupsClaim` is set it ensures an
`oidc-usermodel-client-role-mapper` scoped to **this** `clientId` is present (the
`quay-client-roles` precedent) so project roles assigned on this client surface in
its token — but it does **not** own which roles bind where (that is
`KeycloakGroup.clientRoleBindings`). For `type: confidential` it delivers the
generated client secret into the project namespace as runtime-created,
never-committed material ([secret-handling.md](../../holos/docs/secret-handling.md)),
mirroring the platform's `quay-oidc` bootstrap.

### `KeycloakClientRole` and `KeycloakRealmRole` (AC #5)

A `KeycloakClientRole` is a single client role scoped to one client; a
`KeycloakRealmRole` carries a realm role and the **realm-role → client-role**
mapping (a Keycloak composite role) that lets a broad organizational role compose
down onto a service. These are unchanged in intent from Revision 1, now bound to a
`KeycloakInstance` and made concrete.

**Single owner of the primitive-role client roles.** To avoid two Kinds claiming
the same client role, ownership is **disjoint by construction**: the
`my-project-<role>` client roles that back the project group-claim model — the
`owner`/`editor`/`viewer` triad on the consumer client — are **created and claimed
solely by `KeycloakGroup`** (it creates each role on every `clientRoleBindings`
client and assigns it to the matching `roles/<role>` group, tracking it in
`status`). A `KeycloakClientRole` is the **standalone** Kind for a client role that
is **not** part of a group→claim binding (an ad-hoc, directly-granted role); it
must **not** re-declare a role a `KeycloakGroup` owns (doing so is a `Conflict`
under the same per-CR claim model). The two never co-own a role: the group owns the
primitive triad it surfaces in the claim; `KeycloakClientRole` owns roles outside
that flow.

```yaml
apiVersion: keycloak.holos.run/v1alpha1
kind: KeycloakClientRole
metadata:
  name: my-app-editor
  namespace: my-project
spec:
  instanceRef: {name: holos-keycloak, namespace: keycloak}
  clientRef: my-app             # the KeycloakClient this role is scoped to
  role: editor                  # owner | editor | viewer (the primitive triad)
---
apiVersion: keycloak.holos.run/v1alpha1
kind: KeycloakRealmRole
metadata:
  name: core-services-developer
  namespace: my-project
spec:
  instanceRef: {name: holos-keycloak, namespace: keycloak}
  realmRole: core-services-developer
  # Composite mapping: a person who carries this realm role thereby holds the
  # named client roles (e.g. "my-app editor"), without being named on the client
  # directly. Realized as a Keycloak composite role.
  mapsTo:
    - clientRef: my-app
      role: editor
```

Per the *single owner* rule above, the `my-project-<role>` triad is owned by
`KeycloakGroup`; the standalone `KeycloakClientRole` is **only** for ad-hoc,
non-group role grants outside that flow, and `KeycloakRealmRole` is for the
cross-service "carries a broad role" case. The composite realm-role → client-role
mapping is a **Keycloak composite role** (not a protocol-mapper change), so it
composes with — does not fork — the existing realm-role mapper that folds
realm-role names into `groups`.

### `KeycloakUser` — pre-provision by email + first-login auto-link (AC #5)

A `KeycloakUser` pre-provisions a person **by email** *only if necessary* (e.g. to
assign group membership before that person's first login) and assigns that
person's **group membership**. It does **not** itself configure the realm or IdP:
the **first-login auto-link** that links the federated login to the pre-created
record (rather than creating a duplicate) is **platform realm/IdP configuration**
(the `KeycloakRealmImport` CR's identity-provider/flow fields, **not** the
`keycloak-config-cli` Job — see *What the platform must provide* below) and the CR
**assumes is present**. The CR's surface is the per-user pre-create + membership;
the auto-link behavior is a documented prerequisite, not CR state:

```yaml
apiVersion: keycloak.holos.run/v1alpha1
kind: KeycloakUser
metadata:
  name: bob
  namespace: my-project
spec:
  instanceRef:
    name: holos-keycloak
    namespace: keycloak
  # The user's email — the identity key for pre-create AND auto-link.
  email: bob@example.com
  # Pre-create the local Keycloak user only if one with this email does not
  # already exist (Admin-API create-if-absent). Lets the platform assign group
  # membership before Bob's first login.
  provision: ifNecessary        # ifNecessary | never
  # Group memberships to assign (the role groups from KeycloakGroup). These bind
  # Bob to the project primitive roles ahead of his first login.
  groups:
    - /projects/my-project/roles/editor
status:
  observedGeneration: 1
  conditions:
    - type: Accepted
      status: "True"
      reason: Reconciled
    - type: Programmed
      status: "True"
      reason: Created            # or Skipped when the user already existed
    - type: Ready
      status: "True"
      reason: Reconciled
```

**What the CR owns.** The `KeycloakUser` reconciler does the **Admin-API
pre-create-if-absent** (a local user with the given email), and assigns the listed
**group memberships**. That is the per-user, per-project surface a tenant declares.

**What the platform realm/IdP config must provide.** The **auto-link behavior is
realm-level first-broker-login flow + identity-provider configuration**, which
stays platform-owned, not per-user CR state. Crucially, it is **not** owned by the
`keycloak-config-cli` Job: that Job imports `realm: "holos"` **only** and
deliberately carries **no `identity-provider` (or `enabled`) fields**, which are
owned by the **`KeycloakRealmImport` CR** in the
[instance component](../../holos/components/keycloak/instance/buildplan.cue) to
avoid contention between the two reconciliation paths
([keycloak-clients.md](../../holos/docs/keycloak-clients.md), the *Keycloak
Configuration as Code* guard rail). So the realm's first-broker-login flow
(**`Detect Existing Broker User`** + **`Automatically Set Existing User`**) and the
IdP's **`Trust Email`** setting are configured by the **platform realm/IdP
definition (the `KeycloakRealmImport` CR for IdP/flow fields, the realm component's
flows)** — so that when Bob first authenticates through the federated IdP, Keycloak
matches his email to the pre-created record and **links** it instead of creating a
second user. The `KeycloakUser` CR **assumes** this realm/IdP configuration is
present (a documented prerequisite); it does **not** reconcile the
first-broker-login flow or the IdP itself.

**Security tradeoff of email-based auto-link.** Auto-linking on email trusts the
IdP's assertion that the email is verified and owned by the authenticating user —
`Trust Email` **bypasses** Keycloak's own email-verification step. If the upstream
IdP does **not** verify email ownership, an attacker who can assert a victim's
email at that IdP could be auto-linked to the victim's pre-provisioned record (an
account-takeover vector). The mechanism is therefore safe **only** when the
federated IdP is trusted to verify email ownership; the realm config and the
runbook must state that prerequisite explicitly. `provision: ifNecessary` (only
pre-create when needed, never blindly) and assigning memberships narrowly limit
the blast radius, but the trust assumption is intrinsic to email-based auto-link.

### Ownership / disjointness vs `keycloak-config-cli` (AC #6)

These CRDs are reconciled by the **existing `holos-controller` binary** as a
**separate API group alongside `quay.holos.run`** — additive to, and **disjoint
from**, the existing `keycloak-config-cli` Job that owns the platform's own realm.
The division of ownership and the disjointness *enforcement* generalize Revision
1's reserved-name + claim discussion into a concrete model:

- **The platform keeps owning its own realm.** The platform clients, the platform
  realm roles, the shared `groups`-claim mappers, the `authenticated` default
  group, and the seeded superuser users remain `keycloak-config-cli`'s; the
  **identity-provider and first-broker-login / IdP flow config** (the auto-link
  prerequisite above) is the **`KeycloakRealmImport` CR's** (config-cli imports
  `realm: "holos"` only, with no `identity-provider`/`enabled` fields, to avoid
  contention — see *KeycloakUser* above). The CRDs do **not** redeclare or fight
  over any of these. config-cli's managed-import behavior is **no-delete**: realm
  objects it does not declare are left untouched.
- **The controller owns per-project, tenant-facing objects** reconciled from the
  CRDs above: a project's OIDC clients, its `projects/<project>/{roles,custodians}`
  group tree, its client/realm roles, and its pre-provisioned users.

Keycloak realm objects are a **single global namespace** (one realm has one set of
clients, roles, groups) while these CRDs are **Kubernetes-namespaced** and admit
arbitrary identifiers — so a tenant CR could name a platform object or collide
with another project. `keycloak-config-cli`'s no-delete behavior is **necessary
but not sufficient**: it stops the Job from deleting CR-created objects, but does
nothing to stop a CR from **overwriting** a platform or foreign object. Two
mechanisms enforce disjointness:

- **Reserved platform names.** A CR targeting a platform-owned identifier is
  **rejected** (`Ready=False`, reason `Reserved`), never reconciled. The reserved
  set is keyed on the **actual realm identifiers** the CRDs match against — not
  colloquial names:
  - **client IDs**: `argocd`, `kargo`, **`https://quay.holos.localhost`** (the
    real Quay `clientId`, `QUAY_CLIENT_ID`), and the legacy disabled `quay` client
    ID — reserving the display string `quay` alone would miss the real client and
    leave a bypass;
  - **realm roles**: `platform-owner`, `platform-editor`, `platform-viewer`;
  - **client roles on a platform client**: the `quay` client's own
    `platform-admin`/`project-admin` (a controller-claimed *project-prefixed*
    client role like `my-project-owner` on that same `quay` client is permitted —
    only the platform's own client-role *names* are reserved, so the claim-value
    mechanism above can assign project roles on the consumer client without
    colliding with platform roles);
  - **groups**: `authenticated` (the realm default group);
  - **users**: the seeded superusers `svc-quay-resource-controller` and
    `quay-admin`.
  This list tracks [realm-config buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue);
  keeping the reserved set in sync with the platform realm config is itself a
  guard rail the implementation issue must wire (e.g. a generated constant), not a
  hand-maintained copy that can drift.
- **A durable per-CR ownership / claim model**, mirroring [ADR-19](ADR-19.md)'s
  Organization claim and its durable server-side marker. The controller stamps a
  durable owner record on each realm object it creates — naming the owning CR
  (its `metadata.uid`) in the object's own free-text metadata where one exists
  (a group/client **attribute**, a role **description**) — and keys
  create/heal/delete on it. On reconcile it acts only on an object it created (or
  one whose marker names this CR), **heals** its `status.managedGroups` record if
  a status write was lost, and treats an unmarked or foreign-owned object as a
  **`Conflict`** (`Ready=False`) rather than seizing it. A deterministic
  project-name prefix (`my-project-<role>` client roles, `projects/<project>/...`
  group paths) reduces collisions, but the **claim record — not the prefix — is
  what makes the boundary safe**, exactly as in ADR-19.

Whether the controller eventually **supersedes** config-cli (folding even the
platform's own realm into CRDs) is left as future work; the boundary fixed now is
**disjoint ownership with reserved-name + claim enforcement**. Until a controller
ships these CRDs, **`keycloak-config-cli` remains the sole owner** of all realm
clients, roles, groups, and users.

### Metrics (AC #7)

The controller exposes Keycloak metrics on the **same Prometheus `/metrics`
endpoint** the manager already serves, under the **`holos_controller`** namespace,
**consistent with the existing `quay` pattern**
([internal/controller/quay/metrics.go](../../internal/controller/quay/metrics.go)):

- a **per-Kind reconcile counter** labeled by `kind` (`keycloakinstance` /
  `keycloakclient` / `keycloakgroup` / `keycloakuser` / `keycloakclientrole` /
  `keycloakrealmrole`) and `outcome` (`success` / `error`); and
- a **Keycloak Admin-API request counter** —
  `holos_controller_keycloak_api_requests_total` labeled by `operation` (a fixed,
  low-cardinality set of logical Admin-API verbs: `get_client`, `create_client`,
  `upsert_group`, `assign_group_role`, `get_user`, `create_user`,
  `add_group_member`, …) and `outcome` — the Keycloak analog of the existing
  `quay_api_requests_total`.

**Registration — share the reconcile collector, do not re-register it.** The
existing `holos_controller_reconcile_total` collector is defined **once** as a
package-private `CounterVec` in `internal/controller/quay/metrics.go` and
registered into controller-runtime's `metrics.Registry` via that package's `init`.
A second package (the Keycloak controller) **must not** define and register another
collector with the **same** `Namespace`/`Name` — Prometheus `MustRegister` panics
on a duplicate, which is exactly the failure mode the `quay` package's own comment
calls out. The implementation issue therefore **promotes the cross-Kind reconcile
counter into a shared controller metrics package** (e.g. `internal/controller/metrics`)
that **both** the `quay` and `keycloak` reconcilers import and increment with their
own `kind` label, registered there exactly once. The Keycloak-specific
`keycloak_api_requests_total` (a distinct metric name, no collision) stays in the
Keycloak package and registers via its own `init`, mirroring Quay's
`quay_api_requests_total`. Label cardinality stays bounded (kind, operation, and
outcome are all fixed small sets, none derived from user input), and all
collectors register into controller-runtime's `metrics.Registry` so they serve on
the manager's `/metrics` endpoint with no separate wiring in `main.go`.

### Status reporting (AC #8)

**Every Kind** in this group reports rich status following the Gateway-API model
mandated for all `holos.run` CRs ([ADR-22](ADR-22.md) guard rail; the
[ADR-19](ADR-19.md) precedent):

- a **`status.conditions[]`** slice of standard `metav1.Condition` (`+listType=map`,
  `+listMapKey=type`, merge-patch on `type`) with the standard **`Accepted`** (the
  spec was understood and claimed), **`Programmed`** (the desired state was written
  into Keycloak), and **`Ready`** (fully provisioned and usable) types, plus
  Kind-specific extras where useful (e.g. `KeycloakClient`'s `SecretDelivered`,
  analogous to Repository's `WebhookConfigured`);
- a **`status.observedGeneration`** recording the last `spec` generation
  reconciled; and
- at least one **printer column surfacing `Ready`**.

The condition **types** and **reasons** (`Created`, `Adopted`, `Conflict`,
`Reserved`, `RefNotPermitted`, `CredentialsNotFound`, `KeycloakError`, …) are
defined **once** in a shared constants file in the Keycloak controller package
(the analog of `internal/controller/quay/conditions.go`) and shared by every
reconciler, never re-derived per Kind. A denied cross-namespace `instanceRef`
(missing `ReferenceGrant`) surfaces as `Ready=False` reason `RefNotPermitted`,
which is the observability ADR-22's grant model depends on.

## Decision

1. **The existing `holos-controller` binary ([ADR-18](ADR-18.md)) owns a second
   API group, `keycloak.holos.run/v1alpha1`,** reconciled as a **sibling reconciler
   set alongside `quay.holos.run`** ([ADR-19](ADR-19.md)) — not a new binary —
   against the Keycloak Admin REST API. Its Kinds are **`KeycloakInstance`**,
   **`KeycloakClient`**, **`KeycloakGroup`**, **`KeycloakUser`**,
   **`KeycloakClientRole`**, and **`KeycloakRealmRole`**.
2. **`KeycloakInstance` is the centrally-managed connection/credential record** for
   one Keycloak target: `apiURL` (the Admin API URL, in/out-of/remote-cluster),
   `realm`, a `caBundle` (the controller-wide cross-Kind trust-anchor convention),
   and an admin `credentialsSecretRef` defaulting into the `holos-controller`
   namespace (recommended auth: a confidential service-account client with scoped
   `realm-management` roles, or a realm user with the same). **Multiple instances
   per cluster** are supported. The name `KeycloakInstance` is chosen because the
   object models one running instance + the realm the controller operates within.
3. **Every other Kind references a `KeycloakInstance` via `instanceRef`**, and a
   **cross-namespace** `instanceRef` is authorized by a `security.holos.run`
   `ReferenceGrant` in the instance's (referent) namespace
   ([ADR-22](ADR-22.md), cited not redefined); an ungranted cross-namespace
   reference is rejected (`Ready=False`, `RefNotPermitted`), never silently honored.
4. **`KeycloakGroup` manages a shallow nested group tree**
   `projects/<project>/roles/{owner,editor,viewer}` and
   `projects/<project>/custodians/{owner,editor,viewer}` (native subgroups are
   idiomatic in Keycloak 26.x; kept shallow). **Custodian delegation uses FGAP v2
   `manage-members`/`manage-membership` group scope** (Keycloak ≥ 26.2; platform
   runs 26.6.3) so `custodians/<role>` members manage `roles/<role>` membership in
   Keycloak directly, with a `controller`-layer alternative. This is the concrete
   realization of the `Updates: ADR-3` change — the platform now provisions the
   groups and delegates the custodian approval; ADR-3's RBAC authorization model is
   unchanged.
5. **The role-group's `groups`-claim value is carried by a client role on the
   client whose token must carry it**: because the `oidc-usermodel-client-role-mapper`
   is **per client**, each `roles/<role>` group is assigned the `my-project-<role>`
   client role on the **consumer's** client — the **Quay client
   `https://quay.holos.localhost`** for the [ADR-19](ADR-19.md) `syncedTeams` case,
   whose existing `quay-client-roles` mapper already emits client roles into Quay's
   `groups` claim — or on the project's own client when its own token must carry the
   value. Assigning the role on the wrong client (e.g. only on the project client
   when Quay is the consumer) would **not** surface it in Quay's token. The
   **full-path Group Membership mapper** (emits a path, not the flat value) and a
   **script mapper** (disabled by default, an avoidable security/operational
   liability) are **rejected**.
6. **`KeycloakClient` manages a per-project OIDC client named by its URL**
   (`clientId: https://my-app.holos.localhost`, the platform convention), opts its
   own token into carrying project roles via `emitProjectRolesInGroupsClaim` (the
   per-client mapper on **its own** `clientId`) — while the **authoritative
   role→consumer-client binding is `KeycloakGroup.clientRoleBindings`**, a single
   owning field that works even when the consumer (the Quay client) has no project
   `KeycloakClient` — and delivers a confidential client's secret into the
   project namespace as runtime-created, never-committed material.
   **`KeycloakUser` pre-provisions a person by email only-if-necessary** and assigns
   group membership; the **first-login auto-link** (`Detect Existing Broker User` +
   `Automatically Set Existing User` + `Trust Email`) is **platform realm/IdP config
   the `KeycloakRealmImport` CR owns** (the identity-provider/flow fields config-cli
   deliberately does **not** carry), not CR state — with the documented
   email-based-auto-link security tradeoff. `KeycloakClientRole`/`KeycloakRealmRole`
   carry the client-scoped triad and the realm-role → client-role composite.
7. **The API-group dependency boundary holds (AC #3):** `api/keycloak/v1alpha1`
   imports **only** `k8s.io/api` / `k8s.io/apimachinery`; it reaches Keycloak
   solely via the `KeycloakInstance` credential and takes **no** dependency on
   Quay/Kargo/Argo CD or their types. The OIDC group names Quay consumes
   (`syncedTeams`) remain **data referenced by name** — the two groups meet only at
   the claim-name string, preserving [ADR-19](ADR-19.md)'s boundary in reverse.
8. **Disjoint ownership from the platform realm config is enforced**, not assumed:
   `keycloak-config-cli` keeps owning the platform's own realm objects (clients,
   roles, the `authenticated` group, the superusers) and the `KeycloakRealmImport`
   CR owns the identity-provider / first-broker-login flow (config-cli carries no
   `identity-provider` fields); the controller owns per-project objects.
   Enforcement is **reserved platform names** (the real
   identifiers: `argocd`/`kargo`/`https://quay.holos.localhost` + legacy `quay`
   client IDs, `platform-owner/editor/viewer` realm roles, `authenticated` group,
   `svc-quay-resource-controller`/`quay-admin` users) plus a **durable per-CR
   ownership/claim model** (mirroring [ADR-19](ADR-19.md)); no-delete alone is not
   sufficient. The controller exposes **`holos_controller`** per-Kind reconcile and
   Keycloak Admin-API request **metrics** (the `quay/metrics.go` pattern), and every
   Kind reports the Gateway-API **status** contract (`Accepted`/`Programmed`/`Ready`
   + `observedGeneration` + a `Ready` printer column).
9. **This is a design record only — `Status: Proposed`, no Go or CRD code.** The
   YAML is concrete-but-illustrative of the field-level API; the Go types, CRD
   manifests, and reconcilers land in
   [HOL-1344](https://linear.app/holos-run/issue/HOL-1344) and later issues.

## Consequences

- **The Keycloak (identity) half of the GCP-style primitive-role model is now
  designed.** [ADR-19](ADR-19.md) built the Quay (registry) half and deferred the
  Keycloak side to this ADR; this revision specifies the groups
  (`projects/<project>/roles/{owner,editor,viewer}`), the custodian delegation, and
  the client-role mechanism that surfaces `my-project-{owner,editor,viewer}` in the
  `groups` claim. The two halves meet at those exact claim-name strings — no
  cross-group Go import — and because the value is carried by a client role on the
  **Quay client** (the one whose already-deployed `quay-client-roles` mapper feeds
  Quay's token), once these CRDs ship ADR-19's `syncedTeams` binds the Keycloak
  groups to Quay teams with **no Quay-side or new-mapper change**.
- **A second controller API group, sharing the binary and conventions.** The
  controller gains a sibling reconciler set, reusing ADR-19's `caBundle`,
  `credentialsSecretRef`, claim-model, status, and metrics conventions — so the
  cost is incremental reconciler code, not new infrastructure. The status stays
  `Proposed`; nothing changes operationally until the CRDs ship and a
  `KeycloakInstance` + its admin credential are provisioned.
- **A new, security-sensitive admin credential.** A controller that mints clients,
  delivers confidential secrets into project namespaces, provisions groups, and
  pre-creates/auto-links users holds a high-privilege Keycloak Admin-API
  credential — analogous to the load-bearing Quay superuser token
  ([ADR-19](ADR-19.md)). The recommendation is a **scoped** `realm-management`
  service-account client (not blanket realm-admin), created at runtime and never
  committed, with the reserved-name + claim model bounding its blast radius the way
  ADR-19's claim model bounds `FEATURE_SUPERUSERS_FULL_ACCESS`.
- **Email-based auto-link is only as safe as the upstream IdP.** `Trust Email` +
  first-broker-login auto-link bypasses Keycloak's own email verification, so the
  pre-provision-by-email flow is safe **only** when the federated IdP verifies email
  ownership. This is an intrinsic, documented trust assumption the realm config and
  runbook must state, not a controller bug to fix.
- **The ownership boundary [ADR-18](ADR-18.md) deferred now has a concrete,
  enforced answer.** Reserved platform names (keyed on the real realm identifiers,
  kept in sync with the realm config) plus a per-CR claim model make
  disjoint ownership safe despite the global-realm / namespaced-CRD tension —
  config-cli's no-delete posture alone would not. Keeping the reserved set in sync
  is itself an implementation guard rail.
- **Foundation for the Project/Application components ([ADR-21](ADR-21.md)).** These
  Keycloak CRs are the identity resources a project's rendered manifests would emit
  alongside the Quay [ADR-19](ADR-19.md) resources; ADR-21 generalizes the
  `my-project` scaffold to emit both halves per project. Advancing this ADR past
  `Proposed` is the CRD-implementation issue
  ([HOL-1344](https://linear.app/holos-run/issue/HOL-1344)), which fixes the
  field-level API the YAML here only illustrates.
