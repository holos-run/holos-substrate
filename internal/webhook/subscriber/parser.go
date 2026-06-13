// Package subscriber parses raw webhook events into DeployTasks (ADR-10). It
// is the meaning-assigning half of the pipeline: the receiver (ADR-9) forwards
// raw bodies unmodified, and this package decodes a source-specific payload
// (e.g. a Quay repository push) into the well-known
// [github.com/holos-run/holos-paas/internal/task.DeployTask] contract.
//
// This package is deliberately free of any NATS or Kubernetes import so the
// parsing logic stays trivially testable and reusable. Headers are modeled as
// an [http.Header] (a plain map[string][]string); Phase 2 (ADR-13) adapts the
// NATS message headers onto this shape when wiring the JetStream consumer.
package subscriber

import (
	"net/http"
	"time"

	"github.com/holos-run/holos-paas/internal/task"
)

// Parser decodes one source's raw webhook payload into zero or more
// DeployTasks. A single source event may fan out to several tasks — one per
// pushed tag (ADR-13). Implementations must be pure and side-effect free:
// given the same source, headers, and body they return the same tasks (modulo
// the wall-clock receivedAt), with no network or registry access.
//
// Scope boundary (ADR-13): this layer performs source decoding only. It does
// not match the event to an Application, route between tasks.render and
// tasks.deploy, or resolve a manifest digest — all of that is deferred to
// later phases. A parser returns the mechanically-derived tasks; the Phase 2
// consumer (HOL-1203) is responsible for KRM matching, routing, and digest
// resolution before publishing. Keeping those concerns out of the parser is
// what lets this package stay NATS- and Kubernetes-free and trivially
// testable.
//
// A malformed or unparseable body must return a non-nil, descriptive error and
// no tasks. The error is typed by intent only at this layer; the Term-vs-Nak
// delivery decision lives in Phase 2 (ADR-13).
type Parser interface {
	// Parse decodes body (with hdr as the request/message headers) into the
	// DeployTasks it represents. source is the webhook source token (e.g.
	// "quay") and is stamped onto each task. receivedAt is the time the event
	// was observed and is copied verbatim onto each task.
	Parse(source string, hdr http.Header, body []byte, receivedAt time.Time) ([]task.DeployTask, error)
}

// Registry maps a webhook source token (e.g. "quay") to the Parser that
// decodes that source's events. Phase 2 dispatches by the source token parsed
// from the NATS subject; an unknown source is distinguishable via [Lookup]
// returning found=false so the consumer can Term the message (ADR-13).
//
// The zero value is not usable; construct one with [NewRegistry]. A Registry
// is not safe for concurrent registration, but is safe for concurrent Lookup
// once registration is complete (the expected usage: register at startup, look
// up per message).
type Registry struct {
	parsers map[string]Parser
}

// NewRegistry returns an empty Registry ready for [Registry.Register].
func NewRegistry() *Registry {
	return &Registry{parsers: make(map[string]Parser)}
}

// Register associates p with source, replacing any existing parser for that
// source.
func (r *Registry) Register(source string, p Parser) {
	r.parsers[source] = p
}

// Lookup returns the parser registered for source. found reports whether a
// parser is registered; a false found lets the caller distinguish an unknown
// source (which Phase 2 Terms) from a parse failure.
func (r *Registry) Lookup(source string) (p Parser, found bool) {
	p, found = r.parsers[source]
	return p, found
}

// DefaultRegistry returns a Registry with every built-in source parser
// registered: currently "quay". Additional sources (e.g. GitHub) register here
// without touching the pipeline.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(QuaySource, QuayParser{})
	return r
}
