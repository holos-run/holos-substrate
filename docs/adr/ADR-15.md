# Quay↔Keycloak OIDC SSO

| Metadata | Value                                  |
|----------|----------------------------------------|
| Date     | 2026-06-13                             |
| Author   | @jeffmccune                            |
| Status   | `Implemented`                          |
| Tags     | registry, oidc, security               |

| Revision | Date       | Author      | Info                                                                 |
|----------|------------|-------------|----------------------------------------------------------------------|
| 1        | 2026-06-13 | @jeffmccune | Initial design                                                       |
| 2        | 2026-06-15 | @jeffmccune | HOL-1257: drop PKCE — Quay is a plain confidential client (see below) |
| 3        | 2026-06-16 | @jeffmccune | HOL-1281: run `AUTHENTICATION_TYPE: Database` with Keycloak as a federated login provider (layered, not a backend swap); `FEATURE_TEAM_SYNCING: false`; no-PKCE exception retained |
| 4        | 2026-06-17 | @jeffmccune | HOL-1292/HOL-1293: revert to `AUTHENTICATION_TYPE: OIDC` as the sole identity store; PKCE (S256) re-enabled; team syncing re-enabled; superusers are the Keycloak-backed `svc-quay-resource-controller` + `quay-admin`; the `quay-initial-admin` bootstrap is removed; Quay data-plane provisioning deferred to a future Quay Resource Controller |
| 5        | 2026-06-17 | @jeffmccune | HOL-1299: enable `FEATURE_SUPERUSERS_FULL_ACCESS` so the future Quay Resource Controller can adopt orgs it did not create; clarify the user/org/OAuth-Application distinction and the manual `platform-automation` org bootstrap |
| 6        | 2026-06-17 | @jeffmccune | HOL-1306: the "future Quay Resource Controller" referenced throughout is now designed as the **Holos Controller** ([ADR-18](ADR-18.md)) with `quay.holos.run` Organization/Repository CRDs ([ADR-19](ADR-19.md)); add forward cross-links. The Revision 4 OIDC sole-identity-store model is unchanged. The controller is the intended end state for the **org/repo/robot/webhook provisioning**, which the manual runbook performs until it ships; per ADR-18 the controller still *consumes* the superuser OAuth-Application token this runbook mints (its external credential, read from the `quay`-namespace Secret), not one of the CRDs it reconciles. |
| 7        | 2026-06-18 | @jeffmccune | HOL-1317: **disable PKCE again** for the `quay` client, reversing the Revision 4 re-enablement. Quay 3.17.3 stores the PKCE `code_challenge` state in the `_csrf_token` cookie and never clears it on logout, so a stale `code_verifier` is replayed on the next login and Keycloak rejects the exchange with `code exchange: 400` (login-after-logout fails). The `quay` client now sets `pkce.code.challenge.method` to the empty/"none" method (set explicitly so keycloak-config-cli overwrites any prior `S256` rather than merge-keeping it) and Quay sets `USE_PKCE: false` (no `PKCE_METHOD`); the client's `redirectUris` become the three explicit `/oauth2/keycloak/callback{,/attach,/cli}` paths (no `/*` wildcard). The OIDC sole-identity-store model is otherwise unchanged. |

## Context and Problem Statement

The Quay registry (`quay.holos.internal`) initially authenticated only
against its local database, with no SSO. Platform
users already have identities in the Keycloak `holos` realm and sign in to
Argo CD through it ([ADR-3](ADR-3.md)); requiring a second, registry-local
identity is both poor ergonomics and a second credential store to manage.
This ADR records the decision to make Quay a Single Sign-On relying party of
the `holos` realm — how the login flow is secured, how usernames and
namespaces are derived from the ID token, and how Keycloak roles and groups
map into Quay teams and superusers.

