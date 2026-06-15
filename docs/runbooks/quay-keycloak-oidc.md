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

Quay (`quay.holos.localhost`) is a Single Sign-On **relying party** of the
Keycloak `holos` realm. Users sign in through Keycloak using the OAuth 2.0
Authorization Code flow; Quay authenticates to the token endpoint as a
**confidential client** with a client secret. Usernames come from the ID
token's `preferred_username` claim, and Keycloak groups, client roles, and the
`platform-owner` realm role all flow through a single `groups` claim into Quay
team syncing. Quay superuser status is **not** claim-driven — it comes solely
from the static `SUPER_USERS` config list.

The integration has three moving parts, all reconciled declaratively on every
`scripts/apply`:

- The Keycloak `quay` client, its roles, and its protocol mappers — reconciled
  by the `keycloak-config` keycloak-config-cli Job.
- The shared `quay-oidc` client secret — bootstrapped once into both the
  `keycloak` and `quay` namespaces, never committed.
- Quay's `KEYCLOAK_LOGIN_CONFIG` — rendered into Quay's `config.yaml` with the
  secret substituted at runtime.

For the full behavioral contract (username derivation, namespace scoping, the
role/group → team mapping, the 30-minute team re-sync cadence), read
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
the HOL-1233 note in [CLAUDE.md](../../CLAUDE.md). **Do not re-enable PKCE on
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
  Keycloak group; then a Quay **superuser** binds the matching Quay team to that
  group/role name in the organization UI. Membership lands on the next team
  re-sync (the `TEAM_RESYNC_STALE_TIME` cadence — 30 minutes).

### Inspect or rotate the `quay-oidc` secret

The secret is generate-once and never auto-rotated. Inspect the two copies and
confirm they match:

```bash
kubectl -n keycloak get secret quay-oidc -o jsonpath='{.data.client_secret}' | base64 -d
kubectl -n quay     get secret quay-oidc -o jsonpath='{.data.client_secret}' | base64 -d
```

The two values **must be identical**. To rotate (rarely needed — e.g. a
suspected leak), delete the Secret in **both** namespaces and re-run
`scripts/apply` so the bootstrap Job regenerates one fresh value into both;
then restart the Quay Deployment so its initContainer re-renders `config.yaml`
with the new secret:

```bash
kubectl -n keycloak delete secret quay-oidc
kubectl -n quay     delete secret quay-oidc
scripts/apply
kubectl -n quay rollout restart deploy/quay-app
```

### Force a realm reconcile

Realm/client drift is corrected by re-running the keycloak-config-cli Job —
either by re-applying or by deleting the completed Job so the next apply
recreates it:

```bash
scripts/apply
# or, to force the Job to re-run on the next apply:
kubectl -n keycloak delete job -l app.holos.run/component.name=keycloak-config
```

After editing the realm import document
([holos/components/keycloak/realm-config/buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue)),
run `scripts/render` and commit the regenerated `holos/deploy/` tree before
applying — see
[holos/docs/component-guidelines.md](../../holos/docs/component-guidelines.md).

## Troubleshooting: `Got non-2XX response for code exchange: 400`

**Symptom.** A user clicks "Holos SSO", authenticates in Keycloak, is
redirected back to Quay, and login fails. Quay logs (`kubectl -n quay logs
deploy/quay-app`) show:

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
kubectl -n quay logs deploy/quay-app --tail=100 | grep -i "code exchange"

# Keycloak (the authoritative reason for the 400):
kubectl -n keycloak logs deploy/keycloak --tail=200 | grep -iE "error|invalid|pkce|code_verifier"
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
- [CLAUDE.md](../../CLAUDE.md) — the HOL-1233 PKCE workaround note.
