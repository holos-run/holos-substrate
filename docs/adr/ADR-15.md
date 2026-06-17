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

## Context and Problem Statement

The Quay registry (`quay.holos.localhost`) initially authenticated only
against its local database, bootstrapped by `scripts/quay-init`. Platform
users already have identities in the Keycloak `holos` realm and sign in to
Argo CD through it ([ADR-3](ADR-3.md)); requiring a second, registry-local
identity is both poor ergonomics and a second credential store to manage.
This ADR records the decision to make Quay a Single Sign-On relying party of
the `holos` realm — how the login flow is secured, how usernames and
namespaces are derived from the ID token, and how Keycloak roles and groups
map into Quay teams and superusers.

The integration shipped in two phases: Phase 1 (HOL-1218) added the Keycloak
`quay` client, its roles, and protocol mappers to the realm; Phase 2
(HOL-1219) pointed Quay at that client. **Revision 3 (HOL-1281)** then refined
*how* Quay consumes that client: Quay keeps its own database as the identity
store (`AUTHENTICATION_TYPE: Database`) and treats Keycloak as a *federated
login provider* layered on top, rather than making the OIDC provider Quay's sole
identity backend — so the headless superuser bootstrap and human SSO login
coexist. This ADR documents the resulting behavior.

## References

- [ADR-3 — Authorization via Kubernetes RBAC and group membership](ADR-3.md):
  the platform's authz model keys on Keycloak group membership; the `groups`
  claim is the shared currency between the realm and the relying parties.
- [ADR-8 — Container registry and image tagging](ADR-8.md): the registry this
  ADR adds SSO to.
- The Argo CD OIDC client (`publicClient: true`, PKCE S256), reconciled by the
  same `keycloak-config` Job, is the model this integration follows
  ([components/keycloak/realm-config/buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue)).
- The holos reference Quay OIDC configuration is the authoritative source for
  the exact Quay config keys.
- [Quay↔Keycloak OIDC runbook](../runbooks/quay-keycloak-oidc.md): the
  operational companion to this decision record — how the integration is wired,
  the no-PKCE exception, secret rotation, and the `code exchange: 400`
  troubleshooting.

## Design

### Identity backend: Database, with Keycloak as a *federated login provider*

**Revision 3 (HOL-1281) supersedes the original "OIDC backend" framing.** Quay
runs `AUTHENTICATION_TYPE: Database` — its own Postgres database remains the
identity store — while `KEYCLOAK_LOGIN_CONFIG` layers Keycloak "Holos SSO" on
top as a **federated login provider**. This is a *layered* arrangement, not a
backend swap: the same `KEYCLOAK_LOGIN_CONFIG` block still drives the
interactive "Holos SSO" login (Quay treats any `<PREFIX>_LOGIN_CONFIG` block as
a federated SSO provider regardless of `AUTHENTICATION_TYPE`), but the local
database — not the OIDC provider — owns user records.

The original design used `AUTHENTICATION_TYPE: OIDC`, which makes the OIDC
provider the **sole** identity store. Under that mode Quay disables the local
`admin` user and the `/api/v1/user/initialize` + `/api/v1/superuser/*` REST
APIs, so the headless `quay-admin-bootstrap` Job (HOL-1276) could never mint the
superuser OAuth token that downstream automation depends on (the
`quay-initial-admin` Secret, key `token`). Database auth restores those
endpoints while keeping SSO, so the human SSO login and the headless superuser
bootstrap coexist — the rationale for this revision.

Two consequences of choosing Database auth over OIDC:

- **`FEATURE_TEAM_SYNCING: false`** (and `TEAM_RESYNC_STALE_TIME` is dropped).
  OIDC `groups`-claim → Quay-team synchronization requires a federated auth
  backend whose user handler implements `sync_user_groups`. Under
  `AUTHENTICATION_TYPE: Database` the active handler is `DatabaseUsers`, which
  has no such method (only `OIDCUsers`/`LDAPUsers` do). Quay's OAuth login path
  calls `sync_oidc_groups()` on every SSO callback when team syncing is on with
  a `groups` claim present, so leaving it enabled would `AttributeError` → HTTP
  500 on every "Holos SSO" login (Quay v3.17.3 `oauth/login_utils.py` →
  `auth_system.sync_user_groups`). Automatic group→team sync therefore returns
  only if a future phase moves to a federated backend; for now superuser comes
  solely from `SUPER_USERS` and Quay teams are managed directly. The `groups`
  claim is still emitted (the mappers below are unchanged) so the data is ready
  when team syncing can be re-enabled.
- **The no-PKCE exception (Revision 2, HOL-1257) is retained unchanged.** The
  backend change does not touch the login flow — Quay still authenticates to the
  token endpoint as a plain confidential client with its client secret, and
  neither end sets PKCE.

