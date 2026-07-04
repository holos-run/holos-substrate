# Project and Application Templates — Authoring Guide

The platform's self-service "docker push to deploy" experience
([ADR-21](../../docs/adr/ADR-21.md)) is collection-driven: a product engineer
stands up a project, or an application within one, by adding **one entry** to a
well-known CUE collection. Two Holos components render the full set of resources
that compose a Project or an Application from those entries:

- the **Project component** —
  [`components/project/buildplan.cue`](../components/project/buildplan.cue) —
  renders project-level resources for each `holos/projects/*.cue` entry.
- the **Application component** —
  [`components/application/buildplan.cue`](../components/application/buildplan.cue)
  — renders application-level resources for each `holos/apps/*.cue` entry.

`my-project` is the reference instance: as of HOL-1357 it is produced **entirely**
by these two components (the bespoke `holos/components/my-project/` component was
deleted). Read this guide, then [ADR-21](../../docs/adr/ADR-21.md) for the design
rationale.

## Register a project

Add one file to [`holos/projects/`](../projects/). The canonical worked example
is a single line:

```cue
// holos/projects/my-project.cue
package projects

projects: "my-project": owners: "bob@example.com": _
```

- The **map key** (`my-project`) is the project name. It must be an RFC 1123 DNS
  label, must not collide with a reserved platform namespace, and must not begin
  with a reserved environment prefix (`ci-`/`qa-`/`prod-`) — all enforced at
  **render time** ([`holos/collections.cue`](../collections.cue)
  `#CollectionsValidated`, [`holos/namespaces.cue`](../namespaces.cue)
  `#ReservedNamespaceNames` / `#ProjectNameNoEnvPrefix`).
- **`owners`** is a CUE map keyed by the owner's email
  (`projects.<name>.owners.<email>`), so a project may name one or several
  owners. Each owner email is validated. Every project must name **at least one**
  owner (a render-time assertion). The single-owner registration above is the
  common case; the owner is pre-provisioned as a `KeycloakUser` and seeded into
  first-class `KeycloakGroupMembership` CRs for the project's owner role and
  custodian groups (below).

The `#Project` schema lives in [`holos/projects/projects.cue`](../projects/projects.cue);
`name` is set from the map key (do not author it).

A project stands alone: it is valid and renders a complete control plane with
**zero** applications.

## Register an application

Add one file to [`holos/apps/`](../apps/). The worked example:

```cue
// holos/apps/my-app.cue
package apps

apps: "my-app": {
	project: "my-project"
	image:   "registry.k8s.io/e2e-test-images/agnhost:2.53"
	port:    8080
}
```

The `#App` schema ([`holos/apps/apps.cue`](../apps/apps.cue)) requires:

| Field      | Required | Meaning                                                               |
| ---------- | -------- | --------------------------------------------------------------------- |
| `project`  | yes      | The project the app belongs to. Must be a key in the `projects` collection — an app naming a non-existent project is a **render-time** failure (the `apps` package unifies `project` with `projects.#RegisteredProject`). |
| `image`    | yes      | The container image reference (non-empty).                            |
| `port`     | yes      | The container TCP port (`1..65535`).                                  |
| `host`     | no       | The external hostname. Defaults to `<app-name>.holos.internal`.     |

A project supports **zero to many** applications; each app binds to exactly one
project via `project` (GCP-model containment — the project *is* the namespace
security boundary, [ADR-1](../../docs/adr/ADR-1.md)/[ADR-21](../../docs/adr/ADR-21.md)).

## The env-prefixed namespace model

A project is realized as **one Namespace per environment**. For every
`projects.<name>` entry, [`holos/namespaces.cue`](../namespaces.cue) derives — via
a comprehension over `#Environments` (`["ci", "qa", "prod"]`) and the
`#ProjectNamespace` `(project, env) → "<env>-<name>"` mapping — the namespaces:

- `ci-<name>`, `qa-<name>`, `prod-<name>` — the per-environment delivery
  namespaces, and
- `<name>` — the **bare control namespace**.

All carry `_ambient: true` (Istio ambient enrollment), the
`kargo.akuity.io/project: "true"` adoption label, and the
`kargo.akuity.io/keep-namespace: "true"` annotation. The `namespaces` component
renders the actual `Namespace` manifests; the Project/Application components only
**reference** the derived names (the no-inline-`Namespace` guardrail,
[component-guidelines.md](component-guidelines.md)).

### Where the control-plane CRs land: the bare `<name>` namespace

The project-scoped, environment-independent control-plane CRs — the Quay
`Organization`, the `keycloak.holos.run` groups/user/client, and the
cluster-scoped Kargo `Project`'s adopted namespace — land in the **bare `<name>`**
control namespace.

