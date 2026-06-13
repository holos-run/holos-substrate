package nats

import (
	"context"
	"testing"
	"time"

	natssrv "github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// TestConsume binds a durable consumer to a WorkQueue stream, publishes a
// message to a filtered subject, and asserts the handler receives it and that
// an Ack removes it from the WorkQueue.
func TestConsume(t *testing.T) {
	srv := runEmbeddedJetStream(t)
	defer srv.Shutdown()
	url := srv.ClientURL()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, err := Connect(url)
	if err != nil {
		t.Fatalf("connecting to embedded NATS: %v", err)
	}
	defer conn.Close()

	js := newJetStream(t, url)
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "WEBHOOKS",
		Subjects:  []string{"webhooks.>"},
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.FileStorage,
	}); err != nil {
		t.Fatalf("creating WEBHOOKS stream: %v", err)
	}

	// Publish a message the consumer's filter subject should match.
	if err := conn.Publish(ctx, "webhooks.quay", []byte("hello"), nil); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	received := make(chan jetstream.Msg, 1)
	consumption, err := conn.Consume(ctx, ConsumerConfig{
		Stream:        "WEBHOOKS",
		Durable:       "test-consumer",
		FilterSubject: "webhooks.>",
		MaxDeliver:    5,
		AckWait:       2 * time.Second,
	}, func(msg jetstream.Msg) {
		_ = msg.Ack()
		received <- msg
	})
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	defer consumption.Stop()

	select {
	case msg := <-received:
		if msg.Subject() != "webhooks.quay" {
			t.Errorf("subject = %q, want %q", msg.Subject(), "webhooks.quay")
		}
		if string(msg.Data()) != "hello" {
			t.Errorf("data = %q, want %q", msg.Data(), "hello")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the consumer to receive the message")
	}

	// After the Ack settles, the WorkQueue stream should be empty.
	if err := eventually(5*time.Second, func() bool {
		info, err := js.Stream(ctx, "WEBHOOKS")
		if err != nil {
			return false
		}
		s, err := info.Info(ctx)
		if err != nil {
			return false
		}
		return s.State.Msgs == 0
	}); err != nil {
		t.Fatalf("acked message was not removed from the WorkQueue: %v", err)
	}
}

// TestConsumeConfiguresConsumer asserts the helper binds a durable consumer
// with the explicit-ack policy, filter subject, MaxDeliver, and AckWait it was
// configured with.
func TestConsumeConfiguresConsumer(t *testing.T) {
	srv := runEmbeddedJetStream(t)
	defer srv.Shutdown()
	url := srv.ClientURL()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, err := Connect(url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer conn.Close()

	js := newJetStream(t, url)
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "WEBHOOKS",
		Subjects:  []string{"webhooks.>"},
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.FileStorage,
	}); err != nil {
		t.Fatalf("creating stream: %v", err)
	}

	consumption, err := conn.Consume(ctx, ConsumerConfig{
		Stream:        "WEBHOOKS",
		Durable:       "config-consumer",
		FilterSubject: "webhooks.>",
		MaxDeliver:    7,
		AckWait:       3 * time.Second,
	}, func(msg jetstream.Msg) { _ = msg.Ack() })
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	defer consumption.Stop()

	cons, err := js.Consumer(ctx, "WEBHOOKS", "config-consumer")
	if err != nil {
		t.Fatalf("looking up consumer: %v", err)
	}
	cfg := cons.CachedInfo().Config
	if cfg.AckPolicy != jetstream.AckExplicitPolicy {
		t.Errorf("AckPolicy = %v, want AckExplicitPolicy", cfg.AckPolicy)
	}
	if cfg.FilterSubject != "webhooks.>" {
		t.Errorf("FilterSubject = %q, want %q", cfg.FilterSubject, "webhooks.>")
	}
	if cfg.MaxDeliver != 7 {
		t.Errorf("MaxDeliver = %d, want 7", cfg.MaxDeliver)
	}
	if cfg.AckWait != 3*time.Second {
		t.Errorf("AckWait = %v, want 3s", cfg.AckWait)
	}
}

// TestConsumptionStopIsIdempotent verifies Stop can be called more than once.
func TestConsumptionStopIsIdempotent(t *testing.T) {
	c := &Consumption{}
	c.Stop()
	c.Stop() // must not panic on a nil ConsumeContext
}

func newJetStream(t *testing.T, url string) jetstream.JetStream {
	t.Helper()
	nc, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connecting raw client: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return js
}

// eventually polls cond until it returns true or timeout elapses.
func eventually(timeout time.Duration, cond func() bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if cond() {
		return nil
	}
	return context.DeadlineExceeded
}

// runEmbeddedJetStream starts an in-process NATS server with JetStream enabled
// and returns it; the caller is responsible for Shutdown.
func runEmbeddedJetStream(t *testing.T) *natssrv.Server {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1 // random available port
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natstest.RunServer(&opts)
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("embedded NATS server not ready")
	}
	return srv
}
