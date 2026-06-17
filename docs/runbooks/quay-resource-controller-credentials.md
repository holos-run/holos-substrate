# Runbook: Quay Resource Controller credentials

Operator-facing procedure for minting the OAuth-Application credential the
**future Quay Resource Controller** will authenticate with, now that Quay runs
`AUTHENTICATION_TYPE: OIDC` (the Keycloak `holos` realm is the sole identity
store) and the old headless `quay-initial-admin` superuser-token bootstrap is
gone ([ADR-15](../adr/ADR-15.md) Revision 4, HOL-1293).

Under the OIDC backend Quay has **no local `admin` user** and the
`/api/v1/user/initialize` + `/api/v1/superuser/*` bootstrap endpoints are
unavailable, so a token can no longer be minted headlessly. Until a Quay
Resource Controller exists to reconcile this in-cluster, the data-plane
credential is created **once, by hand**, by an operator following the steps
below. This runbook is the authoritative procedure and records the answers to
the credential's open design questions (which organization, which scopes, can
it create new organizations).

The binding decision record is
[ADR-15 — Quay↔Keycloak OIDC SSO](../adr/ADR-15.md); the SSO wiring and
day-to-day operations are in the
[Quay↔Keycloak OIDC runbook](quay-keycloak-oidc.md).

## Identity: the `svc-quay-resource-controller` service account

The credential is minted while signed in as the **`svc-quay-resource-controller`**
realm user. The `svc-` prefix marks it as a **service account** — a non-human
machine identity — distinct from the human **`quay-admin`** administrator (no
prefix). Both are seeded in the Keycloak `holos` realm by the keycloak phase
(HOL-1294), both hold the `platform-owner` realm role, and both are listed in
Quay's `SUPER_USERS` (`holos/components/quay/buildplan.cue`), matched by
`preferred_username == username`. The repo-wide statement of the `svc-` naming
convention lives in [`AGENTS.md`](../../AGENTS.md).

A token Quay generates for an OAuth Application **acts as the user who generated
it**, so generating it while signed in as `svc-quay-resource-controller` is what
makes the resulting credential the controller's machine identity — not a human's.

## Retrieve the generated passwords (replaces `quay-initial-admin`)

Both realm users' passwords are generated **once at runtime** by the
`quay-user-password-bootstrap` Job (HOL-1294) and stored as Kubernetes Secrets
in the **`keycloak`** namespace — one Secret per user, named for the user, each
carrying a single **`password`** key. Nothing secret is committed to the
repository (the generate-once bootstrap pattern, mirroring
`keycloak-initial-admin`). These two Secrets replace the removed
`quay-initial-admin` Secret as the documented Quay credential source.

| User | Kind | Secret (namespace `keycloak`) | Key |
|------|------|-------------------------------|-----|
| `svc-quay-resource-controller` | service account (`svc-` prefix) | `svc-quay-resource-controller` | `password` |
| `quay-admin` | human administrator | `quay-admin` | `password` |

Retrieve a password with `kubectl ... -o jsonpath` and base64-decode it:

```bash
# The service account that mints the controller credential:
kubectl -n keycloak get secret svc-quay-resource-controller \
  -o jsonpath='{.data.password}' | base64 -d; echo

# The human administrator:
kubectl -n keycloak get secret quay-admin \
  -o jsonpath='{.data.password}' | base64 -d; echo
```

The username for each is the Secret name itself (`svc-quay-resource-controller`
/ `quay-admin`); the realm-user `username`, the SSO login name, and the
`SUPER_USERS` entry are all the same string. Sign in to Quay through **"Holos
SSO"** with the username and the retrieved password.

## Procedure: mint the OAuth-Application credential

Perform these steps once, by hand. They require a reachable
`quay.holos.localhost` (local CA + DNS per
[docs/local-cluster.md](../local-cluster.md)) and the
`svc-quay-resource-controller` password retrieved above.

### 1. Sign in as the service account via "Holos SSO"

