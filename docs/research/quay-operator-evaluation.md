# Research: Quay Operator vs. the Current Hand-Authored Quay Approach

Research date: 2026-06-16.

**Base commit.** This report was researched against `main` at commit
[`a3535d4`](https://github.com/holos-run/holos-paas/commit/a3535d4370a7fc4766a17a302ce1ae0c90ec599a)
(`a3535d4370a7fc4766a17a302ce1ae0c90ec599a`). Every deep link into this
repository below is pinned to that commit so the referenced lines stay stable
as the tree evolves. Upstream Quay Operator links are pinned to the
`quay/quay-operator` `master` branch (and named release branches where noted),
verified against GitHub on the research date.

This report fulfills [HOL-1278](https://linear.app/holos-run/issue/HOL-1278/research-quay-operator).

## Question

holos-paas currently deploys and manages [Quay](https://www.projectquay.io/)
with hand-authored CUE manifests plus create-if-absent bootstrap Jobs, no
operator. HOL-1278 asks: **should we move to the upstream
[Quay Operator](https://github.com/quay/quay-operator) (the `QuayRegistry`
CRD), or keep the current approach?** The decision is judged against six goals:

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

**Keep the current hand-authored approach for the MVP. Do not adopt the Quay
Operator now.** The single decisive fact:

> The `QuayRegistry` CRD manages only the **registry infrastructure** (the Quay
> pods and their backing services — Postgres, Redis, object storage, TLS,
> routing, Clair). It has **no** concept of organizations, repositories, robot
> accounts, or repository notification webhooks. Verified against the operator's
> own API types — the `ComponentKind` constants are exactly `quay`, `postgres`,
> `clair`, `clairpostgres`, `redis`, `horizontalpodautoscaler`, `objectstorage`,
> `route`, `mirror`, `monitoring`, `tls` and nothing else
> ([`apis/quay/v1/quayregistry_types.go`](https://github.com/quay/quay-operator/blob/master/apis/quay/v1/quayregistry_types.go)
> and the [`QuayRegistry` CRD](https://github.com/quay/quay-operator/blob/master/config/crd/bases/quay.redhat.com_quayregistries.yaml);
> the operator's [`docs/components.md`](https://github.com/quay/quay-operator/blob/master/docs/components.md)
> documents the managed/unmanaged model but lists only a subset of these kinds).

Goals 1 and 2 — the genuinely hard, holos-paas-specific goals — are **data-plane**
concerns the Quay Operator does not address. They are reachable only through
Quay's OAuth2 REST API (`/api/v1/...`), exactly as the repo already does today.
A future `Repository`/`ProjectRequest` CRD with a reconciler that calls that API
(the direction [ADR-2](../adr/ADR-2.md) already sets) is **orthogonal** to how
the registry itself is deployed — it is needed whether Quay runs from
hand-authored manifests or from a `QuayRegistry`. Adopting the operator would
therefore *not* advance Goals 1 or 2, while it *would* add an Operator Lifecycle
Manager (OLM) dependency and force its OpenShift-coupled components
(`route`, `tls`, `objectstorage`, `monitoring`) into `unmanaged` mode on the
non-OpenShift (k3d) target — eroding the operator's main "batteries included"
benefit on exactly our platform.

The one goal the operator helps with is Goal 3 (the production path), and only
if/when the production target becomes OpenShift. That is a re-evaluation
trigger, recorded at the end of this report — not a reason to refactor now.

## 1. The current approach (hand-authored manifests + bootstrap Jobs)

Quay is deployed as a plain `apps/v1` `Deployment` rendered from CUE, with no
operator and no `QuayRegistry` CRD. A repository-wide grep for `QuayRegistry` /
`quay-operator` returns nothing; the buildplan itself notes "Quay has no
operator to seed a first user"
([`quay/buildplan.cue#L507-L511`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/holos/components/quay/buildplan.cue#L507-L511)).

| Concern | Current implementation | Deep link (pinned to `a3535d4`) |
|---|---|---|
| Image / version | `quay.io/projectquay/quay:3.17.3`, multi-arch (incl. arm64 for Apple-silicon k3d) | [`quay/buildplan.cue#L36-L45`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/holos/components/quay/buildplan.cue#L36-L45) |
| `config.yaml` template | Rendered constant, substituted at init time | [`quay/buildplan.cue#L236-L280`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/holos/components/quay/buildplan.cue#L236-L280) |
| Storage (blobs) | `LocalStorage` → 5Gi `ReadWriteOnce` PVC, storageClass omitted so it binds k3s `local-path` | [`quay/buildplan.cue#L250-L255`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/holos/components/quay/buildplan.cue#L250-L255), [`#L860-L878`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/holos/components/quay/buildplan.cue#L860-L878) |
| Database | CNPG `Cluster` `quay-db`, 1 instance, local-path PVC; app reads the CNPG-generated `quay-db-app` Secret URI | [`cnpg-clusters/buildplan.cue#L36-L90`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/holos/components/cnpg-clusters/buildplan.cue#L36-L90) |
| Redis | Single ephemeral Deployment, in-cluster only | [`quay/buildplan.cue#L739-L823`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/holos/components/quay/buildplan.cue#L739-L823) |
| Encryption keys | `quay-secret-keys` Secret (`SECRET_KEY`, `DATABASE_SECRET_KEY`), create-if-absent Job | [`quay/buildplan.cue#L311-L345`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/holos/components/quay/buildplan.cue#L311-L345) |
| Admin/superuser credential | `quay-admin-bootstrap` Job POSTs the one-shot `/api/v1/user/initialize` and stores a non-expiring superuser OAuth token in the `quay-initial-admin` Secret (key `token`) | [`quay/buildplan.cue#L507-L617`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/holos/components/quay/buildplan.cue#L507-L617) |
| OIDC (Keycloak SSO) | Confidential client, no PKCE; `quay-oidc` client secret bootstrapped into both namespaces | [`quay/buildplan.cue#L262-L273`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/holos/components/quay/buildplan.cue#L262-L273), [ADR-15](../adr/ADR-15.md) |

### How orgs / repos / robots / webhooks are provisioned today

This is the part to compare carefully against the operator, because **this is
what the operator does not do**. All provisioning is done through Quay's REST
API, authenticating with the `quay-initial-admin` token:

- `scripts/quay-init` bootstraps the shared `holos` org, a push robot, the
  `creators` team, and an in-cluster pull Secret — `/api/v1/user/initialize`
  ([`scripts/quay-init#L237`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/scripts/quay-init#L237)),
  `/api/v1/organization/`
  ([`#L271-L283`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/scripts/quay-init#L271-L283)),
  robots
  ([`#L297-L310`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/scripts/quay-init#L297-L310)),
  team membership
  ([`#L332-L364`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/scripts/quay-init#L332-L364)).
- The **`my-project` delivery scaffold** is the reference instance and the
  template for the future self-service `ProjectRequest`. Its bootstrap Job
  creates the org/repo/robot, grants pull permission, and **registers the
  `repo_push` notification webhook pointing at the Kargo receiver URL** — all
  create-if-absent, all via the REST API:
  - org/repo/robot/permission:
    [`my-project/buildplan.cue#L553-L593`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/holos/components/my-project/buildplan.cue#L553-L593)
  - reads the Kargo receiver URL from `ProjectConfig.status` then `POST`s the
    `repo_push` webhook to `/api/v1/repository/{repo}/notification/`:
    [`#L649-L685`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/holos/components/my-project/buildplan.cue#L649-L685)
  - the Kargo `ProjectConfig` with the matching `webhookReceivers[].quay`:
    [`#L314-L333`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/holos/components/my-project/buildplan.cue#L314-L333)

This webhook → `Warehouse` wiring (Quay `repo_push` → Kargo receiver →
`Freight` → `Stage` promotion) is the mechanism Goal 2 asks for. It is built
entirely on Quay's REST API and Kargo's
[Quay webhook receiver](https://docs.kargo.io/user-guide/reference-docs/webhook-receivers/quay/),
and is **independent of how the Quay registry is deployed**.

## 2. The Quay Operator: what it is, and what it manages

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

So Goal 6's "high quality, well maintained, robust" bar **is met** by the
operator in the abstract. The problem is not the operator's quality — it is that
its scope does not cover Goals 1–2, and its design center (OpenShift) does not
match our target (Goal 4).

### Running on non-OpenShift / a laptop (Goal 4)

The operator is designed for OpenShift first. Its capability detection forces
the components that depend on OpenShift-only APIs to `unmanaged` when those APIs
are absent — on vanilla Kubernetes (our k3d laptop target) that set is
`route`, `tls`, `objectstorage`, and `monitoring`. The remaining components —
`quay` (always managed), `postgres`, `redis`, `clair`, and
`horizontalpodautoscaler` — still default to managed:

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

Net: on a laptop the operator runs `quay` + `postgres`/`redis`/`clair`/`hpa`
managed but `route` + `tls` + `objectstorage` + `monitoring` unmanaged — and it
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
([`cnpg-clusters/buildplan.cue#L36-L90`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/holos/components/cnpg-clusters/buildplan.cue#L36-L90)).

## 3. Evaluation against the six goals

| Goal | Current approach | Quay Operator | Verdict |
|---|---|---|---|
| **1.** Credential to provision orgs/repos | ✅ `quay-initial-admin` token (superuser OAuth) created headlessly via `/api/v1/user/initialize` | ➖ Out of scope — operator never creates such a credential; you would still need the same `/api/v1/user/initialize` bootstrap | **No operator benefit.** Same credential, same mechanism, regardless of deployment method |
| **2.** Repository → Kargo `Warehouse` webhook via a K8s CR | ⚠️ Done today via REST API in the `my-project` Job; the K8s-CR-driven version is a *future holos-paas controller* | ➖ Out of scope — no repository/notification component in `QuayRegistry` | **No operator benefit.** Needs a holos-paas `Repository` reconciler calling Quay's REST API; orthogonal to the operator |
| **3.** No big refactor to production | ⚠️ Scale by editing CUE constants; HA/upgrades are our responsibility | ✅ Supported production path *on OpenShift* (HA, rolling upgrades, config validation) | **Operator wins — but only if production = OpenShift.** Otherwise the unmanaged-component caveats blunt the advantage |
| **4.** Slim, laptop-friendly | ✅ Single pod, local-path PVC, ephemeral Redis, 1-instance CNPG | ⚠️ Works only with `objectstorage`/`route`/`tls`/`monitoring` unmanaged + OLM overhead | **Current approach wins** for the k3d target |
| **5.** CNPG database | ✅ CNPG `quay-db`, app reads `quay-db-app` URI | ⚠️ CNPG only as `unmanaged` Postgres (`DB_URI`); managed Postgres is the operator's own, not CNPG | **Tie / current approach simpler** — CNPG is unmanaged either way |
| **6.** Operator quality | n/a (no operator) | ✅ Actively maintained (branches updated 2026-06), Apache-2.0, Red Hat-backed | **Operator is high quality** — but quality is moot given Goals 1–2 are out of its scope |

## 4. The decisive finding, restated

The question "Quay Operator vs. current approach" conflates two layers:

1. **Registry control plane** — deploying/operating the Quay pods and backing
   services. *Both* approaches solve this; the operator is the more production-
   grade option **on OpenShift**, the hand-authored manifests are the simpler
   option **on a laptop/k3d**.
2. **Registry data plane** — orgs, repos, robots, and notification webhooks.
   *Neither* the operator nor any first-party Quay CRD solves this. The only
   interface is Quay's OAuth2 REST API. Community Kubernetes controllers/CRDs for
   Quay *content* are not part of the project and none is mature enough to
   depend on (search surfaced none; Quay's own guidance is to automate the REST
   API — [Project Quay use guide](https://docs.projectquay.io/use_quay.html),
   [repository notifications](https://docs.quay.io/guides/notifications.html)).

Goals 1 and 2 — the reason HOL-1278 exists — live entirely in layer 2. The
holos-paas-authored `Repository`/`ProjectRequest` CRD + reconciler that
[ADR-2](../adr/ADR-2.md) foreshadows (and that the `my-project` scaffold
prototypes imperatively today) is the actual answer, and it is **the same work
whether or not we adopt the operator at layer 1**.

## 5. Recommendation

1. **Keep the current hand-authored Quay deployment for the MVP.** It already
   satisfies Goals 4 and 5 cleanly, provides the Goal-1 credential, and carries
   the Goal-2 webhook wiring (in the `my-project` scaffold). Adopting the
   operator now would add OLM and a `QuayRegistry` reconciler, force most
   components unmanaged on k3d, and advance *none* of Goals 1, 2, 4, or 5.
2. **Invest the next increment in a holos-paas `Repository` (and
   `ProjectRequest`) CRD + reconciler** that calls Quay's REST API to create the
   org/repo/robot and register the `repo_push` webhook against the project's
   Kargo `Warehouse` receiver URL — promoting the imperative `my-project`
   bootstrap Job into a declarative, reconciled controller. This is what
   actually delivers Goals 1 and 2 as Kubernetes-native custom resources, and it
   is **not blocked by, nor does it block, the layer-1 choice**. This keeps the
   "no significant refactor" promise of Goal 3: the reconciler is reusable
   verbatim no matter how the registry is deployed.
3. **Re-evaluate the operator at one explicit trigger:** the production target
   becomes OpenShift (or another cluster where the `ObjectBucketClaim`/NooBaa,
   Route, and monitoring stacks are first-class). At that point the operator's
   managed components and supported upgrade path make it the better layer-1
   choice, and migration is a contained swap — replace the hand-authored Quay
   `Deployment`/Service/PVC/Secret manifests with a `QuayRegistry` CR
   (`postgres` unmanaged → existing CNPG `DB_URI`; reuse the existing
   `quay-secret-keys` and `quay-oidc` Secrets via the config bundle). The
   layer-2 reconciler and the `quay-initial-admin` credential are untouched by
   that swap — which is precisely why doing layer 2 first is safe.

If/when the operator is adopted, capture the decision in a new revision of
[ADR-16](../adr/ADR-16.md) (Quay's role in delivery) or a dedicated ADR, per the
repo's "revise the existing ADR" convention.

## Sources

Repository (pinned to commit `a3535d4370a7fc4766a17a302ce1ae0c90ec599a`):

- [`holos/components/quay/buildplan.cue`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/holos/components/quay/buildplan.cue)
- [`holos/components/cnpg-clusters/buildplan.cue`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/holos/components/cnpg-clusters/buildplan.cue)
- [`holos/components/my-project/buildplan.cue`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/holos/components/my-project/buildplan.cue)
- [`scripts/quay-init`](https://github.com/holos-run/holos-paas/blob/a3535d4370a7fc4766a17a302ce1ae0c90ec599a/scripts/quay-init)
- [ADR-2](../adr/ADR-2.md), [ADR-15](../adr/ADR-15.md), [ADR-16](../adr/ADR-16.md)

Quay Operator (`quay/quay-operator`, `master` / release branches):

- [`apis/quay/v1/quayregistry_types.go`](https://github.com/quay/quay-operator/blob/master/apis/quay/v1/quayregistry_types.go) — `ComponentKind` constants
- [`config/crd/bases/quay.redhat.com_quayregistries.yaml`](https://github.com/quay/quay-operator/blob/master/config/crd/bases/quay.redhat.com_quayregistries.yaml) — the `QuayRegistry` CRD
- [`docs/components.md`](https://github.com/quay/quay-operator/blob/master/docs/components.md) — managed/unmanaged component model
- [Release branches](https://github.com/quay/quay-operator/branches) — maintenance evidence (June 2026)

Upstream / vendor docs:

- [Deploying the Project Quay Operator](https://docs.projectquay.io/deploy_red_hat_quay_operator.html)
- [Configure Project Quay — `DB_URI` database fields](https://docs.projectquay.io/config_quay.html) (unmanaged Postgres; `pg_trgm` extension requirement is documented in the [operator deploy guide](https://docs.projectquay.io/deploy_red_hat_quay_operator.html))
- [Deploying the Red Hat Quay Operator (3.15) — object storage / components](https://docs.redhat.com/en/documentation/red_hat_quay/3.15/html-single/deploying_the_red_hat_quay_operator_on_openshift_container_platform/index)
- [Use Project Quay (REST API automation)](https://docs.projectquay.io/use_quay.html), [Repository Notifications](https://docs.quay.io/guides/notifications.html)
- [Kargo Quay webhook receiver](https://docs.kargo.io/user-guide/reference-docs/webhook-receivers/quay/)
