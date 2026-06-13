package subscriber

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
	defaultStream        = "WEBHOOKS"
	defaultDurable       = "webhook-subscriber"
	defaultFilterSubject = "webhooks.>"
	defaultMaxDeliver    = 5
	defaultAckWait       = 30 * time.Second
	shutdownTimeout      = 10 * time.Second
)

// options holds the resolved flag/env configuration for the subcommand.
type options struct {
	natsURL       string
	listenAddr    string
	stream        string
	durable       string
	filterSubject string
	maxDeliver    int64
	ackWait       time.Duration
}

// NewCommand returns the "webhook-subscriber" cobra subcommand. It connects to
// NATS, binds a durable pull consumer on the WEBHOOKS stream, drives the
// parse → DeployTask → publish loop, serves the health endpoints, and shuts
// down gracefully on SIGINT/SIGTERM.
//
// Every flag also reads a HOLOS_PAAS_* environment variable default so the
// service is configurable without changing args in a container spec.
func NewCommand() *cobra.Command {
	opts := &options{
		natsURL:       envOr("HOLOS_PAAS_NATS_URL", defaultNATSURL),
		listenAddr:    envOr("HOLOS_PAAS_LISTEN_ADDR", defaultListenAddr),
		stream:        envOr("HOLOS_PAAS_STREAM", defaultStream),
		durable:       envOr("HOLOS_PAAS_DURABLE", defaultDurable),
		filterSubject: envOr("HOLOS_PAAS_FILTER_SUBJECT", defaultFilterSubject),
		maxDeliver:    envInt64Or("HOLOS_PAAS_MAX_DELIVER", defaultMaxDeliver),
		ackWait:       envDurationOr("HOLOS_PAAS_ACK_WAIT", defaultAckWait),
	}

	cmd := &cobra.Command{
		Use:   "webhook-subscriber",
		Short: "Durable JetStream consumer that parses webhooks into DeployTasks",
		Long: "webhook-subscriber binds a durable pull consumer to the WEBHOOKS " +
			"JetStream WorkQueue stream, and for each raw webhook message derives the " +
			"source from the subject, parses it into one DeployTask per pushed tag, and " +
			"publishes each task to tasks.deploy. It acks a raw message only after every " +
			"publish is acked; a parse error or unknown source is terminated (it never " +
			"wedges the WorkQueue) and a transient publish failure is redelivered up to " +
			"MaxDeliver times. See ADR-10.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSubscriber(cmd.Context(), opts)
		},
	}

	f := cmd.Flags()
	f.StringVar(&opts.natsURL, "nats-url", opts.natsURL,
		"NATS server URL (env HOLOS_PAAS_NATS_URL)")
	f.StringVar(&opts.listenAddr, "listen-addr", opts.listenAddr,
		"health HTTP listen address (env HOLOS_PAAS_LISTEN_ADDR)")
	f.StringVar(&opts.stream, "stream", opts.stream,
		"JetStream stream to consume (env HOLOS_PAAS_STREAM)")
	f.StringVar(&opts.durable, "durable", opts.durable,
		"durable consumer name (env HOLOS_PAAS_DURABLE)")
	f.StringVar(&opts.filterSubject, "filter-subject", opts.filterSubject,
		"consumer filter subject (env HOLOS_PAAS_FILTER_SUBJECT)")
	f.Int64Var(&opts.maxDeliver, "max-deliver", opts.maxDeliver,
		"maximum redeliveries before a message is terminated (env HOLOS_PAAS_MAX_DELIVER)")
	f.DurationVar(&opts.ackWait, "ack-wait", opts.ackWait,
		"how long the server waits for an ack before redelivering (env HOLOS_PAAS_ACK_WAIT)")

	return cmd
}

// runSubscriber connects to NATS, starts the consume loop, and serves the
// health endpoints until ctx is canceled.
func runSubscriber(ctx context.Context, opts *options) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Cancel ctx on SIGINT/SIGTERM for graceful shutdown.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	conn, err := nats.Connect(opts.natsURL,
		natsgo.Name("holos-paas-webhook-subscriber"),
		natsgo.RetryOnFailedConnect(true),
		// Reconnect forever; see the receiver for the rationale (an unbounded
		// budget lets the subscriber ride out arbitrarily long NATS outages
		// rather than wedging the pod after the default budget is exhausted).
		natsgo.MaxReconnects(-1),
	)
	if err != nil {
		return err
	}
	defer conn.Close()

	consumer := New(Config{
		Publisher:  conn,
		Registry:   DefaultRegistry(),
		Logger:     log,
		MaxDeliver: int(opts.maxDeliver),
	})

	consumption, err := conn.Consume(ctx, nats.ConsumerConfig{
		Stream:        opts.stream,
		Durable:       opts.durable,
		FilterSubject: opts.filterSubject,
		MaxDeliver:    int(opts.maxDeliver),
		AckWait:       opts.ackWait,
	}, consumer.Handle)
	if err != nil {
		return err
	}
	defer consumption.Stop()

	log.Info("webhook subscriber consuming",
		"stream", opts.stream, "durable", opts.durable,
		"filter_subject", opts.filterSubject, "max_deliver", opts.maxDeliver,
		"ack_wait", opts.ackWait.String())

	return ServeHealth(ctx, opts.listenAddr, conn, shutdownTimeout, log)
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

// envDurationOr returns the time.Duration value of the environment variable key
// (parsed by time.ParseDuration, e.g. "30s"), or def when unset, empty, or
// unparseable.
func envDurationOr(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
