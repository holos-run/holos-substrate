# Container Registry and Image Tagging

> **Archived (PaaS era).** This ADR records a decision made for the Holos PaaS
> prototype and was archived during the Holos Substrate rebrand. It is kept for the
> historical record; see the [active decision log](../README.md)
> for the ADRs that govern the substrate.

| Metadata | Value                                |
| -------- | ------------------------------------ |
| Date     | 2026-06-09                           |
| Author   | @jeffmccune                          |
| Status   | `Approved`                           |
| Tags     | registry, build, kargo, oci, webhook |
| Updates  | ADR-6                                |

| Revision | Date       | Author      | Info           |
| -------- | ---------- | ----------- | -------------- |
| 1        | 2026-06-09 | @jeffmccune | Initial design |
| 2        | 2026-06-14 | @jeffmccune | Revised for the [ADR-16](ADR-16.md) pivot: the registry is watched by a Kargo `Warehouse` rather than emitting a webhook to the receiver; the rendered-manifests OCI artifact is produced client-side with Kustomize + ORAS |

## Context and Problem Statement

The pipeline ([ADR-6](ADR-6.md)) is triggered by a new application version. That
version has to be named and stored somewhere the platform can pull it and react
to it. How is a built image published, and what makes a push observable to the
rest of the pipeline?

## Context / References

- [ADR-6 — MVP Heroku-Style Deployment Pipeline](ADR-6.md)
- [ADR-7 — KubeRay Reference Workload on k3d](ADR-7.md) (produces the image)
- [ADR-9 — Webhook Receiver: Thin NATS Ingress](ADR-9.md) (consumes the push
  webhook)
- [OCI Distribution Spec](https://github.com/opencontainers/distribution-spec)
- [Research: Handling Image-Tag Updates in Argo CD with an OCI Manifest Source](../../research/argocd-oci-image-tag-updates.md)
  (the two-artifact model and digest pinning)

## Design

After the reference workload is built ([ADR-7](ADR-7.md)), its image is **pushed
to a container registry with a tag**. The **tag is the unit of versioning**: the
value deployed by the pipeline is exactly the tag that was pushed. There is no
separate version concept layered on top of the image tag for the MVP.

A new tag push must be **observable to the deployment system**, which treats the
registry as both the artifact store and the event source. The two OCI artifacts
play distinct roles under the [ADR-16](ADR-16.md) pivot, so it matters *which*
push triggers *what*:

- The **application image** push is a **client-side** trigger. It does not start
  promotion on its own; the engineer (or coding agent) responds to it by running
  the client-side render-and-publish workflow ([ADR-16](ADR-16.md)):
  `holos render platform --inject` → Kustomize OCI artifact → `oras push` to the
  **rendered-manifests repository**.
- The **rendered-manifests artifact** push is what the deployment system observes
  in-cluster: a **Kargo `Warehouse` watches the rendered-manifests repository**
  and discovers new `Freight` when that artifact is published. The registry no
  longer POSTs a webhook to an in-cluster receiver. (The original design emitted a
  **registry webhook** to the thin receiver of [ADR-9](ADR-9.md) for the
  application-image push itself; that path is deprecated along with the in-cluster
  NATS pipeline — see [ADR-16](ADR-16.md).)

So a `docker push` of an app image does not by itself start a promotion under the
pivot; it prompts the client-side render that publishes the rendered-manifests
artifact, and *that* publish is what the Kargo `Warehouse` observes.

The research on Argo CD OCI delivery
([report](../../research/argocd-oci-image-tag-updates.md)) draws out a distinction
this milestone must carry forward: there are **two** OCI artifacts — the
**application image** (this ADR) and the **rendered-manifests artifact** that
Argo CD syncs ([ADR-11](ADR-11.md)). Because Holos bakes the application image
tag into the rendered manifests, the deployed version is ultimately selected by
the manifests artifact's `targetRevision`. Prefer **immutable, digest-pinned
references** so "what is deployed" is exact and auditable for both artifacts.

> **Planning note for the milestone:** decide and document:
>
> - which registry the MVP uses (a local registry wired into k3d per
>   [ADR-7](ADR-7.md), and/or a hosted registry such as GHCR);
> - the tagging convention (immutable tags vs. mutable `latest`; whether the tag
>   encodes a git SHA, a semver, or a build number);
> - the rendered-manifests repository the client publishes to and the Kargo
>   `Warehouse` watches ([ADR-16](ADR-16.md)); registry **webhook** capability is
>   no longer required for the MVP — the deprecated webhook receiver/subscriber
>   ([ADR-9](ADR-9.md), [ADR-10](ADR-10.md)) parsed registry payloads, but the
>   pivot replaces that path with a Kargo registry watch;
> - registry authentication: client **push** for the app image and the
>   rendered-manifests artifact (the build/publish step), in-cluster **read** for
>   the Kargo `Warehouse`, Argo CD **pull** for the OCI source, and pull for
>   k3d/KubeRay.

## Decision

1. Built images are **pushed to a container registry with a tag**, and the
   **tag is the deployed version** for the MVP.
2. An **application-image** push is a **client-side trigger only**: it prompts the
   engineer (or coding agent) to run the client-side render-and-publish workflow,
   which produces the **rendered-manifests OCI artifact** with **Kustomize + ORAS**
   ([ADR-16](ADR-16.md)). It is the **rendered-manifests artifact** push — not the
   app-image push — that a **Kargo `Warehouse` observes** in-cluster to drive a
   promotion. The registry is still the deployment system's event source, but the
   `Warehouse` watches the **rendered-manifests repository**, not the app-image
   repository. (The original design emitted a webhook to the thin receiver of
   [ADR-9](ADR-9.md) on the app-image push; that receiver is deprecated under the
   pivot.)
3. The specific registry and tagging convention are to be chosen in this
   milestone (see the planning note), along with the rendered-manifests repository
   the Kargo `Warehouse` watches ([ADR-16](ADR-16.md)). A registry **webhook
   payload format** is **no longer** a milestone requirement: under the pivot
   nothing parses a registry webhook — the deprecated [ADR-10](ADR-10.md)
   subscriber did, but Kargo watches the registry instead.

## Consequences

- Versioning has a single, familiar source of truth — the image tag — which a
  coding agent can set deterministically at push time.
- Under the pivot the choice of registry constrains how the Kargo `Warehouse`
  watches it ([ADR-16](ADR-16.md)) rather than a webhook parser; changing
  registries later is an ADR-level change, not a config tweak. (In the deprecated
  design the registry's webhook format constrained the subscriber's parser
  ([ADR-10](ADR-10.md)).)
- Mutable tags (e.g. `latest`) would make "which version is deployed"
  ambiguous; the milestone should prefer immutable, content-addressable tags.
- The registry becomes a dependency for both push (CI/build) and pull
  (k3d/KubeRay); its availability gates the whole pipeline.
