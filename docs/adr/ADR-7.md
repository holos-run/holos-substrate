# KubeRay Reference Workload on k3d

| Metadata | Value                          |
| -------- | ------------------------------ |
| Date     | 2026-06-09                     |
| Author   | @jeffmccune                    |
| Status   | `Proposed`                     |
| Tags     | workload, reference-app, build |
| Updates  | ADR-6                          |

| Revision | Date       | Author      | Info           |
| -------- | ---------- | ----------- | -------------- |
| 1        | 2026-06-09 | @jeffmccune | Initial design |

## Context and Problem Statement

The MVP pipeline ([ADR-6](ADR-6.md)) needs a real workload to exercise it end to
end. A trivial "hello world" would not surface the constraints a real
application imposes — multi-stage builds, sizeable images, and a non-trivial
runtime. What workload should the MVP build and deploy, and what is the minimum
environment required to run it locally?

## Context / References

- [ADR-6 — MVP Heroku-Style Deployment Pipeline](ADR-6.md)
- [ADR-8 — Container Registry and Image Tagging](ADR-8.md) (where the built
  image is pushed)
- [KubeRay](https://github.com/ray-project/kuberay) — Kubernetes operator for
  [Ray](https://www.ray.io/)
- [k3d](https://k3d.io/) — k3s in Docker, the local cluster target

## Design

The reference workload is **KubeRay** — a Ray cluster managed by the KubeRay
operator. KubeRay was chosen because it represents a *real need* (distributed
compute) rather than a toy, and because it is non-trivial to build and run,
which makes it a credible test of the pipeline.

The local target is **k3d on a Mac with Apple Silicon (arm64)**. The image is
produced with a **multi-stage container build** so the build toolchain is kept
out of the final runtime image.

> **Planning note for the milestone:** this section is intentionally a sketch.
> The milestone work must replace it with the concrete *minimum requirements to
> host KubeRay on k3d on an Apple Silicon Mac*, including at least:
>
> - arm64 base images and the arm64 build of the Ray runtime;
> - the multi-stage `Dockerfile` (builder stage → slim runtime stage) and the
>   resulting image size budget;
> - k3d cluster shape (server/agent count, exposed ports, local registry wiring
>   per [ADR-8](ADR-8.md));
> - the KubeRay operator install method and the minimal `RayCluster` manifest;
> - CPU/memory requests that fit a developer laptop, and any GPU caveats
>   (assume CPU-only for the MVP);
> - the smoke test that proves the Ray cluster is healthy after a deploy.

## Decision

1. **KubeRay is the MVP reference workload** — it showcases a real need and is
   substantial enough to validate the pipeline.
2. The local environment is **k3d on Apple Silicon (arm64)**.
3. The image is built with a **multi-stage build** that separates build
   tooling from the runtime image.
4. The concrete minimum requirements to host KubeRay on this environment are to
   be filled in as the first task of this milestone (see the planning note).

## Consequences

- The pipeline is validated against a realistic workload from day one, so
  constraints (image size, arm64 availability, resource footprint) surface
  early rather than in production.
- Targeting Apple Silicon and k3d keeps the whole demo runnable on a single
  developer laptop, which is the intended demo environment.
- KubeRay's footprint may strain a laptop; the milestone must pin resource
  requests conservatively and may need a CPU-only configuration.
- Choosing a specialized workload means some of the build effort is
  Ray-specific; the multi-stage build pattern, not the Ray specifics, is the
  reusable outcome.
