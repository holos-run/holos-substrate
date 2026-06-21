# holos/ — Deployment Configuration and Policy

The Holos CUE configuration that renders this platform's Kubernetes
manifests using the [Holos](https://holos.run/) rendered-manifests pattern.
This directory is isolated from the Go code per
[ADR-12](../docs/adr/ADR-12.md).

To add or change a component, read
[docs/component-guidelines.md](docs/component-guidelines.md). Components
whose namespaces carry workloads must follow the ambient mesh enrollment
convention in [docs/mesh-enrollment.md](docs/mesh-enrollment.md).
Out-of-scope concerns with a planned home are stubbed in
[docs/placeholders.md](docs/placeholders.md).

## Directory layout

```text
holos/
├── cue.mod/         # CUE module: schemas vendored from holos and k8s APIs
├── platform/        # the Platform spec: registered clusters and components
├── components/      # one directory per component (BuildPlan definitions)
├── deploy/          # rendered manifests, committed: clusters/<cluster>/components/<name>/
└── docs/            # operational guidelines for this directory
```

- **`platform/platform.cue`** registers clusters and components. Every
  cluster in the `clusters` struct gets every registered component,
  parameterized by the `clusterName` tag.
- **`components/<name>/`** holds each component's `buildplan.cue` and
  boilerplate. See the
  [component guidelines](docs/component-guidelines.md#component-directory-anatomy)
  for the anatomy.
- **`deploy/`** is generated output — never edit it by hand. Render with
  `holos render platform` from this directory and commit the result; the
  tree must be diff-clean on re-render. `scripts/render` (from the repo
  root) checks exactly that: it removes `deploy/`, re-renders, and fails
  if anything under `holos/` is modified, deleted, or untracked — catching
  stale edits and orphaned manifests alike.

## Clusters: local development now, production later

The only registered cluster is **`k3d-holos`**, the local development
cluster — [docs/local-cluster.md](../docs/local-cluster.md) is the
quick-start guide for creating it and applying the platform to it. The MVP
demo target is a single Apple Silicon Mac ([ADR-7](../docs/adr/ADR-7.md)).

A production deployment area is planned but not yet established: production
clusters will be registered alongside `k3d-holos` in
`platform/platform.cue`, and each registered cluster renders its own
`deploy/clusters/<cluster>/` tree. See
[docs/placeholders.md](docs/placeholders.md#production-deployment-area).

## How rendered manifests reach the cluster

During bootstrap there is no gitops controller in the cluster yet, so
rendered manifests are applied directly with server-side apply.
`scripts/apply` (from the repo root) applies every platform component — the
Layer 0 foundation and the Layer 1 services — to the
current kubectl context in the correct order:

```bash
scripts/apply
```

This section is the canonical explanation of *why* the apply order is what
it is and the caveats that come with force-applying. For the step-by-step path
from nothing to a running platform — DNS setup, cluster creation, trusted
TLS, then this apply step — follow the quick-start guide,
[docs/local-cluster.md](../docs/local-cluster.md).

The script is idempotent: server-side apply and `kubectl wait` both
converge, so re-running it against a fresh, partially applied, or fully
applied cluster is safe. As a guard against force-applying to the wrong
cluster, it refuses to run when the current context is not `k3d-holos`
unless `KUBE_CONTEXT` is set explicitly, and pins every kubectl call to
the resolved context. Per component it runs

```bash
kubectl apply --server-side --force-conflicts -f holos/deploy/clusters/k3d-holos/components/<name>/
```

and waits only on the critical dependencies between components — CRD
establishment, the istiod rollout, the ambient data-plane DaemonSets, the
cert-manager webhook rollout, the CNPG operator rollout, the Postgres
`Cluster` Ready conditions, and the Keycloak operator rollout — plus waits
on the `echo` Deployment, the `Keycloak` CR Ready and realm import Done
conditions, the `keycloak-config` realm-reconciliation Job, the `quay`
Deployment rollout, and the Argo CD workload rollouts as smoke checks;
nothing else.

Apply order matters beyond "CRD components first". The script applies the
platform components in this order — everything through `echo` is the
Layer 0 cluster foundation; everything from `cnpg-crds` on is a Layer 1
platform service:

1. `namespaces` — every platform Namespace, from the central registry
   ([namespaces.cue](namespaces.cue)); labeled `namespaces: "true"` so apply
   tooling can select it
2. `gateway-api` — Gateway API standard channel CRDs (`crds: "true"`)
3. `cert-manager-crds` — cert-manager CRDs (`crds: "true"`)
4. `istio-base` — Istio CRDs and validation webhook (`crds: "true"`)
5. `istiod` — the Istio control plane
6. `istio-cni` — the node agent that redirects ambient pod traffic to ztunnel
7. `istio-ztunnel` — the ambient node proxy
8. `cert-manager` — the certificate controller, webhook, and cainjector
9. `local-ca` — the CA `ClusterIssuer` that signs all platform certificates
10. `istio-gateway` — the shared Gateway all platform services attach
    `HTTPRoute`s to, and its wildcard TLS certificate
11. `echo` — the permanent smoke-test workload and its `HTTPRoute`
12. `cnpg-crds` — CloudNativePG CRDs (`crds: "true"`), filtered out of the
    single upstream release manifest
13. `cnpg` — the CloudNativePG operator, the platform's single Postgres
    operator
14. `cnpg-clusters` — the per-service Postgres `Cluster` resources
    (`keycloak-db`, `quay-db`), each in its consuming service's namespace
15. `keycloak-operator-crds` — Keycloak operator CRDs (`crds: "true"`),
    fetched as the two separate upstream single-CRD manifests
16. `keycloak-operator` — the Keycloak operator, in the `keycloak`
    namespace (deliberately not ambient-enrolled, see
    [namespaces.cue](namespaces.cue))
17. `keycloak` — the Keycloak server instance: the `Keycloak` CR backed by
    the `keycloak-db` Postgres `Cluster`, its TLS `Certificate`, the
    declarative `holos` realm import, the `HTTPRoute` attaching it to the
    shared Gateway at `auth.holos.localhost`, and the `DestinationRule`
    that re-encrypts the Gateway→Keycloak hop
18. `keycloak-config` — the realm-reconciliation `Job` that converges the
    `holos` realm declaratively on every apply: the
    [keycloak-config-cli](https://github.com/adorsys/keycloak-config-cli)
    `Job` (in the `keycloak` namespace) plus the `ConfigMap` carrying its
    import document. It layers the platform's realm roles
    (`platform-owner`, `platform-editor`, `platform-viewer`), the
    `authenticated` default group, and the public PKCE `argocd` OIDC client
    onto the realm shell the `keycloak` phase bootstraps. See
    [keycloak-config: realm reconciliation](#keycloak-config-realm-reconciliation)
19. `quay` — the Quay registry: the Quay `Deployment` backed by the
    `quay-db` Postgres `Cluster` and a minimal `quay-redis` Deployment,
    with blob storage on a local-path PVC and the `HTTPRoute` pair
    attaching it to the shared Gateway at `quay.holos.localhost`
20. `argocd-crds` — the Argo CD CRDs (`crds: "true"`): `applications`,
    `applicationsets`, and `appprojects` in group `argoproj.io`
21. `argocd` — the Argo CD core install: the application-controller
    `StatefulSet`, the repo-server, server, and redis `Deployment`s, the
    `HTTPRoute` pair attaching the UI to the shared Gateway at
    `argocd.holos.localhost`, and the `ServiceEntry` that resolves the
    Keycloak issuer hostname `auth.holos.localhost` in-cluster for the
    OIDC backchannel (see
    [Argo CD admin credentials and verification](#argo-cd-admin-credentials-and-verification))

The order encodes six rules: the `namespaces` component applies first, so
every Namespace exists before any component that populates it;
CRD components (labeled `crds: "true"`) apply before the controllers that
depend on their types; `istiod` applies before
the Gateway, because the `istio` GatewayClass must exist and istiod must be
running to program the Gateway; `istio-cni` and `istio-ztunnel` apply before
ambient-enrolled workloads like `cert-manager`, `echo`, and `cnpg`, because
they must be capturing traffic when those workloads start (the Gateway
itself is deliberately not enrolled, see
[docs/mesh-enrollment.md](docs/mesh-enrollment.md)); components with
fail-closed admission webhooks apply — and their Deployments are waited on —
before the components that create the resources they admit: `cert-manager`
before the `cert-manager.io` resources (`local-ca`'s `ClusterIssuer`,
`istio-gateway`'s `Certificate`), with a retry on the transient x509
admission error while cainjector injects the webhook's CA bundle, and the
`cnpg` operator before the `postgresql.cnpg.io` `Cluster` resources the
`cnpg-clusters` component creates — with the same shape of retry, because
the operator's webhook may briefly reject admission after its rollout is
Available; and the Gateway applies before components that attach
routes to it. `cnpg-crds` and `cnpg` trail `echo` because CNPG depends only
on its own CRDs (and, being ambient-enrolled, the data plane), so appending
them keeps the established order stable. `cnpg-clusters` trails `cnpg` and
is gated on each `Cluster`'s `Ready` condition because the Keycloak phase
applies a Keycloak CR that needs a reachable database.
`keycloak-operator-crds` and `keycloak-operator` trail `cnpg-clusters`: the
Keycloak operator depends only on its own CRDs and the `keycloak`
namespace, and the `keycloak` component applies `Keycloak` and
`KeycloakRealmImport` CRs that need both the operator reconciling — hence
the gate on its Deployment rollout — and the `keycloak-db` `Cluster`
reachable, so appending the pair after the database keeps the dependency
chain linear. `keycloak` trails the operator: its CRs need everything
above, and it creates a `cert-manager.io` `Certificate`, so its apply
retries through the same transient webhook admission window as `local-ca`
and `istio-gateway`. Its gate waits on the `Keycloak` CR Ready condition
and then on the `holos` `KeycloakRealmImport` Done condition as the Layer 1
smoke check, so a bootstrap cannot report success while the realm import
Job is still running or has failed — the first start pulls the server
image and runs the database schema migrations, so each wait gets a more
generous timeout (`KEYCLOAK_TIMEOUT`, default 600s) than the rollout
gates. `keycloak-config` trails `keycloak`: its keycloak-config-cli `Job`
reconciles the `holos` realm against the live admin API, so the Keycloak
server must be Ready and the realm shell imported (both gated by
`wait_keycloak`) before it runs. A completed `Job`'s pod template is
immutable and `kubectl apply` never re-runs an existing Complete `Job`, so
a `pre_keycloak_config` hook deletes every `keycloak-config` `Job` (by the
`app.kubernetes.io/name` label, `--cascade=foreground`) before the apply —
the apply then always creates a fresh `Job` that re-runs the CLI, and
because keycloak-config-cli converges idempotently, re-running on every
apply is exactly the intended "reconcile on every apply" behavior. The
`wait_keycloak_config` gate then polls that `Job` to completion (the
`wait_quay` Job-poll pattern, so a failure names the `Job` rather than a
generic timeout), reading its content-hashed name from the rendered
manifest so it waits on exactly the `Job` the apply just created. `quay`
trails `keycloak-config` because it needs the `quay-db` `Cluster`
reachable — already gated Ready in the `cnpg-clusters` step — and its gate
waits on the secret-keys bootstrap Job and then on the `quay` Deployment
rollout with its own generous timeout (`QUAY_TIMEOUT`, default 900s),
since the first pull of the Quay image is large and the first start runs
Quay's database schema migrations. `argocd-crds` and `argocd` continue the
sequence: the CRDs apply (and are gated Established) before the
controllers that need the types, and Argo CD depends only on the Gateway
its `HTTPRoute`s attach to — nothing downstream depends on it during
bootstrap — so appending the pair keeps the established order stable. The
`argocd` gate waits on the rollout of exactly the workloads the chart
renders with pods — the redis, repo-server, and server `Deployment`s and
the application-controller `StatefulSet` — as the Argo CD smoke check
(the applicationset-controller `Deployment` renders with `replicas: 0`,
and dex and notifications are disabled and render no workloads). Argo CD
closes the apply sequence: nothing downstream depends on it during
bootstrap, so it is appended last to keep the established order stable.

The first rule exists because nothing orders an apply batch by kind:
kubectl submits the files sequentially in lexical order, so a single
server-side apply that carries a Namespace alongside its namespaced
resources fails with `NotFound` on the first apply whenever a namespaced
resource sorts ahead of its Namespace. The last rule is for verifiability
rather than correctness — route attachment is level-triggered, so an
`HTTPRoute` applied early simply reports unattached until the Gateway
exists — but applying `echo` after the Gateway means the smoke test
exercises a complete traffic path immediately. Certificate issuance is
level-triggered the same way: the Gateway's HTTPS listener reports an
unresolved certificate ref only until cert-manager writes the wildcard
certificate's Secret.

`--force-conflicts` is safe here because the rendered manifests in git are
the source of truth for these resources and, with the exceptions below, no
other controller manages their fields during bootstrap; do not copy it into
contexts where another field manager owns the resources.

cert-manager's cainjector manages `webhooks[].clientConfig.caBundle` on the
rendered cert-manager webhook configurations at runtime. Unlike the Istio
exception below, the field is absent from the rendered manifests, so a
re-apply with `--force-conflicts` never claims or strips it — no enforcement
gap results. The CNPG operator manages the `caBundle` on its own webhook
configurations the same way, and the field is likewise absent from the
rendered `cnpg` manifests.

The other exception is Istio's webhook reconciliation: the rendered
`ValidatingWebhookConfiguration`s (`istiod-default-validator` in
`istio-base`, `istio-validator-istio-system` in `istiod`) set
`failurePolicy: Ignore`, and istiod patches the field to `Fail` once it is
ready to serve admission requests. Re-applying either component with
`--force-conflicts` seizes the field back and downgrades it to `Ignore`
until istiod re-patches it — expect that transient enforcement gap (and the
resulting field-manager churn) on every re-apply of those two components,
including every re-run of `scripts/apply`.

Argo CD itself is now installed by `scripts/apply` (the `argocd-crds` and
`argocd` components). Whole-platform ArgoCD-based delivery has not yet
replaced the direct apply: every component still renders with
`argoAppDisabled: true`, so the `userDefinedBuildPlan` per-component gitops
projection emits no `Application` resources until that projection is enabled
(see [docs/placeholders.md](docs/placeholders.md#argocd-gitops-delivery)).
That deferred projection emits a **git**-source Application per component
(`repoURL: https://github.com/holos-run/holos-paas`, `targetRevision: main`,
`path: holos/deploy/...`) and is distinct from the **hand-authored** sample
`Application`s the Kargo delivery pipelines own — `echo` (the spike) and
`my-project` (see [The `my-project` delivery scaffold](#the-my-project-delivery-scaffold))
— which carry an **OCI** source pointing at a rendered-manifests artifact in
the in-cluster Quay registry and which Argo CD *does* reconcile once that
artifact is published. The OCI `Application` source pattern the hand-authored
Applications use is decided, verified, and documented in
[docs/argocd-application-source.md](docs/argocd-application-source.md).

### Keycloak admin credentials and verification

The Keycloak operator bootstraps the initial admin user itself and stores
the generated credentials in the `keycloak-initial-admin` Secret (keys
`username` and `password`) on first reconcile — no credentials are
committed to this repository. Retrieve them:

```bash
kubectl -n keycloak get secret keycloak-initial-admin -o json \
  | jq '.data | map_values(@base64d)'
```

Verify Keycloak on the live cluster after `scripts/apply`:

```bash
kubectl -n keycloak wait keycloak/keycloak --for=condition=Ready --timeout=600s
curl -fsSI https://auth.holos.localhost/        # trusted chain via the mkcert root
curl -fs https://auth.holos.localhost/realms/holos/.well-known/openid-configuration | jq .issuer
# log in to https://auth.holos.localhost/admin/ with the credentials above
```

State lives in the `keycloak-db` Postgres `Cluster`, not the pod: deleting
the Keycloak pod (`kubectl -n keycloak delete pod -l
app.kubernetes.io/managed-by=keycloak-operator`) loses nothing — after the
operator restarts it, the `holos` realm and admin login still work.

The `KeycloakRealmImport` CR is bootstrap-only — the operator's import Job
skips when the realm already exists, so post-bootstrap realm changes are
not reconciled from the CR. That gap is closed by the `keycloak-config`
component (below): the realm shell still bootstraps from the CR on a clean
cluster, and `keycloak-config` then layers and keeps converged the managed
objects (roles, the default group, the OIDC clients) the platform owns.

#### keycloak-config: realm reconciliation

The `keycloak-config` component
([components/keycloak/realm-config/buildplan.cue](components/keycloak/realm-config/buildplan.cue))
reconciles the `holos` realm declaratively on **every** `scripts/apply` with
an idempotent [keycloak-config-cli](https://github.com/adorsys/keycloak-config-cli)
`Job` (in the `keycloak` namespace). The CR's bootstrap-only import cannot
carry post-bootstrap changes; this Job runs adorsys/keycloak-config-cli
against the live admin API and converges the realm on every run, so editing
the import document and re-applying is the supported way to evolve the
realm. What it reconciles:

- the three platform realm **roles** — `platform-owner`, `platform-editor`,
  `platform-viewer`;
- the `authenticated` **default group**, registered as a realm default so
  every realm user is bound to it on creation (the baseline Argo CD
  read-access subject);
- the public PKCE **`argocd` OIDC client** (`publicClient: true`, no secret,
  `pkce.code.challenge.method: S256`, the `argocd.holos.localhost` callback
  redirect URIs), with two protocol mappers that both write a `groups`
  claim: a group-membership mapper (bare names, e.g. `authenticated`) and a
  realm-role mapper (e.g. `platform-owner`), so a single `groups` claim
  carries both group and role membership for Argo CD RBAC to key on.
- the confidential **`quay` OIDC client** (`publicClient: false`, client-secret
  auth **without** PKCE — `pkce.code.challenge.method` is the empty/"none" method,
  HOL-1317 disabled PKCE because Quay 3.17.3 replays a stale `code_verifier` after
  logout; the three explicit `quay.holos.localhost/oauth2/keycloak/callback`
  redirect URIs) and its `platform-admin` /
  `project-admin` **client roles**,
  with mappers that write group memberships, the `quay` client-role names, the
  `platform-owner` **realm role** (a realm-role mapper added in HOL-1245,
  mirroring the `argocd` client), and `preferred_username` into the token — the
  SSO login Quay relies on, designed in [ADR-15](../docs/adr/ADR-15.md). The
  client secret is the `quay-oidc` Secret, generated once into both the
  `keycloak` and `quay` namespaces and substituted into the import document at
  run time (never committed).

The declarative-client pattern itself — public vs confidential clients (the
public `argocd`/`kargo` clients use PKCE S256; the confidential `quay` client is
the no-PKCE exception, HOL-1317), the secret bootstrap, the three mappers
that feed the shared `groups` claim, the role model, and the guardrail
checklist for adding another client — is
documented in
[docs/keycloak-clients.md](docs/keycloak-clients.md).

The import document is authored in CUE and marshalled to JSON in a
`ConfigMap` the Job mounts at `/config/holos.json`; it carries
`realm: "holos"` only (no `enabled` or identity-provider fields), so it
layers onto the realm shell the `KeycloakRealmImport` CR bootstraps without
contending with it. keycloak-config-cli's default managed-import behavior is
no-delete, so realm objects the Job does not declare are left untouched
(full-realm purge is deliberately not enabled).

**Idempotency and the apply gate.** A completed `Job`'s pod template is
immutable and `kubectl apply` never re-runs an existing Complete `Job`, so
the `Job`'s `metadata.name` carries an 8-char content hash of the import
document and image, and `scripts/apply`'s `pre_keycloak_config` hook deletes
every `keycloak-config` `Job` (by the `app.kubernetes.io/name=keycloak-config`
label, `--cascade=foreground` so the dependent CLI pod is gone too) before
the apply. The apply then always creates a fresh `Job` that re-runs the CLI;
because the CLI converges idempotently, re-running on every apply is the
intended behavior. The `wait_keycloak_config` gate polls that `Job` to
completion — resolving its hashed name from the rendered manifest so it
waits on exactly the `Job` just applied — and trails `wait_keycloak` (the
realm shell must exist first). It sits between `keycloak` and `quay` in the
apply order above.

### Quay credentials and data-plane provisioning

Quay runs `AUTHENTICATION_TYPE: OIDC` with the Keycloak `holos` realm as the
**sole identity store** (ADR-15 Revision 4, HOL-1293): there is no local `admin`
user, and the headless `/api/v1/user/initialize` bootstrap endpoint is
unavailable. The two Quay superusers are Keycloak realm users listed in
`SUPER_USERS` (by `preferred_username`) — the service account
**`svc-quay-resource-controller`** (the shipped Holos Controller's Quay machine
identity, distinguished by its `svc-` prefix) and the human **`quay-admin`** —
both seeded by the keycloak phase (HOL-1294) with passwords generated once at
runtime into Secrets of the same name in the `keycloak` namespace (key
`password`), never committed.

In-cluster Quay data-plane provisioning splits in two. The **organizations,
repositories, and `repo_push` webhooks** are reconciled by the shipped Holos
Controller ([ADR-18](../docs/adr/ADR-18.md)) from the `quay.holos.run`
Organization/Repository CRDs ([ADR-19](../docs/adr/ADR-19.md), `Implemented`); the
**robot accounts** and the Argo CD/Kargo pull-credential Secrets are not modeled
by those CRDs (ADR-19 *Out of scope*) and stay manual. The removed Database-backend
bootstrap (the `quay-admin-bootstrap` Job, the `quay-initial-admin` superuser
token, and the `scripts/quay-init`/`scripts/quay-reset` helpers) no longer exists.
The controller **consumes** a superuser OAuth-Application credential an operator
mints by hand; see the
[Quay Resource Controller credentials runbook](../docs/runbooks/quay-resource-controller-credentials.md).

### Quay OIDC SSO and roles

Quay runs `AUTHENTICATION_TYPE: OIDC` — the Keycloak `holos` realm is the
**sole identity store** (ADR-15 Revision 4, HOL-1293): users log in with the
**Holos SSO** button through the Authorization Code flow **without** PKCE
(disabled in HOL-1317, ADR-15 Revision 7), authenticated by the confidential
client's secret. There is no local `admin` user and no
`/api/v1/user/initialize` bootstrap endpoint under OIDC. The full design — the
OIDC backend, the no-PKCE decision, the confidential client, the
username-from-token behavior, and the roles model — is in
[ADR-15](../docs/adr/ADR-15.md), and the operational companion (wiring, secret
rotation, and the `code exchange: 400` troubleshooting) is the
[Quay↔Keycloak OIDC runbook](../docs/runbooks/quay-keycloak-oidc.md). The
essentials:

- **Login flow.** Quay's `KEYCLOAK_LOGIN_CONFIG`
  ([components/quay/buildplan.cue](components/quay/buildplan.cue)) points at
  the realm's confidential `quay` client, authenticated by its client secret
  without PKCE (`USE_PKCE: false`, no `PKCE_METHOD`; disabled in HOL-1317 to work
  around Quay 3.17.3's logout-state defect), reconciled in `keycloak-config`
  above. The local username/password form is removed
  (`FEATURE_DIRECT_LOGIN: false`).
- **Username and namespace.** The username is taken verbatim from the ID
  token's `preferred_username` claim with no prompt to confirm or edit it
  (`FEATURE_USERNAME_CONFIRMATION: false`); first login auto-provisions
  (`FEATURE_USER_CREATION: true`) the user's personal namespace
  (`quay.holos.localhost/<preferred_username>/...`), which is their per-user
  organization scope and cannot be renamed.
- **Roles → teams.** The `quay` client roles `platform-admin` and
  `project-admin` (and per-project roles by the same convention) are folded
  into the `groups` claim alongside Keycloak group memberships. The `quay`
  client also emits the `platform-owner` **realm role** into that same claim
  (the realm-role mapper added in HOL-1245, mirroring the `argocd` client), so
  the privileged platform-owner role is recognizable to Quay's team sync the
  same way group names are. They are identity labels, not privileges in
  themselves: automatic group/role-name → team syncing is **enabled** under the
  OIDC backend (`FEATURE_TEAM_SYNCING: true` with `TEAM_RESYNC_STALE_TIME: 30m` —
  the OIDC user handler syncs the `groups` claim into Quay teams on the
  30-minute cadence; re-enabled in HOL-1293), so **team membership tracks the
  claim** and must not be edited directly in Quay (sync would overwrite it). A
  Quay **superuser** does the one-time wiring — binding a team to a group/role
  name and setting that team's repository permissions — and the team's
  permissions are what grant access. The full declarative-client pattern and the
  role model are in [docs/keycloak-clients.md](docs/keycloak-clients.md).
- **Superusers.** Superuser status comes solely from `SUPER_USERS` in the
  config (by `preferred_username`), not from the `groups` claim and not from
  the `platform-admin` role. The two superusers are the Keycloak realm users
  `svc-quay-resource-controller` (service account) and `quay-admin` (human);
  there is no local `admin` account under the OIDC backend.

### Quay verification

Two checks prove the registry behaviors the platform depends on; re-run them
after any Quay change. Both assume the registry is reachable and the
`holos/sample` org/repo exists. The `holos/sample` org/repo is **not** one the
Holos Controller reconciles (no Organization/Repository CR targets it), so create
it by hand first — sign in via "Holos SSO" as `svc-quay-resource-controller` or
`quay-admin` and push to `holos/sample` per
[Verify Quay](../docs/local-cluster.md#verify-quay), using a superuser
OAuth-Application token minted per the
[Quay Resource Controller credentials runbook](../docs/runbooks/quay-resource-controller-credentials.md).

**Push webhook.** A `repo_push` webhook notification fires on image push.
Verify it against a temporary in-cluster echo endpoint (the
`mendhak/http-https-echo` image is multi-arch and logs every request body
to stdout):

```bash
kubectl -n quay run quay-echo --image=mendhak/http-https-echo:37 --port=8080 \
  --labels=app.kubernetes.io/name=quay-echo
kubectl -n quay expose pod quay-echo --port=8080
kubectl -n quay wait pod/quay-echo --for=condition=Ready --timeout=120s

# The Quay API takes a superuser OAuth token (basic auth is not accepted).
# Under the OIDC backend there is no headless quay-initial-admin token; mint a
# superuser OAuth-Application token by hand per the Quay Resource Controller
# credentials runbook (../docs/runbooks/quay-resource-controller-credentials.md)
# and export it as TOKEN before running this block.
: "${TOKEN:?export a superuser OAuth-Application token as TOKEN first (see the runbook above)}"
UUID=$(curl -fsS -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -X POST https://quay.holos.localhost/api/v1/repository/holos/sample/notification/ \
  -d '{"event": "repo_push", "method": "webhook",
       "config": {"url": "http://quay-echo.quay.svc:8080/"},
       "eventConfig": {}, "title": "verify-webhook"}' | jq -er '.uuid // empty')
# Fire the built-in test first. The ${UUID:?} expansion aborts this command —
# even when the block is pasted into an interactive shell — if the create
# above failed, instead of POSTing to .../notification//test:
curl -fsS -o /dev/null -H "Authorization: Bearer $TOKEN" -X POST \
  "https://quay.holos.localhost/api/v1/repository/holos/sample/notification/${UUID:?notification create failed}/test"

# Then a real push (docker login per docs/local-cluster.md "Verify Quay"):
docker pull busybox && docker tag busybox quay.holos.localhost/holos/sample:test2
docker push quay.holos.localhost/holos/sample:test2

kubectl -n quay logs quay-echo
```

The echo logs must show one POST per event whose JSON body carries
`repository`, `namespace`, `name`, `docker_url`, and `updated_tags`.
Deliveries to cluster-internal
plain-HTTP URLs work out of the box; no allowlist configuration is
required. Failures can be silent (Quay queues deliveries through Redis),
so on trouble check the notification's failure counter
(`GET .../notification/` → `number_of_failures`) and the Quay pod logs.
Clean up when done:

```bash
curl -fsS -H "Authorization: Bearer $TOKEN" \
  -X DELETE "https://quay.holos.localhost/api/v1/repository/holos/sample/notification/${UUID:?}"
kubectl -n quay delete pod/quay-echo svc/quay-echo
```

**Restart resilience.** Registry state lives in the `quay-db` Postgres
`Cluster` (metadata, including notification configs) and the
`quay-datastorage` PVC (blobs) — not the pods. Delete both pods and
confirm nothing is lost:

```bash
kubectl -n quay delete pod -l app.kubernetes.io/name=quay
kubectl -n quay delete pod -l cnpg.io/cluster=quay-db
kubectl -n quay rollout status deployment/quay --timeout=600s
kubectl -n quay wait cluster/quay-db --for=condition=Ready --timeout=300s
```

After recovery: `docker login` with the robot credentials still works, the
previously pushed tag is still pullable
(`docker rmi quay.holos.localhost/holos/sample:test` then
`docker pull quay.holos.localhost/holos/sample:test`), and any webhook
notification configured above is still listed via the API.

Sizing note: Quay's gunicorn pools enforce per-pool minimums that override
the `WORKER_COUNT_*` pins unless `WORKER_COUNT_UNSUPPORTED_MINIMUM` is
also set — without it the registry pool runs 8 workers and the container
OOMKills against its memory limit. See the env comment in
[components/quay/buildplan.cue](components/quay/buildplan.cue).

### Argo CD admin credentials and verification

The Argo CD UI is served at `https://argocd.holos.localhost` through the
shared Gateway, which terminates TLS with the wildcard certificate — the
server itself runs with `server.insecure: "true"` and a plain-HTTP backend,
like the other routed services. Real users authenticate via **Keycloak
SSO** (OIDC/PKCE, below); the chart's built-in `admin` account is kept
enabled as deliberate local break-glass access. The server bootstraps the
initial `admin` user itself on first startup and stores the generated
password in the `argocd-initial-admin-secret` Secret (`argocd` namespace,
key `password`) — no credentials are committed to this repository,
mirroring the Keycloak `keycloak-initial-admin` pattern. The Secret appears
only after the first server start, so never gate on it ahead of the
rollout. Retrieve the password:

```bash
kubectl -n argocd get secret argocd-initial-admin-secret \
  -o jsonpath='{.data.password}' | base64 -d; echo
```

Verify Argo CD on the live cluster after `scripts/apply` — wait on exactly
the workloads the chart renders with pods (the applicationset-controller
Deployment renders with `replicas: 0`, and dex and notifications are
disabled and render no workloads):

```bash
kubectl -n argocd wait deployment argocd-redis argocd-repo-server argocd-server \
  --for=condition=Available --timeout=300s
kubectl -n argocd rollout status statefulset/argocd-application-controller --timeout=300s
curl -fsSI https://argocd.holos.localhost/   # trusted chain via the mkcert root
# log in to https://argocd.holos.localhost/ as admin with the password above
```

The `argocd` namespace is ambient-enrolled (`_ambient: true` in
[namespaces.cue](namespaces.cue), following the reference platform);
enrolled pods report protocol `HBONE` in
`istioctl ztunnel-config workloads` — see
[docs/mesh-enrollment.md](docs/mesh-enrollment.md). Argo CD reconciles
nothing yet: no `Application` resources are emitted until the gitops
Application projection is enabled (see
[docs/placeholders.md](docs/placeholders.md#argocd-gitops-delivery)).

**Keycloak SSO (OIDC/PKCE).** Argo CD authenticates real users against the
Keycloak `holos` realm using the Authorization Code flow with PKCE (S256),
configured in `argocd-cm` `oidc.config`: `issuer:
https://auth.holos.localhost/realms/holos`, `clientID: argocd`,
`enablePKCEAuthentication: true`, and **no** client secret (the public
`argocd` client provisioned by `keycloak-config`). `argocd-rbac-cm`
`policy.csv` maps the `groups` claim — which carries both Keycloak group
names and realm-role names — to Argo CD roles:

| Keycloak group/role | Argo CD role |
| --- | --- |
| `platform-owner` | `role:admin` |
| `platform-editor` | `role:readonly` (Argo CD has no native editor role) |
| `platform-viewer` | `role:readonly` |
| `authenticated` (default group) | `role:readonly` (baseline for any realm user) |

with `policy.default: ""` (no implicit access) and `scopes: "[groups]"`.
The `argocd-server` OIDC **backchannel** (discovery/JWKS/token) must reach
the issuer in-cluster: the component ships a `ServiceEntry` that makes
`auth.holos.localhost` resolve to the shared Istio ingress gateway, so the
backchannel re-enters through the same Gateway→Keycloak path browsers use
and the `iss` claim matches the configured issuer. The backchannel sets
`oidc.tls.insecure.skip.verify: "true"` to accept the per-machine
mkcert/local-CA backend cert — a local-only MVP posture (the mkcert root
cannot be embedded at render time); production replaces it with `rootCA`
trust (see the production deployment area placeholder in
[docs/placeholders.md](docs/placeholders.md#production-deployment-area)).

To verify SSO end to end: create a user in the `holos` realm
(`https://auth.holos.localhost/admin/`), grant the `platform-owner` realm
role, open `https://argocd.holos.localhost`, click **LOG IN VIA Keycloak**,
complete the login, and confirm you land as an admin; a `platform-viewer`
user lands read-only. Check `kubectl -n argocd logs deploy/argocd-server`
shows no OIDC discovery/JWKS or x509 errors. The step-by-step walkthrough
is in
[docs/local-cluster.md](../docs/local-cluster.md#verify-argo-cd).

### Verify an OCI-source Application

The MVP delivery path syncs `Application` resources from rendered-manifests
OCI artifacts in the in-cluster Quay registry —
[docs/argocd-application-source.md](docs/argocd-application-source.md) is
the pattern's contract (artifact layout, credential Secret shape, how the
repo-server reaches Quay, tag-vs-digest guidance). The procedure below
proves the path end to end with a throwaway artifact and Application;
re-run it after any change to the argocd or quay components, or to the
`quay-holos-localhost` ServiceEntry. It assumes a push-capable robot
credential and the `holos/sample` org/repo exist (see
[Quay credentials and data-plane provisioning](#quay-credentials-and-data-plane-provisioning);
the robot and `holos/sample` org/repo are provisioned by hand) and the
[`oras`](https://oras.land/) CLI is installed. Nothing here is committed:
the artifact is pushed imperatively, so a committed Application would
leave a fresh bootstrap perpetually Degraded (see the
[pattern doc](docs/argocd-application-source.md#what-stays-imperative)).

Package a trivial manifest as the single-layer artifact Argo CD expects
and push it with the robot credentials:

```bash
WORK=$(mktemp -d)
mkdir -p "${WORK}/manifests"
cat > "${WORK}/manifests/configmap.yaml" <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: argocd-smoke
  namespace: echo
data:
  purpose: OCI-source smoke test
YAML
tar -czf "${WORK}/manifests.tar.gz" -C "${WORK}/manifests" .
ROBOT_TOKEN=$(kubectl -n quay get secret quay-robot-pull -o jsonpath='{.data.\.dockerconfigjson}' \
  | base64 -d | jq -r '.auths["quay.holos.localhost"].auth' | base64 -d | cut -d: -f2-)
(cd "${WORK}" && oras push --username 'holos+robot' --password-stdin \
  quay.holos.localhost/holos/argocd-smoke:v1 \
  manifests.tar.gz:application/vnd.oci.image.layer.v1.tar+gzip <<<"${ROBOT_TOKEN:?}")
```

Register the repository with Argo CD and create the test Application. The
`${ROBOT_TOKEN:?}` expansion aborts the paste if the extraction above
failed; `insecure: "true"` is required because the local mkcert CA is not
in the repo-server's trust store (see the
[pattern doc](docs/argocd-application-source.md#repository-credential-secret)):

```bash
kubectl apply --server-side -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: quay-argocd-smoke
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repository
stringData:
  name: argocd-smoke
  url: oci://quay.holos.localhost/holos/argocd-smoke
  type: oci
  username: holos+robot
  password: "${ROBOT_TOKEN:?}"
  insecure: "true"
EOF
kubectl apply --server-side -f - <<'EOF'
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: argocd-smoke
  namespace: argocd
  finalizers:
    - resources-finalizer.argocd.argoproj.io
spec:
  project: default
  source:
    repoURL: oci://quay.holos.localhost/holos/argocd-smoke
    targetRevision: v1
    path: .
  destination:
    server: https://kubernetes.default.svc
    namespace: echo
  syncPolicy:
    automated:
      prune: true
EOF
```

Wait for the sync and confirm the manifest landed. `Application`s are
ordinary namespaced objects — the plain `kubectl get` is the same access
path Kargo's promotion controller uses to patch `targetRevision`:

```bash
kubectl -n argocd wait application/argocd-smoke \
  --for=jsonpath='{.status.sync.status}'=Synced --timeout=120s
kubectl -n argocd wait application/argocd-smoke \
  --for=jsonpath='{.status.health.status}'=Healthy --timeout=120s
kubectl get applications.argoproj.io -n argocd
kubectl -n echo get configmap argocd-smoke
```

Exercise the rollout path Kargo's promotion controller uses — push a
changed artifact, resolve its immutable digest, and patch
`targetRevision` (prefer digests over tags for controller-driven updates;
see the
[pattern doc](docs/argocd-application-source.md#tag-vs-digest-in-targetrevision)):

```bash
cat > "${WORK}/manifests/configmap.yaml" <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: argocd-smoke
  namespace: echo
data:
  purpose: OCI-source smoke test
  version: v2
YAML
tar -czf "${WORK}/manifests.tar.gz" -C "${WORK}/manifests" .
(cd "${WORK}" && oras push --username 'holos+robot' --password-stdin \
  quay.holos.localhost/holos/argocd-smoke:v2 \
  manifests.tar.gz:application/vnd.oci.image.layer.v1.tar+gzip <<<"${ROBOT_TOKEN:?}")
DIGEST=$(oras resolve --username 'holos+robot' --password-stdin \
  quay.holos.localhost/holos/argocd-smoke:v2 <<<"${ROBOT_TOKEN:?}")
kubectl -n argocd patch application argocd-smoke --type merge \
  -p "{\"spec\":{\"source\":{\"targetRevision\":\"${DIGEST:?}\"}}}"
# Two waits: the revision wait alone races the apply — sync.revision
# updates when the controller *compares* against the new digest, before
# the automated sync has written resources, so gate on Synced too.
kubectl -n argocd wait application/argocd-smoke \
  --for=jsonpath="{.status.sync.revision}"="${DIGEST:?}" --timeout=120s
kubectl -n argocd wait application/argocd-smoke \
  --for=jsonpath='{.status.sync.status}'=Synced --timeout=120s
kubectl -n echo get configmap argocd-smoke -o jsonpath='{.data.version}'  # v2
```

Clean up — the finalizer cascades the delete, so Argo CD prunes the
synced ConfigMap before the Application disappears. The
`holos/argocd-smoke` repository stays in the registry (the robot
credential cannot delete repositories), like `holos/sample` from the
[Quay verification](#quay-verification); a re-run converges on it:

```bash
kubectl -n argocd delete application argocd-smoke --timeout=120s
kubectl -n echo wait --for=delete configmap/argocd-smoke --timeout=30s   # prune confirmed
kubectl -n argocd delete secret quay-argocd-smoke
rm -rf "${WORK:?}"
```

### Postgres credentials and connection contract

The `cnpg-clusters` component provisions one Postgres `Cluster` per
consuming service, in that service's namespace. CNPG generates the
credentials and connection endpoints with conventional names — this is the
contract the Keycloak and Quay components consume:

| Cluster       | Namespace  | Credentials Secret | Read-write Service                 |
| ------------- | ---------- | ------------------ | ---------------------------------- |
| `keycloak-db` | `keycloak` | `keycloak-db-app`  | `keycloak-db-rw.keycloak.svc:5432` |
| `quay-db`     | `quay`     | `quay-db-app`      | `quay-db-rw.quay.svc:5432`         |

Each `<cluster>-app` Secret carries the keys `username`, `password`,
`dbname`, `host`, `port`, `uri`, and `jdbc-uri`.

Verify the databases on the live cluster after `scripts/apply`:

```bash
kubectl get cluster -A                       # both: Cluster in healthy state
kubectl -n keycloak get secret keycloak-db-app
kubectl -n quay get secret quay-db-app
KC_POD=$(kubectl -n keycloak get pod \
  -l cnpg.io/cluster=keycloak-db,cnpg.io/instanceRole=primary -o name)
QUAY_POD=$(kubectl -n quay get pod \
  -l cnpg.io/cluster=quay-db,cnpg.io/instanceRole=primary -o name)
kubectl -n keycloak exec "${KC_POD:?no keycloak-db primary pod}" -- \
  psql -U postgres -c 'SELECT 1'
kubectl -n quay exec "${QUAY_POD:?no quay-db primary pod}" -- \
  psql -U postgres -c 'SELECT 1'
```

To exercise the same path the consuming service uses — the `-rw` Service
with the `-app` credentials — run a short-lived client pod with the `uri`
key from the Secret:

```bash
URI=$(kubectl -n keycloak get secret keycloak-db-app -o jsonpath='{.data.uri}' | base64 -d)
kubectl -n keycloak run psql-verify --rm -i --restart=Never \
  --image=ghcr.io/cloudnative-pg/postgresql:18.1 --env="URI=$URI" -- \
  psql "$URI" -c 'SELECT current_user, current_database()'
```

### Deployment: Kargo + client-side ORAS publish

The platform's deployment path is owned by **Kargo** plus a client-side
**ORAS publish workflow** ([ADR-16](../docs/adr/ADR-16.md)): rendered
manifests are packaged and pushed to the in-cluster Quay registry as OCI
artifacts (see
[docs/oci-publish-workflow.md](docs/oci-publish-workflow.md)), and Kargo
promotes them through its Project/Warehouse/Stage resources.

> **Retired:** The earlier NATS event-driven pipeline — the `nats`
> JetStream backbone, the `webhook-receiver`, and the `webhook-subscriber`
> components, together with their Go code, the pipeline protobuf, the
> `wss://nats.holos.localhost` debug endpoint, and the `scripts/nats-webhooks`
> reader — was removed in HOL-1241. Nothing else used NATS, so it is gone
> entirely. ADR-9/10/11/14 that described that pipeline are now
> `Deprecated`. The contract documentation that lived here (the `WEBHOOKS`/
> `TASKS` streams, the `webhooks.>`/`tasks.deploy` subject hierarchy, the
> DeployTask schema, and the receiver/subscriber service contracts) was
> removed with it.

### The `my-project` delivery scaffold

`my-project` is the Layer 3 sample-application delivery scaffold (HOL-1268). As
of HOL-1357 it is no longer a bespoke component: it is a one-line project
registration ([`projects/my-project.cue`](projects/my-project.cue)) plus a
one-line app registration ([`apps/my-app.cue`](apps/my-app.cue)), rendered by the
collection-driven [`components/project/`](components/project/buildplan.cue) and
[`components/application/`](components/application/buildplan.cue) components — the
generalization of the formerly hand-authored scaffold (the bespoke
`components/my-project` was deleted). It lays down everything one project needs to
receive Kargo-driven OCI delivery ([ADR-16](../docs/adr/ADR-16.md)) and is the
template for a future self-service `ProjectRequest` (below). How to register
your own project and app — the `owners` map, the app `project`/`image`/`port`
fields, the env-prefixed namespace model and the bare-`<name>` control
namespace, the primitive-role → Quay-team and → app-client binding, and the
`scripts/apply-projects` workflow — is the authoring guide
[docs/project-and-application-templates.md](docs/project-and-application-templates.md)
([ADR-21](../docs/adr/ADR-21.md), `Implemented`). Its rendered resources are:

- a **Namespace** (`my-project`) — registered centrally in
  [`namespaces.cue`](namespaces.cue), **not** emitted by the component (per
  [component-guidelines.md](docs/component-guidelines.md#namespaces-are-registered-centrally)),
  and carrying the `kargo.akuity.io/project: "true"` adoption label and
  `kargo.akuity.io/keep-namespace: "true"` annotation so Kargo adopts it. This
  one namespace doubles as both the Kargo Project namespace and the workload
  namespace — unlike the echo spike, which splits them across
  `components/kargo-project-echo/` and `components/kargo-echo/`.
- an **Argo CD `AppProject` + `Application`** (in `argocd`) — the AppProject
  scopes `sourceRepos` to `oci://quay.holos.localhost/my-project/*` and
  destinations to the `my-project` namespace; the Application has an OCI source
  (`oci://quay.holos.localhost/my-project/my-project-config`) and the
  `kargo.akuity.io/authorized-stage: my-project:project-config` annotation
  authorizing the Stage to patch its `targetRevision`. The Application is
  hand-authored (not the deferred `argoAppDisabled` projection — see
  [docs/placeholders.md](docs/placeholders.md#argocd-gitops-delivery)) and its
  `targetRevision` is deliberately omitted so Kargo owns that field.
- the **Kargo control plane**: a `Project` (the namespace boundary), a
  `ProjectConfig` (auto-promotion policy plus a native Quay webhook receiver
  and its receiver Secret), a `Warehouse` that watches the `my-project-config`
  OCI artifact, and the `project-config` `Stage` whose `argocd-update` step
  patches the Application's source to each discovered Freight digest.
- a **`quay.holos.run/v1alpha1` `Organization`** (`my-project`, in the
  `my-project` namespace) — emitted as of HOL-1322 with `spec.adopt: false`,
  `spec.credentialsSecretRef.name: holos-controller-quay-creds`, and a gated
  `spec.caBundle` carrying the per-cluster local-ca trust anchor. The shipped
  Holos Controller ([ADR-18](../docs/adr/ADR-18.md)/[ADR-19](../docs/adr/ADR-19.md))
  reconciles it into the in-cluster Quay org, trusting Quay's mkcert-signed
  serving cert via that `caBundle` rather than the controller pod's system trust
  store. The `caBundle` is injected at apply time (see *the separate apply step*
  below), so the committed `holos/deploy/` manifest carries **no** CA material.
- the **Kargo webhook receiver token bootstrap**: a `Job`
  (`my-project-quay-webhook-bootstrap`, in the `my-project` namespace) that
  generates the Kargo receiver Secret's shared token.
- a **`quay.holos.run/v1alpha1` `Repository`** per app — emitted by the
  **Application component** (`my-app-config`, within the `my-project` org, with a
  gated `caBundle`) and reconciled by the Holos Controller. (The bespoke
  component, by contrast, emitted only the Organization; the Repository CR now
  ships per app as of HOL-1356.) What remains **manual** for the Quay-side data
  plane: the app `Repository`'s `repo_push` webhook **registration** (omitted in
  the current phase — the Warehouse polls the config repo as the fallback — until
  the Kargo receiver URL is published into a referenceable Secret), the push
  robot, and the Argo CD pull-robot/repository Secret in `argocd` and the Kargo
  image-credential Secret (the robots/pull Secrets are out of scope for the
  `v1alpha1` CRDs, ADR-19). The earlier `my-project-quay-bootstrap` Job that
  provisioned all of it (authenticating with the removed `quay-initial-admin`
  admin token) no longer exists; only the Kargo-side receiver token Job above
  remains (it needs no Quay admin token). A push will not trigger Freight
  discovery until the robot/webhook are provisioned by hand.

**The separate apply step — `scripts/apply-projects`.** As of HOL-1322,
`my-project` is **deliberately removed from the master `scripts/apply`** and is
applied by the dedicated [`scripts/apply-projects`](../scripts/apply-projects)
instead. That script reads the local-ca PEM (the `cert-manager/local-ca` Secret,
or `$(mkcert -CAROOT)/rootCA.pem`), renders the platform with it injected via the
`ca_bundle_pem` CUE tag (the `scripts/publish` `--inject` pattern), and applies
the `my-project` Namespace + the rendered component (Organization, Argo CD
AppProject/Application, the Kargo control plane, and the webhook-token bootstrap)
with `kubectl apply --server-side`. It runs **after** `scripts/local-ca` and
**after** the manual Quay superuser-credential setup
(`scripts/apply-svc-quay-resource-controller-creds` plus the `platform-automation`
org / OAuth token per the runbooks) — the Argo CD and Kargo CRDs and controllers
and the Holos Controller must be established first, and the Organization's
`credentialsSecretRef` resolves the `holos-controller-quay-creds` Secret. The
script deletes the prior `my-project-quay-webhook-bootstrap` Job so each run
re-generates the token idempotently, and gates that Job's completion, the Kargo
Project's adoption of the `my-project` namespace, and the Organization reaching
Ready. The Application stays `Unknown`/`Missing` until the first
`my-project-config` artifact is published **and** the project's config Quay repo
(`my-project-config`, **not** modeled by a `Repository` CR — only the app's
`my-app-config` repo is, emitted by the Application component) plus the push robot
and pull-credential Secrets are provisioned by hand — expected scaffolding.

**Toward self-service `ProjectRequest`.** Everything above is what a future
self-service `ProjectRequest` API would generate per tenant: a namespace, a
scoped Argo CD AppProject/Application, a Kargo Project/ProjectConfig/Warehouse/
Stage, and the Quay org/repo/webhook/robot bootstrap. `my-project` is the
hand-authored reference instance that proves the shape end-to-end before that
generator exists.

**Verifying the scaffold, and the end-to-end contract it will satisfy.** The
scaffold (Kargo pipeline, Application, the app's emitted Quay `Repository` CR) is
verifiable today; the app `Repository`'s `repo_push` **webhook registration** is
**omitted** in the current phase (the `Warehouse` polls the config repo as the
fallback) until the Kargo receiver URL is published into a referenceable Secret;
the publish step that drives the full push → webhook → Warehouse Freight →
Stage promotion → Application sync loop is **future work**, because a clean
sync needs a **project-scoped** `my-project-config` artifact and `scripts/publish`
today produces only the whole-platform render, which the constrained
`my-project` AppProject cannot sync. The verification commands and that scope
boundary are documented in
[oci-publish-workflow.md → the `my-project` delivery scaffold](docs/oci-publish-workflow.md#downstream-the-my-project-delivery-scaffold).
