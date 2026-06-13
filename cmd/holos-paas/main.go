// Command holos-paas is the single multi-service binary for the Holos PaaS
// (ADR-12): a cobra root command with one subcommand per service. Each service
// runs the same image with a different subcommand. This phase wires only the
// webhook-receiver subcommand (ADR-9); the controller, subscriber, deployer,
// and authproxy subcommands are added by later phases.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/holos-run/holos-paas/internal/webhook/receiver"
	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCommand().ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// newRootCommand builds the "holos-paas" root command and registers the
// per-service subcommands.
func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "holos-paas",
		Short: "Holos PaaS multi-service binary",
		Long: "holos-paas is the single binary for the Holos PaaS platform services. " +
			"Each service is a subcommand running the same image with different args " +
			"(see ADR-12).",
		SilenceUsage: true,
	}
	root.AddCommand(receiver.NewCommand())
	return root
}