The integration shipped in two phases: Phase 1 (HOL-1218) added the Keycloak
`quay` client, its roles, and protocol mappers to the realm; Phase 2
(HOL-1219) pointed Quay at that client. The backend choice then changed twice:
Revision 3 (HOL-1281) briefly ran `AUTHENTICATION_TYPE: Database` with Keycloak
layered on as a federated login provider, to keep a headless superuser-token
bootstrap working. **Revision 4 (HOL-1292/HOL-1293) reverses that and is the
current model:** Quay runs `AUTHENTICATION_TYPE: OIDC` with the Keycloak `holos`
realm as the **sole** identity store. Two Keycloak realm users —
`svc-quay-resource-controller` (a service account) and `quay-admin` (a human
administrator), both Keycloak-backed and both listed in `SUPER_USERS` — replace
the local `admin` user; the `quay-initial-admin` headless bootstrap is removed,
and in-cluster Quay data-plane provisioning (orgs, repos, robots, webhooks) is
**deferred to a future Quay Resource Controller**. Team syncing is re-enabled;
PKCE, briefly re-enabled in Revision 4, is **disabled again** for the `quay`
client in Revision 7 (HOL-1317) to work around a Quay 3.17.3 logout-state defect.
This ADR documents the resulting behavior.

## References

- [ADR-3 — Authorization via Kubernetes RBAC and group membership](ADR-3.md):
  the platform's authz model keys on Keycloak group membership; the `groups`
  claim is the shared currency between the realm and the relying parties.
- [ADR-8 — Container registry and image tagging](archive/ADR-8.md): the registry this
  ADR adds SSO to.
- The Argo CD OIDC client (`publicClient: true`, PKCE S256), reconciled by the
  same `keycloak-config` Job, is the model this integration follows
  ([components/keycloak/realm-config/buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue)).
- The holos reference Quay OIDC configuration is the authoritative source for
  the exact Quay config keys.
- [Quay↔Keycloak OIDC runbook](../runbooks/quay-keycloak-oidc.md): the
  operational companion to this decision record — how the integration is wired,
  the two superuser realm users, secret rotation, and the `code exchange: 400`
  PKCE-verification note.
- [Quay Resource Controller credentials runbook](../runbooks/quay-resource-controller-credentials.md):
  the manual procedure for minting the future Quay Resource Controller's
  OAuth-Application credential while Quay data-plane provisioning is deferred.
- [ADR-18 — The Holos Controller and the GitOps Rendered-Manifest Delivery
  Model](ADR-18.md): the "future Quay Resource Controller" this ADR defers to is
  designed as the **Holos Controller** (`Updates: ADR-15`). It is the intended
  end state for the org/repo/robot/webhook **provisioning** the manual runbook
  performs until it ships; per ADR-18 the controller still *consumes* the
  superuser OAuth-Application token this runbook mints as its external credential
  (read from the `quay`-namespace Secret), not one of the CRDs it reconciles.
- [ADR-19 — Quay API Group (`quay.holos.run`): Organization and Repository
  CRDs](ADR-19.md): the CRDs the Holos Controller reconciles to automate the
  org/repo/robot/webhook provisioning currently deferred to the manual runbook
  (`Updates: ADR-15`).

## Design

### Identity backend: OIDC — the Keycloak realm is the sole identity store

**Revision 4 (HOL-1292/HOL-1293) is the current model and supersedes Revision
3's Database backend.** Quay runs `AUTHENTICATION_TYPE: OIDC`, which makes the
Keycloak `holos` realm the **sole** identity store: there is no local `admin`
user, and the headless **`/api/v1/user/initialize`** one-shot bootstrap endpoint
(which needs no authentication and only answers against a virgin Database-backed
registry) is unavailable. The `/api/v1/superuser/*` endpoints still exist and
answer an authenticated request from a `SUPER_USERS` member's OAuth token — what
is gone is the *headless* path that minted that first token without an existing
user. The `<PREFIX>_LOGIN_CONFIG` block (here `KEYCLOAK_LOGIN_CONFIG`) is what
selects the OIDC provider Quay authenticates against; under OIDC backend that
provider also owns every user record.

