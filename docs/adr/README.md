# Holos PaaS — Architecture Decision Records

This directory holds the Architecture Decision Records (ADRs) for
holos-paas. The format follows the
[NATS architecture-and-design](https://github.com/nats-io/nats-architecture-and-design)
convention, scoped here to **ADR documents only**.

ADRs serve three purposes:

1. **Detailed design specifications** for API resources (CRDs) and their
   reconcilers.
2. **Convention guidance** that explains how and why things are done a certain
   way across the platform.
3. **System-wide design documentation** capturing decisions that affect the
   project as a whole.

These are living documents. Prefer revising an existing ADR (and recording the
change in its revision table) over writing a new one for a minor decision. Use
ADRs for decisions worth remembering, not for routine individual choices.

Before writing an ADR, read [writing-adrs.md](writing-adrs.md) and copy
[adr-template.md](adr-template.md) as your starting point.

## Index

Unlike the upstream NATS repository, this index is **maintained by hand** — add
a row when you add an ADR. Keep the metadata table and header format identical
to the template above.

| Index             | Tags                  | Description                                                        |
|-------------------|-----------------------|-------------------------------------------------------------------|
| [ADR-1](ADR-1.md) | api, multi-tenancy    | Project resource: the platform tenant, adopted from the GCP Project |
| [ADR-2](ADR-2.md) | api, principles       | Core platform principles; KRM is the primary platform API         |
| [ADR-3](ADR-3.md) | rbac, authz, security | Authorization via Kubernetes RBAC and group membership            |
| [ADR-4](ADR-4.md) | api, multi-tenancy    | The platform API must support multiple tenants                    |
| [ADR-5](ADR-5.md) | api, billing, quotas  | Chargeback, quotas, and limits following the GCP model            |
| [ADR-6](ADR-6.md) | pipeline, mvp, nats   | **Deprecated** (superseded by ADR-16) — Six-stage MVP Heroku-style deployment pipeline on a NATS JetStream backbone |
| [ADR-7](ADR-7.md) | workload, build       | KubeRay reference workload on k3d (Apple Silicon), multi-stage build |
| [ADR-8](ADR-8.md) | registry, build, kargo, oci | Container registry and image tagging; the tag is the version; the rendered-manifests artifact push is watched by a Kargo `Warehouse` (ADR-16) |
| [ADR-9](ADR-9.md) | webhook, nats, ingress | **Deprecated** (see ADR-16) — Thin webhook receiver posting raw bodies to a NATS WorkQueue; not used / deferred in favor of a Kargo registry watch |
| [ADR-10](ADR-10.md) | webhook, subscriber | **Deprecated** (see ADR-16) — Webhook subscriber parses events and routes render or deployer tasks by KRM match; not used / deferred in favor of Kargo |
| [ADR-11](ADR-11.md) | api, deployer, gitops | **Deprecated** (see ADR-16) — Deployer updates the Application's config-image version; not used / deferred (Kargo `argocd-update` patches `targetRevision`); Git write-back/SoD deferred |
| [ADR-12](ADR-12.md) | layout, conventions, build | Single-module monorepo layout for multiple Go services and Holos CUE |
| [ADR-13](ADR-13.md) | pipeline, mvp, nats, oci, argocd | **Deprecated** (superseded by ADR-16) — End-to-end MVP deployment flow: two registry-event loops through render and Argo CD |
| [ADR-14](ADR-14.md) | api, nats, protobuf, conventions | **Deprecated** (see ADR-16) — NATS message schemas are ConnectRPC protobuf definitions; not used / deferred (no in-cluster task subscribers under the pivot) |
| [ADR-15](ADR-15.md) | registry, oidc, security | Quay↔Keycloak OIDC SSO: `AUTHENTICATION_TYPE: OIDC` sole identity store (Revision 4, HOL-1293), confidential client with client-secret auth and **no** PKCE (Revision 7 / HOL-1317 — Quay 3.17.3 logout-state defect), username from the ID token, roles/groups via the `groups` claim into Quay teams |
| [ADR-16](ADR-16.md) | pipeline, kargo, oci, oras, kustomize, argocd, mvp | Kargo-driven promotion with a client-side CLI build-and-publish (ORAS) workflow; Kustomize OCI artifact, not Helm; supersedes the NATS pipeline (ADR-6, ADR-13) |
| [ADR-17](ADR-17.md) | cli, conventions, agents, build | Fisk (not Cobra) for the holos-paas CLI: LLM-friendly help and JSON-schema introspection for AI coding agents; the `deploy` subcommand fronts the ADR-16 publish workflow |
| [ADR-18](ADR-18.md) | controller, api, gitops | **Partially Implemented** — The Holos Controller (namespace `holos-controller`, shipped HOL-1309..HOL-1313) reconciles CRDs filling the Quay/Keycloak data-plane gaps the upstream operators leave open (first group `quay.holos.run`, ADR-19; Keycloak group ADR-20 still proposed); delivery is the GitOps rendered-manifest pattern; Rev 2 records the **AC #7 API-group dependency boundary** (the CRs take no Kargo/Argo CD dependency, only the Quay API + credential `secretRef`; the controller binary may) and the repos-only-via-Repository rule (AC #9); refines ADR-12's API-group example; supersedes the ADR-15 (Rev 4–5) manual Quay Resource Controller stop-gap for the data plane |
| [ADR-19](ADR-19.md) | api, controller, quay, registry | **Implemented** — The `quay.holos.run/v1alpha1` Organization and Repository CRDs reconciled by the Holos Controller (ADR-18), **as built** (Rev 2): Organization (`name`/`email`/`adopt` + ownership claim model, **no** inline repos/toggle) and Repository (`organizationRef`/`name`/`visibility`/`description` + a `repo_push` `webhook` with exactly one of inline `url` or `urlSecretRef`, AC #8); Gateway-API conditions; the `credentialsSecretRef` → `holos-controller-quay-creds` (in `holos-controller`) credential design; the **AC #7** no-Kargo/Argo-CD API-group boundary and the **AC #9** repos-only-via-Repository rule; reconciled via the Quay REST API with the ADR-15 superuser OAuth-Application token; **Rev 6** adds Organization `spec.syncedTeams` (OIDC group→Quay-team `role` + optional org default repo permission, referenced by group name only; non-exclusive / adoption-is-an-error via `status.managedTeams` + a durable team-description heal marker; the GCP-style owner/editor/viewer primitive-role use case); updates ADR-15 |
| [ADR-20](ADR-20.md) | api, controller, keycloak, oidc, rbac | **Proposed** — The `keycloak.holos.run` API group the existing `holos-controller` binary owns as a second group alongside `quay.holos.run` (ADR-19), reconciled over the Keycloak Admin API. **Rev 2** makes the design concrete for project group management: a centrally-managed **`KeycloakInstance`** (Admin API URL, `caBundle`, admin `credentialsSecretRef`, `realm`; multiple instances per cluster; in/out-of/remote-cluster), a **`KeycloakClient`** (OIDC client named by URL + `groups`-claim wiring), a **`KeycloakGroup`** (shallow nested `projects/<project>/{roles,custodians}/{owner,editor,viewer}` tree, FGAP-v2 custodian delegation), a **`KeycloakUser`** (Admin-API pre-create-by-email + group membership; the first-broker-login auto-link is platform realm/IdP config the `KeycloakRealmImport` CR owns — not `keycloak-config-cli` — assumed by the CR), and `KeycloakClientRole`/`KeycloakRealmRole`. The role-group's flat `groups`-claim value (`my-project-owner`) comes from a **client role on the consumer's client** (the Quay client for ADR-19 `syncedTeams`, whose `oidc-usermodel-client-role-mapper` is per-client) — full-path mapper / script mapper rejected. Every Kind references a `KeycloakInstance`, cross-namespace via a `security.holos.run` `ReferenceGrant` (ADR-22). Disjoint from `keycloak-config-cli` via reserved platform names + a per-CR claim model; `holos_controller` metrics + Gateway-API status; API-group dependency boundary (`api/keycloak/v1alpha1` imports only k8s libs). Updates ADR-3 (the platform provisions the Keycloak side of its groups); design record only |
| [ADR-21](ADR-21.md) | holos, components, projects, gitops, multi-tenancy | **Implemented** (Rev 4, HOL-1358) — The Holos Project and Application CUE components delivering one-line self-service: a `holos/projects/*.cue` entry renders the project-level resources (a central-registry Namespace entry, the Kargo Project — which brings the shared ProjectConfig + receiver-token bootstrap — the Argo CD AppProject/Application in `argocd`, owner RoleBinding, Quay Organization (ADR-19), and — **Rev 2 (HOL-1340)** — the per-Project `keycloak.holos.run` CRs (ADR-20): the `KeycloakGroup` roles/custodians tree + `my-project-<role>` client-role group→claim binding, the owner's `KeycloakUser`, a conditional `KeycloakClient`, the Gateway-API ReferenceGrant for route/backend references, and the HTTPRoute — while the `security.holos.run` ReferenceGrant (ADR-22) authorizing each Keycloak CR's cross-namespace `KeycloakInstance` reference is created by the instance namespace's platform owner, not rendered by the component) and a `holos/apps/*.cue` entry renders 11 application-level resources (Quay Repository, Kargo Warehouse/Stage + the intended blue-green progressive-delivery pipeline, Argo CD Application in `argocd`, and Deployment/Service/ExternalSecret/ConfigMap/ServiceAccount/RoleBinding workloads in the project namespace); apps are unified under projects by GCP-model containment (Project ≈ Namespace security boundary, `apps.<name>.project` binding); generalizes the `my-project` scaffold; integrates the central `holos/namespaces.cue` registry; updates ADR-1 (maps the Project tenant onto Kubernetes under GitOps). Rev 2 records the **AC #2 decision** (no new umbrella-Project ADR — ADR-1 + ADR-21 are its home) and the **end-to-end worked example** (`owners: "bob@example.com"` → Keycloak groups → `groups` claim → Quay `syncedTeams`). **Rev 3 (HOL-1354)** built the foundation (collections + env-prefixed namespace derivation) and revised the topology to one Namespace per environment. **Rev 4 (HOL-1358)** flips to `Implemented`: the Project + Application components are built (HOL-1355/HOL-1356) and `my-project` is migrated onto them with the bespoke component deleted (HOL-1357). One as-built deviation ratified: control-plane CRs land in the **bare `<name>`** control namespace, not `prod-<name>` (the `validateDirectClientRole` guard, HOL-1350). Deferred follow-ons (full ci→qa→prod promotion + progressive delivery, external-secrets store, self-service `ProjectRequest`) recorded in placeholders + the [authoring guide](../../holos/docs/project-and-application-templates.md) |
| [ADR-22](ADR-22.md) | api, controller, security, references | **Proposed** — The `security.holos.run` API group and its `ReferenceGrant` Kind: the standard, Kubernetes-native, Gateway-API-style mechanism authorizing cross-namespace references between `holos.run` custom resources. Namespaced, lives in the **referent (target) namespace**, with `spec.from[]` (group/kind/namespace of authorized referrers) and `spec.to[]` (group/kind[/name] of local referenceable objects) mirroring Gateway API's From/To; takes **no** external-system dependency (pure Kubernetes-native policy). Trust model: platform owners grant in the instance namespace, platform users consume from their project namespaces, and an ungranted cross-namespace reference is rejected (`Ready=False`), never silently honored. A holos-owned grant is minted (rather than co-opting Gateway API's, for API ownership/boundary — no dependency on the Gateway API being installed, and no overloading of a grant istio-gateway already uses for its route/backend cases) to generalize the From/To pattern to arbitrary CR-to-CR references (e.g. a `keycloak.holos.run` `User`/`Group`/`Client` → a `KeycloakInstance` in another namespace); the two grants coexist; design record only |

## Status values

| Status                  | Meaning                                                            |
|-------------------------|-------------------------------------------------------------------|
| `Proposed`              | Drafted and open for discussion; not yet agreed upon.             |
| `Approved`              | Agreed upon; implementation has not started or is incomplete.     |
| `Partially Implemented` | Some of the design has shipped; the rest is outstanding.          |
| `Implemented`           | The design is fully reflected in the code.                        |
| `Deprecated`            | No longer the recommended approach; kept for historical record.   |
