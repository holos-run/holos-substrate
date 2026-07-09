# Archived ADRs (PaaS era)

This folder holds the Architecture Decision Records made for the Holos PaaS
prototype — the minimum viable Heroku experience the project pursued before
the rebrand to the Holos Substrate. They were archived during that rebrand so
that [docs/adr/](../README.md) reads as the substrate's decision log.

Nothing here is deleted: the do-not-delete rule in
[writing-adrs.md](../writing-adrs.md) is honored by archiving, which preserves
the historical record. Each archived ADR carries a top-of-file blockquote
noting its archived status; its own status and revision tables are otherwise
unchanged.

Archiving records that a decision belongs to the PaaS product direction — it
does not mean the machinery the ADR describes was removed. In particular, the
Kargo/ORAS delivery workflow ([ADR-16](ADR-16.md)) and the Project/Application
CUE components ([ADR-21](ADR-21.md)) remain operational under `holos/` and are
still referenced by the operational docs; those references stay valid as
descriptions of the mechanics. Decisions the substrate carries forward are
re-recorded in active ADRs (for example [ADR-24](../ADR-24.md) builds on
ADR-21's scaffold) rather than by editing the archive.

Do not add new ADRs here and do not revise archived ones except to fix links.
New decisions go in [docs/adr/](../README.md); if a decision supersedes an
archived one, record that in the new ADR rather than editing the archive.

The active and archived ADRs are both indexed in
[docs/adr/README.md](../README.md).
