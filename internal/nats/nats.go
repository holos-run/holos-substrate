// Package nats provides a small JetStream connection and publisher helper for
// the holos-paas services. It exposes a narrow [Publisher] interface so HTTP
// handlers can be unit-tested against a fake without a running NATS server,
// and surfaces connection state for readiness probes.
package nats

import (
	"context"
	"fmt"

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

// Close drains and closes the underlying NATS connection.
func (c *Conn) Close() {
	if c.nc != nil {
		c.nc.Close()
	}
}