A one-time **reset** may be required to adopt this model on a cluster that
already ran under the broken OIDC-only config: `/api/v1/user/initialize` only
answers against a *virgin* registry, so a database that already holds
SSO-provisioned users keeps it closed and prevents the bootstrap Job from ever
minting the token. The deliberate, separate `scripts/quay-reset` helper
(HOL-1283) wipes the Quay database so initialize re-opens; the next
`scripts/apply` re-runs the bootstrap against the now-virgin DB. See the
[runbook](../runbooks/quay-keycloak-oidc.md#one-time-reset-re-opening-initialize)
for the procedure and when it is needed.

### Login flow: Authorization Code, confidential client (no PKCE)

Quay logs users in through the Keycloak `holos` realm using the OAuth 2.0
Authorization Code flow, authenticated by the client secret. PKCE is **not**
used for this client — neither end sets it:

- Quay (`holos/components/quay/buildplan.cue`,
  `KEYCLOAK_LOGIN_CONFIG`): no `USE_PKCE`/`PKCE_METHOD` (Quay defaults
  `USE_PKCE` to `false`, so it sends no `code_challenge`).
- The Keycloak `quay` client
  (`holos/components/keycloak/realm-config/buildplan.cue`): no
  `attributes."pkce.code.challenge.method"` (Keycloak treats a client that
  sets it as *requiring* PKCE).

**Revision 2 (HOL-1257) supersedes the original decision to use PKCE.** The
initial design enabled PKCE (S256) on both ends for consistency with the
public `argocd` client. In practice Quay's confidential OIDC client did not
reliably round-trip a matching PKCE `code_verifier` at the token endpoint:
Quay sent a `code_challenge` while the Keycloak client *required* PKCE, and a
missing/mismatched `code_verifier` produced `Got non-2XX response for code
exchange: 400`, blocking SSO login entirely. Removing PKCE from **both** ends
together — the only mutually-consistent state — restores login. Quay then
authenticates purely as a plain confidential client with its client secret,
which is Red Hat's recommended baseline Quay↔Keycloak OIDC integration. This
is the documented exception to the platform's "use PKCE wherever the flow
supports it" default ([keycloak-clients.md](../../holos/docs/keycloak-clients.md),
and the related HOL-1233 note in `AGENTS.md`); the public `argocd` and `kargo`
clients keep PKCE.

### Confidential client authenticated by a client secret

Unlike the public `argocd` client, the `quay` client is **confidential**
(`publicClient: false`, `standardFlowEnabled: true`,
`serviceAccountsEnabled: false`, `directAccessGrantsEnabled: false`). Quay's
`KEYCLOAK_LOGIN_CONFIG` validator requires a `CLIENT_SECRET`, so Quay cannot
run as a public client; the client secret alone authenticates the application
(PKCE is not layered on — see the login-flow section above).

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
`quay.holos.localhost/<preferred_username>/...`. This is the per-user
namespace-scoping interpretation of the original issue's AC3 — the namespace
is scoped to the user id from the token, and the user cannot rename it because
the username is not editable.

### Roles and groups → Quay teams and superusers

The realm carries two **`quay` client roles**, defined in
`holos/components/keycloak/realm-config/buildplan.cue`:

- `platform-admin` — the Holos Platform Admin role.
- `project-admin` — per-project administrative access in Quay.

