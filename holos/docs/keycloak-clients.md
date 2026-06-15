# Keycloak OIDC Clients: the Declarative Pattern and PKCE Guardrails

How the platform declaratively manages Keycloak OIDC clients, how the
Keycloak↔Quay (and Argo CD) integration works, and the guardrails for adding
another PKCE client. Written for SRE and Platform Engineers maintaining the
integration, and for agents reusing the pattern without rediscovering it.

These are operational guidelines, not decisions. The decision record for the
Quay SSO integration is [ADR-15](../../docs/adr/ADR-15.md) — link to it, do not
duplicate it. The authoritative source for every client, role, and mapper
discussed here is
[`components/keycloak/realm-config/buildplan.cue`](../components/keycloak/realm-config/buildplan.cue),
which also pins the keycloak-config-cli image (`6.5.1-26.5.5`). The Keycloak
version pin (`KeycloakVersion`) lives in
[`components/keycloak/keycloak.cue`](../components/keycloak/keycloak.cue) and
Quay's in [`components/quay/buildplan.cue`](../components/quay/buildplan.cue) —
this doc quotes versions where useful but those CUE files are the source of
truth.

## The reconciliation mechanism

The `holos` realm — its roles, the `authenticated` default group, and the OIDC
clients with their protocol mappers — is reconciled declaratively on **every**
`scripts/apply` by the
[`keycloak-config`](../components/keycloak/realm-config/buildplan.cue) component:
an idempotent [keycloak-config-cli](https://github.com/adorsys/keycloak-config-cli)
`Job` (image pinned to `6.5.1-26.5.5` for the Keycloak `26.6.3` line) running in
the `keycloak` namespace. The import document is authored in CUE, marshalled to
JSON in a `ConfigMap` the Job mounts at `/config/holos.json`.

Why a Job and not the `KeycloakRealmImport` CR: the operator's realm import is
**bootstrap-only** — its import Job skips when the realm already exists, so
post-bootstrap changes (new clients, roles, groups) never reconcile.
keycloak-config-cli runs against the live admin API and converges the realm on
every run. The two paths never fight: the import document carries
`realm: "holos"` only — no `enabled` or `identity-provider` fields, which the
`KeycloakRealmImport` CR owns. keycloak-config-cli's default managed-import
behavior is no-delete, so realm objects the Job does not declare are left
untouched (full-realm purge is deliberately not enabled).

So **realm changes land by editing the import document and re-applying**, never
by manual admin-console edits.

### The apply-gate

A completed `Job`'s pod template is immutable and `kubectl apply` never re-runs
an existing Complete `Job`. Two mechanisms make reconciliation happen anyway:

- The Job's `metadata.name` carries an 8-char content hash of the import
  document and image (`keycloak-config-<hash>`), so any import or image change
  renders a distinct Job and the deploy filename changes visibly in review.
- `scripts/apply`'s `pre_keycloak_config` hook deletes every `keycloak-config`
  Job (by the `app.kubernetes.io/name=keycloak-config` label) before the apply,
  so the apply always creates a fresh Job that re-runs the CLI — covering
  forward edits **and** reverts to a previously-applied config within the Job's
  TTL window.

The `wait_keycloak_config` gate then polls that Job to completion, resolving the
hashed name from the rendered manifest so it waits on exactly the Job just
applied. It sits between `keycloak` and `quay` in the apply order. See
[`holos/README.md`](../README.md#keycloak-config-realm-reconciliation) for the
operator-facing overview of this gate.

## How an OIDC client is declared

A client is an entry in the `clients` list of the `REALM_CONFIG` value in
[`realm-config/buildplan.cue`](../components/keycloak/realm-config/buildplan.cue).
Each entry sets the standard Keycloak client fields and a `protocolMappers`
list. The platform runs two clients today — `argocd` and `quay`. They differ in
the usual per-client details (redirect URIs, web origins, roles, and which
protocol mappers they carry), but the key template distinction is
**public vs confidential**, which also decides whether a client-secret bootstrap
is needed.

### Public PKCE client (argocd)

Argo CD's UI and CLI are public OAuth clients that cannot hold a secret, so the
`argocd` client is `publicClient: true` and uses the Authorization Code flow
with PKCE (`S256`). It holds **no** `secret`:

```cue
clientId:                  ARGOCD_CLIENT_ID
publicClient:              true
standardFlowEnabled:       true
serviceAccountsEnabled:    false
directAccessGrantsEnabled: false
attributes: "pkce.code.challenge.method": "S256"
```

### Confidential client, no PKCE (quay)

Quay's `KEYCLOAK_LOGIN_CONFIG` validator requires a `CLIENT_SECRET`, so the
`quay` client is **confidential** (`publicClient: false`) and authenticates with
that secret alone. Unlike `argocd`, it does **not** use PKCE: the client sets no
`pkce.code.challenge.method` attribute (HOL-1257). This is the documented
exception to the platform's PKCE-by-default posture — Quay's confidential
client-secret flow did not reliably round-trip a PKCE `code_verifier`, producing
`code exchange: 400` SSO failures, so PKCE was removed from both ends (Quay's
`KEYCLOAK_LOGIN_CONFIG` no longer sets `USE_PKCE`/`PKCE_METHOD` either). See
[ADR-15](../../docs/adr/ADR-15.md) Revision 2 and the HOL-1233 note in
[`CLAUDE.md`](../../CLAUDE.md). The `secret` field holds a `$(env:...)`
placeholder, never a literal value:

```cue
clientId:                  QUAY_CLIENT_ID
publicClient:              false // confidential: Quay sends a client secret
standardFlowEnabled:       true
serviceAccountsEnabled:    false
directAccessGrantsEnabled: false
secret: "$(env:QUAY_OIDC_CLIENT_SECRET)"
// No pkce.code.challenge.method: Quay authenticates with the client secret
// alone; setting the attribute would make Keycloak *require* PKCE (HOL-1257).
```

### The runtime client-secret bootstrap

The confidential client's secret is the `quay-oidc` Secret (key
`client_secret`), generated **once** by the `quay-oidc-bootstrap` Job
(`QUAY_OIDC_BOOTSTRAP` in the buildplan) and **never committed**. The Job runs
in the `keycloak` namespace and writes the Secret into **both**:

- the `keycloak` namespace — read by the keycloak-config-cli Job via the
  `QUAY_OIDC_CLIENT_SECRET` env var and substituted into the import document's
  `secret: "$(env:QUAY_OIDC_CLIENT_SECRET)"` placeholder; and
- the `quay` namespace — read by the Quay Deployment (HOL-1219) and substituted
  into Quay's `config.yaml`.

It is a generate-once bootstrap: the script creates the Secret only if absent in
a given namespace, never overwrites, and **fails loudly** if the two namespaces'
copies disagree (Keycloak and Quay must authenticate with the same secret). So
the value is stable across re-applies and the two ends always share one secret.
This mirrors the secret-bootstrap pattern described in
[`CLAUDE.md`](../../CLAUDE.md)'s *OIDC Client Secrets* guard rail.

The `secret: "$(env:...)"` substitution only works because the Job sets
`IMPORT_VARSUBSTITUTION_ENABLED: "true"`. keycloak-config-cli defaults this to
`false`, which would import the literal placeholder string as the confidential
client secret. Substitution only touches `$(...)` tokens, so the rest of the
realm JSON is unaffected.

## The `groups` claim and the three mapper types

Every relying party keys on a single shared **`groups`** claim
(`GROUPS_CLAIM = "groups"`). The platform uses three protocol-mapper types, and
**all three write into that same claim** so a relying party can key on group
names, client roles, and realm roles uniformly:

| Mapper | `protocolMapper` | What it folds into `groups` |
|--------|------------------|-----------------------------|
| group-membership | `oidc-group-membership-mapper` | bare group names (e.g. `authenticated`), `full.path: "false"` |
| realm-role | `oidc-usermodel-realm-role-mapper` | realm-role names (e.g. `platform-owner`) |
| client-role | `oidc-usermodel-client-role-mapper` | the client's role names (e.g. the `quay` client's `platform-admin`) |

