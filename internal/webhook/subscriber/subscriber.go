package subscriber

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/holos-run/holos-paas/internal/nats"
	"github.com/holos-run/holos-paas/internal/task"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// natsMsgIDHeader is the JetStream message-deduplication header. Setting it on a
// published DeployTask lets JetStream collapse redelivered duplicates on the
// TASKS stream within its dedupe window, turning the at-least-once raw-event
// delivery into an effectively-once DeployTask publish (ADR-13 idempotency).
const natsMsgIDHeader = "Nats-Msg-Id"

// dedupeID derives the JetStream dedupe identity for one tag's DeployTask from
// the raw event it was parsed from. It MUST be:
//
//   - stable across redeliveries of the same raw webhook message, so a
//     redelivered partial-success publish is collapsed; and
//   - distinct between separate webhook pushes, even of the same mutable tag,
//     so a later legitimate push of (say) "latest" is NOT silently swallowed as
//     a duplicate of the earlier one.
//
// The WEBHOOKS stream sequence (streamSeq) is exactly this raw-event identity:
// JetStream assigns it once when the receiver publishes the event and preserves
// it across every redelivery, while two distinct pushes always land at distinct
// sequences. Qualifying it with the per-tag idempotency key gives each tag's
// task within a single multi-tag event its own stable, distinct dedupe ID. The
// DeployTask idempotency key alone is unsuitable: it omits any per-event
// component, so it would collapse two genuine pushes of the same tag.
func dedupeID(streamSeq uint64, idempotencyKey string) string {
	return strconv.FormatUint(streamSeq, 10) + ":" + idempotencyKey
}

// Publisher publishes a marshaled DeployTask to JetStream. It is satisfied by
// [github.com/holos-run/holos-paas/internal/nats.Conn]; the narrow interface
// lets the consume loop be unit-tested against a fake.
type Publisher interface {
	Publish(ctx context.Context, subject string, body []byte, hdr natsgo.Header) error
}

// Config configures a [Consumer].
type Config struct {
	// Publisher publishes each DeployTask to task.DeploySubject and blocks
	// until PubAck. Required.
	Publisher Publisher
	// Registry maps a source token to its Parser. Defaults to
	// [DefaultRegistry] when nil.
	Registry *Registry
	// Logger receives structured logs. Defaults to slog.Default when nil.
	Logger *slog.Logger
	// Now supplies the receivedAt stamp on each task. Defaults to time.Now
	// when nil; tests inject a fixed clock.
	Now func() time.Time
	// MaxDeliver is the consumer's redelivery bound, used only to recognize the
	// final delivery so a still-failing message is terminated rather than
	// Nak'd into an immediate (futile) redelivery. Must match the consumer's
	// configured MaxDeliver. A value <= 0 disables the final-delivery Term and
	// the loop always Naks transient failures.
	MaxDeliver int
}

// Consumer drives the subscriber consume loop: for each raw webhook message it
// derives the source from the subject, parses it into DeployTasks, publishes
// each task to task.DeploySubject, and settles the raw message (Ack/Nak/Term)
// per ADR-10's WorkQueue durability semantics. It carries no JetStream binding
// of its own; bind it with [github.com/holos-run/holos-paas/internal/nats.Conn]'s
// Consume and pass [Consumer.Handle] as the message handler.
type Consumer struct {
	pub        Publisher
	registry   *Registry
	log        *slog.Logger
	now        func() time.Time
	maxDeliver int
}