These roles are **identity labels that flow into the `groups` claim** (via the
client-role mapper below) for an operator to bind to a Quay team — they do
**not** by themselves confer any privilege. In particular, the
`platform-admin` role does **not** make a user a Quay superuser: superuser
status comes solely from `SUPER_USERS` (see below). What a role grants is
whatever the bound Quay team is given in the Quay organization (e.g. an
`admin`-permission team for the org). Treat the role names as a convention for
who *should* hold which access, realized through team bindings and
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
   `groups` claim, so once team syncing is re-enabled on a federated backend (it
   is `FEATURE_TEAM_SYNCING: false` today — see
   [Identity backend](#identity-backend-database-with-keycloak-as-a-federated-login-provider))
   Quay and any future relying party key on it the same way they key on group
   names. This only surfaces the role to Quay; it does not confer Quay superuser
   (see `SUPER_USERS` below).
4. A `preferred_username` property mapper writes the username claim.

Quay receives the single `groups` claim
(`PREFERRED_GROUP_CLAIM_NAME: groups`) on every SSO login. Automatic
group→team synchronization is **disabled** in this revision
(`FEATURE_TEAM_SYNCING: false`, Revision 3 / HOL-1281): under
`AUTHENTICATION_TYPE: Database` the active user handler (`DatabaseUsers`) cannot
sync groups, so enabling it would 500 every "Holos SSO" login — see
[Identity backend](#identity-backend-database-with-keycloak-as-a-federated-login-provider)
above. The claim is still emitted so the data is ready when a future federated
backend can consume it; for now Quay teams are managed directly by a superuser,
and superuser status comes solely from `SUPER_USERS` (below). Automatic
group/role-name → team binding (a superuser action, since this platform leaves
`FEATURE_NONSUPERUSER_TEAM_SYNCING_SETUP` off) returns only when team syncing is
re-enabled on a federated backend.

**Superusers** are not derived from the `groups` claim: Quay superuser status
comes solely from `SUPER_USERS` in the config. Bootstrap platform admins are
listed there by their `preferred_username` claim value. The shipped config
keeps the local `admin` entry that `scripts/quay-init` (HOL-1177) creates, so
the break-glass superuser still works with `FEATURE_DIRECT_LOGIN: false`.

### How an operator grants access

- **Platform Admin (superuser):** add the user's `preferred_username` to
  `SUPER_USERS` in `holos/components/quay/buildplan.cue` and re-render/apply.
  This is the only way to confer Quay superuser; the `platform-admin` client
  role does not. Optionally also grant the `quay` `platform-admin` role so the
  intent is visible in Keycloak; while `FEATURE_TEAM_SYNCING: false` a superuser
  realizes that intent by managing the org-admin team's membership directly
  (automatic team binding returns when team syncing is re-enabled).
- **Per-project / team access:** grant the user the project's `quay` client
  role (`project-admin` or a per-project role) or add them to the bound
  Keycloak group; a Quay **superuser** manages the matching Quay team's
  membership directly in the organization UI. Automatic group/role-name → team
  binding is not active while `FEATURE_TEAM_SYNCING: false` (Revision 3); the
  `groups` claim is still emitted, so it returns automatically once team syncing
  can be re-enabled on a federated backend.

## Decision

Quay runs `AUTHENTICATION_TYPE: Database` — its own database is the identity
store — with the Keycloak `holos` realm layered on as a **federated login
provider** (Revision 3, HOL-1281): the "Holos SSO" button logs users in through
the realm via the Authorization Code flow, while Database auth keeps the local
`admin` user and the `/api/v1/user/initialize` + `/api/v1/superuser/*` APIs that
the headless superuser bootstrap (HOL-1276) needs. Quay authenticates to the
token endpoint as a **plain confidential OIDC client with a client secret,
without PKCE** (Revision 2, HOL-1257).
Usernames come from the ID token's `preferred_username` claim with no user
customization, and the personal namespace is scoped to that username. Keycloak
client roles, realm roles, and groups are folded into a single `groups` claim
that Quay receives on each login; automatic group→team syncing is **off**
(`FEATURE_TEAM_SYNCING: false`) because the Database user handler cannot sync
groups, so Quay teams are managed directly and superuser status comes solely
from `SUPER_USERS`. The client, roles, mappers, and secret are reconciled
declaratively by the `keycloak-config` Job; nothing secret is committed.

## Consequences

- Quay no longer requires a registry-local password for normal users; SSO is
  the only interactive login (`FEATURE_DIRECT_LOGIN: false`). The local
  `admin` superuser is retained in `SUPER_USERS` as a break-glass account and
  for `scripts/quay-init`.
- PKCE is deliberately **not** used for the `quay` client (Revision 2,
  HOL-1257): Quay's confidential client-secret flow did not reliably round-trip
  a PKCE `code_verifier`, producing `code exchange: 400` SSO failures, so PKCE
  was removed from both ends. This is the documented exception to the platform's
  PKCE-by-default posture; the public `argocd` and `kargo` clients keep PKCE.
- Automatic group→team synchronization is **disabled** in this revision
  (`FEATURE_TEAM_SYNCING: false`, Revision 3 / HOL-1281): the Database user
  handler cannot sync OIDC groups, so a superuser manages Quay team membership
  directly. The `groups` claim is still emitted, so re-enabling team syncing on
  a future federated backend needs no realm change. The previous behavior —
  eventually-consistent membership on the 30-minute `TEAM_RESYNC_STALE_TIME`
  cadence — does not apply while team syncing is off.
- Database auth keeps the local `admin` superuser and the
  `/api/v1/user/initialize` + `/api/v1/superuser/*` REST APIs available, so the
  headless `quay-admin-bootstrap` Job (HOL-1276) can mint the `quay-initial-admin`
  superuser token. `AUTHENTICATION_TYPE: OIDC` would disable both. Adopting this
  model on a cluster that previously ran the OIDC-only config may require the
  one-time `scripts/quay-reset` (HOL-1283) to re-open the one-shot initialize
  endpoint — see the [runbook](../runbooks/quay-keycloak-oidc.md#one-time-reset-re-opening-initialize).
- The `quay-oidc` client secret must exist identically in both the `keycloak`
  and `quay` namespaces; the bootstrap Job enforces this and fails loudly on a
  mismatch.
- The previously **disabled** placeholder `quay` client in the
  `KeycloakRealmImport` CR was superseded by the enabled, reconciled client in
  `keycloak-config` and removed (HOL-1221); the bootstrap import now creates
  only the realm shell, leaving the `quay` client wholly owned by
  `keycloak-config`.
