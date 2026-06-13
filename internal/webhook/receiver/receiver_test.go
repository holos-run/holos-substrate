package receiver

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	natssrv "github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	localnats "github.com/holos-run/holos-paas/internal/nats"
)

// fakePublisher records the last Publish call and returns a configured error.
// It lets handler tests exercise the success and failure paths without a
// running NATS server.
type fakePublisher struct {
	err error

	called  bool
	subject string
	body    []byte
	hdr     natsgo.Header
}

func (f *fakePublisher) Publish(_ context.Context, subject string, body []byte, hdr natsgo.Header) error {
	f.called = true
	f.subject = subject
	f.body = append([]byte(nil), body...)
	f.hdr = hdr
	return f.err
}

// stateFunc adapts a func to the nats.ConnState interface.
type stateFunc func() bool

func (s stateFunc) Connected() bool { return s() }

func newTestHandler(t *testing.T, cfg Config) *Handler {
	t.Helper()
	if cfg.SubjectPrefix == "" {
		cfg.SubjectPrefix = "webhooks"
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = 1 << 20
	}
	return New(cfg)
}

func TestHandleWebhook(t *testing.T) {
	t.Run("publishes raw body to the correct subject and returns 202", func(t *testing.T) {
		fp := &fakePublisher{}
		h := newTestHandler(t, Config{Publisher: fp, SubjectPrefix: "webhooks"})

		raw := []byte(`{"repository":"quay/app","unmodified":true}`)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/quay", bytes.NewReader(raw))
		rec := httptest.NewRecorder()
		h.Mux().ServeHTTP(rec, req)

		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
		}
		if !fp.called {
			t.Fatal("Publish was not called")
		}
		if fp.subject != "webhooks.quay" {
			t.Errorf("subject = %q, want %q", fp.subject, "webhooks.quay")
		}
		if !bytes.Equal(fp.body, raw) {
			t.Errorf("published body = %q, want %q (must be byte-identical)", fp.body, raw)
		}
	})

	t.Run("maps the selected headers onto NATS headers and omits the rest", func(t *testing.T) {
		fp := &fakePublisher{}
		h := newTestHandler(t, Config{Publisher: fp})

		req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader("body"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Github-Event", "push")
		req.Header.Set("X-Github-Delivery", "delivery-123")
		req.Header.Set("X-Hub-Signature-256", "sha256=abc")
		req.Header.Set("X-Not-Allowlisted", "should-not-appear")
		rec := httptest.NewRecorder()
		h.Mux().ServeHTTP(rec, req)

		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
		}
		want := map[string]string{
			"Content-Type":        "application/json",
			"X-Github-Event":      "push",
			"X-Github-Delivery":   "delivery-123",
			"X-Hub-Signature-256": "sha256=abc",
		}
		for k, v := range want {
			if got := fp.hdr.Get(k); got != v {
				t.Errorf("header %q = %q, want %q", k, got, v)
			}
		}
		if got := fp.hdr.Get("X-Not-Allowlisted"); got != "" {
			t.Errorf("non-allowlisted header leaked: %q", got)
		}
	})

	t.Run("returns 503 when the publish fails", func(t *testing.T) {
		fp := &fakePublisher{err: errors.New("no ack")}
		h := newTestHandler(t, Config{Publisher: fp})

		req := httptest.NewRequest(http.MethodPost, "/webhooks/quay", strings.NewReader("body"))
		rec := httptest.NewRecorder()
		h.Mux().ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
	})

	t.Run("returns 413 for an oversized body and does not publish", func(t *testing.T) {
		fp := &fakePublisher{}
		h := newTestHandler(t, Config{Publisher: fp, MaxBodyBytes: 16})

		req := httptest.NewRequest(http.MethodPost, "/webhooks/quay",
			bytes.NewReader(bytes.Repeat([]byte("x"), 1024)))
		rec := httptest.NewRecorder()
		h.Mux().ServeHTTP(rec, req)

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
		}
		if fp.called {
			t.Error("Publish must not be called for an oversized body (no data loss semantics)")
		}
	})
}

func TestHandleHealthz(t *testing.T) {
	h := newTestHandler(t, Config{Publisher: &fakePublisher{}})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestHandleReadyz(t *testing.T) {
	t.Run("200 when connected", func(t *testing.T) {
		h := newTestHandler(t, Config{
			Publisher: &fakePublisher{},
			ConnState: stateFunc(func() bool { return true }),
		})
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		rec := httptest.NewRecorder()
		h.Mux().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("readyz status = %d, want %d", rec.Code, http.StatusOK)
		}
	})

	t.Run("503 when disconnected", func(t *testing.T) {
		h := newTestHandler(t, Config{
			Publisher: &fakePublisher{},
			ConnState: stateFunc(func() bool { return false }),
		})
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		rec := httptest.NewRecorder()
		h.Mux().ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("readyz status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
	})
}

// TestHandleWebhook_EndToEnd exercises the full path against an embedded
// JetStream server: a real PubAck on a WEBHOOKS-equivalent WorkQueue stream
// bound to webhooks.>, then reads the message back and asserts the body and
// NATS headers are byte-identical to what was POSTed.
func TestHandleWebhook_EndToEnd(t *testing.T) {
	srv := runEmbeddedJetStream(t)
	defer srv.Shutdown()

	url := srv.ClientURL()
	conn, err := localnats.Connect(url)
	if err != nil {
		t.Fatalf("connecting to embedded NATS: %v", err)
	}
	defer conn.Close()

	// Create a WEBHOOKS-equivalent WorkQueue stream bound to webhooks.>.
	nc, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connecting raw client: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "WEBHOOKS",
		Subjects:  []string{"webhooks.>"},
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.FileStorage,
	}); err != nil {
		t.Fatalf("creating WEBHOOKS stream: %v", err)
	}

	h := New(Config{
		Publisher:     conn,
		ConnState:     conn,
		SubjectPrefix: "webhooks",
		MaxBodyBytes:  1 << 20,
	})

	raw := []byte(`{"name":"quay/app","updated_tags":["v1.2.3"]}`)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/quay", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Github-Delivery", "abc-123")
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (PubAck should have succeeded)", rec.Code, http.StatusAccepted)
	}

	// Read the message back off the stream and verify it is byte-identical.
	cons, err := js.CreateOrUpdateConsumer(ctx, "WEBHOOKS", jetstream.ConsumerConfig{
		AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("creating consumer: %v", err)
	}
	msg, err := cons.Next(jetstream.FetchMaxWait(5 * time.Second))
	if err != nil {
		t.Fatalf("fetching message: %v", err)
	}
	if msg.Subject() != "webhooks.quay" {
		t.Errorf("subject = %q, want %q", msg.Subject(), "webhooks.quay")
	}
	if !bytes.Equal(msg.Data(), raw) {
		t.Errorf("stored body = %q, want %q (must be byte-identical)", msg.Data(), raw)
	}
	if got := msg.Headers().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type header = %q, want %q", got, "application/json")
	}
	if got := msg.Headers().Get("X-Github-Delivery"); got != "abc-123" {
		t.Errorf("X-Github-Delivery header = %q, want %q", got, "abc-123")
	}
	_ = msg.Ack()
}

// runEmbeddedJetStream starts starts an in-process NATS server with JetStream enabled
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
