# Runbook: Quay↔Keycloak OIDC SSO

Operational runbook for the Quay registry's Single Sign-On integration with the
Keycloak `holos` realm. Written for SREs and on-call operators: how the
integration is wired, why Quay is the platform's one **no-PKCE** OIDC relying
party, the day-to-day operations (granting access, rotating the secret, forcing
a reconcile), and how to triage the
`Got non-2XX response for code exchange: 400` login failure.

The binding **decision record** is
[ADR-15 — Quay↔Keycloak OIDC SSO](../adr/ADR-15.md); this runbook is its
operational companion and does not restate the full rationale. The declarative
Keycloak client pattern and the PKCE guardrail checklist live in
[holos/docs/keycloak-clients.md](../../holos/docs/keycloak-clients.md).

## Overview

Quay (`quay.holos.localhost`) runs `AUTHENTICATION_TYPE: Database` — its own
database is the identity store — with the Keycloak `holos` realm layered on as
a **federated login provider** (ADR-15 Revision 3, HOL-1281). Users sign in
with the "Holos SSO" button through the OAuth 2.0 Authorization Code flow; Quay
authenticates to the token endpoint as a **confidential client** with a client
secret. Database auth (rather than `AUTHENTICATION_TYPE: OIDC`) is deliberate:
it keeps the local `admin` user and the `/api/v1/user/initialize` +
`/api/v1/superuser/*` REST APIs available, so the headless
`quay-admin-bootstrap` Job can mint the `quay-initial-admin` superuser token —
see [The Database backend and the superuser token](#the-database-backend-and-the-superuser-token).

Usernames come from the ID token's `preferred_username` claim, and Keycloak
groups, client roles, and the `platform-owner` realm role are folded into a
single `groups` claim Quay receives on each login. Automatic group→team
synchronization is **off** (`FEATURE_TEAM_SYNCING: false`): the Database user
handler cannot sync OIDC groups, so a superuser manages Quay teams directly.
Quay superuser status is **not** claim-driven — it comes solely from the static
`SUPER_USERS` config list.

The integration has three moving parts, all reconciled declaratively on every
`scripts/apply`:

- The Keycloak `quay` client, its roles, and its protocol mappers — reconciled
  by the `keycloak-config` keycloak-config-cli Job.
- The shared `quay-oidc` client secret — bootstrapped once into both the
  `keycloak` and `quay` namespaces, never committed.
- Quay's `KEYCLOAK_LOGIN_CONFIG` — rendered into Quay's `config.yaml` with the
  secret substituted at runtime.

For the full behavioral contract (the Database backend + federated SSO model,
username derivation, namespace scoping, the role/group model, and why team
syncing is disabled), read [ADR-15](../adr/ADR-15.md).

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
- **No `attributes."pkce.code.challenge.method"`** — Keycloak treats a client
  that sets this attribute as *requiring* PKCE. Omitting it keeps PKCE optional
  so the confidential client-secret flow succeeds. See
  [The no-PKCE exception](#the-no-pkce-exception).
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
(`CONFIG_YAML`). The operationally relevant keys:

| Key | Value | Notes |
|-----|-------|-------|
| `OIDC_SERVER` | the realm issuer URL | **Required trailing slash** — see below |
| `CLIENT_ID` | the `quay` client ID | matches the Keycloak `clientId` |
| `CLIENT_SECRET` | `__OIDC_CLIENT_SECRET__` | substituted from `quay-oidc` at runtime |
| `SERVICE_NAME` | `Holos SSO` | the label on the Quay login button |
| `LOGIN_SCOPES` | `openid`, `profile`, `email`, `groups`, `offline_access` | |
| `PREFERRED_USERNAME_CLAIM_NAME` | `preferred_username` | username, verbatim |
| `PREFERRED_GROUP_CLAIM_NAME` | `groups` | drives team syncing |

There is deliberately **no** `USE_PKCE` / `PKCE_METHOD` here (HOL-1257). Quay
defaults `USE_PKCE` to `false`, so it sends no `code_challenge`.

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

## The Database backend and the superuser token

Quay runs `AUTHENTICATION_TYPE: Database` (in
[holos/components/quay/buildplan.cue](../../holos/components/quay/buildplan.cue)'s
`CONFIG_YAML`), **not** `AUTHENTICATION_TYPE: OIDC`. The Keycloak
`KEYCLOAK_LOGIN_CONFIG` block layers "Holos SSO" on top as a federated login
provider — Quay treats any `<PREFIX>_LOGIN_CONFIG` block as an SSO provider
regardless of the backend — so SSO login and the local identity store coexist.

This matters operationally because `AUTHENTICATION_TYPE: OIDC` would make the
OIDC provider the **sole** identity store and disable the local `admin` user
along with the `/api/v1/user/initialize` and `/api/v1/superuser/*` REST APIs.
Those endpoints are exactly what the headless `quay-admin-bootstrap` Job
(HOL-1276) calls to mint the **`quay-initial-admin`** superuser OAuth token (the
Secret's `token` key, in the `quay` namespace) — the credential every
declarative automation Job (org/repo/robot/webhook bootstrap) authenticates
with. Database auth keeps those APIs available, so the headless bootstrap and
human SSO login work together.

`FEATURE_TEAM_SYNCING` is **false** under this backend. OIDC `groups`-claim →
Quay-team synchronization needs a federated user handler with a
`sync_user_groups` method; the Database handler (`DatabaseUsers`) has none, so
Quay would `AttributeError` → HTTP 500 on every "Holos SSO" callback if team
syncing were left on (Quay v3.17.3 `oauth/login_utils.py` `sync_oidc_groups`).
The `groups` claim is still emitted, so re-enabling team syncing on a future
federated backend needs no realm change — but for now a superuser binds Quay
teams directly.

### Verify the superuser token

The `quay-admin-bootstrap` Job mints the token during `scripts/apply`
(`wait_quay` gates on the Job and the Secret). Confirm the minted token reaches
the superuser API — expect **HTTP 200**:

```bash
TOKEN="$(kubectl -n quay get secret quay-initial-admin \
  -o jsonpath='{.data.token}' | base64 -d)"
curl -sS -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer ${TOKEN}" \
  https://quay.holos.localhost/api/v1/superuser/users/
# => 200
```

A `401`/`403` means the token is missing superuser scope (the registry was not
reset/re-initialized cleanly — see the reset procedure below); a connection
error means Quay is not serving or the local CA/DNS is not trusted (see
[docs/local-cluster.md](../local-cluster.md)).

The bootstrap is **idempotent**: with `quay-initial-admin` already present the
Job no-ops (it logs that the Secret already exists and leaves it untouched), so
re-running `scripts/apply` does not error or rotate the token.

### One-time reset: re-opening initialize

`/api/v1/user/initialize` only answers against a **virgin** registry — the
moment any user exists in Quay's database it closes permanently. A cluster that
previously ran under the broken OIDC-only config may already hold
SSO-provisioned users, which keeps initialize closed and prevents
`quay-admin-bootstrap` from ever writing the `quay-initial-admin` Secret — even
after the backend is switched to Database. When the superuser-token check above
cannot reach 200 because the token was never minted, run the deliberate,
**destructive** one-time reset (HOL-1283):

```bash
scripts/quay-reset
```

`scripts/quay-reset` is a separate operation — `scripts/apply` never wipes the
database on its own. It targets the local `k3d-holos` cluster (override with
`KUBE_CONTEXT`), prints a destructive-data warning with a 5-second abort window,
then: scales the Quay Deployment to zero, deletes the CNPG `quay-db` Cluster and
its PVCs (so the next apply re-runs `initdb` against empty storage — a virgin
database), deletes the registry blob-storage PVC (`quay-datastorage`), and
finally deletes the stale `quay-initial-admin` Secret so the bootstrap re-mints
the token rather than skipping on its reuse guard. **It destroys all Quay
registry data** — every organization, repository, robot, image, and user. Every
step is idempotent and safe to re-run.

Bring Quay back up and re-mint the token, then re-verify with the superuser-API
check above:

```bash
scripts/apply
```

See the script's `--help` (`scripts/quay-reset --help`) for the full step
rationale and the inline verification commands.

## The no-PKCE exception

The platform standard is **PKCE for OIDC clients**: the public `argocd` and
`kargo` clients are both public PKCE (`S256`) clients. **Quay is the documented
exception** — the one relying party that does **not** use PKCE.

Why: Quay's confidential client-secret token exchange did not reliably
round-trip a matching PKCE `code_verifier` at the token endpoint. With PKCE
required on the Keycloak `quay` client, Quay sent a `code_challenge` but the
missing/mismatched `code_verifier` produced
`Got non-2XX response for code exchange: 400`, blocking SSO login entirely.
Red Hat's recommended baseline Quay↔Keycloak integration is a plain
**confidential client authenticated by a client secret** with PKCE optional, so
PKCE was removed from **both** ends together (HOL-1257) — the only
mutually-consistent state:

- Quay (`holos/components/quay/buildplan.cue`): no `USE_PKCE`/`PKCE_METHOD`.
- The Keycloak `quay` client
  (`holos/components/keycloak/realm-config/buildplan.cue`): no
  `attributes."pkce.code.challenge.method"`.

This exception is recorded as a guardrail-checklist item in
[holos/docs/keycloak-clients.md](../../holos/docs/keycloak-clients.md) and as
the HOL-1233 note in [AGENTS.md](../../AGENTS.md). **Do not re-enable PKCE on
the `quay` client** without a demonstrated Quay-side fix; doing so reintroduces
the `code exchange: 400` failure. The `argocd` and `kargo` clients keep their
PKCE attribute — only `quay` omits it.

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
  Keycloak group; then a Quay **superuser** manages the matching Quay team's
  membership directly in the organization UI. Automatic group/role-name → team
  binding is **not** active while `FEATURE_TEAM_SYNCING: false` (ADR-15 Revision
  3); the `groups` claim is still emitted, so it returns automatically once team
  syncing can be re-enabled on a federated backend.

### Verify "Holos SSO" login and superuser access

This is the manual verification of the HOL-1281 acceptance criteria — that
"Holos SSO login works and a `SUPER_USERS` member has superuser access." It
requires a live cluster, a reachable `quay.holos.localhost` (local CA + DNS per
[docs/local-cluster.md](../local-cluster.md)), and a realm user to sign in as.

1. **Seed a realm user** (if the realm has none yet). The `holos` realm seeds no
   users by default, so there is no `preferred_username` to grant SSO access to
   until one exists. Create a user in the realm (via the declarative realm
   config or the Keycloak admin console) and note its `preferred_username`.

2. **Log in through "Holos SSO".** Open `https://quay.holos.localhost`, click the
   **Holos SSO** button (the `SERVICE_NAME` from `KEYCLOAK_LOGIN_CONFIG`),
   authenticate in Keycloak, and confirm Quay redirects back and logs you in. A
   first login auto-provisions the user's personal namespace
   (`quay.holos.localhost/<preferred_username>/...`); the username is taken
   verbatim from the token with no confirmation prompt. If login fails with
   `Got non-2XX response for code exchange: 400`, see
   [the troubleshooting section](#troubleshooting-got-non-2xx-response-for-code-exchange-400).

3. **Promote the user to superuser** (optional, to verify the `SUPER_USERS`
   path). Add the user's `preferred_username` to `SUPER_USERS` in
   [holos/components/quay/buildplan.cue](../../holos/components/quay/buildplan.cue),
   run `scripts/render`, commit the regenerated `holos/deploy/` tree, and
   `scripts/apply`. After Quay restarts, the user has superuser access: the
   Super User Admin Panel appears in the Quay UI, and their token answers the
   superuser API (the same `GET /api/v1/superuser/users/` → 200 check as
   [Verify the superuser token](#verify-the-superuser-token), using a token
   issued to that user). Superuser status comes **solely** from `SUPER_USERS`,
   never from the `groups` claim or the `platform-admin` client role.

The bootstrap local `admin` (in `SUPER_USERS` by default) is the break-glass
superuser and the automation credential; promoting a realm user is how a
human platform admin gains superuser access through SSO.

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
| 1 | **PKCE required but no verifier** — the `quay` client has `pkce.code.challenge.method` set again, or Quay's `USE_PKCE` was re-added on only one end | `invalid_grant` / `code_verifier_missing` | Confirm **neither** end uses PKCE: no `pkce.code.challenge.method` on the `quay` client, no `USE_PKCE`/`PKCE_METHOD` in `KEYCLOAK_LOGIN_CONFIG`. See [The no-PKCE exception](#the-no-pkce-exception). Remove PKCE from both ends, re-render, re-apply. |
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
- [holos/docs/keycloak-clients.md](../../holos/docs/keycloak-clients.md) — the
  declarative client pattern and PKCE guardrail checklist.
- [holos/components/quay/buildplan.cue](../../holos/components/quay/buildplan.cue)
  — Quay's `KEYCLOAK_LOGIN_CONFIG`.
- [holos/components/keycloak/realm-config/buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue)
  — the Keycloak `quay` client and the secret bootstrap Job.
- [scripts/quay-reset](../../scripts/quay-reset) — the one-time, destructive DB
  reset that re-opens `/api/v1/user/initialize` (HOL-1283).
- [AGENTS.md](../../AGENTS.md) — the Quay Database-auth and no-PKCE guardrail
  notes.
