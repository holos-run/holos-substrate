# The Holos Controller and the GitOps Rendered-Manifest Delivery Model

| Metadata | Value                   |
| -------- | ----------------------- |
| Date     | 2026-06-17              |
| Author   | @jeffmccune             |
| Status   | `Partially Implemented` |
| Tags     | controller, api, gitops |
| Updates  | ADR-12, ADR-15          |

| Revision | Date       | Author      | Info           |
| -------- | ---------- | ----------- | -------------- |
| 1        | 2026-06-17 | @jeffmccune | Initial design |
| 2        | 2026-06-18 | @jeffmccune | The controller shipped (`holos-controller`, HOL-1309..HOL-1313) with its first API group `quay.holos.run` ([ADR-19](ADR-19.md), now `Implemented`). Record the **AC #7 API-group dependency boundary** (authoritative per HOL-1308 AC #10, refining the GitOps framing below): the `quay.holos.run` **API group / CRs** take **no dependency on Kargo or Argo CD** (only the Quay API + the credential `secretRef`), while the **controller binary may**. Confirm the install namespace remains `holos-controller` (no `quay-resource-controller` namespace anywhere) and that the Organization does not inline repositories (AC #9). Status `Proposed` → `Partially Implemented` (Quay group shipped; Keycloak group ADR-20 still proposed) |
| 3        | 2026-06-18 | @jeffmccune | Record the controller-wide **CA-bundle convention** (HOL-1319/HOL-1320/HOL-1321): every CRD Kind that talks to a TLS endpoint accepts a standardized `caBundle []byte` spec field (PEM/base64, empty ⇒ pod system trust), threaded into the client's TLS `RootCAs`. The first instance ships on the `quay.holos.run` Organization and Repository ([ADR-19](ADR-19.md) Revision 5) for the in-cluster Quay registry's local-CA serving cert. |
| 4        | 2026-07-09 | @jeffmccune | Holos Substrate rebrand (HOL-1546): prototype-era product framing in the prose is reworded to the platform/substrate framing. No decision change. |

## Context and Problem Statement

The platform targets a self-service "docker push to deploy" experience for
product engineers: push a tagged image, get a deploy, with everything managed
through the Kubernetes API ([ADR-2](ADR-2.md)). Delivering that experience
requires more capabilities than the upstream operators the platform already runs
provide. The Quay and Keycloak operators install and manage their respective
applications well, but neither offers a Kubernetes-native, declarative API for
the **tenant-facing data-plane objects** the platform needs to provision on a
project's behalf — Quay organizations, repositories, robots, and webhooks;
Keycloak clients, roles, and groups. Today those gaps are filled by manual,
imperative procedures: an operator signs in and clicks through a UI, or runs a
documented one-off bootstrap. The clearest example is the Quay data plane, whose
provisioning was, at the time this ADR was written, **deferred to a "future Quay
Resource Controller"** and performed by hand against a manually-minted superuser
credential ([ADR-15](ADR-15.md) Revisions 4–5;
[Quay Resource Controller credentials runbook](../runbooks/quay-resource-controller-credentials.md)).
This ADR's controller — now shipped (Revision 2) — is that "future Quay Resource
Controller"; the Quay org/repo/webhook surface it reconciles is described below
and specified in [ADR-19](ADR-19.md).

Two questions follow. First, **what supplies the missing Kubernetes-native
APIs** so that platform capabilities the upstream operators do not cover are
modeled as custom resources with reconcilers, per [ADR-2](ADR-2.md), rather than
imperative tools or runbooks? Second, **how is the developer experience
delivered** end to end — how do the resources that drive a project's deployment
reach the cluster? This ADR records the foundational decisions that answer both
and that the later, more detailed ADRs build on.

## Context / References

- [ADR-2 — Core Platform Principles](ADR-2.md): the platform is
  Kubernetes-native; the KRM (the Kubernetes API plus the CRDs the platform
  installs) is the primary API, and capabilities are expressed as custom
  resources reconciled by controllers — not imperative tools. This ADR applies
  that principle to the gaps the upstream operators leave open.
