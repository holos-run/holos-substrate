# Runbook: Quay↔Keycloak OIDC SSO

Operational runbook for the Quay registry's Single Sign-On integration with the
Keycloak `holos` realm. Written for SREs and on-call operators: how the
integration is wired, the two Keycloak-backed superuser realm users, the
day-to-day operations (granting access, rotating the secret, forcing a
reconcile), and how to triage the
`Got non-2XX response for code exchange: 400` login failure.

The binding **decision record** is
[ADR-15 — Quay↔Keycloak OIDC SSO](../adr/ADR-15.md); this runbook is its
operational companion and does not restate the full rationale. The declarative
Keycloak client pattern and the PKCE guardrail checklist live in
[holos/docs/keycloak-clients.md](../../holos/docs/keycloak-clients.md). The
manual procedure for minting the future Quay Resource Controller's
OAuth-Application credential is the
[Quay Resource Controller credentials runbook](quay-resource-controller-credentials.md).

## Overview

Quay (`quay.holos.localhost`) runs `AUTHENTICATION_TYPE: OIDC` — the Keycloak
`holos` realm is the **sole** identity store (ADR-15 Revision 4, HOL-1293).
There is no local `admin` user; every Quay identity is a realm user. Users sign
in with the "Holos SSO" button through the OAuth 2.0 Authorization Code flow;
Quay authenticates to the token endpoint as a **confidential client** with a
client secret **and PKCE (S256)**.

