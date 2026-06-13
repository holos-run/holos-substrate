// Package nats provides a small JetStream connection and publisher helper for
// the holos-paas services. It exposes a narrow [Publisher] interface so HTTP
// handlers can be unit-tested against a fake without a running NATS server,
// and surfaces connection state for readiness probes.
package nats

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Publisher publishes a raw message body, with the supplied NATS headers, to a
// JetStream subject and blocks until the server acknowledges it (PubAck).
//
// The interface is deliberately narrow: the webhook receiver depends only on
// this method, so tests can substitute a fake that records calls or returns an
// error. An error from Publish means the message was NOT durably stored — the
// caller must surface that to its own client (the receiver returns 503 so the
// sender retries; see the receiver package).
type Publisher interface {
	// Publish sends body to subject with hdr as the message headers and
	// returns only after a successful PubAck. A non-nil error means the
	// publish was not acknowledged and the message must be treated as lost.
	Publish(ctx context.Context, subject string, body []byte, hdr nats.Header) error
}

// ConnState reports whether the underlying NATS connection is currently
// usable, for readiness probes.
type ConnState interface {
	// Connected reports whether the client is connected to a NATS server.
	Connected() bool
}

// Conn is a JetStream-backed [Publisher] bound to a live NATS connection. The
// zero value is not usable; construct one with [Connect].
type Conn struct {
	nc *nats.Conn
	js jetstream.JetStream
}

// Connect dials the NATS server at url and initializes a JetStream context.
// The returned Conn must be closed with [Conn.Close] when no longer needed.
//
// opts are passed through to [nats.Connect]; callers typically supply a
// connection name and reconnect tuning. Connect does not block waiting for the
// server: if the initial dial fails it returns an error, but once connected the
// client transparently reconnects, and [Conn.Connected] reflects the live
// state for readiness checks.
func Connect(url string, opts ...nats.Option) (*Conn, error) {
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS at %q: %w", url, err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("initializing JetStream: %w", err)
	}
	return &Conn{nc: nc, js: js}, nil
}

// Publish implements [Publisher]. It publishes body to subject as a JetStream
// message carrying hdr and returns only after the server acknowledges it. A
// non-nil error (including the message being unacknowledged or NATS being
// disconnected) means the message was not durably stored.
func (c *Conn) Publish(ctx context.Context, subject string, body []byte, hdr nats.Header) error {
	msg := &nats.Msg{
		Subject: subject,
		Header:  hdr,
		Data:    body,
	}
	if _, err := c.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("publishing to subject %q: %w", subject, err)
	}
	return nil
}

// Connected implements [ConnState], reporting whether the client currently has
// a live connection to a NATS server.
func (c *Conn) Connected() bool {
	return c.nc != nil && c.nc.IsConnected()
}

// ConsumerConfig configures a durable pull consumer bound by [Conn.Consume].
// The defaults mirror the WEBHOOKS WorkQueue stream the subscriber drains
// (ADR-10): a named durable so redeliveries survive a restart, an explicit-ack
// policy so a message is removed from the WorkQueue only after the handler acks
// it, a filter subject scoping the consumer to the source events it parses, a
// bounded MaxDeliver so a poison message is eventually given up on rather than
// redelivered forever, and an AckWait bounding how long the handler may hold a
// message before it is redelivered.
type ConsumerConfig struct {
	// Stream is the JetStream stream to bind the consumer to, e.g. "WEBHOOKS".
	Stream string
	// Durable is the durable consumer name. A durable consumer's delivery
	// state (including per-message delivery counts) survives process restarts,
	// so MaxDeliver is enforced across restarts.
	Durable string
	// FilterSubject scopes the consumer to a subject subset of the stream,
	// e.g. "webhooks.>". Empty means all subjects on the stream.
	FilterSubject string
	// MaxDeliver bounds how many times a message is delivered before JetStream
	// stops redelivering it. Must be > 0; with explicit ack a Nak'd message is
	// redelivered until this bound, after which the handler is responsible for
	// terminating it (see the subscriber). Bounding it is what keeps a poison
	// message from wedging the WorkQueue indefinitely.
	MaxDeliver int
	// AckWait is how long the server waits for an ack before redelivering a
	// message. It bounds the handler's per-message processing budget.
	AckWait time.Duration
}

// Consumption is a running consumer started by [Conn.Consume]. Stop it with
// [Consumption.Stop]; the underlying consume loop also stops when the Conn is
// closed.
type Consumption struct {
	cc jetstream.ConsumeContext
}

// Stop ends message delivery to the handler and releases the consume loop's
// resources. It is safe to call once; further calls are no-ops.
func (c *Consumption) Stop() {
	if c.cc != nil {
		c.cc.Stop()
		c.cc = nil
	}
}

// Consume creates (or updates) the durable pull consumer described by cfg on
// its stream and starts delivering messages to handler. Each delivered
// [jetstream.Msg] is the handler's responsibility to settle exactly once via
// Ack, Nak, or Term; the helper deliberately does not auto-ack so the caller
// owns the WorkQueue semantics (ADR-10).
//
// Consume returns once the consumer is bound and delivery has started; the
// handler is invoked on background goroutines until [Consumption.Stop] is
// called or the Conn is closed. ctx bounds only the consumer
// creation/binding, not the lifetime of delivery.
func (c *Conn) Consume(ctx context.Context, cfg ConsumerConfig, handler jetstream.MessageHandler) (*Consumption, error) {
	cons, err := c.js.CreateOrUpdateConsumer(ctx, cfg.Stream, jetstream.ConsumerConfig{
		Durable:       cfg.Durable,
		FilterSubject: cfg.FilterSubject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		MaxDeliver:    cfg.MaxDeliver,
		AckWait:       cfg.AckWait,
	})
	if err != nil {
		return nil, fmt.Errorf("creating consumer %q on stream %q: %w", cfg.Durable, cfg.Stream, err)
	}
	cc, err := cons.Consume(handler)
	if err != nil {
		return nil, fmt.Errorf("starting consume loop for %q on stream %q: %w", cfg.Durable, cfg.Stream, err)
	}
	return &Consumption{cc: cc}, nil
}

// Close drains and closes the underlying NATS connection.
func (c *Conn) Close() {
	if c.nc != nil {
		c.nc.Close()
	}
}