The realm-role and client-role mappers set `id.token.claim`,
`access.token.claim`, and `userinfo.token.claim` all to `"true"`,
`multivalued: "true"`, and `jsonType.label: "String"` — emitted
**unconditionally**, not gated by an optional client scope.

- The `argocd` client carries the **group-membership** and **realm-role**
  mappers, so Argo CD RBAC keys on the
  `platform-owner`/`platform-editor`/`platform-viewer` realm roles and group
  names through one claim.
- The `quay` client carries **all three** (plus a `preferred_username` property
  mapper), so Quay's team sync keys on group memberships, the `quay`
  client-role names, and — as of HOL-1245 — the `platform-owner` realm role,
  uniformly.

## The role model

### Realm roles

Three platform realm roles are reconciled by the Job: `platform-owner`,
`platform-editor`, `platform-viewer`. The privileged `platform-owner` role
(HOL-1242) is the one surfaced to relying parties through the realm-role mapper.

### Quay client roles

The `quay` client defines two client roles:

- `platform-admin` — the Holos Platform Admin (Quay superuser/org admin intent).
- `project-admin` — per-project administrative access in Quay.

These are **identity labels that flow into the `groups` claim**, not privileges
in themselves. A Quay superuser binds a Quay team to the group/role name; the
team's permissions are what grant access. Per-project roles follow the same
convention: add a `quay` client role named for the project and grant it.

