# Runbook: Quay Resource Controller credentials

Operator-facing procedure for minting the OAuth-Application credential the
**future Quay Resource Controller** will authenticate with, now that Quay runs
`AUTHENTICATION_TYPE: OIDC` (the Keycloak `holos` realm is the sole identity
store) and the old headless `quay-initial-admin` superuser-token bootstrap is
gone ([ADR-15](../adr/ADR-15.md) Revision 4, HOL-1293).

Under the OIDC backend Quay has **no local `admin` user** and the headless
`/api/v1/user/initialize` one-shot bootstrap endpoint is unavailable, so a token
can no longer be minted **headlessly**. (The `/api/v1/superuser/*` APIs still
answer an authenticated `SUPER_USERS` member's OAuth token — that is exactly the
credential this runbook produces, by signing a superuser in interactively and
generating an OAuth-Application token.) Until a Quay Resource Controller exists
to reconcile this in-cluster, the data-plane credential is created **once, by
hand**, by an operator following the steps below. This runbook is the authoritative procedure and records the answers to
the credential's open design questions (which organization, which scopes, can
it create new organizations).

> **This manual procedure is the interim step.** The "future Quay Resource
> Controller" referenced here has a **proposed** design (`Status: Proposed`, not
> yet implemented): the **Holos Controller** ([ADR-18](../adr/ADR-18.md)), whose
> `quay.holos.run` Organization and Repository CRDs ([ADR-19](../adr/ADR-19.md))
> will reconcile the **org/repo/robot/webhook** provisioning in-cluster, retiring
> the data-plane parts of the hand procedure below. The proposed Holos Project
> and Application components ([ADR-21](../adr/ADR-21.md)) are what would emit those
> CRDs per project/app. Note the credential this runbook produces is **not** one
> of those CRDs: ADR-19 has the controller *read* this OAuth-Application token
> from the `quay`-namespace Secret (it never commits it), so this bootstrap
> credential is the controller's input, not something the CRDs reconcile away.
> Until the controller ships, this runbook remains operative.

The binding decision record is
[ADR-15 — Quay↔Keycloak OIDC SSO](../adr/ADR-15.md); the controller and CRDs that
will automate this provisioning are designed in
[ADR-18 — The Holos Controller](../adr/ADR-18.md) and
[ADR-19 — Quay Organization/Repository CRDs](../adr/ADR-19.md). The SSO wiring and
day-to-day operations are in the
[Quay↔Keycloak OIDC runbook](quay-keycloak-oidc.md).

## Identity: the `svc-quay-resource-controller` service account

The credential is minted while signed in as the **`svc-quay-resource-controller`**
realm user. The `svc-` prefix marks it as a **service account** — a non-human
machine identity — distinct from the human **`quay-admin`** administrator (no
prefix). Both are seeded in the Keycloak `holos` realm by the keycloak phase
(HOL-1294), both hold the `platform-owner` realm role, and both are listed in
Quay's `SUPER_USERS` (`holos/components/quay/buildplan.cue`), matched by
`preferred_username == username`. The authoritative repo-wide statement of the
`svc-` naming convention is in [`AGENTS.md`](../../AGENTS.md) (Conventions).

A token Quay generates for an OAuth Application **acts as the user who generated
it**, so generating it while signed in as `svc-quay-resource-controller` is what
makes the resulting credential the controller's machine identity — not a human's.

## Users vs. organizations: where the credential lives, and what it can touch

Three Quay concepts are easy to conflate; keeping them distinct removes the
confusion around "whose token is this and what can it reach."

- A **user** is an identity that signs in (here, the two Keycloak realm users
  `svc-quay-resource-controller` and `quay-admin`). A user also owns a **personal
  namespace** named for the username.
