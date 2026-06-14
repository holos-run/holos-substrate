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
| [ADR-8](ADR-8.md) | registry, build       | Container registry and image tagging; the tag is the version      |
| [ADR-9](ADR-9.md) | webhook, nats, ingress | **Deprecated** (see ADR-16) — Thin webhook receiver posting raw bodies to a NATS WorkQueue; not used / deferred in favor of a Kargo registry watch |
| [ADR-10](ADR-10.md) | webhook, subscriber | **Deprecated** (see ADR-16) — Webhook subscriber parses events and routes render or deployer tasks by KRM match; not used / deferred in favor of Kargo |
| [ADR-11](ADR-11.md) | api, deployer, gitops | **Deprecated** (see ADR-16) — Deployer updates the Application's config-image version; not used / deferred (Kargo `argocd-update` patches `targetRevision`); Git write-back/SoD deferred |
| [ADR-12](ADR-12.md) | layout, conventions, build | Single-module monorepo layout for multiple Go services and Holos CUE |
| [ADR-13](ADR-13.md) | pipeline, mvp, nats, oci, argocd | **Deprecated** (superseded by ADR-16) — End-to-end MVP deployment flow: two registry-event loops through render and Argo CD |
| [ADR-14](ADR-14.md) | api, nats, protobuf, conventions | **Deprecated** (see ADR-16) — NATS message schemas are ConnectRPC protobuf definitions; not used / deferred (no in-cluster task subscribers under the pivot) |
| [ADR-15](ADR-15.md) | registry, oidc, security | Quay↔Keycloak OIDC SSO with PKCE: confidential client, username from the ID token, roles/groups via the `groups` claim into Quay teams |
| [ADR-16](ADR-16.md) | pipeline, kargo, oci, oras, kustomize, argocd, mvp | Kargo-driven promotion with a client-side CLI build-and-publish (ORAS) workflow; Kustomize OCI artifact, not Helm; supersedes the NATS pipeline (ADR-6, ADR-13) |

## Status values

| Status                  | Meaning                                                            |
|-------------------------|-------------------------------------------------------------------|
| `Proposed`              | Drafted and open for discussion; not yet agreed upon.             |
| `Approved`              | Agreed upon; implementation has not started or is incomplete.     |
| `Partially Implemented` | Some of the design has shipped; the rest is outstanding.          |
| `Implemented`           | The design is fully reflected in the code.                        |
| `Deprecated`            | No longer the recommended approach; kept for historical record.   |
