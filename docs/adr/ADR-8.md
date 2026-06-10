# Container Registry and Image Tagging

| Metadata | Value                    |
|----------|--------------------------|
| Date     | 2026-06-09               |
| Author   | @jeffmccune              |
| Status   | `Proposed`               |
| Tags     | registry, build, webhook |
| Updates  | ADR-6                    |

| Revision | Date       | Author      | Info           |
|----------|------------|-------------|----------------|
| 1        | 2026-06-09 | @jeffmccune | Initial design |

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

## Design

After the reference workload is built ([ADR-7](ADR-7.md)), its image is **pushed
to a container registry with a tag**. The **tag is the unit of versioning**: the
value deployed by the pipeline is exactly the tag that was pushed. There is no
separate version concept layered on top of the image tag for the MVP.

A push of a new tag must emit a **registry webhook** to the receiver
([ADR-9](ADR-9.md)). The webhook delivery is what starts a deployment; the
registry is therefore both the artifact store and the event source.

> **Planning note for the milestone:** decide and document:
>
> - which registry the MVP uses (a local registry wired into k3d per
>   [ADR-7](ADR-7.md), and/or a hosted registry such as GHCR);
> - the tagging convention (immutable tags vs. mutable `latest`; whether the tag
>   encodes a git SHA, a semver, or a build number);
> - the registry's webhook capability and payload shape, since [ADR-10](ADR-10.md)
>   must parse it (registries differ — Docker Registry, Harbor, GHCR, etc.);
> - registry authentication for both push (build step) and pull (k3d/KubeRay).

## Decision

1. Built images are **pushed to a container registry with a tag**, and the
   **tag is the deployed version** for the MVP.
2. A new-tag push **emits a webhook** to the receiver ([ADR-9](ADR-9.md)); the
   registry is the pipeline's event source.
3. The specific registry, tagging convention, and webhook payload format are to
   be chosen in this milestone (see the planning note) because [ADR-10](ADR-10.md)
   depends on the payload shape.

## Consequences

- Versioning has a single, familiar source of truth — the image tag — which a
  coding agent can set deterministically at push time.
- The choice of registry constrains the webhook format and therefore the
  subscriber's parser ([ADR-10](ADR-10.md)); changing registries later is an
  ADR-level change, not a config tweak.
- Mutable tags (e.g. `latest`) would make "which version is deployed"
  ambiguous; the milestone should prefer immutable, content-addressable tags.
- The registry becomes a dependency for both push (CI/build) and pull
  (k3d/KubeRay); its availability gates the whole pipeline.
