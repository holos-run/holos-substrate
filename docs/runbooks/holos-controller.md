# Runbook: Holos Controller — credential wiring

Operator-facing guide for running the **Holos Controller**
([ADR-18](../adr/ADR-18.md)) and wiring it to the two credentials it
authenticates with: the **Quay** superuser credential its `quay.holos.run`
Organization and Repository resources ([ADR-19](../adr/ADR-19.md)) use, and the
**Keycloak** admin credential its `keycloak.holos.run` resources
([ADR-20](../adr/ADR-20.md)) use. The controller installs to the
**`holos-controller`**
namespace and is built/deployed with the isolated `controller-*` make targets
(`Makefile.controller`), separate from `scripts/apply` and `scripts/render`.

This runbook covers the superuser-token assumption — *a superuser-account OAuth
Application token authenticates all controller-managed Quay operations* — and how
to satisfy it. The credential itself is minted by the companion
[Quay Resource Controller credentials runbook](quay-resource-controller-credentials.md);
this runbook is the consumer side: where that token lands and how the controller
reads it.

## The superuser-token assumption

Every Quay operation the controller performs — create/adopt an Organization,
create/adopt a Repository, register a `repo_push` webhook — is authenticated with
**one OAuth-Application token that acts as a Quay superuser**, the
`svc-quay-resource-controller` realm user ([ADR-15](../adr/ADR-15.md) Revisions
4–5). This is a deliberate design assumption, not an incidental one:

- Under `AUTHENTICATION_TYPE: OIDC` there is no local `admin` and no headless
  token-mint path, so the token is minted **by hand, once**, per the credentials
  runbook.
- The token carries `super:user` (plus `org:admin`/`repo:create`) and the
  instance has `FEATURE_SUPERUSERS_FULL_ACCESS: true`, so the controller can both
  **create** orgs and **adopt** orgs other identities created — it is the system
  of record for the Quay data plane and must be able to converge any org it owns
  or is told to adopt.
- Because that reach is instance-wide, the Organization reconciler enforces the
  **ownership/claim model** (ADR-19): it never silently seizes a pre-existing,
  externally-created org — adoption is an explicit `spec.adopt: true` opt-in, and
  the durable `status.created` marker records whether the controller created
  (deletes on removal) or adopted (releases on removal) each org. Repository
  resources use the same claim/adopt/release shape for pre-existing repositories,
  with an ownership marker in the Quay repository description.

There is **one** credential for the whole controller, resolved from its own
namespace; resources do not each carry distinct credentials (they may *name* a
different Secret via `credentialsSecretRef`, but the conventional and default
case is the single shared Secret below).

## The credential Secret: `holos-controller-quay-creds`

The controller resolves the Quay credential from a Secret named by each
resource's `spec.credentialsSecretRef` (a `{name, key}` reference), defaulting to
**`holos-controller-quay-creds`** when omitted. Two properties matter:

- **Namespace = the controller's own namespace.** The resolver reads the Secret
  from the controller's namespace — **`holos-controller`** — taken from the
  `POD_NAMESPACE` downward-API env the manager Deployment sets (default
  `holos-controller`), **not** the resource's namespace. So one operator-managed
  credential in `holos-controller` serves every tenant Organization/Repository
  across all namespaces.
- **Keys the controller reads** (`internal/controller/quay/credentials.go`):

  | Key | Required | Meaning |
  |-----|----------|---------|
  | `url` | yes | the Quay API base URL (e.g. `https://quay.holos.internal`). |
  | `token` | yes | the superuser OAuth-Application access token. |
  | `username` | no | informational — the identity the token acts as (`svc-quay-resource-controller`). |

  `credentialsSecretRef.key`, when set, narrows the **token** lookup to a specific
  key; `url` and `username` always use the conventional key names. When the Secret
  or a required key is missing, the reconciler sets `Programmed`/`Ready` `False`
  with reason `CredentialsNotFound` and requeues — it does not crash.