- An **organization** is a shared namespace that owns repositories, teams,
  robots, and webhooks. Organizations — **not users** — are the only place an
  **OAuth Application** can be created: the Applications tab exists on an org's
  settings, never on a user's personal namespace. **This is why an OAuth
  Application (and therefore the controller's credential) cannot be created
  directly "for" `svc-quay-resource-controller` as a user** — it must be created
  inside an org the user can administer.
- An **OAuth Application token** is the credential. It is **not** scoped to the
  organization that hosts the Application. The host org is merely *where the
  credential record lives*; it is **not a permission boundary**. The token acts
  as the **user who generated it** (`svc-quay-resource-controller`), bounded by
  that user's rights on each target namespace and by the token's selected
  scopes — never by which org happens to host it.

So the `platform-automation` org created below is just the home for the
credential record. What the resulting token can actually do is governed by Quay
RBAC: an action succeeds only if `svc-quay-resource-controller` holds the proper
role on the target namespace (or is a full-access superuser — see below), even
when the token carries a broad scope like `repo:admin`. That splits into two
cases that matter for the future reconciler:

1. **Orgs the controller creates** (the clean GitOps path; e.g. `my-project`).
   The token calls `POST /api/v1/organization/`, and the creating user —
   `svc-quay-resource-controller` — automatically becomes the org's owner/admin.
   From then on it administers that org's repos, teams, robots, and webhooks
   through the normal endpoints because it **owns the namespace**. No extra
   configuration is required, and this matches declarative reconcile-from-scratch
   exactly.
2. **Orgs the controller did *not* create** (adopting a pre-existing org someone
   else owns). By default a superuser has **no** access to another user's
   organization: the token's `super:user` scope only reaches the
   `/api/v1/superuser/*` panel endpoints, so a `PUT` on a repo, robot, or
   notification inside an org the controller is not a member of returns **403**.
   Bridging this gap is a **config** flag, not an application or scope change:
   **`FEATURE_SUPERUSERS_FULL_ACCESS`** grants `SUPER_USERS` read/write/delete on
   namespaces and orgs they do not own. With it enabled (HOL-1299, see below),
   plus the `super:user` scope on the token and the identity in `SUPER_USERS`,
   the controller can administer **every** org on the instance through the same
   normal endpoints.

### `FEATURE_SUPERUSERS_FULL_ACCESS` is enabled (HOL-1299)

`holos/components/quay/buildplan.cue` sets **`FEATURE_SUPERUSERS_FULL_ACCESS:
true`** so the controller is robust against orgs it did not create itself. A
reconciler that is the system of record must be able to take over and converge
*any* org — including one a human pre-created or another automation made — not
only orgs it happened to create. Without the flag the controller would 403 on
those namespaces and silently fail to reconcile them, which is exactly the
fragility this enables us to avoid.

The flag applies to `SUPER_USERS` members only, but to **all** of their
superuser sessions: Quay grants superuser permission for the `super:user` OAuth
scope **or** the internal `direct_user_login` scope used by authenticated web
sessions. So full access is **not** limited to the controller's OAuth token —
the human `quay-admin`, signed in through "Holos SSO", also gains instance-wide
read/write/delete across every org. This is not configurable per-user (the flag
is instance-wide and covers every `SUPER_USERS` member) and it does not widen
access for ordinary, non-`SUPER_USERS` users; treat `quay-admin`'s UI reach as an
acceptable extension of an existing platform administrator. Confirm the flag is
live on the running instance by checking the rendered config:

```bash
kubectl -n quay get configmap quay-config-template \
  -o jsonpath='{.data.config\.yaml}' | grep FEATURE_SUPERUSERS_FULL_ACCESS
# => FEATURE_SUPERUSERS_FULL_ACCESS: true
```

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

### 2. Create the `platform-automation` organization (step by step)

The credential record must live in an organization the service account owns (a
user's personal namespace has no Applications tab — see *Users vs.
organizations* above). Create a dedicated org named **`platform-automation`**,
owned by `svc-quay-resource-controller`. Keeping the Application in its own org
isolates the controller's credential lifecycle from any human-facing org and
makes the token's blast radius and rotation easy to reason about — it is the
only Application in `platform-automation`. The org name is **not** a permission
boundary (the token acts as the user, not the org); it is simply where the
credential record lives.

Perform these steps in the Quay UI while still signed in as
`svc-quay-resource-controller` from step 1:

1. Click the **+** menu in the top navigation bar → **New Organization**.
2. In **Organization Name**, enter exactly `platform-automation`.
3. Enter an **Organization Email** that is distinct from the service account's
   own email, e.g. `svc-quay-resource-controller+platform-automation@holos.localhost`
   (Quay requires every namespace to have a unique email).
4. Click **Create Organization**. Because you are signed in as
   `svc-quay-resource-controller`, that user becomes the org's owner/admin —
   this is what makes the OAuth Application (and its token) belong to the service
   account rather than a human.

> Org creation is a normal user ability and a superuser is always permitted, so
> no extra configuration is needed for this step. (Org creation is restricted
> only if `FEATURE_SUPERUSERS_ORG_CREATION_ONLY` or `FEATURE_RESTRICTED_USERS`
> is set, and even then a superuser may always create — neither is set here.)

### 3. Create the OAuth Application and generate a token

1. Open the `platform-automation` org → **Applications** tab → **Create New
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
| `super:user` | the `/api/v1/superuser/*` API (the caller must also be in `SUPER_USERS`) | superuser-level provisioning across orgs; the broadest data-plane reach. With `FEATURE_SUPERUSERS_FULL_ACCESS: true` (enabled, see above) this scope also reaches the **normal** org/repo/robot/webhook endpoints inside orgs the controller does not own — adopting orgs created by other identities |
| `org:admin` | administer organizations the user can administer (teams, robots, members, webhooks) | manage org/team/robot/webhook objects the controller provisions |
| `repo:create` | create repositories | auto-create repositories under provisioned orgs |

**Can this token create *additional* organizations?** **Yes.** Org creation is a
**user ability**, not a distinct OAuth scope: any authenticated Quay user who is
allowed to create organizations can `POST /api/v1/organization/`, and
`svc-quay-resource-controller` — a superuser — is allowed. The token carries that
ability as long as it can authenticate as the user; `super:user`/`org:admin`
then cover administering the orgs it creates. There is no separate
`org:create` scope to add.

### 5. Verify org-creation and full-access with an API smoke test

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

To verify **`FEATURE_SUPERUSERS_FULL_ACCESS`** specifically — the ability to
administer an org the controller does *not* own — the throwaway-org test above is
insufficient (the controller owns the org it just created). Instead, have a
*different* user create an org (or pick a human-owned one) and confirm the
controller token can read it through the **normal** (non-`superuser`) endpoint:

```bash
# Against an org svc-quay-resource-controller neither owns nor is a member of:
curl -sS -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer ${TOKEN}" \
  https://quay.holos.localhost/api/v1/organization/<other-owners-org>
# => 200 with FEATURE_SUPERUSERS_FULL_ACCESS: true; 403 without it
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

The token is long-lived (a Quay OAuth-Application token; its lifetime is not
operator-configurable), so treat this as a generate-once credential: if it
leaks, delete the Application's token in the
Quay UI, regenerate (steps 3–5), and replace the Secret. When the Holos
Controller ([ADR-18](../adr/ADR-18.md)) and its `quay.holos.run` CRDs
([ADR-19](../adr/ADR-19.md)) ship, they automate the **org/repo/robot/webhook
provisioning** that the rest of this runbook's surrounding workflow performs by
hand — but **this OAuth-Application token stays a manual bootstrap**: ADR-19 has
the controller *read* it from this `quay`-namespace Secret to authenticate to
Quay, it is not something the CRDs mint or reconcile. So this credential step
remains operator-owned even after the controller ships (until a separate
mechanism automates the token itself); keep this Secret populated.

## See also

- [ADR-15 — Quay↔Keycloak OIDC SSO](../adr/ADR-15.md) — the decision record
  (Revision 4: OIDC backend, two Keycloak-backed superusers, data-plane
  provisioning deferred to a future Quay Resource Controller).
- [ADR-18 — The Holos Controller and the GitOps Rendered-Manifest Delivery
  Model](../adr/ADR-18.md) — the controller that will automate the provisioning
  this runbook performs by hand (the proposed design for the "future Quay
  Resource Controller", `Status: Proposed`).
- [ADR-19 — Quay API Group (`quay.holos.run`): Organization and Repository
  CRDs](../adr/ADR-19.md) — the CRDs the controller reconciles to provision
  orgs/repos/robots/webhooks in-cluster.
- [Quay↔Keycloak OIDC runbook](quay-keycloak-oidc.md) — the SSO wiring, the two
  superuser realm users, secret rotation, and `code exchange: 400`
  troubleshooting.
- [docs/local-cluster.md](../local-cluster.md) — bringing the local cluster up
  and the Quay verification steps (SSO login as `quay-admin` /
  `svc-quay-resource-controller`).
- [`AGENTS.md`](../../AGENTS.md) — the runtime-secret guardrail and the
  authoritative `svc-` service-account naming convention (Conventions).
