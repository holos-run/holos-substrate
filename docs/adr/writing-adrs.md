# Writing ADRs

This guide explains how to write an Architecture Decision Record (ADR) for
holos-paas. The format follows the
[NATS architecture-and-design](https://github.com/nats-io/nats-architecture-and-design)
convention, scoped to ADR documents only. Read this before adding or changing an
ADR.

## What an ADR is for

An ADR captures a decision that is worth remembering: the design of an API
resource, the behavior of a reconciler, or a convention that applies across the
platform. Write one when a choice has consequences others will need to
understand later — not for routine, easily reversed decisions.

ADRs are **living documents**. When a decision evolves, prefer revising the
existing ADR and adding a row to its revision table over creating a new record.
Create a new ADR only when the decision is genuinely new, and use the `Updates`
metadata field to point back at the ADR it refines.

## How to write one

1. **Claim the next number.** ADRs are numbered sequentially. Look at the
   [index](README.md) for the highest existing number and use the next one.
2. **Create the file.** Name it `ADR-<n>.md` (e.g. `ADR-2.md`), matching the
   NATS convention.
3. **Start from the template.** Copy [adr-template.md](adr-template.md) and keep
   the metadata table and section headings intact.
4. **Fill in the metadata table.** `Date`, `Author` (one or more `@github`
   handles), `Status`, and `Tags`. Add an `Updates` row only when this ADR
   refines an earlier one; otherwise remove it.
5. **Fill in the revision table.** Start at revision 1. Every later substantive
   change gets a new row with date, author, and a short description.
6. **Write the body.** Use the standard sections: *Context and Problem
   Statement*, an optional *Context / References / Prior Work* section, *Design*,
   *Decision*, and *Consequences*. Omit sections that genuinely do not apply.
7. **Update the index.** Add a row to the table in [README.md](README.md) with
   the index link, tags, and a one-line description. The index is maintained by
   hand.

## Metadata fields

| Field      | Notes                                                                            |
|------------|----------------------------------------------------------------------------------|
| `Date`     | `YYYY-MM-DD` the ADR was first written.                                           |
| `Author`   | One or more `@github` handles.                                                    |
| `Status`   | One of the values in the [status table](README.md#status-values).                |
| `Tags`     | Comma-separated topics, e.g. `api`, `controller`, `rbac`, `webhook`.             |
| `Updates`  | `ADR-XX` when this record refines an earlier one. Remove the row otherwise.       |

## Status lifecycle

A new ADR starts as `Proposed`. Once agreed upon it becomes `Approved`, then
`Partially Implemented` and `Implemented` as the code catches up. An ADR that is
no longer the recommended approach becomes `Deprecated` but is kept for the
historical record — do not delete ADRs.

## Conventions

- One decision per ADR. If you find yourself making two unrelated decisions,
  write two ADRs.
- Keep the metadata and revision tables at the top, exactly as in the template,
  so the index and any future tooling can parse them.
- Link related ADRs to each other so the design record stays navigable.
- Record consequences honestly, including breaking changes, new RBAC
  requirements, and migration burden.
