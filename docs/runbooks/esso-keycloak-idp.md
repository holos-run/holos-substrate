# esso ↔ holos: the Enterprise-SSO Realm and OIDC Broker Runbook

The platform runs **two** realms on the single Keycloak instance at
`https://auth.holos.internal`: the `holos` realm (the platform's identity store,
where every client and authorization lives) and a second `esso` realm modeling
an **upstream Enterprise SSO** identity provider. The `holos` realm **brokers**
logins from `esso` through an OIDC **identity provider** (broker alias `esso`),
so a person authenticates at `esso` and is authorized entirely by `holos`
groups/roles — `esso` authenticates, `holos` authorizes.

This runbook records the as-built shape (HOL-1366: HOL-1367 the ADR, HOL-1368
the esso realm, HOL-1369 the holos IdP + auto-link flow, HOL-1370 the
`scripts/apply` wiring) and how to verify, operate, and rotate it. The decision
record is [ADR-20](../adr/ADR-20.md) (*Two-realm topology: the `esso`
enterprise-SSO realm + the `holos` OIDC broker*); the declarative-client pattern
is [holos/docs/keycloak-clients.md](../../holos/docs/keycloak-clients.md). Those
are the sources of truth — this runbook is the operational companion.

## The topology in one paragraph

`esso` is a second realm on the **same** Keycloak instance, served at
`https://auth.holos.internal/realms/esso` by the existing `Keycloak` CR and the
`auth.holos.internal` `HTTPRoute` (no new route — every realm shares the one
hostname). It is **authentication-only**: it holds a single confidential OIDC
client (`clientId: https://auth.holos.internal/realms/holos`) the `holos` realm's
broker authenticates as, and a single pre-provisioned user, **alice**. The
`holos` realm declares an OIDC **identity provider** (`alias: "esso"`,
`trustEmail: true`, `firstBrokerLoginFlowAlias` pointing at a custom auto-link
flow) that points at the esso realm's discovery document. On alice's first login
through the broker, the holos realm's **first-broker-login auto-link** flow
matches her esso-verified email to a pre-provisioned `holos` user of the same
email and links the two silently — no duplicate account, no manual link prompt.

## How the esso realm is provisioned

