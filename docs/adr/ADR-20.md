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
| 3        | 2026-06-20 | @jeffmccune | **`Status: Partially Implemented`** — the API group **shipped** (HOL-1344..HOL-1348): the `api/keycloak/v1alpha1` types (`KeycloakInstance`/`Group`/`User`/`Client`), the `internal/keycloak` Admin REST client seam, and the four reconcilers (`internal/controller/keycloak`, claim model + finalizers + conditions + metrics + `ReferenceGrant` gating), with CRDs and RBAC installed by the controller's kustomize tree. Phase 6 (HOL-1348) wired the Holos CUE layer: the controller's **admin credential** (the confidential `svc-holos-controller` service-account client with scoped `realm-management` roles + a generate-once bootstrap into `holos-controller-keycloak-creds`), the realm's **first-broker-login auto-link flow** (`Detect Existing Broker User` + `Automatically Set Existing User`), the central **`KeycloakInstance`** + `security.holos.run` `ReferenceGrant` (the `keycloak-instance` component), and the **`my-project`** project CRs (the role/custodian `KeycloakGroup`s, the owner `KeycloakUser` `bob@example.com`, and the project `KeycloakClient`). **Implementation deviations from this ADR's worked examples:** the shipped `KeycloakGroup.spec.clientRoles[]` / `KeycloakClient.spec.clientRoles[]` (a `{clientRef, role}` referencing a `KeycloakClient` CR by object name) replaced the proposed `KeycloakGroup.clientRoleBindings` (a bare `clientId` list) + `KeycloakClient.emitProjectRolesInGroupsClaim` flag, and the `KeycloakClient` reconciler always ensures its own client-role→`groups` mapper. A consequence: the ADR's **"Quay use case"** (folding `my-project-<role>` into the **platform Quay** client's token with no project client) is **not yet implemented** — the reserved-name guard forbids a tenant `KeycloakClient` targeting `https://quay.holos.internal` and `clientRef` resolves only a same-namespace CR, so today the `my-project` claim values surface in the **project's own** `https://my-project.holos.internal` client token (the ADR's "project's own service" path). Folding onto the reserved Quay client is tracked as follow-up. `KeycloakClientRole`/`KeycloakRealmRole` remain unimplemented. `Updates: ADR-3` unchanged |
| 4        | 2026-06-21 | @jeffmccune | **Close the "Quay use case" gap (HOL-1350).** Reconcile the implemented `clientRoles` model with this ADR's proposed `clientRoleBindings`/`emitProjectRolesInGroupsClaim` design: `ClientRoleReference` now names its target client by **exactly one of** `clientRef` (a same-namespace `KeycloakClient` CR, the "project's own service" path) **or `clientId`** (a Keycloak clientId directly — the new field, CEL-enforced mutual exclusion). A `KeycloakGroup` confers a **project-prefixed** client role (`my-project-<role>`) on the **platform-reserved Quay client** (`https://quay.holos.internal`) by naming its `clientId` directly — no tenant `KeycloakClient` CR exists for it (the reserved-name guard still forbids one). The group reconciler **ensures the project-prefixed role exists** on the named client (idempotent create, on the direct path only — a `clientRef` target stays get-only so a group never expands a project client's role vocabulary), and a **direct-path guard** in `conferClientRoles` bounds the raw-`clientId` capability so it cannot escalate privilege: the target must be on a tight **allowlist of permitted reserved consumer clients** (only the Quay client — so the path cannot reach `realm-management`/`argocd`/`kargo` or an arbitrary client); the path must be a `projects/<project>/roles/<leaf>` role group whose **`<project>` equals the CR's namespace** (the project↔namespace ownership boundary — RBAC governs who creates a CR in a namespace, so a tenant cannot declare another project's path); the role must be **exactly** this role group's own name `<project>-<leaf>` (an exact match, not a prefix, so a prefix collision like project `my` conferring `my-project-owner` is rejected); and the platform's own reserved client-role names (`platform-admin`/`project-admin`) are refused. The guard keys on the **resolved** clientId regardless of which field named it: the Keycloak built-in clients (`realm-management`, `account`, `account-console`, `broker`, `security-admin-console`) are added to the reserved set, and a `clientRef` resolving to **any** reserved client is refused outright — so a tenant cannot craft a same-namespace `KeycloakClient` whose `spec.clientId` is `realm-management` and confer `realm-admin` through `clientRef` (a reserved client's roles are conferable only via the bounded direct path, which allowlists only the Quay client). The `quay` **client object** stays config-cli's. `KeycloakClient.spec.clientRoles` is CEL-constrained to forbid `clientId` (a client defines roles on itself), so the shared `ClientRoleReference` type never admits a silently-ignored target there. The role then surfaces in Quay's `groups` claim via the already-deployed `quay-client-roles` mapper, so [ADR-19](ADR-19.md) `syncedTeams[].oidcGroup` membership populates with **no Quay-side or new-mapper change**. The `KeycloakClient` reserved-name guard (client-object create/reconfigure) is unchanged. The `my-project` role groups now confer the role on **both** the Quay client (`clientId`) and the project client (`clientRef`). `KeycloakClientRole`/`KeycloakRealmRole` remain unimplemented. `Status`/`Updates: ADR-3` unchanged |
| 5        | 2026-06-21 | @jeffmccune | **Record the `esso` enterprise-SSO realm + `holos` OIDC brokering topology (HOL-1366/HOL-1367).** Add the *Two-realm topology: the `esso` enterprise-SSO realm + the `holos` OIDC broker* design section: a **second Keycloak realm `esso`** on the **single** Keycloak instance at `https://auth.holos.internal` (reachable at `https://auth.holos.internal/realms/esso`; the existing `auth.holos.internal` HTTPRoute already covers all realms — no new route) models an **authentication-only** upstream Enterprise SSO IdP, and the `holos` realm **brokers** logins from it via an **OIDC identity provider** (broker **alias `esso`**, `trustEmail: true`, `firstBrokerLoginFlowAlias: "first broker login"`). The broker's confidential **esso client** is named `https://auth.holos.internal/realms/holos` with redirect URI `https://auth.holos.internal/realms/holos/broker/esso/endpoint`. **Authorization stays entirely in the `holos` realm's groups/roles** ([ADR-3](ADR-3.md)) — `esso` authenticates, `holos` authorizes. Introducing the `esso` IdP (and correcting the `holos` realm's first-broker-login flow declaration) is what **completes and fixes** the HOL-1348 auto-link flow that currently fails the keycloak-config Job (`Cannot find stored execution by authenticator 'idp-auto-link'…`) because no IdP is present. **AC #5 provisioning constraint:** the `esso` realm and the `holos` IdP are provisioned by **keycloak-config-cli Jobs + bootstrap Jobs only**, with **no dependency on the `holos-controller` API groups** (`keycloak.holos.run`/`quay.holos.run`), to avoid a fresh-cluster provisioning race. **Ownership shift:** the `holos` realm's `identityProviders[]` move under the **holos realm-config keycloak-config-cli Job** (so the IdP `clientSecret` can be injected at runtime via `$(env:…)`), while the `KeycloakRealmImport` CR keeps owning `enabled`. Update the *reserved names* set to add the `esso` realm, the `esso` IdP broker alias, and the `https://auth.holos.internal/realms/holos` esso client ID, and note the broker alias changed from the placeholder `holos` to `esso`. This revision is a **design record only — no CUE/Go behavior changes**; phases 2–4 (HOL-1368..HOL-1370) implement it. `Status: Partially Implemented`/`Updates: ADR-3` unchanged |
| 6        | 2026-06-21 | @jeffmccune | **Reconcile the `esso` IdP section with the as-built first-broker-login flow (HOL-1371, the cleanup phase).** Revision 5 (a design record) wrote `firstBrokerLoginFlowAlias: "first broker login"` — Keycloak's built-in flow. The as-built (HOL-1369) instead points at a **custom** (`builtIn: false`) flow, alias **`first broker login auto-link`** (subflow `User creation or linking auto-link`), because keycloak-config-cli **refuses to add executions to a built-in flow** — the built-in `User creation or linking` subflow lacks `idp-auto-link`, so importing it throws `Cannot find stored execution by authenticator 'idp-auto-link'`. The *`firstBrokerLoginFlowAlias`* bullet and the design-consequences item now describe the custom flow and why it is required. **Documentation-only consistency correction — no CUE/Go behavior change** (the behavior shipped in HOL-1369); `Status: Partially Implemented`/`Updates: ADR-3` unchanged. New operator runbook `docs/runbooks/esso-keycloak-idp.md` and the `holos/docs/keycloak-clients.md` *esso realm* section document the as-built topology |
| 7        | 2026-06-28 | @jeffmccune | **Make the controller transparent — remove the project-prefix / reserved-name / claim-value-rewriting / disjointness model from the reconcilers (HOL-1420/HOL-1421).** The `keycloak.holos.run` reconcilers now reconcile **exactly** the group `path`, client `clientId`, and client-role names declared in the spec — **adding, stripping, requiring, and refusing nothing on organizational-policy grounds**. HOL-1421 removed from the Go reconcilers (PR #207): the reserved client-ID set (`argocd`/`kargo`/`https://quay.holos.internal`/the Keycloak built-ins/the esso broker client), the reserved group prefixes/names (`platform-*`/`authenticated`/`realm_roles`/…), the reserved client-role names (`platform-admin`/`project-admin`), the `validateDirectClientRole` direct-`clientId` guard (the Quay-only allowlist, the `<project>`-equals-namespace project↔namespace check, the `<project>-<leaf>` exact-match rule, the reserved-role refusal), `projectRoleFromGroupPath`, the `clientRef`-resolves-to-reserved refusal, and the `ReasonReserved` condition. A `KeycloakClient` may now declare **any** `clientId` (previously-reserved IDs included); a `KeycloakGroup` may declare **any** `spec.path`; a `KeycloakGroup.spec.clientRoles[]` entry may name **any** client by `clientId` and confer **any** role name — the reconciler resolves the client, ensures the role exists (idempotent create), and confers it. Previously-refused specs now enter the **normal claim/adoption flow** (a write when the controller creates/owns the object, a `Conflict` when an unadopted foreign object already holds the name) rather than being rejected on `Reserved` grounds. **What is preserved** (reconciliation/structural invariants, not org policy): the claim/adoption/ownership-conflict model (`spec.adopt`, `status.created`/`status.adopted`, the finalizer + Keycloak-UUID tracking, `ReasonConflict`/`ReasonReleased`) and all `+kubebuilder:validation:XValidation` markers (immutability, the `clientRef` XOR `clientId`, confidential-requires-`secretRef`, a `KeycloakClient`'s own `clientRoles` may not set `clientId`). The CRDs were diff-clean (the policy lived in Go, not CRD CEL). The superseded design sections below — *Claim value via a client role* (the project-prefix/exact-match rewriting), the reserved-names set in *Ownership / disjointness*, and the reserved-role discussion in *KeycloakClientRole and KeycloakRealmRole* — are marked **historical/removed**, and a new *Transparent contract, migration, and admission-control policy (Rev 7)* section records the new contract, the migration note, and the admission-control pointer. **The recommended home for configuration policy** (naming conventions, reserved prefixes, tenant/platform disjointness) is now Kubernetes **admission control** — a `ValidatingAdmissionPolicy` with CEL and/or a `ValidatingAdmissionWebhook` backed by dedicated policy CRs — authored as a **separate downstream effort** (out of scope for this revision, which documents the contract and points at the mechanism). This is a documentation revision recording HOL-1421's behavior change; `Status: Partially Implemented`/`Updates: ADR-3` unchanged |
| 8        | 2026-06-28 | @jeffmccune | **Add the additive `KeycloakClient.spec.description` field (HOL-1424..HOL-1427).** `KeycloakClientSpec` gains an optional `Description string` (`json:"description,omitempty"`, `+optional`) — free text the reconciler propagates verbatim to the managed Keycloak client's native **Description** attribute. The `internal/keycloak` library carries it on `OIDCClient.Description` (`omitempty`) and `ClientFields.Description` (HOL-1425); the client reconciler sets it in `desiredClient` on create and sends it **unconditionally** (a non-nil pointer to a possibly-empty string) in `updateClient`, so a console-set description is corrected back to the spec on every reconcile and an omitted/empty spec value actively clears it (HOL-1426). Purely additive: no change to `KeycloakGroup`/`KeycloakInstance`/`KeycloakUser`, and existing `KeycloakClient` specs that omit `description` reconcile unchanged. The CRD bundle (`config/crd/bases/keycloak.holos.run_keycloakclients.yaml`) and `zz_generated.deepcopy.go` were regenerated; the docs (this revision table and [holos/docs/keycloak-clients.md](../../holos/docs/keycloak-clients.md)) record the field (HOL-1427). `Status: Partially Implemented`/`Updates: ADR-3` unchanged |

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
  https://quay.holos.internal`) is the **precedent for the claim-value mechanism**
  this ADR adopts.
- [holos/components/keycloak/instance/buildplan.cue](../../holos/components/keycloak/instance/buildplan.cue):
  the Keycloak server instance. The operator names its Service `keycloak-service`,
  serving HTTPS on `8443` in the `keycloak` namespace (in-cluster URL
  `https://keycloak-service.keycloak.svc:8443`); the external hostname is `auth.holos.internal`.
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
(`Ready=False`, reason `ReferenceNotGranted` — the as-built reason name; this ADR's
earlier revisions wrote the placeholder `RefNotPermitted`), never silently honored — the same
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
  # per-client; see "claim value" below). The Quay client https://quay.holos.internal
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
    - clientId: https://quay.holos.internal   # the ADR-19 syncedTeams consumer
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

> **Superseded in part by Revision 7 (HOL-1421).** The *mechanism* below — a
> client role on the consumer client, surfaced by the per-client
> `oidc-usermodel-client-role-mapper` — is unchanged and still how a role
> value reaches a token. What Revision 7 **removed** is the controller's
> *enforcement* of the project-prefix / exact-`<project>-<leaf>`-match /
> reserved-role rewriting around it: the reconciler no longer requires a
> project-prefixed role name, no longer ensures only `<project>-<leaf>` on the
> Quay client via the bounded direct-path guard, and reserves no role name. It
> confers exactly the role named in `spec.clientRoles[]` on exactly the named
> client. The Project component still emits the conventional `<project>-<role>`
> names, so the worked example holds; the controller simply no longer *imposes*
> it. See *Transparent contract, migration, and admission-control policy
> (Rev 7)* below.

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
https://quay.holos.internal`
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
  `KeycloakGroup` lists `clientId: https://quay.holos.internal` in its
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
`https://quay.holos.internal`), matching the platform's own convention where the
real Quay `clientId` **is** `https://quay.holos.internal`
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
  # quay clientId is itself https://quay.holos.internal).
  clientId: https://my-app.holos.internal
  # public (SPA/CLI, PKCE S256, no secret) | confidential (delivered secret).
  # Mirrors the argocd/kargo (public) vs quay (confidential) distinction in
  # keycloak-clients.md. PKCE S256 is the default; relax only per that guardrail.
  type: confidential
  redirectUris:
    - https://my-app.holos.internal/oauth2/callback
  webOrigins:
    - https://my-app.holos.internal
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

> **Note (Revision 7, HOL-1421):** The "disjoint by construction" framing below
> relied on the now-removed project-prefix reservation. The controller no longer
> reserves or rewrites role names. Be precise about what the retained claim model
> covers: the per-CR **claim/`Conflict`** model protects a CR's **own claimed
> Keycloak object** — the group at a `KeycloakGroup.spec.path`, a client, a user.
> It does **not** put a per-role claim boundary around a **directly-referenced**
> client's roles: when a `KeycloakGroup.spec.clientRoles[]` entry names a client by
> `clientId`, the controller **idempotently** ensures that role exists
> (`CreateClientRoleIfNotExists`) and confers it — two CRs naming the same role on
> the same client both succeed (last write wins on the assignment), they do **not**
> arbitrate via `Conflict`. So "two CRs claiming the same role are resolved by
> `Conflict`" holds only for a role defined **on a CR's own claimed client**, not
> for a role created on a foreign client via the direct `clientId` path. (This is
> why the worked example relies on convention — the Project component emitting
> distinct `<name>-<role>` names — not on a controller guarantee.) `KeycloakClientRole`
> / `KeycloakRealmRole` remain unimplemented regardless.

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
(see *What the platform must provide* below) and the CR **assumes is present**.
The CR's surface is the per-user pre-create + membership; the auto-link behavior
is a documented prerequisite, not CR state. (**Rev 5 ownership note:** as of
Revision 5 — see *Two-realm topology* below — the `holos` realm's
`identityProviders[]` are owned by the **holos realm-config keycloak-config-cli
Job**, not the `KeycloakRealmImport` CR, which keeps owning only `enabled`; the
first-broker-login *flow* remains realm config, and either way it is **not** a
`keycloak.holos.run` CR's concern.)

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
stays platform-owned, not per-user CR state — and is **never** a
`keycloak.holos.run` CR's concern. It lives in the **platform realm/IdP
definition** (the `keycloak-config-cli` Job + the `KeycloakRealmImport` CR), with
the field ownership split documented in *Two-realm topology* below: **as of
Revision 5** the `holos` realm's **`identityProviders[]`** (the OIDC broker for
the `esso` realm, including its `trustEmail`/`firstBrokerLoginFlowAlias`) are
owned by the **holos realm-config keycloak-config-cli Job** (so the broker's
`clientSecret` can be injected at runtime via `$(env:…)`), while the
`KeycloakRealmImport` CR keeps owning **`enabled`** — the two paths still avoid
contention because they own disjoint fields
([keycloak-clients.md](../../holos/docs/keycloak-clients.md), the *Keycloak
Configuration as Code* guard rail, updated to match in phases 3/5). The realm's
first-broker-login flow (**`Detect Existing Broker User`** + **`Automatically Set
Existing User`**) and the IdP's **`Trust Email`** setting are therefore configured
by that platform layer — so that when Bob first authenticates through the
federated IdP, Keycloak matches his email to the pre-created record and **links**
it instead of creating a second user. The `KeycloakUser` CR **assumes** this
realm/IdP configuration is present (a documented prerequisite); it does **not**
reconcile the first-broker-login flow or the IdP itself.

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

### Two-realm topology: the `esso` enterprise-SSO realm + the `holos` OIDC broker

The auto-link prerequisite above (`Detect Existing Broker User` +
`Automatically Set Existing User` + `Trust Email`) only completes when there is
an **identity provider** for the federated login to arrive through. In a
production deployment that IdP is the customer's real Enterprise SSO. For
**local development** the platform models that upstream IdP with a **second
Keycloak realm**, `esso`, on the **same** Keycloak instance — so the whole
broker-and-auto-link flow can be exercised end-to-end on a fresh local cluster
with no external dependency. This section records that topology; the CUE/Job
changes that build it land in phases 2–4 (HOL-1368..HOL-1370), not here.

**One Keycloak instance, two realms.** A single Keycloak CR (the
[instance component](../../holos/components/keycloak/instance/buildplan.cue))
serves **both** realms at `https://auth.holos.internal`. The platform realm is
reachable at `https://auth.holos.internal/realms/holos` and the new
enterprise-SSO realm at `https://auth.holos.internal/realms/esso`. The existing
`auth.holos.internal` HTTPRoute already fronts all realms on that host, so **no
new route** is added — the second realm is purely additive realm configuration.

**`esso` is authentication-only.** The `esso` realm models an upstream
Enterprise SSO identity provider. Its sole job is to **authenticate** a person
(e.g. the worked-example user `alice`) and assert a verified email. It carries
**no** authorization state — no project groups, no `owner`/`editor`/`viewer`
roles, no Quay/Argo client roles. All authorization remains in the `holos`
realm's groups and roles ([ADR-3](ADR-3.md)): the `holos` realm authorizes, the
`esso` realm only authenticates. This split is exactly the production model
where the customer's IdP authenticates and the `holos` realm owns authorization.

**The `holos` realm brokers `esso` via an OIDC identity provider.** The `holos`
realm declares an **OIDC identity provider** with **broker alias `esso`** that
points at the `esso` realm's OIDC endpoints. Its key settings:

- **`alias: esso`** — the broker alias that identifies this identity provider in
  the `holos` realm. The IdP selects which first-broker-login flow runs for its
  logins via `firstBrokerLoginFlowAlias` (below); the flow's `idp-auto-link` /
  `idp-create-user-if-unique` executions are flow configuration, not bound to the
  alias. The alias changed from the earlier placeholder `holos` to **`esso`** in
  this revision.
- **`trustEmail: true`** — `esso` is trusted to assert verified email ownership,
  so the broker accepts the asserted email without re-verifying it. This is the
  setting that **enables email-based auto-link** to a pre-provisioned
  `KeycloakUser` (with the *email-based auto-link security tradeoff* documented
  in *KeycloakUser* above — safe only because the upstream `esso` IdP is trusted
  to verify email ownership).
- **`firstBrokerLoginFlowAlias` → a custom auto-link flow** — as built (HOL-1369)
  the broker points at a **custom** (`builtIn: false`) first-broker-login flow,
  alias **`first broker login auto-link`**, **not** Keycloak's built-in
  `first broker login`. The custom flow declares `idp-review-profile` (REQUIRED)
  then a subflow (`first broker login auto-link` → `User creation or linking
  auto-link`) running the HOL-1348 auto-link executions
  (`idp-create-user-if-unique` ALTERNATIVE + `idp-auto-link` ALTERNATIVE). A
  custom flow is required because keycloak-config-cli **refuses to add executions
  to a built-in flow** — the built-in `User creation or linking` subflow has no
  `idp-auto-link` execution, so importing one into it throws `Cannot find stored
  execution by authenticator 'idp-auto-link'` (the failure HOL-1369 fixed). With
  the `esso` IdP present and the custom flow declared, a federated `esso` login
  whose email matches a pre-provisioned `holos` user is **linked** to that user
  rather than creating a duplicate — completing the design HOL-1348 began.

**The confidential `esso` client.** For the `holos` realm to broker to `esso`
over OIDC, the **`esso` realm** hosts a **confidential OIDC client** that the
`holos` broker authenticates as. That client's `clientId` is
**`https://auth.holos.internal/realms/holos`** (it identifies the `holos` realm
as the relying party), and its single redirect URI is the broker callback
**`https://auth.holos.internal/realms/holos/broker/esso/endpoint`** — Keycloak's
canonical `…/broker/<alias>/endpoint` path for the `esso` broker alias. The
client is confidential; its **`clientSecret` is shared** between the `esso`
client definition and the `holos` realm's IdP config and is **generated at
runtime and never committed** (the runtime-secret guardrail,
[secret-handling.md](../../holos/docs/secret-handling.md)).

**This fixes the currently-failing auto-link flow.** Today the `holos` realm's
first-broker-login flow declaration is incomplete/incorrect and no IdP exists to
exercise it, and the keycloak-config Job fails with
`Cannot find stored execution by authenticator 'idp-auto-link'…`. The precise
config-cli/Keycloak cause is diagnosed and fixed in the implementing phases
(HOL-1369), not asserted here; what this ADR records is the **design
resolution**: introducing the `esso` IdP **and correcting the
first-broker-login flow declaration** (phases 2–3) together **complete and fix**
the HOL-1348 auto-link design so the keycloak-config Job goes green and a
federated `esso` login auto-links to its pre-provisioned `holos` user.

**Provisioned by Jobs only — no controller dependency (AC #5).** The `esso`
realm and the `holos` realm's IdP are provisioned **exclusively by
keycloak-config-cli Jobs + bootstrap Jobs** — the same declarative,
config-cli-driven mechanism the platform already uses for its own realm — with
**no dependency on the `holos-controller` API groups** (`keycloak.holos.run` /
`quay.holos.run`). This is load-bearing: the controller and its CRDs are
themselves bootstrapped against the `holos` realm (e.g. the
`svc-holos-controller` admin credential, the `KeycloakInstance`), so making the
realm/IdP topology depend on the controller would create a **fresh-cluster
provisioning race** — the realm the controller authenticates against could not
be brought up until the controller it depends on was already running. Keeping
the two-realm topology entirely in the config-cli/bootstrap-Job layer breaks
that cycle: the realms and the broker exist before any controller reconcile.

**Ownership shift: config-cli owns the `holos` realm's `identityProviders[]`.**
Per ADR-20 today the `holos` realm-config keycloak-config-cli Job imports
`realm: "holos"` and deliberately carries **no `identity-provider` (or
`enabled`) fields**, which are owned by the `KeycloakRealmImport` CR
([keycloak-clients.md](../../holos/docs/keycloak-clients.md), the *Keycloak
Configuration as Code* guard rail). **This feature shifts that boundary:** the
`holos` realm's **`identityProviders[]`** move under the **holos realm-config
Job's** ownership, so the broker's `clientSecret` can be injected at runtime via
`$(env:…)` substitution (`IMPORT_VARSUBSTITUTION_ENABLED`) the same way the
existing confidential-client secrets are. The `KeycloakRealmImport` CR keeps
owning **`enabled`** (and the realm's existence), so the two reconciliation
paths still do not contend. Phases 3 and 5 (HOL-1369/HOL-1371) update the
AGENTS.md *Keycloak Configuration as Code* guard rail and the
[keycloak-clients.md](../../holos/docs/keycloak-clients.md) note to match this
shift; this ADR records the decision so they have a single source of truth.

### Ownership / disjointness vs `keycloak-config-cli` (AC #6)

> **Superseded in part by Revision 7 (HOL-1421).** The **reserved-platform-names**
> enforcement and the **project-prefix** disjointness mechanism described in this
> section have been **removed from the controller** — it no longer reserves any
> client ID, group path, role name, realm, or broker alias, and no longer rejects
> a CR on `Reserved` grounds (`ReasonReserved` is gone). The **claim/adoption
> ownership model** (the durable per-CR marker, `Conflict`/`Released`) is
> **retained** — that is reconciliation correctness, not org policy. Tenant/platform
> disjointness, reserved prefixes, and naming conventions are now the job of
> **admission control** (see *Transparent contract, migration, and admission-control
> policy (Rev 7)* below); the *Reserved platform names* bullet and its
> identifier list below are **historical** — they describe the as-removed Go
> behavior and are kept only as the starting inventory for an admission policy.

These CRDs are reconciled by the **existing `holos-controller` binary** as a
**separate API group alongside `quay.holos.run`** — additive to, and **disjoint
from**, the existing `keycloak-config-cli` Job that owns the platform's own realm.
The division of ownership and the disjointness *enforcement* generalize Revision
1's reserved-name + claim discussion into a concrete model:

- **The platform keeps owning its own realm.** The platform clients, the platform
  realm roles, the shared `groups`-claim mappers, the `authenticated` default
  group, and the seeded superuser users remain `keycloak-config-cli`'s; the
  **identity-provider and first-broker-login / IdP flow config** (the auto-link
  prerequisite above) is **platform realm/IdP configuration owned by the
  config-cli / `KeycloakRealmImport` layer, not the CRDs** — and **as of Revision
  5** that ownership is split: the `holos` realm's **`identityProviders[]`** are
  owned by the **holos realm-config keycloak-config-cli Job** (so the broker's
  `clientSecret` can be injected at runtime via `$(env:…)`; see *Two-realm
  topology* above), while the `KeycloakRealmImport` CR keeps owning **`enabled`**.
  (Before Rev 5 config-cli imported `realm: "holos"` with **no**
  `identity-provider` fields and the `KeycloakRealmImport` CR owned the IdP
  fields; Rev 5 moves `identityProviders[]` to config-cli — the two paths still
  do not contend because they own disjoint fields.) Either way the CRDs do
  **not** redeclare or fight over any of these. config-cli's managed-import
  behavior is **no-delete**: realm objects it does not declare are left untouched.
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
  - **client IDs**: `argocd`, `kargo`, **`https://quay.holos.internal`** (the
    real Quay `clientId`, `QUAY_CLIENT_ID`), the legacy disabled `quay` client
    ID — reserving the display string `quay` alone would miss the real client and
    leave a bypass — the **esso broker client `https://auth.holos.internal/realms/holos`**
    (the confidential client the `esso` realm hosts for the `holos` realm's OIDC
    broker; see *Two-realm topology* above) — and the **Keycloak built-in clients**
    `realm-management` (hosts `realm-admin`/`manage-*`, the prime escalation
    target), `account`, `account-console`, `broker`, `security-admin-console`
    (HOL-1350): reserving these stops a tenant `KeycloakClient`/`KeycloakGroup`
    from defining or conferring roles on Keycloak's own clients;
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
  - **realms**: `holos` (the platform realm these CRs reconcile into) and
    **`esso`** (the enterprise-SSO upstream realm; see *Two-realm topology*
    above) — a tenant CR may not target the `esso` realm via `instanceRef`, and
    the `esso` realm is provisioned only by keycloak-config-cli/bootstrap Jobs,
    never by the controller (HOL-1366/HOL-1367);
  - **identity-provider broker aliases**: **`esso`** (the `holos` realm's OIDC
    broker alias for the upstream `esso` realm; the alias changed from the
    earlier placeholder `holos` to `esso` in this revision). The identity
    providers are owned by the holos realm-config Job / `KeycloakRealmImport`
    CR, not by these CRDs (see *Two-realm topology* above).
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
`ReferenceNotGranted`, `CredentialsNotFound`, `KeycloakError`, …) are
defined **once** in a shared constants file in the Keycloak controller package
(the analog of `internal/controller/quay/conditions.go`) and shared by every
reconciler, never re-derived per Kind. (The `Reserved` reason listed in earlier
revisions was **removed in Revision 7 (HOL-1421)** along with the reserved-name
policy; see *Transparent contract, migration, and admission-control policy
(Rev 7)*.) A denied cross-namespace `instanceRef`
(missing `ReferenceGrant`) surfaces as `Ready=False` reason `ReferenceNotGranted`,
which is the observability ADR-22's grant model depends on.

### Transparent contract, migration, and admission-control policy (Rev 7)

**Revision 7 (HOL-1420/HOL-1421) makes the `keycloak.holos.run` reconcilers
transparent.** The model in *Claim value via a client role* and *Ownership /
disjointness* above — project-prefixed role rewriting, the reserved-name sets,
the `validateDirectClientRole` direct-`clientId` guard, and the project↔namespace
disjointness check — was **enforcement of organizational policy in Go**. Those
sections are now **historical**; this section records the contract as built.

**The transparent contract.**

- The reconciler writes the group `path`, the client `clientId`, and client-role
  names **verbatim** — exactly as declared in the spec. Nothing is added,
  stripped, or required, and **no client ID or role name is reserved or refused**
  by the controller on policy grounds.
- A `KeycloakClient` may declare **any** `spec.clientId`, including
  previously-reserved IDs (`argocd`, `kargo`, `realm-management`,
  `https://quay.holos.internal`, the Keycloak built-in clients, the esso broker
  client). A `KeycloakGroup` may declare **any** `spec.path`, including
  previously-reserved paths/prefixes (`platform-*`, `authenticated`,
  `realm_roles`, `default-roles-holos`).
- A `KeycloakGroup.spec.clientRoles[]` entry may name **any** client by
  `clientId` and confer **any** role name — there is no Quay-only allowlist, no
  `<project>-<leaf>` exact-match rule, no reserved-role-name refusal, and no
  `project == namespace` check. The reconciler resolves the client, ensures the
  named role exists (idempotent create), and confers it.
- **What is preserved** is *reconciliation correctness*, not policy: the
  claim/adoption/ownership-conflict model (`spec.adopt`, `status.created`/
  `status.adopted`, the finalizer + Keycloak-UUID tracking, `ReasonConflict`,
  adopted-release `ReasonReleased`) and all the structural CEL validation markers
  (immutability, `clientRef` XOR `clientId`, confidential-requires-`secretRef`, a
  `KeycloakClient`'s own `clientRoles` may not set `clientId`).

**Where configuration policy lives now.** Naming conventions, reserved prefixes,
and tenant/platform disjointness are **organizational policy**, not reconciler
mechanics — so they move to Kubernetes **admission control**, evaluated at
`CREATE`/`UPDATE` admission before an object is ever persisted:

- A **`ValidatingAdmissionPolicy`** (built-in, CEL-based, no extra deployment)
  for the rules that are expressible as CEL over the incoming object — e.g.
  "a `KeycloakGroup.spec.path` in a tenant namespace must start with
  `projects/<that-namespace>/`", or "a tenant `KeycloakClient.spec.clientId`
  may not be one of {`argocd`, `kargo`, `https://quay.holos.internal`, …}".
- A **`ValidatingAdmissionWebhook` backed by dedicated policy CRs** for rules
  that need state the admission request alone does not carry (cross-object
  disjointness, a centrally-managed reserved-name registry, per-tenant
  allow/deny lists).

**Defining the concrete policies is a separate, downstream effort** — this
revision documents the contract and points at the mechanism; it does **not**
author any `ValidatingAdmissionPolicy`, webhook, or policy CR. The reserved-name
inventory in *Ownership / disjointness* above is the natural starting point for
that work.

**Migration note (existing clusters that relied on the Rev 1–6 behavior).**

- **No CRD/schema change and no data migration.** The removed policy lived in the
  Go reconcilers, not in CRD CEL, so existing CRDs and existing objects are
  unaffected; the controller simply stops rejecting specs it used to refuse.
- **Previously-refused specs now enter the normal claim/adoption flow.** Any
  `KeycloakClient` / `KeycloakGroup` that was being held `Ready=False` with reason
  `Reserved` (a reserved client ID, a reserved group path/prefix, a
  non-allowlisted direct `clientId`, a non-`<project>-<leaf>` role, or a
  `clientRef` resolving to a reserved client) is, after upgrading to the
  transparent controller, **no longer rejected**. Distinguish the two sides of
  what then happens:
  - **The CR's own claimed object** (the group at `spec.path`, the client, the
    user) goes through the retained **claim/adoption** model: the controller
    writes the spec verbatim when it creates or already owns the object, and
    surfaces **`Conflict`** (not a write, not a seizure) when an **unadopted,
    foreign** object already holds that name unless `spec.adopt` is set.
  - **A directly-referenced client's role** named via a
    `KeycloakGroup.spec.clientRoles[]` `clientId` entry is **not** behind that
    per-role claim boundary: once the group is owned/reconciled, the controller
    **idempotently** ensures the named role on the referenced client and assigns
    it — so a formerly-refused direct-`clientId` grant (e.g. a `<role>` on
    `https://quay.holos.internal`) will now create/confer that role on a client
    it does not own. This is the side effect operators most need to anticipate.

  The net change is that a name is no longer **refused outright** on policy
  grounds — so a write that was previously a no-op rejection can now reach
  Keycloak. Operators with such objects should
  review them **before upgrading**, since a name that used to be a guaranteed
  rejection may now resolve to a real Keycloak write or an adoption conflict.
- **Re-establish policy via admission control if you want it.** Nothing in the
  controller now prevents a tenant CR from naming a platform client/role or
  another project's path. If your cluster depended on the controller for that
  protection, **install an admission policy** (per the pointer above) to restore
  it; until then, RBAC on who may create `keycloak.holos.run` CRs in which
  namespace is the only remaining boundary.
- **The Project/Application components are unchanged.** They already emit only the
  conventional `<name>-<role>` / bare-leaf names against the conventional clients,
  so their rendered output and runtime behavior are identical before and after —
  the change only widens what a hand-authored CR is *permitted* to do.

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
   reference is rejected (`Ready=False`, `ReferenceNotGranted` — the as-built reason
   name; earlier revisions wrote `RefNotPermitted`), never silently honored.
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
   `https://quay.holos.internal`** for the [ADR-19](ADR-19.md) `syncedTeams` case,
   whose existing `quay-client-roles` mapper already emits client roles into Quay's
   `groups` claim — or on the project's own client when its own token must carry the
   value. Assigning the role on the wrong client (e.g. only on the project client
   when Quay is the consumer) would **not** surface it in Quay's token. The
   **full-path Group Membership mapper** (emits a path, not the flat value) and a
   **script mapper** (disabled by default, an avoidable security/operational
   liability) are **rejected**.
6. **`KeycloakClient` manages a per-project OIDC client named by its URL**
   (`clientId: https://my-app.holos.internal`, the platform convention), opts its
   own token into carrying project roles via `emitProjectRolesInGroupsClaim` (the
   per-client mapper on **its own** `clientId`) — while the **authoritative
   role→consumer-client binding is `KeycloakGroup.clientRoleBindings`**, a single
   owning field that works even when the consumer (the Quay client) has no project
   `KeycloakClient` — and delivers a confidential client's secret into the
   project namespace as runtime-created, never-committed material.
   **`KeycloakUser` pre-provisions a person by email only-if-necessary** and assigns
   group membership; the **first-login auto-link** (`Detect Existing Broker User` +
   `Automatically Set Existing User` + `Trust Email`) is **platform realm/IdP config
   the config-cli / `KeycloakRealmImport` layer owns** (per Decision #10 / Rev 5
   the `holos` realm's `identityProviders[]` are owned by the holos realm-config
   keycloak-config-cli Job and the `KeycloakRealmImport` CR keeps `enabled`), not
   CR state — with the documented email-based-auto-link security tradeoff.
   `KeycloakClientRole`/`KeycloakRealmRole` carry the client-scoped triad and the
   realm-role → client-role composite.
7. **The API-group dependency boundary holds (AC #3):** `api/keycloak/v1alpha1`
   imports **only** `k8s.io/api` / `k8s.io/apimachinery`; it reaches Keycloak
   solely via the `KeycloakInstance` credential and takes **no** dependency on
   Quay/Kargo/Argo CD or their types. The OIDC group names Quay consumes
   (`syncedTeams`) remain **data referenced by name** — the two groups meet only at
   the claim-name string, preserving [ADR-19](ADR-19.md)'s boundary in reverse.
8. **Disjoint ownership from the platform realm config is enforced**, not assumed:
   *(**Revision 7 (HOL-1421) update:** the reserved-name half of this enforcement
   was **removed from the controller** — it is now transparent. The durable
   per-CR ownership/claim model is retained; reserved names / prefixes /
   disjointness move to admission control, a downstream effort. See *Transparent
   contract, migration, and admission-control policy (Rev 7)*. The original text
   below is historical.)*
   `keycloak-config-cli` keeps owning the platform's own realm objects (clients,
   roles, the `authenticated` group, the superusers) — and, **as of Rev 5
   (Decision #10)**, the `holos` realm's `identityProviders[]` too — while the
   `KeycloakRealmImport` CR owns `enabled` (and, before Rev 5, the IdP fields the
   realm-config Job now owns); the controller owns per-project objects.
   Enforcement is **reserved platform names** (the real
   identifiers: `argocd`/`kargo`/`https://quay.holos.internal` + legacy `quay`
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
10. **A second `esso` realm models the upstream enterprise IdP for local dev,
    brokered by the `holos` realm over OIDC (HOL-1366/HOL-1367).** One Keycloak
    instance serves both `holos` and `esso` at `https://auth.holos.internal`
    (`esso` at `…/realms/esso`; no new HTTPRoute). The `esso` realm is
    **authentication-only**; **all authorization stays in the `holos` realm's
    groups/roles** ([ADR-3](ADR-3.md)). The `holos` realm brokers `esso` via an
    **OIDC identity provider** with **alias `esso`**, `trustEmail: true`, and
    `firstBrokerLoginFlowAlias` pointing at a **custom** auto-link flow (`first
    broker login auto-link`, as built in HOL-1369 — see *The `esso` identity
    provider* above for why a custom flow rather than the built-in is required);
    the `esso` realm hosts the
    broker's **confidential client** `https://auth.holos.internal/realms/holos`
    with redirect URI
    `https://auth.holos.internal/realms/holos/broker/esso/endpoint` and a
    runtime-generated, shared `clientSecret`. Introducing this IdP (and
    correcting the flow declaration) **completes and fixes** the HOL-1348
    auto-link flow that otherwise fails the keycloak-config Job
    (`Cannot find stored execution by authenticator 'idp-auto-link'…`). The
    `esso` realm and the `holos` IdP are provisioned by **keycloak-config-cli +
    bootstrap Jobs only, with no dependency on the `holos-controller` API
    groups** (`keycloak.holos.run`/`quay.holos.run`), to avoid a fresh-cluster
    provisioning race. The **`holos` realm's `identityProviders[]` shift to the
    holos realm-config Job's ownership** (runtime `$(env:…)` secret injection)
    while the `KeycloakRealmImport` CR keeps owning `enabled`. This decision is a
    **design record only**; phases 2–4 (HOL-1368..HOL-1370) implement it.

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
  committed. *(As of Revision 7 (HOL-1421) the controller no longer enforces
  reserved names — only the per-CR **claim/adoption** model bounds the
  credential's blast radius, the way ADR-19's claim model bounds
  `FEATURE_SUPERUSERS_FULL_ACCESS`; reserved-name / disjointness enforcement, if
  desired, is now downstream admission-control work — see *Transparent contract,
  migration, and admission-control policy (Rev 7)*.)*
- **Email-based auto-link is only as safe as the upstream IdP.** `Trust Email` +
  first-broker-login auto-link bypasses Keycloak's own email verification, so the
  pre-provision-by-email flow is safe **only** when the federated IdP verifies email
  ownership. This is an intrinsic, documented trust assumption the realm config and
  runbook must state, not a controller bug to fix.
- **The auto-link prerequisite is now modeled end-to-end for local dev (Rev 5).**
  The `esso` realm + `holos` OIDC broker (alias `esso`) gives the
  first-broker-login auto-link flow an actual identity provider to arrive
  through, so the whole pre-provision-by-email → federated-login → auto-link path
  is exercisable on a fresh local cluster with no external IdP. Because the
  realm/IdP topology is provisioned by config-cli/bootstrap Jobs only — never the
  controller — the realm the controller authenticates against exists **before**
  any controller reconcile, so there is no fresh-cluster provisioning race. The
  cost is the ownership shift of the `holos` realm's `identityProviders[]` from
  the `KeycloakRealmImport` CR to the holos realm-config Job (the
  *Keycloak Configuration as Code* guard rail in AGENTS.md and
  [keycloak-clients.md](../../holos/docs/keycloak-clients.md) are updated to match
  in phases 3/5).
- **The ownership boundary [ADR-18](ADR-18.md) deferred.** *(Revision 7 update:
  this was originally answered with **reserved platform names plus a per-CR claim
  model**. HOL-1421 **removed the reserved-name half from the controller** — the
  retained per-CR claim/adoption model keeps the controller from seizing an
  unadopted foreign object, but **reserved names / project-prefix / tenant↔platform
  disjointness are no longer enforced by the controller**; that enforcement, if a
  cluster wants it, is now downstream **admission-control** work — see *Transparent
  contract, migration, and admission-control policy (Rev 7)*. The original text
  follows as historical.)* Reserved platform names (keyed on the real realm
  identifiers, kept in sync with the realm config) plus a per-CR claim model make
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