> **As-built deviation from ADR-21 Revision 3.** Revision 3 chose `prod-<name>`
> (`#ProjectControlEnvironment`) as the control namespace. The as-built
> components use **bare `<name>`** instead — the same namespace the deleted
> bespoke component used, kept for continuity and legibility. This placement was
> originally also *required* by the Holos Controller's `validateDirectClientRole`
> project↔namespace guard (HOL-1350), but **that guard was removed in HOL-1421
> (ADR-20 Rev 7): the `keycloak.holos.run` controller is now transparent and no
> longer requires a role group's CR namespace to equal the project name** — the
> bare-`<name>` placement is now a convention, not a controller requirement.
> `#ProjectControlEnvironment` is still defined and the `prod-<name>` env
> namespace still carries the per-app validation annotation, but the CRs use bare
> `<name>`. ADR-21 Revision 4 ratifies this.

The `ci-/qa-/prod-<name>` namespaces are derived (so the topology, RBAC
boundaries, and Kargo adoption labels exist) but, in the current phase, the
single wired delivery path runs through the **bare `<name>`** namespace.

## IAM: primitive roles → Quay teams and → app clients

The project gets the GCP-style primitive roles `owner`/`editor`/`viewer`,
realized as `keycloak.holos.run` resources the Holos Controller
([ADR-20](../../docs/adr/ADR-20.md)) reconciles into the `holos` realm:

- **Role groups** `projects/<name>/roles/{owner,editor,viewer}` — the groups a
  person is a member of to hold a primitive role.
- **Custodian groups** `projects/<name>/custodians/{owner,editor,viewer}` — whose
  members manage the matching `roles/*` group's membership.
- Owner **`KeycloakGroupMembership`** CRs — one named
  `<name>-roles-owner-members` that seeds project owners into
  `projects/<name>/roles/owner`, plus one per custodian tier named
  `<name>-custodians-{owner,editor,viewer}-members` that seeds the same owners
  into every custodian group.
- The owner's **`KeycloakUser`** (pre-created by email, first-login auto-linked).
- The project's own **`KeycloakClient`** (`https://<name>.holos.internal`).

Each role group confers its primitive role on **three** clients via
`clientRoles[]`:

1. **The platform Quay client** (`https://quay.holos.internal`) — named directly
   by `clientId`, conferring the project-prefixed role **`<name>-<role>`**. The
   Quay client's existing `quay-client-roles` mapper emits that value into the
   `groups` claim, and the project's Quay `Organization.spec.syncedTeams[]` maps
   each `<name>-<role>` claim value to a Quay team **by name** (owner →
   `role: admin`; editor → `role: creator` + `repositoryPermission: write`;
   viewer → `role: member` + `repositoryPermission: read` — the
   [ADR-19](../../docs/adr/ADR-19.md) primitive-role example). This is the
   one-line-CUE → Keycloak group → `groups` claim → Quay team chain end to end.
2. **The project's own client** (`clientRef` to the project `KeycloakClient`) —
   conferring `<name>-<role>`, so the value reaches that client's token.
3. **Each app's client** (`clientRef` to the app's `KeycloakClient`) — conferring
   the **bare** primitive role (`owner`/`editor`/`viewer`), so project-role
   membership maps onto matching application roles. The Application component
   defines those three roles on the app client
   ([`components/application/buildplan.cue`](../components/application/buildplan.cue));
   the Project component iterates the project's apps and adds one `clientRoles[]`
   entry per app to each role group.

The direct-`clientId` grant is **transparent** (HOL-1421, ADR-20 Rev 7): the
controller resolves the named client, ensures the named role exists, and confers
it **verbatim** for any client and any role name — it no longer restricts the
target to a Quay-client allowlist, no longer requires the role group's `<name>`
to equal the CR's namespace, and no longer enforces an exact `<name>-<leaf>`
match or any reserved-name refusal. The Project component still emits only the
conventional `<name>-<role>` names against the Quay client, so its rendered
output is unchanged. Enforcing naming conventions, reserved prefixes, or
tenant/platform disjointness is now the job of **admission control**
(`ValidatingAdmissionPolicy` / `ValidatingAdmissionWebhook` + policy CRs), a
separate downstream effort. See [ADR-20](../../docs/adr/ADR-20.md) and the
*Project Delivery Scaffold* guardrail in [AGENTS.md](../../AGENTS.md).

The cross-namespace reference each Keycloak CR makes to the central
`KeycloakInstance` (the separate `keycloak-instance` component) is authorized by a
`security.holos.run` `ReferenceGrant` ([ADR-22](../../docs/adr/ADR-22.md)) the
**instance namespace's owner** creates — not a resource the templates render.
Role and custodian group membership is managed through `KeycloakGroupMembership`
CRs in the project's control namespace. The rendered CRs seed the standing owner
set; the intended day-2 owner path is separate owner-authored membership CRs in
that same namespace, using the owner RoleBinding's namespace `admin` role and the
membership RBAC shipped by the reconciler phase. Treat the rendered standing-owner
CRs as platform-owned scaffold, and gate broader owner writes with ADR-24's
rendered-object protection/admission policy.
During the migration from the old user-owned group list, the user reconciler
prunes that group edge once, and the membership CR re-adds the declared edge on
its next reconcile.

