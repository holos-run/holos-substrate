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
list. The platform runs three clients today — `argocd`, `quay`, and `kargo`.
They differ in the usual per-client details (redirect URIs, web origins, roles,
and which protocol mappers they carry), but the key template distinction is
**public vs confidential**, which also decides whether a client-secret bootstrap
is needed. `argocd` and `kargo` are public PKCE clients; `quay` is the
confidential PKCE client (see below). All three use PKCE (`S256`).

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

### Confidential PKCE client (quay)

Quay's `KEYCLOAK_LOGIN_CONFIG` validator requires a `CLIENT_SECRET`, so the
`quay` client is **confidential** (`publicClient: false`) and authenticates with
that secret. Like `argocd`, it also uses PKCE (`S256`): the client sets
`pkce.code.challenge.method: "S256"`, matching Quay's `USE_PKCE: true` /
`PKCE_METHOD: "S256"` (ADR-15 Revision 4, HOL-1293). PKCE was briefly disabled
for `quay` (Revision 2, HOL-1257) over a `code exchange: 400` failure but
re-enabled on both ends in Revision 4 — so `quay` is **no longer** a PKCE
exception. See [ADR-15](../../docs/adr/ADR-15.md) and the operational
[Quay↔Keycloak OIDC runbook](../../docs/runbooks/quay-keycloak-oidc.md). The
`secret` field holds a `$(env:...)` placeholder, never a literal value:

```cue
clientId:                  QUAY_CLIENT_ID
publicClient:              false // confidential: Quay sends a client secret
standardFlowEnabled:       true
serviceAccountsEnabled:    false
directAccessGrantsEnabled: false
secret: "$(env:QUAY_OIDC_CLIENT_SECRET)"
attributes: "pkce.code.challenge.method": "S256"
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
[`AGENTS.md`](../../AGENTS.md)'s *OIDC Client Secrets* guard rail.

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
  mapper), so the single `groups` claim Quay receives carries group
  memberships, the `quay` client-role names, and — as of HOL-1245 — the
  `platform-owner` realm role, uniformly. (Automatic team syncing from this
  claim is **enabled** — `FEATURE_TEAM_SYNCING: true` under Quay's OIDC auth
  backend, ADR-15 Revision 4 — so Quay team membership tracks the claim on the
  `TEAM_RESYNC_STALE_TIME` cadence.)

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
in themselves. The matching Quay team's membership is synced from the claim and
the team's permissions are what grant access. Automatic group/role-name → team
binding from the claim is **enabled** under Quay's OIDC auth backend
(`FEATURE_TEAM_SYNCING: true`, ADR-15 Revision 4 — the OIDC user handler syncs
groups) on the `TEAM_RESYNC_STALE_TIME` cadence; a superuser performs the
one-time team→group binding setup. Per-project roles follow the same convention:
add a `quay` client role named for the project and grant it.

### `platform-owner` into the quay `groups` claim (HOL-1245)

As of HOL-1245 the `quay` client also emits the `platform-owner` realm role into
the `groups` claim, mirroring the `argocd` client. Granting a user the
`platform-owner` realm role surfaces `platform-owner` in their `groups` claim,
so Quay (with team syncing on — `FEATURE_TEAM_SYNCING: true` under the OIDC
backend, ADR-15 Revision 4) and any future relying party key on it the same way
they key on group names.

### The Quay-superuser limitation (not automatic)

Surfacing `platform-owner` (or `platform-admin`) into the `groups` claim does
**not** make a user a Quay superuser. Quay's `SUPER_USERS` is a **static
username list in `config.yaml`** with no claim-driven superuser sync — there is
no mechanism for Quay to promote a user to superuser from an OIDC claim. So
`role → superuser` is **not** automatic.

The supported path today is the **manual `SUPER_USERS` bootstrap**: add the
user's `preferred_username` to `SUPER_USERS` in
[`components/quay/buildplan.cue`](../components/quay/buildplan.cue) and
re-render/apply. Under the OIDC backend there is no local `admin` user; the
seeded superusers are the two Keycloak realm users `svc-quay-resource-controller`
(a service account) and `quay-admin` (a human administrator), both in
`SUPER_USERS` (ADR-15 Revision 4). This keeps the README's
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
   wherever the flow supports it, as the platform default — all three clients
   carry it: the public `argocd` and `kargo`, and the confidential `quay`
   (re-enabled in ADR-15 Revision 4 / HOL-1293). There is **no** standing PKCE
   exception today; only relax PKCE for a client with a demonstrated
   implementation gap (see step 7).
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
   hit this once — its confidential OIDC client failed to round-trip a
   `code_verifier`, producing `code exchange: 400` at the token endpoint, so
   HOL-1257 (Revision 2) dropped PKCE for the `quay` client. That was later
   **resolved**: HOL-1293 (ADR-15 Revision 4) re-enabled PKCE (`S256`) on both
   ends — the `quay` client now carries `pkce.code.challenge.method: "S256"` and
   Quay sets `USE_PKCE: true` / `PKCE_METHOD: "S256"` — matching the production
   reference configuration. So there is **no** standing no-PKCE client today.
   When PKCE *is* mismatched (set on only one end) the same `code exchange: 400`
   appears as a verification signal; the troubleshooting lives in the
   [Quay↔Keycloak OIDC runbook](../../docs/runbooks/quay-keycloak-oidc.md). Only
   relax or skip requiring PKCE for a client with a demonstrated implementation
   gap.
8. **Render then commit.** This is a `holos/components/` change, so follow the
   render contract in [`AGENTS.md`](../../AGENTS.md) and
   [`component-guidelines.md`](component-guidelines.md): commit the `.cue`
   change, run `scripts/render`, then commit the regenerated `holos/deploy/`
   tree (the `configmap-keycloak-realm-config.yaml` import document and the
   re-hashed `job-keycloak-config-<hash>.yaml` filename) together. `git status`
   must be diff-clean afterward.

## References

- [ADR-15 — Quay↔Keycloak OIDC SSO](../../docs/adr/ADR-15.md) — the
  decision record for the Quay SSO integration (Revision 4: OIDC backend, PKCE
  re-enabled for the `quay` client, two Keycloak-backed superusers).
- [Quay↔Keycloak OIDC runbook](../../docs/runbooks/quay-keycloak-oidc.md) — the
  operational companion: wiring, the two superuser realm users, secret rotation,
  and the `code exchange: 400` PKCE-verification note.
- [Quay Resource Controller credentials runbook](../../docs/runbooks/quay-resource-controller-credentials.md)
  — the manual procedure for minting the future controller's OAuth-Application
  credential.
- [`components/keycloak/realm-config/buildplan.cue`](../components/keycloak/realm-config/buildplan.cue)
  — the authoritative source: the keycloak-config-cli Job, the `argocd` and
  `quay` clients, the three mapper types, and the `quay-oidc` bootstrap.
- [`holos/README.md`](../README.md#keycloak-config-realm-reconciliation) — the
  operator-facing overview of `keycloak-config` and
  [Quay OIDC SSO and roles](../README.md#quay-oidc-sso-and-roles).
- [`docs/placeholders.md`](placeholders.md) — the resolved *Keycloak realm
  reconciliation* and *Quay OIDC login* entries.
- [`AGENTS.md`](../../AGENTS.md) — the Guard Rails: CUE Component Rendering,
  the Quay auth note (OIDC backend, HOL-1293), the `svc-` service-account naming
  convention, Keycloak Configuration as Code, and OIDC Client Secrets.