The Deployment does **not** `envFrom`/mount a fixed credential Secret; credentials
are resolved per-resource via `credentialsSecretRef` at reconcile time (HOL-1313).
The Secret's material is created at **runtime** and **never committed** (the
runtime-secret guardrail, [`AGENTS.md`](../../AGENTS.md) Conventions /
[`holos/docs/secret-handling.md`](../../holos/docs/secret-handling.md)).

## Wiring the credential

1. **Mint the token** by hand following
   [quay-resource-controller-credentials.md](quay-resource-controller-credentials.md):
   sign in to Quay via "Holos SSO" as `svc-quay-resource-controller`, create the
   OAuth Application in the `platform-automation` org, and generate a token with
   the authoritative scope set that runbook specifies (the same set the helper
   script selects — `super:user`/`org:admin`/`repo:create` plus the repo
   read/write/admin and user scopes the Repository reconciler needs).

2. **Create the Secret** with the operator helper
   [`scripts/apply-svc-quay-resource-controller-creds`](../../scripts/apply-svc-quay-resource-controller-creds).
   The script keeps its historical name (the token acts as the
   `svc-quay-resource-controller` superuser identity) but now creates the Secret
   the controller actually reads:

   ```bash
   scripts/apply-svc-quay-resource-controller-creds
   # prompts for the token; sets QUAY_URL (default https://quay.holos.internal)
   ```

   It produces, in the `holos-controller` namespace:

   | Field | Value |
   |-------|-------|
   | Namespace | `holos-controller` |
   | Secret name | `holos-controller-quay-creds` |
   | Keys | `url`, `token`, optional `username` |

3. **Reference it** (or rely on the default). A resource that omits
   `credentialsSecretRef` resolves `holos-controller-quay-creds` automatically;
   set the field only to point at a differently-named Secret.

## The Keycloak admin credential: `holos-controller-keycloak-creds`

The controller also reconciles the `keycloak.holos.run` API group
([ADR-20](../adr/ADR-20.md)) — `Instance`, `Group`,
`User`, `Client` — against the Keycloak Admin REST API. Unlike
the Quay token, this credential is **not minted by hand**: it is provisioned at
runtime by the platform's `keycloak-config` component (HOL-1348), so a plain
`scripts/apply` leaves the controller ready to talk to Keycloak with no operator
step.

- **Credential shape (ADR-20, preferred):** a confidential **service-account
  client** named **`svc-holos-controller`** in the `holos` realm, with *Service
  Accounts Enabled* and the scoped `realm-management` client roles
  `manage-clients`, `manage-users`, `query-groups`, `query-clients` (not blanket
  realm-admin). The controller authenticates with a `client_credentials` grant.
  The client is declared in
  [`holos/components/keycloak/realm-config/buildplan.cue`](../../holos/components/keycloak/realm-config/buildplan.cue)
  (`CONTROLLER_CLIENT_ID`) and reconciled by the `keycloak-config`
  keycloak-config-cli Job.
- **Provisioning (generate-once, never committed):** the realm-config component's
  `CONTROLLER_CREDS_BOOTSTRAP` Job generates the client secret once and writes it
  into two Secrets — `svc-holos-controller-oidc` (key `client_secret`) in the
  `keycloak` namespace (read by keycloak-config-cli to set the client's secret in
  the realm) and the controller credential Secret below — mirroring the
  `quay-oidc` bootstrap. It never rotates the value, so it stays stable across
  reconciles, and never commits it (the runtime-secret guardrail).
- **The credential Secret** the controller reads
  (`internal/controller/keycloak/credentials.go`), resolved from its own
  `holos-controller` namespace via `POD_NAMESPACE`:

  | Field | Value |
  |-------|-------|
  | Namespace | `holos-controller` |
  | Secret name | `holos-controller-keycloak-creds` (default; the `Instance` `credentialsSecretRef` default) |
  | Keys | `clientId`, `clientSecret` (optional `tokenUrl`) |

  `clientId` is `svc-holos-controller`; `clientSecret` is the generated secret.
  The `Instance` spec carries the `url` and `realm`, so the in-cluster
  token endpoint is derived and `tokenUrl` is omitted. When the Secret or a
  required key is missing the reconciler sets `Ready` `False` with reason
  `CredentialsNotFound` and requeues.

