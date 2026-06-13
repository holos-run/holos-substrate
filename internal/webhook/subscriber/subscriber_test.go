package subscriber

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	natssrv "github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	localnats "github.com/holos-run/holos-paas/internal/nats"
	"github.com/holos-run/holos-paas/internal/task"
)

func TestSourceFromSubject(t *testing.T) {
	cases := map[string]string{
		"webhooks.quay":   "quay",
		"webhooks.github": "github",
		"quay":            "quay",
		"a.b.c":           "c",
	}
	for subject, want := range cases {
		if got := sourceFromSubject(subject); got != want {
			t.Errorf("sourceFromSubject(%q) = %q, want %q", subject, got, want)
		}
	}
}

// TestSubscriber_GoldenDeployTask is the HOL-1123 AC1 integration test: a valid
// Quay payload published to webhooks.quay yields one DeployTask per tag on
// tasks.deploy, and the raw message is acked (removed from the WEBHOOKS
// WorkQueue).
func TestSubscriber_GoldenDeployTask(t *testing.T) {
	env := newTestEnv(t)

	body, err := os.ReadFile("testdata/quay-repo-push.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	if err := env.conn.Publish(env.ctx, "webhooks.quay", body, nil); err != nil {
		t.Fatalf("publishing fixture: %v", err)
	}

	fixedNow := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	consumer := New(Config{
		Publisher:  env.conn,
		Registry:   DefaultRegistry(),
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Now:        func() time.Time { return fixedNow },
		MaxDeliver: 5,
	})
	env.startConsumer(t, consumer)

	// The fixture lists two tags (v2, latest) → two DeployTasks on tasks.deploy.
	tasks := env.collectDeployTasks(t, 2)

	wantByTag := map[string]task.DeployTask{
		"v2": task.NewDeployTask("quay", "holos/sample-app", "v2",
			"quay.example.com/holos/sample-app", fixedNow),
		"latest": task.NewDeployTask("quay", "holos/sample-app", "latest",
			"quay.example.com/holos/sample-app", fixedNow),
	}
	for _, got := range tasks {
		want, ok := wantByTag[got.Tag]
		if !ok {
			t.Fatalf("unexpected task for tag %q", got.Tag)
		}
		if got != want {
			t.Errorf("task for tag %q =\n  %+v\nwant\n  %+v", got.Tag, got, want)
		}
		if got.App != "sample-app" {
			t.Errorf("App = %q, want %q", got.App, "sample-app")
		}
		if got.SchemaVersion != task.SchemaVersion {
			t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, task.SchemaVersion)
		}
		delete(wantByTag, got.Tag)
	}
	if len(wantByTag) != 0 {
		t.Errorf("missing DeployTasks for tags: %v", wantByTag)
	}

	// The raw message must be acked: the WEBHOOKS WorkQueue is now empty.
	env.requireWebhooksEmpty(t)
}

// TestDedupeID asserts the dedupe identity is stable for a given raw event
// (same stream sequence) yet distinct across separate pushes of the same tag
// (different stream sequence) — the property that keeps a redelivery collapsed
// without swallowing a later genuine push.
func TestDedupeID(t *testing.T) {
	key := task.IdempotencyKey("quay", "holos/sample-app", "latest", "")
	if a, b := dedupeID(7, key), dedupeID(7, key); a != b {
		t.Errorf("dedupeID not stable for the same stream seq: %q != %q", a, b)
	}
	if a, b := dedupeID(7, key), dedupeID(8, key); a == b {
		t.Errorf("dedupeID collided across stream sequences: both %q", a)
	}
	// Distinct tags within one event must also differ.
	k1 := task.IdempotencyKey("quay", "holos/sample-app", "v2", "")
	k2 := task.IdempotencyKey("quay", "holos/sample-app", "latest", "")
	if dedupeID(7, k1) == dedupeID(7, k2) {
		t.Error("dedupeID collided across tags within one event")
	}
}

// TestSubscriber_RepeatedTagPushNotCollapsed is the regression test for the
// dedupe bug: two separate webhook pushes of the same single tag must each
// produce a DeployTask on tasks.deploy. They land at distinct WEBHOOKS stream
// sequences, so their dedupe IDs differ and JetStream does not collapse the
// second as a duplicate of the first.
func TestSubscriber_RepeatedTagPushNotCollapsed(t *testing.T) {
	env := newTestEnv(t)

	payload := []byte(`{"repository":"holos/sample-app","namespace":"holos","name":"sample-app","docker_url":"quay.example.com/holos/sample-app","updated_tags":["latest"]}`)
	for i := 0; i < 2; i++ {
		if err := env.conn.Publish(env.ctx, "webhooks.quay", payload, nil); err != nil {
			t.Fatalf("publishing push %d: %v", i, err)
		}
	}

	consumer := New(Config{
		Publisher:  env.conn,
		Registry:   DefaultRegistry(),
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
		MaxDeliver: 5,
	})
	env.startConsumer(t, consumer)

	// Both pushes must surface a DeployTask despite sharing the same tag.
	tasks := env.collectDeployTasks(t, 2)
	for _, dt := range tasks {
		if dt.Tag != "latest" {
			t.Errorf("unexpected tag %q, want latest", dt.Tag)
		}
	}
	env.requireWebhooksEmpty(t)
}

