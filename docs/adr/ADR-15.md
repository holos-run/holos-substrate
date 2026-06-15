# Quay↔Keycloak OIDC SSO with PKCE

| Metadata | Value                                  |
|----------|----------------------------------------|
| Date     | 2026-06-13                             |
| Author   | @jeffmccune                            |
| Status   | `Implemented`                          |
| Tags     | registry, oidc, security               |

| Revision | Date       | Author      | Info           |
|----------|------------|-------------|----------------|
| 1        | 2026-06-13 | @jeffmccune | Initial design |

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
(HOL-1219) pointed Quay at that client. This ADR documents the resulting
behavior.

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

## Design

### Login flow: Authorization Code with PKCE (S256)

Quay logs users in through the Keycloak `holos` realm using the OAuth 2.0
Authorization Code flow with PKCE (Proof Key for Code Exchange), challenge
method `S256`. PKCE is enabled on both ends so they agree:

- Quay (`holos/components/quay/buildplan.cue`,
  `KEYCLOAK_LOGIN_CONFIG`): `USE_PKCE: true`, `PKCE_METHOD: S256`.
- The Keycloak `quay` client
  (`holos/components/keycloak/realm-config/buildplan.cue`):
  `attributes."pkce.code.challenge.method": "S256"`.

The decision is to **use PKCE wherever the flow supports it**, as a matter of
modern OAuth best practice and for consistency with the realm's `argocd`
client and the holos reference platform. PKCE binds the authorization code to
the client instance that requested it, so an intercepted code cannot be
redeemed by an attacker — protection that matters for an interactive browser
login regardless of whether the client also holds a secret.

### Confidential client with PKCE layered on

Unlike the public `argocd` client, the `quay` client is **confidential**
(`publicClient: false`, `standardFlowEnabled: true`,
`serviceAccountsEnabled: false`, `directAccessGrantsEnabled: false`). Quay's
`KEYCLOAK_LOGIN_CONFIG` validator requires a `CLIENT_SECRET`, so Quay cannot
run as a public client; PKCE is layered **on top of** the confidential client
rather than replacing the secret. The two protections are complementary: the
secret authenticates the application, PKCE binds the code to the login attempt.

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
   `groups` claim, so Quay team sync and any future relying party key on it the
   same way they key on group names. This only surfaces the role to Quay; it
   does not confer Quay superuser (see `SUPER_USERS` below).
4. A `preferred_username` property mapper writes the username claim.

Quay consumes the single `groups` claim
(`PREFERRED_GROUP_CLAIM_NAME: groups`) for team synchronization
(`FEATURE_TEAM_SYNCING: true`). A Quay **superuser** binds a Quay team to a
Keycloak group (or folded client-role or realm-role name) in the Quay
organization UI —
team-sync setup is a superuser action because this platform leaves
`FEATURE_NONSUPERUSER_TEAM_SYNCING_SETUP` off; thereafter membership flows
automatically. Quay re-syncs team membership on its `TEAM_RESYNC_STALE_TIME`
cadence — **30 minutes** — so role/group changes in Keycloak propagate to Quay
teams within that window rather than instantly.

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
  intent is visible in Keycloak and an org-admin team can be bound to it.
- **Per-project / team access:** grant the user the project's `quay` client
  role (`project-admin` or a per-project role) or add them to the bound
  Keycloak group; a Quay **superuser** binds the matching Quay team to that
  group/role name in the organization UI (Quay restricts team-sync setup to
  superusers unless `FEATURE_NONSUPERUSER_TEAM_SYNCING_SETUP` is enabled, which
  this platform leaves off). Membership then lands on the next team re-sync.

## Decision

Quay authenticates against the Keycloak `holos` realm as a **confidential
OIDC client with PKCE (S256) layered on**, using the Authorization Code flow.
Usernames come from the ID token's `preferred_username` claim with no user
customization, the personal namespace is scoped to that username, and Keycloak
client roles, realm roles, and groups flow through a single `groups` claim into
Quay teams via team syncing — while superuser status comes solely from
`SUPER_USERS`. The
client, roles, mappers, and secret are reconciled declaratively by the
`keycloak-config` Job; nothing secret is committed.

## Consequences

- Quay no longer requires a registry-local password for normal users; SSO is
  the only interactive login (`FEATURE_DIRECT_LOGIN: false`). The local
  `admin` superuser is retained in `SUPER_USERS` as a break-glass account and
  for `scripts/quay-init`.
- PKCE S256 requires Quay 3.16.0+; the component pins **3.17.3**.
- Team membership changes are **eventually consistent** on the 30-minute
  `TEAM_RESYNC_STALE_TIME` cadence, not immediate.
- The `quay-oidc` client secret must exist identically in both the `keycloak`
  and `quay` namespaces; the bootstrap Job enforces this and fails loudly on a
  mismatch.
- The previously **disabled** placeholder `quay` client in the
  `KeycloakRealmImport` CR was superseded by the enabled, reconciled client in
  `keycloak-config` and removed (HOL-1221); the bootstrap import now creates
  only the realm shell, leaving the `quay` client wholly owned by
  `keycloak-config`.