### `platform-owner` into the quay `groups` claim (HOL-1245)

As of HOL-1245 the `quay` client also emits the `platform-owner` realm role into
the `groups` claim, mirroring the `argocd` client. Granting a user the
`platform-owner` realm role surfaces `platform-owner` in their `groups` claim,
so Quay team sync and any future relying party key on it the same way they key
on group names.

### The Quay-superuser limitation (not automatic)

Surfacing `platform-owner` (or `platform-admin`) into the `groups` claim does
**not** make a user a Quay superuser. Quay's `SUPER_USERS` is a **static
username list in `config.yaml`** with no claim-driven superuser sync — there is
no mechanism for Quay to promote a user to superuser from an OIDC claim. So
`role → superuser` is **not** automatic.

The supported path today is the **manual `SUPER_USERS` bootstrap**: add the
user's `preferred_username` to `SUPER_USERS` in
[`components/quay/buildplan.cue`](../components/quay/buildplan.cue) and
re-render/apply. The local `admin` account stays in `SUPER_USERS` as a
break-glass superuser. This keeps the README's
[Quay OIDC SSO and roles](../README.md#quay-oidc-sso-and-roles) statement
consistent: superuser status comes solely from `SUPER_USERS`, never from the
`groups` claim.

## Guardrail checklist: adding a new PKCE client

When adding another OIDC client to the realm, work through this checklist. The
`argocd` (public) and `quay` (confidential) clients are the two templates to
copy from.

1. **Public vs confidential.** Decide first. If the relying party cannot hold a
   secret (an SPA, a CLI, a native app), make it **public**
   (`publicClient: true`, no `secret`) — copy `argocd`. If it requires a client
   secret, make it **confidential** (`publicClient: false`) — copy `quay`, and
   complete steps 4–5.
2. **PKCE `S256`.** Set
   `attributes: "pkce.code.challenge.method": "S256"` on the client. Use PKCE
   wherever the flow supports it, as the platform default — both `argocd` and
   `kargo` (public) carry it. The one documented exception is the confidential
   `quay` client, which omits PKCE because its OIDC client did not reliably
   round-trip a `code_verifier` and failed with `code exchange: 400`
   (HOL-1257); only relax PKCE for a client with a demonstrated implementation
   gap (see step 7).
3. **Redirect URIs and web origins.** Set `redirectUris` to the relying party's
   callback URL(s) (host resolves to `127.0.0.1` per
   [`docs/local-cluster.md`](../../docs/local-cluster.md)) and `webOrigins` to
   its public URL. Keep `serviceAccountsEnabled` and `directAccessGrantsEnabled`
   `false` unless the client genuinely needs those flows.
4. **Secret-bootstrap Job (confidential only).** Do **not** commit a secret. Add
   a generate-once bootstrap Job modeled on `QUAY_OIDC_BOOTSTRAP` that writes the
   secret into the owning namespace and any consuming namespace, creating only if
   absent and failing loudly on a mismatch. Set the client's `secret` to a
   `$(env:...)` placeholder and wire the matching env var into the
   keycloak-config-cli Job from the bootstrapped Secret.
5. **`IMPORT_VARSUBSTITUTION_ENABLED` (confidential only).** The keycloak-config
   Job already sets `IMPORT_VARSUBSTITUTION_ENABLED: "true"`; this is what
   expands the `$(env:...)` placeholder. Confirm it stays enabled — without it,
   the literal placeholder is imported as the secret.
6. **Protocol mappers.** Decide which of the three mappers the relying party
   needs (see [the table above](#the-groups-claim-and-the-three-mapper-types)).
   Write them all into the shared `groups` claim unless the relying party
   requires a different claim name.
7. **No PKCE for clients with an implementation gap.** If the relying party's
   PKCE implementation is incomplete, do not require PKCE for its client. Quay
   hit this — its confidential OIDC client did not reliably round-trip a
   `code_verifier`, producing `code exchange: 400` at the token endpoint. The
   resolution (HOL-1257) was to **drop PKCE entirely** for the `quay` client:
   omit the `pkce.code.challenge.method` attribute (Keycloak treats a client
   that sets it as *requiring* PKCE) and remove `USE_PKCE`/`PKCE_METHOD` from
   Quay's `KEYCLOAK_LOGIN_CONFIG`, leaving Quay to authenticate with its client
   secret alone. The `quay` client in
   [`realm-config/buildplan.cue`](../components/keycloak/realm-config/buildplan.cue)
   therefore carries **no** `pkce.*` attribute. (Historical note: the earlier
   *Quay OIDC PKCE Implementation (HOL-1233)* guard rail in
   [`CLAUDE.md`](../../CLAUDE.md) kept PKCE *optional* via the default
   `pkce.force` behavior; HOL-1257 supersedes that with full removal.) Only
   relax or skip requiring PKCE for a client with a demonstrated implementation
   gap.
8. **Render then commit.** This is a `holos/components/` change, so follow the
   render contract in [`CLAUDE.md`](../../CLAUDE.md) and
   [`component-guidelines.md`](component-guidelines.md): commit the `.cue`
   change, run `scripts/render`, then commit the regenerated `holos/deploy/`
   tree (the `configmap-keycloak-realm-config.yaml` import document and the
   re-hashed `job-keycloak-config-<hash>.yaml` filename) together. `git status`
   must be diff-clean afterward.

## References

- [ADR-15 — Quay↔Keycloak OIDC SSO](../../docs/adr/ADR-15.md) — the
  decision record for the Quay SSO integration (Revision 2 dropped PKCE for the
  `quay` client).
- [`components/keycloak/realm-config/buildplan.cue`](../components/keycloak/realm-config/buildplan.cue)
  — the authoritative source: the keycloak-config-cli Job, the `argocd` and
  `quay` clients, the three mapper types, and the `quay-oidc` bootstrap.
- [`holos/README.md`](../README.md#keycloak-config-realm-reconciliation) — the
  operator-facing overview of `keycloak-config` and
  [Quay OIDC SSO and roles](../README.md#quay-oidc-sso-and-roles).
- [`docs/placeholders.md`](placeholders.md) — the resolved *Keycloak realm
  reconciliation* and *Quay OIDC login* entries.
- [`CLAUDE.md`](../../CLAUDE.md) — the Guard Rails: CUE Component Rendering,
  the Quay OIDC PKCE HOL-1233 workaround, Keycloak Configuration as Code, and
  OIDC Client Secrets.