// TestSubscriber_MalformedTerminated is the malformed-payload integration test:
// an undecodable payload published to webhooks.quay is terminated (not
// redelivered forever), produces no message on tasks.deploy, and logs a reason.
func TestSubscriber_MalformedTerminated(t *testing.T) {
	env := newTestEnv(t)

	if err := env.conn.Publish(env.ctx, "webhooks.quay", []byte("{"), nil); err != nil {
		t.Fatalf("publishing malformed payload: %v", err)
	}

	logs := &captureHandler{}
	consumer := New(Config{
		Publisher:  env.conn,
		Registry:   DefaultRegistry(),
		Logger:     slog.New(logs),
		MaxDeliver: 5,
	})
	env.startConsumer(t, consumer)

	// The message must be terminated (removed from the WorkQueue), not stuck
	// pending or redelivered forever.
	env.requireWebhooksEmpty(t)

	// No DeployTask must appear on tasks.deploy.
	if msg := env.tryFetchDeploy(t, time.Second); msg != nil {
		t.Fatalf("a DeployTask was published for a malformed payload: %q", msg.Data())
	}

	// A reason must be logged naming the subject, and the raw payload must be
	// captured (the log-backed dead-letter) so the event is recoverable.
	wantRaw := base64.StdEncoding.EncodeToString([]byte("{"))
	if err := eventually(5*time.Second, func() bool {
		rec, ok := logs.find(slog.LevelError, "terminating message")
		if !ok {
			return false
		}
		return rec.attr("subject") == "webhooks.quay" &&
			rec.attr("reason") == "parse error" &&
			rec.attr("raw_base64") == wantRaw
	}); err != nil {
		t.Fatalf("no termination reason logged for the malformed payload; records: %s", logs.dump())
	}
}

// TestSubscriber_UnknownSourceTerminated asserts an unknown source token is
// terminated with a logged reason and publishes no DeployTask.
func TestSubscriber_UnknownSourceTerminated(t *testing.T) {
	env := newTestEnv(t)

	if err := env.conn.Publish(env.ctx, "webhooks.github", []byte(`{"x":1}`), nil); err != nil {
		t.Fatalf("publishing unknown-source payload: %v", err)
	}

	logs := &captureHandler{}
	consumer := New(Config{
		Publisher:  env.conn,
		Registry:   DefaultRegistry(),
		Logger:     slog.New(logs),
		MaxDeliver: 5,
	})
	env.startConsumer(t, consumer)

	env.requireWebhooksEmpty(t)
	if msg := env.tryFetchDeploy(t, time.Second); msg != nil {
		t.Fatalf("a DeployTask was published for an unknown source: %q", msg.Data())
	}
	if err := eventually(5*time.Second, func() bool {
		rec, ok := logs.find(slog.LevelError, "terminating message")
		return ok && rec.attr("source") == "github" &&
			rec.attr("reason") == "unknown webhook source"
	}); err != nil {
		t.Fatalf("no unknown-source termination logged; records: %s", logs.dump())
	}
}

// TestHealthEndpoints covers the liveness/readiness handlers.
func TestHealthEndpoints(t *testing.T) {
	t.Run("healthz always 200", func(t *testing.T) {
		mux := HealthHandler(stateFunc(func() bool { return false }))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("healthz = %d, want 200", rec.Code)
		}
	})
	t.Run("readyz 200 when connected", func(t *testing.T) {
		mux := HealthHandler(stateFunc(func() bool { return true }))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("readyz = %d, want 200", rec.Code)
		}
	})
	t.Run("readyz 503 when disconnected", func(t *testing.T) {
		mux := HealthHandler(stateFunc(func() bool { return false }))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("readyz = %d, want 503", rec.Code)
		}
	})
	t.Run("readyz 200 when state is nil", func(t *testing.T) {
		mux := HealthHandler(nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("readyz = %d, want 200", rec.Code)
		}
	})
}

// --- test harness ---

type testEnv struct {
	ctx    context.Context
	conn   *localnats.Conn
	js     jetstream.JetStream
	deploy jetstream.Consumer
	cancel context.CancelFunc
}

