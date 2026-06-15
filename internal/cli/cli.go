// Package cli builds the holos-paas command-line interface.
//
// holos-paas is the single multi-service binary for the Holos PaaS (ADR-12):
// one Fisk command per service or workflow, all sharing the same image. The CLI
// is built with Fisk (github.com/choria-io/fisk, ADR-17) rather than Cobra so
// that every command, subcommand, and flag is self-documenting and emits
// LLM-friendly help for AI coding agents:
//
//	holos-paas --help-llm        # Markdown help for the whole tree
//	LLMFORMAT=1 holos-paas ...   # switch all help output to LLM format
//	holos-paas cheat deploy      # task-oriented cheat sheet
//
// New assembles the command tree; Run parses arguments and returns a process
// exit code. Keeping construction (New) and execution (Run) separate lets tests
// build the application and introspect its commands without spawning a process
// or calling os.Exit.
//
// # Adding commands and flags
//
// Every new subcommand and flag MUST be added with Fisk, in its own register*
// function called from New, following the deploy command as the template:
// a fisk help string on the command, a HelpLong block for detail, a Cheat for
// the task-oriented summary, and a clear, fully documented help string with a
// PlaceHolder on every flag. See docs/cli-guardrails.md (indexed in AGENTS.md)
// for the binding convention.
package cli

import (
	"github.com/choria-io/fisk"
)

// version is the reported build version. It is overridden at build time with
// -ldflags "-X github.com/holos-run/holos-paas/internal/cli.version=<v>".
var version = "dev"

// llmExtraInfo augments Fisk's default --help-llm preamble with project-specific
// orientation so an AI agent reading the LLM help knows where the deploy
// workflow and its guardrails are documented.
const llmExtraInfo = `holos-paas is the single multi-service binary for the Holos PaaS (ADR-12).
The "deploy" subcommand drives the demo build-and-publish workflow (ADR-16):
it publishes the rendered platform manifests as an OCI artifact that a Kargo
Warehouse promotes. New subcommands and flags must be added with Fisk; see
docs/cli-guardrails.md.`

const helpRoot = `Holos PaaS multi-service binary.

holos-paas is the single binary for the Holos PaaS platform services and demo
workflows. Each service runs the same image under a different subcommand
(ADR-12). The CLI is built with Fisk (ADR-17): pass --help-llm, or set
LLMFORMAT=1, for Markdown help formatted for AI coding agents, and run
"holos-paas cheat" for task-oriented cheat sheets.`

// New builds the holos-paas root application with every command registered. It
// is exported so tests (and future embedders) can construct the application and
// introspect its command tree without executing it.
func New() *fisk.Application {
	app := fisk.New("holos-paas", helpRoot)
	app.Version(version)
	app.Author("Holos Authors")
	app.LLMExtraInformation(llmExtraInfo)
	app.WithCheats() // registers the `holos-paas cheat` command for the cheats below.

	registerDeploy(app)

	return app
}

// exitPanic carries an exit code out of Fisk's terminate callback. Fisk's
// default terminate is os.Exit, which both reports the code and stops execution;
// a callback that merely records the code would let Parse run on past the point
// Fisk intended to exit (e.g. emitting "command not specified" after --help-llm
// already printed). Panicking reproduces os.Exit's control flow — the first
// terminate wins and unwinds — while letting Run recover the code instead of
// killing the process, which keeps the whole flow testable.
type exitPanic struct{ code int }

// Run builds the application, parses args, and returns a process exit code.
func Run(args []string) (code int) {
	app := New()
	app.Terminate(func(c int) { panic(exitPanic{c}) })
	defer func() {
		if r := recover(); r != nil {
			if ep, ok := r.(exitPanic); ok {
				code = ep.code
				return
			}
			panic(r)
		}
	}()
	app.MustParseWithUsage(args)
	return 0
}