Usernames come from the ID token's `preferred_username` claim, and Keycloak
groups, client roles, and the `platform-owner` realm role are folded into a
single `groups` claim Quay receives on each login. Automatic group→team
synchronization is **on** (`FEATURE_TEAM_SYNCING: true`,
`TEAM_RESYNC_STALE_TIME: 30m`): under the OIDC backend the active user handler
syncs groups, so Quay team membership is eventually consistent with the claim on
the 30-minute resync cadence. Quay superuser status is **not** claim-driven — it
comes solely from the static `SUPER_USERS` config list, which names two Keycloak
realm users: the service account `svc-quay-resource-controller` and the human
`quay-admin` — see
[The two superuser realm users](#the-two-superuser-realm-users).

The integration has three moving parts, all reconciled declaratively on every
`scripts/apply`:

- The Keycloak `quay` client, its roles, and its protocol mappers — reconciled
  by the `keycloak-config` keycloak-config-cli Job.
- The shared `quay-oidc` client secret — bootstrapped once into both the
  `keycloak` and `quay` namespaces, never committed.
- Quay's `KEYCLOAK_LOGIN_CONFIG` — rendered into Quay's `config.yaml` with the
  secret substituted at runtime.

For the full behavioral contract (the OIDC backend model, username derivation,
namespace scoping, the role/group model, and team syncing), read
[ADR-15](../adr/ADR-15.md).

## How it's wired

### The confidential `quay` client

The `quay` client is defined in
[holos/components/keycloak/realm-config/buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue)
(the `quay` entry in `REALM_CONFIG.clients`). Key fields:

- `publicClient: false` — confidential; Quay authenticates with a client
  secret. Quay's `KEYCLOAK_LOGIN_CONFIG` validator **requires** a
  `CLIENT_SECRET`, so Quay cannot run as a public client.
- `standardFlowEnabled: true` — the browser Authorization Code flow.
- `serviceAccountsEnabled: false`, `directAccessGrantsEnabled: false` — the
  other confidential flows are deliberately off; only the browser code flow is
  used.
- `secret: "$(env:QUAY_OIDC_CLIENT_SECRET)"` — substituted at reconcile time
  from the bootstrap secret (see below), never a committed value.
- `attributes."pkce.code.challenge.method": "S256"` — PKCE is required on the
  code exchange, matching Quay's `USE_PKCE: true` / `PKCE_METHOD: "S256"`. See
  [PKCE on the `quay` client](#pkce-on-the-quay-client).
- `redirectUris: ["https://quay.holos.localhost/*"]` — the wildcard covers
  Quay's callback (see [Redirect-URI convention](#redirect-uri-convention)).

### The client-secret bootstrap

The shared secret is the `quay-oidc` Kubernetes Secret (key `client_secret`),
generated **once** by the `quay-oidc` bootstrap Job (`QUAY_OIDC_BOOTSTRAP` in
[holos/components/keycloak/realm-config/buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue))
and written into **both** namespaces:

- In `keycloak`: the keycloak-config-cli Job reads it via the
  `QUAY_OIDC_CLIENT_SECRET` env var and substitutes it into the realm import
  document's `secret: "$(env:QUAY_OIDC_CLIENT_SECRET)"` placeholder.
- In `quay`: the Quay Deployment's config-rendering initContainer reads it and
  substitutes it into the committed `config.yaml` template's
  `__OIDC_CLIENT_SECRET__` placeholder.

The Job creates the Secret only if absent, **never rotates it**, and fails
loudly if the two namespaces' copies disagree — so Keycloak and Quay always
authenticate with the same secret.

### Quay's `KEYCLOAK_LOGIN_CONFIG`

Quay's side is the `KEYCLOAK_LOGIN_CONFIG` block in
[holos/components/quay/buildplan.cue](../../holos/components/quay/buildplan.cue)
(`CONFIG`). The operationally relevant keys:

| Key | Value | Notes |
|-----|-------|-------|
| `OIDC_SERVER` | the realm issuer URL | **Required trailing slash** — see below |
| `CLIENT_ID` | the `quay` client ID | matches the Keycloak `clientId` |
| `CLIENT_SECRET` | `__OIDC_CLIENT_SECRET__` | substituted from `quay-oidc` at runtime |
| `SERVICE_NAME` | `Holos SSO` | the label on the Quay login button |
| `LOGIN_SCOPES` | `openid`, `profile`, `email`, `groups`, `offline_access` | |
| `PREFERRED_USERNAME_CLAIM_NAME` | `preferred_username` | username, verbatim |
| `PREFERRED_GROUP_CLAIM_NAME` | `groups` | the group/role-name claim; team syncing from it is **on** (`FEATURE_TEAM_SYNCING: true`) |
| `USE_PKCE` / `PKCE_METHOD` | `true` / `S256` | PKCE on the code exchange, matching the client's `pkce.code.challenge.method` |

### Issuer trailing-slash requirement

`OIDC_SERVER` is the realm issuer with a **required trailing slash**. Quay's
config validator normalizes the issuer to `TrimSuffix(issuer, "/") + "/"`, so
the value must end in `/` to match Keycloak's advertised `issuer` exactly. A
missing or doubled slash causes issuer-mismatch token-exchange failures.

### Redirect-URI convention

Quay's OIDC callback URL is
`https://quay.holos.localhost/oauth2/<service-id>/callback`, where
`<service-id>` is the lowercase prefix of the login-config key before
`_LOGIN_CONFIG` — here `keycloak` (from `KEYCLOAK_LOGIN_CONFIG`), giving
`https://quay.holos.localhost/oauth2/keycloak/callback`. The Keycloak client's
`redirectUris` is the wildcard `https://quay.holos.localhost/*`, which covers
that callback.

### Reconciliation and the apply-order gate

The `quay` client, its roles, and its mappers are reconciled on every
`scripts/apply` by the idempotent `keycloak-config` keycloak-config-cli Job —
the same Job that manages the `argocd` and `kargo` clients and the platform
realm roles. The `KeycloakRealmImport` CR only bootstraps the realm shell; the
Job layers the managed objects on and keeps them converged. Realm changes land
by editing the import document and re-applying, **not** by manual admin-console
edits (which the Job will revert). The `keycloak-config` component sits between
`keycloak` and `quay` in the apply order so the client exists before Quay tries
to use it — see
[holos/docs/keycloak-clients.md](../../holos/docs/keycloak-clients.md).

## The two superuser realm users

Under `AUTHENTICATION_TYPE: OIDC` the Keycloak realm is the sole identity store,
so there is no local `admin` user. The Quay superusers are two **Keycloak realm
users**, listed in `SUPER_USERS` in
[holos/components/quay/buildplan.cue](../../holos/components/quay/buildplan.cue)
by their `preferred_username` (matched `preferred_username == username`):

| User | Kind | Realm role | Password Secret (namespace `keycloak`) | Key |
|------|------|------------|----------------------------------------|-----|
| `svc-quay-resource-controller` | service account (`svc-` prefix) | `platform-owner` | `svc-quay-resource-controller` | `password` |
| `quay-admin` | human administrator | `platform-owner` | `quay-admin` | `password` |

The `svc-` prefix marks `svc-quay-resource-controller` as a non-human **service
account** — the future Quay Resource Controller's machine identity — distinct
from the human `quay-admin` administrator. Both are seeded in the realm by the
keycloak phase (HOL-1294); each user's password is generated **once at runtime**
by the `quay-user-password-bootstrap` Job into a Secret of the same name in the
`keycloak` namespace, under the `password` key. Nothing secret is committed
(generate-once, mirroring `keycloak-initial-admin`).

This replaces the old Database-backend `quay-initial-admin` superuser token: the
OIDC backend disables the local `admin` user and the `/api/v1/user/initialize` +
`/api/v1/superuser/*` bootstrap endpoints, so there is no headless token to mint.
In-cluster Quay data-plane provisioning (orgs, repos, robots, webhooks) is
**deferred to a future Quay Resource Controller**; until it ships, an operator
mints the controller's OAuth-Application credential by hand — see the
[Quay Resource Controller credentials runbook](quay-resource-controller-credentials.md).

### Retrieve a superuser password

Retrieve a generated password with `kubectl ... -o jsonpath` and base64-decode:

```bash
# The service account:
kubectl -n keycloak get secret svc-quay-resource-controller \
  -o jsonpath='{.data.password}' | base64 -d; echo

# The human administrator:
kubectl -n keycloak get secret quay-admin \
  -o jsonpath='{.data.password}' | base64 -d; echo
```

Sign in to Quay through **"Holos SSO"** with the username (the Secret name) and
the retrieved password.

### Verify superuser access

Sign in to `https://quay.holos.localhost/` as `quay-admin` (or
`svc-quay-resource-controller`) via "Holos SSO". A superuser sees the **Super
User Admin Panel** in the Quay UI. To verify over the API, generate a token for
the user (an OAuth Application token with `super:user` scope — see the
[credentials runbook](quay-resource-controller-credentials.md)) and confirm the
superuser endpoint answers — expect **HTTP 200**:

```bash
TOKEN='<a token generated for a SUPER_USERS member>'
curl -sS -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer ${TOKEN}" \
  https://quay.holos.localhost/api/v1/superuser/users/
# => 200
```

A `401`/`403` means the token's user is not in `SUPER_USERS` or the token lacks
`super:user` scope; a connection error means Quay is not serving or the local
CA/DNS is not trusted (see [docs/local-cluster.md](../local-cluster.md)).

## PKCE on the `quay` client

The platform standard is **PKCE for OIDC clients** (`S256`), and the `quay`
client follows it: it carries `attributes."pkce.code.challenge.method": "S256"`,
matching Quay's `USE_PKCE: true` / `PKCE_METHOD: "S256"`. The public `argocd` and
`kargo` clients carry the same attribute — `quay` is **no longer** an exception
(ADR-15 Revision 4, HOL-1293).

Both ends must agree on PKCE. Quay sends a `code_challenge`; Keycloak (which
treats a client that sets `pkce.code.challenge.method` as *requiring* PKCE)
verifies the matching `code_verifier` at the token endpoint. If PKCE is set on
only one end — e.g. the client attribute is dropped but Quay still sends a
challenge, or vice versa — the token exchange fails with
`Got non-2XX response for code exchange: 400`. Keep both ends in sync:

- Quay (`holos/components/quay/buildplan.cue`): `USE_PKCE: true`,
  `PKCE_METHOD: "S256"` in `KEYCLOAK_LOGIN_CONFIG`.
- The Keycloak `quay` client
  (`holos/components/keycloak/realm-config/buildplan.cue`):
  `attributes."pkce.code.challenge.method": "S256"`.

> **History.** PKCE was briefly **disabled** for the `quay` client (Revision 2,
> HOL-1257) after Quay's confidential client-secret exchange failed to
> round-trip a matching `code_verifier`, producing `code exchange: 400`.
> Revision 4 (HOL-1293) re-enabled it on both ends, matching the production
> reference configuration; the `code exchange: 400` symptom is now a PKCE
> **verification** signal (see
> [troubleshooting](#troubleshooting-got-non-2xx-response-for-code-exchange-400)),
> not a reason to disable PKCE.

## Operations

### Grant a user access

Access grants follow ADR-15 — see
[How an operator grants access](../adr/ADR-15.md#how-an-operator-grants-access)
for the authoritative procedure. In brief:

- **Platform Admin (Quay superuser):** add the user's `preferred_username` to
  `SUPER_USERS` in
  [holos/components/quay/buildplan.cue](../../holos/components/quay/buildplan.cue)
  and re-render/apply. This is the **only** way to confer Quay superuser — the
  `platform-admin` client role does not.
- **Per-project / team access:** grant the user the project's `quay` client
  role (`project-admin` or a per-project role), or add them to the bound
  Keycloak group; Quay binds the matching team's membership from the `groups`
  claim automatically (`FEATURE_TEAM_SYNCING: true`) on the 30-minute
  `TEAM_RESYNC_STALE_TIME` cadence. A superuser performs the one-time setup of
  the team→group binding in the Quay organization UI.

### Verify "Holos SSO" login and superuser access

This is the manual verification that "Holos SSO login works and a `SUPER_USERS`
member has superuser access." It requires a live cluster, a reachable
`quay.holos.localhost` (local CA + DNS per
[docs/local-cluster.md](../local-cluster.md)), and a realm user to sign in as.

1. **Use a seeded superuser** (or seed your own realm user). The keycloak phase
   seeds `svc-quay-resource-controller` and `quay-admin`; retrieve a password
   per [Retrieve a superuser password](#retrieve-a-superuser-password). To test
   a non-superuser, create another realm user and note its `preferred_username`.

2. **Log in through "Holos SSO".** Open `https://quay.holos.localhost`, click the
   **Holos SSO** button (the `SERVICE_NAME` from `KEYCLOAK_LOGIN_CONFIG`),
   authenticate in Keycloak, and confirm Quay redirects back and logs you in. A
   first login auto-provisions the user's personal namespace
   (`quay.holos.localhost/<preferred_username>/...`); the username is taken
   verbatim from the token with no confirmation prompt. If login fails with
   `Got non-2XX response for code exchange: 400`, see
   [the troubleshooting section](#troubleshooting-got-non-2xx-response-for-code-exchange-400).

3. **Confirm superuser access.** Signed in as `quay-admin` or
   `svc-quay-resource-controller`, the **Super User Admin Panel** appears in the
   Quay UI, and a token issued to that user answers the superuser API (the
   `GET /api/v1/superuser/users/` → 200 check in
   [Verify superuser access](#verify-superuser-access)). Superuser status comes
   **solely** from `SUPER_USERS`, never from the `groups` claim or the
   `platform-admin` client role. To grant another realm user superuser, add its
   `preferred_username` to `SUPER_USERS` in
   [holos/components/quay/buildplan.cue](../../holos/components/quay/buildplan.cue),
   regenerate the deploy tree (`cd holos && holos render platform`), commit the
   `.cue` change with the regenerated `holos/deploy/` tree, then run
   `scripts/render` (it fails on uncommitted `holos/` changes, so commit first)
   and `scripts/apply`.

### Inspect or rotate the `quay-oidc` secret

The secret is generate-once and never auto-rotated. Confirm the two copies match
**without printing the secret** — compare hashes of the base64-encoded values, so
the plaintext secret never lands in terminal scrollback or incident notes:

```bash
kubectl -n keycloak get secret quay-oidc -o jsonpath='{.data.client_secret}' | sha256sum
kubectl -n quay     get secret quay-oidc -o jsonpath='{.data.client_secret}' | sha256sum
```

The two hashes **must be identical**. To rotate (rarely needed — e.g. a
suspected leak), delete the Secret in **both** namespaces and re-run
`scripts/apply` so the bootstrap Job regenerates one fresh value into both;
then restart the Quay Deployment so its initContainer re-renders `config.yaml`
with the new secret:

```bash
kubectl -n keycloak delete secret quay-oidc
kubectl -n quay     delete secret quay-oidc
scripts/apply
kubectl -n quay rollout restart deploy/quay
```

### Force a realm reconcile

Realm/client drift is corrected by re-running the keycloak-config-cli Job —
either by re-applying or by deleting the completed Job so the next apply
recreates it:

```bash
scripts/apply
# or, to force the Job to re-run on the next apply, delete the completed Job by
# label (the same selector scripts/apply uses):
kubectl -n keycloak delete job -l app.kubernetes.io/name=keycloak-config
```

After editing the realm import document
([holos/components/keycloak/realm-config/buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue)),
run `scripts/render` and commit the regenerated `holos/deploy/` tree before
applying — see
[holos/docs/component-guidelines.md](../../holos/docs/component-guidelines.md).

## Troubleshooting: `Got non-2XX response for code exchange: 400`

**Symptom.** A user clicks "Holos SSO", authenticates in Keycloak, is
redirected back to Quay, and login fails. Quay logs (`kubectl -n quay logs
deploy/quay`) show:

```
Got non-2XX response for code exchange: 400
```

This string is raised by Quay (`oauth/base.py`) whenever the Keycloak **token
endpoint** returns a non-2xx status during the authorization-code exchange. A
`400` from Keycloak almost always carries an `error` field in the JSON body
(`invalid_grant`, `invalid_client`, etc.) — read the Keycloak side to get it.

**Where to read the logs.**

```bash
# Quay app (the relying party that reports the 400):
kubectl -n quay logs deploy/quay --tail=100 | grep -i "code exchange"

# Keycloak (the authoritative reason for the 400) — the server is the
# operator-managed StatefulSet backing the Keycloak CR, not a Deployment:
kubectl -n keycloak logs statefulset/keycloak --tail=200 | grep -iE "error|invalid|pkce|code_verifier"
```

**Root-cause checklist** (most-likely first), with the Keycloak `error` you'll
typically see and the remediation:

| # | Root cause | Keycloak signal | Remediation |
|---|-----------|-----------------|-------------|
| 1 | **PKCE mismatch — only one end has it** — the `quay` client's `pkce.code.challenge.method` was dropped while Quay still sends a challenge, or Quay's `USE_PKCE` was removed while the client still requires PKCE | `invalid_grant` / `code_verifier_missing` | Confirm **both** ends use PKCE `S256`: `pkce.code.challenge.method: "S256"` on the `quay` client **and** `USE_PKCE: true` / `PKCE_METHOD: "S256"` in `KEYCLOAK_LOGIN_CONFIG`. See [PKCE on the `quay` client](#pkce-on-the-quay-client). Re-set on both ends, re-render, re-apply. |
| 2 | **Client secret mismatch / empty** — the `quay-oidc` Secret differs between namespaces, or wasn't substituted | `invalid_client` | Compare the secret in both namespaces (see [Inspect or rotate](#inspect-or-rotate-the-quay-oidc-secret)). If they differ, rotate so both match; then restart the Quay Deployment. |
| 3 | **Public/confidential mismatch** — the `quay` client flipped to `publicClient: true`, or Quay stopped sending the secret | `invalid_client` | Confirm `publicClient: false` on the Keycloak client and `CLIENT_SECRET` present in Quay's config. Re-render/apply. |
| 4 | **Redirect-URI mismatch** — the callback URL isn't covered by `redirectUris` | `invalid_grant` (redirect_uri) | Confirm the Keycloak client's `redirectUris` wildcard `https://quay.holos.localhost/*` covers `https://quay.holos.localhost/oauth2/keycloak/callback`. See [Redirect-URI convention](#redirect-uri-convention). |
| 5 | **Issuer / trailing-slash mismatch** — `OIDC_SERVER` doesn't end in `/`, or points at the wrong issuer | issuer mismatch / discovery failure | Confirm `OIDC_SERVER` ends in a single trailing `/` and equals Keycloak's advertised `issuer`. See [Issuer trailing-slash requirement](#issuer-trailing-slash-requirement). |
| 6 | **Code reuse / expiry / clock skew** — the authorization code was already exchanged, expired, or node clocks differ | `invalid_grant` | Retry the login (a stale/replayed code is single-use). If persistent, check node time sync (NTP) between the Quay and Keycloak nodes. |

**General remediation flow.** Read the Keycloak `error` first (it names the
cause), match it to the table above, apply the fix in the relevant
`buildplan.cue`, run `scripts/render`, commit the regenerated `holos/deploy/`
tree, `scripts/apply`, then re-test the login. After a secret change, restart
the Quay Deployment so its initContainer re-renders `config.yaml`.

## See also

- [ADR-15 — Quay↔Keycloak OIDC SSO](../adr/ADR-15.md) — the decision record.
- [Quay Resource Controller credentials runbook](quay-resource-controller-credentials.md)
  — the manual procedure for minting the future controller's OAuth-Application
  credential while data-plane provisioning is deferred.
- [holos/docs/keycloak-clients.md](../../holos/docs/keycloak-clients.md) — the
  declarative client pattern and PKCE guardrail checklist.
- [holos/components/quay/buildplan.cue](../../holos/components/quay/buildplan.cue)
  — Quay's `KEYCLOAK_LOGIN_CONFIG` and `SUPER_USERS`.
- [holos/components/keycloak/realm-config/buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue)
  — the Keycloak `quay` client, the two superuser realm users, and the secret
  bootstrap Job.
- [AGENTS.md](../../AGENTS.md) — the Quay OIDC-auth guardrail and the `svc-`
  service-account naming convention.