The central `Instance` (`holos-keycloak`, in the `keycloak` namespace) and
its `security.holos.run` `ReferenceGrant` are emitted by the `keycloak-instance`
component and applied — with the per-cluster local-ca `caBundle` injected at apply
time — by `scripts/apply-projects` (the caBundle is per-cluster trust material
that is never committed, the same pattern as the `my-project` Organization).

## Deploy and verify the controller

The controller's lifecycle is driven by the isolated `controller-*` targets — they
never touch `scripts/apply` or `scripts/render`:

```bash
make controller-manifests        # regenerate CRDs + RBAC from Go markers
make controller-install          # kubectl apply -k config/crd/holos-controller
make controller-manifests-build  # render config/deploy/holos-controller
make controller-deploy           # kubectl apply -k config/deploy/holos-controller
```

The `holos-controller` namespace itself is owned by the central registry
(`holos/namespaces.cue`, `_ambient: true`) and rendered by `scripts/render`; the
kustomize tree targets it but does not create the Namespace object.

`make controller-deploy` deploys the controller from the in-cluster registry
image (`quay.holos.internal/holos/holos-controller:dev` by default) — its
**steady-state / dev** path, used **once the holos Quay organization exists** and
the controller image has been published to it (`make controller-docker-push`).
On a **freshly bootstrapped** cluster that registry does not exist yet (it is
exactly what the bootstrap provisions), so the controller cannot be pulled from
it. Use [`scripts/apply-holos-controller`](../../scripts/apply-holos-controller)
(HOL-1380) for the bootstrap deploy instead: it reuses `make controller-install`
and `make controller-deploy`, overriding the deploy image to a pinned, publicly-pullable
`ghcr.io/holos-run/holos-controller:<tag>`, so the controller can come up and
reconcile the `holos` Quay organization
([`scripts/apply-holos-quay-organization`](../../scripts/apply-holos-quay-organization))
before the in-cluster registry is its own host. After the org is up and the image
is published there, the ordinary `make controller-deploy` is the path going
forward.

Verify:

```bash
# Manager is running in holos-controller:
kubectl -n holos-controller get deploy,pod

# The credential Secret exists with the expected keys:
kubectl -n holos-controller get secret holos-controller-quay-creds \
  -o jsonpath='{.data}' | tr ',' '\n'   # expect url, token (username optional)

# Metrics are scrapable:
kubectl -n holos-controller get svc        # holos-controller-manager-metrics-service:8080
# controller-runtime reconcile metrics plus the custom collectors:
#   holos_controller_reconcile_total{group,kind,outcome}
#   holos_controller_quay_api_requests_total{operation,outcome}
```

A reconciling resource reports Gateway-API conditions
(`Accepted`/`Programmed`/`Ready`, and `WebhookConfigured` on a Repository); a
`CredentialsNotFound` reason on `Ready=False` means the Secret/key wiring above is
incomplete.

### Verify Organization synced teams

An Organization may declare OIDC-synced Quay teams in `spec.syncedTeams` — each
with an org `role` (`admin`/`creator`/`member`), optional per-team `adopt`, and
an optional org default repository permission `repositoryPermission`
(`read`/`write`/`admin`) — which the controller reconciles into Quay after the
org is provisioned ([ADR-19](../adr/ADR-19.md) Revisions 6 and 11). To verify
they reconcile:

