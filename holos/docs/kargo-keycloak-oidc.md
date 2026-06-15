# Kargo ↔ Keycloak OIDC: the SSO Login Pattern

The Kargo control plane ([`components/kargo/`](../components/kargo/buildplan.cue))
authenticates its API/UI against the Keycloak `holos` realm with OIDC, so the
only way into `https://kargo.holos.localhost` is a Keycloak login — there is no
local admin account. The Keycloak `kargo` client was provisioned in HOL-1250
and the Kargo-side wiring landed in HOL-1251; this document records the
as-built shape and how to maintain it. Verify it live with the procedure in
[Verification](#verification) — re-run it after any change to the pieces below.

## The pattern in one paragraph

Kargo is a **public OIDC client** of the Keycloak `holos` realm: it uses the
Authorization Code flow with **PKCE (S256)** and holds **no client secret**.
`api.oidc.enabled: true` points the API at the issuer
`https://auth.holos.localhost/realms/holos` and the public `kargo` client; the
bundled Dex broker is off (`api.oidc.dex.enabled: false`) because Keycloak is a
first-class OIDC provider. The realm roles a user holds arrive in a single
`groups` claim, and `api.oidc.admins/viewers/users` map those role names to
Kargo access levels — `platform-owner` → system-wide admin. The chart's
built-in admin account stays disabled, so Keycloak SSO is the only login path.

## How this differs from Argo CD (and why)

Argo CD ([`components/argocd/`](../components/argocd/controller/buildplan.cue))
authenticates against the same realm, but it trusts the local-CA-signed issuer
certificate by setting `oidc.tls.insecure.skip.verify` — a skip-verify knob.
**Kargo has no such knob.** Its only trust mechanism is `api.cabundle`, so
Kargo instead follows the **Quay `CA_CERTIFICATE` pattern**
([`components/quay/buildplan.cue`](../components/quay/buildplan.cue)): a
cert-manager `Certificate` materialises the `local-ca` root into the kargo
namespace, and the API's `parse-cabundle` initContainer installs it into the
system trust store. The backchannel-reachability piece — making
`auth.holos.localhost` resolve and route in-cluster — is copied almost verbatim
from Argo CD's `ServiceEntry`.

## Components involved

Two components combine to make login work. Both are reconciled on every
`scripts/apply`.

### Keycloak side — the `kargo` client

[`components/keycloak/realm-config/buildplan.cue`](../components/keycloak/realm-config/buildplan.cue)
declares the `kargo` client (reconciled by the keycloak-config-cli Job — see
[Keycloak realm reconciliation](placeholders.md#keycloak-realm-reconciliation)
and [keycloak-clients.md](keycloak-clients.md)):

- `publicClient: true`, `standardFlowEnabled: true`,
  `attributes."pkce.code.challenge.method": "S256"` — a public PKCE client,
  modeled on the `argocd` client (not the confidential `quay` client). No
  client secret and no bootstrap Job.
- `serviceAccountsEnabled: false`, `directAccessGrantsEnabled: false` — a public
  client holds no secret, so the confidential-only flows must be off.
- `redirectUris: ["https://kargo.holos.localhost/*"]`,
  `webOrigins: ["https://kargo.holos.localhost"]` — the web-UI OAuth callback.
- **Two `groups`-claim protocol mappers**, both writing the same `groups` claim
  into the ID, access, and userinfo tokens:
  - `groups` (`oidc-group-membership-mapper`) — Keycloak **group** names
    (e.g. `authenticated`), bare names, not paths.
  - `realm-roles` (`oidc-usermodel-realm-role-mapper`) — the platform **realm
    role** names (`platform-owner`, `platform-editor`, `platform-viewer`),
    multivalued, folded into the same `groups` claim.

  Both mappers are attached to the client and are **unconditional** — they are
  not gated behind an optional client scope. That is why the Kargo side requests
  no `groups` scope (see [below](#why-additionalscopes-)).

### Kargo side — the API wiring

[`components/kargo/buildplan.cue`](../components/kargo/buildplan.cue) carries
three pieces that must land together:

1. **`api.oidc.*` Helm values** — the OIDC configuration and the role mapping
   (see [Role mapping](#role-mapping)).
2. **A cert-manager `Certificate`** (`CA_CERTIFICATE`, secret `kargo-local-ca`)
   issued by the `local-ca` `ClusterIssuer`. Every cert-manager Secret carries
   the signing CA in its `ca.crt` key; only `ca.crt` is consumed. The leaf cert
   itself is never served — it is just the lightest cert-manager-native way to
   put the CA root into this namespace (trust-manager is not deployed). The chart
   mounts the whole Secret via `api.cabundle.secretName: kargo-local-ca` and its
   `parse-cabundle` initContainer installs every cert it finds into the trust
   store.
3. **A `ServiceEntry`** (`auth-holos-localhost`) for the issuer hostname → the
   shared Istio Gateway. The kargo namespace is ambient-enrolled, and
   `*.localhost` resolves to loopback both upstream of CoreDNS and inside
   ztunnel's DNS proxy, so a plain DNS override cannot reach Keycloak from an
   enrolled pod. The `ServiceEntry` makes `auth.holos.localhost` a service the
   mesh resolves to the Gateway (`default-istio.istio-gateways.svc.cluster.local`,
   port 443, `resolution: DNS`), which terminates TLS for `*.holos.localhost`
   and routes by SNI/Host to the keycloak `HTTPRoute`. The API thus traverses
   the exact host path browsers use, and the `iss` claim matches
   `api.oidc.issuerURL` end-to-end. This is the Argo CD `SERVICE_ENTRY` pattern.

All three render through the component's `Resources` generator and the
kubectl-slice transformer into `certificate-kargo-local-ca.yaml`,
`serviceentry-auth-holos-localhost.yaml`, and the OIDC config in
`configmap-kargo-api.yaml` / `deployment-kargo-api.yaml` under
[`holos/deploy/clusters/k3d-holos/components/kargo/`](../deploy/clusters/k3d-holos/components/kargo/).

## Role mapping

The `groups` claim carries realm-role names, and `api.oidc` maps them to Kargo
access levels:

| Realm role / group | Kargo `api.oidc` field | Access level |
| --- | --- | --- |
| `platform-owner` | `admins.claims.groups` | System-wide admin |
| `platform-viewer` | `viewers.claims.groups` | Read-only, all resources |
| `platform-editor`, `authenticated` | `users.claims.groups` | Baseline read of cluster-scoped resources |

`platform-editor` is grouped with the `authenticated` default-group baseline
because it has no system-level edit role until project-scoped roles exist.

**Baseline access is automatic for every realm user.** `authenticated` is a
**realm default group** (`defaultGroups: ["/authenticated"]` in the realm-config
component), so every user in the `holos` realm is a member of it from creation.
That group name flows through the `groups` claim and matches
`users.claims.groups`, so **any** Keycloak realm user with an email already
receives Kargo's baseline `users` access (read of cluster-scoped resources) on
first login — no explicit role assignment is required. Kargo is therefore *not*
gated to an allow-list of users; it is gated to "anyone who can log into the
holos realm", which on this single-user local cluster is the intended posture.
Tightening that takes a **paired** change: introduce a Kargo-specific realm
group (or role) in the realm-config component, **and** replace `authenticated`
in `api.oidc.users.claims.groups` in
[`components/kargo/buildplan.cue`](../components/kargo/buildplan.cue) with that
new group — because Kargo's own claim mapping is what authorizes the
`authenticated` default group. Leaving the Kargo-side claim on `authenticated`
keeps every realm user authorized no matter what the realm-config side does.

**To elevate a user above baseline** (viewer or admin), assign them the
corresponding Keycloak realm role — `platform-viewer` or `platform-owner` — (or
membership in a group that confers it) in the `holos` realm, and make sure their
account has an email set. The next login carries the role name in the `groups`
claim, and Kargo maps it to the access level above. No Kargo-side change is
needed — only the Keycloak role assignment.

## Why `additionalScopes: []`

The Kargo chart defaults `api.oidc.additionalScopes` to `["groups"]`. This
component overrides it to `[]`. The `openid`, `profile`, and `email` scopes are
always requested by Kargo and need not be listed. The `groups` claim arrives
**unconditionally** from the client-attached protocol mappers above — it is not
tied to a `groups` client scope. There is no registered `groups` scope on the
`kargo` client, so requesting one would make Keycloak reject the authorization
request with `invalid_scope`. The override therefore both prevents that error
and preserves the unconditional `groups` claim the role mapping depends on.

Note one rendering subtlety: the chart templates
`OIDC_ADDITIONAL_SCOPES: {{ join "," .additionalScopes }}` unquoted, so an empty
list renders a bare key that YAML parses as `null` — an invalid ConfigMap value.
The component applies a Kustomize JSON-6902 patch to coerce that to the quoted
empty string `""`. If a chart bump changes how this value is templated, re-check
that patch still targets the right key.

## Maintenance over time

### Adjusting role mappings

Change which realm role maps to which Kargo level by editing the
`api.oidc.admins/viewers/users.claims.groups` lists in
[`components/kargo/buildplan.cue`](../components/kargo/buildplan.cue), then run
`scripts/render` and commit the regenerated **ServiceAccount** manifests —
`serviceaccount-kargo-admin.yaml`, `serviceaccount-kargo-viewer.yaml`, and
`serviceaccount-kargo-user.yaml`, whose `rbac.kargo.akuity.io/claims` annotations
carry the group lists (the `OIDC_*` keys in `configmap-kargo-api.yaml` hold only
`enabled`/`issuerURL`/`clientID`/`additionalScopes`/the username claim, not the
role mapping). To add a new realm role to the claim, add it to the realm roles in
[`components/keycloak/realm-config/buildplan.cue`](../components/keycloak/realm-config/buildplan.cue)
(the `realm-roles` mapper emits all realm-role names automatically).

### After a Kargo chart bump

The chart version is pinned in both
[`components/kargo/buildplan.cue`](../components/kargo/buildplan.cue) and the
sibling `kargo-crds` component. After bumping, re-verify against the vendored
chart (`vendor/<version>/kargo`):

- The `api.oidc.*` value schema is unchanged — `enabled`, `issuerURL`,
  `clientID`, `dex.enabled`, `additionalScopes`, and the
  `admins/viewers/users.claims` shape still exist and mean the same thing.
- `api.cabundle.secretName` is still the trust mechanism, and the
  `parse-cabundle` initContainer still mounts the Secret and installs `ca.crt`
  into the trust store (it runs `runAsUser: 0`; this is admissible only because
  the chart applies `global.securityContext` at the container level, with no
  pod-level `runAsNonRoot`).
- The `OIDC_ADDITIONAL_SCOPES` ConfigMap templating still matches the Kustomize
  patch (see [above](#why-additionalscopes-)).

Then re-run the live [Verification](#verification).

### Reconciliation

The `kargo` Keycloak client is reconciled declaratively by the keycloak-config-cli
`Job` on **every** `scripts/apply` (the
[Keycloak realm reconciliation](placeholders.md#keycloak-realm-reconciliation)
mechanism): edits to the client land by changing the realm-config document and
re-applying, not by manual admin-console edits. The Kargo-side OIDC config,
`Certificate`, and `ServiceEntry` are ordinary rendered manifests applied the
same way.

## Verification

This is the live login test (the same end-to-end check from HOL-1251):

1. Create the local cluster with DNS and trusted TLS per
   [docs/local-cluster.md](../../docs/local-cluster.md), then `scripts/apply`.
2. In Keycloak (`https://auth.holos.localhost`, `holos` realm), give a user the
   `platform-owner` realm role and ensure the account has an **email** set.
3. Browse to `https://kargo.holos.localhost` and log in via Keycloak. You are
   redirected to Keycloak, authenticate, and land back in the Kargo UI.
4. Confirm **system-wide admin** access (the `platform-owner` → `admins`
   mapping): all projects and system-level configuration are visible and
   editable.
5. If login fails, read the API logs:

   ```bash
   kubectl -n kargo logs deploy/kargo-api
   ```

   - `x509: certificate signed by unknown authority` (or any issuer-TLS error)
     → the cabundle is not trusted. Re-check that the `kargo-local-ca` Secret
     exists and `api.cabundle.secretName` points at it, and that the
     `parse-cabundle` initContainer ran.
   - `invalid_scope` from Keycloak → re-check `additionalScopes: []` and that no
     `groups` scope is being requested.
   - `invalid_redirect_uri` → re-check the `redirectUris` on the `kargo` client
     in the realm-config component.

## Known limitations

- **No UI break-glass account.** `api.adminAccount.enabled` stays `false`, so
  there is no local Kargo admin login and no admin Secret to commit or rotate —
  Keycloak SSO is the only way in. If Keycloak is down, the Kargo UI is
  inaccessible; recovery is operating Kargo through the Kubernetes API directly.
- **Kargo CLI SSO is not wired.** Only the web-UI redirect URI is registered on
  the `kargo` client. The Kargo CLI's loopback SSO redirect
  (`http://localhost:<port>/...`) is intentionally **not** registered yet, and
  `api.oidc.cliClientID` is unset. Enabling CLI SSO is a paired change: set
  `cliClientID` in the kargo component **and** add the loopback redirect URI to
  the `kargo` client in the realm-config component, once the CLI's callback
  port/path is confirmed.
- **Local CA / local issuer only.** Trust rests on the machine-local mkcert
  `local-ca` root and the `*.holos.localhost` issuer. A real (non-local) CA and
  a public issuer hostname for a production deployment are future hardening —
  see the [Production deployment area](placeholders.md#production-deployment-area)
  placeholder.