The esso side is provisioned by **`scripts/apply` using Jobs only** — the
operator's realm-import CR plus keycloak-config-cli plus a bootstrap Job. It has
**no dependency on the holos-controller API groups** (`keycloak.holos.run` /
`quay.holos.run` / `security.holos.run`): the esso components are in the master
`scripts/apply`, never `scripts/apply-projects`, so a fresh cluster brings the
esso realm up without waiting on the controller (HOL-1370, ADR-20 AC #5).

Three pieces combine, all in the `keycloak` namespace:

1. **The esso realm shell** — a `KeycloakRealmImport` CR (`ESSO_REALM_IMPORT` in
   [`components/keycloak/instance/buildplan.cue`](../../holos/components/keycloak/instance/buildplan.cue))
   that bootstraps `realm: esso, enabled: true` and **nothing else** (no
   clients). Like the `holos` import it is bootstrap-only: the operator's import
   Job skips when the realm already exists. `scripts/apply` gates on its
   `KeycloakRealmImport/esso` `Done` condition (the same gate the `holos` import
   uses) in `wait_keycloak`.
2. **The esso realm contents** — the `realm-esso-config` component
   ([`components/keycloak/realm-esso-config/buildplan.cue`](../../holos/components/keycloak/realm-esso-config/buildplan.cue)),
   an idempotent keycloak-config-cli `Job` (`keycloak-esso-config`) that converges
   the confidential broker client and the alice user onto the realm shell on
   **every** apply. Its import document carries `realm: "esso"` only, so it never
   contends with the holos realm-config Job.
3. **The shared-secret + alice-password bootstrap** — the `esso-secret-bootstrap`
   generate-once `Job` in the same component, which creates two Secrets in the
   `keycloak` namespace (only if absent, never overwriting):
   - **`esso-idp-oidc`** (key `client_secret`) — the **single source** of the
     shared broker client secret; both the esso client and the holos IdP read it.
   - **`esso-user-alice`** (key `password`) — alice's generated password.

### Apply ordering

`scripts/apply` sequences the esso phases **between** `keycloak` and
`keycloak-config`:

```text
keycloak              # the Keycloak server + both realm shells (holos, esso)
keycloak-esso-config  # the esso broker client + alice + the shared-secret bootstrap
keycloak-config       # the holos realm: clients AND the esso identity provider
```

`keycloak-esso-config` must run **before** `keycloak-config` because the holos
realm-config Job (HOL-1369) reads the `esso-idp-oidc` Secret to inject the esso
IdP's `clientSecret` at import time (`$(env:ESSO_IDP_CLIENT_SECRET)`). The
`wait_keycloak_esso_config` gate (mirroring `wait_keycloak_config`) polls the
bootstrap Job, the reconcile Job, and verifies the `esso-idp-oidc` Secret exists
before the holos `keycloak-config` step runs. See the apply-ordering rationale
comment at the top of [`scripts/apply`](../../scripts/apply) and
[holos/README.md](../../holos/README.md#keycloak-config-realm-reconciliation).

## The shared `esso-idp-oidc` secret

Both ends of the broker authenticate with **one** secret value:

- The **esso confidential client** (`https://auth.holos.internal/realms/holos`,
  in the esso realm) holds it as its client secret — substituted into the esso
  realm-config import from `$(env:ESSO_IDP_CLIENT_SECRET)`.
- The **holos esso IdP** (in the holos realm) holds the same value as its
  `config.clientSecret` — substituted into the holos realm-config import from the
  same env var, read from the same `esso-idp-oidc` Secret.

The `esso-secret-bootstrap` Job is the single source: it generates the value once
and never rotates it, so re-applies are stable. The secret is created at runtime
and never committed (the *Runtime Secret Handling* guardrail in
[AGENTS.md](../../AGENTS.md)).

## The first-broker-login auto-link flow

The holos realm declares a **custom** (`builtIn: false`) first-broker-login flow
— not a redefinition of Keycloak's built-in `first broker login`, which
keycloak-config-cli refuses to add executions to (the
`Cannot find stored execution by authenticator 'idp-auto-link'` failure HOL-1369
fixed). The custom flow's aliases are unique
(`FIRST_BROKER_LOGIN_FLOW = "first broker login auto-link"` and its subflow
`"User creation or linking auto-link"`), and the esso IdP's
`firstBrokerLoginFlowAlias` points at it.

The flow's executions are `idp-review-profile` (REQUIRED), then a custom
subflow running `idp-create-user-if-unique` (ALTERNATIVE — *Detect Existing
Broker User*) followed by `idp-auto-link` (ALTERNATIVE — *Automatically Set
Existing User*). Combined with the esso IdP's `trustEmail: true`, a login whose
esso-asserted (and esso-verified) email matches a pre-provisioned holos user
auto-links **silently**: no profile prompt for an existing user, no manual
account-link confirmation.

This is the realm half of the auto-link mechanism ADR-20's `User`
relies on (a controller-created `holos` user pre-provisioned by email — e.g.
`bob@example.com`). For the local demo the pre-provisioned identity is **alice**.

## Verification

Run after creating the cluster per
[docs/local-cluster.md](../local-cluster.md) and `scripts/apply`.

### 1. The esso realm and client exist

```bash
# Both realm imports reached Done
kubectl -n keycloak get keycloakrealmimport
# esso realm discovery document is served (same hostname, /realms/esso)
curl -fs https://auth.holos.internal/realms/esso/.well-known/openid-configuration | jq .issuer
```

The issuer must be `https://auth.holos.internal/realms/esso`.

### 2. The shared secret and alice's password exist

```bash
kubectl -n keycloak get secret esso-idp-oidc esso-user-alice
# alice's password (the value you log in with below)
kubectl -n keycloak get secret esso-user-alice -o jsonpath='{.data.password}' | base64 -d; echo
```

### 3. The holos esso IdP is present

In the Keycloak admin console (`https://auth.holos.internal/admin/`, `holos`
realm → *Identity providers*), confirm an OpenID Connect provider with alias
**esso** exists, is **enabled**, has **Trust email** on, and its
*First login flow* is the custom `first broker login auto-link`.

### 4. Log in as alice through the broker (the end-to-end test)

alice's esso credentials:

- **Username:** `87654321` (the numeric subject an upstream Enterprise SSO
  commonly asserts)
- **Email:** `alice@example.com`
- **Password:** from `kubectl -n keycloak get secret esso-user-alice -o jsonpath='{.data.password}' | base64 -d`

To exercise the broker, browse to a `holos`-realm relying party
(`https://argocd.holos.internal` or `https://kargo.holos.internal`), choose the
**esso** identity provider on the login page, and sign in with alice's
credentials. To verify the **silent auto-link**, first pre-provision a `holos`
user with `email: alice@example.com` (e.g. a `User` CR, or by hand in the
admin console); alice's first esso login then links to that user with no
profile-review or account-link prompt. With no pre-provisioned match, the flow
falls through to ordinary federated-user creation.

## Maintenance

### Rotate the shared client secret

The secret is **generate-once** and not rotated automatically. To rotate it
deliberately, delete the Secret and re-run the bootstrap so a fresh value is
generated and propagated to both ends:

```bash
kubectl -n keycloak delete secret esso-idp-oidc
scripts/apply      # esso-secret-bootstrap regenerates esso-idp-oidc;
                   # keycloak-esso-config writes it onto the esso client and
                   # keycloak-config writes it onto the holos esso IdP
```

Because `scripts/apply` deletes and re-runs the keycloak-config-cli Jobs every
apply, both ends pick up the new value in the same run. Do **not** edit the
secret on only one end — the esso client and the holos IdP must hold the same
value or the broker token exchange fails. (There is no separate alice-password
rotation path; delete `esso-user-alice` and re-apply the same way to regenerate
it.)

### Change the esso realm contents

Edit the esso realm-config import document
([`components/keycloak/realm-esso-config/buildplan.cue`](../../holos/components/keycloak/realm-esso-config/buildplan.cue)),
run `scripts/render`, and commit the regenerated `holos/deploy/` tree, then
`scripts/apply`. The realm shell (`enabled`) stays owned by the
`KeycloakRealmImport` CR; everything else under `realm: "esso"` is the Job's.

### Change the holos esso IdP or auto-link flow

Both the `identityProviders[]` entry and the custom first-broker-login
`authenticationFlows[]` live in the **holos** realm-config component
([`components/keycloak/realm-config/buildplan.cue`](../../holos/components/keycloak/realm-config/buildplan.cue)),
reconciled by the `keycloak-config` Job. Edit there, render, commit, and apply.
Keep the custom flow's aliases distinct from Keycloak's built-in names — a
redefinition of the built-in `first broker login` is what HOL-1369 had to fix.

## Known limitations

- **One pre-provisioned esso user (alice).** The esso realm models an upstream
  IdP with a single demo identity. In a production deployment `esso` is replaced
  by the customer's real Enterprise SSO; the holos IdP `config` (discovery URL,
  clientId/secret) repoints at it.
- **Local CA / local issuer only.** Trust rests on the machine-local mkcert
  `local-ca` root and the `*.holos.internal` issuer; both realms share the one
  hostname. A real CA and a public issuer for production are future hardening —
  see the [Production deployment area](../../holos/docs/placeholders.md#production-deployment-area)
  placeholder.
- **Authentication only.** `esso` confers no authorization. All groups, roles,
  and relying-party access live in the `holos` realm ([ADR-3](../adr/ADR-3.md));
  brokering an esso login never grants more than the linked holos user holds.

## References

- [ADR-20](../adr/ADR-20.md) — the decision record (*Two-realm topology: the
  `esso` enterprise-SSO realm + the `holos` OIDC broker*, Revision 5).
- [holos/docs/keycloak-clients.md](../../holos/docs/keycloak-clients.md) — the
  declarative-client pattern, including the *esso realm and the holos esso IdP*
  section.
- [holos/docs/kargo-keycloak-oidc.md](../../holos/docs/kargo-keycloak-oidc.md) /
  [Quay↔Keycloak OIDC runbook](quay-keycloak-oidc.md) — the sibling
  holos-realm OIDC integrations.
- [`components/keycloak/realm-esso-config/buildplan.cue`](../../holos/components/keycloak/realm-esso-config/buildplan.cue)
  — the esso realm contents (broker client, alice, the bootstrap Job).
- [`components/keycloak/realm-config/buildplan.cue`](../../holos/components/keycloak/realm-config/buildplan.cue)
  — the holos esso IdP and the custom first-broker-login auto-link flow.
- [`components/keycloak/instance/buildplan.cue`](../../holos/components/keycloak/instance/buildplan.cue)
  — the esso realm shell `KeycloakRealmImport`.
- [`scripts/apply`](../../scripts/apply) — the apply ordering and the
  `wait_keycloak_esso_config` gate.
