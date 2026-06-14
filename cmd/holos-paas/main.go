// Command holos-paas is the single multi-service binary for the Holos PaaS
// (ADR-12): a cobra root command with one subcommand per service. Each service
// runs the same image with a different subcommand.
//
// The webhook-receiver (ADR-9) and webhook-subscriber (ADR-10) subcommands
// that previously realized this layout were retired in HOL-1241: Kargo plus
// the client-side ORAS publish workflow (ADR-16) now own deployment,
// superseding the deprecated NATS event-driven pipeline (ADR-9/10/11/14). The
// root command currently registers no service subcommands; the controller,
// deployer, and authproxy subcommands are added by later phases.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCommand().ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// newRootCommand builds the "holos-paas" root command. Per-service subcommands
// are registered here as services are added (see ADR-12).
func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "holos-paas",
		Short: "Holos PaaS multi-service binary",
		Long: "holos-paas is the single binary for the Holos PaaS platform services. " +
			"Each service is a subcommand running the same image with different args " +
			"(see ADR-12).",
		SilenceUsage: true,
	}
	return root
}
