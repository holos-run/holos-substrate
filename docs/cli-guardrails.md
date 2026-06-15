# CLI guardrails: build the holos-paas CLI with Fisk

This is a **binding guardrail** for humans and AI coding agents working on the
holos-paas command-line interface. The decision and rationale are in
[ADR-17](adr/ADR-17.md); this document is the actionable checklist.

## Rule

All commands, subcommands, and flags in the holos-paas CLI MUST be added with
[Fisk](https://github.com/choria-io/fisk) (`github.com/choria-io/fisk`).

- **Never** reintroduce Cobra (`github.com/spf13/cobra`) or `pflag`. They were
  removed in the Fisk migration (ADR-17) and must not return to `go.mod` or any
  package.
- The CLI is built with Fisk specifically so it is legible to AI coding agents:
  `--help-llm` / `LLMFORMAT=1` emit Markdown help, `--fisk-introspect` emits a
  JSON Schema of every command's flags and arguments, and `cheat` surfaces
  task-oriented cheat sheets. Adding a command any other way breaks that
  contract.

## Where the code lives

- The command tree is assembled in `internal/cli` (all implementation lives
  under `internal/`, per [ADR-12](adr/ADR-12.md)).
  - `internal/cli/cli.go` — `New()` builds the `*fisk.Application` and calls one
    `register<Command>` function per command; `Run()` parses args and returns an
    exit code.
  - `internal/cli/deploy.go` — the `deploy` command; **use it as the template.**
- `cmd/holos-paas/main.go` is a thin entry point only:
  `os.Exit(cli.Run(os.Args[1:]))`. Do not build commands there.

## Checklist for adding a command

1. Add a `register<Command>(app *fisk.Application)` function in `internal/cli`
   (its own file for anything non-trivial) and call it from `New()`.
2. Give the command a one-line help string and a `HelpLong(...)` block
   describing what it does and how it composes with the platform.
3. Add a `Cheat("<name>", ...)` with copy-pasteable example invocations.
4. For **every** flag: a clear, complete help string and a `PlaceHolder(...)`.
   Mark required flags `Required()`; give optional flags sensible `Default(...)`
   values where one exists.
5. **Never accept secrets as flags.** Read credentials and other secrets from
   the environment so they are not exposed on the command line or in shell
   history (see `deploy`, which reads `ORAS_USERNAME` / `ORAS_PASSWORD` etc.
   from the environment).
6. Keep flag→behavior translation in a pure, testable function where practical
   (see `deployOptions.invocation`), and add tests under `internal/cli`.
7. Run `make test` (gofmt, `go vet`, race-enabled tests) before opening a PR.

## Verifying the agent-facing affordances

```bash
holos-paas --help-llm                 # Markdown help for the whole tree
LLMFORMAT=1 holos-paas deploy --help  # LLM-formatted help for one command
holos-paas --fisk-introspect          # JSON Schema of every command
holos-paas cheat                      # list cheats
holos-paas cheat deploy               # one cheat sheet
```

A new command is done only when it appears, fully documented, in `--help-llm`
and `--fisk-introspect`.
