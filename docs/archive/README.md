# Archived documents (PaaS era)

This folder holds documentation written for the Holos PaaS prototype — the
minimum viable Heroku experience the project pursued before the rebrand to
the Holos Substrate: the demo walkthroughs, the MVP milestone plan, and the
research reports behind the retired delivery pipeline. They were archived
during that rebrand so the active documentation under [docs/](../) describes
the substrate — the `quay.holos.run` and `keycloak.holos.run` custom
resources, the `security.holos.run` `ReferenceGrant`, and the Holos
Authenticator.

Nothing here is deleted: each document is preserved for the historical
record and carries a top-of-file blockquote noting its archived status.
PaaS-era Architecture Decision Records are archived separately in
[docs/adr/archive/](../adr/archive/README.md).

| Document | Description |
|----------|-------------|
| [demo-README.md](demo-README.md) | Index of the PaaS-era demo walkthroughs (formerly `docs/demo/README.md`) |
| [heroku-onramp-demo.md](heroku-onramp-demo.md) | The aspirational Heroku-style `docker push` to deploy demo (formerly `docs/demo/`) |
| [holos-paas-mvp-milestones.md](holos-paas-mvp-milestones.md) | The Holos PaaS MVP project plan and milestones (formerly `docs/planning/`) |
| [adr-1-14-vs-kargo-cli.md](adr-1-14-vs-kargo-cli.md) | Research: the custom NATS pipeline (ADR 1–14) vs. a CLI + Kargo + Argo CD (formerly `docs/research/`) |
| [rendered-manifests-publish-pipeline.md](rendered-manifests-publish-pipeline.md) | Research: the re-render + ORAS publish step in the retired event-driven pipeline (formerly `docs/research/`) |
