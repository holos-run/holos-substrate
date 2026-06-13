// Package receiver implements the thin webhook ingress described in ADR-9: an
// HTTP endpoint whose only job is to take the raw webhook body and publish it,
// unmodified, to a NATS JetStream subject, then acknowledge the sender. It
// performs no payload parsing, validation of meaning, or business logic — every
// interpretation is deferred to the subscriber (ADR-10).
package receiver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/holos-run/holos-paas/internal/nats"
	natsgo "github.com/nats-io/nats.go"
)

// forwardedHeaders is the single source of truth for the HTTP request headers
// the receiver copies onto the published NATS message. Per ADR-9 the receiver
// frames the raw body as the message payload and a curated set of headers as
// NATS headers; the subscriber (ADR-10) relies on these to route and verify
// the event without the receiver having parsed anything.
//
// The list is intentionally small and provider-agnostic:
//
//   - Content-Type        — the body's media type, needed to parse it later.
//   - X-Github-Event /    — the event-type headers (GitHub, generic). A
//     X-Event-Type           subscriber dispatches on these without decoding.
//   - X-Github-Delivery /  — the delivery-id headers, for idempotency and
//     X-Delivery-Id          tracing a single delivery end to end.
//   - X-Hub-Signature-256 /— the signature headers, carried through so edge or
//     X-Signature            subscriber-side verification can authenticate the
//     sender against the raw body (ADR-9 defers the
//     verification location but must not drop the input).
//
// Only headers present on the request are forwarded; absent ones are omitted.
var forwardedHeaders = []string{
	"Content-Type",
	"X-Github-Event",
	"X-Event-Type",
	"X-Github-Delivery",
	"X-Delivery-Id",
	"X-Hub-Signature-256",
	"X-Signature",
}

// Config configures a receiver Handler.
type Config struct {
	// Publisher publishes raw bodies to JetStream and blocks until PubAck.
	Publisher nats.Publisher
	// ConnState reports NATS connectivity for the readiness probe. Optional;
	// when nil, /readyz reports ready as long as a Publisher is set.
	ConnState nats.ConnState
	// SubjectPrefix is prepended to the {source} path segment to form the
	// publish subject "<prefix>.<source>" (e.g. "webhooks" -> "webhooks.quay").
	SubjectPrefix string
	// MaxBodyBytes bounds the request body via http.MaxBytesReader; an
	// oversized body yields 413. Must be > 0.
	MaxBodyBytes int64
	// Logger receives structured request logs. Optional; defaults to
	// slog.Default.
	Logger *slog.Logger
}

// Handler is the receiver's HTTP handler. Construct it with [New] and serve its
// [Handler.Mux].
type Handler struct {
	cfg Config
	log *slog.Logger
}

// New returns a Handler for the given configuration.
func New(cfg Config) *Handler {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Handler{cfg: cfg, log: log}
}

// Mux returns an http.ServeMux wiring the receiver's routes:
//
//	POST /webhooks/{source}  publish the raw body to "<prefix>.<source>"
//	GET  /healthz            liveness: always 200
//	GET  /readyz             readiness: 200 when connected to NATS, else 503
func (h *Handler) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	// Go 1.22+ method + wildcard routing: only POST to /webhooks/{source}
	// reaches the handler; other methods get 405 automatically.
	mux.HandleFunc("POST /webhooks/{source}", h.handleWebhook)
	mux.HandleFunc("GET /healthz", h.handleHealthz)
	mux.HandleFunc("GET /readyz", h.handleReadyz)
	return mux
}

// handleWebhook publishes the exact raw request body to the per-source subject
// and returns 202 only after a successful JetStream PubAck.
//
// Durability rationale (ADR-9): the receiver is thin and its only failure mode
// is failing to persist to JetStream. It therefore acknowledges the sender
// (202) only after the publish is acked, and returns 503 when the publish fails
// or NATS is unavailable. A 5xx makes the sender (the registry) retry, so an
// event is never silently dropped — durability is owned by the file-backed
// WorkQueue stream, and the receiver's contract is "202 means stored".
func (h *Handler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("source")
	subject := h.cfg.SubjectPrefix + "." + source

	// Bound the body before reading a single byte. MaxBytesReader caps the
	// read and signals an oversized body via *http.MaxBytesError.
	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			h.log.WarnContext(r.Context(), "webhook body too large",
				"source", source, "limit_bytes", h.cfg.MaxBodyBytes)
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		h.log.WarnContext(r.Context(), "reading webhook body", "source", source, "error", err)
		http.Error(w, "error reading request body", http.StatusBadRequest)
		return
	}

	hdr := selectHeaders(r.Header)

	if err := h.cfg.Publisher.Publish(r.Context(), subject, body, hdr); err != nil {
		// Publish failed: the event is NOT durably stored. Return 503 so the
		// sender retries (ADR-9 durability story); do not pretend success.
		h.log.ErrorContext(r.Context(), "publishing webhook to JetStream",
			"source", source, "subject", subject, "error", err)
		http.Error(w, "unable to enqueue webhook", http.StatusServiceUnavailable)
		return
	}

	h.log.InfoContext(r.Context(), "webhook accepted",
		"source", source, "subject", subject, "bytes", len(body))
	w.WriteHeader(http.StatusAccepted)
}

// handleHealthz is the liveness probe: the process is up, so always 200.
func (h *Handler) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

// handleReadyz is the readiness probe: 200 only when connected to NATS (the
// receiver cannot accept webhooks it cannot publish), else 503.
func (h *Handler) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if h.cfg.ConnState != nil && !h.cfg.ConnState.Connected() {
		http.Error(w, "not connected to NATS", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ready")
}

// selectHeaders copies the curated [forwardedHeaders] allowlist from the HTTP
// request onto a fresh nats.Header. Only headers present on the request are
// included, preserving all values for repeated headers.
func selectHeaders(src http.Header) natsgo.Header {
	hdr := natsgo.Header{}
	for _, name := range forwardedHeaders {
		if vals := src.Values(name); len(vals) > 0 {
			hdr[name] = append([]string(nil), vals...)
		}
	}
	return hdr
}

// Serve runs an HTTP server bound to addr serving h.Mux until ctx is canceled
// (e.g. on SIGINT/SIGTERM), then gracefully drains in-flight requests within
// shutdownTimeout. It returns nil on a clean shutdown.
func (h *Handler) Serve(ctx context.Context, addr string, shutdownTimeout time.Duration) error {
	srv := &http.Server{Addr: addr, Handler: h.Mux()}

	errCh := make(chan error, 1)
	go func() {
		h.log.Info("webhook receiver listening", "addr", addr)
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
		h.log.Info("shutting down webhook receiver")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return <-errCh
	}
}