// newTestEnv starts an embedded JetStream server, creates the WEBHOOKS and
// TASKS streams, connects the subscriber's Conn, and returns a consumer on
// tasks.deploy for assertions. Cleanup is registered via t.Cleanup.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	srv := runEmbeddedJetStream(t)
	t.Cleanup(srv.Shutdown)
	url := srv.ClientURL()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	conn, err := localnats.Connect(url)
	if err != nil {
		t.Fatalf("connecting subscriber Conn: %v", err)
	}
	t.Cleanup(conn.Close)

	nc, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connecting raw client: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}

	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "WEBHOOKS",
		Subjects:  []string{"webhooks.>"},
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.FileStorage,
	}); err != nil {
		t.Fatalf("creating WEBHOOKS stream: %v", err)
	}
	// TASKS dedupes on the Nats-Msg-Id header; a short window suffices for tests.
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:       "TASKS",
		Subjects:   []string{"tasks.>"},
		Retention:  jetstream.LimitsPolicy,
		Storage:    jetstream.FileStorage,
		Duplicates: 2 * time.Minute,
	}); err != nil {
		t.Fatalf("creating TASKS stream: %v", err)
	}

	deploy, err := js.CreateOrUpdateConsumer(ctx, "TASKS", jetstream.ConsumerConfig{
		Durable:       "test-deploy-reader",
		FilterSubject: task.DeploySubject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("creating tasks.deploy consumer: %v", err)
	}

	return &testEnv{ctx: ctx, conn: conn, js: js, deploy: deploy, cancel: cancel}
}

// startConsumer binds the subscriber's consume loop on WEBHOOKS and registers
// Stop via t.Cleanup.
func (e *testEnv) startConsumer(t *testing.T, c *Consumer) {
	t.Helper()
	consumption, err := e.conn.Consume(e.ctx, localnats.ConsumerConfig{
		Stream:        "WEBHOOKS",
		Durable:       "webhook-subscriber",
		FilterSubject: "webhooks.>",
		MaxDeliver:    5,
		AckWait:       2 * time.Second,
	}, c.Handle)
	if err != nil {
		t.Fatalf("starting consume loop: %v", err)
	}
	t.Cleanup(consumption.Stop)
}

// collectDeployTasks fetches exactly n DeployTasks off tasks.deploy, decoding
// and acking each. It fails the test if fewer than n arrive within the timeout.
func (e *testEnv) collectDeployTasks(t *testing.T, n int) []task.DeployTask {
	t.Helper()
	out := make([]task.DeployTask, 0, n)
	deadline := time.Now().Add(10 * time.Second)
	for len(out) < n && time.Now().Before(deadline) {
		msg, err := e.deploy.Next(jetstream.FetchMaxWait(time.Second))
		if err != nil {
			continue
		}
		var dt task.DeployTask
		if err := json.Unmarshal(msg.Data(), &dt); err != nil {
			t.Fatalf("decoding DeployTask: %v (body %q)", err, msg.Data())
		}
		// The dedupe header must be the stream-sequence-qualified dedupe ID,
		// i.e. "<streamSeq>:<idempotencyKey>" — stable per raw event, distinct
		// per push (see dedupeID).
		if got := msg.Headers().Get(natsMsgIDHeader); !strings.HasSuffix(got, ":"+dt.IdempotencyKey) {
			t.Errorf("Nats-Msg-Id = %q, want suffix %q", got, ":"+dt.IdempotencyKey)
		}
		out = append(out, dt)
		_ = msg.Ack()
	}
	if len(out) != n {
		t.Fatalf("collected %d DeployTasks, want %d", len(out), n)
	}
	return out
}

// tryFetchDeploy attempts to fetch one message from tasks.deploy within
// timeout; it returns nil when none arrives.
func (e *testEnv) tryFetchDeploy(t *testing.T, timeout time.Duration) jetstream.Msg {
	t.Helper()
	msg, err := e.deploy.Next(jetstream.FetchMaxWait(timeout))
	if err != nil {
		return nil
	}
	return msg
}

// requireWebhooksEmpty asserts the WEBHOOKS WorkQueue drains to zero messages,
// i.e. the raw message was settled (Ack or Term) and removed.
func (e *testEnv) requireWebhooksEmpty(t *testing.T) {
	t.Helper()
	if err := eventually(8*time.Second, func() bool {
		s, err := e.js.Stream(e.ctx, "WEBHOOKS")
		if err != nil {
			return false
		}
		info, err := s.Info(e.ctx)
		if err != nil {
			return false
		}
		return info.State.Msgs == 0
	}); err != nil {
		t.Fatalf("WEBHOOKS WorkQueue did not drain (message not settled): %v", err)
	}
}

// --- captured slog handler ---

type logRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

func (r logRecord) attr(key string) string { return r.attrs[key] }

type captureHandler struct {
	mu      sync.Mutex
	records []logRecord
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := logRecord{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, rec)
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// find returns the first record at level whose message starts with msgPrefix.
func (h *captureHandler) find(level slog.Level, msgPrefix string) (logRecord, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.level == level && strings.HasPrefix(r.msg, msgPrefix) {
			return r, true
		}
	}
	return logRecord{}, false
}

func (h *captureHandler) dump() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var b strings.Builder
	for _, r := range h.records {
		b.WriteString(r.level.String())
		b.WriteString(" ")
		b.WriteString(r.msg)
		b.WriteString("\n")
	}
	return b.String()
}

// stateFunc adapts a func to the nats.ConnState interface.
type stateFunc func() bool

func (s stateFunc) Connected() bool { return s() }

// --- embedded server + polling helpers ---

func runEmbeddedJetStream(t *testing.T) *natssrv.Server {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natstest.RunServer(&opts)
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("embedded NATS server not ready")
	}
	return srv
}

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