- [ADR-12 — Repository Layout for Multiple Go Services](ADR-12.md): establishes
  the kubebuilder multi-group layout (`api/<group>/<version>`) "multi-group from
  day one," with `registry.holos.run` named as the
  illustrative registry-related group. This ADR **refines** that registry
  example: the first **controller** API group is the concrete `quay.holos.run`
  (Quay organizations and repositories), which is the registry data plane ADR-12
  sketched generically. The `<group>.holos.run` convention is unchanged.
- [ADR-15 — Quay↔Keycloak OIDC SSO](ADR-15.md), Revisions 4 and 5: records the
  manual stop-gap this controller replaces. Revision 4 (HOL-1293) establishes the
  OIDC backend — the Keycloak realm is Quay's sole identity store, there is no
  local `admin`, and in-cluster Quay data-plane provisioning (orgs, repos, robots,
  webhooks) is **deferred to a future Quay Resource Controller**. Revision 5
  (HOL-1299) is the current revision and materially shapes that controller's
  credential model: it enables `FEATURE_SUPERUSERS_FULL_ACCESS` so the controller
  can **adopt** orgs it did not create, and clarifies the user/org/OAuth-Application
  distinction and the manual `platform-automation` org bootstrap. The controller
  this ADR names is that "future Quay Resource Controller."
- [ADR-16 — Kargo-Driven Promotion with a Client-Side CLI Build-and-Publish
  (ORAS) Workflow](archive/ADR-16.md): the deployment half of the developer experience —
  Holos renders the platform, the rendered manifests are packaged as an OCI
  artifact, Kargo promotes, and Argo CD syncs. This ADR records the
  rendered-manifest GitOps model that ADR-16's pipeline operates within, and the
  controller supplies the CRDs those rendered manifests reference.
- [Quay Resource Controller credentials runbook](../runbooks/quay-resource-controller-credentials.md):
  the manual OAuth-Application credential procedure. The shipped controller
  automates the **org/repo/webhook** provisioning the runbook performed by hand,
  but still **consumes** the superuser credential the runbook mints (read from the
  `holos-controller-quay-creds` Secret in `holos-controller`), so the credential
  mint itself stays a manual step by design.
- Forward references (later phases, detailed specifications):
  [**ADR-19**](ADR-19.md) — the Quay API group (`quay.holos.run`) Organization
  and Repository CRDs; [**ADR-20**](ADR-20.md) — the Keycloak API group CRDs
  (OIDC clients, client/realm roles, groups); **ADR-21** — the Holos
  Project/Application delivery components. This ADR stays at the system-design
  altitude and does not specify their CRD schemas.

## Design

### The Holos Controller

The platform gains an in-cluster controller — the **Holos Controller** — that
runs in its own namespace, **`holos-controller`**. Its job is to **reconcile the
custom resources that fill the gaps the upstream operators leave open**, so that
platform capabilities those operators do not cover become first-class
Kubernetes-native APIs rather than manual procedures. Concretely, the gaps it
closes are:

