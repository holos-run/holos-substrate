# Research: Quay Operators vs. the Current Hand-Authored Quay Approach

Research date: 2026-06-16.

**Base commit.** This report was researched against `main` at commit
[`d2a85ff`](https://github.com/holos-run/holos-paas/commit/d2a85ff7735b4b044452f522db5df539d0113397)
(`d2a85ff7735b4b044452f522db5df539d0113397`). Every deep link into this
repository below is pinned to that commit so the referenced lines stay stable
as the tree evolves. Upstream Quay Operator links are pinned to the
`quay/quay-operator` `master` branch (and named release branches where noted).
Links into the `herve4m/quay-api-operator` are pinned to its `main` tip
[`0badd68`](https://github.com/herve4m/quay-api-operator/commit/0badd68bc75aebb68e0f023bf754806fe85b6223)
(`0badd68bc75aebb68e0f023bf754806fe85b6223`). All external links were verified
against GitHub / Ansible Galaxy on the research date.

This report fulfills [HOL-1278](https://linear.app/holos-run/issue/HOL-1278/research-quay-operator)
(the upstream **Quay Operator**, layer 1) and
[HOL-1279](https://linear.app/holos-run/issue/HOL-1279/research-quay-api-operator)
(the **`herve4m/quay-api-operator`**, layer 2), evaluated against the same six
goals and acceptance criteria.

## Question

holos-paas currently deploys and manages [Quay](https://www.projectquay.io/)
with hand-authored CUE manifests plus create-if-absent bootstrap Jobs, no
operator. Two follow-up questions are judged against the same six goals:

- **HOL-1278:** should we move to the upstream
  [Quay Operator](https://github.com/quay/quay-operator) (the `QuayRegistry`
  CRD), or keep the current approach?
- **HOL-1279:** should we use the
  [`herve4m/quay-api-operator`](https://github.com/herve4m/quay-api-operator) to
  manage **organizations and repositories** (and the other registry *content*)?

These two operators occupy **different layers** and are not alternatives to each
other — that distinction is the spine of this report (see §4–§5):

- The **Quay Operator** is a **control-plane / layer-1** operator: it deploys and
  operates the Quay *registry infrastructure* (pods + backing services).
- The **`herve4m/quay-api-operator`** is a **data-plane / layer-2** operator: it
  manages the *content inside* a running Quay (orgs, repos, robots,
  notifications) by driving Quay's REST API from custom resources. It does **not**
  install Quay.

The six goals:

1. A credential a Quay controller/operator can use to provision organizations
   and repositories.
2. A way to configure a Repository to send a notification webhook to a Kargo
   `Warehouse` **when a user creates a repository via the Kubernetes API and a
   custom resource representing their Repository**.
3. No significant refactor when moving from the MVP demo to production — no
   complete rewrite of how Quay is managed.
4. A slimmed-down Quay suitable for a laptop (as today, with the local-path
   provisioner backing the PVC).
5. Should support CNPG for the database; may optionally self-manage Postgres if
   the operator does.
6. If an operator is used, it must be high quality, well maintained, and robust,
   and support the features above.

## TL;DR — recommendation

**Keep the current hand-authored approach for the MVP. Adopt neither operator
now.** Two findings, one per layer:

> **Layer 1 (Quay Operator).** The `QuayRegistry` CRD manages only the
> **registry infrastructure** (the Quay pods and their backing services —
> Postgres, Redis, object storage, TLS, routing, Clair). It has **no** concept of
> organizations, repositories, robot accounts, or repository notification
> webhooks. Its `ComponentKind` constants are exactly `quay`, `postgres`,
> `clair`, `clairpostgres`, `redis`, `horizontalpodautoscaler`, `objectstorage`,
> `route`, `mirror`, `monitoring`, `tls` and nothing else
> ([`apis/quay/v1/quayregistry_types.go`](https://github.com/quay/quay-operator/blob/master/apis/quay/v1/quayregistry_types.go)).

> **Layer 2 (`herve4m/quay-api-operator`).** This operator *does* cover exactly
> the data-plane gap the Quay Operator leaves open — it exposes `Organization`,
> `Repository`, `Robot`, `Team`, `Notification`, `FirstUser`, `ApiToken` (and 17
> more) as Kubernetes custom resources that drive Quay's REST API. Its
> `Notification` CR supports `event: repo_push` + `method: webhook`, which is
> precisely Goal 2. **But** the operator *wrapper* does not yet clear Goal 6's
> "high quality, well maintained, robust" bar: it is a single-maintainer personal
> project — 2 stars, 0 forks, 14 commits, **no GitHub release tags** (though
> versioned operator and OLM-bundle images *are* published to quay.io, §3), a CRD
> API group on a personal domain (`quay.herve4m.github.io`)
> ([repo](https://github.com/herve4m/quay-api-operator)). Its *underlying*
> automation — the `infra.quay_configuration` Ansible collection, formerly
> `herve4m.quay`, now governed by [redhat-cop](https://github.com/redhat-cop/quay_configuration)
> — **is** mature; the immaturity is the thin operator-SDK Ansible wrapper around it.

The data-plane work HOL-1278 identified as "the actual answer" (a
`Repository`/`ProjectRequest` CRD + reconciler that calls Quay's REST API, the
direction [ADR-2](../adr/ADR-2.md) sets) is therefore now *partly available off
the shelf*. The `herve4m/quay-api-operator` is the **closest existing
realization of that direction** and the best **reference / prototype** for it —
but it is too immature to adopt as the production control plane today, and its
generic per-object CRUD CRDs are a different abstraction than the single-intent
`ProjectRequest` (one CR → org + repo + robot + webhook + Keycloak group) that
ADR-2 implies. Adoption is a deferred decision with explicit re-evaluation
triggers, recorded at the end of this report.

## 1. The current approach (hand-authored manifests + bootstrap Jobs)

Quay is deployed as a plain `apps/v1` `Deployment` rendered from CUE, with no
operator and no `QuayRegistry` CRD. A repository-wide grep for `QuayRegistry` /
`quay-operator` returns nothing; the buildplan itself notes "Quay has no
operator to seed a first user"
([`quay/buildplan.cue#L507-L511`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/holos/components/quay/buildplan.cue#L507-L511)).

| Concern | Current implementation | Deep link (pinned to `d2a85ff`) |
|---|---|---|
| Image / version | `quay.io/projectquay/quay:3.17.3`, multi-arch (incl. arm64 for Apple-silicon k3d) | [`quay/buildplan.cue#L36-L45`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/holos/components/quay/buildplan.cue#L36-L45) |
| `config.yaml` template | Rendered constant, substituted at init time | [`quay/buildplan.cue#L236-L280`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/holos/components/quay/buildplan.cue#L236-L280) |
| Storage (blobs) | `LocalStorage` → 5Gi `ReadWriteOnce` PVC, storageClass omitted so it binds k3s `local-path` | [`quay/buildplan.cue#L250-L255`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/holos/components/quay/buildplan.cue#L250-L255), [`#L860-L878`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/holos/components/quay/buildplan.cue#L860-L878) |
| Database | CNPG `Cluster` `quay-db`, 1 instance, local-path PVC; app reads the CNPG-generated `quay-db-app` Secret URI | [`cnpg-clusters/buildplan.cue#L36-L90`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/holos/components/cnpg-clusters/buildplan.cue#L36-L90) |
| Redis | Single ephemeral Deployment, in-cluster only | [`quay/buildplan.cue#L739-L823`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/holos/components/quay/buildplan.cue#L739-L823) |
| Encryption keys | `quay-secret-keys` Secret (`SECRET_KEY`, `DATABASE_SECRET_KEY`), create-if-absent Job | [`quay/buildplan.cue#L311-L345`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/holos/components/quay/buildplan.cue#L311-L345) |
| Admin/superuser credential | `quay-admin-bootstrap` Job POSTs the one-shot `/api/v1/user/initialize` and stores a non-expiring superuser OAuth token in the `quay-initial-admin` Secret (key `token`) | [`quay/buildplan.cue#L507-L617`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/holos/components/quay/buildplan.cue#L507-L617) |
| OIDC (Keycloak SSO) | Confidential client, no PKCE; `quay-oidc` client secret bootstrapped into both namespaces | [`quay/buildplan.cue#L262-L273`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/holos/components/quay/buildplan.cue#L262-L273), [ADR-15](../adr/ADR-15.md) |

### How orgs / repos / robots / webhooks are provisioned today

This is the part to compare carefully against **both** operators, because this is
what the **Quay Operator does not do** and what the **`herve4m/quay-api-operator`
does**. All provisioning is done today through Quay's REST API, authenticating
with the `quay-initial-admin` token:

- `scripts/quay-init` bootstraps the shared `holos` org, a push robot, the
  `creators` team, and an in-cluster pull Secret — `/api/v1/user/initialize`
  ([`scripts/quay-init#L237`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/scripts/quay-init#L237)),
  `/api/v1/organization/`
  ([`#L271-L283`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/scripts/quay-init#L271-L283)),
  robots
  ([`#L297-L310`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/scripts/quay-init#L297-L310)),
  team membership
  ([`#L332-L364`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/scripts/quay-init#L332-L364)).
- The **`my-project` delivery scaffold** is the reference instance and the
  template for the future self-service `ProjectRequest`. Its bootstrap Job
  creates the org/repo/robot, grants pull permission, and **registers the
  `repo_push` notification webhook pointing at the Kargo receiver URL** — all
  create-if-absent, all via the REST API:
  - org/repo/robot/permission:
    [`my-project/buildplan.cue#L553-L593`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/holos/components/my-project/buildplan.cue#L553-L593)
  - reads the Kargo receiver URL from `ProjectConfig.status` then `POST`s the
    `repo_push` webhook to `/api/v1/repository/{repo}/notification/`:
    [`#L649-L685`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/holos/components/my-project/buildplan.cue#L649-L685)
  - the Kargo `ProjectConfig` with the matching `webhookReceivers[].quay`:
    [`#L314-L333`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/holos/components/my-project/buildplan.cue#L314-L333)

This webhook → `Warehouse` wiring (Quay `repo_push` → Kargo receiver →
`Freight` → `Stage` promotion) is the mechanism Goal 2 asks for. It is built
entirely on Quay's REST API and Kargo's
[Quay webhook receiver](https://docs.kargo.io/user-guide/reference-docs/webhook-receivers/quay/),
and is **independent of how the Quay registry is deployed**. The
`herve4m/quay-api-operator` (§3) is a candidate to replace these imperative Jobs
with declarative custom resources.

## 2. The Quay Operator (layer 1): what it is, and what it manages

The upstream operator ([`quay/quay-operator`](https://github.com/quay/quay-operator),
Apache-2.0) reconciles a single `QuayRegistry` custom resource. Its
`spec.components[]` list lets you set each backing component `managed: true`
(the operator owns its lifecycle and writes the corresponding `config.yaml`
values) or `managed: false` (you provide it externally and supply the relevant
`config.yaml` keys via the referenced config-bundle Secret)
([`docs/components.md`](https://github.com/quay/quay-operator/blob/master/docs/components.md)).

The complete component set (verified from the API type's `ComponentKind`
constants — [`quayregistry_types.go`](https://github.com/quay/quay-operator/blob/master/apis/quay/v1/quayregistry_types.go)
and the [`QuayRegistry` CRD](https://github.com/quay/quay-operator/blob/master/config/crd/bases/quay.redhat.com_quayregistries.yaml)):

`quay`, `postgres`, `clair`, `clairpostgres`, `redis`, `horizontalpodautoscaler`,
`objectstorage`, `route`, `mirror`, `monitoring`, `tls`.

**There is no `organization`, `repository`, `robot`, or `notification`
component**, and no struct field or status referencing registry content. The
CRD's `spec` is just `configBundleSecret` + `components[]`; its scope is
"deploy and operate the registry," not "manage what is inside the registry."

### Maintenance / quality (Goal 6)

The operator is high quality and actively maintained — it is the same operator
Red Hat ships for Red Hat Quay:

- Release branches are current: `redhat-3.18`, `redhat-3.14`, `redhat-3.12`
  (and older) were all updated in **June 2026**
  ([branches](https://github.com/quay/quay-operator/branches)). (The GitHub
  *Releases* tab looks stale — last tagged `v3.7.10` in 2022 — because releases
  ship as version *branches* and registry images, not GitHub Release tags; the
  branch activity is the accurate signal.)
- Apache-2.0, written in Go with controller-runtime, backed by Red Hat's Quay
  team.

So Goal 6's "high quality, well maintained, robust" bar **is met** by this
operator in the abstract. The problem is not the operator's quality — it is that
its scope does not cover Goals 1–2, and its design center (OpenShift) does not
match our target (Goal 4).

### Running on non-OpenShift / a laptop (Goal 4)

The operator is designed for OpenShift first. Its capability detection forces
the components that depend on OpenShift-only APIs to `unmanaged` when those APIs
are absent — on vanilla Kubernetes (our k3d laptop target) that set is
`route`, `tls`, `objectstorage`, and `monitoring`. Every other component has no
capability check and still defaults to managed — `quay` (always managed),
`postgres`, `redis`, `clair`, `clairpostgres`, `mirror`, and
`horizontalpodautoscaler`:

- **`objectstorage`** (managed) consumes the `ObjectBucketClaim` API, normally
  provided by NooBaa / OpenShift Data Foundation — not present on k3d. On a
  laptop you set `objectstorage: managed: false` and point Quay at an external
  S3-compatible service (e.g. MinIO) via the config bundle. This is **not** a
  drop-in for today's local-path PVC: the operator's managed Quay Deployment
  does not mount a blob-storage PVC at `/datastorage/registry` and the
  `QuayRegistry` CRD exposes no arbitrary volume mounts for the Quay pod, so the
  current `LocalStorage`-on-PVC pattern is not reproducible under the operator —
  the realistic supported path is external object storage.
- **`route`** (managed) creates an OpenShift `Route`; on k8s you terminate at
  your own Ingress/Gateway — as we do at the Istio Gateway today — so `route`
  (and the coupled `tls`) go `unmanaged`.
- **`monitoring`** (managed) wires into OpenShift's Prometheus stack →
  `unmanaged` off-OpenShift.

Net: on a laptop the operator runs `quay` + `postgres` + `redis` + `clair` +
`clairpostgres` + `mirror` + `hpa` managed but `route` + `tls` + `objectstorage`
+ `monitoring` unmanaged — and it
still expects OLM. You can do it, but the OpenShift-coupled "batteries" are
switched off, blob storage must move off the local PVC to an external S3
service, and you add OLM + the `QuayRegistry` reconciler for the privilege.

### Database (Goal 5)

The operator's `postgres: managed: true` deploys **its own** single Postgres
Deployment — *not* CNPG. To keep CNPG you set `postgres: managed: false` and
supply `DB_URI` (plus manually enabling the `pg_trgm` extension) in the config
bundle — the
[external/unmanaged-database path](https://docs.projectquay.io/deploy_red_hat_quay_operator.html).
That `DB_URI` would point at the *same* CNPG `quay-db` cluster the repo already
runs. So for Goal 5 the operator adds nothing: CNPG is `unmanaged` Postgres
either way, and the current approach already wires exactly that
([`cnpg-clusters/buildplan.cue#L36-L90`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/holos/components/cnpg-clusters/buildplan.cue#L36-L90)).

## 3. The `herve4m/quay-api-operator` (layer 2): the data-plane operator

This is the operator HOL-1279 asks about, and it is **a different kind of thing**
than the Quay Operator. It does **not** deploy Quay — its own description is
"manage Quay Container Registry deployments by using Kubernetes resources …
without installing Quay" ([repo](https://github.com/herve4m/quay-api-operator)).
It is the layer-2 / data-plane operator that the §2 operator is not: it turns the
REST-API provisioning the repo does imperatively today (§1) into reconciled
custom resources.

### What it is, technically

- An **operator-SDK Ansible operator**: the runtime is
  `quay.io/operator-framework/ansible-operator:v1.42.2`, and each custom resource
  kind maps to an Ansible role that the operator runs on every reconcile
  ([`Dockerfile`](https://github.com/herve4m/quay-api-operator/blob/0badd68bc75aebb68e0f023bf754806fe85b6223/Dockerfile),
  [`watches.yaml`](https://github.com/herve4m/quay-api-operator/blob/0badd68bc75aebb68e0f023bf754806fe85b6223/watches.yaml)).
- The roles wrap the **`infra.quay_configuration`** Ansible collection (formerly
  `herve4m.quay`) — see
  [`requirements.yml`](https://github.com/herve4m/quay-api-operator/blob/0badd68bc75aebb68e0f023bf754806fe85b6223/requirements.yml).
  That collection's `quay_organization`, `quay_repository`, `quay_robot`,
  `quay_notification`, etc. modules are the actual REST-API drivers; the operator
  is a thin CRD-to-playbook shim over them.
- Installed with **plain Kustomize** (`make install` for the CRDs, `make deploy`
  for the controller) — there is **no `bundle/` directory checked in at the repo
  root**, so OLM is *optional*, not required (an OLM `make bundle` target exists
  and the resulting bundle is published as `quay.io/herve4m/quay-api-operator-bundle`,
  but plain Kustomize is the default path). This is a meaningful Goal-4 advantage
  over the §2 operator,
  which expects OLM.

### The custom resources (the API group `quay.herve4m.github.io/v1alpha1`)

24 CRDs are defined
([`config/crd/bases/`](https://github.com/herve4m/quay-api-operator/tree/0badd68bc75aebb68e0f023bf754806fe85b6223/config/crd/bases)).
The ones that matter for Goals 1–2:

| Kind | What it does | Relevance |
|---|---|---|
| `FirstUser` | POSTs the one-shot `/api/v1/user/initialize`, optionally creates a token, and writes `host`/`token`/`username`/`password` into a return Secret | **Goal 1** — the same bootstrap the `quay-admin-bootstrap` Job does today, as a CR |
| `Application` + `ApiToken` | Create an OAuth application and mint an access token; `ApiToken` writes `host`/`validateCerts`/`token`/`accessToken` into a return Secret usable by every other CR | **Goal 1** — declarative, longer-lived API credentials |
| `Organization` | Create/configure/delete an org (email, time-machine, quota) | **Goal 1** — org provisioning |
| `Repository` | Create/configure/delete a repo (visibility, description, per-team/user/robot `perms`, mirror state) | **Goal 1** — repo provisioning |
| `Robot` | Create a robot account and **write its pull credentials as a `.dockerconfigjson` Secret** (`retSecretRef`) | matches the `my-project` pull-robot pattern exactly |
| `Notification` | Create a repository notification | **Goal 2** — see below |
| `Team`, `Quota`, `ProxyCache`, `RepositoryMirror`, `Prune`, `Immutability`, … | The rest of Quay's content surface | breadth |

Every CR carries a **`connSecretRef`** pointing at a Secret with the Quay
connection parameters (`host`, `token` or `username`+`password`, `validateCerts`)
— so credentials live in Secrets, never in the CR spec, and a `FirstUser`/
`ApiToken` CR can *produce* the Secret that the `Organization`/`Repository`/
`Notification` CRs then *consume*. Each CR also has
`preserveInQuayOnDeletion` to decouple CR deletion from destroying Quay content
([`config/samples/`](https://github.com/herve4m/quay-api-operator/tree/0badd68bc75aebb68e0f023bf754806fe85b6223/config/samples)).

### Goal 2, verified: `repo_push` → webhook as a custom resource

The `Notification` CRD's schema enumerates `event` values including **`repo_push`**
and `method` values including **`webhook`**, with a `config.url` (POST target)
and `config.template` (the JSON body of the webhook POST)
([`config/crd/bases/quay.herve4m.github.io_notifications.yaml`](https://github.com/herve4m/quay-api-operator/blob/0badd68bc75aebb68e0f023bf754806fe85b6223/config/crd/bases/quay.herve4m.github.io_notifications.yaml)).
A `Notification` like the following is the declarative equivalent of the
imperative `repo_push` webhook the `my-project` Job POSTs today — exactly Goal 2,
as a Kubernetes custom resource:

```yaml
apiVersion: quay.herve4m.github.io/v1alpha1
kind: Notification
metadata:
  name: my-project-repo-push
spec:
  connSecretRef:
    name: quay-credentials-secret
  repository: my-project/app
  event: repo_push
  method: webhook
  config:
    url: https://<kargo-receiver-host>/<receiver-path>   # from ProjectConfig.status
```

(The sample shipped in-repo demonstrates `event: vulnerability_found` /
`method: slack`; `repo_push` + `webhook` are valid enum values per the CRD
schema —
[`config/samples/quay_v1alpha1_notification.yaml`](https://github.com/herve4m/quay-api-operator/blob/0badd68bc75aebb68e0f023bf754806fe85b6223/config/samples/quay_v1alpha1_notification.yaml).)

This is the single most important finding for HOL-1279: **a Kubernetes custom
resource that registers the Quay → Kargo `repo_push` webhook already exists off
the shelf.** What it does *not* yet do is read the Kargo receiver URL from
`ProjectConfig.status` for you — `config.url` is a literal, so the
URL-discovery glue the `my-project` Job performs (read `ProjectConfig.status`,
then POST) would still be holos-paas's responsibility, e.g. a small reconciler
or templating step that fills in `config.url`.

### Maintenance / quality (Goal 6) — the decisive caveat

The **operator wrapper** does **not** meet Goal 6's "high quality, well
maintained, robust" bar today:

- **Single-maintainer personal project.** 2 stars, 0 forks, 0 open issues/PRs,
  14 commits total, last commit 2026-04-29, and **no GitHub releases, tags, or
  changelog** ([repo](https://github.com/herve4m/quay-api-operator)). It *does*
  publish versioned images to quay.io — `quay.io/herve4m/quay-api-operator`
  (`1.0.0`–`1.3.0`, `latest`) and an OLM bundle
  `quay.io/herve4m/quay-api-operator-bundle` (`v1.2.0`, `v1.3.0`) — so it is
  installable without building your own image; but the version history lives only
  in registry tags, with no release notes or source tags to audit what changed.
- **CRD API group on a personal domain** (`quay.herve4m.github.io`) — a
  governance smell for a production control-plane dependency (compare the §2
  operator's `quay.redhat.com` group).
- **`v1alpha1`** API version — no stability guarantee; breaking schema changes
  are expected.
- **Ansible-operator runtime trade-offs:** every reconcile shells out to
  `ansible-runner` to run a playbook. That is heavier (CPU/memory per reconcile,
  slower convergence) and offers weaker typed-status/conditions guarantees than a
  Go controller-runtime operator. The default `reconcilePeriod` for token-issuing
  kinds is set extremely long (`87660h` ≈ 10 years) to avoid churn, which also
  means drift is not actively re-reconciled on a normal cadence.

The **underlying automation is a different story and is mature**: the
`infra.quay_configuration` collection (formerly `herve4m.quay`, same author) is
published on Ansible Galaxy and now developed under
[redhat-cop/quay_configuration](https://github.com/redhat-cop/quay_configuration)
(Red Hat Community of Practice), with a real changelog and broad module coverage.
So the *capability* the operator exposes is well-proven; the *operator wrapper
and its CRD API* are the immature, low-bus-factor part.

### Goals 3, 4, 5 for this operator

- **Goal 3 (no big refactor to production).** Adopting these CRDs would replace
  the imperative `scripts/quay-init` and `my-project` bootstrap Jobs with
  declarative CRs that are reusable verbatim MVP → production — *if* the operator
  endures. The countervailing risk is real: betting the control plane on a
  2-star, single-maintainer project with no source releases means a forced
  migration (the operator is abandoned, or its `v1alpha1` API breaks) would itself be the
  "significant refactor" Goal 3 forbids.
- **Goal 4 (laptop).** Neutral-to-positive: it does **not** touch how Quay is
  deployed, so the slim local-path-PVC Quay is unaffected. It adds one
  ansible-operator Deployment (a few hundred MB RAM) and needs no OLM (plain
  Kustomize), so it fits a laptop — lighter on prerequisites than the §2 operator.
- **Goal 5 (CNPG).** Irrelevant by construction — this operator never manages the
  database. CNPG stays exactly as today.

## 4. Evaluation against the six goals

Two operators, two layers — so two scorings. The current hand-authored approach
is the shared baseline.

### Layer 1 — current approach vs. the Quay Operator

| Goal | Current approach | Quay Operator | Verdict |
|---|---|---|---|
| **1.** Credential to provision orgs/repos | ✅ `quay-initial-admin` token (superuser OAuth) created headlessly via `/api/v1/user/initialize` | ➖ Out of scope — operator never creates such a credential | **No operator benefit** |
| **2.** Repository → Kargo `Warehouse` webhook via a K8s CR | ⚠️ Done today via REST API in the `my-project` Job | ➖ Out of scope — no repository/notification component in `QuayRegistry` | **No operator benefit** |
| **3.** No big refactor to production | ⚠️ Scale by editing CUE constants; HA/upgrades are our responsibility | ✅ Supported production path *on OpenShift* | **Operator wins — but only if production = OpenShift** |
| **4.** Slim, laptop-friendly | ✅ Single pod, local-path PVC, ephemeral Redis, 1-instance CNPG | ⚠️ Works only with `objectstorage`/`route`/`tls`/`monitoring` unmanaged + OLM overhead | **Current approach wins** |
| **5.** CNPG database | ✅ CNPG `quay-db`, app reads `quay-db-app` URI | ⚠️ CNPG only as `unmanaged` Postgres (`DB_URI`) | **Tie / current approach simpler** |
| **6.** Operator quality | n/a | ✅ Actively maintained, Apache-2.0, Red Hat-backed | **High quality — but moot given Goals 1–2 out of scope** |

### Layer 2 — current approach vs. the `herve4m/quay-api-operator`

| Goal | Current approach | `herve4m/quay-api-operator` | Verdict |
|---|---|---|---|
| **1.** Credential to provision orgs/repos | ✅ `quay-initial-admin` token via a bootstrap Job | ✅ `FirstUser` + `Application`/`ApiToken` CRs produce the credential Secret declaratively | **Operator matches and declarative-izes it** |
| **2.** Repository → Kargo `Warehouse` webhook via a K8s CR | ⚠️ Imperative REST POST in the `my-project` Job | ✅ `Notification` CR with `event: repo_push` + `method: webhook` + `config.url` — *as a CR* (but URL still needs filling from `ProjectConfig.status`) | **Operator wins on form** — this is the Goal-2 shape we want |
| **3.** No big refactor to production | ⚠️ Imperative Jobs, reusable but not declarative | ⚠️ Declarative + reusable **iff the operator endures**; abandonment/`v1alpha1` break = forced migration | **Promising but risky** |
| **4.** Slim, laptop-friendly | ✅ baseline | ✅ Adds one ansible-operator pod, no OLM, does not touch Quay deploy | **Compatible** |
| **5.** CNPG database | ✅ baseline | ➖ Out of scope (never manages the DB) | **No change** |
| **6.** Operator quality | n/a | ❌ Wrapper immature: 2★, single maintainer, no GitHub releases/tags/changelog (only quay.io image tags `1.0.0`–`1.3.0` + OLM bundle), personal CRD group, `v1alpha1`; ✅ underlying `infra.quay_configuration` collection (redhat-cop) is mature | **Fails Goal 6 as a production dependency today** |

## 5. The decisive finding, restated

The two questions resolve to **two layers** that should not be conflated:

1. **Registry control plane (layer 1)** — deploying/operating the Quay pods and
   backing services. *Both* the current manifests and the Quay Operator solve
   this; the operator is more production-grade **on OpenShift**, the hand-authored
   manifests are simpler **on a laptop/k3d**.
2. **Registry data plane (layer 2)** — orgs, repos, robots, notification webhooks.
   The Quay Operator does **not** touch this; the only interface is Quay's OAuth2
   REST API. HOL-1278 concluded "no community Kubernetes controller for Quay
   *content* is mature enough to depend on (search surfaced none)." **HOL-1279
   updates that conclusion:** such a controller *does* exist —
   `herve4m/quay-api-operator` — and it cleanly maps orgs/repos/robots/
   notifications (including `repo_push` → webhook) to custom resources. It just
   isn't mature enough *yet* (Goal 6) to be a production dependency, even though
   the `infra.quay_configuration` collection beneath it is.

Goals 1 and 2 — the reason both issues exist — live entirely in layer 2. The
holos-paas-authored `Repository`/`ProjectRequest` CRD + reconciler that
[ADR-2](../adr/ADR-2.md) foreshadows (and that the `my-project` scaffold
prototypes imperatively today) is still the destination. The new information is
that `herve4m/quay-api-operator` is the **closest existing implementation of that
destination** — usable as a reference, a prototype backend, or (later) a
dependency — rather than something that has to be built from zero.

## 6. Recommendation

1. **Keep the current hand-authored Quay deployment for the MVP.** It satisfies
   Goals 4 and 5 cleanly, provides the Goal-1 credential, and carries the Goal-2
   webhook wiring (in the `my-project` scaffold). Adopting the **Quay Operator**
   now advances none of Goals 1, 2, 4, or 5 and adds OLM + OpenShift-coupling on
   k3d (§2). **Do not adopt the Quay Operator now.**

2. **Do not adopt the `herve4m/quay-api-operator` as the production control plane
   now — but treat it as the reference design and a candidate backend for the
   layer-2 work.** It is the off-the-shelf realization of the
   `Repository`/`ProjectRequest` direction: `FirstUser`/`ApiToken` for Goal 1,
   `Organization`/`Repository`/`Robot` for provisioning, and a `Notification` CR
   that does Goal 2's `repo_push`-webhook **as a custom resource**. The blocker is
   Goal 6: a 2-star, single-maintainer wrapper with no source releases or
   changelog (only quay.io image tags) on a personal CRD API group is too thin a
   dependency for a production control plane, and its generic
   per-object CRUD CRDs are a lower-level abstraction than the single-intent
   `ProjectRequest` (one CR → org + repo + robot + webhook + Keycloak group) that
   ADR-2 implies. Concretely:
   - **Mine it now.** Adopt its *patterns* immediately at zero dependency cost —
     the `connSecretRef` model (credentials in Secrets, produced by one CR and
     consumed by others), the `Robot` → `.dockerconfigjson` Secret shape, and the
     `Notification` `repo_push`/`webhook` field set — when building the
     holos-paas `Repository`/`ProjectRequest` reconciler. The underlying
     `infra.quay_configuration` (redhat-cop) collection is a sound, mature
     reference for the exact REST calls.
   - **Optionally prototype with it.** It is a fast way to validate the
     CR-driven data-plane end-to-end on a laptop (no OLM, doesn't disturb the
     current Quay) before committing to a hand-written Go reconciler.

3. **Invest the next increment in a holos-paas `Repository` (and
   `ProjectRequest`) CRD + reconciler** that calls Quay's REST API to create the
   org/repo/robot and register the `repo_push` webhook against the project's
   Kargo `Warehouse` receiver URL — promoting the imperative `my-project`
   bootstrap Job into a declarative, reconciled controller. This delivers Goals 1
   and 2 as Kubernetes-native resources and keeps the "no significant refactor"
   promise of Goal 3 regardless of the layer-1 choice. Whether that reconciler is
   hand-written in Go or composes `herve4m/quay-api-operator`'s CRDs as an
   internal backend is an implementation decision to make at design time, weighing
   Goal 6 against build cost — but the holos-paas-owned, single-intent
   `ProjectRequest` API surface should be ours either way, per ADR-2.

4. **Re-evaluation triggers (record, do not act now):**
   - *Layer 1:* the production target becomes OpenShift (or a cluster with
     first-class `ObjectBucketClaim`/NooBaa, Route, monitoring) — then the **Quay
     Operator** becomes the better layer-1 choice; migration is a contained swap
     of the hand-authored Quay `Deployment`/Service/PVC/Secret manifests for a
     `QuayRegistry` CR (`postgres` unmanaged → existing CNPG `DB_URI`; reuse
     `quay-secret-keys` and `quay-oidc` via the config bundle).
   - *Layer 2:* the **`herve4m/quay-api-operator`** graduates — source releases
     and a changelog (it already publishes versioned images and an OLM bundle), a
     stable (`v1`) non-personal API group, adoption by redhat-cop or comparable
     governance, and broader usage. At that point
     adopting it (or backing the holos-paas reconciler with it) becomes a strong
     option that could retire bespoke reconciler code.

If/when either operator is adopted, capture the decision in a new revision of
[ADR-16](../adr/ADR-16.md) (Quay's role in delivery) or a dedicated ADR, per the
repo's "revise the existing ADR" convention.

## Sources

Repository (pinned to commit `d2a85ff7735b4b044452f522db5df539d0113397`):

- [`holos/components/quay/buildplan.cue`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/holos/components/quay/buildplan.cue)
- [`holos/components/cnpg-clusters/buildplan.cue`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/holos/components/cnpg-clusters/buildplan.cue)
- [`holos/components/my-project/buildplan.cue`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/holos/components/my-project/buildplan.cue)
- [`scripts/quay-init`](https://github.com/holos-run/holos-paas/blob/d2a85ff7735b4b044452f522db5df539d0113397/scripts/quay-init)
- [ADR-2](../adr/ADR-2.md), [ADR-15](../adr/ADR-15.md), [ADR-16](../adr/ADR-16.md)

Quay Operator — layer 1 (`quay/quay-operator`, `master` / release branches):

- [`apis/quay/v1/quayregistry_types.go`](https://github.com/quay/quay-operator/blob/master/apis/quay/v1/quayregistry_types.go) — `ComponentKind` constants
- [`config/crd/bases/quay.redhat.com_quayregistries.yaml`](https://github.com/quay/quay-operator/blob/master/config/crd/bases/quay.redhat.com_quayregistries.yaml) — the `QuayRegistry` CRD
- [`docs/components.md`](https://github.com/quay/quay-operator/blob/master/docs/components.md) — managed/unmanaged component model
- [Release branches](https://github.com/quay/quay-operator/branches) — maintenance evidence (June 2026)

`herve4m/quay-api-operator` — layer 2 (pinned to commit `0badd68bc75aebb68e0f023bf754806fe85b6223`):

- [repository root](https://github.com/herve4m/quay-api-operator) — description, maintenance signals (2★, 14 commits, no GitHub releases/tags; versioned images + OLM bundle published to `quay.io/herve4m`)
- [`config/crd/bases/`](https://github.com/herve4m/quay-api-operator/tree/0badd68bc75aebb68e0f023bf754806fe85b6223/config/crd/bases) — the 24 CRD definitions (`organizations`, `repositories`, `robots`, `teams`, `notifications`, `firstusers`, `apitokens`, …)
- [`config/crd/bases/quay.herve4m.github.io_notifications.yaml`](https://github.com/herve4m/quay-api-operator/blob/0badd68bc75aebb68e0f023bf754806fe85b6223/config/crd/bases/quay.herve4m.github.io_notifications.yaml) — `event` enum incl. `repo_push`, `method` enum incl. `webhook` (Goal 2)
- [`config/samples/`](https://github.com/herve4m/quay-api-operator/tree/0badd68bc75aebb68e0f023bf754806fe85b6223/config/samples) — sample CRs (`Notification`, `Organization`, `Repository`, `Robot`, `ApiToken`, `FirstUser`) showing the `connSecretRef` / `retSecretRef` model
- [`watches.yaml`](https://github.com/herve4m/quay-api-operator/blob/0badd68bc75aebb68e0f023bf754806fe85b6223/watches.yaml) — CRD-kind → Ansible-role mapping and finalizers
- [`Dockerfile`](https://github.com/herve4m/quay-api-operator/blob/0badd68bc75aebb68e0f023bf754806fe85b6223/Dockerfile) — `ansible-operator:v1.42.2` base
- [`requirements.yml`](https://github.com/herve4m/quay-api-operator/blob/0badd68bc75aebb68e0f023bf754806fe85b6223/requirements.yml) — depends on the `infra.quay_configuration` collection
- [User Guide](https://herve4m.github.io/quay-api-operator/) — project documentation site

Underlying Ansible collection (`infra.quay_configuration`, formerly `herve4m.quay`):

- [redhat-cop/quay_configuration](https://github.com/redhat-cop/quay_configuration) — Red Hat Community of Practice governance, changelog
- [`infra.quay_configuration` on Ansible Galaxy](https://galaxy.ansible.com/ui/repo/published/infra/quay_configuration/) — published collection, module docs

Upstream / vendor docs:

- [Deploying the Project Quay Operator](https://docs.projectquay.io/deploy_red_hat_quay_operator.html)
- [Configure Project Quay — `DB_URI` database fields](https://docs.projectquay.io/config_quay.html) (unmanaged Postgres; `pg_trgm` extension requirement is documented in the [operator deploy guide](https://docs.projectquay.io/deploy_red_hat_quay_operator.html))
- [Deploying the Red Hat Quay Operator (3.15) — object storage / components](https://docs.redhat.com/en/documentation/red_hat_quay/3.15/html-single/deploying_the_red_hat_quay_operator_on_openshift_container_platform/index)
- [Use Project Quay (REST API automation)](https://docs.projectquay.io/use_quay.html), [Repository Notifications](https://docs.quay.io/guides/notifications.html)
- [Kargo Quay webhook receiver](https://docs.kargo.io/user-guide/reference-docs/webhook-receivers/quay/)