Open `https://quay.holos.localhost/` and click **Sign in with Holos SSO** (the
`SERVICE_NAME` from Quay's `KEYCLOAK_LOGIN_CONFIG`). Authenticate in Keycloak as
`svc-quay-resource-controller` with the password from its Secret. The local
username/password form is disabled (`FEATURE_DIRECT_LOGIN: false`), so SSO is the
only login path. First login auto-provisions the user's personal namespace at
`quay.holos.localhost/svc-quay-resource-controller/...`.

### 2. Create (or choose) the organization that owns the Application

**Recommendation: create a dedicated organization, `holos-controller`, owned by
`svc-quay-resource-controller`, and create the OAuth Application under it.**

Why a dedicated org rather than the service account's personal namespace or a
shared `holos` org:

- A Quay OAuth Application **must** be created under an **Organization**
  (Applications tab) — a user's personal namespace has no Applications tab, so
  the personal namespace alone cannot host the Application.
- Keeping the Application in an org the service account owns isolates the
  controller's credential lifecycle from any human-facing org, and makes the
  token's blast radius (and rotation) easy to reason about: it is the only
  Application in `holos-controller`.
- As a superuser, `svc-quay-resource-controller` can create the org from the
  Quay UI (**+ → New Organization**, name `holos-controller`).

If a platform org already exists and you prefer to consolidate, the Application
may instead be created under it — the scope/ownership reasoning below is
unchanged because the **token's abilities derive from the user, not the org**.

### 3. Create the OAuth Application and generate a token

1. Open the `holos-controller` org → **Applications** tab → **Create New
   Application**; name it e.g. `quay-resource-controller`.
2. Open the Application → **Generate Token**.
3. Select the scopes (see the next section) and generate. **Copy the token
   immediately** — Quay shows it once.

The generated token authenticates API calls **as `svc-quay-resource-controller`**
and inherits that user's abilities (including its superuser status) bounded by
the selected scopes.

### 4. Scopes — and whether the token can create organizations

**Recommendation: generate the token with `super:user`, `org:admin`, and
`repo:create`.**

| Scope | Grants | Why the controller needs it |
|-------|--------|-----------------------------|
| `super:user` | the `/api/v1/superuser/*` API (the caller must also be in `SUPER_USERS`) | superuser-level provisioning across orgs; the broadest data-plane reach |
| `org:admin` | administer organizations the user can administer (teams, robots, members, webhooks) | manage org/team/robot/webhook objects the controller provisions |
| `repo:create` | create repositories | auto-create repositories under provisioned orgs |

**Can this token create *additional* organizations?** **Yes.** Org creation is a
**user ability**, not a distinct OAuth scope: any authenticated Quay user who is
allowed to create organizations can `POST /api/v1/organization/`, and
`svc-quay-resource-controller` — a superuser — is allowed. The token carries that
ability as long as it can authenticate as the user; `super:user`/`org:admin`
then cover administering the orgs it creates. There is no separate
`org:create` scope to add.

### 5. Verify org-creation with an API smoke test

Confirm the token can create an organization end to end. Replace `$TOKEN` with
the generated token:

```bash
TOKEN='<the generated token>'

# Create a throwaway org — expect HTTP 201:
curl -sS -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer ${TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{"name":"smoke-test-org","email":"svc-quay-resource-controller+smoke@holos.localhost"}' \
  https://quay.holos.localhost/api/v1/organization/
# => 201

# Confirm the superuser API answers (super:user scope) — expect HTTP 200:
curl -sS -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer ${TOKEN}" \
  https://quay.holos.localhost/api/v1/superuser/users/
# => 200

# Clean up the throwaway org:
curl -sS -o /dev/null -w '%{http_code}\n' -X DELETE \
  -H "Authorization: Bearer ${TOKEN}" \
  https://quay.holos.localhost/api/v1/organization/smoke-test-org
# => 204
```

A `201` from the create call confirms the token can create additional
organizations; a `200` from the superuser endpoint confirms the `super:user`
scope is effective. A `403` on the create call means the token lacks the
user's org-creation ability (the user is not a superuser / not allowed to
create orgs) or `super:user` was not selected; a `401` means the token is
invalid or was not copied correctly.

### 6. Store the token as a Kubernetes Secret for the future controller

Store the verified token in the `quay` namespace under a stable name the future
Quay Resource Controller will read. Create it imperatively (the value is never
committed — the runtime-secret guardrail in
[`AGENTS.md`](../../AGENTS.md) and
[`holos/docs/secret-handling.md`](../../holos/docs/secret-handling.md)):

```bash
kubectl -n quay create secret generic quay-resource-controller \
  --from-literal=token="${TOKEN}"
```

| Field | Value |
|-------|-------|
| Namespace | `quay` |
| Secret name | `quay-resource-controller` |
| Key | `token` |
| Value | the OAuth-Application token from step 3 |

The token is non-expiring (a Quay OAuth-Application token), so this is a
generate-once credential: if it leaks, delete the Application's token in the
Quay UI, regenerate (steps 3–5), and replace the Secret. When the Quay Resource
Controller ships it will reconcile this credential in-cluster and this manual
procedure is retired.

## See also

- [ADR-15 — Quay↔Keycloak OIDC SSO](../adr/ADR-15.md) — the decision record
  (Revision 4: OIDC backend, two Keycloak-backed superusers, data-plane
  provisioning deferred to a future Quay Resource Controller).
- [Quay↔Keycloak OIDC runbook](quay-keycloak-oidc.md) — the SSO wiring, the two
  superuser realm users, secret rotation, and `code exchange: 400`
  troubleshooting.
- [docs/local-cluster.md](../local-cluster.md) — bringing the local cluster up
  and the Quay verification steps (SSO login as `quay-admin` /
  `svc-quay-resource-controller`).
- [`AGENTS.md`](../../AGENTS.md) — the `svc-` service-account naming convention
  and the runtime-secret guardrail.