- **Quay data plane** — organizations (with their OIDC-synced teams and org
  default repository permissions) and repositories (with an optional `repo_push`
  webhook on a repository). The upstream Quay operator deploys and manages Quay
  itself but offers no CRD for these tenant-facing objects. The shipped
  `quay.holos.run` group ([ADR-19](ADR-19.md)) models an **Organization** (whose
  `spec.syncedTeams` declares OIDC group→Quay-team/role bindings, ADR-19
  Revision 6) and a **Repository**; **repositories are provisioned only via the
  Repository resource — the Organization never inlines them** (AC #9).
- **Keycloak data plane** — clients, roles, and groups. The upstream Keycloak
  operator and the declarative `keycloak-config-cli` reconciliation manage the
  **platform's own** realm configuration today
  ([holos/components/keycloak/realm-config/buildplan.cue](../../holos/components/keycloak/realm-config/buildplan.cue));
  what is missing is a KRM-native API for the **per-project, tenant-facing**
  identity wiring the platform provisions on a project's behalf. The exact ownership
  boundary between the controller's Keycloak group and the existing
  `keycloak-config-cli` Job — so the two reconcilers never fight over the same
  realm objects — is a question [**ADR-20**](ADR-20.md) must resolve; this ADR
  only records that the gap exists and is the controller's to close. (Until
  ADR-20 settles
  that boundary, `keycloak-config-cli` remains the sole owner of realm clients,
  roles, and groups, per the platform's Keycloak guardrails.)

The controller follows the platform's **`<group>.holos.run` API-group
convention** established in [ADR-12](ADR-12.md) (the kubebuilder multi-group
`api/<group>/<version>` layout): each gap area is a versioned API group under the
`holos.run` domain. The **first controller group is `quay.holos.run`** (the Quay
organization and repository resources, specified in ADR-19) — the concrete
registry data plane ADR-12 named generically as `registry.holos.run`; this ADR
refines that example to the controller's actual first group. The Keycloak group
follows in a later phase (ADR-20). One controller hosts these groups'
reconcilers; the API groups grow independently as gaps are closed.

Naming the controller and its first API group here — without specifying the CRD
schemas — is deliberate. The schemas are detailed design that belongs in the
per-group ADRs (ADR-19, ADR-20); this ADR fixes the decisions those ADRs depend
on: that there **is** a controller, where it runs, what it is for, and the
API-group naming convention.

### API-group dependency boundary (AC #7, Revision 2)

The Holos Controller parent issue's acceptance criteria are authoritative
(HOL-1308 AC #10), and AC #7 refines this ADR's GitOps framing with a structural
rule that the shipped controller observes:

- **The controller's API groups (`api/<group>/...`) take no dependency on Kargo
  or Argo CD.** For the first group, `quay.holos.run` (specified in
  [ADR-19](ADR-19.md)), the custom resources depend **only** on the Quay API —
  reached through a credential `secretRef` — and never `import` a Kargo
  `ProjectConfig`, an Argo CD `Application`, or any Quay/Kargo/Argo CD Go type.
  A resource that must carry a pipeline value (e.g. Kargo's hard-to-guess
  `repo_push` webhook receiver URL) takes it as **data** — an inline value or a
  `secretRef` — not as a typed reference to a pipeline object.
- **The controller *binary* may depend on Kargo/Argo CD** if a future need
  arises; that dependency lives in `cmd/holos-controller` / `internal/controller`,
  never in the `api/` packages.

This keeps the API packages extractable into their own module (per
[ADR-12](ADR-12.md)) and the data-plane CRs legible independent of the delivery
pipeline. It **refines** the "rendered manifests reference the controller's CRDs,
Kargo/Argo CD deliver them" framing below: the delivery pipeline operates *around*
the CRs (rendering and syncing them); the CR **types** stay decoupled from it.

### CA-bundle convention (Revision 3)

The controller reaches the external systems it provisions (the in-cluster Quay
registry today; Keycloak later) over TLS, and those endpoints are served by the
platform's **per-cluster mkcert local CA**, not a public root. That CA is not in
the controller pod's system trust store, so the reconcilers' first calls failed
with `x509: certificate signed by unknown authority` (HOL-1319). The endpoint's
trust anchor is **per-cluster configuration**, not something baked into the
controller image, so each resource that talks to such an endpoint must be able to
supply it.

Therefore, as a controller-wide convention:

- **Every CRD Kind in the controller's API groups that establishes TLS to an
  external endpoint accepts a `caBundle` spec field of one standardized shape** —
  a Go `[]byte` (JSON tag `caBundle,omitempty`, `+optional`) holding PEM-encoded
  x509 CA certificates, serialized as a single base64 string (CRD property
  `type: string, format: byte`), following the upstream Kubernetes `caBundle`
  convention. An **empty/omitted** value means "use the controller pod's system
  trust store unchanged," so the field is purely additive.
- **The field is configuration, not a credential.** It rides on the resource
  spec, distinct from the credential `secretRef`, and the reconciler **threads it
  into the client's TLS `RootCAs`** (the system pool with the bundle appended) so
  the controller trusts the endpoint's local CA.
- **The shape is described once and re-used, not redefined per Kind.** Within an
  API group the shared semantics/format live in one place (for `quay.holos.run`,
  `api/quay/v1alpha1/common_types.go`) and each Kind's field refers back to it.

The **first instance** of this convention is the `quay.holos.run` group: both the
Organization and Repository carry the identical `caBundle` field, threaded into
the Quay client's TLS trust for the in-cluster registry. The field shape, its
consumption, and the worked example are specified in [ADR-19](ADR-19.md)
(Revision 5, *CA bundle*). Future Kinds (e.g. the Keycloak group, ADR-20) follow
the same convention when they reach a TLS endpoint served by the local CA.

### Delivery: the GitOps rendered-manifest pattern

The developer experience is delivered via the **GitOps rendered-manifest
pattern**, the same pattern the rest of the platform already uses:

1. **Holos renders CUE collections to manifests.** The platform's deployment
   configuration is authored as Holos CUE and rendered with
   `holos render platform` to fully-resolved Kubernetes YAML — the rendered
   manifests.
2. **Argo CD syncs the rendered manifests.** Those manifests are delivered to
   the cluster through Argo CD, packaged and promoted by the
   [ADR-16](archive/ADR-16.md) pipeline (a Kustomize-built OCI artifact, watched by Kargo,
   pinned by digest in the Argo CD `Application`'s `targetRevision`).
3. **The controller's CRDs are referenced by those manifests and reconciled
   in-cluster.** A project's rendered manifests include custom resources from the
   `holos.run` API groups (e.g., a `quay.holos.run` Organization). The Holos
   Controller reconciles them, provisioning the Quay/Keycloak data-plane objects
   the rendered manifests declare. Rendering produces the desired state; the
   controller makes it real.

This is the model **to start**. It composes with the established
rendered-manifests delivery path rather than introducing a parallel mechanism: a
project's identity and registry wiring become declarative resources in the same
rendered output that already carries its workloads, and an in-cluster reconciler
closes the loop. Stating it as the starting model leaves room for the experience
to evolve — for example, a self-service request API that generates the same
resources — without reopening this decision.

### Relationship to ADR-2 and the manual stop-gap

This ADR is a direct application of [ADR-2](ADR-2.md): the gaps the upstream
operators leave open are closed by **CRDs plus reconcilers, not imperative
tools**. The Holos Controller is the reconciler; the `holos.run` API groups are
the CRDs. Where the platform today documents a human-run procedure to fill a gap,
the KRM-native answer is a custom resource the controller reconciles.

The clearest worked example is Quay. [ADR-15](ADR-15.md) Revisions 4–5 and the
[Quay Resource Controller credentials runbook](../runbooks/quay-resource-controller-credentials.md)
record a deliberate manual stop-gap: because Quay runs `AUTHENTICATION_TYPE: OIDC`
with the Keycloak realm as the sole identity store, there is no headless way to
mint the first superuser token, so an operator signs in as the
`svc-quay-resource-controller` realm user, creates an OAuth Application in the
`platform-automation` org, and provisions orgs/repos/robots/webhooks by hand.
Revision 5's `FEATURE_SUPERUSERS_FULL_ACCESS` is what lets that credential
**adopt** and reconcile orgs the controller did not itself create — a property
the controller's reconcilers depend on. The runbook names the credential's
consumer the **"future Quay Resource Controller"** — and **that controller is the
Holos Controller defined here**. The `quay.holos.run` API group (ADR-19) replaces
the hand-run provisioning with reconciled resources; the manually-minted
OAuth-Application credential becomes the controller's service-account credential.
This ADR **supersedes the manual stop-gap as the intended end state**: ADR-15
Revisions 4–5 and the runbook remain the record of how the gap is filled until the
controller ships, and the controller is what closes it.

## Decision

1. **The platform gains the Holos Controller**, an in-cluster controller running
   in the **`holos-controller`** namespace, whose purpose is to reconcile the
   custom resources that fill the data-plane gaps the upstream Quay and Keycloak
   operators leave open.
2. **The controller's APIs follow the `<group>.holos.run` convention.** The
   first API group is **`quay.holos.run`** (Quay organizations and repositories,
   specified in ADR-19); the Keycloak group (clients, roles, groups) follows in
   ADR-20. This ADR names the controller and the convention; it does **not**
   specify the CRD schemas.
   - **API-group dependency boundary (AC #7, Revision 2).** Each controller API
     group (`api/<group>/...`) imports **no** Kargo or Argo CD types — the
     `quay.holos.run` CRs depend only on the Quay API (via a credential
     `secretRef`) and carry any pipeline value (e.g. a Kargo webhook URL) as
     data. The controller **binary** may depend on Kargo/Argo CD; the `api/`
     packages must not.
   - **Repositories only via the Repository resource (AC #9).** The Organization
     resource never inlines repository creation.
3. **The developer experience is delivered via the GitOps rendered-manifest
   pattern, to start:** Holos renders CUE collections to manifests, Argo CD syncs
   them (via the [ADR-16](archive/ADR-16.md) pipeline), and the controller's CRDs —
   referenced by those rendered manifests — are reconciled in-cluster.
4. **This controller is the "future Quay Resource Controller"** referenced by
   [ADR-15](ADR-15.md) Revisions 4–5 and the
   [Quay Resource Controller credentials runbook](../runbooks/quay-resource-controller-credentials.md).
   It **supersedes that manual stop-gap** as the intended end state: the runbook's
   by-hand provisioning becomes reconciled `quay.holos.run` resources, and the
   manually-minted OAuth-Application credential becomes the controller's
   service-account credential.
5. **The system-design decisions above were the original phase's scope; the
   controller has since shipped (Revision 2).** This ADR fixed the controller,
   its namespace, and the API-group convention without specifying CRD schemas.
   The first group's schemas (ADR-19) are now `Implemented` (the controller
   shipped across HOL-1309..HOL-1313); the Keycloak group (ADR-20) and the
   Project/Application component model (ADR-21) remain later phases that build on
   these decisions.

## Consequences

- **A new namespace and a new operational dependency.** The platform gains the
  `holos-controller` namespace and a controller that must be deployed, operated,
  upgraded, and monitored like any other in-cluster component. It becomes a
  dependency of the self-service data-plane provisioning the platform offers.
- **New RBAC for the controller's service account.** The controller reconciles
  cluster resources and acts against Quay and Keycloak, so it needs a
  service account with RBAC scoped to the `holos.run` API groups it owns, plus
  the external credentials to call Quay/Keycloak (for Quay, the superuser
  OAuth-Application credential the runbook currently mints by hand, carrying the
  Revision-5 `FEATURE_SUPERUSERS_FULL_ACCESS` reach — see ADR-15 Revisions 4–5).
  The specific permission sets are detailed in the per-group ADRs.
- **The manual stop-gap is retired now the controller has shipped (Revision 2)** —
  for the **org/repo/webhook** data plane. [ADR-15](ADR-15.md) Revisions 4–5 and
  the [credentials runbook](../runbooks/quay-resource-controller-credentials.md)
  become the historical record for those by-hand steps, which are now reconciled
  `quay.holos.run` resources ([ADR-19](ADR-19.md)). The runbook stays operative
  for the parts the controller does **not** automate: minting the superuser
  credential the controller consumes, and (until later phases) the robots and
  pull-credential Secrets. Operators trade most of the documented manual procedure
  for a controller they must keep healthy.
- **The KRM-native principle is reinforced.** Closing these gaps with CRDs plus a
  reconciler — rather than expanding the set of manual runbooks — keeps the
  platform's capabilities expressible and auditable through the Kubernetes API
  ([ADR-2](ADR-2.md)), at the cost of the bespoke controller code the per-group
  ADRs introduce.
- **Foundation for later phases.** This ADR is the keystone the Quay-CRD
  (ADR-19), Keycloak-CRD (ADR-20), and Project/Application component (ADR-21)
  ADRs reference. Changing the controller's name, namespace, or API-group
  convention is an ADR-level change that ripples into those records.
