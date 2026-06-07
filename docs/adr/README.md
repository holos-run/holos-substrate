# Holos Controller — Architecture Decision Records

This directory holds the Architecture Decision Records (ADRs) for
holos-controller. The format follows the
[NATS architecture-and-design](https://github.com/nats-io/nats-architecture-and-design)
convention, scoped here to **ADR documents only**.

ADRs serve three purposes:

1. **Detailed design specifications** for API resources (CRDs) and their
   reconcilers.
2. **Convention guidance** that explains how and why things are done a certain
   way across the controller.
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

| Index                          | Tags             | Description                                       |
|--------------------------------|------------------|---------------------------------------------------|
| [ADR-1](ADR-1.md)              | api, controller  | Project resource (placeholder — design pending)   |

## Status values

| Status                  | Meaning                                                            |
|-------------------------|-------------------------------------------------------------------|
| `Proposed`              | Drafted and open for discussion; not yet agreed upon.             |
| `Approved`              | Agreed upon; implementation has not started or is incomplete.     |
| `Partially Implemented` | Some of the design has shipped; the rest is outstanding.          |
| `Implemented`           | The design is fully reflected in the code.                        |
| `Deprecated`            | No longer the recommended approach; kept for historical record.   |
