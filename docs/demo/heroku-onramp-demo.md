# Heroku-Style On-Ramp to an Existing Kubernetes Platform

Turn a `docker push` into a running application on a Kubernetes platform that
already runs **ArgoCD** and is **planning to adopt Kargo** — using
[Holos](https://holos.run) and its rendered-manifests pattern as the on-ramp.
This is a self-guided tutorial you can read top to bottom; the signature moment
(push an image, get a running app) comes first, then the security and
service-management teams layer policy and dashboards on top of the same flow.

---

## How to read this doc

> [!IMPORTANT]
> **This demo describes a target experience, not shipped behavior.**
> `holos-controller` currently has **no implemented resource types** — only API
> group/scheme registration (`api/v1alpha1`). The `Project` resource at the heart
> of this on-ramp is **`Proposed`** in [ADR-1 — Project Resource](../adr/archive/ADR-1.md)
> and is not yet built. All five platform ADRs are `Proposed`, not implemented.
> Read this as the experience the controller is being built toward.

Every step is tagged so you can tell the realizable subset from the target state:

- **[Runnable today]** — works with off-the-shelf tools (Holos, ArgoCD, Kyverno,
  cosign, Grafana) on an existing cluster.
- **[Aspirational]** — depends on `holos-controller` behavior that is designed in
  the ADRs but **not yet implemented**. Do not `kubectl apply` a `Project`; the
  CRD does not exist yet.

External tools (Kyverno, cosign/sigstore, ArgoCD, Kargo, Grafana, holos-console)
evolve quickly; the snippets here are **illustrative**. Treat them as a shape to
adapt, follow the linked upstream docs for exact flags/versions, and do not
expect byte-for-byte reproducibility.

---

## The signature moment

A developer at a laptop, with a personal coding agent ([Claude
Code](https://www.claude.com/product/claude-code)) open, ships a change with two
commands:

```bash
# From the dev host, driven by Claude Code:
docker build -t registry.example.com/acme-store/checkout:$(git rev-parse --short HEAD) .
docker push  registry.example.com/acme-store/checkout:$(git rev-parse --short HEAD)
```

That's the whole developer interface. The image is tagged for the developer's
**Project** (`acme-store`), and the platform takes it from there: it renders the
app's manifests, commits them to Git, and ArgoCD syncs them — the application is
running moments later with no tickets, no YAML, and no `kubectl`. **[Aspirational]**

The rest of this tutorial shows how that on-ramp is wired, and how the security
and service-management teams add policy and visibility around it without changing
the developer's two-command experience.

---

## Audience & personas

This demo has three personas, mapped to the use cases advertised on
[holos.run](https://holos.run):

| Persona | holos.run use case | What they do here |
|---------|--------------------|-------------------|
| **Developer** | Software Developers | From Claude Code, `docker push` an image to their Project and watch it go live. Self-service, faster cycles. |
| **Security team** | Security Teams | Author policy as code — Holos CUE validators at render time **and** Kyverno + cosign at admission — and grant access by group. |
| **Service Management team** | Platform Engineers | Maintain the golden-path rendering pipeline + App-of-Apps, and expose dashboards (holos-console, ArgoCD, Grafana). Plan the Kargo promotion path. |

Holos positions itself as *"an easier way for platform teams to integrate
software into their platform."* The Developer never sees Holos directly; the
Service Management and Security teams use Holos to define the paved road the
Developer rides in on.

---

## Architecture at a glance

```
  Developer laptop                        Existing Kubernetes platform
  ┌────────────────┐
  │  Claude Code   │   docker push        ┌───────────────────────────────┐
  │  + docker CLI  │ ───────────────────► │  Container registry           │
  └────────────────┘   image tagged       │  registry.example.com/<proj>/ │
                       to a Project        └───────────────┬───────────────┘
                                                           │ webhook (image pushed)
                                                           ▼
                                          ┌───────────────────────────────┐
                                          │ holos-controller              │  [Aspirational]
                                          │ reconciles the Project / app  │
                                          └───────────────┬───────────────┘
                                                           │ triggers render
                                                           ▼
                                          ┌───────────────────────────────┐
                                          │ holos render platform         │  [Runnable today]
                                          │ Generators→Transformers→      │
                                          │ Validators ⇒ rendered YAML     │
                                          └───────────────┬───────────────┘
                                                           │ commit
                                                           ▼
                                          ┌───────────────────────────────┐
                                          │ Git (rendered manifests +      │
                                          │ ArgoCD Application resources)  │
                                          └───────────────┬───────────────┘
                                                           │ sync (App-of-Apps)
                                                           ▼
                                          ┌───────────────────────────────┐
                                          │ ArgoCD ⇒ app running           │  [Runnable today]
                                          │ (Kyverno admits / verifies)    │
                                          └───────────────────────────────┘
```

A defining property of Holos: it **stops short of applying**. `holos render
platform` produces fully-rendered, validated Kubernetes manifests and commits
them to Git; a GitOps engine (ArgoCD today, Kargo for promotion later) is what
actually reconciles them onto the cluster. See
[Holos: the rendered manifests pattern](https://holos.run/docs/).

---

## Prerequisites

> This tutorial **does not bootstrap a cluster.** It assumes you already operate a
> Kubernetes platform with ArgoCD — that is the whole point of an *on-ramp to an
> existing platform*.

You will need:

- An existing Kubernetes cluster with **ArgoCD** installed and reconciling from a
  Git repo. (Install: <https://argo-cd.readthedocs.io/>) **[Runnable today]**
- A **container registry** you can push to that can fire **push webhooks**.
- **[Holos](https://holos.run/docs/)** (`holos`), `kubectl`, and `docker` on the
  dev host. **[Runnable today]**
- **[cosign](https://docs.sigstore.dev/cosign/)** for signing images, and
  **[Kyverno](https://kyverno.io/docs/)** installed in the cluster for admission
  policy. **[Runnable today]**
- **[Claude Code](https://docs.claude.com/en/docs/claude-code/overview)** on the
  dev host as the developer's coding agent. Any agent or a plain shell works; we
  use Claude Code as the concrete example.
- For the forward-looking beats: the `holos-controller` `Project` reconcile and
  **[Kargo](https://docs.kargo.io/)** promotion are **[Aspirational]** — read,
  don't run, those sections.

---

## Part 1 — The on-ramp: push an image, get a running app

### 1a. Define the Project (the tenant) — [Aspirational]

The **Project** is the platform's tenant. Per
[ADR-1 — Project Resource](../adr/archive/ADR-1.md) it is adopted directly from the GCP
Project: the unit of ownership, isolation, access control, quotas, and chargeback
([ADR-4 — Multi-Tenancy](../adr/archive/ADR-4.md)). A developer's images are tagged
under their Project, and everything the platform does for that app is scoped to
it.

```yaml
# ASPIRATIONAL — the Project CRD is Proposed (ADR-1), not implemented.
# Shown to convey intent; do NOT kubectl apply this yet.
apiVersion: holos.run/v1alpha1
kind: Project
metadata:
  name: acme-store
spec:
  displayName: "ACME Store"
  # owners, quotas, and limits attach here (GCP-style — see ADR-5).
```

> [ADR-1](../adr/archive/ADR-1.md) deliberately **defers** whether `Project` is
> cluster-scoped or namespace-scoped, plus its full `spec`/`status` schema. This
> tutorial keeps the Project generic and does not assume a scope.

### 1b. Build and push from Claude Code — [Runnable today]

The developer's entire interface is the image push. From the coding agent:

```bash
export IMG=registry.example.com/acme-store/checkout:$(git rev-parse --short HEAD)
docker build -t "$IMG" .
docker push "$IMG"
```

The registry path encodes the Project (`acme-store`) and the app (`checkout`).
That convention is what lets the platform attribute the image to a tenant.

### 1c. What happens automatically

1. The registry fires a **push webhook**. **[Runnable today]** (registry feature)
2. `holos-controller` receives it and **reconciles the app** for that Project —
   resolving the new image tag into the Project's desired state. **[Aspirational]**
3. `holos render platform` runs the BuildPlan pipeline — **Generators →
   Transformers → Validators** — producing fully-rendered, validated manifests
   (including the app `Deployment` pinned to the pushed digest and an ArgoCD
   `Application`), and commits them to Git. **[Runnable today]**
4. **ArgoCD** detects the Git change and syncs (App-of-Apps). **[Runnable today]**

> Today you can run step 3 by hand (`holos render platform && git commit`) and let
> ArgoCD sync. The on-ramp's "magic" is step 2 — having the controller turn a bare
> `docker push` into that render+commit automatically — which is the behavior
> `holos-controller` is being built toward.

### 1d. Observe the running app — [Runnable today]

```bash
kubectl -n acme-store get deploy,pod
argocd app get acme-store-checkout
```

The app is running on the platform. The developer did nothing but `docker push`.

---

## Part 2 — Security team mixes in policy

The security team enforces the same intent in **two places** — at render time and
at admission time — so a bad change is caught early in CI/GitOps and again at the
cluster boundary (defense in depth).

### 2a. Admission policy: Kyverno + cosign — [Runnable today]

Verify that every image is signed by the platform's key, and refuse images from
untrusted registries:

```yaml
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: verify-image-signatures
spec:
  validationFailureAction: Enforce
  rules:
    - name: require-signed-images
      match:
        any:
          - resources:
              kinds: ["Pod"]
      verifyImages:
        - imageReferences:
            - "registry.example.com/*"
          attestors:
            - entries:
                - keys:
                    publicKeys: |-
                      -----BEGIN PUBLIC KEY-----
                      <platform cosign public key>
                      -----END PUBLIC KEY-----
```

The developer signs as part of the push (or CI does it):

```bash
cosign sign --key cosign.key "$IMG"
```

Pod Security and registry-allowlist policies are authored the same way — see the
[Kyverno policy library](https://kyverno.io/policies/) and
[Kyverno + sigstore](https://kyverno.io/docs/policy-types/cluster-policy/verify-images/sigstore/).

### 2b. Render-time policy: Holos CUE validators — [Runnable today]

Holos validators run inside the BuildPlan, so non-compliant config never reaches
Git. CUE is typed, so the policy is a schema, not a string match — e.g. *"reject
raw `Secret`s; require an `ExternalSecret` instead"* or *"every workload must set
resource limits."* See the
[Holos validators tutorial](https://holos.run/docs/).

### 2c. Defense in depth — [Runnable today]

Demonstrate both gates with one push:

- Push an **unsigned** image → Kyverno blocks the Pod at admission.
- Submit a manifest **missing resource limits** → the Holos validator fails the
  render before anything is committed.
- Sign the image and fix the config → the same push now flows all the way to
  running.

### 2d. Access by group — [Runnable today for RBAC; per-Project scoping Aspirational]

Per [ADR-3 — Authorization via Kubernetes RBAC and Group Membership](../adr/ADR-3.md),
the platform does **not** build a second authz system. The security team grants a
developer access to their Project by adding them to a group, which is bound to
RBAC:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: acme-store-developers
  namespace: acme-store      # scoping follows the Project impl (ADR-1, deferred)
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: project-developer
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: Group
    name: acme-store-developers
```

Binding access to the `Project` as a first-class scope is **[Aspirational]** and
follows the deferred `Project` implementation; group-to-RBAC binding itself is
standard Kubernetes today.

---

## Part 3 — Service Management team mixes in dashboards

The service-management team answers: *what's running, who owns it, is it healthy,
what does it cost?*

- **holos-console** — the Holos web console (Go + React over ConnectRPC). The
  target view lists **Projects**, their health, and **quota / chargeback** usage
  per tenant. The Project/quota/chargeback wiring is **[Aspirational]** (it
  depends on [ADR-1](../adr/archive/ADR-1.md) and
  [ADR-5 — Chargeback, Quotas, and Limits (GCP Model)](../adr/archive/ADR-5.md)).
- **ArgoCD UI** — sync status, health, and rollout history per Application.
  **[Runnable today]**
- **Grafana** — per-Project health and **cost / chargeback**, presented in the
  GCP model from [ADR-5](../adr/archive/ADR-5.md) (allocation vs. rate quotas, per-Project
  scope, adjustable defaults). Pair with OpenCost for cost attribution.
  **[Runnable today]**
- **Service catalog / ownership** — optionally surface ownership and on-call via
  [Backstage](https://backstage.io/). *(Mention; not wired in this demo.)*

The narrative: after the developer's push lands the app, the service-management
team sees it appear — healthy in ArgoCD, costed per Project in Grafana, and (as
the console matures) attributed to its tenant with live quota usage in
holos-console.

---

## Part 4 — Tailoring to the holos.run use cases

With the on-ramp established, the same machinery delivers exactly what holos.run
advertises for each persona:

- **Platform Engineers (Service Management team)** — golden paths as reusable
  **Holos Components** (wrapping Helm / Kustomize / CUE in one pipeline) and
  multi-cluster ClusterSets. Onboarding a new app/Project is adding a component,
  not hand-writing manifests. The reference example is
  [bank-of-holos](https://github.com/holos-run/bank-of-holos).
- **Security Teams** — security policy as **typed, reusable CUE** plus admission
  policy, enforced automatically on every Project (Part 2).
- **Software Developers** — self-service via `docker push`; the platform handles
  rendering, GitOps, policy, and promotion (Parts 1, 3, 5).

This is the holos.run pitch made concrete: platform and security teams define a
safe, automated integration layer once; developers ride the paved road.

---

## Part 5 — The forward path: promotion with Kargo — [Aspirational / planning]

The on-ramp lands an app in **dev**. Promotion across environments is where the
platform is *planning to adopt* [Kargo](https://docs.kargo.io/):

- A **Warehouse** watches the registry for new **Freight** (the pushed image plus
  its config).
- **Stages** (`dev → staging → prod`) subscribe to upstream stages, each with
  **verification** (health/metrics checks) and **promotion policies** (automatic
  or gated on approval).
- Result: a single, auditable view of which Freight is in which environment —
  complementing ArgoCD, which reconciles within an environment.

In the demo, narrate this as the next step *after* the image is verified in dev:
Kargo promotes the same Freight forward, applying the security team's policies at
every stage.

---

## Runnable today vs. aspirational

| Demo beat | Tool | Status |
|-----------|------|--------|
| Existing cluster + ArgoCD, registry, CLIs | platform | Runnable today |
| Define the Project (tenant) | `Project` CRD | **Aspirational** (ADR-1 Proposed) |
| `docker build` + `docker push` + `cosign sign` | docker, cosign (via Claude Code) | Runnable today |
| Registry webhook → controller reconcile | holos-controller | **Aspirational** |
| `holos render platform` → commit | Holos | Runnable today |
| ArgoCD App-of-Apps sync → app running | ArgoCD | Runnable today |
| Admission policy (signatures, registries, PSS) | Kyverno + cosign | Runnable today |
| Render-time policy | Holos CUE validators | Runnable today |
| Access by group | Kubernetes RBAC | Runnable today (per-Project scope Aspirational) |
| ArgoCD / Grafana dashboards | ArgoCD UI, Grafana | Runnable today |
| Project / quota / chargeback in console | holos-console | **Aspirational** (ADR-5) |
| Promotion dev→staging→prod | Kargo | **Aspirational / planning** |

---

## How this maps to the ADRs

This demo is a narrative realization of the platform principles. All ADRs are
currently `Proposed`.

| Demo beat | ADR | What it demonstrates |
|-----------|-----|----------------------|
| Image tagged to a Project; Project owns the app | [ADR-1 — Project Resource](../adr/archive/ADR-1.md) | The tenant model, adopted from the GCP Project (scope deferred). |
| Project, rendered manifests, ArgoCD Applications are all Kubernetes resources | [ADR-2 — Core Platform Principles](../adr/ADR-2.md) | KRM is the primary API; no bespoke interface. |
| Access granted by group → RBAC | [ADR-3 — Authorization via Kubernetes RBAC and Group Membership](../adr/ADR-3.md) | Authz reuses Kubernetes RBAC; no second system. |
| Whole flow is per-Project (isolation, ownership) | [ADR-4 — Multi-Tenancy](../adr/archive/ADR-4.md) | Multi-tenancy is first-class. |
| Per-Project cost/quota in dashboards | [ADR-5 — Chargeback, Quotas, and Limits (GCP Model)](../adr/archive/ADR-5.md) | Near-real-time chargeback and GCP-model quotas/limits. |

---

## Cleanup & next steps

- **Cleanup:** remove the demo app's ArgoCD `Application` and rendered manifests
  from Git (ArgoCD prunes the workload), delete demo Kyverno policies, and remove
  the registry webhook. No `Project` CRD exists to delete.
- **Next steps:** track the `Project` design in
  [ADR-1 — Project Resource](../adr/archive/ADR-1.md); explore the
  [Holos docs](https://holos.run/docs/) and the
  [bank-of-holos](https://github.com/holos-run/bank-of-holos) reference platform;
  and review the [Kargo docs](https://docs.kargo.io/) for the promotion path.
