package receiver

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/holos-run/holos-paas/internal/nats"
	natsgo "github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

// Default configuration values. The NATS URL default targets the in-cluster
// JetStream service (see the holos/components/nats component and holos/README).
const (
	defaultNATSURL       = "nats://nats.nats.svc.cluster.local:4222"
	defaultListenAddr    = ":8080"
	defaultSubjectPrefix = "webhooks"
	defaultMaxBodyBytes  = 1 << 20 // 1 MiB
	shutdownTimeout      = 10 * time.Second
)

// options holds the resolved flag/env configuration for the subcommand.
type options struct {
	natsURL       string
	listenAddr    string
	subjectPrefix string
	maxBodyBytes  int64
}

// NewCommand returns the "webhook-receiver" cobra subcommand. It connects to
// NATS, serves the receiver HTTP handler, and shuts down gracefully on
// SIGINT/SIGTERM.
//
// Every flag also reads a HOLOS_PAAS_* environment variable default so the
// service is configurable without changing args in a container spec.
func NewCommand() *cobra.Command {
	opts := &options{
		natsURL:       envOr("HOLOS_PAAS_NATS_URL", defaultNATSURL),
		listenAddr:    envOr("HOLOS_PAAS_LISTEN_ADDR", defaultListenAddr),
		subjectPrefix: envOr("HOLOS_PAAS_SUBJECT_PREFIX", defaultSubjectPrefix),
		maxBodyBytes:  envInt64Or("HOLOS_PAAS_MAX_BODY_BYTES", defaultMaxBodyBytes),
	}

	cmd := &cobra.Command{
		Use:   "webhook-receiver",
		Short: "Thin HTTP ingress that publishes raw webhook bodies to NATS JetStream",
		Long: "webhook-receiver accepts POST /webhooks/{source}, publishes the raw, " +
			"unmodified request body to the NATS subject <prefix>.<source> on the " +
			"WEBHOOKS JetStream WorkQueue stream, and returns 202 Accepted only after " +
			"the publish is acked (503 if NATS is unavailable). It performs no payload " +
			"parsing; see ADR-9.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReceiver(cmd.Context(), opts)
		},
	}

	f := cmd.Flags()
	f.StringVar(&opts.natsURL, "nats-url", opts.natsURL,
		"NATS server URL (env HOLOS_PAAS_NATS_URL)")
	f.StringVar(&opts.listenAddr, "listen-addr", opts.listenAddr,
		"HTTP listen address (env HOLOS_PAAS_LISTEN_ADDR)")
	f.StringVar(&opts.subjectPrefix, "subject-prefix", opts.subjectPrefix,
		"NATS subject prefix; the publish subject is <prefix>.<source> (env HOLOS_PAAS_SUBJECT_PREFIX)")
	f.Int64Var(&opts.maxBodyBytes, "max-body-bytes", opts.maxBodyBytes,
		"maximum request body size in bytes; larger bodies get 413 (env HOLOS_PAAS_MAX_BODY_BYTES)")

	return cmd
}

// runReceiver connects to NATS and serves the receiver until ctx is canceled.
func runReceiver(ctx context.Context, opts *options) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Cancel ctx on SIGINT/SIGTERM for graceful shutdown.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	conn, err := nats.Connect(opts.natsURL,
		natsgo.Name("holos-paas-webhook-receiver"),
		natsgo.RetryOnFailedConnect(true),
	)
	if err != nil {
		return err
	}
	defer conn.Close()

	h := New(Config{
		Publisher:     conn,
		ConnState:     conn,
		SubjectPrefix: opts.subjectPrefix,
		MaxBodyBytes:  opts.maxBodyBytes,
		Logger:        log,
	})

	return h.Serve(ctx, opts.listenAddr, shutdownTimeout)
}

// envOr returns the value of the environment variable key, or def when unset or
// empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt64Or returns the int64 value of the environment variable key, or def
// when unset, empty, or unparseable.
func envInt64Or(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}