```bash
# The teams the controller created and manages are tracked in status:
kubectl -n my-project get organization my-project \
  -o jsonpath='{.status.managedTeams}'   # expect the spec.syncedTeams names

# A pre-existing, externally-created team named in spec.syncedTeams is a conflict
# by default. Set spec.syncedTeams[].adopt: true to claim an unmarked team; a team
# marked for another resource still surfaces Ready=False with reason TeamConflict
# and a Warning event:
kubectl -n my-project get organization my-project \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'
kubectl -n my-project describe organization my-project   # see the Warning event
```

Management is **non-exclusive**: teams the controller never created (absent from
`status.managedTeams`) are ignored, and a team removed from `spec.syncedTeams` is
de-provisioned only if it was controller-managed. `FEATURE_TEAM_SYNCING`
([ADR-15](../adr/ADR-15.md)) keeps each synced team's *membership* tracking the
OIDC `groups` claim; the Organization CR declares which teams exist, their role,
and their default permission.

## Cluster bring-up — provisioning the `my-project` sample

Once the controller is deployed and the credential Secret is wired, the
`my-project` Layer 3 delivery sample is applied **separately** from the master
platform apply. As of HOL-1322, `my-project` is **removed from `scripts/apply`**
and applied by the dedicated
[`scripts/apply-projects`](../../scripts/apply-projects), because its
`quay.holos.run` Organization carries a per-cluster `caBundle` that must be
injected at apply time and never committed.

Run the bring-up steps **in order**:

1. **`scripts/local-ca`** — establishes the cert-manager `local-ca` whose
   certificate the in-cluster Quay serves TLS with, and whose PEM the next step
   injects as the Organization's `caBundle`.
2. **`scripts/apply`** — applies the platform (including the Quay registry the
   controller and credential mint target, the `holos-controller` Namespace —
   owned by the central namespace registry and applied here, **not** by
   `make controller-deploy` — and, via the `keycloak-config` component, the
   controller's **Keycloak admin credential**: the `svc-holos-controller`
   service-account client and the generate-once `CONTROLLER_CREDS_BOOTSTRAP` Job
   that writes the `holos-controller-keycloak-creds` Secret, HOL-1348). The
   Keycloak credential therefore needs **no** manual mint.
3. **`make controller-install && make controller-deploy`** — installs the
   `quay.holos.run`, `keycloak.holos.run`, and `security.holos.run` CRDs, then
   installs the manager config into the `holos-controller` namespace (the
   *Deploy and verify the controller* steps above). The deploy base targets, but
   does not create, that Namespace, and `scripts/apply` does **not** install the
   CRDs; `scripts/apply-projects` fails fast if a CRD it needs is absent.
4. **The manual Quay credential mint** — `scripts/apply-svc-quay-resource-controller-creds`
   plus the `platform-automation` org / OAuth-Application token, per the
   [credentials runbook](quay-resource-controller-credentials.md). This creates
   the `holos-controller-quay-creds` Secret the Organization's
   `credentialsSecretRef` resolves. (The **Keycloak** credential is provisioned at
   runtime in step 2 and needs no manual step.)
5. **`scripts/apply-projects`** — reads the local-ca PEM, renders the platform
   with it injected via the `ca_bundle_pem` CUE tag, and applies the central
   `Instance` + its `ReferenceGrant` (the `keycloak-instance` component,
   carrying the injected `caBundle`), then the `my-project` Namespace +
   Organization + the project's `keycloak.holos.run` CRs (the project client, the
   role/custodian `Group` CRs, the owner `User` CR, and the standing-owner
   `GroupMembership` CRs) and the rest of the component. It gates the
   Organization **and** the
   `keycloak.holos.run` resources (the `Instance`, the project
   `Client`, the role/custodian `Group` CRs, and the owner
   `User`) reaching `Ready`.

```bash
scripts/apply-projects
```