Revision 3 briefly chose Database auth specifically to keep the local `admin`
user and the `/api/v1/user/initialize` bootstrap endpoint available, so a
headless `quay-admin-bootstrap` Job (HOL-1276) could mint a non-expiring
superuser OAuth token (the `quay-initial-admin` Secret, key `token`) that
imperative automation depended on. Revision 4 removes that machinery entirely.
Instead of a local `admin` and a headlessly-minted token, **two Keycloak realm
users are the superusers**:

- **`svc-quay-resource-controller`** — a **service account** (the `svc-` prefix
  marks it as a non-human machine identity), the future Quay Resource
  Controller's identity.
- **`quay-admin`** — a **human** administrator (no prefix).

Both are seeded in the realm by the keycloak phase (HOL-1294) with the
`platform-owner` realm role, each with a password generated once at runtime into
a Kubernetes Secret of the same name in the `keycloak` namespace (key
`password`), and both are listed in `SUPER_USERS` in
`holos/components/quay/buildplan.cue`, matched by `preferred_username ==
username`. Because the headless initialize endpoint is gone (there is no
unattended path to mint a first token), **in-cluster Quay data-plane
provisioning (orgs, repos, robots, webhooks) is deferred to a future Quay
Resource Controller**; until it exists, an operator mints the controller's
OAuth-Application credential by hand following the
[Quay Resource Controller credentials runbook](../runbooks/quay-resource-controller-credentials.md).

Two consequences of the OIDC backend:

- **`FEATURE_TEAM_SYNCING: true`** (with `TEAM_RESYNC_STALE_TIME: 30m`).
  OIDC `groups`-claim → Quay-team synchronization requires an auth backend whose
  user handler implements `sync_user_groups`. Under `AUTHENTICATION_TYPE: OIDC`
  the active handler owns that method, so the SSO callback's
  `sync_oidc_groups()` succeeds rather than `AttributeError`-ing — the failure
  mode that forced team syncing off under the Database backend (Revision 3).
  Team membership is therefore eventually consistent on the 30-minute resync
  cadence from the `groups` claim.
- **PKCE is disabled** for the `quay` client (Revision 7, HOL-1317) — see the
  login-flow section below.

### Login flow: Authorization Code, confidential client without PKCE

Quay logs users in through the Keycloak `holos` realm using the OAuth 2.0
Authorization Code flow, authenticated by the client secret **without** PKCE.
Neither end uses it:

- Quay (`holos/components/quay/buildplan.cue`,
  `KEYCLOAK_LOGIN_CONFIG`): `USE_PKCE: false` (no `PKCE_METHOD`).
- The Keycloak `quay` client
  (`holos/components/keycloak/realm-config/buildplan.cue`):
  `pkce.code.challenge.method` set to the empty/"none" method (set explicitly,
  not omitted, so keycloak-config-cli's attribute merge overwrites any prior
  `S256`).

