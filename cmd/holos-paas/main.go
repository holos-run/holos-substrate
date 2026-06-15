// Command holos-paas is the single multi-service binary for the Holos PaaS
// (ADR-12): a Fisk root command with one subcommand per service or workflow.
// Each service runs the same image with a different subcommand.
//
// The CLI is built with Fisk (github.com/choria-io/fisk, ADR-17) rather than
// Cobra so that commands, subcommands, and flags are self-documenting and emit
// LLM-friendly help (`holos-paas --help-llm`) for AI coding agents. The command
// tree is assembled in the internal/cli package; this file is only the entry
// point that maps its result to a process exit code.
//
// The webhook-receiver (ADR-9) and webhook-subscriber (ADR-10) subcommands that
// previously realized the multi-service layout were retired in HOL-1241: Kargo
// plus the client-side ORAS publish workflow (ADR-16) now own deployment,
// superseding the deprecated NATS event-driven pipeline (ADR-9/10/11/14). The
// surviving workflow subcommand is "deploy" (see internal/cli); the controller,
// deployer, and authproxy service subcommands are added by later phases.
package main

import (
	"os"

	"github.com/holos-run/holos-paas/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