// New returns a Consumer for cfg, applying defaults for the optional fields.
func New(cfg Config) *Consumer {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	reg := cfg.Registry
	if reg == nil {
		reg = DefaultRegistry()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Consumer{
		pub:        cfg.Publisher,
		registry:   reg,
		log:        log,
		now:        now,
		maxDeliver: cfg.MaxDeliver,
	}
}

// sourceFromSubject returns the last dot-separated token of a NATS subject,
// e.g. "webhooks.quay" -> "quay". The receiver publishes raw events to
// "<prefix>.<source>", so the final token is the webhook source.
func sourceFromSubject(subject string) string {
	if i := strings.LastIndex(subject, "."); i >= 0 {
		return subject[i+1:]
	}
	return subject
}

// httpHeader adapts the NATS message headers (a nats.Header, itself a
// map[string][]string) onto the http.Header the Phase 1 [Parser] expects. Both
// are textproto.MIMEHeader-shaped, so a direct conversion preserves all keys
// and values.
func httpHeader(h natsgo.Header) http.Header {
	return http.Header(h)
}

// Handle processes a single raw webhook message and settles it. It is the
// per-message callback passed to the JetStream consume loop.
//
// Settlement rules (ADR-10 / HOL-1123):
//
//   - Unknown source or parse error → Term: the message is unparseable and
//     redelivery cannot help, so it is removed from the WorkQueue with a logged
//     reason. A poison payload therefore never wedges the queue (HOL-1123 AC2).
//   - Publish/NATS failure → Nak for redelivery, unless this is the final
//     delivery (delivery count == MaxDeliver), in which case it is Term'd with a
//     logged reason so it stops being redelivered. Every task carries a
//     Nats-Msg-Id dedupe header keyed on the raw event's WEBHOOKS stream
//     sequence (see dedupeID) so a redelivered partial success is collapsed by
//     JetStream on the TASKS stream without swallowing a later genuine push of
//     the same tag.
//   - All publishes acked → Ack: the raw message is removed from the WorkQueue.
func (c *Consumer) Handle(msg jetstream.Msg) {
	ctx := context.Background()
	subject := msg.Subject()
	source := sourceFromSubject(subject)

	// The WEBHOOKS stream sequence is the raw event's stable identity (see
	// dedupeID): identical on every redelivery, distinct per push. It also
	// labels every termination log so a Term'd event is traceable to its exact
	// stream offset. A missing sequence (metadata unavailable) is transient and
	// Nak'd rather than published with a degraded dedupe ID — publishing without
	// a correct dedupe ID risks either dropping a real push or failing to
	// collapse a redelivery.
	md, err := msg.Metadata()
	if err != nil {
		c.log.Warn("naking message for redelivery: metadata unavailable",
			"subject", subject, "source", source, "error", err)
		if nerr := msg.Nak(); nerr != nil {
			c.log.Error("naking message", "subject", subject, "source", source, "error", nerr)
		}
		return
	}
	streamSeq := md.Sequence.Stream

	parser, found := c.registry.Lookup(source)
	if !found {
		c.terminate(msg, subject, source, streamSeq, msg.Data(), "unknown webhook source", nil)
		return
	}

	tasks, err := parser.Parse(source, httpHeader(msg.Headers()), msg.Data(), c.now())
	if err != nil {
		c.terminate(msg, subject, source, streamSeq, msg.Data(), "parse error", err)
		return
	}

	for _, t := range tasks {
		body, err := marshalTask(t)
		if err != nil {
			// A marshal failure is deterministic for a given task, so
			// redelivery cannot help: terminate rather than wedge the queue.
			c.terminate(msg, subject, source, streamSeq, msg.Data(), "marshaling DeployTask", err, "tag", t.Tag)
			return
		}
		hdr := natsgo.Header{natsMsgIDHeader: []string{dedupeID(streamSeq, t.IdempotencyKey)}}
		if err := c.pub.Publish(ctx, task.DeploySubject, body, hdr); err != nil {
			c.nakOrTerm(msg, subject, source, streamSeq, t.Tag, err)
			return
		}
	}

	if err := msg.Ack(); err != nil {
		c.log.Error("acking message", "subject", subject, "source", source, "error", err)
	}
}

// nakOrTerm handles a transient publish failure: Nak for redelivery, or Term on
// the final delivery so a persistently failing message stops being redelivered.
func (c *Consumer) nakOrTerm(msg jetstream.Msg, subject, source string, streamSeq uint64, tag string, cause error) {
	if c.isFinalDelivery(msg) {
		c.terminate(msg, subject, source, streamSeq, msg.Data(),
			"publish failed on final delivery", cause, "tag", tag)
		return
	}
	c.log.Warn("naking message for redelivery: publish failed",
		"subject", subject, "source", source, "stream_seq", streamSeq, "tag", tag, "error", cause)
	if err := msg.Nak(); err != nil {
		c.log.Error("naking message", "subject", subject, "source", source, "error", err)
	}
}

// terminate logs the full raw payload at error level — a log-backed
// dead-letter so a Term'd event remains recoverable for diagnosis or manual
// replay — and then Terms the message so a poison payload never wedges the
// WorkQueue (HOL-1123 AC2). The payload is captured base64-encoded under
// "raw_base64" so binary/multiline bodies survive structured logging intact.
//
// Durable dead-lettering to a dedicated subject/stream (ADR-13's dead-letter
// subject) is a larger, ADR-scoped addition deferred beyond this phase; until
// it lands, this log line is the recovery record. reason names why the message
// was terminated; cause is the underlying error when one exists (nil for the
// unknown-source case). extra appends additional structured key/value pairs.
func (c *Consumer) terminate(msg jetstream.Msg, subject, source string, streamSeq uint64, raw []byte, reason string, cause error, extra ...any) {
	args := []any{
		"subject", subject,
		"source", source,
		"stream_seq", streamSeq,
		"reason", reason,
		"raw_base64", base64.StdEncoding.EncodeToString(raw),
	}
	if cause != nil {
		args = append(args, "error", cause)
	}
	args = append(args, extra...)
	c.log.Error("terminating message", args...)
	if err := msg.Term(); err != nil {
		c.log.Error("terminating message: Term failed",
			"subject", subject, "source", source, "stream_seq", streamSeq, "error", err)
	}
}

// isFinalDelivery reports whether msg is on its last permitted delivery, so the
// loop should Term rather than Nak a still-failing message. It is conservative:
// when MaxDeliver is not configured (<= 0) or the metadata is unavailable it
// returns false and the message is Nak'd, letting JetStream's own MaxDeliver
// bound terminate it.
func (c *Consumer) isFinalDelivery(msg jetstream.Msg) bool {
	if c.maxDeliver <= 0 {
		return false
	}
	md, err := msg.Metadata()
	if err != nil {
		return false
	}
	return md.NumDelivered >= uint64(c.maxDeliver)
}

// marshalTask serializes a DeployTask to its JSON wire form for publishing to
// task.DeploySubject.
func marshalTask(t task.DeployTask) ([]byte, error) {
	body, err := json.Marshal(t)
	if err != nil {
		return nil, fmt.Errorf("marshaling DeployTask: %w", err)
	}
	return body, nil
}

// HealthHandler returns an http.ServeMux serving the liveness and readiness
// probes for the subscriber Deployment. The subscriber has no inbound business
// HTTP; these endpoints exist solely for kubelet probes so a pod that cannot
// reach NATS stays out of Ready (the Phase 3 rollout gate).
//
//	GET /healthz  liveness: always 200 (the process is up)
//	GET /readyz   readiness: 200 when connected to NATS, else 503
//
// state is optional; when nil /readyz always reports ready.
func HealthHandler(state nats.ConnState) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if state != nil && !state.Connected() {
			http.Error(w, "not connected to NATS", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ready")
	})
	return mux
}

// ServeHealth runs an HTTP server bound to addr serving the health mux until
// ctx is canceled, then gracefully drains within shutdownTimeout. It returns
// nil on a clean shutdown.
func ServeHealth(ctx context.Context, addr string, state nats.ConnState, shutdownTimeout time.Duration, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           HealthHandler(state),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("subscriber health server listening", "addr", addr)
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down subscriber health server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return <-errCh
	}
}