**Revision 7 (HOL-1317) disables PKCE again, reversing the Revision 4 /
HOL-1293 re-enablement.** PKCE's history for this client is: dropped in
Revision 2 (HOL-1257), re-enabled in Revision 4 (HOL-1293/HOL-1294), and dropped
again here. The reason is a Quay 3.17.3 defect: Quay stores the PKCE
`code_challenge` state in the `_csrf_token` cookie and never clears it on logout,
so a stale `code_verifier` is replayed on the next login and Keycloak rejects the
exchange with `Got non-2XX response for code exchange: 400` — a logged-out user
cannot log back in. Disabling PKCE on both ends removes that stale state. The
`quay` client is therefore the deliberate PKCE exception again: the public
`argocd` and `kargo` clients keep `S256`; only `quay` drops it. Do not re-enable
PKCE for `quay` without first confirming the Quay logout-state defect is fixed.
The same `code exchange: 400` also appears if PKCE is *mismatched* (set on only
one end) — see the
[runbook](../runbooks/quay-keycloak-oidc.md#troubleshooting-got-non-2xx-response-for-code-exchange-400).

### Confidential client authenticated by a client secret

Unlike the public `argocd` client, the `quay` client is **confidential**
(`publicClient: false`, `standardFlowEnabled: true`,
`serviceAccountsEnabled: false`, `directAccessGrantsEnabled: false`). Quay's
`KEYCLOAK_LOGIN_CONFIG` validator requires a `CLIENT_SECRET`, so Quay cannot
run as a public client; the client secret alone authenticates the application —
no PKCE is layered on the code exchange (Revision 7, HOL-1317; see the login-flow
section above).

The shared client secret is the `quay-oidc` Secret (key `client_secret`)
provisioned once by HOL-1218's bootstrap Job into **both** the `keycloak` and
`quay` namespaces, never committed to the repository:

- In `keycloak`, the `keycloak-config-cli` Job reads it via the
  `QUAY_OIDC_CLIENT_SECRET` env var and substitutes it into the realm import
  document's `secret: "$(env:QUAY_OIDC_CLIENT_SECRET)"` placeholder.
- In `quay`, the Quay Deployment's config-rendering initContainer reads it and
  substitutes it into the committed `config.yaml` template's
  `__OIDC_CLIENT_SECRET__` placeholder.

The bootstrap Job creates the Secret only if absent, never overwrites it, and
fails loudly if the two namespaces' copies disagree, so the two ends always
share one secret.

### Realm reconciliation via keycloak-config-cli

The `quay` client, its roles, and its protocol mappers are reconciled
declaratively on every `scripts/apply` by the same idempotent
[keycloak-config-cli](https://github.com/adorsys/keycloak-config-cli) `Job`
that manages the `argocd` client and the platform realm roles (the
`keycloak-config` component). The `KeycloakRealmImport` CR only bootstraps the
realm shell; the Job layers the managed objects on and keeps them converged,
so realm changes land by editing the import document and re-applying — not by
manual admin-console edits.

### Username from the ID token; no customization

The username is taken verbatim from the ID token's `preferred_username` claim
(`PREFERRED_USERNAME_CLAIM_NAME: preferred_username`). Quay never prompts the
user to choose or edit it:

- `FEATURE_USERNAME_CONFIRMATION: false` — accept the token's username with no
  confirmation/edit prompt on first login.
- `FEATURE_DIRECT_LOGIN: false` — remove the local username/password form so
  SSO is the only interactive path.
- `FEATURE_USER_CREATION: true` — first SSO login auto-provisions the user's
  account.

On first login Quay creates the user's **personal namespace**, named for the
`preferred_username` claim. In Quay a user's personal namespace **is** their
per-user organization scope: repositories the user owns live under
`quay.holos.internal/<preferred_username>/...`. This is the per-user
namespace-scoping interpretation of the original issue's AC3 — the namespace
is scoped to the user id from the token, and the user cannot rename it because
the username is not editable.

### Roles and groups → Quay teams and superusers

The realm carries two **`quay` client roles**, defined in
`holos/components/keycloak/realm-config/buildplan.cue`:

- `platform-admin` — the Holos Platform Admin role.
- `project-admin` — per-project administrative access in Quay.

These roles are **identity labels that flow into the `groups` claim** (via the
client-role mapper below) — they do **not** by themselves confer any privilege.
In particular, the `platform-admin` role does **not** make a user a Quay
superuser: superuser status comes solely from `SUPER_USERS` (see below). What a
role grants is whatever Quay team an operator binds the role/group name to in
the Quay organization (e.g. an `admin`-permission team for the org). With
`FEATURE_TEAM_SYNCING: true` (Revision 4) Quay keeps that team membership
eventually consistent with the `groups` claim on the 30-minute
`TEAM_RESYNC_STALE_TIME` cadence. Treat the role names as a convention for who
*should* hold which access, realized through Quay team membership and
`SUPER_USERS`, not as Quay-enforced permissions on their own.

Per-project roles follow the same client-role convention: add a client role
on the `quay` client named for the project, and grant it to the users who
should administer that project.

Four protocol mappers on the `quay` client shape the token Quay consumes:

1. A group-membership mapper writes Keycloak group names (bare, e.g.
   `authenticated`) into the `groups` claim.
2. A client-role mapper (`oidc-usermodel-client-role-mapper`,
   `usermodel.clientRoleMapping.clientId: quay`) **folds the `quay` client
   role names into the same `groups` claim**. Granting a user the `quay`
   `platform-admin` role therefore surfaces `platform-admin` in their `groups`
   claim alongside their group memberships.
3. A realm-role mapper (`oidc-usermodel-realm-role-mapper`, HOL-1245)
   **folds realm-role names — including `platform-owner` (HOL-1242) — into the
   same `groups` claim**, mirroring the `argocd` client. Granting a user the
   `platform-owner` realm role therefore surfaces `platform-owner` in their
   `groups` claim, so with team syncing on (`FEATURE_TEAM_SYNCING: true`, see
   [Identity backend](#identity-backend-oidc--the-keycloak-realm-is-the-sole-identity-store))
   Quay and any future relying party key on it the same way they key on group
   names. This only surfaces the role to Quay; it does not confer Quay superuser
   (see `SUPER_USERS` below).
4. A `preferred_username` property mapper writes the username claim.

Quay receives the single `groups` claim
(`PREFERRED_GROUP_CLAIM_NAME: groups`) on every SSO login. Automatic
group→team synchronization is **enabled** in this revision
(`FEATURE_TEAM_SYNCING: true`, `TEAM_RESYNC_STALE_TIME: 30m`, Revision 4 /
HOL-1293): under `AUTHENTICATION_TYPE: OIDC` the active user handler owns
`sync_user_groups`, so the SSO callback's `sync_oidc_groups()` succeeds — see
[Identity backend](#identity-backend-oidc--the-keycloak-realm-is-the-sole-identity-store)
above. Team membership is eventually consistent on the 30-minute resync cadence;
superuser status is separate and comes solely from `SUPER_USERS` (below).

**Superusers** are not derived from the `groups` claim: Quay superuser status
comes solely from `SUPER_USERS` in the config. Under the OIDC backend there is
no local `admin` user, so the superusers are two **Keycloak realm users** listed
in `SUPER_USERS` by their `preferred_username`:
**`svc-quay-resource-controller`** (a service account — the `svc-` prefix marks
it as a non-human machine identity, the future Quay Resource Controller's
identity) and **`quay-admin`** (a human administrator). Both are seeded with the
`platform-owner` realm role by the keycloak phase (HOL-1294), each with a
password generated once at runtime into a Secret of the same name in the
`keycloak` namespace (key `password`). The old `quay-initial-admin` headless
bootstrap (HOL-1276) is removed — the OIDC backend disables the
`/api/v1/user/initialize` endpoint it relied on. Because that endpoint is gone,
in-cluster Quay data-plane provisioning (orgs, repos, robots, webhooks) is
**deferred to a future Quay Resource Controller**; an operator mints its
OAuth-Application credential by hand per the
[Quay Resource Controller credentials runbook](../runbooks/quay-resource-controller-credentials.md).

### Data-plane provisioning: the controller credential and `FEATURE_SUPERUSERS_FULL_ACCESS`

In-cluster data-plane provisioning (orgs, repos, robots, webhooks) is deferred to
a future Quay Resource Controller — now designed as the **Holos Controller**
([ADR-18](ADR-18.md)) with the `quay.holos.run` Organization/Repository CRDs
([ADR-19](ADR-19.md), `Updates: ADR-15`). Until that controller ships, an
operator mints the controller's credential by hand per the
[Quay Resource Controller credentials runbook](../runbooks/quay-resource-controller-credentials.md).
Three Quay concepts must stay distinct to understand that credential (HOL-1299):

- A **user** signs in (`svc-quay-resource-controller`, `quay-admin`) and owns a
  personal namespace named for the username.
- An **organization** is a shared namespace owning repos/teams/robots/webhooks.
  Organizations — **not users** — are the only place an **OAuth Application** can
  be created (the Applications tab exists on an org, never on a personal
  namespace). An OAuth Application/token therefore **cannot** be created directly
  "for" a user; it is created inside an org the user administers.
- An **OAuth Application token** acts as the **user who generated it**, bounded
  by that user's rights on each target namespace and the token's scopes — **not**
  by the org that hosts the Application. The host org is where the credential
  record lives; it is **not a permission boundary**.

The credential is minted while signed in as `svc-quay-resource-controller`, in a
dedicated **`platform-automation`** org that user owns. Its abilities split into
two cases:

- **Orgs the controller creates** (e.g. `my-project`): the creating user becomes
  owner/admin automatically and administers them through the normal endpoints —
  no extra configuration. This is the clean reconcile-from-scratch path.
- **Orgs the controller did not create**: by default a superuser has no access to
  another user's org — `super:user` reaches only the `/api/v1/superuser/*` panel
  endpoints, so a write inside a non-owned org returns `403`.
  **`FEATURE_SUPERUSERS_FULL_ACCESS: true`** (HOL-1299, set in
  `holos/components/quay/buildplan.cue`) grants `SUPER_USERS` read/write/delete on
  namespaces and orgs they do not own, so the controller can **adopt** and
  reconcile any org on the instance through the normal endpoints. The flag
  applies to `SUPER_USERS` members only, but to **all** of their superuser
  sessions: Quay grants superuser permission for the `super:user` OAuth scope
  **or** the internal `direct_user_login` scope used by authenticated web
  sessions, so the human `quay-admin` signed in through the UI also gains
  instance-wide read/write/delete across every org — not only the controller's
  OAuth token. It is not configurable per-user and does not widen access for
  ordinary (non-`SUPER_USERS`) users; including `quay-admin` is an acceptable
  widening of an existing platform administrator's reach.

This is enabled deliberately: a reconciler that is the system of record must
converge *any* org — including one a human pre-created or another automation made
— not only orgs it created itself; without the flag it would `403` on those
namespaces and silently fail to reconcile them.

### How an operator grants access

- **Platform Admin (superuser):** add the user's `preferred_username` to
  `SUPER_USERS` in `holos/components/quay/buildplan.cue` and re-render/apply.
  This is the only way to confer Quay superuser; the `platform-admin` client
  role does not. Optionally also grant the `quay` `platform-admin` role so the
  intent is visible in Keycloak; with `FEATURE_TEAM_SYNCING: true` the matching
  Quay team's membership tracks the `groups` claim automatically on the resync
  cadence.
- **Per-project / team access:** grant the user the project's `quay` client
  role (`project-admin` or a per-project role) or add them to the bound
  Keycloak group; Quay binds the matching team's membership from the `groups`
  claim automatically (`FEATURE_TEAM_SYNCING: true`, Revision 4) on the
  30-minute `TEAM_RESYNC_STALE_TIME` cadence. The one-time setup of the
  team→group binding (and the team's org role / default repository permission) no
  longer requires a superuser clicking through the organization UI: it is now
  declared on the Organization CR's `spec.syncedTeams` and reconciled by the
  Holos Controller ([ADR-19](ADR-19.md), Revision 6). `FEATURE_TEAM_SYNCING`
  still keeps the synced team's *membership* current from the claim.

## Decision

Quay runs `AUTHENTICATION_TYPE: OIDC` — the Keycloak `holos` realm is the
**sole** identity store (Revision 4, HOL-1292/HOL-1293): the "Holos SSO" button
logs users in through the realm via the Authorization Code flow, and there is no
local `admin` user. Quay authenticates to the token endpoint as a **confidential
OIDC client with a client secret and no PKCE** (Revision 7 / HOL-1317 disables
PKCE again, reversing the Revision 4 re-enablement, to work around the Quay
3.17.3 logout-state defect). Usernames come from the ID token's
`preferred_username` claim with no user customization, and the personal
namespace is scoped to that username. Keycloak client roles, realm roles, and
groups are folded into a single `groups` claim that Quay receives on each login;
automatic group→team syncing is **on** (`FEATURE_TEAM_SYNCING: true`,
`TEAM_RESYNC_STALE_TIME: 30m`) because the OIDC user handler can sync groups.
Superuser status is separate and comes solely from `SUPER_USERS`, which lists
two Keycloak realm users: the service account `svc-quay-resource-controller` and
the human `quay-admin` (both `platform-owner`, passwords generated at runtime
into Secrets in the `keycloak` namespace). The `quay-initial-admin` headless
bootstrap is removed, and in-cluster Quay data-plane provisioning is deferred to
a future Quay Resource Controller. The client, roles, mappers, and secret are
reconciled declaratively by the `keycloak-config` Job; nothing secret is
committed.

## Consequences

- Quay has **no** registry-local identities; SSO is the only interactive login
  (`FEATURE_DIRECT_LOGIN: false`) and the Keycloak realm is the sole identity
  store. The two superusers are the Keycloak realm users
  `svc-quay-resource-controller` and `quay-admin` in `SUPER_USERS`.
- PKCE is **not** used for the `quay` client (Revision 7, HOL-1317): the client
  sets `pkce.code.challenge.method` to the empty/"none" method and Quay sets
  `USE_PKCE: false`. PKCE was dropped (Revision 2 / HOL-1257), re-enabled
  (Revision 4 / HOL-1293), then dropped again here because Quay 3.17.3 replays a
  stale `code_verifier` from the `_csrf_token` cookie after logout, breaking the
  next login with `code exchange: 400`. The `quay` client is the deliberate PKCE
  exception; the public `argocd` and `kargo` clients keep `S256`. Re-enabling
  PKCE for `quay` requires first confirming the Quay logout-state defect is fixed.
- Automatic group→team synchronization is **enabled** in this revision
  (`FEATURE_TEAM_SYNCING: true`, `TEAM_RESYNC_STALE_TIME: 30m`, Revision 4 /
  HOL-1293): the OIDC user handler syncs groups, so Quay team membership is
  eventually consistent with the `groups` claim on the 30-minute resync cadence.
- The OIDC backend disables the local `admin` user and the headless
  `/api/v1/user/initialize` bootstrap endpoint (the `/api/v1/superuser/*` APIs
  still answer an authenticated `SUPER_USERS` member's token; what is gone is the
  headless mint of that first token), so the `quay-admin-bootstrap` Job and the
  `quay-initial-admin` superuser token are removed (HOL-1293). In-cluster Quay data-plane provisioning (orgs, repos,
  robots, webhooks) is **deferred to a future Quay Resource Controller**; until
  it ships, an operator mints the controller's OAuth-Application credential by
  hand per the
  [Quay Resource Controller credentials runbook](../runbooks/quay-resource-controller-credentials.md).
- **`FEATURE_SUPERUSERS_FULL_ACCESS: true`** (HOL-1299) extends the `SUPER_USERS`
  identities' reach to orgs they neither own nor are members of, so the future
  Quay Resource Controller can adopt and reconcile orgs created by other
  identities rather than `403`-ing on them. It applies to `SUPER_USERS` members
  only, but to all of their superuser sessions — both a `super:user`-scoped OAuth
  token and an authenticated web/UI session (`direct_user_login`) — so the human
  `quay-admin` also gains instance-wide full access through the UI; it is not
  configurable per-user and does not widen access for non-`SUPER_USERS` users.
  The credential itself lives as an OAuth
  Application in a dedicated `platform-automation` org owned by
  `svc-quay-resource-controller` — the host org is where the credential record
  lives, not a permission boundary.
- The `quay-oidc` client secret must exist identically in both the `keycloak`
  and `quay` namespaces; the bootstrap Job enforces this and fails loudly on a
  mismatch.
- The previously **disabled** placeholder `quay` client in the
  `KeycloakRealmImport` CR was superseded by the enabled, reconciled client in
  `keycloak-config` and removed (HOL-1221); the bootstrap import now creates
  only the realm shell, leaving the `quay` client wholly owned by
  `keycloak-config`.