**TLS trust comes from the resource's `caBundle`, not the pod's system store.**
The in-cluster Quay (`quay.holos.internal`) serves a certificate signed by the
per-cluster mkcert local CA, which is **not** in the controller pod's system
trust store. The controller therefore establishes TLS to Quay by trusting the
**`spec.caBundle`** the `my-project` Organization carries (the standardized
cross-Kind field, [ADR-19](../adr/ADR-19.md) — appended to the system roots, not
replacing them) — `scripts/apply-projects` populates it with the local-ca PEM
at apply time. An Organization applied **without** a `caBundle` (e.g. by `kubectl
apply` of the committed manifest, which carries none) would fail to reach `Ready`
with an `x509: certificate signed by unknown authority` TLS error against Quay;
always provision `my-project` through `scripts/apply-projects` so the trust
anchor is injected.

### caBundle injection vs. the `projects` App-of-Apps (the OCI bootstrap)

The App-of-Apps OCI bootstrap (HOL-1373, [ADR-16 Rev 3](../adr/archive/ADR-16.md);
split per-project in HOL-1382, Rev 6) reconciles the project/application resources
— including each project's Quay `Organization` and each app's `Repository` — from
an OCI bundle: as of HOL-1382 each project has its **own** per-project
`<project>-control-plane` root over its **own** bundle
(`holos/<project>-config:dev`), applied by
`scripts/apply-project-app-of-apps <project>`. Every such bundle is built from the
**committed** `holos/deploy/` tree, which by guarantee carries **no** `caBundle`
material (it is per-cluster trust material, injected only at apply time, never
committed). So the two paths coexist deliberately:

- **`scripts/apply-projects` remains the path that provisions `caBundle`.** It
  reads the local-ca PEM and renders the Organization/Repository CRs with
  `spec.caBundle` injected (the `ca_bundle_pem` CUE tag), so the controller
  trusts the in-cluster Quay's mkcert serving cert. Run it on a local k3d cluster
  where Quay is served with the mkcert local CA.
- **The `projects` App-of-Apps reconciles the same CRs *without* a `caBundle`.**
  Because the bundle carries none, ArgoCD's copy has `spec.caBundle` empty — which
  means the controller falls back to the **pod's system trust store**. That is
  correct **only** where Quay's serving certificate already chains to a CA in the
  system store (a real cluster with a publicly-trusted or system-installed CA),
  **not** the mkcert local k3d cluster.

**Ordering guidance.** On the local k3d cluster, run `scripts/apply-projects`
**after** the App-of-Apps handoff (`scripts/apply-platform-app-of-apps` then
`scripts/apply-projects-app-of-apps`, which apply the platform root and each
project's `<project>-control-plane` root — split out of `scripts/apply` in
HOL-1379, further split per-project in HOL-1382) so the injected `caBundle` is the
last writer on the
Organization/Repository CRs —
`scripts/apply-projects` uses a distinct field manager and re-asserts `caBundle`,
and ArgoCD's `selfHeal` does not strip a field the committed source never sets
(server-side apply field ownership). Where the system trust store already covers
Quay's cert, the `projects` App-of-Apps alone suffices and `scripts/apply-projects`
is unnecessary for trust (it is still the path that injects per-cluster material).
The **no-committed-Secret / no-committed-`caBundle`** guarantee holds either way:
the bundle never contains trust material.

## See also

- [ADR-18 — The Holos Controller](../adr/ADR-18.md) — the controller, its
  `holos-controller` namespace, and the API-group dependency boundary.
- [ADR-19 — Quay API Group (`quay.holos.run`) CRDs](../adr/ADR-19.md) — the
  Organization/Repository schemas, the `credentialsSecretRef` design, the
  `url`/`urlSecretRef` webhook, the repos-only-via-Repository rule, and the
  conditions/reasons.
- [Quay Resource Controller credentials runbook](quay-resource-controller-credentials.md)
  — minting the superuser OAuth-Application token this controller consumes.
- [Quay↔Keycloak OIDC runbook](quay-keycloak-oidc.md) — the SSO/superuser model.
- [`holos/docs/secret-handling.md`](../../holos/docs/secret-handling.md) — the
  runtime-secret guardrail the credential Secret follows.