## The application resource set

For each `apps.<name>` entry the Application component renders, split into two
bundles:

- **Workload** (Argo CD syncs from the published `<app>-config` OCI artifact):
  `Deployment`, `Service`, `HTTPRoute` (attaching to the shared Gateway at
  `<host>`, default `<app>.holos.internal`), `ConfigMap`, `ServiceAccount`, and
  a view `RoleBinding` — all in the project's control namespace.
- **Control plane** (operator-applied, never Argo-synced): the app's
  `KeycloakClient`, the Quay `Repository` (within the project's `Organization`),
  the Kargo `Warehouse` and `Stage`, and the app's Argo CD `Application` (in
  `argocd`, named `<project>-<app>`, destination the project namespace).

The shared Kargo `Project`/`ProjectConfig`/receiver-token bootstrap is the
**Project** component's, not re-emitted per app.

## The one wired delivery path vs. the deferred promotion chain

The current phase wires **one** delivery path (the generalized `my-project`
publish → Freight → promotion → sync loop), through the bare `<name>` namespace.
The following are **deferred** (see [placeholders.md](placeholders.md) →
*Project/Application templates: deferred follow-ups*):

- The full **`ci → qa → prod` Kargo promotion chain** across the three env
  namespaces, plus **blue-green progressive delivery** (Argo Rollouts `Rollout`
  + traffic switching) — the env namespaces are scaffolded for this but no
  cross-environment promotion stages are rendered yet.
- The **external-secrets store/controller** prerequisite. ADR-21 envisioned an
  app `ExternalSecret` for runtime secret material, but the Application component
  **does not emit one today** — the platform ships no external-secrets controller
  and no `SecretStore`/`ClusterSecretStore` for it to resolve against. Standing up
  that controller and store (and then adding the `ExternalSecret` to the app
  resource set) is the deferred prerequisite; until then an app that needs runtime
  secrets is provisioned by hand.
- The self-service **`ProjectRequest` API** (ADR-1/ADR-21 left open): today a
  registration is a reviewed pull request adding a collection entry, not a
  generated-on-request tenant.

Also note: the app Quay `Repository`'s `repo_push` webhook **registration** is
omitted in the current phase (the Warehouse polls the config repo as the
fallback) until the Kargo receiver URL is published into a Secret the
`Repository` can reference.

## Apply and render-then-commit workflow

- **Render.** After editing any `.cue` file under `holos/`, run `scripts/render`.
  It renders the platform and fails if the committed `holos/deploy/` tree is not
  diff-clean. Commit the regenerated YAML under `holos/deploy/` **together with**
  the CUE source (the *CUE Component Rendering* guardrail in
  [AGENTS.md](../../AGENTS.md) and
  [component-guidelines.md](component-guidelines.md)).
- **Apply.** The project/app control-plane resources are applied by
  [`scripts/apply-projects`](../../scripts/apply-projects) (not the master
  `scripts/apply`). It injects the per-cluster local-ca PEM at apply time via the
  `ca_bundle_pem` CUE tag (so the committed tree carries **no** `caBundle`
  material — the *Runtime Secret Handling* guardrail), then applies each rendered
  project and app. Run it **after** `scripts/local-ca`, the manual Quay
  superuser-credential setup, and the platform foundation
  (`scripts/apply`) — the Argo CD/Kargo/Holos Controller must be established first.
  The workload bundle is delivered by Argo CD from the published OCI artifact.
  As of HOL-1382 the project component renders its (all control-plane) resources
  into a `control-plane/` subtree
  (`clusters/<cluster>/components/project/<name>/control-plane/`), mirroring the
  application component's control-plane/workload split.
- **Apply via the per-project App-of-Apps (the GitOps path).** Alternatively, hand
  reconciliation to Argo CD per project (HOL-1382): `scripts/apply-project-app-of-apps
  <project>` builds+pushes the project's own OCI config bundle
  (`holos/<project>-config:dev`) and applies its `<project>-control-plane` root
  (the platform-managed control plane); the service owner then applies the
  `<project>-workload` root with `scripts/apply-project-workload-app-of-apps
  <project>`. `scripts/apply-projects-app-of-apps` runs the control-plane step for
  every registered project. See
  [oci-publish-workflow.md](oci-publish-workflow.md) (*Per-project config bundles
  and the project handoff*).

## See also

- [ADR-21](../../docs/adr/ADR-21.md) — the design record (Project ≈ Namespace,
  the resource set, the worked example, the deferred follow-ons).
- [ADR-19](../../docs/adr/ADR-19.md) / [ADR-20](../../docs/adr/ADR-20.md) — the
  Quay and Keycloak API groups the templates emit CRs for.
- [ADR-1](../../docs/adr/ADR-1.md) — the GCP Project tenant model.
- [`holos/README.md`](../README.md#the-my-project-delivery-scaffold) — the
  `my-project` reference instance orientation.
- [oci-publish-workflow.md](oci-publish-workflow.md) — the publish loop the
  Warehouse/Stage participate in.
