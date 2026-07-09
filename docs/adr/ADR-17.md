# Fisk for the holos-paas CLI

| Metadata | Value                |
|----------|----------------------|
| Date     | 2026-06-14           |
| Author   | @jeffmccune          |
| Status   | `Deprecated`         |
| Tags     | cli, conventions, agents, build |

| Revision | Date       | Author       | Info           |
|----------|------------|--------------|----------------|
| 1        | 2026-06-14 | @jeffmccune  | Initial design |
| 2        | 2026-07-09 | @jeffmccune  | The prototype `holos-paas` binary and its Fisk CLI were removed (HOL-1541, [ADR-12](ADR-12.md) Rev 7): `cmd/holos-paas/`, `internal/cli/`, the `docs/cli-guardrails.md` guardrail this ADR references, and the `github.com/choria-io/fisk` dependency were deleted. This ADR has no remaining subject and is now `Deprecated`; it is kept for the historical record should a user-facing CLI return |

## Context and Problem Statement

The holos-paas multi-service binary (ADR-12) is a single CLI with one
subcommand per service or workflow. It was built with
[Cobra](https://github.com/spf13/cobra), the de-facto Go CLI framework. As the
platform is increasingly driven by AI coding agents, the CLI is not only a
human interface but an **agent** interface: agents discover commands, read flag
documentation, and invoke subcommands. Cobra's help is written for humans and
offers no machine-formatted introspection, so an agent must scrape free-form
`--help` text to learn the command surface.

How should the CLI be built so that commands, subcommands, and flags are
self-documenting for both humans and AI coding agents, and so that the
convention for adding new commands is unambiguous?

## Prior Work

[Fisk](https://github.com/choria-io/fisk) is R.I. Pienaar's maintained fork of
`kingpin`, used across [Choria](https://choria.io/) and the NATS CLI
(`nats`). It is the CLI toolkit behind the same NATS architecture-and-design
convention this repository's ADRs follow (see
[docs/adr/README.md](README.md)). Beyond the kingpin feature set, Fisk adds
capabilities aimed squarely at machine and agent consumption:

- **`--help-llm` / `LLMFORMAT=1`** — emit the entire command tree as Markdown
  formatted for Large Language Models, with an application-level
  `LLMExtraInformation` preamble for project-specific orientation.
- **`--fisk-introspect`** — emit a JSON Schema (and an Anthropic-restricted
  schema variant) describing each command's arguments and flags, so an agent
  can drive the CLI as a structured tool rather than by parsing prose.
- **Cheats** (`WithCheats`, `Cmd.Cheat`) — task-oriented cheat sheets surfaced
  through a `cheat` subcommand and exportable to disk.

These features make the CLI legible to agents without a separate, drift-prone
description of the command surface.

## Design

- The CLI is built with `github.com/choria-io/fisk`. Cobra (and its `pflag`
  dependency) are removed from the `go.mod` and from all packages.
- The command tree is assembled in `internal/cli` (per ADR-12, all
  implementation lives under `internal/`). `cli.New` constructs the
  `*fisk.Application`; `cli.Run` parses arguments and returns a process exit
  code. `cmd/holos-paas/main.go` is a thin entry point: `os.Exit(cli.Run(os.Args[1:]))`.
- Keeping construction (`New`) separate from execution (`Run`) lets tests build
  the application and introspect its commands without spawning a process or
  calling `os.Exit`. `Run` installs a `Terminate` callback that unwinds via
  panic/recover so the first termination wins (reproducing `os.Exit`'s control
  flow) while remaining testable.
- The application enables `--help-llm`, `LLMExtraInformation`, and `WithCheats`
  so the agent-facing affordances above are always available.
- Each command is registered by its own `register<Command>` function called
  from `New`. The first such command is `deploy` (see below); it is the
  template for every future command. Service subcommands (controller, deployer,
  authproxy) are added by later phases the same way.

### The `deploy` subcommand

`deploy` drives the demo build-and-publish workflow (ADR-16): it publishes the
rendered platform manifests as an OCI artifact that a Kargo `Warehouse`
promotes. It is a **thin, fully documented front end over `scripts/publish`**,
the canonical client-side publish workflow, so the shell workflow remains the
single source of truth rather than being re-implemented in Go. The command:

- resolves the repository root by walking up from the working directory for
  `scripts/publish` (overridable with `--repo-root` / `--publish-script`);
- maps each documented flag to `scripts/publish`'s positional arguments and
  environment overrides (`--app-image`, `--manifests-repo`, `--force-push` →
  `FORCE_PUSH`, `--insecure` → `ORAS_INSECURE`, etc.);
- reads registry credentials from the environment, **never from flags**, so
  secrets are not exposed on the command line or in shell history; and
- supports `--dry-run` to print the resolved `scripts/publish` invocation
  without executing it.

The flag-to-invocation translation is a pure function, unit-tested without
touching a registry.

## Decision

Adopt Fisk as the CLI framework for holos-paas, primarily because it integrates
cleanly with AI coding agents (`--help-llm`, `--fisk-introspect`, cheats) while
remaining a conventional, well-documented human CLI. Remove Cobra.

Every new subcommand and flag MUST be added with Fisk, following the `deploy`
command as the template: a help string on the command, a `HelpLong` block for
detail, a `Cheat` for the task-oriented summary, and a documented help string
with a `PlaceHolder` on every flag. This convention was recorded as a guardrail
in `docs/cli-guardrails.md` and indexed in
[AGENTS.md](../../AGENTS.md) so agents apply it. (Both the CLI and the
guardrail document were removed in HOL-1541 — see Revision 2 above.)

## Consequences

- **Dependency change.** `github.com/spf13/cobra` and `github.com/spf13/pflag`
  are replaced by `github.com/choria-io/fisk`. A one-time migration of any
  Cobra-specific code is required; the current CLI had no service subcommands
  yet, so the migration surface was the root command only.
- **New convention, enforced socially.** The guardrail in
  `docs/cli-guardrails.md` (indexed in AGENTS.md) bound humans and agents to add
  commands and flags with Fisk. There was no compiler enforcement; reviewers and
  the agent guardrails carried it. (The guardrail document was removed with the
  CLI in HOL-1541.)
- **Agent legibility.** The CLI now exposes machine-formatted help and JSON
  Schema introspection, so agents can drive it as a structured tool.
- **`deploy` depends on `scripts/publish`.** The subcommand intentionally shells
  out to the canonical publish script rather than re-implementing the
  holos/kustomize/oras pipeline in Go, keeping one source of truth. The binary
  is therefore not self-contained for `deploy`; it must run where
  `scripts/publish` and its prerequisites (holos, kustomize, oras) are
  available. This is acceptable for the client-side demo workflow (ADR-16).
